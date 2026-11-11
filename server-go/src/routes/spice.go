// Port of server/src/routes/spice.ts (123 lines), mounted at /api/ws/instances.
//
// Routes:
//
//	POST   /api/ws/instances/:id/spice-session   JWT → one-shot console capability
//	DELETE /api/ws/instances/:id/spice-session   JWT → close that capability
//	GET    /api/ws/instances/:id/spice           capability-authenticated WebSocket
//
// The capability is delivered in `Sec-WebSocket-Protocol` ("incudal-spice.<ticket>")
// rather than the URL, so it never lands in reverse-proxy access logs; the server
// echoes the client's first protocol ("binary"), never the ticket.
//
// Security position (task iron rule #1): the ticket is validated BEFORE the
// handshake, so an unknown/expired ticket never receives 101. The Node route
// upgrades first and closes with 4001; the JSON bodies are the same shape.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// spiceSessionExpiresIn mirrors the hard-coded `expiresIn: 7200`.
const spiceSessionExpiresIn = 7200

// spiceTicketProtocolPrefix mirrors 'incudal-spice.'.
const spiceTicketProtocolPrefix = "incudal-spice."

// spiceAccessResult is what access() returns when every check passes.
type spiceAccessResult struct {
	User     *db.UserRecord
	Instance *db.InstanceDetailRecord
	Host     *db.HostsRecord
}

// spiceAccess mirrors the `access(context)` closure of spice.ts.
//
// A nil result (with a nil error) is the TypeScript `return null`: every
// authorisation failure collapses into 403 CONSOLE_FORBIDDEN so callers cannot
// distinguish "not yours" from "not running" or "not a VM".
func spiceAccess(ctx context.Context, access *lib.SpiceAccess) (*spiceAccessResult, error) {
	sessionID := ""
	if access.SessionID != nil {
		sessionID = *access.SessionID
	}
	if lib.IsAccessTokenInvalidated(ctx, access.UserID, access.IssuedAt, sessionID) {
		return nil, nil
	}
	user, err := db.FindUserById(ctx, access.UserID)
	if err != nil {
		return nil, err
	}
	if user == nil || user.Status == models.UserStatusBanned {
		return nil, nil
	}
	instance, err := db.GetInstanceById(ctx, access.InstanceID)
	if err != nil {
		return nil, err
	}
	if instance == nil || instance.Status != models.InstanceStatusRunning {
		return nil, nil
	}
	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, nil
	}
	if string(user.Role) != "admin" && instance.UserID != user.ID && host.UserID != user.ID {
		return nil, nil
	}
	if access.HostID != nil && host.ID != *access.HostID {
		return nil, nil
	}
	if access.InstanceName != nil && instance.IncusID != *access.InstanceName {
		return nil, nil
	}
	var pkg *db.PackageDetailRow
	if instance.PackageID != nil {
		pkg, err = db.GetPackageById(ctx, *instance.PackageID)
		if err != nil {
			return nil, err
		}
	}
	if pkg == nil || pkg.InstanceType != "vm" {
		return nil, nil
	}
	return &spiceAccessResult{User: user, Instance: instance, Host: host}, nil
}

// spiceParseRouteID mirrors parseId (strict /^[1-9]\d*$/ + safe integer, int32
// bounded like the other route modules).
func spiceParseRouteID(value string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok || parsed > 2147483647 {
		return 0, false
	}
	return int32(parsed), true
}

// ==================== inline rate limit ====================

type spiceLimiterBucket struct {
	count   int
	resetAt time.Time
}

var spiceLimiter = struct {
	sync.Mutex
	buckets map[string]spiceLimiterBucket
}{buckets: map[string]spiceLimiterBucket{}}

// spiceRateLimit mirrors `config: { rateLimit: { max, timeWindow } }` on the route.
// @fastify/rate-limit keeps a separate store per route, which the key encodes.
func spiceRateLimit(route string, max int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := middleware.ClientIP(c) + ":" + c.Request.Method + ":" + route
		now := time.Now()

		spiceLimiter.Lock()
		bucket, ok := spiceLimiter.buckets[key]
		if !ok || now.After(bucket.resetAt) {
			bucket = spiceLimiterBucket{count: 0, resetAt: now.Add(window)}
		}
		bucket.count++
		allowed := bucket.count <= max
		spiceLimiter.buckets[key] = bucket
		spiceLimiter.Unlock()

		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "Too many requests, please try again later",
			})
			return
		}
		c.Next()
	}
}

