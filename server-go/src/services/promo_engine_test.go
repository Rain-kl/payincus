package services

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

func promoTestDecimal(t *testing.T, value string) models.Decimal {
	t.Helper()
	parsed, err := models.NewDecimalFromString(value)
	if err != nil {
		t.Fatalf("decimal %q: %v", value, err)
	}
	return parsed
}

func promoTestFloat(value float64) *float64 { return &value }
func promoTestInt32(value int32) *int32     { return &value }

// TestPromoValidateEmptyCode covers the only validate() branch that returns
// before touching the database.
func TestPromoValidateEmptyCode(t *testing.T) {
	for _, code := range []string{"", "   ", "\t\n"} {
		result, err := PromoValidate(context.Background(), PromoValidateParams{Code: code})
		if err != nil {
			t.Fatalf("validate(%q): %v", code, err)
		}
		if result.Valid {
			t.Fatalf("validate(%q) = valid, want rejected", code)
		}
		if result.ErrorCode != "PROMO_NOT_FOUND" || result.Error != "优惠码不能为空" {
			t.Fatalf("validate(%q) = %+v, want the empty-code rejection", code, result)
		}
	}
}

func TestPromoQuoteCreation(t *testing.T) {
	cases := []struct {
		name           string
		original       float64
		discountType   models.PromoDiscountType
		discountValue  float64
		wantDiscount   float64
		wantFinalPrice float64
	}{
		{
			name: "percentage", original: 100, discountType: models.PromoDiscountTypePERCENTAGE,
			discountValue: 0.15, wantDiscount: 15, wantFinalPrice: 85,
		},
		{
			name: "percentage rounds like toFixed", original: 3, discountType: models.PromoDiscountTypePERCENTAGE,
			discountValue: 0.1, wantDiscount: 0.3, wantFinalPrice: 2.7,
		},
		{
			name: "fixed amount", original: 100, discountType: models.PromoDiscountTypeFIXEDAMOUNT,
			discountValue: 30, wantDiscount: 30, wantFinalPrice: 70,
		},
		{
			name: "fixed amount clamps to the price", original: 100, discountType: models.PromoDiscountTypeFIXEDAMOUNT,
			discountValue: 130, wantDiscount: 100, wantFinalPrice: 0,
		},
		{
			// Math.min(-20, 100) = -20, so the "discount" is negative and the final
			// price rises. Preserved verbatim from the TypeScript.
			name: "negative fixed amount", original: 100, discountType: models.PromoDiscountTypeFIXEDAMOUNT,
			discountValue: -20, wantDiscount: -20, wantFinalPrice: 120,
		},
		{
			name: "unknown discount type", original: 100, discountType: models.PromoDiscountType("BOGUS"),
			discountValue: 0.5, wantDiscount: 0, wantFinalPrice: 100,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PromoCalculateCreationQuote(PromoCreationQuoteParams{
				OriginalPrice: tc.original,
				DiscountType:  tc.discountType,
				DiscountValue: tc.discountValue,
			})
			if got.OriginalPrice != tc.original {
				t.Fatalf("originalPrice = %v, want %v", got.OriginalPrice, tc.original)
			}
			if got.DiscountAmount != tc.wantDiscount {
				t.Fatalf("discountAmount = %v, want %v", got.DiscountAmount, tc.wantDiscount)
			}
			if got.FinalPrice != tc.wantFinalPrice {
				t.Fatalf("finalPrice = %v, want %v", got.FinalPrice, tc.wantFinalPrice)
			}
		})
	}
}

