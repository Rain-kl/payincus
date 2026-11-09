package routes

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// ==================== harness ====================

// affTestContext builds a Gin context carrying an authenticated user. Only the
// branches that run before the first database access can be exercised without a
// live PostgreSQL; the conformance suite covers the rest.
func affTestContext(t *testing.T, method, target, body, contentType string, params gin.Params, user *lib.AccessTokenClaims) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	context.Request = request
	context.Params = params
	if user != nil {
		context.Set(middleware.ContextUserKey, user)
	}
	return context, recorder
}

func affTestUser() *lib.AccessTokenClaims {
	return &lib.AccessTokenClaims{ID: 7, Username: "alice", Role: "user", Status: "active"}
}

func affTestDecode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response body %q: %v", recorder.Body.String(), err)
	}
	return decoded
}

// ==================== helpers ====================

func TestAffQueryValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/aff/me/logs?page=2&type=renew&empty=", nil)

	value, present, isArray := affQueryValue(context, "page")
	if !present || isArray || value != "2" {
		t.Errorf("page = (%q, %v, %v), want (\"2\", true, false)", value, present, isArray)
	}

	value, present, isArray = affQueryValue(context, "type")
	if !present || isArray || value != "renew" {
		t.Errorf("type = (%q, %v, %v)", value, present, isArray)
	}

	value, present, isArray = affQueryValue(context, "empty")
	if !present || isArray || value != "" {
		t.Errorf("empty = (%q, %v, %v), want (\"\", true, false)", value, present, isArray)
	}

	if _, present, _ := affQueryValue(context, "missing"); present {
		t.Error("missing key reported as present")
	}

	context.Request = httptest.NewRequest(http.MethodGet, "/api/aff/me/logs?page=1&page=2", nil)
	if _, present, isArray := affQueryValue(context, "page"); !present || !isArray {
		t.Errorf("repeated page = (present=%v, isArray=%v), want (true, true)", present, isArray)
	}
}

