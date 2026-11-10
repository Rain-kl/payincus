// Port of server/src/routes/instance-billing.ts.
//
// app.ts registers this module as `fastify.register(instanceBillingRoutes, { prefix:
// '/api/instances' })`, so every path below is relative to /api/instances. All ten
// routes authenticate with the Web (JWT) surface; four of them use
// `authenticateUser` (regular users only) and two use plain `authenticate` (the
// instance owner OR the host owner may read).
//
// Money invariants (docs/WAVE_D_SPEC.md §0.3/§0.4):
//
//   - renewal and plan change run in ONE transaction: balance mutation +
//     balance_logs snapshot + instance_billing_records row + instance expiry plan
//     change, so a failure cannot charge without extending (or extend without
//     charging);
//   - the transaction takes the instance-operation advisory lock (4106) and the
//     user-balance advisory lock (4110) before touching either row;
//   - balance_logs stores balance_before / balance_after snapshots, never deltas;
//   - amounts are computed with lib.JSFixed / lib.CalculateDiscountAmount /
//     lib.CalculatePlanChangeDetails, and month arithmetic uses lib.AddMonths
//     (31-day months — 12 months is 372 days, NOT a calendar year);
//   - promo and VIP/AFF pricing use the already-ported services/promo_engine.go,
//     services/promo_calculator.go and services/vip_benefits.go instead of a local
//     re-implementation.
//
// Idempotency: the TypeScript relies on the optimistic `version` column
// (`updateMany where version = instance.version`). That only stops two requests
// that both read the same version. This port keeps the version guard AND rejects
// an exact replay: a renew that would produce the same period_end as a renew
// billing record written in the last five minutes is treated as already applied,
// so a double-submitted request cannot debit twice. Sequential renewals are
// unaffected because their period_end always moves forward.
package routes

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

const (
	// instanceBillingBatchHiddenReason mirrors BATCH_HIDDEN_REASON: a missing
	// instance and another user's instance must be indistinguishable.
	instanceBillingBatchHiddenReason = "实例不存在或无权操作"

	// instanceBillingLogModule mirrors the literal module passed to createLog.
	instanceBillingLogModule = "instance"

	// instanceBillingReplayWindowSeconds bounds the exact-replay detection window.
	instanceBillingReplayWindowSeconds = 300
)

// instanceBillingRecordTypes mirrors BILLING_RECORD_TYPES.
var instanceBillingRecordTypes = map[string]models.BillingRecordType{
	string(models.BillingRecordTypeNewPurchase): models.BillingRecordTypeNewPurchase,
	string(models.BillingRecordTypeRenew):       models.BillingRecordTypeRenew,
	string(models.BillingRecordTypeUpgrade):     models.BillingRecordTypeUpgrade,
	string(models.BillingRecordTypeDowngrade):   models.BillingRecordTypeDowngrade,
	string(models.BillingRecordTypeRefund):      models.BillingRecordTypeRefund,
	string(models.BillingRecordTypeTransferFee): models.BillingRecordTypeTransferFee,
}

// instanceBillingRenewMonths mirrors `const options = [1, 3, 6, 12]`.
var instanceBillingRenewMonths = []int{1, 3, 6, 12}

// ==================== wire types ====================

type instanceBillingPreviewOption struct {
	Months          int32   `json:"months"`
	Price           float64 `json:"price"`
	DiscountedPrice float64 `json:"discountedPrice"`
	ExpiresAt       string  `json:"expiresAt"`
}

type instanceBillingBatchPreviewItem struct {
	ID              int32                          `json:"id"`
	Name            string                         `json:"name"`
	CanRenew        bool                           `json:"canRenew"`
	AutoRenew       bool                           `json:"autoRenew"`
	Reason          *string                        `json:"reason,omitempty"`
	DaysUntilExpire *int64                         `json:"daysUntilExpire"`
	Options         []instanceBillingPreviewOption `json:"options"`
}

type instanceBillingBatchRenewResult struct {
	ID           int32    `json:"id"`
	Name         string   `json:"name"`
	Success      bool     `json:"success"`
	Skipped      *bool    `json:"skipped,omitempty"`
	Reason       *string  `json:"reason,omitempty"`
	Amount       *float64 `json:"amount,omitempty"`
	NewExpiresAt *string  `json:"newExpiresAt,omitempty"`
}

type instanceBillingAffDiscountView struct {
	DiscountRate    float64 `json:"discountRate"`
	DiscountPercent int64   `json:"discountPercent"`
}

type instanceBillingBillingView struct {
	InstanceID      int32                           `json:"instanceId"`
	InstanceName    string                          `json:"instanceName"`
	PlanID          *int32                          `json:"planId"`
	PlanName        *string                         `json:"planName"`
	Price           *float64                        `json:"price"`
	BillingCycle    *int32                          `json:"billingCycle"`
	ExpiresAt       *string                         `json:"expiresAt"`
	AutoRenew       bool                            `json:"autoRenew"`
	RenewPreview    []instanceBillingPreviewOption  `json:"renewPreview"`
	AffDiscount     *instanceBillingAffDiscountView `json:"affDiscount"`
	DaysUntilExpire *int64                          `json:"daysUntilExpire"`
}

type instanceBillingPlanView struct {
	ID           int32   `json:"id"`
	Name         string  `json:"name"`
	Price        float64 `json:"price"`
	BillingCycle int32   `json:"billingCycle"`
}

type instanceBillingNewPlanView struct {
	ID           int32   `json:"id"`
	Name         string  `json:"name"`
	Price        float64 `json:"price"`
	BillingCycle int32   `json:"billingCycle"`
	IsActive     bool    `json:"isActive"`
	IsSoldOut    bool    `json:"isSoldOut"`
}

type instanceBillingNewConfig struct {
	CPU    int32 `json:"cpu"`
	Memory int32 `json:"memory"`
	Disk   int32 `json:"disk"`
}

type instanceBillingChangePlanPreviewView struct {
	OldPlan            instanceBillingPlanView          `json:"oldPlan"`
	NewPlan            instanceBillingNewPlanView       `json:"newPlan"`
	RemainingDays      float64                          `json:"remainingDays"`
	OldDailyPrice      float64                          `json:"oldDailyPrice"`
	NewDailyPrice      float64                          `json:"newDailyPrice"`
	RemainingValue     float64                          `json:"remainingValue"`
	NewPlanCost        float64                          `json:"newPlanCost"`
	DiscountRate       float64                          `json:"discountRate"`
	DiscountAmount     float64                          `json:"discountAmount"`
	PriceDiff          float64                          `json:"priceDiff"`
	IsUpgrade          bool                             `json:"isUpgrade"`
	NewExpiresAt       string                           `json:"newExpiresAt"`
	NewConfig          instanceBillingNewConfig         `json:"newConfig"`
	ResourceWarnings   []string                         `json:"resourceWarnings"`
	ResourceCapacity   db.HostsPlanUpgradeCapacityCheck `json:"resourceCapacity"`
	CanChange          bool                             `json:"canChange"`
	CannotChangeReason *string                          `json:"cannotChangeReason,omitempty"`
}

// ==================== helpers ====================

// instanceBillingISOTime mirrors Date.prototype.toISOString().
func instanceBillingISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func instanceBillingDecimalsToFloat(value decimal.Decimal) float64 {
	return value.InexactFloat64()
}

// instanceBillingViewerAccess mirrors the permission block shared by
// GET /:id/billing and GET /:id/billing/records: an instance owner or the host
// owner may read. There is deliberately no admin bypass — the TypeScript has
// none, so an admin who owns neither side gets 403.
func instanceBillingViewerAccess(instanceOwnerID, hostOwnerID, userID int32) (isOwner, isHostOwner, allowed bool) {
	isOwner = instanceOwnerID == userID
	isHostOwner = hostOwnerID == userID
	return isOwner, isHostOwner, isOwner || isHostOwner
}

// instanceBillingRenewalAlreadyApplied is the exact-replay predicate: an existing
// `renew` billing record for the same period_end proves this renewal was already
// applied. It runs inside the transaction (under the instance-operation lock) so a
// double-submitted request cannot debit the balance twice.
func instanceBillingRenewalAlreadyApplied(existing []models.InstanceBillingRecord, periodEnd time.Time) bool {
	for _, record := range existing {
		if record.PeriodEnd.Equal(periodEnd) {
			return true
		}
	}
	return false
}

// instanceBillingParseRouteID mirrors parsePositiveRouteId, including the
// JavaScript safe-integer bound. Prisma's Int column cannot hold a value above
// MaxInt32: the TypeScript throws there and surfaces as a 500.
func instanceBillingParseRouteID(c *gin.Context, value string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return 0, false
	}
	if parsed > math.MaxInt32 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return 0, false
	}
	return int32(parsed), true
}

// instanceBillingNormalizeRecordType mirrors normalizeBillingRecordType.
func instanceBillingNormalizeRecordType(value string) models.BillingRecordType {
	if value == "" {
		return ""
	}
	return instanceBillingRecordTypes[value]
}

func instanceBillingInt32Pointer(value int32) *int32 { return &value }

func instanceBillingBoolPointer(value bool) *bool { return &value }

func instanceBillingStringPointer(value string) *string { return &value }

// instanceBillingPriceYuan mirrors serializePlanPriceYuan.
func instanceBillingPriceYuan(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return value / 100
}

// instanceBillingInstancePriceYuan mirrors serializeInstanceBillingPriceYuan.
func instanceBillingInstancePriceYuan(price *float64, fallbackPlanPrice float64) float64 {
	if price != nil && !math.IsNaN(*price) && !math.IsInf(*price, 0) && *price > 0 {
		return *price
	}
	return instanceBillingPriceYuan(fallbackPlanPrice)
}

// instanceBillingFormatUpgradeCapacityMessage mirrors formatUpgradeCapacityMessage.
func instanceBillingFormatUpgradeCapacityMessage(reason *string) string {
	if reason == nil {
		return "实例所在节点资源不足"
	}
	switch *reason {
	case "host_not_found":
		return "实例所在节点不存在"
	case "host_not_online":
		return "实例所在节点不在线"
	case "cpu_insufficient":
		return "实例所在节点 CPU 配额不足"
	case "memory_insufficient":
		return "实例所在节点内存不足"
	case "disk_insufficient":
		return "实例所在节点磁盘容量不足"
	default:
		return "实例所在节点资源不足"
	}
}

// instanceBillingMonthlyPrice mirrors calculateMonthlyPrice.
func instanceBillingMonthlyPrice(price float64, cycle *int32) float64 {
	normalized := int32(1)
	if cycle != nil && *cycle > 1 {
		normalized = *cycle
	}
	return price / float64(normalized)
}

