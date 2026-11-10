// Unit tests for the instance-billing route port.
//
// The tests are deliberately pure: they pin the money arithmetic (rounding and
// the 31-day month), the replay/idempotency predicate, the balance-log snapshot
// contract, the ownership branches, and the Fastify-schema body validators. DB
// behaviour (advisory locks, transaction rollback) is covered by the dual-run
// conformance cases in test/conformance/requests-instance-billing.json.
package routes

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

func instanceBillingTestDecimal(value string) models.Decimal {
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		panic(err)
	}
	return models.Decimal{Decimal: parsed}
}

// TestInstanceBillingRenewAmountRounding pins `Number((monthlyPrice * months).toFixed(2))`.
func TestInstanceBillingRenewAmountRounding(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name   string
		price  string
		cycle  *int32
		months int
		want   float64
	}{
		// 100 / 3 = 33.333…, one month → 33.33.
		{"cycle-3-one-month", "100", instanceBillingInt32Pointer(3), 1, 33.33},
		// 10 / 3 = 3.333…, three months → 9.999999…, toFixed(2) → 10.00.
		{"cycle-3-three-months", "10", instanceBillingInt32Pointer(3), 3, 10.00},
		// 3.015 / 3 = 1.0050000000000001 in IEEE-754 (just above 1.005), so
		// toFixed(2) rounds up to 1.01 — same as Node.
		{"binary-just-above", "3.015", instanceBillingInt32Pointer(3), 1, 1.01},
		// 1.005 itself is stored just below 1.005 binary, so toFixed(2) → 1.00.
		{"binary-1.005", "1.005", instanceBillingInt32Pointer(1), 1, 1.00},
		{"plain-month", "30", instanceBillingInt32Pointer(1), 3, 90},
		{"null-cycle-is-one", "30", nil, 1, 30},
	} {
		instance := &models.Instance{
			BillingPrice: func() *models.Decimal {
				value := instanceBillingTestDecimal(tc.price)
				return &value
			}(),
			BillingCycle: tc.cycle,
			ExpiresAt:    func() *time.Time { value := now.Add(10 * 24 * time.Hour); return &value }(),
		}
		_, amount, _, err := instanceBillingRenewBilling(instance, tc.months, now)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if amount != tc.want {
			t.Errorf("%s: amount = %v, want %v (Number(x.toFixed(2)) semantics)", tc.name, amount, tc.want)
		}
	}
}

