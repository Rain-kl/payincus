// Port of server/src/routes/storage-configs.ts plus the two pure helpers it pulls
// from modules that are not ported yet:
//
//   - normalizeStorageBasePath / normalizeStorageFilename (server/src/storage/path.ts)
//   - the AJV semantics that Fastify applies to `schema.body`
//     (@fastify/ajv-compiler default-ajv-options.js: coerceTypes:'array',
//     useDefaults:true, removeAdditional:true, allErrors:false). Note that
//     removeAdditional only strips keys when a schema sets
//     `additionalProperties:false`, which these schemas do not, so unknown keys
//     survive validation (the POST handler ignores them; PATCH forwards them to
//     Prisma, which rejects them with a 500).
//
// Security-relevant points, all aligned line by line with the TypeScript:
//
//   - The stored password is AES-GCM encrypted via lib.EncryptSensitiveData at
//     exactly the two write points (create, and only when the update body carries
//     a password). An empty/falsy password stores NULL, never "".
//   - No response ever serializes the password column: every response view type
//     below simply has no Password field.
//   - Every host/type pair is validated with lib.AssertSafeStorageTarget (SSRF
//     guard) before it is written, on create and on update (using the *effective*
//     post-update type/host, not just the fields present in the body).
//   - S3 credentials are normalized away from `extra`: access key goes to
//     `username`, secret key to the encrypted `password`, and `extra` is reduced
//     to {bucket, region, forcePathStyle} so nothing secret lands in JSON.
package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// Storage schema bounds, mirroring the POST/PATCH JSON schemas.
const (
	storageNameMaxLength     = 50
	storageHostMaxLength     = 255
	storageUsernameMaxLength = 100
	storagePasswordMaxLength = 255
	storageBasePathMaxLength = 255

	storagePortMinimum = 1
	storagePortMaximum = 65535

	// storageS3BucketMaxLength / storageS3RegionMaxLength mirror normalizeS3Extra.
	storageS3BucketMaxLength = 128
	storageS3RegionMaxLength = 64
)

// Fastify body-parser messages, surfaced verbatim by app.ts' generic handler.
const (
	fastifyEmptyJSONBodyMessage   = "Body cannot be empty when content-type is set to 'application/json'"
	fastifyInvalidJSONBodyMessage = "Body is not valid JSON but content-type is set to 'application/json'"
	// fastifyUndefinedMediaTypeMessage is what FST_ERR_CTP_INVALID_MEDIA_TYPE
	// renders when the request carries no Content-Type at all.
	fastifyUndefinedMediaTypeMessage = "Unsupported Media Type: undefined"

	// storageLogModule mirrors the literal 'storage' module used by the route.
	storageLogModule = "storage"
)

var (
	storageTypes = []string{
		string(models.StorageTypeWEBDAV),
		string(models.StorageTypeFTP),
		string(models.StorageTypeSFTP),
		string(models.StorageTypeS3),
	}

	// storageS3BucketPattern mirrors /^[a-z0-9][a-z0-9.-]*[a-z0-9]$/i.
	storageS3BucketPattern = regexp.MustCompile(`(?i)^[a-z0-9][a-z0-9.-]*[a-z0-9]$`)
)

// ==================== Fastify body parsing ====================

// StorageTestConfig is what StorageConnectionTester receives: the decrypted form
// of a stored config, mirroring storage/factory.ts' decrypt step.
type StorageTestConfig struct {
	Type     models.StorageType
	Host     string
	Port     *int32
	Username *string
	Password *string
	BasePath *string
	Extra    datatypes.JSON
}

// StorageConnectionTester is the seam for
// `StorageFactory.create(config).testConnection()`. The storage adapter module
// (server/src/storage/*) is a separate port; until it lands the default reports an
// explicit failure, following the same convention as the mailer/notifier seams in
// lib/operation_verification.go (never a silent success). The storage module should
// assign this variable from its init().
var StorageConnectionTester = func(ctx context.Context, config StorageTestConfig) error {
	return fmt.Errorf("存储类型 %s 的连接测试尚未移植", config.Type)
}

// fastifyJSONBody is the decoded request body plus the document order of its
// top-level keys. Go maps lose insertion order, but Prisma's validation message
// renders the offending `data` object in insertion order, so the order is captured
// while decoding.
type fastifyJSONBody struct {
	Value      any
	ObjectKeys []string
}

