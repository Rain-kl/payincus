// Port of server/src/routes/admin-notification-channels.ts.
//
// Admin-only management of the global Telegram notification channels that
// packages can bind to. Every route carries `onRequest: [fastify.authenticateAdmin]`,
// i.e. authenticate + requireAdmin, which middleware.AuthenticateAdmin mirrors.
//
// Security invariants of the TypeScript source:
//
//   - The listen endpoint never returns the stored botToken: it renders
//     `configPreview` ("Chat ID: ..."), which is all the list exists for.
//   - The detail endpoint masks the token as first-8 + "..." + last-5, so the
//     secret can never be read back in full.
//   - A masked token is rejected as input (maskedTelegramBotTokenPattern), so a
//     client cannot round-trip the mask into the database.
//   - Nothing here logs a botToken; the audit lines embed only the channel name
//     (and the chat id, which is not a secret).
//
// The POST/PATCH routes declare Fastify JSON schemas. Their validation runs
// through AJV with the defaults documented in GO_MIGRATION.md (coerceTypes:
// 'array', useDefaults: true, removeAdditional: true, allErrors: false), and the
// resulting messages are mapped by setErrorHandler in app.ts. validateChannelBody
// and its helpers below reproduce that pair, including:
//
//   - `required` is checked before `properties`, in schema order;
//   - only the first error is reported (allErrors: false), so `details` is never
//     emitted;
//   - unknown keys are silently dropped (removeAdditional: true);
//   - scalars are coerced ("5" → 5, 1 → "1", 0 → false) and single-element arrays
//     are unwrapped (coerceTypes: 'array');
//   - maxLength counts Unicode code points, not bytes.
package routes

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// Telegram input patterns, mirroring the route module's local constants. They are
// deliberately separate from the identically-shaped ones in notifications.go so
// the two modules can evolve independently.
var (
	// adminChannelBotTokenPattern mirrors /^[1-9]\d{5,19}:[A-Za-z0-9_-]{20,}$/.
	adminChannelBotTokenPattern = regexp.MustCompile(`^[1-9][0-9]{5,19}:[A-Za-z0-9_-]{20,}$`)
	// adminChannelChatIDPattern mirrors /^-?\d{1,20}$|^@[A-Za-z0-9_]{5,32}$/.
	adminChannelChatIDPattern = regexp.MustCompile(`^-?[0-9]{1,20}$|^@[A-Za-z0-9_]{5,32}$`)
	// adminChannelMaskedTokenPattern mirrors /^.{1,20}\.\.\..{1,20}$/. `.` matches
	// no newline in either JS or Go's regexp, so the semantics agree.
	adminChannelMaskedTokenPattern = regexp.MustCompile(`^.{1,20}\.\.\..{1,20}$`)
)

// adminChannelNameMaxLength is the schema's `maxLength: 50`.
const adminChannelNameMaxLength = 50

