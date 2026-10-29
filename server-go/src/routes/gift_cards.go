// Port of server/src/routes/gift-cards.ts.
//
// Wire contract notes (all verified against the TypeScript source):
//   - Amounts are JSON *numbers* here. db/gift_cards.go already converts Decimal
//     to float64 with toMoney() semantics, so nothing decimal leaks to the client.
//   - POST /admin/generate returns 201 { giftCard }; POST /admin/batch returns
//     201 { batchId, count, codes: [{id, code, faceValue, balanceValue}] } — the
//     batch route deliberately narrows the code projection.
//   - Admin routes require authenticate + requireAdmin + the gift-card allowlist
//     (PAYINCUS_GIFT_CARD_ADMIN_IDS / payincus_gift_card_admin_ids).
//   - /user/redeem and /user/generate run Turnstile *after* body validation,
//     mirroring Fastify's preHandler ordering.
package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const giftCardLogModule = "gift_card"

// giftCardCodePattern mirrors GIFT_CARD_CODE_REGEX = /^gc-[A-Za-z0-9_-]{24}$/.
var giftCardCodePattern = regexp.MustCompile(`^gc-[A-Za-z0-9_-]{24}$`)

// giftCardControlPattern mirrors /[\u0000-\u001F\u007F]/.
var giftCardControlPattern = regexp.MustCompile(`[\x00-\x1F\x7F]`)

// giftCardStatuses mirrors the querystring `enum` for status.
var giftCardStatuses = map[string]bool{
	"active":   true,
	"used":     true,
	"disabled": true,
	"expired":  true,
}

// giftCardAdminFields / giftCardBatchFields mirror the two admin body schemas, in
// declaration order (AJV reports the first failure only).
var giftCardAdminFields = []giftCardFieldSpec{
	{Name: "faceValue", Kind: giftCardKindNumber, HasMin: true, Minimum: 0.01, HasMax: true, Maximum: 10000},
	{Name: "balanceValue", Kind: giftCardKindNumber, HasMin: true, Minimum: 0.01, HasMax: true, Maximum: 10000},
	{Name: "expiresAt", Kind: giftCardKindNullableString},
	{Name: "remark", Kind: giftCardKindString, HasMax: true, MaxLength: db.GiftCardMaxRemarkLength},
}

var giftCardBatchFields = []giftCardFieldSpec{
	{Name: "faceValue", Kind: giftCardKindNumber, HasMin: true, Minimum: 0.01, HasMax: true, Maximum: 10000},
	{Name: "balanceValue", Kind: giftCardKindNumber, HasMin: true, Minimum: 0.01, HasMax: true, Maximum: 10000},
	{Name: "count", Kind: giftCardKindInteger, HasMin: true, Minimum: 1, HasMax: true, Maximum: db.GiftCardMaxBatchCount},
	{Name: "expiresAt", Kind: giftCardKindNullableString},
	{Name: "remark", Kind: giftCardKindString, HasMax: true, MaxLength: db.GiftCardMaxRemarkLength},
}

// ==================== AJV number/int coercion ====================

type giftCardFieldKind int

const (
	giftCardKindString giftCardFieldKind = iota
	giftCardKindNullableString
	giftCardKindNumber
	giftCardKindInteger
)

// giftCardFieldSpec describes one body property. Unset bounds/lengths are the Go
// zero value plus the explicit "has" flags.
type giftCardFieldSpec struct {
	Name      string
	Kind      giftCardFieldKind
	MinLength int
	MaxLength int
	HasMax    bool
	HasMin    bool
	Minimum   float64
	Maximum   float64
}

// giftCardBodyValues holds the coerced body values.
type giftCardBodyValues struct {
	Values  map[string]any
	Present map[string]bool
}

func (v giftCardBodyValues) number(name string) (float64, bool) {
	raw, present := v.Values[name]
	if !present {
		return 0, false
	}
	number, ok := raw.(float64)
	return number, ok
}

func (v giftCardBodyValues) stringValue(name string) (string, bool) {
	raw, present := v.Values[name]
	if !present {
		return "", false
	}
	text, ok := raw.(string)
	return text, ok
}

func (v giftCardBodyValues) integer(name string) (int, bool) {
	number, ok := v.number(name)
	if !ok {
		return 0, false
	}
	return int(number), true
}

// ajvNumberValue mirrors AJV's `coerceTypes` handling of `type: 'number'`:
// booleans become 0/1, null becomes 0, numeric strings are parsed, and a
// single-element array is unwrapped first.
func ajvNumberValue(value any) (float64, bool) {
	if array, ok := value.([]any); ok && len(array) == 1 {
		return ajvNumberValue(array[0])
	}
	switch typed := value.(type) {
	case float64:
		return typed, true
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			// Number("") === 0
			return 0, true
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, false
		}
		return parsed, true
	}
	return 0, false
}