// instanceBillingRenewBilling mirrors calculateRenewBilling.
func instanceBillingRenewBilling(instance *models.Instance, months int, now time.Time) (monthlyPrice float64, amount float64, newExpiresAt time.Time, err error) {
	if months < 1 || months > 24 {
		return 0, 0, time.Time{}, errors.New("续费月数必须在 1-24 之间")
	}
	price := 0.0
	if instance.BillingPrice != nil {
		price = instanceBillingDecimalsToFloat(instance.BillingPrice.Decimal)
	}
	monthlyPrice = instanceBillingMonthlyPrice(price, instance.BillingCycle)
	// Number((monthlyPrice * months).toFixed(2))
	amount = lib.JSFixed(monthlyPrice*float64(months), 2)

	baseDate := now
	if instance.ExpiresAt != nil && instance.ExpiresAt.After(now) {
		baseDate = *instance.ExpiresAt
	}
	newExpiresAt = lib.AddMonths(baseDate, months)
	return monthlyPrice, amount, newExpiresAt, nil
}

// instanceBillingPreviewRenewPrices mirrors previewRenewPrices.
func instanceBillingPreviewRenewPrices(instance *models.Instance, now time.Time) []instanceBillingRenewPreview {
	previews := make([]instanceBillingRenewPreview, 0, len(instanceBillingRenewMonths))
	for _, months := range instanceBillingRenewMonths {
		_, amount, expiresAt, err := instanceBillingRenewBilling(instance, months, now)
		if err != nil {
			continue
		}
		previews = append(previews, instanceBillingRenewPreview{
			Months:    int32(months),
			Amount:    amount,
			ExpiresAt: expiresAt,
		})
	}
	return previews
}

// instanceBillingNormalizePlanTrafficLimitSpeed mirrors
// normalizePlanTrafficLimitSpeed while preserving the null-vs-undefined
// distinction the plan-change flow depends on: `ok=false` is the TypeScript
// `undefined` (invalid configuration), a nil value with ok=true is `null` (no
// limit to apply).
func instanceBillingNormalizePlanTrafficLimitSpeed(raw string) (*string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "0" {
		return nil, true
	}
	if match := instanceAdvancedConfigSpeedRegex.FindStringSubmatch(trimmed); match != nil {
		numeric, err := strconv.ParseFloat(match[1], 64)
		if err != nil || math.IsNaN(numeric) || math.IsInf(numeric, 0) || numeric <= 0 {
			return nil, false
		}
		multiplier := 1.0
		if strings.ToLower(match[2]) == "gbit" {
			multiplier = 1000
		}
		mbps := int64(math.Round(numeric * multiplier))
		if mbps <= 0 {
			return nil, false
		}
		formatted := strconv.FormatInt(mbps, 10) + "Mbit"
		return &formatted, true
	}
	if isAllDigitsPattern(trimmed) {
		bytes, ok := new(big.Int).SetString(trimmed, 10)
		if !ok {
			return nil, false
		}
		mbps := new(big.Int).Quo(bytes, big.NewInt(instanceAdvancedConfigMBBytes))
		if mbps.Sign() <= 0 {
			return nil, true
		}
		formatted := mbps.String() + "Mbit"
		return &formatted, true
	}
	return nil, false
}

// instanceBillingCapacityMessage mirrors the route's `price` serialisation plus
// the `Number(...)`/finite checks used by the change-plan preview.
func instanceBillingFiniteYuan(value *float64) float64 {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return 0
	}
	return *value
}

// ==================== billing info ====================

type instanceBillingRenewPreview struct {
	Months           int32
	Amount           float64
	DiscountedAmount float64
	ExpiresAt        time.Time
}

type instanceBillingAffDiscount struct {
	DiscountRate float64
	AffCodeID    int32
}

type instanceBillingInfo struct {
	IsPaid          bool
	ExpiresAt       *time.Time
	BillingPrice    *float64
	BillingCycle    *int32
	MonthlyPrice    *float64
	AutoRenew       bool
	PackagePlan     *models.PackagePlan
	RenewPreview    []instanceBillingRenewPreview
	AffDiscount     *instanceBillingAffDiscount
	DaysUntilExpire *int64
}

