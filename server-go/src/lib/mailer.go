// Port of server/src/lib/mailer.ts.
//
// Transactional e-mail: verification codes, password reset, login alerts, ban
// notifications, billing/instance notices and the admin SMTP test tools.
//
// Mail templates follow the same five rules as the Node original: one visual
// language, inline CSS only (clients strip <style>), both HTML and plain-text
// parts (deliverability), responsive table layout, and no spam-trigger wording.
//
// Deviations from the TypeScript original, all behaviour-preserving:
//   - nodemailer's transporter is not pooled. Sending opens a short-lived SMTP
//     session per message (go-mail's Client), while the *resolved configuration*
//     is cached for the same 60 seconds the Node transporter was cached for.
//     ClearTransporterCache() still drops it, so "config changed -> next mail
//     uses the new config" keeps working.
//   - nodemailer's connectionTimeout/greetingTimeout (10s) map onto go-mail's
//     connection timeout; socketTimeout (30s) maps onto the per-send deadline.
//     A send can never block longer than 30s.
//   - Error strings returned to callers and written to the log are run through
//     SanitizeTokensInString so a provider response can never leak a JWT or a
//     pat_/poa_ token into an API response or the log file.
//
// Wiring: this file registers itself as
// lib.OperationVerificationEmailSender in init(), so
// RequestOperationVerification can deliver email codes once the module is
// linked. See the seam in operation_verification.go.
package lib

import (
	"context"
	"errors"
	"log"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wneessen/go-mail"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// ==================== configuration ====================

const (
	// defaultBrandName mirrors DEFAULT_BRAND_NAME.
	defaultBrandName = "Incudal"
	// defaultBrandLogoURL mirrors DEFAULT_BRAND_LOGO_URL.
	defaultBrandLogoURL = "/logo.svg"
	// legacyBrandLogoURL mirrors the '/incudal_logo.webp' legacy value that is
	// replaced by the default logo.
	legacyBrandLogoURL = "/incudal_logo.webp"

	// smtpConfigCacheTTL mirrors CONFIG_CACHE_TTL (1 minute).
	smtpConfigCacheTTL = 60 * time.Second
	// mailerConnectionTimeout mirrors nodemailer's connectionTimeout/greetingTimeout.
	mailerConnectionTimeout = 10 * time.Second
	// mailerSocketTimeout mirrors nodemailer's socketTimeout: the whole session
	// is capped at 30s.
	mailerSocketTimeout = 30 * time.Second
	// mailerProviderResponseLimit mirrors the `.slice(0, 240)` cap applied to the
	// provider response in the SMTP test result.
	mailerProviderResponseLimit = 240
)

// SMTPConfig mirrors the SmtpConfig interface. The password is already
// decrypted; it is never logged and never serialised.
type SMTPConfig struct {
	Enabled      bool
	Host         string
	Port         int
	Secure       bool
	Username     string
	Password     string
	FromEmail    string
	FromName     string
	BrandName    string
	BrandLogoURL string
}

// SMTPTestDeliveryInfo mirrors SmtpTestDeliveryInfo: safe provider proof
// metadata. Recipient *lists* are intentionally reduced to counts so an admin
// test can never dump addresses, and the provider response has e-mail-like
// values redacted.
type SMTPTestDeliveryInfo struct {
	ProviderMessageID      string `json:"providerMessageId,omitempty"`
	AcceptedRecipientCount int    `json:"acceptedRecipientCount"`
	RejectedRecipientCount int    `json:"rejectedRecipientCount"`
	PendingRecipientCount  int    `json:"pendingRecipientCount"`
	ProviderResponse       string `json:"providerResponse,omitempty"`
}

// SMTPTestDeliveryResult mirrors SmtpTestDeliveryResult.
type SMTPTestDeliveryResult struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
	SMTPTestDeliveryInfo
}

// EmailSendResult mirrors the {success, error?} envelope every send function
// returns in the TypeScript original.
type EmailSendResult struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// VerificationPurpose mirrors VerificationPurpose.
type VerificationPurpose string

const (
	// VerificationPurposeRegister mirrors 'register'.
	VerificationPurposeRegister VerificationPurpose = "register"
	// VerificationPurposeResetPassword mirrors 'reset_password'.
	VerificationPurposeResetPassword VerificationPurpose = "reset_password"
	// VerificationPurposeChangeEmail mirrors 'change_email'.
	VerificationPurposeChangeEmail VerificationPurpose = "change_email"
)

var errMailerNoDatabase = errors.New("mailer: database not initialized")

// ==================== injectable seams (tests, wiring) ====================

// mailConfigLoader resolves the SMTP configuration. Swapped in tests so the
// suite never touches PostgreSQL.
var mailConfigLoader = loadSMTPConfigFromDB

// deliverMail performs one SMTP delivery. Swapped in tests so the suite never
// opens a socket.
var deliverMail = deliverSMTPMail

// verifyMail performs a connect+EHLO+AUTH only (no message). Swapped in tests.
var verifyMail = verifySMTPConnection

// mailerNow is swappable in tests so the 60s config cache and the copyright year
// can be exercised without sleeping.
var mailerNow = time.Now

// ==================== transporter (config) cache ====================

var (
	transporterCacheMu   sync.Mutex
	transporterCache     *SMTPConfig
	transporterCacheTime time.Time
)

// getMailBrandName mirrors getMailBrandName: the trimmed brand_name, or the
// default when unset.
func getMailBrandName(ctx context.Context) (string, error) {
	configured, err := db.GetSystemConfig(ctx, "brand_name", true)
	if err != nil {
		return "", err
	}
	if configured != nil {
		if brandName := strings.TrimSpace(*configured); brandName != "" {
			return brandName, nil
		}
	}
	return defaultBrandName, nil
}

// getMailBrandLogoURL mirrors getMailBrandLogoUrl: the trimmed brand_logo_url,
// or the default when unset or still set to the legacy asset.
func getMailBrandLogoURL(ctx context.Context) (string, error) {
	configured, err := db.GetSystemConfig(ctx, "brand_logo_url", true)
	if err != nil {
		return "", err
	}
	if configured != nil {
		logoURL := strings.TrimSpace(*configured)
		if logoURL != "" && logoURL != legacyBrandLogoURL {
			return logoURL, nil
		}
	}
	return defaultBrandLogoURL, nil
}

// formatBrandSubject mirrors formatBrandSubject: "[Brand] subject".
func formatBrandSubject(brandName, subject string) string {
	return "[" + brandName + "] " + subject
}

// loadSMTPConfigFromDB mirrors getSmtpConfig: read the smtp_* keys from
// system_config and decrypt the stored password.
func loadSMTPConfigFromDB(ctx context.Context) (SMTPConfig, error) {
	if db.DB == nil {
		return SMTPConfig{}, errMailerNoDatabase
	}

	enabled, err := db.GetSystemConfigBoolean(ctx, "smtp_enabled", false)
	if err != nil {
		return SMTPConfig{}, err
	}
	hostValue, err := db.GetSystemConfig(ctx, "smtp_host", true)
	if err != nil {
		return SMTPConfig{}, err
	}
	port, err := db.GetSystemConfigNumber(ctx, "smtp_port", 587)
	if err != nil {
		return SMTPConfig{}, err
	}
	secure, err := db.GetSystemConfigBoolean(ctx, "smtp_secure", false)
	if err != nil {
		return SMTPConfig{}, err
	}
	usernameValue, err := db.GetSystemConfig(ctx, "smtp_username", true)
	if err != nil {
		return SMTPConfig{}, err
	}
	// The raw value is read so an already-decrypted plaintext password that
	// happens to contain ':' is never fed through AES-GCM a second time.
	passwordValue, err := db.GetSystemConfig(ctx, "smtp_password", false)
	if err != nil {
		return SMTPConfig{}, err
	}
	fromEmailValue, err := db.GetSystemConfig(ctx, "smtp_from_email", true)
	if err != nil {
		return SMTPConfig{}, err
	}
	fromNameValue, err := db.GetSystemConfig(ctx, "smtp_from_name", true)
	if err != nil {
		return SMTPConfig{}, err
	}
	brandName, err := getMailBrandName(ctx)
	if err != nil {
		return SMTPConfig{}, err
	}
	brandLogoURL, err := getMailBrandLogoURL(ctx)
	if err != nil {
		return SMTPConfig{}, err
	}

	fromName := ""
	if fromNameValue != nil {
		fromName = strings.TrimSpace(*fromNameValue)
	}
	effectiveFromName := brandName
	if fromName != "" && fromName != defaultBrandName {
		effectiveFromName = fromName
	}

	// If the password is encrypted (iv:tag:ciphertext) decrypt it first.
	decryptedPassword := ""
	if passwordValue != nil {
		decryptedPassword = *passwordValue
	}
	if decryptedPassword != "" && strings.Contains(decryptedPassword, ":") {
		if decrypted := DecryptSensitiveData(decryptedPassword); decrypted != "" {
			decryptedPassword = decrypted
		}
	}

	return SMTPConfig{
		Enabled:      enabled,
		Host:         stringValue(hostValue),
		Port:         port,
		Secure:       secure,
		Username:     stringValue(usernameValue),
		Password:     decryptedPassword,
		FromEmail:    stringValue(fromEmailValue),
		FromName:     effectiveFromName,
		BrandName:    brandName,
		BrandLogoURL: brandLogoURL,
	}, nil
}

