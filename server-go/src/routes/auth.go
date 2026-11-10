// Port of server/src/routes/auth.ts (1436 lines, 19 routes).
//
// app.ts registers this module with `{ prefix: '/api/auth' }`, so every path below
// is relative to that prefix.
//
// Fastify pipeline order is observable on the wire and is reproduced exactly:
//
//	onRequest (authenticate) → body parser → preValidation (JSON schema)
//	→ preHandler (Turnstile) → handler
//
// Consequences pinned by this file:
//   - an unauthenticated request to an `onRequest: [fastify.authenticate]` route
//     answers 401 even when its body is empty/malformed (middleware.Authenticate
//     runs the parser only after auth succeeds);
//   - a public route with a schema validates the body BEFORE Turnstile, and
//     Turnstile runs BEFORE any handler check (registration switch, SMTP, ...);
//   - validation errors use AJV's first-error-only semantics and the messages
//     rendered by app.ts' setErrorHandler (including the username/password
//     special cases).
//
// Security controls are delegated, never re-implemented:
//   - login failure lock  → lib.CheckLoginLock / RecordLoginFailure / ClearLoginFailure
//     (process-local, single-instance invariant; the key is `user:<id>` when the
//     account exists, otherwise the lower-cased trimmed identifier, always scoped
//     by client IP);
//   - 2FA TOTP            → lib.Verify2FAToken (Skew 0: the previous step is
//     rejected);
//   - refresh cookie      → lib.GetRefreshTokenCookieOptions / GetClearCookieOptions
//     are the only source of httpOnly/sameSite/secure/path/domain;
//   - Turnstile           → lib.TurnstileVerifier (client IP wired in app.go);
//   - recovery codes      → lib.GenerateRecoveryCodes + AES-GCM from lib/security.go.
//
// Ported seams (the source module is not part of Wave D yet):
//   - lib/terminal-proxy.ts#closeSessionTerminalSessions / closeUserSessions → the
//     no-op AuthCloseSessionTerminalSessions seam and routes/users.go's
//     UsersCloseUserSessions seam. Both return 0, which is exactly what the
//     TypeScript reports with no live terminal session.
//   - lib/action-ticket.ts#revokeActionTicketsForSession → AuthRevokeActionTicketsForSession.
//   - lib/geoip.ts#getGeoIpInfo → authGetGeoIPInfo (lookup only; login records for
//     loopback/private addresses carry null geo fields exactly like Node).
//   - db/login-records.ts#createLoginRecord → authCreateLoginRecord (the read side,
//     getUserLoginRecords, already lives in db/users.go).
//   - lib/email-domain.ts#validateEmailDomain → routes/users.go's
//     usersValidateEmailDomain (the canonical lib file has not been ported).
//
// Passwords use golang.org/x/crypto/bcrypt. bcryptjs (Node) truncates inputs at 72
// bytes, which is also bcrypt's hard limit, so the helpers below truncate too:
// without that, a 73..128 character password (allowed by the schema) would verify
// in Node but not here.
package routes

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// authLoginIdentifierMaxLength mirrors LOGIN_IDENTIFIER_MAX_LENGTH.
const authLoginIdentifierMaxLength = 254

// authPasswordBcryptCost mirrors `bcrypt.hash(password, 12)`.
const authPasswordBcryptCost = 12

// authBcryptMaxBytes is bcrypt's 72-byte input limit (bcryptjs truncates too).
const authBcryptMaxBytes = 72

// authRecoveryCodeTotal mirrors the fixed `total: 8` of /2fa/recovery-codes.
const authRecoveryCodeTotal = lib.DefaultRecoveryCodeCount

// ==================== seams for unported modules ====================

// AuthCloseSessionTerminalSessions mirrors closeSessionTerminalSessions from
// server/src/lib/terminal-proxy.ts (Wave E). Before that module is ported no
// terminal session can exist, so the default returns 0 — the value the
// TypeScript reports for a session without a live terminal. The terminal module
// should assign this variable from its init().
var AuthCloseSessionTerminalSessions = func(sessionID, reason string) int { return 0 }

// AuthRevokeActionTicketsForSession mirrors revokeActionTicketsForSession from
// server/src/lib/action-ticket.ts. Same wiring contract: the action-ticket module
// assigns it from init(); the default reports zero revoked tickets, matching an
// empty in-process ticket map.
var AuthRevokeActionTicketsForSession = func(sessionID string) int { return 0 }

// ==================== registration ====================

// RegisterAuth mounts /api/auth. app.ts calls
// `fastify.register(authRoutes, { prefix: '/api/auth' })`.
func RegisterAuth(r *gin.Engine) {
	group := r.Group("/api/auth")

	// Public routes: no onRequest auth. The body parser runs before preValidation
	// and preHandler in Fastify, so middleware.RequestBody is mounted first and
	// the handler re-reads the restored body.
	group.POST("/check-2fa", middleware.RequestBody(), authCheck2FA)
	group.POST("/login", middleware.RequestBody(), authLogin)
	group.POST("/send-verification-code", middleware.RequestBody(), authSendVerificationCode)
	group.POST("/register", middleware.RequestBody(), authRegister)
	group.POST("/logout", middleware.RequestBody(), authLogout)
	group.POST("/refresh", middleware.RequestBody(), authRefresh)
	group.POST("/forgot-password/send-code", middleware.RequestBody(), authForgotPasswordSendCode)
	group.POST("/forgot-password/reset", middleware.RequestBody(), authForgotPasswordReset)

	// Authenticated routes (`onRequest: [fastify.authenticate]`). Authenticate
	// parses the body after auth, so 401 always wins over 400/413/415.
	group.GET("/me", middleware.Authenticate(), authMe)
	group.POST("/invite", middleware.Authenticate(), authInvite)
	group.GET("/invites", middleware.Authenticate(), authInvitesList)
	group.DELETE("/invites/:id", middleware.Authenticate(), authInviteDelete)
	group.GET("/2fa/status", middleware.Authenticate(), auth2FAStatus)
	group.POST("/2fa/setup", middleware.Authenticate(), auth2FASetup)
	group.POST("/2fa/enable", middleware.Authenticate(), auth2FAEnable)
	group.POST("/2fa/disable", middleware.Authenticate(), auth2FADisable)
	group.GET("/2fa/recovery-codes", middleware.Authenticate(), auth2FARecoveryCodes)
	group.POST("/2fa/regenerate-recovery-codes", middleware.Authenticate(), auth2FARegenerateRecoveryCodes)
	group.GET("/login-history", middleware.Authenticate(), authLoginHistory)
}

// ==================== shared response shapes ====================

// authUserPayload mirrors buildAuthUserPayload.
type authUserPayload struct {
	ID            int32           `json:"id"`
	Username      string          `json:"username"`
	Email         *string         `json:"email"`
	Role          models.UserRole `json:"role"`
	AvatarStyle   string          `json:"avatarStyle"`
	AvatarBadgeID *string         `json:"avatarBadgeId"`
}

// authBuildUserPayload mirrors buildAuthUserPayload.
func authBuildUserPayload(user *db.UserRecord) authUserPayload {
	return authUserPayload{
		ID:            user.ID,
		Username:      user.Username,
		Email:         user.Email,
		Role:          user.Role,
		AvatarStyle:   user.AvatarStyle,
		AvatarBadgeID: user.AvatarBadgeID,
	}
}

// authQuotaPayload mirrors the /me quota projection (the new quota system).
type authQuotaPayload struct {
	HostLimit    int32 `json:"hostLimit"`
	HostUsed     int32 `json:"hostUsed"`
	FriendLimit  int32 `json:"friendLimit"`
	FriendUsed   int32 `json:"friendUsed"`
	PackageLimit int32 `json:"packageLimit"`
	PackageUsed  int32 `json:"packageUsed"`
}

// authMeUserPayload is buildAuthUserPayload(user) + the quota field.
type authMeUserPayload struct {
	ID            int32             `json:"id"`
	Username      string            `json:"username"`
	Email         *string           `json:"email"`
	Role          models.UserRole   `json:"role"`
	AvatarStyle   string            `json:"avatarStyle"`
	AvatarBadgeID *string           `json:"avatarBadgeId"`
	Quota         *authQuotaPayload `json:"quota"`
}

// ==================== helpers ====================

// authInternalError mirrors the production branch of app.ts' error handler for an
// unexpected throw: no internal detail leaks.
func authInternalError(c *gin.Context) {
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// authSecurityEvent adapts an optional int32 user id to LogSecurityEvent's
// *int64 signature.
func authSecurityEvent(ctx context.Context, eventType lib.SecurityEventType, userID *int32, details lib.SecurityEventDetails) {
	var id *int64
	if userID != nil {
		value := int64(*userID)
		id = &value
	}
	lib.LogSecurityEvent(ctx, eventType, id, details)
}

// authTruncatePassword mirrors bcryptjs' 72-byte truncation (also bcrypt's limit).
func authTruncatePassword(password string) []byte {
	raw := []byte(password)
	if len(raw) > authBcryptMaxBytes {
		return raw[:authBcryptMaxBytes]
	}
	return raw
}

// authHashPassword mirrors `bcrypt.hash(password, 12)`.
func authHashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword(authTruncatePassword(password), authPasswordBcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// authPasswordMatches mirrors `bcrypt.compare(password, hash)`; an unusable hash
// is a non-match, never an error the caller has to special-case.
func authPasswordMatches(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), authTruncatePassword(password)) == nil
}

