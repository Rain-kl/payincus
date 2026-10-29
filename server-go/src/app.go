// Package app wires the Gin router with the middleware chain that mirrors
// server/src/app.ts (cors, helmet, rate limit, demo safety, auth decorators).
package app

import (
	"net/http"
	"regexp"
	"strings"
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

	// Mirror the Fastify not-found handler: JSON for API paths, Cloudflare RUM
	// paths return 204, everything else is a static asset miss.
	router.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/cdn-cgi/") {
			c.Status(http.StatusNoContent)
			return
		}
		if strings.HasPrefix(path, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "API 路由不存在"})
			return
		}
		if hasFileExtension(path) {
			c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
			return
		}
		// Non-API SPA routes fall through to index.html when static serving is
		// enabled in production; that handler is ported with the static module.
		c.JSON(http.StatusNotFound, gin.H{"error": "文件不存在"})
	})

	return router
}

var fileExtensionPattern = regexp.MustCompile(`\.[a-zA-Z0-9]+$`)

func hasFileExtension(path string) bool {
	return fileExtensionPattern.MatchString(path)
}
