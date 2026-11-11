// Port of server/src/services/billing-scheduler.ts.
//
// Six cron entries, all UTC (see schedulers.go for the timezone rationale):
//
//	0  * * * *   auto-renew        (24h before expiry, max 3 attempts)
//	*/30 * * * * expiry suspend   (paid instances past expires_at)
//	0  3 * * *   expiry delete    (3 days after an expiry suspension)
//	15 * * * *   expiry notify    (3 days before, auto-renew instances skipped)
//	30 * * * *   recharge cleanup (pending orders 6h past expired_at)
//	30 4 * * *   callback cleanup (replay-protection rows older than 30 days)
//
// Money-safety invariants:
//
//   - every per-instance step runs under the `billing:<purpose>:instance:<id>`
//     distributed lock (30 min expiry, 100ms wait) so two ticks cannot act on the
//     same instance;
//   - suspension and deletion are conditional UPDATEs that also bump `version`
//     (optimistic claim), and deletion restores the suspended state when the Incus
//     call fails;
//   - auto-renew re-reads the instance under the lock, re-checks eligibility, then
//     delegates to services.BillingSchedulerPerformRenewal which takes the 4106
//     instance lock and the 4110 balance lock with before/after snapshots.
//
// Failure isolation: the TypeScript wraps each item in try/catch and each job in an
// outer try/catch. A single bad instance must never abort the round — every helper
// below logs and returns.
package services

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// BillingSchedulerAutoRenewHoursBefore is AUTO_RENEW_HOURS_BEFORE.
	BillingSchedulerAutoRenewHoursBefore = 24
	// BillingSchedulerAutoRenewMaxAttempts is AUTO_RENEW_MAX_ATTEMPTS.
	BillingSchedulerAutoRenewMaxAttempts = 3
	// BillingSchedulerSuspendAfterDeleteDays is SUSPEND_AFTER_DELETE_DAYS.
	BillingSchedulerSuspendAfterDeleteDays = 3
	// BillingSchedulerConcurrency is the module-level pLimit(5).
	BillingSchedulerConcurrency = 5
	// BillingSchedulerLockExpireMs is INSTANCE_BILLING_JOB_LOCK_EXPIRE_MS.
	BillingSchedulerLockExpireMs = 30 * 60 * 1000
	// BillingSchedulerLockWaitMs is INSTANCE_BILLING_JOB_LOCK_WAIT_MS.
	BillingSchedulerLockWaitMs = 100
	// BillingSchedulerLockRetryMs is the acquireLock retry interval.
	BillingSchedulerLockRetryMs = 50
	// BillingSchedulerRechargeCancelGrace is CANCEL_GRACE_MS (6 hours).
	BillingSchedulerRechargeCancelGrace = 6 * time.Hour
	// BillingSchedulerPaymentCallbackRetention is the 30-day replay window.
	BillingSchedulerPaymentCallbackRetention = 30 * 24 * time.Hour
	// BillingSchedulerExpiryNotifyDays is EXPIRY_NOTIFY_DAYS = [3].
	BillingSchedulerExpiryNotifyDays = 3
)

