package lib

import "testing"

// TestAllowanceToCores pins the ceiling rule, including the case that makes it
// necessary: 101% already needs 2 cores because a container cannot be limited to a
// fraction of a core through limits.cpu.
func TestAllowanceToCores(t *testing.T) {
	cases := map[int]int{
		0:    1,
		-10:  1,
		1:    1,
		100:  1,
		101:  2,
		150:  2,
		200:  2,
		201:  3,
		250:  3,
		300:  3,
		301:  4,
		1000: 10,
	}
	for allowance, want := range cases {
		if got := AllowanceToCores(allowance); got != want {
			t.Errorf("AllowanceToCores(%d) = %d, want %d", allowance, got, want)
		}
	}
}

// TestIsValidAllowance pins the range plus the 5%-step granularity.
func TestIsValidAllowance(t *testing.T) {
	cases := []struct {
		allowance int
		min       int
		max       int
		want      bool
	}{
		{15, 15, -1, true},
		{20, 15, -1, true},
		{14, 15, -1, false},
		{17, 15, -1, false}, // not a multiple of 5
		{100, 15, 200, true},
		{200, 15, 200, true},
		{205, 15, 200, false}, // above the ceiling
		{0, 0, -1, true},
		{5, 0, -1, true},
	}
	for _, tc := range cases {
		if got := IsValidAllowance(tc.allowance, tc.min, tc.max); got != tc.want {
			t.Errorf("IsValidAllowance(%d, %d, %d) = %v, want %v",
				tc.allowance, tc.min, tc.max, got, tc.want)
		}
	}
}

// TestNormalizeAllowance pins the three-step clamp/floor/clamp sequence. The final
// clamp is the subtle one: 12 floors to 10, which is below a minimum of 15.
func TestNormalizeAllowance(t *testing.T) {
	cases := []struct {
		allowance int
		min       int
		max       int
		want      int
	}{
		{12, 15, -1, 15},
		{15, 15, -1, 15},
		{17, 15, -1, 15},
		{18, 15, -1, 15},
		{20, 15, -1, 20},
		{203, 15, 200, 200},
		{199, 15, 200, 195},
		{0, 15, -1, 15},
		{1000, 15, 200, 200},
	}
	for _, tc := range cases {
		if got := NormalizeAllowance(tc.allowance, tc.min, tc.max); got != tc.want {
			t.Errorf("NormalizeAllowance(%d, %d, %d) = %d, want %d",
				tc.allowance, tc.min, tc.max, got, tc.want)
		}
	}
}

// TestIncusConfigFormatters pins the exact strings Incus expects on the wire.
func TestIncusConfigFormatters(t *testing.T) {
	if got := formatAllowanceCores(150); got != "2" {
		t.Errorf("limits.cpu for 150%% = %q, want \"2\"", got)
	}
	if got := formatAllowancePercent(150); got != "150%" {
		t.Errorf("limits.cpu.allowance = %q, want \"150%%\"", got)
	}
	if got := formatMemoryLimit(1024); got != "1024MB" {
		t.Errorf("limits.memory = %q, want \"1024MB\"", got)
	}
	if got := formatDiskSize(20480); got != "20480MB" {
		t.Errorf("root size = %q, want \"20480MB\"", got)
	}
	if got := formatMemoryLimit(0); got != "0MB" {
		t.Errorf("zero memory = %q", got)
	}
}

func TestItoaSmall(t *testing.T) {
	cases := map[int]string{0: "0", 1: "1", 42: "42", 1000: "1000", -7: "-7"}
	for input, want := range cases {
		if got := itoaSmall(input); got != want {
			t.Errorf("itoaSmall(%d) = %q, want %q", input, got, want)
		}
	}
}
