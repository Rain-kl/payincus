// Unit tests for the mailer port. They live in package lib (not lib_test) so
// they can reach the unexported seams and helpers, and they never touch
// PostgreSQL or a real SMTP server: the config loader and the two delivery
// functions are swapped for fakes.
package lib

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// marshalForTest renders a value as JSON (route response shapes are asserted on
// the serialised form, not on Go structs).
func marshalForTest(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(encoded)
}

// mailerTestHarness owns the swapped globals for one test and restores them.
type mailerTestHarness struct {
	config    SMTPConfig
	configErr error

	// delivered collects every message that reached the transport.
	delivered []outgoingMail
	// deliveredConfig collects the config each delivery used.
	deliveredConfig []SMTPConfig
	deliverErr      error
	// verified counts connection probes.
	verified  int
	verifyErr error

	// now drives mailerNow; tests may advance it to exercise the config cache.
	now time.Time
}

// defaultMailerTestTime is a fixed clock so rendered dates and the copyright
// year never depend on when the suite runs.
var defaultMailerTestTime = time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)

func newMailerTestHarness(t *testing.T) *mailerTestHarness {
	t.Helper()

	harness := &mailerTestHarness{now: defaultMailerTestTime}
	previousLoader := mailConfigLoader
	previousDeliver := deliverMail
	previousVerify := verifyMail
	previousNow := mailerNow

	mailConfigLoader = func(context.Context) (SMTPConfig, error) {
		if harness.configErr != nil {
			return SMTPConfig{}, harness.configErr
		}
		return harness.config, nil
	}
	deliverMail = func(_ context.Context, config SMTPConfig, outgoing outgoingMail) (mailDeliveryInfo, error) {
		harness.deliveredConfig = append(harness.deliveredConfig, config)
		if harness.deliverErr != nil {
			return mailDeliveryInfo{}, harness.deliverErr
		}
		harness.delivered = append(harness.delivered, outgoing)
		return mailDeliveryInfo{
			ProviderMessageID: "<test@example.com>",
			Accepted:          []string{outgoing.To},
			ProviderResponse:  "250 2.0.0 Ok: queued as ABC123",
		}, nil
	}
	verifyMail = func(context.Context, SMTPConfig) error {
		harness.verified++
		return harness.verifyErr
	}
	mailerNow = func() time.Time { return harness.now }

	t.Cleanup(func() {
		mailConfigLoader = previousLoader
		deliverMail = previousDeliver
		verifyMail = previousVerify
		mailerNow = previousNow
		ClearTransporterCache()
	})

	// Every test starts from a cold config cache.
	ClearTransporterCache()
	return harness
}

// completeSMTPConfig is a fully configured, enabled SMTP setup.
func completeSMTPConfig() SMTPConfig {
	return SMTPConfig{
		Enabled:      true,
		Host:         "smtp.example.com",
		Port:         587,
		Secure:       false,
		Username:     "mailer",
		Password:     "s3cret",
		FromEmail:    "noreply@example.com",
		FromName:     "Incudal",
		BrandName:    "Incudal",
		BrandLogoURL: defaultBrandLogoURL,
	}
}

// TestToFixedJSMirrorsJavaScript pins Number#toFixed semantics. Go's
// FormatFloat rounds ties half-to-even while JavaScript rounds half-away-from-
// zero on the exact binary value, so this table is the guard against silently
// changing user-visible amounts.
func TestToFixedJSMirrorsJavaScript(t *testing.T) {
	cases := []struct {
		value  float64
		digits int
		want   string
	}{
		{0.125, 2, "0.13"}, // Go FormatFloat would give 0.12
		{-0.125, 2, "-0.13"},
		{0.375, 2, "0.38"},
		{0.625, 2, "0.63"},
		{1.625, 2, "1.63"},
		{0, 2, "0.00"},
		{-0.001, 2, "-0.00"}, // JavaScript keeps the sign of a rounded-to-zero value
		{1e-7, 2, "0.00"},
		{2.5, 2, "2.50"},
		{2.5, 0, "3"},
		{1.5, 0, "2"},
		{1.005, 2, "1.00"}, // exact binary value is just below 1.005
		{2.675, 2, "2.67"},
		{123456789.005, 2, "123456789.00"},
		{1e21, 2, "1e+21"}, // toFixed falls back to ToString above 1e21
		{-1e21, 2, "-1e+21"},
		{1e22, 2, "1e+22"},
	}
	for _, testCase := range cases {
		got := toFixedJS(testCase.value, testCase.digits)
		if got != testCase.want {
			t.Errorf("toFixedJS(%v, %d) = %q, want %q", testCase.value, testCase.digits, got, testCase.want)
		}
	}
}

