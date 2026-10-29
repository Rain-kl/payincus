package lib

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ==================== test scaffolding ====================

type authSeams struct {
	apiToken   *publicAPITokenRecord
	apiErr     error
	oauthToken *oauthAccessTokenRecord
	oauthErr   error
	touchErr   error

	logs        []string
	touchedAPI  []int32
	touchedOAut []int32
}

// withSeams swaps the package-level DB seams for fakes and restores them after
// the test, so the full auth flow can be exercised without a live database.
func withSeams(t *testing.T, seams *authSeams) {
	t.Helper()

	prevLookup := publicAPIAuthLookupToken
	prevLookupOAuth := publicAPIAuthLookupOAuthToken
	prevTouch := publicAPIAuthTouchToken
	prevTouchOAuth := publicAPIAuthTouchOAuthToken
	prevLog := publicAPIAuthWriteLog

	publicAPIAuthLookupToken = func(_ context.Context, _ string) (*publicAPITokenRecord, error) {
		return seams.apiToken, seams.apiErr
	}
	publicAPIAuthLookupOAuthToken = func(_ context.Context, _ string) (*oauthAccessTokenRecord, error) {
		return seams.oauthToken, seams.oauthErr
	}
	publicAPIAuthTouchToken = func(_ context.Context, id int32) error {
		seams.touchedAPI = append(seams.touchedAPI, id)
		return seams.touchErr
	}
	publicAPIAuthTouchOAuthToken = func(_ context.Context, id int32) error {
		seams.touchedOAut = append(seams.touchedOAut, id)
		return seams.touchErr
	}
	publicAPIAuthWriteLog = func(_ context.Context, _ *int64, action, content string) {
		seams.logs = append(seams.logs, action+"|"+content)
	}

	t.Cleanup(func() {
		publicAPIAuthLookupToken = prevLookup
		publicAPIAuthLookupOAuthToken = prevLookupOAuth
		publicAPIAuthTouchToken = prevTouch
		publicAPIAuthTouchOAuthToken = prevTouchOAuth
		publicAPIAuthWriteLog = prevLog
	})
}

func newAuthContext(authorization string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	if authorization != "" {
		c.Request.Header.Set("Authorization", authorization)
	}
	return c, recorder
}

func mustScopesJSON(t *testing.T, scopes []PublicAPIScope) []byte {
	t.Helper()
	raw, err := json.Marshal(scopes)
	if err != nil {
		t.Fatalf("marshal scopes: %v", err)
	}
	return raw
}

// validAPITokenRecord builds a live, unexpired token record bound to raw.
func validAPITokenRecord(t *testing.T, raw string, scopes []PublicAPIScope) *publicAPITokenRecord {
	t.Helper()
	return &publicAPITokenRecord{
		ID:          11,
		UserID:      7,
		Name:        "CI token",
		TokenPrefix: GetPublicAPITokenPrefix(raw),
		TokenHash:   HashPublicAPIToken(raw),
		Scopes:      mustScopesJSON(t, scopes),
		User: publicAPIUserProjection{
			ID: 7, Username: "alice", Role: "user", Status: "active", AvatarStyle: "default",
		},
	}
}

func validOAuthTokenRecord(t *testing.T, raw string, scopes []PublicAPIScope) *oauthAccessTokenRecord {
	t.Helper()
	return &oauthAccessTokenRecord{
		ID:          21,
		UserID:      7,
		TokenPrefix: GetPublicAPITokenPrefix(raw),
		TokenHash:   HashPublicAPIToken(raw),
		Scopes:      mustScopesJSON(t, scopes),
		ExpiresAt:   time.Now().Add(time.Hour),
		AppName:     "Grafana",
		AppEnabled:  true,
		User: publicAPIUserProjection{
			ID: 7, Username: "alice", Role: "user", Status: "active", AvatarStyle: "default",
		},
	}
}

func decodeErrorBody(t *testing.T, recorder *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", recorder.Body.String(), err)
	}
	return body.Error, body.Code
}

// ==================== token shape / hashing ====================

