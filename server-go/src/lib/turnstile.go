// Port of server/src/lib/turnstile.ts.
//
// Cloudflare Turnstile verification. The token is checked against
// https://challenges.cloudflare.com/turnstile/v0/siteverify using the secret key
// stored in system_config ("turnstile_secret_key", an encrypted/sensitive key).
//
// Deviation from the TypeScript original (per migration brief): the siteverify
// request is issued through SafeFetch (lib/outbound_security.go) instead of a
// plain fetch, so the outbound call goes through the same SSRF validation and
// connect-time revalidating transport as every other backend egress. For the
// public Cloudflare hostname this is behaviour-preserving.
package lib

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// Turnstile siteverify endpoint and the two response codes the verifier emits.
// These codes are literal strings in the TypeScript source (it does not route
// them through lib/errors.ts apiError), so they are kept verbatim to preserve the
// exact JSON envelope.
const (
	turnstileSiteVerifyURL             = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	turnstileSiteVerifyLabel           = "Turnstile siteverify URL"
	TurnstileErrorTokenMissing         = "TURNSTILE_TOKEN_MISSING"
	TurnstileErrorVerificationFailed   = "TURNSTILE_VERIFICATION_FAILED"
	turnstileTokenHeader               = "x-turnstile-token"
	turnstileTokenField                = "turnstileToken"
	turnstileNotConfiguredWarningText  = "Turnstile enabled but site key or secret key is not configured"
	turnstileVerificationFailedMessage = "Turnstile verification failed"
	turnstileVerificationRequiredMsg   = "Turnstile verification required"
)

// TurnstileConfig mirrors the getTurnstileConfig() projection.
type TurnstileConfig struct {
	Enabled   bool    `json:"enabled"`
	SiteKey   *string `json:"siteKey"`
	SecretKey *string `json:"secretKey"`
}

// GetTurnstileConfig mirrors getTurnstileConfig.
func GetTurnstileConfig(ctx context.Context) (TurnstileConfig, error) {
	enabled, err := db.GetSystemConfigBoolean(ctx, "turnstile_enabled", false)
	if err != nil {
		return TurnstileConfig{}, err
	}
	siteKey, err := db.GetSystemConfig(ctx, "turnstile_site_key", true)
	if err != nil {
		return TurnstileConfig{}, err
	}
	secretKey, err := db.GetSystemConfig(ctx, "turnstile_secret_key", true)
	if err != nil {
		return TurnstileConfig{}, err
	}
	return TurnstileConfig{Enabled: enabled, SiteKey: siteKey, SecretKey: secretKey}, nil
}

// TurnstileVerifyResult mirrors the verifyTurnstileToken() return shape.
type TurnstileVerifyResult struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// turnstileVerifyResponse mirrors the siteverify JSON body (Cloudflare uses
// hyphenated keys).
type turnstileVerifyResponse struct {
	Success     bool     `json:"success"`
	ErrorCodes  []string `json:"error-codes"`
	ChallengeTS string   `json:"challenge_ts"`
	Hostname    string   `json:"hostname"`
}

// VerifyTurnstileToken mirrors verifyTurnstileToken. It never returns a Go error:
// every failure is reported through the result envelope, exactly like the
// TypeScript implementation.
func VerifyTurnstileToken(ctx context.Context, token, secretKey, remoteIP string) TurnstileVerifyResult {
	form := url.Values{}
	form.Set("secret", secretKey)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}

	headers := http.Header{}
	headers.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := SafeFetch(ctx, turnstileSiteVerifyURL, &SafeFetchInit{
		Method:  http.MethodPost,
		Headers: headers,
		Body:    strings.NewReader(form.Encode()),
	}, turnstileSiteVerifyLabel)
	if err != nil {
		return TurnstileVerifyResult{Success: false, Error: turnstileErrorMessage(err)}
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return TurnstileVerifyResult{Success: false, Error: turnstileErrorMessage(err)}
	}

	var result turnstileVerifyResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return TurnstileVerifyResult{Success: false, Error: turnstileErrorMessage(err)}
	}

	if result.Success {
		return TurnstileVerifyResult{Success: true}
	}
	if len(result.ErrorCodes) > 0 {
		return TurnstileVerifyResult{Success: false, Error: strings.Join(result.ErrorCodes, ", ")}
	}
	return TurnstileVerifyResult{Success: false, Error: "Verification failed"}
}

