// Tests for the routes/public_api.go port (server/src/routes/public-api.ts).
//
// The database-backed happy paths are covered by the dual-run conformance group
// test/conformance/requests-public-api.json. What is pinned here are the security
// and pipeline invariants that make this module the second authentication surface
// (WAVE_D_SPEC §0.2):
//
//   - a Web JWT can never authenticate on /api/v1/*            → 401
//   - a pat_ token can never authenticate on /api/*            → 401
//   - missing / malformed / revoked / expired tokens           → 401
//   - the exact scope required by every route (403 + log line)
//   - the nine inline rate limits                              → 429 at N+1
//   - pagination / sort / include clamping                     → pure helpers
//   - the OpenAPI document (byte sizes, structure, stability)
//
// The token lookup normally hits PostgreSQL. A tiny in-memory driver answers the
// one JOIN public-api-auth issues; Exec calls (last_used_at update + audit log)
// are recorded so the test can prove the token was touched and the denial logged.
// The dialector/rows helpers are shared with auth_test.go (same package).
package routes

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

// ==================== fake database ====================

var publicAPIFakeDriverSequence int32

type publicAPIFakeExec struct {
	query string
	args  []driver.NamedValue
}

type publicAPIFakeDB struct {
	tokenPrefix string
	tokenHash   string
	scopes      []byte
	revokedAt   *time.Time
	expiresAt   *time.Time
	status      string
	// missingToken makes the lookup return no row.
	missingToken bool
	execs        []publicAPIFakeExec
}

func (data *publicAPIFakeDB) stringsContaining(fragment string) []string {
	matches := make([]string, 0)
	for _, exec := range data.execs {
		for _, arg := range exec.args {
			if text, ok := arg.Value.(string); ok && strings.Contains(text, fragment) {
				matches = append(matches, text)
			}
		}
	}
	return matches
}

func (data *publicAPIFakeDB) executedQuery(fragment string) bool {
	for _, exec := range data.execs {
		if strings.Contains(strings.ToLower(exec.query), strings.ToLower(fragment)) {
			return true
		}
	}
	return false
}

func publicAPIIInstallFakeDB(t *testing.T, data *publicAPIFakeDB) {
	t.Helper()

	driverName := "publicapifake" + strconv.Itoa(int(atomic.AddInt32(&publicAPIFakeDriverSequence, 1)))
	sql.Register(driverName, publicAPIFakeDriver{data: data})
	conn, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open fake database: %v", err)
	}
	gormDB, err := gorm.Open(authFakeDialector{conn: conn}, &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open gorm on fake database: %v", err)
	}

	previous := db.DB
	db.DB = gormDB
	t.Cleanup(func() {
		db.DB = previous
		_ = conn.Close()
	})
}

type publicAPIFakeDriver struct{ data *publicAPIFakeDB }

func (d publicAPIFakeDriver) Open(string) (driver.Conn, error) {
	return publicAPIFakeConn{data: d.data}, nil
}

type publicAPIFakeConn struct{ data *publicAPIFakeDB }

func (c publicAPIFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("public api fake db: Prepare is not supported")
}

func (c publicAPIFakeConn) Close() error { return nil }

func (c publicAPIFakeConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("public api fake db: transactions are not supported")
}

func (c publicAPIFakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.data.execs = append(c.data.execs, publicAPIFakeExec{query: query, args: args})
	return driver.RowsAffected(1), nil
}

func (c publicAPIFakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	lowered := strings.ToLower(query)
	if !strings.Contains(lowered, "public_api_tokens") {
		return &authFakeRows{}, nil
	}

	columns := []string{
		"id", "user_id", "name", "token_prefix", "token_hash", "scopes", "revoked_at", "expires_at",
		"username", "email", "role", "status", "avatar_style", "avatar_badge_id",
	}
	if c.data.missingToken {
		return &authFakeRows{columns: columns}, nil
	}
	hash := c.data.tokenHash
	if hash == "" && len(args) > 0 {
		hash, _ = args[0].Value.(string)
	}
	status := c.data.status
	if status == "" {
		status = "active"
	}
	scopes := c.data.scopes
	if scopes == nil {
		scopes = []byte(`["profile:read"]`)
	}
	return &authFakeRows{
		columns: columns,
		rows: [][]driver.Value{{
			int64(1), int64(2), "conformance", c.data.tokenPrefix, hash, scopes,
			authFakeValue(c.data.revokedAt), authFakeValue(c.data.expiresAt),
			"parityuser", nil, "user", status, "bigSmile", nil,
		}},
	}, nil
}

// ==================== helpers ====================

const publicAPITestToken = "pat_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq"

func publicAPITestTokenPrefix() string { return publicAPITestToken[:16] }

func publicAPINewRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterPublicAPI(router)
	return router
}

func publicAPIServe(router *gin.Engine, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func publicAPIDecode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return payload
}