// instanceBillingGetInfo mirrors getInstanceBillingInfo.
func instanceBillingGetInfo(ctx context.Context, instanceID int32) (*instanceBillingInfo, error) {
	instance, err := db.GetInstanceForBilling(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if instance == nil {
		return nil, nil
	}

	isPaid := instance.PackagePlanID != nil
	now := time.Now().UTC()

	var daysUntilExpire *int64
	if instance.ExpiresAt != nil {
		remainingMs := instance.ExpiresAt.Sub(now).Milliseconds()
		days := int64(math.Ceil(float64(remainingMs) / float64(24*60*60*1000)))
		daysUntilExpire = &days
	}

	var packagePlan *models.PackagePlan
	if instance.PackagePlanID != nil {
		packagePlan, err = db.GetPackagePlanById(ctx, *instance.PackagePlanID)
		if err != nil {
			return nil, err
		}
	}

	info := &instanceBillingInfo{
		IsPaid:          isPaid,
		ExpiresAt:       instance.ExpiresAt,
		BillingCycle:    instance.BillingCycle,
		AutoRenew:       instance.AutoRenew,
		PackagePlan:     packagePlan,
		DaysUntilExpire: daysUntilExpire,
	}
	if instance.BillingPrice != nil {
		price := instanceBillingDecimalsToFloat(instance.BillingPrice.Decimal)
		info.BillingPrice = &price
	}

	if !isPaid || instance.BillingPrice == nil {
		return info, nil
	}

	billingPrice := instanceBillingDecimalsToFloat(instance.BillingPrice.Decimal)
	monthlyPrice := instanceBillingMonthlyPrice(billingPrice, instance.BillingCycle)
	info.MonthlyPrice = &monthlyPrice

	promoBinding, err := db.PromoGetInstanceBinding(ctx, instanceID, nil)
	if err != nil {
		return nil, err
	}
	isPromoActive := promoBinding != nil &&
		promoBinding.PromoCode.Enabled &&
		(promoBinding.PromoCode.ExpiresAt == nil || promoBinding.PromoCode.ExpiresAt.After(now))

	affBinding, err := db.AffGetInstanceBinding(ctx, instanceID, nil)
	if err != nil {
		return nil, err
	}
	legacyAffDiscountRate := 0.0
	if affBinding != nil && affBinding.AffCode.Enabled {
		legacyAffDiscountRate = instanceBillingDecimalsToFloat(affBinding.AffCode.DiscountRate.Decimal)
	}

	_, vipBenefit, err := services.VipGetUserContinuousVipBenefit(ctx, instance.UserID, nil)
	if err != nil {
		return nil, err
	}
	vipPercent := vipBenefit.OrderDiscountPercent

	originalPreview := instanceBillingPreviewRenewPrices(instance, now)

	maxEffectiveDiscountRate := 0.0
	renewPreview := make([]instanceBillingRenewPreview, 0, len(originalPreview))
	for _, item := range originalPreview {
		var promoQuote *services.PromoRenewalSplitResult
		if isPromoActive && promoBinding != nil {
			quote := services.PromoQuoteRenewal(monthlyPrice, int(item.Months), promoBinding)
			promoQuote = &quote
		}

		affRate := legacyAffDiscountRate
		vipPrice := services.VipArbitratePrice(services.VipPriceArbitrationInput{
			BasePrice:          item.Amount,
			AffDiscountRate:    &affRate,
			VipDiscountPercent: &vipPercent,
		})

		finalPrice := vipPrice.FinalPrice
		discountAmount := vipPrice.DiscountAmount
		if promoQuote != nil && promoQuote.FinalAmount <= finalPrice && promoQuote.DiscountAmount > 0 {
			finalPrice = promoQuote.FinalAmount
			discountAmount = promoQuote.DiscountAmount
		}

		itemRate := 0.0
		if item.Amount > 0 {
			itemRate = discountAmount / item.Amount
		}
		if itemRate > maxEffectiveDiscountRate {
			maxEffectiveDiscountRate = itemRate
		}

		renewPreview = append(renewPreview, instanceBillingRenewPreview{
			Months:           item.Months,
			Amount:           item.Amount,
			DiscountedAmount: finalPrice,
			ExpiresAt:        item.ExpiresAt,
		})
	}
	info.RenewPreview = renewPreview

	if maxEffectiveDiscountRate > 0 || isPromoActive || affBinding != nil {
		affCodeID := int32(0)
		if promoBinding != nil {
			affCodeID = promoBinding.PromoCodeID
		} else if affBinding != nil {
			affCodeID = affBinding.AffCode.ID
		}
		info.AffDiscount = &instanceBillingAffDiscount{
			DiscountRate: lib.JSFixed(maxEffectiveDiscountRate, 4),
			AffCodeID:    affCodeID,
		}
	}

	return info, nil
}

// instanceBillingPreviewOptions maps the info preview to the wire shape.
func instanceBillingPreviewOptions(preview []instanceBillingRenewPreview) []instanceBillingPreviewOption {
	options := make([]instanceBillingPreviewOption, 0, len(preview))
	for _, item := range preview {
		options = append(options, instanceBillingPreviewOption{
			Months:          item.Months,
			Price:           item.Amount,
			DiscountedPrice: item.DiscountedAmount,
			ExpiresAt:       instanceBillingISOTime(item.ExpiresAt),
		})
	}
	return options
}

// ==================== refund quote / plan change ====================

type instanceBillingRefundQuote struct {
	RemainingDays  int
	RemainingValue float64
	Refundable     float64
	MaxRefundable  float64
	IsPaid         bool
}

// instanceBillingCalculateRefundQuote mirrors calculateInstanceRemainingRefundQuote.
func instanceBillingCalculateRefundQuote(ctx context.Context, instance *models.Instance) (instanceBillingRefundQuote, error) {
	isPaid := instance.PackagePlanID != nil && instance.ExpiresAt != nil && instance.BillingPrice != nil
	if !isPaid {
		return instanceBillingRefundQuote{}, nil
	}

	now := time.Now().UTC()
	if !instance.ExpiresAt.After(now) {
		return instanceBillingRefundQuote{IsPaid: true}, nil
	}

	promoRefund, err := services.PromoGetRefundEligibility(ctx, instance.ID, nil)
	if err != nil {
		return instanceBillingRefundQuote{}, err
	}
	if !promoRefund.IsRefundable {
		return instanceBillingRefundQuote{IsPaid: true}, nil
	}

	remainingDays := lib.CalculateRemainingDays(*instance.ExpiresAt, now)
	maxRefundable, err := db.GetInstanceBillingMaxRefundable(ctx, instance.ID)
	if err != nil {
		return instanceBillingRefundQuote{}, err
	}
	records, err := db.GetInstanceBillingPositivePeriods(ctx, instance.ID)
	if err != nil {
		return instanceBillingRefundQuote{}, err
	}

	remainingValue := 0.0
	for _, record := range records {
		totalMs := record.PeriodEnd.Sub(record.PeriodStart).Milliseconds()
		if totalMs <= 0 || !record.PeriodEnd.After(now) {
			continue
		}
		remainingMs := record.PeriodEnd.Sub(now).Milliseconds()
		if remainingMs < 0 {
			remainingMs = 0
		}
		unusedRatio := math.Min(float64(remainingMs)/float64(totalMs), 1)
		remainingValue += instanceBillingDecimalsToFloat(record.Amount.Decimal) * unusedRatio
	}
	remainingValue = lib.JSFixed(remainingValue, 2)

	if len(records) == 0 && instance.BillingPrice != nil && instance.BillingCycle != nil {
		discountRate := 0.0
		affBinding, err := db.AffGetInstanceBinding(ctx, instance.ID, nil)
		if err != nil {
			return instanceBillingRefundQuote{}, err
		}
		if affBinding != nil && affBinding.AffCode.Enabled {
			discountRate = instanceBillingDecimalsToFloat(affBinding.AffCode.DiscountRate.Decimal)
		}
		remainingValue = lib.CalculateRemainingValue(
			instanceBillingDecimalsToFloat(instance.BillingPrice.Decimal),
			int(*instance.BillingCycle),
			float64(remainingDays),
			discountRate,
		)
	}

	refundable := lib.JSFixed(math.Min(remainingValue, maxRefundable), 2)

	return instanceBillingRefundQuote{
		RemainingDays:  remainingDays,
		RemainingValue: remainingValue,
		Refundable:     refundable,
		MaxRefundable:  maxRefundable,
		IsPaid:         true,
	}, nil
}

type instanceBillingPlanChangeResult struct {
	OldPlan        models.PackagePlan
	NewPlan        models.PackagePlan
	RemainingDays  float64
	OldDailyPrice  float64
	NewDailyPrice  float64
	RemainingValue float64
	NewPlanCost    float64
	DiscountRate   float64
	DiscountAmount float64
	PriceDiff      float64
	IsUpgrade      bool
	NewExpiresAt   time.Time
	NewConfig      instanceBillingNewConfig
	CanChange      bool
	CannotReason   *string
}

// instanceBillingCalculatePlanChange mirrors calculatePlanChange.
func instanceBillingCalculatePlanChange(ctx context.Context, instance *models.Instance, newPlan models.PackagePlan) (instanceBillingPlanChangeResult, error) {
	if instance.PackagePlanID == nil {
		return instanceBillingPlanChangeResult{}, errors.New("免费实例不支持升降级")
	}
	oldPlan, err := db.GetPackagePlanById(ctx, *instance.PackagePlanID)
	if err != nil {
		return instanceBillingPlanChangeResult{}, err
	}
	if oldPlan == nil {
		return instanceBillingPlanChangeResult{}, errors.New("原方案不存在")
	}
	if instance.ExpiresAt == nil {
		return instanceBillingPlanChangeResult{}, errors.New("实例到期时间不存在")
	}

	expiresAt := *instance.ExpiresAt
	remainingDays := lib.CalculateRemainingDaysPrecise(expiresAt, time.Now().UTC())
	const minRemainingDays = 15.0

	canChange := true
	var cannotReason *string
	if remainingDays < minRemainingDays {
		canChange = false
		cannotReason = instanceBillingStringPointer("remaining_days_insufficient")
	}

	discountRate := 0.0
	affBinding, err := db.AffGetInstanceBinding(ctx, instance.ID, nil)
	if err != nil {
		return instanceBillingPlanChangeResult{}, err
	}
	if affBinding != nil {
		discountRate = instanceBillingDecimalsToFloat(affBinding.AffCode.DiscountRate.Decimal)
	}

	oldCyclePrice := 0.0
	if instance.BillingPrice != nil {
		oldCyclePrice = instanceBillingDecimalsToFloat(instance.BillingPrice.Decimal)
	}
	newCyclePrice := instanceBillingPriceYuan(instanceBillingDecimalsToFloat(newPlan.Price.Decimal))

	paidQuote, err := instanceBillingCalculateRefundQuote(ctx, instance)
	if err != nil {
		return instanceBillingPlanChangeResult{}, err
	}

	cycle := int32(1)
	if instance.BillingCycle != nil {
		cycle = *instance.BillingCycle
	}

	calc := lib.CalculatePlanChangeDetails(
		oldCyclePrice,
		int(cycle),
		newCyclePrice,
		int(newPlan.BillingCycle),
		remainingDays,
		discountRate,
		paidQuote.RemainingValue,
		paidQuote.MaxRefundable,
	)

	priceDiff := calc.PriceDiff
	promoRefund, err := services.PromoGetRefundEligibility(ctx, instance.ID, nil)
	if err != nil {
		return instanceBillingPlanChangeResult{}, err
	}
	if !promoRefund.IsRefundable && (priceDiff < 0 || !calc.IsUpgrade) {
		priceDiff = 0
	}

	return instanceBillingPlanChangeResult{
		OldPlan:        *oldPlan,
		NewPlan:        newPlan,
		RemainingDays:  remainingDays,
		OldDailyPrice:  calc.OldDailyPrice,
		NewDailyPrice:  calc.NewDailyPrice,
		RemainingValue: calc.RemainingValue,
		NewPlanCost:    calc.NewPlanCost,
		DiscountRate:   discountRate,
		DiscountAmount: calc.DiscountAmount,
		PriceDiff:      priceDiff,
		IsUpgrade:      calc.IsUpgrade,
		NewExpiresAt:   expiresAt,
		NewConfig: instanceBillingNewConfig{
			CPU:    newPlan.CPU,
			Memory: newPlan.Memory,
			Disk:   newPlan.Disk,
		},
		CanChange:    canChange,
		CannotReason: cannotReason,
	}, nil
}

// ==================== host lookup ====================

type instanceBillingHost struct {
	ID            int32   `gorm:"column:id"`
	UserID        int32   `gorm:"column:user_id"`
	URL           string  `gorm:"column:url"`
	CertPath      *string `gorm:"column:cert_path"`
	KeyPath       *string `gorm:"column:key_path"`
	TunnelEnabled bool    `gorm:"column:tunnel_enabled"`
	TargetHost    string  `gorm:"column:target_host"`
	TargetPort    int32   `gorm:"column:target_port"`
}

func instanceBillingGetHost(ctx context.Context, hostID int32) (*instanceBillingHost, error) {
	if db.DB == nil {
		return nil, errors.New("database not initialized")
	}
	var rows []instanceBillingHost
	err := db.DB.WithContext(ctx).Raw(
		`SELECT id, user_id, url, cert_path, key_path, tunnel_enabled, target_host, target_port
		   FROM hosts WHERE id = ?`, hostID,
	).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ==================== renewal ====================

type instanceBillingRenewResult struct {
	NewExpiresAt   time.Time
	Amount         float64
	DiscountAmount *float64
}

// instanceBillingPerformRenewal mirrors performRenewal.
func instanceBillingPerformRenewal(ctx context.Context, userID int32, instance *models.Instance, months int) (instanceBillingRenewResult, error) {
	if instance.Status == models.InstanceStatusDeleted {
		return instanceBillingRenewResult{}, errors.New("实例已删除，无法续费")
	}
	if instance.PackagePlanID == nil {
		return instanceBillingRenewResult{}, errors.New("免费实例无需续费")
	}
	if months < 1 || months > 24 {
		return instanceBillingRenewResult{}, errors.New("续费月数必须在 1-24 之间")
	}
	if instance.Status == models.InstanceStatusSuspended && (instance.SuspendReason == nil || *instance.SuspendReason != "expired") {
		return instanceBillingRenewResult{}, errors.New("实例已被手动封停，请联系宿主机所有者解封后再续费")
	}

	now := time.Now().UTC()
	monthlyPrice, originalAmount, newExpiresAt, err := instanceBillingRenewBilling(instance, months, now)
	if err != nil {
		return instanceBillingRenewResult{}, err
	}

	promoBinding, err := db.PromoGetInstanceBinding(ctx, instance.ID, nil)
	if err != nil {
		return instanceBillingRenewResult{}, err
	}
	isPromoActive := promoBinding != nil &&
		promoBinding.PromoCode.Enabled &&
		(promoBinding.PromoCode.ExpiresAt == nil || promoBinding.PromoCode.ExpiresAt.After(now))

	var promoRenewalQuote *services.PromoRenewalSplitResult
	if isPromoActive && promoBinding != nil {
		quote := services.PromoQuoteRenewal(monthlyPrice, months, promoBinding)
		promoRenewalQuote = &quote
	}

	affBinding, err := db.AffGetInstanceBinding(ctx, instance.ID, nil)
	if err != nil {
		return instanceBillingRenewResult{}, err
	}

	_, vipBenefit, err := services.VipGetUserContinuousVipBenefit(ctx, userID, nil)
	if err != nil {
		return instanceBillingRenewResult{}, err
	}
	vipPercent := vipBenefit.OrderDiscountPercent

	var affDiscountRate *float64
	if affBinding != nil && affBinding.AffCode.Enabled {
		rate := instanceBillingDecimalsToFloat(affBinding.AffCode.DiscountRate.Decimal)
		affDiscountRate = &rate
	}

	renewalPrice := services.VipArbitratePrice(services.VipPriceArbitrationInput{
		BasePrice:          originalAmount,
		AffDiscountRate:    affDiscountRate,
		VipDiscountPercent: &vipPercent,
	})
	finalAmount := renewalPrice.FinalPrice
	discountAmount := renewalPrice.DiscountAmount
	pricingSource := renewalPrice.Source
	isPromoApplied := false

	if promoRenewalQuote != nil && promoRenewalQuote.FinalAmount <= finalAmount && promoRenewalQuote.DiscountAmount > 0 {
		finalAmount = promoRenewalQuote.FinalAmount
		discountAmount = promoRenewalQuote.DiscountAmount
		pricingSource = "promo"
		isPromoApplied = true
	}

	periodStart := now
	if instance.ExpiresAt != nil {
		periodStart = *instance.ExpiresAt
	}

	remark := fmt.Sprintf("续费（%d个月）：%s", months, instance.Name)
	if discountAmount > 0 {
		remark = fmt.Sprintf("续费（%d个月）：%s，优惠码折扣 -¥%s",
			months, instance.Name, instanceBillingFixed2(discountAmount))
	}
	recordRemark := fmt.Sprintf("续费 %d 个月", months)
	if discountAmount > 0 {
		recordRemark = fmt.Sprintf("续费 %d 个月，优惠码折扣 -¥%s", months, instanceBillingFixed2(discountAmount))
	}

	var balanceLogID int32
	alreadyApplied := false
	version := instance.Version
	status := instance.Status
	suspendReason := instance.SuspendReason

	txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. Serialise every operation on this instance (WAVE_D_SPEC §0.4, 4106).
		locked, err := db.TryInstanceBillingAdvisoryLock(ctx, tx, db.InstanceOperationLockNamespace, instance.ID)
		if err != nil {
			return err
		}
		if !locked {
			return errors.New("实例正在处理中，请稍后重试")
		}

		// 2. Exact-replay guard: a renew that would end at the same period_end as
		//    a record written moments ago has already been applied. Without this,
		//    a client retry after a lost response would debit twice.
		var replays []models.InstanceBillingRecord
		if err := tx.WithContext(ctx).Raw(
			`SELECT id, amount, period_end
			   FROM instance_billing_records
			  WHERE instance_id = ? AND user_id = ? AND type = 'renew'
			    AND period_end = ? AND created_at >= ?
			  ORDER BY id DESC LIMIT 1`,
			instance.ID, userID, newExpiresAt, now.Add(-time.Duration(instanceBillingReplayWindowSeconds)*time.Second),
		).Scan(&replays).Error; err != nil {
			return err
		}
		if instanceBillingRenewalAlreadyApplied(replays, newExpiresAt) {
			alreadyApplied = true
			existing := instanceBillingDecimalsToFloat(replays[0].Amount.Decimal)
			finalAmount = existing
			return nil
		}

		// 3. Debit the balance under the 4110 advisory lock with snapshot logs.
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

		// 4. Optimistic instance update (version guard).
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

		// 5. Billing record in the same transaction.
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

		// 6. Promo settlement joins the same transaction.
		if isPromoApplied && promoBinding != nil {
			renewResult, err := services.PromoSettleOnRenew(ctx, tx, services.PromoSettleRenewParams{
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
				// services/promo/notifications.ts is not ported yet; the inbox
				// message is a non-critical side effect, never a reason to roll
				// back a paid renewal.
				log.Printf("[instance-billing] promo binding exhausted for instance %d", instance.ID)
			}
		}

		return nil
	})
	if txErr != nil {
		return instanceBillingRenewResult{}, txErr
	}

	// AFF commission runs after the commit on purpose. The Go AFF layer
	// (db.AffProcessCommission) is pgx-typed and opens its own transaction, so it
	// cannot join the GORM transaction above; paying the commission afterwards
	// guarantees it is never credited for a renewal that rolled back. The trade
	// off is that a crash between commit and commission loses the commission
	// (never the user's payment) — documented in the module report.
	if !alreadyApplied && affBinding != nil && affBinding.AffCode.Enabled && pricingSource == services.VipPriceSourceAff {
		if err := db.AffProcessCommission(ctx, affBinding.AffCode.ID, instance.ID, originalAmount, models.AffLogTypeRenew, nil); err != nil {
			log.Printf("[instance-billing] AFF commission failed for instance %d: %v", instance.ID, err)
		}
	}

	var discountPtr *float64
	if discountAmount > 0 {
		value := discountAmount
		discountPtr = &value
	}

	return instanceBillingRenewResult{
		NewExpiresAt:   newExpiresAt,
		Amount:         finalAmount,
		DiscountAmount: discountPtr,
	}, nil
}

// instanceBillingFixed2 mirrors `value.toFixed(2)` for a non-negative amount.
func instanceBillingFixed2(value float64) string {
	return strconv.FormatFloat(lib.JSFixed(value, 2), 'f', 2, 64)
}

// ==================== plan change execution ====================

type instanceBillingPlanChangeOutcome struct {
	PriceDiff      float64
	RefundAmount   *float64
	NewConfig      instanceBillingNewConfig
	BandwidthLimit *string
	Capacity       db.HostsPlanUpgradeCapacityCheck
}

// instanceBillingPerformPlanChange mirrors performPlanChange.
func instanceBillingPerformPlanChange(ctx context.Context, userID int32, instance *models.Instance, newPlan models.PackagePlan) (instanceBillingPlanChangeOutcome, error) {
	if instance.Status == models.InstanceStatusDeleted {
		return instanceBillingPlanChangeOutcome{}, errors.New("实例已删除，无法改配")
	}

	isVM := false
	if db.DB != nil {
		var rows []struct {
			InstanceType string `gorm:"column:instance_type"`
		}
		if err := db.DB.WithContext(ctx).Raw("SELECT instance_type::text AS instance_type FROM packages WHERE id = ?", newPlan.PackageID).Scan(&rows).Error; err != nil {
			return instanceBillingPlanChangeOutcome{}, err
		}
		if len(rows) > 0 && rows[0].InstanceType == "vm" {
			isVM = true
		}
	}

	if instance.PackagePlanID == nil {
		return instanceBillingPlanChangeOutcome{}, errors.New("免费实例不支持升降级")
	}
	if instance.Status != models.InstanceStatusRunning && instance.Status != models.InstanceStatusStopped {
		return instanceBillingPlanChangeOutcome{}, errors.New("当前实例状态不允许升降级，仅运行中或已停止的实例可以升降级")
	}
	if instance.PackageID == nil || newPlan.PackageID != *instance.PackageID {
		return instanceBillingPlanChangeOutcome{}, errors.New("新方案必须属于同一套餐")
	}
	if !newPlan.IsActive {
		return instanceBillingPlanChangeOutcome{}, errors.New("新方案已下架")
	}
	if newPlan.IsSoldOut {
		return instanceBillingPlanChangeOutcome{}, errors.New("新方案已售罄")
	}
	if instance.PackagePlanID != nil && *instance.PackagePlanID == newPlan.ID {
		return instanceBillingPlanChangeOutcome{}, errors.New("不能切换到相同方案")
	}

	if err := db.GpusAssertPlanChange(ctx, nil, instance.ID, newPlan.ID); err != nil {
		return instanceBillingPlanChangeOutcome{}, err
	}

	changeResult, err := instanceBillingCalculatePlanChange(ctx, instance, newPlan)
	if err != nil {
		return instanceBillingPlanChangeOutcome{}, err
	}

	normalizedSpeed, speedOK := instanceBillingNormalizePlanTrafficLimitSpeed(newPlan.TrafficLimitSpeed)
	if !speedOK {
		return instanceBillingPlanChangeOutcome{}, errors.New("新方案带宽配置无效")
	}

	if !changeResult.CanChange {
		if changeResult.CannotReason != nil && *changeResult.CannotReason == "remaining_days_insufficient" {
			return instanceBillingPlanChangeOutcome{}, errors.New("剩余时间不足 15 天，无法变更方案")
		}
		return instanceBillingPlanChangeOutcome{}, errors.New("当前不能变更方案")
	}

	var refundAmount *float64
	var capacity db.HostsPlanUpgradeCapacityCheck
	version := instance.Version
	now := time.Now().UTC()

	instanceSpec := db.HostsInstanceSpec{
		ID: instance.ID, HostID: instance.HostID,
		CPU: instance.CPU, Memory: instance.Memory, Disk: instance.Disk,
	}
	planSpec := db.HostsPlanSpec{CPU: newPlan.CPU, Memory: newPlan.Memory, Disk: newPlan.Disk}

	txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := db.GpusAssertPlanChange(ctx, tx, instance.ID, newPlan.ID); err != nil {
			return err
		}

		// Serialise instance operations before reading/writing any resource row.
		locked, err := db.TryInstanceBillingAdvisoryLock(ctx, tx, db.InstanceOperationLockNamespace, instance.ID)
		if err != nil {
			return err
		}
		if !locked {
			return errors.New("实例正在处理中，请稍后重试")
		}

		var balanceRows []struct {
			Balance models.Decimal `gorm:"column:balance"`
		}
		if err := tx.WithContext(ctx).Raw("SELECT balance FROM users WHERE id = ?", userID).Scan(&balanceRows).Error; err != nil {
			return err
		}
		if len(balanceRows) == 0 {
			return errors.New("用户不存在")
		}
		oldBalance := instanceBillingDecimalsToFloat(balanceRows[0].Balance.Decimal)

		reserved, err := db.HostsReservePlanUpgradeCapacityWithLock(ctx, tx, instanceSpec, planSpec)
		if err != nil {
			return err
		}
		capacity = reserved
		if !capacity.CanUpgrade {
			reason := "unknown"
			if capacity.Reason != nil {
				reason = *capacity.Reason
			}
			return fmt.Errorf("HOST_RESOURCES_INSUFFICIENT:%s", reason)
		}

		if changeResult.PriceDiff > 0 {
			if oldBalance < changeResult.PriceDiff {
				return fmt.Errorf("余额不足，需补差价 %s 元", instanceBillingTrimFloat(changeResult.PriceDiff))
			}
			delta := models.Decimal{Decimal: decimal.NewFromFloat(-changeResult.PriceDiff).Round(2)}
			if _, err := db.InstanceBillingApplyBalanceTx(ctx, tx, db.InstanceBillingBalanceChange{
				UserID:     userID,
				InstanceID: instance.ID,
				Delta:      delta,
				LogType:    models.BalanceLogTypeConsume,
				Remark:     fmt.Sprintf("升级方案：%s → %s", changeResult.OldPlan.Name, newPlan.Name),
			}); err != nil {
				return err
			}
		}
		if changeResult.PriceDiff < 0 {
			refund := math.Abs(changeResult.PriceDiff)
			refundAmount = &refund
			delta := models.Decimal{Decimal: decimal.NewFromFloat(refund).Round(2)}
			if _, err := db.InstanceBillingApplyBalanceTx(ctx, tx, db.InstanceBillingBalanceChange{
				UserID:     userID,
				InstanceID: instance.ID,
				Delta:      delta,
				LogType:    models.BalanceLogTypeRefund,
				Remark:     fmt.Sprintf("降级方案退款：%s → %s", changeResult.OldPlan.Name, newPlan.Name),
			}); err != nil {
				return err
			}
		}

		monthlyTrafficLimit, err := lib.ResolveInstanceTrafficLimitForHost(ctx, tx, lib.TrafficMultiplierResolveParams{
			PackageID:        instance.PackageID,
			HostID:           instance.HostID,
			BaseTrafficLimit: &newPlan.TrafficLimit,
		})
		if err != nil {
			return err
		}

		updates := map[string]any{
			"package_plan_id":       newPlan.ID,
			"cpu":                   newPlan.CPU,
			"memory":                newPlan.Memory,
			"disk":                  newPlan.Disk,
			"billing_price":         models.Decimal{Decimal: newPlan.Price.Decimal.Div(decimal.NewFromInt(100))},
			"billing_cycle":         newPlan.BillingCycle,
			"port_limit":            newPlan.PortLimit,
			"snapshot_limit":        newPlan.SnapshotLimit,
			"backup_limit":          newPlan.BackupLimit,
			"site_limit":            newPlan.SiteLimit,
			"monthly_traffic_limit": monthlyTrafficLimit,
			"limits_ingress":        normalizedSpeed,
			"limits_egress":         normalizedSpeed,
			"traffic_status":        lib.CalculateInstanceTrafficStatus(instance.MonthlyTrafficUsed, monthlyTrafficLimit),
			"last_plan_change_at":   now,
			"version":               gorm.Expr("version + 1"),
		}

		if isVM {
			updates["swap_enabled"] = false
			updates["swap_size"] = nil
		} else {
			updates["swap_enabled"] = instance.SwapEnabled
			if lib.InstanceSwapShouldSyncWithPlan(instance.SwapSize, instanceBillingInt32Pointer(newPlan.SwapSize)) {
				updates["swap_size"] = newPlan.SwapSize
			} else {
				updates["swap_size"] = instance.SwapSize
			}
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

		amount := 0.0
		if changeResult.PriceDiff > 0 {
			amount = changeResult.PriceDiff
		}
		periodEnd := now
		if instance.ExpiresAt != nil {
			periodEnd = *instance.ExpiresAt
		}
		remark := fmt.Sprintf("%s → %s", changeResult.OldPlan.Name, newPlan.Name)
		if refundAmount != nil {
			remark = fmt.Sprintf("%s → %s，退款 ¥%s", changeResult.OldPlan.Name, newPlan.Name, instanceBillingFixed2(*refundAmount))
		}
		recordType := models.BillingRecordTypeDowngrade
		if changeResult.IsUpgrade {
			recordType = models.BillingRecordTypeUpgrade
		}
		record := models.InstanceBillingRecord{
			InstanceID:  instance.ID,
			UserID:      userID,
			Type:        recordType,
			Amount:      models.Decimal{Decimal: decimal.NewFromFloat(amount).Round(2)},
			Months:      0,
			PeriodStart: now,
			PeriodEnd:   periodEnd,
			Remark:      &remark,
		}
		return tx.WithContext(ctx).Create(&record).Error
	})
	if txErr != nil {
		return instanceBillingPlanChangeOutcome{}, txErr
	}

	return instanceBillingPlanChangeOutcome{
		PriceDiff:      changeResult.PriceDiff,
		RefundAmount:   refundAmount,
		NewConfig:      changeResult.NewConfig,
		BandwidthLimit: normalizedSpeed,
		Capacity:       capacity,
	}, nil
}

