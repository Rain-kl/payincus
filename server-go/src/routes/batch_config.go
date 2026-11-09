// Port of server/src/routes/batch-config.ts
// (POST /api/hosts/:id/instances/batch-config, the prefix declared in app.ts;
// there is no prefixTrailingSlash and no plugin-root route, so nothing is mounted
// twice).
//
// This is a BULK MUTATION over instances. Two pieces must stay exactly as the
// TypeScript has them:
//
//  1. Per-instance sandbox + concurrency. `pLimit(BATCH_CONFIG_CONCURRENCY)` with
//     BATCH_CONFIG_CONCURRENCY = 10 is reproduced by a bounded worker pool with
//     the same limit. A failure is NOT rolled back: updateSingleInstance catches
//     its own error and returns `{success:false, error}` for that instance, the
//     other workers keep running, and the response reports per-item results plus a
//     retryPayload of the failed instance ids. The only global side effect applied
//     after all workers finish is the accumulated host resource delta (a single
//     atomic increments UPDATE).
//  2. Retry. A failed item whose error message contains one of RETRYABLE_ERRORS
//     (case-insensitive substring) is attempted exactly ONCE more; the retried
//     result replaces the original in place, and resource deltas are counted for
//     the successful attempt only (the first attempt's failure never added a
//     delta). Total possible attempts per instance: two.
//
// Faithfully preserved Node quirks:
//
//   - `Object.keys(config).length === 0` → 400 CONFIG_NO_CHANGES, but a config
//     consisting only of UNKNOWN keys is non-empty and proceeds (AJV keeps unknown
//     keys because the schema has no additionalProperties:false). Unknown keys are
//     then echoed back inside retryPayload.config.
//   - The host permission check is `user.role !== 'admin' && host.user_id !==
//     user.id` (authenticate, not authenticateUser).
//   - A SWAP-enabled instance whose effective swap size is 0 makes every batch
//     update for it fail with '启用 SWAP 前需要设置大于 0 的 SWAP 大小' unless a
//     positive swapSize is supplied, because the check runs on the *effective*
//     values (instance.swapEnabled ?? false).
//   - VM instances reject any SWAP field ('虚拟机不支持批量 SWAP 配置'), including
//     swapSize: null.
//   - `limitsRead` wins over `limitsReadIops` (and write likewise): providing both
//     writes the bandwidth string to Incus and the DB override keeps IOPS = NULL.
//   - The policy upsert of the SWAP DB row writes swapSize = effective size when
//     only `swapEnabled: true` is supplied, but NOT when swapEnabled is merely
//     inherited from the instance.
//   - getIncusClient failures are a hand-built 503 `{error:'无法连接到宿主机: …'}`
//     with no `code` key.
package routes

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// batchConfigConcurrency mirrors BATCH_CONFIG_CONCURRENCY ("每宿主机并发限制").
const batchConfigConcurrency = 10

// batchConfigRetryableErrors mirrors RETRYABLE_ERRORS.
var batchConfigRetryableErrors = []string{
	"ETIMEDOUT",
	"ECONNREFUSED",
	"ECONNRESET",
	"context deadline exceeded",
	"timeout",
}

// batchConfigInstanceIDMaxItems mirrors the schema's maxItems.
const batchConfigInstanceIDMaxItems = 1000

// ==================== AJV-coercion substrate for this module ====================
//
// The storage-configs replica (routes/storage_configs.go) covers string /
// integer / nullable-integer / boolean / object. batch-config additionally needs
// `number` (disk) and multi-type nullable fields, and its `instanceIds` schema is
// the only array in the port, so this module carries the extra AJV pieces it
// needs. They follow ajv@8.17.1's coerceData / checkDataType / limitItems /
// uniqueItems semantics exactly (verified against the installed ajv source).

// batchConfigOptional is the tri-state a JSON body produces, mirroring TypeScript's
// `undefined` vs `null` vs value:
//
//	Present == false          → the key was absent (undefined)
//	Present == true, Value==nil → the key was present with JSON null
type batchConfigOptional[T any] struct {
	Present bool
	Value   *T
}

// batchConfigMatchesType mirrors AJV's checkDataType.
func batchConfigMatchesType(value any, typeName string) bool {
	switch typeName {
	case "null":
		return value == nil
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "integer":
		number, ok := value.(float64)
		return ok && !math.IsNaN(number) && !math.IsInf(number, 0) && number == math.Trunc(number)
	case "number":
		number, ok := value.(float64)
		return ok && !math.IsNaN(number) && !math.IsInf(number, 0)
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	}
	return false
}

// batchConfigMatchesAnyType mirrors checkDataTypes(types, data) for the "correct"
// direction.
func batchConfigMatchesAnyType(value any, types []string) bool {
	for _, typeName := range types {
		if batchConfigMatchesType(value, typeName) {
			return true
		}
	}
	return false
}

