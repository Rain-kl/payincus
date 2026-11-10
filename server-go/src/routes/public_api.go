// Port of server/src/routes/public-api.ts (2,387 lines).
//
// This is the third and last authentication surface of the backend. The module is
// mounted by app.ts at `{ prefix: '/api/v1' }` (app.ts:567), so every path below is
// under /api/v1, NOT /api/public. The three surfaces must never fall back to each
// other (WAVE_D_SPEC §0.2):
//
//   - Web JWT  : /api/*                 (middleware.Authenticate)
//   - Public API: /api/v1/*             (lib.AuthenticatePublicAPIRequest, pat_/poa_)
//   - Agent HMAC: /api/agent/*          (wave E)
//
// A Web JWT therefore answers 401 PUBLIC_API_TOKEN_REQUIRED here, and a pat_ token
// answers 401 UNAUTHORIZED on every /api/* route — both directions are pinned by
// TestPublicAPIWebTokenCannotUsePublicAPI and
// TestPublicAPITokenCannotUseWebJWT.
//
// Fastify pipeline order matters and is reproduced explicitly:
//
//	onRequest rate limit  → body parser → handler (authenticate → validate → DB)
//
// The module authenticates inside the handler, so an empty/malformed JSON body on
// a POST/PATCH/DELETE wins over the 401 (ValidateFastifyBody runs first). GET/HEAD
// are bodyless in Fastify and never parse a body, so GET handlers go straight to
// authentication.
//
// Scopes are enforced by lib.AuthenticatePublicAPIRequest(requiredScope); a token
// missing the scope answers 403 PUBLIC_API_SCOPE_REQUIRED. The scope of every
// route is listed next to its registration at the bottom of this file.
//
// Inline rate limits: the TypeScript declares nine `config: { rateLimit: ... }`
// objects (PUBLIC_API_RATE_LIMITS). route-specific configs are enforced by
// @fastify/rate-limit in a *child store* per route, keyed by request.ip
// (config/rate-limit.ts has no /api/v1 rules, so the global keyGenerator returns
// request.ip). publicAPIRateLimit below reproduces exactly that: one fixed window
// per (route, IP), separate from the global 300/min middleware bucket.
//
// Time: every time.Time written to or compared in SQL is .UTC() (WAVE_D_SPEC
// §0.2b). Money: models.Decimal on the way in, float64 via lib.JSFixed / decimal
// conversion only at the JSON boundary (WAVE_D_SPEC §0.3).
//
// Dependencies that are not ported yet are package-level seams (see the
// publicAPI*Service* vars): they fail loudly instead of silently succeeding.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== constants ====================

const (
	publicAPIMaxPageSize = 100

	publicAPIMinTicketSubjectLength       = 2
	publicAPIMaxTicketSubjectLength       = 200
	publicAPIMinTicketCreateContentLength = 10
	publicAPIMaxTicketReplyLength         = 5000
	publicAPIMaxNotificationTitleLength   = 120
	publicAPIMaxNotificationMessageLength = 2000
	publicAPIMaxNotificationSourceLength  = 80
	publicAPIMaxNotificationTemplateVars  = 10
	publicAPIMaxNotificationVariableLen   = 120
	publicAPIMinAdjustmentReasonLength    = 10
	publicAPIMaxAdjustmentReasonLength    = 500
	publicAPIMaxAdjustmentAmount          = 10000

	// publicAPIMaxSafeInteger mirrors Number.MAX_SAFE_INTEGER; parsePositiveId
	// rejects larger strings (Number(...) would no longer be a safe integer).
	publicAPIMaxSafeInteger = int64(9007199254740991)

	// publicAPIPendingAdjustmentLimit is the per-user pending cap, checked before
	// the row is created (TS: `if (pendingCount >= 5) → 429`).
	publicAPIPendingAdjustmentLimit = 5

	// publicAPIOrderSourceTakeCap mirrors `Math.min(skip + take, 200)`.
	publicAPIOrderSourceTakeCap = 200
)

// publicAPIPositiveIDPattern mirrors POSITIVE_ID_PATTERN.
var publicAPIPositiveIDPattern = regexp.MustCompile(`^[1-9]\d*$`)

// publicAPIAdjustmentOrderNoPattern mirrors PUBLIC_BALANCE_ADJUSTMENT_ORDER_NO_PATTERN.
var publicAPIAdjustmentOrderNoPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,80}$`)

// publicAPINotificationVariableKeyPattern mirrors /^[A-Za-z][A-Za-z0-9_]{0,39}$/.
var publicAPINotificationVariableKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,39}$`)

// publicAPIJSWhitespacePattern mirrors the JavaScript `\s` class (which is what
// `.trim()` and `replace(/\s+/g, ' ')` use). Go's unicode.IsSpace differs in both
// directions (it has U+0085, JavaScript has U+FEFF), so the set is spelled out.
var publicAPIJSWhitespacePattern = regexp.MustCompile(`[\t\n\v\f\r \x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}]+`)

// ==================== module error codes ====================
//
// These are literal `{ error, code }` pairs in the TypeScript; they are not part of
// the shared lib/errors.ts catalog, so they are declared next to their use.
const (
	publicAPIInvalidProfileUpdateCode = "INVALID_PUBLIC_PROFILE_UPDATE"
	publicAPIUserNotFoundCode         = "PUBLIC_API_USER_NOT_FOUND"
	publicAPIInvalidSortCode          = "INVALID_PUBLIC_API_SORT"

	publicAPIInvalidAdjustmentStatus    = "INVALID_BALANCE_ADJUSTMENT_STATUS"
	publicAPIInvalidAdjustmentAmount    = "INVALID_BALANCE_ADJUSTMENT_AMOUNT"
	publicAPIInvalidAdjustmentReason    = "INVALID_BALANCE_ADJUSTMENT_REASON"
	publicAPIInvalidAdjustmentOrderNo   = "INVALID_BALANCE_ADJUSTMENT_ORDER_NO"
	publicAPIPendingAdjustmentLimitCode = "PUBLIC_BALANCE_ADJUSTMENT_PENDING_LIMIT"

	publicAPIInvalidProductID   = "INVALID_PRODUCT_ID"
	publicAPIProductNotFound    = "PRODUCT_NOT_FOUND"
	publicAPIInvalidOrderStatus = "INVALID_ORDER_STATUS"
	publicAPIInvalidOrderID     = "INVALID_ORDER_ID"
	publicAPIOrderNotFound      = "ORDER_NOT_FOUND"

	publicAPIInvalidBillingType = "INVALID_BILLING_RECORD_TYPE"
	publicAPIInvalidServiceID   = "INVALID_SERVICE_ID"
	publicAPIInvalidBillingID   = "INVALID_BILLING_RECORD_ID"
	publicAPIBillingNotFound    = "BILLING_RECORD_NOT_FOUND"

	publicAPIInvalidServiceStatus  = "INVALID_SERVICE_STATUS"
	publicAPIInvalidServiceInclude = "INVALID_SERVICE_INCLUDE"
	publicAPIServiceNotFound       = "SERVICE_NOT_FOUND"
	publicAPIInvalidServiceAction  = "INVALID_SERVICE_ACTION"
	publicAPIServiceSuspendedExp   = "SERVICE_SUSPENDED_EXPIRED"
	publicAPIServiceSuspended      = "SERVICE_SUSPENDED"
	publicAPIServiceAlreadyRunning = "SERVICE_ALREADY_RUNNING"
	publicAPIServiceAlreadyStopped = "SERVICE_ALREADY_STOPPED"
	publicAPIServiceTransferLocked = "SERVICE_TRANSFER_LOCKED"
	publicAPIServiceRestoreRunning = "SERVICE_RESTORE_IN_PROGRESS"
	publicAPIServiceUploadRunning  = "SERVICE_UPLOAD_IN_PROGRESS"
	publicAPIServiceTaskRunning    = "SERVICE_TASK_IN_PROGRESS"
	publicAPIInvalidRenewMonths    = "INVALID_SERVICE_RENEW_MONTHS"
	publicAPIFreeService           = "FREE_SERVICE"
	publicAPIServiceRenewFailed    = "SERVICE_RENEW_FAILED"
	publicAPIInvalidServiceTaskID  = "INVALID_SERVICE_TASK_ID"
	publicAPIServiceTaskNotFound   = "SERVICE_TASK_NOT_FOUND"
	publicAPIServiceTaskCannotStop = "SERVICE_TASK_CANNOT_CANCEL"

	publicAPIInvalidTicketStatus    = "INVALID_TICKET_STATUS"
	publicAPIInvalidTicketCategory  = "INVALID_TICKET_CATEGORY"
	publicAPIInvalidTicketPriority  = "INVALID_TICKET_PRIORITY"
	publicAPIInvalidTicketCreate    = "INVALID_TICKET_CREATE_PAYLOAD"
	publicAPIInvalidInstanceID      = "INVALID_INSTANCE_ID"
	publicAPIInstanceNotFound       = "INSTANCE_NOT_FOUND"
	publicAPIHostNotFound           = "HOST_NOT_FOUND"
	publicAPITicketCreationDisabled = "TICKET_CREATION_DISABLED"
	publicAPIInvalidTicketAttach    = "INVALID_TICKET_ATTACHMENT"
	publicAPIInvalidTicketID        = "INVALID_TICKET_ID"
	publicAPITicketNotFound         = "TICKET_NOT_FOUND"
	publicAPIInvalidTicketStatusAct = "INVALID_TICKET_STATUS_ACTION"
	publicAPITicketAlreadyClosed    = "TICKET_ALREADY_CLOSED"
	publicAPITicketNotClosed        = "TICKET_NOT_CLOSED"
	publicAPITicketStatusConflict   = "TICKET_STATUS_CONFLICT"
	publicAPIInvalidTicketReply     = "INVALID_TICKET_REPLY_CONTENT"
	publicAPITicketClosed           = "TICKET_CLOSED"

	publicAPIInvalidNotificationVars     = "INVALID_NOTIFICATION_VARIABLES"
	publicAPIInvalidNotificationTemplate = "INVALID_NOTIFICATION_TEMPLATE"
	publicAPIInvalidNotificationPayload  = "INVALID_NOTIFICATION_PAYLOAD"
)

// Error messages, verbatim from the TypeScript.
const (
	publicAPIUnauthorizedMessage        = "Unauthorized"
	publicAPIInvalidSortMessage         = "Invalid sort"
	publicAPIUserNotFoundMessage        = "User not found"
	publicAPIInvalidProfileUpdateMsg    = "avatarStyle must be one of the supported DiceBear style identifiers"
	publicAPIInvalidAdjustmentStatusMsg = "Invalid balance adjustment request status"
	publicAPIInvalidAdjustmentAmountMsg = "Invalid balance adjustment amount"
	publicAPIInvalidAdjustmentReasonMsg = "Invalid balance adjustment reason"
	publicAPIInvalidAdjustmentOrderMsg  = "Invalid balance adjustment order number"
	publicAPIPendingAdjustmentLimitMsg  = "Too many pending balance adjustment requests"
	publicAPIInvalidProductIDMessage    = "Invalid product id"
	publicAPIProductNotFoundMessage     = "Product not found"
	publicAPIInvalidOrderStatusMessage  = "Invalid order status"
	publicAPIInvalidOrderIDMessage      = "Invalid order id"
	publicAPIOrderNotFoundMessage       = "Order not found"
	publicAPIInvalidBillingTypeMessage  = "Invalid billing record type"
	publicAPIInvalidServiceIDMessage    = "Invalid service id"
	publicAPIInvalidBillingIDMessage    = "Invalid billing record id"
	publicAPIBillingNotFoundMessage     = "Billing record not found"
	publicAPIInvalidServiceStatusMsg    = "Invalid service status"
	publicAPIInvalidServiceIncludeMsg   = "Invalid service include"
	publicAPIServiceNotFoundMessage     = "Service not found"
	publicAPIInvalidServiceActionMsg    = "action must be one of start, stop, or restart"
	publicAPISuspendedOperationMessage  = "Suspended services cannot be operated through the public API"
	publicAPIAlreadyRunningMessage      = "Service is already running"
	publicAPIAlreadyStoppedMessage      = "Service is already stopped"
	publicAPITransferLockedMessage      = "Service has a pending transfer"
	publicAPIRestoreRunningMessage      = "Service has an active restore task"
	publicAPIUploadRunningMessage       = "Service has an active backup upload task"
	publicAPITaskRunningMessage         = "Service has an active operation task"
	publicAPIInvalidRenewMonthsMessage  = "months must be an integer between 1 and 24"
	publicAPIFreeServiceMessage         = "Free services do not need renewal"
	publicAPIInvalidServiceTaskIDMsg    = "Invalid service task id"
	publicAPIServiceTaskNotFoundMessage = "Service task not found"
	publicAPIServiceTaskCancelMessage   = "Only pending service tasks can be cancelled"
	publicAPIServiceTaskCanceledMessage = "Service task can no longer be cancelled"
	publicAPIInvalidTicketStatusMsg     = "Invalid ticket status"
	publicAPIInvalidTicketCategoryMsg   = "Invalid ticket category"
	publicAPIInvalidTicketPriorityMsg   = "Invalid ticket priority"
	publicAPIInvalidInstanceIDMessage   = "Invalid instance id"
	publicAPIInstanceNotFoundMessage    = "Instance not found"
	publicAPIHostNotFoundMessage        = "Host not found"
	publicAPITicketDisabledMessage      = "Ticket creation is disabled"
	publicAPIInvalidTicketIDMessage     = "Invalid ticket id"
	publicAPITicketNotFoundMessage      = "Ticket not found"
	publicAPIInvalidTicketStatusActMsg  = "action must be one of close or reopen"
	publicAPITicketAlreadyClosedMessage = "Ticket is already closed"
	publicAPITicketNotClosedMessage     = "Only closed tickets can be reopened"
	publicAPITicketConflictMessage      = "Ticket status changed concurrently"
	publicAPITicketClosedMessage        = "Cannot reply to a closed ticket"
)

// publicAPIInvalidTicketCreateMessage mirrors the interpolated TS message.
var publicAPIInvalidTicketCreateMessage = fmt.Sprintf(
	"subject must be %d-%d characters and content must be %d-%d characters when no images are attached",
	publicAPIMinTicketSubjectLength, publicAPIMaxTicketSubjectLength,
	publicAPIMinTicketCreateContentLength, publicAPIMaxTicketReplyLength,
)

// publicAPIInvalidTicketReplyMessage mirrors the interpolated TS message.
var publicAPIInvalidTicketReplyMessage = fmt.Sprintf(
	"content must be non-empty when no images are attached and no longer than %d characters",
	publicAPIMaxTicketReplyLength,
)

// publicAPIInvalidNotificationVarsMessage mirrors the interpolated TS message.
var publicAPIInvalidNotificationVarsMessage = fmt.Sprintf(
	"variables must be an object with up to %d scalar values; keys must be alphanumeric identifiers and values max %d characters",
	publicAPIMaxNotificationTemplateVars, publicAPIMaxNotificationVariableLen,
)

// publicAPIInvalidNotificationPayloadMessage mirrors the interpolated TS message.
var publicAPIInvalidNotificationPayloadMessage = fmt.Sprintf(
	"title and message must be non-empty strings unless a template is used; title max %d, message max %d",
	publicAPIMaxNotificationTitleLength, publicAPIMaxNotificationMessageLength,
)

// ==================== value sets ====================

var (
	publicAPITicketCategories = map[string]bool{"general": true, "billing": true, "technical": true, "abuse": true}
	publicAPITicketPriorities = map[string]bool{"low": true, "normal": true, "high": true, "urgent": true}
	publicAPITicketStatuses   = map[string]bool{"open": true, "in_progress": true, "resolved": true, "closed": true}
	publicAPIServiceStatuses  = map[string]bool{
		"creating": true, "running": true, "stopped": true,
		"suspended": true, "error": true, "deleted": true,
	}
	publicAPIServiceIncludes = map[string]bool{"product": true, "plan": true}
	publicAPICreatedAtSorts  = map[string]bool{"createdAt": true}
	publicAPIServiceSorts    = map[string]bool{"createdAt": true, "displayOrder": true}
	publicAPITicketSorts     = map[string]bool{"createdAt": true, "updatedAt": true}
	publicAPIOrderStatuses   = map[string]bool{"pending": true, "completed": true, "failed": true, "cancelled": true, "refunded": true}
	publicAPIBillingTypes    = map[string]bool{
		"newPurchase": true, "renew": true, "upgrade": true,
		"downgrade": true, "refund": true, "transfer_fee": true,
	}
	publicAPIAdjustmentStatuses = map[string]bool{"pending": true, "approved": true, "rejected": true}
	publicAPIAdjustmentTypes    = map[string]bool{"manual_adjust": true, "refund": true}
	publicAPIBalanceLogTypes    = map[string]bool{
		"recharge": true, "consume": true, "refund": true, "admin_adjust": true,
		"gift": true, "transfer_fee": true, "transfer_refund": true, "invite_generate": true,
	}
	publicAPINotificationTemplates = map[string]bool{
		"flash_sale_reminder": true, "service_action_update": true, "billing_notice": true,
	}
)

// publicAPIAvatarStyles mirrors PUBLIC_PROFILE_AVATAR_STYLES (order irrelevant).
var publicAPIAvatarStyles = map[string]bool{
	"adventurer": true, "adventurerNeutral": true, "avataaars": true, "avataaarsNeutral": true,
	"bigEars": true, "bigEarsNeutral": true, "bigSmile": true, "bottts": true, "botttsNeutral": true,
	"croodles": true, "croodlesNeutral": true, "dylan": true, "funEmoji": true, "glass": true, "icons": true,
	"identicon": true, "initials": true, "lorelei": true, "loreleiNeutral": true, "micah": true, "miniavs": true,
	"notionists": true, "notionistsNeutral": true, "openPeeps": true, "personas": true, "pixelArt": true,
	"pixelArtNeutral": true, "rings": true, "shapes": true, "thumbs": true,
}

// publicAPIOrderStatusFilters mirrors buildPublicOrderStatusFilter. An empty list
// means "this source contributes no rows for this status".
func publicAPIOrderStatusFilters(status string) (rechargeStatuses []string, billingTypes []string, filtered bool) {
	switch status {
	case "pending":
		return []string{"pending", "paid"}, []string{}, true
	case "completed":
		return []string{"completed"}, []string{"newPurchase", "renew", "upgrade", "downgrade", "transfer_fee"}, true
	case "failed":
		return []string{"failed"}, []string{}, true
	case "cancelled":
		return []string{"cancelled"}, []string{}, true
	case "refunded":
		return []string{"refunded"}, []string{"refund"}, true
	default:
		return nil, nil, false
	}
}

// ==================== neutral helpers ====================

// respondPublicAPIError writes the module's literal `{ error, code }` envelope.
func publicAPIRespondError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": message, "code": code})
}

// publicAPIRespondInternal mirrors an unhandled throw reaching app.ts'
// setErrorHandler in production: the details are never exposed.
func publicAPIRespondInternal(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// publicAPIStringOf mirrors `typeof value === 'string' ? value : ...` helpers.
// The *string case exists because the route handlers read ticket form fields via
// optionalString() (a *string), while JSON body fields arrive as plain any.
func publicAPIStringOf(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case *string:
		if typed == nil {
			return "", false
		}
		return *typed, true
	default:
		return "", false
	}
}

