// Unit tests for the routes/packages.go port (server/src/routes/packages.ts).
//
// DB-backed behaviour is covered by the dual-run conformance suite
// (server-go/test/conformance/requests-packages.json). What is pinned here:
//
//   - the full route table (19 Fastify routes; the list and create routes are
//     mounted twice because prefixTrailingSlash is 'both') and its coexistence
//     with every other registered module (gin allows one wildcard name per path
//     position, so a clash would panic at startup);
//   - the public/authenticated boundary: GET /public and GET /public/regions must
//     reach the handler without Authorization, every other route must answer 401
//     before touching the database;
//   - the AJV replica semantics the TypeScript schemas depend on (required before
//     properties, type coercion, array/object sub-schemas, min/max messages);
//   - the normalisers: package name/host pools/traffic multipliers, plan required
//     fields and money rounding, share quota bounds, and the empty-update early
//     return contract;
//   - the owner/admin 403 predicate;
//   - Intl.DisplayNames region names and formatMemory, which feed the two public
//     payloads field by field.
package routes

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== registration ====================

func TestPackagesRegistersAllRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterPackages(router)

	prefix := "/api/packages"
	expected := map[string]bool{
		"GET " + prefix + "/public":                true,
		"GET " + prefix + "/public/regions":        true,
		"GET " + prefix:                            true, // prefixTrailingSlash: both
		"GET " + prefix + "/":                      true,
		"POST " + prefix:                           true,
		"POST " + prefix + "/":                     true,
		"GET " + prefix + "/my-shares":             true,
		"GET " + prefix + "/regions":               true,
		"GET " + prefix + "/:id":                   true,
		"PATCH " + prefix + "/:id":                 true,
		"DELETE " + prefix + "/:id":                true,
		"POST " + prefix + "/:id/share":            true,
		"DELETE " + prefix + "/:id/share/:userId":  true,
		"PATCH " + prefix + "/:id/shares/:shareId": true,
		"GET " + prefix + "/:id/shares":            true,
		"GET " + prefix + "/:id/hosts-detail":      true,
		"POST " + prefix + "/:id/release-quota":    true,
		"GET " + prefix + "/:id/plans":             true,
		"POST " + prefix + "/:id/plans":            true,
		"PUT " + prefix + "/:id/plans/:planId":     true,
		"DELETE " + prefix + "/:id/plans/:planId":  true,
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
}

// TestPackagesRoutesCoexistWithEveryModule registers the whole tree: gin panics
// when two modules claim the same wildcard name at the same position
// (e.g. ':id' vs ':packageId'), so this is the real guard.
func TestPackagesRoutesCoexistWithEveryModule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterAll(router)

	found := false
	for _, route := range router.Routes() {
		if route.Method == http.MethodGet && route.Path == "/api/packages/:id/plans" {
			found = true
		}
	}
	if !found {
		t.Fatal("packages routes disappeared while mounting the full tree")
	}
}

// ==================== auth boundary ====================

func TestPackagesPublicRoutesSkipAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	// A nil db.DB panics inside the handler; turning the panic into 599 proves the
	// request reached the handler instead of being stopped by authenticate.
	router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		c.AbortWithStatus(599)
	}))
	RegisterPackages(router)

	for _, path := range []string{"/api/packages/public", "/api/packages/public/regions"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		router.ServeHTTP(recorder, request)
		if recorder.Code == http.StatusUnauthorized {
			t.Errorf("%s returned 401; the route must be public", path)
		}
		if recorder.Code != 599 {
			t.Errorf("%s status = %d, want 599 (handler reached)", path, recorder.Code)
		}
	}
}

func TestPackagesProtectedRoutesRequireAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterPackages(router)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/packages"},
		{http.MethodGet, "/api/packages/"},
		{http.MethodPost, "/api/packages"},
		{http.MethodPost, "/api/packages/"},
		{http.MethodGet, "/api/packages/my-shares"},
		{http.MethodGet, "/api/packages/regions"},
		{http.MethodGet, "/api/packages/1"},
		{http.MethodPatch, "/api/packages/1"},
		{http.MethodDelete, "/api/packages/1"},
		{http.MethodPost, "/api/packages/1/share"},
		{http.MethodDelete, "/api/packages/1/share/2"},
		{http.MethodPatch, "/api/packages/1/shares/2"},
		{http.MethodGet, "/api/packages/1/shares"},
		{http.MethodGet, "/api/packages/1/hosts-detail"},
		{http.MethodPost, "/api/packages/1/release-quota"},
		{http.MethodGet, "/api/packages/1/plans"},
		{http.MethodPost, "/api/packages/1/plans"},
		{http.MethodPut, "/api/packages/1/plans/2"},
		{http.MethodDelete, "/api/packages/1/plans/2"},
	}
	body := strings.NewReader(`{"name":"x1"}`)
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(tc.method, tc.path, body)
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s status = %d, want 401", tc.method, tc.path, recorder.Code)
		}
		body = strings.NewReader(`{"name":"x1"}`)
	}

	// A syntactically valid but unsigned token must also be rejected before any
	// database access.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/packages", nil)
	request.Header.Set("Authorization", "Bearer not-a-token")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("invalid token status = %d, want 401", recorder.Code)
	}
}

// ==================== owner / admin branches ====================

func TestPackagesOwnerPredicates(t *testing.T) {
	owner := &lib.AccessTokenClaims{ID: 7, Role: "user"}
	other := &lib.AccessTokenClaims{ID: 9, Role: "user"}
	admin := &lib.AccessTokenClaims{ID: 1, Role: "admin"}

	if packagesOwnerOrAdminDenies(7, owner) {
		t.Fatal("owner must not be denied")
	}
	if !packagesOwnerOrAdminDenies(7, other) {
		t.Fatal("non-owner non-admin must be denied (403)")
	}
	if packagesOwnerOrAdminDenies(7, admin) {
		t.Fatal("admin must be allowed")
	}
	if packagesOwnerOnlyDenies(7, owner) {
		t.Fatal("owner must not be denied")
	}
	if !packagesOwnerOnlyDenies(7, other) {
		t.Fatal("non-owner must be denied on share routes")
	}
}

// ==================== schema validation (AJV replica) ====================

func decodeJSONForTest(t *testing.T, raw string) any {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("bad fixture %q: %v", raw, err)
	}
	return decoded
}

func TestPackagesCreateSchemaValidation(t *testing.T) {
	required := []string{"name", "cpuMax", "memoryMax", "diskMax"}
	fields := packagesPackageSchemaFields()

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing body fields", `{}`, "name is required"},
		{"missing cpuMax", `{"name":"x1","memoryMax":1024,"diskMax":10240}`, "cpuMax is required"},
		{"array root", `[]`, "must be object"},
		{"string root", `"x"`, "must be object"},
		{"short name", `{"name":"x","cpuMax":100,"memoryMax":1024,"diskMax":10240}`, "name is too short"},
		{"cpu not integer", `{"name":"x1","cpuMax":"abc","memoryMax":1024,"diskMax":10240}`, "must be integer"},
		{"cpu below minimum", `{"name":"x1","cpuMax":3,"memoryMax":1024,"diskMax":10240}`, "must be >= 15"},
		{"memory below minimum", `{"name":"x1","cpuMax":100,"memoryMax":100,"diskMax":10240}`, "must be >= 128"},
		{"disk below minimum", `{"name":"x1","cpuMax":100,"memoryMax":1024,"diskMax":10}`, "must be >= 512"},
		{"hostIds empty", `{"name":"x1","cpuMax":100,"memoryMax":1024,"diskMax":10240,"hostIds":[]}`, "must NOT have fewer than 1 items"},
		{"hostIds item type", `{"name":"x1","cpuMax":100,"memoryMax":1024,"diskMax":10240,"hostIds":["a"]}`, "must be integer"},
		{"bad network mode", `{"name":"x1","cpuMax":100,"memoryMax":1024,"diskMax":10240,"networkMode":"bogus"}`, "must be equal to one of the allowed values"},
		{"bad instance type", `{"name":"x1","cpuMax":100,"memoryMax":1024,"diskMax":10240,"instanceType":"lxc"}`, "must be equal to one of the allowed values"},
		{"egressProfileId zero", `{"name":"x1","cpuMax":100,"memoryMax":1024,"diskMax":10240,"egressProfileId":0}`, "must be >= 1"},
		{"gpuDeviceIds too many", `{"name":"x1","cpuMax":100,"memoryMax":1024,"diskMax":10240,"gpuDeviceIds":` + gpuIDArray(1001) + `}`, "must NOT have more than 1000 items"},
		{"trafficResetPrice too high", `{"name":"x1","cpuMax":100,"memoryMax":1024,"diskMax":10240,"trafficResetPrice":1000000}`, "must be <= 999999.99"},
		{"limitsCpuPriority too high", `{"name":"x1","cpuMax":100,"memoryMax":1024,"diskMax":10240,"limitsCpuPriority":11}`, "must be <= 10"},
		{"globalMaxInstances wrong type", `{"name":"x1","cpuMax":100,"memoryMax":1024,"diskMax":10240,"globalMaxInstances":"x"}`, "must be integer,null"},
		{"hostStoragePools bad value", `{"name":"x1","cpuMax":100,"memoryMax":1024,"diskMax":10240,"hostStoragePools":{"1":{}}}`, "must be string,null"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, message := packagesValidateSchema(decodeJSONForTest(t, tc.body), required, fields)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
		})
	}
}

