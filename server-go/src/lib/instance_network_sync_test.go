package lib

import (
	"encoding/json"
	"testing"
)

func TestResolveInstanceNetworkAddresses(t *testing.T) {
	stateRaw := json.RawMessage(`{
		"status": "Running",
		"network": {
			"lo": {"addresses": [{"family": "inet6", "address": "::1", "scope": "host"}]},
			"eth0": {"addresses": [{"family": "inet", "address": "10.0.0.8", "scope": "global"}]},
			"eth1": {"addresses": [{"family": "inet6", "address": "fe80::1", "scope": "link"}, {"family": "inet6", "address": "2001:db8::99", "scope": "global"}]}
		}
	}`)
	instanceRaw := json.RawMessage(`{
		"expanded_devices": {
			"eth1": {"type": "nic", "name": "eth1", "ipv6.address": "2001:db8::10/64"}
		}
	}`)

	// Routed IPv6 mode prefers configured ipv6.address over observed address.
	oldV4 := "10.0.0.2"
	res := ResolveInstanceNetworkAddresses(InstanceNetworkSyncTarget{
		ID:          1,
		NetworkMode: "nat_ipv6",
		IPv4:        &oldV4,
	}, instanceRaw, stateRaw)

	if res.IPv4 == nil || *res.IPv4 != "10.0.0.8" {
		t.Fatalf("IPv4 = %v, want 10.0.0.8", res.IPv4)
	}
	if res.IPv6 == nil || *res.IPv6 != "2001:db8::10" || res.IPv6Device != "eth1" {
		t.Fatalf("IPv6 = %v device=%q", res.IPv6, res.IPv6Device)
	}

	// Shared IPv6 mode uses observed IPv6 instead of configured routed address.
	resShared := ResolveInstanceNetworkAddresses(InstanceNetworkSyncTarget{
		ID:          2,
		NetworkMode: "nat_ipv6_nat",
	}, instanceRaw, stateRaw)
	if resShared.IPv6 == nil || *resShared.IPv6 != "2001:db8::99" {
		t.Fatalf("shared IPv6 = %v, want 2001:db8::99", resShared.IPv6)
	}
}
