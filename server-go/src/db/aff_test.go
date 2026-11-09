package db

import (
	"encoding/json"
	"math"
	"testing"
	"unicode/utf8"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== number helpers ====================

// affDecimalFromNumber must behave like `new Prisma.Decimal(number)`: decimal.js
// parses the number's shortest round-trip string, not its exact binary expansion.
func TestAffDecimalFromNumber(t *testing.T) {
	cases := []struct {
		value float64
		want  string
	}{
		{0, "0"},
		{0.1, "0.1"},
		{12.5, "12.5"},
		{100, "100"},
		{-2.5, "-2.5"},
		{0.07, "0.07"},
		{0.30000000000000004, "0.30000000000000004"},
	}
	for _, testCase := range cases {
		got := affDecimalFromNumber(testCase.value).String()
		if got != testCase.want {
			t.Errorf("affDecimalFromNumber(%v) = %q, want %q", testCase.value, got, testCase.want)
		}
	}
}

// affDecimalFromNumber(NaN/Inf) cannot be stored; it falls back to zero instead of
// panicking (shopspring/decimal.NewFromFloat panics on those inputs).
func TestAffDecimalFromNumberNonFinite(t *testing.T) {
	if got := affDecimalFromNumber(math.NaN()).String(); got != "0" {
		t.Errorf("NaN -> %q, want 0", got)
	}
	if got := affDecimalFromNumber(math.Inf(1)).String(); got != "0" {
		t.Errorf("Inf -> %q, want 0", got)
	}
}

func TestAffJSRound(t *testing.T) {
	cases := []struct {
		value float64
		want  float64
	}{
		{0, 0},
		{0.4999, 0},
		{0.5, 1},
		{2.5, 3},
		{12.345, 12},
		{12.5, 13},
	}
	for _, testCase := range cases {
		if got := affJSRound(testCase.value); got != testCase.want {
			t.Errorf("affJSRound(%v) = %v, want %v", testCase.value, got, testCase.want)
		}
	}
}

// ==================== pagination / guards ====================

func TestAffClampPagination(t *testing.T) {
	cases := []struct {
		page, pageSize     int
		wantPage, wantSize int
	}{
		{0, 0, 1, 20},
		{2, 50, 2, 50},
		{-1, 0, 1, 20},
		{0, 500, 1, 100},
		{0, -5, 1, 1},
		{3, 100, 3, 100},
		{1, 1, 1, 1},
	}
	for _, testCase := range cases {
		page, pageSize := affClampPagination(testCase.page, testCase.pageSize)
		if page != testCase.wantPage || pageSize != testCase.wantSize {
			t.Errorf("affClampPagination(%d, %d) = (%d, %d), want (%d, %d)",
				testCase.page, testCase.pageSize, page, pageSize, testCase.wantPage, testCase.wantSize)
		}
	}
}

func TestAffNormalizeLogType(t *testing.T) {
	if got := affNormalizeLogType(""); got != "" {
		t.Errorf("empty -> %q, want empty", got)
	}
	for _, valid := range []string{"new_purchase", "renew", "convert"} {
		if got := affNormalizeLogType(models.AffLogType(valid)); string(got) != valid {
			t.Errorf("%q -> %q, want unchanged", valid, got)
		}
	}
	if got := affNormalizeLogType("bogus"); got != "" {
		t.Errorf("bogus -> %q, want empty (filter dropped)", got)
	}
}

func TestAffNormalizeWithdrawalStatus(t *testing.T) {
	for _, valid := range []string{"pending", "approved", "rejected"} {
		if got := affNormalizeWithdrawalStatus(models.AffWithdrawalStatus(valid)); string(got) != valid {
			t.Errorf("%q -> %q, want unchanged", valid, got)
		}
	}
	if got := affNormalizeWithdrawalStatus("bogus"); got != "" {
		t.Errorf("bogus -> %q, want empty", got)
	}
}

// ==================== username masking ====================

func TestAffMaskUsername(t *testing.T) {
	cases := []struct {
		username string
		want     string
	}{
		{"alice", "al***ce"},
		{"abcdef", "ab***ef"},
		{"abcd", "a***"},
		{"abc", "a***"},
		{"a", "a***"},
		{"", "***"},
		{"张三丰", "张***"},
		{"张三丰四", "a***"}, // placeholder replaced below
		{"张三丰四五", "张三***四五"},
		{"😀😀😀", "😀***😀"},
	}
	// 4 UTF-16 units exactly (two CJK chars) keeps only the first unit.
	cases[7].want = "张***"

	for _, testCase := range cases {
		if got := affMaskUsername(testCase.username); got != testCase.want {
			t.Errorf("affMaskUsername(%q) = %q, want %q", testCase.username, got, testCase.want)
		}
	}
}

// The mask counts UTF-16 code units, so a 4-unit username of two astral runes
// takes the "too short" branch (JavaScript length 4).
//
// Known, accepted deviation: charAt(0) on an astral rune is a lone high
// surrogate. Node serialises it as "\ud83d***"; a Go string cannot hold a lone
// surrogate, so the first unit decodes to U+FFFD. Only usernames whose first
// character is astral AND that are <= 4 UTF-16 units long are affected.
func TestAffMaskUsernameCountsUTF16Units(t *testing.T) {
	username := "😀😀"
	if utf8.RuneCountInString(username) != 2 {
		t.Fatalf("precondition: rune count = %d", utf8.RuneCountInString(username))
	}
	if got := affMaskUsername(username); got != "\uFFFD***" {
		t.Errorf("affMaskUsername(%q) = %q, want %q", username, got, "\uFFFD***")
	}
}

// ==================== code generation ====================

// The generator masks random bytes with 0x3f, which is only uniform when the
// alphabet has exactly 64 entries.
func TestAffCodeAlphabet(t *testing.T) {
	if len(affCodeAlphabet) != 64 {
		t.Fatalf("alphabet length = %d, want 64", len(affCodeAlphabet))
	}
	seen := map[rune]bool{}
	for _, char := range affCodeAlphabet {
		if seen[char] {
			t.Errorf("duplicate alphabet rune %q", char)
		}
		seen[char] = true
	}
}

func TestAffGenerateCode(t *testing.T) {
	code, err := affGenerateCode(42)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(code) != len("AFF-42-")+8 {
		t.Fatalf("code %q has length %d, want %d", code, len(code), len("AFF-42-")+8)
	}
	if code[:7] != "AFF-42-" {
		t.Errorf("code %q does not start with AFF-42-", code)
	}
	for _, char := range code[7:] {
		if char >= 'a' && char <= 'z' {
			t.Errorf("code %q contains a lowercase rune", code)
		}
		switch {
		case char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9':
		case char == '_' || char == '-':
		default:
			t.Errorf("code %q contains unexpected rune %q", code, char)
		}
	}
}

// ==================== JSON shapes ====================

// Every getAffStats field is a JavaScript number (`Promise<{...: number}>`), so
// the JSON must not contain quoted decimals.
func TestAffStatsJSONShape(t *testing.T) {
	encoded, err := json.Marshal(AffStats{
		TotalEarnings:  12.5,
		TotalConverted: 3,
		CurrentBalance: 0,
		TotalCodes:     2,
		TotalUsed:      5,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{"totalEarnings", "totalConverted", "currentBalance", "totalCodes", "totalUsed"} {
		if _, ok := decoded[key].(float64); !ok {
			t.Errorf("%s = %#v, want a JSON number", key, decoded[key])
		}
	}
}

// The admin withdrawal projection keeps the withdrawal Decimal as a string but
// converts the nested user affBalance to a number.
func TestAffPendingWithdrawalJSONShape(t *testing.T) {
	item := AffPendingWithdrawal{
		ID:     7,
		UserID: 3,
		Amount: mustDecimal(t, "12.50"),
		Status: "pending",
		User: AffPendingWithdrawalUser{
			ID:         3,
			Username:   "alice",
			AffBalance: 4.5,
		},
	}

	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded["amount"] != "12.5" {
		t.Errorf("amount = %#v, want the JSON string \"12.5\"", decoded["amount"])
	}
	if decoded["reviewedAt"] != nil {
		t.Errorf("reviewedAt = %#v, want null", decoded["reviewedAt"])
	}
	user, ok := decoded["user"].(map[string]any)
	if !ok {
		t.Fatalf("user = %#v, want an object", decoded["user"])
	}
	if user["affBalance"] != 4.5 {
		t.Errorf("user.affBalance = %#v, want 4.5 (JSON number)", user["affBalance"])
	}
}

// AFF log rows keep their Decimal columns as JSON strings and expose the joined
// code as `{code}` (or null).
func TestAffLogWithDetailsJSONShape(t *testing.T) {
	remark := "新购返利"
	item := AffLogWithDetails{
		ID:            11,
		UserID:        3,
		Type:          "new_purchase",
		Amount:        mustDecimal(t, "1.25"),
		BalanceBefore: mustDecimal(t, "0.00"),
		BalanceAfter:  mustDecimal(t, "1.25"),
		Remark:        &remark,
		AffCode:       &AffLogCodeRef{Code: "AFF-3-ABCDEFGH"},
	}

	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{"amount", "balanceBefore", "balanceAfter"} {
		if _, ok := decoded[key].(string); !ok {
			t.Errorf("%s = %#v, want a JSON string", key, decoded[key])
		}
	}
	if decoded["originalAmount"] != nil {
		t.Errorf("originalAmount = %#v, want null", decoded["originalAmount"])
	}
	code, ok := decoded["affCode"].(map[string]any)
	if !ok || code["code"] != "AFF-3-ABCDEFGH" {
		t.Errorf("affCode = %#v, want {code: AFF-3-ABCDEFGH}", decoded["affCode"])
	}
}

// ==================== helpers ====================
