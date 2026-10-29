package routes

import (
	"encoding/json"
	"testing"

	"github.com/gin-gonic/gin"
)

// mustMarshalJSON encodes value and decodes it back into a generic map.
func mustMarshalJSON(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return decoded
}

// newTestRouter builds a bare Gin engine. Trailing-slash redirects are disabled so
// route paths are reported verbatim, matching app.go.
func newTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.RedirectTrailingSlash = false
	return router
}

// TestRegisterFriendsAndGiftCards mounts both route modules and asserts the exact
// path set the Node app registers (server/src/app.ts prefixes), so a renamed or
// dropped route fails here instead of in production.
func TestRegisterFriendsAndGiftCards(t *testing.T) {
	router := newTestRouter()
	RegisterFriends(router)
	RegisterGiftCards(router)

	wanted := []struct{ method, path string }{
		// friends
		{"GET", "/api/friends"},
		{"GET", "/api/friends/"},
		{"POST", "/api/friends/request"},
		{"GET", "/api/friends/requests"},
		{"GET", "/api/friends/requests/sent"},
		{"DELETE", "/api/friends/request/:id"},
		{"POST", "/api/friends/:id/accept"},
		{"POST", "/api/friends/:id/reject"},
		{"DELETE", "/api/friends/:id"},
		{"GET", "/api/friends/history"},
		{"GET", "/api/friends/search"},
		{"GET", "/api/friends/:id/resources"},
		// gift cards — admin
		{"POST", "/api/gift-cards/admin/generate"},
		{"POST", "/api/gift-cards/admin/batch"},
		{"GET", "/api/gift-cards/admin/list"},
		{"GET", "/api/gift-cards/admin/stats"},
		{"PATCH", "/api/gift-cards/admin/:id/enable"},
		{"PATCH", "/api/gift-cards/admin/:id/disable"},
		{"DELETE", "/api/gift-cards/admin/:id"},
		{"POST", "/api/gift-cards/admin/batch-disable"},
		{"POST", "/api/gift-cards/admin/batch-delete"},
		// gift cards — user
		{"POST", "/api/gift-cards/user/redeem"},
		{"POST", "/api/gift-cards/user/generate"},
		{"POST", "/api/gift-cards/user/:id/revoke"},
		{"GET", "/api/gift-cards/user/mine"},
	}

	registered := map[string]bool{}
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	for _, want := range wanted {
		key := want.method + " " + want.path
		if !registered[key] {
			t.Errorf("missing route %s", key)
		}
	}
}
