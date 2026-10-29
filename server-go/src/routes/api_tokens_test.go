// Pure-logic tests for the api-tokens port (server/src/routes/api-tokens.ts).
//
// The DB-backed paths are covered by the dual-run conformance suite
// (server-go/test/conformance/requests-api-tokens.json); everything anchored here
// is the credential-handling invariants that must never regress:
//
//   - the plaintext token is returned exactly once and never appears in the
//     serialized token view, the audit log line, or the stored digest;
//   - only a SHA-256 digest is persisted;
//   - scope normalization matches normalizePublicApiScopes;
//   - route ids are parsed strictly (^[1-9]\d*$, JS-safe range);
//   - the request body pipeline mirrors Fastify's content-type/JSON errors.
package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// TestAPITokenPlaintextAppearsOnlyInCreateResponse pins the one-time-return rule:
// the generated plaintext is a `pat_` token whose digest is what gets stored, and
// the serialized token view carries neither the plaintext nor the digest.
func TestAPITokenPlaintextAppearsOnlyInCreateResponse(t *testing.T) {
	rawToken := lib.GeneratePublicAPIToken()
	if !strings.HasPrefix(rawToken, lib.PublicAPITokenPrefix) {
		t.Fatalf("token = %q, want pat_ prefix", rawToken)
	}
	// 4 prefix chars + 32 random bytes as unpadded base64url (43 chars).
	if len(rawToken) != len(lib.PublicAPITokenPrefix)+43 {
		t.Fatalf("token length = %d, want %d", len(rawToken), len(lib.PublicAPITokenPrefix)+43)
	}

	tokenHash := lib.HashPublicAPIToken(rawToken)
	if len(tokenHash) != 64 || strings.ToLower(tokenHash) != tokenHash {
		t.Fatalf("hash = %q, want 64 lowercase hex chars", tokenHash)
	}
	if strings.Contains(tokenHash, rawToken) || tokenHash == rawToken {
		t.Fatal("stored digest must not contain the plaintext token")
	}
	if tokenHash != lib.HashPublicAPIToken(rawToken) {
		t.Fatal("hash is not deterministic")
	}
	if got := lib.GetPublicAPITokenPrefix(rawToken); got != rawToken[:16] {
		t.Fatalf("prefix = %q, want %q", got, rawToken[:16])
	}

	// The digest is exactly what a create call persists; the view rendered from
	// the stored row must not leak it.
	view := apiTokenViewFromRow(apiTokenRow{
		ID:          7,
		UserID:      3,
		Name:        "ci",
		TokenPrefix: lib.GetPublicAPITokenPrefix(rawToken),
		TokenHash:   tokenHash,
		Scopes:      []byte(`["profile:read"]`),
		CreatedAt:   time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt:   time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC),
	})

	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	payload := string(encoded)
	for _, secret := range []string{rawToken, tokenHash, "tokenHash"} {
		if strings.Contains(payload, secret) {
			t.Fatalf("serialized token view leaks %q: %s", secret, payload)
		}
	}
	if !strings.Contains(payload, `"tokenPrefix":"`+rawToken[:16]+`"`) {
		t.Fatalf("serialized token view lost the display prefix: %s", payload)
	}
}

// TestAPITokenAuditLogNeverContainsPlaintext pins the redaction of the two
// createLog() messages: they embed the name and scopes only.
func TestAPITokenAuditLogNeverContainsPlaintext(t *testing.T) {
	rawToken := lib.GeneratePublicAPIToken()
	tokenHash := lib.HashPublicAPIToken(rawToken)
	scopes := []lib.PublicAPIScope{lib.PublicAPIScopeProfileRead, lib.PublicAPIScopeTicketsWrite}

	created := apiTokenCreateLogContent("deploy bot", scopes)
	if created != `Created public API token "deploy bot" with scopes: profile:read, tickets:write` {
		t.Fatalf("create log = %q", created)
	}
	revoked := apiTokenRevokeLogContent("deploy bot")
	if revoked != `Revoked public API token "deploy bot"` {
		t.Fatalf("revoke log = %q", revoked)
	}
	for _, line := range []string{created, revoked} {
		for _, secret := range []string{rawToken, tokenHash, rawToken[:16]} {
			if strings.Contains(line, secret) {
				t.Fatalf("audit log %q leaks %q", line, secret)
			}
		}
	}
}

