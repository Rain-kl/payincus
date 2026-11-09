// Port of server/src/routes/aff.ts (AFF referral programme routes).
//
// Mounting: app.ts registers `affRoutes` with `{ prefix: '/api/aff' }` and no
// `prefixTrailingSlash` override. The plugin has no root ('/') route, so the
// default 'both' behaviour adds nothing: every path is a plain child of
// /api/aff. `ignoreTrailingSlash` is false in Fastify and Gin's
// RedirectTrailingSlash is disabled in app.go, so `/api/aff/me/` is a 404 on both
// sides.
//
// Behavioural notes, all line-by-line with the TypeScript:
//
//   - None of the routes has a JSON schema, so the raw decoded body reaches the
//     handler. The handlers destructure `request.body` without a fallback, so a
//     missing or JSON-null body is a TypeError in Node and is answered with the
//     generic 500 here (affRespondInternalError).
//   - The per-route `config: { rateLimit: ... }` blocks are dead code in Node:
//     @fastify/rate-limit installs its onRoute hook on the plugin context before
//     app.ts' hook is added, so only the global 300/min limit applies. config/
//     rate_limit_gen.go has no /api/aff rule for the same reason, so both sides
//     enforce exactly the global limit.
//   - `packagePlanId` submitted as a JSON number outside the int32 column range
//     is treated as "no such row" (the same convention routes/promo_codes.go
//     documents): Prisma would reject the value instead, which is not
//     reproducible byte-for-byte.
//   - GET /me/codes uses `||` (not `??`) for planName / packageName / price, so
//     an empty name or a zero price crosses the wire as null.
package routes

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// ==================== constants ====================

// AFF_ROUTE_MAX_PAGE_SIZE / AFF_REJECT_REASON_MAX_LENGTH.
const (
	affRouteMaxPageSize      = 100
	affRejectReasonMaxLength = 500
	affMaxSafeInteger        = 9007199254740991
	affDefaultPageSize       = 20
)

// Rejection texts, byte-identical to the TypeScript.
const (
	affNotActivatedText     = "推荐计划尚未激活，请先充值任意金额"
	affInvalidPlanIDText    = "方案 ID 无效"
	affPlanNotFoundText     = "套餐方案不存在"
	affEmptyCodeText        = "请输入优惠码"
	affCodeFallbackNotFound = "优惠码不存在"
	affInvalidCodeText      = "优惠码无效"
	affInvalidAmountText    = "请输入有效的转化金额"
	affMissingRejectReason  = "请填写拒绝原因"
	affWithdrawalNotFound   = "申请不存在"
	affApproveMessage       = "审核通过，已转入用户余额"
	affRejectMessage        = "已拒绝"
	affDeleteCodeMessage    = "优惠码已删除"
	affConvertMessage       = "转化成功，已转入账户余额"
	affCreateGlobalMessage  = "全局优惠码创建成功"
	affCreatePlanMessage    = "优惠码创建成功"

	affApproveInboxTitle = "AFF 转化审核通过"
	affRejectInboxTitle  = "AFF 转化申请被拒绝"
)

// ==================== JavaScript helpers ====================

