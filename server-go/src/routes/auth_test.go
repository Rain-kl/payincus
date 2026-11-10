// Tests for the routes/auth.go port (server/src/routes/auth.ts).
//
// DB-backed happy paths are covered by the dual-run conformance suite
// (server-go/test/conformance/requests-auth.json). What is pinned here are the
// security controls the Wave D spec (§3) makes mandatory and the wire contract
// the Fastify pipeline alone decides:
//
//   - a Public API token (`pat_…`) can never act as a Web JWT      → 401
//   - an expired Web token                                          → 401
//   - a Web token hit by token_invalidations                        → 401 Session expired
//   - a failed Turnstile challenge                                  → 403
//   - AJV first-error messages/order for the auth schemas
//   - body-parser messages/statuses and the refresh/logout cookie-less paths
//
// The invalidation and Turnstile cases need two reads that normally hit
// PostgreSQL (token_invalidations and system_configs). To keep `go test` free of a
// database, the helpers below install a minimal in-memory database/sql driver and
// hand it to GORM. It only answers those two tables; nothing is written.
package routes

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== in-memory GORM backend ====================

// authFakeDBData is the state the fake driver serves.
type authFakeDBData struct {
	// config holds system_configs rows by key.
	config map[string]string
	// invalidations is keyed by "userID|sessionID".
	invalidations map[string]authFakeInvalidation
	// user is the row returned for the middleware's users lookup.
	user *authFakeUser
	// invites are the rows returned by the /invites listing query.
	invites []authFakeInvite
}

// authFakeUser is the (username, role, status) projection middleware reads.
type authFakeUser struct {
	username string
	role     string
	status   string
}

// authFakeInvite is one row of the /invites projection.
type authFakeInvite struct {
	id                 int64
	code               string
	createdBy          int64
	usedBy             *int64
	usedAt             *time.Time
	expiresAt          *time.Time
	costSnapshot       []byte
	createdAt          time.Time
	creatorUsername    *string
	creatorEmail       *string
	creatorAvatarStyle *string
	usedUsername       *string
	usedEmail          *string
	usedAvatarStyle    *string
}

// authFakeInvalidation is one token_invalidations row.
type authFakeInvalidation struct {
	userID        int64
	sessionID     string
	invalidatedAt int64
}

func (d *authFakeDBData) addInvalidation(userID int64, sessionID string, invalidatedAt int64) {
	if d.invalidations == nil {
		d.invalidations = map[string]authFakeInvalidation{}
	}
	key := fmt.Sprintf("%d|%s", userID, sessionID)
	d.invalidations[key] = authFakeInvalidation{userID: userID, sessionID: sessionID, invalidatedAt: invalidatedAt}
}

var authFakeDriverSequence int32