// batchConfigCoerceToType applies AJV's coercion chain for one target type.
func batchConfigCoerceToType(value any, typeName string) (any, bool) {
	switch typeName {
	case "string":
		switch typed := value.(type) {
		case float64:
			return formatTerminalCommandNumber(typed), true
		case bool:
			return strconv.FormatBool(typed), true
		case nil:
			return "", true
		}
	case "number":
		switch typed := value.(type) {
		case bool:
			if typed {
				return float64(1), true
			}
			return float64(0), true
		case nil:
			return float64(0), true
		case string:
			if typed != "" {
				if number, ok := ajvToJSNumber(typed); ok {
					return number, true
				}
			}
		}
	case "integer":
		switch typed := value.(type) {
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
	case "boolean":
		switch typed := value.(type) {
		case string:
			if typed == "false" {
				return false, true
			}
			if typed == "true" {
				return true, true
			}
		case float64:
			if typed == 0 {
				return false, true
			}
			if typed == 1 {
				return true, true
			}
		case nil:
			return false, true
		}
	case "null":
		switch typed := value.(type) {
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
	case "array":
		switch value.(type) {
		case string, float64, bool, nil:
			return []any{value}, true
		}
	}
	return nil, false
}

// batchConfigCoerceValue reproduces AJV's coerceData + checkDataTypes for one
// property: match as-is, else unwrap a one-element array, else run the coercion
// chain in the schema's type order, else report the type error.
func batchConfigCoerceValue(value any, types []string) (any, string) {
	if batchConfigMatchesAnyType(value, types) {
		return value, ""
	}

	if array, ok := value.([]any); ok && len(array) == 1 {
		candidate := array[0]
		if batchConfigMatchesAnyType(candidate, types) {
			return candidate, ""
		}
		value = candidate
	}

	for _, typeName := range types {
		if coerced, ok := batchConfigCoerceToType(value, typeName); ok {
			return coerced, ""
		}
	}
	return nil, "must be " + strings.Join(types, ",")
}

// batchConfigBoundsMessage mirrors AJV's limitNumber failures. The number group's
// guard is `typeof data == "number" && isFinite(data)`, so non-finite values skip
// the bounds entirely (and later fail at the DB/Incus stage, exactly as in Node).
func batchConfigBoundsMessage(value any, minimum, maximum float64) string {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return ""
	}
	if !math.IsNaN(maximum) && number > maximum {
		return "must be <= " + formatTerminalCommandNumber(maximum)
	}
	if !math.IsNaN(minimum) && number < minimum {
		return "must be >= " + formatTerminalCommandNumber(minimum)
	}
	return ""
}

// batchConfigFieldSpec is one property of the config schema.
type batchConfigFieldSpec struct {
	Name    string
	Types   []string
	Minimum float64 // NaN when unset
	Maximum float64 // NaN when unset
}

// batchConfigConfigFieldSpecs mirrors the `config.properties` block in schema
// order (AJV validates properties in declaration order with allErrors:false).
var batchConfigConfigFieldSpecs = []batchConfigFieldSpec{
	{Name: "cpu", Types: []string{"integer"}, Minimum: 1, Maximum: math.NaN()},
	{Name: "memory", Types: []string{"integer"}, Minimum: 1, Maximum: math.NaN()},
	{Name: "disk", Types: []string{"number"}, Minimum: 1, Maximum: math.NaN()},
	{Name: "monthlyTrafficLimit", Types: []string{"string", "null"}, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "swapEnabled", Types: []string{"boolean"}, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "swapSize", Types: []string{"integer", "null"}, Minimum: 0, Maximum: 1048576},
	{Name: "portLimit", Types: []string{"integer", "null"}, Minimum: 1, Maximum: 1000},
	{Name: "snapshotLimit", Types: []string{"integer", "null"}, Minimum: 0, Maximum: 1000},
	{Name: "backupLimit", Types: []string{"integer", "null"}, Minimum: 0, Maximum: 1000},
	{Name: "siteLimit", Types: []string{"integer", "null"}, Minimum: 0, Maximum: 1000},
	{Name: "nested", Types: []string{"boolean"}, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "privileged", Types: []string{"boolean"}, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "limitsRead", Types: []string{"string", "null"}, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "limitsWrite", Types: []string{"string", "null"}, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "limitsReadIops", Types: []string{"integer", "null"}, Minimum: 0, Maximum: math.NaN()},
	{Name: "limitsWriteIops", Types: []string{"integer", "null"}, Minimum: 0, Maximum: math.NaN()},
	{Name: "limitsIngress", Types: []string{"string", "null"}, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "limitsEgress", Types: []string{"string", "null"}, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "limitsProcesses", Types: []string{"integer", "null"}, Minimum: 0, Maximum: math.NaN()},
	{Name: "limitsCpuPriority", Types: []string{"integer", "null"}, Minimum: 0, Maximum: 10},
	{Name: "bootAutostart", Types: []string{"boolean", "null"}, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "bootAutostartPriority", Types: []string{"integer", "null"}, Minimum: 0, Maximum: 100},
	{Name: "bootAutostartDelay", Types: []string{"integer", "null"}, Minimum: 5, Maximum: 600},
	{Name: "bootHostShutdownTimeout", Types: []string{"integer", "null"}, Minimum: 30, Maximum: 600},
}

// batchConfigConfig is the typed view of `config`. Every JSON number is a
// float64 because that is what JavaScript (and therefore AJV) stores; conversion
// to the 32-bit columns happens at the DB boundary with an explicit range check.
type batchConfigConfig struct {
	CPU                     batchConfigOptional[float64]
	Memory                  batchConfigOptional[float64]
	Disk                    batchConfigOptional[float64]
	MonthlyTrafficLimit     batchConfigOptional[string]
	SwapEnabled             batchConfigOptional[bool]
	SwapSize                batchConfigOptional[float64]
	PortLimit               batchConfigOptional[float64]
	SnapshotLimit           batchConfigOptional[float64]
	BackupLimit             batchConfigOptional[float64]
	SiteLimit               batchConfigOptional[float64]
	Nested                  batchConfigOptional[bool]
	Privileged              batchConfigOptional[bool]
	LimitsRead              batchConfigOptional[string]
	LimitsWrite             batchConfigOptional[string]
	LimitsReadIops          batchConfigOptional[float64]
	LimitsWriteIops         batchConfigOptional[float64]
	LimitsIngress           batchConfigOptional[string]
	LimitsEgress            batchConfigOptional[string]
	LimitsProcesses         batchConfigOptional[float64]
	LimitsCPUPriority       batchConfigOptional[float64]
	BootAutostart           batchConfigOptional[bool]
	BootAutostartPriority   batchConfigOptional[float64]
	BootAutostartDelay      batchConfigOptional[float64]
	BootHostShutdownTimeout batchConfigOptional[float64]
}

// batchConfigBody is the validated request body. RawConfig keeps the AJV-coerced
// config object (unknown keys included) because retryPayload echoes it verbatim.
type batchConfigBody struct {
	InstanceIDValues []float64
	Config           batchConfigConfig
	RawConfig        map[string]any
}

func batchConfigOptionalNumber(fields map[string]any, name string) batchConfigOptional[float64] {
	raw, present := fields[name]
	if !present {
		return batchConfigOptional[float64]{}
	}
	option := batchConfigOptional[float64]{Present: true}
	if number, ok := raw.(float64); ok {
		option.Value = &number
	}
	return option
}

func batchConfigOptionalString(fields map[string]any, name string) batchConfigOptional[string] {
	raw, present := fields[name]
	if !present {
		return batchConfigOptional[string]{}
	}
	option := batchConfigOptional[string]{Present: true}
	if text, ok := raw.(string); ok {
		option.Value = &text
	}
	return option
}

func batchConfigOptionalBool(fields map[string]any, name string) batchConfigOptional[bool] {
	raw, present := fields[name]
	if !present {
		return batchConfigOptional[bool]{}
	}
	option := batchConfigOptional[bool]{Present: true}
	if flag, ok := raw.(bool); ok {
		option.Value = &flag
	}
	return option
}

// batchConfigUniqueItemsMessage mirrors AJV's uniqueItems for an integer item type
// (the loopN optimisation): scan from the end, skip items that are not integers,
// and report the first duplicate with the stored (higher) index first.
func batchConfigUniqueItemsMessage(items []any) string {
	indices := make(map[float64]int, len(items))
	for i := len(items) - 1; i >= 0; i-- {
		number, ok := items[i].(float64)
		if !ok || !batchConfigMatchesType(number, "integer") {
			continue
		}
		if j, seen := indices[number]; seen {
			return fmt.Sprintf("must NOT have duplicate items (items ## %d and %d are identical)", j, i)
		}
		indices[number] = i
	}
	return ""
}

// batchConfigCoerceArray mirrors AJV's array-type handling for `instanceIds`:
// an array is used as-is (a one-element array of arrays unwraps once, because the
// unwrap step also applies when the inner value already satisfies type "array"),
// and a scalar or null is wrapped into a one-element array.
func batchConfigCoerceArray(value any) ([]any, bool) {
	if array, ok := value.([]any); ok {
		if len(array) == 1 {
			if inner, isArray := array[0].([]any); isArray {
				return inner, true
			}
		}
		return array, true
	}
	switch value.(type) {
	case string, float64, bool, nil:
		return []any{value}, true
	}
	return nil, false
}

// batchConfigValidateInstanceIDs mirrors `instanceIds` in the schema:
// array → minItems/maxItems → uniqueItems → items {type:integer, minimum:1}.
func batchConfigValidateInstanceIDs(value any) ([]float64, string) {
	array, ok := batchConfigCoerceArray(value)
	if !ok {
		return nil, "must be array"
	}
	if len(array) < 1 {
		return nil, "must NOT have fewer than 1 items"
	}
	if len(array) > batchConfigInstanceIDMaxItems {
		return nil, "must NOT have more than 1000 items"
	}
	if message := batchConfigUniqueItemsMessage(array); message != "" {
		return nil, message
	}

	ids := make([]float64, 0, len(array))
	for index, item := range array {
		coerced, message := batchConfigCoerceValue(item, []string{"integer"})
		if message != "" {
			return nil, message
		}
		number, _ := coerced.(float64)
		if message := batchConfigBoundsMessage(number, 1, math.NaN()); message != "" {
			return nil, message
		}
		// AJV coerces the array element in place; normalizeBatchInstanceIds then
		// sees the coerced values (so [1,"1"] reaches it as [1,1]).
		array[index] = number
		ids = append(ids, number)
	}
	return ids, ""
}

// batchConfigValidateBody mirrors the route's JSON schema (required first, then
// `instanceIds`, then `config` and each of its properties in schema order).
func batchConfigValidateBody(decoded any) (*batchConfigBody, string) {
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, "must be object"
	}
	for _, required := range []string{"instanceIds", "config"} {
		if _, present := object[required]; !present {
			return nil, required + " is required"
		}
	}

	instanceIDValues, message := batchConfigValidateInstanceIDs(object["instanceIds"])
	if message != "" {
		return nil, message
	}

	rawConfig, ok := object["config"].(map[string]any)
	if !ok {
		// `{ type: 'object', properties: {...} }` has a rule for the object type,
		// so AJV reports the type error instead of silently skipping validation.
		return nil, "must be object"
	}

	for _, spec := range batchConfigConfigFieldSpecs {
		value, present := rawConfig[spec.Name]
		if !present {
			continue
		}
		coerced, message := batchConfigCoerceValue(value, spec.Types)
		if message != "" {
			return nil, message
		}
		if message := batchConfigBoundsMessage(coerced, spec.Minimum, spec.Maximum); message != "" {
			return nil, message
		}
		rawConfig[spec.Name] = coerced
	}

	return &batchConfigBody{
		InstanceIDValues: instanceIDValues,
		RawConfig:        rawConfig,
		Config: batchConfigConfig{
			CPU:                     batchConfigOptionalNumber(rawConfig, "cpu"),
			Memory:                  batchConfigOptionalNumber(rawConfig, "memory"),
			Disk:                    batchConfigOptionalNumber(rawConfig, "disk"),
			MonthlyTrafficLimit:     batchConfigOptionalString(rawConfig, "monthlyTrafficLimit"),
			SwapEnabled:             batchConfigOptionalBool(rawConfig, "swapEnabled"),
			SwapSize:                batchConfigOptionalNumber(rawConfig, "swapSize"),
			PortLimit:               batchConfigOptionalNumber(rawConfig, "portLimit"),
			SnapshotLimit:           batchConfigOptionalNumber(rawConfig, "snapshotLimit"),
			BackupLimit:             batchConfigOptionalNumber(rawConfig, "backupLimit"),
			SiteLimit:               batchConfigOptionalNumber(rawConfig, "siteLimit"),
			Nested:                  batchConfigOptionalBool(rawConfig, "nested"),
			Privileged:              batchConfigOptionalBool(rawConfig, "privileged"),
			LimitsRead:              batchConfigOptionalString(rawConfig, "limitsRead"),
			LimitsWrite:             batchConfigOptionalString(rawConfig, "limitsWrite"),
			LimitsReadIops:          batchConfigOptionalNumber(rawConfig, "limitsReadIops"),
			LimitsWriteIops:         batchConfigOptionalNumber(rawConfig, "limitsWriteIops"),
			LimitsIngress:           batchConfigOptionalString(rawConfig, "limitsIngress"),
			LimitsEgress:            batchConfigOptionalString(rawConfig, "limitsEgress"),
			LimitsProcesses:         batchConfigOptionalNumber(rawConfig, "limitsProcesses"),
			LimitsCPUPriority:       batchConfigOptionalNumber(rawConfig, "limitsCpuPriority"),
			BootAutostart:           batchConfigOptionalBool(rawConfig, "bootAutostart"),
			BootAutostartPriority:   batchConfigOptionalNumber(rawConfig, "bootAutostartPriority"),
			BootAutostartDelay:      batchConfigOptionalNumber(rawConfig, "bootAutostartDelay"),
			BootHostShutdownTimeout: batchConfigOptionalNumber(rawConfig, "bootHostShutdownTimeout"),
		},
	}, ""
}

