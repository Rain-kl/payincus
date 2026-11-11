// Port of server/src/services/mail-autorenew-scheduler.ts.
//
// Daily at 02:43 UTC the scheduler renews mail subscriptions that expire within
// three days, for exactly one month, from the owner's main balance.
//
// Transaction contract (identical to the TypeScript):
//
//   - one transaction per candidate;
//   - `pg_try_advisory_xact_lock(4109, userId)` first, then
//     `(4110, userId)` — a busy lock is a skip, never a block;
//   - the subscription is re-read INSIDE the locks and must still be active,
//     auto-renewing, still due, and expire at exactly the scanned instant (the
//     timestamp is the optimistic guard);
//   - the charge is idempotent through the `[mail-auto-renew:<id>:<expiresAt>]`
//     marker in the balance log remark;
//   - a failed debit writes an inbox message (deduped by periodKey) and a log;
//   - a successful debit writes the balance log snapshot pair, renews the
//     subscription, and pays the AFF commission in the same transaction.
//
// The 1-month period is fixed: `MAIL_AUTORENEW_MONTHS = 1`, independent of the
// plan's billing cycle (a yearly plan is charged one twelfth per month).
package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// MailAutoRenewDaysAhead is MAIL_AUTORENEW_DAYS_AHEAD.
	MailAutoRenewDaysAhead = 3
	// MailAutoRenewMonths is MAIL_AUTORENEW_MONTHS.
	MailAutoRenewMonths = 1
	// MailRenewRemarkPrefix is MAIL_RENEW_REMARK_PREFIX.
	MailRenewRemarkPrefix = "续费域名邮箱 "
)

var mailAutoRenewRunning = false

// Seams: tests drive the per-item loop without a database. Production points at
// the real scan/processor.
var (
	mailAutoRenewScan     = db.MailGetExpiringSubscriptions
	mailAutoRenewProcess  = ProcessMailAutoRenew
	mailAutoRenewWriteLog = func(ctx context.Context, userID int32, action, content, result string) {
		mailSchedulerLogWithResult(ctx, userID, action, content, result)
	}
)

// MailAutoRenewSchedulerNextRun mirrors `schedule('43 2 * * *')` (UTC).
func MailAutoRenewSchedulerNextRun(now time.Time) time.Time {
	return schedulerDailyUTCNextRun(now, 2, 43)
}

// MailAutoRenewPeriodKey mirrors buildPeriodKey.
func MailAutoRenewPeriodKey(subscriptionID int32, expiresAt time.Time) string {
	return fmt.Sprintf("mail-auto-renew:%d:%s", subscriptionID, expiresAt.UTC().Format("2006-01-02T15:04:05.000Z"))
}

// MailAutoRenewStatus is the AutoRenewResult discriminant.
type MailAutoRenewStatus string

const (
	MailAutoRenewStatusRenewed             MailAutoRenewStatus = "renewed"
	MailAutoRenewStatusInsufficientBalance MailAutoRenewStatus = "insufficient_balance"
	MailAutoRenewStatusDuplicate           MailAutoRenewStatus = "duplicate"
	MailAutoRenewStatusIneligible          MailAutoRenewStatus = "ineligible"
	MailAutoRenewStatusBusy                MailAutoRenewStatus = "busy"
)

// MailAutoRenewResult mirrors AutoRenewResult.
type MailAutoRenewResult struct {
	Status    MailAutoRenewStatus
	Amount    float64
	Balance   float64
	ExpiresAt time.Time
}

// mailAutoRenewRound2 mirrors roundCurrency (`Number(value.toFixed(2))`).
func mailAutoRenewRound2(value float64) float64 {
	return lib.JSFixed(value, 2)
}

// MailAutoRenewSafeError mirrors safeErrorMessage (token redaction).
func MailAutoRenewSafeError(err error) string {
	if err == nil {
		return ""
	}
	return lib.SanitizeTokensInString(err.Error())
}

// MailAutoRenewSubscriptionEligible mirrors the guard inside the transaction. The
// expiry equality check is the optimistic guard: a renewal that moved expires_at
// makes this candidate stale.
func MailAutoRenewSubscriptionEligible(
	subscription *models.MailSubscription,
	userID int32,
	candidateExpiresAt, scanNow, deadline time.Time,
) bool {
	if subscription == nil {
		return false
	}
	if subscription.UserID != userID || subscription.Status != models.MailSubscriptionStatusActive || !subscription.AutoRenew {
		return false
	}
	if !subscription.ExpiresAt.Equal(candidateExpiresAt) {
		return false
	}
	return subscription.ExpiresAt.After(scanNow) && !subscription.ExpiresAt.After(deadline)
}

