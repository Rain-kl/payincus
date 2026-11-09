// Port of server/src/routes/oauth-provider.ts.
//
// app.ts mounts this plugin as
// `fastify.register(oauthProviderRoutes, { prefix: '/api/oauth-provider' })`, so
// every path below lives under that prefix. The TypeScript declares NO JSON schema
// on any route: request bodies are read as `readOAuthProviderRequestBody(body)`
// (non-objects become {}), so Fastify's AJV never runs here — only the body-parser
// rules (400/413/415) apply, and those run in the auth middleware for the
// authenticated routes and at the top of the two public POST handlers.
//
// Authorize input accepts both the OAuth-standard snake_case names and the SPA's
// camelCase ones (`response_type` / `responseType`, ...), joined with `||`, so an
// empty string falls through to the next alias. That exact fallthrough is reproduced
// by oauthProviderJSOr.
//
// Token responses use the snake_case OAuth wire shape; every other response uses the
// camelCase objects db/oauth_provider.go serializes.
package routes

import (
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// oauthProviderMaxScopes mirrors the `raw.length > 24` guard in normalizeOAuthScopes.
const oauthProviderMaxScopes = 24

// oauthProviderAuthorizeInput mirrors the object readAuthorizeInput returns.
type oauthProviderAuthorizeInput struct {
	ResponseType *string
	ClientID     *string
	RedirectURI  *string
	Scopes       []string
	State        *string
}

// oauthProviderJSTruthy mirrors JavaScript truthiness for the JSON/query values the
// routes read. Objects and arrays are always truthy.
func oauthProviderJSTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case float64:
		return typed != 0
	case string:
		return typed != ""
	default:
		return true
	}
}

// oauthProviderJSOr mirrors `a || b || fallback`.
func oauthProviderJSOr(values ...any) any {
	for _, value := range values {
		if oauthProviderJSTruthy(value) {
			return value
		}
	}
	return nil
}

// oauthProviderUTF16Length mirrors String.prototype.length.
func oauthProviderUTF16Length(value string) int {
	units := 0
	for _, r := range value {
		if r > 0xFFFF {
			units += 2
			continue
		}
		units++
	}
	return units
}

// oauthProviderStringValue mirrors stringValue.
func oauthProviderStringValue(value any, maxLength int) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || oauthProviderUTF16Length(trimmed) > maxLength {
		return nil
	}
	return &trimmed
}

// oauthProviderNormalizeRedirectURI mirrors normalizeRedirectUri (the single-value
// form of normalizeOAuthRedirectUris).
func oauthProviderNormalizeRedirectURI(value any) *string {
	normalized, ok := db.OAuthAppsNormalizeRedirectURIs([]any{value})
	if !ok || len(normalized) == 0 {
		return nil
	}
	return &normalized[0]
}

// oauthProviderIsJSSpace mirrors the /\s/ character class.
func oauthProviderIsJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// oauthProviderSplitScopes mirrors `value.split(/\s+/)`, which keeps the empty
// segments a leading/trailing/repeated whitespace run produces.
func oauthProviderSplitScopes(value string) []string {
	segments := []string{}
	var current strings.Builder
	inWhitespaceRun := false
	for _, r := range value {
		if oauthProviderIsJSSpace(r) {
			if !inWhitespaceRun {
				segments = append(segments, current.String())
				current.Reset()
				inWhitespaceRun = true
			}
			continue
		}
		inWhitespaceRun = false
		current.WriteRune(r)
	}
	segments = append(segments, current.String())
	return segments
}

