// Port of the admin-facing half of server/src/lib/oauth-provider.ts: the OAuth
// *application* registry (oauth_client_apps) plus grant listing/revocation. The
// user login-provider config in db/oauth.go is a different module.
//
// Security notes:
//
//   - client_secret is only ever returned as plaintext by create and rotate — the
//     exact two places the TypeScript returns it. Only its sha256 hex digest is
//     stored (client_secret_hash); list/update responses have no secret field at
//     all, so the digest can never leak either.
//   - Secret material is never written to the audit log; log lines carry the app's
//     name or id only.
//
// Prefixing: db is a flat package shared by parallel module ports, so everything
// here is prefixed `OAuthApps` / `oauthApps`.
package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/net/idna"
	"gorm.io/gorm"
)

// OAuthAppsPublicAPIScopes mirrors lib.PublicAPIScopes (the Node PUBLIC_API_SCOPES
// tuple) in the exact catalog order. It is duplicated rather than imported because
// lib already imports db, so a db → lib import would be a cycle. The external test
// package db_test asserts the two lists stay identical.
var OAuthAppsPublicAPIScopes = []string{
	"profile:read",
	"profile:write",
	"balance:read",
	"balance:write",
	"billing:read",
	"products:read",
	"services:read",
	"services:operate",
	"services:billing",
	"orders:read",
	"tickets:read",
	"tickets:write",
	"notifications:read",
	"notifications:send",
}

const (
	// OAuthAppsClientIDPrefix mirrors OAUTH_CLIENT_ID_PREFIX.
	OAuthAppsClientIDPrefix = "pocli_"
	// OAuthAppsClientSecretPrefix mirrors OAUTH_CLIENT_SECRET_PREFIX.
	OAuthAppsClientSecretPrefix = "posec_"

	// oauthAppsClientIDRandomBytes mirrors randomBytes(18) for the client id.
	oauthAppsClientIDRandomBytes = 18
	// oauthAppsClientSecretRandomBytes mirrors randomBytes(32) for the secret.
	oauthAppsClientSecretRandomBytes = 32

	// oauthAppsMaxNameLength mirrors the `<= 120` check of
	// normalizeOAuthAppName.
	oauthAppsMaxNameLength = 120
	// oauthAppsMaxRedirectURIs / oauthAppsMaxRedirectURILength mirror
	// normalizeOAuthRedirectUris.
	oauthAppsMaxRedirectURIs      = 20
	oauthAppsMaxRedirectURILength = 500
	// oauthAppsMaxScopes mirrors the `> 24` rejection of normalizeOAuthScopes.
	oauthAppsMaxScopes = 24
)

// oauthAppsLocalHostnames mirrors the ['localhost','127.0.0.1','[::1]'] list that
// allows plain http for local development redirects (compared against the parsed
// hostname, exactly like the TypeScript).
var oauthAppsLocalHostnames = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"[::1]":     true,
}

// oauthAppsNumericIDPattern mirrors `/^[1-9]\d*$/` used for the user search box.
var oauthAppsNumericIDPattern = regexp.MustCompile(`^[1-9]\d*$`)

// ==================== types ====================

// OAuthAppsClientApp is the serialized oauth_client_apps row
// (SerializedOAuthClientApp). It deliberately has no clientSecretHash field.
type OAuthAppsClientApp struct {
	ID                int32    `json:"id"`
	Name              string   `json:"name"`
	ClientID          string   `json:"clientId"`
	RedirectURIs      []string `json:"redirectUris"`
	Scopes            []string `json:"scopes"`
	Enabled           bool     `json:"enabled"`
	CreatedByUserID   int32    `json:"createdByUserId"`
	CreatedByUsername *string  `json:"createdByUsername"`
	CreatedAt         string   `json:"createdAt"`
	UpdatedAt         string   `json:"updatedAt"`
}

// OAuthAppsAppRef is the nested app projection of a grant.
type OAuthAppsAppRef struct {
	ID       int32  `json:"id"`
	Name     string `json:"name"`
	ClientID string `json:"clientId"`
	Enabled  bool   `json:"enabled"`
}

// OAuthAppsAuthorization is SerializedOAuthAuthorization.
type OAuthAppsAuthorization struct {
	ID               int32           `json:"id"`
	App              OAuthAppsAppRef `json:"app"`
	Scopes           []string        `json:"scopes"`
	Active           bool            `json:"active"`
	LastAuthorizedAt string          `json:"lastAuthorizedAt"`
	RevokedAt        *string         `json:"revokedAt"`
	CreatedAt        string          `json:"createdAt"`
	UpdatedAt        string          `json:"updatedAt"`
}

// OAuthAppsAdminAuthorizationUser is the user projection of an admin grant row.
type OAuthAppsAdminAuthorizationUser struct {
	ID       int32   `json:"id"`
	Username string  `json:"username"`
	Email    *string `json:"email"`
	Role     string  `json:"role"`
	Status   string  `json:"status"`
}

// OAuthAppsTokenStats mirrors tokenStats.
//
// The list endpoint returns the `{ grantId, activeAccessTokens, activeRefreshTokens }`
// object built by the stats loop, while the revoke endpoint passes a literal
// `{ activeAccessTokens: 0, activeRefreshTokens: 0 }` without grantId — hence the
// pointer plus omitempty.
type OAuthAppsTokenStats struct {
	GrantID             *int32 `json:"grantId,omitempty"`
	ActiveAccessTokens  int64  `json:"activeAccessTokens"`
	ActiveRefreshTokens int64  `json:"activeRefreshTokens"`
}

