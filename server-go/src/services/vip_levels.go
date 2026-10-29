// Port of server/src/services/vip-levels.ts.
//
// VIP rule persistence plus the pure rule/level/progress calculators. Every
// numeric field mirrors the TypeScript `number` JSON shape (float64), never
// models.Decimal — see docs/GO_MIGRATION.md §3. Unexported helpers are prefixed
// with `vip` because this package is shared by parallel module ports.
package services

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/shopspring/decimal"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// MaxVipLevel mirrors MAX_VIP_LEVEL.
const MaxVipLevel = 10

const (
	vipMaxMoneyThreshold          = 99999999.99
	vipMaxMoneyThresholdLabel     = "99999999.99"
	vipMaxBenefitPoints           = 1000000000
	vipMaxBenefitInstanceDays     = 3650
	vipMaxBenefitInstanceQuantity = 10
	userVipMetricConfigKey        = "user_vip_metric"
	vipUserVipMetricLabel         = "用户 VIP 统计口径"
	vipUserVipMetricDescription   = "用户 VIP 等级全局计算口径：totalRecharge=累计充值，totalConsume=累计消费"
)

var vipHexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// VipRuleType mirrors the Prisma VipLevelRuleType enum.
type VipRuleType = models.VipLevelRuleType

// VipConditionMode mirrors the Prisma VipLevelConditionMode enum.
type VipConditionMode = models.VipLevelConditionMode

// VipRuleTypeUser / VipConditionMode* mirror the enum literals used inline in TS.
const (
	VipRuleTypeUser     VipRuleType      = models.VipLevelRuleTypeUser
	VipConditionModeAny VipConditionMode = models.VipLevelConditionModeAny
	VipConditionModeAll VipConditionMode = models.VipLevelConditionModeAll
)

// UserVipMetric mirrors `UserVipMetric`.
type UserVipMetric string

// VipProgressMetric mirrors `VipProgressMetric`.
type VipProgressMetric string

const (
	UserVipMetricTotalRecharge UserVipMetric = "totalRecharge"
	UserVipMetricTotalConsume  UserVipMetric = "totalConsume"

	VipProgressMetricTotalRecharge VipProgressMetric = "totalRecharge"
	VipProgressMetricTotalConsume  VipProgressMetric = "totalConsume"
	VipProgressMetricInstanceCount VipProgressMetric = "instanceCount"
)

// VipRuleInput mirrors `VipRuleInput`. Fields stay `any` because the Node route
// performs no schema validation and sanitizeVipRuleInput coerces every value.
type VipRuleInput struct {
	Level         any `json:"level"`
	Enabled       any `json:"enabled"`
	ConditionMode any `json:"conditionMode"`
	UserMetric    any `json:"userMetric"`
	MinRecharge   any `json:"minRecharge"`
	MinConsume    any `json:"minConsume"`
	Benefits      any `json:"benefits"`
}

// VipBadgeStyle mirrors `VipBadgeStyle`.
type VipBadgeStyle struct {
	BackgroundColor string `json:"backgroundColor"`
	TextColor       string `json:"textColor"`
}

// VipBenefitHallBalance mirrors the `balance` block of VipBenefitHallConfig.
type VipBenefitHallBalance struct {
	Enabled bool    `json:"enabled"`
	Amount  float64 `json:"amount"`
}

// VipBenefitHallPoints mirrors the `points` block of VipBenefitHallConfig.
type VipBenefitHallPoints struct {
	Enabled bool    `json:"enabled"`
	Amount  float64 `json:"amount"`
}

// VipBenefitHallInstance mirrors the `instance` block of VipBenefitHallConfig.
type VipBenefitHallInstance struct {
	Enabled     bool     `json:"enabled"`
	PackageID   *float64 `json:"packageId"`
	PackageName *string  `json:"packageName"`
	PlanID      *float64 `json:"planId"`
	PlanName    *string  `json:"planName"`
	Days        float64  `json:"days"`
	Quantity    float64  `json:"quantity"`
}

// VipBenefitHallConfig mirrors `VipBenefitHallConfig`.
type VipBenefitHallConfig struct {
	Balance  VipBenefitHallBalance  `json:"balance"`
	Points   VipBenefitHallPoints   `json:"points"`
	Instance VipBenefitHallInstance `json:"instance"`
}

// NormalizedVipRule mirrors `NormalizedVipRule`. ID is omitted when absent, like
// the optional `id?: number` in TS.
type NormalizedVipRule struct {
	ID            *int32         `json:"id,omitempty"`
	Type          string         `json:"type"`
	Level         float64        `json:"level"`
	Enabled       bool           `json:"enabled"`
	ConditionMode string         `json:"conditionMode"`
	MinRecharge   *float64       `json:"minRecharge"`
	MinConsume    *float64       `json:"minConsume"`
	Benefits      map[string]any `json:"benefits"`
	BadgeStyle    VipBadgeStyle  `json:"badgeStyle"`
}

// UserVipStats mirrors `UserVipStats`.
type UserVipStats struct {
	TotalRecharge float64 `json:"totalRecharge"`
	TotalConsume  float64 `json:"totalConsume"`
}

// VipProgressCondition mirrors `VipProgressCondition`.
type VipProgressCondition struct {
	Metric    string  `json:"metric"`
	Current   float64 `json:"current"`
	Target    float64 `json:"target"`
	Remaining float64 `json:"remaining"`
	Matched   bool    `json:"matched"`
	Progress  float64 `json:"progress"`
}

