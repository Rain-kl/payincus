// Package app wires the Gin router with the middleware chain that mirrors
// server/src/app.ts (cors, helmet, rate limit, demo safety, auth decorators).
package app

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/routes"
)

// NewRouter builds the HTTP router. Registration order matches app.ts so that
// hook ordering (rate limit before demo block, redaction last) is preserved.
func NewRouter() *gin.Engine {
	if lib.IsProductionEnv() {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	// Fastify does not redirect trailing slashes; keep Gin's behaviour identical.
	router.RedirectTrailingSlash = false

	// Gin trusts every proxy by default, which would make c.ClientIP() honour a
	// forged X-Forwarded-For from any caller. The caller IP feeds the rate-limit
	// key and the Turnstile siteverify payload, so disable Gin's resolution and
	// rely on middleware.ClientIP, which implements the same TRUST_PROXY rules as
	// fastify's request.ip.
	if err := router.SetTrustedProxies(nil); err != nil {
		panic(err)
	}

	// fastify request.ip is trust-proxy aware, so every consumer of the caller IP
	// must resolve it the same way. Turnstile needs it for Cloudflare's siteverify.
	lib.SetTurnstileClientIPResolver(middleware.ClientIP)

	router.Use(
		middleware.Recovery(),
		middleware.CORS(lib.GetCorsOrigins()),
		middleware.SecurityHeaders(),
		middleware.RateLimit(),
		middleware.DemoReadOnly(),
		middleware.DemoRedact(),
	)

	router.GET("/api/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":    "ok",
			"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	})

	routes.RegisterAll(router)

	// Fastify's setNotFoundHandler in app.ts is registered only when the backend
	// serves the static client (NODE_ENV=production && SERVE_STATIC_CLIENT!=false).
	// Production deploys set SERVE_STATIC_CLIENT=false, so the handler never
	// registers and the wire contract is Fastify's built-in 404 — verified against
	// fastify 5:
	//   {"message":"Route GET:/api/nope not found","error":"Not Found","statusCode":404}
	// The static-client branch is ported with the static module.
	router.NoRoute(func(c *gin.Context) {
		// Fastify parses the body BEFORE deciding there is no route, so an
		// unmatched POST carrying Content-Type: application/json with an empty or
		// malformed body answers 400, not 404 (measured against Node):
		//   POST /api/nope  json empty  -> 400 "Body cannot be empty…"
		//   POST /api/nope  application/xml -> 404 (no parser runs for it)
		// Gin middleware never runs when no route matches, so the check has to be
		// repeated here. Unauthenticated body errors also cannot be produced by an
		// auth middleware here, since no route means no auth hooks at all.
		if status, message := middleware.ValidateFastifyBody(c); status != 0 {
			middleware.AbortFastifyBody(c, status, message)
			return
		}

		// Fastify renders `Route <method>:<url> not found` where `<url>` is the raw
		// request URL **including the query string**, e.g.
		//   Route GET:/login?error=invalid_state not found
		// (measured against fastify 5; the OAuth callback/authorize redirects land on
		// frontend paths, so a query-carrying 404 is a real, reachable shape).
		// `URL.Path` alone drops the query and diverges from Node.
		c.JSON(http.StatusNotFound, gin.H{
			"message":    "Route " + c.Request.Method + ":" + c.Request.URL.RequestURI() + " not found",
			"error":      "Not Found",
			"statusCode": http.StatusNotFound,
		})
	})

	return router
}
