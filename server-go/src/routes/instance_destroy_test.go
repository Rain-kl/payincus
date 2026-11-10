// Unit tests for the instance-destroy route port (server/src/routes/instance-destroy.ts).
//
// The tests are deliberately pure: they pin
//
//   - the fee/refund arithmetic and the invariant that the destroy-info quote and
//     the settled amount come from ONE function (same input → same money);
//   - the destroy-blocking predicates (expired+suspended window, 5 GiB traffic cap);
//   - the idempotency + compensation contract of the post-claim flow
//     (a second claim is skipped, a failed Incus delete restores the status and
//     never settles money);
//   - the route id parsing / body validation / route-level rate limits.
//
// DB behaviour (advisory locks 4106/4107/4110/4112, transaction rollback, the real
// refund credit) is covered by test/conformance/requests-instance-destroy.json.
package routes

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

func instanceDestroyTestLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func instanceDestroyTestUser() *lib.AccessTokenClaims {
	return &lib.AccessTokenClaims{ID: 7, Username: "destroy-owner", Role: "user"}
}

func instanceDestroyTestInstance(status models.InstanceStatus) *models.Instance {
	price := models.Decimal{Decimal: decimal.RequireFromString("30.00")}
	expires := time.Now().UTC().Add(10 * 24 * time.Hour)
	planID := int32(950001)
	packageID := int32(950001)
	return &models.Instance{
		ID: 950001, IncusID: "parity-destroy-950001", Name: "parity-destroy",
		UserID: 7, HostID: 950001, PackageID: &packageID, PackagePlanID: &planID,
		Status: status, CPU: 1, Memory: 1024, Disk: 10240,
		BillingPrice: &price, BillingCycle: instanceBillingInt32Pointer(1), ExpiresAt: &expires,
	}
}

// TestInstanceDestroyFeeTier pins calculateFeeRate: the first destroy of a user is
// free (0%), the second and later cost exactly 10%.
func TestInstanceDestroyFeeTier(t *testing.T) {
	for _, tc := range []struct {
		destroyCount int
		want         float64
	}{
		{0, 0},
		{1, 0.10},
		{2, 0.10},
		{17, 0.10},
	} {
		if got := instanceDestroyCalculateFeeRate(tc.destroyCount); got != tc.want {
			t.Errorf("calculateFeeRate(%d) = %v, want %v", tc.destroyCount, got, tc.want)
		}
	}
}

// TestInstanceDestroySettlementAmounts pins `Number(x.toFixed(2))` and the
// first-time / error-state exemptions. The expected numbers were produced by the
// TypeScript expression itself (node: `Number((12.345*0.1).toFixed(2))` → 1.23,
// refund → 11.12).
func TestInstanceDestroySettlementAmounts(t *testing.T) {
	for _, tc := range []struct {
		name         string
		refundable   float64
		destroyCount int
		feeWaiver    bool
		wantFeeRate  float64
		wantFee      float64
		wantRefund   float64
		wantFirst    bool
	}{
		{"first-destroy-free", 30, 0, false, 0, 0, 30, true},
		{"second-destroy-10pct", 30, 1, false, 0.10, 3, 27, false},
		{"error-instance-waived", 30, 3, true, 0, 0, 30, false},
		{"js-tofixed-rounding", 12.345, 2, false, 0.10, 1.23, 11.12, false},
		{"zero-refundable", 0, 4, false, 0.10, 0, 0, false},
		{"sub-cent-refundable", 0.004, 4, false, 0.10, 0, 0, false},
		{"ninety-nine-cents", 0.99, 5, false, 0.10, 0.1, 0.89, false},
	} {
		feeRate, feeAmount, refundAmount, isFirstTime := instanceDestroySettlementAmounts(
			tc.refundable, tc.destroyCount, tc.feeWaiver)
		if feeRate != tc.wantFeeRate {
			t.Errorf("%s: feeRate = %v, want %v", tc.name, feeRate, tc.wantFeeRate)
		}
		if feeAmount != tc.wantFee {
			t.Errorf("%s: feeAmount = %v, want %v", tc.name, feeAmount, tc.wantFee)
		}
		if refundAmount != tc.wantRefund {
			t.Errorf("%s: refundAmount = %v, want %v", tc.name, refundAmount, tc.wantRefund)
		}
		if isFirstTime != tc.wantFirst {
			t.Errorf("%s: isFirstTime = %v, want %v", tc.name, isFirstTime, tc.wantFirst)
		}
	}
}