// TestJSNumberToString pins String(number) for the interpolated values, notably
// the exponent spelling (JavaScript writes "1e-7", Go writes "1e-07").
func TestJSNumberToString(t *testing.T) {
	cases := []struct {
		value float64
		want  string
	}{
		{0, "0"},
		{100, "100"},
		{1.5, "1.5"},
		{-2.25, "-2.25"},
		{1e-7, "1e-7"},
		{1.5e-7, "1.5e-7"},
		{0.00001, "0.00001"},
		{1e21, "1e+21"},
		{1e22, "1e+22"},
	}
	for _, testCase := range cases {
		if got := jsNumberToString(testCase.value); got != testCase.want {
			t.Errorf("jsNumberToString(%v) = %q, want %q", testCase.value, got, testCase.want)
		}
	}
}

// TestFormatMemoryAndDisk pins the inline resource formatters (1 GiB boundary).
func TestFormatMemoryAndDisk(t *testing.T) {
	memoryCases := map[float64]string{
		512:  "512 MB",
		1023: "1023 MB",
		1024: "1.0 GB",
		1536: "1.5 GB",
		2048: "2.0 GB",
		0:    "0 MB",
	}
	for megabytes, want := range memoryCases {
		if got := formatMemory(megabytes); got != want {
			t.Errorf("formatMemory(%v) = %q, want %q", megabytes, got, want)
		}
	}

	diskCases := map[float64]string{
		512:   "512 MB",
		1024:  "1 GB",
		1536:  "2 GB", // 1.5 rounds away from zero
		2560:  "3 GB", // 2.5 rounds away from zero
		10239: "10 GB",
	}
	for megabytes, want := range diskCases {
		if got := formatDisk(megabytes); got != want {
			t.Errorf("formatDisk(%v) = %q, want %q", megabytes, got, want)
		}
	}
}

// TestEscapeEmailHTML confirms every HTML metacharacter is neutralised in a
// single pass, so interpolated user text cannot inject markup.
func TestEscapeEmailHTML(t *testing.T) {
	got := escapeEmailHTML(`<script>alert("x&y")</script>' OR 1=1`)
	want := `&lt;script&gt;alert(&quot;x&amp;y&quot;)&lt;/script&gt;&#039; OR 1=1`
	if got != want {
		t.Errorf("escapeEmailHTML = %q, want %q", got, want)
	}
	// A literal "&lt;" in the input is escaped again (not double-decoded).
	if got := escapeEmailHTML("&lt;"); got != "&amp;lt;" {
		t.Errorf("escapeEmailHTML(\"&lt;\") = %q, want %q", got, "&amp;lt;")
	}
}

// TestFormatBrandSubject pins the "[Brand] subject" shape used by every send.
func TestFormatBrandSubject(t *testing.T) {
	if got := formatBrandSubject("Incudal", "您的验证码：123456"); got != "[Incudal] 您的验证码：123456" {
		t.Errorf("formatBrandSubject = %q", got)
	}
}

// TestFormatDateTimeAndDate pins the Asia/Shanghai rendering, including the
// day-boundary rollover that a naive local-time format would get wrong.
func TestFormatDateTimeAndDate(t *testing.T) {
	cases := []struct {
		input    time.Time
		wantDate string
		wantTime string
	}{
		{
			input:    time.Date(2026, 10, 29, 6, 5, 7, 0, time.UTC),
			wantDate: "2026/10/29",
			wantTime: "2026/10/29 14:05:07",
		},
		{
			// 20:00 UTC is already the next day in UTC+8.
			input:    time.Date(2026, 1, 2, 20, 0, 0, 0, time.UTC),
			wantDate: "2026/01/03",
			wantTime: "2026/01/03 04:00:00",
		},
		{
			// 15:59:59 UTC stays on the same day; 16:00:00 rolls over.
			input:    time.Date(2026, 10, 28, 15, 59, 59, 0, time.UTC),
			wantDate: "2026/10/28",
			wantTime: "2026/10/28 23:59:59",
		},
		{
			input:    time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC),
			wantDate: "2026/10/29",
			wantTime: "2026/10/29 00:00:00",
		},
	}
	for _, testCase := range cases {
		if got := formatDate(testCase.input); got != testCase.wantDate {
			t.Errorf("formatDate(%v) = %q, want %q", testCase.input, got, testCase.wantDate)
		}
		if got := formatDateTime(testCase.input); got != testCase.wantTime {
			t.Errorf("formatDateTime(%v) = %q, want %q", testCase.input, got, testCase.wantTime)
		}
	}
}

