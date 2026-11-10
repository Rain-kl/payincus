// Port of server/src/routes/system-config.ts (mounted at /api/system-config).
//
// The module is the admin control plane for every runtime setting, so the
// following details are load-bearing and reproduced line by line:
//
//   - GET / returns db.getAllSystemConfigs(), which masks secrets
//     (smtp_password, turnstile_secret_key, ticket_image_lsky_token,
//     telegram_bot_token, telegram_webhook_secret) as "********" whenever the
//     stored value is non-empty. The masking itself lives in db/system_config.go
//     and is the single place that must stay in sync with the Node version; this
//     route adds the six "virtual" defaults the TypeScript injects for keys that
//     the admin UI edits but initSystemConfig never seeds.
//   - GET /public is unauthenticated and must never echo a secret. It only reads
//     non-sensitive keys, gates turnstileSiteKey on turnstileEnabled, and repeats
//     the createdAt/updatedAt sorting rule for the promo popup verbatim.
//   - PUT / accepts only a whitelisted key list, then applies per-key value
//     validation in the same order and with the same error codes/details as the
//     TypeScript. Values are normalised (trim, round, JSON re-serialise) before
//     they are written, exactly like the original.
//   - Writes go through db.UpdateSystemConfigs, which encrypts sensitive values,
//     skips the "********" placeholder (meaning "keep the stored secret"), runs
//     every update in one transaction and drops the whole config cache. Skipping
//     that cache invalidation would leave every other module reading stale values
//     for up to the cache TTL, so it is asserted by a unit test.
//   - The SMTP transporter cache is cleared when any smtp_* key is written, and
//     both writes and SMTP tests emit a log row plus (for the write) an admin
//     audit entry whose newValue redacts the secret keys.
//
// JSON shapes match the TypeScript byte for byte (snake_case for config rows,
// camelCase for computed fields, `null` for absent optional strings).
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// ==================== constants ====================

const (
	// systemConfigMaskedSecretPlaceholder mirrors MASKED_SECRET_PLACEHOLDER. A
	// sensitive value equal to this string means "keep the stored secret".
	systemConfigMaskedSecretPlaceholder = "********"

	// systemConfigTrafficOverageThrottleKey mirrors TRAFFIC_OVERAGE_THROTTLE_CONFIG_KEY.
	systemConfigTrafficOverageThrottleKey = "traffic_overage_throttle_speed"
	// systemConfigDefaultTrafficOverageThrottleSpeed mirrors
	// DEFAULT_TRAFFIC_OVERAGE_THROTTLE_SPEED.
	systemConfigDefaultTrafficOverageThrottleSpeed = "1Mbit"

	// systemConfigTicketAutoCloseEnabledKey mirrors TICKET_AUTO_CLOSE_ENABLED_CONFIG_KEY.
	systemConfigTicketAutoCloseEnabledKey = "ticket_auto_close_enabled"
	// systemConfigTicketAutoCloseHoursKey mirrors TICKET_AUTO_CLOSE_HOURS_CONFIG_KEY.
	systemConfigTicketAutoCloseHoursKey = "ticket_auto_close_hours"
	// systemConfigDefaultTicketAutoCloseHours mirrors DEFAULT_TICKET_AUTO_CLOSE_HOURS.
	systemConfigDefaultTicketAutoCloseHours = 24

	// systemConfigAffCommissionRateKey mirrors AFF_COMMISSION_RATE_CONFIG_KEY.
	systemConfigAffCommissionRateKey = "aff_commission_rate"
	// systemConfigAffDiscountRateKey mirrors AFF_DISCOUNT_RATE_CONFIG_KEY.
	systemConfigAffDiscountRateKey = "aff_discount_rate"
	// systemConfigDefaultAffRate mirrors DEFAULT_AFF_RATE.
	systemConfigDefaultAffRate = 0.05
	// systemConfigMaxAffCommissionRate mirrors MAX_AFF_COMMISSION_RATE.
	systemConfigMaxAffCommissionRate = 0.5
	// systemConfigMaxAffDiscountRate mirrors MAX_AFF_DISCOUNT_RATE.
	systemConfigMaxAffDiscountRate = 0.95

	// systemConfigMaxRegisterGiftBalance mirrors maxRegisterGiftBalance.
	systemConfigMaxRegisterGiftBalance = 99999999.99
	// systemConfigMaxRegisterGiftPoints mirrors maxRegisterGiftPoints.
	systemConfigMaxRegisterGiftPoints = 2147483647
	// systemConfigMaxTransferFee mirrors maxTransferFee.
	systemConfigMaxTransferFee = 100
	// systemConfigMaxSafeInteger mirrors Number.MAX_SAFE_INTEGER.
	systemConfigMaxSafeInteger = 9007199254740991

	// systemConfigDefaultAvatarAPIBase is the `avatarApiBase || '…'` fallback.
	systemConfigDefaultAvatarAPIBase = "https://api.dicebear.com/9.x"
	// systemConfigDefaultBrandName is the `brandName || 'Incudal'` fallback.
	systemConfigDefaultBrandName = "Incudal"
	// systemConfigDefaultBrandSubtitle is the `brandSubtitle || '…'` fallback.
	systemConfigDefaultBrandSubtitle = "基于 Incus 的低价 NAT VPS"
	// systemConfigDefaultBrandLogoURL is the fallback for a missing logo (and for the
	// retired /incudal_logo.webp).
	systemConfigDefaultBrandLogoURL = "/logo.svg"
	// systemConfigLegacyBrandLogoURL is replaced by the default logo.
	systemConfigLegacyBrandLogoURL = "/incudal_logo.webp"
	// systemConfigDefaultBrandCopyright is the `brandCopyright?.trim() || '…'` fallback.
	systemConfigDefaultBrandCopyright = "版权所有 © 2026， Arctel 和/或其关联公司。保留所有权利。"

	// systemConfigTransferFeeMessage is the custom details string the TypeScript
	// passes to apiError for transfer_fee.
	systemConfigTransferFeeMessage = "转移手续费必须在 0-100 元之间，最多支持两位小数"
	// systemConfigTelegramWebhookSecretMessage is the bespoke error body for
	// telegram_webhook_secret.
	systemConfigTelegramWebhookSecretMessage = "Telegram webhook secret can only contain letters, numbers, underscores, and hyphens"

	// systemConfigLogModule mirrors the literal 'system' log module.
	systemConfigLogModule = "system"
)