// ajvIntegerValue mirrors `type: 'integer'` coercion (integers only).
func ajvIntegerValue(value any) (float64, bool) {
	number, ok := ajvNumberValue(value)
	if !ok {
		return 0, false
	}
	if number != math.Trunc(number) || math.IsInf(number, 0) || math.IsNaN(number) {
		return 0, false
	}
	return number, true
}

// ajvStringValue mirrors `type: 'string'` coercion.
func ajvStringValue(value any) (string, bool) {
	if array, ok := value.([]any); ok && len(array) == 1 {
		return ajvStringValue(array[0])
	}
	switch typed := value.(type) {
	case string:
		return typed, true
	case nil:
		return "", true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		return formatTerminalCommandNumber(typed), true
	}
	return "", false
}

// validateGiftCardBody mirrors validateAJVObject for the gift-card schemas.
// AJV's coerceTypes turns a null root value into {} for `type: 'object'`, so an
// absent body surfaces as the first `required` failure rather than "must be
// object" — matching Fastify exactly.
func validateGiftCardBody(decoded any, required []string, fields []giftCardFieldSpec) (giftCardBodyValues, string) {
	out := giftCardBodyValues{Values: map[string]any{}, Present: map[string]bool{}}

	if decoded == nil {
		decoded = map[string]any{}
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return out, "must be object"
	}
	for _, name := range required {
		if _, present := object[name]; !present {
			return out, name + " is required"
		}
	}
	for _, field := range fields {
		value, present := object[field.Name]
		if !present {
			continue
		}
		coerced, message := coerceGiftCardField(field, value)
		if message != "" {
			return out, message
		}
		out.Present[field.Name] = true
		out.Values[field.Name] = coerced
	}
	return out, ""
}

func coerceGiftCardField(field giftCardFieldSpec, value any) (any, string) {
	switch field.Kind {
	case giftCardKindString:
		text, ok := ajvStringValue(value)
		if !ok {
			return nil, "must be string"
		}
		if field.HasMax && jsStringLength(text) > field.MaxLength {
			return nil, field.Name + " is too long"
		}
		if field.MinLength > 0 && jsStringLength(text) < field.MinLength {
			return nil, field.Name + " is too short"
		}
		return text, ""

	case giftCardKindNullableString:
		if value == nil {
			return nil, ""
		}
		if array, ok := value.([]any); ok && len(array) == 1 {
			if array[0] == nil {
				return nil, ""
			}
		}
		text, ok := ajvStringValue(value)
		if !ok {
			return nil, "must be string,null"
		}
		if field.HasMax && jsStringLength(text) > field.MaxLength {
			return nil, field.Name + " is too long"
		}
		return text, ""

	case giftCardKindInteger:
		number, ok := ajvIntegerValue(value)
		if !ok {
			return nil, "must be integer"
		}
		if message := giftCardBoundsMessage(field, number); message != "" {
			return nil, message
		}
		return number, ""

	case giftCardKindNumber:
		number, ok := ajvNumberValue(value)
		if !ok {
			return nil, "must be number"
		}
		if message := giftCardBoundsMessage(field, number); message != "" {
			return nil, message
		}
		return number, ""
	}
	return nil, "must be valid"
}

// giftCardBoundsMessage mirrors AJV's maximum-then-minimum checks (inclusive).
func giftCardBoundsMessage(field giftCardFieldSpec, number float64) string {
	if field.HasMax && number > field.Maximum {
		return "must be <= " + formatTerminalCommandNumber(field.Maximum)
	}
	if field.HasMin && number < field.Minimum {
		return "must be >= " + formatTerminalCommandNumber(field.Minimum)
	}
	return ""
}

// ==================== request helpers ====================

