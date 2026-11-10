// Pure (no database) tests for the payment-provider domain logic: financial
// validation, secret masking/merge, fee arithmetic and the config codec seam.
package db

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/rain-kl/payincus/server-go/src/models"
)

func TestPaymentProviderFinancialValidation(t *testing.T) {
	input := NewPaymentProviderFinancialInput(map[string]any{})
	data, err := NormalizePaymentProviderFinancialInput(input, true, true)
	if err != nil {
		t.Fatalf("defaults must normalize: %v", err)
	}
	if data.FeeRate == nil || *data.FeeRate != 0 || data.FeeFixed == nil || *data.FeeFixed != 0 ||
		data.MinAmount == nil || *data.MinAmount != 1 || !data.MaxAmountSet || data.MaxAmount != nil {
		t.Fatalf("unexpected defaults: %+v", data)
	}

	// feeRate is normalized to 4 decimals, money to 2.
	values := NewPaymentProviderFinancialInput(map[string]any{
		"feeRate": "0.00600", "feeFixed": 0.5, "minAmount": "2.5", "maxAmount": "100",
	})
	data, err = NormalizePaymentProviderFinancialInput(values, false, true)
	if err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if *data.FeeRate != 0.006 || *data.FeeFixed != 0.5 || *data.MinAmount != 2.5 || *data.MaxAmount != 100 {
		t.Fatalf("unexpected normalization: %+v", data)
	}

	// max < min is only checked when both are present.
	if _, err := NormalizePaymentProviderFinancialInput(NewPaymentProviderFinancialInput(map[string]any{
		"minAmount": 10, "maxAmount": 5,
	}), true, true); err == nil || err.Error() != "最大充值金额不能小于最小充值金额" {
		t.Fatalf("expected max<min rejection, got %v", err)
	}
	// validateRange=false (the PATCH path) skips the cross-field check.
	if _, err := NormalizePaymentProviderFinancialInput(NewPaymentProviderFinancialInput(map[string]any{
		"maxAmount": 5,
	}), false, false); err != nil {
		t.Fatalf("PATCH range must not be validated: %v", err)
	}

	invalid := []struct {
		fields map[string]any
		err    string
	}{
		{map[string]any{"feeRate": 1.5}, "手续费费率不合法"},
		{map[string]any{"feeRate": -0.1}, "手续费费率不合法"},
		{map[string]any{"feeRate": "nope"}, "手续费费率不合法"},
		{map[string]any{"feeFixed": -1}, "固定手续费不合法"},
		{map[string]any{"minAmount": 0}, "最小充值金额不合法"},
		{map[string]any{"maxAmount": -5}, "最大充值金额不合法"},
	}
	for _, testCase := range invalid {
		if _, err := NormalizePaymentProviderFinancialInput(
			NewPaymentProviderFinancialInput(testCase.fields), false, true); err == nil || err.Error() != testCase.err {
			t.Fatalf("%v: expected %q, got %v", testCase.fields, testCase.err, err)
		}
	}

	// An explicit empty-string maxAmount clears the limit.
	data, err = NormalizePaymentProviderFinancialInput(NewPaymentProviderFinancialInput(map[string]any{
		"maxAmount": "",
	}), false, false)
	if err != nil || !data.MaxAmountSet || data.MaxAmount != nil {
		t.Fatalf("empty maxAmount must clear: %+v %v", data, err)
	}
}

func TestPaymentProviderSecretMasking(t *testing.T) {
	config := map[string]any{
		"pid":                  "1",
		"key":                  "super-secret",
		"merchant_private_key": "private-pem",
		"secret_key":           "",
		"platform_public_key":  "public",
		"signKey":              "sign",
	}
	masked := maskProviderConfig(config)
	if masked["key"] != "" || masked["keyConfigured"] != true {
		t.Fatalf("key must be masked: %v", masked)
	}
	if masked["merchant_private_key"] != "" || masked["merchant_private_keyConfigured"] != true {
		t.Fatalf("private key must be masked: %v", masked)
	}
	if masked["signKey"] != "" || masked["signKeyConfigured"] != true {
		t.Fatalf("signKey must be masked: %v", masked)
	}
	if masked["secret_key"] != "" {
		t.Fatalf("empty secrets stay empty: %v", masked)
	}
	if _, exists := masked["secret_keyConfigured"]; exists {
		t.Fatal("empty secrets must not be flagged as configured")
	}
	if masked["platform_public_key"] != "public" {
		t.Fatal("non-secret keys pass through")
	}
	if config["key"] != "super-secret" {
		t.Fatal("masking must not mutate the input")
	}

	merged := MergePaymentProviderConfigForUpdate(map[string]any{
		"pid": "2", "key": "", "merchant_private_key": MaskedPaymentSecretValue,
		"secret_key": "new-secret", "keyConfigured": true, "merchant_private_keyConfigured": true,
	}, map[string]any{
		"pid": "1", "key": "stored-key", "merchant_private_key": "stored-pem",
	})
	if merged["key"] != "stored-key" || merged["merchant_private_key"] != "stored-pem" {
		t.Fatalf("masked/empty secrets must preserve the stored value: %v", merged)
	}
	if merged["secret_key"] != "new-secret" {
		t.Fatalf("a real new secret must win: %v", merged)
	}
	if merged["pid"] != "2" {
		t.Fatal("non-secret fields take the new value")
	}
	if _, exists := merged["keyConfigured"]; exists {
		t.Fatal("Configured sentinels must be stripped before storage")
	}
}

