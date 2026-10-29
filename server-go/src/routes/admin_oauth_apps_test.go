package routes

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// parsePositiveId is the strict `^[1-9]\d*$` regex plus Number.isSafeInteger.
// Values above 2^31-1 are *valid* at this layer; the route then range-checks them
// against the Int column and answers as the Node handler does (P2020 semantics).
func TestAdminOAuthAppsParsePositiveID(t *testing.T) {
	valid := map[string]int64{
		"1":                1,
		"7":                7,
		"2147483647":       2147483647,
		"2147483648":       2147483648,
		"9007199254740991": 9007199254740991, // Number.MAX_SAFE_INTEGER
	}
	for input, want := range valid {
		got, ok := adminOAuthAppsParsePositiveID(input)
		if !ok || got != want {
			t.Errorf("parsePositiveID(%q) = %d,%v want %d", input, got, ok, want)
		}
	}

	for _, input := range []string{
		"",
		"0",
		"01",
		"007",
		"-1",
		"+1",
		"abc",
		"1.5",
		"1e3",
		" 1",
		"1 ",
		"0x10",
		"9007199254740992",     // MAX_SAFE_INTEGER + 1
		"99999999999999999999", // beyond int64
	} {
		if got, ok := adminOAuthAppsParsePositiveID(input); ok {
			t.Errorf("parsePositiveID(%q) = %d, want invalid", input, got)
		}
	}
}

// Ids beyond an Int column must be flagged so the handler can answer with the
// same envelope the Node code produces (P2020 → 400/404/500 depending on route).
func TestAdminOAuthAppsFitsInt32(t *testing.T) {
	if !adminOAuthAppsFitsInt32(1) || !adminOAuthAppsFitsInt32(2147483647) {
		t.Error("in-range ids must fit")
	}
	if adminOAuthAppsFitsInt32(0) || adminOAuthAppsFitsInt32(-1) ||
		adminOAuthAppsFitsInt32(2147483648) || adminOAuthAppsFitsInt32(9007199254740991) {
		t.Error("out-of-range ids must not fit")
	}
}

// normalizeAppPayload checks name, then redirectUris, then scopes, in that order,
// and returns the exact Error message text.
func TestAdminOAuthAppsNormalizePayload(t *testing.T) {
	const (
		name        = "Partner App"
		redirectURI = "https://example.com/cb"
		scope       = "profile:read"
	)

	// Happy path: enabled defaults to true.
	payload, errMessage := adminOAuthAppsNormalizePayload(map[string]any{
		"name":         name,
		"redirectUris": []any{redirectURI},
		"scopes":       []any{scope},
	})
	if errMessage != "" {
		t.Fatalf("unexpected error %q", errMessage)
	}
	if payload.Name != name || len(payload.RedirectURIs) != 1 || payload.RedirectURIs[0] != redirectURI {
		t.Fatalf("payload = %+v", payload)
	}
	if len(payload.Scopes) != 1 || payload.Scopes[0] != scope {
		t.Fatalf("scopes = %v", payload.Scopes)
	}
	if !payload.Enabled {
		t.Fatal("enabled must default to true (body.enabled !== false)")
	}

	// The tri-state trap: only an explicit JSON false disables the app. Absent,
	// null, and even a truthy non-boolean leave it enabled, and a falsy non-boolean
	// (0, "") also leaves it enabled — matching `body.enabled !== false`.
	for _, body := range []map[string]any{
		{"name": name, "redirectUris": []any{redirectURI}, "scopes": []any{scope}},
		{"name": name, "redirectUris": []any{redirectURI}, "scopes": []any{scope}, "enabled": nil},
		{"name": name, "redirectUris": []any{redirectURI}, "scopes": []any{scope}, "enabled": float64(0)},
		{"name": name, "redirectUris": []any{redirectURI}, "scopes": []any{scope}, "enabled": ""},
		{"name": name, "redirectUris": []any{redirectURI}, "scopes": []any{scope}, "enabled": "false"},
		{"name": name, "redirectUris": []any{redirectURI}, "scopes": []any{scope}, "enabled": true},
	} {
		got, message := adminOAuthAppsNormalizePayload(body)
		if message != "" {
			t.Fatalf("body %v: unexpected error %q", body["enabled"], message)
		}
		if !got.Enabled {
			t.Errorf("enabled=%#v must stay true", body["enabled"])
		}
	}

	// An explicit false disables it.
	got, message := adminOAuthAppsNormalizePayload(map[string]any{
		"name": name, "redirectUris": []any{redirectURI}, "scopes": []any{scope}, "enabled": false,
	})
	if message != "" || got.Enabled {
		t.Fatalf("explicit false → enabled=%v err=%q", got.Enabled, message)
	}

	// Validation order: name first, then redirectUris, then scopes.
	cases := []struct {
		name    string
		body    map[string]any
		wantErr string
	}{
		{"empty body", map[string]any{}, adminOAuthAppsInvalidAppNameMessage},
		{"bad name only", map[string]any{"name": "  "}, adminOAuthAppsInvalidAppNameMessage},
		{
			"bad name beats bad redirects",
			map[string]any{"name": "  ", "redirectUris": []any{"ftp://x"}, "scopes": []any{"bad"}},
			adminOAuthAppsInvalidAppNameMessage,
		},
		{
			"bad redirects beat bad scopes",
			map[string]any{"name": name, "redirectUris": []any{"http://example.com/cb"}, "scopes": []any{"bad"}},
			adminOAuthAppsInvalidRedirectMessage,
		},
		{
			"missing redirectUris",
			map[string]any{"name": name, "scopes": []any{scope}},
			adminOAuthAppsInvalidRedirectMessage,
		},
		{
			"bad scopes",
			map[string]any{"name": name, "redirectUris": []any{redirectURI}, "scopes": []any{"root:all"}},
			adminOAuthAppsInvalidScopesMessage,
		},
		{
			"missing scopes",
			map[string]any{"name": name, "redirectUris": []any{redirectURI}},
			adminOAuthAppsInvalidScopesMessage,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, message := adminOAuthAppsNormalizePayload(tc.body); message != tc.wantErr {
				t.Errorf("error = %q, want %q", message, tc.wantErr)
			}
		})
	}
}

