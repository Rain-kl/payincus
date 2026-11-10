// Port of server/src/routes/packages.ts (2877 lines, 19 routes).
//
// Mounting notes (observable on the wire):
//
//   - app.ts registers this plugin with `{ prefix: '/api/packages' }` and declares
//     the list route as `'/'`. Fastify's prefixTrailingSlash defaults to 'both', so
//     GET/POST /api/packages and /api/packages/ are BOTH valid; Gin keeps them
//     distinct, so the list and create routes are registered twice.
//   - Two routes are public (no Authorization): GET /public and GET /public/regions.
//     Their payloads are compared field by field by the conformance suite.
//   - The sharing routes use `authenticateUser` (role=user only), so an admin gets
//     403 USER_ONLY there. Everything else uses `authenticate`.
//   - The owner check order is preserved exactly: invalid id → 400 INVALID_ID,
//     missing package → 404 PACKAGE_NOT_FOUND, non-owner/non-admin → 403 FORBIDDEN.
//     Do not "simplify" 403 into 404: the client depends on the distinction.
//
// Behaviour that is easy to lose and is therefore commented at each site:
//
//   - GET / (list) has four different shapes: scope=mine (raw getAllPackages row +
//     extras), scope=official, source=official/friends (formatSharedPackage), and
//     the merged default (own entries use yet another explicit field list).
//   - GET /:id only exposes host_storage_pools / host_traffic_multipliers / the
//     global_* keys to the owner or an admin; for everyone else the keys are
//     *absent*, not null.
//   - PATCH /:id distinguishes "key absent" from "explicit null" for every nullable
//     column; db.PackageUpdateData models that with PackageField.
//   - POST /:id/plans and PUT both run the plan normaliser (required-field checks,
//     GPU nullable rules, money rounding) before touching the database.
//
// Times written into SQL go through time.Now().UTC() / db's NowFunc (WAVE_D_SPEC
// §0.2b). Money never passes through float64 inside a transaction: the db layer
// takes float64 only at its API boundary and converts to Decimal immediately
// (WAVE_D_SPEC §0.3).
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== constants ====================

const (
	packagesMaxPlanNameLength            = 50
	packagesPublicPackageMaxInstancesMin = 1
	packagesPublicPackageMaxInstancesMax = 5
	packagesMaxPlanPriceCents            = 99999999
	packagesMaxTrafficResetPriceYuan     = 999999.99
	packagesPostgresIntMax               = 2147483647
	packagesMaxPlanBillingCycleMonths    = 120
	packagesMaxPlanSortOrder             = 1000000
	packagesMaxPackageHostBindings       = 1000
	packagesMaxLimitStringLength         = 64
	packagesMaxStoragePoolNameLength     = 64
	packagesMaxPackageDescriptionLength  = 500
)

// packagesKVMUnsupportedNetworkModes mirrors KVM_UNSUPPORTED_NETWORK_MODES.
var packagesKVMUnsupportedNetworkModes = map[string]bool{
	"nat_ipv6_nat": true,
	"ipv6_nat":     true,
}

// packagesPositiveRouteIDPattern mirrors POSITIVE_ROUTE_ID_PATTERN.
var packagesPositiveRouteIDPattern = regexp.MustCompile(`^[1-9]\d*$`)

// packagesControlCharsPattern mirrors /[\u0000-\u001F\u007F]/.
var packagesControlCharsPattern = regexp.MustCompile("[\x00-\x1F\x7F]")

// packagesPrerequisiteErrorCodes mirrors PACKAGE_PREREQUISITE_ERROR_CODES.
var packagesPrerequisiteErrorCodes = map[string]bool{
	"PACKAGE_PREREQUISITE_INVALID": true,
	"PACKAGE_PREREQUISITE_SELF":    true,
	"PACKAGE_PREREQUISITE_CYCLE":   true,
	"PACKAGE_PREREQUISITE_SCOPE":   true,
}

// ==================== error envelopes ====================

// packagesAPIError carries an already-built apiError() envelope through Go's
// error channel. The TypeScript throws the plain object returned by apiError();
// catching it and replying 400 with it is the whole contract.
type packagesAPIError struct {
	envelope lib.APIError
}

func (e packagesAPIError) Error() string { return e.envelope.Error }

// packagesInputError mirrors NetworkInputError: an Error with statusCode 400 and
// no code field, so the app-level error handler renders {"error": message}.
type packagesInputError struct {
	message string
}

func (e packagesInputError) Error() string { return e.message }

func packagesValidationFailure(details string) error {
	return packagesAPIError{envelope: lib.APIErrorFor(lib.ErrValidationError, details)}
}

// packagesRespondNormalizeError mirrors the `catch (error) { ... reply.code(400)
// .send(error) }` blocks of POST/PATCH: GpuError keeps its own code, apiError
// objects keep their envelope, and any other Error goes through the app-level
// handler (statusCode 400 → {"error": message}).
func packagesRespondNormalizeError(c *gin.Context, err error) {
	var gpuErr *lib.GpuError
	if errors.As(err, &gpuErr) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gpuErr.Message, "code": gpuErr.Code()})
		return
	}
	var apiErr packagesAPIError
	if errors.As(err, &apiErr) {
		c.JSON(http.StatusBadRequest, apiErr.envelope)
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
}

// packagesRespondInternal mirrors the production 5xx branch of app.ts'
// setErrorHandler: no internal detail leaks.
func packagesRespondInternal(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// packagesPrerequisiteErrorOf mirrors getPackagePrerequisiteError: db surfaces
// prerequisite failures as "CODE: message".
func packagesPrerequisiteErrorOf(err error) (string, string, bool) {
	message := err.Error()
	separator := strings.Index(message, ":")
	if separator <= 0 {
		return "", "", false
	}
	code := message[:separator]
	if !packagesPrerequisiteErrorCodes[code] {
		return "", "", false
	}
	detail := strings.TrimSpace(message[separator+1:])
	if detail == "" {
		detail = code
	}
	return code, detail, true
}

// ==================== small helpers ====================

// packagesISOTime mirrors Date.prototype.toISOString().
func packagesISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// packagesOwnerOrAdminDenies mirrors `pkg.user_id !== user.id && user.role !==
// 'admin'` — the 403 branch of the owner-or-admin routes.
func packagesOwnerOrAdminDenies(ownerID int32, user *lib.AccessTokenClaims) bool {
	return ownerID != user.ID && user.Role != "admin"
}

// packagesOwnerOnlyDenies mirrors `pkg.user_id !== user.id` — the sharing routes
// are owner-only (and admins never reach them: authenticateUser rejects them).
func packagesOwnerOnlyDenies(ownerID int32, user *lib.AccessTokenClaims) bool {
	return ownerID != user.ID
}

// packagesRouteID mirrors parsePositiveRouteId. Values above int32 are rejected
// here rather than silently wrapping: Postgres' Int column cannot hold them, and
// the Node implementation fails inside Prisma instead of matching a different row.
func packagesRouteID(raw string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(raw)
	if !ok || parsed > math.MaxInt32 {
		return 0, false
	}
	return int32(parsed), true
}

// packagesDecimalNumber mirrors Number(decimal).
func packagesDecimalNumber(value models.Decimal) float64 {
	return value.InexactFloat64()
}

// packagesDecimalText mirrors Prisma Decimal serialisation (toJSON → string).
func packagesDecimalText(value models.Decimal) string {
	return value.String()
}

// packagesFormatMemory mirrors formatMemory(mb).
func packagesFormatMemory(mb int64) string {
	if mb >= 1024 {
		return strconv.FormatFloat(float64(mb)/1024, 'f', 1, 64) + " GB"
	}
	return strconv.FormatInt(mb, 10) + " MB"
}

// packagesLocaleCompareZH approximates name.localeCompare(other, 'zh') for the
// ASCII region names this endpoint produces: ICU compares case-insensitively at
// the primary level and breaks ties on case. Byte order alone would sort "US"
// before "Australia", which the endpoint's tests catch.
func packagesLocaleCompareZH(left, right string) int {
	lowerLeft := strings.ToLower(left)
	lowerRight := strings.ToLower(right)
	if lowerLeft < lowerRight {
		return -1
	}
	if lowerLeft > lowerRight {
		return 1
	}
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

// packagesRegionDisplayNames is Intl.DisplayNames(['en'], {type:'region'}).
var packagesRegionDisplayNames = display.Regions(language.English)

// packagesRegionCodePattern mirrors /^([a-z]{2}|\d{3})$/i.
var packagesRegionCodePattern = regexp.MustCompile(`^([a-z]{2}|\d{3})$`)

// packagesSpecialCountryNames mirrors specialCountryNames.
var packagesSpecialCountryNames = map[string]string{
	"pc": "Pacific Community",
	"xx": "Unknown",
}

// packagesNumericRegionNames mirrors the UN M49 names ICU knows. x/text maps ISO
// numeric country codes (e.g. 100 → Bulgaria), which Intl.DisplayNames does not;
// keeping ICU's smaller table keeps the two backends byte-identical.
var packagesNumericRegionNames = map[string]string{
	"001": "world",
	"002": "Africa",
	"003": "North America",
	"005": "South America",
	"009": "Oceania",
	"011": "Western Africa",
	"013": "Central America",
	"014": "Eastern Africa",
	"015": "Northern Africa",
	"017": "Middle Africa",
	"018": "Southern Africa",
	"019": "Americas",
	"021": "Northern America",
	"029": "Caribbean",
	"030": "Eastern Asia",
	"034": "Southern Asia",
	"035": "Southeast Asia",
	"039": "Southern Europe",
	"053": "Australasia",
	"054": "Melanesia",
	"057": "Micronesian Region",
	"061": "Polynesia",
	"142": "Asia",
	"143": "Central Asia",
	"145": "Western Asia",
	"150": "Europe",
	"151": "Eastern Europe",
	"154": "Northern Europe",
	"155": "Western Europe",
	"202": "Sub-Saharan Africa",
	"419": "Latin America",
}

// packagesRegionNameOverrides pins the 2-letter codes where the CLDR version
// bundled with x/text still differs from the ICU shipped in Node 22+ (verified
// code by code). An empty value means "fall through to the upper-cased code".
var packagesRegionNameOverrides = map[string]string{
	"an": "Curaçao",
	"cq": "Sark",
	"cs": "Serbia",
	"ct": "",
	"jt": "",
	"mi": "",
	"mk": "North Macedonia",
	"mo": "Macao SAR China",
	"nq": "",
	"pu": "",
	"pz": "",
	"qu": "",
	"su": "Russia",
	"sz": "Eswatini",
	"tr": "Türkiye",
	"wk": "",
	"xa": "Pseudo-Accents",
	"xb": "Pseudo-Bidi",
	"yu": "Serbia",
}

// packagesCountryName mirrors getCountryName(code).
func packagesCountryName(code string) string {
	normalized := strings.ToLower(code)
	upper := strings.ToUpper(normalized)

	if packagesRegionCodePattern.MatchString(normalized) {
		if len(normalized) == 3 {
			if name, ok := packagesNumericRegionNames[normalized]; ok {
				return name
			}
		} else {
			if override, ok := packagesRegionNameOverrides[normalized]; ok {
				if override != "" {
					return override
				}
			} else if region, err := language.ParseRegion(upper); err == nil {
				if name := packagesRegionDisplayNames.Name(region); name != "" && name != upper {
					return name
				}
			}
		}
	}

	if name, ok := packagesSpecialCountryNames[normalized]; ok {
		return name
	}
	return upper
}

// packagesNormalizePublicMaxInstances mirrors normalizePublicPackageMaxInstances.
func packagesNormalizePublicMaxInstances(value *int32) int32 {
	if value != nil && *value >= packagesPublicPackageMaxInstancesMin && *value <= packagesPublicPackageMaxInstancesMax {
		return *value
	}
	return packagesPublicPackageMaxInstancesMin
}

// packagesValidatePublicMaxInstances mirrors validatePublicPackageMaxInstances.
func packagesValidatePublicMaxInstances(value *int32) bool {
	return value != nil && *value >= packagesPublicPackageMaxInstancesMin && *value <= packagesPublicPackageMaxInstancesMax
}

// packagesHostTypeCompatible mirrors isHostTypeCompatibleWithPackage.
func packagesHostTypeCompatible(hostType, packageType string) bool {
	normalizedHostType := hostType
	if normalizedHostType == "" {
		normalizedHostType = "container"
	}
	normalizedPackageType := packageType
	if normalizedPackageType == "" {
		normalizedPackageType = "container"
	}
	return normalizedHostType == "both" || normalizedHostType == normalizedPackageType
}

// packagesValidateNetworkModeForInstanceType mirrors
// validatePackageNetworkModeForInstanceType.
func packagesValidateNetworkModeForInstanceType(instanceType, networkMode *string) string {
	if instanceType != nil && *instanceType == "vm" && networkMode != nil && packagesKVMUnsupportedNetworkModes[*networkMode] {
		return "KVM packages do not support IPv4 NAT & IPv6 NAT or IPv6 NAT network modes"
	}
	return ""
}

// packagesRowsMap turns a db row into the JSON object the TypeScript spreads
// (`...p`), keeping omitempty semantics identical.
func packagesRowsMap(value any) gin.H {
	payload, err := json.Marshal(value)
	if err != nil {
		return gin.H{}
	}
	result := gin.H{}
	if err := json.Unmarshal(payload, &result); err != nil {
		return gin.H{}
	}
	return result
}

// ==================== AJV schema replica ====================

// packagesSchemaKind selects the AJV coercion branch for one property.
type packagesSchemaKind int

const (
	packagesSchemaString packagesSchemaKind = iota
	packagesSchemaNullableString
	packagesSchemaInteger
	packagesSchemaNullableInteger
	packagesSchemaNumber
	packagesSchemaNullableNumber
	packagesSchemaBoolean
	packagesSchemaEnum
	packagesSchemaArray
	packagesSchemaObject
)

// packagesSchemaField is one property of a Fastify body schema.
type packagesSchemaField struct {
	name       string
	kind       packagesSchemaKind
	minLength  int     // -1 unset
	maxLength  int     // -1 unset
	minimum    float64 // NaN unset
	maximum    float64 // NaN unset
	enum       []string
	items      *packagesSchemaField
	minItems   int // -1 unset
	maxItems   int // -1 unset
	additional *packagesSchemaField
}

func packagesStringSchema(name string, minLength int) packagesSchemaField {
	return packagesSchemaField{name: name, kind: packagesSchemaString, minLength: minLength, maxLength: -1, minimum: math.NaN(), maximum: math.NaN(), minItems: -1, maxItems: -1}
}

func packagesNullableStringSchema(name string) packagesSchemaField {
	return packagesSchemaField{name: name, kind: packagesSchemaNullableString, minLength: -1, maxLength: -1, minimum: math.NaN(), maximum: math.NaN(), minItems: -1, maxItems: -1}
}

func packagesIntegerSchema(name string, minimum, maximum float64) packagesSchemaField {
	return packagesSchemaField{name: name, kind: packagesSchemaInteger, minLength: -1, maxLength: -1, minimum: minimum, maximum: maximum, minItems: -1, maxItems: -1}
}

func packagesNullableIntegerSchema(name string, minimum, maximum float64) packagesSchemaField {
	return packagesSchemaField{name: name, kind: packagesSchemaNullableInteger, minLength: -1, maxLength: -1, minimum: minimum, maximum: maximum, minItems: -1, maxItems: -1}
}

func packagesNumberSchema(name string, minimum, maximum float64) packagesSchemaField {
	return packagesSchemaField{name: name, kind: packagesSchemaNumber, minLength: -1, maxLength: -1, minimum: minimum, maximum: maximum, minItems: -1, maxItems: -1}
}

func packagesNullableNumberSchema(name string) packagesSchemaField {
	return packagesSchemaField{name: name, kind: packagesSchemaNullableNumber, minLength: -1, maxLength: -1, minimum: math.NaN(), maximum: math.NaN(), minItems: -1, maxItems: -1}
}

func packagesBooleanSchema(name string) packagesSchemaField {
	return packagesSchemaField{name: name, kind: packagesSchemaBoolean, minLength: -1, maxLength: -1, minimum: math.NaN(), maximum: math.NaN(), minItems: -1, maxItems: -1}
}

func packagesEnumSchema(name string, values ...string) packagesSchemaField {
	return packagesSchemaField{name: name, kind: packagesSchemaEnum, enum: values, minLength: -1, maxLength: -1, minimum: math.NaN(), maximum: math.NaN(), minItems: -1, maxItems: -1}
}

func packagesArraySchema(name string, items packagesSchemaField, minItems, maxItems int) packagesSchemaField {
	return packagesSchemaField{name: name, kind: packagesSchemaArray, items: &items, minItems: minItems, maxItems: maxItems, minLength: -1, maxLength: -1, minimum: math.NaN(), maximum: math.NaN()}
}

func packagesObjectSchema(name string, additional packagesSchemaField) packagesSchemaField {
	return packagesSchemaField{name: name, kind: packagesSchemaObject, additional: &additional, minLength: -1, maxLength: -1, minimum: math.NaN(), maximum: math.NaN(), minItems: -1, maxItems: -1}
}

// packagesValidateSchema reproduces the parts of AJV these routes use: root
// object check, `required` list (checked first, in declaration order), then each
// property in schema order with coerceTypes:'array' semantics and allErrors:false
// (the first failure is the only one reported).
func packagesValidateSchema(decoded any, required []string, fields []packagesSchemaField) (map[string]any, string) {
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, "must be object"
	}
	for _, name := range required {
		if _, present := object[name]; !present {
			return nil, name + " is required"
		}
	}

	coerced := make(map[string]any, len(object))
	for key, value := range object {
		coerced[key] = value
	}
	for _, field := range fields {
		raw, present := object[field.name]
		if !present {
			continue
		}
		value, message := packagesCoerceField(field, raw)
		if message != "" {
			return nil, message
		}
		coerced[field.name] = value
	}
	return coerced, ""
}

// packagesCoerceField applies the coercion and keyword constraints of one
// property, in the order AJV generates them.
func packagesCoerceField(field packagesSchemaField, raw any) (any, string) {
	switch field.kind {
	case packagesSchemaString:
		text, message := ajvCoerceString(raw)
		if message != "" {
			return nil, message
		}
		return text, packagesStringConstraints(field, text)

	case packagesSchemaNullableString:
		value, message := ajvCoerceNullableString(raw)
		if message != "" {
			return nil, message
		}
		if text, ok := value.(string); ok {
			if message := packagesStringConstraints(field, text); message != "" {
				return nil, message
			}
		}
		return value, ""

	case packagesSchemaInteger:
		number, ok := ajvCoerceInteger(raw)
		if !ok {
			return nil, "must be integer"
		}
		if message := packagesNumberConstraints(field, number); message != "" {
			return nil, message
		}
		return number, ""

	case packagesSchemaNullableInteger:
		value, ok := ajvCoerceNullableInteger(raw)
		if !ok {
			return nil, "must be integer,null"
		}
		if number, isNumber := value.(float64); isNumber {
			if message := packagesNumberConstraints(field, number); message != "" {
				return nil, message
			}
		}
		return value, ""

	case packagesSchemaNumber:
		number, message := packagesCoerceNumber(raw)
		if message != "" {
			return nil, message
		}
		if message := packagesNumberConstraints(field, number); message != "" {
			return nil, message
		}
		return number, ""

	case packagesSchemaNullableNumber:
		if raw == nil {
			return nil, ""
		}
		number, message := packagesCoerceNumber(raw)
		if message != "" {
			return nil, "must be number,null"
		}
		if message := packagesNumberConstraints(field, number); message != "" {
			return nil, message
		}
		return number, ""

	case packagesSchemaBoolean:
		value, ok := ajvCoerceBoolean(raw)
		if !ok {
			return nil, "must be boolean"
		}
		return value, ""

	case packagesSchemaEnum:
		text, message := ajvCoerceString(raw)
		if message != "" {
			return nil, message
		}
		if !ajvEnumContains(field.enum, text) {
			return nil, "must be equal to one of the allowed values"
		}
		return text, ""

	case packagesSchemaArray:
		array, ok := raw.([]any)
		if !ok {
			if _, isObject := raw.(map[string]any); isObject {
				return nil, "must be array"
			}
			// coerceTypes:'array' wraps a scalar into a one-element array.
			array = []any{raw}
		}
		if field.maxItems >= 0 && len(array) > field.maxItems {
			return nil, fmt.Sprintf("must NOT have more than %d items", field.maxItems)
		}
		if field.minItems >= 0 && len(array) < field.minItems {
			return nil, fmt.Sprintf("must NOT have fewer than %d items", field.minItems)
		}
		result := make([]any, 0, len(array))
		for _, item := range array {
			value, message := packagesCoerceField(*field.items, item)
			if message != "" {
				return nil, message
			}
			result = append(result, value)
		}
		return result, ""

	case packagesSchemaObject:
		object, ok := raw.(map[string]any)
		if !ok {
			return nil, "must be object"
		}
		result := make(map[string]any, len(object))
		for key, item := range object {
			value, message := packagesCoerceField(*field.additional, item)
			if message != "" {
				return nil, message
			}
			result[key] = value
		}
		return result, ""
	}
	return raw, ""
}

// packagesStringConstraints mirrors app.ts' minLength/maxLength messages.
func packagesStringConstraints(field packagesSchemaField, text string) string {
	length := jsStringLength(text)
	if field.maxLength >= 0 && length > field.maxLength {
		return field.name + " is too long"
	}
	if field.minLength >= 0 && length < field.minLength {
		return field.name + " is too short"
	}
	return ""
}

// packagesNumberConstraints mirrors the generated maximum/minimum checks.
func packagesNumberConstraints(field packagesSchemaField, number float64) string {
	if math.IsInf(number, 0) || math.IsNaN(number) {
		return ""
	}
	if !math.IsNaN(field.maximum) && number > field.maximum {
		return "must be <= " + formatTerminalCommandNumber(field.maximum)
	}
	if !math.IsNaN(field.minimum) && number < field.minimum {
		return "must be >= " + formatTerminalCommandNumber(field.minimum)
	}
	return ""
}

// packagesCoerceNumber mirrors AJV's number coercion.
func packagesCoerceNumber(value any) (float64, string) {
	if number, ok := value.(float64); ok {
		return number, ""
	}
	source, _ := ajvUnwrapSingleArray(value)
	switch typed := source.(type) {
	case bool:
		if typed {
			return 1, ""
		}
		return 0, ""
	case nil:
		return 0, ""
	case string:
		if number, ok := ajvToJSNumber(typed); ok {
			return number, ""
		}
	}
	return 0, "must be number"
}

// ==================== schema tables ====================

// packagesPackageSchemaFields mirrors the body schema of POST / and PATCH /:id
// (identical property sets; only `required` differs).
func packagesPackageSchemaFields() []packagesSchemaField {
	return []packagesSchemaField{
		packagesStringSchema("name", 2),
		packagesStringSchema("description", -1),
		packagesIntegerSchema("cpuMax", 15, math.NaN()),
		packagesIntegerSchema("memoryMax", 128, math.NaN()),
		packagesIntegerSchema("diskMax", 512, math.NaN()),
		packagesIntegerSchema("bandwidthMax", math.NaN(), math.NaN()),
		packagesEnumSchema("networkMode", packagesActiveNetworkModes()...),
		packagesEnumSchema("instanceType", "container", "vm"),
		packagesArraySchema("hostIds", packagesIntegerSchema("hostIds", math.NaN(), math.NaN()), 1, -1),
		packagesObjectSchema("hostStoragePools", packagesNullableStringSchema("hostStoragePools")),
		packagesObjectSchema("hostTrafficMultipliers", packagesNullableNumberSchema("hostTrafficMultipliers")),
		packagesEnumSchema("egressMode", "host", "direct", "profile"),
		packagesNullableIntegerSchema("egressProfileId", 1, packagesPostgresIntMax),
		packagesBooleanSchema("gpuShared"),
		packagesArraySchema("gpuDeviceIds", packagesIntegerSchema("gpuDeviceIds", 1, math.NaN()), -1, 1000),
		packagesBooleanSchema("privileged"),
		packagesBooleanSchema("nested"),
		packagesBooleanSchema("active"),
		packagesStringSchema("monthlyTrafficLimit", -1),
		packagesNumberSchema("trafficResetPrice", 0, packagesMaxTrafficResetPriceYuan),
		packagesIntegerSchema("portLimit", 1, math.NaN()),
		packagesIntegerSchema("snapshotLimit", 0, math.NaN()),
		packagesIntegerSchema("backupLimit", 0, math.NaN()),
		packagesIntegerSchema("siteLimit", 0, math.NaN()),
		packagesEnumSchema("ioLimitMode", "throughput", "iops"),
		packagesStringSchema("limitsRead", -1),
		packagesStringSchema("limitsWrite", -1),
		packagesIntegerSchema("limitsReadIops", 0, math.NaN()),
		packagesIntegerSchema("limitsWriteIops", 0, math.NaN()),
		packagesStringSchema("limitsIngress", -1),
		packagesStringSchema("limitsEgress", -1),
		packagesIntegerSchema("limitsProcesses", 0, math.NaN()),
		packagesIntegerSchema("limitsCpuPriority", 0, 10),
		packagesBooleanSchema("bootAutostart"),
		packagesIntegerSchema("bootAutostartPriority", 0, 100),
		packagesIntegerSchema("bootAutostartDelay", 5, 600),
		packagesIntegerSchema("bootHostShutdownTimeout", 30, 600),
		packagesBooleanSchema("globalShared"),
		packagesNullableNumberSchema("globalQuotaMultiplier"),
		packagesNullableIntegerSchema("globalMaxInstances", math.NaN(), math.NaN()),
		packagesNullableIntegerSchema("requiredPackageId", 1, math.NaN()),
		packagesBooleanSchema("allowInstanceDeletion"),
	}
}

// packagesPatchSchemaFields is the PATCH variant: monthlyTrafficLimit accepts null.
func packagesPatchSchemaFields() []packagesSchemaField {
	fields := packagesPackageSchemaFields()
	for index := range fields {
		if fields[index].name == "monthlyTrafficLimit" {
			fields[index] = packagesNullableStringSchema("monthlyTrafficLimit")
		}
	}
	return fields
}

// packagesReleaseQuotaSchemaFields mirrors POST /:id/release-quota's schema.
func packagesReleaseQuotaSchemaFields() []packagesSchemaField {
	return []packagesSchemaField{
		packagesArraySchema("hostIds", packagesIntegerSchema("hostIds", math.NaN(), math.NaN()), 1, 100),
		packagesIntegerSchema("cpuAdd", 0, 10000),
		packagesIntegerSchema("memoryAdd", 0, 1048576),
		packagesBooleanSchema("notify"),
	}
}

// ==================== body decoding ====================

func packagesDecodedBody(c *gin.Context) (any, bool) {
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return nil, false
	}
	return decoded.Value, true
}

