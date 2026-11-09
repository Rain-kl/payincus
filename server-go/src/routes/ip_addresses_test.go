package routes

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ipAddressesTestEngine mounts the handlers behind a stub auth layer, so the
// handlers can be exercised without a JWT or a database. Anything reaching the db
// package fails with 500 (db not initialized), which is why every case below is
// rejected before the first query.
func ipAddressesTestEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(middleware.ContextUserKey, &lib.AccessTokenClaims{
			ID:       42,
			Username: "tester",
			Role:     "user",
			Status:   "active",
		})
		c.Next()
	})
	engine.GET("/api/instances/:id/ips", ipAddressesListHandler)
	engine.POST("/api/instances/:id/ips", ipAddressesCreateHandler)
	engine.DELETE("/api/instances/:id/ips/:ipId", ipAddressesDeleteHandler)
	engine.PUT("/api/instances/:id/ips/:ipId/custom", ipAddressesCustomHandler)
	engine.POST("/api/instances/:id/subnet", ipAddressesAllocateSubnetHandler)
	engine.DELETE("/api/instances/:id/subnet/:subnetId", ipAddressesDeleteSubnetHandler)
	engine.GET("/api/instances/:id/subnets", ipAddressesListSubnetsHandler)
	return engine
}

func ipAddressesPerform(engine *gin.Engine, method, path, contentType, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func ipAddressesDecode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a JSON object: %q (%v)", recorder.Body.String(), err)
	}
	return body
}

// TestRegisterIPAddressesMountsRoutes also proves the gin radix tree accepts the
// `:id` wildcard next to the other /api/instances/:id modules (a conflicting
// wildcard name would panic here).
func TestRegisterIPAddressesMountsRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterIPAddresses(engine)

	want := []string{
		"GET /api/instances/:id/ips",
		"POST /api/instances/:id/ips",
		"DELETE /api/instances/:id/ips/:ipId",
		"PUT /api/instances/:id/ips/:ipId/custom",
		"POST /api/instances/:id/subnet",
		"DELETE /api/instances/:id/subnet/:subnetId",
		"GET /api/instances/:id/subnets",
	}
	got := map[string]bool{}
	for _, route := range engine.Routes() {
		got[route.Method+" "+route.Path] = true
	}
	for _, route := range want {
		if !got[route] {
			t.Errorf("route %q is not mounted", route)
		}
	}
}

// fastify.authenticateUser rejects an anonymous request before the handler runs.
func TestIPAddressesRequireAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterIPAddresses(engine)

	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/instances/1/ips"},
		{http.MethodGet, "/api/instances/1/subnets"},
		{http.MethodPost, "/api/instances/1/ips"},
		{http.MethodDelete, "/api/instances/1/ips/2"},
	}
	for _, tc := range cases {
		recorder := ipAddressesPerform(engine, tc.method, tc.path, "", "")
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", tc.method, tc.path, recorder.Code)
			continue
		}
		if body := ipAddressesDecode(t, recorder); body["error"] != "Unauthorized" {
			t.Errorf("%s %s: body = %v", tc.method, tc.path, body)
		}
	}
}

// parsePositiveId rejects 0, negatives, leading zeros and non-numbers with
// 400 INVALID_PARAMS (apiError(ErrorCode.INVALID_PARAMS)).
func TestIPAddressesInvalidRouteIDs(t *testing.T) {
	engine := ipAddressesTestEngine()
	jsonBody := "{}"

	cases := []struct{ method, path, body string }{
		{http.MethodGet, "/api/instances/0/ips", ""},
		{http.MethodGet, "/api/instances/-1/ips", ""},
		{http.MethodGet, "/api/instances/01/ips", ""},
		{http.MethodGet, "/api/instances/abc/ips", ""},
		{http.MethodGet, "/api/instances/abc/subnets", ""},
		{http.MethodPost, "/api/instances/abc/ips", jsonBody},
		{http.MethodDelete, "/api/instances/abc/ips/1", ""},
		{http.MethodDelete, "/api/instances/1/ips/0", ""},
		{http.MethodDelete, "/api/instances/1/ips/01", ""},
		{http.MethodPut, "/api/instances/abc/ips/1/custom", `{"address":"2001:db8::1"}`},
		{http.MethodPut, "/api/instances/1/ips/abc/custom", `{"address":"2001:db8::1"}`},
		{http.MethodPost, "/api/instances/abc/subnet", jsonBody},
		{http.MethodDelete, "/api/instances/abc/subnet/1", ""},
		{http.MethodDelete, "/api/instances/1/subnet/abc", ""},
	}

	for _, tc := range cases {
		contentType := ""
		if tc.body != "" {
			contentType = "application/json"
		}
		recorder := ipAddressesPerform(engine, tc.method, tc.path, contentType, tc.body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s %s: status = %d, want 400 (body %s)", tc.method, tc.path, recorder.Code, recorder.Body.String())
			continue
		}
		body := ipAddressesDecode(t, recorder)
		if body["error"] != "Invalid parameters" || body["code"] != "INVALID_PARAMS" {
			t.Errorf("%s %s: body = %v", tc.method, tc.path, body)
		}
	}
}

