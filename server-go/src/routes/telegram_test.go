// Unit tests for the Telegram route port. They pin the pure logic that the
// conformance harness cannot observe without a real bot: config normalization
// (including the tri-state/default rules), command parsing, eligibility maths,
// the secret comparison, the webhook URL builder and the webhook body parser.
//
// Nothing here talks to api.telegram.org: telegramHTTPClient is never invoked.
package routes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// ==================== secret helpers ====================

// TestTelegramHashToken pins hashToken against a value computed independently:
// sha256 of the raw token, lowercase hex, 64 characters.
func TestTelegramHashToken(t *testing.T) {
	sum := sha256.Sum256([]byte("bind-token-value"))
	want := hex.EncodeToString(sum[:])
	if got := telegramHashToken("bind-token-value"); got != want {
		t.Fatalf("telegramHashToken() = %q, want %q", got, want)
	}
	if len(telegramHashToken("")) != 64 {
		t.Fatal("hash of the empty string must still be 64 hex characters")
	}
}

// TestTelegramSecureEquals pins secureEquals: identical strings match, and any
// length or content difference does not.
func TestTelegramSecureEquals(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"equal", "secret-token", "secret-token", true},
		{"both empty", "", "", true},
		{"prefix", "secret", "secret-token", false},
		{"different content", "secret-a", "secret-b", false},
		{"case", "Secret", "secret", false},
		{"unicode", "密钥", "密钥", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := telegramSecureEquals(tc.a, tc.b); got != tc.want {
				t.Fatalf("telegramSecureEquals(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestTelegramWebhookSecretPattern pins /^[A-Za-z0-9_-]{1,256}$/.
func TestTelegramWebhookSecretPattern(t *testing.T) {
	valid := []string{"a", "A1_-", strings.Repeat("a", 256)}
	for _, value := range valid {
		if !telegramIsValidWebhookSecret(value) {
			t.Fatalf("expected %q to be valid", value)
		}
	}
	invalid := []string{"", "a b", "a.b", strings.Repeat("a", 257), "密钥", "a\nb", "@"}
	for _, value := range invalid {
		if telegramIsValidWebhookSecret(value) {
			t.Fatalf("expected %q to be invalid", value)
		}
	}
}

// ==================== config normalization ====================

func strPtr(value string) *string { return &value }

// TestTelegramNormalizeBotUsername pins `value?.trim().replace(/^@/, ”) || null`:
// exactly one leading '@' is removed and an empty result is null.
func TestTelegramNormalizeBotUsername(t *testing.T) {
	cases := []struct {
		name  string
		input *string
		want  *string
	}{
		{"nil", nil, nil},
		{"plain", strPtr("incudal_bot"), strPtr("incudal_bot")},
		{"with at", strPtr("@incudal_bot"), strPtr("incudal_bot")},
		{"padded", strPtr("  @incudal_bot  "), strPtr("incudal_bot")},
		{"double at keeps one", strPtr("@@bot"), strPtr("@bot")},
		{"empty", strPtr(""), nil},
		{"whitespace", strPtr("   "), nil},
		{"only at", strPtr("@"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := telegramNormalizeBotUsername(tc.input)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %q, want nil", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("got nil, want %q", *tc.want)
			case tc.want != nil && got != nil && *got != *tc.want:
				t.Fatalf("got %q, want %q", *got, *tc.want)
			}
		})
	}
}

// TestTelegramParseNonNegativeAmount pins parseNonNegativeAmount:
// parseFloat, reject non-finite/negative, then Math.round(x*100)/100.
func TestTelegramParseNonNegativeAmount(t *testing.T) {
	cases := []struct {
		name  string
		input *string
		want  float64
	}{
		{"nil", nil, 0},
		{"empty", strPtr(""), 0},
		{"integer", strPtr("10"), 10},
		{"decimal rounds up", strPtr("10.005"), 10.01},
		{"decimal rounds down", strPtr("10.004"), 10},
		{"round half up", strPtr("2.345"), 2.35},
		{"already two decimals", strPtr("1.25"), 1.25},
		{"negative falls back", strPtr("-1"), 0},
		{"negative decimal falls back", strPtr("-0.01"), 0},
		{"garbage falls back", strPtr("abc"), 0},
		{"numeric prefix", strPtr("12abc"), 12},
		{"infinity falls back", strPtr("Infinity"), 0},
		{"leading dot", strPtr(".5"), 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := telegramParseNonNegativeAmount(tc.input); got != tc.want {
				t.Fatalf("telegramParseNonNegativeAmount(%v) = %v, want %v", derefString(tc.input), got, tc.want)
			}
		})
	}
}

func derefString(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}

// TestTelegramParseInviteExpireMinutes pins parseInviteExpireMinutes: a parseInt
// fallback of 30 and a 1..10080 clamp.
func TestTelegramParseInviteExpireMinutes(t *testing.T) {
	cases := []struct {
		name  string
		input *string
		want  int
	}{
		{"nil", nil, 30},
		{"empty", strPtr(""), 30},
		{"valid", strPtr("45"), 45},
		{"zero clamps to 1", strPtr("0"), 1},
		{"negative clamps to 1", strPtr("-5"), 1},
		{"max", strPtr("10080"), 10080},
		{"over max clamps", strPtr("10081"), 10080},
		{"garbage", strPtr("abc"), 30},
		{"numeric prefix", strPtr("60abc"), 60},
		{"float truncates", strPtr("12.9"), 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := telegramParseInviteExpireMinutes(tc.input); got != tc.want {
				t.Fatalf("telegramParseInviteExpireMinutes(%v) = %d, want %d", derefString(tc.input), got, tc.want)
			}
		})
	}
}

