// Port of the slice of server/src/lib/network-payload-builder.ts that
// server/src/routes/ip-addresses.ts actually uses:
//
//   - syncInstanceNetwork   (the whole eth1/cloud-init rewrite + optional restart)
//   - isValidIpv6Subnet     (the loose CIDR check the subnet route runs first)
//
// syncInstanceNetwork pulls in three more helpers, ported here because the
// instance wave has not landed them yet:
//
//   - generateCloudInitNetworkConfig  (network-config v2 YAML)
//   - generateVmNicMacs               (deterministic per-instance NIC MACs)
//   - restartInstance                 (PUT /1.0/instances/<name>/state)
//
// Deliberately NOT ported (nothing in this module calls them):
// buildNetworkDevices, removeInstanceIpv6, hasIpv6Support, generateCloudInitNetworkConfigLegacy,
// and isValidIpv6 (only reachable through isValidIpv6Subnet).
//
// The cloud-init YAML is reproduced byte for byte, including the quirk that the
// eth1 block is emitted whenever the *unfiltered* ipv6List is non-empty, even if
// every entry is blank (the TS builds the address list from the filtered slice).
package lib

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// NetworkPayloadIPv6Config mirrors IPv6Config. Subnet models `subnet?: string`
// (nil means "no subnet" — the route omits the field rather than sending null).
type NetworkPayloadIPv6Config struct {
	PrimaryIP string
	ExtraIPs  []string
	Subnet    *string
}

// networkPayloadNatIpv4Fallback mirrors the `|| '10.10.1.1'` fallback for a
// missing eth0 ipv4.address.
const networkPayloadNatIpv4Fallback = "10.10.1.1"

// networkPayloadCloudInitNameserver mirrors the hard-coded 10.10.0.1 in the
// network-config template.
const networkPayloadCloudInitNameserver = "10.10.0.1"

// networkPayloadIPv6Pattern mirrors /^([0-9a-fA-F]{0,4}:){2,7}[0-9a-fA-F]{0,4}$/.
var networkPayloadIPv6Pattern = regexp.MustCompile(`^([0-9a-fA-F]{0,4}:){2,7}[0-9a-fA-F]{0,4}$`)

// networkPayloadIncusInstance is the GET /1.0/instances/<name> projection the
// TypeScript casts to: config, devices, profiles and description.
//
// lib.GetInstance's projection has no Description field, and this PUT is a full
// replacement, so a separate decode is required — sending a body without
// `description` would silently erase it on the node.
type networkPayloadIncusInstance struct {
	Config      map[string]string         `json:"config"`
	Devices     map[string]map[string]any `json:"devices"`
	Profiles    []string                  `json:"profiles"`
	Description string                    `json:"description"`
}

// networkPayloadFetchInstance mirrors getInstance(client, name) as used here.
func networkPayloadFetchInstance(ctx context.Context, client *IncusClient, instanceName string) (*networkPayloadIncusInstance, error) {
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/instances/"+instanceName, nil,
		incusInstanceOperationTimeout, nil)
	if err != nil {
		return nil, err
	}
	var instance networkPayloadIncusInstance
	if err := json.Unmarshal(raw, &instance); err != nil {
		return nil, NewIncusAPIError("节点返回的实例数据格式无效", 502, nil)
	}
	return &instance, nil
}