// authSingleQueryValue returns a query value the way `request.query[key]` behaves
// for the parsePositiveInteger helpers and the `status` equality checks: a single
// value is a string, a repeated key is an array in Fastify (which fails both the
// positive-integer pattern and the strict equality check), and an absent key is
// undefined.
func authSingleQueryValue(c *gin.Context, name string) string {
	values, present := c.Request.URL.Query()[name]
	if !present || len(values) != 1 {
		return ""
	}
	return values[0]
}

// authRunTurnstile executes the already-ported verifier inline. Fastify runs
// preHandler after preValidation, so callers invoke it after schema validation.
func authRunTurnstile(c *gin.Context) bool {
	lib.TurnstileVerifier(c)
	return !c.IsAborted()
}

// authReadJSONBody mirrors the Fastify body parser for the body-carrying methods.
// It restores the body afterwards because the Turnstile verifier re-reads it.
// Returns (value, status, message); status 0 means the handler may proceed.
func authReadJSONBody(c *gin.Context) (any, int, string) {
	switch c.Request.Method {
	case http.MethodGet, http.MethodHead, http.MethodTrace:
		return nil, 0, ""
	}

	contentType := c.GetHeader("Content-Type")
	if contentType == "" {
		if c.Request.ContentLength > 0 || c.GetHeader("Transfer-Encoding") != "" {
			return nil, http.StatusUnsupportedMediaType, fastifyUndefinedMediaTypeMessage
		}
		// No body at all: Fastify never runs a parser and request.body is undefined.
		return nil, 0, ""
	}

	lowered := strings.ToLower(contentType)
	switch {
	case mediaTypeIs(lowered, "application/json"):
		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return nil, http.StatusBadRequest, fastifyInvalidJSONBodyMessage
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(data))
		if len(data) == 0 {
			return nil, http.StatusBadRequest, fastifyEmptyJSONBodyMessage
		}
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			return nil, http.StatusBadRequest, fastifyInvalidJSONBodyMessage
		}
		if hasForbiddenPrototypeKey(decoded) {
			// secure-json-parse runs with protoAction/constructorAction 'error'.
			return nil, http.StatusBadRequest, fastifyInvalidJSONBodyMessage
		}
		return decoded, 0, ""
	case mediaTypeIs(lowered, "text/plain"):
		data, _ := io.ReadAll(c.Request.Body)
		c.Request.Body = io.NopCloser(bytes.NewReader(data))
		return string(data), 0, ""
	default:
		return nil, http.StatusUnsupportedMediaType, "Unsupported Media Type: " + contentType
	}
}

// authFieldSpec is ajvFieldSpec plus `format`, which the shared replica leaves to
// the caller.
type authFieldSpec struct {
	ajvFieldSpec
	Format string
}

var (
	authUsernameLoginFields = []authFieldSpec{
		{ajvFieldSpec: ajvFieldSpec{Name: "username", Kind: ajvKindString, MinLength: 3, MaxLength: authLoginIdentifierMaxLength}},
	}
	authLoginFields = []authFieldSpec{
		{ajvFieldSpec: ajvFieldSpec{Name: "username", Kind: ajvKindString, MinLength: 3, MaxLength: authLoginIdentifierMaxLength}},
		{ajvFieldSpec: ajvFieldSpec{Name: "password", Kind: ajvKindString, MinLength: 6, MaxLength: 128}},
		{ajvFieldSpec: ajvFieldSpec{Name: "totpCode", Kind: ajvKindString, MinLength: 6, MaxLength: 6}},
		{ajvFieldSpec: ajvFieldSpec{Name: "recoveryCode", Kind: ajvKindString, MinLength: 8, MaxLength: 20}},
		{ajvFieldSpec: ajvFieldSpec{Name: "turnstileToken", Kind: ajvKindString, MinLength: -1, MaxLength: -1}},
	}
	authEmailTurnstileFields = []authFieldSpec{
		{ajvFieldSpec: ajvFieldSpec{Name: "email", Kind: ajvKindString, MinLength: -1, MaxLength: -1}, Format: "email"},
		{ajvFieldSpec: ajvFieldSpec{Name: "turnstileToken", Kind: ajvKindString, MinLength: -1, MaxLength: -1}},
	}
	authRegisterFields = []authFieldSpec{
		{ajvFieldSpec: ajvFieldSpec{
			Name:      "username",
			Kind:      ajvKindString,
			MinLength: 3,
			MaxLength: 32,
			Pattern:   regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`),
		}},
		{ajvFieldSpec: ajvFieldSpec{Name: "password", Kind: ajvKindString, MinLength: 8, MaxLength: 128}},
		{ajvFieldSpec: ajvFieldSpec{Name: "email", Kind: ajvKindString, MinLength: -1, MaxLength: -1}, Format: "email"},
		{ajvFieldSpec: ajvFieldSpec{Name: "inviteCode", Kind: ajvKindString, MinLength: -1, MaxLength: -1}},
		{ajvFieldSpec: ajvFieldSpec{Name: "turnstileToken", Kind: ajvKindString, MinLength: -1, MaxLength: -1}},
		{ajvFieldSpec: ajvFieldSpec{Name: "emailCode", Kind: ajvKindString, MinLength: 6, MaxLength: 6}},
	}
	authInviteCreateFields = []authFieldSpec{
		{ajvFieldSpec: ajvFieldSpec{Name: "expiresInDays", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()}},
		{ajvFieldSpec: ajvFieldSpec{Name: "count", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 100}},
	}
	authTwoFACodeFields = []authFieldSpec{
		{ajvFieldSpec: ajvFieldSpec{Name: "code", Kind: ajvKindString, MinLength: 6, MaxLength: 6}},
	}
	authTwoFAPasswordCodeFields = []authFieldSpec{
		{ajvFieldSpec: ajvFieldSpec{Name: "password", Kind: ajvKindString, MinLength: -1, MaxLength: -1}},
		{ajvFieldSpec: ajvFieldSpec{Name: "code", Kind: ajvKindString, MinLength: 6, MaxLength: 6}},
	}
	authForgotPasswordResetFields = []authFieldSpec{
		{ajvFieldSpec: ajvFieldSpec{Name: "email", Kind: ajvKindString, MinLength: -1, MaxLength: -1}, Format: "email"},
		{ajvFieldSpec: ajvFieldSpec{Name: "code", Kind: ajvKindString, MinLength: 6, MaxLength: 6}},
		{ajvFieldSpec: ajvFieldSpec{Name: "password", Kind: ajvKindString, MinLength: 8, MaxLength: 128}},
		{ajvFieldSpec: ajvFieldSpec{Name: "turnstileToken", Kind: ajvKindString, MinLength: -1, MaxLength: -1}},
	}
)

// authAJVMessage mirrors the setErrorHandler branches that depend on the field
// name. Everything else is already rendered by the shared AJV replica.
func authAJVMessage(message string) string {
	if message == "username is too short" {
		return "Username must be at least 3 characters"
	}
	return usersAJVMessage(message)
}

// authValidateBody reproduces an AJV object schema with allErrors:false:
// root type check, then `required` in declaration order, then properties in
// schema order (format:'email' is enforced where the property appears).
func authValidateBody(decoded any, required []string, fields []authFieldSpec) (map[string]any, string) {
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, "must be object"
	}
	for _, name := range required {
		if _, present := object[name]; !present {
			return nil, name + " is required"
		}
	}

	values := make(map[string]any, len(fields))
	for _, field := range fields {
		raw, present := object[field.Name]
		if !present {
			continue
		}
		coerced, message := ajvCoerceField(field.ajvFieldSpec, raw)
		if message != "" {
			return nil, authAJVMessage(message)
		}
		if field.Format == "email" {
			text, _ := coerced.(string)
			if !usersEmailFormatValid(text) {
				return nil, "Please enter a valid email address"
			}
		}
		values[field.Name] = coerced
	}
	return values, ""
}

// authReadValidatedBody runs the parser, then the schema. ok=false means the
// response was already written.
func authReadValidatedBody(c *gin.Context, required []string, fields []authFieldSpec) (map[string]any, bool) {
	decoded, status, message := authReadJSONBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return nil, false
	}
	values, validationMessage := authValidateBody(decoded, required, fields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return nil, false
	}
	return values, true
}

// authStringValue returns the coerced optional string property.
func authStringValue(values map[string]any, name string) *string {
	raw, present := values[name]
	if !present {
		return nil
	}
	text, ok := raw.(string)
	if !ok {
		return nil
	}
	return &text
}

// authIntValue returns the coerced optional integer property as an int.
func authIntValue(values map[string]any, name string) (int, bool) {
	raw, present := values[name]
	if !present {
		return 0, false
	}
	number, ok := raw.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return int(number), true
}

// authUserAgent mirrors `request.headers['user-agent'] || 'unknown'`.
func authUserAgent(c *gin.Context) string {
	if value := c.GetHeader("User-Agent"); value != "" {
		return value
	}
	return "unknown"
}

// ==================== refresh cookie ====================

// authSetRefreshCookie mirrors `reply.setCookie('refreshToken', token,
// getRefreshTokenCookieOptions())`. Every attribute comes from lib/cookie_config.go.
func authSetRefreshCookie(c *gin.Context, token string) {
	options := lib.GetRefreshTokenCookieOptions()
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     lib.RefreshTokenCookieName,
		Value:    token,
		Path:     options.Path,
		Domain:   options.Domain,
		MaxAge:   options.MaxAge,
		Secure:   options.Secure,
		HttpOnly: options.HTTPOnly,
		SameSite: authSameSiteMode(options.SameSite),
	})
}

// authClearRefreshCookie mirrors `reply.clearCookie('refreshToken',
// getClearCookieOptions())`: @fastify/cookie adds expires=Date(0) and maxAge=0 to
// the supplied path/domain.
func authClearRefreshCookie(c *gin.Context) {
	options := lib.GetClearCookieOptions()
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     lib.RefreshTokenCookieName,
		Value:    "",
		Path:     options.Path,
		Domain:   options.Domain,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0).UTC(),
		Secure:   false,
		HttpOnly: false,
	})
}