// packagesValidateBody runs the body schema and returns the coerced object the
// handler would see in Fastify.
func packagesValidateBody(c *gin.Context, required []string, fields []packagesSchemaField) (map[string]any, bool) {
	decoded, ok := packagesDecodedBody(c)
	if !ok {
		return nil, false
	}
	coerced, message := packagesValidateSchema(decoded, required, fields)
	if message != "" {
		respondAJVValidation(c, message)
		return nil, false
	}
	return coerced, true
}

// ==================== normalization: packages ====================

// packagesOptionalInteger mirrors optionalPackageInteger.
func packagesOptionalInteger(object map[string]any, key, field string, min, max float64) (*int32, error) {
	raw, present := object[key]
	if !present {
		return nil, nil
	}
	value, err := packagesRequireInteger(raw, field, min, max)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// packagesRequireInteger mirrors requirePackageInteger.
func packagesRequireInteger(value any, field string, min, max float64) (int32, error) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) || math.Abs(number) > 1<<53-1 {
		return 0, packagesValidationFailure(field + " 必须为整数")
	}
	// The route stores int32 columns; a value inside the JS safe-integer range but
	// outside int32 has no representable column value in either backend.
	if number < min || number > max || number > math.MaxInt32 || number < math.MinInt32 {
		return 0, packagesValidationFailure(fmt.Sprintf("%s 必须在 %s-%s 之间", field,
			formatTerminalCommandNumber(min), formatTerminalCommandNumber(max)))
	}
	return int32(number), nil
}

// packagesOptionalBoolean mirrors optionalPackageBoolean.
func packagesOptionalBoolean(object map[string]any, key, field string) (*bool, error) {
	raw, present := object[key]
	if !present {
		return nil, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return nil, packagesValidationFailure(field + " 必须为布尔值")
	}
	return &value, nil
}

// packagesOptionalEnum mirrors optionalPackageEnum.
func packagesOptionalEnum(object map[string]any, key, field string, allowed []string) (*string, error) {
	raw, present := object[key]
	if !present {
		return nil, nil
	}
	text, ok := raw.(string)
	if !ok {
		return nil, packagesValidationFailure(field + " 无效")
	}
	if !ajvEnumContains(allowed, text) {
		return nil, packagesValidationFailure(field + " 无效")
	}
	return &text, nil
}

// packagesOptionalText mirrors optionalPackageText.
func packagesOptionalText(object map[string]any, key, field string, maxLength int) (*string, error) {
	raw, present := object[key]
	if !present {
		return nil, nil
	}
	text, ok := raw.(string)
	if !ok {
		return nil, packagesValidationFailure(field + " 无效")
	}
	trimmed := strings.TrimSpace(text)
	if len([]rune(trimmed)) > maxLength || lib.RemoveDangerousChars(trimmed) != trimmed {
		return nil, packagesValidationFailure(field + " 无效")
	}
	return &trimmed, nil
}

// packagesOptionalNullableString mirrors optionalPackageNullableString. The bool
// distinguishes "absent" from "present with null".
func packagesOptionalNullableString(object map[string]any, key, field string, maxLength int) (*string, bool, error) {
	raw, present := object[key]
	if !present {
		return nil, false, nil
	}
	if raw == nil {
		return nil, true, nil
	}
	text, ok := raw.(string)
	if !ok {
		return nil, true, packagesValidationFailure(field + " 无效")
	}
	trimmed := strings.TrimSpace(text)
	if len([]rune(trimmed)) > maxLength || lib.RemoveDangerousChars(trimmed) != trimmed {
		return nil, true, packagesValidationFailure(field + " 无效")
	}
	return &trimmed, true, nil
}

// packagesOptionalMoneyYuan mirrors optionalMoneyYuan.
func packagesOptionalMoneyYuan(object map[string]any, key, field string, max float64, nullable bool) (*float64, bool, error) {
	raw, present := object[key]
	if !present {
		return nil, false, nil
	}
	if raw == nil && nullable {
		return nil, true, nil
	}
	number, ok := raw.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil, true, packagesValidationFailure(field + " 必须为数字")
	}
	if number < 0 || number > max {
		return nil, true, packagesValidationFailure(fmt.Sprintf("%s 必须在 0-%s 元之间", field, formatTerminalCommandNumber(max)))
	}
	rounded := math.Round(number*100) / 100
	if math.Abs(number-rounded) >= 1e-8 {
		return nil, true, packagesValidationFailure(field + " 最多支持两位小数")
	}
	return &rounded, true, nil
}

// packagesNormalizeHostIDs mirrors normalizePackageHostIds. Duplicate ids are an
// error, not a de-duplication.
func packagesNormalizeHostIDs(object map[string]any, requireHostIDs bool) ([]int32, error) {
	raw, present := object["hostIds"]
	if !present {
		if requireHostIDs {
			return nil, packagesValidationFailure("Package must bind at least one host")
		}
		return nil, nil
	}
	array, ok := raw.([]any)
	if !ok || len(array) == 0 || len(array) > packagesMaxPackageHostBindings {
		return nil, packagesValidationFailure("Package must bind at least one host")
	}
	seen := make(map[int32]bool, len(array))
	result := make([]int32, 0, len(array))
	for _, item := range array {
		number, ok := item.(float64)
		if !ok || math.IsNaN(number) || number != math.Trunc(number) || number <= 0 ||
			math.Abs(number) > 1<<53-1 || number > math.MaxInt32 || seen[int32(number)] {
			return nil, packagesValidationFailure("Invalid package host IDs")
		}
		id := int32(number)
		seen[id] = true
		result = append(result, id)
	}
	return result, nil
}

// packagesNormalizeHostStoragePools mirrors normalizePackageHostStoragePools.
func packagesNormalizeHostStoragePools(object map[string]any) (map[string]*string, error) {
	raw, present := object["hostStoragePools"]
	if !present {
		return nil, nil
	}
	record, ok := raw.(map[string]any)
	if !ok {
		return nil, packagesValidationFailure("hostStoragePools 无效")
	}

	normalized := make(map[string]*string, len(record))
	for hostIDKey, poolName := range record {
		if !packagesPositiveRouteIDPattern.MatchString(hostIDKey) {
			return nil, packagesValidationFailure("hostStoragePools 包含无效宿主机 ID")
		}
		hostID, err := strconv.ParseInt(hostIDKey, 10, 64)
		if err != nil || hostID > 1<<53-1 {
			return nil, packagesValidationFailure("hostStoragePools 包含无效宿主机 ID")
		}
		if poolName == nil {
			normalized[strconv.FormatInt(hostID, 10)] = nil
			continue
		}
		text, ok := poolName.(string)
		if !ok {
			return nil, packagesValidationFailure("存储池名称无效")
		}
		trimmed := strings.TrimSpace(text)
		if len([]rune(trimmed)) > packagesMaxStoragePoolNameLength || lib.RemoveDangerousChars(trimmed) != trimmed {
			return nil, packagesValidationFailure("存储池名称无效")
		}
		if trimmed == "" {
			normalized[strconv.FormatInt(hostID, 10)] = nil
		} else {
			value := trimmed
			normalized[strconv.FormatInt(hostID, 10)] = &value
		}
	}
	return normalized, nil
}

// packagesNormalizeHostTrafficMultipliers mirrors
// normalizePackageHostTrafficMultipliers.
func packagesNormalizeHostTrafficMultipliers(object map[string]any) (map[string]any, map[string]float64, error) {
	raw, present := object["hostTrafficMultipliers"]
	if !present {
		return nil, nil, nil
	}
	record, ok := raw.(map[string]any)
	if !ok {
		return nil, nil, packagesValidationFailure("hostTrafficMultipliers 无效")
	}

	normalized := make(map[string]any, len(record))
	numeric := make(map[string]float64, len(record))
	for hostIDKey, multiplier := range record {
		if !packagesPositiveRouteIDPattern.MatchString(hostIDKey) {
			return nil, nil, packagesValidationFailure("hostTrafficMultipliers 包含无效宿主机 ID")
		}
		hostID, err := strconv.ParseInt(hostIDKey, 10, 64)
		if err != nil || hostID > 1<<53-1 {
			return nil, nil, packagesValidationFailure("hostTrafficMultipliers 包含无效宿主机 ID")
		}
		value, err := lib.NormalizeTrafficMultiplier(multiplier)
		if err != nil {
			return nil, nil, packagesValidationFailure("流量倍率必须在 0.001-100 之间")
		}
		key := strconv.FormatInt(hostID, 10)
		normalized[key] = value
		numeric[key] = value
	}
	return normalized, numeric, nil
}

// packagesNormalizeRequiredID mirrors normalizePackageRequiredId.
func packagesNormalizeRequiredID(object map[string]any) (*int32, bool, error) {
	raw, present := object["requiredPackageId"]
	if !present {
		return nil, false, nil
	}
	if raw == nil {
		return nil, true, nil
	}
	value, err := packagesRequireInteger(raw, "前置套餐", 1, 1<<53-1)
	if err != nil {
		return nil, true, err
	}
	return &value, true, nil
}

// packagesNormalizeEgress mirrors normalizePackageEgress from
// lib/egress/package.ts (NetworkInputError → 400 {"error": message}).
func packagesNormalizeEgress(object map[string]any) (*string, *int32, bool, error) {
	var mode *string
	if raw, present := object["egressMode"]; present {
		text := fmt.Sprint(raw)
		if text != "host" && text != "direct" && text != "profile" {
			return nil, nil, false, packagesInputError{message: "Invalid package egress mode"}
		}
		mode = &text
	}

	var profileID *int32
	profilePresent := false
	if raw, present := object["egressProfileId"]; present {
		profilePresent = true
		if raw != nil {
			number, ok := raw.(float64)
			if !ok || number != math.Trunc(number) || number <= 0 || number > packagesPostgresIntMax || math.Abs(number) > 1<<53-1 {
				return nil, nil, false, packagesInputError{message: "Invalid egress profile"}
			}
			value := int32(number)
			profileID = &value
		}
	}
	return mode, profileID, profilePresent, nil
}

