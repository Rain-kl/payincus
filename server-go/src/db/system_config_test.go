package db

import (
	"strings"
	"testing"
	"time"
)

func TestIsoTimestampMatchesToISOString(t *testing.T) {
	value := time.Date(2026, 1, 29, 12, 34, 56, 789000000, time.UTC)
	if got := isoTimestamp(value); got != "2026-01-29T12:34:56.789Z" {
		t.Fatalf("isoTimestamp = %q, want 2026-01-29T12:34:56.789Z", got)
	}

	// Non-UTC input is converted, and a zero-millisecond value still gets three
	// fractional digits like Date#toISOString.
	zone := time.FixedZone("UTC+8", 8*60*60)
	shifted := time.Date(2026, 1, 29, 20, 34, 56, 0, zone)
	if got := isoTimestamp(shifted); got != "2026-01-29T12:34:56.000Z" {
		t.Fatalf("isoTimestamp = %q, want 2026-01-29T12:34:56.000Z", got)
	}
}

// The numeric namespace values are a cross-process contract shared with
// server/src/db/advisory-locks.ts; a drift here silently breaks mutual exclusion.
func TestAdvisoryLockNamespacesMatchTypeScript(t *testing.T) {
	expected := map[string]int{
		"HOST_TASK_CLAIM_LOCK_NAMESPACE":                HostTaskClaimLockNamespace,
		"HOST_NOTIFICATION_EMAIL_LOCK_NAMESPACE":        HostNotificationEmailLockNamespace,
		"HOST_NOTIFICATION_EMAIL_LOCK_KEY":              HostNotificationEmailLockKey,
		"TRANSFER_CREATE_LOCK_NAMESPACE":                TransferCreateLockNamespace,
		"REDEEM_CODE_LOCK_NAMESPACE":                    RedeemCodeLockNamespace,
		"INSTANCE_OPERATION_LOCK_NAMESPACE":             InstanceOperationLockNamespace,
		"USER_DESTROY_BILLING_LOCK_NAMESPACE":           UserDestroyBillingLockNamespace,
		"USER_ADMIN_ROLE_LOCK_NAMESPACE":                UserAdminRoleLockNamespace,
		"MAIL_SUBSCRIPTION_LOCK_NAMESPACE":              MailSubscriptionLockNamespace,
		"USER_BALANCE_LOCK_NAMESPACE":                   UserBalanceLockNamespace,
		"USER_POINTS_LOCK_NAMESPACE":                    UserPointsLockNamespace,
		"USER_AFF_BALANCE_LOCK_NAMESPACE":               UserAffBalanceLockNamespace,
		"USER_BACKUP_UPLOAD_LOCK_NAMESPACE":             UserBackupUploadLockNamespace,
		"USER_RESOURCE_POOL_LOCK_NAMESPACE":             UserResourcePoolLockNamespace,
		"USER_VIP_BENEFIT_CLAIM_LOCK_NAMESPACE":         UserVipBenefitClaimLockNamespace,
		"MAIL_DOMAIN_LOCK_NAMESPACE":                    MailDomainLockNamespace,
		"USER_EMAIL_REGISTRATION_LOCK_NAMESPACE":        UserEmailRegistrationLockNamespace,
		"OPERATION_VERIFICATION_REQUEST_LOCK_NAMESPACE": OperationVerificationRequestLockNamespace,
		"IP_ADDRESS_ALLOCATION_LOCK_NAMESPACE":          IPAddressAllocationLockNamespace,
		"GIFT_CARD_LOCK_NAMESPACE":                      GiftCardLockNamespace,
		"INSTANCE_TRAFFIC_RESET_LOCK_NAMESPACE":         InstanceTrafficResetLockNamespace,
	}
	want := map[string]int{
		"HOST_TASK_CLAIM_LOCK_NAMESPACE":                4101,
		"HOST_NOTIFICATION_EMAIL_LOCK_NAMESPACE":        4102,
		"HOST_NOTIFICATION_EMAIL_LOCK_KEY":              1,
		"TRANSFER_CREATE_LOCK_NAMESPACE":                4103,
		"REDEEM_CODE_LOCK_NAMESPACE":                    4104,
		"INSTANCE_OPERATION_LOCK_NAMESPACE":             4106,
		"USER_DESTROY_BILLING_LOCK_NAMESPACE":           4107,
		"USER_ADMIN_ROLE_LOCK_NAMESPACE":                4108,
		"MAIL_SUBSCRIPTION_LOCK_NAMESPACE":              4109,
		"USER_BALANCE_LOCK_NAMESPACE":                   4110,
		"USER_POINTS_LOCK_NAMESPACE":                    4111,
		"USER_AFF_BALANCE_LOCK_NAMESPACE":               4112,
		"USER_BACKUP_UPLOAD_LOCK_NAMESPACE":             4113,
		"USER_RESOURCE_POOL_LOCK_NAMESPACE":             4114,
		"USER_VIP_BENEFIT_CLAIM_LOCK_NAMESPACE":         4115,
		"MAIL_DOMAIN_LOCK_NAMESPACE":                    4116,
		"USER_EMAIL_REGISTRATION_LOCK_NAMESPACE":        4117,
		"OPERATION_VERIFICATION_REQUEST_LOCK_NAMESPACE": 4118,
		"IP_ADDRESS_ALLOCATION_LOCK_NAMESPACE":          4119,
		"GIFT_CARD_LOCK_NAMESPACE":                      4120,
		"INSTANCE_TRAFFIC_RESET_LOCK_NAMESPACE":         4121,
	}

	if len(expected) != len(want) {
		t.Fatalf("namespace count = %d, want %d", len(expected), len(want))
	}
	for name, value := range expected {
		if wantValue, ok := want[name]; !ok {
			t.Fatalf("unexpected namespace %s", name)
		} else if value != wantValue {
			t.Errorf("%s = %d, want %d", name, value, wantValue)
		}
	}

	seen := map[int]string{}
	for name, value := range expected {
		if name == "HOST_NOTIFICATION_EMAIL_LOCK_KEY" {
			continue
		}
		if previous, ok := seen[value]; ok {
			t.Errorf("namespaces %s and %s collide on %d", previous, name, value)
		}
		seen[value] = name
	}
}