func TestPromoCalculateRenewalSplit(t *testing.T) {
	t.Run("forever discounts every month and never unbinds", func(t *testing.T) {
		got := PromoCalculateRenewalSplit(PromoRenewalSplitParams{
			MonthlyPrice: 10, Months: 3, DiscountRate: 0.1,
		})
		if got.DiscountedMonths != 3 || got.RegularMonths != 0 || got.ConsumedCycles != 3 {
			t.Fatalf("months = %+v", got)
		}
		if got.DiscountAmount != 3 || got.FinalAmount != 27 {
			t.Fatalf("amounts = %+v", got)
		}
		if got.NewRemainingCycles != nil || got.WillUnbind {
			t.Fatalf("forever binding = %+v", got)
		}
	})

	t.Run("repeating exhausts the remaining cycles", func(t *testing.T) {
		got := PromoCalculateRenewalSplit(PromoRenewalSplitParams{
			MonthlyPrice: 10, Months: 6, DiscountRate: 0.1, RemainingCycles: promoTestInt32(2),
		})
		if got.DiscountedMonths != 2 || got.RegularMonths != 4 || got.ConsumedCycles != 2 {
			t.Fatalf("months = %+v", got)
		}
		if got.DiscountAmount != 2 || got.FinalAmount != 58 {
			t.Fatalf("amounts = %+v", got)
		}
		if got.NewRemainingCycles == nil || *got.NewRemainingCycles != 0 || !got.WillUnbind {
			t.Fatalf("binding = %+v, want unbind with 0 remaining", got)
		}
	})

	t.Run("repeating keeps the leftover cycles", func(t *testing.T) {
		got := PromoCalculateRenewalSplit(PromoRenewalSplitParams{
			MonthlyPrice: 10, Months: 2, DiscountRate: 0.1, RemainingCycles: promoTestInt32(5),
		})
		if got.DiscountedMonths != 2 || got.RegularMonths != 0 {
			t.Fatalf("months = %+v", got)
		}
		if got.NewRemainingCycles == nil || *got.NewRemainingCycles != 3 || got.WillUnbind {
			t.Fatalf("binding = %+v", got)
		}
	})

	t.Run("repeating with no cycles left charges full price", func(t *testing.T) {
		got := PromoCalculateRenewalSplit(PromoRenewalSplitParams{
			MonthlyPrice: 10, Months: 3, DiscountRate: 0.5, RemainingCycles: promoTestInt32(0),
		})
		if got.DiscountedMonths != 0 || got.RegularMonths != 3 {
			t.Fatalf("months = %+v", got)
		}
		if got.DiscountAmount != 0 || got.FinalAmount != 30 {
			t.Fatalf("amounts = %+v", got)
		}
		if got.NewRemainingCycles == nil || *got.NewRemainingCycles != 0 || !got.WillUnbind {
			t.Fatalf("binding = %+v, want unbind", got)
		}
	})

	t.Run("toFixed rounding", func(t *testing.T) {
		got := PromoCalculateRenewalSplit(PromoRenewalSplitParams{
			MonthlyPrice: 33.33, Months: 1, DiscountRate: 0.15,
		})
		// 33.33 * 0.15 = 4.9995000000000005 -> toFixed(2) -> 5.00
		if got.DiscountAmount != 5 {
			t.Fatalf("discountAmount = %v, want 5", got.DiscountAmount)
		}
		if got.FinalAmount != 28.33 {
			t.Fatalf("finalAmount = %v, want 28.33", got.FinalAmount)
		}
	})
}

func TestPromoResolveCreateAmounts(t *testing.T) {
	t.Run("quote wins over the flat fields, even at zero", func(t *testing.T) {
		amounts := promoResolveCreateAmounts(PromoSettleCreateParams{
			DiscountAmount: promoTestFloat(50),
			FinalPrice:     promoTestFloat(50),
			Quote:          &PromoSettleCreateQuote{DiscountAmount: 0, FinalPrice: 0},
		})
		if amounts.discountAmount != 0 || amounts.finalPrice != 0 {
			t.Fatalf("amounts = %+v, want the quote's zeros", amounts)
		}
		if amounts.originalPrice != 0 {
			t.Fatalf("originalPrice = %v, want toFixed(final + discount) = 0", amounts.originalPrice)
		}
	})

	t.Run("flat fields when no quote is given", func(t *testing.T) {
		amounts := promoResolveCreateAmounts(PromoSettleCreateParams{
			DiscountAmount: promoTestFloat(10.005),
			FinalPrice:     promoTestFloat(89.995),
		})
		if amounts.discountAmount != 10.005 || amounts.finalPrice != 89.995 {
			t.Fatalf("amounts = %+v, want the raw fields", amounts)
		}
		if amounts.originalPrice != 100 {
			t.Fatalf("originalPrice = %v, want 100 (toFixed(99.99999999999999))", amounts.originalPrice)
		}
	})

	t.Run("commission by rate uses truthiness", func(t *testing.T) {
		zero := 0.0
		amounts := promoResolveCreateAmounts(PromoSettleCreateParams{
			OriginalPrice:  promoTestFloat(100),
			CommissionRate: &zero,
		})
		if amounts.commissionAmount != 0 {
			t.Fatalf("commissionAmount = %v, want 0 for a falsy rate", amounts.commissionAmount)
		}

		rate := 0.1
		amounts = promoResolveCreateAmounts(PromoSettleCreateParams{
			OriginalPrice:  promoTestFloat(100),
			CommissionRate: &rate,
		})
		if amounts.commissionAmount != 10 {
			t.Fatalf("commissionAmount = %v, want 10", amounts.commissionAmount)
		}

		explicit := promoTestFloat(7)
		amounts = promoResolveCreateAmounts(PromoSettleCreateParams{
			OriginalPrice:    promoTestFloat(100),
			CommissionRate:   &rate,
			CommissionAmount: explicit,
		})
		if amounts.commissionAmount != 7 {
			t.Fatalf("commissionAmount = %v, want the explicit amount", amounts.commissionAmount)
		}
	})
}