// VipLevelProgress mirrors `VipLevelProgress`.
type VipLevelProgress struct {
	CurrentLevel  float64                `json:"currentLevel"`
	NextLevel     *float64               `json:"nextLevel"`
	ConditionMode *string                `json:"conditionMode"`
	UserMetric    *string                `json:"userMetric"`
	Progress      float64                `json:"progress"`
	IsMaxLevel    bool                   `json:"isMaxLevel"`
	Conditions    []VipProgressCondition `json:"conditions"`
}

// VipOverview mirrors `VipOverview`.
type VipOverview struct {
	UserVipLevel      float64          `json:"userVipLevel"`
	UserVipBadgeStyle *VipBadgeStyle   `json:"userVipBadgeStyle"`
	UserVipMetric     UserVipMetric    `json:"userVipMetric"`
	UserStats         UserVipStats     `json:"userStats"`
	UserVipProgress   VipLevelProgress `json:"userVipProgress"`
}

// UserVipLevelResult mirrors calculateUserVipLevel's `{level, stats}` return.
type UserVipLevelResult struct {
	Level float64      `json:"level"`
	Stats UserVipStats `json:"stats"`
}

// vipBenefitInstanceRef mirrors the internal `VipBenefitInstanceRef`.
type vipBenefitInstanceRef struct {
	level     float64
	packageID float64
	planID    float64
}

// ==================== numeric coercion ====================

// vipJSRound mirrors Math.round (floor(x + 0.5)).
func vipJSRound(value float64) float64 {
	return math.Floor(value + 0.5)
}

// vipRoundAmount mirrors roundAmount: Math.round(value * 100) / 100.
func vipRoundAmount(value float64) float64 {
	return vipJSRound(value*100) / 100
}

// vipIsFinite mirrors Number.isFinite.
func vipIsFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// vipIsInteger mirrors Number.isInteger.
func vipIsInteger(value float64) bool {
	return vipIsFinite(value) && value == math.Trunc(value)
}

// vipParseFloat mirrors parseFloat: the longest numeric prefix is parsed and a
// value with no leading digits is NaN.
func vipParseFloat(value string) float64 {
	trimmed := strings.TrimLeft(value, " \t\n\r\v\f")
	if trimmed == "" {
		return math.NaN()
	}

	end := 0
	if trimmed[0] == '+' || trimmed[0] == '-' {
		end = 1
	}
	intStart := end
	for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
		end++
	}
	intDigits := end - intStart

	fracDigits := 0
	if end < len(trimmed) && trimmed[end] == '.' {
		end++
		fracStart := end
		for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
			end++
		}
		fracDigits = end - fracStart
	}
	if intDigits == 0 && fracDigits == 0 {
		return math.NaN()
	}

	if end < len(trimmed) && (trimmed[end] == 'e' || trimmed[end] == 'E') {
		exponentStart := end
		end++
		if end < len(trimmed) && (trimmed[end] == '+' || trimmed[end] == '-') {
			end++
		}
		exponentDigitsStart := end
		for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
			end++
		}
		if end == exponentDigitsStart {
			end = exponentStart
		}
	}

	parsed, err := strconv.ParseFloat(trimmed[:end], 64)
	if err != nil {
		return math.NaN()
	}
	return parsed
}

// vipNumber mirrors JS Number(value); the boolean reports isFinite.
func vipNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case nil:
		return 0, true
	case float64:
		return typed, vipIsFinite(typed)
	case float32:
		converted := float64(typed)
		return converted, vipIsFinite(converted)
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return math.NaN(), false
		}
		return parsed, vipIsFinite(parsed)
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	case models.Decimal:
		converted, _ := typed.Decimal.Float64()
		return converted, vipIsFinite(converted)
	case *models.Decimal:
		if typed == nil {
			return 0, true
		}
		converted, _ := typed.Decimal.Float64()
		return converted, vipIsFinite(converted)
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, true
		}
		switch trimmed {
		case "Infinity", "+Infinity":
			return math.Inf(1), false
		case "-Infinity":
			return math.Inf(-1), false
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return math.NaN(), false
		}
		return parsed, vipIsFinite(parsed)
	default:
		return math.NaN(), false
	}
}

// vipToNumber mirrors the local toNumber(): nullish -> 0, non-finite number -> 0,
// otherwise parseFloat(String(value)) || 0.
func vipToNumber(value any) float64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case float64:
		if !vipIsFinite(typed) {
			return 0
		}
		return typed
	case float32:
		converted := float64(typed)
		if !vipIsFinite(converted) {
			return 0
		}
		return converted
	case int:
		return float64(typed)
	case int32:
		return float64(typed)
	case int64:
		return float64(typed)
	case models.Decimal:
		converted, _ := typed.Decimal.Float64()
		if !vipIsFinite(converted) {
			return 0
		}
		return converted
	case *models.Decimal:
		if typed == nil {
			return 0
		}
		converted, _ := typed.Decimal.Float64()
		if !vipIsFinite(converted) {
			return 0
		}
		return converted
	case string:
		parsed := vipParseFloat(typed)
		if !vipIsFinite(parsed) {
			return 0
		}
		return parsed
	default:
		parsed, ok := vipNumber(value)
		if !ok {
			return 0
		}
		return parsed
	}
}

