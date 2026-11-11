// Route-level tests for src/routes/admin_incus_ui.go.
//
// What is covered here (the parts a conformance run cannot reach, because the
// parity harness speaks plain HTTP and has no node agent):
//
//  1. pre-upgrade authentication for the ticket endpoint, the proxy family and the
//     WebSocket route (401/403/400/404/503, and never a 101 for a bad caller);
//  2. ticket semantics at the route level: single issuance, 256 uses, host binding
//     that does not burn the ticket, and the expiry rule (lib/action_ticket_test.go
//     owns the clock-driven expiry case);
//  3. the HTTP and WebSocket relay against a local fake upstream: bidirectional
//     traffic, the 128-message pre-handshake buffer, close-code propagation in both
//     directions (including the empty close frame that a browser reports as 1005)
//     and the per-user connection slot release;
//  4. the pure helpers (path normalization, query sanitization, subprotocol ticket
//     extraction, sendable close codes).
package routes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== fixtures ====================

const (
	adminIncusUITestAdminID   = int32(7)
	adminIncusUITestUserID    = int32(8)
	adminIncusUITestHostID    = int32(995001)
	adminIncusUITestHostName  = "parity-incus-ui-host"
	adminIncusUITestSessionID = "session-incus-ui"
)

func adminIncusUITestUserRecord(id int32, role models.UserRole) *db.UserRecord {
	return &db.UserRecord{
		ID:       id,
		Username: fmt.Sprintf("user-%d", id),
		Role:     role,
		Status:   models.UserStatusActive,
	}
}

func adminIncusUITestHostRecord(id int32) *db.HostsRecord {
	return &db.HostsRecord{
		ID:         id,
		Name:       adminIncusUITestHostName,
		TargetHost: "127.0.0.1",
		TargetPort: 8443,
	}
}

// adminIncusUITestEnv holds the seam overrides shared by the route tests.
type adminIncusUITestEnv struct {
	hosts         map[int32]*db.HostsRecord
	users         map[int32]*db.UserRecord
	tunnelUp      bool
	dispatcher    *adminIncusUITunnelDispatcher
	dispatcherErr error
	dial          func(ctx context.Context, dispatcher *adminIncusUITunnelDispatcher, targetPath string, subprotocols []string) (*lib.WebSocket, error)
}

// install swaps the production seams for the duration of the test.
func (env *adminIncusUITestEnv) install(t *testing.T) {
	t.Helper()

	previousFindHost := adminIncusUIFindHost
	previousFindUser := adminIncusUIFindUser
	previousOnline := adminIncusUITunnelOnline
	previousDispatcher := adminIncusUIDispatcherForHost
	previousDial := adminIncusUIDialWebSocketUpstream

	adminIncusUIFindHost = func(_ context.Context, id int32) (*db.HostsRecord, error) {
		return env.hosts[id], nil
	}
	adminIncusUIFindUser = func(_ context.Context, id int32) (*db.UserRecord, error) {
		return env.users[id], nil
	}
	adminIncusUITunnelOnline = func(hostID int32) bool {
		if env.hosts[hostID] == nil {
			return false
		}
		return env.tunnelUp
	}
	adminIncusUIDispatcherForHost = func(*db.HostsRecord) (*adminIncusUITunnelDispatcher, error) {
		if env.dispatcherErr != nil {
			return nil, env.dispatcherErr
		}
		if env.dispatcher != nil {
			return env.dispatcher, nil
		}
		return &adminIncusUITunnelDispatcher{}, nil
	}
	if env.dial != nil {
		adminIncusUIDialWebSocketUpstream = env.dial
	} else {
		adminIncusUIDialWebSocketUpstream = func(context.Context, *adminIncusUITunnelDispatcher, string, []string) (*lib.WebSocket, error) {
			return nil, fmt.Errorf("no fake upstream configured")
		}
	}

	t.Cleanup(func() {
		adminIncusUIFindHost = previousFindHost
		adminIncusUIFindUser = previousFindUser
		adminIncusUITunnelOnline = previousOnline
		adminIncusUIDispatcherForHost = previousDispatcher
		adminIncusUIDialWebSocketUpstream = previousDial
	})
}

func adminIncusUITestEnvDefault() *adminIncusUITestEnv {
	return &adminIncusUITestEnv{
		hosts: map[int32]*db.HostsRecord{adminIncusUITestHostID: adminIncusUITestHostRecord(adminIncusUITestHostID)},
		users: map[int32]*db.UserRecord{
			adminIncusUITestAdminID: adminIncusUITestUserRecord(adminIncusUITestAdminID, models.UserRoleAdmin),
			adminIncusUITestUserID:  adminIncusUITestUserRecord(adminIncusUITestUserID, models.UserRoleUser),
		},
		tunnelUp: true,
	}
}

// adminIncusUIAdminToken mints a real HS256 access token for the given user.
func adminIncusUIAdminToken(t *testing.T, userID int32, role string, sessionID string) string {
	t.Helper()
	token, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID:       userID,
		Username: fmt.Sprintf("user-%d", userID),
		Role:     role,
		Status:   "active",
		SID:      sessionID,
	})
	if err != nil {
		t.Fatalf("sign access token: %v", err)
	}
	return token
}

// adminIncusUIEngine mounts the module exactly the way RegisterAdminIncusUI does.
func adminIncusUIEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterAdminIncusUI(engine)
	return engine
}

// adminIncusUITicketEngine mounts only the ticket handler behind a stubbed
// authenticateAdmin (the middleware chain needs a live users table; its
// unauthenticated branches are exercised through adminIncusUIEngine instead).
func adminIncusUITicketEngine(claims *lib.AccessTokenClaims) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/api/admin/hosts/:hostId/incus-ui-ticket", func(c *gin.Context) {
		if claims != nil {
			c.Set(middleware.ContextUserKey, claims)
		}
		c.Next()
	}, adminIncusUITicketHandler)
	return engine
}

func adminIncusUIPerformRequest(t *testing.T, engine *gin.Engine, method, path string, headers map[string]string, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	decoded := map[string]any{}
	if recorder.Body.Len() > 0 {
		_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	}
	return recorder, decoded
}

func adminIncusUIExpectJSON(t *testing.T, label string, recorder *httptest.ResponseRecorder, decoded map[string]any, status int, fields map[string]any) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("%s: status = %d, want %d (body %q)", label, recorder.Code, status, recorder.Body.String())
	}
	for key, want := range fields {
		if got := decoded[key]; got != want {
			t.Fatalf("%s: %s = %#v, want %#v (body %q)", label, key, got, want, recorder.Body.String())
		}
	}
}

