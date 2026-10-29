package routes

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// The fingerprint is a user-visible contract (it is what the UI shows and what
// ssh-key de-duplication compares), so it is pinned against values produced by the
// Node implementation for the same key material.
func TestCalculateFingerprintMatchesNode(t *testing.T) {
	cases := []struct {
		name     string
		keyData  string
		expected string
	}{
		{
			name:     "rsa",
			keyData:  "AAAAB3NzaC1yc2EAAAADAQABAAABAQDCommentTest",
			expected: "SHA256:a1Cv6u+ImDnkxKTKgYnCOwNOTpM1HZJ8A7k9AjL4YCA",
		},
		{
			name:     "ed25519 with padding",
			keyData:  "AAAAC3NzaC1lZDI1NTE5AAAAIJDGkFS8vGJ7Y5x2J3e5H9KpXZ+abc=",
			expected: "SHA256:vnQW6oWJE5RvfwIriL1b84zFSEn3m/N33p2uE9slb0Q",
		},
		{
			name:     "ecdsa",
			keyData:  "AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBJunk==",
			expected: "SHA256:e6V3rLoDjlojP0aI2+HHhdAUkpRYyGiL9Kii7mucuJY",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := calculateFingerprint(tc.keyData); got != tc.expected {
				t.Fatalf("calculateFingerprint() = %q, want %q", got, tc.expected)
			}
		})
	}
}

// nodeBase64Decode must reproduce Buffer.from(x, 'base64'), which skips invalid
// characters and stops at the first '='. If it drifted, the fingerprint of a key
// with padding would change.
func TestNodeBase64DecodeMatchesBufferFrom(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string // hex
	}{
		{"invalid chars are skipped", "!!!!abc=def", "69b7"},
		{"stops at the first padding", "QQ==QQ==", "41"},
		{"unpadded", "QQ", "41"},
		{"standard alphabet", "4pyV", "e29c95"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decoded := nodeBase64Decode(tc.input)
			got := ""
			for _, b := range decoded {
				got += string("0123456789abcdef"[b>>4]) + string("0123456789abcdef"[b&0x0f])
			}
			if got != tc.expected {
				t.Fatalf("nodeBase64Decode(%q) = %s, want %s", tc.input, got, tc.expected)
			}
		})
	}
}

// truncateKey() only redacts long keys; a short key is returned unchanged.
func TestTruncateSshKey(t *testing.T) {
	short := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5"
	if got := truncateSshKey(short); got != short {
		t.Fatalf("short key = %q, want unchanged", got)
	}

	long := "ssh-rsa " + strings.Repeat("A", 80)
	got := truncateSshKey(long)
	if !strings.HasPrefix(got, "ssh-rsa ") {
		t.Fatalf("truncated prefix = %q", got)
	}
	if !strings.Contains(got, "...") {
		t.Fatalf("truncated key must contain the ellipsis: %q", got)
	}
	// first 30 runes + "..." + last 20 runes
	if len(got) != 30+3+20 {
		t.Fatalf("truncated length = %d, want 53", len(got))
	}
	if got != long[:30]+"..."+long[len(long)-20:] {
		t.Fatalf("truncated value = %q", got)
	}

	// Exactly 60 characters is not "short": the TypeScript test is `< 60`.
	sixty := strings.Repeat("k", 60)
	if got := truncateSshKey(sixty); got == sixty {
		t.Fatal("a 60-character key must be truncated")
	}
}

// parseSSHPublicKey accepts the documented algorithms and reports failure as null
// (here: ok=false) rather than panicking.
func TestParseSshPublicKey(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantOK      bool
		wantType    string
		wantKeyData string
		wantComment string
	}{
		{"rsa", "ssh-rsa AAAAB3Nza= user@host", true, "ssh-rsa", "AAAAB3Nza=", "user@host"},
		{"rsa without comment", "ssh-rsa AAAAB3Nza=", true, "ssh-rsa", "AAAAB3Nza=", ""},
		{"ed25519", "ssh-ed25519 AAAAabc=", true, "ssh-ed25519", "AAAAabc=", ""},
		{"ecdsa", "ecdsa-sha2-nistp256 AAAAabc= c", true, "ecdsa-sha2-nistp256", "AAAAabc=", "c"},
		{"newline is folded first", "ssh-rsa\nAAAAabc=", true, "ssh-rsa", "AAAAabc=", ""},
		{"dss is not supported", "ssh-dss AAAAabc=", false, "", "", ""},
		{"comment only", "user@host", false, "", "", ""},
		{"empty", "", false, "", "", ""},
		{"leading junk", "--- ssh-rsa AAAA", false, "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyType, keyData, comment, ok := parseSshPublicKey(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if keyType != tc.wantType || keyData != tc.wantKeyData || comment != tc.wantComment {
				t.Fatalf("parsed = (%q, %q, %q), want (%q, %q, %q)",
					keyType, keyData, comment, tc.wantType, tc.wantKeyData, tc.wantComment)
			}
		})
	}
}

