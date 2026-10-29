package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	app "github.com/rain-kl/payincus/server-go/src"
)

func TestHealthEndpoint(t *testing.T) {
	router := app.NewRouter()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", recorder.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("health status field = %v, want ok", body["status"])
	}

	// Helmet-equivalent headers must be present on every response.
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := recorder.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
}

func TestAnnouncementsRequireAuth(t *testing.T) {
	router := app.NewRouter()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/announcements", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", recorder.Code)
	}
}

func TestDemoReadOnlyBlocksMutation(t *testing.T) {
	router := app.NewRouter()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/users", nil)
	request.Host = "demo.payincus.com"
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("demo mutation status = %d, want 403", recorder.Code)
	}

	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if body["code"] != "DEMO_READ_ONLY" {
		t.Fatalf("demo mutation code = %v, want DEMO_READ_ONLY", body["code"])
	}
}

func TestUnknownAPIRouteIs404(t *testing.T) {
	router := app.NewRouter()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/does-not-exist", nil)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want 404", recorder.Code)
	}
}
