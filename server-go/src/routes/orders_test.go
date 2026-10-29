package routes

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// ==================== ids and paging ====================

// TestOrdersParsePositiveID pins the strict route-id contract shared with the guard
// suites: only canonical positive decimals within Number.MAX_SAFE_INTEGER.
func TestOrdersParsePositiveID(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
		ok   bool
	}{
		{"1", 1, true},
		{"42", 42, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"01", 0, false},
		{"1.0", 0, false},
		{"1e3", 0, false},
		{"", 0, false},
		{"abc", 0, false},
		{"9007199254740993", 0, false}, // beyond Number.MAX_SAFE_INTEGER
		// A syntactically valid but int32-overflowing id parses; the handler then
		// refuses to truncate it into some other order's id.
		{"9007199254740991", 9007199254740991, true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, ok := ordersParsePositiveID(tc.raw)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if tc.ok && got != tc.want {
				t.Fatalf("value = %d, want %d", got, tc.want)
			}
		})
	}

	if narrowed, ok := ordersNarrowOrderID(42); !ok || narrowed != 42 {
		t.Fatalf("narrow(42) = %d ok=%v", narrowed, ok)
	}
	if _, ok := ordersNarrowOrderID(1 << 40); ok {
		t.Fatalf("int32 overflow must not be representable")
	}
}