// OAuthAppsAdminAuthorization is SerializedAdminOAuthAuthorization.
type OAuthAppsAdminAuthorization struct {
	ID               int32                           `json:"id"`
	App              OAuthAppsAppRef                 `json:"app"`
	Scopes           []string                        `json:"scopes"`
	Active           bool                            `json:"active"`
	LastAuthorizedAt string                          `json:"lastAuthorizedAt"`
	RevokedAt        *string                         `json:"revokedAt"`
	CreatedAt        string                          `json:"createdAt"`
	UpdatedAt        string                          `json:"updatedAt"`
	User             OAuthAppsAdminAuthorizationUser `json:"user"`
	TokenStats       OAuthAppsTokenStats             `json:"tokenStats"`
}

// OAuthAppsAuthorizationListOptions mirrors listAdminOAuthAuthorizations' input.
type OAuthAppsAuthorizationListOptions struct {
	// AppID nil means "not filtered".
	AppID *int32
	// User nil means "not filtered"; the empty string is treated as absent, like
	// `input.user?.trim()`.
	User *string
	// Status is one of all/active/revoked/disabled.
	Status   string
	Page     int
	PageSize int
}

// OAuthAppsAuthorizationPage mirrors the listAdminOAuthAuthorizations return value.
type OAuthAppsAuthorizationPage struct {
	Authorizations []OAuthAppsAdminAuthorization `json:"authorizations"`
	Total          int64                         `json:"total"`
	Page           int                           `json:"page"`
	PageSize       int                           `json:"pageSize"`
	TotalPages     int64                         `json:"totalPages"`
}

// OAuthAppsCreateInput mirrors createOAuthClientApp's argument object.
type OAuthAppsCreateInput struct {
	Name            string
	RedirectURIs    []string
	Scopes          []string
	Enabled         bool
	CreatedByUserID int32
}

// OAuthAppsUpdateInput mirrors updateOAuthClientApp's argument object.
type OAuthAppsUpdateInput struct {
	Name         string
	RedirectURIs []string
	Scopes       []string
	Enabled      bool
}

// OAuthAppsSecretResult is the { app, clientSecret } pair returned by create and
// rotate. The secret is plaintext and must be surfaced exactly once.
type OAuthAppsSecretResult struct {
	App          OAuthAppsClientApp
	ClientSecret string
}

// oauthAppsRow is the raw SQL projection (client_secret_hash is read only for
// verification, never serialized).
type oauthAppsRow struct {
	ID               int32     `gorm:"column:id"`
	Name             string    `gorm:"column:name"`
	ClientID         string    `gorm:"column:client_id"`
	ClientSecretHash string    `gorm:"column:client_secret_hash"`
	RedirectURIs     []byte    `gorm:"column:redirect_uris"`
	Scopes           []byte    `gorm:"column:scopes"`
	Enabled          bool      `gorm:"column:enabled"`
	CreatedByUserID  int32     `gorm:"column:created_by_user_id"`
	CreatedAt        time.Time `gorm:"column:created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at"`
	Username         *string   `gorm:"column:username"`
}

// oauthAppsGrantRow is the joined oauth_grants row used by both the admin list and
// the revoke endpoint.
type oauthAppsGrantRow struct {
	ID               int32      `gorm:"column:id"`
	AppID            int32      `gorm:"column:app_id"`
	UserID           int32      `gorm:"column:user_id"`
	Scopes           []byte     `gorm:"column:scopes"`
	LastAuthorizedAt time.Time  `gorm:"column:last_authorized_at"`
	RevokedAt        *time.Time `gorm:"column:revoked_at"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`

	AppName     string `gorm:"column:app_name"`
	AppClientID string `gorm:"column:app_client_id"`
	AppEnabled  bool   `gorm:"column:app_enabled"`

	Username *string `gorm:"column:username"`
	Email    *string `gorm:"column:email"`
	Role     *string `gorm:"column:role"`
	Status   *string `gorm:"column:status"`
}

// ==================== URL serialization ====================
//
// normalizeOAuthRedirectUris re-serializes each entry through `new URL(...)`, so a
// stored redirect URI is the WHATWG canonical form, not the operator's input:
//
//	"HTTPS://Example.COM:443/Path" -> "https://example.com/Path"
//	"http://localhost:80/x"        -> "http://localhost/x"
//	"https://example.com/a/../b"   -> "https://example.com/b"
//	"https://e/p/a b"              -> "https://e/p/a%20b"
//	"https://example.com"          -> "https://example.com/"
//
// net/url cannot reproduce this (Go keeps the raw escapes, does not resolve dot
// segments and encodes a different set), so the parse/serialize is hand-rolled
// with the WHATWG steps and the encode sets measured against Node 26.

// oauthAppsDefaultPorts mirrors the special-scheme default ports that `new URL`
// drops from the serialization.
var oauthAppsDefaultPorts = map[string]string{
	"http":  "80",
	"https": "443",
}