// BillingSchedulerExpiryDeleteDelay is the `await delay(1000)` between serial
// deletions. A seam for tests.
var billingSchedulerExpiryDeleteDelay = func(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// Email seams. Production uses lib/mailer.go; tests replace them to prove the
// scheduler never opens an SMTP connection (WAVE_D_SPEC: no real mail in tests).
var (
	billingSchedulerSendExpiryReminder   = lib.SendExpiryReminderEmail
	billingSchedulerSendAutoRenewSuccess = lib.SendAutoRenewSuccessEmail
	billingSchedulerSendAutoRenewFailed  = lib.SendAutoRenewFailedEmail
)

// The "email delivery failed" path is a warning in the TypeScript: a failed
// reminder must never fail the job.
func logBillingEmailFailure(label string, result lib.EmailSendResult) {
	if !result.Success {
		log.Printf("[Billing] Failed to send %s email: %s", label, result.Error)
	}
}

// ==================== next-run (pure) ====================

// BillingSchedulerAutoRenewNextRun mirrors `schedule('0 * * * *')`.
func BillingSchedulerAutoRenewNextRun(now time.Time) time.Time {
	return schedulerHourlyAtNextRun(now, 0)
}

// BillingSchedulerExpirySuspendNextRun mirrors `schedule('*/30 * * * *')`.
func BillingSchedulerExpirySuspendNextRun(now time.Time) time.Time {
	return schedulerEveryNMinutesNextRun(now, 30)
}

// BillingSchedulerExpiryDeleteNextRun mirrors `schedule('0 3 * * *')`.
func BillingSchedulerExpiryDeleteNextRun(now time.Time) time.Time {
	return schedulerDailyUTCNextRun(now, 3, 0)
}

// BillingSchedulerExpiryNotifyNextRun mirrors `schedule('15 * * * *')`.
func BillingSchedulerExpiryNotifyNextRun(now time.Time) time.Time {
	return schedulerHourlyAtNextRun(now, 15)
}

// BillingSchedulerRechargeCleanupNextRun mirrors `schedule('30 * * * *')`.
func BillingSchedulerRechargeCleanupNextRun(now time.Time) time.Time {
	return schedulerHourlyAtNextRun(now, 30)
}

// BillingSchedulerPaymentCallbackCleanupNextRun mirrors `schedule('30 4 * * *')`.
func BillingSchedulerPaymentCallbackCleanupNextRun(now time.Time) time.Time {
	return schedulerDailyUTCNextRun(now, 4, 30)
}

// BillingSchedulerNotifyWindow mirrors notifyExpiringInstances' ±12h window:
// min = now + (days*24 - 12)h, max = now + (days*24 + 12)h.
func BillingSchedulerNotifyWindow(now time.Time, days int) (min, max time.Time) {
	min = now.Add(time.Duration(days*24-12) * time.Hour)
	max = now.Add(time.Duration(days*24+12) * time.Hour)
	return min.UTC(), max.UTC()
}

// BillingSchedulerAutoRenewEligible mirrors the re-read guard in processAutoRenew.
func BillingSchedulerAutoRenewEligible(instance *models.Instance, now, expiryThreshold time.Time) bool {
	if instance == nil || !instance.AutoRenew || instance.ExpiresAt == nil {
		return false
	}
	if !instance.ExpiresAt.After(now) || instance.ExpiresAt.After(expiryThreshold) {
		return false
	}
	return instance.Status != models.InstanceStatusSuspended && instance.Status != models.InstanceStatusDeleted
}

// BillingSchedulerExpiryNotifyEligible mirrors the re-read guard of
// notifyExpiringInstances (including the 24h "already notified" check).
func BillingSchedulerExpiryNotifyEligible(
	instance *models.Instance,
	minThreshold, maxThreshold, recentNotificationCutoff time.Time,
) bool {
	if instance == nil || instance.AutoRenew || instance.ExpiresAt == nil {
		return false
	}
	if instance.ExpiresAt.Before(minThreshold) || !instance.ExpiresAt.Before(maxThreshold) {
		return false
	}
	if instance.Status == models.InstanceStatusSuspended || instance.Status == models.InstanceStatusDeleted {
		return false
	}
	if instance.ExpiryNotifiedAt != nil && !instance.ExpiryNotifiedAt.Before(recentNotificationCutoff) {
		return false
	}
	return true
}

// ==================== helpers ====================

// withInstanceBillingJobLock mirrors withInstanceBillingJobLock.
func withInstanceBillingJobLock(ctx context.Context, purpose string, instanceID int32, fn func()) {
	lockKey := fmt.Sprintf("billing:%s:instance:%d", purpose, instanceID)
	lock, err := lib.AcquireLock(ctx, lockKey, lib.LockOptions{
		ExpireMs:        intPointer(BillingSchedulerLockExpireMs),
		WaitTimeoutMs:   intPointer(BillingSchedulerLockWaitMs),
		RetryIntervalMs: intPointer(BillingSchedulerLockRetryMs),
	})
	if err != nil || !lock.Success || lock.OwnerID == "" {
		log.Printf("[Billing] Skip %s for instance %d: lock not acquired", purpose, instanceID)
		return
	}
	defer lib.ReleaseLock(ctx, lockKey, lock.OwnerID)
	fn()
}

// billingSchedulerCreateLog is a small wrapper that keeps the CreateLog call sites
// readable (userId is always present here).
func billingSchedulerCreateLog(ctx context.Context, userID int32, module, action, content, result string, instanceID int32) {
	db.CreateLog(ctx, int64Pointer(int64(userID)), module, action, content, result, db.LogOptionsWithInstance(&instanceID))
}

// ==================== auto renew ====================

// executeAutoRenew mirrors executeAutoRenew.
func executeAutoRenew(ctx context.Context) {
	log.Printf("[Billing] Starting auto-renew job...")
	start := time.Now()

	instances, err := db.GetAutoRenewInstances(ctx, BillingSchedulerAutoRenewHoursBefore)
	if err != nil {
		log.Printf("[Billing] Auto-renew job failed: %v", err)
		return
	}
	log.Printf("[Billing] Found %d instances for auto-renew", len(instances))

	semaphore := make(chan struct{}, BillingSchedulerConcurrency)
	var wait sync.WaitGroup
	for _, instance := range instances {
		wait.Add(1)
		go func(instanceID int32) {
			defer wait.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			runIsolated("Billing", func() {
				withInstanceBillingJobLock(ctx, "auto-renew", instanceID, func() {
					processAutoRenew(ctx, instanceID)
				})
			})
		}(instance.ID)
	}
	wait.Wait()
	log.Printf("[Billing] Auto-renew job completed in %dms", time.Since(start).Milliseconds())
}

// processAutoRenew mirrors processAutoRenew.
func processAutoRenew(ctx context.Context, instanceID int32) {
	now := time.Now().UTC()
	expiryThreshold := now.Add(BillingSchedulerAutoRenewHoursBefore * time.Hour)

	instance, err := db.BillingSchedulerInstanceRow(ctx, instanceID)
	if err != nil {
		log.Printf("[Billing] Auto-renew failed for instance %d: %v", instanceID, err)
		return
	}
	if !BillingSchedulerAutoRenewEligible(instance, now, expiryThreshold) {
		log.Printf("[Billing] Skip auto-renew for instance %d: no longer eligible", instanceID)
		return
	}

	attempts := instance.AutoRenewAttempts + 1
	guard := &db.AutoRenewAttemptGuard{
		Now:             now,
		ExpiryThreshold: expiryThreshold,
		Version:         instance.Version,
		CurrentAttempts: instance.AutoRenewAttempts,
	}

	claimAttempt := func(disable bool) bool {
		claimed, claimErr := db.UpdateAutoRenewAttempt(ctx, instance.ID, attempts, disable, guard)
		if claimErr != nil {
			log.Printf("[Billing] Failed to update auto-renew attempt for instance %d: %v", instance.ID, claimErr)
			return false
		}
		if !claimed {
			log.Printf("[Billing] Skip auto-renew attempt update for instance %d: no longer eligible", instance.ID)
		}
		return claimed
	}

	if attempts > BillingSchedulerAutoRenewMaxAttempts {
		log.Printf("[Billing] Instance %d exceeded max auto-renew attempts, disabling", instance.ID)
		claimAttempt(true)
		return
	}

	renewMonths := 1
	if instance.BillingCycle != nil && *instance.BillingCycle > 0 {
		renewMonths = int(*instance.BillingCycle)
	}

	_, renewAmountBase, _, calcErr := BillingSchedulerCalculateRenewBilling(instance, renewMonths, now)
	if calcErr != nil {
		log.Printf("[Billing] Auto-renew failed for instance %d: %v", instance.ID, calcErr)
		claimAttempt(false)
		return
	}

	user, err := db.FindUserById(ctx, instance.UserID)
	if err != nil {
		log.Printf("[Billing] Auto-renew failed for instance %d: %v", instance.ID, err)
		claimAttempt(false)
		return
	}
	if user == nil {
		log.Printf("[Billing] User %d not found for instance %d", instance.UserID, instance.ID)
		claimAttempt(false)
		return
	}

	balance, err := db.GetUserBalance(ctx, instance.UserID)
	if err != nil {
		log.Printf("[Billing] Auto-renew failed for instance %d: %v", instance.ID, err)
		claimAttempt(false)
		return
	}

	// The balance check reproduces the TypeScript's own arithmetic: it arbitrates the
	// max(legacy AFF rate, promo rate) against the VIP discount *before* calling
	// performRenewal (which recomputes the authoritative amount). The two can differ
	// by a cent when a promo code and AFF both apply; the charged amount is always
	// the one performRenewal returns.
	candidateAmount := renewAmountBase
	affBinding, affErr := db.AffGetInstanceBinding(ctx, instance.ID, nil)
	if affErr != nil {
		log.Printf("[Billing] Auto-renew failed for instance %d: %v", instance.ID, affErr)
		claimAttempt(false)
		return
	}
	promoBinding, promoErr := db.PromoGetInstanceBinding(ctx, instance.ID, nil)
	if promoErr != nil {
		log.Printf("[Billing] Auto-renew failed for instance %d: %v", instance.ID, promoErr)
		claimAttempt(false)
		return
	}

	monthlyPrice := BillingSchedulerMonthlyPrice(billingPriceOf(instance), instance.BillingCycle)
	effectiveDiscountRate := 0.0
	if affBinding != nil && affBinding.AffCode.Enabled {
		effectiveDiscountRate = affBinding.AffCode.DiscountRate.InexactFloat64()
	}
	if promoBinding != nil && promoBinding.PromoCode.Enabled &&
		(promoBinding.PromoCode.ExpiresAt == nil || promoBinding.PromoCode.ExpiresAt.After(now)) &&
		renewAmountBase > 0 {
		quote := PromoQuoteRenewal(monthlyPrice, renewMonths, promoBinding)
		if rate := quote.DiscountAmount / renewAmountBase; rate > effectiveDiscountRate {
			effectiveDiscountRate = rate
		}
	}

	_, vipBenefit, vipErr := VipGetUserContinuousVipBenefit(ctx, instance.UserID, nil)
	if vipErr != nil {
		log.Printf("[Billing] Auto-renew failed for instance %d: %v", instance.ID, vipErr)
		claimAttempt(false)
		return
	}
	vipPercent := vipBenefit.OrderDiscountPercent
	candidateAmount = VipArbitratePrice(VipPriceArbitrationInput{
		BasePrice:          renewAmountBase,
		AffDiscountRate:    &effectiveDiscountRate,
		VipDiscountPercent: &vipPercent,
	}).FinalPrice

	if balance < candidateAmount {
		log.Printf("[Billing] Insufficient balance for instance %d: need %.2f, have %.2f", instance.ID, candidateAmount, balance)
		if !claimAttempt(false) {
			return
		}

		failReason := fmt.Sprintf("余额不足（需要 ¥%.2f，当前 ¥%.2f）", candidateAmount, balance)
		lib.SendNotification(ctx, int64(instance.UserID), lib.EventAutoRenewFailed, lib.EventData{
			"instanceName": instance.Name,
			"failReason":   failReason,
			"renewAmount":  candidateAmount,
		})
		if user.Email != nil && *user.Email != "" {
			logBillingEmailFailure("auto-renew failed", billingSchedulerSendAutoRenewFailed(ctx, *user.Email, lib.AutoRenewFailedEmailData{
				Username:       user.Username,
				InstanceName:   instance.Name,
				FailReason:     failReason,
				CurrentAttempt: int(attempts),
				MaxAttempts:    BillingSchedulerAutoRenewMaxAttempts,
				ExpiresAt:      *instance.ExpiresAt,
			}))
		}
		billingSchedulerCreateLog(ctx, instance.UserID, "instance", "instance.auto_renew_failed",
			fmt.Sprintf("Auto-renew failed for instance \"%s\": insufficient balance", instance.Name),
			"failed", instance.ID)
		return
	}

	result, renewErr := BillingSchedulerPerformRenewal(ctx, instance.UserID, instance, renewMonths)
	if renewErr != nil {
		errorMessage := renewErr.Error()
		log.Printf("[Billing] Auto-renew failed for instance %d: %s", instance.ID, errorMessage)
		if !claimAttempt(false) {
			return
		}
		lib.SendNotification(ctx, int64(instance.UserID), lib.EventAutoRenewFailed, lib.EventData{
			"instanceName": instance.Name,
			"failReason":   errorMessage,
		})
		if user.Email != nil && *user.Email != "" {
			logBillingEmailFailure("auto-renew failed", billingSchedulerSendAutoRenewFailed(ctx, *user.Email, lib.AutoRenewFailedEmailData{
				Username:       user.Username,
				InstanceName:   instance.Name,
				FailReason:     errorMessage,
				CurrentAttempt: int(attempts),
				MaxAttempts:    BillingSchedulerAutoRenewMaxAttempts,
				ExpiresAt:      *instance.ExpiresAt,
			}))
		}
		billingSchedulerCreateLog(ctx, instance.UserID, "instance", "instance.auto_renew_failed",
			fmt.Sprintf("Auto-renew failed for instance \"%s\": %s", instance.Name, errorMessage),
			"failed", instance.ID)
		return
	}

	lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceRenewed, lib.EventData{
		"instanceName": instance.Name,
		"renewAmount":  result.Amount,
		"renewMonths":  renewMonths,
		"newExpiresAt": result.NewExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	})

	if user.Email != nil && *user.Email != "" {
		newBalance, balanceErr := db.GetUserBalance(ctx, instance.UserID)
		if balanceErr != nil {
			log.Printf("[Billing] Failed to read balance after renewal for user %d: %v", instance.UserID, balanceErr)
		}
		logBillingEmailFailure("auto-renew success", billingSchedulerSendAutoRenewSuccess(ctx, *user.Email, lib.AutoRenewSuccessEmailData{
			Username:     user.Username,
			InstanceName: instance.Name,
			Amount:       result.Amount,
			Months:       renewMonths,
			NewExpiresAt: result.NewExpiresAt,
			NewBalance:   newBalance,
		}))
	}

	billingSchedulerCreateLog(ctx, instance.UserID, "instance", "instance.auto_renew",
		fmt.Sprintf("Auto-renewed instance \"%s\" for %d month(s), amount: %.2f", instance.Name, renewMonths, result.Amount),
		"success", instance.ID)
	log.Printf("[Billing] Auto-renewed instance %d", instance.ID)
}

