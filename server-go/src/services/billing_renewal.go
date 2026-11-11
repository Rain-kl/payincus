// Renewal arithmetic and the renewal transaction used by the billing scheduler.
//
// Why this exists next to routes/instance_billing.go: `performRenewal` is a
// db-level function in the Node backend (db/billing-operations.ts) and is called
// both by the HTTP renew endpoint and by services/billing-scheduler.ts. The Go
// route port is unexported and routes must not be imported by services, so the
// scheduler owns this copy. It is a line-by-line port of the same transaction — the
// two implementations must stay in sync; TestBillingSchedulerRenewalGuardParity
// pins the guard order.
package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// BillingSchedulerReplayWindowSeconds bounds the exact-replay detection window
// (same value as the route port).
const BillingSchedulerReplayWindowSeconds = 300

// BillingSchedulerRenewResult mirrors
// `{ newExpiresAt, amount, balanceLogId, discountAmount? }`.
type BillingSchedulerRenewResult struct {
	NewExpiresAt   time.Time
	Amount         float64
	BalanceLogID   int32
	DiscountAmount *float64
}

// BillingSchedulerMonthlyPrice mirrors calculateMonthlyPrice:
// `Number(instance.billingPrice) || 0` divided by `Math.max(billingCycle || 1, 1)`.
func BillingSchedulerMonthlyPrice(price float64, cycle *int32) float64 {
	normalized := int32(1)
	if cycle != nil && *cycle > 1 {
		normalized = *cycle
	}
	return price / float64(normalized)
}

// BillingSchedulerCalculateRenewBilling mirrors calculateRenewBilling.
func BillingSchedulerCalculateRenewBilling(instance *models.Instance, months int, now time.Time) (monthlyPrice, amount float64, newExpiresAt time.Time, err error) {
	if months < 1 || months > 24 {
		return 0, 0, time.Time{}, errors.New("续费月数必须在 1-24 之间")
	}
	price := 0.0
	if instance.BillingPrice != nil {
		price = instance.BillingPrice.InexactFloat64()
	}
	monthlyPrice = BillingSchedulerMonthlyPrice(price, instance.BillingCycle)
	amount = lib.JSFixed(monthlyPrice*float64(months), 2)

	baseDate := now
	if instance.ExpiresAt != nil && instance.ExpiresAt.After(now) {
		baseDate = *instance.ExpiresAt
	}
	return monthlyPrice, amount, lib.AddMonths(baseDate, months).UTC(), nil
}

// BillingSchedulerRenewalAlreadyApplied mirrors the exact-replay predicate: an
// existing renew record with the same period_end proves the debit already happened.
func BillingSchedulerRenewalAlreadyApplied(existing []models.InstanceBillingRecord, periodEnd time.Time) bool {
	for _, record := range existing {
		if record.PeriodEnd.Equal(periodEnd) {
			return true
		}
	}
	return false
}

// BillingSchedulerFixed2 mirrors `value.toFixed(2)` for a non-negative amount.
func BillingSchedulerFixed2(value float64) string {
	return fmt.Sprintf("%.2f", value)
}