// normalizeSshKeyCreateInput mirrors the route's post-schema normalization:
// name validation, newline folding, length bounds and control-character rejection.
func TestNormalizeSshKeyCreateInput(t *testing.T) {
	validKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJDGkFS8vGJ7"

	t.Run("accepts a well-formed body and trims the name", func(t *testing.T) {
		body, apiError := normalizeSshKeyCreateInput(map[string]any{
			"name":      "  my key  ",
			"publicKey": validKey,
		})
		if apiError != nil {
			t.Fatalf("unexpected error: %+v", apiError)
		}
		if body.Name != "my key" {
			t.Fatalf("name = %q, want %q", body.Name, "my key")
		}
		if body.PublicKey != validKey {
			t.Fatalf("publicKey = %q, want %q", body.PublicKey, validKey)
		}
	})

	t.Run("folds newlines into single spaces", func(t *testing.T) {
		body, apiError := normalizeSshKeyCreateInput(map[string]any{
			"name":      "folded",
			"publicKey": "ssh-ed25519\n\n AAAAC3NzaC1lZDI1NTE5AAAAIJDGkFS8vGJ7 \r\n",
		})
		if apiError != nil {
			t.Fatalf("unexpected error: %+v", apiError)
		}
		if body.PublicKey != "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJDGkFS8vGJ7" {
			t.Fatalf("publicKey = %q", body.PublicKey)
		}
	})

	t.Run("non-object body", func(t *testing.T) {
		_, apiError := normalizeSshKeyCreateInput([]any{"x"})
		if apiError == nil || apiError.Code != lib.ErrInvalidParams {
			t.Fatalf("apiError = %+v", apiError)
		}
		if apiError.Details != "Invalid request body" {
			t.Fatalf("details = %q", apiError.Details)
		}
	})

	t.Run("non-string field", func(t *testing.T) {
		_, apiError := normalizeSshKeyCreateInput(map[string]any{"name": 1, "publicKey": validKey})
		if apiError == nil || apiError.Details != "Invalid SSH key request" {
			t.Fatalf("apiError = %+v", apiError)
		}
	})

	t.Run("name with illegal characters", func(t *testing.T) {
		_, apiError := normalizeSshKeyCreateInput(map[string]any{
			"name":      "bad/name",
			"publicKey": validKey,
		})
		if apiError == nil || apiError.Code != lib.ErrInvalidParams {
			t.Fatalf("apiError = %+v", apiError)
		}
	})

	t.Run("public key too short", func(t *testing.T) {
		_, apiError := normalizeSshKeyCreateInput(map[string]any{
			"name":      "short",
			"publicKey": strings.Repeat("a", sshKeyMinPublicKeyLength-1),
		})
		if apiError == nil || apiError.Code != lib.ErrInvalidSshKey {
			t.Fatalf("apiError = %+v", apiError)
		}
	})

	t.Run("public key too long", func(t *testing.T) {
		_, apiError := normalizeSshKeyCreateInput(map[string]any{
			"name":      "long",
			"publicKey": "ssh-rsa " + strings.Repeat("A", sshKeyMaxPublicKeyLength),
		})
		if apiError == nil || apiError.Code != lib.ErrInvalidSshKey {
			t.Fatalf("apiError = %+v", apiError)
		}
	})

	t.Run("control characters are rejected", func(t *testing.T) {
		_, apiError := normalizeSshKeyCreateInput(map[string]any{
			"name":      "ctrl",
			"publicKey": "ssh-rsa AAAAB3Nza\x01C1yc2E",
		})
		if apiError == nil || apiError.Code != lib.ErrInvalidSshKey {
			t.Fatalf("apiError = %+v", apiError)
		}
	})
}

