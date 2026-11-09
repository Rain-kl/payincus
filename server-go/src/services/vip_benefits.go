// Port of server/src/services/vip-benefits.ts — the full module surface.
//
// Two independent halves live in the TypeScript file and both are reproduced here:
//
//  1. Continuous VIP benefits (config + level lookup + price arbitration + traffic
//     and resource-pool bonuses). This is the slice the resource-pool route also
//     needs; that route carries a private copy (`resourcePool*`) because it was
//     ported first, so this package is the canonical implementation going forward.
//  2. The VIP benefit reward hall: admin CRUD, the user overview, and the claim
//     state machine claimable|claimed|locked|blocked.
//
// Claim path money semantics (must not be weakened):
//   - The whole claim runs in ONE transaction that first takes
//     pg_advisory_xact_lock(USER_VIP_BENEFIT_CLAIM_LOCK_NAMESPACE=4115, userId), so a
//     reward can never be claimed twice concurrently (Prisma used
//     advisoryTransactionLock with the same namespace).
//   - Balance rewards additionally take pg_try_advisory_xact_lock(4110, userId) and
//     fail with BALANCE_CONFLICT when another balance mutation holds it.
//   - Points rewards take pg_advisory_xact_lock(4111, userId).
//   - Money columns stay Decimal (models.Decimal); JSON numbers use float64 exactly
//     where the TypeScript returns a number.
//
// Naming: this package is shared with parallel ports, so every package-level
// identifier is `Vip`-prefixed. Reusable helpers from vip_levels.go (vipRoundAmount,
// vipToNumber, vipJSRound, ...) are reused rather than duplicated.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// VipBenefitsConfigKey mirrors VIP_BENEFITS_CONFIG_KEY.
	VipBenefitsConfigKey = "vip_benefits_config"
	// VipMaxBenefitsPercent mirrors MAX_VIP_BENEFIT_PERCENT.
	VipMaxBenefitsPercent = 100
	// VipMaxContinuousBenefitLevel mirrors MAX_CONTINUOUS_VIP_LEVEL.
	VipMaxContinuousBenefitLevel = 5

	vipBenefitMaxMoneyAmount      = 99999999.99
	vipBenefitMaxPointsAmount     = 1000000000
	vipBenefitMaxClaimLimit       = 100
	vipBenefitMaxSortOrder        = 1000000
	vipBenefitMaxInstanceDays     = 3650
	vipBenefitMaxInstanceQuantity = 10
	vipBenefitMaxTitleLength      = 80
	vipBenefitMaxDescriptionLen   = 300
	vipBenefitMaxBulkClaims       = 1000
	vipBenefitMaxPackageID        = 2147483647
	vipBenefitMaxLabelLength      = 100
	vipBenefitMaxSafeInteger      = 9007199254740991
)

// VipContinuousBenefitLevel mirrors VipContinuousBenefitLevel.
type VipContinuousBenefitLevel struct {
	OrderDiscountPercent     float64 `json:"orderDiscountPercent"`
	ExtraTrafficPercent      float64 `json:"extraTrafficPercent"`
	ResourcePoolBonusPercent float64 `json:"resourcePoolBonusPercent"`
}

// VipContinuousBenefitsConfig mirrors VipContinuousBenefitsConfig.
type VipContinuousBenefitsConfig struct {
	Levels map[string]VipContinuousBenefitLevel `json:"levels"`
}

// VipDefaultBenefitsConfig mirrors DEFAULT_VIP_BENEFITS_CONFIG.
func VipDefaultBenefitsConfig() VipContinuousBenefitsConfig {
	return VipContinuousBenefitsConfig{Levels: map[string]VipContinuousBenefitLevel{
		"1": {OrderDiscountPercent: 1, ExtraTrafficPercent: 2, ResourcePoolBonusPercent: 2},
		"2": {OrderDiscountPercent: 2, ExtraTrafficPercent: 4, ResourcePoolBonusPercent: 4},
		"3": {OrderDiscountPercent: 3, ExtraTrafficPercent: 6, ResourcePoolBonusPercent: 6},
		"4": {OrderDiscountPercent: 4, ExtraTrafficPercent: 8, ResourcePoolBonusPercent: 8},
		"5": {OrderDiscountPercent: 5, ExtraTrafficPercent: 10, ResourcePoolBonusPercent: 10},
	}}
}

// VipDefaultBenefitsConfigJSON mirrors DEFAULT_VIP_BENEFITS_CONFIG_JSON.
var VipDefaultBenefitsConfigJSON = func() string {
	encoded, err := json.Marshal(VipDefaultBenefitsConfig())
	if err != nil {
		return `{}`
	}
	return string(encoded)
}()

// VipBenefitError mirrors VipBenefitError. StatusCode defaults to 400 in the
// TypeScript constructor; use NewVipBenefitError for the default.
type VipBenefitError struct {
	Code       string
	Message    string
	StatusCode int
}

// Error implements error.
func (e *VipBenefitError) Error() string { return e.Message }

// NewVipBenefitError builds an error with the TypeScript default status (400).
func NewVipBenefitError(code, message string) *VipBenefitError {
	return &VipBenefitError{Code: code, Message: message, StatusCode: 400}
}

func vipBenefitErrorStatus(code, message string, status int) *VipBenefitError {
	return &VipBenefitError{Code: code, Message: message, StatusCode: status}
}

// ==================== config ====================

// vipBenefitJSRecord mirrors getRecord.
func vipBenefitJSRecord(value any) map[string]any {
	if record, ok := value.(map[string]any); ok {
		return record
	}
	return map[string]any{}
}

// vipBenefitNumberLike reports whether value is a JS number (float64 or an
// integer Go type produced by tests/in-process callers).
func vipBenefitNumberLike(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	}
	return 0, false
}

