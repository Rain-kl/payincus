// Port of server/src/routes/mail.ts (2361 lines, 29 routes).
//
// Mounting: app.ts registers this plugin with `{ prefix: '/api/mail' }`, so every
// path below is `/api/mail/...`. Gin needs ONE wildcard name per path position, so
// the "domain id" position is always `:id` (`/domains/:id/accounts`,
// `/domains/:id/accounts/:accountId`, ...) even where the TypeScript said
// `:domainId`.
//
// Error-shape fidelity (verified against the running Node backend, not assumed):
//
//   - `throw apiError(...)` throws a PLAIN OBJECT. Fastify's error handler reads
//     `error.statusCode` (undefined) and `error.message` (undefined), so the
//     client receives **500 `{"error":"Internal server error"}`** — not a 400.
//     That covers every normalizer called outside a try/catch (mail source, mail
//     plan, mail account, cancel input) and `parsePositiveRouteId`.
//     `mailRespondUncaughtAPIError` reproduces exactly that.
//   - `try { normalize() } catch (e) { return reply.code(400).send(e) }` does
//     return the apiError envelope with 400 (auto-renew, purchase, renew).
//   - `reply.code(N).send(apiError(...))` is a normal N + envelope response.
//   - An unhandled exception answers 500 `{"error":"Internal server error"}` in
//     development as well (the handler falls back to that string), which is what
//     `mailRespondInternal` writes.
//
// Money: every amount travels as models.Decimal; the only float64 use is where
// the TypeScript itself does `Number(price)` for display or arithmetic and then
// writes the result back (always through `lib.JSFixed`, which reproduces
// `Number(x.toFixed(2))`).
package routes

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// ==================== constants ====================

const (
	mailCancelReasonMaxLength = 500
	mailSourceSecretMaxLength = 4096
	mailAffCodeMaxLength      = 64
	mailPlanNameMaxLength     = 100
	mailPlanDescMaxLength     = 1000
	mailSourceNameMaxLength   = 100
	mailSourceCodeMaxLength   = 32
	mailSourceURLMaxLength    = 2048
	mailAccountNameMaxLength  = 100
	mailAccountUsernameMax    = 64
	mailAccountPasswordMin    = 8
	mailAccountPasswordMax    = 128
	mailDefaultAccountDiskMB  = 2048

	mailPurchaseRemarkPrefix = "购买域名邮箱 - "
	mailRenewRemarkPrefix    = "续费域名邮箱 "
	mailRefundRemarkPrefix   = "管理员退订邮箱 - "

	mailDayMilliseconds = int64(24 * 60 * 60 * 1000)

	mailPlanMaxPrice   = 99999999.99
	mailMaxSortOrder   = 1000000
	mailMaxDomainLimit = 1000
	mailMaxDiskLimitGB = 102400
)

