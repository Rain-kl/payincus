// Port of server/src/routes/api-tokens.ts.
//
// This is a credential module, so the security invariants of the TypeScript
// source are load-bearing:
//
//   - The plaintext token is returned exactly once, in the 201 response body of
//     POST /api/api-tokens. Only the SHA-256 digest is persisted
//     (public_api_tokens.token_hash); lib.GeneratePublicAPIToken /
//     lib.HashPublicAPIToken / lib.GetPublicAPITokenPrefix provide the exact
//     Node-compatible primitives.
//   - Neither the list projection nor the create/revoke projections ever carry
//     tokenHash: serializePublicApiToken() in TS selects only id/name/
//     tokenPrefix/scopes/lastUsedAt/revokedAt/expiresAt/createdAt/updatedAt, and
//     apiTokenView below has no field able to carry a digest.
//   - No audit log line ever contains the plaintext token (the Node messages only
//     embed the token *name* and the *scope list*).
//
// The TS routes declare `onRequest: [fastify.authenticate]` and no JSON schema.
// Because there is no `schema.body`, Fastify's AJV never runs on this module, so
// the only validation is done by the normalize* helpers. What still has to be
// reproduced is Fastify's *body parser* layer (a global behaviour that runs
// before the handler): the 415 for unsupported media types, the 400 for an empty
// or malformed `application/json` body, BOM stripping, secure-json-parse's
// prototype-poisoning rejection, and the 10 MB bodyLimit. See
// readAPITokenBody() below.
//
// The TypeScript module is the only place that touches public_api_tokens for the
// owner-facing API, and the port is limited to four new files (no db/api_tokens.go),
// so the four SQL statements live at the bottom of this file instead of in db/.
package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// ==================== module error payloads ====================
//
// The Node module replies with literal `{ error, code }` pairs that are NOT part
// of the shared lib/errors.ts catalog, so apiError()/ErrorMessages do not know
// them. They are declared here with byte-identical codes and messages, mirroring
// the approach taken in lib/public_api_auth.go.
const (
	apiTokenInvalidNameCode     = "INVALID_TOKEN_NAME"
	apiTokenInvalidScopesCode   = "INVALID_API_SCOPES"
	apiTokenInvalidExpiresCode  = "INVALID_EXPIRES_AT"
	apiTokenInvalidIDCode       = "INVALID_TOKEN_ID"
	apiTokenNotFoundCode        = "API_TOKEN_NOT_FOUND"
	apiTokenInvalidNameMessage  = "Invalid token name"
	apiTokenInvalidScopeMessage = "Invalid API scopes"
	apiTokenInvalidExpiryMsg    = "Invalid expiry time"
	apiTokenInvalidIDMessage    = "Invalid token id"
	apiTokenNotFoundMessage     = "API token not found"
)

// Fastify body-parser messages. These are surfaced verbatim by the generic
// branch of setErrorHandler in app.ts (`reply.code(status).send({ error })`).
const (
	apiTokenEmptyJSONBodyMessage   = "Body cannot be empty when content-type is set to 'application/json'"
	apiTokenInvalidJSONBodyMessage = "Body is not valid JSON but content-type is set to 'application/json'"
	apiTokenBodyTooLargeMessage    = "Request body is too large"
	apiTokenBodyLimitDefault       = 10 * 1024 * 1024
)

// ==================== response shapes ====================