func billingPriceOf(instance *models.Instance) float64 {
	if instance.BillingPrice == nil {
		return 0
	}
	return instance.BillingPrice.InexactFloat64()
}

// ==================== expiry suspend ====================

// executeExpirySuspend mirrors executeExpirySuspend.
func executeExpirySuspend(ctx context.Context) {
	log.Printf("[Billing] Starting expiry suspend job...")
	start := time.Now()

	instances, err := db.GetExpiredUnsuspendedPaidInstances(ctx)
	if err != nil {
		log.Printf("[Billing] Expiry suspend job failed: %v", err)
		return
	}
	log.Printf("[Billing] Found %d expired instances to suspend", len(instances))

	semaphore := make(chan struct{}, BillingSchedulerConcurrency)
	var wait sync.WaitGroup
	for _, instance := range instances {
		wait.Add(1)
		go func(instanceID int32) {
			defer wait.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			runIsolated("Billing", func() {
				withInstanceBillingJobLock(ctx, "expiry-suspend", instanceID, func() {
					processExpirySuspend(ctx, instanceID)
				})
			})
		}(instance.ID)
	}
	wait.Wait()
	log.Printf("[Billing] Expiry suspend job completed in %dms", time.Since(start).Milliseconds())
}

// processExpirySuspend mirrors processExpirySuspend.
func processExpirySuspend(ctx context.Context, instanceID int32) {
	suspended, err := db.SuspendInstanceByExpiry(ctx, instanceID)
	if err != nil {
		log.Printf("[Billing] Failed to suspend instance %d: %v", instanceID, err)
		return
	}
	if suspended == nil {
		log.Printf("[Billing] Skip expiry suspend for instance %d: no longer expired or already processed", instanceID)
		return
	}

	// A failed stop must not undo the suspension: the user is over quota either way,
	// and the next status sync will observe the instance state.
	if host, hostErr := db.HostsGetByID(ctx, suspended.HostID); hostErr != nil {
		log.Printf("[Billing] Failed to load host for instance %d: %v", suspended.ID, hostErr)
	} else if host != nil {
		if client, clientErr := lib.GetIncusClientForHost(ctx, host); clientErr != nil {
			log.Printf("[Billing] Failed to stop Incus instance %s: %v", suspended.IncusID, clientErr)
		} else if _, stopErr := lib.IncusInstanceStop(ctx, client, suspended.IncusID, true, lib.IncusInstanceBusyRetryOptions{}); stopErr != nil {
			log.Printf("[Billing] Failed to stop Incus instance %s: %v", suspended.IncusID, stopErr)
		}
	}

	lib.SendNotification(ctx, int64(suspended.UserID), lib.EventInstanceSuspended, lib.EventData{
		"instanceName":  suspended.Name,
		"suspendReason": "实例已到期",
	})
	billingSchedulerCreateLog(ctx, suspended.UserID, "instance", "instance.suspended_expired",
		fmt.Sprintf("Instance \"%s\" suspended due to expiry", suspended.Name), "success", suspended.ID)
	log.Printf("[Billing] Suspended expired instance %d", suspended.ID)
}