var (
	mailSourceCodePattern  = regexp.MustCompile(`^[a-z0-9_-]{2,32}$`)
	mailDomainPattern      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*\.[a-z]{2,}$`)
	mailUsernamePattern    = regexp.MustCompile(`^[a-z0-9._-]+$`)
	mailRenewMonthsPattern = regexp.MustCompile(`^(\d+) 个月`)
)

// Upstream calls are seams so the guarded transaction branches (upstream failure
// must not consume quota / must not mark a domain verified) can be unit-tested
// without a network.
var (
	mailCraneCreateDomain    = services.CraneMailCreateDomain
	mailCraneGetDomainInfo   = services.CraneMailGetDomainInfo
	mailCraneDeleteDomain    = services.CraneMailDeleteDomain
	mailCraneResumeDomain    = services.CraneMailResumeDomain
	mailSmarterCreateAccount = services.SmarterMailCreateAccount
	mailSmarterUpdateAccount = services.SmarterMailUpdateAccount
	mailSmarterResetPassword = services.SmarterMailResetPassword
	mailSmarterDeleteAccount = services.SmarterMailDeleteAccount
)

// ==================== small JS-semantics helpers ====================

// mailISOTime renders an instant like Date#toISOString (UTC, milliseconds, Z).
func mailISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// mailISOTimePtr mirrors `value ? value.toISOString() : null`.
func mailISOTimePtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := mailISOTime(*value)
	return &formatted
}

// mailUTF16Length mirrors String#length (UTF-16 code units).
func mailUTF16Length(value string) int {
	return len(utf16.Encode([]rune(value)))
}

// mailJSString mirrors String(value) for JSON values.
func mailJSString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "undefined"
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return mailJSNumberString(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if item == nil {
				parts = append(parts, "")
				continue
			}
			parts = append(parts, mailJSString(item))
		}
		return strings.Join(parts, ",")
	default:
		return "[object Object]"
	}
}

// mailJSNumberString mirrors Number#toString for the values produced here.
func mailJSNumberString(value float64) string {
	if math.IsNaN(value) {
		return "NaN"
	}
	if math.IsInf(value, 1) {
		return "Infinity"
	}
	if math.IsInf(value, -1) {
		return "-Infinity"
	}
	if value == math.Trunc(value) && math.Abs(value) < 1e21 {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// mailFalsy mirrors JavaScript truthiness for JSON values.
func mailFalsy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case bool:
		return !typed
	case float64:
		return typed == 0 || math.IsNaN(typed)
	case string:
		return typed == ""
	default:
		return false
	}
}

// mailDecimalFromNumber mirrors Prisma's conversion of a JS number into a Decimal.
func mailDecimalFromNumber(value float64) models.Decimal {
	parsed, err := models.NewDecimalFromString(strconv.FormatFloat(value, 'f', -1, 64))
	if err != nil {
		return models.Decimal{}
	}
	return parsed
}

func mailDecimalAbsNumber(value models.Decimal) float64 {
	absolute := value.Decimal.Abs()
	asFloat, _ := absolute.Float64()
	return asFloat
}

func mailDecimalFloat(value models.Decimal) float64 {
	asFloat, _ := value.Float64()
	return asFloat
}

// mailJSRound mirrors Math.round (ties toward +Infinity, unlike math.Round).
func mailJSRound(value float64) float64 {
	return math.Floor(value + 0.5)
}

// mailNormalizeBaseURL mirrors `url.toString().replace(/\/+$/, ”)`.
func mailNormalizeBaseURL(parsed *url.URL) string {
	clone := *parsed
	clone.Scheme = strings.ToLower(clone.Scheme)
	host := strings.ToLower(clone.Host)
	if clone.Scheme == "https" && strings.HasSuffix(host, ":443") {
		host = strings.TrimSuffix(host, ":443")
	}
	if clone.Scheme == "http" && strings.HasSuffix(host, ":80") {
		host = strings.TrimSuffix(host, ":80")
	}
	clone.Host = host
	return strings.TrimRight(clone.String(), "/")
}

// ==================== request body helpers ====================

// mailReadRequestBody returns the parsed `request.body`.
//
//	ok=false      the response was already written (malformed body)
//	present=false `request.body` is undefined/null — the TypeScript treats that
//	              differently per route: destructuring throws (500), while the
//	              try/catch normalizers turn it into a 400.
func mailReadRequestBody(c *gin.Context) (value any, present bool, ok bool) {
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return nil, false, false
	}
	if decoded.Value == nil {
		return nil, false, true
	}
	return decoded.Value, true, true
}

// mailObjectFields mirrors property access on `request.body`: objects keep their
// keys, every other primitive simply has none.
func mailObjectFields(value any) map[string]any {
	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

// mailBodyField returns the value and whether the key was present (JSON null is
// present — that distinction decides `!== undefined` branches).
func mailBodyField(body map[string]any, key string) (any, bool) {
	value, present := body[key]
	return value, present
}

// ==================== response helpers ====================

// mailRespondAPIError mirrors `reply.code(status).send(apiError(code, details))`.
func mailRespondAPIError(c *gin.Context, status int, code lib.ErrorCode, details string) {
	c.AbortWithStatusJSON(status, lib.APIErrorFor(code, details))
}

// mailRespondInternal mirrors the global error handler's fallback for a thrown
// Error: always `{"error":"Internal server error"}` because `error.message` is
// undefined for the objects apiError() produces.
func mailRespondInternal(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// mailRespondUncaughtAPIError mirrors `throw apiError(...)` with no try/catch: a
// plain object reaches Fastify's handler and becomes a 500 with this exact body.
func mailRespondUncaughtAPIError(c *gin.Context) {
	mailRespondInternal(c)
}

// mailRespondJSError mirrors app.ts' handler for a thrown Error:
//
//	statusCode = 500
//	message    = NODE_ENV === 'production' ? 'Internal server error' : error.message
//
// The V8 TypeError texts are reproduced verbatim so a development deployment (the
// conformance run) sees byte-identical bodies. Production keeps the generic text.
func mailRespondJSError(c *gin.Context, message string) {
	if os.Getenv("NODE_ENV") == "production" {
		mailRespondInternal(c)
		return
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": message})
}

// mailRespondDestructureError mirrors `const { x } = request.body` when the body is
// undefined or null: a V8 TypeError naming the first destructured property.
func mailRespondDestructureError(c *gin.Context, property string) {
	kind := "undefined"
	if c.GetHeader("Content-Type") != "" {
		kind = "null"
	}
	mailRespondJSError(c, fmt.Sprintf(
		"Cannot destructure property '%s' of 'request.body' as it is %s.", property, kind))
}

// ==================== validation errors ====================

func mailValidationError(format string, args ...any) *lib.APIError {
	apiErr := lib.APIErrorFor(lib.ErrValidationError, fmt.Sprintf(format, args...))
	return &apiErr
}

func mailInvalidParamsError(details string) *lib.APIError {
	apiErr := lib.APIErrorFor(lib.ErrInvalidParams, details)
	return &apiErr
}

// ==================== typed request bodies ====================

type mailPlanInput struct {
	SourceID     *int32
	Name         *string
	Description  *string
	DomainLimit  *int32
	DiskLimitGB  *int32
	BillingCycle *models.MailBillingCycle
	Price        *float64
	Enabled      *bool
	SortOrder    *int32
}

type mailSourceInput struct {
	Name           *string
	Code           *string
	APIURL         *string
	APIKey         *string
	SmarterMailURL *string
	Enabled        *bool
	SortOrder      *int32
}

type mailAccountInput struct {
	Username    *string
	Password    *string
	DisplayName *string
	DiskLimitMB *int32
}

type mailSubscriptionCancelInput struct {
	RefundType string
	Reason     *string
}

// ==================== primitive validators ====================

// mailRequireNumber mirrors requireNumber.
func mailRequireNumber(value any, field string) (float64, *lib.APIError) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, mailValidationError("%s 无效", field)
	}
	return number, nil
}

// mailRequireSafeInteger mirrors requireSafeInteger.
func mailRequireSafeInteger(value any, field string) (int64, *lib.APIError) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, mailValidationError("%s 无效", field)
	}
	if number != math.Trunc(number) || math.Abs(number) > 9007199254740991 {
		return 0, mailValidationError("%s 无效", field)
	}
	return int64(number), nil
}

// mailOptionalStringField mirrors optionalString(input.<key>, field, maxLength):
// absent keys are undefined, present non-strings are an error, and the trimmed
// value must be 1..maxLength (UTF-16 code units, like String#length).
func mailOptionalStringField(body map[string]any, key, field string, maxLength int) (*string, *lib.APIError) {
	value, present := mailBodyField(body, key)
	if !present {
		return nil, nil
	}
	typed, ok := value.(string)
	if !ok {
		return nil, mailValidationError("%s 无效", field)
	}
	trimmed := strings.TrimSpace(typed)
	if trimmed == "" || mailUTF16Length(trimmed) > maxLength {
		return nil, mailValidationError("%s 需为 1-%d 个字符", field, maxLength)
	}
	return &trimmed, nil
}

// ==================== normalizers ====================

// mailNormalizePlanInput mirrors normalizeMailPlanInput.
func mailNormalizePlanInput(body map[string]any, requireAll bool) (mailPlanInput, *lib.APIError) {
	input := mailPlanInput{}

	if requireAll {
		_, pricePresent := mailBodyField(body, "price")
		if mailFalsy(body["sourceId"]) || mailFalsy(body["name"]) || mailFalsy(body["domainLimit"]) ||
			mailFalsy(body["diskLimitGb"]) || mailFalsy(body["billingCycle"]) || !pricePresent {
			return input, mailValidationError("请填写所有必填字段")
		}
	}

	if value, present := mailBodyField(body, "sourceId"); present {
		sourceID, apiErr := mailRequireSafeInteger(value, "邮箱源")
		if apiErr != nil {
			return input, apiErr
		}
		if sourceID <= 0 {
			return input, mailValidationError("邮箱源无效")
		}
		normalized := int32(sourceID)
		input.SourceID = &normalized
	}

	if value, present := mailBodyField(body, "name"); present {
		name := strings.TrimSpace(mailJSString(value))
		if name == "" || mailUTF16Length(name) > mailPlanNameMaxLength {
			return input, mailValidationError("方案名称需为 1-100 个字符")
		}
		input.Name = &name
	}

	if value, present := mailBodyField(body, "description"); present {
		description := strings.TrimSpace(mailJSString(value))
		if mailUTF16Length(description) > mailPlanDescMaxLength {
			return input, mailValidationError("方案描述不能超过 1000 个字符")
		}
		if description != "" {
			input.Description = &description
		}
	}

	if value, present := mailBodyField(body, "domainLimit"); present {
		domainLimit, apiErr := mailRequireSafeInteger(value, "域名数量限制")
		if apiErr != nil {
			return input, apiErr
		}
		if domainLimit <= 0 || domainLimit > mailMaxDomainLimit {
			return input, mailValidationError("域名数量限制需为 1-1000 的整数")
		}
		normalized := int32(domainLimit)
		input.DomainLimit = &normalized
	}

	if value, present := mailBodyField(body, "diskLimitGb"); present {
		diskLimitGB, apiErr := mailRequireSafeInteger(value, "磁盘容量")
		if apiErr != nil {
			return input, apiErr
		}
		if diskLimitGB <= 0 || diskLimitGB > mailMaxDiskLimitGB {
			return input, mailValidationError("磁盘容量需为 1-102400 GB 的整数")
		}
		normalized := int32(diskLimitGB)
		input.DiskLimitGB = &normalized
	}

	if value, present := mailBodyField(body, "billingCycle"); present {
		cycle, ok := value.(string)
		if !ok || (cycle != string(models.MailBillingCycleMonthly) && cycle != string(models.MailBillingCycleYearly)) {
			return input, mailValidationError("计费周期无效")
		}
		normalized := models.MailBillingCycle(cycle)
		input.BillingCycle = &normalized
	}

	if value, present := mailBodyField(body, "price"); present {
		price, apiErr := mailRequireNumber(value, "价格")
		if apiErr != nil {
			return input, apiErr
		}
		if price <= 0 || price > mailPlanMaxPrice {
			return input, mailValidationError("价格需大于 0 且不超过 99999999.99")
		}
		roundedPrice := mailJSRound(price*100) / 100
		if math.Abs(price-roundedPrice) > 1e-8 {
			return input, mailValidationError("价格最多支持两位小数")
		}
		input.Price = &roundedPrice
	}

	if value, present := mailBodyField(body, "enabled"); present {
		enabled := false
		if flag, ok := value.(bool); ok && flag {
			enabled = true
		}
		input.Enabled = &enabled
	}

	if value, present := mailBodyField(body, "sortOrder"); present {
		sortOrder, apiErr := mailRequireSafeInteger(value, "排序值")
		if apiErr != nil {
			return input, apiErr
		}
		if sortOrder < -mailMaxSortOrder || sortOrder > mailMaxSortOrder {
			return input, mailValidationError("排序值无效")
		}
		normalized := int32(sortOrder)
		input.SortOrder = &normalized
	}

	return input, nil
}

// mailNormalizeSourceInput mirrors normalizeMailSourceInput.
func mailNormalizeSourceInput(value any, requireAll bool) (mailSourceInput, *lib.APIError) {
	input := mailSourceInput{}

	if _, ok := value.(map[string]any); !ok {
		return input, mailValidationError("请求参数无效")
	}
	body := value.(map[string]any)

	if requireAll {
		if mailFalsy(body["name"]) || mailFalsy(body["code"]) || mailFalsy(body["apiUrl"]) ||
			mailFalsy(body["apiKey"]) || mailFalsy(body["smarterMailUrl"]) {
			return input, mailValidationError("请填写所有必填字段")
		}
	}

	name, apiErr := mailOptionalStringField(body, "name", "邮箱源名称", mailSourceNameMaxLength)
	if apiErr != nil {
		return input, apiErr
	}
	input.Name = name

	code, apiErr := mailOptionalStringField(body, "code", "地区代码", mailSourceCodeMaxLength)
	if apiErr != nil {
		return input, apiErr
	}
	if code != nil {
		normalizedCode := strings.ToLower(*code)
		if !mailSourceCodePattern.MatchString(normalizedCode) {
			return input, mailValidationError("地区代码只能包含 2-32 位字母、数字、下划线和连字符")
		}
		input.Code = &normalizedCode
	}

	apiURL, apiErr := mailOptionalStringField(body, "apiUrl", "CraneMail API URL", mailSourceURLMaxLength)
	if apiErr != nil {
		return input, apiErr
	}
	input.APIURL = apiURL

	smarterMailURL, apiErr := mailOptionalStringField(body, "smarterMailUrl", "SmarterMail URL", mailSourceURLMaxLength)
	if apiErr != nil {
		return input, apiErr
	}
	input.SmarterMailURL = smarterMailURL

	if rawKey, present := mailBodyField(body, "apiKey"); present {
		key, ok := rawKey.(string)
		if !ok {
			return input, mailValidationError("API Key 无效")
		}
		trimmed := strings.TrimSpace(key)
		if requireAll && trimmed == "" {
			return input, mailValidationError("API Key 不能为空")
		}
		if mailUTF16Length(trimmed) > mailSourceSecretMaxLength {
			return input, mailValidationError("API Key 不能超过 %d 个字符", mailSourceSecretMaxLength)
		}
		input.APIKey = &trimmed
	}

	if rawEnabled, present := mailBodyField(body, "enabled"); present {
		enabled, ok := rawEnabled.(bool)
		if !ok {
			return input, mailValidationError("启用状态无效")
		}
		input.Enabled = &enabled
	}

	if rawSortOrder, present := mailBodyField(body, "sortOrder"); present {
		sortOrder, apiErr := mailRequireSafeInteger(rawSortOrder, "排序值")
		if apiErr != nil {
			return input, apiErr
		}
		if sortOrder < -mailMaxSortOrder || sortOrder > mailMaxSortOrder {
			return input, mailValidationError("排序值无效")
		}
		normalized := int32(sortOrder)
		input.SortOrder = &normalized
	}

	return input, nil
}

// mailNormalizeAccountInput mirrors normalizeMailAccountInput.
func mailNormalizeAccountInput(
	body map[string]any,
	requireUsername bool,
	requirePassword bool,
	maxDiskLimitMB int64,
) (mailAccountInput, *lib.APIError) {
	input := mailAccountInput{}

	maxDisk := int64(math.Floor(float64(maxDiskLimitMB)))
	if maxDisk <= 0 {
		return input, mailValidationError("订阅磁盘配额无效")
	}

	rawUsername, usernamePresent := mailBodyField(body, "username")
	if requireUsername || usernamePresent {
		username := ""
		if !mailFalsy(rawUsername) {
			username = mailJSString(rawUsername)
		}
		username = strings.ToLower(strings.TrimSpace(username))
		if username == "" || mailUTF16Length(username) > mailAccountUsernameMax || !mailUsernamePattern.MatchString(username) {
			return input, mailValidationError("用户名只能包含 1-64 位字母、数字、点、下划线和连字符")
		}
		input.Username = &username
	}

	rawPassword, passwordPresent := mailBodyField(body, "password")
	if requirePassword || passwordPresent {
		password := ""
		if !mailFalsy(rawPassword) {
			password = mailJSString(rawPassword)
		}
		length := mailUTF16Length(password)
		if length < mailAccountPasswordMin || length > mailAccountPasswordMax {
			return input, mailValidationError("密码长度需为 8-128 位")
		}
		input.Password = &password
	}

	if rawDisplayName, present := mailBodyField(body, "displayName"); present {
		displayName := strings.TrimSpace(mailJSString(rawDisplayName))
		if mailUTF16Length(displayName) > mailAccountNameMaxLength {
			return input, mailValidationError("显示名称不能超过 100 个字符")
		}
		if displayName != "" {
			input.DisplayName = &displayName
		}
	}

	if rawDiskLimit, present := mailBodyField(body, "diskLimitMb"); present {
		diskLimitMB, apiErr := mailRequireSafeInteger(rawDiskLimit, "邮箱容量")
		if apiErr != nil {
			return input, apiErr
		}
		if diskLimitMB <= 0 || diskLimitMB > maxDisk {
			return input, mailValidationError("邮箱容量需为 1-%d MB 的整数", maxDisk)
		}
		normalized := int32(diskLimitMB)
		input.DiskLimitMB = &normalized
	}

	return input, nil
}

// mailNormalizeRenewMonths mirrors normalizeMailRenewMonths.
func mailNormalizeRenewMonths(value any) (int64, *lib.APIError) {
	months, apiErr := mailRequireSafeInteger(value, "续费月数")
	if apiErr != nil {
		return 0, apiErr
	}
	if months < 1 {
		return 0, mailValidationError("续费月数需为正整数")
	}
	return months, nil
}

// mailValidateRenewMonthsForBillingCycle mirrors
// validateMailRenewMonthsForBillingCycle.
func mailValidateRenewMonthsForBillingCycle(months int64, billingCycle models.MailBillingCycle) *lib.APIError {
	billingCycleMonths := int64(1)
	if billingCycle == models.MailBillingCycleYearly {
		billingCycleMonths = 12
	}
	if months%billingCycleMonths != 0 {
		return mailValidationError("年付套餐只能按年续费")
	}
	if billingCycle == models.MailBillingCycleMonthly && months > 12 {
		return mailValidationError("续费月数需为 1-12 的整数")
	}
	return nil
}

// mailNormalizeSubscriptionAutoRenewInput mirrors
// normalizeMailSubscriptionAutoRenewInput.
func mailNormalizeSubscriptionAutoRenewInput(value any) (bool, *lib.APIError) {
	body, ok := value.(map[string]any)
	if !ok {
		return false, mailValidationError("自动续费设置无效")
	}
	autoRenew, ok := body["autoRenew"].(bool)
	if !ok {
		return false, mailValidationError("自动续费设置无效")
	}
	return autoRenew, nil
}

// mailNormalizeCancelInput mirrors normalizeMailSubscriptionCancelInput.
func mailNormalizeCancelInput(value any) (mailSubscriptionCancelInput, *lib.APIError) {
	input := mailSubscriptionCancelInput{}
	body, ok := value.(map[string]any)
	if !ok {
		return input, mailValidationError("请求参数无效")
	}

	refundType, _ := body["refundType"].(string)
	if refundType != "none" && refundType != "full" && refundType != "remaining" {
		return input, mailValidationError("请选择退款方式")
	}
	input.RefundType = refundType

	reason := ""
	if rawReason, present := mailBodyField(body, "reason"); present {
		if _, ok := rawReason.(string); !ok {
			return input, mailValidationError("退款原因无效")
		}
		reason = strings.TrimSpace(mailJSString(rawReason))
	}
	if mailUTF16Length(reason) > mailCancelReasonMaxLength {
		return input, mailValidationError("退款原因不能超过 %d 个字符", mailCancelReasonMaxLength)
	}
	if refundType != "none" && reason == "" {
		return input, mailValidationError("退款时必须填写原因")
	}
	if reason != "" {
		input.Reason = &reason
	}
	return input, nil
}

// mailMergeSourceAPIKey mirrors mergeMailSourceApiKeyForUpdate.
func mailMergeSourceAPIKey(next *string) *string {
	if next == nil || *next == "" || *next == db.MaskedMailSourceAPIKeyValue || strings.HasPrefix(*next, "***") {
		return nil
	}
	return next
}

// mailValidateSourceOutboundURLs mirrors validateMailSourceOutboundUrls.
func mailValidateSourceOutboundURLs(
	ctx context.Context,
	apiURL *string,
	smarterMailURL *string,
) (*string, *string, *lib.APIError) {
	normalizedAPIURL, apiErr := mailValidateOneOutboundURL(ctx, apiURL, "CraneMail API URL")
	if apiErr != nil {
		return nil, nil, apiErr
	}
	normalizedSmarterMailURL, apiErr := mailValidateOneOutboundURL(ctx, smarterMailURL, "SmarterMail URL")
	if apiErr != nil {
		return nil, nil, apiErr
	}
	return normalizedAPIURL, normalizedSmarterMailURL, nil
}

func mailValidateOneOutboundURL(ctx context.Context, value *string, label string) (*string, *lib.APIError) {
	if value == nil {
		return nil, nil
	}
	parsed, err := lib.AssertSafeHTTPURL(ctx, *value, label)
	if err != nil {
		var validationError *lib.OutboundTargetValidationError
		message := "邮箱源地址无效"
		if errors.As(err, &validationError) {
			message = validationError.Message
		}
		apiErr := lib.APIErrorFor(lib.ErrValidationError, message)
		return nil, &apiErr
	}
	normalized := mailNormalizeBaseURL(parsed)
	return &normalized, nil
}

// ==================== query helpers ====================

// mailQueryValue mirrors Fastify's query parser for one key: absent is "not
// present", one occurrence is a string, repeats are an array.
func mailQueryValue(c *gin.Context, key string) (string, bool, bool) {
	values, present := c.Request.URL.Query()[key]
	if !present || len(values) == 0 {
		return "", false, false
	}
	return values[0], true, len(values) > 1
}

// mailParseOptionalPositiveQueryInteger mirrors
// parseOptionalPositiveQueryInteger (repeats and non-canonical values fall back).
func mailParseOptionalPositiveQueryInteger(c *gin.Context, key string, max int64) *int32 {
	value, present, isArray := mailQueryValue(c, key)
	if !present || isArray || value == "" {
		return nil
	}
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok || parsed <= 0 || parsed > max || parsed > math.MaxInt32 {
		return nil
	}
	result := int32(parsed)
	return &result
}

// mailParsePositiveQueryInteger mirrors parsePositiveQueryInteger: a value above
// `max` is *not* clamped, it is rejected and the fallback wins (the TypeScript's
// parseOptionalPositiveQueryInteger returns undefined in that case).
func mailParsePositiveQueryInteger(c *gin.Context, key string, fallback int, max int) int {
	if parsed := mailParseOptionalPositiveQueryInteger(c, key, int64(max)); parsed != nil {
		return int(*parsed)
	}
	return fallback
}

// mailRouteID mirrors parsePositiveRouteId for a path parameter.
func mailRouteID(c *gin.Context, name string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(c.Param(name))
	if !ok || parsed > math.MaxInt32 {
		mailRespondUncaughtAPIError(c)
		return 0, false
	}
	return int32(parsed), true
}

// ==================== shared predicates ====================

// mailSubscriptionCanUseUpstream mirrors mailSubscriptionCanUseUpstream.
func mailSubscriptionCanUseUpstream(subscription models.MailSubscription) bool {
	return subscription.Status == models.MailSubscriptionStatusActive && subscription.ExpiresAt.After(time.Now())
}

// mailNextExpiry mirrors
// `const d = new Date(start); d.setMonth(d.getMonth() + 1)` / `setFullYear(+1)`.
func mailNextExpiry(start time.Time, billingCycle models.MailBillingCycle) time.Time {
	if billingCycle == models.MailBillingCycleMonthly {
		return db.MailAddCalendarMonths(start, 1)
	}
	return db.MailAddCalendarYears(start, 1)
}

// mailUserID returns the authenticated user id (authenticate always ran first).
func mailUserID(c *gin.Context) int32 {
	claims := middleware.CurrentUser(c)
	if claims == nil {
		return 0
	}
	return claims.ID
}

// mailLog writes a mail-module log row (createLog).
func mailLog(ctx context.Context, userID int32, action, content, result string) {
	actor := int64(userID)
	db.CreateLog(ctx, &actor, "mail", action, content, result, db.CreateLogOptions{})
}

// ==================== refund math (ported from routes/mail.ts) ====================

func mailRoundCurrency(value float64) float64 {
	return lib.JSFixed(value, 2)
}

// mailGetPaymentPeriodMs mirrors getMailPaymentPeriodMs.
func mailGetPaymentPeriodMs(remark *string, billingCycle models.MailBillingCycle) int64 {
	if remark != nil && strings.HasPrefix(*remark, mailRenewRemarkPrefix) {
		rest := strings.TrimPrefix(*remark, mailRenewRemarkPrefix)
		if matches := mailRenewMonthsPattern.FindStringSubmatch(rest); matches != nil {
			months, err := strconv.ParseFloat(matches[1], 64)
			if err == nil && months == math.Trunc(months) && months > 0 && months <= 9007199254740991 {
				if billingCycle == models.MailBillingCycleYearly {
					return int64((months / 12) * 365 * float64(mailDayMilliseconds))
				}
				return int64(months) * 30 * mailDayMilliseconds
			}
		}
	}
	if billingCycle == models.MailBillingCycleMonthly {
		return 30 * mailDayMilliseconds
	}
	return 365 * mailDayMilliseconds
}

// mailRefundableAmount mirrors getMailRefundableAmount.
func mailRefundableAmount(
	ctx context.Context,
	tx pgx.Tx,
	detail *db.MailSubscriptionAdminDetail,
	now time.Time,
) (float64, float64, error) {
	inputs, err := db.MailLoadRefundInputs(ctx, tx, detail.Subscription.UserID, detail.Subscription.CreatedAt)
	if err != nil {
		return 0, 0, err
	}
	if !inputs.PurchaseFound {
		return 0, 0, nil
	}

	totalPaid := 0.0
	for _, record := range inputs.Payments {
		totalPaid += mailDecimalAbsNumber(record.Amount)
	}
	totalRefunded := 0.0
	for _, amount := range inputs.Refunds {
		totalRefunded += mailDecimalAbsNumber(amount)
	}
	maxRefundable := mailRoundCurrency(math.Max(0, totalPaid-totalRefunded))

	remainingValue := 0.0
	periodEnd := detail.Subscription.ExpiresAt.UnixMilli()
	for index := len(inputs.Payments) - 1; index >= 0; index-- {
		record := inputs.Payments[index]
		periodMs := mailGetPaymentPeriodMs(record.Remark, detail.Plan.BillingCycle)
		periodStart := periodEnd - periodMs
		if periodEnd > now.UnixMilli() {
			unusedRatio := math.Min(math.Max(float64(periodEnd-now.UnixMilli()), 0)/float64(periodMs), 1)
			remainingValue += mailDecimalAbsNumber(record.Amount) * unusedRatio
		}
		periodEnd = periodStart
	}

	return maxRefundable, mailRoundCurrency(math.Min(remainingValue, maxRefundable)), nil
}

// mailResumeSubscriptionDomains mirrors resumeMailSubscriptionDomains.
func mailResumeSubscriptionDomains(ctx context.Context, subscriptionID int32, userID int32) error {
	subscription, err := db.MailGetSubscriptionByID(ctx, subscriptionID)
	if err != nil {
		return err
	}
	if subscription == nil || !mailSubscriptionCanUseUpstream(subscription.Subscription) {
		return nil
	}

	for _, domain := range subscription.Domains {
		if resumeErr := mailCraneResumeDomain(ctx, subscription.Source, domain.Domain.Domain); resumeErr != nil {
			fmt.Fprintf(os.Stderr, "[Mail] Failed to resume renewed mail domain %s; pending retry: %v\n",
				domain.Domain.Domain, resumeErr)
			if _, updateErr := db.MailUpdateDomainStatusForActiveSubscription(
				ctx, domain.Domain.ID, models.MailDomainStatusSuspended, time.Now()); updateErr != nil {
				fmt.Fprintf(os.Stderr, "[Mail] Failed to mark mail domain %d suspended: %v\n", domain.Domain.ID, updateErr)
			}
			mailLog(ctx, userID, "mail_upstream_resume_pending_retry",
				"Upstream resume pending retry for mail domain "+domain.Domain.Domain, "failed")
			continue
		}

		status := models.MailDomainStatusPending
		if domain.Domain.VerifiedAt != nil {
			status = models.MailDomainStatusVerified
		}
		if _, updateErr := db.MailUpdateDomainStatusForActiveSubscription(
			ctx, domain.Domain.ID, status, time.Now()); updateErr != nil {
			fmt.Fprintf(os.Stderr, "[Mail] Failed to update mail domain %d status: %v\n", domain.Domain.ID, updateErr)
		}
	}
	return nil
}

// mailSubscriptionAffCode loads the aff code bound to a subscription, if any.
func mailSubscriptionAffCode(
	ctx context.Context,
	tx pgx.Tx,
	subscriptionID int32,
) (*models.MailSubscriptionAffBinding, *models.AffCode, error) {
	binding, err := db.MailFindAffBindingTx(ctx, tx, subscriptionID)
	if err != nil || binding == nil {
		return nil, nil, err
	}
	affCode, err := db.MailFindAffCodeByIDTx(ctx, tx, binding.AffCodeID)
	if err != nil {
		return nil, nil, err
	}
	return binding, affCode, nil
}

// mailInsufficientBalanceMessage mirrors the interpolated balance error.
func mailInsufficientBalanceMessage(required, balance float64) string {
	return fmt.Sprintf("余额不足，需要 ¥%.2f，当前余额 ¥%.2f", required, balance)
}

// ==================== response views ====================

type mailSourceView struct {
	ID               int32  `json:"id"`
	Name             string `json:"name"`
	Code             string `json:"code"`
	APIURL           string `json:"apiUrl"`
	APIKey           string `json:"apiKey"`
	SmarterMailURL   string `json:"smarterMailUrl"`
	Enabled          bool   `json:"enabled"`
	SortOrder        int32  `json:"sortOrder"`
	CreatedAt        string `json:"createdAt"`
	UpdatedAt        string `json:"updatedAt"`
	APIKeyConfigured bool   `json:"apiKeyConfigured"`
}

func mailNewSourceView(source models.MailSource) mailSourceView {
	return mailSourceView{
		ID:               source.ID,
		Name:             source.Name,
		Code:             source.Code,
		APIURL:           source.APIURL,
		APIKey:           "",
		SmarterMailURL:   source.SmarterMailURL,
		Enabled:          source.Enabled,
		SortOrder:        source.SortOrder,
		CreatedAt:        mailISOTime(source.CreatedAt),
		UpdatedAt:        mailISOTime(source.UpdatedAt),
		APIKeyConfigured: source.APIKey != "",
	}
}

type mailSourceWithStatsView struct {
	mailSourceView
	PlanCount         int64 `json:"planCount"`
	SubscriptionCount int64 `json:"subscriptionCount"`
	DomainCount       int64 `json:"domainCount"`
}

type mailPlanView struct {
	ID           int32          `json:"id"`
	SourceID     int32          `json:"sourceId"`
	Name         string         `json:"name"`
	Description  *string        `json:"description"`
	DomainLimit  int32          `json:"domainLimit"`
	DiskLimitGB  int32          `json:"diskLimitGb"`
	BillingCycle string         `json:"billingCycle"`
	Price        models.Decimal `json:"price"`
	Enabled      bool           `json:"enabled"`
	SortOrder    int32          `json:"sortOrder"`
	CreatedAt    string         `json:"createdAt"`
	UpdatedAt    string         `json:"updatedAt"`
}

func mailNewPlanView(plan models.MailPlan) mailPlanView {
	return mailPlanView{
		ID:           plan.ID,
		SourceID:     plan.SourceID,
		Name:         plan.Name,
		Description:  plan.Description,
		DomainLimit:  plan.DomainLimit,
		DiskLimitGB:  plan.DiskLimitGB,
		BillingCycle: string(plan.BillingCycle),
		Price:        plan.Price,
		Enabled:      plan.Enabled,
		SortOrder:    plan.SortOrder,
		CreatedAt:    mailISOTime(plan.CreatedAt),
		UpdatedAt:    mailISOTime(plan.UpdatedAt),
	}
}

type mailPlanWithSourceView struct {
	mailPlanView
	Source mailSourceView `json:"source"`
}

type mailUserSelectView struct {
	ID            int32   `json:"id"`
	Username      string  `json:"username"`
	Email         *string `json:"email"`
	AvatarStyle   *string `json:"avatarStyle"`
	AvatarBadgeID *string `json:"avatarBadgeId"`
}

func mailNewUserSelectView(user db.MailUserSelect) mailUserSelectView {
	return mailUserSelectView{
		ID:            user.ID,
		Username:      user.Username,
		Email:         user.Email,
		AvatarStyle:   user.AvatarStyle,
		AvatarBadgeID: user.AvatarBadgeID,
	}
}

type mailSubscriptionBaseView struct {
	ID          int32  `json:"id"`
	UserID      int32  `json:"userId"`
	SourceID    int32  `json:"sourceId"`
	PlanID      int32  `json:"planId"`
	Status      string `json:"status"`
	DomainLimit int32  `json:"domainLimit"`
	DiskLimitGB int32  `json:"diskLimitGb"`
	ExpiresAt   string `json:"expiresAt"`
	AutoRenew   bool   `json:"autoRenew"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

func mailNewSubscriptionBaseView(subscription models.MailSubscription) mailSubscriptionBaseView {
	return mailSubscriptionBaseView{
		ID:          subscription.ID,
		UserID:      subscription.UserID,
		SourceID:    subscription.SourceID,
		PlanID:      subscription.PlanID,
		Status:      string(subscription.Status),
		DomainLimit: subscription.DomainLimit,
		DiskLimitGB: subscription.DiskLimitGB,
		ExpiresAt:   mailISOTime(subscription.ExpiresAt),
		AutoRenew:   subscription.AutoRenew,
		CreatedAt:   mailISOTime(subscription.CreatedAt),
		UpdatedAt:   mailISOTime(subscription.UpdatedAt),
	}
}

type mailSubscriptionView struct {
	mailSubscriptionBaseView
	User   mailUserSelectView `json:"user"`
	Source mailSourceView     `json:"source"`
	Plan   mailPlanView       `json:"plan"`
}

// mailDomainSubscriptionView mirrors the admin-domain include:
// `subscription: { include: { user: <select> } }` — no source/plan nested there.
type mailDomainSubscriptionView struct {
	mailSubscriptionBaseView
	User mailUserSelectView `json:"user"`
}

func mailNewSubscriptionView(detail db.MailSubscriptionDetail) mailSubscriptionView {
	user := mailUserSelectView{}
	if detail.User != nil {
		user = mailNewUserSelectView(*detail.User)
	}
	return mailSubscriptionView{
		mailSubscriptionBaseView: mailNewSubscriptionBaseView(detail.Subscription),
		User:                     user,
		Source:                   mailNewSourceView(detail.Source),
		Plan:                     mailNewPlanView(detail.Plan),
	}
}

type mailCountView struct {
	Accounts int64 `json:"accounts"`
}

type mailDomainView struct {
	ID                      int32                      `json:"id"`
	SubscriptionID          int32                      `json:"subscriptionId"`
	SourceID                int32                      `json:"sourceId"`
	Domain                  string                     `json:"domain"`
	Status                  string                     `json:"status"`
	AdminUsername           *string                    `json:"adminUsername"`
	AdminPassword           string                     `json:"adminPassword"`
	DiskUsedMB              int32                      `json:"diskUsedMb"`
	VerifiedAt              *string                    `json:"verifiedAt"`
	CreatedAt               string                     `json:"createdAt"`
	UpdatedAt               string                     `json:"updatedAt"`
	Subscription            mailDomainSubscriptionView `json:"subscription"`
	Source                  mailSourceView             `json:"source"`
	Count                   mailCountView              `json:"_count"`
	AdminPasswordConfigured bool                       `json:"adminPasswordConfigured"`
}

func mailNewDomainView(row db.MailDomainWithRelations) mailDomainView {
	return mailDomainView{
		ID:             row.Domain.ID,
		SubscriptionID: row.Domain.SubscriptionID,
		SourceID:       row.Domain.SourceID,
		Domain:         row.Domain.Domain,
		Status:         string(row.Domain.Status),
		AdminUsername:  row.Domain.AdminUsername,
		AdminPassword:  "",
		DiskUsedMB:     row.Domain.DiskUsedMB,
		VerifiedAt:     mailISOTimePtr(row.Domain.VerifiedAt),
		CreatedAt:      mailISOTime(row.Domain.CreatedAt),
		UpdatedAt:      mailISOTime(row.Domain.UpdatedAt),
		Subscription: mailDomainSubscriptionView{
			mailSubscriptionBaseView: mailNewSubscriptionBaseView(row.Subscription),
			User:                     mailNewUserSelectView(row.User),
		},
		Source:                  mailNewSourceView(row.Source),
		Count:                   mailCountView{Accounts: row.AccountCount},
		AdminPasswordConfigured: row.Domain.AdminPassword != nil && *row.Domain.AdminPassword != "",
	}
}

// ==================== admin: mail sources ====================

func mailAdminListSources(c *gin.Context) {
	ctx := c.Request.Context()
	sources, err := db.MailGetAllSources(ctx, true)
	if err != nil {
		mailRespondInternal(c)
		return
	}

	views := make([]mailSourceWithStatsView, 0, len(sources))
	for _, source := range sources {
		stats, statsErr := db.MailGetSourceStats(ctx, source.ID)
		if statsErr != nil {
			mailRespondInternal(c)
			return
		}
		views = append(views, mailSourceWithStatsView{
			mailSourceView:    mailNewSourceView(source),
			PlanCount:         stats.PlanCount,
			SubscriptionCount: stats.SubscriptionCount,
			DomainCount:       stats.DomainCount,
		})
	}
	c.JSON(http.StatusOK, gin.H{"sources": views})
}

func mailAdminCreateSource(c *gin.Context) {
	ctx := c.Request.Context()
	body, present, ok := mailReadRequestBody(c)
	if !ok {
		return
	}
	if !present {
		mailRespondInternal(c)
		return
	}

	input, apiErr := mailNormalizeSourceInput(body, true)
	if apiErr != nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	existing, err := db.MailGetSourceByCode(ctx, *input.Code)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if existing != nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	apiURL, smarterMailURL, apiErr := mailValidateSourceOutboundURLs(ctx, input.APIURL, input.SmarterMailURL)
	if apiErr != nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	sortOrder := int32(0)
	if input.SortOrder != nil {
		sortOrder = *input.SortOrder
	}

	source, err := db.MailCreateSource(ctx, db.MailCreateSourceInput{
		Name:           *input.Name,
		Code:           *input.Code,
		APIURL:         *apiURL,
		APIKey:         *input.APIKey,
		SmarterMailURL: *smarterMailURL,
		Enabled:        enabled,
		SortOrder:      sortOrder,
	})
	if err != nil {
		mailRespondInternal(c)
		return
	}

	mailLog(ctx, mailUserID(c), "create_mail_source", "Created mail source: "+*input.Name, "success")
	c.JSON(http.StatusOK, gin.H{"source": mailNewSourceView(source)})
}

func mailAdminUpdateSource(c *gin.Context) {
	ctx := c.Request.Context()
	id, ok := mailRouteID(c, "id")
	if !ok {
		return
	}

	body, present, ok := mailReadRequestBody(c)
	if !ok {
		return
	}
	if !present {
		mailRespondInternal(c)
		return
	}
	input, apiErr := mailNormalizeSourceInput(body, false)
	if apiErr != nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	existing, err := db.MailGetSourceByID(ctx, id)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if existing == nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	if input.Code != nil && *input.Code != existing.Code {
		codeExists, err := db.MailGetSourceByCode(ctx, *input.Code)
		if err != nil {
			mailRespondInternal(c)
			return
		}
		if codeExists != nil {
			mailRespondUncaughtAPIError(c)
			return
		}
	}

	apiURL, smarterMailURL, apiErr := mailValidateSourceOutboundURLs(ctx, input.APIURL, input.SmarterMailURL)
	if apiErr != nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	source, err := db.MailUpdateSource(ctx, id, db.MailUpdateSourceInput{
		Name:           input.Name,
		Code:           input.Code,
		APIURL:         apiURL,
		APIKey:         mailMergeSourceAPIKey(input.APIKey),
		SmarterMailURL: smarterMailURL,
		Enabled:        input.Enabled,
		SortOrder:      input.SortOrder,
	})
	if err != nil {
		mailRespondInternal(c)
		return
	}

	mailLog(ctx, mailUserID(c), "update_mail_source", fmt.Sprintf("Updated mail source #%d", id), "success")
	c.JSON(http.StatusOK, gin.H{"source": mailNewSourceView(source)})
}

func mailAdminDeleteSource(c *gin.Context) {
	ctx := c.Request.Context()
	id, ok := mailRouteID(c, "id")
	if !ok {
		return
	}

	source, err := db.MailGetSourceByID(ctx, id)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if source == nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	stats, err := db.MailGetSourceStats(ctx, id)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if stats.SubscriptionCount > 0 || stats.DomainCount > 0 {
		mailRespondUncaughtAPIError(c)
		return
	}

	if err := db.MailDeleteSource(ctx, id); err != nil {
		mailRespondInternal(c)
		return
	}

	mailLog(ctx, mailUserID(c), "delete_mail_source", "Deleted mail source: "+source.Name, "success")
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ==================== admin: mail plans ====================

func mailAdminListPlans(c *gin.Context) {
	ctx := c.Request.Context()
	plans, err := db.MailGetAllPlans(ctx)
	if err != nil {
		mailRespondInternal(c)
		return
	}

	views := make([]mailPlanWithSourceView, 0, len(plans))
	for _, item := range plans {
		views = append(views, mailPlanWithSourceView{
			mailPlanView: mailNewPlanView(item.Plan),
			Source:       mailNewSourceView(item.Source),
		})
	}
	c.JSON(http.StatusOK, gin.H{"plans": views})
}

func mailAdminCreatePlan(c *gin.Context) {
	ctx := c.Request.Context()
	rawBody, present, ok := mailReadRequestBody(c)
	if !ok {
		return
	}
	if !present {
		mailRespondInternal(c)
		return
	}
	body := mailObjectFields(rawBody)

	input, apiErr := mailNormalizePlanInput(body, true)
	if apiErr != nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	source, err := db.MailGetSourceByID(ctx, *input.SourceID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if source == nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	sortOrder := int32(0)
	if input.SortOrder != nil {
		sortOrder = *input.SortOrder
	}

	plan, err := db.MailCreatePlan(ctx, db.MailCreatePlanInput{
		SourceID:     *input.SourceID,
		Name:         *input.Name,
		Description:  input.Description,
		DomainLimit:  *input.DomainLimit,
		DiskLimitGB:  *input.DiskLimitGB,
		BillingCycle: *input.BillingCycle,
		Price:        *input.Price,
		Enabled:      enabled,
		SortOrder:    sortOrder,
	})
	if err != nil {
		mailRespondInternal(c)
		return
	}

	mailLog(ctx, mailUserID(c), "create_mail_plan", "Created mail plan: "+*input.Name, "success")
	c.JSON(http.StatusOK, gin.H{"plan": mailNewPlanView(plan)})
}

func mailAdminUpdatePlan(c *gin.Context) {
	ctx := c.Request.Context()
	id, ok := mailRouteID(c, "id")
	if !ok {
		return
	}

	rawBody, present, ok := mailReadRequestBody(c)
	if !ok {
		return
	}
	if !present {
		mailRespondInternal(c)
		return
	}
	body := mailObjectFields(rawBody)

	input, apiErr := mailNormalizePlanInput(body, false)
	if apiErr != nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	existing, err := db.MailGetPlanByID(ctx, id)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if existing == nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	if input.SourceID != nil {
		source, err := db.MailGetSourceByID(ctx, *input.SourceID)
		if err != nil {
			mailRespondInternal(c)
			return
		}
		if source == nil {
			mailRespondUncaughtAPIError(c)
			return
		}
	}

	plan, err := db.MailUpdatePlan(ctx, id, db.MailUpdatePlanInput{
		SourceID:     input.SourceID,
		Name:         input.Name,
		Description:  input.Description,
		DomainLimit:  input.DomainLimit,
		DiskLimitGB:  input.DiskLimitGB,
		BillingCycle: input.BillingCycle,
		Price:        input.Price,
		Enabled:      input.Enabled,
		SortOrder:    input.SortOrder,
	})
	if err != nil {
		mailRespondInternal(c)
		return
	}

	mailLog(ctx, mailUserID(c), "update_mail_plan", fmt.Sprintf("Updated mail plan #%d", id), "success")
	c.JSON(http.StatusOK, gin.H{"plan": mailNewPlanView(plan)})
}

func mailAdminDeletePlan(c *gin.Context) {
	ctx := c.Request.Context()
	id, ok := mailRouteID(c, "id")
	if !ok {
		return
	}

	plan, err := db.MailGetPlanByID(ctx, id)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if plan == nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	subscriptionCount, err := db.MailCountSubscriptionsByPlan(ctx, id)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if subscriptionCount > 0 {
		mailRespondUncaughtAPIError(c)
		return
	}

	if err := db.MailDeletePlan(ctx, id); err != nil {
		mailRespondInternal(c)
		return
	}

	mailLog(ctx, mailUserID(c), "delete_mail_plan", "Deleted mail plan: "+plan.Plan.Name, "success")
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ==================== admin: mail subscriptions ====================

func mailAdminListSubscriptions(c *gin.Context) {
	ctx := c.Request.Context()

	sourceID := mailParseOptionalPositiveQueryInteger(c, "sourceId", math.MaxInt32)
	status := ""
	if value, present, isArray := mailQueryValue(c, "status"); present && !isArray {
		status = value
	}
	var search *string
	if value, present, isArray := mailQueryValue(c, "search"); present {
		if isArray {
			// `search?.trim()` on an array throws in the TypeScript.
			mailRespondInternal(c)
			return
		}
		if value != "" {
			search = &value
		}
	}
	page := mailParsePositiveQueryInteger(c, "page", 1, math.MaxInt32)
	pageSize := mailParsePositiveQueryInteger(c, "pageSize", 20, 100)

	result, err := db.MailGetAllSubscriptions(ctx, db.MailSubscriptionListOptions{
		SourceID: sourceID,
		Status:   status,
		Search:   search,
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		mailRespondInternal(c)
		return
	}

	views := make([]mailSubscriptionView, 0, len(result.Subscriptions))
	for _, detail := range result.Subscriptions {
		views = append(views, mailNewSubscriptionView(detail))
	}
	c.JSON(http.StatusOK, gin.H{"subscriptions": views, "total": result.Total})
}

func mailAdminCancelSubscription(c *gin.Context) {
	ctx := c.Request.Context()
	subscriptionID, ok := mailRouteID(c, "id")
	if !ok {
		return
	}

	rawBody, present, ok := mailReadRequestBody(c)
	if !ok {
		return
	}
	if !present {
		mailRespondInternal(c)
		return
	}
	input, apiErr := mailNormalizeCancelInput(rawBody)
	if apiErr != nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	subscription, err := db.MailGetSubscriptionAdminDetail(ctx, subscriptionID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if subscription == nil {
		mailRespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "订阅不存在")
		return
	}

	source, err := db.MailGetSourceByID(ctx, subscription.Subscription.SourceID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if source == nil {
		mailRespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "邮箱源不存在")
		return
	}

	// Remove the upstream domains first: a failed remote delete must not remove the
	// local subscription, or the残留 resource would become invisible.
	for _, domain := range subscription.Domains {
		if deleteErr := mailCraneDeleteDomain(ctx, *source, domain.Domain); deleteErr != nil {
			fmt.Fprintf(os.Stderr, "[AdminMailCancel] Failed to delete domain %s from CraneMail: %v\n",
				domain.Domain, deleteErr)
			mailRespondAPIError(c, http.StatusBadGateway, lib.ErrUpstreamError,
				fmt.Sprintf("删除远端域名 %s 失败: %s", domain.Domain, deleteErr.Error()))
			return
		}
	}

	refundAmount := 0.0

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if input.RefundType != "none" {
		locked, lockErr := db.TryAdvisoryTransactionLock(ctx, tx, db.UserBalanceLockNamespace, subscription.UserID)
		if lockErr != nil {
			mailRespondInternal(c)
			return
		}
		if !locked {
			mailRespondUncaughtAPIError(c)
			return
		}

		maxRefundable, remainingRefundable, refundErr := mailRefundableAmount(ctx, tx, subscription, time.Now())
		if refundErr != nil {
			mailRespondInternal(c)
			return
		}
		if input.RefundType == "full" {
			refundAmount = maxRefundable
		} else {
			refundAmount = remainingRefundable
		}

		if refundAmount <= 0 {
			if deleteErr := db.MailDeleteSubscriptionTx(ctx, tx, subscriptionID); deleteErr != nil {
				mailRespondInternal(c)
				return
			}
			if commitErr := tx.Commit(ctx); commitErr != nil {
				mailRespondInternal(c)
				return
			}
			mailLog(ctx, mailUserID(c), "admin_cancel_mail_subscription",
				fmt.Sprintf("Admin cancelled mail subscription #%d for user %s (refund: %s, amount: %v)",
					subscriptionID, subscription.Username, input.RefundType, refundAmount), "success")
			c.JSON(http.StatusOK, gin.H{"success": true, "refundAmount": refundAmount, "refundType": input.RefundType})
			return
		}

		before, beforeAfter, incrementErr := db.MailIncrementUserBalanceTx(
			ctx, tx, subscription.UserID, mailDecimalFromNumber(refundAmount))
		if incrementErr != nil {
			mailRespondInternal(c)
			return
		}
		remark := fmt.Sprintf("管理员退订邮箱 - %s（%s）原因：%s", subscription.Plan.Name,
			mailRefundTypeLabel(input.RefundType), mailStringValue(input.Reason))
		if logErr := db.MailInsertBalanceLogTx(ctx, tx, subscription.UserID, models.BalanceLogTypeRefund,
			mailDecimalFromNumber(refundAmount), before, beforeAfter, &remark); logErr != nil {
			mailRespondInternal(c)
			return
		}
	}

	if deleteErr := db.MailDeleteSubscriptionTx(ctx, tx, subscriptionID); deleteErr != nil {
		mailRespondInternal(c)
		return
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		mailRespondInternal(c)
		return
	}

	mailLog(ctx, mailUserID(c), "admin_cancel_mail_subscription",
		fmt.Sprintf("Admin cancelled mail subscription #%d for user %s (refund: %s, amount: %v)",
			subscriptionID, subscription.Username, input.RefundType, refundAmount), "success")

	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"refundAmount": refundAmount,
		"refundType":   input.RefundType,
	})
}

func mailRefundTypeLabel(refundType string) string {
	if refundType == "full" {
		return "全额退款"
	}
	return "剩余价值退款"
}

func mailStringValue(value *string) string {
	if value == nil {
		return "undefined"
	}
	return *value
}

// ==================== admin: mail domains ====================

func mailAdminListDomains(c *gin.Context) {
	ctx := c.Request.Context()

	sourceID := mailParseOptionalPositiveQueryInteger(c, "sourceId", math.MaxInt32)
	status := ""
	if value, present, isArray := mailQueryValue(c, "status"); present && !isArray {
		status = value
	}
	var search *string
	if value, present, isArray := mailQueryValue(c, "search"); present {
		if isArray {
			mailRespondInternal(c)
			return
		}
		if value != "" {
			search = &value
		}
	}
	page := mailParsePositiveQueryInteger(c, "page", 1, math.MaxInt32)
	pageSize := mailParsePositiveQueryInteger(c, "pageSize", 20, 100)

	result, err := db.MailGetAllDomains(ctx, db.MailDomainListOptions{
		SourceID: sourceID,
		Status:   status,
		Search:   search,
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		mailRespondInternal(c)
		return
	}

	views := make([]mailDomainView, 0, len(result.Domains))
	for _, row := range result.Domains {
		views = append(views, mailNewDomainView(row))
	}
	c.JSON(http.StatusOK, gin.H{"domains": views, "total": result.Total})
}

// ==================== user: available offerings ====================

func mailUserSources(c *gin.Context) {
	ctx := c.Request.Context()

	available, err := db.MailHasAvailableOffering(ctx)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if !available {
		c.JSON(http.StatusOK, gin.H{"sources": []any{}})
		return
	}

	sources, err := db.MailGetAllSources(ctx, false)
	if err != nil {
		mailRespondInternal(c)
		return
	}

	views := make([]gin.H, 0, len(sources))
	for _, source := range sources {
		plans, err := db.MailGetPlansBySource(ctx, source.ID, false)
		if err != nil {
			mailRespondInternal(c)
			return
		}
		planViews := make([]gin.H, 0, len(plans))
		for _, plan := range plans {
			planViews = append(planViews, gin.H{
				"id":           plan.ID,
				"name":         plan.Name,
				"description":  plan.Description,
				"domainLimit":  plan.DomainLimit,
				"diskLimitGb":  plan.DiskLimitGB,
				"billingCycle": plan.BillingCycle,
				"price":        mailDecimalFloat(plan.Price),
			})
		}
		views = append(views, gin.H{
			"id":    source.ID,
			"name":  source.Name,
			"code":  source.Code,
			"plans": planViews,
		})
	}
	c.JSON(http.StatusOK, gin.H{"sources": views})
}

// ==================== user: aff code validation ====================

func mailUserValidateAff(c *gin.Context) {
	ctx := c.Request.Context()
	rawBody, bodyPresent, ok := mailReadRequestBody(c)
	if !ok {
		return
	}
	if !bodyPresent {
		mailRespondDestructureError(c, "code")
		return
	}
	body := mailObjectFields(rawBody)

	rawCode, present := mailBodyField(body, "code")
	if !present || mailFalsy(rawCode) {
		c.JSON(http.StatusOK, gin.H{"valid": false, "error": "请输入优惠码"})
		return
	}
	code, isString := rawCode.(string)
	if !isString {
		// `code.trim()` on a non-string throws a TypeError.
		mailRespondJSError(c, "code.trim is not a function")
		return
	}
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		c.JSON(http.StatusOK, gin.H{"valid": false, "error": "请输入优惠码"})
		return
	}

	validation, err := db.AffValidateMailCode(ctx, trimmed, mailUserID(c))
	if err != nil {
		mailRespondInternal(c)
		return
	}

	response := gin.H{"valid": validation.Valid}
	if validation.DiscountRate != nil {
		response["discountRate"] = *validation.DiscountRate
	}
	if validation.CommissionRate != nil {
		response["commissionRate"] = *validation.CommissionRate
	}
	if validation.Error != "" {
		response["error"] = validation.Error
	}
	c.JSON(http.StatusOK, response)
}

// ==================== user: my subscription ====================

type mailAffCodeView struct {
	Code         string  `json:"code"`
	DiscountRate float64 `json:"discountRate"`
}

type mailAffBindingView struct {
	AffCode mailAffCodeView `json:"affCode"`
}

type mailMyPlanView struct {
	ID           int32   `json:"id"`
	Name         string  `json:"name"`
	DomainLimit  int32   `json:"domainLimit"`
	DiskLimitGB  int32   `json:"diskLimitGb"`
	BillingCycle string  `json:"billingCycle"`
	Price        float64 `json:"price"`
}

type mailMyDomainView struct {
	ID           int32   `json:"id"`
	Domain       string  `json:"domain"`
	Status       string  `json:"status"`
	AccountCount int     `json:"accountCount"`
	DiskUsedMB   int32   `json:"diskUsedMb"`
	VerifiedAt   *string `json:"verifiedAt"`
	CreatedAt    string  `json:"createdAt"`
}

func mailNewMyDomainView(domain models.MailDomain, accountCount int) mailMyDomainView {
	return mailMyDomainView{
		ID:           domain.ID,
		Domain:       domain.Domain,
		Status:       string(domain.Status),
		AccountCount: accountCount,
		DiskUsedMB:   domain.DiskUsedMB,
		VerifiedAt:   mailISOTimePtr(domain.VerifiedAt),
		CreatedAt:    mailISOTime(domain.CreatedAt),
	}
}

func mailUserGetSubscription(c *gin.Context) {
	ctx := c.Request.Context()
	userID := mailUserID(c)

	subscription, err := db.MailGetUserSubscription(ctx, userID, nil)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if subscription == nil {
		c.JSON(http.StatusOK, gin.H{"subscription": nil})
		return
	}

	usage, err := db.MailGetSubscriptionUsageStats(ctx, subscription.Subscription.ID)
	if err != nil {
		mailRespondInternal(c)
		return
	}

	binding, affCode, err := mailSubscriptionAffCode(ctx, nil, subscription.Subscription.ID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	var affBinding *mailAffBindingView
	if binding != nil && affCode != nil {
		affBinding = &mailAffBindingView{AffCode: mailAffCodeView{
			Code:         affCode.Code,
			DiscountRate: mailDecimalFloat(affCode.DiscountRate),
		}}
	}

	domains := make([]mailMyDomainView, 0, len(subscription.Domains))
	for _, item := range subscription.Domains {
		domains = append(domains, mailNewMyDomainView(item.Domain, len(item.Accounts)))
	}

	diskUsedGB := mailJSRound(float64(usage.DiskUsedMB)/1024*100) / 100

	c.JSON(http.StatusOK, gin.H{
		"subscription": gin.H{
			"id":        subscription.Subscription.ID,
			"status":    subscription.Subscription.Status,
			"expiresAt": mailISOTime(subscription.Subscription.ExpiresAt),
			"autoRenew": subscription.Subscription.AutoRenew,
			"source": gin.H{
				"id":   subscription.Source.ID,
				"name": subscription.Source.Name,
				"code": subscription.Source.Code,
			},
			"plan": mailMyPlanView{
				ID:           subscription.Plan.ID,
				Name:         subscription.Plan.Name,
				DomainLimit:  subscription.Subscription.DomainLimit,
				DiskLimitGB:  subscription.Subscription.DiskLimitGB,
				BillingCycle: string(subscription.Plan.BillingCycle),
				Price:        mailDecimalFloat(subscription.Plan.Price),
			},
			"usage": gin.H{
				"domainCount":  usage.DomainCount,
				"accountCount": usage.AccountCount,
				"diskUsedGb":   diskUsedGB,
			},
			"domains":    domains,
			"affBinding": affBinding,
		},
	})
}

func mailUserSetAutoRenew(c *gin.Context) {
	ctx := c.Request.Context()
	userID := mailUserID(c)

	rawBody, present, ok := mailReadRequestBody(c)
	if !ok {
		return
	}
	if !present {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrValidationError, "自动续费设置无效")
		return
	}
	autoRenew, apiErr := mailNormalizeSubscriptionAutoRenewInput(rawBody)
	if apiErr != nil {
		mailRespondAPIError(c, http.StatusBadRequest, apiErr.Code, apiErr.Details)
		return
	}

	subscription, err := db.MailGetUserSubscription(ctx, userID, nil)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if subscription == nil {
		mailRespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "您没有邮箱订阅")
		return
	}

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := db.TryAdvisoryTransactionLock(ctx, tx, db.MailSubscriptionLockNamespace, userID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if !locked {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "邮箱订阅正在处理，请稍后重试")
		return
	}

	updated, err := db.MailSetSubscriptionAutoRenewTx(ctx, tx, subscription.Subscription.ID, userID, autoRenew)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if updated != 1 {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrNotFound, "您没有邮箱订阅")
		return
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		mailRespondInternal(c)
		return
	}

	status := "Disabled"
	if autoRenew {
		status = "Enabled"
	}
	mailLog(ctx, userID, "set_mail_auto_renew",
		fmt.Sprintf("%s mail subscription auto-renew for subscription #%d", status, subscription.Subscription.ID), "success")

	c.JSON(http.StatusOK, gin.H{"autoRenew": autoRenew})
}

// ==================== user: purchase / renew ====================

func mailUserPurchaseSubscription(c *gin.Context) {
	ctx := c.Request.Context()
	userID := mailUserID(c)

	rawBody, present, ok := mailReadRequestBody(c)
	if !ok {
		return
	}

	body, isObject := rawBody.(map[string]any)
	if !present || !isObject {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrValidationError, "请求参数无效")
		return
	}

	rawPlanID, present := mailBodyField(body, "planId")
	if !present {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrValidationError, "邮箱方案 无效")
		return
	}
	planIDValue, apiErr := mailRequireSafeInteger(rawPlanID, "邮箱方案")
	if apiErr != nil {
		mailRespondAPIError(c, http.StatusBadRequest, apiErr.Code, apiErr.Details)
		return
	}
	if planIDValue <= 0 {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrValidationError, "邮箱方案无效")
		return
	}
	planID := int32(planIDValue)

	var affCodeInput *string
	if rawAffCode, present := mailBodyField(body, "affCode"); present && rawAffCode != nil {
		code, isString := rawAffCode.(string)
		if !isString {
			mailRespondAPIError(c, http.StatusBadRequest, lib.ErrValidationError, "优惠码无效")
			return
		}
		normalized := strings.ToUpper(strings.TrimSpace(code))
		if normalized != "" {
			if mailUTF16Length(normalized) > mailAffCodeMaxLength {
				mailRespondAPIError(c, http.StatusBadRequest, lib.ErrValidationError,
					fmt.Sprintf("优惠码不能超过 %d 个字符", mailAffCodeMaxLength))
				return
			}
			affCodeInput = &normalized
		}
	}

	plan, err := db.MailGetPlanByID(ctx, planID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if plan == nil || !plan.Plan.Enabled {
		mailRespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "方案不存在或已下架")
		return
	}

	source := plan.Source
	if !source.Enabled {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "该邮箱源已停用")
		return
	}

	existing, err := db.MailFindExistingSubscriptionForPurchase(ctx, nil, userID, source.ID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	binding, boundAffCode, err := mailSubscriptionAffCode(ctx, nil, existingSubscriptionID(existing))
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if existing != nil && existing.Status == models.MailSubscriptionStatusActive && existing.ExpiresAt.After(time.Now()) {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed,
			"您在该邮箱源已有有效订阅，请续费或等待过期后再购买")
		return
	}

	originalPrice := mailDecimalFloat(plan.Plan.Price)
	finalPrice := originalPrice
	discountAmount := 0.0
	var validatedAffCodeID *int32
	validatedDiscountRate := 0.0

	if binding != nil && boundAffCode != nil && boundAffCode.Enabled {
		validatedAffCodeID = &boundAffCode.ID
		validatedDiscountRate = mailDecimalFloat(boundAffCode.DiscountRate)
		discountAmount = lib.CalculateDiscountAmount(originalPrice, validatedDiscountRate)
		finalPrice = lib.CalculateDiscountedPrice(originalPrice, validatedDiscountRate)
	} else if affCodeInput != nil {
		validation, err := db.AffValidateMailCode(ctx, *affCodeInput, userID)
		if err != nil {
			mailRespondInternal(c)
			return
		}
		if !validation.Valid {
			message := validation.Error
			if message == "" {
				message = "优惠码无效"
			}
			mailRespondAPIError(c, http.StatusBadRequest, lib.ErrValidationError, message)
			return
		}
		if validation.AffCode == nil || validation.DiscountRate == nil {
			mailRespondInternal(c)
			return
		}
		validatedAffCodeID = &validation.AffCode.ID
		validatedDiscountRate = *validation.DiscountRate
		discountAmount = lib.CalculateDiscountAmount(originalPrice, validatedDiscountRate)
		finalPrice = lib.CalculateDiscountedPrice(originalPrice, validatedDiscountRate)
	}

	balance, err := db.GetUserBalance(ctx, userID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if balance < finalPrice {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrInsufficientBalance,
			mailInsufficientBalanceMessage(finalPrice, balance))
		return
	}

	expiresAt := mailNextExpiry(time.Now(), plan.Plan.BillingCycle)

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if locked, lockErr := db.TryAdvisoryTransactionLock(ctx, tx, db.MailSubscriptionLockNamespace, userID); lockErr != nil {
		mailRespondInternal(c)
		return
	} else if !locked {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "邮箱订阅正在处理，请稍后重试")
		return
	}
	if locked, lockErr := db.TryAdvisoryTransactionLock(ctx, tx, db.UserBalanceLockNamespace, userID); lockErr != nil {
		mailRespondInternal(c)
		return
	} else if !locked {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "用户余额正在处理，请稍后重试")
		return
	}

	existingInTx, err := db.MailFindExistingSubscriptionForPurchase(ctx, tx, userID, source.ID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	purchaseNow := time.Now()
	if existingInTx != nil && existingInTx.Status == models.MailSubscriptionStatusActive && existingInTx.ExpiresAt.After(purchaseNow) {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed,
			"您在该邮箱源已有有效订阅，请续费或等待过期后再购买")
		return
	}

	deducted, err := db.MailDeductUserBalanceTx(ctx, tx, userID, mailDecimalFromNumber(finalPrice))
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if !deducted {
		currentBalance, _, balanceErr := db.MailReadUserBalanceTx(ctx, tx, userID)
		if balanceErr != nil {
			mailRespondInternal(c)
			return
		}
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrInsufficientBalance,
			mailInsufficientBalanceMessage(finalPrice, mailDecimalFloat(currentBalance)))
		return
	}

	balanceAfter, _, err := db.MailReadUserBalanceTx(ctx, tx, userID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	balanceAfterFloat := mailDecimalFloat(balanceAfter)
	balanceBeforeFloat := lib.JSFixed(balanceAfterFloat+finalPrice, 2)

	remark := fmt.Sprintf("购买域名邮箱 - %s (%s)", plan.Plan.Name, source.Name)
	if validatedAffCodeID != nil {
		remark = fmt.Sprintf("购买域名邮箱 - %s (折扣 ¥%.2f)", plan.Plan.Name, discountAmount)
	}
	if err := db.MailInsertBalanceLogTx(ctx, tx, userID, models.BalanceLogTypeConsume,
		mailDecimalFromNumber(-finalPrice), mailDecimalFromNumber(balanceBeforeFloat),
		mailDecimalFromNumber(balanceAfterFloat), &remark); err != nil {
		mailRespondInternal(c)
		return
	}

	revived := existingInTx != nil &&
		(existingInTx.Status == models.MailSubscriptionStatusExpired || !existingInTx.ExpiresAt.After(purchaseNow))
	revivalPeriodStart := purchaseNow
	if existingInTx != nil && existingInTx.ExpiresAt.After(purchaseNow) {
		revivalPeriodStart = existingInTx.ExpiresAt
	}
	revivalExpiresAt := mailNextExpiry(revivalPeriodStart, plan.Plan.BillingCycle)

	var newSubscription models.MailSubscription
	var bindingInTx *models.MailSubscriptionAffBinding
	if revived {
		bindingInTx, err = db.MailFindAffBindingTx(ctx, tx, existingInTx.ID)
		if err != nil {
			mailRespondInternal(c)
			return
		}
		domainLimit := plan.Plan.DomainLimit
		diskLimitGB := plan.Plan.DiskLimitGB
		planIDCopy := plan.Plan.ID
		newSubscription, err = db.MailUpdateSubscriptionWithTx(ctx, tx, existingInTx.ID,
			models.MailSubscriptionStatusActive, revivalExpiresAt, &planIDCopy, &domainLimit, &diskLimitGB)
	} else {
		newSubscription, err = db.MailCreateSubscriptionWithTx(ctx, tx, db.MailCreateSubscriptionInput{
			UserID:      userID,
			SourceID:    source.ID,
			PlanID:      plan.Plan.ID,
			DomainLimit: plan.Plan.DomainLimit,
			DiskLimitGB: plan.Plan.DiskLimitGB,
			ExpiresAt:   expiresAt,
		})
	}
	if err != nil {
		mailRespondInternal(c)
		return
	}

	if validatedAffCodeID != nil {
		if bindingInTx == nil {
			if bindingErr := db.AffCreateMailBinding(ctx, newSubscription.ID, *validatedAffCodeID, tx); bindingErr != nil {
				mailRespondInternal(c)
				return
			}
		}
		logType := models.AffLogTypeNewPurchase
		if revived {
			logType = models.AffLogTypeRenew
		}
		if commissionErr := db.AffProcessMailCommission(ctx, *validatedAffCodeID, newSubscription.ID,
			originalPrice, logType, tx); commissionErr != nil {
			mailRespondInternal(c)
			return
		}
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		mailRespondInternal(c)
		return
	}

	if revived {
		if resumeErr := mailResumeSubscriptionDomains(ctx, newSubscription.ID, userID); resumeErr != nil {
			mailRespondInternal(c)
			return
		}
	}

	logContent := fmt.Sprintf("Purchased mail subscription: planId=%d, price=%v", planID, finalPrice)
	if validatedAffCodeID != nil {
		logContent += fmt.Sprintf(", affDiscount=%v", discountAmount)
	}
	mailLog(ctx, userID, "purchase_mail_subscription", logContent, "success")

	// Reload the relations for the response (source/plan are already in hand).
	c.JSON(http.StatusOK, gin.H{
		"subscription": gin.H{
			"id":        newSubscription.ID,
			"status":    newSubscription.Status,
			"expiresAt": mailISOTime(newSubscription.ExpiresAt),
			"source": gin.H{
				"id":   source.ID,
				"name": source.Name,
			},
			"plan": gin.H{
				"id":   plan.Plan.ID,
				"name": plan.Plan.Name,
			},
		},
		"discountApplied": discountAmount > 0,
		"discountAmount":  discountAmount,
		"finalPrice":      finalPrice,
	})
}

