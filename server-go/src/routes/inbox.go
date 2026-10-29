// Port of server/src/routes/inbox.ts.
//
// 站内信（in-app inbox）路由。Every route authenticates through the plugin-level
// `onRequest` hook in the TypeScript module, so the Gin group carries
// middleware.Authenticate(). Because Fastify parses (and, where a schema exists,
// validates) the body *before* the handler runs but *after* onRequest, each
// handler reproduces that order: auth (middleware) → body parse → schema
// validation → route-id parsing → the handler's own checks.
//
// Demo safety has two layers, both preserved here:
//  1. the route-level scrub of `login_new_device` messages
//     (shouldRedactDemoLoginRecords → redactDemoInboxMessage), and
//  2. the generic onSend scrub in middleware.DemoRedact.
package routes

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// singleQueryValue returns the parameter's value only when it occurs exactly
// once. Fastify's querystring parser turns `?a=1&a=2` into an array, and every
// consumer in these routes then fails its strict check (RegExp#test stringifies
// to "1,2", and `isRead === 'true'` is false for an array), so a repeated
// parameter is equivalent to "not provided". Go's url.Values.Get would silently
// pick the first value instead, so the multi-valued case is dropped here.
func singleQueryValue(query url.Values, key string) string {
	values := query[key]
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

// inboxTitleMaxLength / inboxContentMaxLength mirror normalizeInboxMessageInput.
const (
	inboxTitleMaxLength   = 200
	inboxContentMaxLength = 5000
)

// inboxQueryPage clamps the page for the query layer while the response keeps the
// value the route parsed (the TypeScript handler passes it through unchanged).
// A JavaScript-safe page combined with pageSize<=100 always fits a 64-bit offset,
// so the clamp never fires on 64-bit builds and cannot perturb the diff.
func inboxQueryPage(page int64, pageSize int64) int {
	if pageSize <= 0 {
		return int(page)
	}
	maxPage := int64(math.MaxInt)/pageSize + 1
	if page > maxPage {
		return int(maxPage)
	}
	return int(page)
}

// Inbox-local error payloads. The TypeScript routes bypass apiError() for these
// and send `{ error, message }` (no `code`), so they are plain strings here.
const (
	inboxTitleRequiredCode     = "TITLE_REQUIRED"
	inboxTitleRequiredMessage  = "Title is required"
	inboxContentRequiredCode   = "CONTENT_REQUIRED"
	inboxContentRequiredMsg    = "Content is required"
	inboxTitleTooLongCode      = "TITLE_TOO_LONG"
	inboxTitleTooLongMessage   = "Title is too long"
	inboxContentTooLongCode    = "CONTENT_TOO_LONG"
	inboxContentTooLongMessage = "Content is too long"

	inboxCannotMessageSelfCode    = "CANNOT_MESSAGE_SELF"
	inboxCannotMessageSelfMessage = "Cannot send message to yourself"
	inboxInstanceDeletedCode      = "INSTANCE_DELETED"
	inboxInstanceDeletedMessage   = "Instance has been deleted"
	inboxSMTPNotConfiguredCode    = "SMTP_NOT_CONFIGURED"
	inboxSMTPNotConfiguredMessage = "SMTP is not configured"
)

// inboxEventTypes mirror the literal eventType strings persisted by the routes.
const (
	inboxEventSystemAnnouncement = "system_announcement"
	inboxEventHostAnnouncement   = "host_announcement"
	inboxEventAdminMessage       = "admin_message"
	inboxEventHostMessage        = "host_message"
	inboxEventLoginNewDevice     = "login_new_device"
)

// ==================== 输入解析 ====================

// inboxMessageError is the `{ error, message }` shape returned by
// normalizeInboxMessageInput.
type inboxMessageError struct {
	Code    string
	Message string
}

// inboxNormalizedMessage mirrors the successful branch of
// normalizeInboxMessageInput.
type inboxNormalizedMessage struct {
	Title   string
	Content string
}

// inboxJSRuneIsSpace mirrors ECMAScript WhiteSpace + LineTerminator, which is
// what String.prototype.trim removes. Go's unicode.IsSpace differs slightly
// (notably it omits U+FEFF), so the set is spelled out.
func inboxJSRuneIsSpace(char rune) bool {
	switch char {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0x1680,
		0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return char >= 0x2000 && char <= 0x200A
}

// inboxJSTrim mirrors String.prototype.trim.
func inboxJSTrim(value string) string {
	return strings.TrimFunc(value, inboxJSRuneIsSpace)
}

// inboxNormalizeMessageInput mirrors normalizeInboxMessageInput: the body must be
// a plain object, title/content must be non-blank strings, and the trimmed values
// must stay inside the length budget (measured in UTF-16 code units, like
// String#length).
func inboxNormalizeMessageInput(body any) (inboxNormalizedMessage, *inboxMessageError) {
	object, ok := body.(map[string]any)
	if !ok {
		return inboxNormalizedMessage{}, &inboxMessageError{
			Code:    inboxTitleRequiredCode,
			Message: inboxTitleRequiredMessage,
		}
	}

	rawTitle, okTitle := object["title"].(string)
	if !okTitle || inboxJSTrim(rawTitle) == "" {
		return inboxNormalizedMessage{}, &inboxMessageError{
			Code:    inboxTitleRequiredCode,
			Message: inboxTitleRequiredMessage,
		}
	}
	rawContent, okContent := object["content"].(string)
	if !okContent || inboxJSTrim(rawContent) == "" {
		return inboxNormalizedMessage{}, &inboxMessageError{
			Code:    inboxContentRequiredCode,
			Message: inboxContentRequiredMsg,
		}
	}

	trimmedTitle := inboxJSTrim(rawTitle)
	trimmedContent := inboxJSTrim(rawContent)
	if jsStringLength(trimmedTitle) > inboxTitleMaxLength {
		return inboxNormalizedMessage{}, &inboxMessageError{
			Code:    inboxTitleTooLongCode,
			Message: inboxTitleTooLongMessage,
		}
	}
	if jsStringLength(trimmedContent) > inboxContentMaxLength {
		return inboxNormalizedMessage{}, &inboxMessageError{
			Code:    inboxContentTooLongCode,
			Message: inboxContentTooLongMessage,
		}
	}

	return inboxNormalizedMessage{Title: trimmedTitle, Content: trimmedContent}, nil
}

// inboxParsePositiveInteger mirrors parsePositiveInteger: the strict
// ^[1-9]\d*$ pattern plus Number.isSafeInteger, otherwise the fallback.
func inboxParsePositiveInteger(value string, fallback int64) int64 {
	if parsed, ok := lib.ParsePositiveRouteID(value); ok {
		return parsed
	}
	return fallback
}

// inboxParsePageSize mirrors `Math.min(100, parsePositiveInteger(pageSize, 20))`.
func inboxParsePageSize(value string) int64 {
	size := inboxParsePositiveInteger(value, 20)
	if size > 100 {
		size = 100
	}
	return size
}

// inboxParseRouteID mirrors parsePositiveId for a path parameter. Values above
// int32 cannot be persisted ids (PostgreSQL int4), so they are rejected up front
// rather than silently truncated.
func inboxParseRouteID(value string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok || parsed > math.MaxInt32 {
		return 0, false
	}
	return int32(parsed), true
}

// inboxParsePositiveIDArray mirrors parsePositiveIdArray: every element must be a
// safe positive integer, duplicates are dropped preserving first-seen order, and
// an absent array stays nil.
func inboxParsePositiveIDArray(values []float64, present bool) ([]int32, bool) {
	if !present {
		return nil, true
	}

	ids := make([]int32, 0, len(values))
	seen := map[int32]bool{}
	for _, item := range values {
		if math.IsNaN(item) || math.IsInf(item, 0) || item != math.Trunc(item) || item <= 0 {
			return nil, false
		}
		// Number.isSafeInteger(item) — anything above 2^53-1 is not a safe integer
		// in JavaScript; values above int32 cannot be row ids either.
		if item > float64(maxSafeIntegerJS) || item > math.MaxInt32 {
			return nil, false
		}
		id := int32(item)
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, true
}

// maxSafeIntegerJS mirrors Number.MAX_SAFE_INTEGER.
const maxSafeIntegerJS = 1<<53 - 1

// ==================== /hosts/:hostId/notify schema ====================

// inboxNotifyBody is the validated `POST /hosts/:hostId/notify` payload.
type inboxNotifyBody struct {
	InstanceIDs    []float64
	InstanceIDsSet bool
	SendEmail      bool
}

// inboxValidateNotifyBody reproduces the Fastify body schema of the host
// notification route with @fastify/ajv-compiler defaults (coerceTypes:'array',
// useDefaults:true, allErrors:false, removeAdditional:true — the last one is
// inert because the schema has no additionalProperties:false). Keyword order is
// AJV's: root type → required → per property (type → maxLength → minLength →
// maxItems → minItems → items).
func inboxValidateNotifyBody(decoded any) (inboxNotifyBody, string) {
	var out inboxNotifyBody

	object, ok := decoded.(map[string]any)
	if !ok {
		return out, "must be object"
	}

	// `required` is evaluated before the properties, in schema order.
	if _, present := object["title"]; !present {
		return out, "title is required"
	}
	if _, present := object["content"]; !present {
		return out, "content is required"
	}

	if value, present := object["title"]; present {
		text, message := ajvCoerceString(value)
		if message != "" {
			return out, message
		}
		if jsStringLength(text) > inboxTitleMaxLength {
			return out, "title is too long"
		}
		if jsStringLength(text) < 1 {
			return out, "title is too short"
		}
	}

	if value, present := object["content"]; present {
		text, message := ajvCoerceString(value)
		if message != "" {
			return out, message
		}
		if jsStringLength(text) > inboxContentMaxLength {
			return out, "content is too long"
		}
		if jsStringLength(text) < 1 {
			return out, "content is too short"
		}
	}

	if value, present := object["instanceIds"]; present {
		// coerceTypes:'array' wraps a non-array scalar (string/number/boolean/null)
		// into a one-element array before the array keywords run.
		array, isArray := value.([]any)
		if !isArray {
			switch value.(type) {
			case nil, string, float64, bool:
				array = []any{value}
			default:
				return out, "must be array"
			}
		}
		if len(array) > 100 {
			return out, "must NOT have more than 100 items"
		}
		if len(array) < 1 {
			return out, "must NOT have fewer than 1 items"
		}
		ids := make([]float64, 0, len(array))
		for _, item := range array {
			number, ok := ajvCoerceInteger(item)
			if !ok {
				return out, "must be integer"
			}
			if number < 1 {
				return out, "must be >= 1"
			}
			ids = append(ids, number)
		}
		out.InstanceIDs = ids
		out.InstanceIDsSet = true
	}

	if value, present := object["sendEmail"]; present {
		flag, ok := ajvCoerceBoolean(value)
		if !ok {
			return out, "must be boolean"
		}
		out.SendEmail = flag
	}

	return out, ""
}

// ==================== demo 脱敏 ====================

// The login-notification scrubbers live in lib/demo-safety.ts but were not part
// of the Go lib port (only RedactDemoJSONPayload/IsDemoRequest/IsDemoAccount
// were), so they are reproduced here verbatim.
var (
	inboxLoginControlCharsPattern = regexp.MustCompile("[\u0001\u0002]")
	// inboxJSSpaceClass is ECMAScript's `\s` (Go's is narrower).
	inboxJSSpaceClass       = `[\t\n\v\f\r \x{00A0}\x{1680}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}\x{2000}-\x{200A}]`
	inboxLoginDevicePattern = regexp.MustCompile(`(^|\n)` + inboxJSSpaceClass + `*设备:[^\n]*(\n|$)`)
	inboxLoginIPPattern     = regexp.MustCompile(`(^|\n)` + inboxJSSpaceClass + `*IP:[^\n]*(\n|$)`)
	inboxBlankLinesPattern  = regexp.MustCompile(`\n{3,}`)
)

// inboxRedactLoginNotificationContent mirrors
// redactDemoLoginNotificationContent.
func inboxRedactLoginNotificationContent(content string) string {
	content = inboxLoginControlCharsPattern.ReplaceAllString(content, "\n")
	content = inboxLoginDevicePattern.ReplaceAllString(content, "${1}")
	content = inboxLoginIPPattern.ReplaceAllString(content, "${1}IP: "+lib.DemoRedactedIP+"${2}")
	content = inboxBlankLinesPattern.ReplaceAllString(content, "\n\n")
	return inboxJSTrim(content)
}

// inboxSpreadToObject mirrors object spread of a decoded JSON value.
func inboxSpreadToObject(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed)+2)
		for key, entry := range typed {
			out[key] = entry
		}
		return out
	case []any:
		out := make(map[string]any, len(typed)+2)
		for index, entry := range typed {
			out[strconv.Itoa(index)] = entry
		}
		return out
	default:
		return map[string]any{}
	}
}

