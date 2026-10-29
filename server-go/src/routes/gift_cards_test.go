package routes

import (
	"math"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// ==================== friends: history filter ====================

// TestParseFriendHistoryFilter mirrors parseFriendHistoryFilter: absent/empty is
// "no filter", the four union members are accepted, everything else is invalid.
func TestParseFriendHistoryFilter(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		present   bool
		wantValue string
		wantHas   bool
		wantValid bool
	}{
		{"absent", "", false, "", false, true},
		{"empty string", "", true, "", false, true},
		{"all", "all", true, "all", true, true},
		{"accepted", "accepted", true, "accepted", true, true},
		{"rejected", "rejected", true, "rejected", true, true},
		{"removed", "removed", true, "removed", true, true},
		{"pending is not a history status", "pending", true, "", false, false},
		{"case sensitive", "ACCEPTED", true, "", false, false},
		{"garbage", " x ", true, "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, has, valid := parseFriendHistoryFilter(tc.raw, tc.present)
			if valid != tc.wantValid {
				t.Fatalf("valid = %v, want %v", valid, tc.wantValid)
			}
			if !valid {
				return
			}
			if value != tc.wantValue || has != tc.wantHas {
				t.Fatalf("(%q, %v), want (%q, %v)", value, has, tc.wantValue, tc.wantHas)
			}
		})
	}
}

// ==================== gift cards: money parsing ====================

