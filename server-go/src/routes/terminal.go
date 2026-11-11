// Port of server/src/routes/terminal.ts (527 lines), mounted at
// /api/ws/instances (app.ts registers it with that prefix).
//
// Routes:
//
//	POST /api/ws/instances/:id/terminal-ticket   JWT → one-shot terminal ticket
//	GET  /api/ws/instances/:id/terminal          ticket-authenticated WebSocket
//	GET  /api/ws/instances/stats                 admin: live session counters
//
// Security position of this port (task iron rule #1): every authorisation check
// runs BEFORE the RFC 6455 handshake, so an unauthenticated caller never receives
// 101 Switching Protocols. The Node route upgrades first and then closes with
// 4001/4003; a WebSocket client cannot observe the difference beyond the
// handshake failing, and the pre-upgrade rejection also keeps bad tickets out of
// the live socket table. The JSON error bodies are the same shape the Node route
// sends on the upgraded socket:
//
//	{"type":"error","code":"UNAUTHORIZED","message":"Authentication failed"}
//
// A plain GET without the Upgrade headers answers 404 with an empty body, exactly
// like @fastify/websocket (measured; see routes/agent.go for the same finding).
package routes

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// TERMINAL_LIMITS in terminal.ts.
const (
	terminalMaxPerUser     = 5
	terminalMaxPerInstance = 3
)

// pending reservations mirror pendingTerminalConnectionsByUser / ByInstance.
var terminalReservationStore = struct {
	sync.Mutex
	byUser     map[int32]int
	byInstance map[int32]int
}{byUser: map[int32]int{}, byInstance: map[int32]int{}}

// terminalReservation mirrors TerminalConnectionReservation.
type terminalReservation struct {
	Allowed bool
	Scope   string // "user" | "instance"
	Current int
	Limit   int
	release func()
}

// Release drops the pending slot; it is idempotent, like the TypeScript closure.
func (r terminalReservation) Release() {
	if r.release != nil {
		r.release()
	}
}

func terminalDecrementPending(counts map[int32]int, key int32) {
	next := counts[key] - 1
	if next > 0 {
		counts[key] = next
	} else {
		delete(counts, key)
	}
}

// terminalClaimConnection mirrors claimTerminalConnection(userId, instanceId).
//
// The check and the reservation happen under one mutex, which is the Go
// equivalent of the atomicity Node gets from its single-threaded event loop.
func terminalClaimConnection(userID, instanceID int32) terminalReservation {
	stats := lib.GetActiveSessionStats()

	terminalReservationStore.Lock()
	defer terminalReservationStore.Unlock()

	currentUser := stats.ByUser[userID] + terminalReservationStore.byUser[userID]
	if currentUser >= terminalMaxPerUser {
		return terminalReservation{Allowed: false, Scope: "user", Current: currentUser, Limit: terminalMaxPerUser}
	}
	currentInstance := stats.ByInstance[instanceID] + terminalReservationStore.byInstance[instanceID]
	if currentInstance >= terminalMaxPerInstance {
		return terminalReservation{Allowed: false, Scope: "instance", Current: currentInstance, Limit: terminalMaxPerInstance}
	}

	terminalReservationStore.byUser[userID]++
	terminalReservationStore.byInstance[instanceID]++

	released := false
	return terminalReservation{
		Allowed: true,
		release: func() {
			terminalReservationStore.Lock()
			defer terminalReservationStore.Unlock()
			if released {
				return
			}
			released = true
			terminalDecrementPending(terminalReservationStore.byUser, userID)
			terminalDecrementPending(terminalReservationStore.byInstance, instanceID)
		},
	}
}

// resetTerminalReservations clears the pending counters (tests only).
func resetTerminalReservations() {
	terminalReservationStore.Lock()
	terminalReservationStore.byUser = map[int32]int{}
	terminalReservationStore.byInstance = map[int32]int{}
	terminalReservationStore.Unlock()
}

