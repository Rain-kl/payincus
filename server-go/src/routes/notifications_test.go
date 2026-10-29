// Unit tests for the notifications route port. They pin the Fastify/AJV body
// semantics (verified against a real AJV instance with @fastify/ajv-compiler's
// defaults: allErrors:false, removeAdditional:true, coerceTypes:'array'), the
// config validators and the response helpers, without touching PostgreSQL.
package routes

import (
	"strings"
	"testing"
)

// TestValidateNotificationBodyPost pins the POST schema behaviour.
func TestValidateNotificationBodyPost(t *testing.T) {
	cases := []struct {
		name string
		body any
		want string
	}{
		{"empty object reports the first required field", map[string]any{}, "type is required"},
		{"missing name", map[string]any{"type": "telegram", "config": map[string]any{}}, "name is required"},
		{"missing config", map[string]any{"type": "telegram", "name": "n"}, "config is required"},
		{"required wins over a bad enum", map[string]any{"type": "bogus"}, "name is required"},
		{"unknown channel type", map[string]any{"type": "fax", "name": "n", "config": map[string]any{}}, "must be equal to one of the allowed values"},
		{"numeric type is stringified then rejected by enum", map[string]any{"type": float64(1), "name": "n", "config": map[string]any{}}, "must be equal to one of the allowed values"},
		{"single-element type array unwraps", map[string]any{"type": []any{"email"}, "name": "n", "config": map[string]any{}}, ""},
		{"two-element type array is not a string", map[string]any{"type": []any{"email", "webhook"}, "name": "n", "config": map[string]any{}}, "must be string"},
		{"null type coerces to empty then fails the enum", map[string]any{"type": nil, "name": "n", "config": map[string]any{}}, "must be equal to one of the allowed values"},
		{"empty name", map[string]any{"type": "email", "name": "", "config": map[string]any{}}, "name is too short"},
		{"null name coerces to empty then fails minLength", map[string]any{"type": "email", "name": nil, "config": map[string]any{}}, "name is too short"},
		{"name over 50 chars", map[string]any{"type": "email", "name": strings.Repeat("x", 51), "config": map[string]any{}}, "name is too long"},
		{"numeric name is stringified", map[string]any{"type": "email", "name": float64(123), "config": map[string]any{}}, ""},
		{"object name is not a string", map[string]any{"type": "email", "name": map[string]any{}, "config": map[string]any{}}, "must be string"},
		{"config array is not an object", map[string]any{"type": "email", "name": "n", "config": []any{}}, "must be object"},
		{"config null is not an object", map[string]any{"type": "email", "name": "n", "config": nil}, "must be object"},
		{"string config is not an object", map[string]any{"type": "email", "name": "n", "config": "x"}, "must be object"},
		{"non-boolean enabled", map[string]any{"type": "email", "name": "n", "config": map[string]any{}, "enabled": "yes"}, "must be boolean"},
		{"numeric enabled 0 is false", map[string]any{"type": "email", "name": "n", "config": map[string]any{}, "enabled": float64(0)}, ""},
		{"numeric enabled 2 is invalid", map[string]any{"type": "email", "name": "n", "config": map[string]any{}, "enabled": float64(2)}, "must be boolean"},
		{"unknown keys are dropped, not rejected", map[string]any{"type": "email", "name": "n", "config": map[string]any{}, "extra": float64(1)}, ""},
		{"non-object body", []any{float64(1)}, "must be object"},
		{"null body", nil, "must be object"},
		{"string body", "x", "must be object"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, message := validateNotificationBody(tc.body, true)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
		})
	}
}

