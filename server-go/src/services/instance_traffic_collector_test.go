// Unit tests for the instance traffic collector port
// (server/src/services/instance-traffic-collector.ts and the counter reader it
// depends on, server/src/lib/incus/incus-traffic.ts).
//
// The tests pin the pure part — parsing Incus state into counters — because that is
// where unit/rounding/interface-selection bugs actually live. The DB write path
// (instance lock → user lock → snapshot baseline → increments → daily bucket) is
// exercised by the traffic module's conformance cases; the destroy flow only needs
// the counter value read right before deletion.
package services

import (
	"encoding/json"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// TestInstanceTrafficVmNicMacMatchesNode pins the deterministic VM NIC MAC
// derivation against values produced by the TypeScript implementation
// (lib/vm-network-identifiers.ts) — the lib helper is unexported, so this module
// carries its own copy and must stay byte-identical.
func TestInstanceTrafficVmNicMacMatchesNode(t *testing.T) {
	for _, tc := range []struct {
		seed  string
		label string
		want  string
	}{
		{"parity-billing-920001", "eth0", "02:87:04:0f:d5:bd"},
		{"parity-billing-920001", "eth1", "02:8d:21:8f:bf:b4"},
		{"test-seed", "eth0", "02:67:56:92:46:69"},
	} {
		if got := instanceTrafficVmNicMac(tc.seed, tc.label); got != tc.want {
			t.Errorf("mac(%q,%q) = %q, want %q", tc.seed, tc.label, got, tc.want)
		}
	}
}

// TestInstanceTrafficCountersFromState covers the billable-interface rules:
// eth0/eth1 always count, `lo` and container bridges never do, VM NIC MACs count,
// and the eth*/en* fallback only applies when no strict interface was reported.
func TestInstanceTrafficCountersFromState(t *testing.T) {
	state := func(network map[string]any, cpuUsage any) []byte {
		payload := map[string]any{"network": network}
		if cpuUsage != nil {
			payload["cpu"] = map[string]any{"usage": cpuUsage}
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return encoded
	}
	counters := func(rx, tx, rxp, txp any) map[string]any {
		return map[string]any{
			"counters": map[string]any{
				"bytes_received": rx, "bytes_sent": tx,
				"packets_received": rxp, "packets_sent": txp,
			},
		}
	}

	t.Run("sums-eth0-and-eth1", func(t *testing.T) {
		raw := state(map[string]any{
			"eth0": counters(float64(100), float64(200), float64(1), float64(2)),
			"eth1": counters(float64(50), float64(60), float64(3), float64(4)),
		}, float64(123456))
		got, err := InstanceTrafficCountersFromState("inst", raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got.RxBytes != 150 || got.TxBytes != 260 || got.RxPackets != 4 || got.TxPackets != 6 {
			t.Fatalf("counters = %+v, want rx=150 tx=260 rxp=4 txp=6", got)
		}
		if got.CPUUsageRaw == nil || *got.CPUUsageRaw != 123456 {
			t.Fatalf("cpu = %v, want 123456", got.CPUUsageRaw)
		}
	})

	t.Run("ignores-lo-and-bridges", func(t *testing.T) {
		raw := state(map[string]any{
			"eth0":    counters(float64(10), float64(20), float64(1), float64(2)),
			"lo":      counters(float64(999), float64(999), float64(9), float64(9)),
			"docker0": counters(float64(999), float64(999), float64(9), float64(9)),
			"veth1":   counters(float64(999), float64(999), float64(9), float64(9)),
		}, nil)
		got, err := InstanceTrafficCountersFromState("inst", raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got.RxBytes != 10 || got.TxBytes != 20 {
			t.Fatalf("counters = %+v, want the eth0 totals only", got)
		}
		if got.CPUUsageRaw != nil {
			t.Fatalf("cpu = %v, want nil when usage is absent", got.CPUUsageRaw)
		}
	})

	t.Run("zero-cpu-becomes-null", func(t *testing.T) {
		raw := state(map[string]any{}, float64(0))
		got, err := InstanceTrafficCountersFromState("inst", raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		// `counterToBigInt(state.cpu?.usage) || null`
		if got.CPUUsageRaw != nil {
			t.Fatalf("cpu = %v, want nil for usage 0", got.CPUUsageRaw)
		}
	})

	t.Run("counters-as-strings", func(t *testing.T) {
		raw := state(map[string]any{
			"eth0": counters("1000", "2000", "7", "8"),
		}, "42")
		got, err := InstanceTrafficCountersFromState("inst", raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got.RxBytes != 1000 || got.TxBytes != 2000 || got.RxPackets != 7 || got.TxPackets != 8 {
			t.Fatalf("counters = %+v, want string counters parsed", got)
		}
		if got.CPUUsageRaw == nil || *got.CPUUsageRaw != 42 {
			t.Fatalf("cpu = %v, want 42 from the string", got.CPUUsageRaw)
		}
	})

	t.Run("vm-mac-is-billable", func(t *testing.T) {
		mac := instanceTrafficVmNicMac("vm-instance", "eth0")
		raw := state(map[string]any{
			"enp5s0": map[string]any{
				"hwaddr":   mac,
				"counters": map[string]any{"bytes_received": float64(111), "bytes_sent": float64(222)},
			},
		}, nil)
		got, err := InstanceTrafficCountersFromState("vm-instance", raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got.RxBytes != 111 || got.TxBytes != 222 {
			t.Fatalf("counters = %+v, want the VM NIC counted once (strict MAC match)", got)
		}
	})

	t.Run("fallback-when-no-strict-interface", func(t *testing.T) {
		// No eth0/eth1 and no VM MAC: the eth*/en* fallback sums both.
		raw := state(map[string]any{
			"enp1s0": counters(float64(5), float64(6), float64(1), float64(1)),
			"eno2":   counters(float64(7), float64(8), float64(1), float64(1)),
			"lo":     counters(float64(100), float64(100), float64(1), float64(1)),
		}, nil)
		got, err := InstanceTrafficCountersFromState("inst", raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got.RxBytes != 12 || got.TxBytes != 14 {
			t.Fatalf("counters = %+v, want the fallback sum 12/14", got)
		}
	})

	t.Run("no-fallback-when-strict-interface-present", func(t *testing.T) {
		raw := state(map[string]any{
			"eth0":   counters(float64(1), float64(1), float64(1), float64(1)),
			"enp1s0": counters(float64(1000), float64(1000), float64(1), float64(1)),
		}, nil)
		got, err := InstanceTrafficCountersFromState("inst", raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got.RxBytes != 1 || got.TxBytes != 1 {
			t.Fatalf("counters = %+v, want only eth0 (fallback disabled)", got)
		}
	})

	t.Run("invalid-json", func(t *testing.T) {
		if _, err := InstanceTrafficCountersFromState("inst", []byte("not json")); err == nil {
			t.Fatal("expected an error for invalid JSON")
		}
	})
}

// TestInstanceTrafficCounterToInt pins counterToBigInt:
//   - a numeric value is floored and must be finite and > 0;
//   - a string must be all digits;
//   - overflow (beyond int64) is treated as a corrupt counter → 0.
func TestInstanceTrafficCounterToInt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  int64
	}{
		{"float", float64(3.9), 3},
		{"zero", float64(0), 0},
		{"negative", float64(-12), 0},
		{"string-digits", "123456789", 123456789},
		{"string-leading-zero", "007", 7},
		{"string-negative", "-5", 0},
		{"string-float", "1.5", 0},
		{"string-exponent", "1e3", 0},
		{"empty-string", "", 0},
		{"nil", nil, 0},
		{"bool", true, 0},
		{"int64", int64(55), 55},
		{"overflow-string", "999999999999999999999999", 0},
	} {
		if got := instanceTrafficCounterToInt(tc.value); got != tc.want {
			t.Errorf("%s: counterToInt(%v) = %d, want %d", tc.name, tc.value, got, tc.want)
		}
	}
}

// TestInstanceTrafficLockKeys pins the distributed-lock keys, which must match the
// TypeScript exactly or the immediate and scheduled collectors stop serialising.
func TestInstanceTrafficLockKeys(t *testing.T) {
	if got := InstanceTrafficLockKey(42); got != "traffic:instance:42" {
		t.Errorf("instance lock key = %q", got)
	}
	if got := InstanceTrafficUserLockKey(7); got != "traffic:user:7" {
		t.Errorf("user lock key = %q", got)
	}
}

// TestInstanceTrafficLockOptions pins the 30s/5s/50ms timings of
// DEFAULT_TRAFFIC_LOCK_OPTIONS and the per-call override behaviour.
func TestInstanceTrafficLockOptions(t *testing.T) {
	options := instanceTrafficLockOptions(lib.LockOptions{})
	if options.ExpireMs == nil || *options.ExpireMs != 30000 {
		t.Errorf("expireMs = %v, want 30000", options.ExpireMs)
	}
	if options.WaitTimeoutMs == nil || *options.WaitTimeoutMs != 5000 {
		t.Errorf("waitTimeoutMs = %v, want 5000", options.WaitTimeoutMs)
	}
	if options.RetryIntervalMs == nil || *options.RetryIntervalMs != 50 {
		t.Errorf("retryIntervalMs = %v, want 50", options.RetryIntervalMs)
	}

	expire := 1000
	overridden := instanceTrafficLockOptions(lib.LockOptions{ExpireMs: &expire})
	if overridden.ExpireMs == nil || *overridden.ExpireMs != 1000 {
		t.Errorf("override expireMs = %v, want 1000", overridden.ExpireMs)
	}
	if overridden.RetryIntervalMs == nil || *overridden.RetryIntervalMs != 50 {
		t.Errorf("unrelated overrides must keep defaults, got %v", overridden.RetryIntervalMs)
	}
}