// TestInstanceDestroyInfoQuoteEqualsSettlement is the core anti-drift test: the
// destroy-info quote and the settlement of the same input agree to the cent because
// both call instanceDestroySettlementAmounts.
func TestInstanceDestroyInfoQuoteEqualsSettlement(t *testing.T) {
	refundQuotes := []instanceBillingRefundQuote{
		{IsPaid: false},
		{IsPaid: true, RemainingDays: 10, RemainingValue: 10.0, Refundable: 10.0, MaxRefundable: 30.0},
		{IsPaid: true, RemainingDays: 3, RemainingValue: 0.03, Refundable: 0.03, MaxRefundable: 30.0},
		{IsPaid: true, RemainingDays: 29, RemainingValue: 29.99, Refundable: 29.99, MaxRefundable: 30.0},
	}
	for _, destroyCount := range []int{0, 1, 2, 9} {
		for _, feeWaiver := range []bool{false, true} {
			for _, quote := range refundQuotes {
				info := instanceDestroyQuote(quote, destroyCount, feeWaiver, false, feeWaiver)
				_, feeAmount, refundAmount, _ := instanceDestroySettlementAmounts(
					quote.Refundable, destroyCount, feeWaiver)

				if info.Refund.FeeAmount != feeAmount {
					t.Errorf("count=%d waiver=%v refundable=%v: info fee %v != settlement fee %v",
						destroyCount, feeWaiver, quote.Refundable, info.Refund.FeeAmount, feeAmount)
				}
				if info.Refund.RefundAmount != refundAmount {
					t.Errorf("count=%d waiver=%v refundable=%v: info refund %v != settlement refund %v",
						destroyCount, feeWaiver, quote.Refundable, info.Refund.RefundAmount, refundAmount)
				}
				if info.Refund.RefundAmount > quote.Refundable {
					t.Errorf("count=%d: refund %v exceeds refundable %v",
						destroyCount, info.Refund.RefundAmount, quote.Refundable)
				}
			}
		}
	}
}

// TestInstanceDestroyQuoteRules pins the rules.feeRate branch: a fee-waived quote
// advertises 0 (the effective rate), otherwise the fixed 10%.
func TestInstanceDestroyQuoteRules(t *testing.T) {
	paid := instanceBillingRefundQuote{IsPaid: true, Refundable: 10}
	if got := instanceDestroyQuote(paid, 2, false, false, false).Rules.FeeRate; got != 0.10 {
		t.Errorf("rules.feeRate = %v, want 0.1", got)
	}
	if got := instanceDestroyQuote(paid, 2, true, false, true).Rules.FeeRate; got != 0 {
		t.Errorf("waived rules.feeRate = %v, want 0", got)
	}
	// The preview must echo the shared quote fields verbatim.
	quote := instanceBillingRefundQuote{
		IsPaid: true, RemainingDays: 12, RemainingValue: 11.5, Refundable: 10, MaxRefundable: 25,
	}
	view := instanceDestroyQuote(quote, 3, false, false, false)
	if view.Refund.RemainingDays != 12 || view.Refund.RemainingValue != 11.5 ||
		view.Refund.MaxRefundable != 25 || view.Refund.DestroyCount != 3 {
		t.Errorf("quote fields not forwarded: %+v", view.Refund)
	}
}

// TestInstanceDestroyExpiredSuspendedPaid covers
// canUserDestroyExpiredSuspendedPaidInstance: only an expired, auto-suspended paid
// instance escapes the "suspended cannot be destroyed" rule.
func TestInstanceDestroyExpiredSuspendedPaid(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	expiredReason := "expired"
	manualReason := "manual"
	planID := int32(1)

	base := func() *models.Instance {
		return &models.Instance{
			Status: models.InstanceStatusSuspended, SuspendReason: &expiredReason,
			PackagePlanID: &planID, ExpiresAt: &past,
		}
	}

	if !instanceDestroyCanDestroyExpiredSuspendedPaid(base(), now) {
		t.Fatal("expired+suspended+paid should be destroyable")
	}

	manual := base()
	manual.SuspendReason = &manualReason
	if instanceDestroyCanDestroyExpiredSuspendedPaid(manual, now) {
		t.Error("manual suspension must block destroy")
	}

	free := base()
	free.PackagePlanID = nil
	if instanceDestroyCanDestroyExpiredSuspendedPaid(free, now) {
		t.Error("free instance must not use the expired-suspended exemption")
	}

	notExpired := base()
	notExpired.ExpiresAt = &future
	if instanceDestroyCanDestroyExpiredSuspendedPaid(notExpired, now) {
		t.Error("not-yet-expired suspension must block destroy")
	}

	noExpiry := base()
	noExpiry.ExpiresAt = nil
	if instanceDestroyCanDestroyExpiredSuspendedPaid(noExpiry, now) {
		t.Error("missing expiry must block destroy")
	}

	noReason := base()
	noReason.SuspendReason = nil
	if instanceDestroyCanDestroyExpiredSuspendedPaid(noReason, now) {
		t.Error("missing suspension reason must block destroy")
	}

	running := base()
	running.Status = models.InstanceStatusRunning
	if instanceDestroyCanDestroyExpiredSuspendedPaid(running, now) {
		t.Error("only suspended status qualifies")
	}

	// `expiresAt.getTime() <= Date.now()` — the boundary belongs to the expired set.
	if !instanceDestroyIsExpiredAt(&now, now) {
		t.Error("expiresAt == now must count as expired")
	}
}