// publicAPIJSRuneIsSpace mirrors the Unicode WhiteSpace + LineTerminator set that
// JavaScript's trim() and the `\s` regexp class share.
func publicAPIJSRuneIsSpace(char rune) bool {
	switch char {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0x1680,
		0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return char >= 0x2000 && char <= 0x200A
}

// publicAPIJSTrim mirrors String.prototype.trim.
func publicAPIJSTrim(value string) string {
	return strings.TrimFunc(value, publicAPIJSRuneIsSpace)
}

// publicAPIJSCollapseWhitespace mirrors `.replace(/\s+/g, ' ')`.
func publicAPIJSCollapseWhitespace(value string) string {
	return publicAPIJSWhitespacePattern.ReplaceAllString(value, " ")
}

// publicAPIParsePositiveID mirrors parsePositiveId for an already-stringified
// route/query value: ^[1-9]\d*$ and a safe integer.
func publicAPIParsePositiveID(value string) (int64, bool) {
	if !publicAPIPositiveIDPattern.MatchString(value) {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed > publicAPIMaxSafeInteger {
		return 0, false
	}
	return parsed, true
}

// publicAPIParseOptionalPositiveID mirrors parseOptionalPositiveId:
// undefined/null/” mean "not provided" (defined=false); anything else must parse.
func publicAPIParseOptionalPositiveID(value *string) (parsed int64, defined bool, valid bool) {
	if value == nil || *value == "" {
		return 0, false, true
	}
	id, ok := publicAPIParsePositiveID(*value)
	return id, true, ok
}

// ==================== query parameter decoding ====================

// publicAPIQueryParam tells the three shapes Fastify exposes on request.query:
// absent, a single string, or an array (repeated parameter).
type publicAPIQueryParam struct {
	single   bool
	value    string
	repeated bool
}

// publicAPIQuery reads one query parameter with Fastify's object-vs-array
// distinction. A repeated parameter is an array in Fastify, which every
// `typeof value === 'string'` check turns into an invalid/undefined value; the
// caller decides which.
func publicAPIQuery(c *gin.Context, key string) publicAPIQueryParam {
	values, exists := c.Request.URL.Query()[key]
	if !exists {
		return publicAPIQueryParam{}
	}
	if len(values) != 1 {
		return publicAPIQueryParam{repeated: true}
	}
	return publicAPIQueryParam{single: true, value: values[0]}
}

// publicAPIQueryString mirrors `query[key]` for callers that only accept a string.
func (p publicAPIQueryParam) asString() (string, bool) {
	if !p.single {
		return "", false
	}
	return p.value, true
}

// ==================== pagination / sorting ====================

// publicAPIPagination mirrors parsePagination.
type publicAPIPagination struct {
	page     int
	pageSize int
	skip     int64
	take     int
}

func publicAPIParsePagination(query func(string) publicAPIQueryParam) publicAPIPagination {
	page := int64(1)
	if raw, ok := query("page").asString(); ok {
		if parsed, valid := publicAPIParsePositiveID(raw); valid {
			page = parsed
		}
	}
	if page < 1 {
		page = 1
	}

	pageSize := int64(20)
	if raw, ok := query("pageSize").asString(); ok {
		if parsed, valid := publicAPIParsePositiveID(raw); valid {
			pageSize = parsed
		}
	}
	if pageSize > publicAPIMaxPageSize {
		pageSize = publicAPIMaxPageSize
	}
	if pageSize < 1 {
		pageSize = 1
	}

	return publicAPIPagination{
		page:     int(page),
		pageSize: int(pageSize),
		skip:     (page - 1) * pageSize,
		take:     int(pageSize),
	}
}

// publicAPIPaginationMeta mirrors paginationMeta.
type publicAPIPaginationMeta struct {
	Page       int    `json:"page"`
	PageSize   int    `json:"pageSize"`
	Total      int64  `json:"total"`
	TotalPages int64  `json:"totalPages"`
	Sort       string `json:"sort"`
}

func publicAPIPaginationMetaFor(page, pageSize int, total int64, sortValue string) publicAPIPaginationMeta {
	totalPages := (total + int64(pageSize) - 1) / int64(pageSize)
	if totalPages < 1 {
		totalPages = 1
	}
	return publicAPIPaginationMeta{
		Page:       page,
		PageSize:   pageSize,
		Total:      total,
		TotalPages: totalPages,
		Sort:       sortValue,
	}
}

// publicAPISort mirrors PublicApiSort.
type publicAPISort struct {
	field     string
	direction string // "asc" | "desc"
	value     string
}

// publicAPIParseSort mirrors parsePublicApiSort. nil means "invalid → 400".
func publicAPIParseSort(param publicAPIQueryParam, allowed map[string]bool, defaultSort string) *publicAPISort {
	raw := defaultSort
	if param.single && param.value != "" {
		raw = param.value
	} else if param.repeated {
		return nil // an array is not a string → null → 400
	}

	trimmed := publicAPIJSTrim(raw)
	if trimmed == "" {
		return nil
	}
	direction := "asc"
	field := trimmed
	if strings.HasPrefix(trimmed, "-") {
		direction = "desc"
		field = trimmed[1:]
	}
	if field == "" || !allowed[field] {
		return nil
	}
	value := field
	if direction == "desc" {
		value = "-" + field
	}
	return &publicAPISort{field: field, direction: direction, value: value}
}

// publicAPISortColumn maps a whitelisted camelCase sort field to its SQL column.
func publicAPISortColumn(field string) string {
	switch field {
	case "createdAt":
		return "created_at"
	case "updatedAt":
		return "updated_at"
	case "displayOrder":
		return "display_order"
	default:
		return ""
	}
}

// publicAPIOrderBy mirrors buildPublicSortOrder / buildPublicServiceSortOrder.
func publicAPIOrderBy(sort *publicAPISort) string {
	column := publicAPISortColumn(sort.field)
	if column == "" {
		return "id " + sort.direction
	}
	if sort.field == "displayOrder" {
		return "display_order " + sort.direction + ", created_at desc, id desc"
	}
	return column + " " + sort.direction + ", id " + sort.direction
}

// publicAPIQualifyOrderBy prefixes an order clause with a table alias. The billing
// queries join instances/package_plans/packages, all of which carry id/created_at,
// so an unqualified ORDER BY would be ambiguous SQL.
func publicAPIQualifyOrderBy(alias, order string) string {
	parts := strings.Split(order, ", ")
	for i, part := range parts {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		fields[0] = alias + "." + fields[0]
		parts[i] = strings.Join(fields, " ")
	}
	return strings.Join(parts, ", ")
}

// publicAPIParseInclude mirrors parsePublicApiInclude. nil means invalid → 400.
func publicAPIParseInclude(param publicAPIQueryParam, allowed map[string]bool) []string {
	if param.repeated {
		return nil
	}
	if !param.single || param.value == "" {
		return []string{}
	}
	parts := strings.Split(param.value, ",")
	includes := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		trimmed := publicAPIJSTrim(part)
		if trimmed == "" {
			continue
		}
		if !allowed[trimmed] {
			return nil
		}
		if seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		includes = append(includes, trimmed)
	}
	return includes
}

// ==================== money / time serialization ====================

// publicAPIMoney mirrors toMoney(): Prisma Decimal → Number(decimal), and
// anything non-finite → 0.
func publicAPIMoney(value models.Decimal) float64 {
	converted, _ := value.Float64()
	if math.IsNaN(converted) || math.IsInf(converted, 0) {
		return 0
	}
	return converted
}

// publicAPIMoneyPtr mirrors `value === null ? null : toMoney(value)`.
func publicAPIMoneyPtr(value *models.Decimal) *float64 {
	if value == nil {
		return nil
	}
	converted := publicAPIMoney(*value)
	return &converted
}

// publicAPIBigIntString mirrors serializeBigInt (bigint → decimal string).
func publicAPIBigIntString(value *int64) *string {
	if value == nil {
		return nil
	}
	text := strconv.FormatInt(*value, 10)
	return &text
}

// publicAPIBigIntStringRequired mirrors serializeBigInt for non-nullable bigint.
func publicAPIBigIntStringRequired(value int64) string {
	return strconv.FormatInt(value, 10)
}

// publicAPIISO mirrors Date.prototype.toISOString for a UTC instant.
func publicAPIISO(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// publicAPIISOPtr mirrors `value?.toISOString() ?? null`.
func publicAPIISOPtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	text := publicAPIISO(*value)
	return &text
}

// publicAPIMaskTradeNo mirrors maskTradeNo.
func publicAPIMaskTradeNo(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	text := *value
	units := jsStringLength(text)
	if units <= 8 {
		runes := []rune(text)
		prefix := runes
		if len(runes) > 2 {
			prefix = runes[:2]
		}
		masked := string(prefix) + "***"
		return &masked
	}
	runes := []rune(text)
	if len(runes) <= 8 {
		// Non-ASCII trade number shorter than 9 UTF-16 units but 8 runes or fewer.
		masked := string(runes[:2]) + "***"
		return &masked
	}
	masked := string(runes[:4]) + "***" + string(runes[len(runes)-4:])
	return &masked
}

// ==================== body decoding ====================

// publicAPIReadBody decodes the request body the way Fastify leaves it for the
// handler: objects, arrays, strings, numbers and booleans are all possible, and a
// missing body is nil. The Fastify parser has already run (ValidateFastifyBody /
// ticketsParseFrameworkBody), so malformed JSON never reaches here.
func publicAPIReadBody(c *gin.Context) any {
	request := c.Request
	if request.Body == nil {
		return nil
	}
	contentType := request.Header.Get("Content-Type")
	if contentType == "" || !strings.Contains(strings.ToLower(contentType), "application/json") {
		return nil
	}
	raw, err := publicAPIReadAll(c)
	if err != nil || len(raw) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return decoded
}

// publicAPIBodyObject returns the body when it is a JSON object, otherwise nil —
// the equivalent of `request.body?.field` on a non-object body.
func publicAPIBodyObject(body any) map[string]any {
	object, _ := body.(map[string]any)
	return object
}

// publicAPIBodyField mirrors `body?.field` without panicking on non-objects.
func publicAPIBodyField(body any, field string) (any, bool) {
	object := publicAPIBodyObject(body)
	if object == nil {
		return nil, false
	}
	value, ok := object[field]
	return value, ok
}

// publicAPIScalarNumber mirrors `typeof value === 'number' ? value : ...`.
func publicAPIScalarNumber(value any) (float64, bool) {
	number, ok := value.(float64)
	return number, ok
}

// ==================== avatar / notification normalization ====================

// publicAPINormalizeAvatarStyle mirrors normalizePublicAvatarStyle.
func publicAPINormalizeAvatarStyle(value any) (string, bool) {
	text, ok := publicAPIStringOf(value)
	if !ok {
		return "", false
	}
	trimmed := publicAPIJSTrim(text)
	if !publicAPIAvatarStyles[trimmed] {
		return "", false
	}
	return trimmed, true
}

// publicAPINormalizeNotificationText mirrors normalizePublicNotificationText.
func publicAPINormalizeNotificationText(value any, maxLength int) *string {
	text, ok := publicAPIStringOf(value)
	if !ok {
		return nil
	}
	trimmed := publicAPIJSTrim(text)
	if trimmed == "" || jsStringLength(trimmed) > maxLength {
		return nil
	}
	return &trimmed
}

// publicAPINormalizeNotificationTemplate mirrors normalizePublicNotificationTemplate.
func publicAPINormalizeNotificationTemplate(value any) (string, bool) {
	if value == nil {
		return "", false
	}
	text, ok := publicAPIStringOf(value)
	if !ok {
		return "", false
	}
	trimmed := publicAPIJSTrim(text)
	if trimmed == "" || !publicAPINotificationTemplates[trimmed] {
		return "", false
	}
	return trimmed, true
}

// publicAPINormalizeNotificationVariables mirrors normalizePublicNotificationVariables.
// nil means invalid → 400.
func publicAPINormalizeNotificationVariables(value any) (map[string]string, bool) {
	if value == nil {
		return map[string]string{}, true
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	entries := make([]string, 0, len(object))
	for key := range object {
		entries = append(entries, key)
	}
	sort.Strings(entries) // deterministic error order; JSON object order is not part of the contract
	if len(entries) > publicAPIMaxNotificationTemplateVars {
		return nil, false
	}

	normalized := make(map[string]string, len(entries))
	for _, key := range entries {
		if !publicAPINotificationVariableKeyPattern.MatchString(key) {
			return nil, false
		}
		raw := object[key]
		if raw == nil {
			continue
		}
		var text string
		switch typed := raw.(type) {
		case string:
			text = typed
		case float64:
			text = formatTerminalCommandNumber(typed)
		case bool:
			text = strconv.FormatBool(typed)
		default:
			return nil, false
		}
		trimmed := publicAPIJSTrim(text)
		if trimmed == "" || jsStringLength(trimmed) > publicAPIMaxNotificationVariableLen {
			return nil, false
		}
		normalized[key] = trimmed
	}
	return normalized, true
}

// publicAPIRenderNotificationTemplate mirrors renderPublicNotificationTemplate.
func publicAPIRenderNotificationTemplate(template string, variables map[string]string) (title, message string, ok bool) {
	switch template {
	case "flash_sale_reminder":
		campaignName, hasCampaign := variables["campaignName"]
		startsAt, hasStarts := variables["startsAt"]
		if !hasCampaign || !hasStarts || campaignName == "" || startsAt == "" {
			return "", "", false
		}
		productLine := ""
		if productName, hasProduct := variables["productName"]; hasProduct && productName != "" {
			productLine = "\n商品: " + productName
		}
		return "秒杀活动提醒", "活动「" + campaignName + "」将在 " + startsAt + " 开始。" + productLine, true
	case "service_action_update":
		serviceName, hasService := variables["serviceName"]
		statusText, hasStatus := variables["statusText"]
		if !hasService || !hasStatus || serviceName == "" || statusText == "" {
			return "", "", false
		}
		return "服务任务更新", "服务「" + serviceName + "」" + statusText + "。", true
	case "billing_notice":
		subject, hasSubject := variables["subject"]
		amount, hasAmount := variables["amount"]
		if !hasSubject || !hasAmount || subject == "" || amount == "" {
			return "", "", false
		}
		note := ""
		if rawNote, hasNote := variables["note"]; hasNote && rawNote != "" {
			note = "\n" + rawNote
		}
		return "账单提醒", "账单「" + subject + "」金额 ¥" + amount + "。" + note, true
	default:
		return "", "", false
	}
}

// ==================== persistence rows ====================

type publicAPIUserBalanceRow struct {
	ID        int32          `gorm:"column:id"`
	Balance   models.Decimal `gorm:"column:balance"`
	UpdatedAt time.Time      `gorm:"column:updated_at"`
}

type publicAPIBalanceLogRow struct {
	ID            int32                 `gorm:"column:id"`
	Type          models.BalanceLogType `gorm:"column:type"`
	Amount        models.Decimal        `gorm:"column:amount"`
	BalanceBefore models.Decimal        `gorm:"column:balance_before"`
	BalanceAfter  models.Decimal        `gorm:"column:balance_after"`
	OrderID       *string               `gorm:"column:order_id"`
	InstanceID    *int32                `gorm:"column:instance_id"`
	Remark        *string               `gorm:"column:remark"`
	CreatedAt     time.Time             `gorm:"column:created_at"`
}

type publicAPIAdjustmentRow struct {
	ID           int32                                 `gorm:"column:id"`
	UserID       int32                                 `gorm:"column:user_id"`
	Amount       models.Decimal                        `gorm:"column:amount"`
	RequestType  models.BalanceAdjustmentRequestType   `gorm:"column:request_type"`
	Status       models.BalanceAdjustmentRequestStatus `gorm:"column:status"`
	OrderNo      *string                               `gorm:"column:order_no"`
	Reason       string                                `gorm:"column:reason"`
	ReviewRemark *string                               `gorm:"column:review_remark"`
	CreatedAt    time.Time                             `gorm:"column:created_at"`
	UpdatedAt    time.Time                             `gorm:"column:updated_at"`
	ReviewedAt   *time.Time                            `gorm:"column:reviewed_at"`
}

type publicAPIProductRow struct {
	ID                  int32   `gorm:"column:id"`
	Name                string  `gorm:"column:name"`
	Description         *string `gorm:"column:description"`
	InstanceType        string  `gorm:"column:instance_type"`
	NetworkMode         string  `gorm:"column:network_mode"`
	Active              bool    `gorm:"column:active"`
	CPUMax              int32   `gorm:"column:cpu_max"`
	MemoryMax           int32   `gorm:"column:memory_max"`
	DiskMax             int32   `gorm:"column:disk_max"`
	BandwidthMax        *int32  `gorm:"column:bandwidth_max"`
	PortLimit           int32   `gorm:"column:port_limit"`
	SnapshotLimit       int32   `gorm:"column:snapshot_limit"`
	BackupLimit         int32   `gorm:"column:backup_limit"`
	SiteLimit           int32   `gorm:"column:site_limit"`
	MonthlyTrafficLimit *int64  `gorm:"column:monthly_traffic_limit"`
}

type publicAPIPlanRow struct {
	ID                int32           `gorm:"column:id"`
	PackageID         int32           `gorm:"column:package_id"`
	Name              string          `gorm:"column:name"`
	Description       *string         `gorm:"column:description"`
	CPU               int32           `gorm:"column:cpu"`
	Memory            int32           `gorm:"column:memory"`
	Disk              int32           `gorm:"column:disk"`
	Price             models.Decimal  `gorm:"column:price"`
	SetupFee          models.Decimal  `gorm:"column:setup_fee"`
	BillingCycle      int32           `gorm:"column:billing_cycle"`
	IsSoldOut         bool            `gorm:"column:is_sold_out"`
	TrafficLimit      int64           `gorm:"column:traffic_limit"`
	TrafficLimitSpeed string          `gorm:"column:traffic_limit_speed"`
	SlaGuarantee      *models.Decimal `gorm:"column:sla_guarantee"`
}

type publicAPIPlanRefRow struct {
	ID           int32          `gorm:"column:id"`
	Name         string         `gorm:"column:name"`
	BillingCycle int32          `gorm:"column:billing_cycle"`
	Price        models.Decimal `gorm:"column:price"`
	PackageID    *int32         `gorm:"column:package_id"`
	PackageName  *string        `gorm:"column:package_name"`
}

type publicAPIRechargeRow struct {
	ID            int32                 `gorm:"column:id"`
	OrderNo       string                `gorm:"column:order_no"`
	TradeNo       *string               `gorm:"column:trade_no"`
	Amount        models.Decimal        `gorm:"column:amount"`
	ActualAmount  *models.Decimal       `gorm:"column:actual_amount"`
	Fee           models.Decimal        `gorm:"column:fee"`
	PaymentMethod *string               `gorm:"column:payment_method"`
	Status        models.RechargeStatus `gorm:"column:status"`
	FailReason    *string               `gorm:"column:fail_reason"`
	CreatedAt     time.Time             `gorm:"column:created_at"`
	CompletedAt   *time.Time            `gorm:"column:completed_at"`
	ExpiredAt     *time.Time            `gorm:"column:expired_at"`
}

type publicAPIBillingRow struct {
	ID             int32                    `gorm:"column:id"`
	InstanceID     int32                    `gorm:"column:instance_id"`
	Type           models.BillingRecordType `gorm:"column:type"`
	Amount         models.Decimal           `gorm:"column:amount"`
	Months         int32                    `gorm:"column:months"`
	PeriodStart    time.Time                `gorm:"column:period_start"`
	PeriodEnd      time.Time                `gorm:"column:period_end"`
	Remark         *string                  `gorm:"column:remark"`
	CreatedAt      time.Time                `gorm:"column:created_at"`
	InstanceName   *string                  `gorm:"column:instance_name"`
	InstanceStatus *string                  `gorm:"column:instance_status"`
	PlanID         *int32                   `gorm:"column:plan_id"`
	PlanName       *string                  `gorm:"column:plan_name"`
	PackageID      *int32                   `gorm:"column:package_id"`
	PackageName    *string                  `gorm:"column:package_name"`
}

type publicAPIServiceRow struct {
	ID                  int32           `gorm:"column:id"`
	Name                string          `gorm:"column:name"`
	Status              string          `gorm:"column:status"`
	Image               string          `gorm:"column:image"`
	CPU                 int32           `gorm:"column:cpu"`
	Memory              int32           `gorm:"column:memory"`
	Disk                int32           `gorm:"column:disk"`
	IPv4                *string         `gorm:"column:ipv4"`
	IPv6                *string         `gorm:"column:ipv6"`
	NetworkMode         string          `gorm:"column:network_mode"`
	PortLimit           *int32          `gorm:"column:port_limit"`
	SnapshotLimit       *int32          `gorm:"column:snapshot_limit"`
	BackupLimit         *int32          `gorm:"column:backup_limit"`
	SiteLimit           *int32          `gorm:"column:site_limit"`
	MonthlyTrafficLimit *int64          `gorm:"column:monthly_traffic_limit"`
	MonthlyTrafficUsed  int64           `gorm:"column:monthly_traffic_used"`
	TrafficStatus       string          `gorm:"column:traffic_status"`
	ExpiresAt           *time.Time      `gorm:"column:expires_at"`
	SuspendedAt         *time.Time      `gorm:"column:suspended_at"`
	SuspendReason       *string         `gorm:"column:suspend_reason"`
	BillingPrice        *models.Decimal `gorm:"column:billing_price"`
	BillingCycle        *int32          `gorm:"column:billing_cycle"`
	AutoRenew           bool            `gorm:"column:auto_renew"`
	CreatedAt           time.Time       `gorm:"column:created_at"`
	UpdatedAt           time.Time       `gorm:"column:updated_at"`
	PackagePlanID       *int32          `gorm:"column:package_plan_id"`
}

type publicAPIServiceOperationRow struct {
	ID            int32   `gorm:"column:id"`
	HostID        int32   `gorm:"column:host_id"`
	UserID        int32   `gorm:"column:user_id"`
	Name          string  `gorm:"column:name"`
	Status        string  `gorm:"column:status"`
	SuspendReason *string `gorm:"column:suspend_reason"`
}

type publicAPIServiceRenewRow struct {
	ID            int32      `gorm:"column:id"`
	HostID        int32      `gorm:"column:host_id"`
	PackagePlanID *int32     `gorm:"column:package_plan_id"`
	ExpiresAt     *time.Time `gorm:"column:expires_at"`
}

type publicAPIOperationTaskRow struct {
	ID         int32      `gorm:"column:id"`
	InstanceID int32      `gorm:"column:instance_id"`
	TaskType   string     `gorm:"column:task_type"`
	Status     string     `gorm:"column:status"`
	Progress   *string    `gorm:"column:progress"`
	Error      *string    `gorm:"column:error"`
	HostID     int32      `gorm:"column:host_id"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	StartedAt  *time.Time `gorm:"column:started_at"`
	FinishedAt *time.Time `gorm:"column:finished_at"`
}

type publicAPITicketRow struct {
	ID         int32      `gorm:"column:id"`
	Subject    string     `gorm:"column:subject"`
	Category   string     `gorm:"column:category"`
	Priority   string     `gorm:"column:priority"`
	Status     string     `gorm:"column:status"`
	HostID     *int32     `gorm:"column:host_id"`
	InstanceID *int32     `gorm:"column:instance_id"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	UpdatedAt  time.Time  `gorm:"column:updated_at"`
	ResolvedAt *time.Time `gorm:"column:resolved_at"`
	ClosedAt   *time.Time `gorm:"column:closed_at"`
}

type publicAPILatestMessageRow struct {
	TicketID    int32     `gorm:"column:ticket_id"`
	ID          int32     `gorm:"column:id"`
	SenderID    int32     `gorm:"column:sender_id"`
	Content     string    `gorm:"column:content"`
	IsFromOwner bool      `gorm:"column:is_from_owner"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

type publicAPITicketMessageRow struct {
	ID          int32     `gorm:"column:id"`
	TicketID    int32     `gorm:"column:ticket_id"`
	SenderID    int32     `gorm:"column:sender_id"`
	Content     string    `gorm:"column:content"`
	IsFromOwner bool      `gorm:"column:is_from_owner"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

type publicAPIAttachmentRow struct {
	ID           int32     `gorm:"column:id"`
	TicketID     int32     `gorm:"column:ticket_id"`
	MessageID    int32     `gorm:"column:message_id"`
	Filename     string    `gorm:"column:filename"`
	OriginalName string    `gorm:"column:original_name"`
	MimeType     string    `gorm:"column:mime_type"`
	SizeBytes    int32     `gorm:"column:size_bytes"`
	ThumbnailURL *string   `gorm:"column:thumbnail_url"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

type publicAPINotificationRow struct {
	ID        int32     `gorm:"column:id"`
	EventType string    `gorm:"column:event_type"`
	Title     string    `gorm:"column:title"`
	Content   string    `gorm:"column:content"`
	IsRead    bool      `gorm:"column:is_read"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

type publicAPITicketSummaryRow struct {
	ID         int32   `gorm:"column:id"`
	UserID     int32   `gorm:"column:user_id"`
	Subject    string  `gorm:"column:subject"`
	Status     string  `gorm:"column:status"`
	Name       string  `gorm:"column:name"`
	HostID     *int32  `gorm:"column:host_id"`
	HostName   *string `gorm:"column:host_name"`
	HostUserID *int32  `gorm:"column:host_user_id"`
	HostRole   *string `gorm:"column:host_role"`
}

// ==================== queries ====================

// publicAPIReadAll reads the remaining body. The Fastify parser (or the ticket
// parser) has already enforced the size limit, so this is only a convenience.
func publicAPIReadAll(c *gin.Context) ([]byte, error) {
	if c.Request.Body == nil {
		return nil, nil
	}
	return io.ReadAll(c.Request.Body)
}

func publicAPILoadUserBalance(ctx context.Context, userID int32) (*publicAPIUserBalanceRow, error) {
	var rows []publicAPIUserBalanceRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, balance, updated_at FROM users WHERE id = ? LIMIT 1`, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func publicAPIUpdateAvatarStyle(ctx context.Context, userID int32, avatarStyle string) (*publicAPIProfileView, error) {
	if err := db.DB.WithContext(ctx).Exec(
		`UPDATE users SET avatar_style = ?, updated_at = ? WHERE id = ?`,
		avatarStyle, time.Now().UTC(), userID,
	).Error; err != nil {
		return nil, err
	}
	var rows []struct {
		ID            int32   `gorm:"column:id"`
		Username      string  `gorm:"column:username"`
		Email         *string `gorm:"column:email"`
		Role          string  `gorm:"column:role"`
		AvatarStyle   string  `gorm:"column:avatar_style"`
		AvatarBadgeID *string `gorm:"column:avatar_badge_id"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, username, email, role::text AS role, avatar_style, avatar_badge_id
		 FROM users WHERE id = ? LIMIT 1`, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	return &publicAPIProfileView{
		ID:            row.ID,
		Username:      row.Username,
		Email:         row.Email,
		Role:          row.Role,
		AvatarStyle:   row.AvatarStyle,
		AvatarBadgeID: row.AvatarBadgeID,
	}, nil
}

// publicAPIBalanceLogWhere builds the WHERE clause shared by count and list.
func publicAPIBalanceLogWhere(userID int32, logType string, lotteryGift string) (string, []any) {
	clauses := []string{"user_id = ?"}
	args := []any{userID}

	if lotteryGift == "exclude" {
		clauses = append(clauses, "NOT (type = ? AND remark LIKE ?)")
		args = append(args, string(models.BalanceLogTypeGift), "%抽奖中奖%")
	} else if lotteryGift == "only" {
		clauses = append(clauses, "type = ? AND remark LIKE ?")
		args = append(args, string(models.BalanceLogTypeGift), "%抽奖中奖%")
	} else if logType != "" {
		clauses = append(clauses, "type = ?")
		args = append(args, logType)
	}

	return strings.Join(clauses, " AND "), args
}

func publicAPICountBalanceLogs(ctx context.Context, where string, args []any) (int64, error) {
	var total int64
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM balance_logs WHERE "+where, args...,
	).Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func publicAPIListBalanceLogs(ctx context.Context, where string, args []any, order string, limit, offset int64) ([]publicAPIBalanceLogRow, error) {
	query := "SELECT id, type::text AS type, amount, balance_before, balance_after, order_id, instance_id, remark, created_at " +
		"FROM balance_logs WHERE " + where + " ORDER BY " + order + " LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	var rows []publicAPIBalanceLogRow
	if err := db.DB.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// publicAPIInstanceNameMap mirrors the `instance.findMany({ where: { id: { in },
// userId }})` lookup, which is user-scoped on purpose: a log pointing at another
// user's instance must serialize as null.
func publicAPIInstanceNameMap(ctx context.Context, ids []int32, userID int32) (map[int32]string, error) {
	result := map[int32]string{}
	if len(ids) == 0 {
		return result, nil
	}
	var rows []struct {
		ID   int32  `gorm:"column:id"`
		Name string `gorm:"column:name"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT id, name FROM instances WHERE id IN ? AND user_id = ?", ids, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.ID] = row.Name
	}
	return result, nil
}

func publicAPIAdjustmentWhere(userID int32, status string) (string, []any) {
	clauses := []string{"user_id = ?", "requested_by_user_id = ?", "source_type = 'public_api'"}
	args := []any{userID, userID}
	if status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, status)
	}
	return strings.Join(clauses, " AND "), args
}

func publicAPICountAdjustments(ctx context.Context, where string, args []any) (int64, error) {
	var total int64
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM balance_adjustment_requests WHERE "+where, args...,
	).Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func publicAPIListAdjustments(ctx context.Context, where string, args []any, order string, limit, offset int64) ([]publicAPIAdjustmentRow, error) {
	query := `SELECT id, user_id, amount, request_type::text AS request_type, status::text AS status,
		order_no, reason, review_remark, created_at, updated_at, reviewed_at
		FROM balance_adjustment_requests WHERE ` + where + " ORDER BY " + order + " LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	var rows []publicAPIAdjustmentRow
	if err := db.DB.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// publicAPIProductWhere is the `active: true, plans: { some: { isActive: true } }`
// predicate.
const publicAPIProductWhere = `p.active = true AND EXISTS (
	SELECT 1 FROM package_plans pl WHERE pl.package_id = p.id AND pl.is_active = true)`

func publicAPICountProducts(ctx context.Context) (int64, error) {
	var total int64
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM packages p WHERE " + publicAPIProductWhere,
	).Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

const publicAPIProductColumns = `p.id, p.name, p.description, p.instance_type::text AS instance_type,
	p.network_mode::text AS network_mode, p.active, p.cpu_max, p.memory_max, p.disk_max,
	p.bandwidth_max, p.port_limit, p.snapshot_limit, p.backup_limit, p.site_limit,
	p.monthly_traffic_limit`

func publicAPIListProducts(ctx context.Context, order string, limit, offset int64) ([]publicAPIProductRow, error) {
	var rows []publicAPIProductRow
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT "+publicAPIProductColumns+" FROM packages p WHERE "+publicAPIProductWhere+
			" ORDER BY "+order+" LIMIT ? OFFSET ?", limit, offset,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func publicAPIGetProduct(ctx context.Context, id int64) (*publicAPIProductRow, error) {
	var rows []publicAPIProductRow
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT "+publicAPIProductColumns+" FROM packages p WHERE p.id = ? AND "+publicAPIProductWhere+" LIMIT 1", id,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

const publicAPIPlanColumns = `id, package_id, name, description, cpu, memory, disk, price,
	setup_fee, billing_cycle, is_sold_out, traffic_limit, traffic_limit_speed, sla_guarantee`

func publicAPIListPlansForPackages(ctx context.Context, packageIDs []int32) (map[int32][]publicAPIPlanRow, error) {
	result := map[int32][]publicAPIPlanRow{}
	if len(packageIDs) == 0 {
		return result, nil
	}
	var rows []publicAPIPlanRow
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT "+publicAPIPlanColumns+" FROM package_plans WHERE package_id IN ? AND is_active = true "+
			"ORDER BY sort_order asc, id asc", packageIDs,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.PackageID] = append(result[row.PackageID], row)
	}
	return result, nil
}

func publicAPICountRecharges(ctx context.Context, where string, args []any) (int64, error) {
	var total int64
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM recharge_records WHERE "+where, args...,
	).Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func publicAPIListRecharges(ctx context.Context, where string, args []any, order string, limit int64) ([]publicAPIRechargeRow, error) {
	query := `SELECT id, order_no, trade_no, amount, actual_amount, fee, payment_method,
		status::text AS status, fail_reason, created_at, completed_at, expired_at
		FROM recharge_records WHERE ` + where + " ORDER BY " + order + " LIMIT ?"
	args = append(args, limit)
	var rows []publicAPIRechargeRow
	if err := db.DB.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func publicAPIRechargeWhere(userID int32, statuses []string) (string, []any) {
	clauses := []string{"user_id = ?"}
	args := []any{userID}
	if statuses != nil {
		clauses = append(clauses, "status IN ?")
		args = append(args, statuses)
	}
	return strings.Join(clauses, " AND "), args
}

const publicAPIBillingSelect = `SELECT b.id, b.instance_id, b.type::text AS type, b.amount, b.months,
	b.period_start, b.period_end, b.remark, b.created_at,
	i.name AS instance_name, i.status::text AS instance_status,
	pl.id AS plan_id, pl.name AS plan_name, pkg.id AS package_id, pkg.name AS package_name
	FROM instance_billing_records b
	LEFT JOIN instances i ON i.id = b.instance_id
	LEFT JOIN package_plans pl ON pl.id = i.package_plan_id
	LEFT JOIN packages pkg ON pkg.id = pl.package_id`

func publicAPIBillingWhere(userID int32, types []string, serviceID *int64) (string, []any) {
	clauses := []string{"b.user_id = ?"}
	args := []any{userID}
	if types != nil {
		clauses = append(clauses, "b.type IN ?")
		args = append(args, types)
	}
	if serviceID != nil {
		clauses = append(clauses, "b.instance_id = ?")
		args = append(args, *serviceID)
	}
	return strings.Join(clauses, " AND "), args
}

func publicAPICountBillings(ctx context.Context, where string, args []any) (int64, error) {
	var total int64
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM instance_billing_records b WHERE "+where, args...,
	).Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func publicAPIListBillings(ctx context.Context, where string, args []any, order string, limit, offset int64) ([]publicAPIBillingRow, error) {
	query := publicAPIBillingSelect + " WHERE " + where + " ORDER BY " + publicAPIQualifyOrderBy("b", order) + " LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	var rows []publicAPIBillingRow
	if err := db.DB.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func publicAPIGetBilling(ctx context.Context, id int64, userID int32) (*publicAPIBillingRow, error) {
	var rows []publicAPIBillingRow
	if err := db.DB.WithContext(ctx).Raw(
		publicAPIBillingSelect+" WHERE b.id = ? AND b.user_id = ? LIMIT 1", id, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func publicAPIGetRecharge(ctx context.Context, id int64, userID int32) (*publicAPIRechargeRow, error) {
	var rows []publicAPIRechargeRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, order_no, trade_no, amount, actual_amount, fee, payment_method,
		 status::text AS status, fail_reason, created_at, completed_at, expired_at
		 FROM recharge_records WHERE id = ? AND user_id = ? LIMIT 1`, id, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

const publicAPIServiceColumns = `id, name, status::text AS status, image, cpu, memory, disk, ipv4, ipv6,
	network_mode::text AS network_mode, port_limit, snapshot_limit, backup_limit, site_limit,
	monthly_traffic_limit, monthly_traffic_used, traffic_status::text AS traffic_status,
	expires_at, suspended_at, suspend_reason, billing_price, billing_cycle, auto_renew,
	created_at, updated_at, package_plan_id`

func publicAPIServiceWhere(userID int32, status string) (string, []any) {
	clauses := []string{"user_id = ?"}
	args := []any{userID}
	if status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, status)
	}
	return strings.Join(clauses, " AND "), args
}

func publicAPICountServices(ctx context.Context, where string, args []any) (int64, error) {
	var total int64
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM instances WHERE "+where, args...,
	).Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func publicAPIListServices(ctx context.Context, where string, args []any, order string, limit, offset int64) ([]publicAPIServiceRow, error) {
	query := "SELECT " + publicAPIServiceColumns + " FROM instances WHERE " + where +
		" ORDER BY " + order + " LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	var rows []publicAPIServiceRow
	if err := db.DB.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func publicAPIGetService(ctx context.Context, id int64, userID int32) (*publicAPIServiceRow, error) {
	var rows []publicAPIServiceRow
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT "+publicAPIServiceColumns+" FROM instances WHERE id = ? AND user_id = ? LIMIT 1", id, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func publicAPILoadPlanRefs(ctx context.Context, planIDs []int32) (map[int32]publicAPIPlanRefRow, error) {
	result := map[int32]publicAPIPlanRefRow{}
	if len(planIDs) == 0 {
		return result, nil
	}
	var rows []publicAPIPlanRefRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT pl.id, pl.name, pl.billing_cycle, pl.price,
		 pkg.id AS package_id, pkg.name AS package_name
		 FROM package_plans pl LEFT JOIN packages pkg ON pkg.id = pl.package_id
		 WHERE pl.id IN ?`, planIDs,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.ID] = row
	}
	return result, nil
}

func publicAPIListPlanRefsForPackage(ctx context.Context, packageID int64) ([]publicAPIPlanRow, error) {
	var rows []publicAPIPlanRow
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT "+publicAPIPlanColumns+" FROM package_plans WHERE package_id = ? AND is_active = true "+
			"ORDER BY sort_order asc, id asc", packageID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func publicAPILoadServiceOperation(ctx context.Context, id, userID int64) (*publicAPIServiceOperationRow, error) {
	var rows []publicAPIServiceOperationRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, host_id, user_id, name, status::text AS status, suspend_reason
		 FROM instances WHERE id = ? AND user_id = ? LIMIT 1`, id, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func publicAPILoadServiceForRenew(ctx context.Context, id, userID int64) (*publicAPIServiceRenewRow, error) {
	var rows []publicAPIServiceRenewRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, host_id, package_plan_id, expires_at FROM instances WHERE id = ? AND user_id = ? LIMIT 1`,
		id, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func publicAPILoadOperationTask(ctx context.Context, userID, serviceID, taskID int64) (*publicAPIOperationTaskRow, error) {
	var rows []publicAPIOperationTaskRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, instance_id, task_type::text AS task_type, status::text AS status, progress, error,
		 host_id, created_at, started_at, finished_at
		 FROM instance_tasks
		 WHERE id = ? AND instance_id = ? AND user_id = ? AND task_type IN ('start','stop','restart')
		 LIMIT 1`, taskID, serviceID, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func publicAPITicketWhere(userID int32, status, category, priority string) (string, []any) {
	clauses := []string{"user_id = ?"}
	args := []any{userID}
	if status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, status)
	}
	if category != "" {
		clauses = append(clauses, "category = ?")
		args = append(args, category)
	}
	if priority != "" {
		clauses = append(clauses, "priority = ?")
		args = append(args, priority)
	}
	return strings.Join(clauses, " AND "), args
}

func publicAPICountTickets(ctx context.Context, where string, args []any) (int64, error) {
	var total int64
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM tickets WHERE "+where, args...,
	).Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func publicAPIListTickets(ctx context.Context, where string, args []any, order string, limit, offset int64) ([]publicAPITicketRow, error) {
	query := `SELECT id, subject, category, priority::text AS priority, status::text AS status,
		host_id, instance_id, created_at, updated_at, resolved_at, closed_at
		FROM tickets WHERE ` + where + " ORDER BY " + order + " LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	var rows []publicAPITicketRow
	if err := db.DB.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// publicAPILatestMessages mirrors Prisma's `messages: { orderBy: { createdAt:
// desc }, take: 1 }` include: one message per ticket.
func publicAPILatestMessages(ctx context.Context, ticketIDs []int32) (map[int32]publicAPILatestMessageRow, error) {
	result := map[int32]publicAPILatestMessageRow{}
	if len(ticketIDs) == 0 {
		return result, nil
	}
	var rows []publicAPILatestMessageRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT DISTINCT ON (ticket_id) ticket_id, id, sender_id, content, is_from_owner, created_at
		 FROM ticket_messages WHERE ticket_id IN ?
		 ORDER BY ticket_id, created_at DESC, id DESC`, ticketIDs,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.TicketID] = row
	}
	return result, nil
}

func publicAPIGetTicket(ctx context.Context, id, userID int64) (*publicAPITicketRow, error) {
	var rows []publicAPITicketRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, subject, category, priority::text AS priority, status::text AS status,
		 host_id, instance_id, created_at, updated_at, resolved_at, closed_at
		 FROM tickets WHERE id = ? AND user_id = ? LIMIT 1`, id, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func publicAPIListTicketMessages(ctx context.Context, ticketID int32) ([]publicAPITicketMessageRow, error) {
	var rows []publicAPITicketMessageRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, ticket_id, sender_id, content, is_from_owner, created_at
		 FROM ticket_messages WHERE ticket_id = ? ORDER BY created_at asc, id asc`, ticketID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func publicAPIListAttachments(ctx context.Context, ticketID int32, messageIDs []int32) (map[int32][]publicAPIAttachmentRow, error) {
	result := map[int32][]publicAPIAttachmentRow{}
	if len(messageIDs) == 0 {
		return result, nil
	}
	var rows []publicAPIAttachmentRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, ticket_id, message_id, filename, original_name, mime_type, size_bytes, thumbnail_url, created_at
		 FROM ticket_message_attachments WHERE ticket_id = ? AND message_id IN ? ORDER BY id asc`,
		ticketID, messageIDs,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.MessageID] = append(result[row.MessageID], row)
	}
	return result, nil
}

func publicAPIListMessageAttachments(ctx context.Context, ticketID, messageID int32) ([]publicAPIAttachmentRow, error) {
	var rows []publicAPIAttachmentRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, ticket_id, message_id, filename, original_name, mime_type, size_bytes, thumbnail_url, created_at
		 FROM ticket_message_attachments WHERE ticket_id = ? AND message_id = ? ORDER BY id asc`,
		ticketID, messageID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// publicAPITicketHostOwnership is the `host: { select: { id, name, userId,
// user: { select: { role } } } }` include used by the reply flow.
type publicAPITicketHostOwnership struct {
	ID     int32  `gorm:"column:id"`
	Name   string `gorm:"column:name"`
	UserID int32  `gorm:"column:user_id"`
	Role   string `gorm:"column:role"`
}

func publicAPIHostOwnership(ctx context.Context, hostID int32) (*publicAPITicketHostOwnership, error) {
	var rows []publicAPITicketHostOwnership
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT h.id, h.name, h.user_id, u.role::text AS role
		 FROM hosts h LEFT JOIN users u ON u.id = h.user_id WHERE h.id = ? LIMIT 1`, hostID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func publicAPITicketReplyContext(ctx context.Context, id, userID int64) (*publicAPITicketSummaryRow, error) {
	var rows []publicAPITicketSummaryRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT t.id, t.user_id, t.subject, t.status::text AS status, t.host_id,
		 h.name AS host_name, h.user_id AS host_user_id, u.role::text AS host_role
		 FROM tickets t
		 LEFT JOIN hosts h ON h.id = t.host_id
		 LEFT JOIN users u ON u.id = h.user_id
		 WHERE t.id = ? AND t.user_id = ? LIMIT 1`, id, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func publicAPIInstanceForTicket(ctx context.Context, id, userID int64) (*struct {
	ID     int32  `gorm:"column:id"`
	Name   string `gorm:"column:name"`
	HostID int32  `gorm:"column:host_id"`
}, error) {
	var rows []struct {
		ID     int32  `gorm:"column:id"`
		Name   string `gorm:"column:name"`
		HostID int32  `gorm:"column:host_id"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, name, host_id FROM instances WHERE id = ? AND user_id = ? LIMIT 1`, id, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func publicAPINotificationWhere(userID int32, isRead *bool) (string, []any) {
	clauses := []string{"user_id = ?"}
	args := []any{userID}
	if isRead != nil {
		clauses = append(clauses, "is_read = ?")
		args = append(args, *isRead)
	}
	return strings.Join(clauses, " AND "), args
}

func publicAPICountNotifications(ctx context.Context, where string, args []any) (int64, error) {
	var total int64
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM inbox_messages WHERE "+where, args...,
	).Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func publicAPIListNotifications(ctx context.Context, where string, args []any, order string, limit, offset int64) ([]publicAPINotificationRow, error) {
	query := `SELECT id, event_type, title, content, is_read, created_at
		FROM inbox_messages WHERE ` + where + " ORDER BY " + order + " LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	var rows []publicAPINotificationRow
	if err := db.DB.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func publicAPIUnreadNotificationCount(ctx context.Context, userID int32) (int64, error) {
	var total int64
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM inbox_messages WHERE user_id = ? AND is_read = false", userID,
	).Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

// ==================== operation guards ====================

// publicAPIServiceOperationAllowed mirrors assertPublicServiceOperationAllowed.
// A non-nil result carries the exact status/code/message/details to reply with.
type publicAPIOperationDenial struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func publicAPIAssertServiceOperationAllowed(ctx context.Context, service *publicAPIServiceOperationRow, action string) (*publicAPIOperationDenial, error) {
	if service.Status == "deleted" {
		return &publicAPIOperationDenial{Status: http.StatusNotFound, Code: publicAPIServiceNotFound, Message: publicAPIServiceNotFoundMessage}, nil
	}
	if service.Status == "suspended" {
		code := publicAPIServiceSuspended
		if service.SuspendReason != nil && *service.SuspendReason == "expired" {
			code = publicAPIServiceSuspendedExp
		}
		return &publicAPIOperationDenial{Status: http.StatusForbidden, Code: code, Message: publicAPISuspendedOperationMessage}, nil
	}
	if action == "start" && service.Status == "running" {
		return &publicAPIOperationDenial{Status: http.StatusConflict, Code: publicAPIServiceAlreadyRunning, Message: publicAPIAlreadyRunningMessage}, nil
	}
	if action == "stop" && service.Status == "stopped" {
		return &publicAPIOperationDenial{Status: http.StatusConflict, Code: publicAPIServiceAlreadyStopped, Message: publicAPIAlreadyStoppedMessage}, nil
	}

	// The four lookups run concurrently in the TypeScript; the reply uses the
	// first non-empty one in this order, so evaluating them in order is equivalent
	// for a single response and avoids the Promise.all failure coupling.
	var transfer struct {
		ID     int32  `gorm:"column:id"`
		Status string `gorm:"column:status"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, status::text AS status FROM instance_transfers
		 WHERE instance_id = ? AND status IN ('pending','processing') LIMIT 1`, service.ID,
	).Scan(&transfer).Error; err != nil {
		return nil, err
	}
	if transfer.ID != 0 {
		return &publicAPIOperationDenial{
			Status: http.StatusConflict, Code: publicAPIServiceTransferLocked, Message: publicAPITransferLockedMessage,
			Details: map[string]any{"transferId": transfer.ID, "transferStatus": transfer.Status},
		}, nil
	}

	var restore struct {
		ID     int32  `gorm:"column:id"`
		Status string `gorm:"column:status"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, status::text AS status FROM restore_tasks
		 WHERE instance_id = ? AND status IN ('PENDING','PROCESSING') LIMIT 1`, service.ID,
	).Scan(&restore).Error; err != nil {
		return nil, err
	}
	if restore.ID != 0 {
		return &publicAPIOperationDenial{
			Status: http.StatusConflict, Code: publicAPIServiceRestoreRunning, Message: publicAPIRestoreRunningMessage,
			Details: map[string]any{"taskId": restore.ID, "status": restore.Status},
		}, nil
	}

	var upload struct {
		ID     int32  `gorm:"column:id"`
		Status string `gorm:"column:status"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, status::text AS status FROM backup_upload_tasks
		 WHERE instance_id = ? AND status IN ('PENDING','PROCESSING') LIMIT 1`, service.ID,
	).Scan(&upload).Error; err != nil {
		return nil, err
	}
	if upload.ID != 0 {
		return &publicAPIOperationDenial{
			Status: http.StatusConflict, Code: publicAPIServiceUploadRunning, Message: publicAPIUploadRunningMessage,
			Details: map[string]any{"taskId": upload.ID, "status": upload.Status},
		}, nil
	}

	var activeTask struct {
		ID       int32  `gorm:"column:id"`
		TaskType string `gorm:"column:task_type"`
		Status   string `gorm:"column:status"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, task_type::text AS task_type, status::text AS status FROM instance_tasks
		 WHERE instance_id = ? AND status IN ('PENDING','PROCESSING') LIMIT 1`, service.ID,
	).Scan(&activeTask).Error; err != nil {
		return nil, err
	}
	if activeTask.ID != 0 {
		return &publicAPIOperationDenial{
			Status: http.StatusConflict, Code: publicAPIServiceTaskRunning, Message: publicAPITaskRunningMessage,
			Details: map[string]any{
				"taskId": activeTask.ID, "taskType": activeTask.TaskType, "status": activeTask.Status,
			},
		}, nil
	}

	return nil, nil
}

// publicAPIAssertServiceRenewAllowed mirrors assertPublicServiceRenewAllowed.
func publicAPIAssertServiceRenewAllowed(service *publicAPIServiceRenewRow) *publicAPIOperationDenial {
	if service.PackagePlanID == nil {
		return &publicAPIOperationDenial{Status: http.StatusBadRequest, Code: publicAPIFreeService, Message: publicAPIFreeServiceMessage}
	}
	return nil
}

// ==================== renewal seam ====================

// PublicAPIRenewalResult mirrors the subset of performRenewal's result the public
// API serializes.
type PublicAPIRenewalResult struct {
	NewExpiresAt   time.Time
	Amount         float64
	DiscountAmount *float64
}

// PublicAPIPerformRenewal is the seam for db.performRenewal
// (server/src/db/billing-operations.ts:560). That function is owned by the billing
// wave and is not ported yet; until it lands this seam fails explicitly so the
// route answers 400 SERVICE_RENEW_FAILED instead of pretending to renew. The route
// keeps every pre-call guard (months, ownership, free-service) intact.
var PublicAPIPerformRenewal = func(ctx context.Context, userID int32, instanceID int32, months int) (PublicAPIRenewalResult, error) {
	return PublicAPIRenewalResult{}, fmt.Errorf("续费模块尚未移植（seam: db/billing-operations.ts#performRenewal）")
}

// ==================== inline rate limits ====================

// publicAPIRateRule mirrors one PUBLIC_API_RATE_LIMITS entry.
type publicAPIRateRule struct {
	Max    int
	Window time.Duration
}

// publicAPIRateLimits is copied verbatim from PUBLIC_API_RATE_LIMITS.
var publicAPIRateLimits = map[string]publicAPIRateRule{
	"POST /balance/adjustment-requests":  {Max: 5, Window: 10 * time.Minute},
	"POST /services/:id/actions":         {Max: 10, Window: time.Minute},
	"POST /services/:id/renew":           {Max: 5, Window: 10 * time.Minute},
	"GET /services/:id/tasks/:taskId":    {Max: 120, Window: time.Minute},
	"DELETE /services/:id/tasks/:taskId": {Max: 10, Window: time.Minute},
	"POST /tickets":                      {Max: 10, Window: 5 * time.Minute},
	"POST /tickets/:id/replies":          {Max: 20, Window: time.Minute},
	"PATCH /tickets/:id/status":          {Max: 20, Window: time.Minute},
	"POST /notifications":                {Max: 20, Window: time.Minute},
}

type publicAPIRateBucket struct {
	count   int
	resetAt time.Time
}

type publicAPIRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*publicAPIRateBucket
}

var publicAPIInlineLimiter = &publicAPIRateLimiter{buckets: map[string]*publicAPIRateBucket{}}

// reap drops expired buckets. @fastify/rate-limit's LocalStore does the same via
// its toad-cache TTL; without it the map would grow with every caller IP forever.
func (l *publicAPIRateLimiter) reap() {
	ticker := time.NewTicker(time.Minute)
	for range ticker.C {
		now := time.Now()
		l.mu.Lock()
		for key, bucket := range l.buckets {
			if now.After(bucket.resetAt) {
				delete(l.buckets, key)
			}
		}
		l.mu.Unlock()
	}
}

// allow records a hit for (route, ip) and reports whether it is within the quota.
func (l *publicAPIRateLimiter) allow(key string, max int, window time.Duration) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, ok := l.buckets[key]
	if !ok || now.After(bucket.resetAt) {
		l.buckets[key] = &publicAPIRateBucket{count: 1, resetAt: now.Add(window)}
		return true
	}
	bucket.count++
	return bucket.count <= max
}

// publicAPIRateLimit builds the per-route middleware for a PUBLIC_API_RATE_LIMITS
// entry. The key is route + IP, matching @fastify/rate-limit's child store per
// route keyed by request.ip.
func publicAPIRateLimit(route string) gin.HandlerFunc {
	rule := publicAPIRateLimits[route]
	return func(c *gin.Context) {
		key := middleware.ClientIP(c) + ":" + route
		if !publicAPIInlineLimiter.allow(key, rule.Max, rule.Window) {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "Too many requests, please try again later"})
			return
		}
		c.Next()
	}
}

// ==================== response views ====================

type publicAPIProfileView struct {
	ID            int32   `json:"id"`
	Username      string  `json:"username"`
	Email         *string `json:"email"`
	Role          string  `json:"role"`
	AvatarStyle   string  `json:"avatarStyle"`
	AvatarBadgeID *string `json:"avatarBadgeId"`
}

type publicAPITokenMetaView struct {
	TokenID int32                `json:"tokenId"`
	Scopes  []lib.PublicAPIScope `json:"scopes"`
}

type publicAPIProfileResponse struct {
	Data publicAPIProfileView   `json:"data"`
	Meta publicAPITokenMetaView `json:"meta"`
}

func publicAPIProfileFromToken(token *lib.AuthenticatedPublicAPIToken) publicAPIProfileView {
	return publicAPIProfileView{
		ID:            token.User.ID,
		Username:      token.User.Username,
		Email:         token.User.Email,
		Role:          token.User.Role,
		AvatarStyle:   token.User.AvatarStyle,
		AvatarBadgeID: token.User.AvatarBadgeID,
	}
}

func publicAPITokenMeta(token *lib.AuthenticatedPublicAPIToken) publicAPITokenMetaView {
	return publicAPITokenMetaView{TokenID: token.ID, Scopes: token.Scopes}
}

// publicAPIAdjustmentView mirrors serializePublicBalanceAdjustmentRequest.
type publicAPIAdjustmentView struct {
	ID           int32   `json:"id"`
	UserID       int32   `json:"userId"`
	Amount       float64 `json:"amount"`
	RequestType  string  `json:"requestType"`
	Status       string  `json:"status"`
	OrderNo      *string `json:"orderNo"`
	Reason       string  `json:"reason"`
	ReviewRemark *string `json:"reviewRemark"`
	CreatedAt    string  `json:"createdAt"`
	UpdatedAt    string  `json:"updatedAt"`
	ReviewedAt   *string `json:"reviewedAt"`
}

func publicAPIAdjustmentFromRow(row publicAPIAdjustmentRow) publicAPIAdjustmentView {
	return publicAPIAdjustmentView{
		ID:           row.ID,
		UserID:       row.UserID,
		Amount:       publicAPIMoney(row.Amount),
		RequestType:  string(row.RequestType),
		Status:       string(row.Status),
		OrderNo:      row.OrderNo,
		Reason:       row.Reason,
		ReviewRemark: row.ReviewRemark,
		CreatedAt:    publicAPIISO(row.CreatedAt),
		UpdatedAt:    publicAPIISO(row.UpdatedAt),
		ReviewedAt:   publicAPIISOPtr(row.ReviewedAt),
	}
}

type publicAPIProductLimitsView struct {
	CPUMax              int32   `json:"cpuMax"`
	MemoryMax           int32   `json:"memoryMax"`
	DiskMax             int32   `json:"diskMax"`
	BandwidthMax        *int32  `json:"bandwidthMax"`
	PortLimit           int32   `json:"portLimit"`
	SnapshotLimit       int32   `json:"snapshotLimit"`
	BackupLimit         int32   `json:"backupLimit"`
	SiteLimit           int32   `json:"siteLimit"`
	MonthlyTrafficLimit *string `json:"monthlyTrafficLimit"`
}

type publicAPIProductPlanView struct {
	ID                int32    `json:"id"`
	Name              string   `json:"name"`
	Description       *string  `json:"description"`
	CPU               int32    `json:"cpu"`
	Memory            int32    `json:"memory"`
	Disk              int32    `json:"disk"`
	Price             float64  `json:"price"`
	SetupFee          float64  `json:"setupFee"`
	BillingCycle      int32    `json:"billingCycle"`
	IsSoldOut         bool     `json:"isSoldOut"`
	TrafficLimit      string   `json:"trafficLimit"`
	TrafficLimitSpeed string   `json:"trafficLimitSpeed"`
	SlaGuarantee      *float64 `json:"slaGuarantee"`
}

type publicAPIProductView struct {
	ID           int32                      `json:"id"`
	Name         string                     `json:"name"`
	Description  *string                    `json:"description"`
	InstanceType string                     `json:"instanceType"`
	NetworkMode  string                     `json:"networkMode"`
	Active       bool                       `json:"active"`
	Limits       publicAPIProductLimitsView `json:"limits"`
	Plans        []publicAPIProductPlanView `json:"plans"`
}

func publicAPIProductFromRow(row publicAPIProductRow, plans []publicAPIPlanRow) publicAPIProductView {
	views := make([]publicAPIProductPlanView, 0, len(plans))
	for _, plan := range plans {
		views = append(views, publicAPIProductPlanView{
			ID:                plan.ID,
			Name:              plan.Name,
			Description:       plan.Description,
			CPU:               plan.CPU,
			Memory:            plan.Memory,
			Disk:              plan.Disk,
			Price:             publicAPIMoney(plan.Price),
			SetupFee:          publicAPIMoney(plan.SetupFee),
			BillingCycle:      plan.BillingCycle,
			IsSoldOut:         plan.IsSoldOut,
			TrafficLimit:      publicAPIBigIntStringRequired(plan.TrafficLimit),
			TrafficLimitSpeed: plan.TrafficLimitSpeed,
			SlaGuarantee:      publicAPIMoneyPtr(plan.SlaGuarantee),
		})
	}
	return publicAPIProductView{
		ID:           row.ID,
		Name:         row.Name,
		Description:  row.Description,
		InstanceType: row.InstanceType,
		NetworkMode:  row.NetworkMode,
		Active:       row.Active,
		Limits: publicAPIProductLimitsView{
			CPUMax:              row.CPUMax,
			MemoryMax:           row.MemoryMax,
			DiskMax:             row.DiskMax,
			BandwidthMax:        row.BandwidthMax,
			PortLimit:           row.PortLimit,
			SnapshotLimit:       row.SnapshotLimit,
			BackupLimit:         row.BackupLimit,
			SiteLimit:           row.SiteLimit,
			MonthlyTrafficLimit: publicAPIBigIntString(row.MonthlyTrafficLimit),
		},
		Plans: views,
	}
}

// publicAPIPlanSummaryView mirrors the `packagePlan: { select: { id, name,
// package: { select: { id, name } } } }` projection used by orders and billing
// records — no billingCycle/price there.
type publicAPIPlanSummaryView struct {
	ID      int32                            `json:"id"`
	Name    string                           `json:"name"`
	Package *publicAPIInstancePackageRefView `json:"package"`
}

type publicAPIInstancePackageRefView struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

func publicAPIPlanSummary(row publicAPIBillingRow) *publicAPIPlanSummaryView {
	if row.PlanID == nil || row.PlanName == nil {
		return nil
	}
	var pkg *publicAPIInstancePackageRefView
	if row.PackageID != nil && row.PackageName != nil {
		pkg = &publicAPIInstancePackageRefView{ID: *row.PackageID, Name: *row.PackageName}
	}
	return &publicAPIPlanSummaryView{ID: *row.PlanID, Name: *row.PlanName, Package: pkg}
}

type publicAPIInstanceRefView struct {
	ID     int32                     `json:"id"`
	Name   string                    `json:"name"`
	Status string                    `json:"status"`
	Plan   *publicAPIPlanSummaryView `json:"plan"`
}

func publicAPIInstanceRefFromBilling(row publicAPIBillingRow) *publicAPIInstanceRefView {
	if row.InstanceName == nil || row.InstanceStatus == nil {
		return nil
	}
	return &publicAPIInstanceRefView{
		ID:     row.InstanceID,
		Name:   *row.InstanceName,
		Status: *row.InstanceStatus,
		Plan:   publicAPIPlanSummary(row),
	}
}

type publicAPIRechargeOrderView struct {
	ID            string   `json:"id"`
	SourceType    string   `json:"sourceType"`
	SourceID      int32    `json:"sourceId"`
	OrderNo       string   `json:"orderNo"`
	Title         string   `json:"title"`
	Status        string   `json:"status"`
	RawStatus     string   `json:"rawStatus"`
	Amount        float64  `json:"amount"`
	ActualAmount  *float64 `json:"actualAmount"`
	Fee           float64  `json:"fee"`
	PaymentMethod *string  `json:"paymentMethod"`
	TradeNo       *string  `json:"tradeNo"`
	FailReason    *string  `json:"failReason"`
	CreatedAt     string   `json:"createdAt"`
	CompletedAt   *string  `json:"completedAt"`
	ExpiredAt     *string  `json:"expiredAt"`
}

type publicAPIBillingOrderView struct {
	ID            string                    `json:"id"`
	SourceType    string                    `json:"sourceType"`
	SourceID      int32                     `json:"sourceId"`
	OrderNo       string                    `json:"orderNo"`
	Title         string                    `json:"title"`
	Status        string                    `json:"status"`
	RawStatus     string                    `json:"rawStatus"`
	Amount        float64                   `json:"amount"`
	ActualAmount  float64                   `json:"actualAmount"`
	Fee           float64                   `json:"fee"`
	PaymentMethod string                    `json:"paymentMethod"`
	TradeNo       *string                   `json:"tradeNo"`
	FailReason    *string                   `json:"failReason"`
	Instance      *publicAPIInstanceRefView `json:"instance"`
	Months        int32                     `json:"months"`
	PeriodStart   string                    `json:"periodStart"`
	PeriodEnd     string                    `json:"periodEnd"`
	CreatedAt     string                    `json:"createdAt"`
	CompletedAt   *string                   `json:"completedAt"`
	ExpiredAt     *string                   `json:"expiredAt"`
}

// publicAPIOrderEntry pairs a view with the (createdAt, id) sort key the merged
// list is ordered by.
type publicAPIOrderEntry struct {
	createdAt string
	id        string
	view      any
}

func publicAPIMapRechargeStatus(status models.RechargeStatus) string {
	switch string(status) {
	case "completed":
		return "completed"
	case "refunded":
		return "refunded"
	case "failed":
		return "failed"
	case "cancelled":
		return "cancelled"
	default:
		return "pending"
	}
}

func publicAPIMapBillingStatus(recordType models.BillingRecordType) string {
	if recordType == models.BillingRecordTypeRefund {
		return "refunded"
	}
	return "completed"
}

func publicAPIRechargeOrderFromRow(row publicAPIRechargeRow) publicAPIRechargeOrderView {
	return publicAPIRechargeOrderView{
		ID:            fmt.Sprintf("recharge:%d", row.ID),
		SourceType:    "recharge",
		SourceID:      row.ID,
		OrderNo:       row.OrderNo,
		Title:         "余额充值",
		Status:        publicAPIMapRechargeStatus(row.Status),
		RawStatus:     string(row.Status),
		Amount:        publicAPIMoney(row.Amount),
		ActualAmount:  publicAPIMoneyPtr(row.ActualAmount),
		Fee:           publicAPIMoney(row.Fee),
		PaymentMethod: row.PaymentMethod,
		TradeNo:       publicAPIMaskTradeNo(row.TradeNo),
		FailReason:    row.FailReason,
		CreatedAt:     publicAPIISO(row.CreatedAt),
		CompletedAt:   publicAPIISOPtr(row.CompletedAt),
		ExpiredAt:     publicAPIISOPtr(row.ExpiredAt),
	}
}

func publicAPIBillingOrderFromRow(row publicAPIBillingRow) publicAPIBillingOrderView {
	instance := publicAPIInstanceRefFromBilling(row)
	title := "实例计费"
	if row.Remark != nil && *row.Remark != "" {
		title = *row.Remark
	}
	amount := publicAPIMoney(row.Amount)
	createdAt := publicAPIISO(row.CreatedAt)
	return publicAPIBillingOrderView{
		ID:            fmt.Sprintf("instance_billing:%d", row.ID),
		SourceType:    "instance_billing",
		SourceID:      row.ID,
		OrderNo:       fmt.Sprintf("IB-%d", row.ID),
		Title:         title,
		Status:        publicAPIMapBillingStatus(row.Type),
		RawStatus:     string(row.Type),
		Amount:        amount,
		ActualAmount:  amount,
		Fee:           0,
		PaymentMethod: "balance",
		TradeNo:       nil,
		FailReason:    nil,
		Instance:      instance,
		Months:        row.Months,
		PeriodStart:   publicAPIISO(row.PeriodStart),
		PeriodEnd:     publicAPIISO(row.PeriodEnd),
		CreatedAt:     createdAt,
		CompletedAt:   &createdAt,
		ExpiredAt:     nil,
	}
}

type publicAPIBillingRecordView struct {
	ID          int32                     `json:"id"`
	OrderID     string                    `json:"orderId"`
	ServiceID   int32                     `json:"serviceId"`
	Type        string                    `json:"type"`
	Status      string                    `json:"status"`
	Amount      float64                   `json:"amount"`
	Months      int32                     `json:"months"`
	PeriodStart string                    `json:"periodStart"`
	PeriodEnd   string                    `json:"periodEnd"`
	Remark      *string                   `json:"remark"`
	Service     *publicAPIInstanceRefView `json:"service"`
	CreatedAt   string                    `json:"createdAt"`
}

func publicAPIBillingRecordFromRow(row publicAPIBillingRow) publicAPIBillingRecordView {
	service := publicAPIInstanceRefFromBilling(row)
	return publicAPIBillingRecordView{
		ID:          row.ID,
		OrderID:     fmt.Sprintf("instance_billing:%d", row.ID),
		ServiceID:   row.InstanceID,
		Type:        string(row.Type),
		Status:      publicAPIMapBillingStatus(row.Type),
		Amount:      publicAPIMoney(row.Amount),
		Months:      row.Months,
		PeriodStart: publicAPIISO(row.PeriodStart),
		PeriodEnd:   publicAPIISO(row.PeriodEnd),
		Remark:      row.Remark,
		Service:     service,
		CreatedAt:   publicAPIISO(row.CreatedAt),
	}
}

type publicAPIServiceResourcesView struct {
	CPU    int32 `json:"cpu"`
	Memory int32 `json:"memory"`
	Disk   int32 `json:"disk"`
}

type publicAPIServiceNetworkView struct {
	Mode string  `json:"mode"`
	IPv4 *string `json:"ipv4"`
	IPv6 *string `json:"ipv6"`
}

type publicAPIServiceLimitsView struct {
	Ports               *int32  `json:"ports"`
	Snapshots           *int32  `json:"snapshots"`
	Backups             *int32  `json:"backups"`
	Sites               *int32  `json:"sites"`
	MonthlyTrafficLimit *string `json:"monthlyTrafficLimit"`
	MonthlyTrafficUsed  string  `json:"monthlyTrafficUsed"`
	TrafficStatus       string  `json:"trafficStatus"`
}

type publicAPIServicePlanView struct {
	ID           int32                            `json:"id"`
	Name         string                           `json:"name"`
	BillingCycle int32                            `json:"billingCycle"`
	Price        float64                          `json:"price"`
	Package      *publicAPIInstancePackageRefView `json:"package"`
}

type publicAPIServiceBillingView struct {
	ExpiresAt     *string                   `json:"expiresAt"`
	SuspendedAt   *string                   `json:"suspendedAt"`
	SuspendReason *string                   `json:"suspendReason"`
	BillingPrice  *float64                  `json:"billingPrice"`
	BillingCycle  *int32                    `json:"billingCycle"`
	AutoRenew     bool                      `json:"autoRenew"`
	Plan          *publicAPIServicePlanView `json:"plan"`
}

type publicAPIServiceView struct {
	ID        int32                         `json:"id"`
	Name      string                        `json:"name"`
	Status    string                        `json:"status"`
	Image     string                        `json:"image"`
	Resources publicAPIServiceResourcesView `json:"resources"`
	Network   publicAPIServiceNetworkView   `json:"network"`
	Limits    publicAPIServiceLimitsView    `json:"limits"`
	Billing   publicAPIServiceBillingView   `json:"billing"`
	CreatedAt string                        `json:"createdAt"`
	UpdatedAt string                        `json:"updatedAt"`
}

func publicAPIServiceFromRow(row publicAPIServiceRow, plan *publicAPIPlanRefRow) publicAPIServiceView {
	var planView *publicAPIServicePlanView
	if plan != nil && plan.ID != 0 {
		var pkg *publicAPIInstancePackageRefView
		if plan.PackageID != nil && plan.PackageName != nil {
			pkg = &publicAPIInstancePackageRefView{ID: *plan.PackageID, Name: *plan.PackageName}
		}
		planView = &publicAPIServicePlanView{
			ID:           plan.ID,
			Name:         plan.Name,
			BillingCycle: plan.BillingCycle,
			Price:        publicAPIMoney(plan.Price),
			Package:      pkg,
		}
	}
	return publicAPIServiceView{
		ID:     row.ID,
		Name:   row.Name,
		Status: row.Status,
		Image:  row.Image,
		Resources: publicAPIServiceResourcesView{
			CPU: row.CPU, Memory: row.Memory, Disk: row.Disk,
		},
		Network: publicAPIServiceNetworkView{Mode: row.NetworkMode, IPv4: row.IPv4, IPv6: row.IPv6},
		Limits: publicAPIServiceLimitsView{
			Ports:               row.PortLimit,
			Snapshots:           row.SnapshotLimit,
			Backups:             row.BackupLimit,
			Sites:               row.SiteLimit,
			MonthlyTrafficLimit: publicAPIBigIntString(row.MonthlyTrafficLimit),
			MonthlyTrafficUsed:  publicAPIBigIntStringRequired(row.MonthlyTrafficUsed),
			TrafficStatus:       row.TrafficStatus,
		},
		Billing: publicAPIServiceBillingView{
			ExpiresAt:     publicAPIISOPtr(row.ExpiresAt),
			SuspendedAt:   publicAPIISOPtr(row.SuspendedAt),
			SuspendReason: row.SuspendReason,
			BillingPrice:  publicAPIMoneyPtr(row.BillingPrice),
			BillingCycle:  row.BillingCycle,
			AutoRenew:     row.AutoRenew,
			Plan:          planView,
		},
		CreatedAt: publicAPIISO(row.CreatedAt),
		UpdatedAt: publicAPIISO(row.UpdatedAt),
	}
}

type publicAPIIncludedProductView struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

type publicAPIIncludedPlanView struct {
	ID           int32   `json:"id"`
	Name         string  `json:"name"`
	BillingCycle *int32  `json:"billingCycle"`
	Price        float64 `json:"price"`
	ProductID    *int32  `json:"productId"`
}

type publicAPIServiceIncludedView struct {
	// Pointers keep `products: []` / `plans: []` on the wire: the TypeScript sets
	// the property to an empty array (not undefined), and Object.keys(included)
	// then still contains it.
	Products *[]publicAPIIncludedProductView `json:"products,omitempty"`
	Plans    *[]publicAPIIncludedPlanView    `json:"plans,omitempty"`
}

// publicAPIBuildServiceIncluded mirrors buildPublicServiceIncluded: dedup by id in
// first-seen order, and only the requested keys are present.
func publicAPIBuildServiceIncluded(rows []publicAPIServiceRow, planRefs map[int32]publicAPIPlanRefRow, includes []string) *publicAPIServiceIncludedView {
	includeProduct := false
	includePlan := false
	for _, include := range includes {
		if include == "product" {
			includeProduct = true
		}
		if include == "plan" {
			includePlan = true
		}
	}
	if !includeProduct && !includePlan {
		return nil
	}

	view := &publicAPIServiceIncludedView{}
	if includeProduct {
		products := make([]publicAPIIncludedProductView, 0)
		seen := map[int32]bool{}
		for _, row := range rows {
			if row.PackagePlanID == nil {
				continue
			}
			plan, ok := planRefs[*row.PackagePlanID]
			if !ok || plan.PackageID == nil || plan.PackageName == nil || seen[*plan.PackageID] {
				continue
			}
			seen[*plan.PackageID] = true
			products = append(products, publicAPIIncludedProductView{ID: *plan.PackageID, Name: *plan.PackageName})
		}
		view.Products = &products
	}
	if includePlan {
		plans := make([]publicAPIIncludedPlanView, 0)
		seen := map[int32]bool{}
		for _, row := range rows {
			if row.PackagePlanID == nil {
				continue
			}
			plan, ok := planRefs[*row.PackagePlanID]
			if !ok || seen[plan.ID] {
				continue
			}
			seen[plan.ID] = true
			plans = append(plans, publicAPIIncludedPlanView{
				ID:           plan.ID,
				Name:         plan.Name,
				BillingCycle: &plan.BillingCycle,
				Price:        publicAPIMoney(plan.Price),
				ProductID:    plan.PackageID,
			})
		}
		view.Plans = &plans
	}
	return view
}

type publicAPIAdjustmentViewResponse struct {
	Data publicAPIAdjustmentView `json:"data"`
}

type publicAPIProductResponse struct {
	Data publicAPIProductView `json:"data"`
}

type publicAPIServiceResponse struct {
	Data     publicAPIServiceView          `json:"data"`
	Included *publicAPIServiceIncludedView `json:"included,omitempty"`
}

// ==================== shared handler helpers ====================

// publicAPILog writes one audit line. Failure is swallowed by db.CreateLog, like
// the TypeScript `createLog(...)` (it catches internally).
func publicAPILog(c *gin.Context, userID int32, module, action, content string) {
	id := int64(userID)
	db.CreateLog(c.Request.Context(), &id, module, action, content, db.LogResultSuccess, db.CreateLogOptions{})
}

func publicAPILogResult(c *gin.Context, userID int32, module, action, content, result string) {
	id := int64(userID)
	db.CreateLog(c.Request.Context(), &id, module, action, content, result, db.CreateLogOptions{})
}

func publicAPILogWithInstance(c *gin.Context, userID, instanceID int32, module, action, content string) {
	id := int64(userID)
	instance := instanceID
	db.CreateLog(c.Request.Context(), &id, module, action, content, db.LogResultSuccess, db.LogOptionsWithInstance(&instance))
}

// publicAPIValidateBody runs Fastify's body parser before authentication, which is
// where the Node pipeline puts it for this module (auth happens inside handlers).
func publicAPIValidateBody(c *gin.Context) bool {
	if status, message := middleware.ValidateFastifyBody(c); status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return false
	}
	return true
}

// publicAPIJSTruthy mirrors JavaScript truthiness for the `body?.template ?` check.
func publicAPIJSTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0 && !math.IsNaN(typed)
	default:
		return true
	}
}

// ==================== openapi handlers ====================

func publicAPIOpenAPIJSONHandler(c *gin.Context) {
	c.Data(http.StatusOK, "application/json; charset=utf-8", lib.PublicAPIOpenAPIJSON())
}

func publicAPIOpenAPIYAMLHandler(c *gin.Context) {
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", lib.PublicAPIOpenAPIYAML())
}

// ==================== profile ====================

// publicAPIMeReadHandler mirrors GET /me.
func publicAPIMeReadHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeProfileRead)
	if token == nil {
		return
	}

	publicAPILog(c, token.UserID, db.LogModuleAuth, "public_api.me_read",
		fmt.Sprintf(`Public API token "%s" read profile through /api/v1/me`, token.Name))

	c.JSON(http.StatusOK, publicAPIProfileResponse{
		Data: publicAPIProfileFromToken(token),
		Meta: publicAPITokenMeta(token),
	})
}

// publicAPIMeUpdateHandler mirrors PATCH /me.
func publicAPIMeUpdateHandler(c *gin.Context) {
	if !publicAPIValidateBody(c) {
		return
	}
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeProfileWrite)
	if token == nil {
		return
	}

	avatarStyleValue, _ := publicAPIBodyField(publicAPIReadBody(c), "avatarStyle")
	avatarStyle, ok := publicAPINormalizeAvatarStyle(avatarStyleValue)
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidProfileUpdateCode, publicAPIInvalidProfileUpdateMsg)
		return
	}

	profile, err := publicAPIUpdateAvatarStyle(c.Request.Context(), token.UserID, avatarStyle)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if profile == nil {
		// prisma.user.update throws P2025 when the row vanished between the token
		// lookup and the update; app.ts answers 500 for it.
		publicAPIRespondInternal(c)
		return
	}

	publicAPILog(c, token.UserID, db.LogModuleAuth, "public_api.me_update",
		fmt.Sprintf("Public API %s updated current user profile through /api/v1/me", token.Source))

	c.JSON(http.StatusOK, publicAPIProfileResponse{Data: *profile, Meta: publicAPITokenMeta(token)})
}

// ==================== balance ====================

// publicAPIBalanceReadHandler mirrors GET /balance.
func publicAPIBalanceReadHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeBalanceRead)
	if token == nil {
		return
	}

	user, err := publicAPILoadUserBalance(c.Request.Context(), token.UserID)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if user == nil {
		publicAPIRespondError(c, http.StatusNotFound, publicAPIUserNotFoundCode, publicAPIUserNotFoundMessage)
		return
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.balance_read",
		fmt.Sprintf("Public API %s read current user balance", token.Source))

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"userId":    user.ID,
		"balance":   publicAPIMoney(user.Balance),
		"currency":  "CNY",
		"updatedAt": publicAPIISO(user.UpdatedAt),
	}})
}

