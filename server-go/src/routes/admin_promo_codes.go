// Port of server/src/routes/admin-promo-codes.ts (7 routes).
//
// app.ts registers this plugin TWICE with the same handlers:
//
//	await fastify.register(adminPromoCodesRoutes, { prefix: '/api/admin/coupon' })
//	await fastify.register(adminPromoCodesRoutes, { prefix: '/api/admin/promos' })
//
// so every path exists under both prefixes. The module's root route is declared as
// '/' and Fastify's `prefixTrailingSlash` default is 'both', so BOTH
// `/api/admin/promos` and `/api/admin/promos/` answer (measured on the live Node
// server: both return 200). The same is true for /api/admin/coupon. Gin's
// trailing-slash redirect is disabled in app.go, so each root route is registered
// twice, exactly like routes/help.go.
//
// Parity notes:
//   - Every body is read without a JSON schema, so the handlers see raw decoded
//     JSON. The helpers below reproduce JavaScript's Number()/Boolean()/typeof/
//     String#trim semantics instead of Go zero values.
//   - Validation failures use the TypeScript's hand-written payload shape
//     `{error: <message>, code: <CODE>}` — NOT the apiError catalog envelope.
//   - PUT /:id has no "empty update" early return: Prisma always writes
//     @updatedAt, so the Go UPDATE always runs (and always bumps updated_at).
//   - DELETE /:id notifies every still-bound instance through the inbox. The
//     TypeScript does not await those promises; they run in goroutines here too.
//   - `request.user?.id ?? null` for adminId: the requireAdmin guard guarantees a
//     user, so adminId is always that id.
//   - Prisma's `include: { scopes: true }` has no orderBy (heap order). The Go port
//     pins it with `ORDER BY id ASC`, which is the insertion order the nested create
//     produces, so the array cannot shuffle between calls.
package routes

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
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
)

// ==================== wire types ====================

type adminPromoCodesScopeView struct {
	ID            int32  `json:"id"`
	PackageID     *int32 `json:"packageId"`
	PackagePlanID *int32 `json:"packagePlanId"`
}

type adminPromoCodesItemView struct {
	ID                  int32                      `json:"id"`
	Code                string                     `json:"code"`
	Name                *string                    `json:"name"`
	Type                models.PromoCodeType       `json:"type"`
	DiscountType        models.PromoDiscountType   `json:"discountType"`
	DiscountValue       float64                    `json:"discountValue"`
	DurationType        models.PromoDurationType   `json:"durationType"`
	DurationCycles      *int32                     `json:"durationCycles"`
	IsGlobal            bool                       `json:"isGlobal"`
	Scopes              []adminPromoCodesScopeView `json:"scopes"`
	MaxTotalUses        *int32                     `json:"maxTotalUses"`
	UsedTotalCount      int32                      `json:"usedTotalCount"`
	MaxUsesPerUser      *int32                     `json:"maxUsesPerUser"`
	StartsAt            *string                    `json:"startsAt"`
	ExpiresAt           *string                    `json:"expiresAt"`
	Enabled             bool                       `json:"enabled"`
	CreatedAt           string                     `json:"createdAt"`
	TotalDiscountAmount float64                    `json:"totalDiscountAmount"`
	// ActiveBindingsCount is only present on the list and update responses; the
	// create response omits the key entirely (`omitempty` on the pointer).
	ActiveBindingsCount *int64 `json:"activeBindingsCount,omitempty"`
}

// adminPromoCodesAdminInstanceView is one row of GET /:id/instances.
type adminPromoCodesAdminInstanceView struct {
	InstanceID      int32  `json:"instanceId"`
	InstanceName    string `json:"instanceName"`
	UserID          int32  `json:"userId"`
	Username        string `json:"username"`
	TotalCycles     *int32 `json:"totalCycles"`
	UsedCycles      int32  `json:"usedCycles"`
	RemainingCycles *int32 `json:"remainingCycles"`
	BoundAt         string `json:"boundAt"`
}

// ==================== error helpers ====================

// adminPromoCodesError mirrors `reply.code(status).send({ error, code })`.
func adminPromoCodesError(c *gin.Context, status int, message, code string) {
	c.JSON(status, gin.H{"error": message, "code": code})
}