func TestAffParseClampedPositiveQueryInteger(t *testing.T) {
	cases := []struct {
		name     string
		value    string
		present  bool
		fallback int64
		max      int64
		want     int64
		ok       bool
	}{
		{"absent uses fallback", "", false, 1, affMaxSafeInteger, 1, true},
		{"absent pageSize fallback", "", false, 20, affRouteMaxPageSize, 20, true},
		{"valid", "7", true, 1, affMaxSafeInteger, 7, true},
		{"clamped to max", "500", true, 20, affRouteMaxPageSize, 100, true},
		{"zero rejected", "0", true, 1, affMaxSafeInteger, 0, false},
		{"leading zero rejected", "01", true, 1, affMaxSafeInteger, 0, false},
		{"negative rejected", "-1", true, 1, affMaxSafeInteger, 0, false},
		{"empty rejected", "", true, 1, affMaxSafeInteger, 0, false},
		{"text rejected", "abc", true, 1, affMaxSafeInteger, 0, false},
		{"decimal rejected", "1.5", true, 1, affMaxSafeInteger, 0, false},
	}
	for _, testCase := range cases {
		got, ok := affParseClampedPositiveQueryInteger(testCase.value, testCase.present, testCase.fallback, testCase.max)
		if got != testCase.want || ok != testCase.ok {
			t.Errorf("%s: affParseClampedPositiveQueryInteger(%q, %v, %d, %d) = (%d, %v), want (%d, %v)",
				testCase.name, testCase.value, testCase.present, testCase.fallback, testCase.max,
				got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestAffParseOptionalLogType(t *testing.T) {
	if value, ok := affParseOptionalLogType("", false); value != "" || !ok {
		t.Errorf("absent = (%q, %v), want (\"\", true)", value, ok)
	}
	for _, valid := range []string{"new_purchase", "renew", "convert"} {
		if value, ok := affParseOptionalLogType(valid, true); string(value) != valid || !ok {
			t.Errorf("%q = (%q, %v), want unchanged/ok", valid, value, ok)
		}
	}
	if _, ok := affParseOptionalLogType("renew,convert", true); ok {
		t.Error("comma-joined types must be rejected (repeated query param in Fastify)")
	}
	if _, ok := affParseOptionalLogType("", true); ok {
		t.Error("present but empty type must be rejected")
	}
}

func TestAffParseOptionalWithdrawalStatus(t *testing.T) {
	for _, valid := range []string{"pending", "approved", "rejected"} {
		if value, ok := affParseOptionalWithdrawalStatus(valid, true); string(value) != valid || !ok {
			t.Errorf("%q = (%q, %v), want unchanged/ok", valid, value, ok)
		}
	}
	if _, ok := affParseOptionalWithdrawalStatus("nope", true); ok {
		t.Error("unknown status must be rejected")
	}
	if value, ok := affParseOptionalWithdrawalStatus("", false); value != "" || !ok {
		t.Errorf("absent = (%q, %v)", value, ok)
	}
}

func TestAffNormalizeRequiredText(t *testing.T) {
	// JavaScript trims the standard whitespace set, including U+3000.
	value := affNormalizeRequiredText("  拒绝原因  ", 500)
	if value == nil || *value != "拒绝原因" {
		t.Fatalf("trimmed = %v, want 拒绝原因", value)
	}

	if value := affNormalizeRequiredText("   ", 500); value != nil {
		t.Errorf("blank = %v, want nil", *value)
	}
	if value := affNormalizeRequiredText(42, 500); value != nil {
		t.Errorf("non-string = %v, want nil", *value)
	}
	if value := affNormalizeRequiredText(nil, 500); value != nil {
		t.Errorf("nil = %v, want nil", *value)
	}

	// Exactly maxLength is accepted, maxLength+1 is not (UTF-16 units).
	exact := strings.Repeat("a", affRejectReasonMaxLength)
	if value := affNormalizeRequiredText(exact, affRejectReasonMaxLength); value == nil {
		t.Error("exact-length text was rejected")
	}
	tooLong := strings.Repeat("a", affRejectReasonMaxLength+1)
	if value := affNormalizeRequiredText(tooLong, affRejectReasonMaxLength); value != nil {
		t.Error("over-length text was accepted")
	}

	// An astral rune counts as two UTF-16 units, exactly like JavaScript's length.
	astralExact := strings.Repeat("😀", affRejectReasonMaxLength/2)
	if value := affNormalizeRequiredText(astralExact, affRejectReasonMaxLength); value == nil {
		t.Error("exactly 500 UTF-16 units of astral runes was rejected")
	}
}

func TestAffBodyObject(t *testing.T) {
	if _, ok := affBodyObject(nil); ok {
		t.Error("nil body must report ok=false (TypeError on destructuring)")
	}

	value, ok := affBodyObject(map[string]any{"a": 1})
	if !ok || value["a"] != 1 {
		t.Errorf("object = (%v, %v)", value, ok)
	}

	// Primitives and arrays destructure fine in JavaScript but expose no fields.
	for _, raw := range []any{"text", float64(1), true, []any{1, 2}} {
		value, ok := affBodyObject(raw)
		if !ok || len(value) != 0 {
			t.Errorf("affBodyObject(%#v) = (%v, %v), want (empty, true)", raw, value, ok)
		}
	}
}

func TestAffInt32ID(t *testing.T) {
	if value, ok := affInt32ID(5); !ok || value != 5 {
		t.Errorf("affInt32ID(5) = (%d, %v)", value, ok)
	}
	if _, ok := affInt32ID(math.MaxInt32 + 1); ok {
		t.Error("out-of-range id must not fit")
	}
	if _, ok := affInt32ID(0); !ok {
		t.Error("zero fits the column domain (it simply matches no row)")
	}
}

func TestAffFixed2(t *testing.T) {
	cases := []struct {
		value float64
		want  string
	}{
		{10, "10.00"},
		{1.5, "1.50"},
		{0.005, "0.01"},
		{2.675, "2.67"},
		{0.125, "0.13"},
		{0, "0.00"},
	}
	for _, testCase := range cases {
		if got := affFixed2(testCase.value); got != testCase.want {
			t.Errorf("affFixed2(%v) = %q, want %q", testCase.value, got, testCase.want)
		}
	}
}

// ==================== handler contract (pre-DB branches) ====================

func TestAffConvertRejections(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		contentType string
		wantStatus  int
		wantBody    map[string]any
	}{
		{
			name:       "missing body is a destructuring TypeError",
			wantStatus: http.StatusInternalServerError,
			wantBody:   map[string]any{"error": "Internal server error"},
		},
		{
			name:        "empty object",
			body:        "{}",
			contentType: "application/json",
			wantStatus:  http.StatusBadRequest,
			wantBody:    map[string]any{"error": "请输入有效的转化金额"},
		},
		{
			name:        "zero amount",
			body:        `{"amount":0}`,
			contentType: "application/json",
			wantStatus:  http.StatusBadRequest,
			wantBody:    map[string]any{"error": "请输入有效的转化金额"},
		},
		{
			name:        "negative amount",
			body:        `{"amount":-1}`,
			contentType: "application/json",
			wantStatus:  http.StatusBadRequest,
			wantBody:    map[string]any{"error": "请输入有效的转化金额"},
		},
		{
			name:        "string amount fails typeof",
			body:        `{"amount":"5"}`,
			contentType: "application/json",
			wantStatus:  http.StatusBadRequest,
			wantBody:    map[string]any{"error": "请输入有效的转化金额"},
		},
		{
			name:        "null body is a TypeError",
			body:        "null",
			contentType: "application/json",
			wantStatus:  http.StatusInternalServerError,
			wantBody:    map[string]any{"error": "Internal server error"},
		},
	}

	for _, testCase := range cases {
		context, recorder := affTestContext(t, http.MethodPost, "/api/aff/me/convert", testCase.body, testCase.contentType, nil, affTestUser())
		affConvertHandler(context)
		if recorder.Code != testCase.wantStatus {
			t.Errorf("%s: status = %d, want %d (body %q)", testCase.name, recorder.Code, testCase.wantStatus, recorder.Body.String())
			continue
		}
		decoded := affTestDecode(t, recorder)
		for key, want := range testCase.wantBody {
			if decoded[key] != want {
				t.Errorf("%s: %s = %#v, want %#v", testCase.name, key, decoded[key], want)
			}
		}
	}
}

func TestAffValidateRejections(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantError  string
	}{
		{"missing code", `{}`, http.StatusBadRequest, "请输入优惠码"},
		{"blank code", `{"code":"   "}`, http.StatusBadRequest, "请输入优惠码"},
		{"non-string code", `{"code":5}`, http.StatusBadRequest, "请输入优惠码"},
		{"missing plan id", `{"code":"ABC"}`, http.StatusBadRequest, "方案 ID 无效"},
		{"zero plan id", `{"code":"ABC","packagePlanId":0}`, http.StatusBadRequest, "方案 ID 无效"},
		{"string plan id", `{"code":"ABC","packagePlanId":"5"}`, http.StatusBadRequest, "方案 ID 无效"},
		{"fractional plan id", `{"code":"ABC","packagePlanId":1.5}`, http.StatusBadRequest, "方案 ID 无效"},
		{"null plan id", `{"code":"ABC","packagePlanId":null}`, http.StatusBadRequest, "方案 ID 无效"},
	}

	for _, testCase := range cases {
		context, recorder := affTestContext(t, http.MethodPost, "/api/aff/validate", testCase.body, "application/json", nil, affTestUser())
		affValidateHandler(context)
		if recorder.Code != testCase.wantStatus {
			t.Errorf("%s: status = %d, want %d (body %q)", testCase.name, recorder.Code, testCase.wantStatus, recorder.Body.String())
			continue
		}
		if decoded := affTestDecode(t, recorder); decoded["error"] != testCase.wantError {
			t.Errorf("%s: error = %#v, want %q", testCase.name, decoded["error"], testCase.wantError)
		}
	}
}

func TestAffCreateCodeRejections(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantError  string
	}{
		{"zero plan id", `{"packagePlanId":0}`, http.StatusBadRequest, "方案 ID 无效"},
		{"negative plan id", `{"packagePlanId":-3}`, http.StatusBadRequest, "方案 ID 无效"},
		{"string plan id", `{"packagePlanId":"5"}`, http.StatusBadRequest, "方案 ID 无效"},
		{"null plan id", `{"packagePlanId":null}`, http.StatusBadRequest, "方案 ID 无效"},
		{"fractional plan id", `{"packagePlanId":2.5}`, http.StatusBadRequest, "方案 ID 无效"},
	}

	for _, testCase := range cases {
		context, recorder := affTestContext(t, http.MethodPost, "/api/aff/me/codes", testCase.body, "application/json", nil, affTestUser())
		affCreateCodeHandler(context)
		if recorder.Code != testCase.wantStatus {
			t.Errorf("%s: status = %d, want %d (body %q)", testCase.name, recorder.Code, testCase.wantStatus, recorder.Body.String())
			continue
		}
		if decoded := affTestDecode(t, recorder); decoded["error"] != testCase.wantError {
			t.Errorf("%s: error = %#v, want %q", testCase.name, decoded["error"], testCase.wantError)
		}
	}
}

func TestAffDeleteCodeInvalidRouteID(t *testing.T) {
	cases := []string{"0", "-1", "01", "abc", "1.5"}
	for _, rawID := range cases {
		context, recorder := affTestContext(t, http.MethodDelete, "/api/aff/me/codes/"+rawID, "",
			"", gin.Params{{Key: "codeId", Value: rawID}}, affTestUser())
		affDeleteCodeHandler(context)

		if recorder.Code != http.StatusBadRequest {
			t.Errorf("id %q: status = %d, want 400", rawID, recorder.Code)
			continue
		}
		decoded := affTestDecode(t, recorder)
		if decoded["code"] != "INVALID_ID" || decoded["error"] != "Invalid ID" {
			t.Errorf("id %q: body = %v, want the apiError envelope", rawID, decoded)
		}
	}
}

// Fastify parses the body before the handler runs, so a malformed JSON body wins
// over an invalid route id.
func TestAffDeleteCodeBodyParsedBeforeID(t *testing.T) {
	context, recorder := affTestContext(t, http.MethodDelete, "/api/aff/me/codes/0", "{",
		"application/json", gin.Params{{Key: "codeId", Value: "0"}}, affTestUser())
	affDeleteCodeHandler(context)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if decoded := affTestDecode(t, recorder); decoded["error"] != "Body is not valid JSON but content-type is set to 'application/json'" {
		t.Errorf("error = %#v, want the Fastify body-parser message", decoded["error"])
	}
}

func TestAffApproveInvalidRouteID(t *testing.T) {
	// The approve handler never reads request.body, so an absent body is fine.
	context, recorder := affTestContext(t, http.MethodPost, "/api/aff/admin/withdrawals/0/approve", "",
		"", gin.Params{{Key: "withdrawalId", Value: "0"}}, affTestUser())
	affApproveWithdrawalHandler(context)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if decoded := affTestDecode(t, recorder); decoded["code"] != "INVALID_ID" {
		t.Errorf("body = %v, want INVALID_ID", decoded)
	}
}

func TestAffRejectPreDBRejections(t *testing.T) {
	cases := []struct {
		name       string
		param      string
		body       string
		withBody   bool
		wantStatus int
		wantError  string
		wantCode   string
	}{
		{
			name:       "invalid id wins over missing reason",
			param:      "0",
			body:       `{"reason":"x"}`,
			withBody:   true,
			wantStatus: http.StatusBadRequest,
			wantError:  "Invalid ID",
			wantCode:   "INVALID_ID",
		},
		{
			name:       "missing reason",
			param:      "5",
			body:       `{}`,
			withBody:   true,
			wantStatus: http.StatusBadRequest,
			wantError:  "请填写拒绝原因",
		},
		{
			name:       "blank reason",
			param:      "5",
			body:       `{"reason":"   "}`,
			withBody:   true,
			wantStatus: http.StatusBadRequest,
			wantError:  "请填写拒绝原因",
		},
		{
			name:       "non-string reason",
			param:      "5",
			body:       `{"reason":12}`,
			withBody:   true,
			wantStatus: http.StatusBadRequest,
			wantError:  "请填写拒绝原因",
		},
		{
			name:       "absent body is a destructuring TypeError",
			param:      "5",
			wantStatus: http.StatusInternalServerError,
			wantError:  "Internal server error",
		},
	}

	for _, testCase := range cases {
		contentType := ""
		if testCase.withBody {
			contentType = "application/json"
		}
		context, recorder := affTestContext(t, http.MethodPost, "/api/aff/admin/withdrawals/"+testCase.param+"/reject",
			testCase.body, contentType, gin.Params{{Key: "withdrawalId", Value: testCase.param}}, affTestUser())
		affRejectWithdrawalHandler(context)

		if recorder.Code != testCase.wantStatus {
			t.Errorf("%s: status = %d, want %d (body %q)", testCase.name, recorder.Code, testCase.wantStatus, recorder.Body.String())
			continue
		}
		decoded := affTestDecode(t, recorder)
		if decoded["error"] != testCase.wantError {
			t.Errorf("%s: error = %#v, want %q", testCase.name, decoded["error"], testCase.wantError)
		}
		if testCase.wantCode != "" && decoded["code"] != testCase.wantCode {
			t.Errorf("%s: code = %#v, want %q", testCase.name, decoded["code"], testCase.wantCode)
		}
	}
}

func TestAffLogsInvalidQueryParams(t *testing.T) {
	cases := []string{
		"page=0",
		"page=01",
		"page=abc",
		"pageSize=-5",
		"type=bogus",
		"type=",
		"page=1&page=2",
	}
	for _, query := range cases {
		context, recorder := affTestContext(t, http.MethodGet, "/api/aff/me/logs?"+query, "",
			"", nil, affTestUser())
		affMeLogsHandler(context)

		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400 (body %q)", query, recorder.Code, recorder.Body.String())
			continue
		}
		if decoded := affTestDecode(t, recorder); decoded["code"] != "INVALID_PARAMS" {
			t.Errorf("%q: body = %v, want INVALID_PARAMS", query, decoded)
		}
	}
}

