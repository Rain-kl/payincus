package db

import "testing"

// TestProxySitesIsValidDomain pins the security boundary. The accept/reject sets
// come from the TypeScript regex
//
//	/^(?!-)[A-Za-z0-9-]{1,63}(?<!-)(\.[A-Za-z0-9-]{1,63})*\.[A-Za-z]{2,}$/
//
// plus the explicit "no wildcard" guard. The cases marked QUIRK document behaviour
// the regex has but nobody intended; they are asserted so a later "cleanup" cannot
// silently change what the API accepts.
func TestProxySitesIsValidDomain(t *testing.T) {
	valid := []string{
		"example.com",
		"example.co",
		"sub.example.com",
		"deep.sub.example.com",  // middle labels are free-form
		"a-b.example.co",        // hyphen inside a label
		"EXAMPLE.COM",           // the regex is case-insensitive per-letter
		"1example.com",          // leading digit is fine
		"example1.com",          // trailing digit is fine
		"a.-b.co",               // QUIRK: a middle label may start with '-'
		"a.b-.co",               // QUIRK: a middle label may end with '-'
		"xn--bcher-kva.example", // punycode label
		"example.technology",    // long TLD
		"a.bc",                  // two-label minimum, 2-letter TLD
		// NOTE: .local / .internal pass the domain grammar. They are only refused
		// later by resolvePublicHostname (the certificate probe). Kept here so the
		// parity contract cannot drift into "Go is stricter than Node".
		"foo.local",
		"foo.internal",
		"a" + repeatString("b", 62) + ".com", // 63-char first label is the maximum
		"example.c" + repeatString("o", 100),
	}
	for _, domain := range valid {
		if !ProxySitesIsValidDomain(domain) {
			t.Fatalf("ProxySitesIsValidDomain(%q) = false, want true", domain)
		}
	}

	invalid := []string{
		// Wildcards must never reach Caddy.
		"*.example.com",
		"*",
		"*.",
		// Internal / literal targets must not become routes.
		"localhost",
		"localhost.",
		"127.0.0.1",
		"0.0.0.0",
		"[::1]",
		"::1",
		"internal",
		"http://example.com",
		"user@example.com",
		"example.com/path",
		"example.com:443",
		" example.com",
		"example.com ",
		"",
		".",
		"com",
		// Structural violations.
		"a..com",
		".example.com",
		"example.com.",
		"-example.com",
		"example-.com",
		"exa_mple.com",
		"exa mple.com",
		"example.c",                    // TLD needs two letters
		"example.c0",                   // TLD is letters only
		"example.123",                  // TLD is letters only
		"a.b.c",                        // last label is one letter
		repeatString("b", 64) + ".com", // first label may not exceed 63
		"例え.com",                       // non-ASCII is not in [A-Za-z0-9-]
		"example。com",                  // ideographic full stop is not a separator
	}
	for _, domain := range invalid {
		if ProxySitesIsValidDomain(domain) {
			t.Fatalf("ProxySitesIsValidDomain(%q) = true, want false", domain)
		}
	}
}

func repeatString(value string, count int) string {
	result := make([]byte, 0, len(value)*count)
	for index := 0; index < count; index++ {
		result = append(result, value...)
	}
	return string(result)
}

// TestProxySitesIsIPAddress pins isIpAddress(), which is isIpLiteral(): a zone
// index is accepted, a prefix length is not, and a bracketed IPv6 literal is
// unwrapped first.
func TestProxySitesIsIPAddress(t *testing.T) {
	// isIpLiteral normalises first (trim + bracket unwrap), so surrounding spaces
	// are tolerated exactly like normalizeNetworkHost in network-address.ts.
	valid := []string{"1.2.3.4", "0.0.0.0", "::1", "2001:db8::1", "fe80::1%eth0", "[::1]", "::ffff:127.0.0.1", " 1.2.3.4 "}
	for _, value := range valid {
		if !ProxySitesIsIPAddress(value) {
			t.Fatalf("ProxySitesIsIPAddress(%q) = false, want true", value)
		}
	}

	invalid := []string{"example.com", "10.0.0.0/8", "", "1.2.3", "999.1.1.1", "[weird]"}
	for _, value := range invalid {
		if ProxySitesIsIPAddress(value) {
			t.Fatalf("ProxySitesIsIPAddress(%q) = true, want false", value)
		}
	}
}