// adminPromoCodesRespondInternal mirrors app.ts' generic error handler for an
// unhandled throw (production text; internal details are never exposed).
func adminPromoCodesRespondInternal(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// ==================== JavaScript value helpers ====================

// adminPromoCodesQueryRaw returns the raw query value. present=false means the key
// is absent (JavaScript `undefined`); isArray mirrors Fastify's array value for a
// repeated key.
func adminPromoCodesQueryNumber(c *gin.Context, key string) float64 {
	value, present, isArray := promoCodesQueryValue(c, key)
	if !present {
		return math.NaN()
	}
	if isArray {
		// A repeated key stringifies with commas, so Number() is NaN.
		return math.NaN()
	}
	parsed, ok := ajvToJSNumber(value)
	if !ok {
		return math.NaN()
	}
	return parsed
}

// adminPromoCodesQueryText mirrors `(request.query.<key> || ”).trim()`. ok=false
// is the TypeError path: a repeated key yields an array, which has no .trim, so
// the TypeScript throws and the global handler answers 500.
func adminPromoCodesQueryText(c *gin.Context, key string) (string, bool) {
	value, present, isArray := promoCodesQueryValue(c, key)
	if !present {
		return "", true
	}
	if isArray {
		return "", false
	}
	return promoCodesJSTrim(value), true
}

// adminPromoCodesQueryEquals mirrors `query.<key> === expected`.
func adminPromoCodesQueryEquals(c *gin.Context, key, expected string) bool {
	value, present, isArray := promoCodesQueryValue(c, key)
	return present && !isArray && value == expected
}

// adminPromoCodesParsePositiveInteger mirrors parsePositiveInteger.
func adminPromoCodesParsePositiveInteger(value float64, fallback int64) int64 {
	if !promoCodesIsSafeInteger(value) || value <= 0 {
		return fallback
	}
	return int64(value)
}

// adminPromoCodesParseClampedPositiveInteger mirrors parseClampedPositiveInteger.
func adminPromoCodesParseClampedPositiveInteger(value float64, fallback, max int64) int64 {
	if !promoCodesIsSafeInteger(value) || value < 1 {
		return fallback
	}
	if value > float64(max) {
		return max
	}
	return int64(value)
}

// adminPromoCodesRouteID parses a route id through lib.ParsePositiveRouteID and
// narrows it to the INT4 column domain (out-of-range ids cannot exist).
func adminPromoCodesRouteID(value string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok || parsed > math.MaxInt32 {
		return 0, false
	}
	return int32(parsed), true
}

// adminPromoCodesStringValue mirrors `typeof value === 'string'`.
func adminPromoCodesStringValue(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok
}

// adminPromoCodesTrimmedOrNull mirrors
// `typeof value === 'string' && value.trim() ? value.trim() : null`.
func adminPromoCodesTrimmedOrNull(value any) *string {
	text, ok := adminPromoCodesStringValue(value)
	if !ok {
		return nil
	}
	trimmed := promoCodesJSTrim(text)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// adminPromoCodesDecimalFromNumber mirrors `new Prisma.Decimal(number)` for a
// finite double: decimal.js keeps the shortest round-trip representation, which is
// exactly strconv.FormatFloat(v, 'f', -1, 64). PostgreSQL's numeric(10,2) column
// then applies the same rounding Prisma relies on.
func adminPromoCodesDecimalFromNumber(value float64) (models.Decimal, error) {
	parsed, err := decimal.NewFromString(strconv.FormatFloat(value, 'f', -1, 64))
	if err != nil {
		return models.Decimal{}, err
	}
	return models.Decimal{Decimal: parsed}, nil
}

// adminPromoCodesNarrowInt32 narrows a JavaScript safe integer onto the INT4
// column domain. ok=false is the Prisma validation error a too-large value would
// produce (a 500 at the global handler).
func adminPromoCodesNarrowInt32(value float64) (int32, bool) {
	if value > math.MaxInt32 || value < math.MinInt32 {
		return 0, false
	}
	return int32(value), true
}

// adminPromoCodesParseJSDate mirrors `new Date(value)` for the body forms a
// schema-less route can carry: a string (ISO / date-time / date-only) or a number
// (epoch milliseconds). A boolean is coerced to 0/1 ms by JavaScript, which is
// preserved. Everything else (`{}`, `[]`, NaN) is an Invalid Date.
func adminPromoCodesParseJSDate(value any) (*time.Time, bool) {
	switch typed := value.(type) {
	case string:
		for _, layout := range []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05",
			"2006-01-02T15:04",
			"2006-01-02 15:04:05",
			"2006-01-02",
		} {
			if parsed, err := time.Parse(layout, typed); err == nil {
				utc := parsed.UTC().Truncate(time.Millisecond)
				return &utc, true
			}
		}
		return nil, false
	case float64:
		return adminPromoCodesDateFromMillis(typed)
	case bool:
		if typed {
			return adminPromoCodesDateFromMillis(1)
		}
		return adminPromoCodesDateFromMillis(0)
	default:
		return nil, false
	}
}

// adminPromoCodesDateFromMillis mirrors `new Date(number)`: the value is epoch
// milliseconds and the valid range is ±8.64e15 ms (ECMAScript TimeClip).
func adminPromoCodesDateFromMillis(milliseconds float64) (*time.Time, bool) {
	if math.IsNaN(milliseconds) || math.IsInf(milliseconds, 0) || math.Abs(milliseconds) > 8.64e15 {
		return nil, false
	}
	seconds := math.Trunc(milliseconds / 1000)
	remainder := milliseconds - seconds*1000
	moment := time.Unix(int64(seconds), int64(remainder*float64(time.Millisecond))).UTC()
	return &moment, true
}

// adminPromoCodesFormatJSValue mirrors String(value) as a template literal renders
// it: undefined -> "undefined", null -> "null", numbers via the JS number
// formatter, objects as "[object Object]".
func adminPromoCodesFormatJSValue(value any, present bool) string {
	if !present {
		return "undefined"
	}
	switch typed := value.(type) {
	case nil:
		return "null"
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
	default:
		return "[object Object]"
	}
}

// ==================== response builders ====================

// adminPromoCodesItemsView mirrors the `items` mapping shared by list/create/update.
func adminPromoCodesItemsView(code models.PromoCode, scopes []models.PromoCodeScope, bindingsCount *int64) adminPromoCodesItemView {
	scopeViews := make([]adminPromoCodesScopeView, 0, len(scopes))
	for _, scope := range scopes {
		scopeViews = append(scopeViews, adminPromoCodesScopeView{
			ID:            scope.ID,
			PackageID:     scope.PackageID,
			PackagePlanID: scope.PackagePlanID,
		})
	}

	var startsAt *string
	if code.StartsAt != nil {
		formatted := promoCodesISOTime(*code.StartsAt)
		startsAt = &formatted
	}
	var expiresAt *string
	if code.ExpiresAt != nil {
		formatted := promoCodesISOTime(*code.ExpiresAt)
		expiresAt = &formatted
	}

	return adminPromoCodesItemView{
		ID:                  code.ID,
		Code:                code.Code,
		Name:                code.Name,
		Type:                code.Type,
		DiscountType:        code.DiscountType,
		DiscountValue:       code.DiscountValue.InexactFloat64(),
		DurationType:        code.DurationType,
		DurationCycles:      code.DurationCycles,
		IsGlobal:            code.IsGlobal,
		Scopes:              scopeViews,
		MaxTotalUses:        code.MaxTotalUses,
		UsedTotalCount:      code.UsedTotalCount,
		MaxUsesPerUser:      code.MaxUsesPerUser,
		StartsAt:            startsAt,
		ExpiresAt:           expiresAt,
		Enabled:             code.Enabled,
		CreatedAt:           promoCodesISOTime(code.CreatedAt),
		TotalDiscountAmount: code.TotalDiscountAmount.InexactFloat64(),
		ActiveBindingsCount: bindingsCount,
	}
}

// ==================== data access ====================

// adminPromoCodesColumns is the wire-relevant promo_codes projection. Enum columns
// are cast to text so GORM's typed string scan works (same convention as
// db/promo_codes.go).
const adminPromoCodesColumns = `id, code, name, type::text AS type, user_id, admin_id, is_global,
	discount_type::text AS discount_type, discount_value, commission_rate,
	duration_type::text AS duration_type, duration_cycles, max_total_uses, used_total_count,
	max_uses_per_user, starts_at, expires_at, enabled, total_discount_amount, total_earnings,
	created_at, updated_at`

type adminPromoCodesListFilters struct {
	Search  string
	Type    models.PromoCodeType
	Enabled *bool
}

// adminPromoCodesQueryList mirrors the findMany/count pair of GET /.
func adminPromoCodesQueryList(ctx context.Context, filters adminPromoCodesListFilters, page, pageSize int64) ([]models.PromoCode, int64, error) {
	// GORM's Raw() only substitutes `?` placeholders (it appends any leftover vars
	// as extra placeholders), so every query below uses `?` and relies on the
	// postgres dialector to render `$n`.
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if filters.Search != "" {
		conditions = append(conditions,
			"(code ILIKE '%' || ? || '%' OR name ILIKE '%' || ? || '%')")
		args = append(args, filters.Search, filters.Search)
	}
	if filters.Type != "" {
		conditions = append(conditions, "type = ?")
		args = append(args, string(filters.Type))
	}
	if filters.Enabled != nil {
		conditions = append(conditions, "enabled = ?")
		args = append(args, *filters.Enabled)
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	var total int64
	if err := db.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM promo_codes `+where, args...).Scan(&total).Error; err != nil {
		return nil, 0, err
	}

	listArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	var codes []models.PromoCode
	if err := db.DB.WithContext(ctx).Raw(`SELECT `+adminPromoCodesColumns+` FROM promo_codes `+where+
		` ORDER BY created_at DESC LIMIT ? OFFSET ?`,
		listArgs...).Scan(&codes).Error; err != nil {
		return nil, 0, err
	}
	if codes == nil {
		codes = []models.PromoCode{}
	}
	return codes, total, nil
}

// adminPromoCodesLoadScopes mirrors the `include: { scopes: true }` relation read.
func adminPromoCodesLoadScopes(ctx context.Context, promoCodeIDs []int32) (map[int32][]models.PromoCodeScope, error) {
	scopes := map[int32][]models.PromoCodeScope{}
	if len(promoCodeIDs) == 0 {
		return scopes, nil
	}

	rows, err := db.Pool().Query(ctx, `SELECT id, promo_code_id, package_id, package_plan_id
		FROM promo_code_scopes WHERE promo_code_id = ANY($1) ORDER BY id ASC`, promoCodeIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var scope models.PromoCodeScope
		if err := rows.Scan(&scope.ID, &scope.PromoCodeID, &scope.PackageID, &scope.PackagePlanID); err != nil {
			return nil, err
		}
		scopes[scope.PromoCodeID] = append(scopes[scope.PromoCodeID], scope)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return scopes, nil
}

// adminPromoCodesLoadBindingCounts mirrors `_count: { select: { bindings: true } }`.
func adminPromoCodesLoadBindingCounts(ctx context.Context, promoCodeIDs []int32) (map[int32]int64, error) {
	counts := map[int32]int64{}
	if len(promoCodeIDs) == 0 {
		return counts, nil
	}

	rows, err := db.Pool().Query(ctx, `SELECT promo_code_id, COUNT(*)
		FROM instance_promo_bindings WHERE promo_code_id = ANY($1) GROUP BY promo_code_id`, promoCodeIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id    int32
			count int64
		)
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

// adminPromoCodesFindByCode mirrors `prisma.promoCode.findUnique({ where: { code } })`.
func adminPromoCodesFindByCode(ctx context.Context, code string) (*models.PromoCode, error) {
	var row models.PromoCode
	result := db.DB.WithContext(ctx).Raw(`SELECT `+adminPromoCodesColumns+` FROM promo_codes WHERE code = ?`, code).Scan(&row)
	if result.Error != nil {
		return nil, result.Error
	}
	if row.ID == 0 {
		return nil, nil
	}
	return &row, nil
}

// adminPromoCodesFindByID mirrors `prisma.promoCode.findUnique({ where: { id } })`
// without includes.
func adminPromoCodesFindByID(ctx context.Context, id int32) (*models.PromoCode, error) {
	var row models.PromoCode
	result := db.DB.WithContext(ctx).Raw(`SELECT `+adminPromoCodesColumns+` FROM promo_codes WHERE id = ?`, id).Scan(&row)
	if result.Error != nil {
		return nil, result.Error
	}
	if row.ID == 0 {
		return nil, nil
	}
	return &row, nil
}

// ==================== handlers ====================

// adminPromoCodesListHandler mirrors GET /.
func adminPromoCodesListHandler(c *gin.Context) {
	ctx := c.Request.Context()

	page := adminPromoCodesParsePositiveInteger(adminPromoCodesQueryNumber(c, "page"), 1)
	pageSize := adminPromoCodesParseClampedPositiveInteger(adminPromoCodesQueryNumber(c, "pageSize"), 20, 100)

	search, ok := adminPromoCodesQueryText(c, "search")
	if !ok {
		adminPromoCodesRespondInternal(c)
		return
	}

	filters := adminPromoCodesListFilters{Search: search}
	if adminPromoCodesQueryEquals(c, "type", string(models.PromoCodeTypeAFFUSER)) {
		filters.Type = models.PromoCodeTypeAFFUSER
	} else if adminPromoCodesQueryEquals(c, "type", string(models.PromoCodeTypeADMINPROMO)) {
		filters.Type = models.PromoCodeTypeADMINPROMO
	}
	if adminPromoCodesQueryEquals(c, "enabled", "true") {
		enabled := true
		filters.Enabled = &enabled
	} else if adminPromoCodesQueryEquals(c, "enabled", "false") {
		enabled := false
		filters.Enabled = &enabled
	}

	codes, total, err := adminPromoCodesQueryList(ctx, filters, page, pageSize)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}

	ids := make([]int32, 0, len(codes))
	for _, code := range codes {
		ids = append(ids, code.ID)
	}
	scopes, err := adminPromoCodesLoadScopes(ctx, ids)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}
	bindingCounts, err := adminPromoCodesLoadBindingCounts(ctx, ids)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}

	items := make([]adminPromoCodesItemView, 0, len(codes))
	for _, code := range codes {
		count := bindingCounts[code.ID]
		items = append(items, adminPromoCodesItemsView(code, scopes[code.ID], &count))
	}

	totalPages := int64(0)
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}

	c.JSON(http.StatusOK, gin.H{
		"promos":     items,
		"items":      items,
		"total":      total,
		"page":       page,
		"pageSize":   pageSize,
		"totalPages": totalPages,
	})
}

// adminPromoCodesCreateHandler mirrors POST /.
func adminPromoCodesCreateHandler(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	body := promoCodesBodyObject(decoded.Value)

	rawCode, codePresent := body["code"]
	codeText, codeIsString := adminPromoCodesStringValue(rawCode)
	if !codePresent || codeText == "" || !codeIsString {
		adminPromoCodesError(c, http.StatusBadRequest, "优惠码不能为空", "INVALID_PARAMS")
		return
	}
	code := strings.ToUpper(promoCodesJSTrim(codeText))
	if length := jsStringLength(code); length < 3 || length > 32 {
		adminPromoCodesError(c, http.StatusBadRequest, "优惠码长度需在 3-32 字符之间", "INVALID_PARAMS")
		return
	}

	name := adminPromoCodesTrimmedOrNull(body["name"])

	discountType := models.PromoDiscountTypePERCENTAGE
	if raw, present := body["discountType"]; present {
		if text, ok := raw.(string); ok && text == "FIXED_AMOUNT" {
			discountType = models.PromoDiscountTypeFIXEDAMOUNT
		}
	}

	rawDiscount, discountPresent := body["discountValue"]
	discountValue, discountIsNumber := rawDiscount.(float64)
	if !discountPresent || !discountIsNumber || math.IsNaN(discountValue) || math.IsInf(discountValue, 0) || discountValue <= 0 {
		adminPromoCodesError(c, http.StatusBadRequest, "折扣数值必须大于 0", "INVALID_PARAMS")
		return
	}
	if discountType == models.PromoDiscountTypePERCENTAGE && discountValue > 1.0 {
		adminPromoCodesError(c, http.StatusBadRequest, "百分比折扣数值必须在 0 到 1.0 之间", "INVALID_PARAMS")
		return
	}

	durationType := models.PromoDurationTypeFOREVER
	if discountType == models.PromoDiscountTypeFIXEDAMOUNT {
		durationType = models.PromoDurationTypeONCE
	} else if raw, present := body["durationType"]; present {
		if text, ok := raw.(string); ok {
			switch models.PromoDurationType(text) {
			case models.PromoDurationTypeONCE, models.PromoDurationTypeREPEATING, models.PromoDurationTypeFOREVER:
				durationType = models.PromoDurationType(text)
			}
		}
	}

	var durationCycles *int32
	if durationType == models.PromoDurationTypeREPEATING {
		cycles := math.NaN()
		if raw, present := body["durationCycles"]; present {
			cycles = promoCodesJSNumber(raw)
		}
		if !promoCodesIsSafeInteger(cycles) || cycles < 1 {
			adminPromoCodesError(c, http.StatusBadRequest, "重复周期数必须为大于等于 1 的整数", "INVALID_PARAMS")
			return
		}
		value, fits := adminPromoCodesNarrowInt32(cycles)
		if !fits {
			adminPromoCodesRespondInternal(c)
			return
		}
		durationCycles = &value
	}

	isGlobal := false
	if raw, present := body["isGlobal"]; present {
		isGlobal = promoCodesTruthy(raw)
	}

	validatedScopes := make([]adminPromoCodesScopeParams, 0, 4)
	if !isGlobal {
		rawScopes, present := body["scopes"]
		scopeItems, isArray := rawScopes.([]any)
		if !present || !isArray || len(scopeItems) == 0 {
			adminPromoCodesError(c, http.StatusBadRequest, "非全局优惠码必须指定至少一个适用范围", "INVALID_PARAMS")
			return
		}
		for _, item := range scopeItems {
			if item == nil {
				// `scope.packageId` on null/undefined throws in the TypeScript.
				adminPromoCodesRespondInternal(c)
				return
			}
			scope, _ := item.(map[string]any)

			packageID := math.NaN()
			if raw, present := scope["packageId"]; present {
				packageID = promoCodesJSNumber(raw)
			}
			if !promoCodesIsSafeInteger(packageID) || packageID <= 0 {
				adminPromoCodesError(c, http.StatusBadRequest, "适用范围套餐 ID 无效", "INVALID_PARAMS")
				return
			}
			packageValue, fits := adminPromoCodesNarrowInt32(packageID)
			if !fits {
				adminPromoCodesRespondInternal(c)
				return
			}

			var packagePlanID *int32
			if raw, present := scope["packagePlanId"]; present && raw != nil {
				planID := promoCodesJSNumber(raw)
				if !promoCodesIsSafeInteger(planID) || planID <= 0 {
					adminPromoCodesError(c, http.StatusBadRequest, "适用范围套餐方案 ID 无效", "INVALID_PARAMS")
					return
				}
				planValue, planFits := adminPromoCodesNarrowInt32(planID)
				if !planFits {
					adminPromoCodesRespondInternal(c)
					return
				}
				packagePlanID = &planValue
			}

			validatedScopes = append(validatedScopes, adminPromoCodesScopeParams{
				PackageID:     packageValue,
				PackagePlanID: packagePlanID,
			})
		}
	}

	var maxTotalUses *int32
	if raw, present := body["maxTotalUses"]; present && raw != nil {
		parsed := promoCodesJSNumber(raw)
		if !promoCodesIsSafeInteger(parsed) || parsed <= 0 {
			adminPromoCodesError(c, http.StatusBadRequest, "总使用次数必须为正整数", "INVALID_PARAMS")
			return
		}
		value, fits := adminPromoCodesNarrowInt32(parsed)
		if !fits {
			adminPromoCodesRespondInternal(c)
			return
		}
		maxTotalUses = &value
	}

	maxUsesPerUserValue := int32(1)
	if raw, present := body["maxUsesPerUser"]; present && raw != nil {
		parsed := promoCodesJSNumber(raw)
		if !promoCodesIsSafeInteger(parsed) || parsed <= 0 {
			adminPromoCodesError(c, http.StatusBadRequest, "单用户最大使用次数必须为正整数", "INVALID_PARAMS")
			return
		}
		value, fits := adminPromoCodesNarrowInt32(parsed)
		if !fits {
			adminPromoCodesRespondInternal(c)
			return
		}
		maxUsesPerUserValue = value
	}
	maxUsesPerUser := &maxUsesPerUserValue

	var startsAt, expiresAt *time.Time
	if raw, present := body["startsAt"]; present && promoCodesTruthy(raw) {
		parsed, ok := adminPromoCodesParseJSDate(raw)
		if !ok {
			adminPromoCodesError(c, http.StatusBadRequest, "开始时间格式无效", "INVALID_PARAMS")
			return
		}
		startsAt = parsed
	}
	if raw, present := body["expiresAt"]; present && promoCodesTruthy(raw) {
		parsed, ok := adminPromoCodesParseJSDate(raw)
		if !ok {
			adminPromoCodesError(c, http.StatusBadRequest, "过期时间格式无效", "INVALID_PARAMS")
			return
		}
		expiresAt = parsed
	}
	if startsAt != nil && expiresAt != nil && !startsAt.Before(*expiresAt) {
		adminPromoCodesError(c, http.StatusBadRequest, "开始时间必须早于过期时间", "INVALID_PARAMS")
		return
	}

	enabled := true
	if raw, present := body["enabled"]; present {
		enabled = promoCodesTruthy(raw)
	}

	existing, err := adminPromoCodesFindByCode(ctx, code)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}
	if existing != nil {
		adminPromoCodesError(c, http.StatusConflict, "优惠码已存在", "PROMO_CODE_EXISTS")
		return
	}

	discountDecimal, err := adminPromoCodesDecimalFromNumber(discountValue)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}

	adminID := user.ID
	created, scopes, err := adminPromoCodesCreate(ctx, adminPromoCodesCreateParams{
		Code:           code,
		Name:           name,
		AdminID:        &adminID,
		IsGlobal:       isGlobal,
		DiscountType:   discountType,
		DiscountValue:  discountDecimal,
		DurationType:   durationType,
		DurationCycles: durationCycles,
		MaxTotalUses:   maxTotalUses,
		MaxUsesPerUser: maxUsesPerUser,
		StartsAt:       startsAt,
		ExpiresAt:      expiresAt,
		Enabled:        enabled,
		Scopes:         validatedScopes,
	})
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}

	c.JSON(http.StatusCreated, adminPromoCodesItemsView(created, scopes, nil))
}

// adminPromoCodesUpdateHandler mirrors PUT /:id.
func adminPromoCodesUpdateHandler(c *gin.Context) {
	ctx := c.Request.Context()

	id, ok := adminPromoCodesRouteID(c.Param("id"))
	if !ok {
		adminPromoCodesError(c, http.StatusBadRequest, "无效的优惠码 ID", "INVALID_ID")
		return
	}

	existing, err := db.PromoGetCodeByID(ctx, id, nil)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}
	if existing == nil {
		adminPromoCodesError(c, http.StatusNotFound, "优惠码不存在", "PROMO_NOT_FOUND")
		return
	}

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	body := promoCodesBodyObject(decoded.Value)

	// 1. Immutable fields, checked in the TypeScript's order.
	if raw, present := body["code"]; present {
		if text, isString := raw.(string); isString {
			if strings.ToUpper(promoCodesJSTrim(text)) != existing.Code {
				adminPromoCodesError(c, http.StatusBadRequest, "优惠券码创建后不允许修改", "PROMO_FIELD_IMMUTABLE")
				return
			}
		}
	}

	if raw, present := body["discountType"]; present {
		text, isString := raw.(string)
		if !isString || text != string(existing.DiscountType) {
			adminPromoCodesError(c, http.StatusBadRequest, "折扣类型创建后不允许修改", "PROMO_FIELD_IMMUTABLE")
			return
		}
	}

	if raw, present := body["discountValue"]; present {
		if promoCodesJSNumber(raw) != existing.DiscountValue.InexactFloat64() {
			adminPromoCodesError(c, http.StatusBadRequest, "折扣百分比/数值创建后不允许修改", "PROMO_FIELD_IMMUTABLE")
			return
		}
	}

	if raw, present := body["durationType"]; present {
		text, isString := raw.(string)
		if !isString || text != string(existing.DurationType) {
			adminPromoCodesError(c, http.StatusBadRequest, "有效周期模式创建后不允许修改", "PROMO_FIELD_IMMUTABLE")
			return
		}
	}

	if raw, present := body["durationCycles"]; present {
		var newCycles *float64
		if raw != nil {
			value := promoCodesJSNumber(raw)
			newCycles = &value
		}
		var oldCycles *float64
		if existing.DurationCycles != nil {
			value := float64(*existing.DurationCycles)
			oldCycles = &value
		}
		different := newCycles != nil && (oldCycles == nil || *newCycles != *oldCycles)
		if newCycles == nil && oldCycles != nil {
			different = true
		}
		if different {
			adminPromoCodesError(c, http.StatusBadRequest, "有效周期模式创建后不允许修改", "PROMO_FIELD_IMMUTABLE")
			return
		}
	}

	if raw, present := body["isGlobal"]; present {
		if promoCodesTruthy(raw) != existing.IsGlobal {
			adminPromoCodesError(c, http.StatusBadRequest, "适用范围创建后不允许修改", "PROMO_FIELD_IMMUTABLE")
			return
		}
	}

	if raw, present := body["scopes"]; present {
		scopeItems, isArray := raw.([]any)
		if !isArray {
			// `body.scopes.map` is not a function in the TypeScript.
			adminPromoCodesRespondInternal(c)
			return
		}
		existingScopes := make([]string, 0, len(existing.Scopes))
		for _, scope := range existing.Scopes {
			var packageValue any
			if scope.PackageID != nil {
				packageValue = float64(*scope.PackageID)
			}
			packagePart := adminPromoCodesFormatJSValue(packageValue, true)
			planPart := ""
			if scope.PackagePlanID != nil {
				planPart = adminPromoCodesFormatJSValue(float64(*scope.PackagePlanID), true)
			}
			existingScopes = append(existingScopes, packagePart+":"+planPart)
		}
		newScopes := make([]string, 0, len(scopeItems))
		for _, item := range scopeItems {
			scope, _ := item.(map[string]any)
			rawPackage, packagePresent := scope["packageId"]
			packagePart := adminPromoCodesFormatJSValue(rawPackage, packagePresent)
			planPart := ""
			if rawPlan, present := scope["packagePlanId"]; present && rawPlan != nil {
				planPart = adminPromoCodesFormatJSValue(rawPlan, true)
			}
			newScopes = append(newScopes, packagePart+":"+planPart)
		}
		sort.Strings(existingScopes)
		sort.Strings(newScopes)
		if len(existingScopes) != len(newScopes) {
			adminPromoCodesError(c, http.StatusBadRequest, "适用范围创建后不允许修改", "PROMO_FIELD_IMMUTABLE")
			return
		}
		for index := range existingScopes {
			if existingScopes[index] != newScopes[index] {
				adminPromoCodesError(c, http.StatusBadRequest, "适用范围创建后不允许修改", "PROMO_FIELD_IMMUTABLE")
				return
			}
		}
	}

	// 2. Mutable fields.
	var (
		updateParams = adminPromoCodesUpdateParams{ID: id}
		nameSet      bool
		nameValue    *string
	)
	if raw, present := body["name"]; present {
		nameValue = adminPromoCodesTrimmedOrNull(raw)
		nameSet = true
	}
	if nameSet {
		updateParams.NameSet = true
		updateParams.Name = nameValue
	}

	if raw, present := body["maxTotalUses"]; present {
		updateParams.MaxTotalUsesSet = true
		if raw == nil {
			updateParams.MaxTotalUses = nil
		} else {
			parsed := promoCodesJSNumber(raw)
			if !promoCodesIsSafeInteger(parsed) || parsed <= 0 {
				adminPromoCodesError(c, http.StatusBadRequest, "总使用次数必须为正整数", "INVALID_PARAMS")
				return
			}
			value, fits := adminPromoCodesNarrowInt32(parsed)
			if !fits {
				adminPromoCodesRespondInternal(c)
				return
			}
			updateParams.MaxTotalUses = &value
		}
	}

	if raw, present := body["maxUsesPerUser"]; present {
		updateParams.MaxUsesPerUserSet = true
		if raw == nil {
			updateParams.MaxUsesPerUser = nil
		} else {
			parsed := promoCodesJSNumber(raw)
			if !promoCodesIsSafeInteger(parsed) || parsed <= 0 {
				adminPromoCodesError(c, http.StatusBadRequest, "单用户最大使用次数必须为正整数", "INVALID_PARAMS")
				return
			}
			value, fits := adminPromoCodesNarrowInt32(parsed)
			if !fits {
				adminPromoCodesRespondInternal(c)
				return
			}
			updateParams.MaxUsesPerUser = &value
		}
	}

	if raw, present := body["startsAt"]; present {
		updateParams.StartsAtSet = true
		if raw == nil {
			updateParams.StartsAt = nil
		} else if text, isString := raw.(string); isString && text == "" {
			updateParams.StartsAt = nil
		} else {
			parsed, ok := adminPromoCodesParseJSDate(raw)
			if !ok {
				adminPromoCodesError(c, http.StatusBadRequest, "开始时间格式无效", "INVALID_PARAMS")
				return
			}
			updateParams.StartsAt = parsed
		}
	}

	if raw, present := body["expiresAt"]; present {
		updateParams.ExpiresAtSet = true
		if raw == nil {
			updateParams.ExpiresAt = nil
		} else if text, isString := raw.(string); isString && text == "" {
			updateParams.ExpiresAt = nil
		} else {
			parsed, ok := adminPromoCodesParseJSDate(raw)
			if !ok {
				adminPromoCodesError(c, http.StatusBadRequest, "过期时间格式无效", "INVALID_PARAMS")
				return
			}
			updateParams.ExpiresAt = parsed
		}
	}

	effectiveStartsAt := existing.StartsAt
	if updateParams.StartsAtSet {
		effectiveStartsAt = updateParams.StartsAt
	}
	effectiveExpiresAt := existing.ExpiresAt
	if updateParams.ExpiresAtSet {
		effectiveExpiresAt = updateParams.ExpiresAt
	}
	if effectiveStartsAt != nil && effectiveExpiresAt != nil && !effectiveStartsAt.Before(*effectiveExpiresAt) {
		adminPromoCodesError(c, http.StatusBadRequest, "开始时间必须早于过期时间", "INVALID_PARAMS")
		return
	}

	if raw, present := body["enabled"]; present {
		updateParams.EnabledSet = true
		enabled := promoCodesTruthy(raw)
		updateParams.Enabled = &enabled
	}

	updated, err := adminPromoCodesUpdate(ctx, updateParams)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}

	counts, err := adminPromoCodesLoadBindingCounts(ctx, []int32{updated.ID})
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}
	count := counts[updated.ID]

	c.JSON(http.StatusOK, adminPromoCodesItemsView(updated, existing.Scopes, &count))
}

// adminPromoCodesToggleHandler mirrors PATCH /:id/toggle.
func adminPromoCodesToggleHandler(c *gin.Context) {
	ctx := c.Request.Context()

	id, ok := adminPromoCodesRouteID(c.Param("id"))
	if !ok {
		adminPromoCodesError(c, http.StatusBadRequest, "无效的优惠码 ID", "INVALID_ID")
		return
	}

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}

	var (
		enabled       bool
		enabledIsBool bool
	)
	body, isObject := decoded.Value.(map[string]any)
	if isObject {
		if value, present := body["enabled"]; present {
			enabled, enabledIsBool = value.(bool)
		}
	}
	if !isObject || !enabledIsBool {
		adminPromoCodesError(c, http.StatusBadRequest, "enabled 字段必须为布尔值", "INVALID_PARAMS")
		return
	}

	existing, err := adminPromoCodesFindByID(ctx, id)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}
	if existing == nil {
		adminPromoCodesError(c, http.StatusNotFound, "优惠码不存在", "PROMO_NOT_FOUND")
		return
	}

	updated, err := adminPromoCodesSetEnabled(ctx, id, enabled)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "enabled": updated.Enabled})
}

// adminPromoCodesDeleteHandler mirrors DELETE /:id.
func adminPromoCodesDeleteHandler(c *gin.Context) {
	ctx := c.Request.Context()

	id, ok := adminPromoCodesRouteID(c.Param("id"))
	if !ok {
		adminPromoCodesError(c, http.StatusBadRequest, "无效的优惠码 ID", "INVALID_ID")
		return
	}

	promo, err := adminPromoCodesFindByID(ctx, id)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}
	if promo == nil {
		adminPromoCodesError(c, http.StatusNotFound, "优惠码不存在", "PROMO_NOT_FOUND")
		return
	}

	affected, err := adminPromoCodesDeleteWithBindings(ctx, id)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}

	// After the transaction commits, notify affected instances (fire and forget).
	for _, binding := range affected {
		if binding.InstanceID == 0 {
			continue
		}
		adminPromoCodesSendCodeDeletedNotification(binding.UserID, binding.InstanceID, binding.InstanceName, promo.Code)
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "affectedInstances": len(affected)})
}

// adminPromoCodesInstancesHandler mirrors GET /:id/instances.
func adminPromoCodesInstancesHandler(c *gin.Context) {
	ctx := c.Request.Context()

	id, ok := adminPromoCodesRouteID(c.Param("id"))
	if !ok {
		adminPromoCodesError(c, http.StatusBadRequest, "无效的优惠码 ID", "INVALID_ID")
		return
	}

	promo, err := adminPromoCodesFindByID(ctx, id)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}
	if promo == nil {
		adminPromoCodesError(c, http.StatusNotFound, "优惠码不存在", "PROMO_NOT_FOUND")
		return
	}

	page := adminPromoCodesParsePositiveInteger(adminPromoCodesQueryNumber(c, "page"), 1)
	pageSize := adminPromoCodesParseClampedPositiveInteger(adminPromoCodesQueryNumber(c, "pageSize"), 20, 100)

	rows, total, err := adminPromoCodesQueryBindings(ctx, id, page, pageSize)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}

	totalPages := int64(0)
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}

	c.JSON(http.StatusOK, gin.H{
		"instances":  rows,
		"items":      rows,
		"total":      total,
		"page":       page,
		"pageSize":   pageSize,
		"totalPages": totalPages,
	})
}

// adminPromoCodesUnbindHandler mirrors POST /:id/unbind/:instanceId.
func adminPromoCodesUnbindHandler(c *gin.Context) {
	ctx := c.Request.Context()

	id, ok := adminPromoCodesRouteID(c.Param("id"))
	instanceID, instanceOK := adminPromoCodesRouteID(c.Param("instanceId"))
	if !ok || !instanceOK {
		adminPromoCodesError(c, http.StatusBadRequest, "无效的路由参数 ID", "INVALID_ID")
		return
	}

	binding, err := adminPromoCodesUnbind(ctx, id, instanceID)
	if err != nil {
		adminPromoCodesRespondInternal(c)
		return
	}
	if binding == nil {
		adminPromoCodesError(c, http.StatusNotFound, "未找到该实例的优惠码绑定", "BINDING_NOT_FOUND")
		return
	}

	if binding.InstanceID != 0 {
		adminPromoCodesSendUnboundByAdminNotification(binding.UserID, binding.InstanceID, binding.InstanceName, binding.PromoCode)
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "message": "实例已解绑，已发送站内信通知"})
}

// ==================== mutations ====================

// adminPromoCodesScopeParams is one validated scope row.
type adminPromoCodesScopeParams struct {
	PackageID     int32
	PackagePlanID *int32
}

// adminPromoCodesCreateParams mirrors the `data` object of the create transaction.
type adminPromoCodesCreateParams struct {
	Code           string
	Name           *string
	AdminID        *int32
	IsGlobal       bool
	DiscountType   models.PromoDiscountType
	DiscountValue  models.Decimal
	DurationType   models.PromoDurationType
	DurationCycles *int32
	MaxTotalUses   *int32
	MaxUsesPerUser *int32
	StartsAt       *time.Time
	ExpiresAt      *time.Time
	Enabled        bool
	Scopes         []adminPromoCodesScopeParams
}

// adminPromoCodesUpdateParams mirrors `Prisma.PromoCodeUpdateInput`: the Set flags
// distinguish `undefined` (skip) from an explicit null.
type adminPromoCodesUpdateParams struct {
	ID                int32
	NameSet           bool
	Name              *string
	MaxTotalUsesSet   bool
	MaxTotalUses      *int32
	MaxUsesPerUserSet bool
	MaxUsesPerUser    *int32
	StartsAtSet       bool
	StartsAt          *time.Time
	ExpiresAtSet      bool
	ExpiresAt         *time.Time
	EnabledSet        bool
	Enabled           *bool
}

// adminPromoCodesAffectedBinding mirrors
// `instancePromoBinding.findMany({ include: { instance: { select: {...} } } })`.
type adminPromoCodesAffectedBinding struct {
	InstanceID   int32
	InstanceName string
	UserID       int32
}

// adminPromoCodesCreate mirrors the `$transaction(tx => tx.promoCode.create({
// data: {... scopes: { create: [...] } }, include: { scopes: true } }))` call.
func adminPromoCodesCreate(ctx context.Context, params adminPromoCodesCreateParams) (models.PromoCode, []models.PromoCodeScope, error) {
	var (
		created models.PromoCode
		scopes  = []models.PromoCodeScope{}
	)

	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Raw(`INSERT INTO promo_codes
			(code, name, type, admin_id, is_global, discount_type, discount_value,
			 duration_type, duration_cycles, max_total_uses, max_uses_per_user,
			 starts_at, expires_at, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(), NOW())
			RETURNING `+adminPromoCodesColumns,
			params.Code, params.Name, string(models.PromoCodeTypeADMINPROMO), params.AdminID,
			params.IsGlobal, string(params.DiscountType), params.DiscountValue,
			string(params.DurationType), params.DurationCycles, params.MaxTotalUses,
			params.MaxUsesPerUser, params.StartsAt, params.ExpiresAt, params.Enabled,
		).Scan(&created).Error; err != nil {
			return err
		}

		if len(params.Scopes) == 0 {
			return nil
		}

		args := make([]any, 0, len(params.Scopes)*3)
		values := make([]string, 0, len(params.Scopes))
		for _, scope := range params.Scopes {
			args = append(args, created.ID, scope.PackageID, scope.PackagePlanID)
			values = append(values, "(?, ?, ?)")
		}
		if err := tx.Exec(`INSERT INTO promo_code_scopes (promo_code_id, package_id, package_plan_id) VALUES `+
			strings.Join(values, ", "), args...).Error; err != nil {
			return err
		}
		return tx.Raw(`SELECT id, promo_code_id, package_id, package_plan_id
			FROM promo_code_scopes WHERE promo_code_id = ? ORDER BY id ASC`, created.ID).Scan(&scopes).Error
	})
	return created, scopes, err
}

// adminPromoCodesUpdate mirrors `prisma.promoCode.update({ where: { id }, data })`.
// Prisma always writes updated_at (its @updatedAt column), even for an empty data
// object, so no field-emptiness early return exists here.
func adminPromoCodesUpdate(ctx context.Context, params adminPromoCodesUpdateParams) (models.PromoCode, error) {
	sets := make([]string, 0, 8)
	args := make([]any, 0, 9)
	add := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = ?")
	}
	if params.NameSet {
		add("name", params.Name)
	}
	if params.MaxTotalUsesSet {
		add("max_total_uses", params.MaxTotalUses)
	}
	if params.MaxUsesPerUserSet {
		add("max_uses_per_user", params.MaxUsesPerUser)
	}
	if params.StartsAtSet {
		add("starts_at", params.StartsAt)
	}
	if params.ExpiresAtSet {
		add("expires_at", params.ExpiresAt)
	}
	if params.EnabledSet {
		add("enabled", *params.Enabled)
	}
	add("updated_at", time.Now().UTC())

	args = append(args, params.ID)
	var updated models.PromoCode
	if err := db.DB.WithContext(ctx).Raw(`UPDATE promo_codes SET `+strings.Join(sets, ", ")+
		` WHERE id = ? RETURNING `+adminPromoCodesColumns, args...).Scan(&updated).Error; err != nil {
		return models.PromoCode{}, err
	}
	if updated.ID == 0 {
		return models.PromoCode{}, db.ErrPromoNotFound
	}
	return updated, nil
}

// adminPromoCodesSetEnabled mirrors `prisma.promoCode.update({ data: { enabled } })`.
func adminPromoCodesSetEnabled(ctx context.Context, id int32, enabled bool) (models.PromoCode, error) {
	var updated models.PromoCode
	if err := db.DB.WithContext(ctx).Raw(`UPDATE promo_codes SET enabled = ?, updated_at = NOW()
		WHERE id = ? RETURNING `+adminPromoCodesColumns, enabled, id).Scan(&updated).Error; err != nil {
		return models.PromoCode{}, err
	}
	if updated.ID == 0 {
		return models.PromoCode{}, db.ErrPromoNotFound
	}
	return updated, nil
}

// adminPromoCodesDeleteWithBindings mirrors the DELETE /:id transaction: read the
// affected bindings (with their instance), then delete bindings, scopes and the
// promo code itself.
func adminPromoCodesDeleteWithBindings(ctx context.Context, id int32) ([]adminPromoCodesAffectedBinding, error) {
	affected := []adminPromoCodesAffectedBinding{}

	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []struct {
			InstanceID   int32   `gorm:"column:instance_id"`
			InstanceName *string `gorm:"column:instance_name"`
			UserID       *int32  `gorm:"column:user_id"`
		}
		if err := tx.Raw(`SELECT b.instance_id, i.name AS instance_name, i.user_id
			FROM instance_promo_bindings b
			LEFT JOIN instances i ON i.id = b.instance_id
			WHERE b.promo_code_id = ?`, id).Scan(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			binding := adminPromoCodesAffectedBinding{InstanceID: row.InstanceID}
			if row.InstanceName != nil {
				binding.InstanceName = *row.InstanceName
			}
			if row.UserID != nil {
				binding.UserID = *row.UserID
			}
			affected = append(affected, binding)
		}

		if err := tx.Exec(`DELETE FROM instance_promo_bindings WHERE promo_code_id = ?`, id).Error; err != nil {
			return err
		}
		if err := tx.Exec(`DELETE FROM promo_code_scopes WHERE promo_code_id = ?`, id).Error; err != nil {
			return err
		}
		return tx.Exec(`DELETE FROM promo_codes WHERE id = ?`, id).Error
	})
	return affected, err
}

// adminPromoCodesQueryBindings mirrors GET /:id/instances' findMany/count pair.
func adminPromoCodesQueryBindings(ctx context.Context, promoCodeID int32, page, pageSize int64) ([]adminPromoCodesAdminInstanceView, int64, error) {
	var total int64
	if err := db.DB.WithContext(ctx).Raw(`SELECT COUNT(*) FROM instance_promo_bindings WHERE promo_code_id = ?`,
		promoCodeID).Scan(&total).Error; err != nil {
		return nil, 0, err
	}

	var rows []struct {
		InstanceID      int32     `gorm:"column:instance_id"`
		InstanceName    *string   `gorm:"column:instance_name"`
		UserID          *int32    `gorm:"column:user_id"`
		Username        *string   `gorm:"column:username"`
		TotalCycles     *int32    `gorm:"column:total_cycles"`
		UsedCycles      int32     `gorm:"column:used_cycles"`
		RemainingCycles *int32    `gorm:"column:remaining_cycles"`
		BoundAt         time.Time `gorm:"column:bound_at"`
	}
	if err := db.DB.WithContext(ctx).Raw(`SELECT b.instance_id, i.name AS instance_name, i.user_id,
			u.username, b.total_cycles, b.used_cycles, b.remaining_cycles, b.bound_at
		FROM instance_promo_bindings b
		LEFT JOIN instances i ON i.id = b.instance_id
		LEFT JOIN users u ON u.id = i.user_id
		WHERE b.promo_code_id = ?
		ORDER BY b.bound_at DESC
		LIMIT ? OFFSET ?`, promoCodeID, pageSize, (page-1)*pageSize).Scan(&rows).Error; err != nil {
		return nil, 0, err
	}

	instances := make([]adminPromoCodesAdminInstanceView, 0, len(rows))
	for _, row := range rows {
		view := adminPromoCodesAdminInstanceView{
			InstanceID:      row.InstanceID,
			UsedCycles:      row.UsedCycles,
			TotalCycles:     row.TotalCycles,
			RemainingCycles: row.RemainingCycles,
			BoundAt:         promoCodesISOTime(row.BoundAt),
		}
		if row.InstanceName != nil {
			view.InstanceName = *row.InstanceName
		}
		if row.UserID != nil {
			view.UserID = *row.UserID
		}
		if row.Username != nil {
			view.Username = *row.Username
		}
		instances = append(instances, view)
	}
	return instances, total, nil
}

// adminPromoCodesUnbindTarget is the binding row returned by the unbind transaction.
type adminPromoCodesUnbindTarget struct {
	InstanceID   int32
	InstanceName string
	UserID       int32
	PromoCode    string
}

// adminPromoCodesUnbind mirrors POST /:id/unbind/:instanceId's transaction:
// findFirst the binding, then unbindPromoFromInstance(tx, instanceId).
func adminPromoCodesUnbind(ctx context.Context, promoCodeID, instanceID int32) (*adminPromoCodesUnbindTarget, error) {
	var target *adminPromoCodesUnbindTarget
	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []struct {
			InstanceID   int32   `gorm:"column:instance_id"`
			InstanceName *string `gorm:"column:instance_name"`
			UserID       *int32  `gorm:"column:user_id"`
			PromoCode    string  `gorm:"column:promo_code"`
		}
		if err := tx.Raw(`SELECT b.instance_id, i.name AS instance_name, i.user_id, p.code AS promo_code
			FROM instance_promo_bindings b
			LEFT JOIN instances i ON i.id = b.instance_id
			JOIN promo_codes p ON p.id = b.promo_code_id
			WHERE b.promo_code_id = ? AND b.instance_id = ?
			LIMIT 1`, promoCodeID, instanceID).Scan(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		row := rows[0]
		found := &adminPromoCodesUnbindTarget{InstanceID: row.InstanceID, PromoCode: row.PromoCode}
		if row.InstanceName != nil {
			found.InstanceName = *row.InstanceName
		}
		if row.UserID != nil {
			found.UserID = *row.UserID
		}
		if err := tx.Exec(`DELETE FROM instance_promo_bindings WHERE instance_id = ?`, instanceID).Error; err != nil {
			return err
		}
		target = found
		return nil
	})
	return target, err
}

