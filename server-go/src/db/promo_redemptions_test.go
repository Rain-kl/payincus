package db

import (
	"encoding/json"
	"testing"
)

// promoTestInt32 / promoTestFloat64 keep the table cases readable. (The helpers
// are prefixed because storage_configs_test.go already owns int32Ptr.)
func promoTestInt32(value int32) *int32       { return &value }
func promoTestFloat64(value float64) *float64 { return &value }

// TestPromoRedemptionLogRow pins the `?? 1` / `?? 0` defaults and the
// `new Prisma.Decimal(value.toFixed(2))` rounding of createPromoRedemptionLog.
func TestPromoRedemptionLogRow(t *testing.T) {
	row := promoRedemptionLogRow(PromoRedemptionLogData{
		PromoCodeID:    11,
		InstanceID:     22,
		UserID:         33,
		ActionType:     PromoRedemptionActionCreate,
		CycleIndex:     1,
		OriginalPrice:  12.345,
		DiscountAmount: 2.675,
		FinalPrice:     9.67,
	})

	if row.PromoCodeID != 11 || row.InstanceID != 22 || row.UserID != 33 {
		t.Fatalf("identity fields = %+v", row)
	}
	if row.ActionType != "create" || row.CycleIndex != 1 {
		t.Fatalf("action fields = %+v", row)
	}
	if row.Months != 1 {
		t.Fatalf("months = %d, want the `?? 1` default", row.Months)
	}
	if got := row.OriginalPrice.String(); got != "12.35" {
		t.Fatalf("originalPrice = %s, want 12.35 (toFixed(2) of 12.345)", got)
	}
	if got := row.DiscountAmount.String(); got != "2.67" {
		t.Fatalf("discountAmount = %s, want 2.67 (toFixed(2) of 2.675)", got)
	}
	if got := row.CommissionAmount.String(); got != "0" {
		t.Fatalf("commissionAmount = %s, want the `?? 0` default", got)
	}

	explicit := promoRedemptionLogRow(PromoRedemptionLogData{
		ActionType:       PromoRedemptionActionRenew,
		Months:           promoTestInt32(6),
		CommissionAmount: promoTestFloat64(1.005),
	})
	if explicit.Months != 6 {
		t.Fatalf("months = %d, want 6", explicit.Months)
	}
	if got := explicit.CommissionAmount.String(); got != "1" {
		t.Fatalf("commissionAmount = %s, want 1 (1.005 rounds down in binary)", got)
	}
}

// TestPromoRedemptionLogRowJSON pins the wire shape: Decimal columns serialize as
// strings (Prisma) and months as a number.
func TestPromoRedemptionLogRowJSON(t *testing.T) {
	row := promoRedemptionLogRow(PromoRedemptionLogData{
		ActionType:     PromoRedemptionActionCreate,
		CycleIndex:     1,
		OriginalPrice:  100,
		DiscountAmount: 10,
		FinalPrice:     90,
	})

	payload, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, want := range map[string]string{
		"originalPrice":    "100",
		"discountAmount":   "10",
		"finalPrice":       "90",
		"commissionAmount": "0",
	} {
		if got := decoded[key]; got != want {
			t.Errorf("%s = %#v, want the Prisma decimal string %q", key, got, want)
		}
	}
	if got := decoded["months"]; got != float64(1) {
		t.Errorf("months = %#v, want the number 1", got)
	}
}

// TestPromoRedemptionMoneyRounding pins PromoMoneyFixed2 on the values the ledger
// writes, including the binary-representation quirks JavaScript shares.
func TestPromoRedemptionMoneyRounding(t *testing.T) {
	cases := []struct {
		value float64
		want  string
	}{
		{0, "0"},
		{12.345, "12.35"},
		{2.675, "2.67"},
		{1.005, "1"},
		{0.005, "0.01"},
		{-1.005, "-1"},
		{-0.005, "-0.01"},
		{100, "100"},
	}
	for _, tc := range cases {
		if got := promoDecimal2(tc.value).String(); got != tc.want {
			t.Errorf("promoDecimal2(%v) = %s, want %s", tc.value, got, tc.want)
		}
	}
}