// apiTokenView mirrors serializePublicApiToken(). It deliberately has no
// tokenHash/token field: the digest and the plaintext can never be echoed by the
// list endpoint or by any nested projection.
type apiTokenView struct {
	ID          int32   `json:"id"`
	Name        string  `json:"name"`
	TokenPrefix string  `json:"tokenPrefix"`
	Scopes      []any   `json:"scopes"`
	LastUsedAt  *string `json:"lastUsedAt"`
	RevokedAt   *string `json:"revokedAt"`
	ExpiresAt   *string `json:"expiresAt"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
}

// apiTokenRow is the raw SQL projection (column names, not Prisma field names).
type apiTokenRow struct {
	ID          int32      `gorm:"column:id"`
	UserID      int32      `gorm:"column:user_id"`
	Name        string     `gorm:"column:name"`
	TokenPrefix string     `gorm:"column:token_prefix"`
	TokenHash   string     `gorm:"column:token_hash"`
	Scopes      []byte     `gorm:"column:scopes"`
	LastUsedAt  *time.Time `gorm:"column:last_used_at"`
	RevokedAt   *time.Time `gorm:"column:revoked_at"`
	ExpiresAt   *time.Time `gorm:"column:expires_at"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

// apiTokenISOTime mirrors Date.prototype.toISOString() (UTC, millisecond
// precision, trailing Z).
func apiTokenISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func apiTokenOptionalISOTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := apiTokenISOTime(*value)
	return &formatted
}

// apiTokenScopesJSON mirrors `Array.isArray(token.scopes) ? token.scopes : []`.
// The stored JSON is passed through unchanged (it is never re-normalized), but a
// non-array value degrades to an empty array so the wire shape is always a list.
func apiTokenScopesJSON(raw []byte) []any {
	if len(raw) == 0 {
		return []any{}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return []any{}
	}
	items, ok := decoded.([]any)
	if !ok {
		return []any{}
	}
	return items
}

func apiTokenViewFromRow(row apiTokenRow) apiTokenView {
	return apiTokenView{
		ID:          row.ID,
		Name:        row.Name,
		TokenPrefix: row.TokenPrefix,
		Scopes:      apiTokenScopesJSON(row.Scopes),
		LastUsedAt:  apiTokenOptionalISOTime(row.LastUsedAt),
		RevokedAt:   apiTokenOptionalISOTime(row.RevokedAt),
		ExpiresAt:   apiTokenOptionalISOTime(row.ExpiresAt),
		CreatedAt:   apiTokenISOTime(row.CreatedAt),
		UpdatedAt:   apiTokenISOTime(row.UpdatedAt),
	}
}

// ==================== audit log messages ====================
//
// Mirrors the two createLog() calls. Both embed only the token *name* (and, for
// create, the scope list) — never the plaintext token and never the digest.

func apiTokenCreateLogContent(name string, scopes []lib.PublicAPIScope) string {
	parts := make([]string, len(scopes))
	for i, scope := range scopes {
		parts[i] = string(scope)
	}
	return `Created public API token "` + name + `" with scopes: ` + strings.Join(parts, ", ")
}

func apiTokenRevokeLogContent(name string) string {
	return `Revoked public API token "` + name + `"`
}

// ==================== Fastify body-parser emulation ====================

// apiTokenBody is the parsed request body. Present=false is Fastify's
// `request.body === undefined` (no body was sent at all).
type apiTokenBody struct {
	Value   any
	Present bool
}

// apiTokenBodyError is a parser failure that Fastify would have raised before
// the handler ran; it is rendered as `{ error }` with the given status.
type apiTokenBodyError struct {
	Status  int
	Message string
}

// apiTokenBodyLimit mirrors `bodyLimit: parseInt(process.env.BODY_LIMIT || '10485760', 10)`.
// parseInt consumes a leading integer and ignores the rest, so "100abc" is 100;
// a value with no leading digits yields NaN in Node and the SSE default here.
func apiTokenBodyLimit() int64 {
	raw := strings.TrimSpace(os.Getenv("BODY_LIMIT"))
	if raw == "" {
		return apiTokenBodyLimitDefault
	}
	end := 0
	if raw[0] == '+' || raw[0] == '-' {
		end = 1
	}
	digitsStart := end
	for end < len(raw) && raw[end] >= '0' && raw[end] <= '9' {
		end++
	}
	if end == digitsStart {
		return apiTokenBodyLimitDefault
	}
	parsed, err := strconv.ParseInt(raw[:end], 10, 64)
	if err != nil || parsed <= 0 {
		return apiTokenBodyLimitDefault
	}
	return parsed
}

