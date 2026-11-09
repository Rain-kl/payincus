package routes

import (
	"encoding/json"
	"testing"
)

// decodeAdvancedBody mirrors the Fastify body decode the handler performs.
func decodeAdvancedBody(t *testing.T, raw string) any {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("test payload is not valid JSON: %v", err)
	}
	return decoded
}

// Messages and ordering are pinned to real Fastify+AJV output (probed with
// ajv@8.17.1 under @fastify/ajv-compiler defaults): allOf runs before the property
// keywords, then minProperties, then properties in schema order.
func TestInstanceAdvancedConfigValidateBody(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantMsg string
		checks  map[string]any
	}{
		{"empty object", `{}`, "must NOT have fewer than 1 properties", nil},
		{"unknown only is stripped", `{"foo":1}`, "", map[string]any{}},
		{"non-object array", `[]`, "must NOT be valid", nil},
		{"non-object null", `null`, "must NOT be valid", nil},
		{"non-object string", `"x"`, "must NOT be valid", nil},
		{"both read pair", `{"limitsRead":"100MB","limitsReadIops":5}`, "must NOT be valid", nil},
		{"both write pair", `{"limitsWrite":null,"limitsWriteIops":null}`, "must NOT be valid", nil},
		{"allOf wins over a property error", `{"limitsRead":"bad","limitsReadIops":1}`, "must NOT be valid", nil},
		{"valid read", `{"limitsRead":"100MB"}`, "", map[string]any{"limitsRead": "100MB"}},
		{"bad read pattern", `{"limitsRead":"100xb"}`, `limitsRead format is invalid`, nil},
		{"numeric read is stringified then pattern-checked", `{"limitsRead":5}`, `limitsRead format is invalid`, nil},
		{"null read accepted", `{"limitsRead":null}`, "", map[string]any{"limitsRead": nil}},
		{"single string array is not unwrapped for multi-type", `{"limitsRead":["100MB"]}`, "must be string,null", nil},
		{"single number array is coerced", `{"limitsRead":[5]}`, `limitsRead format is invalid`, nil},
		{"ingress zero rejected by pattern", `{"limitsIngress":"0Mbit"}`, `limitsIngress format is invalid`, nil},
		{"ingress Mbps accepted", `{"limitsIngress":"1Mbps"}`, "", map[string]any{"limitsIngress": "1Mbps"}},
		{"iops string coerced", `{"limitsReadIops":"12"}`, "", map[string]any{"limitsReadIops": float64(12)}},
		{"iops string rejected", `{"limitsReadIops":"abc"}`, "must be integer,null", nil},
		{"iops above max", `{"limitsReadIops":2147483648}`, "must be <= 2147483647", nil},
		{"iops below min", `{"limitsReadIops":-1}`, "must be >= 0", nil},
		{"cpu priority above max", `{"limitsCpuPriority":11}`, "must be <= 10", nil},
		{"autostart arbitrary string rejected", `{"bootAutostart":"x"}`, "must be boolean,null", nil},
		{"autostart null accepted", `{"bootAutostart":null}`, "", map[string]any{"bootAutostart": nil}},
		{"autostart string true coerced", `{"bootAutostart":"true"}`, "", map[string]any{"bootAutostart": true}},
		{"autostart single true array fails (AJV multi-type quirk)", `{"bootAutostart":[true]}`, "must be boolean,null", nil},
		{"autostart single false array becomes null", `{"bootAutostart":[false]}`, "", map[string]any{"bootAutostart": nil}},
		{"delay above max", `{"bootAutostartDelay":601}`, "must be <= 600", nil},
		{"shutdown timeout above max", `{"bootHostShutdownTimeout":601}`, "must be <= 600", nil},
		{"priority above max", `{"bootAutostartPriority":101}`, "must be <= 100", nil},
	}

	for _, tc := range cases {
		values, message := instanceAdvancedConfigValidateBody(decodeAdvancedBody(t, tc.raw))
		if message != tc.wantMsg {
			t.Errorf("%s: message = %q, want %q", tc.name, message, tc.wantMsg)
			continue
		}
		if tc.wantMsg != "" {
			continue
		}
		if len(values) != len(tc.checks) {
			t.Errorf("%s: values = %v, want %v", tc.name, values, tc.checks)
			continue
		}
		for key, want := range tc.checks {
			got, present := values[key]
			if !present || got != want {
				t.Errorf("%s: values[%q] = %v (present=%v), want %v", tc.name, key, got, present, want)
			}
		}
	}
}

