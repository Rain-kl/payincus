package routes

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== harness ====================

// promoCodesTestContext builds a Gin context with a JSON request body. The auth
// middleware is not mounted, so only the handler branches that run before the
// first database access can be exercised (the empty-code / invalid-param paths).
func promoCodesTestContext(t *testing.T, method, target, body string, params gin.Params) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	context.Request = request
	context.Params = params
	return context, recorder
}

func promoCodesDecodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response body %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

// ==================== handler-level contract ====================

// TestPromoCodesValidateEarlyRejections pins the 200-with-errorCode envelope of
// the three checks that run before the database is touched.
func TestPromoCodesValidateEarlyRejections(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantError string
		wantCode  string
	}{
		{"missing code", `{}`, "优惠码不能为空", "PROMO_NOT_FOUND"},
		{"empty code", `{"code":""}`, "优惠码不能为空", "PROMO_NOT_FOUND"},
		{"whitespace code", `{"code":"   \t"}`, "优惠码不能为空", "PROMO_NOT_FOUND"},
		{"null code", `{"code":null}`, "优惠码不能为空", "PROMO_NOT_FOUND"},
		{"falsy number code", `{"code":0}`, "优惠码不能为空", "PROMO_NOT_FOUND"},
		{"missing plan id", `{"code":"X"}`, "方案 ID 无效", "INVALID_PARAMS"},
		{"zero plan id", `{"code":"X","packagePlanId":0}`, "方案 ID 无效", "INVALID_PARAMS"},
		{"fractional plan id", `{"code":"X","packagePlanId":1.5}`, "方案 ID 无效", "INVALID_PARAMS"},
		{"non-numeric plan id", `{"code":"X","packagePlanId":"abc"}`, "方案 ID 无效", "INVALID_PARAMS"},
		{"plan id beyond safe integer", `{"code":"X","packagePlanId":9007199254740992}`, "方案 ID 无效", "INVALID_PARAMS"},
		{"non-object body", `"hello"`, "优惠码不能为空", "PROMO_NOT_FOUND"},
		{"array body", `[1,2]`, "优惠码不能为空", "PROMO_NOT_FOUND"},
		{"null body", `null`, "优惠码不能为空", "PROMO_NOT_FOUND"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			context, recorder := promoCodesTestContext(t, http.MethodPost, "/api/coupon/validate", tc.body, nil)
			promoCodesValidateHandler(context)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			body := promoCodesDecodeBody(t, recorder)
			if body["valid"] != false {
				t.Fatalf("valid = %#v, want false", body["valid"])
			}
			if body["error"] != tc.wantError {
				t.Fatalf("error = %#v, want %q", body["error"], tc.wantError)
			}
			if body["errorCode"] != tc.wantCode {
				t.Fatalf("errorCode = %#v, want %q", body["errorCode"], tc.wantCode)
			}
		})
	}
}

// TestPromoCodesValidateTruthyNonStringCode covers the TypeError path: a truthy
// non-string has no `.trim`, so Node's handler throws and app.ts answers 500.
func TestPromoCodesValidateTruthyNonStringCode(t *testing.T) {
	for _, body := range []string{`{"code":5}`, `{"code":true}`, `{"code":{}}`, `{"code":[]}`} {
		context, recorder := promoCodesTestContext(t, http.MethodPost, "/api/coupon/validate", body, nil)
		promoCodesValidateHandler(context)

		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("%s: status = %d, want 500", body, recorder.Code)
		}
		decoded := promoCodesDecodeBody(t, recorder)
		if decoded["error"] != "Internal server error" {
			t.Fatalf("%s: error = %#v", body, decoded["error"])
		}
	}
}