// decodeJSONRequestBody reproduces Fastify's body handling for the methods that
// carry a body (bodywith = DELETE/OPTIONS/PATCH/POST/PUT). It returns status 0 when
// the handler may proceed. Note the order: Fastify runs onRequest hooks (auth)
// before parsing the body, which the Gin middleware chain matches naturally.
func decodeJSONRequestBody(c *gin.Context) (*fastifyJSONBody, int, string) {
	switch c.Request.Method {
	case http.MethodGet, http.MethodHead, http.MethodTrace:
		// bodyless methods: Fastify never parses a body for these.
		return &fastifyJSONBody{}, 0, ""
	}

	contentType := c.GetHeader("Content-Type")
	if contentType == "" {
		if c.Request.ContentLength > 0 || c.GetHeader("Transfer-Encoding") != "" {
			return nil, http.StatusUnsupportedMediaType, fastifyUndefinedMediaTypeMessage
		}
		return &fastifyJSONBody{}, 0, ""
	}

	lowered := strings.ToLower(contentType)
	switch {
	case mediaTypeIs(lowered, "application/json"):
		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return nil, http.StatusBadRequest, fastifyInvalidJSONBodyMessage
		}
		if len(data) == 0 {
			return nil, http.StatusBadRequest, fastifyEmptyJSONBodyMessage
		}
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			return nil, http.StatusBadRequest, fastifyInvalidJSONBodyMessage
		}
		// secure-json-parse runs with the default protoAction/constructorAction of
		// 'error', so prototype-poisoning payloads are rejected as invalid JSON.
		if hasForbiddenPrototypeKey(decoded) {
			return nil, http.StatusBadRequest, fastifyInvalidJSONBodyMessage
		}
		return &fastifyJSONBody{Value: decoded, ObjectKeys: jsonTopLevelKeys(data)}, 0, ""
	case mediaTypeIs(lowered, "text/plain"):
		// Fastify's default text/plain parser keeps the raw string.
		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return &fastifyJSONBody{Value: ""}, 0, ""
		}
		return &fastifyJSONBody{Value: string(data)}, 0, ""
	default:
		return nil, http.StatusUnsupportedMediaType, "Unsupported Media Type: " + contentType
	}
}

// jsonTopLevelKeys returns the top-level object keys of a JSON document in the
// order they appear. Non-object documents yield nil.
func jsonTopLevelKeys(data []byte) []string {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil
	}
	keys := make([]string, 0, 8)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return keys
		}
		if key, ok := keyToken.(string); ok {
			keys = append(keys, key)
		}
		var discard json.RawMessage
		if err := decoder.Decode(&discard); err != nil {
			return keys
		}
	}
	return keys
}

// mediaTypeIs mirrors ContentTypeParser.getParser's prefix comparison: the header
// must match the parser name exactly or be followed by ';', ' ' or '\t'.
func mediaTypeIs(lowered, parserName string) bool {
	if !strings.HasPrefix(lowered, parserName) {
		return false
	}
	if len(lowered) == len(parserName) {
		return true
	}
	switch lowered[len(parserName)] {
	case ';', ' ', '\t':
		return true
	}
	return false
}

// hasForbiddenPrototypeKey mirrors secure-json-parse's protoAction/constructorAction
// of 'error'.
func hasForbiddenPrototypeKey(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, entry := range typed {
			if key == "__proto__" {
				return true
			}
			if key == "constructor" {
				if nested, ok := entry.(map[string]any); ok {
					if _, hasPrototype := nested["prototype"]; hasPrototype {
						return true
					}
				}
			}
			if hasForbiddenPrototypeKey(entry) {
				return true
			}
		}
	case []any:
		for _, entry := range typed {
			if hasForbiddenPrototypeKey(entry) {
				return true
			}
		}
	}
	return false
}

// ==================== AJV replica ====================

type ajvFieldKind int

const (
	ajvKindString ajvFieldKind = iota
	ajvKindNullableString
	ajvKindInteger
	ajvKindNullableInteger
	ajvKindObject
	ajvKindNullableObject
	ajvKindBoolean
)

// ajvFieldSpec describes one property of a Fastify body schema.
type ajvFieldSpec struct {
	Name      string
	Kind      ajvFieldKind
	MinLength int     // -1 when unset
	MaxLength int     // -1 when unset
	Minimum   float64 // NaN when unset
	Maximum   float64 // NaN when unset
	Enum      []string
}

// ajvValidated holds the coerced values of a body. Values are `any` and use the
// same Go types as encoding/json (string, float64, bool, nil, map, slice).
type ajvValidated struct {
	Values  map[string]any
	Present map[string]bool
}

func (v ajvValidated) stringValue(name string) (string, bool) {
	raw, present := v.Values[name]
	if !present {
		return "", false
	}
	text, ok := raw.(string)
	return text, ok
}

func (v ajvValidated) nullableString(name string) (*string, bool) {
	if !v.Present[name] {
		return nil, false
	}
	raw := v.Values[name]
	if raw == nil {
		return nil, true
	}
	text, ok := raw.(string)
	if !ok {
		return nil, true
	}
	return &text, true
}

func (v ajvValidated) nullableInt32(name string) (*int32, bool) {
	if !v.Present[name] {
		return nil, false
	}
	raw := v.Values[name]
	number, ok := raw.(float64)
	if !ok || math.IsInf(number, 0) || math.IsNaN(number) {
		// JSON.stringify(Infinity) is null, and the integer constraints are skipped
		// for non-finite values in AJV, so the wire value is null.
		return nil, true
	}
	converted := int32(number)
	return &converted, true
}

func (v ajvValidated) boolValue(name string) bool {
	value, _ := v.Values[name].(bool)
	return value
}

// validateAJVObject reproduces the parts of an AJV validator that these routes use:
// the root object type check, the `required` list (checked before properties, in
// order), then each property in schema order with coerceTypes:'array' semantics and
// allErrors:false (the first failure is the only one reported).
func validateAJVObject(decoded any, required []string, fields []ajvFieldSpec) (ajvValidated, string) {
	out := ajvValidated{Values: map[string]any{}, Present: map[string]bool{}}

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
		coerced, message := ajvCoerceField(field, value)
		if message != "" {
			return out, message
		}
		out.Present[field.Name] = true
		out.Values[field.Name] = coerced
	}
	return out, ""
}

