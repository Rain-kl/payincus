package lib

import (
	"testing"
	"time"
)

// TestCalculateIncrement pins the counter-reset rule: a backwards counter
// contributes nothing instead of a huge burst.
func TestCalculateIncrement(t *testing.T) {
	cases := []struct{ current, previous, want int64 }{
		{1000, 400, 600},
		{400, 400, 0},
		{400, 1000, 0},
		{0, 0, 0},
		{1 << 40, 0, 1 << 40},
	}
	for _, tc := range cases {
		if got := CalculateIncrement(tc.current, tc.previous); got != tc.want {
			t.Errorf("CalculateIncrement(%d, %d) = %d, want %d", tc.current, tc.previous, got, tc.want)
		}
	}
}

func TestGetEffectiveLimit(t *testing.T) {
	if GetEffectiveLimit(nil, 100) != nil {
		t.Fatal("unlimited must stay unlimited")
	}
	limit := int64(1000)
	effective := GetEffectiveLimit(&limit, 250)
	if effective == nil || *effective != 1250 {
		t.Fatalf("effective = %v", effective)
	}
}

// TestIsOverLimitAndWarningThreshold pins the exact boundaries: over-limit is
// >= (not >), and the 80% threshold uses integer arithmetic on the limit.
func TestIsOverLimitAndWarningThreshold(t *testing.T) {
	limit := int64(1000)

	if IsOverLimit(999, &limit) {
		t.Fatal("999/1000 is not over the limit")
	}
	if !IsOverLimit(1000, &limit) {
		t.Fatal("exactly at the limit counts as over")
	}
	if IsOverLimit(1<<40, nil) {
		t.Fatal("unlimited never trips")
	}

	if IsWarningThreshold(799, &limit) {
		t.Fatal("799 is below 80%")
	}
	if !IsWarningThreshold(800, &limit) {
		t.Fatal("800 is exactly 80%")
	}

	// Integer truncation: (333 * 80) / 100 = 266 (not 266.4).
	odd := int64(333)
	if IsWarningThreshold(265, &odd) {
		t.Fatal("265 is below the truncated threshold")
	}
	if !IsWarningThreshold(266, &odd) {
		t.Fatal("266 is at the truncated threshold")
	}
}

func TestCalculateTrafficStatus(t *testing.T) {
	limit := int64(1000)

	if got := CalculateUserTrafficStatus(0, &limit); got != TrafficStatusNormal {
		t.Fatalf("got %q", got)
	}
	if got := CalculateUserTrafficStatus(800, &limit); got != TrafficStatusWarning {
		t.Fatalf("got %q", got)
	}
	if got := CalculateUserTrafficStatus(1000, &limit); got != TrafficStatusLimited {
		t.Fatalf("got %q", got)
	}

	// Instances have no warning tier.
	if got := CalculateInstanceTrafficStatus(800, &limit); got != TrafficStatusNormal {
		t.Fatalf("got %q", got)
	}
	if got := CalculateInstanceTrafficStatus(1000, &limit); got != TrafficStatusLimited {
		t.Fatalf("got %q", got)
	}

	// Unlimited is always normal.
	if got := CalculateUserTrafficStatus(1<<50, nil); got != TrafficStatusNormal {
		t.Fatalf("got %q", got)
	}
}

// TestFormatBytes pins the values measured against real Node.
func TestFormatBytes(t *testing.T) {
	cases := map[int64]string{
		0:             "0.00 B",
		1:             "1.00 B",
		1023:          "1023.00 B",
		1024:          "1.00 KB",
		1536:          "1.50 KB",
		1048576:       "1.00 MB",
		5368709120:    "5.00 GB",
		1099511627776: "1.00 TB",
		// The unit list stops at TB, so this keeps growing in TB.
		1125899906842624: "1024.00 TB",
	}
	for input, want := range cases {
		if got := FormatBytes(input); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", input, got, want)
		}
	}
}

// TestParseBytes pins the values measured against real Node.
func TestParseBytes(t *testing.T) {
	cases := map[string]int64{
		"100GB":  107374182400,
		"1.5TB":  1649267441664,
		"500MB":  524288000,
		"1B":     1,
		"0.5KB":  512,
		"100 gb": 107374182400,
	}
	for input, want := range cases {
		got, err := ParseBytes(input)
		if err != nil {
			t.Errorf("ParseBytes(%q) error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParseBytes(%q) = %d, want %d", input, got, want)
		}
	}

	for _, bad := range []string{"", "100", "100XB", "-1GB", "GB"} {
		if _, err := ParseBytes(bad); err == nil {
			t.Errorf("ParseBytes(%q) should fail", bad)
		}
	}
}