func TestGeneratePublicAPITokenShape(t *testing.T) {
	first := GeneratePublicAPIToken()
	second := GeneratePublicAPIToken()

	if !strings.HasPrefix(first, PublicAPITokenPrefix) {
		t.Fatalf("token %q missing %q prefix", first, PublicAPITokenPrefix)
	}
	// `pat_` + 43 base64url chars.
	if len(first) != len(PublicAPITokenPrefix)+43 {
		t.Fatalf("token length = %d, want %d", len(first), len(PublicAPITokenPrefix)+43)
	}
	for _, r := range first[len(PublicAPITokenPrefix):] {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", r) {
			t.Fatalf("token body has non-base64url char %q in %q", r, first)
		}
	}
	if first == second {
		t.Fatal("two generated tokens must differ")
	}
}

func TestHashPublicAPITokenKnownVector(t *testing.T) {
	// SHA-256("hello") — proves the digest is lowercase hex SHA-256.
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got := HashPublicAPIToken("hello"); got != want {
		t.Fatalf("hash = %q, want %q", got, want)
	}
}

func TestGetPublicAPITokenPrefix(t *testing.T) {
	token := "pat_0123456789ABCDEFGHIJ"
	if got := GetPublicAPITokenPrefix(token); got != "pat_0123456789AB" {
		t.Fatalf("prefix = %q, want pat_0123456789AB", got)
	}
	if got := GetPublicAPITokenPrefix("short"); got != "short" {
		t.Fatalf("short prefix = %q, want short", got)
	}
}

// ==================== normalization ====================

func TestNormalizePublicAPITokenName(t *testing.T) {
	if got := NormalizePublicAPITokenName("  my token  "); got == nil || *got != "my token" {
		t.Fatalf("trimmed name = %v, want my token", got)
	}
	if got := NormalizePublicAPITokenName("   "); got != nil {
		t.Fatalf("blank name = %v, want nil", got)
	}
	if got := NormalizePublicAPITokenName(42); got != nil {
		t.Fatalf("non-string name = %v, want nil", got)
	}
	if got := NormalizePublicAPITokenName(strings.Repeat("x", PublicAPITokenMaxNameLength+1)); got != nil {
		t.Fatalf("over-long name = %v, want nil", got)
	}
	if got := NormalizePublicAPITokenName(strings.Repeat("x", PublicAPITokenMaxNameLength)); got == nil {
		t.Fatal("max-length name must be accepted")
	}
}

func TestNormalizePublicAPIScopes(t *testing.T) {
	// Valid: order preserved.
	got := NormalizePublicAPIScopes([]any{"profile:read", "balance:read"})
	if len(got) != 2 || got[0] != PublicAPIScopeProfileRead || got[1] != PublicAPIScopeBalanceRead {
		t.Fatalf("valid scopes = %v", got)
	}
	// Whitespace is trimmed.
	if got := NormalizePublicAPIScopes([]any{"  tickets:write "}); len(got) != 1 || got[0] != PublicAPIScopeTicketsWrite {
		t.Fatalf("trimmed scopes = %v", got)
	}
	// Unknown scope → whole input invalid.
	if got := NormalizePublicAPIScopes([]any{"profile:read", "root:everything"}); got != nil {
		t.Fatalf("unknown scope = %v, want nil", got)
	}
	// Duplicate → length mismatch → invalid (mirrors the Node length check).
	if got := NormalizePublicAPIScopes([]any{"profile:read", "profile:read"}); got != nil {
		t.Fatalf("duplicate scope = %v, want nil", got)
	}
	// Empty and non-array.
	if got := NormalizePublicAPIScopes([]any{}); got != nil {
		t.Fatalf("empty scopes = %v, want nil", got)
	}
	if got := NormalizePublicAPIScopes("profile:read"); got != nil {
		t.Fatalf("non-array scopes = %v, want nil", got)
	}
	// Non-string entry → mapped to "" → invalid.
	if got := NormalizePublicAPIScopes([]any{"profile:read", 5}); got != nil {
		t.Fatalf("non-string entry = %v, want nil", got)
	}
	// Over the cap.
	tooMany := make([]any, 0, PublicAPITokenMaxScopes+1)
	for i := 0; i <= PublicAPITokenMaxScopes; i++ {
		tooMany = append(tooMany, "profile:read")
	}
	if got := NormalizePublicAPIScopes(tooMany); got != nil {
		t.Fatalf("over-cap scopes = %v, want nil", got)
	}
}

