package services

import (
	"context"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/models"
)

func trafficBandwidthSP(value string) *string { return &value }

// TestTrafficBandwidthNormalizeMatchesNode reuses the table produced by importing
// server/src/services/traffic-bandwidth.ts with tsx.
func TestTrafficBandwidthNormalizeMatchesNode(t *testing.T) {
	cases := []struct {
		input   *string
		want    *string // nil = null
		invalid bool    // true = undefined
	}{
		{nil, nil, false},                        // null
		{trafficBandwidthSP(""), nil, false},     // ''
		{trafficBandwidthSP("   "), nil, false},  // whitespace only
		{trafficBandwidthSP("0"), nil, false},    // exact "0"
		{trafficBandwidthSP("0Mbit"), nil, true}, // numeric <= 0 → undefined
		{trafficBandwidthSP("1Mbit"), trafficBandwidthSP("1Mbit"), false},
		{trafficBandwidthSP("1.5Mbit"), trafficBandwidthSP("2Mbit"), false},  // Math.round(1.5)
		{trafficBandwidthSP("01Mbit"), trafficBandwidthSP("1Mbit"), false},   // leading zero dropped
		{trafficBandwidthSP("2Gbit"), trafficBandwidthSP("2000Mbit"), false}, // Gbit → ×1000
		{trafficBandwidthSP("0.5Gbit"), trafficBandwidthSP("500Mbit"), false},
		{trafficBandwidthSP(" 10Mbit "), trafficBandwidthSP("10Mbit"), false}, // trimmed
		{trafficBandwidthSP("100 mbit"), trafficBandwidthSP("100Mbit"), false},
		{trafficBandwidthSP("1MBIT"), trafficBandwidthSP("1Mbit"), false},
		{trafficBandwidthSP("10 Gbit"), trafficBandwidthSP("10000Mbit"), false},
		{trafficBandwidthSP("1.9Gbit"), trafficBandwidthSP("1900Mbit"), false},
		{trafficBandwidthSP("1000"), nil, false},    // 1000 bytes < 1 MiB → null
		{trafficBandwidthSP("1048575"), nil, false}, // one byte short of 1 MiB → null
		{trafficBandwidthSP("1048576"), trafficBandwidthSP("1Mbit"), false},
		{trafficBandwidthSP("2097152"), trafficBandwidthSP("2Mbit"), false},
		{trafficBandwidthSP("3000000"), trafficBandwidthSP("2Mbit"), false}, // truncating BigInt division
		{trafficBandwidthSP("abc"), nil, true},
		{trafficBandwidthSP("Mbit"), nil, true},
		{trafficBandwidthSP("10Mbps"), nil, true},
		{trafficBandwidthSP("1mb it"), nil, true},
		// BigInt keeps full precision where int64 would overflow.
		{trafficBandwidthSP("999999999999999999999999"), trafficBandwidthSP("953674316406249999Mbit"), false},
	}

	for _, tc := range cases {
		got := TrafficBandwidthNormalizePlanTrafficLimitSpeed(tc.input)
		label := "<nil>"
		if tc.input != nil {
			label = *tc.input
		}
		if got.Invalid != tc.invalid {
			t.Errorf("normalize(%q).Invalid = %v, want %v", label, got.Invalid, tc.invalid)
			continue
		}
		if tc.want == nil {
			if got.Value != nil {
				t.Errorf("normalize(%q) = %q, want null", label, *got.Value)
			}
			if tc.invalid == false && !got.IsNull() {
				t.Errorf("normalize(%q) should report null", label)
			}
			continue
		}
		if got.Value == nil || *got.Value != *tc.want {
			t.Errorf("normalize(%q) = %v, want %q", label, got.Value, *tc.want)
		}
	}
}

