// Unit tests for the outbound-security (SSRF guard) port. They live in package
// lib so they can exercise the unexported helpers directly.
package lib

import (
	"context"
	"errors"
	"testing"
)

// TestIsIPPrivateOrReserved pins the address-classification matrix. Every entry is
// a decision the TypeScript original makes, so a drift here is a security
// regression.
func TestIsIPPrivateOrReserved(t *testing.T) {
	cases := []struct {
		ip      string
		private bool
		why     string
	}{
		// Public addresses must be allowed.
		{"8.8.8.8", false, "public IPv4 (Google DNS)"},
		{"1.1.1.1", false, "public IPv4 (Cloudflare DNS)"},
		{"172.32.0.1", false, "just above the 172.16/12 private block"},
		{"198.20.0.1", false, "just above 198.18/15"},
		{"2606:4700:4700::1111", false, "public IPv6"},
		{"2001:db9::1", false, "just outside 2001:db8::/32"},

		// IPv4 private / reserved ranges.
		{"0.0.0.0", true, "0.0.0.0/8 this-network"},
		{"0.255.255.255", true, "0.0.0.0/8 upper bound"},
		{"10.0.0.1", true, "10.0.0.0/8"},
		{"10.255.255.255", true, "10.0.0.0/8 upper bound"},
		{"100.64.0.1", true, "100.64.0.0/10 CGNAT"},
		{"100.127.255.255", true, "100.64.0.0/10 upper bound"},
		{"100.128.0.1", false, "just above CGNAT"},
		{"127.0.0.1", true, "127.0.0.0/8 loopback"},
		{"127.255.255.254", true, "127.0.0.0/8 upper bound"},
		{"169.254.169.254", true, "169.254.0.0/16 link-local (cloud metadata)"},
		{"172.16.0.1", true, "172.16.0.0/12 lower bound"},
		{"172.31.255.255", true, "172.16.0.0/12 upper bound"},
		{"192.0.0.1", true, "192.0.0.0/24 IETF protocol assignments"},
		{"192.0.2.1", true, "192.0.2.0/24 TEST-NET-1"},
		{"192.168.1.1", true, "192.168.0.0/16"},
		{"198.18.0.1", true, "198.18.0.0/15 benchmarking"},
		{"198.51.100.1", true, "198.51.100.0/24 TEST-NET-2"},
		{"203.0.113.1", true, "203.0.113.0/24 TEST-NET-3"},
		{"224.0.0.1", true, "224.0.0.0/4 multicast"},
		{"239.255.255.255", true, "224.0.0.0/4 upper bound"},
		{"240.0.0.1", true, "240.0.0.0/4 reserved"},
		{"255.255.255.255", true, "240.0.0.0/4 upper bound (broadcast)"},

		// IPv6 special ranges.
		{"::", true, "unspecified address"},
		{"::1", true, "loopback"},
		{"fc00::1", true, "ULA fc00::/7"},
		{"fd12:3456::1", true, "ULA fd00::/8"},
		{"fe80::1", true, "link-local fe80::/10"},
		{"febf::1", true, "link-local upper bound"},
		{"fec0::1", false, "site-local (deprecated) is not in the TS list"},
		{"ff02::1", true, "multicast ff00::/8"},
		{"2001:db8::1", true, "documentation 2001:db8::/32"},
		{"2001:DB8::1", true, "documentation range is case-insensitive"},

		// IPv4-mapped IPv6 (dotted form, which is what the TS regex matches).
		{"::ffff:127.0.0.1", true, "IPv4-mapped loopback"},
		{"::ffff:10.0.0.1", true, "IPv4-mapped private"},
		{"::ffff:8.8.8.8", false, "IPv4-mapped public"},
		{"[::1]", true, "bracketed loopback"},

		// Fail closed: anything not a valid literal is reserved.
		{"", true, "empty string"},
		{"not-an-ip", true, "garbage"},
		{"999.999.999.999", true, "out-of-range octets"},
		{"1.2.3", true, "incomplete IPv4"},
	}

	for _, tc := range cases {
		if got := IsIPPrivateOrReserved(tc.ip); got != tc.private {
			t.Errorf("IsIPPrivateOrReserved(%q) = %v, want %v (%s)", tc.ip, got, tc.private, tc.why)
		}
	}
}

// TestIsIPPrivateOrReservedIPv4MappedHexForm documents a deliberate fidelity
// choice: the TypeScript regex only matches the dotted "::ffff:a.b.c.d" form, so a
// hex-encoded IPv4-mapped address is NOT recognised as private. The Go port keeps
// the same behaviour so the two implementations agree; fixing it is a TypeScript
// change that must happen in both places at once.
func TestIsIPPrivateOrReservedIPv4MappedHexForm(t *testing.T) {
	if IsIPPrivateOrReserved("::ffff:7f00:1") {
		t.Fatal("hex-form IPv4-mapped loopback now flagged private; TS and Go must be updated together")
	}
}

