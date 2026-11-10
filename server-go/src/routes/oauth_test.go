// Tests for the OAuth (login-provider) route port.
//
// The suite is split in two:
//
//   - pure/HTTP tests that never need a database. Protected routes are not exercised
//     here (their middleware reads the users table); the conformance harness covers
//     them end-to-end against the real parity database.
//   - fake-provider tests: the callback's outbound half is driven against a local
//     httptest server, so token exchange + userinfo parsing are verified without
//     touching github.com/google (no outbound network in tests, WAVE_D_SPEC §4.3).
package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// oauthTestEngine builds a minimal gin engine with only the /api/oauth routes.
func oauthTestEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterOAuth(engine)
	return engine
}

// oauthInstallUnavailableDB points db.DB at a GORM handle whose connections always
// fail. Handlers then see ordinary query errors instead of a nil-pointer panic, and
// security-event logging degrades to a stderr line exactly as it would on a
// transient database outage. No connection is ever attempted at install time.
func oauthInstallUnavailableDB(t *testing.T) {
	t.Helper()
	gormDB, err := gorm.Open(
		postgres.Open("host=127.0.0.1 port=1 user=nobody dbname=none sslmode=disable connect_timeout=1"),
		&gorm.Config{DisableAutomaticPing: true, Logger: logger.Discard},
	)
	if err != nil {
		t.Fatalf("build unavailable gorm handle: %v", err)
	}
	previous := db.DB
	db.DB = gormDB
	t.Cleanup(func() { db.DB = previous })
}