// TestRedactEmailLikeValuesAndTruncate covers the SMTP-test proof metadata
// sanitisation: e-mail addresses in a provider response are replaced, and the
// response is capped at 240 characters.
func TestRedactEmailLikeValuesAndTruncate(t *testing.T) {
	got := redactEmailLikeValues("250 2.0.0 Ok: queued for user@example.com and ops@mail.example.org")
	if strings.Contains(got, "user@example.com") || strings.Contains(got, "ops@mail.example.org") {
		t.Fatalf("redactEmailLikeValues leaked an address: %q", got)
	}
	if !strings.Contains(got, "[redacted-email]") {
		t.Fatalf("redactEmailLikeValues = %q, want the redaction marker", got)
	}

	long := strings.Repeat("a", 300)
	if got := truncateRunes(long, mailerProviderResponseLimit); len(got) != mailerProviderResponseLimit {
		t.Fatalf("truncateRunes length = %d, want %d", len(got), mailerProviderResponseLimit)
	}
	// Multi-byte input is cut on rune boundaries, never mid-character.
	multi := strings.Repeat("验", 300)
	if got := truncateRunes(multi, mailerProviderResponseLimit); len([]rune(got)) != mailerProviderResponseLimit {
		t.Fatalf("truncateRunes kept %d runes, want %d", len([]rune(got)), mailerProviderResponseLimit)
	}

	info := buildSMTPTestDeliveryInfo(mailDeliveryInfo{
		ProviderMessageID: "<id@example.com>",
		Accepted:          []string{"a@example.com"},
		Rejected:          []string{"b@example.com", "c@example.com"},
		Pending:           nil,
		ProviderResponse:  "queued for d@example.com",
	})
	if info.AcceptedRecipientCount != 1 || info.RejectedRecipientCount != 2 || info.PendingRecipientCount != 0 {
		t.Fatalf("recipient counts = %+v", info)
	}
	if strings.Contains(info.ProviderResponse, "d@example.com") {
		t.Fatalf("provider response leaked an address: %q", info.ProviderResponse)
	}
}

// TestGenerateEmailTextStructure pins the plain-text layout, including the
// blank line between paragraphs and the footer.
func TestGenerateEmailTextStructure(t *testing.T) {
	newMailerTestHarness(t)

	text := generateEmailText(emailTextOptions{
		Title:        "注册验证码",
		Greeting:     "您好，",
		Paragraphs:   []string{"第一段", "第二段"},
		Code:         "123456",
		CodeExpiry:   "此验证码将在 10 分钟后失效",
		InfoItems:    []emailInfoItem{{Label: "节点", Value: "hk-1"}},
		InfoTitle:    "通知信息",
		ActionTip:    "请忽略此邮件。",
		BrandName:    "Incudal",
		BrandLogoURL: defaultBrandLogoURL,
	})

	wants := []string{
		"Incudal\n" + strings.Repeat("=", 50),
		"您好，",
		"第一段",
		"第二段",
		strings.Repeat("-", 30),
		"验证码: 123456",
		"此验证码将在 10 分钟后失效",
		"通知信息",
		"节点: hk-1",
		"注意: 请忽略此邮件。",
		"(C) 2026 Incudal. 此邮件由系统自动发送，请勿回复。",
	}
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("plain-text body missing %q\n--- body ---\n%s", want, text)
		}
	}
	if strings.Contains(text, "<") {
		t.Errorf("plain-text body must not contain markup:\n%s", text)
	}
}

// TestGenerateEmailHTML covers the template branches: title escaping, the
// embedded logo rule, the verification-code block, the info list and the
// per-alert-type palette.
func TestGenerateEmailHTML(t *testing.T) {
	newMailerTestHarness(t)

	html := generateEmailHTML(emailHTMLOptions{
		Title:        `标题 <b>"x"</b>`,
		Greeting:     "您好，<script>",
		AlertType:    emailAlertDanger,
		AlertTitle:   "账户已被禁用",
		AlertMessage: "原因 & 说明",
		Paragraphs:   []string{"第一段"},
		Code:         "654321",
		CodeExpiry:   "此验证码将在 5 分钟后失效",
		InfoItems: []emailInfoItem{
			{Label: "节点", Value: "hk-1"},
			{Label: "标题", Value: "运维通知"},
		},
		InfoTitle:  "通知信息",
		FooterNote: "自动测试邮件",
		ActionTip:  "请尽快处理",
		BrandName:  "Incudal",
		// A relative logo URL must NOT be embedded as an <img>.
		BrandLogoURL: "/logo.svg",
	})

	// Escaped, never raw.
	if strings.Contains(html, "<script>") {
		t.Fatal("HTML body contains an unescaped <script>")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("HTML body is missing the escaped greeting")
	}
	if !strings.Contains(html, "原因 &amp; 说明") {
		t.Error("HTML body is missing the escaped alert message")
	}
	// Template fields.
	for _, want := range []string{
		"<title>标题 &lt;b&gt;&quot;x&quot;&lt;/b&gt;</title>",
		"654321",
		"此验证码将在 5 分钟后失效",
		"通知信息",
		"节点",
		"hk-1",
		"运维通知",
		"自动测试邮件",
		"请尽快处理",
		"© 2026 Incudal. 此邮件由系统自动发送，请勿回复。",
		// Danger palette.
		"#fef2f2",
		"#ef4444",
		"#991b1b",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML body missing %q", want)
		}
	}
	// Unknown placeholder must never survive rendering.
	if strings.Contains(html, "{{") {
		t.Error("HTML body contains an unresolved template placeholder")
	}
	// Relative logo URL -> text header, no <img>.
	if strings.Contains(html, `src="/logo.svg"`) {
		t.Error("relative brand logo URL must not be embedded as an image")
	}

	// Absolute logo URL -> embedded <img>.
	withLogo := generateEmailHTML(emailHTMLOptions{
		Title:        "t",
		BrandName:    "Incudal",
		BrandLogoURL: "https://cdn.example.com/logo.png",
	})
	if !strings.Contains(withLogo, `src="https://cdn.example.com/logo.png"`) {
		t.Error("absolute brand logo URL must be embedded as an image")
	}

	// No info items -> no info block at all (and no stray border-bottom: none).
	minimal := generateEmailHTML(emailHTMLOptions{Title: "t", BrandName: "Incudal"})
	if strings.Contains(minimal, "border-bottom: none") {
		t.Error("an empty info list must not render the info block")
	}
}

