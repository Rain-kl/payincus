package db

import (
	"testing"
)

// Pinned to real URLSearchParams output (node 20):
//
//	new URL('https://example.com/cb?b=2&a=1'); searchParams.set('a','x y');
//	searchParams.set('state', "a!b'c(d)e*f~g/h+i") → b=2&a=x+y&state=a%21b%27c%28d%29e*f%7Eg%2Fh%2Bi
func TestOAuthProviderSetQueryParams(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		params [][2]string
		want   string
	}{
		{
			name:   "no existing query appends",
			raw:    "https://example.com/cb",
			params: [][2]string{{"code", "poc_abc-_~"}},
			want:   "https://example.com/cb?code=poc_abc-_%7E",
		},
		{
			name:   "existing keys keep their order and position",
			raw:    "https://example.com/cb?b=2&a=1",
			params: [][2]string{{"a", "x y"}, {"state", "a!b'c(d)e*f~g/h+i"}},
			want:   "https://example.com/cb?b=2&a=x+y&state=a%21b%27c%28d%29e*f%7Eg%2Fh%2Bi",
		},
		{
			name:   "utf8 is percent-encoded per byte",
			raw:    "https://example.com/cb?x=%E4%B8%AD",
			params: [][2]string{{"y", "中 文"}},
			want:   "https://example.com/cb?x=%E4%B8%AD&y=%E4%B8%AD+%E6%96%87",
		},
		{
			name:   "duplicate keys collapse to the first position",
			raw:    "https://example.com/cb?a=1&b=2&a=3",
			params: [][2]string{{"a", "9"}},
			want:   "https://example.com/cb?a=9&b=2",
		},
		{
			name:   "fragment is preserved",
			raw:    "https://example.com/cb#frag",
			params: [][2]string{{"code", "x"}},
			want:   "https://example.com/cb?code=x#frag",
		},
	}
	for _, tc := range cases {
		got, err := OAuthProviderSetQueryParams(tc.raw, tc.params)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}

	if _, err := OAuthProviderSetQueryParams("not a url", nil); err == nil {
		t.Fatal("an unparseable URL must be an error (new URL throws)")
	}
}

func TestOAuthProviderQueryFormEncoding(t *testing.T) {
	encoded := oauthProviderFormEncode("a!b'c(d)e*f~g/h+i;j:k l")
	want := "a%21b%27c%28d%29e*f%7Eg%2Fh%2Bi%3Bj%3Ak+l"
	if encoded != want {
		t.Fatalf("encode = %q, want %q", encoded, want)
	}
	if decoded := oauthProviderFormDecode(encoded); decoded != "a!b'c(d)e*f~g/h+i;j:k l" {
		t.Fatalf("decode = %q", decoded)
	}
	// Invalid percent sequences survive verbatim (URLSearchParams is lenient).
	if decoded := oauthProviderFormDecode("a%zz%2"); decoded != "a%zz%2" {
		t.Fatalf("lenient decode = %q", decoded)
	}
}

func TestOAuthProviderTokenPrefixesAndHash(t *testing.T) {
	if got := OAuthProviderAccessTokenPrefixValue("poa_0123456789abcdefXYZ"); got != "poa_0123456789ab" {
		t.Fatalf("access prefix = %q", got)
	}
	if got := OAuthProviderRefreshTokenPrefixValue("por_0123456789abcdefXYZ"); got != "por_0123456789ab" {
		t.Fatalf("refresh prefix = %q", got)
	}
	if got := OAuthProviderAccessTokenPrefixValue("short"); got != "short" {
		t.Fatalf("short token = %q", got)
	}
	// sha256("abc") = ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad
	if got := OAuthProviderHashSecret("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("hash = %q", got)
	}
}

func TestOAuthProviderReadRequestBody(t *testing.T) {
	if got := OAuthProviderReadRequestBody("text"); len(got) != 0 {
		t.Fatalf("string body = %v", got)
	}
	if got := OAuthProviderReadRequestBody(nil); len(got) != 0 {
		t.Fatalf("nil body = %v", got)
	}
	if got := OAuthProviderReadRequestBody([]any{1}); len(got) != 0 {
		t.Fatalf("array body = %v", got)
	}
	body := map[string]any{"token": "x"}
	if got := OAuthProviderReadRequestBody(body); got["token"] != "x" {
		t.Fatalf("object body = %v", got)
	}
}

func TestOAuthProviderSerializeScopes(t *testing.T) {
	scopes := OAuthProviderSerializeScopes([]byte(`["profile:read","profile:read","nope"]`))
	if len(scopes) != 0 {
		t.Fatalf("an invalid stored scope list degrades to [], got %v", scopes)
	}
	scopes = OAuthProviderSerializeScopes([]byte(`["profile:read","balance:read"]`))
	if len(scopes) != 2 || scopes[0] != "profile:read" {
		t.Fatalf("scopes = %v", scopes)
	}
	if scopes := OAuthProviderSerializeScopes(nil); len(scopes) != 0 {
		t.Fatalf("empty JSON = %v", scopes)
	}
}
