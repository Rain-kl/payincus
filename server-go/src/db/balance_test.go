package db

import (
	"encoding/json"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// The relation fields on BalanceAdjustmentRequestWithRelations must shadow the
// identically named `gorm:"-"` fields on the embedded model, otherwise the JSON
// shape would silently diverge from Prisma's `include` projection.
func TestBalanceAdjustmentRequestWithRelationsJSONShape(t *testing.T) {
	email := "user@example.com"
	item := BalanceAdjustmentRequestWithRelations{
		BalanceAdjustmentRequest: models.BalanceAdjustmentRequest{
			ID:     7,
			UserID: 1,
			Amount: mustDecimal(t, "12.50"),
			Status: models.BalanceAdjustmentRequestStatusPending,
		},
		User:        &BalanceAdjustmentUserRef{ID: 1, Username: "alice", Email: &email},
		RequestedBy: &BalanceAdjustmentUserRef{ID: 2, Username: "admin"},
	}

	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Scalar fields from the embedded model survive.
	if decoded["id"] != float64(7) {
		t.Errorf("id = %v, want 7", decoded["id"])
	}
	// models.Decimal marshals as a JSON string (trailing zeros trimmed by
	// shopspring's String()).
	if amount, ok := decoded["amount"].(string); !ok || amount != "12.5" {
		t.Errorf("amount = %#v, want the JSON string \"12.5\"", decoded["amount"])
	}

	// Relations come from the outer struct, including explicit nulls.
	user, ok := decoded["user"].(map[string]any)
	if !ok || user["username"] != "alice" {
		t.Fatalf("user = %v, want the outer relation", decoded["user"])
	}
	requestedBy, ok := decoded["requestedBy"].(map[string]any)
	if !ok || requestedBy["username"] != "admin" {
		t.Fatalf("requestedBy = %v, want the outer relation", decoded["requestedBy"])
	}
	if value, present := decoded["reviewedBy"]; !present || value != nil {
		t.Errorf("reviewedBy = %v (present=%v), want an explicit null", value, present)
	}
	if value, present := decoded["balanceLog"]; !present || value != nil {
		t.Errorf("balanceLog = %v (present=%v), want an explicit null", value, present)
	}
}

func TestBalanceLogWithInstanceJSONShape(t *testing.T) {
	instanceID := int32(9)
	item := BalanceLogWithInstance{
		BalanceLog: models.BalanceLog{
			ID:         3,
			UserID:     1,
			Type:       models.BalanceLogTypeConsume,
			Amount:     mustDecimal(t, "-4.20"),
			InstanceID: &instanceID,
		},
		Instance: &BalanceInstanceRef{ID: instanceID, Name: "vm-1"},
	}

	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["type"] != "consume" {
		t.Errorf("type = %v, want consume", decoded["type"])
	}
	if amount, ok := decoded["amount"].(string); !ok || amount != "-4.2" {
		t.Errorf("amount = %#v, want the JSON string \"-4.2\"", decoded["amount"])
	}
	instance, ok := decoded["instance"].(map[string]any)
	if !ok || instance["name"] != "vm-1" {
		t.Fatalf("instance = %v, want {id:9,name:vm-1}", decoded["instance"])
	}
	// Relation fields on the embedded model are empty and omitted.
	if _, present := decoded["user"]; present {
		t.Errorf("user should be omitted, got %v", decoded["user"])
	}
}

func mustDecimal(t *testing.T, value string) models.Decimal {
	t.Helper()
	parsed, err := models.NewDecimalFromString(value)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", value, err)
	}
	return parsed
}

func TestNormalizeBalanceAmount(t *testing.T) {
	cases := []struct {
		input string
		want  string
		ok    bool
	}{
		{"12.345", "12.35", true},            // toFixed(2) rounding
		{"-5.5", "-5.5", true},               // negatives are allowed
		{"99999999.99", "99999999.99", true}, // exactly the cap is allowed
		{"0", "", false},                     // zero is rejected
		{"0.001", "", false},                 // rounds to zero, still rejected
		{"-0.004", "", false},                // rounds to zero, still rejected
		{"100000000", "", false},             // above MAX_BALANCE_AMOUNT
		{"-100000000.01", "", false},         // below -MAX_BALANCE_AMOUNT
	}
	for _, tc := range cases {
		got, err := normalizeBalanceAmount(mustDecimal(t, tc.input))
		if tc.ok {
			if err != nil {
				t.Errorf("normalizeBalanceAmount(%s) unexpected error: %v", tc.input, err)
				continue
			}
			if got.String() != tc.want {
				t.Errorf("normalizeBalanceAmount(%s) = %s, want %s", tc.input, got.String(), tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("normalizeBalanceAmount(%s) = %s, want error", tc.input, got.String())
		}
	}
}

func TestClampPagination(t *testing.T) {
	cases := []struct {
		page, pageSize     int
		wantPage, wantSize int
	}{
		{0, 0, 1, 20},     // both undefined -> defaults
		{2, 50, 2, 50},    // in range
		{-3, 0, 1, 20},    // non-positive page -> 1
		{1, 1000, 1, 100}, // clamped to maxPageSize
		{1, -5, 1, 1},     // clamped to min 1
		{1, 1, 1, 1},      // boundary
		{1, 100, 1, 100},  // boundary
	}
	for _, tc := range cases {
		page, pageSize := clampPagination(tc.page, tc.pageSize)
		if page != tc.wantPage || pageSize != tc.wantSize {
			t.Errorf("clampPagination(%d, %d) = (%d, %d), want (%d, %d)",
				tc.page, tc.pageSize, page, pageSize, tc.wantPage, tc.wantSize)
		}
	}
}

func TestNormalizeBalanceLogType(t *testing.T) {
	allowed := []models.BalanceLogType{
		models.BalanceLogTypeRecharge,
		models.BalanceLogTypeConsume,
		models.BalanceLogTypeRefund,
		models.BalanceLogTypeAdminAdjust,
		models.BalanceLogTypeGift,
		models.BalanceLogTypeTransferFee,
		models.BalanceLogTypeTransferRefund,
		models.BalanceLogTypeInviteGenerate,
	}
	for _, logType := range allowed {
		if got := normalizeBalanceLogType(logType); got != logType {
			t.Errorf("normalizeBalanceLogType(%q) = %q, want it unchanged", logType, got)
		}
	}

	// Types present in the Prisma enum but deliberately excluded from
	// BALANCE_LOG_TYPES must normalize to "not set".
	for _, excluded := range []models.BalanceLogType{
		models.BalanceLogTypeExchangePurchase,
		models.BalanceLogTypeExchangeRefund,
		models.BalanceLogTypeExchangeTransfer,
		models.BalanceLogTypeGiftCardIssue,
		models.BalanceLogType("nonsense"),
		models.BalanceLogType(""),
	} {
		if got := normalizeBalanceLogType(excluded); got != "" {
			t.Errorf("normalizeBalanceLogType(%q) = %q, want empty", excluded, got)
		}
	}
}

func TestBalanceLogTypesCount(t *testing.T) {
	if len(balanceLogTypes) != 8 {
		t.Fatalf("balanceLogTypes count = %d, want 8", len(balanceLogTypes))
	}
}