// vipNullableMoney mirrors nullableMoney. A nil result means "no threshold".
func vipNullableMoney(value any) (*float64, error) {
	if value == nil {
		return nil, nil
	}
	if asString, ok := value.(string); ok && asString == "" {
		return nil, nil
	}

	parsed, ok := vipNumber(value)
	if !ok || parsed < 0 || parsed > vipMaxMoneyThreshold {
		return nil, fmt.Errorf("Money threshold must be between 0 and %s", vipMaxMoneyThresholdLabel)
	}
	rounded := vipRoundAmount(parsed)
	return &rounded, nil
}

// vipHasThreshold mirrors hasThreshold.
func vipHasThreshold(value *float64) bool {
	return value != nil && *value > 0
}

// vipToProgressPercent mirrors toProgressPercent.
func vipToProgressPercent(current, target float64) float64 {
	if !vipIsFinite(current) || !vipIsFinite(target) || target <= 0 {
		return 0
	}
	value := vipJSRound((current / target) * 100)
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

// ==================== metric / badge defaults ====================

// vipNormalizeUserVipMetric mirrors normalizeUserVipMetric (string | null input).
func vipNormalizeUserVipMetric(value *string) UserVipMetric {
	if value != nil && *value == string(UserVipMetricTotalConsume) {
		return UserVipMetricTotalConsume
	}
	return UserVipMetricTotalRecharge
}

// vipNormalizeUserVipMetricAny mirrors normalizeUserVipMetric (unknown input).
func vipNormalizeUserVipMetricAny(value any) UserVipMetric {
	if asString, ok := value.(string); ok && asString == string(UserVipMetricTotalConsume) {
		return UserVipMetricTotalConsume
	}
	return UserVipMetricTotalRecharge
}

// vipDefaultBadgeStyles mirrors the 10-entry default palette.
var vipDefaultBadgeStyles = [10]VipBadgeStyle{
	{BackgroundColor: "#FEF3C7", TextColor: "#92400E"},
	{BackgroundColor: "#D1FAE5", TextColor: "#047857"},
	{BackgroundColor: "#DBEAFE", TextColor: "#1D4ED8"},
	{BackgroundColor: "#EDE9FE", TextColor: "#6D28D9"},
	{BackgroundColor: "#FFEDD5", TextColor: "#C2410C"},
	{BackgroundColor: "#FCE7F3", TextColor: "#BE185D"},
	{BackgroundColor: "#FFE4E6", TextColor: "#BE123C"},
	{BackgroundColor: "#CFFAFE", TextColor: "#0E7490"},
	{BackgroundColor: "#FAE8FF", TextColor: "#A21CAF"},
	{BackgroundColor: "#FEF9C3", TextColor: "#A16207"},
}

// GetDefaultVipBadgeStyle mirrors getDefaultVipBadgeStyle.
func GetDefaultVipBadgeStyle(level float64) VipBadgeStyle {
	index := int(math.Max(1, math.Floor(level))) - 1
	index %= len(vipDefaultBadgeStyles)
	if index < 0 {
		index += len(vipDefaultBadgeStyles)
	}
	return vipDefaultBadgeStyles[index]
}

// vipNormalizeHexColor mirrors normalizeHexColor.
func vipNormalizeHexColor(value any, fallback string) string {
	if asString, ok := value.(string); ok && vipHexColorRe.MatchString(asString) {
		return asString
	}
	return fallback
}

// NormalizeVipBadgeStyle mirrors normalizeVipBadgeStyle.
func NormalizeVipBadgeStyle(level float64, value any) VipBadgeStyle {
	fallback := GetDefaultVipBadgeStyle(level)
	style, ok := value.(map[string]any)
	if !ok {
		return fallback
	}
	return VipBadgeStyle{
		BackgroundColor: vipNormalizeHexColor(style["backgroundColor"], fallback.BackgroundColor),
		TextColor:       vipNormalizeHexColor(style["textColor"], fallback.TextColor),
	}
}

// ==================== benefit hall normalization ====================

// vipGetRecord mirrors getRecord.
func vipGetRecord(value any) map[string]any {
	if record, ok := value.(map[string]any); ok {
		return record
	}
	return map[string]any{}
}

// vipNormalizeBenefits mirrors normalizeBenefits.
func vipNormalizeBenefits(value any) map[string]any {
	if record, ok := value.(map[string]any); ok {
		return record
	}
	return map[string]any{}
}

// vipNormalizeBenefitMoney mirrors normalizeBenefitMoney.
func vipNormalizeBenefitMoney(value any) float64 {
	parsed, ok := vipNumber(value)
	if !ok || parsed <= 0 {
		return 0
	}
	return math.Min(vipMaxMoneyThreshold, vipRoundAmount(parsed))
}

// vipNormalizeBenefitInteger mirrors normalizeBenefitInteger.
func vipNormalizeBenefitInteger(value any, max float64) float64 {
	parsed, ok := vipNumber(value)
	if !ok || parsed <= 0 {
		return 0
	}
	return math.Min(max, math.Floor(parsed))
}

// vipNormalizeBenefitID mirrors normalizeBenefitId.
func vipNormalizeBenefitID(value any) *float64 {
	parsed, ok := vipNumber(value)
	if !ok || !vipIsInteger(parsed) || parsed <= 0 {
		return nil
	}
	return &parsed
}

// vipNormalizeBenefitLabel mirrors normalizeBenefitLabel (trim + slice(0, 100)).
func vipNormalizeBenefitLabel(value any) *string {
	asString, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(asString)
	if trimmed == "" {
		return nil
	}
	runes := []rune(trimmed)
	if len(runes) > 100 {
		trimmed = string(runes[:100])
	}
	return &trimmed
}

// vipNormalizeBenefitHallConfig mirrors normalizeBenefitHallConfig.
func vipNormalizeBenefitHallConfig(value any, strict bool) (VipBenefitHallConfig, error) {
	raw := vipGetRecord(value)
	balance := vipGetRecord(raw["balance"])
	points := vipGetRecord(raw["points"])
	instance := vipGetRecord(raw["instance"])

	balanceAmount := vipNormalizeBenefitMoney(balance["amount"])
	pointsAmount := vipNormalizeBenefitInteger(points["amount"], vipMaxBenefitPoints)
	packageID := vipNormalizeBenefitID(instance["packageId"])
	planID := vipNormalizeBenefitID(instance["planId"])

	days := vipNormalizeBenefitInteger(instance["days"], vipMaxBenefitInstanceDays)
	if days == 0 {
		days = 30
	}
	quantity := vipNormalizeBenefitInteger(instance["quantity"], vipMaxBenefitInstanceQuantity)
	if quantity == 0 {
		quantity = 1
	}

	balanceEnabled := balance["enabled"] == true
	pointsEnabled := points["enabled"] == true
	instanceEnabled := instance["enabled"] == true

	if strict {
		if balanceEnabled && balanceAmount <= 0 {
			return VipBenefitHallConfig{}, fmt.Errorf("VIP benefit balance amount must be greater than 0")
		}
		if pointsEnabled && pointsAmount <= 0 {
			return VipBenefitHallConfig{}, fmt.Errorf("VIP benefit points amount must be a positive integer")
		}
		if instanceEnabled && (packageID == nil || planID == nil) {
			return VipBenefitHallConfig{}, fmt.Errorf("VIP benefit instance requires a package and plan")
		}
		if instanceEnabled && vipNormalizeBenefitInteger(instance["days"], vipMaxBenefitInstanceDays) <= 0 {
			return VipBenefitHallConfig{}, fmt.Errorf("VIP benefit instance days must be a positive integer")
		}
		if instanceEnabled && vipNormalizeBenefitInteger(instance["quantity"], vipMaxBenefitInstanceQuantity) <= 0 {
			return VipBenefitHallConfig{}, fmt.Errorf("VIP benefit instance quantity must be a positive integer")
		}
	}

	return VipBenefitHallConfig{
		Balance: VipBenefitHallBalance{
			Enabled: balanceEnabled && balanceAmount > 0,
			Amount:  balanceAmount,
		},
		Points: VipBenefitHallPoints{
			Enabled: pointsEnabled && pointsAmount > 0,
			Amount:  pointsAmount,
		},
		Instance: VipBenefitHallInstance{
			Enabled:     instanceEnabled && packageID != nil && planID != nil,
			PackageID:   packageID,
			PackageName: vipNormalizeBenefitLabel(instance["packageName"]),
			PlanID:      planID,
			PlanName:    vipNormalizeBenefitLabel(instance["planName"]),
			Days:        days,
			Quantity:    quantity,
		},
	}, nil
}

// vipSanitizeBenefits mirrors sanitizeBenefits.
func vipSanitizeBenefits(level float64, benefits any, strict bool) (map[string]any, error) {
	normalized := vipNormalizeBenefits(benefits)
	hall, err := vipNormalizeBenefitHallConfig(normalized["benefitHall"], strict)
	if err != nil {
		return nil, err
	}

	out := make(map[string]any, len(normalized)+2)
	for key, value := range normalized {
		out[key] = value
	}
	out["badgeStyle"] = NormalizeVipBadgeStyle(level, normalized["badgeStyle"])
	out["benefitHall"] = hall
	return out, nil
}

// vipCollectBenefitInstanceRefs mirrors collectBenefitInstanceRefs.
func vipCollectBenefitInstanceRefs(rules []NormalizedVipRule) []vipBenefitInstanceRef {
	refs := []vipBenefitInstanceRef{}
	for _, rule := range rules {
		hall, err := vipNormalizeBenefitHallConfig(rule.Benefits["benefitHall"], false)
		if err != nil {
			continue
		}
		if hall.Instance.Enabled && hall.Instance.PackageID != nil && hall.Instance.PlanID != nil {
			refs = append(refs, vipBenefitInstanceRef{
				level:     rule.Level,
				packageID: *hall.Instance.PackageID,
				planID:    *hall.Instance.PlanID,
			})
		}
	}
	return refs
}

// vipInt32ID narrows a JSON number id to int32. Prisma ids are int32, so an
// out-of-range value can never match a row (mirrors Node's "no match" outcome).
func vipInt32ID(value float64) (int32, bool) {
	if !vipIsInteger(value) || value < 1 || value > math.MaxInt32 {
		return 0, false
	}
	return int32(value), true
}

// vipValidateBenefitInstanceRefs mirrors validateBenefitInstanceRefs.
func vipValidateBenefitInstanceRefs(ctx context.Context, refs []vipBenefitInstanceRef) error {
	if len(refs) == 0 {
		return nil
	}

	packageIDSet := map[int32]bool{}
	planIDSet := map[int32]bool{}
	packageIDs := make([]int32, 0, len(refs))
	planIDs := make([]int32, 0, len(refs))
	for _, ref := range refs {
		if packageID, ok := vipInt32ID(ref.packageID); ok && !packageIDSet[packageID] {
			packageIDSet[packageID] = true
			packageIDs = append(packageIDs, packageID)
		}
		if planID, ok := vipInt32ID(ref.planID); ok && !planIDSet[planID] {
			planIDSet[planID] = true
			planIDs = append(planIDs, planID)
		}
	}

	existingPackageIDs := map[int32]bool{}
	if len(packageIDs) > 0 {
		var packages []models.Package
		if err := db.DB.WithContext(ctx).
			Select("id").
			Where("id IN ? AND active = ?", packageIDs, true).
			Find(&packages).Error; err != nil {
			return err
		}
		for _, row := range packages {
			existingPackageIDs[row.ID] = true
		}
	}

	activePlanPackageMap := map[int32]int32{}
	if len(planIDs) > 0 {
		var plans []models.PackagePlan
		if err := db.DB.WithContext(ctx).
			Select("id", "package_id").
			Where("id IN ? AND is_active = ?", planIDs, true).
			Find(&plans).Error; err != nil {
			return err
		}
		for _, row := range plans {
			activePlanPackageMap[row.ID] = row.PackageID
		}
	}

	for _, ref := range refs {
		packageID, packageOK := vipInt32ID(ref.packageID)
		if !packageOK || !existingPackageIDs[packageID] {
			return fmt.Errorf("VIP%d benefit package does not exist or is disabled", int(ref.level))
		}
		planID, planOK := vipInt32ID(ref.planID)
		mappedPackageID, planFound := activePlanPackageMap[planID]
		if !planOK || !planFound || mappedPackageID != packageID {
			return fmt.Errorf("VIP%d benefit plan does not exist, is disabled, or does not belong to the selected package", int(ref.level))
		}
	}
	return nil
}

// ==================== rule normalization ====================

// vipBenefitsFromJSON decodes the stored benefits JSON into a generic value.
func vipBenefitsFromJSON(data datatypes.JSON) any {
	if len(data) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil
	}
	return decoded
}

