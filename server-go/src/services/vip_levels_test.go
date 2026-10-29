// Unit tests for the pure helpers ported from server/src/services/vip-levels.ts.
// These cover the boundary behaviour the Node module relies on; DB-backed paths
// are covered by the dual-run conformance harness.
package services

import (
	"math"
	"testing"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestGetDefaultVipBadgeStyle(t *testing.T) {
	cases := []struct {
		name  string
		level float64
		want  VipBadgeStyle
	}{
		{"level1", 1, VipBadgeStyle{BackgroundColor: "#FEF3C7", TextColor: "#92400E"}},
		{"level10", 10, VipBadgeStyle{BackgroundColor: "#FEF9C3", TextColor: "#A16207"}},
		{"level11-wraps", 11, VipBadgeStyle{BackgroundColor: "#FEF3C7", TextColor: "#92400E"}},
		{"level0-clamps", 0, VipBadgeStyle{BackgroundColor: "#FEF3C7", TextColor: "#92400E"}},
		{"negative-clamps", -5, VipBadgeStyle{BackgroundColor: "#FEF3C7", TextColor: "#92400E"}},
		{"fraction-floors", 2.9, VipBadgeStyle{BackgroundColor: "#D1FAE5", TextColor: "#047857"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GetDefaultVipBadgeStyle(tc.level); got != tc.want {
				t.Fatalf("GetDefaultVipBadgeStyle(%v) = %+v, want %+v", tc.level, got, tc.want)
			}
		})
	}
}

func TestNormalizeVipBadgeStyle(t *testing.T) {
	fallback := GetDefaultVipBadgeStyle(3)

	cases := []struct {
		name  string
		value any
		want  VipBadgeStyle
	}{
		{"nil-falls-back", nil, fallback},
		{"non-object-falls-back", "nope", fallback},
		{"array-falls-back", []any{"#000000"}, fallback},
		{
			"valid-hex-kept",
			map[string]any{"backgroundColor": "#aAbBcC", "textColor": "#123456"},
			VipBadgeStyle{BackgroundColor: "#aAbBcC", TextColor: "#123456"},
		},
		{
			"invalid-hex-uses-fallback",
			map[string]any{"backgroundColor": "#GGGGGG", "textColor": "red"},
			fallback,
		},
		{
			"partial-invalid-mixes",
			map[string]any{"backgroundColor": "#FFFFFF"},
			VipBadgeStyle{BackgroundColor: "#FFFFFF", TextColor: fallback.TextColor},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeVipBadgeStyle(3, tc.value); got != tc.want {
				t.Fatalf("NormalizeVipBadgeStyle(3, %v) = %+v, want %+v", tc.value, got, tc.want)
			}
		})
	}
}

func TestVipNullableMoney(t *testing.T) {
	t.Run("nil-is-null", func(t *testing.T) {
		got, err := vipNullableMoney(nil)
		if err != nil || got != nil {
			t.Fatalf("vipNullableMoney(nil) = %v, %v; want nil, nil", got, err)
		}
	})
	t.Run("empty-string-is-null", func(t *testing.T) {
		got, err := vipNullableMoney("")
		if err != nil || got != nil {
			t.Fatalf("vipNullableMoney(\"\") = %v, %v; want nil, nil", got, err)
		}
	})
	t.Run("zero-is-kept", func(t *testing.T) {
		got, err := vipNullableMoney(float64(0))
		if err != nil || got == nil || *got != 0 {
			t.Fatalf("vipNullableMoney(0) = %v, %v; want 0, nil", got, err)
		}
	})
	t.Run("rounds-float64-above-half-up", func(t *testing.T) {
		// 12.345 as an IEEE-754 double is slightly above 12.345, so both JS
		// Math.round and vipJSRound produce 12.35.
		got, err := vipNullableMoney(12.345)
		if err != nil || got == nil || !almostEqual(*got, 12.35) {
			t.Fatalf("vipNullableMoney(12.345) = %v, %v; want 12.35, nil", got, err)
		}
	})
	t.Run("rounds-float64-below-half-down", func(t *testing.T) {
		// 1.005 as a double is slightly below 1.005, so it rounds to 1.
		got, err := vipNullableMoney(1.005)
		if err != nil || got == nil || !almostEqual(*got, 1) {
			t.Fatalf("vipNullableMoney(1.005) = %v, %v; want 1, nil", got, err)
		}
	})
	t.Run("rounds-up", func(t *testing.T) {
		got, err := vipNullableMoney(12.346)
		if err != nil || got == nil || !almostEqual(*got, 12.35) {
			t.Fatalf("vipNullableMoney(12.346) = %v, %v; want 12.35, nil", got, err)
		}
	})
	t.Run("max-is-accepted", func(t *testing.T) {
		got, err := vipNullableMoney(99999999.99)
		if err != nil || got == nil || !almostEqual(*got, 99999999.99) {
			t.Fatalf("vipNullableMoney(99999999.99) = %v, %v; want 99999999.99, nil", got, err)
		}
	})
	t.Run("negative-rejected", func(t *testing.T) {
		if _, err := vipNullableMoney(-1); err == nil {
			t.Fatal("vipNullableMoney(-1) should error")
		}
	})
	t.Run("above-max-rejected", func(t *testing.T) {
		if _, err := vipNullableMoney(100000000); err == nil {
			t.Fatal("vipNullableMoney(100000000) should error")
		}
	})
	t.Run("non-numeric-rejected", func(t *testing.T) {
		if _, err := vipNullableMoney("abc"); err == nil {
			t.Fatal("vipNullableMoney(\"abc\") should error")
		}
	})
}

