// Tests for the redirect-validator port. The table is the open-redirect bypass
// matrix: every "false" case was verified against the TypeScript
// isValidRedirectUrl with Node 26 (same inputs, same verdicts) before being pinned
// here. `getSafeRedirectUrl` must additionally return the trimmed value.
package lib

import "testing"

func TestSafeRedirectBypassMatrix(t *testing.T) {
	cases := []struct {
		name  string
		input string
		valid bool
		// safe is the expected GetSafeRedirectURL result with default "/".
		safe string
	}{
		// --- protocol-relative and scheme tricks ---
		{"protocol-relative", "//evil.com", false, "/"},
		{"protocol-relative-with-path", "//evil.com/x", false, "/"},
		{"bare-double-slash", "//", false, "/"},
		{"backslash-relative", `/\evil.com`, false, "/"},
		{"leading-backslash", `\evil.com`, false, "/"},
		{"backslash-anywhere", "/a\\b", false, "/"},
		{"absolute-https", "https://evil.com", false, "/"},
		{"absolute-http", "http://evil.com", false, "/"},
		{"https-single-slash", "https:/evil.com", false, "/"},
		{"scheme-relative-colon", "evil.com:80", false, "/"},
		{"protocol-with-space-scheme", "//evil.com	", false, "/"},

		// --- javascript:/data:/vbscript: ---
		{"javascript-scheme", "javascript:alert(1)", false, "/"},
		{"javascript-in-path", "/javascript:void(0)", false, "/"},
		{"javascript-after-query", "/path?x=javascript:alert(1)", false, "/"},
		{"javascript-uppercase", "/JavaScript:alert(1)", false, "/"},
		{"data-uri", "/data:text/html;base64,PHNjcmlwdD4=", false, "/"},
		{"vbscript", "/vbscript:msgbox(1)", false, "/"},

		// --- encoded bypasses ---
		{"encoded-protocol-relative", "/%2F%2Fevil.com", false, "/"},
		{"encoded-protocol-relative-lower", "/%2f%2fevil.com", false, "/"},
		{"encoded-protocol-relative-short", "/%2Fx", false, "/"},
		{"encoded-backslash-prefix", "/%5Cevil.com", false, "/"},
		{"encoded-backslash-inside", "/x%5C", false, "/"},
		{"double-encoded-protocol-relative-stays-path", "/%252F%252Fevil.com", true, "/%252F%252Fevil.com"},
		{"encoded-newline-crlf", "/%0d%0aSet-Cookie:x", false, "/"},
		{"encoded-newline-upper", "/%0D", false, "/"},
		{"encoded-newline-lf", "/%0A", false, "/"},
		{"encoded-vertical-tab-not-blocked", "/%0B", true, "/%0B"},
		{"encoded-form-feed-not-blocked", "/%0C", true, "/%0C"},

		// --- raw control characters ---
		{"carriage-return", "/x\ry", false, "/"},
		{"line-feed", "/x\ny", false, "/"},
		{"tab", "/x\ty", false, "/"},
		{"leading-tab-absolute", " /\tevil", false, "/"},

		// --- userinfo / authority tricks (all stay on-origin once validated) ---
		{"userinfo-is-path", "/@evil.com", true, "/@evil.com"},
		{"userinfo-with-backslash", `/\\@evil.com`, false, "/"},
		{"dot-segment-path", "/..//evil.com", true, "/..//evil.com"},

		// --- legitimate relative paths, including whitespace trimming ---
		{"root", "/", true, "/"},
		{"simple", "/profile", true, "/profile"},
		{"path-with-query", "/admin/users?tab=1", true, "/admin/users?tab=1"},
		{"nested", "/a/b/c", true, "/a/b/c"},
		{"padded", "  /profile  ", true, "/profile"},
		{"bom-prefixed-relative", "\uFEFF/profile", true, "/profile"},
		{"bom-prefixed-protocol-relative", "\uFEFF//evil.com", false, "/"},
		{"empty", "", false, "/"},
		{"spaces-only", "   ", false, "/"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsValidRedirectURL(tc.input); got != tc.valid {
				t.Fatalf("IsValidRedirectURL(%q) = %v, want %v", tc.input, got, tc.valid)
			}
			if got := GetSafeRedirectURL(tc.input, "/"); got != tc.safe {
				t.Fatalf("GetSafeRedirectURL(%q) = %q, want %q", tc.input, got, tc.safe)
			}
		})
	}
}

// TestSafeRedirectDefaultOverride covers the bind-mode default.
func TestSafeRedirectDefaultOverride(t *testing.T) {
	if got := GetSafeRedirectURL("//evil.com", "/profile"); got != "/profile" {
		t.Fatalf("bind-mode default = %q, want /profile", got)
	}
	if got := GetSafeRedirectURL("/settings", "/profile"); got != "/settings" {
		t.Fatalf("valid url must win over the default, got %q", got)
	}
}
