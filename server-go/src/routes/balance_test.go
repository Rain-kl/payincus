// Unit tests for the balance route port. They pin the query/pagination clamping,
// the enum allowlists, the admin body normalizers (with their exact Chinese
// messages) and — most importantly — the numeric JSON shape of every money field.
package routes

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// TestBalanceParsePageAndSize pins parsePositiveIntegerQuery(page, 1) and
// parseClampedPositiveIntegerQuery(pageSize, 20, 100).
func TestBalanceParsePageAndSize(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"", 1},
		{"0", 1},
		{"-1", 1},
		{"01", 1},
		{"1.5", 1},
		{"abc", 1},
		{"2", 2},
		{"999999", 999999},
		{"9007199254740993", 1}, // > MAX_SAFE_INTEGER
	} {
		if got := balanceParsePage(tc.value); got != tc.want {
			t.Errorf("balanceParsePage(%q) = %d, want %d", tc.value, got, tc.want)
		}
	}

	for _, tc := range []struct {
		value string
		want  int
	}{
		{"", 20},
		{"abc", 20},
		{"0", 20},
		{"1", 1},
		{"100", 100},
		{"1000", 100},
	} {
		if got := balanceParsePageSize(tc.value); got != tc.want {
			t.Errorf("balanceParsePageSize(%q) = %d, want %d", tc.value, got, tc.want)
		}
	}

	// A JS-safe page combined with pageSize<=100 always fits a 64-bit offset, so the
	// clamp must never perturb real requests; page/pageSize are still echoed as parsed.
	if got := balanceQueryPage(9_000_000_000, 100); got != 9_000_000_000 {
		t.Fatalf("balanceQueryPage must not clamp real pages, got %d", got)
	}
	if got := balanceQueryPage(9007199254740991, 100); got != 9007199254740991 {
		t.Fatalf("balanceQueryPage(MAX_SAFE_INTEGER) = %d", got)
	}
	if got := balanceQueryPage(7, 20); got != 7 {
		t.Fatalf("balanceQueryPage(7) = %d", got)
	}
	if got := balanceQueryPage(1, 0); got != 1 {
		t.Fatalf("balanceQueryPage with pageSize 0 = %d", got)
	}
}

