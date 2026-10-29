// Security headers matching the @fastify/helmet configuration in server/src/app.ts.
package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

const productionCSP = "default-src 'self';" +
	" script-src 'self' 'unsafe-inline' https://challenges.cloudflare.com https://static.cloudflareinsights.com;" +
	" style-src 'self' 'unsafe-inline' https://fonts.googleapis.com;" +
	" font-src 'self' https://fonts.gstatic.com data:;" +
	" img-src 'self' data: blob: http: https: https://kkksr.com https://api.dicebear.com https://dicebear.incudal.com https://*.githubusercontent.com https://avatars.githubusercontent.com https://lh3.googleusercontent.com;" +
	" connect-src 'self' ws: wss: https://challenges.cloudflare.com https://cloudflareinsights.com https://api.dicebear.com https://dicebear.incudal.com;" +
	" frame-src 'self' https://challenges.cloudflare.com;" +
	" object-src 'none';" +
	" base-uri 'self';" +
	" form-action 'self';" +
	" frame-ancestors 'none'"

// SecurityHeaders applies the helmet-equivalent response headers.
func SecurityHeaders() gin.HandlerFunc {
	csp := ""
	if lib.IsProductionEnv() {
		csp = productionCSP
	}

	return func(c *gin.Context) {
		header := c.Writer.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("X-XSS-Protection", "0")
		header.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		header.Set("Cross-Origin-Resource-Policy", "cross-origin")
		if csp != "" {
			header.Set("Content-Security-Policy", csp)
		}
		// Hide the framework fingerprint.
		header.Del("X-Powered-By")
		c.Next()
	}
}

// originAllowed reports whether origin is present in the allowed list.
func originAllowed(origin string, allowed []string) bool {
	for _, candidate := range allowed {
		if strings.EqualFold(candidate, origin) {
			return true
		}
	}
	return false
}
