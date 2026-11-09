package db

import (
	"encoding/json"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/models"
)

func TestAdminLotteryClampPagination(t *testing.T) {
	cases := []struct {
		page, pageSize         int64
		wantPage, wantPageSize int64
	}{
		{0, 0, 1, 20},
		{-5, -5, 1, 20},
		{1, 1, 1, 1},
		{3, 100, 3, 100},
		{3, 101, 3, 100},
		{3, 5000, 3, 100},
	}
	for _, tc := range cases {
		page, pageSize := adminLotteryClampPagination(tc.page, tc.pageSize)
		if page != tc.wantPage || pageSize != tc.wantPageSize {
			t.Errorf("clamp(%d, %d) = (%d, %d), want (%d, %d)",
				tc.page, tc.pageSize, page, pageSize, tc.wantPage, tc.wantPageSize)
		}
	}
}

func TestAdminLotteryIsSafePointsAdjustment(t *testing.T) {
	cases := []struct {
		value int64
		want  bool
	}{
		{0, false},
		{1, true},
		{-1, true},
		{1_000_000_000, true},
		{-1_000_000_000, true},
		{1_000_000_001, false},
		{-1_000_000_001, false},
	}
	for _, tc := range cases {
		if got := adminLotteryIsSafePointsAdjustment(tc.value); got != tc.want {
			t.Errorf("isSafePointsAdjustment(%d) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestAdminLotteryFormatBool(t *testing.T) {
	if got := adminLotteryFormatBool(true); got != "true" {
		t.Errorf("formatBool(true) = %q", got)
	}
	if got := adminLotteryFormatBool(false); got != "false" {
		t.Errorf("formatBool(false) = %q", got)
	}
}

func TestAdminLotteryPointsOrderColumns(t *testing.T) {
	// The route only accepts these three fields; every one must map onto a column.
	for _, field := range []string{"points", "totalEarned", "totalSpent"} {
		if _, ok := adminLotteryPointsOrderColumn[field]; !ok {
			t.Errorf("missing sort column for %q", field)
		}
	}
	if len(adminLotteryPointsOrderColumn) != 3 {
		t.Errorf("sort column set = %v, want exactly three fields", adminLotteryPointsOrderColumn)
	}
}

func TestAdminLotteryStatsJSONShape(t *testing.T) {
	stats := AdminLotteryStats{
		TotalDraws: 4,
		PrizeStats: []AdminLotteryPrizeStat{
			{Type: models.LotteryPrizeTypePoints, Count: 3},
			{Type: models.LotteryPrizeTypeNothing, Count: 1},
		},
	}
	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["totalDraws"] != float64(4) {
		t.Errorf("totalDraws = %v", decoded["totalDraws"])
	}
	prizeStats, ok := decoded["prizeStats"].([]any)
	if !ok || len(prizeStats) != 2 {
		t.Fatalf("prizeStats = %v", decoded["prizeStats"])
	}
	first := prizeStats[0].(map[string]any)
	if first["type"] != "points" || first["count"] != float64(3) {
		t.Errorf("prizeStats[0] = %v", first)
	}
}