// TestOrdersParsePageSizeClamps pins parsePageSize: fallback 20, clamped to 100,
// and every malformed input falls back to the default rather than erroring.
func TestOrdersParsePageSizeClamps(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", 20},
		{"1", 1},
		{"100", 100},
		{"101", 100},
		{"100000", 100},
		{"0", 20},
		{"-2", 20},
		{"02", 20},
		{"1.5", 20},
		{"nope", 20},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			if got := ordersParsePageSize(tc.raw); got != tc.want {
				t.Fatalf("pageSize(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
	if got := ordersParsePositiveQuery("", 1); got != 1 {
		t.Fatalf("page fallback = %d, want 1", got)
	}
	if got := ordersParsePositiveQuery("900", 1); got != 900 {
		t.Fatalf("page = %d, want 900", got)
	}
}

// ==================== filters ====================

// TestOrdersNormalizeFilters pins which query values are accepted. Crucially the
// same `?status=` parameter is interpreted as a recharge status OR a billing type
// depending on `?type=`, exactly as in the TypeScript.
func TestOrdersNormalizeFilters(t *testing.T) {
	if got := ordersNormalizeOrderType("recharge"); got != "recharge" {
		t.Fatalf("type recharge = %q", got)
	}
	if got := ordersNormalizeOrderType("instance_billing"); got != "instance_billing" {
		t.Fatalf("type instance_billing = %q", got)
	}
	for _, bogus := range []string{"", "all", "RECHARGE", " billing", "refund"} {
		if got := ordersNormalizeOrderType(bogus); got != "" {
			t.Fatalf("type %q = %q, want empty", bogus, got)
		}
	}

	for _, status := range []string{"pending", "paid", "completed", "failed", "cancelled", "refunded"} {
		if got := ordersNormalizeRechargeStatus(status); got != status {
			t.Fatalf("recharge status %q = %q", status, got)
		}
	}
	for _, bogus := range []string{"", "COMPLETED", "success", "newPurchase"} {
		if got := ordersNormalizeRechargeStatus(bogus); got != "" {
			t.Fatalf("recharge status %q = %q, want empty", bogus, got)
		}
	}

	for _, billingType := range []string{"newPurchase", "renew", "upgrade", "downgrade", "refund", "transfer_fee"} {
		if got := ordersNormalizeBillingType(billingType); got != billingType {
			t.Fatalf("billing type %q = %q", billingType, got)
		}
	}
	for _, bogus := range []string{"", "new_purchase", "completed", "renew "} {
		if got := ordersNormalizeBillingType(bogus); got != "" {
			t.Fatalf("billing type %q = %q, want empty", bogus, got)
		}
	}
}

// TestOrdersNormalizeOperationStatus pins the state machine's input gate: an
// unknown status silently degrades to pending_review (never an error), and all
// four enum values round-trip.
func TestOrdersNormalizeOperationStatus(t *testing.T) {
	for _, status := range []string{"pending_review", "confirmed", "compensated", "closed"} {
		if got := ordersNormalizeOperationStatus(status); got != status {
			t.Fatalf("status %q = %q", status, got)
		}
	}
	for _, bogus := range []any{"", "done", "PENDING_REVIEW", nil, 7.0, true} {
		if got := ordersNormalizeOperationStatus(bogus); got != "pending_review" {
			t.Fatalf("status %#v = %q, want pending_review", bogus, got)
		}
	}
}

// TestOrdersNormalizeSearch pins trim + slice(0, 128) and the empty→nil mapping.
func TestOrdersNormalizeSearch(t *testing.T) {
	if got := ordersNormalizeSearch("   "); got != nil {
		t.Fatalf("blank search = %#v, want nil", got)
	}
	if got := ordersNormalizeSearch("  abc  "); got == nil || *got != "abc" {
		t.Fatalf("trimmed search = %#v", got)
	}
	long := make([]rune, 200)
	for index := range long {
		long[index] = 'a'
	}
	got := ordersNormalizeSearch(string(long))
	if got == nil || len([]rune(*got)) != 128 {
		t.Fatalf("long search length = %d, want 128", len([]rune(*got)))
	}
}

// TestOrdersParseDateQuery pins that an unparseable date is silently dropped
// (never a 400), matching parseDateQuery.
func TestOrdersParseDateQuery(t *testing.T) {
	if got := ordersParseDateQuery(""); got != nil {
		t.Fatalf("empty date = %v, want nil", got)
	}
	if got := ordersParseDateQuery("not-a-date"); got != nil {
		t.Fatalf("invalid date = %v, want nil", got)
	}
	got := ordersParseDateQuery("2026-01-02T03:04:05.000Z")
	if got == nil || got.UTC().Format(time.RFC3339) != "2026-01-02T03:04:05Z" {
		t.Fatalf("valid date = %v", got)
	}
}

// ==================== money ====================

// TestOrdersToMoneyShape pins the JSON form of the order amounts: the TypeScript
// calls toMoney()/Number() before returning, so amounts are JSON numbers and must
// never be emitted as Prisma-style decimal strings.
func TestOrdersToMoneyShape(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
	}{
		{"100.00", 100},
		{"12.50", 12.5},
		{"0.01", 0.01},
		{"99999999.99", 99999999.99},
		{"0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got := ordersToMoney(ordersDecimalFromText(tc.raw))
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("toMoney(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if encoded[0] == '"' {
				t.Fatalf("amount must be a JSON number, got %s", encoded)
			}
		})
	}

	// A malformed numeric column degrades to 0 rather than panicking.
	if got := ordersToMoney(ordersDecimalFromText("not-a-number")); got != 0 {
		t.Fatalf("malformed decimal = %v, want 0", got)
	}

	// Optional amounts keep the null branch.
	if got := ordersOptionalMoney(nil); got != nil {
		t.Fatalf("nil optional amount = %v", got)
	}
	value := "5.25"
	if got := ordersOptionalMoney(&value); got == nil || *got != 5.25 {
		t.Fatalf("optional amount = %v", got)
	}
}

// TestOrdersMaskTradeNo pins the two masking branches and the null cases; the raw
// trade number must never leak.
func TestOrdersMaskTradeNo(t *testing.T) {
	cases := []struct {
		name string
		raw  *string
		want *string
	}{
		{"nil", nil, nil},
		{"empty", orderStrPtr(""), nil},
		{"short", orderStrPtr("AB123456"), orderStrPtr("AB***")},
		{"long", orderStrPtr("2026010212345678"), orderStrPtr("2026***5678")},
		{"nine chars", orderStrPtr("123456789"), orderStrPtr("1234***6789")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ordersMaskTradeNo(tc.raw)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %q, want nil", *got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("got %v, want %q", got, *tc.want)
			}
		})
	}
}

