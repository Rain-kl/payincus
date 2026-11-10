package lib

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

// testVaultSecret is a >=32 char secret so nodeStorageMaster accepts it.
const testVaultSecret = "test-encryption-key-0123456789abcdef"

func randomKeyHex(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(raw)
}

// TestNodeStorageVaultRoundTrip pins the seal/unseal contract.
func TestNodeStorageVaultRoundTrip(t *testing.T) {
	key := randomKeyHex(t)
	identity := NodeStorageKeyIdentity{HostID: 1, Dataset: "tank/instances/vm-1", Version: 1}

	envelope, err := SealNodeStorageKeyWithSecret(key, identity, testVaultSecret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	parts := strings.Split(envelope, ":")
	if len(parts) != 4 || parts[0] != "v1" {
		t.Fatalf("envelope shape = %q", envelope)
	}
	if len(parts[1]) != 24 || len(parts[2]) != 32 || len(parts[3]) != 64 {
		t.Fatalf("envelope field lengths = %d/%d/%d, want 24/32/64",
			len(parts[1]), len(parts[2]), len(parts[3]))
	}

	recovered, err := UnsealNodeStorageKeyWithSecret(envelope, identity, testVaultSecret)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	if recovered != key {
		t.Fatalf("recovered key mismatch")
	}
}

// TestNodeStorageVaultBindsIdentity pins that the identity triple is AES-GCM
// additional data: a sealed key copied to another host, dataset or version must NOT
// decrypt, because that would mean the wrong volume gets the key.
func TestNodeStorageVaultBindsIdentity(t *testing.T) {
	key := randomKeyHex(t)
	sealed := NodeStorageKeyIdentity{HostID: 1, Dataset: "tank/instances/vm-1", Version: 1}

	envelope, err := SealNodeStorageKeyWithSecret(key, sealed, testVaultSecret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	others := []NodeStorageKeyIdentity{
		{HostID: 2, Dataset: sealed.Dataset, Version: sealed.Version},
		{HostID: sealed.HostID, Dataset: "tank/instances/vm-2", Version: sealed.Version},
		{HostID: sealed.HostID, Dataset: sealed.Dataset, Version: 2},
	}
	for _, other := range others {
		if _, err := UnsealNodeStorageKeyWithSecret(envelope, other, testVaultSecret); err == nil {
			t.Fatalf("identity %#v must not decrypt the envelope", other)
		}
	}
}

// TestNodeStorageVaultRejectsWrongSecret pins that a wrong master key fails the tag
// check rather than returning garbage.
func TestNodeStorageVaultRejectsWrongSecret(t *testing.T) {
	key := randomKeyHex(t)
	identity := NodeStorageKeyIdentity{HostID: 1, Dataset: "tank/x", Version: 1}

	envelope, err := SealNodeStorageKeyWithSecret(key, identity, testVaultSecret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	if _, err := UnsealNodeStorageKeyWithSecret(envelope, identity, "another-encryption-key-0123456789ab"); err == nil {
		t.Fatal("a wrong secret must fail")
	}
}

// TestNodeStorageVaultRejectsTampering pins that any mutated field fails.
func TestNodeStorageVaultRejectsTampering(t *testing.T) {
	key := randomKeyHex(t)
	identity := NodeStorageKeyIdentity{HostID: 1, Dataset: "tank/x", Version: 1}

	envelope, err := SealNodeStorageKeyWithSecret(key, identity, testVaultSecret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	parts := strings.Split(envelope, ":")

	mutations := [][4]string{
		{"v2", parts[1], parts[2], parts[3]},
		{parts[0], strings.Repeat("0", 24), parts[2], parts[3]},
		{parts[0], parts[1], strings.Repeat("0", 32), parts[3]},
		{parts[0], parts[1], parts[2], strings.Repeat("0", 64)},
	}
	for _, mutated := range mutations {
		candidate := strings.Join(mutated[:], ":")
		if _, err := UnsealNodeStorageKeyWithSecret(candidate, identity, testVaultSecret); err == nil {
			t.Fatalf("tampered envelope accepted: %q", candidate)
		}
	}

	// A truncated envelope.
	if _, err := UnsealNodeStorageKeyWithSecret("v1:aa:bb", identity, testVaultSecret); err == nil {
		t.Fatal("malformed envelope must be rejected")
	}
}

// TestNodeStorageKeyFingerprint pins the sha256-of-raw-bytes digest and the input
// validation.
func TestNodeStorageKeyFingerprint(t *testing.T) {
	key := strings.Repeat("ab", 32)

	fingerprint, err := NodeStorageKeyFingerprint(key)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fingerprint) != 64 {
		t.Fatalf("fingerprint length = %d", len(fingerprint))
	}
	// The same key must always produce the same fingerprint.
	again, _ := NodeStorageKeyFingerprint(key)
	if again != fingerprint {
		t.Fatal("fingerprint is not deterministic")
	}

	// Different keys produce different fingerprints.
	other, _ := NodeStorageKeyFingerprint(strings.Repeat("cd", 32))
	if other == fingerprint {
		t.Fatal("distinct keys produced the same fingerprint")
	}

	for _, bad := range []string{
		"",
		"ab",
		strings.Repeat("AB", 32), // uppercase is rejected
		strings.Repeat("zz", 32),
		strings.Repeat("ab", 31),
		strings.Repeat("ab", 33),
	} {
		if _, err := NodeStorageKeyFingerprint(bad); err == nil {
			t.Errorf("fingerprint(%q) should fail", bad)
		}
	}
}

// TestNodeStorageVaultRejectsBadIdentity pins the identity validation bounds.
func TestNodeStorageVaultRejectsBadIdentity(t *testing.T) {
	key := randomKeyHex(t)

	for _, bad := range []NodeStorageKeyIdentity{
		{HostID: 0, Dataset: "tank/x", Version: 1},
		{HostID: -1, Dataset: "tank/x", Version: 1},
		{HostID: 1, Dataset: "tank/x", Version: 0},
		{HostID: 1, Dataset: "", Version: 1},
		{HostID: 1, Dataset: "1tank/x", Version: 1},   // must start with a letter
		{HostID: 1, Dataset: "tank/../x", Version: 1}, // traversal
		{HostID: 1, Dataset: strings.Repeat("a", 201), Version: 1},
	} {
		if _, err := SealNodeStorageKeyWithSecret(key, bad, testVaultSecret); err == nil {
			t.Errorf("identity %#v should be rejected", bad)
		}
	}
}

// TestNodeStorageVaultRejectsShortSecret pins the ENCRYPTION_KEY strength check.
func TestNodeStorageVaultRejectsShortSecret(t *testing.T) {
	key := randomKeyHex(t)
	identity := NodeStorageKeyIdentity{HostID: 1, Dataset: "tank/x", Version: 1}

	for _, secret := range []string{"", "short", strings.Repeat("a", 31)} {
		if _, err := SealNodeStorageKeyWithSecret(key, identity, secret); err == nil {
			t.Errorf("secret of length %d should be rejected", len(secret))
		}
	}
	// Exactly 32 is accepted.
	if _, err := SealNodeStorageKeyWithSecret(key, identity, strings.Repeat("a", 32)); err != nil {
		t.Fatalf("32-char secret rejected: %v", err)
	}
}

// TestDialNodePinnedRejectsBadFingerprint pins that a malformed pin is refused
// BEFORE dialling, so a bad config can never become an unpinned connection.
func TestDialNodePinnedRejectsBadFingerprint(t *testing.T) {
	for _, bad := range []string{"", "abc", strings.Repeat("A", 64), strings.Repeat("z", 64)} {
		if _, err := DialNodePinned(t.Context(), "https://127.0.0.1:8443", bad, NodePinnedCredentials{}); err == nil {
			t.Errorf("fingerprint %q should be rejected", bad)
		}
	}
}

// TestDialNodePinnedRequiresHTTPS pins the no-plaintext rule.
func TestDialNodePinnedRequiresHTTPS(t *testing.T) {
	valid := strings.Repeat("a", 64)
	if _, err := DialNodePinned(t.Context(), "http://127.0.0.1:8443", valid, NodePinnedCredentials{}); err == nil {
		t.Fatal("http must be refused")
	}
}

// TestConstantTimeHexEqual pins the comparison helper.
func TestConstantTimeHexEqual(t *testing.T) {
	left := strings.Repeat("ab", 32)
	if !constantTimeHexEqual(left, left) {
		t.Fatal("identical strings must compare equal")
	}
	if constantTimeHexEqual(left, strings.Repeat("ab", 31)+"ac") {
		t.Fatal("different strings must not compare equal")
	}
	if constantTimeHexEqual(left, strings.Repeat("ab", 31)) {
		t.Fatal("length mismatch must not compare equal")
	}
}

// testSelfSignedCertificate builds a throwaway leaf certificate for the TLS
// tests below. It returns the certificate (usable as both the server and the
// client half) together with its sha256 fingerprint.
func testSelfSignedCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "example.test"},
		DNSNames:     []string{"example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	sum := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, fmt.Sprintf("%x", sum)
}

// TestDialNodePinnedOverCustomDial covers the tunnel path: a pinned node whose
// byte stream comes from a caller-supplied dialer (the host agent tunnel) must
// still complete TLS and verify the pin before the connection is handed back.
func TestDialNodePinnedOverCustomDial(t *testing.T) {
	certificate, fingerprint := testSelfSignedCertificate(t)
	serverConfig := &tls.Config{Certificates: []tls.Certificate{certificate}}

	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	go func() {
		server := tls.Server(serverConn, serverConfig)
		_ = server.Handshake()
	}()

	dial := func(context.Context) (net.Conn, error) { return clientConn, nil }
	_, err := DialNodePinned(t.Context(), "https://example.test:8443", fingerprint,
		NodePinnedCredentials{Certificate: certificate, Dial: dial})
	if err != nil {
		t.Fatalf("tunnelled pinned dial failed: %v", err)
	}
	// Close the raw pipe, not the tls.Conn: tls.Conn.Close writes close_notify,
	// which would block on an unread net.Pipe until the deadline.
	_ = clientConn.Close()
}

// TestDialNodePinnedOverCustomDialRejectsMismatch proves the pin is still checked
// on the tunnel path: a different certificate must not be accepted.
func TestDialNodePinnedOverCustomDialRejectsMismatch(t *testing.T) {
	certificate, _ := testSelfSignedCertificate(t)
	_, otherFingerprint := testSelfSignedCertificate(t)
	serverConfig := &tls.Config{Certificates: []tls.Certificate{certificate}}

	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	go func() {
		server := tls.Server(serverConn, serverConfig)
		_ = server.Handshake()
	}()

	_, err := DialNodePinned(t.Context(), "https://example.test:8443", otherFingerprint,
		NodePinnedCredentials{Certificate: certificate, Dial: func(context.Context) (net.Conn, error) {
			return clientConn, nil
		}})
	if !errors.Is(err, ErrNodeIdentityMismatch) {
		t.Fatalf("expected ErrNodeIdentityMismatch, got %v", err)
	}
}

// TestDialNodePinnedReportsDialFailure proves a failing tunnel dial surfaces as a
// connection failure, never as an accepted connection.
func TestDialNodePinnedReportsDialFailure(t *testing.T) {
	_, fingerprint := testSelfSignedCertificate(t)
	_, err := DialNodePinned(t.Context(), "https://example.test:8443", fingerprint,
		NodePinnedCredentials{Dial: func(context.Context) (net.Conn, error) {
			return nil, errors.New("tunnel offline")
		}})
	if !errors.Is(err, ErrNodeConnectFailed) {
		t.Fatalf("expected ErrNodeConnectFailed, got %v", err)
	}
}