// packagesNormalizedPackage is the server-side normalisation of the package body.
// Tri-state is explicit: a nil pointer with a *Set bool true means "explicitly
// null" (only possible for the nullable columns).
type packagesNormalizedPackage struct {
	EgressMode              *string
	EgressProfileID         *int32
	EgressProfileSet        bool
	GPUShared               *bool
	GPUSharedSet            bool
	GPUDeviceIDs            []int32
	GPUDeviceIDsSet         bool
	Name                    *string
	Description             *string
	DescriptionSet          bool
	CPUMax                  *int32
	MemoryMax               *int32
	DiskMax                 *int32
	BandwidthMax            *int32
	NetworkMode             *string
	InstanceType            *string
	HostIDs                 []int32
	HostIDsSet              bool
	HostStoragePools        map[string]*string
	HostTrafficRaw          map[string]any
	HostTraffic             map[string]float64
	Privileged              *bool
	Nested                  *bool
	Active                  *bool
	MonthlyTraffic          *string
	MonthlyTrafficSet       bool
	TrafficResetPrice       *float64
	TrafficResetSet         bool
	PortLimit               *int32
	SnapshotLimit           *int32
	BackupLimit             *int32
	SiteLimit               *int32
	IOLimitMode             *string
	LimitsRead              *string
	LimitsWrite             *string
	LimitsReadIops          *int32
	LimitsWriteIops         *int32
	LimitsIngress           *string
	LimitsEgress            *string
	LimitsProcesses         *int32
	LimitsCPUPriority       *int32
	BootAutostart           *bool
	BootAutostartPriority   *int32
	BootAutostartDelay      *int32
	BootHostShutdownTimeout *int32
	GlobalShared            *bool
	GlobalMaxInstances      *int32
	GlobalMaxSet            bool
	RequiredPackageID       *int32
	RequiredPackageSet      bool
}

// packagesNormalizeInput mirrors normalizePackageInput, in the same validation
// order. Scalar presence is read from the coerced schema object, so the
// TypeScript's "key absent vs explicit null" branches are preserved.
func packagesNormalizeInput(object map[string]any, requireAll bool) (*packagesNormalizedPackage, error) {
	if object == nil {
		return nil, packagesValidationFailure("请求参数无效")
	}

	if requireAll {
		for _, field := range []string{"name", "cpuMax", "memoryMax", "diskMax"} {
			if _, present := object[field]; !present {
				return nil, packagesValidationFailure("请填写所有必填字段")
			}
		}
	}

	normalized := &packagesNormalizedPackage{}

	shared, sharedPresent, deviceIDs, deviceIDsPresent, err := lib.NormalizeGpuInput(object, false)
	if err != nil {
		return nil, err
	}
	normalized.GPUShared, normalized.GPUSharedSet = shared, sharedPresent
	normalized.GPUDeviceIDs, normalized.GPUDeviceIDsSet = deviceIDs, deviceIDsPresent

	if raw, present := object["name"]; present {
		name, ok := raw.(string)
		if !ok {
			return nil, packagesValidationFailure("Package name cannot be empty")
		}
		validation := lib.ValidateName(name, "Package name", 2, 64)
		if !validation.Valid {
			return nil, packagesValidationFailure(packagesValidationMessage(validation))
		}
		normalized.Name = validation.Sanitized
	}

	if raw, present := object["description"]; present {
		text, ok := raw.(string)
		if !ok {
			return nil, packagesValidationFailure("Package description 无效")
		}
		validation := lib.ValidateText(text, "Package description", packagesMaxPackageDescriptionLength)
		if !validation.Valid {
			return nil, packagesValidationFailure(packagesValidationMessage(validation))
		}
		if validation.Sanitized != nil {
			normalized.Description = validation.Sanitized
		} else {
			empty := ""
			normalized.Description = &empty
		}
		normalized.DescriptionSet = true
	}

	if normalized.CPUMax, err = packagesOptionalInteger(object, "cpuMax", "CPU", 15, 10000); err != nil {
		return nil, err
	}
	if normalized.MemoryMax, err = packagesOptionalInteger(object, "memoryMax", "内存", 128, 1048576); err != nil {
		return nil, err
	}
	if normalized.DiskMax, err = packagesOptionalInteger(object, "diskMax", "磁盘", 512, 104857600); err != nil {
		return nil, err
	}
	if normalized.BandwidthMax, err = packagesOptionalInteger(object, "bandwidthMax", "带宽", 0, packagesPostgresIntMax); err != nil {
		return nil, err
	}
	if normalized.NetworkMode, err = packagesOptionalEnum(object, "networkMode", "网络模式", packagesActiveNetworkModes()); err != nil {
		return nil, err
	}
	if normalized.InstanceType, err = packagesOptionalEnum(object, "instanceType", "实例类型", []string{"container", "vm"}); err != nil {
		return nil, err
	}

	mode, profileID, profilePresent, err := packagesNormalizeEgress(object)
	if err != nil {
		return nil, err
	}
	normalized.EgressMode = mode
	normalized.EgressProfileID = profileID
	normalized.EgressProfileSet = profilePresent

	hostIDs, err := packagesNormalizeHostIDs(object, requireAll)
	if err != nil {
		return nil, err
	}
	if hostIDs != nil {
		normalized.HostIDs = hostIDs
		normalized.HostIDsSet = true
	}
	if normalized.HostStoragePools, err = packagesNormalizeHostStoragePools(object); err != nil {
		return nil, err
	}
	if normalized.HostTrafficRaw, normalized.HostTraffic, err = packagesNormalizeHostTrafficMultipliers(object); err != nil {
		return nil, err
	}

	if normalized.Privileged, err = packagesOptionalBoolean(object, "privileged", "privileged"); err != nil {
		return nil, err
	}
	if normalized.Nested, err = packagesOptionalBoolean(object, "nested", "nested"); err != nil {
		return nil, err
	}
	if normalized.Active, err = packagesOptionalBoolean(object, "active", "active"); err != nil {
		return nil, err
	}

	normalized.MonthlyTraffic, normalized.MonthlyTrafficSet, err =
		packagesOptionalNullableString(object, "monthlyTrafficLimit", "月流量限制", 64)
	if err != nil {
		return nil, err
	}
	normalized.TrafficResetPrice, normalized.TrafficResetSet, err =
		packagesOptionalMoneyYuan(object, "trafficResetPrice", "流量重置价格", packagesMaxTrafficResetPriceYuan, false)
	if err != nil {
		return nil, err
	}

	if normalized.PortLimit, err = packagesOptionalInteger(object, "portLimit", "端口映射数", 1, packagesPostgresIntMax); err != nil {
		return nil, err
	}
	if normalized.SnapshotLimit, err = packagesOptionalInteger(object, "snapshotLimit", "快照数", 0, packagesPostgresIntMax); err != nil {
		return nil, err
	}
	if normalized.BackupLimit, err = packagesOptionalInteger(object, "backupLimit", "备份数", 0, packagesPostgresIntMax); err != nil {
		return nil, err
	}
	if normalized.SiteLimit, err = packagesOptionalInteger(object, "siteLimit", "反代站点数", 0, packagesPostgresIntMax); err != nil {
		return nil, err
	}

	if normalized.IOLimitMode, err = packagesOptionalEnum(object, "ioLimitMode", "I/O限制模式", []string{"throughput", "iops"}); err != nil {
		return nil, err
	}
	if normalized.LimitsRead, err = packagesOptionalText(object, "limitsRead", "读取限制", packagesMaxLimitStringLength); err != nil {
		return nil, err
	}
	if normalized.LimitsWrite, err = packagesOptionalText(object, "limitsWrite", "写入限制", packagesMaxLimitStringLength); err != nil {
		return nil, err
	}
	if normalized.LimitsReadIops, err = packagesOptionalInteger(object, "limitsReadIops", "读取 IOPS", 0, packagesPostgresIntMax); err != nil {
		return nil, err
	}
	if normalized.LimitsWriteIops, err = packagesOptionalInteger(object, "limitsWriteIops", "写入 IOPS", 0, packagesPostgresIntMax); err != nil {
		return nil, err
	}
	if normalized.LimitsIngress, err = packagesOptionalText(object, "limitsIngress", "入站带宽限制", packagesMaxLimitStringLength); err != nil {
		return nil, err
	}
	if normalized.LimitsEgress, err = packagesOptionalText(object, "limitsEgress", "出站带宽限制", packagesMaxLimitStringLength); err != nil {
		return nil, err
	}
	if normalized.LimitsProcesses, err = packagesOptionalInteger(object, "limitsProcesses", "进程限制", 0, packagesPostgresIntMax); err != nil {
		return nil, err
	}
	if normalized.LimitsCPUPriority, err = packagesOptionalInteger(object, "limitsCpuPriority", "CPU优先级", 0, 10); err != nil {
		return nil, err
	}

	if normalized.BootAutostart, err = packagesOptionalBoolean(object, "bootAutostart", "bootAutostart"); err != nil {
		return nil, err
	}
	if normalized.BootAutostartPriority, err = packagesOptionalInteger(object, "bootAutostartPriority", "启动优先级", 0, 100); err != nil {
		return nil, err
	}
	if normalized.BootAutostartDelay, err = packagesOptionalInteger(object, "bootAutostartDelay", "启动延迟", 5, 600); err != nil {
		return nil, err
	}
	if normalized.BootHostShutdownTimeout, err = packagesOptionalInteger(object, "bootHostShutdownTimeout", "关机超时", 30, 600); err != nil {
		return nil, err
	}

	if normalized.GlobalShared, err = packagesOptionalBoolean(object, "globalShared", "globalShared"); err != nil {
		return nil, err
	}
	if raw, present := object["globalQuotaMultiplier"]; present {
		if raw != nil {
			if _, isNumber := raw.(float64); !isNumber {
				return nil, packagesValidationFailure("全局配额倍数无效")
			}
		}
		// The value is deliberately discarded: global quota multipliers were retired
		// and are always stored as NULL.
	}
	if raw, present := object["globalMaxInstances"]; present {
		normalized.GlobalMaxSet = true
		if raw != nil {
			value, err := packagesRequireInteger(raw, "公开套餐最大实例数",
				packagesPublicPackageMaxInstancesMin, packagesPublicPackageMaxInstancesMax)
			if err != nil {
				return nil, err
			}
			normalized.GlobalMaxInstances = &value
		}
	}

	normalized.RequiredPackageID, normalized.RequiredPackageSet, err = packagesNormalizeRequiredID(object)
	if err != nil {
		return nil, err
	}

	return normalized, nil
}

// packagesValidationMessage renders lib.InputValidationResult.Message.
func packagesValidationMessage(result lib.InputValidationResult) string {
	return result.Message
}

// packagesActiveNetworkModes mirrors ACTIVE_PACKAGE_NETWORK_MODES as strings for
// the schema enum helper.
func packagesActiveNetworkModes() []string {
	modes := make([]string, 0, len(lib.ActivePackageNetworkModes))
	for _, mode := range lib.ActivePackageNetworkModes {
		modes = append(modes, string(mode))
	}
	return modes
}

// ==================== normalization: plans ====================

// packagesPlanInput mirrors NormalizedPackagePlanInput.
type packagesPlanInput struct {
	GPUShared         *bool
	GPUSharedSet      bool
	GPUDeviceIDs      []int32
	GPUDeviceIDsSet   bool
	Name              *string
	Description       *string
	DescriptionSet    bool
	CPU               *int32
	Memory            *int32
	Disk              *int32
	PortLimit         *int32
	SnapshotLimit     *int32
	BackupLimit       *int32
	SiteLimit         *int32
	SwapSize          *int32
	TrafficLimit      any
	TrafficLimitSet   bool
	TrafficLimitSpeed any
	TrafficSpeedSet   bool
	Price             *int32
	TrafficResetPrice *float64
	TrafficResetSet   bool
	BillingCycle      *int32
	SortOrder         *int32
	IsActive          *bool
	IsSoldOut         *bool
	SlaGuarantee      *float64
	SlaGuaranteeSet   bool
}

// packagesValidatePlanName mirrors validatePackagePlanName.
func packagesValidatePlanName(value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", packagesValidationFailure("方案名称不能为空")
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", packagesValidationFailure("方案名称不能为空")
	}
	if len([]rune(trimmed)) > packagesMaxPlanNameLength {
		return "", packagesValidationFailure(fmt.Sprintf("方案名称不能超过 %d 个字符", packagesMaxPlanNameLength))
	}
	if packagesControlCharsPattern.MatchString(trimmed) || lib.RemoveDangerousChars(trimmed) != trimmed {
		return "", packagesValidationFailure("方案名称包含非法字符")
	}
	return trimmed, nil
}

// packagesPlanOptionalInteger mirrors optionalPlanInteger.
func packagesPlanOptionalInteger(object map[string]any, key, field string, min, max float64) (*int32, error) {
	raw, present := object[key]
	if !present {
		return nil, nil
	}
	value, err := packagesRequireInteger(raw, field, min, max)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// packagesPlanOptionalBoolean mirrors optionalPlanBoolean.
func packagesPlanOptionalBoolean(object map[string]any, key, field string) (*bool, error) {
	raw, present := object[key]
	if !present {
		return nil, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return nil, packagesValidationFailure(field + " 必须为布尔值")
	}
	return &value, nil
}

// packagesNormalizePlanSlaGuarantee mirrors normalizePackagePlanSlaGuarantee.
func packagesNormalizePlanSlaGuarantee(value any, present bool) (*float64, bool, error) {
	if !present {
		return nil, false, nil
	}
	if value == nil {
		return nil, true, nil
	}
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil, true, packagesValidationFailure("SLA保证必须为数字")
	}
	if number < 1 || number > 100 {
		return nil, true, packagesValidationFailure("SLA保证必须在 1-100 之间")
	}
	rounded := math.Round(number*100) / 100
	if math.Abs(number-rounded) > 1e-8 {
		return nil, true, packagesValidationFailure("SLA保证最多支持两位小数")
	}
	return &rounded, true, nil
}

// packagesNormalizePlanDescription mirrors normalizePackagePlanDescription.
func packagesNormalizePlanDescription(value any, present bool) (*string, bool, error) {
	if !present {
		return nil, false, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, true, packagesValidationFailure("方案描述无效")
	}
	trimmed := strings.TrimSpace(text)
	if len([]rune(trimmed)) > packagesMaxPackageDescriptionLength {
		return nil, true, packagesValidationFailure("方案描述不能超过 500 个字符")
	}
	return &trimmed, true, nil
}

// packagesNormalizePlanInput mirrors normalizePackagePlanInput.
func packagesNormalizePlanInput(input any, requireAll, isVM bool) (*packagesPlanInput, error) {
	object, ok := input.(map[string]any)
	if !ok {
		return nil, packagesValidationFailure("请求参数无效")
	}

	normalized := &packagesPlanInput{}

	shared, sharedPresent, deviceIDs, deviceIDsPresent, err := lib.NormalizeGpuInput(object, true)
	if err != nil {
		return nil, err
	}
	normalized.GPUShared, normalized.GPUSharedSet = shared, sharedPresent
	normalized.GPUDeviceIDs, normalized.GPUDeviceIDsSet = deviceIDs, deviceIDsPresent

	if requireAll {
		for _, field := range []string{"name", "cpu", "memory", "disk", "portLimit", "snapshotLimit", "backupLimit", "siteLimit", "trafficLimit", "price"} {
			if _, present := object[field]; !present {
				return nil, packagesValidationFailure("请填写所有必填字段")
			}
		}
		if !isVM {
			if _, present := object["swapSize"]; !present {
				return nil, packagesValidationFailure("请填写所有必填字段")
			}
		}
	}

	if raw, present := object["name"]; present {
		name, err := packagesValidatePlanName(raw)
		if err != nil {
			return nil, err
		}
		normalized.Name = &name
	}

	if normalized.Description, normalized.DescriptionSet, err = packagesNormalizePlanDescription(object["description"], packagesHasKey(object, "description")); err != nil {
		return nil, err
	}

	if normalized.CPU, err = packagesPlanOptionalInteger(object, "cpu", "CPU", 15, 10000); err != nil {
		return nil, err
	}
	if normalized.Memory, err = packagesPlanOptionalInteger(object, "memory", "内存", 128, 62144); err != nil {
		return nil, err
	}
	if normalized.Disk, err = packagesPlanOptionalInteger(object, "disk", "磁盘", 512, 104857600); err != nil {
		return nil, err
	}
	if normalized.PortLimit, err = packagesPlanOptionalInteger(object, "portLimit", "端口映射数", 0, packagesPostgresIntMax); err != nil {
		return nil, err
	}
	if normalized.SnapshotLimit, err = packagesPlanOptionalInteger(object, "snapshotLimit", "快照数", 0, packagesPostgresIntMax); err != nil {
		return nil, err
	}
	if normalized.BackupLimit, err = packagesPlanOptionalInteger(object, "backupLimit", "备份数", 0, packagesPostgresIntMax); err != nil {
		return nil, err
	}
	if normalized.SiteLimit, err = packagesPlanOptionalInteger(object, "siteLimit", "反代站点数", 0, packagesPostgresIntMax); err != nil {
		return nil, err
	}
	if isVM {
		zero := int32(0)
		normalized.SwapSize = &zero
	} else if normalized.SwapSize, err = packagesPlanOptionalInteger(object, "swapSize", "SWAP", 0, 1048576); err != nil {
		return nil, err
	}
	if normalized.Price, err = packagesPlanOptionalInteger(object, "price", "价格", 0, packagesMaxPlanPriceCents); err != nil {
		return nil, err
	}
	if normalized.TrafficResetPrice, normalized.TrafficResetSet, err =
		packagesOptionalMoneyYuan(object, "trafficResetPrice", "流量重置价格", packagesMaxTrafficResetPriceYuan, true); err != nil {
		return nil, err
	}
	if normalized.BillingCycle, err = packagesPlanOptionalInteger(object, "billingCycle", "计费周期", 1, packagesMaxPlanBillingCycleMonths); err != nil {
		return nil, err
	}
	if normalized.SortOrder, err = packagesPlanOptionalInteger(object, "sortOrder", "排序值", -packagesMaxPlanSortOrder, packagesMaxPlanSortOrder); err != nil {
		return nil, err
	}
	if normalized.IsActive, err = packagesPlanOptionalBoolean(object, "isActive", "isActive"); err != nil {
		return nil, err
	}
	if normalized.IsSoldOut, err = packagesPlanOptionalBoolean(object, "isSoldOut", "isSoldOut"); err != nil {
		return nil, err
	}
	if normalized.SlaGuarantee, normalized.SlaGuaranteeSet, err =
		packagesNormalizePlanSlaGuarantee(object["slaGuarantee"], packagesHasKey(object, "slaGuarantee")); err != nil {
		return nil, err
	}

	if raw, present := object["trafficLimit"]; present {
		normalized.TrafficLimit = raw
		normalized.TrafficLimitSet = true
	}
	if raw, present := object["trafficLimitSpeed"]; present {
		normalized.TrafficLimitSpeed = raw
		normalized.TrafficSpeedSet = true
	}

	return normalized, nil
}

func packagesHasKey(object map[string]any, key string) bool {
	_, present := object[key]
	return present
}

// ==================== normalization: shares ====================

// packagesShareInput mirrors NormalizedPackageShareInput.
type packagesShareInput struct {
	FriendID        *int32
	QuotaMultiplier *float64
	QuotaSet        bool
	MaxInstances    *int32
	MaxSet          bool
}