// ==================== ticket endpoint ====================

func TestAdminIncusUITicketRequiresAuthentication(t *testing.T) {
	engine := adminIncusUIEngine()

	// No credentials at all: the shared authenticateAdmin decorator answers before
	// the handler sees the host id (same as Fastify's onRequest).
	recorder, decoded := adminIncusUIPerformRequest(t, engine, http.MethodPost,
		"/api/admin/hosts/0/incus-ui-ticket", nil, "")
	adminIncusUIExpectJSON(t, "anonymous", recorder, decoded, 401, map[string]any{"error": "Unauthorized"})

	// A syntactically invalid token never reaches the database.
	recorder, decoded = adminIncusUIPerformRequest(t, engine, http.MethodPost,
		"/api/admin/hosts/0/incus-ui-ticket", map[string]string{"Authorization": "Bearer not-a-jwt"}, "")
	adminIncusUIExpectJSON(t, "invalid token", recorder, decoded, 401, map[string]any{"error": "Unauthorized"})

	// Non-Bearer schemes are "no credentials".
	recorder, decoded = adminIncusUIPerformRequest(t, engine, http.MethodPost,
		"/api/admin/hosts/0/incus-ui-ticket", map[string]string{"Authorization": "Basic abc"}, "")
	adminIncusUIExpectJSON(t, "basic auth", recorder, decoded, 401, map[string]any{"error": "Unauthorized"})
}

func TestAdminIncusUITicketHandler(t *testing.T) {
	env := adminIncusUITestEnvDefault()
	env.install(t)

	adminClaims := func() *lib.AccessTokenClaims {
		return &lib.AccessTokenClaims{
			ID:       adminIncusUITestAdminID,
			Username: "admin",
			Role:     "admin",
			Status:   "active",
			SID:      adminIncusUITestSessionID,
			IssuedAt: time.Now().UTC().Unix(),
		}
	}

	cases := []struct {
		name     string
		claims   *lib.AccessTokenClaims
		path     string
		tunnelUp bool
		status   int
		fields   map[string]any
	}{
		{
			name:   "non-admin",
			claims: &lib.AccessTokenClaims{ID: adminIncusUITestUserID, Role: "user", Status: "active", IssuedAt: time.Now().UTC().Unix()},
			path:   "/api/admin/hosts/995001/incus-ui-ticket",
			status: 403,
			fields: map[string]any{"error": "Admin privileges required", "code": "ADMIN_REQUIRED"},
		},
		{
			name:   "missing-token-iat",
			claims: &lib.AccessTokenClaims{ID: adminIncusUITestAdminID, Role: "admin", Status: "active"},
			path:   "/api/admin/hosts/995001/incus-ui-ticket",
			status: 401,
			fields: map[string]any{"error": "Unauthorized", "code": "UNAUTHORIZED"},
		},
		{
			name:   "invalid-host-id-zero",
			claims: adminClaims(),
			path:   "/api/admin/hosts/0/incus-ui-ticket",
			status: 400,
			fields: map[string]any{"error": "Invalid host ID", "code": "INVALID_HOST_ID"},
		},
		{
			name:   "invalid-host-id-leading-zero",
			claims: adminClaims(),
			path:   "/api/admin/hosts/01/incus-ui-ticket",
			status: 400,
			fields: map[string]any{"error": "Invalid host ID", "code": "INVALID_HOST_ID"},
		},
		{
			name:   "invalid-host-id-text",
			claims: adminClaims(),
			path:   "/api/admin/hosts/abc/incus-ui-ticket",
			status: 400,
			fields: map[string]any{"error": "Invalid host ID", "code": "INVALID_HOST_ID"},
		},
		{
			name:   "host-not-found",
			claims: adminClaims(),
			path:   "/api/admin/hosts/999999/incus-ui-ticket",
			status: 404,
			fields: map[string]any{"error": "Host not found", "code": "HOST_NOT_FOUND"},
		},
		{
			name:     "tunnel-offline",
			claims:   adminClaims(),
			path:     "/api/admin/hosts/995001/incus-ui-ticket",
			tunnelUp: false,
			status:   503,
			fields:   map[string]any{"error": "宿主机反向隧道未连接（Agent 离线），无法访问 Incus UI", "code": "TUNNEL_OFFLINE"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env.tunnelUp = testCase.tunnelUp
			if testCase.tunnelUp == false {
				defer func() { env.tunnelUp = true }()
			}
			engine := adminIncusUITicketEngine(testCase.claims)
			recorder, decoded := adminIncusUIPerformRequest(t, engine, http.MethodPost, testCase.path, nil, "")
			adminIncusUIExpectJSON(t, testCase.name, recorder, decoded, testCase.status, testCase.fields)
		})
	}

	t.Run("success", func(t *testing.T) {
		claims := adminClaims()
		engine := adminIncusUITicketEngine(claims)
		recorder, decoded := adminIncusUIPerformRequest(t, engine, http.MethodPost,
			"/api/admin/hosts/995001/incus-ui-ticket", nil, "")

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
		ticket, ok := decoded["ticket"].(string)
		if !ok || ticket == "" {
			t.Fatalf("ticket missing: %#v", decoded)
		}
		if got := decoded["expiresIn"]; got != float64(300) {
			t.Fatalf("expiresIn = %#v, want 300", got)
		}

		// The ticket is bound to the issuing user, host, token iat and session.
		hostID := adminIncusUITestHostID
		verified := lib.VerifyIncusUIAccessTicket(ticket, &hostID)
		if !verified.Valid || verified.UserID != adminIncusUITestAdminID || verified.HostID != hostID {
			t.Fatalf("issued ticket does not verify: %+v", verified)
		}
		if verified.IssuedAt != claims.IssuedAt {
			t.Fatalf("issuedAt = %d, want %d", verified.IssuedAt, claims.IssuedAt)
		}
		if verified.SessionID == nil || *verified.SessionID != adminIncusUITestSessionID {
			t.Fatalf("session binding = %v", verified.SessionID)
		}

		// A different host must not accept it (and must not burn it).
		otherHost := int32(995002)
		if mismatch := lib.VerifyIncusUIAccessTicket(ticket, &otherHost); mismatch.Valid {
			t.Fatalf("ticket accepted for another host: %+v", mismatch)
		}
		if again := lib.VerifyIncusUIAccessTicket(ticket, &hostID); !again.Valid {
			t.Fatalf("host mismatch burned the ticket: %+v", again)
		}
	})
}

