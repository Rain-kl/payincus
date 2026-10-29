// Port of server/src/lib/outbound-security.ts.
//
// SSRF guard for every outbound request the backend makes on behalf of users
// (webhooks, storage targets, ...). It has two layers:
//
//  1. ResolvePublicHostname / AssertSafe*URL reject private, loopback,
//     link-local, ULA, multicast and otherwise reserved targets up front.
//  2. SafeOutboundHTTPClient re-validates the resolved addresses *at connect
//     time*, so a DNS rebinding between the up-front check and the actual
//     connection cannot reach an internal address (the Node original gets the
//     same property from undici's connect.lookup hook).
//
// The address classification is a line-by-line port of the TypeScript version
// (including its IPv6 string-prefix checks and its IPv4-mapped handling), so the
// two implementations accept and reject exactly the same inputs. See the tests in
// outbound_security_test.go for the pinned matrix.
package lib

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OutboundTargetValidationError mirrors the TypeScript error class of the same
// name. Callers use errors.As to tell "this target is not allowed" apart from a
// transport failure.
type OutboundTargetValidationError struct {
	Message string
}

func (e *OutboundTargetValidationError) Error() string { return e.Message }

func outboundValidationError(format string, args ...any) *OutboundTargetValidationError {
	return &OutboundTargetValidationError{Message: fmt.Sprintf(format, args...)}
}

// SupportedProtocol mirrors the TypeScript union 'http' | 'https' | 'ftp' | 'sftp'.
const (
	ProtocolHTTP  = "http"
	ProtocolHTTPS = "https"
	ProtocolFTP   = "ftp"
	ProtocolSFTP  = "sftp"
)

var (
	// ipv6BracketPattern mirrors /^\[|\]$/g in normalizeIpv6.
	ipv6BracketPattern = regexp.MustCompile(`^\[|\]$`)
	// linkLocalIPv6Pattern mirrors /^fe[89ab]/i.
	linkLocalIPv6Pattern = regexp.MustCompile(`^fe[89ab]`)
	// mappedIPv4Pattern mirrors /^::ffff:(\d+\.\d+\.\d+\.\d+)$/i.
	mappedIPv4Pattern = regexp.MustCompile(`^::ffff:(\d+\.\d+\.\d+\.\d+)$`)
)

// ipv4ReservedRange mirrors the ranges array in isIpPrivateOrReserved.
type ipv4ReservedRange struct {
	base   string
	prefix int
}

var ipv4ReservedRanges = []ipv4ReservedRange{
	{"0.0.0.0", 8},
	{"10.0.0.0", 8},
	{"100.64.0.0", 10},
	{"127.0.0.0", 8},
	{"169.254.0.0", 16},
	{"172.16.0.0", 12},
	{"192.0.0.0", 24},
	{"192.0.2.0", 24},
	{"192.168.0.0", 16},
	{"198.18.0.0", 15},
	{"198.51.100.0", 24},
	{"203.0.113.0", 24},
	{"224.0.0.0", 4},
	{"240.0.0.0", 4},
}

// buildOutboundURL mirrors buildUrl: it prepends the default protocol when the
// input carries no "://" and reports the TypeScript error strings verbatim.
func buildOutboundURL(input, defaultProtocol string) (*url.URL, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil, outboundValidationError("Target cannot be empty")
	}

	raw := trimmed
	if !strings.Contains(trimmed, "://") {
		raw = defaultProtocol + "://" + trimmed
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, outboundValidationError("Target format is invalid")
	}
	return parsed, nil
}

// ipv4ToUint32 mirrors ipv4ToInt. It returns ok=false for anything that is not a
// canonical dotted quad, which is what net.isIP rejects too.
func ipv4ToUint32(ip string) (uint32, bool) {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return 0, false
	}
	var result uint32
	for _, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || value > 255 {
			return 0, false
		}
		result = (result << 8) + uint32(value)
	}
	return result, true
}

// isIPv4InCIDR mirrors isIpv4InCidr.
func isIPv4InCIDR(ip, baseIP string, prefixLength int) bool {
	ipInt, ok := ipv4ToUint32(ip)
	if !ok {
		return false
	}
	baseInt, ok := ipv4ToUint32(baseIP)
	if !ok {
		return false
	}

	var mask uint32
	if prefixLength != 0 {
		mask = ^uint32(0) << uint(32-prefixLength)
	}
	return (ipInt & mask) == (baseInt & mask)
}