// instanceBillingTrimFloat mirrors JavaScript's number-to-string for the
// "余额不足，需补差价 X 元" message.
func instanceBillingTrimFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// ==================== batch helpers ====================

// instanceBillingBuildBatchPreviewItem mirrors buildBatchRenewPreviewItem.
func instanceBillingBuildBatchPreviewItem(ctx context.Context, userID, instanceID int32) (instanceBillingBatchPreviewItem, error) {
	hidden := func(name string) instanceBillingBatchPreviewItem {
		reason := instanceBillingBatchHiddenReason
		return instanceBillingBatchPreviewItem{
			ID: instanceID, Name: name, CanRenew: false, AutoRenew: false,
			Reason: &reason, DaysUntilExpire: nil,
			Options: make([]instanceBillingPreviewOption, 0),
		}
	}

	instance, err := db.GetInstanceForBilling(ctx, instanceID)
	if err != nil {
		return instanceBillingBatchPreviewItem{}, err
	}
	if instance == nil || instance.UserID != userID {
		return hidden(fmt.Sprintf("#%d", instanceID)), nil
	}
	if instance.Status == models.InstanceStatusDeleted {
		reason := "实例已删除，无法续费"
		return instanceBillingBatchPreviewItem{
			ID: instance.ID, Name: instance.Name, CanRenew: false, AutoRenew: false,
			Reason: &reason, DaysUntilExpire: nil,
			Options: make([]instanceBillingPreviewOption, 0),
		}, nil
	}
	if instance.PackagePlanID == nil {
		reason := "免费实例无需续费"
		return instanceBillingBatchPreviewItem{
			ID: instance.ID, Name: instance.Name, CanRenew: false, AutoRenew: false,
			Reason: &reason, DaysUntilExpire: nil,
			Options: make([]instanceBillingPreviewOption, 0),
		}, nil
	}

	info, err := instanceBillingGetInfo(ctx, instanceID)
	if err != nil {
		return instanceBillingBatchPreviewItem{}, err
	}
	if info == nil {
		reason := "无法获取实例计费信息"
		return instanceBillingBatchPreviewItem{
			ID: instance.ID, Name: instance.Name, CanRenew: false, AutoRenew: false,
			Reason: &reason, DaysUntilExpire: nil,
			Options: make([]instanceBillingPreviewOption, 0),
		}, nil
	}

	options := instanceBillingPreviewOptions(info.RenewPreview)
	item := instanceBillingBatchPreviewItem{
		ID:              instance.ID,
		Name:            instance.Name,
		CanRenew:        len(options) > 0,
		AutoRenew:       info.AutoRenew,
		DaysUntilExpire: info.DaysUntilExpire,
		Options:         options,
	}
	if len(options) == 0 {
		item.Reason = instanceBillingStringPointer("暂无可用续费选项")
	}
	return item, nil
}

