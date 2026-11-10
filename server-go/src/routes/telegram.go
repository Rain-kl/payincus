// Port of server/src/routes/telegram.ts.
//
// Nine routes under the /api/telegram prefix (app.ts:
// `fastify.register(telegramRoutes, { prefix: '/api/telegram' })`):
//
//	GET    /admin/webhook/info      admin   Telegram getWebhookInfo passthrough
//	POST   /admin/webhook/setup     admin   setWebhook + setMyCommands
//	POST   /admin/webhook/delete    admin   deleteWebhook
//	GET    /admin/bindings          admin   paginated binding list + eligibility
//	DELETE /admin/bindings/:id      admin   unlink a binding
//	GET    /binding                 user    my binding
//	POST   /bind-token              user    mint a one-shot bind token
//	DELETE /binding                 user    unlink my binding
//	POST   /webhook                 PUBLIC  Telegram update receiver
//
// `POST /webhook` is the only unauthenticated route in the module: Telegram calls
// it, so it authenticates with the `X-Telegram-Bot-Api-Secret-Token` header
// (constant-time compared against the configured secret) instead of a JWT. When
// the bot is disabled/unconfigured the route answers `{"ok":true}` *before* any
// secret check, exactly like the TypeScript.
//
// Everything the bot sends back is best-effort: notifyTelegram swallows transport
// failures (they are logged without the token) so Telegram never retries an update
// because a reply failed.
//
// Bot tokens and webhook secrets are never logged and never returned; the only
// response that carries them is the 502 error text Telegram itself produced.
package routes

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// ==================== constants ====================

// ==================== constants ====================

// telegramBotAPIBase is the Telegram Bot API origin. It is not derived from user
// input, so the SSRF guard is only applied at connect time by
// lib.SafeOutboundHTTPClient. It is a var solely so tests can point it at an
// httptest server; production never reassigns it.
var telegramBotAPIBase = "https://api.telegram.org"

const (
	// telegramBindTokenPrefix mirrors `const bindTokenPrefix = 'bind_'`.
	telegramBindTokenPrefix = "bind_"
	// telegramBindTokenTTL mirrors `const bindTokenTtlMs = 10 * 60 * 1000`.
	telegramBindTokenTTL = 10 * time.Minute
	// telegramAPITimeout mirrors `const telegramApiTimeoutMs = 15_000`.
	telegramAPITimeout = 15 * time.Second
	// telegramWebhookBaseURLMaxLength mirrors `telegramWebhookBaseUrlMaxLength`.
	telegramWebhookBaseURLMaxLength = 2048
	// telegramWebhookPath is the fixed path appended to the base URL.
	telegramWebhookPath = "/api/telegram/webhook"

	// telegramBindingPageSizeDefault / Max mirror parsePositiveInteger's arguments.
	telegramPageFallback     = 1
	telegramPageMax          = 100000
	telegramPageSizeFallback = 5
	telegramPageSizeMax      = 100

	// telegramInviteExpireFallback mirrors parseInviteExpireMinutes' fallback.
	telegramInviteExpireFallback = 30
	// telegramInviteExpireMin / Max mirror the 1..10080 clamp.
	telegramInviteExpireMin = 1
	telegramInviteExpireMax = 10080

	telegramLogModule       = "system"
	telegramActionUnlink    = "telegram.binding_unlink"
	telegramResultSuccess   = "success"
	telegramGroupTargetNorm = "normal"
	telegramGroupTargetVIP  = "vip"
)

var (
	// telegramWebhookSecretPattern mirrors /^[A-Za-z0-9_-]{1,256}$/.
	telegramWebhookSecretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
	// telegramBindCommandPattern mirrors /^\/start\s+bind_([A-Za-z0-9_-]{20,128})$/.
	telegramBindCommandPattern = regexp.MustCompile(`^/start\s+bind_([A-Za-z0-9_-]{20,128})$`)
	// telegramStartJoinPattern mirrors /^\/start\s+(join|group|apply_group)$/.
	telegramStartJoinPattern = regexp.MustCompile(`^/start\s+(join|group|apply_group)$`)
	// telegramStartJoinVIPPattern mirrors /^\/start\s+join_vip$/.
	telegramStartJoinVIPPattern = regexp.MustCompile(`^/start\s+join_vip$`)
	// telegramCommandAtSuffixPattern mirrors /@\w+$/.
	telegramCommandAtSuffixPattern = regexp.MustCompile(`@[A-Za-z0-9_]+$`)
	// telegramWhitespacePattern mirrors /\s+/ at the start of normalizeTelegramCommand.
	telegramWhitespacePattern = regexp.MustCompile(`\s+`)
)

// telegramGroupCommands mirrors
// `new Set(telegramGroupCommandAliases.map(c => '/'+c))`.
var telegramGroupCommands = map[string]bool{
	"/join":        true,
	"/group":       true,
	"/apply_group": true,
}

// telegramVipGroupCommands mirrors `new Set(['/join_vip'])`.
var telegramVipGroupCommands = map[string]bool{"/join_vip": true}

// telegramBotCommand mirrors one entry of telegramBotCommands.
type telegramBotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// telegramBotCommands mirrors the const array (also echoed by webhook setup).
var telegramBotCommands = []telegramBotCommand{
	{Command: "start", Description: "绑定站内账号或查看使用说明"},
	{Command: "join", Description: "申请加入普通用户群"},
	{Command: "join_vip", Description: "申请加入高级用户群"},
}

// ==================== Telegram API client ====================

