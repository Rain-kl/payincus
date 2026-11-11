// Port of server/src/routes/admin-incus-ui.ts (723 lines) — the admin Incus Web UI
// tunnel entry point. app.ts registers the module WITHOUT a prefix, so every path
// below is absolute.
//
// Route surface (verified against the TypeScript; the migration checklist calls
// this "2 routes" and mentions `GET /api/admin/hosts/:hostId/incus-ui`, which does
// not exist in the source. The real surface is one ticket endpoint plus a proxy
// family, split by transport):
//
//	POST   /api/admin/hosts/:hostId/incus-ui-ticket   authenticateAdmin
//	GET    /api/admin/hosts/:hostId/incus-proxy/*     authenticateAdmin,
//	                                                  or ?__incudal_ticket= on a
//	                                                  bodyless GET/HEAD (downloads)
//	HEAD   /api/admin/hosts/:hostId/incus-proxy/*     (Fastify exposeHeadRoutes)
//	POST   /api/admin/hosts/:hostId/incus-proxy/*     authenticateAdmin
//	PUT    /api/admin/hosts/:hostId/incus-proxy/*     authenticateAdmin
//	PATCH  /api/admin/hosts/:hostId/incus-proxy/*     authenticateAdmin
//	DELETE /api/admin/hosts/:hostId/incus-proxy/*     authenticateAdmin
//	GET    /api/ws/hosts/:hostId/incus-proxy/*        ticket via
//	                                                  `Sec-WebSocket-Protocol:
//	                                                  incudal-incus-ui.<ticket>`
//	                                                  or ?ticket=
//
// The Incus SPA (incus-ui/src/lib/payincus-bridge.ts) is the only client: it fetches
// the ticket from the ticket endpoint, rewrites every /1.0 REST call to
// /api/admin/hosts/<id>/incus-proxy/1.0/* with a Bearer header, appends the ticket
// as a subprotocol to /1.0/events + terminal + SPICE WebSockets, and puts
// ?__incudal_ticket= on native <a> downloads.
//
// ============ auth surface (WAVE_D_SPEC §0.2) ============
// Only the Web JWT surface reaches these routes: the ticket endpoint and the proxy
// mutations require a Bearer access token (401/403 before anything else), the proxy
// GET/HEAD also accepts the short-lived incus-ui ticket, and the WebSocket route
// accepts ONLY the ticket (a browser cannot set Authorization on a WebSocket).
// No public-API (pat_/poa_) or host-agent HMAC credential is accepted anywhere here.
//
// ============ one deliberate ordering difference (required by the task spec) =====
// The TypeScript WebSocket route upgrades first and then closes with 4001/4003/4004
// for authentication, origin, path, host and tunnel failures. This port authenticates
// (and validates origin/path/host/tunnel/certificate) BEFORE the 101 handshake, so an
// unauthorised caller never receives an upgrade. The Node close codes map onto HTTP
// statuses 1:1 so nothing is lost:
//
//	4000 'Invalid host ID'                  -> 400 INVALID_HOST_ID
//	4003 'Origin not allowed'               -> 403 FORBIDDEN_ORIGIN
//	4001 'Authentication ticket required'    -> 401 UNAUTHORIZED
//	4001 'Invalid or expired ticket'        -> 401 UNAUTHORIZED
//	4001 'Session expired'                  -> 401 SESSION_INVALIDATED
//	4001 'Unauthorized'                     -> 401 UNAUTHORIZED
//	4003 'Admin privileges required'        -> 403 ADMIN_REQUIRED
//	4008 'WebSocket connection limit...'    -> 429 WS_LIMIT_EXCEEDED
//	4003 'Forbidden Incus proxy path'       -> 403 FORBIDDEN_PROXY_PATH
//	4004 'Host not found'                   -> 404 HOST_NOT_FOUND
//	4004 'Host tunnel offline'              -> 503 TUNNEL_OFFLINE
//	4000 'Host certificate unavailable'     -> 502 INCUS_CERT_UNAVAILABLE
//
// After the upgrade everything matches the TypeScript again: upstream close codes are
// forwarded through toSendableCloseCode (an unsendable code becomes an EMPTY close
// frame, i.e. 1005 for the browser, exactly like `socket.close()`), messages are
// buffered up to 128 while the upstream handshake is in flight, and either side
// closing tears the other one down.
//
// ============ body parsing on the proxy routes ============
// The TypeScript registers the proxy routes in a nested plugin that calls
// removeAllContentTypeParsers() + addContentTypeParser('*', parseAs: 'buffer',
// bodyLimit: 100 MiB). Every content type is therefore accepted verbatim — binary
// image uploads, empty JSON bodies and malformed JSON all reach the handler.
// middleware.AuthenticateAdmin would apply Fastify's *default* JSON rules (400 on
// an empty JSON body, 415 on application/octet-stream) and break real uploads, so
// the proxy family authenticates through adminIncusUIAuthenticateBearer, a
// step-by-step copy of the decorator, and reads the raw body itself.
//
// Process-local state (AGENTS.md / WAVE_D_SPEC §0.1): the per-host dispatcher cache
// and the per-user WebSocket counter both live in this process, exactly like the
// TypeScript Maps. A second backend replica would double-count and re-dial, so do
// not scale out without an owner-approved architecture change.
package routes

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