// authSameSiteMode maps the cookie_config decision onto net/http.
func authSameSiteMode(value string) http.SameSite {
	switch value {
	case "lax":
		return http.SameSiteLaxMode
	case "none":
		return http.SameSiteNoneMode
	case "strict":
		return http.SameSiteStrictMode
	}
	return http.SameSiteDefaultMode
}

// authReadRefreshCookie mirrors `request.cookies.refreshToken`.
func authReadRefreshCookie(c *gin.Context) string {
	value, err := c.Cookie(lib.RefreshTokenCookieName)
	if err != nil {
		return ""
	}
	return value
}

// ==================== POST /check-2fa ====================

func authCheck2FA(c *gin.Context) {
	values, ok := authReadValidatedBody(c, []string{"username"}, authUsernameLoginFields)
	if !ok {
		return
	}
	username, _ := values["username"].(string)
	ctx := c.Request.Context()

	user, err := db.FindUserByUsernameOrEmail(ctx, username)
	if err != nil {
		authInternalError(c)
		return
	}
	// Both "unknown user" and "banned" answer requires2FA:false on purpose: the
	// endpoint must not reveal whether an account exists or has 2FA configured.
	if user == nil || user.Status == models.UserStatusBanned {
		c.JSON(http.StatusOK, gin.H{"requires2FA": false})
		return
	}

	enabled, err := db.Is2FAEnabled(ctx, user.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"requires2FA": enabled})
}

// ==================== POST /login ====================

func authLogin(c *gin.Context) {
	values, ok := authReadValidatedBody(c, []string{"username", "password"}, authLoginFields)
	if !ok {
		return
	}
	if !authRunTurnstile(c) {
		return
	}

	username, _ := values["username"].(string)
	password, _ := values["password"].(string)
	totpCode := authStringValue(values, "totpCode")
	recoveryCode := authStringValue(values, "recoveryCode")

	ctx := c.Request.Context()
	clientIP := middleware.ClientIP(c)

	// Look the user up first so the lock key can use the stable account id: the
	// same account must lock no matter whether it was addressed by username or
	// by email.
	user, err := db.FindUserByUsernameOrEmail(ctx, username)
	if err != nil {
		authInternalError(c)
		return
	}

	lockKey := strings.ToLower(strings.TrimSpace(username))
	if user != nil {
		lockKey = fmt.Sprintf("user:%d", user.ID)
	}

	if status := lib.CheckLoginLock(clientIP, lockKey); status.Locked {
		var userID *int32
		logUsername := username
		if user != nil {
			userID = &user.ID
			logUsername = user.Username
		}
		authSecurityEvent(ctx, lib.SecurityEventRateLimitExceeded, userID, lib.SecurityEventDetails{
			IP:       clientIP,
			Username: logUsername,
			Action:   "login",
			Reason:   "Account locked due to too many failed attempts",
		})
		minutes := (status.RetryAfter + 59) / 60
		lib.RespondAPIError(c, http.StatusTooManyRequests, lib.ErrTooManyAttempts,
			fmt.Sprintf("%d minutes", minutes))
		return
	}

	if user == nil {
		lib.RecordLoginFailure(clientIP, username)
		authSecurityEvent(ctx, lib.SecurityEventLoginFailed, nil, lib.SecurityEventDetails{
			IP:       clientIP,
			Username: username,
			Reason:   "User not found",
		})
		lib.RespondAPIError(c, http.StatusUnauthorized, lib.ErrInvalidCredentials, "")
		return
	}

	if user.Status == models.UserStatusBanned {
		authSecurityEvent(ctx, lib.SecurityEventLoginFailed, &user.ID, lib.SecurityEventDetails{
			IP:       clientIP,
			Username: username,
			Reason:   "Account banned",
		})
		if user.BanReason != nil && *user.BanReason != "" {
			c.JSON(http.StatusForbidden, gin.H{"error": *user.BanReason, "code": lib.ErrAccountBanned})
			return
		}
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrAccountBanned, "")
		return
	}

	if !authPasswordMatches(user.PasswordHash, password) {
		lib.RecordLoginFailure(clientIP, fmt.Sprintf("user:%d", user.ID))
		authSecurityEvent(ctx, lib.SecurityEventLoginFailed, &user.ID, lib.SecurityEventDetails{
			IP:       clientIP,
			Username: user.Username,
			Reason:   "Invalid password",
		})
		lib.RespondAPIError(c, http.StatusUnauthorized, lib.ErrInvalidCredentials, "")
		return
	}

	twoFAEnabled, err := db.Is2FAEnabled(ctx, user.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	if twoFAEnabled {
		if totpCode == nil && recoveryCode == nil {
			lib.RecordLoginFailure(clientIP, fmt.Sprintf("user:%d", user.ID))
			authSecurityEvent(ctx, lib.SecurityEventLoginFailed, &user.ID, lib.SecurityEventDetails{
				IP:       clientIP,
				Username: user.Username,
				Reason:   "2FA required but not provided",
			})
			lib.RespondAPIError(c, http.StatusUnauthorized, lib.ErrTwoFaRequired, "")
			return
		}

		verified, proceed := authVerifySecondFactor(ctx, c, user, totpCode, recoveryCode)
		if !proceed {
			return
		}
		if !verified {
			// A malformed stored secret or recovery-code blob is a verification
			// failure, not an internal error (decryptSensitiveData returns the
			// input on failure, so the TOTP/JSON check rejects it).
			lib.RecordLoginFailure(clientIP, fmt.Sprintf("user:%d", user.ID))
			authSecurityEvent(ctx, lib.SecurityEventLoginFailed, &user.ID, lib.SecurityEventDetails{
				IP:       clientIP,
				Username: user.Username,
				Reason:   "Invalid 2FA code or recovery code",
			})
			lib.RespondAPIError(c, http.StatusUnauthorized, lib.ErrInvalid2faCode, "")
			return
		}
	}
	// Users without 2FA ignore any supplied code, exactly like the TypeScript.

	lib.ClearLoginFailure(clientIP, fmt.Sprintf("user:%d", user.ID))

	userAgent := authUserAgent(c)
	protectDemoAccount := usersShouldProtectDemoAccount(c, user)

	// Detect a new login location BEFORE creating the new session (the Node code
	// fixed a race by moving this ahead of generateRefreshToken).
	var loginAlert *lib.NewLoginLocation
	if user.Email != nil && *user.Email != "" && !protectDemoAccount {
		detected := lib.DetectNewLoginLocation(ctx, user.ID, clientIP, userAgent)
		loginAlert = &detected
	}

	refreshToken, err := lib.GenerateRefreshToken(ctx, user.ID, user.Username, user.Role, clientIP, userAgent)
	if err != nil {
		authInternalError(c)
		return
	}
	sessionID := lib.GetRefreshTokenSessionID(refreshToken)

	accessToken, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID:       user.ID,
		Username: user.Username,
		Role:     string(user.Role),
		SID:      sessionID,
	})
	if err != nil {
		authInternalError(c)
		return
	}

	authSetRefreshCookie(c, refreshToken)

	authSecurityEvent(ctx, lib.SecurityEventLoginSuccess, &user.ID, lib.SecurityEventDetails{
		IP:       clientIP,
		Username: username,
	})

	// Login records and alerts are fire-and-forget in Node; they must never block
	// or fail the login response.
	authRecordLoginAsync(user.ID, clientIP, userAgent, protectDemoAccount)

	if user.Email != nil && *user.Email != "" && loginAlert != nil && (loginAlert.IsNewIP || loginAlert.IsNewDevice) {
		email := *user.Email
		alert := *loginAlert
		go func() {
			mailCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			lib.SendLoginAlertEmail(mailCtx, email, lib.LoginAlertEmailData{
				Username:    user.Username,
				IP:          clientIP,
				UserAgent:   userAgent,
				LoginTime:   time.Now(),
				IsNewIP:     alert.IsNewIP,
				IsNewDevice: alert.IsNewDevice,
			})
		}()

		lib.SendNotification(ctx, int64(user.ID), lib.EventLoginNewDevice, lib.EventData{
			"instanceName": "",
			"deviceInfo":   userAgent,
			"ipAddress":    clientIP,
			"loginTime":    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"token": accessToken,
		"user":  authBuildUserPayload(user),
	})
}