// packagesNormalizeShareQuotaMultiplier mirrors normalizePackageShareQuotaMultiplier.
func packagesNormalizeShareQuotaMultiplier(value any, present bool) (*float64, bool, error) {
	if !present {
		return nil, false, nil
	}
	if value == nil {
		return nil, true, nil
	}
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil, true, packagesValidationFailure("配额倍数必须为数字")
	}
	if number < 0.1 || number > 99.9 {
		return nil, true, packagesValidationFailure("配额倍数必须在 0.1-99.9 之间")
	}
	rounded := math.Round(number*10) / 10
	if math.Abs(number-rounded) > 1e-8 {
		return nil, true, packagesValidationFailure("配额倍数最多支持一位小数")
	}
	return &rounded, true, nil
}

// packagesNormalizeShareInput mirrors normalizePackageShareInput.
func packagesNormalizeShareInput(input any, requireFriendID bool) (*packagesShareInput, error) {
	object, ok := input.(map[string]any)
	if !ok {
		return nil, packagesValidationFailure("请求参数无效")
	}

	normalized := &packagesShareInput{}
	if requireFriendID {
		raw, present := object["friendId"]
		if !present {
			return nil, packagesValidationFailure("好友 ID 必须为整数")
		}
		value, err := packagesRequireInteger(raw, "好友 ID", 1, 1<<53-1)
		if err != nil {
			return nil, err
		}
		normalized.FriendID = &value
	} else if _, present := object["friendId"]; present {
		return nil, packagesValidationFailure("请求参数无效")
	}

	quota, set, err := packagesNormalizeShareQuotaMultiplier(object["quotaMultiplier"], packagesHasKey(object, "quotaMultiplier"))
	if err != nil {
		return nil, err
	}
	normalized.QuotaMultiplier, normalized.QuotaSet = quota, set

	if raw, present := object["maxInstances"]; present {
		normalized.MaxSet = true
		if raw != nil {
			value, err := packagesRequireInteger(raw, "共享最大实例数", 0, packagesMaxPackageHostBindings)
			if err != nil {
				return nil, err
			}
			normalized.MaxInstances = &value
		}
	}

	return normalized, nil
}

// ==================== plan traffic limit speed ====================