// TestTelegramBuildGroupConfig pins the configured flag (enabled AND has chat id)
// and the chat-id trimming.
func TestTelegramBuildGroupConfig(t *testing.T) {
	t.Run("enabled with chat id", func(t *testing.T) {
		group := telegramBuildGroupConfig(true, strPtr(" -100123 "), strPtr("all"), strPtr("5"), strPtr("6"), strPtr("15"))
		if !group.Enabled || !group.Configured {
			t.Fatalf("expected enabled+configured, got %+v", group)
		}
		if group.ChatID == nil || *group.ChatID != "-100123" {
			t.Fatalf("chatId = %v, want trimmed -100123", group.ChatID)
		}
		if group.JoinMode != "all" {
			t.Fatalf("joinMode = %q, want all", group.JoinMode)
		}
		if group.MinRecharge != 5 || group.MinConsume != 6 || group.InviteExpireMinutes != 15 {
			t.Fatalf("unexpected thresholds: %+v", group)
		}
	})

	t.Run("enabled without chat id is not configured", func(t *testing.T) {
		group := telegramBuildGroupConfig(true, strPtr("   "), nil, nil, nil, nil)
		if group.ChatID != nil {
			t.Fatalf("chatId = %v, want nil", group.ChatID)
		}
		if group.Configured {
			t.Fatal("a group without a chat id must not be configured")
		}
		if group.JoinMode != "any" {
			t.Fatalf("joinMode = %q, want any", group.JoinMode)
		}
		if group.InviteExpireMinutes != 30 {
			t.Fatalf("inviteExpireMinutes = %d, want 30", group.InviteExpireMinutes)
		}
	})

	t.Run("disabled with chat id is not configured", func(t *testing.T) {
		group := telegramBuildGroupConfig(false, strPtr("-100"), nil, nil, nil, nil)
		if group.Configured {
			t.Fatal("a disabled group must not be configured")
		}
	})
}

// TestTelegramNormalizeGroupJoinMode pins the all/any split.
func TestTelegramNormalizeGroupJoinMode(t *testing.T) {
	if got := telegramNormalizeGroupJoinMode(strPtr("all")); got != "all" {
		t.Fatalf("got %q, want all", got)
	}
	for _, value := range []*string{nil, strPtr(""), strPtr("ALL"), strPtr("any"), strPtr("bogus")} {
		if got := telegramNormalizeGroupJoinMode(value); got != "any" {
			t.Fatalf("telegramNormalizeGroupJoinMode(%v) = %q, want any", derefString(value), got)
		}
	}
}

// ==================== command parsing ====================

