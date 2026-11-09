// Port of server/src/routes/tickets.ts (plus the request-parsing half of
// server/src/lib/ticket-attachments.ts, which that route depends on).
//
// Secure-by-default points that are pinned by scripts/test-ticket-image-security.ts
// and must survive this port:
//
//   - the attachment proxy never follows redirects (`redirect: 'manual'`), runs
//     every URL through the SSRF guard (lib.AssertSafeHTTPURL), applies a hard
//     50MB streamed byte cap, and only ever serves MIME types from the
//     image/* allowlist (upstream type first, then the stored type);
//   - it always sets `X-Content-Type-Options: nosniff` and a private cache header;
//   - ticket create/reply body caps are enforced on JavaScript string length
//     (UTF-16 units), matching `subject.length`;
//   - route ids are parsed with lib.ParsePositiveRouteID, never a bare Atoi.
//
// The image-bed module (server/src/lib/lsky.ts) is a separate port that has not
// landed, so uploads/deletes go through the TicketsImageUploader /
// TicketsImageCleaner seams below. Their defaults reproduce the "Lsky image bed
// is not configured" behaviour of an unconfigured deployment, which is exactly
// what the TypeScript produces when ticket_image_lsky_* are empty (the seeded
// defaults), and the error is a handled payload error → 400 INVALID_PARAMS.
package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== ticket-attachments.ts constants ====================

const (
	ticketsMaxImages       = 6
	ticketsMaxImageSize    = 50 * 1024 * 1024
	ticketsUploadBodyLimit = (ticketsMaxImages * ticketsMaxImageSize) + (4 * 1024 * 1024)

	// ticketsMultipartMaxParts mirrors the `limits.parts: 30` registered in app.ts.
	ticketsMultipartMaxParts = 30
	// ticketsMultipartFieldSize mirrors busboy's default fieldSize (1MB).
	ticketsMultipartFieldSize = 1024 * 1024

	ticketsProxyFetchTimeout = 15 * time.Second
)

var ticketsAllowedImageMimeTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
	"image/gif":  true,
	"image/avif": true,
}

var ticketsValidStatuses = map[string]bool{
	string(models.TicketStatusOpen):       true,
	string(models.TicketStatusInProgress): true,
	string(models.TicketStatusResolved):   true,
	string(models.TicketStatusClosed):     true,
}

var ticketsValidPriorities = map[string]bool{
	string(models.TicketPriorityLow):    true,
	string(models.TicketPriorityNormal): true,
	string(models.TicketPriorityHigh):   true,
	string(models.TicketPriorityUrgent): true,
}

var ticketsValidCategories = map[string]bool{
	"general":   true,
	"billing":   true,
	"technical": true,
	"abuse":     true,
}

var ticketsValidSourceTypes = map[string]bool{"all": true, "user": true, "official": true}

var ticketsValidQueues = map[string]bool{
	"pending":          true,
	"due_soon":         true,
	"overdue":          true,
	"waiting_user":     true,
	"waiting_internal": true,
}

// ==================== image-bed seams ====================

// TicketsImageInput mirrors LskyTicketImageUploadInput.
type TicketsImageInput struct {
	Buffer      []byte
	Filename    string
	ContentType string
	SizeBytes   int64
}

// TicketsImageUploader mirrors uploadTicketImageToLsky. Default: the image bed is
// not configured. The Lsky port should replace this from its init().
var TicketsImageUploader = func(ctx context.Context, image TicketsImageInput) (db.TicketsAttachmentData, error) {
	return db.TicketsAttachmentData{}, errors.New("Lsky image bed is not configured")
}

// TicketsImageCleaner mirrors deleteTicketImageFromLsky. cleanupUploadedTicketImages
// uses Promise.allSettled, so failures are dropped by design.
var TicketsImageCleaner = func(ctx context.Context, providerVersion string, providerFileID *string) error {
	return nil
}

// TicketsRegisterImageBed wires the Lsky implementation in.
func TicketsRegisterImageBed(
	upload func(ctx context.Context, image TicketsImageInput) (db.TicketsAttachmentData, error),
	cleanup func(ctx context.Context, providerVersion string, providerFileID *string) error,
) {
	if upload != nil {
		TicketsImageUploader = upload
	}
	if cleanup != nil {
		TicketsImageCleaner = cleanup
	}
}

// ==================== JS value helpers ====================

// ticketsJSTrim mirrors String#trim(): JavaScript also strips U+FEFF, which
// unicode.IsSpace does not.
func ticketsJSTrim(value string) string {
	return strings.Trim(strings.TrimSpace(value), "\uFEFF")
}

// ticketsSanitizeContent mirrors sanitizeContent(): non-strings become ”.
func ticketsSanitizeContent(value *string) string {
	if value == nil {
		return ""
	}
	return ticketsJSTrim(*value)
}

// ticketsStringValue mirrors String(value) for a decoded JSON value.
func ticketsStringValue(value any) (string, bool) {
	switch typed := value.(type) {
	case nil:
		return "", false
	case string:
		return typed, true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		return formatTerminalCommandNumber(typed), true
	case map[string]any:
		return "[object Object]", true
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := ticketsStringValue(item)
			if !ok {
				text = ""
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, ","), true
	default:
		return fmt.Sprint(typed), true
	}
}

