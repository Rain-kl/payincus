// Port of server/src/routes/notifications.ts.
//
// User-facing notification channel management plus the notification log/stats
// endpoints. Every route requires authentication (the TypeScript module puts
// `onRequest: [fastify.authenticate]` on each route), and every channel-scoped
// route restricts access to the owning user.
//
// The POST/PATCH routes declare Fastify body schemas. The validation below
// reproduces @fastify/ajv-compiler's defaults (allErrors:false, removeAdditional:true,
// coerceTypes:'array'), so only the first error is reported, unknown keys are
// dropped silently, scalars are stringified and single-element arrays unwrap. The
// messages match the branches of setErrorHandler in app.ts; the ordering of those
// errors (required before properties, properties in schema order) was verified
// against the real AJV instance.
package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

var (
	// userNotificationBotTokenPattern mirrors /^[1-9]\d{5,19}:[A-Za-z0-9_-]{20,}$/.
	userNotificationBotTokenPattern = regexp.MustCompile(`^[1-9][0-9]{5,19}:[A-Za-z0-9_-]{20,}$`)
	// userNotificationChatIDPattern mirrors /^-?\d{1,20}$|^@[A-Za-z0-9_]{5,32}$/.
	userNotificationChatIDPattern = regexp.MustCompile(`^-?[0-9]{1,20}$|^@[A-Za-z0-9_]{5,32}$`)
)

const (
	notificationMaxWebhookSecretLength = 256
	notificationNameMaxLength          = 50

	// Notification-local error codes. The TypeScript routes bypass apiError() for
	// these and send `{ error, ... }` straight to the client, so they stay plain
	// strings instead of lib.ErrorCode values (which would rewrite the payload).
	notificationTestFailedCode = "TEST_FAILED"

	// Fastify body-parser messages, surfaced verbatim by setErrorHandler.
	notificationEmptyBodyMessage   = "Body cannot be empty when content-type is set to 'application/json'"
	notificationInvalidJSONMessage = "Body is not valid JSON but content-type is set to 'application/json'"
)

// userNotificationChannelTypes mirrors USER_NOTIFICATION_CHANNEL_TYPES.
var userNotificationChannelTypes = map[string]bool{
	string(models.NotificationChannelTypeTelegram): true,
	string(models.NotificationChannelTypeDiscord):  true,
	string(models.NotificationChannelTypeWebhook):  true,
	string(models.NotificationChannelTypeEmail):    true,
}

// ==================== 请求体 ====================

// notificationCreateBody is the validated POST body.
type notificationCreateBody struct {
	Type    string
	Name    string
	Config  map[string]any
	Enabled bool
}

// notificationUpdateBody is the validated PATCH body. Nil pointers mean "key
// absent"; ConfigProvided distinguishes absent config from an empty object.
type notificationUpdateBody struct {
	Name           *string
	Config         map[string]any
	ConfigProvided bool
	Enabled        *bool
}

// readNotificationBody decodes the raw JSON body, mirroring Fastify's JSON
// body-parser failures.
func readNotificationBody(c *gin.Context) (any, string) {
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, notificationInvalidJSONMessage
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, notificationEmptyBodyMessage
	}

	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, notificationInvalidJSONMessage
	}
	return decoded, ""
}

// coerceNotificationString mirrors AJV's `coerceTypes: 'array'` coercion for a
// string-typed property: one level of single-element array unwrapping, then the
// JS String() coercion for scalars.
func coerceNotificationString(value any) (string, bool) {
	if array, ok := value.([]any); ok {
		if len(array) != 1 {
			return "", false
		}
		value = array[0]
	}

	switch typed := value.(type) {
	case string:
		return typed, true
	case nil:
		// AJV coerces null to "" for type:'string'; minLength then rejects it.
		return "", true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		// formatTerminalCommandNumber is the package-local JS String(number).
		return formatTerminalCommandNumber(typed), true
	default:
		return "", false
	}
}

