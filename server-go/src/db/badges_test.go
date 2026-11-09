package db

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// The catalogue item must distinguish `undefined` (omitted) from `null` exactly
// like BadgeCatalogItem: sourceId/assetUrlDark/displayOrder disappear when unset,
// while nameEn/seriesNameEn stay as explicit nulls.
func TestBadgesCatalogItemJSONOptionalFields(t *testing.T) {
	displayOrder := int32(0)
	isActive := false
	seriesIsActive := true
	item := BadgesCatalogItem{
		ID:                "elite",
		Name:              "精英",
		NameEn:            nil,
		FullLabel:         "精英 (Elite)",
		SeriesID:          "supreme",
		SeriesTitle:       "SUPREME",
		SeriesNameZh:      "尊贵系列",
		SeriesDescription: "desc",
		AssetURL:          "/badges/dark/elite.svg",
		DisplayOrder:      &displayOrder,
		IsActive:          &isActive,
		SeriesIsActive:    &seriesIsActive,
	}

	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{"sourceId", "sourceLabel", "assetUrlDark", "assetUrlLight"} {
		if _, present := decoded[key]; present {
			t.Errorf("%s must be omitted when undefined", key)
		}
	}
	if value, present := decoded["nameEn"]; !present || value != nil {
		t.Errorf("nameEn = %v (present=%v), want an explicit null", value, present)
	}
	if value, present := decoded["seriesNameEn"]; !present || value != nil {
		t.Errorf("seriesNameEn = %v (present=%v), want an explicit null", value, present)
	}
	// Pointer-backed numbers/booleans keep their zero values on the wire.
	if value, present := decoded["displayOrder"]; !present || value != float64(0) {
		t.Errorf("displayOrder = %v (present=%v), want 0", value, present)
	}
	if value, present := decoded["isActive"]; !present || value != false {
		t.Errorf("isActive = %v (present=%v), want false", value, present)
	}
}

func TestBadgesFallbackForIDShape(t *testing.T) {
	fallback := badgesFallbackForID("missing-badge")
	if fallback.Name != "missing-badge" || fallback.FullLabel != "missing-badge" {
		t.Errorf("fallback name/fullLabel = %q/%q", fallback.Name, fallback.FullLabel)
	}
	if fallback.SeriesID != "unknown" || fallback.SeriesTitle != "未知系列" {
		t.Errorf("fallback series = %q/%q", fallback.SeriesID, fallback.SeriesTitle)
	}
	if fallback.AssetURL != "/badges/dark/missing-badge.svg" ||
		fallback.AssetURLDark == nil || *fallback.AssetURLDark != "/badges/dark/missing-badge.svg" ||
		fallback.AssetURLLight == nil || *fallback.AssetURLLight != "/badges/light/missing-badge.svg" {
		t.Errorf("fallback assets = %q/%v/%v", fallback.AssetURL, fallback.AssetURLDark, fallback.AssetURLLight)
	}
	if fallback.IsActive == nil || *fallback.IsActive {
		t.Error("fallback IsActive must be false")
	}
	if fallback.SeriesIsActive == nil || *fallback.SeriesIsActive {
		t.Error("fallback SeriesIsActive must be false")
	}
	if fallback.DisplayOrder != nil {
		t.Error("fallback must omit displayOrder")
	}

	encoded, err := json.Marshal(fallback)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := decoded["displayOrder"]; present {
		t.Error("fallback JSON must omit displayOrder")
	}
	if decoded["isActive"] != false || decoded["seriesIsActive"] != false {
		t.Errorf("fallback flags = %v/%v", decoded["isActive"], decoded["seriesIsActive"])
	}
}