// authInstallFakeDB replaces db.DB with a GORM handle backed by the fake driver
// and restores the previous handle when the test finishes.
func authInstallFakeDB(t *testing.T, data *authFakeDBData) {
	t.Helper()

	driverName := "authfake" + strconv.Itoa(int(atomic.AddInt32(&authFakeDriverSequence, 1)))
	sql.Register(driverName, authFakeDriver{data: data})
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

type authFakeDriver struct{ data *authFakeDBData }

func (d authFakeDriver) Open(string) (driver.Conn, error) { return authFakeConn{data: d.data}, nil }

type authFakeConn struct{ data *authFakeDBData }

func (c authFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("auth fake db: Prepare is not supported")
}

func (c authFakeConn) Close() error { return nil }

func (c authFakeConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("auth fake db: transactions are not supported")
}

func (c authFakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

func (c authFakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	lowered := strings.ToLower(query)
	switch {
	case strings.Contains(lowered, "token_invalidations"):
		return c.queryInvalidations(args), nil
	case strings.Contains(lowered, "system_configs"):
		return c.querySystemConfig(args), nil
	case strings.Contains(lowered, "invite_codes"):
		if strings.Contains(lowered, "count(") {
			return &authFakeRows{
				columns: []string{"count"},
				rows:    [][]driver.Value{{int64(len(c.data.invites))}},
			}, nil
		}
		return c.queryInvites(), nil
	case strings.Contains(lowered, "users"):
		return c.queryUser(), nil
	default:
		// Unknown table: behave like an empty result set (GORM reports
		// ErrRecordNotFound, which is what an absent row means).
		return &authFakeRows{}, nil
	}
}

func (c authFakeConn) queryInvalidations(args []driver.NamedValue) driver.Rows {
	columns := []string{"id", "user_id", "session_id", "invalidated_at", "created_at"}
	if len(args) < 2 {
		return &authFakeRows{columns: columns}
	}
	userID, ok := authFakeInt64(args[0].Value)
	if !ok {
		return &authFakeRows{columns: columns}
	}
	sessionID, _ := args[1].Value.(string)
	row, found := c.data.invalidations[fmt.Sprintf("%d|%s", userID, sessionID)]
	if !found {
		return &authFakeRows{columns: columns}
	}
	return &authFakeRows{
		columns: columns,
		rows: [][]driver.Value{{
			int64(1), row.userID, row.sessionID, row.invalidatedAt, time.Now().UTC(),
		}},
	}
}

func (c authFakeConn) querySystemConfig(args []driver.NamedValue) driver.Rows {
	columns := []string{"id", "key", "value", "type", "label", "description", "created_at", "updated_at"}
	if len(args) < 1 {
		return &authFakeRows{columns: columns}
	}
	key, _ := args[0].Value.(string)
	value, found := c.data.config[key]
	if !found {
		return &authFakeRows{columns: columns}
	}
	return &authFakeRows{
		columns: columns,
		rows: [][]driver.Value{{
			int64(1), key, value, "string", nil, nil, time.Now().UTC(), time.Now().UTC(),
		}},
	}
}

func (c authFakeConn) queryUser() driver.Rows {
	columns := []string{"username", "role", "status"}
	if c.data.user == nil {
		return &authFakeRows{columns: columns}
	}
	return &authFakeRows{
		columns: columns,
		rows:    [][]driver.Value{{c.data.user.username, c.data.user.role, c.data.user.status}},
	}
}

func (c authFakeConn) queryInvites() driver.Rows {
	columns := []string{
		"id", "code", "created_by", "used_by", "used_at", "expires_at",
		"cost_snapshot", "created_at", "creator_username", "creator_email",
		"creator_avatar_style", "used_username", "used_email", "used_avatar_style",
	}
	rows := make([][]driver.Value, 0, len(c.data.invites))
	for _, invite := range c.data.invites {
		rows = append(rows, []driver.Value{
			invite.id, invite.code, invite.createdBy,
			authFakeValue(invite.usedBy), authFakeValue(invite.usedAt),
			authFakeValue(invite.expiresAt), authFakeValue(invite.costSnapshot), invite.createdAt,
			authFakeValue(invite.creatorUsername), authFakeValue(invite.creatorEmail), authFakeValue(invite.creatorAvatarStyle),
			authFakeValue(invite.usedUsername), authFakeValue(invite.usedEmail), authFakeValue(invite.usedAvatarStyle),
		})
	}
	return &authFakeRows{columns: columns, rows: rows}
}

// authFakeValue dereferences the optional column values: database/sql only
// accepts nil for a NULL cell, not a typed nil pointer.
func authFakeValue(value any) driver.Value {
	switch typed := value.(type) {
	case nil:
		return nil
	case *int64:
		if typed == nil {
			return nil
		}
		return *typed
	case *string:
		if typed == nil {
			return nil
		}
		return *typed
	case *time.Time:
		if typed == nil {
			return nil
		}
		return *typed
	case []byte:
		if typed == nil {
			return nil
		}
		return typed
	default:
		return value
	}
}

func authFakeInt64(value driver.Value) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int32:
		return int64(typed), true
	case int:
		return int64(typed), true
	case float64:
		return int64(typed), true
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		return parsed, err == nil
	}
	return 0, false
}

type authFakeRows struct {
	columns []string
	rows    [][]driver.Value
	index   int
}

func (r *authFakeRows) Columns() []string { return r.columns }
func (r *authFakeRows) Close() error      { return nil }

func (r *authFakeRows) Next(dest []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.index])
	r.index++
	return nil
}

// authFakeDialector is the minimal gorm.Dialector that accepts our fake ConnPool.
type authFakeDialector struct{ conn *sql.DB }

func (d authFakeDialector) Name() string { return "authfake" }

func (d authFakeDialector) Initialize(database *gorm.DB) error {
	// Every real dialector registers the default callback chain; without it
	// gorm's Rows()/Scan() silently return nil rows. The fake only needs the
	// clause set, not a live connection.
	callbacks.RegisterDefaultCallbacks(database, &callbacks.Config{})
	database.ConnPool = d.conn
	database.Dialector = d
	return nil
}