// ==================== config predicates ====================

// batchConfigHasSwapConfig mirrors hasSwapConfig.
func batchConfigHasSwapConfig(config batchConfigConfig) bool {
	return config.SwapEnabled.Present || config.SwapSize.Present
}

// batchConfigHasAdvancedIncusConfig mirrors hasAdvancedIncusConfig.
func batchConfigHasAdvancedIncusConfig(config batchConfigConfig) bool {
	return config.Nested.Present || config.Privileged.Present ||
		config.LimitsRead.Present || config.LimitsWrite.Present ||
		config.LimitsReadIops.Present || config.LimitsWriteIops.Present ||
		config.LimitsIngress.Present || config.LimitsEgress.Present ||
		config.LimitsProcesses.Present || config.LimitsCPUPriority.Present ||
		config.BootAutostart.Present || config.BootAutostartPriority.Present ||
		config.BootAutostartDelay.Present || config.BootHostShutdownTimeout.Present
}

// batchConfigHasDBAdvancedConfig mirrors hasDbAdvancedConfig (nested/privileged are
// Incus-only, so they are absent here).
func batchConfigHasDBAdvancedConfig(config batchConfigConfig) bool {
	return config.LimitsRead.Present || config.LimitsWrite.Present ||
		config.LimitsReadIops.Present || config.LimitsWriteIops.Present ||
		config.LimitsIngress.Present || config.LimitsEgress.Present ||
		config.LimitsProcesses.Present || config.LimitsCPUPriority.Present ||
		config.BootAutostart.Present || config.BootAutostartPriority.Present ||
		config.BootAutostartDelay.Present || config.BootHostShutdownTimeout.Present
}