// oauthAppsSerializeURL mirrors `new URL(input).toString()` after `hash = ”`.
// ok=false covers everything `new URL` throws on.
func oauthAppsSerializeURL(input string) (string, bool) {
	schemeEnd := strings.IndexByte(input, ':')
	if schemeEnd <= 0 {
		return "", false
	}
	scheme := strings.ToLower(input[:schemeEnd])
	if !oauthAppsValidScheme(scheme) {
		return "", false
	}
	// Only http/https carry an authority that this normalizer understands; the
	// caller's protocol check rejects the rest anyway.
	if scheme != "http" && scheme != "https" {
		return "", false
	}

	rest := input[schemeEnd+1:]

	// Fragment is discarded (`parsed.hash = ''`).
	if hashIndex := strings.IndexByte(rest, '#'); hashIndex >= 0 {
		rest = rest[:hashIndex]
	}

	// The query is preserved verbatim apart from its own encode set, including the
	// bare "?" form (`new URL('https://e/?')` keeps it).
	rawQuery := ""
	hasQuery := false
	if queryIndex := strings.IndexByte(rest, '?'); queryIndex >= 0 {
		rawQuery = rest[queryIndex+1:]
		hasQuery = true
		rest = rest[:queryIndex]
	}

	// Special schemes strip any number of leading slashes: `https:foo`,
	// `https:/example.com` and `https:///x` all resolve to host "foo"/"example.com"/"x".
	rest = strings.TrimLeft(rest, "/")
	authority := rest
	rawPath := ""
	if slashIndex := strings.IndexByte(rest, '/'); slashIndex >= 0 {
		authority = rest[:slashIndex]
		rawPath = rest[slashIndex:]
	}

	userinfo := ""
	if atIndex := strings.LastIndexByte(authority, '@'); atIndex >= 0 {
		userinfo = authority[:atIndex]
		authority = authority[atIndex+1:]
	}

	host := authority
	port := ""
	if strings.HasPrefix(authority, "[") {
		closeIndex := strings.IndexByte(authority, ']')
		if closeIndex < 0 {
			return "", false
		}
		host = authority[:closeIndex+1]
		remainder := authority[closeIndex+1:]
		if remainder != "" {
			if !strings.HasPrefix(remainder, ":") {
				return "", false
			}
			port = remainder[1:]
		}
	} else if colonIndex := strings.LastIndexByte(authority, ':'); colonIndex >= 0 {
		if strings.ContainsRune(authority[:colonIndex], ':') {
			// Bare IPv6 without brackets is a parse error in WHATWG.
			return "", false
		}
		host = authority[:colonIndex]
		port = authority[colonIndex+1:]
	}

	if host == "" {
		return "", false
	}

	normalizedHost, ok := oauthAppsNormalizeHost(host)
	if !ok {
		return "", false
	}

	// normalizeOAuthRedirectUris' protocol guard: https, or plain http to one of
	// the three literal local hostnames. This runs on the *parsed* host, so
	// `http://LOCALHOST/x` is accepted while `http://localhost.evil.com/x` is not.
	if scheme != "https" {
		if scheme != "http" || !oauthAppsLocalHostnames[normalizedHost] {
			return "", false
		}
	}

	normalizedPort, ok := oauthAppsNormalizePort(port, scheme)
	if !ok {
		return "", false
	}

	encodedPath := oauthAppsEncodePath(rawPath)
	encodedQuery := oauthAppsEncodeQuery(rawQuery)

	var builder strings.Builder
	builder.Grow(len(input) + 8)
	builder.WriteString(scheme)
	builder.WriteString("://")
	if userinfo != "" {
		builder.WriteString(oauthAppsEncodeUserinfo(userinfo))
		builder.WriteByte('@')
	}
	builder.WriteString(normalizedHost)
	if normalizedPort != "" {
		builder.WriteByte(':')
		builder.WriteString(normalizedPort)
	}
	builder.WriteString(encodedPath)
	if hasQuery {
		builder.WriteByte('?')
		builder.WriteString(encodedQuery)
	}
	return builder.String(), true
}

// oauthAppsValidScheme mirrors the URL scheme grammar.
func oauthAppsValidScheme(scheme string) bool {
	if scheme == "" {
		return false
	}
	if !((scheme[0] >= 'a' && scheme[0] <= 'z') || (scheme[0] >= 'A' && scheme[0] <= 'Z')) {
		return false
	}
	for index := 1; index < len(scheme); index++ {
		ch := scheme[index]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '+', ch == '-', ch == '.':
		default:
			return false
		}
	}
	return true
}

// oauthAppsNormalizeHost mirrors the URL parser's host step: percent-escapes are
// decoded first (the host is UTF-8 percent-decoded, unlike the path), forbidden
// code points reject the URL, then the name is lowercased and IDNA-encoded
// (`bücher.example` -> `xn--bcher-kva.example`).
func oauthAppsNormalizeHost(host string) (string, bool) {
	if strings.HasPrefix(host, "[") {
		// IPv6 literal: kept verbatim, already bracketed.
		return strings.ToLower(host), true
	}
	if strings.ContainsRune(host, ':') {
		return "", false
	}

	decoded, ok := oauthAppsPercentDecodeHost(host)
	if !ok {
		return "", false
	}

	// Forbidden host code points (the URL spec's forbidden domain code points plus
	// C0 controls, space and the delimiters that can never appear in an authority).
	for _, ch := range decoded {
		if ch <= 0x20 || ch == 0x7F {
			return "", false
		}
		switch ch {
		case '#', '%', '/', ':', '<', '>', '?', '@', '[', '\\', ']', '^', '|':
			return "", false
		}
	}

	for _, ch := range decoded {
		if ch > 0x7F {
			ascii, err := idna.Lookup.ToASCII(decoded)
			if err != nil {
				return "", false
			}
			return strings.ToLower(ascii), true
		}
	}
	return strings.ToLower(decoded), true
}

