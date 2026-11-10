// Port of server/src/lib/incus/incus-traffic.ts.
//
// Traffic metering and bandwidth throttling. Two rules from that file are
// load-bearing for billing and are reproduced exactly:
//
//  1. Only Incudal's own NICs are billable: eth0 (IPv4/NAT) and eth1 (routed
//     IPv6). Guest-created bridges, veth pairs, docker/tunnel interfaces reflect
//     the same packets again and would inflate quota usage; they are skipped. For
//     VMs the guest interface name is distro dependent (enp5s0, ...), so the
//     deterministic MACs we assign when creating the NICs are matched instead.
//     When no strict NIC is present, a fallback pass counts interfaces that look
//     like external guest NICs (ethN / enoX / enpX / ensX / enxX).
//  2. Counters are bigint in the TypeScript. A JSON *string* is accepted only when
//     it is all digits; a JSON *number* must be finite and positive and is
//     floored. Anything else is 0 — never an error, because one bad counter must
//     not stop metering. The raw literal is kept (json.RawMessage) so a quoted
//     "12.5" is still rejected like the TS rejects it.
//
// Nothing here mutates money. ApplyThrottle/RestoreBandwidth only rewrite the
// limits.* keys on `type=nic` devices, leaving every other device attribute in
// place (dropping them would detach the storage pool or change the NIC type).
package lib

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	// trafficEthPattern mirrors /^eth[0-9]+$/
	trafficEthPattern = regexp.MustCompile(`^eth[0-9]+$`)
	// trafficEnPattern mirrors /^en(?:o|p|s|x)[a-z0-9]+$/
	trafficEnPattern = regexp.MustCompile(`^en(?:o|p|s|x)[a-z0-9]+$`)
	// trafficDigitsPattern mirrors /^\d+$/
	trafficDigitsPattern = regexp.MustCompile(`^[0-9]+$`)
)

// NetworkCounters mirrors NetworkCounters.
type NetworkCounters struct {
	RxBytes     int64
	TxBytes     int64
	RxPackets   int64
	TxPackets   int64
	CPUUsageRaw *int64
	SampledAt   time.Time
}

// InstanceStateNetwork mirrors InstanceStateNetwork.
type InstanceStateNetwork struct {
	Addresses []InstanceStateAddress `json:"addresses"`
	Hwaddr    *string                `json:"hwaddr"`
	Counters  *InstanceStateCounters `json:"counters"`
	State     *string                `json:"state"`
}

// InstanceStateAddress mirrors one entry of `addresses`.
type InstanceStateAddress struct {
	Family  string `json:"family"`
	Address string `json:"address"`
	Scope   string `json:"scope"`
}

// InstanceStateCounters mirrors the raw counters object. The counters arrive as
// either a JSON number or a numeric string depending on the Incus version, so the
// literal is kept and normalised by trafficCounterToInt64.
type InstanceStateCounters struct {
	BytesReceived   json.RawMessage `json:"bytes_received"`
	BytesSent       json.RawMessage `json:"bytes_sent"`
	PacketsReceived json.RawMessage `json:"packets_received"`
	PacketsSent     json.RawMessage `json:"packets_sent"`
}

// InstanceTrafficState mirrors InstanceTrafficState.
type InstanceTrafficState struct {
	Status  *string                         `json:"status"`
	Network map[string]InstanceStateNetwork `json:"network"`
	CPU     *InstanceStateCPU               `json:"cpu"`
}

// InstanceStateCPU mirrors `cpu: { usage }`. `usage` is a number or a numeric
// string in the Incus API.
type InstanceStateCPU struct {
	Usage json.RawMessage `json:"usage"`
}

// ThrottleBandwidth mirrors THROTTLE_BANDWIDTH.
const ThrottleBandwidth = "1Mbit"