// TestParsePositiveMoney pins parsePositiveMoney, which guards every amount the
// admin and user routes accept.
func TestParsePositiveMoney(t *testing.T) {
	cases := []struct {
		name   string
		value  any
		want   float64
		wantOK bool
	}{
		{"integer", float64(1), 1, true},
		{"two decimals", 12.34, 12.34, true},
		// JS parity: Math.round(1.005*100)/100 === 1 (1.005 is slightly below 1.005
		// in float64, so *100 rounds down). The TypeScript behaves the same way.
		{"1.005 rounds like JS Math.round", 1.005, 1, true},
		{"rounds down to cents", 1.004, 1, true},
		{"minimum accepted", 0.01, 0.01, true},
		// JS parity: 0.004 > 0 passes the positivity check and then normalizes to 0;
		// the DB-layer guard rejects it with GIFT_CARD_BALANCE_EXCEEDS_FACE_VALUE.
		{"sub-cent positive normalizes to zero", 0.004, 0, true},
		{"zero is rejected", float64(0), 0, false},
		{"negative is rejected", -1.0, 0, false},
		{"at the face-value cap", float64(db.GiftCardMaxFaceValue), db.GiftCardMaxFaceValue, true},
		{"above the face-value cap", float64(db.GiftCardMaxFaceValue) + 0.01, 0, false},
		{"NaN is rejected", math.NaN(), 0, false},
		{"Infinity is rejected", math.Inf(1), 0, false},
		{"string is rejected (AJV already coerced numbers)", "12.5", 0, false},
		{"nil is rejected", nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parsePositiveMoney(tc.value)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %v)", ok, tc.wantOK, got)
			}
			if ok && math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("value = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseClampedPositiveInteger pins the batch-count clamp.
func TestParseClampedPositiveInteger(t *testing.T) {
	cases := []struct {
		name     string
		value    any
		fallback int
		max      int
		want     int
	}{
		{"valid", float64(10), 1, 500, 10},
		{"clamped to max", float64(600), 1, 500, 500},
		{"exactly max", float64(500), 1, 500, 500},
		{"zero falls back", float64(0), 1, 500, 1},
		{"negative falls back", -3.0, 1, 500, 1},
		{"fraction falls back", 2.5, 1, 500, 1},
		{"NaN falls back", math.NaN(), 1, 500, 1},
		{"string falls back", "10", 1, 500, 1},
		{"nil falls back", nil, 1, 500, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseClampedPositiveInteger(tc.value, tc.fallback, tc.max); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// ==================== gift cards: code / string normalisation ====================

// TestNormalizeGiftCardCode pins ^gc-[A-Za-z0-9_-]{24}$ plus the trim.
func TestNormalizeGiftCardCode(t *testing.T) {
	valid := "gc-abcdefghijklmnopqrstuvwx"
	cases := []struct {
		name  string
		value any
		want  *string
	}{
		{"valid", valid, &valid},
		{"trims surrounding space", "  " + valid + "  ", &valid},
		{"wrong prefix", "xx-abcdefghijklmnopqrstuvwx", nil},
		{"too short", "gc-abcdefghijklmnopqrstuvw", nil},
		{"too long", "gc-abcdefghijklmnopqrstuvwxy", nil},
		{"invalid character", "gc-abcdefghijklmnopqrstuvw!", nil},
		{"uppercase prefix rejected", "GC-abcdefghijklmnopqrstuvwx", nil},
		{"non-string", float64(1), nil},
		{"nil", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeGiftCardCode(tc.value)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %q, want nil", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("got nil, want %q", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("got %q, want %q", *got, *tc.want)
			}
		})
	}
}

// TestNormalizeOptionalString covers trim / max length / control-character rules.
func TestNormalizeOptionalString(t *testing.T) {
	long := make([]rune, 201)
	for i := range long {
		long[i] = 'x'
	}
	longString := string(long)

	long64 := make([]rune, 65)
	for i := range long64 {
		long64[i] = 'x'
	}

	cases := []struct {
		name      string
		value     any
		maxLength int
		wantNil   bool
		want      string
	}{
		{"plain", "hello", 200, false, "hello"},
		{"trimmed", "  hello  ", 200, false, "hello"},
		{"blank becomes absent", "   ", 200, true, ""},
		{"at max length", longString[:200], 200, false, longString[:200]},
		{"over max length", longString, 200, true, ""},
		{"control char rejected", "bad\x01value", 200, true, ""},
		{"del char rejected", "bad\x7fvalue", 200, true, ""},
		{"non-string", float64(1), 200, true, ""},
		{"nil", nil, 200, true, ""},
		{"expiry length limit", string(long64[:64]), 64, false, string(long64[:64])},
		{"expiry over limit", string(long64), 64, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeOptionalString(tc.value, tc.maxLength)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("got %q, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want %q", tc.want)
			}
			if *got != tc.want {
				t.Fatalf("got %q, want %q", *got, tc.want)
			}
		})
	}
}

// TestNormalizeGiftCardStatus pins the enum filter.
func TestNormalizeGiftCardStatus(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{"active", "active"},
		{"used", "used"},
		{"disabled", "disabled"},
		{"expired", "expired"},
		{"pending", ""},
		{"ACTIVE", ""},
		{float64(1), ""},
		{nil, ""},
	}
	for _, tc := range cases {
		if got := string(normalizeGiftCardStatus(tc.value)); got != tc.want {
			t.Errorf("normalizeGiftCardStatus(%#v) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

// ==================== gift cards: id list ====================

// TestNormalizeGiftCardIDList covers the batch-disable / batch-delete contract:
// non-empty, unique, positive, capped.
func TestNormalizeGiftCardIDList(t *testing.T) {
	ids := func(values ...float64) []any {
		out := make([]any, len(values))
		for i, value := range values {
			out[i] = value
		}
		return out
	}

	t.Run("valid list", func(t *testing.T) {
		got, ok := normalizeGiftCardIDList(ids(3, 1, 2))
		if !ok {
			t.Fatal("valid list rejected")
		}
		if len(got) != 3 || got[0] != 3 || got[1] != 1 || got[2] != 2 {
			t.Fatalf("got %v, want insertion order preserved [3 1 2]", got)
		}
	})
	t.Run("duplicates rejected", func(t *testing.T) {
		if _, ok := normalizeGiftCardIDList(ids(1, 1)); ok {
			t.Fatal("duplicates must be rejected")
		}
	})
	t.Run("empty rejected", func(t *testing.T) {
		if _, ok := normalizeGiftCardIDList([]any{}); ok {
			t.Fatal("empty list must be rejected")
		}
	})
	t.Run("zero rejected", func(t *testing.T) {
		if _, ok := normalizeGiftCardIDList(ids(0)); ok {
			t.Fatal("zero id must be rejected")
		}
	})
	t.Run("fraction rejected", func(t *testing.T) {
		if _, ok := normalizeGiftCardIDList(ids(1.5)); ok {
			t.Fatal("fractional id must be rejected")
		}
	})
	t.Run("string rejected", func(t *testing.T) {
		if _, ok := normalizeGiftCardIDList([]any{"1"}); ok {
			t.Fatal("string id must be rejected")
		}
	})
	t.Run("non-array rejected", func(t *testing.T) {
		if _, ok := normalizeGiftCardIDList(float64(1)); ok {
			t.Fatal("non-array must be rejected")
		}
	})
	t.Run("over the batch cap rejected", func(t *testing.T) {
		values := make([]any, db.GiftCardMaxBatchCount+1)
		for i := range values {
			values[i] = float64(i + 1)
		}
		if _, ok := normalizeGiftCardIDList(values); ok {
			t.Fatal("list above MAX_BATCH_COUNT must be rejected")
		}
	})
}

// ==================== gift cards: AJV body validation ====================

// TestValidateGiftCardBodyAdminGenerate pins the POST /admin/generate schema:
// required faceValue, 0.01..10000 bounds, optional balanceValue/expiresAt/remark.
func TestValidateGiftCardBodyAdminGenerate(t *testing.T) {
	cases := []struct {
		name     string
		body     any
		want     string
		wantFace float64
	}{
		{"missing faceValue", map[string]any{}, "faceValue is required", 0},
		{"faceValue only", map[string]any{"faceValue": float64(10)}, "", 10},
		{"faceValue below minimum", map[string]any{"faceValue": float64(0)}, "must be >= 0.01", 0},
		{"faceValue above maximum", map[string]any{"faceValue": float64(10001)}, "must be <= 10000", 0},
		{"faceValue as numeric string is coerced", map[string]any{"faceValue": "25.5"}, "", 25.5},
		{"faceValue true coerces to 1", map[string]any{"faceValue": true}, "", 1},
		{"faceValue false coerces to 0", map[string]any{"faceValue": false}, "must be >= 0.01", 0},
		{"faceValue as null is coerced to 0", map[string]any{"faceValue": nil}, "must be >= 0.01", 0},
		{"single-element array unwraps", map[string]any{"faceValue": []any{float64(8)}}, "", 8},
		{"multi-element array is not a number", map[string]any{"faceValue": []any{float64(1), float64(2)}}, "must be number", 0},
		{"remark too long", map[string]any{"faceValue": float64(1), "remark": string(make([]rune, 201))}, "remark is too long", 0},
		{"non-object body", []any{float64(1)}, "must be object", 0},
		// AJV coerces a null root into {} for type:'object', so the required check
		// fires first. This matches Fastify, where an absent body with a valid
		// Content-Type reaches the schema as {}.
		{"null body reports the first required field", nil, "faceValue is required", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, message := validateGiftCardBody(tc.body, []string{"faceValue"}, giftCardAdminFields)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
			if tc.want != "" {
				return
			}
			face, ok := body.number("faceValue")
			if !ok || math.Abs(face-tc.wantFace) > 1e-9 {
				t.Fatalf("faceValue = %v (present=%v), want %v", face, ok, tc.wantFace)
			}
		})
	}
}

// TestValidateGiftCardBodyBatch pins the additional `count` integer field.
func TestValidateGiftCardBodyBatch(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"both required present", map[string]any{"faceValue": float64(1), "count": float64(5)}, ""},
		{"count missing", map[string]any{"faceValue": float64(1)}, "count is required"},
		{"count below minimum", map[string]any{"faceValue": float64(1), "count": float64(0)}, "must be >= 1"},
		{"count above maximum", map[string]any{"faceValue": float64(1), "count": float64(501)}, "must be <= 500"},
		{"count must be an integer", map[string]any{"faceValue": float64(1), "count": 2.5}, "must be integer"},
		{"count as numeric string is coerced", map[string]any{"faceValue": float64(1), "count": "3"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// giftCardBatchFields lists faceValue, balanceValue, count, expiresAt, remark.
			_, message := validateGiftCardBody(tc.body, []string{"faceValue", "count"}, giftCardBatchFields)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
		})
	}
}

// TestValidateGiftCardBodyRedeem pins POST /user/redeem: code 1..64 chars.
func TestValidateGiftCardBodyRedeem(t *testing.T) {
	fields := []giftCardFieldSpec{
		{Name: "code", Kind: giftCardKindString, MinLength: 1, HasMax: true, MaxLength: 64},
		{Name: "turnstileToken", Kind: giftCardKindString},
	}
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"valid", map[string]any{"code": "gc-abcdefghijklmnopqrstuvwx"}, ""},
		{"missing code", map[string]any{}, "code is required"},
		{"empty code", map[string]any{"code": ""}, "code is too short"},
		{"code over 64 chars", map[string]any{"code": string(make([]rune, 65))}, "code is too long"},
		{"turnstileToken accepted", map[string]any{"code": "x", "turnstileToken": "tok"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, message := validateGiftCardBody(tc.body, []string{"code"}, fields)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
		})
	}
}