func (d authFakeDialector) Migrator(*gorm.DB) gorm.Migrator { return nil }
func (d authFakeDialector) DataTypeOf(*schema.Field) string { return "text" }

func (d authFakeDialector) DefaultValueOf(*schema.Field) clause.Expression {
	return clause.Expr{SQL: "NULL"}
}

func (d authFakeDialector) BindVarTo(writer clause.Writer, stmt *gorm.Statement, _ interface{}) {
	_ = writer.WriteByte('$')
	_, _ = writer.WriteString(strconv.Itoa(len(stmt.Vars)))
}

func (d authFakeDialector) QuoteTo(writer clause.Writer, value string) {
	_ = writer.WriteByte('"')
	_, _ = writer.WriteString(strings.ReplaceAll(value, `"`, `""`))
	_ = writer.WriteByte('"')
}

func (d authFakeDialector) Explain(query string, _ ...interface{}) string { return query }

// ==================== HTTP helpers ====================

func authNewRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterAuth(router)
	return router
}

func authServe(router *gin.Engine, request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func authDecode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return payload
}

// ==================== route table ====================

// TestAuthRegistersAllRoutes pins the 19 handlers auth.ts declares.
func TestAuthRegistersAllRoutes(t *testing.T) {
	router := authNewRouter()

	prefix := "/api/auth"
	expected := map[string]bool{
		"POST " + prefix + "/check-2fa":                     true,
		"POST " + prefix + "/login":                         true,
		"POST " + prefix + "/send-verification-code":        true,
		"POST " + prefix + "/register":                      true,
		"GET " + prefix + "/me":                             true,
		"POST " + prefix + "/logout":                        true,
		"POST " + prefix + "/refresh":                       true,
		"POST " + prefix + "/invite":                        true,
		"GET " + prefix + "/invites":                        true,
		"DELETE " + prefix + "/invites/:id":                 true,
		"GET " + prefix + "/2fa/status":                     true,
		"POST " + prefix + "/2fa/setup":                     true,
		"POST " + prefix + "/2fa/enable":                    true,
		"POST " + prefix + "/2fa/disable":                   true,
		"GET " + prefix + "/2fa/recovery-codes":             true,
		"POST " + prefix + "/2fa/regenerate-recovery-codes": true,
		"POST " + prefix + "/forgot-password/send-code":     true,
		"POST " + prefix + "/forgot-password/reset":         true,
		"GET " + prefix + "/login-history":                  true,
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
	if len(seen) != len(expected) {
		t.Errorf("registered %d routes, want %d", len(seen), len(expected))
	}
}

// TestAuthProtectedRoutesRejectAnonymous proves every authenticated handler is
// wired to middleware.Authenticate (401, never 404).
func TestAuthProtectedRoutesRejectAnonymous(t *testing.T) {
	router := authNewRouter()

	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/auth/me"},
		{http.MethodPost, "/api/auth/invite"},
		{http.MethodGet, "/api/auth/invites"},
		{http.MethodDelete, "/api/auth/invites/1"},
		{http.MethodGet, "/api/auth/2fa/status"},
		{http.MethodPost, "/api/auth/2fa/setup"},
		{http.MethodPost, "/api/auth/2fa/enable"},
		{http.MethodPost, "/api/auth/2fa/disable"},
		{http.MethodGet, "/api/auth/2fa/recovery-codes"},
		{http.MethodPost, "/api/auth/2fa/regenerate-recovery-codes"},
		{http.MethodGet, "/api/auth/login-history"},
	}
	for _, testCase := range cases {
		request := httptest.NewRequest(testCase.method, testCase.path, nil)
		recorder := authServe(router, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", testCase.method, testCase.path, recorder.Code)
			continue
		}
		payload := authDecode(t, recorder)
		if payload["error"] != "Unauthorized" {
			t.Errorf("%s %s: error = %v, want Unauthorized", testCase.method, testCase.path, payload["error"])
		}
	}
}

// ==================== reverse test 1: Public API token on /api/auth/* ====================

