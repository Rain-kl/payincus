// Port of server/src/routes/oauth.ts (748 lines, 10 routes).
//
// app.ts registers this module with `{ prefix: '/api/oauth' }`, so every path below
// is relative to that prefix. This is the "PayIncus is an OAuth *client*" half of
// the OAuth surface (GitHub/Google login + account binding). The "PayIncus is an
// OAuth *provider*" half is routes/oauth_provider.go (prefix /api/oauth-provider)
// and shares no state with this file.
//
// Auth surfaces (WAVE_D_SPEC §0.2): the admin routes use authenticateAdmin, the
// three account routes use authenticate, and /providers, /authorize/:provider,
// /callback/:provider and /exchange-code are public. No public-API (pat_/poa_) or
// agent-HMAC credential can reach any route here.
//
// Security controls are delegated, never re-implemented:
//   - OAuth state nonce     → lib.GenerateOAuthState / VerifyAndConsumeOAuthState
//     (HMAC-signed, single use, 10 min TTL, process-local replay set);
//   - one-time login code   → lib.GenerateOAuthLoginCode /
//     lib.VerifyAndConsumeOAuthLoginCode (single use, 60 s TTL, mutex-protected);
//   - open-redirect defence → lib.GetSafeRedirectURL (port of redirect-validator.ts,
//     applied to both the incoming `redirect` and the state-carried one);
//   - bind ticket           → lib.ConsumeOAuthBindTicket (single use, 60 s, bound to
//     user + token iat + refresh session);
//   - refresh cookie        → routes.authSetRefreshCookie → lib/cookie_config.go is
//     the only source of httpOnly/secure/sameSite/path/domain/maxAge;
//   - log redaction         → lib.SanitizeObject / lib.SanitizeTokensInString.
//
// Process-local state (single-instance invariant, AGENTS.md): the OAuth state replay
// set, the one-time login-code set and the bind-ticket store. A second backend
// replica would accept replayed states — do not scale out without an owner-approved
// architecture change.
//
// Deliberate quirks reproduced from the TypeScript:
//   - every user-visible failure in /callback is a 302 to the frontend with an
//     `error=` code, never a JSON error (the front-end owns the message);
//   - /exchange-code answers 401 INVALID_LOGIN_CODE for an unknown code and 403
//     ACCOUNT_BANNED only for status 'banned' (not 'inactive');
//   - an out-of-enum :provider reaches Prisma's client-side enum validation and
//     surfaces as 500 on DELETE /configs/:provider and DELETE /bindings/:provider,
//     while GET /authorize rejects it with 400 INVALID_PROVIDER first.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// ==================== provider catalog ====================

// oauthClientProvider mirrors one entry of OAUTH_PROVIDERS.
type oauthClientProvider struct {
	AuthURL  string
	TokenURL string
	UserURL  string
	Scope    string
}

// oauthClientProviders mirrors OAUTH_PROVIDERS. It is a package variable (not a
// const map) so the callback tests can point the token/userinfo URLs at a local
// httptest server; production never writes it.
var oauthClientProviders = map[string]oauthClientProvider{
	"github": {
		AuthURL:  "https://github.com/login/oauth/authorize",
		TokenURL: "https://github.com/login/oauth/access_token",
		UserURL:  "https://api.github.com/user",
		Scope:    "read:user user:email",
	},
	"google": {
		AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
		UserURL:  "https://www.googleapis.com/oauth2/v2/userinfo",
		Scope:    "openid email profile",
	},
}

// oauthClientHTTPClient mirrors `fetch(..., { redirect: 'manual' })`: a 3xx is
// returned to the caller instead of being followed.
var oauthClientHTTPClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

const (
	// oauthSecretUnchangedPlaceholder mirrors OAUTH_SECRET_UNCHANGED_PLACEHOLDER.
	oauthSecretUnchangedPlaceholder = "UNCHANGED"
	// oauthProviderFetchTimeout mirrors OAUTH_PROVIDER_FETCH_TIMEOUT_MS.
	oauthProviderFetchTimeout = 15 * time.Second
	// oauthProviderMaxResponseBytes mirrors OAUTH_PROVIDER_MAX_RESPONSE_BYTES.
	oauthProviderMaxResponseBytes = 128 * 1024
	// oauthDefaultUserAgent mirrors `request.headers['user-agent'] || 'OAuth Login'`.
	oauthDefaultUserAgent = "OAuth Login"
)

// oauthIsKnownProvider reports whether the value is one of the enum members
// (github/google). It is the Go equivalent of Prisma's client-side enum check.
func oauthIsKnownProvider(provider string) bool {
	_, ok := oauthClientProviders[provider]
	return ok
}

// oauthRespondInternal mirrors the error handler's fallback for a non-validation
// error without a statusCode: 500 (production masks the message, and the other Go
// modules answer the masked shape; the conformance harness compares 500 by status).
func oauthRespondInternal(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// oauthISOTimestamp mirrors Date#toISOString (UTC, millisecond precision).
func oauthISOTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// ==================== redirect helpers ====================

// oauthRedirect mirrors `reply.redirect(url)`: Fastify sets `location` and sends an
// empty 302 body, so Gin must not use http.Redirect (which writes an HTML body).
func oauthRedirect(c *gin.Context, location string) {
	c.Header("Location", location)
	c.Status(http.StatusFound)
}

// oauthEncodeURIComponent mirrors encodeURIComponent (reuses the implementation the
// OAuth-provider routes already pinned against Node).
func oauthEncodeURIComponent(value string) string {
	return oauthProviderEncodeURIComponent(value)
}

// oauthFormEncode mirrors URLSearchParams serialization for one value.
func oauthFormEncode(value string) string {
	const uppercase = "0123456789ABCDEF"
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		current := value[index]
		switch {
		case current == ' ':
			builder.WriteByte('+')
		case current >= 'a' && current <= 'z',
			current >= 'A' && current <= 'Z',
			current >= '0' && current <= '9',
			current == '*', current == '-', current == '.', current == '_':
			builder.WriteByte(current)
		default:
			builder.WriteByte('%')
			builder.WriteByte(uppercase[current>>4])
			builder.WriteByte(uppercase[current&0x0F])
		}
	}
	return builder.String()
}