// affJSRuneIsSpace mirrors the WhiteSpace + LineTerminator set that
// String.prototype.trim removes.
func affJSRuneIsSpace(char rune) bool {
	switch char {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0x1680,
		0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return char >= 0x2000 && char <= 0x200A
}

// affJSTrim mirrors String.prototype.trim.
func affJSTrim(value string) string {
	return strings.TrimFunc(value, affJSRuneIsSpace)
}

// affRespondInternalError writes the envelope app.ts' generic handler produces
// for an unhandled throw (production text; the port never exposes details).
func affRespondInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// affBodyObject mirrors `const {...} = request.body`: a JSON object is destructured
// normally, a primitive/array simply has no such properties, and a missing or
// null body throws a TypeError (ok=false).
func affBodyObject(decoded any) (map[string]any, bool) {
	if decoded == nil {
		return nil, false
	}
	if object, ok := decoded.(map[string]any); ok {
		return object, true
	}
	return map[string]any{}, true
}

// affQueryValue mirrors Fastify's query parser for one key: absent keys are "not
// present", a single occurrence is a string and repeated occurrences are an array
// (reported via isArray; the TypeScript's regex/set tests then reject them).
func affQueryValue(c *gin.Context, key string) (value string, present, isArray bool) {
	values, found := c.Request.URL.Query()[key]
	if !found || len(values) == 0 {
		return "", false, false
	}
	return values[0], true, len(values) > 1
}

// affParsePositiveRouteID mirrors parsePositiveRouteId: `^[1-9]\d*$` and a
// Number.isSafeInteger result.
func affParsePositiveRouteID(value string) (int64, bool) {
	return lib.ParsePositiveRouteID(value)
}

// affParseClampedPositiveQueryInteger mirrors
// parseClampedPositiveQueryInteger(value, fallback, max). ok=false is the `null`
// branch that maps to 400 INVALID_PARAMS.
func affParseClampedPositiveQueryInteger(value string, present bool, fallback, max int64) (int64, bool) {
	if !present {
		return fallback, true
	}
	parsed, ok := affParsePositiveRouteID(value)
	if !ok {
		return 0, false
	}
	if parsed > max {
		return max, true
	}
	return parsed, true
}

// affParseOptionalLogType mirrors parseOptionalAffLogType.
func affParseOptionalLogType(value string, present bool) (models.AffLogType, bool) {
	if !present {
		return "", true
	}
	switch models.AffLogType(value) {
	case models.AffLogTypeNewPurchase, models.AffLogTypeRenew, models.AffLogTypeConvert:
		return models.AffLogType(value), true
	}
	return "", false
}

// affParseOptionalWithdrawalStatus mirrors parseOptionalAffWithdrawalStatus.
func affParseOptionalWithdrawalStatus(value string, present bool) (models.AffWithdrawalStatus, bool) {
	if !present {
		return "", true
	}
	switch models.AffWithdrawalStatus(value) {
	case models.AffWithdrawalStatusPending, models.AffWithdrawalStatusApproved, models.AffWithdrawalStatusRejected:
		return models.AffWithdrawalStatus(value), true
	}
	return "", false
}

// affNormalizeRequiredText mirrors normalizeRequiredText: a non-string, a blank
// string or a string longer than maxLength (UTF-16 units) yields nil.
func affNormalizeRequiredText(value any, maxLength int) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := affJSTrim(text)
	if trimmed == "" || jsStringLength(trimmed) > maxLength {
		return nil
	}
	return &trimmed
}

// affNumber mirrors `Number(decimal)` for a Prisma Decimal column.
func affNumber(value models.Decimal) float64 {
	return value.Decimal.InexactFloat64()
}

// affISOTime renders a timestamp exactly like Prisma's JSON Date
// (Date#toISOString): UTC, millisecond precision, trailing Z.
func affISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// affISOTimePtr renders an optional timestamp as an ISO string or JSON null.
func affISOTimePtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := affISOTime(*value)
	return &formatted
}

// affFixed2 mirrors `Number(value).toFixed(2)`.
func affFixed2(value float64) string {
	return strconv.FormatFloat(db.PromoMoneyFixed2(value), 'f', 2, 64)
}

// affInt32ID narrows a safe integer to the int32 column domain. ok=false means the
// value cannot address an existing row.
func affInt32ID(value int64) (int32, bool) {
	if value < math.MinInt32 || value > math.MaxInt32 {
		return 0, false
	}
	return int32(value), true
}

// ==================== response shapes ====================

type affCodeItem struct {
	ID             int32    `json:"id"`
	Code           string   `json:"code"`
	PackagePlanID  *int32   `json:"packagePlanId"`
	PlanName       *string  `json:"planName"`
	PackageName    *string  `json:"packageName"`
	Price          *float64 `json:"price"`
	IsGlobal       bool     `json:"isGlobal"`
	DiscountRate   float64  `json:"discountRate"`
	CommissionRate float64  `json:"commissionRate"`
	UsedCount      int32    `json:"usedCount"`
	TotalEarnings  float64  `json:"totalEarnings"`
	CreatedAt      string   `json:"createdAt"`
}

type affAvailablePlanItem struct {
	ID          int32   `json:"id"`
	Name        string  `json:"name"`
	Price       float64 `json:"price"`
	PackageName string  `json:"packageName"`
	HasCode     bool    `json:"hasCode"`
}