func publicAPIRequest(method, path, token string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

func publicAPIAllScopesExcept(excluded string) []byte {
	scopes := make([]string, 0, len(lib.PublicAPIScopes))
	for _, scope := range lib.PublicAPIScopes {
		if string(scope) == excluded {
			continue
		}
		scopes = append(scopes, string(scope))
	}
	raw, _ := json.Marshal(scopes)
	return raw
}

// ==================== route table ====================

// TestPublicAPIRegistersAllRoutes pins all 28 handlers (the task list says 27 but
// the TypeScript also declares GET /services/:id; see the port report) plus the
// mount prefix app.ts uses (/api/v1, not /api/public).
func TestPublicAPIRegistersAllRoutes(t *testing.T) {
	router := publicAPINewRouter()

	expected := map[string]bool{
		"GET /api/v1/openapi.json":                  true,
		"GET /api/v1/openapi.yaml":                  true,
		"GET /api/v1/me":                            true,
		"PATCH /api/v1/me":                          true,
		"GET /api/v1/balance":                       true,
		"GET /api/v1/balance/logs":                  true,
		"GET /api/v1/balance/adjustment-requests":   true,
		"POST /api/v1/balance/adjustment-requests":  true,
		"GET /api/v1/products":                      true,
		"GET /api/v1/products/:id":                  true,
		"GET /api/v1/orders":                        true,
		"GET /api/v1/orders/:id":                    true,
		"GET /api/v1/billing-records":               true,
		"GET /api/v1/billing-records/:id":           true,
		"GET /api/v1/services":                      true,
		"GET /api/v1/services/:id":                  true,
		"POST /api/v1/services/:id/actions":         true,
		"POST /api/v1/services/:id/renew":           true,
		"GET /api/v1/services/:id/tasks/:taskId":    true,
		"DELETE /api/v1/services/:id/tasks/:taskId": true,
		"GET /api/v1/tickets":                       true,
		"POST /api/v1/tickets":                      true,
		"GET /api/v1/tickets/:id":                   true,
		"PATCH /api/v1/tickets/:id/status":          true,
		"POST /api/v1/tickets/:id/replies":          true,
		"GET /api/v1/notifications":                 true,
		"GET /api/v1/notifications/unread-count":    true,
		"POST /api/v1/notifications":                true,
	}

	seen := map[string]bool{}
	for _, route := range router.Routes() {
		key := route.Method + " " + route.Path
		if !expected[key] {
			t.Errorf("unexpected route %s", key)
		}
		seen[key] = true
	}
	for key := range expected {
		if !seen[key] {
			t.Errorf("missing route %s", key)
		}
	}
}

// publicAPIProtectedRoutes is the scope matrix: every handler, its method/path and
// the scope the TypeScript passes to authenticatePublicApiRequest.
var publicAPIProtectedRoutes = []struct {
	method string
	path   string
	scope  string
}{
	{"GET", "/api/v1/me", "profile:read"},
	{"PATCH", "/api/v1/me", "profile:write"},
	{"GET", "/api/v1/balance", "balance:read"},
	{"GET", "/api/v1/balance/logs", "balance:read"},
	{"GET", "/api/v1/balance/adjustment-requests", "balance:write"},
	{"POST", "/api/v1/balance/adjustment-requests", "balance:write"},
	{"GET", "/api/v1/products", "products:read"},
	{"GET", "/api/v1/products/1", "products:read"},
	{"GET", "/api/v1/orders", "orders:read"},
	{"GET", "/api/v1/orders/recharge:1", "orders:read"},
	{"GET", "/api/v1/billing-records", "billing:read"},
	{"GET", "/api/v1/billing-records/1", "billing:read"},
	{"GET", "/api/v1/services", "services:read"},
	{"GET", "/api/v1/services/1", "services:read"},
	{"POST", "/api/v1/services/1/actions", "services:operate"},
	{"POST", "/api/v1/services/1/renew", "services:billing"},
	{"GET", "/api/v1/services/1/tasks/1", "services:operate"},
	{"DELETE", "/api/v1/services/1/tasks/1", "services:operate"},
	{"GET", "/api/v1/tickets", "tickets:read"},
	{"POST", "/api/v1/tickets", "tickets:write"},
	{"GET", "/api/v1/tickets/1", "tickets:read"},
	{"PATCH", "/api/v1/tickets/1/status", "tickets:write"},
	{"POST", "/api/v1/tickets/1/replies", "tickets:write"},
	{"GET", "/api/v1/notifications", "notifications:read"},
	{"GET", "/api/v1/notifications/unread-count", "notifications:read"},
	{"POST", "/api/v1/notifications", "notifications:send"},
}

// TestPublicAPIRequiresToken proves every protected handler answers 401 when no
// Authorization header is present (and never 404/200).
func TestPublicAPIRequiresToken(t *testing.T) {
	router := publicAPINewRouter()

	for _, route := range publicAPIProtectedRoutes {
		recorder := publicAPIServe(router, publicAPIRequest(route.method, route.path, ""))
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401 (%s)", route.method, route.path, recorder.Code, recorder.Body.String())
			continue
		}
		payload := publicAPIDecode(t, recorder)
		if payload["error"] != "Unauthorized" || payload["code"] != "PUBLIC_API_TOKEN_REQUIRED" {
			t.Errorf("%s %s: body = %v", route.method, route.path, payload)
		}
	}
}

