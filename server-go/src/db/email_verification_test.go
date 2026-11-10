// Tests for db/email_verification.go. The SQL paths are covered by the dual-run
// conformance suite; what is pinned here is the pure crypto/serialisation contract:
// sha256 storage, legacy plaintext matching, and the 100000..999999 code space.
package db

import (
	"regexp"
	"strings"
	"testing"
)

// TestAuthHashVerificationCode pins the `sha256:<hex>` storage form.
func TestAuthHashVerificationCode(t *testing.T) {
	digest := AuthHashVerificationCode("123456")
	if !strings.HasPrefix(digest, authVerificationHashPrefix) {
		t.Fatalf("digest %q must start with %q", digest, authVerificationHashPrefix)
	}
	if len(digest) != len(authVerificationHashPrefix)+64 {
		t.Fatalf("digest length = %d, want %d", len(digest), len(authVerificationHashPrefix)+64)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(digest) {
		t.Fatalf("digest %q is not lowercase hex", digest)
	}
	if AuthHashVerificationCode("123456") != digest {
		t.Fatal("hashing must be deterministic")
	}
	if AuthHashVerificationCode("123457") == digest {
		t.Fatal("different codes must not collide")
	}
}

// TestAuthVerificationCodeMatches pins both storage generations.
func TestAuthVerificationCodeMatches(t *testing.T) {
	stored := AuthHashVerificationCode("654321")
	if !AuthVerificationCodeMatches(stored, "654321") {
		t.Fatal("hashed code must match its plaintext")
	}
	if AuthVerificationCodeMatches(stored, "000000") {
		t.Fatal("hashed code must not match a different plaintext")
	}
	// Legacy rows (pre-hashing) still hold the plaintext code.
	if !AuthVerificationCodeMatches("654321", "654321") {
		t.Fatal("legacy plaintext code must still match")
	}
	if AuthVerificationCodeMatches("654321", "123456") {
		t.Fatal("legacy plaintext code must not match a different value")
	}
}

// TestAuthGenerateVerificationCode pins the inclusive 100000..999999 range and
// the six-digit rendering.
func TestAuthGenerateVerificationCode(t *testing.T) {
	pattern := regexp.MustCompile(`^[1-9][0-9]{5}$`)
	seen := map[string]bool{}
	for iteration := 0; iteration < 200; iteration++ {
		code, err := AuthGenerateVerificationCode()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !pattern.MatchString(code) {
			t.Fatalf("code %q is not a six-digit code without a leading zero", code)
		}
		value := 0
		for _, character := range code {
			value = value*10 + int(character-'0')
		}
		if value < authVerificationCodeMin || value > authVerificationCodeMax {
			t.Fatalf("code %q outside [%d, %d]", code, authVerificationCodeMin, authVerificationCodeMax)
		}
		seen[code] = true
	}
	if len(seen) < 100 {
		t.Fatalf("only %d distinct codes in 200 draws; the generator does not look random", len(seen))
	}
}
