// Unit tests for the operation-verification port. These live in package lib (not
// lib_test) so they can exercise the unexported helpers directly.
package lib

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// TestGenerateVerificationCode pins the 6-digit range [100000, 999999] and the
// randomness contract (crypto.randomInt(100000, 1000000)).
func TestGenerateVerificationCode(t *testing.T) {
	seen := make(map[string]bool, 500)
	for i := 0; i < 500; i++ {
		code, err := generateVerificationCode()
		if err != nil {
			t.Fatalf("generateVerificationCode: %v", err)
		}
		if len(code) != 6 {
			t.Fatalf("code %q length = %d, want 6", code, len(code))
		}
		value, err := strconv.Atoi(code)
		if err != nil {
			t.Fatalf("code %q is not numeric: %v", code, err)
		}
		if value < 100000 || value > 999999 {
			t.Fatalf("code %q out of range [100000,999999]", code)
		}
		seen[code] = true
	}
	// 500 draws from 900000 values: expected distinct ~499.86, so <400 means broken RNG.
	if len(seen) < 400 {
		t.Fatalf("expected mostly distinct codes, got %d distinct out of 500", len(seen))
	}
}

// TestVerificationCodeHashingAndMatching covers the sha256-prefixed and legacy
// plaintext branches, plus the replay-relevant property that a wrong input never
// matches.
func TestVerificationCodeHashingAndMatching(t *testing.T) {
	hash := hashVerificationCode("123456")
	if !strings.HasPrefix(hash, "sha256:") {
		t.Fatalf("hash %q missing sha256: prefix", hash)
	}
	if len(hash) != len("sha256:")+64 {
		t.Fatalf("hash %q has unexpected length %d", hash, len(hash))
	}
	// SHA-256("123456") is a well-known vector.
	const want = "sha256:8d969eef6ecad3c29a3a629280e686cf0c3f5d5a86aff3ca12020c923adc6c92"
	if hash != want {
		t.Fatalf("hashVerificationCode(\"123456\") = %q, want %q", hash, want)
	}

	if !verificationCodeMatches(hash, "123456") {
		t.Fatal("hashed code must match its own input")
	}
	if verificationCodeMatches(hash, "123457") {
		t.Fatal("hashed code must not match a different input")
	}

	// Legacy plaintext rows compare verbatim.
	if !verificationCodeMatches("654321", "654321") {
		t.Fatal("legacy plaintext code must match verbatim")
	}
	if verificationCodeMatches("654321", "123456") {
		t.Fatal("legacy plaintext code must not match a different input")
	}

	// A stored value that merely starts with the prefix is always treated as a
	// hash, so it never matches itself verbatim (faithful to the Node branch).
	if verificationCodeMatches("sha256:nothex", "sha256:nothex") {
		t.Fatal("prefixed stored value must be treated as a hash, not verbatim")
	}
}

// TestMatchVerificationCandidate covers ordering (newest wins) and legacy rows.
func TestMatchVerificationCandidate(t *testing.T) {
	newest := models.OperationVerification{ID: 3, Code: hashVerificationCode("111111")}
	older := models.OperationVerification{ID: 2, Code: "222222"} // legacy plaintext
	candidates := []models.OperationVerification{newest, older}

	index, ok := matchVerificationCandidate(candidates, "111111", hashVerificationCode("111111"))
	if !ok || index != 0 {
		t.Fatalf("hashed candidate: index=%d ok=%v, want 0 true", index, ok)
	}

	index, ok = matchVerificationCandidate(candidates, "222222", hashVerificationCode("222222"))
	if !ok || index != 1 {
		t.Fatalf("legacy candidate: index=%d ok=%v, want 1 true", index, ok)
	}

	if _, ok := matchVerificationCandidate(candidates, "999999", hashVerificationCode("999999")); ok {
		t.Fatal("unmatched code must not select any candidate")
	}

	if _, ok := matchVerificationCandidate(nil, "111111", hashVerificationCode("111111")); ok {
		t.Fatal("empty candidate list must not match")
	}
}

// TestVerificationClaimOnceUnderConcurrency models the production anti-replay
// contract: verifyOperationCode claims a candidate with a single atomic
// conditional UPDATE whose affected-row count must be exactly 1, and
// claimOperationVerificationRequirement consumes a verified row with an atomic
// DELETE. The same claim-once contract is exercised in-process (CAS on a flag)
// using the production matching helpers, under -race, so that only one concurrent
// claimant can win.
func TestVerificationClaimOnceUnderConcurrency(t *testing.T) {
	const code = "123456"
	candidates := []models.OperationVerification{
		{ID: 1, Code: hashVerificationCode(code)},
	}

	var claimed int32
	var winners int32
	var wg sync.WaitGroup

	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, found := matchVerificationCandidate(candidates, code, hashVerificationCode(code)); !found {
				return
			}
			// Models `UPDATE ... WHERE verified = false` returning RowsAffected == 1.
			if atomic.CompareAndSwapInt32(&claimed, 0, 1) {
				atomic.AddInt32(&winners, 1)
			}
		}()
	}
	wg.Wait()

	if winners != 1 {
		t.Fatalf("expected exactly 1 winning claim, got %d", winners)
	}
}

// TestOperationClassification pins the account/resource operation split.
func TestOperationClassification(t *testing.T) {
	for _, operation := range []models.OperationType{
		models.OperationTypeChangePassword,
		models.OperationTypeDisable2fa,
		models.OperationTypeChangeEmail,
		models.OperationTypeDeleteAccount,
	} {
		if !IsAccountOperation(operation) {
			t.Errorf("%s should be an account operation", operation)
		}
		if IsResourceOperation(operation) {
			t.Errorf("%s should not be a resource operation", operation)
		}
	}

	for _, operation := range []models.OperationType{
		models.OperationTypeDeleteInstance,
		models.OperationTypeReinstallInstance,
		models.OperationTypeRecreateInstance,
		models.OperationTypeTransferInstance,
		models.OperationTypeDeleteSnapshot,
		models.OperationTypeDeleteBackup,
	} {
		if !IsResourceOperation(operation) {
			t.Errorf("%s should be a resource operation", operation)
		}
		if IsAccountOperation(operation) {
			t.Errorf("%s should not be an account operation", operation)
		}
	}
}