// existingSubscriptionID returns the id for the aff-binding lookup, or 0.
func existingSubscriptionID(subscription *models.MailSubscription) int32 {
	if subscription == nil {
		return 0
	}
	return subscription.ID
}

func mailUserRenewSubscription(c *gin.Context) {
	ctx := c.Request.Context()
	userID := mailUserID(c)

	rawBody, present, ok := mailReadRequestBody(c)
	if !ok {
		return
	}
	body, isObject := rawBody.(map[string]any)
	if !present || !isObject {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrValidationError, "请求参数无效")
		return
	}
	rawMonths, present := mailBodyField(body, "months")
	if !present {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrValidationError, "续费月数 无效")
		return
	}
	renewMonths, apiErr := mailNormalizeRenewMonths(rawMonths)
	if apiErr != nil {
		mailRespondAPIError(c, http.StatusBadRequest, apiErr.Code, apiErr.Details)
		return
	}

	subscription, err := db.MailGetUserSubscription(ctx, userID, nil)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if subscription == nil {
		mailRespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "您没有邮箱订阅")
		return
	}
	if subscription.Subscription.Status == models.MailSubscriptionStatusSuspended {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "订阅已被暂停，无法续费")
		return
	}

	plan := subscription.Plan
	if cycleErr := mailValidateRenewMonthsForBillingCycle(renewMonths, plan.BillingCycle); cycleErr != nil {
		mailRespondAPIError(c, http.StatusBadRequest, cycleErr.Code, cycleErr.Details)
		return
	}

	monthlyPrice := mailDecimalFloat(plan.Price)
	if plan.BillingCycle == models.MailBillingCycleYearly {
		monthlyPrice = mailDecimalFloat(plan.Price) / 12
	}
	originalPrice := mailJSRound(monthlyPrice*float64(renewMonths)*100) / 100

	binding, boundAffCode, err := mailSubscriptionAffCode(ctx, nil, subscription.Subscription.ID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	discountRate := 0.0
	discountAmount := 0.0
	finalPrice := originalPrice
	if binding != nil && boundAffCode != nil && boundAffCode.Enabled {
		discountRate = mailDecimalFloat(boundAffCode.DiscountRate)
		discountAmount = lib.CalculateDiscountAmount(originalPrice, discountRate)
		finalPrice = lib.CalculateDiscountedPrice(originalPrice, discountRate)
	}

	balance, err := db.GetUserBalance(ctx, userID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if balance < finalPrice {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrInsufficientBalance,
			mailInsufficientBalanceMessage(finalPrice, balance))
		return
	}

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if locked, lockErr := db.TryAdvisoryTransactionLock(ctx, tx, db.MailSubscriptionLockNamespace, userID); lockErr != nil {
		mailRespondInternal(c)
		return
	} else if !locked {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "邮箱订阅正在处理，请稍后重试")
		return
	}
	if locked, lockErr := db.TryAdvisoryTransactionLock(ctx, tx, db.UserBalanceLockNamespace, userID); lockErr != nil {
		mailRespondInternal(c)
		return
	} else if !locked {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "用户余额正在处理，请稍后重试")
		return
	}

	currentSubscription, err := db.MailFindSubscriptionByIDTx(ctx, tx, subscription.Subscription.ID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if currentSubscription == nil || currentSubscription.UserID != userID {
		mailRespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "您没有邮箱订阅")
		return
	}
	if currentSubscription.Status == models.MailSubscriptionStatusSuspended {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "订阅已被暂停，无法续费")
		return
	}
	currentPlan, err := db.MailGetPlanByID(ctx, currentSubscription.PlanID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if currentPlan != nil {
		if cycleErr := mailValidateRenewMonthsForBillingCycle(renewMonths, currentPlan.Plan.BillingCycle); cycleErr != nil {
			mailRespondAPIError(c, http.StatusBadRequest, cycleErr.Code, cycleErr.Details)
			return
		}
	}

	deducted, err := db.MailDeductUserBalanceTx(ctx, tx, userID, mailDecimalFromNumber(finalPrice))
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if !deducted {
		currentBalance, _, balanceErr := db.MailReadUserBalanceTx(ctx, tx, userID)
		if balanceErr != nil {
			mailRespondInternal(c)
			return
		}
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrInsufficientBalance,
			mailInsufficientBalanceMessage(finalPrice, mailDecimalFloat(currentBalance)))
		return
	}

	balanceAfter, _, err := db.MailReadUserBalanceTx(ctx, tx, userID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	balanceAfterFloat := mailDecimalFloat(balanceAfter)
	balanceBeforeFloat := lib.JSFixed(balanceAfterFloat+finalPrice, 2)

	remarkText := fmt.Sprintf("续费域名邮箱 %d 个月", renewMonths)
	if discountAmount > 0 {
		remarkText = fmt.Sprintf("续费域名邮箱 %d 个月（优惠码折扣 -¥%.2f）", renewMonths, discountAmount)
	}
	if err := db.MailInsertBalanceLogTx(ctx, tx, userID, models.BalanceLogTypeConsume,
		mailDecimalFromNumber(-finalPrice), mailDecimalFromNumber(balanceBeforeFloat),
		mailDecimalFromNumber(balanceAfterFloat), &remarkText); err != nil {
		mailRespondInternal(c)
		return
	}

	now := time.Now()
	periodStart := now
	if currentSubscription.ExpiresAt.After(now) {
		periodStart = currentSubscription.ExpiresAt
	}
	revived := currentSubscription.Status == models.MailSubscriptionStatusExpired || !currentSubscription.ExpiresAt.After(now)
	newExpiry := db.MailAddCalendarMonths(periodStart, int(renewMonths))

	updated, err := db.MailUpdateSubscriptionWithTx(ctx, tx, subscription.Subscription.ID,
		models.MailSubscriptionStatusActive, newExpiry, nil, nil, nil)
	if err != nil {
		mailRespondInternal(c)
		return
	}

	if binding != nil && boundAffCode != nil {
		if commissionErr := db.AffProcessMailCommission(ctx, boundAffCode.ID, subscription.Subscription.ID,
			originalPrice, models.AffLogTypeRenew, tx); commissionErr != nil {
			mailRespondInternal(c)
			return
		}
	}

	if commitErr := tx.Commit(ctx); commitErr != nil {
		mailRespondInternal(c)
		return
	}

	if revived {
		if resumeErr := mailResumeSubscriptionDomains(ctx, updated.ID, userID); resumeErr != nil {
			mailRespondInternal(c)
			return
		}
	}

	logContent := fmt.Sprintf("Renewed mail subscription for %d months, price=%v", renewMonths, finalPrice)
	if discountAmount > 0 {
		logContent += fmt.Sprintf(", affDiscount=%v", discountAmount)
	}
	mailLog(ctx, userID, "renew_mail_subscription", logContent, "success")

	c.JSON(http.StatusOK, gin.H{
		"expiresAt":       mailISOTime(updated.ExpiresAt),
		"discountApplied": discountAmount > 0,
		"discountAmount":  discountAmount,
		"finalPrice":      finalPrice,
	})
}