// NormalizeVipRule mirrors normalizeVipRule (non-strict; never fails).
func NormalizeVipRule(rule models.VipLevelRule) NormalizedVipRule {
	level := float64(rule.Level)
	benefits, err := vipSanitizeBenefits(level, vipBenefitsFromJSON(rule.Benefits), false)
	if err != nil {
		// strict=false cannot fail; keep the zero value defensively.
		benefits = map[string]any{}
	}

	var minRecharge *float64
	if rule.MinRecharge != nil {
		value := vipToNumber(*rule.MinRecharge)
		minRecharge = &value
	}
	var minConsume *float64
	if rule.MinConsume != nil {
		value := vipToNumber(*rule.MinConsume)
		minConsume = &value
	}

	id := rule.ID
	return NormalizedVipRule{
		ID:            &id,
		Type:          string(rule.Type),
		Level:         level,
		Enabled:       rule.Enabled,
		ConditionMode: string(rule.ConditionMode),
		MinRecharge:   minRecharge,
		MinConsume:    minConsume,
		Benefits:      benefits,
		BadgeStyle:    NormalizeVipBadgeStyle(level, benefits["badgeStyle"]),
	}
}

// SanitizeVipRuleInput mirrors sanitizeVipRuleInput.
func SanitizeVipRuleInput(ruleType VipRuleType, input VipRuleInput, userMetric UserVipMetric) (NormalizedVipRule, error) {
	level, levelOK := vipNumber(input.Level)
	if !levelOK || !vipIsInteger(level) || level < 1 || level > MaxVipLevel {
		return NormalizedVipRule{}, fmt.Errorf("VIP level must be between 1 and %d", MaxVipLevel)
	}

	conditionMode := VipConditionModeAny
	if mode, ok := input.ConditionMode.(string); ok && mode == string(VipConditionModeAll) {
		conditionMode = VipConditionModeAll
	}

	enabled := true
	if asBool, ok := input.Enabled.(bool); ok {
		enabled = asBool
	}

	normalized := NormalizedVipRule{
		Type:          string(ruleType),
		Level:         level,
		Enabled:       enabled,
		ConditionMode: string(conditionMode),
	}

	if ruleType == VipRuleTypeUser {
		normalized.ConditionMode = string(VipConditionModeAny)

		if userMetric == UserVipMetricTotalRecharge {
			minRecharge, err := vipNullableMoney(input.MinRecharge)
			if err != nil {
				return NormalizedVipRule{}, err
			}
			normalized.MinRecharge = minRecharge
		}
		if userMetric == UserVipMetricTotalConsume {
			minConsume, err := vipNullableMoney(input.MinConsume)
			if err != nil {
				return NormalizedVipRule{}, err
			}
			normalized.MinConsume = minConsume
		}
	}

	benefits, err := vipSanitizeBenefits(level, input.Benefits, true)
	if err != nil {
		return NormalizedVipRule{}, err
	}
	normalized.Benefits = benefits
	normalized.BadgeStyle = NormalizeVipBadgeStyle(level, benefits["badgeStyle"])

	if enabled && !HasActiveCondition(ruleType, normalized) {
		return NormalizedVipRule{}, fmt.Errorf("VIP%d requires at least one condition", int(level))
	}

	return normalized, nil
}

