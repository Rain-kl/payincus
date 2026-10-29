// Port of server/src/routes/redeem-codes.ts.
//
// Host owners manage the h-codes ("h-" prefix) of their own nodes. app.ts
// registers this module as `fastify.register(redeemCodesRoutes, { prefix: '/api' })`
// and every path in the TypeScript file is absolute, so the routes below are
// absolute too: /api/hosts/:hostId/redeem-codes[/...] and /api/redeem-code-options.
// Every route carries `onRequest: [fastify.authenticate]`.
//
// The Fastify pipeline is reproduced explicitly because the response contract
// depends on *which* layer fails:
//
//   - params/querystring/body JSON-schema validation -> 400
//     `{error: <message>, code: 'VALIDATION_ERROR'}` (no details), using
//     @fastify/ajv-compiler's defaults: coerceTypes:'array', useDefaults:true,
//     allErrors:false. Messages come from the setErrorHandler branches in app.ts
//     (`X is required`, `X is too long`, `must be integer`, `must be >= 1`,
//     `must NOT have more than 100 items`, `X format is invalid`, ...). AJV
//     *replaces* request.body with the coerced value, so the normalizers below
//     read the validated map, never the raw decode.
//   - the module's own normalizers -> `apiError(INVALID_PARAMS, message)`, i.e.
//     `{error:'Invalid parameters', code:'INVALID_PARAMS', details:<message>}`.
//   - `parsePositiveId` (route-local) is `^\d+$` plus the JS-safe-integer range;
//     unlike lib.ParsePositiveRouteID it accepts leading zeros ("01" -> 1), which
//     the guarded TypeScript helper also does.
package routes

