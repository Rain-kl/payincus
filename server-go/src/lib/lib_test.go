package lib_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/config"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

func TestErrorCatalogParity(t *testing.T) {
	if len(lib.AllErrorCodes) != 254 {
		t.Fatalf("expected 254 error codes, got %d", len(lib.AllErrorCodes))
	}
	if len(lib.ErrorMessages) != 254 {
		t.Fatalf("expected 254 error messages, got %d", len(lib.ErrorMessages))
	}
	// Spot-check messages against the TypeScript source.
	cases := map[lib.ErrorCode]string{
		lib.ErrInvalidId:     "Invalid ID",
		lib.ErrForbidden:     "Access denied",
		lib.ErrInvalidParams: "Invalid parameters",
		lib.ErrUnauthorized:  "Unauthorized",
		lib.ErrInternalError: "Internal server error",
	}
	for code, want := range cases {
		if got := lib.ErrorMessages[code]; got != want {
			t.Errorf("ErrorMessages[%s] = %q, want %q", code, got, want)
		}
	}
}

func TestAPIErrorForHidesInternalDetails(t *testing.T) {
	response := lib.APIErrorFor(lib.ErrInternalError, "leaked secret")
	if response.Details != "" {
		t.Fatalf("internal error leaked details: %q", response.Details)
	}
}

func TestAPIErrorForTruncatesDetails(t *testing.T) {
	response := lib.APIErrorFor(lib.ErrInvalidId, strings.Repeat("x", 300))
	if len([]rune(response.Details)) != 256 {
		t.Fatalf("details length = %d, want 256", len([]rune(response.Details)))
	}
}

func TestSanitizeTokensInString(t *testing.T) {
	// The Node regex only treats a key as a secret when it follows '?' or '&'.
	if got := lib.SanitizeTokensInString("?token=abc123&x=1"); got != "?token=[REDACTED]&x=1" {
		t.Errorf("query secret not redacted: %q", got)
	}
	// The Node regex replaces the entire "Bearer <token>" match.
	if got := lib.SanitizeTokensInString("Bearer abc.def.ghi"); got != "[REDACTED_JWT]" {
		t.Errorf("bearer token not redacted: %q", got)
	}
}

func TestIsSensitiveField(t *testing.T) {
	if !lib.IsSensitiveField("password") {
		t.Fatal("password should be sensitive")
	}
	if lib.IsSensitiveField("username") {
		t.Fatal("username should not be sensitive")
	}
}

func TestDemoMutationBlocking(t *testing.T) {
	if !lib.ShouldBlockDemoMutation("demo.payincus.com", "", "POST", "/api/users") {
		t.Fatal("demo POST /api/users must be blocked")
	}
	if lib.ShouldBlockDemoMutation("demo.payincus.com", "", "POST", "/api/auth/login") {
		t.Fatal("demo login must be allowed")
	}
	if lib.ShouldBlockDemoMutation("demo.payincus.com", "", "GET", "/api/users") {
		t.Fatal("demo GET must be allowed")
	}
	if lib.ShouldBlockDemoMutation("pay.payincus.com", "", "POST", "/api/users") {
		t.Fatal("non-demo POST must be allowed")
	}
}

func TestRedactDemoJSONPayload(t *testing.T) {
	payload := map[string]any{
		"ip":       "203.0.113.9",
		"email":    "user@example.com",
		"password": "hunter2",
	}
	redacted, ok := lib.RedactDemoJSONPayload(payload).(map[string]any)
	if !ok {
		t.Fatal("redacted payload is not a map")
	}
	if redacted["ip"] != lib.DemoRedactedIP {
		t.Errorf("ip = %v, want redacted", redacted["ip"])
	}
	if redacted["password"] != lib.DemoRedactedIP {
		t.Errorf("password = %v, want redacted", redacted["password"])
	}
}

func TestNetworkModeHelpers(t *testing.T) {
	if !lib.NetworkModeNeedsNatIpv4("nat") || !lib.NetworkModeNeedsNatIpv4("legacy-unknown") {
		t.Fatal("nat/unknown should need NAT IPv4")
	}
	if !lib.NetworkModeNeedsPublicIpv4("public_ipv4_ipv6") {
		t.Fatal("public_ipv4_ipv6 should need public IPv4")
	}
	if lib.NetworkModeNeedsRoutedIpv6("nat") {
		t.Fatal("nat should not need routed IPv6")
	}
	if lib.NormalizeNetworkMode("garbage") != lib.NetworkModeNat {
		t.Fatal("unknown mode should normalize to nat")
	}
}

func TestParsePositiveRouteID(t *testing.T) {
	if _, ok := lib.ParsePositiveRouteID("0"); ok {
		t.Fatal("0 must be rejected")
	}
	if _, ok := lib.ParsePositiveRouteID("-1"); ok {
		t.Fatal("negative must be rejected")
	}
	if _, ok := lib.ParsePositiveRouteID("01"); ok {
		t.Fatal("leading zero must be rejected")
	}
	if value, ok := lib.ParsePositiveRouteID("42"); !ok || value != 42 {
		t.Fatalf("42 rejected: %d %v", value, ok)
	}
}

func TestRateLimitConfigParity(t *testing.T) {
	rule := config.FindRateLimitRule("/api/auth/login", "POST")
	if rule == nil || rule.Max != 5 {
		t.Fatalf("login rule = %+v, want max 5", rule)
	}
	if !config.IsWhitelisted("/api/health") {
		t.Fatal("/api/health should be whitelisted")
	}
	if config.IsWhitelisted("/api/users") {
		t.Fatal("/api/users should not be whitelisted")
	}
	if config.ParseTimeWindow("1 minute") != time.Minute {
		t.Fatal("1 minute should parse to time.Minute")
	}
	if config.ParseTimeWindow("30 seconds") != 30*time.Second {
		t.Fatal("30 seconds should parse to 30s")
	}
}