// ResetSpiceRateLimits clears the process-local buckets (tests only).
func ResetSpiceRateLimits() {
	spiceLimiter.Lock()
	spiceLimiter.buckets = map[string]spiceLimiterBucket{}
	spiceLimiter.Unlock()
}

// ==================== body validation (Fastify/AJV) ====================

// spiceWriteValidationError mirrors the `error.validation` branch of
// app.ts setErrorHandler.
func spiceWriteValidationError(c *gin.Context, message string) {
	c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
		"error": message,
		"code":  "VALIDATION_ERROR",
	})
}

func spiceContentTypeIsJSON(c *gin.Context) bool {
	contentType := strings.ToLower(c.GetHeader("Content-Type"))
	return strings.HasPrefix(contentType, "application/json") || strings.Contains(contentType, "+json")
}

// spiceDecodeBodyObject decodes the JSON body as an object. The Fastify parser
// normally validates JSON syntax in middleware.ValidateFastifyBody; the direct
// handler path (and the DELETE route, which has no parser hook of its own) still
// needs to distinguish "not JSON" from "JSON that is not an object".
func spiceDecodeBodyObject(data []byte) (map[string]any, bool, error) {
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, false, err
	}
	object, ok := decoded.(map[string]any)
	return object, ok, nil
}

// spiceCoerceBoolean mirrors AJV's coerceTypes:'array' coercion for type boolean.
func spiceCoerceBoolean(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case nil:
		return false, true
	case float64:
		return typed != 0, true
	case string:
		switch typed {
		case "true":
			return true, true
		case "false":
			return false, true
		}
		return false, false
	case []any:
		if len(typed) == 1 {
			return spiceCoerceBoolean(typed[0])
		}
		return false, false
	default:
		return false, false
	}
}

// spiceReadSessionBody validates `{ reconnect?: boolean }` with
// additionalProperties:false. It reports ok=false after writing the 400.
func spiceReadSessionBody(c *gin.Context) (reconnect bool, ok bool) {
	hasBody := c.Request.ContentLength > 0 || len(c.Request.TransferEncoding) > 0
	if !hasBody || !spiceContentTypeIsJSON(c) {
		// Fastify leaves request.body undefined when there is no body or the content
		// type has no parser; AJV then rejects `undefined` against `type: object`
		// with "must be object" (measured against the Node backend).
		spiceWriteValidationError(c, "must be object")
		return false, false
	}
	payload, err := io.ReadAll(c.Request.Body)
	if err != nil {
		spiceWriteValidationError(c, "Body is not valid JSON but content-type is set to 'application/json'")
		return false, false
	}
	object, isObject, decodeErr := spiceDecodeBodyObject(payload)
	if decodeErr != nil {
		spiceWriteValidationError(c, "Body is not valid JSON but content-type is set to 'application/json'")
		return false, false
	}
	if !isObject {
		spiceWriteValidationError(c, "must be object")
		return false, false
	}
	// AJV runs with removeAdditional:true (@fastify/ajv-compiler's defaults), so
	// `additionalProperties:false` SILENTLY DROPS unknown keys instead of rejecting
	// them — measured against the Node backend by the conformance harness.
	if value, present := object["reconnect"]; present {
		coerced, valid := spiceCoerceBoolean(value)
		if !valid {
			spiceWriteValidationError(c, "must be boolean")
			return false, false
		}
		reconnect = coerced
	}
	return reconnect, true
}

// spiceReadDeleteBody validates `{ ticket: string(maxLength 64) }` (required).
func spiceReadDeleteBody(c *gin.Context) (ticket string, ok bool) {
	hasBody := c.Request.ContentLength > 0 || len(c.Request.TransferEncoding) > 0
	if !hasBody {
		spiceWriteValidationError(c, "must be object")
		return "", false
	}
	if !spiceContentTypeIsJSON(c) {
		spiceWriteValidationError(c, "must be object")
		return "", false
	}
	payload, err := io.ReadAll(c.Request.Body)
	if err != nil {
		spiceWriteValidationError(c, "Body is not valid JSON but content-type is set to 'application/json'")
		return "", false
	}
	object, isObject, decodeErr := spiceDecodeBodyObject(payload)
	if decodeErr != nil {
		spiceWriteValidationError(c, "Body is not valid JSON but content-type is set to 'application/json'")
		return "", false
	}
	if !isObject {
		spiceWriteValidationError(c, "must be object")
		return "", false
	}
	raw, present := object["ticket"]
	if !present {
		spiceWriteValidationError(c, "ticket is required")
		return "", false
	}
	value, valid := spiceCoerceString(raw)
	if !valid {
		spiceWriteValidationError(c, "must be string")
		return "", false
	}
	if len([]rune(value)) > 64 {
		spiceWriteValidationError(c, "ticket is too long")
		return "", false
	}
	return value, true
}

