package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

func spiceTestClaims() *lib.AccessTokenClaims {
	return &lib.AccessTokenClaims{ID: 77, IssuedAt: 1_700_000_000, SID: "session-1"}
}

// spiceHandlerContext builds a gin context for a direct handler call.
func spiceHandlerContext(t *testing.T, method, path, id, body string, claims *lib.AccessTokenClaims) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, path, nil)
	} else {
		request = httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	context.Request = request
	context.Params = gin.Params{{Key: "id", Value: id}}
	context.Set(middleware.ContextUserKey, claims)
	return context, recorder
}

// TestSpiceWebSocketRejectsBeforeUpgrade pins the SPICE handshake gate: no
// upgrade → 404; bad id/origin/ticket → HTTP error, never 101.
func TestSpiceWebSocketRejectsBeforeUpgrade(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("NODE_ENV", "development")
	t.Setenv("CORS_ORIGINS", "")

	router := gin.New()
	RegisterSpiceRoutes(router)
	serve := func(request *http.Request, headers map[string]string) *httptest.ResponseRecorder {
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder
	}

	// Plain GET (no upgrade): 404 empty body.
	recorder := serve(httptest.NewRequest(http.MethodGet, "/api/ws/instances/1/spice", nil), nil)
	if recorder.Code != http.StatusNotFound || recorder.Body.Len() != 0 {
		t.Fatalf("plain GET = %d %q", recorder.Code, recorder.Body.String())
	}

	cases := []struct {
		name    string
		path    string
		headers map[string]string
		status  int
		code    string
	}{
		{
			name:   "invalid id",
			path:   "/api/ws/instances/0/spice",
			status: http.StatusBadRequest,
			code:   "INVALID_ID",
		},
		{
			name:    "origin not allowed",
			path:    "/api/ws/instances/1/spice",
			headers: map[string]string{"Origin": "https://evil.example.com"},
			status:  http.StatusForbidden,
			code:    "ORIGIN_NOT_ALLOWED",
		},
		{
			name:   "missing ticket protocol",
			path:   "/api/ws/instances/1/spice",
			status: http.StatusUnauthorized,
			code:   "UNAUTHORIZED",
		},
		{
			name: "unknown ticket",
			path: "/api/ws/instances/1/spice",
			headers: map[string]string{
				"Sec-WebSocket-Protocol": "binary, incudal-spice." + strings.Repeat("a", 48),
			},
			status: http.StatusUnauthorized,
			code:   "UNAUTHORIZED",
		},
		{
			name: "ticket too long",
			path: "/api/ws/instances/1/spice",
			headers: map[string]string{
				"Sec-WebSocket-Protocol": "binary, incudal-spice." + strings.Repeat("a", 65),
			},
			status: http.StatusUnauthorized,
			code:   "UNAUTHORIZED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := wsUpgradeRequest(tc.path, "")
			// The generic upgrade request carries a fixed key; SPICE only needs it
			// after the preflight, but keep it valid anyway.
			recorder := serve(request, tc.headers)
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d (never 101)", recorder.Code, tc.status)
			}
			body := decodeResponse(t, recorder)
			if body["code"] != tc.code {
				t.Fatalf("code = %v, want %s", body["code"], tc.code)
			}
		})
	}
}

// TestSpiceSessionBodyValidation pins the AJV semantics of `{reconnect?: boolean}`
// with additionalProperties:false, rendered through app.ts's error mapping.
func TestSpiceSessionBodyValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name    string
		body    string
		status  int
		message string
	}{
		{"wrong type", `{"reconnect":"maybe"}`, http.StatusBadRequest, "must be boolean"},
		{"not an object", `[]`, http.StatusBadRequest, "must be object"},
		{"invalid json", `{`, http.StatusBadRequest, "Body is not valid JSON but content-type is set to 'application/json'"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			context, recorder := spiceHandlerContext(t, http.MethodPost, "/api/ws/instances/1/spice-session", "1", tc.body, spiceTestClaims())
			spiceCreateSessionHandler(context)
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tc.status, recorder.Body.String())
			}
			body := decodeResponse(t, recorder)
			if body["error"] != tc.message || body["code"] != "VALIDATION_ERROR" {
				t.Fatalf("body = %v", body)
			}
		})
	}

	// AJV's removeAdditional:true drops unknown keys instead of rejecting them, and
	// coerceTypes accepts "true". Both body shapes reach the handler, which then
	// fails on the database/Incus side (no DB in unit tests) rather than with a 400.
	for _, body := range []string{`{"reconnect":true,"extra":1}`, `{"reconnect":"true"}`, `{"reconnect":1}`, `{}`} {
		context, recorder := spiceHandlerContext(t, http.MethodPost, "/api/ws/instances/1/spice-session", "1", body, spiceTestClaims())
		spiceCreateSessionHandler(context)
		if recorder.Code == http.StatusBadRequest {
			t.Fatalf("valid body %q rejected: %s", body, recorder.Body.String())
		}
	}
}