// oauthFormEncodePairs mirrors `new URLSearchParams(obj).toString()`: insertion order,
// every pair encoded, joined with '&'.
func oauthFormEncodePairs(params [][2]string) string {
	parts := make([]string, 0, len(params))
	for _, param := range params {
		parts = append(parts, oauthFormEncode(param[0])+"="+oauthFormEncode(param[1]))
	}
	return strings.Join(parts, "&")
}

// oauthRedirectWithQuery mirrors redirectWithQuery(baseUrl, params).
func oauthRedirectWithQuery(baseURL string, params [][2]string) string {
	separator := "?"
	if strings.Contains(baseURL, "?") {
		separator = "&"
	}
	return baseURL + separator + oauthFormEncodePairs(params)
}

// oauthIsAdminRedirectPath mirrors isAdminRedirectPath.
func oauthIsAdminRedirectPath(redirectURL string) bool {
	return redirectURL == "/admin" || strings.HasPrefix(redirectURL, "/admin/")
}

// oauthLoginPathForRedirect mirrors getLoginPathForRedirect.
func oauthLoginPathForRedirect(redirectURL string) string {
	if oauthIsAdminRedirectPath(redirectURL) {
		return "/admin/login"
	}
	return "/login"
}

// oauthDefaultRedirectForMode mirrors the `validMode === 'bind' ? '/profile' : '/'`
// default used by both /authorize and /callback.
func oauthDefaultRedirectForMode(mode lib.OAuthStateMode) string {
	if mode == lib.OAuthStateModeBind {
		return "/profile"
	}
	return "/"
}

// oauthNormalizePublicBaseURL mirrors normalizePublicBaseUrl: trim, then strip every
// trailing slash.
func oauthNormalizePublicBaseURL(value string) string {
	return strings.TrimRight(lib.JSTrim(value), "/")
}

// oauthParseConfiguredPublicBaseURL mirrors parseConfiguredPublicBaseUrl: parse with
// the WHATWG rules, accept only http/https, return `<protocol>//<host>` (default
// port dropped, host lower-cased).
func oauthParseConfiguredPublicBaseURL(configured string) (string, bool) {
	if configured == "" {
		return "", false
	}
	normalized := oauthNormalizePublicBaseURL(configured)
	parsed, err := url.Parse(normalized)
	if err != nil {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	host := strings.ToLower(parsed.Host)
	if host == "" {
		return "", false
	}
	// `new URL('https://example.com:443/').host` drops the default port.
	switch {
	case scheme == "http" && strings.HasSuffix(host, ":80"):
		host = strings.TrimSuffix(host, ":80")
	case scheme == "https" && strings.HasSuffix(host, ":443"):
		host = strings.TrimSuffix(host, ":443")
	}
	return scheme + "://" + host, true
}

// oauthConfiguredCustomerBaseURL mirrors getConfiguredCustomerBaseUrl:
// SITE_URL?.trim() || FRONTEND_URL?.split(',')[0]?.trim().
func oauthConfiguredCustomerBaseURL() (string, bool) {
	siteURL := lib.JSTrim(os.Getenv("SITE_URL"))
	if siteURL != "" {
		return oauthParseConfiguredPublicBaseURL(siteURL)
	}
	firstFrontend := strings.Split(os.Getenv("FRONTEND_URL"), ",")[0]
	return oauthParseConfiguredPublicBaseURL(lib.JSTrim(firstFrontend))
}

// oauthConfiguredAdminBaseURL mirrors getConfiguredAdminBaseUrl.
func oauthConfiguredAdminBaseURL() (string, bool) {
	return oauthParseConfiguredPublicBaseURL(lib.JSTrim(os.Getenv("ADMIN_FRONTEND_URL")))
}

// oauthLastHeaderEntry mirrors getLastEntryInMultiHeaderValue.
func oauthLastHeaderEntry(headerValue string) string {
	if index := strings.LastIndexByte(headerValue, ','); index >= 0 {
		return lib.JSTrim(headerValue[index+1:])
	}
	return lib.JSTrim(headerValue)
}

// oauthRequestProtocol mirrors the fastify `request.protocol` getter.
func oauthRequestProtocol(c *gin.Context) string {
	if lib.GetTrustProxyEnabled() {
		if forwarded := c.GetHeader("X-Forwarded-Proto"); forwarded != "" {
			return oauthLastHeaderEntry(forwarded)
		}
	}
	if c.Request.TLS != nil {
		return "https"
	}
	return "http"
}

// oauthRequestHostname mirrors the fastify `request.hostname` getter (it strips the
// port but keeps IPv6 brackets).
func oauthRequestHostname(c *gin.Context) string {
	host := c.Request.Host
	if lib.GetTrustProxyEnabled() {
		if forwarded := c.GetHeader("X-Forwarded-Host"); forwarded != "" {
			host = oauthLastHeaderEntry(forwarded)
		}
	}
	if strings.HasPrefix(host, "[") {
		if closeIndex := strings.IndexByte(host, ']'); closeIndex >= 0 {
			return host[:closeIndex+1]
		}
		return host
	}
	if colonIndex := strings.IndexByte(host, ':'); colonIndex >= 0 {
		return host[:colonIndex]
	}
	return host
}

// oauthCallbackBaseURL mirrors getOAuthCallbackBaseUrl. The error is the production
// hard failure ("<env> must be configured before OAuth can be used in production"),
// which the route turns into OAUTH_CALLBACK_URL_NOT_CONFIGURED.
func oauthCallbackBaseURL(c *gin.Context, redirectURL string) (string, error) {
	isAdminRedirect := oauthIsAdminRedirectPath(redirectURL)
	configured, ok := oauthConfiguredCustomerBaseURL()
	if isAdminRedirect {
		configured, ok = oauthConfiguredAdminBaseURL()
	}
	if ok {
		return configured, nil
	}
	if lib.IsProductionEnv() {
		requiredEnv := "SITE_URL or FRONTEND_URL"
		if isAdminRedirect {
			requiredEnv = "ADMIN_FRONTEND_URL"
		}
		return "", fmt.Errorf("%s must be configured before OAuth can be used in production", requiredEnv)
	}
	return oauthRequestProtocol(c) + "://" + oauthRequestHostname(c), nil
}

// oauthBuildCallbackURL mirrors buildOAuthCallbackUrl.
func oauthBuildCallbackURL(c *gin.Context, provider, redirectURL string) (string, error) {
	base, err := oauthCallbackBaseURL(c, redirectURL)
	if err != nil {
		return "", err
	}
	return base + "/api/oauth/callback/" + provider, nil
}

// ==================== query helpers ====================

// oauthQueryValue mirrors `request.query[name]` as the TypeScript uses it. A
// repeated key is an array in Fastify; joining with ',' reproduces the JS
// Array#toString coercion the comparisons below rely on (e.g. `mode === 'bind'`
// is false and a joined bindTicket can never hit the ticket store).
func oauthQueryValue(c *gin.Context, name string) (string, bool) {
	values, present := c.Request.URL.Query()[name]
	if !present || len(values) == 0 {
		return "", false
	}
	return strings.Join(values, ","), true
}

// ==================== outbound provider calls ====================

// oauthProviderJSONObject decodes a provider response body into an object, mirroring
// readOAuthJsonResponse<T>. A non-object (null, array, scalar) is invalid: the
// TypeScript would throw a TypeError on property access.
func oauthProviderJSONObject(response *http.Response, label string) (map[string]any, error) {
	text, err := lib.ReadLimitedTextResponse(response, label, oauthProviderMaxResponseBytes)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return nil, errors.New(label + " returned invalid JSON")
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, errors.New(label + " returned invalid JSON")
	}
	return object, nil
}

