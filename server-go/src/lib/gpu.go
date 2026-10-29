// Port of server/src/lib/gpu.ts.
//
// GPUs are shared per physical device: NVIDIA does not partition VRAM or compute,
// so the policy is a hard admission rule rather than a hint. Two invariants are
// security-relevant and are why this is a separate module:
//
//  1. Shared GPU is only allowed on non-privileged LXC containers. A VM or a
//     privileged container with a device passthrough would escape the isolation
//     the sharing model assumes, so applyGpuDevice rejects both.
//  2. A PCI address must be a single well-formed device address. Duplicates are
//     rejected too, because listing the same GPU twice would let a caller claim
//     more of it than the host has.
//
// The default profile list is cleared on purpose: inheriting a profile could
// expose every GPU on the host instead of only the selected ones.
package lib

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// GpuReportMaxAge mirrors GPU_REPORT_MAX_AGE_MS.
const GpuReportMaxAge = 180 * time.Second

// GpuSharingNotice mirrors GPU_SHARING_NOTICE.
const GpuSharingNotice = "共享物理 GPU；显存和算力由同池实例共同使用，不提供独立显存或算力保证。"

// gpuPCIAddressPattern mirrors /^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$/.
var gpuPCIAddressPattern = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

// GpuError mirrors the TypeScript GpuError (statusCode 400, code
// GPU_CONFIGURATION_INVALID).
type GpuError struct {
	Message string
}

// Error implements error.
func (e *GpuError) Error() string { return e.Message }

// StatusCode mirrors the class field.
func (e *GpuError) StatusCode() int { return 400 }

// Code mirrors the class field.
func (e *GpuError) Code() string { return "GPU_CONFIGURATION_INVALID" }

// GpuPolicy mirrors the GpuPolicy interface.
type GpuPolicy struct {
	GpuShared    bool
	GpuDeviceIDs []int32
}

// GpuPlanPolicy mirrors the nullable plan override passed to ResolveGpuPolicy.
type GpuPlanPolicy struct {
	GpuShared    *bool
	GpuDeviceIDs []int32
}

// GpuDevice mirrors one reported device.
type GpuDevice struct {
	Name           string
	MemoryTotalMiB int64
}

// GpuModelSummary mirrors GpuModelSummary.
type GpuModelSummary struct {
	Name           string `json:"name"`
	MemoryTotalMiB int64  `json:"memoryTotalMiB"`
	Count          int    `json:"count"`
}

// GpuConfiguration mirrors GpuConfiguration.
type GpuConfiguration struct {
	HostIDs []int64           `json:"hostIds"`
	Devices []GpuModelSummary `json:"devices"`
	Count   int               `json:"count"`
}

// GpuHostDevice is one device together with the host it belongs to, used by
// SummarizeGpuConfigurations.
type GpuHostDevice struct {
	HostID int64
	GpuDevice
}

// NormalizeGpuInput mirrors normalizeGpuInput. nullable mirrors the second
// parameter: when true an explicit null is accepted for gpuShared.
//
// The tri-state is preserved: a field absent from the result vs present-but-null
// are different outcomes, so both are returned explicitly.
func NormalizeGpuInput(input map[string]any, nullable bool) (shared *bool, sharedPresent bool, deviceIDs []int32, deviceIDsPresent bool, err error) {
	if raw, present := input["gpuShared"]; present {
		sharedPresent = true
		switch value := raw.(type) {
		case bool:
			shared = &value
		case nil:
			if !nullable {
				return nil, true, nil, false, &GpuError{Message: "GPU sharing must be a boolean"}
			}
			shared = nil
		default:
			return nil, true, nil, false, &GpuError{Message: "GPU sharing must be a boolean"}
		}
	}

	raw, present := input["gpuDeviceIDs"]
	if !present {
		return shared, sharedPresent, nil, false, nil
	}
	deviceIDsPresent = true

	array, ok := raw.([]any)
	if !ok || len(array) > 1000 {
		return nil, sharedPresent, nil, true, &GpuError{Message: "Invalid GPU device selection"}
	}

	// `[...new Set(ids)]` in the TypeScript: de-duplicate while preserving order.
	seen := make(map[int32]bool, len(array))
	deviceIDs = make([]int32, 0, len(array))
	for _, entry := range array {
		number, ok := entry.(float64)
		if !ok {
			return nil, sharedPresent, nil, true, &GpuError{Message: "Invalid GPU device selection"}
		}
		// Number.isSafeInteger(id) && id > 0 && id <= 2147483647
		if number != float64(int64(number)) || number <= 0 || number > 2147483647 {
			return nil, sharedPresent, nil, true, &GpuError{Message: "Invalid GPU device selection"}
		}
		id := int32(number)
		if seen[id] {
			continue
		}
		seen[id] = true
		deviceIDs = append(deviceIDs, id)
	}

	return shared, sharedPresent, deviceIDs, true, nil
}