func gpuIDArray(count int) string {
	parts := make([]string, count)
	for index := range parts {
		parts[index] = "1"
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func TestPackagesCreateSchemaCoercion(t *testing.T) {
	required := []string{"name", "cpuMax", "memoryMax", "diskMax"}
	fields := packagesPackageSchemaFields()

	coerced, message := packagesValidateSchema(decodeJSONForTest(t,
		`{"name":"pkg","cpuMax":"200","memoryMax":1024,"diskMax":10240,"hostIds":["3"]}`), required, fields)
	if message != "" {
		t.Fatalf("unexpected message %q", message)
	}
	if value, _ := coerced["cpuMax"].(float64); value != 200 {
		t.Fatalf("cpuMax = %#v, want 200", coerced["cpuMax"])
	}
	hostIDs, ok := coerced["hostIds"].([]any)
	if !ok || len(hostIDs) != 1 {
		t.Fatalf("hostIds = %#v", coerced["hostIds"])
	}
	if value, _ := hostIDs[0].(float64); value != 3 {
		t.Fatalf("hostIds[0] = %#v, want 3", hostIDs[0])
	}
}

func TestPackagesPatchSchemaAllowsNullMonthlyTraffic(t *testing.T) {
	fields := packagesPatchSchemaFields()
	coerced, message := packagesValidateSchema(decodeJSONForTest(t,
		`{"monthlyTrafficLimit":null}`), nil, fields)
	if message != "" {
		t.Fatalf("unexpected message %q", message)
	}
	if value, present := coerced["monthlyTrafficLimit"]; !present || value != nil {
		t.Fatalf("monthlyTrafficLimit = %#v (present=%v), want nil", value, present)
	}
}

func TestPackagesReleaseQuotaSchema(t *testing.T) {
	required := []string{"hostIds", "cpuAdd", "memoryAdd"}
	fields := packagesReleaseQuotaSchemaFields()

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing hostIds", `{"cpuAdd":1,"memoryAdd":1}`, "hostIds is required"},
		{"empty hostIds", `{"hostIds":[],"cpuAdd":1,"memoryAdd":1}`, "must NOT have fewer than 1 items"},
		{"too many hosts", `{"hostIds":` + gpuIDArray(101) + `,"cpuAdd":1,"memoryAdd":1}`, "must NOT have more than 100 items"},
		{"negative cpu", `{"hostIds":[1],"cpuAdd":-1,"memoryAdd":1}`, "must be >= 0"},
		{"cpu too big", `{"hostIds":[1],"cpuAdd":10001,"memoryAdd":1}`, "must be <= 10000"},
		{"memory too big", `{"hostIds":[1],"cpuAdd":1,"memoryAdd":1048577}`, "must be <= 1048576"},
		{"notify not boolean", `{"hostIds":[1],"cpuAdd":1,"memoryAdd":1,"notify":"x"}`, "must be boolean"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, message := packagesValidateSchema(decodeJSONForTest(t, tc.body), required, fields)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
		})
	}
}

// ==================== normalisers ====================

