// Port of server/src/routes/promo-codes.ts.
//
// app.ts registers promoCodesRoutes TWICE with the same handlers:
//
//	await fastify.register(promoCodesRoutes, { prefix: '/api/coupon' })
//	await fastify.register(promoCodesRoutes, { prefix: '/api/promos' })
//
// so every path exists under both prefixes. None of the three Fastify paths is
// '/', so prefixTrailingSlash:'both' adds nothing here; Fastify's
// ignoreTrailingSlash is false, so `/api/coupon/validate/` stays a 404 (Gin's
// trailing-slash redirect is disabled in app.go).
//
// Behavioural notes (all line-by-line with the TypeScript):
//
//   - /validate answers HTTP 200 for every business rejection (empty code,
//     invalid plan id, plan not found, invalid/disabled/expired code). Only the
//     /apply route uses 400 for those, and its body is
//     {success,error,errorCode}, not the apiError envelope.
//   - /validate and /apply read `(request.body || {})` without a JSON schema, so
//     the raw decoded values reach the handler. `Number(...)`, `||` and `??`
//     therefore behave like JavaScript, not like Go zero values.
//   - The per-instance checks run before the promo engine: instance missing or
//     deleted -> 404, not the owner -> 403, no package/plan -> 400 NOT_ELIGIBLE,
//     already bound -> 400 PROMO_BINDING_ACTIVE.
//   - `bindingStatus` compares `expiresAt <= now` (EXPIRED), while the engine's
//     validate uses `now > expiresAt`; each mirrors its own call site.
package routes

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// PromoCreateBilling mirrors `calculateCreateBilling(plan)` from
// db/billing-operations.ts. That module is not ported — it belongs to the
// instance-billing module (setup fees, expiry arithmetic, plan snapshots) — so
// this seam reports an explicit unsupported error instead of fabricating a price.
// A wrong quote is worse than a visible failure. The instance-billing module
// should assign this variable from its init() once it lands.
//
// Only `totalPrice` is consumed by this route; the TypeScript computes
// `price = Number(plan.price) / 100` with no setup fee and returns
// {plan, price, billingCycle, setupFee, totalPrice, expiresAt}.
var PromoCreateBilling = func(plan models.PackagePlan) (float64, error) {
	return 0, fmt.Errorf("calculateCreateBilling 尚未接线")
}

// The create-quote slice of db/billing-operations.ts IS ported (db/billing_operations.go
// lands CalculateCreateBilling and registers it as the quote provider), so the seam is
// wired at package-init time rather than left unsupported. The remaining
// billing-operations functions (plan changes, refunds, renewals) stay with the billing
// wave; the seam only ever needed totalPrice.
func init() {
	if quote := db.GetCreateBillingQuote(); quote != nil {
		PromoCreateBilling = quote
	}
}

// ==================== constants ====================

// Promo rejection texts, byte-identical to the TypeScript.
const (
	promoCodesEmptyCodeText   = "优惠码不能为空"
	promoCodesInvalidPlanText = "方案 ID 无效"
	promoCodesPlanNotFound    = "套餐方案不存在"
	promoCodesInstanceMissing = "实例不存在"
	promoCodesNoPermission    = "无权访问该实例"
	promoCodesApplyPermission = "无权操作该实例"
	promoCodesNotEligible     = "当前实例不支持绑定优惠码"
	promoCodesBindingActive   = "该实例已绑定其他优惠码"
	promoCodesInvalidFallback = "优惠码无效"
	promoCodesPercentOnly     = "仅支持百分比折扣优惠码绑定续费"
	promoCodesMonthsInvalid   = "续费月数无效"
	promoCodesInstanceIDWrong = "实例 ID 无效"
)

// promoCodesInstanceColumns / promoCodesInstanceApplyColumns are the instance
// projections the two routes read. The TypeScript `include`s are handled
// separately: the package plan is fetched only by /validate and /renew-preview
// (calculateCreateBilling / the monthly-price fallback need it), and the
// `include: { host: true }` of /apply is unused by that handler and therefore
// skipped. Enum columns are cast to text because every hand-written query in this
// repo does the same (Prisma uses native PG enums; GORM's typed string scan wants
// text).
const (
	promoCodesInstanceColumns      = `id, user_id, package_id, package_plan_id, status::text AS status, billing_price, billing_cycle`
	promoCodesInstanceApplyColumns = `id, user_id, package_id, package_plan_id, status::text AS status`
)