// TestSMTPConfigCompleteAndEnabled pins the "enabled && host && username &&
// password" gate used by isSmtpEnabled and the transporter.
func TestSMTPConfigCompleteAndEnabled(t *testing.T) {
	harness := newMailerTestHarness(t)

	harness.config = completeSMTPConfig()
	enabled, err := IsSMTPEnabled(context.Background())
	if err != nil {
		t.Fatalf("IsSMTPEnabled: %v", err)
	}
	if !enabled {
		t.Fatal("a fully configured SMTP setup must report enabled")
	}

	// Each missing field, and enabled=false, must report not-enabled.
	mutations := map[string]func(*SMTPConfig){
		"disabled": func(config *SMTPConfig) { config.Enabled = false },
		"no host":  func(config *SMTPConfig) { config.Host = "" },
		"no user":  func(config *SMTPConfig) { config.Username = "" },
		"no pass":  func(config *SMTPConfig) { config.Password = "" },
	}
	for name, mutate := range mutations {
		config := completeSMTPConfig()
		mutate(&config)
		if smtpConfigComplete(config) {
			t.Errorf("%s: smtpConfigComplete must be false", name)
		}

		harness.config = config
		ClearTransporterCache()
		enabled, err := IsSMTPEnabled(context.Background())
		if err != nil {
			t.Fatalf("%s: IsSMTPEnabled: %v", name, err)
		}
		if enabled {
			t.Errorf("%s: IsSMTPEnabled must be false", name)
		}
	}

	// A missing from_email does NOT disable sending (it only breaks the
	// From header), matching the TypeScript predicate.
	config := completeSMTPConfig()
	config.FromEmail = ""
	if !smtpConfigComplete(config) {
		t.Error("from_email must not affect the enabled predicate")
	}
}

// TestTransportConfigCache exercises the 60s config cache and
// ClearTransporterCache, which is what "SMTP settings changed" relies on.
func TestTransportConfigCache(t *testing.T) {
	harness := newMailerTestHarness(t)

	base := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	harness.now = base

	loadCount := 0
	mailConfigLoader = func(context.Context) (SMTPConfig, error) {
		loadCount++
		return harness.config, nil
	}
	harness.config = completeSMTPConfig()

	if _, err := getTransportConfig(context.Background()); err != nil {
		t.Fatalf("getTransportConfig: %v", err)
	}
	if loadCount != 1 {
		t.Fatalf("first call loaded %d times, want 1", loadCount)
	}

	// Within the TTL the cached config is reused.
	harness.now = base.Add(59 * time.Second)
	if _, err := getTransportConfig(context.Background()); err != nil {
		t.Fatalf("getTransportConfig: %v", err)
	}
	if loadCount != 1 {
		t.Fatalf("cached call loaded %d times, want 1", loadCount)
	}

	// At/after the TTL it reloads.
	harness.now = base.Add(60 * time.Second)
	if _, err := getTransportConfig(context.Background()); err != nil {
		t.Fatalf("getTransportConfig: %v", err)
	}
	if loadCount != 2 {
		t.Fatalf("expired call loaded %d times, want 2", loadCount)
	}

	// Explicit invalidation.
	ClearTransporterCache()
	if _, err := getTransportConfig(context.Background()); err != nil {
		t.Fatalf("getTransportConfig: %v", err)
	}
	if loadCount != 3 {
		t.Fatalf("post-invalidation call loaded %d times, want 3", loadCount)
	}

	// An incomplete config yields nil (the "SMTP not configured" path) and is
	// not cached, so enabling SMTP takes effect immediately.
	ClearTransporterCache()
	harness.config = SMTPConfig{Enabled: false, Host: "smtp.example.com"}
	config, err := getTransportConfig(context.Background())
	if err != nil {
		t.Fatalf("getTransportConfig: %v", err)
	}
	if config != nil {
		t.Fatal("an incomplete config must yield nil")
	}
}