// ==================== user: domains ====================

type mailMyDomainDetailView struct {
	ID                      int32             `json:"id"`
	Domain                  string            `json:"domain"`
	Status                  string            `json:"status"`
	DiskUsedMB              int32             `json:"diskUsedMb"`
	VerifiedAt              *string           `json:"verifiedAt"`
	CreatedAt               string            `json:"createdAt"`
	AdminUsername           *string           `json:"adminUsername"`
	AdminPasswordConfigured bool              `json:"adminPasswordConfigured"`
	SourceCode              string            `json:"sourceCode"`
	Accounts                []mailAccountView `json:"accounts"`
}

type mailAccountView struct {
	ID          int32   `json:"id"`
	Email       string  `json:"email"`
	Username    string  `json:"username"`
	DisplayName *string `json:"displayName"`
	DiskLimitMB int32   `json:"diskLimitMb"`
	DiskUsedMB  int32   `json:"diskUsedMb"`
	IsAdmin     bool    `json:"isAdmin"`
	CreatedAt   string  `json:"createdAt"`
}

func mailNewAccountView(account models.MailAccount) mailAccountView {
	return mailAccountView{
		ID:          account.ID,
		Email:       account.Email,
		Username:    account.Username,
		DisplayName: account.DisplayName,
		DiskLimitMB: account.DiskLimitMB,
		DiskUsedMB:  account.DiskUsedMB,
		IsAdmin:     account.IsAdmin,
		CreatedAt:   mailISOTime(account.CreatedAt),
	}
}