// TestValidateNotificationBodyPostValues pins the coerced values that end up in the
// database for accepted bodies.
func TestValidateNotificationBodyPostValues(t *testing.T) {
	validated, message := validateNotificationBody(map[string]any{
		"type":   []any{"telegram"},
		"name":   float64(42),
		"config": map[string]any{"botToken": "1:a", "chatId": "-1"},
		"extra":  "dropped",
	}, true)
	if message != "" {
		t.Fatalf("unexpected validation error %q", message)
	}

	body := validated.(notificationCreateBody)
	if body.Type != "telegram" {
		t.Fatalf("type = %q", body.Type)
	}
	if body.Name != "42" {
		t.Fatalf("name = %q", body.Name)
	}
	if !body.Enabled {
		t.Fatal("absent enabled must default to true (handler-level `?? true`)")
	}
	if _, present := body.Config["extra"]; present {
		t.Fatal("config contents are not filtered by the schema")
	}

	validated, message = validateNotificationBody(map[string]any{
		"type": "email", "name": "n", "config": map[string]any{}, "enabled": "false",
	}, true)
	if message != "" {
		t.Fatalf("unexpected validation error %q", message)
	}
	if validated.(notificationCreateBody).Enabled {
		t.Fatal(`enabled:"false" must coerce to false`)
	}
}

// TestValidateNotificationBodyPatch pins the PATCH schema: no `required` list and
// no `type` property (an unknown key, so removeAdditional drops it).
func TestValidateNotificationBodyPatch(t *testing.T) {
	validated, message := validateNotificationBody(map[string]any{}, false)
	if message != "" {
		t.Fatalf("empty PATCH body must be valid, got %q", message)
	}
	body := validated.(notificationUpdateBody)
	if body.Name != nil || body.ConfigProvided || body.Enabled != nil {
		t.Fatalf("empty PATCH body produced %#v", body)
	}

	// `type` is not part of the PATCH schema, so even a bogus value is dropped.
	if _, message := validateNotificationBody(map[string]any{"type": "bogus"}, false); message != "" {
		t.Fatalf("PATCH must ignore the POST-only type key, got %q", message)
	}

	if _, message := validateNotificationBody(map[string]any{"name": ""}, false); message != "name is too short" {
		t.Fatalf("empty PATCH name message = %q", message)
	}
	if _, message := validateNotificationBody(map[string]any{"name": strings.Repeat("x", 51)}, false); message != "name is too long" {
		t.Fatalf("long PATCH name message = %q", message)
	}
	if _, message := validateNotificationBody(map[string]any{"config": []any{}}, false); message != "must be object" {
		t.Fatalf("PATCH config array message = %q", message)
	}
	if _, message := validateNotificationBody(map[string]any{"enabled": "x"}, false); message != "must be boolean" {
		t.Fatalf("PATCH enabled message = %q", message)
	}
	if _, message := validateNotificationBody(nil, false); message != "must be object" {
		t.Fatalf("PATCH null body message = %q", message)
	}

	validated, message = validateNotificationBody(map[string]any{
		"name": "ok", "config": map[string]any{}, "enabled": []any{false},
	}, false)
	if message != "" {
		t.Fatalf("unexpected PATCH validation error %q", message)
	}
	body = validated.(notificationUpdateBody)
	if body.Name == nil || *body.Name != "ok" || !body.ConfigProvided || body.Enabled == nil || *body.Enabled {
		t.Fatalf("PATCH body = %#v", body)
	}
}