func TestBadgesMapOwnershipFields(t *testing.T) {
	appliedAt := time.Date(2027, 1, 2, 3, 4, 5, 678_000_000, time.UTC)
	createdAt := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	instanceID := int32(9)
	instanceName := "web-1"
	avatarTarget := string(models.BadgeApplicationTargetAvatar)

	record := badgesOwnershipRecord{
		ID:                  42,
		BadgeID:             "elite",
		Source:              string(models.UserBadgeSourceDraw),
		ApplicationTarget:   &avatarTarget,
		AppliedInstanceID:   &instanceID,
		AppliedAt:           &appliedAt,
		CreatedAt:           createdAt,
		AppliedInstanceName: &instanceName,
	}
	badgeMap := map[string]BadgesCatalogItem{
		"elite": {ID: "elite", Name: "精英", FullLabel: "精英 (Elite)", SeriesID: "supreme", SeriesTitle: "SUPREME", AssetURL: "/badges/dark/elite.svg"},
	}

	view := badgesMapOwnership(record, badgeMap)
	if view.BadgeName != "精英" || view.BadgeLabel != "精英 (Elite)" {
		t.Errorf("badge fields = %q/%q", view.BadgeName, view.BadgeLabel)
	}
	if view.ApplicationTarget == nil || *view.ApplicationTarget != models.BadgeApplicationTargetAvatar {
		t.Errorf("applicationTarget = %v", view.ApplicationTarget)
	}
	if view.AppliedAt == nil || *view.AppliedAt != "2027-01-02T03:04:05.678Z" {
		t.Errorf("appliedAt = %v", view.AppliedAt)
	}
	if view.CreatedAt != "2026-12-31T23:59:59.000Z" {
		t.Errorf("createdAt = %q", view.CreatedAt)
	}
	if view.AppliedInstanceID == nil || *view.AppliedInstanceID != 9 ||
		view.AppliedInstanceName == nil || *view.AppliedInstanceName != "web-1" {
		t.Errorf("instance = %v/%v", view.AppliedInstanceID, view.AppliedInstanceName)
	}

	// A missing catalogue row falls back to the placeholder instead of nulls.
	missing := badgesMapOwnership(badgesOwnershipRecord{ID: 1, BadgeID: "gone", Source: "draw"}, map[string]BadgesCatalogItem{})
	if missing.BadgeName != "gone" || missing.SeriesID != "unknown" || missing.AssetURL != "/badges/dark/gone.svg" {
		t.Errorf("fallback ownership = %+v", missing)
	}
	if missing.AppliedAt != nil || missing.ApplicationTarget != nil {
		t.Errorf("unapplied ownership must serialize nulls: %+v", missing)
	}
}

func TestBadgesErrorCode(t *testing.T) {
	if got := BadgesErrorCode(nil); got != "UNKNOWN_ERROR" {
		t.Errorf("nil error code = %q", got)
	}
	unique := &pgconn.PgError{Code: "23505"}
	if got := BadgesErrorCode(unique); got != "BADGE_STATE_CONFLICT" {
		t.Errorf("unique violation code = %q", got)
	}
	if got := BadgesErrorCode(errors.New("BADGE_NOT_FOUND")); got != "BADGE_NOT_FOUND" {
		t.Errorf("generic code = %q", got)
	}
}

func TestBadgesCosts(t *testing.T) {
	if BadgesRandomDrawCost != 500 {
		t.Errorf("BadgesRandomDrawCost = %d, want 500", BadgesRandomDrawCost)
	}
	if BadgesSelectCost != 1500 {
		t.Errorf("BadgesSelectCost = %d, want 1500", BadgesSelectCost)
	}
}

func TestBadgesRandomIndexIsUniformFloor(t *testing.T) {
	original := BadgesRandomFloat
	defer func() { BadgesRandomFloat = original }()

	cases := []struct {
		random float64
		length int
		want   int
	}{
		{0, 4, 0},
		{0.24, 4, 0},
		{0.25, 4, 1},
		{0.999999, 4, 3},
		{1, 4, 3}, // defensive clamp; Math.random never returns 1
	}
	for _, tc := range cases {
		BadgesRandomFloat = func() float64 { return tc.random }
		if got := badgesRandomIndex(tc.length); got != tc.want {
			t.Errorf("badgesRandomIndex(%d) with random=%v = %d, want %d", tc.length, tc.random, got, tc.want)
		}
	}
}

func TestBadgesRetryableTransactionErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"serialization failure", &pgconn.PgError{Code: "40001"}, true},
		{"deadlock detected", &pgconn.PgError{Code: "40P01"}, true},
		{"message deadlock", errors.New("deadlock detected"), true},
		{"message write conflict", errors.New("write conflict"), true},
		{"message serialize", errors.New("could not serialize access"), true},
		{"business error", errors.New("NO_BADGES_AVAILABLE"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := badgesIsRetryableTransactionError(tc.err); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// BadgeSeriesView must expose every key getBadgeSeriesList always sets, including
// the two aggregate counts and ISO timestamps.
func TestBadgesSeriesViewJSON(t *testing.T) {
	view := BadgesSeriesView{
		ID:               "supreme",
		Title:            "SUPREME",
		NameZh:           "尊贵系列",
		Description:      "desc",
		DisplayOrder:     100,
		IsActive:         true,
		BadgeCount:       3,
		ActiveBadgeCount: 2,
		CreatedAt:        "2026-01-01T00:00:00.000Z",
		UpdatedAt:        "2026-01-02T00:00:00.000Z",
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"id", "title", "nameZh", "nameEn", "description", "sourceId",
		"sourceLabel", "displayOrder", "isActive", "badgeCount", "activeBadgeCount", "createdAt", "updatedAt"} {
		if _, present := decoded[key]; !present {
			t.Errorf("missing key %q", key)
		}
	}
	if decoded["badgeCount"] != float64(3) || decoded["activeBadgeCount"] != float64(2) {
		t.Errorf("counts = %v/%v", decoded["badgeCount"], decoded["activeBadgeCount"])
	}
}