// batchConfigHasIncusConfig mirrors hasIncusConfig.
func batchConfigHasIncusConfig(config batchConfigConfig) bool {
	return config.CPU.Present || config.Memory.Present || config.Disk.Present ||
		batchConfigHasSwapConfig(config) || batchConfigHasAdvancedIncusConfig(config)
}

// batchConfigIsRetryableError mirrors isRetryableError.
func batchConfigIsRetryableError(message string) bool {
	lowered := strings.ToLower(message)
	for _, candidate := range batchConfigRetryableErrors {
		if strings.Contains(lowered, strings.ToLower(candidate)) {
			return true
		}
	}
	return false
}

// ==================== SWAP helpers (lib/instance-swap.ts) ====================

// batchConfigNormalizeConfiguredSwapSize mirrors normalizeConfiguredSwapSize:
// null stays null, otherwise max(0, floor(size)).
func batchConfigNormalizeConfiguredSwapSize(sizeMB *int32) *int32 {
	if sizeMB == nil {
		return nil
	}
	value := *sizeMB
	if value < 0 {
		value = 0
	}
	return &value
}

// batchConfigResolveEffectiveSwapSize mirrors resolveEffectiveSwapSize.
func batchConfigResolveEffectiveSwapSize(instanceSizeMB, planSizeMB *int32) int32 {
	if normalized := batchConfigNormalizeConfiguredSwapSize(instanceSizeMB); normalized != nil {
		return *normalized
	}
	if normalized := batchConfigNormalizeConfiguredSwapSize(planSizeMB); normalized != nil {
		return *normalized
	}
	return 0
}

// batchConfigFormatIncusSwapLimit mirrors formatIncusSwapLimit.
func batchConfigFormatIncusSwapLimit(sizeMB int32) string {
	value := sizeMB
	if value < 0 {
		value = 0
	}
	return strconv.FormatInt(int64(value), 10) + "MiB"
}

// batchConfigResolveIncusSwapValue mirrors resolveIncusSwapValue.
func batchConfigResolveIncusSwapValue(enabled bool, sizeMB int32) string {
	if !enabled || sizeMB <= 0 {
		return "false"
	}
	return batchConfigFormatIncusSwapLimit(sizeMB)
}

// ==================== numeric conversions ====================

// batchConfigRound mirrors Math.round (half towards +Infinity) and rejects values
// that cannot fit the 32-bit columns Prisma/Incus accept.
func batchConfigRound(value float64) (int32, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("Unable to fit integer value '%s' into a 32-bit signed integer.",
			formatTerminalCommandNumber(value))
	}
	rounded := math.Floor(value + 0.5)
	if rounded < math.MinInt32 || rounded > math.MaxInt32 {
		return 0, fmt.Errorf("Unable to fit integer value '%s' into a 32-bit signed integer.",
			formatTerminalCommandNumber(rounded))
	}
	return int32(rounded), nil
}

// batchConfigInt32 mirrors the integer-typed schema fields (already integral after
// AJV) into the 32-bit column range, reporting the same overflow the Prisma write
// would have raised.
func batchConfigInt32(value float64) (int32, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < math.MinInt32 || value > math.MaxInt32 {
		return 0, fmt.Errorf("Unable to fit integer value '%s' into a 32-bit signed integer.",
			formatTerminalCommandNumber(value))
	}
	return int32(value), nil
}

// ==================== per-instance worker ====================

// batchConfigStorageOverrides mirrors ResolvedStorageOverrides.
type batchConfigStorageOverrides struct {
	LimitsRead      batchConfigOptional[string]
	LimitsReadIops  batchConfigOptional[float64]
	LimitsWrite     batchConfigOptional[string]
	LimitsWriteIops batchConfigOptional[float64]
}

// batchConfigResolveStorageOverrides mirrors resolveStorageOverrides.
func batchConfigResolveStorageOverrides(config batchConfigConfig) batchConfigStorageOverrides {
	var overrides batchConfigStorageOverrides

	if config.LimitsRead.Present {
		overrides.LimitsRead = config.LimitsRead
		overrides.LimitsReadIops = batchConfigOptional[float64]{Present: true}
	} else if config.LimitsReadIops.Present {
		overrides.LimitsRead = batchConfigOptional[string]{Present: true}
		overrides.LimitsReadIops = config.LimitsReadIops
	}

	if config.LimitsWrite.Present {
		overrides.LimitsWrite = config.LimitsWrite
		overrides.LimitsWriteIops = batchConfigOptional[float64]{Present: true}
	} else if config.LimitsWriteIops.Present {
		overrides.LimitsWrite = batchConfigOptional[string]{Present: true}
		overrides.LimitsWriteIops = config.LimitsWriteIops
	}

	return overrides
}

// batchConfigBoolString mirrors `cond ? 'true' : 'false'`.
func batchConfigBoolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// batchConfigCloneDevices mirrors `JSON.parse(JSON.stringify(devices))`.
func batchConfigCloneDevices(devices map[string]map[string]any) map[string]map[string]any {
	cloned := make(map[string]map[string]any, len(devices))
	for name, attributes := range devices {
		copied := make(map[string]any, len(attributes))
		for key, value := range attributes {
			copied[key] = value
		}
		cloned[name] = copied
	}
	return cloned
}

// batchConfigOutcome is the internal result of one instance update.
type batchConfigOutcome struct {
	Success bool
	Error   string
}