// ==================== rule matching ====================

// HasActiveCondition mirrors hasActiveCondition.
func HasActiveCondition(ruleType VipRuleType, rule NormalizedVipRule) bool {
	if ruleType == VipRuleTypeUser {
		return vipHasThreshold(rule.MinRecharge) || vipHasThreshold(rule.MinConsume)
	}
	return false
}

// vipGetRuleChecks mirrors getRuleChecks.
func vipGetRuleChecks(ruleType VipRuleType, rule NormalizedVipRule, stats UserVipStats) []bool {
	checks := []bool{}
	if ruleType == VipRuleTypeUser {
		if vipHasThreshold(rule.MinRecharge) {
			checks = append(checks, stats.TotalRecharge >= *rule.MinRecharge)
		}
		if vipHasThreshold(rule.MinConsume) {
			checks = append(checks, stats.TotalConsume >= *rule.MinConsume)
		}
	}
	return checks
}

// vipGetUserRuleChecks mirrors getUserRuleChecks.
func vipGetUserRuleChecks(rule NormalizedVipRule, stats UserVipStats, userMetric UserVipMetric) []bool {
	checks := []bool{}
	if userMetric == UserVipMetricTotalRecharge && vipHasThreshold(rule.MinRecharge) {
		checks = append(checks, stats.TotalRecharge >= *rule.MinRecharge)
	}
	if userMetric == UserVipMetricTotalConsume && vipHasThreshold(rule.MinConsume) {
		checks = append(checks, stats.TotalConsume >= *rule.MinConsume)
	}
	return checks
}