var (
	// systemConfigTelegramWebhookSecretPattern mirrors /^[A-Za-z0-9_-]{1,256}$/.
	systemConfigTelegramWebhookSecretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
	// systemConfigDecimalMoneyPattern mirrors /^\d+(?:\.\d{1,2})?$/.
	systemConfigDecimalMoneyPattern = regexp.MustCompile(`^\d+(?:\.\d{1,2})?$`)
	// systemConfigPositiveIntegerPattern mirrors /^[1-9]\d*$/.
	systemConfigPositiveIntegerPattern = regexp.MustCompile(`^[1-9]\d*$`)
	// systemConfigNonNegativeIntegerPattern mirrors /^(?:0|[1-9]\d*)$/.
	systemConfigNonNegativeIntegerPattern = regexp.MustCompile(`^(?:0|[1-9]\d*)$`)
	// systemConfigRatePattern mirrors /^(?:0|[1-9]\d*)(?:\.\d+)?$/.
	systemConfigRatePattern = regexp.MustCompile(`^(?:0|[1-9]\d*)(?:\.\d+)?$`)
	// systemConfigAdminIDCSVPattern mirrors /^(?:\s*[1-9]\d*\s*)(?:,\s*[1-9]\d*\s*)*$/.
	systemConfigAdminIDCSVPattern = regexp.MustCompile(`^(?:\s*[1-9]\d*\s*)(?:,\s*[1-9]\d*\s*)*$`)
	// systemConfigTrafficBandwidthPattern mirrors /^(\d+(?:\.\d+)?)\s*(?:Mbit|Gbit)$/i.
	systemConfigTrafficBandwidthPattern = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*(?:Mbit|Gbit)$`)
	// systemConfigEmailFormatPattern is ajv-formats' `email` in "full" mode, which is
	// what @fastify/ajv-compiler installs (it calls ajv-formats with no options, and
	// the default mode is "full"). The local usersEmailFormatRegex is the "fast"
	// spelling, which accepts things full mode rejects, so it is not reused here.
	systemConfigEmailFormatPattern = regexp.MustCompile(`(?i)^[a-z0-9!#$%&'*+/=?^_` + "`" + `{|}~-]+(?:\.[a-z0-9!#$%&'*+/=?^_` + "`" + `{|}~-]+)*@(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	// systemConfigJSExponentPattern matches the "e-07" / "e+05" form Go emits and
	// JavaScript does not: String(1e-7) is "1e-7", not "1e-07".
	systemConfigJSExponentPattern = regexp.MustCompile(`e([+-])0+(\d)`)

	// systemConfigTelegramGroupJoinModes mirrors telegramGroupJoinModes.
	systemConfigTelegramGroupJoinModes = map[string]bool{"any": true, "all": true}

	// systemConfigHighRiskAdminIDKeys mirrors HIGH_RISK_ADMIN_ID_KEYS: these may only
	// be written by the literal `admin` account, not by any other admin.
	systemConfigHighRiskAdminIDKeys = map[string]bool{
		"system_update_allowed_admin_ids": true,
		"payincus_gift_card_admin_ids":    true,
	}

	// systemConfigValidKeys mirrors the validKeys array of the PUT handler.
	systemConfigValidKeys = systemConfigKeySet(
		"default_quota_host",
		"default_quota_friend",
		"default_quota_package",
		"registration_enabled",
		"require_invite_code",
		"invite_generation_costs",
		"invite_default_expire_days",
		"ticket_enabled",
		systemConfigTicketAutoCloseEnabledKey,
		systemConfigTicketAutoCloseHoursKey,
		systemConfigAffCommissionRateKey,
		systemConfigAffDiscountRateKey,
		services.VipBenefitsConfigKey,
		"free_site_mode",
		"free_site_register_gift_enabled",
		"free_site_register_gift_balance",
		"free_site_register_gift_points",
		"brand_name",
		"brand_subtitle",
		"brand_logo_url",
		"brand_copyright",
		"popup_announcement",
		"popup_promo_image_url",
		"popup_promo_package_id",
		"turnstile_enabled",
		"turnstile_site_key",
		"turnstile_secret_key",
		"avatar_api_base",
		"footer_contact_email",
		"footer_telegram_link",
		// Telegram bot binding
		"telegram_bot_enabled",
		"telegram_bot_username",
		"telegram_bot_token",
		"telegram_webhook_secret",
		"telegram_group_join_enabled",
		"telegram_group_chat_id",
		"telegram_group_join_mode",
		"telegram_group_min_recharge",
		"telegram_group_min_consume",
		"telegram_group_invite_expire_minutes",
		"telegram_vip_group_join_enabled",
		"telegram_vip_group_chat_id",
		"telegram_vip_group_join_mode",
		"telegram_vip_group_min_recharge",
		"telegram_vip_group_min_consume",
		"telegram_vip_group_invite_expire_minutes",
		// SMTP configuration
		"smtp_enabled",
		"smtp_host",
		"smtp_port",
		"smtp_secure",
		"smtp_username",
		"smtp_password",
		"smtp_from_email",
		"smtp_from_name",
		// Email domain whitelist
		"email_domain_whitelist_enabled",
		"email_allowed_domains",
		// Transfer fee
		"transfer_fee",
		// Ticket image Lsky config
		"ticket_image_lsky_base_url",
		"ticket_image_lsky_token",
		"ticket_image_lsky_api_version",
		"ticket_image_lsky_target_id",
		// Platform operation config
		"system_update_allowed_admin_ids",
		"payincus_gift_card_admin_ids",
		systemConfigTrafficOverageThrottleKey,
	)

	// systemConfigBooleanKeys mirrors booleanKeys.
	systemConfigBooleanKeys = systemConfigKeySet(
		"registration_enabled",
		"require_invite_code",
		"ticket_enabled",
		systemConfigTicketAutoCloseEnabledKey,
		"free_site_mode",
		"free_site_register_gift_enabled",
		"turnstile_enabled",
		"telegram_bot_enabled",
		"telegram_group_join_enabled",
		"telegram_vip_group_join_enabled",
		"smtp_enabled",
		"smtp_secure",
		"email_domain_whitelist_enabled",
	)

	// systemConfigStringKeys mirrors stringKeys.
	systemConfigStringKeys = systemConfigKeySet(
		"turnstile_site_key",
		"turnstile_secret_key",
		"avatar_api_base",
		"footer_contact_email",
		"footer_telegram_link",
		"brand_name",
		"brand_subtitle",
		"brand_logo_url",
		"brand_copyright",
		"telegram_bot_username",
		"telegram_bot_token",
		"telegram_webhook_secret",
		"telegram_group_chat_id",
		"telegram_group_join_mode",
		"telegram_vip_group_chat_id",
		"telegram_vip_group_join_mode",
		"popup_announcement",
		"popup_promo_image_url",
		"popup_promo_package_id",
		"smtp_host",
		"smtp_username",
		"smtp_password",
		"smtp_from_email",
		"smtp_from_name",
		"email_allowed_domains",
		"ticket_image_lsky_base_url",
		"ticket_image_lsky_token",
		"ticket_image_lsky_api_version",
		"ticket_image_lsky_target_id",
		"system_update_allowed_admin_ids",
		"payincus_gift_card_admin_ids",
		systemConfigTrafficOverageThrottleKey,
	)

	// systemConfigJSONKeys mirrors jsonKeys.
	systemConfigJSONKeys = systemConfigKeySet(
		"invite_generation_costs",
		services.VipBenefitsConfigKey,
	)

	// systemConfigNumberKeys mirrors numberKeys (the default quota keys included).
	systemConfigNumberKeys = systemConfigKeySet(
		"smtp_port",
		"default_quota_host",
		"default_quota_friend",
		"default_quota_package",
		"free_site_register_gift_points",
		"invite_default_expire_days",
		"telegram_group_invite_expire_minutes",
		"telegram_vip_group_invite_expire_minutes",
		systemConfigTicketAutoCloseHoursKey,
	)

	// systemConfigDecimalKeys mirrors decimalKeys.
	systemConfigDecimalKeys = systemConfigKeySet(
		"free_site_register_gift_balance",
		"telegram_group_min_recharge",
		"telegram_group_min_consume",
		"telegram_vip_group_min_recharge",
		"telegram_vip_group_min_consume",
	)

	// systemConfigRateKeys mirrors rateKeys.
	systemConfigRateKeys = systemConfigKeySet(
		systemConfigAffCommissionRateKey,
		systemConfigAffDiscountRateKey,
	)

	// systemConfigSMTPKeys mirrors smtpConfigKeys: writing any of them clears the
	// cached nodemailer transporter.
	systemConfigSMTPKeys = systemConfigKeySet(
		"smtp_enabled",
		"smtp_host",
		"smtp_port",
		"smtp_secure",
		"smtp_username",
		"smtp_password",
		"smtp_from_email",
		"smtp_from_name",
	)

	// systemConfigAuditSensitiveKeys mirrors sensitiveKeys in the audit branch.
	systemConfigAuditSensitiveKeys = systemConfigKeySet(
		"turnstile_secret_key",
		"smtp_password",
		"ticket_image_lsky_token",
		"telegram_bot_token",
		"telegram_group_chat_id",
		"telegram_vip_group_chat_id",
		"telegram_webhook_secret",
	)
)

// systemConfigKeySet builds a membership set from a literal key list.
func systemConfigKeySet(keys ...string) map[string]bool {
	set := make(map[string]bool, len(keys))
	for _, key := range keys {
		set[key] = true
	}
	return set
}

// ==================== mail offering seam ====================

// SystemConfigAvailableMailOffering is the seam for db.hasAvailableMailOffering().
// server/src/db/mail.ts belongs to the mail module, which is ported separately;
// when db/mail.go lands it can assign this variable from its init(). The stock
// implementation runs the same query (an enabled plan whose source is enabled).
var SystemConfigAvailableMailOffering = systemConfigCountAvailableMailOffering

// systemConfigCountAvailableMailOffering mirrors the mailPlan.count query.
func systemConfigCountAvailableMailOffering(ctx context.Context) (bool, error) {
	if db.DB == nil {
		return false, errors.New("system config: database not initialized")
	}
	var count int64
	err := db.DB.WithContext(ctx).
		Model(&models.MailPlan{}).
		Joins("JOIN mail_sources ON mail_sources.id = mail_plans.source_id").
		Where("mail_plans.enabled = ? AND mail_sources.enabled = ?", true, true).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ==================== pure helpers ====================

// systemConfigGetEmailDomainForAudit mirrors getEmailDomainForAudit: only the
// domain part of the address is logged, stripped to [a-z0-9.-] and capped at 120.
func systemConfigGetEmailDomainForAudit(email string) string {
	domain := "unknown"
	if at := strings.LastIndex(email, "@"); at >= 0 {
		domain = strings.ToLower(strings.TrimSpace(email[at+1:]))
	}
	var builder strings.Builder
	for _, char := range domain {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '.', char == '-':
			builder.WriteRune(char)
		}
	}
	sanitized := builder.String()
	if runes := []rune(sanitized); len(runes) > 120 {
		sanitized = string(runes[:120])
	}
	if sanitized == "" {
		return "unknown"
	}
	return sanitized
}

// systemConfigIsHTTPImageURL mirrors isHttpImageUrl: `new URL(value)` must succeed
// and its protocol must be http: or https:.
//
// The WHATWG parser is more forgiving than RFC 3986, so this replica follows its
// special-scheme rules rather than net/url: surrounding whitespace is ignored, the
// scheme match is case-insensitive, any number of slashes (including none) may
// follow it, backslashes are treated as slashes, userinfo is allowed, and the host
// must be non-empty. A literal port must be numeric and in range. Verified against
// the live Node runtime for 23 inputs (http:///path, https:example.com,
// http://example.com:abc, http://?x, …).
func systemConfigIsHTTPImageURL(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}

	colon := strings.IndexByte(trimmed, ':')
	if colon <= 0 {
		return false
	}
	scheme := strings.ToLower(trimmed[:colon])
	if scheme != "http" && scheme != "https" {
		return false
	}
	for _, char := range trimmed[:colon] {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9',
			char == '+', char == '-', char == '.':
		default:
			return false
		}
	}

	// Special schemes treat any run of '/' or '\' after the scheme as the authority
	// separator, so "https:example.com" and "http:///path" both parse.
	rest := strings.TrimLeft(trimmed[colon+1:], `/\`)
	if rest == "" {
		return false
	}

	authority := rest
	if index := strings.IndexAny(authority, `/\?#`); index >= 0 {
		authority = authority[:index]
	}
	for _, char := range authority {
		if char <= 0x20 || char == 0x7f {
			return false
		}
	}
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	if authority == "" {
		return false
	}

	host := authority
	port := ""
	if strings.HasPrefix(authority, "[") {
		// IPv6 literal: the bracketed part is the host.
		end := strings.Index(authority, "]")
		if end < 0 {
			return false
		}
		host = authority[:end+1]
		if remainder := authority[end+1:]; strings.HasPrefix(remainder, ":") {
			port = remainder[1:]
		} else if remainder != "" {
			return false
		}
	} else if index := strings.IndexByte(authority, ':'); index >= 0 {
		host = authority[:index]
		port = authority[index+1:]
	}
	if host == "" {
		return false
	}
	if port != "" {
		for _, char := range port {
			if char < '0' || char > '9' {
				return false
			}
		}
		number, err := strconv.ParseUint(port, 10, 32)
		if err != nil || number > 65535 {
			return false
		}
	}
	return true
}