// isIPFamily mirrors net.isIP: 4 for an IPv4 literal, 6 for an IPv6 literal
// (including IPv4-mapped forms) and 0 for anything else.
func isIPFamily(ip string) int {
	if net.ParseIP(ip) == nil {
		return 0
	}
	if strings.Contains(ip, ":") {
		return 6
	}
	return 4
}

// normalizeIPv6 mirrors normalizeIpv6: lower-case and strip surrounding brackets.
func normalizeIPv6(ip string) string {
	return ipv6BracketPattern.ReplaceAllString(strings.ToLower(ip), "")
}

// isIPv6PrivateOrReserved mirrors isIpv6PrivateOrReserved. It deliberately works
// on the *textual* form (not a normalized net.IP) so that its prefix checks match
// the TypeScript original exactly.
func isIPv6PrivateOrReserved(ip string) bool {
	normalized := normalizeIPv6(ip)

	if normalized == "::" ||
		normalized == "::1" ||
		strings.HasPrefix(normalized, "fc") ||
		strings.HasPrefix(normalized, "fd") ||
		linkLocalIPv6Pattern.MatchString(normalized) ||
		strings.HasPrefix(normalized, "ff") ||
		strings.HasPrefix(normalized, "2001:db8") {
		return true
	}

	if match := mappedIPv4Pattern.FindStringSubmatch(normalized); match != nil {
		return IsIPPrivateOrReserved(match[1])
	}

	return false
}

// IsIPPrivateOrReserved mirrors isIpPrivateOrReserved. Unparseable input is
// treated as reserved (fail closed), matching the TypeScript `return true` tail.
func IsIPPrivateOrReserved(ip string) bool {
	switch isIPFamily(ip) {
	case 4:
		for _, r := range ipv4ReservedRanges {
			if isIPv4InCIDR(ip, r.base, r.prefix) {
				return true
			}
		}
		return false
	case 6:
		return isIPv6PrivateOrReserved(ip)
	default:
		return true
	}
}

// PublicResolvedAddress mirrors the TypeScript interface of the same name.
type PublicResolvedAddress struct {
	Address string `json:"address"`
	Family  int    `json:"family"`
}

// dnsErrorCode maps a Go resolver error onto the Node dns error code that the
// TypeScript implementation interpolates into the message.
func dnsErrorCode(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return "ENOTFOUND"
		case dnsErr.IsTimeout:
			return "ETIMEOUT"
		default:
			return "EAI_AGAIN"
		}
	}
	return "UNKNOWN"
}

// lookupHostAddresses resolves a hostname to PublicResolvedAddress records using
// the system resolver (equivalent to dns.lookup with {all:true, verbatim:true}).
func lookupHostAddresses(ctx context.Context, host string) ([]PublicResolvedAddress, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, outboundValidationError("Unable to resolve hostname (%s)", dnsErrorCode(err))
	}

	records := make([]PublicResolvedAddress, 0, len(addrs))
	for _, addr := range addrs {
		family := 6
		if addr.IP.To4() != nil {
			family = 4
		}
		records = append(records, PublicResolvedAddress{Address: addr.IP.String(), Family: family})
	}
	return records, nil
}

// ResolvePublicHostname mirrors resolvePublicHostname. Every resolved record must
// be a public address; a single private/reserved record rejects the hostname.
func ResolvePublicHostname(ctx context.Context, hostname string) ([]PublicResolvedAddress, error) {
	normalizedHost := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	if normalizedHost == "" {
		return nil, outboundValidationError("Hostname cannot be empty")
	}

	if normalizedHost == "localhost" ||
		strings.HasSuffix(normalizedHost, ".localhost") ||
		strings.HasSuffix(normalizedHost, ".local") ||
		strings.HasSuffix(normalizedHost, ".internal") {
		return nil, outboundValidationError("Private or local targets are not allowed")
	}

	if family := isIPFamily(normalizedHost); family != 0 {
		if IsIPPrivateOrReserved(normalizedHost) {
			return nil, outboundValidationError("Private or reserved IP targets are not allowed")
		}
		return []PublicResolvedAddress{{Address: normalizedHost, Family: family}}, nil
	}

	if !strings.Contains(normalizedHost, ".") {
		return nil, outboundValidationError("Private or local hostnames are not allowed")
	}

	records, err := lookupHostAddresses(ctx, normalizedHost)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, outboundValidationError("Unable to resolve hostname")
	}

	for _, record := range records {
		if IsIPPrivateOrReserved(record.Address) {
			return nil, outboundValidationError("Targets resolving to private or reserved IPs are not allowed")
		}
	}

	return records, nil
}