// instanceBillingRenewForUser mirrors executeRenewForUser.
func instanceBillingRenewForUser(ctx context.Context, userID int32, instanceID int32, months int) (instanceBillingBatchRenewResult, error) {
	preview, err := instanceBillingBuildBatchPreviewItem(ctx, userID, instanceID)
	if err != nil {
		return instanceBillingBatchRenewResult{}, err
	}
	skipped := instanceBillingBoolPointer(true)
	if !preview.CanRenew {
		return instanceBillingBatchRenewResult{
			ID: preview.ID, Name: preview.Name, Success: false, Skipped: skipped, Reason: preview.Reason,
		}, nil
	}

	found := false
	for _, option := range preview.Options {
		if int(option.Months) == months {
			found = true
			break
		}
	}
	if !found {
		return instanceBillingBatchRenewResult{
			ID: preview.ID, Name: preview.Name, Success: false, Skipped: skipped,
			Reason: instanceBillingStringPointer("该实例不支持当前续费时长"),
		}, nil
	}

	instance, err := db.GetInstanceForBilling(ctx, instanceID)
	if err != nil {
		return instanceBillingBatchRenewResult{}, err
	}
	if instance == nil || instance.UserID != userID {
		reason := instanceBillingBatchHiddenReason
		return instanceBillingBatchRenewResult{
			ID: instanceID, Name: fmt.Sprintf("#%d", instanceID), Success: false, Skipped: skipped, Reason: &reason,
		}, nil
	}
	if instance.Status == models.InstanceStatusDeleted {
		reason := "实例已删除，无法续费"
		return instanceBillingBatchRenewResult{
			ID: instance.ID, Name: instance.Name, Success: false, Skipped: skipped, Reason: &reason,
		}, nil
	}

	result, err := instanceBillingPerformRenewal(ctx, userID, instance, months)
	if err != nil {
		log.Printf("[instance-billing] batch renew failed for instance %d: %v", instance.ID, err)
		return instanceBillingBatchRenewResult{
			ID: preview.ID, Name: preview.Name, Success: false,
			Reason: instanceBillingStringPointer("续费失败"),
		}, nil
	}

	newExpiresAt := instanceBillingISOTime(result.NewExpiresAt)
	lib.SendNotification(ctx, int64(userID), lib.EventInstanceRenewed, lib.EventData{
		"instanceName": instance.Name,
		"renewAmount":  result.Amount,
		"renewMonths":  months,
		"newExpiresAt": newExpiresAt,
	})

	if userInfo, lookupErr := db.FindUserById(ctx, userID); lookupErr == nil && userInfo != nil && userInfo.Email != nil {
		lib.SendRenewSuccessEmail(ctx, *userInfo.Email, lib.RenewSuccessEmailData{
			Username:     userInfo.Username,
			InstanceName: instance.Name,
			Amount:       result.Amount,
			Months:       months,
			NewExpiresAt: result.NewExpiresAt,
		})
	}

	instanceIDCopy := instance.ID
	db.CreateLog(ctx, instanceBillingInt64Pointer(int64(userID)), instanceBillingLogModule, "instance.renew",
		fmt.Sprintf(`Renewed instance "%s" for %d month(s), amount: %s`, instance.Name, months, instanceBillingTrimFloat(result.Amount)),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceIDCopy))

	amount := result.Amount
	return instanceBillingBatchRenewResult{
		ID: instance.ID, Name: instance.Name, Success: true,
		Amount: &amount, NewExpiresAt: &newExpiresAt,
	}, nil
}

// instanceBillingInt64Pointer mirrors taking the address of the logged-in user id.
func instanceBillingInt64Pointer(value int64) *int64 { return &value }

// ==================== body validation ====================

// instanceBillingBodyObject mirrors AJV's root `type: 'object'` check.
func instanceBillingBodyObject(decoded any) (map[string]any, string) {
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, "must be object"
	}
	return object, ""
}

// instanceBillingAJVInteger mirrors AJV's integer coercion for one property.
func instanceBillingAJVInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed {
			return 0, false
		}
		return int64(typed), true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	case nil:
		// AJV coerces null to 0 for an integer property (measured: {"instanceIds":null}
		// on batch destroy-info answers 200 with an id-0 item, not a validation error).
		return 0, true
	case []any:
		if len(typed) == 1 {
			return instanceBillingAJVInteger(typed[0])
		}
	}
	return 0, false
}

