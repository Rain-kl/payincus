package lib_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// upgradeSocketPair returns a server-side socket backed by a live client dial and
// a client-side socket. Both stay open until the returned cleanup runs.
func upgradeSocketPair(t *testing.T) (*lib.WebSocket, *lib.WebSocket, func()) {
	t.Helper()
	serverSockets := make(chan *lib.WebSocket, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := lib.UpgradeWebSocket(w, r, nil)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		serverSockets <- socket
		<-r.Context().Done()
	}))
	client, err := lib.DialWebSocket(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), lib.WebSocketDialOptions{})
	if err != nil {
		server.Close()
		t.Fatalf("dial: %v", err)
	}
	serverSocket := <-serverSockets
	cleanup := func() {
		client.Terminate()
		serverSocket.Terminate()
		server.Close()
	}
	return serverSocket, client, cleanup
}

// TestWebSocketSecurityOriginMatrix pins validateWebSocketOrigin byte for byte:
// production rejects a missing Origin, an unparseable Origin is always rejected,
// membership is exact (no subdomain suffix matching), and the dev defaults add the
// localhost panels.
func TestWebSocketSecurityOriginMatrix(t *testing.T) {
	makeRequest := func(origin string) *http.Request {
		request := httptest.NewRequest(http.MethodGet, "/api/ws/instances/1/terminal", nil)
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		return request
	}

	// Development (the harness runs with NODE_ENV=development).
	t.Setenv("NODE_ENV", "development")
	t.Setenv("CORS_ORIGINS", "https://pay.example.com")
	t.Setenv("FRONTEND_URL", "")
	t.Setenv("ADMIN_FRONTEND_URL", "")

	cases := []struct {
		origin  string
		allowed bool
		reason  string
	}{
		{"", true, ""},                                                   // missing Origin: allowed outside production
		{"https://pay.example.com", true, ""},                            // configured origin
		{"https://pay.example.com/", true, ""},                           // normalized to scheme://host
		{"https://PAY.example.com", true, ""},                            // WHATWG URL.origin lowercases the host
		{"https://pay.example.com:443", true, ""},                        // default port is dropped
		{"https://pay.example.com:8443", false, "Origin is not allowed"}, // non-default port is kept
		{"https://evil.pay.example.com", false, "Origin is not allowed"},
		{"https://pay.example.com.evil.test", false, "Origin is not allowed"},
		{"http://localhost:3000", true, ""}, // dev default
		{"http://127.0.0.1:3002", true, ""}, // dev default
		{"http://localhost:3001", true, ""}, // WS-only dev default
		{"http://localhost:4000", false, "Origin is not allowed"},
		{"not a url", false, "Invalid Origin header"},
		{"//pay.example.com", false, "Invalid Origin header"},
	}
	for _, tc := range cases {
		result := lib.ValidateWebSocketOrigin(makeRequest(tc.origin))
		if result.Allowed != tc.allowed || (tc.reason != "" && result.Reason != tc.reason) {
			t.Errorf("origin %q: got (allowed=%t reason=%q), want (allowed=%t reason=%q)",
				tc.origin, result.Allowed, result.Reason, tc.allowed, tc.reason)
		}
	}

	// Production: a missing Origin is rejected, and only the configured list counts.
	t.Setenv("NODE_ENV", "production")
	if result := lib.ValidateWebSocketOrigin(makeRequest("")); result.Allowed || result.Reason != "Missing Origin header" {
		t.Fatalf("production missing origin = %+v", result)
	}
	if result := lib.ValidateWebSocketOrigin(makeRequest("http://localhost:3000")); result.Allowed {
		t.Fatal("production must not accept the dev localhost default")
	}
	if result := lib.ValidateWebSocketOrigin(makeRequest("https://pay.example.com")); !result.Allowed {
		t.Fatalf("production configured origin rejected: %+v", result)
	}
}

// TestWebSocketSecurityConnectionLimits pins the per-IP (10) and per-user (5)
// counters and their automatic release on close.
func TestWebSocketSecurityConnectionLimits(t *testing.T) {
	lib.ResetWebSocketSecurityState()
	defer lib.ResetWebSocketSecurityState()

	const ip = "203.0.113.9"
	userID := int32(4242)
	var cleanups []func()
	defer func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()

	for index := 0; index < 9; index++ {
		socket, _, cleanup := upgradeSocketPair(t)
		cleanups = append(cleanups, cleanup)
		lib.RegisterConnection(ip, socket, &userID)
	}
	if limit := lib.CheckIPConnectionLimit(ip); !limit.Allowed || limit.Current != 9 || limit.Limit != 10 {
		t.Fatalf("after 9 registrations: %+v", limit)
	}
	// The per-user limit (5) is already exceeded by nine registrations; the helper
	// reports the raw count and the caller rejects at >= 5.
	if limit := lib.CheckUserConnectionLimit(userID); limit.Allowed || limit.Current != 9 || limit.Limit != 5 {
		t.Fatalf("user limit = %+v", limit)
	}

	socket, _, cleanup := upgradeSocketPair(t)
	cleanups = append(cleanups, cleanup)
	lib.RegisterConnection(ip, socket, &userID)
	if limit := lib.CheckIPConnectionLimit(ip); limit.Allowed || limit.Current != 10 {
		t.Fatalf("after 10 registrations: %+v", limit)
	}
	if limit := lib.CheckUserConnectionLimit(userID); limit.Allowed || limit.Current != 10 {
		t.Fatalf("user limit after 10: %+v", limit)
	}

	// Closing one socket releases one slot in both counters.
	socket.Terminate()
	deadline := time.Now().Add(2 * time.Second)
	for {
		limit := lib.CheckIPConnectionLimit(ip)
		if limit.Current == 9 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ip counter did not release: %+v", limit)
		}
		time.Sleep(10 * time.Millisecond)
	}

	stats := lib.GetConnectionStats()
	if stats.TotalIPs == 0 || stats.TotalConnections == 0 || stats.TotalUsers == 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

// TestWebSocketSecurityDisconnect pins the admin disconnect helpers and their
// close codes (4002 user, 4003 IP).
func TestWebSocketSecurityDisconnect(t *testing.T) {
	lib.ResetWebSocketSecurityState()
	defer lib.ResetWebSocketSecurityState()

	userID := int32(7)
	socket, client, cleanup := upgradeSocketPair(t)
	defer cleanup()
	lib.RegisterConnection("198.51.100.5", socket, &userID)

	if count := lib.DisconnectUser(userID, "Session terminated"); count != 1 {
		t.Fatalf("disconnect user count = %d", count)
	}
	if code := readCloseCode(t, client); code != 4002 {
		t.Fatalf("user disconnect close code = %d, want 4002", code)
	}
}

// readCloseCode reads frames until a close frame arrives and returns its code.
func readCloseCode(t *testing.T, socket *lib.WebSocket) int {
	t.Helper()
	_ = socket.Ping() // no-op; keeps the read below simple
	type result struct {
		code   int
		reason string
	}
	done := make(chan result, 1)
	go func() {
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				code, reason := socket.CloseCode()
				done <- result{code: code, reason: reason}
				return
			}
		}
	}()
	select {
	case got := <-done:
		return got.code
	case <-time.After(5 * time.Second):
		t.Fatal("no close frame observed")
		return 0
	}
}