// ==================== gift cards: query validation ====================

// TestValidateGiftCardAdminListQuery pins the admin/list querystring schema and
// its AJV defaults (page=1, pageSize=20, revealCode=false).
func TestValidateGiftCardAdminListQuery(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		page, pageSize, status, batchID, reveal, message := validateGiftCardAdminListQuery(nil)
		if message != "" {
			t.Fatalf("message = %q", message)
		}
		if page != 1 || pageSize != 20 || status != "" || batchID != nil || reveal {
			t.Fatalf("defaults = %d/%d/%q/%v/%v", page, pageSize, status, batchID, reveal)
		}
	})
	t.Run("explicit values", func(t *testing.T) {
		query := map[string][]string{
			"page":       {"3"},
			"pageSize":   {"50"},
			"status":     {"used"},
			"batchId":    {"abc123"},
			"revealCode": {"true"},
		}
		page, pageSize, status, batchID, reveal, message := validateGiftCardAdminListQuery(query)
		if message != "" {
			t.Fatalf("message = %q", message)
		}
		if page != 3 || pageSize != 50 || status != "used" || !reveal {
			t.Fatalf("values = %d/%d/%q/%v", page, pageSize, status, reveal)
		}
		if batchID == nil || *batchID != "abc123" {
			t.Fatalf("batchId = %v", batchID)
		}
	})
	t.Run("pageSize clamped to 100 by the schema maximum", func(t *testing.T) {
		_, _, _, _, _, message := validateGiftCardAdminListQuery(map[string][]string{"pageSize": {"101"}})
		if message != "must be <= 100" {
			t.Fatalf("message = %q, want the schema maximum message", message)
		}
	})
	t.Run("page below minimum", func(t *testing.T) {
		_, _, _, _, _, message := validateGiftCardAdminListQuery(map[string][]string{"page": {"0"}})
		if message != "must be >= 1" {
			t.Fatalf("message = %q", message)
		}
	})
	t.Run("page not an integer", func(t *testing.T) {
		_, _, _, _, _, message := validateGiftCardAdminListQuery(map[string][]string{"page": {"abc"}})
		if message != "must be integer" {
			t.Fatalf("message = %q", message)
		}
	})
	t.Run("unknown status", func(t *testing.T) {
		_, _, _, _, _, message := validateGiftCardAdminListQuery(map[string][]string{"status": {"pending"}})
		if message != "must be equal to one of the allowed values" {
			t.Fatalf("message = %q", message)
		}
	})
	t.Run("revealCode must be boolean", func(t *testing.T) {
		_, _, _, _, _, message := validateGiftCardAdminListQuery(map[string][]string{"revealCode": {"yes"}})
		if message != "must be boolean" {
			t.Fatalf("message = %q", message)
		}
	})
}

