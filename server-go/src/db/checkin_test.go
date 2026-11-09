package db

import (
	"math"
	"testing"
	"time"
)

// ==================== date helpers ====================

// TestCheckinDateKeyInBeijing pins getBeijingDateKey: YYYY-MM-DD in Asia/Shanghai,
// i.e. the day rolls over at 16:00 UTC.
func TestCheckinDateKeyInBeijing(t *testing.T) {
	cases := []struct {
		moment time.Time
		want   string
	}{
		{time.Date(2026, 1, 1, 15, 59, 59, 0, time.UTC), "2026-01-01"},
		{time.Date(2026, 1, 1, 16, 0, 0, 0, time.UTC), "2026-01-02"},
		{time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), "2027-01-01"},
		{time.Date(2026, 6, 15, 4, 0, 0, 0, time.UTC), "2026-06-15"},
	}
	for _, tc := range cases {
		if got := CheckinDateKeyInBeijing(tc.moment); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.moment, got, tc.want)
		}
	}
}

// TestCheckinYesterdayDateKey pins getYesterdayDateKey, including month and year
// boundaries.
func TestCheckinYesterdayDateKey(t *testing.T) {
	cases := map[string]string{
		"2026-03-01": "2026-02-28",
		"2024-03-01": "2024-02-29", // leap year
		"2026-01-01": "2025-12-31",
		"2026-07-15": "2026-07-14",
	}
	for dateKey, want := range cases {
		if got := checkinYesterdayDateKey(dateKey); got != want {
			t.Fatalf("%s: got %q, want %q", dateKey, got, want)
		}
	}
}

// ==================== randomization ====================

// TestCheckinRandomizePoints pins randomizePoints:
// Math.floor(Math.random() * (max - min + 1)) + min.
func TestCheckinRandomizePoints(t *testing.T) {
	original := checkinRandomFloat
	defer func() { checkinRandomFloat = original }()

	checkinRandomFloat = func() float64 { return 0 }
	if got := checkinRandomizePoints(1, 500); got != 1 {
		t.Fatalf("random=0: got %d", got)
	}

	checkinRandomFloat = func() float64 { return 0.9999999999 }
	if got := checkinRandomizePoints(1, 500); got != 500 {
		t.Fatalf("random~1: got %d", got)
	}

	checkinRandomFloat = func() float64 { return 0.5 }
	if got := checkinRandomizePoints(10, 20); got != 15 {
		t.Fatalf("random=0.5: got %d", got)
	}

	// Single-value range: floor(random*1)+min is always min.
	checkinRandomFloat = func() float64 { return 0.99 }
	if got := checkinRandomizePoints(42, 42); got != 42 {
		t.Fatalf("single-value range: got %d", got)
	}
}

// ==================== string helpers ====================

// TestCheckinJSSlice pins the UTF-16 slice semantics of String.prototype.slice.
func TestCheckinJSSlice(t *testing.T) {
	cases := []struct {
		value string
		limit int
		want  string
	}{
		{"hello", 0, ""},
		{"hello", 3, "hel"},
		{"hello", 10, "hello"},
		{"签到成功", 2, "签到"},
		{"a😀b", 1, "a"},
		{"a😀b", 2, "a"}, // the surrogate pair does not fit (orphan dropped, documented)
		{"a😀b", 3, "a😀"},
	}
	for _, tc := range cases {
		if got := CheckinJSSlice(tc.value, tc.limit); got != tc.want {
			t.Fatalf("%q[%d]: got %q, want %q", tc.value, tc.limit, got, tc.want)
		}
	}
}

// TestCheckinNullableSlicedText pins `value?.slice(0,n) || null`.
func TestCheckinNullableSlicedText(t *testing.T) {
	if got := checkinNullableSlicedText(nil, 10); got != nil {
		t.Fatalf("nil: %v", got)
	}
	empty := ""
	if got := checkinNullableSlicedText(&empty, 10); got != nil {
		t.Fatalf("empty must become null: %v", *got)
	}
	long := "0123456789"
	if got := checkinNullableSlicedText(&long, 4); got == nil || *got != "0123" {
		t.Fatalf("sliced: %v", got)
	}
}

// ==================== number formatting ====================

// TestCheckinFormatInt64 pins String(bigint) for the pool/traffic values.
func TestCheckinFormatInt64(t *testing.T) {
	cases := map[int64]string{
		0:             "0",
		1:             "1",
		1073741824:    "1073741824",
		-42:           "-42",
		math.MaxInt64: "9223372036854775807",
		math.MinInt64: "-9223372036854775808",
	}
	for value, want := range cases {
		if got := CheckinFormatInt64(value); got != want {
			t.Fatalf("%d: got %q, want %q", value, got, want)
		}
	}
}

// TestCheckinFormatInt pins the int flavour used in log remarks.
func TestCheckinFormatInt(t *testing.T) {
	cases := map[int]string{0: "0", 7: "7", -7: "-7", 100: "100"}
	for value, want := range cases {
		if got := CheckinFormatInt(value); got != want {
			t.Fatalf("%d: got %q, want %q", value, got, want)
		}
	}
}

// ==================== wire timestamps ====================

// TestCheckinISOTime pins the Date.toISOString() shape used on the wire.
func TestCheckinISOTime(t *testing.T) {
	moment := time.Date(2027, 1, 2, 3, 4, 5, 678_000_000, time.UTC)
	if got := checkinISOTime(moment); got != "2027-01-02T03:04:05.678Z" {
		t.Fatalf("got %q", got)
	}
	offset := time.FixedZone("CST", 8*60*60)
	local := time.Date(2027, 1, 2, 11, 4, 5, 678_000_000, offset)
	if got := checkinISOTime(local); got != "2027-01-02T03:04:05.678Z" {
		t.Fatalf("offset time: got %q", got)
	}
}