// TestPromoCodesApplyEarlyRejections pins the 400 {success,error,errorCode} shape
// of the /apply route and its id parsing order.
func TestPromoCodesApplyEarlyRejections(t *testing.T) {
	cases := []struct {
		name       string
		instanceID string
		body       string
		wantStatus int
		// wantKey is the envelope used by the branch: apiError carries `code`,
		// the hand-written {success:false} object carries `errorCode`.
		wantKey   string
		wantError string
		wantCode  string
	}{
		{"invalid id zero", "0", `{"code":"X"}`, http.StatusBadRequest, "code", "Invalid ID", "INVALID_ID"},
		{"invalid id leading zero", "01", `{"code":"X"}`, http.StatusBadRequest, "code", "Invalid ID", "INVALID_ID"},
		{"invalid id text", "abc", `{"code":"X"}`, http.StatusBadRequest, "code", "Invalid ID", "INVALID_ID"},
		{"missing code", "1", `{}`, http.StatusBadRequest, "errorCode", "优惠码不能为空", "PROMO_NOT_FOUND"},
		{"blank code", "1", `{"code":"  "}`, http.StatusBadRequest, "errorCode", "优惠码不能为空", "PROMO_NOT_FOUND"},
		{"non-object body", "1", `5`, http.StatusBadRequest, "errorCode", "优惠码不能为空", "PROMO_NOT_FOUND"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			context, recorder := promoCodesTestContext(t, http.MethodPost, "/api/coupon/apply/"+tc.instanceID, tc.body,
				gin.Params{{Key: "instanceId", Value: tc.instanceID}})
			promoCodesApplyHandler(context)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
			body := promoCodesDecodeBody(t, recorder)
			if body["error"] != tc.wantError {
				t.Fatalf("error = %#v, want %q", body["error"], tc.wantError)
			}
			if body[tc.wantKey] != tc.wantCode {
				t.Fatalf("%s = %#v, want %q", tc.wantKey, body[tc.wantKey], tc.wantCode)
			}
			if tc.wantKey == "errorCode" {
				if body["success"] != false {
					t.Fatalf("success = %#v, want false", body["success"])
				}
			}
		})
	}
}

// TestPromoCodesApplyInvalidIDRoute pins the apiError envelope details string.
func TestPromoCodesApplyInvalidIDRoute(t *testing.T) {
	context, recorder := promoCodesTestContext(t, http.MethodPost, "/api/coupon/apply/0", `{"code":"X"}`,
		gin.Params{{Key: "instanceId", Value: "0"}})
	promoCodesApplyHandler(context)

	body := promoCodesDecodeBody(t, recorder)
	if body["code"] != "INVALID_ID" || body["details"] != "实例 ID 无效" {
		t.Fatalf("body = %#v, want the apiError envelope for INVALID_ID", body)
	}
}

// ==================== registration ====================

// TestRegisterPromoCodesMountsBothPrefixes pins app.ts' double registration and
// the absence of a trailing-slash twin.
func TestRegisterPromoCodesMountsBothPrefixes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterPromoCodes(engine)

	registered := map[string]bool{}
	for _, route := range engine.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	want := []string{
		"POST /api/coupon/validate",
		"GET /api/coupon/renew-preview/:instanceId",
		"POST /api/coupon/apply/:instanceId",
		"POST /api/promos/validate",
		"GET /api/promos/renew-preview/:instanceId",
		"POST /api/promos/apply/:instanceId",
	}
	for _, key := range want {
		if !registered[key] {
			t.Errorf("missing route %s", key)
		}
	}
	if len(registered) != len(want) {
		t.Errorf("registered %d routes, want %d: %v", len(registered), len(want), registered)
	}
	if registered["POST /api/coupon/validate/"] {
		t.Error("trailing-slash twin must not be registered")
	}
}

// ==================== pure helpers ====================

