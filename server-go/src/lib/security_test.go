// Unit tests for the security.go port. They live in package lib (not lib_test)
// so the unexported helpers (encryption key derivation, nonce stores, UA
// normalisation, refresh-token lookup keys) can be exercised directly.
package lib

import (
	"encoding/base32"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// withSecurityNow pins the package clock for the duration of fn.
func withSecurityNow(ms int64, fn func()) {
	previous := securityNowMs
	securityNowMs = func() int64 { return ms }
	defer func() { securityNowMs = previous }()
	fn()
}

// ==================== 加解密 ====================

// Node-produced vector: ENCRYPTION_KEY="test-encryption-key-1234567890",
// IV=000102030405060708090a0b0c0d0e0f, plaintext="hello world 中文".
// Produced with crypto.createCipheriv('aes-256-gcm', sha256(key), iv).
const (
	nodeCiphertext = "000102030405060708090a0b0c0d0e0f:da8a4ce6471a4c6399bd8c6a8c63e731:fe928ae8374396ef09a6cf07cc3d56164e87"
	nodePlaintext  = "hello world 中文"
	nodeKey        = "test-encryption-key-1234567890"
)

func TestEncryptionRoundTripAndFormat(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", nodeKey)
	t.Setenv("JWT_SECRET", "unused-for-this-test")

	plaintext := "sensitive-value-中文-42"
	ciphertext, err := EncryptSensitiveData(plaintext)
	if err != nil {
		t.Fatalf("EncryptSensitiveData: %v", err)
	}
	if !IsEncrypted(ciphertext) {
		t.Fatalf("IsEncrypted(%q) = false, want true", ciphertext)
	}

	parts := strings.Split(ciphertext, ":")
	if len(parts) != 3 {
		t.Fatalf("ciphertext has %d parts, want 3", len(parts))
	}
	if len(parts[0]) != 32 || len(parts[1]) != 32 {
		t.Fatalf("iv/tag hex lengths = %d/%d, want 32/32", len(parts[0]), len(parts[1]))
	}
	if parts[2] == "" {
		t.Fatal("ciphertext body is empty")
	}

	if got := DecryptSensitiveData(ciphertext); got != plaintext {
		t.Fatalf("round-trip = %q, want %q", got, plaintext)
	}

	// Two encryptions of the same value must use different random IVs.
	other, err := EncryptSensitiveData(plaintext)
	if err != nil {
		t.Fatalf("EncryptSensitiveData: %v", err)
	}
	if other == ciphertext {
		t.Fatal("two encryptions produced identical ciphertext (IV not random?)")
	}
}

// TestDecryptNodeInterop proves Go can read ciphertext written by Node with the
// same key (the cross-backend compatibility contract).
func TestDecryptNodeInterop(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", nodeKey)
	if got := DecryptSensitiveData(nodeCiphertext); got != nodePlaintext {
		t.Fatalf("decrypt(Node vector) = %q, want %q", got, nodePlaintext)
	}
}

func TestDecryptFallbacks(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", nodeKey)

	// Legacy plaintext (not iv:tag:ct) is returned unchanged.
	if got := DecryptSensitiveData("plain-legacy-value"); got != "plain-legacy-value" {
		t.Fatalf("legacy passthrough = %q", got)
	}
	if IsEncrypted("plain-legacy-value") {
		t.Fatal("IsEncrypted must reject a value with no colons")
	}
	if IsEncrypted("aa:bb:cc") {
		t.Fatal("IsEncrypted must reject short iv/tag segments")
	}

	// Tampered ciphertext fails authentication and returns the original input.
	tampered := "000102030405060708090a0b0c0d0e0f:da8a4ce6471a4c6399bd8c6a8c63e731:ff928ae8374396ef09a6cf07cc3d56164e87"
	if got := DecryptSensitiveData(tampered); got != tampered {
		t.Fatalf("tampered decrypt = %q, want original input", got)
	}

	// Invalid hex in a 3-part value also falls back.
	if got := DecryptSensitiveData("zz:zz:zz"); got != "zz:zz:zz" {
		t.Fatalf("invalid hex fallback = %q", got)
	}
}

// ==================== 登录失败锁 ====================

func TestLoginLockLifecycle(t *testing.T) {
	const base = int64(1_700_000_000_000)

	withSecurityNow(base, func() {
		if status := CheckLoginLock("10.0.0.1", "alice"); status.Locked {
			t.Fatal("fresh key must not be locked")
		}

		// Four failures stay unlocked.
		for i := 0; i < 4; i++ {
			RecordLoginFailure("10.0.0.1", "alice")
		}
		if status := CheckLoginLock("10.0.0.1", "alice"); status.Locked {
			t.Fatal("4 failures must not lock")
		}

		// The fifth failure locks for 15 minutes.
		RecordLoginFailure("10.0.0.1", "alice")
		status := CheckLoginLock("10.0.0.1", "alice")
		if !status.Locked {
			t.Fatal("5 failures must lock")
		}
		if status.RetryAfter != 15*60 {
			t.Fatalf("retryAfter = %d, want 900", status.RetryAfter)
		}
	})

	// After the lock window elapses the entry is reset.
	withSecurityNow(base+15*60*1000+1, func() {
		if status := CheckLoginLock("10.0.0.1", "alice"); status.Locked {
			t.Fatal("lock must expire after 15 minutes")
		}
	})

	// A successful login clears the record.
	withSecurityNow(base, func() {
		for i := 0; i < 5; i++ {
			RecordLoginFailure("10.0.0.2", "bob")
		}
		if !CheckLoginLock("10.0.0.2", "bob").Locked {
			t.Fatal("expected locked before clear")
		}
		ClearLoginFailure("10.0.0.2", "bob")
		if CheckLoginLock("10.0.0.2", "bob").Locked {
			t.Fatal("clearLoginFailure must reset the lock")
		}
	})
}

func TestLoginLockAttemptWindowResets(t *testing.T) {
	const base = int64(1_700_000_000_000)

	withSecurityNow(base, func() {
		for i := 0; i < 4; i++ {
			RecordLoginFailure("10.0.0.3", "carol")
		}
	})

	// A failure after the 5-minute window starts a fresh cycle (count=1), so the
	// previous 4 attempts do not contribute.
	withSecurityNow(base+5*60*1000+1, func() {
		RecordLoginFailure("10.0.0.3", "carol")
		if CheckLoginLock("10.0.0.3", "carol").Locked {
			t.Fatal("attempts outside the window must not accumulate")
		}
	})
}

// TestLoginLockConcurrent drives many concurrent failures for the same key and
// asserts the lock still engages (mutex-protected shared state).
func TestLoginLockConcurrent(t *testing.T) {
	const base = int64(1_700_000_000_000)

	withSecurityNow(base, func() {
		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				RecordLoginFailure("10.9.9.9", "dave")
			}()
		}
		wg.Wait()

		if !CheckLoginLock("10.9.9.9", "dave").Locked {
			t.Fatal("concurrent failures must lock the key")
		}
	})
}