// ==================== expiry delete ====================

// executeExpiryDelete mirrors executeExpiryDelete: serial by design (a burst of
// deletions would hammer the hosts), one second apart.
func executeExpiryDelete(ctx context.Context) {
	log.Printf("[Billing] Starting expiry delete job...")
	start := time.Now()

	instances, err := db.GetSuspendedInstancesOlderThan(ctx, BillingSchedulerSuspendAfterDeleteDays)
	if err != nil {
		log.Printf("[Billing] Expiry delete job failed: %v", err)
		return
	}

	expired := make([]models.Instance, 0, len(instances))
	for _, instance := range instances {
		if instance.SuspendReason != nil && *instance.SuspendReason == "expired" {
			expired = append(expired, instance)
		}
	}
	log.Printf("[Billing] Found %d expired instances to delete", len(expired))

	for _, instance := range expired {
		instanceID := instance.ID
		runIsolated("Billing", func() {
			withInstanceBillingJobLock(ctx, "expiry-delete", instanceID, func() {
				processExpiryDelete(ctx, instanceID)
			})
		})
		billingSchedulerExpiryDeleteDelay(ctx, time.Second)
	}
	log.Printf("[Billing] Expiry delete job completed in %dms", time.Since(start).Milliseconds())
}

// processExpiryDelete mirrors processExpiryDelete.
func processExpiryDelete(ctx context.Context, instanceID int32) {
	instance, err := db.BillingSchedulerInstanceRow(ctx, instanceID)
	if err != nil || instance == nil {
		log.Printf("[Billing] Failed to load instance %d for expiry delete: %v", instanceID, err)
		return
	}

	deleteThreshold := time.Now().UTC().AddDate(0, 0, -BillingSchedulerSuspendAfterDeleteDays)
	claimed, err := db.ExpiryDeleteClaim(ctx, instance.ID, instance.Version, deleteThreshold)
	if err != nil {
		log.Printf("[Billing] Failed to delete instance %d: %v", instance.ID, err)
		return
	}
	if !claimed {
		log.Printf("[Billing] Skip expiry delete for instance %d: no longer eligible or already processed", instance.ID)
		return
	}

	host, hostErr := db.HostsGetByID(ctx, instance.HostID)
	if hostErr != nil || host == nil {
		if restoreErr := db.RestoreExpiryDeleteClaim(ctx, instance.ID); restoreErr != nil {
			log.Printf("[Billing] Failed to restore expired suspension for instance %d: %v", instance.ID, restoreErr)
		}
		log.Printf("[Billing] Skip expiry delete for instance %d: host not found (%v)", instance.ID, hostErr)
		return
	}

	client, clientErr := lib.GetIncusClientForHost(ctx, host)
	if clientErr != nil {
		if restoreErr := db.RestoreExpiryDeleteClaim(ctx, instance.ID); restoreErr != nil {
			log.Printf("[Billing] Failed to restore expired suspension for instance %d: %v", instance.ID, restoreErr)
		}
		log.Printf("[Billing] Failed to delete Incus instance %s, restored expired suspension: %v", instance.IncusID, clientErr)
		return
	}
	if _, deleteErr := lib.IncusInstanceDelete(ctx, client, instance.IncusID); deleteErr != nil {
		if restoreErr := db.RestoreExpiryDeleteClaim(ctx, instance.ID); restoreErr != nil {
			log.Printf("[Billing] Failed to restore expired suspension for instance %d: %v", instance.ID, restoreErr)
		}
		log.Printf("[Billing] Failed to delete Incus instance %s, restored expired suspension: %v", instance.IncusID, deleteErr)
		return
	}

	portCount, err := db.CountInstancePortMappings(ctx, instance.ID)
	if err != nil {
		log.Printf("[Billing] Failed to count port mappings for instance %d: %v", instance.ID, err)
	}
	portCount32 := int32(portCount)
	if err := db.RollbackResources(ctx, db.RollbackResourcesOptions{
		HostID:    instance.HostID,
		CPU:       instance.CPU,
		Memory:    instance.Memory,
		Disk:      instance.Disk,
		PortCount: portCount32,
	}); err != nil {
		log.Printf("[Billing] Failed to roll back resources for instance %d: %v", instance.ID, err)
	}

	// The row stays as a soft-deleted instance so billing records keep their link;
	// only the network allocations that used to cascade are released explicitly.
	if err := db.DeleteInstanceNetworkAllocations(ctx, instance.ID); err != nil {
		log.Printf("[Billing] Failed to release network allocations for instance %d: %v", instance.ID, err)
	}

	used, err := db.CalculateHostResourcesFromInstances(ctx, instance.HostID)
	if err != nil {
		log.Printf("[Billing] Failed to recalculate host resources for host %d: %v", instance.HostID, err)
	} else {
		if err := db.UpdateHostResources(ctx, instance.HostID, &used.CPUUsed, &used.MemoryUsed, &used.DiskUsed); err != nil {
			log.Printf("[Billing] Failed to update host resources for host %d: %v", instance.HostID, err)
		}
	}
	actualPorts, err := db.CountHostActivePortMappings(ctx, instance.HostID)
	if err != nil {
		log.Printf("[Billing] Failed to count active ports for host %d: %v", instance.HostID, err)
	} else if err := db.UpdateHostNATPortsUsed(ctx, instance.HostID, actualPorts); err != nil {
		log.Printf("[Billing] Failed to update NAT port count for host %d: %v", instance.HostID, err)
	}

	lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceDeleted, lib.EventData{
		"instanceName": instance.Name,
	})
	billingSchedulerCreateLog(ctx, instance.UserID, "instance", "instance.deleted_expired",
		fmt.Sprintf("Instance \"%s\" automatically deleted after %d days of suspension", instance.Name, BillingSchedulerSuspendAfterDeleteDays),
		"success", instance.ID)
	log.Printf("[Billing] Deleted expired instance %d", instance.ID)

	if instance.PackageID != nil {
		params := lib.ReleaseNotificationParams{
			PackageID: *instance.PackageID,
			CPU:       instance.CPU,
			Memory:    instance.Memory,
			Source:    "实例过期自动删除",
		}
		// Fire-and-forget in the TypeScript (`.catch(() => {})`); the release
		// notification goes to a package channel and must not delay the round.
		go lib.SendReleaseNotification(ctx, params)
	}
}