// packagesNormalizePlanTrafficLimitSpeed mirrors
// services/traffic-bandwidth.ts#normalizePlanTrafficLimitSpeed. The second return
// value is false for the TypeScript's `undefined` (invalid) outcome; a nil string
// with true means "null".
func packagesNormalizePlanTrafficLimitSpeed(value any) (*string, bool) {
	if value == nil {
		return nil, true
	}
	text, ok := value.(string)
	if !ok {
		return nil, false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || trimmed == "0" {
		return nil, true
	}

	if match := packagesBandwidthUnitPattern.FindStringSubmatch(trimmed); match != nil {
		number, err := strconv.ParseFloat(match[1], 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 {
			return nil, false
		}
		multiplier := 1.0
		if strings.EqualFold(match[2], "Gbit") {
			multiplier = 1000
		}
		mbps := math.Round(number * multiplier)
		if mbps <= 0 {
			return nil, false
		}
		result := strconv.FormatInt(int64(mbps), 10) + "Mbit"
		return &result, true
	}

	if packagesDigitsPattern.MatchString(trimmed) {
		bytes := new(big.Int)
		bytes.SetString(trimmed, 10)
		mbps := new(big.Int).Quo(bytes, big.NewInt(1024*1024))
		if mbps.Sign() <= 0 {
			return nil, true
		}
		result := mbps.String() + "Mbit"
		return &result, true
	}

	return nil, false
}

var (
	packagesBandwidthUnitPattern = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*(Mbit|Gbit)$`)
	packagesDigitsPattern        = regexp.MustCompile(`^\d+$`)
)

// packagesTrafficLimitSpeedValue applies `normalized ?? '0'`.
func packagesTrafficLimitSpeedValue(value *string) string {
	if value == nil {
		return "0"
	}
	return *value
}

// ==================== host validation ====================

// packagesHostRow is the projection the host compatibility checks need.
type packagesHostRow struct {
	ID           int32  `gorm:"column:id"`
	Name         string `gorm:"column:name"`
	InstanceType string `gorm:"column:instance_type"`
}

// packagesIncompatibleHosts mirrors getIncompatiblePackageHosts.
func packagesIncompatibleHosts(ctx context.Context, hostIDs []int32, instanceType string) ([]gin.H, error) {
	unique := make([]int32, 0, len(hostIDs))
	seen := make(map[int32]bool, len(hostIDs))
	for _, id := range hostIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return nil, nil
	}

	var hosts []packagesHostRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, name, instance_type::text AS instance_type FROM hosts
		 WHERE id IN (`+packagesPlaceholders(len(unique))+`) ORDER BY id ASC`,
		packagesArgs(unique)...).Scan(&hosts).Error; err != nil {
		return nil, err
	}

	result := make([]gin.H, 0, len(hosts))
	for _, host := range hosts {
		if packagesHostTypeCompatible(host.InstanceType, instanceType) {
			continue
		}
		hostType := host.InstanceType
		if hostType == "" {
			hostType = "container"
		}
		result = append(result, gin.H{"id": host.ID, "name": host.Name, "instanceType": hostType})
	}
	return result, nil
}

// packagesHostsWithoutSystemDiskPool mirrors getHostsWithoutSystemDiskPool.
func packagesHostsWithoutSystemDiskPool(ctx context.Context, hostIDs []int32, packageID *int32, hostStoragePools map[string]*string) ([]gin.H, error) {
	unique := make([]int32, 0, len(hostIDs))
	seen := make(map[int32]bool, len(hostIDs))
	for _, id := range hostIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return nil, nil
	}

	var hosts []packagesHostRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, name FROM hosts WHERE id IN (`+packagesPlaceholders(len(unique))+`)`,
		packagesArgs(unique)...).Scan(&hosts).Error; err != nil {
		return nil, err
	}
	hostNameByID := make(map[int32]string, len(hosts))
	for _, host := range hosts {
		hostNameByID[host.ID] = host.Name
	}

	unavailable := make([]gin.H, 0, len(unique))
	for _, hostID := range unique {
		hostName, ok := hostNameByID[hostID]
		if !ok {
			hostName = "#" + strconv.FormatInt(int64(hostID), 10)
		}

		key := strconv.FormatInt(int64(hostID), 10)
		explicitPool, hasExplicit := hostStoragePools[key]
		if hasExplicit {
			if explicitPool != nil && *explicitPool != "" {
				ok, err := db.StoragePoolsIsSystemDiskPool(ctx, hostID, *explicitPool)
				if err != nil {
					return nil, err
				}
				if !ok {
					unavailable = append(unavailable, gin.H{
						"id":     hostID,
						"name":   hostName,
						"reason": "存储池 " + *explicitPool + " 不存在或不是系统盘池",
					})
				}
			} else {
				pool, err := db.StoragePoolsGetRandomSystemDiskPool(ctx, hostID)
				if err != nil {
					return nil, err
				}
				if pool == nil {
					unavailable = append(unavailable, gin.H{"id": hostID, "name": hostName, "reason": "未配置系统盘存储池"})
				}
			}
			continue
		}

		resolved, err := db.StoragePoolsResolveNewInstancePool(ctx, hostID, packageID)
		if err != nil {
			return nil, err
		}
		if resolved == nil {
			unavailable = append(unavailable, gin.H{"id": hostID, "name": hostName, "reason": "未配置系统盘存储池"})
		}
	}
	return unavailable, nil
}

// packagesIncompatibleHostsResponse mirrors incompatiblePackageHostsResponse.
func packagesIncompatibleHostsResponse(hosts []gin.H, instanceType string) gin.H {
	if instanceType == "" {
		instanceType = "container"
	}
	return gin.H{
		"error": fmt.Sprintf("Package instance type %q is not supported by the selected host", instanceType),
		"code":  "HOST_INSTANCE_TYPE_MISMATCH",
		"hosts": hosts,
	}
}

// packagesStoragePoolUnavailableResponse mirrors storagePoolUnavailableResponse.
func packagesStoragePoolUnavailableResponse(hosts []gin.H) gin.H {
	return gin.H{
		"error": "Selected hosts are missing instance data storage pools",
		"code":  "HOST_STORAGE_POOL_UNAVAILABLE",
		"hosts": hosts,
	}
}

// packagesPlaceholders renders `?, ?, ...` for a GORM IN list.
func packagesPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", count), ", ")
}

func packagesArgs(values []int32) []any {
	args := make([]any, 0, len(values))
	for _, value := range values {
		args = append(args, value)
	}
	return args
}

// packagesUserID64 converts the int32 claim to the int64 log-signature form.
func packagesUserID64(userID int32) *int64 {
	value := int64(userID)
	return &value
}

// ==================== routes ====================

// RegisterPackages mounts the 19 package routes.
func RegisterPackages(r *gin.Engine) {
	group := r.Group("/api/packages")

	// 公开 API（无需登录，带限流）
	group.GET("/public", packagesPublicList)
	group.GET("/public/regions", packagesPublicRegions)

	// 套餐列表：Fastify prefixTrailingSlash='both' → 两种写法都路由到这里
	group.GET("", middleware.Authenticate(), packagesList)
	group.GET("/", middleware.Authenticate(), packagesList)
	group.POST("", middleware.Authenticate(), packagesCreate)
	group.POST("/", middleware.Authenticate(), packagesCreate)

	// 我的共享 / 地区（静态段必须先于 :id 声明，可读性用）
	group.GET("/my-shares", middleware.AuthenticateUser(), packagesMyShares)
	group.GET("/regions", middleware.Authenticate(), packagesRegions)

	// 套餐详情
	group.GET("/:id", middleware.Authenticate(), packagesDetail)

	// 更新 / 删除
	group.PATCH("/:id", middleware.Authenticate(), packagesUpdate)
	group.DELETE("/:id", middleware.Authenticate(), packagesDelete)

	// 套餐共享（好友系统已移除，仍保留旧接口）
	group.POST("/:id/share", middleware.AuthenticateUser(), packagesShareCreate)
	group.DELETE("/:id/share/:userId", middleware.AuthenticateUser(), packagesShareDelete)
	group.PATCH("/:id/shares/:shareId", middleware.AuthenticateUser(), packagesShareUpdate)
	group.GET("/:id/shares", middleware.AuthenticateUser(), packagesShareList)

	// 资源释放
	group.GET("/:id/hosts-detail", middleware.Authenticate(), packagesHostsDetail)
	group.POST("/:id/release-quota", middleware.Authenticate(), packagesReleaseQuota)

	// 套餐方案管理
	group.GET("/:id/plans", middleware.Authenticate(), packagesPlansList)
	group.POST("/:id/plans", middleware.Authenticate(), packagesPlansCreate)
	group.PUT("/:id/plans/:planId", middleware.Authenticate(), packagesPlansUpdate)
	group.DELETE("/:id/plans/:planId", middleware.Authenticate(), packagesPlansDelete)
}

func init() {
	Register(RegisterPackages)
}

// ==================== GET /public ====================

// packagesPublicList mirrors GET /public.
func packagesPublicList(c *gin.Context) {
	ctx := c.Request.Context()
	source := c.DefaultQuery("source", "all")

	query := db.DB.WithContext(ctx).Model(&models.Package{}).
		Where("packages.active = ? AND packages.global_shared = ?", true, true)
	if source == "official" {
		query = query.Joins("JOIN users AS owner ON owner.id = packages.user_id AND owner.role::text = ?", "admin")
	}

	// Explicit projection: gpu_device_ids is an int4[] column, and GORM's JSON
	// serializer cannot scan it (same workaround as db/packages.go, which reads
	// array_to_json(...)::text instead). The public payload does not need it.
	var packages []models.Package
	if err := query.
		Select(`packages.id, packages.user_id, packages.name, packages.description,
			packages.cpu_max, packages.memory_max, packages.disk_max,
			packages.monthly_traffic_limit, packages.traffic_reset_price,
			packages.network_mode, packages.instance_type, packages.privileged,
			packages.nested, packages.global_max_instances`).
		Order("packages.name ASC").Find(&packages).Error; err != nil {
		packagesRespondInternal(c)
		return
	}

	packageIDs := make([]int32, 0, len(packages))
	for _, pkg := range packages {
		packageIDs = append(packageIDs, pkg.ID)
	}
	hostIDsByPackage, err := packagesHostIDsByPackage(ctx, packageIDs)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	soldOutByPackage, err := db.CheckPackagesSoldOut(ctx, packageIDs)
	if err != nil {
		packagesRespondInternal(c)
		return
	}

	type entry struct {
		soldOut bool
		body    gin.H
	}
	entries := make([]entry, 0, len(packages))
	for _, pkg := range packages {
		plans, err := db.GetPackagePlansByPackageId(ctx, pkg.ID, db.PackagePlansOptions{ActiveOnly: true})
		if err != nil {
			packagesRespondInternal(c)
			return
		}

		planViews := make([]gin.H, 0, len(plans))
		for _, plan := range plans {
			swapSize := plan.SwapSize
			if string(pkg.InstanceType) == "vm" {
				swapSize = 0
			}
			planViews = append(planViews, gin.H{
				"id":                plan.ID,
				"name":              plan.Name,
				"description":       plan.Description,
				"cpu":               plan.CPU,
				"memory":            plan.Memory,
				"disk":              plan.Disk,
				"portLimit":         plan.PortLimit,
				"snapshotLimit":     plan.SnapshotLimit,
				"backupLimit":       plan.BackupLimit,
				"siteLimit":         plan.SiteLimit,
				"swapSize":          swapSize,
				"trafficLimit":      strconv.FormatInt(plan.TrafficLimit, 10),
				"trafficLimitSpeed": plan.TrafficLimitSpeed,
				"price":             packagesDecimalText(plan.Price),
				"billingCycle":      plan.BillingCycle,
				"setupFee":          packagesDecimalText(plan.SetupFee),
				"isSoldOut":         plan.IsSoldOut,
				"slaGuarantee":      packagesNullableDecimalText(plan.SlaGuarantee),
				"monthlyPrice":      db.PackagePlanCalculateMonthlyPrice(plan),
			})
		}

		monthlyTrafficLimit := any(nil)
		if pkg.MonthlyTrafficLimit != nil {
			monthlyTrafficLimit = strconv.FormatInt(*pkg.MonthlyTrafficLimit, 10)
		}
		soldOut, known := soldOutByPackage[pkg.ID]
		if !known {
			soldOut = true
		}

		body := gin.H{
			"id":                      pkg.ID,
			"name":                    pkg.Name,
			"description":             pkg.Description,
			"cpu_max":                 pkg.CPUMax,
			"memory_max":              pkg.MemoryMax,
			"disk_max":                pkg.DiskMax,
			"monthly_traffic_limit":   monthlyTrafficLimit,
			"traffic_reset_price":     packagesDecimalNumber(pkg.TrafficResetPrice),
			"network_mode":            string(pkg.NetworkMode),
			"instance_type":           string(pkg.InstanceType),
			"host_ids":                packagesInt32Slice(hostIDsByPackage[pkg.ID]),
			"privileged":              pkg.Privileged,
			"nested":                  pkg.Nested,
			"global_quota_multiplier": nil,
			"global_max_instances":    packagesNormalizePublicMaxInstances(pkg.GlobalMaxInstances),
			"required_package_id":     nil,
			"required_package_name":   nil,
			"sourceType":              "official",
			"soldOut":                 soldOut,
			"isPaid":                  len(plans) > 0,
			"plans":                   planViews,
		}
		entries = append(entries, entry{soldOut: soldOut, body: body})
	}

	// 售罄的套餐排在最后（stable，同组保持 name ASC）。
	sort.SliceStable(entries, func(left, right int) bool {
		if entries[left].soldOut == entries[right].soldOut {
			return false
		}
		return !entries[left].soldOut
	})

	packagesOut := make([]gin.H, 0, len(entries))
	for _, item := range entries {
		packagesOut = append(packagesOut, item.body)
	}
	c.JSON(http.StatusOK, gin.H{"packages": packagesOut, "total": len(packagesOut)})
}

// packagesNullableDecimalText mirrors serialising a nullable Prisma Decimal.
func packagesNullableDecimalText(value *models.Decimal) any {
	if value == nil {
		return nil
	}
	return packagesDecimalText(*value)
}

// packagesInt32Slice returns a non-nil slice (JSON [] not null).
func packagesInt32Slice(values []int32) []int32 {
	if values == nil {
		return []int32{}
	}
	return values
}

// packagesHostIDsByPackage mirrors the packageHosts relation projection, keeping
// binding insertion order (Prisma issues no ORDER BY; the parity database's heap
// order is insertion order for these rows).
func packagesHostIDsByPackage(ctx context.Context, packageIDs []int32) (map[int32][]int32, error) {
	result := make(map[int32][]int32, len(packageIDs))
	if len(packageIDs) == 0 {
		return result, nil
	}

	var rows []struct {
		PackageID int32 `gorm:"column:package_id"`
		HostID    int32 `gorm:"column:host_id"`
	}
	if err := db.DB.WithContext(ctx).Raw(`SELECT package_id, host_id FROM package_hosts
		WHERE package_id IN (`+packagesPlaceholders(len(packageIDs))+`)`,
		packagesArgs(packageIDs)...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.PackageID] = append(result[row.PackageID], row.HostID)
	}
	return result, nil
}

// ==================== GET /public/regions ====================

// packagesPublicRegions mirrors GET /public/regions.
func packagesPublicRegions(c *gin.Context) {
	ctx := c.Request.Context()
	source := c.DefaultQuery("source", "all")

	query := db.DB.WithContext(ctx).Model(&models.Package{}).
		Where("packages.active = ? AND packages.global_shared = ?", true, true)
	if source == "official" {
		query = query.Joins("JOIN users AS owner ON owner.id = packages.user_id AND owner.role::text = ?", "admin")
	}

	var packages []models.Package
	if err := query.Select("packages.id").Find(&packages).Error; err != nil {
		packagesRespondInternal(c)
		return
	}

	packageIDs := make([]int32, 0, len(packages))
	for _, pkg := range packages {
		packageIDs = append(packageIDs, pkg.ID)
	}
	hostIDsByPackage, err := packagesHostIDsByPackage(ctx, packageIDs)
	if err != nil {
		packagesRespondInternal(c)
		return
	}

	hostSet := make(map[int32]bool)
	for _, hostIDs := range hostIDsByPackage {
		for _, hostID := range hostIDs {
			hostSet[hostID] = true
		}
	}
	hostIDs := make([]int32, 0, len(hostSet))
	for hostID := range hostSet {
		hostIDs = append(hostIDs, hostID)
	}

	var hosts []struct {
		ID          int32   `gorm:"column:id"`
		CountryCode string  `gorm:"column:country_code"`
		Location    *string `gorm:"column:location"`
	}
	if len(hostIDs) > 0 {
		if err := db.DB.WithContext(ctx).Raw(`SELECT id, country_code, location FROM hosts
			WHERE id IN (`+packagesPlaceholders(len(hostIDs))+`) AND status = ?`,
			append(packagesArgs(hostIDs), "online")...).Scan(&hosts).Error; err != nil {
			packagesRespondInternal(c)
			return
		}
	}
	hostByID := make(map[int32]int, len(hosts))
	for index, host := range hosts {
		hostByID[host.ID] = index
	}

	type region struct {
		name       string
		packageIDs []int32
		hostIDs    []int32
		seen       map[int32]bool
		hostSeen   map[int32]bool
	}
	regions := make(map[string]*region)
	order := make([]string, 0, 4)

	for _, pkg := range packages {
		for _, hostID := range hostIDsByPackage[pkg.ID] {
			index, ok := hostByID[hostID]
			if !ok {
				continue
			}
			host := hosts[index]
			code := strings.ToUpper(host.CountryCode)
			if code == "" {
				code = "UNKNOWN"
			}
			if _, exists := regions[code]; !exists {
				name := code
				if host.Location != nil && *host.Location != "" {
					name = *host.Location
				}
				regions[code] = &region{name: name, seen: map[int32]bool{}, hostSeen: map[int32]bool{}}
				order = append(order, code)
			}
			current := regions[code]
			if !current.seen[pkg.ID] {
				current.seen[pkg.ID] = true
				current.packageIDs = append(current.packageIDs, pkg.ID)
			}
			if !current.hostSeen[hostID] {
				current.hostSeen[hostID] = true
				current.hostIDs = append(current.hostIDs, hostID)
			}
		}
	}

	type regionView struct {
		code       string
		name       string
		packageIDs []int32
		hostCount  int
	}
	views := make([]regionView, 0, len(order))
	for _, code := range order {
		item := regions[code]
		views = append(views, regionView{
			code:       code,
			name:       item.name,
			packageIDs: item.packageIDs,
			hostCount:  len(item.hostIDs),
		})
	}
	sort.SliceStable(views, func(left, right int) bool {
		return packagesLocaleCompareZH(views[left].name, views[right].name) < 0
	})

	out := make([]gin.H, 0, len(views))
	for _, view := range views {
		out = append(out, gin.H{
			"code":       view.code,
			"name":       view.name,
			"packageIds": packagesInt32Slice(view.packageIDs),
			"hostCount":  view.hostCount,
		})
	}
	c.JSON(http.StatusOK, gin.H{"regions": out})
}

// ==================== GET / ====================

// packagesList mirrors GET /.
func packagesList(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	activeOnly := c.Query("all") != "true"
	source := c.DefaultQuery("source", "all")
	scope := c.Query("scope")
	isAdmin := user.Role == "admin"

	if isAdmin && scope != "" {
		switch scope {
		case "mine":
			own, err := db.GetAllPackages(ctx, activeOnly, db.PackageListOptions{UserID: &user.ID})
			if err != nil {
				packagesRespondInternal(c)
				return
			}
			ids := packagesListIDs(own)
			soldOutMap, err := db.CheckPackagesSoldOut(ctx, ids)
			if err != nil {
				packagesRespondInternal(c)
				return
			}
			counts, err := db.GetNormalInstanceCountsByPackages(ctx, ids)
			if err != nil {
				packagesRespondInternal(c)
				return
			}
			out := make([]gin.H, 0, len(own))
			for _, row := range own {
				body := packagesRowsMap(row)
				body["sourceType"] = "own"
				body["isOwn"] = true
				body["isShared"] = false
				body["soldOut"] = soldOutMap[row.ID]
				body["instance_count"] = counts[row.ID]
				out = append(out, body)
			}
			c.JSON(http.StatusOK, gin.H{"packages": out, "total": len(out)})
			return

		case "official":
			official, err := db.GetAllPackages(ctx, activeOnly, db.PackageListOptions{OwnerRole: packagesStringPtr("admin")})
			if err != nil {
				packagesRespondInternal(c)
				return
			}
			ids := packagesListIDs(official)
			soldOutMap, err := db.CheckPackagesSoldOut(ctx, ids)
			if err != nil {
				packagesRespondInternal(c)
				return
			}
			counts, err := db.GetNormalInstanceCountsByPackages(ctx, ids)
			if err != nil {
				packagesRespondInternal(c)
				return
			}
			out := make([]gin.H, 0, len(official))
			for _, row := range official {
				body := packagesRowsMap(row)
				body["sourceType"] = "official"
				body["isOwn"] = row.UserID == user.ID
				body["isShared"] = false
				body["soldOut"] = soldOutMap[row.ID]
				body["instance_count"] = counts[row.ID]
				out = append(out, body)
			}
			c.JSON(http.StatusOK, gin.H{"packages": out, "total": len(out)})
			return
		}
	}

	if source == "official" {
		globalPackages, err := db.GetGlobalSharedPackages(ctx)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		ids := packagesSharedIDs(globalPackages)
		soldOutMap, err := db.CheckPackagesSoldOut(ctx, ids)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		out := make([]gin.H, 0, len(globalPackages))
		for _, pkg := range globalPackages {
			body, err := packagesFormatSharedPackage(ctx, pkg, true, "official", user.ID)
			if err != nil {
				packagesRespondInternal(c)
				return
			}
			body["soldOut"] = soldOutMap[pkg.ID]
			out = append(out, body)
		}
		c.JSON(http.StatusOK, gin.H{"packages": out, "total": len(out)})
		return
	}

	if source == "friends" {
		friendPackages, err := db.GetSharedToUser(ctx, user.ID)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		ids := packagesSharedIDs(friendPackages)
		soldOutMap, err := db.CheckPackagesSoldOut(ctx, ids)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		out := make([]gin.H, 0, len(friendPackages))
		for _, pkg := range friendPackages {
			body, err := packagesFormatSharedPackage(ctx, pkg, false, "friends", user.ID)
			if err != nil {
				packagesRespondInternal(c)
				return
			}
			body["soldOut"] = soldOutMap[pkg.ID]
			out = append(out, body)
		}
		c.JSON(http.StatusOK, gin.H{"packages": out, "total": len(out)})
		return
	}

	// source=all（默认）：自己的 > 好友分享 > 官方直营，按 id 去重
	options := db.PackageListOptions{}
	if !isAdmin {
		options.UserID = &user.ID
	}
	ownPackages, err := db.GetAllPackages(ctx, activeOnly, options)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	ownInstanceCounts, err := db.GetNormalInstanceCountsByPackages(ctx, packagesListIDs(ownPackages))
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	sharedPackages, err := db.GetSharedToUser(ctx, user.ID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	globalPackages, err := db.GetGlobalSharedPackages(ctx)
	if err != nil {
		packagesRespondInternal(c)
		return
	}

	type listEntry struct {
		id   int32
		body gin.H
	}
	entries := make([]listEntry, 0, len(ownPackages)+len(sharedPackages)+len(globalPackages))
	added := make(map[int32]bool, len(ownPackages)+len(sharedPackages)+len(globalPackages))

	for _, row := range ownPackages {
		added[row.ID] = true
		hasRequired := true
		if row.RequiredPackageID != nil {
			value, err := db.UserHasNormalInstanceForPackage(ctx, user.ID, *row.RequiredPackageID, nil)
			if err != nil {
				packagesRespondInternal(c)
				return
			}
			hasRequired = value
		}
		entries = append(entries, listEntry{id: row.ID, body: packagesOwnListEntry(row, row.UserID, hasRequired, ownInstanceCounts[row.ID])})
	}

	for _, pkg := range sharedPackages {
		if added[pkg.ID] {
			continue
		}
		added[pkg.ID] = true
		body, err := packagesFormatSharedPackage(ctx, pkg, false, "friends", user.ID)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		entries = append(entries, listEntry{id: pkg.ID, body: body})
	}

	for _, pkg := range globalPackages {
		if added[pkg.ID] {
			continue
		}
		added[pkg.ID] = true
		body, err := packagesFormatSharedPackage(ctx, pkg, true, "official", user.ID)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		entries = append(entries, listEntry{id: pkg.ID, body: body})
	}

	allIDs := make([]int32, 0, len(entries))
	for _, item := range entries {
		allIDs = append(allIDs, item.id)
	}
	soldOutMap, err := db.CheckPackagesSoldOut(ctx, allIDs)
	if err != nil {
		packagesRespondInternal(c)
		return
	}

	out := make([]gin.H, 0, len(entries))
	for _, item := range entries {
		item.body["soldOut"] = soldOutMap[item.id]
		out = append(out, item.body)
	}
	c.JSON(http.StatusOK, gin.H{"packages": out, "total": len(out)})
}

func packagesStringPtr(value string) *string { return &value }

func packagesListIDs(rows []db.PackageListRow) []int32 {
	ids := make([]int32, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func packagesSharedIDs(rows []db.SharedPackageInfo) []int32 {
	ids := make([]int32, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

// packagesOwnListEntry mirrors the explicit object literal used for the user's
// own packages in the source=all branch (NOT the spread used by scope=mine).
func packagesOwnListEntry(row db.PackageListRow, userID int32, hasRequired bool, instanceCount int64) gin.H {
	return gin.H{
		"id":                            row.ID,
		"name":                          row.Name,
		"description":                   row.Description,
		"cpu_max":                       row.CPUMax,
		"memory_max":                    row.MemoryMax,
		"disk_max":                      row.DiskMax,
		"bandwidth_max":                 row.BandwidthMax,
		"network_mode":                  row.NetworkMode,
		"instance_type":                 row.InstanceType,
		"host_ids":                      packagesInt32Slice(row.HostIDs),
		"host_storage_pools":            packagesStoragePoolMap(row.HostStoragePools),
		"host_traffic_multipliers":      packagesMultiplierMap(row.HostTrafficMultipliers),
		"privileged":                    row.Privileged,
		"nested":                        row.Nested,
		"active":                        row.Active,
		"monthly_traffic_limit":         row.MonthlyTrafficLimit,
		"port_limit":                    row.PortLimit,
		"snapshot_limit":                row.SnapshotLimit,
		"backup_limit":                  row.BackupLimit,
		"site_limit":                    row.SiteLimit,
		"global_shared":                 row.GlobalShared,
		"io_limit_mode":                 row.IOLimitMode,
		"limits_read":                   row.LimitsRead,
		"limits_write":                  row.LimitsWrite,
		"limits_read_iops":              row.LimitsReadIops,
		"limits_write_iops":             row.LimitsWriteIops,
		"limits_ingress":                row.LimitsIngress,
		"limits_egress":                 row.LimitsEgress,
		"limits_processes":              row.LimitsProcesses,
		"limits_cpu_priority":           row.LimitsCPUPriority,
		"boot_autostart":                row.BootAutostart,
		"boot_autostart_priority":       row.BootAutostartPriority,
		"boot_autostart_delay":          row.BootAutostartDelay,
		"boot_host_shutdown_timeout":    row.BootHostShutdownTimeout,
		"allow_instance_deletion":       row.AllowInstanceDeletion,
		"required_package_id":           row.RequiredPackageID,
		"required_package_name":         row.RequiredPackageName,
		"has_required_package_instance": hasRequired,
		"ownerId":                       userID,
		"ownerUsername":                 row.OwnerUsername,
		"isOwn":                         true,
		"isShared":                      false,
		"sourceType":                    "own",
		"instance_count":                instanceCount,
	}
}

func packagesStoragePoolMap(value map[string]*string) map[string]*string {
	if value == nil {
		return map[string]*string{}
	}
	return value
}

func packagesMultiplierMap(value map[string]float64) map[string]float64 {
	if value == nil {
		return map[string]float64{}
	}
	return value
}

// packagesFormatSharedPackage mirrors formatSharedPackage. The io/boot/network
// fields fall back to the shared-package defaults because SharedPackageInfo does
// not carry them (same as the TypeScript interface).
func packagesFormatSharedPackage(ctx context.Context, pkg db.SharedPackageInfo, isGlobalShared bool, sourceType string, userID int32) (gin.H, error) {
	hasRequired := true
	if pkg.RequiredPackageID != nil {
		value, err := db.UserHasNormalInstanceForPackage(ctx, userID, *pkg.RequiredPackageID, nil)
		if err != nil {
			return nil, err
		}
		hasRequired = value
	}

	ioLimitMode := "throughput"
	limitsRead := "100MB"
	limitsWrite := "100MB"
	limitsReadIops := int32(500)
	limitsWriteIops := int32(500)

	return gin.H{
		"id":                            pkg.ID,
		"name":                          pkg.Name,
		"description":                   pkg.Description,
		"cpu_max":                       pkg.CPUMax,
		"memory_max":                    pkg.MemoryMax,
		"disk_max":                      pkg.DiskMax,
		"bandwidth_max":                 pkg.BandwidthMax,
		"network_mode":                  pkg.NetworkMode,
		"instance_type":                 pkg.InstanceType,
		"host_ids":                      packagesInt32Slice(pkg.HostIDs),
		"privileged":                    pkg.Privileged,
		"nested":                        pkg.Nested,
		"active":                        1,
		"monthly_traffic_limit":         pkg.MonthlyTrafficLimit,
		"port_limit":                    pkg.PortLimit,
		"snapshot_limit":                pkg.SnapshotLimit,
		"backup_limit":                  pkg.BackupLimit,
		"site_limit":                    pkg.SiteLimit,
		"io_limit_mode":                 ioLimitMode,
		"limits_read":                   limitsRead,
		"limits_write":                  limitsWrite,
		"limits_read_iops":              limitsReadIops,
		"limits_write_iops":             limitsWriteIops,
		"limits_ingress":                "300Mbit",
		"limits_egress":                 "300Mbit",
		"limits_processes":              500,
		"limits_cpu_priority":           10,
		"boot_autostart":                true,
		"boot_autostart_priority":       20,
		"boot_autostart_delay":          15,
		"boot_host_shutdown_timeout":    30,
		"allow_instance_deletion":       pkg.AllowInstanceDeletion,
		"gpu_shared":                    pkg.GPUShared,
		"egress_mode":                   "host",
		"egress_profile_id":             nil,
		"gpu_device_ids":                packagesInt32Slice(pkg.GPUDeviceIDs),
		"required_package_id":           pkg.RequiredPackageID,
		"required_package_name":         pkg.RequiredPackageName,
		"ownerId":                       pkg.OwnerID,
		"ownerUsername":                 pkg.OwnerUsername,
		"isOwn":                         false,
		"isShared":                      true,
		"isGlobalShared":                isGlobalShared,
		"sharedAt":                      packagesISOTime(pkg.SharedAt),
		"sourceType":                    sourceType,
		"has_required_package_instance": hasRequired,
	}, nil
}

// ==================== GET /:id ====================

// packagesDetail mirrors GET /:id. The access chain is canUserAccessPackage OR
// "I already own an instance of this package" OR admin; the quota object is only
// computed for users who may actually create instances.
func packagesDetail(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	packageID, ok := packagesRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}

	canAccess, err := db.CanUserAccessPackage(ctx, user.ID, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}

	hasInstanceWithPackage := false
	if !canAccess {
		instances, err := db.GetInstancesByUserId(ctx, user.ID)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		for _, instance := range instances {
			if instance.PackageID != nil && *instance.PackageID == packageID {
				hasInstanceWithPackage = true
				break
			}
		}
	}

	isAdmin := user.Role == "admin"
	if !canAccess && !hasInstanceWithPackage && !isAdmin {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	isOwn := pkg.UserID == user.ID

	quotaInfo := gin.H{
		"maxInstances":       nil,
		"remainingInstances": nil,
		"maxCpu":             nil,
		"remainingCpu":       nil,
		"maxMemory":          nil,
		"remainingMemory":    nil,
		"ownerUsername":      nil,
	}

	switch {
	case canAccess && !isOwn:
		shareInfo, err := db.GetPackageShareForUser(ctx, packageID, user.ID)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		if shareInfo != nil {
			usage, err := db.GetSharedPackageUsage(ctx, packageID, user.ID)
			if err != nil {
				packagesRespondInternal(c)
				return
			}
			if pkg.UserID != 0 {
				owner, err := db.FindUserById(ctx, pkg.UserID)
				if err != nil {
					packagesRespondInternal(c)
					return
				}
				if owner != nil {
					quotaInfo["ownerUsername"] = owner.Username
				}
			}
			if shareInfo.MaxInstances != nil {
				quotaInfo["maxInstances"] = *shareInfo.MaxInstances
				remaining := *shareInfo.MaxInstances - int32(usage.InstanceCount)
				if remaining < 0 {
					remaining = 0
				}
				quotaInfo["remainingInstances"] = remaining
			}
			if shareInfo.QuotaMultiplier != nil {
				maxCPU := int32(math.Floor(float64(pkg.CPUMax) * *shareInfo.QuotaMultiplier))
				remainingCPU := maxCPU - int32(usage.TotalCPU)
				if remainingCPU < 0 {
					remainingCPU = 0
				}
				maxMemory := int32(math.Floor(float64(pkg.MemoryMax) * *shareInfo.QuotaMultiplier))
				remainingMemory := maxMemory - int32(usage.TotalMemory)
				if remainingMemory < 0 {
					remainingMemory = 0
				}
				quotaInfo["maxCpu"] = maxCPU
				quotaInfo["remainingCpu"] = remainingCPU
				quotaInfo["maxMemory"] = maxMemory
				quotaInfo["remainingMemory"] = remainingMemory
			}
		}

	case canAccess && isOwn:
		quotaInfo["ownerUsername"] = user.Username

	case hasInstanceWithPackage:
		if pkg.UserID != 0 {
			owner, err := db.FindUserById(ctx, pkg.UserID)
			if err != nil {
				packagesRespondInternal(c)
				return
			}
			if owner != nil {
				quotaInfo["ownerUsername"] = owner.Username
			}
		}
	}

	hasRequired := true
	if pkg.RequiredPackageID != nil {
		value, err := db.UserHasNormalInstanceForPackage(ctx, user.ID, *pkg.RequiredPackageID, nil)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		hasRequired = value
	}

	egressMode := pkg.EgressMode
	if egressMode == "" {
		egressMode = "host"
	}
	ioLimitMode := pkg.IOLimitMode
	if ioLimitMode == "" {
		ioLimitMode = "throughput"
	}

	body := gin.H{
		"id":                            pkg.ID,
		"name":                          pkg.Name,
		"description":                   pkg.Description,
		"cpu_max":                       pkg.CPUMax,
		"memory_max":                    pkg.MemoryMax,
		"disk_max":                      pkg.DiskMax,
		"bandwidth_max":                 pkg.BandwidthMax,
		"network_mode":                  pkg.NetworkMode,
		"instance_type":                 pkg.InstanceType,
		"egress_mode":                   egressMode,
		"egress_profile_id":             pkg.EgressProfileID,
		"gpu_shared":                    pkg.GPUShared,
		"gpu_device_ids":                packagesInt32Slice(pkg.GPUDeviceIDs),
		"host_ids":                      packagesInt32Slice(pkg.HostIDs),
		"privileged":                    pkg.Privileged,
		"nested":                        pkg.Nested,
		"active":                        pkg.Active,
		"monthly_traffic_limit":         pkg.MonthlyTrafficLimit,
		"traffic_reset_price":           pkg.TrafficResetPrice,
		"port_limit":                    pkg.PortLimit,
		"snapshot_limit":                pkg.SnapshotLimit,
		"backup_limit":                  pkg.BackupLimit,
		"site_limit":                    pkg.SiteLimit,
		"io_limit_mode":                 ioLimitMode,
		"limits_read":                   pkg.LimitsRead,
		"limits_write":                  pkg.LimitsWrite,
		"limits_read_iops":              pkg.LimitsReadIops,
		"limits_write_iops":             pkg.LimitsWriteIops,
		"limits_ingress":                pkg.LimitsIngress,
		"limits_egress":                 pkg.LimitsEgress,
		"limits_processes":              pkg.LimitsProcesses,
		"limits_cpu_priority":           pkg.LimitsCPUPriority,
		"boot_autostart":                pkg.BootAutostart,
		"boot_autostart_priority":       pkg.BootAutostartPriority,
		"boot_autostart_delay":          pkg.BootAutostartDelay,
		"boot_host_shutdown_timeout":    pkg.BootHostShutdownTimeout,
		"allow_instance_deletion":       pkg.AllowInstanceDeletion,
		"required_package_id":           pkg.RequiredPackageID,
		"required_package_name":         pkg.RequiredPackageName,
		"has_required_package_instance": hasRequired,
		"ownerId":                       pkg.UserID,
		"isOwn":                         isOwn,
		"quotaInfo":                     quotaInfo,
	}
	if isOwn || isAdmin {
		body["host_storage_pools"] = packagesStoragePoolMap(pkg.HostStoragePools)
		body["host_traffic_multipliers"] = packagesMultiplierMap(pkg.HostTrafficMultipliers)
	}
	if !isOwn {
		body["privileged"] = int32(0)
		body["nested"] = int32(0)
	}
	if isOwn {
		body["global_shared"] = pkg.GlobalShared
		body["global_quota_multiplier"] = nil
		if pkg.GlobalShared {
			body["global_max_instances"] = packagesNormalizePublicMaxInstances(pkg.GlobalMaxInstances)
		} else {
			body["global_max_instances"] = nil
		}
	}

	c.JSON(http.StatusOK, gin.H{"package": body})
}

// ==================== POST / ====================

// packagesCreate mirrors POST /.
func packagesCreate(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, ok := packagesValidateBody(c, []string{"name", "cpuMax", "memoryMax", "diskMax"}, packagesPackageSchemaFields())
	if !ok {
		return
	}
	input, err := packagesNormalizeInput(body, true)
	if err != nil {
		packagesRespondNormalizeError(c, err)
		return
	}

	if input.HostIDs == nil || len(input.HostIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Package must bind at least one host", "code": "NO_HOSTS"})
		return
	}

	instanceType := "container"
	if input.InstanceType != nil {
		instanceType = *input.InstanceType
	}
	if message := packagesValidateNetworkModeForInstanceType(input.InstanceType, input.NetworkMode); message != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": message, "code": "KVM_NETWORK_MODE_UNSUPPORTED"})
		return
	}

	incompatible, err := packagesIncompatibleHosts(ctx, input.HostIDs, instanceType)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if len(incompatible) > 0 {
		c.JSON(http.StatusBadRequest, packagesIncompatibleHostsResponse(incompatible, instanceType))
		return
	}

	storageMissing, err := packagesHostsWithoutSystemDiskPool(ctx, input.HostIDs, nil, input.HostStoragePools)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if len(storageMissing) > 0 {
		c.JSON(http.StatusBadRequest, packagesStoragePoolUnavailableResponse(storageMissing))
		return
	}

	existing, err := db.GetPackageByUserAndName(ctx, user.ID, *input.Name)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if existing != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Package name already exists", "code": "NAME_EXISTS"})
		return
	}

	validation, err := db.ValidatePackagePrerequisiteReferenceLocked(ctx, nil, input.RequiredPackageID, &user.ID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if !validation.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": validation.Message, "code": validation.Code})
		return
	}
	if input.GlobalShared != nil && *input.GlobalShared && !packagesValidatePublicMaxInstances(input.GlobalMaxInstances) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "公开套餐最大实例数必须是 1-5 之间的整数",
			"code":  "INVALID_MAX_INSTANCES",
		})
		return
	}

	trafficLimit, trafficPresent, trafficValid := lib.ParseNullablePostgresBigIntInput(body["monthlyTrafficLimit"])
	if !trafficValid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid monthly traffic limit")
		return
	}

	data := db.PackageCreateData{
		UserID:                  user.ID,
		Name:                    *input.Name,
		Description:             input.Description,
		HostIDs:                 input.HostIDs,
		HostStoragePools:        input.HostStoragePools,
		HostTrafficMultipliers:  input.HostTrafficRaw,
		EgressMode:              input.EgressMode,
		EgressProfileID:         input.EgressProfileID,
		GPUShared:               input.GPUShared,
		GPUDeviceIDs:            input.GPUDeviceIDs,
		Privileged:              input.Privileged,
		Nested:                  input.Nested,
		Active:                  input.Active,
		PortLimit:               input.PortLimit,
		SnapshotLimit:           input.SnapshotLimit,
		BackupLimit:             input.BackupLimit,
		SiteLimit:               input.SiteLimit,
		TrafficResetPrice:       input.TrafficResetPrice,
		IOLimitMode:             input.IOLimitMode,
		LimitsRead:              input.LimitsRead,
		LimitsWrite:             input.LimitsWrite,
		LimitsReadIops:          input.LimitsReadIops,
		LimitsWriteIops:         input.LimitsWriteIops,
		LimitsIngress:           input.LimitsIngress,
		LimitsEgress:            input.LimitsEgress,
		LimitsProcesses:         input.LimitsProcesses,
		LimitsCPUPriority:       input.LimitsCPUPriority,
		BootAutostart:           input.BootAutostart,
		BootAutostartPriority:   input.BootAutostartPriority,
		BootAutostartDelay:      input.BootAutostartDelay,
		BootHostShutdownTimeout: input.BootHostShutdownTimeout,
		GlobalShared:            input.GlobalShared,
		RequiredPackageID:       input.RequiredPackageID,
		AllowInstanceDeletion:   packageBoolPtr(true),
	}
	if input.NetworkMode != nil {
		data.NetworkMode = input.NetworkMode
	}
	if input.InstanceType != nil {
		data.InstanceType = input.InstanceType
	}
	if input.CPUMax != nil {
		data.CPUMax = *input.CPUMax
	}
	if input.MemoryMax != nil {
		data.MemoryMax = *input.MemoryMax
	}
	if input.DiskMax != nil {
		data.DiskMax = *input.DiskMax
	}
	if input.BandwidthMax != nil {
		data.BandwidthMax = input.BandwidthMax
	}
	if trafficPresent {
		value := int64(trafficLimit)
		data.MonthlyTrafficLimit = &value
	}
	if input.GlobalShared != nil && *input.GlobalShared {
		data.GlobalMaxInstances = input.GlobalMaxInstances
	}

	id, err := db.CreatePackage(ctx, data, user.Role == "admin")
	if err != nil {
		packagesRespondCreateError(c, err)
		return
	}

	networkMode := "nat"
	if input.NetworkMode != nil {
		networkMode = *input.NetworkMode
	}
	db.CreateLog(ctx, packagesUserID64(user.ID), "package", "package.create",
		fmt.Sprintf("Created package %q [CPU: %d%%, Memory: %dMB, Disk: %dMB, instanceType: %s, network: %s, hosts: %s]",
			*input.Name, data.CPUMax, data.MemoryMax, data.DiskMax, instanceType, networkMode, packagesJoinIDs(input.HostIDs)),
		"success", db.CreateLogOptions{})

	c.JSON(http.StatusCreated, gin.H{
		"message": "Package created",
		"package": gin.H{"id": id, "name": *input.Name},
	})
}

func packageBoolPtr(value bool) *bool { return &value }

func packagesJoinIDs(ids []int32) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(int64(id), 10))
	}
	return strings.Join(parts, ",")
}

// packagesRespondCreateError mirrors the create/update db-error mapping:
// GpuError keeps its code, prerequisite failures keep "CODE: message", and
// everything else becomes INVALID_PACKAGE_CONFIG.
func packagesRespondCreateError(c *gin.Context, err error) {
	var gpuErr *lib.GpuError
	if errors.As(err, &gpuErr) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gpuErr.Message, "code": gpuErr.Code()})
		return
	}
	if code, detail, ok := packagesPrerequisiteErrorOf(err); ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": detail, "code": code})
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "INVALID_PACKAGE_CONFIG"})
}

// ==================== PATCH /:id ====================

// packagesUpdate mirrors PATCH /:id.
func packagesUpdate(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	packageID, ok := packagesRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}
	if packagesOwnerOrAdminDenies(pkg.UserID, user) {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	body, ok := packagesValidateBody(c, nil, packagesPatchSchemaFields())
	if !ok {
		return
	}
	input, err := packagesNormalizeInput(body, false)
	if err != nil {
		packagesRespondNormalizeError(c, err)
		return
	}

	if input.Name != nil && *input.Name != pkg.Name {
		existing, err := db.GetPackageByUserAndName(ctx, pkg.UserID, *input.Name)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		if existing != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Package name already exists", "code": "NAME_EXISTS"})
			return
		}
	}

	update := db.PackageUpdateData{}
	if input.EgressMode != nil {
		update.EgressMode = db.PackageValue(*input.EgressMode)
	}
	if input.EgressProfileSet {
		if input.EgressProfileID != nil {
			update.EgressProfileID = db.PackageValue(*input.EgressProfileID)
		} else {
			update.EgressProfileID = db.PackageNull[int32]()
		}
	}
	if input.Name != nil {
		update.Name = db.PackageValue(*input.Name)
	}
	if input.DescriptionSet {
		if input.Description != nil {
			update.Description = db.PackageValue(*input.Description)
		} else {
			update.Description = db.PackageNull[string]()
		}
	}
	if input.CPUMax != nil {
		update.CPUMax = db.PackageValue(*input.CPUMax)
	}
	if input.MemoryMax != nil {
		update.MemoryMax = db.PackageValue(*input.MemoryMax)
	}
	if input.DiskMax != nil {
		update.DiskMax = db.PackageValue(*input.DiskMax)
	}
	if input.BandwidthMax != nil {
		update.BandwidthMax = db.PackageValue(*input.BandwidthMax)
	}
	if input.NetworkMode != nil {
		update.NetworkMode = db.PackageValue(*input.NetworkMode)
	}
	if input.InstanceType != nil {
		update.InstanceType = db.PackageValue(*input.InstanceType)
	}
	if input.Privileged != nil {
		update.Privileged = db.PackageValue(*input.Privileged)
	}
	if input.Nested != nil {
		update.Nested = db.PackageValue(*input.Nested)
	}
	if input.Active != nil {
		update.Active = db.PackageValue(*input.Active)
	}
	update.AllowInstanceDeletion = db.PackageValue(true)

	nextInstanceType := pkg.InstanceType
	if input.InstanceType != nil {
		nextInstanceType = *input.InstanceType
	}
	if nextInstanceType == "" {
		nextInstanceType = "container"
	}
	nextNetworkMode := pkg.NetworkMode
	if input.NetworkMode != nil {
		nextNetworkMode = *input.NetworkMode
	}
	if nextNetworkMode == "" {
		nextNetworkMode = "nat"
	}
	if message := packagesValidateNetworkModeForInstanceType(&nextInstanceType, &nextNetworkMode); message != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": message, "code": "KVM_NETWORK_MODE_UNSUPPORTED"})
		return
	}

	if input.HostIDsSet {
		if len(input.HostIDs) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Package must bind at least one host", "code": "NO_HOSTS"})
			return
		}
		update.HostIDs = input.HostIDs
	}

	nextHostIDs := pkg.HostIDs
	if input.HostIDsSet {
		nextHostIDs = input.HostIDs
	}
	incompatible, err := packagesIncompatibleHosts(ctx, nextHostIDs, nextInstanceType)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if len(incompatible) > 0 {
		c.JSON(http.StatusBadRequest, packagesIncompatibleHostsResponse(incompatible, nextInstanceType))
		return
	}

	storageMissing, err := packagesHostsWithoutSystemDiskPool(ctx, nextHostIDs, &packageID, input.HostStoragePools)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if len(storageMissing) > 0 {
		c.JSON(http.StatusBadRequest, packagesStoragePoolUnavailableResponse(storageMissing))
		return
	}

	if input.HostStoragePools != nil {
		update.HostStoragePools = input.HostStoragePools
	}
	if input.HostTrafficRaw != nil {
		update.HostTrafficMultipliers = input.HostTrafficRaw
	}

	var parsedTrafficLimit *int64
	if input.MonthlyTrafficSet {
		value, present, valid := lib.ParseNullablePostgresBigIntInput(body["monthlyTrafficLimit"])
		if !valid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid monthly traffic limit")
			return
		}
		if present {
			parsed := int64(value)
			parsedTrafficLimit = &parsed
			update.MonthlyTrafficLimit = db.PackageValue(parsed)
		} else {
			update.MonthlyTrafficLimit = db.PackageNull[int64]()
		}
	}
	if input.TrafficResetPrice != nil {
		update.TrafficResetPrice = db.PackageValue(*input.TrafficResetPrice)
	}
	if input.PortLimit != nil {
		update.PortLimit = db.PackageValue(*input.PortLimit)
	}
	if input.SnapshotLimit != nil {
		update.SnapshotLimit = db.PackageValue(*input.SnapshotLimit)
	}
	if input.BackupLimit != nil {
		update.BackupLimit = db.PackageValue(*input.BackupLimit)
	}
	if input.SiteLimit != nil {
		update.SiteLimit = db.PackageValue(*input.SiteLimit)
	}
	if input.IOLimitMode != nil {
		update.IOLimitMode = db.PackageValue(*input.IOLimitMode)
	}
	if input.LimitsRead != nil {
		update.LimitsRead = db.PackageValue(*input.LimitsRead)
	}
	if input.LimitsWrite != nil {
		update.LimitsWrite = db.PackageValue(*input.LimitsWrite)
	}
	if input.LimitsReadIops != nil {
		update.LimitsReadIops = db.PackageValue(*input.LimitsReadIops)
	}
	if input.LimitsWriteIops != nil {
		update.LimitsWriteIops = db.PackageValue(*input.LimitsWriteIops)
	}
	if input.LimitsIngress != nil {
		update.LimitsIngress = db.PackageValue(*input.LimitsIngress)
	}
	if input.LimitsEgress != nil {
		update.LimitsEgress = db.PackageValue(*input.LimitsEgress)
	}
	if input.LimitsProcesses != nil {
		update.LimitsProcesses = db.PackageValue(*input.LimitsProcesses)
	}
	if input.LimitsCPUPriority != nil {
		update.LimitsCPUPriority = db.PackageValue(*input.LimitsCPUPriority)
	}
	if input.BootAutostart != nil {
		update.BootAutostart = db.PackageValue(*input.BootAutostart)
	}
	if input.BootAutostartPriority != nil {
		update.BootAutostartPriority = db.PackageValue(*input.BootAutostartPriority)
	}
	if input.BootAutostartDelay != nil {
		update.BootAutostartDelay = db.PackageValue(*input.BootAutostartDelay)
	}
	if input.BootHostShutdownTimeout != nil {
		update.BootHostShutdownTimeout = db.PackageValue(*input.BootHostShutdownTimeout)
	}

	if input.GlobalShared != nil {
		update.GlobalShared = db.PackageValue(*input.GlobalShared)
		if !*input.GlobalShared {
			update.GlobalQuotaMultiplier = db.PackageNull[float64]()
			update.GlobalMaxInstances = db.PackageNull[int32]()
		}
	}
	willBeGlobalShared := pkg.GlobalShared
	if input.GlobalShared != nil {
		willBeGlobalShared = *input.GlobalShared
	}
	if willBeGlobalShared {
		update.GlobalQuotaMultiplier = db.PackageNull[float64]()
		nextMaxInstances := pkg.GlobalMaxInstances
		if input.GlobalMaxSet {
			nextMaxInstances = input.GlobalMaxInstances
		}
		normalizedMax := packagesNormalizePublicMaxInstances(nextMaxInstances)
		if !packagesValidatePublicMaxInstances(&normalizedMax) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "公开套餐最大实例数必须是 1-5 之间的整数",
				"code":  "INVALID_MAX_INSTANCES",
			})
			return
		}
		update.GlobalMaxInstances = db.PackageValue(normalizedMax)
	} else if input.GlobalShared == nil &&
		(packagesHasKey(body, "globalQuotaMultiplier") || packagesHasKey(body, "globalMaxInstances")) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Cannot update globalQuotaMultiplier or globalMaxInstances when globalShared is false",
			"code":  "INVALID_GLOBAL_SHARED_STATE",
		})
		return
	}

	if input.RequiredPackageSet {
		validation, err := db.ValidatePackagePrerequisiteReferenceLocked(ctx, &packageID, input.RequiredPackageID, &pkg.UserID)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		if !validation.Valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": validation.Message, "code": validation.Code})
			return
		}
		if input.RequiredPackageID != nil {
			update.RequiredPackageID = db.PackageValue(*input.RequiredPackageID)
		} else {
			update.RequiredPackageID = db.PackageNull[int32]()
		}
	}

	shouldSync, err := packagesShouldSyncTrafficLimits(ctx, packageID, pkg,
		parsedTrafficLimit, input.MonthlyTrafficSet, input.HostIDs, input.HostIDsSet, input.HostTraffic, input.HostTrafficRaw != nil)
	if err != nil {
		packagesRespondInternal(c)
		return
	}

	shared, sharedPresent, deviceIDs, deviceIDsPresent, err := lib.NormalizeGpuInput(body, false)
	if err != nil {
		packagesRespondNormalizeError(c, err)
		return
	}
	if sharedPresent {
		if shared != nil {
			update.GPUShared = db.PackageValue(*shared)
		} else {
			update.GPUShared = db.PackageNull[bool]()
		}
	}
	if deviceIDsPresent {
		update.GPUDeviceIDs = db.PackageValue(deviceIDs)
	}

	if err := db.UpdatePackage(ctx, packageID, update, user.Role == "admin"); err != nil {
		packagesRespondCreateError(c, err)
		return
	}

	if shouldSync {
		syncResult, err := db.SyncPackageTrafficLimitsToInstances(ctx, &packageID)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		if len(syncResult.InstanceIDs) > 0 {
			if err := TrafficReconcileInstanceIDs(ctx, syncResult.InstanceIDs); err != nil {
				fmt.Fprintf(os.Stderr, "[Packages] 套餐流量限额已同步，流量状态即时复核失败: %v\n", err)
			}
		}
	}

	changes := make([]string, 0, 6)
	if input.Name != nil && *input.Name != pkg.Name {
		changes = append(changes, fmt.Sprintf("name -> %q", *input.Name))
	}
	if input.CPUMax != nil {
		changes = append(changes, fmt.Sprintf("cpuMax -> %d%%", *input.CPUMax))
	}
	if input.MemoryMax != nil {
		changes = append(changes, fmt.Sprintf("memoryMax -> %dMB", *input.MemoryMax))
	}
	if input.DiskMax != nil {
		changes = append(changes, fmt.Sprintf("diskMax -> %dMB", *input.DiskMax))
	}
	if input.InstanceType != nil {
		changes = append(changes, "instanceType -> "+*input.InstanceType)
	}
	if input.Active != nil {
		changes = append(changes, fmt.Sprintf("active -> %t", *input.Active))
	}
	if input.HostIDsSet {
		changes = append(changes, "hosts -> ["+packagesJoinIDs(input.HostIDs)+"]")
	}
	summary := "minor changes"
	if len(changes) > 0 {
		summary = strings.Join(changes, ", ")
	}

	db.CreateLog(ctx, packagesUserID64(user.ID), "package", "package.update",
		fmt.Sprintf("Updated package %q [%s]", pkg.Name, summary), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"message": "Package updated"})
}

// packagesShouldSyncTrafficLimits mirrors shouldSyncPackageTrafficLimitsAfterUpdate.
func packagesShouldSyncTrafficLimits(
	ctx context.Context,
	packageID int32,
	pkg *db.PackageDetailRow,
	nextLimit *int64,
	nextLimitSet bool,
	hostIDs []int32,
	hostIDsSet bool,
	nextMultipliers map[string]float64,
	multipliersSet bool,
) (bool, error) {
	if nextLimitSet {
		var current *int64
		if pkg.MonthlyTrafficLimit != nil {
			parsed, err := strconv.ParseInt(*pkg.MonthlyTrafficLimit, 10, 64)
			if err != nil {
				return false, err
			}
			current = &parsed
		}
		if (current == nil) != (nextLimit == nil) {
			return true, nil
		}
		if current != nil && nextLimit != nil && *current != *nextLimit {
			return true, nil
		}
	}

	if !hostIDsSet && !multipliersSet {
		return false, nil
	}

	var instanceHostIDs []int32
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT DISTINCT host_id FROM instances WHERE package_id = ? AND status <> 'deleted'`,
		packageID).Scan(&instanceHostIDs).Error; err != nil {
		return false, err
	}
	if len(instanceHostIDs) == 0 {
		return false, nil
	}

	currentHostIDs := make(map[int32]bool, len(pkg.HostIDs))
	for _, id := range pkg.HostIDs {
		currentHostIDs[id] = true
	}
	nextHostIDSet := currentHostIDs
	if hostIDsSet {
		nextHostIDSet = make(map[int32]bool, len(hostIDs))
		for _, id := range hostIDs {
			nextHostIDSet[id] = true
		}
	}
	currentMultipliers := pkg.HostTrafficMultipliers
	if currentMultipliers == nil {
		currentMultipliers = map[string]float64{}
	}
	if nextMultipliers == nil {
		nextMultipliers = map[string]float64{}
	}

	for _, hostID := range instanceHostIDs {
		key := strconv.FormatInt(int64(hostID), 10)
		_, explicit := nextMultipliers[key]
		if !explicit && (!hostIDsSet || !nextHostIDSet[hostID]) {
			continue
		}

		currentMultiplier := 1.0
		if currentHostIDs[hostID] {
			value := 1.0
			if raw, ok := currentMultipliers[key]; ok {
				value = raw
			}
			normalized, err := lib.NormalizeTrafficMultiplier(value)
			if err != nil {
				return false, err
			}
			currentMultiplier = normalized
		}
		nextMultiplier := currentMultiplier
		if explicit {
			normalized, err := lib.NormalizeTrafficMultiplier(nextMultipliers[key])
			if err != nil {
				return false, err
			}
			nextMultiplier = normalized
		}
		if currentMultiplier != nextMultiplier {
			return true, nil
		}
	}
	return false, nil
}