// oauthProviderRequest performs one outbound call with the fetch options the
// TypeScript uses: a 15 s abort signal, redirect:'manual' (client-level
// CheckRedirect) and JSON-only body handling.
func oauthProviderRequest(ctx context.Context, method, target string, headers map[string]string, body io.Reader) (*http.Response, error) {
	requestCtx, cancel := context.WithTimeout(ctx, oauthProviderFetchTimeout)
	request, err := http.NewRequestWithContext(requestCtx, method, target, body)
	if err != nil {
		cancel()
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := oauthClientHTTPClient.Do(request)
	if err != nil {
		cancel()
		return nil, err
	}
	response.Body = &oauthCancelOnCloseBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

// oauthCancelOnCloseBody releases the per-request timeout when the body is closed.
type oauthCancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (body *oauthCancelOnCloseBody) Close() error {
	err := body.ReadCloser.Close()
	body.cancel()
	return err
}

// oauthExchangeProviderCode mirrors step 1 of the callback: POST the authorization
// code to the provider's token URL. Returns the parsed JSON object and the status.
func oauthExchangeProviderCode(
	ctx context.Context,
	providerName string,
	provider oauthClientProvider,
	clientID, clientSecret, code, redirectURI string,
) (map[string]any, int, error) {
	form := oauthFormEncodePairs([][2]string{
		{"client_id", clientID},
		{"client_secret", clientSecret},
		{"code", code},
		{"redirect_uri", redirectURI},
		{"grant_type", "authorization_code"},
	})
	response, err := oauthProviderRequest(ctx, http.MethodPost, provider.TokenURL, map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Accept":       "application/json",
	}, strings.NewReader(form))
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	payload, err := oauthProviderJSONObject(response, providerName+" OAuth token response")
	if err != nil {
		return nil, response.StatusCode, err
	}
	return payload, response.StatusCode, nil
}

// oauthFetchProviderUser mirrors step 2 of the callback: GET the provider userinfo
// endpoint with the bearer token.
func oauthFetchProviderUser(
	ctx context.Context,
	providerName string,
	provider oauthClientProvider,
	accessToken string,
) (map[string]any, int, error) {
	response, err := oauthProviderRequest(ctx, http.MethodGet, provider.UserURL, map[string]string{
		"Authorization": "Bearer " + accessToken,
		"Accept":        "application/json",
	}, nil)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	payload, err := oauthProviderJSONObject(response, providerName+" OAuth userinfo response")
	if err != nil {
		return nil, response.StatusCode, err
	}
	return payload, response.StatusCode, nil
}

// ==================== provider user parsing ====================

// oauthProviderUser mirrors parseOAuthUser's return value.
type oauthProviderUser struct {
	ID       string
	Username *string
	Email    *string
	Avatar   *string
}

// oauthJSString mirrors String(value) for the JSON scalar types a provider can send.
func oauthJSString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return oauthJSNumberString(typed)
	case map[string]any:
		return "[object Object]"
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if item == nil {
				continue // Array#join renders null/undefined as an empty string.
			}
			parts = append(parts, oauthJSString(item))
		}
		return strings.Join(parts, ",")
	}
	return ""
}

// oauthJSNumberString mirrors Number#toString for the integral ids providers send.
func oauthJSNumberString(value float64) string {
	if value == 0 {
		return "0"
	}
	if value == float64(int64(value)) && value < 1e21 && value > -1e21 {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// oauthJSStringOrEmpty mirrors `String(data.id || ”)`: a falsy value becomes ”.
func oauthJSStringOrEmpty(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		if !typed {
			return ""
		}
		return "true"
	case float64:
		if typed == 0 {
			return ""
		}
		return oauthJSNumberString(typed)
	}
	return oauthJSString(value)
}

// oauthNonNullString mirrors `(data.login as string) || null`: only a non-empty
// string survives (a numeric value would be rejected by Prisma's String column in
// Node, so it is not invented here either).
func oauthNonNullString(value any) *string {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil
	}
	return &text
}