func TestPaymentProviderConfigCodecRoundTrip(t *testing.T) {
	// Install a deterministic fake codec: "enc:" + base64(JSON).
	RegisterPaymentProviderSensitiveCodec(
		func(plaintext string) (string, error) {
			return "enc:" + base64.StdEncoding.EncodeToString([]byte(plaintext)), nil
		},
		func(ciphertext string) string {
			if !strings.HasPrefix(ciphertext, "enc:") {
				return ""
			}
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ciphertext, "enc:"))
			if err != nil {
				return ""
			}
			return string(decoded)
		},
	)

	config := map[string]any{"pid": "1", "key": "secret", "version": "v1"}
	encoded, err := encodePaymentProviderConfig(config)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.HasPrefix(string(encoded), `"enc:`) {
		t.Fatalf("the column must hold a JSON string scalar: %s", encoded)
	}
	decoded := decodePaymentProviderConfig(encoded)
	if decoded["pid"] != "1" || decoded["key"] != "secret" || decoded["version"] != "v1" {
		t.Fatalf("round trip mismatch: %v", decoded)
	}

	// Legacy plain JSON object.
	legacyObject := datatypes.JSON([]byte(`{"pid":"2","key":"old"}`))
	if decoded := decodePaymentProviderConfig(legacyObject); decoded["pid"] != "2" {
		t.Fatalf("legacy object mismatch: %v", decoded)
	}
	// Legacy JSON *string* containing JSON.
	legacyString := datatypes.JSON([]byte(`"{\"pid\":\"3\"}"`))
	if decoded := decodePaymentProviderConfig(legacyString); decoded["pid"] != "3" {
		t.Fatalf("legacy string mismatch: %v", decoded)
	}
	// Garbage degrades to {} and never panics.
	for _, raw := range []string{"", "null", "[1,2]", `"garbage"`, "\"enc:!!!\""} {
		if decoded := decodePaymentProviderConfig(datatypes.JSON([]byte(raw))); len(decoded) != 0 {
			t.Fatalf("%q must decode to an empty object, got %v", raw, decoded)
		}
	}
	// JSON arrays are not objects.
	if decoded := decodePaymentProviderConfig(datatypes.JSON([]byte(`[{"a":1}]`))); len(decoded) != 0 {
		t.Fatalf("arrays must decode to {}, got %v", decoded)
	}
}

func TestPaymentProviderFeeArithmetic(t *testing.T) {
	yipay := PaymentProviderRecord{
		Type: models.PaymentProviderTypeYipay, FeeRate: 0.03, FeeFixed: 0.5, MinAmount: 1,
		Config: map[string]any{"methodFees": map[string]any{
			"alipay": map[string]any{"feeRate": 0.03, "feeFixed": 0.5},
			"wxpay":  0.04,
			"bad":    map[string]any{"feeRate": 2},
		}},
	}
	method := "alipay"
	if fee := CalculatePaymentFee(yipay, 100, &method); fee != 3.5 {
		t.Fatalf("alipay fee mismatch: %v", fee)
	}
	if payable := CalculatePayableAmount(yipay, 100, &method); payable != 103.5 {
		t.Fatalf("yipay surcharge payable mismatch: %v", payable)
	}
	if actual := CalculateActualAmount(yipay, 100); actual != 100 {
		t.Fatalf("yipay credits the full amount: %v", actual)
	}
	wxpay := "wxpay"
	if fee := CalculatePaymentFee(yipay, 100, &wxpay); fee != 4 {
		t.Fatalf("numeric method fee mismatch: %v", fee)
	}
	// An entry that exists but is invalid normalises to {0,0}: the TS returns the
	// (truthy) object instead of falling back to the provider defaults.
	bad := "bad"
	if fee := CalculatePaymentFee(yipay, 100, &bad); fee != 0 {
		t.Fatalf("invalid method entry must normalise to zero: %v", fee)
	}

	heleket := PaymentProviderRecord{
		Type: models.PaymentProviderTypeHeleket, FeeRate: 0.05, FeeFixed: 0.3, MinAmount: 10,
		MaxAmount: floatPtr(100),
	}
	emptyMethod := ""
	if fee := CalculatePaymentFee(heleket, 100, &emptyMethod); fee != 5.3 {
		t.Fatalf("provider fee mismatch: %v", fee)
	}
	if payable := CalculatePayableAmount(heleket, 100, nil); payable != 100 {
		t.Fatalf("non-yipay payable must equal the amount: %v", payable)
	}
	if actual := CalculateActualAmount(heleket, 100); actual != 94.7 {
		t.Fatalf("non-yipay actual amount mismatch: %v", actual)
	}

	// JS toFixed boundaries: 1.005 -> 1.00, 2.675 -> 2.67 (binary value), 0.1+0.2 -> 0.30.
	if fee := CalculateFee(PaymentProviderRecord{FeeRate: 0, FeeFixed: 1.005}, 0); fee != 1 {
		t.Fatalf("1.005 toFixed(2) must be 1.00: %v", fee)
	}
	if fee := CalculateFee(PaymentProviderRecord{FeeRate: 0, FeeFixed: 2.675}, 0); fee != 2.67 {
		t.Fatalf("2.675 toFixed(2) must be 2.67: %v", fee)
	}
	if fee := CalculateFee(PaymentProviderRecord{FeeRate: 0.1, FeeFixed: 0}, 3); fee != 0.3 {
		t.Fatalf("0.1*3 toFixed(2) must be 0.30: %v", fee)
	}

	valid, message := ValidateRechargeAmount(heleket, 5)
	if valid || message != "充值金额不能低于 10 元" {
		t.Fatalf("min validation mismatch: %v %q", valid, message)
	}
	valid, message = ValidateRechargeAmount(heleket, 101)
	if valid || message != "充值金额不能超过 100 元" {
		t.Fatalf("max validation mismatch: %v %q", valid, message)
	}
	if valid, _ := ValidateRechargeAmount(heleket, 50); !valid {
		t.Fatal("in-range amount must pass")
	}
	if methods := GetProviderMethods(PaymentProviderRecord{}); len(methods) != 0 {
		t.Fatal("nil methods must read as empty")
	}
}