// ==================== proxy auth ====================

func TestAdminIncusUIProxyTicketAuth(t *testing.T) {
	env := adminIncusUITestEnvDefault()
	env.install(t)
	engine := adminIncusUIEngine()
	hostID := int32(adminIncusUITestHostID)
	proxyPath := fmt.Sprintf("/api/admin/hosts/%d/incus-proxy/1.0/info", hostID)

	ticketFor := func(host int32, userID int32, issuedAt int64) string {
		return lib.GenerateIncusUIAccessTicket(userID, host, issuedAt, adminIncusUISessionIDPtr(adminIncusUITestSessionID))
	}

	t.Run("anonymous", func(t *testing.T) {
		recorder, decoded := adminIncusUIPerformRequest(t, engine, http.MethodGet, proxyPath, nil, "")
		adminIncusUIExpectJSON(t, "anonymous", recorder, decoded, 401, map[string]any{"error": "Unauthorized"})
	})

	t.Run("non-admin-bearer", func(t *testing.T) {
		token := adminIncusUIAdminToken(t, adminIncusUITestUserID, "user", "sess")
		recorder, decoded := adminIncusUIPerformRequest(t, engine, http.MethodGet, proxyPath,
			map[string]string{"Authorization": "Bearer " + token}, "")
		adminIncusUIExpectJSON(t, "non-admin", recorder, decoded, 403,
			map[string]any{"error": "Admin privileges required", "code": "ADMIN_REQUIRED"})
	})

	t.Run("invalid-ticket", func(t *testing.T) {
		recorder, decoded := adminIncusUIPerformRequest(t, engine, http.MethodGet,
			proxyPath+"?__incudal_ticket=deadbeef", nil, "")
		adminIncusUIExpectJSON(t, "invalid ticket", recorder, decoded, 401,
			map[string]any{"error": "Invalid or expired Incus UI ticket", "code": "UNAUTHORIZED"})
	})

	t.Run("host-mismatch", func(t *testing.T) {
		otherHostTicket := ticketFor(hostID+1, adminIncusUITestAdminID, time.Now().UTC().Unix())
		recorder, decoded := adminIncusUIPerformRequest(t, engine, http.MethodGet,
			proxyPath+"?__incudal_ticket="+otherHostTicket, nil, "")
		adminIncusUIExpectJSON(t, "host mismatch", recorder, decoded, 401,
			map[string]any{"error": "Invalid or expired Incus UI ticket", "code": "UNAUTHORIZED"})

		// Not burned: the same ticket still works for its own host, which is the
		// TypeScript's "a host mismatch does not consume" rule.
		env.tunnelUp = false
		ownTicket := ticketFor(hostID, adminIncusUITestAdminID, time.Now().UTC().Unix())
		recorder, decoded = adminIncusUIPerformRequest(t, engine, http.MethodGet,
			proxyPath+"?__incudal_ticket="+ownTicket, nil, "")
		adminIncusUIExpectJSON(t, "own host", recorder, decoded, 503,
			map[string]any{"code": "TUNNEL_OFFLINE"})
	})

	t.Run("host-id-before-ticket", func(t *testing.T) {
		ticket := ticketFor(hostID, adminIncusUITestAdminID, time.Now().UTC().Unix())
		recorder, decoded := adminIncusUIPerformRequest(t, engine, http.MethodGet,
			"/api/admin/hosts/0/incus-proxy/1.0/info?__incudal_ticket="+ticket, nil, "")
		adminIncusUIExpectJSON(t, "invalid host id", recorder, decoded, 400,
			map[string]any{"error": "Invalid host ID", "code": "INVALID_HOST_ID"})
	})

	t.Run("host-not-found", func(t *testing.T) {
		// The ticket is bound to the path host, so a missing host is only reached with
		// a ticket that was issued for that same id.
		ticket := ticketFor(999999, adminIncusUITestAdminID, time.Now().UTC().Unix())
		recorder, decoded := adminIncusUIPerformRequest(t, engine, http.MethodGet,
			"/api/admin/hosts/999999/incus-proxy/1.0/info?__incudal_ticket="+ticket, nil, "")
		adminIncusUIExpectJSON(t, "host not found", recorder, decoded, 404,
			map[string]any{"error": "Host not found", "code": "HOST_NOT_FOUND"})
	})

	t.Run("mutations-do-not-accept-tickets", func(t *testing.T) {
		ticket := ticketFor(hostID, adminIncusUITestAdminID, time.Now().UTC().Unix())
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			recorder, decoded := adminIncusUIPerformRequest(t, engine, method,
				proxyPath+"?__incudal_ticket="+ticket, nil, `{"name":"x"}`)
			adminIncusUIExpectJSON(t, method+" with ticket", recorder, decoded, 401,
				map[string]any{"error": "Unauthorized"})
		}
	})

	t.Run("ticket-exhausts-after-256-uses", func(t *testing.T) {
		env.tunnelUp = false
		defer func() { env.tunnelUp = true }()
		ticket := ticketFor(hostID, adminIncusUITestAdminID, time.Now().UTC().Unix())

		// Every request consumes one use and stops at the tunnel check.
		for attempt := 0; attempt < 256; attempt++ {
			recorder, decoded := adminIncusUIPerformRequest(t, engine, http.MethodGet,
				proxyPath+"?__incudal_ticket="+ticket, nil, "")
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("use %d: status = %d, want 503 (body %q)", attempt+1, recorder.Code, recorder.Body.String())
			}
			if decoded["code"] != "TUNNEL_OFFLINE" {
				t.Fatalf("use %d: body %q", attempt+1, recorder.Body.String())
			}
		}
		recorder, decoded := adminIncusUIPerformRequest(t, engine, http.MethodGet,
			proxyPath+"?__incudal_ticket="+ticket, nil, "")
		adminIncusUIExpectJSON(t, "257th use", recorder, decoded, 401,
			map[string]any{"error": "Invalid or expired Incus UI ticket", "code": "UNAUTHORIZED"})
	})
}

// ==================== pure helpers ====================