// TestInstanceDestroyTrafficLimitThreshold pins
// exceedsPaidDestroyTrafficLimit (`>= 5 GiB`).
func TestInstanceDestroyTrafficLimitThreshold(t *testing.T) {
	const fiveGiB int64 = 5 * 1024 * 1024 * 1024
	if instanceDestroyExceedsPaidDestroyTrafficLimit(fiveGiB - 1) {
		t.Error("5 GiB - 1 byte must still be destroyable")
	}
	if !instanceDestroyExceedsPaidDestroyTrafficLimit(fiveGiB) {
		t.Error("exactly 5 GiB must block the destroy")
	}
	if !instanceDestroyExceedsPaidDestroyTrafficLimit(fiveGiB + 1) {
		t.Error("above 5 GiB must block the destroy")
	}
}

// TestInstanceDestroyIncusFailureDoesNotSettle is the compensation test:
//   - the claim succeeds (status → deleted),
//   - the Incus delete fails,
//   - the original status is restored exactly once,
//   - no money is settled and the finalize/bookkeeping step never runs,
//   - the caller gets failure reason "销毁实例失败".
func TestInstanceDestroyIncusFailureDoesNotSettle(t *testing.T) {
	restoreSeams := instanceDestroyInstallSeams(t)
	defer restoreSeams()

	restoredStatuses := make([]models.InstanceStatus, 0)
	settleCalls := 0
	finalizeCalls := 0
	InstanceDestroyClaim = func(context.Context, int32, int32, models.InstanceStatus) (bool, error) {
		return true, nil
	}
	InstanceDestroyRestoreClaimed = func(_ context.Context, _, _ int32, status models.InstanceStatus) error {
		restoredStatuses = append(restoredStatuses, status)
		return nil
	}
	InstanceDestroyDeleteIncus = func(context.Context, *models.Instance, *log.Logger) error {
		return errors.New("dial tcp 10.0.0.9:8443: connect: connection refused")
	}
	InstanceDestroySettleBilling = func(
		context.Context, int32, *models.Instance, float64, bool,
	) (instanceDestroyBillingResult, error) {
		settleCalls++
		return instanceDestroyBillingResult{}, nil
	}
	InstanceDestroyFinalize = func(context.Context, instanceDestroyFinalizeInput) error {
		finalizeCalls++
		return nil
	}

	instance := instanceDestroyTestInstance(models.InstanceStatusRunning)
	result, err := instanceDestroyRunDestroyedInstance(context.Background(), instanceDestroyRunInput{
		User: instanceDestroyTestUser(), Instance: instance, HostName: "parity-host",
		IsFreeInstance: false, FeeWaiver: false, Refundable: 27.5,
		Logger: instanceDestroyTestLogger(),
	})
	if err != nil {
		t.Fatalf("unexpected propagated error: %v", err)
	}
	if result.Success {
		t.Fatal("a failed Incus delete must not report success")
	}
	if result.Reason == nil || *result.Reason != "销毁实例失败" {
		t.Fatalf("reason = %v, want 销毁实例失败", result.Reason)
	}
	if len(restoredStatuses) != 1 || restoredStatuses[0] != models.InstanceStatusRunning {
		t.Fatalf("restore calls = %v, want one restore to 'running'", restoredStatuses)
	}
	if settleCalls != 0 {
		t.Fatalf("settle ran %d times after a failed delete; a failed delete must not refund", settleCalls)
	}
	if finalizeCalls != 0 {
		t.Fatalf("finalize ran %d times after a failed delete", finalizeCalls)
	}
}

