package services

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

func TestPruneAndNormalizePresetStore(t *testing.T) {
	fresh := time.Now().UTC()
	stale := fresh.Add(-31 * 24 * time.Hour)

	store := PruneAndNormalizePresetStore(map[string]any{
		"version": 1,
		"byTask": map[string]any{
			"t1": map[string]any{
				"hostId":    float64(4),
				"incusName": " u1 ",
				"filePath":  " /path.tar.gz ",
				"preset": map[string]any{
					"packageId":   float64(9),
					"validDays":   float64(3),
					"packageName": "pkg",
					"createdAt":   fresh.Format(time.RFC3339),
				},
			},
			"stale-task": map[string]any{
				"hostId": float64(4),
				"preset": map[string]any{
					"createdAt": stale.Format(time.RFC3339),
				},
			},
			"bad-host": map[string]any{
				"hostId": 0,
				"preset": map[string]any{"createdAt": fresh.Format(time.RFC3339)},
			},
		},
		"byUnassigned": map[string]any{
			"4:u2": map[string]any{
				"userId":    float64(7),
				"createdAt": fresh.Format(time.RFC3339),
			},
			"4:old": map[string]any{
				"createdAt": stale.Format(time.RFC3339),
			},
		},
	}, fresh)

	if len(store.ByTask) != 1 {
		t.Fatalf("byTask entries = %d, want 1 (stale + bad-host pruned): %+v", len(store.ByTask), store.ByTask)
	}
	entry := store.ByTask["t1"]
	if entry.HostID != 4 || entry.IncusName == nil || *entry.IncusName != "u1" || entry.FilePath == nil || *entry.FilePath != "/path.tar.gz" {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.Preset.PackageID == nil || *entry.Preset.PackageID != 9 || entry.Preset.ValidDays == nil || *entry.Preset.ValidDays != 3 {
		t.Fatalf("preset = %+v", entry.Preset)
	}
	if len(store.ByUnassigned) != 1 {
		t.Fatalf("byUnassigned entries = %d, want 1: %+v", len(store.ByUnassigned), store.ByUnassigned)
	}
	if preset := store.ByUnassigned["4:u2"]; preset.UserID == nil || *preset.UserID != 7 {
		t.Fatalf("unassigned preset = %+v", preset)
	}

	// Non-record input collapses to an empty store.
	if empty := PruneAndNormalizePresetStore("junk", fresh); len(empty.ByTask) != 0 || len(empty.ByUnassigned) != 0 || empty.Version != 1 {
		t.Fatalf("junk input = %+v", empty)
	}
}

func TestResolveExplicitExpiresAt(t *testing.T) {
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)

	if _, err := ResolveExplicitExpiresAt(nil, nil, nil, now); err != nil {
		t.Fatalf("default nil must pass through: %v", err)
	}
	def := now.Add(24 * time.Hour)
	got, err := ResolveExplicitExpiresAt(nil, nil, &def, now)
	if err != nil || !got.Equal(def) {
		t.Fatalf("default = %v (%v)", got, err)
	}

	exp := "2026-12-01T00:00:00Z"
	got, err = ResolveExplicitExpiresAt(nil, &exp, nil, now)
	if err != nil || got.Format("2006-01-02") != "2026-12-01" {
		t.Fatalf("explicit = %v (%v)", got, err)
	}

	bad := "not-a-date"
	if _, err := ResolveExplicitExpiresAt(nil, &bad, nil, now); err == nil {
		t.Fatal("invalid expiresAt must fail")
	}

	days := int32(3)
	got, err = ResolveExplicitExpiresAt(&days, nil, nil, now)
	if err != nil || got.Sub(now) != 3*24*time.Hour {
		t.Fatalf("validDays = %v (%v)", got, err)
	}

	badDays := int32(36501)
	_, err = ResolveExplicitExpiresAt(&badDays, nil, nil, now)
	var bindingErr *PackageBindingError
	if !errors.As(err, &bindingErr) || bindingErr.Code != "INVALID_VALID_DAYS" {
		t.Fatalf("over-range validDays error = %v", err)
	}

	zeroDays := int32(0)
	if _, err := ResolveExplicitExpiresAt(&zeroDays, nil, nil, now); err == nil {
		t.Fatal("validDays=0 must fail")
	}
}

func TestNormalizeIncusSizeAndCpu(t *testing.T) {
	cases := []struct {
		raw  any
		want int32
	}{
		{"512MiB", 512},
		{"1GiB", 1024},
		{"2G", 2048}, // normalizeIncusSizeToMB accepts bare [kmgt] units
		{"100MB", 100},
		{"", 777},
		{nil, 777},
		{"junk", 777},
	}
	for _, c := range cases {
		if got := normalizeIncusSizeToMB(c.raw, 777); got != c.want {
			t.Fatalf("normalizeIncusSizeToMB(%v) = %d, want %d", c.raw, got, c.want)
		}
	}

	if got := normalizeIncusCpuAllowance(map[string]any{"limits.cpu.allowance": "30%"}); got != 30 {
		t.Fatalf("allowance = %d", got)
	}
	if got := normalizeIncusCpuAllowance(map[string]any{"limits.cpu": "4"}); got != 400 {
		t.Fatalf("cores = %d", got)
	}
	if got := normalizeIncusCpuAllowance(map[string]any{}); got != 100 {
		t.Fatalf("default = %d", got)
	}
}

func TestPackageBindingErrorShape(t *testing.T) {
	err := newPackageBindingError("所选套餐不存在", 404, "PACKAGE_NOT_FOUND")
	var bindingErr *PackageBindingError
	if !errors.As(err, &bindingErr) {
		t.Fatalf("not a PackageBindingError: %v", err)
	}
	if bindingErr.StatusCode != 404 || bindingErr.Code != "PACKAGE_NOT_FOUND" || bindingErr.Error() != "所选套餐不存在" {
		t.Fatalf("binding error = %+v", bindingErr)
	}
}

func TestUnassignedPresetKeyAndStoreJSON(t *testing.T) {
	if key := unassignedPresetKey(4, "u1"); key != "4:u1" {
		t.Fatalf("key = %q", key)
	}
	// The store round-trips through JSON with the same field names the Node
	// backend reads (byTask/byUnassigned/hostId/preset).
	store := emptyPresetStore()
	name := "u1"
	store.ByTask["t1"] = storedTaskPreset{
		HostID: 4, IncusName: &name,
		Preset: PendingImportPreset{CreatedAt: "2026-11-19T00:00:00Z"},
	}
	encoded, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(encoded)
	for _, want := range []string{`"byTask"`, `"byUnassigned"`, `"hostId":4`, `"incusName":"u1"`, `"preset"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("encoded store missing %q: %s", want, text)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ver, _ := decoded["version"].(float64); ver != 1 {
		t.Fatalf("version = %v", decoded["version"])
	}
}

func TestModelsNetworkModePassthrough(t *testing.T) {
	// Ensures the alias used by the transfer route maps 1:1 onto the model enum.
	if models.NetworkMode("nat") != models.NetworkModeNat {
		t.Fatal("nat mode mismatch")
	}
	if models.NetworkMode("nat_ipv6") != models.NetworkModeNatIpv6 {
		t.Fatal("nat_ipv6 mode mismatch")
	}
	if models.NetworkMode("ipv6_only") != models.NetworkModeIpv6Only {
		t.Fatal("ipv6_only mode mismatch")
	}
}