// coerceNotificationBoolean mirrors AJV's boolean coercion: null/0/"false" become
// false, 1/"true" become true, everything else fails. A single-element array is
// unwrapped first.
func coerceNotificationBoolean(value any) (bool, bool) {
	if array, ok := value.([]any); ok {
		if len(array) != 1 {
			return false, false
		}
		value = array[0]
	}

	switch typed := value.(type) {
	case bool:
		return typed, true
	case nil:
		return false, true
	case float64:
		if typed == 0 {
			return false, true
		}
		if typed == 1 {
			return true, true
		}
		return false, false
	case string:
		if typed == "false" {
			return false, true
		}
		if typed == "true" {
			return true, true
		}
		return false, false
	default:
		return false, false
	}
}

// notificationObject mirrors AJV's type:'object' check: arrays and null are not
// objects, and `coerceTypes: 'array'` does not unwrap into an object.
func notificationObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

// validateNotificationBody mirrors the two route schemas. requireAll selects the
// POST schema (which has a `required` list) over the PATCH schema. The returned
// string is the first validation message, mapped exactly like setErrorHandler.
func validateNotificationBody(decoded any, requireAll bool) (any, string) {
	object, ok := notificationObject(decoded)
	if !ok {
		return nil, "must be object"
	}

	// `required` is checked before the properties, in schema order.
	if requireAll {
		for _, field := range []string{"type", "name", "config"} {
			if _, present := object[field]; !present {
				return nil, field + " is required"
			}
		}
	}

	create := notificationCreateBody{}
	update := notificationUpdateBody{}

	if requireAll {
		if value, present := object["type"]; present {
			coerced, ok := coerceNotificationString(value)
			if !ok {
				return nil, "must be string"
			}
			if !userNotificationChannelTypes[coerced] {
				return nil, "must be equal to one of the allowed values"
			}
			create.Type = coerced
		}
	}

	if value, present := object["name"]; present {
		coerced, ok := coerceNotificationString(value)
		if !ok {
			return nil, "must be string"
		}
		length := utf8.RuneCountInString(coerced)
		if length < 1 {
			return nil, "name is too short"
		}
		if length > notificationNameMaxLength {
			return nil, "name is too long"
		}
		create.Name = coerced
		update.Name = &coerced
	}

	if value, present := object["config"]; present {
		config, ok := notificationObject(value)
		if !ok {
			return nil, "must be object"
		}
		create.Config = config
		update.Config = config
		update.ConfigProvided = true
	}

	if value, present := object["enabled"]; present {
		coerced, ok := coerceNotificationBoolean(value)
		if !ok {
			return nil, "must be boolean"
		}
		create.Enabled = coerced
		update.Enabled = &coerced
	}

	// The schema has no defaults, so POST's `enabled` falls back to the handler's
	// `input.enabled ?? true` when the key is absent.
	if _, present := object["enabled"]; !present {
		create.Enabled = true
	}

	if requireAll {
		return create, ""
	}
	return update, ""
}

// respondNotificationValidation mirrors the validation branch of setErrorHandler.
func respondNotificationValidation(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": message, "code": "VALIDATION_ERROR"})
}

// respondNotificationBadRequest mirrors `reply.code(400).send({ error })` used by
// the body normalizers and config validators.
func respondNotificationBadRequest(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": message})
}

// ==================== 配置校验 ====================

// notificationConfigValidation is the three-way result of validateConfig: valid,
// a 400 message, or an internal failure (the TypeScript code throws on a
// non-string value in a position it does not type-check).
type notificationConfigValidation struct {
	Valid    bool
	Error    string
	Internal bool
}