func TestIPv4ToUint32(t *testing.T) {
	cases := []struct {
		ip   string
		want uint32
		ok   bool
	}{
		{"0.0.0.0", 0, true},
		{"255.255.255.255", 0xffffffff, true},
		{"1.2.3.4", 0x01020304, true},
		{"1.2.3", 0, false},
		{"1.2.3.256", 0, false},
		{"a.b.c.d", 0, false},
	}
	for _, tc := range cases {
		got, ok := ipv4ToUint32(tc.ip)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("ipv4ToUint32(%q) = (%d, %v), want (%d, %v)", tc.ip, got, ok, tc.want, tc.ok)
		}
	}
}

func TestIsIPv4InCIDR(t *testing.T) {
	cases := []struct {
		ip     string
		base   string
		prefix int
		want   bool
	}{
		{"192.168.5.5", "192.168.0.0", 16, true},
		{"192.169.0.1", "192.168.0.0", 16, false},
		{"10.1.2.3", "10.0.0.0", 8, true},
		{"11.0.0.1", "10.0.0.0", 8, false},
		{"8.8.8.8", "0.0.0.0", 0, true},   // prefix 0 matches everything
		{"8.8.8.8", "8.8.8.8", 32, true},  // exact host match
		{"8.8.8.9", "8.8.8.8", 32, false}, // exact host mismatch
	}
	for _, tc := range cases {
		if got := isIPv4InCIDR(tc.ip, tc.base, tc.prefix); got != tc.want {
			t.Errorf("isIPv4InCIDR(%q, %q, %d) = %v, want %v", tc.ip, tc.base, tc.prefix, got, tc.want)
		}
	}
}

func TestIsIPFamily(t *testing.T) {
	cases := []struct {
		ip   string
		want int
	}{
		{"1.2.3.4", 4},
		{"::1", 6},
		{"::ffff:1.2.3.4", 6},
		{"[::1]", 0},
		{"1.2.3", 0},
		{"", 0},
		{"host.example.com", 0},
	}
	for _, tc := range cases {
		if got := isIPFamily(tc.ip); got != tc.want {
			t.Errorf("isIPFamily(%q) = %d, want %d", tc.ip, got, tc.want)
		}
	}
}

func TestBuildOutboundURL(t *testing.T) {
	if _, err := buildOutboundURL("", ProtocolHTTPS); err == nil {
		t.Error("empty target should be rejected")
	}
	if _, err := buildOutboundURL("   ", ProtocolHTTPS); err == nil {
		t.Error("whitespace-only target should be rejected")
	}

	parsed, err := buildOutboundURL("example.com/path", ProtocolHTTPS)
	if err != nil {
		t.Fatalf("buildOutboundURL: %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "example.com" || parsed.Path != "/path" {
		t.Errorf("buildOutboundURL = %s, want https://example.com/path", parsed)
	}

	// An explicit scheme wins over the default.
	parsed, err = buildOutboundURL("ftp://example.com/dir", ProtocolHTTPS)
	if err != nil {
		t.Fatalf("buildOutboundURL: %v", err)
	}
	if parsed.Scheme != "ftp" {
		t.Errorf("scheme = %q, want ftp", parsed.Scheme)
	}

	// Scheme is lower-cased like the WHATWG URL parser.
	parsed, err = buildOutboundURL("HTTPS://example.com", ProtocolHTTPS)
	if err != nil {
		t.Fatalf("buildOutboundURL: %v", err)
	}
	if parsed.Scheme != "https" {
		t.Errorf("scheme = %q, want https", parsed.Scheme)
	}

	for _, invalid := range []string{"http://", "https://", "://example.com"} {
		if _, err := buildOutboundURL(invalid, ProtocolHTTPS); err == nil {
			t.Errorf("buildOutboundURL(%q) should be invalid", invalid)
		}
	}
}

// TestResolvePublicHostnameRejectsLocalHosts covers the hostname checks that do not
// require DNS.
func TestResolvePublicHostnameRejectsLocalHosts(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		host string
	}{
		{"localhost"},
		{"LOCALHOST"},
		{"foo.localhost"},
		{"printer.local"},
		{"service.internal"},
		{"localhost."},   // trailing dot is stripped before the check
		{"   "},          // empty after trimming
		{"intranethost"}, // no dot -> private/local hostname
		{"127.0.0.1"},
		{"10.1.2.3"},
		{"169.254.169.254"},
		{"::1"},
		{"fd00::1"},
		{"fe80::1"},
		{"2001:db8::1"},
	}
	for _, tc := range cases {
		if _, err := ResolvePublicHostname(ctx, tc.host); err == nil {
			t.Errorf("ResolvePublicHostname(%q) should be rejected", tc.host)
		}
	}
}

// TestResolvePublicHostnameAllowsPublicIPLiteral covers the IP-literal fast path
// (no DNS involved).
func TestResolvePublicHostnameAllowsPublicIPLiteral(t *testing.T) {
	ctx := context.Background()

	records, err := ResolvePublicHostname(ctx, "8.8.8.8")
	if err != nil {
		t.Fatalf("ResolvePublicHostname(8.8.8.8): %v", err)
	}
	if len(records) != 1 || records[0].Address != "8.8.8.8" || records[0].Family != 4 {
		t.Fatalf("records = %+v, want one IPv4 8.8.8.8", records)
	}

	records, err = ResolvePublicHostname(ctx, "2606:4700:4700::1111")
	if err != nil {
		t.Fatalf("ResolvePublicHostname(public IPv6): %v", err)
	}
	if len(records) != 1 || records[0].Family != 6 {
		t.Fatalf("records = %+v, want one IPv6 record", records)
	}
}