// AssertSafeHTTPURL mirrors assertSafeHttpUrl(url, label). An empty label uses the
// TypeScript default "URL".
func AssertSafeHTTPURL(ctx context.Context, rawURL, label string) (*url.URL, error) {
	if label == "" {
		label = "URL"
	}

	parsed, err := buildOutboundURL(rawURL, ProtocolHTTPS)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != ProtocolHTTP && parsed.Scheme != ProtocolHTTPS {
		return nil, outboundValidationError("%s must use http or https", label)
	}

	if _, err := ResolvePublicHostname(ctx, parsed.Hostname()); err != nil {
		return nil, err
	}
	return parsed, nil
}

// AssertSafeWebhookURL mirrors assertSafeWebhookUrl.
func AssertSafeWebhookURL(ctx context.Context, rawURL string) (*url.URL, error) {
	return AssertSafeHTTPURL(ctx, rawURL, "Webhook URL")
}

// AssertSafeStorageTarget mirrors assertSafeStorageTarget. Unknown target types
// fall through to the same "Target format is invalid" error the TypeScript build
// would produce for an unmapped protocol.
func AssertSafeStorageTarget(ctx context.Context, targetType, host string) error {
	defaultProtocol := ""
	switch targetType {
	case "WEBDAV":
		defaultProtocol = ProtocolHTTPS
	case "FTP":
		defaultProtocol = ProtocolFTP
	case "SFTP":
		defaultProtocol = ProtocolSFTP
	case "S3":
		defaultProtocol = ProtocolHTTPS
	}

	parsed, err := buildOutboundURL(host, defaultProtocol)
	if err != nil {
		return err
	}
	_, err = ResolvePublicHostname(ctx, parsed.Hostname())
	return err
}

// revalidatingLookupAddresses mirrors revalidatingLookup: resolve the hostname and
// reject it if any record is private or reserved. It runs at connect time so a
// rebinding between validation and connection is still caught.
func revalidatingLookupAddresses(ctx context.Context, host string) ([]net.IPAddr, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, outboundValidationError("Unable to resolve hostname")
	}
	for _, addr := range addrs {
		if IsIPPrivateOrReserved(addr.IP.String()) {
			return nil, outboundValidationError("Target resolved to a private or reserved IP")
		}
	}
	return addrs, nil
}

// safeOutboundDialContext is the Go equivalent of undici's connect.lookup hook: it
// resolves the hostname itself, validates every address and only then dials a
// validated IP. TLS still uses the original hostname for SNI (http.Transport sets
// ServerName from the request URL), so certificate verification is unaffected.
func safeOutboundDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	dialer := &net.Dialer{}

	if net.ParseIP(host) != nil {
		if IsIPPrivateOrReserved(host) {
			return nil, outboundValidationError("Target resolved to a private or reserved IP")
		}
		return dialer.DialContext(ctx, network, addr)
	}

	addrs, err := revalidatingLookupAddresses(ctx, host)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, ipAddr := range addrs {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	if lastErr == nil {
		lastErr = outboundValidationError("Unable to resolve hostname")
	}
	return nil, lastErr
}

// safeOutboundHTTPClient is the shared client used by SafeFetch. It mirrors
// safeOutboundDispatcher; callers must not mutate it.
var safeOutboundHTTPClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           safeOutboundDialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	},
}

// SafeOutboundHTTPClient exposes the connect-time-revalidating client. It is the
// Go equivalent of the exported safeOutboundDispatcher.
func SafeOutboundHTTPClient() *http.Client { return safeOutboundHTTPClient }

// SafeFetchInit mirrors the subset of RequestInit that safeFetch accepts.
type SafeFetchInit struct {
	Method  string
	Headers http.Header
	Body    io.Reader
}

// SafeFetch mirrors safeFetch(url, init, label): it validates the target, then
// issues the request through the revalidating transport.
func SafeFetch(ctx context.Context, rawURL string, init *SafeFetchInit, label string) (*http.Response, error) {
	if _, err := AssertSafeHTTPURL(ctx, rawURL, label); err != nil {
		return nil, err
	}

	method := http.MethodGet
	var body io.Reader
	var headers http.Header
	if init != nil {
		if init.Method != "" {
			method = init.Method
		}
		body = init.Body
		headers = init.Headers
	}

	request, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}

	return safeOutboundHTTPClient.Do(request)
}