// ==================== 口令与输入校验 ====================

func TestValidatePasswordBoundaries(t *testing.T) {
	cases := []struct {
		password string
		valid    bool
		message  string
	}{
		{"Ab1", false, "Password must be at least 8 characters"},
		{"Abcdef1", false, "Password must be at least 8 characters"}, // 7 chars
		{"Abcdefg1", true, ""},                            // 8 chars
		{"Abcdefg1" + strings.Repeat("x", 120), true, ""}, // 128 chars
		{"Abcdefg1" + strings.Repeat("x", 121), false, "Password cannot exceed 128 characters"},
		{"abcdefg1", false, "Password must contain at least one uppercase letter"},
		{"ABCDEFG1", false, "Password must contain at least one lowercase letter"},
		{"Abcdefgh", false, "Password must contain at least one number"},
	}
	for _, tc := range cases {
		result := ValidatePassword(tc.password)
		if result.Valid != tc.valid {
			t.Errorf("ValidatePassword(len=%d) valid = %v, want %v", len(tc.password), result.Valid, tc.valid)
		}
		if result.Message != tc.message {
			t.Errorf("ValidatePassword(len=%d) message = %q, want %q", len(tc.password), result.Message, tc.message)
		}
	}
}

func TestDangerousCharsClass(t *testing.T) {
	forbidden := []string{"'", "\"", "\\", "/", "?", "[", "]", "=", "+", ".", "<", ">", "`", ";", ":", "|", "!", "@", "#", "$", "%", "^", "&", "*", "{", "}", "~"}
	for _, ch := range forbidden {
		if !ContainsDangerousChars("a" + ch + "b") {
			t.Errorf("ContainsDangerousChars must match %q", ch)
		}
		if got := RemoveDangerousChars("a" + ch + "b"); got != "ab" {
			t.Errorf("RemoveDangerousChars(%q) = %q, want %q", ch, got, "ab")
		}
	}

	// Allowed characters survive.
	allowed := "abcXYZ019 ( ),-_\u4e2d\u6587"
	if ContainsDangerousChars(allowed) {
		t.Fatalf("allowed string flagged as dangerous: %q", allowed)
	}
	if got := RemoveDangerousChars(allowed); got != allowed {
		t.Fatalf("RemoveDangerousChars removed allowed chars: %q", got)
	}
}