// telegramHTTPClient mirrors the two fetch calls: no redirect following
// (redirect: 'manual') and a 15s cap (AbortSignal.timeout). It reuses lib's
// connect-time-revalidating transport so the outbound guard from
// lib/outbound_security.go is in force even though the host is fixed.
var telegramHTTPClient = func() *http.Client {
	transport := http.RoundTripper(http.DefaultTransport)
	if base := lib.SafeOutboundHTTPClient(); base != nil && base.Transport != nil {
		transport = base.Transport
	}
	return &http.Client{
		Transport: transport,
		Timeout:   telegramAPITimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}()

// telegramAPIResponse mirrors `TelegramApiResponse<T>`.
type telegramAPIResponse struct {
	OK          bool
	Result      any
	HasResult   bool
	Description *string
	ErrorCode   *int64
	// TransportError carries a fetch-level failure. Node's callTelegramApi has no
	// try/catch around fetch, so such an error propagates out of the handler and
	// becomes a 500 — it must NOT be turned into a 502.
	TransportError error
}

// telegramCallAPI mirrors callTelegramApi: POST when a payload is given, GET
// otherwise, JSON in and JSON out, with a non-object body mapped to
// `{ok:false, description:"Telegram API returned HTTP <status>", error_code:<status>}`.
func telegramCallAPI(ctx context.Context, botToken, method string, payload map[string]any) telegramAPIResponse {
	target := telegramBotAPIBase + "/bot" + botToken + "/" + method

	var body io.Reader
	requestMethod := http.MethodGet
	if payload != nil {
		requestMethod = http.MethodPost
		encoded, err := json.Marshal(payload)
		if err != nil {
			return telegramAPIResponse{TransportError: err}
		}
		body = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, requestMethod, target, body)
	if err != nil {
		return telegramAPIResponse{TransportError: err}
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := telegramHTTPClient.Do(request)
	if err != nil {
		return telegramAPIResponse{TransportError: err}
	}
	defer response.Body.Close()

	raw, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		raw = nil
	}

	var decoded any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			decoded = nil
		}
	}

	// `if (!data || typeof data !== 'object')` — arrays pass that test in JS
	// (typeof [] === 'object'), so an array response is read as an object with
	// every field undefined rather than taking the HTTP-status branch.
	var object map[string]any
	switch typed := decoded.(type) {
	case map[string]any:
		object = typed
	case []any:
		object = map[string]any{}
	default:
		statusText := "Telegram API returned HTTP " + strconv.Itoa(response.StatusCode)
		code := int64(response.StatusCode)
		return telegramAPIResponse{OK: false, Description: &statusText, ErrorCode: &code}
	}

	result := telegramAPIResponse{}
	if flag, ok := object["ok"].(bool); ok {
		result.OK = flag
	}
	if value, present := object["result"]; present {
		result.Result = value
		result.HasResult = true
	}
	if description, ok := object["description"].(string); ok {
		result.Description = &description
	}
	if rawCode, ok := object["error_code"].(float64); ok {
		code := int64(rawCode)
		result.ErrorCode = &code
	}
	return result
}