// stringValue mirrors `value || ”` for a nullable string.
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// GetSMTPConfig mirrors getSmtpConfig. The returned config always has a brand
// name, a brand logo URL and an effective From name.
func GetSMTPConfig(ctx context.Context) (SMTPConfig, error) {
	return mailConfigLoader(ctx)
}

// smtpConfigComplete mirrors the `enabled && host && username && password`
// guard used by isSmtpEnabled and getTransporter.
func smtpConfigComplete(config SMTPConfig) bool {
	return config.Enabled && config.Host != "" && config.Username != "" && config.Password != ""
}

// IsSMTPEnabled mirrors isSmtpEnabled: SMTP is enabled *and* fully configured.
func IsSMTPEnabled(ctx context.Context) (bool, error) {
	config, err := mailConfigLoader(ctx)
	if err != nil {
		return false, err
	}
	return smtpConfigComplete(config), nil
}

// getTransportConfig mirrors getTransporter: the cached, enabled configuration,
// or nil when SMTP is not usable.
func getTransportConfig(ctx context.Context) (*SMTPConfig, error) {
	transporterCacheMu.Lock()
	defer transporterCacheMu.Unlock()

	if transporterCache != nil && mailerNow().Sub(transporterCacheTime) < smtpConfigCacheTTL {
		return transporterCache, nil
	}

	config, err := mailConfigLoader(ctx)
	if err != nil {
		return nil, err
	}
	if !smtpConfigComplete(config) {
		return nil, nil
	}

	cached := config
	transporterCache = &cached
	transporterCacheTime = mailerNow()
	return transporterCache, nil
}

// ClearTransporterCache mirrors clearTransporterCache: drop the cached
// configuration so the next send re-reads system_config.
func ClearTransporterCache() {
	transporterCacheMu.Lock()
	defer transporterCacheMu.Unlock()
	transporterCache = nil
	transporterCacheTime = time.Time{}
}

// ==================== template helpers ====================

// emailHTMLEscapeReplacer mirrors escapeHtml. A single-pass replacer is used so
// a replacement is never re-escaped (the JS implementation maps in one pass too).
var emailHTMLEscapeReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#039;",
)

// escapeEmailHTML mirrors escapeHtml (XSS-safe text interpolation).
func escapeEmailHTML(text string) string {
	return emailHTMLEscapeReplacer.Replace(text)
}

// mailLocation is Asia/Shanghai as a fixed +08:00 zone: the zone has had no DST
// since 1991, and a fixed zone keeps formatting independent of tzdata being
// installed in the runtime image.
var mailLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

// getCurrentYear mirrors `new Date().getFullYear()` (server-local year).
func getCurrentYear() int {
	return mailerNow().Year()
}

// formatDateTime mirrors toLocaleString('zh-CN', {Asia/Shanghai, 2-digit ...})
// which renders as "YYYY/MM/DD HH:mm:ss".
func formatDateTime(value time.Time) string {
	return value.In(mailLocation).Format("2006/01/02 15:04:05")
}

// formatDate mirrors toLocaleDateString('zh-CN', {Asia/Shanghai, 2-digit ...})
// which renders as "YYYY/MM/DD".
func formatDate(value time.Time) string {
	return value.In(mailLocation).Format("2006/01/02")
}

// emailLikePattern mirrors /[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}/gi.
var emailLikePattern = regexp.MustCompile(`(?i)[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}`)

// redactEmailLikeValues mirrors redactEmailLikeValues.
func redactEmailLikeValues(value string) string {
	return emailLikePattern.ReplaceAllString(value, "[redacted-email]")
}

// normalizeStringArray mirrors normalizeStringArray. A Go []string cannot hold
// non-string entries, so the JS "filter out non-strings" step collapses to a
// copy that preserves nil.
func normalizeStringArray(value []string) []string {
	if value == nil {
		return nil
	}
	out := make([]string, len(value))
	copy(out, value)
	return out
}

// buildSMTPTestDeliveryInfo mirrors buildSmtpTestDeliveryInfo.
func buildSMTPTestDeliveryInfo(info mailDeliveryInfo) SMTPTestDeliveryInfo {
	accepted := normalizeStringArray(info.Accepted)
	rejected := normalizeStringArray(info.Rejected)
	pending := normalizeStringArray(info.Pending)

	response := ""
	if info.ProviderResponse != "" {
		response = truncateRunes(redactEmailLikeValues(info.ProviderResponse), mailerProviderResponseLimit)
	}

	return SMTPTestDeliveryInfo{
		ProviderMessageID:      info.ProviderMessageID,
		AcceptedRecipientCount: len(accepted),
		RejectedRecipientCount: len(rejected),
		PendingRecipientCount:  len(pending),
		ProviderResponse:       response,
	}
}

// buildFailedSMTPTestDeliveryResult mirrors buildFailedSmtpTestDeliveryResult.
func buildFailedSMTPTestDeliveryResult(errorMessage string) SMTPTestDeliveryResult {
	return SMTPTestDeliveryResult{
		Success: false,
		Error:   errorMessage,
	}
}

// truncateRunes mirrors String#slice for the ASCII-ish provider responses while
// staying safe on multi-byte input.
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// ==================== JS number formatting ====================

// toFixedJS mirrors Number.prototype.toFixed.
//
// This is *not* strconv.FormatFloat(v, 'f', digits, 64): Go rounds the exact
// binary value half-to-even, JS rounds half-away-from-zero on the exact binary
// value. They disagree on ties, e.g. 0.125 -> JS "0.13" vs Go "0.12". Amounts
// and memory sizes end up in user-visible text, so the JS semantics are
// reproduced exactly with big.Rat.
func toFixedJS(value float64, digits int) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	}

	// Number#toFixed falls back to ToString for |x| >= 1e21.
	if math.Abs(value) >= 1e21 {
		return jsNumberToString(value)
	}

	negative := value < 0
	magnitude := value
	if negative {
		magnitude = -magnitude
	}

	scaled := new(big.Rat).SetFloat64(magnitude)
	if scaled == nil {
		return "0"
	}
	scaleFactor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	scaled.Mul(scaled, new(big.Rat).SetInt(scaleFactor))

	quotient, remainder := new(big.Int).QuoRem(scaled.Num(), scaled.Denom(), new(big.Int))
	// Round half away from zero on the exact value.
	if new(big.Int).Mul(remainder, big.NewInt(2)).Cmp(scaled.Denom()) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}

	digitsText := quotient.String()
	if digits == 0 {
		if negative {
			return "-" + digitsText
		}
		return digitsText
	}
	if len(digitsText) <= digits {
		digitsText = strings.Repeat("0", digits-len(digitsText)+1) + digitsText
	}
	split := len(digitsText) - digits
	result := digitsText[:split] + "." + digitsText[split:]
	if negative {
		result = "-" + result
	}
	return result
}