type publicAPIInstanceNameView struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

type publicAPIBalanceLogView struct {
	ID            int32                      `json:"id"`
	Type          string                     `json:"type"`
	Amount        float64                    `json:"amount"`
	BalanceBefore float64                    `json:"balanceBefore"`
	BalanceAfter  float64                    `json:"balanceAfter"`
	OrderID       *string                    `json:"orderId"`
	Instance      *publicAPIInstanceNameView `json:"instance"`
	Remark        *string                    `json:"remark"`
	CreatedAt     string                     `json:"createdAt"`
}

// publicAPIBalanceLogsHandler mirrors GET /balance/logs.
func publicAPIBalanceLogsHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeBalanceRead)
	if token == nil {
		return
	}

	pagination := publicAPIParsePagination(func(key string) publicAPIQueryParam { return publicAPIQuery(c, key) })
	sortSpec := publicAPIParseSort(publicAPIQuery(c, "sort"), publicAPICreatedAtSorts, "-createdAt")
	if sortSpec == nil {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidSortCode, publicAPIInvalidSortMessage)
		return
	}

	logType := ""
	if raw, ok := publicAPIQuery(c, "type").asString(); ok && publicAPIBalanceLogTypes[raw] {
		logType = raw
	}
	lotteryGift := ""
	if raw, ok := publicAPIQuery(c, "lotteryGift").asString(); ok && (raw == "exclude" || raw == "only") {
		lotteryGift = raw
	}

	where, args := publicAPIBalanceLogWhere(token.UserID, logType, lotteryGift)
	total, err := publicAPICountBalanceLogs(c.Request.Context(), where, args)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	logs, err := publicAPIListBalanceLogs(c.Request.Context(), where, args, publicAPIOrderBy(sortSpec), int64(pagination.take), pagination.skip)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	instanceIDs := make([]int32, 0, len(logs))
	seenInstance := map[int32]bool{}
	for _, row := range logs {
		if row.InstanceID == nil || seenInstance[*row.InstanceID] {
			continue
		}
		seenInstance[*row.InstanceID] = true
		instanceIDs = append(instanceIDs, *row.InstanceID)
	}
	instanceNames, err := publicAPIInstanceNameMap(c.Request.Context(), instanceIDs, token.UserID)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.balance_logs_list",
		fmt.Sprintf("Public API %s listed current user balance logs", token.Source))

	data := make([]publicAPIBalanceLogView, 0, len(logs))
	for _, row := range logs {
		var instance *publicAPIInstanceNameView
		if row.InstanceID != nil && *row.InstanceID != 0 {
			if name, ok := instanceNames[*row.InstanceID]; ok {
				instance = &publicAPIInstanceNameView{ID: *row.InstanceID, Name: name}
			}
		}
		data = append(data, publicAPIBalanceLogView{
			ID:            row.ID,
			Type:          string(row.Type),
			Amount:        publicAPIMoney(row.Amount),
			BalanceBefore: publicAPIMoney(row.BalanceBefore),
			BalanceAfter:  publicAPIMoney(row.BalanceAfter),
			OrderID:       row.OrderID,
			Instance:      instance,
			Remark:        row.Remark,
			CreatedAt:     publicAPIISO(row.CreatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": publicAPIPaginationMetaFor(pagination.page, pagination.pageSize, total, sortSpec.value),
	})
}

// publicAPIAdjustmentsListHandler mirrors GET /balance/adjustment-requests.
func publicAPIAdjustmentsListHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeBalanceWrite)
	if token == nil {
		return
	}

	pagination := publicAPIParsePagination(func(key string) publicAPIQueryParam { return publicAPIQuery(c, key) })
	sortSpec := publicAPIParseSort(publicAPIQuery(c, "sort"), publicAPICreatedAtSorts, "-createdAt")
	if sortSpec == nil {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidSortCode, publicAPIInvalidSortMessage)
		return
	}

	defined, status, invalid := publicAPIEnumQuery(c, "status", publicAPIAdjustmentStatuses)
	if invalid {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidAdjustmentStatus, publicAPIInvalidAdjustmentStatusMsg)
		return
	}
	if !defined {
		status = ""
	}

	where, args := publicAPIAdjustmentWhere(token.UserID, status)
	total, err := publicAPICountAdjustments(c.Request.Context(), where, args)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	rows, err := publicAPIListAdjustments(c.Request.Context(), where, args, publicAPIOrderBy(sortSpec), int64(pagination.take), pagination.skip)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.balance_adjustment_requests_list",
		fmt.Sprintf("Public API %s listed own balance adjustment requests", token.Source))

	data := make([]publicAPIAdjustmentView, 0, len(rows))
	for _, row := range rows {
		data = append(data, publicAPIAdjustmentFromRow(row))
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": publicAPIPaginationMetaFor(pagination.page, pagination.pageSize, total, sortSpec.value),
	})
}