// readGiftCardJSONBody reproduces Fastify's body handling for a bodywith method:
//
//   - no Content-Type and no payload     → no body parsed (value is nil)
//   - no Content-Type but a payload      → 415 "Unsupported Media Type: undefined"
//   - application/json, empty payload    → 400 FST_ERR_CTP_EMPTY_JSON_BODY
//   - application/json, malformed body   → 400 FST_ERR_CTP_INVALID_JSON_BODY
//   - text/plain                         → the raw string
//   - any other media type               → 415
//
// It also re-arms the request body so the Turnstile preHandler (which binds the
// body again) still sees the payload.
func readGiftCardJSONBody(c *gin.Context) (any, int, string) {
	contentType := c.GetHeader("Content-Type")
	if contentType == "" {
		if c.Request.ContentLength > 0 || c.GetHeader("Transfer-Encoding") != "" {
			return nil, http.StatusUnsupportedMediaType, fastifyUndefinedMediaTypeMessage
		}
		// Nothing to parse: Fastify leaves request.body undefined.
		return nil, 0, ""
	}

	lowered := strings.ToLower(contentType)
	switch {
	case mediaTypeIs(lowered, "application/json"):
		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return nil, http.StatusBadRequest, fastifyInvalidJSONBodyMessage
		}
		// Restore the stream for downstream body readers (Turnstile).
		c.Request.Body = io.NopCloser(bytes.NewReader(data))

		if len(data) == 0 {
			return nil, http.StatusBadRequest, fastifyEmptyJSONBodyMessage
		}
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			return nil, http.StatusBadRequest, fastifyInvalidJSONBodyMessage
		}
		// secure-json-parse runs with protoAction/constructorAction 'error'.
		if hasForbiddenPrototypeKey(decoded) {
			return nil, http.StatusBadRequest, fastifyInvalidJSONBodyMessage
		}
		return decoded, 0, ""

	case mediaTypeIs(lowered, "text/plain"):
		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return "", 0, ""
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(data))
		return string(data), 0, ""

	default:
		return nil, http.StatusUnsupportedMediaType, "Unsupported Media Type: " + contentType
	}
}

// parsePositiveMoney mirrors parsePositiveMoney: a finite number > 0, rounded to
// 2 decimals, rejected above MAX_FACE_VALUE.
func parsePositiveMoney(value any) (float64, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 {
		return 0, false
	}
	normalized := math.Round(number*100) / 100
	if normalized > db.GiftCardMaxFaceValue {
		return 0, false
	}
	return normalized, true
}

// parseClampedPositiveInteger mirrors parseClampedPositiveInteger.
func parseClampedPositiveInteger(value any, fallback, max int) int {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) || number < 1 {
		return fallback
	}
	converted := int(number)
	if converted > max {
		return max
	}
	return converted
}

// normalizeOptionalString mirrors normalizeOptionalString (returns nil for
// "undefined").
func normalizeOptionalString(value any, maxLength int) *string {
	if value == nil {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || jsStringLength(trimmed) > maxLength || giftCardControlPattern.MatchString(trimmed) {
		return nil
	}
	return &trimmed
}

// normalizeGiftCardCode mirrors normalizeGiftCardCode.
func normalizeGiftCardCode(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if !giftCardCodePattern.MatchString(trimmed) {
		return nil
	}
	return &trimmed
}

// normalizeGiftCardStatus mirrors normalizeStatus.
func normalizeGiftCardStatus(value any) models.GiftCardStatus {
	text, ok := value.(string)
	if !ok || !giftCardStatuses[text] {
		return ""
	}
	return models.GiftCardStatus(text)
}

// normalizeGiftCardIDList mirrors normalizeIdList: a non-empty array of unique
// positive safe integers, at most MAX_BATCH_COUNT entries.
func normalizeGiftCardIDList(value any) ([]int32, bool) {
	array, ok := value.([]any)
	if !ok || len(array) == 0 || len(array) > db.GiftCardMaxBatchCount {
		return nil, false
	}
	seen := map[int32]bool{}
	ids := make([]int32, 0, len(array))
	for _, item := range array {
		number, ok := item.(float64)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) || number <= 0 {
			return nil, false
		}
		id := int32(number)
		if seen[id] {
			return nil, false
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, true
}

// requireGiftCardManager mirrors the requireGiftCardManager preHandler. It
// returns true when the caller may proceed (otherwise it has already replied).
func requireGiftCardManager(c *gin.Context) bool {
	user := middleware.CurrentUser(c)
	if user == nil || user.Role != "admin" {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrAdminRequired, "")
		return false
	}
	allowlist, err := lib.GetCombinedAdminIDAllowlist(c.Request.Context(),
		"payincus_gift_card_admin_ids", []string{"PAYINCUS_GIFT_CARD_ADMIN_IDS"})
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return false
	}
	if len(allowlist.IDs) > 0 && !allowlist.Contains(int64(user.ID)) && user.Username != "admin" {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden,
			"Gift card management requires gift card admin allowlist")
		return false
	}
	return true
}

