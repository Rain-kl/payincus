// Port of server/src/lib/network-address.ts.
//
// Small but load-bearing helpers: they decide how a host address is rendered inside a
// URL (an IPv6 literal needs brackets), what DNS record type a domain gets, and which
// local address a listener should bind to. A mistake here produces a config that
// parses on the panel and fails on the node.
package lib

import (
	"net/netip"
	"net/url"
	"strings"
)

// DNSRecordType mirrors the DnsRecordType union.
type DNSRecordType string

const (
	DNSRecordTypeA     DNSRecordType = "A"
	DNSRecordTypeAAAA  DNSRecordType = "AAAA"
	DNSRecordTypeCNAME DNSRecordType = "CNAME"
)

// NormalizeNetworkHost mirrors normalizeNetworkHost: trim, and unwrap a bracketed
// IPv6 literal. The bracket is only removed when the inner value actually looks like
// IPv6, so "[weird]" stays "[weird]" rather than becoming something else.
func NormalizeNetworkHost(value string) string {
	trimmed := strings.TrimSpace(value)

	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		if strings.ContainsRune(inner, ':') {
			return inner
		}
	}

	return trimmed
}

// GetAddressFamily mirrors getAddressFamily: 4, 6, or 0 for "not a literal".
//
// Three details were measured against node's net.isIP and must be preserved:
//
//   - a ZONE INDEX is accepted: "fe80::1%eth0" is family 6 (net.ParseIP would return
//     nil here, so netip is used instead)
//   - an IPv4-MAPPED IPv6 address is family 6: "::ffff:127.0.0.1" → 6, even though
//     net.IP.To4() reports it as v4. Using To4() would silently misclassify it and
//     then decide bind/DNS behaviour from the wrong family.
//   - a prefix length is NOT accepted: "10.0.0.0/8" → 0
func GetAddressFamily(value string) int {
	normalized := NormalizeNetworkHost(value)
	if normalized == "" {
		return 0
	}
	// A CIDR is not a literal; netip.ParseAddr would reject it anyway, but the check
	// documents the intent.
	if strings.ContainsRune(normalized, '/') {
		return 0
	}

	parsed, err := netip.ParseAddr(normalized)
	if err != nil {
		return 0
	}
	if parsed.Is4() {
		return 4
	}
	if parsed.Is6() {
		// Is6() is true for IPv4-mapped addresses too, which is what node reports.
		return 6
	}
	return 0
}

// IsIPLiteral mirrors isIpLiteral.
func IsIPLiteral(value string) bool {
	return GetAddressFamily(value) != 0
}

// FormatHostForURL mirrors formatHostForUrl: bracket an IPv6 literal.
func FormatHostForURL(value string) string {
	normalized := NormalizeNetworkHost(value)
	if GetAddressFamily(normalized) == 6 {
		return "[" + normalized + "]"
	}
	return normalized
}

// FormatHostForListen mirrors formatHostForListen, which is an alias.
func FormatHostForListen(value string) string {
	return FormatHostForURL(value)
}

// ExtractHostAddressFromURL mirrors extractHostAddressFromUrl: the hostname of a URL,
// normalised, or nil when the value is empty or unparseable.
func ExtractHostAddressFromURL(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}

	parsed, err := url.Parse(*value)
	if err != nil || parsed.Hostname() == "" {
		return nil
	}
	normalized := NormalizeNetworkHost(parsed.Hostname())
	return &normalized
}

// SelectBindableIpv4ListenAddress mirrors selectBindableIpv4ListenAddress.
//
// The rule is deliberately conservative: a listener binds to a specific address ONLY
// when the operator set an explicit IPv4 bind address, or when the public address the
// instance advertises is genuinely local to this host. Everything else binds 0.0.0.0,
// because binding to an address the node does not own makes the listener unreachable.
//
// The hostURL parameter is unused in the TS as well (its name is prefixed with an
// underscore); it is kept in the signature for call-site compatibility.
func SelectBindableIpv4ListenAddress(
	explicitBindIpv4 *string,
	fallbackPublicIpv4 *string,
	hostURL *string,
	hostIPAddress *string,
) string {
	explicit := ""
	if explicitBindIpv4 != nil && *explicitBindIpv4 != "" {
		explicit = NormalizeNetworkHost(*explicitBindIpv4)
	}
	if GetAddressFamily(explicit) == 4 {
		return explicit
	}

	localCandidates := map[string]bool{}
	var normalizedHostIP *string
	if hostIPAddress != nil && *hostIPAddress != "" {
		normalized := NormalizeNetworkHost(*hostIPAddress)
		normalizedHostIP = &normalized
	}

	fallback := ""
	if fallbackPublicIpv4 != nil && *fallbackPublicIpv4 != "" {
		fallback = NormalizeNetworkHost(*fallbackPublicIpv4)
	}
	if GetAddressFamily(fallback) != 4 {
		return "0.0.0.0"
	}

	if normalizedHostIP != nil && GetAddressFamily(*normalizedHostIP) == 4 {
		localCandidates[*normalizedHostIP] = true
	}

	if localCandidates[fallback] {
		return fallback
	}
	return "0.0.0.0"
}

// IsIpv6Address mirrors isIpv6Address. Used when pairing a listen address with a
// connect address: a v6 listen with a v4 connect must disable kernel NAT.
func IsIpv6Address(value string) bool {
	return GetAddressFamily(value) == 6
}

// GetDNSRecordType mirrors getDnsRecordType: A for IPv4, AAAA for IPv6, CNAME for a
// hostname.
func GetDNSRecordType(value string) DNSRecordType {
	switch GetAddressFamily(value) {
	case 4:
		return DNSRecordTypeA
	case 6:
		return DNSRecordTypeAAAA
	default:
		return DNSRecordTypeCNAME
	}
}