// jsNumberToString mirrors String(number) for the values interpolated into mail
// bodies (CPU percent, month/day counts).
func jsNumberToString(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	case value == 0:
		return "0"
	}
	abs := math.Abs(value)
	if abs < 1e-6 || abs >= 1e21 {
		return normalizeJSExponent(strconv.FormatFloat(value, 'e', -1, 64))
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// jsExponentZeroPattern matches the "e-07" / "e+05" form Go emits and
// JavaScript does not: String(1e-7) is "1e-7", not "1e-07".
var jsExponentZeroPattern = regexp.MustCompile(`e([+-])0+(\d)`)

// normalizeJSExponent rewrites a Go exponent into JavaScript's spelling.
func normalizeJSExponent(value string) string {
	return jsExponentZeroPattern.ReplaceAllString(value, "e$1$2")
}

// formatMemory mirrors the inline formatMemory helper: GB above 1 GiB, MB below.
func formatMemory(megabytes float64) string {
	if megabytes >= 1024 {
		return toFixedJS(megabytes/1024, 1) + " GB"
	}
	return jsNumberToString(megabytes) + " MB"
}

// formatDisk mirrors the inline formatDisk helper: GB above 1 GiB without
// decimals, MB below.
func formatDisk(megabytes float64) string {
	if megabytes >= 1024 {
		return toFixedJS(megabytes/1024, 0) + " GB"
	}
	return jsNumberToString(megabytes) + " MB"
}

// ==================== HTML template ====================

// emailInfoItem mirrors the EmailInfoItem interface.
type emailInfoItem struct {
	Label string
	Value string
}

// emailAlertType mirrors EmailAlertType.
type emailAlertType string

const (
	emailAlertWarning emailAlertType = "warning"
	emailAlertInfo    emailAlertType = "info"
	emailAlertSuccess emailAlertType = "success"
	emailAlertDanger  emailAlertType = "danger"
)

type emailAlertColors struct {
	background string
	border     string
	text       string
}

// emailAlertPalette mirrors the alertColors map.
var emailAlertPalette = map[emailAlertType]emailAlertColors{
	emailAlertWarning: {background: "#fffbeb", border: "#f59e0b", text: "#92400e"},
	emailAlertInfo:    {background: "#eff6ff", border: "#3b82f6", text: "#1e40af"},
	emailAlertSuccess: {background: "#f0fdf4", border: "#22c55e", text: "#166534"},
	emailAlertDanger:  {background: "#fef2f2", border: "#ef4444", text: "#991b1b"},
}

// emailHTMLOptions mirrors the generateEmailHtml options object.
type emailHTMLOptions struct {
	Title        string
	Greeting     string
	AlertType    emailAlertType
	AlertTitle   string
	AlertMessage string
	Paragraphs   []string
	Code         string
	CodeExpiry   string
	InfoItems    []emailInfoItem
	InfoTitle    string
	FooterNote   string
	ActionTip    string
	BrandName    string
	BrandLogoURL string
}

// brandLogoPattern mirrors /^https?:\/\//i.
var brandLogoPattern = regexp.MustCompile(`(?i)^https?://`)

// emailDocumentTemplate is the outer shell. Placeholders are replaced in one
// pass so interpolated user text can never be interpreted as a placeholder.
const emailDocumentTemplate = `<!DOCTYPE html>
<html lang="zh-CN" xmlns="http://www.w3.org/1999/xhtml" xmlns:v="urn:schemas-microsoft-com:vml" xmlns:o="urn:schemas-microsoft-com:office:office">
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <meta http-equiv="X-UA-Compatible" content="IE=edge">
    <meta name="x-apple-disable-message-reformatting">
    <meta name="format-detection" content="telephone=no,address=no,email=no,date=no,url=no">
    <title>{{TITLE}}</title>
    <!--[if mso]>
    <noscript>
        <xml>
            <o:OfficeDocumentSettings>
                <o:PixelsPerInch>96</o:PixelsPerInch>
            </o:OfficeDocumentSettings>
        </xml>
    </noscript>
    <![endif]-->
    <style type="text/css">
        body, table, td, p, a, li, blockquote { -webkit-text-size-adjust: 100%; -ms-text-size-adjust: 100%; }
        table, td { mso-table-lspace: 0pt; mso-table-rspace: 0pt; }
        img { -ms-interpolation-mode: bicubic; border: 0; height: auto; line-height: 100%; outline: none; text-decoration: none; }
        body { margin: 0 !important; padding: 0 !important; width: 100% !important; }
        a[x-apple-data-detectors] { color: inherit !important; text-decoration: none !important; font-size: inherit !important; font-family: inherit !important; font-weight: inherit !important; line-height: inherit !important; }
        @media only screen and (max-width: 600px) {
            .email-container { width: 100% !important; }
            .mobile-padding { padding-left: 20px !important; padding-right: 20px !important; }
        }
    </style>
</head>
<body style="margin: 0; padding: 0; background-color: #ffffff; font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif;">
    <table role="presentation" cellpadding="0" cellspacing="0" border="0" width="100%" style="background-color: #ffffff;">
        <tr>
            <td align="center" style="padding: 0;">
                <table role="presentation" class="email-container" cellpadding="0" cellspacing="0" border="0" width="600" style="max-width: 600px; width: 100%;">
                    <!-- Header -->
                    <tr>
                        <td class="mobile-padding" style="padding: 40px 24px 32px 24px; border-bottom: 1px solid #e5e5e5;">
                            {{BRAND_HEADER}}
                        </td>
                    </tr>
                    <!-- Content -->
                    <tr>
                        <td class="mobile-padding" style="padding: 32px 24px;">
                            <table role="presentation" cellpadding="0" cellspacing="0" border="0" width="100%">
                                {{ALERT}}{{GREETING}}{{PARAGRAPHS}}{{CODE}}{{INFO}}{{ACTION_TIP}}
                            </table>
                        </td>
                    </tr>
                    <!-- Footer -->
                    <tr>
                        <td class="mobile-padding" style="padding: 24px; border-top: 1px solid #e5e5e5;">
                            <p style="margin: 0; font-size: 12px; color: #a3a3a3; line-height: 1.5;">© {{YEAR}} {{BRAND}}. 此邮件由系统自动发送，请勿回复。</p>
                            {{FOOTER_NOTE}}
                        </td>
                    </tr>
                </table>
            </td>
        </tr>
    </table>
</body>
</html>`

// generateEmailHTML mirrors generateEmailHtml.
func generateEmailHTML(options emailHTMLOptions) string {
	year := getCurrentYear()
	brandName := strings.TrimSpace(options.BrandName)
	if brandName == "" {
		brandName = defaultBrandName
	}
	brandLogoURL := strings.TrimSpace(options.BrandLogoURL)
	canEmbedLogo := brandLogoPattern.MatchString(brandLogoURL)

	var brandHeaderHTML string
	if canEmbedLogo {
		brandHeaderHTML = `<table role="presentation" cellpadding="0" cellspacing="0" border="0">
                                <tr>
                                    <td style="vertical-align: middle; padding: 0 12px 0 0;">
                                        <img src="` + escapeEmailHTML(brandLogoURL) + `" width="32" height="32" alt="` + escapeEmailHTML(brandName) + `" style="display: block; width: 32px; height: 32px; object-fit: contain; border: 0;">
                                    </td>
                                    <td style="vertical-align: middle;">
                                        <p style="margin: 0; font-size: 18px; font-weight: 600; color: #000000; letter-spacing: -0.02em;">` + escapeEmailHTML(brandName) + `</p>
                                    </td>
                                </tr>
                            </table>`
	} else {
		brandHeaderHTML = `<p style="margin: 0; font-size: 18px; font-weight: 600; color: #000000; letter-spacing: -0.02em;">` + escapeEmailHTML(brandName) + `</p>`
	}

	alertHTML := ""
	if options.AlertType != "" && options.AlertTitle != "" {
		colors := emailAlertPalette[options.AlertType]
		messageHTML := ""
		if options.AlertMessage != "" {
			messageHTML = `<p style="margin: 0; font-size: 13px; color: ` + colors.text + `; line-height: 1.5;">` + escapeEmailHTML(options.AlertMessage) + `</p>`
		}
		alertHTML = `
            <tr>
                <td style="padding: 0 0 24px 0;">
                    <table width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color: ` + colors.background + `; border-left: 4px solid ` + colors.border + `;">
                        <tr>
                            <td style="padding: 16px 20px;">
                                <p style="margin: 0 0 4px 0; font-size: 14px; font-weight: 600; color: ` + colors.text + `;">` + escapeEmailHTML(options.AlertTitle) + `</p>
                                ` + messageHTML + `
                            </td>
                        </tr>
                    </table>
                </td>
            </tr>`
	}

	greetingHTML := ""
	if options.Greeting != "" {
		greetingHTML = `
            <tr>
                <td style="padding: 0 0 16px 0;">
                    <p style="margin: 0; font-size: 14px; color: #000000; line-height: 1.6;">` + escapeEmailHTML(options.Greeting) + `</p>
                </td>
            </tr>`
	}

	paragraphsHTML := ""
	if len(options.Paragraphs) > 0 {
		var builder strings.Builder
		for _, paragraph := range options.Paragraphs {
			builder.WriteString(`
            <tr>
                <td style="padding: 0 0 16px 0;">
                    <p style="margin: 0; font-size: 14px; color: #000000; line-height: 1.6;">`)
			builder.WriteString(escapeEmailHTML(paragraph))
			builder.WriteString(`</p>
                </td>
            </tr>`)
		}
		paragraphsHTML = builder.String()
	}

	codeHTML := ""
	if options.Code != "" {
		expiryHTML := ""
		if options.CodeExpiry != "" {
			expiryHTML = `<p style="margin: 16px 0 0 0; font-size: 13px; color: #737373;">` + escapeEmailHTML(options.CodeExpiry) + `</p>`
		}
		codeHTML = `
            <tr>
                <td style="padding: 8px 0 24px 0;">
                    <table width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color: #f5f5f5; border: 1px solid #e5e5e5;">
                        <tr>
                            <td style="padding: 28px 24px; text-align: center;">
                                <p style="margin: 0; font-size: 32px; font-weight: 600; letter-spacing: 8px; color: #000000; font-family: 'Consolas', 'Monaco', monospace;">` + escapeEmailHTML(options.Code) + `</p>
                                ` + expiryHTML + `
                            </td>
                        </tr>
                    </table>
                </td>
            </tr>`
	}

	infoHTML := ""
	if len(options.InfoItems) > 0 {
		var rows strings.Builder
		for index, item := range options.InfoItems {
			borderBottom := "1px solid #e5e5e5"
			if index == len(options.InfoItems)-1 {
				borderBottom = "none"
			}
			rows.WriteString(`
                <tr>
                    <td style="padding: 12px 0; border-bottom: `)
			rows.WriteString(borderBottom)
			rows.WriteString(`;">
                        <table width="100%" cellpadding="0" cellspacing="0" border="0">
                            <tr>
                                <td style="font-size: 13px; color: #737373; width: 120px; vertical-align: top;">`)
			rows.WriteString(escapeEmailHTML(item.Label))
			rows.WriteString(`</td>
                                <td style="font-size: 13px; color: #000000; font-weight: 500;">`)
			rows.WriteString(escapeEmailHTML(item.Value))
			rows.WriteString(`</td>
                            </tr>
                        </table>
                    </td>
                </tr>`)
		}

		infoTitleHTML := ""
		if options.InfoTitle != "" {
			infoTitleHTML = `<p style="margin: 0 0 16px 0; font-size: 13px; font-weight: 600; color: #000000;">` + escapeEmailHTML(options.InfoTitle) + `</p>`
		}

		infoHTML = `
            <tr>
                <td style="padding: 8px 0 24px 0;">
                    <table width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color: #f5f5f5; border: 1px solid #e5e5e5;">
                        <tr>
                            <td style="padding: 20px;">
                                ` + infoTitleHTML + `
                                <table width="100%" cellpadding="0" cellspacing="0" border="0">
                                    ` + rows.String() + `
                                </table>
                            </td>
                        </tr>
                    </table>
                </td>
            </tr>`
	}

	actionTipHTML := ""
	if options.ActionTip != "" {
		actionTipHTML = `
            <tr>
                <td style="padding: 16px 0 0 0;">
                    <table width="100%" cellpadding="0" cellspacing="0" border="0" style="background-color: #fafafa; border: 1px solid #e5e5e5;">
                        <tr>
                            <td style="padding: 14px 16px;">
                                <p style="margin: 0; font-size: 12px; color: #737373; line-height: 1.5;">` + escapeEmailHTML(options.ActionTip) + `</p>
                            </td>
                        </tr>
                    </table>
                </td>
            </tr>`
	}

	footerNoteHTML := ""
	if options.FooterNote != "" {
		footerNoteHTML = `<p style="margin: 8px 0 0 0; font-size: 12px; color: #a3a3a3;">` + escapeEmailHTML(options.FooterNote) + `</p>`
	}

	return strings.NewReplacer(
		"{{TITLE}}", escapeEmailHTML(options.Title),
		"{{BRAND_HEADER}}", brandHeaderHTML,
		"{{ALERT}}", alertHTML,
		"{{GREETING}}", greetingHTML,
		"{{PARAGRAPHS}}", paragraphsHTML,
		"{{CODE}}", codeHTML,
		"{{INFO}}", infoHTML,
		"{{ACTION_TIP}}", actionTipHTML,
		"{{YEAR}}", strconv.Itoa(year),
		"{{BRAND}}", escapeEmailHTML(brandName),
		"{{FOOTER_NOTE}}", footerNoteHTML,
	).Replace(emailDocumentTemplate)
}

// emailTextOptions mirrors the generateEmailText options object. Title is
// accepted for parity with the HTML generator but, like the Node original, is
// not rendered in the plain-text body.
type emailTextOptions struct {
	Title        string
	Greeting     string
	AlertTitle   string
	AlertMessage string
	Paragraphs   []string
	Code         string
	CodeExpiry   string
	InfoItems    []emailInfoItem
	InfoTitle    string
	ActionTip    string
	BrandName    string
	BrandLogoURL string
}

// generateEmailText mirrors generateEmailText.
func generateEmailText(options emailTextOptions) string {
	year := getCurrentYear()
	brandName := strings.TrimSpace(options.BrandName)
	if brandName == "" {
		brandName = defaultBrandName
	}

	lines := make([]string, 0, 24)
	lines = append(lines, brandName)
	lines = append(lines, strings.Repeat("=", 50))
	lines = append(lines, "")

	if options.AlertTitle != "" {
		lines = append(lines, "["+options.AlertTitle+"]")
		if options.AlertMessage != "" {
			lines = append(lines, options.AlertMessage)
		}
		lines = append(lines, "")
	}

	if options.Greeting != "" {
		lines = append(lines, options.Greeting)
		lines = append(lines, "")
	}

	for _, paragraph := range options.Paragraphs {
		lines = append(lines, paragraph)
		lines = append(lines, "")
	}

	if options.Code != "" {
		lines = append(lines, strings.Repeat("-", 30))
		lines = append(lines, "验证码: "+options.Code)
		if options.CodeExpiry != "" {
			lines = append(lines, options.CodeExpiry)
		}
		lines = append(lines, strings.Repeat("-", 30))
		lines = append(lines, "")
	}

	if len(options.InfoItems) > 0 {
		if options.InfoTitle != "" {
			lines = append(lines, options.InfoTitle)
		}
		lines = append(lines, strings.Repeat("-", 30))
		for _, item := range options.InfoItems {
			lines = append(lines, item.Label+": "+item.Value)
		}
		lines = append(lines, strings.Repeat("-", 30))
		lines = append(lines, "")
	}

	if options.ActionTip != "" {
		lines = append(lines, "注意: "+options.ActionTip)
		lines = append(lines, "")
	}

	lines = append(lines, strings.Repeat("=", 50))
	lines = append(lines, "(C) "+strconv.Itoa(year)+" "+brandName+". 此邮件由系统自动发送，请勿回复。")

	return strings.Join(lines, "\n")
}

// ==================== SMTP delivery ====================

// outgoingMail is one fully rendered message.
type outgoingMail struct {
	To        string
	Subject   string
	Text      string
	HTML      string
	FromName  string
	FromEmail string
}

// mailDeliveryInfo is the transport-level proof collected after a send. It
// mirrors the subset of nodemailer's SentMessageInfo that the SMTP test uses.
type mailDeliveryInfo struct {
	ProviderMessageID string
	Accepted          []string
	Rejected          []string
	Pending           []string
	ProviderResponse  string
}

// newSMTPClient builds a go-mail client for the given configuration.
func newSMTPClient(config SMTPConfig) (*mail.Client, error) {
	if config.Host == "" {
		return nil, errors.New("SMTP host is not configured")
	}

	options := []mail.Option{
		mail.WithPort(config.Port),
		// Connection + greeting timeout (nodemailer: 10000ms each).
		mail.WithTimeout(mailerConnectionTimeout),
	}
	if config.Secure {
		// Implicit TLS, like nodemailer's `secure: true` (usually port 465).
		options = append(options, mail.WithSSL())
	} else {
		// opportunistic STARTTLS: upgrade when the server offers it, otherwise
		// stay plaintext (nodemailer's default with requireTLS=false).
		options = append(options, mail.WithTLSPolicy(mail.TLSOpportunistic))
	}
	if config.Username != "" {
		options = append(options,
			mail.WithUsername(config.Username),
			mail.WithPassword(config.Password),
			// Pick the strongest mechanism the server advertises; PLAIN/LOGIN are
			// allowed over plaintext because nodemailer would do the same.
			mail.WithOpportunisticSMTPAuth(
				mail.SMTPAuthSCRAMSHA256,
				mail.SMTPAuthSCRAMSHA1,
				mail.SMTPAuthCramMD5,
				mail.SMTPAuthNTLM,
				mail.SMTPAuthPlainNoEnc,
				mail.SMTPAuthLoginNoEnc,
			),
		)
	}

	return mail.NewClient(config.Host, options...)
}

// buildMailMessage renders one go-mail message with a text part and an HTML
// alternative (the order nodemailer uses for {text, html}).
func buildMailMessage(outgoing outgoingMail) (*mail.Msg, error) {
	message := mail.NewMsg()
	if outgoing.FromName != "" {
		if err := message.FromFormat(outgoing.FromName, outgoing.FromEmail); err != nil {
			return nil, err
		}
	} else if err := message.From(outgoing.FromEmail); err != nil {
		return nil, err
	}
	if err := message.To(outgoing.To); err != nil {
		return nil, err
	}
	message.Subject(outgoing.Subject)
	message.SetBodyString(mail.TypeTextPlain, outgoing.Text)
	message.AddAlternativeString(mail.TypeTextHTML, outgoing.HTML)
	return message, nil
}

// deliverSMTPMail opens a session and sends exactly one message, mirroring
// transporter.sendMail(...) including the 30s socket cap.
func deliverSMTPMail(ctx context.Context, config SMTPConfig, outgoing outgoingMail) (mailDeliveryInfo, error) {
	client, err := newSMTPClient(config)
	if err != nil {
		return mailDeliveryInfo{}, err
	}
	message, err := buildMailMessage(outgoing)
	if err != nil {
		return mailDeliveryInfo{}, err
	}

	sendCtx, cancel := context.WithTimeout(ctx, mailerSocketTimeout)
	defer cancel()

	if err := client.DialAndSendWithContext(sendCtx, message); err != nil {
		return mailDeliveryInfo{}, err
	}

	info := mailDeliveryInfo{
		ProviderMessageID: message.GetMessageID(),
		ProviderResponse:  message.ServerResponse(),
	}
	if recipients, err := message.GetRecipients(); err == nil {
		info.Accepted = recipients
	}
	return info, nil
}

// verifySMTPConnection mirrors transporter.verify(): connect, EHLO, STARTTLS and
// AUTH, then disconnect without sending anything.
func verifySMTPConnection(ctx context.Context, config SMTPConfig) error {
	client, err := newSMTPClient(config)
	if err != nil {
		return err
	}

	verifyCtx, cancel := context.WithTimeout(ctx, mailerSocketTimeout)
	defer cancel()

	if err := client.DialWithContext(verifyCtx); err != nil {
		return err
	}
	return client.Close()
}

// ==================== send plumbing ====================

// mailComposer renders one message from the resolved configuration.
type mailComposer func(config SMTPConfig) (outgoingMail, error)

// mailErrorMessage mirrors `error instanceof Error ? error.message : String(error)`.
func mailErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// failedEmailResult logs and returns a failure envelope. The log line and the
// returned message are token-sanitized so a provider response can never echo a
// credential back to a caller or into the log file.
func failedEmailResult(logLabel string, err error) EmailSendResult {
	message := SanitizeTokensInString(mailErrorMessage(err))
	log.Printf("Failed to send %s email: %s", logLabel, message)
	return EmailSendResult{Success: false, Error: message}
}

// sendBrandedEmail mirrors the shared prologue of every sendXxx function:
// resolve the transporter (nil -> "SMTP not configured"), render, deliver.
func sendBrandedEmail(ctx context.Context, logLabel string, compose mailComposer) EmailSendResult {
	config, err := getTransportConfig(ctx)
	if err != nil {
		return failedEmailResult(logLabel, err)
	}
	if config == nil {
		return EmailSendResult{Success: false, Error: "SMTP not configured"}
	}

	outgoing, err := compose(*config)
	if err != nil {
		return failedEmailResult(logLabel, err)
	}
	if _, err := deliverMail(ctx, *config, outgoing); err != nil {
		return failedEmailResult(logLabel, err)
	}
	return EmailSendResult{Success: true}
}

// mailFrom mirrors `config.fromName ? "\"name\" <email>" : email`. The header is
// built from structured parts in buildMailMessage; this helper only exists for
// the plain-text display of the sender in the SMTP test body.
func mailFromDisplay(config SMTPConfig) string {
	return config.FromName + " <" + config.FromEmail + ">"
}

// ==================== verification code email ====================

// SendVerificationEmail mirrors sendVerificationEmail: the registration,
// password-reset and change-email code mail.
func SendVerificationEmail(ctx context.Context, email, code string, expiresInMinutes int, purpose VerificationPurpose) EmailSendResult {
	return sendBrandedEmail(ctx, "verification", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName

		var title, paragraph, actionTip string
		switch purpose {
		case VerificationPurposeResetPassword:
			title = "重置密码验证码"
			paragraph = "您正在重置 " + brandName + " 账户密码。请使用以下验证码完成操作："
			actionTip = "如果您没有请求重置密码，请忽略此邮件。如果您持续收到此类邮件，您的账户可能存在安全风险，建议尽快修改密码。"
		case VerificationPurposeChangeEmail:
			title = "修改邮箱验证码"
			paragraph = "您正在修改 " + brandName + " 账户邮箱地址。请使用以下验证码完成新邮箱验证："
			actionTip = "如果您没有请求修改邮箱，请忽略此邮件，并检查您的账户安全。"
		default:
			title = "注册验证码"
			paragraph = "您正在注册 " + brandName + " 账户。请使用以下验证码完成操作："
			actionTip = "如果您没有请求此验证码，请忽略此邮件。如果您持续收到此类邮件，建议检查您的账户安全。"
		}

		codeExpiry := "此验证码将在 " + strconv.Itoa(expiresInMinutes) + " 分钟后失效"

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        title,
			Greeting:     "您好，",
			Paragraphs:   []string{paragraph},
			Code:         code,
			CodeExpiry:   codeExpiry,
			ActionTip:    actionTip,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		shortActionTip := "如果您没有请求此验证码，请忽略此邮件。"
		switch purpose {
		case VerificationPurposeResetPassword:
			shortActionTip = "如果您没有请求重置密码，请忽略此邮件。"
		case VerificationPurposeChangeEmail:
			shortActionTip = "如果您没有请求修改邮箱，请忽略此邮件。"
		}

		textContent := generateEmailText(emailTextOptions{
			Title:        title,
			Greeting:     "您好，",
			Paragraphs:   []string{paragraph},
			Code:         code,
			CodeExpiry:   codeExpiry,
			ActionTip:    shortActionTip,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "您的验证码："+code),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// ==================== SMTP diagnostics ====================

// TestSMTPConnection mirrors testSmtpConnection: allowed even while
// smtp_enabled is false, as long as the individual fields are filled in.
func TestSMTPConnection(ctx context.Context) EmailSendResult {
	config, err := mailConfigLoader(ctx)
	if err != nil {
		return failedSMTPTest(err)
	}
	if config.Host == "" || config.Username == "" || config.Password == "" || config.FromEmail == "" {
		return EmailSendResult{
			Success: false,
			Error:   "SMTP configuration incomplete. Please fill in host, username, password, and sender email.",
		}
	}

	if err := verifyMail(ctx, config); err != nil {
		return failedSMTPTest(err)
	}
	return EmailSendResult{Success: true}
}

// failedSMTPTest mirrors the catch block of testSmtpConnection.
func failedSMTPTest(err error) EmailSendResult {
	message := SanitizeTokensInString(mailErrorMessage(err))
	log.Printf("SMTP connection test failed: %s", message)
	if message == "" {
		message = "Connection failed"
	}
	return EmailSendResult{Success: false, Error: message}
}

// SendTestEmail mirrors sendTestEmail: a real delivery used by the admin panel,
// also allowed while smtp_enabled is false.
func SendTestEmail(ctx context.Context, to string) SMTPTestDeliveryResult {
	config, err := mailConfigLoader(ctx)
	if err != nil {
		return buildFailedSMTPTestDeliveryResult(SanitizeTokensInString(mailErrorMessage(err)))
	}

	if config.Host == "" || config.Username == "" || config.Password == "" || config.FromEmail == "" {
		return buildFailedSMTPTestDeliveryResult("SMTP configuration incomplete. Please fill in host, username, password, and sender email.")
	}
	if to == "" || !strings.Contains(to, "@") {
		return buildFailedSMTPTestDeliveryResult("Invalid recipient email address")
	}

	brandName := config.BrandName
	testTime := formatDateTime(mailerNow())

	infoItems := []emailInfoItem{
		{Label: "SMTP 服务器", Value: config.Host},
		{Label: "端口", Value: strconv.Itoa(config.Port)},
		{Label: "SSL/TLS", Value: boolLabel(config.Secure, "已启用", "未启用")},
		{Label: "发件人", Value: mailFromDisplay(config)},
		{Label: "测试时间", Value: testTime},
	}

	htmlContent := generateEmailHTML(emailHTMLOptions{
		Title:        "SMTP 测试邮件",
		AlertType:    emailAlertSuccess,
		AlertTitle:   "SMTP 配置测试",
		AlertMessage: "如果您收到此邮件，说明您的 SMTP 设置工作正常。",
		Greeting:     "您好，",
		Paragraphs:   []string{"这是一封来自您的 " + brandName + " SMTP 配置的测试邮件。"},
		InfoTitle:    "测试详情",
		InfoItems:    infoItems,
		FooterNote:   "这是一封自动测试邮件。",
		BrandName:    brandName,
		BrandLogoURL: config.BrandLogoURL,
	})

	textContent := generateEmailText(emailTextOptions{
		Title:        "SMTP 测试邮件",
		AlertTitle:   "SMTP 配置测试",
		AlertMessage: "如果您收到此邮件，说明您的 SMTP 设置工作正常。",
		Greeting:     "您好，",
		Paragraphs:   []string{"这是一封来自您的 " + brandName + " SMTP 配置的测试邮件。"},
		InfoTitle:    "测试详情",
		InfoItems:    infoItems,
		BrandName:    brandName,
		BrandLogoURL: config.BrandLogoURL,
	})

	outgoing := outgoingMail{
		To:        to,
		Subject:   formatBrandSubject(brandName, "SMTP 配置测试成功"),
		Text:      textContent,
		HTML:      htmlContent,
		FromName:  config.FromName,
		FromEmail: config.FromEmail,
	}

	deliveryInfo, err := deliverMail(ctx, config, outgoing)
	if err != nil {
		message := SanitizeTokensInString(mailErrorMessage(err))
		log.Printf("Failed to send test email: %s", message)
		return buildFailedSMTPTestDeliveryResult(message)
	}

	result := SMTPTestDeliveryResult{Success: true}
	result.SMTPTestDeliveryInfo = buildSMTPTestDeliveryInfo(deliveryInfo)
	return result
}

// boolLabel mirrors `cond ? whenTrue : whenFalse`.
func boolLabel(condition bool, whenTrue, whenFalse string) string {
	if condition {
		return whenTrue
	}
	return whenFalse
}

// LoginAlertEmailData is the payload for SendLoginAlertEmail.
type LoginAlertEmailData struct {
	Username    string
	IP          string
	UserAgent   string
	LoginTime   time.Time
	IsNewIP     bool
	IsNewDevice bool
}

// SendLoginAlertEmail mirrors sendLoginAlertEmail.
func SendLoginAlertEmail(ctx context.Context, email string, data LoginAlertEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "login alert", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName
		loginTime := formatDateTime(data.LoginTime)

		// Truncate over-long User-Agents.
		userAgent := data.UserAgent
		if len([]rune(userAgent)) > 80 {
			userAgent = string([]rune(userAgent)[:80]) + "..."
		}

		alertType := "新设备登录"
		alertMessage := "您的账户刚刚在一个新的设备上登录。"
		if data.IsNewIP && data.IsNewDevice {
			alertType = "新设备和新 IP 地址登录"
			alertMessage = "您的账户刚刚在一个新的位置和新的设备上登录。"
		} else if data.IsNewIP {
			alertType = "新 IP 地址登录"
			alertMessage = "您的账户刚刚在一个新的 IP 地址登录。"
		}

		infoItems := []emailInfoItem{
			{Label: "登录时间", Value: loginTime},
			{Label: "IP 地址", Value: data.IP},
			{Label: "设备", Value: userAgent},
		}

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        "安全提醒",
			AlertType:    emailAlertWarning,
			AlertTitle:   alertType,
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"我们检测到您的账户有一次新的登录活动："},
			InfoTitle:    "登录详情",
			InfoItems:    infoItems,
			ActionTip:    "如果这是您本人的操作，请忽略此邮件。如果这不是您本人的操作，请立即登录您的账户并修改密码。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        "安全提醒",
			AlertTitle:   alertType,
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"我们检测到您的账户有一次新的登录活动："},
			InfoTitle:    "登录详情",
			InfoItems:    infoItems,
			ActionTip:    "如果这不是您本人的操作，请立即修改密码。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "安全提醒："+alertType),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// SendOperationVerificationEmail mirrors sendOperationVerificationEmail. The
// payload type is OperationVerificationEmailData, declared next to the seam in
// operation_verification.go, so a send can be wired straight into
// OperationVerificationEmailSender without a conversion.
func SendOperationVerificationEmail(ctx context.Context, email string, data OperationVerificationEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "operation verification", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName
		alertMessage := "您正在尝试执行「" + data.OperationName + "」操作。"
		codeExpiry := "此验证码将在 " + strconv.Itoa(data.ExpiresInMinutes) + " 分钟后失效"

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        "操作验证码",
			AlertType:    emailAlertWarning,
			AlertTitle:   "敏感操作验证",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"请使用以下验证码完成操作确认："},
			Code:         data.Code,
			CodeExpiry:   codeExpiry,
			ActionTip:    "如果这不是您本人的操作，请立即修改您的账户密码并检查账户安全。请勿将此验证码透露给任何人。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        "操作验证码",
			AlertTitle:   "敏感操作验证",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"请使用以下验证码完成操作确认："},
			Code:         data.Code,
			CodeExpiry:   codeExpiry,
			ActionTip:    "如果这不是您本人的操作，请立即修改密码。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "操作验证码："+data.Code),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// ==================== billing / instance notifications ====================

// RechargeSuccessEmailData is the payload for SendRechargeSuccessEmail.
type RechargeSuccessEmailData struct {
	Username   string
	Amount     float64
	OrderNo    string
	TradeNo    *string
	NewBalance float64
	Time       time.Time
}

// SendRechargeSuccessEmail mirrors sendRechargeSuccessEmail.
func SendRechargeSuccessEmail(ctx context.Context, email string, data RechargeSuccessEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "recharge success", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName
		timeText := formatDateTime(data.Time)
		amountText := "¥" + toFixedJS(data.Amount, 2)
		tradeNo := "N/A"
		if data.TradeNo != nil && *data.TradeNo != "" {
			tradeNo = *data.TradeNo
		}

		infoItems := []emailInfoItem{
			{Label: "充值金额", Value: amountText},
			{Label: "订单号", Value: data.OrderNo},
			{Label: "交易号", Value: tradeNo},
			{Label: "到账时间", Value: timeText},
			{Label: "当前余额", Value: "¥" + toFixedJS(data.NewBalance, 2)},
		}
		alertMessage := "您的账户已成功充值 " + amountText

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        "充值成功通知",
			AlertType:    emailAlertSuccess,
			AlertTitle:   "充值已到账",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"您的充值已成功到账，以下是充值详情："},
			InfoTitle:    "充值详情",
			InfoItems:    infoItems,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        "充值成功通知",
			AlertTitle:   "充值已到账",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"您的充值已成功到账，以下是充值详情："},
			InfoTitle:    "充值详情",
			InfoItems:    infoItems,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "充值成功 "+amountText),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// InstanceCreatedEmailData is the payload for SendInstanceCreatedEmail.