func TestValidateNameAndIdentifier(t *testing.T) {
	if result := ValidateName("", "Name", 1, 64); result.Valid || result.Message != "Name cannot be empty" {
		t.Fatalf("empty name = %+v", result)
	}
	if result := ValidateName("中文实例 1", "Name", 1, 64); !result.Valid || result.Sanitized == nil || *result.Sanitized != "中文实例 1" {
		t.Fatalf("chinese name = %+v", result)
	}
	if result := ValidateName("a", "Name", 2, 64); result.Valid || result.Message != "Name must be at least 2 characters" {
		t.Fatalf("min-length name = %+v", result)
	}
	if result := ValidateName(strings.Repeat("a", 65), "Name", 1, 64); result.Valid || result.Message != "Name cannot exceed 64 characters" {
		t.Fatalf("max-length name = %+v", result)
	}
	if result := ValidateName("bad$name", "Name", 1, 64); result.Valid || result.Message != "Name contains illegal characters" {
		t.Fatalf("dangerous name = %+v", result)
	}
	// "é" is neither dangerous nor in the safe-name alphabet, so it exercises the
	// SAFE_NAME_REGEX branch (every non-safe ASCII char is also a dangerous char).
	if result := ValidateName("name\u00e9", "Name", 1, 64); result.Valid || !strings.Contains(result.Message, "can only contain") {
		t.Fatalf("unsafe name = %+v", result)
	}

	if result := ValidateIdentifier("a", "Identifier", 2, 64); result.Valid {
		t.Fatal("identifier shorter than min must fail")
	}
	if result := ValidateIdentifier("_abc", "Identifier", 2, 64); result.Valid {
		t.Fatal("identifier must start with a letter or number")
	}
	if result := ValidateIdentifier("host-01_x", "Identifier", 2, 64); !result.Valid {
		t.Fatalf("valid identifier rejected: %+v", result)
	}
}

func TestValidateTextURLAndPath(t *testing.T) {
	empty := ValidateText("", "Content", 1000)
	if !empty.Valid || empty.Sanitized == nil || *empty.Sanitized != "" {
		t.Fatalf("empty text = %+v", empty)
	}
	over := ValidateText(strings.Repeat("x", 1001), "Content", 1000)
	if over.Valid || over.Message != "Content cannot exceed 1000 characters" {
		t.Fatalf("over-length text = %+v", over)
	}
	cleaned := ValidateText("  hello<script>  ", "Content", 1000)
	if !cleaned.Valid || cleaned.Sanitized == nil || *cleaned.Sanitized != "helloscript" {
		t.Fatalf("sanitized text = %+v", cleaned)
	}

	if result := ValidateURL("https://example.com/path", "URL"); !result.Valid {
		t.Fatalf("valid https URL rejected: %+v", result)
	}
	if result := ValidateURL("http://example.com", "URL"); !result.Valid {
		t.Fatalf("valid http URL rejected: %+v", result)
	}
	if result := ValidateURL("ftp://example.com", "URL"); result.Valid || result.Message != "URL must start with http:// or https://" {
		t.Fatalf("ftp URL = %+v", result)
	}
	if result := ValidateURL("not a url", "URL"); result.Valid {
		t.Fatal("garbage URL must fail")
	}
	if result := ValidateURL("", "URL"); result.Valid || result.Message != "URL cannot be empty" {
		t.Fatalf("empty URL = %+v", result)
	}

	if result := ValidateFilePath("/var/lib/incus/../etc/passwd", "path"); result.Valid {
		t.Fatal("path traversal must fail")
	}
	if result := ValidateFilePath("/var/lib/incus/backup.tar.gz", "path"); !result.Valid {
		t.Fatalf("valid path rejected: %+v", result)
	}
	if result := ValidateFilePath("/tmp/a;rm -rf", "path"); result.Valid {
		t.Fatal("dangerous path must fail")
	}
}