// TestCoerceNotificationString pins the AJV string coercion table.
func TestCoerceNotificationString(t *testing.T) {
	cases := []struct {
		value any
		want  string
		ok    bool
	}{
		{"x", "x", true},
		{nil, "", true},
		{true, "true", true},
		{false, "false", true},
		{float64(123), "123", true},
		{float64(1.5), "1.5", true},
		{float64(0), "0", true},
		{[]any{"x"}, "x", true},
		{[]any{float64(7)}, "7", true},
		{[]any{[]any{"x"}}, "", false},
		{[]any{"a", "b"}, "", false},
		{[]any{}, "", false},
		{map[string]any{}, "", false},
	}
	for _, tc := range cases {
		got, ok := coerceNotificationString(tc.value)
		if got != tc.want || ok != tc.ok {
			t.Errorf("coerceNotificationString(%#v) = (%q,%v), want (%q,%v)", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

// TestCoerceNotificationBoolean pins the AJV boolean coercion table.
func TestCoerceNotificationBoolean(t *testing.T) {
	cases := []struct {
		value any
		want  bool
		ok    bool
	}{
		{true, true, true},
		{false, false, true},
		{nil, false, true},
		{float64(0), false, true},
		{float64(1), true, true},
		{float64(2), false, false},
		{"true", true, true},
		{"false", false, true},
		{"1", false, false},
		{"", false, false},
		{[]any{true}, true, true},
		{[]any{float64(0)}, false, true},
		{[]any{true, false}, false, false},
		{[]any{}, false, false},
		{map[string]any{}, false, false},
	}
	for _, tc := range cases {
		got, ok := coerceNotificationBoolean(tc.value)
		if got != tc.want || ok != tc.ok {
			t.Errorf("coerceNotificationBoolean(%#v) = (%v,%v), want (%v,%v)", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

// TestValidateNotificationConfig pins validateConfig for each channel type,
// including the in-place normalization the original performs.
func TestValidateNotificationConfig(t *testing.T) {
	telegram := map[string]any{
		"botToken": "  123456789:abcdefghijklmnopqrstuvwxyz  ",
		"chatId":   " -1001234567890 ",
	}
	if result := validateNotificationConfig("telegram", telegram); !result.Valid {
		t.Fatalf("valid telegram config rejected: %#v", result)
	}
	if telegram["botToken"] != "123456789:abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("botToken was not trimmed: %v", telegram["botToken"])
	}
	if telegram["chatId"] != "-1001234567890" {
		t.Fatalf("chatId was not trimmed: %v", telegram["chatId"])
	}

	cases := []struct {
		name     string
		type_    string
		config   map[string]any
		want     string
		internal bool
	}{
		{"telegram missing fields", "telegram", map[string]any{}, "Please provide Bot Token and Chat ID", false},
		{"telegram non-string token", "telegram", map[string]any{"botToken": float64(1), "chatId": "-1"}, "Please provide Bot Token and Chat ID", false},
		{"telegram bad token", "telegram", map[string]any{"botToken": "abc", "chatId": "-1"}, "Invalid Telegram Bot Token", false},
		{"telegram zero-leading token", "telegram", map[string]any{"botToken": "0123456:abcdefghijklmnopqrstuvwxyz", "chatId": "-1"}, "Invalid Telegram Bot Token", false},
		{"telegram bad chat id", "telegram", map[string]any{"botToken": "123456789:abcdefghijklmnopqrstuvwxyz", "chatId": "nope"}, "Invalid Telegram Chat ID", false},
		{"telegram @username chat id", "telegram", map[string]any{"botToken": "123456789:abcdefghijklmnopqrstuvwxyz", "chatId": "@mychannel"}, "", false},
		{"discord missing url", "discord", map[string]any{}, "Please provide Discord Webhook URL", false},
		{"discord wrong host", "discord", map[string]any{"webhookUrl": "https://example.com/api/webhooks/1"}, "Invalid Discord Webhook URL", false},
		{"discord http scheme", "discord", map[string]any{"webhookUrl": "http://discord.com/api/webhooks/1"}, "Invalid Discord Webhook URL", false},
		{"discord non-string url", "discord", map[string]any{"webhookUrl": float64(5)}, "Please provide Discord Webhook URL", false},
		{"webhook missing url", "webhook", map[string]any{}, "Please provide Webhook URL", false},
		{"webhook without scheme", "webhook", map[string]any{"url": "example.com/hook"}, "Invalid Webhook URL", false},
		{"webhook oversized secret", "webhook", map[string]any{"url": "https://example.com/h", "secret": strings.Repeat("s", 257)}, "Invalid Webhook secret", false},
		{"webhook non-string secret", "webhook", map[string]any{"url": "https://example.com/h", "secret": float64(1)}, "Invalid Webhook secret", false},
		{"webhook null secret", "webhook", map[string]any{"url": "https://example.com/h", "secret": nil}, "Invalid Webhook secret", false},
		{"email missing address", "email", map[string]any{}, "Please provide email address", false},
		{"email empty address", "email", map[string]any{"email": ""}, "Please provide email address", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := validateNotificationConfig(tc.type_, tc.config)
			if result.Internal != tc.internal {
				t.Fatalf("internal = %v, want %v (%#v)", result.Internal, tc.internal, result)
			}
			if result.Error != tc.want {
				t.Fatalf("error = %q, want %q", result.Error, tc.want)
			}
			if tc.want == "" && !tc.internal && !result.Valid {
				t.Fatal("expected a valid result")
			}
		})
	}

	// The webhook URL is trimmed in place.
	webhook := map[string]any{"url": "  https://example.com/hook  ", "secret": "  s  "}
	if result := validateNotificationConfig("webhook", webhook); !result.Valid {
		t.Fatalf("valid webhook config rejected: %#v", result)
	}
	if webhook["url"] != "https://example.com/hook" || webhook["secret"] != "s" {
		t.Fatalf("webhook config not normalized: %#v", webhook)
	}
}

// TestValidateNotificationOutboundConfig pins that only webhook channels get the
// SSRF pre-check and that private targets are rejected with the guard message.
func TestValidateNotificationOutboundConfig(t *testing.T) {
	// Non-webhook types and non-string urls skip the check entirely.
	if result := validateNotificationOutboundConfig(t.Context(), "telegram", map[string]any{"botToken": "x"}); !result.Valid {
		t.Fatalf("telegram must skip the outbound check: %#v", result)
	}
	if result := validateNotificationOutboundConfig(t.Context(), "webhook", map[string]any{"url": float64(1)}); !result.Valid {
		t.Fatalf("non-string url must skip the outbound check: %#v", result)
	}

	privateTargets := []string{
		"http://127.0.0.1:9/hook",
		"https://10.0.0.1/hook",
		"http://169.254.169.254/latest/meta-data",
		"https://localhost/hook",
	}
	for _, target := range privateTargets {
		result := validateNotificationOutboundConfig(t.Context(), "webhook", map[string]any{"url": target})
		if result.Valid {
			t.Fatalf("%q must be rejected", target)
		}
		if result.Internal || result.Error == "" {
			t.Fatalf("%q rejection = %#v", target, result)
		}
	}
}

// TestNotificationConfigPreview pins getConfigPreview for each channel type.
func TestNotificationConfigPreview(t *testing.T) {
	cases := []struct {
		name        string
		channelType string
		config      string
		want        string
	}{
		{"telegram with chat id", "telegram", `{"chatId":"-100123"}`, "Chat ID: -100123"},
		{"telegram without chat id", "telegram", `{"botToken":"x"}`, "Chat ID: Not set"},
		{"discord keeps the last 20 chars", "discord", `{"webhookUrl":"https://discord.com/api/webhooks/1234/abcdefghij"}`, "Webhook: ...ooks/1234/abcdefghij"},
		{"discord without url", "discord", `{}`, "Not set"},
		{"webhook shows the hostname", "webhook", `{"url":"https://hooks.example.com:8443/path?x=1"}`, "URL: hooks.example.com"},
		{"webhook invalid url", "webhook", `{"url":"not a url"}`, "URL: Invalid"},
		{"webhook without url", "webhook", `{"secret":"s"}`, "Not set"},
		{"email shows the address", "email", `{"email":"a@b.c"}`, "a@b.c"},
		{"email without address", "email", `{}`, "Not set"},
		{"unknown type", "fax", `{}`, "Unknown type"},
		{"malformed json", "telegram", `{`, "Config error"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := notificationConfigPreview(tc.channelType, tc.config); got != tc.want {
				t.Fatalf("preview = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMaskNotificationSensitiveConfig pins the credential masking applied by the
// channel detail route.
func TestMaskNotificationSensitiveConfig(t *testing.T) {
	telegram := maskNotificationSensitiveConfig("telegram", map[string]any{
		"botToken": "123456789:abcdefghijklmnopqrstuvwxyz", "chatId": "-1",
	})
	if got := telegram["botToken"]; got != "123456789:...vwxyz" {
		t.Fatalf("masked botToken = %v", got)
	}
	if telegram["chatId"] != "-1" {
		t.Fatalf("chatId must stay visible, got %v", telegram["chatId"])
	}

	discord := maskNotificationSensitiveConfig("discord", map[string]any{
		"webhookUrl": "https://discord.com/api/webhooks/1234567890/abcdefghijklmnopqrstuvwxyz",
	})
	if got := discord["webhookUrl"]; got != "https://discord.com/api/webhooks/1234567890/abcdef..." {
		t.Fatalf("masked webhookUrl = %v", got)
	}

	webhook := maskNotificationSensitiveConfig("webhook", map[string]any{"url": "https://e.com/h", "secret": "s"})
	if webhook["secret"] != "***已设置***" {
		t.Fatalf("masked secret = %v", webhook["secret"])
	}
	if webhook["url"] != "https://e.com/h" {
		t.Fatalf("url must stay visible, got %v", webhook["url"])
	}

	// The input map is copied, not mutated.
	original := map[string]any{"secret": "s"}
	_ = maskNotificationSensitiveConfig("webhook", original)
	if original["secret"] != "s" {
		t.Fatal("maskSensitiveConfig must not mutate the caller's map")
	}
}

// TestParseNotificationQueryInteger pins `parsePositiveInteger` (parseInt with a
// fallback) and the trailing-junk behaviour parseInt has.
func TestParseNotificationQueryInteger(t *testing.T) {
	cases := []struct {
		value    string
		fallback int
		want     int
	}{
		{"", 1, 1},
		{"abc", 1, 1},
		{"0", 1, 1},
		{"-3", 7, 7},
		{"3", 1, 3},
		{"  42", 1, 42},
		{"12abc", 1, 12},
		{"1.9", 1, 1},
	}
	for _, tc := range cases {
		if got := parseNotificationQueryInteger(tc.value, tc.fallback); got != tc.want {
			t.Errorf("parseNotificationQueryInteger(%q, %d) = %d, want %d", tc.value, tc.fallback, got, tc.want)
		}
	}
}

// TestJSStringSlice pins the slice() helper used by the preview/mask helpers.
func TestJSStringSlice(t *testing.T) {
	cases := []struct {
		value      string
		start, end int
		want       string
	}{
		{"https://discord.com/api/webhooks/1/x", -20, 36, "com/api/webhooks/1/x"},
		{"abc", 0, 10, "abc"},
		{"abc", 0, 2, "ab"},
		{"abc", -2, 3, "bc"},
		{"abc", 5, 10, ""},
		{"abc", 2, 1, ""},
		{"123456789:abcdefghijklmnopqrstuvwxyz", 0, 10, "123456789:"},
	}
	for _, tc := range cases {
		if got := jsStringSlice(tc.value, tc.start, tc.end); got != tc.want {
			t.Errorf("jsStringSlice(%q, %d, %d) = %q, want %q", tc.value, tc.start, tc.end, got, tc.want)
		}
	}
}

// TestJSURLHostname pins the hostname extraction (a port is dropped, a missing
// scheme is invalid).
func TestJSURLHostname(t *testing.T) {
	cases := []struct {
		value string
		want  string
		ok    bool
	}{
		{"https://hooks.example.com:8443/path", "hooks.example.com", true},
		{"http://EXAMPLE.com", "example.com", true},
		{"https://[2001:db8::1]:8443/x", "2001:db8::1", true},
		{"not a url", "", false},
		{"//example.com/x", "", false},
	}
	for _, tc := range cases {
		got, ok := jsURLHostname(tc.value)
		if got != tc.want || ok != tc.ok {
			t.Errorf("jsURLHostname(%q) = (%q,%v), want (%q,%v)", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

// TestNormalizeNotificationChannelName pins the trim + 1..50 rule, including the
// UTF-16 length semantics (a non-BMP rune counts as 2).
func TestNormalizeNotificationChannelName(t *testing.T) {
	name, ok := normalizeNotificationChannelName("  hello  ")
	if !ok || name != "hello" {
		t.Fatalf("trim result = (%q,%v)", name, ok)
	}
	if name, ok := normalizeNotificationChannelName("   "); ok || name != "" {
		t.Fatalf("blank name = (%q,%v)", name, ok)
	}
	if _, ok := normalizeNotificationChannelName(strings.Repeat("x", 51)); ok {
		t.Fatal("51 chars must be rejected")
	}
	if _, ok := normalizeNotificationChannelName(strings.Repeat("😀", 26)); ok {
		t.Fatal("26 emoji are 52 UTF-16 code units and must be rejected")
	}
	if _, ok := normalizeNotificationChannelName(strings.Repeat("😀", 25)); !ok {
		t.Fatal("25 emoji are 50 UTF-16 code units and must be accepted")
	}
}

// TestUTF16Length pins the surrogate-pair accounting.
func TestUTF16Length(t *testing.T) {
	cases := []struct {
		value string
		want  int
	}{
		{"", 0},
		{"abc", 3},
		{"中文", 2},
		{"😀", 2},
		{"a😀b", 4},
	}
	for _, tc := range cases {
		if got := utf16Length(tc.value); got != tc.want {
			t.Errorf("utf16Length(%q) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

// TestNotificationTruthy pins the JS truthiness table for decoded JSON values.
func TestNotificationTruthy(t *testing.T) {
	falsy := []any{nil, false, "", float64(0)}
	for _, value := range falsy {
		if notificationTruthy(value) {
			t.Errorf("%#v must be falsy", value)
		}
	}
	truthy := []any{true, "x", float64(1), float64(-1), []any{}, map[string]any{}}
	for _, value := range truthy {
		if !notificationTruthy(value) {
			t.Errorf("%#v must be truthy", value)
		}
	}
}

// TestParseIntPrefix pins parseInt(value, 10) edge cases.
func TestParseIntPrefix(t *testing.T) {
	cases := []struct {
		value string
		want  int
		ok    bool
	}{
		{"", 0, false},
		{"  12", 12, true},
		{"+12", 12, true},
		{"-12", -12, true},
		{"12px", 12, true},
		{"0x10", 0, true},
		{"abc", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseIntPrefix(tc.value)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseIntPrefix(%q) = (%d,%v), want (%d,%v)", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

// TestNotificationRouteErrorCodes pins the local (non-lib) error payload shapes:
// the routes bypass apiError() for these and send { error } / { error, code }.
func TestNotificationRouteErrorCodes(t *testing.T) {
	if notificationTestFailedCode != "TEST_FAILED" {
		t.Fatalf("test failure code = %q", notificationTestFailedCode)
	}
	if userNotificationBotTokenPattern.MatchString("0:short") {
		t.Fatal("bot tokens must start with 1-9")
	}
	if !userNotificationChatIDPattern.MatchString("-1001234567890") || !userNotificationChatIDPattern.MatchString("@channel") {
		t.Fatal("chat id pattern must accept numeric ids and @usernames")
	}
	if userNotificationChatIDPattern.MatchString("@chan") {
		t.Fatal("@usernames need at least 5 characters")
	}
}
