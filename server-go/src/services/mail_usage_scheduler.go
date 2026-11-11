// Port of server/src/services/mail-usage-scheduler.ts.
//
// Daily at 03:17 UTC the scheduler pulls the real disk usage of every active mail
// domain from the provider and stores it locally:
//
//   - CraneMail reports the domain total (`diskUsedMb`);
//   - SmarterMail reports per-account usage, matched to local accounts by lowercased
//     username.
//
// Both upstream calls are best-effort: a provider outage logs and moves on, and an
// individual account write failure never aborts the domain or the sweep.
package services

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// MailUsageSchedulerCronHour / Minute are `17 3 * * *`.
const (
	MailUsageSchedulerCronHour   = 3
	MailUsageSchedulerCronMinute = 17
)

var mailUsageSyncRunning = false

// MailUsageSchedulerNextRun mirrors `schedule('17 3 * * *')` (UTC).
func MailUsageSchedulerNextRun(now time.Time) time.Time {
	return schedulerDailyUTCNextRun(now, MailUsageSchedulerCronHour, MailUsageSchedulerCronMinute)
}

// SyncDomainUsage mirrors syncDomainUsage.
func SyncDomainUsage(ctx context.Context, domainID int32) {
	domain, err := db.MailGetDomainByID(ctx, domainID)
	if err != nil {
		log.Printf("[MailUsage] Usage sync failed for domain %d: %s", domainID, MailAutoRenewSafeError(err))
		return
	}
	// The TypeScript returns silently when the domain vanished, the subscription is
	// no longer active or the source was disabled.
	if domain == nil || domain.Subscription.Status != models.MailSubscriptionStatusActive || !domain.Source.Enabled {
		return
	}

	if usage, usageErr := CraneMailGetDomainUsage(ctx, domain.Source, domain.Domain.Domain); usageErr != nil {
		log.Printf("[MailUsage] CraneMail usage sync failed for domain %d: %s", domain.Domain.ID, MailAutoRenewSafeError(usageErr))
	} else if usage != nil {
		if err := db.MailDomainSetDiskUsed(ctx, domain.Domain.ID, int64(usage.DiskUsedMB)); err != nil {
			log.Printf("[MailUsage] Failed to store domain usage for %d: %s", domain.Domain.ID, MailAutoRenewSafeError(err))
		}
	}

	if domain.Domain.AdminUsername == nil || domain.Domain.AdminPassword == nil {
		log.Printf("[MailUsage] Skipping account usage for domain %d: admin credentials unavailable", domain.Domain.ID)
		return
	}

	upstreamAccounts, smarterErr := SmarterMailListAccountUsage(
		ctx, domain.Source, domain.Domain.Domain,
		*domain.Domain.AdminUsername, *domain.Domain.AdminPassword,
	)
	if smarterErr != nil {
		log.Printf("[MailUsage] SmarterMail usage sync failed for domain %d: %s", domain.Domain.ID, MailAutoRenewSafeError(smarterErr))
		return
	}

	local := make(map[string]models.MailAccount, len(domain.Accounts))
	for _, account := range domain.Accounts {
		local[stringsToLower(account.Username)] = account
	}
	for _, upstream := range upstreamAccounts {
		account, ok := local[stringsToLower(upstream.Username)]
		if !ok {
			continue
		}
		if err := db.MailAccountSetDiskUsed(ctx, account.ID, int32(upstream.DiskUsedMB)); err != nil {
			log.Printf("[MailUsage] Account usage update failed for account %d: %s", account.ID, MailAutoRenewSafeError(err))
		}
	}
}

// SyncMailUsage mirrors syncMailUsage.
func SyncMailUsage(ctx context.Context) {
	if mailUsageSyncRunning {
		log.Printf("[MailUsage] Previous usage sync is still running, skipping")
		return
	}
	mailUsageSyncRunning = true
	defer func() { mailUsageSyncRunning = false }()

	domainIDs, err := db.MailDomainIDsDueForUsageSync(ctx)
	if err != nil {
		log.Printf("[MailUsage] Failed to list domains: %s", MailAutoRenewSafeError(err))
		return
	}
	for _, domainID := range domainIDs {
		// Per-domain isolation: the TypeScript logs and continues.
		runIsolated("MailUsage", func() { SyncDomainUsage(ctx, domainID) })
	}
}

// stringsToLower mirrors `username.toLowerCase()` for the account-matching map.
func stringsToLower(value string) string { return strings.ToLower(value) }

// startMailUsageScheduler wires the 03:17 cron entry.
func startMailUsageScheduler(group *schedulerGroup) {
	group.startLoop("MailUsage", MailUsageSchedulerNextRun, func(ctx context.Context) error {
		SyncMailUsage(ctx)
		return nil
	}, nil)
}

// MailUsageSyncIsRunning exposes the overlap flag to tests.
func MailUsageSyncIsRunning() bool { return mailUsageSyncRunning }

func mailUsageSetRunningForTest(value bool) { mailUsageSyncRunning = value }