// authVerifySecondFactor mirrors the TOTP-then-recovery-code block of /login.
// It returns (verified, proceed); proceed is false only after an internal error
// response has already been written (a failed code is a normal false).
func authVerifySecondFactor(ctx context.Context, c *gin.Context, user *db.UserRecord, totpCode, recoveryCode *string) (bool, bool) {
	verified := false

	if totpCode != nil {
		secret, err := db.Get2FASecret(ctx, user.ID)
		if err != nil {
			authInternalError(c)
			return false, false
		}
		if secret != nil {
			decrypted := lib.DecryptSensitiveData(*secret)
			if decrypted != "" && lib.Verify2FAToken(*totpCode, decrypted) {
				verified = true
			}
		}
	}

	if !verified && recoveryCode != nil {
		encryptedCodes, err := db.Get2FARecoveryCodes(ctx, user.ID)
		if err != nil {
			authInternalError(c)
			return false, false
		}
		if encryptedCodes != nil {
			decryptedJSON := lib.DecryptSensitiveData(*encryptedCodes)
			if decryptedJSON != "" {
				var codes []string
				if err := json.Unmarshal([]byte(decryptedJSON), &codes); err == nil {
					index := -1
					target := strings.ToUpper(*recoveryCode)
					for position, code := range codes {
						if code == target {
							index = position
							break
						}
					}
					if index != -1 {
						codes = append(codes[:index], codes[index+1:]...)
						encoded, err := json.Marshal(codes)
						if err != nil {
							authInternalError(c)
							return false, false
						}
						encrypted, err := lib.EncryptSensitiveData(string(encoded))
						if err != nil {
							authInternalError(c)
							return false, false
						}
						if err := db.Save2FARecoveryCodes(ctx, user.ID, encrypted); err != nil {
							authInternalError(c)
							return false, false
						}
						verified = true
						authSecurityEvent(ctx, lib.SecurityEventLoginSuccess, &user.ID, lib.SecurityEventDetails{
							IP:       middleware.ClientIP(c),
							Username: user.Username,
							Reason:   fmt.Sprintf("Used recovery code, %d remaining", len(codes)),
						})
					}
				}
			}
		}
	}

	return verified, true
}

// ==================== POST /send-verification-code ====================

func authSendVerificationCode(c *gin.Context) {
	values, ok := authReadValidatedBody(c, []string{"email"}, authEmailTurnstileFields)
	if !ok {
		return
	}
	if !authRunTurnstile(c) {
		return
	}

	email, _ := values["email"].(string)
	ctx := c.Request.Context()

	if !authEnsureRegistrationEnabled(c, ctx) {
		return
	}

	smtpEnabled, err := lib.IsSMTPEnabled(ctx)
	if err != nil {
		authInternalError(c)
		return
	}
	if !smtpEnabled {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailVerificationDisabled, "")
		return
	}

	if !strings.Contains(email, "@") {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidEmail, "")
		return
	}
	emailWithoutAllowed := strings.ReplaceAll(strings.ReplaceAll(email, "@", ""), ".", "")
	if lib.ContainsDangerousChars(emailWithoutAllowed) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailContainsIllegal, "")
		return
	}

	existingUser, err := db.FindUserByEmail(ctx, email)
	if err != nil {
		authInternalError(c)
		return
	}
	if existingUser != nil {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailAlreadyRegistered, "")
		return
	}

	domain, valid, err := usersValidateEmailDomain(ctx, email)
	if err != nil {
		authInternalError(c)
		return
	}
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailDomainNotAllowed, domain)
		return
	}

	result, err := db.AuthCreateVerificationCode(ctx, email, db.AuthEmailVerificationPurposeRegister)
	if err != nil {
		authInternalError(c)
		return
	}
	if result == nil {
		lib.RespondAPIError(c, http.StatusTooManyRequests, lib.ErrTooManyVerificationRequests, "")
		return
	}

	sendResult := lib.SendVerificationEmail(ctx, email, result.Code, authVerificationEmailExpiryMinutes, lib.VerificationPurposeRegister)
	if !sendResult.Success {
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrEmailSendFailed, "")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":   "Verification code sent",
		"expiresAt": usersISOTime(result.ExpiresAt),
	})
}

// authVerificationEmailExpiryMinutes mirrors the `10` passed to
// sendVerificationEmail'.
const authVerificationEmailExpiryMinutes = 10

// authEnsureRegistrationEnabled mirrors ensureRegistrationEnabled: 403 when the
// registration switch is off.
func authEnsureRegistrationEnabled(c *gin.Context, ctx context.Context) bool {
	enabled, err := db.IsRegistrationEnabled(ctx)
	if err != nil {
		authInternalError(c)
		return false
	}
	if !enabled {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrRegistrationDisabled, "")
		return false
	}
	return true
}

// ==================== POST /register ====================

func authRegister(c *gin.Context) {
	values, ok := authReadValidatedBody(c, []string{"username", "password", "email"}, authRegisterFields)
	if !ok {
		return
	}
	if !authRunTurnstile(c) {
		return
	}

	username, _ := values["username"].(string)
	password, _ := values["password"].(string)
	email, _ := values["email"].(string)
	inviteCode := authStringValue(values, "inviteCode")
	emailCode := authStringValue(values, "emailCode")

	ctx := c.Request.Context()
	clientIP := middleware.ClientIP(c)

	if !authEnsureRegistrationEnabled(c, ctx) {
		return
	}

	usernameValidation := lib.ValidateIdentifier(username, "Username", 3, 32)
	if !usernameValidation.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": usernameValidation.Message})
		return
	}
	if lib.ContainsDangerousChars(username) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrUsernameContainsIllegal, "")
		return
	}

	passwordCheck := lib.ValidatePassword(password)
	if !passwordCheck.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": passwordCheck.Message})
		return
	}

	if !strings.Contains(email, "@") {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidEmail, "")
		return
	}
	emailWithoutAllowed := strings.ReplaceAll(strings.ReplaceAll(email, "@", ""), ".", "")
	if lib.ContainsDangerousChars(emailWithoutAllowed) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailContainsIllegal, "")
		return
	}

	domain, valid, err := usersValidateEmailDomain(ctx, email)
	if err != nil {
		authInternalError(c)
		return
	}
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailDomainNotAllowed, domain)
		return
	}

	smtpEnabled, err := lib.IsSMTPEnabled(ctx)
	if err != nil {
		authInternalError(c)
		return
	}
	if smtpEnabled {
		if emailCode == nil {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailCodeRequired, "")
			return
		}
		isValid, err := db.AuthVerifyCode(ctx, email, *emailCode, db.AuthEmailVerificationPurposeRegister)
		if err != nil {
			authInternalError(c)
			return
		}
		if !isValid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidEmailCode, "")
			return
		}
	}

	requireInvite, err := db.IsInviteCodeRequired(ctx)
	if err != nil {
		authInternalError(c)
		return
	}

	var invite *db.UserInvitesCodeRow
	if requireInvite {
		if inviteCode == nil {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidInviteCode, "")
			return
		}
		invite, err = db.UserInvitesFindByCode(ctx, *inviteCode)
		if err != nil {
			authInternalError(c)
			return
		}
		if invite == nil {
			authSecurityEvent(ctx, lib.SecurityEventSuspiciousActivity, nil, lib.SecurityEventDetails{
				IP:       clientIP,
				Action:   "register",
				Reason:   "Invalid invite code",
				Username: username,
			})
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidInviteCode, "")
			return
		}
		if invite.ExpiresAt != nil && invite.ExpiresAt.Before(time.Now()) {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInviteCodeExpired, "")
			return
		}
	}

	existingUser, err := db.FindUserByUsername(ctx, username)
	if err != nil {
		authInternalError(c)
		return
	}
	if existingUser != nil {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrUserExists, "")
		return
	}

	existingUserByEmail, err := db.FindUserByEmail(ctx, email)
	if err != nil {
		authInternalError(c)
		return
	}
	if existingUserByEmail != nil {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailAlreadyRegistered, "")
		return
	}

	passwordHash, err := authHashPassword(password)
	if err != nil {
		authInternalError(c)
		return
	}

	normalizedEmail := strings.ToLower(strings.TrimSpace(email))

	var inviteArg *string
	if invite != nil && inviteCode != nil {
		inviteArg = inviteCode
	}

	created, err := db.CreateRegisteredUser(ctx, db.CreateRegisteredUserInput{
		Username:     username,
		Email:        &normalizedEmail,
		PasswordHash: passwordHash,
		InviteCode:   inviteArg,
	})
	if err != nil {
		switch {
		case errors.Is(err, db.ErrInviteCodeUnavailable):
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidInviteCode, "")
		case errors.Is(err, db.ErrUserExists):
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrUserExists, "")
		case errors.Is(err, db.ErrEmailAlreadyRegistered):
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailAlreadyRegistered, "")
		default:
			authInternalError(c)
		}
		return
	}
	userID := created.UserID
	registerGift := created.RegisterGift

	authSecurityEvent(ctx, lib.SecurityEventRegisterSuccess, &userID, lib.SecurityEventDetails{
		IP:       clientIP,
		Username: username,
	})

	newUser, err := db.FindUserById(ctx, userID)
	if err != nil {
		authInternalError(c)
		return
	}
	if newUser == nil {
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrUserNotFound, "")
		return
	}

	if registerGift != nil && newUser.Email != nil && *newUser.Email != "" {
		emailAddress := *newUser.Email
		gift := *registerGift
		go func() {
			mailCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			lib.SendFreeSiteRegisterGiftEmail(mailCtx, emailAddress, lib.FreeSiteRegisterGiftEmailData{
				Username:      newUser.Username,
				BalanceAmount: gift.BalanceAmount,
				PointsAmount:  float64(gift.PointsAmount),
				NewBalance:    gift.NewBalance,
				NewPoints:     float64(gift.NewPoints),
				Time:          time.Now(),
			})
		}()
	}

	userAgent := authUserAgent(c)
	refreshToken, err := lib.GenerateRefreshToken(ctx, newUser.ID, newUser.Username, newUser.Role, clientIP, userAgent)
	if err != nil {
		authInternalError(c)
		return
	}
	sessionID := lib.GetRefreshTokenSessionID(refreshToken)

	accessToken, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID:       newUser.ID,
		Username: newUser.Username,
		Role:     string(newUser.Role),
		SID:      sessionID,
	})
	if err != nil {
		authInternalError(c)
		return
	}

	authSetRefreshCookie(c, refreshToken)

	authRecordLoginAsync(newUser.ID, clientIP, userAgent, false)

	c.JSON(http.StatusCreated, gin.H{
		"token": accessToken,
		"user":  authBuildUserPayload(newUser),
	})
}