// oauthAppsPercentDecodeHost decodes %XX escapes as UTF-8. An incomplete escape is
// a parse error (matching the URL parser) rather than being kept literally.
func oauthAppsPercentDecodeHost(value string) (string, bool) {
	if !strings.ContainsRune(value, '%') {
		return value, true
	}
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		if value[index] != '%' {
			builder.WriteByte(value[index])
			continue
		}
		if index+2 >= len(value) {
			return "", false
		}
		decoded, err := hex.DecodeString(value[index+1 : index+3])
		if err != nil {
			return "", false
		}
		builder.WriteByte(decoded[0])
		index += 2
	}
	return builder.String(), true
}

// oauthAppsNormalizePort mirrors the port parsing: empty and default ports are
// dropped, leading zeros are stripped, and anything outside 0..65535 is an error.
func oauthAppsNormalizePort(port, scheme string) (string, bool) {
	if port == "" {
		return "", true
	}
	for index := 0; index < len(port); index++ {
		if port[index] < '0' || port[index] > '9' {
			return "", false
		}
	}
	trimmed := strings.TrimLeft(port, "0")
	if trimmed == "" {
		trimmed = "0"
	}
	if len(trimmed) > 5 {
		return "", false
	}
	value := 0
	for index := 0; index < len(trimmed); index++ {
		value = value*10 + int(trimmed[index]-'0')
	}
	if value > 65535 {
		return "", false
	}
	if defaultPort, ok := oauthAppsDefaultPorts[scheme]; ok && trimmed == defaultPort {
		return "", true
	}
	return trimmed, true
}

// oauthAppsEncodeUserinfo encodes the userinfo with the WHATWG userinfo set.
func oauthAppsEncodeUserinfo(value string) string {
	return oauthAppsPercentEncode(value, func(ch byte) bool {
		if ch >= 0x7F {
			return true
		}
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
			return false
		}
		switch ch {
		case '-', '.', '_', '~', '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=', ':', '%':
			return false
		}
		return true
	})
}

// oauthAppsEncodePath applies the WHATWG path encode set over the raw (still
// percent-escaped) path, then resolves dot segments.
func oauthAppsEncodePath(rawPath string) string {
	if rawPath == "" {
		return "/"
	}
	// Backslashes are path separators for special schemes.
	rawPath = strings.ReplaceAll(rawPath, `\`, "/")

	encoded := oauthAppsPercentEncode(rawPath, func(ch byte) bool {
		// Non-ASCII bytes are percent-encoded as UTF-8 octets, like the URL parser.
		if ch < 0x20 || ch >= 0x7F {
			return true
		}
		switch ch {
		case ' ', '"', '<', '>', '^', '`', '{', '}':
			return true
		}
		return false
	})
	return oauthAppsResolveDotSegments(encoded)
}

// oauthAppsEncodeQuery applies the WHATWG query encode set.
func oauthAppsEncodeQuery(rawQuery string) string {
	return oauthAppsPercentEncode(rawQuery, func(ch byte) bool {
		if ch < 0x20 || ch >= 0x7F {
			return true
		}
		switch ch {
		case ' ', '"', '<', '>', '\'':
			return true
		}
		return false
	})
}

// oauthAppsPercentEncode walks the string, percent-encoding every byte matched by
// `shouldEncode` (as UTF-8) and copying everything else, including existing
// `%XX` escapes, untouched.
func oauthAppsPercentEncode(value string, shouldEncode func(byte) bool) string {
	var builder strings.Builder
	builder.Grow(len(value) + 8)
	for index := 0; index < len(value); index++ {
		ch := value[index]
		if !shouldEncode(ch) {
			builder.WriteByte(ch)
			continue
		}
		builder.WriteByte('%')
		builder.WriteString(strings.ToUpper(hex.EncodeToString([]byte{ch})))
	}
	return builder.String()
}

// oauthAppsResolveDotSegments mirrors the URL parser's path state: single-dot
// segments are dropped, double-dot segments pop the previous segment, and `%2e`
// spellings count as dots.
func oauthAppsResolveDotSegments(path string) string {
	segments := strings.Split(path, "/")
	if len(segments) == 0 {
		return "/"
	}

	output := make([]string, 0, len(segments))
	lastWasDot := false
	for index, segment := range segments {
		if index == 0 {
			// The leading empty segment is the root marker; the leading "/" is
			// re-added when joining.
			continue
		}
		switch {
		case oauthAppsIsDoubleDotSegment(segment):
			if len(output) > 0 {
				output = output[:len(output)-1]
			}
			lastWasDot = true
			continue
		case oauthAppsIsSingleDotSegment(segment):
			lastWasDot = true
			continue
		}
		output = append(output, segment)
		lastWasDot = false
	}
	if lastWasDot {
		output = append(output, "")
	}

	return "/" + strings.Join(output, "/")
}

// oauthAppsIsSingleDotSegment mirrors the parser's single-dot test.
func oauthAppsIsSingleDotSegment(segment string) bool {
	return segment == "." || strings.EqualFold(segment, "%2e")
}

// oauthAppsIsDoubleDotSegment mirrors the parser's double-dot test.
func oauthAppsIsDoubleDotSegment(segment string) bool {
	switch strings.ToLower(segment) {
	case "..", ".%2e", "%2e.", "%2e%2e":
		return true
	}
	return false
}

// OAuthAppsNormalizeName mirrors normalizeOAuthAppName: trimmed, non-empty, at
// most 120 UTF-16 units.
func OAuthAppsNormalizeName(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || jsStringLengthDB(trimmed) > oauthAppsMaxNameLength {
		return "", false
	}
	return trimmed, true
}