type affCreatedCodeView struct {
	ID             int32   `json:"id"`
	Code           string  `json:"code"`
	PackagePlanID  *int32  `json:"packagePlanId"`
	IsGlobal       bool    `json:"isGlobal"`
	DiscountRate   float64 `json:"discountRate"`
	CommissionRate float64 `json:"commissionRate"`
	CreatedAt      string  `json:"createdAt"`
}

type affLogItem struct {
	ID             int32    `json:"id"`
	Type           string   `json:"type"`
	Amount         float64  `json:"amount"`
	OriginalAmount *float64 `json:"originalAmount"`
	BalanceBefore  float64  `json:"balanceBefore"`
	BalanceAfter   float64  `json:"balanceAfter"`
	AffCodeID      *int32   `json:"affCodeId"`
	AffCode        *string  `json:"affCode"`
	InstanceID     *int32   `json:"instanceId"`
	Remark         *string  `json:"remark"`
	CreatedAt      string   `json:"createdAt"`
}

type affWithdrawalItem struct {
	ID           int32   `json:"id"`
	Amount       float64 `json:"amount"`
	Status       string  `json:"status"`
	RejectReason *string `json:"rejectReason"`
	ReviewedAt   *string `json:"reviewedAt"`
	CreatedAt    string  `json:"createdAt"`
}

type affAdminWithdrawalItem struct {
	ID             int32   `json:"id"`
	UserID         int32   `json:"userId"`
	Username       string  `json:"username"`
	UserAffBalance float64 `json:"userAffBalance"`
	Amount         float64 `json:"amount"`
	Status         string  `json:"status"`
	RejectReason   *string `json:"rejectReason"`
	ReviewedBy     *int32  `json:"reviewedBy"`
	ReviewedAt     *string `json:"reviewedAt"`
	CreatedAt      string  `json:"createdAt"`
}

type affConvertWithdrawalView struct {
	ID        int32   `json:"id"`
	Amount    float64 `json:"amount"`
	Status    string  `json:"status"`
	CreatedAt string  `json:"createdAt"`
}

// ==================== user handlers ====================

// affMeHandler mirrors GET /me.
func affMeHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	stats, err := db.AffGetStats(ctx, user.ID)
	if err != nil {
		affRespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"activated":      db.AffIsActivated(ctx, user.ID),
		"totalEarnings":  stats.TotalEarnings,
		"totalConverted": stats.TotalConverted,
		"currentBalance": stats.CurrentBalance,
		"totalCodes":     stats.TotalCodes,
		"totalUsed":      stats.TotalUsed,
	})
}

// affMeCodesHandler mirrors GET /me/codes.
func affMeCodesHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)

	codes, err := db.AffGetUserCodes(c.Request.Context(), user.ID)
	if err != nil {
		affRespondInternalError(c)
		return
	}

	items := make([]affCodeItem, 0, len(codes))
	for _, code := range codes {
		item := affCodeItem{
			ID:             code.ID,
			Code:           code.Code,
			PackagePlanID:  code.PackagePlanID,
			IsGlobal:       code.PackagePlanID == nil,
			DiscountRate:   affNumber(code.DiscountRate),
			CommissionRate: affNumber(code.CommissionRate),
			UsedCount:      code.UsedCount,
			TotalEarnings:  affNumber(code.TotalEarnings),
			CreatedAt:      affISOTime(code.CreatedAt),
		}

		// `code.packagePlan?.name || null` — an empty name becomes null.
		if code.PackagePlan != nil {
			if code.PackagePlan.Name != "" {
				name := code.PackagePlan.Name
				item.PlanName = &name
			}
			if code.PackagePlan.Package.Name != "" {
				name := code.PackagePlan.Package.Name
				item.PackageName = &name
			}
			// `code.packagePlan?.price || null` — a zero price becomes null.
			if code.PackagePlan.Price != 0 {
				price := code.PackagePlan.Price
				item.Price = &price
			}
		}

		items = append(items, item)
	}

	c.JSON(http.StatusOK, gin.H{"codes": items})
}

// affMeAvailablePlansHandler mirrors GET /me/available-plans.
func affMeAvailablePlansHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	if !db.AffIsActivated(ctx, user.ID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": affNotActivatedText})
		return
	}

	result, err := db.AffGetAvailablePlansForCode(ctx, user.ID)
	if err != nil {
		affRespondInternalError(c)
		return
	}

	plans := make([]affAvailablePlanItem, 0, len(result.Plans))
	for _, plan := range result.Plans {
		plans = append(plans, affAvailablePlanItem{
			ID:          plan.ID,
			Name:        plan.Name,
			Price:       plan.Price,
			PackageName: plan.Package.Name,
			HasCode:     plan.HasCode,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"plans":         plans,
		"hasGlobalCode": result.HasGlobalCode,
	})
}

