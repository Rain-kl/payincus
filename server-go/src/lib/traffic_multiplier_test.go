package lib

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// Every expected value below was produced by the TypeScript implementation in
// server/src/lib/traffic-multiplier.ts under Node (`Math.round(v * 1000) / 1000`).
func TestNormalizeTrafficMultiplierMatchesNode(t *testing.T) {
	decimalOnePointFive, err := models.NewDecimalFromString("1.500")
	if err != nil {
		t.Fatalf("decimal: %v", err)
	}
	decimalPointZeroZeroOne, err := models.NewDecimalFromString("0.001")
	if err != nil {
		t.Fatalf("decimal: %v", err)
	}

	cases := []struct {
		name  string
		value any
		want  float64
	}{
		// null / undefined / '' all collapse to the column default.
		{"nil (null/undefined)", nil, 1},
		{"empty string", "", 1},
		{"whitespace-only string", "   ", 0}, // Number('   ') === 0 → rejected below
		// numeric inputs
		{"integer one", float64(1), 1},
		{"integer one hundred", float64(100), 100},
		{"lower bound", 0.001, 0.001},
		{"three decimals", 1.2344, 1.234},
		{"half rounds up (1.2345)", 1.2345, 1.235},
		{"half rounds up (1.0005)", 1.0005, 1.001},
		{"toFixed trap value 2.675", 2.675, 2.675},
		{"toFixed trap value 1.005", 1.005, 1.005},
		{"fractional 0.5", 0.5, 0.5},
		// strings go through Number()
		{"numeric string", "1.5", 1.5},
		{"padded numeric string", " 2.25 ", 2.25},
		{"scientific string", "1e2", 100},
		{"hex string", "0x10", 16},
		{"binary string", "0b1010", 10},
		{"octal string", "0o17", 15},
		{"negative-prefixed zero is rejected", "-1", 0},
		// booleans: Number(true) === 1
		{"true", true, 1},
		// Prisma Decimal values (models.Decimal mirrors Decimal(8,3)).
		{"decimal 1.500", decimalOnePointFive, 1.5},
		{"decimal pointer 0.001", &decimalPointZeroZeroOne, 0.001},
		{"nil decimal pointer", (*models.Decimal)(nil), 1},
		// json.Number arrives when a route decodes with UseNumber().
		{"json.Number", json.Number("3.5"), 3.5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeTrafficMultiplier(tc.value)
			if tc.name == "whitespace-only string" || tc.name == "negative-prefixed zero is rejected" {
				if err == nil {
					t.Fatalf("NormalizeTrafficMultiplier(%#v) = %v, want range error", tc.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeTrafficMultiplier(%#v): %v", tc.value, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeTrafficMultiplier(%#v) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestNormalizeTrafficMultiplierRejections(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"below the lower bound", 0.0009},
		{"zero", float64(0)},
		{"negative", -1.0},
		{"above the upper bound", 100.0001},
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
		{"non-numeric string", "abc"},
		{"comma string (Number is NaN, ParseFloat would stop early)", "1,000"},
		{"false", false},
		{"empty object", map[string]any{}},
		{"slice", []any{1.5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := NormalizeTrafficMultiplier(tc.value); err == nil {
				t.Fatalf("NormalizeTrafficMultiplier(%#v) = %v, want error", tc.value, got)
			} else if err.Error() != "Traffic multiplier must be at least 0.001 and no more than 100" {
				t.Fatalf("error message = %q", err.Error())
			}
		})
	}
}

func TestApplyTrafficMultiplierMatchesBigIntArithmetic(t *testing.T) {
	limit := func(value int64) *int64 { return &value }

	// (limit * scaled) / 1000n truncates toward zero, exactly like BigInt.
	cases := []struct {
		name       string
		limit      *int64
		multiplier any
		want       *int64
	}{
		{"unlimited stays unlimited", nil, 1.5, nil},
		{"x1", limit(1000), 1, limit(1000)},
		{"x1.5", limit(1000), 1.5, limit(1500)},
		{"x0.001 truncates", limit(999), 0.001, limit(0)},
		{"negative truncates toward zero", limit(-999), 0.001, limit(0)},
		{"x100 on a 100 byte allowance", limit(100), 100, limit(10000)},
		{"string multiplier", limit(2000), "2.5", limit(5000)},
		{"default multiplier when nil", limit(777), nil, limit(777)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ApplyTrafficMultiplier(tc.limit, tc.multiplier)
			if err != nil {
				t.Fatalf("ApplyTrafficMultiplier: %v", err)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", *got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("got %v, want %v", got, *tc.want)
			}
		})
	}

	if _, err := ApplyTrafficMultiplier(limit(1), 0); err == nil {
		t.Fatal("multiplier 0 must be rejected before the multiplication")
	}

	// BigInt would keep going; an int64 column cannot. Fail loudly.
	max := int64(math.MaxInt64)
	if _, err := ApplyTrafficMultiplier(&max, 100); err == nil {
		t.Fatal("int64 overflow must be reported, not truncated")
	}
}

func TestResolveInstanceTrafficLimitForHostWithoutPackage(t *testing.T) {
	base := int64(4096)
	zero := int32(0)
	noPackage := TrafficMultiplierResolveParams{PackageID: nil, HostID: 1, BaseTrafficLimit: &base}
	got, err := ResolveInstanceTrafficLimitForHost(t.Context(), nil, noPackage)

	if err != nil || got == nil || *got != base {
		t.Fatalf("nil package id must return the base limit, got %v err %v", got, err)
	}

	zeroPackage := TrafficMultiplierResolveParams{PackageID: &zero, HostID: 1, BaseTrafficLimit: &base}
	got, err = ResolveInstanceTrafficLimitForHost(t.Context(), nil, zeroPackage)
	if err != nil || got == nil || *got != base {
		t.Fatalf("package id 0 must return the base limit (JS falsy), got %v err %v", got, err)
	}
}

// The db package keeps a private duplicate of these two functions (db ↛ lib);
// the values are pinned on both sides so a drift fails one of the suites.
func TestTrafficMultiplierScaleIsThousand(t *testing.T) {
	if TrafficMultiplierScale != 1000 {
		t.Fatalf("TrafficMultiplierScale = %d, want 1000", TrafficMultiplierScale)
	}
}