func mailUserListDomains(c *gin.Context) {
	ctx := c.Request.Context()
	userID := mailUserID(c)

	subscription, err := db.MailGetUserSubscription(ctx, userID, nil)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if subscription == nil {
		c.JSON(http.StatusOK, gin.H{"domains": []any{}})
		return
	}

	domains, err := db.MailGetDomainsBySubscription(ctx, subscription.Subscription.ID)
	if err != nil {
		mailRespondInternal(c)
		return
	}

	views := make([]mailMyDomainView, 0, len(domains))
	for _, item := range domains {
		views = append(views, mailNewMyDomainView(item.Domain, len(item.Accounts)))
	}
	c.JSON(http.StatusOK, gin.H{"domains": views})
}

// mailLoadOwnedDomain mirrors the repeated
// `if (!domain || domain.subscription.userId !== request.user.id) 404` guard.
func mailLoadOwnedDomain(c *gin.Context, domainID int32) (*db.MailDomainDetail, bool) {
	ctx := c.Request.Context()
	domain, err := db.MailGetDomainByID(ctx, domainID)
	if err != nil {
		mailRespondInternal(c)
		return nil, false
	}
	if domain == nil || domain.Subscription.UserID != mailUserID(c) {
		mailRespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "域名不存在")
		return nil, false
	}
	return domain, true
}