// adminChannelUTF16Length mirrors String.prototype.length (UTF-16 code units),
// which is what `name.length > 50` in the TypeScript normalizer measures. It is
// intentionally local to this module rather than shared, so the module has no
// compile-time dependency on another route file.
func adminChannelUTF16Length(value string) int {
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

// ==================== body parsing (Fastify parity) ====================

// channelBodyError is a body-phase failure that Fastify would have produced
// before the handler ran. `Validation` distinguishes the AJV branch
// (`{ error, code: 'VALIDATION_ERROR' }`, mapped by setErrorHandler) from the
// body-parser branch (`{ error }`).
type channelBodyError struct {
	Status     int
	Message    string
	Validation bool
}

func (e *channelBodyError) respond(c *gin.Context) {
	if e.Validation {
		c.JSON(e.Status, gin.H{"error": e.Message, "code": "VALIDATION_ERROR"})
		return
	}
	c.JSON(e.Status, gin.H{"error": e.Message})
}

// readChannelRequestBody mirrors the Fastify body parser for this module. It
// returns only *parser* failures; "the parsed value is not an object" belongs to
// validateChannelBody, exactly as it belongs to AJV in Node. A nil value with a
// nil error is Fastify's `request.body === undefined` (no body was sent, or the
// multipart parser consumed it).
func readChannelRequestBody(c *gin.Context) (any, *channelBodyError) {
	request := c.Request

	if request.Method == http.MethodGet || request.Method == http.MethodHead || request.Method == http.MethodTrace {
		return nil, nil
	}

	contentType := request.Header.Get("Content-Type")
	if contentType == "" {
		if len(request.TransferEncoding) == 0 && request.ContentLength <= 0 {
			// No body at all: Fastify never runs a parser.
			return nil, nil
		}
		return nil, &channelBodyError{Status: http.StatusUnsupportedMediaType, Message: "Unsupported Media Type: undefined"}
	}

	kind := apiTokenBodyParserKind(contentType)
	switch kind {
	case "":
		return nil, &channelBodyError{Status: http.StatusUnsupportedMediaType, Message: "Unsupported Media Type: " + contentType}
	case "multipart":
		// @fastify/multipart with attachFieldsToBody:false leaves body undefined.
		return nil, nil
	}

	limit := apiTokenBodyLimit()
	if request.ContentLength > limit {
		return nil, &channelBodyError{Status: http.StatusRequestEntityTooLarge, Message: apiTokenBodyTooLargeMessage}
	}
	data, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	if err != nil {
		return nil, &channelBodyError{Status: http.StatusBadRequest, Message: apiTokenInvalidJSONBodyMessage}
	}
	if int64(len(data)) > limit {
		return nil, &channelBodyError{Status: http.StatusRequestEntityTooLarge, Message: apiTokenBodyTooLargeMessage}
	}

	if kind == "text" {
		// text/plain parses to a string; the object schema then rejects it.
		return string(data), nil
	}

	if len(data) == 0 {
		return nil, &channelBodyError{Status: http.StatusBadRequest, Message: apiTokenEmptyJSONBodyMessage}
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, &channelBodyError{Status: http.StatusBadRequest, Message: apiTokenInvalidJSONBodyMessage}
	}
	if apiTokenHasForbiddenPrototypeKey(decoded) {
		// secure-json-parse rejects the payload, so Fastify reports it as bad JSON.
		return nil, &channelBodyError{Status: http.StatusBadRequest, Message: apiTokenInvalidJSONBodyMessage}
	}
	return decoded, nil
}

// channelBody is the validated POST/PATCH payload. A nil pointer means the key was
// absent (PATCH then leaves the column alone); `Enabled` additionally needs
// EnabledPresent to distinguish an explicit false.
type channelBody struct {
	Name           *string
	BotToken       *string
	ChatID         *string
	Enabled        *bool
	EnabledPresent bool
}

// channelSchemaField describes one `properties` entry.
type channelSchemaField struct {
	Name      string
	MinLength int
	MaxLength int
	IsBoolean bool
}

// channelPostSchema mirrors the POST body schema, in source order.
var channelPostSchema = []channelSchemaField{
	{Name: "name", MinLength: 1, MaxLength: adminChannelNameMaxLength},
	{Name: "botToken", MinLength: 1},
	{Name: "chatId", MinLength: 1},
	{Name: "enabled", IsBoolean: true},
}

// channelPostRequired mirrors `required: ['name', 'botToken', 'chatId']`.
var channelPostRequired = []string{"name", "botToken", "chatId"}

// coerceChannelString mirrors AJV `coerceTypes: 'array'` for type:'string'.
func coerceChannelString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case nil:
		// AJV coerces null to "" for type:'string'; minLength then rejects it.
		return "", true
	case bool:
		if typed {
			return "true", true
		}
		return "false", true
	case float64:
		return formatTerminalCommandNumber(typed), true
	case []any:
		if len(typed) == 1 {
			return coerceChannelString(typed[0])
		}
		return "", false
	default:
		return "", false
	}
}

// coerceChannelBool mirrors AJV `coerceTypes: 'array'` for type:'boolean'.
// Verified against the running Node backend: only 0/1 and the strings "true"/
// "false" coerce (0 and 1 do NOT behave like arbitrary truthiness), and every
// other value — including "0", 2 and -1 — is a hard type error.
func coerceChannelBool(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case nil:
		// AJV coerces null to false for type:'boolean'.
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
		if typed == "true" {
			return true, true
		}
		if typed == "false" {
			return false, true
		}
		return false, false
	case []any:
		if len(typed) == 1 {
			return coerceChannelBool(typed[0])
		}
		return false, false
	default:
		return false, false
	}
}