// oauthParseProviderUser mirrors parseOAuthUser.
func oauthParseProviderUser(provider string, data map[string]any) oauthProviderUser {
	switch provider {
	case "github":
		return oauthProviderUser{
			ID:       oauthJSStringOrEmpty(data["id"]),
			Username: oauthNonNullString(data["login"]),
			Email:    oauthNonNullString(data["email"]),
			Avatar:   oauthNonNullString(data["avatar_url"]),
		}
	case "google":
		username := oauthNonNullString(data["name"])
		if username == nil {
			// `email.split('@')[0]` (the empty local part stays an empty string).
			if email, ok := data["email"].(string); ok {
				local := strings.Split(email, "@")[0]
				username = &local
			}
		}
		return oauthProviderUser{
			ID:       oauthJSStringOrEmpty(data["id"]),
			Username: username,
			Email:    oauthNonNullString(data["email"]),
			Avatar:   oauthNonNullString(data["picture"]),
		}
	}
	return oauthProviderUser{ID: oauthJSStringOrEmpty(data["id"])}
}

// ==================== registration ====================

// RegisterOAuth mounts /api/oauth (app.ts: `fastify.register(oauthRoutes, { prefix: '/api/oauth' })`).
func RegisterOAuth(r *gin.Engine) {
	group := r.Group("/api/oauth")

	group.GET("/configs", middleware.AuthenticateAdmin(), oauthConfigsList)
	group.PUT("/configs/:provider", middleware.AuthenticateAdmin(), oauthConfigUpdate)
	group.DELETE("/configs/:provider", middleware.AuthenticateAdmin(), oauthConfigDelete)

	// Public: login-page provider list.
	group.GET("/providers", oauthProvidersList)

	group.POST("/bind-ticket", middleware.Authenticate(), oauthBindTicket)
	group.GET("/authorize/:provider", oauthAuthorize)
	group.GET("/callback/:provider", oauthCallback)
	group.GET("/bindings", middleware.Authenticate(), oauthBindingsList)
	group.DELETE("/bindings/:provider", middleware.Authenticate(), oauthBindingDelete)

	// Public + schema-validated body: mount the Fastify parser rules explicitly.
	group.POST("/exchange-code", middleware.RequestBody(), oauthExchangeCode)
}

func init() {
	Register(RegisterOAuth)
	// The auth module left this seam for the action-ticket port (auth.ts calls
	// revokeActionTicketsForSession when a refresh session is revoked). Wire the
	// real process-local implementation now that it exists.
	AuthRevokeActionTicketsForSession = lib.RevokeActionTicketsForSession
}

// ==================== GET /configs ====================

// oauthConfigsList mirrors GET /configs (admin).
func oauthConfigsList(c *gin.Context) {
	configs, err := db.OAuthLoginConfigList(c.Request.Context())
	if err != nil {
		oauthRespondInternal(c)
		return
	}

	items := make([]gin.H, 0, len(configs))
	for _, config := range configs {
		var masked *string
		if config.ClientSecret != "" {
			secret := lib.DecryptSensitiveData(config.ClientSecret)
			value := "****" + oauthLastRunes(secret, 4)
			masked = &value
		}
		items = append(items, gin.H{
			"provider":           config.Provider,
			"clientId":           config.ClientID,
			"clientSecretMasked": masked,
			"enabled":            config.Enabled,
			"updatedAt":          oauthISOTimestamp(config.UpdatedAt),
		})
	}
	c.JSON(http.StatusOK, gin.H{"configs": items})
}

// oauthLastRunes mirrors String#slice(-n): the last n UTF-16 units (ASCII secrets in
// practice, runes are the same sequence).
func oauthLastRunes(value string, count int) string {
	runes := []rune(value)
	if len(runes) <= count {
		return value
	}
	return string(runes[len(runes)-count:])
}

// ==================== PUT /configs/:provider ====================