// systemConfigParseSafeInteger mirrors `Number.isSafeInteger(Number(text))` for a
// pattern-validated decimal digit string, bounded by max.
func systemConfigParseSafeInteger(text string, max int64) (int64, bool) {
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil || parsed > systemConfigMaxSafeInteger || parsed > max {
		return 0, false
	}
	return parsed, true
}

// systemConfigParsePositiveConfigInteger mirrors parsePositiveConfigInteger.
func systemConfigParsePositiveConfigInteger(value string, max int64) (int64, bool) {
	trimmed := strings.TrimSpace(value)
	if !systemConfigPositiveIntegerPattern.MatchString(trimmed) {
		return 0, false
	}
	return systemConfigParseSafeInteger(trimmed, max)
}

// systemConfigParseNonNegativeConfigInteger mirrors parseNonNegativeConfigInteger.
func systemConfigParseNonNegativeConfigInteger(value string, max int64) (int64, bool) {
	trimmed := strings.TrimSpace(value)
	if !systemConfigNonNegativeIntegerPattern.MatchString(trimmed) {
		return 0, false
	}
	return systemConfigParseSafeInteger(trimmed, max)
}

// systemConfigParseDecimalMoneyConfig mirrors parseDecimalMoneyConfig.
func systemConfigParseDecimalMoneyConfig(value string, max float64) (float64, bool) {
	trimmed := strings.TrimSpace(value)
	if !systemConfigDecimalMoneyPattern.MatchString(trimmed) {
		return 0, false
	}
	parsed, ok := ajvToJSNumber(trimmed)
	if !ok || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
		return 0, false
	}
	if parsed < 0 || parsed > max {
		return 0, false
	}
	return parsed, true
}

// systemConfigParseRateConfig mirrors parseRateConfig.
func systemConfigParseRateConfig(value string, max float64) (float64, bool) {
	trimmed := strings.TrimSpace(value)
	if !systemConfigRatePattern.MatchString(trimmed) {
		return 0, false
	}
	parsed, ok := ajvToJSNumber(trimmed)
	if !ok || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
		return 0, false
	}
	if parsed < 0 || parsed > max {
		return 0, false
	}
	return parsed, true
}

// systemConfigNormalizeAdminIDCSV mirrors normalizeAdminIdCsv. ok=false is the
// TypeScript `null` return (invalid input).
func systemConfigNormalizeAdminIDCSV(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", true
	}
	if !systemConfigAdminIDCSVPattern.MatchString(trimmed) {
		return "", false
	}

	seen := make(map[int64]bool, 4)
	order := make([]int64, 0, 4)
	for _, item := range strings.Split(trimmed, ",") {
		id, ok := systemConfigParseSafeInteger(strings.TrimSpace(item), systemConfigMaxSafeInteger)
		if !ok || id <= 0 {
			return "", false
		}
		if !seen[id] {
			seen[id] = true
			order = append(order, id)
		}
	}

	parts := make([]string, 0, len(order))
	for _, id := range order {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ","), true
}

