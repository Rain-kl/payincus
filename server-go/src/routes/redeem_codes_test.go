// Pure-logic tests for the redeem-code route port
// (server/src/routes/redeem-codes.ts).
//
// The DB-backed paths are covered by the dual-run conformance suite
// (server-go/test/conformance/requests-verification-redeem.json). This file pins
// the guard invariants that the Node `test-redeem-code-management-guards.ts` and
// `test-system-redeem-consistency.ts` suites pin in the TypeScript:
//
//   - `parsePositiveId` is `^\d+$` + JS-safe-integer, so "01" parses (unlike
//     lib.ParsePositiveRouteID) while "0"/"-1"/"1e3" are rejected;
//   - only c/r/d/t are accepted as code types (`p` is deprecated);
//   - remark normalization trims, caps at 200 characters (JSON-schema maxLength)
//     and rejects control characters;
//   - expiry normalization turns ""/null into "clear" and an unparseable string
//     into a hard 400;
//   - batch-delete ids must be unique positive integers and the AJV messages are
//     the ones setErrorHandler renders.
package routes

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestRedeemCodeRoutesRegisterWithoutConflict pins that Gin accepts the mixing of
// static and wildcard siblings these paths need (`.../redeem-codes/batch-delete`
// next to `.../redeem-codes/:codeId`). A panic here would take down the whole
// server at startup.
func TestRedeemCodeRoutesRegisterWithoutConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if err := registerRedeemCodeRoutesSafely(t, engine); err != nil {
		t.Fatalf("registration panicked: %v", err)
	}

	want := map[string]bool{
		"GET /api/hosts/:hostId/redeem-codes":                false,
		"POST /api/hosts/:hostId/redeem-codes":               false,
		"PATCH /api/hosts/:hostId/redeem-codes/:codeId":      false,
		"DELETE /api/hosts/:hostId/redeem-codes/:codeId":     false,
		"POST /api/hosts/:hostId/redeem-codes/batch-delete":  false,
		"GET /api/hosts/:hostId/redeem-codes/:codeId/usages": false,
		"GET /api/redeem-code-options":                       false,
	}
	for _, route := range engine.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Fatalf("route %q is not registered", key)
		}
	}
}

func registerRedeemCodeRoutesSafely(t *testing.T, engine *gin.Engine) (err error) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			switch typed := recovered.(type) {
			case error:
				err = typed
			case string:
				err = errors.New(typed)
			default:
				err = fmt.Errorf("%v", typed)
			}
		}
	}()
	RegisterRedeemCodes(engine)
	return nil
}