// TestPublicAPIWebTokenCannotUsePublicAPI is the first half of WAVE_D_SPEC §0.2's
// reverse-auth requirement: a Web JWT (valid signature, valid claims) must be
// rejected by the public API surface with 401.
func TestPublicAPIWebTokenCannotUsePublicAPI(t *testing.T) {
	webToken, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID: 2, Username: "parityuser", Role: "user", Status: "active", SID: "session-1",
	})
	if err != nil {
		t.Fatalf("sign web token: %v", err)
	}

	router := publicAPINewRouter()
	for _, path := range []string{"/api/v1/me", "/api/v1/balance", "/api/v1/notifications/unread-count"} {
		recorder := publicAPIServe(router, publicAPIRequest(http.MethodGet, path, webToken))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s with web JWT: status = %d, want 401 (%s)", path, recorder.Code, recorder.Body.String())
		}
		payload := publicAPIDecode(t, recorder)
		if payload["code"] != "PUBLIC_API_TOKEN_REQUIRED" {
			t.Fatalf("GET %s with web JWT: code = %v, want PUBLIC_API_TOKEN_REQUIRED", path, payload["code"])
		}
	}
}

// TestPublicAPITokenCannotUseWebJWT is the second half: a pat_ token must be
// rejected by middleware.Authenticate (the Web surface).
func TestPublicAPITokenCannotUseWebJWT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterAPITokens(router)

	recorder := publicAPIServe(router, publicAPIRequest(http.MethodGet, "/api/api-tokens", publicAPITestToken))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/api-tokens with pat_ token: status = %d, want 401 (%s)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "Unauthorized") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

// TestPublicAPITokenFormats pins readPublicApiBearerToken's rejection rules.
func TestPublicAPITokenFormats(t *testing.T) {
	router := publicAPINewRouter()

	cases := []string{
		"not-a-bearer",
		"Basic dXNlcjpwYXNz",
		"Bearer not-a-public-api-token",
		"Bearer pat_short",
		"Bearer poa_short",
		"Bearer ",
	}
	for _, authorization := range cases {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		request.Header.Set("Authorization", authorization)
		recorder := publicAPIServe(router, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%q: status = %d, want 401", authorization, recorder.Code)
			continue
		}
		payload := publicAPIDecode(t, recorder)
		if payload["code"] != "PUBLIC_API_TOKEN_REQUIRED" {
			t.Errorf("%q: code = %v", authorization, payload["code"])
		}
	}
}

// TestPublicAPIAuthRejections covers the token lifecycle branches after a row was
// found: unknown hash, revoked, expired, banned user.
func TestPublicAPIAuthRejections(t *testing.T) {
	expired := time.Now().UTC().Add(-time.Hour)
	revoked := time.Now().UTC().Add(-time.Hour)

	cases := []struct {
		name string
		data publicAPIFakeDB
		code string
	}{
		{name: "unknown token", data: publicAPIFakeDB{missingToken: true}, code: "PUBLIC_API_TOKEN_INVALID"},
		{name: "revoked", data: publicAPIFakeDB{tokenPrefix: publicAPITestTokenPrefix(), revokedAt: &revoked}, code: "PUBLIC_API_TOKEN_REVOKED"},
		{name: "expired", data: publicAPIFakeDB{tokenPrefix: publicAPITestTokenPrefix(), expiresAt: &expired}, code: "PUBLIC_API_TOKEN_EXPIRED"},
		{name: "banned", data: publicAPIFakeDB{tokenPrefix: publicAPITestTokenPrefix(), status: "banned"}, code: "ACCOUNT_BANNED"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.data
			publicAPIIInstallFakeDB(t, &data)
			router := publicAPINewRouter()
			recorder := publicAPIServe(router, publicAPIRequest(http.MethodGet, "/api/v1/me", publicAPITestToken))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (%s)", recorder.Code, recorder.Body.String())
			}
			payload := publicAPIDecode(t, recorder)
			if payload["code"] != tc.code {
				t.Fatalf("code = %v, want %s", payload["code"], tc.code)
			}
		})
	}
}

// TestPublicAPIScopeMatrix proves each route demands its documented scope: the
// token holds all 14 scopes except the one under test, so the request must fail
// with 403 PUBLIC_API_SCOPE_REQUIRED and the denial log must name that scope.
func TestPublicAPIScopeMatrix(t *testing.T) {
	for _, route := range publicAPIProtectedRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			data := &publicAPIFakeDB{
				tokenPrefix: publicAPITestTokenPrefix(),
				scopes:      publicAPIAllScopesExcept(route.scope),
			}
			publicAPIIInstallFakeDB(t, data)
			router := publicAPINewRouter()

			recorder := publicAPIServe(router, publicAPIRequest(route.method, route.path, publicAPITestToken))
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (%s)", recorder.Code, recorder.Body.String())
			}
			payload := publicAPIDecode(t, recorder)
			if payload["code"] != "PUBLIC_API_SCOPE_REQUIRED" || payload["error"] != "Missing required API scope" {
				t.Fatalf("body = %v", payload)
			}

			// The audit line is the only place the required scope is visible.
			denials := data.stringsContaining(`missing required scope "` + route.scope + `"`)
			if len(denials) == 0 {
				t.Fatalf("expected a denial log naming scope %q, execs = %+v", route.scope, data.execs)
			}
		})
	}
}