// systemConfigFormatJSNumber mirrors String(number) for the values this route
// writes back: plain decimal notation between 1e-6 and 1e21, JavaScript's
// exponent spelling outside that window.
func systemConfigFormatJSNumber(value float64) string {
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
		return systemConfigJSExponentPattern.ReplaceAllString(
			strconv.FormatFloat(value, 'e', -1, 64), "e$1$2")
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// systemConfigStringJS mirrors JavaScript's String(value) for the JSON scalar
// types the invite-cost parser can see. Arrays join their elements with "," and
// plain objects stringify to "[object Object]", exactly as in the original — the
// resource name is then rejected by the supported-resource check.
func systemConfigStringJS(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case float64:
		return systemConfigFormatJSNumber(typed)
	case bool:
		return strconv.FormatBool(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, systemConfigStringJS(item))
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// systemConfigRespondAPIError mirrors apiError(): the envelope is
// {error, code, details?} and details is only omitted when it is undefined. The
// shared lib helper drops an empty string too, so empty details (reachable with a
// coerced empty config key) are emitted here.
func systemConfigRespondAPIError(c *gin.Context, status int, code lib.ErrorCode, details string) {
	if details == "" {
		c.JSON(status, gin.H{"error": lib.ErrorMessages[code], "code": code, "details": ""})
		return
	}
	lib.RespondAPIError(c, status, code, details)
}

// ==================== response shapes ====================

// systemConfigPublicPromoPlan mirrors PublicPromoPackagePlan.
type systemConfigPublicPromoPlan struct {
	ID           int32   `json:"id"`
	Name         string  `json:"name"`
	Description  *string `json:"description"`
	CPU          int32   `json:"cpu"`
	Memory       int32   `json:"memory"`
	Disk         int32   `json:"disk"`
	TrafficLimit string  `json:"trafficLimit"`
	Price        float64 `json:"price"`
	BillingCycle int32   `json:"billingCycle"`
	IsSoldOut    bool    `json:"isSoldOut"`
}

// systemConfigPublicPromoPackage mirrors PublicPromoPackage.
type systemConfigPublicPromoPackage struct {
	ID          int32                         `json:"id"`
	Name        string                        `json:"name"`
	Description *string                       `json:"description"`
	Source      string                        `json:"source"`
	Plans       []systemConfigPublicPromoPlan `json:"plans"`
}

// systemConfigPublicResponse is the GET /public body. Field order follows the
// TypeScript object literal.
type systemConfigPublicResponse struct {
	RegistrationEnabled         bool                            `json:"registrationEnabled"`
	RequireInviteCode           bool                            `json:"requireInviteCode"`
	TicketEnabled               bool                            `json:"ticketEnabled"`
	FreeSiteMode                bool                            `json:"freeSiteMode"`
	MailAvailable               bool                            `json:"mailAvailable"`
	TurnstileEnabled            bool                            `json:"turnstileEnabled"`
	TurnstileSiteKey            *string                         `json:"turnstileSiteKey"`
	AvatarAPIBase               string                          `json:"avatarApiBase"`
	EmailVerificationEnabled    bool                            `json:"emailVerificationEnabled"`
	EmailDomainWhitelistEnabled bool                            `json:"emailDomainWhitelistEnabled"`
	AllowedEmailDomains         []string                        `json:"allowedEmailDomains"`
	TransferFee                 float64                         `json:"transferFee"`
	FooterContactEmail          *string                         `json:"footerContactEmail"`
	FooterTelegramLink          *string                         `json:"footerTelegramLink"`
	BrandName                   string                          `json:"brandName"`
	BrandSubtitle               string                          `json:"brandSubtitle"`
	BrandLogoURL                string                          `json:"brandLogoUrl"`
	BrandCopyright              string                          `json:"brandCopyright"`
	PopupAnnouncement           *string                         `json:"popupAnnouncement"`
	PopupAnnouncementUpdatedAt  *string                         `json:"popupAnnouncementUpdatedAt"`
	PopupPromoImageURL          *string                         `json:"popupPromoImageUrl"`
	PopupPromoPackage           *systemConfigPublicPromoPackage `json:"popupPromoPackage"`
	PopupPromoUpdatedAt         *string                         `json:"popupPromoUpdatedAt"`
}

// systemConfigListResponse is the GET / body.
type systemConfigListResponse struct {
	Configs []db.SystemConfigItem `json:"configs"`
}

// systemConfigSendTestResponse is the POST /smtp/send-test success body. The
// absolute fields mirror JSON.stringify dropping undefined provider values.
type systemConfigSendTestResponse struct {
	Success                bool   `json:"success"`
	Message                string `json:"message"`
	ProviderMessageID      string `json:"providerMessageId,omitempty"`
	AcceptedRecipientCount int    `json:"acceptedRecipientCount"`
	RejectedRecipientCount int    `json:"rejectedRecipientCount"`
	PendingRecipientCount  int    `json:"pendingRecipientCount"`
	ProviderResponse       string `json:"providerResponse,omitempty"`
}

// systemConfigSafeValue is the audit entry shape for one written config.
type systemConfigSafeValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// systemConfigUpdatePair is one {key, value} pair of the PUT body, kept in
// document order (duplicates included) so the log/audit counters match.
type systemConfigUpdatePair struct {
	Key   string
	Value string
}

// ==================== AJV replicas ====================

// systemConfigValidateUpdateBody applies the PUT schema exactly as AJV does with
// @fastify/ajv-compiler's options (coerceTypes:'array', useDefaults:true,
// removeAdditional:true, allErrors:false). It returns the first validation
// message in the form app.ts' setErrorHandler renders it ("" when valid).
//
// Measured behaviour worth remembering:
//
//	{"configs": null|5|"abc"}  -> one-element array -> "must be object" (/configs/0)
//	{"configs": {}}            -> objects are not coercible -> "must be array"
//	{"configs": [{}]}          -> required runs first, field is "configs/0"
//	                              -> "configs/0 is required"
//	{"configs": [{"key": 5}]}  -> required 'value' wins over the key type error
//	                              -> "configs/0 is required"
func systemConfigValidateUpdateBody(decoded any) ([]systemConfigUpdatePair, string) {
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, "must be object"
	}
	raw, present := object["configs"]
	if !present {
		return nil, "configs is required"
	}

	var items []any
	switch typed := raw.(type) {
	case []any:
		items = typed
	case nil, string, float64, bool:
		// coerceTypes:'array' wraps scalars (null included) into a one-element array.
		items = []any{typed}
	default:
		// A JSON object is not coercible to an array.
		return nil, "must be array"
	}

	pairs := make([]systemConfigUpdatePair, 0, len(items))
	for index, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			return nil, "must be object"
		}
		path := "configs/" + strconv.Itoa(index)
		// `required` is validated before the item's properties, in schema order.
		if _, present := record["key"]; !present {
			return nil, path + " is required"
		}
		if _, present := record["value"]; !present {
			return nil, path + " is required"
		}
		key, message := ajvCoerceString(record["key"])
		if message != "" {
			return nil, message
		}
		value, message := ajvCoerceString(record["value"])
		if message != "" {
			return nil, message
		}
		pairs = append(pairs, systemConfigUpdatePair{Key: key, Value: value})
	}
	return pairs, ""
}

// systemConfigValidateSendTestBody applies the POST /smtp/send-test schema
// (`to` required, type string, format email).
func systemConfigValidateSendTestBody(decoded any) (string, string) {
	object, ok := decoded.(map[string]any)
	if !ok {
		return "", "must be object"
	}
	raw, present := object["to"]
	if !present {
		return "", "to is required"
	}
	to, message := ajvCoerceString(raw)
	if message != "" {
		return "", message
	}
	if !systemConfigEmailFormatPattern.MatchString(to) {
		// app.ts maps `format: 'email'` to this message.
		return "", "Please enter a valid email address"
	}
	return to, ""
}

// ==================== GET /public ====================