func TestPackagesEmptyUpdateEarlyReturn(t *testing.T) {
	coerced, message := packagesValidateSchema(map[string]any{}, nil, packagesPatchSchemaFields())
	if message != "" {
		t.Fatalf("unexpected message %q", message)
	}
	if len(coerced) != 0 {
		t.Fatalf("coerced = %#v, want empty", coerced)
	}

	input, err := packagesNormalizeInput(coerced, false)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if input.Name != nil || input.DescriptionSet || input.CPUMax != nil || input.HostIDsSet ||
		input.MonthlyTrafficSet || input.GlobalShared != nil || input.RequiredPackageSet {
		t.Fatalf("empty body produced fields: %#v", input)
	}
	if input.HostStoragePools != nil || input.HostTrafficRaw != nil {
		t.Fatal("empty body must not produce host maps")
	}
}

func TestPackagesNormalizeInputRejections(t *testing.T) {
	check := func(t *testing.T, raw, want string) {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatalf("bad fixture %q: %v", raw, err)
		}
		_, err := packagesNormalizeInput(body, false)
		if err == nil {
			t.Fatalf("expected %q", want)
		}
		if apiErr, ok := err.(packagesAPIError); ok {
			if apiErr.envelope.Details != want {
				t.Fatalf("details = %q, want %q", apiErr.envelope.Details, want)
			}
			return
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want containing %q", err.Error(), want)
		}
	}

	base := `"cpuMax":100,"memoryMax":1024,"diskMax":10240`
	check(t, `{"name":"bad<name>",`+base+`}`, "Package name contains illegal characters")
	check(t, `{"name":"good-name",`+base+`,"hostIds":[1,1]}`, "Invalid package host IDs")
	check(t, `{"name":"good-name",`+base+`,"hostIds":[0]}`, "Invalid package host IDs")
	check(t, `{"name":"good-name",`+base+`,"hostStoragePools":{"0":"pool"}}`, "hostStoragePools 包含无效宿主机 ID")
	check(t, `{"name":"good-name",`+base+`,"hostStoragePools":{"1":"bad/pool"}}`, "存储池名称无效")
	check(t, `{"name":"good-name",`+base+`,"hostTrafficMultipliers":{"1":500}}`, "流量倍率必须在 0.001-100 之间")
	check(t, `{"name":"good-name",`+base+`,"portLimit":0}`, "端口映射数 必须在 1-2147483647 之间")
	check(t, `{"name":"good-name",`+base+`,"limitsRead":"bad|value"}`, "读取限制 无效")
	check(t, `{"name":"good-name",`+base+`,"requiredPackageId":0}`, "前置套餐 必须在 1-9007199254740991 之间")
	check(t, `{"name":"good-name",`+base+`,"globalMaxInstances":7}`, "公开套餐最大实例数 必须在 1-5 之间")

	// A valid body must pass untouched.
	var valid map[string]any
	if err := json.Unmarshal([]byte(`{"name":"good-name",`+base+`}`), &valid); err != nil {
		t.Fatal(err)
	}
	if _, err := packagesNormalizeInput(valid, false); err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
}

func TestPackagesNormalizeInputEgressErrors(t *testing.T) {
	if _, _, _, err := packagesNormalizeEgress(map[string]any{"egressMode": "bogus"}); err == nil {
		t.Fatal("expected invalid egress mode error")
	} else if err.Error() != "Invalid package egress mode" {
		t.Fatalf("message = %q", err.Error())
	}
	if _, _, _, err := packagesNormalizeEgress(map[string]any{"egressProfileId": float64(0)}); err == nil {
		t.Fatal("expected invalid egress profile error")
	} else if err.Error() != "Invalid egress profile" {
		t.Fatalf("message = %q", err.Error())
	}
	mode, profileID, present, err := packagesNormalizeEgress(map[string]any{"egressMode": "direct", "egressProfileId": nil})
	if err != nil || mode == nil || *mode != "direct" || profileID != nil || !present {
		t.Fatalf("unexpected result %v %v %v %v", mode, profileID, present, err)
	}
}