type InstanceCreatedEmailData struct {
	Username     string
	InstanceName string
	HostName     string
	Image        string
	CPU          float64
	Memory       float64
	Disk         float64
	IPv4         string
	IPv6         string
	IsPaid       bool
	PlanName     string
	Amount       *float64
	ExpiresAt    *time.Time
}

// SendInstanceCreatedEmail mirrors sendInstanceCreatedEmail.
func SendInstanceCreatedEmail(ctx context.Context, email string, data InstanceCreatedEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "instance created", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName

		infoItems := []emailInfoItem{
			{Label: "实例名称", Value: data.InstanceName},
			{Label: "节点", Value: data.HostName},
			{Label: "镜像", Value: data.Image},
			{Label: "CPU", Value: jsNumberToString(data.CPU) + "%"},
			{Label: "内存", Value: formatMemory(data.Memory)},
			{Label: "磁盘", Value: formatDisk(data.Disk)},
		}
		if data.IPv4 != "" {
			infoItems = append(infoItems, emailInfoItem{Label: "IPv4", Value: data.IPv4})
		}
		if data.IPv6 != "" {
			infoItems = append(infoItems, emailInfoItem{Label: "IPv6", Value: data.IPv6})
		}
		if data.IsPaid && data.PlanName != "" {
			infoItems = append(infoItems, emailInfoItem{Label: "方案", Value: data.PlanName})
			if data.Amount != nil {
				infoItems = append(infoItems, emailInfoItem{Label: "扣费", Value: "¥" + toFixedJS(*data.Amount, 2)})
			}
			if data.ExpiresAt != nil {
				infoItems = append(infoItems, emailInfoItem{Label: "到期时间", Value: formatDate(*data.ExpiresAt)})
			}
		}

		alertMessage := "您的实例「" + data.InstanceName + "」已创建成功"
		if data.IsPaid {
			alertMessage = "您的付费实例「" + data.InstanceName + "」已创建成功"
		}

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        "实例创建成功",
			AlertType:    emailAlertSuccess,
			AlertTitle:   "实例已创建",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"您的实例已创建成功并正在启动，以下是实例详情："},
			InfoTitle:    "实例详情",
			InfoItems:    infoItems,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        "实例创建成功",
			AlertTitle:   "实例已创建",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"您的实例已创建成功并正在启动，以下是实例详情："},
			InfoTitle:    "实例详情",
			InfoItems:    infoItems,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "实例「"+data.InstanceName+"」创建成功"),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// RenewSuccessEmailData is the payload for SendRenewSuccessEmail.