// TestAssertSafeHTTPURLPortAndScheme exercises the scheme and port handling without
// touching DNS (IP-literal hosts only).
func TestAssertSafeHTTPURLPortAndScheme(t *testing.T) {
	ctx := context.Background()

	if _, err := AssertSafeHTTPURL(ctx, "https://8.8.8.8:8443/hook", "Hook"); err != nil {
		t.Fatalf("public IP literal with port should be allowed: %v", err)
	}

	// Non-http(s) schemes are rejected before any resolution.
	for _, raw := range []string{"ftp://8.8.8.8/x", "file:///etc/passwd", "gopher://8.8.8.8/"} {
		_, err := AssertSafeHTTPURL(ctx, raw, "Hook")
		if err == nil {
			t.Fatalf("AssertSafeHTTPURL(%q) should reject non-http(s) scheme", raw)
		}
		var validation *OutboundTargetValidationError
		if !errors.As(err, &validation) {
			t.Errorf("error type = %T, want *OutboundTargetValidationError", err)
		}
	}

	// Private IP literal with a port.
	if _, err := AssertSafeHTTPURL(ctx, "http://127.0.0.1:8080/x", "Hook"); err == nil {
		t.Error("loopback target should be rejected")
	}

	// The label is interpolated into the message verbatim.
	_, err := AssertSafeHTTPURL(ctx, "ftp://8.8.8.8/x", "Webhook URL")
	if err == nil || err.Error() != "Webhook URL must use http or https" {
		t.Errorf("error = %v, want \"Webhook URL must use http or https\"", err)
	}
}

func TestAssertSafeWebhookURLDefaultsLabel(t *testing.T) {
	_, err := AssertSafeWebhookURL(context.Background(), "ftp://8.8.8.8/x")
	if err == nil || err.Error() != "Webhook URL must use http or https" {
		t.Errorf("error = %v, want the Webhook URL label", err)
	}
}

func TestAssertSafeStorageTarget(t *testing.T) {
	ctx := context.Background()

	// Public IP literals pass for every supported type.
	if err := AssertSafeStorageTarget(ctx, "WEBDAV", "8.8.8.8/dav"); err != nil {
		t.Errorf("WEBDAV public target: %v", err)
	}
	if err := AssertSafeStorageTarget(ctx, "FTP", "8.8.8.8"); err != nil {
		t.Errorf("FTP public target: %v", err)
	}
	if err := AssertSafeStorageTarget(ctx, "SFTP", "8.8.8.8"); err != nil {
		t.Errorf("SFTP public target: %v", err)
	}
	// S3 defaults to https and, for a public IP literal, passes without DNS.
	if err := AssertSafeStorageTarget(ctx, "S3", "https://8.8.8.8/bucket"); err != nil {
		t.Errorf("S3 public target: %v", err)
	}

	// Private targets are rejected regardless of type.
	for _, targetType := range []string{"WEBDAV", "FTP", "SFTP", "S3"} {
		if err := AssertSafeStorageTarget(ctx, targetType, "127.0.0.1"); err == nil {
			t.Errorf("%s loopback target should be rejected", targetType)
		}
	}

	// Unknown type falls back to the "invalid target format" path.
	if err := AssertSafeStorageTarget(ctx, "NFS", "8.8.8.8"); err == nil {
		t.Error("unknown storage target type should be rejected")
	}
}

func TestRevalidatingDialContextRejectsPrivateLiteral(t *testing.T) {
	ctx := context.Background()
	if _, err := safeOutboundDialContext(ctx, "tcp", "127.0.0.1:80"); err == nil {
		t.Fatal("dialing a loopback IP literal must be refused")
	}
}

func TestSafeOutboundHTTPClientIsStable(t *testing.T) {
	if SafeOutboundHTTPClient() == nil {
		t.Fatal("SafeOutboundHTTPClient must not be nil")
	}
	if SafeOutboundHTTPClient() != SafeOutboundHTTPClient() {
		t.Fatal("SafeOutboundHTTPClient must return the shared client")
	}
}

func TestSafeFetchRejectsPrivateTarget(t *testing.T) {
	ctx := context.Background()
	if _, err := SafeFetch(ctx, "http://127.0.0.1/hook", nil, "Hook"); err == nil {
		t.Fatal("SafeFetch must reject a loopback target before dialing")
	}
}

func TestOutboundValidationErrorIsError(t *testing.T) {
	var err error = outboundValidationError("boom %d", 1)
	if err.Error() != "boom 1" {
		t.Fatalf("Error() = %q, want boom 1", err.Error())
	}
	if !errors.Is(err, err) {
		t.Fatal("a validation error must be usable with errors.Is on itself")
	}
}
