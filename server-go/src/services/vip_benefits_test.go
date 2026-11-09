package services

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/rain-kl/payincus/server-go/src/models"
)

func TestVipDefaultBenefitsConfig(t *testing.T) {
	config := VipDefaultBenefitsConfig()
	if len(config.Levels) != 5 {
		t.Fatalf("default levels = %d, want 5", len(config.Levels))
	}
	for level := 1; level <= 5; level++ {
		entry, ok := config.Levels[strconv.Itoa(level)]
		if !ok {
			t.Fatalf("missing level %d", level)
		}
		if entry.OrderDiscountPercent != float64(level) || entry.ExtraTrafficPercent != float64(level*2) || entry.ResourcePoolBonusPercent != float64(level*2) {
			t.Fatalf("level %d = %+v", level, entry)
		}
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(VipDefaultBenefitsConfigJSON), &decoded); err != nil {
		t.Fatalf("default JSON is invalid: %v", err)
	}
	levels, ok := decoded["levels"].(map[string]any)
	if !ok || len(levels) != 5 {
		t.Fatalf("default JSON shape = %v", decoded)
	}
}

func TestVipNormalizeBenefitsConfig(t *testing.T) {
	var decoded any
	if err := json.Unmarshal([]byte(`{"levels":{"1":{"orderDiscountPercent":1.234,"extraTrafficPercent":2,"resourcePoolBonusPercent":3},"2":{"orderDiscountPercent":2,"extraTrafficPercent":4,"resourcePoolBonusPercent":4},"3":{"orderDiscountPercent":3,"extraTrafficPercent":6,"resourcePoolBonusPercent":6},"4":{"orderDiscountPercent":4,"extraTrafficPercent":8,"resourcePoolBonusPercent":8},"5":{"orderDiscountPercent":5,"extraTrafficPercent":10,"resourcePoolBonusPercent":10}}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	normalized, err := VipNormalizeBenefitsConfig(decoded)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if got := normalized.Levels["1"].OrderDiscountPercent; got != 1.23 {
		t.Fatalf("order discount = %v, want 1.23 (round(value*100)/100)", got)
	}
	if got := normalized.Levels["5"].ExtraTrafficPercent; got != 10 {
		t.Fatalf("extra traffic = %v", got)
	}

	invalid := []struct {
		name  string
		value any
		want  string
	}{
		{"missing percent", map[string]any{"levels": map[string]any{}}, "VIP1 order discount must be between 0 and 100"},
		{"string percent", map[string]any{"levels": map[string]any{"1": map[string]any{"orderDiscountPercent": "1"}}}, "VIP1 order discount must be between 0 and 100"},
		{"negative", map[string]any{"levels": map[string]any{"1": map[string]any{"orderDiscountPercent": -1}}}, "VIP1 order discount must be between 0 and 100"},
		{"over 100", map[string]any{"levels": map[string]any{"1": map[string]any{"orderDiscountPercent": 101}}}, "VIP1 order discount must be between 0 and 100"},
	}
	for _, tc := range invalid {
		if _, err := VipNormalizeBenefitsConfig(tc.value); err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestVipParseBenefitsConfigJSON(t *testing.T) {
	if got := VipParseBenefitsConfigJSON(nil); got.Levels["1"].OrderDiscountPercent != 1 {
		t.Fatalf("nil must yield defaults, got %+v", got)
	}
	empty := ""
	if got := VipParseBenefitsConfigJSON(&empty); got.Levels["5"].ResourcePoolBonusPercent != 10 {
		t.Fatalf("empty must yield defaults, got %+v", got)
	}
	garbage := "{not json"
	if got := VipParseBenefitsConfigJSON(&garbage); got.Levels["3"].ExtraTrafficPercent != 6 {
		t.Fatalf("garbage must yield defaults, got %+v", got)
	}
	outOfRange := `{"levels":{"1":{"orderDiscountPercent":101,"extraTrafficPercent":2,"resourcePoolBonusPercent":2}}}`
	if got := VipParseBenefitsConfigJSON(&outOfRange); got.Levels["1"].OrderDiscountPercent != 1 {
		t.Fatalf("invalid percent must yield defaults, got %+v", got)
	}
	valid := `{"levels":{"1":{"orderDiscountPercent":9.999,"extraTrafficPercent":2,"resourcePoolBonusPercent":2},"2":{"orderDiscountPercent":2,"extraTrafficPercent":4,"resourcePoolBonusPercent":4},"3":{"orderDiscountPercent":3,"extraTrafficPercent":6,"resourcePoolBonusPercent":6},"4":{"orderDiscountPercent":4,"extraTrafficPercent":8,"resourcePoolBonusPercent":8},"5":{"orderDiscountPercent":5,"extraTrafficPercent":10,"resourcePoolBonusPercent":10}}}`
	if got := VipParseBenefitsConfigJSON(&valid); got.Levels["1"].OrderDiscountPercent != 10 {
		t.Fatalf("valid JSON must be normalized, got %+v", got)
	}
}

func TestVipArbitratePrice(t *testing.T) {
	tenPercent := 10.0
	tenRate := 0.1
	fiveRate := 0.05

	cases := []struct {
		name     string
		input    VipPriceArbitrationInput
		final    float64
		discount float64
		source   VipPriceSource
	}{
		{"base wins when there are no discounts", VipPriceArbitrationInput{BasePrice: 10}, 10, 0, VipPriceSourceBase},
		{"aff 10% is cheaper", VipPriceArbitrationInput{BasePrice: 10, AffDiscountRate: &tenRate}, 9, 1, VipPriceSourceAff},
		{"a tie prefers the earlier candidate", VipPriceArbitrationInput{BasePrice: 10, AffDiscountRate: &tenRate, VipDiscountPercent: &tenPercent}, 9, 1, VipPriceSourceAff},
		{"vip 10% beats aff 5%", VipPriceArbitrationInput{BasePrice: 10, AffDiscountRate: &fiveRate, VipDiscountPercent: &tenPercent}, 9, 1, VipPriceSourceVip},
		{"rounding to two decimals", VipPriceArbitrationInput{BasePrice: 9.999}, 10, 0, VipPriceSourceBase},
		{"discount rounds the result", VipPriceArbitrationInput{BasePrice: 10, VipDiscountPercent: &tenPercent}, 9, 1, VipPriceSourceVip},
	}
	for _, tc := range cases {
		got := VipArbitratePrice(tc.input)
		if got.FinalPrice != tc.final || got.DiscountAmount != tc.discount || got.Source != tc.source {
			t.Errorf("%s: got %+v, want final=%v discount=%v source=%v", tc.name, got, tc.final, tc.discount, tc.source)
		}
	}

	// Rates are clamped to [0,1] (percent / 100).
	over := 500.0
	if got := VipArbitratePrice(VipPriceArbitrationInput{BasePrice: 10, VipDiscountPercent: &over}); got.FinalPrice != 0 {
		t.Fatalf("clamped 500%% discount must be free, got %+v", got)
	}
}

func TestVipCalculateExtraTraffic(t *testing.T) {
	if got := VipCalculateExtraTraffic(nil, 10); got.Sign() != 0 {
		t.Fatalf("nil base = %v", got)
	}
	if got := VipCalculateExtraTraffic(big.NewInt(0), 10); got.Sign() != 0 {
		t.Fatalf("zero base = %v", got)
	}
	if got := VipCalculateExtraTraffic(big.NewInt(-5), 10); got.Sign() != 0 {
		t.Fatalf("negative base = %v", got)
	}
	if got := VipCalculateExtraTraffic(big.NewInt(1000), 0); got.Sign() != 0 {
		t.Fatalf("zero percent = %v", got)
	}
	// 2% of 1000 = 20 (basis points 200 / 10000).
	if got := VipCalculateExtraTraffic(big.NewInt(1000), 2); got.Cmp(big.NewInt(20)) != 0 {
		t.Fatalf("2%% of 1000 = %v, want 20", got)
	}
	// 2.5% of 10000 = 250.
	if got := VipCalculateExtraTraffic(big.NewInt(10000), 2.5); got.Cmp(big.NewInt(250)) != 0 {
		t.Fatalf("2.5%% of 10000 = %v, want 250", got)
	}
	// Percent clamps at 100: 100% of 500 = 500.
	if got := VipCalculateExtraTraffic(big.NewInt(500), 1000); got.Cmp(big.NewInt(500)) != 0 {
		t.Fatalf("clamped percent = %v, want 500", got)
	}
}

func TestVipCalculateResourcePoolCost(t *testing.T) {
	cases := []struct {
		amount int64
		bonus  float64
		want   int64
	}{
		{100, 0, 100},
		{100, 2, 99},   // ceil(10000/102) = 99
		{100, 100, 50}, // ceil(10000/200) = 50
		{1, 100, 1},    // never below 1
		{100, -5, 100}, // negative bonus clamps to 0
		{100, 500, 50}, // over 100 clamps to 100
	}
	for _, tc := range cases {
		got, err := VipCalculateResourcePoolCost(tc.amount, tc.bonus)
		if err != nil {
			t.Fatalf("amount=%d bonus=%v: %v", tc.amount, tc.bonus, err)
		}
		if got != tc.want {
			t.Errorf("amount=%d bonus=%v: got %d, want %d", tc.amount, tc.bonus, got, tc.want)
		}
	}

	if _, err := VipCalculateResourcePoolCost(0, 0); err == nil {
		t.Fatal("zero amount must fail")
	}
	if _, err := VipCalculateResourcePoolCost(-1, 0); err == nil {
		t.Fatal("negative amount must fail")
	}
	if _, err := VipCalculateResourcePoolCost(vipBenefitMaxSafeInteger+1, 0); err == nil {
		t.Fatal("unsafe integer amount must fail")
	}
}

func TestVipBenefitInputFromAndNormalizeRewardData(t *testing.T) {
	if input := VipBenefitRewardInputFrom([]any{1}); len(input) != 0 {
		t.Fatalf("non-object body must yield an empty input, got %v", input)
	}
	if input := VipBenefitRewardInputFrom(nil); len(input) != 0 {
		t.Fatalf("nil body must yield an empty input, got %v", input)
	}

	ctx := context.Background()
	cases := []struct {
		name    string
		input   map[string]any
		wantErr string
	}{
		{"missing type", map[string]any{}, "Invalid VIP benefit reward type"},
		{"balance without amount", map[string]any{"type": "balance"}, "Balance reward amount must be a finite number"},
		{"balance amount zero", map[string]any{"type": "balance", "config": map[string]any{"amount": 0}}, "Balance reward amount must be between 0.01 and 99999999.99"},
		{"points amount zero", map[string]any{"type": "points", "config": map[string]any{"amount": 0}}, "Points reward amount must be an integer between 1 and 1000000000"},
		{"missing level", map[string]any{"type": "balance", "config": map[string]any{"amount": 5}}, "VIP level must be a safe integer"},
		{"level zero", map[string]any{"type": "balance", "level": 0, "config": map[string]any{"amount": 5}}, "VIP level must be between 1 and 10"},
		{"level over max", map[string]any{"type": "balance", "level": 11, "config": map[string]any{"amount": 5}}, "VIP level must be between 1 and 10"},
		{"missing title", map[string]any{"type": "balance", "level": 1, "config": map[string]any{"amount": 5}}, "Reward title is required"},
		{"blank title", map[string]any{"type": "balance", "level": 1, "title": "   ", "config": map[string]any{"amount": 5}}, "Reward title is required"},
		{"title too long", map[string]any{"type": "balance", "level": 1, "title": strings.Repeat("x", 81), "config": map[string]any{"amount": 5}}, "Reward title cannot exceed 80 characters"},
		{"claim limit too high", map[string]any{"type": "balance", "level": 1, "title": "A", "config": map[string]any{"amount": 5}, "claimLimit": 101}, "Claim limit must be an integer between 1 and 100"},
		{"sort order negative", map[string]any{"type": "balance", "level": 1, "title": "A", "config": map[string]any{"amount": 5}, "sortOrder": -1}, "Sort order must be an integer between 0 and 1000000"},
	}
	for _, tc := range cases {
		_, err := vipBenefitNormalizeRewardData(ctx, nil, VipBenefitRewardInput(tc.input), nil)
		if err == nil || err.Error() != tc.wantErr {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
		}
	}

	valid := map[string]any{
		"type":        "balance",
		"level":       float64(3),
		"title":       "  VIP3 赠金  ",
		"description": strings.Repeat("描", 400),
		"config":      map[string]any{"amount": 12.345},
		"claimLimit":  "",
		"sortOrder":   nil,
		"enabled":     "true", // only boolean true enables
	}
	data, err := vipBenefitNormalizeRewardData(ctx, nil, VipBenefitRewardInput(valid), nil)
	if err != nil {
		t.Fatalf("valid input: %v", err)
	}
	if data.Title != "VIP3 赠金" {
		t.Errorf("title = %q", data.Title)
	}
	if data.Description == nil || vipBenefitJSStringLength(*data.Description) != 300 {
		t.Errorf("description must be trimmed to 300 UTF-16 units, got %v", data.Description)
	}
	if data.ClaimLimit != 1 || data.SortOrder != 0 {
		t.Errorf("claimLimit=%d sortOrder=%d, want 1/0", data.ClaimLimit, data.SortOrder)
	}
	if data.Enabled {
		t.Error(`enabled:"true" must NOT enable the reward (input.enabled === true)`)
	}
	if data.Config.Amount == nil || *data.Config.Amount != 12.35 {
		t.Errorf("amount = %v, want 12.35", data.Config.Amount)
	}
	if got := data.Config.toJSON("balance"); got["amount"] != 12.35 {
		t.Errorf("balance config JSON = %v", got)
	}

	// Sufficient input keeps enabled true by default.
	minimal := map[string]any{"type": "points", "level": float64(1), "title": "P", "config": map[string]any{"amount": float64(100)}}
	data, err = vipBenefitNormalizeRewardData(ctx, nil, VipBenefitRewardInput(minimal), nil)
	if err != nil {
		t.Fatalf("minimal points input: %v", err)
	}
	if !data.Enabled || data.ClaimLimit != 1 {
		t.Errorf("defaults = %+v", data)
	}
}

func TestVipBenefitNormalizeRewardDataMergesBase(t *testing.T) {
	ctx := context.Background()
	amount := 7.5
	description := "base description"
	enabled := false
	base := &models.VipBenefitReward{
		ID:          9,
		Level:       2,
		Type:        models.VipBenefitRewardTypeBalance,
		Title:       "Base title",
		Description: &description,
		ClaimLimit:  4,
		SortOrder:   7,
		Enabled:     enabled,
		Config:      datatypes.JSON(`{"amount":7.5}`),
	}

	// An empty update must reproduce the stored row.
	data, err := vipBenefitNormalizeRewardData(ctx, nil, VipBenefitRewardInput{}, base)
	if err != nil {
		t.Fatalf("empty update: %v", err)
	}
	if data.Level != 2 || data.Type != "balance" || data.Title != "Base title" || data.ClaimLimit != 4 || data.SortOrder != 7 || data.Enabled {
		t.Fatalf("merged data = %+v", data)
	}
	if data.Description == nil || *data.Description != description {
		t.Fatalf("description = %v", data.Description)
	}
	if data.Config.Amount == nil || math.Abs(*data.Config.Amount-amount) > 1e-9 {
		t.Fatalf("amount = %v", data.Config.Amount)
	}

	// `description: null` is nullish for `??`, so it keeps the base value.
	data, err = vipBenefitNormalizeRewardData(ctx, nil, VipBenefitRewardInput{"description": nil}, base)
	if err != nil {
		t.Fatalf("null description: %v", err)
	}
	if data.Description == nil || *data.Description != description {
		t.Fatalf("null description must fall back to the base value, got %v", data.Description)
	}

	// An explicit empty string does clear it (normalizeDescription returns null).
	data, err = vipBenefitNormalizeRewardData(ctx, nil, VipBenefitRewardInput{"description": "  "}, base)
	if err != nil {
		t.Fatalf("blank description: %v", err)
	}
	if data.Description != nil {
		t.Fatalf("blank description must clear, got %v", *data.Description)
	}

	// Explicit true enables; explicit false disables.
	data, err = vipBenefitNormalizeRewardData(ctx, nil, VipBenefitRewardInput{"enabled": true}, base)
	if err != nil || !data.Enabled {
		t.Fatalf("enabled=true: %v %v", data.Enabled, err)
	}
}

func TestVipBenefitStoredConfigNormalization(t *testing.T) {
	// Balance: clamped at 0 and rounded to 2 decimals; missing means 0.
	config := vipBenefitNormalizeStoredRewardConfig("balance", map[string]any{"amount": "12.345"})
	if config.Amount == nil || *config.Amount != 12.35 {
		t.Fatalf("balance amount = %v", config.Amount)
	}
	config = vipBenefitNormalizeStoredRewardConfig("balance", nil)
	if config.Amount == nil || *config.Amount != 0 {
		t.Fatalf("missing balance amount = %v", config.Amount)
	}

	// Points: floored and never negative.
	config = vipBenefitNormalizeStoredRewardConfig("points", map[string]any{"amount": 9.9})
	if config.Amount == nil || *config.Amount != 9 {
		t.Fatalf("points amount = %v", config.Amount)
	}

	// Instance: package/plan ids stay null when not integers, days/quantity clamp.
	config = vipBenefitNormalizeStoredRewardConfig("instance", map[string]any{
		"packageId": 3.5, "planId": "7",
		"days": 0, "quantity": 50,
		"packageName": strings.Repeat("n", 120),
	})
	if config.PackageID != nil {
		t.Fatalf("non-integer packageId must be null, got %v", *config.PackageID)
	}
	if config.PlanID == nil || *config.PlanID != 7 {
		t.Fatalf("planId = %v", config.PlanID)
	}
	if config.Days != 30 || config.Quantity != 10 {
		t.Fatalf("days=%v quantity=%v, want 30/10", config.Days, config.Quantity)
	}
	if config.PackageName == nil || vipBenefitJSStringLength(*config.PackageName) != 100 {
		t.Fatalf("packageName = %v", config.PackageName)
	}

	jsonValue := config.toJSON("instance")
	if _, ok := jsonValue["packageId"]; !ok {
		t.Fatalf("instance config must always emit packageId, got %v", jsonValue)
	}
	if jsonValue["packageId"] != nil || jsonValue["planId"] != 7.0 {
		t.Fatalf("instance config JSON = %v", jsonValue)
	}
}

func TestVipBenefitClaimViewAndAdminView(t *testing.T) {
	delivered := time.Date(2026, 2, 3, 4, 5, 6, 789_000_000, time.UTC)
	claim := models.VipBenefitClaim{
		ID:          5,
		RewardID:    6,
		Level:       2,
		Status:      models.VipBenefitClaimStatusDelivered,
		ClaimNo:     1,
		Snapshot:    datatypes.JSON(`{"rewardId":6}`),
		DeliveredAt: &delivered,
		CreatedAt:   delivered,
	}
	view := vipBenefitClaimToView(claim)
	if view.CreatedAt != "2026-02-03T04:05:06.789Z" || view.DeliveredAt == nil || *view.DeliveredAt != "2026-02-03T04:05:06.789Z" {
		t.Fatalf("timestamps = %q / %v", view.CreatedAt, view.DeliveredAt)
	}
	if view.Snapshot["rewardId"] != float64(6) {
		t.Fatalf("snapshot = %v", view.Snapshot)
	}

	pending := claim
	pending.DeliveredAt = nil
	if view := vipBenefitClaimToView(pending); view.DeliveredAt != nil {
		t.Fatalf("pending deliveredAt must be null, got %v", *view.DeliveredAt)
	}

	reward := models.VipBenefitReward{
		ID: 7, Level: 1, Type: models.VipBenefitRewardTypePoints, Title: "T",
		ClaimLimit: 3, SortOrder: 2, Enabled: true,
		Config: datatypes.JSON(`{"amount":50}`), CreatedAt: delivered, UpdatedAt: delivered,
	}
	admin := vipBenefitRewardToAdminView(reward)
	if admin.ClaimedCount != 0 || admin.RemainingClaims != 3 || admin.State != "locked" || admin.BlockedByLevel != nil {
		t.Fatalf("admin view = %+v", admin)
	}
	if len(admin.Claims) != 0 {
		t.Fatalf("admin claims must be an empty slice, got %v", admin.Claims)
	}
	if admin.Config["amount"] != float64(50) {
		t.Fatalf("admin config = %v", admin.Config)
	}
}

func TestVipBenefitAddRewardAmount(t *testing.T) {
	summary := VipBenefitAmountSummary{}

	vipBenefitAddRewardAmount(&summary, VipBenefitRewardView{Type: "balance", Config: map[string]any{"amount": 10.005}}, 2)
	if summary.BalanceAmount != 20.01 {
		t.Fatalf("balance = %v, want 20.01 (roundMoney(10.005*2))", summary.BalanceAmount)
	}
	vipBenefitAddRewardAmount(&summary, VipBenefitRewardView{Type: "points", Config: map[string]any{"amount": 5.9}}, 3)
	if summary.PointsAmount != 15 {
		t.Fatalf("points = %v, want 15 (floor(5.9)*3)", summary.PointsAmount)
	}
	vipBenefitAddRewardAmount(&summary, VipBenefitRewardView{Type: "instance", Config: map[string]any{"quantity": 0}}, 2)
	if summary.InstanceQuantity != 2 {
		t.Fatalf("instance = %v, want 2 (max(1,floor(0||1))*2)", summary.InstanceQuantity)
	}
	vipBenefitAddRewardAmount(&summary, VipBenefitRewardView{Type: "balance", Config: map[string]any{"amount": 100}}, 0)
	if summary.BalanceAmount != 20.01 {
		t.Fatalf("zero multiplier must not change the summary, got %v", summary.BalanceAmount)
	}
}

func TestVipBenefitErrorStatus(t *testing.T) {
	err := NewVipBenefitError("X", "message")
	if err.StatusCode != 400 || err.Code != "X" || err.Error() != "message" {
		t.Fatalf("err = %+v", err)
	}
	custom := vipBenefitErrorStatus("Y", "nope", 409)
	if custom.StatusCode != 409 {
		t.Fatalf("status = %d", custom.StatusCode)
	}
}

// vipBenefitTestTime is shared by the timestamp assertions.
var vipBenefitTestTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestVipBenefitISOTime(t *testing.T) {
	if got := vipBenefitISOTime(vipBenefitTestTime); got != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("iso = %q", got)
	}
	if got := vipBenefitISOTime(vipBenefitTestTime.Add(1500 * time.Millisecond)); got != "2026-01-01T00:00:01.500Z" {
		t.Fatalf("iso = %q", got)
	}
}