func TestValidateIPsAndDomains(t *testing.T) {
	if result := ValidateIPv4("192.168.1.1", "IP"); !result.Valid {
		t.Fatalf("valid IPv4 rejected: %+v", result)
	}
	if result := ValidateIPv4("999.1.1.1", "IP"); result.Valid {
		t.Fatal("invalid IPv4 accepted")
	}
	if result := ValidateIPv4("example.com", "IP"); result.Valid {
		t.Fatal("domain must not pass IPv4 validation")
	}

	if result := ValidateIPAddress("10.0.0.5", "IP"); !result.Valid {
		t.Fatalf("IPv4 rejected: %+v", result)
	}
	if result := ValidateIPAddress("[2001:db8::1]", "IP"); !result.Valid || result.Sanitized == nil || *result.Sanitized != "2001:db8::1" {
		t.Fatalf("bracketed IPv6 = %+v", result)
	}
	if result := ValidateIPAddress("nonsense", "IP"); result.Valid {
		t.Fatal("garbage must not pass IP validation")
	}

	if result := ValidateIPOrDomain("example.com", "Host"); !result.Valid {
		t.Fatalf("domain rejected: %+v", result)
	}
	if result := ValidateIPOrDomain("sub.example.co.uk", "Host"); !result.Valid {
		t.Fatalf("multi-level domain rejected: %+v", result)
	}
	if result := ValidateIPOrDomain("2001:db8::1", "Host"); !result.Valid {
		t.Fatalf("bare IPv6 rejected: %+v", result)
	}
	badLabel := strings.Repeat("a", 64) + ".com"
	if result := ValidateIPOrDomain(badLabel, "Host"); result.Valid {
		t.Fatal("oversized domain label must fail")
	}
}

func TestInstanceNameHelpers(t *testing.T) {
	if got := SanitizeInstanceName("my host!@#name"); got != "myhostname" {
		t.Fatalf("SanitizeInstanceName = %q", got)
	}
	if !IsValidInstanceName("host-01_x") {
		t.Fatal("valid instance name rejected")
	}
	if IsValidInstanceName("-host") || IsValidInstanceName("host name") {
		t.Fatal("invalid instance name accepted")
	}
}

func TestValidateInputsCollectsFirstFailure(t *testing.T) {
	result := ValidateInputs([]InputValidation{
		{Value: "ok", Validator: func() InputValidationResult { return validInput("ok") }},
		{Value: "", Validator: func() InputValidationResult { return invalidInput("bad") }},
		{Value: "ignored", Validator: func() InputValidationResult { return invalidInput("second") }},
	})
	if result.Valid || result.Message != "bad" {
		t.Fatalf("ValidateInputs = %+v", result)
	}

	all := ValidateInputs([]InputValidation{
		{Value: "a", Validator: func() InputValidationResult { return validInput("a") }},
	})
	if !all.Valid {
		t.Fatalf("all-valid ValidateInputs = %+v", all)
	}
}

// ==================== 2FA ====================

func Test2FAKeyURIAndSecret(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"

	// Exact otplib authenticator.keyuri output.
	want := "otpauth://totp/Incudal:alice?secret=JBSWY3DPEHPK3PXP&period=30&digits=6&algorithm=SHA1&issuer=Incudal"
	if got := Generate2FAKeyURI("alice", secret); got != want {
		t.Fatalf("Generate2FAKeyURI = %q, want %q", got, want)
	}
	// accountName is encodeURIComponent-encoded.
	wantEncoded := "otpauth://totp/Incudal:a%40b.com?secret=JBSWY3DPEHPK3PXP&period=30&digits=6&algorithm=SHA1&issuer=Incudal"
	if got := Generate2FAKeyURI("a@b.com", secret); got != wantEncoded {
		t.Fatalf("Generate2FAKeyURI(encoded) = %q, want %q", got, wantEncoded)
	}

	generated, err := Generate2FASecret()
	if err != nil {
		t.Fatalf("Generate2FASecret: %v", err)
	}
	if len(generated) != 16 {
		t.Fatalf("secret length = %d, want 16 (otplib default 10 bytes)", len(generated))
	}
	if _, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(generated); err != nil {
		t.Fatalf("secret is not valid base32: %v", err)
	}
}