func mailUserDomainDetail(c *gin.Context) {
	domainID, ok := mailRouteID(c, "id")
	if !ok {
		return
	}
	domain, ok := mailLoadOwnedDomain(c, domainID)
	if !ok {
		return
	}

	accounts := make([]mailAccountView, 0, len(domain.Accounts))
	for _, account := range domain.Accounts {
		accounts = append(accounts, mailNewAccountView(account))
	}

	c.JSON(http.StatusOK, gin.H{
		"domain": mailMyDomainDetailView{
			ID:                      domain.Domain.ID,
			Domain:                  domain.Domain.Domain,
			Status:                  string(domain.Domain.Status),
			DiskUsedMB:              domain.Domain.DiskUsedMB,
			VerifiedAt:              mailISOTimePtr(domain.Domain.VerifiedAt),
			CreatedAt:               mailISOTime(domain.Domain.CreatedAt),
			AdminUsername:           domain.Domain.AdminUsername,
			AdminPasswordConfigured: domain.Domain.AdminPassword != nil && *domain.Domain.AdminPassword != "",
			SourceCode:              domain.Source.Code,
			Accounts:                accounts,
		},
	})
}

func mailUserDomainAdminPassword(c *gin.Context) {
	domainID, ok := mailRouteID(c, "id")
	if !ok {
		return
	}
	domain, ok := mailLoadOwnedDomain(c, domainID)
	if !ok {
		return
	}
	var password any
	if domain.Domain.AdminPassword != nil {
		password = *domain.Domain.AdminPassword
	}
	c.JSON(http.StatusOK, gin.H{"adminPassword": password})
}