func TestSensitiveConfigKeys(t *testing.T) {
	if len(sensitiveConfigKeys) != 5 {
		t.Fatalf("sensitive key count = %d, want 5", len(sensitiveConfigKeys))
	}
	for _, key := range []string{
		"smtp_password",
		"turnstile_secret_key",
		"ticket_image_lsky_token",
		"telegram_bot_token",
		"telegram_webhook_secret",
	} {
		if !sensitiveConfigKeys[key] {
			t.Errorf("%s should be sensitive", key)
		}
	}
	if sensitiveConfigKeys["brand_name"] {
		t.Error("brand_name must not be treated as sensitive")
	}
	if maskedSecretPlaceholder != "********" {
		t.Fatalf("maskedSecretPlaceholder = %q, want ********", maskedSecretPlaceholder)
	}
}

func TestDefaultSystemConfigSeeds(t *testing.T) {
	if len(defaultSystemConfigs) != 64 {
		t.Fatalf("seed count = %d, want 64", len(defaultSystemConfigs))
	}
	seen := map[string]bool{}
	for _, seed := range defaultSystemConfigs {
		if seed.key == "" {
			t.Fatal("seed with empty key")
		}
		if seen[seed.key] {
			t.Fatalf("duplicate seed key %q", seed.key)
		}
		seen[seed.key] = true
		if seed.configType == "" {
			t.Fatalf("seed %q has no type", seed.key)
		}
	}
	// Spot-check a few well-known defaults against the TypeScript source.
	byKey := map[string]systemConfigSeed{}
	for _, seed := range defaultSystemConfigs {
		byKey[seed.key] = seed
	}
	if byKey["require_invite_code"].value != "true" {
		t.Errorf("require_invite_code default = %q, want true", byKey["require_invite_code"].value)
	}
	if byKey["default_quota_host"].value != "0" {
		t.Errorf("default_quota_host default = %q, want 0", byKey["default_quota_host"].value)
	}
	if byKey["brand_name"].value != "Incudal" {
		t.Errorf("brand_name default = %q, want Incudal", byKey["brand_name"].value)
	}
	if byKey["smtp_port"].value != "587" {
		t.Errorf("smtp_port default = %q, want 587", byKey["smtp_port"].value)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key")
	t.Setenv("NODE_ENV", "test")

	plaintext := "s3cr3t-token-值"
	ciphertext, err := encryptSensitiveData(plaintext)
	if err != nil {
		t.Fatalf("encryptSensitiveData: %v", err)
	}
	if ciphertext == plaintext {
		t.Fatal("ciphertext must differ from plaintext")
	}
	parts := strings.Split(ciphertext, ":")
	if len(parts) != 3 {
		t.Fatalf("ciphertext format = %q, want iv:tag:payload", ciphertext)
	}
	if len(parts[0]) != encryptionIVLength*2 {
		t.Errorf("iv hex length = %d, want %d", len(parts[0]), encryptionIVLength*2)
	}
	if len(parts[1]) != 32 {
		t.Errorf("gcm tag hex length = %d, want 32", len(parts[1]))
	}

	if got := decryptSensitiveData(ciphertext); got != plaintext {
		t.Fatalf("decryptSensitiveData = %q, want %q", got, plaintext)
	}
}

func TestDecryptSensitiveDataReturnsPlaintextUnchanged(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", "unit-test-encryption-key")

	// Legacy plaintext (no colons) and malformed ciphertext are returned as-is,
	// mirroring decryptSensitiveData's never-throw contract.
	for _, input := range []string{
		"not-encrypted",
		"a:b",
		"zz:zz:zz",
		"",
	} {
		if got := decryptSensitiveData(input); got != input {
			t.Errorf("decryptSensitiveData(%q) = %q, want unchanged", input, got)
		}
	}
}

func TestParseIntJS(t *testing.T) {
	cases := []struct {
		input string
		want  int64
		ok    bool
	}{
		{"0", 0, true},
		{"42", 42, true},
		{" 42 ", 42, true},
		{"-7", -7, true},
		{"+7", 7, true},
		{"12abc", 12, true},
		{"1.9", 1, true},
		{"08", 8, true},
		{"abc", 0, false},
		{"", 0, false},
		{"-", 0, false},
		{"   ", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseIntJS(tc.input)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseIntJS(%q) = (%d, %v), want (%d, %v)", tc.input, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseFloatJS(t *testing.T) {
	cases := []struct {
		input string
		want  float64
		ok    bool
	}{
		{"3.14", 3.14, true},
		{"3.14xyz", 3.14, true},
		{".5", 0.5, true},
		{"-0.25", -0.25, true},
		{"1e3", 1000, true},
		{"2.5E-2", 0.025, true},
		{"1e", 1, true},
		{"abc", 0, false},
		{"", 0, false},
		{".", 0, false},
		{"Infinity", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseFloatJS(tc.input)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseFloatJS(%q) = (%v, %v), want (%v, %v)", tc.input, got, ok, tc.want, tc.ok)
		}
	}
}