// publicAPIEnumQuery mirrors normalizeOptionalEnum(request.query?.key, allowed):
// absent/empty → undefined, a single member → defined, a repeated parameter
// (array in Fastify) or an unknown value → invalid (400).
func publicAPIEnumQuery(c *gin.Context, key string, allowed map[string]bool) (defined bool, value string, invalid bool) {
	param := publicAPIQuery(c, key)
	if param.repeated {
		return false, "", true
	}
	if !param.single || param.value == "" {
		return false, "", false
	}
	trimmed := publicAPIJSTrim(param.value)
	if !allowed[trimmed] {
		return false, "", true
	}
	return true, trimmed, false
}

// publicAPINormalizeAdjustmentAmount mirrors normalizePublicBalanceAdjustmentAmount.
func publicAPINormalizeAdjustmentAmount(value any) *float64 {
	var amount float64
	switch typed := value.(type) {
	case float64:
		amount = typed
	case string:
		parsed, ok := ticketsJSToNumber(publicAPIJSTrim(typed))
		if !ok {
			return nil
		}
		amount = parsed
	default:
		return nil
	}
	if math.IsNaN(amount) || math.IsInf(amount, 0) {
		return nil
	}
	normalized := lib.JSFixed(amount, 2)
	if normalized <= 0 || normalized > publicAPIMaxAdjustmentAmount {
		return nil
	}
	return &normalized
}