// mapGiftCardError mirrors mapGiftCardError. It returns true when it wrote a
// response.
func mapGiftCardError(c *gin.Context, err error) bool {
	switch {
	case errors.Is(err, db.ErrGiftCardNotFound):
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrGiftCardNotFound, "")
	case errors.Is(err, db.ErrGiftCardUsed):
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrGiftCardUsed, "")
	case errors.Is(err, db.ErrGiftCardSelfRedeem):
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrGiftCardSelfRedeem, "")
	case errors.Is(err, db.ErrGiftCardUsed):
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrGiftCardUsed, "")
	case errors.Is(err, db.ErrGiftCardAlreadyUsedByUser):
		// The TypeScript mapGiftCardError has this branch even though the db layer
		// never raises it today; kept so the mapping table stays 1:1.
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrGiftCardAlreadyUsedByUser, "")
	case errors.Is(err, db.ErrGiftCardSelfRedeem):
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrGiftCardSelfRedeem, "")
	case errors.Is(err, db.ErrGiftCardExpired):
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrGiftCardExpired, "")
	case errors.Is(err, db.ErrGiftCardDisabled):
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrGiftCardDisabled, "")
	case errors.Is(err, db.ErrGiftCardInsufficientBalance):
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrGiftCardInsufficientBalance, "")
	case errors.Is(err, db.ErrGiftCardBatchTooLarge):
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrGiftCardBatchTooLarge, "")
	case errors.Is(err, db.ErrGiftCardBalanceExceedsFace):
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams,
			"Balance value cannot exceed face value")
	case errors.Is(err, db.ErrGiftCardBatchValueTooLarge):
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams,
			"Gift card batch total face value is too large")
	case errors.Is(err, db.ErrGiftCardNotIssuer):
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden,
			"Only the issuer can revoke this gift card")
	default:
		// USER_NOT_FOUND has no branch in the TypeScript mapGiftCardError, so it
		// falls through to the handler's 500 INTERNAL_ERROR. Keep it unmapped.
		return false
	}
	return true
}

// parseGiftCardExpiresAt mirrors `const d = new Date(raw); Number.isNaN(...)`.
// Accepts the ISO/date-time forms the Go port supports everywhere else.
func parseGiftCardExpiresAt(raw string) (*time.Time, bool) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			utc := parsed.UTC().Truncate(time.Millisecond)
			return &utc, true
		}
	}
	return nil, false
}

func truncateGiftCardCode(code string) string {
	if len(code) <= 10 {
		return code
	}
	return code[:10]
}