// validateNotificationConfig mirrors validateConfig(type, config). It mutates
// config in place (trimmed botToken/chatId/url/secret), exactly like the original
// so the normalized values are what gets persisted.
func validateNotificationConfig(channelType string, config map[string]any) notificationConfigValidation {
	switch channelType {
	case "telegram":
		botToken, okToken := config["botToken"].(string)
		chatID, okChat := config["chatId"].(string)
		if !okToken || !okChat {
			return notificationConfigValidation{Error: "Please provide Bot Token and Chat ID"}
		}
		trimmedToken := strings.TrimSpace(botToken)
		trimmedChatID := strings.TrimSpace(chatID)
		if !userNotificationBotTokenPattern.MatchString(trimmedToken) {
			return notificationConfigValidation{Error: "Invalid Telegram Bot Token"}
		}
		if !userNotificationChatIDPattern.MatchString(trimmedChatID) {
			return notificationConfigValidation{Error: "Invalid Telegram Chat ID"}
		}
		config["botToken"] = trimmedToken
		config["chatId"] = trimmedChatID

	case "discord":
		// `if (!config.webhookUrl || typeof config.webhookUrl !== 'string')`
		webhookURL, present := config["webhookUrl"]
		webhookURLText, isString := webhookURL.(string)
		if !present || !notificationTruthy(webhookURL) || !isString {
			return notificationConfigValidation{Error: "Please provide Discord Webhook URL"}
		}
		if !strings.HasPrefix(webhookURLText, "https://discord.com/api/webhooks/") {
			return notificationConfigValidation{Error: "Invalid Discord Webhook URL"}
		}

	case "webhook":
		rawURL, present := config["url"]
		if !present || !notificationTruthy(rawURL) {
			return notificationConfigValidation{Error: "Please provide Webhook URL"}
		}
		urlText, isString := rawURL.(string)
		if !isString {
			return notificationConfigValidation{Error: "Please provide Webhook URL"}
		}
		trimmedURL := strings.TrimSpace(urlText)
		if !strings.HasPrefix(trimmedURL, "http://") && !strings.HasPrefix(trimmedURL, "https://") {
			return notificationConfigValidation{Error: "Invalid Webhook URL"}
		}
		config["url"] = trimmedURL

		// `if (config.secret !== undefined)`: an explicit null still takes this
		// branch and is rejected because it is not a string.
		if secret, present := config["secret"]; present {
			secretText, isString := secret.(string)
			if !isString || utf16Length(secretText) > notificationMaxWebhookSecretLength {
				return notificationConfigValidation{Error: "Invalid Webhook secret"}
			}
			config["secret"] = strings.TrimSpace(secretText)
		}

	case "email":
		if email, present := config["email"]; !present || !notificationTruthy(email) {
			return notificationConfigValidation{Error: "Please provide email address"}
		}
	}

	return notificationConfigValidation{Valid: true}
}

// notificationTruthy mirrors JavaScript truthiness for decoded JSON scalars.
func notificationTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0
	case json.Number:
		number, err := typed.Float64()
		return err != nil || number != 0
	default:
		// Objects and arrays are always truthy.
		return true
	}
}

// validateNotificationOutboundConfig mirrors validateOutboundConfig: only webhook
// channels with a string URL are SSRF-checked up front.
func validateNotificationOutboundConfig(ctx context.Context, channelType string, config map[string]any) notificationConfigValidation {
	if channelType != "webhook" {
		return notificationConfigValidation{Valid: true}
	}
	urlText, isString := config["url"].(string)
	if !isString {
		return notificationConfigValidation{Valid: true}
	}

	if _, err := lib.AssertSafeWebhookURL(ctx, urlText); err != nil {
		var validationError *lib.OutboundTargetValidationError
		if errors.As(err, &validationError) {
			return notificationConfigValidation{Error: validationError.Message}
		}
		return notificationConfigValidation{Internal: true}
	}
	return notificationConfigValidation{Valid: true}
}

// ==================== 展示辅助 ====================

// jsStringSlice mirrors String.prototype.slice(start, end) with negative indexes.
// Config values are ASCII tokens/URLs, so rune indexing is equivalent to the
// UTF-16 code-unit indexing the TypeScript uses.
func jsStringSlice(value string, start, end int) string {
	runes := []rune(value)
	length := len(runes)

	if start < 0 {
		start = length + start
		if start < 0 {
			start = 0
		}
	}
	if start > length {
		start = length
	}
	if end < 0 {
		end = length + end
		if end < 0 {
			end = 0
		}
	}
	if end > length {
		end = length
	}
	if end < start {
		end = start
	}
	return string(runes[start:end])
}

// jsURLHostname mirrors `new URL(value).hostname`, reporting false when the
// constructor would throw.
func jsURLHostname(value string) (string, bool) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", false
	}
	return strings.ToLower(parsed.Hostname()), true
}