func TestPaymentProviderSanitizeResponse(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	record := PaymentProviderRecord{
		ID: 7, Name: "n", Type: models.PaymentProviderTypeManual, Status: models.PaymentProviderStatusActive,
		Config:  map[string]any{"instructions": "x", "key": "secret"},
		Methods: []string{"alipay"}, MethodsRaw: []any{"alipay"},
		FeeRate: 0.006, FeeFixed: 0.5, MinAmount: 1, SortOrder: 3,
		CreatedAt: created, UpdatedAt: created,
	}
	response := SanitizePaymentProviderForResponse(record)
	if response.Config["key"] != "" || response.Config["keyConfigured"] != true {
		t.Fatalf("config not masked: %v", response.Config)
	}
	if response.FeeRate != "0.006" || response.FeeFixed != "0.5" || response.MinAmount != "1" || response.MaxAmount != nil {
		t.Fatalf("decimal strings mismatch: %+v", response)
	}
	if response.CreatedAt != "2026-01-02T03:04:05.000Z" || response.UpdatedAt != "2026-01-02T03:04:05.000Z" {
		t.Fatalf("timestamps mismatch: %s / %s", response.CreatedAt, response.UpdatedAt)
	}
	if _, ok := response.Methods.([]any); !ok {
		t.Fatalf("methods must keep their JSON shape: %T", response.Methods)
	}

	record.MaxAmount = floatPtr(100)
	response = SanitizePaymentProviderForResponse(record)
	if response.MaxAmount == nil || *response.MaxAmount != "100" {
		t.Fatalf("maxAmount mismatch: %v", response.MaxAmount)
	}

	empty := SanitizePaymentProviderForResponse(PaymentProviderRecord{})
	if empty.Config == nil || empty.Methods == nil {
		t.Fatal("nil config/methods must serialise as {} and []")
	}
}

func floatPtr(value float64) *float64 { return &value }

// TestPaymentProviderToFixedMatchesJS pins the helper used by the financial
// normalisation against values where math.Round disagrees with toFixed.
func TestPaymentProviderToFixedMatchesJS(t *testing.T) {
	cases := []struct {
		value    float64
		digits   int
		expected float64
	}{
		{1.005, 2, 1.00},
		{2.675, 2, 2.67},
		{-1.005, 2, -1.00},
		{1.45, 1, 1.4},
		{0.006, 4, 0.006},
		{1.00005, 4, 1.0001},
	}
	for _, testCase := range cases {
		if got := paymentProviderToFixed(testCase.value, testCase.digits); math.Abs(got-testCase.expected) > 1e-12 {
			t.Fatalf("toFixed(%v, %d) = %v, want %v", testCase.value, testCase.digits, got, testCase.expected)
		}
	}
	// NaN/Inf and the >=1e21 exponential switch pass through unchanged.
	if !math.IsNaN(paymentProviderToFixed(math.NaN(), 2)) {
		t.Fatal("NaN must pass through")
	}
	if paymentProviderToFixed(1e22, 2) != 1e22 {
		t.Fatal("1e22 must pass through")
	}
}

// Keep json imported for future cases; the codec tests use raw JSON above.
var _ = json.Marshal
