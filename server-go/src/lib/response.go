// HTTP response helpers shared by routes.
package lib

import "github.com/gin-gonic/gin"

// RespondAPIError writes the canonical error envelope for an ErrorCode.
func RespondAPIError(c *gin.Context, status int, code ErrorCode, details string) {
	c.JSON(status, APIErrorFor(code, details))
}