// ==================== status mapping ====================

// TestOrdersStatusMapping pins the derived status of both order families. This is
// the read-side of the order state machine: a recharge in `paid` is still pending
// on the customer-facing timeline, while billing rows only ever surface as
// completed (or refunded for the refund type).
func TestOrdersStatusMapping(t *testing.T) {
	rechargeCases := map[string]string{
		"pending":   "pending",
		"paid":      "pending",
		"completed": "completed",
		"failed":    "failed",
		"cancelled": "cancelled",
		"refunded":  "refunded",
	}
	for raw, want := range rechargeCases {
		if got := ordersMapRechargeStatus(raw); got != want {
			t.Fatalf("mapRechargeStatus(%q) = %q, want %q", raw, got, want)
		}
	}
	if got := ordersMapRechargeStatus("weird"); got != "pending" {
		t.Fatalf("unknown recharge status = %q, want pending", got)
	}

	for _, billingType := range []string{"newPurchase", "renew", "upgrade", "downgrade", "transfer_fee"} {
		if got := ordersMapBillingStatus(billingType); got != "completed" {
			t.Fatalf("mapBillingStatus(%q) = %q, want completed", billingType, got)
		}
	}
	if got := ordersMapBillingStatus("refund"); got != "refunded" {
		t.Fatalf("mapBillingStatus(refund) = %q, want refunded", got)
	}
}

// TestOrdersBillingOrderShape pins the synthesized billing order fields: the
// BILL- id/orderNo, the per-type Chinese title, `fee: 0`, the balance payment
// method, completedAt == createdAt and the null provider.
func TestOrdersBillingOrderShape(t *testing.T) {
	createdAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	row := ordersBillingRow{
		ID:           77,
		InstanceID:   9,
		UserID:       3,
		Type:         "renew",
		Amount:       "12.50",
		Months:       2,
		PeriodStart:  createdAt,
		PeriodEnd:    createdAt.AddDate(0, 2, 0),
		CreatedAt:    createdAt,
		InstanceRef:  orderInt32Ptr(9),
		InstanceName: orderStrPtr("web-1"),
		PlanID:       orderInt32Ptr(5),
		PlanName:     orderStrPtr("Pro"),
		PackageID:    orderInt32Ptr(6),
		PackageName:  orderStrPtr("Cloud"),
	}

	order := ordersBuildBillingOrder(row, nil)
	if order.ID != "instance_billing:77" {
		t.Fatalf("id = %q", order.ID)
	}
	if order.OrderNo != "BILL-77" {
		t.Fatalf("orderNo = %q", order.OrderNo)
	}
	if order.Title != "实例续费" {
		t.Fatalf("title = %q", order.Title)
	}
	if order.Status != "completed" || order.RawStatus != "renew" {
		t.Fatalf("status = %q / raw = %q", order.Status, order.RawStatus)
	}
	if order.Amount != 12.5 || order.ActualAmount == nil || *order.ActualAmount != 12.5 {
		t.Fatalf("amount = %v actual = %v", order.Amount, order.ActualAmount)
	}
	if order.Fee != 0 {
		t.Fatalf("fee = %v, want 0", order.Fee)
	}
	if order.PaymentMethod == nil || *order.PaymentMethod != "balance" {
		t.Fatalf("paymentMethod = %v", order.PaymentMethod)
	}
	if order.Provider != nil || order.ProviderStatusSummary != nil || order.TradeNo != nil {
		t.Fatalf("billing orders must not carry provider data")
	}
	if order.CompletedAt == nil || *order.CompletedAt != order.CreatedAt {
		t.Fatalf("completedAt = %v, want createdAt", order.CompletedAt)
	}
	if order.Months == nil || *order.Months != 2 {
		t.Fatalf("months = %v", order.Months)
	}
	if order.Instance == nil || order.Instance.PackagePlan == nil || order.Instance.PackagePlan.Package == nil {
		t.Fatalf("instance projection is incomplete: %#v", order.Instance)
	}
	if order.CreatedAt != "2026-03-04T05:06:07.000Z" {
		t.Fatalf("createdAt = %q", order.CreatedAt)
	}

	// An unknown billing type falls back to the generic title.
	row.Type = "mystery"
	if got := ordersBuildBillingOrder(row, nil).Title; got != "实例账单" {
		t.Fatalf("fallback title = %q", got)
	}
}