// ==================== expiry notify ====================

// executeExpiryNotify mirrors executeExpiryNotify.
func executeExpiryNotify(ctx context.Context) {
	log.Printf("[Billing] Starting expiry notify job...")
	start := time.Now()
	notifyExpiringInstances(ctx, BillingSchedulerExpiryNotifyDays)
	log.Printf("[Billing] Expiry notify job completed in %dms", time.Since(start).Milliseconds())
}

// notifyExpiringInstances mirrors notifyExpiringInstances.
//
// The TypeScript loads a Prisma page with a ±12h window, then re-reads each row
// under the lock and claims `expiryNotifiedAt` before sending. That double read is
// what makes "exactly one reminder per 24h" work across overlapping ticks, so the
// Go port keeps both reads and the conditional claim.
func notifyExpiringInstances(ctx context.Context, days int) {
	now := time.Now().UTC()
	minThreshold, maxThreshold := BillingSchedulerNotifyWindow(now, days)
	recentCutoff := now.Add(-24 * time.Hour)

	candidates, err := db.InstanceListExpiryNotifyCandidates(ctx, minThreshold, maxThreshold, recentCutoff)
	if err != nil {
		log.Printf("[Billing] Expiry notify job failed: %v", err)
		return
	}
	log.Printf("[Billing] Found %d instances expiring in %d days", len(candidates), days)

	semaphore := make(chan struct{}, BillingSchedulerConcurrency)
	var wait sync.WaitGroup
	for _, candidate := range candidates {
		wait.Add(1)
		go func(instanceID int32) {
			defer wait.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			runIsolated("Billing", func() {
				withInstanceBillingJobLock(ctx, "expiry-notify", instanceID, func() {
					processExpiryNotify(ctx, instanceID, days, minThreshold, maxThreshold, recentCutoff)
				})
			})
		}(candidate.ID)
	}
	wait.Wait()
}