// ==================== notification helpers ====================

// adminPromoCodesSendCodeDeletedNotification mirrors
// sendPromoCodeDeletedNotification (services/promo/notifications.ts): a
// fire-and-forget inbox message whose failure is only logged.
func adminPromoCodesSendCodeDeletedNotification(userID, instanceID int32, instanceName, promoCode string) {
	go func() {
		_, err := db.CreateInboxMessage(context.Background(), db.CreateInboxMessageData{
			UserID:    userID,
			EventType: "promo_code_deleted",
			Title:     "【优惠调整】您使用的优惠码已下线终止",
			Content: fmt.Sprintf("尊敬的用户：\n您在实例 **%s**（ID: #%d）上所享受的优惠码 **%s** 已由平台官方正式下线并终止优惠策略。\n该实例后续续费将恢复为标准官方价格。感谢您的理解与支持。",
				instanceName, instanceID, promoCode),
			Data: map[string]any{
				"instanceId":   instanceID,
				"instanceName": instanceName,
				"promoCode":    promoCode,
			},
		})
		if err != nil {
			fmt.Printf("[Promo Notification] Failed to send promo_code_deleted notification: %v\n", err)
		}
	}()
}

// adminPromoCodesSendUnboundByAdminNotification mirrors
// sendPromoUnboundByAdminNotification.
func adminPromoCodesSendUnboundByAdminNotification(userID, instanceID int32, instanceName, promoCode string) {
	go func() {
		_, err := db.CreateInboxMessage(context.Background(), db.CreateInboxMessageData{
			UserID:    userID,
			EventType: "promo_binding_unbound_by_admin",
			Title:     "【优惠变更】您的实例已解除优惠码绑定",
			Content: fmt.Sprintf("尊敬的用户：\n您的实例 **%s**（ID: #%d）绑定的优惠码 **%s** 已由系统管理员解除绑定。\n即日起该实例将不再享受此优惠码的续费折扣，后续续费将按标准资费计费。如有疑问，请通过工单系统联系客服支持。",
				instanceName, instanceID, promoCode),
			Data: map[string]any{
				"instanceId":   instanceID,
				"instanceName": instanceName,
				"promoCode":    promoCode,
			},
		})
		if err != nil {
			fmt.Printf("[Promo Notification] Failed to send promo_binding_unbound_by_admin notification: %v\n", err)
		}
	}()
}

// ==================== registration ====================

// RegisterAdminPromoCodes mounts the admin promo-code routes under both prefixes
// app.ts registers this module with.
func RegisterAdminPromoCodes(r *gin.Engine) {
	for _, prefix := range []string{"/api/admin/coupon", "/api/admin/promos"} {
		group := r.Group(prefix, middleware.AuthenticateAdmin())
		group.GET("", adminPromoCodesListHandler)
		group.GET("/", adminPromoCodesListHandler)
		group.POST("", adminPromoCodesCreateHandler)
		group.POST("/", adminPromoCodesCreateHandler)
		group.PUT("/:id", adminPromoCodesUpdateHandler)
		group.PATCH("/:id/toggle", adminPromoCodesToggleHandler)
		group.DELETE("/:id", adminPromoCodesDeleteHandler)
		group.GET("/:id/instances", adminPromoCodesInstancesHandler)
		group.POST("/:id/unbind/:instanceId", adminPromoCodesUnbindHandler)
	}
}

func init() {
	Register(RegisterAdminPromoCodes)
}