// TestSendVerificationEmailComposesPurpose pins the three subject/body variants
// and that the plain-text action tip is the *short* form while the HTML one is
// the long form (exactly like the TypeScript original).
//
// Note: generateEmailText never renders options.title — the TypeScript original
// has no title field either, so the title only appears in the HTML <title> and
// the code-mail subject. The purpose-specific paragraph is what distinguishes
// the plain-text bodies.
func TestSendVerificationEmailComposesPurpose(t *testing.T) {
	harness := newMailerTestHarness(t)
	harness.config = completeSMTPConfig()

	cases := []struct {
		purpose       VerificationPurpose
		wantTitle     string
		wantParagraph string
		wantShortTip  string
		wantLongTip   string
	}{
		{
			purpose:       VerificationPurposeRegister,
			wantTitle:     "注册验证码",
			wantParagraph: "您正在注册 Incudal 账户。请使用以下验证码完成操作：",
			wantShortTip:  "如果您没有请求此验证码，请忽略此邮件。",
			wantLongTip:   "如果您没有请求此验证码，请忽略此邮件。如果您持续收到此类邮件，建议检查您的账户安全。",
		},
		{
			purpose:       VerificationPurposeResetPassword,
			wantTitle:     "重置密码验证码",
			wantParagraph: "您正在重置 Incudal 账户密码。请使用以下验证码完成操作：",
			wantShortTip:  "如果您没有请求重置密码，请忽略此邮件。",
			wantLongTip:   "如果您没有请求重置密码，请忽略此邮件。如果您持续收到此类邮件，您的账户可能存在安全风险，建议尽快修改密码。",
		},
		{
			purpose:       VerificationPurposeChangeEmail,
			wantTitle:     "修改邮箱验证码",
			wantParagraph: "您正在修改 Incudal 账户邮箱地址。请使用以下验证码完成新邮箱验证：",
			wantShortTip:  "如果您没有请求修改邮箱，请忽略此邮件。",
			wantLongTip:   "如果您没有请求修改邮箱，请忽略此邮件，并检查您的账户安全。",
		},
	}

	for _, testCase := range cases {
		harness.delivered = nil
		result := SendVerificationEmail(context.Background(), "user@example.com", "987654", 10, testCase.purpose)
		if !result.Success {
			t.Fatalf("%s: send failed: %s", testCase.purpose, result.Error)
		}
		if len(harness.delivered) != 1 {
			t.Fatalf("%s: delivered %d messages, want 1", testCase.purpose, len(harness.delivered))
		}

		mail := harness.delivered[0]
		if mail.To != "user@example.com" {
			t.Errorf("%s: To = %q", testCase.purpose, mail.To)
		}
		// The code is in the subject and both bodies.
		if mail.Subject != "[Incudal] 您的验证码：987654" {
			t.Errorf("%s: Subject = %q", testCase.purpose, mail.Subject)
		}
		// The title only reaches the HTML document.
		if !strings.Contains(mail.HTML, testCase.wantTitle) {
			t.Errorf("%s: HTML missing title %q", testCase.purpose, testCase.wantTitle)
		}
		if !strings.Contains(mail.Text, testCase.wantParagraph) {
			t.Errorf("%s: plain text missing paragraph %q", testCase.purpose, testCase.wantParagraph)
		}
		if !strings.Contains(mail.Text, "验证码: 987654") {
			t.Errorf("%s: plain text missing the code", testCase.purpose)
		}
		if !strings.Contains(mail.Text, testCase.wantShortTip) {
			t.Errorf("%s: plain text missing the short action tip", testCase.purpose)
		}
		if !strings.Contains(mail.HTML, testCase.wantLongTip) {
			t.Errorf("%s: HTML missing the long action tip", testCase.purpose)
		}
		if !strings.Contains(mail.HTML, "987654") {
			t.Errorf("%s: HTML missing the code", testCase.purpose)
		}
		if mail.FromName != "Incudal" || mail.FromEmail != "noreply@example.com" {
			t.Errorf("%s: from = %q <%q>", testCase.purpose, mail.FromName, mail.FromEmail)
		}
	}
}

// TestSendVerificationEmailNotConfigured pins the "SMTP not configured"
// envelope, which routes turn into a 500 EMAIL_SEND_FAILED.
func TestSendVerificationEmailNotConfigured(t *testing.T) {
	harness := newMailerTestHarness(t)
	harness.config = SMTPConfig{Enabled: false, Host: "smtp.example.com", Username: "u", Password: "p"}

	result := SendVerificationEmail(context.Background(), "user@example.com", "123456", 10, VerificationPurposeRegister)
	if result.Success {
		t.Fatal("an unconfigured SMTP setup must not report success")
	}
	if result.Error != "SMTP not configured" {
		t.Fatalf("Error = %q, want %q", result.Error, "SMTP not configured")
	}
	if len(harness.delivered) != 0 {
		t.Fatal("nothing must be delivered when SMTP is not configured")
	}
}