// TestBalanceEnumAllowlists pins the three query allowlists. Anything outside the
// list must normalize to the zero value ("no filter"), never to the raw input.
func TestBalanceEnumAllowlists(t *testing.T) {
	logTypes := []struct {
		value string
		want  models.BalanceLogType
	}{
		{"recharge", models.BalanceLogTypeRecharge},
		{"consume", models.BalanceLogTypeConsume},
		{"refund", models.BalanceLogTypeRefund},
		{"admin_adjust", models.BalanceLogTypeAdminAdjust},
		{"gift", models.BalanceLogTypeGift},
		{"transfer_fee", models.BalanceLogTypeTransferFee},
		{"transfer_refund", models.BalanceLogTypeTransferRefund},
		{"invite_generate", models.BalanceLogTypeInviteGenerate},
		// Deliberately excluded from the TypeScript allowlist.
		{"exchange_purchase", ""},
		{"exchange_refund", ""},
		{"exchange_transfer", ""},
		{"gift_card_issue", ""},
		{"", ""},
		{"RECHARGE", ""},
		{"bogus", ""},
	}
	for _, tc := range logTypes {
		if got := balanceNormalizeLogType(tc.value); got != tc.want {
			t.Errorf("balanceNormalizeLogType(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}

	for _, tc := range []struct {
		value string
		want  models.BillingRecordType
	}{
		{"newPurchase", models.BillingRecordTypeNewPurchase},
		{"renew", models.BillingRecordTypeRenew},
		{"upgrade", models.BillingRecordTypeUpgrade},
		{"downgrade", models.BillingRecordTypeDowngrade},
		{"refund", models.BillingRecordTypeRefund},
		{"transfer_fee", models.BillingRecordTypeTransferFee},
		{"", ""},
		{"newpurchase", ""},
		{"bogus", ""},
	} {
		if got := balanceNormalizeBillingType(tc.value); got != tc.want {
			t.Errorf("balanceNormalizeBillingType(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}

	for _, tc := range []struct {
		value string
		want  models.BalanceAdjustmentRequestStatus
	}{
		{"pending", models.BalanceAdjustmentRequestStatusPending},
		{"approved", models.BalanceAdjustmentRequestStatusApproved},
		{"rejected", models.BalanceAdjustmentRequestStatusRejected},
		{"", ""},
		{"PENDING", ""},
		{"bogus", ""},
	} {
		if got := balanceNormalizeAdjustmentStatus(tc.value); got != tc.want {
			t.Errorf("balanceNormalizeAdjustmentStatus(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}

	// lotteryGift only accepts the two literals.
	for _, tc := range []struct{ value, want string }{
		{"exclude", "exclude"},
		{"only", "only"},
		{"", ""},
		{"EXCLUDE", ""},
		{"bogus", ""},
	} {
		if got := balanceLotteryGift(tc.value); got != tc.want {
			t.Errorf("balanceLotteryGift(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

// TestBalanceNormalizeAdminAdjustInput pins normalizeAdminBalanceAdjustInput's
// messages and its remark rules.
func TestBalanceNormalizeAdminAdjustInput(t *testing.T) {
	cases := []struct {
		name string
		body any
		want string
	}{
		{"nil body", nil, "请求参数无效"},
		{"array body", []any{float64(1)}, "请求参数无效"},
		{"string body", "x", "请求参数无效"},
		{"missing amount", map[string]any{"remark": "r"}, "调整金额必须是有效的数字"},
		{"null amount", map[string]any{"amount": nil, "remark": "r"}, "调整金额必须是有效的数字"},
		{"string amount", map[string]any{"amount": "10", "remark": "r"}, "调整金额必须是有效的数字"},
		{"bool amount", map[string]any{"amount": true, "remark": "r"}, "调整金额必须是有效的数字"},
		{"zero amount", map[string]any{"amount": float64(0), "remark": "r"}, "调整金额不能为 0"},
		{"missing remark", map[string]any{"amount": float64(5)}, "必须填写调整原因"},
		{"null remark", map[string]any{"amount": float64(5), "remark": nil}, "必须填写调整原因"},
		{"blank remark", map[string]any{"amount": float64(5), "remark": "   "}, "必须填写调整原因"},
		{"non-string remark", map[string]any{"amount": float64(5), "remark": float64(1)}, "调整原因必须是字符串"},
		{"long remark", map[string]any{"amount": float64(5), "remark": strings.Repeat("x", 501)}, "调整原因不能超过 500 字符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, inputError := balanceNormalizeAdminAdjustInput(tc.body)
			if inputError == nil || inputError.Message != tc.want {
				t.Fatalf("error = %#v, want %q", inputError, tc.want)
			}
		})
	}

	// A negative amount is allowed (deduction) and the remark is trimmed.
	input, inputError := balanceNormalizeAdminAdjustInput(map[string]any{
		"amount": float64(-12.5), "remark": "  调整  ",
	})
	if inputError != nil {
		t.Fatalf("unexpected error %#v", inputError)
	}
	if input.Amount != -12.5 || input.Remark != "调整" {
		t.Fatalf("input = %#v", input)
	}

	// Exactly 500 characters is accepted (UTF-16 units).
	if _, inputError := balanceNormalizeAdminAdjustInput(map[string]any{
		"amount": float64(1), "remark": strings.Repeat("x", 500),
	}); inputError != nil {
		t.Fatalf("500 chars must be accepted: %#v", inputError)
	}
}

// TestBalanceNormalizeAdjustmentRequestInput pins
// normalizeAdminBalanceAdjustmentRequestInput, including the two-decimal regex and
// the refund sign rule.
func TestBalanceNormalizeAdjustmentRequestInput(t *testing.T) {
	valid := map[string]any{"amount": float64(12.34), "reason": "理由"}
	input, inputError := balanceNormalizeAdjustmentRequestInput(valid)
	if inputError != nil {
		t.Fatalf("unexpected error %#v", inputError)
	}
	if input.Amount != 12.34 || input.RequestType != models.BalanceAdjustmentRequestTypeManualAdjust {
		t.Fatalf("default requestType = %#v", input)
	}
	if input.Reason != "理由" || input.SourceType != nil || input.SourceID != nil || input.OrderNo != nil {
		t.Fatalf("optional fields = %#v", input)
	}

	cases := []struct {
		name string
		body any
		want string
	}{
		{"nil body", nil, "请求参数无效"},
		{"missing amount", map[string]any{"reason": "r"}, "调账金额必须是有效的数字"},
		{"string amount", map[string]any{"amount": "1", "reason": "r"}, "调账金额必须是有效的数字"},
		{"zero amount", map[string]any{"amount": float64(0), "reason": "r"}, "调账金额不能为 0"},
		{"three decimals", map[string]any{"amount": float64(1.234), "reason": "r"}, "调账金额最多保留两位小数"},
		{"missing reason", map[string]any{"amount": float64(1)}, "必须填写申请原因"},
		{"blank reason", map[string]any{"amount": float64(1), "reason": " "}, "必须填写申请原因"},
		{"refund with negative amount", map[string]any{"amount": float64(-1), "reason": "r", "requestType": "refund"}, "退款申请金额必须大于 0"},
		{"refund with zero-ish amount", map[string]any{"amount": float64(-0.5), "reason": "r", "requestType": "refund"}, "退款申请金额必须大于 0"},
		{"non-string sourceType", map[string]any{"amount": float64(1), "reason": "r", "sourceType": float64(1)}, "来源类型必须是字符串"},
		{"long sourceType", map[string]any{"amount": float64(1), "reason": "r", "sourceType": strings.Repeat("x", 65)}, "来源类型不能超过 64 字符"},
		{"zero sourceId", map[string]any{"amount": float64(1), "reason": "r", "sourceId": float64(0)}, "来源 ID必须是正整数"},
		{"float sourceId", map[string]any{"amount": float64(1), "reason": "r", "sourceId": float64(1.5)}, "来源 ID必须是正整数"},
		{"string sourceId", map[string]any{"amount": float64(1), "reason": "r", "sourceId": "5"}, "来源 ID必须是正整数"},
		{"non-string orderNo", map[string]any{"amount": float64(1), "reason": "r", "orderNo": float64(1)}, "订单号必须是字符串"},
		{"long orderNo", map[string]any{"amount": float64(1), "reason": "r", "orderNo": strings.Repeat("x", 129)}, "订单号不能超过 128 字符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, inputError := balanceNormalizeAdjustmentRequestInput(tc.body)
			if inputError == nil || inputError.Message != tc.want {
				t.Fatalf("error = %#v, want %q", inputError, tc.want)
			}
		})
	}

	// A refund with a positive amount is accepted, and an unknown requestType falls
	// back to manual_adjust (not rejected).
	input, inputError = balanceNormalizeAdjustmentRequestInput(map[string]any{
		"amount": float64(5), "reason": "r", "requestType": "refund",
	})
	if inputError != nil || input.RequestType != models.BalanceAdjustmentRequestTypeRefund {
		t.Fatalf("refund input = (%#v,%#v)", input, inputError)
	}
	input, inputError = balanceNormalizeAdjustmentRequestInput(map[string]any{
		"amount": float64(5), "reason": "r", "requestType": "bogus",
	})
	if inputError != nil || input.RequestType != models.BalanceAdjustmentRequestTypeManualAdjust {
		t.Fatalf("unknown requestType = (%#v,%#v)", input, inputError)
	}

	// Optional text is trimmed; a blank value becomes nil rather than "".
	input, inputError = balanceNormalizeAdjustmentRequestInput(map[string]any{
		"amount": float64(5), "reason": "r", "sourceType": "  recharge  ", "sourceId": float64(9),
		"orderNo": "  ORD-1  ", "requestType": "refund",
	})
	if inputError != nil {
		t.Fatalf("unexpected error %#v", inputError)
	}
	if input.SourceType == nil || *input.SourceType != "recharge" {
		t.Fatalf("sourceType = %v", input.SourceType)
	}
	if input.SourceID == nil || *input.SourceID != 9 {
		t.Fatalf("sourceId = %v", input.SourceID)
	}
	if input.OrderNo == nil || *input.OrderNo != "ORD-1" {
		t.Fatalf("orderNo = %v", input.OrderNo)
	}

	input, inputError = balanceNormalizeAdjustmentRequestInput(map[string]any{
		"amount": float64(5), "reason": "r", "sourceType": "   ",
	})
	if inputError != nil || input.SourceType != nil {
		t.Fatalf("blank sourceType = (%#v,%#v)", input, inputError)
	}
}

// TestBalanceNormalizeReviewInput pins the approve (optional) / reject (required)
// review-remark rules.
func TestBalanceNormalizeReviewInput(t *testing.T) {
	// approve: absent body is fine.
	if input, inputError := balanceNormalizeReviewInput(nil, false, false); inputError != nil || input.Remark != nil {
		t.Fatalf("absent optional remark = (%#v,%#v)", input, inputError)
	}
	// reject: absent body is an error.
	if _, inputError := balanceNormalizeReviewInput(nil, false, true); inputError == nil || inputError.Message != "必须填写审核备注" {
		t.Fatalf("absent required remark = %#v", inputError)
	}
	// Non-object bodies are rejected in both modes.
	if _, inputError := balanceNormalizeReviewInput([]any{}, true, false); inputError == nil || inputError.Message != "请求参数无效" {
		t.Fatalf("array body = %#v", inputError)
	}
	// A blank remark is nil for approve and an error for reject.
	if input, inputError := balanceNormalizeReviewInput(map[string]any{"remark": "  "}, true, false); inputError != nil || input.Remark != nil {
		t.Fatalf("blank optional remark = (%#v,%#v)", input, inputError)
	}
	if _, inputError := balanceNormalizeReviewInput(map[string]any{"remark": "  "}, true, true); inputError == nil || inputError.Message != "必须填写审核备注" {
		t.Fatalf("blank required remark = %#v", inputError)
	}
	// Non-string and over-long remarks.
	if _, inputError := balanceNormalizeReviewInput(map[string]any{"remark": float64(1)}, true, false); inputError == nil || inputError.Message != "审核备注必须是字符串" {
		t.Fatalf("non-string remark = %#v", inputError)
	}
	if _, inputError := balanceNormalizeReviewInput(map[string]any{"remark": strings.Repeat("x", 501)}, true, false); inputError == nil || inputError.Message != "审核备注不能超过 500 字符" {
		t.Fatalf("long remark = %#v", inputError)
	}
	if input, inputError := balanceNormalizeReviewInput(map[string]any{"remark": " ok "}, true, false); inputError != nil || input.Remark == nil || *input.Remark != "ok" {
		t.Fatalf("trimmed remark = (%#v,%#v)", input, inputError)
	}
}

// TestBalanceNormalizeGiftInput pins normalizeAdminBalanceGiftInput: the amount
// must be strictly positive and the remark is optional.
func TestBalanceNormalizeGiftInput(t *testing.T) {
	cases := []struct {
		name string
		body any
		want string
	}{
		{"nil body", nil, "请求参数无效"},
		{"missing amount", map[string]any{}, "赠送金额必须是有效的数字"},
		{"string amount", map[string]any{"amount": "5"}, "赠送金额必须是有效的数字"},
		{"zero amount", map[string]any{"amount": float64(0)}, "赠送金额必须大于 0"},
		{"negative amount", map[string]any{"amount": float64(-1)}, "赠送金额必须大于 0"},
		{"non-string remark", map[string]any{"amount": float64(1), "remark": float64(2)}, "赠送原因必须是字符串"},
		{"long remark", map[string]any{"amount": float64(1), "remark": strings.Repeat("x", 501)}, "赠送原因不能超过 500 字符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, inputError := balanceNormalizeGiftInput(tc.body)
			if inputError == nil || inputError.Message != tc.want {
				t.Fatalf("error = %#v, want %q", inputError, tc.want)
			}
		})
	}

	if input, inputError := balanceNormalizeGiftInput(map[string]any{"amount": float64(3)}); inputError != nil || input.Amount != 3 || input.Remark != nil {
		t.Fatalf("gift without remark = (%#v,%#v)", input, inputError)
	}
	if input, inputError := balanceNormalizeGiftInput(map[string]any{"amount": float64(3), "remark": " 红包 "}); inputError != nil || input.Remark == nil || *input.Remark != "红包" {
		t.Fatalf("gift with remark = (%#v,%#v)", input, inputError)
	}
}

// TestBalanceMoneyShapeIsNumeric is the regression test for the module's biggest
// wire-format trap: TypeScript returns `Promise<number>` for every money value, so
// the JSON must carry a number, never a Prisma-style decimal string.
func TestBalanceMoneyShapeIsNumeric(t *testing.T) {
	amount, _ := decimal.NewFromString("12.50")

	serialized := balanceSerializeBalanceLog(models.BalanceLog{
		ID:            1,
		Type:          models.BalanceLogTypeGift,
		Amount:        models.Decimal{Decimal: amount},
		BalanceBefore: models.Decimal{Decimal: decimal.Zero},
		BalanceAfter:  models.Decimal{Decimal: amount},
	})

	for _, key := range []string{"amount", "balanceBefore", "balanceAfter"} {
		raw, present := serialized[key]
		if !present {
			t.Fatalf("%s missing", key)
		}
		if _, isString := raw.(string); isString {
			t.Fatalf("%s must be a JSON number, got %#v", key, raw)
		}
		if _, isFloat := raw.(float64); !isFloat {
			t.Fatalf("%s must be float64, got %T", key, raw)
		}
	}
	if serialized["amount"] != 12.5 {
		t.Fatalf("amount = %v", serialized["amount"])
	}

	// The same holds end-to-end through encoding/json.
	encoded, err := json.Marshal(serialized)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"amount":12.5`) {
		t.Fatalf("encoded balance log = %s", encoded)
	}

	// Adjustment requests likewise emit numeric amounts, including the nested log.
	reviewedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	request := db.BalanceAdjustmentRequestWithRelations{
		BalanceAdjustmentRequest: models.BalanceAdjustmentRequest{
			ID:          7,
			UserID:      3,
			Amount:      models.Decimal{Decimal: amount},
			RequestType: models.BalanceAdjustmentRequestTypeRefund,
			Status:      models.BalanceAdjustmentRequestStatusApproved,
			Reason:      "r",
			CreatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			UpdatedAt:   time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
			ReviewedAt:  &reviewedAt,
		},
		User:        &db.BalanceAdjustmentUserRef{ID: 3, Username: "u"},
		RequestedBy: &db.BalanceAdjustmentUserRef{ID: 9, Username: "admin"},
	}
	view := balanceSerializeAdjustmentRequest(request)
	if view["amount"] != 12.5 {
		t.Fatalf("request amount = %#v", view["amount"])
	}
	reviewedAtText, isString := view["reviewedAt"].(*string)
	if !isString || reviewedAtText == nil || *reviewedAtText != "2026-01-02T03:04:05.000Z" {
		t.Fatalf("reviewedAt = %#v", view["reviewedAt"])
	}
	if view["createdAt"] != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("createdAt = %#v", view["createdAt"])
	}
	// reviewedBy / balanceLog are null (not missing) when the relations are absent.
	if view["reviewedBy"] != nil || view["balanceLog"] != nil {
		t.Fatalf("absent relations = (%#v, %#v)", view["reviewedBy"], view["balanceLog"])
	}
	// The nested user carries email; requestedBy does not (per the TS select).
	user, ok := view["user"].(gin.H)
	if !ok {
		t.Fatalf("user = %#v", view["user"])
	}
	if _, hasEmail := user["email"]; !hasEmail {
		t.Fatalf("user projection must carry email: %#v", user)
	}
	requestedBy, ok := view["requestedBy"].(gin.H)
	if !ok {
		t.Fatalf("requestedBy = %#v", view["requestedBy"])
	}
	if _, hasEmail := requestedBy["email"]; hasEmail {
		t.Fatalf("requestedBy must not carry email: %#v", requestedBy)
	}
}

// TestBalanceConsumeStatsShape pins UserConsumeStats' JSON keys (the admin route
// spreads them while /me nests them).
func TestBalanceConsumeStatsShape(t *testing.T) {
	encoded, err := json.Marshal(db.UserConsumeStats{
		TotalRecharge:       100.5,
		TotalConsume:        40,
		TotalRefund:         10.25,
		TotalDestroyedValue: 5,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"totalRecharge":100.5`,
		`"totalConsume":40`,
		`"totalRefund":10.25`,
		`"totalDestroyedValue":5`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("stats JSON %s is missing %s", encoded, want)
		}
	}
}

// TestBalanceRouteIDParsing pins the admin path-ID parser used by every /admin
// route.
func TestBalanceRouteIDParsing(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int32
		ok    bool
	}{
		{"1", 1, true},
		{"2147483647", 2147483647, true},
		{"0", 0, false},
		{"01", 0, false},
		{"-1", 0, false},
		{"1.5", 0, false},
		{"abc", 0, false},
		{"2147483648", 0, false},
		{"9007199254740993", 0, false},
	} {
		got, ok := balanceParseRouteID(tc.value)
		if got != tc.want || ok != tc.ok {
			t.Errorf("balanceParseRouteID(%q) = (%d,%v), want (%d,%v)", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

// TestBalanceIsPlainRecord pins isPlainRecord: null and arrays are not records.
func TestBalanceIsPlainRecord(t *testing.T) {
	if _, ok := balanceIsPlainRecord(nil); ok {
		t.Fatal("null is not a plain record")
	}
	if _, ok := balanceIsPlainRecord([]any{}); ok {
		t.Fatal("an array is not a plain record")
	}
	if _, ok := balanceIsPlainRecord("x"); ok {
		t.Fatal("a string is not a plain record")
	}
	if _, ok := balanceIsPlainRecord(map[string]any{}); !ok {
		t.Fatal("an object is a plain record")
	}
}

// TestBalanceReadFiniteNumber pins the `Number.isFinite` guard: Infinity and NaN
// are rejected, while 0 and negatives pass through (0 is rejected later by the
// per-route rule).
func TestBalanceReadFiniteNumber(t *testing.T) {
	object := map[string]any{
		"missing":    nil,
		"null":       nil,
		"string":     "5",
		"bool":       true,
		"zero":       float64(0),
		"negative":   float64(-2.5),
		"positive":   float64(3),
		"infinity":   math.Inf(1),
		"nan":        math.NaN(),
		"emptyArray": []any{},
	}
	_ = object["missing"]

	cases := []struct {
		key   string
		want  float64
		valid bool
	}{
		{"null", 0, false},
		{"string", 0, false},
		{"bool", 0, false},
		{"zero", 0, true},
		{"negative", -2.5, true},
		{"positive", 3, true},
		{"infinity", 0, false},
		{"nan", 0, false},
		{"emptyArray", 0, false},
	}
	for _, tc := range cases {
		value, ok := balanceReadFiniteNumber(map[string]any{"amount": object[tc.key]})
		if ok != tc.valid || (tc.valid && value != tc.want) {
			t.Errorf("amount=%v -> (%v,%v), want (%v,%v)", object[tc.key], value, ok, tc.want, tc.valid)
		}
	}
	if _, ok := balanceReadFiniteNumber(map[string]any{}); ok {
		t.Fatal("a missing amount must be invalid")
	}
}

// TestBalanceLogAmount pins the audit-log number formatting: an explicit '+' for
// positive deltas, `String(number)` otherwise.
func TestBalanceLogAmount(t *testing.T) {
	// Computed through variables so the compiler does not constant-fold the
	// addition into the exact decimal 0.3.
	left, right := 0.1, 0.2
	floatSum := left + right

	cases := []struct {
		value float64
		want  string
	}{
		{12.5, "+12.5"},
		{-12.5, "-12.5"},
		{0, "0"},
		{100, "+100"},
		{floatSum, "+0.30000000000000004"},
	}
	for _, tc := range cases {
		if got := balanceLogAmount(tc.value); got != tc.want {
			t.Errorf("balanceLogAmount(%v) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

// TestBalanceDecimalRoundTrip pins balanceDecimalFromFloat: the JS number becomes
// a Decimal whose JSON form is Prisma-compatible (trailing zeros dropped).
func TestBalanceDecimalRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		value float64
		want  string
	}{
		{12.5, "12.5"},
		{100, "100"},
		{-0.25, "-0.25"},
		{0.1, "0.1"},
	} {
		encoded, err := json.Marshal(balanceDecimalFromFloat(tc.value))
		if err != nil {
			t.Fatalf("marshal %v: %v", tc.value, err)
		}
		if string(encoded) != `"`+tc.want+`"` {
			t.Errorf("balanceDecimalFromFloat(%v) JSON = %s, want %q", tc.value, encoded, tc.want)
		}
	}
}

// TestBalanceSerializeLogRowInstance pins the instanceName projection: the name
// when the instance exists, null when it was deleted in the meantime.
func TestBalanceSerializeLogRowInstance(t *testing.T) {
	instanceID := int32(42)
	base := db.BalanceLogWithInstance{
		BalanceLog: models.BalanceLog{
			ID:            1,
			Type:          models.BalanceLogTypeConsume,
			Amount:        models.Decimal{Decimal: decimal.RequireFromString("-5")},
			BalanceBefore: models.Decimal{Decimal: decimal.RequireFromString("100")},
			BalanceAfter:  models.Decimal{Decimal: decimal.RequireFromString("95")},
			InstanceID:    &instanceID,
			CreatedAt:     time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		},
	}

	row := balanceSerializeLogRow(base)
	if row["instanceName"] != nil {
		t.Fatalf("missing instance must serialize instanceName as null, got %#v", row["instanceName"])
	}
	if row["amount"] != -5.0 {
		t.Fatalf("amount = %#v", row["amount"])
	}
	if row["createdAt"] != "2026-03-04T05:06:07.000Z" {
		t.Fatalf("createdAt = %#v", row["createdAt"])
	}
	if id, ok := row["instanceId"].(*int32); !ok || id == nil || *id != 42 {
		t.Fatalf("instanceId = %#v", row["instanceId"])
	}

	base.Instance = &db.BalanceInstanceRef{ID: 42, Name: "vm-1"}
	if got := balanceSerializeLogRow(base)["instanceName"]; got != "vm-1" {
		t.Fatalf("instanceName = %#v", got)
	}

	// The two log endpoints emit the same rows under different keys; this checks
	// the shared projection is field-for-field what the TypeScript maps.
	wantKeys := []string{
		"id", "type", "amount", "balanceBefore", "balanceAfter", "orderId",
		"instanceId", "instanceName", "remark", "createdAt",
	}
	gotKeys := make([]string, 0, len(row))
	for key := range row {
		gotKeys = append(gotKeys, key)
	}
	if len(gotKeys) != len(wantKeys) {
		t.Fatalf("keys = %v, want %v", gotKeys, wantKeys)
	}
	if !reflect.DeepEqual(serializedKeys(row), wantKeys) {
		t.Fatalf("keys = %v, want %v", serializedKeys(row), wantKeys)
	}
}

func serializedKeys(row map[string]any) []string {
	// encoding/json sorts map keys, so this doubles as a stable comparison.
	encoded, _ := json.Marshal(row)
	var decoded map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &decoded)
	keys := make([]string, 0, len(row))
	for _, key := range []string{
		"id", "type", "amount", "balanceBefore", "balanceAfter", "orderId",
		"instanceId", "instanceName", "remark", "createdAt",
	} {
		if _, present := decoded[key]; !present {
			keys = append(keys, "MISSING:"+key)
			continue
		}
		keys = append(keys, key)
	}
	return keys
}