// batchConfigApplyInstance mirrors updateSingleInstance's body. Errors returned here
// become that instance's failure item; nothing is rolled back.
func batchConfigApplyInstance(
	ctx context.Context,
	instance db.BatchConfigInstance,
	config batchConfigConfig,
	monthlyTrafficLimit batchConfigOptional[uint64],
	client *lib.IncusClient,
) error {
	activeTask, err := db.BatchConfigGetActiveTaskForInstance(ctx, instance.ID)
	if err != nil {
		return err
	}
	if activeTask != nil {
		return fmt.Errorf("实例存在进行中的任务: %s", activeTask.TaskType)
	}

	hasSwapConfig := batchConfigHasSwapConfig(config)
	instanceKind := "container"
	if instance.PackageInstanceType != nil && *instance.PackageInstanceType == "vm" {
		instanceKind = "vm"
	}
	currentSwapSize := batchConfigResolveEffectiveSwapSize(instance.SwapSize, instance.PackagePlanSwapSize)

	nextSwapEnabled := instance.SwapEnabled
	if config.SwapEnabled.Present && config.SwapEnabled.Value != nil {
		nextSwapEnabled = *config.SwapEnabled.Value
	}

	nextSwapSize := currentSwapSize
	if config.SwapSize.Present {
		if config.SwapSize.Value != nil {
			configured := *config.SwapSize.Value
			if configured < 0 {
				configured = 0
			}
			converted, err := batchConfigInt32(configured)
			if err != nil {
				return err
			}
			nextSwapSize = converted
		} else {
			nextSwapSize = 0
		}
	}

	storageOverrides := batchConfigResolveStorageOverrides(config)

	if hasSwapConfig && instanceKind == "vm" {
		return errors.New("虚拟机不支持批量 SWAP 配置")
	}
	if nextSwapEnabled && nextSwapSize <= 0 {
		return errors.New("启用 SWAP 前需要设置大于 0 的 SWAP 大小")
	}

	// Numeric conversions happen once; the same 32-bit values feed Incus and the DB.
	var cpuValue, memoryValue, diskValue *int32
	if config.CPU.Present && config.CPU.Value != nil {
		converted, err := batchConfigInt32(*config.CPU.Value)
		if err != nil {
			return err
		}
		cpuValue = &converted
	}
	if config.Memory.Present && config.Memory.Value != nil {
		converted, err := batchConfigInt32(*config.Memory.Value)
		if err != nil {
			return err
		}
		memoryValue = &converted
	}
	if config.Disk.Present && config.Disk.Value != nil {
		converted, err := batchConfigRound(*config.Disk.Value)
		if err != nil {
			return err
		}
		diskValue = &converted
	}

	// 1. 更新资源配置（CPU、内存、磁盘）到 Incus
	if client != nil && (config.CPU.Present || config.Memory.Present || config.Disk.Present) {
		var cpuArg, memoryArg, diskArg *int
		if cpuValue != nil {
			value := int(*cpuValue)
			cpuArg = &value
		}
		if memoryValue != nil {
			value := int(*memoryValue)
			memoryArg = &value
		}
		if diskValue != nil {
			value := int(*diskValue)
			diskArg = &value
		}
		if _, err := lib.PatchInstanceResources(ctx, client, instance.IncusID, cpuArg, memoryArg, diskArg); err != nil {
			return err
		}
	}

	// 2. 更新高级配置到 Incus
	if client != nil && (batchConfigHasAdvancedIncusConfig(config) || hasSwapConfig) {
		incusConfig := map[string]any{}
		needsDeviceConfig := config.LimitsRead.Present || config.LimitsReadIops.Present ||
			config.LimitsWrite.Present || config.LimitsWriteIops.Present ||
			config.LimitsIngress.Present || config.LimitsEgress.Present
		var incusDevices map[string]map[string]any

		if hasSwapConfig {
			incusConfig["limits.memory.swap"] = batchConfigResolveIncusSwapValue(nextSwapEnabled, nextSwapSize)
		}

		// 容器权限
		if config.Nested.Present && config.Nested.Value != nil {
			incusConfig["security.nesting"] = batchConfigBoolString(*config.Nested.Value)
		}
		if config.Privileged.Present && config.Privileged.Value != nil {
			incusConfig["security.privileged"] = batchConfigBoolString(*config.Privileged.Value)
		}

		// 数值配置：provided + non-null → String(value); provided + null → null (unset)
		batchConfigSetIncusNumber(incusConfig, "limits.processes", config.LimitsProcesses)
		batchConfigSetIncusNumber(incusConfig, "limits.cpu.priority", config.LimitsCPUPriority)
		batchConfigSetIncusBool(incusConfig, "boot.autostart", config.BootAutostart)
		batchConfigSetIncusNumber(incusConfig, "boot.autostart.priority", config.BootAutostartPriority)
		batchConfigSetIncusNumber(incusConfig, "boot.autostart.delay", config.BootAutostartDelay)
		batchConfigSetIncusNumber(incusConfig, "boot.host_shutdown_timeout", config.BootHostShutdownTimeout)

		if needsDeviceConfig {
			// 复制完整的设备列表，然后修改需要修改的设备
			currentInstance, err := lib.GetInstance(ctx, client, instance.IncusID)
			if err != nil {
				return err
			}
			if currentInstance.Devices != nil {
				incusDevices = batchConfigCloneDevices(currentInstance.Devices)
			} else {
				incusDevices = map[string]map[string]any{}
			}
		}

		// 存储 I/O 限制 (应用到 root 设备)
		if config.LimitsRead.Present || config.LimitsReadIops.Present ||
			config.LimitsWrite.Present || config.LimitsWriteIops.Present {
			if incusDevices == nil {
				incusDevices = map[string]map[string]any{}
			}
			if _, ok := incusDevices["root"]; !ok {
				incusDevices["root"] = map[string]any{"path": "/", "pool": "default", "type": "disk"}
			}
			root := incusDevices["root"]

			if config.LimitsRead.Present {
				if config.LimitsRead.Value != nil {
					root["limits.read"] = *config.LimitsRead.Value
				} else {
					delete(root, "limits.read")
				}
			} else if config.LimitsReadIops.Present {
				if config.LimitsReadIops.Value != nil {
					iops, err := batchConfigInt32(*config.LimitsReadIops.Value)
					if err != nil {
						return err
					}
					root["limits.read"] = strconv.FormatInt(int64(iops), 10) + "iops"
				} else {
					delete(root, "limits.read")
				}
			}

			if config.LimitsWrite.Present {
				if config.LimitsWrite.Value != nil {
					root["limits.write"] = *config.LimitsWrite.Value
				} else {
					delete(root, "limits.write")
				}
			} else if config.LimitsWriteIops.Present {
				if config.LimitsWriteIops.Value != nil {
					iops, err := batchConfigInt32(*config.LimitsWriteIops.Value)
					if err != nil {
						return err
					}
					root["limits.write"] = strconv.FormatInt(int64(iops), 10) + "iops"
				} else {
					delete(root, "limits.write")
				}
			}
		}

		// 网络限制 (应用到 eth0 设备)
		if config.LimitsIngress.Present || config.LimitsEgress.Present {
			if incusDevices == nil {
				incusDevices = map[string]map[string]any{}
			}
			if _, ok := incusDevices["eth0"]; !ok {
				incusDevices["eth0"] = map[string]any{
					"type":    "nic",
					"name":    "eth0",
					"network": "incusbr0",
				}
			}
			eth0 := incusDevices["eth0"]

			if config.LimitsIngress.Present {
				if config.LimitsIngress.Value != nil {
					eth0["limits.ingress"] = *config.LimitsIngress.Value
				} else {
					delete(eth0, "limits.ingress")
				}
			}
			if config.LimitsEgress.Present {
				if config.LimitsEgress.Value != nil {
					eth0["limits.egress"] = *config.LimitsEgress.Value
				} else {
					delete(eth0, "limits.egress")
				}
			}
		}

		updates := map[string]any{}
		if len(incusConfig) > 0 {
			updates["config"] = incusConfig
		}
		if len(incusDevices) > 0 {
			updates["devices"] = incusDevices
		}
		if len(updates) > 0 {
			if _, err := lib.UpdateInstance(ctx, client, instance.IncusID, updates); err != nil {
				return err
			}
		}
	}

	// 3. 更新数据库中的资源配置
	if config.CPU.Present || config.Memory.Present || config.Disk.Present || config.MonthlyTrafficLimit.Present {
		update := db.BatchConfigInstanceResourcesUpdate{
			CPUProvided:                 config.CPU.Present,
			MemoryProvided:              config.Memory.Present,
			DiskProvided:                config.Disk.Present,
			MonthlyTrafficLimitProvided: config.MonthlyTrafficLimit.Present,
			MonthlyTrafficLimit:         monthlyTrafficLimit.Value,
		}
		if cpuValue != nil {
			update.CPU = *cpuValue
		}
		if memoryValue != nil {
			update.Memory = *memoryValue
		}
		if diskValue != nil {
			update.Disk = *diskValue
		}
		// 注意：宿主机资源使用量在批量处理完成后统一更新，避免并发问题
		if err := db.BatchConfigUpdateInstanceResources(ctx, instance.ID, update); err != nil {
			return err
		}
	}

	// 4. 更新数据库中的配额限制
	if config.PortLimit.Present || config.SnapshotLimit.Present ||
		config.BackupLimit.Present || config.SiteLimit.Present {
		update := db.BatchConfigInstanceQuotaUpdate{
			PortLimitProvided:     config.PortLimit.Present,
			SnapshotLimitProvided: config.SnapshotLimit.Present,
			BackupLimitProvided:   config.BackupLimit.Present,
			SiteLimitProvided:     config.SiteLimit.Present,
		}
		if config.PortLimit.Present && config.PortLimit.Value != nil {
			converted, err := batchConfigInt32(*config.PortLimit.Value)
			if err != nil {
				return err
			}
			update.PortLimit = &converted
		}
		if config.SnapshotLimit.Present && config.SnapshotLimit.Value != nil {
			converted, err := batchConfigInt32(*config.SnapshotLimit.Value)
			if err != nil {
				return err
			}
			update.SnapshotLimit = &converted
		}
		if config.BackupLimit.Present && config.BackupLimit.Value != nil {
			converted, err := batchConfigInt32(*config.BackupLimit.Value)
			if err != nil {
				return err
			}
			update.BackupLimit = &converted
		}
		if config.SiteLimit.Present && config.SiteLimit.Value != nil {
			converted, err := batchConfigInt32(*config.SiteLimit.Value)
			if err != nil {
				return err
			}
			update.SiteLimit = &converted
		}
		if err := db.BatchConfigUpdateInstanceQuota(ctx, instance.ID, update); err != nil {
			return err
		}
	}

	// 5. 更新数据库中的 SWAP 配置
	if hasSwapConfig {
		update := db.BatchConfigInstanceSwapUpdate{}
		if config.SwapEnabled.Present && config.SwapEnabled.Value != nil {
			update.SwapEnabledProvided = true
			update.SwapEnabled = *config.SwapEnabled.Value
		}
		if config.SwapSize.Present {
			update.SwapSizeProvided = true
			if config.SwapSize.Value != nil {
				converted, err := batchConfigInt32(*config.SwapSize.Value)
				if err != nil {
					return err
				}
				update.SwapSize = &converted
			}
		} else if config.SwapEnabled.Present && config.SwapEnabled.Value != nil && *config.SwapEnabled.Value {
			effective := nextSwapSize
			update.SwapSizeProvided = true
			update.SwapSize = &effective
		}
		if err := db.BatchConfigUpdateInstanceSwap(ctx, instance.ID, update); err != nil {
			return err
		}
	}

	// 6. 更新数据库中的高级配置
	if batchConfigHasDBAdvancedConfig(config) {
		update := db.BatchConfigInstanceAdvancedConfigUpdate{
			LimitsReadProvided:              storageOverrides.LimitsRead.Present,
			LimitsRead:                      storageOverrides.LimitsRead.Value,
			LimitsWriteProvided:             storageOverrides.LimitsWrite.Present,
			LimitsWrite:                     storageOverrides.LimitsWrite.Value,
			LimitsIngressProvided:           config.LimitsIngress.Present,
			LimitsIngress:                   config.LimitsIngress.Value,
			LimitsEgressProvided:            config.LimitsEgress.Present,
			LimitsEgress:                    config.LimitsEgress.Value,
			LimitsProcessesProvided:         config.LimitsProcesses.Present,
			LimitsCPUPriorityProvided:       config.LimitsCPUPriority.Present,
			BootAutostartProvided:           config.BootAutostart.Present,
			BootAutostart:                   config.BootAutostart.Value,
			BootAutostartPriorityProvided:   config.BootAutostartPriority.Present,
			BootAutostartDelayProvided:      config.BootAutostartDelay.Present,
			BootHostShutdownTimeoutProvided: config.BootHostShutdownTimeout.Present,
		}

		if storageOverrides.LimitsReadIops.Present && storageOverrides.LimitsReadIops.Value != nil {
			converted, err := batchConfigInt32(*storageOverrides.LimitsReadIops.Value)
			if err != nil {
				return err
			}
			update.LimitsReadIops = &converted
		}
		update.LimitsReadIopsProvided = storageOverrides.LimitsReadIops.Present
		if storageOverrides.LimitsWriteIops.Present && storageOverrides.LimitsWriteIops.Value != nil {
			converted, err := batchConfigInt32(*storageOverrides.LimitsWriteIops.Value)
			if err != nil {
				return err
			}
			update.LimitsWriteIops = &converted
		}
		update.LimitsWriteIopsProvided = storageOverrides.LimitsWriteIops.Present

		if config.LimitsProcesses.Present && config.LimitsProcesses.Value != nil {
			converted, err := batchConfigInt32(*config.LimitsProcesses.Value)
			if err != nil {
				return err
			}
			update.LimitsProcesses = &converted
		}
		if config.LimitsCPUPriority.Present && config.LimitsCPUPriority.Value != nil {
			converted, err := batchConfigInt32(*config.LimitsCPUPriority.Value)
			if err != nil {
				return err
			}
			update.LimitsCPUPriority = &converted
		}
		if config.BootAutostartPriority.Present && config.BootAutostartPriority.Value != nil {
			converted, err := batchConfigInt32(*config.BootAutostartPriority.Value)
			if err != nil {
				return err
			}
			update.BootAutostartPriority = &converted
		}
		if config.BootAutostartDelay.Present && config.BootAutostartDelay.Value != nil {
			converted, err := batchConfigInt32(*config.BootAutostartDelay.Value)
			if err != nil {
				return err
			}
			update.BootAutostartDelay = &converted
		}
		if config.BootHostShutdownTimeout.Present && config.BootHostShutdownTimeout.Value != nil {
			converted, err := batchConfigInt32(*config.BootHostShutdownTimeout.Value)
			if err != nil {
				return err
			}
			update.BootHostShutdownTimeout = &converted
		}

		if err := db.BatchConfigUpdateInstanceConfig(ctx, instance.ID, update); err != nil {
			return err
		}
	}

	return nil
}