const (
	// incusUISubprotocolPrefix mirrors INCUS_UI_SUBPROTOCOL_PREFIX.
	incusUISubprotocolPrefix = "incudal-incus-ui."
	// incusUIMaxWSConnectionsPerUser mirrors INCUS_UI_MAX_WS_PER_USER. The Incus UI
	// page holds events + terminal(data/control) + console(data/control) at once, so
	// the generic 5/user limit would close the events socket right after the 101.
	incusUIMaxWSConnectionsPerUser = 64
	// incusUIWSMaxPayload mirrors the 16 MiB maxPayload of the upstream `ws` client.
	incusUIWSMaxPayload = 16 * 1024 * 1024
	// incusUIProxyBodyLimit mirrors the 100 MiB bodyLimit of the '*' buffer parser.
	incusUIProxyBodyLimit = 100 * 1024 * 1024
	// incusUIWSHandshakeTimeout mirrors `handshakeTimeout: 15_000`.
	incusUIWSHandshakeTimeout = 15 * time.Second
	// incusUIUpstreamHeadersTimeout mirrors `headersTimeout: 120_000`.
	incusUIUpstreamHeadersTimeout = 120 * time.Second
	// incusUIMaxBufferedClientMessages mirrors `pendingClientMessages.length < 128`.
	incusUIMaxBufferedClientMessages = 128
	// incusUIMaxTicketSubprotocolLength mirrors `candidate.length <= 128`.
	incusUIMaxTicketSubprotocolLength = 128
	// incusUIUpstreamHostPort is the URL the proxy addresses; the tunnel stream is
	// what actually reaches the node (target_host:target_port).
	incusUIUpstreamHostPort = "127.0.0.1:8443"
	// incusUITicketExpiresInSeconds is the advertised lifetime (5 min TTL).
	incusUITicketExpiresInSeconds = 300
)

// incusUIHopByHopHeaders mirrors HOP_BY_HOP_HEADERS.
var incusUIHopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
	"host":                true,
	"authorization":       true,
	"cookie":              true,
	"content-length":      true,
}

// incusUIStrippedResponseHeaders mirrors STRIPPED_RESPONSE_HEADERS.
var incusUIStrippedResponseHeaders = map[string]bool{
	"connection":                true,
	"keep-alive":                true,
	"transfer-encoding":         true,
	"content-security-policy":   true,
	"x-frame-options":           true,
	"strict-transport-security": true,
}

// incusUIProtocolTokenPattern mirrors /^[a-zA-Z0-9._-]+$/ for forwarded protocols.
var incusUIProtocolTokenPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// incusUIMultiSlashPattern mirrors `.replace(/\/+/g, '/')`.
var incusUIMultiSlashPattern = regexp.MustCompile(`/{2,}`)

// ==================== test seams ====================

// These seams exist so the route-level behaviour (ticket binding, upstream close
// propagation) can be unit-tested without a database or a connected node agent.
// Production always uses the real implementations below.
var (
	adminIncusUIFindHost = func(ctx context.Context, id int32) (*db.HostsRecord, error) {
		return db.HostsGetByID(ctx, id)
	}
	adminIncusUIFindUser = func(ctx context.Context, id int32) (*db.UserRecord, error) {
		return db.FindUserById(ctx, id)
	}
	adminIncusUITunnelOnline = func(hostID int32) bool {
		return lib.TunnelManagerInstance().IsTunnelOnline(hostID)
	}
	adminIncusUIDispatcherForHost = func(host *db.HostsRecord) (*adminIncusUITunnelDispatcher, error) {
		return adminIncusUIGetOrCreateDispatcher(host)
	}
	adminIncusUIDialWebSocketUpstream = func(
		ctx context.Context,
		dispatcher *adminIncusUITunnelDispatcher,
		targetPath string,
		subprotocols []string,
	) (*lib.WebSocket, error) {
		return adminIncusUIDialUpstreamWebSocket(ctx, dispatcher, targetPath, subprotocols)
	}
	// adminIncusUILogMutation is the `void createLog(...)` call: fire-and-forget, and
	// a seam so the relay tests never need a database.
	adminIncusUILogMutation = func(ctx context.Context, userID int32, hostName string, hostID int32, method, path string) {
		id := int64(userID)
		content := fmt.Sprintf("IncusUI 节点操作: %s (#%d) | %s %s", hostName, hostID, method, path)
		db.CreateLog(ctx, &id, db.LogModuleHost, "host.incus_ui_mutation", content, db.LogResultSuccess, db.CreateLogOptions{})
	}
)

// ==================== identity ====================

// adminIncusUIUser is the slice of request.user the TypeScript reads.
type adminIncusUIUser struct {
	ID       int32
	Username string
	Role     string
}