func TestTrafficBandwidthComputeEffectiveMatchesNode(t *testing.T) {
	plan := func(speed string) *TrafficBandwidthPlanSpeed {
		return &TrafficBandwidthPlanSpeed{TrafficLimitSpeed: trafficBandwidthSP(speed)}
	}
	limits := func(ingress, egress string) *TrafficBandwidthPackageLimits {
		return &TrafficBandwidthPackageLimits{LimitsIngress: trafficBandwidthSP(ingress), LimitsEgress: trafficBandwidthSP(egress)}
	}

	cases := []struct {
		name     string
		instance TrafficBandwidthArbitrationSource
		throttle string
		want     TrafficBandwidthEffective
	}{
		{
			name:     "all empty",
			instance: TrafficBandwidthArbitrationSource{TrafficStatus: "NORMAL"},
			throttle: TrafficBandwidthDefaultOverageThrottleSpeed,
			want:     TrafficBandwidthEffective{},
		},
		{
			name:     "plan only",
			instance: TrafficBandwidthArbitrationSource{TrafficStatus: "NORMAL", PackagePlan: plan("100Mbit")},
			throttle: TrafficBandwidthDefaultOverageThrottleSpeed,
			want:     TrafficBandwidthEffective{Ingress: trafficBandwidthSP("100Mbit"), Egress: trafficBandwidthSP("100Mbit")},
		},
		{
			name: "configured wins over the plan",
			instance: TrafficBandwidthArbitrationSource{
				TrafficStatus:           "NORMAL",
				ConfiguredLimitsIngress: trafficBandwidthSP("50Mbit"),
				ConfiguredLimitsEgress:  trafficBandwidthSP("200Mbit"),
				PackagePlan:             plan("100Mbit"),
			},
			throttle: TrafficBandwidthDefaultOverageThrottleSpeed,
			want:     TrafficBandwidthEffective{Ingress: trafficBandwidthSP("50Mbit"), Egress: trafficBandwidthSP("200Mbit")},
		},
		{
			name:     "package is the fallback",
			instance: TrafficBandwidthArbitrationSource{TrafficStatus: "NORMAL", Package: limits("20Mbit", "30Mbit")},
			throttle: TrafficBandwidthDefaultOverageThrottleSpeed,
			want:     TrafficBandwidthEffective{Ingress: trafficBandwidthSP("20Mbit"), Egress: trafficBandwidthSP("30Mbit")},
		},
		{
			name:     "limited clamps to the throttle",
			instance: TrafficBandwidthArbitrationSource{TrafficStatus: "LIMITED", PackagePlan: plan("100Mbit")},
			throttle: TrafficBandwidthDefaultOverageThrottleSpeed,
			want:     TrafficBandwidthEffective{Ingress: trafficBandwidthSP("1Mbit"), Egress: trafficBandwidthSP("1Mbit")},
		},
		{
			name: "limited never raises a slower baseline",
			instance: TrafficBandwidthArbitrationSource{
				TrafficStatus:           "LIMITED",
				ConfiguredLimitsIngress: trafficBandwidthSP("0.5Mbit"),
			},
			throttle: "1Mbit",
			want:     TrafficBandwidthEffective{Ingress: trafficBandwidthSP("1Mbit"), Egress: trafficBandwidthSP("1Mbit")},
		},
		{
			name:     "limited with no baseline still throttles",
			instance: TrafficBandwidthArbitrationSource{TrafficStatus: "LIMITED"},
			throttle: "1Mbit",
			want:     TrafficBandwidthEffective{Ingress: trafficBandwidthSP("1Mbit"), Egress: trafficBandwidthSP("1Mbit")},
		},
		{
			name:     "custom throttle",
			instance: TrafficBandwidthArbitrationSource{TrafficStatus: "LIMITED", PackagePlan: plan("100Mbit")},
			throttle: "5Mbit",
			want:     TrafficBandwidthEffective{Ingress: trafficBandwidthSP("5Mbit"), Egress: trafficBandwidthSP("5Mbit")},
		},
		{
			name:     "non-LIMITED ignores the throttle",
			instance: TrafficBandwidthArbitrationSource{TrafficStatus: "UNLIMITED", PackagePlan: plan("100Mbit")},
			throttle: "1Mbit",
			want:     TrafficBandwidthEffective{Ingress: trafficBandwidthSP("100Mbit"), Egress: trafficBandwidthSP("100Mbit")},
		},
		{
			name: "invalid plan speed falls through to the package",
			instance: TrafficBandwidthArbitrationSource{
				TrafficStatus: "NORMAL",
				PackagePlan:   plan("bogus"),
				Package:       &TrafficBandwidthPackageLimits{LimitsIngress: trafficBandwidthSP("10Mbit"), LimitsEgress: nil},
			},
			throttle: "1Mbit",
			want:     TrafficBandwidthEffective{Ingress: trafficBandwidthSP("10Mbit"), Egress: nil},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TrafficBandwidthComputeEffective(tc.instance, tc.throttle)
			trafficBandwidthAssertEffective(t, got, tc.want)
		})
	}
}

