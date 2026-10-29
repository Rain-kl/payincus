// Port of server/src/lib/log-sanitizer.ts — keeps sensitive values out of logs.
package lib

import (
	"regexp"
	"strings"
)

// Redaction placeholders, kept byte-identical to the Node implementation.
const (
	Redacted    = "[REDACTED]"
	RedactedJWT = "[REDACTED_JWT]"
)

var sensitiveFields = []string{
	"password",
	"adminPassword",
	"smtpPassword",
	"mailPassword",
	"newPassword",
	"oldPassword",
	"currentPassword",
	"confirmPassword",
	"token",
	"ticket",
	"sec-websocket-protocol",
	"assetToken",
	"agentSecret",
	"pat",
	"poa",
	"botToken",
	"bot_token",
	"telegramBotToken",
	"telegram_bot_token",
	"sign",
	"signature",
	"accessToken",
	"refreshToken",
	"access_token",
	"refresh_token",
	"id_token",
	"accesstoken",
	"refreshtoken",
	"authorization",
	"cookie",
	"secret",
	"jwtSecret",
	"apiKey",
	"apiSecret",
	"api_key",
	"api_secret",
	"key",
	"merchantPrivateKey",
	"merchant_private_key",
	"webhookSecret",
	"webhook_secret",
	"webhookUrl",
	"webhook_url",
	"discordWebhookUrl",
	"discord_webhook_url",
	"x-signature",
	"privateKey",
	"clientSecret",
	"private_key",
	"client_secret",
	"totpCode",
	"recoveryCode",
	"twoFactorSecret",
	"encryptedSecret",
	"backupCode",
	"totp_code",
	"recovery_code",
	"two_factor_secret",
	"encrypted_secret",
	"backup_code",
	"otp",
	"pin",
	"creditCard",
	"cardNumber",
	"cvv",
	"ssn",
	"x-auth-token",
	"x-api-key",
	"chatId",
	"chat_id",
}

var sensitiveFieldPattern = regexp.MustCompile(`(?i)^(` + strings.Join(sensitiveFields, "|") + `)$`)

type tokenPattern struct {
	re          *regexp.Regexp
	replacement string
}

var tokenPatterns = []tokenPattern{
	// JWT
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`), RedactedJWT},
	// PayIncus API token (pat_/poa_ + 43 chars)
	{regexp.MustCompile(`\b(?:pat|poa)_[A-Za-z0-9_-]{43}\b`), RedactedJWT},
	// Basic auth
	{regexp.MustCompile(`(?i)Basic\s+[A-Za-z0-9+/=]{8,}`), RedactedJWT},
	// Bearer token
	{regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/=:-]+`), RedactedJWT},
	// URL query secrets
	{
		regexp.MustCompile(`(?i)([?&](?:ticket|token|secret|key|sign|signature|access_token|refresh_token|api_key|apikey|apiKey|password|pwd|assetToken|agentSecret|code)=)[^&#\s]+`),
		"${1}" + Redacted,
	},
}

// IsSensitiveField reports whether a field name should be fully redacted.
func IsSensitiveField(fieldName string) bool {
	return sensitiveFieldPattern.MatchString(fieldName)
}

// SanitizeTokensInString strips credential-shaped substrings from free text.
func SanitizeTokensInString(value string) string {
	result := value
	for _, p := range tokenPatterns {
		result = p.re.ReplaceAllString(result, p.replacement)
	}
	return result
}

// SanitizeObject recursively redacts sensitive fields from a decoded JSON value.
func SanitizeObject(obj any) any {
	return sanitizeObjectDepth(obj, 0)
}

func sanitizeObjectDepth(obj any, depth int) any {
	if depth > 10 {
		return "[REDACTED_TRUNCATED]"
	}

	switch v := obj.(type) {
	case nil:
		return nil
	case string:
		return SanitizeTokensInString(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = sanitizeObjectDepth(item, depth+1)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			if IsSensitiveField(k) {
				out[k] = Redacted
				continue
			}
			out[k] = sanitizeObjectDepth(val, depth+1)
		}
		return out
	default:
		return obj
	}
}