// adminIncusUIRespondInternal mirrors the error handler's 500 fallback. Production
// masks the message; the development build would echo error.message, which is not
// reproducible for arbitrary failures (the conformance harness never hits this).
func adminIncusUIRespondInternal(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// adminIncusUIBearerToken mirrors `request.headers.authorization` handling: only
// the "Bearer " scheme yields a token, and an empty header is "no credentials".
func adminIncusUIBearerToken(c *gin.Context) string {
	header := c.GetHeader("Authorization")
	if len(header) > 7 && strings.EqualFold(header[:7], "Bearer ") {
		return header[7:]
	}
	return ""
}

// adminIncusUIQueryParam mirrors Fastify's query parsing for a single string value:
// a duplicated key is an ARRAY in Fastify (`typeof !== 'string'`), so it does not
// count as a ticket.
func adminIncusUIQueryParam(c *gin.Context, name string) string {
	values, ok := c.Request.URL.Query()[name]
	if !ok || len(values) != 1 {
		return ""
	}
	return values[0]
}

// adminIncusUISessionIDPtr mirrors passing `user.sid` (possibly undefined).
func adminIncusUISessionIDPtr(sessionID string) *string {
	if sessionID == "" {
		return nil
	}
	value := sessionID
	return &value
}

// ==================== ticket endpoint ====================

// adminIncusUITicketHandler mirrors POST /api/admin/hosts/:hostId/incus-ui-ticket.
//
// authenticateAdmin runs as the route middleware, which preserves Fastify's order:
// auth (401/403) -> body parse (400) -> handler (400 INVALID_HOST_ID, 404, 503).
func adminIncusUITicketHandler(c *gin.Context) {
	hostID, ok := lib.ParsePositiveRouteID(c.Param("hostId"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid host ID", "code": "INVALID_HOST_ID"})
		return
	}

	claims := middleware.CurrentUser(c)
	if claims == nil {
		// Unreachable after middleware.AuthenticateAdmin; kept because the TypeScript
		// handler re-checks the role itself.
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized", "code": "UNAUTHORIZED"})
		return
	}
	if claims.Role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Admin privileges required", "code": "ADMIN_REQUIRED"})
		return
	}
	if claims.IssuedAt == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized", "code": "UNAUTHORIZED"})
		return
	}

	host, err := adminIncusUIFindHost(c.Request.Context(), int32(hostID))
	if err != nil {
		adminIncusUIRespondInternal(c)
		return
	}
	if host == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Host not found", "code": "HOST_NOT_FOUND"})
		return
	}

	if !adminIncusUITunnelOnline(host.ID) {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "宿主机反向隧道未连接（Agent 离线），无法访问 Incus UI",
			"code":  "TUNNEL_OFFLINE",
		})
		return
	}

	ticket := lib.GenerateIncusUIAccessTicket(claims.ID, host.ID, claims.IssuedAt, adminIncusUISessionIDPtr(claims.SID))
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"ticket": ticket, "expiresIn": incusUITicketExpiresInSeconds})
}

// ==================== proxy authentication ====================

// adminIncusUIAuthenticateBearer reproduces `await fastify.authenticateAdmin(request,
// reply)` for the proxy family, whose permissive body parser means the JSON body
// rules must NOT run (see the file header). The steps and response bodies are the
// decorator's, in order:
//
//	jwtVerify failure            -> 401 {error:'Unauthorized'}
//	missing id/iat               -> 401 {error:'Unauthorized', code:'UNAUTHORIZED'}
//	token invalidated            -> 401 {error:'Session expired', code:'SESSION_INVALIDATED'}
//	user row missing             -> 401 {error:'Unauthorized', code:'UNAUTHORIZED'}
//	status != active             -> 401 {error:'Account banned', code:'ACCOUNT_BANNED'}
//	role != admin                -> 403 {error:'Admin privileges required', code:'ADMIN_REQUIRED'}
//
// Any database error inside the decorator is caught by its `catch` and also becomes
// 401 {error:'Unauthorized'}, so this copy does the same.
func adminIncusUIAuthenticateBearer(c *gin.Context) *adminIncusUIUser {
	token := adminIncusUIBearerToken(c)
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return nil
	}
	claims, err := lib.VerifyAccessToken(token)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return nil
	}
	if claims.ID == 0 || claims.IssuedAt == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized", "code": "UNAUTHORIZED"})
		return nil
	}
	if lib.IsAccessTokenInvalidated(c.Request.Context(), claims.ID, claims.IssuedAt, claims.SID) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Session expired", "code": "SESSION_INVALIDATED"})
		return nil
	}
	user, err := adminIncusUIFindUser(c.Request.Context(), claims.ID)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return nil
	}
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized", "code": "UNAUTHORIZED"})
		return nil
	}
	if string(user.Status) != "active" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Account banned", "code": "ACCOUNT_BANNED"})
		return nil
	}
	if string(user.Role) != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Admin privileges required", "code": "ADMIN_REQUIRED"})
		return nil
	}
	return &adminIncusUIUser{ID: user.ID, Username: user.Username, Role: string(user.Role)}
}

// adminIncusUIAuthenticateTicket mirrors the ticket branch of
// authenticateAdminProxyRequest (and of the WebSocket handler).
func adminIncusUIAuthenticateTicket(c *gin.Context, hostID int32) *adminIncusUIUser {
	ticket := adminIncusUIQueryParam(c, "__incudal_ticket")
	if ticket == "" {
		// Only the WebSocket route reads the ticket from elsewhere; callers that need
		// a specific source pass it through adminIncusUIVerifyTicket.
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "Invalid or expired Incus UI ticket",
			"code":  "UNAUTHORIZED",
		})
		return nil
	}
	return adminIncusUIVerifyTicket(c, ticket, hostID, "Invalid or expired Incus UI ticket")
}