func TestPromoCodesJSNumber(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  float64
		nan   bool
	}{
		{"nil", nil, 0, false},
		{"number", float64(42), 42, false},
		{"true", true, 1, false},
		{"false", false, 0, false},
		{"numeric string", "42", 42, false},
		{"numeric string with spaces", " 42 ", 42, false},
		{"empty string", "", 0, false},
		{"decimal string", "1.5", 1.5, false},
		{"exponent string", "1e3", 1000, false},
		{"hex string", "0x10", 16, false},
		{"invalid string", "abc", 0, true},
		{"empty array", []any{}, 0, false},
		{"single number array", []any{float64(3)}, 3, false},
		{"single string array", []any{"3"}, 3, false},
		{"single bool array", []any{true}, 0, true},
		{"multi element array", []any{float64(1), float64(2)}, 0, true},
		{"object", map[string]any{}, 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := promoCodesJSNumber(tc.value)
			if tc.nan {
				if !math.IsNaN(got) {
					t.Fatalf("got %v, want NaN", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPromoCodesIsSafeInteger(t *testing.T) {
	if !promoCodesIsSafeInteger(1) || !promoCodesIsSafeInteger(9007199254740991) {
		t.Fatal("safe integers rejected")
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 1.5, 9007199254740992, -9007199254740992} {
		if promoCodesIsSafeInteger(value) {
			t.Fatalf("%v accepted as a safe integer", value)
		}
	}
}

func TestPromoCodesJSFieldTrim(t *testing.T) {
	cases := []struct {
		name   string
		body   map[string]any
		want   string
		wantOK bool
	}{
		{"absent", map[string]any{}, "", true},
		{"null", map[string]any{"code": nil}, "", true},
		{"string", map[string]any{"code": "  AbC "}, "AbC", true},
		{"empty string", map[string]any{"code": ""}, "", true},
		{"zero", map[string]any{"code": float64(0)}, "", true},
		{"false", map[string]any{"code": false}, "", true},
		{"nonzero number throws", map[string]any{"code": float64(5)}, "", false},
		{"true throws", map[string]any{"code": true}, "", false},
		{"object throws", map[string]any{"code": map[string]any{}}, "", false},
		{"array throws", map[string]any{"code": []any{}}, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := promoCodesJSFieldTrim(tc.body, "code")
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("= (%q,%v), want (%q,%v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestPromoCodesNarrowID(t *testing.T) {
	if id, ok := promoCodesNarrowID(7); !ok || id != 7 {
		t.Fatalf("narrow(7) = (%d,%v)", id, ok)
	}
	if _, ok := promoCodesNarrowID(float64(math.MaxInt32) + 1); ok {
		t.Fatal("id above int32 accepted")
	}
	if _, ok := promoCodesNarrowID(float64(math.MinInt32) - 1); ok {
		t.Fatal("id below int32 accepted")
	}
}

func TestPromoCodesQueryValue(t *testing.T) {
	cases := []struct {
		target    string
		want      string
		present   bool
		wantArray bool
	}{
		{"/x?months=3", "3", true, false},
		{"/x?months=", "", true, false},
		{"/x?months", "", true, false},
		{"/x?months=1&months=2", "1", true, true},
		{"/x", "", false, false},
	}
	for _, tc := range cases {
		context, _ := promoCodesTestContext(t, http.MethodGet, tc.target, "", nil)
		got, present, isArray := promoCodesQueryValue(context, "months")
		if got != tc.want || present != tc.present || isArray != tc.wantArray {
			t.Fatalf("%s = (%q,%v,%v), want (%q,%v,%v)",
				tc.target, got, present, isArray, tc.want, tc.present, tc.wantArray)
		}
	}
}

func promoCodesTestDecimal(t *testing.T, value string) models.Decimal {
	t.Helper()
	parsed, err := models.NewDecimalFromString(value)
	if err != nil {
		t.Fatalf("decimal %q: %v", value, err)
	}
	return parsed
}

func promoCodesTestBinding(t *testing.T, remaining *int32, enabled bool, expiresAt *time.Time, discount string) *db.PromoInstanceBindingWithCode {
	t.Helper()
	return &db.PromoInstanceBindingWithCode{
		ID:              1,
		InstanceID:      9,
		PromoCodeID:     3,
		DurationType:    models.PromoDurationTypeREPEATING,
		RemainingCycles: remaining,
		PromoCode: db.PromoCodeWithDetails{
			PromoCode: models.PromoCode{
				Code:          "SAVE10",
				Enabled:       enabled,
				ExpiresAt:     expiresAt,
				DiscountValue: promoCodesTestDecimal(t, discount),
			},
		},
	}
}

func TestPromoCodesBindingStatus(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

	if got := promoCodesBindingStatus(nil, now); got != nil {
		t.Fatalf("no binding = %v, want null", *got)
	}

	active := promoCodesTestBinding(t, nil, true, nil, "0.1")
	if got := promoCodesBindingStatus(active, now); got == nil || *got != "ACTIVE" {
		t.Fatalf("status = %v, want ACTIVE", got)
	}

	disabled := promoCodesTestBinding(t, nil, false, nil, "0.1")
	if got := promoCodesBindingStatus(disabled, now); got == nil || *got != "DISABLED" {
		t.Fatalf("status = %v, want DISABLED", got)
	}

	past := now.Add(-time.Minute)
	expired := promoCodesTestBinding(t, nil, true, &past, "0.1")
	if got := promoCodesBindingStatus(expired, now); got == nil || *got != "EXPIRED" {
		t.Fatalf("status = %v, want EXPIRED", got)
	}

	// `expiresAt <= now` counts as expired in this route.
	exactly := promoCodesTestBinding(t, nil, true, &now, "0.1")
	if got := promoCodesBindingStatus(exactly, now); got == nil || *got != "EXPIRED" {
		t.Fatalf("status = %v, want EXPIRED at exactly now", got)
	}

	future := now.Add(time.Minute)
	valid := promoCodesTestBinding(t, nil, true, &future, "0.1")
	if got := promoCodesBindingStatus(valid, now); got == nil || *got != "ACTIVE" {
		t.Fatalf("status = %v, want ACTIVE", got)
	}
}

func TestPromoCodesIsPromoActive(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Minute)
	past := now.Add(-time.Minute)

	cases := []struct {
		name    string
		binding *db.PromoInstanceBindingWithCode
		want    bool
	}{
		{"no binding", nil, false},
		{"enabled", promoCodesTestBinding(t, nil, true, nil, "0.1"), true},
		{"disabled", promoCodesTestBinding(t, nil, false, nil, "0.1"), false},
		{"not expired", promoCodesTestBinding(t, nil, true, &future, "0.1"), true},
		{"expired", promoCodesTestBinding(t, nil, true, &past, "0.1"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := promoCodesIsPromoActive(tc.binding, now); got != tc.want {
				t.Fatalf("isPromoActive = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPromoCodesRenewOptionsWithoutPromo(t *testing.T) {
	options := promoCodesRenewOptions(10, []int{1, 3}, false, nil)
	if len(options) != 2 {
		t.Fatalf("options = %d, want 2", len(options))
	}
	for index, wantMonths := range []int{1, 3} {
		option := options[index]
		if option.Months != wantMonths {
			t.Fatalf("option %d months = %d", index, option.Months)
		}
		wantOriginal := 10.0 * float64(wantMonths)
		if option.OriginalPrice != wantOriginal || option.FinalPrice != wantOriginal {
			t.Fatalf("option %d prices = %+v", index, option)
		}
		if option.DiscountAmount != 0 || option.DiscountedMonths != 0 || option.RegularMonths != wantMonths || option.WillUnbind {
			t.Fatalf("option %d = %+v", index, option)
		}
	}
}

func TestPromoCodesRenewOptionsWithActivePromo(t *testing.T) {
	binding := promoCodesTestBinding(t, promoCodesInt32Ptr(1), true, nil, "0.1")

	options := promoCodesRenewOptions(10, []int{3}, true, binding)
	if len(options) != 1 {
		t.Fatalf("options = %d", len(options))
	}
	option := options[0]
	// REPEATING with one remaining cycle: 1 discounted month, 2 regular.
	if option.DiscountedMonths != 1 || option.RegularMonths != 2 {
		t.Fatalf("breakdown = %+v", option)
	}
	if option.OriginalPrice != 30 || option.DiscountAmount != 1 || option.FinalPrice != 29 {
		t.Fatalf("prices = %+v", option)
	}
	if !option.WillUnbind {
		t.Fatalf("willUnbind = false, want true")
	}
}

func TestPromoCodesRenewBindingView(t *testing.T) {
	expires := time.Date(2026, 1, 2, 3, 4, 5, 678_000_000, time.UTC)
	binding := promoCodesTestBinding(t, promoCodesInt32Ptr(2), true, &expires, "0.25")

	view := promoCodesRenewBindingView(binding)
	if view == nil {
		t.Fatal("view = nil")
	}
	if view.Code != "SAVE10" || view.DiscountValue != 0.25 || !view.Enabled {
		t.Fatalf("view = %+v", view)
	}
	if view.RemainingCycles == nil || *view.RemainingCycles != 2 {
		t.Fatalf("remainingCycles = %v", view.RemainingCycles)
	}
	if view.ExpiresAt == nil || *view.ExpiresAt != "2026-01-02T03:04:05.678Z" {
		t.Fatalf("expiresAt = %v, want the Prisma ISO form", view.ExpiresAt)
	}

	if promoCodesRenewBindingView(nil) != nil {
		t.Fatal("nil binding must render as null")
	}

	noExpiry := promoCodesTestBinding(t, nil, true, nil, "0.1")
	if view := promoCodesRenewBindingView(noExpiry); view.ExpiresAt != nil {
		t.Fatalf("expiresAt = %v, want null", *view.ExpiresAt)
	}
}

func TestPromoCalculateMonthlyPrice(t *testing.T) {
	price := promoCodesTestDecimal(t, "50")

	cases := []struct {
		name         string
		billingPrice *models.Decimal
		billingCycle *int32
		want         float64
	}{
		{"no price", nil, promoCodesInt32Ptr(1), 0},
		{"monthly", &price, promoCodesInt32Ptr(1), 50},
		{"yearly", &price, promoCodesInt32Ptr(12), 50.0 / 12},
		{"no cycle", &price, nil, 50},
		{"zero cycle", &price, promoCodesInt32Ptr(0), 50},
		{"negative cycle", &price, promoCodesInt32Ptr(-3), 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			instance := &models.Instance{BillingPrice: tc.billingPrice, BillingCycle: tc.billingCycle}
			if got := promoCalculateMonthlyPrice(instance); got != tc.want {
				t.Fatalf("monthlyPrice = %v, want %v", got, tc.want)
			}
		})
	}
}

func promoCodesInt32Ptr(value int32) *int32 { return &value }