// systemConfigPublicHandler mirrors `fastify.get('/public')`. It is intentionally
// unauthenticated: the SPA reads it before login.
func systemConfigPublicHandler(c *gin.Context) {
	ctx := c.Request.Context()

	registrationEnabled, err := db.IsRegistrationEnabled(ctx)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	requireInviteCode, err := db.IsInviteCodeRequired(ctx)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	ticketCreationEnabled, err := db.GetSystemConfigBoolean(ctx, "ticket_enabled", true)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	freeSiteMode, err := db.GetSystemConfigBoolean(ctx, "free_site_mode", false)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	mailAvailable, err := SystemConfigAvailableMailOffering(ctx)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	turnstileEnabled, err := db.GetSystemConfigBoolean(ctx, "turnstile_enabled", false)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	turnstileSiteKey, err := db.GetSystemConfig(ctx, "turnstile_site_key", true)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	avatarAPIBase, err := db.GetSystemConfig(ctx, "avatar_api_base", true)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	smtpEnabled, err := db.GetSystemConfigBoolean(ctx, "smtp_enabled", false)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	emailDomainWhitelistEnabled, err := db.GetSystemConfigBoolean(ctx, "email_domain_whitelist_enabled", false)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	emailAllowedDomains, err := db.GetSystemConfig(ctx, "email_allowed_domains", true)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	transferFee, err := db.GetSystemConfigFloat(ctx, "transfer_fee", 0)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	footerContactEmail, err := db.GetSystemConfig(ctx, "footer_contact_email", true)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	footerTelegramLink, err := db.GetSystemConfig(ctx, "footer_telegram_link", true)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	brandName, err := db.GetSystemConfig(ctx, "brand_name", true)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	brandSubtitle, err := db.GetSystemConfig(ctx, "brand_subtitle", true)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	brandLogoURL, err := db.GetSystemConfig(ctx, "brand_logo_url", true)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	brandCopyright, err := db.GetSystemConfig(ctx, "brand_copyright", true)
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	popupAnnouncementConfig, err := db.GetSystemConfigValueWithUpdatedAt(ctx, "popup_announcement")
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	popupPromoImageURLConfig, err := db.GetSystemConfigValueWithUpdatedAt(ctx, "popup_promo_image_url")
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	popupPromoPackageIDConfig, err := db.GetSystemConfigValueWithUpdatedAt(ctx, "popup_promo_package_id")
	if err != nil {
		systemConfigInternalError(c)
		return
	}

	// Allowed domains: only resolved when the whitelist is enabled; a missing value
	// falls back to the built-in default list.
	var allowedDomains []string
	if emailDomainWhitelistEnabled {
		if emailAllowedDomains != nil && *emailAllowedDomains != "" {
			allowedDomains = make([]string, 0, 8)
			for _, domain := range strings.Split(*emailAllowedDomains, ",") {
				trimmed := strings.ToLower(strings.TrimSpace(domain))
				if trimmed != "" {
					allowedDomains = append(allowedDomains, trimmed)
				}
			}
		} else {
			allowedDomains = append([]string(nil), usersDefaultAllowedEmailDomains...)
		}
	}

	popupAnnouncement := ""
	if popupAnnouncementConfig.Value != nil {
		popupAnnouncement = strings.TrimSpace(*popupAnnouncementConfig.Value)
	}
	popupPromoImageURL := ""
	if popupPromoImageURLConfig.Value != nil {
		popupPromoImageURL = strings.TrimSpace(*popupPromoImageURLConfig.Value)
	}
	popupPromoPackageID := ""
	if popupPromoPackageIDConfig.Value != nil {
		popupPromoPackageID = strings.TrimSpace(*popupPromoPackageIDConfig.Value)
	}

	var popupPromoPackage *systemConfigPublicPromoPackage
	if popupPromoImageURL != "" && systemConfigIsHTTPImageURL(popupPromoImageURL) && popupPromoPackageID != "" {
		if packageID, ok := systemConfigParsePositiveConfigInteger(popupPromoPackageID, 2147483647); ok {
			pkg, err := systemConfigFindPromoPackage(ctx, int32(packageID))
			if err != nil {
				systemConfigInternalError(c)
				return
			}
			popupPromoPackage = pkg
		}
	}

	response := systemConfigPublicResponse{
		RegistrationEnabled:         registrationEnabled,
		RequireInviteCode:           requireInviteCode,
		TicketEnabled:               ticketCreationEnabled,
		FreeSiteMode:                freeSiteMode,
		MailAvailable:               mailAvailable,
		TurnstileEnabled:            turnstileEnabled,
		AvatarAPIBase:               systemConfigDefaultAvatarAPIBase,
		EmailVerificationEnabled:    smtpEnabled,
		EmailDomainWhitelistEnabled: emailDomainWhitelistEnabled,
		AllowedEmailDomains:         allowedDomains,
		TransferFee:                 transferFee,
		FooterContactEmail:          footerContactEmail,
		FooterTelegramLink:          footerTelegramLink,
		BrandName:                   systemConfigDefaultBrandName,
		BrandSubtitle:               systemConfigDefaultBrandSubtitle,
		BrandLogoURL:                systemConfigDefaultBrandLogoURL,
		BrandCopyright:              systemConfigDefaultBrandCopyright,
	}

	// turnstileSiteKey: turnstileEnabled ? turnstileSiteKey : null
	if turnstileEnabled {
		response.TurnstileSiteKey = turnstileSiteKey
	}
	if avatarAPIBase != nil && *avatarAPIBase != "" {
		response.AvatarAPIBase = *avatarAPIBase
	}
	if brandName != nil && *brandName != "" {
		response.BrandName = *brandName
	}
	if brandSubtitle != nil && *brandSubtitle != "" {
		response.BrandSubtitle = *brandSubtitle
	}
	// (brandLogoUrl && brandLogoUrl !== '/incudal_logo.webp') ? brandLogoUrl : '/logo.svg'
	if brandLogoURL != nil && *brandLogoURL != "" && *brandLogoURL != systemConfigLegacyBrandLogoURL {
		response.BrandLogoURL = *brandLogoURL
	}
	if brandCopyright != nil {
		if trimmed := strings.TrimSpace(*brandCopyright); trimmed != "" {
			response.BrandCopyright = trimmed
		}
	}

	// popupAnnouncement: trim() only decides *whether* to publish; the raw value is
	// what the SPA receives.
	if popupAnnouncement != "" {
		response.PopupAnnouncement = popupAnnouncementConfig.Value
		response.PopupAnnouncementUpdatedAt = popupAnnouncementConfig.UpdatedAt
	}
	if popupPromoPackage != nil {
		response.PopupPromoImageURL = popupPromoImageURLConfig.Value
		response.PopupPromoPackage = popupPromoPackage
		// [imageUrl.updatedAt, packageId.updatedAt].filter(Boolean).sort().pop() || null
		updatedAt := []string{}
		for _, candidate := range []*string{popupPromoImageURLConfig.UpdatedAt, popupPromoPackageIDConfig.UpdatedAt} {
			if candidate != nil && *candidate != "" {
				updatedAt = append(updatedAt, *candidate)
			}
		}
		if len(updatedAt) > 0 {
			newest := updatedAt[0]
			for _, candidate := range updatedAt[1:] {
				if candidate > newest {
					newest = candidate
				}
			}
			response.PopupPromoUpdatedAt = &newest
		}
	}

	c.JSON(http.StatusOK, response)
}

// systemConfigFindPromoPackage mirrors the promo popup's package.findFirst query:
// an active, globally shared package plus its three cheapest-ish active plans
// ordered by (isSoldOut asc, sortOrder asc, id asc).
func systemConfigFindPromoPackage(ctx context.Context, packageID int32) (*systemConfigPublicPromoPackage, error) {
	var pkg models.Package
	err := db.DB.WithContext(ctx).
		Where("id = ? AND active = ? AND global_shared = ?", packageID, true, true).
		First(&pkg).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var plans []models.PackagePlan
	err = db.DB.WithContext(ctx).
		Where("package_id = ? AND is_active = ?", pkg.ID, true).
		Order("is_sold_out ASC, sort_order ASC, id ASC").
		Limit(3).
		Find(&plans).Error
	if err != nil {
		return nil, err
	}

	packagePlans := make([]systemConfigPublicPromoPlan, 0, len(plans))
	for _, plan := range plans {
		price, _ := plan.Price.Decimal.Float64()
		packagePlans = append(packagePlans, systemConfigPublicPromoPlan{
			ID:           plan.ID,
			Name:         plan.Name,
			Description:  plan.Description,
			CPU:          plan.CPU,
			Memory:       plan.Memory,
			Disk:         plan.Disk,
			TrafficLimit: strconv.FormatInt(plan.TrafficLimit, 10),
			Price:        price,
			BillingCycle: plan.BillingCycle,
			IsSoldOut:    plan.IsSoldOut,
		})
	}

	return &systemConfigPublicPromoPackage{
		ID:          pkg.ID,
		Name:        pkg.Name,
		Description: pkg.Description,
		Source:      "official",
		Plans:       packagePlans,
	}, nil
}

// ==================== GET / ====================

// systemConfigVirtualDefault is one of the eight rows the TypeScript appends when
// GET / does not find the key in the table.
type systemConfigVirtualDefault struct {
	Key         string
	Value       string
	Type        string
	Label       string
	Description string
}