// ==================== GET /me ====================

func authMe(c *gin.Context) {
	claims := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	user, err := db.FindUserById(ctx, claims.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	quota, err := db.GetUserQuota(ctx, user.ID)
	if err != nil {
		authInternalError(c)
		return
	}

	payload := authMeUserPayload{
		ID:            user.ID,
		Username:      user.Username,
		Email:         user.Email,
		Role:          user.Role,
		AvatarStyle:   user.AvatarStyle,
		AvatarBadgeID: user.AvatarBadgeID,
		Quota:         nil,
	}
	if quota != nil {
		payload.Quota = &authQuotaPayload{
			HostLimit:    quota.HostLimit,
			HostUsed:     quota.HostUsed,
			FriendLimit:  quota.FriendLimit,
			FriendUsed:   quota.FriendUsed,
			PackageLimit: quota.PackageLimit,
			PackageUsed:  quota.PackageUsed,
		}
	}

	c.JSON(http.StatusOK, gin.H{"user": payload})
}

// ==================== POST /logout ====================

func authLogout(c *gin.Context) {
	ctx := c.Request.Context()
	clientIP := middleware.ClientIP(c)
	userAgent := c.GetHeader("User-Agent")

	refreshToken := authReadRefreshCookie(c)

	sessionIDs := make([]string, 0, 2)
	seenSessions := map[string]bool{}
	addSession := func(sessionID string) {
		if sessionID == "" || seenSessions[sessionID] {
			return
		}
		seenSessions[sessionID] = true
		sessionIDs = append(sessionIDs, sessionID)
	}

	var userID *int32
	username := ""

	// request.jwtVerify() is optional here: logout must still clear the HttpOnly
	// refresh cookie when the access token is expired or invalid.
	if header := c.GetHeader("Authorization"); len(header) > 7 && strings.EqualFold(header[:7], "Bearer ") {
		if claims, err := lib.VerifyAccessToken(header[7:]); err == nil {
			if claims.ID != 0 {
				id := claims.ID
				userID = &id
				username = claims.Username
			}
			addSession(claims.SID)
		}
	}

	if refreshToken != "" {
		addSession(lib.GetRefreshTokenSessionID(refreshToken))
		if tokenData := lib.VerifyRefreshToken(ctx, refreshToken, clientIP, userAgent); tokenData != nil {
			id := tokenData.UserID
			userID = &id
			username = tokenData.Username
		}
	}

	if refreshToken != "" {
		lib.RevokeRefreshToken(ctx, refreshToken)
	}

	if userID != nil {
		for _, sessionID := range sessionIDs {
			lib.InvalidateSessionAccessToken(ctx, *userID, sessionID)
			AuthRevokeActionTicketsForSession(sessionID)
			AuthCloseSessionTerminalSessions(sessionID, "Session logged out")
		}
	}

	authClearRefreshCookie(c)

	if userID != nil {
		authSecurityEvent(ctx, lib.SecurityEventLogout, userID, lib.SecurityEventDetails{
			Username: username,
		})
	}

	c.JSON(http.StatusOK, gin.H{"message": "Logged out"})
}

// ==================== POST /refresh ====================

func authRefresh(c *gin.Context) {
	ctx := c.Request.Context()
	clientIP := middleware.ClientIP(c)
	userAgent := c.GetHeader("User-Agent")

	refreshToken := authReadRefreshCookie(c)
	if refreshToken == "" {
		lib.RespondAPIError(c, http.StatusUnauthorized, lib.ErrRefreshTokenMissing, "")
		return
	}

	// The Node retry loop exists for transient database errors; verifyRefreshToken
	// never surfaces those (it returns nil), so a single call is equivalent.
	tokenData := lib.VerifyRefreshToken(ctx, refreshToken, clientIP, userAgent)
	if tokenData == nil {
		authClearRefreshCookie(c)
		lib.RespondAPIError(c, http.StatusUnauthorized, lib.ErrRefreshTokenInvalid, "")
		return
	}

	user, err := db.FindUserById(ctx, tokenData.UserID)
	if err != nil {
		authInternalError(c)
		return
	}

	if user == nil || user.Status == models.UserStatusBanned {
		lib.RevokeRefreshToken(ctx, refreshToken)
		authClearRefreshCookie(c)
		if user != nil && user.BanReason != nil && *user.BanReason != "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": *user.BanReason, "code": lib.ErrAccountBanned})
			return
		}
		lib.RespondAPIError(c, http.StatusUnauthorized, lib.ErrAccountBanned, "")
		return
	}

	sessionID := lib.GetRefreshTokenSessionID(refreshToken)

	// updateSessionActivity swallows its own errors in Node (returns null), so the
	// Go error is deliberately ignored: a failure only means the cookie is not
	// re-sent below.
	sessionUpdate, _ := lib.UpdateSessionActivity(ctx, refreshToken)

	newAccessToken, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID:       user.ID,
		Username: user.Username,
		Role:     string(user.Role),
		SID:      sessionID,
	})
	if err != nil {
		authInternalError(c)
		return
	}

	if sessionUpdate != nil {
		authSetRefreshCookie(c, refreshToken)
	}

	c.JSON(http.StatusOK, gin.H{"token": newAccessToken})
}

// ==================== POST /invite (admin) ====================

// authInviteCodeAlphabet is nanoid v3's default urlAlphabet, which is what
// `nanoid(12)` draws from.
const authInviteCodeAlphabet = "useandom-26T198340PX75pxJACKVERYMINDBUSHWOLF_GQZbfghjklqvwyzrict"

// authNanoID mirrors nanoid(size) for size <= 64: each byte selects one of the 64
// alphabet characters with `byte & 63` (the same masking nanoid applies).
func authNanoID(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, buffer); err != nil {
		return "", err
	}
	result := make([]byte, size)
	for index, value := range buffer {
		result[index] = authInviteCodeAlphabet[value&63]
	}
	return string(result), nil
}

func authInvite(c *gin.Context) {
	claims := middleware.CurrentUser(c)

	// Fastify validates the body (preValidation) BEFORE the handler runs, so a
	// non-admin with a malformed body gets the validation error, not 403. The
	// admin check below therefore sits after the schema check, exactly where the
	// TypeScript handler starts.
	decoded, status, message := authReadJSONBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	values, validationMessage := authValidateBody(decoded, nil, authInviteCreateFields)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	if claims.Role != "admin" {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrAdminRequired, "")
		return
	}

	ctx := c.Request.Context()

	count := 1
	if parsed, present := authIntValue(values, "count"); present {
		count = parsed
	}

	var expiresAt *string
	if days, present := authIntValue(values, "expiresInDays"); present {
		date := time.Now().AddDate(0, 0, days)
		formatted := usersISOTime(date)
		expiresAt = &formatted
	}

	var expiresAtTime *time.Time
	if expiresAt != nil {
		if parsed, err := time.Parse(time.RFC3339, *expiresAt); err == nil {
			expiresAtTime = &parsed
		}
	}

	codes := make([]string, 0, count)
	for index := 0; index < count; index++ {
		code, err := authNanoID(12)
		if err != nil {
			authInternalError(c)
			return
		}
		if _, err := db.UserInvitesCreate(ctx, code, claims.ID, expiresAtTime); err != nil {
			authInternalError(c)
			return
		}
		codes = append(codes, code)
	}

	if count == 1 {
		c.JSON(http.StatusOK, gin.H{
			"code":      codes[0],
			"expiresAt": expiresAt,
			"url":       "/register/" + codes[0],
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"codes":     codes,
		"count":     len(codes),
		"expiresAt": expiresAt,
	})
}

// ==================== GET /invites (admin) ====================