// batchConfigSetIncusNumber mirrors the repeated
// `if (config.x !== undefined && config.x !== null) {...} else if (config.x !== undefined) {...}`
// blocks.
func batchConfigSetIncusNumber(target map[string]any, key string, option batchConfigOptional[float64]) {
	if !option.Present {
		return
	}
	if option.Value == nil {
		target[key] = nil
		return
	}
	// String(value): integral values print without a decimal point.
	target[key] = formatTerminalCommandNumber(*option.Value)
}

// batchConfigSetIncusBool mirrors the boot.autostart block.
func batchConfigSetIncusBool(target map[string]any, key string, option batchConfigOptional[bool]) {
	if !option.Present {
		return
	}
	if option.Value == nil {
		target[key] = nil
		return
	}
	target[key] = batchConfigBoolString(*option.Value)
}

// batchConfigUpdateSingleInstance mirrors updateSingleInstance.
func batchConfigUpdateSingleInstance(
	ctx context.Context,
	instance db.BatchConfigInstance,
	config batchConfigConfig,
	monthlyTrafficLimit batchConfigOptional[uint64],
	client *lib.IncusClient,
) batchConfigOutcome {
	if err := batchConfigApplyInstance(ctx, instance, config, monthlyTrafficLimit, client); err != nil {
		message := err.Error()
		log.Printf("[BatchConfig] Failed to update instance %s: %s", instance.Name, message)
		return batchConfigOutcome{Success: false, Error: message}
	}
	return batchConfigOutcome{Success: true}
}