// vipBenefitNormalizePercent mirrors normalizeBenefitPercent.
func vipBenefitNormalizePercent(value any, field string) (float64, error) {
	number, ok := vipBenefitNumberLike(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > VipMaxBenefitsPercent {
		return 0, NewVipBenefitError("INVALID_CONTINUOUS_BENEFIT_CONFIG",
			fmt.Sprintf("%s must be between 0 and %d", field, VipMaxBenefitsPercent))
	}
	return vipJSRound(number*100) / 100, nil
}

// VipNormalizeBenefitsConfig mirrors normalizeVipBenefitsConfig.
func VipNormalizeBenefitsConfig(value any) (VipContinuousBenefitsConfig, error) {
	input := vipBenefitJSRecord(value)
	levels := vipBenefitJSRecord(input["levels"])
	normalized := make(map[string]VipContinuousBenefitLevel, VipMaxContinuousBenefitLevel)

	for level := 1; level <= VipMaxContinuousBenefitLevel; level++ {
		levelConfig := vipBenefitJSRecord(levels[strconv.Itoa(level)])
		orderDiscount, err := vipBenefitNormalizePercent(levelConfig["orderDiscountPercent"], fmt.Sprintf("VIP%d order discount", level))
		if err != nil {
			return VipContinuousBenefitsConfig{}, err
		}
		extraTraffic, err := vipBenefitNormalizePercent(levelConfig["extraTrafficPercent"], fmt.Sprintf("VIP%d extra traffic", level))
		if err != nil {
			return VipContinuousBenefitsConfig{}, err
		}
		resourcePoolBonus, err := vipBenefitNormalizePercent(levelConfig["resourcePoolBonusPercent"], fmt.Sprintf("VIP%d resource pool bonus", level))
		if err != nil {
			return VipContinuousBenefitsConfig{}, err
		}
		normalized[strconv.Itoa(level)] = VipContinuousBenefitLevel{
			OrderDiscountPercent:     orderDiscount,
			ExtraTrafficPercent:      extraTraffic,
			ResourcePoolBonusPercent: resourcePoolBonus,
		}
	}
	return VipContinuousBenefitsConfig{Levels: normalized}, nil
}

// VipParseBenefitsConfigJSON mirrors parseVipBenefitsConfigJson.
func VipParseBenefitsConfigJSON(value *string) VipContinuousBenefitsConfig {
	if value == nil || *value == "" {
		return VipDefaultBenefitsConfig()
	}
	var decoded any
	if err := json.Unmarshal([]byte(*value), &decoded); err != nil {
		return VipDefaultBenefitsConfig()
	}
	normalized, err := VipNormalizeBenefitsConfig(decoded)
	if err != nil {
		return VipDefaultBenefitsConfig()
	}
	return normalized
}

// VipGetBenefitsConfig mirrors getVipBenefitsConfig.
func VipGetBenefitsConfig(ctx context.Context, client *gorm.DB) (VipContinuousBenefitsConfig, error) {
	if client == nil {
		client = db.DB
	}
	var raw *string
	var row models.SystemConfig
	err := client.WithContext(ctx).Select("value").Where("key = ?", VipBenefitsConfigKey).First(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		raw = nil
	case err != nil:
		return VipContinuousBenefitsConfig{}, err
	default:
		value := row.Value
		raw = &value
	}
	return VipParseBenefitsConfigJSON(raw), nil
}

// VipGetUserContinuousVipBenefit mirrors getUserContinuousVipBenefit.
func VipGetUserContinuousVipBenefit(ctx context.Context, userID int32, client *gorm.DB) (float64, VipContinuousBenefitLevel, error) {
	level, err := VipGetCurrentUserVipLevel(ctx, client, userID)
	if err != nil {
		return 0, VipContinuousBenefitLevel{}, err
	}
	config, err := VipGetBenefitsConfig(ctx, client)
	if err != nil {
		return 0, VipContinuousBenefitLevel{}, err
	}
	if level <= 0 {
		return level, VipContinuousBenefitLevel{}, nil
	}
	bounded := int(math.Min(level, VipMaxContinuousBenefitLevel))
	if benefit, ok := config.Levels[strconv.Itoa(bounded)]; ok {
		return level, benefit, nil
	}
	return level, VipContinuousBenefitLevel{}, nil
}

// VipPriceSource mirrors VipPriceSource.
type VipPriceSource string

const (
	VipPriceSourceBase VipPriceSource = "base"
	VipPriceSourceAff  VipPriceSource = "aff"
	VipPriceSourceVip  VipPriceSource = "vip"
)

// VipPriceArbitrationInput mirrors the arbitrateVipPrice argument.
type VipPriceArbitrationInput struct {
	BasePrice          float64
	AffDiscountRate    *float64
	VipDiscountPercent *float64
}

// VipPriceArbitration mirrors arbitrateVipPrice's return value.
type VipPriceArbitration struct {
	FinalPrice     float64
	DiscountAmount float64
	Source         VipPriceSource
}

// VipArbitratePrice mirrors arbitrateVipPrice: the cheapest of base/aff/vip wins,
// ties keep the earlier candidate (base first).
func VipArbitratePrice(input VipPriceArbitrationInput) VipPriceArbitration {
	basePrice := vipRoundAmount(input.BasePrice)

	affRate := 0.0
	if input.AffDiscountRate != nil {
		affRate = *input.AffDiscountRate
	}
	affRate = math.Max(0, math.Min(1, affRate))

	vipRate := 0.0
	if input.VipDiscountPercent != nil {
		vipRate = *input.VipDiscountPercent
	}
	vipRate = math.Max(0, math.Min(1, vipRate/100))

	candidates := []struct {
		price  float64
		source VipPriceSource
	}{
		{price: basePrice, source: VipPriceSourceBase},
		{price: vipRoundAmount(basePrice * (1 - affRate)), source: VipPriceSourceAff},
		{price: vipRoundAmount(basePrice * (1 - vipRate)), source: VipPriceSourceVip},
	}
	winner := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.price < winner.price {
			winner = candidate
		}
	}
	return VipPriceArbitration{
		FinalPrice:     winner.price,
		DiscountAmount: vipRoundAmount(basePrice - winner.price),
		Source:         winner.source,
	}
}

// VipCalculateExtraTraffic mirrors calculateVipExtraTraffic (bigint arithmetic).
func VipCalculateExtraTraffic(baseLimit *big.Int, percent float64) *big.Int {
	if baseLimit == nil || baseLimit.Sign() <= 0 || percent <= 0 {
		return big.NewInt(0)
	}
	bounded := math.Min(VipMaxBenefitsPercent, percent)
	basisPoints := big.NewInt(int64(vipJSRound(bounded * 100)))
	return new(big.Int).Quo(new(big.Int).Mul(baseLimit, basisPoints), big.NewInt(10000))
}

// VipCalculateResourcePoolCost mirrors calculateVipResourcePoolCost. The ceil
// division is exact decimal arithmetic.
func VipCalculateResourcePoolCost(amount int64, bonusPercent float64) (int64, error) {
	if amount <= 0 || amount > vipBenefitMaxSafeInteger {
		return 0, NewVipBenefitError("INVALID_RESOURCE_POOL_AMOUNT", "Resource pool amount must be a positive safe integer")
	}
	bounded := bonusPercent
	if math.IsNaN(bounded) {
		bounded = 0
	}
	bounded = math.Max(0, math.Min(VipMaxBenefitsPercent, bounded))

	numerator := decimal.NewFromInt(amount).Mul(decimal.NewFromInt(100))
	denominator := decimal.NewFromInt(100).Add(decimal.NewFromFloat(bounded))
	cost := numerator.Div(denominator).Ceil().IntPart()
	if cost < 1 {
		cost = 1
	}
	return cost, nil
}

// ==================== reward inputs / views ====================

// VipBenefitRewardType mirrors the Prisma enum values used inline in TS.
const (
	VipBenefitRewardTypeBalance  = "balance"
	VipBenefitRewardTypePoints   = "points"
	VipBenefitRewardTypeInstance = "instance"
)

// VipBenefitRewardState mirrors VipBenefitRewardState.
const (
	VipBenefitRewardStateClaimable = "claimable"
	VipBenefitRewardStateClaimed   = "claimed"
	VipBenefitRewardStateLocked    = "locked"
	VipBenefitRewardStateBlocked   = "blocked"
)

var vipBenefitRewardTypes = map[string]bool{
	VipBenefitRewardTypeBalance:  true,
	VipBenefitRewardTypePoints:   true,
	VipBenefitRewardTypeInstance: true,
}

// VipBenefitRewardInput mirrors VipBenefitRewardInput: every field is unknown at
// this layer, exactly like the TypeScript interface.
type VipBenefitRewardInput map[string]any

// VipBenefitRewardInputFrom mirrors `request.body || {}` plus isRecord coercion.
func VipBenefitRewardInputFrom(value any) VipBenefitRewardInput {
	if record, ok := value.(map[string]any); ok {
		return VipBenefitRewardInput(record)
	}
	return VipBenefitRewardInput{}
}

// vipBenefitRewardConfig mirrors VipBenefitRewardConfig.
type vipBenefitRewardConfig struct {
	Amount      *float64
	PackageID   *float64
	PackageName *string
	PlanID      *float64
	PlanName    *string
	Days        float64
	Quantity    float64
}

// toJSON mirrors the per-type config object the TypeScript returns. Keys are built
// explicitly because nulls must be emitted (`planName: null`) and zero amounts must
// not be omitted.
func (config vipBenefitRewardConfig) toJSON(rewardType string) map[string]any {
	switch rewardType {
	case VipBenefitRewardTypeBalance, VipBenefitRewardTypePoints:
		amount := 0.0
		if config.Amount != nil {
			amount = *config.Amount
		}
		return map[string]any{"amount": amount}
	default:
		return map[string]any{
			"packageId":   float64OrNil(config.PackageID),
			"packageName": stringOrNil(config.PackageName),
			"planId":      float64OrNil(config.PlanID),
			"planName":    stringOrNil(config.PlanName),
			"days":        config.Days,
			"quantity":    config.Quantity,
		}
	}
}