// TestOrdersRechargeOrderShape pins the recharge projection: masked trade number,
// the nested provider, the ISO timestamps and the null billing-only fields.
func TestOrdersRechargeOrderShape(t *testing.T) {
	createdAt := time.Date(2026, 5, 6, 7, 8, 9, 500*int(time.Millisecond), time.UTC)
	completedAt := createdAt.Add(time.Minute)
	row := ordersRechargeRow{
		ID:            11,
		UserID:        4,
		OrderNo:       "R20260506001",
		TradeNo:       orderStrPtr("2026010212345678"),
		Amount:        "100.00",
		ActualAmount:  orderStrPtr("99.10"),
		Fee:           "0.90",
		PaymentMethod: orderStrPtr("alipay"),
		Status:        "completed",
		CreatedAt:     createdAt,
		CompletedAt:   &completedAt,
		ProviderID:    orderInt32Ptr(2),
		ProviderName:  orderStrPtr("AliPay"),
		ProviderType:  orderStrPtr("alipay"),
	}
	user := &orderUserRef{ID: 4, Username: "alice"}

	order := ordersBuildRechargeOrder(row)
	if order.ID != "recharge:11" || order.SourceType != "recharge" || order.SourceID != 11 {
		t.Fatalf("identity = %q %q %d", order.ID, order.SourceType, order.SourceID)
	}
	if order.Title != "余额充值" {
		t.Fatalf("title = %q", order.Title)
	}
	if order.Status != "completed" || order.RawStatus != "completed" {
		t.Fatalf("status = %q / %q", order.Status, order.RawStatus)
	}
	if order.Amount != 100 || order.Fee != 0.9 {
		t.Fatalf("amount = %v fee = %v", order.Amount, order.Fee)
	}
	if order.ActualAmount == nil || *order.ActualAmount != 99.1 {
		t.Fatalf("actualAmount = %v", order.ActualAmount)
	}
	if order.TradeNo == nil || *order.TradeNo != "2026010212345678" {
		t.Fatalf("detail tradeNo must stay unmasked, got %v", order.TradeNo)
	}
	if order.ProviderStatusSummary == nil || order.ProviderStatusSummary.TradeNo == nil ||
		*order.ProviderStatusSummary.TradeNo != "2026***5678" {
		t.Fatalf("summary tradeNo must be masked, got %#v", order.ProviderStatusSummary)
	}
	if order.Provider == nil || order.Provider.Type != "alipay" {
		t.Fatalf("provider = %#v", order.Provider)
	}
	if order.Instance != nil || order.Months != nil || order.PeriodStart != nil || order.Remark != nil {
		t.Fatalf("recharge orders must null the billing-only fields")
	}
	if order.CreatedAt != "2026-05-06T07:08:09.500Z" {
		t.Fatalf("createdAt = %q", order.CreatedAt)
	}
	if order.CompletedAt == nil || *order.CompletedAt != "2026-05-06T07:09:09.500Z" {
		t.Fatalf("completedAt = %q", *order.CompletedAt)
	}

	// User is only attached when the row carries the joined columns.
	if order.User != nil {
		t.Fatalf("user must stay null without the join")
	}
	withUser := ordersRechargeOrderWithUser(row, user)
	if withUser.User == nil || withUser.User.Username != "alice" {
		t.Fatalf("user = %#v", withUser.User)
	}
}