// TestSendErrorsAreTokenSanitized guards the security requirement that a
// provider error can never echo a credential back to a caller. The delivered
// message text must also never contain the SMTP password.
func TestSendErrorsAreTokenSanitized(t *testing.T) {
	harness := newMailerTestHarness(t)
	harness.config = completeSMTPConfig()
	harness.deliverErr = errors.New(
		"535 auth failed for Bearer abc123.def456.ghi789 with pat_" + strings.Repeat("a", 43),
	)

	result := SendVerificationEmail(context.Background(), "user@example.com", "123456", 10, VerificationPurposeRegister)
	if result.Success {
		t.Fatal("a delivery failure must not report success")
	}
	for _, leaked := range []string{"abc123.def456.ghi789", "pat_" + strings.Repeat("a", 43)} {
		if strings.Contains(result.Error, leaked) {
			t.Fatalf("error message leaked a credential (%q): %q", leaked, result.Error)
		}
	}
	if !strings.Contains(result.Error, RedactedJWT) {
		t.Fatalf("error message was not sanitized: %q", result.Error)
	}

	// A successful send must never put the password in any body or subject.
	harness.deliverErr = nil
	harness.config.Password = "sup3r-secret-password"
	ClearTransporterCache()
	ok := SendVerificationEmail(context.Background(), "user@example.com", "123456", 10, VerificationPurposeRegister)
	if !ok.Success {
		t.Fatalf("send failed: %s", ok.Error)
	}
	mail := harness.delivered[len(harness.delivered)-1]
	for name, body := range map[string]string{"html": mail.HTML, "text": mail.Text, "subject": mail.Subject} {
		if strings.Contains(body, "sup3r-secret-password") {
			t.Fatalf("%s body leaked the SMTP password", name)
		}
	}
}