// oauthProviderNormalizeScopes mirrors normalizeOAuthScopes: a whitespace-separated
// string or an array, 1..24 raw entries, deduplicated, every entry in the Public API
// scope catalog. nil means "invalid".
func oauthProviderNormalizeScopes(value any) []string {
	var raw []any
	switch typed := value.(type) {
	case string:
		segments := oauthProviderSplitScopes(typed)
		raw = make([]any, 0, len(segments))
		for _, segment := range segments {
			raw = append(raw, segment)
		}
	case []any:
		raw = typed
	default:
		return nil
	}
	if len(raw) == 0 || len(raw) > oauthProviderMaxScopes {
		return nil
	}

	allowed := make(map[string]bool, len(db.OAuthAppsPublicAPIScopes))
	for _, scope := range db.OAuthAppsPublicAPIScopes {
		allowed[scope] = true
	}

	scopes := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		text, ok := item.(string)
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			continue
		}
		if !allowed[trimmed] {
			return nil
		}
		if seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		scopes = append(scopes, trimmed)
	}
	if len(scopes) == 0 {
		return nil
	}
	return scopes
}

// oauthProviderQueryBody builds a fast-querystring-like object: a repeated key is an
// array, a single key is its string value.
func oauthProviderQueryBody(c *gin.Context) map[string]any {
	body := map[string]any{}
	for key, values := range c.Request.URL.Query() {
		if len(values) == 1 {
			body[key] = values[0]
			continue
		}
		items := make([]any, 0, len(values))
		for _, value := range values {
			items = append(items, value)
		}
		body[key] = items
	}
	return body
}

// oauthProviderReadAuthorizeInput mirrors readAuthorizeInput.
func oauthProviderReadAuthorizeInput(body map[string]any) oauthProviderAuthorizeInput {
	responseType := oauthProviderStringValue(oauthProviderJSOr(body["responseType"], body["response_type"], "code"), 20)
	clientID := oauthProviderStringValue(oauthProviderJSOr(body["clientId"], body["client_id"]), 120)
	redirectURI := oauthProviderNormalizeRedirectURI(oauthProviderJSOr(body["redirectUri"], body["redirect_uri"]))

	scopesValue := body["scope"]
	if _, present := body["scopes"]; present {
		scopesValue = oauthProviderJSOr(body["scope"], body["scopes"])
	}
	scopes := oauthProviderNormalizeScopes(oauthProviderJSOr(scopesValue, "profile:read"))
	state := oauthProviderStringValue(body["state"], 500)

	return oauthProviderAuthorizeInput{
		ResponseType: responseType,
		ClientID:     clientID,
		RedirectURI:  redirectURI,
		Scopes:       scopes,
		State:        state,
	}
}

// oauthProviderValidateAuthorizeInput mirrors the four early 400s shared by
// /authorize/consent, /authorize and /authorize/confirm.
func oauthProviderValidateAuthorizeInput(c *gin.Context, input oauthProviderAuthorizeInput) bool {
	if input.ResponseType == nil || *input.ResponseType != "code" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported response_type", "code": "UNSUPPORTED_RESPONSE_TYPE"})
		return false
	}
	if input.ClientID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid client_id", "code": "INVALID_CLIENT_ID"})
		return false
	}
	if input.RedirectURI == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid redirect_uri", "code": "INVALID_REDIRECT_URI"})
		return false
	}
	if len(input.Scopes) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid scope", "code": "INVALID_SCOPE"})
		return false
	}
	return true
}

// oauthProviderEncodeURIComponent mirrors encodeURIComponent.
func oauthProviderEncodeURIComponent(value string) string {
	const uppercase = "0123456789ABCDEF"
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		current := value[index]
		if (current >= 'a' && current <= 'z') || (current >= 'A' && current <= 'Z') || (current >= '0' && current <= '9') {
			builder.WriteByte(current)
			continue
		}
		switch current {
		case '-', '_', '.', '!', '~', '*', '\'', '(', ')':
			builder.WriteByte(current)
		default:
			builder.WriteByte('%')
			builder.WriteByte(uppercase[current>>4])
			builder.WriteByte(uppercase[current&0x0F])
		}
	}
	return builder.String()
}