// respondGiftCardInternal mirrors the *global* Fastify error handler for the
// routes whose TypeScript handler has no try/catch (admin/list, admin/stats,
// enable/disable/delete, batch-*, user/mine). Those handlers let the error
// propagate, so the body is `{ error: "Internal server error" }` — with no `code`
// field, unlike apiError(INTERNAL_ERROR).
func respondGiftCardInternal(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// ==================== responses ====================

// giftCardBatchCode mirrors the inline mapping in POST /admin/batch: only the
// four fields below are sent, with the amounts as numbers.
type giftCardBatchCode struct {
	ID           int32   `json:"id"`
	Code         string  `json:"code"`
	FaceValue    float64 `json:"faceValue"`
	BalanceValue float64 `json:"balanceValue"`
}

// ==================== route ====================

// RegisterGiftCards mounts /api/gift-cards.
func RegisterGiftCards(r *gin.Engine) {
	group := r.Group("/api/gift-cards")

	// ==================== 管理员 ====================

	// POST /admin/generate — 生成单个兑换码
	group.POST("/admin/generate", middleware.Authenticate(), middleware.RequireAdmin(), func(c *gin.Context) {
		if !requireGiftCardManager(c) {
			return
		}
		ctx := c.Request.Context()
		user := middleware.CurrentUser(c)

		decoded, status, message := readGiftCardJSONBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}
		body, validationMessage := validateGiftCardBody(decoded, []string{"faceValue"}, giftCardAdminFields)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		rawFaceValue, _ := body.Values["faceValue"]
		faceValue, ok := parsePositiveMoney(rawFaceValue)
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid face value")
			return
		}

		balanceValue := faceValue
		if rawBalance, present := body.Values["balanceValue"]; present {
			parsed, ok := parsePositiveMoney(rawBalance)
			if !ok {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid balance value")
				return
			}
			balanceValue = parsed
		}
		if balanceValue > faceValue {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams,
				"Balance value cannot exceed face value")
			return
		}

		var expiresAt *time.Time
		if raw := normalizeOptionalString(body.Values["expiresAt"], 64); raw != nil {
			parsed, ok := parseGiftCardExpiresAt(*raw)
			if !ok {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid expiry date")
				return
			}
			expiresAt = parsed
		}
		remark := normalizeOptionalString(body.Values["remark"], db.GiftCardMaxRemarkLength)

		createdByID := user.ID
		giftCard, err := db.CreateGiftCard(ctx, db.GiftCardCreateInput{
			FaceValue:    faceValue,
			BalanceValue: balanceValue,
			CreatedByID:  &createdByID,
			ExpiresAt:    expiresAt,
			Remark:       remark,
		})
		if err != nil {
			if mapGiftCardError(c, err) {
				return
			}
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, err.Error())
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, giftCardLogModule, "admin_generate",
			"Generated gift card "+truncateGiftCardCode(giftCard.Code)+"..., face="+
				formatTerminalCommandNumber(faceValue)+" balance="+
				formatTerminalCommandNumber(balanceValue),
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusCreated, gin.H{"giftCard": giftCard})
	})

	// POST /admin/batch — 批量生成
	group.POST("/admin/batch", middleware.Authenticate(), middleware.RequireAdmin(), func(c *gin.Context) {
		if !requireGiftCardManager(c) {
			return
		}
		ctx := c.Request.Context()
		user := middleware.CurrentUser(c)

		decoded, status, message := readGiftCardJSONBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}
		body, validationMessage := validateGiftCardBody(decoded, []string{"faceValue", "count"}, giftCardBatchFields)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		rawFaceValue, _ := body.Values["faceValue"]
		faceValue, ok := parsePositiveMoney(rawFaceValue)
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid face value")
			return
		}

		balanceValue := faceValue
		if rawBalance, present := body.Values["balanceValue"]; present {
			parsed, ok := parsePositiveMoney(rawBalance)
			if !ok {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid balance value")
				return
			}
			balanceValue = parsed
		}
		if balanceValue > faceValue {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams,
				"Balance value cannot exceed face value")
			return
		}

		count := parseClampedPositiveInteger(body.Values["count"], 1, db.GiftCardMaxBatchCount)
		if faceValue*float64(count) > db.GiftCardMaxBatchFaceValue {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams,
				"Gift card batch total face value is too large")
			return
		}

		var expiresAt *time.Time
		if raw := normalizeOptionalString(body.Values["expiresAt"], 64); raw != nil {
			parsed, ok := parseGiftCardExpiresAt(*raw)
			if !ok {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid expiry date")
				return
			}
			expiresAt = parsed
		}
		remark := normalizeOptionalString(body.Values["remark"], db.GiftCardMaxRemarkLength)

		createdByID := user.ID
		result, err := db.CreateGiftCardBatch(ctx, count, db.GiftCardCreateInput{
			FaceValue:    faceValue,
			BalanceValue: balanceValue,
			CreatedByID:  &createdByID,
			ExpiresAt:    expiresAt,
			Remark:       remark,
		})
		if err != nil {
			if mapGiftCardError(c, err) {
				return
			}
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, err.Error())
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, giftCardLogModule, "admin_batch_generate",
			"Batch generated "+strconv.Itoa(result.Count)+" gift cards ("+result.BatchID+
				"), face="+formatTerminalCommandNumber(faceValue),
			db.LogResultSuccess, db.CreateLogOptions{})

		codes := make([]giftCardBatchCode, 0, len(result.Codes))
		for _, code := range result.Codes {
			codes = append(codes, giftCardBatchCode{
				ID:           code.ID,
				Code:         code.Code,
				FaceValue:    code.FaceValue,
				BalanceValue: code.BalanceValue,
			})
		}

		c.JSON(http.StatusCreated, gin.H{
			"batchId": result.BatchID,
			"count":   result.Count,
			"codes":   codes,
		})
	})

	// GET /admin/list
	group.GET("/admin/list", middleware.Authenticate(), middleware.RequireAdmin(), func(c *gin.Context) {
		if !requireGiftCardManager(c) {
			return
		}

		query := c.Request.URL.Query()
		page, pageSize, status, batchID, revealCode, validationMessage :=
			validateGiftCardAdminListQuery(query)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		result, err := db.ListGiftCardsByAdmin(c.Request.Context(), db.GiftCardAdminListOptions{
			Page:       page,
			PageSize:   pageSize,
			Status:     status,
			BatchID:    batchID,
			RevealCode: revealCode,
		})
		if err != nil {
			respondGiftCardInternal(c)
			return
		}
		c.JSON(http.StatusOK, result)
	})

	// GET /admin/stats
	group.GET("/admin/stats", middleware.Authenticate(), middleware.RequireAdmin(), func(c *gin.Context) {
		if !requireGiftCardManager(c) {
			return
		}
		stats, err := db.GetGiftCardStats(c.Request.Context())
		if err != nil {
			respondGiftCardInternal(c)
			return
		}
		c.JSON(http.StatusOK, stats)
	})

	// PATCH /admin/:id/enable
	group.PATCH("/admin/:id/enable", middleware.Authenticate(), middleware.RequireAdmin(), func(c *gin.Context) {
		if !requireGiftCardManager(c) {
			return
		}
		id, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		result, err := db.EnableGiftCard(c.Request.Context(), int32(id))
		if err != nil {
			respondGiftCardInternal(c)
			return
		}
		if result.Count == 0 {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrGiftCardNotFound, "")
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	// PATCH /admin/:id/disable
	group.PATCH("/admin/:id/disable", middleware.Authenticate(), middleware.RequireAdmin(), func(c *gin.Context) {
		if !requireGiftCardManager(c) {
			return
		}
		id, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		result, err := db.DisableGiftCard(c.Request.Context(), int32(id))
		if err != nil {
			respondGiftCardInternal(c)
			return
		}
		if result.Count == 0 {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrGiftCardNotFound, "")
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	// DELETE /admin/:id
	group.DELETE("/admin/:id", middleware.Authenticate(), middleware.RequireAdmin(), func(c *gin.Context) {
		if !requireGiftCardManager(c) {
			return
		}
		// DELETE is a bodywith method in Fastify.
		if _, status, message := readGiftCardJSONBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}
		id, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		result, err := db.DeleteGiftCard(c.Request.Context(), int32(id))
		if err != nil {
			respondGiftCardInternal(c)
			return
		}
		if result.Count == 0 {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrGiftCardNotFound, "")
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	// POST /admin/batch-disable
	group.POST("/admin/batch-disable", middleware.Authenticate(), middleware.RequireAdmin(), func(c *gin.Context) {
		if !requireGiftCardManager(c) {
			return
		}
		ids, valid, status, ajvMessage, bodyMessage := decodeGiftCardIDList(c)
		if !valid {
			if status != 0 {
				c.JSON(status, gin.H{"error": bodyMessage})
				return
			}
			if ajvMessage != "" {
				respondAJVValidation(c, ajvMessage)
				return
			}
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid ids")
			return
		}
		result, err := db.BatchDisableGiftCards(c.Request.Context(), ids)
		if err != nil {
			respondGiftCardInternal(c)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "count": result.Count})
	})

	// POST /admin/batch-delete
	group.POST("/admin/batch-delete", middleware.Authenticate(), middleware.RequireAdmin(), func(c *gin.Context) {
		if !requireGiftCardManager(c) {
			return
		}
		ids, valid, status, ajvMessage, bodyMessage := decodeGiftCardIDList(c)
		if !valid {
			if status != 0 {
				c.JSON(status, gin.H{"error": bodyMessage})
				return
			}
			if ajvMessage != "" {
				respondAJVValidation(c, ajvMessage)
				return
			}
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid ids")
			return
		}
		result, err := db.BatchDeleteGiftCards(c.Request.Context(), ids)
		if err != nil {
			respondGiftCardInternal(c)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "count": result.Count})
	})

	// ==================== 用户 ====================

	// POST /user/redeem — 兑换码充值
	group.POST("/user/redeem", middleware.Authenticate(), func(c *gin.Context) {
		ctx := c.Request.Context()
		user := middleware.CurrentUser(c)

		decoded, status, message := readGiftCardJSONBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}
		body, validationMessage := validateGiftCardBody(decoded, []string{"code"}, []giftCardFieldSpec{
			{Name: "code", Kind: giftCardKindString, MinLength: 1, HasMax: true, MaxLength: 64},
			{Name: "turnstileToken", Kind: giftCardKindString},
		})
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		// Fastify runs preHandler (Turnstile) after schema validation.
		if !runGiftCardTurnstile(c) {
			return
		}

		rawCode, _ := body.Values["code"]
		code := normalizeGiftCardCode(rawCode)
		if code == nil {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrGiftCardInvalidCode, "")
			return
		}

		result, err := db.RedeemGiftCard(ctx, *code, user.ID)
		if err != nil {
			if mapGiftCardError(c, err) {
				return
			}
			userID := int64(user.ID)
			db.CreateLog(ctx, &userID, giftCardLogModule, "redeem.failed",
				"Failed redemption of "+truncateGiftCardCode(*code)+"...: "+err.Error(),
				db.LogResultFailed, db.CreateLogOptions{})
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, err.Error())
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, giftCardLogModule, "redeem",
			"Redeemed gift card "+truncateGiftCardCode(*code)+"..., +"+
				formatTerminalCommandNumber(result.BalanceValue),
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{
			"success":       true,
			"amount":        result.BalanceValue,
			"balanceBefore": result.BalanceBefore,
			"balanceAfter":  result.BalanceAfter,
		})
	})

	// POST /user/generate — 用余额生成兑换码
	group.POST("/user/generate", middleware.Authenticate(), func(c *gin.Context) {
		ctx := c.Request.Context()
		user := middleware.CurrentUser(c)

		decoded, status, message := readGiftCardJSONBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}
		body, validationMessage := validateGiftCardBody(decoded, []string{"faceValue"}, []giftCardFieldSpec{
			{Name: "faceValue", Kind: giftCardKindNumber, HasMin: true, Minimum: 0.01, HasMax: true, Maximum: db.GiftCardMaxFaceValue},
			{Name: "remark", Kind: giftCardKindString, HasMax: true, MaxLength: db.GiftCardMaxRemarkLength},
			{Name: "turnstileToken", Kind: giftCardKindString},
		})
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		if !runGiftCardTurnstile(c) {
			return
		}

		rawFaceValue, _ := body.Values["faceValue"]
		faceValue, ok := parsePositiveMoney(rawFaceValue)
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid face value")
			return
		}
		remark := normalizeOptionalString(body.Values["remark"], db.GiftCardMaxRemarkLength)

		result, err := db.GenerateGiftCardFromBalance(ctx, user.ID, faceValue, remark)
		if err != nil {
			if mapGiftCardError(c, err) {
				return
			}
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, err.Error())
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, giftCardLogModule, "user_generate",
			"Generated gift card "+truncateGiftCardCode(result.GiftCard.Code)+"..., face="+
				formatTerminalCommandNumber(faceValue),
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusCreated, gin.H{
			"giftCard":   result.GiftCard,
			"newBalance": result.BalanceAfter,
		})
	})

	// POST /user/:id/revoke — 撤销未兑换的余额礼品卡并退款（无 Turnstile）
	group.POST("/user/:id/revoke", middleware.Authenticate(), func(c *gin.Context) {
		ctx := c.Request.Context()
		user := middleware.CurrentUser(c)

		if _, status, message := readGiftCardJSONBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		id, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		result, err := db.RevokeGiftCard(ctx, int32(id), user.ID)
		if err != nil {
			if mapGiftCardError(c, err) {
				return
			}
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, err.Error())
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, giftCardLogModule, "revoke",
			"Revoked gift card id="+c.Param("id"), db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, result)
	})

	// GET /user/mine — 我的兑换码
	group.GET("/user/mine", middleware.Authenticate(), func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		query := c.Request.URL.Query()
		page, pageSize, status, validationMessage := validateGiftCardUserListQuery(query)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		result, err := db.ListGiftCardsByUser(c.Request.Context(), user.ID, db.GiftCardUserListOptions{
			Page:     page,
			PageSize: pageSize,
			Status:   status,
		})
		if err != nil {
			respondGiftCardInternal(c)
			return
		}
		c.JSON(http.StatusOK, result)
	})
}