func TestAdminIncusUINormalizeProxyTargetPath(t *testing.T) {
	cases := []struct {
		name      string
		wildcard  string
		rawSearch string
		want      string
	}{
		{name: "root-api", wildcard: "/1.0", want: "/1.0"},
		{name: "no-leading-slash", wildcard: "1.0", want: "/1.0"},
		{name: "nested", wildcard: "1.0/instances", want: "/1.0/instances"},
		{name: "leading-slashes", wildcard: "///1.0/info", want: "/1.0/info"},
		{name: "collapse-double-slash", wildcard: "1.0//instances//a", want: "/1.0/instances/a"},
		{name: "empty", wildcard: "", want: ""},
		{name: "slash-only", wildcard: "/", want: ""},
		{name: "wrong-version", wildcard: "2.0/info", want: ""},
		{name: "not-api-prefix", wildcard: "1.00/info", want: ""},
		{name: "traversal-plain", wildcard: "1.0/../secret", want: ""},
		{name: "traversal-encoded", wildcard: "1.0/%2e%2e/secret", want: ""},
		{name: "backslash", wildcard: `1.0\secret`, want: ""},
		{name: "encoded-backslash", wildcard: "1.0/%5csecret", want: ""},
		{name: "encoded-nul", wildcard: "1.0/info%00", want: ""},
		{name: "bad-escape", wildcard: "1.0/%zz", want: ""},
		{name: "private-extension", wildcard: "1.0/payincus", want: ""},
		{name: "private-extension-child", wildcard: "1.0/payincus/status", want: ""},
		{name: "private-extension-lookalike", wildcard: "1.0/payincusx", want: "/1.0/payincusx"},
		{name: "query-kept", wildcard: "1.0/info", rawSearch: "?recursion=1", want: "/1.0/info?recursion=1"},
		{name: "ticket-stripped", wildcard: "1.0/instances", rawSearch: "?__incudal_ticket=abc&project=default", want: "/1.0/instances?project=default"},
		{name: "ticket-query-stripped", wildcard: "1.0/instances", rawSearch: "?ticket=abc", want: "/1.0/instances"},
		{name: "empty-query", wildcard: "1.0/info", rawSearch: "?", want: "/1.0/info"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := adminIncusUINormalizeProxyTargetPath(testCase.wildcard, testCase.rawSearch); got != testCase.want {
				t.Fatalf("normalize(%q, %q) = %q, want %q", testCase.wildcard, testCase.rawSearch, got, testCase.want)
			}
		})
	}
}

func TestAdminIncusUIExtractTicketFromSubprotocols(t *testing.T) {
	longTicket := strings.Repeat("a", incusUIMaxTicketSubprotocolLength+1)

	t.Run("ticket-and-forward", func(t *testing.T) {
		ticket, forward := adminIncusUIExtractTicketFromSubprotocols("binary, incudal-incus-ui.tok_123")
		if ticket != "tok_123" {
			t.Fatalf("ticket = %q", ticket)
		}
		if len(forward) != 1 || forward[0] != "binary" {
			t.Fatalf("forward = %v", forward)
		}
	})

	t.Run("last-ticket-wins", func(t *testing.T) {
		ticket, _ := adminIncusUIExtractTicketFromSubprotocols("incudal-incus-ui.first, incudal-incus-ui.second")
		if ticket != "second" {
			t.Fatalf("ticket = %q", ticket)
		}
	})

	t.Run("overlong-ticket-is-ignored-and-not-forwarded", func(t *testing.T) {
		ticket, forward := adminIncusUIExtractTicketFromSubprotocols("incudal-incus-ui." + longTicket)
		if ticket != "" {
			t.Fatalf("ticket = %q, want empty", ticket)
		}
		if len(forward) != 0 {
			t.Fatalf("forward = %v", forward)
		}
	})

	t.Run("invalid-token-is-not-forwarded", func(t *testing.T) {
		ticket, forward := adminIncusUIExtractTicketFromSubprotocols("bad protocol, good-token")
		if ticket != "" {
			t.Fatalf("ticket = %q", ticket)
		}
		if len(forward) != 1 || forward[0] != "good-token" {
			t.Fatalf("forward = %v", forward)
		}
	})

	t.Run("empty", func(t *testing.T) {
		ticket, forward := adminIncusUIExtractTicketFromSubprotocols("")
		if ticket != "" || len(forward) != 0 {
			t.Fatalf("ticket = %q forward = %v", ticket, forward)
		}
	})
}

func TestAdminIncusUIToSendableCloseCode(t *testing.T) {
	sendable := map[int]bool{1000: true, 1001: true, 1002: true, 1003: true, 1007: true, 1014: true, 3000: true, 4999: true}
	unsendable := map[int]bool{0: true, 999: true, 1004: true, 1005: true, 1006: true, 1015: true, 1016: true, 2999: true, 5000: true}

	for code := range sendable {
		if _, ok := adminIncusUIToSendableCloseCode(code); !ok {
			t.Fatalf("%d must be sendable", code)
		}
	}
	for code := range unsendable {
		if _, ok := adminIncusUIToSendableCloseCode(code); ok {
			t.Fatalf("%d must not be sendable", code)
		}
	}
}

// ==================== WebSocket: pre-upgrade ====================

func TestAdminIncusUIWebSocketRejectsNonUpgrade(t *testing.T) {
	env := adminIncusUITestEnvDefault()
	env.install(t)
	engine := adminIncusUIEngine()

	recorder, _ := adminIncusUIPerformRequest(t, engine, http.MethodGet,
		"/api/ws/hosts/995001/incus-proxy/1.0/events", nil, "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %q)", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", recorder.Body.String())
	}
}

// adminIncusUIRawHandshake performs a literal HTTP/1.1 upgrade request so the test
// can observe the status code the server answers BEFORE the 101.
func adminIncusUIRawHandshake(t *testing.T, addr, path string, headers map[string]string) (int, []byte, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 16))
	var request strings.Builder
	fmt.Fprintf(&request, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&request, "Host: %s\r\n", addr)
	request.WriteString("Upgrade: websocket\r\n")
	request.WriteString("Connection: Upgrade\r\n")
	fmt.Fprintf(&request, "Sec-WebSocket-Key: %s\r\n", key)
	request.WriteString("Sec-WebSocket-Version: 13\r\n")
	for name, value := range headers {
		fmt.Fprintf(&request, "%s: %s\r\n", name, value)
	}
	request.WriteString("\r\n")
	if _, err := conn.Write([]byte(request.String())); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, body, conn
}