// publicAPINormalizeAdjustmentReason mirrors normalizePublicBalanceAdjustmentReason.
func publicAPINormalizeAdjustmentReason(value any) *string {
	text, ok := publicAPIStringOf(value)
	if !ok {
		return nil
	}
	reason := publicAPIJSCollapseWhitespace(publicAPIJSTrim(text))
	length := jsStringLength(reason)
	if length < publicAPIMinAdjustmentReasonLength || length > publicAPIMaxAdjustmentReasonLength {
		return nil
	}
	return &reason
}

// publicAPINormalizeAdjustmentType mirrors normalizePublicBalanceAdjustmentType.
func publicAPINormalizeAdjustmentType(value any) models.BalanceAdjustmentRequestType {
	if text, ok := publicAPIStringOf(value); ok && publicAPIAdjustmentTypes[text] {
		return models.BalanceAdjustmentRequestType(text)
	}
	return models.BalanceAdjustmentRequestTypeManualAdjust
}

// publicAPINormalizeAdjustmentOrderNo mirrors normalizePublicBalanceAdjustmentOrderNo.
// nil means invalid → 400.
func publicAPINormalizeAdjustmentOrderNo(value any) **string {
	if value == nil {
		return nil // undefined
	}
	if text, ok := publicAPIStringOf(value); ok && text == "" {
		return nil
	}
	text, ok := publicAPIStringOf(value)
	if !ok {
		invalid := (*string)(nil)
		return &invalid
	}
	trimmed := publicAPIJSTrim(text)
	if !publicAPIAdjustmentOrderNoPattern.MatchString(trimmed) {
		invalid := (*string)(nil)
		return &invalid
	}
	valid := &trimmed
	return &valid
}