// ProcessMailAutoRenew mirrors processMailAutoRenew: one candidate, one
// transaction, both advisory locks.
func ProcessMailAutoRenew(ctx context.Context, candidate db.MailExpiringSubscription, scanNow, deadline time.Time) (MailAutoRenewResult, error) {
	if db.Pool() == nil {
		return MailAutoRenewResult{}, errors.New("database not initialized")
	}
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return MailAutoRenewResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := db.TryAdvisoryTransactionLock(ctx, tx, db.MailSubscriptionLockNamespace, candidate.UserID)
	if err != nil {
		return MailAutoRenewResult{}, err
	}
	if !locked {
		return MailAutoRenewResult{Status: MailAutoRenewStatusBusy}, nil
	}
	balanceLocked, err := db.TryAdvisoryTransactionLock(ctx, tx, db.UserBalanceLockNamespace, candidate.UserID)
	if err != nil {
		return MailAutoRenewResult{}, err
	}
	if !balanceLocked {
		return MailAutoRenewResult{Status: MailAutoRenewStatusBusy}, nil
	}

	loaded, err := db.MailAutoRenewLoad(ctx, tx, candidate.ID)
	if err != nil {
		return MailAutoRenewResult{}, err
	}
	if loaded == nil {
		return MailAutoRenewResult{Status: MailAutoRenewStatusIneligible}, nil
	}
	subscription := loaded.Subscription
	if !MailAutoRenewSubscriptionEligible(&subscription, candidate.UserID, candidate.ExpiresAt, scanNow, deadline) {
		return MailAutoRenewResult{Status: MailAutoRenewStatusIneligible}, nil
	}

	periodKey := MailAutoRenewPeriodKey(subscription.ID, subscription.ExpiresAt)
	marker := "[" + periodKey + "]"
	charged, err := db.MailAutoRenewChargeExists(ctx, tx, subscription.UserID, marker)
	if err != nil {
		return MailAutoRenewResult{}, err
	}
	if charged {
		return MailAutoRenewResult{Status: MailAutoRenewStatusDuplicate}, nil
	}

	monthlyPrice := loaded.Plan.Price.InexactFloat64()
	if loaded.Plan.BillingCycle != models.MailBillingCycleMonthly {
		monthlyPrice /= 12
	}
	originalPrice := mailAutoRenewRound2(monthlyPrice * MailAutoRenewMonths)
	discountAmount := 0.0
	finalPrice := originalPrice
	if loaded.AffCode != nil && loaded.AffCode.Enabled {
		rate := loaded.AffCode.DiscountRate.InexactFloat64()
		discountAmount = lib.CalculateDiscountAmount(originalPrice, rate)
		finalPrice = lib.CalculateDiscountedPrice(originalPrice, rate)
	}

	deducted, err := db.MailAutoRenewDebitBalance(ctx, tx, subscription.UserID, finalPrice)
	if err != nil {
		return MailAutoRenewResult{}, err
	}
	if !deducted {
		balance, balanceErr := db.UserBalanceNumberTx(ctx, tx, subscription.UserID)
		if balanceErr != nil {
			return MailAutoRenewResult{}, balanceErr
		}
		exists, existsErr := db.MailAutoRenewReminderExists(ctx, tx, subscription.UserID,
			"mail_auto_renew_insufficient_balance", periodKey)
		if existsErr != nil {
			return MailAutoRenewResult{}, existsErr
		}
		if !exists {
			title := "邮箱自动续费余额不足"
			content := fmt.Sprintf("邮箱订阅自动续费需要 ¥%.2f，当前余额 ¥%.2f。请充值后手动续费。", finalPrice, balance)
			if err := db.MailAutoRenewCreateInsufficientReminder(ctx, tx, subscription.UserID,
				subscription.ID, periodKey, MailAutoRenewMonths, finalPrice, balance,
				subscription.ExpiresAt, title, content); err != nil {
				return MailAutoRenewResult{}, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return MailAutoRenewResult{}, err
		}
		return MailAutoRenewResult{
			Status:  MailAutoRenewStatusInsufficientBalance,
			Amount:  finalPrice,
			Balance: balance,
		}, nil
	}

	balanceAfter, err := db.UserBalanceNumberTx(ctx, tx, subscription.UserID)
	if err != nil {
		return MailAutoRenewResult{}, err
	}
	balanceBefore := mailAutoRenewRound2(balanceAfter + finalPrice)

	remark := fmt.Sprintf("%s%d 个月（自动续费） %s", MailRenewRemarkPrefix, MailAutoRenewMonths, marker)
	if discountAmount > 0 {
		remark = fmt.Sprintf("%s%d 个月（自动续费，优惠码折扣 -¥%.2f） %s",
			MailRenewRemarkPrefix, MailAutoRenewMonths, discountAmount, marker)
	}
	if err := db.MailAutoRenewCreateBalanceLog(ctx, tx, subscription.UserID,
		-finalPrice, balanceBefore, balanceAfter, remark); err != nil {
		return MailAutoRenewResult{}, err
	}

	renewed, err := db.MailRenewSubscription(ctx, subscription.ID, MailAutoRenewMonths, tx)
	if err != nil {
		return MailAutoRenewResult{}, err
	}

	if loaded.AffBinding != nil {
		// The TypeScript calls processMailAffCommission whenever the binding exists
		// (the commission helper itself does not check `enabled`).
		if err := db.AffProcessMailCommission(ctx, loaded.AffBinding.AffCodeID, subscription.ID, originalPrice, models.AffLogTypeRenew, tx); err != nil {
			return MailAutoRenewResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return MailAutoRenewResult{}, err
	}
	return MailAutoRenewResult{
		Status:    MailAutoRenewStatusRenewed,
		Amount:    finalPrice,
		ExpiresAt: renewed.ExpiresAt,
	}, nil
}

// RunMailAutoRenew mirrors runMailAutoRenew. The guard is the TypeScript's
// module-level `autoRenewRunning` flag; a skipped tick is logged, not queued.
func RunMailAutoRenew(ctx context.Context) {
	if mailAutoRenewRunning {
		log.Printf("[MailAutoRenew] Previous run is still active, skipping")
		return
	}
	mailAutoRenewRunning = true
	defer func() { mailAutoRenewRunning = false }()

	scanNow := time.Now().UTC()
	deadline := scanNow.AddDate(0, 0, MailAutoRenewDaysAhead)

	candidates, err := mailAutoRenewScan(ctx, MailAutoRenewDaysAhead, scanNow)
	if err != nil {
		log.Printf("[MailAutoRenew] Failed to scan subscriptions: %s", MailAutoRenewSafeError(err))
		return
	}

	for _, candidate := range candidates {
		// Per-item isolation: one subscription's failure must not stop the scan.
		result, err := mailAutoRenewProcess(ctx, candidate, scanNow, deadline)
		if err != nil {
			message := MailAutoRenewSafeError(err)
			log.Printf("[MailAutoRenew] Subscription #%d failed: %s", candidate.ID, message)
			mailAutoRenewWriteLog(ctx, candidate.UserID, "mail_auto_renew_failed",
				fmt.Sprintf("Mail subscription #%d auto-renew failed: %s", candidate.ID, message), "failed")
			continue
		}
		if result.Status == MailAutoRenewStatusRenewed {
			mailAutoRenewWriteLog(ctx, candidate.UserID, "mail_auto_renew",
				fmt.Sprintf("Auto-renewed mail subscription #%d for %d month, amount=%.2f",
					candidate.ID, MailAutoRenewMonths, result.Amount), "success")
		}
	}
}

// mailSchedulerLogWithResult is mailSchedulerLog with an explicit result field.
func mailSchedulerLogWithResult(ctx context.Context, userID int32, action, content, result string) {
	db.CreateLog(ctx, int64Pointer(int64(userID)), "mail", action, content, result, db.CreateLogOptions{})
}

// startMailAutoRenewScheduler wires the 02:43 cron entry.
func startMailAutoRenewScheduler(group *schedulerGroup) {
	group.startLoop("MailAutoRenew", MailAutoRenewSchedulerNextRun, func(ctx context.Context) error {
		RunMailAutoRenew(ctx)
		return nil
	}, nil)
}

// MailAutoRenewIsRunning exposes the overlap flag to tests.
func MailAutoRenewIsRunning() bool { return mailAutoRenewRunning }

// mailAutoRenewSetRunningForTest drives the flag without a database.
func mailAutoRenewSetRunningForTest(value bool) { mailAutoRenewRunning = value }