// systemConfigVirtualDefaults mirrors the six `if (!processedConfigs.some(…)) push`
// blocks of GET /. They are the settings the admin UI expects to exist even though
// initSystemConfig never seeds them.
func systemConfigVirtualDefaults() []systemConfigVirtualDefault {
	return []systemConfigVirtualDefault{
		{
			Key:         systemConfigTrafficOverageThrottleKey,
			Value:       systemConfigDefaultTrafficOverageThrottleSpeed,
			Type:        "string",
			Label:       "流量超量限速",
			Description: "实例流量超量后应用的独立带宽限制；套餐 trafficLimitSpeed 表示正常线速",
		},
		{
			Key:         systemConfigTicketAutoCloseEnabledKey,
			Value:       "true",
			Type:        "boolean",
			Label:       "工单自动关闭",
			Description: "自动关闭超过等待时限且最后公开消息来自处理方的已解决工单",
		},
		{
			Key:         systemConfigTicketAutoCloseHoursKey,
			Value:       strconv.Itoa(systemConfigDefaultTicketAutoCloseHours),
			Type:        "number",
			Label:       "工单自动关闭等待时间",
			Description: "从最后一条公开消息开始计算，单位为小时",
		},
		{
			Key:         systemConfigAffCommissionRateKey,
			Value:       systemConfigFormatJSNumber(systemConfigDefaultAffRate),
			Type:        "number",
			Label:       "AFF 佣金率",
			Description: "新生成 AFF 码的佣金比例，如 0.05 = 5%",
		},
		{
			Key:         systemConfigAffDiscountRateKey,
			Value:       systemConfigFormatJSNumber(systemConfigDefaultAffRate),
			Type:        "number",
			Label:       "AFF 折扣率",
			Description: "新生成 AFF 码的折扣比例，如 0.05 = 5%",
		},
		{
			Key:         services.VipBenefitsConfigKey,
			Value:       services.VipDefaultBenefitsConfigJSON,
			Type:        "json",
			Label:       "VIP 持续权益",
			Description: "按 VIP 等级配置新购/续费折扣、额外流量和资源池加成百分比",
		},
	}
}

// systemConfigListHandler mirrors `fastify.get('/')`: every row (secrets masked by
// the db layer) plus the virtual defaults, with email_allowed_domains filled from
// the built-in list so the admin sees what is actually in effect.
func systemConfigListHandler(c *gin.Context) {
	ctx := c.Request.Context()

	configs, err := db.GetAllSystemConfigs(ctx)
	if err != nil {
		systemConfigInternalError(c)
		return
	}

	processed := make([]db.SystemConfigItem, 0, len(configs)+6)
	for _, config := range configs {
		if config.Key == "email_allowed_domains" && config.Value == "" {
			config.Value = strings.Join(usersDefaultAllowedEmailDomains, ",")
		}
		processed = append(processed, config)
	}

	present := make(map[string]bool, len(processed))
	for _, config := range processed {
		present[config.Key] = true
	}
	for _, fallback := range systemConfigVirtualDefaults() {
		if present[fallback.Key] {
			continue
		}
		label := fallback.Label
		description := fallback.Description
		processed = append(processed, db.SystemConfigItem{
			ID:          0,
			Key:         fallback.Key,
			Value:       fallback.Value,
			Type:        fallback.Type,
			Label:       &label,
			Description: &description,
			CreatedAt:   "",
			UpdatedAt:   "",
		})
	}

	c.JSON(http.StatusOK, systemConfigListResponse{Configs: systemConfigMaskItems(processed)})
}

// ==================== GET /default-quota ====================

// systemConfigDefaultQuotaHandler mirrors `fastify.get('/default-quota')`.
func systemConfigDefaultQuotaHandler(c *gin.Context) {
	quota, err := db.GetDefaultQuotaConfig(c.Request.Context())
	if err != nil {
		systemConfigInternalError(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"quota": quota})
}

// ==================== PUT / ====================

// systemConfigUpdateHandler mirrors `fastify.put('/')`.
//
// Order of operations (all observable):
//  1. AJV validation of {configs:[{key,value}]}.
//  2. The high-risk key check runs BEFORE any key is validated, and answers 403
//     for every admin except the literal `admin` account.
//  3. Per-key validation/normalisation in document order; the first failure wins.
//  4. free_site_mode drives forced zeroing of the registration gift values.
//  5. Four keys get an upsert-if-missing first (so the UPDATE below cannot fail for
//     them), then every pair is written in one transaction.
//  6. SMTP transporter cache clear, log row, admin audit entry, {message}.
func systemConfigUpdateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	pairs, validationMessage := systemConfigValidateUpdateBody(decoded.Value)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	for _, pair := range pairs {
		if systemConfigHighRiskAdminIDKeys[pair.Key] && user.Username != "admin" {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}
	}

	// requestConfigMap: key -> last occurrence (a JS Map built from the array).
	byKey := make(map[string]int, len(pairs))
	for index := range pairs {
		byKey[pairs[index].Key] = index
	}

	if !systemConfigValidateUpdateValues(c, pairs) {
		return
	}

	// ---- free_site_mode -> registration gift defaults ----
	nextFreeSiteMode := false
	if index, present := byKey["free_site_mode"]; present {
		nextFreeSiteMode = pairs[index].Value == "true"
	} else {
		value, err := db.GetSystemConfigBoolean(ctx, "free_site_mode", false)
		if err != nil {
			systemConfigInternalError(c)
			return
		}
		nextFreeSiteMode = value
	}

	ensureConfigValue := func(key, value string) {
		if index, present := byKey[key]; present {
			pairs[index].Value = value
			return
		}
		pairs = append(pairs, systemConfigUpdatePair{Key: key, Value: value})
		byKey[key] = len(pairs) - 1
	}

	if !nextFreeSiteMode {
		ensureConfigValue("free_site_register_gift_enabled", "false")
		ensureConfigValue("free_site_register_gift_balance", "0")
		ensureConfigValue("free_site_register_gift_points", "0")
	} else {
		nextGiftEnabled := false
		if index, present := byKey["free_site_register_gift_enabled"]; present {
			nextGiftEnabled = pairs[index].Value == "true"
		} else {
			value, err := db.GetSystemConfigBoolean(ctx, "free_site_register_gift_enabled", false)
			if err != nil {
				systemConfigInternalError(c)
				return
			}
			nextGiftEnabled = value
		}
		if !nextGiftEnabled {
			ensureConfigValue("free_site_register_gift_balance", "0")
			ensureConfigValue("free_site_register_gift_points", "0")
		}
	}

	// ---- upsert-if-missing for the keys that were never seeded ----
	for _, fallback := range systemConfigVirtualDefaults() {
		if _, present := byKey[fallback.Key]; !present {
			continue
		}
		if fallback.Key == services.VipBenefitsConfigKey ||
			fallback.Key == systemConfigTicketAutoCloseEnabledKey ||
			fallback.Key == systemConfigTicketAutoCloseHoursKey ||
			fallback.Key == systemConfigTrafficOverageThrottleKey {
			if err := systemConfigUpsertConfigIfMissing(ctx, fallback); err != nil {
				systemConfigInternalError(c)
				return
			}
		}
	}

	updates := make([]db.SystemConfigUpdate, 0, len(pairs))
	for _, pair := range pairs {
		updates = append(updates, db.SystemConfigUpdate{Key: pair.Key, Value: pair.Value})
	}
	if err := systemConfigPersistUpdates(ctx, updates); err != nil {
		systemConfigInternalError(c)
		return
	}

	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, systemConfigLogModule, "system.config_update",
		"Updated "+strconv.Itoa(len(pairs))+" system configurations",
		db.LogResultSuccess, db.CreateLogOptions{})

	// AUTH003: the admin audit entry redacts secrets before they leave the process.
	safeConfigs := systemConfigAuditValues(pairs)
	lib.LogAdminAction(ctx, userID, "system.config_update", lib.AuditLogDetails{
		IP:           middleware.ClientIP(c),
		UserAgent:    c.GetHeader("User-Agent"),
		ResourceType: "system_config",
		NewValue:     safeConfigs,
		Metadata:     map[string]any{"configCount": len(pairs)},
	})

	c.JSON(http.StatusOK, gin.H{"message": "Config updated"})
}