// apiTokenBodyParserKind reproduces ContentTypeParser#getParser: the registered
// content types are matched case-insensitively as a *prefix* that must be
// followed by end-of-string, ';', ' ' or '\t'. All three parsers registered by
// the Node app are covered: application/json and text/plain (built in) plus
// multipart/form-data (added by @fastify/multipart).
func apiTokenBodyParserKind(contentType string) string {
	lower := strings.ToLower(contentType)
	for _, candidate := range []struct {
		prefix string
		kind   string
	}{
		{"multipart/form-data", "multipart"},
		{"application/json", "json"},
		{"text/plain", "text"},
	} {
		if !strings.HasPrefix(lower, candidate.prefix) {
			continue
		}
		rest := lower[len(candidate.prefix):]
		if rest == "" || rest[0] == ';' || rest[0] == ' ' || rest[0] == '\t' {
			return candidate.kind
		}
	}
	return ""
}

// readAPITokenBody mirrors Fastify's request pipeline for a body-carrying method:
// get the content type, pick the parser, enforce bodyLimit, then parse. Because
// the Node routes put authentication in `onRequest` (which runs before body
// parsing), the caller must run the auth middleware first — exactly like the
// handler order below.
func readAPITokenBody(c *gin.Context) (apiTokenBody, *apiTokenBodyError) {
	request := c.Request

	switch request.Method {
	case http.MethodGet, http.MethodHead, http.MethodTrace:
		// Fastify's bodyless method set: the body is never parsed.
		return apiTokenBody{}, nil
	}

	contentType := request.Header.Get("Content-Type")
	if contentType == "" {
		// No content type: Fastify only skips parsing when there is provably no
		// body (no transfer-encoding and no/zero content-length). Otherwise it
		// looks up the '' parser, which does not exist -> 415.
		// net/http has already turned those two headers into TransferEncoding and
		// ContentLength (0 = absent, -1 = chunked/unknown).
		if len(request.TransferEncoding) == 0 && request.ContentLength <= 0 {
			return apiTokenBody{}, nil
		}
		return apiTokenBody{}, &apiTokenBodyError{
			Status:  http.StatusUnsupportedMediaType,
			Message: "Unsupported Media Type: undefined",
		}
	}

	kind := apiTokenBodyParserKind(contentType)
	if kind == "" {
		return apiTokenBody{}, &apiTokenBodyError{
			Status:  http.StatusUnsupportedMediaType,
			Message: "Unsupported Media Type: " + contentType,
		}
	}

	limit := apiTokenBodyLimit()
	if request.ContentLength > limit {
		return apiTokenBody{}, &apiTokenBodyError{Status: http.StatusRequestEntityTooLarge, Message: apiTokenBodyTooLargeMessage}
	}

	data, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	if err != nil {
		return apiTokenBody{}, &apiTokenBodyError{Status: http.StatusBadRequest, Message: apiTokenInvalidJSONBodyMessage}
	}
	if int64(len(data)) > limit {
		return apiTokenBody{}, &apiTokenBodyError{Status: http.StatusRequestEntityTooLarge, Message: apiTokenBodyTooLargeMessage}
	}

	switch kind {
	case "multipart":
		// @fastify/multipart is registered with attachFieldsToBody:false, so the
		// parser consumes the stream and leaves request.body undefined.
		return apiTokenBody{}, nil
	case "text":
		return apiTokenBody{Value: string(data), Present: true}, nil
	}

	// application/json: an empty payload is the dedicated "empty body" error; a
	// leading BOM is then stripped (secure-json-parse does this before JSON.parse),
	// which can itself leave nothing to parse ("\uFEFF" alone is JSON-parse failure
	// in Node, not an empty-body error).
	if len(data) == 0 {
		return apiTokenBody{}, &apiTokenBodyError{Status: http.StatusBadRequest, Message: apiTokenEmptyJSONBodyMessage}
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return apiTokenBody{}, &apiTokenBodyError{Status: http.StatusBadRequest, Message: apiTokenInvalidJSONBodyMessage}
	}
	if apiTokenHasForbiddenPrototypeKey(decoded) {
		// secure-json-parse throws a SyntaxError with protoAction/constructorAction
		// 'error', which Fastify turns into the same "not valid JSON" message.
		return apiTokenBody{}, &apiTokenBodyError{Status: http.StatusBadRequest, Message: apiTokenInvalidJSONBodyMessage}
	}
	return apiTokenBody{Value: decoded, Present: true}, nil
}

