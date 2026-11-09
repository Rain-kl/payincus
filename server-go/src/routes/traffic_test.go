// Unit tests for the pure half of routes/traffic.go: route-id parsing, the
// bigint-limit body contract, the wire serializers and the Phase-7 seam defaults.
// Database-backed behaviour is covered by test/conformance/requests-traffic.json.
package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

func TestTrafficParseInt32RouteID(t *testing.T) {
	cases := []struct {
		raw            string
		want           int32
		wantOK         bool
		wantOutOfRange bool
	}{
		{"1", 1, true, false},
		{"2147483647", 2147483647, true, false},
		{"0", 0, false, false},
		{"01", 0, false, false}, // strict: canonical positive decimal only
		{"-1", 0, false, false},
		{"abc", 0, false, false},
		{"1.5", 0, false, false},
		{"", 0, false, false},
		{"2147483648", 0, false, true},        // parses, but Prisma P2020 → 500
		{"9007199254740993", 0, false, false}, // above Number.MAX_SAFE_INTEGER
	}
	for _, tc := range cases {
		got, ok, outOfRange := trafficParseInt32RouteID(tc.raw)
		if ok != tc.wantOK || outOfRange != tc.wantOutOfRange || (ok && got != tc.want) {
			t.Errorf("id=%q: got (%d, %v, %v), want (%d, %v, %v)",
				tc.raw, got, ok, outOfRange, tc.want, tc.wantOK, tc.wantOutOfRange)
		}
	}
}

func TestTrafficSerializeBigInt(t *testing.T) {
	if value := trafficSerializeBigInt(nil); value != nil {
		t.Fatalf("null bigint = %v, want nil", value)
	}
	zero := int64(0)
	if value := trafficSerializeBigInt(&zero); value != "0" {
		t.Fatalf("zero = %v, want \"0\"", value)
	}
	negative := int64(-5)
	if value := trafficSerializeBigInt(&negative); value != "-5" {
		t.Fatalf("negative = %v", value)
	}
	large := int64(9223372036854775807)
	if value := trafficSerializeBigInt(&large); value != "9223372036854775807" {
		t.Fatalf("max int64 = %v", value)
	}
	if value := trafficSerializeBigIntValue(1024); value != "1024" {
		t.Fatalf("value = %v", value)
	}
}

func TestTrafficLimitFormatted(t *testing.T) {
	// A zero bigint is falsy in the TypeScript, so it formats as null even though
	// the sibling monthlyLimit serializes as "0".
	zero := int64(0)
	if value := trafficLimitFormatted(&zero); value != nil {
		t.Fatalf("zero limit formatted = %v, want nil", value)
	}
	if value := trafficLimitFormatted(nil); value != nil {
		t.Fatalf("nil limit formatted = %v, want nil", value)
	}
	bytes := int64(1)
	if value := trafficLimitFormatted(&bytes); value != "1.00 B" {
		t.Fatalf("1 byte = %v", value)
	}
	kib := int64(1024)
	if value := trafficLimitFormatted(&kib); value != "1.00 KB" {
		t.Fatalf("1024 bytes = %v", value)
	}
}

func TestTrafficResetDayOrDefault(t *testing.T) {
	if value := trafficResetDayOrDefault(nil); value != 1 {
		t.Fatalf("missing host reset day = %d, want 1", value)
	}
	if value := trafficResetDayOrDefault(&db.TrafficHost{TrafficResetDay: 17}); value != 17 {
		t.Fatalf("host reset day = %d, want 17", value)
	}
}

// newTrafficJSONContext builds a gin context for a PUT with the supplied raw body.
func newTrafficJSONContext(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/users/1/traffic/limit", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	return context, recorder
}

func TestTrafficReadMonthlyLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	valid := []struct {
		name string
		body string
		want *int64
	}{
		{name: "value", body: `{"monthlyLimit":"1099511627776"}`, want: int64Pointer(1099511627776)},
		{name: "zero", body: `{"monthlyLimit":"0"}`, want: int64Pointer(0)},
		{name: "maximum bigint", body: `{"monthlyLimit":"9223372036854775807"}`, want: int64Pointer(9223372036854775807)},
		{name: "explicit null", body: `{"monthlyLimit":null}`, want: nil},
		{name: "empty string", body: `{"monthlyLimit":""}`, want: nil},
		{name: "absent key", body: `{}`, want: nil},
		{name: "non-object body", body: `[]`, want: nil},
	}
	for _, tc := range valid {
		context, recorder := newTrafficJSONContext(t, tc.body)
		limit, ok := trafficReadMonthlyLimit(context)
		if !ok {
			t.Errorf("%s: unexpected failure: %s", tc.name, recorder.Body.String())
			continue
		}
		switch {
		case tc.want == nil && limit != nil:
			t.Errorf("%s: limit = %d, want nil", tc.name, *limit)
		case tc.want != nil && limit == nil:
			t.Errorf("%s: limit = nil, want %d", tc.name, *tc.want)
		case tc.want != nil && *limit != *tc.want:
			t.Errorf("%s: limit = %d, want %d", tc.name, *limit, *tc.want)
		}
	}

	invalid := []struct {
		name string
		body string
	}{
		{name: "letters", body: `{"monthlyLimit":"abc"}`},
		{name: "negative", body: `{"monthlyLimit":"-1"}`},
		{name: "float", body: `{"monthlyLimit":"1.5"}`},
		{name: "overflow", body: `{"monthlyLimit":"9223372036854775808"}`},
		{name: "json number is not a string", body: `{"monthlyLimit":123}`},
		{name: "boolean", body: `{"monthlyLimit":true}`},
	}
	for _, tc := range invalid {
		context, recorder := newTrafficJSONContext(t, tc.body)
		if _, ok := trafficReadMonthlyLimit(context); ok {
			t.Errorf("%s: expected rejection", tc.name)
			continue
		}
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, recorder.Code)
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body is not JSON: %v", tc.name, err)
		}
		if body["error"] != "Invalid parameters" || body["code"] != "INVALID_PARAMS" ||
			body["details"] != "Invalid monthly traffic limit" {
			t.Errorf("%s: body = %v", tc.name, body)
		}
	}

	// A JSON `null` body makes `const { monthlyLimit } = request.body` throw in the
	// TypeScript, which app.ts renders as a 500.
	context, recorder := newTrafficJSONContext(t, `null`)
	if _, ok := trafficReadMonthlyLimit(context); ok {
		t.Fatalf("null body must be rejected")
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("null body status = %d, want 500", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("null body response is not JSON: %v", err)
	}
	if body["error"] != "Internal server error" {
		t.Fatalf("null body response = %v", body)
	}
}