// A space-separated scope string is accepted by normalizeOAuthScopes, so
// `scopes: "profile:read balance:read"` is valid.
func TestAdminOAuthAppsNormalizePayloadAcceptsScopeString(t *testing.T) {
	payload, message := adminOAuthAppsNormalizePayload(map[string]any{
		"name":         "App",
		"redirectUris": []any{"https://example.com/cb"},
		"scopes":       "profile:read balance:read",
	})
	if message != "" {
		t.Fatalf("unexpected error %q", message)
	}
	if len(payload.Scopes) != 2 || payload.Scopes[0] != "profile:read" || payload.Scopes[1] != "balance:read" {
		t.Fatalf("scopes = %v", payload.Scopes)
	}
}

// The Prisma P2025 message echoed by PUT /:id for a missing row. Kept as a literal
// because it is a wire contract (it lands verbatim in the JSON error field):
// @prisma/client 7.1.0 in NODE_ENV=production prefixes the invalid-invocation
// header and then appends the coded message.
func TestAdminOAuthAppsUpdateNotFoundMessage(t *testing.T) {
	want := "\nInvalid `prisma.oAuthClientApp.update()` invocation:\n\n\n" +
		"An operation failed because it depends on one or more records that were " +
		"required but not found. No record was found for an update."
	if adminOAuthAppsUpdateNotFoundMessage != want {
		t.Fatalf("message = %q\nwant      %q", adminOAuthAppsUpdateNotFoundMessage, want)
	}

	// The out-of-range (P2020) variant shares the preamble.
	wantRange := "\nInvalid `prisma.oAuthClientApp.update()` invocation:\n\n\n" +
		"Value out of range for the type: value \"2147483648\" is out of range for type integer"
	if adminOAuthAppsOutOfRangeMessage != wantRange {
		t.Fatalf("range message = %q\nwant          %q", adminOAuthAppsOutOfRangeMessage, wantRange)
	}
}

// The app/serialization types exposed by the route must match the TypeScript
// object literals, and none of them may carry secret material.
func TestAdminOAuthAppsResponseShapes(t *testing.T) {
	app := db.OAuthAppsClientApp{
		ID:                1,
		Name:              "Partner",
		ClientID:          "pocli_abc",
		RedirectURIs:      []string{"https://example.com/cb"},
		Scopes:            []string{"profile:read"},
		Enabled:           true,
		CreatedByUserID:   9,
		CreatedByUsername: nil,
		CreatedAt:         "2026-10-29T12:00:00.000Z",
		UpdatedAt:         "2026-10-29T12:00:00.000Z",
	}

	encoded, err := json.Marshal(app)
	if err != nil {
		t.Fatalf("marshal app: %v", err)
	}
	for _, key := range []string{
		`"id"`, `"name"`, `"clientId"`, `"redirectUris"`, `"scopes"`, `"enabled"`,
		`"createdByUserId"`, `"createdByUsername"`, `"createdAt"`, `"updatedAt"`,
	} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("app JSON %s is missing %s", encoded, key)
		}
	}
	for _, forbidden := range []string{"secret", "Secret", "hash", "Hash"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("app JSON %s leaks %q", encoded, forbidden)
		}
	}

	// The create/rotate envelope carries the plaintext secret alongside the app.
	envelope, err := json.Marshal(gin.H{
		"app":          app,
		"clientSecret": "posec_plaintext",
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if !strings.Contains(string(envelope), `"clientSecret":"posec_plaintext"`) {
		t.Errorf("create envelope = %s, want the plaintext secret", envelope)
	}
}

// The admin authorization list envelope: pagination keys plus authorizations.
func TestAdminOAuthAppsAuthorizationPageShape(t *testing.T) {
	page := db.OAuthAppsAuthorizationPage{
		Authorizations: []db.OAuthAppsAdminAuthorization{},
		Total:          0,
		Page:           1,
		PageSize:       20,
		TotalPages:     1,
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"authorizations"`, `"total"`, `"page"`, `"pageSize"`, `"totalPages"`} {
		if !strings.Contains(string(encoded), key) {
			t.Errorf("page JSON %s is missing %s", encoded, key)
		}
	}
	// An empty list must serialize as [] , never null.
	if !strings.Contains(string(encoded), `"authorizations":[]`) {
		t.Errorf("authorizations must be an empty array: %s", encoded)
	}
}
