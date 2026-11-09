// Unit tests for the pure half of db/traffic.go. Everything that needs a database
// (locks, transactions, resets) is covered by the dual-run conformance suite in
// test/conformance/requests-traffic.json; these tests pin the arithmetic and the
// calendar boundaries that the TypeScript defines.
package db

import (
	"math"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

func TestTrafficAdvisoryLockNamespaces(t *testing.T) {
	// Cross-process contract with the Node backend (db/advisory-locks.ts). A
	// different value silently drops the mutual exclusion during the parallel run.
	if InstanceTrafficResetLockNamespace != 4121 {
		t.Fatalf("InstanceTrafficResetLockNamespace = %d, want 4121", InstanceTrafficResetLockNamespace)
	}
	if UserBalanceLockNamespace != 4110 {
		t.Fatalf("UserBalanceLockNamespace = %d, want 4110", UserBalanceLockNamespace)
	}
}

func TestShanghaiDateParts(t *testing.T) {
	cases := []struct {
		name  string
		input time.Time
		year  int
		month int
		day   int
	}{
		{
			name:  "before utc midnight is the previous shanghai hour but same day",
			input: time.Date(2026, 1, 1, 15, 30, 0, 0, time.UTC), // 23:30 Shanghai
			year:  2026, month: 0, day: 1,
		},
		{
			name:  "after 16:00 utc rolls into the next shanghai day",
			input: time.Date(2026, 1, 1, 16, 30, 0, 0, time.UTC), // 00:30 Shanghai on Jan 2
			year:  2026, month: 0, day: 2,
		},
		{
			name:  "new year rollover",
			input: time.Date(2025, 12, 31, 20, 0, 0, 0, time.UTC), // 04:00 Shanghai on Jan 1
			year:  2026, month: 0, day: 1,
		},
		{
			name:  "month is zero based like getUTCMonth",
			input: time.Date(2026, 7, 9, 2, 0, 0, 0, time.UTC), // 10:00 Shanghai, July
			year:  2026, month: 6, day: 9,
		},
	}
	for _, tc := range cases {
		year, month, day := ShanghaiDateParts(tc.input)
		if year != tc.year || month != tc.month || day != tc.day {
			t.Errorf("%s: got (%d,%d,%d), want (%d,%d,%d)", tc.name, year, month, day, tc.year, tc.month, tc.day)
		}
	}
}

func TestShanghaiDateOnly(t *testing.T) {
	// 2026-01-31T20:00Z is 2026-02-01 04:00 in Shanghai.
	day := ShanghaiDateOnly(time.Date(2026, 1, 31, 20, 0, 0, 0, time.UTC), 0)
	if want := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC); !day.Equal(want) {
		t.Fatalf("date-only = %s, want %s", day, want)
	}
	if day.Location() != time.UTC {
		t.Fatalf("date-only location = %s, want UTC", day.Location())
	}

	// Day offsets normalise across months (Date.UTC does the same).
	shifted := ShanghaiDateOnly(time.Date(2026, 1, 31, 20, 0, 0, 0, time.UTC), 1)
	if want := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC); !shifted.Equal(want) {
		t.Fatalf("shifted = %s, want %s", shifted, want)
	}
	negative := ShanghaiDateOnly(time.Date(2026, 3, 1, 4, 0, 0, 0, time.UTC), -3)
	if want := time.Date(2026, 2, 26, 0, 0, 0, 0, time.UTC); !negative.Equal(want) {
		t.Fatalf("negative shift = %s, want %s", negative, want)
	}
}

func TestStartOfMonthShanghai(t *testing.T) {
	// 2026-03-15T12:00Z → Shanghai March → 2026-03-01 00:00+08 = 2026-02-28T16:00Z.
	got := StartOfMonthShanghai(time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC))
	want := time.Date(2026, 2, 28, 16, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("start of month = %s, want %s", got, want)
	}

	// An instant that is still February in UTC but March in Shanghai must use March.
	got = StartOfMonthShanghai(time.Date(2026, 2, 28, 20, 0, 0, 0, time.UTC))
	if !got.Equal(want) {
		t.Fatalf("start of shanghai month = %s, want %s", got, want)
	}
}