func TestVerify2FATokenWindow(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXP"

	// A token for the current step verifies; retry once in case the 30s boundary
	// is crossed between generation and verification.
	verified := false
	current := ""
	for attempt := 0; attempt < 3 && !verified; attempt++ {
		code, err := totp.GenerateCode(secret, time.Now().UTC())
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}
		current = code
		verified = Verify2FAToken(code, secret)
	}
	if !verified {
		t.Fatal("current-step token must verify")
	}

	// A wrong token (one digit flipped) must not verify.
	wrong := "0" + current[1:]
	if wrong == current {
		wrong = "1" + current[1:]
	}
	if Verify2FAToken(wrong, secret) {
		t.Fatalf("flipped token %q must not verify", wrong)
	}

	// otplib window is 0: a token from three steps ago must be rejected.
	stale, err := totp.GenerateCode(secret, time.Now().UTC().Add(-90*time.Second))
	if err != nil {
		t.Fatalf("GenerateCode(stale): %v", err)
	}
	if Verify2FAToken(stale, secret) {
		t.Fatal("stale token must be rejected with window 0")
	}
}

func TestGenerateRecoveryCodes(t *testing.T) {
	codes, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	if len(codes) != DefaultRecoveryCodeCount {
		t.Fatalf("default count = %d, want %d", len(codes), DefaultRecoveryCodeCount)
	}
	pattern := regexp.MustCompile(`^[A-Z0-9]{4}-[A-Z0-9]{4}$`)
	seen := make(map[string]bool, len(codes))
	for _, code := range codes {
		if !pattern.MatchString(code) {
			t.Fatalf("recovery code %q does not match XXXX-XXXX", code)
		}
		seen[code] = true
	}
	if len(seen) != len(codes) {
		t.Fatalf("recovery codes are not unique: %v", codes)
	}

	explicit, err := GenerateRecoveryCodes(3)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes(3): %v", err)
	}
	if len(explicit) != 3 {
		t.Fatalf("explicit count = %d, want 3", len(explicit))
	}
}

// ==================== OAuth state / 一次性登录码 ====================

func TestOAuthStateRoundTripAndReplay(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-oauth-secret-1234567890")

	const base = int64(1_700_000_000_000)
	var state string
	withSecurityNow(base, func() {
		var err error
		state, err = GenerateOAuthState(OAuthStateModeLogin, "/dashboard", nil)
		if err != nil {
			t.Fatalf("GenerateOAuthState: %v", err)
		}
	})

	withSecurityNow(base+1000, func() {
		data := VerifyAndConsumeOAuthState(state)
		if data == nil {
			t.Fatal("valid state must verify")
		}
		if data.Mode != OAuthStateModeLogin || data.Redirect != "/dashboard" {
			t.Fatalf("state data = %+v", data)
		}
		if data.UserID != nil {
			t.Fatal("login-mode state must not carry a userId")
		}
		// Replay must be rejected.
		if VerifyAndConsumeOAuthState(state) != nil {
			t.Fatal("replayed state must be rejected")
		}
	})

	// Tampered signature.
	withSecurityNow(base+1000, func() {
		if VerifyAndConsumeOAuthState(state+"x") != nil {
			t.Fatal("tampered signature must be rejected")
		}
		if VerifyAndConsumeOAuthState("no-dot") != nil {
			t.Fatal("malformed state must be rejected")
		}
	})
}