// turnstileErrorMessage mirrors the catch branch: "Turnstile verification error: <msg>".
func turnstileErrorMessage(err error) string {
	return fmt.Sprintf("Turnstile verification error: %s", err.Error())
}

// turnstileClientIPResolver resolves the caller IP the way fastify's
// trust-proxy-aware request.ip does. middleware.ClientIP is the production
// implementation; the default falls back to Gin's ClientIP so the verifier works
// standalone. lib cannot import middleware (middleware imports lib).
var turnstileClientIPResolver = func(c *gin.Context) string { return c.ClientIP() }

// SetTurnstileClientIPResolver injects the trust-proxy-aware client-IP resolver.
// A nil resolver is ignored.
func SetTurnstileClientIPResolver(resolver func(*gin.Context) string) {
	if resolver != nil {
		turnstileClientIPResolver = resolver
	}
}

// turnstileTokenFromRequest mirrors the token lookup order: body, then query,
// then the x-turnstile-token header.
func turnstileTokenFromRequest(c *gin.Context) string {
	if token := turnstileTokenFromBody(c); token != "" {
		return token
	}
	if token := c.Query(turnstileTokenField); token != "" {
		return token
	}
	return c.GetHeader(turnstileTokenHeader)
}

// turnstileTokenFromBody reads turnstileToken from a JSON body. ShouldBindBodyWith
// caches the raw body so the downstream handler can bind it again, which is how
// Gin stands in for fastify's already-parsed request.body.
func turnstileTokenFromBody(c *gin.Context) string {
	if c.Request == nil || c.Request.Body == nil {
		return ""
	}
	if !strings.Contains(c.ContentType(), "application/json") {
		return ""
	}

	var payload struct {
		TurnstileToken string `json:"turnstileToken"`
	}
	if err := c.ShouldBindBodyWith(&payload, binding.JSON); err != nil {
		return ""
	}
	return payload.TurnstileToken
}

// CreateTurnstileVerifier mirrors createTurnstileVerifier. required is retained for
// signature parity; whether verification is enforced is decided by the system
// switch plus complete key configuration, exactly as in TypeScript.
func CreateTurnstileVerifier(required bool) gin.HandlerFunc {
	_ = required

	return func(c *gin.Context) {
		config, err := GetTurnstileConfig(c.Request.Context())
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		// Not enabled: always skip. Production may keep stale keys around while the
		// switch is off, so the backend must not demand a challenge the frontend no
		// longer renders.
		if !config.Enabled {
			return
		}

		// Enabled but incompletely configured: neither key can be required.
		if config.SiteKey == nil || *config.SiteKey == "" || config.SecretKey == nil || *config.SecretKey == "" {
			log.Printf("%s", turnstileNotConfiguredWarningText)
			return
		}

		token := turnstileTokenFromRequest(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": turnstileVerificationRequiredMsg,
				"code":  TurnstileErrorTokenMissing,
			})
			return
		}

		result := VerifyTurnstileToken(c.Request.Context(), token, *config.SecretKey, turnstileClientIPResolver(c))
		if !result.Success {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":   turnstileVerificationFailedMessage,
				"code":    TurnstileErrorVerificationFailed,
				"details": result.Error,
			})
			return
		}
	}
}

// TurnstileVerifier mirrors the exported turnstileVerifier instance.
var TurnstileVerifier = CreateTurnstileVerifier(true)