// publicAPICreateAdjustmentHandler mirrors POST /balance/adjustment-requests.
func publicAPICreateAdjustmentHandler(c *gin.Context) {
	if !publicAPIValidateBody(c) {
		return
	}
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeBalanceWrite)
	if token == nil {
		return
	}

	body := publicAPIReadBody(c)
	amountValue, _ := publicAPIBodyField(body, "amount")
	reasonValue, _ := publicAPIBodyField(body, "reason")
	requestTypeValue, _ := publicAPIBodyField(body, "requestType")
	orderNoValue, _ := publicAPIBodyField(body, "orderNo")

	amount := publicAPINormalizeAdjustmentAmount(amountValue)
	reason := publicAPINormalizeAdjustmentReason(reasonValue)
	requestType := publicAPINormalizeAdjustmentType(requestTypeValue)
	orderNoPtr := publicAPINormalizeAdjustmentOrderNo(orderNoValue)
	if amount == nil {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidAdjustmentAmount, publicAPIInvalidAdjustmentAmountMsg)
		return
	}
	if reason == nil {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidAdjustmentReason, publicAPIInvalidAdjustmentReasonMsg)
		return
	}
	if orderNoPtr != nil && *orderNoPtr == nil {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidAdjustmentOrderNo, publicAPIInvalidAdjustmentOrderMsg)
		return
	}
	var orderNo *string
	if orderNoPtr != nil {
		orderNo = *orderNoPtr
	}

	pendingWhere, pendingArgs := publicAPIAdjustmentWhere(token.UserID, "pending")
	pendingCount, err := publicAPICountAdjustments(c.Request.Context(), pendingWhere, pendingArgs)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if pendingCount >= publicAPIPendingAdjustmentLimit {
		publicAPIRespondError(c, http.StatusTooManyRequests, publicAPIPendingAdjustmentLimitCode, publicAPIPendingAdjustmentLimitMsg)
		return
	}

	sourceType := "public_api"
	sourceID := token.ID
	created, err := db.CreateBalanceAdjustmentRequest(c.Request.Context(), db.BalanceAdjustmentRequestInput{
		UserID:            token.UserID,
		RequestedByUserID: token.UserID,
		Amount:            balanceDecimalFromFloat(*amount),
		RequestType:       requestType,
		Reason:            "[Public API] " + *reason,
		SourceType:        &sourceType,
		SourceID:          &sourceID,
		OrderNo:           orderNo,
	})
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.balance_adjustment_request_create",
		fmt.Sprintf("Public API %s created pending balance adjustment request #%d", token.Source, created.ID))

	c.JSON(http.StatusAccepted, publicAPIAdjustmentViewResponse{Data: publicAPIAdjustmentFromModel(created)})
}

// publicAPIAdjustmentFromModel adapts db.CreateBalanceAdjustmentRequest's return
// value (which carries loaded relations) to the public view.
func publicAPIAdjustmentFromModel(request db.BalanceAdjustmentRequestWithRelations) publicAPIAdjustmentView {
	model := request.BalanceAdjustmentRequest
	return publicAPIAdjustmentView{
		ID:           model.ID,
		UserID:       model.UserID,
		Amount:       publicAPIMoney(model.Amount),
		RequestType:  string(model.RequestType),
		Status:       string(model.Status),
		OrderNo:      model.OrderNo,
		Reason:       model.Reason,
		ReviewRemark: model.ReviewRemark,
		CreatedAt:    publicAPIISO(model.CreatedAt),
		UpdatedAt:    publicAPIISO(model.UpdatedAt),
		ReviewedAt:   publicAPIISOPtr(model.ReviewedAt),
	}
}

// ==================== products ====================

// publicAPIProductsListHandler mirrors GET /products.
func publicAPIProductsListHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeProductsRead)
	if token == nil {
		return
	}

	pagination := publicAPIParsePagination(func(key string) publicAPIQueryParam { return publicAPIQuery(c, key) })
	sortSpec := publicAPIParseSort(publicAPIQuery(c, "sort"), publicAPICreatedAtSorts, "-createdAt")
	if sortSpec == nil {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidSortCode, publicAPIInvalidSortMessage)
		return
	}

	total, err := publicAPICountProducts(c.Request.Context())
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	rows, err := publicAPIListProducts(c.Request.Context(), publicAPIOrderBy(sortSpec), int64(pagination.take), pagination.skip)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	packageIDs := make([]int32, 0, len(rows))
	for _, row := range rows {
		packageIDs = append(packageIDs, row.ID)
	}
	plans, err := publicAPIListPlansForPackages(c.Request.Context(), packageIDs)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.products_list",
		fmt.Sprintf("Public API %s listed products", token.Source))

	data := make([]publicAPIProductView, 0, len(rows))
	for _, row := range rows {
		data = append(data, publicAPIProductFromRow(row, plans[row.ID]))
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": publicAPIPaginationMetaFor(pagination.page, pagination.pageSize, total, sortSpec.value),
	})
}

// publicAPIProductReadHandler mirrors GET /products/:id.
func publicAPIProductReadHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeProductsRead)
	if token == nil {
		return
	}

	id, ok := publicAPIParsePositiveID(c.Param("id"))
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidProductID, publicAPIInvalidProductIDMessage)
		return
	}
	row, err := publicAPIGetProduct(c.Request.Context(), id)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if row == nil {
		publicAPIRespondError(c, http.StatusNotFound, publicAPIProductNotFound, publicAPIProductNotFoundMessage)
		return
	}
	plans, err := publicAPIListPlanRefsForPackage(c.Request.Context(), id)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.product_read",
		fmt.Sprintf("Public API %s read product #%d", token.Source, id))

	c.JSON(http.StatusOK, publicAPIProductResponse{Data: publicAPIProductFromRow(*row, plans)})
}

// ==================== orders ====================

// publicAPIOrdersListHandler mirrors GET /orders.
func publicAPIOrdersListHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeOrdersRead)
	if token == nil {
		return
	}

	pagination := publicAPIParsePagination(func(key string) publicAPIQueryParam { return publicAPIQuery(c, key) })
	sortSpec := publicAPIParseSort(publicAPIQuery(c, "sort"), publicAPICreatedAtSorts, "-createdAt")
	if sortSpec == nil {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidSortCode, publicAPIInvalidSortMessage)
		return
	}

	definedStatus, status, invalidStatus := publicAPIEnumQuery(c, "status", publicAPIOrderStatuses)
	if invalidStatus {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidOrderStatus, publicAPIInvalidOrderStatusMessage)
		return
	}
	if !definedStatus {
		status = ""
	}

	rechargeStatuses, billingTypes, statusFiltered := publicAPIOrderStatusFilters(status)
	rechargeWhere, rechargeArgs := publicAPIRechargeWhere(token.UserID, rechargeStatuses)
	billingWhere, billingArgs := publicAPIBillingWhere(token.UserID, billingTypes, nil)

	// `status: null` means "no filter"; an empty list means "this source cannot
	// match this status" and contributes no rows (and no count).
	includeRecharges := !statusFiltered || len(rechargeStatuses) > 0
	includeBillings := !statusFiltered || len(billingTypes) > 0

	sourceTake := pagination.skip + int64(pagination.take)
	if sourceTake > publicAPIOrderSourceTakeCap {
		sourceTake = publicAPIOrderSourceTakeCap
	}

	var rechargeTotal, billingTotal int64
	if includeRecharges {
		total, err := publicAPICountRecharges(c.Request.Context(), rechargeWhere, rechargeArgs)
		if err != nil {
			publicAPIRespondInternal(c)
			return
		}
		rechargeTotal = total
	}
	if includeBillings {
		total, err := publicAPICountBillings(c.Request.Context(), billingWhere, billingArgs)
		if err != nil {
			publicAPIRespondInternal(c)
			return
		}
		billingTotal = total
	}

	var recharges []publicAPIRechargeRow
	if includeRecharges {
		rows, err := publicAPIListRecharges(c.Request.Context(), rechargeWhere, rechargeArgs,
			publicAPIOrderBy(sortSpec), sourceTake)
		if err != nil {
			publicAPIRespondInternal(c)
			return
		}
		recharges = rows
	}
	var billings []publicAPIBillingRow
	if includeBillings {
		rows, err := publicAPIListBillings(c.Request.Context(), billingWhere, billingArgs,
			publicAPIOrderBy(sortSpec), sourceTake, 0)
		if err != nil {
			publicAPIRespondInternal(c)
			return
		}
		billings = rows
	}

	entries := make([]publicAPIOrderEntry, 0, len(recharges)+len(billings))
	for _, row := range recharges {
		view := publicAPIRechargeOrderFromRow(row)
		entries = append(entries, publicAPIOrderEntry{createdAt: view.CreatedAt, id: view.ID, view: view})
	}
	for _, row := range billings {
		view := publicAPIBillingOrderFromRow(row)
		entries = append(entries, publicAPIOrderEntry{createdAt: view.CreatedAt, id: view.ID, view: view})
	}

	multiplier := 1
	if sortSpec.direction == "desc" {
		multiplier = -1
	}
	sort.SliceStable(entries, func(i, j int) bool {
		createdCompare := strings.Compare(entries[i].createdAt, entries[j].createdAt) * multiplier
		if createdCompare != 0 {
			return createdCompare < 0
		}
		return strings.Compare(entries[i].id, entries[j].id)*multiplier < 0
	})

	start := pagination.skip
	if start > int64(len(entries)) {
		start = int64(len(entries))
	}
	end := start + int64(pagination.take)
	if end > int64(len(entries)) {
		end = int64(len(entries))
	}
	data := make([]any, 0, end-start)
	for _, entry := range entries[start:end] {
		data = append(data, entry.view)
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.orders_list",
		fmt.Sprintf("Public API %s listed own orders", token.Source))

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": publicAPIPaginationMetaFor(pagination.page, pagination.pageSize, rechargeTotal+billingTotal, sortSpec.value),
	})
}

// publicAPIParseOrderID mirrors parsePublicOrderId.
func publicAPIParseOrderID(value string) (string, int64, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return "", 0, false
	}
	sourceType := parts[0]
	if sourceType != "recharge" && sourceType != "instance_billing" {
		return "", 0, false
	}
	sourceID, ok := publicAPIParsePositiveID(parts[1])
	if !ok {
		return "", 0, false
	}
	return sourceType, sourceID, true
}

// publicAPIOrderReadHandler mirrors GET /orders/:id.
func publicAPIOrderReadHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeOrdersRead)
	if token == nil {
		return
	}

	sourceType, sourceID, ok := publicAPIParseOrderID(c.Param("id"))
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidOrderID, publicAPIInvalidOrderIDMessage)
		return
	}

	if sourceType == "recharge" {
		row, err := publicAPIGetRecharge(c.Request.Context(), sourceID, token.UserID)
		if err != nil {
			publicAPIRespondInternal(c)
			return
		}
		if row == nil {
			publicAPIRespondError(c, http.StatusNotFound, publicAPIOrderNotFound, publicAPIOrderNotFoundMessage)
			return
		}
		publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.order_read",
			fmt.Sprintf("Public API %s read own recharge order #%d", token.Source, row.ID))
		c.JSON(http.StatusOK, gin.H{"data": publicAPIRechargeOrderFromRow(*row)})
		return
	}

	row, err := publicAPIGetBilling(c.Request.Context(), sourceID, token.UserID)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if row == nil {
		publicAPIRespondError(c, http.StatusNotFound, publicAPIOrderNotFound, publicAPIOrderNotFoundMessage)
		return
	}
	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.order_read",
		fmt.Sprintf("Public API %s read own billing order #%d", token.Source, row.ID))
	c.JSON(http.StatusOK, gin.H{"data": publicAPIBillingOrderFromRow(*row)})
}

// ==================== billing records ====================

// publicAPIBillingRecordsListHandler mirrors GET /billing-records.
func publicAPIBillingRecordsListHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeBillingRead)
	if token == nil {
		return
	}

	pagination := publicAPIParsePagination(func(key string) publicAPIQueryParam { return publicAPIQuery(c, key) })
	sortSpec := publicAPIParseSort(publicAPIQuery(c, "sort"), publicAPICreatedAtSorts, "-createdAt")
	if sortSpec == nil {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidSortCode, publicAPIInvalidSortMessage)
		return
	}

	typeDefined, typeValue, typeInvalid := publicAPIEnumQuery(c, "type", publicAPIBillingTypes)
	if typeInvalid {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidBillingType, publicAPIInvalidBillingTypeMessage)
		return
	}
	recordType := ""
	if typeDefined {
		recordType = typeValue
	}

	serviceIDParam := publicAPIQuery(c, "serviceId")
	var serviceFilter *int64
	if serviceIDParam.repeated {
		// Repeated query parameters are an array in Fastify → parsePositiveId null.
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidServiceID, publicAPIInvalidServiceIDMessage)
		return
	}
	if serviceIDParam.single {
		serviceID, defined, valid := publicAPIParseOptionalPositiveID(&serviceIDParam.value)
		if !valid {
			publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidServiceID, publicAPIInvalidServiceIDMessage)
			return
		}
		if defined {
			serviceFilter = &serviceID
		}
	}

	billingTypes := []string(nil)
	if recordType != "" {
		billingTypes = []string{recordType}
	}
	where, args := publicAPIBillingWhere(token.UserID, billingTypes, serviceFilter)
	total, err := publicAPICountBillings(c.Request.Context(), where, args)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	rows, err := publicAPIListBillings(c.Request.Context(), where, args, publicAPIOrderBy(sortSpec), int64(pagination.take), pagination.skip)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.billing_records_list",
		fmt.Sprintf("Public API %s listed own billing records", token.Source))

	data := make([]publicAPIBillingRecordView, 0, len(rows))
	for _, row := range rows {
		data = append(data, publicAPIBillingRecordFromRow(row))
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": publicAPIPaginationMetaFor(pagination.page, pagination.pageSize, total, sortSpec.value),
	})
}

// publicAPIBillingRecordReadHandler mirrors GET /billing-records/:id.
func publicAPIBillingRecordReadHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeBillingRead)
	if token == nil {
		return
	}

	id, ok := publicAPIParsePositiveID(c.Param("id"))
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidBillingID, publicAPIInvalidBillingIDMessage)
		return
	}
	row, err := publicAPIGetBilling(c.Request.Context(), id, token.UserID)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if row == nil {
		publicAPIRespondError(c, http.StatusNotFound, publicAPIBillingNotFound, publicAPIBillingNotFoundMessage)
		return
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.billing_record_read",
		fmt.Sprintf("Public API %s read own billing record #%d", token.Source, id))

	c.JSON(http.StatusOK, gin.H{"data": publicAPIBillingRecordFromRow(*row)})
}

// ==================== services ====================

// publicAPIServicesListHandler mirrors GET /services.
func publicAPIServicesListHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeServicesRead)
	if token == nil {
		return
	}

	pagination := publicAPIParsePagination(func(key string) publicAPIQueryParam { return publicAPIQuery(c, key) })
	sortSpec := publicAPIParseSort(publicAPIQuery(c, "sort"), publicAPIServiceSorts, "displayOrder")
	if sortSpec == nil {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidSortCode, publicAPIInvalidSortMessage)
		return
	}

	definedStatus, status, invalidStatus := publicAPIEnumQuery(c, "status", publicAPIServiceStatuses)
	if invalidStatus {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidServiceStatus, publicAPIInvalidServiceStatusMsg)
		return
	}
	if !definedStatus {
		status = ""
	}

	includes := publicAPIParseInclude(publicAPIQuery(c, "include"), publicAPIServiceIncludes)
	if includes == nil {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidServiceInclude, publicAPIInvalidServiceIncludeMsg)
		return
	}

	where, args := publicAPIServiceWhere(token.UserID, status)
	total, err := publicAPICountServices(c.Request.Context(), where, args)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	rows, err := publicAPIListServices(c.Request.Context(), where, args, publicAPIOrderBy(sortSpec), int64(pagination.take), pagination.skip)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	planRefs, err := publicAPIPlanRefsForServices(c.Request.Context(), rows)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.services_list",
		fmt.Sprintf("Public API %s listed own services", token.Source))

	data := make([]publicAPIServiceView, 0, len(rows))
	for _, row := range rows {
		var plan *publicAPIPlanRefRow
		if row.PackagePlanID != nil {
			if ref, ok := planRefs[*row.PackagePlanID]; ok {
				plan = &ref
			}
		}
		data = append(data, publicAPIServiceFromRow(row, plan))
	}
	included := publicAPIBuildServiceIncluded(rows, planRefs, includes)

	response := gin.H{
		"data": data,
		"meta": publicAPIPaginationMetaFor(pagination.page, pagination.pageSize, total, sortSpec.value),
	}
	if included != nil {
		response["included"] = included
	}
	c.JSON(http.StatusOK, response)
}

// publicAPIPlanRefsForServices loads the plan/package projection for the services
// in one batch, mirroring Prisma's include.
func publicAPIPlanRefsForServices(ctx context.Context, rows []publicAPIServiceRow) (map[int32]publicAPIPlanRefRow, error) {
	planIDs := make([]int32, 0, len(rows))
	seen := map[int32]bool{}
	for _, row := range rows {
		if row.PackagePlanID == nil || seen[*row.PackagePlanID] {
			continue
		}
		seen[*row.PackagePlanID] = true
		planIDs = append(planIDs, *row.PackagePlanID)
	}
	return publicAPILoadPlanRefs(ctx, planIDs)
}

// publicAPIServiceReadHandler mirrors GET /services/:id.
func publicAPIServiceReadHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeServicesRead)
	if token == nil {
		return
	}

	id, ok := publicAPIParsePositiveID(c.Param("id"))
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidServiceID, publicAPIInvalidServiceIDMessage)
		return
	}
	includes := publicAPIParseInclude(publicAPIQuery(c, "include"), publicAPIServiceIncludes)
	if includes == nil {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidServiceInclude, publicAPIInvalidServiceIncludeMsg)
		return
	}
	row, err := publicAPIGetService(c.Request.Context(), id, token.UserID)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if row == nil {
		publicAPIRespondError(c, http.StatusNotFound, publicAPIServiceNotFound, publicAPIServiceNotFoundMessage)
		return
	}
	planRefs, err := publicAPIPlanRefsForServices(c.Request.Context(), []publicAPIServiceRow{*row})
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.service_read",
		fmt.Sprintf("Public API %s read own service #%d", token.Source, id))

	var plan *publicAPIPlanRefRow
	if row.PackagePlanID != nil {
		if ref, ok := planRefs[*row.PackagePlanID]; ok {
			plan = &ref
		}
	}
	response := publicAPIServiceResponse{Data: publicAPIServiceFromRow(*row, plan)}
	response.Included = publicAPIBuildServiceIncluded([]publicAPIServiceRow{*row}, planRefs, includes)
	c.JSON(http.StatusOK, response)
}