// affCreateCodeHandler mirrors POST /me/codes.
func affCreateCodeHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	body, ok := affBodyObject(decoded.Value)
	if !ok {
		affRespondInternalError(c)
		return
	}

	// `if (packagePlanId !== undefined)` — a JSON null is *not* undefined and takes
	// the validation branch (typeof null !== 'number').
	var packagePlanID *int32
	if raw, present := body["packagePlanId"]; present {
		number, isNumber := raw.(float64)
		if !isNumber || math.IsNaN(number) || math.IsInf(number, 0) ||
			number != math.Trunc(number) || number <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": affInvalidPlanIDText})
			return
		}
		if number > math.MaxInt32 {
			// Cannot address an existing plan row; see the file header.
			c.JSON(http.StatusBadRequest, gin.H{"error": "方案不存在或不是付费方案"})
			return
		}
		narrowed := int32(number)
		packagePlanID = &narrowed
	}

	result := db.AffCreateCode(ctx, user.ID, packagePlanID)
	if !result.Success {
		c.JSON(http.StatusBadRequest, gin.H{"error": result.Error})
		return
	}
	if result.AffCode == nil {
		affRespondInternalError(c)
		return
	}

	isGlobal := result.AffCode.PackagePlanID == nil
	logContent := "Created global AFF code " + result.AffCode.Code
	if !isGlobal && packagePlanID != nil {
		logContent = "Created AFF code " + result.AffCode.Code + " for plan #" +
			strconv.FormatInt(int64(*packagePlanID), 10)
	}

	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, "user", "aff.create_code", logContent, db.LogResultSuccess, db.CreateLogOptions{})

	message = affCreatePlanMessage
	if isGlobal {
		message = affCreateGlobalMessage
	}

	c.JSON(http.StatusOK, gin.H{
		"message": message,
		"code": affCreatedCodeView{
			ID:             result.AffCode.ID,
			Code:           result.AffCode.Code,
			PackagePlanID:  result.AffCode.PackagePlanID,
			IsGlobal:       isGlobal,
			DiscountRate:   affNumber(result.AffCode.DiscountRate),
			CommissionRate: affNumber(result.AffCode.CommissionRate),
			CreatedAt:      affISOTime(result.AffCode.CreatedAt),
		},
	})
}

// affDeleteCodeHandler mirrors DELETE /me/codes/:codeId.
func affDeleteCodeHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	// Fastify parses the body before the handler runs, so a malformed JSON body
	// wins over an invalid route id.
	if _, status, message := decodeJSONRequestBody(c); status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}

	codeID, ok := affParsePositiveRouteID(c.Param("codeId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	narrowed, fits := affInt32ID(codeID)
	if !fits {
		c.JSON(http.StatusBadRequest, gin.H{"error": "优惠码不存在"})
		return
	}

	result := db.AffDeleteCode(ctx, user.ID, narrowed)
	if !result.Success {
		c.JSON(http.StatusBadRequest, gin.H{"error": result.Error})
		return
	}

	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, "user", "aff.delete_code",
		"Deleted AFF code "+result.Code, db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"message": affDeleteCodeMessage})
}