// instanceBillingValidateMonths mirrors the months property (integer 1..24).
func instanceBillingValidateMonths(object map[string]any) (int, string) {
	raw, present := object["months"]
	if !present {
		return 0, "months is required"
	}
	value, ok := instanceBillingAJVInteger(raw)
	if !ok {
		return 0, "must be integer"
	}
	if value > 24 {
		return 0, "must be <= 24"
	}
	if value < 1 {
		return 0, "must be >= 1"
	}
	return int(value), ""
}

// instanceBillingValidatePositiveInteger mirrors `{ type: 'integer' }` with the
// route's extra safe-integer/positive check.
func instanceBillingValidatePositiveInteger(object map[string]any, name string) (int32, string) {
	raw, present := object[name]
	if !present {
		return 0, name + " is required"
	}
	value, ok := instanceBillingAJVInteger(raw)
	if !ok {
		return 0, "must be integer"
	}
	if value <= 0 || value > math.MaxInt32 {
		return 0, ""
	}
	return int32(value), ""
}

// instanceBillingValidateBoolean mirrors `{ type: 'boolean' }`.
func instanceBillingValidateBoolean(object map[string]any, name string) (bool, string) {
	raw, present := object[name]
	if !present {
		return false, name + " is required"
	}
	switch typed := raw.(type) {
	case bool:
		return typed, ""
	case string:
		if typed == "true" {
			return true, ""
		}
		if typed == "false" {
			return false, ""
		}
	case []any:
		if len(typed) == 1 {
			if value, ok := typed[0].(bool); ok {
				return value, ""
			}
			if text, ok := typed[0].(string); ok {
				if text == "true" {
					return true, ""
				}
				if text == "false" {
					return false, ""
				}
			}
		}
	}
	return false, "must be boolean"
}

// instanceBillingValidateAffCode mirrors the apply-aff string schema.
func instanceBillingValidateAffCode(object map[string]any) (string, string) {
	raw, present := object["affCode"]
	if !present {
		return "", "affCode is required"
	}
	text, ok := raw.(string)
	if !ok {
		return "", "must be string"
	}
	length := len([]rune(text))
	if length > 64 {
		return "", "affCode is too long"
	}
	if length < 3 {
		return "", "affCode is too short"
	}
	return text, ""
}

// instanceBillingValidateInstanceIDs mirrors the instanceIds array schema
// (items integer, minItems 1, maxItems 100) and returns the deduplicated list.
func instanceBillingValidateInstanceIDs(object map[string]any) ([]int32, string) {
	raw, present := object["instanceIds"]
	if !present {
		return nil, "instanceIds is required"
	}
	items, ok := raw.([]any)
	if !ok {
		// AJV's coerceTypes:'array' wraps a scalar into a one-element array. Measured
		// against Node: "950001", 950001, true and null are all accepted, while an
		// object is not coerced.
		if _, isObject := raw.(map[string]any); !isObject {
			items = []any{raw}
		} else {
			return nil, "must be array"
		}
	}
	if len(items) < 1 {
		return nil, "must NOT have fewer than 1 items"
	}
	if len(items) > 100 {
		return nil, "must NOT have more than 100 items"
	}

	seen := map[int32]bool{}
	unique := make([]int32, 0, len(items))
	for _, item := range items {
		value, ok := instanceBillingAJVInteger(item)
		if !ok {
			return nil, "must be integer"
		}
		converted := int32(value)
		if !seen[converted] {
			seen[converted] = true
			unique = append(unique, converted)
		}
	}
	return unique, ""
}

// ==================== handlers ====================

// instanceBillingGetBilling handles GET /api/instances/:id/billing.
func instanceBillingGetBilling(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, ok := instanceBillingParseRouteID(c, c.Param("id"))
	if !ok {
		return
	}

	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	host, err := instanceBillingGetHost(ctx, instance.HostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	hostOwnerID := int32(0)
	if host != nil {
		hostOwnerID = host.UserID
	}
	_, _, allowed := instanceBillingViewerAccess(instance.UserID, hostOwnerID, user.ID)
	if !allowed {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	info, err := instanceBillingGetInfo(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if info == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	view := instanceBillingBillingView{
		InstanceID:      instanceID,
		InstanceName:    instance.Name,
		Price:           info.BillingPrice,
		BillingCycle:    info.BillingCycle,
		ExpiresAt:       nil,
		AutoRenew:       info.AutoRenew,
		RenewPreview:    nil,
		AffDiscount:     nil,
		DaysUntilExpire: info.DaysUntilExpire,
	}
	if info.PackagePlan != nil {
		planID := info.PackagePlan.ID
		planName := info.PackagePlan.Name
		view.PlanID = &planID
		view.PlanName = &planName
	}
	if info.ExpiresAt != nil {
		formatted := instanceBillingISOTime(*info.ExpiresAt)
		view.ExpiresAt = &formatted
	}
	if info.RenewPreview != nil {
		view.RenewPreview = instanceBillingPreviewOptions(info.RenewPreview)
	}
	if info.AffDiscount != nil {
		view.AffDiscount = &instanceBillingAffDiscountView{
			DiscountRate:    info.AffDiscount.DiscountRate,
			DiscountPercent: int64(math.Round(info.AffDiscount.DiscountRate * 100)),
		}
	}

	c.JSON(http.StatusOK, gin.H{"billing": view})
}

// instanceBillingApplyAff handles POST /api/instances/:id/apply-aff.
//
// The feature is retired: the TypeScript validates the body and then always
// answers 403 该功能已下线.
func instanceBillingApplyAff(c *gin.Context) {
	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object, validationMessage := instanceBillingBodyObject(body.Value)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	if _, validationMessage := instanceBillingValidateAffCode(object); validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	if _, ok := instanceBillingParseRouteID(c, c.Param("id")); !ok {
		return
	}
	lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "该功能已下线")
}

// instanceBillingRenew handles POST /api/instances/:id/renew.
func instanceBillingRenew(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object, validationMessage := instanceBillingBodyObject(body.Value)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	months, validationMessage := instanceBillingValidateMonths(object)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	instanceID, ok := instanceBillingParseRouteID(c, c.Param("id"))
	if !ok {
		return
	}

	instance, err := db.GetInstanceForBilling(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.Status == models.InstanceStatusDeleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已删除，无法续费", "code": "INSTANCE_DELETED"})
		return
	}
	if instance.PackagePlanID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "免费实例无需续费", "code": "FREE_INSTANCE"})
		return
	}

	result, err := instanceBillingPerformRenewal(ctx, user.ID, instance, months)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	newExpiresAt := instanceBillingISOTime(result.NewExpiresAt)
	lib.SendNotification(ctx, int64(user.ID), lib.EventInstanceRenewed, lib.EventData{
		"instanceName": instance.Name,
		"renewAmount":  result.Amount,
		"renewMonths":  months,
		"newExpiresAt": newExpiresAt,
	})

	if userInfo, lookupErr := db.FindUserById(ctx, user.ID); lookupErr == nil && userInfo != nil && userInfo.Email != nil {
		lib.SendRenewSuccessEmail(ctx, *userInfo.Email, lib.RenewSuccessEmailData{
			Username:     userInfo.Username,
			InstanceName: instance.Name,
			Amount:       result.Amount,
			Months:       months,
			NewExpiresAt: result.NewExpiresAt,
		})
	}

	instanceIDCopy := instanceID
	db.CreateLog(ctx, instanceBillingInt64Pointer(int64(user.ID)), instanceBillingLogModule, "instance.renew",
		fmt.Sprintf(`Renewed instance "%s" for %d month(s), amount: %s`, instance.Name, months, instanceBillingTrimFloat(result.Amount)),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceIDCopy))

	c.JSON(http.StatusOK, gin.H{
		"message":      "续费成功",
		"amount":       result.Amount,
		"months":       months,
		"newExpiresAt": newExpiresAt,
	})
}