// oauthProviderBuildConsentURL mirrors the consentUrl template.
func oauthProviderBuildConsentURL(clientID, redirectURI string, scopes []string, state *string) string {
	builder := strings.Builder{}
	builder.WriteString("/oauth/authorize?client_id=")
	builder.WriteString(oauthProviderEncodeURIComponent(clientID))
	builder.WriteString("&redirect_uri=")
	builder.WriteString(oauthProviderEncodeURIComponent(redirectURI))
	builder.WriteString("&scope=")
	builder.WriteString(oauthProviderEncodeURIComponent(strings.Join(scopes, " ")))
	if state != nil && *state != "" {
		builder.WriteString("&state=")
		builder.WriteString(oauthProviderEncodeURIComponent(*state))
	}
	return builder.String()
}

// oauthProviderBuildErrorRedirect mirrors buildAuthorizationErrorRedirect.
func oauthProviderBuildErrorRedirect(redirectURI string, state *string, errorCode, description string) (string, error) {
	params := [][2]string{{"error", errorCode}, {"error_description", description}}
	if state != nil && *state != "" {
		params = append(params, [2]string{"state", *state})
	}
	return db.OAuthProviderSetQueryParams(redirectURI, params)
}

// oauthProviderLog mirrors createLog for the security module.
func oauthProviderLog(c *gin.Context, userID int32, action, content string) {
	subject := int64(userID)
	db.CreateLog(c.Request.Context(), &subject, db.LogModuleSecurity, action, content, db.LogResultSuccess, db.CreateLogOptions{})
}

// oauthProviderScopeMetadata converts validated scope names to the lib metadata
// lookup. Unknown names (impossible after normalization) yield zero metadata, exactly
// like the TypeScript map lookup.
func oauthProviderScopeMetadata(scopes []string) []lib.PublicAPIScopeMetadata {
	values := make([]lib.PublicAPIScope, 0, len(scopes))
	for _, scope := range scopes {
		values = append(values, lib.PublicAPIScope(scope))
	}
	return lib.ListPublicAPIScopeMetadata(values...)
}

