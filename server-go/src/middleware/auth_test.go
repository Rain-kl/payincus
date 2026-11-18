package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// TestStackedAuthDefersBodyValidationUntilRequireAdmin verifies that when
// Authenticate() is stacked with RequireAdmin(), an authenticated non-admin
// receives 403 ADMIN_REQUIRED even when sending an empty or malformed JSON
// body (matching Fastify's onRequest hooks running before body parsing).
func TestStackedAuthDefersBodyValidationUntilRequireAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	simulateAuthenticate := func() gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set(ContextUserKey, &lib.AccessTokenClaims{
				ID:       2,
				Username: "regular",
				Role:     "user",
				Status:   "active",
				IssuedAt: 1700000000,
			})
			if hasRequireAdminHandler(c) {
				c.Next()
				return
			}
			continueAfterAuth(c)
		}
	}

	router.POST("/admin-only", simulateAuthenticate(), RequireAdmin(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodPost, "/admin-only", strings.NewReader("{bad json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ADMIN_REQUIRED") {
		t.Fatalf("body = %s, want ADMIN_REQUIRED", rec.Body.String())
	}
}

// TestHasRequireAdminHandlerDetection checks the handler-chain inspection used
// to defer Fastify body parsing when RequireAdmin follows Authenticate.
func TestHasRequireAdminHandlerDetection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	var stackedResult, singleResult bool
	router.POST("/stacked", func(c *gin.Context) {
		stackedResult = hasRequireAdminHandler(c)
		c.AbortWithStatus(http.StatusOK)
	}, RequireAdmin())
	router.POST("/single", func(c *gin.Context) {
		singleResult = hasRequireAdminHandler(c)
		c.AbortWithStatus(http.StatusOK)
	})

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/stacked", nil))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/single", nil))

	if !stackedResult {
		t.Fatal("expected hasRequireAdminHandler=true when RequireAdmin is in chain")
	}
	if singleResult {
		t.Fatal("expected hasRequireAdminHandler=false when RequireAdmin is absent")
	}
}