// apiTokenHasForbiddenPrototypeKey mirrors secure-json-parse's scan: any object
// owning a `__proto__` key, or a `constructor` key whose value is an object that
// itself owns a `prototype` key, aborts the parse. (JSON.parse creates these as
// ordinary own properties, so a plain decode-and-walk is equivalent to the
// library's regex pre-check plus filter.)
func apiTokenHasForbiddenPrototypeKey(value any) bool {
	switch node := value.(type) {
	case map[string]any:
		if _, present := node["__proto__"]; present {
			return true
		}
		if constructor, present := node["constructor"]; present {
			if object, ok := constructor.(map[string]any); ok {
				if _, hasPrototype := object["prototype"]; hasPrototype {
					return true
				}
			}
		}
		for _, child := range node {
			if apiTokenHasForbiddenPrototypeKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if apiTokenHasForbiddenPrototypeKey(child) {
				return true
			}
		}
	}
	return false
}

func respondAPITokenBodyError(c *gin.Context, bodyErr *apiTokenBodyError) {
	c.JSON(bodyErr.Status, gin.H{"error": bodyErr.Message})
}

// apiTokenField mirrors `request.body?.key`: a non-object body (string, number,
// array, null) has no such field, exactly like optional chaining.
func apiTokenField(body apiTokenBody, key string) any {
	if !body.Present {
		return nil
	}
	object, ok := body.Value.(map[string]any)
	if !ok {
		return nil
	}
	return object[key]
}

// ==================== expiresAt normalization ====================

// apiTokenISODatePattern covers the ECMAScript Date Time String Format that V8
// parses deterministically: YYYY, YYYY-MM, YYYY-MM-DD, optionally followed by
// THH:mm[:ss[.sss]] and a Z / ±HH:mm / ±HHmm offset. A space is accepted as the
// date/time separator because V8 accepts it there as well.
var apiTokenISODatePattern = regexp.MustCompile(
	`^([+-]\d{6}|\d{4})(?:-(\d{2})(?:-(\d{2}))?)?(?:[Tt ](\d{2}):(\d{2})(?::(\d{2})(?:\.(\d{1,9}))?)?(Z|z|[+-]\d{2}:?\d{2})?)?$`,
)

// apiTokenLegacyDatePattern covers the lenient fallback parser V8 uses when the
// string is not in the ES Date Time String Format: unpadded (or zero-padded)
// components with '/', '.' or '-' as the date separator, and an optional
// space-separated time. The date/time separator must be a space — V8 rejects
// "2027-1-1T00:00:00". Trailing "Z" / "GMT" / "UTC" and numeric offsets are
// accepted. Leading zeros are not significant ("2027-0001-01" is January 1st),
// so every numeric component is a plain digit run validated by value.
var apiTokenLegacyDatePattern = regexp.MustCompile(
	`^(\d{4,})[-/.](\d+)(?:[-/.](\d+))?(?: +(\d+):(\d+)(?::(\d+)(?:\.(\d+))?)?)? *(Z|z|GMT|UTC|[+-]\d{1,2}:?\d{2})?$`,
)

// apiTokenDateParts is the shared decomposition of both patterns.
type apiTokenDateParts struct {
	Year        int
	Month       int
	Day         int
	HasTime     bool
	Hour        int
	Minute      int
	Second      int
	Millisecond int
	Offset      string
}

// apiTokenParseFraction truncates and right-pads a fractional-second digit run to
// whole milliseconds, as V8 does.
func apiTokenParseFraction(fraction string) int {
	if fraction == "" {
		return 0
	}
	if len(fraction) > 3 {
		fraction = fraction[:3]
	}
	for len(fraction) < 3 {
		fraction += "0"
	}
	millisecond, _ := strconv.Atoi(fraction)
	return millisecond
}