// telegramSendMessage mirrors sendTelegramMessage: it reports whether Telegram
// answered 2xx.
func telegramSendMessage(ctx context.Context, botToken string, chatID float64, text string) bool {
	target := telegramBotAPIBase + "/bot" + botToken + "/sendMessage"
	encoded, err := json.Marshal(map[string]any{"chat_id": chatID, "text": text})
	if err != nil {
		return false
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		return false
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := telegramHTTPClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	// Drain so the connection can be reused, like undici does.
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode >= 200 && response.StatusCode < 300
}

// telegramSyncBotCommands mirrors syncTelegramBotCommands.
func telegramSyncBotCommands(ctx context.Context, botToken string) telegramAPIResponse {
	commands := make([]any, 0, len(telegramBotCommands))
	for _, command := range telegramBotCommands {
		commands = append(commands, map[string]any{
			"command":     command.Command,
			"description": command.Description,
		})
	}
	return telegramCallAPI(ctx, botToken, "setMyCommands", map[string]any{"commands": commands})
}

// telegramStringPtr is a small local helper (this module must not add shared
// identifiers to the flat routes package).
func telegramStringPtr(value string) *string { return &value }

// ==================== config model ====================

// telegramGroupConfig mirrors the TelegramGroupConfig interface.
type telegramGroupConfig struct {
	Enabled             bool
	Configured          bool
	ChatID              *string
	JoinMode            string
	MinRecharge         float64
	MinConsume          float64
	InviteExpireMinutes int
}

// telegramBotConfig mirrors the TelegramBotConfig interface.
type telegramBotConfig struct {
	Enabled       bool
	Configured    bool
	BotUsername   *string
	BotToken      *string
	WebhookSecret *string
	Group         telegramGroupConfig
	VIPGroup      telegramGroupConfig
}

// telegramLoadBotConfig mirrors getTelegramBotConfig. The TypeScript issues the 16
// reads in parallel with Promise.all; sequential reads here produce the same values.
func telegramLoadBotConfig(ctx context.Context) (telegramBotConfig, error) {
	enabled, err := db.GetSystemConfigBoolean(ctx, "telegram_bot_enabled", false)
	if err != nil {
		return telegramBotConfig{}, err
	}
	botUsernameValue, err := db.GetSystemConfig(ctx, "telegram_bot_username", true)
	if err != nil {
		return telegramBotConfig{}, err
	}
	botToken, err := db.GetSystemConfig(ctx, "telegram_bot_token", true)
	if err != nil {
		return telegramBotConfig{}, err
	}
	webhookSecret, err := db.GetSystemConfig(ctx, "telegram_webhook_secret", true)
	if err != nil {
		return telegramBotConfig{}, err
	}

	groupEnabled, err := db.GetSystemConfigBoolean(ctx, "telegram_group_join_enabled", false)
	if err != nil {
		return telegramBotConfig{}, err
	}
	groupChatID, err := db.GetSystemConfig(ctx, "telegram_group_chat_id", true)
	if err != nil {
		return telegramBotConfig{}, err
	}
	groupJoinMode, err := db.GetSystemConfig(ctx, "telegram_group_join_mode", true)
	if err != nil {
		return telegramBotConfig{}, err
	}
	groupMinRecharge, err := db.GetSystemConfig(ctx, "telegram_group_min_recharge", true)
	if err != nil {
		return telegramBotConfig{}, err
	}
	groupMinConsume, err := db.GetSystemConfig(ctx, "telegram_group_min_consume", true)
	if err != nil {
		return telegramBotConfig{}, err
	}
	groupInviteExpire, err := db.GetSystemConfig(ctx, "telegram_group_invite_expire_minutes", true)
	if err != nil {
		return telegramBotConfig{}, err
	}

	vipEnabled, err := db.GetSystemConfigBoolean(ctx, "telegram_vip_group_join_enabled", false)
	if err != nil {
		return telegramBotConfig{}, err
	}
	vipChatID, err := db.GetSystemConfig(ctx, "telegram_vip_group_chat_id", true)
	if err != nil {
		return telegramBotConfig{}, err
	}
	vipJoinMode, err := db.GetSystemConfig(ctx, "telegram_vip_group_join_mode", true)
	if err != nil {
		return telegramBotConfig{}, err
	}
	vipMinRecharge, err := db.GetSystemConfig(ctx, "telegram_vip_group_min_recharge", true)
	if err != nil {
		return telegramBotConfig{}, err
	}
	vipMinConsume, err := db.GetSystemConfig(ctx, "telegram_vip_group_min_consume", true)
	if err != nil {
		return telegramBotConfig{}, err
	}
	vipInviteExpire, err := db.GetSystemConfig(ctx, "telegram_vip_group_invite_expire_minutes", true)
	if err != nil {
		return telegramBotConfig{}, err
	}

	botUsername := telegramNormalizeBotUsername(botUsernameValue)
	// getSystemConfig returns "" for an empty stored value (it only maps a missing
	// row to null), but every consumer tests `!config.botToken`, i.e. truthiness.
	// Normalizing "" to nil keeps those checks identical.
	botTokenValue := telegramTruthyString(botToken)
	webhookSecretValue := telegramTrimmedOrNil(webhookSecret)

	config := telegramBotConfig{
		Enabled:       enabled,
		BotUsername:   botUsername,
		BotToken:      botTokenValue,
		WebhookSecret: webhookSecretValue,
		Group: telegramBuildGroupConfig(
			groupEnabled, groupChatID, groupJoinMode,
			groupMinRecharge, groupMinConsume, groupInviteExpire,
		),
		VIPGroup: telegramBuildGroupConfig(
			vipEnabled, vipChatID, vipJoinMode,
			vipMinRecharge, vipMinConsume, vipInviteExpire,
		),
	}
	config.Configured = enabled &&
		botUsername != nil &&
		botTokenValue != nil &&
		webhookSecretValue != nil &&
		telegramIsValidWebhookSecret(*webhookSecretValue)
	return config, nil
}

// telegramTruthyString mirrors JavaScript truthiness for a config string: an empty
// string behaves like null everywhere this module reads a credential.
func telegramTruthyString(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	return value
}

// telegramNormalizeBotUsername mirrors normalizeBotUsername:
// `value?.trim().replace(/^@/, ”) || null` — note the single leading '@' and the
// absence of a second trim.
func telegramNormalizeBotUsername(value *string) *string {
	if value == nil {
		return nil
	}
	username := strings.TrimPrefix(strings.TrimSpace(*value), "@")
	if username == "" {
		return nil
	}
	return &username
}

// telegramTrimmedOrNil mirrors `value?.trim() || null`.
func telegramTrimmedOrNil(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// telegramIsValidWebhookSecret mirrors isValidTelegramWebhookSecret.
func telegramIsValidWebhookSecret(value string) bool {
	return telegramWebhookSecretPattern.MatchString(value)
}

// telegramParseNonNegativeAmount mirrors parseNonNegativeAmount: a JS parseFloat,
// a non-finite/negative rejection, then Math.round(amount*100)/100.
func telegramParseNonNegativeAmount(value *string) float64 {
	raw := ""
	if value != nil {
		raw = *value
	}
	amount, ok := telegramParseFloatJS(raw)
	if !ok || math.IsInf(amount, 0) || math.IsNaN(amount) || amount < 0 {
		return 0
	}
	return math.Round(amount*100) / 100
}

// telegramParseInviteExpireMinutes mirrors parseInviteExpireMinutes.
func telegramParseInviteExpireMinutes(value *string) int {
	raw := ""
	if value != nil {
		raw = *value
	}
	minutes, ok := parseIntPrefix(raw)
	if !ok {
		return telegramInviteExpireFallback
	}
	if minutes < telegramInviteExpireMin {
		return telegramInviteExpireMin
	}
	if minutes > telegramInviteExpireMax {
		return telegramInviteExpireMax
	}
	return minutes
}

// telegramNormalizeGroupJoinMode mirrors normalizeGroupJoinMode.
func telegramNormalizeGroupJoinMode(value *string) string {
	if value != nil && *value == "all" {
		return "all"
	}
	return "any"
}

// telegramBuildGroupConfig mirrors buildTelegramGroupConfig.
func telegramBuildGroupConfig(
	enabled bool,
	chatIDValue *string,
	joinModeValue *string,
	minRechargeValue *string,
	minConsumeValue *string,
	inviteExpireMinutesValue *string,
) telegramGroupConfig {
	var chatID *string
	if chatIDValue != nil {
		trimmed := strings.TrimSpace(*chatIDValue)
		if trimmed != "" {
			chatID = &trimmed
		}
	}

	return telegramGroupConfig{
		Enabled:             enabled,
		Configured:          enabled && chatID != nil,
		ChatID:              chatID,
		JoinMode:            telegramNormalizeGroupJoinMode(joinModeValue),
		MinRecharge:         telegramParseNonNegativeAmount(minRechargeValue),
		MinConsume:          telegramParseNonNegativeAmount(minConsumeValue),
		InviteExpireMinutes: telegramParseInviteExpireMinutes(inviteExpireMinutesValue),
	}
}

// ==================== webhook URL ====================

// telegramNormalizeWebhookBaseURL mirrors normalizeWebhookBaseUrl: it returns
// `<protocol>//<host>` or nil. Production requires HTTPS.
func telegramNormalizeWebhookBaseURL(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || utf8.RuneCountInString(trimmed) > telegramWebhookBaseURLMaxLength {
		return nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil
	}
	protocol := strings.ToLower(parsed.Scheme)
	if protocol != "https" && protocol != "http" {
		return nil
	}
	if lib.IsProductionEnv() && protocol != "https" {
		return nil
	}

	host := strings.ToLower(parsed.Host)
	// JS URL drops the scheme's default port.
	if protocol == "https" && strings.HasSuffix(host, ":443") {
		host = strings.TrimSuffix(host, ":443")
	}
	if protocol == "http" && strings.HasSuffix(host, ":80") {
		host = strings.TrimSuffix(host, ":80")
	}

	normalized := protocol + "://" + host
	return &normalized
}

// telegramBuildWebhookURL mirrors buildWebhookUrl. It deliberately never looks at
// X-Forwarded-* headers (the bot webhook must not be pointable at an attacker host).
func telegramBuildWebhookURL(baseURL *string) *string {
	if baseURL != nil {
		if explicit := telegramNormalizeWebhookBaseURL(*baseURL); explicit != nil {
			full := *explicit + telegramWebhookPath
			return &full
		}
	}

	configuredBaseURL := strings.TrimSpace(telegramConfiguredSiteURL())
	if configuredBaseURL != "" {
		if normalized := telegramNormalizeWebhookBaseURL(configuredBaseURL); normalized != nil {
			full := *normalized + telegramWebhookPath
			return &full
		}
	}

	return nil
}

// telegramConfiguredSiteURL mirrors
// `(process.env.SITE_URL || process.env.FRONTEND_URL?.split(',')[0])?.trim()`.
// Note the JS truthiness check happens on the RAW value: a whitespace-only
// SITE_URL wins the `||` and then trims to "", so FRONTEND_URL is NOT consulted.
func telegramConfiguredSiteURL() string {
	if raw := os.Getenv("SITE_URL"); raw != "" {
		return strings.TrimSpace(raw)
	}
	raw := os.Getenv("FRONTEND_URL")
	if raw == "" {
		return ""
	}
	return strings.TrimSpace(strings.Split(raw, ",")[0])
}

// ==================== command parsing ====================

// telegramNormalizeTelegramCommand mirrors normalizeTelegramCommand:
// `text.trim().split(/\s+/)[0].replace(/@\w+$/, ”).toLowerCase()`.
func telegramNormalizeTelegramCommand(text string) string {
	trimmed := strings.TrimSpace(text)
	parts := telegramWhitespacePattern.Split(trimmed, -1)
	first := ""
	if len(parts) > 0 {
		first = parts[0]
	}
	// String#replace without /g removes a single trailing occurrence.
	if loc := telegramCommandAtSuffixPattern.FindStringIndex(first); loc != nil && loc[1] == len(first) {
		first = first[:loc[0]]
	}
	return strings.ToLower(first)
}

// telegramGetGroupJoinTarget mirrors getGroupJoinTarget.
func telegramGetGroupJoinTarget(text string) string {
	normalized := strings.ToLower(strings.TrimSpace(text))
	if telegramStartJoinPattern.MatchString(normalized) {
		return telegramGroupTargetNorm
	}
	if telegramStartJoinVIPPattern.MatchString(normalized) {
		return telegramGroupTargetVIP
	}

	command := telegramNormalizeTelegramCommand(text)
	if telegramGroupCommands[command] {
		return telegramGroupTargetNorm
	}
	if telegramVipGroupCommands[command] {
		return telegramGroupTargetVIP
	}
	return ""
}

// ==================== eligibility ====================

// telegramFormatMoney mirrors formatMoney (Number#toFixed(2)).
func telegramFormatMoney(value float64) string {
	return strconv.FormatFloat(value, 'f', 2, 64)
}

// telegramEvaluateGroupEligibility mirrors evaluateGroupEligibility.
func telegramEvaluateGroupEligibility(stats db.UserConsumeStats, group telegramGroupConfig) (bool, string) {
	rechargeRequired := group.MinRecharge > 0
	consumeRequired := group.MinConsume > 0

	if !rechargeRequired && !consumeRequired {
		return true, "当前未设置金额门槛。"
	}

	rechargePassed := !rechargeRequired || stats.TotalRecharge >= group.MinRecharge
	consumePassed := !consumeRequired || stats.TotalConsume >= group.MinConsume

	var eligible bool
	if group.JoinMode == "all" {
		eligible = rechargePassed && consumePassed
	} else {
		eligible = (rechargeRequired && rechargePassed) || (consumeRequired && consumePassed)
	}

	requirements := make([]string, 0, 2)
	if rechargeRequired {
		requirements = append(requirements, "累计充值 ¥"+telegramFormatMoney(group.MinRecharge)+
			"（当前 ¥"+telegramFormatMoney(stats.TotalRecharge)+"）")
	}
	if consumeRequired {
		requirements = append(requirements, "累计消费 ¥"+telegramFormatMoney(group.MinConsume)+
			"（当前 ¥"+telegramFormatMoney(stats.TotalConsume)+"）")
	}

	prefix := "满足任一即可"
	if group.JoinMode == "all" {
		prefix = "需同时满足"
	}
	return eligible, prefix + "：" + strings.Join(requirements, "；")
}

// telegramAdminEligibility mirrors the object returned by
// buildAdminGroupEligibility.
type telegramAdminEligibility struct {
	Eligible    bool    `json:"eligible"`
	Status      string  `json:"status"`
	Message     string  `json:"message"`
	JoinMode    string  `json:"joinMode"`
	MinRecharge float64 `json:"minRecharge"`
	MinConsume  float64 `json:"minConsume"`
}

// telegramBuildAdminGroupEligibility mirrors buildAdminGroupEligibility.
func telegramBuildAdminGroupEligibility(stats db.UserConsumeStats, group telegramGroupConfig, groupName string) telegramAdminEligibility {
	if !group.Enabled {
		return telegramAdminEligibility{
			Eligible:    false,
			Status:      "disabled",
			Message:     groupName + "入群申请未启用。",
			JoinMode:    group.JoinMode,
			MinRecharge: group.MinRecharge,
			MinConsume:  group.MinConsume,
		}
	}
	if !group.Configured {
		return telegramAdminEligibility{
			Eligible:    false,
			Status:      "unconfigured",
			Message:     groupName + " Chat ID 未配置。",
			JoinMode:    group.JoinMode,
			MinRecharge: group.MinRecharge,
			MinConsume:  group.MinConsume,
		}
	}

	eligible, message := telegramEvaluateGroupEligibility(stats, group)
	status := "ineligible"
	if eligible {
		status = "eligible"
	}
	return telegramAdminEligibility{
		Eligible:    eligible,
		Status:      status,
		Message:     message,
		JoinMode:    group.JoinMode,
		MinRecharge: group.MinRecharge,
		MinConsume:  group.MinConsume,
	}
}

// ==================== response shapes ====================

// telegramGroupConfigView mirrors serializeGroupConfig (the chat id is not exposed).
type telegramGroupConfigView struct {
	Enabled     bool    `json:"enabled"`
	Configured  bool    `json:"configured"`
	JoinMode    string  `json:"joinMode"`
	MinRecharge float64 `json:"minRecharge"`
	MinConsume  float64 `json:"minConsume"`
}

func telegramSerializeGroupConfig(group telegramGroupConfig) telegramGroupConfigView {
	return telegramGroupConfigView{
		Enabled:     group.Enabled,
		Configured:  group.Configured,
		JoinMode:    group.JoinMode,
		MinRecharge: group.MinRecharge,
		MinConsume:  group.MinConsume,
	}
}

// telegramAdminBindingView mirrors buildAdminBindingView.
type telegramAdminBindingView struct {
	ID               int32                    `json:"id"`
	UserID           int32                    `json:"userId"`
	User             *telegramUserView        `json:"user"`
	TelegramUserID   string                   `json:"telegramUserId"`
	TelegramUsername *string                  `json:"telegramUsername"`
	FirstName        *string                  `json:"firstName"`
	LastName         *string                  `json:"lastName"`
	BoundAt          string                   `json:"boundAt"`
	UpdatedAt        string                   `json:"updatedAt"`
	Stats            telegramBindingStatsView `json:"stats"`
	Eligibility      telegramAdminEligibility `json:"eligibility"`
	VIPEligibility   telegramAdminEligibility `json:"vipEligibility"`
}

// telegramUserView is the admin list's `user` projection.
type telegramUserView struct {
	ID       int32   `json:"id"`
	Username string  `json:"username"`
	Email    *string `json:"email"`
	Status   string  `json:"status"`
}

// telegramBindingStatsView mirrors the `stats` block.
type telegramBindingStatsView struct {
	TotalRecharge       float64 `json:"totalRecharge"`
	TotalConsume        float64 `json:"totalConsume"`
	TotalRefund         float64 `json:"totalRefund"`
	TotalDestroyedValue float64 `json:"totalDestroyedValue"`
}

// telegramISOTime mirrors Date#toISOString().
func telegramISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// telegramHashToken mirrors hashToken (sha256 hex).
func telegramHashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// telegramSecureEquals mirrors secureEquals: a length check followed by a
// constant-time comparison (Node returns false before timingSafeEqual when the
// byte lengths differ).
func telegramSecureEquals(a, b string) bool {
	aBytes := []byte(a)
	bBytes := []byte(b)
	if len(aBytes) != len(bBytes) {
		return false
	}
	return subtle.ConstantTimeCompare(aBytes, bBytes) == 1
}

// ==================== helpers ====================

// telegramRespondInternalError mirrors the dev-mode setErrorHandler body used by
// every 500 in this module.
func telegramRespondInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// telegramParsePositiveInteger mirrors parsePositiveInteger(value, fallback, max):
// a JS parseInt, an invalid/<1 fallback and a clamp to max.
func telegramParsePositiveInteger(raw string, fallback, max int) int {
	parsed, ok := parseIntPrefix(raw)
	if !ok || parsed < 1 {
		return fallback
	}
	if parsed > max {
		return max
	}
	return parsed
}

// telegramParseFloatJS mirrors Number.parseFloat: the longest numeric prefix wins
// and a non-numeric prefix yields NaN (ok=false).
func telegramParseFloatJS(value string) (float64, bool) {
	trimmed := strings.TrimLeft(value, " \t\n\r\v\f")
	if trimmed == "" {
		return 0, false
	}

	end := 0
	if trimmed[0] == '+' || trimmed[0] == '-' {
		end = 1
	}
	intStart := end
	for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
		end++
	}
	intDigits := end - intStart

	fracDigits := 0
	if end < len(trimmed) && trimmed[end] == '.' {
		end++
		fracStart := end
		for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
			end++
		}
		fracDigits = end - fracStart
	}
	if intDigits == 0 && fracDigits == 0 {
		return 0, false
	}

	if end < len(trimmed) && (trimmed[end] == 'e' || trimmed[end] == 'E') {
		exponentStart := end
		end++
		if end < len(trimmed) && (trimmed[end] == '+' || trimmed[end] == '-') {
			end++
		}
		exponentDigitsStart := end
		for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
			end++
		}
		if end == exponentDigitsStart {
			end = exponentStart
		}
	}

	parsed, err := strconv.ParseFloat(trimmed[:end], 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	// ErrRange means overflow (±Inf) or underflow (0), exactly what JS parseFloat
	// reports; the caller's isFinite guard decides what to do with it.
	return parsed, true
}

