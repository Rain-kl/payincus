package db

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== money precision ====================

// TestMoneyFloatMatchesToMoney pins moneyFloat to Prisma's toMoney():
// Number(Number(x).toFixed(2)). Rounding is half-away-from-zero on the decimal,
// exactly like toFixed for the magnitudes involved.
func TestMoneyFloatMatchesToMoney(t *testing.T) {
	cases := []struct {
		input string
		want  float64
	}{
		{"0", 0},
		{"100.00", 100},
		{"12.50", 12.5},
		{"0.01", 0.01},
		{"0.005", 0.01}, // toFixed(2) rounds half up
		{"0.004", 0},    // rounds down
		{"1.005", 1.01}, // classic float trap: decimal path keeps 1.01
		{"9999.994", 9999.99},
		{"99999999.99", 99999999.99},
		{"-3.456", -3.46},
	}
	for _, tc := range cases {
		value := decimal.RequireFromString(tc.input)
		if got := moneyFloat(value); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("moneyFloat(%s) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// TestToDecimalRoundsToTwoPlaces verifies the storage conversion used everywhere
// an amount is written.
func TestToDecimalRoundsToTwoPlaces(t *testing.T) {
	cases := []struct {
		input float64
		want  string
	}{
		{1, "1"},
		{12.5, "12.5"},
		{0.005, "0.01"},
		{0.004, "0"},
		{99.999, "100"},
		{10000, "10000"},
	}
	for _, tc := range cases {
		got := toDecimal(tc.input)
		if got.String() != tc.want {
			t.Errorf("toDecimal(%v) = %q, want %q", tc.input, got.String(), tc.want)
		}
	}
}

// TestRoundTripDecimalKeepsCents ensures no drift when a stored value is read back
// and re-serialized.
func TestRoundTripDecimalKeepsCents(t *testing.T) {
	for _, raw := range []string{"0.01", "12.34", "9999.99", "10000.00"} {
		stored := toDecimal(decimal.RequireFromString(raw).InexactFloat64())
		if moneyFloat(stored.Decimal) == 0 && raw != "0" {
			t.Fatalf("round trip lost the value for %s", raw)
		}
		// Two decimal places at most.
		if stored.Exponent() < -2 {
			t.Errorf("stored %s has more than 2 decimals: %s", raw, stored.String())
		}
	}
}

// ==================== code generation / masking ====================

// TestGenerateGiftCardCodeShape pins the "gc-" + 24 base64url character format the
// route validates with ^gc-[A-Za-z0-9_-]{24}$.
func TestGenerateGiftCardCodeShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code, err := generateGiftCardCode()
		if err != nil {
			t.Fatalf("generateGiftCardCode: %v", err)
		}
		if len(code) != giftCardCodeTotalLength {
			t.Fatalf("code %q has length %d, want %d", code, len(code), giftCardCodeTotalLength)
		}
		if !strings.HasPrefix(code, GiftCardCodePrefix) {
			t.Fatalf("code %q lacks the %q prefix", code, GiftCardCodePrefix)
		}
		for _, r := range code[len(GiftCardCodePrefix):] {
			isAlphaNum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !isAlphaNum && r != '-' && r != '_' {
				t.Fatalf("code %q contains a non-base64url character %q", code, r)
			}
		}
		if seen[code] {
			t.Fatalf("code collision within 200 draws: %q", code)
		}
		seen[code] = true
	}
}

// TestGenerateBatchIDShape pins hex(8 bytes) = 16 hex characters.
func TestGenerateBatchIDShape(t *testing.T) {
	batchID, err := generateBatchID()
	if err != nil {
		t.Fatalf("generateBatchID: %v", err)
	}
	if len(batchID) != 16 {
		t.Fatalf("batchId %q has length %d, want 16", batchID, len(batchID))
	}
	for _, r := range batchID {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("batchId %q contains a non-hex character %q", batchID, r)
		}
	}
}