type RenewSuccessEmailData struct {
	Username     string
	InstanceName string
	Amount       float64
	Months       int
	NewExpiresAt time.Time
}

// SendRenewSuccessEmail mirrors sendRenewSuccessEmail.
func SendRenewSuccessEmail(ctx context.Context, email string, data RenewSuccessEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "renew success", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName
		monthsText := strconv.Itoa(data.Months) + " 个月"
		alertMessage := "实例「" + data.InstanceName + "」已成功续费 " + monthsText

		infoItems := []emailInfoItem{
			{Label: "实例名称", Value: data.InstanceName},
			{Label: "续费时长", Value: monthsText},
			{Label: "续费金额", Value: "¥" + toFixedJS(data.Amount, 2)},
			{Label: "新到期时间", Value: formatDate(data.NewExpiresAt)},
		}

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        "续费成功通知",
			AlertType:    emailAlertSuccess,
			AlertTitle:   "续费成功",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"您的实例续费已成功，以下是续费详情："},
			InfoTitle:    "续费详情",
			InfoItems:    infoItems,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        "续费成功通知",
			AlertTitle:   "续费成功",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"您的实例续费已成功，以下是续费详情："},
			InfoTitle:    "续费详情",
			InfoItems:    infoItems,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "实例「"+data.InstanceName+"」续费成功"),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// BalanceAdjustedEmailData is the payload for SendBalanceAdjustedEmail.