func TestListPublicAPIScopeMetadata(t *testing.T) {
	all := ListPublicAPIScopeMetadata()
	if len(all) != len(PublicAPIScopes) {
		t.Fatalf("metadata count = %d, want %d", len(all), len(PublicAPIScopes))
	}
	if all[0].Scope != PublicAPIScopeProfileRead || all[0].Risk != "low" || !all[0].Implemented {
		t.Fatalf("first metadata = %+v", all[0])
	}
	subset := ListPublicAPIScopeMetadata(PublicAPIScopeTicketsWrite)
	if len(subset) != 1 || subset[0].Risk != "high" || subset[0].Access != "write" {
		t.Fatalf("subset metadata = %+v", subset)
	}
}

// ==================== bearer parsing ====================

func TestReadPublicAPIBearerToken(t *testing.T) {
	pat := PublicAPITokenPrefix + strings.Repeat("A", 43)
	poa := OAuthAccessTokenPrefix + strings.Repeat("B", 43)

	cases := []struct {
		name    string
		header  string
		wantNil bool
	}{
		{"absent", "", true},
		{"wrong scheme", "Basic " + pat, true},
		{"wrong prefix", "Bearer sk_" + strings.Repeat("C", 43), true},
		{"too short", "Bearer " + PublicAPITokenPrefix + "abc", true},
		{"pat ok", "Bearer " + pat, false},
		{"poa ok", "Bearer " + poa, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				request.Header.Set("Authorization", tc.header)
			}
			got := ReadPublicAPIBearerToken(request)
			if tc.wantNil && got != nil {
				t.Fatalf("got %q, want nil", *got)
			}
			if !tc.wantNil && got == nil {
				t.Fatal("got nil, want token")
			}
		})
	}
}

func TestSafeTimingEqual(t *testing.T) {
	if !safeTimingEqual("same-value", "same-value") {
		t.Fatal("equal values must compare equal")
	}
	if safeTimingEqual("value-a", "value-b") {
		t.Fatal("different values must compare unequal")
	}
	if safeTimingEqual("short", "longer-value") {
		t.Fatal("length mismatch must compare unequal")
	}
	if !safeTimingEqual("", "") {
		t.Fatal("empty strings must compare equal")
	}
}

// ==================== authenticate: shared failures ====================

func TestAuthenticatePublicAPIRequestRejectsMissingToken(t *testing.T) {
	withSeams(t, &authSeams{})
	c, recorder := newAuthContext("")

	if token := AuthenticatePublicAPIRequest(c, PublicAPIScopeProfileRead); token != nil {
		t.Fatal("expected nil token")
	}
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if _, code := decodeErrorBody(t, recorder); code != string(ErrPublicAPITokenRequired) {
		t.Fatalf("code = %q, want %q", code, ErrPublicAPITokenRequired)
	}
}

func TestAuthenticatePublicAPIRequestRejectsUnknownToken(t *testing.T) {
	withSeams(t, &authSeams{apiToken: nil})
	raw := GeneratePublicAPIToken()
	c, recorder := newAuthContext("Bearer " + raw)

	if token := AuthenticatePublicAPIRequest(c, PublicAPIScopeProfileRead); token != nil {
		t.Fatal("expected nil token")
	}
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if _, code := decodeErrorBody(t, recorder); code != string(ErrPublicAPITokenInvalid) {
		t.Fatalf("code = %q, want %q", code, ErrPublicAPITokenInvalid)
	}
}

func TestAuthenticatePublicAPIRequestRejectsPrefixMismatch(t *testing.T) {
	raw := GeneratePublicAPIToken()
	record := validAPITokenRecord(t, raw, []PublicAPIScope{PublicAPIScopeProfileRead})
	record.TokenPrefix = "pat_000000000000" // stored prefix does not match the token
	withSeams(t, &authSeams{apiToken: record})

	c, recorder := newAuthContext("Bearer " + raw)
	if token := AuthenticatePublicAPIRequest(c, PublicAPIScopeProfileRead); token != nil {
		t.Fatal("expected nil token")
	}
	if _, code := decodeErrorBody(t, recorder); code != string(ErrPublicAPITokenInvalid) {
		t.Fatalf("code = %q, want %q", code, ErrPublicAPITokenInvalid)
	}
}

