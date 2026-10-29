// Recovery mirrors the generic branch of fastify.setErrorHandler: production hides
// internal details, development surfaces them.
package middleware

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// Recovery converts panics into the standard error envelope.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				message := "Internal server error"
				if !lib.IsProductionEnv() {
					message = fmt.Sprint(recovered)
				}
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": message})
			}
		}()
		c.Next()
	}
}