// publicAPINormalizeServiceOperation mirrors normalizePublicServiceOperation.
func publicAPINormalizeServiceOperation(value any) (string, bool) {
	text, ok := publicAPIStringOf(value)
	if !ok {
		return "", false
	}
	action := publicAPIJSTrim(text)
	if action != "start" && action != "stop" && action != "restart" {
		return "", false
	}
	return action, true
}

// publicAPIServiceActionHandler mirrors POST /services/:id/actions.
func publicAPIServiceActionHandler(c *gin.Context) {
	if !publicAPIValidateBody(c) {
		return
	}
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeServicesOperate)
	if token == nil {
		return
	}

	id, ok := publicAPIParsePositiveID(c.Param("id"))
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidServiceID, publicAPIInvalidServiceIDMessage)
		return
	}
	actionValue, _ := publicAPIBodyField(publicAPIReadBody(c), "action")
	action, ok := publicAPINormalizeServiceOperation(actionValue)
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidServiceAction, publicAPIInvalidServiceActionMsg)
		return
	}

	service, err := publicAPILoadServiceOperation(c.Request.Context(), id, int64(token.UserID))
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if service == nil {
		publicAPIRespondError(c, http.StatusNotFound, publicAPIServiceNotFound, publicAPIServiceNotFoundMessage)
		return
	}

	denial, err := publicAPIAssertServiceOperationAllowed(c.Request.Context(), service, action)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if denial != nil {
		body := gin.H{"error": denial.Message, "code": denial.Code}
		if denial.Details != nil {
			body["details"] = denial.Details
		}
		c.AbortWithStatusJSON(denial.Status, body)
		return
	}

	task, err := db.CreateInstanceTask(c.Request.Context(), db.CreateInstanceTaskData{
		InstanceID: service.ID,
		HostID:     service.HostID,
		UserID:     token.UserID,
		TaskType:   models.InstanceTaskType(action),
	})
	if err != nil {
		var conflict *db.InstanceTaskConflictError
		if errors.As(err, &conflict) {
			body := gin.H{"error": publicAPITaskRunningMessage, "code": publicAPIServiceTaskRunning}
			if conflict.ActiveTask != nil {
				body["details"] = gin.H{
					"taskId":   conflict.ActiveTask.ID,
					"taskType": conflict.ActiveTask.TaskType,
					"status":   conflict.ActiveTask.Status,
				}
			}
			c.AbortWithStatusJSON(http.StatusConflict, body)
			return
		}
		publicAPIRespondInternal(c)
		return
	}

	publicAPILogWithInstance(c, token.UserID, service.ID, db.LogModulePlugin, "public_api.service_action_queued",
		fmt.Sprintf(`Public API %s queued %s task #%d for own service "%s"`, token.Source, action, task.ID, service.Name))

	c.JSON(http.StatusAccepted, gin.H{"data": gin.H{
		"serviceId": service.ID,
		"action":    action,
		"taskId":    task.ID,
		"taskType":  task.TaskType,
		"status":    task.Status,
	}})
}

// publicAPINormalizeServiceRenewMonths mirrors normalizePublicServiceRenewMonths.
func publicAPINormalizeServiceRenewMonths(value any) (int, bool) {
	number, ok := publicAPIScalarNumber(value)
	if !ok {
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) {
		return 0, false
	}
	if math.Abs(number) > float64(publicAPIMaxSafeInteger) {
		return 0, false
	}
	months := int(number)
	if months < 1 || months > 24 {
		return 0, false
	}
	return months, true
}

// publicAPIServiceRenewHandler mirrors POST /services/:id/renew.
func publicAPIServiceRenewHandler(c *gin.Context) {
	if !publicAPIValidateBody(c) {
		return
	}
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeServicesBilling)
	if token == nil {
		return
	}

	id, ok := publicAPIParsePositiveID(c.Param("id"))
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidServiceID, publicAPIInvalidServiceIDMessage)
		return
	}
	monthsValue, _ := publicAPIBodyField(publicAPIReadBody(c), "months")
	months, ok := publicAPINormalizeServiceRenewMonths(monthsValue)
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidRenewMonths, publicAPIInvalidRenewMonthsMessage)
		return
	}

	service, err := publicAPILoadServiceForRenew(c.Request.Context(), id, int64(token.UserID))
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if service == nil {
		publicAPIRespondError(c, http.StatusNotFound, publicAPIServiceNotFound, publicAPIServiceNotFoundMessage)
		return
	}
	if denial := publicAPIAssertServiceRenewAllowed(service); denial != nil {
		c.AbortWithStatusJSON(denial.Status, gin.H{"error": denial.Message, "code": denial.Code})
		return
	}

	result, err := PublicAPIPerformRenewal(c.Request.Context(), token.UserID, service.ID, months)
	if err != nil {
		publicAPILogResult(c, token.UserID, db.LogModulePlugin, "public_api.service_renew_failed",
			fmt.Sprintf(`Public API %s failed to renew own service "%s": %s`, token.Source, serviceNameForRenew(c, id), err.Error()),
			db.LogResultFailed)
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIServiceRenewFailed, err.Error())
		return
	}

	lib.SendNotification(c.Request.Context(), int64(token.UserID), lib.EventInstanceRenewed, lib.EventData{
		"instanceName": serviceNameForRenew(c, id),
		"renewAmount":  result.Amount,
		"renewMonths":  months,
		"newExpiresAt": publicAPIISO(result.NewExpiresAt),
	})

	if user, findErr := db.FindUserById(c.Request.Context(), token.UserID); findErr == nil && user != nil && user.Email != nil {
		lib.SendRenewSuccessEmail(c.Request.Context(), *user.Email, lib.RenewSuccessEmailData{
			Username:     user.Username,
			InstanceName: serviceNameForRenew(c, id),
			Amount:       result.Amount,
			Months:       months,
			NewExpiresAt: result.NewExpiresAt,
		})
	}

	name := serviceNameForRenew(c, id)
	publicAPILogWithInstance(c, token.UserID, service.ID, db.LogModulePlugin, "public_api.service_renew",
		fmt.Sprintf(`Public API %s renewed own service "%s" for %d month(s), amount: %s`,
			token.Source, name, months, formatTerminalCommandNumber(result.Amount)))

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"serviceId":      service.ID,
		"months":         months,
		"amount":         result.Amount,
		"discountAmount": result.DiscountAmount,
		"newExpiresAt":   publicAPIISO(result.NewExpiresAt),
	}})
}

// serviceNameForRenew loads the instance name for the renewal log/notification
// messages. performRenewal in Node receives the whole instance row; the seam only
// receives ids, so the name is read here.
func serviceNameForRenew(c *gin.Context, id int64) string {
	var rows []struct {
		Name string `gorm:"column:name"`
	}
	if err := db.DB.WithContext(c.Request.Context()).Raw(
		"SELECT name FROM instances WHERE id = ? LIMIT 1", id,
	).Scan(&rows).Error; err != nil || len(rows) == 0 {
		return ""
	}
	return rows[0].Name
}

// publicAPISerializeOperationTask mirrors serializePublicServiceOperationTask.
func publicAPISerializeOperationTask(ctx context.Context, row *publicAPIOperationTaskRow) (gin.H, error) {
	queuePosition := int64(0)
	if row.Status == "PENDING" {
		position, err := db.GetTaskQueuePosition(ctx, row.ID, row.HostID)
		if err != nil {
			return nil, err
		}
		queuePosition = position
	}
	return gin.H{
		"id":            row.ID,
		"serviceId":     row.InstanceID,
		"taskType":      row.TaskType,
		"status":        row.Status,
		"progress":      row.Progress,
		"error":         row.Error,
		"queuePosition": queuePosition,
		"createdAt":     publicAPIISO(row.CreatedAt),
		"startedAt":     publicAPIISOPtr(row.StartedAt),
		"finishedAt":    publicAPIISOPtr(row.FinishedAt),
	}, nil
}

// publicAPIServiceTaskReadHandler mirrors GET /services/:id/tasks/:taskId.
func publicAPIServiceTaskReadHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeServicesOperate)
	if token == nil {
		return
	}

	serviceID, okService := publicAPIParsePositiveID(c.Param("id"))
	taskID, okTask := publicAPIParsePositiveID(c.Param("taskId"))
	if !okService || !okTask {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidServiceTaskID, publicAPIInvalidServiceTaskIDMsg)
		return
	}

	task, err := publicAPILoadOperationTask(c.Request.Context(), int64(token.UserID), serviceID, taskID)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if task == nil {
		publicAPIRespondError(c, http.StatusNotFound, publicAPIServiceTaskNotFound, publicAPIServiceTaskNotFoundMessage)
		return
	}

	publicAPILogWithInstance(c, token.UserID, int32(serviceID), db.LogModulePlugin, "public_api.service_task_read",
		fmt.Sprintf("Public API %s read task #%d for own service #%d", token.Source, task.ID, serviceID))

	data, err := publicAPISerializeOperationTask(c.Request.Context(), task)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": data})
}

// publicAPIServiceTaskCancelHandler mirrors DELETE /services/:id/tasks/:taskId.
func publicAPIServiceTaskCancelHandler(c *gin.Context) {
	if !publicAPIValidateBody(c) {
		return
	}
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeServicesOperate)
	if token == nil {
		return
	}

	serviceID, okService := publicAPIParsePositiveID(c.Param("id"))
	taskID, okTask := publicAPIParsePositiveID(c.Param("taskId"))
	if !okService || !okTask {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidServiceTaskID, publicAPIInvalidServiceTaskIDMsg)
		return
	}

	task, err := publicAPILoadOperationTask(c.Request.Context(), int64(token.UserID), serviceID, taskID)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if task == nil {
		publicAPIRespondError(c, http.StatusNotFound, publicAPIServiceTaskNotFound, publicAPIServiceTaskNotFoundMessage)
		return
	}
	if task.Status != "PENDING" {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error":         publicAPIServiceTaskCancelMessage,
			"code":          publicAPIServiceTaskCannotStop,
			"currentStatus": task.Status,
		})
		return
	}

	cancelled, err := db.CancelInstanceTask(c.Request.Context(), int32(task.ID))
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if cancelled == nil {
		publicAPIRespondError(c, http.StatusConflict, publicAPIServiceTaskCannotStop, publicAPIServiceTaskCanceledMessage)
		return
	}

	publicTask, err := publicAPILoadOperationTask(c.Request.Context(), int64(token.UserID), serviceID, int64(cancelled.ID))
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if publicTask == nil {
		publicAPIRespondError(c, http.StatusNotFound, publicAPIServiceTaskNotFound, publicAPIServiceTaskNotFoundMessage)
		return
	}

	publicAPILogWithInstance(c, token.UserID, int32(serviceID), db.LogModulePlugin, "public_api.service_task_cancel",
		fmt.Sprintf("Public API %s cancelled task #%d for own service #%d", token.Source, task.ID, serviceID))

	data, err := publicAPISerializeOperationTask(c.Request.Context(), publicTask)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": data})
}

// ==================== tickets ====================

type publicAPIAttachmentView struct {
	ID           int32   `json:"id"`
	Filename     string  `json:"filename"`
	OriginalName string  `json:"originalName"`
	MimeType     string  `json:"mimeType"`
	SizeBytes    int32   `json:"sizeBytes"`
	ThumbnailURL *string `json:"thumbnailUrl"`
	CreatedAt    string  `json:"createdAt"`
}

func publicAPIAttachmentFromRow(row publicAPIAttachmentRow) publicAPIAttachmentView {
	return publicAPIAttachmentView{
		ID:           row.ID,
		Filename:     row.Filename,
		OriginalName: row.OriginalName,
		MimeType:     row.MimeType,
		SizeBytes:    row.SizeBytes,
		ThumbnailURL: row.ThumbnailURL,
		CreatedAt:    publicAPIISO(row.CreatedAt),
	}
}

type publicAPILatestMessageView struct {
	ID          int32  `json:"id"`
	SenderID    int32  `json:"senderId"`
	Content     string `json:"content"`
	IsFromOwner bool   `json:"isFromOwner"`
	CreatedAt   string `json:"createdAt"`
}

type publicAPITicketSummaryView struct {
	ID            int32                       `json:"id"`
	Subject       string                      `json:"subject"`
	Category      string                      `json:"category"`
	Priority      string                      `json:"priority"`
	Status        string                      `json:"status"`
	HostID        *int32                      `json:"hostId"`
	InstanceID    *int32                      `json:"instanceId"`
	CreatedAt     string                      `json:"createdAt"`
	UpdatedAt     string                      `json:"updatedAt"`
	ResolvedAt    *string                     `json:"resolvedAt"`
	ClosedAt      *string                     `json:"closedAt"`
	LatestMessage *publicAPILatestMessageView `json:"latestMessage"`
}

// publicAPITicketsListHandler mirrors GET /tickets.
func publicAPITicketsListHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeTicketsRead)
	if token == nil {
		return
	}

	pagination := publicAPIParsePagination(func(key string) publicAPIQueryParam { return publicAPIQuery(c, key) })
	sortSpec := publicAPIParseSort(publicAPIQuery(c, "sort"), publicAPITicketSorts, "-updatedAt")
	if sortSpec == nil {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidSortCode, publicAPIInvalidSortMessage)
		return
	}

	defined, status, invalid := publicAPIEnumQuery(c, "status", publicAPITicketStatuses)
	if invalid {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidTicketStatus, publicAPIInvalidTicketStatusMsg)
		return
	}
	if !defined {
		status = ""
	}
	defined, category, invalid := publicAPIEnumQuery(c, "category", publicAPITicketCategories)
	if invalid {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidTicketCategory, publicAPIInvalidTicketCategoryMsg)
		return
	}
	if !defined {
		category = ""
	}
	defined, priority, invalid := publicAPIEnumQuery(c, "priority", publicAPITicketPriorities)
	if invalid {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidTicketPriority, publicAPIInvalidTicketPriorityMsg)
		return
	}
	if !defined {
		priority = ""
	}

	where, args := publicAPITicketWhere(token.UserID, status, category, priority)
	total, err := publicAPICountTickets(c.Request.Context(), where, args)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	rows, err := publicAPIListTickets(c.Request.Context(), where, args, publicAPIOrderBy(sortSpec), int64(pagination.take), pagination.skip)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	ticketIDs := make([]int32, 0, len(rows))
	for _, row := range rows {
		ticketIDs = append(ticketIDs, row.ID)
	}
	latest, err := publicAPILatestMessages(c.Request.Context(), ticketIDs)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.tickets_list",
		fmt.Sprintf("Public API %s listed own tickets", token.Source))

	data := make([]publicAPITicketSummaryView, 0, len(rows))
	for _, row := range rows {
		var latestView *publicAPILatestMessageView
		if message, ok := latest[row.ID]; ok {
			latestView = &publicAPILatestMessageView{
				ID:          message.ID,
				SenderID:    message.SenderID,
				Content:     message.Content,
				IsFromOwner: message.IsFromOwner,
				CreatedAt:   publicAPIISO(message.CreatedAt),
			}
		}
		data = append(data, publicAPITicketSummaryView{
			ID:            row.ID,
			Subject:       row.Subject,
			Category:      row.Category,
			Priority:      row.Priority,
			Status:        row.Status,
			HostID:        row.HostID,
			InstanceID:    row.InstanceID,
			CreatedAt:     publicAPIISO(row.CreatedAt),
			UpdatedAt:     publicAPIISO(row.UpdatedAt),
			ResolvedAt:    publicAPIISOPtr(row.ResolvedAt),
			ClosedAt:      publicAPIISOPtr(row.ClosedAt),
			LatestMessage: latestView,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": publicAPIPaginationMetaFor(pagination.page, pagination.pageSize, total, sortSpec.value),
	})
}

// publicAPINormalizeTicketSubject mirrors normalizePublicTicketSubject.
func publicAPINormalizeTicketSubject(value any) *string {
	text, ok := publicAPIStringOf(value)
	if !ok {
		return nil
	}
	subject := publicAPIJSTrim(text)
	length := jsStringLength(subject)
	if length < publicAPIMinTicketSubjectLength || length > publicAPIMaxTicketSubjectLength {
		return nil
	}
	return &subject
}

// publicAPINormalizeTicketContent mirrors normalizePublicTicketMessageContent.
func publicAPINormalizeTicketContent(value any) string {
	text, ok := publicAPIStringOf(value)
	if !ok {
		return ""
	}
	return publicAPIJSTrim(text)
}

// publicAPINormalizeTicketCategory mirrors normalizePublicTicketCategory.
func publicAPINormalizeTicketCategory(value any) string {
	text, ok := publicAPIStringOf(value)
	if !ok {
		return "general"
	}
	category := publicAPIJSTrim(text)
	if !publicAPITicketCategories[category] {
		return "general"
	}
	return category
}

// publicAPINormalizeTicketPriority mirrors normalizePublicTicketPriority.
func publicAPINormalizeTicketPriority(value any) string {
	text, ok := publicAPIStringOf(value)
	if !ok {
		return "normal"
	}
	priority := publicAPIJSTrim(text)
	if !publicAPITicketPriorities[priority] {
		return "normal"
	}
	return priority
}

// publicAPINormalizeTicketStatusAction mirrors normalizePublicTicketStatusAction.
func publicAPINormalizeTicketStatusAction(value any) (string, bool) {
	text, ok := publicAPIStringOf(value)
	if !ok {
		return "", false
	}
	action := publicAPIJSTrim(text)
	if action != "close" && action != "reopen" {
		return "", false
	}
	return action, true
}

// publicAPITicketCreateHandler mirrors POST /tickets.
func publicAPITicketCreateHandler(c *gin.Context) {
	base, baseErr := ticketsParseFrameworkBody(c, ticketsUploadBodyLimit)
	if baseErr != nil {
		ticketsRespondPayloadError(c, baseErr, "Invalid ticket payload")
		return
	}

	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeTicketsWrite)
	if token == nil {
		return
	}

	ctx := c.Request.Context()
	uploaded := make([]db.TicketsAttachmentData, 0)
	cleanupAndRespond := func(err error) {
		if len(uploaded) > 0 {
			ticketsCleanupUploads(ctx, uploaded)
		}
		if ticketsIsHandledPayloadError(err) {
			publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidTicketAttach, err.Error())
			return
		}
		publicAPIRespondInternal(c)
	}

	enabled, err := db.GetSystemConfigBoolean(ctx, "ticket_enabled", true)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if !enabled {
		publicAPIRespondError(c, http.StatusForbidden, publicAPITicketCreationDisabled, publicAPITicketDisabledMessage)
		return
	}

	payload, payloadErr := ticketsReadPayload(c, base)
	if payloadErr != nil {
		cleanupAndRespond(payloadErr)
		return
	}

	subject := publicAPINormalizeTicketSubject(optionalString(payload.Fields, "subject"))
	content := publicAPINormalizeTicketContent(optionalString(payload.Fields, "content"))
	if subject == nil || jsStringLength(content) > publicAPIMaxTicketReplyLength ||
		(len(payload.Images) == 0 && jsStringLength(content) < publicAPIMinTicketCreateContentLength) {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidTicketCreate, publicAPIInvalidTicketCreateMessage)
		return
	}

	instanceID, instanceDefined, instanceValid := publicAPIParseOptionalPositiveID(optionalString(payload.Fields, "instanceId"))
	if !instanceValid {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidInstanceID, publicAPIInvalidInstanceIDMessage)
		return
	}

	var hostID *int32
	var host *publicAPITicketHostOwnership
	var instanceName string
	if instanceDefined {
		instance, err := publicAPIInstanceForTicket(ctx, instanceID, int64(token.UserID))
		if err != nil {
			publicAPIRespondInternal(c)
			return
		}
		if instance == nil {
			publicAPIRespondError(c, http.StatusNotFound, publicAPIInstanceNotFound, publicAPIInstanceNotFoundMessage)
			return
		}
		hostID = &instance.HostID
		instanceName = instance.Name
		host, err = publicAPIHostOwnership(ctx, instance.HostID)
		if err != nil {
			publicAPIRespondInternal(c)
			return
		}
		if host == nil {
			publicAPIRespondError(c, http.StatusNotFound, publicAPIHostNotFound, publicAPIHostNotFoundMessage)
			return
		}
	}

	category := publicAPINormalizeTicketCategory(optionalString(payload.Fields, "category"))
	priority := publicAPINormalizeTicketPriority(optionalString(payload.Fields, "priority"))

	if len(payload.Images) > 0 {
		uploaded, err = ticketsUploadImages(ctx, payload.Images)
		if err != nil {
			cleanupAndRespond(err)
			return
		}
	}

	priorityValue := models.TicketPriority(priority)
	categoryValue := category
	ticketID, messageID, err := db.TicketsCreateTicket(ctx, db.TicketsCreateTicketData{
		UserID:      token.UserID,
		HostID:      hostID,
		InstanceID:  optionalInt32(instanceID, instanceDefined),
		Subject:     *subject,
		Category:    &categoryValue,
		Priority:    &priorityValue,
		Content:     content,
		Attachments: uploaded,
	})
	if err != nil {
		cleanupAndRespond(err)
		return
	}

	attachments := make([]publicAPIAttachmentView, 0)
	if len(uploaded) > 0 {
		rows, err := publicAPIListMessageAttachments(ctx, ticketID, messageID)
		if err != nil {
			cleanupAndRespond(err)
			return
		}
		for _, row := range rows {
			attachments = append(attachments, publicAPIAttachmentFromRow(row))
		}
	}

	hostName := "系统"
	if host != nil && host.Name != "" {
		hostName = host.Name
	}
	if instanceName == "" {
		instanceName = "无"
	}
	notificationData := lib.EventData{
		"username":     token.User.Username,
		"subject":      *subject,
		"hostName":     hostName,
		"instanceName": instanceName,
	}
	if host != nil && host.Role != "admin" {
		lib.SendNotification(ctx, int64(host.UserID), lib.EventTicketCreated, notificationData)
	} else {
		if adminIDs, adminErr := db.TicketsGetAllAdminUserIDs(ctx); adminErr == nil {
			for _, adminID := range adminIDs {
				lib.SendNotification(ctx, int64(adminID), lib.EventTicketCreated, notificationData)
			}
		}
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.ticket_create",
		fmt.Sprintf("Public API %s created ticket #%d", token.Source, ticketID))

	c.JSON(http.StatusCreated, gin.H{"data": gin.H{
		"id":          ticketID,
		"messageId":   messageID,
		"subject":     *subject,
		"category":    category,
		"priority":    priority,
		"status":      "open",
		"hostId":      hostID,
		"instanceId":  optionalInt32(instanceID, instanceDefined),
		"attachments": attachments,
	}})
}