// apiTokenParseOffset maps a trailing zone designator to a location. An empty
// designator means "no offset given".
func apiTokenParseOffset(offset string) (*time.Location, bool) {
	switch offset {
	case "":
		return nil, true
	case "Z", "z", "GMT", "UTC":
		return time.UTC, true
	}

	sign := 1
	if offset[0] == '-' {
		sign = -1
	}
	digits := strings.ReplaceAll(offset[1:], ":", "")
	if len(digits) != 3 && len(digits) != 4 {
		return nil, false
	}
	// "+8:00" and "+0800" both end in two minute digits; the hour part is 1-2.
	minutesPart := digits[len(digits)-2:]
	hoursPart := digits[:len(digits)-2]
	offsetHours, err := strconv.Atoi(hoursPart)
	if err != nil {
		return nil, false
	}
	offsetMinutes, err := strconv.Atoi(minutesPart)
	if err != nil || offsetHours > 23 || offsetMinutes > 59 {
		return nil, false
	}
	return time.FixedZone("", sign*(offsetHours*3600+offsetMinutes*60)), true
}

// apiTokenBuildTime validates the component ranges and builds the instant.
// Out-of-range month/day/hour combinations roll over exactly like V8
// ("2027-02-30" → 2027-03-02, hour 24 → next midnight), except where V8 rejects
// the string outright (day 00 or 32+, hour > 24, hour 24 with a non-zero minute).
func apiTokenBuildTime(parts apiTokenDateParts, defaultLocation *time.Location) (time.Time, bool) {
	if parts.Month < 1 || parts.Month > 12 || parts.Day < 1 || parts.Day > 31 {
		return time.Time{}, false
	}

	location := defaultLocation
	if parts.Offset != "" {
		parsed, ok := apiTokenParseOffset(parts.Offset)
		if !ok {
			return time.Time{}, false
		}
		location = parsed
	}

	if !parts.HasTime {
		return time.Date(parts.Year, time.Month(parts.Month), parts.Day, 0, 0, 0, 0, location), true
	}

	if parts.Minute > 59 || parts.Second > 59 {
		return time.Time{}, false
	}
	// V8 only allows hour 24 when the rest of the time is zero.
	if parts.Hour > 24 || (parts.Hour == 24 && (parts.Minute != 0 || parts.Second != 0 || parts.Millisecond != 0)) {
		return time.Time{}, false
	}

	return time.Date(
		parts.Year, time.Month(parts.Month), parts.Day,
		parts.Hour, parts.Minute, parts.Second, parts.Millisecond*int(time.Millisecond),
		location,
	), true
}

// parseAPITokenJSDatetime mirrors `new Date(value)` for the formats V8 parses:
// the ES Date Time String Format (date-only = UTC, date-time without an offset =
// local) and the legacy fallback (always local unless an offset is given).
// Free-form English forms ("Jan 1 2027", "01/02/2027") are NOT reproduced and are
// reported as invalid; see the module report.
func parseAPITokenJSDatetime(value string) (time.Time, bool) {
	if match := apiTokenISODatePattern.FindStringSubmatch(value); match != nil {
		parts := apiTokenDateParts{Year: atoiOr(match[1], 0), Month: 1, Day: 1, Offset: match[8]}
		if match[2] != "" {
			parts.Month = atoiOr(match[2], 0)
		}
		if match[3] != "" {
			parts.Day = atoiOr(match[3], 0)
		}
		if match[4] != "" {
			parts.HasTime = true
			parts.Hour = atoiOr(match[4], 0)
			parts.Minute = atoiOr(match[5], 0)
			parts.Second = atoiOr(match[6], 0)
			parts.Millisecond = apiTokenParseFraction(match[7])
		}
		// A date-only ISO string is UTC; a date-time without an offset is local.
		defaultLocation := time.Local
		if !parts.HasTime {
			defaultLocation = time.UTC
		}
		return apiTokenBuildTime(parts, defaultLocation)
	}

	if match := apiTokenLegacyDatePattern.FindStringSubmatch(value); match != nil {
		parts := apiTokenDateParts{
			Year:   atoiOr(match[1], 0),
			Month:  atoiOr(match[2], 0),
			Day:    1,
			Offset: match[8],
		}
		if match[3] != "" {
			parts.Day = atoiOr(match[3], 0)
		}
		if match[4] != "" {
			parts.HasTime = true
			parts.Hour = atoiOr(match[4], 0)
			parts.Minute = atoiOr(match[5], 0)
			parts.Second = atoiOr(match[6], 0)
			parts.Millisecond = apiTokenParseFraction(match[7])
		}
		// The legacy parser always interprets wall-clock time as local.
		return apiTokenBuildTime(parts, time.Local)
	}

	return time.Time{}, false
}

