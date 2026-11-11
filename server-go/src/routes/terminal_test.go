package routes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// wsUpgradeRequest builds a valid RFC 6455 upgrade request for the route tests.
func wsUpgradeRequest(path, origin string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	return request
}

func decodeResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON (%q): %v", recorder.Body.String(), err)
	}
	return body
}

// TestTerminalWebSocketRejectsBeforeUpgrade is the iron-rule test: every failure
// mode answers HTTP and NEVER 101.
func TestTerminalWebSocketRejectsBeforeUpgrade(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("NODE_ENV", "development")
	t.Setenv("CORS_ORIGINS", "")
	t.Setenv("FRONTEND_URL", "")
	t.Setenv("ADMIN_FRONTEND_URL", "")

	router := gin.New()
	RegisterTerminalRoutes(router)

	serve := func(request *http.Request) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}

	// A plain GET (no Upgrade) is 404 with an empty body, like @fastify/websocket.
	recorder := serve(httptest.NewRequest(http.MethodGet, "/api/ws/instances/1/terminal", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("plain GET status = %d, want 404", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("plain GET body = %q, want empty", recorder.Body.String())
	}

	cases := []struct {
		name    string
		request *http.Request
		status  int
		code    string
	}{
		{
			name:    "no ticket",
			request: wsUpgradeRequest("/api/ws/instances/1/terminal", ""),
			status:  http.StatusUnauthorized,
			code:    "UNAUTHORIZED",
		},
		{
			name:    "bad ticket",
			request: wsUpgradeRequest("/api/ws/instances/1/terminal?ticket=bogus", ""),
			status:  http.StatusUnauthorized,
			code:    "UNAUTHORIZED",
		},
		{
			name:    "invalid id",
			request: wsUpgradeRequest("/api/ws/instances/0/terminal?ticket=bogus", ""),
			status:  http.StatusBadRequest,
			code:    "INVALID_ID",
		},
		{
			name:    "origin not allowed",
			request: wsUpgradeRequest("/api/ws/instances/1/terminal?ticket=bogus", "https://evil.example.com"),
			status:  http.StatusForbidden,
			code:    "ORIGIN_NOT_ALLOWED",
		},
		{
			name:    "ticket bound to another instance",
			request: wsUpgradeRequest("/api/ws/instances/1/terminal?ticket="+lib.GenerateTerminalAccessTicket(5, 999, 1_700_000_000, nil), ""),
			status:  http.StatusUnauthorized,
			code:    "UNAUTHORIZED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := serve(tc.request)
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d (never 101)", recorder.Code, tc.status)
			}
			body := decodeResponse(t, recorder)
			if body["code"] != tc.code {
				t.Fatalf("code = %v, want %s", body["code"], tc.code)
			}
		})
	}

	// The configured dev origin passes the origin gate and then hits the ticket
	// gate — proving the order (origin before ticket) matches terminal.ts.
	recorder = serve(wsUpgradeRequest("/api/ws/instances/1/terminal", "http://localhost:3000"))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("allowed origin without ticket = %d, want 401", recorder.Code)
	}
}

// TestTerminalStatsRequiresAdmin pins the admin decorator on GET /stats.
func TestTerminalStatsRequiresAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterTerminalRoutes(router)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/ws/instances/stats", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
}