// runGiftCardTurnstile runs the (already-ported) Turnstile verifier inline so it
// sits between validation and the handler, exactly like a Fastify preHandler.
func runGiftCardTurnstile(c *gin.Context) bool {
	giftCardTurnstileVerifier(c)
	return !c.IsAborted()
}

// giftCardTurnstileVerifier mirrors `const turnstileRequired =
// createTurnstileVerifier(true)` in the route module.
var giftCardTurnstileVerifier = lib.CreateTurnstileVerifier(true)

// ==================== query validation ====================

// validateGiftCardAdminListQuery mirrors the admin/list querystring schema
// (page, pageSize, status, batchId, revealCode) including AJV defaults.
func validateGiftCardAdminListQuery(query map[string][]string) (int, int, models.GiftCardStatus, *string, bool, string) {
	page, message := ajvQueryInteger(query, "page", 1, 1, 0)
	if message != "" {
		return 0, 0, "", nil, false, message
	}
	pageSize, message := ajvQueryInteger(query, "pageSize", 20, 1, 100)
	if message != "" {
		return 0, 0, "", nil, false, message
	}

	var status models.GiftCardStatus
	if raw, present := query["status"]; present {
		value := firstOrEmpty(raw)
		if !giftCardStatuses[value] {
			return 0, 0, "", nil, false, "must be equal to one of the allowed values"
		}
		status = models.GiftCardStatus(value)
	}

	var batchID *string
	if _, present := query["batchId"]; present {
		batchID = normalizeOptionalString(firstOrEmpty(query["batchId"]), 64)
	}

	revealCode := false
	if raw, present := query["revealCode"]; present {
		parsed, ok := ajvQueryBoolean(firstOrEmpty(raw))
		if !ok {
			return 0, 0, "", nil, false, "must be boolean"
		}
		revealCode = parsed
	}

	return parseClampedPositiveInteger(float64(page), 1, 999999),
		parseClampedPositiveInteger(float64(pageSize), 20, 100),
		status, batchID, revealCode, ""
}

