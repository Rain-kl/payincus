// Port of server/src/lib/redirect-validator.ts.
//
// This is the open-redirect defence for every `/login?redirect=...` style
// parameter (the OAuth authorize/callback pair is the security-critical caller).
// The rules are reproduced check-for-check and in the same order, because each
// one closes a specific browser-normalisation bypass:
//
//   - the value must be a relative path starting with a single '/';
//   - '//host' and '/\host' are rejected (browsers treat both as protocol-relative);
//   - any backslash anywhere is rejected (WHATWG URL treats '\' as '/');
//   - '%5c' anywhere, and '/%2f' | '/%5c' prefixes, are rejected (double-encoding);
//   - javascript:/data:/vbscript: are rejected case-insensitively;
//   - CR/LF/TAB and their percent-encoded forms are rejected (header splitting).
//
// `strings.TrimSpace` is deliberately NOT used for the initial trim: JavaScript's
// String#trim removes U+FEFF as well (and leaves U+0085 alone), so JSTrim below
// reproduces the exact ECMAScript WhiteSpace + LineTerminator set.
package lib

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	redirectControlCharsPattern   = regexp.MustCompile("[\r\n\t]")
	redirectEncodedNewlinePattern = regexp.MustCompile(`%0[dD]|%0[aA]`)
)

// JSTrim mirrors ECMAScript String.prototype.trim (WhiteSpace + LineTerminator).
func JSTrim(value string) string {
	start := 0
	for start < len(value) {
		r, size := utf8.DecodeRuneInString(value[start:])
		if !jsIsWhitespace(r) {
			break
		}
		start += size
	}
	end := len(value)
	for end > start {
		r, size := utf8.DecodeLastRuneInString(value[start:end])
		if !jsIsWhitespace(r) {
			break
		}
		end -= size
	}
	return value[start:end]
}

// jsIsWhitespace is the ECMAScript WhiteSpace + LineTerminator predicate.
func jsIsWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0x1680,
		0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	// Zs category: U+2000..U+200A (the rest of Zs is listed above).
	return r >= 0x2000 && r <= 0x200A
}

// IsValidRedirectURL mirrors isValidRedirectUrl(): true only for a single-slash
// relative path with no scheme, backslash or control-character tricks.
func IsValidRedirectURL(url string) bool {
	trimmed := JSTrim(url)

	// An empty string is not a redirect target.
	if trimmed == "" {
		return false
	}

	// Must start with '/' (relative path).
	if !strings.HasPrefix(trimmed, "/") {
		return false
	}

	// '//' or '/\' is protocol-relative in every browser (backslash == slash).
	if len(trimmed) > 1 && (trimmed[1] == '/' || trimmed[1] == '\\') {
		return false
	}

	// No backslash anywhere: the WHATWG URL parser normalises it to '/'.
	if strings.ContainsRune(trimmed, '\\') {
		return false
	}

	lowerURL := strings.ToLower(trimmed)
	// Encoded backslash / encoded protocol-relative prefix.
	if strings.Contains(lowerURL, "%5c") ||
		strings.HasPrefix(lowerURL, "/%2f") ||
		strings.HasPrefix(lowerURL, "/%5c") {
		return false
	}

	// Scripting schemes, including variants embedded after a relative prefix.
	if strings.Contains(lowerURL, "javascript:") ||
		strings.Contains(lowerURL, "data:") ||
		strings.Contains(lowerURL, "vbscript:") {
		return false
	}

	// CR/LF/TAB (raw).
	if redirectControlCharsPattern.MatchString(trimmed) {
		return false
	}

	// Encoded CR/LF (%0d/%0D/%0a/%0A); %0b/%0c are not checked by the TypeScript.
	if redirectEncodedNewlinePattern.MatchString(trimmed) {
		return false
	}

	return true
}

// GetSafeRedirectURL mirrors getSafeRedirectUrl(url, defaultUrl): the trimmed URL
// when it passes validation, otherwise the caller's default. Callers must pass the
// default explicitly (every TypeScript call site does).
func GetSafeRedirectURL(url, defaultURL string) string {
	if IsValidRedirectURL(url) {
		return JSTrim(url)
	}
	return defaultURL
}