func TestInstanceAdvancedConfigNormalizePlanTrafficLimitSpeed(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  *string
	}{
		{"null", nil, nil},
		{"empty", "", nil},
		{"zero", "0", nil},
		{"blank", "   ", nil},
		{"non-string", float64(5), nil},
		{"plain Mbit", "100Mbit", instanceAdvancedConfigTestStrPtr("100Mbit")},
		{"lowercase unit", "100mbit", instanceAdvancedConfigTestStrPtr("100Mbit")},
		{"Gbit multiplies", "1Gbit", instanceAdvancedConfigTestStrPtr("1000Mbit")},
		{"fractional Gbit rounds", "1.5Gbit", instanceAdvancedConfigTestStrPtr("1500Mbit")},
		{"whitespace before unit", "100 Mbit", instanceAdvancedConfigTestStrPtr("100Mbit")},
		{"sub-Mbit rounds up", "0.5Mbit", instanceAdvancedConfigTestStrPtr("1Mbit")},
		{"zero numeric", "0Mbit", nil},
		{"bytes divide by MiB", "1048576", instanceAdvancedConfigTestStrPtr("1Mbit")},
		{"bytes below one MiB", "1048575", nil},
		{"garbage", "abc", nil},
		{"Mbps is not a line-speed unit", "100Mbps", nil},
	}
	for _, tc := range cases {
		got := instanceAdvancedConfigNormalizePlanTrafficLimitSpeed(tc.value)
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("%s: got %q, want nil", tc.name, *got)
		case tc.want != nil && (got == nil || *got != *tc.want):
			t.Errorf("%s: got %v, want %q", tc.name, got, *tc.want)
		}
	}
}

func instanceAdvancedConfigTestStrPtr(value string) *string { return &value }