// ==================== JavaScript value helpers ====================

// promoCodesJSRuneIsSpace mirrors the WhiteSpace + LineTerminator set that
// String.prototype.trim removes (it omits U+0085 and includes U+FEFF).
func promoCodesJSRuneIsSpace(char rune) bool {
	switch char {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0x1680,
		0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return char >= 0x2000 && char <= 0x200A
}

// promoCodesJSTrim mirrors String.prototype.trim.
func promoCodesJSTrim(value string) string {
	return strings.TrimFunc(value, promoCodesJSRuneIsSpace)
}

// promoCodesTruthy mirrors JavaScript truthiness for a decoded JSON value.
func promoCodesTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case float64:
		return typed != 0 && !math.IsNaN(typed)
	case string:
		return typed != ""
	default:
		// Objects and arrays are always truthy.
		return true
	}
}

// promoCodesJSNumber mirrors `Number(value)` for a decoded JSON value.
//
// Only strings need real coercion work; ajvToJSNumber (storage_configs.go)
// already implements JavaScript's unary plus, including hex/binary/octal and
// Infinity. Arrays go through JavaScript's array-to-string coercion
// (`Number(["3"]) === 3`, `Number([1,2])` is NaN) because a schema-less body can
// carry one.
func promoCodesJSNumber(value any) float64 {
	switch typed := value.(type) {
	case nil:
		// Number(null) === 0. A missing key is `undefined` (NaN in JavaScript), but
		// both are falsy, so every caller takes the same branch.
		return 0
	case float64:
		return typed
	case bool:
		if typed {
			return 1
		}
		return 0
	case string:
		if parsed, ok := ajvToJSNumber(typed); ok {
			return parsed
		}
		return math.NaN()
	case []any:
		parts := make([]string, len(typed))
		for index, item := range typed {
			parts[index] = promoCodesJSStringElement(item)
		}
		if parsed, ok := ajvToJSNumber(strings.Join(parts, ",")); ok {
			return parsed
		}
		return math.NaN()
	default:
		return math.NaN()
	}
}

// promoCodesJSStringElement mirrors String(element) as used by Array#join.
func promoCodesJSStringElement(value any) string {
	switch typed := value.(type) {
	case nil:
		// Array.prototype.join renders null/undefined as the empty string.
		return ""
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return formatTerminalCommandNumber(typed)
	case []any:
		parts := make([]string, len(typed))
		for index, item := range typed {
			parts[index] = promoCodesJSStringElement(item)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return ""
	}
}

// promoCodesIsSafeInteger mirrors Number.isSafeInteger.
func promoCodesIsSafeInteger(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) &&
		value == math.Trunc(value) && math.Abs(value) <= 9007199254740991
}