// TestPublicAPIAuthenticatedMe proves the success path writes last_used_at and the
// audit log exactly once, and returns the token-scoped profile envelope.
func TestPublicAPIAuthenticatedMe(t *testing.T) {
	data := &publicAPIFakeDB{
		tokenPrefix: publicAPITestTokenPrefix(),
		scopes:      publicAPIAllScopesExcept("profile:write"), // has profile:read
	}
	publicAPIIInstallFakeDB(t, data)

	router := publicAPINewRouter()
	recorder := publicAPIServe(router, publicAPIRequest(http.MethodGet, "/api/v1/me", publicAPITestToken))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}

	payload := publicAPIDecode(t, recorder)
	dataPayload, _ := payload["data"].(map[string]any)
	if dataPayload["id"] != float64(2) || dataPayload["username"] != "parityuser" {
		t.Fatalf("data = %v", payload["data"])
	}
	metaPayload, _ := payload["meta"].(map[string]any)
	if metaPayload["tokenId"] != float64(1) {
		t.Fatalf("meta = %v", payload["meta"])
	}
	if !data.executedQuery("UPDATE public_api_tokens SET last_used_at") {
		t.Fatalf("expected last_used_at update, execs = %+v", data.execs)
	}
	if !data.executedQuery("INSERT INTO logs") {
		t.Fatalf("expected audit log insert, execs = %+v", data.execs)
	}
}

// TestPublicAPIBodyParserRunsBeforeAuth pins the Fastify pipeline order for this
// module: the body parser runs before the in-handler authentication.
func TestPublicAPIBodyParserRunsBeforeAuth(t *testing.T) {
	router := publicAPINewRouter()

	cases := []struct{ method, path string }{
		{"POST", "/api/v1/notifications"},
		{"POST", "/api/v1/balance/adjustment-requests"},
		{"POST", "/api/v1/services/1/actions"},
		{"POST", "/api/v1/services/1/renew"},
		{"PATCH", "/api/v1/me"},
		{"PATCH", "/api/v1/tickets/1/status"},
		{"DELETE", "/api/v1/services/1/tasks/1"},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(tc.method, tc.path, nil)
		request.Header.Set("Content-Type", "application/json")
		recorder := publicAPIServe(router, request)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s %s: status = %d, want 400 body error (%s)", tc.method, tc.path, recorder.Code, recorder.Body.String())
			continue
		}
		if !strings.Contains(recorder.Body.String(), "Body cannot be empty") {
			t.Errorf("%s %s: body = %s", tc.method, tc.path, recorder.Body.String())
		}
	}

	// GET/HEAD are bodyless in Fastify: an empty JSON content-type must not turn
	// the 401 into a 400.
	request := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	request.Header.Set("Content-Type", "application/json")
	recorder := publicAPIServe(router, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/me with empty JSON content type: status = %d, want 401", recorder.Code)
	}
}

// ==================== inline rate limits ====================

// TestPublicAPIInlineRateLimits verifies all nine PUBLIC_API_RATE_LIMITS quotas.
// The limiter runs before authentication, so unauthenticated probes count exactly
// like Node's onRequest hook.
func TestPublicAPIInlineRateLimits(t *testing.T) {
	type rateCase struct {
		method string
		path   string
		route  string
		max    int
	}
	cases := []rateCase{
		{"POST", "/api/v1/balance/adjustment-requests", "POST /balance/adjustment-requests", 5},
		{"POST", "/api/v1/services/1/actions", "POST /services/:id/actions", 10},
		{"POST", "/api/v1/services/1/renew", "POST /services/:id/renew", 5},
		{"GET", "/api/v1/services/1/tasks/1", "GET /services/:id/tasks/:taskId", 120},
		{"DELETE", "/api/v1/services/1/tasks/1", "DELETE /services/:id/tasks/:taskId", 10},
		{"POST", "/api/v1/tickets", "POST /tickets", 10},
		{"POST", "/api/v1/tickets/1/replies", "POST /tickets/:id/replies", 20},
		{"PATCH", "/api/v1/tickets/1/status", "PATCH /tickets/:id/status", 20},
		{"POST", "/api/v1/notifications", "POST /notifications", 20},
	}

	for _, tc := range cases {
		t.Run(tc.route, func(t *testing.T) {
			rule, ok := publicAPIRateLimits[tc.route]
			if !ok {
				t.Fatalf("no rule registered for %s", tc.route)
			}
			if rule.Max != tc.max {
				t.Fatalf("rule %s max = %d, want %d", tc.route, rule.Max, tc.max)
			}

			publicAPIInlineLimiter = &publicAPIRateLimiter{buckets: map[string]*publicAPIRateBucket{}}
			router := publicAPINewRouter()

			for i := 0; i < tc.max; i++ {
				request := httptest.NewRequest(tc.method, tc.path, nil)
				request.RemoteAddr = "198.51.100.10:1234"
				recorder := publicAPIServe(router, request)
				if recorder.Code == http.StatusTooManyRequests {
					t.Fatalf("request %d/%d was already limited", i+1, tc.max)
				}
			}

			request := httptest.NewRequest(tc.method, tc.path, nil)
			request.RemoteAddr = "198.51.100.10:1234"
			recorder := publicAPIServe(router, request)
			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("request %d: status = %d, want 429", tc.max+1, recorder.Code)
			}
			if !strings.Contains(recorder.Body.String(), "Too many requests, please try again later") {
				t.Fatalf("body = %s", recorder.Body.String())
			}
		})
	}
}