func TestInstanceAdvancedConfigComputeEffectiveBandwidth(t *testing.T) {
	cases := []struct {
		name    string
		source  instanceAdvancedConfigBandwidthSource
		overage string
		ingress *string
		egress  *string
	}{
		{
			name: "configured values win",
			source: instanceAdvancedConfigBandwidthSource{
				ConfiguredLimitsIngress: instanceAdvancedConfigTestStrPtr("500Mbit"),
				ConfiguredLimitsEgress:  instanceAdvancedConfigTestStrPtr("400Mbit"),
				TrafficStatus:           "NORMAL",
				PackageLimitsIngress:    instanceAdvancedConfigTestStrPtr("300Mbit"),
				PackageLimitsEgress:     instanceAdvancedConfigTestStrPtr("300Mbit"),
			},
			overage: "1Mbit",
			ingress: instanceAdvancedConfigTestStrPtr("500Mbit"),
			egress:  instanceAdvancedConfigTestStrPtr("400Mbit"),
		},
		{
			name: "plan normal speed is the baseline",
			source: instanceAdvancedConfigBandwidthSource{
				TrafficStatus:        "NORMAL",
				PackagePlanSpeed:     instanceAdvancedConfigTestStrPtr("2Gbit"),
				PackageLimitsIngress: instanceAdvancedConfigTestStrPtr("300Mbit"),
				PackageLimitsEgress:  instanceAdvancedConfigTestStrPtr("300Mbit"),
			},
			overage: "1Mbit",
			ingress: instanceAdvancedConfigTestStrPtr("2000Mbit"),
			egress:  instanceAdvancedConfigTestStrPtr("2000Mbit"),
		},
		{
			name: "package limits fallback",
			source: instanceAdvancedConfigBandwidthSource{
				TrafficStatus:        "NORMAL",
				PackageLimitsIngress: instanceAdvancedConfigTestStrPtr("300Mbit"),
				PackageLimitsEgress:  instanceAdvancedConfigTestStrPtr("250Mbit"),
			},
			overage: "1Mbit",
			ingress: instanceAdvancedConfigTestStrPtr("300Mbit"),
			egress:  instanceAdvancedConfigTestStrPtr("250Mbit"),
		},
		{
			name: "limited traffic caps at the overage throttle",
			source: instanceAdvancedConfigBandwidthSource{
				TrafficStatus:           "LIMITED",
				ConfiguredLimitsIngress: instanceAdvancedConfigTestStrPtr("500Mbit"),
				ConfiguredLimitsEgress:  instanceAdvancedConfigTestStrPtr("400Mbit"),
			},
			overage: "1Mbit",
			ingress: instanceAdvancedConfigTestStrPtr("1Mbit"),
			egress:  instanceAdvancedConfigTestStrPtr("1Mbit"),
		},
		{
			name: "throttle above the configured value keeps the configured value",
			source: instanceAdvancedConfigBandwidthSource{
				TrafficStatus:           "LIMITED",
				ConfiguredLimitsIngress: instanceAdvancedConfigTestStrPtr("1Mbit"),
			},
			overage: "10Mbit",
			ingress: instanceAdvancedConfigTestStrPtr("1Mbit"),
			egress:  instanceAdvancedConfigTestStrPtr("10Mbit"),
		},
		{
			name:    "nothing configured",
			source:  instanceAdvancedConfigBandwidthSource{TrafficStatus: "NORMAL"},
			overage: "1Mbit",
			ingress: nil,
			egress:  nil,
		},
	}
	for _, tc := range cases {
		got := instanceAdvancedConfigComputeEffectiveBandwidth(tc.source, tc.overage)
		assertOptionalString(t, tc.name+" ingress", got.Ingress, tc.ingress)
		assertOptionalString(t, tc.name+" egress", got.Egress, tc.egress)
	}
}

func assertOptionalString(t *testing.T, label string, got, want *string) {
	t.Helper()
	switch {
	case got == nil && want == nil:
		return
	case got == nil || want == nil:
		t.Errorf("%s: got %v, want %v", label, got, want)
	case *got != *want:
		t.Errorf("%s: got %q, want %q", label, *got, *want)
	}
}

func TestInstanceAdvancedConfigReplaceSpeedSuffix(t *testing.T) {
	cases := map[string]string{
		"100Mbps":  "100Mbit",
		"1Gbps":    "1Gbit",
		"100Kbps":  "100kbit",
		"100MBPS":  "100Mbit",
		"100Mbit":  "100Mbit",
		"100":      "100",
		"Mbps":     "Mbit",
		"abcMbpsX": "abcMbpsX",
	}
	for input, want := range cases {
		if got := instanceAdvancedConfigReplaceSpeedSuffix(input); got != want {
			t.Errorf("replaceSpeedSuffix(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestInstanceAdvancedConfigStringJS(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{"x", "x"},
		{true, "true"},
		{false, "false"},
		{float64(500), "500"},
		{int32(20), "20"},
		{nil, "undefined"},
	}
	for _, tc := range cases {
		if got := instanceAdvancedConfigStringJS(tc.value); got != tc.want {
			t.Errorf("String(%v) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

// The in-process active set is the Node module's `const active = new Set()`.
func TestInstanceAdvancedConfigActiveSet(t *testing.T) {
	if !instanceAdvancedConfigTryAcquire(424242) {
		t.Fatal("first acquire must succeed")
	}
	if instanceAdvancedConfigTryAcquire(424242) {
		t.Fatal("second acquire must fail while the first is held")
	}
	instanceAdvancedConfigRelease(424242)
	if !instanceAdvancedConfigTryAcquire(424242) {
		t.Fatal("acquire after release must succeed")
	}
	instanceAdvancedConfigRelease(424242)
}
