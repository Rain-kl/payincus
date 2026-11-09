package routes

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// adminPromoCodesTestContext builds a Gin context whose request carries the given
// JSON body ("" means no body at all).
func adminPromoCodesTestContext(t *testing.T, method, target, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	context.Request = request
	return context, recorder
}

// adminPromoCodesTestJSON decodes the recorder body.
func adminPromoCodesTestJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

func TestAdminPromoCodesPaginationHelpers(t *testing.T) {
	positiveCases := []struct {
		value    float64
		fallback int64
		want     int64
	}{
		{math.NaN(), 1, 1},
		{0, 1, 1},
		{-3, 1, 1},
		{2.5, 1, 1}, // not a safe integer
		{3, 1, 3},   // valid
		{1e15, 1, 1e15},
	}
	for _, tc := range positiveCases {
		if got := adminPromoCodesParsePositiveInteger(tc.value, tc.fallback); got != tc.want {
			t.Errorf("adminPromoCodesParsePositiveInteger(%v) = %d, want %d", tc.value, got, tc.want)
		}
	}

	clampedCases := []struct {
		value    float64
		fallback int64
		max      int64
		want     int64
	}{
		{math.NaN(), 20, 100, 20},
		{0, 20, 100, 20},
		{-1, 20, 100, 20},
		{1, 20, 100, 1},
		{100, 20, 100, 100},
		{101, 20, 100, 100},
		{5000, 20, 100, 100},
		{3.5, 20, 100, 20},
	}
	for _, tc := range clampedCases {
		if got := adminPromoCodesParseClampedPositiveInteger(tc.value, tc.fallback, tc.max); got != tc.want {
			t.Errorf("adminPromoCodesParseClampedPositiveInteger(%v) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

func TestAdminPromoCodesParseJSDate(t *testing.T) {
	parsed, ok := adminPromoCodesParseJSDate("2026-01-02T03:04:05.678Z")
	if !ok {
		t.Fatalf("ISO timestamp was rejected")
	}
	if want := time.Date(2026, 1, 2, 3, 4, 5, 678_000_000, time.UTC); !parsed.Equal(want) {
		t.Errorf("parsed = %v, want %v", parsed, want)
	}

	parsed, ok = adminPromoCodesParseJSDate("2026-01-02")
	if !ok || !parsed.Equal(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date-only parse = %v/%v", parsed, ok)
	}

	parsed, ok = adminPromoCodesParseJSDate(float64(1_000))
	if !ok || !parsed.Equal(time.Unix(1, 0).UTC()) {
		t.Errorf("epoch millis parse = %v/%v", parsed, ok)
	}

	if _, ok := adminPromoCodesParseJSDate("not-a-date"); ok {
		t.Errorf("invalid string was accepted")
	}
	if _, ok := adminPromoCodesParseJSDate(map[string]any{}); ok {
		t.Errorf("object was accepted as a date")
	}
}

func TestAdminPromoCodesDecimalFromNumber(t *testing.T) {
	cases := map[float64]string{
		0.5:   "0.5",
		0.125: "0.125",
		1:     "1",
		0.1:   "0.1",
	}
	for input, want := range cases {
		parsed, err := adminPromoCodesDecimalFromNumber(input)
		if err != nil {
			t.Fatalf("decimal(%v): %v", input, err)
		}
		if parsed.String() != want {
			t.Errorf("decimal(%v).String() = %q, want %q", input, parsed.String(), want)
		}
	}
}

func TestAdminPromoCodesTrimmedOrNull(t *testing.T) {
	if value := adminPromoCodesTrimmedOrNull("  promo  "); value == nil || *value != "promo" {
		t.Errorf("trimmed string = %v", value)
	}
	if value := adminPromoCodesTrimmedOrNull("   "); value != nil {
		t.Errorf("whitespace-only string = %v, want nil", *value)
	}
	if value := adminPromoCodesTrimmedOrNull(nil); value != nil {
		t.Errorf("nil = %v, want nil", value)
	}
	if value := adminPromoCodesTrimmedOrNull(float64(3)); value != nil {
		t.Errorf("number = %v, want nil", value)
	}
}

func TestAdminPromoCodesFormatJSValue(t *testing.T) {
	cases := []struct {
		value   any
		present bool
		want    string
	}{
		{nil, false, "undefined"},
		{nil, true, "null"},
		{"x", true, "x"},
		{float64(5), true, "5"},
		{float64(5.5), true, "5.5"},
		{true, true, "true"},
		{map[string]any{}, true, "[object Object]"},
	}
	for _, tc := range cases {
		if got := adminPromoCodesFormatJSValue(tc.value, tc.present); got != tc.want {
			t.Errorf("adminPromoCodesFormatJSValue(%v, %v) = %q, want %q", tc.value, tc.present, got, tc.want)
		}
	}
}

func TestAdminPromoCodesItemViewShape(t *testing.T) {
	decimalValue, err := adminPromoCodesDecimalFromNumber(0.25)
	if err != nil {
		t.Fatalf("decimal: %v", err)
	}
	totalDiscount, err := adminPromoCodesDecimalFromNumber(12.5)
	if err != nil {
		t.Fatalf("decimal: %v", err)
	}
	created := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	expires := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	packageID := int32(7)
	name := "Launch"

	view := adminPromoCodesItemsView(models.PromoCode{
		ID:                  3,
		Code:                "LAUNCH",
		Name:                &name,
		Type:                models.PromoCodeTypeADMINPROMO,
		DiscountType:        models.PromoDiscountTypePERCENTAGE,
		DiscountValue:       decimalValue,
		DurationType:        models.PromoDurationTypeFOREVER,
		IsGlobal:            false,
		ExpiresAt:           &expires,
		Enabled:             true,
		CreatedAt:           created,
		TotalDiscountAmount: totalDiscount,
	}, []models.PromoCodeScope{{ID: 11, PromoCodeID: 3, PackageID: &packageID}}, nil)

	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, present := decoded["activeBindingsCount"]; present {
		t.Errorf("create response must omit activeBindingsCount: %s", encoded)
	}
	if decoded["discountValue"] != 0.25 {
		t.Errorf("discountValue = %v, want 0.25 (a JSON number)", decoded["discountValue"])
	}
	if decoded["totalDiscountAmount"] != 12.5 {
		t.Errorf("totalDiscountAmount = %v", decoded["totalDiscountAmount"])
	}
	if decoded["createdAt"] != "2026-05-06T07:08:09.000Z" {
		t.Errorf("createdAt = %v", decoded["createdAt"])
	}
	if decoded["expiresAt"] != "2026-06-01T00:00:00.000Z" {
		t.Errorf("expiresAt = %v", decoded["expiresAt"])
	}
	if decoded["startsAt"] != nil {
		t.Errorf("startsAt = %v, want null", decoded["startsAt"])
	}
	scopes, ok := decoded["scopes"].([]any)
	if !ok || len(scopes) != 1 {
		t.Fatalf("scopes = %v", decoded["scopes"])
	}
	scope := scopes[0].(map[string]any)
	if scope["packageId"] != float64(7) || scope["packagePlanId"] != nil {
		t.Errorf("scope = %v", scope)
	}
}

// ==================== handler validation (no database access) ====================

func TestAdminPromoCodesHandlersRejectInvalidIDs(t *testing.T) {
	cases := []struct {
		name    string
		handler gin.HandlerFunc
		param   string
		body    string
		method  string
		target  string
	}{
		{"update", adminPromoCodesUpdateHandler, "id", "", http.MethodPut, "/x"},
		{"toggle", adminPromoCodesToggleHandler, "id", `{"enabled":true}`, http.MethodPatch, "/x"},
		{"delete", adminPromoCodesDeleteHandler, "id", "", http.MethodDelete, "/x"},
		{"instances", adminPromoCodesInstancesHandler, "id", "", http.MethodGet, "/x"},
	}
	for _, tc := range cases {
		context, recorder := adminPromoCodesTestContext(t, tc.method, tc.target, tc.body)
		context.Params = gin.Params{{Key: tc.param, Value: "0"}}
		tc.handler(context)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, recorder.Code)
			continue
		}
		decoded := adminPromoCodesTestJSON(t, recorder)
		if decoded["code"] != "INVALID_ID" || decoded["error"] != "无效的优惠码 ID" {
			t.Errorf("%s: body = %v", tc.name, decoded)
		}
	}
}

func TestAdminPromoCodesUnbindRejectsInvalidRouteParams(t *testing.T) {
	context, recorder := adminPromoCodesTestContext(t, http.MethodPost, "/x", "")
	context.Params = gin.Params{{Key: "id", Value: "5"}, {Key: "instanceId", Value: "01"}}
	adminPromoCodesUnbindHandler(context)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	decoded := adminPromoCodesTestJSON(t, recorder)
	if decoded["code"] != "INVALID_ID" || decoded["error"] != "无效的路由参数 ID" {
		t.Errorf("body = %v", decoded)
	}
}

func TestAdminPromoCodesToggleRequiresBooleanEnabled(t *testing.T) {
	for _, body := range []string{`{"enabled":"true"}`, `{"enabled":1}`, `{}`} {
		context, recorder := adminPromoCodesTestContext(t, http.MethodPatch, "/x", body)
		context.Params = gin.Params{{Key: "id", Value: "3"}}
		adminPromoCodesToggleHandler(context)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, recorder.Code)
			continue
		}
		decoded := adminPromoCodesTestJSON(t, recorder)
		if decoded["code"] != "INVALID_PARAMS" || decoded["error"] != "enabled 字段必须为布尔值" {
			t.Errorf("body %s: payload = %v", body, decoded)
		}
	}
}

func TestAdminPromoCodesCreateValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing code", `{}`, "优惠码不能为空"},
		{"numeric code", `{"code":123}`, "优惠码不能为空"},
		{"too short", `{"code":"ab","discountValue":0.1,"isGlobal":true}`, "优惠码长度需在 3-32 字符之间"},
		{"discount zero", `{"code":"ABC","discountValue":0}`, "折扣数值必须大于 0"},
		{"discount as string", `{"code":"ABC","discountValue":"0.5"}`, "折扣数值必须大于 0"},
		{"percentage above one", `{"code":"ABC","discountValue":1.5}`, "百分比折扣数值必须在 0 到 1.0 之间"},
		{"missing scopes", `{"code":"ABC","discountValue":0.5}`, "非全局优惠码必须指定至少一个适用范围"},
		{"bad scope package", `{"code":"ABC","discountValue":0.5,"scopes":[{"packageId":0}]}`, "适用范围套餐 ID 无效"},
		{"bad scope plan", `{"code":"ABC","discountValue":0.5,"scopes":[{"packageId":1,"packagePlanId":-2}]}`, "适用范围套餐方案 ID 无效"},
		{"bad max total", `{"code":"ABC","discountValue":0.5,"isGlobal":true,"maxTotalUses":0}`, "总使用次数必须为正整数"},
		{"bad max per user", `{"code":"ABC","discountValue":0.5,"isGlobal":true,"maxUsesPerUser":1.5}`, "单用户最大使用次数必须为正整数"},
		{"bad start", `{"code":"ABC","discountValue":0.5,"isGlobal":true,"startsAt":"nope"}`, "开始时间格式无效"},
		{"bad expiry", `{"code":"ABC","discountValue":0.5,"isGlobal":true,"expiresAt":"nope"}`, "过期时间格式无效"},
		{"start after expiry", `{"code":"ABC","discountValue":0.5,"isGlobal":true,"startsAt":"2026-02-01","expiresAt":"2026-01-01"}`, "开始时间必须早于过期时间"},
		{"repeat missing cycles", `{"code":"ABC","discountValue":0.5,"isGlobal":true,"durationType":"REPEATING"}`, "重复周期数必须为大于等于 1 的整数"},
	}
	for _, tc := range cases {
		context, recorder := adminPromoCodesTestContext(t, http.MethodPost, "/x", tc.body)
		adminPromoCodesCreateHandler(context)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, recorder.Code, recorder.Body.String())
			continue
		}
		decoded := adminPromoCodesTestJSON(t, recorder)
		if decoded["error"] != tc.want || decoded["code"] != "INVALID_PARAMS" {
			t.Errorf("%s: payload = %v, want error %q", tc.name, decoded, tc.want)
		}
	}
}