// terminalParseRouteID mirrors POSITIVE_ROUTE_ID_PATTERN + Number.isSafeInteger,
// bounded to the int32 Prisma column.
func terminalParseRouteID(value string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok || parsed > 2147483647 {
		return 0, false
	}
	return int32(parsed), true
}

// terminalPermission mirrors checkTerminalPermission: admins may use every
// instance, owners their own, and host owners every instance on their node.
func terminalPermission(ctx context.Context, user *db.UserRecord, instance *db.InstanceDetailRecord) (bool, error) {
	if string(user.Role) == "admin" {
		return true, nil
	}
	if instance.UserID == user.ID {
		return true, nil
	}
	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		return false, err
	}
	return host != nil && host.UserID == user.ID, nil
}

// terminalHandler is the subset of a verified identity the WebSocket preflight
// needs.
type terminalIdentity struct {
	User         *db.UserRecord
	Instance     *db.InstanceDetailRecord
	Host         *db.HostsRecord
	InstanceType string
}

// terminalWriteError mirrors `reply.code(status).send({error, code})` for the
// HTTP (pre-upgrade) paths.
func terminalWriteError(c *gin.Context, status int, code, message string) {
	payload := gin.H{"error": message}
	if code != "" {
		payload["code"] = code
	}
	c.AbortWithStatusJSON(status, payload)
}

// terminalWriteSocketError is the JSON a WebSocket client receives before the
// close frame (Node: safeSend + socket.close).
func terminalWriteSocketError(socket *lib.WebSocket, code string, message string, closeCode int, reason string) {
	encoded := fmt.Sprintf(`{"type":"error","code":%q,"message":%q}`, code, message)
	_ = socket.SendText(encoded)
	socket.Close(closeCode, reason)
}

// terminalTicketHandler mirrors POST /:id/terminal-ticket.
func terminalTicketHandler(c *gin.Context) {
	claims := middleware.CurrentUser(c)
	if claims == nil {
		terminalWriteError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Unauthorized")
		return
	}

	instanceID, ok := terminalParseRouteID(c.Param("id"))
	if !ok {
		terminalWriteError(c, http.StatusBadRequest, "INVALID_ID", "Invalid instance ID")
		return
	}
	if claims.IssuedAt == 0 {
		terminalWriteError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Unauthorized")
		return
	}

	ctx := c.Request.Context()
	user, err := db.FindUserById(ctx, claims.ID)
	if err != nil {
		log.Printf("[terminal] find user %d failed: %v", claims.ID, err)
		terminalWriteError(c, http.StatusInternalServerError, "", "Internal server error")
		return
	}
	if user == nil || user.Status == models.UserStatusBanned {
		terminalWriteError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Unauthorized")
		return
	}

	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		log.Printf("[terminal] get instance %d failed: %v", instanceID, err)
		terminalWriteError(c, http.StatusInternalServerError, "", "Internal server error")
		return
	}
	if instance == nil {
		terminalWriteError(c, http.StatusNotFound, "INSTANCE_NOT_FOUND", "Instance not found")
		return
	}

	allowed, err := terminalPermission(ctx, user, instance)
	if err != nil {
		log.Printf("[terminal] permission lookup for instance %d failed: %v", instanceID, err)
		terminalWriteError(c, http.StatusInternalServerError, "", "Internal server error")
		return
	}
	if !allowed {
		terminalWriteError(c, http.StatusForbidden, "FORBIDDEN", "Forbidden")
		return
	}

	if instance.Status != models.InstanceStatusRunning {
		terminalWriteError(c, http.StatusBadRequest, "INSTANCE_NOT_RUNNING", "Instance must be running to access terminal")
		return
	}

	sessionID := terminalSessionIDPointer(claims.SID)
	ticket := lib.GenerateTerminalAccessTicket(claims.ID, instanceID, claims.IssuedAt, sessionID)
	c.JSON(http.StatusOK, gin.H{"ticket": ticket, "expiresIn": 60})
}