// TestNormalizeAPITokenExpiresAt covers normalizeExpiresAt's tri-state contract.
func TestNormalizeAPITokenExpiresAt(t *testing.T) {
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	future := "2027-06-01T12:30:00.250Z"

	cases := []struct {
		name    string
		value   any
		wantNil bool
		wantOK  bool
		want    string
	}{
		{name: "undefined", value: nil, wantNil: true, wantOK: true},
		{name: "empty string", value: "", wantNil: true, wantOK: true},
		{name: "number", value: float64(1), wantNil: false, wantOK: false},
		{name: "bool", value: true, wantNil: false, wantOK: false},
		{name: "array", value: []any{"2027-06-01"}, wantNil: false, wantOK: false},
		{name: "object", value: map[string]any{}, wantNil: false, wantOK: false},
		{name: "garbage", value: "not-a-date", wantNil: false, wantOK: false},
		{name: "past", value: "2026-12-31T23:59:59Z", wantNil: false, wantOK: false},
		{name: "exactly now", value: "2027-01-01T00:00:00Z", wantNil: false, wantOK: false},
		{name: "valid", value: future, wantNil: false, wantOK: true, want: "2027-06-01T12:30:00.250Z"},
		{name: "valid date only", value: "2027-06-01", wantNil: false, wantOK: true, want: "2027-06-01T00:00:00.000Z"},
		{name: "valid offset", value: "2027-06-01T20:30:00+08:00", wantNil: false, wantOK: true, want: "2027-06-01T12:30:00.000Z"},
		{name: "invalid month", value: "2027-13-01T00:00:00Z", wantNil: false, wantOK: false},
		{name: "invalid hour", value: "2027-06-01T25:00:00Z", wantNil: false, wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeAPITokenExpiresAt(tc.value, now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantNil {
				if got != nil {
					t.Fatalf("value = %v, want nil", got)
				}
				return
			}
			if !tc.wantOK {
				return
			}
			if got == nil {
				t.Fatalf("value = nil, want %s", tc.want)
			}
			if rendered := apiTokenISOTime(*got); rendered != tc.want {
				t.Fatalf("value = %s, want %s", rendered, tc.want)
			}
		})
	}
}

// TestParseAPITokenJSDatetimeMatchesV8 pins the subset of `new Date(value)` the
// module accepts. Behaviour was verified request-by-request against the running
// Node backend; the two notable rules are that a date-only string is UTC while a
// date-time without an offset is local, and that out-of-range components roll
// over the way V8 rolls them over.
func TestParseAPITokenJSDatetimeMatchesV8(t *testing.T) {
	previousLocal := time.Local
	// V8's legacy parser reads wall-clock time in the process zone; pin it so the
	// expectations are deterministic regardless of the machine running the tests.
	time.Local = time.FixedZone("CST", 8*3600)
	defer func() { time.Local = previousLocal }()

	cases := []struct {
		input string
		want  string
		ok    bool
	}{
		{"2027-01-01", "2027-01-01T00:00:00.000Z", true},
		{"2027-01", "2027-01-01T00:00:00.000Z", true},
		{"2027", "2027-01-01T00:00:00.000Z", true},
		{"2027-01-01T00:00:00Z", "2027-01-01T00:00:00.000Z", true},
		{"2027-01-01T00:00:00", "2026-12-31T16:00:00.000Z", true},
		{"2027-01-01T00:00:00+08:00", "2026-12-31T16:00:00.000Z", true},
		{"2027-01-01T00:00:00+0800", "2026-12-31T16:00:00.000Z", true},
		{"2027-01-01T00:00Z", "2027-01-01T00:00:00.000Z", true},
		{"2027-01-01T00:00:00.1Z", "2027-01-01T00:00:00.100Z", true},
		{"2027-06-01T12:30:00.123456789Z", "2027-06-01T12:30:00.123Z", true},
		{"2027-02-30", "2027-03-02T00:00:00.000Z", true},
		{"2027-01-01T24:00:00Z", "2027-01-02T00:00:00.000Z", true},
		{"+002027-01-01T00:00:00Z", "2027-01-01T00:00:00.000Z", true},
		// Legacy fallback parser (unpadded / alternate separators / space time).
		{"2027-1-1", "2026-12-31T16:00:00.000Z", true},
		{"2027/1/1", "2026-12-31T16:00:00.000Z", true},
		{"2027.1.1", "2026-12-31T16:00:00.000Z", true},
		{"2027-1", "2026-12-31T16:00:00.000Z", true},
		{"2027-0001-01", "2026-12-31T16:00:00.000Z", true},
		{"2027-1-1 00:00:00", "2026-12-31T16:00:00.000Z", true},
		{"2027-1-1 1:2:3", "2026-12-31T17:02:03.000Z", true},
		{"2027-1-1 12:30:45.123", "2027-01-01T04:30:45.123Z", true},
		{"2027-1-1 00:00:00Z", "2027-01-01T00:00:00.000Z", true},
		{"2027-1-1 00:00:00 GMT", "2027-01-01T00:00:00.000Z", true},
		{"2027-1-1 24:00", "2027-01-01T16:00:00.000Z", true},
		// Rejected by V8 (and therefore left as INVALID_EXPIRES_AT).
		{"2027-1-1T00:00:00", "", false},
		{"2027-01-01T00:00:00,5Z", "", false},
		{"2027-01-01T00:00:00+08", "", false},
		{"2027-01-01T", "", false},
		{"2027-01-01T00:00:00.Z", "", false},
		{"2027-01-32", "", false},
		{"2027-01-00", "", false},
		{"2027-00-10", "", false},
		{"2027-01-01T24:00:01Z", "", false},
		{"2027-01-01T25:00:00Z", "", false},
		{"2027-1-1 25:00", "", false},
		{"2027-01-01 12", "", false},
		{"2027-W01-1", "", false},
		{"", "", false},
		{"not-a-date", "", false},
	}

	for _, tc := range cases {
		got, ok := parseAPITokenJSDatetime(tc.input)
		if ok != tc.ok {
			t.Fatalf("parse(%q) ok = %v, want %v", tc.input, ok, tc.ok)
		}
		if !ok {
			continue
		}
		if rendered := apiTokenISOTime(got); rendered != tc.want {
			t.Fatalf("parse(%q) = %s, want %s", tc.input, rendered, tc.want)
		}
	}
}