// adminIncusUIVerifyTicket mirrors the shared ticket verification block: verify,
// token-invalidation check, user load, admin check. invalidTicketMessage is the
// proxy's "Invalid or expired Incus UI ticket" or the WebSocket route's
// "Invalid or expired ticket" (the TypeScript reason string differs per transport).
func adminIncusUIVerifyTicket(c *gin.Context, ticket string, hostID int32, invalidTicketMessage string) *adminIncusUIUser {
	result := lib.VerifyIncusUIAccessTicket(ticket, &hostID)
	if !result.Valid || result.UserID == 0 || result.IssuedAt == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": invalidTicketMessage, "code": "UNAUTHORIZED"})
		return nil
	}

	sessionID := ""
	if result.SessionID != nil {
		sessionID = *result.SessionID
	}
	if lib.IsAccessTokenInvalidated(c.Request.Context(), result.UserID, result.IssuedAt, sessionID) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Session expired", "code": "SESSION_INVALIDATED"})
		return nil
	}

	user, err := adminIncusUIFindUser(c.Request.Context(), result.UserID)
	if err != nil {
		adminIncusUIRespondInternal(c)
		return nil
	}
	if user == nil || string(user.Status) != "active" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized", "code": "UNAUTHORIZED"})
		return nil
	}
	if string(user.Role) != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Admin privileges required", "code": "ADMIN_REQUIRED"})
		return nil
	}
	return &adminIncusUIUser{ID: user.ID, Username: user.Username, Role: string(user.Role)}
}

// ==================== path normalization ====================

// adminIncusUINormalizeProxyTargetPath mirrors normalizeIncusProxyTargetPath.
// An empty return means "reject with 403 FORBIDDEN_PROXY_PATH".
func adminIncusUINormalizeProxyTargetPath(wildcard, rawSearch string) string {
	trimmed := strings.TrimLeft(wildcard, "/")
	if trimmed == "" {
		return ""
	}

	decoded, err := url.PathUnescape(trimmed)
	if err != nil {
		return ""
	}

	if strings.Contains(trimmed, "..") || strings.Contains(decoded, "..") ||
		strings.Contains(trimmed, "\\") || strings.Contains(decoded, "\\") ||
		strings.Contains(decoded, "\x00") {
		return ""
	}

	cleanPath := "/" + trimmed
	cleanPath = incusUIMultiSlashPattern.ReplaceAllString(cleanPath, "/")
	if cleanPath != "/1.0" && !strings.HasPrefix(cleanPath, "/1.0/") {
		return ""
	}
	// /1.0/payincus is the node agent's private control extension: never proxied.
	if cleanPath == "/1.0/payincus" || strings.HasPrefix(cleanPath, "/1.0/payincus/") {
		return ""
	}

	return cleanPath + adminIncusUISanitizeProxyQueryString(rawSearch)
}

// adminIncusUISanitizeProxyQueryString mirrors sanitizeProxyQueryString: the access
// ticket must never reach incusd, but the remaining query text is forwarded verbatim.
func adminIncusUISanitizeProxyQueryString(rawSearch string) string {
	if rawSearch == "" || rawSearch == "?" {
		return ""
	}
	raw := strings.TrimPrefix(rawSearch, "?")
	kept := make([]string, 0, 8)
	for _, segment := range strings.Split(raw, "&") {
		if segment == "" {
			continue
		}
		key := segment
		if index := strings.IndexByte(segment, '='); index >= 0 {
			key = segment[:index]
		}
		if key == "__incudal_ticket" || key == "ticket" {
			continue
		}
		kept = append(kept, segment)
	}
	if len(kept) == 0 {
		return ""
	}
	return "?" + strings.Join(kept, "&")
}

// ==================== subprotocol ticket extraction ====================

// adminIncusUIExtractTicketFromSubprotocols mirrors
// extractIncusUiTicketFromSubprotocols: the LAST valid `incudal-incus-ui.<ticket>`
// protocol wins, and every other RFC 6455 token is forwarded upstream.
func adminIncusUIExtractTicketFromSubprotocols(headerValue string) (string, []string) {
	ticket := ""
	forward := make([]string, 0, 4)
	for _, raw := range strings.Split(headerValue, ",") {
		part := strings.TrimSpace(raw)
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, incusUISubprotocolPrefix) {
			candidate := strings.TrimSpace(part[len(incusUISubprotocolPrefix):])
			if candidate != "" && len(candidate) <= incusUIMaxTicketSubprotocolLength {
				ticket = candidate
			}
			continue
		}
		if incusUIProtocolTokenPattern.MatchString(part) {
			forward = append(forward, part)
		}
	}
	return ticket, forward
}

// adminIncusUIToSendableCloseCode mirrors toSendableCloseCode: ws may only put
// 1000-1003, 1007-1014 and 3000-4999 on the wire.
func adminIncusUIToSendableCloseCode(code int) (int, bool) {
	switch {
	case code == 1000 || code == 1001 || code == 1002 || code == 1003:
		return code, true
	case code >= 1007 && code <= 1014:
		return code, true
	case code >= 3000 && code <= 4999:
		return code, true
	}
	return 0, false
}

// ==================== tunnel dispatcher cache ====================

// adminIncusUITunnelDispatcher mirrors CachedTunnelDispatcher: one mTLS HTTP
// transport (undici Agent in Node) per host, reused until the certificate paths,
// target host or target port change.
type adminIncusUITunnelDispatcher struct {
	client    *http.Client
	transport *http.Transport
	hostID    int32
	certPath  string
	keyPath   string
	// upstreamHostPort is the URL host the proxy addresses. Production always uses
	// incusUIUpstreamHostPort ("127.0.0.1:8443") because the tunnel decides the real
	// destination; the field exists so a test can point the proxy at a local server.
	upstreamHostPort string
	targetHost       string
	targetPort       int
	certificate      tls.Certificate
}

var adminIncusUIDispatchers = struct {
	sync.Mutex
	entries map[int32]*adminIncusUITunnelDispatcher
}{entries: make(map[int32]*adminIncusUITunnelDispatcher)}