// ==================== bounded worker pool ====================

// batchConfigForEachLimited runs fn over items with at most `limit` concurrent
// invocations — the same contract as pLimit(limit).map(...).
func batchConfigForEachLimited[T any](limit int, items []T, fn func(T)) {
	if limit < 1 {
		limit = 1
	}
	semaphore := make(chan struct{}, limit)
	var waitGroup sync.WaitGroup

	for _, item := range items {
		waitGroup.Add(1)
		semaphore <- struct{}{}
		go func(item T) {
			defer waitGroup.Done()
			defer func() { <-semaphore }()
			fn(item)
		}(item)
	}
	waitGroup.Wait()
}

// ==================== response types ====================

// batchConfigInternalError is the generic 500 used where the TypeScript lets a
// database error bubble to Fastify's error handler (every Go module in this port
// uses the fixed envelope).
func batchConfigInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// batchConfigResultItem mirrors ResultItem. `error` is omitted for successes
// (JSON.stringify drops undefined).
type batchConfigResultItem struct {
	InstanceID int32   `json:"instanceId"`
	IncusID    string  `json:"incusId"`
	Name       string  `json:"name"`
	Success    bool    `json:"success"`
	Error      *string `json:"error,omitempty"`
}

type batchConfigRetryPayload struct {
	InstanceIDs []int32        `json:"instanceIds"`
	Config      map[string]any `json:"config"`
}

type batchConfigResponse struct {
	Success      bool                     `json:"success"`
	TotalCount   int                      `json:"totalCount"`
	SuccessCount int                      `json:"successCount"`
	FailedCount  int                      `json:"failedCount"`
	FailedItems  []batchConfigResultItem  `json:"failedItems"`
	RetryPayload *batchConfigRetryPayload `json:"retryPayload,omitempty"`
}

// ==================== handler ====================