// BillingSchedulerPerformRenewal mirrors performRenewal.
//
// Transaction shape (WAVE_D_SPEC §0.3/§0.4):
//  1. advisory lock 4106 (instance operation) — every operation on the instance
//     serialises on it;
//  2. exact-replay guard inside the lock, so a client retry cannot double debit;
//  3. balance change under lock 4110 with before/after snapshots;
//  4. optimistic version guard on the instance row;
//  5. billing record in the same transaction;
//  6. promo settlement joins the transaction.
//
// The AFF commission runs after commit because db.AffProcessCommission is
// pgx-typed and cannot join this GORM transaction. Paying it afterwards can lose a
// commission if the process dies between commit and commission, but it can never
// charge the user without the instance being renewed.
func BillingSchedulerPerformRenewal(ctx context.Context, userID int32, instance *models.Instance, months int) (BillingSchedulerRenewResult, error) {
	if instance.Status == models.InstanceStatusDeleted {
		return BillingSchedulerRenewResult{}, errors.New("实例已删除，无法续费")
	}
	if instance.PackagePlanID == nil {
		return BillingSchedulerRenewResult{}, errors.New("免费实例无需续费")
	}
	if months < 1 || months > 24 {
		return BillingSchedulerRenewResult{}, errors.New("续费月数必须在 1-24 之间")
	}
	if instance.Status == models.InstanceStatusSuspended &&
		(instance.SuspendReason == nil || *instance.SuspendReason != "expired") {
		return BillingSchedulerRenewResult{}, errors.New("实例已被手动封停，请联系宿主机所有者解封后再续费")
	}
	if db.DB == nil {
		return BillingSchedulerRenewResult{}, errors.New("database not initialized")
	}

	now := time.Now().UTC()
	monthlyPrice, originalAmount, newExpiresAt, err := BillingSchedulerCalculateRenewBilling(instance, months, now)
	if err != nil {
		return BillingSchedulerRenewResult{}, err
	}

	promoBinding, err := db.PromoGetInstanceBinding(ctx, instance.ID, nil)
	if err != nil {
		return BillingSchedulerRenewResult{}, err
	}
	isPromoActive := promoBinding != nil && promoBinding.PromoCode.Enabled &&
		(promoBinding.PromoCode.ExpiresAt == nil || promoBinding.PromoCode.ExpiresAt.After(now))

	var promoQuote *PromoRenewalSplitResult
	if isPromoActive && promoBinding != nil {
		quote := PromoQuoteRenewal(monthlyPrice, months, promoBinding)
		promoQuote = &quote
	}

	affBinding, err := db.AffGetInstanceBinding(ctx, instance.ID, nil)
	if err != nil {
		return BillingSchedulerRenewResult{}, err
	}

	_, vipBenefit, err := VipGetUserContinuousVipBenefit(ctx, userID, nil)
	if err != nil {
		return BillingSchedulerRenewResult{}, err
	}
	vipPercent := vipBenefit.OrderDiscountPercent

	var affDiscountRate *float64
	if affBinding != nil && affBinding.AffCode.Enabled {
		rate := affBinding.AffCode.DiscountRate.InexactFloat64()
		affDiscountRate = &rate
	}

	renewalPrice := VipArbitratePrice(VipPriceArbitrationInput{
		BasePrice:          originalAmount,
		AffDiscountRate:    affDiscountRate,
		VipDiscountPercent: &vipPercent,
	})
	finalAmount := renewalPrice.FinalPrice
	discountAmount := renewalPrice.DiscountAmount
	pricingSource := renewalPrice.Source
	isPromoApplied := false
	if promoQuote != nil && promoQuote.FinalAmount <= finalAmount && promoQuote.DiscountAmount > 0 {
		finalAmount = promoQuote.FinalAmount
		discountAmount = promoQuote.DiscountAmount
		pricingSource = VipPriceSource("promo")
		isPromoApplied = true
	}

	periodStart := now
	if instance.ExpiresAt != nil {
		periodStart = instance.ExpiresAt.UTC()
	}

	remark := fmt.Sprintf("续费（%d个月）：%s", months, instance.Name)
	recordRemark := fmt.Sprintf("续费 %d 个月", months)
	if discountAmount > 0 {
		remark = fmt.Sprintf("续费（%d个月）：%s，优惠码折扣 -¥%s", months, instance.Name, BillingSchedulerFixed2(discountAmount))
		recordRemark = fmt.Sprintf("续费 %d 个月，优惠码折扣 -¥%s", months, BillingSchedulerFixed2(discountAmount))
	}

	var balanceLogID int32
	alreadyApplied := false
	version := instance.Version
	status := instance.Status
	suspendReason := instance.SuspendReason

	txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := db.TryInstanceBillingAdvisoryLock(ctx, tx, db.InstanceOperationLockNamespace, instance.ID)
		if err != nil {
			return err
		}
		if !locked {
			return errors.New("实例正在处理中，请稍后重试")
		}

		var replays []models.InstanceBillingRecord
		if err := tx.WithContext(ctx).Raw(
			`SELECT id, amount, period_end
			   FROM instance_billing_records
			  WHERE instance_id = ? AND user_id = ? AND type = 'renew'
			    AND period_end = ? AND created_at >= ?
			  ORDER BY id DESC LIMIT 1`,
			instance.ID, userID, newExpiresAt, now.Add(-time.Duration(BillingSchedulerReplayWindowSeconds)*time.Second),
		).Scan(&replays).Error; err != nil {
			return err
		}
		if len(replays) > 0 && BillingSchedulerRenewalAlreadyApplied(replays, newExpiresAt) {
			alreadyApplied = true
			finalAmount = replays[0].Amount.InexactFloat64()
			// The TypeScript leaves balanceLogId undefined on this path; the route port
			// returns 0, so the scheduler does the same (nobody consumes it here).
			balanceLogID = 0
			return nil
		}

		balanceDelta := models.Decimal{Decimal: decimal.NewFromFloat(-finalAmount).Round(2)}
		balanceResult, err := db.InstanceBillingApplyBalanceTx(ctx, tx, db.InstanceBillingBalanceChange{
			UserID:     userID,
			InstanceID: instance.ID,
			Delta:      balanceDelta,
			LogType:    models.BalanceLogTypeConsume,
			Remark:     remark,
		})
		if err != nil {
			return err
		}
		balanceLogID = balanceResult.BalanceLogID

		updates := map[string]any{
			"expires_at":          newExpiresAt,
			"auto_renew_attempts": int32(0),
			"version":             gorm.Expr("version + 1"),
		}
		if status == models.InstanceStatusSuspended && suspendReason != nil && *suspendReason == "expired" {
			updates["status"] = string(models.InstanceStatusStopped)
			updates["suspended_at"] = nil
			updates["suspended_by"] = nil
			updates["suspend_reason"] = nil
		}
		result := tx.WithContext(ctx).Model(&models.Instance{}).
			Where("id = ? AND status <> ?", instance.ID, string(models.InstanceStatusDeleted)).
			Where("version = ?", version).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errors.New("实例状态已变更，请重试")
		}

		record := models.InstanceBillingRecord{
			InstanceID:   instance.ID,
			UserID:       userID,
			Type:         models.BillingRecordTypeRenew,
			Amount:       models.Decimal{Decimal: decimal.NewFromFloat(finalAmount).Round(2)},
			Months:       int32(months),
			PeriodStart:  periodStart,
			PeriodEnd:    newExpiresAt,
			BalanceLogID: &balanceLogID,
			Remark:       &recordRemark,
		}
		if err := tx.WithContext(ctx).Create(&record).Error; err != nil {
			return err
		}

		if isPromoApplied && promoBinding != nil {
			renewResult, err := PromoSettleOnRenew(ctx, tx, PromoSettleRenewParams{
				InstanceID:   instance.ID,
				UserID:       userID,
				MonthlyPrice: monthlyPrice,
				Months:       months,
				Binding:      promoBinding,
			})
			if err != nil {
				return err
			}
			if renewResult.Unbound {
				log.Printf("[Billing] promo binding exhausted for instance %d", instance.ID)
			}
		}
		return nil
	})
	if txErr != nil {
		return BillingSchedulerRenewResult{}, txErr
	}

	if !alreadyApplied && affBinding != nil && affBinding.AffCode.Enabled && pricingSource == VipPriceSourceAff {
		if err := db.AffProcessCommission(ctx, affBinding.AffCode.ID, instance.ID, originalAmount, models.AffLogTypeRenew, nil); err != nil {
			log.Printf("[Billing] AFF commission failed for instance %d: %v", instance.ID, err)
		}
	}

	var discountPtr *float64
	if discountAmount > 0 {
		value := discountAmount
		discountPtr = &value
	}
	return BillingSchedulerRenewResult{
		NewExpiresAt:   newExpiresAt,
		Amount:         finalAmount,
		BalanceLogID:   balanceLogID,
		DiscountAmount: discountPtr,
	}, nil
}
