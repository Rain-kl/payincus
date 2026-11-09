// Port of server/src/services/promo-engine.ts.
//
// PromoCodeEngine is the single entry point the instance/billing flows use to
// validate a code, quote a discount and settle a redemption. In Go the static
// class methods become package functions; every export is Promo-prefixed because
// the services package is shared by parallel module ports.
//
// Invariants that must not drift from the TypeScript:
//
//   - validate() checks in this order: empty -> not found -> disabled ->
//     not started -> expired -> total quota -> per-user quota -> self-referral ->
//     existing binding -> scope eligibility. The first failure wins.
//   - Money crosses into the `number` domain only where the TypeScript does;
//     `toFixed(2)` is reproduced by db.PromoMoneyFixed2.
//   - `??` (nullish) and `||` (truthy) fallbacks are different; the helpers below
//     keep them apart.
//
// Concurrency: settleOnCreate takes the promo_codes row lock through
// db.PromoIncrementUsageWithLock inside the caller's transaction, then re-checks
// the per-user limit under that lock — the same ordering as Node.
package services

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ErrPromoUserLimitExceeded mirrors `throw new Error('PROMO_USER_LIMIT_EXCEEDED')`
// from settleOnCreate (the second, in-lock per-user check).
var ErrPromoUserLimitExceeded = errors.New("PROMO_USER_LIMIT_EXCEEDED")

// PromoValidateParams mirrors PromoValidateParams. A nil TargetInstanceID means
// `targetInstanceId` was not supplied (the TypeScript checks it with a truthiness
// test, so 0 behaves like absent).
type PromoValidateParams struct {
	Code             string
	PackageID        int32
	PackagePlanID    int32
	UserID           int32
	TargetInstanceID *int32
	Tx               *gorm.DB
}

// PromoValidationResult mirrors PromoValidationResult. The discount/duration
// fields are pointers because they are absent (`undefined`) on a failed result.
type PromoValidationResult struct {
	Valid          bool                      `json:"valid"`
	Error          string                    `json:"error,omitempty"`
	ErrorCode      string                    `json:"errorCode,omitempty"`
	PromoCode      *db.PromoCodeWithDetails  `json:"promoCode,omitempty"`
	DiscountType   *models.PromoDiscountType `json:"discountType,omitempty"`
	DiscountValue  float64                   `json:"discountValue,omitempty"`
	DurationType   *models.PromoDurationType `json:"durationType,omitempty"`
	DurationCycles *int32                    `json:"durationCycles,omitempty"`
	Description    *string                   `json:"description,omitempty"`
}

// PromoSettleCreateQuote mirrors the `quote` sub-object of PromoSettleCreateParams.
// OriginalPrice is optional (`originalPrice?: number`), so it is a pointer.
type PromoSettleCreateQuote struct {
	DiscountAmount float64
	FinalPrice     float64
	OriginalPrice  *float64
}

// PromoSettleCreateParams mirrors PromoSettleCreateParams.
type PromoSettleCreateParams struct {
	PromoCodeID      int32
	InstanceID       int32
	UserID           int32
	OriginalPrice    *float64
	DiscountAmount   *float64
	FinalPrice       *float64
	DurationType     models.PromoDurationType
	DurationCycles   *int32
	CommissionAmount *float64
	CommissionRate   *float64
	Quote            *PromoSettleCreateQuote
}

// PromoSettleCreateResult mirrors PromoSettleCreateResult.
type PromoSettleCreateResult struct {
	PromoCode     models.PromoCode
	RedemptionLog models.PromoRedemptionLog
	Binding       *models.InstancePromoBinding
}

// PromoSettleRenewParams mirrors PromoSettleRenewParams.
type PromoSettleRenewParams struct {
	InstanceID       int32
	UserID           int32
	MonthlyPrice     float64
	Months           int
	Binding          *db.PromoInstanceBindingWithCode
	CommissionAmount *float64
}