// systemConfigValidateUpdateValues runs the per-key validation/normalisation loop.
// It mutates pairs in place and writes the error reply itself, returning false
// when the handler must stop.
func systemConfigValidateUpdateValues(c *gin.Context, pairs []systemConfigUpdatePair) bool {
	ctx := c.Request.Context()

	for index := range pairs {
		key := pairs[index].Key
		value := pairs[index].Value

		if !systemConfigValidKeys[key] {
			systemConfigRespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidKey, key)
			return false
		}

		// ---- boolean keys ----
		if systemConfigBooleanKeys[key] {
			if value != "true" && value != "false" {
				systemConfigRespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue, key)
				return false
			}
			continue
		}

		// ---- string keys (empty values allowed unless noted) ----
		if systemConfigStringKeys[key] {
			normalized, ok, normalizeErr := systemConfigNormalizeStringValue(ctx, key, value)
			if normalizeErr != nil {
				// A database failure inside the validation step propagates as a 500,
				// exactly like the uncaught Prisma rejection in the TypeScript.
				systemConfigInternalError(c)
				return false
			}
			if !ok {
				if key == "telegram_webhook_secret" {
					c.JSON(http.StatusBadRequest, gin.H{
						"error": systemConfigTelegramWebhookSecretMessage,
						"code":  "TELEGRAM_WEBHOOK_SECRET_INVALID",
					})
					return false
				}
				systemConfigRespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue, key)
				return false
			}
			pairs[index].Value = normalized
			continue
		}

		// ---- JSON keys ----
		if systemConfigJSONKeys[key] {
			normalized, ok := systemConfigNormalizeJSONValue(key, value)
			if !ok {
				systemConfigRespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue, key)
				return false
			}
			pairs[index].Value = normalized
			continue
		}

		// ---- decimal money keys ----
		if systemConfigDecimalKeys[key] {
			number, ok := systemConfigParseDecimalMoneyConfig(value, systemConfigMaxRegisterGiftBalance)
			if !ok {
				systemConfigRespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue, key)
				return false
			}
			pairs[index].Value = systemConfigFormatJSNumber(math.Round(number*100) / 100)
			continue
		}

		// ---- rate keys ----
		if systemConfigRateKeys[key] {
			max := systemConfigMaxAffDiscountRate
			if key == systemConfigAffCommissionRateKey {
				max = systemConfigMaxAffCommissionRate
			}
			rate, ok := systemConfigParseRateConfig(value, max)
			if !ok {
				systemConfigRespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue, key)
				return false
			}
			pairs[index].Value = systemConfigFormatJSNumber(rate)
			continue
		}

		// ---- transfer fee: 0-100 with at most two decimals ----
		if key == "transfer_fee" {
			trimmed := strings.TrimSpace(value)
			number, parsed := ajvToJSNumber(trimmed)
			finite := parsed && !math.IsInf(number, 0) && !math.IsNaN(number)
			if !systemConfigDecimalMoneyPattern.MatchString(trimmed) || !finite || number < 0 || number > systemConfigMaxTransferFee {
				systemConfigRespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue, systemConfigTransferFeeMessage)
				return false
			}
			pairs[index].Value = systemConfigFormatJSNumber(math.Round(number*100) / 100)
			continue
		}

		// ---- number keys (smtp_port, quotas, …) ----
		if systemConfigNumberKeys[key] {
			max := int64(systemConfigMaxSafeInteger)
			switch key {
			case "free_site_register_gift_points":
				max = systemConfigMaxRegisterGiftPoints
			case "smtp_port":
				max = 65535
			}
			number, ok := systemConfigParseNonNegativeConfigInteger(value, max)
			if !ok {
				systemConfigRespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue, key)
				return false
			}
			if key == systemConfigTicketAutoCloseHoursKey && number < 1 {
				systemConfigRespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue, key)
				return false
			}
			if (key == "telegram_group_invite_expire_minutes" || key == "telegram_vip_group_invite_expire_minutes") &&
				(number < 1 || number > 10080) {
				systemConfigRespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue, key)
				return false
			}
			if key == "invite_default_expire_days" && number > 3650 {
				systemConfigRespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue, key)
				return false
			}
			pairs[index].Value = strconv.FormatInt(number, 10)
			continue
		}

		// ---- default: non-negative integer (default quota keys) ----
		number, ok := systemConfigParseNonNegativeConfigInteger(value, systemConfigMaxSafeInteger)
		if !ok {
			systemConfigRespondAPIError(c, http.StatusBadRequest, lib.ErrConfigInvalidValue, key)
			return false
		}
		pairs[index].Value = strconv.FormatInt(number, 10)
	}
	return true
}

// systemConfigNormalizeStringValue applies the per-key string rules. ok=false
// means the value must be rejected with CONFIG_INVALID_VALUE (callers special-case
// telegram_webhook_secret's bespoke body); a non-nil error is a database failure
// and must surface as a 500, like the uncaught Prisma rejection in the TypeScript.
func systemConfigNormalizeStringValue(ctx context.Context, key, value string) (string, bool, error) {
	switch key {
	case systemConfigTrafficOverageThrottleKey:
		value = strings.TrimSpace(value)
		match := systemConfigTrafficBandwidthPattern.FindStringSubmatch(value)
		if match == nil {
			return "", false, nil
		}
		number, err := strconv.ParseFloat(match[1], 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return "", false, nil
		}
		if number <= 0 {
			return "", false, nil
		}
		return value, true, nil

	case "popup_announcement":
		// Length is measured on the raw value, before any trimming.
		if jsStringLength(value) > 5000 {
			return "", false, nil
		}
		return value, true, nil

	case "brand_name":
		value = strings.TrimSpace(value)
		if jsStringLength(value) > 200 {
			return "", false, nil
		}
		return value, true, nil

	case "brand_subtitle":
		value = strings.TrimSpace(value)
		if jsStringLength(value) > 300 {
			return "", false, nil
		}
		return value, true, nil

	case "brand_logo_url":
		value = strings.TrimSpace(value)
		if value != "" && jsStringLength(value) > 1000 {
			return "", false, nil
		}
		if value != "" && !systemConfigIsHTTPImageURL(value) && !strings.HasPrefix(value, "/") {
			return "", false, nil
		}
		return value, true, nil

	case "brand_copyright":
		value = strings.TrimSpace(value)
		if value != "" && jsStringLength(value) > 500 {
			return "", false, nil
		}
		return value, true, nil

	case "popup_promo_image_url":
		value = strings.TrimSpace(value)
		if value != "" && jsStringLength(value) > 1000 {
			return "", false, nil
		}
		if value != "" && !systemConfigIsHTTPImageURL(value) {
			return "", false, nil
		}
		return value, true, nil

	case "popup_promo_package_id":
		value = strings.TrimSpace(value)
		if value != "" {
			packageID, ok := systemConfigParsePositiveConfigInteger(value, 2147483647)
			if !ok {
				return "", false, nil
			}
			value = strconv.FormatInt(packageID, 10)
			var existing models.Package
			err := db.DB.WithContext(ctx).
				Select("id").
				Where("id = ? AND active = ? AND global_shared = ?", packageID, true, true).
				First(&existing).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "", false, nil
			}
			if err != nil {
				return "", false, err
			}
		}
		return value, true, nil

	case "ticket_image_lsky_api_version":
		if value != "v1" && value != "v2" {
			return "", false, nil
		}
		return value, true, nil

	case "ticket_image_lsky_base_url":
		value = strings.TrimSpace(value)
		if value != "" && jsStringLength(value) > 1000 {
			return "", false, nil
		}
		if value != "" {
			if _, err := lib.AssertSafeHTTPURL(ctx, value, "Lsky base URL"); err != nil {
				return "", false, nil
			}
		}
		return value, true, nil

	case "system_update_allowed_admin_ids", "payincus_gift_card_admin_ids":
		normalized, ok := systemConfigNormalizeAdminIDCSV(value)
		if !ok {
			return "", false, nil
		}
		return normalized, true, nil

	case "telegram_group_join_mode", "telegram_vip_group_join_mode":
		if !systemConfigTelegramGroupJoinModes[value] {
			return "", false, nil
		}
		return value, true, nil

	case "telegram_webhook_secret":
		if value != "" && value != systemConfigMaskedSecretPlaceholder &&
			!systemConfigTelegramWebhookSecretPattern.MatchString(value) {
			return "", false, nil
		}
		return value, true, nil
	}

	// Plain string key: stored as sent (no trimming), like the TypeScript.
	return value, true, nil
}