// ==================== DELETE /:id ====================

// packagesDelete mirrors DELETE /:id. "Package has instances" is decided by the
// ported db.GetInstanceCountByPackage, and the prerequisite check by
// db.GetPackagesDependingOnPackage; the route never issues those queries itself.
func packagesDelete(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	packageID, ok := packagesRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}
	if packagesOwnerOrAdminDenies(pkg.UserID, user) {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	dependents, err := db.GetPackagesDependingOnPackage(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if len(dependents) > 0 {
		names := make([]string, 0, len(dependents))
		for _, dependent := range dependents {
			names = append(names, dependent.Name)
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"error":   "Package is required by other packages: " + strings.Join(names, ", "),
			"code":    "PACKAGE_PREREQUISITE_IN_USE",
			"details": "请先删除后置套餐：" + strings.Join(names, "、"),
		})
		return
	}

	instanceCount, err := db.GetInstanceCountByPackage(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if instanceCount > 0 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPackageHasInstances, strconv.FormatInt(instanceCount, 10))
		return
	}

	if err := db.DeletePackage(ctx, packageID); err != nil {
		packagesRespondInternal(c)
		return
	}

	if err := packagesDecrementUserQuotaUsed(ctx, user.ID); err != nil {
		packagesRespondInternal(c)
		return
	}

	db.CreateLog(ctx, packagesUserID64(user.ID), "package", "package.delete",
		fmt.Sprintf("Deleted package %q [CPU: %d%%, Memory: %dMB]", pkg.Name, pkg.CPUMax, pkg.MemoryMax),
		"success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"message": "Package deleted"})
}

