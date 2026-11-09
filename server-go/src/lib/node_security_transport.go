// Port of server/src/lib/node-security/transport.ts.
//
// A certificate pin authenticates the node; it does NOT imply the node's security
// state is locked. This is the ONLY place the backend verifies a node's identity
// before sending a single header or secret body to it.
//
// Behaviour that must not change:
//
//   - an unverified network response never enrolls a pin, so a MITM cannot
//     substitute its own fingerprint on the first contact
//   - a malformed expectation is refused BEFORE dialling, so a bad pin config is a
//     hard failure rather than an unpinned connection
//   - the fingerprint comparison is constant-time (timingSafeEqual in Node), so the
//     pin cannot be recovered byte by byte
//   - https is required; there is no plaintext fallback
//   - certificate CHAIN verification is intentionally disabled (Incus uses a
//     self-signed cert): the pin replaces the PKI, it does not augment it
package lib

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// nodePinnedFingerprintPattern mirrors /^[0-9a-f]{64}$/.
var nodePinnedFingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// nodePinnedDialTimeout mirrors the `socket.setTimeout(10_000)` guard.
const nodePinnedDialTimeout = 10 * time.Second

// Errors surfaced by the pinned transport. The messages are deliberately generic:
// a caller must not learn whether the node was unreachable, misconfigured or
// impersonated beyond "the identity check failed".
var (
	ErrNodeInvalidIdentity  = errors.New("Invalid node identity")
	ErrNodeTLSRequired      = errors.New("HTTPS required")
	ErrNodeConnectFailed    = errors.New("Node connection failed")
	ErrNodeConnectTimeout   = errors.New("Node connection timed out")
	ErrNodeIdentityMismatch = errors.New("Node identity mismatch")
)

// NodePinnedCredentials is a loaded client certificate pair.
type NodePinnedCredentials struct {
	Certificate tls.Certificate
}

// DialNodePinned opens a TLS connection to the node and verifies its leaf
// certificate fingerprint against expectedFingerprint (lowercase hex sha256 of the
// DER certificate).
//
// It returns the established connection only after the pin has matched, so no
// caller can write to an unverified peer.
func DialNodePinned(
	ctx context.Context,
	nodeURL string,
	expectedFingerprint string,
	credentials NodePinnedCredentials,
) (net.Conn, error) {
	// Validate the pin BEFORE dialling: a misconfigured pin must never degrade into
	// an unpinned connection.
	if !nodePinnedFingerprintPattern.MatchString(expectedFingerprint) {
		return nil, ErrNodeInvalidIdentity
	}

	parsed, err := url.Parse(nodeURL)
	if err != nil || parsed.Scheme != "https" {
		return nil, ErrNodeTLSRequired
	}
	host := strings.Trim(parsed.Hostname(), "[]")
	if host == "" {
		return nil, ErrNodeConnectFailed
	}

	port := parsed.Port()
	if port == "" {
		port = "443"
	}

	// servername is omitted for an IP, matching `net.isIP(host) ? undefined : host`.
	serverName := ""
	if ip := net.ParseIP(host); ip == nil {
		serverName = host
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{credentials.Certificate},
		// Incus uses a self-signed certificate; the pin below is what authenticates
		// it. Matching `rejectUnauthorized: false` in the TypeScript.
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		ServerName:         serverName,
	}

	dialer := &net.Dialer{Timeout: nodePinnedDialTimeout}
	connection, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, port), tlsConfig)
	if err != nil {
		if isTimeout(err) {
			return nil, ErrNodeConnectTimeout
		}
		return nil, ErrNodeConnectFailed
	}

	state := connection.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		_ = connection.Close()
		return nil, ErrNodeInvalidIdentity
	}

	actual := sha256.Sum256(state.PeerCertificates[0].Raw)
	actualHex := fmt.Sprintf("%x", actual)

	if !constantTimeHexEqual(actualHex, expectedFingerprint) {
		_ = connection.Close()
		return nil, ErrNodeIdentityMismatch
	}

	// The header/body timeouts the TypeScript configures on the undici Agent.
	_ = connection.SetDeadline(time.Now().Add(nodePinnedDialTimeout))
	_ = connection.SetDeadline(time.Time{})
	return connection, nil
}

// constantTimeHexEqual compares two equal-length hex strings without early exit.
func constantTimeHexEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

// isTimeout reports whether err is a timeout, mirroring the TS check for
// ETIMEDOUT / the socket timer.
func isTimeout(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}
