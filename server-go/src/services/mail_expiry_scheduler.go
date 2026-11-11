// Port of server/src/services/mail-expiry-scheduler.ts.
//
// Unlike the node-cron schedulers this one uses `setInterval(…, 1h)` and runs once
// immediately at startup. It has no isRunning flag in the TypeScript, so two runs
// may overlap; every write is conditional and the upstream suspend/resume calls are
// documented as idempotent, which is what makes that safe.
//
// The tricky part is the renewal race. A renewal can land:
//
//   - between the "expired" scan and the status update → the guarded updateMany
//     re-checks expiry;
//   - between two upstream suspends of the same subscription → the per-domain
//     re-check re-reads the row before every call;
//   - during a suspend call itself → the "markedSuspended.count === 0" branch
//     compensates by resuming the domain upstream;
//   - while a domain is suspended → the resume scan restores it and rewrites the
//     local status from verifiedAt.
//
// All four paths are preserved; dropping the compensation would leave a renewed
// customer's domain suspended at the provider.
package services

import (
	"context"
	"log"
	"strconv"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// MailExpirySchedulerInterval is the 1-hour setInterval period.
const MailExpirySchedulerInterval = time.Hour

// MailExpirySchedulerNextRun mirrors `setInterval(…, 1h)`: the next fire is one
// interval after `now` (the immediate startup run is separate).
func MailExpirySchedulerNextRun(now time.Time) time.Time {
	return now.UTC().Add(MailExpirySchedulerInterval)
}

// CheckMailSubscriptionExpiry mirrors checkMailSubscriptionExpiry.
//
// The TypeScript rethrows the outer error; here the error is returned so the
// scheduler logs it while startup continues.
func CheckMailSubscriptionExpiry(ctx context.Context) error {
	now := time.Now().UTC()

	expired, err := db.MailListActiveExpiredSubscriptions(ctx, now)
	if err != nil {
		log.Printf("[Mail] Error checking mail subscription expiry: %v", err)
		return err
	}
	if len(expired) > 0 {
		log.Printf("[Mail] Found %d expired mail subscription candidate(s)", len(expired))
	}

	ids := make([]int32, 0, len(expired))
	for _, subscription := range expired {
		ids = append(ids, subscription.ID)
	}
	updated, err := db.MailMarkSubscriptionsExpired(ctx, ids, now)
	if err != nil {
		log.Printf("[Mail] Error checking mail subscription expiry: %v", err)
		return err
	}
	log.Printf("[Mail] Updated %d mail subscriptions to expired status", updated)

	if err := mailSuspendExpiredDomains(ctx, now); err != nil {
		return err
	}
	return mailResumeRenewedDomains(ctx, now)
}

// mailSuspendExpiredDomains mirrors the second half of
// checkMailSubscriptionExpiry: expired subscriptions still holding unsuspended
// domains are suspended upstream, one domain at a time.
func mailSuspendExpiredDomains(ctx context.Context, now time.Time) error {
	subscriptions, err := db.MailListExpiredSubscriptionsWithUnsuspendedDomains(ctx, now)
	if err != nil {
		log.Printf("[Mail] Error checking mail subscription expiry: %v", err)
		return err
	}

	for _, subscription := range subscriptions {
		source, sourceErr := db.MailGetSourceByID(ctx, subscription.SourceID)
		if sourceErr != nil {
			log.Printf("[Mail] Cannot load source for expired subscription #%d: %v", subscription.ID, sourceErr)
			mailSchedulerLog(ctx, subscription.UserID, "mail_upstream_suspend_pending_retry",
				"Mail subscription #"+itoa(subscription.ID)+" source could not be loaded; upstream suspend pending retry")
			continue
		}
		if source == nil {
			log.Printf("[Mail] Cannot suspend expired subscription #%d: source not found", subscription.ID)
			mailSchedulerLog(ctx, subscription.UserID, "mail_upstream_suspend_pending_retry",
				"Mail subscription #"+itoa(subscription.ID)+" source is unavailable; upstream suspend pending retry")
			continue
		}

		for _, domain := range subscription.Domains {
			// Re-check before every upstream call: a renewal may have landed since the
			// page was read (or since the previous domain was processed).
			stillExpired, checkErr := db.MailSubscriptionStillExpired(ctx, subscription.ID, now)
			if checkErr != nil {
				log.Printf("[Mail] Failed to re-check subscription #%d: %v", subscription.ID, checkErr)
				continue
			}
			if !stillExpired {
				break
			}

			if suspendErr := CraneMailSuspendDomain(ctx, *source, domain.Domain); suspendErr != nil {
				log.Printf("[Mail] Failed to suspend expired mail domain %s; pending retry: %v", domain.Domain, suspendErr)
				mailSchedulerLog(ctx, subscription.UserID, "mail_upstream_suspend_pending_retry",
					"Upstream suspend pending retry for mail domain "+domain.Domain)
				continue
			}

			marked, markErr := db.MailMarkDomainSuspendedIfSubscriptionExpired(ctx, domain.ID, now)
			if markErr != nil {
				log.Printf("[Mail] Failed to mark expired mail domain %s as suspended: %v", domain.Domain, markErr)
				continue
			}
			if marked {
				continue
			}

			// A renewal landed during the upstream call: compensate immediately so the
			// provider state matches the paid state.
			renewed, renewErr := db.MailSubscriptionActiveNow(ctx, subscription.ID, time.Now().UTC())
			if renewErr != nil {
				log.Printf("[Mail] Failed to re-check renewal for subscription #%d: %v", subscription.ID, renewErr)
				continue
			}
			if !renewed {
				continue
			}
			if resumeErr := CraneMailResumeDomain(ctx, *source, domain.Domain); resumeErr != nil {
				log.Printf("[Mail] Failed to compensate resumed mail domain %s; pending retry: %v", domain.Domain, resumeErr)
				if err := db.MailMarkDomainSuspendedForRenewed(ctx, domain.ID, time.Now().UTC()); err != nil {
					log.Printf("[Mail] Failed to persist retry marker for domain %s: %v", domain.Domain, err)
				}
				mailSchedulerLog(ctx, subscription.UserID, "mail_upstream_resume_pending_retry",
					"Upstream resume pending retry for mail domain "+domain.Domain)
				continue
			}
			if err := db.MailRestoreDomainAfterResume(ctx, domain.ID, domain.VerifiedAt != nil, time.Now().UTC()); err != nil {
				log.Printf("[Mail] Failed to restore local status for domain %s: %v", domain.Domain, err)
			}
		}
	}
	return nil
}

// mailResumeRenewedDomains mirrors the third block of checkMailSubscriptionExpiry:
// an active, unexpired subscription whose domains are still marked suspended gets
// them resumed upstream. `suspended` doubles as the persistent retry marker.
func mailResumeRenewedDomains(ctx context.Context, now time.Time) error {
	subscriptions, err := db.MailListActiveSubscriptionsWithSuspendedDomains(ctx, now)
	if err != nil {
		log.Printf("[Mail] Error checking mail subscription expiry: %v", err)
		return err
	}

	for _, subscription := range subscriptions {
		source, sourceErr := db.MailGetSourceByID(ctx, subscription.SourceID)
		if sourceErr != nil {
			log.Printf("[Mail] Cannot load source for renewed subscription #%d: %v", subscription.ID, sourceErr)
			mailSchedulerLog(ctx, subscription.UserID, "mail_upstream_resume_pending_retry",
				"Mail subscription #"+itoa(subscription.ID)+" source could not be loaded; upstream resume pending retry")
			continue
		}
		if source == nil {
			// The TypeScript silently continues when the source is missing.
			continue
		}

		for _, domain := range subscription.Domains {
			if resumeErr := CraneMailResumeDomain(ctx, *source, domain.Domain); resumeErr != nil {
				log.Printf("[Mail] Failed to resume renewed mail domain %s; pending retry: %v", domain.Domain, resumeErr)
				mailSchedulerLog(ctx, subscription.UserID, "mail_upstream_resume_pending_retry",
					"Upstream resume pending retry for mail domain "+domain.Domain)
				continue
			}
			if _, err := db.MailMarkDomainResumedIfSubscriptionActive(ctx, domain.ID, domain.VerifiedAt != nil, time.Now().UTC()); err != nil {
				log.Printf("[Mail] Failed to mark resumed mail domain %s: %v", domain.Domain, err)
			}
		}
	}
	return nil
}

// mailSchedulerLog mirrors createLog for the mail schedulers (no instance id).
func mailSchedulerLog(ctx context.Context, userID int32, action, content string) {
	db.CreateLog(ctx, int64Pointer(int64(userID)), "mail", action, content, "failed", db.CreateLogOptions{})
}

// itoa keeps the string concatenation at the call sites terse.
func itoa(value int32) string {
	return strconv.FormatInt(int64(value), 10)
}

// startMailExpiryScheduler wires the hourly interval with an immediate first run.
func startMailExpiryScheduler(group *schedulerGroup) {
	group.startIntervalLoop("MailExpiry", MailExpirySchedulerInterval, true, func(ctx context.Context) error {
		return CheckMailSubscriptionExpiry(ctx)
	}, nil)
}