// inboxRedactLoginNotificationData mirrors redactDemoLoginNotificationData,
// including the `if (!data) return data` short-circuit.
func inboxRedactLoginNotificationData(data any) any {
	switch typed := data.(type) {
	case nil:
		return nil
	case bool:
		if !typed {
			return data
		}
	case float64:
		if typed == 0 {
			return data
		}
	case string:
		if typed == "" {
			return data
		}
	}

	redacted := inboxSpreadToObject(data)
	redacted["deviceInfo"] = nil
	redacted["ipAddress"] = lib.DemoRedactedIP
	return redacted
}

// inboxRedactDemoMessage mirrors redactDemoInboxMessage: only new-device login
// notifications are rewritten.
func inboxRedactDemoMessage(message db.InboxMessage) db.InboxMessage {
	if message.EventType != inboxEventLoginNewDevice {
		return message
	}
	message.Content = inboxRedactLoginNotificationContent(message.Content)
	message.Data = inboxRedactLoginNotificationData(message.Data)
	return message
}

// inboxShouldRedactDemoLoginRecords mirrors shouldRedactDemoLoginRecords =
// shouldProtectDemoAccount = isDemoRequest && isDemoAccount. The JWT payload has
// no email claim, so only the username can match (exactly like Node, where
// request.user.email is undefined).
func inboxShouldRedactDemoLoginRecords(c *gin.Context, user *lib.AccessTokenClaims) bool {
	if user == nil {
		return false
	}
	return lib.IsDemoRequest(c.GetHeader("X-Forwarded-Host"), c.Request.Host) &&
		lib.IsDemoAccount(user.Username, "")
}