func TestPromoQuoteRenewalUsesBindingDiscountValue(t *testing.T) {
	remaining := int32(2)
	binding := &db.PromoInstanceBindingWithCode{
		InstanceID:      9,
		PromoCodeID:     3,
		DurationType:    models.PromoDurationTypeREPEATING,
		RemainingCycles: &remaining,
		PromoCode: db.PromoCodeWithDetails{
			PromoCode: models.PromoCode{DiscountValue: promoTestDecimal(t, "0.25")},
		},
	}

	got := PromoQuoteRenewal(40, 3, binding)
	if got.DiscountedMonths != 2 || got.RegularMonths != 1 {
		t.Fatalf("months = %+v", got)
	}
	// 40 * 2 * 0.25 = 20, final = 120 - 20 = 100
	if got.DiscountAmount != 20 || got.FinalAmount != 100 {
		t.Fatalf("amounts = %+v", got)
	}
	if got.NewRemainingCycles == nil || *got.NewRemainingCycles != 0 || !got.WillUnbind {
		t.Fatalf("binding = %+v", got)
	}
}

func TestPromoIsProductEligibleForPromo(t *testing.T) {
	plan32 := int32(32)
	package7 := int32(7)
	otherPlan := int32(99)

	global := PromoCodeWithScopes{IsGlobal: true}
	if !PromoIsProductEligibleForPromo(global, PromoTarget{PackageID: 7, PackagePlanID: 32}) {
		t.Fatal("global code must match every target")
	}

	planScope := PromoCodeWithScopes{Scopes: []PromoScopeNode{{PackageID: &package7, PackagePlanID: &plan32}}}
	if !PromoIsProductEligibleForPromo(planScope, PromoTarget{PackageID: 7, PackagePlanID: 32}) {
		t.Fatal("plan scope must match its plan")
	}
	if PromoIsProductEligibleForPromo(planScope, PromoTarget{PackageID: 7, PackagePlanID: 99}) {
		t.Fatal("plan scope must not match a different plan")
	}
	if !PromoIsProductEligibleForPromo(planScope, PromoTarget{PackageID: 99, PackagePlanID: 32}) {
		t.Fatal("the leaf rule compares plan ids only, whatever the package id is")
	}

	packageScope := PromoCodeWithScopes{Scopes: []PromoScopeNode{{PackageID: &package7}}}
	if !PromoIsProductEligibleForPromo(packageScope, PromoTarget{PackageID: 7, PackagePlanID: 77}) {
		t.Fatal("package scope must match any plan of the package")
	}
	if PromoIsProductEligibleForPromo(packageScope, PromoTarget{PackageID: 8, PackagePlanID: 77}) {
		t.Fatal("package scope must not match another package")
	}

	// A row with a plan id is a plan row: it never satisfies the package rule.
	bothIDs := PromoCodeWithScopes{Scopes: []PromoScopeNode{{PackageID: &package7, PackagePlanID: &otherPlan}}}
	if PromoIsProductEligibleForPromo(bothIDs, PromoTarget{PackageID: 7, PackagePlanID: 32}) {
		t.Fatal("a plan row must not match through the package rule")
	}

	empty := PromoCodeWithScopes{}
	if PromoIsProductEligibleForPromo(empty, PromoTarget{PackageID: 7, PackagePlanID: 32}) {
		t.Fatal("a non-global code without scopes must not match")
	}
}

func TestPromoScopesOf(t *testing.T) {
	plan := int32(5)
	promo := &db.PromoCodeWithDetails{
		PromoCode: models.PromoCode{IsGlobal: false},
		Scopes: []models.PromoCodeScope{
			{ID: 1, PromoCodeID: 2, PackagePlanID: &plan},
			{ID: 2, PromoCodeID: 2},
		},
	}

	got := PromoScopesOf(promo)
	if got.IsGlobal || len(got.Scopes) != 2 {
		t.Fatalf("scopes = %+v", got)
	}
	if got.Scopes[0].PackagePlanID == nil || *got.Scopes[0].PackagePlanID != 5 {
		t.Fatalf("first scope = %+v", got.Scopes[0])
	}
	if got.Scopes[1].PackageID != nil || got.Scopes[1].PackagePlanID != nil {
		t.Fatalf("second scope = %+v, want both ids nil", got.Scopes[1])
	}
}

// TestPromoJSTrim pins the JavaScript whitespace set (U+FEFF is trimmed,
// U+0085 is not).
func TestPromoJSTrim(t *testing.T) {
	if got := promoJSTrim("\uFEFF code \u00A0"); got != "code" {
		t.Fatalf("trim = %q, want \"code\"", got)
	}
	if got := promoJSTrim("\u0085code"); got != "\u0085code" {
		t.Fatalf("trim = %q, want U+0085 preserved", got)
	}
}

// TestPromoDecimalFromFixed2 pins the `new Prisma.Decimal(value.toFixed(2))` form.
func TestPromoDecimalFromFixed2(t *testing.T) {
	if got := promoDecimalFromFixed2(12.345).String(); got != "12.35" {
		t.Fatalf("decimal = %s, want 12.35", got)
	}
	if got := promoDecimalFromFixed2(0).String(); got != "0" {
		t.Fatalf("decimal = %s, want 0", got)
	}
	// decimal.NewFromString accepts the exact two-decimal form.
	if _, err := decimal.NewFromString(promoDecimalFromFixed2(1).String()); err != nil {
		t.Fatalf("round trip: %v", err)
	}
}