// TestAPITokenScopeNormalization pins the shared scope normalizer the POST route
// relies on: unknown / duplicate / empty / oversized lists are all rejected.
func TestAPITokenScopeNormalization(t *testing.T) {
	if got := lib.NormalizePublicAPIScopes([]any{"profile:read", "tickets:write"}); got == nil || len(got) != 2 {
		t.Fatalf("valid scopes = %#v", got)
	}
	if got := lib.NormalizePublicAPIScopes([]any{"profile:read", "profile:read"}); got != nil {
		t.Fatalf("duplicate scopes must be rejected, got %#v", got)
	}
	if got := lib.NormalizePublicAPIScopes([]any{}); got != nil {
		t.Fatalf("empty scopes must be rejected, got %#v", got)
	}
	if got := lib.NormalizePublicAPIScopes([]any{"admin:everything"}); got != nil {
		t.Fatalf("unknown scope must be rejected, got %#v", got)
	}
	if got := lib.NormalizePublicAPIScopes([]any{float64(1)}); got != nil {
		t.Fatalf("non-string scope must be rejected, got %#v", got)
	}
	if got := lib.NormalizePublicAPIScopes("profile:read"); got != nil {
		t.Fatalf("non-array scopes must be rejected, got %#v", got)
	}
	tooMany := make([]any, lib.PublicAPITokenMaxScopes+1)
	for i := range tooMany {
		tooMany[i] = "profile:read"
	}
	if got := lib.NormalizePublicAPIScopes(tooMany); got != nil {
		t.Fatalf("oversized scope list must be rejected, got %#v", got)
	}
}

// TestAPITokenNameNormalization pins normalizePublicApiTokenName: trim, reject
// empty and over-long names, reject non-strings.
func TestAPITokenNameNormalization(t *testing.T) {
	if got := lib.NormalizePublicAPITokenName("  deploy bot  "); got == nil || *got != "deploy bot" {
		t.Fatalf("name = %#v", got)
	}
	for _, bad := range []any{nil, "", "   ", float64(5), true, []any{"x"}, strings.Repeat("a", lib.PublicAPITokenMaxNameLength+1)} {
		if got := lib.NormalizePublicAPITokenName(bad); got != nil {
			t.Fatalf("name %#v must be rejected, got %q", bad, *got)
		}
	}
	if got := lib.NormalizePublicAPITokenName(strings.Repeat("a", lib.PublicAPITokenMaxNameLength)); got == nil {
		t.Fatal("80-character name must be accepted")
	}
}