// ResolveGpuPolicy mirrors resolveGpuPolicy: a plan override wins only when it
// explicitly sets gpuShared (not null); otherwise the package policy applies.
func ResolveGpuPolicy(pkg GpuPolicy, plan *GpuPlanPolicy) GpuPolicy {
	if plan != nil && plan.GpuShared != nil {
		return GpuPolicy{GpuShared: *plan.GpuShared, GpuDeviceIDs: plan.GpuDeviceIDs}
	}
	return pkg
}

// IsGpuFresh mirrors isGpuFresh. The upper bound guards against a clock skew
// where the node reports a timestamp in the future.
func IsGpuFresh(available, runtimeReady bool, lastSeenAt time.Time, now time.Time) bool {
	return available && runtimeReady &&
		now.Sub(lastSeenAt) <= GpuReportMaxAge &&
		lastSeenAt.Before(now.Add(30*time.Second))
}

// SummarizeGpuModels mirrors summarizeGpuModels: group identical (name, memory)
// devices, order by name then memory.
func SummarizeGpuModels(devices []GpuDevice) []GpuModelSummary {
	type key struct {
		name   string
		memory int64
	}
	order := make([]key, 0, len(devices))
	groups := make(map[key]*GpuModelSummary, len(devices))

	for _, device := range devices {
		composite := key{name: device.Name, memory: device.MemoryTotalMiB}
		if group, ok := groups[composite]; ok {
			group.Count++
			continue
		}
		summary := GpuModelSummary{Name: device.Name, MemoryTotalMiB: device.MemoryTotalMiB, Count: 1}
		groups[composite] = &summary
		order = append(order, composite)
	}

	result := make([]GpuModelSummary, 0, len(order))
	for _, composite := range order {
		result = append(result, *groups[composite])
	}

	// The TS comparator is `a.name.localeCompare(b.name) || a.memory - b.memory`.
	sort.SliceStable(result, func(i, j int) bool {
		byName := localeCompare(result[i].Name, result[j].Name)
		if byName != 0 {
			return byName < 0
		}
		return result[i].MemoryTotalMiB < result[j].MemoryTotalMiB
	})
	return result
}

// SummarizeGpuConfigurations mirrors summarizeGpuConfigurations: each host
// provides its whole bundle, so equal bundles across hosts are scheduler
// alternatives rather than additive GPUs.
func SummarizeGpuConfigurations(devices []GpuHostDevice) []GpuConfiguration {
	hostOrder := make([]int64, 0, len(devices))
	byHost := make(map[int64][]GpuDevice)
	for _, device := range devices {
		if _, seen := byHost[device.HostID]; !seen {
			hostOrder = append(hostOrder, device.HostID)
		}
		byHost[device.HostID] = append(byHost[device.HostID], device.GpuDevice)
	}

	order := make([]string, 0, len(hostOrder))
	groups := make(map[string]*GpuConfiguration, len(hostOrder))

	for _, hostID := range hostOrder {
		cards := byHost[hostID]
		models := SummarizeGpuModels(cards)
		// The TS keys on JSON.stringify(models), so a deterministic serialisation
		// is enough; the exact byte form never reaches a client.
		serialized := gpuBundleKey(models)
		if group, ok := groups[serialized]; ok {
			group.HostIDs = append(group.HostIDs, hostID)
			continue
		}
		configuration := GpuConfiguration{HostIDs: []int64{hostID}, Devices: models, Count: len(cards)}
		groups[serialized] = &configuration
		order = append(order, serialized)
	}

	result := make([]GpuConfiguration, 0, len(order))
	for _, composite := range order {
		result = append(result, *groups[composite])
	}
	return result
}