func float64OrNil(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func stringOrNil(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// VipBenefitClaimView mirrors VipBenefitClaimView.
type VipBenefitClaimView struct {
	ID          int32          `json:"id"`
	RewardID    int32          `json:"rewardId"`
	Level       int32          `json:"level"`
	Status      string         `json:"status"`
	ClaimNo     int32          `json:"claimNo"`
	Snapshot    map[string]any `json:"snapshot"`
	DeliveredAt *string        `json:"deliveredAt"`
	CreatedAt   string         `json:"createdAt"`
}

// VipBenefitRewardView mirrors VipBenefitRewardView.
type VipBenefitRewardView struct {
	ID              int32                 `json:"id"`
	Level           int32                 `json:"level"`
	Type            string                `json:"type"`
	Title           string                `json:"title"`
	Description     *string               `json:"description"`
	ClaimLimit      int32                 `json:"claimLimit"`
	SortOrder       int32                 `json:"sortOrder"`
	Enabled         bool                  `json:"enabled"`
	Config          map[string]any        `json:"config"`
	CreatedAt       string                `json:"createdAt"`
	UpdatedAt       string                `json:"updatedAt"`
	ClaimedCount    int                   `json:"claimedCount"`
	RemainingClaims int                   `json:"remainingClaims"`
	State           string                `json:"state"`
	BlockedByLevel  *int32                `json:"blockedByLevel"`
	Claims          []VipBenefitClaimView `json:"claims"`
}

// VipBenefitAmountSummary mirrors VipBenefitAmountSummary.
type VipBenefitAmountSummary struct {
	BalanceAmount    float64 `json:"balanceAmount"`
	PointsAmount     float64 `json:"pointsAmount"`
	InstanceQuantity float64 `json:"instanceQuantity"`
}

// VipBenefitSummary mirrors the overview's summary block.
type VipBenefitSummary struct {
	TotalRewards     int                     `json:"totalRewards"`
	UnlockedRewards  int                     `json:"unlockedRewards"`
	ClaimableRewards int                     `json:"claimableRewards"`
	ClaimedRewards   int                     `json:"claimedRewards"`
	LockedRewards    int                     `json:"lockedRewards"`
	BlockedRewards   int                     `json:"blockedRewards"`
	PendingRewards   int                     `json:"pendingRewards"`
	Entitlement      VipBenefitAmountSummary `json:"entitlement"`
	Remaining        VipBenefitAmountSummary `json:"remaining"`
}

// VipBenefitOverview mirrors VipBenefitOverview.
type VipBenefitOverview struct {
	CurrentLevel      float64                `json:"currentLevel"`
	UserVipMetric     UserVipMetric          `json:"userVipMetric"`
	UserStats         UserVipStats           `json:"userStats"`
	UserVipBadgeStyle *VipBadgeStyle         `json:"userVipBadgeStyle"`
	Rewards           []VipBenefitRewardView `json:"rewards"`
	Summary           VipBenefitSummary      `json:"summary"`
}

// VipBenefitClaimResult mirrors claimVipBenefitReward's return shape.
type VipBenefitClaimResult struct {
	Claim    VipBenefitClaimView `json:"claim"`
	Overview VipBenefitOverview  `json:"overview"`
}

// VipBenefitBulkClaimResult mirrors claimAllAvailableVipBenefitRewards' return shape.
type VipBenefitBulkClaimResult struct {
	Claims   []VipBenefitClaimView `json:"claims"`
	Overview VipBenefitOverview    `json:"overview"`
}

// ==================== primitive normalizers ====================

// vipBenefitISOTime mirrors Date.prototype.toISOString().
func vipBenefitISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// vipBenefitJSStringLength mirrors String(value).length (UTF-16 code units).
func vipBenefitJSStringLength(value string) int {
	units := 0
	for _, r := range value {
		if r > 0xFFFF {
			units += 2
			continue
		}
		units++
	}
	return units
}

// vipBenefitJSSlice mirrors String.prototype.slice(0, max) at UTF-16 boundaries.
func vipBenefitJSSlice(value string, max int) string {
	if max <= 0 {
		return ""
	}
	units := 0
	for index, r := range value {
		width := 1
		if r > 0xFFFF {
			width = 2
		}
		if units+width > max {
			return value[:index]
		}
		units += width
	}
	return value
}

// vipBenefitSafeInteger mirrors Number.isSafeInteger for a float64.
func vipBenefitSafeInteger(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value == math.Trunc(value) && math.Abs(value) <= vipBenefitMaxSafeInteger
}

// vipBenefitRequireSafeInteger mirrors requireSafeInteger.
func vipBenefitRequireSafeInteger(value any, field string) (float64, error) {
	number, ok := vipBenefitNumberLike(value)
	if !ok || !vipBenefitSafeInteger(number) {
		return 0, NewVipBenefitError("INVALID_NUMBER", field+" must be a safe integer")
	}
	return number, nil
}

// vipBenefitRequireFinite mirrors requireFiniteNumber.
func vipBenefitRequireFinite(value any, field string) (float64, error) {
	number, ok := vipBenefitNumberLike(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, NewVipBenefitError("INVALID_NUMBER", field+" must be a finite number")
	}
	return number, nil
}

// vipBenefitNormalizeLevel mirrors normalizeLevel.
func vipBenefitNormalizeLevel(value any) (int32, error) {
	level, err := vipBenefitRequireSafeInteger(value, "VIP level")
	if err != nil {
		return 0, err
	}
	if level < 1 || level > MaxVipLevel {
		return 0, NewVipBenefitError("INVALID_LEVEL", fmt.Sprintf("VIP level must be between 1 and %d", MaxVipLevel))
	}
	return int32(level), nil
}

// vipBenefitNormalizeRewardType mirrors normalizeRewardType.
func vipBenefitNormalizeRewardType(value any) (string, error) {
	if text, ok := value.(string); ok && vipBenefitRewardTypes[text] {
		return text, nil
	}
	return "", NewVipBenefitError("INVALID_REWARD_TYPE", "Invalid VIP benefit reward type")
}

// vipBenefitNormalizeTitle mirrors normalizeTitle.
func vipBenefitNormalizeTitle(value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", NewVipBenefitError("INVALID_TITLE", "Reward title is required")
	}
	title := strings.TrimSpace(text)
	if title == "" {
		return "", NewVipBenefitError("INVALID_TITLE", "Reward title is required")
	}
	if vipBenefitJSStringLength(title) > vipBenefitMaxTitleLength {
		return "", NewVipBenefitError("INVALID_TITLE", fmt.Sprintf("Reward title cannot exceed %d characters", vipBenefitMaxTitleLength))
	}
	return title, nil
}

// vipBenefitNormalizeDescription mirrors normalizeDescription: a nullish or
// non-string value clears the description, and the result is truncated.
func vipBenefitNormalizeDescription(value any) *string {
	if value == nil {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return nil
	}
	description := strings.TrimSpace(text)
	if description == "" {
		return nil
	}
	truncated := vipBenefitJSSlice(description, vipBenefitMaxDescriptionLen)
	return &truncated
}

// vipBenefitNormalizePositiveInteger mirrors normalizePositiveInteger.
func vipBenefitNormalizePositiveInteger(value any, field string, max int) (int32, error) {
	parsed, err := vipBenefitRequireSafeInteger(value, field)
	if err != nil {
		return 0, err
	}
	if parsed <= 0 || parsed > float64(max) {
		return 0, NewVipBenefitError("INVALID_NUMBER", fmt.Sprintf("%s must be an integer between 1 and %d", field, max))
	}
	return int32(parsed), nil
}

// vipBenefitNormalizeOptionalPositiveInteger mirrors
// normalizeOptionalPositiveInteger.
func vipBenefitNormalizeOptionalPositiveInteger(value any, fallback int, field string, max int) (int32, error) {
	if value == nil {
		return int32(fallback), nil
	}
	if text, ok := value.(string); ok && text == "" {
		return int32(fallback), nil
	}
	return vipBenefitNormalizePositiveInteger(value, field, max)
}

// vipBenefitNormalizeMoney mirrors normalizeMoney.
func vipBenefitNormalizeMoney(value any, field string) (float64, error) {
	parsed, err := vipBenefitRequireFinite(value, field)
	if err != nil {
		return 0, err
	}
	if parsed <= 0 || parsed > vipBenefitMaxMoneyAmount {
		return 0, NewVipBenefitError("INVALID_AMOUNT",
			fmt.Sprintf("%s must be between 0.01 and %s", field, strconv.FormatFloat(vipBenefitMaxMoneyAmount, 'f', -1, 64)))
	}
	return vipRoundAmount(parsed), nil
}

// vipBenefitNormalizeSortOrder mirrors normalizeSortOrder.
func vipBenefitNormalizeSortOrder(value any) (int32, error) {
	if value == nil {
		return 0, nil
	}
	if text, ok := value.(string); ok && text == "" {
		return 0, nil
	}
	parsed, err := vipBenefitRequireSafeInteger(value, "Sort order")
	if err != nil {
		return 0, err
	}
	if parsed < 0 || parsed > vipBenefitMaxSortOrder {
		return 0, NewVipBenefitError("INVALID_SORT_ORDER", fmt.Sprintf("Sort order must be an integer between 0 and %d", vipBenefitMaxSortOrder))
	}
	return int32(parsed), nil
}

// vipBenefitNormalizeLabel mirrors normalizeLabel.
func vipBenefitNormalizeLabel(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	truncated := vipBenefitJSSlice(trimmed, vipBenefitMaxLabelLength)
	return &truncated
}

// vipBenefitNormalizeRewardConfig mirrors normalizeRewardConfig (strict input).
func vipBenefitNormalizeRewardConfig(rewardType string, value any) (vipBenefitRewardConfig, error) {
	config := vipBenefitJSRecord(value)
	if rewardType == VipBenefitRewardTypeBalance {
		amount, err := vipBenefitNormalizeMoney(config["amount"], "Balance reward amount")
		if err != nil {
			return vipBenefitRewardConfig{}, err
		}
		return vipBenefitRewardConfig{Amount: &amount}, nil
	}
	if rewardType == VipBenefitRewardTypePoints {
		amount, err := vipBenefitNormalizePositiveInteger(config["amount"], "Points reward amount", vipBenefitMaxPointsAmount)
		if err != nil {
			return vipBenefitRewardConfig{}, err
		}
		asFloat := float64(amount)
		return vipBenefitRewardConfig{Amount: &asFloat}, nil
	}

	packageID, err := vipBenefitNormalizePositiveInteger(config["packageId"], "Package ID", vipBenefitMaxPackageID)
	if err != nil {
		return vipBenefitRewardConfig{}, err
	}
	planID, err := vipBenefitNormalizePositiveInteger(config["planId"], "Plan ID", vipBenefitMaxPackageID)
	if err != nil {
		return vipBenefitRewardConfig{}, err
	}
	days, err := vipBenefitNormalizeOptionalPositiveInteger(config["days"], 30, "Instance reward days", vipBenefitMaxInstanceDays)
	if err != nil {
		return vipBenefitRewardConfig{}, err
	}
	quantity, err := vipBenefitNormalizeOptionalPositiveInteger(config["quantity"], 1, "Instance reward quantity", vipBenefitMaxInstanceQuantity)
	if err != nil {
		return vipBenefitRewardConfig{}, err
	}

	packageIDFloat := float64(packageID)
	planIDFloat := float64(planID)
	return vipBenefitRewardConfig{
		PackageID:   &packageIDFloat,
		PackageName: vipBenefitNormalizeLabel(config["packageName"]),
		PlanID:      &planIDFloat,
		PlanName:    vipBenefitNormalizeLabel(config["planName"]),
		Days:        float64(days),
		Quantity:    float64(quantity),
	}, nil
}

// vipBenefitNormalizeStoredRewardConfig mirrors normalizeStoredRewardConfig: stored
// rows are coerced, never rejected.
func vipBenefitNormalizeStoredRewardConfig(rewardType string, value any) vipBenefitRewardConfig {
	config := vipBenefitJSRecord(value)
	if rewardType == VipBenefitRewardTypeBalance {
		amount := vipRoundAmount(math.Max(0, vipToNumber(config["amount"])))
		return vipBenefitRewardConfig{Amount: &amount}
	}
	if rewardType == VipBenefitRewardTypePoints {
		amount := math.Max(0, math.Floor(vipToNumber(config["amount"])))
		return vipBenefitRewardConfig{Amount: &amount}
	}

	integerOrNil := func(value any) *float64 {
		number := vipToNumber(value)
		if !vipIsInteger(number) {
			return nil
		}
		return &number
	}
	days := math.Max(1, math.Min(vipBenefitMaxInstanceDays, math.Floor(vipToNumberOrZero(config["days"], 30))))
	quantity := math.Max(1, math.Min(vipBenefitMaxInstanceQuantity, math.Floor(vipToNumberOrZero(config["quantity"], 1))))

	return vipBenefitRewardConfig{
		PackageID:   integerOrNil(config["packageId"]),
		PackageName: vipBenefitNormalizeLabel(config["packageName"]),
		PlanID:      integerOrNil(config["planId"]),
		PlanName:    vipBenefitNormalizeLabel(config["planName"]),
		Days:        days,
		Quantity:    quantity,
	}
}

// vipToNumberOrZero mirrors `toNumber(value) || fallback` for the days/quantity
// clamp (0 is falsy in JS, so it falls back too).
func vipToNumberOrZero(value any, fallback float64) float64 {
	number := vipToNumber(value)
	if number == 0 {
		return fallback
	}
	return number
}

// vipBenefitValidateInstanceConfig mirrors validateInstanceConfig.
func vipBenefitValidateInstanceConfig(ctx context.Context, client *gorm.DB, config vipBenefitRewardConfig) (vipBenefitRewardConfig, error) {
	if config.PackageID == nil || config.PlanID == nil || *config.PackageID == 0 || *config.PlanID == 0 {
		return vipBenefitRewardConfig{}, NewVipBenefitError("INVALID_INSTANCE_REWARD", "Instance reward requires a package and plan")
	}
	packageID := int32(*config.PackageID)
	planID := int32(*config.PlanID)

	if client == nil {
		client = db.DB
	}

	// Raw SQL instead of model queries: models.Package / models.PackagePlan carry
	// GPUDeviceIds []int32, which GORM's schema parser rejects ("unsupported data
	// type: &[]"), so a First() on those structs fails before any SQL is built.
	var pkg struct {
		ID   int32  `gorm:"column:id"`
		Name string `gorm:"column:name"`
	}
	if err := client.WithContext(ctx).Raw(
		`SELECT id, name FROM packages WHERE id = ? AND active = ? LIMIT 1`, packageID, true,
	).Scan(&pkg).Error; err != nil {
		return vipBenefitRewardConfig{}, err
	}
	if pkg.ID == 0 {
		return vipBenefitRewardConfig{}, NewVipBenefitError("INVALID_INSTANCE_REWARD", "Selected package does not exist or is disabled")
	}

	var plan struct {
		ID   int32  `gorm:"column:id"`
		Name string `gorm:"column:name"`
	}
	if err := client.WithContext(ctx).Raw(
		`SELECT id, name FROM package_plans WHERE id = ? AND package_id = ? AND is_active = ? LIMIT 1`,
		planID, packageID, true,
	).Scan(&plan).Error; err != nil {
		return vipBenefitRewardConfig{}, err
	}
	if plan.ID == 0 {
		return vipBenefitRewardConfig{}, NewVipBenefitError("INVALID_INSTANCE_REWARD", "Selected plan does not exist, is disabled, or does not belong to the package")
	}

	if config.PackageName == nil || *config.PackageName == "" {
		name := pkg.Name
		config.PackageName = &name
	}
	if config.PlanName == nil || *config.PlanName == "" {
		name := plan.Name
		config.PlanName = &name
	}
	return config, nil
}

// vipBenefitNormalizedReward is the row shape normalizeRewardData produces.
type vipBenefitNormalizedReward struct {
	Level       int32
	Type        string
	Title       string
	Description *string
	ClaimLimit  int32
	SortOrder   int32
	Enabled     bool
	Config      vipBenefitRewardConfig
}

// vipBenefitCoalesce mirrors `input[key] ?? fallback` (nullish, not falsy).
func vipBenefitCoalesce(input VipBenefitRewardInput, key string, fallback any) any {
	if value, present := input[key]; present && value != nil {
		return value
	}
	return fallback
}

// vipBenefitBaseField reads a field of the existing row in `?? base?.field` form.
func vipBenefitBaseField(base *models.VipBenefitReward, field string) any {
	if base == nil {
		return nil
	}
	switch field {
	case "level":
		return float64(base.Level)
	case "type":
		return string(base.Type)
	case "title":
		return base.Title
	case "description":
		if base.Description == nil {
			return nil
		}
		return *base.Description
	case "claimLimit":
		return float64(base.ClaimLimit)
	case "sortOrder":
		return float64(base.SortOrder)
	case "enabled":
		return base.Enabled
	case "config":
		return vipBenefitJSONValue(base.Config)
	}
	return nil
}

// vipBenefitJSONValue decodes a JSON column into a generic value.
func vipBenefitJSONValue(data datatypes.JSON) any {
	if len(data) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil
	}
	return decoded
}