// oauthConfigFields mirrors the body schema's properties, in schema order.
var oauthConfigFields = []ajvFieldSpec{
	{Name: "clientId", Kind: ajvKindString, MinLength: 1, MaxLength: -1},
	{Name: "clientSecret", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
	{Name: "enabled", Kind: ajvKindBoolean, MinLength: -1, MaxLength: -1},
}

// oauthProviderParamsFields mirrors the params schema (enum github|google).
var oauthProviderParamsFields = []ajvFieldSpec{
	{Name: "provider", Kind: ajvKindString, MinLength: -1, MaxLength: -1,
		Enum: []string{"github", "google"}},
}

// oauthConfigUpdate mirrors PUT /configs/:provider (admin).
func oauthConfigUpdate(c *gin.Context) {
	ctx := c.Request.Context()
	provider := c.Param("provider")

	// Fastify validates params before body (lib/validation.js order).
	if _, message := validateAJVObject(
		map[string]any{"provider": provider}, nil, oauthProviderParamsFields,
	); message != "" {
		respondAJVValidation(c, message)
		return
	}

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	validated, message := validateAJVObject(
		decoded.Value, []string{"clientId"}, oauthConfigFields,
	)
	if message != "" {
		respondAJVValidation(c, message)
		return
	}

	clientID, _ := validated.stringValue("clientId")
	clientSecret, hasClientSecret := validated.stringValue("clientSecret")
	enabled := false
	if value, present := validated.Values["enabled"]; present {
		enabled, _ = value.(bool)
	}

	existing, err := db.OAuthLoginConfigGet(ctx, provider)
	if err != nil {
		oauthRespondInternal(c)
		return
	}

	// `clientSecret === undefined || '' || 'UNCHANGED'` keeps the stored secret.
	shouldKeepExistingSecret := existing != nil && (!hasClientSecret ||
		clientSecret == "" || clientSecret == oauthSecretUnchangedPlaceholder)

	if existing == nil && (!hasClientSecret || clientSecret == "") {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Client Secret is required",
			"code":  "OAUTH_CLIENT_SECRET_REQUIRED",
		})
		return
	}

	secretToStore := clientSecret
	if shouldKeepExistingSecret {
		// The TypeScript passes the *decrypted* existing secret back through
		// upsertOAuthConfig, which re-encrypts it (fresh IV).
		secretToStore = lib.DecryptSensitiveData(existing.ClientSecret)
	}

	ciphertext := secretToStore
	if !lib.IsEncrypted(ciphertext) {
		encrypted, err := lib.EncryptSensitiveData(ciphertext)
		if err != nil {
			oauthRespondInternal(c)
			return
		}
		ciphertext = encrypted
	}

	if _, err := db.OAuthLoginConfigUpsert(ctx, provider, clientID, ciphertext, enabled); err != nil {
		oauthRespondInternal(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": provider + " OAuth 配置已更新"})
}

// ==================== DELETE /configs/:provider ====================

// oauthConfigDelete mirrors DELETE /configs/:provider (admin).
func oauthConfigDelete(c *gin.Context) {
	provider := c.Param("provider")

	// No params schema in the TypeScript, so an out-of-enum provider reaches
	// Prisma's enum validation and fails the request (500); a valid provider whose
	// row is missing is P2025 (also 500).
	if !oauthIsKnownProvider(provider) {
		oauthRespondInternal(c)
		return
	}
	if err := db.OAuthLoginConfigDelete(c.Request.Context(), provider); err != nil {
		oauthRespondInternal(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": provider + " OAuth 配置已删除"})
}

// ==================== GET /providers ====================

// oauthProvidersList mirrors GET /providers (public).
func oauthProvidersList(c *gin.Context) {
	providers, err := db.OAuthLoginEnabledProviders(c.Request.Context())
	if err != nil {
		oauthRespondInternal(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"providers": providers})
}

// ==================== POST /bind-ticket ====================

// oauthBindTicket mirrors POST /bind-ticket.
func oauthBindTicket(c *gin.Context) {
	claims := middleware.CurrentUser(c)
	if claims == nil || claims.IssuedAt == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized", "code": "UNAUTHORIZED"})
		return
	}

	var sessionID *string
	if claims.SID != "" {
		sessionID = &claims.SID
	}
	c.JSON(http.StatusOK, gin.H{
		"ticket":    lib.GenerateOAuthBindTicket(claims.ID, claims.IssuedAt, sessionID),
		"expiresIn": 60,
	})
}

// ==================== GET /authorize/:provider ====================

// oauthAuthorize mirrors GET /authorize/:provider (public).
func oauthAuthorize(c *gin.Context) {
	ctx := c.Request.Context()
	provider := c.Param("provider")
	mode, _ := oauthQueryValue(c, "mode")
	redirect, _ := oauthQueryValue(c, "redirect")
	bindTicket, hasBindTicket := oauthQueryValue(c, "bindTicket")

	if !oauthIsKnownProvider(provider) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Unsupported OAuth provider",
			"code":  "INVALID_PROVIDER",
		})
		return
	}

	config, err := db.OAuthLoginConfigGet(ctx, provider)
	if err != nil {
		oauthRespondInternal(c)
		return
	}
	if config == nil || !config.Enabled {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrOauthNotEnabled, provider)
		return
	}

	providerConfig := oauthClientProviders[provider]
	validMode := lib.OAuthStateModeLogin
	if mode == "bind" {
		validMode = lib.OAuthStateModeBind
	}
	safeRedirectURL := lib.GetSafeRedirectURL(redirect, oauthDefaultRedirectForMode(validMode))

	// Bind mode: the caller proves its session with a single-use bind ticket, so the
	// login JWT never travels in the URL.
	var bindSession *lib.OAuthBindSession
	if validMode == lib.OAuthStateModeBind {
		if !hasBindTicket {
			oauthRedirect(c, oauthRedirectWithQuery(safeRedirectURL,
				[][2]string{{"error", "not_logged_in"}}))
			return
		}
		ticketData := lib.ConsumeOAuthBindTicket(bindTicket)
		if !ticketData.Valid || ticketData.UserID == 0 || ticketData.IssuedAt == 0 {
			oauthRedirect(c, oauthRedirectWithQuery(safeRedirectURL,
				[][2]string{{"error", "invalid_session"}}))
			return
		}
		sessionID := ""
		if ticketData.SessionID != nil {
			sessionID = *ticketData.SessionID
		}
		if lib.IsAccessTokenInvalidated(ctx, ticketData.UserID, ticketData.IssuedAt, sessionID) {
			oauthRedirect(c, oauthRedirectWithQuery(safeRedirectURL,
				[][2]string{{"error", "invalid_session"}}))
			return
		}
		bindUser, err := db.FindUserById(ctx, ticketData.UserID)
		if err != nil {
			oauthRespondInternal(c)
			return
		}
		if bindUser == nil || string(bindUser.Status) != "active" {
			oauthRedirect(c, oauthRedirectWithQuery(safeRedirectURL,
				[][2]string{{"error", "invalid_session"}}))
			return
		}
		bindSession = &lib.OAuthBindSession{
			UserID:    bindUser.ID,
			IssuedAt:  ticketData.IssuedAt,
			SessionID: ticketData.SessionID,
		}
	}

	state, err := lib.GenerateOAuthState(validMode, safeRedirectURL, bindSession)
	if err != nil {
		oauthRespondInternal(c)
		return
	}

	callbackURL, err := oauthBuildCallbackURL(c, provider, safeRedirectURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[OAuth] OAuth public callback URL is not configured (provider=%s): %s\n",
			provider, lib.SanitizeTokensInString(err.Error()))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "OAuth callback URL is not configured",
			"code":  "OAUTH_CALLBACK_URL_NOT_CONFIGURED",
		})
		return
	}

	params := [][2]string{
		{"client_id", config.ClientID},
		{"redirect_uri", callbackURL},
		{"scope", providerConfig.Scope},
		{"state", state},
		{"response_type", "code"},
	}
	if provider == "google" {
		params = append(params, [2]string{"access_type", "offline"}, [2]string{"prompt", "consent"})
	}

	oauthRedirect(c, providerConfig.AuthURL+"?"+oauthFormEncodePairs(params))
}

// ==================== GET /callback/:provider ====================