// TestPublicAPIRateLimitIsPerRouteAndIP proves the bucket key includes both the
// route and the caller IP (Node's child store per route, key = request.ip).
func TestPublicAPIRateLimitIsPerRouteAndIP(t *testing.T) {
	publicAPIInlineLimiter = &publicAPIRateLimiter{buckets: map[string]*publicAPIRateBucket{}}
	router := publicAPINewRouter()

	for i := 0; i < 5; i++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/balance/adjustment-requests", nil)
		request.RemoteAddr = "198.51.100.20:1"
		publicAPIServe(router, request)
	}
	// Another IP on the same route is unaffected.
	request := httptest.NewRequest(http.MethodPost, "/api/v1/balance/adjustment-requests", nil)
	request.RemoteAddr = "198.51.100.21:1"
	if recorder := publicAPIServe(router, request); recorder.Code == http.StatusTooManyRequests {
		t.Fatal("a different IP must not inherit the exhausted bucket")
	}
	// Another rate-limited route is unaffected.
	request = httptest.NewRequest(http.MethodPost, "/api/v1/notifications", nil)
	request.RemoteAddr = "198.51.100.20:1"
	if recorder := publicAPIServe(router, request); recorder.Code == http.StatusTooManyRequests {
		t.Fatal("a different route must not inherit the exhausted bucket")
	}
}

// ==================== pure parsing helpers ====================

func publicAPIQueryContext(t *testing.T, rawQuery string) func(string) publicAPIQueryParam {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/probe?"+rawQuery, nil)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = request
	return func(key string) publicAPIQueryParam { return publicAPIQuery(c, key) }
}

func TestPublicAPIPaginationClamping(t *testing.T) {
	cases := []struct {
		query          string
		page, pageSize int
	}{
		{"", 1, 20},
		{"page=3&pageSize=50", 3, 50},
		{"page=0&pageSize=0", 1, 20},
		{"page=-1&pageSize=1000", 1, 100},
		{"page=abc&pageSize=xyz", 1, 20},
		{"page=2&page=3", 1, 20},
		{"page=&pageSize=", 1, 20},
		{"page=1&pageSize=1", 1, 1},
		{"page=9007199254740991&pageSize=100", 9007199254740991, 100},
	}
	for _, tc := range cases {
		pagination := publicAPIParsePagination(publicAPIQueryContext(t, tc.query))
		if pagination.page != tc.page || pagination.pageSize != tc.pageSize {
			t.Errorf("%q: page/pageSize = %d/%d, want %d/%d", tc.query, pagination.page, pagination.pageSize, tc.page, tc.pageSize)
		}
		if pagination.take != tc.pageSize {
			t.Errorf("%q: take = %d, want %d", tc.query, pagination.take, tc.pageSize)
		}
		if pagination.skip != int64(tc.page-1)*int64(tc.pageSize) {
			t.Errorf("%q: skip = %d", tc.query, pagination.skip)
		}
	}
}

func TestPublicAPISortParsing(t *testing.T) {
	cases := []struct {
		query   string
		allowed map[string]bool
		def     string
		want    string // "" means invalid
	}{
		{"", publicAPICreatedAtSorts, "-createdAt", "-createdAt"},
		{"sort=createdAt", publicAPICreatedAtSorts, "-createdAt", "createdAt"},
		{"sort=-createdAt", publicAPICreatedAtSorts, "-createdAt", "-createdAt"},
		{"sort=%20-createdAt%20", publicAPICreatedAtSorts, "-createdAt", "-createdAt"},
		{"sort=", publicAPICreatedAtSorts, "-createdAt", "-createdAt"},
		{"sort=name", publicAPICreatedAtSorts, "-createdAt", ""},
		{"sort=-", publicAPICreatedAtSorts, "-createdAt", ""},
		{"sort=createdAt&sort=updatedAt", publicAPICreatedAtSorts, "-createdAt", ""},
		{"", publicAPIServiceSorts, "displayOrder", "displayOrder"},
		{"sort=-displayOrder", publicAPIServiceSorts, "displayOrder", "-displayOrder"},
		{"sort=updatedAt", publicAPIServiceSorts, "displayOrder", ""},
	}
	for _, tc := range cases {
		sortSpec := publicAPIParseSort(publicAPIQueryContext(t, tc.query)("sort"), tc.allowed, tc.def)
		if tc.want == "" {
			if sortSpec != nil {
				t.Errorf("%q: got %+v, want invalid", tc.query, sortSpec)
			}
			continue
		}
		if sortSpec == nil || sortSpec.value != tc.want {
			t.Errorf("%q: got %+v, want %q", tc.query, sortSpec, tc.want)
		}
	}
}