// notificationConfigPreview mirrors getConfigPreview(type, configStr).
func notificationConfigPreview(channelType, configJSON string) string {
	var decoded any
	if err := json.Unmarshal([]byte(configJSON), &decoded); err != nil {
		return "Config error"
	}

	config, isObject := decoded.(map[string]any)
	if !isObject {
		// The TypeScript indexes into whatever JSON.parse produced; only objects
		// can carry the fields below, so everything else reads as undefined.
		config = map[string]any{}
	}

	switch channelType {
	case "telegram":
		chatID := "Not set"
		if value, present := config["chatId"]; present && notificationTruthy(value) {
			chatID = notificationValueString(value)
		}
		return "Chat ID: " + chatID
	case "discord":
		if webhookURL, ok := config["webhookUrl"].(string); ok && webhookURL != "" {
			return "Webhook: ..." + jsStringSlice(webhookURL, -20, len([]rune(webhookURL)))
		}
		return "Not set"
	case "webhook":
		if urlText, ok := config["url"].(string); ok && urlText != "" {
			if hostname, ok := jsURLHostname(urlText); ok {
				return "URL: " + hostname
			}
			return "URL: Invalid"
		}
		return "Not set"
	case "email":
		if email, ok := config["email"].(string); ok {
			return email
		}
		return "Not set"
	default:
		return "Unknown type"
	}
}

// notificationValueString mirrors template-literal interpolation of a scalar.
func notificationValueString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return formatTerminalCommandNumber(typed)
	case nil:
		return "null"
	default:
		return "[object Object]"
	}
}

// spreadToObject mirrors object spread of a decoded JSON value: objects are
// copied as-is, arrays become index-keyed objects and scalars/null become {}.
func spreadToObject(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case []any:
		out := make(map[string]any, len(typed))
		for index, item := range typed {
			out[strconv.Itoa(index)] = item
		}
		return out
	default:
		return map[string]any{}
	}
}

// maskNotificationSensitiveConfig mirrors maskSensitiveConfig(type, config).
func maskNotificationSensitiveConfig(channelType string, config map[string]any) map[string]any {
	masked := make(map[string]any, len(config))
	for key, value := range config {
		masked[key] = value
	}

	switch channelType {
	case "telegram":
		if botToken, ok := masked["botToken"].(string); ok && botToken != "" {
			runes := []rune(botToken)
			masked["botToken"] = jsStringSlice(botToken, 0, 10) + "..." + jsStringSlice(botToken, len(runes)-5, len(runes))
		}
	case "discord":
		if webhookURL, ok := masked["webhookUrl"].(string); ok && webhookURL != "" {
			masked["webhookUrl"] = jsStringSlice(webhookURL, 0, 50) + "..."
		}
	case "webhook":
		if notificationTruthy(masked["secret"]) {
			masked["secret"] = "***已设置***"
		}
	}

	return masked
}

// parseNotificationQueryInteger mirrors parsePositiveInteger:
// parseInt(value, 10) with a fallback for anything that is not a positive integer.
func parseNotificationQueryInteger(value string, fallback int) int {
	parsed, ok := parseIntPrefix(value)
	if !ok || parsed <= 0 {
		return fallback
	}
	return parsed
}

// parseIntPrefix mirrors parseInt(value, 10): leading whitespace and sign are
// accepted, parsing stops at the first non-digit, and no digits means NaN.
func parseIntPrefix(value string) (int, bool) {
	trimmed := strings.TrimLeft(value, " \t\n\r\v\f")
	if trimmed == "" {
		return 0, false
	}

	index := 0
	if trimmed[0] == '+' || trimmed[0] == '-' {
		index = 1
	}
	digitsStart := index
	for index < len(trimmed) && trimmed[index] >= '0' && trimmed[index] <= '9' {
		index++
	}
	if index == digitsStart {
		return 0, false
	}

	parsed, err := strconv.ParseInt(trimmed[:index], 10, 64)
	if err != nil || parsed > math.MaxInt32 || parsed < math.MinInt32 {
		// Number.isInteger still holds beyond int32, but every consumer is a page
		// number; clamp to the int32 range rather than overflow.
		if err != nil {
			return 0, false
		}
		return math.MaxInt32, true
	}
	return int(parsed), true
}