// adminIncusUIReadCertificateFile reproduces readFileSync's error text for ENOENT,
// because that message is forwarded verbatim in the 502 body.
func adminIncusUIReadCertificateFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("ENOENT: no such file or directory, open '%s'", path)
		}
		return nil, err
	}
	return data, nil
}

// adminIncusUIGetOrCreateDispatcher mirrors getOrCreateTunnelDispatcher.
//
// Like the TypeScript it reads both files BEFORE consulting the cache, so a node
// whose certificate was deleted starts failing immediately instead of serving from
// a stale cached agent.
func adminIncusUIGetOrCreateDispatcher(host *db.HostsRecord) (*adminIncusUITunnelDispatcher, error) {
	certPath, keyPath := lib.ResolveCertificatePair(host.CertPath, host.KeyPath)
	if certPath == nil || keyPath == nil || *certPath == "" || *keyPath == "" {
		return nil, errors.New("宿主机未配置有效的客户端证书或私钥")
	}

	certPEM, err := adminIncusUIReadCertificateFile(*certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := adminIncusUIReadCertificateFile(*keyPath)
	if err != nil {
		return nil, err
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}

	targetHost := host.TargetHost
	if targetHost == "" {
		targetHost = "127.0.0.1"
	}
	targetPort := int(host.TargetPort)
	if targetPort == 0 {
		targetPort = 8443
	}

	adminIncusUIDispatchers.Lock()
	defer adminIncusUIDispatchers.Unlock()

	if existing := adminIncusUIDispatchers.entries[host.ID]; existing != nil &&
		existing.certPath == *certPath && existing.keyPath == *keyPath &&
		existing.targetHost == targetHost && existing.targetPort == targetPort {
		return existing, nil
	}

	if existing := adminIncusUIDispatchers.entries[host.ID]; existing != nil {
		existing.transport.CloseIdleConnections()
		delete(adminIncusUIDispatchers.entries, host.ID)
	}

	hostID := host.ID
	transport := &http.Transport{
		// The node is only reachable through its agent tunnel: replace the TCP dial
		// with a duplex stream to the node's local Incus endpoint and let the
		// transport run TLS on top (tls.connect({ socket, cert, key }) in Node).
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return lib.TunnelManagerInstance().CreateDuplexStream(hostID, targetHost, targetPort)
		},
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{certificate},
			// Incus uses a self-signed certificate and the client cert is the credential:
			// `rejectUnauthorized: false` in the TypeScript.
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
			// undici speaks HTTP/1.1 only; do not let the transport negotiate h2.
			NextProtos: []string{"http/1.1"},
		},
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: incusUIUpstreamHeadersTimeout,
		DisableCompression:    false,
	}
	dispatcher := &adminIncusUITunnelDispatcher{
		client:           &http.Client{Transport: transport},
		transport:        transport,
		hostID:           hostID,
		certPath:         *certPath,
		keyPath:          *keyPath,
		upstreamHostPort: incusUIUpstreamHostPort,
		targetHost:       targetHost,
		targetPort:       targetPort,
		certificate:      certificate,
	}
	adminIncusUIDispatchers.entries[host.ID] = dispatcher
	return dispatcher, nil
}

// ==================== HTTP reverse proxy ====================

// adminIncusUISplitTargetPath splits "/1.0/x?a=b" into its path and raw query, at
// the FIRST '?' exactly like the string concatenation in the TypeScript.
func adminIncusUISplitTargetPath(targetPath string) (string, string) {
	if index := strings.IndexByte(targetPath, '?'); index >= 0 {
		return targetPath[:index], targetPath[index+1:]
	}
	return targetPath, ""
}

// adminIncusUIProxyHandler serves GET/HEAD/POST/PUT/PATCH/DELETE
// /api/admin/hosts/:hostId/incus-proxy/*.
//
// The auth ORDER follows the TypeScript exactly:
//   - bodyless GET/HEAD without an Authorization header may use ?__incudal_ticket=,
//     and the host id is validated BEFORE the ticket (that is where the Fastify
//     onRequest hook skips authenticateAdmin);
//   - every other shape authenticates first (401/403) and only then validates the
//     host id, with the raw body read in between (Fastify parses the body after
//     onRequest and before the handler, so a >100 MiB body is a 413 even for a bad
//     host id).
func adminIncusUIProxyHandler(c *gin.Context) {
	method := strings.ToUpper(c.Request.Method)
	bodyless := method == http.MethodGet || method == http.MethodHead

	hasBearer := c.GetHeader("Authorization") != ""
	hasQueryTicket := adminIncusUIQueryParam(c, "__incudal_ticket") != ""
	ticketDownload := bodyless && !hasBearer && hasQueryTicket

	var (
		user   *adminIncusUIUser
		hostID int64
		ok     bool
	)

	if ticketDownload {
		hostID, ok = lib.ParsePositiveRouteID(c.Param("hostId"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid host ID", "code": "INVALID_HOST_ID"})
			return
		}
		user = adminIncusUIAuthenticateTicket(c, int32(hostID))
		if user == nil {
			return
		}
	} else {
		user = adminIncusUIAuthenticateBearer(c)
		if user == nil {
			return
		}
	}

	var rawBody []byte
	if !bodyless {
		var aborted bool
		rawBody, aborted = adminIncusUIReadProxyBody(c)
		if aborted {
			return
		}
	}

	if !ticketDownload {
		hostID, ok = lib.ParsePositiveRouteID(c.Param("hostId"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid host ID", "code": "INVALID_HOST_ID"})
			return
		}
	}

	targetPath := adminIncusUINormalizeProxyTargetPath(c.Param("path"), adminIncusUIRawSearch(c))
	if targetPath == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden Incus proxy path", "code": "FORBIDDEN_PROXY_PATH"})
		return
	}

	host, err := adminIncusUIFindHost(c.Request.Context(), int32(hostID))
	if err != nil {
		adminIncusUIRespondInternal(c)
		return
	}
	if host == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Host not found", "code": "HOST_NOT_FOUND"})
		return
	}

	if !adminIncusUITunnelOnline(host.ID) {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "宿主机反向隧道未连接（Agent 离线），无法访问 Incus UI",
			"code":  "TUNNEL_OFFLINE",
		})
		return
	}

	dispatcher, err := adminIncusUIDispatcherForHost(host)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "code": "INCUS_CERT_UNAVAILABLE"})
		return
	}

	adminIncusUIForwardRequest(c, user, host, dispatcher, method, targetPath, rawBody)
}