// TestAuthRejectsPublicAPIToken pins Wave D spec §3: the Web JWT face must not
// accept a Public API `pat_` credential (the three auth faces never fall back to
// each other).
func TestAuthRejectsPublicAPIToken(t *testing.T) {
	t.Setenv("JWT_SECRET", "auth-test-secret")

	router := authNewRouter()
	publicAPIToken := "pat_" + strings.Repeat("A", 43) // never a real credential

	for _, path := range []string{"/api/auth/me", "/api/auth/2fa/status", "/api/auth/login-history"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+publicAPIToken)
		recorder := authServe(router, request)

		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s with a Public API token: status = %d, want 401", path, recorder.Code)
		}
		payload := authDecode(t, recorder)
		if payload["error"] != "Unauthorized" {
			t.Fatalf("%s with a Public API token: error = %v, want Unauthorized", path, payload["error"])
		}
		if _, present := payload["code"]; present {
			t.Fatalf("%s with a Public API token: unexpected code %v", path, payload["code"])
		}
	}
}

// ==================== reverse test 2: expired Web token ====================

// TestAuthRejectsExpiredWebToken pins Wave D spec §3: an expired Web JWT is
// rejected before the handler runs.
func TestAuthRejectsExpiredWebToken(t *testing.T) {
	const secret = "auth-test-secret"
	t.Setenv("JWT_SECRET", secret)

	expired := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"id":       float64(4242),
		"username": "parityuser",
		"role":     "user",
		"sid":      "expired-session",
		"iat":      time.Now().Add(-2 * time.Hour).Unix(),
		"exp":      time.Now().Add(-time.Hour).Unix(),
	})
	signed, err := expired.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign expired token: %v", err)
	}

	router := authNewRouter()
	request := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	request.Header.Set("Authorization", "Bearer "+signed)
	recorder := authServe(router, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: status = %d, want 401", recorder.Code)
	}
	payload := authDecode(t, recorder)
	if payload["error"] != "Unauthorized" {
		t.Fatalf("expired token: error = %v, want Unauthorized", payload["error"])
	}
}

// ==================== reverse test 3: invalidated Web token ====================

// TestAuthRejectsInvalidatedToken pins Wave D spec §3: once a row exists in
// token_invalidations for (user, session), the Web token is rejected with the Node
// `Session expired` envelope. The control case (same token, no row) proves the
// difference comes from the invalidation lookup and not from the token itself.
func TestAuthRejectsInvalidatedToken(t *testing.T) {
	t.Setenv("JWT_SECRET", "auth-test-secret")

	data := &authFakeDBData{}
	authInstallFakeDB(t, data)

	const userID = int32(4242)
	const sessionID = "session-under-test"
	token, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID:       userID,
		Username: "parityuser",
		Role:     "user",
		SID:      sessionID,
	})
	if err != nil {
		t.Fatalf("sign access token: %v", err)
	}

	router := authNewRouter()
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		return authServe(router, req)
	}

	// Control: without an invalidation row the request proceeds to the user lookup
	// (the fake DB has no users row) and fails as UNAUTHORIZED, not SESSION_INVALIDATED.
	control := request()
	if control.Code != http.StatusUnauthorized {
		t.Fatalf("control request: status = %d, want 401", control.Code)
	}
	if payload := authDecode(t, control); payload["code"] != "UNAUTHORIZED" {
		t.Fatalf("control request: code = %v, want UNAUTHORIZED", payload["code"])
	}

	data.addInvalidation(int64(userID), sessionID, time.Now().Unix()+120)

	invalidated := request()
	if invalidated.Code != http.StatusUnauthorized {
		t.Fatalf("invalidated token: status = %d, want 401", invalidated.Code)
	}
	payload := authDecode(t, invalidated)
	if payload["error"] != "Session expired" {
		t.Fatalf("invalidated token: error = %v, want Session expired", payload["error"])
	}
	if payload["code"] != "SESSION_INVALIDATED" {
		t.Fatalf("invalidated token: code = %v, want SESSION_INVALIDATED", payload["code"])
	}
}