// ==================== DB helpers (thin projections) ====================

func inboxGetUser(ctx context.Context, id int32) (*models.User, error) {
	var user models.User
	err := db.DB.WithContext(ctx).Select("id", "username", "email").First(&user, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func inboxGetHost(ctx context.Context, id int32) (*models.Host, error) {
	var host models.Host
	err := db.DB.WithContext(ctx).Select("id", "user_id", "name").First(&host, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &host, nil
}

func inboxGetInstance(ctx context.Context, id int32) (*models.Instance, error) {
	var instance models.Instance
	err := db.DB.WithContext(ctx).
		Select("id", "name", "user_id", "host_id", "status").
		First(&instance, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &instance, nil
}

// inboxCreateAnnouncement mirrors announcementsDb.createAnnouncement, which is
// not part of the ported db/announcements.go API. createdAt is the Prisma column
// default (now()), so it is set explicitly — GORM would otherwise insert the zero
// timestamp.
func inboxCreateAnnouncement(ctx context.Context, announcement models.Announcement) error {
	announcement.CreatedAt = time.Now()
	return db.DB.WithContext(ctx).Create(&announcement).Error
}

// ==================== host notification email queue ====================

// Mirrors server/src/db/host-notification-email-tasks.ts constants.
const (
	hostNotificationEmailInterval             = 60 * time.Second
	hostNotificationEmailLockRetryLimit       = 20
	hostNotificationEmailLockRetryDelay       = 100 * time.Millisecond
	hostNotificationEmailTaskDefaultStatus    = models.HostNotificationEmailTaskStatusPENDING
	hostNotificationEmailTaskInsertColumnList = "(user_id, host_id, email, username, host_name, title, content, status, retry_count, scheduled_for, created_at)"
)

// inboxHostNotificationEmailTask mirrors EnqueueHostNotificationEmailTaskInput.
type inboxHostNotificationEmailTask struct {
	UserID   int32
	HostID   int32
	Email    string
	Username string
	HostName string
	Title    string
	Content  string
}

// inboxEnqueueHostNotificationEmailTasks mirrors
// enqueueHostNotificationEmailTasks: serialize on the
// HOST_NOTIFICATION_EMAIL_LOCK advisory lock, space the new jobs one minute apart
// after the latest queued/attempted job, and retry the lock up to 20 times.
func inboxEnqueueHostNotificationEmailTasks(ctx context.Context, tasks []inboxHostNotificationEmailTask) (int64, error) {
	if len(tasks) == 0 {
		return 0, nil
	}

	pool := db.Pool()
	if pool == nil {
		return 0, errors.New("database not initialized")
	}

	for attempt := 0; attempt < hostNotificationEmailLockRetryLimit; attempt++ {
		queued, locked, err := inboxTryEnqueue(ctx, tasks)
		if err != nil {
			return 0, err
		}
		if locked {
			return queued, nil
		}

		delay := time.Duration(attempt+1) * hostNotificationEmailLockRetryDelay
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delay):
		}
	}

	return 0, errors.New("Failed to acquire host notification email queue lock")
}

func inboxTryEnqueue(ctx context.Context, tasks []inboxHostNotificationEmailTask) (int64, bool, error) {
	pool := db.Pool()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := db.TryAdvisoryTransactionLock(ctx, tx, db.HostNotificationEmailLockNamespace, db.HostNotificationEmailLockKey)
	if err != nil {
		return 0, false, err
	}
	if !locked {
		return 0, false, nil
	}

	now := time.Now()

	baseTime := now
	var latestQueued time.Time
	err = tx.QueryRow(ctx,
		`SELECT scheduled_for FROM host_notification_email_tasks
		 WHERE status IN ('PENDING', 'PROCESSING')
		 ORDER BY scheduled_for DESC, id DESC LIMIT 1`,
	).Scan(&latestQueued)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}
	if err == nil {
		if candidate := latestQueued.Add(hostNotificationEmailInterval); candidate.After(baseTime) {
			baseTime = candidate
		}
	}

	var latestStarted time.Time
	err = tx.QueryRow(ctx,
		`SELECT started_at FROM host_notification_email_tasks
		 WHERE started_at IS NOT NULL
		 ORDER BY started_at DESC, id DESC LIMIT 1`,
	).Scan(&latestStarted)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}
	if err == nil {
		if candidate := latestStarted.Add(hostNotificationEmailInterval); candidate.After(baseTime) {
			baseTime = candidate
		}
	}

	var inserted int64
	for index, task := range tasks {
		scheduledFor := baseTime.Add(time.Duration(index) * hostNotificationEmailInterval)
		tag, execErr := tx.Exec(ctx,
			`INSERT INTO host_notification_email_tasks `+hostNotificationEmailTaskInsertColumnList+`
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, $9, $10)`,
			task.UserID, task.HostID, task.Email, task.Username, task.HostName,
			task.Title, task.Content, string(hostNotificationEmailTaskDefaultStatus),
			scheduledFor, now,
		)
		if execErr != nil {
			return 0, false, execErr
		}
		inserted += tag.RowsAffected()
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, false, err
	}
	return inserted, true, nil
}

