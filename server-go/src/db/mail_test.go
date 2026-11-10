// Pure (no database) tests for the db/mail.go port. The DB-backed half of the
// contract is pinned by test/conformance/requests-mail.json and by the guard tests
// in routes/mail_test.go; what is anchored here are the normalisation helpers that
// silently change query results when they drift.
package db

import (
	"strings"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/models"
)

func TestMailClampListPagination(t *testing.T) {
	page := func(value int) *int { return &value }

	gotPage, gotSize := mailClampListPagination(nil, nil)
	if gotPage != 1 || gotSize != 20 {
		t.Fatalf("defaults = (%d,%d), want (1,20)", gotPage, gotSize)
	}

	gotPage, gotSize = mailClampListPagination(page(0), page(0))
	if gotPage != 1 || gotSize != 1 {
		t.Fatalf("zero = (%d,%d), want (1,1)", gotPage, gotSize)
	}

	gotPage, gotSize = mailClampListPagination(page(3), page(500))
	if gotPage != 3 || gotSize != 100 {
		t.Fatalf("clamp = (%d,%d), want (3,100)", gotPage, gotSize)
	}
}

func TestMailNormalizeListSearch(t *testing.T) {
	if got := mailNormalizeListSearch(nil); got != nil {
		t.Fatalf("nil search = %v", *got)
	}
	blank := "   "
	if got := mailNormalizeListSearch(&blank); got != nil {
		t.Fatalf("blank search must normalise to nil, got %q", *got)
	}

	long := "  " + strings.Repeat("x", 200) + "  "
	got := mailNormalizeListSearch(&long)
	if got == nil || len([]rune(*got)) != mailListSearchMaxLength {
		t.Fatalf("long search length = %d, want %d", len([]rune(*got)), mailListSearchMaxLength)
	}
}

func TestMailParseSearchUserID(t *testing.T) {
	valid := "42"
	if got := MailParseSearchUserID(&valid); got == nil || *got != 42 {
		t.Fatalf("valid id = %v", got)
	}

	for _, invalid := range []string{"", "0", "-1", "1.5", "abc", " 42", "42 ", "99999999999999999999"} {
		value := invalid
		if got := MailParseSearchUserID(&value); got != nil {
			t.Fatalf("MailParseSearchUserID(%q) = %d, want nil", invalid, *got)
		}
	}
}

func TestMailNormalizeStatuses(t *testing.T) {
	if got := mailNormalizeSubscriptionStatus("active"); got != models.MailSubscriptionStatusActive {
		t.Fatalf("active = %q", got)
	}
	if got := mailNormalizeSubscriptionStatus("bogus"); got != "" {
		t.Fatalf("bogus = %q", got)
	}
	if got := mailNormalizeDomainStatus("verified"); got != models.MailDomainStatusVerified {
		t.Fatalf("verified = %q", got)
	}
	if got := mailNormalizeDomainStatus("Verified"); got != "" {
		t.Fatalf("case-sensitive status must be rejected, got %q", got)
	}
}

func TestMailSecretRoundTrip(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "mail-test-encryption-key")
	t.Setenv("NODE_ENV", "development")

	encrypted, err := mailEncryptSecret("super-secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !isEncryptedSensitiveData(encrypted) {
		t.Fatalf("ciphertext %q is not recognised as encrypted", encrypted)
	}
	if got := mailDecryptSecret(encrypted); got != "super-secret" {
		t.Fatalf("round trip = %q", got)
	}

	// Already-encrypted input is not double encrypted.
	again, err := mailEncryptSecret(encrypted)
	if err != nil {
		t.Fatalf("re-encrypt: %v", err)
	}
	if again != encrypted {
		t.Fatalf("encryptMailSecret must not double-encrypt")
	}

	// Legacy plaintext passes through untouched.
	if got := mailDecryptSecret("plain"); got != "plain" {
		t.Fatalf("plaintext = %q", got)
	}
}

func TestMailDiskQuotaAllows(t *testing.T) {
	if !MailDiskQuotaAllows(0, 10240, 10) {
		t.Fatalf("10240 MB must fit exactly in 10 GB")
	}
	if MailDiskQuotaAllows(10240, 1, 10) {
		t.Fatalf("10241 MB must not fit in 10 GB")
	}
	if MailDiskQuotaAllows(0, 2048, 1) {
		t.Fatalf("2 GB request must not fit in a 1 GB subscription")
	}
}