func TestPackagesPlanNormalizeRequiredFields(t *testing.T) {
	required := `{"name":"plan","cpu":100,"memory":1024,"disk":10240,"portLimit":10,"snapshotLimit":1,"backupLimit":1,"siteLimit":1,"swapSize":0,"trafficLimit":"1073741824","price":1000}`
	var body map[string]any
	if err := json.Unmarshal([]byte(required), &body); err != nil {
		t.Fatal(err)
	}
	input, err := packagesNormalizePlanInput(body, true, false)
	if err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	if input.SwapSize == nil || *input.SwapSize != 0 {
		t.Fatalf("swapSize = %#v", input.SwapSize)
	}

	vmBody := map[string]any{}
	for key, value := range body {
		vmBody[key] = value
	}
	delete(vmBody, "swapSize")
	vmInput, err := packagesNormalizePlanInput(vmBody, true, true)
	if err != nil {
		t.Fatalf("vm plan without swapSize rejected: %v", err)
	}
	if vmInput.SwapSize == nil || *vmInput.SwapSize != 0 {
		t.Fatalf("vm swapSize = %#v, want 0", vmInput.SwapSize)
	}

	delete(vmBody, "name")
	if _, err := packagesNormalizePlanInput(vmBody, true, true); err == nil {
		t.Fatal("missing name must fail")
	}
}

func TestPackagesPlanNormalizeBounds(t *testing.T) {
	var valid map[string]any
	if err := json.Unmarshal([]byte(`{"name":"plan","cpu":100,"memory":1024,"disk":10240,
		"portLimit":10,"snapshotLimit":1,"backupLimit":1,"siteLimit":1,
		"swapSize":0,"trafficLimit":"1073741824","price":1000}`), &valid); err != nil {
		t.Fatal(err)
	}
	withField := func(key string, value any) map[string]any {
		copied := map[string]any{}
		for k, v := range valid {
			copied[k] = v
		}
		copied[key] = value
		return copied
	}

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"cpu too small", withField("cpu", float64(10)), "CPU 必须在 15-10000 之间"},
		{"price too large", withField("price", float64(100000000)), "价格 必须在 0-99999999 之间"},
		{"money precision", withField("trafficResetPrice", 1.234), "流量重置价格 最多支持两位小数"},
		{"sla precision", withField("slaGuarantee", 99.999), "SLA保证最多支持两位小数"},
		{"sla bounds", withField("slaGuarantee", float64(101)), "SLA保证必须在 1-100 之间"},
		{"name too long", withField("name", strings.Repeat("a", 51)), "方案名称不能超过 50 个字符"},
		{"name illegal", withField("name", "bad<name>"), "方案名称包含非法字符"},
		{"description type", withField("description", float64(1)), "方案描述无效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := packagesNormalizePlanInput(tc.body, true, false)
			if err == nil {
				t.Fatalf("expected %q", tc.want)
			}
			apiErr, ok := err.(packagesAPIError)
			if !ok || apiErr.envelope.Details != tc.want {
				t.Fatalf("error = %#v, want details %q", err, tc.want)
			}
		})
	}

	if _, err := packagesNormalizePlanInput(withField("trafficResetPrice", nil), true, false); err != nil {
		t.Fatalf("null trafficResetPrice must be accepted: %v", err)
	}
}

func TestPackagesShareInputValidation(t *testing.T) {
	if _, err := packagesNormalizeShareInput(map[string]any{}, true); err == nil {
		t.Fatal("missing friendId must fail")
	}
	if _, err := packagesNormalizeShareInput(map[string]any{"friendId": float64(1)}, true); err != nil {
		t.Fatalf("valid share rejected: %v", err)
	}
	if _, err := packagesNormalizeShareInput(map[string]any{"friendId": float64(1)}, false); err == nil {
		t.Fatal("friendId on quota update must fail")
	}

	quotaCases := []struct {
		value any
		want  string
	}{
		{0.05, "配额倍数必须在 0.1-99.9 之间"},
		{100.0, "配额倍数必须在 0.1-99.9 之间"},
		{1.25, "配额倍数最多支持一位小数"},
	}
	for _, tc := range quotaCases {
		_, err := packagesNormalizeShareInput(map[string]any{"friendId": float64(1), "quotaMultiplier": tc.value}, true)
		if err == nil {
			t.Fatalf("expected %q for %v", tc.want, tc.value)
		}
		apiErr, ok := err.(packagesAPIError)
		if !ok || apiErr.envelope.Details != tc.want {
			t.Fatalf("error = %#v, want details %q", err, tc.want)
		}
	}

	input, err := packagesNormalizeShareInput(map[string]any{
		"friendId": float64(1), "quotaMultiplier": 1.5, "maxInstances": float64(3),
	}, true)
	if err != nil {
		t.Fatalf("valid share rejected: %v", err)
	}
	if input.QuotaMultiplier == nil || *input.QuotaMultiplier != 1.5 || input.MaxInstances == nil || *input.MaxInstances != 3 {
		t.Fatalf("unexpected normalized share %#v", input)
	}
}