// ordersRechargeOrderWithUser attaches the user projection the way the SQL join
// does, so the null-vs-present branch is covered without a database.
func ordersRechargeOrderWithUser(row ordersRechargeRow, user *orderUserRef) orderView {
	row.UserRefID = &user.ID
	row.UserRefName = &user.Username
	row.UserRefEmail = user.Email
	return ordersBuildRechargeOrder(row)
}

// ==================== operation input ====================

// TestOrdersRefundAmountPrecision pins normalizeRefundAmount: two-decimal rounding,
// the > 0 bound, the MAX_REFUND_AMOUNT bound, and the rejection of exponent-shaped
// values that String(number) would render outside /^\d+(\.\d{1,2})?$/.
func TestOrdersRefundAmountPrecision(t *testing.T) {
	cases := []struct {
		name    string
		value   any
		present bool
		want    *float64
		wantErr string
	}{
		{name: "absent optional", value: nil, present: false},
		{name: "explicit null optional", value: nil, present: true},
		{name: "absent required", value: nil, present: false, wantErr: "必须填写退款金额"},
		{name: "zero", value: 0.0, present: true, wantErr: "退款金额必须大于 0，最多两位小数"},
		{name: "negative", value: -1.0, present: true, wantErr: "退款金额必须大于 0，最多两位小数"},
		{name: "three decimals", value: 1.234, present: true, wantErr: "退款金额必须大于 0，最多两位小数"},
		{name: "over max", value: 100000000.0, present: true, wantErr: "退款金额必须大于 0，最多两位小数"},
		{name: "string rejected", value: "1.00", present: true, wantErr: "退款金额必须是有效数字"},
		{name: "bool rejected", value: true, present: true, wantErr: "退款金额必须是有效数字"},
		{name: "exact max", value: 99999999.99, present: true, want: orderFloatPtr(99999999.99)},
		{name: "two decimals", value: 12.34, present: true, want: orderFloatPtr(12.34)},
		{name: "integer", value: 5.0, present: true, want: orderFloatPtr(5)},
		{name: "float noise rounds", value: 0.1 + 0.2, present: true, want: orderFloatPtr(0.3)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ordersNormalizeRefundAmount(tc.value, tc.present, tc.wantErr == "必须填写退款金额")
			if tc.wantErr != "" {
				if err == nil || err.Message != tc.wantErr {
					t.Fatalf("error = %#v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err.Message)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("value = %v, want nil", *got)
				}
				return
			}
			if got == nil || math.Abs(*got-*tc.want) > 1e-9 {
				t.Fatalf("value = %v, want %v", got, *tc.want)
			}
		})
	}
}