// affMeLogsHandler mirrors GET /me/logs.
func affMeLogsHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	rawPage, pagePresent, pageIsArray := affQueryValue(c, "page")
	rawPageSize, pageSizePresent, pageSizeIsArray := affQueryValue(c, "pageSize")
	rawType, typePresent, typeIsArray := affQueryValue(c, "type")

	if pageIsArray || pageSizeIsArray || typeIsArray {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}

	parsedPage, pageOK := affParseClampedPositiveQueryInteger(rawPage, pagePresent, 1, affMaxSafeInteger)
	parsedPageSize, pageSizeOK := affParseClampedPositiveQueryInteger(
		rawPageSize, pageSizePresent, affDefaultPageSize, affRouteMaxPageSize)
	parsedType, typeOK := affParseOptionalLogType(rawType, typePresent)

	if !pageOK || !pageSizeOK || !typeOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}

	result, err := db.AffGetLogs(ctx, user.ID, db.AffLogListOptions{
		Page:     int(parsedPage),
		PageSize: int(parsedPageSize),
		Type:     parsedType,
	})
	if err != nil {
		affRespondInternalError(c)
		return
	}

	logs := make([]affLogItem, 0, len(result.Logs))
	for _, log := range result.Logs {
		item := affLogItem{
			ID:            log.ID,
			Type:          string(log.Type),
			Amount:        affNumber(log.Amount),
			BalanceBefore: affNumber(log.BalanceBefore),
			BalanceAfter:  affNumber(log.BalanceAfter),
			AffCodeID:     log.AffCodeID,
			InstanceID:    log.InstanceID,
			Remark:        log.Remark,
			CreatedAt:     affISOTime(log.CreatedAt),
		}
		// `log.originalAmount ? Number(log.originalAmount) : null` — a Prisma
		// Decimal object is always truthy, so only SQL NULL maps to null.
		if log.OriginalAmount != nil {
			original := affNumber(*log.OriginalAmount)
			item.OriginalAmount = &original
		}
		// `log.affCode?.code || null`.
		if log.AffCode != nil && log.AffCode.Code != "" {
			code := log.AffCode.Code
			item.AffCode = &code
		}
		logs = append(logs, item)
	}

	c.JSON(http.StatusOK, gin.H{
		"logs":     logs,
		"total":    result.Total,
		"page":     result.Page,
		"pageSize": result.PageSize,
	})
}

// affLeaderboardHandler mirrors GET /leaderboard.
func affLeaderboardHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)

	leaderboard, err := db.AffGetLeaderboard(c.Request.Context(), &user.ID)
	if err != nil {
		affRespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"leaderboard": leaderboard})
}

