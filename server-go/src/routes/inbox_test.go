// Unit tests for the inbox route port. They pin the parts that must match the
// TypeScript module exactly without touching PostgreSQL: the strict query/route
// ID parsing, the shared message normalizer, the Fastify/AJV body schema of the
// host-notify route, and every demo-redaction entry point.
package routes

import (
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

// TestSingleQueryValue pins the repeated-parameter rule: Fastify's querystring
// parser yields an array for `?a=1&a=2`, and every strict check in these routes
// then fails, so a repeated key behaves like an absent one. Go's url.Values.Get
// would silently pick the first value instead.
func TestSingleQueryValue(t *testing.T) {
	query := url.Values{
		"page":      {"2"},
		"repeated":  {"1", "2"},
		"empty":     {""},
		"emptyPair": {"", ""},
		"isRead":    {"true"},
	}
	cases := []struct {
		key  string
		want string
	}{
		{"page", "2"},
		{"absent", ""},
		{"repeated", ""},
		{"empty", ""},
		{"emptyPair", ""},
		{"isRead", "true"},
	}
	for _, tc := range cases {
		if got := singleQueryValue(query, tc.key); got != tc.want {
			t.Errorf("singleQueryValue(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// TestInboxParsePositiveInteger pins parsePositiveInteger's strict pattern plus
// the `Math.min(100, ...)` page-size clamp.
func TestInboxParsePositiveInteger(t *testing.T) {
	cases := []struct {
		value    string
		fallback int64
		want     int64
	}{
		{"", 1, 1},
		{"0", 1, 1},                // zero is rejected by ^[1-9]\d*$
		{"-3", 7, 7},               // negative is rejected
		{"01", 1, 1},               // leading zero is rejected
		{"abc", 1, 1},              // non-numeric
		{"3", 1, 3},                // valid
		{"1.5", 1, 1},              // not an integer
		{" 5", 1, 1},               // whitespace is not trimmed in the strict parser
		{"9007199254740993", 1, 1}, // > Number.MAX_SAFE_INTEGER
		{"100000", 1, 100000},
	}
	for _, tc := range cases {
		if got := inboxParsePositiveInteger(tc.value, tc.fallback); got != tc.want {
			t.Errorf("inboxParsePositiveInteger(%q, %d) = %d, want %d", tc.value, tc.fallback, got, tc.want)
		}
	}

	// pageSize defaults to 20 and is clamped to 100.
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"", 20},
		{"abc", 20},
		{"0", 20},
		{"5", 5},
		{"100", 100},
		{"101", 100},
		{"999999", 100},
	} {
		if got := inboxParsePageSize(tc.value); got != tc.want {
			t.Errorf("inboxParsePageSize(%q) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

// TestInboxParseRouteID pins the route-ID parser, including the int32 bound.
func TestInboxParseRouteID(t *testing.T) {
	cases := []struct {
		value string
		want  int32
		ok    bool
	}{
		{"1", 1, true},
		{"2147483647", 2147483647, true},
		{"0", 0, false},
		{"01", 0, false},
		{"-1", 0, false},
		{"1.5", 0, false},
		{"abc", 0, false},
		{"2147483648", 0, false}, // above int4: cannot be a row id
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := inboxParseRouteID(tc.value)
		if got != tc.want || ok != tc.ok {
			t.Errorf("inboxParseRouteID(%q) = (%d,%v), want (%d,%v)", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

// TestInboxParsePositiveIDArray pins parsePositiveIdArray: absent stays nil,
// invalid elements reject the whole array, duplicates collapse in first-seen
// order.
func TestInboxParsePositiveIDArray(t *testing.T) {
	if ids, ok := inboxParsePositiveIDArray(nil, false); !ok || ids != nil {
		t.Fatalf("absent array = (%v,%v), want (nil,true)", ids, ok)
	}

	ids, ok := inboxParsePositiveIDArray([]float64{5, 5, 3, 1}, true)
	if !ok || !reflect.DeepEqual(ids, []int32{5, 3, 1}) {
		t.Fatalf("dedupe = (%v,%v)", ids, ok)
	}

	for _, invalid := range [][]float64{
		{0},
		{-1},
		{1.5},
		{2147483648},
		{9007199254740993},
		{1, 0},
	} {
		if ids, ok := inboxParsePositiveIDArray(invalid, true); ok || ids != nil {
			t.Errorf("%v must be rejected, got (%v,%v)", invalid, ids, ok)
		}
	}

	// An explicitly empty array is valid and yields an empty (non-nil) slice, which
	// the DB layer treats as "no instance filter".
	if ids, ok := inboxParsePositiveIDArray([]float64{}, true); !ok || ids == nil || len(ids) != 0 {
		t.Fatalf("empty array = (%v,%v)", ids, ok)
	}
}

// TestInboxNormalizeMessageInput pins the shared send-body validator used by all
// four send routes.
func TestInboxNormalizeMessageInput(t *testing.T) {
	cases := []struct {
		name    string
		body    any
		wantErr string
	}{
		{"nil body", nil, inboxTitleRequiredCode},
		{"array body", []any{}, inboxTitleRequiredCode},
		{"string body", "x", inboxTitleRequiredCode},
		{"number body", float64(1), inboxTitleRequiredCode},
		{"empty object", map[string]any{}, inboxTitleRequiredCode},
		{"non-string title", map[string]any{"title": float64(1), "content": "c"}, inboxTitleRequiredCode},
		{"blank title", map[string]any{"title": "   ", "content": "c"}, inboxTitleRequiredCode},
		{"missing content", map[string]any{"title": "t"}, inboxContentRequiredCode},
		{"non-string content", map[string]any{"title": "t", "content": nil}, inboxContentRequiredCode},
		{"blank content", map[string]any{"title": "t", "content": "\n\t "}, inboxContentRequiredCode},
		{"title too long", map[string]any{"title": strings.Repeat("x", 201), "content": "c"}, inboxTitleTooLongCode},
		{"content too long", map[string]any{"title": "t", "content": strings.Repeat("x", 5001)}, inboxContentTooLongCode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, inputError := inboxNormalizeMessageInput(tc.body)
			if inputError == nil {
				t.Fatalf("expected %s", tc.wantErr)
			}
			if inputError.Code != tc.wantErr {
				t.Fatalf("code = %q, want %q", inputError.Code, tc.wantErr)
			}
		})
	}

	// The accepted value is trimmed, and length is measured in UTF-16 code units:
	// 100 emoji are 200 units and therefore still fit.
	normalized, inputError := inboxNormalizeMessageInput(map[string]any{
		"title":   "  hello  ",
		"content": "\n body \n",
	})
	if inputError != nil {
		t.Fatalf("unexpected error %#v", inputError)
	}
	if normalized.Title != "hello" || normalized.Content != "body" {
		t.Fatalf("normalized = %#v", normalized)
	}

	if _, inputError := inboxNormalizeMessageInput(map[string]any{
		"title": strings.Repeat("😀", 100), "content": "c",
	}); inputError != nil {
		t.Fatalf("200 UTF-16 units must fit in a 200-char title: %#v", inputError)
	}
	if _, inputError := inboxNormalizeMessageInput(map[string]any{
		"title": strings.Repeat("😀", 101), "content": "c",
	}); inputError == nil || inputError.Code != inboxTitleTooLongCode {
		t.Fatalf("202 UTF-16 units must be rejected, got %#v", inputError)
	}
}

// TestInboxJSTrim pins ECMAScript's trim set, which differs from unicode.IsSpace
// (U+FEFF in particular).
func TestInboxJSTrim(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  a  ", "a"},
		{"\t\na\r\n", "a"},
		{"\u00a0a\u00a0", "a"},
		{"\u3000a\u3000", "a"},
		{"\ufeffa\ufeff", "a"},
		{"\u2003a\u2003", "a"},
		{"\va\f", "a"},
		{"a", "a"},
		{"a b", "a b"},
	}
	for _, tc := range cases {
		if got := inboxJSTrim(tc.in); got != tc.want {
			t.Errorf("inboxJSTrim(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestInboxValidateNotifyBody pins the Fastify/AJV body schema of
// POST /hosts/:hostId/notify. Verified against a real @fastify/ajv-compiler
// instance: allErrors:false (first error only), required before properties,
// coerceTypes:'array', and AJV's keyword order (items after minItems/maxItems).
func TestInboxValidateNotifyBody(t *testing.T) {
	valid := map[string]any{"title": "t", "content": "c"}
	cases := []struct {
		name string
		body any
		want string
	}{
		{"non-object body", []any{}, "must be object"},
		{"null body", nil, "must be object"},
		{"string body", "x", "must be object"},
		{"empty object", map[string]any{}, "title is required"},
		{"missing content", map[string]any{"title": "t"}, "content is required"},
		{"title empty", map[string]any{"title": "", "content": "c"}, "title is too short"},
		{"title null coerces to empty", map[string]any{"title": nil, "content": "c"}, "title is too short"},
		{"title too long", map[string]any{"title": strings.Repeat("x", 201), "content": "c"}, "title is too long"},
		{"content empty", map[string]any{"title": "t", "content": ""}, "content is too short"},
		{"content too long", map[string]any{"title": "t", "content": strings.Repeat("x", 5001)}, "content is too long"},
		{"title object", map[string]any{"title": map[string]any{}, "content": "c"}, "must be string"},
		{"instanceIds object", map[string]any{"title": "t", "content": "c", "instanceIds": map[string]any{"a": float64(1)}}, "must be array"},
		{"instanceIds empty", map[string]any{"title": "t", "content": "c", "instanceIds": []any{}}, "must NOT have fewer than 1 items"},
		{"instanceIds too many", map[string]any{"title": "t", "content": "c", "instanceIds": hundredAndOne()}, "must NOT have more than 100 items"},
		{"instanceIds zero", map[string]any{"title": "t", "content": "c", "instanceIds": []any{float64(0)}}, "must be >= 1"},
		{"instanceIds float", map[string]any{"title": "t", "content": "c", "instanceIds": []any{1.5}}, "must be integer"},
		{"instanceIds bad string", map[string]any{"title": "t", "content": "c", "instanceIds": []any{"abc"}}, "must be integer"},
		{"sendEmail invalid", map[string]any{"title": "t", "content": "c", "sendEmail": float64(2)}, "must be boolean"},
		{"sendEmail bad string", map[string]any{"title": "t", "content": "c", "sendEmail": "1"}, "must be boolean"},
		{"required wins over a bad title type", map[string]any{"content": "c"}, "title is required"},
		{"present-but-wrong title type", map[string]any{"title": map[string]any{}, "content": "c"}, "must be string"},
		{"valid", valid, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, message := inboxValidateNotifyBody(tc.body)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
		})
	}

	// Title is stringified from scalars, and a single-element array unwraps.
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"numeric title", map[string]any{"title": float64(123), "content": "c"}},
		{"boolean content", map[string]any{"title": "t", "content": true}},
		{"array title", map[string]any{"title": []any{"a"}, "content": "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, message := inboxValidateNotifyBody(tc.body); message != "" {
				t.Fatalf("unexpected message %q", message)
			}
		})
	}

	// instanceIds accepts a scalar (wrapped into an array) and coerces strings.
	body, message := inboxValidateNotifyBody(map[string]any{
		"title": "t", "content": "c", "instanceIds": "5",
	})
	if message != "" || !body.InstanceIDsSet || !reflect.DeepEqual(body.InstanceIDs, []float64{5}) {
		t.Fatalf("scalar instanceIds = (%#v,%q)", body, message)
	}
	body, message = inboxValidateNotifyBody(map[string]any{
		"title": "t", "content": "c", "instanceIds": []any{"1", "0x10"},
	})
	if message != "" || !reflect.DeepEqual(body.InstanceIDs, []float64{1, 16}) {
		t.Fatalf("string instanceIds = (%#v,%q)", body, message)
	}

	// sendEmail coerces "true"/1 and defaults to false when absent.
	body, message = inboxValidateNotifyBody(map[string]any{
		"title": "t", "content": "c", "sendEmail": "true",
	})
	if message != "" || !body.SendEmail {
		t.Fatalf(`sendEmail:"true" = (%#v,%q)`, body, message)
	}
	body, message = inboxValidateNotifyBody(map[string]any{"title": "t", "content": "c"})
	if message != "" || body.SendEmail {
		t.Fatalf("absent sendEmail must default to false, got %#v", body)
	}

	// The schema has no additionalProperties:false, so unknown keys are kept (and
	// ignored) rather than rejected.
	if _, message := inboxValidateNotifyBody(map[string]any{
		"title": "t", "content": "c", "bogus": float64(1),
	}); message != "" {
		t.Fatalf("unknown key must not be rejected: %q", message)
	}
}

func hundredAndOne() []any {
	values := make([]any, 101)
	for index := range values {
		values[index] = float64(index + 1)
	}
	return values
}

// TestInboxRedactLoginNotificationContent pins the demo scrub of the message body.
func TestInboxRedactLoginNotificationContent(t *testing.T) {
	raw := "您的账户在新设备上登录\n设备: Chrome on macOS\nIP: 203.0.113.9\n时间: 2026-01-01T00:00:00.000Z\n如非本人操作，请立即修改密码"
	want := "您的账户在新设备上登录\nIP: " + lib.DemoRedactedIP + "\n时间: 2026-01-01T00:00:00.000Z\n如非本人操作，请立即修改密码"
	if got := inboxRedactLoginNotificationContent(raw); got != want {
		t.Fatalf("redacted content = %q, want %q", got, want)
	}

	// The notifier joins lines with \n; \u0001/\u0002 separators become newlines
	// and runs of blank lines collapse.
	withControls := "line1\u0001设备: x\u0001IP: 1.2.3.4\u0001line2"
	got := inboxRedactLoginNotificationContent(withControls)
	if strings.Contains(got, "设备:") {
		t.Fatalf("device line must be dropped: %q", got)
	}
	if !strings.Contains(got, "IP: "+lib.DemoRedactedIP) {
		t.Fatalf("IP line must be masked: %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Fatalf("blank-line runs must collapse: %q", got)
	}

	// Leading/trailing whitespace is trimmed.
	if got := inboxRedactLoginNotificationContent("  hello  "); got != "hello" {
		t.Fatalf("trim = %q", got)
	}
}

// TestInboxRedactDemoMessage pins redactDemoInboxMessage: only login_new_device
// is rewritten, and data gets deviceInfo=null + ipAddress="已隐藏".
func TestInboxRedactDemoMessage(t *testing.T) {
	login := db.InboxMessage{
		EventType: inboxEventLoginNewDevice,
		Content:   "新设备登录\n设备: iPhone\nIP: 198.51.100.7",
		Data: map[string]any{
			"deviceInfo": "iPhone",
			"ipAddress":  "198.51.100.7",
			"loginTime":  "2026-01-01T00:00:00.000Z",
		},
	}
	redacted := inboxRedactDemoMessage(login)
	if strings.Contains(redacted.Content, "iPhone") {
		t.Fatalf("device must be scrubbed from content: %q", redacted.Content)
	}
	if !strings.Contains(redacted.Content, "IP: "+lib.DemoRedactedIP) {
		t.Fatalf("IP must be masked in content: %q", redacted.Content)
	}
	data, ok := redacted.Data.(map[string]any)
	if !ok {
		t.Fatalf("data type = %T", redacted.Data)
	}
	if data["deviceInfo"] != nil {
		t.Fatalf("deviceInfo = %v, want nil", data["deviceInfo"])
	}
	if data["ipAddress"] != lib.DemoRedactedIP {
		t.Fatalf("ipAddress = %v", data["ipAddress"])
	}
	if data["loginTime"] != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("other keys must survive, got %v", data["loginTime"])
	}

	// Any other event type is returned untouched (same value, no copy).
	other := db.InboxMessage{
		EventType: "instance_created",
		Content:   "IP: 198.51.100.7",
		Data:      map[string]any{"ipAddress": "198.51.100.7"},
	}
	if got := inboxRedactDemoMessage(other); !reflect.DeepEqual(got, other) {
		t.Fatalf("non-login message was modified: %#v", got)
	}

	// A null data column stays null (redactDemoLoginNotificationData's `if (!data)`).
	if got := inboxRedactDemoMessage(db.InboxMessage{EventType: inboxEventLoginNewDevice}).Data; got != nil {
		t.Fatalf("nil data became %#v", got)
	}
}

// TestInboxShouldRedactDemoLoginRecords pins the demo gate: it needs *both* a demo
// host and a demo account, and reads the host from Request.Host (not a header).
func TestInboxShouldRedactDemoLoginRecords(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newContext := func(host, forwardedHost string) *gin.Context {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest("GET", "/api/inbox", nil)
		request.Host = host
		if forwardedHost != "" {
			request.Header.Set("X-Forwarded-Host", forwardedHost)
		}
		context, _ := gin.CreateTestContext(recorder)
		context.Request = request
		return context
	}

	cases := []struct {
		name          string
		host          string
		forwardedHost string
		username      string
		want          bool
	}{
		{"demo host + demo user", "demo.payincus.com", "", "admin", true},
		{"demo admin host + demo user", "demoadmin.payincus.com", "", "demo", true},
		{"demo host + demo email-only user (no email claim)", "demo.payincus.com", "", "someone", false},
		{"demo host via X-Forwarded-Host", "127.0.0.1:3001", "demo.payincus.com", "admin", true},
		{"forwarded host with port", "127.0.0.1:3001", "demo.payincus.com:443", "demo", true},
		{"demo host + non-demo user", "demo.payincus.com", "", "alice", false},
		{"normal host + demo user", "pay.payincus.com", "", "admin", false},
		{"normal host + normal user", "pay.payincus.com", "", "alice", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			context := newContext(tc.host, tc.forwardedHost)
			claims := &lib.AccessTokenClaims{ID: 1, Username: tc.username}
			if got := inboxShouldRedactDemoLoginRecords(context, claims); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	if inboxShouldRedactDemoLoginRecords(newContext("demo.payincus.com", ""), nil) {
		t.Fatal("a nil user claim must never trigger redaction")
	}
}

// TestInboxRedactDemoLoginNotificationData pins the truthiness short-circuit of
// redactDemoLoginNotificationData.
func TestInboxRedactDemoLoginNotificationData(t *testing.T) {
	for _, unchanged := range []any{nil, false, float64(0), ""} {
		if got := inboxRedactLoginNotificationData(unchanged); !reflect.DeepEqual(got, unchanged) {
			t.Errorf("%#v must be returned unchanged, got %#v", unchanged, got)
		}
	}

	// An array spreads into an index-keyed object, like `{ ...[...] }`.
	got := inboxRedactLoginNotificationData([]any{"a", "b"})
	object, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("array data became %T", got)
	}
	if object["0"] != "a" || object["1"] != "b" {
		t.Fatalf("index keys missing: %#v", object)
	}
	if object["deviceInfo"] != nil || object["ipAddress"] != lib.DemoRedactedIP {
		t.Fatalf("redaction fields missing: %#v", object)
	}
}