// optionalInt32 mirrors `value ?? null` for an optional positive id.
func optionalInt32(value int64, defined bool) *int32 {
	if !defined {
		return nil
	}
	converted := int32(value)
	return &converted
}

// publicAPITicketReadHandler mirrors GET /tickets/:id.
func publicAPITicketReadHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeTicketsRead)
	if token == nil {
		return
	}

	id, ok := publicAPIParsePositiveID(c.Param("id"))
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidTicketID, publicAPIInvalidTicketIDMessage)
		return
	}
	ticket, err := publicAPIGetTicket(c.Request.Context(), id, int64(token.UserID))
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if ticket == nil {
		publicAPIRespondError(c, http.StatusNotFound, publicAPITicketNotFound, publicAPITicketNotFoundMessage)
		return
	}

	messages, err := publicAPIListTicketMessages(c.Request.Context(), ticket.ID)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	messageIDs := make([]int32, 0, len(messages))
	for _, message := range messages {
		messageIDs = append(messageIDs, message.ID)
	}
	attachments, err := publicAPIListAttachments(c.Request.Context(), ticket.ID, messageIDs)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.ticket_read",
		fmt.Sprintf("Public API %s read own ticket #%d", token.Source, id))

	messageViews := make([]gin.H, 0, len(messages))
	for _, message := range messages {
		attachmentViews := make([]publicAPIAttachmentView, 0, len(attachments[message.ID]))
		for _, attachment := range attachments[message.ID] {
			attachmentViews = append(attachmentViews, publicAPIAttachmentFromRow(attachment))
		}
		messageViews = append(messageViews, gin.H{
			"id":          message.ID,
			"senderId":    message.SenderID,
			"content":     message.Content,
			"isFromOwner": message.IsFromOwner,
			"createdAt":   publicAPIISO(message.CreatedAt),
			"attachments": attachmentViews,
		})
	}

	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"id":         ticket.ID,
		"subject":    ticket.Subject,
		"category":   ticket.Category,
		"priority":   ticket.Priority,
		"status":     ticket.Status,
		"hostId":     ticket.HostID,
		"instanceId": ticket.InstanceID,
		"createdAt":  publicAPIISO(ticket.CreatedAt),
		"updatedAt":  publicAPIISO(ticket.UpdatedAt),
		"resolvedAt": publicAPIISOPtr(ticket.ResolvedAt),
		"closedAt":   publicAPIISOPtr(ticket.ClosedAt),
		"messages":   messageViews,
	}})
}

// publicAPITicketStatusHandler mirrors PATCH /tickets/:id/status.
func publicAPITicketStatusHandler(c *gin.Context) {
	if !publicAPIValidateBody(c) {
		return
	}
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeTicketsWrite)
	if token == nil {
		return
	}

	id, ok := publicAPIParsePositiveID(c.Param("id"))
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidTicketID, publicAPIInvalidTicketIDMessage)
		return
	}
	actionValue, _ := publicAPIBodyField(publicAPIReadBody(c), "action")
	action, ok := publicAPINormalizeTicketStatusAction(actionValue)
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidTicketStatusAct, publicAPIInvalidTicketStatusActMsg)
		return
	}

	ticket, err := publicAPIGetTicket(c.Request.Context(), id, int64(token.UserID))
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if ticket == nil {
		publicAPIRespondError(c, http.StatusNotFound, publicAPITicketNotFound, publicAPITicketNotFoundMessage)
		return
	}
	if action == "close" && ticket.Status == "closed" {
		publicAPIRespondError(c, http.StatusConflict, publicAPITicketAlreadyClosed, publicAPITicketAlreadyClosedMessage)
		return
	}
	if action == "reopen" && ticket.Status != "closed" {
		publicAPIRespondError(c, http.StatusConflict, publicAPITicketNotClosed, publicAPITicketNotClosedMessage)
		return
	}

	now := time.Now().UTC()
	nextStatus := "open"
	if action == "close" {
		nextStatus = "closed"
	}
	query := db.DB.WithContext(c.Request.Context()).Exec(
		`UPDATE tickets SET status = ?, resolved_at = ?, closed_at = ?, updated_at = ?
		 WHERE id = ? AND user_id = ? AND `+publicAPITicketStatusGuard(action),
		nextStatus,
		publicAPINullTime(action == "close", now),
		publicAPINullTime(action == "close", now),
		now, ticket.ID, token.UserID,
	)
	if query.Error != nil {
		publicAPIRespondInternal(c)
		return
	}
	if query.RowsAffected == 0 {
		publicAPIRespondError(c, http.StatusConflict, publicAPITicketStatusConflict, publicAPITicketConflictMessage)
		return
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.ticket_status_update",
		fmt.Sprintf("Public API %s changed own ticket #%d status to %s", token.Source, ticket.ID, nextStatus))

	var resolvedAt, closedAt *string
	if action == "close" {
		iso := publicAPIISO(now)
		resolvedAt = &iso
		closedAt = &iso
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"id":             ticket.ID,
		"status":         nextStatus,
		"previousStatus": ticket.Status,
		"resolvedAt":     resolvedAt,
		"closedAt":       closedAt,
		"updatedAt":      publicAPIISO(now),
	}})
}

// publicAPITicketStatusGuard renders the conditional update's status predicate.
func publicAPITicketStatusGuard(action string) string {
	if action == "close" {
		return "status <> 'closed'"
	}
	return "status = 'closed'"
}

// publicAPINullTime converts the close-only timestamp into a SQL value.
func publicAPINullTime(present bool, value time.Time) any {
	if !present {
		return nil
	}
	return value
}

// publicAPITicketReplyHandler mirrors POST /tickets/:id/replies.
func publicAPITicketReplyHandler(c *gin.Context) {
	base, baseErr := ticketsParseFrameworkBody(c, ticketsUploadBodyLimit)
	if baseErr != nil {
		ticketsRespondPayloadError(c, baseErr, "Invalid ticket payload")
		return
	}

	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeTicketsWrite)
	if token == nil {
		return
	}

	id, ok := publicAPIParsePositiveID(c.Param("id"))
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidTicketID, publicAPIInvalidTicketIDMessage)
		return
	}

	ctx := c.Request.Context()
	uploaded := make([]db.TicketsAttachmentData, 0)
	cleanupAndRespond := func(err error) {
		if len(uploaded) > 0 {
			ticketsCleanupUploads(ctx, uploaded)
		}
		if ticketsIsHandledPayloadError(err) {
			publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidTicketAttach, err.Error())
			return
		}
		publicAPIRespondInternal(c)
	}

	payload, payloadErr := ticketsReadPayload(c, base)
	if payloadErr != nil {
		cleanupAndRespond(payloadErr)
		return
	}
	content := publicAPINormalizeTicketContent(optionalString(payload.Fields, "content"))
	if jsStringLength(content) > publicAPIMaxTicketReplyLength ||
		(len(payload.Images) == 0 && jsStringLength(content) < 1) {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidTicketReply, publicAPIInvalidTicketReplyMessage)
		return
	}

	ticket, err := publicAPITicketReplyContext(ctx, id, int64(token.UserID))
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	if ticket == nil {
		publicAPIRespondError(c, http.StatusNotFound, publicAPITicketNotFound, publicAPITicketNotFoundMessage)
		return
	}
	if ticket.Status == "closed" {
		publicAPIRespondError(c, http.StatusConflict, publicAPITicketClosed, publicAPITicketClosedMessage)
		return
	}

	if len(payload.Images) > 0 {
		uploaded, err = ticketsUploadImages(ctx, payload.Images)
		if err != nil {
			cleanupAndRespond(err)
			return
		}
	}

	message, err := db.TicketsAddTicketMessage(ctx, ticket.ID, token.UserID, content, false, uploaded)
	if err != nil {
		cleanupAndRespond(err)
		return
	}

	attachments := make([]publicAPIAttachmentView, 0)
	if len(uploaded) > 0 {
		rows, err := publicAPIListMessageAttachments(ctx, ticket.ID, message.ID)
		if err != nil {
			cleanupAndRespond(err)
			return
		}
		for _, row := range rows {
			attachments = append(attachments, publicAPIAttachmentFromRow(row))
		}
	}

	hostName := "系统"
	if ticket.HostName != nil && *ticket.HostName != "" {
		hostName = *ticket.HostName
	}
	replyData := lib.EventData{
		"subject":   ticket.Subject,
		"hostName":  hostName,
		"replyFrom": token.User.Username,
	}
	if adminIDs, adminErr := db.TicketsGetAllAdminUserIDs(ctx); adminErr == nil {
		for _, adminID := range adminIDs {
			lib.SendNotification(ctx, int64(adminID), lib.EventTicketReplied, replyData)
		}
	}

	publicAPILog(c, token.UserID, db.LogModulePlugin, "public_api.ticket_reply_create",
		fmt.Sprintf("Public API %s replied to own ticket #%d", token.Source, ticket.ID))

	c.JSON(http.StatusCreated, gin.H{"data": gin.H{
		"id":          message.ID,
		"ticketId":    message.TicketID,
		"senderId":    message.SenderID,
		"content":     message.Content,
		"isFromOwner": message.IsFromOwner,
		"createdAt":   message.CreatedAt,
		"attachments": attachments,
	}})
}

// ==================== notifications ====================

// publicAPINotificationsListHandler mirrors GET /notifications.
func publicAPINotificationsListHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeNotificationsRead)
	if token == nil {
		return
	}

	pagination := publicAPIParsePagination(func(key string) publicAPIQueryParam { return publicAPIQuery(c, key) })
	sortSpec := publicAPIParseSort(publicAPIQuery(c, "sort"), publicAPICreatedAtSorts, "-createdAt")
	if sortSpec == nil {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidSortCode, publicAPIInvalidSortMessage)
		return
	}

	var isRead *bool
	if raw, ok := publicAPIQuery(c, "isRead").asString(); ok {
		if raw == "true" {
			value := true
			isRead = &value
		} else if raw == "false" {
			value := false
			isRead = &value
		}
	}

	where, args := publicAPINotificationWhere(token.UserID, isRead)
	total, err := publicAPICountNotifications(c.Request.Context(), where, args)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}
	rows, err := publicAPIListNotifications(c.Request.Context(), where, args, publicAPIOrderBy(sortSpec), int64(pagination.take), pagination.skip)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	publicAPILog(c, token.UserID, db.LogModuleNotification, "public_api.notifications_list",
		fmt.Sprintf("Public API %s listed current user notifications", token.Source))

	data := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		data = append(data, gin.H{
			"id":        row.ID,
			"eventType": row.EventType,
			"title":     row.Title,
			"content":   row.Content,
			"isRead":    row.IsRead,
			"createdAt": publicAPIISO(row.CreatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": publicAPIPaginationMetaFor(pagination.page, pagination.pageSize, total, sortSpec.value),
	})
}

// publicAPIUnreadCountHandler mirrors GET /notifications/unread-count.
func publicAPIUnreadCountHandler(c *gin.Context) {
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeNotificationsRead)
	if token == nil {
		return
	}

	count, err := publicAPIUnreadNotificationCount(c.Request.Context(), token.UserID)
	if err != nil {
		publicAPIRespondInternal(c)
		return
	}

	publicAPILog(c, token.UserID, db.LogModuleNotification, "public_api.notifications_unread_count",
		fmt.Sprintf("Public API %s read current user unread notification count", token.Source))

	c.JSON(http.StatusOK, gin.H{"data": gin.H{"count": count}})
}

// publicAPINotificationSendHandler mirrors POST /notifications.
func publicAPINotificationSendHandler(c *gin.Context) {
	if !publicAPIValidateBody(c) {
		return
	}
	token := lib.AuthenticatePublicAPIRequest(c, lib.PublicAPIScopeNotificationsSend)
	if token == nil {
		return
	}

	body := publicAPIReadBody(c)
	variablesValue, _ := publicAPIBodyField(body, "variables")
	variables, ok := publicAPINormalizeNotificationVariables(variablesValue)
	if !ok {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidNotificationVars, publicAPIInvalidNotificationVarsMessage)
		return
	}

	templateField, _ := publicAPIBodyField(body, "template")
	templateID := ""
	title := ""
	message := ""
	if publicAPIJSTruthy(templateField) {
		id, idOK := publicAPINormalizeNotificationTemplate(templateField)
		if !idOK {
			templateIDs := "flash_sale_reminder, service_action_update, billing_notice"
			publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidNotificationTemplate,
				"template must be one of "+templateIDs+", and all required variables must be provided")
			return
		}
		renderedTitle, renderedMessage, renderedOK := publicAPIRenderNotificationTemplate(id, variables)
		if !renderedOK {
			publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidNotificationTemplate,
				"template must be one of flash_sale_reminder, service_action_update, billing_notice, and all required variables must be provided")
			return
		}
		templateID = id
		title = renderedTitle
		message = renderedMessage
	} else {
		titleValue, _ := publicAPIBodyField(body, "title")
		messageValue, _ := publicAPIBodyField(body, "message")
		if normalized := publicAPINormalizeNotificationText(titleValue, publicAPIMaxNotificationTitleLength); normalized != nil {
			title = *normalized
		}
		if normalized := publicAPINormalizeNotificationText(messageValue, publicAPIMaxNotificationMessageLength); normalized != nil {
			message = *normalized
		}
	}

	sourceValue, _ := publicAPIBodyField(body, "source")
	source := token.Name
	if normalized := publicAPINormalizeNotificationText(sourceValue, publicAPIMaxNotificationSourceLength); normalized != nil {
		source = *normalized
	}

	if title == "" || message == "" {
		publicAPIRespondError(c, http.StatusBadRequest, publicAPIInvalidNotificationPayload, publicAPIInvalidNotificationPayloadMessage)
		return
	}

	templateForData := "custom"
	if templateID != "" {
		templateForData = templateID
	}
	result := lib.SendNotification(c.Request.Context(), int64(token.UserID), lib.EventPublicAPINotification, lib.EventData{
		"publicApiTitle":    title,
		"publicApiMessage":  message,
		"publicApiSource":   source,
		"publicApiTemplate": templateForData,
	})

	publicAPILog(c, token.UserID, db.LogModuleNotification, "public_api.notification_send",
		fmt.Sprintf(`Public API %s sent self notification through token "%s"`, token.Source, token.Name))

	var templateValue *string
	if templateID != "" {
		templateValue = &templateID
	}
	c.JSON(http.StatusAccepted, gin.H{"data": gin.H{
		"userId":   token.UserID,
		"title":    title,
		"template": templateValue,
		"source":   source,
		"sent":     result.Sent,
		"failed":   result.Failed,
	}})
}

// ==================== registration ====================

// RegisterPublicAPI mounts the module at /api/v1 (app.ts:567). Every path, its
// required scope and its inline rate limit are listed here:
//
//	GET    /openapi.json                     public (no token)
//	GET    /openapi.yaml                     public (no token)
//	GET    /me                               profile:read
//	PATCH  /me                               profile:write
//	GET    /balance                          balance:read
//	GET    /balance/logs                     balance:read
//	GET    /balance/adjustment-requests      balance:write
//	POST   /balance/adjustment-requests      balance:write   5/10m
//	GET    /products                         products:read
//	GET    /products/:id                     products:read
//	GET    /orders                           orders:read
//	GET    /orders/:id                       orders:read
//	GET    /billing-records                  billing:read
//	GET    /billing-records/:id              billing:read
//	GET    /services                         services:read
//	GET    /services/:id                     services:read
//	POST   /services/:id/actions             services:operate 10/1m
//	POST   /services/:id/renew               services:billing  5/10m
//	GET    /services/:id/tasks/:taskId       services:operate 120/1m
//	DELETE /services/:id/tasks/:taskId       services:operate 10/1m
//	GET    /tickets                          tickets:read
//	POST   /tickets                          tickets:write   10/5m
//	GET    /tickets/:id                      tickets:read
//	PATCH  /tickets/:id/status               tickets:write   20/1m
//	POST   /tickets/:id/replies              tickets:write   20/1m
//	GET    /notifications                    notifications:read
//	GET    /notifications/unread-count       notifications:read
//	POST   /notifications                    notifications:send 20/1m
func RegisterPublicAPI(r *gin.Engine) {
	group := r.Group("/api/v1")

	group.GET("/openapi.json", publicAPIOpenAPIJSONHandler)
	group.GET("/openapi.yaml", publicAPIOpenAPIYAMLHandler)

	group.GET("/me", publicAPIMeReadHandler)
	group.PATCH("/me", publicAPIMeUpdateHandler)

	group.GET("/balance", publicAPIBalanceReadHandler)
	group.GET("/balance/logs", publicAPIBalanceLogsHandler)
	group.GET("/balance/adjustment-requests", publicAPIAdjustmentsListHandler)
	group.POST("/balance/adjustment-requests",
		publicAPIRateLimit("POST /balance/adjustment-requests"), publicAPICreateAdjustmentHandler)

	group.GET("/products", publicAPIProductsListHandler)
	group.GET("/products/:id", publicAPIProductReadHandler)

	group.GET("/orders", publicAPIOrdersListHandler)
	group.GET("/orders/:id", publicAPIOrderReadHandler)

	group.GET("/billing-records", publicAPIBillingRecordsListHandler)
	group.GET("/billing-records/:id", publicAPIBillingRecordReadHandler)

	group.GET("/services", publicAPIServicesListHandler)
	group.GET("/services/:id", publicAPIServiceReadHandler)
	group.POST("/services/:id/actions",
		publicAPIRateLimit("POST /services/:id/actions"), publicAPIServiceActionHandler)
	group.POST("/services/:id/renew",
		publicAPIRateLimit("POST /services/:id/renew"), publicAPIServiceRenewHandler)
	group.GET("/services/:id/tasks/:taskId",
		publicAPIRateLimit("GET /services/:id/tasks/:taskId"), publicAPIServiceTaskReadHandler)
	group.DELETE("/services/:id/tasks/:taskId",
		publicAPIRateLimit("DELETE /services/:id/tasks/:taskId"), publicAPIServiceTaskCancelHandler)

	group.GET("/tickets", publicAPITicketsListHandler)
	group.POST("/tickets", publicAPIRateLimit("POST /tickets"), publicAPITicketCreateHandler)
	group.GET("/tickets/:id", publicAPITicketReadHandler)
	group.PATCH("/tickets/:id/status",
		publicAPIRateLimit("PATCH /tickets/:id/status"), publicAPITicketStatusHandler)
	group.POST("/tickets/:id/replies",
		publicAPIRateLimit("POST /tickets/:id/replies"), publicAPITicketReplyHandler)

	group.GET("/notifications", publicAPINotificationsListHandler)
	group.GET("/notifications/unread-count", publicAPIUnreadCountHandler)
	group.POST("/notifications", publicAPIRateLimit("POST /notifications"), publicAPINotificationSendHandler)
}

func init() {
	go publicAPIInlineLimiter.reap()
	Register(RegisterPublicAPI)
}
