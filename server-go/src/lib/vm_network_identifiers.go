// Port of server/src/lib/vm-network-identifiers.ts.
//
// The MAC addresses are deterministic on purpose: they are written into the
// instance's Incus devices (eth1 routed) and into cloud-init's network-config
// match rules, so a rebuild that derives a different MAC would leave the guest
// with no address at all. The derivation is `sha256("incudal-vm-nic:<seed>:<nic>")`
// with the first five digest bytes appended to the locally-administered prefix
// 0x02 — byte-identical to the TypeScript.
//
// Naming note: package lib is flat, so the symbols carry the VMNetwork prefix.
// lib/network_payload_builder.go already had a private copy of this derivation
// (networkPayloadGenerateVmNicMac) because it was ported before this module;
// lib/vm_network_identifiers_test.go pins the two together so they cannot drift.
package lib

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// VMNetworkNicMacs mirrors VmNicMacs.
type VMNetworkNicMacs struct {
	// Eth0 is the bridged primary NIC (NAT IPv4).
	Eth0 string
	// Eth1 is the routed IPv6 NIC (only present when the instance is dual-stack).
	Eth1 string
}

// VMNetworkNicLabels are the only two labels the TypeScript accepts.
const (
	VMNetworkNicLabelEth0 = "eth0"
	VMNetworkNicLabelEth1 = "eth1"
)

// vmNetworkFormatMac mirrors formatMac: two lowercase hex digits per byte joined
// by ':'.
func vmNetworkFormatMac(bytes []byte) string {
	parts := make([]string, 0, len(bytes))
	for _, octet := range bytes {
		parts = append(parts, hex.EncodeToString([]byte{octet}))
	}
	return strings.Join(parts, ":")
}

// VMNetworkGenerateMac mirrors generateVmNicMac(seed, nicLabel).
//
// nicLabel is only ever "eth0"/"eth1" in the TypeScript; the string is used
// verbatim in the hash input, so an unexpected label still yields a stable (if
// unused) address rather than a panic.
func VMNetworkGenerateMac(seed, nicLabel string) string {
	sum := sha256.Sum256([]byte("incudal-vm-nic:" + seed + ":" + nicLabel))
	return vmNetworkFormatMac([]byte{0x02, sum[0], sum[1], sum[2], sum[3], sum[4]})
}

// VMNetworkGenerateMacs mirrors generateVmNicMacs(seed).
func VMNetworkGenerateMacs(seed string) VMNetworkNicMacs {
	return VMNetworkNicMacs{
		Eth0: VMNetworkGenerateMac(seed, VMNetworkNicLabelEth0),
		Eth1: VMNetworkGenerateMac(seed, VMNetworkNicLabelEth1),
	}
}