// processExpiryNotify is one item of notifyExpiringInstances.
func processExpiryNotify(
	ctx context.Context,
	instanceID int32,
	days int,
	minThreshold, maxThreshold, recentNotificationCutoff time.Time,
) {
	latest, err := db.BillingSchedulerInstanceRow(ctx, instanceID)
	if err != nil {
		log.Printf("[Billing] Failed to notify instance %d: %v", instanceID, err)
		return
	}
	if !BillingSchedulerExpiryNotifyEligible(latest, minThreshold, maxThreshold, recentNotificationCutoff) {
		log.Printf("[Billing] Skip expiry notification for instance %d: no longer eligible", instanceID)
		return
	}

	claimed, err := db.UpdateExpiryNotifiedAt(ctx, latest.ID, &db.ExpiryNotificationClaimGuard{
		MinExpiresAt:             minThreshold,
		MaxExpiresAt:             maxThreshold,
		RecentNotificationCutoff: recentNotificationCutoff,
	})
	if err != nil {
		log.Printf("[Billing] Failed to notify instance %d: %v", latest.ID, err)
		return
	}
	if !claimed {
		log.Printf("[Billing] Skip expiry notification for instance %d: notification already claimed or no longer eligible", latest.ID)
		return
	}

	lib.SendNotification(ctx, int64(latest.UserID), lib.EventInstanceExpiring, lib.EventData{
		"instanceName":  latest.Name,
		"daysRemaining": days,
		"newExpiresAt":  latest.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	})

	user, userErr := db.FindUserById(ctx, latest.UserID)
	if userErr != nil {
		log.Printf("[Billing] Failed to load user %d for expiry email: %v", latest.UserID, userErr)
	} else if user != nil && user.Email != nil && *user.Email != "" {
		logBillingEmailFailure("expiry reminder", billingSchedulerSendExpiryReminder(ctx, *user.Email, lib.ExpiryReminderEmailData{
			Username:      user.Username,
			InstanceName:  latest.Name,
			DaysRemaining: days,
			ExpiresAt:     *latest.ExpiresAt,
		}))
	}
	log.Printf("[Billing] Sent expiry notification for instance %d", latest.ID)
}