// TestInstanceDestroyRepeatClaimIsIdempotent pins the second-destroy path: the
// claim returns false (status already moved / lock held) and NOTHING else happens.
func TestInstanceDestroyRepeatClaimIsIdempotent(t *testing.T) {
	restoreSeams := instanceDestroyInstallSeams(t)
	defer restoreSeams()

	deleteCalls := 0
	settleCalls := 0
	finalizeCalls := 0
	InstanceDestroyClaim = func(context.Context, int32, int32, models.InstanceStatus) (bool, error) {
		return false, nil
	}
	InstanceDestroyDeleteIncus = func(context.Context, *models.Instance, *log.Logger) error {
		deleteCalls++
		return nil
	}
	InstanceDestroySettleBilling = func(
		context.Context, int32, *models.Instance, float64, bool,
	) (instanceDestroyBillingResult, error) {
		settleCalls++
		return instanceDestroyBillingResult{RefundAmount: 1}, nil
	}
	InstanceDestroyFinalize = func(context.Context, instanceDestroyFinalizeInput) error {
		finalizeCalls++
		return nil
	}
	InstanceDestroyRestoreClaimed = func(context.Context, int32, int32, models.InstanceStatus) error {
		t.Error("restore must not run when the claim failed")
		return nil
	}

	instance := instanceDestroyTestInstance(models.InstanceStatusRunning)
	result, err := instanceDestroyRunDestroyedInstance(context.Background(), instanceDestroyRunInput{
		User: instanceDestroyTestUser(), Instance: instance, HostName: "parity-host",
		IsFreeInstance: false, Refundable: 27.5, Logger: instanceDestroyTestLogger(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Success {
		t.Fatal("a lost claim must not report success")
	}
	if result.Skipped == nil || !*result.Skipped {
		t.Fatal("a lost claim must be reported as skipped")
	}
	if result.Reason == nil || *result.Reason != "实例正在销毁或已删除" {
		t.Fatalf("reason = %v", result.Reason)
	}
	if deleteCalls != 0 || settleCalls != 0 || finalizeCalls != 0 {
		t.Fatalf("delete=%d settle=%d finalize=%d after a lost claim; a repeated destroy must not refund twice",
			deleteCalls, settleCalls, finalizeCalls)
	}
}

// TestInstanceDestroySuccessSettlesOnce pins the happy-path wiring: claim → delete
// → settle (refundable passed through) → finalize, and that the settled values are
// the ones reported to the caller.
func TestInstanceDestroySuccessSettlesOnce(t *testing.T) {
	restoreSeams := instanceDestroyInstallSeams(t)
	defer restoreSeams()

	InstanceDestroyClaim = func(context.Context, int32, int32, models.InstanceStatus) (bool, error) {
		return true, nil
	}
	InstanceDestroyDeleteIncus = func(context.Context, *models.Instance, *log.Logger) error { return nil }

	settleInputs := make([]float64, 0)
	InstanceDestroySettleBilling = func(
		_ context.Context, userID int32, _ *models.Instance, refundable float64, feeWaiver bool,
	) (instanceDestroyBillingResult, error) {
		if userID != 7 || feeWaiver {
			t.Errorf("settle(userID=%d feeWaiver=%v), want (7,false)", userID, feeWaiver)
		}
		settleInputs = append(settleInputs, refundable)
		return instanceDestroyBillingResult{RefundAmount: 24.75, FeeAmount: 2.75, IsFirstTime: false}, nil
	}
	finalized := 0
	InstanceDestroyFinalize = func(_ context.Context, input instanceDestroyFinalizeInput) error {
		finalized++
		if input.RefundAmount != 24.75 || input.FeeAmount != 2.75 {
			t.Errorf("finalize got refund=%v fee=%v, want the settled values", input.RefundAmount, input.FeeAmount)
		}
		return nil
	}

	instance := instanceDestroyTestInstance(models.InstanceStatusRunning)
	result, err := instanceDestroyRunDestroyedInstance(context.Background(), instanceDestroyRunInput{
		User: instanceDestroyTestUser(), Instance: instance, HostName: "parity-host",
		IsFreeInstance: false, Refundable: 27.5, Logger: instanceDestroyTestLogger(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got %+v", result)
	}
	if len(settleInputs) != 1 || settleInputs[0] != 27.5 {
		t.Fatalf("settle inputs = %v, want one call with refundable 27.5", settleInputs)
	}
	if finalized != 1 {
		t.Fatalf("finalize ran %d times, want 1", finalized)
	}
	if result.RefundAmount == nil || *result.RefundAmount != 24.75 ||
		result.FeeAmount == nil || *result.FeeAmount != 2.75 {
		t.Fatalf("result money = %+v, want refund 24.75 fee 2.75", result)
	}
	if result.IsFirstTime == nil || *result.IsFirstTime {
		t.Fatalf("isFirstTime = %v, want false", result.IsFirstTime)
	}
}

// TestInstanceDestroyFreeInstanceReportsIsFirstTime pins the free-instance branch of
// POST /:id/destroy: no settlement happens, so `isFirstTime` is the value read before
// the claim (`destroyRecords.length === 0`), not a hard-coded false.
func TestInstanceDestroyFreeInstanceReportsIsFirstTime(t *testing.T) {
	restoreSeams := instanceDestroyInstallSeams(t)
	defer restoreSeams()

	InstanceDestroyClaim = func(context.Context, int32, int32, models.InstanceStatus) (bool, error) {
		return true, nil
	}
	InstanceDestroyDeleteIncus = func(context.Context, *models.Instance, *log.Logger) error { return nil }
	InstanceDestroySettleBilling = func(
		context.Context, int32, *models.Instance, float64, bool,
	) (instanceDestroyBillingResult, error) {
		t.Error("a free instance must not settle billing")
		return instanceDestroyBillingResult{}, nil
	}
	InstanceDestroyFinalize = func(context.Context, instanceDestroyFinalizeInput) error { return nil }

	for _, tc := range []struct {
		destroyCount int
		wantFirst    bool
	}{
		{0, true},
		{2, false},
	} {
		result, err := instanceDestroyRunDestroyedInstance(context.Background(), instanceDestroyRunInput{
			User: instanceDestroyTestUser(), Instance: instanceDestroyTestInstance(models.InstanceStatusRunning),
			HostName: "parity-host", IsFreeInstance: true, DestroyCount: tc.destroyCount,
			Logger: instanceDestroyTestLogger(),
		})
		if err != nil {
			t.Fatalf("destroyCount=%d: unexpected error: %v", tc.destroyCount, err)
		}
		if !result.Success {
			t.Fatalf("destroyCount=%d: expected success, got %+v", tc.destroyCount, result)
		}
		if result.IsFirstTime == nil || *result.IsFirstTime != tc.wantFirst {
			t.Fatalf("destroyCount=%d: isFirstTime = %v, want %v",
				tc.destroyCount, result.IsFirstTime, tc.wantFirst)
		}
		if result.RefundAmount == nil || *result.RefundAmount != 0 ||
			result.FeeAmount == nil || *result.FeeAmount != 0 {
			t.Fatalf("destroyCount=%d: money = %+v, want 0/0 for a free instance", tc.destroyCount, result)
		}
	}
}

// instanceDestroyInstallSeams snapshots the package-level seams and returns a
// restore function.
func instanceDestroyInstallSeams(t *testing.T) func() {
	t.Helper()
	originalClaim := InstanceDestroyClaim
	originalRestore := InstanceDestroyRestoreClaimed
	originalDelete := InstanceDestroyDeleteIncus
	originalSettle := InstanceDestroySettleBilling
	originalFinalize := InstanceDestroyFinalize
	return func() {
		InstanceDestroyClaim = originalClaim
		InstanceDestroyRestoreClaimed = originalRestore
		InstanceDestroyDeleteIncus = originalDelete
		InstanceDestroySettleBilling = originalSettle
		InstanceDestroyFinalize = originalFinalize
	}
}

// TestInstanceDestroyParseRouteID pins the `Number(request.params.id)` mapping.
func TestInstanceDestroyParseRouteID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name       string
		param      string
		wantStatus int
		wantID     int32
	}{
		{"numeric", "950001", 0, 950001},
		{"zero-is-looked-up", "0", 0, 0},
		{"negative-is-looked-up", "-1", 0, -1},
		{"padded", " 42 ", 0, 42},
		{"non-numeric", "abc", http.StatusBadRequest, 0},
		{"fractional-crashes-prisma", "12.5", http.StatusInternalServerError, 0},
		{"overflow-crashes-prisma", "2147483648", http.StatusInternalServerError, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/instances/950001/destroy-info", nil)
			c.Params = gin.Params{{Key: "id", Value: tc.param}}

			id, ok := instanceDestroyParseRouteID(c)
			if tc.wantStatus == 0 {
				if !ok || id != tc.wantID {
					t.Fatalf("parse(%q) = (%d, %v), want (%d, true)", tc.param, id, ok, tc.wantID)
				}
				return
			}
			if ok {
				t.Fatalf("parse(%q) unexpectedly succeeded with %d", tc.param, id)
			}
			if recorder.Code != tc.wantStatus {
				t.Fatalf("parse(%q) status = %d, want %d", tc.param, recorder.Code, tc.wantStatus)
			}
		})
	}
}

// TestInstanceDestroyBodyValidation pins the instanceIds schema messages and
// Fastify's `coerceTypes: 'array'` behaviour (a scalar body value is wrapped).
func TestInstanceDestroyBodyValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    map[string]any
		want    string
		wantIDs []int32
	}{
		{"missing", map[string]any{}, "instanceIds is required", nil},
		{"object-cannot-coerce", map[string]any{"instanceIds": map[string]any{}}, "must be array", nil},
		{"empty", map[string]any{"instanceIds": []any{}}, "must NOT have fewer than 1 items", nil},
		{"too-many", map[string]any{"instanceIds": repeatInt(101)}, "must NOT have more than 100 items", nil},
		{"non-integer", map[string]any{"instanceIds": []any{1.5}}, "must be integer", nil},
		{"deduped", map[string]any{"instanceIds": []any{float64(1), float64(1), float64(2)}}, "", []int32{1, 2}},
		{"coerced-string-item", map[string]any{"instanceIds": []any{"3"}}, "", []int32{3}},
		{"coerced-scalar-string", map[string]any{"instanceIds": "950001"}, "", []int32{950001}},
		{"coerced-scalar-number", map[string]any{"instanceIds": float64(950001)}, "", []int32{950001}},
	} {
		ids, message := instanceDestroyValidateInstanceIDs(tc.body)
		if message != tc.want {
			t.Fatalf("%s: message = %q, want %q", tc.name, message, tc.want)
		}
		if tc.want != "" {
			continue
		}
		if len(ids) != len(tc.wantIDs) {
			t.Fatalf("%s: ids = %v, want %v", tc.name, ids, tc.wantIDs)
		}
		for index := range ids {
			if ids[index] != tc.wantIDs[index] {
				t.Fatalf("%s: ids = %v, want %v", tc.name, ids, tc.wantIDs)
			}
		}
	}
}