// OAuthAppsNormalizeRedirectURIs mirrors normalizeOAuthRedirectUris, including the
// WHATWG URL re-serialization (default port dropped, dot segments resolved,
// percent-encoding of the path/query sets). Anything unparseable, non-https and
// non-local-http, or out of bounds rejects the whole list; duplicates collapse
// while keeping first-seen order.
func OAuthAppsNormalizeRedirectURIs(value any) ([]string, bool) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 || len(items) > oauthAppsMaxRedirectURIs {
		return nil, false
	}

	normalized := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || jsStringLengthDB(trimmed) > oauthAppsMaxRedirectURILength {
			return nil, false
		}
		serialized, ok := oauthAppsSerializeURL(trimmed)
		if !ok {
			return nil, false
		}
		if seen[serialized] {
			continue
		}
		seen[serialized] = true
		normalized = append(normalized, serialized)
	}
	return normalized, true
}

// OAuthAppsNormalizeScopes mirrors normalizeOAuthScopes: a space-separated string
// or an array, 1..24 entries, all from the Public API scope catalog, duplicates
// collapsed in first-seen order.
func OAuthAppsNormalizeScopes(value any) ([]string, bool) {
	var raw []any
	switch typed := value.(type) {
	case string:
		fields := strings.FieldsFunc(typed, oauthAppsIsJSSpace)
		raw = make([]any, 0, len(fields))
		for _, field := range fields {
			raw = append(raw, field)
		}
	case []any:
		raw = typed
	default:
		return nil, false
	}
	if len(raw) == 0 || len(raw) > oauthAppsMaxScopes {
		return nil, false
	}

	allowed := make(map[string]bool, len(OAuthAppsPublicAPIScopes))
	for _, scope := range OAuthAppsPublicAPIScopes {
		allowed[scope] = true
	}

	scopes := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, item := range raw {
		text, _ := item.(string)
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			continue
		}
		if !allowed[trimmed] {
			return nil, false
		}
		if seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		scopes = append(scopes, trimmed)
	}
	if len(scopes) == 0 {
		return nil, false
	}
	return scopes, true
}

// oauthAppsIsJSSpace is the rune predicate for JavaScript's /\s+/.
func oauthAppsIsJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// OAuthAppsSerializeScopes mirrors serializeScopes: an invalid stored value
// degrades to an empty array (never null).
func OAuthAppsSerializeScopes(raw []byte) []string {
	if len(raw) == 0 {
		return []string{}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return []string{}
	}
	scopes, ok := OAuthAppsNormalizeScopes(decoded)
	if !ok {
		return []string{}
	}
	return scopes
}