// TestValidateGiftCardUserListQuery pins the user/mine schema (no batchId /
// revealCode).
func TestValidateGiftCardUserListQuery(t *testing.T) {
	page, pageSize, status, message := validateGiftCardUserListQuery(nil)
	if message != "" || page != 1 || pageSize != 20 || status != "" {
		t.Fatalf("defaults = %d/%d/%q (%q)", page, pageSize, status, message)
	}

	_, _, _, message = validateGiftCardUserListQuery(map[string][]string{"status": {"expired"}})
	if message != "" {
		t.Fatalf("expired must be accepted, got %q", message)
	}

	_, _, _, message = validateGiftCardUserListQuery(map[string][]string{"status": {"bogus"}})
	if message != "must be equal to one of the allowed values" {
		t.Fatalf("message = %q", message)
	}
}

// ==================== gift cards: expiry parsing ====================

// TestParseGiftCardExpiresAt pins the accepted date forms and the rejection path.
func TestParseGiftCardExpiresAt(t *testing.T) {
	valid := []string{
		"2027-01-02T03:04:05Z",
		"2027-01-02T03:04:05.123Z",
		"2027-01-02T03:04:05+08:00",
		"2027-01-02T03:04:05",
		"2027-01-02T03:04",
		"2027-01-02 03:04:05",
		"2027-01-02",
	}
	for _, raw := range valid {
		if _, ok := parseGiftCardExpiresAt(raw); !ok {
			t.Errorf("parseGiftCardExpiresAt(%q) rejected a valid form", raw)
		}
	}

	invalid := []string{
		"",
		"not a date",
		"2027-13-45",
		"Jan 1 2027", // V8 legacy form: documented unsupported gap
	}
	for _, raw := range invalid {
		if _, ok := parseGiftCardExpiresAt(raw); ok {
			t.Errorf("parseGiftCardExpiresAt(%q) accepted an invalid form", raw)
		}
	}
}