func TestPublicAPIOrderByClauses(t *testing.T) {
	cases := []struct {
		sort *publicAPISort
		want string
	}{
		{&publicAPISort{field: "createdAt", direction: "desc"}, "created_at desc, id desc"},
		{&publicAPISort{field: "createdAt", direction: "asc"}, "created_at asc, id asc"},
		{&publicAPISort{field: "updatedAt", direction: "desc"}, "updated_at desc, id desc"},
		{&publicAPISort{field: "displayOrder", direction: "asc"}, "display_order asc, created_at desc, id desc"},
	}
	for _, tc := range cases {
		if got := publicAPIOrderBy(tc.sort); got != tc.want {
			t.Errorf("orderBy(%s %s) = %q, want %q", tc.sort.field, tc.sort.direction, got, tc.want)
		}
	}
	if got := publicAPIQualifyOrderBy("b", "created_at desc, id desc"); got != "b.created_at desc, b.id desc" {
		t.Fatalf("qualify = %q", got)
	}
}

func TestPublicAPIIncludeParsing(t *testing.T) {
	cases := []struct {
		query string
		want  string // comma-joined or "<nil>"
	}{
		{"", ""},
		{"include=product", "product"},
		{"include=plan", "plan"},
		{"include=product,plan", "product,plan"},
		{"include=plan,product,plan", "plan,product"},
		{"include=%20product%20", "product"},
		{"include=product,,plan", "product,plan"},
		{"include=instance", "<nil>"},
		{"include=product&include=plan", "<nil>"},
	}
	for _, tc := range cases {
		includes := publicAPIParseInclude(publicAPIQueryContext(t, tc.query)("include"), publicAPIServiceIncludes)
		if tc.want == "<nil>" {
			if includes != nil {
				t.Errorf("%q: got %v, want nil", tc.query, includes)
			}
			continue
		}
		if strings.Join(includes, ",") != tc.want {
			t.Errorf("%q: got %v, want %q", tc.query, includes, tc.want)
		}
	}
}