// GetTrafficCounters mirrors getTrafficCounters: read the instance state and sum
// the billable NIC counters.
func GetTrafficCounters(ctx context.Context, client *IncusClient, instanceName string) (NetworkCounters, error) {
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/instances/"+instanceName+"/state", nil, 0, nil)
	if err != nil {
		return NetworkCounters{}, err
	}

	var state InstanceTrafficState
	if err := json.Unmarshal(raw, &state); err != nil {
		return NetworkCounters{}, NewIncusAPIError("节点返回的实例状态格式无效", 502, nil)
	}
	return GetTrafficCountersFromState(instanceName, state), nil
}

// GetTrafficCountersFromState mirrors getTrafficCountersFromState.
func GetTrafficCountersFromState(instanceName string, state InstanceTrafficState) NetworkCounters {
	billableVMmacs := trafficBillableVMMacs(instanceName)

	totals := NetworkCounters{SampledAt: time.Now().UTC()}
	if state.CPU != nil {
		// `counterToBigInt(state.cpu?.usage) || null` — zero becomes null.
		if usage := trafficCounterToInt64(state.CPU.Usage); usage > 0 {
			value := usage
			totals.CPUUsageRaw = &value
		}
	}

	type fallbackInterface struct {
		name string
		data InstanceStateNetwork
	}
	fallbackInterfaces := make([]fallbackInterface, 0, 2)
	hasStrictBillableInterface := false

	for ifName, ifData := range state.Network {
		if trafficIsBillableNetworkInterface(ifName, ifData, billableVMmacs) {
			hasStrictBillableInterface = true
			trafficAddCounters(&totals, ifData.Counters)
			continue
		}

		if trafficIsLikelyExternalGuestInterface(ifName) {
			fallbackInterfaces = append(fallbackInterfaces, fallbackInterface{name: ifName, data: ifData})
			continue
		}
	}

	// A single eth0/eth1 (or a known VM MAC) is authoritative; the fallback exists
	// only for guests whose NIC is named something else entirely.
	if !hasStrictBillableInterface && len(fallbackInterfaces) > 0 {
		for _, item := range fallbackInterfaces {
			trafficAddCounters(&totals, item.data.Counters)
		}
	}

	return totals
}

// trafficBillableVMMacs mirrors getBillableVmMacs.
func trafficBillableVMMacs(instanceName string) map[string]struct{} {
	macs := VMNetworkGenerateMacs(instanceName)
	return map[string]struct{}{
		strings.ToLower(macs.Eth0): {},
		strings.ToLower(macs.Eth1): {},
	}
}

// trafficNormalizeMac mirrors normalizeMac.
func trafficNormalizeMac(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	lowered := strings.ToLower(trimmed)
	return &lowered
}

// trafficIsBillableNetworkInterface mirrors isBillableNetworkInterface.
func trafficIsBillableNetworkInterface(ifName string, ifData InstanceStateNetwork, billableVMmacs map[string]struct{}) bool {
	if ifName == "lo" {
		return false
	}

	// Incudal-created LXC NICs are eth0 (IPv4/NAT) and optionally eth1 (routed IPv6).
	if ifName == "eth0" || ifName == "eth1" {
		return true
	}

	hwaddr := trafficNormalizeMac(ifData.Hwaddr)
	if hwaddr == nil {
		return false
	}
	_, ok := billableVMmacs[*hwaddr]
	return ok
}

// trafficIsLikelyExternalGuestInterface mirrors isLikelyExternalGuestInterface.
func trafficIsLikelyExternalGuestInterface(ifName string) bool {
	normalized := strings.ToLower(ifName)
	return trafficEthPattern.MatchString(normalized) || trafficEnPattern.MatchString(normalized)
}

// trafficCounterToInt64 mirrors counterToBigInt.
//
// A quoted value must be all digits; an unquoted one must be a finite number > 0
// and is floored. Values beyond int64 saturate instead of wrapping (the TS uses
// BigInt and has no such limit; saturating keeps the counter monotone).
func trafficCounterToInt64(raw json.RawMessage) int64 {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return 0
	}

	if text[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return 0
		}
		value = strings.TrimSpace(value)
		if !trafficDigitsPattern.MatchString(value) {
			return 0
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return math.MaxInt64
		}
		return parsed
	}

	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
		return 0
	}
	floored := math.Floor(parsed)
	if floored >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(floored)
}

