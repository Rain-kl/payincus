package db

import (
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// mapSshKeyRow is the whole response contract of the SSH-key module: snake_case
// keys and an ISO-8601 created_at. Pin it without a database.
func TestMapSshKeyRow(t *testing.T) {
	created := time.Date(2026, 10, 29, 12, 34, 56, 789_000_000, time.UTC)
	row := models.SshKey{
		ID:          7,
		UserID:      3,
		Name:        "laptop",
		PublicKey:   "ssh-ed25519 AAAA",
		Fingerprint: "SHA256:abc",
		CreatedAt:   created,
	}

	got := mapSshKeyRow(row)
	if got.ID != 7 || got.UserID != 3 || got.Name != "laptop" {
		t.Fatalf("identity fields = %+v", got)
	}
	if got.PublicKey != "ssh-ed25519 AAAA" || got.Fingerprint != "SHA256:abc" {
		t.Fatalf("key fields = %+v", got)
	}
	if got.CreatedAt != "2026-10-29T12:34:56.789Z" {
		t.Fatalf("created_at = %q, want Date#toISOString() form", got.CreatedAt)
	}

	// The timestamp is normalized to UTC, exactly like Date#toISOString().
	zoned := row
	zoned.CreatedAt = time.Date(2026, 10, 29, 20, 34, 56, 0, time.FixedZone("CST", 8*3600))
	if mapped := mapSshKeyRow(zoned); mapped.CreatedAt != "2026-10-29T12:34:56.000Z" {
		t.Fatalf("created_at with offset = %q", mapped.CreatedAt)
	}
}

func TestSshKeyCreatedAtUsesISO8601(t *testing.T) {
	cases := []struct {
		input time.Time
		want  string
	}{
		{time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "2026-01-02T03:04:05.000Z"},
		{time.Date(2026, 1, 2, 3, 4, 5, 500_000_000, time.UTC), "2026-01-02T03:04:05.500Z"},
	}
	for _, tc := range cases {
		if got := isoTimestamp(tc.input); got != tc.want {
			t.Errorf("isoTimestamp(%v) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