// affValidateHandler mirrors POST /validate.
func affValidateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	body, ok := affBodyObject(decoded.Value)
	if !ok {
		affRespondInternalError(c)
		return
	}

	// `if (!code || typeof code !== 'string' || !code.trim())` — every non-string
	// is either falsy or fails the typeof test, so both answer the same 400.
	rawCode, _ := body["code"]
	code, isString := rawCode.(string)
	if !isString || affJSTrim(code) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": affEmptyCodeText})
		return
	}
	trimmedCode := affJSTrim(code)

	// `if (!packagePlanId || typeof packagePlanId !== 'number' ||
	//  !Number.isSafeInteger(packagePlanId))`
	rawPlanID := body["packagePlanId"]
	planIDNumber, isNumber := rawPlanID.(float64)
	if !isNumber || math.IsNaN(planIDNumber) || math.IsInf(planIDNumber, 0) ||
		planIDNumber == 0 || planIDNumber != math.Trunc(planIDNumber) ||
		math.Abs(planIDNumber) > affMaxSafeInteger {
		c.JSON(http.StatusBadRequest, gin.H{"error": affInvalidPlanIDText})
		return
	}

	plan, err := affFindPlan(ctx, planIDNumber)
	if err != nil {
		affRespondInternalError(c)
		return
	}
	if plan == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": affPlanNotFoundText})
		return
	}

	// 1. The unified promo-code engine runs first.
	validation, err := services.PromoValidate(ctx, services.PromoValidateParams{
		Code:          trimmedCode,
		PackageID:     plan.PackageID,
		PackagePlanID: plan.ID,
		UserID:        user.ID,
	})
	if err != nil {
		affRespondInternalError(c)
		return
	}

	if validation.Valid && validation.PromoCode != nil {
		billing := db.CalculateCreateBilling(*plan, time.Now())
		quote := services.PromoQuoteCreation(billing.TotalPrice, services.PromoQuotePromo{
			DiscountType:  validation.PromoCode.DiscountType,
			DiscountValue: validation.PromoCode.DiscountValue,
		})

		discountRate := 0.0
		if billing.TotalPrice > 0 {
			discountRate = quote.DiscountAmount / billing.TotalPrice
		}

		c.JSON(http.StatusOK, gin.H{
			"valid":          true,
			"discountRate":   discountRate,
			"commissionRate": affNumber(validation.PromoCode.CommissionRate),
			"promoCode":      validation.PromoCode.Code,
		})
		return
	}

	// 2. Legacy AFF codes are the fallback when the engine found nothing at all.
	if validation.ErrorCode == "PROMO_NOT_FOUND" {
		affResult, err := db.AffValidateCode(ctx, trimmedCode, plan.ID, user.ID)
		if err != nil {
			affRespondInternalError(c)
			return
		}
		if !affResult.Valid {
			errorText := affResult.Error
			if errorText == "" {
				errorText = validation.Error
			}
			if errorText == "" {
				errorText = affCodeFallbackNotFound
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": errorText})
			return
		}

		discountRate := 0.0
		if affResult.DiscountRate != nil {
			discountRate = *affResult.DiscountRate
		}
		commissionRate := 0.0
		if affResult.CommissionRate != nil {
			commissionRate = *affResult.CommissionRate
		}
		promoCode := ""
		if affResult.AffCode != nil {
			promoCode = affResult.AffCode.Code
		}

		c.JSON(http.StatusOK, gin.H{
			"valid":          true,
			"discountRate":   discountRate,
			"commissionRate": commissionRate,
			"promoCode":      promoCode,
		})
		return
	}

	// 3. The engine rejected the code (disabled, expired, quota, self-use, scope…).
	errorText := validation.Error
	if errorText == "" {
		errorText = affInvalidCodeText
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": errorText})
}

// affFindPlan loads the package plan columns the validate route needs. nil, nil
// means "no row".
func affFindPlan(ctx context.Context, planIDNumber float64) (*models.PackagePlan, error) {
	planID, fits := affInt32ID(int64(planIDNumber))
	if !fits {
		// Outside the int32 column domain: cannot exist (see the file header).
		return nil, nil
	}

	var plan models.PackagePlan
	err := db.DB.WithContext(ctx).Raw(
		`SELECT id, package_id, price, billing_cycle FROM package_plans WHERE id = ?`, planID,
	).Scan(&plan).Error
	if err != nil {
		return nil, err
	}
	if plan.ID == 0 {
		return nil, nil
	}
	return &plan, nil
}

// affConvertHandler mirrors POST /me/convert.
func affConvertHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	body, ok := affBodyObject(decoded.Value)
	if !ok {
		affRespondInternalError(c)
		return
	}

	// `if (!amount || typeof amount !== 'number' || !Number.isFinite(amount) || amount <= 0)`
	amount, isNumber := body["amount"].(float64)
	if !isNumber || math.IsNaN(amount) || math.IsInf(amount, 0) || amount <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": affInvalidAmountText})
		return
	}

	// `Math.round(amount * 100) / 100` — the input is positive, so Go's math.Round
	// (half away from zero) matches JavaScript's half-toward-+Infinity.
	normalizedAmount := math.Round(amount*100) / 100

	result := db.AffCreateWithdrawal(ctx, user.ID, normalizedAmount)
	if !result.Success {
		c.JSON(http.StatusBadRequest, gin.H{"error": result.Error})
		return
	}
	if result.Withdrawal == nil {
		affRespondInternalError(c)
		return
	}

	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, "user", "aff.convert_request",
		"AFF balance converted: "+formatTerminalCommandNumber(normalizedAmount),
		db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{
		"message": affConvertMessage,
		"withdrawal": affConvertWithdrawalView{
			ID:        result.Withdrawal.ID,
			Amount:    affNumber(result.Withdrawal.Amount),
			Status:    string(result.Withdrawal.Status),
			CreatedAt: affISOTime(result.Withdrawal.CreatedAt),
		},
	})
}