// oauthCallback mirrors GET /callback/:provider (public).
func oauthCallback(c *gin.Context) {
	ctx := c.Request.Context()
	provider := c.Param("provider")
	code, hasCode := oauthQueryValue(c, "code")
	state, hasState := oauthQueryValue(c, "state")
	errorParam, hasError := oauthQueryValue(c, "error")
	ip := middleware.ClientIP(c)

	// Provider-side error (user denied consent, ...).
	if hasError && errorParam != "" {
		lib.LogSecurityEvent(ctx, lib.SecurityEventLoginFailed, nil, lib.SecurityEventDetails{
			IP:     ip,
			Action: "oauth_callback",
			Reason: "OAuth error: " + errorParam,
		})
		oauthRedirect(c, "/login?error="+oauthEncodeURIComponent(errorParam))
		return
	}

	if !hasState || state == "" {
		lib.LogSecurityEvent(ctx, lib.SecurityEventSuspiciousActivity, nil, lib.SecurityEventDetails{
			IP:     ip,
			Action: "oauth_callback",
			Reason: "Missing state parameter",
		})
		oauthRedirect(c, "/login?error=invalid_state")
		return
	}

	// One-time state: this consumes it, so a replayed callback fails below.
	stateData := lib.VerifyAndConsumeOAuthState(state)
	if stateData == nil {
		lib.LogSecurityEvent(ctx, lib.SecurityEventSuspiciousActivity, nil, lib.SecurityEventDetails{
			IP:     ip,
			Action: "oauth_callback",
			Reason: "Invalid or expired state token",
		})
		oauthRedirect(c, "/login?error=state_expired")
		return
	}

	// Re-validate the redirect carried by the state (defence in depth: the state is
	// signed, but the URL is also checked at authorize time).
	redirectURL := lib.GetSafeRedirectURL(stateData.Redirect, oauthDefaultRedirectForMode(stateData.Mode))
	loginRedirectBase := oauthLoginPathForRedirect(redirectURL)
	errorRedirectBase := loginRedirectBase
	if stateData.Mode == lib.OAuthStateModeBind {
		errorRedirectBase = redirectURL
	}

	if !hasCode || code == "" {
		oauthRedirect(c, oauthRedirectWithQuery(errorRedirectBase,
			[][2]string{{"error", "missing_code"}}))
		return
	}

	// Everything below is the TypeScript's try/catch: any failure becomes a 302 with
	// error=oauth_error instead of a JSON 5xx.
	location, err := oauthCompleteCallback(c, oauthCallbackInput{
		Provider:          provider,
		Code:              code,
		StateData:         stateData,
		RedirectURL:       redirectURL,
		LoginRedirectBase: loginRedirectBase,
		ErrorRedirectBase: errorRedirectBase,
		IP:                ip,
		UserAgent:         oauthHeaderOrDefault(c.GetHeader("User-Agent"), oauthDefaultUserAgent),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[OAuth] OAuth callback error (provider=%s): %s\n",
			provider, lib.SanitizeTokensInString(err.Error()))
		oauthRedirect(c, oauthRedirectWithQuery(errorRedirectBase,
			[][2]string{{"error", "oauth_error"}}))
		return
	}
	oauthRedirect(c, location)
}

// oauthCallbackInput bundles the callback state handed to oauthCompleteCallback.
type oauthCallbackInput struct {
	Provider          string
	Code              string
	StateData         *lib.OAuthStateData
	RedirectURL       string
	LoginRedirectBase string
	ErrorRedirectBase string
	IP                string
	UserAgent         string
}