func TestAuthenticatePublicAPIRequestRejectsHashMismatch(t *testing.T) {
	raw := GeneratePublicAPIToken()
	record := validAPITokenRecord(t, raw, []PublicAPIScope{PublicAPIScopeProfileRead})
	record.TokenHash = HashPublicAPIToken("a-different-token") // constant-time compare must fail
	withSeams(t, &authSeams{apiToken: record})

	c, recorder := newAuthContext("Bearer " + raw)
	if token := AuthenticatePublicAPIRequest(c, PublicAPIScopeProfileRead); token != nil {
		t.Fatal("expected nil token")
	}
	if _, code := decodeErrorBody(t, recorder); code != string(ErrPublicAPITokenInvalid) {
		t.Fatalf("code = %q, want %q", code, ErrPublicAPITokenInvalid)
	}
}

func TestAuthenticatePublicAPIRequestRejectsRevokedToken(t *testing.T) {
	raw := GeneratePublicAPIToken()
	record := validAPITokenRecord(t, raw, []PublicAPIScope{PublicAPIScopeProfileRead})
	revoked := time.Now().Add(-time.Minute)
	record.RevokedAt = &revoked
	withSeams(t, &authSeams{apiToken: record})

	c, recorder := newAuthContext("Bearer " + raw)
	if token := AuthenticatePublicAPIRequest(c, PublicAPIScopeProfileRead); token != nil {
		t.Fatal("expected nil token")
	}
	if _, code := decodeErrorBody(t, recorder); code != string(ErrPublicAPITokenRevoked) {
		t.Fatalf("code = %q, want %q", code, ErrPublicAPITokenRevoked)
	}
}

func TestAuthenticatePublicAPIRequestRejectsExpiredToken(t *testing.T) {
	raw := GeneratePublicAPIToken()
	record := validAPITokenRecord(t, raw, []PublicAPIScope{PublicAPIScopeProfileRead})
	expired := time.Now().Add(-time.Second)
	record.ExpiresAt = &expired
	withSeams(t, &authSeams{apiToken: record})

	c, recorder := newAuthContext("Bearer " + raw)
	if token := AuthenticatePublicAPIRequest(c, PublicAPIScopeProfileRead); token != nil {
		t.Fatal("expected nil token")
	}
	if _, code := decodeErrorBody(t, recorder); code != string(ErrPublicAPITokenExpired) {
		t.Fatalf("code = %q, want %q", code, ErrPublicAPITokenExpired)
	}
}

func TestAuthenticatePublicAPIRequestRejectsBannedUser(t *testing.T) {
	raw := GeneratePublicAPIToken()
	record := validAPITokenRecord(t, raw, []PublicAPIScope{PublicAPIScopeProfileRead})
	record.User.Status = "banned"
	withSeams(t, &authSeams{apiToken: record})

	c, recorder := newAuthContext("Bearer " + raw)
	if token := AuthenticatePublicAPIRequest(c, PublicAPIScopeProfileRead); token != nil {
		t.Fatal("expected nil token")
	}
	if _, code := decodeErrorBody(t, recorder); code != string(ErrAccountBanned) {
		t.Fatalf("code = %q, want %q", code, ErrAccountBanned)
	}
}

func TestAuthenticatePublicAPIRequestRejectsInsufficientScope(t *testing.T) {
	raw := GeneratePublicAPIToken()
	record := validAPITokenRecord(t, raw, []PublicAPIScope{PublicAPIScopeProfileRead})
	seams := &authSeams{apiToken: record}
	withSeams(t, seams)

	c, recorder := newAuthContext("Bearer " + raw)
	// Token only has profile:read; require balance:write.
	if token := AuthenticatePublicAPIRequest(c, PublicAPIScopeBalanceWrite); token != nil {
		t.Fatal("expected nil token")
	}
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	if _, code := decodeErrorBody(t, recorder); code != string(ErrPublicAPIScopeRequired) {
		t.Fatalf("code = %q, want %q", code, ErrPublicAPIScopeRequired)
	}
	if len(seams.logs) != 1 || !strings.HasPrefix(seams.logs[0], "public_api.token_scope_denied|") {
		t.Fatalf("scope-denied log = %v", seams.logs)
	}
	if len(seams.touchedAPI) != 0 {
		t.Fatalf("last_used_at must not be touched on denial: %v", seams.touchedAPI)
	}
}

