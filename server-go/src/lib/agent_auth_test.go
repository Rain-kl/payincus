package lib_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// ==================== stable body hashing (the HMAC binding) ====================

func TestAgentStableStringifyMatchesTypeScript(t *testing.T) {
	cases := []struct {
		name string
		body any
		want string
	}{
		{"nil", nil, "null"},
		{"empty object", map[string]any{}, "{}"},
		{"sorted keys", map[string]any{"b": float64(1), "a": "x"}, `{"a":"x","b":1}`},
		{
			"array indices come first, then sorted keys",
			map[string]any{"b": float64(1), "2": "two", "1": "one", "a": true},
			`{"1":"one","2":"two","a":true,"b":1}`,
		},
		{"nested", map[string]any{"z": []any{float64(1), nil, "s"}, "a": map[string]any{"y": false, "x": true}},
			`{"a":{"x":true,"y":false},"z":[1,null,"s"]}`},
		{"integer nine digits stays fixed", map[string]any{"n": float64(1000000)}, `{"n":1000000}`},
		{"1e21 uses exponential", map[string]any{"n": float64(1e21)}, `{"n":1e+21}`},
		{"1e-7 exponent has no zero padding", map[string]any{"n": float64(1e-7)}, `{"n":1e-7}`},
		{"1e-6 stays fixed", map[string]any{"n": float64(1e-6)}, `{"n":0.000001}`},
		{"negative zero is 0", map[string]any{"n": float64(0) * -1}, `{"n":0}`},
		{"fraction", map[string]any{"n": 1.5}, `{"n":1.5}`},
		{"no html escaping", map[string]any{"s": "<a>&b</a>"}, `{"s":"<a>&b</a>"}`},
		{"line separator stays raw", map[string]any{"s": "a\u2028b"}, "{\"s\":\"a\u2028b\"}"},
		{"control chars", map[string]any{"s": "\x01\t\n\r\b\f\"\\"}, `{"s":"\u0001\t\n\r\b\f\"\\"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := lib.StableStringifyAgentBody(testCase.body); got != testCase.want {
				t.Fatalf("StableStringifyAgentBody = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestAgentBodyHashConstants(t *testing.T) {
	// sha256("{}") — the hash the node sends for a bodyless report.
	emptySum := sha256.Sum256([]byte("{}"))
	emptyHash := hex.EncodeToString(emptySum[:])
	if got := lib.CreateAgentBodyHash(map[string]any{}); got != emptyHash {
		t.Fatalf("empty object hash = %s, want %s", got, emptyHash)
	}
	// createAgentBodyHash itself hashes stableStringify(undefined) = "null";
	// substituting {} for a missing body is the route's job (see
	// AgentBodyHashForRequest below).
	nullSum := sha256.Sum256([]byte("null"))
	if got := lib.CreateAgentBodyHash(nil); got != hex.EncodeToString(nullSum[:]) {
		t.Fatalf("nil body hash = %s, want sha256(\"null\")", got)
	}

	// GET without a body hashes the empty string, not "{}".
	blankSum := sha256.Sum256(nil)
	blankHash := hex.EncodeToString(blankSum[:])
	if got := lib.AgentBodyHashForRequest(http.MethodGet, false, nil); got != blankHash {
		t.Fatalf("GET body hash = %s, want sha256(\"\") = %s", got, blankHash)
	}
	if got := lib.AgentBodyHashForRequest(http.MethodGet, false, map[string]any{}); got != blankHash {
		t.Fatalf("GET with empty body map = %s, want %s", got, blankHash)
	}
	if got := lib.AgentBodyHashForRequest(http.MethodPost, false, nil); got != emptyHash {
		t.Fatalf("POST without a body = %s, want sha256({}) = %s", got, emptyHash)
	}
}

// ==================== HMAC signature ====================

func TestAgentSigningPayloadLayout(t *testing.T) {
	payload := lib.AgentSigningPayload{
		Method:    "post",
		Path:      "/api/agent/heartbeat",
		Timestamp: "1700000000000",
		Nonce:     "nonce-12345",
		BodyHash:  strings.ToUpper(strings.Repeat("ab", 32)),
	}
	want := "POST\n/api/agent/heartbeat\n1700000000000\nnonce-12345\n" + strings.Repeat("ab", 32)
	if got := lib.BuildAgentSigningPayload(payload); got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}

func TestAgentSignatureVerify(t *testing.T) {
	secret := lib.GenerateAgentSecret()
	payload := lib.AgentSigningPayload{
		Method:    http.MethodPost,
		Path:      "/api/agent/security-report",
		Timestamp: "1700000000000",
		Nonce:     "nonce-abcdef",
		BodyHash:  lib.CreateAgentBodyHash(map[string]any{"a": float64(1)}),
	}
	signature := lib.CreateAgentSignature(secret, payload)
	if len(signature) != 64 {
		t.Fatalf("signature length = %d, want 64 hex chars", len(signature))
	}
	if !lib.VerifyAgentSignature(secret, payload, signature) {
		t.Fatal("valid signature rejected")
	}
	if !lib.VerifyAgentSignature(secret, payload, strings.ToUpper(signature)) {
		t.Fatal("uppercase signature should be accepted (the TypeScript lowercases it)")
	}
	if lib.VerifyAgentSignature(secret, payload, strings.Repeat("0", 64)) {
		t.Fatal("wrong signature accepted")
	}
	if lib.VerifyAgentSignature(lib.GenerateAgentSecret(), payload, signature) {
		t.Fatal("signature accepted under a different secret")
	}
	// A tampered field must invalidate the signature.
	tampered := payload
	tampered.Path = "/api/agent/heartbeat"
	if lib.VerifyAgentSignature(secret, tampered, signature) {
		t.Fatal("signature accepted for a different path")
	}
	// Malformed signatures are rejected before any HMAC work.
	for _, bad := range []string{"", "zz", strings.Repeat("g", 64), signature + "0"} {
		if lib.VerifyAgentSignature(secret, payload, bad) {
			t.Fatalf("malformed signature %q accepted", bad)
		}
	}
}

func TestAgentTimestampFreshness(t *testing.T) {
	now := int64(1700000000000)
	if !lib.IsAgentTimestampFresh("1700000000000", now) {
		t.Fatal("current timestamp rejected")
	}
	if !lib.IsAgentTimestampFresh("1700000300000", now) {
		t.Fatal("+5min boundary must be accepted")
	}
	if lib.IsAgentTimestampFresh("1700000300001", now) {
		t.Fatal("+5min+1ms accepted")
	}
	if !lib.IsAgentTimestampFresh("1699999700000", now) {
		t.Fatal("-5min boundary must be accepted")
	}
	if lib.IsAgentTimestampFresh("1699999699999", now) {
		t.Fatal("-5min-1ms accepted")
	}
	for _, bad := range []string{"", "123", "17000000000000", "17e12", "-1700000000000"} {
		if lib.IsAgentTimestampFresh(bad, now) {
			t.Fatalf("timestamp %q accepted", bad)
		}
	}
}

func TestAgentHeaderValidation(t *testing.T) {
	valid := lib.AgentAuthHeaders{
		AgentID:   lib.GenerateAgentID(),
		Timestamp: "1700000000000",
		Nonce:     "nonce-12345",
		BodyHash:  strings.Repeat("a", 64),
		Signature: strings.Repeat("b", 64),
	}
	if got := lib.ValidateAgentHeaders(&valid); got != "" {
		t.Fatalf("valid headers rejected: %s", got)
	}
	broken := valid
	broken.AgentID = "agt_short"
	if got := lib.ValidateAgentHeaders(&broken); got != "invalid agent id" {
		t.Fatalf("agent id error = %q", got)
	}
	broken = valid
	broken.Timestamp = "12345"
	if got := lib.ValidateAgentHeaders(&broken); got != "invalid timestamp" {
		t.Fatalf("timestamp error = %q", got)
	}
	broken = valid
	broken.Nonce = "short"
	if got := lib.ValidateAgentHeaders(&broken); got != "invalid nonce" {
		t.Fatalf("nonce error = %q", got)
	}
	broken = valid
	broken.BodyHash = "nothex"
	if got := lib.ValidateAgentHeaders(&broken); got != "invalid body hash" {
		t.Fatalf("body hash error = %q", got)
	}
	broken = valid
	broken.Signature = "nothex"
	if got := lib.ValidateAgentHeaders(&broken); got != "invalid signature" {
		t.Fatalf("signature error = %q", got)
	}
}

func TestAgentReadAuthHeaders(t *testing.T) {
	headers := http.Header{}
	if lib.ReadAgentAuthHeaders(headers) != nil {
		t.Fatal("empty headers must produce nil")
	}
	headers.Set("X-Incus-Agent-Id", "agt_0123456789abcdefghijklmn")
	headers.Set("X-Incus-Timestamp", "1700000000000")
	headers.Set("X-Incus-Nonce", "nonce-12345")
	headers.Set("X-Incus-Body-Sha256", strings.Repeat("a", 64))
	headers.Set("X-Incus-Signature", strings.Repeat("b", 64))
	parsed := lib.ReadAgentAuthHeaders(headers)
	if parsed == nil || parsed.AgentID != "agt_0123456789abcdefghijklmn" {
		t.Fatalf("headers not parsed: %+v", parsed)
	}
	// Multiple values: the first wins (getHeaderValue).
	headers.Add("X-Incus-Nonce", "second-nonce")
	if parsed := lib.ReadAgentAuthHeaders(headers); parsed.Nonce != "nonce-12345" {
		t.Fatalf("nonce = %q, want the first header value", parsed.Nonce)
	}
}

func TestAgentIdentityFormats(t *testing.T) {
	secret := lib.GenerateAgentSecret()
	if !lib.IsValidAgentSecret(secret) {
		t.Fatalf("generated secret %q rejected", secret)
	}
	if !strings.HasPrefix(secret, "ias_") {
		t.Fatalf("secret prefix = %q", secret[:4])
	}
	agentID := lib.GenerateAgentID()
	if !lib.IsValidAgentID(agentID) {
		t.Fatalf("generated agent id %q rejected", agentID)
	}
	if lib.IsValidAgentID("agt_") || lib.IsValidAgentID("AGENT_"+agentID) {
		t.Fatal("invalid agent ids accepted")
	}
	if lib.IsValidAgentSecret("ias_short") {
		t.Fatal("short secret accepted")
	}
	// hashAgentSecret is a plain sha256 hex, lowercase.
	sum := sha256.Sum256([]byte("ias_abc"))
	if got := lib.HashAgentSecret("ias_abc"); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("secret hash = %s", got)
	}
}

func TestAgentInstallTokenShape(t *testing.T) {
	if !lib.IsAgentInstallToken("ait_" + strings.Repeat("a", 32)) {
		t.Fatal("minimum-length token rejected")
	}
	if lib.IsAgentInstallToken("ait_short") {
		t.Fatal("short token accepted")
	}
	if lib.IsAgentInstallToken("pat_" + strings.Repeat("a", 32)) {
		t.Fatal("non-ait token accepted")
	}
}

// ==================== startup-key envelopes ====================

func TestStartupEnvelopeRoundTrip(t *testing.T) {
	secret := lib.GenerateAgentSecret()
	agentID := lib.GenerateAgentID()
	sealed, err := lib.SealStartup(secret, agentID, "request", map[string]any{
		"token":     secret,
		"timestamp": float64(1700000000000),
		"nonce":     "nonce-12345",
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if sealed.IV == "" || sealed.Ciphertext == "" {
		t.Fatal("empty envelope")
	}

	proof, err := lib.VerifyStartupProof(secret, agentID, map[string]any{
		"iv": sealed.IV, "ciphertext": sealed.Ciphertext,
	}, 1700000000000)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if proof.Nonce != "nonce-12345" || proof.Timestamp != 1700000000000 {
		t.Fatalf("proof = %+v", proof)
	}

	// Wrong secret, wrong aAD (agent id / direction) and a tampered tag must fail.
	if _, err := lib.VerifyStartupProof(lib.GenerateAgentSecret(), agentID, sealed, 1700000000000); err == nil {
		t.Fatal("wrong secret accepted")
	}
	if _, err := lib.VerifyStartupProof(secret, lib.GenerateAgentID(), sealed, 1700000000000); err == nil {
		t.Fatal("wrong agent id accepted (the agentId is in the AAD)")
	}
	if _, err := lib.OpenStartup(secret, agentID, "response", sealed); err == nil {
		t.Fatal("wrong direction accepted (the direction is in the AAD)")
	}
	tampered := sealed.Ciphertext[:len(sealed.Ciphertext)-2] + "AA"
	if _, err := lib.VerifyStartupProof(secret, agentID, map[string]any{"iv": sealed.IV, "ciphertext": tampered}, 1700000000000); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}

	// Stale proof (outside the 3 minute window) and a bad nonce are rejected.
	if _, err := lib.VerifyStartupProof(secret, agentID, sealed, 1700000000000+lib.StartupWindowMs+1); err == nil {
		t.Fatal("stale proof accepted")
	}
	shortNonce, err := lib.SealStartup(secret, agentID, "request", map[string]any{
		"token": secret, "timestamp": float64(1700000000000), "nonce": "x",
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := lib.VerifyStartupProof(secret, agentID, shortNonce, 1700000000000); err == nil {
		t.Fatal("invalid nonce accepted")
	}
	// A proof carrying a different token is rejected even though it decrypts.
	wrongToken, err := lib.SealStartup(secret, agentID, "request", map[string]any{
		"token":     lib.GenerateAgentSecret(),
		"timestamp": float64(1700000000000),
		"nonce":     "nonce-12345",
	})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := lib.VerifyStartupProof(secret, agentID, wrongToken, 1700000000000); err == nil {
		t.Fatal("proof with a different token accepted")
	}
}

func TestStartupOpenRejectsMalformedEnvelopes(t *testing.T) {
	secret := lib.GenerateAgentSecret()
	agentID := lib.GenerateAgentID()
	cases := []any{
		nil,
		"string",
		map[string]any{},
		map[string]any{"iv": "!!!", "ciphertext": "AAAA"},
		map[string]any{"iv": "AAAA", "ciphertext": "AAAA"},
	}
	for _, input := range cases {
		if _, err := lib.OpenStartup(secret, agentID, "request", input); err == nil {
			t.Fatalf("malformed envelope %#v accepted", input)
		}
	}
}

// ==================== panel certificate ====================

func writeTestCertificate(t *testing.T, notBefore, notAfter time.Time) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "payincus-panel-test"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return string(pemBytes), hex.EncodeToString(func() []byte {
		sum := sha256.Sum256(der)
		return sum[:]
	}())
}

func TestReadPanelClientCertificate(t *testing.T) {
	certificate, fingerprint := writeTestCertificate(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	path := filepath.Join(t.TempDir(), "client.crt")
	if err := os.WriteFile(path, []byte(certificate), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	t.Setenv("PANEL_CRT_PATH", path)

	read, err := lib.ReadPanelClientCertificate()
	if err != nil {
		t.Fatalf("read certificate: %v", err)
	}
	if read.Fingerprint != fingerprint {
		t.Fatalf("fingerprint = %s, want %s", read.Fingerprint, fingerprint)
	}
	if !strings.HasPrefix(read.Certificate, "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("certificate is not PEM: %q", read.Certificate[:40])
	}
}

func TestReadPanelClientCertificateRejections(t *testing.T) {
	expired, _ := writeTestCertificate(t, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	path := filepath.Join(t.TempDir(), "expired.crt")
	if err := os.WriteFile(path, []byte(expired), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	t.Setenv("PANEL_CRT_PATH", path)
	if _, err := lib.ReadPanelClientCertificate(); err == nil ||
		!strings.Contains(err.Error(), "not currently valid") {
		t.Fatalf("expired certificate error = %v", err)
	}

	// A file that also carries a private key must be refused outright.
	valid, _ := writeTestCertificate(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	keyPEM := "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"
	if err := os.WriteFile(path, []byte(valid+keyPEM), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if _, err := lib.ReadPanelClientCertificate(); err == nil ||
		!strings.Contains(err.Error(), "without private key material") {
		t.Fatalf("private-key certificate error = %v", err)
	}

	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if _, err := lib.ReadPanelClientCertificate(); err == nil {
		t.Fatal("garbage certificate accepted")
	}
}