// trafficAddCounters mirrors addCounters.
func trafficAddCounters(totals *NetworkCounters, counters *InstanceStateCounters) {
	if counters == nil {
		return
	}
	totals.RxBytes = trafficSaturatingAdd(totals.RxBytes, trafficCounterToInt64(counters.BytesReceived))
	totals.TxBytes = trafficSaturatingAdd(totals.TxBytes, trafficCounterToInt64(counters.BytesSent))
	totals.RxPackets = trafficSaturatingAdd(totals.RxPackets, trafficCounterToInt64(counters.PacketsReceived))
	totals.TxPackets = trafficSaturatingAdd(totals.TxPackets, trafficCounterToInt64(counters.PacketsSent))
}

// trafficSaturatingAdd keeps a hostile counter from wrapping the sum negative.
func trafficSaturatingAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

// ApplyThrottle mirrors applyThrottle: pin every NIC to 1Mbit.
func ApplyThrottle(ctx context.Context, client *IncusClient, instanceName string) error {
	devices, err := trafficLoadDevices(ctx, client, instanceName)
	if err != nil {
		return err
	}

	for deviceName, device := range devices {
		if trafficDeviceType(device) != "nic" {
			continue
		}
		updated := trafficCopyDevice(device)
		updated["limits.ingress"] = ThrottleBandwidth
		updated["limits.egress"] = ThrottleBandwidth
		devices[deviceName] = updated
	}

	_, err = UpdateInstance(ctx, client, instanceName, map[string]any{"devices": devices})
	return err
}

// RestoreBandwidth mirrors restoreBandwidth: put the plan limits back, or delete
// the keys when the plan has no limit (a leftover 1Mbit throttle would otherwise
// permanently cripple the instance).
func RestoreBandwidth(ctx context.Context, client *IncusClient, instanceName string, limitsIngress, limitsEgress *string) error {
	devices, err := trafficLoadDevices(ctx, client, instanceName)
	if err != nil {
		return err
	}

	for deviceName, device := range devices {
		if trafficDeviceType(device) != "nic" {
			continue
		}
		updated := trafficCopyDevice(device)

		if limitsIngress != nil && *limitsIngress != "" {
			updated["limits.ingress"] = *limitsIngress
		} else {
			delete(updated, "limits.ingress")
		}

		if limitsEgress != nil && *limitsEgress != "" {
			updated["limits.egress"] = *limitsEgress
		} else {
			delete(updated, "limits.egress")
		}

		devices[deviceName] = updated
	}

	_, err = UpdateInstance(ctx, client, instanceName, map[string]any{"devices": devices})
	return err
}

// IsThrottled mirrors isThrottled.
func IsThrottled(ctx context.Context, client *IncusClient, instanceName string) (bool, error) {
	instance, err := GetInstance(ctx, client, instanceName)
	if err != nil {
		return false, err
	}
	for _, device := range instance.Devices {
		if trafficDeviceType(device) != "nic" {
			continue
		}
		if ingress, ok := device["limits.ingress"].(string); ok && ingress == ThrottleBandwidth {
			return true, nil
		}
	}
	return false, nil
}

// trafficLoadDevices reads the instance and normalises a nil device map to an
// empty one (`instance.devices || {}`).
func trafficLoadDevices(ctx context.Context, client *IncusClient, instanceName string) (map[string]map[string]any, error) {
	instance, err := GetInstance(ctx, client, instanceName)
	if err != nil {
		return nil, err
	}
	if instance.Devices == nil {
		return map[string]map[string]any{}, nil
	}
	return instance.Devices, nil
}

// trafficCopyDevice mirrors the `{ ...device }` spread.
func trafficCopyDevice(device map[string]any) map[string]any {
	updated := make(map[string]any, len(device)+2)
	for key, value := range device {
		updated[key] = value
	}
	return updated
}

// trafficDeviceType mirrors `device.type === 'nic'` for a decoded device map.
func trafficDeviceType(device map[string]any) string {
	value, ok := device["type"]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}