// TestAuthRejectsUserLevelInvalidation covers the "__USER_LEVEL__" marker, which
// invalidates every session of a user (bans, role changes, password reset).
func TestAuthRejectsUserLevelInvalidation(t *testing.T) {
	t.Setenv("JWT_SECRET", "auth-test-secret")

	data := &authFakeDBData{}
	authInstallFakeDB(t, data)

	const userID = int32(4243)
	token, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID:       userID,
		Username: "parityuser",
		Role:     "user",
		SID:      "some-session",
	})
	if err != nil {
		t.Fatalf("sign access token: %v", err)
	}
	data.addInvalidation(int64(userID), lib.UserLevelSessionID, time.Now().Unix()+120)

	router := authNewRouter()
	request := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := authServe(router, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("user-level invalidation: status = %d, want 401", recorder.Code)
	}
	if payload := authDecode(t, recorder); payload["code"] != "SESSION_INVALIDATED" {
		t.Fatalf("user-level invalidation: code = %v, want SESSION_INVALIDATED", payload["code"])
	}
}

// ==================== reverse test 4: Turnstile failure ====================

// TestAuthTurnstileFailureReturns403 pins Wave D spec §3: a failing Turnstile
// challenge on /login answers 403 with the literal envelope of
// lib/turnstile.ts (not an apiError, so the code is TURNSTILE_VERIFICATION_FAILED).
// Turnstile must be enabled and fully configured for the verifier to run; an
// invalid token always fails whether siteverify is reachable or not.
func TestAuthTurnstileFailureReturns403(t *testing.T) {
	t.Setenv("JWT_SECRET", "auth-test-secret")

	authInstallFakeDB(t, &authFakeDBData{config: map[string]string{
		"turnstile_enabled":    "true",
		"turnstile_site_key":   "fake-site-key",
		"turnstile_secret_key": "fake-secret-key",
	}})

	router := authNewRouter()
	body := `{"username":"somebody","password":"Password1","turnstileToken":"definitely-invalid"}`

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	recorder := authServe(router, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("turnstile failure: status = %d, want 403 (body %s)", recorder.Code, recorder.Body.String())
	}
	payload := authDecode(t, recorder)
	if payload["error"] != "Turnstile verification failed" {
		t.Fatalf("turnstile failure: error = %v", payload["error"])
	}
	if payload["code"] != lib.TurnstileErrorVerificationFailed {
		t.Fatalf("turnstile failure: code = %v, want %s", payload["code"], lib.TurnstileErrorVerificationFailed)
	}
}

// ==================== body parser ====================

// TestAuthBodyParserMessages pins the Fastify parser contract for public routes:
// empty JSON → 400, malformed JSON → 400, wrong content type → 415.
func TestAuthBodyParserMessages(t *testing.T) {
	router := authNewRouter()

	cases := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantError   string
	}{
		{
			name:        "empty json body",
			body:        "",
			contentType: "application/json",
			wantStatus:  http.StatusBadRequest,
			wantError:   "Body cannot be empty when content-type is set to 'application/json'",
		},
		{
			name:        "malformed json body",
			body:        "{not json",
			contentType: "application/json",
			wantStatus:  http.StatusBadRequest,
			wantError:   "Body is not valid JSON but content-type is set to 'application/json'",
		},
		{
			name:        "unsupported media type",
			body:        `{"username":"abc"}`,
			contentType: "application/xml",
			wantStatus:  http.StatusUnsupportedMediaType,
			wantError:   "Unsupported Media Type: application/xml",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/auth/check-2fa", strings.NewReader(testCase.body))
			request.Header.Set("Content-Type", testCase.contentType)
			recorder := authServe(router, request)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, testCase.wantStatus)
			}
			if payload := authDecode(t, recorder); payload["error"] != testCase.wantError {
				t.Fatalf("error = %v, want %s", payload["error"], testCase.wantError)
			}
		})
	}
}