// vipBenefitNormalizeRewardData mirrors normalizeRewardData.
func vipBenefitNormalizeRewardData(ctx context.Context, client *gorm.DB, input VipBenefitRewardInput, base *models.VipBenefitReward) (vipBenefitNormalizedReward, error) {
	rewardType, err := vipBenefitNormalizeRewardType(vipBenefitCoalesce(input, "type", vipBenefitBaseField(base, "type")))
	if err != nil {
		return vipBenefitNormalizedReward{}, err
	}

	rawConfig := any(nil)
	if value, present := input["config"]; present {
		rawConfig = value
	} else {
		rawConfig = vipBenefitBaseField(base, "config")
	}
	config, err := vipBenefitNormalizeRewardConfig(rewardType, rawConfig)
	if err != nil {
		return vipBenefitNormalizedReward{}, err
	}
	if rewardType == VipBenefitRewardTypeInstance {
		config, err = vipBenefitValidateInstanceConfig(ctx, client, config)
		if err != nil {
			return vipBenefitNormalizedReward{}, err
		}
	}

	level, err := vipBenefitNormalizeLevel(vipBenefitCoalesce(input, "level", vipBenefitBaseField(base, "level")))
	if err != nil {
		return vipBenefitNormalizedReward{}, err
	}
	title, err := vipBenefitNormalizeTitle(vipBenefitCoalesce(input, "title", vipBenefitBaseField(base, "title")))
	if err != nil {
		return vipBenefitNormalizedReward{}, err
	}
	claimLimit, err := vipBenefitNormalizeOptionalPositiveInteger(
		vipBenefitCoalesce(input, "claimLimit", vipBenefitBaseField(base, "claimLimit")),
		1, "Claim limit", vipBenefitMaxClaimLimit,
	)
	if err != nil {
		return vipBenefitNormalizedReward{}, err
	}

	sortOrderFallback := vipBenefitBaseField(base, "sortOrder")
	if sortOrderFallback == nil {
		sortOrderFallback = float64(0)
	}
	sortOrder, err := vipBenefitNormalizeSortOrder(vipBenefitCoalesce(input, "sortOrder", sortOrderFallback))
	if err != nil {
		return vipBenefitNormalizedReward{}, err
	}

	enabled := true
	if raw, present := input["enabled"]; present {
		enabled = raw == true
	} else if base != nil {
		enabled = base.Enabled
	}

	return vipBenefitNormalizedReward{
		Level:       level,
		Type:        rewardType,
		Title:       title,
		Description: vipBenefitNormalizeDescription(vipBenefitCoalesce(input, "description", vipBenefitBaseField(base, "description"))),
		ClaimLimit:  claimLimit,
		SortOrder:   sortOrder,
		Enabled:     enabled,
		Config:      config,
	}, nil
}