// NetworkPayloadSyncInstanceNetwork mirrors syncInstanceNetwork.
//
// It rewrites eth1's ipv6.address/ipv6.routes, regenerates the cloud-init
// network-config and PUTs the whole instance back (Incus PUT is a full
// replacement), then restarts the instance when restart is true. A restart
// failure is only logged, exactly like the TypeScript's try/catch.
func NetworkPayloadSyncInstanceNetwork(
	ctx context.Context,
	client *IncusClient,
	instanceName string,
	ipv6Config NetworkPayloadIPv6Config,
	hostInterface string,
	restart bool,
) error {
	current, err := networkPayloadFetchInstance(ctx, client, instanceName)
	if err != nil {
		return err
	}

	// `current.devices?.eth0?.['ipv4.address'] || '10.10.1.1'`
	currentIpv4 := networkPayloadNatIpv4Fallback
	if eth0, ok := current.Devices["eth0"]; ok {
		if address, ok := eth0["ipv4.address"].(string); ok && address != "" {
			currentIpv4 = address
		}
	}

	allIPs := make([]string, 0, 1+len(ipv6Config.ExtraIPs))
	allIPs = append(allIPs, ipv6Config.PrimaryIP)
	if len(ipv6Config.ExtraIPs) > 0 {
		allIPs = append(allIPs, ipv6Config.ExtraIPs...)
	}
	joinedAddresses := strings.Join(allIPs, ",")

	devices := make(map[string]map[string]any, len(current.Devices)+1)
	for name, device := range current.Devices {
		devices[name] = device
	}

	if existing, ok := devices["eth1"]; ok {
		updated := make(map[string]any, len(existing)+2)
		for key, value := range existing {
			updated[key] = value
		}
		updated["ipv6.address"] = joinedAddresses
		if ipv6Config.Subnet != nil {
			updated["ipv6.routes"] = *ipv6Config.Subnet
		} else {
			delete(updated, "ipv6.routes")
		}
		devices["eth1"] = updated
	} else {
		_, generatedEth1 := networkPayloadGenerateVmNicMacs(instanceName)
		eth1 := map[string]any{
			"type":         "nic",
			"nictype":      "routed",
			"parent":       hostInterface,
			"name":         "eth1",
			"hwaddr":       generatedEth1,
			"ipv6.address": joinedAddresses,
		}
		// security.ipv6_filtering / security.mac_filtering are bridged-only; routed
		// devices must not carry them (the TypeScript omits them entirely).
		if ipv6Config.Subnet != nil {
			eth1["ipv6.routes"] = *ipv6Config.Subnet
		}
		devices["eth1"] = eth1
	}

	config := make(map[string]string, len(current.Config)+1)
	for key, value := range current.Config {
		config[key] = value
	}
	config["user.network-config"] = networkPayloadGenerateCloudInitNetworkConfig(currentIpv4, allIPs)

	body := map[string]any{
		"config":      config,
		"devices":     devices,
		"profiles":    current.Profiles,
		"description": current.Description,
	}
	if _, err := client.Request(ctx, http.MethodPut, "/1.0/instances/"+instanceName, body,
		incusInstanceOperationTimeout, body); err != nil {
		return err
	}

	if restart {
		// A failed restart must not fail the sync: the config is already applied and
		// the TypeScript only warns.
		_ = networkPayloadRestartInstance(ctx, client, instanceName, false)
	}
	return nil
}

// networkPayloadRestartInstance mirrors restartInstance(client, name, force).
func networkPayloadRestartInstance(ctx context.Context, client *IncusClient, name string, force bool) error {
	body := map[string]any{
		"action":  "restart",
		"timeout": 60,
		"force":   force,
	}
	_, err := client.Request(ctx, http.MethodPut, "/1.0/instances/"+name+"/state", body,
		incusInstanceOperationTimeout, body)
	return err
}

// networkPayloadGenerateCloudInitNetworkConfig mirrors
// generateCloudInitNetworkConfig: network-config v2 YAML with a static eth0 IPv4
// (DHCPv6/RA disabled so the guest cannot pick up a second NAT IPv6) and an
// optional static eth1 IPv6 block.
func networkPayloadGenerateCloudInitNetworkConfig(ipv4Address string, ipv6List []string) string {
	config := `version: 2
ethernets:
  eth0:
    dhcp4: false
    dhcp6: false
    accept-ra: false
    addresses:
      - ` + ipv4Address + `/22
    routes:
      - to: 0.0.0.0/0
        via: ` + NatIPv4Gateway + `
    nameservers:
      addresses:
        - ` + networkPayloadCloudInitNameserver + `
`

	// Note: the condition uses the raw list length, not the filtered one — a list
	// of blank strings still emits an empty eth1 block, matching the TypeScript.
	if len(ipv6List) > 0 {
		formatted := make([]string, 0, len(ipv6List))
		for _, address := range ipv6List {
			trimmed := strings.TrimSpace(address)
			if trimmed == "" {
				continue
			}
			if strings.ContainsRune(trimmed, '/') {
				formatted = append(formatted, trimmed)
			} else {
				formatted = append(formatted, trimmed+"/128")
			}
		}
		addressLines := make([]string, 0, len(formatted))
		for _, address := range formatted {
			addressLines = append(addressLines, "      - "+address)
		}

		config += `  eth1:
    dhcp4: false
    dhcp6: false
    accept-ra: false
    addresses:
` + strings.Join(addressLines, "\n") + `
    gateway6: fe80::1
    nameservers:
      addresses:
        - ` + networkPayloadCloudInitNameserver + `
`
	}

	return config
}