// adminIncusUIRawSearch mirrors `rawUrl.slice(rawUrl.indexOf('?'))`.
func adminIncusUIRawSearch(c *gin.Context) string {
	rawQuery := c.Request.URL.RawQuery
	if rawQuery == "" {
		return ""
	}
	return "?" + rawQuery
}

// adminIncusUIReadProxyBody implements the '*' buffer parser: any content type, up
// to 100 MiB, and an empty body is valid.
func adminIncusUIReadProxyBody(c *gin.Context) ([]byte, bool) {
	if c.Request.ContentLength > incusUIProxyBodyLimit {
		middleware.AbortFastifyBody(c, http.StatusRequestEntityTooLarge, middleware.FastifyTooLargeMessage)
		return nil, true
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, incusUIProxyBodyLimit+1))
	if err != nil {
		// Fastify's raw body parser surfaces a truncated stream as an invalid
		// Content-Length; keep the same status/message rather than a 500.
		middleware.AbortFastifyBody(c, http.StatusBadRequest, "Request body size did not match Content-Length")
		return nil, true
	}
	if len(body) > incusUIProxyBodyLimit {
		middleware.AbortFastifyBody(c, http.StatusRequestEntityTooLarge, middleware.FastifyTooLargeMessage)
		return nil, true
	}
	return body, false
}

// adminIncusUIForwardRequest performs the upstream call and streams the response.
func adminIncusUIForwardRequest(
	c *gin.Context,
	user *adminIncusUIUser,
	host *db.HostsRecord,
	dispatcher *adminIncusUITunnelDispatcher,
	method string,
	targetPath string,
	rawBody []byte,
) {
	headers := make(http.Header, len(c.Request.Header))
	for name, values := range c.Request.Header {
		if incusUIHopByHopHeaders[strings.ToLower(name)] {
			continue
		}
		// The TypeScript joins repeated headers with ", ". Multi-line values are
		// legal on the wire but not identical, so join the same way.
		headers.Set(name, strings.Join(values, ", "))
	}

	hasBody := method != http.MethodGet && method != http.MethodHead && len(rawBody) > 0
	var reader io.Reader
	if hasBody {
		reader = bytes.NewReader(rawBody)
	}

	upstreamHostPort := dispatcher.upstreamHostPort
	if upstreamHostPort == "" {
		upstreamHostPort = incusUIUpstreamHostPort
	}
	pathPart, queryPart := adminIncusUISplitTargetPath(targetPath)
	targetURL := &url.URL{Scheme: "https", Host: upstreamHostPort, Path: pathPart}
	if queryPart != "" {
		targetURL.RawQuery = queryPart
	}

	request, err := http.NewRequestWithContext(c.Request.Context(), method, targetURL.String(), reader)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": "Failed to proxy request to host Incus daemon over tunnel",
			"code":  "INCUS_PROXY_ERROR",
		})
		return
	}
	request.Header = headers

	response, err := dispatcher.client.Do(request)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": "Failed to proxy request to host Incus daemon over tunnel",
			"code":  "INCUS_PROXY_ERROR",
		})
		return
	}
	defer func() { _ = response.Body.Close() }()

	if hasBody && response.StatusCode < 400 {
		// `void createLog(...).catch(() => {})`: fire and forget, never on the hot path.
		go adminIncusUILogMutation(context.WithoutCancel(c.Request.Context()), user.ID, host.Name, host.ID, method, pathPart)
	}

	for name, values := range response.Header {
		if incusUIStrippedResponseHeaders[strings.ToLower(name)] {
			continue
		}
		for _, value := range values {
			c.Writer.Header().Add(name, value)
		}
	}
	c.Status(response.StatusCode)
	if _, err := io.Copy(c.Writer, response.Body); err != nil {
		// The TypeScript destroys the hijacked raw response here so the client sees a
		// truncated body instead of a silently short one. Go has no equivalent without
		// panicking out of the handler; the copy simply stops.
		return
	}
}

// ==================== WebSocket reverse proxy ====================

var adminIncusUIConnections = struct {
	sync.Mutex
	byUser map[int32]int
}{byUser: make(map[int32]int)}

// adminIncusUIAcquireConnectionSlot mirrors the `>= INCUS_UI_MAX_WS_PER_USER` check
// and the increment. Doing both under one lock means a burst cannot over-admit the
// way the TypeScript's check-then-await-then-increment can.
func adminIncusUIAcquireConnectionSlot(userID int32) bool {
	adminIncusUIConnections.Lock()
	defer adminIncusUIConnections.Unlock()
	if adminIncusUIConnections.byUser[userID] >= incusUIMaxWSConnectionsPerUser {
		return false
	}
	adminIncusUIConnections.byUser[userID]++
	return true
}