// promoCodesBodyObject mirrors `(request.body || {})`: a non-object body has no
// properties, so field access yields undefined.
func promoCodesBodyObject(decoded any) map[string]any {
	if object, ok := decoded.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

// promoCodesJSFieldTrim mirrors `(body.<field> || ”).trim()`.
//
// ok=false is the TypeError path: a truthy non-string has no `.trim`, so the
// TypeScript throws and app.ts' generic handler answers 500.
func promoCodesJSFieldTrim(body map[string]any, field string) (string, bool) {
	raw, present := body[field]
	if !present || raw == nil {
		return "", true
	}
	if text, ok := raw.(string); ok {
		return promoCodesJSTrim(text), true
	}
	if promoCodesTruthy(raw) {
		return "", false
	}
	return "", true
}

// promoCodesRespondInternalError writes the envelope app.ts' generic error
// handler produces for an unhandled throw (production text; the Go port never
// exposes internal details).
func promoCodesRespondInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// ==================== query / id helpers ====================

// promoCodesQueryValue mirrors Fastify's query parser for one key: absent keys are
// "not present", a single occurrence is a string and repeated occurrences are an
// array (reported via isArray).
func promoCodesQueryValue(c *gin.Context, key string) (value string, present, isArray bool) {
	values, found := c.Request.URL.Query()[key]
	if !found || len(values) == 0 {
		return "", false, false
	}
	return values[0], true, len(values) > 1
}

// promoCodesNarrowID maps a JavaScript-safe route id onto the int32 column
// domain. ok=false means the id cannot exist in the table (the TypeScript passes
// it to Prisma and matches no row).
func promoCodesNarrowID(value float64) (int32, bool) {
	if value < math.MinInt32 || value > math.MaxInt32 {
		return 0, false
	}
	return int32(value), true
}

// ==================== data access ====================

// promoCodesFindPlan mirrors `prisma.packagePlan.findUnique({ where: { id } })`
// for the two columns the route needs (price is cents, billingCycle is months).
func promoCodesFindPlan(ctx context.Context, planID int32) (*models.PackagePlan, error) {
	var plan models.PackagePlan
	err := db.DB.WithContext(ctx).Raw(
		`SELECT id, package_id, price, billing_cycle FROM package_plans WHERE id = ?`, planID,
	).Scan(&plan).Error
	if err != nil {
		return nil, err
	}
	// Primary keys start at 1, so a zero id means "no row" (GORM leaves the
	// destination untouched for an empty result).
	if plan.ID == 0 {
		return nil, nil
	}
	return &plan, nil
}

// promoCodesFindInstance mirrors `prisma.instance.findUnique({ where: { id } })`
// for the given projection. nil, nil means "no row".
func promoCodesFindInstance(ctx context.Context, instanceID int32, columns string) (*models.Instance, error) {
	var instance models.Instance
	err := db.DB.WithContext(ctx).Raw(
		`SELECT `+columns+` FROM instances WHERE id = ?`, instanceID,
	).Scan(&instance).Error
	if err != nil {
		return nil, err
	}
	if instance.ID == 0 {
		return nil, nil
	}
	return &instance, nil
}

// promoCalculateMonthlyPrice mirrors calculateMonthlyPrice from
// db/billing-operations.ts:
//
//	const price = Number(instance.billingPrice) || 0
//	const cycle = Math.max(instance.billingCycle || 1, 1)
//	return price / cycle
//
// It is pure arithmetic over the instance row (no billing tables), so it is
// inlined here rather than seamed; PromoCreateBilling covers the part of that
// module that needs the real billing model.
func promoCalculateMonthlyPrice(instance *models.Instance) float64 {
	price := 0.0
	if instance.BillingPrice != nil {
		price = instance.BillingPrice.InexactFloat64()
	}
	if math.IsNaN(price) {
		price = 0
	}

	cycle := 1.0
	if instance.BillingCycle != nil && *instance.BillingCycle != 0 {
		cycle = float64(*instance.BillingCycle)
	}
	if cycle < 1 {
		cycle = 1
	}

	return price / cycle
}

// promoCodesISOTime renders a timestamp exactly like Prisma's JSON Date
// (Date#toISOString): UTC, millisecond precision, trailing Z.
func promoCodesISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// ==================== response types ====================

type promoCodesRenewOption struct {
	Months           int     `json:"months"`
	OriginalPrice    float64 `json:"originalPrice"`
	DiscountAmount   float64 `json:"discountAmount"`
	FinalPrice       float64 `json:"finalPrice"`
	DiscountedMonths int     `json:"discountedMonths"`
	RegularMonths    int     `json:"regularMonths"`
	WillUnbind       bool    `json:"willUnbind"`
}

type promoCodesBindingView struct {
	Code            string                   `json:"code"`
	DiscountValue   float64                  `json:"discountValue"`
	DurationType    models.PromoDurationType `json:"durationType"`
	RemainingCycles *int32                   `json:"remainingCycles"`
	Enabled         bool                     `json:"enabled"`
	ExpiresAt       *string                  `json:"expiresAt"`
}

type promoCodesApplyBindingView struct {
	ID              int32                    `json:"id"`
	InstanceID      int32                    `json:"instanceId"`
	PromoCodeID     int32                    `json:"promoCodeId"`
	DurationType    models.PromoDurationType `json:"durationType"`
	TotalCycles     *int32                   `json:"totalCycles"`
	UsedCycles      int32                    `json:"usedCycles"`
	RemainingCycles *int32                   `json:"remainingCycles"`
}

// ==================== pure response builders ====================

// promoCodesBindingStatus mirrors the `bindingStatus` ternary chain: ACTIVE,
// DISABLED, EXPIRED or null when there is no binding. `expiresAt <= now` counts
// as expired, matching this route's `<=` (the engine compares with `>`).
func promoCodesBindingStatus(binding *db.PromoInstanceBindingWithCode, now time.Time) *string {
	if binding == nil {
		return nil
	}

	status := "ACTIVE"
	if !binding.PromoCode.Enabled {
		status = "DISABLED"
	} else if binding.PromoCode.ExpiresAt != nil && !binding.PromoCode.ExpiresAt.After(now) {
		status = "EXPIRED"
	}
	return &status
}

// promoCodesIsPromoActive mirrors
// `Boolean(binding && enabled && (!expiresAt || expiresAt > now))`.
func promoCodesIsPromoActive(binding *db.PromoInstanceBindingWithCode, now time.Time) bool {
	if binding == nil || !binding.PromoCode.Enabled {
		return false
	}
	return binding.PromoCode.ExpiresAt == nil || binding.PromoCode.ExpiresAt.After(now)
}

// promoCodesRenewOptions mirrors the `monthList.map(...)` block: with an active
// promo binding every month is quoted through the renewal splitter, otherwise the
// option is a plain full-price quote.
func promoCodesRenewOptions(monthlyPrice float64, monthsList []int, isPromoActive bool, binding *db.PromoInstanceBindingWithCode) []promoCodesRenewOption {
	options := make([]promoCodesRenewOption, 0, len(monthsList))
	for _, months := range monthsList {
		originalPrice := db.PromoMoneyFixed2(monthlyPrice * float64(months))

		if isPromoActive && binding != nil {
			quote := services.PromoQuoteRenewal(monthlyPrice, months, binding)
			options = append(options, promoCodesRenewOption{
				Months:           months,
				OriginalPrice:    originalPrice,
				DiscountAmount:   quote.DiscountAmount,
				FinalPrice:       quote.FinalAmount,
				DiscountedMonths: quote.DiscountedMonths,
				RegularMonths:    quote.RegularMonths,
				WillUnbind:       quote.WillUnbind,
			})
			continue
		}

		options = append(options, promoCodesRenewOption{
			Months:           months,
			OriginalPrice:    originalPrice,
			DiscountAmount:   0,
			FinalPrice:       originalPrice,
			DiscountedMonths: 0,
			RegularMonths:    months,
			WillUnbind:       false,
		})
	}
	return options
}

// promoCodesRenewBindingView mirrors the nested `binding: binding ? {...} : null`.
func promoCodesRenewBindingView(binding *db.PromoInstanceBindingWithCode) *promoCodesBindingView {
	if binding == nil {
		return nil
	}

	var expiresAt *string
	if binding.PromoCode.ExpiresAt != nil {
		formatted := promoCodesISOTime(*binding.PromoCode.ExpiresAt)
		expiresAt = &formatted
	}

	return &promoCodesBindingView{
		Code:            binding.PromoCode.Code,
		DiscountValue:   binding.PromoCode.DiscountValue.InexactFloat64(),
		DurationType:    binding.DurationType,
		RemainingCycles: binding.RemainingCycles,
		Enabled:         binding.PromoCode.Enabled,
		ExpiresAt:       expiresAt,
	}
}

// ==================== handlers ====================

// promoCodesValidateHandler mirrors POST /validate.
func promoCodesValidateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	body := promoCodesBodyObject(decoded.Value)

	trimmedCode, ok := promoCodesJSFieldTrim(body, "code")
	if !ok {
		promoCodesRespondInternalError(c)
		return
	}
	if trimmedCode == "" {
		c.JSON(http.StatusOK, gin.H{
			"valid":     false,
			"error":     promoCodesEmptyCodeText,
			"errorCode": "PROMO_NOT_FOUND",
		})
		return
	}

	// `let packagePlanId = Number(body.packagePlanId)`, then
	// `if (!packagePlanId || !Number.isSafeInteger(packagePlanId))`.
	packagePlanID := math.NaN()
	if rawPlanID, present := body["packagePlanId"]; present {
		packagePlanID = promoCodesJSNumber(rawPlanID)
	}
	if packagePlanID == 0 || !promoCodesIsSafeInteger(packagePlanID) {
		c.JSON(http.StatusOK, gin.H{
			"valid":     false,
			"error":     promoCodesInvalidPlanText,
			"errorCode": "INVALID_PARAMS",
		})
		return
	}

	var plan *models.PackagePlan
	planID, planIDExists := promoCodesNarrowID(packagePlanID)
	if planIDExists {
		loaded, err := promoCodesFindPlan(ctx, planID)
		if err != nil {
			promoCodesRespondInternalError(c)
			return
		}
		plan = loaded
	}

	// `let packageId = Number(body.packageId)`; a valid value must match the plan,
	// otherwise the plan's own packageId is used. Both TypeScript branches answer
	// PLAN_NOT_FOUND when the plan is missing, so that check runs first.
	packageID := math.NaN()
	if rawPackageID, present := body["packageId"]; present {
		packageID = promoCodesJSNumber(rawPackageID)
	}

	if plan == nil {
		c.JSON(http.StatusOK, gin.H{
			"valid":     false,
			"error":     promoCodesPlanNotFound,
			"errorCode": "PLAN_NOT_FOUND",
		})
		return
	}

	// `if (packageId && Number.isSafeInteger(packageId))` — JavaScript truthiness
	// rejects NaN and 0 as well.
	packageIDGiven := !math.IsNaN(packageID) && packageID != 0 && promoCodesIsSafeInteger(packageID)

	var effectivePackageID int32
	if packageIDGiven {
		if float64(plan.PackageID) != packageID {
			c.JSON(http.StatusOK, gin.H{
				"valid":     false,
				"error":     promoCodesPlanNotFound,
				"errorCode": "PLAN_NOT_FOUND",
			})
			return
		}
		// packageID equals an int32 column value, so it is representable.
		effectivePackageID = int32(packageID)
	} else {
		effectivePackageID = plan.PackageID
	}

	validation, err := services.PromoValidate(ctx, services.PromoValidateParams{
		Code:          trimmedCode,
		PackageID:     effectivePackageID,
		PackagePlanID: planID,
		UserID:        user.ID,
	})
	if err != nil {
		promoCodesRespondInternalError(c)
		return
	}

	if !validation.Valid || validation.PromoCode == nil {
		errorText := validation.Error
		if errorText == "" {
			errorText = promoCodesInvalidFallback
		}
		errorCode := validation.ErrorCode
		if errorCode == "" {
			errorCode = "PROMO_INVALID"
		}
		c.JSON(http.StatusOK, gin.H{
			"valid":     false,
			"error":     errorText,
			"errorCode": errorCode,
		})
		return
	}

	totalPrice, err := PromoCreateBilling(*plan)
	if err != nil {
		// The billing seam is unsupported; never invent a price.
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	promoCode := validation.PromoCode
	quote := services.PromoQuoteCreation(totalPrice, services.PromoQuotePromo{
		DiscountType:  promoCode.DiscountType,
		DiscountValue: promoCode.DiscountValue,
	})

	discountRate := 0.0
	if totalPrice > 0 {
		discountRate = quote.DiscountAmount / totalPrice
	}

	c.JSON(http.StatusOK, gin.H{
		"valid":             true,
		"code":              promoCode.Code,
		"discountType":      promoCode.DiscountType,
		"discountValue":     promoCode.DiscountValue.InexactFloat64(),
		"durationType":      promoCode.DurationType,
		"durationCycles":    promoCode.DurationCycles,
		"discountRate":      discountRate,
		"commissionRate":    promoCode.CommissionRate.InexactFloat64(),
		"estimatedDiscount": quote.DiscountAmount,
		"finalPrice":        quote.FinalPrice,
	})
}

// promoCodesRenewPreviewHandler mirrors GET /renew-preview/:instanceId.
func promoCodesRenewPreviewHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	rawInstanceID, ok := lib.ParsePositiveRouteID(c.Param("instanceId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, promoCodesInstanceIDWrong)
		return
	}
	instanceID, idExists := promoCodesNarrowID(float64(rawInstanceID))
	if !idExists {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, promoCodesInstanceMissing)
		return
	}

	// `if (query.months !== undefined)` — a present but malformed value (including
	// an empty one) is a 400, not a fallback.
	rawMonths, monthsPresent, monthsIsArray := promoCodesQueryValue(c, "months")
	var monthsList []int
	if monthsPresent {
		monthValue, parseOK := lib.ParsePositiveRouteID(rawMonths)
		if monthsIsArray || !parseOK || monthValue <= 0 {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, promoCodesMonthsInvalid)
			return
		}
		monthsList = []int{int(monthValue)}
	} else {
		monthsList = []int{1, 3, 6, 12}
	}

	instance, err := promoCodesFindInstance(ctx, instanceID, promoCodesInstanceColumns)
	if err != nil {
		promoCodesRespondInternalError(c)
		return
	}
	if instance == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, promoCodesInstanceMissing)
		return
	}

	if user.Role != "admin" && instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, promoCodesNoPermission)
		return
	}

	monthlyPrice := promoCalculateMonthlyPrice(instance)
	if monthlyPrice <= 0 && instance.PackagePlanID != nil {
		plan, err := promoCodesFindPlan(ctx, *instance.PackagePlanID)
		if err != nil {
			promoCodesRespondInternalError(c)
			return
		}
		if plan != nil {
			// `(Number(plan.price) / 100) / Math.max(plan.billingCycle || 1, 1)`
			monthlyPrice = (plan.Price.InexactFloat64() / 100) / math.Max(float64(plan.BillingCycle), 1)
		}
	}

	binding, err := db.PromoGetInstanceBinding(ctx, instanceID, nil)
	if err != nil {
		promoCodesRespondInternalError(c)
		return
	}

	now := time.Now().UTC()
	active := promoCodesIsPromoActive(binding, now)

	c.JSON(http.StatusOK, gin.H{
		"instanceId":    instanceID,
		"hasBinding":    binding != nil,
		"isPromoActive": active,
		"bindingStatus": promoCodesBindingStatus(binding, now),
		"binding":       promoCodesRenewBindingView(binding),
		"options":       promoCodesRenewOptions(monthlyPrice, monthsList, active, binding),
	})
}

