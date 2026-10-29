// Port of server/src/routes/admin-oauth-apps.ts (mounted at
// /api/admin/oauth-apps).
//
// Every route is admin-only (fastify.authenticateAdmin → 401/403).
//
// Behavioural notes that are easy to get wrong:
//
//   - Route ids use the strict `parsePositiveId` regex (`^[1-9]\d*$`), so "0",
//     "01", "-1" and "abc" answer 400 with a bespoke code, not the shared
//     lib.ErrInvalidId envelope.
//   - The query-string pagination is *loose*: `parsePositiveId(page) || 1`, so an
//     invalid value silently becomes 1 / 20 rather than erroring.
//   - `status` outside active/revoked/disabled falls back to "all".
//   - clientSecret is returned in plaintext by POST / and POST /:id/rotate-secret
//     only. GET /, PUT /:id and the authorization endpoints never include it (nor
//     the stored hash), and it is never written to the audit log.
//   - PUT /:id and POST /:id/rotate-secret collapse *every* failure (bad body,
//     missing row) into a single 400/404 envelope, so the Prisma P2025 message text
//     is echoed in `error` for a missing row — reproduced verbatim below.
package routes

import (
	"errors"
	"math"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// oauthApp messages and codes, copied verbatim from the TypeScript.
const (
	adminOAuthAppsInvalidAppNameMessage      = "Invalid OAuth app name"
	adminOAuthAppsInvalidRedirectMessage     = "Invalid OAuth redirect URIs"
	adminOAuthAppsInvalidScopesMessage       = "Invalid OAuth scopes"
	adminOAuthAppsInvalidIDMessage           = "Invalid OAuth app id"
	adminOAuthAppsInvalidIDCode              = "INVALID_OAUTH_APP_ID"
	adminOAuthAppsInvalidAuthorizationIDMsg  = "Invalid OAuth authorization id"
	adminOAuthAppsInvalidAuthorizationIDCode = "INVALID_OAUTH_AUTHORIZATION_ID"
	adminOAuthAppsAuthorizationNotFoundMsg   = "OAuth authorization not found"
	adminOAuthAppsAuthorizationNotFoundCode  = "OAUTH_AUTHORIZATION_NOT_FOUND"
	adminOAuthAppsInvalidAppCode             = "INVALID_OAUTH_APP"
	adminOAuthAppsUpdateFailedCode           = "OAUTH_APP_UPDATE_FAILED"
	adminOAuthAppsNotFoundMessage            = "OAuth app not found"
	adminOAuthAppsNotFoundCode               = "OAUTH_APP_NOT_FOUND"
	adminOAuthAppsDeletedMessage             = "OAuth app deleted"

	// adminOAuthAppsPrismaUpdatePreamble is how @prisma/client 7.1.0 prefixes the
	// message of a failed `oAuthClientApp.update()` when NODE_ENV=production. The
	// route echoes `error.message` straight back, so this exact text is the wire
	// contract (measured against the real client).
	adminOAuthAppsPrismaUpdatePreamble = "\nInvalid `prisma.oAuthClientApp.update()` invocation:\n\n\n"

	// adminOAuthAppsUpdateNotFoundTail is Prisma's P2025 tail for a missing row.
	adminOAuthAppsUpdateNotFoundTail = "An operation failed because it depends on one or more records that were required but not found. No record was found for an update."
)

// adminOAuthAppsUpdateNotFoundMessage is the full production message for a missing
// app row (preamble + P2025 tail).
const adminOAuthAppsUpdateNotFoundMessage = adminOAuthAppsPrismaUpdatePreamble + adminOAuthAppsUpdateNotFoundTail

// adminOAuthAppsPayload mirrors normalizeAppPayload's return value.
type adminOAuthAppsPayload struct {
	Name         string
	RedirectURIs []string
	Scopes       []string
	Enabled      bool
}

// adminOAuthAppsNormalizePayload mirrors normalizeAppPayload. The second return
// value is the exact `new Error(message)` text ("" when valid).
func adminOAuthAppsNormalizePayload(body map[string]any) (adminOAuthAppsPayload, string) {
	name, nameOK := db.OAuthAppsNormalizeName(body["name"])
	redirectURIs, redirectOK := db.OAuthAppsNormalizeRedirectURIs(body["redirectUris"])
	scopes, scopesOK := db.OAuthAppsNormalizeScopes(body["scopes"])

	if !nameOK {
		return adminOAuthAppsPayload{}, adminOAuthAppsInvalidAppNameMessage
	}
	if !redirectOK {
		return adminOAuthAppsPayload{}, adminOAuthAppsInvalidRedirectMessage
	}
	if !scopesOK {
		return adminOAuthAppsPayload{}, adminOAuthAppsInvalidScopesMessage
	}

	// `enabled: body.enabled !== false` — the tri-state trap: only an explicit
	// JSON false disables the app; absent or null means enabled.
	enabled := true
	if flag, ok := body["enabled"].(bool); ok {
		enabled = flag
	}

	return adminOAuthAppsPayload{
		Name:         name,
		RedirectURIs: redirectURIs,
		Scopes:       scopes,
		Enabled:      enabled,
	}, ""
}

// adminOAuthAppsParsePositiveID mirrors parsePositiveId: the strict
// `^[1-9]\d*$` regex plus Number.isSafeInteger, so ids beyond int32 are *accepted*
// here and only rejected by the database (exactly like Prisma rejecting an Int
// outside 32 bits). Callers must therefore range-check before narrowing.
func adminOAuthAppsParsePositiveID(value string) (int64, bool) {
	return lib.ParsePositiveRouteID(value)
}

// adminOAuthAppsFitsInt32 reports whether an id can address a Prisma Int column.
func adminOAuthAppsFitsInt32(value int64) bool {
	return value > 0 && value <= math.MaxInt32
}

// adminOAuthAppsOutOfRangeMessage is the full production message for an id above
// 2^31-1 (Prisma P2020, surfaced by PUT /:id through the generic catch). The other
// handlers turn it into a 404/500 as shown at each call site.
const adminOAuthAppsOutOfRangeMessage = adminOAuthAppsPrismaUpdatePreamble +
	"Value out of range for the type: value \"2147483648\" is out of range for type integer"

// adminOAuthAppsReadBody decodes the JSON body, treating a missing body as `{}`
// (`request.body || {}`). Fastify's body-parser errors are surfaced first.
func adminOAuthAppsReadBody(c *gin.Context) (map[string]any, bool) {
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return nil, false
	}
	switch typed := decoded.Value.(type) {
	case map[string]any:
		return typed, true
	case nil:
		return map[string]any{}, true
	default:
		// `request.body || {}` keeps a truthy non-object (e.g. an array) as-is, and
		// normalizeAppPayload then finds no name → INVALID_OAUTH_APP. Returning an
		// empty object produces the same envelope.
		return map[string]any{}, true
	}
}