func TestAdminIncusUIWebSocketAuthBeforeUpgrade(t *testing.T) {
	t.Setenv("NODE_ENV", "development")

	env := adminIncusUITestEnvDefault()
	env.install(t)

	server := httptest.NewServer(adminIncusUIEngine())
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")
	hostID := adminIncusUITestHostID
	wsPath := fmt.Sprintf("/api/ws/hosts/%d/incus-proxy/1.0/events", hostID)

	validTicket := func(userID int32) string {
		return lib.GenerateIncusUIAccessTicket(userID, hostID, time.Now().UTC().Unix(), nil)
	}

	cases := []struct {
		name       string
		path       string
		headers    map[string]string
		prepare    func()
		status     int
		wantFields map[string]any
	}{
		{
			name:       "missing-ticket",
			path:       wsPath,
			status:     http.StatusUnauthorized,
			wantFields: map[string]any{"error": "Authentication ticket required", "code": "UNAUTHORIZED"},
		},
		{
			name:       "invalid-ticket",
			path:       wsPath,
			headers:    map[string]string{"Sec-WebSocket-Protocol": "incudal-incus-ui.deadbeef"},
			status:     http.StatusUnauthorized,
			wantFields: map[string]any{"error": "Invalid or expired ticket", "code": "UNAUTHORIZED"},
		},
		{
			name:       "non-admin-ticket",
			path:       wsPath,
			headers:    map[string]string{"Sec-WebSocket-Protocol": "incudal-incus-ui." + validTicket(adminIncusUITestUserID)},
			status:     http.StatusForbidden,
			wantFields: map[string]any{"error": "Admin privileges required", "code": "ADMIN_REQUIRED"},
		},
		{
			name:       "invalid-host-id",
			path:       "/api/ws/hosts/0/incus-proxy/1.0/events",
			headers:    map[string]string{"Sec-WebSocket-Protocol": "incudal-incus-ui." + validTicket(adminIncusUITestAdminID)},
			status:     http.StatusBadRequest,
			wantFields: map[string]any{"error": "Invalid host ID", "code": "INVALID_HOST_ID"},
		},
		{
			name:       "origin-not-allowed",
			path:       wsPath,
			headers:    map[string]string{"Origin": "https://evil.example", "Sec-WebSocket-Protocol": "incudal-incus-ui." + validTicket(adminIncusUITestAdminID)},
			status:     http.StatusForbidden,
			wantFields: map[string]any{"error": "Origin not allowed", "code": "FORBIDDEN_ORIGIN"},
		},
		{
			name:       "forbidden-path",
			path:       fmt.Sprintf("/api/ws/hosts/%d/incus-proxy/1.0/payincus/status", hostID),
			headers:    map[string]string{"Sec-WebSocket-Protocol": "incudal-incus-ui." + validTicket(adminIncusUITestAdminID)},
			status:     http.StatusForbidden,
			wantFields: map[string]any{"error": "Forbidden Incus proxy path", "code": "FORBIDDEN_PROXY_PATH"},
		},
		{
			name:       "host-not-found",
			path:       "/api/ws/hosts/999999/incus-proxy/1.0/events",
			headers:    map[string]string{"Sec-WebSocket-Protocol": "incudal-incus-ui." + lib.GenerateIncusUIAccessTicket(adminIncusUITestAdminID, 999999, time.Now().UTC().Unix(), nil)},
			status:     http.StatusNotFound,
			wantFields: map[string]any{"error": "Host not found", "code": "HOST_NOT_FOUND"},
		},
		{
			name:       "tunnel-offline",
			path:       wsPath,
			headers:    map[string]string{"Sec-WebSocket-Protocol": "incudal-incus-ui." + validTicket(adminIncusUITestAdminID)},
			prepare:    func() { env.tunnelUp = false },
			status:     http.StatusServiceUnavailable,
			wantFields: map[string]any{"code": "TUNNEL_OFFLINE"},
		},
		{
			name:       "certificate-unavailable",
			path:       wsPath,
			headers:    map[string]string{"Sec-WebSocket-Protocol": "incudal-incus-ui." + validTicket(adminIncusUITestAdminID)},
			prepare:    func() { env.dispatcherErr = fmt.Errorf("宿主机未配置有效的客户端证书或私钥") },
			status:     http.StatusBadGateway,
			wantFields: map[string]any{"error": "宿主机未配置有效的客户端证书或私钥", "code": "INCUS_CERT_UNAVAILABLE"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env.tunnelUp = true
			env.dispatcherErr = nil
			if testCase.prepare != nil {
				testCase.prepare()
			}
			status, body, conn := adminIncusUIRawHandshake(t, addr, testCase.path, testCase.headers)
			defer func() { _ = conn.Close() }()

			if status == http.StatusSwitchingProtocols {
				t.Fatalf("%s: server upgraded before rejecting the request", testCase.name)
			}
			if status != testCase.status {
				t.Fatalf("%s: status = %d, want %d (body %q)", testCase.name, status, testCase.status, body)
			}
			decoded := map[string]any{}
			if len(body) > 0 {
				_ = json.Unmarshal(body, &decoded)
			}
			for key, want := range testCase.wantFields {
				if got := decoded[key]; got != want {
					t.Fatalf("%s: %s = %#v, want %#v (body %q)", testCase.name, key, got, want, body)
				}
			}
		})
	}

	// Nothing was upgraded, so no slot was taken and none leaked.
	if count := adminIncusUIConnectionCount(adminIncusUITestAdminID); count != 0 {
		t.Fatalf("connection slots leaked: %d", count)
	}
}

// ==================== WebSocket: fake upstream relay ====================

// adminIncusUIFakeUpstream is a local WebSocket server that stands in for the node
// agent's tunnel + Incus UI socket ("fake duplex").
type adminIncusUIFakeUpstream struct {
	server   *httptest.Server
	opened   chan struct{}
	closed   chan struct{}
	mu       sync.Mutex
	received []string
	protocol []string
	closeAt  int
	onMsg    func(socket *lib.WebSocket, payload string, isBinary bool)
}