// TestCalculatePercentage pins the cap and the zero-limit guard.
func TestCalculatePercentage(t *testing.T) {
	if got := CalculatePercentage(100, nil); got != 0 {
		t.Fatalf("nil limit → %v", got)
	}
	zero := int64(0)
	if got := CalculatePercentage(100, &zero); got != 0 {
		t.Fatalf("zero limit → %v", got)
	}
	limit := int64(1000)
	if got := CalculatePercentage(160, &limit); got < 15.99 || got > 16.01 {
		t.Fatalf("160/1000 → %v, want ~16", got)
	}
	if got := CalculatePercentage(5000, &limit); got != 100 {
		t.Fatalf("over 100%% must cap at 100, got %v", got)
	}
}

// TestGetTrafficPeriod pins both branches of the reset-day window.
func TestGetTrafficPeriod(t *testing.T) {
	location := time.UTC

	// currentDay (20) >= resetDay (5): window is this month 5th → next month 5th.
	reference := time.Date(2026, 10, 20, 12, 0, 0, 0, location)
	start, end := GetTrafficPeriod(5, reference)
	if !start.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, location)) {
		t.Fatalf("start = %v", start)
	}
	if !end.Equal(time.Date(2026, 11, 5, 0, 0, 0, 0, location)) {
		t.Fatalf("end = %v", end)
	}

	// currentDay (3) < resetDay (5): window is last month 5th → this month 5th.
	reference = time.Date(2026, 10, 3, 12, 0, 0, 0, location)
	start, end = GetTrafficPeriod(5, reference)
	if !start.Equal(time.Date(2026, 9, 5, 0, 0, 0, 0, location)) {
		t.Fatalf("start = %v", start)
	}
	if !end.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, location)) {
		t.Fatalf("end = %v", end)
	}

	// January rollover.
	reference = time.Date(2026, 1, 1, 0, 0, 0, 0, location)
	start, _ = GetTrafficPeriod(28, reference)
	if !start.Equal(time.Date(2025, 12, 28, 0, 0, 0, 0, location)) {
		t.Fatalf("january start = %v", start)
	}

	// resetDay clamps to 1..28.
	reference = time.Date(2026, 10, 20, 0, 0, 0, 0, location)
	start, _ = GetTrafficPeriod(0, reference)
	if start.Day() != 1 {
		t.Fatalf("resetDay 0 must clamp to 1, got day %d", start.Day())
	}
	start, _ = GetTrafficPeriod(31, reference)
	if start.Day() != 28 {
		t.Fatalf("resetDay 31 must clamp to 28, got day %d", start.Day())
	}
}

// TestIsWarningSentThisMonth pins that a warning from the previous month (or the
// same month one year ago) does not suppress this month's warning.
func TestIsWarningSentThisMonth(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	if IsWarningSentThisMonth(nil, now) {
		t.Fatal("nil must be false")
	}

	sameMonth := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if !IsWarningSentThisMonth(&sameMonth, now) {
		t.Fatal("same month/year must be true")
	}

	lastMonth := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	if IsWarningSentThisMonth(&lastMonth, now) {
		t.Fatal("previous month must be false")
	}

	lastYear := time.Date(2025, 10, 15, 0, 0, 0, 0, time.UTC)
	if IsWarningSentThisMonth(&lastYear, now) {
		t.Fatal("same month last year must be false")
	}
}

func TestFormatLocalDate(t *testing.T) {
	moment := time.Date(2026, 3, 7, 23, 30, 0, 0, time.UTC)
	if got := FormatLocalDate(moment); got != "2026-03-07" {
		t.Fatalf("FormatLocalDate = %q", got)
	}

	// The same instant in Asia/Shanghai is already the 8th, which is why the
	// traffic routes use FormatShanghaiDate instead.
	if got := FormatShanghaiDate(moment); got != "2026-03-08" {
		t.Fatalf("FormatShanghaiDate = %q", got)
	}
}
