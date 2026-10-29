// Unit tests for the Turnstile port. They live in package lib so they can exercise
// the unexported token-extraction helpers directly. The siteverify call itself is
// network-bound and is not exercised here.
package lib

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTurnstileTestContext(t *testing.T, method, target, contentType, body string) *gin.Context {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	c.Request = request
	return c
}

func TestTurnstileTokenFromHeader(t *testing.T) {
	c := newTurnstileTestContext(t, "POST", "/api/login", "application/json", "")
	c.Request.Header.Set("x-turnstile-token", "header-token")
	if got := turnstileTokenFromRequest(c); got != "header-token" {
		t.Fatalf("token = %q, want header-token", got)
	}
}

func TestTurnstileTokenFromQuery(t *testing.T) {
	c := newTurnstileTestContext(t, "GET", "/api/verify?turnstileToken=query-token", "", "")
	if got := turnstileTokenFromRequest(c); got != "query-token" {
		t.Fatalf("token = %q, want query-token", got)
	}
}

func TestTurnstileTokenFromJSONBody(t *testing.T) {
	c := newTurnstileTestContext(t, "POST", "/api/login", "application/json; charset=utf-8", `{"turnstileToken":"body-token"}`)
	if got := turnstileTokenFromRequest(c); got != "body-token" {
		t.Fatalf("token = %q, want body-token", got)
	}
}

func TestTurnstileTokenPrecedence(t *testing.T) {
	c := newTurnstileTestContext(t, "POST", "/api/login?turnstileToken=query-token", "application/json", `{"turnstileToken":"body-token"}`)
	c.Request.Header.Set("x-turnstile-token", "header-token")
	if got := turnstileTokenFromRequest(c); got != "body-token" {
		t.Fatalf("token = %q, want body-token (body wins)", got)
	}
}

func TestTurnstileTokenMissing(t *testing.T) {
	c := newTurnstileTestContext(t, "POST", "/api/login", "application/json", `{"other":"value"}`)
	if got := turnstileTokenFromRequest(c); got != "" {
		t.Fatalf("token = %q, want empty", got)
	}

	// A non-JSON body must not be parsed.
	c = newTurnstileTestContext(t, "POST", "/api/login", "text/plain", "turnstileToken=plain")
	if got := turnstileTokenFromRequest(c); got != "" {
		t.Fatalf("token from a non-JSON body = %q, want empty", got)
	}

	// Malformed JSON is ignored rather than fatal.
	c = newTurnstileTestContext(t, "POST", "/api/login", "application/json", `{"turnstileToken":`)
	if got := turnstileTokenFromRequest(c); got != "" {
		t.Fatalf("token from malformed JSON = %q, want empty", got)
	}
}

func TestTurnstileErrorMessageFormat(t *testing.T) {
	got := turnstileErrorMessage(errors.New("boom"))
	if got != "Turnstile verification error: boom" {
		t.Fatalf("turnstileErrorMessage = %q", got)
	}
}

func TestTurnstileResponseCodes(t *testing.T) {
	if TurnstileErrorTokenMissing != "TURNSTILE_TOKEN_MISSING" {
		t.Errorf("TurnstileErrorTokenMissing = %q", TurnstileErrorTokenMissing)
	}
	if TurnstileErrorVerificationFailed != "TURNSTILE_VERIFICATION_FAILED" {
		t.Errorf("TurnstileErrorVerificationFailed = %q", TurnstileErrorVerificationFailed)
	}
	if turnstileSiteVerifyURL != "https://challenges.cloudflare.com/turnstile/v0/siteverify" {
		t.Errorf("siteverify URL = %q", turnstileSiteVerifyURL)
	}
}

func TestSetTurnstileClientIPResolverIgnoresNil(t *testing.T) {
	original := turnstileClientIPResolver
	t.Cleanup(func() { turnstileClientIPResolver = original })

	SetTurnstileClientIPResolver(nil)
	if turnstileClientIPResolver == nil {
		t.Fatal("resolver must not be set to nil")
	}

	SetTurnstileClientIPResolver(func(*gin.Context) string { return "203.0.113.9" })
	c := newTurnstileTestContext(t, "GET", "/", "", "")
	if got := turnstileClientIPResolver(c); got != "203.0.113.9" {
		t.Fatalf("resolver = %q, want 203.0.113.9", got)
	}
}

func TestTurnstileVerifierIsConstructed(t *testing.T) {
	if TurnstileVerifier == nil {
		t.Fatal("TurnstileVerifier must be constructed at package init")
	}
	if CreateTurnstileVerifier(false) == nil {
		t.Fatal("CreateTurnstileVerifier must return a handler")
	}
}