// The POST body schema is `{type:'object', properties:{customAddress:{type:'string'}}}`
// and Fastify validates it before the handler, so a non-object body and a
// non-string customAddress are VALIDATION_ERRORs.
func TestIPAddressesCreateValidatesBody(t *testing.T) {
	engine := ipAddressesTestEngine()

	cases := []struct {
		body      string
		wantError string
	}{
		{"[]", "must be object"},
		{`"text"`, "must be object"},
		{"null", "must be object"},
		// Fastify's setErrorHandler pushes AJV's raw message for `type` failures,
		// which never carries the field name.
		{`{"customAddress":{}}`, "must be string"},
		{`{"customAddress":[1,2]}`, "must be string"},
	}
	for _, tc := range cases {
		recorder := ipAddressesPerform(engine, http.MethodPost, "/api/instances/abc/ips", "application/json", tc.body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", tc.body, recorder.Code)
			continue
		}
		body := ipAddressesDecode(t, recorder)
		if body["error"] != tc.wantError || body["code"] != "VALIDATION_ERROR" {
			t.Errorf("body %s: response = %v, want error=%q code=VALIDATION_ERROR", tc.body, body, tc.wantError)
		}
	}

	// AJV's coerceTypes turns scalars into strings, so these pass validation and
	// fail later on the id.
	for _, body := range []string{`{"customAddress":123}`, `{"customAddress":true}`} {
		recorder := ipAddressesPerform(engine, http.MethodPost, "/api/instances/abc/ips", "application/json", body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, recorder.Code)
			continue
		}
		if decoded := ipAddressesDecode(t, recorder); decoded["code"] != "INVALID_PARAMS" {
			t.Errorf("body %s: response = %v, want INVALID_PARAMS", body, decoded)
		}
	}
}

// PUT /custom has no schema: `request.body.address?.trim()` throws on a missing
// body (500) but a missing/blank address is just an invalid format (400).
func TestIPAddressesCustomAddressBodyHandling(t *testing.T) {
	engine := ipAddressesTestEngine()

	internalError := []struct{ contentType, body string }{
		{"", ""},
		{"application/json", "null"},
		{"application/json", `{"address":123}`},
		{"application/json", `{"address":{}}`},
	}
	for _, tc := range internalError {
		recorder := ipAddressesPerform(engine, http.MethodPut, "/api/instances/1/ips/1/custom", tc.contentType, tc.body)
		if recorder.Code != http.StatusInternalServerError {
			t.Errorf("body %q: status = %d, want 500", tc.body, recorder.Code)
			continue
		}
		if body := ipAddressesDecode(t, recorder); body["error"] != "Internal server error" {
			t.Errorf("body %q: response = %v", tc.body, body)
		}
	}

	invalidFormat := []struct{ contentType, body string }{
		{"application/json", "{}"},
		{"application/json", `{"address":null}`},
		{"application/json", `{"address":""}`},
		{"application/json", `{"address":"   "}`},
		{"application/json", `{"address":"not-an-ip"}`},
		{"application/json", `{"address":"10.0.0.1"}`},
		{"application/json", `"2001:db8::1"`},
	}
	for _, tc := range invalidFormat {
		recorder := ipAddressesPerform(engine, http.MethodPut, "/api/instances/1/ips/1/custom", tc.contentType, tc.body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400 (body %s)", tc.body, recorder.Code, recorder.Body.String())
			continue
		}
		body := ipAddressesDecode(t, recorder)
		if body["error"] != "Invalid IPv6 address format" || body["code"] != "INVALID_IPV6_FORMAT" {
			t.Errorf("body %q: response = %v", tc.body, body)
		}
	}
}

// POST /subnet destructures request.body directly, so a missing body is a 500.
func TestIPAddressesAllocateSubnetBodyHandling(t *testing.T) {
	engine := ipAddressesTestEngine()

	for _, tc := range []struct{ contentType, body string }{
		{"", ""},
		{"application/json", "null"},
	} {
		recorder := ipAddressesPerform(engine, http.MethodPost, "/api/instances/1/subnet", tc.contentType, tc.body)
		if recorder.Code != http.StatusInternalServerError {
			t.Errorf("body %q: status = %d, want 500", tc.body, recorder.Code)
		}
	}

	// A non-object body destructures to undefined fields, so the id check runs.
	for _, body := range []string{`{}`, `"text"`, `[]`} {
		recorder := ipAddressesPerform(engine, http.MethodPost, "/api/instances/abc/subnet", "application/json", body)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, recorder.Code)
			continue
		}
		if decoded := ipAddressesDecode(t, recorder); decoded["code"] != "INVALID_PARAMS" {
			t.Errorf("body %q: response = %v", body, decoded)
		}
	}
}