// vipHasUserMetricCondition mirrors hasUserMetricCondition.
func vipHasUserMetricCondition(rule NormalizedVipRule, userMetric UserVipMetric) bool {
	if userMetric == UserVipMetricTotalRecharge {
		return vipHasThreshold(rule.MinRecharge)
	}
	return vipHasThreshold(rule.MinConsume)
}

// IsVipRuleMatched mirrors isVipRuleMatched.
func IsVipRuleMatched(ruleType VipRuleType, rule NormalizedVipRule, stats UserVipStats) bool {
	if !rule.Enabled || !HasActiveCondition(ruleType, rule) {
		return false
	}
	checks := vipGetRuleChecks(ruleType, rule, stats)
	if len(checks) == 0 {
		return false
	}
	if rule.ConditionMode == string(VipConditionModeAll) {
		for _, matched := range checks {
			if !matched {
				return false
			}
		}
		return true
	}
	for _, matched := range checks {
		if matched {
			return true
		}
	}
	return false
}

// IsUserVipRuleMatched mirrors isUserVipRuleMatched.
func IsUserVipRuleMatched(rule NormalizedVipRule, stats UserVipStats, userMetric UserVipMetric) bool {
	if !rule.Enabled || rule.Type != string(VipRuleTypeUser) || !vipHasUserMetricCondition(rule, userMetric) {
		return false
	}
	checks := vipGetUserRuleChecks(rule, stats, userMetric)
	if len(checks) == 0 {
		return false
	}
	for _, matched := range checks {
		if matched {
			return true
		}
	}
	return false
}

// CalculateVipLevel mirrors calculateVipLevel (highest matched level, else 0).
func CalculateVipLevel(ruleType VipRuleType, rules []NormalizedVipRule, stats UserVipStats) float64 {
	best := 0.0
	for _, rule := range rules {
		if rule.Type != string(ruleType) || !IsVipRuleMatched(ruleType, rule, stats) {
			continue
		}
		if rule.Level > best {
			best = rule.Level
		}
	}
	return best
}

// CalculateUserVipLevelByMetric mirrors calculateUserVipLevelByMetric.
func CalculateUserVipLevelByMetric(rules []NormalizedVipRule, stats UserVipStats, userMetric UserVipMetric) float64 {
	best := 0.0
	for _, rule := range rules {
		if !IsUserVipRuleMatched(rule, stats, userMetric) {
			continue
		}
		if rule.Level > best {
			best = rule.Level
		}
	}
	return best
}

// ==================== progress ====================

// vipBuildVipProgressCondition mirrors buildVipProgressCondition.
func vipBuildVipProgressCondition(metric VipProgressMetric, current, target float64) VipProgressCondition {
	normalizedCurrent := vipRoundAmount(math.Max(current, 0))
	normalizedTarget := vipRoundAmount(math.Max(target, 0))
	return VipProgressCondition{
		Metric:    string(metric),
		Current:   normalizedCurrent,
		Target:    normalizedTarget,
		Remaining: vipRoundAmount(math.Max(normalizedTarget-normalizedCurrent, 0)),
		Matched:   normalizedCurrent >= normalizedTarget,
		Progress:  vipToProgressPercent(normalizedCurrent, normalizedTarget),
	}
}

// vipGetVipProgressConditions mirrors getVipProgressConditions.
func vipGetVipProgressConditions(ruleType VipRuleType, rule NormalizedVipRule, stats UserVipStats, userMetric UserVipMetric) []VipProgressCondition {
	conditions := []VipProgressCondition{}
	if ruleType == VipRuleTypeUser {
		if userMetric == UserVipMetricTotalRecharge && vipHasThreshold(rule.MinRecharge) {
			conditions = append(conditions, vipBuildVipProgressCondition(VipProgressMetricTotalRecharge, stats.TotalRecharge, *rule.MinRecharge))
		}
		if userMetric == UserVipMetricTotalConsume && vipHasThreshold(rule.MinConsume) {
			conditions = append(conditions, vipBuildVipProgressCondition(VipProgressMetricTotalConsume, stats.TotalConsume, *rule.MinConsume))
		}
	}
	return conditions
}