// oauthCompleteCallback is the TypeScript try-block. It returns the redirect to
// emit, or an error that the caller turns into error=oauth_error.
func oauthCompleteCallback(c *gin.Context, input oauthCallbackInput) (string, error) {
	ctx := c.Request.Context()

	config, err := db.OAuthLoginConfigGet(ctx, input.Provider)
	if err != nil {
		return "", err
	}
	if config == nil || !config.Enabled {
		return oauthRedirectWithQuery(input.ErrorRedirectBase,
			[][2]string{{"error", "provider_disabled"}}), nil
	}
	providerConfig := oauthClientProviders[input.Provider]

	callbackURL, err := oauthBuildCallbackURL(c, input.Provider, input.RedirectURL)
	if err != nil {
		return "", err
	}

	clientSecret := lib.DecryptSensitiveData(config.ClientSecret)

	// 1. code → access_token
	tokenData, tokenStatus, err := oauthExchangeProviderCode(
		ctx, input.Provider, providerConfig, config.ClientID, clientSecret, input.Code, callbackURL,
	)
	if err != nil {
		return "", err
	}
	accessToken, hasAccessToken := tokenData["access_token"].(string)
	if tokenStatus < 200 || tokenStatus >= 300 || !hasAccessToken || accessToken == "" {
		// Sanitised log only: the payload may carry tokens.
		sanitized, _ := json.Marshal(lib.SanitizeObject(tokenData))
		fmt.Fprintf(os.Stderr, "[OAuth] OAuth token exchange failed (provider=%s, status=%d): %s\n",
			input.Provider, tokenStatus, sanitized)
		return oauthRedirectWithQuery(input.ErrorRedirectBase,
			[][2]string{{"error", "token_error"}}), nil
	}
	refreshTokenValue, hasRefreshToken := tokenData["refresh_token"].(string)

	// 2. access_token → userinfo
	userData, userStatus, err := oauthFetchProviderUser(ctx, input.Provider, providerConfig, accessToken)
	if err != nil {
		return "", err
	}
	if userStatus < 200 || userStatus >= 300 {
		raw, _ := json.Marshal(userData)
		truncated := string(raw)
		if len(truncated) > 2048 {
			truncated = truncated[:2048]
		}
		return "", fmt.Errorf("OAuth userinfo failed with status %d: %s",
			userStatus, lib.SanitizeTokensInString(truncated))
	}

	oauthUser := oauthParseProviderUser(input.Provider, userData)
	if oauthUser.ID == "" {
		return "", errors.New("OAuth userinfo response did not include a provider user id")
	}

	// 3a. bind mode
	if input.StateData.Mode == lib.OAuthStateModeBind {
		userID := int32(0)
		if input.StateData.UserID != nil {
			userID = *input.StateData.UserID
		}
		var userIssuedAt int64
		if input.StateData.UserIssuedAt != nil {
			userIssuedAt = *input.StateData.UserIssuedAt
		}
		if userID == 0 || userIssuedAt == 0 {
			return oauthRedirectWithQuery(input.ErrorRedirectBase,
				[][2]string{{"error", "not_logged_in"}}), nil
		}

		sessionID := ""
		if input.StateData.UserSessionID != nil {
			sessionID = *input.StateData.UserSessionID
		}
		if lib.IsAccessTokenInvalidated(ctx, userID, userIssuedAt, sessionID) {
			return oauthRedirectWithQuery(input.ErrorRedirectBase,
				[][2]string{{"error", "invalid_session"}}), nil
		}

		bindUser, err := db.FindUserById(ctx, userID)
		if err != nil {
			return "", err
		}
		if bindUser == nil || string(bindUser.Status) != "active" {
			return oauthRedirectWithQuery(input.ErrorRedirectBase,
				[][2]string{{"error", "invalid_session"}}), nil
		}

		// One provider account can only back one PayIncus user.
		existingBinding, err := db.OAuthLoginBindingFind(ctx, input.Provider, oauthUser.ID)
		if err != nil {
			return "", err
		}
		if existingBinding != nil && existingBinding.UserID != userID {
			return oauthRedirectWithQuery(input.ErrorRedirectBase,
				[][2]string{{"error", "already_bound_other"}}), nil
		}

		hasBound, err := db.HasOAuthBinding(ctx, userID, input.Provider)
		if err != nil {
			return "", err
		}
		if hasBound {
			// Re-binding the same provider refreshes the stored tokens.
			if err := db.OAuthLoginBindingUpdateToken(ctx, userID, input.Provider,
				encryptedOrOriginal(accessToken), encryptedRefreshTokenOrNil(refreshTokenValue, hasRefreshToken)); err != nil {
				return "", err
			}
		} else {
			if _, err := db.OAuthLoginBindingCreate(ctx, userID, db.OAuthLoginBindingInput{
				Provider:         input.Provider,
				ProviderUserID:   oauthUser.ID,
				ProviderUsername: oauthUser.Username,
				ProviderEmail:    oauthUser.Email,
				ProviderAvatar:   oauthUser.Avatar,
				AccessToken:      encryptedStringPtr(accessToken),
				RefreshToken:     encryptedRefreshTokenOrNil(refreshTokenValue, hasRefreshToken),
			}); err != nil {
				return "", err
			}
		}

		return oauthRedirectWithQuery(input.RedirectURL,
			[][2]string{{"success", "bound_" + input.Provider}}), nil
	}

	// 3b. login mode
	binding, err := db.OAuthLoginBindingFind(ctx, input.Provider, oauthUser.ID)
	if err != nil {
		return "", err
	}
	if binding == nil {
		return oauthRedirectWithQuery(input.LoginRedirectBase,
			[][2]string{{"error", "not_bound"}, {"provider", input.Provider}}), nil
	}

	user, err := db.FindUserById(ctx, binding.UserID)
	if err != nil {
		return "", err
	}
	if user == nil {
		return oauthRedirectWithQuery(input.LoginRedirectBase,
			[][2]string{{"error", "user_not_found"}}), nil
	}
	if string(user.Status) != "active" {
		return oauthRedirectWithQuery(input.LoginRedirectBase,
			[][2]string{{"error", "account_banned"}}), nil
	}

	if err := db.OAuthLoginBindingUpdateToken(ctx, user.ID, input.Provider,
		encryptedOrOriginal(accessToken), encryptedRefreshTokenOrNil(refreshTokenValue, hasRefreshToken)); err != nil {
		return "", err
	}

	// Detect the new location BEFORE the session is created (Node fixed a race here).
	var loginAlert *lib.NewLoginLocation
	if user.Email != nil && *user.Email != "" {
		detected := lib.DetectNewLoginLocation(ctx, user.ID, input.IP, input.UserAgent)
		loginAlert = &detected
	}

	lib.LogSecurityEvent(ctx, lib.SecurityEventLoginSuccess, oauthInt64Ptr(int64(user.ID)), lib.SecurityEventDetails{
		IP:       input.IP,
		Username: user.Username,
		Action:   "oauth_" + input.Provider,
	})

	if user.Email != nil && *user.Email != "" && loginAlert != nil &&
		(loginAlert.IsNewIP || loginAlert.IsNewDevice) {
		email := *user.Email
		alert := *loginAlert
		username := user.Username
		ip := input.IP
		userAgent := input.UserAgent
		go func() {
			mailCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			lib.SendLoginAlertEmail(mailCtx, email, lib.LoginAlertEmailData{
				Username:    username,
				IP:          ip,
				UserAgent:   userAgent,
				LoginTime:   time.Now(),
				IsNewIP:     alert.IsNewIP,
				IsNewDevice: alert.IsNewDevice,
			})
		}()
	}

	// The login code only carries the user identity; /exchange-code mints the
	// refresh token, so no token ever travels in a URL.
	loginCode, err := lib.GenerateOAuthLoginCode(lib.OAuthLoginCodeInput{
		UserID:   user.ID,
		Username: user.Username,
		Role:     string(user.Role),
	})
	if err != nil {
		return "", err
	}

	separator := "?"
	if strings.Contains(input.RedirectURL, "?") {
		separator = "&"
	}
	return input.RedirectURL + separator + "oauth_code=" + oauthEncodeURIComponent(loginCode), nil
}

// ==================== GET /bindings ====================

// oauthBindingsList mirrors GET /bindings.
func oauthBindingsList(c *gin.Context) {
	claims := middleware.CurrentUser(c)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	bindings, err := db.OAuthLoginBindingListByUser(c.Request.Context(), claims.ID)
	if err != nil {
		oauthRespondInternal(c)
		return
	}

	items := make([]gin.H, 0, len(bindings))
	for _, binding := range bindings {
		items = append(items, gin.H{
			"provider": binding.Provider,
			"username": binding.ProviderUsername,
			"email":    binding.ProviderEmail,
			"avatar":   binding.ProviderAvatar,
			"boundAt":  oauthISOTimestamp(binding.CreatedAt),
		})
	}
	c.JSON(http.StatusOK, gin.H{"bindings": items})
}