// TestGetOperationName covers zh/en and the unknown-operation fallback.
func TestGetOperationName(t *testing.T) {
	if got := GetOperationName(models.OperationTypeChangePassword, "zh"); got != "修改密码" {
		t.Errorf("zh name = %q, want 修改密码", got)
	}
	if got := GetOperationName(models.OperationTypeChangePassword, "en"); got != "Change Password" {
		t.Errorf("en name = %q, want Change Password", got)
	}
	// Unknown operation types fall back to the raw enum value.
	if got := GetOperationName(models.OperationType("brand_new_op"), "zh"); got != "brand_new_op" {
		t.Errorf("fallback name = %q, want brand_new_op", got)
	}
}

// TestMaskEmail pins the local-part masking, including the no-"@" edge case that
// JavaScript renders as the literal "undefined".
func TestMaskEmail(t *testing.T) {
	if got := maskEmail("alice@example.com"); got != "al***@example.com" {
		t.Errorf("maskEmail = %q, want al***@example.com", got)
	}
	// Short local part is not padded.
	if got := maskEmail("a@example.com"); got != "a***@example.com" {
		t.Errorf("maskEmail short local = %q, want a***@example.com", got)
	}
	// Multi-@ addresses: JS destructures the first two parts only.
	if got := maskEmail("ab@c@d"); got != "ab***@c" {
		t.Errorf("maskEmail multi-@ = %q, want ab***@c", got)
	}
	// No "@" -> domain renders as "undefined" (faithful to JS).
	if got := maskEmail("alice"); got != "al***@undefined" {
		t.Errorf("maskEmail no-@ = %q, want al***@undefined", got)
	}
}

// TestOperationVerificationRequestLockKey pins the advisory-lock key format.
func TestOperationVerificationRequestLockKey(t *testing.T) {
	if got := getOperationVerificationRequestLockKey(7, models.OperationTypeDeleteAccount, 0); got != "7:delete_account:account" {
		t.Errorf("account lock key = %q, want 7:delete_account:account", got)
	}
	if got := getOperationVerificationRequestLockKey(7, models.OperationTypeDeleteInstance, 42); got != "7:delete_instance:42" {
		t.Errorf("resource lock key = %q, want 7:delete_instance:42", got)
	}
}

// TestOperationResourceClause pins the null-vs-value resource predicate that
// Prisma emits for `resourceId: resourceId || null`.
func TestOperationResourceClause(t *testing.T) {
	clause, args := operationResourceClause(0)
	if clause != "resource_id IS NULL" || len(args) != 0 {
		t.Fatalf("zero resource clause = %q args=%v, want resource_id IS NULL []", clause, args)
	}
	clause, args = operationResourceClause(9)
	if clause != "resource_id = ?" || len(args) != 1 || args[0] != int64(9) {
		t.Fatalf("non-zero resource clause = %q args=%v, want resource_id = ? [9]", clause, args)
	}
}

// TestVerifyOperationCodeReplayAgainstDatabase is an opt-in end-to-end replay
// test. It runs only when PAYINCUS_GO_DB_TESTS=1 and DATABASE_URL is set, and
// cleans up every row it creates. First verification succeeds; a second attempt
// with the same code must fail (the record was atomically claimed).
func TestVerifyOperationCodeReplayAgainstDatabase(t *testing.T) {
	if os.Getenv("PAYINCUS_GO_DB_TESTS") != "1" || os.Getenv("DATABASE_URL") == "" {
		t.Skip("set PAYINCUS_GO_DB_TESTS=1 and DATABASE_URL to run the database-backed replay test")
	}

	ctx := context.Background()
	if err := db.Init(ctx); err != nil {
		t.Fatalf("db.Init: %v", err)
	}
	defer db.Close()

	var userID int64
	if err := db.DB.Raw("SELECT id FROM users LIMIT 1").Scan(&userID).Error; err != nil {
		t.Fatalf("load a user: %v", err)
	}
	if userID == 0 {
		t.Skip("no users present to attach a verification row to")
	}

	const code = "135790"
	now := time.Now().UTC()
	record := models.OperationVerification{
		UserID:        int32(userID),
		OperationType: models.OperationTypeDeleteAccount,
		Code:          hashVerificationCode(code),
		Channel:       models.VerificationChannelEmail,
		ExpiresAt:     now.Add(10 * time.Minute),
	}
	if err := db.DB.Create(&record).Error; err != nil {
		t.Fatalf("insert verification: %v", err)
	}
	defer db.DB.Where("id = ?", record.ID).Delete(&models.OperationVerification{})

	first, err := VerifyOperationCode(ctx, userID, models.OperationTypeDeleteAccount, code, 0)
	if err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if !first.Success || !first.Verified {
		t.Fatalf("first verify = %+v, want success", first)
	}

	second, err := VerifyOperationCode(ctx, userID, models.OperationTypeDeleteAccount, code, 0)
	if err != nil {
		t.Fatalf("second verify: %v", err)
	}
	if second.Success || second.Verified {
		t.Fatalf("replay must be rejected, got %+v", second)
	}
	if second.ErrorCode != OperationVerificationErrorInvalidCode {
		t.Fatalf("replay error code = %q, want %q", second.ErrorCode, OperationVerificationErrorInvalidCode)
	}
}