// spiceCoerceString mirrors AJV's string coercion (numbers/booleans → string,
// null → "", single-element arrays unwrap).
func spiceCoerceString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case nil:
		return "", true
	case bool:
		return strconv.FormatBool(typed), true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case []any:
		if len(typed) == 1 {
			return spiceCoerceString(typed[0])
		}
		return "", false
	default:
		return "", false
	}
}

// ==================== handlers ====================

// spiceCreateSessionHandler mirrors POST /:id/spice-session.
func spiceCreateSessionHandler(c *gin.Context) {
	claims := middleware.CurrentUser(c)
	if claims == nil {
		terminalWriteError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Unauthorized")
		return
	}

	instanceID, ok := spiceParseRouteID(c.Param("id"))
	if !ok {
		terminalWriteError(c, http.StatusBadRequest, "INVALID_ID", "Invalid instance ID")
		return
	}
	if claims.IssuedAt == 0 {
		terminalWriteError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Unauthorized")
		return
	}

	reconnect, bodyOK := spiceReadSessionBody(c)
	if !bodyOK {
		return
	}

	ctx := c.Request.Context()
	access := lib.SpiceAccess{
		UserID:     claims.ID,
		InstanceID: instanceID,
		IssuedAt:   claims.IssuedAt,
		SessionID:  terminalSessionIDPointer(claims.SID),
	}

	current, err := spiceAccess(ctx, &access)
	if err != nil {
		log.Printf("[spice] access check for instance %d failed: %v", instanceID, err)
		terminalWriteError(c, http.StatusInternalServerError, "", "Internal server error")
		return
	}
	if current == nil {
		terminalWriteError(c, http.StatusForbidden, "CONSOLE_FORBIDDEN", "Running VM access required")
		return
	}
	access.HostID = &current.Host.ID
	access.InstanceName = &current.Instance.IncusID

	clientIP := middleware.ClientIP(c)
	if !lib.CheckIPConnectionLimit(clientIP).Allowed || !lib.CheckUserConnectionLimit(access.UserID).Allowed {
		terminalWriteError(c, http.StatusTooManyRequests, "CONSOLE_LIMIT", "Connection limit exceeded")
		return
	}

	open := func(openCtx context.Context) (*lib.SpiceBackend, error) {
		return lib.CreateSpiceBackend(openCtx, current.Host, current.Instance.IncusID, reconnect)
	}
	validate := func(validateCtx context.Context) bool {
		checked, checkErr := spiceAccess(validateCtx, &access)
		return checkErr == nil && checked != nil
	}

	ticket, err := lib.SpiceSessions.Create(access, open, validate, reconnect)
	if err != nil {
		// A client that vanished while the console was opening gets no response.
		if c.Request.Context().Err() != nil {
			if ticket != "" {
				lib.SpiceSessions.Close(ticket)
			}
			return
		}
		limited := errors.Is(err, lib.ErrSpiceConsoleLimit)
		log.Printf("[spice] instance %d console setup failed: stage=%s httpStatus=%d",
			instanceID, lib.SpiceConnectionStageOf(err), lib.SpiceConnectionStatusOf(err))
		if limited {
			terminalWriteError(c, http.StatusConflict, "CONSOLE_LIMIT", "Console already open or connection limit reached")
			return
		}
		terminalWriteError(c, http.StatusBadGateway, "CONSOLE_UNAVAILABLE", "Unable to open Incus graphical console")
		return
	}

	if c.Request.Context().Err() != nil {
		lib.SpiceSessions.Close(ticket)
		return
	}

	// This capability is limited to this console session; it is NOT a reusable
	// login token.
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"ticket": ticket, "expiresIn": spiceSessionExpiresIn})
}