// TestOperationVerificationEmailComposition covers the sensitive-operation mail
// and, with it, the seam wiring: a successful send returns success.
func TestOperationVerificationEmailComposition(t *testing.T) {
	harness := newMailerTestHarness(t)
	harness.config = completeSMTPConfig()

	result := SendOperationVerificationEmail(context.Background(), "user@example.com", OperationVerificationEmailData{
		Username:         "alice",
		OperationName:    "修改密码",
		Code:             "246810",
		ExpiresInMinutes: 10,
	})
	if !result.Success {
		t.Fatalf("send failed: %s", result.Error)
	}
	if len(harness.delivered) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(harness.delivered))
	}
	mail := harness.delivered[0]
	if mail.Subject != "[Incudal] 操作验证码：246810" {
		t.Errorf("Subject = %q", mail.Subject)
	}
	for _, want := range []string{"敏感操作验证", "修改密码", "您好，alice", "246810", "此验证码将在 10 分钟后失效"} {
		if !strings.Contains(mail.HTML, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	if !strings.Contains(mail.Text, "验证码: 246810") {
		t.Error("plain text missing the code")
	}
}

// TestOperationVerificationEmailSeamIsWired proves the init() registration
// actually reaches RequestOperationVerification: the seam is non-nil and, with a
// working SMTP config, a code send succeeds.
func TestOperationVerificationEmailSeamIsWired(t *testing.T) {
	if OperationVerificationEmailSender == nil {
		t.Fatal("OperationVerificationEmailSender must be registered by mailer.go's init()")
	}

	harness := newMailerTestHarness(t)
	harness.config = completeSMTPConfig()

	result := OperationVerificationEmailSender("user@example.com", OperationVerificationEmailData{
		Username:         "bob",
		OperationName:    "删除账户",
		Code:             "112233",
		ExpiresInMinutes: 10,
	})
	if !result.Success {
		t.Fatalf("seam send failed: %s", result.Error)
	}
	if len(harness.delivered) != 1 {
		t.Fatalf("seam delivered %d messages, want 1", len(harness.delivered))
	}
	if !strings.Contains(harness.delivered[0].HTML, "112233") {
		t.Error("seam did not deliver the verification code")
	}

	// And it degrades to an explicit failure (never a silent success) when SMTP
	// is not configured, which is what the seam contract requires.
	ClearTransporterCache()
	harness.config = SMTPConfig{}
	failing := OperationVerificationEmailSender("user@example.com", OperationVerificationEmailData{
		Username: "bob", OperationName: "删除账户", Code: "112233", ExpiresInMinutes: 10,
	})
	if failing.Success {
		t.Fatal("an unconfigured seam must report failure, not silent success")
	}
}

// TestBanNotificationEmail pins the danger-alert variant, the reason paragraph
// and the fixed subject.
func TestBanNotificationEmail(t *testing.T) {
	harness := newMailerTestHarness(t)
	harness.config = completeSMTPConfig()

	result := SendBanNotificationEmail(context.Background(), "user@example.com", BanNotificationEmailData{
		Username: "carol",
		Reason:   "多次违规 <b>滥用</b>",
	})
	if !result.Success {
		t.Fatalf("send failed: %s", result.Error)
	}
	mail := harness.delivered[0]
	if mail.Subject != "[Incudal] 账户已被禁用" {
		t.Errorf("Subject = %q", mail.Subject)
	}
	if !strings.Contains(mail.HTML, "封禁原因：多次违规 &lt;b&gt;滥用&lt;/b&gt;") {
		t.Error("HTML must escape the ban reason")
	}
	if !strings.Contains(mail.Text, "封禁原因：多次违规 <b>滥用</b>") {
		t.Error("plain text must keep the reason verbatim")
	}
	if !strings.Contains(mail.HTML, "#fef2f2") {
		t.Error("ban mail must use the danger palette")
	}
}

// TestSendTestEmailValidation covers the admin send-test guard rails and the
// safe provider metadata in the success envelope.
func TestSendTestEmailValidation(t *testing.T) {
	harness := newMailerTestHarness(t)

	// Incomplete configuration (still allowed to be *tested*).
	harness.config = SMTPConfig{Host: "smtp.example.com", Username: "u", FromEmail: "a@example.com"}
	result := SendTestEmail(context.Background(), "user@example.com")
	if result.Success {
		t.Fatal("incomplete configuration must not succeed")
	}
	if !strings.Contains(result.Error, "SMTP configuration incomplete") {
		t.Fatalf("Error = %q", result.Error)
	}

	// Invalid recipient.
	harness.config = completeSMTPConfig()
	for _, recipient := range []string{"", "not-an-email", "user.example.com"} {
		ClearTransporterCache()
		invalid := SendTestEmail(context.Background(), recipient)
		if invalid.Success {
			t.Fatalf("recipient %q must be rejected", recipient)
		}
		if invalid.Error != "Invalid recipient email address" {
			t.Fatalf("recipient %q: Error = %q", recipient, invalid.Error)
		}
	}
	if len(harness.delivered) != 0 {
		t.Fatal("invalid recipients must not be delivered to")
	}

	// Success path exposes counts, never recipient lists.
	ClearTransporterCache()
	success := SendTestEmail(context.Background(), "user@example.com")
	if !success.Success {
		t.Fatalf("send failed: %s", success.Error)
	}
	if success.AcceptedRecipientCount != 1 || success.RejectedRecipientCount != 0 || success.PendingRecipientCount != 0 {
		t.Fatalf("counts = %+v", success.SMTPTestDeliveryInfo)
	}
	if success.ProviderMessageID != "<test@example.com>" {
		t.Fatalf("ProviderMessageID = %q", success.ProviderMessageID)
	}
	if !strings.Contains(success.ProviderResponse, "queued") {
		t.Fatalf("ProviderResponse = %q", success.ProviderResponse)
	}
	if success.Error != "" {
		t.Fatalf("Error must be empty on success, got %q", success.Error)
	}
}

// TestTestSMTPConnection covers the verify-only probe: field validation, the
// "allowed while disabled" rule, and the fallback error text.
func TestTestSMTPConnection(t *testing.T) {
	harness := newMailerTestHarness(t)

	// Disabled but fully configured: testing is still allowed.
	config := completeSMTPConfig()
	config.Enabled = false
	harness.config = config

	result := TestSMTPConnection(context.Background())
	if !result.Success {
		t.Fatalf("verify should succeed while disabled but configured: %s", result.Error)
	}
	if harness.verified != 1 {
		t.Fatalf("verify called %d times, want 1", harness.verified)
	}
	if len(harness.delivered) != 0 {
		t.Fatal("a connection probe must not deliver a message")
	}

	// Missing from_email is fatal for the probe (unlike for sending).
	harness.config = SMTPConfig{Host: "smtp.example.com", Username: "u", Password: "p"}
	ClearTransporterCache()
	incomplete := TestSMTPConnection(context.Background())
	if incomplete.Success {
		t.Fatal("a probe with no sender address must fail")
	}
	if !strings.Contains(incomplete.Error, "SMTP configuration incomplete") {
		t.Fatalf("Error = %q", incomplete.Error)
	}

	// A transport error is surfaced, sanitized, and falls back to a generic
	// message when the provider gives no text.
	harness.config = completeSMTPConfig()
	ClearTransporterCache()
	harness.verifyErr = errors.New("dial tcp: i/o timeout")
	timedOut := TestSMTPConnection(context.Background())
	if timedOut.Success {
		t.Fatal("a failed probe must not report success")
	}
	if timedOut.Error != "dial tcp: i/o timeout" {
		t.Fatalf("Error = %q", timedOut.Error)
	}

	ClearTransporterCache()
	harness.verifyErr = errors.New("")
	empty := TestSMTPConnection(context.Background())
	if empty.Success {
		t.Fatal("a failed probe must not report success")
	}
	if empty.Error != "Connection failed" {
		t.Fatalf("Error = %q, want %q", empty.Error, "Connection failed")
	}
}

// TestBillingEmailComposition spot-checks two of the many transactional
// templates: the recharge notice (amount formatting) and the balance adjustment
// (sign handling), both of which interpolate user-visible money.
func TestBillingEmailComposition(t *testing.T) {
	harness := newMailerTestHarness(t)
	harness.config = completeSMTPConfig()
	when := time.Date(2026, 10, 29, 6, 5, 7, 0, time.UTC)

	tradeNo := "T-1"
	recharge := SendRechargeSuccessEmail(context.Background(), "user@example.com", RechargeSuccessEmailData{
		Username:   "dave",
		Amount:     0.125,
		OrderNo:    "O-1",
		TradeNo:    &tradeNo,
		NewBalance: 10,
		Time:       when,
	})
	if !recharge.Success {
		t.Fatalf("recharge send failed: %s", recharge.Error)
	}
	mail := harness.delivered[len(harness.delivered)-1]
	// 0.125 -> "0.13" (half away from zero), not Go's "0.12".
	if !strings.Contains(mail.Text, "充值金额: ¥0.13") {
		t.Errorf("recharge amount not formatted like JS toFixed:\n%s", mail.Text)
	}
	if !strings.Contains(mail.Text, "到账时间: 2026/10/29 14:05:07") {
		t.Errorf("recharge time not rendered in Asia/Shanghai:\n%s", mail.Text)
	}
	if !strings.Contains(mail.Subject, "¥0.13") {
		t.Errorf("Subject = %q", mail.Subject)
	}

	// Negative balance adjustment keeps the leading minus after abs().
	decrease := SendBalanceAdjustedEmail(context.Background(), "user@example.com", BalanceAdjustedEmailData{
		Username:   "dave",
		Amount:     -12.5,
		Remark:     "人工调整",
		NewBalance: 87.5,
		Time:       when,
	})
	if !decrease.Success {
		t.Fatalf("balance adjust send failed: %s", decrease.Error)
	}
	adjusted := harness.delivered[len(harness.delivered)-1]
	if !strings.Contains(adjusted.Subject, "余额变动通知 -¥12.50") {
		t.Errorf("Subject = %q", adjusted.Subject)
	}
	if !strings.Contains(adjusted.Text, "当前余额: ¥87.50") {
		t.Errorf("balance value not formatted:\n%s", adjusted.Text)
	}
	if !strings.Contains(adjusted.HTML, "#fffbeb") {
		t.Error("a deduction must use the warning palette")
	}

	// A positive adjustment uses the success palette, and amount 0 counts as a
	// deduction (the TypeScript condition is `amount > 0`).
	increase := SendBalanceAdjustedEmail(context.Background(), "user@example.com", BalanceAdjustedEmailData{
		Username: "dave", Amount: 5, Remark: "补偿", NewBalance: 92.5, Time: when,
	})
	if !increase.Success {
		t.Fatalf("balance increase send failed: %s", increase.Error)
	}
	increased := harness.delivered[len(harness.delivered)-1]
	if !strings.Contains(increased.HTML, "#f0fdf4") {
		t.Error("an increase must use the success palette")
	}
	if !strings.Contains(increased.Subject, "+¥5.00") {
		t.Errorf("Subject = %q", increased.Subject)
	}
}

// TestHostAnnouncementParagraphs pins the \r\n -> \n normalisation and the
// trim, which decides how many paragraphs the mail renders. Only leading and
// trailing whitespace of the whole string is trimmed — inner indentation is
// preserved, exactly like String#trim in the TypeScript original.
func TestHostAnnouncementParagraphs(t *testing.T) {
	cases := []struct {
		content string
		want    []string
	}{
		{"单行通知", []string{"单行通知"}},
		{"第一行\r\n第二行", []string{"第一行", "第二行"}},
		{"  首尾空白  \r\n\r\n中间空行", []string{"首尾空白  ", "", "中间空行"}},
		{"\r\n\r\n", []string{""}},
		{"\n\n前后空行\n\n", []string{"前后空行"}},
	}
	for _, testCase := range cases {
		got := normalizeAnnouncementParagraphs(testCase.content)
		if len(got) != len(testCase.want) {
			t.Fatalf("normalizeAnnouncementParagraphs(%q) = %#v, want %#v", testCase.content, got, testCase.want)
		}
		for index := range got {
			if got[index] != testCase.want[index] {
				t.Fatalf("normalizeAnnouncementParagraphs(%q)[%d] = %q, want %q", testCase.content, index, got[index], testCase.want[index])
			}
		}
	}
}

// TestEmailSendResultJSONShape pins the wire shape routes depend on: a success
// omits the error field, a failure carries it.
func TestEmailSendResultJSONShape(t *testing.T) {
	success := EmailSendResult{Success: true}
	if got := marshalForTest(t, success); got != `{"success":true}` {
		t.Errorf("success envelope = %s", got)
	}
	failure := EmailSendResult{Success: false, Error: "SMTP not configured"}
	if got := marshalForTest(t, failure); got != `{"success":false,"error":"SMTP not configured"}` {
		t.Errorf("failure envelope = %s", got)
	}
	empty := SMTPTestDeliveryResult{Success: false, Error: "boom"}
	if got := marshalForTest(t, empty); !strings.Contains(got, `"acceptedRecipientCount":0`) {
		t.Errorf("test result must always include the counts: %s", got)
	}
}

// TestLoadSMTPConfigFromDBWithoutDatabase pins the guard that keeps a missing
// database from panicking the mailer (routes surface it as a send failure).
func TestLoadSMTPConfigFromDBWithoutDatabase(t *testing.T) {
	previous := db.DB
	db.DB = nil
	t.Cleanup(func() { db.DB = previous })

	if _, err := loadSMTPConfigFromDB(context.Background()); err == nil {
		t.Fatal("loadSMTPConfigFromDB must fail when the database is not initialized")
	}

	result := SendVerificationEmail(context.Background(), "user@example.com", "123456", 10, VerificationPurposeRegister)
	if result.Success {
		t.Fatal("sending without a database must not succeed")
	}
	if !strings.Contains(result.Error, "database not initialized") {
		t.Fatalf("Error = %q", result.Error)
	}
}