// authInviteRow is the admin listing projection: the invite row plus the creator
// and used-by user summaries that Prisma's `include` returns.
type authInviteRow struct {
	ID                 int32      `gorm:"column:id"`
	Code               string     `gorm:"column:code"`
	CreatedBy          int32      `gorm:"column:created_by"`
	UsedBy             *int32     `gorm:"column:used_by"`
	UsedAt             *time.Time `gorm:"column:used_at"`
	ExpiresAt          *time.Time `gorm:"column:expires_at"`
	CostSnapshot       []byte     `gorm:"column:cost_snapshot"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	CreatorUsername    *string    `gorm:"column:creator_username"`
	CreatorEmail       *string    `gorm:"column:creator_email"`
	CreatorAvatarStyle *string    `gorm:"column:creator_avatar_style"`
	UsedUsername       *string    `gorm:"column:used_username"`
	UsedEmail          *string    `gorm:"column:used_email"`
	UsedAvatarStyle    *string    `gorm:"column:used_avatar_style"`
}

// authInviteItem mirrors the route's response mapping.
type authInviteItem struct {
	ID                   int32           `json:"id"`
	Code                 string          `json:"code"`
	CreatedBy            int32           `json:"createdBy"`
	CreatedByUsername    *string         `json:"createdByUsername"`
	CreatedByEmail       *string         `json:"createdByEmail"`
	CreatedByAvatarStyle *string         `json:"createdByAvatarStyle"`
	UsedBy               *int32          `json:"usedBy"`
	UsedByUsername       *string         `json:"usedByUsername"`
	UsedByEmail          *string         `json:"usedByEmail"`
	UsedByAvatarStyle    *string         `json:"usedByAvatarStyle"`
	UsedAt               *string         `json:"usedAt"`
	ExpiresAt            *string         `json:"expiresAt"`
	CostSnapshot         json.RawMessage `json:"costSnapshot"`
	CreatedAt            string          `json:"createdAt"`
}

// authNullableString mirrors `value || null`: empty strings collapse to null.
func authNullableString(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	return value
}

// authTimeString mirrors `date?.toISOString() ?? null`.
func authTimeString(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := usersISOTime(*value)
	return &formatted
}

func authInvitesList(c *gin.Context) {
	claims := middleware.CurrentUser(c)
	if claims.Role != "admin" {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrAdminRequired, "")
		return
	}

	page := lib.ParsePositiveIntegerQuery(authSingleQueryValue(c, "page"), 1, math.MaxInt32)
	pageSize := lib.ParsePositiveIntegerQuery(authSingleQueryValue(c, "pageSize"), 20, 100)

	status := ""
	switch authSingleQueryValue(c, "status") {
	case "used", "unused":
		status = authSingleQueryValue(c, "status")
	}

	ctx := c.Request.Context()

	where := ""
	switch status {
	case "used":
		where = " WHERE c.used_by IS NOT NULL"
	case "unused":
		where = " WHERE c.used_by IS NULL"
	}

	var total int64
	if err := db.DB.WithContext(ctx).Raw("SELECT COUNT(*) FROM invite_codes c" + where).Scan(&total).Error; err != nil {
		authInternalError(c)
		return
	}

	offset := (page - 1) * pageSize
	var rows []authInviteRow
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT c.id, c.code, c.created_by, c.used_by, c.used_at, c.expires_at,
		        c.cost_snapshot, c.created_at,
		        cb.username AS creator_username,
		        cb.email AS creator_email,
		        cb.avatar_style AS creator_avatar_style,
		        ub.username AS used_username,
		        ub.email AS used_email,
		        ub.avatar_style AS used_avatar_style
		   FROM invite_codes c
		   LEFT JOIN users cb ON cb.id = c.created_by
		   LEFT JOIN users ub ON ub.id = c.used_by`+where+`
		  ORDER BY c.id DESC LIMIT ? OFFSET ?`,
		pageSize, offset,
	).Scan(&rows).Error; err != nil {
		authInternalError(c)
		return
	}

	invites := make([]authInviteItem, 0, len(rows))
	for _, row := range rows {
		if row.CreatorUsername == nil {
			// Prisma's required `creator` relation would be null and the TypeScript
			// projection (`i.createdByUsername`) would throw; keep the same 500.
			authInternalError(c)
			return
		}
		var costSnapshot json.RawMessage
		if row.CostSnapshot != nil {
			costSnapshot = json.RawMessage(row.CostSnapshot)
		}
		invites = append(invites, authInviteItem{
			ID:                   row.ID,
			Code:                 row.Code,
			CreatedBy:            row.CreatedBy,
			CreatedByUsername:    authNullableString(row.CreatorUsername),
			CreatedByEmail:       authNullableString(row.CreatorEmail),
			CreatedByAvatarStyle: authNullableString(row.CreatorAvatarStyle),
			UsedBy:               row.UsedBy,
			UsedByUsername:       authNullableString(row.UsedUsername),
			UsedByEmail:          authNullableString(row.UsedEmail),
			UsedByAvatarStyle:    authNullableString(row.UsedAvatarStyle),
			UsedAt:               authTimeString(row.UsedAt),
			ExpiresAt:            authTimeString(row.ExpiresAt),
			CostSnapshot:         costSnapshot,
			CreatedAt:            usersISOTime(row.CreatedAt),
		})
	}

	totalPages := 1
	if pageSize > 0 {
		totalPages = int(math.Max(1, math.Ceil(float64(total)/float64(pageSize))))
	}

	c.JSON(http.StatusOK, gin.H{
		"invites":    invites,
		"total":      total,
		"page":       page,
		"pageSize":   pageSize,
		"totalPages": totalPages,
	})
}

// ==================== DELETE /invites/:id (admin) ====================

func authInviteDelete(c *gin.Context) {
	claims := middleware.CurrentUser(c)
	if claims.Role != "admin" {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrAdminRequired, "")
		return
	}

	id, ok := lib.ParsePositiveRouteID(c.Param("id"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}
	// Prisma's Int is 32-bit: an id above int32 max makes findUnique throw, so the
	// Node handler answers 500. Truncating here (like a bare int32 cast) would hit
	// a *different* invite code, so the range check is mandatory before any write.
	if id > math.MaxInt32 {
		authInternalError(c)
		return
	}

	ctx := c.Request.Context()
	invite, err := db.UserInvitesGetByID(ctx, int32(id))
	if err != nil {
		authInternalError(c)
		return
	}
	if invite == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInviteCodeNotFound, "")
		return
	}

	// Used codes may be deleted too; Prisma's delete would throw on a vanished row
	// and the route does not catch it, so a delete failure is a 500.
	if err := db.UserInvitesDelete(ctx, int32(id)); err != nil {
		authInternalError(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Invite code deleted"})
}

// ==================== 2FA ====================

func auth2FAStatus(c *gin.Context) {
	claims := middleware.CurrentUser(c)
	status, err := db.GetUser2FAStatus(c.Request.Context(), claims.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": status.Enabled})
}

func auth2FASetup(c *gin.Context) {
	claims := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	status, err := db.GetUser2FAStatus(ctx, claims.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	if status.Enabled {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTwoFaAlreadyEnabled, "")
		return
	}

	secret, err := lib.Generate2FASecret()
	if err != nil {
		authInternalError(c)
		return
	}

	encryptedSecret, err := lib.EncryptSensitiveData(secret)
	if err != nil {
		authInternalError(c)
		return
	}
	if err := db.Save2FASecret(ctx, claims.ID, encryptedSecret); err != nil {
		authInternalError(c)
		return
	}

	qrCode, err := lib.Generate2FAQRCode(claims.Username, secret)
	if err != nil {
		authInternalError(c)
		return
	}

	recoveryCodes, err := lib.GenerateRecoveryCodes()
	if err != nil {
		authInternalError(c)
		return
	}
	encoded, err := json.Marshal(recoveryCodes)
	if err != nil {
		authInternalError(c)
		return
	}
	encryptedCodes, err := lib.EncryptSensitiveData(string(encoded))
	if err != nil {
		authInternalError(c)
		return
	}
	if err := db.Save2FARecoveryCodes(ctx, claims.ID, encryptedCodes); err != nil {
		authInternalError(c)
		return
	}

	userID := int64(claims.ID)
	db.CreateLog(ctx, &userID, db.LogModuleSecurity, "2fa.setup", "Started 2FA setup", db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{
		"secret":        secret,
		"qrCode":        qrCode,
		"recoveryCodes": recoveryCodes,
	})
}

func auth2FAEnable(c *gin.Context) {
	values, ok := authReadValidatedBody(c, []string{"code"}, authTwoFACodeFields)
	if !ok {
		return
	}
	claims := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	code, _ := values["code"].(string)

	encryptedSecret, err := db.Get2FASecret(ctx, claims.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	if encryptedSecret == nil {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTwoFaNotEnabled, "")
		return
	}

	secret := lib.DecryptSensitiveData(*encryptedSecret)
	if secret == "" || !lib.Verify2FAToken(code, secret) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalid2faCode, "")
		return
	}

	if err := db.Enable2FA(ctx, claims.ID); err != nil {
		authInternalError(c)
		return
	}
	if _, err := lib.RevokeAllUserRefreshTokens(ctx, claims.ID); err != nil {
		authInternalError(c)
		return
	}
	lib.InvalidateUserAccessTokens(ctx, claims.ID)
	UsersCloseUserSessions(claims.ID, "2FA enabled")

	userID := int64(claims.ID)
	db.CreateLog(ctx, &userID, db.LogModuleSecurity, "2fa.enable", "Enabled two-factor authentication", db.LogResultSuccess, db.CreateLogOptions{})

	lib.SendNotification(ctx, userID, lib.Event2FAEnabled, lib.EventData{"instanceName": ""})

	c.JSON(http.StatusOK, gin.H{"message": "2FA enabled"})
}