func TestIPAddressesBuildIPv6Config(t *testing.T) {
	records := []models.IpAddress{
		{ID: 1, Address: "2001:db8::1", Type: models.IpTypeInet6, IsPrimary: true},
		{ID: 2, Address: "2001:db8::2", Type: models.IpTypeInet6},
		{ID: 3, Address: "2001:db8::3", Type: models.IpTypeInet6},
		{ID: 4, Address: "10.10.1.5", Type: models.IpTypeInet4},
	}

	config, ok := ipAddressesBuildIPv6Config(records, nil)
	if !ok {
		t.Fatal("expected a primary IPv6 record")
	}
	if config.PrimaryIP != "2001:db8::1" {
		t.Errorf("primary = %q", config.PrimaryIP)
	}
	if len(config.ExtraIPs) != 2 || config.ExtraIPs[0] != "2001:db8::2" || config.ExtraIPs[1] != "2001:db8::3" {
		t.Errorf("extra = %v", config.ExtraIPs)
	}
	if config.Subnet != nil {
		t.Errorf("subnet must be nil, got %v", *config.Subnet)
	}

	exclude := int32(2)
	config, ok = ipAddressesBuildIPv6Config(records, &exclude)
	if !ok || len(config.ExtraIPs) != 1 || config.ExtraIPs[0] != "2001:db8::3" {
		t.Errorf("excluded config = %+v ok=%v", config, ok)
	}

	withoutPrimary := []models.IpAddress{
		{ID: 1, Address: "2001:db8::2", Type: models.IpTypeInet6},
		{ID: 2, Address: "10.10.1.5", Type: models.IpTypeInet4, IsPrimary: true},
	}
	if _, ok := ipAddressesBuildIPv6Config(withoutPrimary, nil); ok {
		t.Error("an inet4 primary must not satisfy the IPv6 primary lookup")
	}
	if _, ok := ipAddressesBuildIPv6Config(nil, nil); ok {
		t.Error("an empty record set has no primary")
	}
}

func TestIPAddressesTruthy(t *testing.T) {
	falsy := []any{nil, false, float64(0), ""}
	for _, value := range falsy {
		if ipAddressesTruthy(value) {
			t.Errorf("%v must be falsy", value)
		}
	}
	truthy := []any{true, float64(1), "x", []any{}, map[string]any{}}
	for _, value := range truthy {
		if !ipAddressesTruthy(value) {
			t.Errorf("%v must be truthy", value)
		}
	}
}

func TestIPAddressesPrefixAllowed(t *testing.T) {
	for _, prefix := range []int{112, 120, 124} {
		if !ipAddressesPrefixAllowed(prefix) {
			t.Errorf("/%d must be allowed", prefix)
		}
	}
	for _, prefix := range []int{0, 48, 64, 96, 128, 111, 113} {
		if ipAddressesPrefixAllowed(prefix) {
			t.Errorf("/%d must not be allowed", prefix)
		}
	}
}

func TestIPAddressesISOTime(t *testing.T) {
	value := time.Date(2026, 1, 2, 3, 4, 5, 678_000_000, time.UTC)
	if got := ipAddressesISOTime(value); got != "2026-01-02T03:04:05.678Z" {
		t.Errorf("iso = %q", got)
	}
	// Milliseconds are always three digits, and the value is normalised to UTC.
	value = time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("CST", 8*3600))
	if got := ipAddressesISOTime(value); got != "2026-01-01T19:04:05.000Z" {
		t.Errorf("iso = %q", got)
	}
}

func TestIPAddressesParentInterface(t *testing.T) {
	if got := ipAddressesParentInterface(nil); got != "eth0" {
		t.Errorf("nil host = %q, want eth0", got)
	}
	empty := ""
	if got := ipAddressesParentInterface(&db.IpAddressesHost{IPv6ParentInterface: &empty}); got != "eth0" {
		t.Errorf("empty interface = %q, want eth0", got)
	}
	ens3 := "ens3"
	if got := ipAddressesParentInterface(&db.IpAddressesHost{IPv6ParentInterface: &ens3}); got != "ens3" {
		t.Errorf("interface = %q, want ens3", got)
	}
}