// TestOrdersOperationInputStateMachine pins the full body normalization: the reason
// is mandatory, the status defaults to pending_review, refundAmount is mandatory
// exactly when createRefundRequest is true, and the text length cap is UTF-16.
func TestOrdersOperationInputStateMachine(t *testing.T) {
	// Happy path without a refund request.
	input, err := ordersNormalizeOperationInput(map[string]any{
		"status": "confirmed",
		"reason": "  已核实  ",
		"result": "补偿到位",
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Message)
	}
	if input.Status != "confirmed" || input.Reason != "已核实" {
		t.Fatalf("input = %#v", input)
	}
	if input.Result == nil || *input.Result != "补偿到位" {
		t.Fatalf("result = %v", input.Result)
	}
	if input.CreateRefundRequest || input.RefundAmount != nil {
		t.Fatalf("refund must stay unset")
	}

	// Unknown status degrades; missing reason fails.
	input, err = ordersNormalizeOperationInput(map[string]any{"status": "bogus", "reason": "x"})
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Message)
	}
	if input.Status != "pending_review" {
		t.Fatalf("status = %q, want pending_review", input.Status)
	}
	if _, err := ordersNormalizeOperationInput(map[string]any{"status": "closed"}); err == nil ||
		err.Message != "必须填写处理原因" {
		t.Fatalf("missing reason error = %#v", err)
	}
	if _, err := ordersNormalizeOperationInput(map[string]any{"reason": "   "}); err == nil ||
		err.Message != "必须填写处理原因" {
		t.Fatalf("blank reason error = %#v", err)
	}
	if _, err := ordersNormalizeOperationInput(map[string]any{"reason": 42.0}); err == nil ||
		err.Message != "处理原因必须是字符串" {
		t.Fatalf("non-string reason error = %#v", err)
	}
	if _, err := ordersNormalizeOperationInput([]any{}); err == nil || err.Message != "请求参数无效" {
		t.Fatalf("non-object body error = %#v", err)
	}

	// createRefundRequest=true makes the amount mandatory.
	if _, err := ordersNormalizeOperationInput(map[string]any{
		"reason":              "退款",
		"createRefundRequest": true,
	}); err == nil || err.Message != "必须填写退款金额" {
		t.Fatalf("mandatory refund error = %#v", err)
	}

	input, err = ordersNormalizeOperationInput(map[string]any{
		"reason":              "退款",
		"createRefundRequest": true,
		"refundAmount":        10.5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Message)
	}
	if !input.CreateRefundRequest || input.RefundAmount == nil || *input.RefundAmount != 10.5 {
		t.Fatalf("input = %#v", input)
	}
	if !input.RefundAmountDecimal.Equal(decimal.NewFromFloat(10.5)) {
		t.Fatalf("decimal = %s", input.RefundAmountDecimal)
	}

	// Only a literal boolean true opts in.
	input, err = ordersNormalizeOperationInput(map[string]any{
		"reason":              "x",
		"createRefundRequest": "true",
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Message)
	}
	if input.CreateRefundRequest {
		t.Fatalf("createRefundRequest must be strict `=== true`")
	}

	// The text cap counts UTF-16 code units, so 500 astral chars (1000 units) fail.
	astral := make([]rune, ordersOperationTextMax)
	for index := range astral {
		astral[index] = '𝄞'
	}
	if _, err := ordersNormalizeOperationInput(map[string]any{
		"reason": string(astral),
	}); err == nil {
		t.Fatalf("over-long reason must be rejected")
	}
}

// TestOrdersOperationInputResultNilIsAbsent pins the tri-state: an explicit JSON
// null result behaves like an absent key (Prisma skips it on update).
func TestOrdersOperationInputResultNilIsAbsent(t *testing.T) {
	input, err := ordersNormalizeOperationInput(map[string]any{
		"reason": "x",
		"result": nil,
		"status": "closed",
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Message)
	}
	if input.Result != nil {
		t.Fatalf("result = %v, want nil", *input.Result)
	}
}

// ==================== billing order keyword ====================

// TestOrdersParseBillingOrderKeyword pins the `BILL-<id>` / bare-digit keyword
// parsing used by the instance-billing search branch.
func TestOrdersParseBillingOrderKeyword(t *testing.T) {
	cases := []struct {
		name string
		raw  *string
		want *int32
	}{
		{"absent", nil, nil},
		{"empty", orderStrPtr(""), nil},
		{"bill prefix", orderStrPtr("BILL-42"), orderInt32Ptr(42)},
		{"bill lowercase", orderStrPtr("bill-42"), orderInt32Ptr(42)},
		{"bare digits", orderStrPtr("42"), orderInt32Ptr(42)},
		{"zero", orderStrPtr("BILL-0"), nil},
		{"leading zero", orderStrPtr("042"), nil},
		{"text", orderStrPtr("hello"), nil},
		{"bill no digits", orderStrPtr("BILL-"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ordersParseBillingOrderKeyword(tc.raw)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %d, want nil", *got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("got %v, want %d", got, *tc.want)
			}
		})
	}
}