// ==================== view mapping ====================

// vipBenefitClaimToView mirrors claimToView.
func vipBenefitClaimToView(claim models.VipBenefitClaim) VipBenefitClaimView {
	view := VipBenefitClaimView{
		ID:        claim.ID,
		RewardID:  claim.RewardID,
		Level:     claim.Level,
		Status:    string(claim.Status),
		ClaimNo:   claim.ClaimNo,
		Snapshot:  vipBenefitJSRecord(vipBenefitJSONValue(claim.Snapshot)),
		CreatedAt: vipBenefitISOTime(claim.CreatedAt),
	}
	if claim.DeliveredAt != nil {
		formatted := vipBenefitISOTime(*claim.DeliveredAt)
		view.DeliveredAt = &formatted
	}
	return view
}

// vipBenefitRewardToAdminView mirrors rewardToAdminView.
func vipBenefitRewardToAdminView(reward models.VipBenefitReward) VipBenefitRewardView {
	rewardType := string(reward.Type)
	return VipBenefitRewardView{
		ID:              reward.ID,
		Level:           reward.Level,
		Type:            rewardType,
		Title:           reward.Title,
		Description:     reward.Description,
		ClaimLimit:      reward.ClaimLimit,
		SortOrder:       reward.SortOrder,
		Enabled:         reward.Enabled,
		Config:          vipBenefitNormalizeStoredRewardConfig(rewardType, vipBenefitJSONValue(reward.Config)).toJSON(rewardType),
		CreatedAt:       vipBenefitISOTime(reward.CreatedAt),
		UpdatedAt:       vipBenefitISOTime(reward.UpdatedAt),
		ClaimedCount:    0,
		RemainingClaims: int(reward.ClaimLimit),
		State:           VipBenefitRewardStateLocked,
		BlockedByLevel:  nil,
		Claims:          []VipBenefitClaimView{},
	}
}

// vipBenefitAddRewardAmount mirrors addRewardAmount.
func vipBenefitAddRewardAmount(summary *VipBenefitAmountSummary, reward VipBenefitRewardView, multiplier int) {
	if multiplier <= 0 {
		return
	}
	amount := vipToNumber(reward.Config["amount"])
	switch reward.Type {
	case VipBenefitRewardTypeBalance:
		summary.BalanceAmount = vipRoundAmount(summary.BalanceAmount + amount*float64(multiplier))
	case VipBenefitRewardTypePoints:
		summary.PointsAmount += math.Floor(amount) * float64(multiplier)
	case VipBenefitRewardTypeInstance:
		quantity := vipToNumber(reward.Config["quantity"])
		if quantity == 0 {
			quantity = 1
		}
		summary.InstanceQuantity += math.Max(1, math.Floor(quantity)) * float64(multiplier)
	}
}

// ==================== current user VIP ====================

// vipBenefitCurrentUserVip mirrors getCurrentUserVip's return shape.
type vipBenefitCurrentUserVip struct {
	Level      float64
	Metric     UserVipMetric
	Stats      UserVipStats
	BadgeStyle *VipBadgeStyle
}