// ==================== registration ====================

func TestAdminPromoCodesRegistersAllRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterAdminPromoCodes(router)

	expected := map[string]bool{}
	for _, prefix := range []string{"/api/admin/coupon", "/api/admin/promos"} {
		// The module root is declared as '/' and Fastify's prefixTrailingSlash
		// default is 'both', so both spellings exist (measured on live Node).
		expected["GET "+prefix] = true
		expected["GET "+prefix+"/"] = true
		expected["POST "+prefix] = true
		expected["POST "+prefix+"/"] = true
		expected["PUT "+prefix+"/:id"] = true
		expected["PATCH "+prefix+"/:id/toggle"] = true
		expected["DELETE "+prefix+"/:id"] = true
		expected["GET "+prefix+"/:id/instances"] = true
		expected["POST "+prefix+"/:id/unbind/:instanceId"] = true
	}

	seen := map[string]bool{}
	for _, route := range router.Routes() {
		key := route.Method + " " + route.Path
		if !expected[key] {
			t.Errorf("unexpected route %s", key)
		}
		seen[key] = true
	}
	for key := range expected {
		if !seen[key] {
			t.Errorf("missing route %s", key)
		}
	}
	if len(seen) != len(expected) {
		t.Errorf("registered %d routes, want %d", len(seen), len(expected))
	}
}