// adminIncusUIReleaseConnectionSlot mirrors the decrement in the socket close handler.
func adminIncusUIReleaseConnectionSlot(userID int32) {
	adminIncusUIConnections.Lock()
	defer adminIncusUIConnections.Unlock()
	remaining := adminIncusUIConnections.byUser[userID] - 1
	if remaining > 0 {
		adminIncusUIConnections.byUser[userID] = remaining
		return
	}
	delete(adminIncusUIConnections.byUser, userID)
}

// adminIncusUIConnectionCount reports the live socket count for a user (tests).
func adminIncusUIConnectionCount(userID int32) int {
	adminIncusUIConnections.Lock()
	defer adminIncusUIConnections.Unlock()
	return adminIncusUIConnections.byUser[userID]
}

// adminIncusUIWebSocketHandler serves GET /api/ws/hosts/:hostId/incus-proxy/*.
func adminIncusUIWebSocketHandler(c *gin.Context) {
	if !lib.IsWebSocketUpgrade(c.Request) {
		// Measured against @fastify/websocket: a plain GET on a websocket route never
		// reaches the handler and answers 404 with an empty body.
		c.Status(http.StatusNotFound)
		return
	}

	hostID, ok := lib.ParsePositiveRouteID(c.Param("hostId"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid host ID", "code": "INVALID_HOST_ID"})
		return
	}

	if origin := lib.ValidateWebSocketOrigin(c.Request); !origin.Allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": "Origin not allowed", "code": "FORBIDDEN_ORIGIN"})
		return
	}

	subprotocols := strings.Join(c.Request.Header.Values("Sec-WebSocket-Protocol"), ",")
	ticket, forwardProtocols := adminIncusUIExtractTicketFromSubprotocols(subprotocols)
	if ticket == "" {
		ticket = adminIncusUIQueryParam(c, "ticket")
	}
	if ticket == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication ticket required", "code": "UNAUTHORIZED"})
		return
	}

	// The TypeScript closes with these reasons after the upgrade; the pre-upgrade
	// port answers the same text as an HTTP error body.
	user := adminIncusUIVerifyTicket(c, ticket, int32(hostID), "Invalid or expired ticket")
	if user == nil {
		return
	}

	if !adminIncusUIAcquireConnectionSlot(user.ID) {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "WebSocket connection limit exceeded",
			"code":  "WS_LIMIT_EXCEEDED",
		})
		return
	}
	slotHeld := true
	defer func() {
		if slotHeld {
			adminIncusUIReleaseConnectionSlot(user.ID)
		}
	}()

	targetPath := adminIncusUINormalizeProxyTargetPath(c.Param("path"), adminIncusUIRawSearch(c))
	if targetPath == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden Incus proxy path", "code": "FORBIDDEN_PROXY_PATH"})
		return
	}

	host, err := adminIncusUIFindHost(c.Request.Context(), int32(hostID))
	if err != nil {
		adminIncusUIRespondInternal(c)
		return
	}
	if host == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Host not found", "code": "HOST_NOT_FOUND"})
		return
	}
	if !adminIncusUITunnelOnline(host.ID) {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "宿主机反向隧道未连接（Agent 离线），无法访问 Incus UI",
			"code":  "TUNNEL_OFFLINE",
		})
		return
	}

	dispatcher, err := adminIncusUIDispatcherForHost(host)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "code": "INCUS_CERT_UNAVAILABLE"})
		return
	}

	// Everything from here on is post-101: the browser socket is the @fastify default
	// 1 MiB cap, the upstream client is the TypeScript's 16 MiB.
	socket, err := lib.UpgradeWebSocket(c.Writer, c.Request, &lib.WebSocketUpgradeOptions{
		MaxPayload: lib.DefaultWebSocketMaxPayload,
	})
	if err != nil {
		return
	}
	slotHeld = false // the bridge owns the slot from now on

	bridge := &adminIncusUIBridge{
		client:     socket,
		userID:     user.ID,
		finishedCh: make(chan struct{}),
	}
	go bridge.readClient()

	upstream, err := adminIncusUIDialWebSocketUpstream(c.Request.Context(), dispatcher, targetPath, forwardProtocols)
	if err != nil {
		bridge.shutdown()
		<-bridge.finishedCh
		return
	}
	if !bridge.attach(upstream) {
		<-bridge.finishedCh
		return
	}
	go bridge.readUpstream()
	<-bridge.finishedCh
}

// adminIncusUIDialUpstreamWebSocket mirrors `new WebSocket('wss://127.0.0.1:8443…')`
// with createConnection(): TLS with the node client certificate over a fresh tunnel
// duplex stream.
func adminIncusUIDialUpstreamWebSocket(
	ctx context.Context,
	dispatcher *adminIncusUITunnelDispatcher,
	targetPath string,
	subprotocols []string,
) (*lib.WebSocket, error) {
	hostID := dispatcher.hostID
	return lib.DialWebSocket(ctx, "wss://"+incusUIUpstreamHostPort+targetPath, lib.WebSocketDialOptions{
		TLSConfig: &tls.Config{
			Certificates:       []tls.Certificate{dispatcher.certificate},
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
			NextProtos:         []string{"http/1.1"},
		},
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return lib.TunnelManagerInstance().CreateDuplexStream(hostID, dispatcher.targetHost, dispatcher.targetPort)
		},
		HandshakeTimeout: incusUIWSHandshakeTimeout,
		MaxPayload:       incusUIWSMaxPayload,
		Subprotocols:     subprotocols,
	})
}