// ==================== maintenance ====================

// cleanExpiredRechargeOrders mirrors cleanExpiredRechargeOrders.
func cleanExpiredRechargeOrders(ctx context.Context) {
	log.Printf("[Billing] Starting expired recharge orders cleanup job...")
	start := time.Now()
	count, err := db.CancelStalePendingRechargeOrders(ctx, BillingSchedulerRechargeCancelGrace)
	if err != nil {
		log.Printf("[Billing] Expired recharge orders cleanup failed: %v", err)
		return
	}
	if count > 0 {
		log.Printf("[Billing] Cancelled %d expired recharge orders in %dms", count, time.Since(start).Milliseconds())
	}
}

// cleanOldPaymentCallbacks mirrors cleanOldPaymentCallbacks.
func cleanOldPaymentCallbacks(ctx context.Context) {
	log.Printf("[Billing] Starting old payment callbacks cleanup job...")
	start := time.Now()
	count, err := db.DeleteOldPaymentCallbacks(ctx, time.Now().UTC().Add(-BillingSchedulerPaymentCallbackRetention))
	if err != nil {
		log.Printf("[Billing] Old payment callbacks cleanup failed: %v", err)
		return
	}
	if count > 0 {
		log.Printf("[Billing] Deleted %d old payment callback records in %dms", count, time.Since(start).Milliseconds())
	}
}

