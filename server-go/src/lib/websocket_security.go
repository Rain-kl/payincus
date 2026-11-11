// Port of server/src/lib/websocket-security.ts.
//
// Process-local connection accounting for the WebSocket surfaces (terminal and
// SPICE console). The backend runs as exactly one Node/Go process (AGENTS.md,
// WAVE_D_SPEC §0.1), so in-process maps are the correct parity choice; a shared
// store would silently change the semantics.
//
// The origin rules are a byte-for-byte port of validateWebSocketOrigin plus
// lib/origin-config.ts (GetAllowedWebSocketOrigins): the Origin header is
// normalised to scheme://host[:port] and must be a member of the configured list
// (CORS_ORIGINS / FRONTEND_URL / ADMIN_FRONTEND_URL, plus the fixed localhost
// entries outside production). There is NO wildcard or suffix matching: an
// unknown subdomain of an allowed host is rejected, exactly like
// `allowedOrigins.includes(origin)` in the TypeScript.
//
// A missing Origin header is accepted outside production and rejected with
// "Missing Origin header" in production, matching `process.env.NODE_ENV`.
package lib

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WebSocket connection limits (WS_LIMITS in the TypeScript).
const (
	WebSocketMaxConnectionsPerIP   = 10
	WebSocketMaxConnectionsPerUser = 5
	WebSocketConnectionTimeout     = 30 * time.Second
	WebSocketHeartbeatInterval     = 30 * time.Second
	WebSocketHeartbeatTimeout      = 10 * time.Second
)

// WebSocketOriginCheck mirrors the `{ allowed, origin?, reason? }` result.
type WebSocketOriginCheck struct {
	Allowed bool
	Origin  string
	Reason  string
}

// normalizeWebSocketOrigin mirrors the TypeScript's `new URL(origin).origin`
// exactly, which lib.NormalizeOrigin (the shared CORS helper) does not: WHATWG
// URL lowercases the hostname and drops the scheme's default port. `ws` compares
// these serialized origins, so "https://PAY.example.com:443" and
// "https://pay.example.com" are the same origin to Node and must be here too.
func normalizeWebSocketOrigin(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := parsed.Port()
	switch {
	case scheme == "https" && port == "443",
		scheme == "http" && port == "80",
		scheme == "ws" && port == "80",
		scheme == "wss" && port == "443":
		port = ""
	}
	if port != "" {
		if _, err := strconv.Atoi(port); err != nil {
			return ""
		}
		return scheme + "://" + host + ":" + port
	}
	return scheme + "://" + host
}

// ValidateWebSocketOrigin mirrors validateWebSocketOrigin(request).
func ValidateWebSocketOrigin(request *http.Request) WebSocketOriginCheck {
	rawOrigin := ""
	if request != nil {
		rawOrigin = request.Header.Get("Origin")
	}

	if rawOrigin == "" {
		if IsProductionEnv() {
			return WebSocketOriginCheck{Allowed: false, Reason: "Missing Origin header"}
		}
		return WebSocketOriginCheck{Allowed: true}
	}

	origin := normalizeWebSocketOrigin(rawOrigin)
	if origin == "" {
		return WebSocketOriginCheck{Allowed: false, Reason: "Invalid Origin header"}
	}

	for _, allowed := range GetAllowedWebSocketOrigins() {
		// The allow list comes from arbitrary env values; normalize each entry with
		// the same WHATWG rules so a configured "https://Pay.example.com:443" still
		// matches "https://pay.example.com".
		if normalizeWebSocketOrigin(allowed) == origin || allowed == origin {
			return WebSocketOriginCheck{Allowed: true, Origin: origin}
		}
	}
	return WebSocketOriginCheck{Allowed: false, Origin: origin, Reason: "Origin is not allowed"}
}

// WebSocketConnectionLimit mirrors `{ allowed, current, limit }`.
type WebSocketConnectionLimit struct {
	Allowed bool
	Current int
	Limit   int
}

var webSocketSecurityStore = struct {
	sync.Mutex
	ipConnections   map[string]map[*WebSocket]struct{}
	userConnections map[int32]map[*WebSocket]struct{}
}{
	ipConnections:   map[string]map[*WebSocket]struct{}{},
	userConnections: map[int32]map[*WebSocket]struct{}{},
}

// CheckIPConnectionLimit mirrors checkIPConnectionLimit(ip).
func CheckIPConnectionLimit(ip string) WebSocketConnectionLimit {
	webSocketSecurityStore.Lock()
	defer webSocketSecurityStore.Unlock()
	current := len(webSocketSecurityStore.ipConnections[ip])
	return WebSocketConnectionLimit{
		Allowed: current < WebSocketMaxConnectionsPerIP,
		Current: current,
		Limit:   WebSocketMaxConnectionsPerIP,
	}
}

// CheckUserConnectionLimit mirrors checkUserConnectionLimit(userId).
func CheckUserConnectionLimit(userID int32) WebSocketConnectionLimit {
	webSocketSecurityStore.Lock()
	defer webSocketSecurityStore.Unlock()
	current := len(webSocketSecurityStore.userConnections[userID])
	return WebSocketConnectionLimit{
		Allowed: current < WebSocketMaxConnectionsPerUser,
		Current: current,
		Limit:   WebSocketMaxConnectionsPerUser,
	}
}

