package routes

import (
	"testing"
)

func TestOAuthProviderStringValue(t *testing.T) {
	if got := oauthProviderStringValue("  abc  ", 10); got == nil || *got != "abc" {
		t.Fatalf("trim = %v", got)
	}
	if got := oauthProviderStringValue("   ", 10); got != nil {
		t.Fatalf("blank = %v", *got)
	}
	if got := oauthProviderStringValue(5, 10); got != nil {
		t.Fatalf("non-string = %v", *got)
	}
	if got := oauthProviderStringValue("abcdef", 5); got != nil {
		t.Fatalf("too long = %v", *got)
	}
	if got := oauthProviderStringValue("abcde", 5); got == nil {
		t.Fatalf("exactly max must pass")
	}
	// .length counts UTF-16 units, so an emoji counts as two.
	if got := oauthProviderStringValue("a😀", 2); got != nil {
		t.Fatalf("UTF-16 length 3 must exceed the 2-unit bound, got %q", *got)
	}
	if got := oauthProviderStringValue("a😀", 3); got == nil {
		t.Fatalf("UTF-16 length 3 must pass")
	}
}

func TestOAuthProviderNormalizeScopes(t *testing.T) {
	scopes := oauthProviderNormalizeScopes("profile:read  balance:read")
	if len(scopes) != 2 || scopes[0] != "profile:read" || scopes[1] != "balance:read" {
		t.Fatalf("string split = %v", scopes)
	}
	if scopes := oauthProviderNormalizeScopes([]any{"profile:read", "profile:read"}); len(scopes) != 1 {
		t.Fatalf("dedupe = %v", scopes)
	}
	if scopes := oauthProviderNormalizeScopes([]any{"profile:read", 5, nil}); len(scopes) != 1 {
		t.Fatalf("non-strings are dropped = %v", scopes)
	}
	if scopes := oauthProviderNormalizeScopes(""); scopes != nil {
		t.Fatalf("empty string = %v", scopes)
	}
	if scopes := oauthProviderNormalizeScopes([]any{}); scopes != nil {
		t.Fatalf("empty array = %v", scopes)
	}
	if scopes := oauthProviderNormalizeScopes("nope:read"); scopes != nil {
		t.Fatalf("unknown scope = %v", scopes)
	}
	if scopes := oauthProviderNormalizeScopes(5); scopes != nil {
		t.Fatalf("non-string/array = %v", scopes)
	}

	tooMany := make([]any, 25)
	for index := range tooMany {
		tooMany[index] = "profile:read"
	}
	if scopes := oauthProviderNormalizeScopes(tooMany); scopes != nil {
		t.Fatalf("25 raw entries must be rejected before dedupe, got %v", scopes)
	}

	// A leading space produces an empty first segment, which is filtered — but it
	// still counts toward the 24-entry raw-length guard in JavaScript.
	if scopes := oauthProviderNormalizeScopes(" profile:read"); len(scopes) != 1 || scopes[0] != "profile:read" {
		t.Fatalf("leading space = %v", scopes)
	}
}