// ==================== DELETE /bindings/:provider ====================

// oauthBindingDelete mirrors DELETE /bindings/:provider.
func oauthBindingDelete(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)
	if claims == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	provider := c.Param("provider")

	// An out-of-enum provider fails Prisma's enum validation (500) before the
	// has-binding check; keep the same status instead of answering 400.
	if !oauthIsKnownProvider(provider) {
		oauthRespondInternal(c)
		return
	}

	hasBound, err := db.HasOAuthBinding(ctx, claims.ID, provider)
	if err != nil {
		oauthRespondInternal(c)
		return
	}
	if !hasBound {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrOauthNotBound, "")
		return
	}

	if err := db.DeleteOAuthBinding(ctx, claims.ID, provider); err != nil {
		oauthRespondInternal(c)
		return
	}

	// Unbinding is a credential-revocation event: kill every session of the user so
	// a stolen refresh token cannot keep minting access tokens.
	_, _ = lib.RevokeAllUserRefreshTokens(ctx, claims.ID)
	lib.InvalidateUserAccessTokens(ctx, claims.ID)
	UsersCloseUserSessions(claims.ID, provider+" OAuth unbound")

	userID := int64(claims.ID)
	db.CreateLog(ctx, &userID, db.LogModuleSecurity, "oauth.unbind",
		"Unbound "+provider+" OAuth account", db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"message": "已解除 " + provider + " 绑定"})
}

// ==================== POST /exchange-code ====================

// oauthExchangeCodeFields mirrors the body schema (required code, minLength 10).
var oauthExchangeCodeFields = []ajvFieldSpec{
	{Name: "code", Kind: ajvKindString, MinLength: 10, MaxLength: -1},
}

// oauthExchangeCode mirrors POST /exchange-code (public).
func oauthExchangeCode(c *gin.Context) {
	ctx := c.Request.Context()

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	validated, message := validateAJVObject(decoded.Value, []string{"code"}, oauthExchangeCodeFields)
	if message != "" {
		respondAJVValidation(c, message)
		return
	}
	code, _ := validated.stringValue("code")

	// Single-use login code: the nonce is consumed even if the rest fails, so a
	// replay can never mint a second session.
	loginData := lib.VerifyAndConsumeOAuthLoginCode(code)
	if loginData == nil {
		lib.LogSecurityEvent(ctx, lib.SecurityEventSuspiciousActivity, nil, lib.SecurityEventDetails{
			IP:     middleware.ClientIP(c),
			Action: "oauth_exchange_code",
			Reason: "Invalid or expired login code",
		})
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "Invalid or expired login code",
			"code":  "INVALID_LOGIN_CODE",
		})
		return
	}

	// Re-check the account: it may have been banned between callback and exchange.
	user, err := db.FindUserById(ctx, loginData.UserID)
	if err != nil {
		oauthRespondInternal(c)
		return
	}
	if user == nil {
		lib.LogSecurityEvent(ctx, lib.SecurityEventSuspiciousActivity, oauthInt64Ptr(int64(loginData.UserID)), lib.SecurityEventDetails{
			IP:     middleware.ClientIP(c),
			Action: "oauth_exchange_code",
			Reason: "User not found during code exchange",
		})
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not found", "code": "USER_NOT_FOUND"})
		return
	}

	if string(user.Status) == "banned" {
		lib.LogSecurityEvent(ctx, lib.SecurityEventLoginFailed, oauthInt64Ptr(int64(loginData.UserID)), lib.SecurityEventDetails{
			IP:       middleware.ClientIP(c),
			Username: loginData.Username,
			Action:   "oauth_exchange_code",
			Reason:   "Account banned during code exchange",
		})
		if user.BanReason != nil && *user.BanReason != "" {
			c.JSON(http.StatusForbidden, gin.H{"error": *user.BanReason, "code": "ACCOUNT_BANNED"})
			return
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "Account banned", "code": "ACCOUNT_BANNED"})
		return
	}

	// The refresh token is minted here, never carried through the URL.
	userAgent := oauthHeaderOrDefault(c.GetHeader("User-Agent"), oauthDefaultUserAgent)
	refreshToken, err := lib.GenerateRefreshToken(ctx, user.ID, user.Username, user.Role,
		middleware.ClientIP(c), userAgent)
	if err != nil {
		oauthRespondInternal(c)
		return
	}

	authSetRefreshCookie(c, refreshToken)

	sessionID := lib.GetRefreshTokenSessionID(refreshToken)
	accessToken, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID:       loginData.UserID,
		Username: loginData.Username,
		Role:     loginData.Role,
		SID:      sessionID,
	})
	if err != nil {
		oauthRespondInternal(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token": accessToken,
		"user": gin.H{
			"id":       loginData.UserID,
			"username": loginData.Username,
			"role":     loginData.Role,
		},
	})
}

// ==================== small helpers ====================

// oauthHeaderOrDefault mirrors `header || fallback`.
func oauthHeaderOrDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// oauthInt64Ptr returns a pointer to the value (log helpers take *int64).
func oauthInt64Ptr(value int64) *int64 { return &value }

// encryptedOrOriginal mirrors db/oauth.ts' encryptSensitiveData at the write
// boundary: an already-encrypted value is stored unchanged.
func encryptedOrOriginal(plaintext string) string {
	if lib.IsEncrypted(plaintext) {
		return plaintext
	}
	encrypted, err := lib.EncryptSensitiveData(plaintext)
	if err != nil {
		return plaintext
	}
	return encrypted
}

// encryptedStringPtr encrypts an optional token for storage.
func encryptedStringPtr(plaintext string) *string {
	if plaintext == "" {
		return nil
	}
	encrypted := encryptedOrOriginal(plaintext)
	return &encrypted
}

// encryptedRefreshTokenOrNil mirrors `tokenData.refresh_token || null` followed by
// the nullable-column write: absent/empty stays NULL.
func encryptedRefreshTokenOrNil(value string, present bool) *string {
	if !present || value == "" {
		return nil
	}
	return encryptedStringPtr(value)
}