import (
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// Route-local bounds, mirroring the TypeScript constants.
const (
	maxRedeemCodeBatchCount   = 100
	maxRedeemCodeMaxUses      = 1000
	maxRedeemCodeRemarkLength = 200
)

// redeemCodeTypes mirrors REDEEM_CODE_TYPES. The deprecated `p` (points) type is
// deliberately excluded; the schema enum and the normalizer both reject it.
var redeemCodeTypes = []string{"c", "r", "d", "t"}

// redeemCodeTypeNames / redeemCodeTypeUnits mirror CODE_TYPE_NAMES / CODE_TYPE_UNITS.
var (
	redeemCodeTypeNames = map[string]string{
		"c": "CPU",
		"r": "Memory",
		"d": "Disk",
		"t": "Traffic",
	}
	redeemCodeTypeUnits = map[string]string{
		"c": "%",
		"r": "MB",
		"d": "MB",
		"t": "GB",
	}
)

// redeemCodeParamsPattern mirrors the params schema pattern '^\\d+$'.
var redeemCodeParamsPattern = regexp.MustCompile(`^[0-9]+$`)

// redeemCodeControlPattern mirrors /[\u0000-\u001F\u007F]/.
var redeemCodeControlPattern = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// ==================== helpers ====================

// parseRedeemCodePositiveID mirrors parsePositiveId: `^\d+$`, then a JS-safe
// integer strictly greater than zero.
func parseRedeemCodePositiveID(value string) (int64, bool) {
	if !redeemCodeParamsPattern.MatchString(value) {
		return 0, false
	}

	var parsed int64
	for _, char := range value {
		parsed = parsed*10 + int64(char-'0')
		if parsed > 1<<53-1 {
			return 0, false
		}
	}
	if parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

// redeemCodeJSInteger mirrors `typeof value === 'number' && Number.isSafeInteger(value)`.
func redeemCodeJSInteger(value any) (int64, bool) {
	number, ok := value.(float64)
	if !ok {
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) || math.Abs(number) > 1<<53-1 {
		return 0, false
	}
	return int64(number), true
}

// redeemCodeAPIParamsError mirrors `reply.code(400).send(apiError(INVALID_PARAMS, msg))`.
func redeemCodeAPIParamsError(c *gin.Context, message string) {
	lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, message)
}

// redeemCodeQueryValue reproduces fast-querystring's shape: a repeated key becomes
// an array of strings, a single key stays a string, an absent key is undefined.
func redeemCodeQueryValue(query url.Values, key string) (any, bool) {
	values, present := query[key]
	if !present {
		return nil, false
	}
	if len(values) == 1 {
		return values[0], true
	}
	items := make([]any, 0, len(values))
	for _, value := range values {
		items = append(items, value)
	}
	return items, true
}

// redeemCodeQueryInteger applies AJV's integer coercion, bounds and `default` to
// one querystring property. A NaN bound means "no bound". The second return value
// is the AJV message ("" on success).
func redeemCodeQueryInteger(query url.Values, key string, defaultValue int, minimum, maximum float64) (int, string) {
	raw, present := redeemCodeQueryValue(query, key)
	if !present {
		return defaultValue, ""
	}

	number, ok := ajvCoerceInteger(raw)
	if !ok {
		return 0, "must be integer"
	}
	if math.IsInf(number, 0) || math.IsNaN(number) {
		// AJV skips the numeric bounds for non-finite values.
		return defaultValue, ""
	}
	if !math.IsNaN(maximum) && number > maximum {
		return 0, "must be <= " + formatTerminalCommandNumber(maximum)
	}
	if !math.IsNaN(minimum) && number < minimum {
		return 0, "must be >= " + formatTerminalCommandNumber(minimum)
	}
	// `offset` declares no maximum, so AJV accepts a huge value and Prisma receives
	// it as `skip`. Clamp instead of overflowing Go's int, which would turn the
	// OFFSET negative.
	if number > math.MaxInt32 {
		return math.MaxInt32, ""
	}
	return int(number), ""
}

// redeemCodeQueryEnum applies AJV's string coercion plus an enum check.
func redeemCodeQueryEnum(query url.Values, key string, allowed []string) (string, bool, string) {
	raw, present := redeemCodeQueryValue(query, key)
	if !present {
		return "", false, ""
	}
	coerced, message := ajvCoerceString(raw)
	if message != "" {
		return "", false, message
	}
	if !ajvEnumContains(allowed, coerced) {
		return "", false, "must be equal to one of the allowed values"
	}
	return coerced, true, ""
}

// unwrapAJVRootArray mirrors AJV's root-level `coerceTypes:'array'` step: a
// single-element array is replaced by its element before the root type test, so
// `[{"a":1}]` validates as the object `{"a":1}` (real Fastify behaviour).
func unwrapAJVRootArray(decoded any) any {
	source, unwrapped := ajvUnwrapSingleArray(decoded)
	if unwrapped {
		return source
	}
	return decoded
}

// redeemCodeOptionalExpiry mirrors normalizeOptionalExpiryDate. It returns
// (value, valid): (nil, true) for "no expiry", (nil, false) for an unparseable
// date.
func redeemCodeOptionalExpiry(text *string, present bool) (*time.Time, bool) {
	if !present || text == nil {
		return nil, true
	}
	if strings.TrimSpace(*text) == "" {
		return nil, true
	}

	parsed, ok := parseAPITokenJSDatetime(*text)
	if !ok {
		return nil, false
	}
	utc := parsed.UTC().Truncate(time.Millisecond)
	return &utc, true
}

// redeemCodeOptionalRemark mirrors normalizeOptionalRedeemCodeRemark.
func redeemCodeOptionalRemark(text string, present bool) (*string, bool) {
	if !present {
		return nil, true
	}
	trimmed := strings.TrimSpace(text)
	if utf8.RuneCountInString(trimmed) > maxRedeemCodeRemarkLength || redeemCodeControlPattern.MatchString(trimmed) {
		return nil, false
	}
	if trimmed == "" {
		return nil, true
	}
	return &trimmed, true
}

// requireRedeemCodeInteger mirrors requireRedeemCodeInteger.
func requireRedeemCodeInteger(value any, field string, min, max int64) (int64, string) {
	number, ok := redeemCodeJSInteger(value)
	if !ok {
		return 0, field + " must be an integer"
	}
	if number < min || number > max {
		return 0, field + " must be between " + formatTerminalCommandNumber(float64(min)) +
			" and " + formatTerminalCommandNumber(float64(max))
	}
	return number, ""
}

// redeemCodeTypeLabel mirrors `CODE_TYPE_NAMES[codeType] || codeType` and the
// matching unit lookup.
func redeemCodeTypeLabel(codeType string) (string, string) {
	typeName := redeemCodeTypeNames[codeType]
	if typeName == "" {
		typeName = codeType
	}
	return typeName, redeemCodeTypeUnits[codeType]
}

// ==================== body normalizers ====================

// redeemCodeCreateBody mirrors normalizeRedeemCodeCreateBody's result.
type redeemCodeCreateBody struct {
	CodeType    models.RedeemCodeType
	CodeValue   int32
	MaxUses     *int32
	BatchCount  *int32
	ExpiresAt   *string
	ExpiresSeen bool
	Remark      *string
}

// normalizeRedeemCodeCreateBody mirrors normalizeRedeemCodeCreateBody over the
// AJV-validated body. The second return value is the INVALID_PARAMS detail.
func normalizeRedeemCodeCreateBody(body ajvValidated) (redeemCodeCreateBody, string) {
	var out redeemCodeCreateBody

	rawType, _ := body.stringValue("codeType")
	if !ajvEnumContains(redeemCodeTypes, rawType) {
		return out, "Invalid redeem code type"
	}
	codeType := models.RedeemCodeType(rawType)
	out.CodeType = codeType

	codeValueRange := db.CodeValueRanges[codeType]
	codeValue, message := requireRedeemCodeInteger(body.Values["codeValue"],
		"codeValue", int64(codeValueRange.Min), int64(codeValueRange.Max))
	if message != "" {
		return out, message
	}
	out.CodeValue = int32(codeValue)

	if body.Present["maxUses"] {
		parsed, message := requireRedeemCodeInteger(body.Values["maxUses"], "maxUses", 1, maxRedeemCodeMaxUses)
		if message != "" {
			return out, message
		}
		converted := int32(parsed)
		out.MaxUses = &converted
	}

	if body.Present["batchCount"] {
		parsed, message := requireRedeemCodeInteger(body.Values["batchCount"], "batchCount", 1, maxRedeemCodeBatchCount)
		if message != "" {
			return out, message
		}
		converted := int32(parsed)
		out.BatchCount = &converted
	}

	if body.Present["expiresAt"] {
		out.ExpiresSeen = true
		text, _ := body.nullableString("expiresAt")
		out.ExpiresAt = text
	}

	remarkText, remarkPresent := body.stringValue("remark")
	remark, valid := redeemCodeOptionalRemark(remarkText, remarkPresent)
	if !valid {
		return out, "Invalid remark"
	}
	out.Remark = remark

	return out, ""
}

// redeemCodeUpdateBody mirrors normalizeRedeemCodeUpdateBody's result.
type redeemCodeUpdateBody struct {
	Enabled     *bool
	Remark      *string
	RemarkSeen  bool
	MaxUses     *int32
	ExpiresAt   *string
	ExpiresSeen bool
}

// normalizeRedeemCodeUpdateBody mirrors normalizeRedeemCodeUpdateBody.
func normalizeRedeemCodeUpdateBody(body ajvValidated) (redeemCodeUpdateBody, string) {
	var out redeemCodeUpdateBody

	if body.Present["enabled"] {
		flag := body.boolValue("enabled")
		out.Enabled = &flag
	}

	if body.Present["expiresAt"] {
		out.ExpiresSeen = true
		text, _ := body.nullableString("expiresAt")
		out.ExpiresAt = text
	}

	remarkText, remarkPresent := body.stringValue("remark")
	remark, valid := redeemCodeOptionalRemark(remarkText, remarkPresent)
	if !valid {
		return out, "Invalid remark"
	}
	out.Remark = remark
	out.RemarkSeen = remarkPresent

	if body.Present["maxUses"] {
		parsed, message := requireRedeemCodeInteger(body.Values["maxUses"], "maxUses", 1, maxRedeemCodeMaxUses)
		if message != "" {
			return out, message
		}
		converted := int32(parsed)
		out.MaxUses = &converted
	}

	return out, ""
}

// ==================== AJV body schemas ====================

var (
	redeemCodeCreateBodyFields = []ajvFieldSpec{
		{Name: "codeType", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN(), Enum: redeemCodeTypes},
		{Name: "codeValue", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
		{Name: "maxUses", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: maxRedeemCodeMaxUses},
		{Name: "expiresAt", Kind: ajvKindNullableString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "remark", Kind: ajvKindString, MinLength: -1, MaxLength: maxRedeemCodeRemarkLength, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "batchCount", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: maxRedeemCodeBatchCount},
	}

	redeemCodeUpdateBodyFields = []ajvFieldSpec{
		{Name: "enabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "remark", Kind: ajvKindString, MinLength: -1, MaxLength: maxRedeemCodeRemarkLength, Minimum: math.NaN(), Maximum: math.NaN()},
		{Name: "maxUses", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: maxRedeemCodeMaxUses},
		{Name: "expiresAt", Kind: ajvKindNullableString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	}
)

// validateRedeemCodeCreateBodyAJV mirrors the create body schema
// (`required: ['codeType','codeValue']`).
func validateRedeemCodeCreateBodyAJV(decoded any) (ajvValidated, string) {
	return validateAJVObject(unwrapAJVRootArray(decoded), []string{"codeType", "codeValue"}, redeemCodeCreateBodyFields)
}

// validateRedeemCodeUpdateBodyAJV mirrors the update body schema (no required keys).
func validateRedeemCodeUpdateBodyAJV(decoded any) (ajvValidated, string) {
	return validateAJVObject(unwrapAJVRootArray(decoded), nil, redeemCodeUpdateBodyFields)
}

// prepareRedeemCodeBatchDelete mirrors the batch-delete pipeline: AJV first
// (`{ids: {type:'array', items:{type:'integer', minimum:1}, minItems:1,
// maxItems:100}}`), then normalizePositiveUniqueIds, which after AJV can only
// still fail on duplicates.
func prepareRedeemCodeBatchDelete(decoded any) (ids []int64, validationMessage, paramsMessage string) {
	root := unwrapAJVRootArray(decoded)

	object, ok := root.(map[string]any)
	if !ok {
		return nil, "must be object", ""
	}
	value, present := object["ids"]
	if !present {
		return nil, "ids is required", ""
	}

	items, ok := value.([]any)
	if !ok {
		// coerceTypes:'array' wraps a scalar (string/number/boolean/null) in a
		// one-element array; a one-element array of items unwraps to its element,
		// which is then re-tested against `type: 'array'`.
		switch value.(type) {
		case string, float64, bool, nil:
			items = []any{value}
		default:
			return nil, "must be array", ""
		}
	} else if len(items) == 1 {
		if inner, isArray := items[0].([]any); isArray {
			items = inner
		}
	}

	// AJV's `maxItems`/`minItems` are validated before the item keywords.
	if len(items) > 100 {
		return nil, "must NOT have more than 100 items", ""
	}
	if len(items) < 1 {
		return nil, "must NOT have fewer than 1 items", ""
	}

	coerced := make([]int64, 0, len(items))
	seen := make(map[int64]bool, len(items))
	for _, item := range items {
		number, ok := ajvCoerceInteger(item)
		if !ok {
			return nil, "must be integer", ""
		}
		if math.IsInf(number, 0) || math.IsNaN(number) {
			continue
		}
		if number < 1 {
			return nil, "must be >= 1", ""
		}
		integer := int64(number)
		if seen[integer] {
			// normalizePositiveUniqueIds returns null on duplicates.
			return nil, "", "Invalid redeem code IDs"
		}
		seen[integer] = true
		coerced = append(coerced, integer)
	}

	return coerced, "", ""
}

// ==================== shared host guard ====================

// redeemCodeHostAccess mirrors the host lookup repeated by every route:
// 404 HOST_NOT_FOUND when the host is missing, 403 FORBIDDEN when the caller is
// neither its owner nor an admin. It returns the host name and whether the handler
// may continue.
func redeemCodeHostAccess(c *gin.Context, hostID int64, userID int32, role string) (string, bool) {
	host, err := db.GetRedeemCodeHostByID(c.Request.Context(), hostID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return "", false
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return "", false
	}
	if host.UserID != userID && role != "admin" {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return "", false
	}
	return host.Name, true
}

// redeemCodeLogCreate mirrors the createLog calls of the create route; the module
// is the literal 'redeem_code'.
func redeemCodeLogCreate(c *gin.Context, userID int32, action, content, result string) {
	logUserID := int64(userID)
	db.CreateLog(c.Request.Context(), &logUserID, "redeem_code", action, content, result, db.CreateLogOptions{})
}

// ==================== routes ====================

// RegisterRedeemCodes mounts the redeem-code routes under /api (the prefix
// declared in app.ts). All paths below are absolute, matching the TypeScript.
func RegisterRedeemCodes(r *gin.Engine) {
	group := r.Group("/api", middleware.Authenticate())

	// ==================== 获取宿主机的兑换码列表 ====================
	group.GET("/hosts/:hostId/redeem-codes", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		query := c.Request.URL.Query()

		rawHostID := c.Param("hostId")
		if !redeemCodeParamsPattern.MatchString(rawHostID) {
			respondAJVValidation(c, "hostId format is invalid")
			return
		}

		limit, message := redeemCodeQueryInteger(query, "limit", 50, 1, 100)
		if message != "" {
			respondAJVValidation(c, message)
			return
		}
		offset, message := redeemCodeQueryInteger(query, "offset", 0, 0, math.NaN())
		if message != "" {
			respondAJVValidation(c, message)
			return
		}
		enabledRaw, enabledPresent, message := redeemCodeQueryEnum(query, "enabled", []string{"true", "false"})
		if message != "" {
			respondAJVValidation(c, message)
			return
		}

		hostID, ok := parseRedeemCodePositiveID(rawHostID)
		if !ok {
			redeemCodeAPIParamsError(c, "Invalid host ID")
			return
		}
		if _, allowed := redeemCodeHostAccess(c, hostID, user.ID, user.Role); !allowed {
			return
		}

		options := db.RedeemCodeListOptions{Limit: &limit, Offset: &offset}
		if enabledPresent {
			enabled := enabledRaw == "true"
			options.Enabled = &enabled
		}

		result, err := db.GetRedeemCodesByHost(c.Request.Context(), hostID, options)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, result)
	})

	// ==================== 创建兑换码 ====================
	group.POST("/hosts/:hostId/redeem-codes", func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		rawHostID := c.Param("hostId")
		if !redeemCodeParamsPattern.MatchString(rawHostID) {
			respondAJVValidation(c, "hostId format is invalid")
			return
		}

		decoded, status, parseMessage := decodeJSONRequestBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": parseMessage})
			return
		}
		validated, validationMessage := validateRedeemCodeCreateBodyAJV(decoded.Value)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		hostID, hostIDValid := parseRedeemCodePositiveID(rawHostID)

		// The TypeScript normalizes the body *before* it rejects an invalid host id.
		body, normalizeMessage := normalizeRedeemCodeCreateBody(validated)
		if normalizeMessage != "" {
			redeemCodeAPIParamsError(c, normalizeMessage)
			return
		}

		if !hostIDValid {
			redeemCodeAPIParamsError(c, "Invalid host ID")
			return
		}

		hostName, allowed := redeemCodeHostAccess(c, hostID, user.ID, user.Role)
		if !allowed {
			return
		}

		expiresAt, expiresValid := redeemCodeOptionalExpiry(body.ExpiresAt, body.ExpiresSeen)
		if !expiresValid {
			redeemCodeAPIParamsError(c, "Invalid expiry date")
			return
		}

		typeName, unit := redeemCodeTypeLabel(string(body.CodeType))
		hostIDText := formatTerminalCommandNumber(float64(hostID))
		codeValueText := formatTerminalCommandNumber(float64(body.CodeValue))

		if body.BatchCount != nil && *body.BatchCount > 1 {
			// 批量创建一次性兑换码
			created, err := db.CreateRedeemCodeBatch(c.Request.Context(), db.RedeemCodeBatchCreateData{
				HostID:      hostID,
				CreatedByID: int64(user.ID),
				CodeType:    body.CodeType,
				CodeValue:   body.CodeValue,
				Count:       int(*body.BatchCount),
				ExpiresAt:   expiresAt,
				Remark:      body.Remark,
			})
			if err != nil {
				redeemCodeLogCreate(c, user.ID, "create.failed",
					"Failed to create redeem code for host (ID: "+hostIDText+"): "+err.Error(), "failed")
				lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, err.Error())
				return
			}

			batchCountText := formatTerminalCommandNumber(float64(*body.BatchCount))
			redeemCodeLogCreate(c, user.ID, "batch_create",
				"Created "+batchCountText+" redeem codes for host \""+hostName+"\" (ID: "+hostIDText+"): "+
					typeName+" +"+codeValueText+unit+", batch: "+created.BatchID,
				"success")

			c.JSON(http.StatusOK, gin.H{
				"message": "Batch created successfully",
				"codes":   created.Codes,
				"batchId": created.BatchID,
				"count":   *body.BatchCount,
			})
			return
		}

		// 创建单个兑换码（可多次使用）
		maxUses := int32(1)
		if body.MaxUses != nil {
			maxUses = *body.MaxUses
		}

		created, err := db.CreateRedeemCode(c.Request.Context(), db.RedeemCodeCreateData{
			HostID:      hostID,
			CreatedByID: int64(user.ID),
			CodeType:    body.CodeType,
			CodeValue:   body.CodeValue,
			MaxUses:     &maxUses,
			ExpiresAt:   expiresAt,
			Remark:      body.Remark,
		})
		if err != nil {
			redeemCodeLogCreate(c, user.ID, "create.failed",
				"Failed to create redeem code for host (ID: "+hostIDText+"): "+err.Error(), "failed")
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, err.Error())
			return
		}

		redeemCodeLogCreate(c, user.ID, "create",
			"Created redeem code for host \""+hostName+"\" (ID: "+hostIDText+"): "+
				typeName+" +"+codeValueText+unit+", max uses: "+formatTerminalCommandNumber(float64(maxUses)),
			"success")

		c.JSON(http.StatusOK, gin.H{
			"message": "Created successfully",
			"code":    created.Code,
			"id":      created.ID,
		})
	})

	// ==================== 更新兑换码 ====================
	group.PATCH("/hosts/:hostId/redeem-codes/:codeId", func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		rawHostID := c.Param("hostId")
		rawCodeID := c.Param("codeId")
		if !redeemCodeParamsPattern.MatchString(rawHostID) {
			respondAJVValidation(c, "hostId format is invalid")
			return
		}
		if !redeemCodeParamsPattern.MatchString(rawCodeID) {
			respondAJVValidation(c, "codeId format is invalid")
			return
		}

		decoded, status, parseMessage := decodeJSONRequestBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": parseMessage})
			return
		}
		validated, validationMessage := validateRedeemCodeUpdateBodyAJV(decoded.Value)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		hostID, hostIDValid := parseRedeemCodePositiveID(rawHostID)
		codeID, codeIDValid := parseRedeemCodePositiveID(rawCodeID)

		body, normalizeMessage := normalizeRedeemCodeUpdateBody(validated)
		if normalizeMessage != "" {
			redeemCodeAPIParamsError(c, normalizeMessage)
			return
		}

		if !hostIDValid || !codeIDValid {
			redeemCodeAPIParamsError(c, "Invalid host or redeem code ID")
			return
		}

		if _, allowed := redeemCodeHostAccess(c, hostID, user.ID, user.Role); !allowed {
			return
		}

		expiresAt, expiresValid := redeemCodeOptionalExpiry(body.ExpiresAt, body.ExpiresSeen)
		if !expiresValid {
			redeemCodeAPIParamsError(c, "Invalid expiry date")
			return
		}

		update := db.RedeemCodeUpdateData{
			ExpiresAtProvided: body.ExpiresSeen,
			ExpiresAt:         expiresAt,
		}
		if body.Enabled != nil {
			update.EnabledProvided = true
			update.Enabled = *body.Enabled
		}
		if body.RemarkSeen && body.Remark != nil {
			// `remark: undefined` (absent key, or a blank value that normalizes to
			// `trimmed || undefined`) is dropped by Prisma, so the column is left
			// untouched. Only a non-blank remark is written.
			update.RemarkProvided = true
			update.Remark = body.Remark
		}
		if body.MaxUses != nil {
			update.MaxUsesProvided = true
			update.MaxUses = *body.MaxUses
		}

		updated, err := db.UpdateRedeemCodeForHost(c.Request.Context(), hostID, codeID, update)
		if err != nil {
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, err.Error())
			return
		}
		if !updated {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrRedeemCodeNotFound, "")
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "Updated successfully"})
	})

	// ==================== 删除兑换码 ====================
	group.DELETE("/hosts/:hostId/redeem-codes/:codeId", func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		rawHostID := c.Param("hostId")
		rawCodeID := c.Param("codeId")
		if !redeemCodeParamsPattern.MatchString(rawHostID) {
			respondAJVValidation(c, "hostId format is invalid")
			return
		}
		if !redeemCodeParamsPattern.MatchString(rawCodeID) {
			respondAJVValidation(c, "codeId format is invalid")
			return
		}

		hostID, hostIDValid := parseRedeemCodePositiveID(rawHostID)
		codeID, codeIDValid := parseRedeemCodePositiveID(rawCodeID)
		if !hostIDValid || !codeIDValid {
			redeemCodeAPIParamsError(c, "Invalid host or redeem code ID")
			return
		}

		if _, allowed := redeemCodeHostAccess(c, hostID, user.ID, user.Role); !allowed {
			return
		}

		outcome, err := db.DeleteRedeemCodeForHost(c.Request.Context(), hostID, codeID)
		if err != nil {
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, err.Error())
			return
		}
		switch outcome {
		case db.RedeemCodeNotFound:
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrRedeemCodeNotFound, "")
			return
		case db.RedeemCodeDisabled:
			lib.RespondAPIError(c, http.StatusConflict, lib.ErrRedeemCodeUsed,
				"Redeem code has usage records and was disabled instead of deleted")
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "Deleted successfully"})
	})

	// ==================== 批量删除兑换码 ====================
	group.POST("/hosts/:hostId/redeem-codes/batch-delete", func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		rawHostID := c.Param("hostId")
		if !redeemCodeParamsPattern.MatchString(rawHostID) {
			respondAJVValidation(c, "hostId format is invalid")
			return
		}

		decoded, status, parseMessage := decodeJSONRequestBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": parseMessage})
			return
		}

		ids, validationMessage, paramsMessage := prepareRedeemCodeBatchDelete(decoded.Value)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		hostID, hostIDValid := parseRedeemCodePositiveID(rawHostID)
		if !hostIDValid {
			redeemCodeAPIParamsError(c, "Invalid host ID")
			return
		}

		if paramsMessage != "" {
			redeemCodeAPIParamsError(c, paramsMessage)
			return
		}

		if _, allowed := redeemCodeHostAccess(c, hostID, user.ID, user.Role); !allowed {
			return
		}

		result, err := db.DeleteRedeemCodeBatchForHost(c.Request.Context(), hostID, ids)
		if err != nil {
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, err.Error())
			return
		}
		if result.FoundCount != len(ids) {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrRedeemCodeNotFound, "")
			return
		}
		if result.DisabledCount > 0 {
			lib.RespondAPIError(c, http.StatusConflict, lib.ErrRedeemCodeUsed,
				formatTerminalCommandNumber(float64(result.DisabledCount))+
					" redeem code(s) have usage records and were disabled instead of deleted")
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"message": "Batch deleted successfully",
			"count":   result.DeletedCount,
		})
	})

	// ==================== 获取兑换码使用记录 ====================
	group.GET("/hosts/:hostId/redeem-codes/:codeId/usages", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		query := c.Request.URL.Query()

		rawHostID := c.Param("hostId")
		rawCodeID := c.Param("codeId")
		if !redeemCodeParamsPattern.MatchString(rawHostID) {
			respondAJVValidation(c, "hostId format is invalid")
			return
		}
		if !redeemCodeParamsPattern.MatchString(rawCodeID) {
			respondAJVValidation(c, "codeId format is invalid")
			return
		}

		limit, message := redeemCodeQueryInteger(query, "limit", 20, 1, 100)
		if message != "" {
			respondAJVValidation(c, message)
			return
		}
		offset, message := redeemCodeQueryInteger(query, "offset", 0, 0, math.NaN())
		if message != "" {
			respondAJVValidation(c, message)
			return
		}

		hostID, hostIDValid := parseRedeemCodePositiveID(rawHostID)
		codeID, codeIDValid := parseRedeemCodePositiveID(rawCodeID)
		if !hostIDValid || !codeIDValid {
			redeemCodeAPIParamsError(c, "Invalid host or redeem code ID")
			return
		}

		if _, allowed := redeemCodeHostAccess(c, hostID, user.ID, user.Role); !allowed {
			return
		}

		belongsToHost, err := db.IsRedeemCodeBelongsToHost(c.Request.Context(), codeID, hostID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if !belongsToHost {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrRedeemCodeNotFound, "")
			return
		}

		result, err := db.GetRedeemCodeUsagesForHost(c.Request.Context(), hostID, codeID, db.RedeemCodeUsageListOptions{
			Limit:  &limit,
			Offset: &offset,
		})
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, result)
	})

	// ==================== 获取可选的资源类型和范围 ====================
	group.GET("/redeem-code-options", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"types": []gin.H{
				{"value": "c", "label": "CPU", "unit": "%"},
				{"value": "r", "label": "Memory", "unit": "MB"},
				{"value": "d", "label": "Disk", "unit": "MB"},
				{"value": "t", "label": "Traffic", "unit": "GB"},
			},
			"ranges": db.CodeValueRanges,
		})
	})
}

func init() {
	Register(RegisterRedeemCodes)
}