func newAdminIncusUIFakeUpstream(t *testing.T, onMsg func(socket *lib.WebSocket, payload string, isBinary bool)) *adminIncusUIFakeUpstream {
	t.Helper()
	upstream := &adminIncusUIFakeUpstream{
		opened: make(chan struct{}),
		closed: make(chan struct{}),
		onMsg:  onMsg,
	}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.mu.Lock()
		upstream.protocol = append([]string{}, lib.WSOfferedSubprotocols(r)...)
		upstream.mu.Unlock()

		socket, err := lib.UpgradeWebSocket(w, r, &lib.WebSocketUpgradeOptions{MaxPayload: incusUIWSMaxPayload})
		if err != nil {
			t.Errorf("fake upstream upgrade: %v", err)
			return
		}
		close(upstream.opened)

		for {
			payload, isBinary, readErr := socket.ReadMessage()
			if readErr != nil {
				code, _ := socket.CloseCode()
				upstream.mu.Lock()
				upstream.closeAt = code
				upstream.mu.Unlock()
				close(upstream.closed)
				return
			}
			upstream.mu.Lock()
			upstream.received = append(upstream.received, string(payload))
			upstream.mu.Unlock()
			if upstream.onMsg != nil {
				upstream.onMsg(socket, string(payload), isBinary)
			}
		}
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *adminIncusUIFakeUpstream) receivedMessages() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string{}, u.received...)
}

func (u *adminIncusUIFakeUpstream) receivedCloseCode() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.closeAt
}

// adminIncusUIImageRelayFixture wires the fake upstream into the route seams.
func adminIncusUIImageRelayFixture(t *testing.T, onMsg func(socket *lib.WebSocket, payload string, isBinary bool)) (*httptest.Server, *adminIncusUIFakeUpstream) {
	t.Helper()
	env := adminIncusUITestEnvDefault()
	upstream := newAdminIncusUIFakeUpstream(t, onMsg)
	env.dial = func(ctx context.Context, _ *adminIncusUITunnelDispatcher, targetPath string, subprotocols []string) (*lib.WebSocket, error) {
		// Mirror the production dial's TLS-over-tunnel step with a plain ws dial to the
		// local fake, keeping the same target path and forwarded protocols.
		return lib.DialWebSocket(ctx, "ws://"+strings.TrimPrefix(upstream.server.URL, "http://")+targetPath, lib.WebSocketDialOptions{
			HandshakeTimeout: 5 * time.Second,
			MaxPayload:       incusUIWSMaxPayload,
			Subprotocols:     subprotocols,
		})
	}
	env.install(t)

	server := httptest.NewServer(adminIncusUIEngine())
	t.Cleanup(server.Close)
	return server, upstream
}

// adminIncusUIDialBrowser opens the browser-side socket with the ticket subprotocol.
func adminIncusUIDialBrowser(t *testing.T, server *httptest.Server, path, ticket string) *lib.WebSocket {
	t.Helper()
	client, err := lib.DialWebSocket(context.Background(),
		"ws://"+strings.TrimPrefix(server.URL, "http://")+path,
		lib.WebSocketDialOptions{
			HandshakeTimeout: 5 * time.Second,
			MaxPayload:       lib.DefaultWebSocketMaxPayload,
			Subprotocols:     []string{"binary", "incudal-incus-ui." + ticket},
		})
	if err != nil {
		t.Fatalf("dial browser socket: %v", err)
	}
	t.Cleanup(func() { client.Terminate() })
	return client
}

func adminIncusUIReadWithTimeout(t *testing.T, socket *lib.WebSocket) (string, bool) {
	t.Helper()
	type result struct {
		payload string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		payload, _, err := socket.ReadMessage()
		done <- result{payload: string(payload), err: err}
	}()
	select {
	case value := <-done:
		if value.err != nil {
			return "", false
		}
		return value.payload, true
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for a websocket message")
		return "", false
	}
}

func adminIncusUIWaitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestAdminIncusUIWebSocketRelayBidirectional(t *testing.T) {
	t.Setenv("NODE_ENV", "development")
	hostID := adminIncusUITestHostID
	path := fmt.Sprintf("/api/ws/hosts/%d/incus-proxy/1.0/events", hostID)

	server, upstream := adminIncusUIImageRelayFixture(t, func(socket *lib.WebSocket, payload string, isBinary bool) {
		_ = socket.SendMessage([]byte("echo:"+payload), isBinary)
	})

	ticket := lib.GenerateIncusUIAccessTicket(adminIncusUITestAdminID, hostID, time.Now().UTC().Unix(), nil)
	client := adminIncusUIDialBrowser(t, server, path, ticket)

	if got := client.Subprotocol(); got != "binary" {
		t.Fatalf("negotiated subprotocol = %q, want the first offered protocol", got)
	}

	if err := client.SendText("hello"); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if got, ok := adminIncusUIReadWithTimeout(t, client); !ok || got != "echo:hello" {
		t.Fatalf("echo = %q ok=%v", got, ok)
	}

	// Binary frames are relayed with their opcode intact.
	done := make(chan struct{})
	go func() {
		payload, isBinary, err := client.ReadMessage()
		if err != nil || !isBinary || string(payload) != "echo:bytes" {
			t.Errorf("binary echo = %q isBinary=%v err=%v", payload, isBinary, err)
		}
		close(done)
	}()
	if err := client.SendBinary([]byte("bytes")); err != nil {
		t.Fatalf("send binary: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for the binary echo")
	}

	<-upstream.opened
	adminIncusUIWaitFor(t, 5*time.Second, func() bool {
		return len(upstream.receivedMessages()) >= 2
	})

	upstream.mu.Lock()
	protocol := append([]string{}, upstream.protocol...)
	upstream.mu.Unlock()
	if len(protocol) != 1 || protocol[0] != "binary" {
		t.Fatalf("upstream protocols = %v, want only the forwarded (non-ticket) protocols", protocol)
	}

	// Closing the browser socket tears the upstream down and frees the slot.
	client.Close(1000, "")
	select {
	case <-upstream.closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("upstream socket was not closed after the browser closed")
	}
	adminIncusUIWaitFor(t, 5*time.Second, func() bool {
		return adminIncusUIConnectionCount(adminIncusUITestAdminID) == 0
	})
}