// telegramWebhookMessage is the subset of a Telegram update the route reads.
type telegramWebhookMessage struct {
	ChatID   float64
	ChatType *string
	From     map[string]any
	Text     *string
}

// telegramParseWebhookUpdate mirrors the `request.body?.message` destructuring.
func telegramParseWebhookUpdate(body any) (telegramWebhookMessage, bool) {
	root, ok := body.(map[string]any)
	if !ok {
		return telegramWebhookMessage{}, false
	}
	rawMessage, present := root["message"]
	if !present || rawMessage == nil {
		return telegramWebhookMessage{}, false
	}
	message, ok := rawMessage.(map[string]any)
	if !ok {
		return telegramWebhookMessage{}, false
	}

	var out telegramWebhookMessage

	if rawChat, present := message["chat"]; present && rawChat != nil {
		if chat, ok := rawChat.(map[string]any); ok {
			if id, ok := chat["id"].(float64); ok {
				out.ChatID = id
			}
			if chatType, ok := chat["type"].(string); ok {
				out.ChatType = &chatType
			}
		}
	}
	if rawFrom, present := message["from"]; present && rawFrom != nil {
		if from, ok := rawFrom.(map[string]any); ok {
			out.From = from
		}
	}
	if rawText, ok := message["text"].(string); ok {
		trimmed := strings.TrimSpace(rawText)
		out.Text = &trimmed
	}
	return out, true
}