// notificationChannelOrError loads a channel by id, writes the 404 response when it
// is absent and returns nil so the handler can bail out.
func notificationChannelOrError(c *gin.Context, channelID int64) *db.NotificationChannelRow {
	channel, err := db.GetNotificationChannelById(c.Request.Context(), channelID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return nil
	}
	if channel == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotificationChannelNotFound, "")
		return nil
	}
	return channel
}

// ==================== 路由 ====================

// RegisterNotifications mounts /api/notifications. Every route requires auth.
func RegisterNotifications(r *gin.Engine) {
	group := r.Group("/api/notifications", middleware.Authenticate())

	// 获取用户的通知渠道列表
	listChannels := func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		channels, err := db.GetNotificationChannelsByUserId(c.Request.Context(), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		items := make([]gin.H, 0, len(channels))
		for _, channel := range channels {
			items = append(items, gin.H{
				"id":      channel.ID,
				"type":    string(channel.Type),
				"name":    channel.Name,
				"enabled": channel.Enabled != 0,
				// 隐藏敏感配置信息
				"configPreview": notificationConfigPreview(string(channel.Type), channel.Config),
				"createdAt":     channel.CreatedAt,
			})
		}
		c.JSON(http.StatusOK, gin.H{"channels": items})
	}
	// Fastify registers both '/api/notifications' and '/api/notifications/'
	// (prefixTrailingSlash defaults to 'both').
	group.GET("", listChannels)
	group.GET("/", listChannels)

	// 获取单个通知渠道详情
	group.GET("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		channelID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		channel := notificationChannelOrError(c, channelID)
		if channel == nil {
			return
		}

		// 管理员只管理系统层面，不参与用户通知渠道操作
		if channel.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		// JSON.parse throws only for malformed JSON; a non-object value is still
		// valid and `{ ...config }` then produces an (index-keyed or empty) object.
		var decoded any
		if err := json.Unmarshal([]byte(channel.Config), &decoded); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"channel": gin.H{
			"id":        channel.ID,
			"type":      string(channel.Type),
			"name":      channel.Name,
			"config":    maskNotificationSensitiveConfig(string(channel.Type), spreadToObject(decoded)),
			"enabled":   channel.Enabled != 0,
			"createdAt": channel.CreatedAt,
		}})
	})

	// 添加通知渠道
	createChannel := func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		decoded, decodeError := readNotificationBody(c)
		if decodeError != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": decodeError})
			return
		}

		validated, message := validateNotificationBody(decoded, true)
		if message != "" {
			respondNotificationValidation(c, message)
			return
		}
		body := validated.(notificationCreateBody)

		normalized, ok := normalizeNotificationChannelCreateInput(c, body)
		if !ok {
			return
		}

		// 验证配置
		config := normalized.config
		if validation := validateNotificationConfig(normalized.channelType, config); !validation.Valid {
			if validation.Internal {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
				return
			}
			respondNotificationBadRequest(c, validation.Error)
			return
		}
		if validation := validateNotificationOutboundConfig(c.Request.Context(), normalized.channelType, config); !validation.Valid {
			if validation.Internal {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
				return
			}
			respondNotificationBadRequest(c, validation.Error)
			return
		}

		enabled := normalized.enabled
		channelID, err := db.CreateNotificationChannel(c.Request.Context(), db.CreateNotificationChannelData{
			UserID:  user.ID,
			Type:    models.NotificationChannelType(normalized.channelType),
			Name:    normalized.name,
			Config:  config,
			Enabled: &enabled,
		})
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		userID := int64(user.ID)
		db.CreateLog(c.Request.Context(), &userID, db.LogModuleNotification, db.LogActionNotificationAdd,
			"Added "+normalized.channelType+" notification channel \""+normalized.name+"\"",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusCreated, gin.H{
			"message": "Notification channel added",
			"channel": gin.H{
				"id":      channelID,
				"type":    normalized.channelType,
				"name":    normalized.name,
				"enabled": enabled,
			},
		})
	}
	group.POST("", createChannel)
	// Fastify's prefixTrailingSlash default ('both') also exposes '/api/notifications/'.
	group.POST("/", createChannel)

	// 更新通知渠道
	group.PATCH("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		// Fastify validates the body before the handler runs, so the schema check
		// precedes the route-id check here as well.
		decoded, decodeError := readNotificationBody(c)
		if decodeError != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": decodeError})
			return
		}

		validated, message := validateNotificationBody(decoded, false)
		if message != "" {
			respondNotificationValidation(c, message)
			return
		}
		body := validated.(notificationUpdateBody)

		channelID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		normalized, ok := normalizeNotificationChannelUpdateInput(c, body)
		if !ok {
			return
		}

		channel := notificationChannelOrError(c, channelID)
		if channel == nil {
			return
		}

		// 管理员只管理系统层面，不参与用户通知渠道操作
		if channel.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		// 如果更新配置，验证配置（`if (config)` is true for every provided object）
		if normalized.configProvided {
			config := normalized.config
			if validation := validateNotificationConfig(string(channel.Type), config); !validation.Valid {
				if validation.Internal {
					c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
					return
				}
				respondNotificationBadRequest(c, validation.Error)
				return
			}
			if validation := validateNotificationOutboundConfig(c.Request.Context(), string(channel.Type), config); !validation.Valid {
				if validation.Internal {
					c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
					return
				}
				respondNotificationBadRequest(c, validation.Error)
				return
			}
		}

		if err := db.UpdateNotificationChannel(c.Request.Context(), int64(channel.ID), db.UpdateNotificationChannelData{
			Name:           normalized.name,
			Config:         normalized.config,
			ConfigProvided: normalized.configProvided,
			Enabled:        normalized.enabled,
		}); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		// 构建变更详情
		changes := make([]string, 0, 3)
		if normalized.name != nil {
			changes = append(changes, "name -> \""+*normalized.name+"\"")
		}
		if normalized.configProvided {
			changes = append(changes, "config updated")
		}
		if normalized.enabled != nil {
			changes = append(changes, "enabled -> "+strconv.FormatBool(*normalized.enabled))
		}

		userID := int64(user.ID)
		db.CreateLog(c.Request.Context(), &userID, db.LogModuleNotification, "notification.update",
			"Updated "+string(channel.Type)+" notification channel \""+channel.Name+"\": "+strings.Join(changes, ", "),
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "Notification channel updated"})
	})

	// 删除通知渠道
	group.DELETE("/:id", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		channelID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		channel := notificationChannelOrError(c, channelID)
		if channel == nil {
			return
		}

		// 管理员只管理系统层面，不参与用户通知渠道操作
		if channel.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		if err := db.DeleteNotificationChannel(c.Request.Context(), int64(channel.ID)); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		userID := int64(user.ID)
		db.CreateLog(c.Request.Context(), &userID, db.LogModuleNotification, db.LogActionNotificationDelete,
			"Deleted "+string(channel.Type)+" notification channel \""+channel.Name+"\"",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "Notification channel deleted"})
	})

	// 测试通知渠道
	group.POST("/:id/test", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		channelID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		channel := notificationChannelOrError(c, channelID)
		if channel == nil {
			return
		}

		// 管理员只管理系统层面，不参与用户通知渠道操作
		if channel.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		result := lib.TestNotificationChannel(c.Request.Context(), lib.NotificationChannel{
			ID:     channel.ID,
			Type:   string(channel.Type),
			Config: channel.Config,
		})

		if result.Success {
			c.JSON(http.StatusOK, gin.H{"message": "Test notification sent"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Test failed: " + result.Error,
			"code":  notificationTestFailedCode,
		})
	})

	// 切换启用/禁用
	group.POST("/:id/toggle", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		channelID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		channel := notificationChannelOrError(c, channelID)
		if channel == nil {
			return
		}

		// 管理员只管理系统层面，不参与用户通知渠道操作
		if channel.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		newEnabled := channel.Enabled == 0
		if err := db.UpdateNotificationChannel(c.Request.Context(), int64(channel.ID), db.UpdateNotificationChannelData{
			Enabled: &newEnabled,
		}); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		action := "notification.disable"
		label := "Disabled"
		message := "Notification channel disabled"
		if newEnabled {
			action = "notification.enable"
			label = "Enabled"
			message = "Notification channel enabled"
		}

		userID := int64(user.ID)
		db.CreateLog(c.Request.Context(), &userID, db.LogModuleNotification, action,
			label+" "+string(channel.Type)+" notification channel \""+channel.Name+"\"",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": message, "enabled": newEnabled})
	})

	// 获取通知历史记录
	group.GET("/logs", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		query := c.Request.URL.Query()

		page := parseNotificationQueryInteger(query.Get("page"), 1)
		pageSize := parseNotificationQueryInteger(query.Get("pageSize"), 20)
		if pageSize > 100 {
			pageSize = 100
		}
		status := db.NormalizeNotificationLogStatus(query.Get("status"))

		result, err := db.GetNotificationLogsByUserId(c.Request.Context(), user.ID, db.NotificationLogListOptions{
			Page:     page,
			PageSize: pageSize,
			Status:   status,
		})
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, result)
	})

	// 获取通知统计
	group.GET("/stats", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		stats, err := db.GetNotificationStats(c.Request.Context(), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"stats": stats})
	})

	// 获取所有全局通知渠道（用于绑定套餐）
	group.GET("/global-channels", func(c *gin.Context) {
		channels, err := db.GetGlobalNotificationChannels(c.Request.Context())
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		items := make([]gin.H, 0, len(channels))
		for _, channel := range channels {
			// 不向普通用户暴露 chatId 等渠道配置明细，仅返回是否已配置。
			preview := "未配置"
			var decoded any
			if err := json.Unmarshal(channel.Config, &decoded); err == nil {
				if config, ok := decoded.(map[string]any); ok && notificationTruthy(config["chatId"]) {
					preview = "已配置"
				}
			}
			items = append(items, gin.H{
				"id":            channel.ID,
				"name":          channel.Name,
				"type":          channel.Type,
				"configPreview": preview,
			})
		}
		c.JSON(http.StatusOK, gin.H{"channels": items})
	})
}