func TestTrafficPrismaDateParam(t *testing.T) {
	// Prisma's @db.Date binding takes the UTC date part of the instant
	// (adapter-pg formatDate → getUTC*). A local-midnight period start in a UTC+8
	// process therefore compares against the previous calendar day — the quirk
	// getDailyTrafficByPeriod reproduces.
	local := time.FixedZone("CST", 8*3600)
	periodStart := time.Date(2026, 3, 1, 0, 0, 0, 0, local) // 2026-02-28T16:00Z
	got := TrafficPrismaDateParam(periodStart)
	if want := time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("date param = %s, want %s", got, want)
	}

	// A UTC-midnight instant (the stored daily_traffic canonical value) keeps its
	// own date.
	canonical := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if got := TrafficPrismaDateParam(canonical); !got.Equal(canonical) {
		t.Fatalf("canonical date param = %s, want %s", got, canonical)
	}
}

func TestTrafficNormalizeMoney(t *testing.T) {
	cases := []struct {
		input float64
		want  float64
	}{
		{0, 0},
		{1.1, 1.1},
		{12.5, 12.5},
		{2.675, 2.67}, // V8: (2.675).toFixed(2) === "2.67"
		{1.005, 1},    // V8: (1.005).toFixed(2) === "1.00"
		{0.005, 0.01}, // the binary value is just above 0.005
		{-1.005, -1},  // toFixed rounds the absolute value, then applies the sign
		{-2.675, -2.67},
		{99.999, 100},
	}
	for _, tc := range cases {
		if got := trafficNormalizeMoney(tc.input); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("normalizeMoney(%v) = %v, want %v", tc.input, got, tc.want)
		}
	}

	if value := trafficNormalizeMoney(math.NaN()); !math.IsNaN(value) {
		t.Errorf("NaN normalised to %v", value)
	}
	if value := trafficNormalizeMoney(math.Inf(1)); !math.IsInf(value, 1) {
		t.Errorf("+Inf normalised to %v", value)
	}
}

func TestTrafficLimitHelpers(t *testing.T) {
	limit := int64(100)

	effective := trafficGetEffectiveLimit(&limit, 50)
	if effective == nil || *effective != 150 {
		t.Fatalf("effective limit = %v, want 150", effective)
	}
	if trafficGetEffectiveLimit(nil, 50) != nil {
		t.Fatalf("unlimited must stay unlimited")
	}

	if trafficIsOverLimit(99, &limit) {
		t.Fatalf("99/100 must not be over limit")
	}
	if !trafficIsOverLimit(100, &limit) {
		t.Fatalf("100/100 must be over limit")
	}
	if trafficIsOverLimit(math.MaxInt64, nil) {
		t.Fatalf("unlimited must never be over limit")
	}

	// 80% threshold uses bigint arithmetic: 80*80/100 = 64.
	if trafficIsWarningThreshold(63, &limit) {
		t.Fatalf("63/100 must be below the warning threshold")
	}
	if !trafficIsWarningThreshold(80, &limit) {
		t.Fatalf("80/100 must hit the warning threshold")
	}
	if trafficIsWarningThreshold(math.MaxInt64, nil) {
		t.Fatalf("unlimited must never warn")
	}

	if status := trafficCalculateUserStatus(0, nil); status != models.TrafficStatusNORMAL {
		t.Fatalf("unlimited user status = %s", status)
	}
	if status := trafficCalculateUserStatus(80, &limit); status != models.TrafficStatusWARNING {
		t.Fatalf("80%% user status = %s", status)
	}
	if status := trafficCalculateUserStatus(100, &limit); status != models.TrafficStatusLIMITED {
		t.Fatalf("100%% user status = %s", status)
	}
	if status := trafficCalculateUserStatus(0, nil); status != models.TrafficStatusNORMAL {
		t.Fatalf("unlimited instance status = %s", status)
	}
	if status := trafficCalculateInstanceStatus(80, &limit); status != models.TrafficStatusNORMAL {
		t.Fatalf("instance has no warning tier, got %s", status)
	}
	if status := trafficCalculateInstanceStatus(100, &limit); status != models.TrafficStatusLIMITED {
		t.Fatalf("instance at limit = %s", status)
	}
	// 0-byte limit: `used >= limit` is true even at zero, exactly like the bigint
	// comparison in the TypeScript.
	zero := int64(0)
	if status := trafficCalculateUserStatus(0, &zero); status != models.TrafficStatusLIMITED {
		t.Fatalf("zero limit status = %s", status)
	}
}