// ==================== scheduler entry ====================

// RunAllBillingJobs mirrors runAllBillingJobs (manual trigger / tests).
func RunAllBillingJobs(ctx context.Context) {
	executeAutoRenew(ctx)
	executeExpirySuspend(ctx)
	executeExpiryDelete(ctx)
	executeExpiryNotify(ctx)
}

// startBillingScheduler wires the six billing cron entries.
func startBillingScheduler(group *schedulerGroup) {
	group.startLoop("BillingAutoRenew", BillingSchedulerAutoRenewNextRun, func(ctx context.Context) error {
		executeAutoRenew(ctx)
		return nil
	}, nil)
	group.startLoop("BillingExpirySuspend", BillingSchedulerExpirySuspendNextRun, func(ctx context.Context) error {
		executeExpirySuspend(ctx)
		return nil
	}, nil)
	group.startLoop("BillingExpiryDelete", BillingSchedulerExpiryDeleteNextRun, func(ctx context.Context) error {
		executeExpiryDelete(ctx)
		return nil
	}, nil)
	group.startLoop("BillingExpiryNotify", BillingSchedulerExpiryNotifyNextRun, func(ctx context.Context) error {
		executeExpiryNotify(ctx)
		return nil
	}, nil)
	group.startLoop("BillingRechargeCleanup", BillingSchedulerRechargeCleanupNextRun, func(ctx context.Context) error {
		cleanExpiredRechargeOrders(ctx)
		return nil
	}, nil)
	group.startLoop("BillingPaymentCallbackCleanup", BillingSchedulerPaymentCallbackCleanupNextRun, func(ctx context.Context) error {
		cleanOldPaymentCallbacks(ctx)
		return nil
	}, nil)
}