// ==================== traffic limit speed ====================

func TestPackagesNormalizePlanTrafficLimitSpeed(t *testing.T) {
	cases := []struct {
		name      string
		value     any
		want      string
		wantValid bool
	}{
		{"nil", nil, "", true},
		{"empty", "", "", true},
		{"zero string", "0", "", true},
		{"mbit", "100Mbit", "100Mbit", true},
		{"gbit", "1Gbit", "1000Mbit", true},
		{"fractional gbit", "1.5Gbit", "1500Mbit", true},
		{"bytes", "1048576", "1Mbit", true},
		{"bytes below one mbit", "1000000", "", true},
		{"garbage", "abc", "", false},
		{"number", float64(100), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, valid := packagesNormalizePlanTrafficLimitSpeed(tc.value)
			if valid != tc.wantValid {
				t.Fatalf("valid = %v, want %v", valid, tc.wantValid)
			}
			if !valid {
				return
			}
			if got == nil {
				if tc.want != "" {
					t.Fatalf("got nil, want %q", tc.want)
				}
				return
			}
			if *got != tc.want {
				t.Fatalf("got %q, want %q", *got, tc.want)
			}
		})
	}
}

// ==================== public helpers ====================

func TestPackagesCountryNames(t *testing.T) {
	cases := map[string]string{
		"us":      "United States",
		"hk":      "Hong Kong SAR China",
		"tw":      "Taiwan",
		"jp":      "Japan",
		"tr":      "Türkiye",
		"mk":      "North Macedonia",
		"xx":      "Unknown",
		"pc":      "Pacific Community",
		"001":     "world",
		"419":     "Latin America",
		"100":     "100",
		"zz":      "Unknown Region",
		"unknown": "UNKNOWN",
	}
	for code, want := range cases {
		if got := packagesCountryName(code); got != want {
			t.Errorf("packagesCountryName(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestPackagesFormatMemory(t *testing.T) {
	cases := map[int64]string{
		512:  "512 MB",
		1024: "1.0 GB",
		1536: "1.5 GB",
		0:    "0 MB",
	}
	for value, want := range cases {
		if got := packagesFormatMemory(value); got != want {
			t.Errorf("packagesFormatMemory(%d) = %q, want %q", value, got, want)
		}
	}
}

func TestPackagesNormalizePublicMaxInstances(t *testing.T) {
	value := func(input int32) *int32 { return &input }
	if got := packagesNormalizePublicMaxInstances(nil); got != 1 {
		t.Errorf("nil = %d, want 1", got)
	}
	if got := packagesNormalizePublicMaxInstances(value(0)); got != 1 {
		t.Errorf("0 = %d, want 1", got)
	}
	if got := packagesNormalizePublicMaxInstances(value(3)); got != 3 {
		t.Errorf("3 = %d, want 3", got)
	}
	if got := packagesNormalizePublicMaxInstances(value(6)); got != 1 {
		t.Errorf("6 = %d, want 1", got)
	}
	if packagesValidatePublicMaxInstances(value(5)) != true {
		t.Error("5 must validate")
	}
	if packagesValidatePublicMaxInstances(nil) != false {
		t.Error("nil must not validate")
	}
}

func TestPackagesLocaleCompareZH(t *testing.T) {
	if packagesLocaleCompareZH("United States", "Japan") <= 0 {
		t.Fatal("United States must sort after Japan")
	}
	if packagesLocaleCompareZH("US", "Australia") <= 0 {
		t.Fatal("case-insensitive primary order expected: Australia before US")
	}
	if packagesLocaleCompareZH("Same", "Same") != 0 {
		t.Fatal("identical names must compare equal")
	}
}

// ==================== invalid route ids short-circuit before the database ====================

func packagesInvokeHandler(t *testing.T, handler gin.HandlerFunc, method, body string, params gin.Params) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(method, "/api/packages/x", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	c.Request = request
	c.Params = params
	handler(c)
	return recorder
}

func TestPackagesInvalidRouteIDs(t *testing.T) {
	invalid := gin.Params{{Key: "id", Value: "abc"}}
	invalidZero := gin.Params{{Key: "id", Value: "0"}}
	invalidPlan := gin.Params{{Key: "id", Value: "1"}, {Key: "planId", Value: "-1"}}
	invalidShareDelete := gin.Params{{Key: "id", Value: "1"}, {Key: "userId", Value: "x"}}
	invalidShareUpdate := gin.Params{{Key: "id", Value: "1"}, {Key: "shareId", Value: "0"}}

	cases := []struct {
		name    string
		handler gin.HandlerFunc
		method  string
		body    string
		params  gin.Params
	}{
		{"detail", packagesDetail, http.MethodGet, "", invalid},
		{"detail zero", packagesDetail, http.MethodGet, "", invalidZero},
		{"update", packagesUpdate, http.MethodPatch, `{}`, invalid},
		{"delete", packagesDelete, http.MethodDelete, "", invalid},
		{"hosts-detail", packagesHostsDetail, http.MethodGet, "", invalid},
		{"release-quota", packagesReleaseQuota, http.MethodPost, `{"hostIds":[1],"cpuAdd":1,"memoryAdd":0}`, invalid},
		{"plans-list", packagesPlansList, http.MethodGet, "", invalid},
		{"plans-create", packagesPlansCreate, http.MethodPost, `{}`, invalid},
		{"plans-update", packagesPlansUpdate, http.MethodPut, `{}`, invalidPlan},
		{"plans-delete", packagesPlansDelete, http.MethodDelete, "", invalidPlan},
		{"share-create", packagesShareCreate, http.MethodPost, `{"friendId":1}`, invalid},
		{"share-delete", packagesShareDelete, http.MethodDelete, "", invalidShareDelete},
		{"share-update", packagesShareUpdate, http.MethodPatch, `{}`, invalidShareUpdate},
		{"share-list", packagesShareList, http.MethodGet, "", invalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := packagesInvokeHandler(t, tc.handler, tc.method, tc.body, tc.params)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", recorder.Code)
			}
			var payload map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("body: %v", err)
			}
			if payload["code"] != "INVALID_ID" {
				t.Fatalf("code = %#v, want INVALID_ID", payload["code"])
			}
		})
	}
}