func terminalSessionIDPointer(sessionID string) *string {
	if sessionID == "" {
		return nil
	}
	value := sessionID
	return &value
}

// terminalStatsHandler mirrors GET /stats (admin).
func terminalStatsHandler(c *gin.Context) {
	stats := lib.GetActiveSessionStats()
	byInstance := make(map[string]int32, len(stats.ByInstance))
	for instanceID, count := range stats.ByInstance {
		byInstance[strconv.FormatInt(int64(instanceID), 10)] = int32(count)
	}
	byUser := make(map[string]int32, len(stats.ByUser))
	for userID, count := range stats.ByUser {
		byUser[strconv.FormatInt(int64(userID), 10)] = int32(count)
	}
	c.JSON(http.StatusOK, gin.H{
		"total":      stats.Total,
		"byInstance": byInstance,
		"byUser":     byUser,
	})
}

// terminalWebSocketPreflight performs every check the TypeScript performs after
// the upgrade, but before the handshake (task iron rule #1). It returns the
// identity on success and writes the rejection on failure.
func terminalWebSocketPreflight(c *gin.Context) (*terminalIdentity, bool) {
	clientIP := middleware.ClientIP(c)

	instanceID, ok := terminalParseRouteID(c.Param("id"))
	if !ok {
		terminalWriteError(c, http.StatusBadRequest, "INVALID_ID", "Invalid instance ID")
		return nil, false
	}

	originCheck := lib.ValidateWebSocketOrigin(c.Request)
	if !originCheck.Allowed {
		origin := originCheck.Origin
		if origin == "" && c.Request != nil {
			origin = c.Request.Header.Get("Origin")
		}
		log.Printf("[terminal] rejected WebSocket origin %q: %s", origin, originCheck.Reason)
		terminalWriteError(c, http.StatusForbidden, "ORIGIN_NOT_ALLOWED", "WebSocket origin is not allowed")
		return nil, false
	}

	if ipCheck := lib.CheckIPConnectionLimit(clientIP); !ipCheck.Allowed {
		terminalWriteError(c, http.StatusTooManyRequests, "IP_LIMIT_EXCEEDED",
			fmt.Sprintf("Too many connections from this IP (%d/%d)", ipCheck.Current, ipCheck.Limit))
		return nil, false
	}

	ticket := c.Query("ticket")
	if ticket == "" {
		terminalWriteError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authentication ticket is required")
		return nil, false
	}

	authResult := lib.ConsumeTerminalAccessTicket(ticket, &instanceID)
	if !authResult.Valid || authResult.UserID == 0 || authResult.IssuedAt == 0 {
		message := authResult.Error
		if message == "" {
			message = "Authentication failed"
		}
		terminalWriteError(c, http.StatusUnauthorized, "UNAUTHORIZED", message)
		return nil, false
	}

	sessionID := ""
	if authResult.SessionID != nil {
		sessionID = *authResult.SessionID
	}
	if lib.IsAccessTokenInvalidated(c.Request.Context(), authResult.UserID, authResult.IssuedAt, sessionID) {
		terminalWriteError(c, http.StatusUnauthorized, "SESSION_INVALIDATED", "Session expired")
		return nil, false
	}

	ctx := c.Request.Context()
	dbUser, err := db.FindUserById(ctx, authResult.UserID)
	if err != nil {
		log.Printf("[terminal] find user %d failed: %v", authResult.UserID, err)
		terminalWriteError(c, http.StatusInternalServerError, "", "Internal server error")
		return nil, false
	}
	if dbUser == nil || dbUser.Status == models.UserStatusBanned {
		terminalWriteError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authentication failed")
		return nil, false
	}

	stats := lib.GetActiveSessionStats()
	if current := stats.ByUser[dbUser.ID]; current >= terminalMaxPerUser {
		terminalWriteError(c, http.StatusTooManyRequests, "USER_LIMIT_EXCEEDED",
			fmt.Sprintf("Too many terminal connections (%d/%d)", current, terminalMaxPerUser))
		return nil, false
	}

	if wsCheck := lib.CheckUserConnectionLimit(dbUser.ID); !wsCheck.Allowed {
		terminalWriteError(c, http.StatusTooManyRequests, "WS_LIMIT_EXCEEDED",
			fmt.Sprintf("Too many WebSocket connections (%d/%d)", wsCheck.Current, wsCheck.Limit))
		return nil, false
	}

	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		log.Printf("[terminal] get instance %d failed: %v", instanceID, err)
		terminalWriteError(c, http.StatusInternalServerError, "", "Internal server error")
		return nil, false
	}
	if instance == nil {
		terminalWriteError(c, http.StatusNotFound, "INSTANCE_NOT_FOUND", "Instance not found")
		return nil, false
	}

	allowed, err := terminalPermission(ctx, dbUser, instance)
	if err != nil {
		log.Printf("[terminal] permission lookup for instance %d failed: %v", instanceID, err)
		terminalWriteError(c, http.StatusInternalServerError, "", "Internal server error")
		return nil, false
	}
	if !allowed {
		terminalWriteError(c, http.StatusForbidden, "FORBIDDEN", "You do not have permission to access this terminal")
		return nil, false
	}

	if instance.Status != models.InstanceStatusRunning {
		terminalWriteError(c, http.StatusBadRequest, "INSTANCE_NOT_RUNNING", "Instance must be running to access terminal")
		return nil, false
	}

	if current := stats.ByInstance[instanceID]; current >= terminalMaxPerInstance {
		terminalWriteError(c, http.StatusTooManyRequests, "INSTANCE_LIMIT_EXCEEDED",
			fmt.Sprintf("Too many terminal connections for this instance (%d/%d)", current, terminalMaxPerInstance))
		return nil, false
	}

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		log.Printf("[terminal] get host %d failed: %v", instance.HostID, err)
		terminalWriteError(c, http.StatusInternalServerError, "", "Internal server error")
		return nil, false
	}
	if host == nil {
		terminalWriteError(c, http.StatusNotFound, "HOST_NOT_FOUND", "Host not found")
		return nil, false
	}

	instanceType := "container"
	if instance.PackageID != nil {
		if pkg, pkgErr := db.GetPackageById(ctx, *instance.PackageID); pkgErr == nil && pkg != nil && pkg.InstanceType == "vm" {
			instanceType = "vm"
		}
	}

	return &terminalIdentity{User: dbUser, Instance: instance, Host: host, InstanceType: instanceType}, true
}