// ajvCoerceField applies the type coercion then the keyword constraints for one
// property, returning a message on the first failure.
func ajvCoerceField(field ajvFieldSpec, value any) (any, string) {
	switch field.Kind {
	case ajvKindString:
		text, message := ajvCoerceString(value)
		if message != "" {
			return nil, message
		}
		if len(field.Enum) > 0 && !ajvEnumContains(field.Enum, text) {
			return nil, "must be equal to one of the allowed values"
		}
		if field.MaxLength >= 0 && jsStringLength(text) > field.MaxLength {
			return nil, field.Name + " is too long"
		}
		if field.MinLength >= 0 && jsStringLength(text) < field.MinLength {
			return nil, field.Name + " is too short"
		}
		return text, ""

	case ajvKindNullableString:
		coerced, message := ajvCoerceNullableString(value)
		if message != "" {
			return nil, message
		}
		if field.MaxLength >= 0 {
			if text, ok := coerced.(string); ok && jsStringLength(text) > field.MaxLength {
				return nil, field.Name + " is too long"
			}
		}
		if field.MinLength >= 0 {
			if text, ok := coerced.(string); ok && jsStringLength(text) < field.MinLength {
				return nil, field.Name + " is too short"
			}
		}
		return coerced, ""

	case ajvKindInteger:
		number, ok := ajvCoerceInteger(value)
		if !ok {
			return nil, "must be integer"
		}
		if message := ajvIntegerBoundsMessage(field, number); message != "" {
			return nil, message
		}
		return number, ""

	case ajvKindNullableInteger:
		coerced, ok := ajvCoerceNullableInteger(value)
		if !ok {
			return nil, "must be integer,null"
		}
		if number, isNumber := coerced.(float64); isNumber {
			if message := ajvIntegerBoundsMessage(field, number); message != "" {
				return nil, message
			}
		}
		return coerced, ""

	case ajvKindObject:
		object, ok := value.(map[string]any)
		if !ok {
			return nil, "must be object"
		}
		return object, ""

	case ajvKindNullableObject:
		coerced, message := ajvCoerceNullableObject(value)
		if message != "" {
			return nil, message
		}
		return coerced, ""

	case ajvKindBoolean:
		coerced, ok := ajvCoerceBoolean(value)
		if !ok {
			return nil, "must be boolean"
		}
		return coerced, ""
	}
	return nil, "must be valid"
}

// ajvIntegerBoundsMessage mirrors the generated `maximum` then `minimum` checks,
// which only run for finite numbers.
func ajvIntegerBoundsMessage(field ajvFieldSpec, number float64) string {
	if math.IsInf(number, 0) || math.IsNaN(number) {
		return ""
	}
	if !math.IsNaN(field.Maximum) && number > field.Maximum {
		return "must be <= " + formatTerminalCommandNumber(field.Maximum)
	}
	if !math.IsNaN(field.Minimum) && number < field.Minimum {
		return "must be >= " + formatTerminalCommandNumber(field.Minimum)
	}
	return ""
}

func ajvEnumContains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

// ajvUnwrapSingleArray mirrors the `coerceTypes:'array'` step: a one-element array
// is replaced by its element (AJV then re-tests the element's type).
func ajvUnwrapSingleArray(value any) (any, bool) {
	if array, ok := value.([]any); ok && len(array) == 1 {
		return array[0], true
	}
	return value, false
}

func ajvCoerceString(value any) (string, string) {
	if text, ok := value.(string); ok {
		return text, ""
	}
	source, _ := ajvUnwrapSingleArray(value)
	switch typed := source.(type) {
	case string:
		return typed, ""
	case float64:
		return formatTerminalCommandNumber(typed), ""
	case bool:
		return strconv.FormatBool(typed), ""
	case nil:
		return "", ""
	}
	return "", "must be string"
}

func ajvCoerceNullableString(value any) (any, string) {
	if text, ok := value.(string); ok {
		return text, ""
	}
	if value == nil {
		return nil, ""
	}
	source, _ := ajvUnwrapSingleArray(value)
	switch typed := source.(type) {
	case float64:
		return formatTerminalCommandNumber(typed), ""
	case bool:
		return strconv.FormatBool(typed), ""
	case nil:
		return "", ""
	case string:
		if typed == "" {
			return nil, ""
		}
		return "", "must be string,null"
	}
	return "", "must be string,null"
}

// ajvIsInteger mirrors `(typeof data == "number") && !(data % 1) && isFinite(data)`.
func ajvIsInteger(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value == math.Trunc(value)
}

func ajvCoerceInteger(value any) (float64, bool) {
	if number, ok := value.(float64); ok && ajvIsInteger(number) {
		return number, true
	}
	source, unwrapped := ajvUnwrapSingleArray(value)
	if unwrapped {
		// The single-type unwrap shortcut accepts the element as-is when it already
		// satisfies the integer type test.
		if number, ok := source.(float64); ok && ajvIsInteger(number) {
			return number, true
		}
	}
	switch typed := source.(type) {
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	case string:
		return ajvStringToInteger(typed)
	}
	return 0, false
}