// validateChannelBody mirrors the schema + AJV run. `required` is applied first
// (POST only) in schema order, then the properties in schema order; the first
// failure is returned, exactly as allErrors:false would.
func validateChannelBody(decoded any, requireAll bool) (channelBody, *channelBodyError) {
	var out channelBody

	object, ok := decoded.(map[string]any)
	if !ok {
		return out, &channelBodyError{Status: http.StatusBadRequest, Message: "must be object", Validation: true}
	}

	if requireAll {
		for _, key := range channelPostRequired {
			if _, present := object[key]; !present {
				return out, &channelBodyError{Status: http.StatusBadRequest, Message: key + " is required", Validation: true}
			}
		}
	}

	invalid := func(message string) (channelBody, *channelBodyError) {
		return channelBody{}, &channelBodyError{Status: http.StatusBadRequest, Message: message, Validation: true}
	}

	for _, field := range channelPostSchema {
		value, present := object[field.Name]
		if !present {
			continue
		}

		if field.IsBoolean {
			coerced, ok := coerceChannelBool(value)
			if !ok {
				return invalid("must be boolean")
			}
			out.Enabled = &coerced
			out.EnabledPresent = true
			continue
		}

		coerced, ok := coerceChannelString(value)
		if !ok {
			return invalid("must be string")
		}
		length := len([]rune(coerced))
		if field.MaxLength > 0 && length > field.MaxLength {
			return invalid(field.Name + " is too long")
		}
		if length < field.MinLength {
			return invalid(field.Name + " is too short")
		}

		fieldValue := coerced
		switch field.Name {
		case "name":
			out.Name = &fieldValue
		case "botToken":
			out.BotToken = &fieldValue
		case "chatId":
			out.ChatID = &fieldValue
		}
	}

	return out, nil
}

// ==================== input normalization ====================

// channelNormalized mirrors normalizeGlobalTelegramChannelInput's value branch.
type channelNormalized struct {
	Name     *string
	BotToken *string
	ChatID   *string
}

// normalizeGlobalTelegramChannelInput mirrors normalizeGlobalTelegramChannelInput:
// each provided field is trimmed and format-checked; the first failure wins.
func normalizeGlobalTelegramChannelInput(body channelBody) (channelNormalized, string) {
	var value channelNormalized

	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		// The TS uses `name.length > 50`, i.e. UTF-16 code units — NOT code points.
		// (AJV's maxLength in the schema counts code points, so the two differ for
		// astral characters: 50 emoji pass AJV and then fail here.)
		if name == "" || adminChannelUTF16Length(name) > 50 {
			return channelNormalized{}, "Channel name must be 1-50 characters"
		}
		value.Name = &name
	}

	if body.BotToken != nil {
		botToken := strings.TrimSpace(*body.BotToken)
		if botToken == "" || adminChannelMaskedTokenPattern.MatchString(botToken) || !adminChannelBotTokenPattern.MatchString(botToken) {
			return channelNormalized{}, "Invalid Bot Token format"
		}
		value.BotToken = &botToken
	}

	if body.ChatID != nil {
		chatID := strings.TrimSpace(*body.ChatID)
		if !adminChannelChatIDPattern.MatchString(chatID) {
			return channelNormalized{}, "Invalid Telegram Chat ID format"
		}
		value.ChatID = &chatID
	}

	return value, ""
}

// ==================== response shapes ====================

func channelISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// getTelegramPreview mirrors getTelegramPreview().
func getTelegramPreview(config map[string]any) string {
	chatID := "Not set"
	if raw, present := config["chatId"]; present {
		if text, ok := raw.(string); ok && text != "" {
			chatID = text
		} else if raw != nil {
			// `config.chatId || 'Not set'` also renders truthy non-strings.
			if rendered, ok := channelJSString(raw); ok {
				chatID = rendered
			}
		}
	}
	return "Chat ID: " + chatID
}