// instanceBillingBatchRenewPreview handles POST /api/instances/batch/renew-preview.
func instanceBillingBatchRenewPreview(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object, validationMessage := instanceBillingBodyObject(body.Value)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	uniqueIDs, validationMessage := instanceBillingValidateInstanceIDs(object)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	if len(uniqueIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择至少一个实例"})
		return
	}

	items := make([]instanceBillingBatchPreviewItem, 0, len(uniqueIDs))
	for _, instanceID := range uniqueIDs {
		item, err := instanceBillingBuildBatchPreviewItem(ctx, user.ID, instanceID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// instanceBillingBatchRenew handles POST /api/instances/batch/renew.
func instanceBillingBatchRenew(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object, validationMessage := instanceBillingBodyObject(body.Value)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	uniqueIDs, validationMessage := instanceBillingValidateInstanceIDs(object)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	months, validationMessage := instanceBillingValidateMonths(object)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	if len(uniqueIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择至少一个实例"})
		return
	}

	results := make([]instanceBillingBatchRenewResult, 0, len(uniqueIDs))
	for _, instanceID := range uniqueIDs {
		item, err := instanceBillingRenewForUser(ctx, user.ID, instanceID, months)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		results = append(results, item)
	}

	successCount := 0
	skippedCount := 0
	for _, item := range results {
		if item.Success {
			successCount++
		}
		if item.Skipped != nil && *item.Skipped {
			skippedCount++
		}
	}
	failedCount := len(results) - successCount - skippedCount

	result := db.LogResultSuccess
	if failedCount > 0 {
		result = db.LogResultWarning
	}
	db.CreateLog(ctx, instanceBillingInt64Pointer(int64(user.ID)), instanceBillingLogModule, "instance.batch_renew",
		fmt.Sprintf("Batch renew processed %d instance(s): success=%d, skipped=%d, failed=%d",
			len(results), successCount, skippedCount, failedCount),
		result, db.CreateLogOptions{})

	message2 := "没有实例完成续费"
	if successCount > 0 {
		message2 = "批量续费已处理"
	}
	c.JSON(http.StatusOK, gin.H{
		"message":      message2,
		"successCount": successCount,
		"skippedCount": skippedCount,
		"failedCount":  failedCount,
		"results":      results,
	})
}

// instanceBillingChangePlanPreview handles GET /api/instances/:id/change-plan/preview.
func instanceBillingChangePlanPreview(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, ok := instanceBillingParseRouteID(c, c.Param("id"))
	if !ok {
		return
	}
	newPlanID, ok := instanceBillingParseRouteID(c, c.Query("newPlanId"))
	if !ok {
		return
	}

	instance, err := db.GetInstanceForBilling(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.Status == models.InstanceStatusDeleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已删除，无法改配", "code": "INSTANCE_DELETED"})
		return
	}
	if instance.PackagePlanID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "免费实例不支持升降级", "code": "FREE_INSTANCE"})
		return
	}

	newPlan, err := db.GetPackagePlanById(ctx, newPlanID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if newPlan == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "方案不存在", "code": "PLAN_NOT_FOUND"})
		return
	}
	if instance.PackageID == nil || newPlan.PackageID != *instance.PackageID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "新方案必须属于同一套餐", "code": "PLAN_NOT_IN_PACKAGE"})
		return
	}
	if *instance.PackagePlanID == newPlanID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能切换到相同方案", "code": "SAME_PLAN"})
		return
	}
	if !newPlan.IsActive {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该方案已下架", "code": "PLAN_INACTIVE"})
		return
	}
	if newPlan.IsSoldOut {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该方案已售罄", "code": "PLAN_SOLD_OUT"})
		return
	}

	statusCanChange := instance.Status == models.InstanceStatusRunning || instance.Status == models.InstanceStatusStopped

	if err := db.GpusAssertPlanChange(ctx, nil, instance.ID, newPlan.ID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	preview, err := instanceBillingCalculatePlanChange(ctx, instance, *newPlan)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !preview.IsUpgrade {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不支持降级，只能升级到更高配置的方案", "code": "DOWNGRADE_NOT_ALLOWED"})
		return
	}

	capacity, err := db.HostsCheckPlanUpgradeCapacity(ctx,
		db.HostsInstanceSpec{ID: instance.ID, HostID: instance.HostID, CPU: instance.CPU, Memory: instance.Memory, Disk: instance.Disk},
		db.HostsPlanSpec{CPU: newPlan.CPU, Memory: newPlan.Memory, Disk: newPlan.Disk},
		nil,
	)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var capacityMessage *string
	if !capacity.CanUpgrade {
		message := instanceBillingFormatUpgradeCapacityMessage(capacity.Reason)
		capacityMessage = &message
	}

	view := instanceBillingChangePlanPreviewView{
		OldPlan: instanceBillingPlanView{
			ID:           preview.OldPlan.ID,
			Name:         preview.OldPlan.Name,
			Price:        instanceBillingInstancePriceYuan(infoPricePointer(instance.BillingPrice), instanceBillingDecimalsToFloat(preview.OldPlan.Price.Decimal)),
			BillingCycle: preview.OldPlan.BillingCycle,
		},
		NewPlan: instanceBillingNewPlanView{
			ID:           preview.NewPlan.ID,
			Name:         preview.NewPlan.Name,
			Price:        instanceBillingPriceYuan(instanceBillingDecimalsToFloat(preview.NewPlan.Price.Decimal)),
			BillingCycle: preview.NewPlan.BillingCycle,
			IsActive:     preview.NewPlan.IsActive,
			IsSoldOut:    preview.NewPlan.IsSoldOut,
		},
		RemainingDays:    preview.RemainingDays,
		OldDailyPrice:    preview.OldDailyPrice,
		NewDailyPrice:    preview.NewDailyPrice,
		RemainingValue:   preview.RemainingValue,
		NewPlanCost:      preview.NewPlanCost,
		DiscountRate:     preview.DiscountRate,
		DiscountAmount:   preview.DiscountAmount,
		PriceDiff:        preview.PriceDiff,
		IsUpgrade:        preview.IsUpgrade,
		NewExpiresAt:     instanceBillingISOTime(preview.NewExpiresAt),
		NewConfig:        preview.NewConfig,
		ResourceCapacity: capacity,
		CanChange:        statusCanChange && preview.CanChange && capacity.CanUpgrade,
	}
	if capacityMessage != nil {
		view.ResourceWarnings = []string{*capacityMessage}
	}

	switch {
	case !statusCanChange:
		view.CannotChangeReason = instanceBillingStringPointer("instance_status_invalid")
	case preview.CannotReason != nil:
		view.CannotChangeReason = preview.CannotReason
	case !capacity.CanUpgrade:
		reason := "host_resources_insufficient"
		if capacity.Reason != nil {
			reason = *capacity.Reason
		}
		view.CannotChangeReason = &reason
	}

	c.JSON(http.StatusOK, gin.H{"preview": view})
}

// infoPricePointer converts the instance billing price to the *float64 shape the
// serialisers expect.
func infoPricePointer(value *models.Decimal) *float64 {
	if value == nil {
		return nil
	}
	converted := instanceBillingDecimalsToFloat(value.Decimal)
	return &converted
}

// instanceBillingChangePlan handles POST /api/instances/:id/change-plan.
func instanceBillingChangePlan(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object, validationMessage := instanceBillingBodyObject(body.Value)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	newPlanIDInput, validationMessage := instanceBillingValidatePositiveInteger(object, "newPlanId")
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	instanceID, ok := instanceBillingParseRouteID(c, c.Param("id"))
	if !ok {
		return
	}
	if newPlanIDInput == 0 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	newPlanID := newPlanIDInput

	instance, err := db.GetInstanceForBilling(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.Status == models.InstanceStatusDeleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已删除，无法改配", "code": "INSTANCE_DELETED"})
		return
	}
	if instance.PackagePlanID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "免费实例不支持升降级", "code": "FREE_INSTANCE"})
		return
	}

	newPlan, err := db.GetPackagePlanById(ctx, newPlanID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if newPlan == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "方案不存在", "code": "PLAN_NOT_FOUND"})
		return
	}
	if instance.PackageID == nil || newPlan.PackageID != *instance.PackageID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "新方案必须属于同一套餐", "code": "PLAN_NOT_IN_PACKAGE"})
		return
	}
	if *instance.PackagePlanID == newPlanID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能切换到相同方案", "code": "SAME_PLAN"})
		return
	}
	if !newPlan.IsActive {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该方案已下架", "code": "PLAN_INACTIVE"})
		return
	}
	if newPlan.IsSoldOut {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该方案已售罄", "code": "PLAN_SOLD_OUT"})
		return
	}
	if instance.Status != models.InstanceStatusRunning && instance.Status != models.InstanceStatusStopped {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "当前实例状态不允许升级，仅运行中或已停止的实例可以操作",
			"code":  "INSTANCE_STATUS_INVALID",
		})
		return
	}

	oldPlan, err := db.GetPackagePlanById(ctx, *instance.PackagePlanID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if oldPlan != nil {
		instancePrice := 0.0
		if instance.BillingPrice != nil {
			instancePrice = instanceBillingDecimalsToFloat(instance.BillingPrice.Decimal)
		}
		cycle := int32(1)
		if instance.BillingCycle != nil {
			cycle = *instance.BillingCycle
		}
		oldDailyPrice := lib.CalculateDailyPrice(instancePrice, int(cycle))
		newDailyPrice := lib.CalculateDailyPrice(instanceBillingDecimalsToFloat(newPlan.Price.Decimal)/100, int(newPlan.BillingCycle))
		if newDailyPrice <= oldDailyPrice {
			c.JSON(http.StatusBadRequest, gin.H{"error": "不支持降级，只能升级到更高配置的方案", "code": "DOWNGRADE_NOT_ALLOWED"})
			return
		}
	}

	outcome, err := instanceBillingPerformPlanChange(ctx, user.ID, instance, *newPlan)
	if err != nil {
		errorMessage := err.Error()
		if strings.Contains(errorMessage, "HOST_RESOURCES_INSUFFICIENT") {
			parts := strings.SplitN(errorMessage, ":", 2)
			reason := ""
			if len(parts) > 1 {
				reason = parts[1]
			}
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  instanceBillingFormatUpgradeCapacityMessage(&reason),
				"code":   "HOST_RESOURCES_INSUFFICIENT",
				"reason": reason,
			})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": errorMessage})
		return
	}

	// Push the new CPU/memory/disk to Incus. A failure is reported to the client
	// (needRestart + restartMessage) instead of failing the already-paid change,
	// exactly like the TypeScript.
	incusSyncSuccess := false
	incusSyncError := ""
	if host, hostErr := instanceBillingGetHost(ctx, instance.HostID); hostErr == nil && host != nil {
		if client, clientErr := lib.GetIncusClient(ctx, lib.IncusClientOptions{
			URL:           host.URL,
			CertPath:      host.CertPath,
			KeyPath:       host.KeyPath,
			TunnelEnabled: host.TunnelEnabled,
			HostID:        int64(host.ID),
			TargetHost:    host.TargetHost,
			TargetPort:    int(host.TargetPort),
		}); clientErr == nil {
			if patchErr := instanceBillingApplyIncusPlan(ctx, client, instance.IncusID, newPlan, outcome.BandwidthLimit); patchErr != nil {
				incusSyncError = patchErr.Error()
				log.Printf("[ChangePlan] 实例 %s Incus 配置同步失败: %v", instance.Name, patchErr)
			} else {
				incusSyncSuccess = true
				log.Printf("[ChangePlan] 实例 %s Incus 配置同步成功", instance.Name)
			}
		} else {
			incusSyncError = clientErr.Error()
		}
	}

	oldPlanName := "未知"
	if oldPlan != nil {
		oldPlanName = oldPlan.Name
	}
	lib.SendNotification(ctx, int64(user.ID), lib.EventInstancePlanChanged, lib.EventData{
		"instanceName": instance.Name,
		"oldPlanName":  oldPlanName,
		"newPlanName":  newPlan.Name,
		"priceDiff":    outcome.PriceDiff,
	})
	db.CreateLog(ctx, instanceBillingInt64Pointer(int64(user.ID)), instanceBillingLogModule, "instance.change_plan",
		fmt.Sprintf(`Changed plan for instance "%s": %s → %s, priceDiff: %s`,
			instance.Name, oldPlanName, newPlan.Name, instanceBillingTrimFloat(outcome.PriceDiff)),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))

	instanceType := "container"
	if db.DB != nil {
		var rows []struct {
			InstanceType string `gorm:"column:instance_type"`
		}
		if err := db.DB.WithContext(ctx).Raw("SELECT instance_type::text AS instance_type FROM packages WHERE id = ?", newPlan.PackageID).Scan(&rows).Error; err == nil && len(rows) > 0 {
			instanceType = rows[0].InstanceType
		}
	}
	isVM := instanceType == "vm"

	needRestart := isVM || !incusSyncSuccess
	var restartMessage *string
	if !incusSyncSuccess {
		if incusSyncError != "" {
			restartMessage = instanceBillingStringPointer("Incus配置同步失败: " + incusSyncError + "，请重启实例以应用新配置")
		} else {
			restartMessage = instanceBillingStringPointer("配置已更新，请重启实例以应用新配置")
		}
	} else if isVM {
		restartMessage = instanceBillingStringPointer("KVM_RESTART_REQUIRED")
	}

	// `refundAmount: result.refundAmount` is undefined on an upgrade in the
	// TypeScript, and JSON.stringify drops undefined keys — so the field must be
	// absent, not null.
	response := gin.H{
		"message":        "方案升级成功",
		"priceDiff":      outcome.PriceDiff,
		"newConfig":      outcome.NewConfig,
		"needRestart":    needRestart,
		"restartMessage": restartMessage,
	}
	if outcome.RefundAmount != nil {
		response["refundAmount"] = *outcome.RefundAmount
	}
	c.JSON(http.StatusOK, response)
}