// normalizedNotificationCreate mirrors NotificationChannelCreateInput after the
// runtime normalizers have run.
type normalizedNotificationCreate struct {
	channelType string
	name        string
	config      map[string]any
	enabled     bool
}

// normalizeNotificationChannelCreateInput mirrors
// normalizeNotificationChannelCreateInput; it writes the 400 response and reports
// ok=false when normalization fails. The schema guarantees the shapes, so only the
// name rule can still fail here.
func normalizeNotificationChannelCreateInput(c *gin.Context, body notificationCreateBody) (normalizedNotificationCreate, bool) {
	name, ok := normalizeNotificationChannelName(body.Name)
	if !ok {
		respondNotificationBadRequest(c, "Notification channel name must be 1-50 characters")
		return normalizedNotificationCreate{}, false
	}

	return normalizedNotificationCreate{
		channelType: body.Type,
		name:        name,
		config:      body.Config,
		enabled:     body.Enabled,
	}, true
}

// normalizedNotificationUpdate mirrors NotificationChannelUpdateInput.
type normalizedNotificationUpdate struct {
	name           *string
	config         map[string]any
	configProvided bool
	enabled        *bool
}

// normalizeNotificationChannelUpdateInput mirrors
// normalizeNotificationChannelUpdateInput.
func normalizeNotificationChannelUpdateInput(c *gin.Context, body notificationUpdateBody) (normalizedNotificationUpdate, bool) {
	out := normalizedNotificationUpdate{
		config:         body.Config,
		configProvided: body.ConfigProvided,
		enabled:        body.Enabled,
	}
	if body.Name != nil {
		name, ok := normalizeNotificationChannelName(*body.Name)
		if !ok {
			respondNotificationBadRequest(c, "Notification channel name must be 1-50 characters")
			return normalizedNotificationUpdate{}, false
		}
		out.name = &name
	}
	return out, true
}

// normalizeNotificationChannelName mirrors normalizeNotificationChannelName: trim,
// then require 1-50 (UTF-16 code units, like String#length).
func normalizeNotificationChannelName(value string) (string, bool) {
	name := strings.TrimSpace(value)
	if name == "" || utf16Length(name) > notificationNameMaxLength {
		return "", false
	}
	return name, true
}

// utf16Length mirrors String.prototype.length (UTF-16 code units).
func utf16Length(value string) int {
	length := 0
	for _, char := range value {
		if char > 0xFFFF {
			length += 2
			continue
		}
		length++
	}
	return length
}

func init() {
	Register(RegisterNotifications)
}