func TestAdminIncusUIWebSocketRelayBuffersEarlyMessages(t *testing.T) {
	t.Setenv("NODE_ENV", "development")
	hostID := adminIncusUITestHostID
	path := fmt.Sprintf("/api/ws/hosts/%d/incus-proxy/1.0/events", hostID)

	// Slow upstream handshake: every message sent by the browser during the dial must
	// be buffered and flushed afterwards, in order.
	env := adminIncusUITestEnvDefault()
	upstream := newAdminIncusUIFakeUpstream(t, nil)
	env.dial = func(ctx context.Context, _ *adminIncusUITunnelDispatcher, targetPath string, subprotocols []string) (*lib.WebSocket, error) {
		time.Sleep(150 * time.Millisecond)
		return lib.DialWebSocket(ctx, "ws://"+strings.TrimPrefix(upstream.server.URL, "http://")+targetPath, lib.WebSocketDialOptions{
			HandshakeTimeout: 5 * time.Second,
			MaxPayload:       incusUIWSMaxPayload,
			Subprotocols:     subprotocols,
		})
	}
	env.install(t)
	server := httptest.NewServer(adminIncusUIEngine())
	defer server.Close()

	ticket := lib.GenerateIncusUIAccessTicket(adminIncusUITestAdminID, hostID, time.Now().UTC().Unix(), nil)
	client := adminIncusUIDialBrowser(t, server, path, ticket)

	for index := 0; index < 3; index++ {
		if err := client.SendText(fmt.Sprintf("early-%d", index)); err != nil {
			t.Fatalf("send early-%d: %v", index, err)
		}
	}

	adminIncusUIWaitFor(t, 5*time.Second, func() bool {
		return len(upstream.receivedMessages()) >= 3
	})
	got := upstream.receivedMessages()
	want := []string{"early-0", "early-1", "early-2"}
	if len(got) < 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("received = %v, want %v", got, want)
	}
}

func TestAdminIncusUIWebSocketRelayForwardsUpstreamCloseCodes(t *testing.T) {
	t.Setenv("NODE_ENV", "development")
	hostID := adminIncusUITestHostID
	path := fmt.Sprintf("/api/ws/hosts/%d/incus-proxy/1.0/events", hostID)

	cases := []struct {
		name     string
		upstream func(*lib.WebSocket)
		wantCode int
	}{
		{name: "sendable-code", upstream: func(socket *lib.WebSocket) { socket.Close(1001, "") }, wantCode: 1001},
		{name: "protocol-error", upstream: func(socket *lib.WebSocket) { socket.Close(1002, "") }, wantCode: 1002},
		{name: "application-code", upstream: func(socket *lib.WebSocket) { socket.Close(4001, "") }, wantCode: 4001},
		// `ws.close()` with no code: an empty close frame, which a browser reports as
		// 1005. This is the case that needs lib.WebSocket.CloseNoStatus.
		{name: "empty-close-frame", upstream: func(socket *lib.WebSocket) { socket.CloseNoStatus() }, wantCode: 1005},
		// A transport drop has no code (1006 internally) and must also become an empty
		// close frame rather than a fabricated 1000.
		{name: "abnormal-terminate", upstream: func(socket *lib.WebSocket) { socket.Terminate() }, wantCode: 1005},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := adminIncusUITestEnvDefault()
			upstream := newAdminIncusUIFakeUpstream(t, func(socket *lib.WebSocket, payload string, _ bool) {
				if payload == "close-now" {
					select {
					case <-time.After(20 * time.Millisecond):
					}
					testCase.upstream(socket)
				}
			})
			env.dial = func(ctx context.Context, _ *adminIncusUITunnelDispatcher, targetPath string, subprotocols []string) (*lib.WebSocket, error) {
				return lib.DialWebSocket(ctx, "ws://"+strings.TrimPrefix(upstream.server.URL, "http://")+targetPath, lib.WebSocketDialOptions{
					HandshakeTimeout: 5 * time.Second,
					MaxPayload:       incusUIWSMaxPayload,
					Subprotocols:     subprotocols,
				})
			}
			env.install(t)
			server := httptest.NewServer(adminIncusUIEngine())
			defer server.Close()

			ticket := lib.GenerateIncusUIAccessTicket(adminIncusUITestAdminID, hostID, time.Now().UTC().Unix(), nil)
			client := adminIncusUIDialBrowser(t, server, path, ticket)
			if err := client.SendText("close-now"); err != nil {
				t.Fatalf("send close-now: %v", err)
			}

			if _, ok := adminIncusUIReadWithTimeout(t, client); ok {
				t.Fatalf("expected the client socket to close")
			}
			code, _ := client.CloseCode()
			if code != testCase.wantCode {
				t.Fatalf("client close code = %d, want %d", code, testCase.wantCode)
			}
			adminIncusUIWaitFor(t, 5*time.Second, func() bool {
				return adminIncusUIConnectionCount(adminIncusUITestAdminID) == 0
			})
		})
	}
}

func TestAdminIncusUIWebSocketRelayDialFailureClosesClient(t *testing.T) {
	t.Setenv("NODE_ENV", "development")
	env := adminIncusUITestEnvDefault()
	env.dial = func(context.Context, *adminIncusUITunnelDispatcher, string, []string) (*lib.WebSocket, error) {
		return nil, fmt.Errorf("tunnel dial failed")
	}
	env.install(t)
	server := httptest.NewServer(adminIncusUIEngine())
	defer server.Close()

	hostID := adminIncusUITestHostID
	path := fmt.Sprintf("/api/ws/hosts/%d/incus-proxy/1.0/events", hostID)
	ticket := lib.GenerateIncusUIAccessTicket(adminIncusUITestAdminID, hostID, time.Now().UTC().Unix(), nil)
	client := adminIncusUIDialBrowser(t, server, path, ticket)

	if _, ok := adminIncusUIReadWithTimeout(t, client); ok {
		t.Fatalf("expected a close when the upstream dial fails")
	}
	// The TypeScript's cleanupBoth() calls socket.close(), i.e. an empty close frame.
	if code, _ := client.CloseCode(); code != 1005 {
		t.Fatalf("close code = %d, want 1005", code)
	}
	adminIncusUIWaitFor(t, 5*time.Second, func() bool {
		return adminIncusUIConnectionCount(adminIncusUITestAdminID) == 0
	})
}

// ==================== HTTP reverse proxy relay ====================

// adminIncusUIFakeProxyUpstream is a local TLS server standing in for incusd: it
// records what the proxy forwarded and answers with a fixed response.
type adminIncusUIFakeProxyUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	method string
	path   string
	query  string
	header http.Header
	body   string
	calls  int
}