type BalanceAdjustedEmailData struct {
	Username   string
	Amount     float64
	Remark     string
	NewBalance float64
	Time       time.Time
}

// SendBalanceAdjustedEmail mirrors sendBalanceAdjustedEmail.
func SendBalanceAdjustedEmail(ctx context.Context, email string, data BalanceAdjustedEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "balance adjusted", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName
		timeText := formatDateTime(data.Time)
		isIncrease := data.Amount > 0

		amountText := "-¥" + toFixedJS(math.Abs(data.Amount), 2)
		if isIncrease {
			amountText = "+¥" + toFixedJS(data.Amount, 2)
		}

		alertTitle := "余额扣除"
		verb := "扣除"
		if isIncrease {
			alertTitle = "余额增加"
			verb = "增加"
		}
		alertMessage := "管理员已" + verb + "您的账户余额 " + amountText

		infoItems := []emailInfoItem{
			{Label: "变动金额", Value: amountText},
			{Label: "变动原因", Value: data.Remark},
			{Label: "变动时间", Value: timeText},
			{Label: "当前余额", Value: "¥" + toFixedJS(data.NewBalance, 2)},
		}

		alertType := emailAlertWarning
		if isIncrease {
			alertType = emailAlertSuccess
		}

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        "余额变动通知",
			AlertType:    alertType,
			AlertTitle:   alertTitle,
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"您的账户余额已被管理员调整，以下是变动详情："},
			InfoTitle:    "变动详情",
			InfoItems:    infoItems,
			ActionTip:    "如对此次余额变动有疑问，请联系管理员。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        "余额变动通知",
			AlertTitle:   alertTitle,
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"您的账户余额已被管理员调整，以下是变动详情："},
			InfoTitle:    "变动详情",
			InfoItems:    infoItems,
			ActionTip:    "如对此次余额变动有疑问，请联系管理员。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "余额变动通知 "+amountText),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// FreeSiteRegisterGiftEmailData is the payload for SendFreeSiteRegisterGiftEmail.
type FreeSiteRegisterGiftEmailData struct {
	Username      string
	BalanceAmount float64
	PointsAmount  float64
	NewBalance    float64
	NewPoints     float64
	Time          time.Time
}