// validateGiftCardUserListQuery mirrors the user/mine querystring schema.
func validateGiftCardUserListQuery(query map[string][]string) (int, int, models.GiftCardStatus, string) {
	page, message := ajvQueryInteger(query, "page", 1, 1, 0)
	if message != "" {
		return 0, 0, "", message
	}
	pageSize, message := ajvQueryInteger(query, "pageSize", 20, 1, 100)
	if message != "" {
		return 0, 0, "", message
	}

	var status models.GiftCardStatus
	if raw, present := query["status"]; present {
		value := firstOrEmpty(raw)
		if !giftCardStatuses[value] {
			return 0, 0, "", "must be equal to one of the allowed values"
		}
		status = models.GiftCardStatus(value)
	}

	return parseClampedPositiveInteger(float64(page), 1, 999999),
		parseClampedPositiveInteger(float64(pageSize), 20, 100),
		status, ""
}

// ajvQueryInteger mirrors `type: 'integer'` + minimum/maximum for a querystring
// value, applying the schema default when the key is absent. max 0 means "no
// maximum".
func ajvQueryInteger(query map[string][]string, name string, fallback, minimum, maximum int) (int, string) {
	raw, present := query[name]
	if !present {
		return fallback, ""
	}
	value := firstOrEmpty(raw)
	number, ok := ajvIntegerValue(value)
	if !ok {
		return 0, "must be integer"
	}
	if number < float64(minimum) {
		return 0, "must be >= " + formatTerminalCommandNumber(float64(minimum))
	}
	if maximum > 0 && number > float64(maximum) {
		return 0, "must be <= " + formatTerminalCommandNumber(float64(maximum))
	}
	return int(number), ""
}