func auth2FADisable(c *gin.Context) {
	values, ok := authReadValidatedBody(c, []string{"password", "code"}, authTwoFAPasswordCodeFields)
	if !ok {
		return
	}
	claims := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	password, _ := values["password"].(string)
	code, _ := values["code"].(string)

	user, err := db.FindUserById(ctx, claims.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	if !authPasswordMatches(user.PasswordHash, password) {
		lib.RespondAPIError(c, http.StatusUnauthorized, lib.ErrInvalidCredentials, "")
		return
	}

	encryptedSecret, err := db.Get2FASecret(ctx, claims.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	if encryptedSecret != nil {
		secret := lib.DecryptSensitiveData(*encryptedSecret)
		if secret == "" || !lib.Verify2FAToken(code, secret) {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalid2faCode, "")
			return
		}
	}

	// Sensitive operation second factor: claim only after password and TOTP pass,
	// so a wrong input cannot burn the verification code.
	verification, err := lib.ClaimOperationVerificationRequirement(ctx, int64(claims.ID), models.OperationTypeDisable2fa, 0)
	if err != nil {
		authInternalError(c)
		return
	}
	if !verification.Verified {
		c.JSON(http.StatusForbidden, gin.H{
			"error":         "Sensitive operation requires verification",
			"code":          "VERIFICATION_REQUIRED",
			"operationType": string(models.OperationTypeDisable2fa),
		})
		return
	}

	if err := db.Disable2FAComplete(ctx, claims.ID); err != nil {
		authInternalError(c)
		return
	}
	if _, err := lib.RevokeAllUserRefreshTokens(ctx, claims.ID); err != nil {
		authInternalError(c)
		return
	}
	lib.InvalidateUserAccessTokens(ctx, claims.ID)
	UsersCloseUserSessions(claims.ID, "2FA disabled")

	userID := int64(claims.ID)
	db.CreateLog(ctx, &userID, db.LogModuleSecurity, "2fa.disable", "Disabled two-factor authentication", db.LogResultSuccess, db.CreateLogOptions{})

	lib.SendNotification(ctx, userID, lib.Event2FADisabled, lib.EventData{"instanceName": ""})

	c.JSON(http.StatusOK, gin.H{"message": "2FA disabled"})
}

func auth2FARecoveryCodes(c *gin.Context) {
	claims := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	status, err := db.GetUser2FAStatus(ctx, claims.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	if !status.Enabled {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTwoFaNotEnabled, "")
		return
	}

	encryptedCodes, err := db.Get2FARecoveryCodes(ctx, claims.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	empty := gin.H{"total": authRecoveryCodeTotal, "remaining": 0, "used": authRecoveryCodeTotal}
	if encryptedCodes == nil {
		c.JSON(http.StatusOK, empty)
		return
	}

	decryptedJSON := lib.DecryptSensitiveData(*encryptedCodes)
	if decryptedJSON == "" {
		c.JSON(http.StatusOK, empty)
		return
	}

	var codes []string
	if err := json.Unmarshal([]byte(decryptedJSON), &codes); err != nil {
		c.JSON(http.StatusOK, empty)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"total":     authRecoveryCodeTotal,
		"remaining": len(codes),
		"used":      authRecoveryCodeTotal - len(codes),
	})
}

func auth2FARegenerateRecoveryCodes(c *gin.Context) {
	values, ok := authReadValidatedBody(c, []string{"password", "code"}, authTwoFAPasswordCodeFields)
	if !ok {
		return
	}
	claims := middleware.CurrentUser(c)
	ctx := c.Request.Context()
	password, _ := values["password"].(string)
	code, _ := values["code"].(string)

	status, err := db.GetUser2FAStatus(ctx, claims.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	if !status.Enabled {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTwoFaNotEnabled, "")
		return
	}

	user, err := db.FindUserById(ctx, claims.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	if !authPasswordMatches(user.PasswordHash, password) {
		lib.RespondAPIError(c, http.StatusUnauthorized, lib.ErrInvalidCredentials, "")
		return
	}

	encryptedSecret, err := db.Get2FASecret(ctx, claims.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	if encryptedSecret != nil {
		secret := lib.DecryptSensitiveData(*encryptedSecret)
		if secret == "" || !lib.Verify2FAToken(code, secret) {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalid2faCode, "")
			return
		}
	}

	recoveryCodes, err := lib.GenerateRecoveryCodes()
	if err != nil {
		authInternalError(c)
		return
	}
	encoded, err := json.Marshal(recoveryCodes)
	if err != nil {
		authInternalError(c)
		return
	}
	encryptedCodes, err := lib.EncryptSensitiveData(string(encoded))
	if err != nil {
		authInternalError(c)
		return
	}
	if err := db.Save2FARecoveryCodes(ctx, claims.ID, encryptedCodes); err != nil {
		authInternalError(c)
		return
	}

	userID := int64(claims.ID)
	db.CreateLog(ctx, &userID, db.LogModuleSecurity, "2fa.recovery_reset", "Regenerated 2FA recovery codes", db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"recoveryCodes": recoveryCodes})
}

// ==================== forgot password ====================

func authForgotPasswordSendCode(c *gin.Context) {
	values, ok := authReadValidatedBody(c, []string{"email"}, authEmailTurnstileFields)
	if !ok {
		return
	}
	if !authRunTurnstile(c) {
		return
	}

	email, _ := values["email"].(string)
	ctx := c.Request.Context()

	smtpEnabled, err := lib.IsSMTPEnabled(ctx)
	if err != nil {
		authInternalError(c)
		return
	}
	if !smtpEnabled {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailVerificationDisabled, "")
		return
	}

	if !strings.Contains(email, "@") {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidEmail, "")
		return
	}
	emailWithoutAllowed := strings.ReplaceAll(strings.ReplaceAll(email, "@", ""), ".", "")
	if lib.ContainsDangerousChars(emailWithoutAllowed) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailContainsIllegal, "")
		return
	}

	user, err := db.FindUserByEmail(ctx, email)
	if err != nil {
		authInternalError(c)
		return
	}
	if user == nil {
		// Do not reveal whether the address is registered: answer exactly like a
		// successful send, but skip the mail.
		c.JSON(http.StatusOK, gin.H{
			"message":   "If the email is registered, a verification code will be sent",
			"expiresAt": usersISOTime(time.Now().Add(10 * time.Minute)),
		})
		return
	}
	if user.Status == models.UserStatusBanned {
		if user.BanReason != nil && *user.BanReason != "" {
			c.JSON(http.StatusForbidden, gin.H{"error": *user.BanReason, "code": lib.ErrAccountBanned})
			return
		}
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrAccountBanned, "")
		return
	}

	result, err := db.AuthCreateVerificationCode(ctx, email, db.AuthEmailVerificationPurposePasswordReset)
	if err != nil {
		authInternalError(c)
		return
	}
	if result == nil {
		lib.RespondAPIError(c, http.StatusTooManyRequests, lib.ErrTooManyVerificationRequests, "")
		return
	}

	sendResult := lib.SendVerificationEmail(ctx, email, result.Code, authVerificationEmailExpiryMinutes, lib.VerificationPurposeResetPassword)
	if !sendResult.Success {
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrEmailSendFailed, "")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":   "Verification code sent",
		"expiresAt": usersISOTime(result.ExpiresAt),
	})
}

func authForgotPasswordReset(c *gin.Context) {
	values, ok := authReadValidatedBody(c, []string{"email", "code", "password"}, authForgotPasswordResetFields)
	if !ok {
		return
	}
	if !authRunTurnstile(c) {
		return
	}

	email, _ := values["email"].(string)
	code, _ := values["code"].(string)
	password, _ := values["password"].(string)
	ctx := c.Request.Context()
	clientIP := middleware.ClientIP(c)

	smtpEnabled, err := lib.IsSMTPEnabled(ctx)
	if err != nil {
		authInternalError(c)
		return
	}
	if !smtpEnabled {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailVerificationDisabled, "")
		return
	}

	if !strings.Contains(email, "@") {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidEmail, "")
		return
	}
	emailWithoutAllowed := strings.ReplaceAll(strings.ReplaceAll(email, "@", ""), ".", "")
	if lib.ContainsDangerousChars(emailWithoutAllowed) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailContainsIllegal, "")
		return
	}

	// Validate the new password BEFORE consuming the email code, otherwise a weak
	// password would burn the code.
	passwordCheck := lib.ValidatePassword(password)
	if !passwordCheck.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": passwordCheck.Message})
		return
	}

	user, err := db.FindUserByEmail(ctx, email)
	if err != nil {
		authInternalError(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidEmailCode, "")
		return
	}
	if user.Status == models.UserStatusBanned {
		if user.BanReason != nil && *user.BanReason != "" {
			c.JSON(http.StatusForbidden, gin.H{"error": *user.BanReason, "code": lib.ErrAccountBanned})
			return
		}
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrAccountBanned, "")
		return
	}

	isValid, err := db.AuthVerifyCode(ctx, email, code, db.AuthEmailVerificationPurposePasswordReset)
	if err != nil {
		authInternalError(c)
		return
	}
	if !isValid {
		authSecurityEvent(ctx, lib.SecurityEventSuspiciousActivity, nil, lib.SecurityEventDetails{
			IP:       clientIP,
			Action:   "forgot_password_verify_code",
			Reason:   "Invalid verification code",
			Username: email, // tracked for abuse, never reflected in the response
		})
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidEmailCode, "")
		return
	}

	passwordHash, err := authHashPassword(password)
	if err != nil {
		authInternalError(c)
		return
	}

	isTwoFAEnabled, err := db.Is2FAEnabled(ctx, user.ID)
	if err != nil {
		authInternalError(c)
		return
	}
	twoFactorDisabled := false
	if isTwoFAEnabled {
		if err := db.Disable2FAComplete(ctx, user.ID); err != nil {
			authInternalError(c)
			return
		}
		twoFactorDisabled = true
		disabledUserID := int64(user.ID)
		db.CreateLog(ctx, &disabledUserID, db.LogModuleSecurity, "2fa.disable", "2FA disabled due to password reset", db.LogResultSuccess, db.CreateLogOptions{})
	}

	if err := db.UpdateUser(ctx, user.ID, db.UserUpdate{PasswordHash: &passwordHash}); err != nil {
		authInternalError(c)
		return
	}

	if _, err := lib.RevokeAllUserRefreshTokens(ctx, user.ID); err != nil {
		authInternalError(c)
		return
	}
	lib.InvalidateUserAccessTokens(ctx, user.ID)
	UsersCloseUserSessions(user.ID, "Password reset")

	authSecurityEvent(ctx, lib.SecurityEventSuspiciousActivity, &user.ID, lib.SecurityEventDetails{
		IP:       clientIP,
		Username: user.Username,
		Action:   "password_reset",
		Reason:   "Password reset via forgot password",
	})

	c.JSON(http.StatusOK, gin.H{
		"message":           "Password reset successfully. Please log in with your new password.",
		"twoFactorDisabled": twoFactorDisabled,
	})
}