// oauthAppsRedirectURIsJSON mirrors
// `Array.isArray(app.redirectUris) ? app.redirectUris.filter(...) : []`.
func oauthAppsRedirectURIsJSON(raw []byte) []string {
	if len(raw) == 0 {
		return []string{}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return []string{}
	}
	items, ok := decoded.([]any)
	if !ok {
		return []string{}
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

// oauthAppsTimestamp mirrors Date#toISOString.
func oauthAppsTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// oauthAppsOptionalTimestamp mirrors `value?.toISOString() ?? null`.
func oauthAppsOptionalTimestamp(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := oauthAppsTimestamp(*value)
	return &formatted
}

// jsStringLengthDB mirrors JavaScript's String#length (UTF-16 code units).
func jsStringLengthDB(value string) int {
	units := 0
	for _, runeValue := range value {
		if runeValue > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return units
}

// oauthAppsJSONArray renders a []string as a jsonb literal.
func oauthAppsJSONArray(values []string) string {
	if values == nil {
		values = []string{}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

// ==================== secrets ====================

// OAuthAppsHashSecret mirrors hashOAuthSecret (sha256 hex).
func OAuthAppsHashSecret(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// oauthAppsGenerateClientID mirrors generateOAuthClientId: randomBytes(18) in
// base64url (unpadded).
func oauthAppsGenerateClientID() (string, error) {
	buffer := make([]byte, oauthAppsClientIDRandomBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return OAuthAppsClientIDPrefix + base64.RawURLEncoding.EncodeToString(buffer), nil
}

// oauthAppsGenerateClientSecret mirrors generateOAuthClientSecret.
func oauthAppsGenerateClientSecret() (string, error) {
	buffer := make([]byte, oauthAppsClientSecretRandomBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return OAuthAppsClientSecretPrefix + base64.RawURLEncoding.EncodeToString(buffer), nil
}

// oauthAppsSafeTimingEqual mirrors safeTimingEqual on the secret digest.
func oauthAppsSafeTimingEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ==================== serialization ====================

// oauthAppsClientApp serializes one row into SerializedOAuthClientApp.
func oauthAppsClientApp(row oauthAppsRow) OAuthAppsClientApp {
	var username *string
	if row.Username != nil && *row.Username != "" {
		username = row.Username
	}
	return OAuthAppsClientApp{
		ID:                row.ID,
		Name:              row.Name,
		ClientID:          row.ClientID,
		RedirectURIs:      oauthAppsRedirectURIsJSON(row.RedirectURIs),
		Scopes:            OAuthAppsSerializeScopes(row.Scopes),
		Enabled:           row.Enabled,
		CreatedByUserID:   row.CreatedByUserID,
		CreatedByUsername: username,
		CreatedAt:         oauthAppsTimestamp(row.CreatedAt),
		UpdatedAt:         oauthAppsTimestamp(row.UpdatedAt),
	}
}

// oauthAppsAdminAuthorization serializes one joined grant row.
func oauthAppsAdminAuthorization(row oauthAppsGrantRow, stats OAuthAppsTokenStats) OAuthAppsAdminAuthorization {
	app := OAuthAppsAppRef{
		ID:       row.AppID,
		Name:     row.AppName,
		ClientID: row.AppClientID,
		Enabled:  row.AppEnabled,
	}
	username := ""
	if row.Username != nil {
		username = *row.Username
	}
	role := ""
	if row.Role != nil {
		role = *row.Role
	}
	status := ""
	if row.Status != nil {
		status = *row.Status
	}
	return OAuthAppsAdminAuthorization{
		ID:               row.ID,
		App:              app,
		Scopes:           OAuthAppsSerializeScopes(row.Scopes),
		Active:           row.RevokedAt == nil && row.AppEnabled,
		LastAuthorizedAt: oauthAppsTimestamp(row.LastAuthorizedAt),
		RevokedAt:        oauthAppsOptionalTimestamp(row.RevokedAt),
		CreatedAt:        oauthAppsTimestamp(row.CreatedAt),
		UpdatedAt:        oauthAppsTimestamp(row.UpdatedAt),
		User: OAuthAppsAdminAuthorizationUser{
			ID:       row.UserID,
			Username: username,
			Email:    row.Email,
			Role:     role,
			Status:   status,
		},
		TokenStats: stats,
	}
}

// ==================== reads ====================

const oauthAppsSelectColumns = `a.id, a.name, a.client_id, a.client_secret_hash,
	a.redirect_uris, a.scopes, a.enabled, a.created_by_user_id, a.created_at, a.updated_at,
	u.username`

const oauthAppsGrantSelectColumns = `g.id, g.app_id, g.user_id, g.scopes, g.last_authorized_at,
	g.revoked_at, g.created_at, g.updated_at,
	a.name, a.client_id, a.enabled,
	u.username, u.email, u.role, u.status`

func scanOAuthAppsRow(scan func(...any) error) (oauthAppsRow, error) {
	var row oauthAppsRow
	err := scan(
		&row.ID, &row.Name, &row.ClientID, &row.ClientSecretHash,
		&row.RedirectURIs, &row.Scopes, &row.Enabled, &row.CreatedByUserID,
		&row.CreatedAt, &row.UpdatedAt, &row.Username,
	)
	return row, err
}

func scanOAuthAppsGrantRow(scan func(...any) error) (oauthAppsGrantRow, error) {
	var row oauthAppsGrantRow
	err := scan(
		&row.ID, &row.AppID, &row.UserID, &row.Scopes, &row.LastAuthorizedAt,
		&row.RevokedAt, &row.CreatedAt, &row.UpdatedAt,
		&row.AppName, &row.AppClientID, &row.AppEnabled,
		&row.Username, &row.Email, &row.Role, &row.Status,
	)
	return row, err
}

// OAuthAppsList mirrors listOAuthClientApps: newest created_at first.
func OAuthAppsList(ctx context.Context) ([]OAuthAppsClientApp, error) {
	rows, err := DB.WithContext(ctx).Raw(
		"SELECT " + oauthAppsSelectColumns + " FROM oauth_client_apps a" +
			" LEFT JOIN users u ON u.id = a.created_by_user_id" +
			" ORDER BY a.created_at DESC",
	).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	apps := make([]OAuthAppsClientApp, 0, 8)
	for rows.Next() {
		row, err := scanOAuthAppsRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		apps = append(apps, oauthAppsClientApp(row))
	}
	return apps, rows.Err()
}

// oauthAppsGetByID reads one app row (used by the update/rotate responses).
func oauthAppsGetByID(ctx context.Context, id int32) (*oauthAppsRow, error) {
	row, err := scanOAuthAppsRow(func(dest ...any) error {
		return DB.WithContext(ctx).Raw(
			"SELECT "+oauthAppsSelectColumns+" FROM oauth_client_apps a"+
				" LEFT JOIN users u ON u.id = a.created_by_user_id"+
				" WHERE a.id = ? LIMIT 1", id,
		).Row().Scan(dest...)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// OAuthAppsListAuthorizations mirrors listAdminOAuthAuthorizations.
func OAuthAppsListAuthorizations(ctx context.Context, options OAuthAppsAuthorizationListOptions) (OAuthAppsAuthorizationPage, error) {
	page := options.Page
	if page < 1 {
		page = 1
	}
	pageSize := options.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	conditions := make([]string, 0, 4)
	args := make([]any, 0, 6)

	if options.AppID != nil {
		conditions = append(conditions, "g.app_id = ?")
		args = append(args, *options.AppID)
	}

	if options.User != nil {
		userSearch := strings.TrimSpace(*options.User)
		if userSearch != "" {
			clauses := make([]string, 0, 3)
			if oauthAppsNumericIDPattern.MatchString(userSearch) {
				if numericID, err := strconv.ParseInt(userSearch, 10, 64); err == nil {
					clauses = append(clauses, "u.id = ?")
					args = append(args, numericID)
				}
			}
			// Prisma's `contains` compiles to LIKE with the pattern escaped for
			// `%`/`_`, so a literal underscore is not a wildcard.
			pattern := "%" + oauthAppsEscapeLike(userSearch) + "%"
			clauses = append(clauses, "u.username ILIKE ?", "u.email ILIKE ?")
			args = append(args, pattern, pattern)
			conditions = append(conditions, "("+strings.Join(clauses, " OR ")+")")
		}
	}

	switch options.Status {
	case "active":
		conditions = append(conditions, "g.revoked_at IS NULL", "a.enabled = true")
	case "revoked":
		conditions = append(conditions, "g.revoked_at IS NOT NULL")
	case "disabled":
		conditions = append(conditions, "g.revoked_at IS NULL", "a.enabled = false")
	}

	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}

	var total int64
	if err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM oauth_grants g"+
			" JOIN oauth_client_apps a ON a.id = g.app_id"+
			" JOIN users u ON u.id = g.user_id"+where, args...,
	).Scan(&total).Error; err != nil {
		return OAuthAppsAuthorizationPage{}, err
	}

	listArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := DB.WithContext(ctx).Raw(
		"SELECT "+oauthAppsGrantSelectColumns+" FROM oauth_grants g"+
			" JOIN oauth_client_apps a ON a.id = g.app_id"+
			" JOIN users u ON u.id = g.user_id"+where+
			" ORDER BY g.last_authorized_at DESC, g.id DESC LIMIT ? OFFSET ?", listArgs...,
	).Rows()
	if err != nil {
		return OAuthAppsAuthorizationPage{}, err
	}
	defer rows.Close()

	grants := make([]oauthAppsGrantRow, 0, pageSize)
	for rows.Next() {
		row, err := scanOAuthAppsGrantRow(rows.Scan)
		if err != nil {
			return OAuthAppsAuthorizationPage{}, err
		}
		grants = append(grants, row)
	}
	if err := rows.Err(); err != nil {
		return OAuthAppsAuthorizationPage{}, err
	}

	now := time.Now().UTC()
	stats := make(map[int32]OAuthAppsTokenStats, len(grants))
	for _, grant := range grants {
		accessTokens, err := oauthAppsCountLiveTokens(ctx, "oauth_access_tokens", grant.ID, now)
		if err != nil {
			return OAuthAppsAuthorizationPage{}, err
		}
		refreshTokens, err := oauthAppsCountLiveTokens(ctx, "oauth_refresh_tokens", grant.ID, now)
		if err != nil {
			return OAuthAppsAuthorizationPage{}, err
		}
		grantID := grant.ID
		stats[grant.ID] = OAuthAppsTokenStats{
			GrantID:             &grantID,
			ActiveAccessTokens:  accessTokens,
			ActiveRefreshTokens: refreshTokens,
		}
	}

	authorizations := make([]OAuthAppsAdminAuthorization, 0, len(grants))
	for _, grant := range grants {
		authorizations = append(authorizations, oauthAppsAdminAuthorization(grant, stats[grant.ID]))
	}

	totalPages := int64(1)
	if total > 0 {
		totalPages = (total + int64(pageSize) - 1) / int64(pageSize)
		if totalPages < 1 {
			totalPages = 1
		}
	}

	return OAuthAppsAuthorizationPage{
		Authorizations: authorizations,
		Total:          total,
		Page:           page,
		PageSize:       pageSize,
		TotalPages:     totalPages,
	}, nil
}

// oauthAppsCountLiveTokens mirrors the unrevoked, unexpired token count.
func oauthAppsCountLiveTokens(ctx context.Context, table string, grantID int32, now time.Time) (int64, error) {
	var count int64
	err := DB.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM "+table+" WHERE grant_id = ? AND revoked_at IS NULL AND expires_at > ?",
		grantID, now,
	).Scan(&count).Error
	return count, err
}

// oauthAppsEscapeLike mirrors Prisma's LIKE-pattern escaping for `contains`
// (backslash is the escape character, `%` and `_` are literal).
func oauthAppsEscapeLike(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, ch := range value {
		switch ch {
		case '\\', '%', '_':
			builder.WriteByte('\\')
		}
		builder.WriteRune(ch)
	}
	return builder.String()
}

// ==================== writes ====================

// OAuthAppsCreate mirrors createOAuthClientApp and returns the plaintext secret
// exactly once (only its digest is stored).
func OAuthAppsCreate(ctx context.Context, input OAuthAppsCreateInput) (OAuthAppsSecretResult, error) {
	clientID, err := oauthAppsGenerateClientID()
	if err != nil {
		return OAuthAppsSecretResult{}, err
	}
	clientSecret, err := oauthAppsGenerateClientSecret()
	if err != nil {
		return OAuthAppsSecretResult{}, err
	}

	var id int32
	if err := DB.WithContext(ctx).Raw(
		`INSERT INTO oauth_client_apps
			(name, client_id, client_secret_hash, redirect_uris, scopes, enabled,
			 created_by_user_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?::jsonb, ?::jsonb, ?, ?, NOW(), NOW())
		 RETURNING id`,
		input.Name, clientID, OAuthAppsHashSecret(clientSecret),
		oauthAppsJSONArray(input.RedirectURIs), oauthAppsJSONArray(input.Scopes),
		input.Enabled, input.CreatedByUserID,
	).Scan(&id).Error; err != nil {
		return OAuthAppsSecretResult{}, err
	}

	row, err := oauthAppsGetByID(ctx, id)
	if err != nil {
		return OAuthAppsSecretResult{}, err
	}
	if row == nil {
		return OAuthAppsSecretResult{}, errors.New("oauth app not found after create")
	}
	return OAuthAppsSecretResult{App: oauthAppsClientApp(*row), ClientSecret: clientSecret}, nil
}

// OAuthAppsUpdate mirrors updateOAuthClientApp. A missing row surfaces as
// gorm.ErrRecordNotFound (Prisma P2025), which the route turns into
// OAUTH_APP_UPDATE_FAILED with the Prisma message text.
func OAuthAppsUpdate(ctx context.Context, id int32, input OAuthAppsUpdateInput) (OAuthAppsClientApp, error) {
	result := DB.WithContext(ctx).Exec(
		`UPDATE oauth_client_apps
		 SET name = ?, redirect_uris = ?::jsonb, scopes = ?::jsonb, enabled = ?, updated_at = NOW()
		 WHERE id = ?`,
		input.Name, oauthAppsJSONArray(input.RedirectURIs), oauthAppsJSONArray(input.Scopes),
		input.Enabled, id,
	)
	if result.Error != nil {
		return OAuthAppsClientApp{}, result.Error
	}
	if result.RowsAffected == 0 {
		return OAuthAppsClientApp{}, gorm.ErrRecordNotFound
	}

	row, err := oauthAppsGetByID(ctx, id)
	if err != nil {
		return OAuthAppsClientApp{}, err
	}
	if row == nil {
		return OAuthAppsClientApp{}, gorm.ErrRecordNotFound
	}
	return oauthAppsClientApp(*row), nil
}

// OAuthAppsRotateSecret mirrors rotateOAuthClientSecret; a missing row errors
// (Prisma P2025) and the route answers 404 OAUTH_APP_NOT_FOUND.
func OAuthAppsRotateSecret(ctx context.Context, id int32) (OAuthAppsSecretResult, error) {
	clientSecret, err := oauthAppsGenerateClientSecret()
	if err != nil {
		return OAuthAppsSecretResult{}, err
	}

	result := DB.WithContext(ctx).Exec(
		"UPDATE oauth_client_apps SET client_secret_hash = ?, updated_at = NOW() WHERE id = ?",
		OAuthAppsHashSecret(clientSecret), id,
	)
	if result.Error != nil {
		return OAuthAppsSecretResult{}, result.Error
	}
	if result.RowsAffected == 0 {
		return OAuthAppsSecretResult{}, gorm.ErrRecordNotFound
	}

	row, err := oauthAppsGetByID(ctx, id)
	if err != nil {
		return OAuthAppsSecretResult{}, err
	}
	if row == nil {
		return OAuthAppsSecretResult{}, gorm.ErrRecordNotFound
	}
	return OAuthAppsSecretResult{App: oauthAppsClientApp(*row), ClientSecret: clientSecret}, nil
}

// OAuthAppsDelete mirrors deleteOAuthClientApp (a missing row is an error).
func OAuthAppsDelete(ctx context.Context, id int32) error {
	result := DB.WithContext(ctx).Exec("DELETE FROM oauth_client_apps WHERE id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// OAuthAppsRevokeAuthorization mirrors revokeOAuthAuthorizationById: it revokes
// the grant and every still-live access/refresh token of that grant in one
// transaction, returning nil when the grant does not exist.
func OAuthAppsRevokeAuthorization(ctx context.Context, grantID int32) (*OAuthAppsAdminAuthorization, error) {
	tx, err := oauthAppsBeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, found, err := oauthAppsGetGrantTx(ctx, tx, grantID); err != nil || !found {
		return nil, err
	}

	revokedAt := time.Now().UTC()
	if _, err := tx.Exec(ctx, "UPDATE oauth_grants SET revoked_at = $1, updated_at = NOW() WHERE id = $2",
		revokedAt, grantID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		"UPDATE oauth_access_tokens SET revoked_at = $1, updated_at = NOW() WHERE grant_id = $2 AND revoked_at IS NULL",
		revokedAt, grantID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		"UPDATE oauth_refresh_tokens SET revoked_at = $1, updated_at = NOW() WHERE grant_id = $2 AND revoked_at IS NULL",
		revokedAt, grantID); err != nil {
		return nil, err
	}

	// Prisma's `update()` returns the row it just wrote (fresh revoked_at *and* the
	// @updatedAt timestamp), so re-read inside the transaction instead of mutating
	// the pre-update copy — otherwise the response carries a stale updatedAt.
	updated, found, err := oauthAppsGetGrantTx(ctx, tx, grantID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	authorization := oauthAppsAdminAuthorization(updated, OAuthAppsTokenStats{
		ActiveAccessTokens:  0,
		ActiveRefreshTokens: 0,
	})
	return &authorization, nil
}

// oauthAppsBeginTx starts the transaction the revoke path needs.
func oauthAppsBeginTx(ctx context.Context) (pgx.Tx, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	return pool.Begin(ctx)
}

// oauthAppsGetGrantTx reads one grant with its app and user projections inside a
// transaction.
func oauthAppsGetGrantTx(ctx context.Context, tx pgx.Tx, grantID int32) (oauthAppsGrantRow, bool, error) {
	row, err := scanOAuthAppsGrantRow(func(dest ...any) error {
		return tx.QueryRow(ctx,
			"SELECT "+oauthAppsGrantSelectColumns+" FROM oauth_grants g"+
				" JOIN oauth_client_apps a ON a.id = g.app_id"+
				" JOIN users u ON u.id = g.user_id"+
				" WHERE g.id = $1", grantID,
		).Scan(dest...)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return oauthAppsGrantRow{}, false, nil
	}
	if err != nil {
		return oauthAppsGrantRow{}, false, err
	}
	return row, true, nil
}

// OAuthAppsVerifyClientSecret exposes the digest comparison for callers that need
// to validate a secret without a second hash (used by tests and future token
// exchange routes).
func OAuthAppsVerifyClientSecret(storedHash, presented string) bool {
	return oauthAppsSafeTimingEqual(storedHash, OAuthAppsHashSecret(presented))
}
