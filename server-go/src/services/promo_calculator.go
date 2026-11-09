// Port of server/src/services/promo/promo-calculator.ts.
//
// Pure arithmetic: no database access, no I/O. Every function mirrors one
// TypeScript export and keeps the exact JavaScript evaluation order, because
// floating-point money is order sensitive:
//
//	Number(x.toFixed(2))  ->  db.PromoMoneyFixed2(x)
//	Math.max(0, ...)      ->  math.Max(0, ...)
//	Math.min(a, b)        ->  math.Min(a, b)
//
// `toFixed(2)` rounds the *exact* binary value of the double (ties away from
// zero), which is what db.PromoMoneyFixed2 reproduces — see promo_codes.go.
//
// Exports are Promo-prefixed because the services package is shared by parallel
// module ports.
package services

import (
	"math"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// PromoCreationQuoteParams mirrors CreationQuoteParams.
type PromoCreationQuoteParams struct {
	OriginalPrice float64
	DiscountType  models.PromoDiscountType
	DiscountValue float64
}

// PromoCreationQuoteResult mirrors CreationQuoteResult.
type PromoCreationQuoteResult struct {
	OriginalPrice  float64 `json:"originalPrice"`
	DiscountAmount float64 `json:"discountAmount"`
	FinalPrice     float64 `json:"finalPrice"`
}

// PromoRenewalSplitParams mirrors RenewalSplitParams. A nil RemainingCycles means
// FOREVER (the TypeScript uses `null` for the same meaning).
type PromoRenewalSplitParams struct {
	MonthlyPrice    float64
	Months          int
	DiscountRate    float64
	RemainingCycles *int32
}

// PromoRenewalSplitResult mirrors RenewalSplitResult.
type PromoRenewalSplitResult struct {
	DiscountedMonths   int     `json:"discountedMonths"`
	RegularMonths      int     `json:"regularMonths"`
	DiscountAmount     float64 `json:"discountAmount"`
	FinalAmount        float64 `json:"finalAmount"`
	ConsumedCycles     int     `json:"consumedCycles"`
	NewRemainingCycles *int32  `json:"newRemainingCycles"`
	WillUnbind         bool    `json:"willUnbind"`
}

// PromoCalculateFinalPrice mirrors calculateFinalPrice:
//
//	Math.max(0, Number((originalPrice - discountAmount).toFixed(2)))
//
// The subtraction happens in float64 first, exactly like the TypeScript.
func PromoCalculateFinalPrice(originalPrice, discountAmount float64) float64 {
	return math.Max(0, db.PromoMoneyFixed2(originalPrice-discountAmount))
}

// PromoCalculateCreationQuote mirrors calculateCreationQuote. A discount type
// other than PERCENTAGE / FIXED_AMOUNT yields no discount, like the TypeScript
// `let discountAmount = 0` that only the two branches overwrite.
func PromoCalculateCreationQuote(params PromoCreationQuoteParams) PromoCreationQuoteResult {
	discountAmount := 0.0

	switch params.DiscountType {
	case models.PromoDiscountTypePERCENTAGE:
		discountAmount = db.PromoMoneyFixed2(params.OriginalPrice * params.DiscountValue)
	case models.PromoDiscountTypeFIXEDAMOUNT:
		discountAmount = db.PromoMoneyFixed2(math.Min(params.OriginalPrice, params.DiscountValue))
	}

	return PromoCreationQuoteResult{
		OriginalPrice:  params.OriginalPrice,
		DiscountAmount: discountAmount,
		FinalPrice:     PromoCalculateFinalPrice(params.OriginalPrice, discountAmount),
	}
}

// PromoCalculateRenewalSplit mirrors calculatePromoRenewalSplit: the FOREVER
// branch (remainingCycles === null) discounts every month and never unbinds, the
// REPEATING branch discounts only the remaining cycles and unbinds when they run
// out.
func PromoCalculateRenewalSplit(params PromoRenewalSplitParams) PromoRenewalSplitResult {
	if params.RemainingCycles == nil {
		total := params.MonthlyPrice * float64(params.Months)
		discountAmount := db.PromoMoneyFixed2(total * params.DiscountRate)

		return PromoRenewalSplitResult{
			DiscountedMonths:   params.Months,
			RegularMonths:      0,
			DiscountAmount:     discountAmount,
			FinalAmount:        PromoCalculateFinalPrice(total, discountAmount),
			ConsumedCycles:     params.Months,
			NewRemainingCycles: nil,
			WillUnbind:         false,
		}
	}

	remainingCycles := int(*params.RemainingCycles)
	discountedMonths := params.Months
	// Math.min(months, Math.max(0, remainingCycles))
	if limit := max(0, remainingCycles); discountedMonths > limit {
		discountedMonths = limit
	}
	regularMonths := params.Months - discountedMonths

	discountAmount := db.PromoMoneyFixed2(
		(params.MonthlyPrice * float64(discountedMonths)) * params.DiscountRate)
	total := params.MonthlyPrice * float64(params.Months)

	newRemainingCycles := remainingCycles - discountedMonths
	if newRemainingCycles < 0 {
		newRemainingCycles = 0
	}
	narrowed := int32(newRemainingCycles)

	return PromoRenewalSplitResult{
		DiscountedMonths:   discountedMonths,
		RegularMonths:      regularMonths,
		DiscountAmount:     discountAmount,
		FinalAmount:        PromoCalculateFinalPrice(total, discountAmount),
		ConsumedCycles:     discountedMonths,
		NewRemainingCycles: &narrowed,
		WillUnbind:         newRemainingCycles <= 0,
	}
}
