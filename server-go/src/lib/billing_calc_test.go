package lib

import (
	"math"
	"testing"
	"time"
)

// TestJSToFixedMatchesNode pins the exact `Number(x.toFixed(d))` results measured
// against real V8. Every expectation below was produced by node, not derived.
//
// The interesting rows are the ties: V8 rounds the EXACT binary value with ties
// away from zero, so 1.005 → 1 (its double is 1.00499999…) while 0.125 → 0.13
// (exactly representable, a real tie). Formatting the float with strconv would
// round ties to even and get several of these wrong.
func TestJSToFixedMatchesNode(t *testing.T) {
	cases := []struct {
		value  float64
		digits int
		want   float64
	}{
		{1.005, 2, 1},
		{2.675, 2, 2.67},
		{-1.005, 2, -1},
		{0.125, 2, 0.13},
		{-0.125, 2, -0.13},
		{2.125, 2, 2.13},
		{-2.125, 2, -2.13},
		{1234.565, 2, 1234.57},
		{-1234.565, 2, -1234.57},
		{0.005, 2, 0.01},
		{100.005, 2, 100},
		{1.45, 1, 1.4},
		{1.005, 4, 1.005},
		{1.00005, 4, 1.0001},
		{0.1 + 0.2, 2, 0.3},
		{1e21, 2, 1e21},
		{1.7976931348623157e308, 2, 1.7976931348623157e308},
		{0, 2, 0},
		{-0, 2, 0},
	}

	for _, tc := range cases {
		got := jsToFixed(tc.value, tc.digits)
		if got != tc.want {
			t.Errorf("jsToFixed(%v, %d) = %v, want %v", tc.value, tc.digits, got, tc.want)
		}
	}
}

// TestJSToFixedKeepsNegativeZero pins that -0.001 → -0, so callers that stringify
// the result see the same "-0" Node would... except JSON.stringify(-0) is "0", so
// the sign is observable only through math.Signbit, which is what the statistics
// route relies on.
func TestJSToFixedKeepsNegativeZero(t *testing.T) {
	got := jsToFixed(-0.001, 2)
	if got != 0 {
		t.Fatalf("value = %v, want 0", got)
	}
	if !math.Signbit(got) {
		t.Fatal("expected negative zero, matching Number((-0.001).toFixed(2))")
	}
}

// TestGetCycleDays pins the fixed 31-day month table and the fallback.
func TestGetCycleDays(t *testing.T) {
	cases := map[int]int{1: 31, 3: 93, 6: 186, 12: 372, 2: 62, 0: 0, 24: 744}
	for cycle, want := range cases {
		if got := GetCycleDays(cycle); got != want {
			t.Errorf("GetCycleDays(%d) = %d, want %d", cycle, got, want)
		}
	}
}

// TestCalculateMonthlyPriceGuardsZero pins `Math.max(billingCycle || 1, 1)`.
func TestCalculateMonthlyPriceGuardsZero(t *testing.T) {
	if got := CalculateMonthlyPrice(100, 0); got != 100 {
		t.Fatalf("cycle 0 = %v, want 100", got)
	}
	if got := CalculateMonthlyPrice(100, -5); got != 100 {
		t.Fatalf("negative cycle = %v, want 100", got)
	}
	if got := CalculateMonthlyPrice(93, 3); got != 31 {
		t.Fatalf("cycle 3 = %v, want 31", got)
	}
}

// TestCalculateRemainingDays pins the ceiling and the floor at zero.
func TestCalculateRemainingDays(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	if got := CalculateRemainingDays(now.Add(24*time.Hour), now); got != 1 {
		t.Fatalf("exactly one day = %d, want 1", got)
	}
	if got := CalculateRemainingDays(now.Add(24*time.Hour+time.Millisecond), now); got != 2 {
		t.Fatalf("just over one day = %d, want 2 (ceil)", got)
	}
	if got := CalculateRemainingDays(now.Add(-time.Hour), now); got != 0 {
		t.Fatalf("expired = %d, want 0", got)
	}
	if got := CalculateRemainingDays(now.Add(25*time.Hour), now); got != 2 {
		t.Fatalf("25h = %d, want 2", got)
	}
}

// TestCalculateRemainingDaysPrecise pins the fractional variant.
func TestCalculateRemainingDaysPrecise(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if got := CalculateRemainingDaysPrecise(now.Add(12*time.Hour), now); got != 0.5 {
		t.Fatalf("12h = %v, want 0.5", got)
	}
	if got := CalculateRemainingDaysPrecise(now.Add(-time.Hour), now); got != 0 {
		t.Fatalf("expired = %v, want 0", got)
	}
}

// TestCalculateRemainingValue pins the discount path and the 2-decimal rounding.
func TestCalculateRemainingValue(t *testing.T) {
	// 31 元 / 31 days = 1 元/day, 10 days left.
	if got := CalculateRemainingValue(31, 1, 10, 0); got != 10 {
		t.Fatalf("no discount = %v, want 10", got)
	}
	// 10% off 31 = 27.9; 27.9/31 * 10 = 9.0.
	if got := CalculateRemainingValue(31, 1, 10, 0.1); got != 9 {
		t.Fatalf("10%% off = %v, want 9", got)
	}
}