// TestAPITokenRouteIDParsing pins the strict id parse used by DELETE /:id; a bare
// strconv.Atoi would wrongly accept "0", "-1", "01" and "1.5".
func TestAPITokenRouteIDParsing(t *testing.T) {
	valid := map[string]int64{
		"1":                1,
		"42":               42,
		"9007199254740991": 9007199254740991,
	}
	for raw, want := range valid {
		if got, ok := lib.ParsePositiveRouteID(raw); !ok || got != want {
			t.Fatalf("ParsePositiveRouteID(%q) = (%d, %v), want (%d, true)", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "0", "00", "01", "-1", "+1", "1.0", "1e3", "abc", " 1", "1 ", "9007199254740992", "9007199254740993"} {
		if got, ok := lib.ParsePositiveRouteID(raw); ok {
			t.Fatalf("ParsePositiveRouteID(%q) = (%d, true), want rejection", raw, got)
		}
	}
}

// TestAPITokenContentTypeParsing pins ContentTypeParser#getParser's prefix match.
func TestAPITokenContentTypeParsing(t *testing.T) {
	cases := map[string]string{
		"application/json":                "json",
		"application/json; charset=utf-8": "json",
		"APPLICATION/JSON":                "json",
		"application/json ; charset":      "json",
		"text/plain":                      "text",
		"text/plain; charset=utf-8":       "text",
		"multipart/form-data; boundary=x": "multipart",
		"application/jsonx":               "",
		"application/vnd.api+json":        "",
		"application/octet-stream":        "",
		"text/html":                       "",
		"":                                "",
	}
	for contentType, want := range cases {
		if got := apiTokenBodyParserKind(contentType); got != want {
			t.Fatalf("parser(%q) = %q, want %q", contentType, got, want)
		}
	}
}

// TestAPITokenPrototypePoisoningRejected pins secure-json-parse's protoAction /
// constructorAction 'error' behaviour (Fastify answers "not valid JSON").
func TestAPITokenPrototypePoisoningRejected(t *testing.T) {
	rejected := []string{
		`{"__proto__":{"admin":true}}`,
		`{"a":{"__proto__":{"admin":true}}}`,
		`{"constructor":{"prototype":{"admin":true}}}`,
		`[{"__proto__":1}]`,
	}
	for _, payload := range rejected {
		var decoded any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("decode %s: %v", payload, err)
		}
		if !apiTokenHasForbiddenPrototypeKey(decoded) {
			t.Fatalf("%s must be rejected", payload)
		}
	}

	accepted := []string{
		`{"constructor":5}`,
		`{"constructor":"x"}`,
		`{"a":{"constructor":{}}}`,
		`{"prototype":{}}`,
		`{"name":"x"}`,
	}
	for _, payload := range accepted {
		var decoded any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("decode %s: %v", payload, err)
		}
		if apiTokenHasForbiddenPrototypeKey(decoded) {
			t.Fatalf("%s must be accepted", payload)
		}
	}
}

// newAPITokenBodyContext builds a gin context carrying the given raw request so
// readAPITokenBody can be exercised without a live server.
func newAPITokenBodyContext(t *testing.T, method, contentType, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/api/api-tokens", strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.ContentLength = int64(len(body))
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	return context
}