func TestOAuthStateBindAndExpiry(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-oauth-secret-1234567890")

	const base = int64(1_700_000_000_000)
	sessionID := "abcd1234abcd1234abcd1234abcd1234"

	var state string
	withSecurityNow(base, func() {
		var err error
		state, err = GenerateOAuthState(OAuthStateModeBind, "/admin/profile", &OAuthBindSession{
			UserID:    7,
			IssuedAt:  base / 1000,
			SessionID: &sessionID,
		})
		if err != nil {
			t.Fatalf("GenerateOAuthState(bind): %v", err)
		}
	})

	withSecurityNow(base+1000, func() {
		data := VerifyAndConsumeOAuthState(state)
		if data == nil {
			t.Fatal("bind state must verify")
		}
		if data.Mode != OAuthStateModeBind || data.UserID == nil || *data.UserID != 7 {
			t.Fatalf("bind state = %+v", data)
		}
		if data.UserIssuedAt == nil || *data.UserIssuedAt != base/1000 {
			t.Fatalf("bind issuedAt = %v", data.UserIssuedAt)
		}
		if data.UserSessionID == nil || *data.UserSessionID != sessionID {
			t.Fatalf("bind sessionId = %v", data.UserSessionID)
		}
	})

	// Expired state (> 10 minutes) must be rejected.
	withSecurityNow(base, func() {
		expiring, err := GenerateOAuthState(OAuthStateModeLogin, "/", nil)
		if err != nil {
			t.Fatalf("GenerateOAuthState: %v", err)
		}
		withSecurityNow(base+10*60*1000+1, func() {
			if VerifyAndConsumeOAuthState(expiring) != nil {
				t.Fatal("expired state must be rejected")
			}
		})
	})
}

func TestOAuthLoginCode(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-oauth-secret-1234567890")

	const base = int64(1_700_000_000_000)
	var code string
	withSecurityNow(base, func() {
		var err error
		code, err = GenerateOAuthLoginCode(OAuthLoginCodeInput{UserID: 42, Username: "erin", Role: "user"})
		if err != nil {
			t.Fatalf("GenerateOAuthLoginCode: %v", err)
		}
	})

	withSecurityNow(base+1000, func() {
		data := VerifyAndConsumeOAuthLoginCode(code)
		if data == nil {
			t.Fatal("valid login code must verify")
		}
		if data.UserID != 42 || data.Username != "erin" || data.Role != "user" {
			t.Fatalf("login code data = %+v", data)
		}
		if VerifyAndConsumeOAuthLoginCode(code) != nil {
			t.Fatal("replayed login code must be rejected")
		}
	})

	// Expired after 60 seconds.
	withSecurityNow(base, func() {
		expiring, err := GenerateOAuthLoginCode(OAuthLoginCodeInput{UserID: 1, Username: "f", Role: "user"})
		if err != nil {
			t.Fatalf("GenerateOAuthLoginCode: %v", err)
		}
		withSecurityNow(base+60*1000+1, func() {
			if VerifyAndConsumeOAuthLoginCode(expiring) != nil {
				t.Fatal("expired login code must be rejected")
			}
		})
	})

	if VerifyAndConsumeOAuthLoginCode("garbage") != nil {
		t.Fatal("malformed login code must be rejected")
	}
}

// ==================== 导出任务 ====================

func TestExportTaskLifecycle(t *testing.T) {
	const base = int64(1_700_000_000_000)

	var id string
	withSecurityNow(base, func() {
		id = "task-1"
		task := CreateExportTask(CreateExportTaskInput{
			ID:              id,
			InstanceID:      5,
			BackupID:        9,
			IncusBackupName: "backup0",
			UserID:          3,
			Status:          ExportTaskStatusReady,
		})
		if task.ExpiresAt != base+exportTaskTTLMs {
			t.Fatalf("expiresAt = %d, want %d", task.ExpiresAt, base+exportTaskTTLMs)
		}

		got := GetExportTask(id)
		if got == nil || got.Status != ExportTaskStatusReady {
			t.Fatalf("GetExportTask = %+v", got)
		}

		if !UpdateExportTaskStatus(id, ExportTaskStatusDownloading, "") {
			t.Fatal("UpdateExportTaskStatus must succeed for a live task")
		}
		got = GetExportTask(id)
		if got == nil || got.Status != ExportTaskStatusDownloading {
			t.Fatalf("status after update = %+v", got)
		}

		if !UpdateExportTaskStatus(id, ExportTaskStatusError, "boom") {
			t.Fatal("error update must succeed")
		}
		got = GetExportTask(id)
		if got == nil || got.Error != "boom" {
			t.Fatalf("error after update = %+v", got)
		}

		if !DeleteExportTask(id) {
			t.Fatal("DeleteExportTask must report the task existed")
		}
		if GetExportTask(id) != nil {
			t.Fatal("deleted task must be gone")
		}
		if DeleteExportTask(id) {
			t.Fatal("second delete must report not found")
		}
	})

	// Expiry drops the task on read.
	withSecurityNow(base, func() {
		CreateExportTask(CreateExportTaskInput{ID: "task-2", Status: ExportTaskStatusCreating})
	})
	withSecurityNow(base+exportTaskTTLMs+1, func() {
		if GetExportTask("task-2") != nil {
			t.Fatal("expired task must be dropped")
		}
		if UpdateExportTaskStatus("task-2", ExportTaskStatusReady, "") {
			t.Fatal("expired task must not be updatable")
		}
	})
}