// TestCalculateSafeRefundAmountClampsToPaid pins the cap.
func TestCalculateSafeRefundAmountClampsToPaid(t *testing.T) {
	max := 5.0
	if got := CalculateSafeRefundAmount(31, 1, 10, 0, &max); got != 5 {
		t.Fatalf("capped = %v, want 5", got)
	}
	bigger := 100.0
	if got := CalculateSafeRefundAmount(31, 1, 10, 0, &bigger); got != 10 {
		t.Fatalf("uncapped = %v, want 10", got)
	}
	if got := CalculateSafeRefundAmount(31, 1, 10, 0, nil); got != 10 {
		t.Fatalf("nil cap = %v, want 10", got)
	}
}

// TestCalculatePriceDiffSubCentIsZero pins the 0.01 threshold.
func TestCalculatePriceDiffSubCentIsZero(t *testing.T) {
	// Same price and cycle: the difference is exactly 0.
	if got := CalculatePriceDiff(31, 1, 31, 1, 10, 0); got != 0 {
		t.Fatalf("same plan = %v, want 0", got)
	}
	// Upgrading doubles the daily price: (2-1)*10 = 10.
	if got := CalculatePriceDiff(31, 1, 62, 1, 10, 0); got != 10 {
		t.Fatalf("upgrade = %v, want 10", got)
	}
	// A change too small to reach one cent collapses to 0.
	if got := CalculatePriceDiff(31, 1, 31.2, 1, 0.1, 0); got != 0 {
		t.Fatalf("sub-cent = %v, want 0", got)
	}
}

// TestCalculatePlanChangeDetailsPinsCreditCap pins that the upgrade credit never
// exceeds what was actually paid.
func TestCalculatePlanChangeDetailsPinsCreditCap(t *testing.T) {
	// 31 元/mo, 10 days left. Paid remaining 10, but only 3 is still refundable.
	result := CalculatePlanChangeDetails(31, 1, 62, 1, 10, 0, 10, 3)

	if result.RemainingValue != 3 {
		t.Fatalf("remainingValue = %v, want 3 (capped by maxPaidAmount)", result.RemainingValue)
	}
	// New plan costs 2/day * 10 = 20; 20 - 3 = 17.
	if result.NewPlanCost != 20 {
		t.Fatalf("newPlanCost = %v, want 20", result.NewPlanCost)
	}
	if result.PriceDiff != 17 {
		t.Fatalf("priceDiff = %v, want 17", result.PriceDiff)
	}
	if !result.IsUpgrade {
		t.Fatal("doubling the daily price is an upgrade")
	}
	// Daily prices are reported to 4 decimals.
	if result.OldDailyPrice != 1 || result.NewDailyPrice != 2 {
		t.Fatalf("daily prices = %v / %v, want 1 / 2", result.OldDailyPrice, result.NewDailyPrice)
	}
}

// TestCalculatePlanChangeDetailsNegativeCreditClamped pins `Math.max(0, x)` on
// both credit inputs.
func TestCalculatePlanChangeDetailsNegativeCreditClamped(t *testing.T) {
	result := CalculatePlanChangeDetails(31, 1, 31, 1, 10, 0, -50, -50)
	if result.RemainingValue != 0 {
		t.Fatalf("remainingValue = %v, want 0", result.RemainingValue)
	}
	if result.PriceDiff != 10 {
		t.Fatalf("priceDiff = %v, want 10", result.PriceDiff)
	}
	if result.IsUpgrade {
		t.Fatal("equal daily prices are not an upgrade")
	}
}

// TestCalculateRenewAmount pins the discount arithmetic.
func TestCalculateRenewAmount(t *testing.T) {
	plain := CalculateRenewAmount(31, 3, 0)
	if plain.OriginalAmount != 93 || plain.DiscountAmount != 0 || plain.FinalAmount != 93 {
		t.Fatalf("no discount = %#v", plain)
	}

	discounted := CalculateRenewAmount(31, 3, 0.1)
	if discounted.OriginalAmount != 93 {
		t.Fatalf("original = %v, want 93", discounted.OriginalAmount)
	}
	// 93 * 0.1 = 9.3 exactly.
	if discounted.DiscountAmount != 9.3 {
		t.Fatalf("discount = %v, want 9.3", discounted.DiscountAmount)
	}
	if discounted.FinalAmount != 83.7 {
		t.Fatalf("final = %v, want 83.7", discounted.FinalAmount)
	}
}

// TestAddMonthsUsesThirtyOneDayMonths pins the deliberate 31-day month: a year is
// 372 days, NOT a calendar year.
func TestAddMonthsUsesThirtyOneDayMonths(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if got := AddMonths(base, 1); !got.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("+1 month = %v", got)
	}
	// 12 * 31 = 372 days, which lands on 2027-01-08, not 2027-01-01.
	if got := AddMonths(base, 12); !got.Equal(time.Date(2027, 1, 8, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("+12 months = %v, want 372 days later", got)
	}
}