// gpuBundleKey builds the equivalent of JSON.stringify([]GpuModelSummary).
func gpuBundleKey(models []GpuModelSummary) string {
	var builder strings.Builder
	builder.WriteByte('[')
	for index, model := range models {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(model.Name)
		builder.WriteByte('|')
		builder.WriteString(itoa64(model.MemoryTotalMiB))
		builder.WriteByte('|')
		builder.WriteString(itoa64(int64(model.Count)))
	}
	builder.WriteByte(']')
	return builder.String()
}

// ApplyGpuDevice mirrors applyGpuDevice: reject privileged/VM targets, validate
// the PCI addresses, clear the profile list and rewrite the gpu devices.
func ApplyGpuDevice(config map[string]any, pci any) error {
	var addresses []string
	switch value := pci.(type) {
	case string:
		addresses = []string{value}
	case []string:
		addresses = value
	case []any:
		addresses = make([]string, 0, len(value))
		for _, entry := range value {
			text, ok := entry.(string)
			if !ok {
				return &GpuError{Message: "Invalid GPU PCI address"}
			}
			addresses = append(addresses, text)
		}
	default:
		return &GpuError{Message: "Invalid GPU PCI address"}
	}

	if configType, _ := config["type"].(string); configType == "virtual-machine" {
		return &GpuError{Message: "共享 GPU 仅支持非特权 LXC 容器"}
	}
	if settings, ok := config["config"].(map[string]any); ok {
		if privileged, _ := settings["security.privileged"].(string); privileged == "true" {
			return &GpuError{Message: "共享 GPU 仅支持非特权 LXC 容器"}
		}
	}

	if len(addresses) == 0 {
		return &GpuError{Message: "Invalid GPU PCI address"}
	}
	seen := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		if seen[address] {
			return &GpuError{Message: "Invalid GPU PCI address"}
		}
		seen[address] = true
		if !gpuPCIAddressPattern.MatchString(address) {
			return &GpuError{Message: "Invalid GPU PCI address"}
		}
	}

	// Do not inherit a default profile that might expose every GPU on the host.
	config["profiles"] = []any{}

	settings, _ := config["config"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
	}
	settings["nvidia.runtime"] = "true"
	settings["nvidia.driver.capabilities"] = "compute,utility"
	config["config"] = settings

	devices, _ := config["devices"].(map[string]any)
	if devices == nil {
		devices = map[string]any{}
	}
	for name, device := range devices {
		if entry, ok := device.(map[string]any); ok {
			if deviceType, _ := entry["type"].(string); deviceType == "gpu" {
				delete(devices, name)
			}
		}
	}
	for index, address := range addresses {
		devices["gpu"+itoa64(int64(index))] = map[string]any{
			"type":    "gpu",
			"gputype": "physical",
			"pci":     address,
		}
	}
	config["devices"] = devices
	return nil
}

// localeCompare approximates String.prototype.localeCompare for the ASCII device
// names NVIDIA reports. A full ICU collation is not worth pulling in: the only
// consumer sorts model names, and ICU and byte order agree for that alphabet.
func localeCompare(left, right string) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

// itoa64 formats an int64 without importing strconv into every call site.
func itoa64(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}