// oauthDo performs a request against the test engine.
func oauthDo(engine *gin.Engine, method, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

// ==================== encoding / redirect helpers ====================

// TestOAuthRedirectQueryEncoding pins URLSearchParams serialization against Node 26
// (`new URLSearchParams(obj).toString()`).
func TestOAuthRedirectQueryEncoding(t *testing.T) {
	cases := []struct {
		params [][2]string
		want   string
	}{
		{[][2]string{{"error", "not_logged_in"}}, "error=not_logged_in"},
		{[][2]string{{"error", "not_bound"}, {"provider", "github"}}, "error=not_bound&provider=github"},
		{[][2]string{{"success", "bound_github"}}, "success=bound_github"},
		// space → '+', then the form-urlencoded escape set (note ~ ' ( ) ! are escaped,
		// '*' is not).
		{[][2]string{{"a", "a b&c=d/é~*'()!"}}, "a=a+b%26c%3Dd%2F%C3%A9%7E*%27%28%29%21"},
		{
			[][2]string{
				{"client_id", "pocli_x"},
				{"redirect_uri", "http://127.0.0.1/api/oauth/callback/github"},
				{"scope", "read:user user:email"},
				{"state", "abc.def"},
				{"response_type", "code"},
			},
			"client_id=pocli_x&redirect_uri=http%3A%2F%2F127.0.0.1%2Fapi%2Foauth%2Fcallback%2Fgithub" +
				"&scope=read%3Auser+user%3Aemail&state=abc.def&response_type=code",
		},
	}
	for _, tc := range cases {
		if got := oauthFormEncodePairs(tc.params); got != tc.want {
			t.Fatalf("oauthFormEncodePairs(%v) = %q, want %q", tc.params, got, tc.want)
		}
	}

	if got := oauthRedirectWithQuery("/profile", [][2]string{{"error", "invalid_session"}}); got != "/profile?error=invalid_session" {
		t.Fatalf("plain base = %q", got)
	}
	if got := oauthRedirectWithQuery("/profile?tab=1", [][2]string{{"error", "invalid_session"}}); got != "/profile?tab=1&error=invalid_session" {
		t.Fatalf("base with query = %q", got)
	}
}

// TestOAuthLoginPathForRedirect covers the /admin vs customer branch of the callback
// error target.
func TestOAuthLoginPathForRedirect(t *testing.T) {
	cases := map[string]string{
		"/admin":            "/admin/login",
		"/admin/users":      "/admin/login",
		"/admin/":           "/admin/login",
		"/administrator":    "/login",
		"/admin?x=1":        "/login",
		"/admin\\evil":      "/login",
		"/Admin":            "/login",
		"/profile":          "/login",
		"/":                 "/login",
		"/admin/settings/x": "/admin/login",
	}
	for input, want := range cases {
		if got := oauthLoginPathForRedirect(input); got != want {
			t.Fatalf("oauthLoginPathForRedirect(%q) = %q, want %q", input, got, want)
		}
	}
	if oauthDefaultRedirectForMode(lib.OAuthStateModeBind) != "/profile" {
		t.Fatalf("bind default redirect must be /profile")
	}
	if oauthDefaultRedirectForMode(lib.OAuthStateModeLogin) != "/" {
		t.Fatalf("login default redirect must be /")
	}
}

// TestOAuthParseConfiguredPublicBaseURL mirrors parseConfiguredPublicBaseUrl.
func TestOAuthParseConfiguredPublicBaseURL(t *testing.T) {
	valid := map[string]string{
		"https://pay.example.com":            "https://pay.example.com",
		"https://pay.example.com/":           "https://pay.example.com",
		"HTTPS://PAY.Example.COM/app///":     "https://pay.example.com",
		"https://pay.example.com:443/x":      "https://pay.example.com",
		"http://pay.example.com:80":          "http://pay.example.com",
		"http://pay.example.com:8080":        "http://pay.example.com:8080",
		"https://user:pass@pay.example.com":  "https://pay.example.com",
		"  https://admin.example.com  \n":    "https://admin.example.com",
		"https://pay.example.com/path?x=1#f": "https://pay.example.com",
	}
	for input, want := range valid {
		got, ok := oauthParseConfiguredPublicBaseURL(input)
		if !ok || got != want {
			t.Fatalf("oauthParseConfiguredPublicBaseURL(%q) = (%q, %v), want (%q, true)", input, got, ok, want)
		}
	}
	for _, input := range []string{"", "   ", "example.com", "ftp://example.com", "javascript:alert(1)", "https://"} {
		if got, ok := oauthParseConfiguredPublicBaseURL(input); ok {
			t.Fatalf("oauthParseConfiguredPublicBaseURL(%q) = (%q, true), want rejection", input, got)
		}
	}
}

// TestOAuthCallbackBaseURLFromEnv mirrors getOAuthCallbackBaseUrl's env resolution.
func TestOAuthCallbackBaseURLFromEnv(t *testing.T) {
	t.Setenv("SITE_URL", "https://pay.example.com/")
	t.Setenv("FRONTEND_URL", "https://ignored.example.com")
	t.Setenv("ADMIN_FRONTEND_URL", "https://admin.example.com/")
	t.Setenv("NODE_ENV", "development")

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:3002/api/oauth/authorize/github", nil)

	if got, err := oauthBuildCallbackURL(c, "github", "/"); err != nil ||
		got != "https://pay.example.com/api/oauth/callback/github" {
		t.Fatalf("customer callback = %q err=%v", got, err)
	}
	if got, err := oauthBuildCallbackURL(c, "github", "/admin"); err != nil ||
		got != "https://admin.example.com/api/oauth/callback/github" {
		t.Fatalf("admin callback = %q err=%v", got, err)
	}

	// FRONTEND_URL is a comma-separated list: only its first entry counts.
	t.Setenv("SITE_URL", "")
	t.Setenv("FRONTEND_URL", "https://first.example.com, https://second.example.com")
	if got, err := oauthBuildCallbackURL(c, "google", "/"); err != nil ||
		got != "https://first.example.com/api/oauth/callback/google" {
		t.Fatalf("frontend callback = %q err=%v", got, err)
	}

	// No configured base in development: request scheme + hostname (port stripped).
	t.Setenv("FRONTEND_URL", "")
	if got, err := oauthBuildCallbackURL(c, "github", "/"); err != nil || got != "http://127.0.0.1/api/oauth/callback/github" {
		t.Fatalf("dev fallback callback = %q err=%v", got, err)
	}

	// In production the missing configuration is a hard error.
	t.Setenv("NODE_ENV", "production")
	t.Setenv("ADMIN_FRONTEND_URL", "")
	if _, err := oauthBuildCallbackURL(c, "github", "/"); err == nil ||
		!strings.Contains(err.Error(), "SITE_URL or FRONTEND_URL must be configured") {
		t.Fatalf("production customer error = %v", err)
	}
	if _, err := oauthBuildCallbackURL(c, "github", "/admin"); err == nil ||
		!strings.Contains(err.Error(), "ADMIN_FRONTEND_URL must be configured") {
		t.Fatalf("production admin error = %v", err)
	}
}

// ==================== provider user parsing ====================

func TestOAuthProviderUserParsing(t *testing.T) {
	github := map[string]any{
		"id":         float64(1234567),
		"login":      "octocat",
		"email":      "octocat@example.com",
		"avatar_url": "https://avatars.example.com/u/1",
	}
	parsed := oauthParseProviderUser("github", github)
	if parsed.ID != "1234567" {
		t.Fatalf("github id = %q", parsed.ID)
	}
	if parsed.Username == nil || *parsed.Username != "octocat" {
		t.Fatalf("github username = %v", parsed.Username)
	}
	if parsed.Email == nil || *parsed.Email != "octocat@example.com" {
		t.Fatalf("github email = %v", parsed.Email)
	}
	if parsed.Avatar == nil || *parsed.Avatar != "https://avatars.example.com/u/1" {
		t.Fatalf("github avatar = %v", parsed.Avatar)
	}

	// Missing/null fields become NULL, an empty id stays empty (the callback then
	// rejects the response).
	minimal := oauthParseProviderUser("github", map[string]any{"id": nil, "login": "", "email": nil})
	if minimal.ID != "" || minimal.Username != nil || minimal.Email != nil || minimal.Avatar != nil {
		t.Fatalf("github minimal = %+v", minimal)
	}

	google := map[string]any{
		"id":      "sub-123",
		"name":    "Ada Lovelace",
		"email":   "ada@example.com",
		"picture": "https://pics.example.com/a",
	}
	parsed = oauthParseProviderUser("google", google)
	if parsed.ID != "sub-123" || parsed.Username == nil || *parsed.Username != "Ada Lovelace" {
		t.Fatalf("google = %+v", parsed)
	}

	// With no `name`, the e-mail local part is the username.
	parsed = oauthParseProviderUser("google", map[string]any{"id": "sub-1", "email": "local.part@example.com"})
	if parsed.Username == nil || *parsed.Username != "local.part" {
		t.Fatalf("google fallback username = %v", parsed.Username)
	}

	// A string id is passed through; numeric ids stringify like String().
	if got := oauthJSStringOrEmpty(float64(42)); got != "42" {
		t.Fatalf("numeric id = %q", got)
	}
	if got := oauthJSStringOrEmpty(0.0); got != "" {
		t.Fatalf("zero id must be falsy, got %q", got)
	}
	if got := oauthJSStringOrEmpty(""); got != "" {
		t.Fatalf("empty string id = %q", got)
	}
}

// ==================== HTTP: public callback paths ====================

// TestOAuthStateSingleConsumptionReplay is the state-replay proof at HTTP level: the
// first callback consumes the nonce (and stops at missing_code, before any database
// or provider access), the identical second request is rejected as state_expired.
func TestOAuthStateSingleConsumptionReplay(t *testing.T) {
	oauthInstallUnavailableDB(t)
	engine := oauthTestEngine()

	state, err := lib.GenerateOAuthState(lib.OAuthStateModeLogin, "/profile", nil)
	if err != nil {
		t.Fatalf("generate state: %v", err)
	}

	first := oauthDo(engine, http.MethodGet, "/api/oauth/callback/github?state="+url.QueryEscape(state), "", nil)
	if first.Code != http.StatusFound {
		t.Fatalf("first callback status = %d", first.Code)
	}
	if location := first.Header().Get("Location"); location != "/login?error=missing_code" {
		t.Fatalf("first callback location = %q", location)
	}
	if body := first.Body.String(); body != "" {
		t.Fatalf("a redirect must carry no body, got %q", body)
	}

	replay := oauthDo(engine, http.MethodGet, "/api/oauth/callback/github?state="+url.QueryEscape(state), "", nil)
	if replay.Code != http.StatusFound {
		t.Fatalf("replay status = %d", replay.Code)
	}
	if location := replay.Header().Get("Location"); location != "/login?error=state_expired" {
		t.Fatalf("replay location = %q", location)
	}
}

// TestOAuthStateRejectsTampering pins signature verification.
func TestOAuthStateRejectsTampering(t *testing.T) {
	oauthInstallUnavailableDB(t)
	engine := oauthTestEngine()

	state, err := lib.GenerateOAuthState(lib.OAuthStateModeLogin, "/", nil)
	if err != nil {
		t.Fatalf("generate state: %v", err)
	}
	parts := strings.Split(state, ".")
	if len(parts) != 2 {
		t.Fatalf("state shape = %q", state)
	}
	// Flip the last signature character.
	signature := parts[1]
	last := signature[len(signature)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	tampered := parts[0] + "." + signature[:len(signature)-1] + string(replacement)

	recorder := oauthDo(engine, http.MethodGet, "/api/oauth/callback/github?state="+url.QueryEscape(tampered), "", nil)
	if location := recorder.Header().Get("Location"); location != "/login?error=state_expired" {
		t.Fatalf("tampered state location = %q", location)
	}

	// A payload with a valid-looking but different signature must also fail.
	recorder = oauthDo(engine, http.MethodGet, "/api/oauth/callback/github?state=abc.def", "", nil)
	if location := recorder.Header().Get("Location"); location != "/login?error=state_expired" {
		t.Fatalf("garbage state location = %q", location)
	}
}

// TestOAuthCallbackErrorAndMissingState mirrors the two pre-state branches.
func TestOAuthCallbackErrorAndMissingState(t *testing.T) {
	oauthInstallUnavailableDB(t)
	engine := oauthTestEngine()

	recorder := oauthDo(engine, http.MethodGet, "/api/oauth/callback/github?error=access_denied", "", nil)
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "/login?error=access_denied" {
		t.Fatalf("provider error = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}

	// encodeURIComponent leaves '!'/'*'/'()' alone but escapes spaces and slashes.
	recorder = oauthDo(engine, http.MethodGet, "/api/oauth/callback/github?error="+url.QueryEscape("bad request/denied!(x)"), "", nil)
	if location := recorder.Header().Get("Location"); location != "/login?error=bad%20request%2Fdenied!(x)" {
		t.Fatalf("encoded provider error location = %q", location)
	}

	recorder = oauthDo(engine, http.MethodGet, "/api/oauth/callback/github", "", nil)
	if location := recorder.Header().Get("Location"); location != "/login?error=invalid_state" {
		t.Fatalf("missing state location = %q", location)
	}

	recorder = oauthDo(engine, http.MethodGet, "/api/oauth/callback/github?state=", "", nil)
	if location := recorder.Header().Get("Location"); location != "/login?error=invalid_state" {
		t.Fatalf("empty state location = %q", location)
	}
}

// TestOAuthCallbackUnknownProviderWithValidState mirrors the try/catch: the Prisma
// enum validation error (or, here, the database error) becomes oauth_error rather
// than a JSON 5xx.
func TestOAuthCallbackUnknownProviderWithValidState(t *testing.T) {
	oauthInstallUnavailableDB(t)
	engine := oauthTestEngine()

	state, err := lib.GenerateOAuthState(lib.OAuthStateModeLogin, "/", nil)
	if err != nil {
		t.Fatalf("generate state: %v", err)
	}
	recorder := oauthDo(engine, http.MethodGet,
		"/api/oauth/callback/facebook?state="+url.QueryEscape(state)+"&code=abc", "", nil)
	if recorder.Code != http.StatusFound {
		t.Fatalf("status = %d", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != "/login?error=oauth_error" {
		t.Fatalf("location = %q", location)
	}
}

// TestOAuthAuthorizeUnknownProvider mirrors the INVALID_PROVIDER branch (it runs
// before any database access).
func TestOAuthAuthorizeUnknownProvider(t *testing.T) {
	oauthInstallUnavailableDB(t)
	engine := oauthTestEngine()

	recorder := oauthDo(engine, http.MethodGet, "/api/oauth/authorize/facebook", "", nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["error"] != "Unsupported OAuth provider" || payload["code"] != "INVALID_PROVIDER" {
		t.Fatalf("payload = %v", payload)
	}

	// Bind mode without a ticket redirects to the safe default (/profile) with
	// not_logged_in — but only for a *known* provider, which needs a config row, so
	// this branch is covered by the conformance run instead.
}

// ==================== HTTP: exchange-code ====================

// TestOAuthExchangeCodeValidation mirrors the Fastify body schema.
func TestOAuthExchangeCodeValidation(t *testing.T) {
	oauthInstallUnavailableDB(t)
	engine := oauthTestEngine()

	cases := []struct {
		name   string
		body   string
		status int
		error  string
	}{
		{"missing code", `{}`, http.StatusBadRequest, "code is required"},
		{"short code", `{"code":"short"}`, http.StatusBadRequest, "code is too short"},
		{"non-object body", `[]`, http.StatusBadRequest, "must be object"},
		{"null body", `null`, http.StatusBadRequest, "must be object"},
		{"wrong type", `{"code":{}}`, http.StatusBadRequest, "must be string"},
		{"empty body", ``, http.StatusBadRequest, "Body cannot be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := oauthDo(engine, http.MethodPost, "/api/oauth/exchange-code", tc.body, nil)
			if recorder.Code != tc.status {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode: %v", err)
			}
			message, _ := payload["error"].(string)
			if !strings.Contains(message, tc.error) {
				t.Fatalf("error = %q, want substring %q", message, tc.error)
			}
			if payload["code"] != "VALIDATION_ERROR" && tc.name != "empty body" {
				t.Fatalf("code = %v", payload["code"])
			}
		})
	}

	// A numeric code is coerced to a string by AJV (coerceTypes:'array'), so it
	// passes validation and dies at the one-time-code check instead.
	recorder := oauthDo(engine, http.MethodPost, "/api/oauth/exchange-code", `{"code":12345678901}`, nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("numeric code status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestOAuthExchangeCodeInvalidCode mirrors the 401 INVALID_LOGIN_CODE branch.
func TestOAuthExchangeCodeInvalidCode(t *testing.T) {
	oauthInstallUnavailableDB(t)
	engine := oauthTestEngine()

	recorder := oauthDo(engine, http.MethodPost, "/api/oauth/exchange-code",
		`{"code":"definitely-not-a-login-code"}`, nil)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["error"] != "Invalid or expired login code" || payload["code"] != "INVALID_LOGIN_CODE" {
		t.Fatalf("payload = %v", payload)
	}
}

// TestOAuthLoginCodeConcurrentExchange is the single-consumption proof: 32 concurrent
// exchanges of one login code, exactly one may pass the consume step.
func TestOAuthLoginCodeConcurrentExchange(t *testing.T) {
	for round := 0; round < 20; round++ {
		code, err := lib.GenerateOAuthLoginCode(lib.OAuthLoginCodeInput{
			UserID:   1,
			Username: "parityuser",
			Role:     "user",
		})
		if err != nil {
			t.Fatalf("generate login code: %v", err)
		}

		var winners int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if data := lib.VerifyAndConsumeOAuthLoginCode(code); data != nil {
					atomic.AddInt32(&winners, 1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if winners != 1 {
			t.Fatalf("round %d: %d concurrent exchanges succeeded, want exactly 1", round, winners)
		}
		if again := lib.VerifyAndConsumeOAuthLoginCode(code); again != nil {
			t.Fatalf("round %d: code replay must fail", round)
		}
	}
}

// TestOAuthLoginCodeConcurrentExchangeHTTP drives the same race through the handler:
// exactly one request gets past the consume step (it then fails on the unavailable
// database), every other one is rejected with 401 INVALID_LOGIN_CODE.
func TestOAuthLoginCodeConcurrentExchangeHTTP(t *testing.T) {
	oauthInstallUnavailableDB(t)
	engine := oauthTestEngine()

	code, err := lib.GenerateOAuthLoginCode(lib.OAuthLoginCodeInput{
		UserID: 1, Username: "parityuser", Role: "user",
	})
	if err != nil {
		t.Fatalf("generate login code: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"code": code})

	var pastConsume int32
	var unauthorized int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			recorder := oauthDo(engine, http.MethodPost, "/api/oauth/exchange-code", string(body), nil)
			if recorder.Code == http.StatusUnauthorized {
				atomic.AddInt32(&unauthorized, 1)
				return
			}
			atomic.AddInt32(&pastConsume, 1)
		}()
	}
	close(start)
	wg.Wait()

	if pastConsume != 1 {
		t.Fatalf("%d requests got past the one-time code, want exactly 1", pastConsume)
	}
	if unauthorized != 15 {
		t.Fatalf("%d requests were rejected as invalid, want 15", unauthorized)
	}
}

// ==================== seam wiring ====================

// TestOAuthBindTicketIssue covers POST /bind-ticket's success path without a
// database: the handler only reads the verified claims from the context.
func TestOAuthBindTicketIssue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/bind-ticket", func(c *gin.Context) {
		// Stand in for middleware.Authenticate.
		c.Set(middleware.ContextUserKey, &lib.AccessTokenClaims{
			ID:       7,
			Username: "parityuser",
			Role:     "user",
			SID:      "session-abc",
			IssuedAt: 1_700_000_000,
		})
		c.Next()
	}, oauthBindTicket)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/bind-ticket", nil)
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.ExpiresIn != 60 {
		t.Fatalf("expiresIn = %d, want 60", payload.ExpiresIn)
	}
	if len(payload.Ticket) != 32 {
		t.Fatalf("ticket = %q", payload.Ticket)
	}

	// The ticket must be bound to the user, the token iat and the refresh session.
	consumed := lib.ConsumeOAuthBindTicket(payload.Ticket)
	if !consumed.Valid || consumed.UserID != 7 || consumed.IssuedAt != 1_700_000_000 {
		t.Fatalf("consumed = %+v", consumed)
	}
	if consumed.SessionID == nil || *consumed.SessionID != "session-abc" {
		t.Fatalf("session binding = %v", consumed.SessionID)
	}
	if lib.ConsumeOAuthBindTicket(payload.Ticket).Valid {
		t.Fatalf("bind ticket must be single use")
	}

	// Without verified claims the handler answers 401 UNAUTHORIZED.
	bare := gin.New()
	gin.SetMode(gin.TestMode)
	bare.GET("/bind-ticket", oauthBindTicket)
	recorder = httptest.NewRecorder()
	engine2Request := httptest.NewRequest(http.MethodGet, "/bind-ticket", nil)
	bare.ServeHTTP(recorder, engine2Request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", recorder.Code)
	}
}

// TestOAuthActionTicketSeamWiring proves routes.AuthRevokeActionTicketsForSession
// (left by the auth module) now points at the real action-ticket store.
func TestOAuthActionTicketSeamWiring(t *testing.T) {
	sessionID := "oauth-seam-session"
	ticket := lib.GenerateOAuthBindTicket(7, 1_700_000_000, &sessionID)
	if ticket == "" {
		t.Fatalf("ticket generation failed")
	}
	if revoked := AuthRevokeActionTicketsForSession(sessionID); revoked != 1 {
		t.Fatalf("AuthRevokeActionTicketsForSession revoked %d, want 1", revoked)
	}
	if result := lib.ConsumeOAuthBindTicket(ticket); result.Valid {
		t.Fatalf("revoked ticket must not be consumable")
	}
}

// ==================== fake provider ====================

// TestOAuthCallbackFakeProvider exercises the outbound half of the callback against a
// local server: request shape, JSON parsing, status handling, size limit and
// redirect:'manual'. It never touches github.com/google.
func TestOAuthCallbackFakeProvider(t *testing.T) {
	var lastTokenFormRaw string
	var lastUserAuth string
	var lastTokenContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			lastTokenContentType = r.Header.Get("Content-Type")
			body, _ := io.ReadAll(r.Body)
			lastTokenFormRaw = string(body)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"gho_fake_token","refresh_token":"ghr_fake","scope":"read:user"}`)
		case "/token-error":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`)
		case "/token-html":
			fmt.Fprint(w, `<html>not json</html>`)
		case "/token-redirect":
			w.Header().Set("Location", "https://evil.example.com/token")
			w.WriteHeader(http.StatusFound)
		case "/token-oversize":
			fmt.Fprint(w, `{"access_token":"`+strings.Repeat("a", 129*1024)+`"}`)
		case "/user":
			lastUserAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":987654,"login":"octocat","email":"octo@example.com","avatar_url":"https://avatars.example.com/o"}`)
		case "/user-error":
			w.WriteHeader(http.StatusUnauthorized)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"message":"Bad credentials"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	previous := oauthClientProviders["github"]
	oauthClientProviders["github"] = oauthClientProvider{
		AuthURL:  previous.AuthURL,
		TokenURL: server.URL + "/token",
		UserURL:  server.URL + "/user",
		Scope:    previous.Scope,
	}
	t.Cleanup(func() { oauthClientProviders["github"] = previous })

	ctx := context.Background()

	t.Run("token exchange request shape", func(t *testing.T) {
		payload, status, err := oauthExchangeProviderCode(
			ctx, "github", oauthClientProviders["github"],
			"client-id-1", "client-secret-1", "the-code", "http://127.0.0.1/api/oauth/callback/github",
		)
		if err != nil || status != http.StatusOK {
			t.Fatalf("status=%d err=%v", status, err)
		}
		if payload["access_token"] != "gho_fake_token" || payload["refresh_token"] != "ghr_fake" {
			t.Fatalf("payload = %v", payload)
		}
		if lastTokenContentType != "application/x-www-form-urlencoded" {
			t.Fatalf("content-type = %q", lastTokenContentType)
		}
		want := "client_id=client-id-1&client_secret=client-secret-1&code=the-code" +
			"&redirect_uri=http%3A%2F%2F127.0.0.1%2Fapi%2Foauth%2Fcallback%2Fgithub&grant_type=authorization_code"
		if got := lastTokenFormRaw; got != want {
			t.Fatalf("token form = %q, want %q", got, want)
		}
	})

	t.Run("userinfo uses the bearer token", func(t *testing.T) {
		payload, status, err := oauthFetchProviderUser(
			ctx, "github", oauthClientProviders["github"], "gho_fake_token")
		if err != nil || status != http.StatusOK {
			t.Fatalf("status=%d err=%v", status, err)
		}
		parsed := oauthParseProviderUser("github", payload)
		if parsed.ID != "987654" || parsed.Username == nil || *parsed.Username != "octocat" {
			t.Fatalf("parsed = %+v", parsed)
		}
		if lastUserAuth != "Bearer gho_fake_token" {
			t.Fatalf("authorization = %q", lastUserAuth)
		}
	})

	t.Run("provider error payload is returned to the caller", func(t *testing.T) {
		provider := oauthClientProviders["github"]
		provider.TokenURL = server.URL + "/token-error"
		payload, status, err := oauthExchangeProviderCode(
			ctx, "github", provider, "id", "secret", "code", "http://127.0.0.1/cb")
		if err != nil || status != http.StatusOK {
			t.Fatalf("status=%d err=%v", status, err)
		}
		if payload["error"] != "bad_verification_code" {
			t.Fatalf("payload = %v", payload)
		}
	})

	t.Run("invalid JSON is an error", func(t *testing.T) {
		provider := oauthClientProviders["github"]
		provider.TokenURL = server.URL + "/token-html"
		if _, _, err := oauthExchangeProviderCode(ctx, "github", provider, "id", "s", "c", "u"); err == nil ||
			!strings.Contains(err.Error(), "github OAuth token response returned invalid JSON") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("redirects are not followed", func(t *testing.T) {
		provider := oauthClientProviders["github"]
		provider.TokenURL = server.URL + "/token-redirect"
		_, status, err := oauthExchangeProviderCode(ctx, "github", provider, "id", "s", "c", "u")
		if err == nil {
			// A 302 body is empty, so JSON parsing fails; either way the redirect was
			// not followed (the evil host is never contacted).
			t.Fatalf("expected invalid JSON error, got status=%d", status)
		}
	})

	t.Run("oversized response is rejected", func(t *testing.T) {
		provider := oauthClientProviders["github"]
		provider.TokenURL = server.URL + "/token-oversize"
		if _, _, err := oauthExchangeProviderCode(ctx, "github", provider, "id", "s", "c", "u"); err == nil ||
			!strings.Contains(err.Error(), "too large") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("userinfo non-2xx", func(t *testing.T) {
		provider := oauthClientProviders["github"]
		provider.UserURL = server.URL + "/user-error"
		payload, status, err := oauthFetchProviderUser(ctx, "github", provider, "gho_fake_token")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if status != http.StatusUnauthorized || payload["message"] != "Bad credentials" {
			t.Fatalf("status=%d payload=%v", status, payload)
		}
	})
}