func newAdminIncusUIFakeProxyUpstream(t *testing.T, status int, responseHeader map[string]string, responseBody string) *adminIncusUIFakeProxyUpstream {
	t.Helper()
	upstream := &adminIncusUIFakeProxyUpstream{server: nil}
	upstream.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.calls++
		upstream.method = r.Method
		upstream.path = r.URL.Path
		upstream.query = r.URL.RawQuery
		upstream.header = r.Header.Clone()
		upstream.body = string(body)
		upstream.mu.Unlock()

		for name, value := range responseHeader {
			w.Header().Set(name, value)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(responseBody))
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// adminIncusUIDispatcherForUpstream builds a dispatcher whose HTTP client talks to the
// fake incusd instead of dialling the tunnel (the transport's certificate is
// irrelevant there, only the address matters).
func adminIncusUIFakeProxyDispatcher(t *testing.T, upstream *adminIncusUIFakeProxyUpstream) *adminIncusUITunnelDispatcher {
	t.Helper()
	return &adminIncusUITunnelDispatcher{
		client:           &http.Client{Transport: upstream.server.Client().Transport},
		upstreamHostPort: strings.TrimPrefix(upstream.server.URL, "https://"),
		targetHost:       "127.0.0.1",
		targetPort:       8443,
	}
}

func TestAdminIncusUIProxyForwardsRequestAndStreamsResponse(t *testing.T) {
	t.Setenv("NODE_ENV", "development")

	upstream := newAdminIncusUIFakeProxyUpstream(t, http.StatusOK,
		map[string]string{"X-Upstream": "yes", "Content-Type": "application/json"},
		`{"type":"sync","metadata":{}}`)

	env := adminIncusUITestEnvDefault()
	env.dispatcher = adminIncusUIFakeProxyDispatcher(t, upstream)

	var logged int32
	previousLog := adminIncusUILogMutation
	adminIncusUILogMutation = func(context.Context, int32, string, int32, string, string) {
		atomic.AddInt32(&logged, 1)
	}
	t.Cleanup(func() { adminIncusUILogMutation = previousLog })

	env.install(t)
	engine := adminIncusUIEngine()
	token := adminIncusUIAdminToken(t, adminIncusUITestAdminID, "admin", "sess")

	recorder, _ := adminIncusUIPerformRequest(t, engine, http.MethodGet,
		"/api/admin/hosts/995001/incus-proxy/1.0/info?recursion=0&__incudal_ticket=strip-me",
		map[string]string{
			"Authorization": "Bearer " + token,
			"X-Parity":      "1",
			"Cookie":        "session=secret",
		}, "")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != `{"type":"sync","metadata":{}}` {
		t.Fatalf("body = %q", got)
	}
	if got := recorder.Header().Get("X-Upstream"); got != "yes" {
		t.Fatalf("upstream response header not forwarded: %q", got)
	}

	upstream.mu.Lock()
	method, path, query := upstream.method, upstream.path, upstream.query
	header, calls := upstream.header.Clone(), upstream.calls
	upstream.mu.Unlock()

	if calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls)
	}
	if method != http.MethodGet {
		t.Fatalf("upstream method = %q", method)
	}
	if path != "/1.0/info" {
		t.Fatalf("upstream path = %q", path)
	}
	if query != "recursion=0" {
		t.Fatalf("upstream query = %q (the ticket parameter must be stripped)", query)
	}
	if header.Get("X-Parity") != "1" {
		t.Fatalf("X-Parity not forwarded: %v", header)
	}
	if header.Get("Authorization") != "" {
		t.Fatalf("Authorization must never reach incusd: %v", header)
	}
	if header.Get("Cookie") != "" {
		t.Fatalf("Cookie must never reach incusd: %v", header)
	}
	if atomic.LoadInt32(&logged) != 0 {
		t.Fatalf("bodyless requests must not be audited as mutations")
	}
}

func TestAdminIncusUIProxyMutationForwardsBodyAndLogs(t *testing.T) {
	t.Setenv("NODE_ENV", "development")

	upstream := newAdminIncusUIFakeProxyUpstream(t, http.StatusCreated,
		map[string]string{"Content-Type": "application/json"},
		`{"type":"sync"}`)

	env := adminIncusUITestEnvDefault()
	env.dispatcher = adminIncusUIFakeProxyDispatcher(t, upstream)

	type logCall struct {
		userID   int32
		hostName string
		hostID   int32
		method   string
		path     string
	}
	logged := make(chan logCall, 4)
	previousLog := adminIncusUILogMutation
	adminIncusUILogMutation = func(_ context.Context, userID int32, hostName string, hostID int32, method, path string) {
		logged <- logCall{userID: userID, hostName: hostName, hostID: hostID, method: method, path: path}
	}
	t.Cleanup(func() { adminIncusUILogMutation = previousLog })

	env.install(t)
	engine := adminIncusUIEngine()
	token := adminIncusUIAdminToken(t, adminIncusUITestAdminID, "admin", "sess")

	recorder, _ := adminIncusUIPerformRequest(t, engine, http.MethodPost,
		"/api/admin/hosts/995001/incus-proxy/1.0/instances",
		map[string]string{"Authorization": "Bearer " + token}, `{"name":"parity"}`)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", recorder.Code, recorder.Body.String())
	}

	upstream.mu.Lock()
	body := upstream.body
	contentType := upstream.header.Get("Content-Type")
	upstream.mu.Unlock()
	if body != `{"name":"parity"}` {
		t.Fatalf("upstream body = %q", body)
	}
	if contentType != "application/json" {
		t.Fatalf("upstream content-type = %q", contentType)
	}

	select {
	case call := <-logged:
		if call.userID != adminIncusUITestAdminID || call.hostID != adminIncusUITestHostID ||
			call.method != http.MethodPost || call.path != "/1.0/instances" {
			t.Fatalf("audit call = %+v", call)
		}
		if call.hostName != adminIncusUITestHostName {
			t.Fatalf("audit host name = %q", call.hostName)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("mutation was not audited")
	}
}

func TestAdminIncusUIProxyUpstreamFailureIsBadGateway(t *testing.T) {
	t.Setenv("NODE_ENV", "development")

	env := adminIncusUITestEnvDefault()
	// No TLS server behind the address: the client fails to connect.
	env.dispatcher = &adminIncusUITunnelDispatcher{
		client:           &http.Client{Transport: &http.Transport{}},
		upstreamHostPort: "127.0.0.1:1",
	}
	env.install(t)
	engine := adminIncusUIEngine()
	token := adminIncusUIAdminToken(t, adminIncusUITestAdminID, "admin", "sess")

	recorder, decoded := adminIncusUIPerformRequest(t, engine, http.MethodGet,
		"/api/admin/hosts/995001/incus-proxy/1.0/info",
		map[string]string{"Authorization": "Bearer " + token}, "")
	adminIncusUIExpectJSON(t, "bad gateway", recorder, decoded, http.StatusBadGateway, map[string]any{
		"error": "Failed to proxy request to host Incus daemon over tunnel",
		"code":  "INCUS_PROXY_ERROR",
	})
}