// batchConfigHandler mirrors POST /:id/instances/batch-config.
func batchConfigHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, bodyMessage := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": bodyMessage})
		return
	}
	body, validationMessage := batchConfigValidateBody(decoded.Value)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	hostID, ok := lib.ParsePositiveRouteID(c.Param("hostId"))
	if !ok || hostID > math.MaxInt32 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	hostIDValue := int32(hostID)

	normalizedIDs, ok := batchConfigNormalizeInstanceIDs(body.InstanceIDValues)
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid instance IDs")
		return
	}

	host, err := db.SnapshotsGetHostByID(ctx, hostIDValue)
	if err != nil {
		batchConfigInternalError(c)
		return
	}
	if host == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
		return
	}

	// 权限检查：管理员或宿主机所有者可以批量修改配置
	if user.Role != "admin" && host.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	// 检查是否有任何配置要修改
	if len(body.RawConfig) == 0 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrConfigNoChanges, "")
		return
	}

	var monthlyTrafficLimit batchConfigOptional[uint64]
	if body.Config.MonthlyTrafficLimit.Present {
		parsed, present, valid := lib.ParseNullablePostgresBigIntInput(body.Config.MonthlyTrafficLimit.Value)
		if !valid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid monthly traffic limit")
			return
		}
		monthlyTrafficLimit.Present = true
		if present {
			monthlyTrafficLimit.Value = &parsed
		}
	}

	instances, err := db.BatchConfigListInstancesForHost(ctx, hostIDValue, normalizedIDs)
	if err != nil {
		batchConfigInternalError(c)
		return
	}
	if len(instances) != len(normalizedIDs) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams,
			"Some instances are not available on this host")
		return
	}

	// 判断是否需要连接 Incus
	var client *lib.IncusClient
	if batchConfigHasIncusConfig(body.Config) {
		client, err = lib.GetIncusClient(ctx, lib.IncusClientOptions{
			URL:      host.URL,
			CertPath: host.CertPath,
			KeyPath:  host.KeyPath,
			HostID:   int64(host.ID),
		})
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "无法连接到宿主机: " + err.Error()})
			return
		}
	}

	results := make([]batchConfigResultItem, 0, len(instances))
	var resultsMu sync.Mutex
	var totalCPUDelta, totalMemoryDelta, totalDiskDelta int64

	// 跟踪资源变化量（用于批量完成后统一更新）
	batchConfigForEachLimited(batchConfigConcurrency, instances, func(instance db.BatchConfigInstance) {
		outcome := batchConfigUpdateSingleInstance(ctx, instance, body.Config, monthlyTrafficLimit, client)
		result := batchConfigResultItem{
			InstanceID: instance.ID,
			IncusID:    instance.IncusID,
			Name:       instance.Name,
			Success:    outcome.Success,
		}
		if !outcome.Success {
			message := outcome.Error
			result.Error = &message
		}

		resultsMu.Lock()
		results = append(results, result)
		// 累计资源变化量
		if result.Success {
			cpuDelta, memoryDelta, diskDelta := batchConfigResourceDeltas(body.Config, instance)
			totalCPUDelta += cpuDelta
			totalMemoryDelta += memoryDelta
			totalDiskDelta += diskDelta
		}
		resultsMu.Unlock()

		// 如果是可重试错误，自动重试一次
		if !result.Success && result.Error != nil && batchConfigIsRetryableError(*result.Error) {
			log.Printf("[BatchConfig] Retrying instance %s due to: %s", instance.Name, *result.Error)
			retryOutcome := batchConfigUpdateSingleInstance(ctx, instance, body.Config, monthlyTrafficLimit, client)
			retryResult := batchConfigResultItem{
				InstanceID: instance.ID,
				IncusID:    instance.IncusID,
				Name:       instance.Name,
				Success:    retryOutcome.Success,
			}
			if !retryOutcome.Success {
				message := retryOutcome.Error
				retryResult.Error = &message
			}

			resultsMu.Lock()
			// The TypeScript looks the entry up by instanceId (ids are unique).
			index := -1
			for i := range results {
				if results[i].InstanceID == instance.ID {
					index = i
					break
				}
			}
			if index >= 0 {
				// 如果重试成功，累计资源变化量
				if !results[index].Success && retryResult.Success {
					cpuDelta, memoryDelta, diskDelta := batchConfigResourceDeltas(body.Config, instance)
					totalCPUDelta += cpuDelta
					totalMemoryDelta += memoryDelta
					totalDiskDelta += diskDelta
				}
				results[index] = retryResult
			}
			resultsMu.Unlock()
		}
	})

	// 批量完成后，原子增加宿主机资源使用量，避免并发批量互相覆盖
	if totalCPUDelta != 0 || totalMemoryDelta != 0 || totalDiskDelta != 0 {
		if err := db.BatchConfigIncrementHostResourceUsage(ctx, hostIDValue, totalCPUDelta, totalMemoryDelta, totalDiskDelta); err != nil {
			batchConfigInternalError(c)
			return
		}
	}

	successCount := 0
	failedItems := make([]batchConfigResultItem, 0, len(results))
	for _, result := range results {
		if result.Success {
			successCount++
		} else {
			failedItems = append(failedItems, result)
		}
	}
	failedCount := len(failedItems)

	logUserID := int64(user.ID)
	db.CreateLog(ctx, &logUserID, "instance", "instance.batch_config",
		`Batch updated `+strconv.Itoa(successCount)+` instances config on host "`+host.Name+`", failed: `+strconv.Itoa(failedCount),
		batchConfigLogResult(failedCount > 0), db.CreateLogOptions{})

	response := batchConfigResponse{
		Success:      failedCount == 0,
		TotalCount:   len(instances),
		SuccessCount: successCount,
		FailedCount:  failedCount,
		FailedItems:  failedItems,
	}
	if failedCount > 0 {
		retryIDs := make([]int32, 0, len(failedItems))
		for _, item := range failedItems {
			retryIDs = append(retryIDs, item.InstanceID)
		}
		response.RetryPayload = &batchConfigRetryPayload{
			InstanceIDs: retryIDs,
			Config:      body.RawConfig,
		}
	}

	c.JSON(http.StatusOK, response)
}

// batchConfigLogResult mirrors `failedCount > 0 ? 'partial' : 'success'`. Note that
// 'partial' is not one of db.LogResult*'s three values; it is a literal in the
// TypeScript and is kept verbatim.
func batchConfigLogResult(partial bool) string {
	if partial {
		return "partial"
	}
	return db.LogResultSuccess
}

// batchConfigResourceDeltas mirrors the `config.x !== undefined` delta accounting.
func batchConfigResourceDeltas(config batchConfigConfig, instance db.BatchConfigInstance) (cpu, memory, disk int64) {
	if config.CPU.Present && config.CPU.Value != nil {
		cpu = int64(*config.CPU.Value) - int64(instance.CPU)
	}
	if config.Memory.Present && config.Memory.Value != nil {
		memory = int64(*config.Memory.Value) - int64(instance.Memory)
	}
	if config.Disk.Present && config.Disk.Value != nil {
		if rounded, err := batchConfigRound(*config.Disk.Value); err == nil {
			disk = int64(rounded) - int64(instance.Disk)
		}
	}
	return cpu, memory, disk
}

// batchConfigNormalizeInstanceIDs mirrors normalizeBatchInstanceIds: reject empty
// lists, non-safe-integers, non-positive values and duplicates (duplicates can
// survive AJV's uniqueItems because item values are coerced afterwards). Values
// beyond the INT4 column range are rejected too — Node would forward them to
// Prisma and surface a 500.
func batchConfigNormalizeInstanceIDs(instanceIDs []float64) ([]int32, bool) {
	if len(instanceIDs) == 0 {
		return nil, false
	}
	seen := make(map[int32]bool, len(instanceIDs))
	normalized := make([]int32, 0, len(instanceIDs))
	for _, value := range instanceIDs {
		if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) || value <= 0 {
			return nil, false
		}
		if value > math.MaxInt32 {
			return nil, false
		}
		instanceID := int32(value)
		if seen[instanceID] {
			return nil, false
		}
		seen[instanceID] = true
		normalized = append(normalized, instanceID)
	}
	return normalized, true
}

// RegisterBatchConfig mounts /api/hosts.
//
// The wildcard is `:hostId`, not the TypeScript's `:id`, because gin allows only one
// wildcard name per path position and routes/redeem_codes.go already mounts
// /api/hosts/:hostId/redeem-codes. The route still runs fastify.authenticate
// (any authenticated user; the handler does the admin/host-owner check).
func RegisterBatchConfig(r *gin.Engine) {
	group := r.Group("/api/hosts", middleware.Authenticate())

	group.POST("/:hostId/instances/batch-config", batchConfigHandler)
}

func init() {
	Register(RegisterBatchConfig)
}