// TestTelegramNormalizeCommand pins
// `text.trim().split(/\s+/)[0].replace(/@\w+$/, ”).toLowerCase()`.
func TestTelegramNormalizeCommand(t *testing.T) {
	cases := map[string]string{
		"/join":                "/join",
		"  /join  ":            "/join",
		"/join extra args":     "/join",
		"/join@incudal_bot":    "/join",
		"/JOIN":                "/join",
		"/Join@IncudalBot now": "/join",
		"":                     "",
		"   ":                  "",
		"hello":                "hello",
		"/join@":               "/join@", // \w+ requires at least one character
	}
	for input, want := range cases {
		if got := telegramNormalizeTelegramCommand(input); got != want {
			t.Fatalf("telegramNormalizeTelegramCommand(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestTelegramGetGroupJoinTarget pins getGroupJoinTarget: the anchored /start
// forms first, then the bare command aliases.
func TestTelegramGetGroupJoinTarget(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"/start join", "normal"},
		{"/start group", "normal"},
		{"/start apply_group", "normal"},
		{"/start join_vip", "vip"},
		{"/join", "normal"},
		{"/group", "normal"},
		{"/apply_group", "normal"},
		{"/join_vip", "vip"},
		{"/join_vip@bot", "vip"},
		{"  /JOIN  ", "normal"},
		{"/join extra", "normal"},
		{"/start", ""},
		{"/start join extra", ""},
		{"/start  join", "normal"},
		{"hello", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := telegramGetGroupJoinTarget(tc.input); got != tc.want {
			t.Fatalf("telegramGetGroupJoinTarget(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ==================== eligibility ====================

// TestTelegramEvaluateGroupEligibility pins the zero-threshold short circuit, the
// any/all split and the rendered message.
func TestTelegramEvaluateGroupEligibility(t *testing.T) {
	t.Run("no thresholds", func(t *testing.T) {
		eligible, message := telegramEvaluateGroupEligibility(
			db.UserConsumeStats{},
			telegramGroupConfig{JoinMode: "any"},
		)
		if !eligible {
			t.Fatal("expected eligible when no threshold is set")
		}
		if message != "当前未设置金额门槛。" {
			t.Fatalf("message = %q", message)
		}
	})

	t.Run("any mode passes on recharge", func(t *testing.T) {
		eligible, message := telegramEvaluateGroupEligibility(
			db.UserConsumeStats{TotalRecharge: 100, TotalConsume: 0},
			telegramGroupConfig{JoinMode: "any", MinRecharge: 50, MinConsume: 50},
		)
		if !eligible {
			t.Fatal("expected eligible: recharge alone satisfies 'any'")
		}
		want := "满足任一即可：累计充值 ¥50.00（当前 ¥100.00）；累计消费 ¥50.00（当前 ¥0.00）"
		if message != want {
			t.Fatalf("message = %q, want %q", message, want)
		}
	})

	t.Run("any mode fails when neither passes", func(t *testing.T) {
		eligible, _ := telegramEvaluateGroupEligibility(
			db.UserConsumeStats{TotalRecharge: 1, TotalConsume: 1},
			telegramGroupConfig{JoinMode: "any", MinRecharge: 50, MinConsume: 50},
		)
		if eligible {
			t.Fatal("expected ineligible")
		}
	})

	t.Run("all mode requires both", func(t *testing.T) {
		eligible, message := telegramEvaluateGroupEligibility(
			db.UserConsumeStats{TotalRecharge: 100, TotalConsume: 0},
			telegramGroupConfig{JoinMode: "all", MinRecharge: 50, MinConsume: 50},
		)
		if eligible {
			t.Fatal("expected ineligible: consume threshold is unmet")
		}
		if !strings.HasPrefix(message, "需同时满足：") {
			t.Fatalf("message = %q, want the 需同时满足 prefix", message)
		}
	})

	t.Run("all mode passes at the boundary", func(t *testing.T) {
		eligible, _ := telegramEvaluateGroupEligibility(
			db.UserConsumeStats{TotalRecharge: 50, TotalConsume: 50},
			telegramGroupConfig{JoinMode: "all", MinRecharge: 50, MinConsume: 50},
		)
		if !eligible {
			t.Fatal("expected eligible at the exact threshold")
		}
	})

	t.Run("single threshold", func(t *testing.T) {
		eligible, message := telegramEvaluateGroupEligibility(
			db.UserConsumeStats{TotalConsume: 500},
			telegramGroupConfig{JoinMode: "any", MinConsume: 10},
		)
		if !eligible {
			t.Fatal("expected eligible")
		}
		if strings.Contains(message, "充值") {
			t.Fatalf("message = %q, must not mention recharge", message)
		}
	})
}

// TestTelegramBuildAdminGroupEligibility pins the disabled/unconfigured statuses.
func TestTelegramBuildAdminGroupEligibility(t *testing.T) {
	disabled := telegramBuildAdminGroupEligibility(
		db.UserConsumeStats{},
		telegramGroupConfig{JoinMode: "any"},
		"普通群",
	)
	if disabled.Status != "disabled" || disabled.Eligible {
		t.Fatalf("status = %q eligible = %v, want disabled/false", disabled.Status, disabled.Eligible)
	}
	if disabled.Message != "普通群入群申请未启用。" {
		t.Fatalf("message = %q", disabled.Message)
	}

	unconfigured := telegramBuildAdminGroupEligibility(
		db.UserConsumeStats{},
		telegramGroupConfig{Enabled: true, JoinMode: "all", MinRecharge: 1, MinConsume: 2},
		"高级群",
	)
	if unconfigured.Status != "unconfigured" {
		t.Fatalf("status = %q, want unconfigured", unconfigured.Status)
	}
	if unconfigured.Message != "高级群 Chat ID 未配置。" {
		t.Fatalf("message = %q", unconfigured.Message)
	}
	if unconfigured.JoinMode != "all" || unconfigured.MinRecharge != 1 || unconfigured.MinConsume != 2 {
		t.Fatalf("thresholds not echoed: %+v", unconfigured)
	}

	eligible := telegramBuildAdminGroupEligibility(
		db.UserConsumeStats{TotalRecharge: 10},
		telegramGroupConfig{Enabled: true, Configured: true, JoinMode: "any", MinRecharge: 5},
		"普通群",
	)
	if eligible.Status != "eligible" || !eligible.Eligible {
		t.Fatalf("status = %q eligible = %v, want eligible/true", eligible.Status, eligible.Eligible)
	}

	ineligible := telegramBuildAdminGroupEligibility(
		db.UserConsumeStats{},
		telegramGroupConfig{Enabled: true, Configured: true, JoinMode: "any", MinRecharge: 5},
		"普通群",
	)
	if ineligible.Status != "ineligible" || ineligible.Eligible {
		t.Fatalf("status = %q eligible = %v, want ineligible/false", ineligible.Status, ineligible.Eligible)
	}
}

// TestTelegramFormatMoney pins toFixed(2).
func TestTelegramFormatMoney(t *testing.T) {
	cases := map[float64]string{
		0:     "0.00",
		1:     "1.00",
		1.5:   "1.50",
		1.005: "1.00", // binary float keeps 1.005 just below the tie
		1.25:  "1.25",
		100:   "100.00",
	}
	for input, want := range cases {
		if got := telegramFormatMoney(input); got != want {
			t.Fatalf("telegramFormatMoney(%v) = %q, want %q", input, got, want)
		}
	}
}

// ==================== webhook URL ====================

// TestTelegramNormalizeWebhookBaseURL pins protocol/host normalization and the
// production HTTPS requirement.
func TestTelegramNormalizeWebhookBaseURL(t *testing.T) {
	t.Setenv("NODE_ENV", "development")

	cases := []struct {
		name  string
		input string
		want  *string
	}{
		{"https", "https://pay.example.com", strPtr("https://pay.example.com")},
		{"http allowed in dev", "http://pay.example.com", strPtr("http://pay.example.com")},
		{"path dropped", "https://pay.example.com/some/path?q=1", strPtr("https://pay.example.com")},
		{"port kept", "https://pay.example.com:8443", strPtr("https://pay.example.com:8443")},
		{"default port dropped", "https://pay.example.com:443", strPtr("https://pay.example.com")},
		{"uppercase host lowered", "HTTPS://Pay.Example.COM", strPtr("https://pay.example.com")},
		{"padded", "  https://pay.example.com  ", strPtr("https://pay.example.com")},
		{"ftp rejected", "ftp://pay.example.com", nil},
		{"relative rejected", "/api/telegram", nil},
		{"empty", "", nil},
		{"whitespace", "   ", nil},
		{"no host", "https://", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := telegramNormalizeWebhookBaseURL(tc.input)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %q, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want %q", *tc.want)
			}
			if *got != *tc.want {
				t.Fatalf("got %q, want %q", *got, *tc.want)
			}
		})
	}

	t.Run("length cap", func(t *testing.T) {
		long := "https://example.com/" + strings.Repeat("a", telegramWebhookBaseURLMaxLength)
		if got := telegramNormalizeWebhookBaseURL(long); got != nil {
			t.Fatalf("expected an over-long URL to be rejected, got %q", *got)
		}
	})

	t.Run("production requires https", func(t *testing.T) {
		t.Setenv("NODE_ENV", "production")
		if got := telegramNormalizeWebhookBaseURL("http://pay.example.com"); got != nil {
			t.Fatalf("production must reject http, got %q", *got)
		}
		if got := telegramNormalizeWebhookBaseURL("https://pay.example.com"); got == nil {
			t.Fatal("production must accept https")
		}
	})
}

// TestTelegramBuildWebhookURL pins the precedence: explicit baseUrl, then
// SITE_URL, then the first FRONTEND_URL entry, and never forwarded headers.
func TestTelegramBuildWebhookURL(t *testing.T) {
	t.Setenv("NODE_ENV", "development")
	t.Setenv("SITE_URL", "")
	t.Setenv("FRONTEND_URL", "")

	t.Run("explicit base url wins", func(t *testing.T) {
		t.Setenv("SITE_URL", "https://site.example.com")
		got := telegramBuildWebhookURL(strPtr("https://explicit.example.com"))
		if got == nil || *got != "https://explicit.example.com/api/telegram/webhook" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("invalid explicit base url falls back to SITE_URL", func(t *testing.T) {
		// The handler rejects an invalid explicit baseUrl with a 400 before this
		// builder runs; the fallback is still the documented behaviour.
		t.Setenv("SITE_URL", "https://site.example.com")
		got := telegramBuildWebhookURL(strPtr("not a url"))
		if got == nil || *got != "https://site.example.com/api/telegram/webhook" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("SITE_URL", func(t *testing.T) {
		t.Setenv("SITE_URL", "https://site.example.com/path")
		got := telegramBuildWebhookURL(nil)
		if got == nil || *got != "https://site.example.com/api/telegram/webhook" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("FRONTEND_URL first entry", func(t *testing.T) {
		t.Setenv("FRONTEND_URL", "https://front.example.com,https://other.example.com")
		got := telegramBuildWebhookURL(nil)
		if got == nil || *got != "https://front.example.com/api/telegram/webhook" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("nothing configured", func(t *testing.T) {
		if got := telegramBuildWebhookURL(nil); got != nil {
			t.Fatalf("got %q, want nil", *got)
		}
	})

	t.Run("whitespace SITE_URL shadows FRONTEND_URL", func(t *testing.T) {
		// `SITE_URL || FRONTEND_URL...` is truthiness-based in JS, so a
		// whitespace-only SITE_URL wins and then trims to "" — FRONTEND_URL is not
		// consulted.
		t.Setenv("SITE_URL", "   ")
		t.Setenv("FRONTEND_URL", "https://front.example.com")
		if got := telegramBuildWebhookURL(nil); got != nil {
			t.Fatalf("got %q, want nil", *got)
		}
	})
}

// ==================== webhook body parsing ====================

// TestTelegramParseWebhookUpdate pins the `request.body?.message` destructuring.
func TestTelegramParseWebhookUpdate(t *testing.T) {
	t.Run("full update", func(t *testing.T) {
		body := map[string]any{
			"message": map[string]any{
				"chat": map[string]any{"id": float64(42), "type": "private"},
				"from": map[string]any{"id": float64(7), "username": "alice"},
				"text": "  /join  ",
			},
		}
		update, ok := telegramParseWebhookUpdate(body)
		if !ok {
			t.Fatal("expected a parsed update")
		}
		if update.ChatID != 42 {
			t.Fatalf("chatId = %v, want 42", update.ChatID)
		}
		if update.ChatType == nil || *update.ChatType != "private" {
			t.Fatalf("chatType = %v", update.ChatType)
		}
		if update.From == nil || update.From["username"] != "alice" {
			t.Fatalf("from = %v", update.From)
		}
		if update.Text == nil || *update.Text != "/join" {
			t.Fatalf("text = %v, want the trimmed value", update.Text)
		}
	})

	t.Run("missing message", func(t *testing.T) {
		if _, ok := telegramParseWebhookUpdate(map[string]any{}); ok {
			t.Fatal("expected no update")
		}
	})

	t.Run("null message", func(t *testing.T) {
		if _, ok := telegramParseWebhookUpdate(map[string]any{"message": nil}); ok {
			t.Fatal("expected no update")
		}
	})

	t.Run("non-object body", func(t *testing.T) {
		for _, body := range []any{nil, []any{}, "x", float64(1)} {
			if _, ok := telegramParseWebhookUpdate(body); ok {
				t.Fatalf("expected no update for %v", body)
			}
		}
	})

	t.Run("message without chat/from/text", func(t *testing.T) {
		update, ok := telegramParseWebhookUpdate(map[string]any{"message": map[string]any{}})
		if !ok {
			t.Fatal("expected an update (the handler then 200s on the empty fields)")
		}
		if update.ChatID != 0 || update.From != nil || update.Text != nil {
			t.Fatalf("unexpected values: %+v", update)
		}
	})

	t.Run("edited_message is not message", func(t *testing.T) {
		body := map[string]any{"edited_message": map[string]any{"chat": map[string]any{"id": float64(1)}}}
		if _, ok := telegramParseWebhookUpdate(body); ok {
			t.Fatal("edited_message must not be treated as message")
		}
	})
}

// TestTelegramBindCommandPattern pins /^\/start\s+bind_([A-Za-z0-9_-]{20,128})$/.
func TestTelegramBindCommandPattern(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		match bool
		token string
	}{
		{"valid", "/start bind_" + strings.Repeat("a", 32), true, strings.Repeat("a", 32)},
		{"valid with tab separator", "/start\tbind_" + strings.Repeat("a", 20), true, strings.Repeat("a", 20)},
		// The handler trims the text first (message.text.trim()), so surrounding
		// whitespace never reaches the anchored pattern.
		{"surrounding whitespace is trimmed first", "  /start bind_" + strings.Repeat("a", 32) + "  ", true, strings.Repeat("a", 32)},
		{"19 chars is too short", "/start bind_" + strings.Repeat("a", 19), false, ""},
		{"20 chars is accepted", "/start bind_" + strings.Repeat("a", 20), true, strings.Repeat("a", 20)},
		{"129 chars is too long", "/start bind_" + strings.Repeat("a", 129), false, ""},
		{"128 chars is accepted", "/start bind_" + strings.Repeat("a", 128), true, strings.Repeat("a", 128)},
		{"illegal chars", "/start bind_" + strings.Repeat("!", 32), false, ""},
		{"missing prefix", "/start " + strings.Repeat("a", 32), false, ""},
		{"inner newline is not \\s+", "/start\nbind_" + strings.Repeat("a", 32), true, strings.Repeat("a", 32)},
		{"trailing text", "/start bind_" + strings.Repeat("a", 32) + " extra", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The handler matches against the already-trimmed text.
			match := telegramBindCommandPattern.FindStringSubmatch(strings.TrimSpace(tc.text))
			if tc.match {
				if match == nil {
					t.Fatalf("expected %q to match", tc.text)
				}
				if match[1] != tc.token {
					t.Fatalf("captured %q, want %q", match[1], tc.token)
				}
				return
			}
			if match != nil {
				t.Fatalf("expected %q not to match", tc.text)
			}
		})
	}
}

// TestTelegramValidateSetupBody pins the POST /admin/webhook/setup schema
// `{ baseUrl?: string, minLength: 1 }` with AJV coercion.
func TestTelegramValidateSetupBody(t *testing.T) {
	cases := []struct {
		name    string
		body    any
		want    *string
		message string
	}{
		{"absent", map[string]any{}, nil, ""},
		{"valid", map[string]any{"baseUrl": "https://x.example.com"}, strPtr("https://x.example.com"), ""},
		{"empty is too short", map[string]any{"baseUrl": ""}, nil, "baseUrl is too short"},
		{"null coerces to empty", map[string]any{"baseUrl": nil}, nil, "baseUrl is too short"},
		{"number is stringified", map[string]any{"baseUrl": float64(5)}, strPtr("5"), ""},
		{"boolean is stringified", map[string]any{"baseUrl": true}, strPtr("true"), ""},
		{"single-element array unwraps", map[string]any{"baseUrl": []any{"https://x.example.com"}}, strPtr("https://x.example.com"), ""},
		{"two-element array is not a string", map[string]any{"baseUrl": []any{"a", "b"}}, nil, "must be string"},
		{"object is not a string", map[string]any{"baseUrl": map[string]any{}}, nil, "must be string"},
		{"non-object body", []any{}, nil, "must be object"},
		{"unknown keys are dropped", map[string]any{"baseUrl": "x", "other": float64(1)}, strPtr("x"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, message := telegramValidateSetupBody(tc.body)
			if message != tc.message {
				t.Fatalf("message = %q, want %q", message, tc.message)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %q, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want %q", *tc.want)
			}
			if *got != *tc.want {
				t.Fatalf("got %q, want %q", *got, *tc.want)
			}
		})
	}
}

// ==================== misc helpers ====================

// TestTelegramParsePositiveInteger pins parsePositiveInteger(value, fallback, max).
func TestTelegramParsePositiveInteger(t *testing.T) {
	cases := []struct {
		raw      string
		fallback int
		max      int
		want     int
	}{
		{"", 1, 100000, 1},
		{"7", 1, 100000, 7},
		{"0", 1, 100000, 1},
		{"-3", 1, 100000, 1},
		{"abc", 5, 100, 5},
		{"999", 5, 100, 100},
		{"2.9", 1, 100, 2},
		{" 12 ", 1, 100, 12},
	}
	for _, tc := range cases {
		if got := telegramParsePositiveInteger(tc.raw, tc.fallback, tc.max); got != tc.want {
			t.Fatalf("telegramParsePositiveInteger(%q, %d, %d) = %d, want %d",
				tc.raw, tc.fallback, tc.max, got, tc.want)
		}
	}
}

// TestTelegramJSString pins String(value) for the Telegram `from.id` shapes.
func TestTelegramJSString(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"number", float64(12345), "12345"},
		{"string", "12345", "12345"},
		{"bool", true, "true"},
		{"nil is undefined", nil, "undefined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := telegramJSString(tc.value); got != tc.want {
				t.Fatalf("telegramJSString(%v) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// TestTelegramISOTime pins Date#toISOString() formatting.
func TestTelegramISOTime(t *testing.T) {
	value := time.Date(2026, 10, 29, 13, 45, 6, 123_000_000, time.UTC)
	if got := telegramISOTime(value); got != "2026-10-29T13:45:06.123Z" {
		t.Fatalf("telegramISOTime() = %q", got)
	}

	offset := time.FixedZone("UTC+8", 8*3600)
	local := time.Date(2026, 10, 29, 21, 45, 6, 0, offset)
	if got := telegramISOTime(local); got != "2026-10-29T13:45:06.000Z" {
		t.Fatalf("telegramISOTime() must render in UTC, got %q", got)
	}
}

// TestTelegramSerializeGroupConfig pins the public group view: the chat id is
// deliberately absent (a private chat id is not a secret, but the Node view omits
// it and so must this one).
func TestTelegramSerializeGroupConfig(t *testing.T) {
	view := telegramSerializeGroupConfig(telegramGroupConfig{
		Enabled:             true,
		Configured:          true,
		ChatID:              strPtr("-100123"),
		JoinMode:            "all",
		MinRecharge:         10,
		MinConsume:          20,
		InviteExpireMinutes: 30,
	})

	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), "100123") {
		t.Fatalf("serialized group config leaks the chat id: %s", encoded)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantKeys := map[string]bool{
		"enabled": true, "configured": true, "joinMode": true,
		"minRecharge": true, "minConsume": true,
	}
	if len(decoded) != len(wantKeys) {
		t.Fatalf("keys = %v, want exactly %v", decoded, wantKeys)
	}
	for key := range wantKeys {
		if _, present := decoded[key]; !present {
			t.Fatalf("missing key %q in %v", key, decoded)
		}
	}
}