// packagesDecrementUserQuotaUsed mirrors decrementUserQuotaUsed(userId, 'package').
// SEAM: db/quota-operations.go is not ported yet; this is the exact single-column
// decrement Prisma performs (`data: { packageUsed: { decrement: 1 } }`), and it
// keeps Prisma's "missing row throws" behaviour so a 500 stays a 500.
func packagesDecrementUserQuotaUsed(ctx context.Context, userID int32) error {
	result := db.DB.WithContext(ctx).Exec(
		`UPDATE user_quotas SET package_used = package_used - 1 WHERE user_id = ?`, userID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("user_quotas: record not found")
	}
	return nil
}

// ==================== sharing routes ====================

// packagesShareView serialises a share the way Prisma + Date.toISOString() do.
func packagesShareView(share db.PackageShareInfo) gin.H {
	return gin.H{
		"id":                    share.ID,
		"packageId":             share.PackageID,
		"packageName":           share.PackageName,
		"ownerId":               share.OwnerID,
		"ownerUsername":         share.OwnerUsername,
		"sharedToId":            share.SharedToID,
		"sharedToUsername":      share.SharedToUsername,
		"sharedToAvatarStyle":   share.SharedToAvatarStyle,
		"sharedToAvatarBadgeId": share.SharedToAvatarBadgeID,
		"quotaMultiplier":       share.QuotaMultiplier,
		"maxInstances":          share.MaxInstances,
		"createdAt":             packagesISOTime(share.CreatedAt),
	}
}

// packagesShareCreate mirrors POST /:id/share.
func packagesShareCreate(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	decoded, ok := packagesDecodedBody(c)
	if !ok {
		return
	}
	// Validation of the body precedes the id check in the TypeScript.
	shareInput, err := packagesNormalizeShareInput(decoded, true)
	if err != nil {
		packagesRespondNormalizeError(c, err)
		return
	}

	packageID, ok := packagesRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}
	if packagesOwnerOnlyDenies(pkg.UserID, user) {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	areFriends, err := db.AreFriends(ctx, user.ID, *shareInput.FriendID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if !areFriends {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrNotFriends, "")
		return
	}

	existing, err := db.IsPackageSharedTo(ctx, packageID, *shareInput.FriendID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if existing {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPackageAlreadyShared, "")
		return
	}

	shareID, err := db.SharePackage(ctx, packageID, user.ID, *shareInput.FriendID, shareInput.QuotaMultiplier, shareInput.MaxInstances)
	if err != nil {
		packagesRespondInternal(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true, "shareId": shareID})
}

