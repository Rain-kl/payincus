// Port of server/src/routes/instance-advanced-config.ts.
//
// app.ts does not mount this module directly: server/src/routes/instances.ts calls
// `registerAdvancedConfigRoute(fastify, rejectActiveInstanceWorkflowConflict,
// checkTransferLock)` inside its own plugin, so the only path is
// PATCH /api/instances/:id/advanced-config.
//
// Flow (line by line with the TypeScript):
//
//  1. Body schema first (Fastify validates before the handler): a non-object body
//     fails the allOf[0].not branch with "must NOT be valid", an unknown-only body
//     is stripped by removeAdditional and then hits the handler's empty-object
//     INVALID_PARAMS guard, and `{limitsRead, limitsReadIops}` is rejected by allOf.
//  2. Strict route-id regex (NOT AJV coercion: `/^[1-9]\d*$/` then safe integer) → 400 INVALID_ID.
//  3. instance → host → ownership → status (running|stopped) → in-process active set.
//  4. transfer lock, then active-workflow conflict (both live in unported modules —
//     see the seams below).
//  5. bandwidth arbitration: the configured Mbps value is normalized to Mbit, the
//     plan's normal line speed becomes the baseline, the traffic-overage throttle is
//     the cap, and effective = the most restrictive of the three.
//  6. Incus PATCH, then the DB write (configured_* store the request; limits_* store
//     the effective value). updateInstanceConfig keeps its empty-update early return.
//
// The two callbacks are injected in Node, so they are package-level function seams
// here. Until transfers / instance-task modules are ported they report "nothing to
// block", which is the only honest default: a fake 409 would break the route, and a
// bug-for-bug in-memory copy of those modules would be a second source of truth.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/url"
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

const (
	// instanceAdvancedConfigIncusTimeout mirrors AbortSignal.timeout(30000).
	instanceAdvancedConfigIncusTimeout = 30 * time.Second
	// instanceAdvancedConfigLockExpireMs / WaitTimeoutMs mirror the withLock options.
	instanceAdvancedConfigLockExpireMs      = 90000
	instanceAdvancedConfigLockWaitTimeoutMs = 1000

	// instanceAdvancedConfigOverageThrottleConfigKey mirrors
	// TRAFFIC_OVERAGE_THROTTLE_CONFIG_KEY.
	instanceAdvancedConfigOverageThrottleConfigKey = "traffic_overage_throttle_speed"
	// instanceAdvancedConfigDefaultOverageThrottleSpeed mirrors
	// DEFAULT_TRAFFIC_OVERAGE_THROTTLE_SPEED.
	instanceAdvancedConfigDefaultOverageThrottleSpeed = "1Mbit"

	// instanceAdvancedConfigMBBytes is MB_IN_BYTES (1024n * 1024n).
	instanceAdvancedConfigMBBytes = 1024 * 1024

	// instanceAdvancedConfigLogModule mirrors the literal 'instance' module.
	instanceAdvancedConfigLogModule = "instance"
)

// InstanceAdvancedConfigTransferLock mirrors checkTransferLock(instanceId, reply).
// It returns true when it already wrote a response (the request must stop).
//
// NOT PORTED YET: the transfers module (db.hasPendingTransfer) is a later wave.
// The seam exists so that module can assign it without editing this file.
var InstanceAdvancedConfigTransferLock = func(c *gin.Context, instanceID int32) bool {
	return false
}

// InstanceAdvancedConfigConflictChecker mirrors
// rejectActiveInstanceWorkflowConflict(reply, instanceId): restore tasks, backup
// upload tasks and instance tasks are all part of unported modules, so the default
// reports no conflict. Assign this from those modules when they land.
var InstanceAdvancedConfigConflictChecker = func(c *gin.Context, instanceID int32) bool {
	return false
}

// instanceAdvancedConfigActive mirrors the module-level `const active = new Set()`.
// It rejects a concurrent update for the same instance in this process; the AGENTS.md
// single-instance invariant is what makes an in-memory set sufficient, exactly as in
// the Node backend.
var instanceAdvancedConfigActive = struct {
	sync.Mutex
	ids map[int32]bool
}{ids: map[int32]bool{}}

func instanceAdvancedConfigTryAcquire(id int32) bool {
	instanceAdvancedConfigActive.Lock()
	defer instanceAdvancedConfigActive.Unlock()
	if instanceAdvancedConfigActive.ids[id] {
		return false
	}
	instanceAdvancedConfigActive.ids[id] = true
	return true
}

func instanceAdvancedConfigRelease(id int32) {
	instanceAdvancedConfigActive.Lock()
	defer instanceAdvancedConfigActive.Unlock()
	delete(instanceAdvancedConfigActive.ids, id)
}

// instanceAdvancedConfigFields mirrors `advancedFields` (camelCase -> column).
var instanceAdvancedConfigFields = map[string]string{
	"limitsRead":              "limits_read",
	"limitsWrite":             "limits_write",
	"limitsReadIops":          "limits_read_iops",
	"limitsWriteIops":         "limits_write_iops",
	"limitsIngress":           "limits_ingress",
	"limitsEgress":            "limits_egress",
	"limitsProcesses":         "limits_processes",
	"limitsCpuPriority":       "limits_cpu_priority",
	"bootAutostart":           "boot_autostart",
	"bootAutostartPriority":   "boot_autostart_priority",
	"bootAutostartDelay":      "boot_autostart_delay",
	"bootHostShutdownTimeout": "boot_host_shutdown_timeout",
}