// vipBenefitGetVipRules mirrors getVipRules against an arbitrary client.
func vipBenefitGetVipRules(ctx context.Context, client *gorm.DB, ruleType VipRuleType) ([]NormalizedVipRule, error) {
	var rows []models.VipLevelRule
	if err := client.WithContext(ctx).
		Where("type = ?", string(ruleType)).
		Order("level ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	rules := make([]NormalizedVipRule, 0, len(rows))
	for _, row := range rows {
		rules = append(rules, NormalizeVipRule(row))
	}
	return rules, nil
}

// vipBenefitGetUserVipStats mirrors getUserVipStats against an arbitrary client.
func vipBenefitGetUserVipStats(ctx context.Context, client *gorm.DB, userID int32) (UserVipStats, error) {
	consumeMap, err := db.GetUsersTotalConsumeMap(ctx, []int32{userID}, client)
	if err != nil {
		return UserVipStats{}, err
	}
	var rechargeRow struct {
		Value models.Decimal `gorm:"column:value"`
	}
	if err := client.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(COALESCE(actual_amount, amount)), 0)::numeric AS value
		 FROM recharge_records
		 WHERE user_id = ?
		   AND status = 'completed'`, userID,
	).Scan(&rechargeRow).Error; err != nil {
		return UserVipStats{}, err
	}
	return UserVipStats{
		TotalConsume:  consumeMap[userID],
		TotalRecharge: vipToNumber(rechargeRow.Value),
	}, nil
}

// vipBenefitGetUserVipMetric mirrors getUserVipMetric. The TypeScript helper takes no
// client: even inside the claim transaction it resolves `user_vip_metric` through the
// module-level prisma client and therefore through the system-config cache. The cached
// read is reproduced here (a raw tx query would bypass it and could observe a value
// the Node backend still has cached).
func vipBenefitGetUserVipMetric(ctx context.Context) (UserVipMetric, error) {
	value, err := db.GetSystemConfig(ctx, userVipMetricConfigKey, true)
	if err != nil {
		return "", err
	}
	return vipNormalizeUserVipMetric(value), nil
}

// VipGetCurrentUserVip mirrors getCurrentUserVip. A nil client uses the shared pool;
// pass a transaction to keep every read inside the claim transaction.
func VipGetCurrentUserVip(ctx context.Context, client *gorm.DB, userID int32) (vipBenefitCurrentUserVip, error) {
	if client == nil {
		client = db.DB
	}
	metric, err := vipBenefitGetUserVipMetric(ctx)
	if err != nil {
		return vipBenefitCurrentUserVip{}, err
	}
	rules, err := vipBenefitGetVipRules(ctx, client, VipRuleTypeUser)
	if err != nil {
		return vipBenefitCurrentUserVip{}, err
	}
	stats, err := vipBenefitGetUserVipStats(ctx, client, userID)
	if err != nil {
		return vipBenefitCurrentUserVip{}, err
	}
	level := CalculateUserVipLevelByMetric(rules, stats, metric)
	return vipBenefitCurrentUserVip{
		Level:      level,
		Metric:     metric,
		Stats:      stats,
		BadgeStyle: GetVipBadgeStyleForLevel(rules, VipRuleTypeUser, level),
	}, nil
}

// VipGetCurrentUserVipLevel is the level-only projection used by continuous
// benefits (getUserContinuousVipBenefit calls getCurrentUserVip then reads level).
func VipGetCurrentUserVipLevel(ctx context.Context, client *gorm.DB, userID int32) (float64, error) {
	vip, err := VipGetCurrentUserVip(ctx, client, userID)
	if err != nil {
		return 0, err
	}
	return vip.Level, nil
}

// ==================== admin CRUD ====================

// VipListAdminBenefitRewards mirrors listAdminVipBenefitRewards.
func VipListAdminBenefitRewards(ctx context.Context) ([]VipBenefitRewardView, error) {
	var rewards []models.VipBenefitReward
	if err := db.DB.WithContext(ctx).
		Order("level ASC, sort_order ASC, id ASC").
		Find(&rewards).Error; err != nil {
		return nil, err
	}
	views := make([]VipBenefitRewardView, 0, len(rewards))
	for _, reward := range rewards {
		views = append(views, vipBenefitRewardToAdminView(reward))
	}
	return views, nil
}

// VipCreateBenefitReward mirrors createVipBenefitReward.
func VipCreateBenefitReward(ctx context.Context, input VipBenefitRewardInput) (VipBenefitRewardView, error) {
	data, err := vipBenefitNormalizeRewardData(ctx, nil, input, nil)
	if err != nil {
		return VipBenefitRewardView{}, err
	}
	configJSON, err := json.Marshal(data.Config.toJSON(data.Type))
	if err != nil {
		return VipBenefitRewardView{}, err
	}
	now := time.Now().UTC()
	row := models.VipBenefitReward{
		Level:       data.Level,
		Type:        models.VipBenefitRewardType(data.Type),
		Title:       data.Title,
		Description: data.Description,
		ClaimLimit:  data.ClaimLimit,
		SortOrder:   data.SortOrder,
		Enabled:     data.Enabled,
		Config:      datatypes.JSON(configJSON),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := db.DB.WithContext(ctx).Clauses(clause.Returning{}).Create(&row).Error; err != nil {
		return VipBenefitRewardView{}, err
	}
	return vipBenefitRewardToAdminView(row), nil
}

// VipUpdateBenefitReward mirrors updateVipBenefitReward.
func VipUpdateBenefitReward(ctx context.Context, id int32, input VipBenefitRewardInput) (VipBenefitRewardView, error) {
	var existing models.VipBenefitReward
	err := db.DB.WithContext(ctx).Where("id = ?", id).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return VipBenefitRewardView{}, vipBenefitErrorStatus("REWARD_NOT_FOUND", "VIP benefit reward not found", 404)
	}
	if err != nil {
		return VipBenefitRewardView{}, err
	}

	data, err := vipBenefitNormalizeRewardData(ctx, nil, input, &existing)
	if err != nil {
		return VipBenefitRewardView{}, err
	}
	configJSON, err := json.Marshal(data.Config.toJSON(data.Type))
	if err != nil {
		return VipBenefitRewardView{}, err
	}
	updates := map[string]any{
		"level":       data.Level,
		"type":        models.VipBenefitRewardType(data.Type),
		"title":       data.Title,
		"description": data.Description,
		"claim_limit": data.ClaimLimit,
		"sort_order":  data.SortOrder,
		"enabled":     data.Enabled,
		"config":      datatypes.JSON(configJSON),
		"updated_at":  time.Now().UTC(),
	}
	result := db.DB.WithContext(ctx).Model(&models.VipBenefitReward{}).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		return VipBenefitRewardView{}, result.Error
	}
	if result.RowsAffected == 0 {
		return VipBenefitRewardView{}, vipBenefitErrorStatus("REWARD_NOT_FOUND", "VIP benefit reward not found", 404)
	}

	var row models.VipBenefitReward
	if err := db.DB.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		return VipBenefitRewardView{}, err
	}
	return vipBenefitRewardToAdminView(row), nil
}

// VipDeleteBenefitReward mirrors deleteVipBenefitReward: soft-disable, 404 when the
// row does not exist (Prisma P2025 on update).
func VipDeleteBenefitReward(ctx context.Context, id int32) error {
	tag := db.DB.WithContext(ctx).Model(&models.VipBenefitReward{}).
		Where("id = ?", id).Update("enabled", false)
	if tag.Error != nil {
		return tag.Error
	}
	if tag.RowsAffected == 0 {
		return vipBenefitErrorStatus("REWARD_NOT_FOUND", "VIP benefit reward not found", 404)
	}
	return nil
}

// ==================== overview ====================

// VipGetBenefitOverview mirrors getVipBenefitOverview.
func VipGetBenefitOverview(ctx context.Context, userID int32) (VipBenefitOverview, error) {
	vip, err := VipGetCurrentUserVip(ctx, nil, userID)
	if err != nil {
		return VipBenefitOverview{}, err
	}

	var rewards []models.VipBenefitReward
	if err := db.DB.WithContext(ctx).Where("enabled = ?", true).
		Order("level ASC, sort_order ASC, id ASC").Find(&rewards).Error; err != nil {
		return VipBenefitOverview{}, err
	}
	var claims []models.VipBenefitClaim
	if err := db.DB.WithContext(ctx).Where("user_id = ?", userID).
		Order("level ASC, reward_id ASC, claim_no ASC").Find(&claims).Error; err != nil {
		return VipBenefitOverview{}, err
	}

	claimsByReward := map[int32][]models.VipBenefitClaim{}
	for _, claim := range claims {
		claimsByReward[claim.RewardID] = append(claimsByReward[claim.RewardID], claim)
	}

	type baseView struct {
		reward          models.VipBenefitReward
		claimedCount    int
		remainingClaims int
		claims          []models.VipBenefitClaim
	}
	outstandingLevels := map[int32]bool{}
	baseViews := make([]baseView, 0, len(rewards))
	for _, reward := range rewards {
		rewardClaims := claimsByReward[reward.ID]
		claimedCount := len(rewardClaims)
		remainingClaims := int(reward.ClaimLimit) - claimedCount
		if remainingClaims < 0 {
			remainingClaims = 0
		}
		if float64(reward.Level) <= vip.Level && remainingClaims > 0 {
			outstandingLevels[reward.Level] = true
		}
		baseViews = append(baseViews, baseView{
			reward:          reward,
			claimedCount:    claimedCount,
			remainingClaims: remainingClaims,
			claims:          rewardClaims,
		})
	}

	rewardViews := make([]VipBenefitRewardView, 0, len(baseViews))
	for _, item := range baseViews {
		rewardType := string(item.reward.Type)

		var lowerOutstanding *int32
		levels := make([]int32, 0, len(outstandingLevels))
		for level := range outstandingLevels {
			if level < item.reward.Level {
				levels = append(levels, level)
			}
		}
		sort.Slice(levels, func(i, j int) bool { return levels[i] < levels[j] })
		if len(levels) > 0 {
			level := levels[0]
			lowerOutstanding = &level
		}

		state := VipBenefitRewardStateClaimable
		switch {
		case item.remainingClaims <= 0:
			state = VipBenefitRewardStateClaimed
		case float64(item.reward.Level) > vip.Level:
			state = VipBenefitRewardStateLocked
		case lowerOutstanding != nil:
			state = VipBenefitRewardStateBlocked
		}

		var blockedByLevel *int32
		if state == VipBenefitRewardStateBlocked {
			blockedByLevel = lowerOutstanding
		}

		claimViews := make([]VipBenefitClaimView, 0, len(item.claims))
		for _, claim := range item.claims {
			claimViews = append(claimViews, vipBenefitClaimToView(claim))
		}

		rewardViews = append(rewardViews, VipBenefitRewardView{
			ID:              item.reward.ID,
			Level:           item.reward.Level,
			Type:            rewardType,
			Title:           item.reward.Title,
			Description:     item.reward.Description,
			ClaimLimit:      item.reward.ClaimLimit,
			SortOrder:       item.reward.SortOrder,
			Enabled:         item.reward.Enabled,
			Config:          vipBenefitNormalizeStoredRewardConfig(rewardType, vipBenefitJSONValue(item.reward.Config)).toJSON(rewardType),
			CreatedAt:       vipBenefitISOTime(item.reward.CreatedAt),
			UpdatedAt:       vipBenefitISOTime(item.reward.UpdatedAt),
			ClaimedCount:    item.claimedCount,
			RemainingClaims: item.remainingClaims,
			State:           state,
			BlockedByLevel:  blockedByLevel,
			Claims:          claimViews,
		})
	}

	entitlement := VipBenefitAmountSummary{}
	remaining := VipBenefitAmountSummary{}
	for _, reward := range rewardViews {
		if float64(reward.Level) <= vip.Level {
			vipBenefitAddRewardAmount(&entitlement, reward, int(reward.ClaimLimit))
			vipBenefitAddRewardAmount(&remaining, reward, reward.RemainingClaims)
		}
	}

	summary := VipBenefitSummary{
		TotalRewards: len(rewardViews),
		Entitlement:  entitlement,
		Remaining:    remaining,
	}
	for _, reward := range rewardViews {
		if float64(reward.Level) <= vip.Level {
			summary.UnlockedRewards++
		}
		switch reward.State {
		case VipBenefitRewardStateClaimable:
			summary.ClaimableRewards++
		case VipBenefitRewardStateClaimed:
			summary.ClaimedRewards++
		case VipBenefitRewardStateLocked:
			summary.LockedRewards++
		case VipBenefitRewardStateBlocked:
			summary.BlockedRewards++
		}
	}
	for _, claim := range claims {
		if string(claim.Status) == string(models.VipBenefitClaimStatusPending) {
			summary.PendingRewards++
		}
	}

	return VipBenefitOverview{
		CurrentLevel:      vip.Level,
		UserVipMetric:     vip.Metric,
		UserStats:         vip.Stats,
		UserVipBadgeStyle: vip.BadgeStyle,
		Rewards:           rewardViews,
		Summary:           summary,
	}, nil
}

// ==================== claim ====================

// vipBenefitBuildClaimSnapshot mirrors buildClaimSnapshot.
func vipBenefitBuildClaimSnapshot(reward models.VipBenefitReward, config vipBenefitRewardConfig) map[string]any {
	var description any
	if reward.Description != nil {
		description = *reward.Description
	}
	return map[string]any{
		"rewardId":    reward.ID,
		"level":       reward.Level,
		"type":        string(reward.Type),
		"title":       reward.Title,
		"description": description,
		"claimLimit":  reward.ClaimLimit,
		"config":      config.toJSON(string(reward.Type)),
	}
}

// vipBenefitDeliveryStatus mirrors getDeliveryStatus.
func vipBenefitDeliveryStatus(rewardType string) string {
	if rewardType == VipBenefitRewardTypeInstance {
		return string(models.VipBenefitClaimStatusPending)
	}
	return string(models.VipBenefitClaimStatusDelivered)
}

// vipBenefitIsUniqueViolation reports the PostgreSQL unique_violation Prisma maps to
// P2002.
func vipBenefitIsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// VipClaimBenefitReward mirrors claimVipBenefitReward. The advisory lock on
// (4115, userId) is what makes double-claiming impossible.
func VipClaimBenefitReward(ctx context.Context, userID, rewardID int32) (VipBenefitClaimResult, error) {
	var createdClaim models.VipBenefitClaim

	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?, ?) AS locked",
			db.UserVipBenefitClaimLockNamespace, userID).Error; err != nil {
			return err
		}

		vip, err := VipGetCurrentUserVip(ctx, tx, userID)
		if err != nil {
			return err
		}

		var reward models.VipBenefitReward
		err = tx.Where("id = ? AND enabled = ?", rewardID, true).First(&reward).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return vipBenefitErrorStatus("REWARD_NOT_FOUND", "VIP benefit reward not found", 404)
		}
		if err != nil {
			return err
		}
		if float64(reward.Level) > vip.Level {
			return vipBenefitErrorStatus("VIP_LEVEL_REQUIRED", fmt.Sprintf("VIP%d is required to claim this reward", reward.Level), 403)
		}

		var lowerRewards []models.VipBenefitReward
		if err := tx.Where("enabled = ? AND level < ? AND level <= ?", true, reward.Level, int32(vip.Level)).
			Order("level ASC, sort_order ASC, id ASC").Find(&lowerRewards).Error; err != nil {
			return err
		}

		rewardIDsToCheck := make([]int32, 0, len(lowerRewards)+1)
		for _, lower := range lowerRewards {
			rewardIDsToCheck = append(rewardIDsToCheck, lower.ID)
		}
		rewardIDsToCheck = append(rewardIDsToCheck, reward.ID)

		var relevantClaims []struct {
			RewardID int32 `gorm:"column:reward_id"`
		}
		if err := tx.Raw(
			"SELECT reward_id FROM vip_benefit_claims WHERE user_id = ? AND reward_id IN ?", userID, rewardIDsToCheck,
		).Scan(&relevantClaims).Error; err != nil {
			return err
		}
		claimCounts := map[int32]int{}
		for _, claim := range relevantClaims {
			claimCounts[claim.RewardID]++
		}

		for _, lower := range lowerRewards {
			if claimCounts[lower.ID] < int(lower.ClaimLimit) {
				return vipBenefitErrorStatus("LOWER_LEVEL_REWARD_REQUIRED",
					fmt.Sprintf("Please claim all VIP%d rewards first", lower.Level), 409)
			}
		}

		existingCount := claimCounts[reward.ID]
		if existingCount >= int(reward.ClaimLimit) {
			return vipBenefitErrorStatus("CLAIM_LIMIT_REACHED", "This VIP benefit reward has already been fully claimed", 409)
		}

		rewardType := string(reward.Type)
		config := vipBenefitNormalizeStoredRewardConfig(rewardType, vipBenefitJSONValue(reward.Config))
		snapshot, err := json.Marshal(vipBenefitBuildClaimSnapshot(reward, config))
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		claim := models.VipBenefitClaim{
			UserID:    userID,
			RewardID:  reward.ID,
			Level:     reward.Level,
			Status:    models.VipBenefitClaimStatus(vipBenefitDeliveryStatus(rewardType)),
			ClaimNo:   int32(existingCount + 1),
			Snapshot:  datatypes.JSON(snapshot),
			CreatedAt: now,
		}
		if rewardType != VipBenefitRewardTypeInstance {
			claim.DeliveredAt = &now
		}
		if err := tx.Clauses(clause.Returning{}).Create(&claim).Error; err != nil {
			return err
		}

		if rewardType == VipBenefitRewardTypeBalance {
			amount, err := vipBenefitNormalizeMoney(vipBenefitConfigAmount(config), "Balance reward amount")
			if err != nil {
				return err
			}

			var balanceLocked bool
			if err := tx.Raw("SELECT pg_try_advisory_xact_lock(?, ?) AS locked",
				db.UserBalanceLockNamespace, userID).Scan(&balanceLocked).Error; err != nil {
				return err
			}
			if !balanceLocked {
				return vipBenefitErrorStatus("BALANCE_CONFLICT", "Balance is being processed, please try again later", 409)
			}

			var user models.User
			err = tx.Select("id", "balance").Where("id = ?", userID).First(&user).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return vipBenefitErrorStatus("USER_NOT_FOUND", "User not found", 404)
			}
			if err != nil {
				return err
			}
			balanceBefore := vipToNumber(user.Balance)

			amountDecimal := decimal.NewFromFloat(amount)
			if err := tx.Exec("UPDATE users SET balance = balance + ? WHERE id = ?", amountDecimal, userID).Error; err != nil {
				return err
			}
			if err := tx.Select("balance").Where("id = ?", userID).First(&user).Error; err != nil {
				return err
			}
			balanceAfter := vipToNumber(user.Balance)

			remark := "VIP benefit: " + reward.Title
			balanceLog := models.BalanceLog{
				UserID:        userID,
				Type:          models.BalanceLogTypeGift,
				Amount:        models.Decimal{Decimal: amountDecimal},
				BalanceBefore: models.Decimal{Decimal: decimal.NewFromFloat(balanceBefore)},
				BalanceAfter:  models.Decimal{Decimal: decimal.NewFromFloat(balanceAfter)},
				Remark:        &remark,
				CreatedAt:     time.Now().UTC(),
			}
			if err := tx.Create(&balanceLog).Error; err != nil {
				return err
			}
		} else if rewardType == VipBenefitRewardTypePoints {
			amount, err := vipBenefitNormalizePositiveInteger(vipBenefitConfigAmount(config), "Points reward amount", vipBenefitMaxPointsAmount)
			if err != nil {
				return err
			}
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?, ?) AS locked",
				db.UserPointsLockNamespace, userID).Error; err != nil {
				return err
			}

			// Prisma upsert with `update: {}`: create the row when missing.
			// user_points.updated_at has no database default, so it is supplied here.
			if err := tx.Exec("INSERT INTO user_points (user_id, updated_at) VALUES (?, ?) ON CONFLICT (user_id) DO NOTHING",
				userID, time.Now().UTC()).Error; err != nil {
				return err
			}
			var userPoints models.UserPoints
			if err := tx.Select("user_id", "points").Where("user_id = ?", userID).First(&userPoints).Error; err != nil {
				return err
			}
			pointsBefore := userPoints.Points

			if err := tx.Exec(
				"UPDATE user_points SET points = points + ?, total_earned = total_earned + ? WHERE user_id = ?",
				amount, amount, userID,
			).Error; err != nil {
				return err
			}
			if err := tx.Select("points").Where("user_id = ?", userID).First(&userPoints).Error; err != nil {
				return err
			}

			remark := "VIP benefit: " + reward.Title
			claimID := claim.ID
			pointsLog := models.PointsLog{
				UserID:       userID,
				Type:         models.PointsLogTypeVipBenefit,
				Amount:       amount,
				PointsBefore: pointsBefore,
				PointsAfter:  userPoints.Points,
				RelatedID:    &claimID,
				Remark:       &remark,
				CreatedAt:    time.Now().UTC(),
			}
			if err := tx.Create(&pointsLog).Error; err != nil {
				return err
			}
		}

		createdClaim = claim
		return nil
	})
	if err != nil {
		if vipBenefitIsUniqueViolation(err) {
			return VipBenefitClaimResult{}, vipBenefitErrorStatus("CLAIM_CONFLICT", "This VIP benefit reward was already claimed, please refresh and try again", 409)
		}
		return VipBenefitClaimResult{}, err
	}

	overview, err := VipGetBenefitOverview(ctx, userID)
	if err != nil {
		return VipBenefitClaimResult{}, err
	}
	return VipBenefitClaimResult{Claim: vipBenefitClaimToView(createdClaim), Overview: overview}, nil
}

// vipBenefitConfigAmount extracts config.amount for the claim path.
func vipBenefitConfigAmount(config vipBenefitRewardConfig) any {
	if config.Amount == nil {
		return nil
	}
	return *config.Amount
}

// VipClaimAllAvailableBenefitRewards mirrors claimAllAvailableVipBenefitRewards.
func VipClaimAllAvailableBenefitRewards(ctx context.Context, userID int32) (VipBenefitBulkClaimResult, error) {
	claims := []VipBenefitClaimView{}
	overview, err := VipGetBenefitOverview(ctx, userID)
	if err != nil {
		return VipBenefitBulkClaimResult{}, err
	}

	for attempt := 0; attempt < vipBenefitMaxBulkClaims; attempt++ {
		var next *VipBenefitRewardView
		candidates := make([]VipBenefitRewardView, 0, len(overview.Rewards))
		for _, reward := range overview.Rewards {
			if reward.State == VipBenefitRewardStateClaimable {
				candidates = append(candidates, reward)
			}
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].Level != candidates[j].Level {
				return candidates[i].Level < candidates[j].Level
			}
			if candidates[i].SortOrder != candidates[j].SortOrder {
				return candidates[i].SortOrder < candidates[j].SortOrder
			}
			return candidates[i].ID < candidates[j].ID
		})
		if len(candidates) > 0 {
			next = &candidates[0]
		}
		if next == nil {
			return VipBenefitBulkClaimResult{Claims: claims, Overview: overview}, nil
		}

		result, err := VipClaimBenefitReward(ctx, userID, next.ID)
		if err != nil {
			return VipBenefitBulkClaimResult{}, err
		}
		claims = append(claims, result.Claim)
		overview = result.Overview
	}

	return VipBenefitBulkClaimResult{}, vipBenefitErrorStatus("BULK_CLAIM_LIMIT_EXCEEDED", "Too many VIP benefit rewards to claim at once", 409)
}