// ==================== GET /login-history ====================

// authLoginRecordResponse mirrors the route's record mapping.
type authLoginRecordResponse struct {
	ID        int32   `json:"id"`
	IP        string  `json:"ip"`
	Country   *string `json:"country"`
	Region    *string `json:"region"`
	City      *string `json:"city"`
	Isp       *string `json:"isp"`
	Timezone  *string `json:"timezone"`
	UserAgent *string `json:"userAgent"`
	CreatedAt string  `json:"createdAt"`
}

func authLoginHistory(c *gin.Context) {
	claims := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	page := lib.ParsePositiveIntegerQuery(authSingleQueryValue(c, "page"), 1, math.MaxInt32)
	pageSize := lib.ParsePositiveIntegerQuery(authSingleQueryValue(c, "pageSize"), 20, 50)

	result, err := db.GetUserLoginRecords(ctx, claims.ID, page, pageSize)
	if err != nil {
		authInternalError(c)
		return
	}

	shouldRedact := authShouldRedactDemoLoginClaims(c, claims)
	records := make([]authLoginRecordResponse, 0, len(result.Records))
	for _, record := range result.Records {
		item := authLoginRecordResponse{
			ID:        record.ID,
			IP:        record.IP,
			Country:   record.Country,
			Region:    record.Region,
			City:      record.City,
			Isp:       record.Isp,
			Timezone:  record.Timezone,
			UserAgent: record.UserAgent,
			CreatedAt: usersISOTime(record.CreatedAt),
		}
		if shouldRedact {
			item.IP = lib.DemoRedactedIP
			item.Country = nil
			item.Region = nil
			item.City = nil
			item.Isp = nil
			item.Timezone = nil
			item.UserAgent = nil
		}
		records = append(records, item)
	}

	c.JSON(http.StatusOK, gin.H{
		"records":    records,
		"total":      result.Total,
		"page":       result.Page,
		"pageSize":   result.PageSize,
		"totalPages": result.TotalPages,
	})
}

// authShouldRedactDemoLoginClaims mirrors shouldRedactDemoLoginRecords(request,
// request.user): the JWT payload carries the username but no email, exactly like
// the Node request.user object.
func authShouldRedactDemoLoginClaims(c *gin.Context, claims *lib.AccessTokenClaims) bool {
	return lib.ShouldProtectDemoAccount(c.GetHeader("X-Forwarded-Host"), c.Request.Host, claims.Username, "")
}

// ==================== login records (db/login-records.ts slice) ====================

// authLoginRecordParams mirrors CreateLoginRecordParams.
type authLoginRecordParams struct {
	UserID    int32
	IP        string
	Country   *string
	Region    *string
	City      *string
	Isp       *string
	Timezone  *string
	UserAgent *string
}

// authCreateLoginRecord mirrors createLoginRecord. Timestamps are written as
// explicit UTC values (never SQL NOW()): Prisma stores UTC wall-clock time in
// timestamp(3) columns, and pgx encodes a time.Time by its own wall clock.
func authCreateLoginRecord(ctx context.Context, params authLoginRecordParams) error {
	return db.DB.WithContext(ctx).Exec(
		`INSERT INTO login_records (user_id, ip, country, region, city, isp, timezone, user_agent, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		params.UserID, params.IP, params.Country, params.Region, params.City,
		params.Isp, params.Timezone, params.UserAgent, time.Now().UTC(),
	).Error
}

// authRecordLoginAsync mirrors the fire-and-forget createLoginRecord calls: the
// response is never delayed or failed by the record write.
func authRecordLoginAsync(userID int32, ip, userAgent string, protectDemoAccount bool) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if protectDemoAccount {
			_ = authCreateLoginRecord(ctx, authLoginRecordParams{
				UserID: userID,
				IP:     lib.DemoRedactedIP,
			})
			return
		}

		agent := userAgent
		geo := authGetGeoIPInfo(ctx, ip)
		err := authCreateLoginRecord(ctx, authLoginRecordParams{
			UserID:    userID,
			IP:        ip,
			Country:   geo.Country,
			Region:    geo.Region,
			City:      geo.City,
			Isp:       geo.Isp,
			Timezone:  geo.Timezone,
			UserAgent: &agent,
		})
		if err != nil {
			// GeoIP lookup failure still records the login without geo fields.
			_ = authCreateLoginRecord(ctx, authLoginRecordParams{
				UserID:    userID,
				IP:        ip,
				UserAgent: &agent,
			})
		}
	}()
}

// authGeoIPInfo mirrors GeoIpInfo; nil fields serialise as JSON null.
type authGeoIPInfo struct {
	IP       string
	Country  *string
	Region   *string
	City     *string
	Isp      *string
	Timezone *string
}

// authGeoIPAPITimeout mirrors the 5s AbortSignal.timeout of the TypeScript fetch.
const authGeoIPAPITimeout = 5 * time.Second

// authGetGeoIPInfo is the minimal slice of lib/geoip.ts needed by the auth routes.
// The canonical module adds the request queue + 24h cache; this helper performs a
// single bounded lookup and returns null fields on any failure, which is the
// contract the login flow depends on. Loopback and private addresses never leave
// the process, exactly like isLocalIp.
func authGetGeoIPInfo(ctx context.Context, ip string) authGeoIPInfo {
	normalized := strings.TrimSpace(ip)
	if normalized == "" || net.ParseIP(normalized) == nil {
		return authGeoIPInfo{IP: ip}
	}
	if authIsLocalIP(normalized) {
		return authGeoIPInfo{IP: normalized}
	}

	lookupCtx, cancel := context.WithTimeout(ctx, authGeoIPAPITimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(lookupCtx, http.MethodGet,
		"http://ip-api.com/json/"+normalized+"?fields=status,message,country,regionName,city,isp,timezone,query", nil)
	if err != nil {
		return authGeoIPInfo{IP: normalized}
	}
	request.Header.Set("Accept", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return authGeoIPInfo{IP: normalized}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return authGeoIPInfo{IP: normalized}
	}

	var payload struct {
		Status     string `json:"status"`
		Country    string `json:"country"`
		RegionName string `json:"regionName"`
		City       string `json:"city"`
		Isp        string `json:"isp"`
		Timezone   string `json:"timezone"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || payload.Status == "fail" {
		return authGeoIPInfo{IP: normalized}
	}

	nullable := func(value string) *string {
		if value == "" {
			return nil
		}
		return &value
	}
	return authGeoIPInfo{
		IP:       normalized,
		Country:  nullable(payload.Country),
		Region:   nullable(payload.RegionName),
		City:     nullable(payload.City),
		Isp:      nullable(payload.Isp),
		Timezone: nullable(payload.Timezone),
	}
}

// authIsLocalIP mirrors isLocalIp.
func authIsLocalIP(ip string) bool {
	if ip == "127.0.0.1" || ip == "localhost" ||
		strings.HasPrefix(ip, "192.168.") ||
		strings.HasPrefix(ip, "10.") ||
		(strings.HasPrefix(ip, "172.") && authIsPrivate172(ip)) {
		return true
	}
	lowered := strings.ToLower(ip)
	return ip == "::1" || strings.HasPrefix(lowered, "fe80:") ||
		strings.HasPrefix(lowered, "fc") || strings.HasPrefix(lowered, "fd")
}

// authIsPrivate172 mirrors isPrivate172 (172.16.0.0/12).
func authIsPrivate172(ip string) bool {
	parts := strings.Split(ip, ".")
	if len(parts) < 2 {
		return false
	}
	second, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return second >= 16 && second <= 31
}

func init() {
	Register(RegisterAuth)
}