// PromoSettleRenewResult mirrors PromoSettleRenewResult.
type PromoSettleRenewResult struct {
	Quote         PromoRenewalSplitResult
	Unbound       bool
	RedemptionLog models.PromoRedemptionLog
}

// PromoRefundEligibilityResult mirrors PromoRefundEligibilityResult.
type PromoRefundEligibilityResult struct {
	IsRefundable bool   `json:"isRefundable"`
	Reason       string `json:"reason,omitempty"`
}

// PromoQuotePromo mirrors the inline `promo: { discountType; discountValue }`
// argument of PromoCodeEngine.calculateCreationQuote. In Node the value may also
// be a raw number; callers with a number should use
// models.Decimal{Decimal: decimal.NewFromFloat(n)} — every production caller
// passes the Prisma Decimal straight through.
type PromoQuotePromo struct {
	DiscountType  models.PromoDiscountType
	DiscountValue models.Decimal
}

// ==================== JavaScript helpers ====================

// promoJSTRuneIsSpace mirrors the WhiteSpace + LineTerminator set that
// String.prototype.trim removes (it notably omits U+0085 and includes U+FEFF).
func promoJSTRuneIsSpace(char rune) bool {
	switch char {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0x1680,
		0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return char >= 0x2000 && char <= 0x200A
}

// promoJSTrim mirrors String.prototype.trim.
func promoJSTrim(value string) string {
	return strings.TrimFunc(value, promoJSTRuneIsSpace)
}

// promoDecimalFromFixed2 mirrors `new Prisma.Decimal(value.toFixed(2))`.
func promoDecimalFromFixed2(value float64) models.Decimal {
	parsed, err := decimal.NewFromString(strconv.FormatFloat(db.PromoMoneyFixed2(value), 'f', 2, 64))
	if err != nil {
		return models.Decimal{Decimal: decimal.Zero}
	}
	return models.Decimal{Decimal: parsed}
}

// promoStringPtr returns a pointer to value, for the optional `actionType`
// argument of db.PromoCountUserRedemptions.
func promoStringPtr(value string) *string { return &value }

// ==================== validate ====================

// PromoValidate mirrors PromoCodeEngine.validate.
//
// The returned error is non-nil only for an infrastructure failure; a rejected
// code is a normal (Valid=false) result, exactly like the TypeScript.
func PromoValidate(ctx context.Context, params PromoValidateParams) (PromoValidationResult, error) {
	code := promoJSTrim(params.Code)
	if code == "" {
		return PromoValidationResult{
			Valid:     false,
			Error:     "优惠码不能为空",
			ErrorCode: "PROMO_NOT_FOUND",
		}, nil
	}

	promo, err := db.PromoGetCodeByCode(ctx, code, params.Tx)
	if err != nil {
		return PromoValidationResult{}, err
	}
	// Codes are stored uppercase in practice; the fallback only runs when the
	// input is not already uppercase (`code.toUpperCase() !== code`).
	if promo == nil {
		if upper := strings.ToUpper(code); upper != code {
			promo, err = db.PromoGetCodeByCode(ctx, upper, params.Tx)
			if err != nil {
				return PromoValidationResult{}, err
			}
		}
	}
	if promo == nil {
		return PromoValidationResult{
			Valid:     false,
			Error:     "优惠码不存在",
			ErrorCode: "PROMO_NOT_FOUND",
		}, nil
	}

	if !promo.Enabled {
		return PromoValidationResult{
			Valid:     false,
			Error:     "优惠码已被禁用",
			ErrorCode: "PROMO_DISABLED",
		}, nil
	}

	now := time.Now()
	if promo.StartsAt != nil && now.Before(*promo.StartsAt) {
		return PromoValidationResult{
			Valid:     false,
			Error:     "优惠码活动尚未开始",
			ErrorCode: "PROMO_NOT_STARTED",
		}, nil
	}

	// Note the strict comparison: the engine treats expiresAt == now as still
	// valid, while the renew-preview route reports EXPIRED for `<=` (both mirror
	// their own TypeScript call sites).
	if promo.ExpiresAt != nil && now.After(*promo.ExpiresAt) {
		return PromoValidationResult{
			Valid:     false,
			Error:     "优惠码已过期",
			ErrorCode: "PROMO_EXPIRED",
		}, nil
	}

	if promo.MaxTotalUses != nil && promo.UsedTotalCount >= *promo.MaxTotalUses {
		return PromoValidationResult{
			Valid:     false,
			Error:     "优惠码使用次数已达上限",
			ErrorCode: "PROMO_QUOTA_EXCEEDED",
		}, nil
	}

	if promo.MaxUsesPerUser != nil {
		userUses, err := db.PromoCountUserRedemptions(
			ctx, promo.ID, params.UserID, promoStringPtr(db.PromoRedemptionActionCreate), params.Tx)
		if err != nil {
			return PromoValidationResult{}, err
		}
		if userUses >= int64(*promo.MaxUsesPerUser) {
			return PromoValidationResult{
				Valid:     false,
				Error:     "您已达到该优惠码的最大使用次数",
				ErrorCode: "PROMO_USER_LIMIT_EXCEEDED",
			}, nil
		}
	}

	// Self-referral guard: an AFF code may not be redeemed by its owner.
	if promo.Type == models.PromoCodeTypeAFFUSER && promo.UserID != nil && *promo.UserID == params.UserID {
		return PromoValidationResult{
			Valid:     false,
			Error:     "不能使用自己的推广优惠码",
			ErrorCode: "CANNOT_USE_OWN_AFF_CODE",
		}, nil
	}

	// An instance may hold only one active promo binding.
	if params.TargetInstanceID != nil && *params.TargetInstanceID != 0 {
		binding, err := db.PromoGetInstanceBinding(ctx, *params.TargetInstanceID, params.Tx)
		if err != nil {
			return PromoValidationResult{}, err
		}
		if binding != nil {
			return PromoValidationResult{
				Valid:     false,
				Error:     "该实例已绑定其他优惠码",
				ErrorCode: "PROMO_BINDING_ACTIVE",
			}, nil
		}
	}

	eligible := PromoIsProductEligibleForPromo(PromoScopesOf(promo), PromoTarget{
		PackageID:     params.PackageID,
		PackagePlanID: params.PackagePlanID,
	})
	if !eligible {
		return PromoValidationResult{
			Valid:     false,
			Error:     "该优惠码不适用于当前套餐或方案",
			ErrorCode: "PROMO_SCOPE_MISMATCH",
		}, nil
	}

	discountType := promo.DiscountType
	durationType := promo.DurationType
	return PromoValidationResult{
		Valid:          true,
		PromoCode:      promo,
		DiscountType:   &discountType,
		DiscountValue:  promo.DiscountValue.InexactFloat64(),
		DurationType:   &durationType,
		DurationCycles: promo.DurationCycles,
		Description:    promo.Name,
	}, nil
}

// ==================== quotes ====================

// PromoQuoteCreation mirrors PromoCodeEngine.calculateCreationQuote.
func PromoQuoteCreation(planPrice float64, promo PromoQuotePromo) PromoCreationQuoteResult {
	return PromoCalculateCreationQuote(PromoCreationQuoteParams{
		OriginalPrice: planPrice,
		DiscountType:  promo.DiscountType,
		DiscountValue: promo.DiscountValue.InexactFloat64(),
	})
}

// PromoQuoteRenewal mirrors PromoCodeEngine.calculateRenewalQuote.
func PromoQuoteRenewal(monthlyPrice float64, months int, binding *db.PromoInstanceBindingWithCode) PromoRenewalSplitResult {
	return PromoCalculateRenewalSplit(PromoRenewalSplitParams{
		MonthlyPrice:    monthlyPrice,
		Months:          months,
		DiscountRate:    binding.PromoCode.DiscountValue.InexactFloat64(),
		RemainingCycles: binding.RemainingCycles,
	})
}

// ==================== settlement ====================

// promoCreateAmounts is the resolved form of the `??` chain at the top of
// settleOnCreate.
type promoCreateAmounts struct {
	discountAmount   float64
	finalPrice       float64
	originalPrice    float64
	commissionAmount float64
}

// promoResolveCreateAmounts mirrors:
//
//	discountAmount   = quote?.discountAmount ?? discountAmount ?? 0
//	finalPrice       = quote?.finalPrice ?? finalPrice ?? 0
//	originalPrice    = quote?.originalPrice ?? originalPrice ?? Number((finalPrice + discountAmount).toFixed(2))
//	commissionAmount = commissionAmount ?? (commissionRate ? Number((originalPrice * commissionRate).toFixed(2)) : 0)
//
// `??` only falls through on null/undefined (so a provided 0 wins), while the
// commission rate uses JavaScript truthiness (0 and NaN are "not provided").
func promoResolveCreateAmounts(params PromoSettleCreateParams) promoCreateAmounts {
	discountAmount := 0.0
	if params.Quote != nil {
		discountAmount = params.Quote.DiscountAmount
	} else if params.DiscountAmount != nil {
		discountAmount = *params.DiscountAmount
	}

	finalPrice := 0.0
	if params.Quote != nil {
		finalPrice = params.Quote.FinalPrice
	} else if params.FinalPrice != nil {
		finalPrice = *params.FinalPrice
	}

	originalPrice := 0.0
	switch {
	case params.Quote != nil && params.Quote.OriginalPrice != nil:
		originalPrice = *params.Quote.OriginalPrice
	case params.OriginalPrice != nil:
		originalPrice = *params.OriginalPrice
	default:
		originalPrice = db.PromoMoneyFixed2(finalPrice + discountAmount)
	}

	commissionAmount := 0.0
	switch {
	case params.CommissionAmount != nil:
		commissionAmount = *params.CommissionAmount
	case params.CommissionRate != nil && *params.CommissionRate != 0 && !math.IsNaN(*params.CommissionRate):
		commissionAmount = db.PromoMoneyFixed2(originalPrice * *params.CommissionRate)
	}

	return promoCreateAmounts{
		discountAmount:   discountAmount,
		finalPrice:       finalPrice,
		originalPrice:    originalPrice,
		commissionAmount: commissionAmount,
	}
}

// PromoSettleOnCreate mirrors PromoCodeEngine.settleOnCreate.
//
// Order matters: the promo row is locked and incremented first, then the per-user
// limit is re-checked under that lock, then the ledger row is written, then the
// binding decision is made. Any failure returns the error and the caller must
// abort the transaction.
func PromoSettleOnCreate(ctx context.Context, tx *gorm.DB, params PromoSettleCreateParams) (PromoSettleCreateResult, error) {
	if tx == nil {
		return PromoSettleCreateResult{}, errors.New("PromoSettleOnCreate requires a transaction")
	}

	amounts := promoResolveCreateAmounts(params)

	// 1. Atomic increment of the total use count and total discount, under the
	// promo_codes row lock.
	updatedPromo, err := db.PromoIncrementUsageWithLock(ctx, tx, params.PromoCodeID, amounts.discountAmount)
	if err != nil {
		return PromoSettleCreateResult{}, err
	}

	// Second per-user check, inside the lock, so concurrent redemptions cannot
	// break the per-user cap.
	if updatedPromo.MaxUsesPerUser != nil {
		userUses, err := db.PromoCountUserRedemptions(
			ctx, updatedPromo.ID, params.UserID, promoStringPtr(db.PromoRedemptionActionCreate), tx)
		if err != nil {
			return PromoSettleCreateResult{}, err
		}
		if userUses >= int64(*updatedPromo.MaxUsesPerUser) {
			return PromoSettleCreateResult{}, ErrPromoUserLimitExceeded
		}
	}

	// 2. Ledger row.
	redemptionLog, err := db.PromoCreateRedemptionLog(ctx, tx, db.PromoRedemptionLogData{
		PromoCodeID:      params.PromoCodeID,
		InstanceID:       params.InstanceID,
		UserID:           params.UserID,
		ActionType:       db.PromoRedemptionActionCreate,
		CycleIndex:       1,
		OriginalPrice:    amounts.originalPrice,
		DiscountAmount:   amounts.discountAmount,
		FinalPrice:       amounts.finalPrice,
		CommissionAmount: &amounts.commissionAmount,
	})
	if err != nil {
		return PromoSettleCreateResult{}, err
	}

	// 3. Binding decision. ONCE codes and zero-discount codes never bind.
	var binding *models.InstancePromoBinding

	if amounts.discountAmount > 0 && params.DurationType != models.PromoDurationTypeONCE {
		switch params.DurationType {
		case models.PromoDurationTypeREPEATING:
			totalCycles := int32(1)
			if params.DurationCycles != nil && *params.DurationCycles != 0 {
				totalCycles = *params.DurationCycles
			}
			remainingCycles := totalCycles - 1
			if remainingCycles < 0 {
				remainingCycles = 0
			}
			if remainingCycles > 0 {
				bound, err := db.PromoBindToInstance(ctx, tx, db.PromoBindParams{
					InstanceID:      params.InstanceID,
					PromoCodeID:     params.PromoCodeID,
					DurationType:    params.DurationType,
					TotalCycles:     &totalCycles,
					UsedCycles:      1,
					RemainingCycles: &remainingCycles,
				})
				if err != nil {
					return PromoSettleCreateResult{}, err
				}
				binding = &bound
			}
		case models.PromoDurationTypeFOREVER:
			bound, err := db.PromoBindToInstance(ctx, tx, db.PromoBindParams{
				InstanceID:      params.InstanceID,
				PromoCodeID:     params.PromoCodeID,
				DurationType:    params.DurationType,
				TotalCycles:     nil,
				UsedCycles:      1,
				RemainingCycles: nil,
			})
			if err != nil {
				return PromoSettleCreateResult{}, err
			}
			binding = &bound
		}
	}

	return PromoSettleCreateResult{
		PromoCode:     updatedPromo,
		RedemptionLog: redemptionLog,
		Binding:       binding,
	}, nil
}

// PromoSettleOnRenew mirrors PromoCodeEngine.settleOnRenew.
func PromoSettleOnRenew(ctx context.Context, tx *gorm.DB, params PromoSettleRenewParams) (PromoSettleRenewResult, error) {
	if tx == nil {
		return PromoSettleRenewResult{}, errors.New("PromoSettleOnRenew requires a transaction")
	}

	binding := params.Binding
	if binding == nil {
		loaded, err := db.PromoGetInstanceBinding(ctx, params.InstanceID, tx)
		if err != nil {
			return PromoSettleRenewResult{}, err
		}
		binding = loaded
	}
	if binding == nil {
		return PromoSettleRenewResult{}, db.ErrPromoBindingMissing
	}

	quote := PromoQuoteRenewal(params.MonthlyPrice, params.Months, binding)

	// `promoCode.update({ totalDiscountAmount: { increment: ... } })`. Prisma also
	// bumps the @updatedAt column, so updated_at is set explicitly here.
	if quote.DiscountAmount > 0 {
		result := tx.WithContext(ctx).Exec(
			`UPDATE promo_codes
			    SET total_discount_amount = total_discount_amount + ?, updated_at = ?
			  WHERE id = ?`,
			promoDecimalFromFixed2(quote.DiscountAmount), time.Now(), binding.PromoCodeID)
		if result.Error != nil {
			return PromoSettleRenewResult{}, result.Error
		}
		if result.RowsAffected == 0 {
			// Prisma's update throws P2025 when the row disappeared mid-transaction.
			return PromoSettleRenewResult{}, db.ErrPromoNotFound
		}
	}

	originalPrice := db.PromoMoneyFixed2(params.MonthlyPrice * float64(params.Months))
	months := int32(params.Months)
	redemptionLog, err := db.PromoCreateRedemptionLog(ctx, tx, db.PromoRedemptionLogData{
		PromoCodeID:      binding.PromoCodeID,
		InstanceID:       params.InstanceID,
		UserID:           params.UserID,
		ActionType:       db.PromoRedemptionActionRenew,
		CycleIndex:       binding.UsedCycles + 1,
		Months:           &months,
		OriginalPrice:    originalPrice,
		DiscountAmount:   quote.DiscountAmount,
		FinalPrice:       quote.FinalAmount,
		CommissionAmount: params.CommissionAmount,
	})
	if err != nil {
		return PromoSettleRenewResult{}, err
	}

	if quote.WillUnbind {
		if _, err := db.PromoUnbindFromInstance(ctx, tx, params.InstanceID); err != nil {
			return PromoSettleRenewResult{}, err
		}
		return PromoSettleRenewResult{Quote: quote, Unbound: true, RedemptionLog: redemptionLog}, nil
	}

	if _, err := db.PromoUpdateBindingCycles(ctx, tx, params.InstanceID, db.PromoBindingCycles{
		ConsumedCycles:     int32(quote.ConsumedCycles),
		NewRemainingCycles: quote.NewRemainingCycles,
	}); err != nil {
		return PromoSettleRenewResult{}, err
	}
	return PromoSettleRenewResult{Quote: quote, Unbound: false, RedemptionLog: redemptionLog}, nil
}

// PromoGetRefundEligibility mirrors PromoCodeEngine.getPromoRefundEligibility.
//
// The TypeScript probes `(client as any).billingRecord?.count` and then
// `instanceBillingRecord?.count`. Prisma only has the latter delegate
// (BillingRecordType lives on InstanceBillingRecord), so the second branch is the
// live one: it counts renew rows without the `status: 'paid'` filter the dead
// first branch carries.
func PromoGetRefundEligibility(ctx context.Context, instanceID int32, tx *gorm.DB) (PromoRefundEligibilityResult, error) {
	binding, err := db.PromoGetInstanceBinding(ctx, instanceID, tx)
	if err != nil {
		return PromoRefundEligibilityResult{}, err
	}
	if binding != nil {
		return PromoRefundEligibilityResult{IsRefundable: false, Reason: "PROMO_BINDING_ACTIVE"}, nil
	}

	logs, err := db.PromoGetRedemptionLogsByInstance(ctx, instanceID, tx)
	if err != nil {
		return PromoRefundEligibilityResult{}, err
	}
	hasCreatePromo := false
	for _, log := range logs {
		if log.ActionType == db.PromoRedemptionActionCreate {
			hasCreatePromo = true
			break
		}
	}

	if hasCreatePromo {
		client := db.DB
		if tx != nil {
			client = tx
		}
		var renewCount int64
		if err := client.WithContext(ctx).Model(&models.InstanceBillingRecord{}).
			Where("instance_id = ?", instanceID).
			Where("type = ?", models.BillingRecordTypeRenew).
			Count(&renewCount).Error; err != nil {
			return PromoRefundEligibilityResult{}, err
		}
		if renewCount == 0 {
			return PromoRefundEligibilityResult{IsRefundable: false, Reason: "PROMO_FIRST_CYCLE_LOCKED"}, nil
		}
	}

	return PromoRefundEligibilityResult{IsRefundable: true}, nil
}