// ==================== handlers ====================

// inboxEmailSummary mirrors the emailSummary object returned by the host
// notification route.
type inboxEmailSummary struct {
	Requested    bool   `json:"requested"`
	Mode         string `json:"mode"`
	SentCount    int    `json:"sentCount"`
	QueuedCount  int64  `json:"queuedCount"`
	SkippedCount int    `json:"skippedCount"`
	FailedCount  int    `json:"failedCount"`
}

func inboxInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

func inboxRespondMessageError(c *gin.Context, inputError *inboxMessageError) {
	c.JSON(http.StatusBadRequest, gin.H{"error": inputError.Code, "message": inputError.Message})
}

// inboxReadBody mirrors the Fastify body-parsing phase. It returns ok=false after
// writing the response.
func inboxReadBody(c *gin.Context) (any, bool) {
	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return nil, false
	}
	return body.Value, true
}

// RegisterInbox mounts /api/inbox. Every route requires authentication, mirroring
// the plugin-level onRequest hook.
func RegisterInbox(r *gin.Engine) {
	group := r.Group("/api/inbox", middleware.Authenticate())

	// GET /api/inbox — Fastify's prefixTrailingSlash 'both' also serves the slash.
	listMessages := func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		query := c.Request.URL.Query()

		// 只接受显式的 'true'/'false'，其它值等同于未传。
		var isReadFilter *bool
		switch singleQueryValue(query, "isRead") {
		case "true":
			value := true
			isReadFilter = &value
		case "false":
			value := false
			isReadFilter = &value
		}

		page := inboxParsePositiveInteger(singleQueryValue(query, "page"), 1)
		pageSize := inboxParsePageSize(singleQueryValue(query, "pageSize"))

		result, err := db.GetInboxMessages(c.Request.Context(), user.ID, db.InboxListOptions{
			Page:     inboxQueryPage(page, pageSize),
			PageSize: int(pageSize),
			IsRead:   isReadFilter,
		})
		if err != nil {
			inboxInternalError(c)
			return
		}
		// The DB layer may have clamped the page for the query; the response echoes
		// the values the route parsed, like the TypeScript handler.
		result.Page = int(page)
		result.PageSize = int(pageSize)

		if inboxShouldRedactDemoLoginRecords(c, user) {
			for index := range result.Messages {
				result.Messages[index] = inboxRedactDemoMessage(result.Messages[index])
			}
		}

		c.JSON(http.StatusOK, result)
	}
	group.GET("", listMessages)
	group.GET("/", listMessages)

	// GET /api/inbox/unread-count
	group.GET("/unread-count", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		count, err := db.GetUnreadCount(c.Request.Context(), user.ID)
		if err != nil {
			inboxInternalError(c)
			return
		}
		c.JSON(http.StatusOK, gin.H{"count": count})
	})

	// POST /api/inbox/:id/read
	group.POST("/:id/read", func(c *gin.Context) {
		if _, ok := inboxReadBody(c); !ok {
			return
		}
		user := middleware.CurrentUser(c)

		messageID, ok := inboxParseRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		success, err := db.MarkAsRead(c.Request.Context(), messageID, user.ID)
		if err != nil {
			inboxInternalError(c)
			return
		}
		if !success {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrMessageNotFound, "")
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	// POST /api/inbox/read-all
	group.POST("/read-all", func(c *gin.Context) {
		if _, ok := inboxReadBody(c); !ok {
			return
		}
		user := middleware.CurrentUser(c)

		count, err := db.MarkAllAsRead(c.Request.Context(), user.ID)
		if err != nil {
			inboxInternalError(c)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "count": count})
	})

	// DELETE /api/inbox/:id
	group.DELETE("/:id", func(c *gin.Context) {
		if _, ok := inboxReadBody(c); !ok {
			return
		}
		user := middleware.CurrentUser(c)

		messageID, ok := inboxParseRouteID(c.Param("id"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		success, err := db.DeleteMessage(c.Request.Context(), messageID, user.ID)
		if err != nil {
			inboxInternalError(c)
			return
		}
		if !success {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrMessageNotFound, "")
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	// DELETE /api/inbox/read — 删除所有已读消息
	group.DELETE("/read", func(c *gin.Context) {
		if _, ok := inboxReadBody(c); !ok {
			return
		}
		user := middleware.CurrentUser(c)

		count, err := db.DeleteReadMessages(c.Request.Context(), user.ID)
		if err != nil {
			inboxInternalError(c)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "count": count})
	})

	// POST /api/inbox/admin/broadcast — 管理员发送全站站内信
	group.POST("/admin/broadcast", func(c *gin.Context) {
		body, ok := inboxReadBody(c)
		if !ok {
			return
		}
		user := middleware.CurrentUser(c)

		if user.Role != "admin" {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		input, inputError := inboxNormalizeMessageInput(body)
		if inputError != nil {
			inboxRespondMessageError(c, inputError)
			return
		}

		ctx := c.Request.Context()
		userIDs, err := db.GetAllActiveUserIds(ctx)
		if err != nil {
			inboxInternalError(c)
			return
		}
		if len(userIDs) == 0 {
			c.JSON(http.StatusOK, gin.H{"success": true, "count": 0})
			return
		}

		count, err := db.CreateBulkMessages(ctx, db.CreateBulkInboxMessagesData{
			UserIDs:   userIDs,
			EventType: inboxEventSystemAnnouncement,
			Title:     input.Title,
			Content:   input.Content,
			Data:      map[string]any{"senderId": user.ID},
		})
		if err != nil {
			inboxInternalError(c)
			return
		}

		if err := inboxCreateAnnouncement(ctx, models.Announcement{
			Type:           models.AnnouncementTypeSystemBroadcast,
			SenderID:       user.ID,
			Title:          input.Title,
			Content:        input.Content,
			RecipientCount: int32(count),
		}); err != nil {
			inboxInternalError(c)
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "count": count})
	})

	// POST /api/inbox/hosts/:hostId/notify — 节点所有者通知实例用户
	group.POST("/hosts/:hostId/notify", func(c *gin.Context) {
		body, ok := inboxReadBody(c)
		if !ok {
			return
		}

		// Fastify validates the body schema before the handler runs.
		notifyBody, validationMessage := inboxValidateNotifyBody(body)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		user := middleware.CurrentUser(c)
		hostID, ok := inboxParseRouteID(c.Param("hostId"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}
		instanceIDs, ok := inboxParsePositiveIDArray(notifyBody.InstanceIDs, notifyBody.InstanceIDsSet)
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		ctx := c.Request.Context()
		host, err := inboxGetHost(ctx, hostID)
		if err != nil {
			inboxInternalError(c)
			return
		}
		if host == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
			return
		}

		if host.UserID != user.ID && user.Role != "admin" {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		input, inputError := inboxNormalizeMessageInput(body)
		if inputError != nil {
			inboxRespondMessageError(c, inputError)
			return
		}

		recipients, err := db.GetUniqueRecipientsByHostId(ctx, hostID, instanceIDs)
		if err != nil {
			inboxInternalError(c)
			return
		}

		userIDs := make([]int32, 0, len(recipients))
		emailRecipients := make([]db.HostNotificationRecipient, 0, len(recipients))
		for _, recipient := range recipients {
			userIDs = append(userIDs, recipient.UserID)
		}
		if notifyBody.SendEmail {
			for _, recipient := range recipients {
				if recipient.Email != nil && strings.TrimSpace(*recipient.Email) != "" {
					emailRecipients = append(emailRecipients, recipient)
				}
			}
		}

		if notifyBody.SendEmail && len(emailRecipients) > 0 {
			smtpEnabled, err := lib.IsSMTPEnabled(ctx)
			if err != nil {
				inboxInternalError(c)
				return
			}
			if !smtpEnabled {
				c.JSON(http.StatusBadRequest, gin.H{
					"error":   inboxSMTPNotConfiguredCode,
					"message": inboxSMTPNotConfiguredMessage,
				})
				return
			}
		}

		if len(userIDs) == 0 {
			if notifyBody.SendEmail {
				c.JSON(http.StatusOK, gin.H{
					"success": true,
					"count":   0,
					"email": inboxEmailSummary{
						Requested:    true,
						Mode:         "none",
						SentCount:    0,
						QueuedCount:  0,
						SkippedCount: 0,
						FailedCount:  0,
					},
				})
				return
			}
			c.JSON(http.StatusOK, gin.H{"success": true, "count": 0})
			return
		}

		messageContent := input.Content + "\n\n来自节点：" + host.Name
		count, err := db.CreateBulkMessages(ctx, db.CreateBulkInboxMessagesData{
			UserIDs:   userIDs,
			EventType: inboxEventHostAnnouncement,
			Title:     input.Title,
			Content:   messageContent,
			Data: map[string]any{
				"hostId":   hostID,
				"hostName": host.Name,
				"senderId": user.ID,
			},
		})
		if err != nil {
			inboxInternalError(c)
			return
		}

		if err := inboxCreateAnnouncement(ctx, models.Announcement{
			Type:           models.AnnouncementTypeHostBroadcast,
			SenderID:       user.ID,
			Title:          input.Title,
			Content:        input.Content,
			RecipientCount: int32(count),
			HostID:         &hostID,
		}); err != nil {
			inboxInternalError(c)
			return
		}

		if !notifyBody.SendEmail {
			c.JSON(http.StatusOK, gin.H{"success": true, "count": count})
			return
		}

		summary := inboxEmailSummary{
			Requested:    true,
			Mode:         "none",
			SkippedCount: len(recipients) - len(emailRecipients),
		}

		if len(recipients) == 1 && len(emailRecipients) == 1 {
			recipient := emailRecipients[0]
			summary.Mode = "direct"

			email := strings.TrimSpace(*recipient.Email)
			result := lib.SendHostAnnouncementEmail(ctx, email, lib.HostAnnouncementEmailData{
				Username: recipient.Username,
				HostName: host.Name,
				Title:    input.Title,
				Content:  input.Content,
			})
			if result.Success {
				summary.SentCount = 1
			} else {
				summary.FailedCount = 1
				log.Printf("[Inbox] Failed to send direct host announcement email to user %d: %s", recipient.UserID, result.Error)
			}
		} else if len(emailRecipients) > 0 {
			summary.Mode = "queued"

			tasks := make([]inboxHostNotificationEmailTask, 0, len(emailRecipients))
			for _, recipient := range emailRecipients {
				tasks = append(tasks, inboxHostNotificationEmailTask{
					UserID:   recipient.UserID,
					HostID:   hostID,
					Email:    strings.TrimSpace(*recipient.Email),
					Username: recipient.Username,
					HostName: host.Name,
					Title:    input.Title,
					Content:  input.Content,
				})
			}

			queued, err := inboxEnqueueHostNotificationEmailTasks(ctx, tasks)
			if err != nil {
				summary.FailedCount = len(emailRecipients)
				log.Printf("[Inbox] Failed to enqueue host announcement emails for host %d: %v", hostID, err)
			} else {
				summary.QueuedCount = queued
				summary.FailedCount = len(emailRecipients) - int(queued)
				if summary.FailedCount < 0 {
					summary.FailedCount = 0
				}
			}
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "count": count, "email": summary})
	})

	// POST /api/inbox/admin/send/:userId — 管理员发送站内信给特定用户
	group.POST("/admin/send/:userId", func(c *gin.Context) {
		body, ok := inboxReadBody(c)
		if !ok {
			return
		}
		user := middleware.CurrentUser(c)

		if user.Role != "admin" {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		targetUserID, ok := inboxParseRouteID(c.Param("userId"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		ctx := c.Request.Context()
		targetUser, err := inboxGetUser(ctx, targetUserID)
		if err != nil {
			inboxInternalError(c)
			return
		}
		if targetUser == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
			return
		}

		if targetUserID == user.ID {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   inboxCannotMessageSelfCode,
				"message": inboxCannotMessageSelfMessage,
			})
			return
		}

		input, inputError := inboxNormalizeMessageInput(body)
		if inputError != nil {
			inboxRespondMessageError(c, inputError)
			return
		}

		if _, err := db.CreateInboxMessage(ctx, db.CreateInboxMessageData{
			UserID:    targetUserID,
			EventType: inboxEventAdminMessage,
			Title:     input.Title,
			Content:   input.Content,
			Data:      map[string]any{"senderId": user.ID},
		}); err != nil {
			inboxInternalError(c)
			return
		}

		if err := inboxCreateAnnouncement(ctx, models.Announcement{
			Type:           models.AnnouncementTypeAdminMessage,
			SenderID:       user.ID,
			Title:          input.Title,
			Content:        input.Content,
			RecipientCount: 1,
			TargetUserID:   &targetUserID,
		}); err != nil {
			inboxInternalError(c)
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	})

	// POST /api/inbox/instances/:instanceId/notify — 宿主机所有者通知实例用户
	group.POST("/instances/:instanceId/notify", func(c *gin.Context) {
		body, ok := inboxReadBody(c)
		if !ok {
			return
		}
		user := middleware.CurrentUser(c)

		instanceID, ok := inboxParseRouteID(c.Param("instanceId"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		ctx := c.Request.Context()
		instance, err := inboxGetInstance(ctx, instanceID)
		if err != nil {
			inboxInternalError(c)
			return
		}
		if instance == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
			return
		}

		if instance.Status == models.InstanceStatusDeleted {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   inboxInstanceDeletedCode,
				"message": inboxInstanceDeletedMessage,
			})
			return
		}

		host, err := inboxGetHost(ctx, instance.HostID)
		if err != nil {
			inboxInternalError(c)
			return
		}
		if host == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
			return
		}

		if host.UserID != user.ID && user.Role != "admin" {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}

		if instance.UserID == user.ID {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   inboxCannotMessageSelfCode,
				"message": inboxCannotMessageSelfMessage,
			})
			return
		}

		input, inputError := inboxNormalizeMessageInput(body)
		if inputError != nil {
			inboxRespondMessageError(c, inputError)
			return
		}

		messageContent := input.Content + "\n\n来自节点：" + host.Name
		if _, err := db.CreateInboxMessage(ctx, db.CreateInboxMessageData{
			UserID:    instance.UserID,
			EventType: inboxEventHostMessage,
			Title:     input.Title,
			Content:   messageContent,
			Data: map[string]any{
				"hostId":       host.ID,
				"hostName":     host.Name,
				"instanceId":   instance.ID,
				"instanceName": instance.Name,
				"senderId":     user.ID,
			},
		}); err != nil {
			inboxInternalError(c)
			return
		}

		targetUserID := instance.UserID
		if err := inboxCreateAnnouncement(ctx, models.Announcement{
			Type:           models.AnnouncementTypeHostMessage,
			SenderID:       user.ID,
			Title:          input.Title,
			Content:        input.Content,
			RecipientCount: 1,
			HostID:         &host.ID,
			TargetUserID:   &targetUserID,
			InstanceID:     &instance.ID,
		}); err != nil {
			inboxInternalError(c)
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	})
}

func init() {
	Register(RegisterInbox)
}