func TestTrafficBandwidthResolveLimitsMatchesNode(t *testing.T) {
	plan := func(speed string) *TrafficBandwidthPlanSpeed {
		return &TrafficBandwidthPlanSpeed{TrafficLimitSpeed: trafficBandwidthSP(speed)}
	}
	pkg := func(ingress, egress string) *TrafficBandwidthPackageLimits {
		return &TrafficBandwidthPackageLimits{LimitsIngress: trafficBandwidthSP(ingress), LimitsEgress: trafficBandwidthSP(egress)}
	}

	cases := []struct {
		name     string
		instance TrafficBandwidthSource
		options  TrafficBandwidthResolveOptions
		want     TrafficBandwidthResolvedLimits
	}{
		{
			name:     "plan line speed wins over instance limits",
			instance: TrafficBandwidthSource{LimitsIngress: trafficBandwidthSP("1Mbit"), LimitsEgress: trafficBandwidthSP("2Mbit"), PackagePlan: plan("100Mbit")},
			want: TrafficBandwidthResolvedLimits{
				IncusIngress: trafficBandwidthSP("100Mbit"), IncusEgress: trafficBandwidthSP("100Mbit"),
				DBIngress: trafficBandwidthSP("100Mbit"), DBEgress: trafficBandwidthSP("100Mbit"),
			},
		},
		{
			name:     "instance limits with a package fallback for incus only",
			instance: TrafficBandwidthSource{LimitsIngress: trafficBandwidthSP("50Mbit"), LimitsEgress: trafficBandwidthSP("60Mbit"), Package: pkg("10Mbit", "20Mbit")},
			want: TrafficBandwidthResolvedLimits{
				IncusIngress: trafficBandwidthSP("50Mbit"), IncusEgress: trafficBandwidthSP("60Mbit"),
				DBIngress: trafficBandwidthSP("50Mbit"), DBEgress: trafficBandwidthSP("60Mbit"),
			},
		},
		{
			name:     "strip throttle override replaces it with the package value",
			instance: TrafficBandwidthSource{LimitsIngress: trafficBandwidthSP("1Mbit"), LimitsEgress: trafficBandwidthSP("2Mbit"), Package: pkg("10Mbit", "20Mbit")},
			options:  TrafficBandwidthResolveOptions{StripThrottleOverride: true},
			want: TrafficBandwidthResolvedLimits{
				IncusIngress: trafficBandwidthSP("10Mbit"), IncusEgress: trafficBandwidthSP("2Mbit"),
				DBIngress: nil, DBEgress: trafficBandwidthSP("2Mbit"),
			},
		},
		{
			name:     "strip honours a custom throttle value",
			instance: TrafficBandwidthSource{LimitsIngress: trafficBandwidthSP("3Mbit"), LimitsEgress: trafficBandwidthSP("2Mbit")},
			options:  TrafficBandwidthResolveOptions{StripThrottleOverride: true, OverageThrottleSpeed: trafficBandwidthSP("3Mbit")},
			want: TrafficBandwidthResolvedLimits{
				IncusIngress: nil, IncusEgress: trafficBandwidthSP("2Mbit"),
				DBIngress: nil, DBEgress: trafficBandwidthSP("2Mbit"),
			},
		},
		{
			name:     "default throttle is not stripped unless asked",
			instance: TrafficBandwidthSource{LimitsIngress: trafficBandwidthSP("1Mbit")},
			want: TrafficBandwidthResolvedLimits{
				IncusIngress: trafficBandwidthSP("1Mbit"), IncusEgress: nil,
				DBIngress: trafficBandwidthSP("1Mbit"), DBEgress: nil,
			},
		},
		{
			name:     "nothing configured",
			instance: TrafficBandwidthSource{},
			want:     TrafficBandwidthResolvedLimits{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TrafficBandwidthResolveTrafficBandwidthLimits(tc.instance, tc.options)
			if !trafficBandwidthEqualPtr(got.IncusIngress, tc.want.IncusIngress) || !trafficBandwidthEqualPtr(got.IncusEgress, tc.want.IncusEgress) ||
				!trafficBandwidthEqualPtr(got.DBIngress, tc.want.DBIngress) || !trafficBandwidthEqualPtr(got.DBEgress, tc.want.DBEgress) {
				t.Errorf("got %+v, want %+v", trafficBandwidthStringifyResolved(got), trafficBandwidthStringifyResolved(tc.want))
			}
		})
	}
}