// TestReadAPITokenBody covers the Fastify body-parser errors that precede the
// handler, plus the "no body" and empty-object cases.
func TestReadAPITokenBody(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		contentType string
		body        string
		wantStatus  int
		wantMessage string
		wantPresent bool
	}{
		{name: "no body at all", method: http.MethodPost, wantPresent: false},
		{name: "empty json", method: http.MethodPost, contentType: "application/json", wantStatus: 400, wantMessage: apiTokenEmptyJSONBodyMessage},
		{name: "malformed json", method: http.MethodPost, contentType: "application/json", body: "{bad", wantStatus: 400, wantMessage: apiTokenInvalidJSONBodyMessage},
		{name: "json object", method: http.MethodPost, contentType: "application/json", body: `{"name":"x"}`, wantPresent: true},
		{name: "json boms", method: http.MethodPost, contentType: "application/json", body: "\xef\xbb\xbf{}", wantPresent: true},
		{name: "json bom only", method: http.MethodPost, contentType: "application/json", body: "\xef\xbb\xbf", wantStatus: 400, wantMessage: apiTokenInvalidJSONBodyMessage},
		{name: "json whitespace only", method: http.MethodPost, contentType: "application/json", body: "  ", wantStatus: 400, wantMessage: apiTokenInvalidJSONBodyMessage},
		{name: "multipart leaves body undefined", method: http.MethodPost, contentType: "multipart/form-data; boundary=x", body: "--x--\r\n", wantPresent: false},
		{name: "json null", method: http.MethodPost, contentType: "application/json", body: "null", wantPresent: true},
		{name: "text plain", method: http.MethodPost, contentType: "text/plain", body: `{}`, wantPresent: true},
		{name: "unsupported type", method: http.MethodPost, contentType: "application/x-www-form-urlencoded", body: "a=1", wantStatus: 415, wantMessage: "Unsupported Media Type: application/x-www-form-urlencoded"},
		{name: "body without content type", method: http.MethodPost, body: "{}", wantStatus: 415, wantMessage: "Unsupported Media Type: undefined"},
		{name: "delete invalid json", method: http.MethodDelete, contentType: "application/json", body: "{bad", wantStatus: 400, wantMessage: apiTokenInvalidJSONBodyMessage},
		{name: "get ignores body", method: http.MethodGet, contentType: "application/x-www-form-urlencoded", body: "a=1", wantPresent: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			context := newAPITokenBodyContext(t, tc.method, tc.contentType, tc.body)
			body, bodyErr := readAPITokenBody(context)

			if tc.wantStatus != 0 {
				if bodyErr == nil {
					t.Fatalf("expected error %d, got none", tc.wantStatus)
				}
				if bodyErr.Status != tc.wantStatus {
					t.Fatalf("status = %d, want %d", bodyErr.Status, tc.wantStatus)
				}
				if bodyErr.Message != tc.wantMessage {
					t.Fatalf("message = %q, want %q", bodyErr.Message, tc.wantMessage)
				}
				return
			}
			if bodyErr != nil {
				t.Fatalf("unexpected error: %+v", bodyErr)
			}
			if body.Present != tc.wantPresent {
				t.Fatalf("present = %v, want %v", body.Present, tc.wantPresent)
			}
		})
	}
}

// TestAPITokenFieldAccess pins `request.body?.key` semantics: only an object body
// exposes fields, so a string/array/null body never yields a value.
func TestAPITokenFieldAccess(t *testing.T) {
	object := apiTokenBody{Value: map[string]any{"name": "x"}, Present: true}
	if got := apiTokenField(object, "name"); got != "x" {
		t.Fatalf("name = %#v", got)
	}
	for _, body := range []apiTokenBody{
		{},
		{Value: "string", Present: true},
		{Value: []any{"x"}, Present: true},
		{Value: nil, Present: true},
	} {
		if got := apiTokenField(body, "name"); got != nil {
			t.Fatalf("field from %#v = %#v, want nil", body, got)
		}
	}
}

// TestAPITokenScopesJSON pins `Array.isArray(token.scopes) ? token.scopes : []`.
func TestAPITokenScopesJSON(t *testing.T) {
	if got := apiTokenScopesJSON([]byte(`["profile:read"]`)); len(got) != 1 || got[0] != "profile:read" {
		t.Fatalf("scopes = %#v", got)
	}
	for _, raw := range [][]byte{nil, {}, []byte(`null`), []byte(`"x"`), []byte(`{`), []byte(`{"a":1}`)} {
		got := apiTokenScopesJSON(raw)
		if got == nil || len(got) != 0 {
			t.Fatalf("scopes(%q) = %#v, want empty non-nil array", raw, got)
		}
	}
}

// TestAPITokenViewTimeFormatting pins the Date#toISOString rendering, including
// the null-preserving optional timestamps.
func TestAPITokenViewTimeFormatting(t *testing.T) {
	stamp := time.Date(2027, 1, 2, 3, 4, 5, 600_000_000, time.UTC)
	if got := apiTokenISOTime(stamp); got != "2027-01-02T03:04:05.600Z" {
		t.Fatalf("iso = %q", got)
	}
	if got := apiTokenOptionalISOTime(nil); got != nil {
		t.Fatalf("nil time = %v, want nil", *got)
	}
	rendered := apiTokenOptionalISOTime(&stamp)
	if rendered == nil || *rendered != "2027-01-02T03:04:05.600Z" {
		t.Fatalf("optional iso = %#v", rendered)
	}
}