// telegramJSString mirrors String(value) for the JSON scalars that can appear as a
// Telegram user id.
func telegramJSString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return formatTerminalCommandNumber(typed)
	case bool:
		return strconv.FormatBool(typed)
	case nil:
		return "undefined"
	default:
		return "[object Object]"
	}
}

// telegramNotify sends a chat message and swallows (but logs) failures, mirroring
// notifyTelegram. The bot token is never logged.
func telegramNotify(ctx context.Context, botToken string, chatID float64, text string) {
	if !telegramSendMessage(ctx, botToken, chatID, text) {
		log.Printf("Telegram message API returned non-OK response (chatId=%s)", formatTerminalCommandNumber(chatID))
	}
}

// telegramIsUserInGroup mirrors isTelegramUserInGroup: nil means "unknown", and a
// non-nil error is the fetch failure the TypeScript would let propagate (→ 500).
func telegramIsUserInGroup(ctx context.Context, botToken, groupChatID string, telegramUserID float64) (*bool, error) {
	response := telegramCallAPI(ctx, botToken, "getChatMember", map[string]any{
		"chat_id": groupChatID,
		"user_id": telegramUserID,
	})
	if response.TransportError != nil {
		return nil, response.TransportError
	}
	if !response.OK || !response.HasResult {
		return nil, nil
	}
	member, ok := response.Result.(map[string]any)
	if !ok {
		return nil, nil
	}
	status, _ := member["status"].(string)
	inGroup := status != "left" && status != "kicked"
	return &inGroup, nil
}

// telegramGroupInviteResult mirrors the object returned by createGroupInviteLink.
type telegramGroupInviteResult struct {
	InviteLink *string
	Error      *string
	ErrorCode  *int64
}

// telegramCreateGroupInviteLink mirrors createGroupInviteLink. A fetch failure is
// returned as an error because the TypeScript does not catch it (→ 500), unlike a
// Telegram-level `ok:false`, which is a normal 200 reply with a warning message.
func telegramCreateGroupInviteLink(ctx context.Context, botToken string, group telegramGroupConfig, inviteName string) (telegramGroupInviteResult, error) {
	if group.ChatID == nil {
		message := "Telegram group chat ID is not configured"
		return telegramGroupInviteResult{Error: &message}, nil
	}

	expireDate := time.Now().UTC().Unix() + int64(group.InviteExpireMinutes)*60
	response := telegramCallAPI(ctx, botToken, "createChatInviteLink", map[string]any{
		"chat_id":      *group.ChatID,
		"expire_date":  expireDate,
		"member_limit": 1,
		"name":         inviteName,
	})
	if response.TransportError != nil {
		return telegramGroupInviteResult{}, response.TransportError
	}

	failureText := func() string {
		if response.Description != nil && *response.Description != "" {
			return *response.Description
		}
		return "Telegram invite link creation failed"
	}

	if !response.OK || !response.HasResult {
		message := failureText()
		return telegramGroupInviteResult{Error: &message, ErrorCode: response.ErrorCode}, nil
	}

	result, ok := response.Result.(map[string]any)
	if !ok {
		message := failureText()
		return telegramGroupInviteResult{Error: &message, ErrorCode: response.ErrorCode}, nil
	}
	inviteLink, ok := result["invite_link"].(string)
	if !ok || inviteLink == "" {
		message := failureText()
		return telegramGroupInviteResult{Error: &message, ErrorCode: response.ErrorCode}, nil
	}

	return telegramGroupInviteResult{InviteLink: &inviteLink}, nil
}