// TestParseGiftCardExpiresAtNormalisesToUTC checks the millisecond/UTC
// normalisation the DB write relies on.
func TestParseGiftCardExpiresAtNormalisesToUTC(t *testing.T) {
	parsed, ok := parseGiftCardExpiresAt("2027-01-02T03:04:05+08:00")
	if !ok {
		t.Fatal("rejected a valid offset timestamp")
	}
	if parsed.Location().String() != "UTC" {
		t.Fatalf("location = %v, want UTC", parsed.Location())
	}
	if parsed.Nanosecond()%1_000_000 != 0 {
		t.Fatalf("timestamp not truncated to milliseconds: %v", parsed)
	}
}

// ==================== gift cards: JS string length ====================

// TestJSStringLengthCountsUTF16 pins the helper the schema length checks use:
// astral characters count as two code units, like String#length.
func TestJSStringLengthCountsUTF16(t *testing.T) {
	cases := []struct {
		value string
		want  int
	}{
		{"", 0},
		{"abc", 3},
		{"中文", 2},
		{"😀", 2}, // U+1F600 is a surrogate pair
		{"a😀b", 4},
	}
	for _, tc := range cases {
		if got := jsStringLength(tc.value); got != tc.want {
			t.Errorf("jsStringLength(%q) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

// ==================== gift cards: batch projection ====================

// TestGiftCardBatchCodeShape pins the narrowed projection of POST /admin/batch:
// exactly four keys, amounts as numbers.
func TestGiftCardBatchCodeShape(t *testing.T) {
	code := giftCardBatchCode{ID: 7, Code: "gc-abcdefghijklmnopqrstuvwx", FaceValue: 100, BalanceValue: 12.5}
	encoded := mustMarshalJSON(t, code)
	want := map[string]any{
		"id":           float64(7),
		"code":         "gc-abcdefghijklmnopqrstuvwx",
		"faceValue":    float64(100),
		"balanceValue": 12.5,
	}
	if len(encoded) != len(want) {
		t.Fatalf("keys = %v, want exactly %v", encoded, want)
	}
	for key, value := range want {
		if encoded[key] != value {
			t.Errorf("%s = %#v, want %#v", key, encoded[key], value)
		}
	}
}