// CalculateVipProgress mirrors calculateVipProgress.
func CalculateVipProgress(ruleType VipRuleType, rules []NormalizedVipRule, currentLevel float64, stats UserVipStats, userMetric UserVipMetric) VipLevelProgress {
	var nextRule *NormalizedVipRule
	for index := range rules {
		rule := rules[index]
		if rule.Type != string(ruleType) || !rule.Enabled || rule.Level <= currentLevel {
			continue
		}
		var eligible bool
		if ruleType == VipRuleTypeUser {
			eligible = vipHasUserMetricCondition(rule, userMetric)
		} else {
			eligible = HasActiveCondition(ruleType, rule)
		}
		if !eligible {
			continue
		}
		if nextRule == nil || rule.Level < nextRule.Level {
			ruleCopy := rule
			nextRule = &ruleCopy
		}
	}

	userMetricValue := (*string)(nil)
	if ruleType == VipRuleTypeUser {
		metric := string(userMetric)
		userMetricValue = &metric
	}

	if nextRule == nil {
		progress := 0.0
		if currentLevel > 0 {
			progress = 100
		}
		return VipLevelProgress{
			CurrentLevel:  currentLevel,
			NextLevel:     nil,
			ConditionMode: nil,
			UserMetric:    userMetricValue,
			Progress:      progress,
			IsMaxLevel:    currentLevel > 0,
			Conditions:    []VipProgressCondition{},
		}
	}

	conditions := vipGetVipProgressConditions(ruleType, *nextRule, stats, userMetric)
	progress := 0.0
	if len(conditions) > 0 {
		if nextRule.ConditionMode == string(VipConditionModeAll) {
			progress = conditions[0].Progress
			for _, condition := range conditions[1:] {
				if condition.Progress < progress {
					progress = condition.Progress
				}
			}
		} else {
			progress = conditions[0].Progress
			for _, condition := range conditions[1:] {
				if condition.Progress > progress {
					progress = condition.Progress
				}
			}
		}
	}

	nextLevel := nextRule.Level
	conditionMode := nextRule.ConditionMode
	return VipLevelProgress{
		CurrentLevel:  currentLevel,
		NextLevel:     &nextLevel,
		ConditionMode: &conditionMode,
		UserMetric:    userMetricValue,
		Progress:      progress,
		IsMaxLevel:    false,
		Conditions:    conditions,
	}
}

// ==================== persistence ====================

// GetVipRules mirrors getVipRules (ordered by level ascending).
func GetVipRules(ctx context.Context, ruleType VipRuleType) ([]NormalizedVipRule, error) {
	var rows []models.VipLevelRule
	if err := db.DB.WithContext(ctx).
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

// vipFloatToDecimalPtr converts an optional JSON number to an optional Decimal.
func vipFloatToDecimalPtr(value *float64) *models.Decimal {
	if value == nil {
		return nil
	}
	converted := models.Decimal{Decimal: decimal.NewFromFloat(*value)}
	return &converted
}

// vipUpsertUserVipMetric mirrors the systemConfig upsert on user_vip_metric.
func vipUpsertUserVipMetric(tx *gorm.DB, metric UserVipMetric) error {
	label := vipUserVipMetricLabel
	description := vipUserVipMetricDescription
	row := models.SystemConfig{
		Key:         userVipMetricConfigKey,
		Value:       string(metric),
		Type:        "string",
		Label:       &label,
		Description: &description,
	}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.Assignments(map[string]any{"value": string(metric)}),
	}).Create(&row).Error
}

// vipUpsertVipRule mirrors the vipLevelRule upsert keyed on (type, level).
func vipUpsertVipRule(tx *gorm.DB, ruleType VipRuleType, rule NormalizedVipRule) error {
	benefitsJSON, err := json.Marshal(rule.Benefits)
	if err != nil {
		return err
	}

	row := models.VipLevelRule{
		Type:          models.VipLevelRuleType(ruleType),
		Level:         int32(rule.Level),
		Enabled:       rule.Enabled,
		ConditionMode: models.VipLevelConditionMode(rule.ConditionMode),
		MinRecharge:   vipFloatToDecimalPtr(rule.MinRecharge),
		MinConsume:    vipFloatToDecimalPtr(rule.MinConsume),
		Benefits:      datatypes.JSON(benefitsJSON),
	}

	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "type"}, {Name: "level"}},
		DoUpdates: clause.Assignments(map[string]any{
			"enabled":        rule.Enabled,
			"condition_mode": rule.ConditionMode,
			"min_recharge":   row.MinRecharge,
			"min_consume":    row.MinConsume,
			"benefits":       row.Benefits,
		}),
	}).Create(&row).Error
}

