// Port of server/src/lib/billing-calc.ts.
//
// The single source of truth for price, daily-price, proration and remaining-value
// arithmetic. Every money value here is a JavaScript `number` in YUAN, produced by
// the same `Number(x.toFixed(n))` rounding the TypeScript uses, so the results are
// bit-comparable with Node rather than merely close.
//
// That rounding detail matters. `toFixed` rounds the exact binary value of the
// double with ties away from zero; Go's `math.Round` rounds half away from zero on
// the same value but `strconv.FormatFloat`-based formatting rounds half to even.
// `jsFixed` below reproduces V8's algorithm instead of approximating it, which is
// why `1.005` becomes `1.00` here (the double is 1.00499999…) exactly as in Node.
package lib

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// billingCycleDays mirrors CYCLE_DAYS: one "month" is always 31 days.
var billingCycleDays = map[int]int{
	1:  31,
	3:  93,
	6:  186,
	12: 372,
}

// GetCycleDays mirrors getCycleDays: a known cycle uses the table, anything else
// falls back to `billingCycle * 31`.
func GetCycleDays(billingCycle int) int {
	if days, ok := billingCycleDays[billingCycle]; ok {
		return days
	}
	return billingCycle * 31
}

// CalculateDailyPrice mirrors calculateDailyPrice.
func CalculateDailyPrice(price float64, billingCycle int) float64 {
	return price / float64(GetCycleDays(billingCycle))
}

// CalculateMonthlyPrice mirrors calculateMonthlyPrice: a zero/negative cycle is
// treated as 1 to avoid dividing by zero.
func CalculateMonthlyPrice(price float64, billingCycle int) float64 {
	cycle := billingCycle
	if cycle < 1 {
		cycle = 1
	}
	return price / float64(cycle)
}

// CalculateDiscountedPrice mirrors calculateDiscountedPrice.
func CalculateDiscountedPrice(originalPrice, discountRate float64) float64 {
	return jsToFixed(originalPrice*(1-discountRate), 2)
}

// CalculateDiscountAmount mirrors calculateDiscountAmount.
func CalculateDiscountAmount(originalPrice, discountRate float64) float64 {
	return jsToFixed(originalPrice*discountRate, 2)
}

// CalculateRemainingDays mirrors calculateRemainingDays: positive milliseconds
// rounded UP to whole days, floored at 0.
func CalculateRemainingDays(expiresAt, now time.Time) int {
	remainingMs := expiresAt.Sub(now).Milliseconds()
	if remainingMs <= 0 {
		return 0
	}
	return int(math.Ceil(float64(remainingMs) / float64(24*60*60*1000)))
}

// CalculateRemainingDaysPrecise mirrors calculateRemainingDaysPrecise: the same
// value without the ceiling, floored at 0.
func CalculateRemainingDaysPrecise(expiresAt, now time.Time) float64 {
	remainingMs := expiresAt.Sub(now).Milliseconds()
	if remainingMs <= 0 {
		return 0
	}
	return float64(remainingMs) / float64(24*60*60*1000)
}

// CalculateRemainingValue mirrors calculateRemainingValue: the discounted daily
// price times the remaining days.
func CalculateRemainingValue(price float64, billingCycle int, remainingDays float64, discountRate float64) float64 {
	effectivePrice := price
	if discountRate > 0 {
		effectivePrice = CalculateDiscountedPrice(price, discountRate)
	}
	dailyPrice := CalculateDailyPrice(effectivePrice, billingCycle)
	return jsToFixed(dailyPrice*remainingDays, 2)
}

// CalculateSafeRefundAmount mirrors calculateSafeRefundAmount: the remaining value
// can never exceed what was actually paid.
func CalculateSafeRefundAmount(
	price float64,
	billingCycle int,
	remainingDays float64,
	discountRate float64,
	maxRefundable *float64,
) float64 {
	remainingValue := CalculateRemainingValue(price, billingCycle, remainingDays, discountRate)
	if maxRefundable != nil && remainingValue > *maxRefundable {
		return jsToFixed(*maxRefundable, 2)
	}
	return remainingValue
}

// CalculatePriceDiff mirrors calculatePriceDiff: (new daily − old daily) × days,
// with anything under 0.01 treated as zero.
func CalculatePriceDiff(
	oldPrice float64,
	oldBillingCycle int,
	newPrice float64,
	newBillingCycle int,
	remainingDays float64,
	discountRate float64,
) float64 {
	actualOldPrice := oldPrice
	if discountRate > 0 {
		actualOldPrice = CalculateDiscountedPrice(oldPrice, discountRate)
	}
	actualNewPrice := newPrice
	if discountRate > 0 {
		actualNewPrice = CalculateDiscountedPrice(newPrice, discountRate)
	}

	oldDailyPrice := CalculateDailyPrice(actualOldPrice, oldBillingCycle)
	newDailyPrice := CalculateDailyPrice(actualNewPrice, newBillingCycle)

	priceDiff := (newDailyPrice - oldDailyPrice) * remainingDays
	if math.Abs(priceDiff) < 0.01 {
		return 0
	}
	return jsToFixed(priceDiff, 2)
}

// PlanChangeCalcResult mirrors PlanChangeCalcResult.
type PlanChangeCalcResult struct {
	OldDailyPrice  float64
	NewDailyPrice  float64
	RemainingValue float64
	NewPlanCost    float64
	DiscountAmount float64
	PriceDiff      float64
	IsUpgrade      bool
}