func ajvCoerceNullableInteger(value any) (any, bool) {
	if number, ok := value.(float64); ok && ajvIsInteger(number) {
		return number, true
	}
	if value == nil {
		return nil, true
	}
	// Multi-type schemas: the unwrap shortcut can never accept (AJV requires
	// `isInteger(data) && data === null`), so the element falls through to the
	// coercion chain below.
	source, _ := ajvUnwrapSingleArray(value)

	// `dataType === "boolean" || data === null || (string && data && data == +data && !(data % 1))`
	switch typed := source.(type) {
	case bool:
		if typed {
			return float64(1), true
		}
		return float64(0), true
	case nil:
		return float64(0), true
	case string:
		if typed != "" {
			if number, ok := ajvStringToInteger(typed); ok {
				return number, true
			}
		}
	}
	// `data === "" || data === 0 || data === false` → null
	switch typed := source.(type) {
	case string:
		if typed == "" {
			return nil, true
		}
	case float64:
		if typed == 0 {
			return nil, true
		}
	case bool:
		if !typed {
			return nil, true
		}
	}
	return nil, false
}

func ajvCoerceNullableObject(value any) (any, string) {
	if object, ok := value.(map[string]any); ok {
		return object, ""
	}
	if value == nil {
		return nil, ""
	}
	source, _ := ajvUnwrapSingleArray(value)
	switch typed := source.(type) {
	case string:
		if typed == "" {
			return nil, ""
		}
	case float64:
		if typed == 0 {
			return nil, ""
		}
	case bool:
		if !typed {
			return nil, ""
		}
	}
	return nil, "must be object,null"
}

func ajvCoerceBoolean(value any) (bool, bool) {
	if flag, ok := value.(bool); ok {
		return flag, true
	}
	source, _ := ajvUnwrapSingleArray(value)
	switch typed := source.(type) {
	case bool:
		return typed, true
	case nil:
		return false, true
	case float64:
		if typed == 0 {
			return false, true
		}
		if typed == 1 {
			return true, true
		}
	case string:
		if typed == "false" {
			return false, true
		}
		if typed == "true" {
			return true, true
		}
	}
	return false, false
}