func TestAffWithdrawalsInvalidQueryParams(t *testing.T) {
	for _, query := range []string{"status=bogus", "pageSize=0", "page=-1"} {
		context, recorder := affTestContext(t, http.MethodGet, "/api/aff/me/withdrawals?"+query, "",
			"", nil, affTestUser())
		affMeWithdrawalsHandler(context)

		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", query, recorder.Code)
			continue
		}
		if decoded := affTestDecode(t, recorder); decoded["code"] != "INVALID_PARAMS" {
			t.Errorf("%q: body = %v, want INVALID_PARAMS", query, decoded)
		}
	}
}

// ==================== registration ====================

func TestRegisterAffRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.RedirectTrailingSlash = false
	RegisterAff(engine)

	registered := map[string]bool{}
	for _, route := range engine.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	want := []string{
		"GET /api/aff/me",
		"GET /api/aff/me/codes",
		"GET /api/aff/me/available-plans",
		"POST /api/aff/me/codes",
		"DELETE /api/aff/me/codes/:codeId",
		"GET /api/aff/me/logs",
		"GET /api/aff/leaderboard",
		"POST /api/aff/validate",
		"POST /api/aff/me/convert",
		"GET /api/aff/me/withdrawals",
		"GET /api/aff/admin/withdrawals",
		"POST /api/aff/admin/withdrawals/:withdrawalId/approve",
		"POST /api/aff/admin/withdrawals/:withdrawalId/reject",
	}
	for _, key := range want {
		if !registered[key] {
			t.Errorf("missing route %s", key)
		}
	}
	if len(registered) != len(want) {
		t.Errorf("registered %d routes, want %d: %v", len(registered), len(want), registered)
	}

	// Fastify's ignoreTrailingSlash is false and Gin's redirect is disabled, so a
	// trailing slash on a non-root route must stay a 404.
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/aff/me/", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("GET /api/aff/me/ = %d, want 404", recorder.Code)
	}
}

func TestRegisterAffRequiresAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.RedirectTrailingSlash = false
	RegisterAff(engine)

	requests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/aff/me"},
		{http.MethodGet, "/api/aff/me/codes"},
		{http.MethodGet, "/api/aff/me/available-plans"},
		{http.MethodPost, "/api/aff/me/codes"},
		{http.MethodDelete, "/api/aff/me/codes/1"},
		{http.MethodGet, "/api/aff/me/logs"},
		{http.MethodGet, "/api/aff/leaderboard"},
		{http.MethodPost, "/api/aff/validate"},
		{http.MethodPost, "/api/aff/me/convert"},
		{http.MethodGet, "/api/aff/me/withdrawals"},
		{http.MethodGet, "/api/aff/admin/withdrawals"},
		{http.MethodPost, "/api/aff/admin/withdrawals/1/approve"},
		{http.MethodPost, "/api/aff/admin/withdrawals/1/reject"},
	}

	for _, request := range requests {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(request.method, request.path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s (anonymous) = %d, want 401", request.method, request.path, recorder.Code)
		}
	}
}