// ticketsJSToNumber mirrors JavaScript's unary plus for strings.
func ticketsJSToNumber(raw string) (float64, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, true // +"" === 0
	}

	sign := 1.0
	unsigned := trimmed
	if strings.HasPrefix(unsigned, "+") {
		unsigned = unsigned[1:]
	} else if strings.HasPrefix(unsigned, "-") {
		sign = -1
		unsigned = unsigned[1:]
	}

	switch {
	case strings.EqualFold(unsigned, "Infinity"):
		return sign * math.Inf(1), true
	case strings.HasPrefix(unsigned, "0x") || strings.HasPrefix(unsigned, "0X"):
		if sign < 0 || unsigned != trimmed {
			return 0, false
		}
		parsed, err := strconv.ParseUint(unsigned[2:], 16, 64)
		if err != nil {
			return 0, false
		}
		return float64(parsed), true
	case strings.HasPrefix(unsigned, "0b") || strings.HasPrefix(unsigned, "0B"):
		if sign < 0 || unsigned != trimmed {
			return 0, false
		}
		parsed, err := strconv.ParseUint(unsigned[2:], 2, 64)
		if err != nil {
			return 0, false
		}
		return float64(parsed), true
	case strings.HasPrefix(unsigned, "0o") || strings.HasPrefix(unsigned, "0O"):
		if sign < 0 || unsigned != trimmed {
			return 0, false
		}
		parsed, err := strconv.ParseUint(unsigned[2:], 8, 64)
		if err != nil {
			return 0, false
		}
		return float64(parsed), true
	}

	if !ticketsJSNumberPattern.MatchString(trimmed) {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

var ticketsJSNumberPattern = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$`)

// ticketsParsePositiveInteger mirrors parsePositiveInteger(value, fallback, max).
func ticketsParsePositiveInteger(raw string, fallback int, max *int) int {
	parsed, ok := ticketsJSToNumber(raw)
	if !ok || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed != math.Trunc(parsed) || parsed <= 0 {
		return fallback
	}
	if max != nil && parsed > float64(*max) {
		return *max
	}
	// Keep SQL OFFSET arithmetic in range for absurd (but JS-valid) page numbers.
	if parsed > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(parsed)
}

// ticketsParsePositiveID mirrors parsePositiveId(value: unknown).
func ticketsParsePositiveID(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) || typed <= 0 {
			return 0, false
		}
		if typed > 1<<53-1 {
			return 0, false
		}
		return int64(typed), true
	case string:
		return lib.ParsePositiveRouteID(typed)
	default:
		return 0, false
	}
}

// ticketsOptionalPositiveID mirrors parseOptionalPositiveId(value: unknown): the
// first return value is the parsed id, the second reports "absent or valid", the
// third reports "present but malformed" (the TypeScript null case).
func ticketsOptionalPositiveID(value *string) (id int64, present bool, valid bool) {
	if value == nil || *value == "" {
		return 0, false, true
	}
	parsed, ok := lib.ParsePositiveRouteID(*value)
	if !ok {
		return 0, false, false
	}
	return parsed, true, true
}

// ==================== ticket payload reading ====================

// ticketsParsedPayload mirrors ParsedTicketPayload.
type ticketsParsedPayload struct {
	Fields map[string]string
	Images []TicketsImageInput
}

// ticketsPayloadError is one of the Fastify-level failures that the route's catch
// block rethrows (they are not handled payload errors).
type ticketsPayloadError struct {
	Status  int
	Message string
	Code    string
}

func (e *ticketsPayloadError) Error() string { return e.Message }

// ticketsFrameworkBody is the result of the body parse Fastify performs *before*
// the handler runs: a decoded JSON value or the text body, plus whether the
// request is multipart (whose parts are read lazily inside the handler).
type ticketsFrameworkBody struct {
	Decoded     any
	Present     bool
	IsMultipart bool
}

// ticketsParseFrameworkBody mirrors Fastify's content-type parser dispatch and
// JSON/text decoding, including bodyLimit, the empty-body and invalid-JSON
// errors. Multipart is only *detected* here, exactly like @fastify/multipart's
// `addContentTypeParser('multipart/form-data', setMultipart)`.
func ticketsParseFrameworkBody(c *gin.Context, bodyLimit int64) (ticketsFrameworkBody, error) {
	request := c.Request
	contentType := request.Header.Get("Content-Type")

	if contentType == "" {
		if len(request.TransferEncoding) == 0 && request.ContentLength <= 0 {
			// No body at all: Fastify leaves request.body undefined.
			return ticketsFrameworkBody{}, nil
		}
		return ticketsFrameworkBody{}, &ticketsPayloadError{
			Status:  http.StatusUnsupportedMediaType,
			Message: "Unsupported Media Type: undefined",
		}
	}

	lowered := strings.ToLower(contentType)
	switch {
	case mediaTypeIs(lowered, "multipart/form-data"):
		return ticketsFrameworkBody{IsMultipart: true}, nil
	case !mediaTypeIs(lowered, "application/json") && !mediaTypeIs(lowered, "text/plain"):
		return ticketsFrameworkBody{}, &ticketsPayloadError{
			Status:  http.StatusUnsupportedMediaType,
			Message: "Unsupported Media Type: " + contentType,
		}
	}

	if bodyLimit > 0 {
		if request.ContentLength > bodyLimit {
			return ticketsFrameworkBody{}, &ticketsPayloadError{
				Status:  http.StatusRequestEntityTooLarge,
				Message: "Request body is too large",
			}
		}
		data, err := io.ReadAll(io.LimitReader(request.Body, bodyLimit+1))
		if err != nil {
			return ticketsFrameworkBody{}, &ticketsPayloadError{
				Status:  http.StatusBadRequest,
				Message: fastifyInvalidJSONBodyMessage,
			}
		}
		if int64(len(data)) > bodyLimit {
			return ticketsFrameworkBody{}, &ticketsPayloadError{
				Status:  http.StatusRequestEntityTooLarge,
				Message: "Request body is too large",
			}
		}
		return ticketsDecodeFrameworkBody(lowered, data)
	}

	data, err := io.ReadAll(request.Body)
	if err != nil {
		return ticketsFrameworkBody{}, &ticketsPayloadError{
			Status:  http.StatusBadRequest,
			Message: fastifyInvalidJSONBodyMessage,
		}
	}
	return ticketsDecodeFrameworkBody(lowered, data)
}

// ticketsDecodeFrameworkBody is the parser half of ticketsParseFrameworkBody.
func ticketsDecodeFrameworkBody(lowered string, data []byte) (ticketsFrameworkBody, error) {
	if mediaTypeIs(lowered, "text/plain") {
		// Fastify's default text/plain parser keeps the raw string.
		return ticketsFrameworkBody{Decoded: string(data), Present: true}, nil
	}

	if len(data) == 0 {
		return ticketsFrameworkBody{}, &ticketsPayloadError{
			Status:  http.StatusBadRequest,
			Message: fastifyEmptyJSONBodyMessage,
		}
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil || hasForbiddenPrototypeKey(decoded) {
		return ticketsFrameworkBody{}, &ticketsPayloadError{
			Status:  http.StatusBadRequest,
			Message: fastifyInvalidJSONBodyMessage,
		}
	}
	return ticketsFrameworkBody{Decoded: decoded, Present: true}, nil
}

// ticketsReadPayload mirrors readTicketPayload().
func ticketsReadPayload(c *gin.Context, base ticketsFrameworkBody) (ticketsParsedPayload, error) {
	if base.IsMultipart {
		return ticketsReadMultipart(c.Request, c.GetHeader("Content-Type"))
	}

	fields := map[string]string{}
	if !base.Present {
		return ticketsParsedPayload{Fields: fields}, nil
	}

	switch typed := base.Decoded.(type) {
	case map[string]any:
		for key, value := range typed {
			if value == nil {
				continue // `value !== null && value !== undefined`
			}
			if text, ok := ticketsStringValue(value); ok {
				fields[key] = text
			}
		}
	case []any:
		for index, value := range typed {
			if value == nil {
				continue
			}
			if text, ok := ticketsStringValue(value); ok {
				fields[strconv.Itoa(index)] = text
			}
		}
	case string:
		fields = ticketsFieldMapFromString(typed)
	}

	return ticketsParsedPayload{Fields: fields}, nil
}

// ticketsFieldMapFromString mirrors `Object.entries(str)` (index → character).
// Only used for the text/plain and JSON-string body edge cases.
func ticketsFieldMapFromString(value string) map[string]string {
	fields := map[string]string{}
	for index, runeValue := range []rune(value) {
		fields[strconv.Itoa(index)] = string(runeValue)
	}
	return fields
}

// ticketsReadMultipart mirrors the multipart branch of readTicketPayload.
func ticketsReadMultipart(request *http.Request, contentType string) (ticketsParsedPayload, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		// busboy throws when the boundary is missing; Fastify surfaces a 400.
		return ticketsParsedPayload{}, &ticketsPayloadError{
			Status:  http.StatusBadRequest,
			Message: "the request is not multipart",
		}
	}

	reader := multipart.NewReader(request.Body, params["boundary"])
	fields := map[string]string{}
	images := make([]TicketsImageInput, 0, ticketsMaxImages)
	parts := 0
	files := 0

	// ticketsTooLarge mirrors busboy's truncation + toBuffer's
	// throwFileSizeLimit behaviour (FST_REQ_FILE_TOO_LARGE, rendered specially by
	// app.ts as 413 {error, code: 'FILE_TOO_LARGE'}).
	ticketsTooLarge := &ticketsPayloadError{
		Status:  http.StatusRequestEntityTooLarge,
		Message: "request file too large",
		Code:    "FILE_TOO_LARGE",
	}

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ticketsParsedPayload{}, &ticketsPayloadError{
				Status:  http.StatusBadRequest,
				Message: err.Error(),
			}
		}

		parts++
		if parts > ticketsMultipartMaxParts {
			return ticketsParsedPayload{}, &ticketsPayloadError{
				Status:  http.StatusRequestEntityTooLarge,
				Message: "reach parts limit",
			}
		}

		filename := part.FileName()
		fieldname := part.FormName()
		if filename == "" {
			// busboy's fieldSize cap is 1MB; overflow truncates the field silently.
			content, readErr := io.ReadAll(io.LimitReader(part, ticketsMultipartFieldSize+1))
			if readErr != nil {
				return ticketsParsedPayload{}, &ticketsPayloadError{
					Status:  http.StatusBadRequest,
					Message: readErr.Error(),
				}
			}
			if len(content) > ticketsMultipartFieldSize {
				content = content[:ticketsMultipartFieldSize]
			}
			fields[fieldname] = string(content)
			continue
		}

		files++
		if files > ticketsMaxImages {
			return ticketsParsedPayload{}, &ticketsPayloadError{
				Status:  http.StatusRequestEntityTooLarge,
				Message: "reach files limit",
			}
		}

		mimetype := part.Header.Get("Content-Type")
		if mimetype != "" {
			if parsed, _, parseErr := mime.ParseMediaType(mimetype); parseErr == nil {
				mimetype = parsed
			}
		}

		if fieldname != "images" {
			// `await part.toBuffer()` then discard (still enforces fileSize).
			buffer, readErr := io.ReadAll(io.LimitReader(part, ticketsMaxImageSize+1))
			if readErr != nil {
				return ticketsParsedPayload{}, &ticketsPayloadError{
					Status:  http.StatusBadRequest,
					Message: readErr.Error(),
				}
			}
			if int64(len(buffer)) > ticketsMaxImageSize {
				return ticketsParsedPayload{}, ticketsTooLarge
			}
			continue
		}

		if mimetype == "" || !ticketsAllowedImageMimeTypes[strings.ToLower(mimetype)] {
			return ticketsParsedPayload{}, errors.New("Only JPG, PNG, WebP, GIF and AVIF images are supported")
		}
		if len(images) >= ticketsMaxImages {
			return ticketsParsedPayload{}, fmt.Errorf("A ticket message can contain at most %d images", ticketsMaxImages)
		}

		buffer, readErr := io.ReadAll(io.LimitReader(part, ticketsMaxImageSize+1))
		if readErr != nil {
			return ticketsParsedPayload{}, &ticketsPayloadError{
				Status:  http.StatusBadRequest,
				Message: readErr.Error(),
			}
		}
		if int64(len(buffer)) > ticketsMaxImageSize {
			return ticketsParsedPayload{}, ticketsTooLarge
		}
		if len(buffer) == 0 {
			continue
		}

		images = append(images, TicketsImageInput{
			Buffer:      buffer,
			Filename:    filename,
			ContentType: strings.ToLower(mimetype),
			SizeBytes:   int64(len(buffer)),
		})
	}

	return ticketsParsedPayload{Fields: fields, Images: images}, nil
}

// ticketsHandledPayloadErrorFragments mirrors isHandledTicketPayloadError().
var ticketsHandledPayloadErrorFragments = []string{
	"Only JPG",
	"A ticket message can contain at most",
	"Each image must be no larger than",
	"Lsky",
	"image bed",
	"Cannot reply to a closed ticket",
	"must use http or https",
	"Private or",
	"Unable to resolve hostname",
	"Targets resolving",
}

func ticketsIsHandledPayloadError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, fragment := range ticketsHandledPayloadErrorFragments {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

// ticketsUploadImages mirrors uploadTicketImages().
func ticketsUploadImages(ctx context.Context, images []TicketsImageInput) ([]db.TicketsAttachmentData, error) {
	uploaded := make([]db.TicketsAttachmentData, 0, len(images))
	for _, image := range images {
		result, err := TicketsImageUploader(ctx, image)
		if err != nil {
			ticketsCleanupUploads(ctx, uploaded)
			return nil, err
		}
		uploaded = append(uploaded, result)
	}
	return uploaded, nil
}

// ticketsCleanupUploads mirrors cleanupUploadedTicketImages(): all failures are
// swallowed (Promise.allSettled).
func ticketsCleanupUploads(ctx context.Context, attachments []db.TicketsAttachmentData) {
	for _, attachment := range attachments {
		_ = TicketsImageCleaner(ctx, attachment.ProviderVersion, attachment.ProviderFileID)
	}
}

// ==================== attachment proxy ====================

// ticketsNormalizeAllowedImageMimeType mirrors normalizeAllowedImageMimeType().
func ticketsNormalizeAllowedImageMimeType(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := ticketsJSTrim(strings.SplitN(*value, ";", 2)[0])
	normalized = strings.ToLower(normalized)
	if normalized == "" || !ticketsAllowedImageMimeTypes[normalized] {
		return nil
	}
	return &normalized
}

// ticketsManualRedirectClient mirrors `safeFetch(..., { redirect: 'manual' })`: it
// reuses the connect-time-revalidating transport but never follows a redirect.
var ticketsManualRedirectClient = &http.Client{
	Transport: lib.SafeOutboundHTTPClient().Transport,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// ticketsReadRemoteImageBody mirrors readRemoteImageBody().
func ticketsReadRemoteImageBody(response *http.Response) ([]byte, error) {
	if raw := response.Header.Get("Content-Length"); raw != "" {
		if contentLength, ok := ticketsJSToNumber(raw); ok && !math.IsInf(contentLength, 0) &&
			!math.IsNaN(contentLength) && contentLength > ticketsMaxImageSize {
			return nil, errors.New("Remote image is too large")
		}
	}

	limited := io.LimitReader(response.Body, ticketsMaxImageSize+1)
	buffer, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.New("Remote image is too large")
	}
	if int64(len(buffer)) > ticketsMaxImageSize {
		return nil, errors.New("Remote image is too large")
	}
	return buffer, nil
}

// ==================== route helpers ====================

// ticketsHostOwnership mirrors getTicketHostOwnership().
type ticketsHostOwnership struct {
	ID     int32
	Name   string
	UserID int32
	Role   string
}

func ticketsGetHostOwnership(ctx context.Context, hostID *int32) (*ticketsHostOwnership, error) {
	if hostID == nil || *hostID == 0 {
		return nil, nil
	}
	var rows []struct {
		ID     int32  `gorm:"column:id"`
		Name   string `gorm:"column:name"`
		UserID int32  `gorm:"column:user_id"`
		Role   string `gorm:"column:role"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT h.id, h.name, h.user_id, u.role::text AS role
		 FROM hosts h LEFT JOIN users u ON u.id = h.user_id
		 WHERE h.id = ? LIMIT 1`, *hostID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &ticketsHostOwnership{ID: rows[0].ID, Name: rows[0].Name, UserID: rows[0].UserID, Role: rows[0].Role}, nil
}

// ticketsInstanceRef is the projection of db.getInstanceById used here.
type ticketsInstanceRef struct {
	ID     int32  `gorm:"column:id"`
	Name   string `gorm:"column:name"`
	UserID int32  `gorm:"column:user_id"`
	HostID int32  `gorm:"column:host_id"`
}

func ticketsGetInstance(ctx context.Context, instanceID int64) (*ticketsInstanceRef, error) {
	var rows []ticketsInstanceRef
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT id, name, user_id, host_id FROM instances WHERE id = ? LIMIT 1", instanceID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ticketsHostRow is the projection the host-ownership check needs. It is
// separate from ticketsInstanceRef because `hosts` has no `host_id` column — the
// host's own `id` is the identity, and only the owner matters here.
type ticketsHostRow struct {
	ID     int32  `gorm:"column:id"`
	Name   string `gorm:"column:name"`
	UserID int32  `gorm:"column:user_id"`
}

// ticketsHostOwner mirrors the `db.getHostById(hostId)` ownership check.
func ticketsHostOwner(ctx context.Context, hostID int64) (*ticketsHostRow, error) {
	var rows []ticketsHostRow
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT id, name, user_id FROM hosts WHERE id = ? LIMIT 1", hostID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// ticketsRespondInternal mirrors the generic 500 envelope.
func ticketsRespondInternal(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// ticketsRespondPayloadError routes an error from the payload pipeline the same
// way the TypeScript catch block does: Fastify-level errors keep their own
// status/envelope, handled payload errors become 400 INVALID_PARAMS.
func ticketsRespondPayloadError(c *gin.Context, err error, fallback string) {
	var payloadErr *ticketsPayloadError
	if errors.As(err, &payloadErr) {
		if payloadErr.Code != "" {
			c.JSON(payloadErr.Status, gin.H{"error": payloadErr.Message, "code": payloadErr.Code})
			return
		}
		c.JSON(payloadErr.Status, gin.H{"error": payloadErr.Message})
		return
	}
	if ticketsIsHandledPayloadError(err) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, err.Error())
		return
	}
	// Unhandled: the TypeScript rethrows, so app.ts' generic handler answers.
	if fallback == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// ticketsNotifyAdmins mirrors `const adminIds = await getAllAdminUserIds(); for …`.
func ticketsNotifyAdmins(ctx context.Context, eventType lib.EventType, data lib.EventData) {
	adminIDs, err := db.TicketsGetAllAdminUserIDs(ctx)
	if err != nil {
		return // the TypeScript wraps this whole block in try/catch
	}
	for _, adminID := range adminIDs {
		lib.SendNotification(ctx, int64(adminID), eventType, data)
	}
}

// ticketsHostName mirrors `ticket.host?.name || '系统'`.
func ticketsHostName(host *db.TicketsTicketHostRef) string {
	if host == nil || host.Name == "" {
		return "系统"
	}
	return host.Name
}

// ==================== handlers ====================

// ticketsCreateHandler mirrors `fastify.post('/', { onRequest: [authenticate] })`.
func ticketsCreateHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	uploaded := make([]db.TicketsAttachmentData, 0)

	// Fastify parses the (non-multipart) body before the handler runs, so a 415 /
	// invalid-JSON / empty-body failure wins over the ticket_enabled 403 below.
	// Multipart parts are only read later, inside readTicketPayload, exactly like
	// @fastify/multipart's lazy `request.parts()`.
	base, baseErr := ticketsParseFrameworkBody(c, ticketsUploadBodyLimit)
	if baseErr != nil {
		ticketsRespondPayloadError(c, baseErr, "Invalid ticket payload")
		return
	}

	enabled, err := db.GetSystemConfigBoolean(ctx, "ticket_enabled", true)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if !enabled {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "Ticket creation is disabled")
		return
	}

	payload, payloadErr := ticketsReadPayload(c, base)
	if payloadErr != nil {
		ticketsRespondPayloadError(c, payloadErr, "Invalid ticket payload")
		return
	}

	instanceID, hasInstanceID, instanceIDValid := ticketsOptionalPositiveID(optionalString(payload.Fields, "instanceId"))
	if !instanceIDValid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	subject := ticketsSanitizeContent(optionalString(payload.Fields, "subject"))
	category := ticketsSanitizeContent(optionalString(payload.Fields, "category"))
	priority := ticketsSanitizeContent(optionalString(payload.Fields, "priority"))
	content := ticketsSanitizeContent(optionalString(payload.Fields, "content"))

	if subject == "" || jsStringLength(subject) < 2 || jsStringLength(subject) > 200 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Subject must be 2-200 characters")
		return
	}
	if jsStringLength(content) > 5000 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Content must be 0-5000 characters")
		return
	}
	if len(payload.Images) == 0 && jsStringLength(content) < 10 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Content must be 10-5000 characters when no images are attached")
		return
	}
	if category != "" && !ticketsValidCategories[category] {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid category")
		return
	}
	if priority != "" && !ticketsValidPriorities[priority] {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid priority")
		return
	}

	var (
		hostID   *int32
		instance *ticketsInstanceRef
		host     *ticketsHostOwnership
	)

	if hasInstanceID {
		instance, err = ticketsGetInstance(ctx, instanceID)
		if err != nil {
			ticketsRespondInternal(c)
			return
		}
		if instance == nil || instance.UserID != user.ID {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceNotFound, "")
			return
		}
		hostID = &instance.HostID

		host, err = ticketsGetHostOwnership(ctx, hostID)
		if err != nil {
			ticketsRespondInternal(c)
			return
		}
		if host == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
			return
		}
	}

	if len(payload.Images) > 0 {
		uploaded, err = ticketsUploadImages(ctx, payload.Images)
		if err != nil {
			ticketsCleanupUploads(ctx, uploaded)
			ticketsRespondPayloadError(c, err, "Invalid ticket payload")
			return
		}
	}

	createData := db.TicketsCreateTicketData{
		UserID:      user.ID,
		HostID:      hostID,
		Subject:     subject,
		Category:    nil,
		Content:     content,
		Attachments: uploaded,
	}
	if hasInstanceID {
		value := instanceID
		instanceIDInt32 := int32(value)
		createData.InstanceID = &instanceIDInt32
	}
	if category != "" {
		createData.Category = &category
	}
	if priority != "" {
		parsed := models.TicketPriority(priority)
		createData.Priority = &parsed
	}

	ticketID, messageID, err := db.TicketsCreateTicket(ctx, createData)
	if err != nil {
		ticketsCleanupUploads(ctx, uploaded)
		ticketsRespondPayloadError(c, err, "Invalid ticket payload")
		return
	}

	// 通知所有管理员；失败不影响创建结果。
	instanceName := "无"
	if instance != nil && instance.Name != "" {
		instanceName = instance.Name
	}
	hostName := "系统"
	if host != nil && host.Name != "" {
		hostName = host.Name
	}
	ticketsNotifyAdmins(ctx, lib.EventTicketCreated, lib.EventData{
		"username":     user.Username,
		"subject":      subject,
		"hostName":     hostName,
		"instanceName": instanceName,
	})

	c.JSON(http.StatusCreated, gin.H{
		"message": "Ticket created successfully",
		"ticket": gin.H{
			"id":        ticketID,
			"messageId": messageID,
		},
	})
}

// optionalString returns a pointer to the map value when it exists.
func optionalString(values map[string]string, key string) *string {
	value, present := values[key]
	if !present {
		return nil
	}
	return &value
}

func hasKey(values map[string]string, key string) bool {
	_, present := values[key]
	return present
}

// ticketsUserListHandler mirrors `fastify.get('/', { onRequest: [authenticate] })`.
func ticketsUserListHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	query := c.Request.URL.Query()

	status := query.Get("status")
	if status != "" && status != "active" && !ticketsValidStatuses[status] {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid status")
		return
	}

	page := ticketsParsePositiveInteger(query.Get("page"), 1, nil)
	maxPageSize := 100
	pageSize := ticketsParsePositiveInteger(query.Get("pageSize"), 10, &maxPageSize)

	options := db.TicketsListOptions{Status: status, Page: &page, PageSize: &pageSize}
	if search, present := query["search"]; present {
		value := ""
		if len(search) > 0 {
			value = search[0]
		}
		options.Search = &value
	}

	result, err := db.TicketsGetUserTickets(c.Request.Context(), user.ID, options)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ticketsAttachmentContentHandler mirrors
// `fastify.get('/attachments/:attachmentId/content')`.
func ticketsAttachmentContentHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	attachmentID, ok := lib.ParsePositiveRouteID(c.Param("attachmentId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	attachment, err := db.TicketsGetTicketMessageAttachmentByID(ctx, int32(attachmentID))
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if attachment == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	access, err := db.TicketsCanUserAccessTicket(ctx, user.ID, attachment.TicketID, user.Role)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if !access.CanAccess {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	safeURL, err := lib.AssertSafeHTTPURL(ctx, attachment.URL, "Ticket attachment image URL")
	if err != nil {
		lib.RespondAPIError(c, http.StatusBadGateway, lib.ErrInternalError, "Failed to load remote image")
		return
	}

	fetchCtx, cancel := context.WithTimeout(ctx, ticketsProxyFetchTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, safeURL.String(), nil)
	if err != nil {
		lib.RespondAPIError(c, http.StatusBadGateway, lib.ErrInternalError, "Failed to load remote image")
		return
	}

	upstream, err := ticketsManualRedirectClient.Do(request)
	if err != nil {
		lib.RespondAPIError(c, http.StatusBadGateway, lib.ErrInternalError, "Failed to load remote image")
		return
	}
	defer upstream.Body.Close()

	if upstream.StatusCode < 200 || upstream.StatusCode > 299 {
		lib.RespondAPIError(c, http.StatusBadGateway, lib.ErrInternalError, "Failed to load remote image")
		return
	}

	upstreamContentType := upstream.Header.Get("Content-Type")
	upstreamMimeType := ticketsNormalizeAllowedImageMimeType(&upstreamContentType)
	storedMimeType := ticketsNormalizeAllowedImageMimeType(&attachment.MimeType)

	if upstreamContentType != "" && upstreamMimeType == nil {
		lib.RespondAPIError(c, http.StatusBadGateway, lib.ErrInternalError, "Remote file is not an allowed image type")
		return
	}

	responseMimeType := upstreamMimeType
	if responseMimeType == nil {
		responseMimeType = storedMimeType
	}
	if responseMimeType == nil {
		lib.RespondAPIError(c, http.StatusBadGateway, lib.ErrInternalError, "Attachment image type is not allowed")
		return
	}

	imageBuffer, err := ticketsReadRemoteImageBody(upstream)
	if err != nil {
		lib.RespondAPIError(c, http.StatusBadGateway, lib.ErrInternalError, "Remote image is too large")
		return
	}

	c.Header("Cache-Control", "private, max-age=300")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, *responseMimeType, imageBuffer)
}

// ticketsDetailHandler mirrors `fastify.get('/:id')`.
func ticketsDetailHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	ticketID, ok := lib.ParsePositiveRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	access, err := db.TicketsCanUserAccessTicket(ctx, user.ID, int32(ticketID), user.Role)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if !access.CanAccess {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	ticket, err := db.TicketsGetTicketByID(ctx, int32(ticketID))
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if ticket == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ticket":    ticket,
		"isOwner":   access.IsOwner,
		"isCreator": access.IsCreator,
	})
}

// ticketsSupportContextHandler mirrors `fastify.get('/:id/support-context')`.
func ticketsSupportContextHandler(c *gin.Context) {
	ticketID, ok := lib.ParsePositiveRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	contextPayload, err := db.TicketsGetAdminTicketSuccessContext(c.Request.Context(), int32(ticketID))
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if contextPayload == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}
	c.JSON(http.StatusOK, contextPayload)
}

// ticketsReadJSONObjectBody reproduces Fastify's body parse for the body-carrying
// non-multipart routes; it returns the decoded value and whether the request may
// proceed.
func ticketsReadJSONObjectBody(c *gin.Context) (any, bool) {
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return nil, false
	}
	return decoded.Value, true
}

// ticketsBodyStringField mirrors `typeof body?.field === 'string' ? body.field : undefined`.
// A non-object body yields nil (matching `request.body?.x`).
func ticketsBodyStringField(body any, key string) *string {
	object, ok := body.(map[string]any)
	if !ok {
		return nil
	}
	value, present := object[key]
	if !present {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return nil
	}
	return &text
}

// ticketsInternalNoteHandler mirrors `fastify.post('/:id/internal-notes')`.
func ticketsInternalNoteHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	body, ok := ticketsReadJSONObjectBody(c)
	if !ok {
		return
	}

	ticketID, idOK := lib.ParsePositiveRouteID(c.Param("id"))
	content := ticketsSanitizeContent(ticketsBodyStringField(body, "content"))

	if !idOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if jsStringLength(content) < 1 || jsStringLength(content) > 3000 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Internal note must be 1-3000 characters")
		return
	}

	ticket, err := db.TicketsGetTicketByID(ctx, int32(ticketID))
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if ticket == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	note, err := db.TicketsCreateTicketInternalNote(ctx, int32(ticketID), user.ID, user.Username, content)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"note": note})
}

// ticketsLinkHandler mirrors `fastify.post('/:id/links')`.
func ticketsLinkHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	body, ok := ticketsReadJSONObjectBody(c)
	if !ok {
		return
	}

	ticketID, ticketIDOK := lib.ParsePositiveRouteID(c.Param("id"))

	var rawObjectID any
	if object, isObject := body.(map[string]any); isObject {
		rawObjectID = object["objectId"]
	}
	objectID, objectIDOK := ticketsParsePositiveID(rawObjectID)

	// normalizeTicketObjectLinkType takes the raw value (it does not trim).
	rawObjectType := ""
	if text := ticketsBodyStringField(body, "objectType"); text != nil {
		rawObjectType = *text
	}
	objectType := db.TicketsNormalizeTicketObjectLinkType(rawObjectType)

	if !ticketIDOK || !objectIDOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if objectType == "" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid object type")
		return
	}

	link, err := db.TicketsCreateTicketObjectLink(ctx, int32(ticketID), objectType, int32(objectID), user.ID, user.Username)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if link == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"link": link})
}

// ticketsNotifyHandler mirrors `fastify.post('/:id/notify')`.
func ticketsNotifyHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	body, ok := ticketsReadJSONObjectBody(c)
	if !ok {
		return
	}

	ticketID, idOK := lib.ParsePositiveRouteID(c.Param("id"))
	content := ticketsSanitizeContent(ticketsBodyStringField(body, "content"))

	if !idOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if jsStringLength(content) < 1 || jsStringLength(content) > 1000 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Notification must be 1-1000 characters")
		return
	}

	ticket, err := db.TicketsGetTicketByID(ctx, int32(ticketID))
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if ticket == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	lib.SendNotification(ctx, int64(ticket.UserID), lib.EventTicketReplied, lib.EventData{
		"subject":   ticket.Subject,
		"hostName":  ticketsHostName(ticket.Host),
		"message":   content,
		"replyFrom": user.Username,
	})
	if _, err := db.TicketsCreateTicketInternalNote(
		ctx, int32(ticketID), user.ID, user.Username, "已向用户发送通知："+content,
	); err != nil {
		ticketsRespondInternal(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Notification sent successfully"})
}

// ticketsMessagesListHandler mirrors `fastify.get('/:id/messages')`.
func ticketsMessagesListHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	query := c.Request.URL.Query()

	ticketID, ok := lib.ParsePositiveRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	access, err := db.TicketsCanUserAccessTicket(ctx, user.ID, int32(ticketID), user.Role)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if !access.CanAccess {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	maxPageSize := 100
	page := ticketsParsePositiveInteger(query.Get("page"), 1, nil)
	pageSize := ticketsParsePositiveInteger(query.Get("pageSize"), 50, &maxPageSize)

	result, err := db.TicketsGetTicketMessages(ctx, int32(ticketID), &page, &pageSize)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ticketsReplyHandler mirrors `fastify.post('/:id/messages')`.
func ticketsReplyHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	uploaded := make([]db.TicketsAttachmentData, 0)

	// Fastify parses the non-multipart body before the handler; multipart parts are
	// consumed later by readTicketPayload.
	base, baseErr := ticketsParseFrameworkBody(c, ticketsUploadBodyLimit)
	if baseErr != nil {
		ticketsRespondPayloadError(c, baseErr, "Invalid ticket reply payload")
		return
	}

	ticketID, ok := lib.ParsePositiveRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	payload, payloadErr := ticketsReadPayload(c, base)
	if payloadErr != nil {
		ticketsRespondPayloadError(c, payloadErr, "Invalid ticket reply payload")
		return
	}

	content := ticketsSanitizeContent(optionalString(payload.Fields, "content"))

	if jsStringLength(content) > 5000 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Content must be 0-5000 characters")
		return
	}
	if len(payload.Images) == 0 && jsStringLength(content) < 1 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Content must be 1-5000 characters when no images are attached")
		return
	}

	access, err := db.TicketsCanUserAccessTicket(ctx, user.ID, int32(ticketID), user.Role)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if !access.CanAccess {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	ticket, err := db.TicketsGetTicketByID(ctx, int32(ticketID))
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if ticket == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	if ticket.Status == string(models.TicketStatusClosed) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Cannot reply to a closed ticket")
		return
	}

	if len(payload.Images) > 0 {
		uploaded, err = ticketsUploadImages(ctx, payload.Images)
		if err != nil {
			ticketsCleanupUploads(ctx, uploaded)
			ticketsRespondPayloadError(c, err, "Invalid ticket reply payload")
			return
		}
	}

	message, err := db.TicketsAddTicketMessage(ctx, int32(ticketID), user.ID, content, access.IsOwner, uploaded)
	if err != nil {
		ticketsCleanupUploads(ctx, uploaded)
		ticketsRespondPayloadError(c, err, "Invalid ticket reply payload")
		return
	}

	// 通知；失败不影响回复结果。
	notification := lib.EventData{
		"subject":   ticket.Subject,
		"hostName":  ticketsHostName(ticket.Host),
		"replyFrom": user.Username,
	}
	if access.IsOwner {
		lib.SendNotification(ctx, int64(ticket.UserID), lib.EventTicketReplied, notification)
	} else {
		ticketsNotifyAdmins(ctx, lib.EventTicketReplied, notification)
	}

	c.JSON(http.StatusCreated, gin.H{
		"message": "Message added successfully",
		"data":    message,
	})
}

// ticketsDeleteMessageHandler mirrors `fastify.delete('/:id/messages/:messageId')`.
func ticketsDeleteMessageHandler(c *gin.Context) {
	ctx := c.Request.Context()

	// Fastify parses (and 400s on an empty JSON) body before the handler runs.
	if _, status, message := decodeJSONRequestBody(c); status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}

	ticketID, ticketOK := lib.ParsePositiveRouteID(c.Param("id"))
	messageID, messageOK := lib.ParsePositiveRouteID(c.Param("messageId"))

	if !ticketOK || !messageOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	message, err := db.TicketsGetTicketMessageByIDRef(ctx, int32(messageID))
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if message == nil || message.TicketID != int32(ticketID) {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	attachments, err := db.TicketsGetTicketMessageAttachments(ctx, int32(messageID))
	if err != nil {
		ticketsRespondInternal(c)
		return
	}

	if _, err := db.TicketsDeleteTicketMessage(ctx, int32(messageID)); err != nil {
		ticketsRespondInternal(c)
		return
	}

	for _, attachment := range attachments {
		_ = TicketsImageCleaner(ctx, attachment.ProviderVersion, attachment.ProviderFileID)
	}

	c.JSON(http.StatusOK, gin.H{"message": "Message deleted successfully"})
}

// ticketsNormalizeStatusBody mirrors normalizeTicketStatusBody(body).
func ticketsNormalizeStatusBody(body any) string {
	object, ok := body.(map[string]any)
	if !ok {
		return ""
	}
	value, present := object["status"]
	if !present {
		return ""
	}
	text, isString := value.(string)
	if !isString || !ticketsValidStatuses[text] {
		return ""
	}
	return text
}

// ticketsStatusHandler mirrors `fastify.patch('/:id/status')`.
func ticketsStatusHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	body, ok := ticketsReadJSONObjectBody(c)
	if !ok {
		return
	}

	ticketID, idOK := lib.ParsePositiveRouteID(c.Param("id"))
	status := ticketsNormalizeStatusBody(body)

	if !idOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	if status == "" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid status")
		return
	}

	access, err := db.TicketsCanUserAccessTicket(ctx, user.ID, int32(ticketID), user.Role)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if !access.CanAccess {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	ticket, err := db.TicketsGetTicketByID(ctx, int32(ticketID))
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if ticket == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	isCreatorReopen := access.IsCreator && ticket.Status == string(models.TicketStatusClosed) &&
		status == string(models.TicketStatusOpen)
	if !access.IsOwner && !isCreatorReopen {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	if err := db.TicketsUpdateTicketStatus(ctx, int32(ticketID), models.TicketStatus(status)); err != nil {
		ticketsRespondInternal(c)
		return
	}

	lib.SendNotification(ctx, int64(ticket.UserID), lib.EventTicketStatusChanged, lib.EventData{
		"subject":   ticket.Subject,
		"hostName":  ticketsHostName(ticket.Host),
		"newStatus": status,
	})

	c.JSON(http.StatusOK, gin.H{
		"message": "Status updated successfully",
		"status":  status,
	})
}

// ticketsCloseHandler mirrors `fastify.post('/:id/close')`.
func ticketsCloseHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	if _, status, message := decodeJSONRequestBody(c); status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}

	ticketID, idOK := lib.ParsePositiveRouteID(c.Param("id"))
	if !idOK {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	access, err := db.TicketsCanUserAccessTicket(ctx, user.ID, int32(ticketID), user.Role)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if !access.CanAccess {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	ticket, err := db.TicketsGetTicketByID(ctx, int32(ticketID))
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if ticket == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrNotFound, "")
		return
	}

	if ticket.Status == string(models.TicketStatusClosed) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Ticket is already closed")
		return
	}

	if err := db.TicketsUpdateTicketStatus(ctx, int32(ticketID), models.TicketStatusClosed); err != nil {
		ticketsRespondInternal(c)
		return
	}

	notification := lib.EventData{
		"subject":  ticket.Subject,
		"hostName": ticketsHostName(ticket.Host),
		"closedBy": user.Username,
	}
	if access.IsCreator {
		ticketsNotifyAdmins(ctx, lib.EventTicketClosed, notification)
	} else {
		lib.SendNotification(ctx, int64(ticket.UserID), lib.EventTicketClosed, notification)
	}

	c.JSON(http.StatusOK, gin.H{"message": "Ticket closed successfully"})
}

// ticketsHostListHandler mirrors `fastify.get('/hosts/:hostId')`.
func ticketsHostListHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	query := c.Request.URL.Query()

	hostID, ok := lib.ParsePositiveRouteID(c.Param("hostId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	host, err := ticketsHostOwner(ctx, hostID)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	if host == nil || host.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	status := query.Get("status")
	if status != "" && status != "active" && !ticketsValidStatuses[status] {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid status")
		return
	}

	maxPageSize := 100
	page := ticketsParsePositiveInteger(query.Get("page"), 1, nil)
	pageSize := ticketsParsePositiveInteger(query.Get("pageSize"), 10, &maxPageSize)

	options := db.TicketsListOptions{Status: status, Page: &page, PageSize: &pageSize}
	if search, present := query["search"]; present {
		value := ""
		if len(search) > 0 {
			value = search[0]
		}
		options.Search = &value
	}

	result, err := db.TicketsGetHostTickets(ctx, int32(hostID), options)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ticketsOwnerAllHandler mirrors `fastify.get('/my-hosts')`.
func ticketsOwnerAllHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	query := c.Request.URL.Query()

	isAdmin := user.Role == "admin"

	rawHostID, hostIDPresent := query["hostId"]
	hostIDValue := ""
	if len(rawHostID) > 0 {
		hostIDValue = rawHostID[0]
	}
	var hostIDInput *string
	if hostIDPresent {
		hostIDInput = &hostIDValue
	}
	parsedHostID, hasHostID, hostIDValid := ticketsOptionalPositiveID(hostIDInput)

	status := query.Get("status")
	if status != "" && status != "active" && !ticketsValidStatuses[status] {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid status")
		return
	}

	if !hostIDValid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	sourceType := query.Get("sourceType")
	if sourceType != "" && !ticketsValidSourceTypes[sourceType] {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid source type")
		return
	}
	queue := query.Get("queue")
	if queue != "" && !ticketsValidQueues[queue] {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid queue")
		return
	}
	if queue != "" && !isAdmin {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if !isAdmin && sourceType != "" && sourceType != "all" {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if hasHostID && sourceType == "user" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "User tickets cannot be filtered by host")
		return
	}

	var hostID *int32
	if hasHostID {
		value := int32(parsedHostID)
		hostID = &value

		if !isAdmin {
			host, err := ticketsHostOwner(ctx, parsedHostID)
			if err != nil {
				ticketsRespondInternal(c)
				return
			}
			if host == nil || host.UserID != user.ID {
				lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
				return
			}
		}
	}

	maxPageSize := 100
	page := ticketsParsePositiveInteger(query.Get("page"), 1, nil)
	pageSize := ticketsParsePositiveInteger(query.Get("pageSize"), 10, &maxPageSize)

	options := db.TicketsOwnerListOptions{
		Status:   status,
		HostID:   hostID,
		Queue:    queue,
		Page:     &page,
		PageSize: &pageSize,
	}
	if sourceType != "" && sourceType != "all" {
		options.SourceType = sourceType
	}
	if search, present := query["search"]; present {
		value := ""
		if len(search) > 0 {
			value = search[0]
		}
		options.Search = &value
	}

	var ownerID *int32
	if !isAdmin {
		value := user.ID
		ownerID = &value
	}

	result, err := db.TicketsGetOwnerAllTickets(ctx, ownerID, options)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ticketsPendingCountHandler mirrors `fastify.get('/pending-count')`.
func ticketsPendingCountHandler(c *gin.Context) {
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	isAdmin := user.Role == "admin"

	userHostCount := int64(0)
	if !isAdmin {
		if err := db.DB.WithContext(ctx).Raw(
			"SELECT COUNT(*) FROM hosts WHERE user_id = ?", user.ID,
		).Scan(&userHostCount).Error; err != nil {
			ticketsRespondInternal(c)
			return
		}
	}

	userCount := int64(0)
	if !isAdmin {
		count, err := db.TicketsGetUserOpenTicketCount(ctx, user.ID)
		if err != nil {
			ticketsRespondInternal(c)
			return
		}
		userCount = count
	}

	var ownerID *int32
	if !isAdmin {
		value := user.ID
		ownerID = &value
	}
	ownerCount, err := db.TicketsGetOwnerPendingTicketCount(ctx, ownerID)
	if err != nil {
		ticketsRespondInternal(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"userTickets": userCount,
		"hostTickets": ownerCount,
		"total":       userCount + ownerCount,
		"isHostOwner": isAdmin || userHostCount > 0,
	})
}

// ==================== registration ====================

// RegisterTickets mounts /api/tickets with the exact route set and middleware of
// the Fastify plugin. Fastify registers both "/api/tickets" and
// "/api/tickets/" for a plugin root handler, so both spellings are mounted.
func RegisterTickets(r *gin.Engine) {
	group := r.Group("/api/tickets", middleware.Authenticate())

	// ==================== 用户端 API ====================
	group.POST("", ticketsCreateHandler)
	group.POST("/", ticketsCreateHandler)

	group.GET("", ticketsUserListHandler)
	group.GET("/", ticketsUserListHandler)

	group.GET("/attachments/:attachmentId/content", ticketsAttachmentContentHandler)

	group.GET("/hosts/:hostId", ticketsHostListHandler)
	group.GET("/my-hosts", ticketsOwnerAllHandler)
	group.GET("/pending-count", ticketsPendingCountHandler)

	group.GET("/:id", ticketsDetailHandler)
	group.GET("/:id/messages", ticketsMessagesListHandler)
	group.POST("/:id/messages", ticketsReplyHandler)
	group.PATCH("/:id/status", ticketsStatusHandler)
	group.POST("/:id/close", ticketsCloseHandler)

	// ==================== 管理员 API ====================
	admin := r.Group("/api/tickets", middleware.Authenticate(), middleware.RequireAdmin())
	admin.GET("/:id/support-context", ticketsSupportContextHandler)
	admin.POST("/:id/internal-notes", ticketsInternalNoteHandler)
	admin.POST("/:id/links", ticketsLinkHandler)
	admin.POST("/:id/notify", ticketsNotifyHandler)
	admin.DELETE("/:id/messages/:messageId", ticketsDeleteMessageHandler)
}

func init() {
	Register(RegisterTickets)
}