// CalculatePlanChangeDetails mirrors calculatePlanChangeDetails.
//
// The upgrade credit only recognises the unused value of what was actually paid,
// capped by the still-refundable total. paidRemainingValue / maxPaidAmount come
// from the same billing quote used for destruction refunds, so this function never
// invents credit.
func CalculatePlanChangeDetails(
	oldPrice float64,
	oldBillingCycle int,
	newPrice float64,
	newBillingCycle int,
	remainingDays float64,
	discountRate float64,
	paidRemainingValue float64,
	maxPaidAmount float64,
) PlanChangeCalcResult {
	actualOldPrice := oldPrice
	if discountRate > 0 {
		actualOldPrice = CalculateDiscountedPrice(oldPrice, discountRate)
	}
	actualNewPrice := newPrice
	if discountRate > 0 {
		actualNewPrice = CalculateDiscountedPrice(newPrice, discountRate)
	}

	oldDailyPrice := CalculateDailyPrice(actualOldPrice, oldBillingCycle)
	newDailyPrice := CalculateDailyPrice(actualNewPrice, newBillingCycle)

	remainingValue := math.Min(
		jsToFixed(math.Max(0, paidRemainingValue), 2),
		jsToFixed(math.Max(0, maxPaidAmount), 2),
	)

	newPlanCost := jsToFixed(newDailyPrice*remainingDays, 2)

	var discountAmount float64
	if discountRate > 0 {
		discountAmount = jsToFixed(
			(newPrice/float64(GetCycleDays(newBillingCycle)))*remainingDays*discountRate, 2)
	}

	priceDiff := jsToFixed(newPlanCost-remainingValue, 2)
	finalPriceDiff := priceDiff
	if math.Abs(priceDiff) < 0.01 {
		finalPriceDiff = 0
	}

	return PlanChangeCalcResult{
		OldDailyPrice:  jsToFixed(oldDailyPrice, 4),
		NewDailyPrice:  jsToFixed(newDailyPrice, 4),
		RemainingValue: remainingValue,
		NewPlanCost:    newPlanCost,
		DiscountAmount: discountAmount,
		PriceDiff:      finalPriceDiff,
		IsUpgrade:      newDailyPrice > oldDailyPrice,
	}
}

// RenewAmountResult mirrors the object returned by calculateRenewAmount.
type RenewAmountResult struct {
	OriginalAmount float64
	DiscountAmount float64
	FinalAmount    float64
}

// CalculateRenewAmount mirrors calculateRenewAmount.
func CalculateRenewAmount(monthlyPrice float64, months int, discountRate float64) RenewAmountResult {
	originalAmount := jsToFixed(monthlyPrice*float64(months), 2)

	var discountAmount float64
	if discountRate > 0 {
		discountAmount = jsToFixed(originalAmount*discountRate, 2)
	}

	return RenewAmountResult{
		OriginalAmount: originalAmount,
		DiscountAmount: discountAmount,
		FinalAmount:    jsToFixed(originalAmount-discountAmount, 2),
	}
}

// AddMonths mirrors addMonths: month arithmetic is 31-day arithmetic everywhere in
// this codebase, including for `expiresAt`. That is deliberate (a "month" is 31
// days, so a year is 372), so it is NOT time.AddDate(0, n, 0).
func AddMonths(date time.Time, months int) time.Time {
	return date.AddDate(0, 0, months*31)
}

// jsToFixed reproduces JavaScript's `Number(value.toFixed(digits))`.
//
// Algorithm (ECMA-262 Number.prototype.toFixed, then Number()): the exact binary
// value of the double is rounded to `digits` decimals with ties away from zero,
// then parsed back. Formatting the float with strconv would round ties to even, so
// the exact decimal expansion is used instead.
func jsToFixed(value float64, digits int) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return value
	}
	// JS switches to exponential notation at 1e21, where toFixed stops applying and
	// the value is returned unchanged.
	if math.Abs(value) >= 1e21 {
		return value
	}

	negative := math.Signbit(value)
	absolute := math.Abs(value)

	// Format with enough decimals to represent the double exactly, then round
	// manually. 30 significant decimals covers every double this module can see.
	exact := strconv.FormatFloat(absolute, 'f', 40, 64)

	integerPart := exact
	fractionPart := ""
	if dot := strings.IndexByte(exact, '.'); dot >= 0 {
		integerPart = exact[:dot]
		fractionPart = exact[dot+1:]
	}

	// Pad so the digit at position `digits` and the ones after it exist.
	for len(fractionPart) < digits+1 {
		fractionPart += "0"
	}

	kept := fractionPart[:digits]
	roundDigit := fractionPart[digits]

	roundedDigits := kept
	if roundDigit >= '5' {
		// Propagate the carry through the kept fraction, then the integer part.
		carry := true
		buffer := []byte(kept)
		for index := len(buffer) - 1; index >= 0 && carry; index-- {
			if buffer[index] == '9' {
				buffer[index] = '0'
			} else {
				buffer[index]++
				carry = false
			}
		}
		roundedDigits = string(buffer)
		if carry {
			// The fraction overflowed (all nines); carry into the integer part.
			integerDigits := []byte(integerPart)
			carry = true
			for index := len(integerDigits) - 1; index >= 0 && carry; index-- {
				if integerDigits[index] == '9' {
					integerDigits[index] = '0'
				} else {
					integerDigits[index]++
					carry = false
				}
			}
			if carry {
				integerDigits = append([]byte{'1'}, integerDigits...)
			}
			integerPart = string(integerDigits)
			roundedDigits = strings.Repeat("0", digits)
		}
	}

	assembled := integerPart
	if digits > 0 {
		assembled += "." + roundedDigits
	}

	parsed, err := strconv.ParseFloat(assembled, 64)
	if err != nil {
		return value
	}
	if negative {
		return -parsed
	}
	return parsed
}

// JSFixed is the exported spelling of jsToFixed for the modules that need the same
// rounding rule (money projections that live outside this file).
func JSFixed(value float64, digits int) float64 { return jsToFixed(value, digits) }