// packagesShareDelete mirrors DELETE /:id/share/:userId.
func packagesShareDelete(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	packageID, ok := packagesRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	targetUserID, ok := packagesRouteID(c.Param("userId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}
	if packagesOwnerOnlyDenies(pkg.UserID, user) {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	success, err := db.UnsharePackage(ctx, packageID, targetUserID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": success})
}

// packagesShareUpdate mirrors PATCH /:id/shares/:shareId.
func packagesShareUpdate(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	decoded, ok := packagesDecodedBody(c)
	if !ok {
		return
	}
	shareInput, err := packagesNormalizeShareInput(decoded, false)
	if err != nil {
		packagesRespondNormalizeError(c, err)
		return
	}

	packageID, ok := packagesRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	shareID, ok := packagesRouteID(c.Param("shareId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}
	if packagesOwnerOnlyDenies(pkg.UserID, user) {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	success, err := db.UpdateShareQuota(ctx, shareID, packageID, shareInput.QuotaMultiplier, shareInput.MaxInstances)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if !success {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrShareNotFound, "")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// packagesShareList mirrors GET /:id/shares.
func packagesShareList(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	packageID, ok := packagesRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}
	if packagesOwnerOnlyDenies(pkg.UserID, user) {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	shares, err := db.GetPackageShares(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	out := make([]gin.H, 0, len(shares))
	for _, share := range shares {
		out = append(out, packagesShareView(share))
	}
	c.JSON(http.StatusOK, gin.H{"shares": out})
}

// packagesMyShares mirrors GET /my-shares.
func packagesMyShares(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	shares, err := db.GetSharedByUser(ctx, user.ID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	out := make([]gin.H, 0, len(shares))
	for _, share := range shares {
		out = append(out, packagesShareView(share))
	}
	c.JSON(http.StatusOK, gin.H{"shares": out})
}

// ==================== GET /:id/hosts-detail ====================

// packagesHostsDetail mirrors GET /:id/hosts-detail.
func packagesHostsDetail(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	packageID, ok := packagesRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}
	if packagesOwnerOrAdminDenies(pkg.UserID, user) {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	hosts, err := db.GetPackageHostsDetail(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if hosts == nil {
		hosts = []db.PackageHostDetail{}
	}

	c.JSON(http.StatusOK, gin.H{
		"hosts":       hosts,
		"packageName": pkg.Name,
		"cpuMax":      pkg.CPUMax,
		"memoryMax":   pkg.MemoryMax,
	})
}

// ==================== POST /:id/release-quota ====================

// packagesReleaseQuota mirrors POST /:id/release-quota. The quota arithmetic is
// db.IncreaseHostQuota (the ported transactional function), never route SQL.
func packagesReleaseQuota(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, ok := packagesValidateBody(c, []string{"hostIds", "cpuAdd", "memoryAdd"}, packagesReleaseQuotaSchemaFields())
	if !ok {
		return
	}

	packageID, ok := packagesRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	rawHostIDs, _ := body["hostIds"].([]any)
	hostIDs := make([]int32, 0, len(rawHostIDs))
	for _, item := range rawHostIDs {
		if number, ok := item.(float64); ok {
			hostIDs = append(hostIDs, int32(number))
		}
	}
	cpuAdd, _ := body["cpuAdd"].(float64)
	memoryAdd, _ := body["memoryAdd"].(float64)
	notify, _ := body["notify"].(bool)
	cpuAddValue := int32(cpuAdd)
	memoryAddValue := int32(memoryAdd)

	if cpuAddValue <= 0 && memoryAddValue <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "At least one quota must be greater than 0"})
		return
	}

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}
	if packagesOwnerOrAdminDenies(pkg.UserID, user) {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	boundHosts := make(map[int32]bool, len(pkg.HostIDs))
	for _, hostID := range pkg.HostIDs {
		boundHosts[hostID] = true
	}
	invalidHosts := 0
	for _, hostID := range hostIDs {
		if !boundHosts[hostID] {
			invalidHosts++
		}
	}
	if invalidHosts > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Some hosts are not bound to this package"})
		return
	}

	results := make([]gin.H, 0, len(hostIDs))
	for _, hostID := range hostIDs {
		host, err := db.HostsGetByID(ctx, hostID)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		if host == nil {
			continue
		}

		updated, err := db.IncreaseHostQuota(ctx, hostID, cpuAddValue, memoryAddValue)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		countryCode := host.CountryCode
		if countryCode == "" {
			countryCode = "us"
		}
		results = append(results, gin.H{
			"hostId":          host.ID,
			"hostName":        host.Name,
			"countryCode":     countryCode,
			"cpuAllowanceMax": updated.CPUAllowanceMax,
			"memoryMax":       updated.MemoryMax,
			"cpuAvailable":    updated.CPUAllowanceMax - host.CPUUsed,
			"memoryAvailable": updated.MemoryMax - host.MemoryUsed,
		})
	}

	db.CreateLog(ctx, packagesUserID64(user.ID), "package", "package.release_quota",
		fmt.Sprintf("Released quota for package %q: CPU +%d%%, Memory +%dMB on %d host(s)",
			pkg.Name, cpuAddValue, memoryAddValue, len(hostIDs)),
		"success", db.CreateLogOptions{})

	if notify {
		var channel models.NotificationChannel
		err := db.DB.WithContext(ctx).Where("is_global = true").Take(&channel).Error
		if err == nil {
			hostLines := make([]string, 0, len(results))
			for _, result := range results {
				hostName := strings.ToUpper(fmt.Sprint(result["hostName"]))
				cpuAvailable := result["cpuAvailable"]
				memoryAvailable, _ := result["memoryAvailable"].(int32)
				hostLines = append(hostLines, fmt.Sprintf("• %s: CPU %v%% | 内存 %s",
					hostName, cpuAvailable, packagesFormatMemory(int64(memoryAvailable))))
			}

			var link *lib.NotificationLink
			if siteURL := os.Getenv("SITE_URL"); siteURL != "" {
				link = &lib.NotificationLink{
					Label: "🛒 立即开通",
					URL:   siteURL + "/instances/create?source=official&package=" + strconv.FormatInt(int64(packageID), 10),
				}
			}

			message := strings.Join([]string{
				fmt.Sprintf("+%d%% CPU / +%s 内存", cpuAddValue, packagesFormatMemory(int64(memoryAddValue))),
				"",
				"当前可用配额：",
				strings.Join(hostLines, "\n"),
				"",
				"⏰ " + packagesShanghaiTimestamp(),
			}, "\n")

			_ = lib.SendToChannel(ctx, int64(channel.ID), "📦 资源释放 - "+pkg.Name, message, link)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Quota released successfully",
		"results": results,
	})
}

// packagesShanghaiTimestamp mirrors
// new Date().toLocaleString('zh-CN', { timeZone: 'Asia/Shanghai' }).
func packagesShanghaiTimestamp() string {
	location := time.FixedZone("CST", 8*3600)
	return time.Now().In(location).Format("2006/1/2 15:04:05")
}

// ==================== GET /:id/plans ====================

// packagesPlansList mirrors GET /:id/plans.
func packagesPlansList(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	packageID, ok := packagesRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	activeOnly := c.Query("activeOnly") == "true"

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}
	if pkg.UserID != user.ID && user.Role != "admin" {
		canAccess, err := db.CanUserAccessPackage(ctx, user.ID, packageID)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		if !canAccess {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}
	}

	plans, err := db.GetPackagePlansByPackageId(ctx, packageID, db.PackagePlansOptions{ActiveOnly: activeOnly})
	if err != nil {
		packagesRespondInternal(c)
		return
	}

	pkgPolicy := lib.GpuPolicy{GpuShared: pkg.GPUShared, GpuDeviceIDs: pkg.GPUDeviceIDs}
	policies := make(map[int32]lib.GpuPolicy, len(plans))
	gpuIDs := make([]int32, 0, 4)
	gpuSeen := map[int32]bool{}
	for _, plan := range plans {
		policy := lib.ResolveGpuPolicy(pkgPolicy, &lib.GpuPlanPolicy{GpuShared: plan.GPUShared, GpuDeviceIDs: plan.GPUDeviceIds})
		policies[plan.ID] = policy
		if !policy.GpuShared {
			continue
		}
		for _, gpuID := range policy.GpuDeviceIDs {
			if gpuSeen[gpuID] {
				continue
			}
			gpuSeen[gpuID] = true
			gpuIDs = append(gpuIDs, gpuID)
		}
	}

	gpuDevices := make([]models.HostGpu, 0, len(gpuIDs))
	if len(gpuIDs) > 0 {
		if err := db.DB.WithContext(ctx).Where("id IN ?", gpuIDs).Order("id ASC").Find(&gpuDevices).Error; err != nil {
			packagesRespondInternal(c)
			return
		}
	}

	availability := map[int32]bool{}
	if activeOnly {
		for _, plan := range plans {
			available, err := db.GpusPlanAvailable(ctx, packageID, plan.ID)
			if err != nil {
				packagesRespondInternal(c)
				return
			}
			availability[plan.ID] = available
		}
	}

	swapSizeFor := func(plan models.PackagePlan) int32 {
		if pkg.InstanceType == "vm" {
			return 0
		}
		return plan.SwapSize
	}

	out := make([]gin.H, 0, len(plans))
	for _, plan := range plans {
		policy := policies[plan.ID]
		configDevices := make([]lib.GpuHostDevice, 0, len(gpuDevices))
		for _, gpu := range gpuDevices {
			if !policy.GpuShared || !packagesContainsInt32(policy.GpuDeviceIDs, gpu.ID) {
				continue
			}
			configDevices = append(configDevices, lib.GpuHostDevice{
				HostID:    int64(gpu.HostID),
				GpuDevice: lib.GpuDevice{Name: gpu.Name, MemoryTotalMiB: int64(gpu.MemoryTotalMiB)},
			})
		}

		isSoldOut := plan.IsSoldOut
		if available, known := availability[plan.ID]; known && !available {
			isSoldOut = true
		}
		out = append(out, gin.H{
			"id":                plan.ID,
			"name":              plan.Name,
			"description":       plan.Description,
			"gpuShared":         plan.GPUShared,
			"gpuDeviceIds":      packagesInt32Slice(plan.GPUDeviceIds),
			"gpuConfigurations": lib.SummarizeGpuConfigurations(configDevices),
			"cpu":               plan.CPU,
			"memory":            plan.Memory,
			"disk":              plan.Disk,
			"portLimit":         plan.PortLimit,
			"snapshotLimit":     plan.SnapshotLimit,
			"backupLimit":       plan.BackupLimit,
			"siteLimit":         plan.SiteLimit,
			"swapSize":          swapSizeFor(plan),
			"trafficLimit":      strconv.FormatInt(plan.TrafficLimit, 10),
			"trafficLimitSpeed": plan.TrafficLimitSpeed,
			"price":             packagesDecimalNumber(plan.Price),
			"trafficResetPrice": packagesNullableDecimalNumber(plan.TrafficResetPrice),
			"billingCycle":      plan.BillingCycle,
			"setupFee":          packagesDecimalNumber(plan.SetupFee),
			"monthlyPrice":      db.PackagePlanCalculateMonthlyPrice(plan),
			"isActive":          plan.IsActive,
			"isSoldOut":         isSoldOut,
			"sortOrder":         plan.SortOrder,
			"slaGuarantee":      packagesNullableDecimalNumber(plan.SlaGuarantee),
		})
	}

	c.JSON(http.StatusOK, gin.H{"plans": out})
}

func packagesContainsInt32(values []int32, candidate int32) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func packagesNullableDecimalNumber(value *models.Decimal) any {
	if value == nil {
		return nil
	}
	return packagesDecimalNumber(*value)
}

// ==================== POST /:id/plans ====================

// packagesPlansCreate mirrors POST /:id/plans.
func packagesPlansCreate(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	packageID, ok := packagesRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}
	if packagesOwnerOrAdminDenies(pkg.UserID, user) {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	decoded, ok := packagesDecodedBody(c)
	if !ok {
		return
	}
	planInput, err := packagesNormalizePlanInput(decoded, true, pkg.InstanceType == "vm")
	if err != nil {
		packagesRespondNormalizeError(c, err)
		return
	}
	planName := *planInput.Name

	unique, err := db.IsPackagePlanNameUnique(ctx, packageID, planName, nil)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if !unique {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该套餐下已存在同名方案"})
		return
	}

	parsedTrafficLimit, valid := lib.ParseRequiredPostgresBigIntInput(planInput.TrafficLimit)
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid plan traffic limit")
		return
	}
	var trafficLimitSpeed *string
	if planInput.TrafficSpeedSet {
		normalized, valid := packagesNormalizePlanTrafficLimitSpeed(planInput.TrafficLimitSpeed)
		if !valid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid plan traffic limit speed")
			return
		}
		value := packagesTrafficLimitSpeedValue(normalized)
		trafficLimitSpeed = &value
	}

	plan, err := db.CreatePackagePlan(ctx, db.CreatePlanInput{
		GPUShared:         planInput.GPUShared,
		GPUDeviceIDs:      planInput.GPUDeviceIDs,
		PackageID:         packageID,
		Name:              planName,
		Description:       planInput.Description,
		CPU:               *planInput.CPU,
		Memory:            *planInput.Memory,
		Disk:              *planInput.Disk,
		PortLimit:         *planInput.PortLimit,
		SnapshotLimit:     *planInput.SnapshotLimit,
		BackupLimit:       *planInput.BackupLimit,
		SiteLimit:         *planInput.SiteLimit,
		SwapSize:          *planInput.SwapSize,
		TrafficLimit:      int64(parsedTrafficLimit),
		TrafficLimitSpeed: trafficLimitSpeed,
		Price:             float64(*planInput.Price),
		TrafficResetPrice: planInput.TrafficResetPrice,
		BillingCycle:      planInput.BillingCycle,
		SetupFee:          packagesFloatPtr(0),
		IsActive:          planInput.IsActive,
		IsSoldOut:         planInput.IsSoldOut,
		SortOrder:         planInput.SortOrder,
		SlaGuarantee:      planInput.SlaGuarantee,
	})
	if err != nil {
		packagesRespondPlanError(c, err, "创建方案失败")
		return
	}

	db.CreateLog(ctx, packagesUserID64(user.ID), "package", "plan.create",
		fmt.Sprintf("Created plan %q for package %q", planName, pkg.Name), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{
		"id":      plan.ID,
		"name":    plan.Name,
		"message": "方案创建成功",
	})
}

func packagesFloatPtr(value float64) *float64 { return &value }

// packagesRespondPlanError mirrors the plan handlers' catch blocks: GpuError is a
// 400 with its code, anything else is a 500 with the route-specific message.
func packagesRespondPlanError(c *gin.Context, err error, message string) {
	var gpuErr *lib.GpuError
	if errors.As(err, &gpuErr) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gpuErr.Message, "code": gpuErr.Code()})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": message})
}

// ==================== PUT /:id/plans/:planId ====================

// packagesPlansUpdate mirrors PUT /:id/plans/:planId.
func packagesPlansUpdate(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	packageID, ok := packagesRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	planID, ok := packagesRouteID(c.Param("planId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}
	if packagesOwnerOrAdminDenies(pkg.UserID, user) {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	existingPlan, err := db.GetPackagePlanById(ctx, planID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if existingPlan == nil || existingPlan.PackageID != packageID {
		c.JSON(http.StatusNotFound, gin.H{"error": "方案不存在"})
		return
	}

	decoded, ok := packagesDecodedBody(c)
	if !ok {
		return
	}
	planInput, err := packagesNormalizePlanInput(decoded, false, pkg.InstanceType == "vm")
	if err != nil {
		packagesRespondNormalizeError(c, err)
		return
	}

	if planInput.Name != nil && *planInput.Name != existingPlan.Name {
		unique, err := db.IsPackagePlanNameUnique(ctx, packageID, *planInput.Name, &planID)
		if err != nil {
			packagesRespondInternal(c)
			return
		}
		if !unique {
			c.JSON(http.StatusBadRequest, gin.H{"error": "该套餐下已存在同名方案"})
			return
		}
	}

	update := db.UpdatePlanInput{}
	if planInput.Name != nil {
		update.Name = planInput.Name
	}
	if planInput.DescriptionSet {
		update.Description = db.PackagePlanValue(planInput.Description)
	}
	if planInput.CPU != nil {
		update.CPU = planInput.CPU
	}
	if planInput.Memory != nil {
		update.Memory = planInput.Memory
	}
	if planInput.Disk != nil {
		update.Disk = planInput.Disk
	}
	if planInput.PortLimit != nil {
		update.PortLimit = planInput.PortLimit
	}
	if planInput.SnapshotLimit != nil {
		update.SnapshotLimit = planInput.SnapshotLimit
	}
	if planInput.BackupLimit != nil {
		update.BackupLimit = planInput.BackupLimit
	}
	if planInput.SiteLimit != nil {
		update.SiteLimit = planInput.SiteLimit
	}
	if pkg.InstanceType == "vm" {
		zero := int32(0)
		update.SwapSize = &zero
	} else if planInput.SwapSize != nil {
		update.SwapSize = planInput.SwapSize
	}
	if planInput.TrafficLimitSet {
		parsed, valid := lib.ParseRequiredPostgresBigIntInput(planInput.TrafficLimit)
		if !valid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid plan traffic limit")
			return
		}
		value := int64(parsed)
		update.TrafficLimit = &value
	}
	if planInput.TrafficSpeedSet {
		normalized, valid := packagesNormalizePlanTrafficLimitSpeed(planInput.TrafficLimitSpeed)
		if !valid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid plan traffic limit speed")
			return
		}
		value := packagesTrafficLimitSpeedValue(normalized)
		update.TrafficLimitSpeed = &value
	}
	if planInput.Price != nil {
		price := float64(*planInput.Price)
		update.Price = &price
	}
	if planInput.TrafficResetSet {
		update.TrafficResetPrice = db.PackagePlanValue(planInput.TrafficResetPrice)
	}
	if planInput.BillingCycle != nil {
		update.BillingCycle = planInput.BillingCycle
	}
	if planInput.IsActive != nil {
		update.IsActive = planInput.IsActive
	}
	if planInput.IsSoldOut != nil {
		update.IsSoldOut = planInput.IsSoldOut
	}
	if planInput.SortOrder != nil {
		update.SortOrder = planInput.SortOrder
	}
	if planInput.SlaGuaranteeSet {
		update.SlaGuarantee = db.PackagePlanValue(planInput.SlaGuarantee)
	}

	shared, sharedPresent, deviceIDs, deviceIDsPresent, err := lib.NormalizeGpuInput(decoded.(map[string]any), true)
	if err != nil {
		packagesRespondNormalizeError(c, err)
		return
	}
	if sharedPresent {
		update.GPUShared = db.PackagePlanValue(shared)
	}
	if deviceIDsPresent {
		update.GPUDeviceIDs = deviceIDs
		update.GPUDeviceIDsSet = true
	}

	updatedPlan, err := db.UpdatePackagePlan(ctx, planID, update)
	if err != nil {
		packagesRespondPlanError(c, err, "更新方案失败")
		return
	}

	hasQuotaChange := update.PortLimit != nil || update.SnapshotLimit != nil ||
		update.BackupLimit != nil || update.SiteLimit != nil ||
		update.SwapSize != nil || update.TrafficLimit != nil
	if hasQuotaChange {
		previousSwap := existingPlan.SwapSize
		syncResult, err := db.SyncPackagePlanQuotaToInstances(ctx, planID, *updatedPlan, &previousSwap)
		if err != nil {
			packagesRespondPlanError(c, err, "更新方案失败")
			return
		}
		if syncResult.Count > 0 {
			if len(syncResult.InstanceIDs) > 0 {
				if err := TrafficReconcileInstanceIDs(ctx, syncResult.InstanceIDs); err != nil {
					fmt.Fprintf(os.Stderr, "[Packages] 方案配额已同步，流量状态即时复核失败: %v\n", err)
				}
			}
			db.CreateLog(ctx, packagesUserID64(user.ID), "package", "plan.sync_quota",
				fmt.Sprintf("Synced quota to %d instance(s) for plan %q", syncResult.Count, updatedPlan.Name),
				"success", db.CreateLogOptions{})
		}
	}

	db.CreateLog(ctx, packagesUserID64(user.ID), "package", "plan.update",
		fmt.Sprintf("Updated plan %q for package %q", updatedPlan.Name, pkg.Name), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{
		"id":      updatedPlan.ID,
		"name":    updatedPlan.Name,
		"message": "方案更新成功",
	})
}

// ==================== DELETE /:id/plans/:planId ====================

// packagesPlansDelete mirrors DELETE /:id/plans/:planId.
func packagesPlansDelete(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	packageID, ok := packagesRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	planID, ok := packagesRouteID(c.Param("planId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if pkg == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrPackageNotFound, "")
		return
	}
	if packagesOwnerOrAdminDenies(pkg.UserID, user) {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	existingPlan, err := db.GetPackagePlanById(ctx, planID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if existingPlan == nil || existingPlan.PackageID != packageID {
		c.JSON(http.StatusNotFound, gin.H{"error": "方案不存在"})
		return
	}

	canDelete, err := db.CanDeletePlan(ctx, planID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if !canDelete.CanDelete {
		reason := "方案无法删除"
		if canDelete.Reason != nil {
			reason = *canDelete.Reason
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": reason})
		return
	}

	if err := db.DeletePackagePlan(ctx, planID); err != nil {
		packagesRespondPlanError(c, err, "删除方案失败")
		return
	}

	db.CreateLog(ctx, packagesUserID64(user.ID), "package", "plan.delete",
		fmt.Sprintf("Deleted plan %q from package %q", existingPlan.Name, pkg.Name), "success", db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"message": "方案删除成功"})
}

// ==================== GET /regions ====================

// packagesRegions mirrors GET /regions.
func packagesRegions(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)
	source := c.DefaultQuery("source", "official")

	if source == "friends" {
		packagesFriendRegions(c, ctx, user.ID)
		return
	}

	query := db.DB.WithContext(ctx).Model(&models.Package{}).
		Where("packages.active = ?", true).
		Where("EXISTS (SELECT 1 FROM package_plans pp WHERE pp.package_id = packages.id AND pp.is_active = true)")
	if source == "official" {
		query = query.Where("packages.global_shared = ?", true).
			Joins("JOIN users AS owner ON owner.id = packages.user_id AND owner.role::text = ?", "admin")
	}

	var packages []models.Package
	if err := query.Select("packages.id").Find(&packages).Error; err != nil {
		packagesRespondInternal(c)
		return
	}

	packageIDs := make([]int32, 0, len(packages))
	for _, pkg := range packages {
		packageIDs = append(packageIDs, pkg.ID)
	}
	if len(packageIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"regions": []gin.H{}})
		return
	}

	var bindings []struct {
		PackageID int32 `gorm:"column:package_id"`
		HostID    int32 `gorm:"column:host_id"`
	}
	if err := db.DB.WithContext(ctx).Raw(`SELECT package_id, host_id FROM package_hosts
		WHERE package_id IN (`+packagesPlaceholders(len(packageIDs))+`)`,
		packagesArgs(packageIDs)...).Scan(&bindings).Error; err != nil {
		packagesRespondInternal(c)
		return
	}

	hostSet := map[int32]bool{}
	hostIDs := make([]int32, 0, len(bindings))
	for _, binding := range bindings {
		if hostSet[binding.HostID] {
			continue
		}
		hostSet[binding.HostID] = true
		hostIDs = append(hostIDs, binding.HostID)
	}
	var hosts []struct {
		ID          int32   `gorm:"column:id"`
		CountryCode string  `gorm:"column:country_code"`
		Status      string  `gorm:"column:status"`
		Location    *string `gorm:"column:location"`
	}
	if len(hostIDs) > 0 {
		if err := db.DB.WithContext(ctx).Raw(`SELECT id, country_code, status::text AS status, location
			FROM hosts WHERE id IN (`+packagesPlaceholders(len(hostIDs))+`)`,
			packagesArgs(hostIDs)...).Scan(&hosts).Error; err != nil {
			packagesRespondInternal(c)
			return
		}
	}
	online := make(map[int32]string, len(hosts))
	for _, host := range hosts {
		if host.Status != "online" {
			continue
		}
		country := strings.ToLower(host.CountryCode)
		if country == "" {
			country = "us"
		}
		online[host.ID] = country
	}

	regionPackages := make(map[string][]int32)
	regionSeen := make(map[string]map[int32]bool)
	order := make([]string, 0, 4)
	for _, binding := range bindings {
		country, isOnline := online[binding.HostID]
		if !isOnline {
			continue
		}
		if _, exists := regionPackages[country]; !exists {
			regionPackages[country] = []int32{}
			regionSeen[country] = map[int32]bool{}
			order = append(order, country)
		}
		if regionSeen[country][binding.PackageID] {
			continue
		}
		regionSeen[country][binding.PackageID] = true
		regionPackages[country] = append(regionPackages[country], binding.PackageID)
	}

	c.JSON(http.StatusOK, gin.H{"regions": packagesRegionViews(order, regionPackages)})
}

// packagesFriendRegions mirrors the source=friends branch of GET /regions.
func packagesFriendRegions(c *gin.Context, ctx context.Context, userID int32) {
	friendPackages, err := db.GetSharedToUser(ctx, userID)
	if err != nil {
		packagesRespondInternal(c)
		return
	}
	if len(friendPackages) == 0 {
		c.JSON(http.StatusOK, gin.H{"regions": []gin.H{}})
		return
	}

	hostSet := map[int32]bool{}
	hostIDs := make([]int32, 0, len(friendPackages))
	for _, pkg := range friendPackages {
		for _, hostID := range pkg.HostIDs {
			if hostSet[hostID] {
				continue
			}
			hostSet[hostID] = true
			hostIDs = append(hostIDs, hostID)
		}
	}

	var hosts []struct {
		ID          int32  `gorm:"column:id"`
		CountryCode string `gorm:"column:country_code"`
	}
	if len(hostIDs) > 0 {
		if err := db.DB.WithContext(ctx).Raw(`SELECT id, country_code FROM hosts
			WHERE id IN (`+packagesPlaceholders(len(hostIDs))+`) AND status = ?`,
			append(packagesArgs(hostIDs), "online")...).Scan(&hosts).Error; err != nil {
			packagesRespondInternal(c)
			return
		}
	}

	friendPackageIDs := make(map[int32]bool, len(friendPackages))
	for _, pkg := range friendPackages {
		friendPackageIDs[pkg.ID] = true
	}

	regionPackages := make(map[string][]int32)
	regionSeen := make(map[string]map[int32]bool)
	order := make([]string, 0, 4)
	for _, host := range hosts {
		country := strings.ToLower(host.CountryCode)
		if country == "" {
			country = "us"
		}
		var bindings []struct {
			PackageID int32 `gorm:"column:package_id"`
		}
		if err := db.DB.WithContext(ctx).Raw(`SELECT package_id FROM package_hosts WHERE host_id = ?`,
			host.ID).Scan(&bindings).Error; err != nil {
			packagesRespondInternal(c)
			return
		}
		for _, binding := range bindings {
			if !friendPackageIDs[binding.PackageID] {
				continue
			}
			if _, exists := regionPackages[country]; !exists {
				regionPackages[country] = []int32{}
				regionSeen[country] = map[int32]bool{}
				order = append(order, country)
			}
			if regionSeen[country][binding.PackageID] {
				continue
			}
			regionSeen[country][binding.PackageID] = true
			regionPackages[country] = append(regionPackages[country], binding.PackageID)
		}
	}

	c.JSON(http.StatusOK, gin.H{"regions": packagesRegionViews(order, regionPackages)})
}

// packagesRegionViews builds and sorts the region payload: packageCount DESC,
// stable so equal counts keep their insertion order like Array.prototype.sort.
func packagesRegionViews(order []string, regionPackages map[string][]int32) []gin.H {
	type regionView struct {
		code       string
		name       string
		packageIDs []int32
		count      int
	}
	views := make([]regionView, 0, len(order))
	for _, code := range order {
		packageIDs := regionPackages[code]
		views = append(views, regionView{
			code:       code,
			name:       packagesCountryName(code),
			packageIDs: packageIDs,
			count:      len(packageIDs),
		})
	}
	sort.SliceStable(views, func(left, right int) bool {
		return views[left].count > views[right].count
	})

	out := make([]gin.H, 0, len(views))
	for _, view := range views {
		out = append(out, gin.H{
			"code":         view.code,
			"name":         view.name,
			"packageCount": view.count,
			"packageIds":   packagesInt32Slice(view.packageIDs),
		})
	}
	return out
}