// instanceAdvancedConfigDefaults mirrors `advancedDefaults`.
var instanceAdvancedConfigDefaults = map[string]any{
	"limits_read":                "100MB",
	"limits_write":               "100MB",
	"limits_read_iops":           500,
	"limits_write_iops":          500,
	"limits_ingress":             "300Mbit",
	"limits_egress":              "300Mbit",
	"limits_processes":           500,
	"limits_cpu_priority":        10,
	"boot_autostart":             true,
	"boot_autostart_priority":    20,
	"boot_autostart_delay":       15,
	"boot_host_shutdown_timeout": 30,
}

// instanceAdvancedConfigConfigKeys mirrors `configKeys` (camelCase -> Incus key).
var instanceAdvancedConfigConfigKeys = []struct {
	Name   string
	Target string
}{
	{"limitsProcesses", "limits.processes"},
	{"limitsCpuPriority", "limits.cpu.priority"},
	{"bootAutostart", "boot.autostart"},
	{"bootAutostartPriority", "boot.autostart.priority"},
	{"bootAutostartDelay", "boot.autostart.delay"},
	{"bootHostShutdownTimeout", "boot.host_shutdown_timeout"},
}

// instanceAdvancedConfigFields list order mirrors the schema's property order, which
// is the order AJV reports the first validation failure in.
var instanceAdvancedConfigSchema = []ajvFieldSpec{
	{Name: "limitsRead", Kind: ajvKindNullableString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "limitsWrite", Kind: ajvKindNullableString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "limitsReadIops", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 2147483647},
	{Name: "limitsWriteIops", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 2147483647},
	{Name: "limitsIngress", Kind: ajvKindNullableString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "limitsEgress", Kind: ajvKindNullableString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "limitsProcesses", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 2147483647},
	{Name: "limitsCpuPriority", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 10},
	{Name: "bootAutostart", Kind: ajvKindNullableBoolean, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "bootAutostartPriority", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 100},
	{Name: "bootAutostartDelay", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 600},
	{Name: "bootHostShutdownTimeout", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 600},
}

// ajvKindNullableBoolean is the [boolean,null] multi-type.
const ajvKindNullableBoolean ajvFieldKind = 100

// patternMatcher wraps a compiled schema pattern.
type patternMatcher struct {
	re *regexp.Regexp
}

// MatchString reports whether the pattern matches.
func (m *patternMatcher) MatchString(value string) bool { return m.re.MatchString(value) }

// FindStringSubmatch exposes the capture groups of the compiled pattern.
func (m *patternMatcher) FindStringSubmatch(value string) []string {
	return m.re.FindStringSubmatch(value)
}

// mustPattern compiles a schema pattern. A malformed constant is a programming
// error, so panic at init rather than silently accepting/rejecting every value.
func mustPattern(pattern string) *patternMatcher {
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return &patternMatcher{re: compiled}
}

var (
	instanceAdvancedConfigDiskRegex    = mustPattern(instanceAdvancedConfigDiskPatternPattern)
	instanceAdvancedConfigNetworkRegex = mustPattern(instanceAdvancedConfigNetworkPatternPattern)
	instanceAdvancedConfigSpeedRegex   = mustPattern(`(?i)^(\d+(?:\.\d+)?)\s*(Mbit|Gbit)$`)
	instanceAdvancedConfigDigitsRegex  = mustPattern(`^\d+$`)
)

// instanceAdvancedConfigCoerceField applies the AJV semantics for the advanced
// schema's property set, including the [string,null] / [integer,null] /
// [boolean,null] coercion chains and the pattern check.
func instanceAdvancedConfigCoerceField(field ajvFieldSpec, value any) (any, string) {
	switch field.Name {
	case "limitsRead", "limitsWrite":
		coerced, message := instanceAdvancedConfigCoerceNullableString(value)
		if message != "" {
			return nil, message
		}
		if text, ok := coerced.(string); ok && !instanceAdvancedConfigDiskRegex.MatchString(text) {
			// app.ts maps a `pattern` failure to "<field> format is invalid"; AJV's raw
			// text never reaches the client (verified against the live Node server).
			return nil, ajvPatternMessage(field.Name)
		}
		return coerced, ""
	case "limitsIngress", "limitsEgress":
		coerced, message := instanceAdvancedConfigCoerceNullableString(value)
		if message != "" {
			return nil, message
		}
		if text, ok := coerced.(string); ok && !instanceAdvancedConfigNetworkRegex.MatchString(text) {
			return nil, ajvPatternMessage(field.Name)
		}
		return coerced, ""
	case "bootAutostart":
		return instanceAdvancedConfigCoerceNullableBoolean(value)
	default:
		switch field.Kind {
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
		}
	}
	return nil, "must be valid"
}

// instanceAdvancedConfigCoerceNullableString mirrors AJV's [string,null] coercion.
func instanceAdvancedConfigCoerceNullableString(value any) (any, string) {
	switch typed := value.(type) {
	case string:
		return typed, ""
	case nil:
		return nil, ""
	case float64:
		return formatTerminalCommandNumber(typed), ""
	case bool:
		return strconv.FormatBool(typed), ""
	case []any:
		if len(typed) != 1 {
			return nil, "must be string,null"
		}
		// Multi-type schemas never accept the unwrapped element directly (AJV
		// generates `typeof x === "string" && x === null`), so it falls into the
		// coercion chain instead of being unwrapped.
		switch element := typed[0].(type) {
		case nil:
			return "", ""
		case float64:
			return formatTerminalCommandNumber(element), ""
		case bool:
			return strconv.FormatBool(element), ""
		}
		return nil, "must be string,null"
	default:
		return nil, "must be string,null"
	}
}