// spiceDeleteSessionHandler mirrors DELETE /:id/spice-session.
func spiceDeleteSessionHandler(c *gin.Context) {
	claims := middleware.CurrentUser(c)
	if claims == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	instanceID, ok := spiceParseRouteID(c.Param("id"))
	if !ok {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid instance ID"})
		return
	}
	ticket, bodyOK := spiceReadDeleteBody(c)
	if !bodyOK {
		return
	}

	contextAccess := lib.SpiceSessions.Get(ticket, instanceID)
	if contextAccess == nil {
		// The main channel may already have cleaned it up.
		c.JSON(http.StatusOK, gin.H{"success": true})
		return
	}
	sessionID := ""
	if contextAccess.SessionID != nil {
		sessionID = *contextAccess.SessionID
	}
	if contextAccess.UserID != claims.ID || sessionID != claims.SID {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Forbidden"})
		return
	}

	<-lib.SpiceSessions.Close(ticket)
	lib.SpiceSessions.WaitForCleanup(instanceID)
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// spiceWebSocketHandler mirrors GET /:id/spice (websocket: true).
func spiceWebSocketHandler(c *gin.Context) {
	if !lib.IsWebSocketUpgrade(c.Request) {
		c.Status(http.StatusNotFound)
		return
	}

	instanceID, ok := spiceParseRouteID(c.Param("id"))
	if !ok {
		terminalWriteError(c, http.StatusBadRequest, "INVALID_ID", "Invalid instance ID")
		return
	}
	originCheck := lib.ValidateWebSocketOrigin(c.Request)
	if !originCheck.Allowed {
		terminalWriteError(c, http.StatusForbidden, "ORIGIN_NOT_ALLOWED", "WebSocket origin is not allowed")
		return
	}

	// Keep the capability out of URLs and standard reverse-proxy access logs. The
	// "binary" protocol is first, so ws negotiates that one and never echoes the
	// ticket.
	ticket := spiceTicketFromProtocols(lib.WSOfferedSubprotocols(c.Request))
	if ticket == "" || len(ticket) > 64 {
		terminalWriteError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Console session expired")
		return
	}
	contextAccess := lib.SpiceSessions.Get(ticket, instanceID)
	if contextAccess == nil {
		terminalWriteError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Console session expired")
		return
	}
	clientIP := middleware.ClientIP(c)
	userID := contextAccess.UserID
	accessCopy := *contextAccess
	instanceIDCopy := instanceID

	socket, err := lib.UpgradeWebSocket(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("[spice] websocket upgrade failed: %v", err)
		return
	}

	lib.SpiceSessions.Attach(ticket, instanceID, socket, func(main bool) bool {
		checked, checkErr := spiceAccess(context.Background(), &accessCopy)
		if checkErr != nil || checked == nil {
			return false
		}
		if main {
			// The main/display/input sockets are one logical console for generic
			// limits.
			if !lib.CheckIPConnectionLimit(clientIP).Allowed || !lib.CheckUserConnectionLimit(userID).Allowed {
				return false
			}
			lib.RegisterConnection(clientIP, socket, &userID)
			socket.OnClose(func() {
				go db.CreateLog(context.Background(), spiceUserIDPointer(int64(userID)), "terminal", "console.disconnect",
					"Graphical console disconnected (#"+strconv.FormatInt(int64(instanceIDCopy), 10)+")", db.LogResultSuccess,
					db.LogOptionsWithInstance(&instanceIDCopy))
			})
			go db.CreateLog(context.Background(), spiceUserIDPointer(int64(userID)), "terminal", "console.connect",
				"Graphical console connected (#"+strconv.FormatInt(int64(instanceIDCopy), 10)+")", db.LogResultSuccess,
				db.LogOptionsWithInstance(&instanceIDCopy))
		}
		return true
	})
}

func spiceTicketFromProtocols(protocols []string) string {
	for _, protocol := range protocols {
		if strings.HasPrefix(protocol, spiceTicketProtocolPrefix) {
			return protocol[len(spiceTicketProtocolPrefix):]
		}
	}
	return ""
}

func spiceUserIDPointer(value int64) *int64 { return &value }

// RegisterSpiceRoutes mounts the module at /api/ws/instances.
func RegisterSpiceRoutes(r *gin.Engine) {
	group := r.Group("/api/ws/instances")
	group.POST("/:id/spice-session",
		spiceRateLimit("/api/ws/instances/:id/spice-session", 10, time.Minute),
		middleware.Authenticate(),
		spiceCreateSessionHandler)
	group.DELETE("/:id/spice-session", middleware.Authenticate(), spiceDeleteSessionHandler)
	group.GET("/:id/spice", spiceWebSocketHandler)
}

func init() {
	Register(RegisterSpiceRoutes)
}