// The POST body schema runs before the handler, so its message and ordering must
// match Fastify + AJV (allErrors:false, coerceTypes:'array').
func TestValidateSshKeyCreateBody(t *testing.T) {
	validKey := strings.Repeat("k", sshKeyMinPublicKeyLength)

	cases := []struct {
		name    string
		body    any
		want    string
		wantKey string
	}{
		{"non-object", []any{1}, "must be object", ""},
		{"null body", nil, "must be object", ""},
		{"missing name", map[string]any{"publicKey": validKey}, "name is required", ""},
		{"missing publicKey", map[string]any{"name": "k"}, "publicKey is required", ""},
		{"missing both reports name first", map[string]any{}, "name is required", ""},
		{"empty name", map[string]any{"name": "", "publicKey": validKey}, "name is too short", ""},
		{"null name coerces to empty", map[string]any{"name": nil, "publicKey": validKey}, "name is too short", ""},
		{"name over 50", map[string]any{"name": strings.Repeat("x", 51), "publicKey": validKey}, "name is too long", ""},
		{"name of exactly 50", map[string]any{"name": strings.Repeat("x", 50), "publicKey": validKey}, "", strings.Repeat("x", 50)},
		{"number name is stringified", map[string]any{"name": float64(123), "publicKey": validKey}, "", "123"},
		{"boolean name is stringified", map[string]any{"name": true, "publicKey": validKey}, "", "true"},
		{"single-element array unwraps", map[string]any{"name": []any{"x"}, "publicKey": validKey}, "", "x"},
		{"multi-element array", map[string]any{"name": []any{"x", "y"}, "publicKey": validKey}, "must be string", ""},
		{"object name", map[string]any{"name": map[string]any{}, "publicKey": validKey}, "must be string", ""},
		{"short publicKey", map[string]any{"name": "k", "publicKey": "short"}, "publicKey is too short", ""},
		{"numeric publicKey is stringified then too short", map[string]any{"name": "k", "publicKey": float64(123)}, "publicKey is too short", ""},
		{"unknown keys are accepted and ignored", map[string]any{"name": "k", "publicKey": validKey, "extra": 1.0}, "", "k"},
		{"unicode name counts UTF-16 units", map[string]any{"name": strings.Repeat("\U0001F600", 26), "publicKey": validKey}, "name is too long", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, message := validateSshKeyCreateBody(tc.body)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
			if tc.want != "" {
				return
			}
			if body.Name != tc.wantKey {
				t.Fatalf("name = %q, want %q", body.Name, tc.wantKey)
			}
		})
	}
}

// opensshRsaPublicKey must produce the same ssh-rsa wire blob Node builds from the
// SPKI->JWK export, including the 0x00 modulus prefix when the high bit is set.
func TestOpenSSHRsaPublicKeyRoundTrips(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	encoded := opensshRsaPublicKey(&key.PublicKey)
	keyType, keyData, _, ok := parseSshPublicKey(encoded)
	if !ok || keyType != "ssh-rsa" {
		t.Fatalf("parseSshPublicKey(%q) = (%q, ok=%v)", encoded, keyType, ok)
	}

	blob := nodeBase64Decode(keyData)
	// SSH wire format: uint32 len + type, uint32 len + e, uint32 len + n.
	readField := func(data []byte, offset int) ([]byte, int) {
		length := int(data[offset])<<24 | int(data[offset+1])<<16 | int(data[offset+2])<<8 | int(data[offset+3])
		offset += 4
		return data[offset : offset+length], offset + length
	}
	wireType, offset := readField(blob, 0)
	if string(wireType) != "ssh-rsa" {
		t.Fatalf("wire type = %q", wireType)
	}
	exponent, offset := readField(blob, offset)
	if len(exponent) != 3 || exponent[0] != 0x01 || exponent[1] != 0x00 || exponent[2] != 0x01 {
		t.Fatalf("exponent = %x, want 010001", exponent)
	}
	modulus, _ := readField(blob, offset)
	if len(modulus) > 0 && modulus[0] == 0x00 {
		modulus = modulus[1:]
	}
	if string(modulus) != string(key.N.Bytes()) {
		t.Fatal("modulus does not match the generated key")
	}

	// /generate returns the PKCS#1 PEM that OpenSSH understands.
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "RSA PRIVATE KEY" {
		t.Fatalf("unexpected PEM block: %#v", block)
	}
	if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
		t.Fatalf("parse PKCS#1 private key: %v", err)
	}
}

// bigEndianUnsigned drops leading zero bytes, matching the JWK "e" encoding.
func TestBigEndianUnsigned(t *testing.T) {
	cases := []struct {
		value float64
		want  []byte
	}{
		{65537, []byte{0x01, 0x00, 0x01}},
		{3, []byte{0x03}},
		{0, []byte{}},
	}
	for _, tc := range cases {
		got := bigEndianUnsigned(int(tc.value))
		if string(got) != string(tc.want) {
			t.Errorf("bigEndianUnsigned(%v) = %x, want %x", tc.value, got, tc.want)
		}
	}
}