// ajvQueryBoolean mirrors `type: 'boolean'` coercion for a querystring value.
func ajvQueryBoolean(raw string) (bool, bool) {
	switch raw {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// decodeGiftCardIDList decodes and validates the `{ ids: number[] }` body shared
// by batch-disable / batch-delete against the real schema:
//
//	ids: { type: 'array', items: { type: 'integer', minimum: 1 }, minItems: 1, maxItems: 500 }
//
// Return contract — the AJV branch returns the message instead of writing a
// response, because the caller has to choose between the AJV envelope and the
// INVALID_PARAMS one:
//   - (ids, true, 0, "", "")        → proceed
//   - (nil, false, 0, msg, "")      → AJV validation failure → 400 {error: msg,
//     code: VALIDATION_ERROR} via respondAJVValidation
//   - (nil, false, status, "", msg) → body-parser error → caller writes {error: msg}
//
// The previous version wrote the AJV response inline AND returned "not valid",
// so the caller appended a second INVALID_PARAMS envelope to the same socket
// (two concatenated JSON objects). The dual-run harness caught it.
func decodeGiftCardIDList(c *gin.Context) (ids []int32, valid bool, status int, ajvMessage string, bodyMessage string) {
	decoded, status, bodyMessage := readGiftCardJSONBody(c)
	if status != 0 {
		return nil, false, status, "", bodyMessage
	}

	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, false, 0, "must be object", ""
	}
	rawIDs, present := object["ids"]
	if !present {
		return nil, false, 0, "ids is required", ""
	}

	// AJV's coerceTypes:'array' wraps a bare scalar into a one-element array, so
	// {"ids":"x"} reports "must be integer" rather than "must be array"
	// (verified against the live Node server).
	array, isArray := rawIDs.([]any)
	if !isArray {
		array = []any{rawIDs}
	}

	if len(array) < 1 {
		return nil, false, 0, "must NOT have fewer than 1 items", ""
	}
	if len(array) > db.GiftCardMaxBatchCount {
		return nil, false, 0, "must NOT have more than 500 items", ""
	}
	for _, item := range array {
		number, ok := ajvIntegerValue(item)
		if !ok {
			return nil, false, 0, "must be integer", ""
		}
		if number < 1 {
			return nil, false, 0, "must be >= 1", ""
		}
	}

	ids, ok = normalizeGiftCardIDList(array)
	if !ok {
		// Structurally valid but rejected by normalizeIdList: duplicates, or an
		// id beyond int32 (Prisma would raise P2020). Node answers INVALID_PARAMS.
		return nil, false, 0, "", ""
	}
	return ids, true, 0, "", ""
}

func init() {
	Register(RegisterGiftCards)
}
