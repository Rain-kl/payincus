package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHostInstanceBackupsAdminBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterHostInstanceBackups(engine)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		request := httptest.NewRequest(method, "/api/admin/hosts/3/instance-backups", strings.NewReader(`{}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Code != 401 {
			t.Fatalf("%s: %d", method, response.Code)
		}
	}
}

func TestHostInstanceBackupsValidation(t *testing.T) {
	for _, body := range []string{`{}`, `{"id":"bad","directory":"/mnt/backup"}`, `{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","directory":"relative"}`, `{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","directory":"/mnt/backup","command":"sh"}`, `{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","directory":"/mnt/backup"} {}`} {
		response := nodeSecurityHandlerRequest(t, http.MethodPost, "/api/admin/hosts/3/instance-backups", "application/json", body, hostInstanceBackupsHandler)
		if response.Code != 400 {
			t.Fatalf("%s: %d %s", body, response.Code, response.Body.String())
		}
	}
}