// channelJSString mirrors String(value) for the JSON scalars that can appear here.
func channelJSString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case float64:
		return formatTerminalCommandNumber(typed), true
	case bool:
		if typed {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

// maskBotToken mirrors maskBotToken(): first 8 + "..." + last 5, leaving every
// other config key untouched. The original map is not mutated.
func maskBotToken(config map[string]any) map[string]any {
	masked := make(map[string]any, len(config))
	for key, value := range config {
		masked[key] = value
	}

	token, ok := masked["botToken"].(string)
	if !ok || token == "" {
		return masked
	}
	// JS slice() saturates; for tokens shorter than 13 chars the two ranges
	// overlap, which mirrors `token.slice(0,8) + '...' + token.slice(-5)`.
	head := token
	if len([]rune(head)) > 8 {
		head = string([]rune(token)[:8])
	}
	runes := []rune(token)
	tail := token
	if len(runes) > 5 {
		tail = string(runes[len(runes)-5:])
	}
	masked["botToken"] = head + "..." + tail
	return masked
}

// channelConfigMap decodes the stored jsonb config. A non-object payload degrades
// to an empty map so JSON.parse-style callers cannot panic.
func channelConfigMap(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return map[string]any{}
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return object
}

// ==================== persistence ====================

type globalChannelListRow struct {
	ID            int32     `gorm:"column:id"`
	Name          string    `gorm:"column:name"`
	Type          string    `gorm:"column:type"`
	Enabled       bool      `gorm:"column:enabled"`
	Config        []byte    `gorm:"column:config"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	BoundPackages int64     `gorm:"column:bound_packages"`
}

type globalChannelRow struct {
	ID        int32     `gorm:"column:id"`
	UserID    int32     `gorm:"column:user_id"`
	Type      string    `gorm:"column:type"`
	Name      string    `gorm:"column:name"`
	Config    []byte    `gorm:"column:config"`
	Enabled   bool      `gorm:"column:enabled"`
	IsGlobal  bool      `gorm:"column:is_global"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// listGlobalChannels mirrors the findMany({ where: { isGlobal: true },
// orderBy: { createdAt: 'desc' }, select: { ..., _count: { packages } } }) query.
func listGlobalChannels(c *gin.Context) ([]globalChannelListRow, error) {
	var rows []globalChannelListRow
	err := db.DB.WithContext(c.Request.Context()).Raw(
		"SELECT c.id, c.name, c.type::text AS type, c.enabled, c.config, c.created_at," +
			" (SELECT COUNT(*) FROM packages p WHERE p.release_channel_id = c.id) AS bound_packages" +
			" FROM notification_channels c WHERE c.is_global = true" +
			" ORDER BY c.created_at DESC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// findGlobalChannel mirrors findFirst({ where: { id, isGlobal: true } }).
func findGlobalChannel(c *gin.Context, id int32) (*globalChannelRow, error) {
	var rows []globalChannelRow
	err := db.DB.WithContext(c.Request.Context()).Raw(
		"SELECT id, user_id, type::text AS type, name, config, enabled, is_global, created_at"+
			" FROM notification_channels WHERE id = ? AND is_global = true LIMIT 1", id).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

type globalChannelCreateData struct {
	UserID   int32
	Name     string
	BotToken string
	ChatID   any
	Enabled  bool
}

// createGlobalChannel mirrors the `notificationChannel.create({ data })` call.
func createGlobalChannel(c *gin.Context, data globalChannelCreateData) (int32, error) {
	config, err := json.Marshal(map[string]any{"botToken": data.BotToken, "chatId": data.ChatID})
	if err != nil {
		return 0, err
	}

	var rows []struct {
		ID int32 `gorm:"column:id"`
	}
	err = db.DB.WithContext(c.Request.Context()).Raw(
		"INSERT INTO notification_channels (user_id, type, name, config, enabled, is_global)"+
			" VALUES (?, 'telegram'::\"NotificationChannelType\", ?, ?::jsonb, ?, true)"+
			" RETURNING id",
		data.UserID, data.Name, string(config), data.Enabled).
		Scan(&rows).Error
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	return rows[0].ID, nil
}

// updateGlobalChannel mirrors `notificationChannel.update({ where: { id }, data })`:
// only the provided columns are written.
func updateGlobalChannel(c *gin.Context, id int32, name *string, enabled *bool, config map[string]any) error {
	assignments := make([]string, 0, 3)
	args := make([]any, 0, 4)

	if name != nil {
		assignments = append(assignments, "name = ?")
		args = append(args, *name)
	}
	if enabled != nil {
		assignments = append(assignments, "enabled = ?")
		args = append(args, *enabled)
	}
	if config != nil {
		encoded, err := json.Marshal(config)
		if err != nil {
			return err
		}
		assignments = append(assignments, "config = ?::jsonb")
		args = append(args, string(encoded))
	}
	if len(assignments) == 0 {
		// Prisma still issues an UPDATE for an empty data object; nothing changes.
		return nil
	}

	args = append(args, id)
	return db.DB.WithContext(c.Request.Context()).Exec(
		"UPDATE notification_channels SET "+strings.Join(assignments, ", ")+" WHERE id = ?", args...).
		Error
}

// deleteGlobalChannel mirrors `notificationChannel.delete({ where: { id } })`.
func deleteGlobalChannel(c *gin.Context, id int32) error {
	return db.DB.WithContext(c.Request.Context()).Exec(
		"DELETE FROM notification_channels WHERE id = ?", id).Error
}

// ==================== routes ====================

// errChannelNotFound is the literal 404 envelope of this module; it is not part of
// the shared error catalog (the module replies with `{ error }` only).
const (
	channelInvalidIDMessage  = "Invalid ID"
	channelNotFoundMessage   = "Channel not found"
	channelTestFailedPrefix  = "Test failed: "
	channelTelegramTypeValue = "telegram"
)

// RegisterAdminNotificationChannels mounts /api/admin/notification-channels.
//
// Fastify's prefix handling makes the plugin's '/' route answer both
// '/api/admin/notification-channels' and the trailing-slash form; '/:id' and
// '/:id/test' only match without one.
func RegisterAdminNotificationChannels(r *gin.Engine) {
	group := r.Group("/api/admin/notification-channels", middleware.AuthenticateAdmin())

	list := func(c *gin.Context) {
		rows, err := listGlobalChannels(c)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		channels := make([]gin.H, 0, len(rows))
		for _, row := range rows {
			config := channelConfigMap(row.Config)
			channels = append(channels, gin.H{
				"id":            row.ID,
				"name":          row.Name,
				"type":          row.Type,
				"enabled":       row.Enabled,
				"boundPackages": row.BoundPackages,
				// No botToken: the list only exposes the chat id preview.
				"configPreview": getTelegramPreview(config),
				"createdAt":     channelISOTime(row.CreatedAt),
			})
		}

		c.JSON(http.StatusOK, gin.H{"channels": channels})
	}

	get := func(c *gin.Context) {
		channelID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": channelInvalidIDMessage})
			return
		}

		channel, err := findGlobalChannel(c, int32(channelID))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if channel == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": channelNotFoundMessage})
			return
		}

		c.JSON(http.StatusOK, gin.H{"channel": gin.H{
			"id":      channel.ID,
			"name":    channel.Name,
			"type":    channel.Type,
			"enabled": channel.Enabled,
			// Only the masked token leaves the server.
			"config":    maskBotToken(channelConfigMap(channel.Config)),
			"createdAt": channelISOTime(channel.CreatedAt),
		}})
	}

	create := func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		// Fastify parses + validates the body before the handler, so both failures
		// outrank anything in the handler itself.
		decoded, bodyErr := readChannelRequestBody(c)
		if bodyErr != nil {
			bodyErr.respond(c)
			return
		}
		body, validationErr := validateChannelBody(decoded, true)
		if validationErr != nil {
			validationErr.respond(c)
			return
		}

		normalized, normalizeError := normalizeGlobalTelegramChannelInput(body)
		if normalizeError != "" || normalized.Name == nil || normalized.BotToken == nil || normalized.ChatID == nil {
			message := "Please provide channel name, Bot Token, and Chat ID"
			if normalizeError != "" {
				message = normalizeError
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": message})
			return
		}

		// `const { enabled = true } = request.body`: an absent key defaults to true,
		// and the schema coerced it to a real boolean already.
		enabled := true
		if body.EnabledPresent && body.Enabled != nil {
			enabled = *body.Enabled
		}

		channelID, err := createGlobalChannel(c, globalChannelCreateData{
			UserID:   user.ID,
			Name:     *normalized.Name,
			BotToken: *normalized.BotToken,
			ChatID:   *normalized.ChatID,
			Enabled:  enabled,
		})
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		userID := int64(user.ID)
		db.CreateLog(c.Request.Context(), &userID, db.LogModuleNotification, "admin.global_channel.create",
			`Created global TG channel "`+*normalized.Name+`" (chatId: `+*normalized.ChatID+`)`,
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusCreated, gin.H{
			"message": "Global notification channel created",
			"channel": gin.H{
				"id":      channelID,
				"name":    *normalized.Name,
				"type":    channelTelegramTypeValue,
				"enabled": enabled,
			},
		})
	}

	update := func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		// Body validation precedes the route-id parse, mirroring Fastify's pipeline.
		decoded, bodyErr := readChannelRequestBody(c)
		if bodyErr != nil {
			bodyErr.respond(c)
			return
		}
		body, validationErr := validateChannelBody(decoded, false)
		if validationErr != nil {
			validationErr.respond(c)
			return
		}

		channelID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": channelInvalidIDMessage})
			return
		}

		existing, err := findGlobalChannel(c, int32(channelID))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if existing == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": channelNotFoundMessage})
			return
		}

		normalized, normalizeError := normalizeGlobalTelegramChannelInput(body)
		if normalizeError != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": normalizeError})
			return
		}

		existingConfig := channelConfigMap(existing.Config)
		var updatedConfig map[string]any
		if normalized.BotToken != nil || normalized.ChatID != nil {
			// `botToken || existingConfig.botToken` — but a nil input means "absent",
			// and an empty string can never reach here (the format check rejects it).
			botToken := existingConfig["botToken"]
			if normalized.BotToken != nil {
				botToken = *normalized.BotToken
			}
			chatID := existingConfig["chatId"]
			if normalized.ChatID != nil {
				chatID = *normalized.ChatID
			}
			updatedConfig = map[string]any{"botToken": botToken, "chatId": chatID}
		}

		if err := updateGlobalChannel(c, int32(channelID), normalized.Name, body.Enabled, updatedConfig); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		userID := int64(user.ID)
		db.CreateLog(c.Request.Context(), &userID, db.LogModuleNotification, "admin.global_channel.update",
			`Updated global TG channel "`+existing.Name+`"`,
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "Channel updated"})
	}

	remove := func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		// The route declares no schema, but Fastify still runs the body parser for
		// DELETE, so a malformed or empty application/json body wins over the id.
		if _, bodyErr := readChannelRequestBody(c); bodyErr != nil {
			bodyErr.respond(c)
			return
		}

		channelID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": channelInvalidIDMessage})
			return
		}

		channel, err := findGlobalChannel(c, int32(channelID))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if channel == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": channelNotFoundMessage})
			return
		}

		if err := deleteGlobalChannel(c, int32(channelID)); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		userID := int64(user.ID)
		db.CreateLog(c.Request.Context(), &userID, db.LogModuleNotification, "admin.global_channel.delete",
			`Deleted global TG channel "`+channel.Name+`"`,
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": "Channel deleted"})
	}

	test := func(c *gin.Context) {
		// Same as DELETE: the body is parsed before the handler runs.
		if _, bodyErr := readChannelRequestBody(c); bodyErr != nil {
			bodyErr.respond(c)
			return
		}

		channelID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": channelInvalidIDMessage})
			return
		}

		channel, err := findGlobalChannel(c, int32(channelID))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if channel == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": channelNotFoundMessage})
			return
		}

		result := lib.TestNotificationChannel(c.Request.Context(), lib.NotificationChannel{
			ID:     channel.ID,
			Type:   channel.Type,
			Config: string(channel.Config),
		})
		if result.Success {
			c.JSON(http.StatusOK, gin.H{"message": "Test notification sent successfully"})
			return
		}

		c.JSON(http.StatusBadRequest, gin.H{"error": channelTestFailedPrefix + result.Error})
	}

	for _, path := range []string{"", "/"} {
		group.GET(path, list)
		group.POST(path, create)
	}
	group.GET("/:id", get)
	group.PATCH("/:id", update)
	group.DELETE("/:id", remove)
	group.POST("/:id/test", test)
}

func init() {
	Register(RegisterAdminNotificationChannels)
}