// atoiOr mirrors a lenient Number() over an already-regex-validated digit run.
func atoiOr(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// normalizeAPITokenExpiresAt mirrors normalizeExpiresAt():
//
//	undefined | null | ''  -> null (no expiry), valid
//	non-string             -> 'invalid'
//	unparseable / <= now   -> 'invalid'
//
// The returned time is UTC-truncated to milliseconds, matching the Date value
// Prisma writes into the timestamp(3) column.
func normalizeAPITokenExpiresAt(value any, now time.Time) (*time.Time, bool) {
	if value == nil {
		return nil, true
	}
	text, ok := value.(string)
	if !ok {
		return nil, false
	}
	if text == "" {
		return nil, true
	}

	parsed, ok := parseAPITokenJSDatetime(text)
	if !ok || !parsed.After(now) {
		return nil, false
	}
	utc := parsed.UTC().Truncate(time.Millisecond)
	return &utc, true
}

// ==================== persistence ====================

const apiTokenSelectColumns = `
	id, user_id, name, token_prefix, token_hash, scopes,
	last_used_at, revoked_at, expires_at, created_at, updated_at`

func listAPITokensByUserID(ctx context.Context, userID int32) ([]apiTokenRow, error) {
	var rows []apiTokenRow
	err := db.DB.WithContext(ctx).Raw(
		"SELECT "+apiTokenSelectColumns+
			" FROM public_api_tokens WHERE user_id = ? ORDER BY created_at DESC", userID).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func findAPITokenForUser(ctx context.Context, id int32, userID int32) (*apiTokenRow, error) {
	var rows []apiTokenRow
	err := db.DB.WithContext(ctx).Raw(
		"SELECT "+apiTokenSelectColumns+
			" FROM public_api_tokens WHERE id = ? AND user_id = ? LIMIT 1", id, userID).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

type apiTokenCreateData struct {
	UserID      int32
	Name        string
	TokenPrefix string
	TokenHash   string
	Scopes      string
	ExpiresAt   *time.Time
}

func createAPIToken(ctx context.Context, data apiTokenCreateData) (apiTokenRow, error) {
	// created_at is left to its column default, exactly like a Prisma create that
	// does not provide it; updated_at is supplied because the column has no
	// default (Prisma's @updatedAt always writes it).
	var rows []apiTokenRow
	err := db.DB.WithContext(ctx).Raw(
		"INSERT INTO public_api_tokens (user_id, name, token_prefix, token_hash, scopes, expires_at, updated_at)"+
			" VALUES (?, ?, ?, ?, ?::jsonb, ?, ?)"+
			" RETURNING "+apiTokenSelectColumns,
		data.UserID, data.Name, data.TokenPrefix, data.TokenHash, data.Scopes, data.ExpiresAt, time.Now().UTC()).
		Scan(&rows).Error
	if err != nil {
		return apiTokenRow{}, err
	}
	if len(rows) == 0 {
		return apiTokenRow{}, io.ErrUnexpectedEOF
	}
	return rows[0], nil
}

func revokeAPIToken(ctx context.Context, id int32) error {
	now := time.Now().UTC()
	return db.DB.WithContext(ctx).Exec(
		"UPDATE public_api_tokens SET revoked_at = ?, updated_at = ? WHERE id = ?", now, now, id).
		Error
}

// ==================== routes ====================

// RegisterAPITokens mounts /api/api-tokens. Every route requires a web JWT,
// mirroring the per-route `onRequest: [fastify.authenticate]` in the TS module.
//
// Fastify's prefix handling makes the plugin's '/' route answer both
// '/api/api-tokens' and '/api/api-tokens/' (find-my-way registers both), while
// '/:id' only matches without a trailing slash — hence the explicit pairing here.
func RegisterAPITokens(r *gin.Engine) {
	group := r.Group("/api/api-tokens", middleware.Authenticate())

	list := func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		rows, err := listAPITokensByUserID(c.Request.Context(), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		tokens := make([]apiTokenView, 0, len(rows))
		for _, row := range rows {
			tokens = append(tokens, apiTokenViewFromRow(row))
		}

		// `scopes: PUBLIC_API_SCOPES` is the full catalog, not the token's scopes.
		c.JSON(http.StatusOK, gin.H{
			"scopes": lib.PublicAPIScopes,
			"tokens": tokens,
		})
	}

	create := func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		body, bodyErr := readAPITokenBody(c)
		if bodyErr != nil {
			respondAPITokenBodyError(c, bodyErr)
			return
		}

		name := lib.NormalizePublicAPITokenName(apiTokenField(body, "name"))
		scopes := lib.NormalizePublicAPIScopes(apiTokenField(body, "scopes"))
		expiresAt, expiresValid := normalizeAPITokenExpiresAt(apiTokenField(body, "expiresAt"), time.Now())

		// The validation order is part of the contract: name, then scopes, then expiry.
		if name == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": apiTokenInvalidNameMessage, "code": apiTokenInvalidNameCode})
			return
		}
		if scopes == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": apiTokenInvalidScopeMessage, "code": apiTokenInvalidScopesCode})
			return
		}
		if !expiresValid {
			c.JSON(http.StatusBadRequest, gin.H{"error": apiTokenInvalidExpiryMsg, "code": apiTokenInvalidExpiresCode})
			return
		}

		scopesJSON, err := json.Marshal(scopes)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		// Plaintext token: generated here, returned once below, never persisted.
		rawToken := lib.GeneratePublicAPIToken()
		record, err := createAPIToken(c.Request.Context(), apiTokenCreateData{
			UserID:      user.ID,
			Name:        *name,
			TokenPrefix: lib.GetPublicAPITokenPrefix(rawToken),
			TokenHash:   lib.HashPublicAPIToken(rawToken),
			Scopes:      string(scopesJSON),
			ExpiresAt:   expiresAt,
		})
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		userID := int64(user.ID)
		db.CreateLog(c.Request.Context(), &userID, db.LogModuleSecurity, "public_api.token_create",
			apiTokenCreateLogContent(*name, scopes), db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusCreated, gin.H{
			"token":    rawToken,
			"apiToken": apiTokenViewFromRow(record),
		})
	}

	revoke := func(c *gin.Context) {
		user := middleware.CurrentUser(c)

		// Fastify parses the body before the handler runs, so a malformed JSON body
		// wins over an invalid route id.
		if _, bodyErr := readAPITokenBody(c); bodyErr != nil {
			respondAPITokenBodyError(c, bodyErr)
			return
		}

		tokenID, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": apiTokenInvalidIDMessage, "code": apiTokenInvalidIDCode})
			return
		}

		token, err := findAPITokenForUser(c.Request.Context(), int32(tokenID), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if token == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": apiTokenNotFoundMessage, "code": apiTokenNotFoundCode})
			return
		}

		if token.RevokedAt == nil {
			if err := revokeAPIToken(c.Request.Context(), token.ID); err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
				return
			}

			userID := int64(user.ID)
			db.CreateLog(c.Request.Context(), &userID, db.LogModuleSecurity, "public_api.token_revoke",
				apiTokenRevokeLogContent(token.Name), db.LogResultSuccess, db.CreateLogOptions{})
		}

		c.JSON(http.StatusOK, gin.H{"message": "API token revoked"})
	}

	for _, path := range []string{"", "/"} {
		group.GET(path, list)
		group.POST(path, create)
	}
	group.DELETE("/:id", revoke)
}

func init() {
	Register(RegisterAPITokens)
}