// TestTerminalTicketHandlerValidation drives the handler directly (the auth
// middleware needs a database) to pin the id and iat checks.
func TestTerminalTicketHandlerValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newContext := func(id string, claims *lib.AccessTokenClaims) (*gin.Context, *httptest.ResponseRecorder) {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodPost, "/api/ws/instances/"+id+"/terminal-ticket", nil)
		context.Params = gin.Params{{Key: "id", Value: id}}
		context.Set(middleware.ContextUserKey, claims)
		return context, recorder
	}

	context, recorder := newContext("0", &lib.AccessTokenClaims{ID: 1, IssuedAt: 1_700_000_000})
	terminalTicketHandler(context)
	if recorder.Code != http.StatusBadRequest || decodeResponse(t, recorder)["code"] != "INVALID_ID" {
		t.Fatalf("invalid id: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	context, recorder = newContext("1", &lib.AccessTokenClaims{ID: 1})
	terminalTicketHandler(context)
	if recorder.Code != http.StatusUnauthorized || decodeResponse(t, recorder)["code"] != "UNAUTHORIZED" {
		t.Fatalf("missing iat: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestTerminalClaimConnectionLimits pins the atomic reservation: five sessions per
// user and three per instance.
func TestTerminalClaimConnectionLimits(t *testing.T) {
	lib.ResetTerminalSessionsForTest()
	resetTerminalReservations()
	t.Cleanup(func() {
		lib.ResetTerminalSessionsForTest()
		resetTerminalReservations()
	})

	for index := 0; index < terminalMaxPerUser; index++ {
		lib.AddTerminalSessionForTest(&lib.TerminalSession{
			ID: fmt.Sprintf("term_user_%d", index), UserID: 7, InstanceID: int32(100 + index),
		})
	}
	if reservation := terminalClaimConnection(7, 200); reservation.Allowed || reservation.Scope != "user" ||
		reservation.Current != terminalMaxPerUser || reservation.Limit != terminalMaxPerUser {
		t.Fatalf("user limit reservation = %+v", reservation)
	}

	lib.ResetTerminalSessionsForTest()
	resetTerminalReservations()
	for index := 0; index < terminalMaxPerInstance; index++ {
		lib.AddTerminalSessionForTest(&lib.TerminalSession{
			ID: fmt.Sprintf("term_instance_%d", index), UserID: int32(20 + index), InstanceID: 300,
		})
	}
	if reservation := terminalClaimConnection(8, 300); reservation.Allowed || reservation.Scope != "instance" ||
		reservation.Current != terminalMaxPerInstance {
		t.Fatalf("instance limit reservation = %+v", reservation)
	}

	// Pending reservations count too, and releasing is idempotent.
	lib.ResetTerminalSessionsForTest()
	resetTerminalReservations()
	var granted []terminalReservation
	for index := 0; index < terminalMaxPerInstance; index++ {
		reservation := terminalClaimConnection(9, 400)
		if !reservation.Allowed {
			t.Fatalf("connection %d rejected: %+v", index, reservation)
		}
		granted = append(granted, reservation)
	}
	if reservation := terminalClaimConnection(9, 400); reservation.Allowed || reservation.Scope != "instance" {
		t.Fatalf("fourth pending connection = %+v", reservation)
	}
	for _, reservation := range granted {
		reservation.Release()
		reservation.Release() // idempotent
	}
	if reservation := terminalClaimConnection(9, 400); !reservation.Allowed {
		t.Fatalf("released reservation not available again: %+v", reservation)
	}
}

// TestTerminalParseRouteID pins the strict id parser used by both handlers.
func TestTerminalParseRouteID(t *testing.T) {
	cases := map[string]bool{
		"1": true, "900001": true, "2147483647": true,
		"0": false, "01": false, "-1": false, "+1": false, "abc": false, "1.5": false,
		"2147483648": false, "9007199254740993": false,
	}
	for value, want := range cases {
		if _, ok := terminalParseRouteID(value); ok != want {
			t.Errorf("terminalParseRouteID(%q) ok = %t, want %t", value, ok, want)
		}
	}
}

// TestTerminalWebSocketRouteNo101ForBadOrigin catches a regression where the
// handshake happens before the origin gate.
func TestTerminalWebSocketRouteNo101ForBadOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterTerminalRoutes(router)
	recorder := httptest.NewRecorder()
	request := wsUpgradeRequest("/api/ws/instances/1/terminal", "https://evil.example.com")
	router.ServeHTTP(recorder, request)
	if strings.HasPrefix(recorder.Body.String(), "HTTP/1.1 101") || recorder.Code == http.StatusSwitchingProtocols {
		t.Fatal("an unauthenticated upgrade must never receive 101")
	}
}