// ==================== JSON shape ====================

// TestOrdersOperationCaseJSONShape pins the serialized case: refundAmount is a JSON
// number (or null), providerSummary is passed through verbatim, and the refund
// chain amounts are numbers rather than decimal strings.
func TestOrdersOperationCaseJSONShape(t *testing.T) {
	amount := 12.34
	view := orderOperationCaseView{
		ID:              1,
		SourceType:      "recharge",
		SourceID:        5,
		Status:          "pending_review",
		Reason:          "review",
		RefundAmount:    &amount,
		ProviderSummary: map[string]any{"rawStatus": "completed"},
		BalanceAdjustmentRequest: &orderBalanceAdjustmentView{
			ID:     3,
			Amount: 12.34,
			BalanceLog: &orderBalanceLogView{
				ID:            4,
				Amount:        12.34,
				BalanceBefore: 100,
				BalanceAfter:  112.34,
			},
		},
		CreatedAt: "2026-01-01T00:00:00.000Z",
		UpdatedAt: "2026-01-01T00:00:00.000Z",
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got, ok := decoded["refundAmount"].(float64); !ok || got != 12.34 {
		t.Fatalf("refundAmount = %#v (must be a JSON number)", decoded["refundAmount"])
	}
	summary, ok := decoded["providerSummary"].(map[string]any)
	if !ok || summary["rawStatus"] != "completed" {
		t.Fatalf("providerSummary = %#v", decoded["providerSummary"])
	}
	adjustment := decoded["balanceAdjustmentRequest"].(map[string]any)
	if got, ok := adjustment["amount"].(float64); !ok || got != 12.34 {
		t.Fatalf("adjustment amount = %#v", adjustment["amount"])
	}
	log := adjustment["balanceLog"].(map[string]any)
	if got, ok := log["balanceAfter"].(float64); !ok || got != 112.34 {
		t.Fatalf("balanceAfter = %#v", log["balanceAfter"])
	}
}

// TestOrdersListEnvelopeShape pins the list envelope keys (orders/total/page/
// pageSize — note it is NOT the {items,...} envelope the other modules use).
func TestOrdersListEnvelopeShape(t *testing.T) {
	encoded, err := json.Marshal(ordersListResult{
		Orders:   []orderWithOperationCase{},
		Total:    0,
		Page:     1,
		PageSize: 20,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"orders", "total", "page", "pageSize"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("missing key %q in %s", key, encoded)
		}
	}
	if _, ok := decoded["items"]; ok {
		t.Fatalf("orders must not use the items envelope")
	}
	if _, ok := decoded["totalPages"]; ok {
		t.Fatalf("orders must not emit totalPages")
	}
}

// ==================== helpers ====================

func orderStrPtr(value string) *string { return &value }

func orderInt32Ptr(value int32) *int32 { return &value }

func orderFloatPtr(value float64) *float64 { return &value }

// TestOrdersDerivedMoneyNeverUsesDecimalStrings guards the whole order view: a
// Decimal leaking into the payload would serialize as a quoted string and break
// every client doing arithmetic on the amount.
func TestOrdersDerivedMoneyNeverUsesDecimalStrings(t *testing.T) {
	row := ordersBillingRow{
		ID:          1,
		UserID:      2,
		Type:        "refund",
		Amount:      "10.00",
		Months:      1,
		PeriodStart: time.Now().UTC(),
		PeriodEnd:   time.Now().UTC(),
		CreatedAt:   time.Now().UTC(),
	}
	encoded, err := json.Marshal(ordersBuildBillingOrder(row, nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := decoded["amount"].(float64); !ok {
		t.Fatalf("amount must be a JSON number, got %#v", decoded["amount"])
	}
	if decoded["status"] != "refunded" {
		t.Fatalf("refund status = %#v", decoded["status"])
	}
}