// telegramHandleGroupJoinRequest mirrors handleGroupJoinRequest.
func telegramHandleGroupJoinRequest(
	ctx context.Context,
	config telegramBotConfig,
	chatID float64,
	from map[string]any,
	chatType *string,
	target string,
) error {
	if config.BotToken == nil {
		return nil
	}
	botToken := *config.BotToken

	group := config.Group
	groupName := "普通用户群"
	command := "/join"
	inviteName := "Incudal user access"
	if target == telegramGroupTargetVIP {
		group = config.VIPGroup
		groupName = "高级用户群"
		command = "/join_vip"
		inviteName = "Incudal VIP access"
	}

	if chatType != nil && *chatType != "private" {
		telegramNotify(ctx, botToken, chatID,
			"请私聊机器人发送 "+command+" 申请"+groupName+"，避免邀请链接泄露到群聊。")
		return nil
	}

	if !group.Enabled || !group.Configured {
		telegramNotify(ctx, botToken, chatID, groupName+"申请暂未开放，请稍后再试或联系管理员。")
		return nil
	}

	telegramUserID := telegramJSString(from["id"])
	binding, err := db.TelegramGetBindingByTelegramUserID(ctx, telegramUserID)
	if err != nil {
		return err
	}

	if binding == nil || binding.User == nil {
		telegramNotify(ctx, botToken, chatID,
			"请先登录网站，在个人设置里完成 Telegram 绑定后再发送 "+command+"。")
		return nil
	}
	if binding.User.Status != "active" {
		telegramNotify(ctx, botToken, chatID, "你的站内账号当前不可用，暂不能申请入群。")
		return nil
	}

	stats, err := db.GetUserConsumeStats(ctx, binding.User.ID)
	if err != nil {
		return err
	}
	eligible, message := telegramEvaluateGroupEligibility(stats, group)
	if !eligible {
		telegramNotify(ctx, botToken, chatID, "暂未达到"+groupName+"入群条件。\n"+message)
		return nil
	}

	if group.ChatID != nil {
		telegramUserNumber, _ := from["id"].(float64)
		alreadyInGroup, err := telegramIsUserInGroup(ctx, botToken, *group.ChatID, telegramUserNumber)
		if err != nil {
			return err
		}
		if alreadyInGroup != nil && *alreadyInGroup {
			telegramNotify(ctx, botToken, chatID, "你已经在"+groupName+"内，无需重复申请。")
			return nil
		}
	}

	inviteResult, err := telegramCreateGroupInviteLink(ctx, botToken, group, inviteName)
	if err != nil {
		return err
	}
	if inviteResult.InviteLink == nil {
		errorText := ""
		if inviteResult.Error != nil {
			errorText = *inviteResult.Error
		}
		log.Printf("Telegram group invite link creation failed (error=%s)", errorText)
		telegramNotify(ctx, botToken, chatID, "生成邀请链接失败，请联系管理员检查机器人群管理员权限。")
		return nil
	}

	telegramNotify(ctx, botToken, chatID,
		"已达标，可以加入"+groupName+"。\n"+message+
			"\n\n一次性邀请链接："+*inviteResult.InviteLink+
			"\n有效期："+strconv.Itoa(group.InviteExpireMinutes)+" 分钟。")
	return nil
}

// telegramBuildAdminBindingView mirrors buildAdminBindingView.
func telegramBuildAdminBindingView(ctx context.Context, binding db.TelegramBindingWithUser, group, vipGroup telegramGroupConfig) (telegramAdminBindingView, error) {
	stats, err := db.GetUserConsumeStats(ctx, binding.UserID)
	if err != nil {
		return telegramAdminBindingView{}, err
	}

	view := telegramAdminBindingView{
		ID:               binding.ID,
		UserID:           binding.UserID,
		TelegramUserID:   binding.TelegramUserID,
		TelegramUsername: binding.TelegramUsername,
		FirstName:        binding.FirstName,
		LastName:         binding.LastName,
		BoundAt:          telegramISOTime(binding.BoundAt),
		UpdatedAt:        telegramISOTime(binding.UpdatedAt),
		Stats: telegramBindingStatsView{
			TotalRecharge:       stats.TotalRecharge,
			TotalConsume:        stats.TotalConsume,
			TotalRefund:         stats.TotalRefund,
			TotalDestroyedValue: stats.TotalDestroyedValue,
		},
		Eligibility:    telegramBuildAdminGroupEligibility(stats, group, "普通群"),
		VIPEligibility: telegramBuildAdminGroupEligibility(stats, vipGroup, "高级群"),
	}
	if binding.User != nil {
		view.User = &telegramUserView{
			ID:       binding.User.ID,
			Username: binding.User.Username,
			Email:    binding.User.Email,
			Status:   binding.User.Status,
		}
	}
	return view, nil
}

// telegramBaseURLValidation mirrors the setup route's body schema:
// `{ baseUrl?: string, minLength: 1 }` with no additionalProperties:false.
func telegramValidateSetupBody(decoded any) (*string, string) {
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, "must be object"
	}
	raw, present := object["baseUrl"]
	if !present {
		return nil, ""
	}
	text, message := ajvCoerceString(raw)
	if message != "" {
		return nil, message
	}
	if utf8.RuneCountInString(text) < 1 {
		return nil, "baseUrl is too short"
	}
	return &text, ""
}

// telegramWriteAPIErrorBody writes the `{ error, errorCode? }` shape used by the
// 502 branches.
func telegramWriteAPIErrorBody(c *gin.Context, status int, fallback string, response telegramAPIResponse, extra gin.H) {
	message := fallback
	if response.Description != nil && *response.Description != "" {
		message = *response.Description
	}
	body := gin.H{"error": message}
	if response.ErrorCode != nil {
		body["errorCode"] = *response.ErrorCode
	}
	for key, value := range extra {
		body[key] = value
	}
	c.JSON(status, body)
}

// ==================== routes ====================