// instanceBillingApplyIncusPlan mirrors the patchInstanceResources +
// restoreBandwidth pair the TypeScript runs after a plan change.
func instanceBillingApplyIncusPlan(ctx context.Context, client *lib.IncusClient, instanceName string, plan *models.PackagePlan, bandwidthLimit *string) error {
	cpuValue := int(plan.CPU)
	memoryValue := int(plan.Memory)
	diskValue := int(plan.Disk)
	if _, err := lib.PatchInstanceResources(ctx, client, instanceName, &cpuValue, &memoryValue, &diskValue); err != nil {
		return err
	}
	return instanceBillingRestoreBandwidth(ctx, client, instanceName, bandwidthLimit)
}

// instanceBillingRestoreBandwidth mirrors restoreBandwidth(client, name, limit,
// limit): every nic device gets limits.ingress / limits.egress set (or removed
// when the plan has no bandwidth limit), then the instance is updated.
func instanceBillingRestoreBandwidth(ctx context.Context, client *lib.IncusClient, instanceName string, bandwidthLimit *string) error {
	instance, err := lib.GetInstance(ctx, client, instanceName)
	if err != nil {
		return err
	}
	devices := instance.Devices
	if devices == nil {
		devices = map[string]map[string]any{}
	}
	for deviceName, device := range devices {
		if deviceType, _ := device["type"].(string); deviceType != "nic" {
			continue
		}
		updated := make(map[string]any, len(device)+2)
		for key, value := range device {
			updated[key] = value
		}
		if bandwidthLimit != nil {
			updated["limits.ingress"] = *bandwidthLimit
			updated["limits.egress"] = *bandwidthLimit
		} else {
			delete(updated, "limits.ingress")
			delete(updated, "limits.egress")
		}
		devices[deviceName] = updated
	}
	_, err = lib.UpdateInstance(ctx, client, instanceName, map[string]any{"devices": devices})
	return err
}

// instanceBillingUpdateAutoRenew handles PATCH /api/instances/:id/auto-renew.
func instanceBillingUpdateAutoRenew(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object, validationMessage := instanceBillingBodyObject(body.Value)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	autoRenew, validationMessage := instanceBillingValidateBoolean(object, "autoRenew")
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	instanceID, ok := instanceBillingParseRouteID(c, c.Param("id"))
	if !ok {
		return
	}

	instance, err := db.GetInstanceForBilling(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	if instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.PackagePlanID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "免费实例不支持自动续费", "code": "FREE_INSTANCE"})
		return
	}

	if err := db.UpdateInstanceAutoRenew(ctx, instanceID, autoRenew); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	action := "Disabled"
	message2 := "已关闭自动续费"
	if autoRenew {
		action = "Enabled"
		message2 = "已开启自动续费"
	}
	db.CreateLog(ctx, instanceBillingInt64Pointer(int64(user.ID)), instanceBillingLogModule, "instance.auto_renew",
		fmt.Sprintf(`%s auto-renew for instance "%s"`, action, instance.Name),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))

	c.JSON(http.StatusOK, gin.H{"message": message2, "autoRenew": autoRenew})
}

// instanceBillingBatchAutoRenew handles PATCH /api/instances/batch/auto-renew.
func instanceBillingBatchAutoRenew(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object, validationMessage := instanceBillingBodyObject(body.Value)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	uniqueIDs, validationMessage := instanceBillingValidateInstanceIDs(object)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	autoRenew, validationMessage := instanceBillingValidateBoolean(object, "autoRenew")
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	if len(uniqueIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择至少一个实例"})
		return
	}

	type batchAutoRenewResult struct {
		ID        int32   `json:"id"`
		Name      string  `json:"name"`
		Success   bool    `json:"success"`
		Skipped   *bool   `json:"skipped,omitempty"`
		Reason    *string `json:"reason,omitempty"`
		AutoRenew *bool   `json:"autoRenew,omitempty"`
	}
	results := make([]batchAutoRenewResult, 0, len(uniqueIDs))

	for _, instanceID := range uniqueIDs {
		instance, err := db.GetInstanceForBilling(ctx, instanceID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if instance == nil || instance.UserID != user.ID {
			reason := instanceBillingBatchHiddenReason
			results = append(results, batchAutoRenewResult{ID: instanceID, Name: fmt.Sprintf("#%d", instanceID), Success: false, Reason: &reason})
			continue
		}
		if instance.PackagePlanID == nil {
			reason := "免费实例不支持自动续费"
			results = append(results, batchAutoRenewResult{ID: instance.ID, Name: instance.Name, Success: false, Skipped: instanceBillingBoolPointer(true), Reason: &reason})
			continue
		}
		if instance.AutoRenew == autoRenew {
			reason := "已经关闭自动续费"
			if autoRenew {
				reason = "已经开启自动续费"
			}
			results = append(results, batchAutoRenewResult{ID: instance.ID, Name: instance.Name, Success: false, Skipped: instanceBillingBoolPointer(true), Reason: &reason})
			continue
		}
		if err := db.UpdateInstanceAutoRenew(ctx, instance.ID, autoRenew); err != nil {
			reason := "自动续费设置失败"
			results = append(results, batchAutoRenewResult{ID: instance.ID, Name: instance.Name, Success: false, Reason: &reason})
			continue
		}
		action := "Disabled"
		if autoRenew {
			action = "Enabled"
		}
		db.CreateLog(ctx, instanceBillingInt64Pointer(int64(user.ID)), instanceBillingLogModule, "instance.auto_renew",
			fmt.Sprintf(`%s auto-renew for instance "%s"`, action, instance.Name),
			db.LogResultSuccess, db.LogOptionsWithInstance(&instance.ID))
		value := autoRenew
		results = append(results, batchAutoRenewResult{ID: instance.ID, Name: instance.Name, Success: true, AutoRenew: &value})
	}

	successCount := 0
	skippedCount := 0
	for _, item := range results {
		if item.Success {
			successCount++
		}
		if item.Skipped != nil && *item.Skipped {
			skippedCount++
		}
	}
	failedCount := len(results) - successCount - skippedCount

	message2 := "批量关闭自动续费已处理"
	if autoRenew {
		message2 = "批量开启自动续费已处理"
	}
	c.JSON(http.StatusOK, gin.H{
		"message":      message2,
		"successCount": successCount,
		"skippedCount": skippedCount,
		"failedCount":  failedCount,
		"results":      results,
	})
}

// instanceBillingBillingRecords handles GET /api/instances/:id/billing/records.
func instanceBillingBillingRecords(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, ok := instanceBillingParseRouteID(c, c.Param("id"))
	if !ok {
		return
	}

	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}

	host, err := instanceBillingGetHost(ctx, instance.HostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	hostOwnerID := int32(0)
	if host != nil {
		hostOwnerID = host.UserID
	}
	isOwner, _, allowed := instanceBillingViewerAccess(instance.UserID, hostOwnerID, user.ID)
	if !allowed {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	page := lib.ParsePositiveIntegerQuery(c.Query("page"), 1, math.MaxInt32)
	pageSize := lib.ParsePositiveIntegerQuery(c.Query("pageSize"), 20, 100)

	options := db.InstanceBillingRecordListOptions{
		Page:     page,
		PageSize: pageSize,
		Type:     instanceBillingNormalizeRecordType(c.Query("type")),
	}
	if isOwner {
		options.UserID = user.ID
	}

	result, err := db.GetInstanceBillingRecords(ctx, instanceID, options)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	type recordView struct {
		ID          int32   `json:"id"`
		Type        string  `json:"type"`
		Amount      float64 `json:"amount"`
		Months      int32   `json:"months"`
		PeriodStart string  `json:"periodStart"`
		PeriodEnd   string  `json:"periodEnd"`
		Remark      *string `json:"remark"`
		CreatedAt   string  `json:"createdAt"`
	}
	records := make([]recordView, 0, len(result.Records))
	for _, record := range result.Records {
		records = append(records, recordView{
			ID:          record.ID,
			Type:        string(record.Type),
			Amount:      instanceBillingDecimalsToFloat(record.Amount.Decimal),
			Months:      record.Months,
			PeriodStart: instanceBillingISOTime(record.PeriodStart),
			PeriodEnd:   instanceBillingISOTime(record.PeriodEnd),
			Remark:      record.Remark,
			CreatedAt:   instanceBillingISOTime(record.CreatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"records":  records,
		"total":    result.Total,
		"page":     result.Page,
		"pageSize": result.PageSize,
	})
}

// ==================== route registration ====================

// RegisterInstanceBilling mounts the instance-billing routes under /api/instances,
// matching `fastify.register(instanceBillingRoutes, { prefix: '/api/instances' })`.
func RegisterInstanceBilling(r *gin.Engine) {
	group := r.Group("/api/instances")

	group.GET("/:id/billing", middleware.Authenticate(), instanceBillingGetBilling)
	group.POST("/:id/apply-aff", middleware.AuthenticateUser(), instanceBillingApplyAff)
	group.POST("/:id/renew", middleware.AuthenticateUser(), instanceBillingRenew)
	group.POST("/batch/renew-preview", middleware.AuthenticateUser(), instanceBillingBatchRenewPreview)
	group.POST("/batch/renew", middleware.AuthenticateUser(), instanceBillingBatchRenew)
	group.GET("/:id/change-plan/preview", middleware.AuthenticateUser(), instanceBillingChangePlanPreview)
	group.POST("/:id/change-plan", middleware.AuthenticateUser(), instanceBillingChangePlan)
	group.PATCH("/:id/auto-renew", middleware.AuthenticateUser(), instanceBillingUpdateAutoRenew)
	group.PATCH("/batch/auto-renew", middleware.AuthenticateUser(), instanceBillingBatchAutoRenew)
	group.GET("/:id/billing/records", middleware.Authenticate(), instanceBillingBillingRecords)
}

func init() {
	Register(RegisterInstanceBilling)
}