// RegisterOAuthProvider mounts the OAuth provider endpoints (prefix
// /api/oauth-provider).
func RegisterOAuthProvider(r *gin.Engine) {
	group := r.Group("/api/oauth-provider")

	group.GET("/scopes", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"scopes":    lib.ListPublicAPIScopeMetadata(),
			"updatedAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	})

	group.GET("/authorize/consent", middleware.Authenticate(), func(c *gin.Context) {
		ctx := c.Request.Context()
		input := oauthProviderReadAuthorizeInput(oauthProviderQueryBody(c))
		if !oauthProviderValidateAuthorizeInput(c, input) {
			return
		}

		user := middleware.CurrentUser(c)
		prepared, err := db.OAuthProviderPrepareAuthorizationRequest(ctx, db.OAuthProviderAuthorizationRequestInput{
			ClientID:    *input.ClientID,
			RedirectURI: *input.RedirectURI,
			Scopes:      input.Scopes,
			UserID:      user.ID,
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "OAUTH_CONSENT_FAILED"})
			return
		}

		existingScopes := []string{}
		if prepared.ExistingGrant != nil && prepared.ExistingGrant.RevokedAt == nil {
			if normalized := oauthProviderNormalizeScopes(db.OAuthProviderSerializeScopes(prepared.ExistingGrant.Scopes)); normalized != nil {
				existingScopes = normalized
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"app": gin.H{
				"id":       prepared.App.ID,
				"name":     prepared.App.Name,
				"clientId": prepared.App.ClientID,
			},
			"requestedScopes": prepared.RequestedScopes,
			"scopeMetadata":   oauthProviderScopeMetadata(prepared.RequestedScopes),
			"existingScopes":  existingScopes,
			"consentRequired": prepared.ConsentRequired,
			"request": gin.H{
				"responseType": *input.ResponseType,
				"clientId":     *input.ClientID,
				"redirectUri":  *input.RedirectURI,
				"scope":        strings.Join(input.Scopes, " "),
				"state":        input.State,
			},
		})
	})

	group.POST("/authorize", middleware.Authenticate(), func(c *gin.Context) {
		ctx := c.Request.Context()
		decoded, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			middleware.AbortFastifyBody(c, status, message)
			return
		}
		body := db.OAuthProviderReadRequestBody(decoded.Value)
		input := oauthProviderReadAuthorizeInput(body)
		if !oauthProviderValidateAuthorizeInput(c, input) {
			return
		}

		user := middleware.CurrentUser(c)
		prepared, err := db.OAuthProviderPrepareAuthorizationRequest(ctx, db.OAuthProviderAuthorizationRequestInput{
			ClientID:    *input.ClientID,
			RedirectURI: *input.RedirectURI,
			Scopes:      input.Scopes,
			UserID:      user.ID,
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "OAUTH_AUTHORIZE_FAILED"})
			return
		}
		if prepared.ConsentRequired && body["confirmed"] != true {
			c.JSON(http.StatusPreconditionRequired, gin.H{
				"error":      "OAuth consent required",
				"code":       "OAUTH_CONSENT_REQUIRED",
				"consentUrl": oauthProviderBuildConsentURL(*input.ClientID, *input.RedirectURI, input.Scopes, input.State),
			})
			return
		}

		authorization, err := db.OAuthProviderCreateAuthorizationCode(ctx, db.OAuthProviderCreateCodeInput{
			ClientID:    *input.ClientID,
			RedirectURI: *input.RedirectURI,
			Scopes:      input.Scopes,
			UserID:      user.ID,
			State:       input.State,
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "OAUTH_AUTHORIZE_FAILED"})
			return
		}
		oauthProviderLog(c, user.ID, "oauth_provider.authorize",
			"Issued OAuth authorization code for "+*input.ClientID+" with scopes: "+strings.Join(input.Scopes, " "))
		c.JSON(http.StatusOK, authorization)
	})

	group.POST("/authorize/confirm", middleware.Authenticate(), func(c *gin.Context) {
		ctx := c.Request.Context()
		decoded, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			middleware.AbortFastifyBody(c, status, message)
			return
		}
		body := db.OAuthProviderReadRequestBody(decoded.Value)
		input := oauthProviderReadAuthorizeInput(body)
		if !oauthProviderValidateAuthorizeInput(c, input) {
			return
		}

		if body["confirmed"] != true {
			redirectTo, err := oauthProviderBuildErrorRedirect(*input.RedirectURI, input.State, "access_denied", "The user denied the authorization request")
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "OAUTH_AUTHORIZE_FAILED"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"redirectTo": redirectTo})
			return
		}

		user := middleware.CurrentUser(c)
		authorization, err := db.OAuthProviderCreateAuthorizationCode(ctx, db.OAuthProviderCreateCodeInput{
			ClientID:    *input.ClientID,
			RedirectURI: *input.RedirectURI,
			Scopes:      input.Scopes,
			UserID:      user.ID,
			State:       input.State,
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "OAUTH_AUTHORIZE_FAILED"})
			return
		}
		oauthProviderLog(c, user.ID, "oauth_provider.consent_approve",
			"Approved OAuth app "+*input.ClientID+" with scopes: "+strings.Join(input.Scopes, " "))
		c.JSON(http.StatusOK, authorization)
	})

	group.POST("/token", func(c *gin.Context) {
		ctx := c.Request.Context()
		decoded, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			middleware.AbortFastifyBody(c, status, message)
			return
		}
		body := db.OAuthProviderReadRequestBody(decoded.Value)

		grantType := oauthProviderStringValue(oauthProviderJSOr(body["grantType"], body["grant_type"]), 80)
		clientID := oauthProviderStringValue(oauthProviderJSOr(body["clientId"], body["client_id"]), 120)
		clientSecret := oauthProviderStringValue(oauthProviderJSOr(body["clientSecret"], body["client_secret"]), 200)
		code := oauthProviderStringValue(body["code"], 200)
		redirectURI := oauthProviderNormalizeRedirectURI(oauthProviderJSOr(body["redirectUri"], body["redirect_uri"]))
		refreshToken := oauthProviderStringValue(oauthProviderJSOr(body["refreshToken"], body["refresh_token"]), 200)

		if grantType == nil || (*grantType != "authorization_code" && *grantType != "refresh_token") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Unsupported grant_type", "code": "UNSUPPORTED_GRANT_TYPE"})
			return
		}
		if clientID == nil || clientSecret == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid OAuth token request", "code": "INVALID_OAUTH_TOKEN_REQUEST"})
			return
		}

		var result *db.OAuthProviderTokenResult
		var err error
		if *grantType == "authorization_code" {
			if code == nil || redirectURI == nil {
				result = nil
			} else {
				exchanged, exchangeErr := db.OAuthProviderExchangeAuthorizationCode(ctx, db.OAuthProviderCodeExchangeInput{
					ClientID:     *clientID,
					ClientSecret: *clientSecret,
					Code:         *code,
					RedirectURI:  *redirectURI,
				})
				if exchangeErr != nil {
					err = exchangeErr
				} else {
					result = &exchanged
				}
			}
		} else if refreshToken == nil {
			result = nil
		} else {
			exchanged, exchangeErr := db.OAuthProviderExchangeRefreshToken(ctx, db.OAuthProviderRefreshExchangeInput{
				ClientID:     *clientID,
				ClientSecret: *clientSecret,
				RefreshToken: *refreshToken,
			})
			if exchangeErr != nil {
				err = exchangeErr
			} else {
				result = &exchanged
			}
		}

		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "OAUTH_TOKEN_EXCHANGE_FAILED"})
			return
		}
		if result == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid OAuth token request", "code": "INVALID_OAUTH_TOKEN_REQUEST"})
			return
		}

		action := "oauth_provider.token_refresh"
		logContent := "Refreshed OAuth access token for " + *clientID
		if *grantType == "authorization_code" {
			action = "oauth_provider.token_exchange"
			logContent = "Exchanged OAuth authorization code for " + *clientID
		}
		oauthProviderLog(c, result.AccessTokenRecord.UserID, action, logContent)

		c.JSON(http.StatusOK, gin.H{
			"access_token":             result.AccessToken,
			"refresh_token":            result.RefreshToken,
			"token_type":               result.TokenType,
			"expires_in":               result.ExpiresIn,
			"refresh_token_expires_in": result.RefreshExpiresIn,
			"scope":                    result.Scope,
		})
	})

	group.POST("/revoke", func(c *gin.Context) {
		ctx := c.Request.Context()
		decoded, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			middleware.AbortFastifyBody(c, status, message)
			return
		}
		body := db.OAuthProviderReadRequestBody(decoded.Value)
		token := oauthProviderStringValue(body["token"], 200)
		if token == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid token", "code": "INVALID_TOKEN"})
			return
		}

		userID, err := db.OAuthProviderRevokeToken(ctx, *token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if userID != nil {
			oauthProviderLog(c, *userID, "oauth_provider.token_revoke", "Revoked OAuth access token")
		}
		c.JSON(http.StatusOK, gin.H{"message": "Token revoked"})
	})

	group.GET("/authorizations", middleware.Authenticate(), func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		grants, err := db.OAuthProviderListAuthorizations(c.Request.Context(), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"authorizations": grants})
	})

	group.DELETE("/authorizations/:id", middleware.Authenticate(), func(c *gin.Context) {
		ctx := c.Request.Context()
		user := middleware.CurrentUser(c)

		parsed, ok := lib.ParsePositiveRouteID(c.Param("id"))
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid authorization id", "code": "INVALID_AUTHORIZATION_ID"})
			return
		}
		if parsed > math.MaxInt32 {
			// Prisma P2020 for an out-of-range Int; uncaught in the Node route → 500.
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		grant, err := db.OAuthProviderRevokeAuthorization(ctx, user.ID, int32(parsed))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if grant == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "OAuth authorization not found", "code": "OAUTH_AUTHORIZATION_NOT_FOUND"})
			return
		}
		oauthProviderLog(c, user.ID, "oauth_provider.authorization_revoke",
			"Revoked OAuth authorization for "+grant.App.Name)
		c.JSON(http.StatusOK, gin.H{"authorization": grant})
	})
}

func init() {
	Register(RegisterOAuthProvider)
}