// ReplaceVipRules mirrors replaceVipRules.
func ReplaceVipRules(ctx context.Context, ruleType VipRuleType, inputs []VipRuleInput, userMetric *UserVipMetric) ([]NormalizedVipRule, error) {
	if len(inputs) > MaxVipLevel {
		return nil, fmt.Errorf("At most %d VIP rules can be saved", MaxVipLevel)
	}

	var metricToSave *UserVipMetric
	if ruleType == VipRuleTypeUser {
		metricToSave = userMetric
	}

	effectiveUserMetric := UserVipMetricTotalRecharge
	if ruleType == VipRuleTypeUser {
		if metricToSave != nil {
			effectiveUserMetric = *metricToSave
		} else {
			metric, err := GetUserVipMetric(ctx)
			if err != nil {
				return nil, err
			}
			effectiveUserMetric = metric
		}
	}

	byLevel := map[int]NormalizedVipRule{}
	for _, input := range inputs {
		rule, err := SanitizeVipRuleInput(ruleType, input, effectiveUserMetric)
		if err != nil {
			return nil, err
		}
		byLevel[int(rule.Level)] = rule
	}

	levels := make([]int, 0, len(byLevel))
	for level := range byLevel {
		levels = append(levels, level)
	}
	sort.Ints(levels)

	normalizedRules := make([]NormalizedVipRule, 0, len(levels))
	for _, level := range levels {
		normalizedRules = append(normalizedRules, byLevel[level])
	}

	if err := vipValidateBenefitInstanceRefs(ctx, vipCollectBenefitInstanceRefs(normalizedRules)); err != nil {
		return nil, err
	}

	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if metricToSave != nil {
			if err := vipUpsertUserVipMetric(tx, *metricToSave); err != nil {
				return err
			}
		}

		if len(normalizedRules) == 0 {
			return tx.Where("type = ?", string(ruleType)).Delete(&models.VipLevelRule{}).Error
		}

		levelValues := make([]int32, 0, len(normalizedRules))
		for _, rule := range normalizedRules {
			levelValues = append(levelValues, int32(rule.Level))
		}
		if err := tx.Where("type = ? AND level NOT IN ?", string(ruleType), levelValues).
			Delete(&models.VipLevelRule{}).Error; err != nil {
			return err
		}

		for _, rule := range normalizedRules {
			if err := vipUpsertVipRule(tx, ruleType, rule); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if metricToSave != nil {
		lib.InvalidateCachedConfig(userVipMetricConfigKey)
	}

	return GetVipRules(ctx, ruleType)
}

// ==================== stats / overview ====================

// GetUserVipStats mirrors getUserVipStats.
func GetUserVipStats(ctx context.Context, userID int32) (UserVipStats, error) {
	consumeMap, err := db.GetUsersTotalConsumeMap(ctx, []int32{userID}, nil)
	if err != nil {
		return UserVipStats{}, err
	}

	var rechargeRow struct {
		Value models.Decimal `gorm:"column:value"`
	}
	if err := db.DB.WithContext(ctx).Raw(
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

// GetUserVipMetric mirrors getUserVipMetric.
func GetUserVipMetric(ctx context.Context) (UserVipMetric, error) {
	value, err := db.GetSystemConfig(ctx, userVipMetricConfigKey, true)
	if err != nil {
		return "", err
	}
	return vipNormalizeUserVipMetric(value), nil
}

// UpdateUserVipMetric mirrors updateUserVipMetric.
func UpdateUserVipMetric(ctx context.Context, value any) (UserVipMetric, error) {
	metric := vipNormalizeUserVipMetricAny(value)
	if err := vipUpsertUserVipMetric(db.DB.WithContext(ctx), metric); err != nil {
		return "", err
	}
	lib.InvalidateCachedConfig(userVipMetricConfigKey)
	return metric, nil
}

// CalculateUserVipLevel mirrors calculateUserVipLevel.
func CalculateUserVipLevel(ctx context.Context, userID int32) (UserVipLevelResult, error) {
	rules, err := GetVipRules(ctx, VipRuleTypeUser)
	if err != nil {
		return UserVipLevelResult{}, err
	}
	stats, err := GetUserVipStats(ctx, userID)
	if err != nil {
		return UserVipLevelResult{}, err
	}
	metric, err := GetUserVipMetric(ctx)
	if err != nil {
		return UserVipLevelResult{}, err
	}
	return UserVipLevelResult{
		Level: CalculateUserVipLevelByMetric(rules, stats, metric),
		Stats: stats,
	}, nil
}

// GetVipOverview mirrors getVipOverview.
func GetVipOverview(ctx context.Context, userID int32) (VipOverview, error) {
	userRules, err := GetVipRules(ctx, VipRuleTypeUser)
	if err != nil {
		return VipOverview{}, err
	}
	userStats, err := GetUserVipStats(ctx, userID)
	if err != nil {
		return VipOverview{}, err
	}
	userVipMetric, err := GetUserVipMetric(ctx)
	if err != nil {
		return VipOverview{}, err
	}

	userVipLevel := CalculateUserVipLevelByMetric(userRules, userStats, userVipMetric)

	return VipOverview{
		UserVipLevel:      userVipLevel,
		UserVipBadgeStyle: GetVipBadgeStyleForLevel(userRules, VipRuleTypeUser, userVipLevel),
		UserVipMetric:     userVipMetric,
		UserStats:         userStats,
		UserVipProgress:   CalculateVipProgress(VipRuleTypeUser, userRules, userVipLevel, userStats, userVipMetric),
	}, nil
}

// GetVipBadgeStyleForLevel mirrors getVipBadgeStyleForLevel.
func GetVipBadgeStyleForLevel(rules []NormalizedVipRule, ruleType VipRuleType, level float64) *VipBadgeStyle {
	if level == 0 {
		return nil
	}
	for _, rule := range rules {
		if rule.Type == string(ruleType) && rule.Level == level {
			style := rule.BadgeStyle
			return &style
		}
	}
	style := GetDefaultVipBadgeStyle(level)
	return &style
}