// affMeWithdrawalsHandler mirrors GET /me/withdrawals.
func affMeWithdrawalsHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	rawPage, pagePresent, pageIsArray := affQueryValue(c, "page")
	rawPageSize, pageSizePresent, pageSizeIsArray := affQueryValue(c, "pageSize")
	rawStatus, statusPresent, statusIsArray := affQueryValue(c, "status")

	if pageIsArray || pageSizeIsArray || statusIsArray {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}

	parsedPage, pageOK := affParseClampedPositiveQueryInteger(rawPage, pagePresent, 1, affMaxSafeInteger)
	parsedPageSize, pageSizeOK := affParseClampedPositiveQueryInteger(
		rawPageSize, pageSizePresent, affDefaultPageSize, affRouteMaxPageSize)
	parsedStatus, statusOK := affParseOptionalWithdrawalStatus(rawStatus, statusPresent)

	if !pageOK || !pageSizeOK || !statusOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}

	result, err := db.AffGetUserWithdrawals(ctx, user.ID, db.AffWithdrawalListOptions{
		Page:     int(parsedPage),
		PageSize: int(parsedPageSize),
		Status:   parsedStatus,
	})
	if err != nil {
		affRespondInternalError(c)
		return
	}

	items := make([]affWithdrawalItem, 0, len(result.Withdrawals))
	for _, withdrawal := range result.Withdrawals {
		items = append(items, affWithdrawalItem{
			ID:           withdrawal.ID,
			Amount:       affNumber(withdrawal.Amount),
			Status:       string(withdrawal.Status),
			RejectReason: withdrawal.RejectReason,
			ReviewedAt:   affISOTimePtr(withdrawal.ReviewedAt),
			CreatedAt:    affISOTime(withdrawal.CreatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"withdrawals": items,
		"total":       result.Total,
		"page":        result.Page,
		"pageSize":    result.PageSize,
	})
}

// ==================== admin handlers ====================

// affAdminWithdrawalsHandler mirrors GET /admin/withdrawals.
func affAdminWithdrawalsHandler(c *gin.Context) {
	ctx := c.Request.Context()

	rawPage, pagePresent, pageIsArray := affQueryValue(c, "page")
	rawPageSize, pageSizePresent, pageSizeIsArray := affQueryValue(c, "pageSize")
	rawStatus, statusPresent, statusIsArray := affQueryValue(c, "status")

	if pageIsArray || pageSizeIsArray || statusIsArray {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}

	parsedPage, pageOK := affParseClampedPositiveQueryInteger(rawPage, pagePresent, 1, affMaxSafeInteger)
	parsedPageSize, pageSizeOK := affParseClampedPositiveQueryInteger(
		rawPageSize, pageSizePresent, affDefaultPageSize, affRouteMaxPageSize)
	parsedStatus, statusOK := affParseOptionalWithdrawalStatus(rawStatus, statusPresent)

	if !pageOK || !pageSizeOK || !statusOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
		return
	}

	result, err := db.AffGetPendingWithdrawals(ctx, db.AffPendingWithdrawalListOptions{
		Page:     int(parsedPage),
		PageSize: int(parsedPageSize),
		Status:   parsedStatus,
	})
	if err != nil {
		affRespondInternalError(c)
		return
	}

	items := make([]affAdminWithdrawalItem, 0, len(result.Withdrawals))
	for _, withdrawal := range result.Withdrawals {
		items = append(items, affAdminWithdrawalItem{
			ID:             withdrawal.ID,
			UserID:         withdrawal.UserID,
			Username:       withdrawal.User.Username,
			UserAffBalance: withdrawal.User.AffBalance,
			Amount:         affNumber(withdrawal.Amount),
			Status:         string(withdrawal.Status),
			RejectReason:   withdrawal.RejectReason,
			ReviewedBy:     withdrawal.ReviewedBy,
			ReviewedAt:     affISOTimePtr(withdrawal.ReviewedAt),
			CreatedAt:      affISOTime(withdrawal.CreatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"withdrawals": items,
		"total":       result.Total,
		"page":        result.Page,
		"pageSize":    result.PageSize,
	})
}

// affApproveWithdrawalHandler mirrors POST /admin/withdrawals/:withdrawalId/approve.
func affApproveWithdrawalHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	// The TypeScript declares no Body type but Fastify still parses the body.
	if _, status, message := decodeJSONRequestBody(c); status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}

	withdrawalID, ok := affParsePositiveRouteID(c.Param("withdrawalId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	// Pre-read: the TypeScript loads the row before approving to learn userId and
	// amount for the log/inbox side effects. An out-of-range id cannot exist.
	if narrowed, fits := affInt32ID(withdrawalID); fits {
		withdrawal, err := db.AffGetWithdrawalByID(ctx, narrowed)
		if err != nil {
			affRespondInternalError(c)
			return
		}
		if withdrawal == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": affWithdrawalNotFound})
			return
		}

		result := db.AffApproveWithdrawal(ctx, narrowed, user.ID)
		if !result.Success {
			c.JSON(http.StatusBadRequest, gin.H{"error": result.Error})
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, "admin", "aff.approve_convert",
			"Approved AFF convert request #"+strconv.FormatInt(withdrawalID, 10),
			db.LogResultSuccess, db.CreateLogOptions{})

		amount := affNumber(withdrawal.Amount)
		content := "您的 AFF 余额转化申请已审核通过，¥" + affFixed2(amount) + " 已转入您的账户余额。"
		if _, err := db.CreateInboxMessage(ctx, db.CreateInboxMessageData{
			UserID:    withdrawal.UserID,
			EventType: "aff_convert_approved",
			Title:     affApproveInboxTitle,
			Content:   content,
			Data:      map[string]any{"withdrawalId": narrowed, "amount": amount},
		}); err != nil {
			affRespondInternalError(c)
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": affApproveMessage})
		return
	}

	c.JSON(http.StatusBadRequest, gin.H{"error": affWithdrawalNotFound})
}

// affRejectWithdrawalHandler mirrors POST /admin/withdrawals/:withdrawalId/reject.
func affRejectWithdrawalHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	body, ok := affBodyObject(decoded.Value)
	if !ok {
		affRespondInternalError(c)
		return
	}

	// Computed before the id check, exactly as in the TypeScript.
	reason := affNormalizeRequiredText(body["reason"], affRejectReasonMaxLength)

	withdrawalID, idOK := affParsePositiveRouteID(c.Param("withdrawalId"))
	if !idOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if reason == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": affMissingRejectReason})
		return
	}

	narrowed, fits := affInt32ID(withdrawalID)
	if !fits {
		c.JSON(http.StatusBadRequest, gin.H{"error": affWithdrawalNotFound})
		return
	}

	withdrawal, err := db.AffGetWithdrawalByID(ctx, narrowed)
	if err != nil {
		affRespondInternalError(c)
		return
	}
	if withdrawal == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": affWithdrawalNotFound})
		return
	}

	result := db.AffRejectWithdrawal(ctx, narrowed, user.ID, *reason)
	if !result.Success {
		c.JSON(http.StatusBadRequest, gin.H{"error": result.Error})
		return
	}

	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, "admin", "aff.reject_convert",
		"Rejected AFF convert request #"+strconv.FormatInt(withdrawalID, 10)+": "+*reason,
		db.LogResultSuccess, db.CreateLogOptions{})

	amount := affNumber(withdrawal.Amount)
	content := "您的 AFF 余额转化申请（¥" + affFixed2(amount) + "）已被拒绝。\n拒绝原因：" + *reason
	if _, err := db.CreateInboxMessage(ctx, db.CreateInboxMessageData{
		UserID:    withdrawal.UserID,
		EventType: "aff_convert_rejected",
		Title:     affRejectInboxTitle,
		Content:   content,
		Data:      map[string]any{"withdrawalId": narrowed, "amount": amount, "reason": *reason},
	}); err != nil {
		affRespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": affRejectMessage})
}