// TestAuthCheck2FARequiresObjectBody pins AJV's `type: object` root check for an
// absent body: Fastify validates `undefined` and reports "must be object".
func TestAuthCheck2FARequiresObjectBody(t *testing.T) {
	router := authNewRouter()
	request := httptest.NewRequest(http.MethodPost, "/api/auth/check-2fa", nil)
	recorder := authServe(router, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	payload := authDecode(t, recorder)
	if payload["error"] != "must be object" {
		t.Fatalf("error = %v, want must be object", payload["error"])
	}
	if payload["code"] != "VALIDATION_ERROR" {
		t.Fatalf("code = %v, want VALIDATION_ERROR", payload["code"])
	}
}

// ==================== AJV message table ====================

// TestAuthValidationMessages pins the setErrorHandler messages for the auth
// schemas, including the username/password/pattern special cases and the
// required-before-properties ordering.
func TestAuthValidationMessages(t *testing.T) {
	cases := []struct {
		name     string
		decoded  any
		required []string
		fields   []authFieldSpec
		want     string
	}{
		{
			name:     "login missing username",
			decoded:  map[string]any{},
			required: []string{"username", "password"},
			fields:   authLoginFields,
			want:     "username is required",
		},
		{
			name:     "login missing password",
			decoded:  map[string]any{"username": "abc"},
			required: []string{"username", "password"},
			fields:   authLoginFields,
			want:     "password is required",
		},
		{
			name:     "login username too short",
			decoded:  map[string]any{"username": "ab", "password": "Password1"},
			required: []string{"username", "password"},
			fields:   authLoginFields,
			want:     "Username must be at least 3 characters",
		},
		{
			name:     "login password too short",
			decoded:  map[string]any{"username": "abc", "password": "short"},
			required: []string{"username", "password"},
			fields:   authLoginFields,
			want:     "Password must be at least 8 characters",
		},
		{
			name:     "login totp too short",
			decoded:  map[string]any{"username": "abc", "password": "Password1", "totpCode": "12345"},
			required: []string{"username", "password"},
			fields:   authLoginFields,
			want:     "totpCode is too short",
		},
		{
			name:     "login recovery code too short",
			decoded:  map[string]any{"username": "abc", "password": "Password1", "recoveryCode": "short"},
			required: []string{"username", "password"},
			fields:   authLoginFields,
			want:     "recoveryCode is too short",
		},
		{
			name:     "register username pattern",
			decoded:  map[string]any{"username": "1abc", "password": "Password1", "email": "a@b.com"},
			required: []string{"username", "password", "email"},
			fields:   authRegisterFields,
			want:     "Username must start with a letter and contain only letters, numbers, underscores, and hyphens",
		},
		{
			name:     "register email format",
			decoded:  map[string]any{"username": "abc", "password": "Password1", "email": "not-an-email"},
			required: []string{"username", "password", "email"},
			fields:   authRegisterFields,
			want:     "Please enter a valid email address",
		},
		{
			name:     "invite count below minimum",
			decoded:  map[string]any{"count": float64(0)},
			required: nil,
			fields:   authInviteCreateFields,
			want:     "must be >= 1",
		},
		{
			name:     "invite count above maximum",
			decoded:  map[string]any{"count": float64(101)},
			required: nil,
			fields:   authInviteCreateFields,
			want:     "must be <= 100",
		},
		{
			name:     "invite count not an integer",
			decoded:  map[string]any{"count": 1.5},
			required: nil,
			fields:   authInviteCreateFields,
			want:     "must be integer",
		},
		{
			name:     "2fa enable code too short",
			decoded:  map[string]any{"code": "12345"},
			required: []string{"code"},
			fields:   authTwoFACodeFields,
			want:     "code is too short",
		},
		{
			name:     "missing body is not an object",
			decoded:  nil,
			required: []string{"username"},
			fields:   authUsernameLoginFields,
			want:     "must be object",
		},
		{
			name:     "array body is not an object",
			decoded:  []any{"username"},
			required: []string{"username"},
			fields:   authUsernameLoginFields,
			want:     "must be object",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, got := authValidateBody(testCase.decoded, testCase.required, testCase.fields); got != testCase.want {
				t.Fatalf("message = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestAuthValidationCoercion pins coerceTypes:'array' behaviour that the login
// schema relies on (single-element arrays unwrap, scalars stringify).
func TestAuthValidationCoercion(t *testing.T) {
	values, message := authValidateBody(
		map[string]any{"username": []any{"abc"}, "password": float64(12345678)},
		[]string{"username", "password"},
		authLoginFields,
	)
	if message != "" {
		t.Fatalf("unexpected validation message %q", message)
	}
	if values["username"] != "abc" {
		t.Fatalf("username = %v, want abc", values["username"])
	}
	if values["password"] != "12345678" {
		t.Fatalf("password = %v, want 12345678", values["password"])
	}
}

// ==================== cookie-less public paths ====================

// TestAuthRefreshWithoutCookie pins the only /refresh response that needs no
// session: 401 REFRESH_TOKEN_MISSING.
func TestAuthRefreshWithoutCookie(t *testing.T) {
	router := authNewRouter()
	request := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	recorder := authServe(router, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	payload := authDecode(t, recorder)
	if payload["error"] != "Refresh token missing" {
		t.Fatalf("error = %v", payload["error"])
	}
	if payload["code"] != "REFRESH_TOKEN_MISSING" {
		t.Fatalf("code = %v, want REFRESH_TOKEN_MISSING", payload["code"])
	}
}

// TestAuthLogoutWithoutCredentials pins that logout succeeds even with no access
// token and no refresh cookie (it must still be able to clear the cookie).
func TestAuthLogoutWithoutCredentials(t *testing.T) {
	router := authNewRouter()
	request := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	recorder := authServe(router, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if payload := authDecode(t, recorder); payload["message"] != "Logged out" {
		t.Fatalf("message = %v, want Logged out", payload["message"])
	}
}

// TestAuthInvitesListProjection pins the admin listing wire shape: creator /
// used-by fallbacks (`value || null`), ISO timestamps, the raw jsonb cost snapshot
// and the pagination envelope.
func TestAuthInvitesListProjection(t *testing.T) {
	t.Setenv("JWT_SECRET", "auth-test-secret")

	timestamp := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	createdAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	usedBy := int64(9)
	creatorEmail := "admin@example.com"
	creatorAvatar := "rings"
	usedEmail := "buyer@example.com"
	emptyAvatar := ""

	data := &authFakeDBData{
		user: &authFakeUser{username: "admin", role: "admin", status: "active"},
		invites: []authFakeInvite{{
			id:                 7,
			code:               "abcd1234",
			createdBy:          1,
			usedBy:             &usedBy,
			usedAt:             &timestamp,
			expiresAt:          &timestamp,
			costSnapshot:       []byte(`{"resource":"balance","amount":1.5}`),
			createdAt:          createdAt,
			creatorUsername:    authTestStringPointer("admin"),
			creatorEmail:       &creatorEmail,
			creatorAvatarStyle: &creatorAvatar,
			usedUsername:       authTestStringPointer("buyer"),
			usedEmail:          &usedEmail,
			usedAvatarStyle:    &emptyAvatar,
		}},
	}
	authInstallFakeDB(t, data)

	token, err := lib.SignAccessToken(lib.AccessTokenClaims{ID: 1, Username: "admin", Role: "admin", SID: "session"})
	if err != nil {
		t.Fatalf("sign access token: %v", err)
	}

	router := authNewRouter()
	request := httptest.NewRequest(http.MethodGet, "/api/auth/invites?page=1&pageSize=20", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := authServe(router, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	payload := authDecode(t, recorder)
	if payload["total"] != float64(1) || payload["page"] != float64(1) ||
		payload["pageSize"] != float64(20) || payload["totalPages"] != float64(1) {
		t.Fatalf("pagination envelope = %v", payload)
	}

	invites, ok := payload["invites"].([]any)
	if !ok || len(invites) != 1 {
		t.Fatalf("invites = %v", payload["invites"])
	}
	item, _ := invites[0].(map[string]any)

	expected := map[string]any{
		"id":                   float64(7),
		"code":                 "abcd1234",
		"createdBy":            float64(1),
		"createdByUsername":    "admin",
		"createdByEmail":       "admin@example.com",
		"createdByAvatarStyle": "rings",
		"usedBy":               float64(9),
		"usedByUsername":       "buyer",
		"usedByEmail":          "buyer@example.com",
		// `usedByAvatarStyle: i.usedByAvatarStyle || null` collapses "" to null.
		"usedByAvatarStyle": nil,
		"usedAt":            "2026-02-03T04:05:06.000Z",
		"expiresAt":         "2026-02-03T04:05:06.000Z",
		"createdAt":         "2026-01-02T03:04:05.000Z",
	}
	for key, want := range expected {
		if got := item[key]; got != want {
			t.Fatalf("%s = %v, want %v", key, got, want)
		}
	}
	snapshot, ok := item["costSnapshot"].(map[string]any)
	if !ok || snapshot["resource"] != "balance" || snapshot["amount"] != 1.5 {
		t.Fatalf("costSnapshot = %v", item["costSnapshot"])
	}
}

// TestAuthInviteDeleteRejectsOutsideInt32 pins the id-range guard: Prisma's Int is
// 32-bit, so an id above int32 max must fail exactly like the Node findUnique
// throw instead of wrapping onto a different invite code.
func TestAuthInviteDeleteRejectsOutsideInt32(t *testing.T) {
	t.Setenv("JWT_SECRET", "auth-test-secret")

	authInstallFakeDB(t, &authFakeDBData{
		user: &authFakeUser{username: "admin", role: "admin", status: "active"},
	})

	token, err := lib.SignAccessToken(lib.AccessTokenClaims{ID: 1, Username: "admin", Role: "admin", SID: "session"})
	if err != nil {
		t.Fatalf("sign access token: %v", err)
	}

	router := authNewRouter()
	request := httptest.NewRequest(http.MethodDelete, "/api/auth/invites/4294967297", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := authServe(router, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", recorder.Code, recorder.Body.String())
	}
}

// authTestStringPointer returns a pointer to value.
func authTestStringPointer(value string) *string { return &value }

// ==================== helpers ====================

// TestAuthNanoID pins the alphabet and length of the invite-code generator.
func TestAuthNanoID(t *testing.T) {
	for iteration := 0; iteration < 32; iteration++ {
		code, err := authNanoID(12)
		if err != nil {
			t.Fatalf("authNanoID: %v", err)
		}
		if len(code) != 12 {
			t.Fatalf("len = %d, want 12", len(code))
		}
		for _, character := range code {
			if !strings.ContainsRune(authInviteCodeAlphabet, character) {
				t.Fatalf("code %q contains %q outside the nanoid alphabet", code, character)
			}
		}
	}
}

// TestAuthSameSiteMapping pins the cookie_config → net/http translation used by
// the refresh cookie (the attributes themselves come from lib/cookie_config.go).
func TestAuthSameSiteMapping(t *testing.T) {
	cases := map[string]http.SameSite{
		"strict": http.SameSiteStrictMode,
		"lax":    http.SameSiteLaxMode,
		"none":   http.SameSiteNoneMode,
		"":       http.SameSiteDefaultMode,
	}
	for value, want := range cases {
		if got := authSameSiteMode(value); got != want {
			t.Fatalf("authSameSiteMode(%q) = %v, want %v", value, got, want)
		}
	}
}

// TestAuthLocalIP mirrors the isLocalIp filter used before a GeoIP lookup: local
// and private addresses must never leave the process.
func TestAuthLocalIP(t *testing.T) {
	local := []string{"127.0.0.1", "192.168.1.10", "10.0.0.5", "172.16.0.1", "172.31.255.1", "::1", "fe80::1", "fd00::1"}
	for _, address := range local {
		if !authIsLocalIP(address) {
			t.Errorf("%s must be local", address)
		}
	}
	public := []string{"8.8.8.8", "172.15.0.1", "172.32.0.1", "1.1.1.1"}
	for _, address := range public {
		if authIsLocalIP(address) {
			t.Errorf("%s must not be local", address)
		}
	}
}

// TestAuthDemoRedaction pins shouldRedactDemoLoginRecords for a demo host/user.
func TestAuthDemoRedaction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/auth/login-history", nil)
	context.Request.Host = "demo.payincus.com"

	if !authShouldRedactDemoLoginClaims(context, &lib.AccessTokenClaims{Username: "admin"}) {
		t.Fatal("demo host + demo username must redact login records")
	}
	if authShouldRedactDemoLoginClaims(context, &lib.AccessTokenClaims{Username: "parityuser"}) {
		t.Fatal("non-demo username must not redact login records")
	}

	context.Request.Host = "panel.payincus.com"
	if authShouldRedactDemoLoginClaims(context, &lib.AccessTokenClaims{Username: "admin"}) {
		t.Fatal("non-demo host must not redact login records")
	}
}

// TestAuthQuotaPayloadShape pins the /me quota JSON keys.
func TestAuthQuotaPayloadShape(t *testing.T) {
	encoded, err := json.Marshal(authQuotaPayload{
		HostLimit: 1, HostUsed: 2, FriendLimit: 3, FriendUsed: 4, PackageLimit: 5, PackageUsed: 6,
	})
	if err != nil {
		t.Fatalf("marshal quota: %v", err)
	}
	for _, key := range []string{"hostLimit", "hostUsed", "friendLimit", "friendUsed", "packageLimit", "packageUsed"} {
		if !strings.Contains(string(encoded), `"`+key+`"`) {
			t.Fatalf("quota payload missing %s: %s", key, encoded)
		}
	}
}

// Ensure the models import stays referenced if the route table changes.
var _ = models.OperationTypeDisable2fa