// ajvJSNumberPattern is the decimal syntax JavaScript's unary plus accepts.
var ajvJSNumberPattern = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$`)

// ajvStringToInteger mirrors `data && data == +data && !(data % 1)` for strings.
func ajvStringToInteger(raw string) (float64, bool) {
	if raw == "" {
		return 0, false
	}
	number, ok := ajvToJSNumber(raw)
	if !ok {
		return 0, false
	}
	if math.IsInf(number, 0) {
		// Infinity % 1 is NaN, and !NaN is true, so AJV keeps it (bounds are then
		// skipped because the value is not finite).
		return number, true
	}
	if number != math.Trunc(number) {
		return 0, false
	}
	return number, true
}

// ajvToJSNumber mirrors JavaScript's unary plus on a string.
func ajvToJSNumber(raw string) (float64, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		// +"   " === 0
		return 0, true
	}
	sign := 1.0
	unsigned := trimmed
	if strings.HasPrefix(unsigned, "+") {
		unsigned = unsigned[1:]
	} else if strings.HasPrefix(unsigned, "-") {
		sign = -1
		unsigned = unsigned[1:]
	}

	switch {
	case strings.EqualFold(unsigned, "Infinity"):
		return sign * math.Inf(1), true
	case strings.HasPrefix(unsigned, "0x") || strings.HasPrefix(unsigned, "0X"):
		// Hex/binary/octal literals have no sign in JS: +"-0x10" is NaN.
		if sign < 0 || unsigned != trimmed {
			return 0, false
		}
		parsed, err := strconv.ParseUint(unsigned[2:], 16, 64)
		if err != nil {
			return 0, false
		}
		return float64(parsed), true
	case strings.HasPrefix(unsigned, "0b") || strings.HasPrefix(unsigned, "0B"):
		if sign < 0 || unsigned != trimmed {
			return 0, false
		}
		parsed, err := strconv.ParseUint(unsigned[2:], 2, 64)
		if err != nil {
			return 0, false
		}
		return float64(parsed), true
	case strings.HasPrefix(unsigned, "0o") || strings.HasPrefix(unsigned, "0O"):
		if sign < 0 || unsigned != trimmed {
			return 0, false
		}
		parsed, err := strconv.ParseUint(unsigned[2:], 8, 64)
		if err != nil {
			return 0, false
		}
		return float64(parsed), true
	}

	if !ajvJSNumberPattern.MatchString(trimmed) {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// jsStringLength returns JavaScript's `.length` for a string, i.e. the number of
// UTF-16 code units. AJV compares minLength/maxLength against that, so astral
// characters count as two.
func jsStringLength(value string) int {
	units := 0
	for _, runeValue := range value {
		if runeValue > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return units
}

// respondAJVValidation mirrors the validation branch of app.ts' setErrorHandler:
// allErrors is off, so a single message and no `details`.
func respondAJVValidation(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": message, "code": "VALIDATION_ERROR"})
}

// ==================== storage/path.ts helpers ====================

// normalizeStorageBasePath mirrors normalizeStorageBasePath().
func normalizeStorageBasePath(basePath *string) (string, error) {
	raw := ""
	if basePath != nil {
		raw = strings.TrimSpace(*basePath)
	}
	if raw == "" {
		return "", nil
	}
	if strings.ContainsRune(raw, '\x00') || strings.Contains(raw, `\`) {
		return "", errors.New("Storage base path contains invalid characters")
	}

	absolute := strings.HasPrefix(raw, "/")
	segments := make([]string, 0, 4)
	for _, segment := range strings.Split(raw, "/") {
		if segment == "" {
			continue
		}
		if segment == "." || segment == ".." {
			return "", errors.New("Storage base path cannot contain relative path segments")
		}
		segments = append(segments, segment)
	}
	if len(segments) == 0 {
		if absolute {
			return "/", nil
		}
		return "", nil
	}
	prefix := ""
	if absolute {
		prefix = "/"
	}
	return prefix + strings.Join(segments, "/") + "/", nil
}

// normalizeS3Extra mirrors normalizeS3Extra(). The returned map only ever holds
// {bucket, region, forcePathStyle}, so S3 credentials cannot leak through `extra`.
func normalizeS3Extra(extra map[string]any) (map[string]any, error) {
	bucket := ""
	if raw, ok := extra["bucket"].(string); ok {
		bucket = strings.TrimSpace(raw)
	}
	if bucket == "" || len(bucket) > storageS3BucketMaxLength || !storageS3BucketPattern.MatchString(bucket) {
		return nil, errors.New("S3 bucket 配置无效")
	}

	region := "auto"
	if raw, ok := extra["region"].(string); ok && strings.TrimSpace(raw) != "" {
		region = strings.TrimSpace(raw)
	}
	if len(region) > storageS3RegionMaxLength {
		return nil, errors.New("S3 region 配置过长")
	}

	forcePathStyle := false
	if raw, ok := extra["forcePathStyle"].(bool); ok {
		forcePathStyle = raw
	}

	return map[string]any{
		"bucket":         bucket,
		"region":         region,
		"forcePathStyle": forcePathStyle,
	}, nil
}

// normalizeStorageExtra mirrors normalizeStorageExtra(): only S3 has extra
// requirements, everything else is passed through untouched.
func normalizeStorageExtra(storageType string, extra map[string]any, username *string, hasSecret bool) (map[string]any, error) {
	if storageType != string(models.StorageTypeS3) {
		return extra, nil
	}
	if username == nil || strings.TrimSpace(*username) == "" {
		return nil, errors.New("S3 access key 不能为空")
	}
	if !hasSecret {
		return nil, errors.New("S3 secret key 不能为空")
	}
	return normalizeS3Extra(extra)
}

// ==================== response views (password never serialized) ====================

// storageConfigListResponse mirrors the GET projection. There is deliberately no
// Password field: the TypeScript simply omits it.
type storageConfigListResponse struct {
	ID        int32              `json:"id"`
	Name      string             `json:"name"`
	Type      models.StorageType `json:"type"`
	Host      string             `json:"host"`
	Port      *int32             `json:"port"`
	Username  *string            `json:"username"`
	BasePath  *string            `json:"basePath"`
	Extra     datatypes.JSON     `json:"extra"`
	IsDefault bool               `json:"isDefault"`
	CreatedAt string             `json:"createdAt"`
	UpdatedAt string             `json:"updatedAt"`
}

// storageConfigCreatedResponse mirrors the POST projection (createdAt only).
type storageConfigCreatedResponse struct {
	ID        int32              `json:"id"`
	Name      string             `json:"name"`
	Type      models.StorageType `json:"type"`
	Host      string             `json:"host"`
	Port      *int32             `json:"port"`
	Username  *string            `json:"username"`
	BasePath  *string            `json:"basePath"`
	Extra     datatypes.JSON     `json:"extra"`
	IsDefault bool               `json:"isDefault"`
	CreatedAt string             `json:"createdAt"`
}

// storageConfigUpdatedResponse mirrors the PATCH projection (updatedAt only).
type storageConfigUpdatedResponse struct {
	ID        int32              `json:"id"`
	Name      string             `json:"name"`
	Type      models.StorageType `json:"type"`
	Host      string             `json:"host"`
	Port      *int32             `json:"port"`
	Username  *string            `json:"username"`
	BasePath  *string            `json:"basePath"`
	Extra     datatypes.JSON     `json:"extra"`
	IsDefault bool               `json:"isDefault"`
	UpdatedAt string             `json:"updatedAt"`
}

// storageISOTime mirrors Date#toISOString().
func storageISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// jsonFromAny marshals a decoded JSON value for the `extra` column.
func jsonFromAny(value any) datatypes.JSON {
	if value == nil {
		return nil
	}
	if raw, ok := value.(datatypes.JSON); ok {
		return raw
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return datatypes.JSON(encoded)
}

// extraAsMap decodes the stored `extra` column into the shape the helpers expect.
func extraAsMap(extra datatypes.JSON) map[string]any {
	if len(extra) == 0 {
		return nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(extra, &decoded); err != nil {
		return nil
	}
	return decoded
}

// ==================== route ====================

// storageCreateFields / storageUpdateFields describe the two Fastify body schemas.
// They live at package scope because the handlers are shared by the "" and "/"
// route spellings.
var storageCreateFields = []ajvFieldSpec{
	{Name: "name", Kind: ajvKindString, MinLength: 1, MaxLength: storageNameMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "type", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN(), Enum: storageTypes},
	{Name: "host", Kind: ajvKindString, MinLength: 1, MaxLength: storageHostMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "port", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: storagePortMinimum, Maximum: storagePortMaximum},
	{Name: "username", Kind: ajvKindString, MinLength: -1, MaxLength: storageUsernameMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "password", Kind: ajvKindString, MinLength: -1, MaxLength: storagePasswordMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "basePath", Kind: ajvKindString, MinLength: -1, MaxLength: storageBasePathMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "extra", Kind: ajvKindObject, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "isDefault", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
}

var storageUpdateFields = []ajvFieldSpec{
	{Name: "name", Kind: ajvKindString, MinLength: 1, MaxLength: storageNameMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "type", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN(), Enum: storageTypes},
	{Name: "host", Kind: ajvKindString, MinLength: 1, MaxLength: storageHostMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "port", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: storagePortMinimum, Maximum: storagePortMaximum},
	{Name: "username", Kind: ajvKindNullableString, MinLength: -1, MaxLength: storageUsernameMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "password", Kind: ajvKindNullableString, MinLength: -1, MaxLength: storagePasswordMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "basePath", Kind: ajvKindNullableString, MinLength: -1, MaxLength: storageBasePathMaxLength, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "extra", Kind: ajvKindNullableObject, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "isDefault", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
}

// storageConfigsListHandler mirrors `fastify.get('/', { onRequest: [fastify.authenticate] })`.
func storageConfigsListHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	configs, err := db.GetStorageConfigsByUserID(c.Request.Context(), user.ID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	items := make([]storageConfigListResponse, 0, len(configs))
	for _, config := range configs {
		items = append(items, storageConfigListResponse{
			ID:        config.ID,
			Name:      config.Name,
			Type:      config.Type,
			Host:      config.Host,
			Port:      config.Port,
			Username:  config.Username,
			BasePath:  config.BasePath,
			Extra:     config.Extra,
			IsDefault: config.IsDefault,
			CreatedAt: storageISOTime(config.CreatedAt),
			UpdatedAt: storageISOTime(config.UpdatedAt),
		})
	}
	c.JSON(http.StatusOK, items)
}

// storageConfigsCreateHandler mirrors `fastify.post('/', { onRequest: [fastify.authenticate], schema })`.
func storageConfigsCreateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	body, validationMessage := validateAJVObject(decoded.Value, []string{"name", "type", "host"}, storageCreateFields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	name, _ := body.stringValue("name")
	rawType, _ := body.stringValue("type")
	host, _ := body.stringValue("host")
	port, _ := body.nullableInt32("port")
	username, _ := body.nullableString("username")
	password, _ := body.nullableString("password")
	basePath, _ := body.nullableString("basePath")
	extra, _ := body.Values["extra"].(map[string]any)
	isDefault := body.boolValue("isDefault")

	// First pass: SSRF guard + path/extra normalization, all mapped to
	// 400 INVALID_PARAMS with the helper's message (as the TS catch does).
	if err := storageValidateTarget(ctx, rawType, host); err != nil {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, err.Error())
		return
	}
	if _, err := normalizeStorageBasePath(basePath); err != nil {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, err.Error())
		return
	}
	normalizedExtra, err := normalizeStorageExtra(rawType, extra, username, password != nil && *password != "")
	if err != nil {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, err.Error())
		return
	}

	// Second pass: encrypt the secret then persist. Failures here are 500s.
	var encryptedPassword *string
	if password != nil && *password != "" {
		encrypted, err := lib.EncryptSensitiveData(*password)
		if err != nil {
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrStorageConfigCreateFailed, err.Error())
			return
		}
		encryptedPassword = &encrypted
	}
	normalizedBasePath, err := normalizeStorageBasePath(basePath)
	if err != nil {
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrStorageConfigCreateFailed, err.Error())
		return
	}

	config, err := db.CreateStorageConfig(ctx, db.StorageConfigCreateData{
		UserID:    user.ID,
		Name:      name,
		Type:      models.StorageType(rawType),
		Host:      host,
		Port:      port,
		Username:  username,
		Password:  encryptedPassword,
		BasePath:  nullableNonEmpty(normalizedBasePath),
		Extra:     jsonFromAny(normalizedExtra),
		IsDefault: isDefault,
	})
	if err != nil {
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrStorageConfigCreateFailed, err.Error())
		return
	}

	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, storageLogModule, "storage.create",
		"Created storage config \""+name+"\" ("+rawType+")",
		db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, storageConfigCreatedResponse{
		ID:        config.ID,
		Name:      config.Name,
		Type:      config.Type,
		Host:      config.Host,
		Port:      config.Port,
		Username:  config.Username,
		BasePath:  config.BasePath,
		Extra:     config.Extra,
		IsDefault: config.IsDefault,
		CreatedAt: storageISOTime(config.CreatedAt),
	})
}

// RegisterStorageConfigs mounts /api/storage-configs. Every route uses
// `fastify.authenticate` (admins included), and every :id route verifies ownership.
func RegisterStorageConfigs(r *gin.Engine) {
	group := r.Group("/api/storage-configs", middleware.Authenticate())

	// GET / — the user's configs; never includes the password column.
	//
	// Fastify registers both "/api/storage-configs" and "/api/storage-configs/" for
	// a plugin root, so both spellings are mounted here as well.
	group.GET("", storageConfigsListHandler)
	group.GET("/", storageConfigsListHandler)

	// POST / — create a config.
	group.POST("", storageConfigsCreateHandler)
	group.POST("/", storageConfigsCreateHandler)

	// PATCH /:id — partial update.
	group.PATCH("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		decoded, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}
		body, validationMessage := validateAJVObject(decoded.Value, nil, storageUpdateFields)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		id, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		existing, err := db.GetStorageConfigByID(ctx, int32(id))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if existing == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrStorageConfigNotFound, "")
			return
		}
		// 管理员只管理系统层面，不参与用户存储配置操作
		if existing.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		// Effective values after the patch, exactly as the TS computes nextType /
		// nextHost / nextExtra / nextUsername / hasSecret.
		nextType := string(existing.Type)
		if rawType, present := body.stringValue("type"); present {
			nextType = rawType
		}
		nextHost := existing.Host
		if host, present := body.stringValue("host"); present {
			nextHost = host
		}
		nextExtra := extraAsMap(existing.Extra)
		if body.Present["extra"] {
			if candidate, ok := body.Values["extra"].(map[string]any); ok {
				nextExtra = candidate
			} else {
				nextExtra = nil
			}
		}
		nextUsername := existing.Username
		if username, present := body.nullableString("username"); present {
			nextUsername = username
		}
		hasSecret := existing.Password != nil && *existing.Password != ""
		if password, present := body.nullableString("password"); present {
			hasSecret = password != nil && *password != ""
		}

		if err := storageValidateTarget(ctx, nextType, nextHost); err != nil {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, err.Error())
			return
		}
		normalizedExtra, err := normalizeStorageExtra(nextType, nextExtra, nextUsername, hasSecret)
		if err != nil {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, err.Error())
			return
		}

		updateData := db.StorageConfigUpdateData{}
		if name, present := body.stringValue("name"); present {
			updateData.NameProvided = true
			updateData.Name = name
		}
		if _, present := body.stringValue("type"); present {
			updateData.TypeProvided = true
			updateData.Type = models.StorageType(nextType)
		}
		if host, present := body.stringValue("host"); present {
			updateData.HostProvided = true
			updateData.Host = host
		}
		if port, present := body.nullableInt32("port"); present {
			updateData.PortProvided = true
			updateData.Port = port
		}
		if username, present := body.nullableString("username"); present {
			updateData.UsernameProvided = true
			updateData.Username = username
		}
		if password, present := body.nullableString("password"); present {
			// Only the presence of a password encrypts; an empty/new-less value
			// clears the column (encryptSensitiveData is never called with "").
			updateData.PasswordProvided = true
			if password != nil && *password != "" {
				encrypted, err := lib.EncryptSensitiveData(*password)
				if err != nil {
					lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrStorageConfigUpdateFailed, err.Error())
					return
				}
				updateData.Password = &encrypted
			}
		}
		if basePath, present := body.nullableString("basePath"); present {
			updateData.BasePathProvided = true
			if basePath != nil {
				normalized, err := normalizeStorageBasePath(basePath)
				if err != nil {
					lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, err.Error())
					return
				}
				updateData.BasePath = nullableNonEmpty(normalized)
			}
		}
		if body.Present["extra"] || nextType == string(models.StorageTypeS3) || existing.Type == models.StorageTypeS3 {
			updateData.ExtraProvided = true
			updateData.Extra = jsonFromAny(normalizedExtra)
		}
		if body.Present["isDefault"] {
			updateData.IsDefaultProvided = true
			updateData.IsDefault = body.boolValue("isDefault")
		}

		// `const { password, type, ...rest } = request.body` forwards every other
		// key straight into Prisma, which rejects unknown arguments. Reproduce that
		// failure instead of silently dropping them.
		if unknownKeys := storageUnknownUpdateKeys(decoded); len(unknownKeys) > 0 {
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrStorageConfigUpdateFailed,
				prismaUnknownArgumentMessage(id, decoded, unknownKeys[0]))
			return
		}

		config, err := db.UpdateStorageConfig(ctx, int32(id), updateData)
		if err != nil {
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrStorageConfigUpdateFailed, err.Error())
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, storageLogModule, "storage.update",
			"Updated storage config \""+config.Name+"\"",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, storageConfigUpdatedResponse{
			ID:        config.ID,
			Name:      config.Name,
			Type:      config.Type,
			Host:      config.Host,
			Port:      config.Port,
			Username:  config.Username,
			BasePath:  config.BasePath,
			Extra:     config.Extra,
			IsDefault: config.IsDefault,
			UpdatedAt: storageISOTime(config.UpdatedAt),
		})
	})

	// DELETE /:id
	group.DELETE("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		// Fastify parses (and 400s on an empty JSON) body before the handler runs.
		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		id, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		existing, err := db.GetStorageConfigByID(ctx, int32(id))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if existing == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrStorageConfigNotFound, "")
			return
		}
		if existing.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		activeTaskCount, err := db.CountActiveTasksForStorageConfig(ctx, int32(id))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if activeTaskCount > 0 {
			c.JSON(http.StatusConflict, gin.H{
				"error":   "STORAGE_HAS_ACTIVE_TASKS",
				"message": "该存储配置有 " + strconv.FormatInt(activeTaskCount, 10) + " 个上传任务正在进行中，请等待完成后再删除",
			})
			return
		}

		if err := db.DeleteStorageConfig(ctx, int32(id)); err != nil {
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrStorageConfigDeleteFailed, err.Error())
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, storageLogModule, "storage.delete",
			"Deleted storage config \""+existing.Name+"\"",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "Storage config deleted"})
	})

	// POST /:id/test — connection test through the (not yet ported) storage module.
	group.POST("/:id/test", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		id, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		config, err := db.GetStorageConfigByID(ctx, int32(id))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if config == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrStorageConfigNotFound, "")
			return
		}
		if config.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		// storage/factory.ts decrypts the stored secret before handing the config to
		// a provider; the seam receives the same decrypted view.
		var decryptedPassword *string
		if config.Password != nil {
			plaintext := lib.DecryptSensitiveData(*config.Password)
			decryptedPassword = &plaintext
		}

		if err := StorageConnectionTester(ctx, StorageTestConfig{
			Type:     config.Type,
			Host:     config.Host,
			Port:     config.Port,
			Username: config.Username,
			Password: decryptedPassword,
			BasePath: config.BasePath,
			Extra:    config.Extra,
		}); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"error":   "CONNECTION_TEST_FAILED",
				"message": err.Error(),
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "连接测试成功"})
	})

	// POST /:id/set-default
	group.POST("/:id/set-default", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		id, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		config, err := db.GetStorageConfigByID(ctx, int32(id))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if config == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrStorageConfigNotFound, "")
			return
		}
		if config.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		if err := db.SetDefaultStorageConfig(ctx, user.ID, int32(id)); err != nil {
			lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrStorageConfigUpdateFailed, "")
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "已设为默认"})
	})
}

// storageValidateTarget mirrors validateStorageTarget(): the SSRF guard error text
// is surfaced to the client unchanged.
func storageValidateTarget(ctx context.Context, targetType, host string) error {
	return lib.AssertSafeStorageTarget(ctx, targetType, host)
}

// nullableNonEmpty mirrors `normalized || null`.
func nullableNonEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// storageWritableUpdateKeys is the set of Prisma arguments that the TypeScript
// `...rest` spread can legitimately carry.
var storageWritableUpdateKeys = map[string]bool{
	"name":      true,
	"type":      true,
	"host":      true,
	"port":      true,
	"username":  true,
	"password":  true,
	"basePath":  true,
	"extra":     true,
	"isDefault": true,
}

// storageUnknownUpdateKeys returns the body keys, in document order, that Prisma
// would reject as unknown arguments.
func storageUnknownUpdateKeys(body *fastifyJSONBody) []string {
	if body == nil {
		return nil
	}
	if _, ok := body.Value.(map[string]any); !ok {
		return nil
	}
	unknown := make([]string, 0)
	for _, key := range body.ObjectKeys {
		if !storageWritableUpdateKeys[key] {
			unknown = append(unknown, key)
		}
	}
	return unknown
}

// prismaStorageUpdateOptionsBlock is the "available options" listing Prisma prints
// in a PrismaClientValidationError for storageConfig.update. Prisma lists every
// writable argument, including the ones this route never sends.
const prismaStorageUpdateOptionsBlock = "?   name?: String | StringFieldUpdateOperationsInput," +
	"\n?   type?: StorageType | EnumStorageTypeFieldUpdateOperationsInput," +
	"\n?   host?: String | StringFieldUpdateOperationsInput," +
	"\n?   port?: Int | NullableIntFieldUpdateOperationsInput | Null," +
	"\n?   username?: String | NullableStringFieldUpdateOperationsInput | Null," +
	"\n?   password?: String | NullableStringFieldUpdateOperationsInput | Null," +
	"\n?   basePath?: String | NullableStringFieldUpdateOperationsInput | Null," +
	"\n?   extra?: NullableJsonNullValueInput | Json," +
	"\n?   isDefault?: Boolean | BoolFieldUpdateOperationsInput," +
	"\n?   createdAt?: DateTime | DateTimeFieldUpdateOperationsInput," +
	"\n?   updatedAt?: DateTime | DateTimeFieldUpdateOperationsInput," +
	"\n?   user?: UserUpdateOneRequiredWithoutStorageConfigsNestedInput," +
	"\n?   uploadTasks?: BackupUploadTaskUpdateManyWithoutStorageConfigNestedInput"

// prismaUnknownArgumentMessage reproduces the message Prisma raises for an unknown
// update argument, so the 500 details match the Node backend (up to apiError's
// 256-character cap).
//
// Known, accepted difference: when Prisma can locate its call site it prefixes the
// message with the TypeScript file/line ("... invocation in\n/…/db/storage-configs.ts:112:33")
// and a source excerpt. That preamble is environment-specific (absolute build
// path), and apiError truncates the message at 256 characters either way, so this
// replica always emits Prisma's location-less form. The JSON argument block that
// follows is identical.
func prismaUnknownArgumentMessage(id int64, body *fastifyJSONBody, unknownKey string) string {
	object, _ := body.Value.(map[string]any)

	var builder strings.Builder
	builder.WriteString("\nInvalid `prisma.storageConfig.update()` invocation:\n\n{\n  where: {\n    id: ")
	builder.WriteString(strconv.FormatInt(id, 10))
	builder.WriteString("\n  },\n  data: {\n")

	for _, key := range body.ObjectKeys {
		builder.WriteString("    ")
		builder.WriteString(key)
		builder.WriteString(": ")
		builder.WriteString(prismaRenderValue(object[key]))
		builder.WriteString(",\n")
		if key == unknownKey {
			builder.WriteString("    ~~~~\n")
		}
	}

	builder.WriteString(prismaStorageUpdateOptionsBlock)
	builder.WriteString("\n  }\n}\n\nUnknown argument `")
	builder.WriteString(unknownKey)
	builder.WriteString("`. Available options are marked with ?.")
	return builder.String()
}

// prismaRenderValue renders a JSON value the way Prisma prints it in its validation
// error body (JSON-ish scalars, compact JSON composites).
func prismaRenderValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return formatTerminalCommandNumber(typed)
	case string:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return `"` + typed + `"`
		}
		return string(encoded)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return "null"
		}
		return string(encoded)
	}
}

func init() {
	Register(RegisterStorageConfigs)
}