// networkPayloadGenerateVmNicMacs mirrors generateVmNicMacs(seed).
func networkPayloadGenerateVmNicMacs(seed string) (eth0 string, eth1 string) {
	return networkPayloadGenerateVmNicMac(seed, "eth0"), networkPayloadGenerateVmNicMac(seed, "eth1")
}

// networkPayloadGenerateVmNicMac mirrors generateVmNicMac: sha256 over
// `incudal-vm-nic:<seed>:<nic>`, prefixed with the locally-administered 0x02.
func networkPayloadGenerateVmNicMac(seed, nicLabel string) string {
	sum := sha256.Sum256([]byte("incudal-vm-nic:" + seed + ":" + nicLabel))
	octets := []byte{0x02, sum[0], sum[1], sum[2], sum[3], sum[4]}
	parts := make([]string, 0, len(octets))
	for _, octet := range octets {
		parts = append(parts, hex.EncodeToString([]byte{octet}))
	}
	return strings.Join(parts, ":")
}

// NetworkPayloadIsValidIpv6 mirrors isValidIpv6: a deliberately loose check (the
// subnet route runs it before the strict Address6 normalization).
func NetworkPayloadIsValidIpv6(address string) bool {
	trimmed := strings.Split(strings.TrimSpace(address), "/")[0]
	return networkPayloadIPv6Pattern.MatchString(trimmed) || trimmed == "::1" || trimmed == "::"
}

// NetworkPayloadIsValidIpv6Subnet mirrors isValidIpv6Subnet.
//
// Quirks preserved from the TypeScript:
//   - the prefix is parsed with parseInt, so "112abc" is accepted as 112;
//   - only 48..128 are allowed here (the route narrows it to 112/120/124 later);
//   - the address part goes through the loose isValidIpv6 regex, not Address6.
func NetworkPayloadIsValidIpv6Subnet(cidr string) bool {
	parts := strings.Split(cidr, "/")
	if len(parts) != 2 {
		return false
	}

	prefix, ok := networkPayloadParseIntDecimal(parts[1])
	if !ok || prefix < 48 || prefix > 128 {
		return false
	}
	return NetworkPayloadIsValidIpv6(parts[0])
}

// networkPayloadParseIntDecimal mirrors `parseInt(value, 10)`: skip leading
// whitespace, accept an optional sign, then consume leading decimal digits and
// stop at the first non-digit. Returns ok=false for NaN.
func networkPayloadParseIntDecimal(value string) (float64, bool) {
	trimmed := strings.TrimLeft(value, " \t\n\r\v\f")
	sign := 1.0
	if strings.HasPrefix(trimmed, "+") {
		trimmed = trimmed[1:]
	} else if strings.HasPrefix(trimmed, "-") {
		sign = -1
		trimmed = trimmed[1:]
	}

	end := 0
	for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	digits := trimmed[:end]

	// Accumulate like a double so enormous inputs saturate to +Inf the way
	// JavaScript's parseInt does.
	result := 0.0
	for index := 0; index < len(digits); index++ {
		result = result*10 + float64(digits[index]-'0')
	}
	return sign * result, true
}

// init installs the canonical NAT IPv4 pool into the db layer. db must not import
// lib (lib imports db), so db keeps a fallback copy of the pool bounds and this
// registration makes lib.ListNatIpv4Pool the single source of truth at runtime —
// the same registration-hook pattern db/logs.go uses for log localization.
func init() {
	db.IpAddressesNatIpv4PoolSource = ListNatIpv4Pool
}