// adminOAuthAppsClampInt narrows a page/pageSize parsed by the strict id regex.
// Both are capped by the db layer (pageSize to 100), so an overflow can only
// produce a huge page number, which is harmless; MaxInt32 keeps it representable.
func adminOAuthAppsClampInt(value int64) int {
	if value > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(value)
}

// adminOAuthAppsCurrentUser mirrors getRequestUser(request).
func adminOAuthAppsCurrentUser(c *gin.Context) *lib.AccessTokenClaims {
	return middleware.CurrentUser(c)
}

// RegisterAdminOAuthApps mounts /api/admin/oauth-apps.
func RegisterAdminOAuthApps(r *gin.Engine) {
	group := r.Group("/api/admin/oauth-apps", middleware.AuthenticateAdmin())

	// GET /authorizations — paginated grants with token counters.
	group.GET("/authorizations", func(c *gin.Context) {
		query := c.Request.URL.Query()

		var appID *int32
		if raw := query.Get("appId"); raw != "" {
			if parsed, ok := adminOAuthAppsParsePositiveID(raw); ok {
				if !adminOAuthAppsFitsInt32(parsed) {
					// Prisma raises P2020 for an Int column out of range, which the
					// Node route does not catch → 500.
					adminOAuthAppsInternalError(c)
					return
				}
				value := int32(parsed)
				appID = &value
			}
			// An invalid appId becomes null (no filter), not a 400.
		}

		status := "all"
		switch query.Get("status") {
		case "active", "revoked", "disabled":
			status = query.Get("status")
		}

		var user *string
		if raw := query.Get("user"); raw != "" {
			user = &raw
		}

		page := 1
		if raw := query.Get("page"); raw != "" {
			if parsed, ok := adminOAuthAppsParsePositiveID(raw); ok {
				page = adminOAuthAppsClampInt(parsed)
			}
		}
		pageSize := 20
		if raw := query.Get("pageSize"); raw != "" {
			if parsed, ok := adminOAuthAppsParsePositiveID(raw); ok {
				pageSize = adminOAuthAppsClampInt(parsed)
			}
		}

		result, err := db.OAuthAppsListAuthorizations(c.Request.Context(), db.OAuthAppsAuthorizationListOptions{
			AppID:    appID,
			User:     user,
			Status:   status,
			Page:     page,
			PageSize: pageSize,
		})
		if err != nil {
			adminOAuthAppsInternalError(c)
			return
		}
		c.JSON(http.StatusOK, result)
	})

	// DELETE /authorizations/:id — revoke a grant (and its live tokens).
	group.DELETE("/authorizations/:id", func(c *gin.Context) {
		// Fastify parses the body of every bodywith method (DELETE included) before
		// the handler runs, so a request that declares a JSON content type with an
		// empty body is rejected here, not by the id check.
		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		parsed, ok := adminOAuthAppsParsePositiveID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": adminOAuthAppsInvalidAuthorizationIDMsg,
				"code":  adminOAuthAppsInvalidAuthorizationIDCode,
			})
			return
		}
		if !adminOAuthAppsFitsInt32(parsed) {
			// Prisma P2020 is uncaught on this route → 500.
			adminOAuthAppsInternalError(c)
			return
		}
		grantID := int32(parsed)

		ctx := c.Request.Context()
		authorization, err := db.OAuthAppsRevokeAuthorization(ctx, grantID)
		if err != nil {
			adminOAuthAppsInternalError(c)
			return
		}
		if authorization == nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error": adminOAuthAppsAuthorizationNotFoundMsg,
				"code":  adminOAuthAppsAuthorizationNotFoundCode,
			})
			return
		}

		user := adminOAuthAppsCurrentUser(c)
		actorID := int64(user.ID)
		db.CreateLog(ctx, &actorID, db.LogModuleSecurity, "oauth_provider.admin_authorization_revoke",
			"Admin revoked OAuth authorization #"+formatTerminalCommandNumber(float64(authorization.ID))+
				" for app \""+authorization.App.Name+"\"",
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"authorization": authorization})
	})

	// GET / — every registered app.
	listHandler := func(c *gin.Context) {
		apps, err := db.OAuthAppsList(c.Request.Context())
		if err != nil {
			adminOAuthAppsInternalError(c)
			return
		}
		c.JSON(http.StatusOK, gin.H{"apps": apps})
	}
	group.GET("", listHandler)
	group.GET("/", listHandler)

	// POST / — create an app; the plaintext secret is returned exactly once.
	createHandler := func(c *gin.Context) {
		body, ok := adminOAuthAppsReadBody(c)
		if !ok {
			return
		}
		user := adminOAuthAppsCurrentUser(c)

		payload, normalizeError := adminOAuthAppsNormalizePayload(body)
		if normalizeError != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": normalizeError, "code": adminOAuthAppsInvalidAppCode})
			return
		}

		ctx := c.Request.Context()
		result, err := db.OAuthAppsCreate(ctx, db.OAuthAppsCreateInput{
			Name:            payload.Name,
			RedirectURIs:    payload.RedirectURIs,
			Scopes:          payload.Scopes,
			Enabled:         payload.Enabled,
			CreatedByUserID: user.ID,
		})
		if err != nil {
			// The TypeScript catches everything here and echoes error.message, so a
			// database failure also lands in the 400 INVALID_OAUTH_APP envelope.
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": adminOAuthAppsInvalidAppCode})
			return
		}

		actorID := int64(user.ID)
		db.CreateLog(ctx, &actorID, db.LogModuleSecurity, "oauth_provider.app_create",
			"Created OAuth app \""+payload.Name+"\"", db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusCreated, gin.H{
			"app":          result.App,
			"clientSecret": result.ClientSecret,
		})
	}
	group.POST("", createHandler)
	group.POST("/", createHandler)

	// PUT /:id — full replacement of name/redirectUris/scopes/enabled.
	group.PUT("/:id", func(c *gin.Context) {
		parsed, ok := adminOAuthAppsParsePositiveID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": adminOAuthAppsInvalidIDMessage, "code": adminOAuthAppsInvalidIDCode})
			return
		}

		body, ok := adminOAuthAppsReadBody(c)
		if !ok {
			return
		}
		payload, normalizeError := adminOAuthAppsNormalizePayload(body)
		if normalizeError != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": normalizeError, "code": adminOAuthAppsUpdateFailedCode})
			return
		}

		if !adminOAuthAppsFitsInt32(parsed) {
			// The Node handler's catch puts Prisma's P2020 message straight into the
			// 400 envelope.
			c.JSON(http.StatusBadRequest, gin.H{"error": adminOAuthAppsOutOfRangeMessage, "code": adminOAuthAppsUpdateFailedCode})
			return
		}
		appID := int32(parsed)

		ctx := c.Request.Context()
		app, err := db.OAuthAppsUpdate(ctx, appID, db.OAuthAppsUpdateInput{
			Name:         payload.Name,
			RedirectURIs: payload.RedirectURIs,
			Scopes:       payload.Scopes,
			Enabled:      payload.Enabled,
		})
		if err != nil {
			// Prisma's P2025 text is what the TypeScript echoes for a missing row.
			message := err.Error()
			if errors.Is(err, gorm.ErrRecordNotFound) {
				message = adminOAuthAppsUpdateNotFoundMessage
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": message, "code": adminOAuthAppsUpdateFailedCode})
			return
		}

		user := adminOAuthAppsCurrentUser(c)
		actorID := int64(user.ID)
		db.CreateLog(ctx, &actorID, db.LogModuleSecurity, "oauth_provider.app_update",
			"Updated OAuth app \""+payload.Name+"\"", db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"app": app})
	})

	// POST /:id/rotate-secret — new secret, returned once.
	group.POST("/:id/rotate-secret", func(c *gin.Context) {
		// The handler ignores the body, but Fastify still parses it first.
		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		parsed, ok := adminOAuthAppsParsePositiveID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": adminOAuthAppsInvalidIDMessage, "code": adminOAuthAppsInvalidIDCode})
			return
		}
		if !adminOAuthAppsFitsInt32(parsed) {
			// `catch { 404 }` — P2020 is reported as "not found".
			c.JSON(http.StatusNotFound, gin.H{"error": adminOAuthAppsNotFoundMessage, "code": adminOAuthAppsNotFoundCode})
			return
		}
		appID := int32(parsed)

		ctx := c.Request.Context()
		result, err := db.OAuthAppsRotateSecret(ctx, appID)
		if err != nil {
			// `catch { 404 }` — any failure is reported as "not found".
			c.JSON(http.StatusNotFound, gin.H{"error": adminOAuthAppsNotFoundMessage, "code": adminOAuthAppsNotFoundCode})
			return
		}

		user := adminOAuthAppsCurrentUser(c)
		actorID := int64(user.ID)
		db.CreateLog(ctx, &actorID, db.LogModuleSecurity, "oauth_provider.app_secret_rotate",
			"Rotated OAuth app secret for \""+result.App.Name+"\"", db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{
			"app":          result.App,
			"clientSecret": result.ClientSecret,
		})
	})

	// DELETE /:id — remove an app (cascades to grants/tokens/codes).
	group.DELETE("/:id", func(c *gin.Context) {
		// Same Fastify body-with-DELETE rule as above.
		if _, status, message := decodeJSONRequestBody(c); status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}

		parsed, ok := adminOAuthAppsParsePositiveID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": adminOAuthAppsInvalidIDMessage, "code": adminOAuthAppsInvalidIDCode})
			return
		}
		if !adminOAuthAppsFitsInt32(parsed) {
			// `catch { 404 }` — P2020 is reported as "not found".
			c.JSON(http.StatusNotFound, gin.H{"error": adminOAuthAppsNotFoundMessage, "code": adminOAuthAppsNotFoundCode})
			return
		}
		appID := int32(parsed)

		ctx := c.Request.Context()
		if err := db.OAuthAppsDelete(ctx, appID); err != nil {
			// `catch { 404 }` — a missing row and a delete failure look alike.
			c.JSON(http.StatusNotFound, gin.H{"error": adminOAuthAppsNotFoundMessage, "code": adminOAuthAppsNotFoundCode})
			return
		}

		user := adminOAuthAppsCurrentUser(c)
		actorID := int64(user.ID)
		db.CreateLog(ctx, &actorID, db.LogModuleSecurity, "oauth_provider.app_delete",
			"Deleted OAuth app #"+formatTerminalCommandNumber(float64(appID)),
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{"message": adminOAuthAppsDeletedMessage})
	})
}

// adminOAuthAppsInternalError mirrors the generic 500 branch of app.ts' handler.
func adminOAuthAppsInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

func init() {
	Register(RegisterAdminOAuthApps)
}