// TestInstanceBillingAddMonthsIs31DayArithmetic pins lib.AddMonths: one month is
// 31 days, twelve months is 372 days, and it is NOT a calendar year.
func TestInstanceBillingAddMonthsIs31DayArithmetic(t *testing.T) {
	base := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	if got, want := lib.AddMonths(base, 1), base.AddDate(0, 0, 31); !got.Equal(want) {
		t.Errorf("AddMonths(+1) = %v, want %v", got, want)
	}
	if got, want := lib.AddMonths(base, 12), base.AddDate(0, 0, 372); !got.Equal(want) {
		t.Errorf("AddMonths(+12) = %v, want %v (372 days)", got, want)
	}
	if calendar := base.AddDate(1, 0, 0); lib.AddMonths(base, 12).Equal(calendar) {
		t.Errorf("AddMonths(+12) must not equal a calendar year")
	}

	// The renewal helper uses the same rule.
	instance := &models.Instance{
		BillingPrice: func() *models.Decimal { value := instanceBillingTestDecimal("30"); return &value }(),
		BillingCycle: instanceBillingInt32Pointer(1),
		ExpiresAt:    func() *time.Time { value := base; return &value }(),
	}
	_, _, newExpiresAt, err := instanceBillingRenewBilling(instance, 12, base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := base.AddDate(0, 0, 372); !newExpiresAt.Equal(want) {
		t.Errorf("renew +12 months = %v, want %v", newExpiresAt, want)
	}
}

// TestInstanceBillingRenewBillingBaseDate pins "expired → now, active → expiresAt".
func TestInstanceBillingRenewBillingBaseDate(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	price := instanceBillingTestDecimal("30")

	future := now.Add(10 * 24 * time.Hour)
	instance := &models.Instance{
		BillingPrice: &price,
		BillingCycle: instanceBillingInt32Pointer(1),
		ExpiresAt:    &future,
	}
	_, _, activeResult, err := instanceBillingRenewBilling(instance, 1, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := future.AddDate(0, 0, 31); !activeResult.Equal(want) {
		t.Errorf("active renewal base = %v, want %v", activeResult, want)
	}

	past := now.Add(-10 * 24 * time.Hour)
	instance.ExpiresAt = &past
	_, _, expiredResult, err := instanceBillingRenewBilling(instance, 1, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := now.AddDate(0, 0, 31); !expiredResult.Equal(want) {
		t.Errorf("expired renewal base = %v, want %v", expiredResult, want)
	}
}

// TestInstanceBillingIdempotentReplayRejected pins the exact-replay guard: the
// same period_end means the renewal already happened, so the transaction returns
// without debiting again. A different period_end is a legitimate new renewal.
func TestInstanceBillingIdempotentReplayRejected(t *testing.T) {
	periodEnd := time.Date(2026, 12, 11, 12, 0, 0, 0, time.UTC)
	records := []models.InstanceBillingRecord{
		{ID: 1, Type: models.BillingRecordTypeRenew, PeriodEnd: periodEnd.Add(-31 * 24 * time.Hour)},
		{ID: 2, Type: models.BillingRecordTypeRenew, PeriodEnd: periodEnd},
	}

	if !instanceBillingRenewalAlreadyApplied(records, periodEnd) {
		t.Error("an existing record with the same period_end must be treated as already applied")
	}
	if instanceBillingRenewalAlreadyApplied(records, periodEnd.Add(time.Millisecond)) {
		t.Error("a different period_end must not be treated as a replay")
	}
	if instanceBillingRenewalAlreadyApplied(nil, periodEnd) {
		t.Error("no records means the renewal has not been applied")
	}

	// A sequential renewal always moves period_end forward by months*31 days, so it
	// must never be mistaken for a replay.
	next := lib.AddMonths(periodEnd, 1)
	if instanceBillingRenewalAlreadyApplied(records, next) {
		t.Error("a sequential renewal (period_end + 31d) must not be blocked")
	}
}

// TestInstanceBillingBalanceSnapshot pins the balance_logs contract: the audit
// row stores balance_before / balance_after snapshots plus the signed delta, not
// just an increment.
func TestInstanceBillingBalanceSnapshot(t *testing.T) {
	before := instanceBillingTestDecimal("100.00")
	after := instanceBillingTestDecimal("70.00")
	delta := instanceBillingTestDecimal("-30.00")

	row := db.InstanceBillingBalanceLogRow(db.InstanceBillingBalanceChange{
		UserID:     7,
		InstanceID: 920001,
		Delta:      delta,
		LogType:    models.BalanceLogTypeConsume,
		Remark:     "续费（1个月）：parity-renew",
	}, before, after)

	if row.Type != models.BalanceLogTypeConsume {
		t.Errorf("Type = %q, want consume", row.Type)
	}
	if !row.BalanceBefore.Equal(before.Decimal) {
		t.Errorf("BalanceBefore = %s, want 100", row.BalanceBefore)
	}
	if !row.BalanceAfter.Equal(after.Decimal) {
		t.Errorf("BalanceAfter = %s, want 70", row.BalanceAfter)
	}
	if !row.Amount.Equal(delta.Decimal) {
		t.Errorf("Amount = %s, want -30", row.Amount)
	}
	if row.InstanceID == nil || *row.InstanceID != 920001 {
		t.Errorf("InstanceID = %v, want 920001", row.InstanceID)
	}
	if row.Remark == nil || *row.Remark == "" {
		t.Error("Remark must be recorded")
	}
}

// TestInstanceBillingViewerAccessBranches pins the owner / host-owner / stranger
// decision, including the fact that there is no admin bypass.
func TestInstanceBillingViewerAccessBranches(t *testing.T) {
	const instanceOwner int32 = 10
	const hostOwner int32 = 20

	if isOwner, isHostOwner, allowed := instanceBillingViewerAccess(instanceOwner, hostOwner, instanceOwner); !allowed || !isOwner || isHostOwner {
		t.Errorf("instance owner: owner=%v host=%v allowed=%v", isOwner, isHostOwner, allowed)
	}
	if isOwner, isHostOwner, allowed := instanceBillingViewerAccess(instanceOwner, hostOwner, hostOwner); !allowed || isOwner || !isHostOwner {
		t.Errorf("host owner: owner=%v host=%v allowed=%v", isOwner, isHostOwner, allowed)
	}
	if _, _, allowed := instanceBillingViewerAccess(instanceOwner, hostOwner, 99); allowed {
		t.Error("a stranger must be denied")
	}
	// No host row: only the instance owner can read (a stranger must not match the
	// zero host-owner id by accident).
	if _, _, allowed := instanceBillingViewerAccess(instanceOwner, 0, 99); allowed {
		t.Error("missing host owner must not grant access to a stranger")
	}
	// An admin is only special if they happen to own the instance/host.
	if _, _, allowed := instanceBillingViewerAccess(instanceOwner, hostOwner, 1); allowed {
		t.Error("admin without ownership must be denied (no admin bypass in this route)")
	}
}

// TestInstanceBillingValidateInstanceIDs pins the array schema and the dedupe.
func TestInstanceBillingValidateInstanceIDs(t *testing.T) {
	if _, message := instanceBillingValidateInstanceIDs(map[string]any{}); message != "instanceIds is required" {
		t.Errorf("missing: %q", message)
	}
	// AJV coerceTypes:'array' wraps a scalar and then validates the item, so a
	// scalar string is an integer failure, not a type-of-array failure. Measured
	// against Node on POST /api/instances/batch/destroy-info.
	if _, message := instanceBillingValidateInstanceIDs(map[string]any{"instanceIds": "nope"}); message != "must be integer" {
		t.Errorf("unparsable scalar: %q", message)
	}
	if _, message := instanceBillingValidateInstanceIDs(map[string]any{"instanceIds": map[string]any{}}); message != "must be array" {
		t.Errorf("object must not be coerced: %q", message)
	}
	if ids, message := instanceBillingValidateInstanceIDs(map[string]any{"instanceIds": "950001"}); message != "" || len(ids) != 1 || ids[0] != 950001 {
		t.Errorf("scalar string not wrapped: ids=%v message=%q", ids, message)
	}
	if ids, message := instanceBillingValidateInstanceIDs(map[string]any{"instanceIds": nil}); message != "" || len(ids) != 1 || ids[0] != 0 {
		t.Errorf("null must coerce to 0: ids=%v message=%q", ids, message)
	}
	if _, message := instanceBillingValidateInstanceIDs(map[string]any{"instanceIds": []any{}}); message != "must NOT have fewer than 1 items" {
		t.Errorf("empty: %q", message)
	}

	tooMany := make([]any, 0, 101)
	for index := 0; index < 101; index++ {
		tooMany = append(tooMany, float64(index+1))
	}
	if _, message := instanceBillingValidateInstanceIDs(map[string]any{"instanceIds": tooMany}); message != "must NOT have more than 100 items" {
		t.Errorf("too many: %q", message)
	}
	if _, message := instanceBillingValidateInstanceIDs(map[string]any{"instanceIds": []any{1.5}}); message != "must be integer" {
		t.Errorf("fractional item: %q", message)
	}
	if _, message := instanceBillingValidateInstanceIDs(map[string]any{"instanceIds": []any{"abc"}}); message != "must be integer" {
		t.Errorf("non-numeric item: %q", message)
	}

	ids, message := instanceBillingValidateInstanceIDs(map[string]any{"instanceIds": []any{float64(5), float64(5), float64(3), "7"}})
	if message != "" {
		t.Fatalf("valid input rejected: %q", message)
	}
	want := []int32{5, 3, 7}
	if len(ids) != len(want) {
		t.Fatalf("dedupe = %v, want %v", ids, want)
	}
	for index := range want {
		if ids[index] != want[index] {
			t.Fatalf("dedupe = %v, want %v", ids, want)
		}
	}
}

// TestInstanceBillingValidateMonths pins the integer 1..24 body schema.
func TestInstanceBillingValidateMonths(t *testing.T) {
	if _, message := instanceBillingValidateMonths(map[string]any{}); message != "months is required" {
		t.Errorf("missing: %q", message)
	}
	if _, message := instanceBillingValidateMonths(map[string]any{"months": 1.5}); message != "must be integer" {
		t.Errorf("fractional: %q", message)
	}
	if _, message := instanceBillingValidateMonths(map[string]any{"months": float64(25)}); message != "must be <= 24" {
		t.Errorf("maximum: %q", message)
	}
	if _, message := instanceBillingValidateMonths(map[string]any{"months": float64(0)}); message != "must be >= 1" {
		t.Errorf("minimum: %q", message)
	}
	months, message := instanceBillingValidateMonths(map[string]any{"months": "3"})
	if message != "" || months != 3 {
		t.Errorf("coerced string: months=%d message=%q", months, message)
	}
}

// TestInstanceBillingValidateBooleanAndAffCode pins the remaining body schemas.
func TestInstanceBillingValidateBooleanAndAffCode(t *testing.T) {
	if _, message := instanceBillingValidateBoolean(map[string]any{}, "autoRenew"); message != "autoRenew is required" {
		t.Errorf("boolean missing: %q", message)
	}
	if _, message := instanceBillingValidateBoolean(map[string]any{"autoRenew": "yes"}, "autoRenew"); message != "must be boolean" {
		t.Errorf("boolean wrong type: %q", message)
	}
	if value, message := instanceBillingValidateBoolean(map[string]any{"autoRenew": "true"}, "autoRenew"); message != "" || !value {
		t.Errorf("boolean coercion: value=%v message=%q", value, message)
	}

	if _, message := instanceBillingValidateAffCode(map[string]any{}); message != "affCode is required" {
		t.Errorf("affCode missing: %q", message)
	}
	if _, message := instanceBillingValidateAffCode(map[string]any{"affCode": 12}); message != "must be string" {
		t.Errorf("affCode wrong type: %q", message)
	}
	if _, message := instanceBillingValidateAffCode(map[string]any{"affCode": "ab"}); message != "affCode is too short" {
		t.Errorf("affCode too short: %q", message)
	}
	long := make([]rune, 0, 65)
	for index := 0; index < 65; index++ {
		long = append(long, 'a')
	}
	if _, message := instanceBillingValidateAffCode(map[string]any{"affCode": string(long)}); message != "affCode is too long" {
		t.Errorf("affCode too long: %q", message)
	}
	if value, message := instanceBillingValidateAffCode(map[string]any{"affCode": "AFF10"}); message != "" || value != "AFF10" {
		t.Errorf("affCode valid: value=%q message=%q", value, message)
	}
}

// TestInstanceBillingNormalizePlanTrafficLimitSpeed pins the null-vs-undefined
// distinction the plan-change flow relies on.
func TestInstanceBillingNormalizePlanTrafficLimitSpeed(t *testing.T) {
	for _, tc := range []struct {
		raw       string
		want      *string
		wantValid bool
	}{
		{"1Mbit", instanceBillingStringPointer("1Mbit"), true},
		{"1Gbit", instanceBillingStringPointer("1000Mbit"), true},
		{"100", nil, true},
		{"1048576", instanceBillingStringPointer("1Mbit"), true},
		{"2000000", instanceBillingStringPointer("1Mbit"), true},
		{"", nil, true},
		{"0", nil, true},
		{"1024", nil, true},
		{"abc", nil, false},
		{"-1Mbit", nil, false},
	} {
		got, valid := instanceBillingNormalizePlanTrafficLimitSpeed(tc.raw)
		if valid != tc.wantValid {
			t.Errorf("%q: valid = %v, want %v", tc.raw, valid, tc.wantValid)
			continue
		}
		if tc.want == nil {
			if got != nil {
				t.Errorf("%q: got %q, want nil", tc.raw, *got)
			}
			continue
		}
		if got == nil || *got != *tc.want {
			t.Errorf("%q: got %v, want %q", tc.raw, got, *tc.want)
		}
	}
}

// TestInstanceBillingPriceSerializers pins serializePlanPriceYuan /
// serializeInstanceBillingPriceYuan (plan prices are cents, instance prices are
// already yuan).
func TestInstanceBillingPriceSerializers(t *testing.T) {
	if got := instanceBillingPriceYuan(3000); got != 30 {
		t.Errorf("plan 3000 cents = %v, want 30", got)
	}
	if got := instanceBillingPriceYuan(0); got != 0 {
		t.Errorf("plan 0 = %v, want 0", got)
	}

	instancePrice := 25.5
	if got := instanceBillingInstancePriceYuan(&instancePrice, 3000); got != 25.5 {
		t.Errorf("instance price = %v, want 25.5", got)
	}
	zero := 0.0
	if got := instanceBillingInstancePriceYuan(&zero, 3000); got != 30 {
		t.Errorf("zero instance price falls back to plan: %v, want 30", got)
	}
	if got := instanceBillingInstancePriceYuan(nil, 3000); got != 30 {
		t.Errorf("nil instance price falls back to plan: %v, want 30", got)
	}
}

// TestInstanceBillingCapacityMessages pins formatUpgradeCapacityMessage.
func TestInstanceBillingCapacityMessages(t *testing.T) {
	for reason, want := range map[string]string{
		"host_not_found":      "实例所在节点不存在",
		"host_not_online":     "实例所在节点不在线",
		"cpu_insufficient":    "实例所在节点 CPU 配额不足",
		"memory_insufficient": "实例所在节点内存不足",
		"disk_insufficient":   "实例所在节点磁盘容量不足",
		"something_else":      "实例所在节点资源不足",
	} {
		value := reason
		if got := instanceBillingFormatUpgradeCapacityMessage(&value); got != want {
			t.Errorf("%s: %q, want %q", reason, got, want)
		}
	}
	if got := instanceBillingFormatUpgradeCapacityMessage(nil); got != "实例所在节点资源不足" {
		t.Errorf("nil reason: %q", got)
	}
}

// TestInstanceBillingPreviewOptionSerialization pins the wire shape of renewal
// options (ISO-8601 with milliseconds, like Date.prototype.toISOString).
func TestInstanceBillingPreviewOptionSerialization(t *testing.T) {
	expiresAt := time.Date(2026, 12, 11, 12, 34, 56, 789*int(time.Millisecond), time.UTC)
	options := instanceBillingPreviewOptions([]instanceBillingRenewPreview{{
		Months:           3,
		Amount:           90,
		DiscountedAmount: 85.5,
		ExpiresAt:        expiresAt,
	}})
	if len(options) != 1 {
		t.Fatalf("options = %v", options)
	}
	option := options[0]
	if option.Months != 3 || option.Price != 90 || option.DiscountedPrice != 85.5 {
		t.Errorf("option = %+v", option)
	}
	if option.ExpiresAt != "2026-12-11T12:34:56.789Z" {
		t.Errorf("expiresAt = %q", option.ExpiresAt)
	}

	// A non-UTC location must still render in UTC (the DB stores UTC wall time).
	shanghai := time.FixedZone("UTC+8", 8*60*60)
	local := time.Date(2026, 12, 11, 20, 34, 56, 789*int(time.Millisecond), shanghai)
	if got := instanceBillingISOTime(local); got != "2026-12-11T12:34:56.789Z" {
		t.Errorf("local time serialised as %q, want UTC", got)
	}
}

// TestInstanceBillingRecordTypeFilter pins the allowlist (anything else means
// "no filter", matching normalizeBillingRecordType).
func TestInstanceBillingRecordTypeFilter(t *testing.T) {
	if got := instanceBillingNormalizeRecordType("renew"); got != models.BillingRecordTypeRenew {
		t.Errorf("renew = %q", got)
	}
	if got := instanceBillingNormalizeRecordType("transfer_fee"); got != models.BillingRecordTypeTransferFee {
		t.Errorf("transfer_fee = %q", got)
	}
	if got := instanceBillingNormalizeRecordType("nope"); got != "" {
		t.Errorf("unknown type must be ignored, got %q", got)
	}
	if got := instanceBillingNormalizeRecordType(""); got != "" {
		t.Errorf("empty type must be ignored, got %q", got)
	}
}
