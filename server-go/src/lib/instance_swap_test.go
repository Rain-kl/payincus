package lib

import "testing"

func instanceSwapInt32Ptr(value int32) *int32 { return &value }

func TestInstanceSwapResolveEffectiveSize(t *testing.T) {
	cases := []struct {
		name     string
		instance *int32
		plan     *int32
		want     int32
	}{
		// An explicit instance value always wins, including zero.
		{"instance zero wins over plan", instanceSwapInt32Ptr(0), instanceSwapInt32Ptr(512), 0},
		{"instance value wins over plan", instanceSwapInt32Ptr(1024), instanceSwapInt32Ptr(512), 1024},
		// NULL instance means "inherit the plan".
		{"inherit plan", nil, instanceSwapInt32Ptr(512), 512},
		{"inherit zero plan", nil, instanceSwapInt32Ptr(0), 0},
		// Both NULL resolves to 0.
		{"both null", nil, nil, 0},
		// Negatives clamp to zero on either side.
		{"negative instance", instanceSwapInt32Ptr(-1), instanceSwapInt32Ptr(512), 0},
		{"negative plan", nil, instanceSwapInt32Ptr(-1), 0},
		{"negative instance and plan", instanceSwapInt32Ptr(-9), instanceSwapInt32Ptr(-9), 0},
	}
	for _, tc := range cases {
		if got := InstanceSwapResolveEffectiveSize(tc.instance, tc.plan); got != tc.want {
			t.Errorf("%s: InstanceSwapResolveEffectiveSize(%v, %v) = %d, want %d", tc.name, derefInt32(tc.instance), derefInt32(tc.plan), got, tc.want)
		}
	}
}

func TestInstanceSwapShouldSyncWithPlan(t *testing.T) {
	cases := []struct {
		name     string
		instance *int32
		plan     *int32
		want     bool
	}{
		// NULL means the instance follows the plan by definition.
		{"null instance", nil, instanceSwapInt32Ptr(512), true},
		{"null instance null plan", nil, nil, true},
		// Equal values keep following the plan (after clamping).
		{"equal", instanceSwapInt32Ptr(512), instanceSwapInt32Ptr(512), true},
		{"both zero", instanceSwapInt32Ptr(0), instanceSwapInt32Ptr(0), true},
		{"negative normalises to plan zero", instanceSwapInt32Ptr(-1), instanceSwapInt32Ptr(0), true},
		{"negative normalises to null plan", instanceSwapInt32Ptr(-1), nil, true},
		// Different values are a user override: do not sync.
		{"differs", instanceSwapInt32Ptr(1024), instanceSwapInt32Ptr(512), false},
		{"zero vs plan", instanceSwapInt32Ptr(0), instanceSwapInt32Ptr(512), false},
		{"value vs null plan", instanceSwapInt32Ptr(1), nil, false},
	}
	for _, tc := range cases {
		if got := InstanceSwapShouldSyncWithPlan(tc.instance, tc.plan); got != tc.want {
			t.Errorf("%s: InstanceSwapShouldSyncWithPlan(%v, %v) = %v, want %v", tc.name, derefInt32(tc.instance), derefInt32(tc.plan), got, tc.want)
		}
	}
}

func TestInstanceSwapFormatIncusLimit(t *testing.T) {
	cases := []struct {
		size int32
		want string
	}{
		{0, "0MiB"},
		{-5, "0MiB"},
		{1, "1MiB"},
		{512, "512MiB"},
		{1024, "1024MiB"},
		{2147483647, "2147483647MiB"},
	}
	for _, tc := range cases {
		if got := InstanceSwapFormatIncusLimit(tc.size); got != tc.want {
			t.Errorf("InstanceSwapFormatIncusLimit(%d) = %q, want %q", tc.size, got, tc.want)
		}
	}
}

func TestInstanceSwapResolveIncusValue(t *testing.T) {
	cases := []struct {
		enabled bool
		size    *int32
		want    string
	}{
		{false, instanceSwapInt32Ptr(512), "false"},
		{false, nil, "false"},
		{false, instanceSwapInt32Ptr(0), "false"},
		{true, nil, "false"},
		{true, instanceSwapInt32Ptr(0), "false"},
		{true, instanceSwapInt32Ptr(-3), "false"},
		{true, instanceSwapInt32Ptr(1), "1MiB"},
		{true, instanceSwapInt32Ptr(512), "512MiB"},
	}
	for _, tc := range cases {
		if got := InstanceSwapResolveIncusValue(tc.enabled, tc.size); got != tc.want {
			t.Errorf("InstanceSwapResolveIncusValue(%v, %v) = %q, want %q", tc.enabled, derefInt32(tc.size), got, tc.want)
		}
	}
}

// TestInstanceSwapMatchesNode pins the full 6x6 input matrix produced by
// server/src/lib/instance-swap.ts (integer inputs only, because every caller feeds
// an Int column). negative values are the ones that separate `Math.max(0, x)` from
// a raw passthrough, and NULL is what separates "inherit" from "override".
func TestInstanceSwapMatchesNode(t *testing.T) {
	null := (*int32)(nil)
	zero, minusOne, one, half, kilo := instanceSwapInt32Ptr(0), instanceSwapInt32Ptr(-1), instanceSwapInt32Ptr(1), instanceSwapInt32Ptr(512), instanceSwapInt32Ptr(1024)
	inputs := []*int32{null, zero, minusOne, one, half, kilo}

	type expectation struct {
		effective int32
		sync      bool
	}
	// Rows: instance size. Columns: previous plan size (same order as inputs).
	want := [][]expectation{
		// instance NULL → inherit the plan, always in sync.
		{{0, true}, {0, true}, {0, true}, {1, true}, {512, true}, {1024, true}},
		// instance 0 → explicit zero, in sync only with plan 0 / NULL / negative.
		{{0, true}, {0, true}, {0, true}, {0, false}, {0, false}, {0, false}},
		// instance -1 clamps to 0, same as above.
		{{0, true}, {0, true}, {0, true}, {0, false}, {0, false}, {0, false}},
		// instance 1.
		{{1, false}, {1, false}, {1, false}, {1, true}, {1, false}, {1, false}},
		// instance 512.
		{{512, false}, {512, false}, {512, false}, {512, false}, {512, true}, {512, false}},
		// instance 1024.
		{{1024, false}, {1024, false}, {1024, false}, {1024, false}, {1024, false}, {1024, true}},
	}

	for row, instance := range inputs {
		for column, plan := range inputs {
			wantRow := want[row][column]
			if got := InstanceSwapResolveEffectiveSize(instance, plan); got != wantRow.effective {
				t.Errorf("effective(instance=%s, plan=%s) = %d, want %d", labelInt32(instance), labelInt32(plan), got, wantRow.effective)
			}
			if got := InstanceSwapShouldSyncWithPlan(instance, plan); got != wantRow.sync {
				t.Errorf("sync(instance=%s, plan=%s) = %v, want %v", labelInt32(instance), labelInt32(plan), got, wantRow.sync)
			}
		}
	}
}

func derefInt32(value *int32) any {
	if value == nil {
		return nil
	}
	return *value
}

func labelInt32(value *int32) string {
	if value == nil {
		return "null"
	}
	return itoaForTest(*value)
}

func itoaForTest(value int32) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [12]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		index--
		digits[index] = '-'
	}
	return string(digits[index:])
}
