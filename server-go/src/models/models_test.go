package models_test

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// The Node schema declares 130 models and 66 enums; the generated Go models must
// stay in lockstep so nothing silently disappears during a re-generation.
func TestGeneratedModelCounts(t *testing.T) {
	if len(models.ModelNames) != 130 {
		t.Fatalf("expected 130 models, got %d", len(models.ModelNames))
	}
	if len(models.EnumNames) != 66 {
		t.Fatalf("expected 66 enums, got %d", len(models.EnumNames))
	}
}

func TestTableNameMapping(t *testing.T) {
	if got := (models.User{}).TableName(); got != "users" {
		t.Fatalf("User.TableName() = %q, want users", got)
	}
	if got := (models.TokenInvalidation{}).TableName(); got != "token_invalidations" {
		t.Fatalf("TokenInvalidation.TableName() = %q", got)
	}
}

// Prisma serializes Decimal to a JSON string; the Go type must match so money
// fields stay byte-compatible with the Node API.
//
// NOTE: exact trailing-zero formatting (e.g. "100.00" vs "100") must be confirmed
// against the Node server by the dual-run conformance harness before release.
func TestDecimalMarshalsAsString(t *testing.T) {
	value, err := models.NewDecimalFromString("100.00")
	if err != nil {
		t.Fatalf("parse decimal: %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal decimal: %v", err)
	}
	if len(encoded) == 0 || encoded[0] != '"' {
		t.Fatalf("decimal JSON must be a quoted string, got %s", encoded)
	}

	var roundTrip string
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatalf("decimal JSON is not a string: %v", err)
	}
	parsed, err := decimal.NewFromString(roundTrip)
	if err != nil || !parsed.Equal(value.Decimal) {
		t.Fatalf("decimal round-trip mismatch: %s vs %s", roundTrip, value.String())
	}
}

func TestEnumValuesMatchSchema(t *testing.T) {
	if models.UserRoleAdmin != "admin" || models.UserRoleUser != "user" {
		t.Fatalf("UserRole values drifted: %q %q", models.UserRoleAdmin, models.UserRoleUser)
	}
	if models.AnnouncementTypeHostBroadcast != "host_broadcast" {
		t.Fatalf("AnnouncementType value drifted: %q", models.AnnouncementTypeHostBroadcast)
	}
}