// ==================== relay bridge ====================

type adminIncusUIBufferedMessage struct {
	payload []byte
	binary  bool
}

// adminIncusUIBridge is the full-duplex relay between the browser socket and the
// node's Incus UI socket. It reproduces the TypeScript's wiring:
//
//   - client messages are buffered (max 128) until the upstream handshake finishes,
//     then flushed in order;
//   - after that every client message is forwarded when the upstream is OPEN;
//   - upstream messages are forwarded when the client is OPEN;
//   - an upstream close forwards the code through toSendableCloseCode, falling back
//     to an EMPTY close frame (browser sees 1005) exactly like socket.close();
//   - either side failing tears both down;
//   - the per-user connection slot is released exactly once.
type adminIncusUIBridge struct {
	client *lib.WebSocket
	userID int32

	mu         sync.Mutex
	pending    []adminIncusUIBufferedMessage
	upstream   *lib.WebSocket
	ready      bool
	finished   bool
	finishedCh chan struct{}

	shutdownOnce sync.Once
}

// readClient is the client→upstream pump. It owns the client socket's read side.
func (b *adminIncusUIBridge) readClient() {
	defer b.shutdown()
	for {
		payload, isBinary, err := b.client.ReadMessage()
		if err != nil {
			return
		}

		b.mu.Lock()
		if !b.ready {
			if len(b.pending) < incusUIMaxBufferedClientMessages {
				b.pending = append(b.pending, adminIncusUIBufferedMessage{payload: payload, binary: isBinary})
			}
			b.mu.Unlock()
			continue
		}
		upstream := b.upstream
		b.mu.Unlock()

		if upstream == nil || !upstream.IsOpen() {
			continue
		}
		if err := upstream.SendMessage(payload, isBinary); err != nil {
			return
		}
	}
}

// attach installs the upstream socket and flushes the buffered messages. It returns
// false when the bridge already shut down (the client closed while dialing).
func (b *adminIncusUIBridge) attach(upstream *lib.WebSocket) bool {
	b.mu.Lock()
	if b.finished || !b.client.IsOpen() {
		b.mu.Unlock()
		upstream.Terminate()
		b.shutdown()
		return false
	}
	pending := b.pending
	b.pending = nil
	b.upstream = upstream
	b.ready = true
	b.mu.Unlock()

	for _, message := range pending {
		if err := upstream.SendMessage(message.payload, message.binary); err != nil {
			b.shutdown()
			return false
		}
	}
	return true
}

// readUpstream is the upstream→client pump plus close-code propagation.
func (b *adminIncusUIBridge) readUpstream() {
	defer b.shutdown()
	for {
		payload, isBinary, err := b.upstream.ReadMessage()
		if err != nil {
			break
		}
		if b.client.IsOpen() {
			_ = b.client.SendMessage(payload, isBinary)
		}
	}

	code, _ := b.upstream.CloseCode()
	if !b.client.IsOpen() {
		return
	}
	if sendable, ok := adminIncusUIToSendableCloseCode(code); ok {
		b.client.Close(sendable, "")
		return
	}
	// 1005/1006/1015/out-of-range: ws never puts those on the wire either.
	b.client.CloseNoStatus()
}

// shutdown tears both sockets down and releases the connection slot once.
func (b *adminIncusUIBridge) shutdown() {
	b.shutdownOnce.Do(func() {
		b.mu.Lock()
		b.finished = true
		upstream := b.upstream
		b.mu.Unlock()

		if upstream != nil {
			upstream.Terminate()
		}
		if b.client.IsOpen() || b.client.ReadyState() == int(lib.WebSocketConnecting) {
			b.client.CloseNoStatus()
		}
		adminIncusUIReleaseConnectionSlot(b.userID)
		close(b.finishedCh)
	})
}

// ==================== registration ====================

// RegisterAdminIncusUI mounts the module. Paths are absolute because app.ts registers
// it without a prefix.
func RegisterAdminIncusUI(r *gin.Engine) {
	// The ticket endpoint lives on the root instance in Fastify, so it gets the
	// default JSON body parser and the standard authenticateAdmin decorator.
	tickets := r.Group("/api/admin/hosts")
	tickets.POST("/:hostId/incus-ui-ticket", middleware.AuthenticateAdmin(), adminIncusUITicketHandler)

	// Proxy family: auth happens inside the handler because the plugin replaces the
	// body parsers (see the file header).
	proxy := r.Group("/api/admin/hosts/:hostId/incus-proxy")
	proxy.GET("/*path", adminIncusUIProxyHandler)
	proxy.HEAD("/*path", adminIncusUIProxyHandler) // Fastify exposeHeadRoutes default
	proxy.POST("/*path", adminIncusUIProxyHandler)
	proxy.PUT("/*path", adminIncusUIProxyHandler)
	proxy.PATCH("/*path", adminIncusUIProxyHandler)
	proxy.DELETE("/*path", adminIncusUIProxyHandler)

	// WebSocket family: auth (ticket) runs before the upgrade.
	websocket := r.Group("/api/ws/hosts/:hostId/incus-proxy")
	websocket.GET("/*path", adminIncusUIWebSocketHandler)
}

func init() {
	Register(RegisterAdminIncusUI)
}