func TestSanitizeVipRuleInput(t *testing.T) {
	t.Run("valid-user-rule", func(t *testing.T) {
		rule, err := SanitizeVipRuleInput(VipRuleTypeUser, VipRuleInput{
			Level:       float64(3),
			MinRecharge: float64(100),
		}, UserVipMetricTotalRecharge)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rule.Level != 3 || rule.Type != "user" || !rule.Enabled {
			t.Fatalf("unexpected rule: %+v", rule)
		}
		if rule.ConditionMode != "any" {
			t.Fatalf("user conditionMode = %q, want any", rule.ConditionMode)
		}
		if rule.MinRecharge == nil || !almostEqual(*rule.MinRecharge, 100) {
			t.Fatalf("minRecharge = %v, want 100", rule.MinRecharge)
		}
		if rule.MinConsume != nil {
			t.Fatalf("minConsume = %v, want nil", rule.MinConsume)
		}
		if rule.BadgeStyle != GetDefaultVipBadgeStyle(3) {
			t.Fatalf("badgeStyle = %+v, want default", rule.BadgeStyle)
		}
	})

	t.Run("consume-metric-uses-minConsume", func(t *testing.T) {
		rule, err := SanitizeVipRuleInput(VipRuleTypeUser, VipRuleInput{
			Level:      float64(2),
			MinConsume: float64(50),
		}, UserVipMetricTotalConsume)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rule.MinRecharge != nil {
			t.Fatalf("minRecharge = %v, want nil", rule.MinRecharge)
		}
		if rule.MinConsume == nil || !almostEqual(*rule.MinConsume, 50) {
			t.Fatalf("minConsume = %v, want 50", rule.MinConsume)
		}
	})

	t.Run("conditionMode-all-forced-to-any", func(t *testing.T) {
		rule, err := SanitizeVipRuleInput(VipRuleTypeUser, VipRuleInput{
			Level:         float64(1),
			ConditionMode: "all",
			MinRecharge:   float64(1),
		}, UserVipMetricTotalRecharge)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rule.ConditionMode != "any" {
			t.Fatalf("conditionMode = %q, want any", rule.ConditionMode)
		}
	})

	levelErrors := []struct {
		name  string
		level any
	}{
		{"zero", float64(0)},
		{"eleven", float64(11)},
		{"fraction", 1.5},
		{"missing", nil},
		{"string-numeric-is-coerced-ok", "1"},
	}
	for _, tc := range levelErrors {
		t.Run("level-"+tc.name, func(t *testing.T) {
			_, err := SanitizeVipRuleInput(VipRuleTypeUser, VipRuleInput{
				Level:       tc.level,
				MinRecharge: float64(1),
			}, UserVipMetricTotalRecharge)
			if tc.name == "string-numeric-is-coerced-ok" {
				if err != nil {
					t.Fatalf("level \"1\" should be coerced to 1, got error %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("level %v should be rejected", tc.level)
			}
		})
	}

	t.Run("enabled-without-condition-rejected", func(t *testing.T) {
		_, err := SanitizeVipRuleInput(VipRuleTypeUser, VipRuleInput{Level: float64(1)}, UserVipMetricTotalRecharge)
		if err == nil || err.Error() != "VIP1 requires at least one condition" {
			t.Fatalf("error = %v, want VIP1 requires at least one condition", err)
		}
	})

	t.Run("disabled-without-condition-allowed", func(t *testing.T) {
		if _, err := SanitizeVipRuleInput(VipRuleTypeUser, VipRuleInput{
			Level:   float64(1),
			Enabled: false,
		}, UserVipMetricTotalRecharge); err != nil {
			t.Fatalf("disabled rule without condition should be allowed, got %v", err)
		}
	})

	t.Run("enabled-string-false-is-truthy", func(t *testing.T) {
		rule, err := SanitizeVipRuleInput(VipRuleTypeUser, VipRuleInput{
			Level:       float64(1),
			Enabled:     "false",
			MinRecharge: float64(1),
		}, UserVipMetricTotalRecharge)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !rule.Enabled {
			t.Fatal("enabled should stay true for the string \"false\" (Node `!== false`)")
		}
	})

	t.Run("benefit-balance-zero-rejected-in-strict-mode", func(t *testing.T) {
		_, err := SanitizeVipRuleInput(VipRuleTypeUser, VipRuleInput{
			Level:       float64(1),
			MinRecharge: float64(1),
			Benefits: map[string]any{
				"benefitHall": map[string]any{
					"balance": map[string]any{"enabled": true, "amount": 0},
				},
			},
		}, UserVipMetricTotalRecharge)
		if err == nil || err.Error() != "VIP benefit balance amount must be greater than 0" {
			t.Fatalf("error = %v, want balance amount error", err)
		}
	})
}

func TestIsVipRuleMatched(t *testing.T) {
	threshold := 100.0
	rule := NormalizedVipRule{
		Type:          "user",
		Level:         1,
		Enabled:       true,
		ConditionMode: "any",
		MinRecharge:   &threshold,
	}

	if !IsVipRuleMatched(VipRuleTypeUser, rule, UserVipStats{TotalRecharge: 100}) {
		t.Fatal("recharge == threshold should match")
	}
	if IsVipRuleMatched(VipRuleTypeUser, rule, UserVipStats{TotalRecharge: 99.99}) {
		t.Fatal("recharge below threshold should not match")
	}

	disabled := rule
	disabled.Enabled = false
	if IsVipRuleMatched(VipRuleTypeUser, disabled, UserVipStats{TotalRecharge: 1000}) {
		t.Fatal("disabled rule should not match")
	}

	// conditionMode "all" with both thresholds set.
	low, high := 10.0, 20.0
	both := NormalizedVipRule{
		Type:          "user",
		Level:         2,
		Enabled:       true,
		ConditionMode: "all",
		MinRecharge:   &low,
		MinConsume:    &high,
	}
	if IsVipRuleMatched(VipRuleTypeUser, both, UserVipStats{TotalRecharge: 50, TotalConsume: 5}) {
		t.Fatal("mode=all should fail when one condition is unmet")
	}
	if !IsVipRuleMatched(VipRuleTypeUser, both, UserVipStats{TotalRecharge: 50, TotalConsume: 25}) {
		t.Fatal("mode=all should match when both conditions hold")
	}
}

func TestCalculateVipLevel(t *testing.T) {
	level1, level2, level5 := 100.0, 500.0, 5000.0
	rules := []NormalizedVipRule{
		{Type: "user", Level: 1, Enabled: true, ConditionMode: "any", MinRecharge: &level1},
		{Type: "user", Level: 2, Enabled: true, ConditionMode: "any", MinRecharge: &level2},
		{Type: "user", Level: 5, Enabled: true, ConditionMode: "any", MinRecharge: &level5},
		{Type: "user", Level: 9, Enabled: false, ConditionMode: "any", MinRecharge: &level1},
	}

	cases := []struct {
		name  string
		stats UserVipStats
		want  float64
	}{
		{"below-all", UserVipStats{TotalRecharge: 10}, 0},
		{"exact-level1", UserVipStats{TotalRecharge: 100}, 1},
		{"level2", UserVipStats{TotalRecharge: 700}, 2},
		{"level5", UserVipStats{TotalRecharge: 9999}, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CalculateVipLevel(VipRuleTypeUser, rules, tc.stats); got != tc.want {
				t.Fatalf("CalculateVipLevel = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCalculateUserVipLevelByMetric(t *testing.T) {
	recharge, consume := 100.0, 100.0
	rules := []NormalizedVipRule{
		{Type: "user", Level: 1, Enabled: true, ConditionMode: "any", MinRecharge: &recharge},
		{Type: "user", Level: 2, Enabled: true, ConditionMode: "any", MinConsume: &consume},
	}

	if got := CalculateUserVipLevelByMetric(rules, UserVipStats{TotalRecharge: 500}, UserVipMetricTotalRecharge); got != 1 {
		t.Fatalf("recharge metric level = %v, want 1", got)
	}
	if got := CalculateUserVipLevelByMetric(rules, UserVipStats{TotalConsume: 500}, UserVipMetricTotalConsume); got != 2 {
		t.Fatalf("consume metric level = %v, want 2", got)
	}
	if got := CalculateUserVipLevelByMetric(rules, UserVipStats{}, UserVipMetricTotalRecharge); got != 0 {
		t.Fatalf("no stats level = %v, want 0", got)
	}
}

func TestCalculateVipProgress(t *testing.T) {
	target := 100.0
	rules := []NormalizedVipRule{
		{Type: "user", Level: 1, Enabled: true, ConditionMode: "any", MinRecharge: &target},
	}

	t.Run("halfway", func(t *testing.T) {
		progress := CalculateVipProgress(VipRuleTypeUser, rules, 0, UserVipStats{TotalRecharge: 50}, UserVipMetricTotalRecharge)
		if progress.NextLevel == nil || *progress.NextLevel != 1 {
			t.Fatalf("nextLevel = %v, want 1", progress.NextLevel)
		}
		if progress.ConditionMode == nil || *progress.ConditionMode != "any" {
			t.Fatalf("conditionMode = %v, want any", progress.ConditionMode)
		}
		if progress.UserMetric == nil || *progress.UserMetric != "totalRecharge" {
			t.Fatalf("userMetric = %v, want totalRecharge", progress.UserMetric)
		}
		if !almostEqual(progress.Progress, 50) {
			t.Fatalf("progress = %v, want 50", progress.Progress)
		}
		if progress.IsMaxLevel {
			t.Fatal("isMaxLevel should be false")
		}
		if len(progress.Conditions) != 1 {
			t.Fatalf("conditions len = %d, want 1", len(progress.Conditions))
		}
		condition := progress.Conditions[0]
		if condition.Metric != "totalRecharge" || !almostEqual(condition.Current, 50) ||
			!almostEqual(condition.Target, 100) || !almostEqual(condition.Remaining, 50) || condition.Matched {
			t.Fatalf("unexpected condition: %+v", condition)
		}
	})

	t.Run("target-reached-caps-progress", func(t *testing.T) {
		progress := CalculateVipProgress(VipRuleTypeUser, rules, 0, UserVipStats{TotalRecharge: 250}, UserVipMetricTotalRecharge)
		if !almostEqual(progress.Progress, 100) {
			t.Fatalf("progress = %v, want 100", progress.Progress)
		}
		if !progress.Conditions[0].Matched || !almostEqual(progress.Conditions[0].Remaining, 0) {
			t.Fatalf("unexpected condition: %+v", progress.Conditions[0])
		}
	})

	t.Run("max-level", func(t *testing.T) {
		progress := CalculateVipProgress(VipRuleTypeUser, rules, 1, UserVipStats{TotalRecharge: 250}, UserVipMetricTotalRecharge)
		if progress.NextLevel != nil {
			t.Fatalf("nextLevel = %v, want nil", progress.NextLevel)
		}
		if progress.ConditionMode != nil {
			t.Fatalf("conditionMode = %v, want nil", progress.ConditionMode)
		}
		if !almostEqual(progress.Progress, 100) || !progress.IsMaxLevel {
			t.Fatalf("progress = %v isMaxLevel = %v, want 100/true", progress.Progress, progress.IsMaxLevel)
		}
		if len(progress.Conditions) != 0 {
			t.Fatalf("conditions len = %d, want 0", len(progress.Conditions))
		}
	})

	t.Run("no-rules-from-zero", func(t *testing.T) {
		progress := CalculateVipProgress(VipRuleTypeUser, []NormalizedVipRule{}, 0, UserVipStats{}, UserVipMetricTotalRecharge)
		if progress.NextLevel != nil || progress.IsMaxLevel || progress.Progress != 0 {
			t.Fatalf("unexpected progress: %+v", progress)
		}
		if progress.Conditions == nil {
			t.Fatal("conditions should serialize as an empty array, not null")
		}
	})

	t.Run("disabled-next-rule-skipped", func(t *testing.T) {
		disabled := NormalizedVipRule{Type: "user", Level: 2, Enabled: false, ConditionMode: "any", MinRecharge: &target}
		progress := CalculateVipProgress(VipRuleTypeUser, append(rules, disabled), 1, UserVipStats{TotalRecharge: 10}, UserVipMetricTotalRecharge)
		if progress.NextLevel != nil {
			t.Fatalf("nextLevel = %v, want nil (disabled rule must be skipped)", progress.NextLevel)
		}
		if !progress.IsMaxLevel {
			t.Fatal("isMaxLevel should be true when no enabled higher rule exists")
		}
	})
}

func TestToProgressPercentBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		current float64
		target  float64
		want    float64
	}{
		{"zero-target", 10, 0, 0},
		{"negative-target", 10, -5, 0},
		{"negative-current", -10, 100, 0},
		{"over-target", 500, 100, 100},
		{"one-third", 1, 3, 33},
		{"two-thirds", 2, 3, 67},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vipToProgressPercent(tc.current, tc.target); !almostEqual(got, tc.want) {
				t.Fatalf("vipToProgressPercent(%v, %v) = %v, want %v", tc.current, tc.target, got, tc.want)
			}
		})
	}
}

func TestNormalizeBenefitHallConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		hall, err := vipNormalizeBenefitHallConfig(nil, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hall.Instance.Days != 30 || hall.Instance.Quantity != 1 {
			t.Fatalf("days/quantity = %v/%v, want 30/1", hall.Instance.Days, hall.Instance.Quantity)
		}
		if hall.Balance.Enabled || hall.Points.Enabled || hall.Instance.Enabled {
			t.Fatalf("nothing should be enabled by default: %+v", hall)
		}
	})

	t.Run("instance-requires-package-and-plan", func(t *testing.T) {
		hall, err := vipNormalizeBenefitHallConfig(map[string]any{
			"instance": map[string]any{"enabled": true, "packageId": 5},
		}, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hall.Instance.Enabled {
			t.Fatal("instance benefit without a plan must stay disabled")
		}
	})

	t.Run("strict-instance-missing-plan-rejected", func(t *testing.T) {
		_, err := vipNormalizeBenefitHallConfig(map[string]any{
			"instance": map[string]any{"enabled": true, "packageId": 5},
		}, true)
		if err == nil || err.Error() != "VIP benefit instance requires a package and plan" {
			t.Fatalf("error = %v, want package/plan error", err)
		}
	})

	t.Run("caps-applied", func(t *testing.T) {
		hall, err := vipNormalizeBenefitHallConfig(map[string]any{
			"points":   map[string]any{"enabled": true, "amount": 9999999999},
			"instance": map[string]any{"enabled": true, "packageId": 1, "planId": 2, "days": 99999, "quantity": 999},
		}, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hall.Points.Amount != vipMaxBenefitPoints {
			t.Fatalf("points amount = %v, want %v", hall.Points.Amount, float64(vipMaxBenefitPoints))
		}
		if hall.Instance.Days != vipMaxBenefitInstanceDays || hall.Instance.Quantity != vipMaxBenefitInstanceQuantity {
			t.Fatalf("days/quantity = %v/%v, want capped", hall.Instance.Days, hall.Instance.Quantity)
		}
	})

	t.Run("label-trim-and-truncate", func(t *testing.T) {
		long := ""
		for i := 0; i < 150; i++ {
			long += "a"
		}
		hall, err := vipNormalizeBenefitHallConfig(map[string]any{
			"instance": map[string]any{"packageName": "  Plan A  ", "planName": long},
		}, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hall.Instance.PackageName == nil || *hall.Instance.PackageName != "Plan A" {
			t.Fatalf("packageName = %v, want \"Plan A\"", hall.Instance.PackageName)
		}
		if hall.Instance.PlanName == nil || len(*hall.Instance.PlanName) != 100 {
			t.Fatalf("planName len = %v, want 100", hall.Instance.PlanName)
		}
	})
}

func TestVipNumberCoercion(t *testing.T) {
	cases := []struct {
		name       string
		value      any
		want       float64
		wantFinite bool
	}{
		{"nil", nil, 0, true},
		{"number", float64(12.5), 12.5, true},
		{"numeric-string", "12.5", 12.5, true},
		{"empty-string", "  ", 0, true},
		{"bad-string", "abc", math.NaN(), false},
		{"bool-true", true, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := vipNumber(tc.value)
			if ok != tc.wantFinite {
				t.Fatalf("vipNumber(%v) finite = %v, want %v", tc.value, ok, tc.wantFinite)
			}
			if tc.wantFinite && !almostEqual(got, tc.want) {
				t.Fatalf("vipNumber(%v) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