// RegisterTelegram mounts /api/telegram.
//
// NOTE: no route in this module is a plugin root, so Fastify's
// prefixTrailingSlash:'both' never applies — unlike notifications.ts there is no
// extra '/api/telegram' vs '/api/telegram/' spelling to mount.
func RegisterTelegram(r *gin.Engine) {
	group := r.Group("/api/telegram")

	admin := group.Group("/admin", middleware.AuthenticateAdmin())
	user := group.Group("", middleware.Authenticate())

	// GET /admin/webhook/info — passthrough of Telegram's getWebhookInfo.
	admin.GET("/webhook/info", func(c *gin.Context) {
		config, err := telegramLoadBotConfig(c.Request.Context())
		if err != nil {
			telegramRespondInternalError(c)
			return
		}
		if config.BotToken == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Telegram bot token is not configured"})
			return
		}

		response := telegramCallAPI(c.Request.Context(), *config.BotToken, "getWebhookInfo", nil)
		if response.TransportError != nil {
			// Node does not catch fetch failures here, so they surface as a 500.
			telegramRespondInternalError(c)
			return
		}
		if !response.OK {
			telegramWriteAPIErrorBody(c, http.StatusBadGateway, "Telegram webhook info request failed", response, nil)
			return
		}

		body := gin.H{}
		if response.HasResult {
			body["info"] = response.Result
		}
		c.JSON(http.StatusOK, body)
	})

	// POST /admin/webhook/setup — setWebhook (+ setMyCommands).
	admin.POST("/webhook/setup", func(c *gin.Context) {
		decoded, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}
		baseURL, validationMessage := telegramValidateSetupBody(decoded.Value)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		ctx := c.Request.Context()
		config, err := telegramLoadBotConfig(ctx)
		if err != nil {
			telegramRespondInternalError(c)
			return
		}
		if config.BotToken == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Telegram bot token is not configured"})
			return
		}
		if config.WebhookSecret == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Telegram webhook secret is not configured"})
			return
		}
		if !telegramIsValidWebhookSecret(*config.WebhookSecret) {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "Telegram webhook secret can only contain letters, numbers, underscores, and hyphens",
				"code":  "TELEGRAM_WEBHOOK_SECRET_INVALID",
			})
			return
		}

		if baseURL != nil && telegramNormalizeWebhookBaseURL(*baseURL) == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "baseUrl must be a valid public HTTP(S) URL; production requires HTTPS",
				"code":  "INVALID_TELEGRAM_WEBHOOK_BASE_URL",
			})
			return
		}

		webhookURL := telegramBuildWebhookURL(baseURL)
		if webhookURL == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "SITE_URL or FRONTEND_URL must be configured before Telegram webhook setup in production",
				"code":  "TELEGRAM_WEBHOOK_BASE_URL_REQUIRED",
			})
			return
		}

		response := telegramCallAPI(ctx, *config.BotToken, "setWebhook", map[string]any{
			"url":             *webhookURL,
			"secret_token":    *config.WebhookSecret,
			"allowed_updates": []any{"message"},
		})
		if response.TransportError != nil {
			telegramRespondInternalError(c)
			return
		}
		if !response.OK {
			telegramWriteAPIErrorBody(c, http.StatusBadGateway, "Telegram webhook setup failed", response,
				gin.H{"webhookUrl": *webhookURL})
			return
		}

		commandsResponse := telegramSyncBotCommands(ctx, *config.BotToken)
		if commandsResponse.TransportError != nil {
			telegramRespondInternalError(c)
			return
		}
		if !commandsResponse.OK {
			telegramWriteAPIErrorBody(c, http.StatusBadGateway, "Telegram bot command setup failed", commandsResponse,
				gin.H{"webhookUrl": *webhookURL, "webhookConfigured": true})
			return
		}

		body := gin.H{
			"message":        "Telegram webhook configured",
			"webhookUrl":     *webhookURL,
			"commandsSynced": true,
			"commands":       telegramBotCommands,
		}
		if response.HasResult {
			body["result"] = response.Result
		}
		c.JSON(http.StatusOK, body)
	})

	// POST /admin/webhook/delete — deleteWebhook.
	admin.POST("/webhook/delete", func(c *gin.Context) {
		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		ctx := c.Request.Context()
		config, err := telegramLoadBotConfig(ctx)
		if err != nil {
			telegramRespondInternalError(c)
			return
		}
		if config.BotToken == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Telegram bot token is not configured"})
			return
		}

		response := telegramCallAPI(ctx, *config.BotToken, "deleteWebhook", nil)
		if response.TransportError != nil {
			telegramRespondInternalError(c)
			return
		}
		if !response.OK {
			telegramWriteAPIErrorBody(c, http.StatusBadGateway, "Telegram webhook delete failed", response, nil)
			return
		}

		body := gin.H{"message": "Telegram webhook deleted"}
		if response.HasResult {
			body["result"] = response.Result
		}
		c.JSON(http.StatusOK, body)
	})

	// GET /admin/bindings — paginated binding list plus both group states.
	admin.GET("/bindings", func(c *gin.Context) {
		query := c.Request.URL.Query()
		page := telegramParsePositiveInteger(query.Get("page"), telegramPageFallback, telegramPageMax)
		pageSize := telegramParsePositiveInteger(query.Get("pageSize"), telegramPageSizeFallback, telegramPageSizeMax)

		var search *string
		if raw := query.Get("search"); raw != "" {
			trimmed := strings.TrimSpace(raw)
			search = &trimmed
		}

		ctx := c.Request.Context()
		config, err := telegramLoadBotConfig(ctx)
		if err != nil {
			telegramRespondInternalError(c)
			return
		}

		bindings, total, err := db.TelegramListBindings(ctx, db.TelegramBindingListOptions{
			Page:     page,
			PageSize: pageSize,
			Search:   search,
		})
		if err != nil {
			telegramRespondInternalError(c)
			return
		}

		views := make([]telegramAdminBindingView, 0, len(bindings))
		for _, binding := range bindings {
			view, err := telegramBuildAdminBindingView(ctx, binding, config.Group, config.VIPGroup)
			if err != nil {
				telegramRespondInternalError(c)
				return
			}
			views = append(views, view)
		}

		c.JSON(http.StatusOK, gin.H{
			"bindings": views,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
			"group":    telegramSerializeGroupConfig(config.Group),
			"vipGroup": telegramSerializeGroupConfig(config.VIPGroup),
		})
	})

	// DELETE /admin/bindings/:id — unlink a binding.
	admin.DELETE("/bindings/:id", func(c *gin.Context) {
		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		bindingID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid Telegram binding ID"})
			return
		}

		ctx := c.Request.Context()
		binding, err := db.TelegramGetBindingByID(ctx, int32(bindingID))
		if err != nil {
			telegramRespondInternalError(c)
			return
		}
		if binding == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Telegram binding not found"})
			return
		}

		if err := db.TelegramDeleteBindingByID(ctx, int32(bindingID)); err != nil {
			telegramRespondInternalError(c)
			return
		}

		username := "unknown"
		if binding.User != nil && binding.User.Username != "" {
			username = binding.User.Username
		}
		currentUser := middleware.CurrentUser(c)
		var actorID *int64
		if currentUser != nil {
			id := int64(currentUser.ID)
			actorID = &id
		}
		db.CreateLog(ctx, actorID, telegramLogModule, telegramActionUnlink,
			"Admin unlinked Telegram binding for user #"+strconv.FormatInt(int64(binding.UserID), 10)+
				" ("+username+")",
			telegramResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "Telegram binding removed"})
	})

	// GET /binding — the caller's own binding.
	user.GET("/binding", func(c *gin.Context) {
		ctx := c.Request.Context()
		currentUser := middleware.CurrentUser(c)

		config, err := telegramLoadBotConfig(ctx)
		if err != nil {
			telegramRespondInternalError(c)
			return
		}

		binding, err := db.TelegramGetBindingByUserID(ctx, currentUser.ID)
		if err != nil {
			telegramRespondInternalError(c)
			return
		}

		var bindingView any
		if binding != nil {
			bindingView = gin.H{
				"telegramUserId":   binding.TelegramUserID,
				"telegramUsername": binding.TelegramUsername,
				"firstName":        binding.FirstName,
				"lastName":         binding.LastName,
				"boundAt":          telegramISOTime(binding.BoundAt),
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"enabled":     config.Enabled,
			"configured":  config.Configured,
			"botUsername": config.BotUsername,
			"binding":     bindingView,
		})
	})

	// POST /bind-token — mint a one-shot /start bind token.
	user.POST("/bind-token", func(c *gin.Context) {
		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		ctx := c.Request.Context()
		currentUser := middleware.CurrentUser(c)

		config, err := telegramLoadBotConfig(ctx)
		if err != nil {
			telegramRespondInternalError(c)
			return
		}
		if !config.Configured || config.BotUsername == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Telegram bot is not configured"})
			return
		}

		if err := db.TelegramDeleteUnusedBindTokens(ctx, currentUser.ID); err != nil {
			telegramRespondInternalError(c)
			return
		}

		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			telegramRespondInternalError(c)
			return
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		expiresAt := time.Now().UTC().Add(telegramBindTokenTTL)

		if err := db.TelegramCreateBindToken(ctx, currentUser.ID, telegramHashToken(token), expiresAt); err != nil {
			telegramRespondInternalError(c)
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"bindUrl":   "https://t.me/" + *config.BotUsername + "?start=" + telegramBindTokenPrefix + token,
			"expiresAt": telegramISOTime(expiresAt),
		})
	})

	// DELETE /binding — unlink every binding of the caller.
	user.DELETE("/binding", func(c *gin.Context) {
		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		currentUser := middleware.CurrentUser(c)
		if err := db.TelegramDeleteBindingsByUserID(c.Request.Context(), currentUser.ID); err != nil {
			telegramRespondInternalError(c)
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "Telegram binding removed"})
	})

	// POST /webhook — PUBLIC. Registered on the engine (not on `group`) so the
	// authenticate middleware never runs: Telegram authenticates with the secret
	// header, not a JWT.
	r.POST(telegramWebhookPath, telegramWebhookHandler)
}