// TestParseRedeemCodePositiveID pins the route-local id parser.
func TestParseRedeemCodePositiveID(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
		ok   bool
	}{
		{"1", 1, true},
		{"7", 7, true},
		{"01", 1, true}, // `^\d+$` allows leading zeros -> Number("01") === 1
		{"00042", 42, true},
		{"9007199254740991", 9007199254740991, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"1.0", 0, false},
		{"1e3", 0, false},
		{"abc", 0, false},
		{"", 0, false},
		{" 1", 0, false},
		{"1 ", 0, false},
		{"9007199254740992", 0, false}, // 2^53, outside Number.isSafeInteger
	}

	for _, tc := range cases {
		got, ok := parseRedeemCodePositiveID(tc.raw)
		if ok != tc.ok {
			t.Fatalf("parseRedeemCodePositiveID(%q) ok = %v, want %v", tc.raw, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Fatalf("parseRedeemCodePositiveID(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// TestRedeemCodeJSInteger pins the Number.isSafeInteger mirror used by the
// normalizers.
func TestRedeemCodeJSInteger(t *testing.T) {
	cases := []struct {
		value any
		want  int64
		ok    bool
	}{
		{float64(1), 1, true},
		{float64(9007199254740991), 9007199254740991, true},
		{float64(0), 0, true},
		{float64(-5), -5, true},
		{float64(1.5), 0, false},
		{float64(9007199254740992), 0, false},
		{math.Inf(1), 0, false},
		{math.NaN(), 0, false},
		{"1", 0, false},
		{nil, 0, false},
		{true, 0, false},
	}

	for _, tc := range cases {
		got, ok := redeemCodeJSInteger(tc.value)
		if ok != tc.ok {
			t.Fatalf("redeemCodeJSInteger(%v) ok = %v, want %v", tc.value, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Fatalf("redeemCodeJSInteger(%v) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

// TestRedeemCodeOptionalRemark pins normalizeOptionalRedeemCodeRemark.
func TestRedeemCodeOptionalRemark(t *testing.T) {
	t.Run("absent stays absent", func(t *testing.T) {
		value, ok := redeemCodeOptionalRemark("whatever", false)
		if !ok || value != nil {
			t.Fatalf("absent remark = (%v, %v), want (nil, true)", value, ok)
		}
	})

	t.Run("blank collapses to absent", func(t *testing.T) {
		value, ok := redeemCodeOptionalRemark("   ", true)
		if !ok || value != nil {
			t.Fatalf("blank remark = (%v, %v), want (nil, true)", value, ok)
		}
	})

	t.Run("trimmed value kept", func(t *testing.T) {
		value, ok := redeemCodeOptionalRemark("  hello  ", true)
		if !ok || value == nil || *value != "hello" {
			t.Fatalf("remark = (%v, %v), want \"hello\"", value, ok)
		}
	})

	t.Run("control characters rejected", func(t *testing.T) {
		for _, raw := range []string{"a\x00b", "a\nb", "a\tb", "a\x7f b"} {
			if _, ok := redeemCodeOptionalRemark(raw, true); ok {
				t.Fatalf("remark %q must be rejected", raw)
			}
		}
	})

	t.Run("length cap", func(t *testing.T) {
		exactly := ""
		for i := 0; i < 200; i++ {
			exactly += "a"
		}
		if _, ok := redeemCodeOptionalRemark(exactly, true); !ok {
			t.Fatal("200-character remark must be accepted")
		}
		if _, ok := redeemCodeOptionalRemark(exactly+"a", true); ok {
			t.Fatal("201-character remark must be rejected")
		}
	})
}

// TestRedeemCodeOptionalExpiry pins normalizeOptionalExpiryDate.
func TestRedeemCodeOptionalExpiry(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		value, ok := redeemCodeOptionalExpiry(nil, false)
		if !ok || value != nil {
			t.Fatalf("absent = (%v, %v), want (nil, true)", value, ok)
		}
	})

	t.Run("null and blank clear the column", func(t *testing.T) {
		for _, text := range []*string{nil, stringPtr(""), stringPtr("   ")} {
			value, ok := redeemCodeOptionalExpiry(text, true)
			if !ok || value != nil {
				t.Fatalf("expiry %v = (%v, %v), want (nil, true)", text, value, ok)
			}
		}
	})

	t.Run("valid ISO timestamps parse", func(t *testing.T) {
		value, ok := redeemCodeOptionalExpiry(stringPtr("2027-01-02T03:04:05.000Z"), true)
		if !ok || value == nil {
			t.Fatalf("valid expiry = (%v, %v)", value, ok)
		}
		if got := value.UTC().Format("2006-01-02T15:04:05.000Z"); got != "2027-01-02T03:04:05.000Z" {
			t.Fatalf("parsed = %q", got)
		}
	})

	t.Run("garbage is a hard failure", func(t *testing.T) {
		for _, text := range []string{"not a date", "2027-13-45", "soon"} {
			if _, ok := redeemCodeOptionalExpiry(stringPtr(text), true); ok {
				t.Fatalf("expiry %q must be rejected", text)
			}
		}
	})
}

// TestRequireRedeemCodeInteger pins the INVALID_PARAMS messages.
func TestRequireRedeemCodeInteger(t *testing.T) {
	cases := []struct {
		value   any
		field   string
		min     int64
		max     int64
		want    int64
		message string
	}{
		{float64(5), "codeValue", 1, 100, 5, ""},
		{float64(1), "codeValue", 1, 100, 1, ""},
		{float64(100), "codeValue", 1, 100, 100, ""},
		{float64(0), "codeValue", 1, 100, 0, "codeValue must be between 1 and 100"},
		{float64(101), "codeValue", 1, 100, 0, "codeValue must be between 1 and 100"},
		{float64(1.5), "codeValue", 1, 100, 0, "codeValue must be an integer"},
		{"5", "codeValue", 1, 100, 0, "codeValue must be an integer"},
		{nil, "maxUses", 1, 1000, 0, "maxUses must be an integer"},
		{float64(1001), "maxUses", 1, 1000, 0, "maxUses must be between 1 and 1000"},
	}

	for _, tc := range cases {
		got, message := requireRedeemCodeInteger(tc.value, tc.field, tc.min, tc.max)
		if message != tc.message {
			t.Fatalf("requireRedeemCodeInteger(%v) message = %q, want %q", tc.value, message, tc.message)
		}
		if message == "" && got != tc.want {
			t.Fatalf("requireRedeemCodeInteger(%v) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

// TestNormalizeRedeemCodeCreateBodyRejectsPoints pins that create refuses `p`
// (points). The create schema enum is `['c','r','d','t']`, so AJV rejects it
// first (VALIDATION_ERROR); the normalizer keeps its own guard as a second line of
// defence, exactly like the TypeScript's normalizeRedeemCodeType.
func TestNormalizeRedeemCodeCreateBodyRejectsPoints(t *testing.T) {
	decoded := decodeRedeemCodeBody(t, `{"codeType":"p","codeValue":5}`)
	if _, message := validateRedeemCodeCreateBodyAJV(decoded); message != "must be equal to one of the allowed values" {
		t.Fatalf("AJV message = %q, want \"must be equal to one of the allowed values\"", message)
	}

	// The normalizer alone also rejects it (with its own INVALID_PARAMS message).
	validated := ajvValidated{
		Values:  map[string]any{"codeType": "p", "codeValue": float64(5)},
		Present: map[string]bool{"codeType": true, "codeValue": true},
	}
	if _, message := normalizeRedeemCodeCreateBody(validated); message != "Invalid redeem code type" {
		t.Fatalf("message = %q, want \"Invalid redeem code type\"", message)
	}
}

// TestNormalizeRedeemCodeCreateBodyValues pins a successful normalize.
func TestNormalizeRedeemCodeCreateBodyValues(t *testing.T) {
	body := `{"codeType":"c","codeValue":5,"maxUses":3,"batchCount":2,"expiresAt":null,"remark":"  hi  "}`
	decoded := decodeRedeemCodeBody(t, body)

	validated, validationMessage := validateAJVObject(decoded, []string{"codeType", "codeValue"}, redeemCodeCreateBodyFields)
	if validationMessage != "" {
		t.Fatalf("validation message = %q", validationMessage)
	}
	result, message := normalizeRedeemCodeCreateBody(validated)
	if message != "" {
		t.Fatalf("normalize message = %q", message)
	}

	if result.CodeType != "c" || result.CodeValue != 5 {
		t.Fatalf("type/value = %v/%d", result.CodeType, result.CodeValue)
	}
	if result.MaxUses == nil || *result.MaxUses != 3 {
		t.Fatalf("maxUses = %v", result.MaxUses)
	}
	if result.BatchCount == nil || *result.BatchCount != 2 {
		t.Fatalf("batchCount = %v", result.BatchCount)
	}
	if !result.ExpiresSeen || result.ExpiresAt != nil {
		t.Fatalf("expiresAt = (%v, seen=%v), want (nil, true)", result.ExpiresAt, result.ExpiresSeen)
	}
	if result.Remark == nil || *result.Remark != "hi" {
		t.Fatalf("remark = %v, want \"hi\"", result.Remark)
	}
}

// TestNormalizeRedeemCodeCreateBodyRejectsOutOfRangeValue pins the per-type range
// check (the schema only enforces `minimum: 1`).
func TestNormalizeRedeemCodeCreateBodyRejectsOutOfRangeValue(t *testing.T) {
	// CPU's max is 100.
	decoded := decodeRedeemCodeBody(t, `{"codeType":"c","codeValue":101}`)
	validated, validationMessage := validateAJVObject(decoded, []string{"codeType", "codeValue"}, redeemCodeCreateBodyFields)
	if validationMessage != "" {
		t.Fatalf("AJV must accept 101 (only minimum:1 is declared): %q", validationMessage)
	}
	if _, message := normalizeRedeemCodeCreateBody(validated); message != "codeValue must be between 1 and 100" {
		t.Fatalf("message = %q", message)
	}
}

// TestNormalizeRedeemCodeUpdateBody pins the update normalizer tri-state handling.
func TestNormalizeRedeemCodeUpdateBody(t *testing.T) {
	t.Run("empty body keeps everything absent", func(t *testing.T) {
		validated, message := validateAJVObject(map[string]any{}, nil, redeemCodeUpdateBodyFields)
		if message != "" {
			t.Fatalf("validation = %q", message)
		}
		body, normalizeMessage := normalizeRedeemCodeUpdateBody(validated)
		if normalizeMessage != "" {
			t.Fatalf("normalize = %q", normalizeMessage)
		}
		if body.Enabled != nil || body.RemarkSeen || body.MaxUses != nil || body.ExpiresSeen {
			t.Fatalf("empty body produced %+v", body)
		}
	})

	t.Run("full body", func(t *testing.T) {
		decoded := decodeRedeemCodeBody(t, `{"enabled":false,"remark":"note","maxUses":9,"expiresAt":""}`)
		validated, message := validateAJVObject(decoded, nil, redeemCodeUpdateBodyFields)
		if message != "" {
			t.Fatalf("validation = %q", message)
		}
		body, normalizeMessage := normalizeRedeemCodeUpdateBody(validated)
		if normalizeMessage != "" {
			t.Fatalf("normalize = %q", normalizeMessage)
		}
		if body.Enabled == nil || *body.Enabled {
			t.Fatalf("enabled = %v, want false", body.Enabled)
		}
		if !body.RemarkSeen || body.Remark == nil || *body.Remark != "note" {
			t.Fatalf("remark = %v (seen=%v)", body.Remark, body.RemarkSeen)
		}
		if body.MaxUses == nil || *body.MaxUses != 9 {
			t.Fatalf("maxUses = %v", body.MaxUses)
		}
		if !body.ExpiresSeen || body.ExpiresAt == nil || *body.ExpiresAt != "" {
			t.Fatalf("expiresAt = %v (seen=%v)", body.ExpiresAt, body.ExpiresSeen)
		}
	})

	t.Run("schema rejects non-boolean enabled", func(t *testing.T) {
		decoded := decodeRedeemCodeBody(t, `{"enabled":"yes"}`)
		if _, message := validateAJVObject(decoded, nil, redeemCodeUpdateBodyFields); message != "must be boolean" {
			t.Fatalf("message = %q, want \"must be boolean\"", message)
		}
	})
}

// TestPrepareRedeemCodeBatchDelete pins the AJV + uniqueness pipeline.
func TestPrepareRedeemCodeBatchDelete(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantIDs    []int64
		validation string
		params     string
	}{
		{"valid", `{"ids":[3,1,2]}`, []int64{3, 1, 2}, "", ""},
		{"scalar coerced into an array", `{"ids":7}`, []int64{7}, "", ""},
		{"missing ids", `{}`, nil, "ids is required", ""},
		{"non-object", `[]`, nil, "must be object", ""},
		{"empty list", `{"ids":[]}`, nil, "must NOT have fewer than 1 items", ""},
		{"too many", `{"ids":[` + repeatIDs(101) + `]}`, nil, "must NOT have more than 100 items", ""},
		{"zero id", `{"ids":[0]}`, nil, "must be >= 1", ""},
		{"negative id", `{"ids":[-2]}`, nil, "must be >= 1", ""},
		{"non-integer id", `{"ids":[1.5]}`, nil, "must be integer", ""},
		{"string id coerced", `{"ids":["5"]}`, []int64{5}, "", ""},
		{"duplicate ids", `{"ids":[2,2]}`, nil, "", "Invalid redeem code IDs"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decoded := decodeRedeemCodeBody(t, tc.body)
			ids, validationMessage, paramsMessage := prepareRedeemCodeBatchDelete(decoded)
			if validationMessage != tc.validation || paramsMessage != tc.params {
				t.Fatalf("messages = (%q, %q), want (%q, %q)",
					validationMessage, paramsMessage, tc.validation, tc.params)
			}
			if tc.validation != "" || tc.params != "" {
				return
			}
			if len(ids) != len(tc.wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
			}
			for i := range tc.wantIDs {
				if ids[i] != tc.wantIDs[i] {
					t.Fatalf("ids = %v, want %v", ids, tc.wantIDs)
				}
			}
		})
	}
}

// TestRedeemCodeQueryInteger pins the querystring coercion/defaults/bounds.
func TestRedeemCodeQueryInteger(t *testing.T) {
	cases := []struct {
		name     string
		query    url.Values
		key      string
		def      int
		min, max float64
		want     int
		message  string
	}{
		{"absent uses the default", url.Values{}, "limit", 50, 1, 100, 50, ""},
		{"in range", url.Values{"limit": {"20"}}, "limit", 50, 1, 100, 20, ""},
		{"above max", url.Values{"limit": {"101"}}, "limit", 50, 1, 100, 0, "must be <= 100"},
		{"below min", url.Values{"limit": {"0"}}, "limit", 50, 1, 100, 0, "must be >= 1"},
		{"non numeric", url.Values{"limit": {"abc"}}, "limit", 50, 1, 100, 0, "must be integer"},
		{"fractional", url.Values{"offset": {"1.5"}}, "offset", 0, 0, math.NaN(), 0, "must be integer"},
		{"zero offset allowed", url.Values{"offset": {"0"}}, "offset", 0, 0, math.NaN(), 0, ""},
		{"large offset allowed", url.Values{"offset": {"1000000"}}, "offset", 0, 0, math.NaN(), 1000000, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, message := redeemCodeQueryInteger(tc.query, tc.key, tc.def, tc.min, tc.max)
			if message != tc.message {
				t.Fatalf("message = %q, want %q", message, tc.message)
			}
			if message == "" && value != tc.want {
				t.Fatalf("value = %d, want %d", value, tc.want)
			}
		})
	}
}

// TestRedeemCodeQueryEnum pins the enabled query flag.
func TestRedeemCodeQueryEnum(t *testing.T) {
	value, present, message := redeemCodeQueryEnum(url.Values{"enabled": {"true"}}, "enabled", []string{"true", "false"})
	if message != "" || !present || value != "true" {
		t.Fatalf("enabled=true = (%q, %v, %q)", value, present, message)
	}

	if _, present, _ := redeemCodeQueryEnum(url.Values{}, "enabled", []string{"true", "false"}); present {
		t.Fatal("absent enabled must report present=false")
	}

	if _, _, message := redeemCodeQueryEnum(url.Values{"enabled": {"yes"}}, "enabled", []string{"true", "false"}); message != "must be equal to one of the allowed values" {
		t.Fatalf("message = %q", message)
	}
}

// TestRedeemCodeRootArrayCoercion pins AJV's root `coerceTypes:'array'` unwrap.
func TestRedeemCodeRootArrayCoercion(t *testing.T) {
	if got := unwrapAJVRootArray([]any{map[string]any{"a": 1}}); got == nil {
		t.Fatal("single-element array must unwrap to its element")
	}
	object := map[string]any{"a": 1}
	if got, ok := unwrapAJVRootArray(object).(map[string]any); !ok || len(got) != 1 {
		t.Fatalf("object must pass through unchanged, got %#v", got)
	}
	if got, ok := unwrapAJVRootArray([]any{1, 2}).([]any); !ok || len(got) != 2 {
		t.Fatalf("multi-element array must pass through unchanged, got %#v", got)
	}
}

// ==================== test helpers ====================

func decodeRedeemCodeBody(t *testing.T, body string) any {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return decoded
}

func stringPtr(value string) *string { return &value }

func repeatIDs(count int) string {
	result := ""
	for i := 0; i < count; i++ {
		if i > 0 {
			result += ","
		}
		result += "1"
	}
	return result
}