// terminalWebSocketHandler mirrors GET /:id/terminal (websocket: true).
//
// As in the TypeScript, the instance/POST-check ordering is: id, origin, IP limit,
// ticket, session invalidation, user, per-user limits, instance, permission,
// status, per-instance limit, host. Those checks now run before the upgrade.
func terminalWebSocketHandler(c *gin.Context) {
	if !lib.IsWebSocketUpgrade(c.Request) {
		// Measured against the running Node backend: @fastify/websocket answers a
		// plain GET on a websocket route with 404 and an EMPTY body.
		c.Status(http.StatusNotFound)
		return
	}

	identity, ok := terminalWebSocketPreflight(c)
	if !ok {
		return
	}

	// From here on the request is authenticated. The atomic reservation below is
	// what closes the race between the preflight counters and two simultaneous
	// connections (the TypeScript does the same with claimTerminalConnection).
	reservation := terminalClaimConnection(identity.User.ID, identity.Instance.ID)

	// Re-check the limits inside the reservation to produce the exact claim error.
	if !reservation.Allowed {
		isUserLimit := reservation.Scope == "user"
		code := "INSTANCE_LIMIT_EXCEEDED"
		message := fmt.Sprintf("Too many terminal connections for this instance (%d/%d)", reservation.Current, reservation.Limit)
		if isUserLimit {
			code = "USER_LIMIT_EXCEEDED"
			message = fmt.Sprintf("Too many terminal connections (%d/%d)", reservation.Current, reservation.Limit)
		}
		terminalWriteError(c, http.StatusTooManyRequests, code, message)
		return
	}
	// The reservation is released below once the session is registered (or on error).

	socket, err := lib.UpgradeWebSocket(c.Writer, c.Request, nil)
	if err != nil {
		reservation.Release()
		log.Printf("[terminal] websocket upgrade failed: %v", err)
		return
	}
	owned := false
	defer func() {
		// The session that is created below takes ownership of the socket. If we did
		// not get that far (error path), close it so nothing leaks.
		if !owned {
			socket.Terminate()
		}
	}()

	clientIP := middleware.ClientIP(c)
	lib.RegisterConnection(clientIP, socket, &identity.User.ID)

	session, err := lib.CreateTerminalSession(
		socket,
		identity.Host,
		identity.Instance.ID,
		identity.Instance.IncusID,
		identity.User.ID,
		identity.InstanceType,
		terminalSessionIDPointer(sessionIDFromClaims(c)),
	)
	reservation.Release()
	if err != nil {
		// Internal error details stay in the log; the client gets a generic message.
		log.Printf("[terminal] failed to create session for instance %d: %v", identity.Instance.ID, err)
		terminalWriteSocketError(socket, "CONNECTION_FAILED", "Failed to connect to terminal. Please try again later.", 4000, "Connection failed")
		userID := int64(identity.User.ID)
		db.CreateLog(context.Background(), &userID, "terminal", "terminal.connect",
			fmt.Sprintf("Terminal connection failed | instance: %s (#%d) | host: %s | type: %s | ip: %s | error: %v",
				identity.Instance.Name, identity.Instance.ID, identity.Host.Name, identity.InstanceType, clientIP, err),
			db.LogResultFailed, db.LogOptionsWithInstance(&identity.Instance.ID))
		return
	}
	owned = true

	logDetails := fmt.Sprintf("instance: %s (#%d) | host: %s | type: %s | mode: %s | ip: %s | session: %s",
		identity.Instance.Name, identity.Instance.ID, identity.Host.Name, identity.InstanceType,
		session.ConnectionMode(), clientIP, session.ID)
	userID := int64(identity.User.ID)
	instanceID := identity.Instance.ID
	db.CreateLog(context.Background(), &userID, "terminal", "terminal.connect",
		"Terminal connected | "+logDetails, db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))

	socket.OnClose(func() {
		// OnClose runs on the teardown goroutine; a database write must not block it.
		go func() {
			id := int64(identity.User.ID)
			db.CreateLog(context.Background(), &id, "terminal", "terminal.disconnect",
				"Terminal disconnected | "+logDetails, db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
		}()
	})
}

func sessionIDFromClaims(c *gin.Context) string {
	claims := middleware.CurrentUser(c)
	if claims == nil {
		return ""
	}
	return claims.SID
}

// RegisterTerminalRoutes mounts the module at /api/ws/instances.
func RegisterTerminalRoutes(r *gin.Engine) {
	group := r.Group("/api/ws/instances")
	group.POST("/:id/terminal-ticket", middleware.Authenticate(), terminalTicketHandler)
	group.GET("/stats", middleware.AuthenticateAdmin(), terminalStatsHandler)
	group.GET("/:id/terminal", terminalWebSocketHandler)
}

func init() {
	Register(RegisterTerminalRoutes)

	// Wire the Wave E seams declared by routes/auth.go and routes/users.go. Those
	// modules shipped before terminal-proxy.ts was ported and defaulted to "no live
	// session"; now a logout / ban / password change really closes them.
	AuthCloseSessionTerminalSessions = func(sessionID, reason string) int {
		return lib.CloseSessionTerminalSessions(sessionID, reason)
	}
	UsersCloseUserSessions = func(userID int32, reason string) int {
		return lib.CloseUserSessions(userID, reason)
	}
}