func TestTrafficBandwidthApplyLimits(t *testing.T) {
	devices := map[string]map[string]any{
		"eth0": {"type": "nic", "nictype": "bridged", "limits.ingress": "1Mbit", "limits.egress": "1Mbit"},
		"root": {"type": "disk", "path": "/", "limits.ingress": "keep-me"},
		"eth1": {"type": "nic", "name": "eth1"},
	}

	updated := TrafficBandwidthApplyLimits(devices, trafficBandwidthSP("100Mbit"), nil)

	if updated["eth0"]["limits.ingress"] != "100Mbit" {
		t.Errorf("eth0 ingress = %v", updated["eth0"]["limits.ingress"])
	}
	if _, present := updated["eth0"]["limits.egress"]; present {
		t.Error("eth0 egress must be deleted when the limit is null")
	}
	if updated["root"]["limits.ingress"] != "keep-me" {
		t.Error("non-nic devices must be untouched")
	}
	if _, present := updated["eth1"]["limits.egress"]; present {
		t.Error("eth1 egress must not appear")
	}
	if got := updated["eth1"]["limits.ingress"]; got != "100Mbit" {
		t.Errorf("eth1 ingress = %v", got)
	}

	// The input must not be mutated (the caller keeps its own copy).
	if devices["eth0"]["limits.ingress"] != "1Mbit" {
		t.Error("ApplyLimits must not mutate its input")
	}
	if _, present := devices["eth0"]["limits.egress"]; !present {
		t.Error("ApplyLimits must not mutate its input")
	}

	// Empty strings behave like null (truthiness in the TypeScript).
	updated = TrafficBandwidthApplyLimits(devices, trafficBandwidthSP(""), trafficBandwidthSP(""))
	if _, present := updated["eth0"]["limits.ingress"]; present {
		t.Error("an empty limit must delete the key")
	}
	if _, present := updated["eth0"]["limits.egress"]; present {
		t.Error("an empty limit must delete the key")
	}
}

func TestTrafficBandwidthLockKeyAndConstants(t *testing.T) {
	if got := trafficBandwidthInstanceLockKey(42); got != "bandwidth:instance:42" {
		t.Errorf("lock key = %q", got)
	}
	if TrafficBandwidthOverageThrottleConfigKey != "traffic_overage_throttle_speed" {
		t.Errorf("config key = %q", TrafficBandwidthOverageThrottleConfigKey)
	}
	if TrafficBandwidthDefaultOverageThrottleSpeed != "1Mbit" {
		t.Errorf("default throttle = %q", TrafficBandwidthDefaultOverageThrottleSpeed)
	}
}

func TestTrafficBandwidthReconcileRequiresDatabase(t *testing.T) {
	// No database in unit tests: the lock cannot be acquired and the function must
	// fail rather than silently return an unpersisted value.
	if _, err := TrafficBandwidthReconcileEffective(context.Background(), 1, nil); err == nil {
		t.Fatal("expected an error without a database")
	}
}

func TestTrafficBandwidthReconcileLockOptions(t *testing.T) {
	// The TypeScript passes {expireMs: 30000, waitTimeoutMs: 10000,
	// retryIntervalMs: 50}; the constants are asserted through the options helper so
	// a future refactor cannot quietly fall back to the 5 s default.
	options := trafficBandwidthLockOptions()
	if options.ExpireMs == nil || *options.ExpireMs != 30_000 {
		t.Error("expireMs must be 30 s")
	}
	if options.WaitTimeoutMs == nil || *options.WaitTimeoutMs != 10_000 {
		t.Error("waitTimeoutMs must be 10 s")
	}
	if options.RetryIntervalMs == nil || *options.RetryIntervalMs != 50 {
		t.Error("retryIntervalMs must be 50 ms")
	}
}

func trafficBandwidthAssertEffective(t *testing.T, got, want TrafficBandwidthEffective) {
	t.Helper()
	if !trafficBandwidthEqualPtr(got.Ingress, want.Ingress) || !trafficBandwidthEqualPtr(got.Egress, want.Egress) {
		t.Errorf("got ingress=%v egress=%v, want ingress=%v egress=%v",
			trafficBandwidthStringify(got.Ingress), trafficBandwidthStringify(got.Egress), trafficBandwidthStringify(want.Ingress), trafficBandwidthStringify(want.Egress))
	}
}

func trafficBandwidthEqualPtr(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func trafficBandwidthStringify(value *string) string {
	if value == nil {
		return "null"
	}
	return *value
}

func trafficBandwidthStringifyResolved(value TrafficBandwidthResolvedLimits) string {
	return "{" + trafficBandwidthStringify(value.IncusIngress) + "," + trafficBandwidthStringify(value.IncusEgress) + "," +
		trafficBandwidthStringify(value.DBIngress) + "," + trafficBandwidthStringify(value.DBEgress) + "}"
}

var _ = models.TrafficStatusLIMITED