// SendFreeSiteRegisterGiftEmail mirrors sendFreeSiteRegisterGiftEmail.
func SendFreeSiteRegisterGiftEmail(ctx context.Context, email string, data FreeSiteRegisterGiftEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "free site register gift", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName
		timeText := formatDateTime(data.Time)

		giftItems := make([]string, 0, 2)
		if data.BalanceAmount > 0 {
			giftItems = append(giftItems, "余额 +¥"+toFixedJS(data.BalanceAmount, 2))
		}
		if data.PointsAmount > 0 {
			giftItems = append(giftItems, "积分 +"+jsNumberToString(data.PointsAmount))
		}
		giftSummary := "白嫖券一张"
		if len(giftItems) > 0 {
			giftSummary = strings.Join(giftItems, "，")
		}

		balanceValue := "本次没有余额小红包"
		if data.BalanceAmount > 0 {
			balanceValue = "+¥" + toFixedJS(data.BalanceAmount, 2)
		}
		pointsValue := "本次没有积分薯条"
		if data.PointsAmount > 0 {
			pointsValue = "+" + jsNumberToString(data.PointsAmount)
		}

		infoItems := []emailInfoItem{
			{Label: "到账余额", Value: balanceValue},
			{Label: "到账积分", Value: pointsValue},
			{Label: "当前余额", Value: "¥" + toFixedJS(data.NewBalance, 2)},
			{Label: "当前积分", Value: jsNumberToString(data.NewPoints)},
			{Label: "到账时间", Value: timeText},
		}
		alertMessage := giftSummary + " 已经咣当一声掉进您的账户。"
		paragraphs := []string{
			"欢迎加入白嫖站！系统刚刚给您的新手背包塞了点启动资金和快乐积分。",
			"不用充值，不用许愿，到账就是这么朴实无华。",
		}

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        "白嫖补给到账",
			AlertType:    emailAlertSuccess,
			AlertTitle:   "恭喜，白嫖小车已发车",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "到账详情",
			InfoItems:    infoItems,
			ActionTip:    "快去逛逛能兑换什么，白嫖虽好，也要记得优雅使用。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        "白嫖补给到账",
			AlertTitle:   "恭喜，白嫖小车已发车",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "到账详情",
			InfoItems:    infoItems,
			ActionTip:    "快去逛逛能兑换什么，白嫖虽好，也要记得优雅使用。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "白嫖补给到账 "+giftSummary),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// AdminInstanceCreatedEmailData is the payload for
// SendAdminInstanceCreatedEmail.
type AdminInstanceCreatedEmailData struct {
	Username     string
	InstanceName string
	PackageName  string
	PlanName     string
	HostName     string
	CPU          float64
	Memory       float64
	Disk         float64
	IsPaid       bool
	Charged      bool
	Amount       *float64
	ExpiresAt    *time.Time
	GiftDays     int
	CreatorType  string
	CreatorName  string
}

// SendAdminInstanceCreatedEmail mirrors sendAdminInstanceCreatedEmail.
func SendAdminInstanceCreatedEmail(ctx context.Context, email string, data AdminInstanceCreatedEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "admin instance created", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName

		infoItems := []emailInfoItem{
			{Label: "实例名称", Value: data.InstanceName},
			{Label: "套餐", Value: data.PackageName},
			{Label: "节点", Value: data.HostName},
			{Label: "CPU", Value: jsNumberToString(data.CPU) + "%"},
			{Label: "内存", Value: formatMemory(data.Memory)},
			{Label: "磁盘", Value: formatDisk(data.Disk)},
		}
		if data.IsPaid && data.PlanName != "" {
			infoItems = append(infoItems, emailInfoItem{Label: "方案", Value: data.PlanName})
			if data.Charged && data.Amount != nil {
				infoItems = append(infoItems, emailInfoItem{Label: "扣费", Value: "¥" + toFixedJS(*data.Amount, 2)})
			} else if !data.Charged {
				feeText := "首月免费赠送"
				if data.GiftDays > 0 {
					feeText = "免费赠送 " + strconv.Itoa(data.GiftDays) + " 天"
				}
				infoItems = append(infoItems, emailInfoItem{Label: "费用", Value: feeText})
			}
			if data.ExpiresAt != nil {
				infoItems = append(infoItems, emailInfoItem{Label: "到期时间", Value: formatDate(*data.ExpiresAt)})
			}
		}

		creatorLabel := "管理员"
		if data.CreatorType == "host_owner" {
			creatorLabel = "节点所有者"
		}
		creatorWithName := creatorLabel
		if data.CreatorName != "" {
			creatorWithName = creatorLabel + " " + data.CreatorName
		}

		var alertTitle, alertMessage string
		if data.IsPaid {
			alertTitle = "收到" + creatorLabel + "赠送的付费实例"
			if data.Charged {
				alertTitle = creatorLabel + "为您开通了付费实例"
			}
			switch {
			case data.Charged:
				alertMessage = "付费实例「" + data.InstanceName + "」已开通，已扣除费用 ¥" + toFixedJSPointer(data.Amount, 2)
			case data.GiftDays > 0:
				alertMessage = creatorLabel + "赠送的付费实例「" + data.InstanceName + "」已开通，免费时长 " + strconv.Itoa(data.GiftDays) + " 天"
			default:
				alertMessage = creatorLabel + "赠送的付费实例「" + data.InstanceName + "」已开通，首月免费"
			}
		} else {
			alertTitle = "收到" + creatorLabel + "赠送的实例"
			alertMessage = "实例「" + data.InstanceName + "」已创建成功"
		}

		paragraphs := []string{creatorWithName + "为您创建了一台新实例，以下是实例详情："}

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        alertTitle,
			AlertType:    emailAlertSuccess,
			AlertTitle:   alertTitle,
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "实例详情",
			InfoItems:    infoItems,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        alertTitle,
			AlertTitle:   alertTitle,
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "实例详情",
			InfoItems:    infoItems,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, alertTitle),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// toFixedJSPointer mirrors `amount?.toFixed(digits)`: nil renders as "undefined",
// exactly like the template literal in the TypeScript original would.
func toFixedJSPointer(value *float64, digits int) string {
	if value == nil {
		return "undefined"
	}
	return toFixedJS(*value, digits)
}

// BanNotificationEmailData is the payload for SendBanNotificationEmail.
type BanNotificationEmailData struct {
	Username string
	Reason   string
}

// SendBanNotificationEmail mirrors sendBanNotificationEmail.
func SendBanNotificationEmail(ctx context.Context, email string, data BanNotificationEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "ban notification", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName
		paragraphs := []string{
			"您的 " + brandName + " 账户已被管理员禁用，您将无法登录或使用该账户。",
			"",
			"封禁原因：" + data.Reason,
		}

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        "账户已被禁用",
			AlertType:    emailAlertDanger,
			AlertTitle:   "账户已被禁用",
			AlertMessage: "您的账户已被管理员禁用。",
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			ActionTip:    "如有疑问，请联系管理员。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        "账户已被禁用",
			AlertTitle:   "账户已被禁用",
			AlertMessage: "您的账户已被管理员禁用。",
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			ActionTip:    "如有疑问，请联系管理员。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "账户已被禁用"),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// HostAnnouncementEmailData is the payload for SendHostAnnouncementEmail.
type HostAnnouncementEmailData struct {
	Username string
	HostName string
	Title    string
	Content  string
}

// normalizeAnnouncementParagraphs mirrors
// `content.replace(/\r\n/g, '\n').trim().split('\n')`.
func normalizeAnnouncementParagraphs(content string) []string {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	return strings.Split(strings.TrimSpace(normalized), "\n")
}

// SendHostAnnouncementEmail mirrors sendHostAnnouncementEmail.
func SendHostAnnouncementEmail(ctx context.Context, email string, data HostAnnouncementEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "host announcement", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName
		paragraphs := normalizeAnnouncementParagraphs(data.Content)
		alertMessage := "您收到了来自节点「" + data.HostName + "」的一条通知。"

		infoItems := []emailInfoItem{
			{Label: "节点", Value: data.HostName},
			{Label: "标题", Value: data.Title},
		}

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        "宿主机通知",
			AlertType:    emailAlertInfo,
			AlertTitle:   data.Title,
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "通知信息",
			InfoItems:    infoItems,
			ActionTip:    "如需处理该通知，请登录面板查看相关实例。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        "宿主机通知",
			AlertTitle:   data.Title,
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "通知信息",
			InfoItems:    infoItems,
			ActionTip:    "如需处理该通知，请登录面板查看相关实例。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "节点通知："+data.Title),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// ExpiryReminderEmailData is the payload for SendExpiryReminderEmail.
type ExpiryReminderEmailData struct {
	Username      string
	InstanceName  string
	DaysRemaining int
	ExpiresAt     time.Time
}

// SendExpiryReminderEmail mirrors sendExpiryReminderEmail.
func SendExpiryReminderEmail(ctx context.Context, email string, data ExpiryReminderEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "expiry reminder", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName
		expiresAtText := formatDate(data.ExpiresAt)

		alertType := emailAlertWarning
		urgencyText := "您的实例将在 " + strconv.Itoa(data.DaysRemaining) + " 天后到期"
		if data.DaysRemaining <= 1 {
			alertType = emailAlertDanger
			urgencyText = "紧急！您的实例即将到期"
		}

		alertMessage := "实例「" + data.InstanceName + "」将于 " + expiresAtText + " 到期"
		paragraphs := []string{
			"为避免实例被封停，请及时续费。",
			"",
			"实例到期后将被封停，封停 3 天后将被永久删除，数据无法恢复。",
		}
		infoItems := []emailInfoItem{
			{Label: "实例名称", Value: data.InstanceName},
			{Label: "到期时间", Value: expiresAtText},
			{Label: "剩余天数", Value: strconv.Itoa(data.DaysRemaining) + " 天"},
		}
		actionTip := "请登录 " + brandName + " 控制面板进行续费操作。"

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        "实例到期提醒",
			AlertType:    alertType,
			AlertTitle:   urgencyText,
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "实例详情",
			InfoItems:    infoItems,
			ActionTip:    actionTip,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        "实例到期提醒",
			AlertTitle:   urgencyText,
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "实例详情",
			InfoItems:    infoItems,
			ActionTip:    actionTip,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "实例「"+data.InstanceName+"」将在 "+strconv.Itoa(data.DaysRemaining)+" 天后到期"),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// AutoRenewSuccessEmailData is the payload for SendAutoRenewSuccessEmail.
type AutoRenewSuccessEmailData struct {
	Username     string
	InstanceName string
	Amount       float64
	Months       int
	NewExpiresAt time.Time
	NewBalance   float64
}

// SendAutoRenewSuccessEmail mirrors sendAutoRenewSuccessEmail.
func SendAutoRenewSuccessEmail(ctx context.Context, email string, data AutoRenewSuccessEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "auto-renew success", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName
		monthsText := strconv.Itoa(data.Months) + " 个月"
		alertMessage := "实例「" + data.InstanceName + "」已自动续费 " + monthsText

		infoItems := []emailInfoItem{
			{Label: "实例名称", Value: data.InstanceName},
			{Label: "续费时长", Value: monthsText},
			{Label: "扣费金额", Value: "￥" + toFixedJS(data.Amount, 2)},
			{Label: "新到期时间", Value: formatDate(data.NewExpiresAt)},
			{Label: "剩余余额", Value: "￥" + toFixedJS(data.NewBalance, 2)},
		}

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        "自动续费成功",
			AlertType:    emailAlertSuccess,
			AlertTitle:   "自动续费成功",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"您的实例已自动续费成功，以下是续费详情："},
			InfoTitle:    "续费详情",
			InfoItems:    infoItems,
			FooterNote:   "此邮件为自动续费通知，如有疑问请登录控制面板查看。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        "自动续费成功",
			AlertTitle:   "自动续费成功",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   []string{"您的实例已自动续费成功，以下是续费详情："},
			InfoTitle:    "续费详情",
			InfoItems:    infoItems,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "自动续费成功：实例「"+data.InstanceName+"」"),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// AutoRenewFailedEmailData is the payload for SendAutoRenewFailedEmail.
type AutoRenewFailedEmailData struct {
	Username       string
	InstanceName   string
	FailReason     string
	CurrentAttempt int
	MaxAttempts    int
	ExpiresAt      time.Time
}

// SendAutoRenewFailedEmail mirrors sendAutoRenewFailedEmail.
func SendAutoRenewFailedEmail(ctx context.Context, email string, data AutoRenewFailedEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "auto-renew failed", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName
		expiresAtText := formatDate(data.ExpiresAt)
		remainingAttempts := data.MaxAttempts - data.CurrentAttempt
		isLastAttempt := remainingAttempts <= 0

		alertType := emailAlertWarning
		alertTitle := "自动续费失败（第 " + strconv.Itoa(data.CurrentAttempt) + " 次尝试）"
		attemptsInfo := "还剩 " + strconv.Itoa(remainingAttempts) + " 次尝试机会，系统将在稍后自动重试。建议您尽快充值或手动续费。"
		if isLastAttempt {
			alertType = emailAlertDanger
			alertTitle = "自动续费已失败（无剩余尝试次数）"
			attemptsInfo = "已达到最大尝试次数，自动续费已关闭。请手动续费以避免实例被封停。"
		}

		alertMessage := "实例「" + data.InstanceName + "」自动续费失败"
		paragraphs := []string{
			"您的实例自动续费失败，请查看详情：",
			"",
			attemptsInfo,
		}
		infoItems := []emailInfoItem{
			{Label: "实例名称", Value: data.InstanceName},
			{Label: "失败原因", Value: data.FailReason},
			{Label: "当前尝试", Value: "第 " + strconv.Itoa(data.CurrentAttempt) + " 次 / 共 " + strconv.Itoa(data.MaxAttempts) + " 次"},
			{Label: "到期时间", Value: expiresAtText},
		}

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        "自动续费失败",
			AlertType:    alertType,
			AlertTitle:   alertTitle,
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "失败详情",
			InfoItems:    infoItems,
			ActionTip:    "请尽快登录 " + brandName + " 控制面板进行手动续费，避免实例到期后被封停。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        "自动续费失败",
			AlertTitle:   alertTitle,
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "失败详情",
			InfoItems:    infoItems,
			ActionTip:    "请尽快登录 " + brandName + " 控制面板进行手动续费。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "自动续费失败：实例「"+data.InstanceName+"」"),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// RenewalPriceUpdatedEmailData is the payload for
// SendRenewalPriceUpdatedEmail.
type RenewalPriceUpdatedEmailData struct {
	Username     string
	InstanceName string
	HostName     string
	OldPrice     float64
	NewPrice     float64
}

// SendRenewalPriceUpdatedEmail mirrors sendRenewalPriceUpdatedEmail.
func SendRenewalPriceUpdatedEmail(ctx context.Context, email string, data RenewalPriceUpdatedEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "renewal price updated", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName
		alertMessage := "实例「" + data.InstanceName + "」的后续续费价格已更新"
		paragraphs := []string{"该价格将在您下一次续费时生效，当前计费周期不受影响。"}
		infoItems := []emailInfoItem{
			{Label: "实例名称", Value: data.InstanceName},
			{Label: "节点", Value: data.HostName},
			{Label: "原价格", Value: "¥" + toFixedJS(data.OldPrice, 2) + "/月"},
			{Label: "新价格", Value: "¥" + toFixedJS(data.NewPrice, 2) + "/月"},
		}
		actionTip := "如有疑问，请登录 " + brandName + " 控制面板查看实例计费信息。"

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        "实例续费价格调整通知",
			AlertType:    emailAlertInfo,
			AlertTitle:   "实例续费价格已调整",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "价格变更详情",
			InfoItems:    infoItems,
			ActionTip:    actionTip,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        "实例续费价格调整通知",
			AlertTitle:   "实例续费价格已调整",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "价格变更详情",
			InfoItems:    infoItems,
			ActionTip:    actionTip,
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, "实例续费价格已调整："+data.InstanceName),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// InstanceDestroyRefundEmailData is the payload for
// SendInstanceDestroyRefundEmail.
type InstanceDestroyRefundEmailData struct {
	Username      string
	InstanceName  string
	RefundAmount  float64
	FeeAmount     *float64
	RefundType    string
	Reason        string
	IsUserDestroy bool
}

// SendInstanceDestroyRefundEmail mirrors sendInstanceDestroyRefundEmail.
func SendInstanceDestroyRefundEmail(ctx context.Context, email string, data InstanceDestroyRefundEmailData) EmailSendResult {
	return sendBrandedEmail(ctx, "instance destroy refund", func(config SMTPConfig) (outgoingMail, error) {
		brandName := config.BrandName

		title := "实例删除退款通知"
		var alertMessage string
		paragraphs := []string{"管理员已删除您的实例，以下是退款处理结果。"}
		if data.IsUserDestroy {
			title = "实例销毁退款通知"
			alertMessage = "实例「" + data.InstanceName + "」已销毁，退款结果已生成"
			paragraphs = []string{"您的实例销毁操作已完成，以下是退款处理结果。"}
		} else {
			alertMessage = "实例「" + data.InstanceName + "」已被删除，退款结果已生成"
		}

		infoItems := []emailInfoItem{
			{Label: "实例名称", Value: data.InstanceName},
			{Label: "退款金额", Value: "¥" + toFixedJS(data.RefundAmount, 2)},
		}
		if data.FeeAmount != nil {
			infoItems = append(infoItems, emailInfoItem{Label: "手续费", Value: "¥" + toFixedJS(*data.FeeAmount, 2)})
		}
		if data.RefundType != "" {
			infoItems = append(infoItems, emailInfoItem{Label: "退款方式", Value: data.RefundType})
		}
		if data.Reason != "" {
			infoItems = append(infoItems, emailInfoItem{Label: "说明", Value: data.Reason})
		}

		htmlContent := generateEmailHTML(emailHTMLOptions{
			Title:        title,
			AlertType:    emailAlertSuccess,
			AlertTitle:   "退款已处理",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "退款详情",
			InfoItems:    infoItems,
			ActionTip:    "退款金额已发放至您的账户余额，请登录控制面板查看。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		textContent := generateEmailText(emailTextOptions{
			Title:        title,
			AlertTitle:   "退款已处理",
			AlertMessage: alertMessage,
			Greeting:     "您好，" + data.Username,
			Paragraphs:   paragraphs,
			InfoTitle:    "退款详情",
			InfoItems:    infoItems,
			ActionTip:    "退款金额已发放至您的账户余额，请登录控制面板查看。",
			BrandName:    brandName,
			BrandLogoURL: config.BrandLogoURL,
		})

		return outgoingMail{
			To:        email,
			Subject:   formatBrandSubject(brandName, title+"："+data.InstanceName),
			Text:      textContent,
			HTML:      htmlContent,
			FromName:  config.FromName,
			FromEmail: config.FromEmail,
		}, nil
	})
}

// ==================== operation-verification seam ====================

// init registers this module as the email sender for sensitive-operation
// verification. operation_verification.go declares the seam (a plain function
// variable) and falls back to an explicit SEND_FAILED while it is nil; assigning
// it here is enough, no app wiring is required.
func init() {
	OperationVerificationEmailSender = func(email string, data OperationVerificationEmailData) OperationVerificationSendResult {
		result := SendOperationVerificationEmail(context.Background(), email, data)
		return OperationVerificationSendResult{Success: result.Success, Error: result.Error}
	}
}