func TestAuthenticatePublicAPIRequestSuccess(t *testing.T) {
	raw := GeneratePublicAPIToken()
	record := validAPITokenRecord(t, raw, []PublicAPIScope{PublicAPIScopeProfileRead, PublicAPIScopeBalanceRead})
	seams := &authSeams{apiToken: record}
	withSeams(t, seams)

	c, recorder := newAuthContext("Bearer " + raw)
	token := AuthenticatePublicAPIRequest(c, PublicAPIScopeBalanceRead)
	if token == nil {
		t.Fatalf("expected token, status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if token.Source != PublicAPITokenSourceAPI || token.UserID != 7 || token.User.Username != "alice" {
		t.Fatalf("token = %+v", token)
	}
	if len(token.Scopes) != 2 {
		t.Fatalf("scopes = %v", token.Scopes)
	}
	if len(seams.touchedAPI) != 1 || seams.touchedAPI[0] != record.ID {
		t.Fatalf("touched = %v, want [%d]", seams.touchedAPI, record.ID)
	}
	if len(seams.logs) != 0 {
		t.Fatalf("unexpected logs on success: %v", seams.logs)
	}
}

// ==================== authenticate: OAuth access tokens ====================

func TestAuthenticateOAuthTokenRejectsDisabledApp(t *testing.T) {
	raw := OAuthAccessTokenPrefix + strings.Repeat("Z", 43)
	record := validOAuthTokenRecord(t, raw, []PublicAPIScope{PublicAPIScopeProfileRead})
	record.AppEnabled = false
	withSeams(t, &authSeams{oauthToken: record})

	c, recorder := newAuthContext("Bearer " + raw)
	if token := AuthenticatePublicAPIRequest(c, PublicAPIScopeProfileRead); token != nil {
		t.Fatal("expected nil token")
	}
	if _, code := decodeErrorBody(t, recorder); code != string(ErrOAuthAppDisabled) {
		t.Fatalf("code = %q, want %q", code, ErrOAuthAppDisabled)
	}
}

func TestAuthenticateOAuthTokenRejectsExpired(t *testing.T) {
	raw := OAuthAccessTokenPrefix + strings.Repeat("Z", 43)
	record := validOAuthTokenRecord(t, raw, []PublicAPIScope{PublicAPIScopeProfileRead})
	record.ExpiresAt = time.Now().Add(-time.Second)
	withSeams(t, &authSeams{oauthToken: record})

	c, recorder := newAuthContext("Bearer " + raw)
	if token := AuthenticatePublicAPIRequest(c, PublicAPIScopeProfileRead); token != nil {
		t.Fatal("expected nil token")
	}
	if _, code := decodeErrorBody(t, recorder); code != string(ErrPublicAPITokenExpired) {
		t.Fatalf("code = %q, want %q", code, ErrPublicAPITokenExpired)
	}
}

func TestAuthenticateOAuthTokenRejectsInsufficientScope(t *testing.T) {
	raw := OAuthAccessTokenPrefix + strings.Repeat("Z", 43)
	record := validOAuthTokenRecord(t, raw, []PublicAPIScope{PublicAPIScopeProfileRead})
	seams := &authSeams{oauthToken: record}
	withSeams(t, seams)

	c, recorder := newAuthContext("Bearer " + raw)
	if token := AuthenticatePublicAPIRequest(c, PublicAPIScopeTicketsWrite); token != nil {
		t.Fatal("expected nil token")
	}
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	if len(seams.logs) != 1 || !strings.HasPrefix(seams.logs[0], "oauth_provider.token_scope_denied|") {
		t.Fatalf("scope-denied log = %v", seams.logs)
	}
}

func TestAuthenticateOAuthTokenSuccess(t *testing.T) {
	raw := OAuthAccessTokenPrefix + strings.Repeat("Z", 43)
	record := validOAuthTokenRecord(t, raw, []PublicAPIScope{PublicAPIScopeProfileRead})
	seams := &authSeams{oauthToken: record}
	withSeams(t, seams)

	c, recorder := newAuthContext("Bearer " + raw)
	token := AuthenticatePublicAPIRequest(c, PublicAPIScopeProfileRead)
	if token == nil {
		t.Fatalf("expected token, status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if token.Source != PublicAPITokenSourceOAuth {
		t.Fatalf("source = %q, want %q", token.Source, PublicAPITokenSourceOAuth)
	}
	if token.Name != "OAuth app Grafana" {
		t.Fatalf("name = %q, want OAuth app Grafana", token.Name)
	}
	if len(seams.touchedOAut) != 1 || seams.touchedOAut[0] != record.ID {
		t.Fatalf("touched = %v, want [%d]", seams.touchedOAut, record.ID)
	}
}
