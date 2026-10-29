// Auth middleware mirroring the fastify decorators in server/src/app.ts:
// authenticate / requireAdmin / authenticateAdmin / authenticateUser.
package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ContextUserKey stores the verified *lib.AccessTokenClaims on the request context.
const ContextUserKey = "user"

// CurrentUser returns the verified claims for the request, or nil.
func CurrentUser(c *gin.Context) *lib.AccessTokenClaims {
	if value, ok := c.Get(ContextUserKey); ok {
		if claims, ok := value.(*lib.AccessTokenClaims); ok {
			return claims
		}
	}
	return nil
}

func bearerToken(c *gin.Context) string {
	header := c.GetHeader("Authorization")
	if len(header) > 7 && strings.EqualFold(header[:7], "Bearer ") {
		return header[7:]
	}
	return ""
}

func unauthorized(c *gin.Context) {
	c.AbortWithStatusJSON(401, gin.H{"error": "Unauthorized"})
}

// ensureActiveAccessToken mirrors ensureActiveAccessToken(): validates invalidation
// and refreshes the role/status from the database.
func ensureActiveAccessToken(c *gin.Context, claims *lib.AccessTokenClaims) bool {
	if claims.ID == 0 || claims.IssuedAt == 0 {
		c.AbortWithStatusJSON(401, gin.H{"error": "Unauthorized", "code": "UNAUTHORIZED"})
		return false
	}

	if lib.IsAccessTokenInvalidated(c.Request.Context(), claims.ID, claims.IssuedAt, claims.SID) {
		c.AbortWithStatusJSON(401, gin.H{"error": "Session expired", "code": "SESSION_INVALIDATED"})
		return false
	}

	var user models.User
	err := db.DB.WithContext(c.Request.Context()).
		Select("username", "role", "status").
		First(&user, claims.ID).Error
	if err != nil {
		c.AbortWithStatusJSON(401, gin.H{"error": "Unauthorized", "code": "UNAUTHORIZED"})
		return false
	}
	if string(user.Status) != "active" {
		c.AbortWithStatusJSON(401, gin.H{"error": "Account banned", "code": "ACCOUNT_BANNED"})
		return false
	}

	claims.Username = user.Username
	claims.Role = string(user.Role)
	claims.Status = string(user.Status)
	return true
}

func ensureCurrentAdmin(c *gin.Context, claims *lib.AccessTokenClaims) bool {
	if claims.ID == 0 || claims.Role != "admin" || claims.Status != "active" {
		c.AbortWithStatusJSON(403, gin.H{"error": "Admin privileges required", "code": "ADMIN_REQUIRED"})
		return false
	}
	return true
}

func verifyAndActivate(c *gin.Context) *lib.AccessTokenClaims {
	token := bearerToken(c)
	if token == "" {
		unauthorized(c)
		return nil
	}
	claims, err := lib.VerifyAccessToken(token)
	if err != nil {
		unauthorized(c)
		return nil
	}
	if !ensureActiveAccessToken(c, claims) {
		return nil
	}
	c.Set(ContextUserKey, claims)
	return claims
}

// Authenticate mirrors app.authenticate.
func Authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if verifyAndActivate(c) == nil {
			return
		}
		c.Next()
	}
}

// RequireAdmin mirrors app.requireAdmin (must run after Authenticate).
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims := CurrentUser(c)
		if claims == nil {
			c.AbortWithStatusJSON(401, gin.H{"error": "Unauthorized", "code": "UNAUTHORIZED"})
			return
		}
		if !ensureCurrentAdmin(c, claims) {
			return
		}
		c.Next()
	}
}

// AuthenticateAdmin mirrors app.authenticateAdmin.
func AuthenticateAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims := verifyAndActivate(c)
		if claims == nil {
			return
		}
		if !ensureCurrentAdmin(c, claims) {
			return
		}
		c.Next()
	}
}

// AuthenticateUser mirrors app.authenticateUser (regular users only).
func AuthenticateUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims := verifyAndActivate(c)
		if claims == nil {
			return
		}
		if claims.Role == "admin" {
			c.AbortWithStatusJSON(403, gin.H{"error": "This feature is for regular users only", "code": "USER_ONLY"})
			return
		}
		c.Next()
	}
}