// TestPackagesCreateSchemaErrorEnvelope pins the app-level validation envelope:
// code VALIDATION_ERROR, error = the first AJV message, no details key.
func TestPackagesCreateSchemaErrorEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/api/packages", strings.NewReader(`{"name":"x1"}`))
	request.Header.Set("Content-Type", "application/json")
	c.Request = request
	c.Set(middleware.ContextUserKey, &lib.AccessTokenClaims{ID: 1, Username: "admin", Role: "admin", Status: "active"})

	packagesCreate(c)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["code"] != "VALIDATION_ERROR" {
		t.Fatalf("code = %#v", payload["code"])
	}
	if payload["error"] != "cpuMax is required" {
		t.Fatalf("error = %#v", payload["error"])
	}
	if _, present := payload["details"]; present {
		t.Fatal("single AJV message must not emit details")
	}
}

// ==================== small format helpers ====================

func TestPackagesDecimalFormatting(t *testing.T) {
	value, err := models.NewDecimalFromString("10.50")
	if err != nil {
		t.Fatal(err)
	}
	if got := packagesDecimalText(value); got != "10.5" {
		t.Fatalf("text = %q, want 10.5 (Prisma Decimal.toString drops trailing zeros)", got)
	}
	if got := packagesDecimalNumber(value); math.Abs(got-10.5) > 1e-9 {
		t.Fatalf("number = %v, want 10.5", got)
	}
	if got := packagesNullableDecimalText(nil); got != nil {
		t.Fatalf("nil decimal = %#v, want null", got)
	}
}