func mailUserCreateDomain(c *gin.Context) {
	ctx := c.Request.Context()
	userID := mailUserID(c)

	rawBody, bodyPresent, ok := mailReadRequestBody(c)
	if !ok {
		return
	}
	if !bodyPresent {
		mailRespondDestructureError(c, "domain")
		return
	}
	body := mailObjectFields(rawBody)

	rawDomain, present := mailBodyField(body, "domain")
	normalizedDomain := ""
	if present && rawDomain != nil {
		value, isString := rawDomain.(string)
		if !isString {
			// `domainName?.toLowerCase()` on a non-string throws a TypeError.
			mailRespondJSError(c, "domainName?.toLowerCase is not a function")
			return
		}
		normalizedDomain = strings.ToLower(value)
	}

	if normalizedDomain == "" || !mailDomainPattern.MatchString(normalizedDomain) {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrValidationError, "请输入有效的域名")
		return
	}

	subscription, err := db.MailGetUserSubscription(ctx, userID, nil)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if subscription == nil {
		mailRespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "您没有邮箱订阅")
		return
	}
	if !mailSubscriptionCanUseUpstream(subscription.Subscription) {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "订阅已过期或暂停")
		return
	}

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	statusCode := http.StatusOK
	var failure *lib.APIError

	fail := func(status int, apiErr lib.APIError) {
		statusCode = status
		failure = &apiErr
	}

	if locked, lockErr := db.TryAdvisoryTransactionLock(ctx, tx, db.MailSubscriptionLockNamespace, userID); lockErr != nil {
		mailRespondInternal(c)
		return
	} else if !locked {
		fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrOperationNotAllowed, "邮箱订阅正在处理，请稍后重试"))
	}
	if failure == nil {
		if locked, lockErr := db.TryAdvisoryTransactionLock(ctx, tx, db.MailDomainLockNamespace, subscription.Source.ID); lockErr != nil {
			mailRespondInternal(c)
			return
		} else if !locked {
			fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrOperationNotAllowed, "邮箱域名正在处理，请稍后重试"))
		}
	}

	if failure == nil {
		currentSubscription, err := db.MailFindSubscriptionByIDTx(ctx, tx, subscription.Subscription.ID)
		if err != nil {
			mailRespondInternal(c)
			return
		}
		if currentSubscription == nil || currentSubscription.UserID != userID {
			fail(http.StatusNotFound, lib.APIErrorFor(lib.ErrNotFound, "您没有邮箱订阅"))
		} else if !mailSubscriptionCanUseUpstream(*currentSubscription) {
			fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrOperationNotAllowed, "订阅已过期或暂停"))
		} else {
			domainCount, countErr := db.MailCountDomainsForSubscription(ctx, tx, currentSubscription.ID)
			if countErr != nil {
				mailRespondInternal(c)
				return
			}
			if domainCount >= int64(currentSubscription.DomainLimit) {
				fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrQuotaExceeded, "已达域名数量上限"))
			} else {
				exists, existsErr := db.MailFindDomainByDomainAndSource(ctx, tx, normalizedDomain, currentSubscription.SourceID)
				if existsErr != nil {
					mailRespondInternal(c)
					return
				}
				if exists != nil {
					fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrResourceExists, "该域名已被使用"))
				} else {
					domainDiskLimitMB := int64(math.Floor(
						float64(currentSubscription.DiskLimitGB) * 1024 / float64(currentSubscription.DomainLimit)))
					adminDiskLimitMB := domainDiskLimitMB
					if adminDiskLimitMB > mailDefaultAccountDiskMB {
						adminDiskLimitMB = mailDefaultAccountDiskMB
					}
					hasQuota, quotaErr := db.MailSubscriptionHasDiskQuota(ctx, tx, currentSubscription.ID,
						int64(currentSubscription.DiskLimitGB), adminDiskLimitMB, nil)
					if quotaErr != nil {
						mailRespondInternal(c)
						return
					}
					if !hasQuota {
						fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrQuotaExceeded, "订阅共享磁盘配额不足"))
					} else {
						craneResult, craneErr := mailCraneCreateDomain(ctx, subscription.Source, normalizedDomain,
							float64(currentSubscription.DiskLimitGB), int(currentSubscription.DomainLimit))
						if craneErr != nil {
							fail(http.StatusBadGateway, lib.APIErrorFor(lib.ErrUpstreamError,
								"创建域名失败: "+craneErr.Error()))
						} else {
							mailDomain, createErr := db.MailCreateDomainWithTx(ctx, tx, db.MailCreateDomainInput{
								SubscriptionID: currentSubscription.ID,
								SourceID:       currentSubscription.SourceID,
								Domain:         normalizedDomain,
								AdminUsername:  craneResult.Username,
								AdminPassword:  craneResult.Password,
							})
							if createErr != nil {
								mailRespondInternal(c)
								return
							}

							if craneResult.Username != nil && craneResult.Password != nil {
								usernameWithoutDomain := strings.Split(*craneResult.Username, "@")[0]
								if usernameWithoutDomain == "" {
									usernameWithoutDomain = "postmaster"
								}
								displayName := "Administrator"
								diskLimit := int32(adminDiskLimitMB)
								isAdmin := true
								if _, accountErr := db.MailCreateAccountWithTx(ctx, tx, db.MailCreateAccountInput{
									DomainID:    mailDomain.ID,
									Email:       *craneResult.Username,
									Username:    usernameWithoutDomain,
									DisplayName: &displayName,
									DiskLimitMB: &diskLimit,
									IsAdmin:     &isAdmin,
								}); accountErr != nil {
									mailRespondInternal(c)
									return
								}
							}

							if commitErr := tx.Commit(ctx); commitErr != nil {
								mailRespondInternal(c)
								return
							}

							mailLog(ctx, userID, "add_mail_domain", "Added mail domain: "+normalizedDomain, "success")
							c.JSON(http.StatusOK, gin.H{
								"domain": gin.H{
									"id":        mailDomain.ID,
									"domain":    mailDomain.Domain,
									"status":    mailDomain.Status,
									"createdAt": mailISOTime(mailDomain.CreatedAt),
								},
							})
							return
						}
					}
				}
			}
		}
	}

	if failure != nil {
		mailRespondAPIError(c, statusCode, failure.Code, failure.Details)
	}
}

func mailUserVerifyDomain(c *gin.Context) {
	ctx := c.Request.Context()
	domainID, ok := mailRouteID(c, "id")
	if !ok {
		return
	}
	domain, ok := mailLoadOwnedDomain(c, domainID)
	if !ok {
		return
	}
	if !mailSubscriptionCanUseUpstream(domain.Subscription) {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "订阅已过期或暂停，无法操作上游")
		return
	}

	info, err := mailCraneGetDomainInfo(ctx, domain.Source, domain.Domain.Domain)
	if err != nil {
		mailRespondAPIError(c, http.StatusBadGateway, lib.ErrUpstreamError,
			"检查验证状态失败: "+err.Error())
		return
	}

	if info.Verified {
		verifiedAt := time.Now()
		status := models.MailDomainStatusVerified
		if _, updateErr := db.MailUpdateDomain(ctx, domainID, db.MailUpdateDomainInput{
			Status:     &status,
			VerifiedAt: &verifiedAt,
		}); updateErr != nil {
			mailRespondInternal(c)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "verified", "verified": true})
		return
	}

	// Verification failed: nothing is written. The row stays pending (or
	// suspended) — a failed DNS check must never be persisted as verified.
	// `txtRecord` is optional in the TypeScript, so an absent value means the key
	// is omitted from the JSON (not null).
	response := gin.H{"status": "pending", "verified": false}
	if info.TxtRecord != nil {
		response["txtRecord"] = *info.TxtRecord
	}
	c.JSON(http.StatusOK, response)
}

func mailUserDomainDNS(c *gin.Context) {
	ctx := c.Request.Context()
	domainID, ok := mailRouteID(c, "id")
	if !ok {
		return
	}
	domain, ok := mailLoadOwnedDomain(c, domainID)
	if !ok {
		return
	}
	if !mailSubscriptionCanUseUpstream(domain.Subscription) {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "订阅已过期或暂停，无法操作上游")
		return
	}

	info, err := mailCraneGetDomainInfo(ctx, domain.Source, domain.Domain.Domain)
	if err != nil {
		mailRespondAPIError(c, http.StatusBadGateway, lib.ErrUpstreamError,
			"获取 DNS 配置失败: "+err.Error())
		return
	}

	response := gin.H{
		"verified":     info.Verified,
		"dnsRecords":   info.DNSRecords,
		"mxRecords":    info.MXRecords,
		"cnameRecords": info.CNAMERecords,
	}
	if info.TxtRecord != nil {
		response["txtRecord"] = *info.TxtRecord
	}
	if info.SPFRecord != nil {
		response["spfRecord"] = *info.SPFRecord
	}
	if info.DKIMRecord != nil {
		response["dkimRecord"] = *info.DKIMRecord
	}
	c.JSON(http.StatusOK, response)
}

func mailUserDeleteDomain(c *gin.Context) {
	ctx := c.Request.Context()
	domainID, ok := mailRouteID(c, "id")
	if !ok {
		return
	}
	domain, ok := mailLoadOwnedDomain(c, domainID)
	if !ok {
		return
	}
	if !mailSubscriptionCanUseUpstream(domain.Subscription) {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "订阅已过期或暂停，无法操作上游")
		return
	}

	if err := mailCraneDeleteDomain(ctx, domain.Source, domain.Domain.Domain); err != nil {
		fmt.Fprintf(os.Stderr, "CraneMail delete domain error: %v\n", err)
		mailRespondAPIError(c, http.StatusBadGateway, lib.ErrUpstreamError,
			"删除远端域名失败: "+err.Error())
		return
	}

	if err := db.MailDeleteDomain(ctx, domainID); err != nil {
		mailRespondInternal(c)
		return
	}

	mailLog(ctx, mailUserID(c), "delete_mail_domain", "Deleted mail domain: "+domain.Domain.Domain, "success")
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ==================== user: mail accounts ====================

func mailUserListAccounts(c *gin.Context) {
	ctx := c.Request.Context()
	domainID, ok := mailRouteID(c, "id")
	if !ok {
		return
	}
	if _, ok := mailLoadOwnedDomain(c, domainID); !ok {
		return
	}

	accounts, err := db.MailGetAccountsByDomain(ctx, domainID)
	if err != nil {
		mailRespondInternal(c)
		return
	}

	views := make([]mailAccountView, 0, len(accounts))
	for _, account := range accounts {
		views = append(views, mailNewAccountView(account))
	}
	c.JSON(http.StatusOK, gin.H{"accounts": views})
}

func mailUserCreateAccount(c *gin.Context) {
	ctx := c.Request.Context()
	userID := mailUserID(c)
	domainID, ok := mailRouteID(c, "id")
	if !ok {
		return
	}

	rawBody, bodyPresent, ok := mailReadRequestBody(c)
	if !ok {
		return
	}
	if !bodyPresent {
		mailRespondDestructureError(c, "username")
		return
	}
	body := mailObjectFields(rawBody)

	domain, ok := mailLoadOwnedDomain(c, domainID)
	if !ok {
		return
	}
	if !mailSubscriptionCanUseUpstream(domain.Subscription) {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "订阅已过期或暂停，无法操作上游")
		return
	}
	if domain.Domain.Status != models.MailDomainStatusVerified {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "请先完成域名验证")
		return
	}

	maxDiskLimitMB := int64(domain.Subscription.DiskLimitGB) * 1024
	accountInput, apiErr := mailNormalizeAccountInput(body, true, true, maxDiskLimitMB)
	if apiErr != nil {
		mailRespondUncaughtAPIError(c)
		return
	}
	accountDiskLimitMB := int64(mailDefaultAccountDiskMB)
	if accountDiskLimitMB > maxDiskLimitMB {
		accountDiskLimitMB = maxDiskLimitMB
	}
	if accountInput.DiskLimitMB != nil {
		accountDiskLimitMB = int64(*accountInput.DiskLimitMB)
	}

	email := *accountInput.Username + "@" + domain.Domain.Domain

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	statusCode := http.StatusOK
	var failure *lib.APIError
	fail := func(status int, apiErr lib.APIError) {
		statusCode = status
		failure = &apiErr
	}

	if locked, lockErr := db.TryAdvisoryTransactionLock(ctx, tx, db.MailSubscriptionLockNamespace, userID); lockErr != nil {
		mailRespondInternal(c)
		return
	} else if !locked {
		fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrOperationNotAllowed, "邮箱订阅正在处理，请稍后重试"))
	}

	if failure == nil {
		currentDomain, fetchErr := db.MailFindDomainByIDTx(ctx, tx, domainID)
		if fetchErr != nil {
			mailRespondInternal(c)
			return
		}
		if currentDomain == nil {
			fail(http.StatusNotFound, lib.APIErrorFor(lib.ErrNotFound, "域名不存在"))
		} else {
			currentSubscription, subErr := db.MailFindSubscriptionByIDTx(ctx, tx, currentDomain.SubscriptionID)
			if subErr != nil {
				mailRespondInternal(c)
				return
			}
			if currentSubscription == nil || currentSubscription.UserID != userID {
				fail(http.StatusNotFound, lib.APIErrorFor(lib.ErrNotFound, "域名不存在"))
			} else if !mailSubscriptionCanUseUpstream(*currentSubscription) {
				fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrOperationNotAllowed, "订阅已过期或暂停，无法操作上游"))
			} else if currentDomain.Status != models.MailDomainStatusVerified {
				fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrOperationNotAllowed, "请先完成域名验证"))
			} else {
				exists, existsErr := db.MailCheckAccountExistsTx(ctx, tx, domainID, *accountInput.Username)
				if existsErr != nil {
					mailRespondInternal(c)
					return
				}
				if exists {
					fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrResourceExists, "该邮箱账户已存在"))
				} else {
					hasQuota, quotaErr := db.MailSubscriptionHasDiskQuota(ctx, tx, currentSubscription.ID,
						int64(currentSubscription.DiskLimitGB), accountDiskLimitMB, nil)
					if quotaErr != nil {
						mailRespondInternal(c)
						return
					}
					if !hasQuota {
						fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrQuotaExceeded, "订阅共享磁盘配额不足"))
					} else {
						displayName := *accountInput.Username
						if accountInput.DisplayName != nil {
							displayName = *accountInput.DisplayName
						}
						diskLimit := int32(accountDiskLimitMB)
						isAdmin := false
						provision := mailProvisionMailAccount(
							func() error {
								return mailSmarterCreateAccount(ctx, domain.Source, domain.Domain.Domain,
									mailAdminUsername(domain.Domain.AdminUsername),
									mailAdminPassword(domain.Domain.AdminPassword),
									services.SmarterMailAccountInput{
										Username:    *accountInput.Username,
										Password:    *accountInput.Password,
										DisplayName: displayName,
										DiskLimitMB: float64(accountDiskLimitMB),
									})
							},
							func() error {
								return mailSmarterDeleteAccount(ctx, domain.Source, domain.Domain.Domain,
									mailAdminUsername(domain.Domain.AdminUsername),
									mailAdminPassword(domain.Domain.AdminPassword),
									*accountInput.Username)
							},
							func() (models.MailAccount, error) {
								return db.MailCreateAccountWithTx(ctx, tx, db.MailCreateAccountInput{
									DomainID:    domainID,
									Email:       email,
									Username:    *accountInput.Username,
									DisplayName: accountInput.DisplayName,
									DiskLimitMB: &diskLimit,
									IsAdmin:     &isAdmin,
								})
							},
						)
						if provision.Error != nil {
							fail(provision.Status, *provision.Error)
						} else {
							if commitErr := tx.Commit(ctx); commitErr != nil {
								mailRespondInternal(c)
								return
							}
							account := *provision.Account
							mailLog(ctx, userID, "create_mail_account", "Created mail account: "+email, "success")
							c.JSON(http.StatusOK, gin.H{
								"account": gin.H{
									"id":          account.ID,
									"email":       account.Email,
									"username":    account.Username,
									"displayName": account.DisplayName,
									"diskLimitMb": account.DiskLimitMB,
									"isAdmin":     account.IsAdmin,
								},
							})
							return
						}
					}
				}
			}
		}
	}

	if failure != nil {
		mailRespondAPIError(c, statusCode, failure.Code, failure.Details)
	}
}