func int64Pointer(value int64) *int64 { return &value }

func TestTrafficUserSummaryWireShape(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Unlimited: monthlyLimit is null, no formatted limit, percentage 0.
	summary := trafficUserSummary(&db.UserTrafficQuota{
		MonthlyTrafficUsed:  1024,
		ExtraTrafficQuota:   0,
		ExtraTrafficUsed:    0,
		TrafficStatus:       models.TrafficStatusNORMAL,
		MonthlyTrafficLimit: nil,
	})
	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["monthlyUsed"] != "1024" {
		t.Errorf("monthlyUsed = %v", decoded["monthlyUsed"])
	}
	if decoded["monthlyUsedFormatted"] != "1.00 KB" {
		t.Errorf("monthlyUsedFormatted = %v", decoded["monthlyUsedFormatted"])
	}
	if decoded["monthlyLimit"] != nil || decoded["monthlyLimitFormatted"] != nil {
		t.Errorf("unlimited limit = %v / %v", decoded["monthlyLimit"], decoded["monthlyLimitFormatted"])
	}
	if decoded["extraQuota"] != "0" {
		t.Errorf("extraQuota = %v", decoded["extraQuota"])
	}
	if decoded["percentage"] != float64(0) {
		t.Errorf("percentage = %v", decoded["percentage"])
	}

	// 75 used of (100 + 50) extra-扩容 → 50%.
	limit := int64(100)
	summary = trafficUserSummary(&db.UserTrafficQuota{
		MonthlyTrafficUsed:  75,
		MonthlyTrafficLimit: &limit,
		ExtraTrafficQuota:   50,
		TrafficStatus:       models.TrafficStatusNORMAL,
	})
	encoded, _ = json.Marshal(summary)
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["monthlyLimit"] != "150" || decoded["monthlyLimitFormatted"] != "150.00 B" {
		t.Errorf("limit = %v / %v", decoded["monthlyLimit"], decoded["monthlyLimitFormatted"])
	}
	if decoded["percentage"] != float64(50) {
		t.Errorf("percentage = %v", decoded["percentage"])
	}

	// A zero effective limit is falsy in the TypeScript: "0" plus a null formatted.
	zero := int64(0)
	summary = trafficUserSummary(&db.UserTrafficQuota{MonthlyTrafficLimit: &zero})
	encoded, _ = json.Marshal(summary)
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["monthlyLimit"] != "0" || decoded["monthlyLimitFormatted"] != nil {
		t.Errorf("zero limit = %v / %v", decoded["monthlyLimit"], decoded["monthlyLimitFormatted"])
	}
}

func TestTrafficSchedulerSeamsFailExplicitly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	if err := TrafficReconcileInstanceIDs(nil, []int32{1}); err == nil {
		t.Fatalf("instance reconciliation seam must fail until Phase 7")
	} else if !strings.Contains(err.Error(), "Phase 7") {
		t.Fatalf("instance reconciliation seam error = %v", err)
	}
	if err := TrafficReconcileUser(nil, 1); err == nil {
		t.Fatalf("user reconciliation seam must fail until Phase 7")
	}
	if err := TrafficRunJob(nil); err == nil {
		t.Fatalf("traffic job seam must fail until Phase 7")
	}
}

func TestRegisterTrafficRoutesRequireAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterTraffic(router)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/me/traffic"},
		{http.MethodGet, "/api/users/1/traffic"},
		{http.MethodPut, "/api/users/1/traffic/limit"},
		{http.MethodGet, "/api/instances/1/traffic"},
		{http.MethodGet, "/api/instances/1/traffic/history"},
		{http.MethodPut, "/api/instances/1/traffic/limit"},
		{http.MethodPost, "/api/instances/1/traffic/reset"},
		{http.MethodGet, "/api/hosts/1/traffic/history"},
		{http.MethodPost, "/api/traffic/collect"},
		{http.MethodPost, "/api/traffic/sync-package-limits"},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(tc.method, tc.path, nil)
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", tc.method, tc.path, recorder.Code)
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s: body is not JSON: %v", tc.method, tc.path, err)
			continue
		}
		if body["error"] != "Unauthorized" {
			t.Errorf("%s %s: body = %v", tc.method, tc.path, body)
		}
	}

	// A sibling path is still an unmatched route, proving the mounts above are the
	// only ones this module registers (no module-root route in traffic.ts).
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/me/traffic/extra", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unmatched path status = %d, want 404", recorder.Code)
	}
}