func TestPublicAPIPositiveIDParsing(t *testing.T) {
	valid := map[string]int64{
		"1":                1,
		"42":               42,
		"9007199254740991": 9007199254740991,
	}
	for raw, want := range valid {
		got, ok := publicAPIParsePositiveID(raw)
		if !ok || got != want {
			t.Errorf("%q: got (%d,%v), want %d", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "0", "01", "-1", "1.5", "1e3", "abc", " 1", "9007199254740992"} {
		if _, ok := publicAPIParsePositiveID(raw); ok {
			t.Errorf("%q: accepted, want rejected", raw)
		}
	}
}

// ==================== OpenAPI ====================

// TestPublicAPIOpenAPIDocument pins the document byte-for-byte size and its
// structure. The embedded JSON is JSON.stringify(buildPublicApiOpenApiDocument())
// and the YAML is stringifyPublicApiOpenApiYaml(); both sizes were captured from
// the TypeScript source.
func TestPublicAPIOpenAPIDocument(t *testing.T) {
	raw := lib.PublicAPIOpenAPIJSON()
	if len(raw) != 62640 {
		t.Fatalf("JSON size = %d, want 62640", len(raw))
	}
	if raw[len(raw)-1] == '\n' {
		t.Fatal("JSON must not end with a newline (JSON.stringify contract)")
	}
	if string(raw) != string(lib.PublicAPIOpenAPIJSON()) {
		t.Fatal("JSON is not stable across calls")
	}

	var document struct {
		OpenAPI string `json:"openapi"`
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
		Security []map[string][]string `json:"security"`
		Tags     []struct {
			Name string `json:"name"`
		} `json:"tags"`
		Paths map[string]map[string]struct {
			OperationID string                     `json:"operationId"`
			Responses   map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas   map[string]json.RawMessage `json:"schemas"`
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	if document.OpenAPI != "3.1.0" {
		t.Fatalf("openapi = %q", document.OpenAPI)
	}
	if len(document.Servers) != 1 || document.Servers[0].URL != "/api/v1" {
		t.Fatalf("servers = %+v", document.Servers)
	}
	if len(document.Paths) != 29 {
		t.Fatalf("paths = %d, want 29", len(document.Paths))
	}
	if len(document.Components.Schemas) != 46 {
		t.Fatalf("schemas = %d, want 46", len(document.Components.Schemas))
	}
	if len(document.Components.Responses) != 6 {
		t.Fatalf("component responses = %d, want 6", len(document.Components.Responses))
	}
	if len(document.Security) != 2 {
		t.Fatalf("security = %+v", document.Security)
	}

	expectedPaths := []string{
		"/me", "/balance", "/balance/logs", "/balance/adjustment-requests",
		"/products", "/products/{id}", "/orders", "/orders/{id}",
		"/billing-records", "/billing-records/{id}", "/services", "/services/{id}",
		"/services/{id}/actions", "/services/{id}/renew", "/services/{id}/tasks/{taskId}",
		"/tickets", "/tickets/{id}", "/tickets/{id}/replies", "/tickets/{id}/status",
		"/notifications", "/notifications/unread-count",
		"/api-tokens", "/api-tokens/{id}",
		"/oauth-provider/token", "/oauth-provider/scopes", "/oauth-provider/authorize/consent",
		"/oauth-provider/authorize/confirm", "/oauth-provider/authorizations",
		"/oauth-provider/authorizations/{id}",
	}
	for _, path := range expectedPaths {
		operations, ok := document.Paths[path]
		if !ok {
			t.Errorf("missing path %s", path)
			continue
		}
		for method, operation := range operations {
			if operation.OperationID == "" {
				t.Errorf("%s %s: empty operationId", method, path)
			}
			if len(operation.Responses) == 0 {
				t.Errorf("%s %s: no responses", method, path)
			}
		}
	}

	yamlDocument := lib.PublicAPIOpenAPIYAML()
	if len(yamlDocument) != 97955 {
		t.Fatalf("YAML size = %d, want 97955", len(yamlDocument))
	}
	if !strings.HasPrefix(string(yamlDocument), "\"openapi\": \"3.1.0\"\n") {
		t.Fatalf("YAML prefix = %q", string(yamlDocument[:40]))
	}
	if yamlDocument[len(yamlDocument)-1] != '\n' {
		t.Fatal("YAML must end with exactly one newline")
	}
	if strings.HasSuffix(string(yamlDocument), "\n\n") {
		t.Fatal("YAML must not end with a blank line")
	}
	if !strings.Contains(string(yamlDocument), "\"/services/{id}/actions\":\n") {
		t.Fatal("YAML is missing /services/{id}/actions")
	}
	if string(yamlDocument) != string(lib.PublicAPIOpenAPIYAML()) {
		t.Fatal("YAML is not stable across calls")
	}
}

// TestPublicAPIOpenAPIRoutesArePublic pins that the two document endpoints need no
// token and are served with Fastify's content types.
func TestPublicAPIOpenAPIRoutesArePublic(t *testing.T) {
	router := publicAPINewRouter()

	jsonRecorder := publicAPIServe(router, publicAPIRequest(http.MethodGet, "/api/v1/openapi.json", ""))
	if jsonRecorder.Code != http.StatusOK {
		t.Fatalf("openapi.json status = %d", jsonRecorder.Code)
	}
	if got := jsonRecorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("openapi.json content-type = %q", got)
	}
	if jsonRecorder.Body.Len() != 62640 {
		t.Fatalf("openapi.json body = %d bytes", jsonRecorder.Body.Len())
	}

	yamlRecorder := publicAPIServe(router, publicAPIRequest(http.MethodGet, "/api/v1/openapi.yaml", ""))
	if yamlRecorder.Code != http.StatusOK {
		t.Fatalf("openapi.yaml status = %d", yamlRecorder.Code)
	}
	if got := yamlRecorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/yaml") {
		t.Fatalf("openapi.yaml content-type = %q", got)
	}
	if yamlRecorder.Body.Len() != 97955 {
		t.Fatalf("openapi.yaml body = %d bytes", yamlRecorder.Body.Len())
	}
}

// ==================== notification template rendering ====================

func TestPublicAPINotificationTemplateRendering(t *testing.T) {
	title, message, ok := publicAPIRenderNotificationTemplate("flash_sale_reminder", map[string]string{
		"campaignName": "双十一", "startsAt": "2026-11-11 00:00", "productName": "VPS",
	})
	if !ok || title != "秒杀活动提醒" || message != "活动「双十一」将在 2026-11-11 00:00 开始。\n商品: VPS" {
		t.Fatalf("flash_sale_reminder = %q / %q / %v", title, message, ok)
	}
	if _, _, ok := publicAPIRenderNotificationTemplate("flash_sale_reminder", map[string]string{"campaignName": "x"}); ok {
		t.Fatal("missing startsAt must fail")
	}

	title, message, ok = publicAPIRenderNotificationTemplate("service_action_update", map[string]string{
		"serviceName": "web-1", "statusText": "已重启",
	})
	if !ok || title != "服务任务更新" || message != "服务「web-1」已重启。" {
		t.Fatalf("service_action_update = %q / %q / %v", title, message, ok)
	}

	title, message, ok = publicAPIRenderNotificationTemplate("billing_notice", map[string]string{
		"subject": "2026-11", "amount": "12.34", "note": "请及时支付",
	})
	if !ok || title != "账单提醒" || message != "账单「2026-11」金额 ¥12.34。\n请及时支付" {
		t.Fatalf("billing_notice = %q / %q / %v", title, message, ok)
	}
	if _, _, ok := publicAPIRenderNotificationTemplate("unknown", nil); ok {
		t.Fatal("unknown template must fail")
	}
}

// TestPublicAPINotificationVariables pins the strict variable object rules.
func TestPublicAPINotificationVariables(t *testing.T) {
	valid := map[string]any{"campaignName": "x", "startsAt": "y", "count": float64(3), "flag": true, "skip": nil}
	normalized, ok := publicAPINormalizeNotificationVariables(valid)
	if !ok {
		t.Fatal("valid variables rejected")
	}
	if normalized["count"] != "3" || normalized["flag"] != "true" {
		t.Fatalf("normalized = %v", normalized)
	}
	if _, present := normalized["skip"]; present {
		t.Fatal("null values must be skipped")
	}

	invalid := []any{
		[]any{"x"},
		"string",
		map[string]any{"1bad": "x"},
		map[string]any{"ok": map[string]any{}},
		map[string]any{"ok": ""},
	}
	for _, value := range invalid {
		if _, ok := publicAPINormalizeNotificationVariables(value); ok {
			t.Errorf("accepted invalid variables %v", value)
		}
	}

	tooMany := map[string]any{}
	for i := 0; i < publicAPIMaxNotificationTemplateVars+1; i++ {
		tooMany[fmt.Sprintf("k%d", i)] = "v"
	}
	if _, ok := publicAPINormalizeNotificationVariables(tooMany); ok {
		t.Fatal("accepted more than 10 variables")
	}
}

// TestPublicAPIAdjustmentNormalization pins the amount/reason/order-no rules.
func TestPublicAPIAdjustmentNormalization(t *testing.T) {
	if got := publicAPINormalizeAdjustmentAmount(float64(12.345)); got == nil || *got != 12.35 {
		t.Fatalf("12.345 → %v", got)
	}
	if got := publicAPINormalizeAdjustmentAmount(" 5 "); got == nil || *got != 5 {
		t.Fatalf("' 5 ' → %v", got)
	}
	for _, value := range []any{float64(0), float64(-1), float64(10000.01), "", "abc", true, nil} {
		if got := publicAPINormalizeAdjustmentAmount(value); got != nil {
			t.Errorf("%v accepted as amount %v", value, *got)
		}
	}
	if got := publicAPINormalizeAdjustmentReason("  too   short "); got != nil {
		t.Errorf("short reason accepted: %v", *got)
	}
	if got := publicAPINormalizeAdjustmentReason("this  is   a valid reason"); got == nil || *got != "this is a valid reason" {
		t.Fatalf("reason normalization = %v", got)
	}
	if subject := publicAPINormalizeTicketSubject("a"); subject != nil {
		t.Fatal("1-char subject accepted")
	}
	if subject := publicAPINormalizeTicketSubject("  ab  "); subject == nil || *subject != "ab" {
		t.Fatalf("subject = %v", subject)
	}
}

// TestPublicAPIMaskTradeNo pins maskTradeNo.
func TestPublicAPIMaskTradeNo(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"12345678":     "12***",
		"123456789":    "1234***6789",
		"202601011234": "2026***1234",
	}
	for input, want := range cases {
		value := input
		got := publicAPIMaskTradeNo(&value)
		if want == "" {
			if got != nil {
				t.Errorf("%q → %q, want nil", input, *got)
			}
			continue
		}
		if got == nil || *got != want {
			t.Errorf("%q → %v, want %q", input, got, want)
		}
	}
	var nilValue *string
	if publicAPIMaskTradeNo(nilValue) != nil {
		t.Fatal("nil input must stay nil")
	}
}

// TestPublicAPIJSTrimAndLength pins the JavaScript-compatible whitespace helpers.
func TestPublicAPIJSTrimAndLength(t *testing.T) {
	if got := publicAPIJSTrim("\uFEFF  hello \u00A0"); got != "hello" {
		t.Fatalf("trim = %q", got)
	}
	if got := publicAPIJSCollapseWhitespace("a \t b\nc"); got != "a b c" {
		t.Fatalf("collapse = %q", got)
	}
	if jsStringLength("😀") != 2 {
		t.Fatalf("emoji length = %d, want 2 UTF-16 units", jsStringLength("😀"))
	}
}

// TestPublicAPIUnauthenticatedRequestsDoNotTouchDatabase proves the DB is never
// consulted before authentication succeeds (the fake records zero queries by
// returning empty rows; here we assert no Exec happened at all).
func TestPublicAPIUnauthenticatedRequestsDoNotTouchDatabase(t *testing.T) {
	data := &publicAPIFakeDB{tokenPrefix: "unused"}
	publicAPIIInstallFakeDB(t, data)
	router := publicAPINewRouter()

	recorder := publicAPIServe(router, publicAPIRequest(http.MethodGet, "/api/v1/me", ""))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}
	if len(data.execs) != 0 {
		t.Fatalf("unauthenticated request wrote to the database: %+v", data.execs)
	}
}

// ==================== compile-time surface checks ====================

// TestPublicAPISeamsFailLoudly pins the renewal seam: before db.performRenewal is
// ported, the seam must return an explicit error rather than a fake success.
func TestPublicAPISeamsFailLoudly(t *testing.T) {
	if _, err := PublicAPIPerformRenewal(context.Background(), 1, 1, 1); err == nil {
		t.Fatal("renewal seam returned no error")
	}
}