// TestSpiceSessionInvalidID pins the id gate of POST and DELETE.
func TestSpiceSessionInvalidID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	context, recorder := spiceHandlerContext(t, http.MethodPost, "/api/ws/instances/0/spice-session", "0", `{}`, spiceTestClaims())
	spiceCreateSessionHandler(context)
	if recorder.Code != http.StatusBadRequest || decodeResponse(t, recorder)["code"] != "INVALID_ID" {
		t.Fatalf("POST invalid id = %d %s", recorder.Code, recorder.Body.String())
	}

	context, recorder = spiceHandlerContext(t, http.MethodDelete, "/api/ws/instances/0/spice-session", "0", `{"ticket":"x"}`, spiceTestClaims())
	spiceDeleteSessionHandler(context)
	if recorder.Code != http.StatusBadRequest || decodeResponse(t, recorder)["error"] != "Invalid instance ID" {
		t.Fatalf("DELETE invalid id = %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestSpiceDeleteSessionValidation pins the DELETE body schema and the
// unknown-ticket success path.
func TestSpiceDeleteSessionValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newDelete := func(id, body string) (*gin.Context, *httptest.ResponseRecorder) {
		return spiceHandlerContext(t, http.MethodDelete, "/api/ws/instances/"+id+"/spice-session", id, body, spiceTestClaims())
	}

	context, recorder := newDelete("1", "")
	spiceDeleteSessionHandler(context)
	if recorder.Code != http.StatusBadRequest || decodeResponse(t, recorder)["error"] != "must be object" {
		t.Fatalf("no body = %d %s", recorder.Code, recorder.Body.String())
	}

	context, recorder = newDelete("1", `{}`)
	spiceDeleteSessionHandler(context)
	if recorder.Code != http.StatusBadRequest || decodeResponse(t, recorder)["error"] != "ticket is required" {
		t.Fatalf("missing ticket = %d %s", recorder.Code, recorder.Body.String())
	}

	context, recorder = newDelete("1", `{"ticket":"`+strings.Repeat("a", 65)+`"}`)
	spiceDeleteSessionHandler(context)
	if recorder.Code != http.StatusBadRequest || decodeResponse(t, recorder)["error"] != "ticket is too long" {
		t.Fatalf("long ticket = %d %s", recorder.Code, recorder.Body.String())
	}

	// The main channel may already have cleaned the session up: still 200.
	context, recorder = newDelete("1", `{"ticket":"unknown-ticket"}`)
	spiceDeleteSessionHandler(context)
	if recorder.Code != http.StatusOK || decodeResponse(t, recorder)["success"] != true {
		t.Fatalf("unknown ticket = %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestSpiceRateLimit pins the inline 10/minute limit on POST /spice-session.
func TestSpiceRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ResetSpiceRateLimits()
	defer ResetSpiceRateLimits()

	router := gin.New()
	router.POST("/api/ws/instances/:id/spice-session",
		spiceRateLimit("/api/ws/instances/:id/spice-session", 10, time.Minute),
		func(c *gin.Context) { c.Status(http.StatusOK) })

	for index := 0; index < 10; index++ {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/ws/instances/1/spice-session", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d = %d", index, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/ws/instances/1/spice-session", nil))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("11th request = %d, want 429", recorder.Code)
	}
	if decodeResponse(t, recorder)["error"] != "Too many requests, please try again later" {
		t.Fatalf("429 body = %s", recorder.Body.String())
	}
}

// TestSpiceTicketProtocolParsing pins the Sec-WebSocket-Protocol extraction.
func TestSpiceTicketProtocolParsing(t *testing.T) {
	if got := spiceTicketFromProtocols([]string{"binary", "incudal-spice.abc"}); got != "abc" {
		t.Fatalf("ticket = %q", got)
	}
	if got := spiceTicketFromProtocols([]string{"binary"}); got != "" {
		t.Fatalf("ticket without prefix = %q", got)
	}
	if got := spiceTicketFromProtocols(nil); got != "" {
		t.Fatalf("nil protocols = %q", got)
	}
}