// promoCodesApplyHandler mirrors POST /apply/:instanceId.
func promoCodesApplyHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	rawInstanceID, ok := lib.ParsePositiveRouteID(c.Param("instanceId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, promoCodesInstanceIDWrong)
		return
	}
	instanceID, idExists := promoCodesNarrowID(float64(rawInstanceID))
	if !idExists {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, promoCodesInstanceMissing)
		return
	}

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	body := promoCodesBodyObject(decoded.Value)

	trimmedCode, ok := promoCodesJSFieldTrim(body, "code")
	if !ok {
		promoCodesRespondInternalError(c)
		return
	}
	if trimmedCode == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success":   false,
			"error":     promoCodesEmptyCodeText,
			"errorCode": "PROMO_NOT_FOUND",
		})
		return
	}

	instance, err := promoCodesFindInstance(ctx, instanceID, promoCodesInstanceApplyColumns)
	if err != nil {
		promoCodesRespondInternalError(c)
		return
	}
	if instance == nil || instance.Status == models.InstanceStatusDeleted {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, promoCodesInstanceMissing)
		return
	}

	if user.Role != "admin" && instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, promoCodesApplyPermission)
		return
	}

	// `if (!instance.packageId || !instance.packagePlanId)`
	if instance.PackageID == nil || *instance.PackageID == 0 ||
		instance.PackagePlanID == nil || *instance.PackagePlanID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"success":   false,
			"error":     promoCodesNotEligible,
			"errorCode": "NOT_ELIGIBLE",
		})
		return
	}

	existingBinding, err := db.PromoGetInstanceBinding(ctx, instanceID, nil)
	if err != nil {
		promoCodesRespondInternalError(c)
		return
	}
	if existingBinding != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success":   false,
			"error":     promoCodesBindingActive,
			"errorCode": "PROMO_BINDING_ACTIVE",
		})
		return
	}

	validation, err := services.PromoValidate(ctx, services.PromoValidateParams{
		Code:             trimmedCode,
		PackageID:        *instance.PackageID,
		PackagePlanID:    *instance.PackagePlanID,
		UserID:           user.ID,
		TargetInstanceID: &instanceID,
	})
	if err != nil {
		promoCodesRespondInternalError(c)
		return
	}

	if !validation.Valid || validation.PromoCode == nil {
		errorText := validation.Error
		if errorText == "" {
			errorText = promoCodesInvalidFallback
		}
		errorCode := validation.ErrorCode
		if errorCode == "" {
			errorCode = "PROMO_INVALID"
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"success":   false,
			"error":     errorText,
			"errorCode": errorCode,
		})
		return
	}

	if validation.DiscountType == nil || *validation.DiscountType != models.PromoDiscountTypePERCENTAGE {
		c.JSON(http.StatusBadRequest, gin.H{
			"success":   false,
			"error":     promoCodesPercentOnly,
			"errorCode": "INVALID_DISCOUNT_TYPE",
		})
		return
	}

	if validation.DurationType == nil {
		// Unreachable for a valid result; guard instead of dereferencing nil.
		promoCodesRespondInternalError(c)
		return
	}
	durationType := *validation.DurationType

	// `durationType === 'REPEATING' ? (durationCycles || 1) : (ONCE ? 1 : null)`
	var totalCycles *int32
	switch durationType {
	case models.PromoDurationTypeREPEATING:
		cycles := int32(1)
		if validation.DurationCycles != nil && *validation.DurationCycles != 0 {
			cycles = *validation.DurationCycles
		}
		totalCycles = &cycles
	case models.PromoDurationTypeONCE:
		once := int32(1)
		totalCycles = &once
	default:
		totalCycles = nil
	}

	// `const remainingCycles = totalCycles`
	var remainingCycles *int32
	if totalCycles != nil {
		copied := *totalCycles
		remainingCycles = &copied
	}

	binding, err := db.PromoBindToInstance(ctx, nil, db.PromoBindParams{
		InstanceID:      instanceID,
		PromoCodeID:     validation.PromoCode.ID,
		DurationType:    durationType,
		TotalCycles:     totalCycles,
		UsedCycles:      0,
		RemainingCycles: remainingCycles,
	})
	if err != nil {
		promoCodesRespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"binding": promoCodesApplyBindingView{
			ID:              binding.ID,
			InstanceID:      binding.InstanceID,
			PromoCodeID:     binding.PromoCodeID,
			DurationType:    binding.DurationType,
			TotalCycles:     binding.TotalCycles,
			UsedCycles:      binding.UsedCycles,
			RemainingCycles: binding.RemainingCycles,
		},
	})
}

// ==================== registration ====================

// RegisterPromoCodes mounts the promo-code routes under both prefixes app.ts uses
// for this module.
func RegisterPromoCodes(r *gin.Engine) {
	for _, prefix := range []string{"/api/coupon", "/api/promos"} {
		group := r.Group(prefix)
		group.POST("/validate", middleware.Authenticate(), promoCodesValidateHandler)
		group.GET("/renew-preview/:instanceId", middleware.Authenticate(), promoCodesRenewPreviewHandler)
		group.POST("/apply/:instanceId", middleware.Authenticate(), promoCodesApplyHandler)
	}
}

func init() {
	Register(RegisterPromoCodes)
}
