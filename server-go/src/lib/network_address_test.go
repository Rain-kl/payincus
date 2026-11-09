package lib

import "testing"

// TestGetAddressFamilyMatchesNode pins the family detection against values measured
// with node's net.isIP. The two that bite are a zone index (accepted, family 6) and an
// IPv4-mapped address (family 6, NOT 4).
func TestGetAddressFamilyMatchesNode(t *testing.T) {
	cases := map[string]int{
		"127.0.0.1":        4,
		"0.0.0.0":          4,
		"255.255.255.255":  4,
		"::1":              6,
		"[::1]":            6,
		"2001:db8::1":      6,
		"[fe80::1%eth0]":   6, // zone index is accepted by net.isIP
		"fe80::1%eth0":     6,
		"::ffff:127.0.0.1": 6, // IPv4-mapped is family 6, not 4
		"10.0.0.0/8":       0,
		"":                 0,
		"weird":            0,
		"[weird]":          0,
		"256.0.0.1":        0,
	}
	for input, want := range cases {
		if got := GetAddressFamily(input); got != want {
			t.Errorf("GetAddressFamily(%q) = %d, want %d", input, got, want)
		}
	}
}

// TestNormalizeNetworkHostMatchesNode pins the bracket unwrapping, including the case
// where the brackets are NOT removed because the content is not IPv6.
func TestNormalizeNetworkHostMatchesNode(t *testing.T) {
	cases := map[string]string{
		"  127.0.0.1  ":  "127.0.0.1",
		"[::1]":          "::1",
		"[weird]":        "[weird]",
		"weird":          "weird",
		"[fe80::1%eth0]": "fe80::1%eth0",
		"[2001:db8::1]":  "2001:db8::1",
		"[  ::1  ]":      "::1",
	}
	for input, want := range cases {
		if got := NormalizeNetworkHost(input); got != want {
			t.Errorf("NormalizeNetworkHost(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestFormatHostForURLBracketsIPv6 pins the bracket rule.
func TestFormatHostForURLBracketsIPv6(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1":     "127.0.0.1",
		"::1":           "[::1]",
		"2001:db8::1":   "[2001:db8::1]",
		"example.com":   "example.com",
		"[2001:db8::1]": "[2001:db8::1]",
	}
	for input, want := range cases {
		if got := FormatHostForURL(input); got != want {
			t.Errorf("FormatHostForURL(%q) = %q, want %q", input, got, want)
		}
	}
	if FormatHostForListen("::1") != "[::1]" {
		t.Fatal("FormatHostForListen must be an alias of FormatHostForURL")
	}
}

// TestExtractHostAddressFromURL pins the URL hostname extraction.
func TestExtractHostAddressFromURL(t *testing.T) {
	if got := ExtractHostAddressFromURL(nil); got != nil {
		t.Fatal("nil must yield nil")
	}
	empty := ""
	if got := ExtractHostAddressFromURL(&empty); got != nil {
		t.Fatal("empty must yield nil")
	}

	value := "https://[2001:db8::1]:8443/x"
	got := ExtractHostAddressFromURL(&value)
	if got == nil || *got != "2001:db8::1" {
		t.Fatalf("bracketed ipv6 = %v", got)
	}

	host := "https://example.com:8443/x"
	got = ExtractHostAddressFromURL(&host)
	if got == nil || *got != "example.com" {
		t.Fatalf("hostname = %v", got)
	}

	bad := "not a url"
	if got := ExtractHostAddressFromURL(&bad); got != nil {
		t.Fatalf("unparseable must yield nil, got %v", *got)
	}
}

// TestSelectBindableIpv4ListenAddress pins the conservative bind rule.
func TestSelectBindableIpv4ListenAddress(t *testing.T) {
	strPtr := func(value string) *string { return &value }

	// An explicit IPv4 bind wins.
	if got := SelectBindableIpv4ListenAddress(strPtr("192.168.1.10"), strPtr("203.0.113.5"), nil, nil); got != "192.168.1.10" {
		t.Fatalf("explicit bind = %q", got)
	}

	// No fallback IPv4 → wildcard.
	if got := SelectBindableIpv4ListenAddress(nil, nil, nil, nil); got != "0.0.0.0" {
		t.Fatalf("no fallback = %q", got)
	}
	if got := SelectBindableIpv4ListenAddress(nil, strPtr("2001:db8::1"), nil, nil); got != "0.0.0.0" {
		t.Fatalf("ipv6 fallback = %q", got)
	}

	// Fallback is local → bind it.
	if got := SelectBindableIpv4ListenAddress(nil, strPtr("203.0.113.5"), nil, strPtr("203.0.113.5")); got != "203.0.113.5" {
		t.Fatalf("local fallback = %q", got)
	}

	// Fallback is NOT local → wildcard, because binding a foreign address makes the
	// listener unreachable.
	if got := SelectBindableIpv4ListenAddress(nil, strPtr("203.0.113.5"), nil, strPtr("10.0.0.1")); got != "0.0.0.0" {
		t.Fatalf("foreign fallback = %q", got)
	}

	// An IPv6 explicit bind is not a usable v4 listen address.
	if got := SelectBindableIpv4ListenAddress(strPtr("2001:db8::1"), strPtr("203.0.113.5"), nil, strPtr("203.0.113.5")); got != "203.0.113.5" {
		t.Fatalf("ipv6 explicit bind = %q, want the local fallback", got)
	}
}

// TestGetDNSRecordType pins the A/AAAA/CNAME mapping.
func TestGetDNSRecordType(t *testing.T) {
	cases := map[string]DNSRecordType{
		"203.0.113.5": DNSRecordTypeA,
		"2001:db8::1": DNSRecordTypeAAAA,
		"example.com": DNSRecordTypeCNAME,
		"":            DNSRecordTypeCNAME,
	}
	for input, want := range cases {
		if got := GetDNSRecordType(input); got != want {
			t.Errorf("GetDNSRecordType(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestIsIpv6Address pins the NAT pairing check.
func TestIsIpv6Address(t *testing.T) {
	if !IsIpv6Address("2001:db8::1") {
		t.Fatal("ipv6 literal must be reported as v6")
	}
	if IsIpv6Address("10.0.0.1") {
		t.Fatal("ipv4 must not be reported as v6")
	}
	if IsIpv6Address("example.com") {
		t.Fatal("a hostname is not v6")
	}
}