func TestOAuthProviderReadAuthorizeInput(t *testing.T) {
	// snake_case wins when camelCase is absent; the default scope applies.
	input := oauthProviderReadAuthorizeInput(map[string]any{
		"response_type": "code",
		"client_id":     "pocli_x",
		"redirect_uri":  "https://example.com/cb",
	})
	if input.ResponseType == nil || *input.ResponseType != "code" {
		t.Fatalf("responseType = %v", input.ResponseType)
	}
	if input.ClientID == nil || *input.ClientID != "pocli_x" {
		t.Fatalf("clientId = %v", input.ClientID)
	}
	if input.RedirectURI == nil || *input.RedirectURI != "https://example.com/cb" {
		t.Fatalf("redirectUri = %v", input.RedirectURI)
	}
	if len(input.Scopes) != 1 || input.Scopes[0] != "profile:read" {
		t.Fatalf("default scopes = %v", input.Scopes)
	}

	// camelCase wins over snake_case when truthy; empty strings fall through.
	input = oauthProviderReadAuthorizeInput(map[string]any{
		"responseType":  "code",
		"response_type": "token",
		"clientId":      "pocli_camel",
		"client_id":     "pocli_snake",
		"redirectUri":   "",
		"redirect_uri":  "https://example.com/cb",
		"scope":         "balance:read",
		"state":         " s ",
	})
	if *input.ResponseType != "code" {
		t.Fatalf("responseType = %q", *input.ResponseType)
	}
	if *input.ClientID != "pocli_camel" {
		t.Fatalf("clientId = %q", *input.ClientID)
	}
	if *input.RedirectURI != "https://example.com/cb" {
		t.Fatalf("redirectUri = %q", *input.RedirectURI)
	}
	if len(input.Scopes) != 1 || input.Scopes[0] != "balance:read" {
		t.Fatalf("scopes = %v", input.Scopes)
	}
	if input.State == nil || *input.State != "s" {
		t.Fatalf("state = %v", input.State)
	}

	// `'scopes' in body ? body.scope || body.scopes : body.scope`
	input = oauthProviderReadAuthorizeInput(map[string]any{
		"response_type": "code", "client_id": "x", "redirect_uri": "https://e.com/cb",
		"scopes": []any{"profile:write"},
	})
	if len(input.Scopes) != 1 || input.Scopes[0] != "profile:write" {
		t.Fatalf("scopes array = %v", input.Scopes)
	}

	// An invalid response_type is surfaced as nil, not as the literal value.
	input = oauthProviderReadAuthorizeInput(map[string]any{"response_type": "token"})
	if input.ResponseType == nil || *input.ResponseType != "token" {
		t.Fatalf("responseType = %v", input.ResponseType)
	}
	input = oauthProviderReadAuthorizeInput(map[string]any{"response_type": stringsRepeat("x", 21)})
	if input.ResponseType != nil {
		t.Fatalf("over-long responseType must be nil, got %q", *input.ResponseType)
	}
}

func TestOAuthProviderEncodeURIComponent(t *testing.T) {
	cases := map[string]string{
		"abc123-_.":         "abc123-_.",
		"a b":               "a%20b",
		"a!b'c(d)e*f~g/h+i": "a!b'c(d)e*f~g%2Fh%2Bi",
		"中":                 "%E4%B8%AD",
		"client_id":         "client_id",
		"a:b;c,d?e":         "a%3Ab%3Bc%2Cd%3Fe",
	}
	for input, want := range cases {
		if got := oauthProviderEncodeURIComponent(input); got != want {
			t.Errorf("encodeURIComponent(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestOAuthProviderBuildConsentURL(t *testing.T) {
	url := oauthProviderBuildConsentURL("pocli_x", "https://example.com/cb", []string{"profile:read", "balance:read"}, nil)
	want := "/oauth/authorize?client_id=pocli_x&redirect_uri=https%3A%2F%2Fexample.com%2Fcb&scope=profile%3Aread%20balance%3Aread"
	if url != want {
		t.Fatalf("consent url = %q, want %q", url, want)
	}
	state := "s 1"
	url = oauthProviderBuildConsentURL("pocli_x", "https://example.com/cb", []string{"profile:read"}, &state)
	want = "/oauth/authorize?client_id=pocli_x&redirect_uri=https%3A%2F%2Fexample.com%2Fcb&scope=profile%3Aread&state=s%201"
	if url != want {
		t.Fatalf("consent url with state = %q, want %q", url, want)
	}
	empty := ""
	if url := oauthProviderBuildConsentURL("c", "https://e.com/cb", []string{"a:b"}, &empty); stringsContains(url, "state=") {
		t.Fatalf("empty state must be omitted, got %q", url)
	}
}

func TestOAuthProviderJSOrAndTruthy(t *testing.T) {
	if oauthProviderJSTruthy(nil) || oauthProviderJSTruthy("") || oauthProviderJSTruthy(float64(0)) || oauthProviderJSTruthy(false) {
		t.Fatal("empty values must be falsy")
	}
	if !oauthProviderJSTruthy("x") || !oauthProviderJSTruthy(float64(1)) || !oauthProviderJSTruthy(true) || !oauthProviderJSTruthy([]any{}) {
		t.Fatal("non-empty values must be truthy")
	}
	if got := oauthProviderJSOr("", nil, "code"); got != "code" {
		t.Fatalf("JSOr = %v", got)
	}
	if got := oauthProviderJSOr("first", "second"); got != "first" {
		t.Fatalf("JSOr = %v", got)
	}
	if got := oauthProviderJSOr("", ""); got != nil {
		t.Fatalf("all-empty JSOr = %v", got)
	}
}

// Tiny local helpers keep this file independent of other agents' test helpers.
func stringsRepeat(value string, count int) string {
	out := ""
	for index := 0; index < count; index++ {
		out += value
	}
	return out
}

func stringsContains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