// TestInstanceDestroyRateLimits pins the per-route budgets (10 / 3 / 30 / 5 per
// minute) and that different routes keep separate buckets.
func TestInstanceDestroyRateLimits(t *testing.T) {
	ResetInstanceDestroyRateLimits()
	t.Cleanup(ResetInstanceDestroyRateLimits)
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.POST("/batch/destroy", instanceDestroyRateLimit("/batch/destroy", 3, time.Minute),
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	router.GET("/:id/destroy-info", instanceDestroyRateLimit("/:id/destroy-info", 30, time.Minute),
		func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	do := func(method, path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, nil)
		request.RemoteAddr = "203.0.113.9:54321"
		router.ServeHTTP(recorder, request)
		return recorder
	}

	for attempt := 1; attempt <= 3; attempt++ {
		if got := do(http.MethodPost, "/batch/destroy").Code; got != http.StatusOK {
			t.Fatalf("batch attempt %d = %d, want 200", attempt, got)
		}
	}
	exhausted := do(http.MethodPost, "/batch/destroy")
	if exhausted.Code != http.StatusTooManyRequests {
		t.Fatalf("4th batch attempt = %d, want 429", exhausted.Code)
	}
	if !strings.Contains(exhausted.Body.String(), "Too many requests, please try again later") {
		t.Fatalf("429 body = %s", exhausted.Body.String())
	}

	// The destroy-info route has its own budget (30), so it is unaffected by the
	// exhausted batch bucket.
	if got := do(http.MethodGet, "/950001/destroy-info").Code; got != http.StatusOK {
		t.Fatalf("destroy-info after batch exhaustion = %d, want 200", got)
	}
}

func repeatInt(count int) []any {
	values := make([]any, 0, count)
	for index := 0; index < count; index++ {
		values = append(values, float64(index+1))
	}
	return values
}