// ==================== registration ====================

// RegisterAff mounts the AFF routes under /api/aff.
func RegisterAff(r *gin.Engine) {
	group := r.Group("/api/aff")

	group.GET("/me", middleware.Authenticate(), affMeHandler)
	group.GET("/me/codes", middleware.Authenticate(), affMeCodesHandler)
	group.GET("/me/available-plans", middleware.Authenticate(), affMeAvailablePlansHandler)
	group.POST("/me/codes", middleware.Authenticate(), affCreateCodeHandler)
	group.DELETE("/me/codes/:codeId", middleware.Authenticate(), affDeleteCodeHandler)
	group.GET("/me/logs", middleware.Authenticate(), affMeLogsHandler)
	group.GET("/leaderboard", middleware.Authenticate(), affLeaderboardHandler)
	group.POST("/validate", middleware.Authenticate(), affValidateHandler)
	group.POST("/me/convert", middleware.Authenticate(), affConvertHandler)
	group.GET("/me/withdrawals", middleware.Authenticate(), affMeWithdrawalsHandler)

	group.GET("/admin/withdrawals", middleware.Authenticate(), middleware.RequireAdmin(), affAdminWithdrawalsHandler)
	group.POST("/admin/withdrawals/:withdrawalId/approve",
		middleware.Authenticate(), middleware.RequireAdmin(), affApproveWithdrawalHandler)
	group.POST("/admin/withdrawals/:withdrawalId/reject",
		middleware.Authenticate(), middleware.RequireAdmin(), affRejectWithdrawalHandler)
}

func init() {
	Register(RegisterAff)
}