// telegramWebhookHandler mirrors the public POST /webhook route.
func telegramWebhookHandler(c *gin.Context) {
	// Fastify parses the body before the handler runs (no schema here).
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}

	ctx := c.Request.Context()
	config, err := telegramLoadBotConfig(ctx)
	if err != nil {
		telegramRespondInternalError(c)
		return
	}

	// Disabled/unconfigured bot: acknowledge without touching the secret.
	if !config.Configured || config.BotToken == nil {
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}

	if config.WebhookSecret != nil {
		// Node joins duplicate header values with ", "; mirror that so a repeated
		// header cannot be smuggled past the constant-time compare.
		secretFromHeader := strings.Join(c.Request.Header.Values("X-Telegram-Bot-Api-Secret-Token"), ", ")
		if secretFromHeader == "" || !telegramSecureEquals(secretFromHeader, *config.WebhookSecret) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			return
		}
	}

	var body any
	if decoded != nil {
		body = decoded.Value
	}

	update, ok := telegramParseWebhookUpdate(body)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}

	// `if (!chatId || !from || !text)`: chat id 0 is falsy in JavaScript too.
	if update.ChatID == 0 || update.From == nil || update.Text == nil || *update.Text == "" {
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}

	botToken := *config.BotToken
	text := *update.Text

	if target := telegramGetGroupJoinTarget(text); target != "" {
		if err := telegramHandleGroupJoinRequest(ctx, config, update.ChatID, update.From, update.ChatType, target); err != nil {
			telegramRespondInternalError(c)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}

	match := telegramBindCommandPattern.FindStringSubmatch(text)
	if match == nil {
		telegramNotify(ctx, botToken, update.ChatID,
			"可用指令：\n绑定账号：请先登录网站，在个人设置里生成 Telegram 绑定链接。"+
				"\n申请普通群：绑定后私聊发送 /join。"+
				"\n申请高级群：绑定后私聊发送 /join_vip。")
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}

	now := time.Now().UTC()
	tokenHash := telegramHashToken(match[1])
	bindToken, err := db.TelegramFindBindToken(ctx, tokenHash, now)
	if err != nil {
		telegramRespondInternalError(c)
		return
	}

	const invalidTokenText = "绑定链接无效或已过期，请回到网站重新生成绑定链接。"
	if bindToken == nil {
		telegramNotify(ctx, botToken, update.ChatID, invalidTokenText)
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}

	telegramUserID := telegramJSString(update.From["id"])
	existingBinding, err := db.TelegramGetBindingByTelegramUserID(ctx, telegramUserID)
	if err != nil {
		telegramRespondInternalError(c)
		return
	}
	if existingBinding != nil && existingBinding.UserID != bindToken.UserID {
		telegramNotify(ctx, botToken, update.ChatID, "这个 Telegram 账号已经绑定到另一个网站账号。")
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}

	// Consume the token with a conditional UPDATE before any side effect; a count
	// other than 1 means another update won the race.
	consumed, err := db.TelegramConsumeBindToken(ctx, bindToken.ID, now)
	if err != nil {
		telegramRespondInternalError(c)
		return
	}
	if consumed != 1 {
		telegramNotify(ctx, botToken, update.ChatID, invalidTokenText)
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}

	var telegramUsername, firstName, lastName *string
	if value, ok := update.From["username"].(string); ok && value != "" {
		telegramUsername = &value
	}
	if value, ok := update.From["first_name"].(string); ok && value != "" {
		firstName = &value
	}
	if value, ok := update.From["last_name"].(string); ok && value != "" {
		lastName = &value
	}

	if err := db.TelegramUpsertBinding(ctx, db.TelegramUpsertBindingInput{
		UserID:           bindToken.UserID,
		TelegramUserID:   telegramUserID,
		TelegramUsername: telegramUsername,
		FirstName:        firstName,
		LastName:         lastName,
		BoundAt:          now,
	}); err != nil {
		telegramRespondInternalError(c)
		return
	}

	telegramNotify(ctx, botToken, update.ChatID,
		"绑定成功。你现在可以关闭 Telegram，回到网站个人设置查看状态。")

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func init() {
	Register(RegisterTelegram)
}