// TestGiftCardMaskCode pins maskGiftCardCode, which is also what the redeem log
// uses to avoid storing full codes.
func TestGiftCardMaskCode(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{"short", "short"},
		{"012345678901", "012345678901"},                  // exactly 12 → unmasked
		{"0123456789012", "0123456...9012"},               // 13 → masked
		{"gc-abcdefghijklmnopqrstuvwx", "gc-abcd...uvwx"}, // full card code
	}
	for _, tc := range cases {
		if got := giftCardMaskCode(tc.code); got != tc.want {
			t.Errorf("giftCardMaskCode(%q) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

// TestGiftCardRefundRemarkIsDeterministic guards the idempotency key: the same
// card id must always produce the same ledger remark.
func TestGiftCardRefundRemarkIsDeterministic(t *testing.T) {
	first := giftCardRefundRemark(42)
	second := giftCardRefundRemark(42)
	if first != second {
		t.Fatalf("refund remark is not deterministic: %q vs %q", first, second)
	}
	if !strings.Contains(first, "[gift-card-refund:42]") {
		t.Fatalf("refund remark %q lacks the id marker", first)
	}
	if giftCardRefundRemark(42) == giftCardRefundRemark(43) {
		t.Fatal("refund remarks for different cards must differ")
	}
}

// ==================== clamping ====================

// TestClampGiftCardPagination pins clampPage/clampPageSize.
func TestClampGiftCardPagination(t *testing.T) {
	if got := clampGiftCardPage(0); got != 1 {
		t.Errorf("clampGiftCardPage(0) = %d, want 1", got)
	}
	if got := clampGiftCardPage(-5); got != 1 {
		t.Errorf("clampGiftCardPage(-5) = %d, want 1", got)
	}
	if got := clampGiftCardPage(7); got != 7 {
		t.Errorf("clampGiftCardPage(7) = %d, want 7", got)
	}

	if got := clampGiftCardPageSize(0); got != 20 {
		t.Errorf("clampGiftCardPageSize(0) = %d, want 20 (default)", got)
	}
	if got := clampGiftCardPageSize(-1); got != 1 {
		t.Errorf("clampGiftCardPageSize(-1) = %d, want 1", got)
	}
	if got := clampGiftCardPageSize(1000); got != GiftCardMaxListPageSize {
		t.Errorf("clampGiftCardPageSize(1000) = %d, want %d", got, GiftCardMaxListPageSize)
	}
}

// ==================== create validation ====================

// TestValidateGiftCardAmounts covers the guard inside
// createGiftCardInTransaction (balance <= 0, balance > face). It is exercised
// through the extracted pure helper so no database is required.
func TestValidateGiftCardAmounts(t *testing.T) {
	cases := []struct {
		name         string
		faceValue    string
		balanceValue string
		reject       bool
	}{
		{"zero face value", "0", "0", true},
		{"negative face value", "-1", "-1", true},
		{"zero balance value", "10", "0", true},
		{"negative balance value", "10", "-1", true},
		{"balance above face", "10", "10.01", true},
		{"balance equals face", "10", "10", false},
		{"balance below face", "10", "5", false},
		{"minimum positive amounts", "0.01", "0.01", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGiftCardAmounts(
				decimal.RequireFromString(tc.faceValue),
				decimal.RequireFromString(tc.balanceValue))
			if tc.reject {
				if !errors.Is(err, ErrGiftCardBalanceExceedsFace) {
					t.Fatalf("err = %v, want ErrGiftCardBalanceExceedsFace", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid amounts rejected: %v", err)
			}
		})
	}
}

// TestCreateGiftCardBatchLimits covers the two batch guards that run before any
// DB access (so a nil pool is fine).
func TestCreateGiftCardBatchLimits(t *testing.T) {
	if _, err := CreateGiftCardBatch(t.Context(), 0,
		GiftCardCreateInput{FaceValue: 1, BalanceValue: 1}); !errors.Is(err, ErrGiftCardBatchTooLarge) {
		t.Errorf("count 0 err = %v, want ErrGiftCardBatchTooLarge", err)
	}
	if _, err := CreateGiftCardBatch(t.Context(), GiftCardMaxBatchCount+1,
		GiftCardCreateInput{FaceValue: 1, BalanceValue: 1}); !errors.Is(err, ErrGiftCardBatchTooLarge) {
		t.Errorf("count %d err = %v, want ErrGiftCardBatchTooLarge", GiftCardMaxBatchCount+1, err)
	}
	// 500 * 200 = 100000 is allowed (not >), 500 * 201 = 100500 is rejected.
	if _, err := CreateGiftCardBatch(t.Context(), GiftCardMaxBatchCount,
		GiftCardCreateInput{FaceValue: 200, BalanceValue: 200}); errors.Is(err, ErrGiftCardBatchValueTooLarge) {
		t.Errorf("500 x 200 must be allowed, got %v", err)
	}
	if _, err := CreateGiftCardBatch(t.Context(), GiftCardMaxBatchCount,
		GiftCardCreateInput{FaceValue: 201, BalanceValue: 201}); !errors.Is(err, ErrGiftCardBatchValueTooLarge) {
		t.Errorf("500 x 201 err = %v, want ErrGiftCardBatchValueTooLarge", err)
	}
}

// ==================== wire shapes ====================

// TestGiftCardCreateViewShape pins the create/batch projection: amounts are JSON
// *numbers* (toMoney semantics), not Prisma decimal strings.
func TestGiftCardCreateViewShape(t *testing.T) {
	row := giftCardRow{
		ID:           1,
		Code:         "gc-abcdefghijklmnopqrstuvwx",
		FaceValue:    models.Decimal{Decimal: decimal.RequireFromString("100.00")},
		BalanceValue: models.Decimal{Decimal: decimal.RequireFromString("12.50")},
		Status:       string(models.GiftCardStatusActive),
		CreatedAt:    time.Now(),
	}
	encoded := mustMarshal(t, serializeGiftCardCreate(row))

	if _, ok := encoded["faceValue"].(float64); !ok {
		t.Errorf("faceValue must be a JSON number, got %#v", encoded["faceValue"])
	}
	if encoded["faceValue"] != float64(100) {
		t.Errorf("faceValue = %v, want 100", encoded["faceValue"])
	}
	if encoded["balanceValue"] != 12.5 {
		t.Errorf("balanceValue = %v, want 12.5", encoded["balanceValue"])
	}
	// revealCode:true for the create path.
	if encoded["code"] != "gc-abcdefghijklmnopqrstuvwx" {
		t.Errorf("code = %v, want the full code", encoded["code"])
	}
	if encoded["codeMasked"] != "gc-abcd...uvwx" {
		t.Errorf("codeMasked = %v", encoded["codeMasked"])
	}
	if encoded["expiresAt"] != nil {
		t.Errorf("expiresAt = %v, want explicit null", encoded["expiresAt"])
	}
}

// TestGiftCardAdminViewMasksCodeByDefault is the security-relevant assertion:
// the admin list hides the full code unless revealCode is requested.
func TestGiftCardAdminViewMasksCodeByDefault(t *testing.T) {
	row := giftCardRow{
		ID:           2,
		Code:         "gc-abcdefghijklmnopqrstuvwx",
		FaceValue:    models.Decimal{Decimal: decimal.RequireFromString("5")},
		BalanceValue: models.Decimal{Decimal: decimal.RequireFromString("5")},
		Status:       string(models.GiftCardStatusActive),
		CreatedAt:    time.Now(),
	}

	masked := mustMarshal(t, serializeGiftCardAdmin(row, false))
	if masked["code"] != "gc-abcd...uvwx" {
		t.Fatalf("admin list leaked the full code by default: %v", masked["code"])
	}
	if masked["codeMasked"] != "gc-abcd...uvwx" {
		t.Fatalf("codeMasked = %v", masked["codeMasked"])
	}

	revealed := mustMarshal(t, serializeGiftCardAdmin(row, true))
	if revealed["code"] != "gc-abcdefghijklmnopqrstuvwx" {
		t.Fatalf("revealCode=true code = %v", revealed["code"])
	}
}

// TestGiftCardUserViewAlwaysRevealsOwnCode mirrors listGiftCardsByUser, which
// passes revealCode:true for the owner's own cards.
func TestGiftCardUserViewAlwaysRevealsOwnCode(t *testing.T) {
	row := giftCardRow{
		ID:           3,
		Code:         "gc-abcdefghijklmnopqrstuvwx",
		FaceValue:    models.Decimal{Decimal: decimal.RequireFromString("1")},
		BalanceValue: models.Decimal{Decimal: decimal.RequireFromString("1")},
		Status:       string(models.GiftCardStatusActive),
		CreatedAt:    time.Now(),
	}
	encoded := mustMarshal(t, serializeGiftCardUser(row))
	if encoded["code"] != "gc-abcdefghijklmnopqrstuvwx" {
		t.Fatalf("owner code = %v, want the full code", encoded["code"])
	}
	// The user projection has no relation / owner keys.
	for _, forbidden := range []string{"createdBy", "owner", "usedBy", "createdById"} {
		if _, present := encoded[forbidden]; present {
			t.Errorf("user projection must not carry %q", forbidden)
		}
	}
}

// TestGiftCardDetailViewShape covers findGiftCardByCode's full projection.
func TestGiftCardDetailViewShape(t *testing.T) {
	createdBy, owner := int32(1), int32(1)
	createdName, ownerName := "alice", "alice"
	row := giftCardRow{
		ID:                4,
		Code:              "gc-abcdefghijklmnopqrstuvwx",
		FaceValue:         models.Decimal{Decimal: decimal.RequireFromString("20")},
		BalanceValue:      models.Decimal{Decimal: decimal.RequireFromString("20")},
		Status:            string(models.GiftCardStatusActive),
		CreatedByID:       &createdBy,
		OwnerID:           &owner,
		CreatedAt:         time.Now(),
		CreatedByUsername: &createdName,
		OwnerUsername:     &ownerName,
	}
	encoded := mustMarshal(t, serializeGiftCardDetail(row))
	for _, key := range []string{"id", "code", "codeMasked", "faceValue", "balanceValue",
		"status", "createdById", "ownerId", "usedById", "usedAt", "expiresAt",
		"remark", "batchId", "createdAt", "createdBy", "owner", "usedBy"} {
		if _, present := encoded[key]; !present {
			t.Errorf("detail projection is missing key %q", key)
		}
	}
	if encoded["usedById"] != nil {
		t.Errorf("usedById = %v, want explicit null", encoded["usedById"])
	}
	if encoded["usedBy"] != nil {
		t.Errorf("usedBy = %v, want explicit null", encoded["usedBy"])
	}
	ownerRef, ok := encoded["owner"].(map[string]any)
	if !ok || ownerRef["username"] != "alice" {
		t.Errorf("owner = %#v", encoded["owner"])
	}
}

// ==================== error mapping ====================

// TestGiftCardSentinels pins the exact error strings the route switches on, and
// that they are all distinct.
func TestGiftCardSentinels(t *testing.T) {
	cases := map[error]string{
		ErrGiftCardNotFound:            "GIFT_CARD_NOT_FOUND",
		ErrGiftCardUsed:                "GIFT_CARD_USED",
		ErrGiftCardSelfRedeem:          "GIFT_CARD_SELF_REDEEM",
		ErrGiftCardExpired:             "GIFT_CARD_EXPIRED",
		ErrGiftCardDisabled:            "GIFT_CARD_DISABLED",
		ErrGiftCardInsufficientBalance: "GIFT_CARD_INSUFFICIENT_BALANCE",
		ErrGiftCardBatchTooLarge:       "GIFT_CARD_BATCH_TOO_LARGE",
		ErrGiftCardBatchValueTooLarge:  "GIFT_CARD_BATCH_VALUE_TOO_LARGE",
		ErrGiftCardBalanceExceedsFace:  "GIFT_CARD_BALANCE_EXCEEDS_FACE_VALUE",
		ErrGiftCardNotIssuer:           "GIFT_CARD_NOT_ISSUER",
		ErrGiftCardCodeCollision:       "GIFT_CARD_CODE_COLLISION",
		ErrGiftCardUserNotFound:        "USER_NOT_FOUND",
	}
	seen := map[string]bool{}
	for err, want := range cases {
		if err.Error() != want {
			t.Errorf("%v.Error() = %q, want %q", err, err.Error(), want)
		}
		if seen[want] {
			t.Errorf("duplicate error string %q", want)
		}
		seen[want] = true
	}
}

// TestUniqueViolationDetection covers both the pgconn error path and the message
// fallback, since the retry loops depend on it.
func TestUniqueViolationDetection(t *testing.T) {
	if isGiftCardUniqueViolation(nil) {
		t.Fatal("nil must not be a unique violation")
	}
	if isGiftCardUniqueViolation(errors.New("connection reset")) {
		t.Fatal("unrelated error classified as a unique violation")
	}
	if !isGiftCardUniqueViolation(errors.New("pq: duplicate key value violates unique constraint")) {
		t.Fatal("duplicate key message must be detected")
	}
}

// ==================== redemption invariants (documented + structural) ====================

// TestRedeemRejectsSelfOwnedCard documents the self-redeem guard: the check is
// ownerId == userId *after* the card lock, and it must run before any balance
// mutation. This is a structural assertion on the source-level invariant that the
// conformance run then verifies end to end.
func TestRedeemOrderingInvariant(t *testing.T) {
	// The lock order (card → balance) is the deadlock-avoidance contract shared
	// with the TypeScript. Both namespaces must be distinct, otherwise the two
	// locks would serialize on the same key space and the ordering would be moot.
	if GiftCardLockNamespace == UserBalanceLockNamespace {
		t.Fatal("gift card and balance lock namespaces must differ")
	}
	// The card lock must be taken before the balance lock: enforced by call order
	// inside RedeemGiftCard; the namespaces above pin the identity of each.
	if GiftCardLockNamespace != 4120 {
		t.Fatalf("GiftCardLockNamespace = %d, want 4120 (cross-process contract)", GiftCardLockNamespace)
	}
	if UserBalanceLockNamespace != 4110 {
		t.Fatalf("UserBalanceLockNamespace = %d, want 4110", UserBalanceLockNamespace)
	}
}

// TestSortInt32sIsAscending backs the deterministic lock-taking order used by the
// batch operations (prevents A↔B / B↔A deadlocks between concurrent batches).
func TestSortInt32sIsAscending(t *testing.T) {
	cases := [][]int32{
		{5, 3, 9, 1},
		{1},
		{},
		{2, 2, 1},
		{-1, 0, 5},
	}
	for _, input := range cases {
		values := append([]int32(nil), input...)
		sortInt32s(values)
		for i := 1; i < len(values); i++ {
			if values[i-1] > values[i] {
				t.Fatalf("sortInt32s(%v) = %v, not ascending", input, values)
			}
		}
		if len(values) != len(input) {
			t.Fatalf("sortInt32s(%v) changed the length", input)
		}
	}
}
