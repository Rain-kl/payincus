// Route self-registration. app.ts lists every module explicitly; in Go we invert
// that so parallel module ports never need to edit the shared app.go.
package routes

import "github.com/gin-gonic/gin"

var registrars []func(*gin.Engine)

// Register lets a route module register its mounter during package init.
func Register(fn func(*gin.Engine)) {
	registrars = append(registrars, fn)
}

// RegisterAll mounts every self-registered route module.
func RegisterAll(r *gin.Engine) {
	for _, fn := range registrars {
		fn(r)
	}
}