// RegisterConnection mirrors registerConnection(ip, socket, userId?): it books the
// socket against the caller IP (and user, when known) and releases both slots
// automatically when the socket closes.
func RegisterConnection(ip string, socket *WebSocket, userID *int32) {
	if socket == nil {
		return
	}

	webSocketSecurityStore.Lock()
	if webSocketSecurityStore.ipConnections[ip] == nil {
		webSocketSecurityStore.ipConnections[ip] = map[*WebSocket]struct{}{}
	}
	webSocketSecurityStore.ipConnections[ip][socket] = struct{}{}
	if userID != nil {
		if webSocketSecurityStore.userConnections[*userID] == nil {
			webSocketSecurityStore.userConnections[*userID] = map[*WebSocket]struct{}{}
		}
		webSocketSecurityStore.userConnections[*userID][socket] = struct{}{}
	}
	webSocketSecurityStore.Unlock()

	// `socket.on('close', ...)` in the TypeScript.
	socket.OnClose(func() {
		UnregisterConnection(ip, socket, userID)
	})
}

// UnregisterConnection mirrors unregisterConnection(ip, socket, userId?).
func UnregisterConnection(ip string, socket *WebSocket, userID *int32) {
	webSocketSecurityStore.Lock()
	defer webSocketSecurityStore.Unlock()

	if connections := webSocketSecurityStore.ipConnections[ip]; connections != nil {
		delete(connections, socket)
		if len(connections) == 0 {
			delete(webSocketSecurityStore.ipConnections, ip)
		}
	}
	if userID != nil {
		if connections := webSocketSecurityStore.userConnections[*userID]; connections != nil {
			delete(connections, socket)
			if len(connections) == 0 {
				delete(webSocketSecurityStore.userConnections, *userID)
			}
		}
	}
}

// SetAuthenticationTimeout mirrors setAuthenticationTimeout(socket, timeoutMs):
// an unauthenticated socket is closed with 4001 after the timeout.
func SetAuthenticationTimeout(socket *WebSocket, timeout time.Duration) *time.Timer {
	if timeout <= 0 {
		timeout = WebSocketConnectionTimeout
	}
	return time.AfterFunc(timeout, func() {
		if socket.IsOpen() {
			socket.Close(4001, "Authentication timeout")
		}
	})
}

// WebSocketIPConnectionCount mirrors one entry of getConnectionStats().topIPs.
type WebSocketIPConnectionCount struct {
	IP    string
	Count int
}

// WebSocketConnectionStats mirrors getConnectionStats().
type WebSocketConnectionStats struct {
	TotalIPs         int
	TotalConnections int
	TotalUsers       int
	TopIPs           []WebSocketIPConnectionCount
}

// GetConnectionStats mirrors getConnectionStats(): the ten busiest IPs first.
func GetConnectionStats() WebSocketConnectionStats {
	webSocketSecurityStore.Lock()
	defer webSocketSecurityStore.Unlock()

	stats := WebSocketConnectionStats{TotalIPs: len(webSocketSecurityStore.ipConnections)}
	for ip, connections := range webSocketSecurityStore.ipConnections {
		stats.TotalConnections += len(connections)
		stats.TopIPs = append(stats.TopIPs, WebSocketIPConnectionCount{IP: ip, Count: len(connections)})
	}
	stats.TotalUsers = len(webSocketSecurityStore.userConnections)

	// Node sorts descending and keeps the first ten; ties keep map order, which is
	// not observable for the counters. Sort by count then IP for determinism.
	sort.SliceStable(stats.TopIPs, func(i, j int) bool {
		if stats.TopIPs[i].Count != stats.TopIPs[j].Count {
			return stats.TopIPs[i].Count > stats.TopIPs[j].Count
		}
		return stats.TopIPs[i].IP < stats.TopIPs[j].IP
	})
	if len(stats.TopIPs) > 10 {
		stats.TopIPs = stats.TopIPs[:10]
	}
	return stats
}

// DisconnectIP mirrors disconnectIP(ip, reason): returns how many sockets were
// asked to close (4003).
func DisconnectIP(ip string, reason string) int {
	if reason == "" {
		reason = "Disconnected by admin"
	}
	webSocketSecurityStore.Lock()
	sockets := make([]*WebSocket, 0, len(webSocketSecurityStore.ipConnections[ip]))
	for socket := range webSocketSecurityStore.ipConnections[ip] {
		sockets = append(sockets, socket)
	}
	webSocketSecurityStore.Unlock()

	count := 0
	for _, socket := range sockets {
		if socket.IsOpen() {
			socket.Close(4003, reason)
			count++
		}
	}
	return count
}

// DisconnectUser mirrors disconnectUser(userId, reason): returns how many sockets
// were asked to close (4002).
func DisconnectUser(userID int32, reason string) int {
	if reason == "" {
		reason = "Session terminated"
	}
	webSocketSecurityStore.Lock()
	sockets := make([]*WebSocket, 0, len(webSocketSecurityStore.userConnections[userID]))
	for socket := range webSocketSecurityStore.userConnections[userID] {
		sockets = append(sockets, socket)
	}
	webSocketSecurityStore.Unlock()

	count := 0
	for _, socket := range sockets {
		if socket.IsOpen() {
			socket.Close(4002, reason)
			count++
		}
	}
	return count
}

// ResetWebSocketSecurityState clears the process-local registry. Tests only.
func ResetWebSocketSecurityState() {
	webSocketSecurityStore.Lock()
	webSocketSecurityStore.ipConnections = map[string]map[*WebSocket]struct{}{}
	webSocketSecurityStore.userConnections = map[int32]map[*WebSocket]struct{}{}
	webSocketSecurityStore.Unlock()
}