// mailAccountProvisionResult is the outcome of the upstream+local account
// provisioning pair.
type mailAccountProvisionResult struct {
	Account *models.MailAccount
	Status  int
	Error   *lib.APIError
}

// mailProvisionMailAccount mirrors the create-account compensation contract:
//
//   - the upstream mailbox is created first; when that fails nothing local is
//     written (so the subscription's disk allocation never grows — quota is the
//     sum of the local rows);
//   - when the local row cannot be written after the upstream mailbox exists, the
//     upstream mailbox is deleted again; if that compensation also fails the
//     caller gets a 502 instead of a silent orphan.
//
// It is deliberately free of DB/HTTP dependencies so the matrix is unit-tested.
func mailProvisionMailAccount(
	createUpstream func() error,
	deleteUpstream func() error,
	createLocal func() (models.MailAccount, error),
) mailAccountProvisionResult {
	if err := createUpstream(); err != nil {
		apiErr := lib.APIErrorFor(lib.ErrUpstreamError, "创建邮箱账户失败: "+err.Error())
		return mailAccountProvisionResult{Status: http.StatusBadGateway, Error: &apiErr}
	}

	account, localErr := createLocal()
	if localErr != nil {
		if compensationErr := deleteUpstream(); compensationErr != nil {
			fmt.Fprintf(os.Stderr, "SmarterMail account create compensation failed: %v\n", compensationErr)
			apiErr := lib.APIErrorFor(lib.ErrUpstreamError,
				"邮箱账户已在远端创建，但本地记录保存失败且远端补偿删除失败，请联系管理员处理")
			return mailAccountProvisionResult{Status: http.StatusBadGateway, Error: &apiErr}
		}
		fmt.Fprintf(os.Stderr, "Mail account local create failed after upstream create: %v\n", localErr)
		apiErr := lib.APIErrorFor(lib.ErrInternalError, "邮箱账户创建失败，已回滚远端账号，请重试")
		return mailAccountProvisionResult{Status: http.StatusInternalServerError, Error: &apiErr}
	}

	return mailAccountProvisionResult{Account: &account, Status: http.StatusOK}
}

// mailAdminUsername mirrors `domain.adminUsername!`: a NULL column is interpolated
// as "null" by the TypeScript template string.
func mailAdminUsername(value *string) string {
	if value == nil {
		return "null"
	}
	return *value
}

// mailAdminPassword mirrors `domain.adminPassword!` for the same reason.
func mailAdminPassword(value *string) string {
	if value == nil {
		return "null"
	}
	return *value
}

func mailUserUpdateAccount(c *gin.Context) {
	ctx := c.Request.Context()
	userID := mailUserID(c)
	domainID, ok := mailRouteID(c, "id")
	if !ok {
		return
	}
	accountID, ok := mailRouteID(c, "accountId")
	if !ok {
		return
	}

	rawBody, bodyPresent, ok := mailReadRequestBody(c)
	if !ok {
		return
	}
	if !bodyPresent {
		mailRespondDestructureError(c, "displayName")
		return
	}
	body := mailObjectFields(rawBody)

	domain, ok := mailLoadOwnedDomain(c, domainID)
	if !ok {
		return
	}
	if !mailSubscriptionCanUseUpstream(domain.Subscription) {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "订阅已过期或暂停，无法操作上游")
		return
	}

	account, err := db.MailGetAccountByID(ctx, accountID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if account == nil || account.DomainID != domainID {
		mailRespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "邮箱账户不存在")
		return
	}

	accountInput, apiErr := mailNormalizeAccountInput(body, false, false, int64(domain.Subscription.DiskLimitGB)*1024)
	if apiErr != nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	statusCode := http.StatusOK
	var failure *lib.APIError
	fail := func(status int, apiErr lib.APIError) {
		statusCode = status
		failure = &apiErr
	}

	if locked, lockErr := db.TryAdvisoryTransactionLock(ctx, tx, db.MailSubscriptionLockNamespace, userID); lockErr != nil {
		mailRespondInternal(c)
		return
	} else if !locked {
		fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrOperationNotAllowed, "邮箱订阅正在处理，请稍后重试"))
	}

	if failure == nil {
		currentDomain, fetchErr := db.MailFindDomainByIDTx(ctx, tx, domainID)
		if fetchErr != nil {
			mailRespondInternal(c)
			return
		}
		if currentDomain == nil {
			fail(http.StatusNotFound, lib.APIErrorFor(lib.ErrNotFound, "域名不存在"))
		} else {
			currentSubscription, subErr := db.MailFindSubscriptionByIDTx(ctx, tx, currentDomain.SubscriptionID)
			if subErr != nil {
				mailRespondInternal(c)
				return
			}
			if currentSubscription == nil || currentSubscription.UserID != userID {
				fail(http.StatusNotFound, lib.APIErrorFor(lib.ErrNotFound, "域名不存在"))
			} else if !mailSubscriptionCanUseUpstream(*currentSubscription) {
				fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrOperationNotAllowed, "订阅已过期或暂停，无法操作上游"))
			} else {
				currentAccount, accountErr := db.MailFindAccountByIDTx(ctx, tx, accountID)
				if accountErr != nil {
					mailRespondInternal(c)
					return
				}
				if currentAccount == nil || currentAccount.DomainID != domainID {
					fail(http.StatusNotFound, lib.APIErrorFor(lib.ErrNotFound, "邮箱账户不存在"))
				} else {
					quotaOK := true
					if accountInput.DiskLimitMB != nil {
						hasQuota, quotaErr := db.MailSubscriptionHasDiskQuota(ctx, tx, currentDomain.SubscriptionID,
							int64(currentSubscription.DiskLimitGB), int64(*accountInput.DiskLimitMB), &currentAccount.ID)
						if quotaErr != nil {
							mailRespondInternal(c)
							return
						}
						quotaOK = hasQuota
					}
					if !quotaOK {
						fail(http.StatusBadRequest, lib.APIErrorFor(lib.ErrQuotaExceeded, "订阅共享磁盘配额不足"))
					} else {
						var diskLimitFloat *float64
						if accountInput.DiskLimitMB != nil {
							value := float64(*accountInput.DiskLimitMB)
							diskLimitFloat = &value
						}
						if updateErr := mailSmarterUpdateAccount(ctx, domain.Source, domain.Domain.Domain,
							mailAdminUsername(domain.Domain.AdminUsername),
							mailAdminPassword(domain.Domain.AdminPassword),
							currentAccount.Username,
							services.SmarterMailAccountUpdates{
								DisplayName: accountInput.DisplayName,
								DiskLimitMB: diskLimitFloat,
							}); updateErr != nil {
							fail(http.StatusBadGateway, lib.APIErrorFor(lib.ErrUpstreamError,
								"更新邮箱账户失败: "+updateErr.Error()))
						} else {
							updated, updateErr := db.MailUpdateAccountWithTx(ctx, tx, accountID, db.MailUpdateAccountInput{
								DisplayName: accountInput.DisplayName,
								DiskLimitMB: accountInput.DiskLimitMB,
							})
							if updateErr != nil {
								mailRespondInternal(c)
								return
							}
							if commitErr := tx.Commit(ctx); commitErr != nil {
								mailRespondInternal(c)
								return
							}
							c.JSON(http.StatusOK, gin.H{
								"account": gin.H{
									"id":          updated.ID,
									"email":       updated.Email,
									"displayName": updated.DisplayName,
									"diskLimitMb": updated.DiskLimitMB,
								},
							})
							return
						}
					}
				}
			}
		}
	}

	if failure != nil {
		mailRespondAPIError(c, statusCode, failure.Code, failure.Details)
	}
}

func mailUserResetAccountPassword(c *gin.Context) {
	ctx := c.Request.Context()
	domainID, ok := mailRouteID(c, "id")
	if !ok {
		return
	}
	accountID, ok := mailRouteID(c, "accountId")
	if !ok {
		return
	}

	rawBody, bodyPresent, ok := mailReadRequestBody(c)
	if !ok {
		return
	}
	if !bodyPresent {
		mailRespondDestructureError(c, "password")
		return
	}
	body := mailObjectFields(rawBody)

	domain, ok := mailLoadOwnedDomain(c, domainID)
	if !ok {
		return
	}
	if !mailSubscriptionCanUseUpstream(domain.Subscription) {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "订阅已过期或暂停，无法操作上游")
		return
	}

	account, err := db.MailGetAccountByID(ctx, accountID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if account == nil || account.DomainID != domainID {
		mailRespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "邮箱账户不存在")
		return
	}

	passwordInput, apiErr := mailNormalizeAccountInput(body, false, true, int64(domain.Subscription.DiskLimitGB)*1024)
	if apiErr != nil {
		mailRespondUncaughtAPIError(c)
		return
	}

	if err := mailSmarterResetPassword(ctx, domain.Source, domain.Domain.Domain,
		mailAdminUsername(domain.Domain.AdminUsername),
		mailAdminPassword(domain.Domain.AdminPassword),
		account.Username, *passwordInput.Password); err != nil {
		mailRespondAPIError(c, http.StatusBadGateway, lib.ErrUpstreamError, "重置密码失败: "+err.Error())
		return
	}

	mailLog(ctx, mailUserID(c), "reset_mail_account_password",
		"Reset password for mail account: "+account.Email, "success")
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func mailUserDeleteAccount(c *gin.Context) {
	ctx := c.Request.Context()
	domainID, ok := mailRouteID(c, "id")
	if !ok {
		return
	}
	accountID, ok := mailRouteID(c, "accountId")
	if !ok {
		return
	}

	domain, ok := mailLoadOwnedDomain(c, domainID)
	if !ok {
		return
	}
	if !mailSubscriptionCanUseUpstream(domain.Subscription) {
		mailRespondAPIError(c, http.StatusBadRequest, lib.ErrOperationNotAllowed, "订阅已过期或暂停，无法操作上游")
		return
	}

	account, err := db.MailGetAccountByID(ctx, accountID)
	if err != nil {
		mailRespondInternal(c)
		return
	}
	if account == nil || account.DomainID != domainID {
		mailRespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "邮箱账户不存在")
		return
	}

	if err := mailSmarterDeleteAccount(ctx, domain.Source, domain.Domain.Domain,
		mailAdminUsername(domain.Domain.AdminUsername),
		mailAdminPassword(domain.Domain.AdminPassword),
		account.Username); err != nil {
		fmt.Fprintf(os.Stderr, "SmarterMail delete account error: %v\n", err)
		mailRespondAPIError(c, http.StatusBadGateway, lib.ErrUpstreamError, "删除远端邮箱账户失败: "+err.Error())
		return
	}

	if err := db.MailDeleteAccount(ctx, accountID); err != nil {
		mailRespondInternal(c)
		return
	}

	mailLog(ctx, mailUserID(c), "delete_mail_account", "Deleted mail account: "+account.Email, "success")
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ==================== registration ====================

// RegisterMail mounts the mail module under /api/mail (app.ts prefix).
func RegisterMail(r *gin.Engine) {
	group := r.Group("/api/mail")

	// Admin: mail sources
	group.GET("/admin/sources", middleware.AuthenticateAdmin(), mailAdminListSources)
	group.POST("/admin/sources", middleware.AuthenticateAdmin(), mailAdminCreateSource)
	group.PUT("/admin/sources/:id", middleware.AuthenticateAdmin(), mailAdminUpdateSource)
	group.DELETE("/admin/sources/:id", middleware.AuthenticateAdmin(), mailAdminDeleteSource)

	// Admin: mail plans
	group.GET("/admin/plans", middleware.AuthenticateAdmin(), mailAdminListPlans)
	group.POST("/admin/plans", middleware.AuthenticateAdmin(), mailAdminCreatePlan)
	group.PUT("/admin/plans/:id", middleware.AuthenticateAdmin(), mailAdminUpdatePlan)
	group.DELETE("/admin/plans/:id", middleware.AuthenticateAdmin(), mailAdminDeletePlan)

	// Admin: mail subscriptions and domains
	group.GET("/admin/subscriptions", middleware.AuthenticateAdmin(), mailAdminListSubscriptions)
	group.POST("/admin/subscriptions/:id/cancel", middleware.AuthenticateAdmin(), mailAdminCancelSubscription)
	group.GET("/admin/domains", middleware.AuthenticateAdmin(), mailAdminListDomains)

	// User: offerings and aff codes
	group.GET("/sources", middleware.Authenticate(), mailUserSources)
	group.POST("/validate-aff", middleware.Authenticate(), mailUserValidateAff)

	// User: subscription
	group.GET("/subscription", middleware.Authenticate(), mailUserGetSubscription)
	group.PATCH("/subscription/auto-renew", middleware.Authenticate(), mailUserSetAutoRenew)
	group.POST("/subscription", middleware.Authenticate(), mailUserPurchaseSubscription)
	group.POST("/subscription/renew", middleware.Authenticate(), mailUserRenewSubscription)

	// User: domains
	group.GET("/domains", middleware.Authenticate(), mailUserListDomains)
	group.POST("/domains", middleware.Authenticate(), mailUserCreateDomain)
	group.GET("/domains/:id", middleware.Authenticate(), mailUserDomainDetail)
	group.GET("/domains/:id/admin-password", middleware.Authenticate(), mailUserDomainAdminPassword)
	group.POST("/domains/:id/verify", middleware.Authenticate(), mailUserVerifyDomain)
	group.GET("/domains/:id/dns", middleware.Authenticate(), mailUserDomainDNS)
	group.DELETE("/domains/:id", middleware.Authenticate(), mailUserDeleteDomain)

	// User: mail accounts
	group.GET("/domains/:id/accounts", middleware.Authenticate(), mailUserListAccounts)
	group.POST("/domains/:id/accounts", middleware.Authenticate(), mailUserCreateAccount)
	group.PUT("/domains/:id/accounts/:accountId", middleware.Authenticate(), mailUserUpdateAccount)
	group.POST("/domains/:id/accounts/:accountId/reset-password", middleware.Authenticate(), mailUserResetAccountPassword)
	group.DELETE("/domains/:id/accounts/:accountId", middleware.Authenticate(), mailUserDeleteAccount)
}

func init() {
	Register(RegisterMail)
}