// instanceAdvancedConfigCoerceNullableBoolean mirrors AJV's [boolean,null] chain.
func instanceAdvancedConfigCoerceNullableBoolean(value any) (any, string) {
	if flag, ok := value.(bool); ok {
		return flag, ""
	}
	if value == nil {
		return nil, ""
	}
	source, _ := ajvUnwrapSingleArray(value)
	switch typed := source.(type) {
	case string:
		if typed == "false" {
			return false, ""
		}
		if typed == "true" {
			return true, ""
		}
		if typed == "" {
			return nil, ""
		}
	case float64:
		if typed == 0 {
			return false, ""
		}
		if typed == 1 {
			return true, ""
		}
	case bool:
		// `data === "" || data === 0 || data === false` → null; `true` matches no
		// branch and is a type error (AJV multi-type array-unwrap quirk).
		if !typed {
			return nil, ""
		}
	}
	return nil, "must be boolean,null"
}

// instanceAdvancedConfigValidateBody mirrors the route's body schema.
func instanceAdvancedConfigValidateBody(decoded any) (map[string]any, string) {
	object, ok := decoded.(map[string]any)
	if !ok {
		// allOf[0].not { required: [...] } fails for every non-object.
		return nil, "must NOT be valid"
	}
	if _, hasRead := object["limitsRead"]; hasRead {
		if _, hasIops := object["limitsReadIops"]; hasIops {
			return nil, "must NOT be valid"
		}
	}
	if _, hasWrite := object["limitsWrite"]; hasWrite {
		if _, hasIops := object["limitsWriteIops"]; hasIops {
			return nil, "must NOT be valid"
		}
	}
	if len(object) == 0 {
		return nil, "must NOT have fewer than 1 properties"
	}

	values := make(map[string]any, len(instanceAdvancedConfigSchema))
	for _, field := range instanceAdvancedConfigSchema {
		value, present := object[field.Name]
		if !present {
			continue
		}
		coerced, message := instanceAdvancedConfigCoerceField(field, value)
		if message != "" {
			return nil, message
		}
		values[field.Name] = coerced
	}
	return values, ""
}

// ==================== traffic bandwidth (services/traffic-bandwidth.ts) ====================

const (
	instanceAdvancedConfigDiskPatternPattern    = `^([0-9]+(\.[0-9]+)?)(B|kB|MB|GB|TB|KiB|MiB|GiB|TiB)$`
	instanceAdvancedConfigNetworkPatternPattern = `^([1-9][0-9]*)(Mbit|Gbit|Mbps|Gbps)$`
)