func TestTrafficSanitizeBigInt(t *testing.T) {
	if value, err := trafficSanitizeBigInt(0); err != nil || value != 0 {
		t.Fatalf("sanitize(0) = (%d, %v)", value, err)
	}
	if _, err := trafficSanitizeBigInt(-1); err == nil {
		t.Fatalf("sanitize(-1) must fail")
	}
}

func TestTrafficLockKeys(t *testing.T) {
	if got := trafficInstanceLockKey(42); got != "traffic:instance:42" {
		t.Fatalf("instance lock key = %q", got)
	}
	if got := trafficUserLockKey(7); got != "traffic:user:7" {
		t.Fatalf("user lock key = %q", got)
	}
	if trafficLockOwnerID() == "" {
		t.Fatalf("lock owner must not be empty")
	}
	if trafficLockOwnerID() == trafficLockOwnerID() {
		t.Fatalf("lock owner must be unique per acquisition")
	}
}

func TestNormalizeTrafficMultiplier(t *testing.T) {
	cases := []struct {
		name    string
		input   any
		want    float64
		wantErr bool
	}{
		{name: "nil defaults to 1", input: nil, want: 1},
		{name: "empty string defaults to 1", input: "", want: 1},
		{name: "numeric", input: float64(1.5), want: 1.5},
		{name: "string", input: "2.5", want: 2.5},
		{name: "rounded to three decimals", input: 1.23456, want: 1.235},
		{name: "minimum", input: 0.001, want: 0.001},
		{name: "maximum", input: 100.0, want: 100},
		{name: "below minimum", input: 0.0009, wantErr: true},
		{name: "above maximum", input: 100.1, wantErr: true},
		{name: "garbage", input: "abc", wantErr: true},
	}
	for _, tc := range cases {
		got, err := NormalizeTrafficMultiplier(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected an error, got %v", tc.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestApplyTrafficMultiplier(t *testing.T) {
	limit := int64(1000)
	multiplied, err := ApplyTrafficMultiplier(&limit, 1.5)
	if err != nil || multiplied == nil || *multiplied != 1500 {
		t.Fatalf("apply(1000, 1.5) = %v, %v", multiplied, err)
	}

	// `(limit * scaled) / 1000n` truncates toward zero.
	odd := int64(999)
	truncated, err := ApplyTrafficMultiplier(&odd, 0.5)
	if err != nil || truncated == nil || *truncated != 499 {
		t.Fatalf("apply(999, 0.5) = %v, %v", truncated, err)
	}

	if result, err := ApplyTrafficMultiplier(nil, 1.5); err != nil || result != nil {
		t.Fatalf("a null limit must stay unlimited, got %v, %v", result, err)
	}

	if _, err := ApplyTrafficMultiplier(&limit, 0.0001); err == nil {
		t.Fatalf("invalid multiplier must fail")
	}
}

func TestInstanceTrafficInfoTrafficResetPriceYuan(t *testing.T) {
	planPrice := 5.5
	if got := (InstanceTrafficInfo{PackagePlanTrafficResetPrice: &planPrice, PackageTrafficResetPrice: 99}).TrafficResetPriceYuan(); got != 5.5 {
		t.Fatalf("plan price = %v, want 5.5", got)
	}

	// A NULL plan price falls back to the package price (and is normalised to 2dp).
	if got := (InstanceTrafficInfo{PackageTrafficResetPrice: 3.333}).TrafficResetPriceYuan(); got != 3.33 {
		t.Fatalf("package price = %v, want 3.33", got)
	}

	// Math.max(0, ...) clamps negative prices.
	if got := (InstanceTrafficInfo{PackageTrafficResetPrice: -2}).TrafficResetPriceYuan(); got != 0 {
		t.Fatalf("negative price = %v, want 0", got)
	}

	if got := (InstanceTrafficInfo{}).TrafficResetPriceYuan(); got != 0 {
		t.Fatalf("missing price = %v, want 0", got)
	}
}