// systemConfigNormalizeJSONValue validates and re-serialises the two JSON keys.
func systemConfigNormalizeJSONValue(key, value string) (string, bool) {
	var parsed any
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		return "", false
	}

	switch key {
	case services.VipBenefitsConfigKey:
		normalized, err := services.VipNormalizeBenefitsConfig(parsed)
		if err != nil {
			return "", false
		}
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return "", false
		}
		return string(encoded), true

	case "invite_generation_costs":
		items, ok := parsed.([]any)
		if !ok {
			return "", false
		}
		options := make([]db.UserInvitesCostOption, 0, len(items))
		for _, item := range items {
			record, ok := item.(map[string]any)
			if !ok {
				return "", false
			}
			// `String(item.resource || '').trim()`
			rawResource := record["resource"]
			switch typed := rawResource.(type) {
			case nil:
				rawResource = ""
			case string:
			case float64:
				if typed == 0 {
					rawResource = ""
				}
			case bool:
				if !typed {
					rawResource = ""
				}
			}
			resource := strings.TrimSpace(systemConfigStringJS(rawResource))
			if !db.SystemConfigIsSupportedInviteCostResource(resource) {
				return "", false
			}
			amount, isNumber := record["amount"].(float64)
			if !isNumber || math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
				return "", false
			}
			if resource == "balance" && amount > systemConfigMaxRegisterGiftBalance {
				return "", false
			}
			if resource == "points" {
				if math.Trunc(amount) != amount || amount > systemConfigMaxRegisterGiftPoints {
					return "", false
				}
			}
			enabled, _ := record["enabled"].(bool)
			options = append(options, db.UserInvitesCostOption{
				Resource: resource,
				Amount:   amount,
				Enabled:  enabled,
			})
		}
		serialized, err := db.SystemConfigSerializeInviteCostOptions(options)
		if err != nil {
			return "", false
		}
		return serialized, true
	}

	return "", false
}

// systemConfigUpsertConfigIfMissing mirrors the `systemConfig.upsert({update: {}})`
// calls: create the row with its documented defaults when it does not exist, leave
// an existing row untouched.
func systemConfigUpsertConfigIfMissing(ctx context.Context, fallback systemConfigVirtualDefault) error {
	label := fallback.Label
	description := fallback.Description
	row := models.SystemConfig{
		Key:         fallback.Key,
		Value:       fallback.Value,
		Type:        fallback.Type,
		Label:       &label,
		Description: &description,
	}
	return db.DB.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoNothing: true}).
		Create(&row).Error
}

// ==================== POST /smtp/test ====================

// systemConfigSMTPTestHandler mirrors `fastify.post('/smtp/test')`.
func systemConfigSMTPTestHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	result := lib.TestSMTPConnection(ctx)
	userID := int64(user.ID)

	if result.Success {
		db.CreateLog(ctx, &userID, systemConfigLogModule, "smtp.test",
			"SMTP connection test successful", db.LogResultSuccess, db.CreateLogOptions{})
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "SMTP connection successful"})
		return
	}

	db.CreateLog(ctx, &userID, systemConfigLogModule, "smtp.test",
		"SMTP connection test failed: "+result.Error, db.LogResultFailed, db.CreateLogOptions{})
	c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": result.Error})
}

// ==================== POST /smtp/send-test ====================

// systemConfigSMTPSendTestHandler mirrors `fastify.post('/smtp/send-test')`. Only
// the recipient's domain is logged (and echoed) — never the address itself.
func systemConfigSMTPSendTestHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	to, validationMessage := systemConfigValidateSendTestBody(decoded.Value)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	userID := int64(user.ID)
	recipientDomain := systemConfigGetEmailDomainForAudit(to)
	result := lib.SendTestEmail(ctx, to)

	if result.Success {
		providerReference := ""
		if result.ProviderMessageID != "" {
			providerReference = " providerMessageId=" + result.ProviderMessageID
		}
		db.CreateLog(ctx, &userID, systemConfigLogModule, "smtp.send_test",
			"SMTP test email accepted for recipient domain "+recipientDomain+
				"; accepted="+strconv.Itoa(result.AcceptedRecipientCount)+
				"; rejected="+strconv.Itoa(result.RejectedRecipientCount)+
				"; pending="+strconv.Itoa(result.PendingRecipientCount)+
				";"+providerReference,
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, systemConfigSendTestResponse{
			Success:                true,
			Message:                "Test email accepted for recipient domain " + recipientDomain,
			ProviderMessageID:      result.ProviderMessageID,
			AcceptedRecipientCount: result.AcceptedRecipientCount,
			RejectedRecipientCount: result.RejectedRecipientCount,
			PendingRecipientCount:  result.PendingRecipientCount,
			ProviderResponse:       result.ProviderResponse,
		})
		return
	}

	db.CreateLog(ctx, &userID, systemConfigLogModule, "smtp.send_test",
		"Failed to send SMTP test email for recipient domain "+recipientDomain+": "+result.Error,
		db.LogResultFailed, db.CreateLogOptions{})
	c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": result.Error})
}

// ==================== helpers ====================

// systemConfigPersistUpdates is the write step of PUT /. It delegates to
// db.UpdateSystemConfigs, which encrypts sensitive values, skips the "********"
// placeholder and invalidates the whole process-local config cache (see
// db/system_config.go). A stale cache would make every other module read the old
// value for up to the cache TTL, so the invalidation contract is unit-tested
// through this function. The SMTP transporter cache is cleared here as well.
func systemConfigPersistUpdates(ctx context.Context, updates []db.SystemConfigUpdate) error {
	if err := db.UpdateSystemConfigs(ctx, updates); err != nil {
		return err
	}
	for _, update := range updates {
		if systemConfigSMTPKeys[update.Key] {
			lib.ClearTransporterCache()
			break
		}
	}
	return nil
}

// systemConfigAuditValues mirrors the safeConfigs mapping of PUT /: the secret
// keys are replaced by ***REDACTED*** before the audit entry is written.
func systemConfigAuditValues(pairs []systemConfigUpdatePair) []systemConfigSafeValue {
	values := make([]systemConfigSafeValue, 0, len(pairs))
	for _, pair := range pairs {
		value := pair.Value
		if systemConfigAuditSensitiveKeys[pair.Key] {
			value = "***REDACTED***"
		}
		values = append(values, systemConfigSafeValue{Key: pair.Key, Value: value})
	}
	return values
}

// systemConfigMaskItems applies the secret-masking rule to the rows GET / returns.
// db.GetAllSystemConfigs already masks and re-masking is idempotent, so this is a
// belt-and-braces pass that keeps the wire guarantee assertable by a unit test.
func systemConfigMaskItems(items []db.SystemConfigItem) []db.SystemConfigItem {
	masked := make([]db.SystemConfigItem, 0, len(items))
	for _, item := range items {
		item.Value = db.SystemConfigMaskValue(item.Key, item.Value)
		masked = append(masked, item)
	}
	return masked
}

// systemConfigInternalError mirrors the generic 500 branch of app.ts' error
// handler for non-leaky messages.
func systemConfigInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// ==================== registration ====================

// RegisterSystemConfig mounts /api/system-config. GET /public is anonymous; every
// other route carries `fastify.authenticateAdmin` (onRequest), which also means the
// request body is parsed before the handler and after authentication.
func RegisterSystemConfig(r *gin.Engine) {
	group := r.Group("/api/system-config")

	// GET /public (no auth, no rate-limit rule: the inline config.rateLimit of the
	// TypeScript route is dead code, see GO_MIGRATION.md).
	group.GET("/public", systemConfigPublicHandler)

	admin := group.Group("", middleware.AuthenticateAdmin())

	// GET / — Fastify registers both the bare and the trailing-slash spelling for a
	// plugin root route.
	admin.GET("", systemConfigListHandler)
	admin.GET("/", systemConfigListHandler)

	admin.GET("/default-quota", systemConfigDefaultQuotaHandler)

	admin.PUT("", systemConfigUpdateHandler)
	admin.PUT("/", systemConfigUpdateHandler)

	admin.POST("/smtp/test", systemConfigSMTPTestHandler)
	admin.POST("/smtp/send-test", systemConfigSMTPSendTestHandler)
}

func init() {
	Register(RegisterSystemConfig)
}