// instanceAdvancedConfigNormalizePlanTrafficLimitSpeed mirrors
// normalizePlanTrafficLimitSpeed. A nil result covers both null and undefined; every
// call site treats them identically (`if (x)` / `??`).
func instanceAdvancedConfigNormalizePlanTrafficLimitSpeed(value any) *string {
	if value == nil {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return nil
	}
	if text == "" {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || trimmed == "0" {
		return nil
	}

	if match := instanceAdvancedConfigSpeedRegex.FindStringSubmatch(trimmed); match != nil {
		numeric, err := strconv.ParseFloat(match[1], 64)
		if err != nil || math.IsNaN(numeric) || math.IsInf(numeric, 0) || numeric <= 0 {
			return nil
		}
		multiplier := 1.0
		if strings.ToLower(match[2]) == "gbit" {
			multiplier = 1000
		}
		mbps := int64(math.Round(numeric * multiplier))
		if mbps <= 0 {
			return nil
		}
		formatted := strconv.FormatInt(mbps, 10) + "Mbit"
		return &formatted
	}

	if isAllDigitsPattern(trimmed) {
		bytes, ok := new(big.Int).SetString(trimmed, 10)
		if !ok {
			return nil
		}
		mbps := new(big.Int).Quo(bytes, big.NewInt(instanceAdvancedConfigMBBytes))
		if mbps.Sign() <= 0 {
			return nil
		}
		formatted := mbps.String() + "Mbit"
		return &formatted
	}
	return nil
}

// isAllDigitsPattern mirrors `/^\d+$/.test(value)`.
func isAllDigitsPattern(value string) bool {
	return instanceAdvancedConfigDigitsRegex.MatchString(value)
}

// instanceAdvancedConfigBandwidthMbps mirrors bandwidthMbps.
func instanceAdvancedConfigBandwidthMbps(value *string) (int64, bool) {
	normalized := instanceAdvancedConfigNormalizePlanTrafficLimitSpeed(stringValueOrNil(value))
	if normalized == nil {
		return 0, false
	}
	digits := strings.TrimSuffix(*normalized, "Mbit")
	parsed, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func stringValueOrNil(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// instanceAdvancedConfigMostRestrictive mirrors mostRestrictiveBandwidth.
func instanceAdvancedConfigMostRestrictive(values ...*string) *string {
	minimum := int64(0)
	found := false
	for _, value := range values {
		if mbps, ok := instanceAdvancedConfigBandwidthMbps(value); ok {
			if !found || mbps < minimum {
				minimum = mbps
				found = true
			}
		}
	}
	if !found {
		return nil
	}
	formatted := strconv.FormatInt(minimum, 10) + "Mbit"
	return &formatted
}

// instanceAdvancedConfigEffectiveBandwidth mirrors EffectiveBandwidth.
type instanceAdvancedConfigEffectiveBandwidth struct {
	Ingress *string
	Egress  *string
}

// instanceAdvancedConfigBandwidthSource mirrors the arbitration source object.
type instanceAdvancedConfigBandwidthSource struct {
	ConfiguredLimitsIngress *string
	ConfiguredLimitsEgress  *string
	TrafficStatus           string
	PackageLimitsIngress    *string
	PackageLimitsEgress     *string
	PackagePlanSpeed        *string
}

// instanceAdvancedConfigComputeEffectiveBandwidth mirrors computeEffectiveBandwidth:
// normal line speed is read from the plan only, never from the current limits.
func instanceAdvancedConfigComputeEffectiveBandwidth(source instanceAdvancedConfigBandwidthSource, overageThrottleSpeed string) instanceAdvancedConfigEffectiveBandwidth {
	normalLineSpeed := instanceAdvancedConfigNormalizePlanTrafficLimitSpeed(stringValueOrNil(source.PackagePlanSpeed))
	baselineIngress := firstNonNull(source.ConfiguredLimitsIngress, normalLineSpeed, source.PackageLimitsIngress)
	baselineEgress := firstNonNull(source.ConfiguredLimitsEgress, normalLineSpeed, source.PackageLimitsEgress)

	var trafficConstraint *string
	if source.TrafficStatus == "LIMITED" {
		trafficConstraint = &overageThrottleSpeed
	}

	return instanceAdvancedConfigEffectiveBandwidth{
		Ingress: instanceAdvancedConfigMostRestrictive(baselineIngress, trafficConstraint),
		Egress:  instanceAdvancedConfigMostRestrictive(baselineEgress, trafficConstraint),
	}
}

func firstNonNull(values ...*string) *string {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// ==================== Incus applyAdvancedConfig ====================

// instanceAdvancedConfigDevices is the validated `devices` shape (each device is a
// flat map of strings).
type instanceAdvancedConfigDevices map[string]map[string]string

// instanceAdvancedConfigValidDevices mirrors validDevices(value).
func instanceAdvancedConfigValidDevices(value any) (instanceAdvancedConfigDevices, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	devices := make(instanceAdvancedConfigDevices, len(object))
	for name, raw := range object {
		device, ok := raw.(map[string]any)
		if !ok {
			return nil, false
		}
		flat := make(map[string]string, len(device))
		for key, entry := range device {
			text, ok := entry.(string)
			if !ok {
				return nil, false
			}
			flat[key] = text
		}
		devices[name] = flat
	}
	return devices, true
}

// instanceAdvancedConfigSortedNames returns map keys in a deterministic order.
// JavaScript object insertion order is not representable after JSON decoding into a
// Go map; the only order-sensitive lookup is "the first eth0 candidate", and Incus
// device names are unique, so a sorted walk is behaviourally identical.
func instanceAdvancedConfigSortedNames(devices instanceAdvancedConfigDevices) []string {
	names := make([]string, 0, len(devices))
	for name := range devices {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// instanceAdvancedConfigStringJS mirrors String(value) for the values the schema and
// package defaults can carry.
func instanceAdvancedConfigStringJS(value any) string {
	switch typed := value.(type) {
	case nil:
		return "undefined"
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return formatTerminalCommandNumber(typed)
	case float32:
		return formatTerminalCommandNumber(float64(typed))
	case int:
		return strconv.FormatInt(int64(typed), 10)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return fmt.Sprint(value)
	}
}

// instanceAdvancedConfigApplyAdvancedConfig mirrors applyAdvancedConfig. Incus 7.0
// LTS PATCH merges configuration keys and whole named device definitions.
func instanceAdvancedConfigApplyAdvancedConfig(
	ctx context.Context,
	client *lib.IncusClient,
	name string,
	changes map[string]any,
	defaults map[string]any,
) error {
	path := "/1.0/instances/" + url.PathEscape(name)
	raw, err := client.Request(ctx, http.MethodGet, path, nil, instanceAdvancedConfigIncusTimeout, nil)
	if err != nil {
		return err
	}

	var current map[string]any
	if err := json.Unmarshal(raw, &current); err != nil {
		return errors.New("Invalid Incus instance response")
	}
	instanceType, _ := current["type"].(string)
	if instanceType != "container" && instanceType != "virtual-machine" {
		return errors.New("Invalid Incus instance response")
	}
	devices, devicesOK := instanceAdvancedConfigValidDevices(current["devices"])
	expanded, expandedOK := instanceAdvancedConfigValidDevices(current["expanded_devices"])
	if !devicesOK || !expandedOK {
		return errors.New("Invalid Incus instance response")
	}
	if _, present := changes["limitsProcesses"]; present && instanceType == "virtual-machine" {
		return errors.New("Process limits require a container")
	}

	// value(key) = changes[key] ?? defaults[column] ?? advancedDefaults[column]
	value := func(key string) any {
		if candidate, present := changes[key]; present && candidate != nil {
			return candidate
		}
		column := instanceAdvancedConfigFields[key]
		if candidate, present := defaults[column]; present && candidate != nil {
			return candidate
		}
		return instanceAdvancedConfigDefaults[column]
	}

	config := map[string]string{}
	for _, entry := range instanceAdvancedConfigConfigKeys {
		if _, present := changes[entry.Name]; present {
			config[entry.Target] = instanceAdvancedConfigStringJS(value(entry.Name))
		}
	}

	updatedDevices := instanceAdvancedConfigDevices{}
	device := func(kind string) (map[string]string, error) {
		candidates := make([]string, 0, len(expanded))
		for _, name := range instanceAdvancedConfigSortedNames(expanded) {
			candidate := expanded[name]
			if candidate["type"] != kind {
				continue
			}
			if kind == "disk" && candidate["path"] != "/" {
				continue
			}
			candidates = append(candidates, name)
		}

		selected := ""
		if kind == "nic" {
			for _, name := range candidates {
				if name == "eth0" {
					selected = name
					break
				}
			}
			if selected == "" {
				for _, name := range candidates {
					if expanded[name]["name"] == "eth0" {
						selected = name
						break
					}
				}
			}
			if selected == "" && len(candidates) == 1 {
				selected = candidates[0]
			}
		} else if len(candidates) == 1 {
			selected = candidates[0]
		}
		if selected == "" {
			return nil, errors.New("Cannot identify instance " + kind + " device")
		}

		if existing, ok := updatedDevices[selected]; ok {
			return existing, nil
		}
		merged := make(map[string]string, len(expanded[selected])+len(devices[selected]))
		for key, entry := range expanded[selected] {
			merged[key] = entry
		}
		for key, entry := range devices[selected] {
			merged[key] = entry
		}
		updatedDevices[selected] = merged
		return merged, nil
	}

	for _, pair := range [][3]string{{"limitsRead", "limitsReadIops", "limits.read"}, {"limitsWrite", "limitsWriteIops", "limits.write"}} {
		throughput, iops, target := pair[0], pair[1], pair[2]
		_, hasThroughput := changes[throughput]
		_, hasIops := changes[iops]
		if !hasThroughput && !hasIops {
			continue
		}
		if hasThroughput && hasIops {
			return errors.New("Choose throughput or IOPS, not both")
		}
		key := throughput
		if !hasThroughput {
			key = iops
		}
		target_device, err := device("disk")
		if err != nil {
			return err
		}
		suffix := ""
		if key == iops {
			suffix = "iops"
		}
		target_device[target] = instanceAdvancedConfigStringJS(value(key)) + suffix
	}

	for _, pair := range [][2]string{{"limitsIngress", "limits.ingress"}, {"limitsEgress", "limits.egress"}} {
		key, target := pair[0], pair[1]
		if _, present := changes[key]; !present {
			continue
		}
		target_device, err := device("nic")
		if err != nil {
			return err
		}
		normalized := instanceAdvancedConfigStringJS(value(key))
		normalized = instanceAdvancedConfigReplaceSpeedSuffix(normalized)
		target_device[target] = normalized
	}

	body := map[string]any{"config": config}
	if len(updatedDevices) > 0 {
		body["devices"] = updatedDevices
	}
	_, err = client.Request(ctx, http.MethodPatch, path, body, instanceAdvancedConfigIncusTimeout, body)
	return err
}

// instanceAdvancedConfigReplaceSpeedSuffix mirrors the three /Mbps$/i style
// replacements applied to ingress/egress values.
func instanceAdvancedConfigReplaceSpeedSuffix(value string) string {
	for _, pair := range [][2]string{{"Mbps", "Mbit"}, {"Gbps", "Gbit"}, {"Kbps", "kbit"}} {
		if len(value) >= len(pair[0]) && strings.EqualFold(value[len(value)-len(pair[0]):], pair[0]) {
			value = value[:len(value)-len(pair[0])] + pair[1]
		}
	}
	return value
}

// ==================== DB projections ====================

// instanceAdvancedConfigInstance mirrors the getInstanceById fields the route reads.
type instanceAdvancedConfigInstance struct {
	ID        int32
	HostID    int32
	PackageID *int32
	IncusID   string
	Name      string
	Status    string
}

// instanceAdvancedConfigHost mirrors the getHostById fields the route reads.
type instanceAdvancedConfigHost struct {
	ID            int32
	UserID        int32
	URL           string
	CertPath      *string
	KeyPath       *string
	TunnelEnabled bool
	TargetHost    string
	TargetPort    int32
}

func instanceAdvancedConfigGetInstance(ctx context.Context, id int32) (*instanceAdvancedConfigInstance, error) {
	var row struct {
		ID        int32  `gorm:"column:id"`
		HostID    int32  `gorm:"column:host_id"`
		PackageID *int32 `gorm:"column:package_id"`
		IncusID   string `gorm:"column:incus_id"`
		Name      string `gorm:"column:name"`
		Status    string `gorm:"column:status"`
	}
	err := db.DB.WithContext(ctx).Raw(
		"SELECT id, host_id, package_id, incus_id, name, status FROM instances WHERE id = ? LIMIT 1", id,
	).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == 0 {
		return nil, nil
	}
	return &instanceAdvancedConfigInstance{
		ID: row.ID, HostID: row.HostID, PackageID: row.PackageID,
		IncusID: row.IncusID, Name: row.Name, Status: row.Status,
	}, nil
}

func instanceAdvancedConfigGetHost(ctx context.Context, id int32) (*instanceAdvancedConfigHost, error) {
	var row struct {
		ID            int32   `gorm:"column:id"`
		UserID        int32   `gorm:"column:user_id"`
		URL           string  `gorm:"column:url"`
		CertPath      *string `gorm:"column:cert_path"`
		KeyPath       *string `gorm:"column:key_path"`
		TunnelEnabled bool    `gorm:"column:tunnel_enabled"`
		TargetHost    string  `gorm:"column:target_host"`
		TargetPort    int32   `gorm:"column:target_port"`
	}
	err := db.DB.WithContext(ctx).Raw(
		`SELECT id, user_id, url, cert_path, key_path, tunnel_enabled, target_host, target_port
		 FROM hosts WHERE id = ? LIMIT 1`, id,
	).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == 0 {
		return nil, nil
	}
	return &instanceAdvancedConfigHost{
		ID: row.ID, UserID: row.UserID, URL: row.URL,
		CertPath: row.CertPath, KeyPath: row.KeyPath,
		TunnelEnabled: row.TunnelEnabled, TargetHost: row.TargetHost, TargetPort: row.TargetPort,
	}, nil
}

// instanceAdvancedConfigGetPackageDefaults mirrors `{...getPackageById(id)}` — the
// snake_case projection applyAdvancedConfig reads as defaults.
func instanceAdvancedConfigGetPackageDefaults(ctx context.Context, id int32) (map[string]any, error) {
	var row struct {
		ID                      int32   `gorm:"column:id"`
		LimitsRead              *string `gorm:"column:limits_read"`
		LimitsWrite             *string `gorm:"column:limits_write"`
		LimitsReadIops          *int32  `gorm:"column:limits_read_iops"`
		LimitsWriteIops         *int32  `gorm:"column:limits_write_iops"`
		LimitsIngress           *string `gorm:"column:limits_ingress"`
		LimitsEgress            *string `gorm:"column:limits_egress"`
		LimitsProcesses         *int32  `gorm:"column:limits_processes"`
		LimitsCPUPriority       *int32  `gorm:"column:limits_cpu_priority"`
		BootAutostart           *bool   `gorm:"column:boot_autostart"`
		BootAutostartPriority   *int32  `gorm:"column:boot_autostart_priority"`
		BootAutostartDelay      *int32  `gorm:"column:boot_autostart_delay"`
		BootHostShutdownTimeout *int32  `gorm:"column:boot_host_shutdown_timeout"`
	}
	err := db.DB.WithContext(ctx).Raw(
		`SELECT id, limits_read, limits_write, limits_read_iops, limits_write_iops,
		        limits_ingress, limits_egress, limits_processes, limits_cpu_priority,
		        boot_autostart, boot_autostart_priority, boot_autostart_delay, boot_host_shutdown_timeout
		 FROM packages WHERE id = ? LIMIT 1`, id,
	).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == 0 {
		return nil, nil
	}

	// getPackageById emits the values as-is; nullable columns stay null so `??` in
	// value(key) falls through to advancedDefaults.
	defaults := map[string]any{}
	put := func(key string, value any) { defaults[key] = value }
	put("limits_read", nullableStringValue(row.LimitsRead))
	put("limits_write", nullableStringValue(row.LimitsWrite))
	put("limits_read_iops", nullableInt32Value(row.LimitsReadIops))
	put("limits_write_iops", nullableInt32Value(row.LimitsWriteIops))
	put("limits_ingress", nullableStringValue(row.LimitsIngress))
	put("limits_egress", nullableStringValue(row.LimitsEgress))
	put("limits_processes", nullableInt32Value(row.LimitsProcesses))
	put("limits_cpu_priority", nullableInt32Value(row.LimitsCPUPriority))
	put("boot_autostart", nullableBoolValue(row.BootAutostart))
	put("boot_autostart_priority", nullableInt32Value(row.BootAutostartPriority))
	put("boot_autostart_delay", nullableInt32Value(row.BootAutostartDelay))
	put("boot_host_shutdown_timeout", nullableInt32Value(row.BootHostShutdownTimeout))
	return defaults, nil
}

func nullableStringValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt32Value(value *int32) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableBoolValue(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}

// instanceAdvancedConfigGetBandwidthState mirrors deps.getBandwidthState.
func instanceAdvancedConfigGetBandwidthState(ctx context.Context, id int32) (*instanceAdvancedConfigBandwidthSource, bool, error) {
	var row struct {
		ID                      int32   `gorm:"column:id"`
		ConfiguredLimitsIngress *string `gorm:"column:configured_limits_ingress"`
		ConfiguredLimitsEgress  *string `gorm:"column:configured_limits_egress"`
		TrafficStatus           string  `gorm:"column:traffic_status"`
		PackageLimitsIngress    *string `gorm:"column:package_limits_ingress"`
		PackageLimitsEgress     *string `gorm:"column:package_limits_egress"`
		PlanTrafficLimitSpeed   *string `gorm:"column:plan_traffic_limit_speed"`
	}
	err := db.DB.WithContext(ctx).Raw(
		`SELECT i.id,
		        i.configured_limits_ingress, i.configured_limits_egress, i.traffic_status,
		        p.limits_ingress AS package_limits_ingress,
		        p.limits_egress AS package_limits_egress,
		        pp.traffic_limit_speed AS plan_traffic_limit_speed
		 FROM instances i
		 LEFT JOIN packages p ON p.id = i.package_id
		 LEFT JOIN package_plans pp ON pp.id = i.package_plan_id
		 WHERE i.id = ? LIMIT 1`, id,
	).Scan(&row).Error
	if err != nil {
		return nil, false, err
	}
	if row.ID == 0 {
		return nil, false, nil
	}
	return &instanceAdvancedConfigBandwidthSource{
		ConfiguredLimitsIngress: row.ConfiguredLimitsIngress,
		ConfiguredLimitsEgress:  row.ConfiguredLimitsEgress,
		TrafficStatus:           row.TrafficStatus,
		PackageLimitsIngress:    row.PackageLimitsIngress,
		PackageLimitsEgress:     row.PackageLimitsEgress,
		PackagePlanSpeed:        row.PlanTrafficLimitSpeed,
	}, true, nil
}

// instanceAdvancedConfigUpdateInstanceConfig mirrors db.updateInstanceConfig,
// including the empty-update early return (it must not touch updated_at or report a
// 404 for a missing row when there is nothing to write).
func instanceAdvancedConfigUpdateInstanceConfig(
	ctx context.Context,
	instanceID int32,
	changes map[string]any,
	effective instanceAdvancedConfigEffectiveBandwidth,
) error {
	updates := map[string]any{}
	integerColumns := map[string]bool{
		"limits_read_iops": true, "limits_write_iops": true,
		"limits_processes": true, "limits_cpu_priority": true,
		"boot_autostart_priority": true, "boot_autostart_delay": true,
		"boot_host_shutdown_timeout": true,
	}
	assign := func(changeKey, column string) {
		value, present := changes[changeKey]
		if !present {
			return
		}
		// AJV-coerced integers are float64; Prisma would bind them as Int.
		if number, ok := value.(float64); ok && integerColumns[column] {
			updates[column] = int32(number)
			return
		}
		updates[column] = value
	}
	assign("limitsRead", "limits_read")
	assign("limitsWrite", "limits_write")
	assign("limitsReadIops", "limits_read_iops")
	assign("limitsWriteIops", "limits_write_iops")
	assign("limitsIngress", "configured_limits_ingress")
	assign("limitsEgress", "configured_limits_egress")
	assign("limitsProcesses", "limits_processes")
	assign("limitsCpuPriority", "limits_cpu_priority")
	assign("bootAutostart", "boot_autostart")
	assign("bootAutostartPriority", "boot_autostart_priority")
	assign("bootAutostartDelay", "boot_autostart_delay")
	assign("bootHostShutdownTimeout", "boot_host_shutdown_timeout")

	if effective.Ingress != nil {
		updates["limits_ingress"] = *effective.Ingress
	}
	if effective.Egress != nil {
		updates["limits_egress"] = *effective.Egress
	}

	if len(updates) == 0 {
		return nil
	}
	// Model(&models.Instance{}) also bumps updated_at, exactly like Prisma's
	// `@updatedAt` on update.
	return db.DB.WithContext(ctx).Model(&models.Instance{}).Where("id = ?", instanceID).Updates(updates).Error
}

// ==================== route ====================

// RegisterInstanceAdvancedConfig mounts PATCH /api/instances/:id/advanced-config.
func RegisterInstanceAdvancedConfig(r *gin.Engine) {
	group := r.Group("/api")
	group.PATCH("/instances/:id/advanced-config", middleware.Authenticate(), func(c *gin.Context) {
		ctx := c.Request.Context()

		body, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			middleware.AbortFastifyBody(c, status, message)
			return
		}
		changes, validationMessage := instanceAdvancedConfigValidateBody(body.Value)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}
		// removeAdditional already stripped unknown keys; an object that only carried
		// unknown keys reaches this guard.
		if len(changes) == 0 {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "")
			return
		}

		instanceID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		if instanceID > math.MaxInt32 {
			// The id parses but cannot address a Prisma Int column (P2020). The Node
			// lookup sits outside the route's try block, so it surfaces as a 500.
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		id := int32(instanceID)

		instance, err := instanceAdvancedConfigGetInstance(ctx, id)
		if err != nil {
			lib.RespondAPIError(c, http.StatusBadGateway, lib.ErrConfigUpdateFailed, "")
			return
		}
		if instance == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
			return
		}
		host, err := instanceAdvancedConfigGetHost(ctx, instance.HostID)
		if err != nil {
			lib.RespondAPIError(c, http.StatusBadGateway, lib.ErrConfigUpdateFailed, "")
			return
		}
		if host == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
			return
		}

		user := middleware.CurrentUser(c)
		if user.Role != "admin" && host.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}
		if instance.Status != "running" && instance.Status != "stopped" {
			lib.RespondAPIError(c, http.StatusConflict, lib.ErrInstanceStatusInvalid, "")
			return
		}
		if !instanceAdvancedConfigTryAcquire(id) {
			c.JSON(http.StatusConflict, gin.H{"error": "Configuration update in progress"})
			return
		}
		defer instanceAdvancedConfigRelease(id)

		if InstanceAdvancedConfigTransferLock(c, id) || InstanceAdvancedConfigConflictChecker(c, id) {
			return
		}

		apply := func() error {
			inherited := map[string]any{}
			if instance.PackageID != nil {
				defaults, err := instanceAdvancedConfigGetPackageDefaults(ctx, *instance.PackageID)
				if err != nil {
					return err
				}
				if defaults != nil {
					inherited = defaults
				}
			}

			var effective instanceAdvancedConfigEffectiveBandwidth
			_, hasIngress := changes["limitsIngress"]
			_, hasEgress := changes["limitsEgress"]
			if hasIngress || hasEgress {
				state, found, err := instanceAdvancedConfigGetBandwidthState(ctx, id)
				if err != nil {
					return err
				}
				if !found {
					return errors.New("Instance unavailable")
				}

				for _, key := range []string{"limitsIngress", "limitsEgress"} {
					if text, ok := changes[key].(string); ok {
						changes[key] = instanceAdvancedConfigReplaceSpeedSuffix(text)
					}
				}

				if normalSpeed := instanceAdvancedConfigNormalizePlanTrafficLimitSpeed(stringValueOrNil(state.PackagePlanSpeed)); normalSpeed != nil {
					inherited["limits_ingress"] = *normalSpeed
					inherited["limits_egress"] = *normalSpeed
				}

				overageRaw, err := db.GetSystemConfig(ctx, instanceAdvancedConfigOverageThrottleConfigKey, true)
				if err != nil {
					return err
				}
				overage := instanceAdvancedConfigDefaultOverageThrottleSpeed
				if normalized := instanceAdvancedConfigNormalizePlanTrafficLimitSpeed(stringValueOrNil(overageRaw)); normalized != nil {
					overage = *normalized
				}

				configuredIngress := state.ConfiguredLimitsIngress
				if value, present := changes["limitsIngress"]; present {
					configuredIngress = anyToStringPointer(value)
				}
				configuredEgress := state.ConfiguredLimitsEgress
				if value, present := changes["limitsEgress"]; present {
					configuredEgress = anyToStringPointer(value)
				}

				source := instanceAdvancedConfigBandwidthSource{
					ConfiguredLimitsIngress: configuredIngress,
					ConfiguredLimitsEgress:  configuredEgress,
					TrafficStatus:           state.TrafficStatus,
					PackageLimitsIngress:    state.PackageLimitsIngress,
					PackageLimitsEgress:     state.PackageLimitsEgress,
					PackagePlanSpeed:        state.PackagePlanSpeed,
				}
				limits := instanceAdvancedConfigComputeEffectiveBandwidth(source, overage)

				if _, present := changes["limitsIngress"]; present {
					selected := limits.Ingress
					if selected == nil {
						fallback := "300Mbit"
						if inheritedIngress, ok := inherited["limits_ingress"]; ok && inheritedIngress != nil {
							fallback = instanceAdvancedConfigStringJS(inheritedIngress)
						}
						selected = &fallback
					}
					effective.Ingress = selected
				}
				if _, present := changes["limitsEgress"]; present {
					selected := limits.Egress
					if selected == nil {
						fallback := "300Mbit"
						if inheritedEgress, ok := inherited["limits_egress"]; ok && inheritedEgress != nil {
							fallback = instanceAdvancedConfigStringJS(inheritedEgress)
						}
						selected = &fallback
					}
					effective.Egress = selected
				}
			}

			client, err := lib.GetIncusClient(ctx, lib.IncusClientOptions{
				URL:           host.URL,
				CertPath:      host.CertPath,
				KeyPath:       host.KeyPath,
				TunnelEnabled: host.TunnelEnabled,
				HostID:        int64(host.ID),
				TargetHost:    host.TargetHost,
				TargetPort:    int(host.TargetPort),
			})
			if err != nil {
				return err
			}

			incusChanges := make(map[string]any, len(changes)+2)
			for key, value := range changes {
				incusChanges[key] = value
			}
			if effective.Ingress != nil {
				incusChanges["limitsIngress"] = *effective.Ingress
			}
			if effective.Egress != nil {
				incusChanges["limitsEgress"] = *effective.Egress
			}
			if err := instanceAdvancedConfigApplyAdvancedConfig(ctx, client, instance.IncusID, incusChanges, inherited); err != nil {
				return err
			}
			return instanceAdvancedConfigUpdateInstanceConfig(ctx, id, changes, effective)
		}

		_, hasIngress := changes["limitsIngress"]
		_, hasEgress := changes["limitsEgress"]
		if hasIngress || hasEgress {
			result := lib.WithLock(ctx, "bandwidth:instance:"+strconv.FormatInt(int64(id), 10), func() (struct{}, error) {
				return struct{}{}, apply()
			}, lib.LockOptions{
				ExpireMs:      intPointer(instanceAdvancedConfigLockExpireMs),
				WaitTimeoutMs: intPointer(instanceAdvancedConfigLockWaitTimeoutMs),
			})
			if !result.Success {
				lib.RespondAPIError(c, http.StatusBadGateway, lib.ErrConfigUpdateFailed, "")
				return
			}
		} else if err := apply(); err != nil {
			lib.RespondAPIError(c, http.StatusBadGateway, lib.ErrConfigUpdateFailed, "")
			return
		}

		userID := int64(user.ID)
		db.CreateLog(ctx, &userID, instanceAdvancedConfigLogModule, "instance.update_config",
			"Updated advanced configuration for instance \""+instance.Name+"\"",
			db.LogResultSuccess, db.LogOptionsWithInstance(&id))
		c.JSON(http.StatusOK, gin.H{"message": "Instance configuration applied", "applied": true})
	})
}

func intPointer(value int) *int { return &value }

// anyToStringPointer mirrors `changes[key] as string | null`.
func anyToStringPointer(value any) *string {
	if value == nil {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return nil
	}
	return &text
}

func init() {
	Register(RegisterInstanceAdvancedConfig)
}