// ==================== Refresh token 助手 ====================

func TestRefreshTokenStorageKey(t *testing.T) {
	raw := "rt_plain-refresh-token-for-storage-test"
	key := GetRefreshTokenStorageKey(raw)
	if key == raw {
		t.Fatal("storage key must not equal the raw token")
	}
	if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(key) {
		t.Fatalf("storage key %q is not a sha256 digest", key)
	}
	if GetRefreshTokenStorageKey(key) != key {
		t.Fatal("hashed storage key must be idempotent")
	}

	sessionID := GetRefreshTokenSessionID(raw)
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(sessionID) {
		t.Fatalf("session id %q is not a 32-char digest prefix", sessionID)
	}
	if strings.HasPrefix(raw, sessionID) {
		t.Fatal("session id must not expose the raw token prefix")
	}

	keys := getRefreshTokenLookupKeys(raw)
	if len(keys) != 2 || keys[0] != key || keys[1] != raw {
		t.Fatalf("lookup keys = %v", keys)
	}
	if single := getRefreshTokenLookupKeys(key); len(single) != 1 || single[0] != key {
		t.Fatalf("hashed lookup keys = %v", single)
	}
}

// ==================== 异地登录 / 设备绑定 ====================

func TestNormalizeLoginUserAgent(t *testing.T) {
	chrome := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"
	if got := normalizeLoginUserAgent(chrome); got != "chrome|windows|desktop" {
		t.Fatalf("chrome fingerprint = %q", got)
	}

	// Edge UAs contain "Chrome" earlier in the string, so the leftmost match wins
	// (matching the JS regex) and Edge is classified as chrome.
	edge := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36 Edg/120.0"
	if got := normalizeLoginUserAgent(edge); got != "chrome|windows|desktop" {
		t.Fatalf("edge fingerprint = %q (leftmost match must win)", got)
	}

	iphone := "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
	// The iPhone UA also contains "like Mac OS X", and the OS checks run
	// macos-before-ios, so this is reported as safari|macos|mobile — matching the
	// TypeScript branch order.
	if got := normalizeLoginUserAgent(iphone); got != "safari|macos|mobile" {
		t.Fatalf("iphone fingerprint = %q", got)
	}
}

func TestNormalizeUserAgentForBindingAndValidate(t *testing.T) {
	chrome := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0 Safari/537.36"
	if got := normalizeUserAgentForBinding(chrome); got != "chrome|win10|d" {
		t.Fatalf("binding chrome fingerprint = %q", got)
	}
	if got := normalizeUserAgentForBinding(""); got != "unknown" {
		t.Fatalf("empty UA = %q, want unknown", got)
	}

	// checkUserAgent defaults on: mismatched fingerprints are rejected.
	result := ValidateDeviceBinding("1.2.3.4", chrome, "1.2.3.4", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Safari/604.1")
	if result.Valid {
		t.Fatalf("device fingerprint mismatch must be rejected: %+v", result)
	}
	if !strings.Contains(result.Reason, "Device fingerprint mismatch") {
		t.Fatalf("reason = %q", result.Reason)
	}

	// Same fingerprint passes (IP diff only matters when strict mode is enabled).
	if !deviceBindingStrictMode {
		if same := ValidateDeviceBinding("1.2.3.4", chrome, "9.9.9.9", chrome); !same.Valid {
			t.Fatalf("same fingerprint must pass when strict IP matching is off: %+v", same)
		}
	}
}
