// Tests for the hosts route module (server/src/routes/hosts.ts, 7987 lines,
// 65 Fastify route declarations).
//
// The suite covers the parts that can be proven without a database:
//
//   - route-id parsing (strict `^[1-9]\d*$`, int32 bound) and the body parsers
//     the module uses before touching the database;
//   - the AJV body-schema semantics of every schema this module declares
//     (required / type / enum / minLength / maxLength / minimum / maximum /
//     minItems / maxItems), with the exact messages app.ts renders;
//   - the public-vs-authenticated route split (anonymous requests must reach the
//     public handlers and get 401 from the JWT ones);
//   - the one-time agent-log ticket: single use, bound to its host, expiring.
//
// Everything that needs rows (hosts listed, 403 for a non-owner, 409 for an
// active task, Incus failures) is covered by the dual-run conformance file
// test/conformance/requests-hosts.json, which logs in as a real admin AND a real
// non-admin user against the parity database.
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

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// ==================== route id parsing ====================

func TestHostsParsePositiveRouteID(t *testing.T) {
	cases := []struct {
		value string
		want  int32
		ok    bool
	}{
		{"1", 1, true},
		{"900001", 900001, true},
		{"2147483647", math.MaxInt32, true},
		{"0", 0, false},                      // POSITIVE_ROUTE_ID_PATTERN rejects 0
		{"01", 0, false},                     // leading zero
		{"-1", 0, false},                     // negative
		{"+1", 0, false},                     // sign
		{"abc", 0, false},                    // not a number
		{"1.5", 0, false},                    // not an integer
		{" 1", 0, false},                     // whitespace
		{"2147483648", 0, false},             // > int32 (Prisma Int4)
		{"9007199254740993", 0, false},       // > Number.MAX_SAFE_INTEGER
		{"9999999999999999999999", 0, false}, // far out of range
	}
	for _, tc := range cases {
		got, ok := hostsParsePositiveRouteID(tc.value)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("hostsParsePositiveRouteID(%q) = (%d,%t), want (%d,%t)", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

func TestHostsParseIntegerBody(t *testing.T) {
	if value, ok := hostsParseIntegerBody(float64(5), 1, 10); !ok || value != 5 {
		t.Fatalf("expected 5, got (%d,%t)", value, ok)
	}
	if _, ok := hostsParseIntegerBody(float64(0), 1, 10); ok {
		t.Fatal("0 must be below the minimum")
	}
	if _, ok := hostsParseIntegerBody(float64(11), 1, 10); ok {
		t.Fatal("11 must be above the maximum")
	}
	if _, ok := hostsParseIntegerBody(1.5, 1, 10); ok {
		t.Fatal("a fractional number is not an integer")
	}
	if _, ok := hostsParseIntegerBody("5", 1, 10); ok {
		t.Fatal("a string is not a number")
	}
	if _, ok := hostsParseOptionalIntegerBody(nil, 1, 10); !ok {
		t.Fatal("nil must parse as undefined")
	}
}

func TestHostsParsePositiveIntegerArrayBody(t *testing.T) {
	valid, ok := hostsParsePositiveIntegerArrayBody([]any{float64(3), float64(1)}, 5)
	if !ok || len(valid) != 2 || valid[0] != 3 || valid[1] != 1 {
		t.Fatalf("unexpected parse result: %v %t", valid, ok)
	}
	for _, tc := range []struct {
		name string
		body any
		max  int
	}{
		{"empty", []any{}, 5},
		{"duplicate", []any{float64(1), float64(1)}, 5},
		{"zero", []any{float64(0)}, 5},
		{"fraction", []any{1.5}, 5},
		{"string", []any{"1"}, 5},
		{"too many", []any{float64(1), float64(2), float64(3)}, 2},
		{"not array", "abc", 5},
	} {
		if _, ok := hostsParsePositiveIntegerArrayBody(tc.body, tc.max); ok {
			t.Fatalf("%s must be rejected", tc.name)
		}
	}
}

func TestHostsParsePageSizes(t *testing.T) {
	size, ok := hostsParseHostListPageSize("500")
	if !ok || size == nil || *size != hostsListMaxPageSize {
		t.Fatalf("pageSize must clamp to %d, got %v %t", hostsListMaxPageSize, size, ok)
	}
	if size, ok := hostsParseHostListPageSize(""); !ok || size != nil {
		t.Fatalf("empty pageSize must be undefined, got %v %t", size, ok)
	}
	if _, ok := hostsParseHostListPageSize("0"); ok {
		t.Fatal("pageSize=0 must be rejected")
	}
	audit, ok := hostsParseAuditHistoryPageSize("500")
	if !ok || audit == nil || *audit != hostsAuditHistoryMaxPageSize {
		t.Fatalf("audit pageSize must clamp to %d", hostsAuditHistoryMaxPageSize)
	}
}

// ==================== AJV schemas ====================

// hostsTestEngine mounts one handler with fabricated admin claims (no auth, no DB).
func hostsTestEngine(method, path string, handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(middleware.ContextUserKey, &lib.AccessTokenClaims{
			ID: 1, Username: "admin", Role: "admin", Status: "active",
			IssuedAt: time.Now().Unix(),
		})
	})
	engine.Handle(method, path, handler)
	return engine
}

func hostsTestRequest(engine *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func hostsTestValidationError(t *testing.T, recorder *httptest.ResponseRecorder, want string) {
	t.Helper()
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	if payload.Error != want {
		t.Fatalf("error = %q, want %q", payload.Error, want)
	}
	if payload.Code != "VALIDATION_ERROR" {
		t.Fatalf("code = %q, want VALIDATION_ERROR", payload.Code)
	}
}

// TestHostsCreateSchema pins POST /api/hosts.
func TestHostsCreateSchema(t *testing.T) {
	engine := hostsTestEngine(http.MethodPost, "/api/hosts", hostsCreateHandler)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty", `{}`, "name is required"},
		{"missing url", `{"name":"abc"}`, "url is required"},
		{"short name", `{"name":"a","url":"https://x"}`, "name is too short"},
		{"country too long", `{"name":"abc","url":"https://x","countryCode":"usa"}`, "countryCode is too long"},
		{"country too short", `{"name":"abc","url":"https://x","countryCode":"u"}`, "countryCode is too short"},
		{"bad storage driver", `{"name":"abc","url":"https://x","storageDriver":"ceph"}`, "must be equal to one of the allowed values"},
		{"bad storage type", `{"name":"abc","url":"https://x","storageType":"raid"}`, "must be equal to one of the allowed values"},
		{"bad instance type", `{"name":"abc","url":"https://x","instanceType":"docker"}`, "must be equal to one of the allowed values"},
		{"storage size not integer", `{"name":"abc","url":"https://x","storageSize":1.5}`, "must be integer"},
		{"tags not array", `{"name":"abc","url":"https://x","tags":"a"}`, "must be array"},
		{"tag not string", `{"name":"abc","url":"https://x","tags":[1]}`, "must be string"},
		{"nat config not object", `{"name":"abc","url":"https://x","natConfig":"a"}`, "must be object"},
		{"nat port not integer", `{"name":"abc","url":"https://x","natConfig":{"portRangeStart":"a"}}`, "must be integer"},
		{"body is not object", `[]`, "must be object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts", tc.body), tc.want)
		})
	}
}

// TestHostsUpdateSchema pins PATCH /api/hosts/:id.
func TestHostsUpdateSchema(t *testing.T) {
	engine := hostsTestEngine(http.MethodPatch, "/api/hosts/:hostId", hostsUpdateHandler)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"country too long", `{"countryCode":"usa"}`, "countryCode is too long"},
		{"traffic reset day above max", `{"trafficResetDay":29}`, "must be <= 28"},
		{"traffic reset day below min", `{"trafficResetDay":0}`, "must be >= 1"},
		{"bad instance type", `{"instanceType":"docker"}`, "must be equal to one of the allowed values"},
		{"announcement too long", `{"announcement":"` + strings.Repeat("a", 1001) + `"}`, "announcement is too long"},
		{"probe url too long", `{"probeUrl":"` + strings.Repeat("a", 501) + `"}`, "probeUrl is too long"},
		{"cpu allowance not integer", `{"cpuAllowanceMax":"a"}`, "must be integer"},
		{"transfer flag not boolean", `{"transferEnabled":"maybe"}`, "must be boolean"},
		{"body is not object", `"x"`, "must be object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPatch, "/api/hosts/1", tc.body), tc.want)
		})
	}
}

// TestHostsMaintenanceSchema pins POST /api/hosts/:id/maintenance.
func TestHostsMaintenanceSchema(t *testing.T) {
	engine := hostsTestEngine(http.MethodPost, "/api/hosts/:hostId/maintenance", hostsMaintenanceHandler)
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/maintenance", `{}`), "enabled is required")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/maintenance", `{"enabled":"yes"}`), "must be boolean")
}

// TestHostsInstanceIDSchemas pins the shared `instanceIds` array schema used by
// the batch-delete preview, batch delete, suspend, unsuspend, sync-status,
// gift-days and migrate routes.
func TestHostsInstanceIDSchemas(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		handler gin.HandlerFunc
		method  string
	}{
		{"batch-delete-preview", "/api/hosts/:hostId/instances/batch-delete-preview", hostsBatchDeletePreviewHandler, http.MethodPost},
		{"batch-delete", "/api/hosts/:hostId/instances/batch", hostsBatchDeleteHandler, http.MethodDelete},
		{"suspend", "/api/hosts/:hostId/instances/suspend", hostsSuspendHandler, http.MethodPost},
		{"unsuspend", "/api/hosts/:hostId/instances/unsuspend", hostsUnsuspendHandler, http.MethodPost},
		{"sync-status", "/api/hosts/:hostId/instances/sync-status", hostsSyncStatusHandler, http.MethodPost},
		{"gift-days", "/api/hosts/:hostId/instances/gift-days", hostsGiftDaysHandler, http.MethodPost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := hostsTestEngine(tc.method, tc.path, tc.handler)
			path := strings.NewReplacer(":hostId", "1").Replace(tc.path)
			hostsTestValidationError(t, hostsTestRequest(engine, tc.method, path, `{}`), "instanceIds is required")
			// gift-days also requires `days`; the other routes do not, and Fastify
			// reports the first missing required property in schema order.
			extra := ""
			if tc.name == "gift-days" {
				extra = `,"days":1`
			}
			hostsTestValidationError(t, hostsTestRequest(engine, tc.method, path, `{"instanceIds":[]`+extra+`}`), "must NOT have fewer than 1 items")
			hostsTestValidationError(t, hostsTestRequest(engine, tc.method, path, `{"instanceIds":["x"]`+extra+`}`), "must be integer")
			tooMany := make([]string, 0, 101)
			for i := 0; i < 101; i++ {
				tooMany = append(tooMany, "1")
			}
			hostsTestValidationError(t, hostsTestRequest(engine, tc.method, path,
				`{"instanceIds":[`+strings.Join(tooMany, ",")+`]`+extra+`}`), "must NOT have more than 100 items")
		})
	}
}

// TestHostsImagesSchema pins PUT /api/hosts/:id/images.
func TestHostsImagesSchema(t *testing.T) {
	engine := hostsTestEngine(http.MethodPut, "/api/hosts/:hostId/images", hostsImagesPutHandler)
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPut, "/api/hosts/1/images", `{}`), "useDefault is required")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPut, "/api/hosts/1/images", `{"useDefault":"yes"}`), "must be boolean")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPut, "/api/hosts/1/images", `{"useDefault":true,"imageIds":["x"]}`), "must be integer")
}

// TestHostsExtendAllSchema pins POST /api/hosts/:id/extend-all.
func TestHostsExtendAllSchema(t *testing.T) {
	engine := hostsTestEngine(http.MethodPost, "/api/hosts/:hostId/extend-all", hostsExtendAllHandler)
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/extend-all", `{}`), "days is required")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/extend-all", `{"days":0}`), "must be >= 1")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/extend-all", `{"days":366}`), "must be <= 365")
}

// TestHostsMigrateSchema pins POST /api/hosts/:id/instances/migrate.
func TestHostsMigrateSchema(t *testing.T) {
	engine := hostsTestEngine(http.MethodPost, "/api/hosts/:hostId/instances/migrate", hostsMigrateHandler)
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/instances/migrate", `{}`), "instanceIds is required")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/instances/migrate",
		`{"instanceIds":[1],"targetHostId":2,"targetImage":""}`), "targetImage is too short")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/instances/migrate",
		`{"instanceIds":[1],"targetHostId":"x","targetImage":"debian"}`), "must be integer")
	ids := make([]string, 0, 31)
	for i := 0; i < 31; i++ {
		ids = append(ids, "1")
	}
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/instances/migrate",
		`{"instanceIds":[`+strings.Join(ids, ",")+`],"targetHostId":2,"targetImage":"debian"}`), "must NOT have more than 30 items")
}

// TestHostsCreateForUserSchema pins POST /api/hosts/:id/instances/create-for-user.
func TestHostsCreateForUserSchema(t *testing.T) {
	engine := hostsTestEngine(http.MethodPost, "/api/hosts/:hostId/instances/create-for-user", hostsCreateForUserHandler)
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/instances/create-for-user", `{}`), "username is required")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/instances/create-for-user",
		`{"username":"ab","name":"abc","packageId":1,"image":"debian","cpu":10}`), "must be >= 15")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/instances/create-for-user",
		`{"username":"ab","name":"abc","packageId":1,"image":"debian","memory":100}`), "must be >= 128")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/instances/create-for-user",
		`{"username":"ab","name":"abc","packageId":0,"image":"debian"}`), "must be >= 1")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, "/api/hosts/1/instances/create-for-user",
		`{"username":"ab","name":"abc","packageId":1,"image":"debian","giftDays":366}`), "must be <= 365")
}

// TestHostsRenewalPriceSchema pins
// PATCH /api/hosts/:hostId/instances/:instanceId/renewal-price.
func TestHostsRenewalPriceSchema(t *testing.T) {
	engine := hostsTestEngine(http.MethodPatch, "/api/hosts/:hostId/instances/:instanceId/renewal-price", hostsRenewalPriceHandler)
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPatch, "/api/hosts/1/instances/1/renewal-price", `{}`), "newPrice is required")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPatch, "/api/hosts/1/instances/1/renewal-price", `{"newPrice":"x"}`), "must be number")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPatch, "/api/hosts/1/instances/1/renewal-price", `{"newPrice":100000}`), "must be <= 99999")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPatch, "/api/hosts/1/instances/1/renewal-price", `{"newPrice":-1}`), "must be >= 0")
}

// TestHostsStoragePoolSchema pins POST/PATCH /api/hosts/:id/storage-pools...
func TestHostsStoragePoolSchema(t *testing.T) {
	create := hostsTestEngine(http.MethodPost, "/api/hosts/:hostId/storage-pools", hostsStoragePoolCreateHandler)
	hostsTestValidationError(t, hostsTestRequest(create, http.MethodPost, "/api/hosts/1/storage-pools", `{}`), "name is required")
	hostsTestValidationError(t, hostsTestRequest(create, http.MethodPost, "/api/hosts/1/storage-pools",
		`{"name":"`+strings.Repeat("a", 65)+`"}`), "name is too long")
	hostsTestValidationError(t, hostsTestRequest(create, http.MethodPost, "/api/hosts/1/storage-pools",
		`{"name":"pool","driver":"ceph"}`), "must be equal to one of the allowed values")
	hostsTestValidationError(t, hostsTestRequest(create, http.MethodPost, "/api/hosts/1/storage-pools",
		`{"name":"pool","loopDirectory":"`+strings.Repeat("a", 256)+`"}`), "loopDirectory is too long")

	patch := hostsTestEngine(http.MethodPatch, "/api/hosts/:hostId/storage-pools/:poolName", hostsStoragePoolUpdateHandler)
	hostsTestValidationError(t, hostsTestRequest(patch, http.MethodPatch, "/api/hosts/1/storage-pools/data",
		`{"purpose":"cache"}`), "must be equal to one of the allowed values")
}

// TestHostsAuditSchemas pins the audit rule / kill-process bodies.
func TestHostsAuditSchemas(t *testing.T) {
	rules := hostsTestEngine(http.MethodPost, "/api/hosts/:hostId/ops/audit/rules", hostsAuditRuleCreateHandler)
	hostsTestValidationError(t, hostsTestRequest(rules, http.MethodPost, "/api/hosts/1/ops/audit/rules", `{}`), "name is required")
	hostsTestValidationError(t, hostsTestRequest(rules, http.MethodPost, "/api/hosts/1/ops/audit/rules",
		`{"name":"`+strings.Repeat("a", 81)+`","pattern":"x"}`), "name is too long")
	hostsTestValidationError(t, hostsTestRequest(rules, http.MethodPost, "/api/hosts/1/ops/audit/rules",
		`{"name":"rule","pattern":"`+strings.Repeat("a", 501)+`"}`), "pattern is too long")
	hostsTestValidationError(t, hostsTestRequest(rules, http.MethodPost, "/api/hosts/1/ops/audit/rules",
		`{"name":"rule","pattern":"x","severity":"critical"}`), "must be equal to one of the allowed values")

	builtin := hostsTestEngine(http.MethodPatch, "/api/hosts/:hostId/ops/audit/builtin-rules/:ruleId", hostsAuditBuiltinRuleOverrideHandler)
	hostsTestValidationError(t, hostsTestRequest(builtin, http.MethodPatch, "/api/hosts/1/ops/audit/builtin-rules/miner",
		`{"severity":"critical"}`), "must be equal to one of the allowed values")
	hostsTestValidationError(t, hostsTestRequest(builtin, http.MethodPatch, "/api/hosts/1/ops/audit/builtin-rules/miner",
		`{"matchType":"glob"}`), "must be equal to one of the allowed values")

	kill := hostsTestEngine(http.MethodPost, "/api/hosts/:hostId/ops/instances/:instanceId/audit/kill-process", hostsAuditKillProcessHandler)
	killPath := "/api/hosts/1/ops/instances/1/audit/kill-process"
	hostsTestValidationError(t, hostsTestRequest(kill, http.MethodPost, killPath, `{}`), "pid is required")
	hostsTestValidationError(t, hostsTestRequest(kill, http.MethodPost, killPath,
		`{"pid":1,"reason":"abc","confirmationText":"x"}`), "must be >= 2")
	hostsTestValidationError(t, hostsTestRequest(kill, http.MethodPost, killPath,
		`{"pid":10,"reason":"abc","confirmationText":"x","signal":"HUP"}`), "must be equal to one of the allowed values")
	hostsTestValidationError(t, hostsTestRequest(kill, http.MethodPost, killPath,
		`{"pid":10,"reason":"ab","confirmationText":"x"}`), "reason is too short")
	hostsTestValidationError(t, hostsTestRequest(kill, http.MethodPost, killPath,
		`{"pid":10,"reason":"abc","confirmationText":"x","expectedCommand":"`+strings.Repeat("a", 501)+`"}`), "expectedCommand is too long")
}

// TestHostsDangerousActionSchema pins the ops dangerous-action body.
func TestHostsDangerousActionSchema(t *testing.T) {
	engine := hostsTestEngine(http.MethodPost, "/api/hosts/:hostId/ops/instances/:instanceId/dangerous-action", hostsDangerousActionHandler)
	path := "/api/hosts/1/ops/instances/1/dangerous-action"
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, path, `{}`), "action is required")
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, path,
		`{"action":"nuke","imageAlias":"debian","confirmationText":"x","riskConfirmed":true}`), "must be equal to one of the allowed values")
	ids := make([]string, 0, 21)
	for i := 0; i < 21; i++ {
		ids = append(ids, "1")
	}
	hostsTestValidationError(t, hostsTestRequest(engine, http.MethodPost, path,
		`{"action":"rebuild","imageAlias":"debian","confirmationText":"x","riskConfirmed":true,"customInitCommandIds":[`+strings.Join(ids, ",")+`]}`),
		"must NOT have more than 20 items")
}

// ==================== auth split ====================

// TestHostsPublicVersusAuthenticatedRoutes mounts the real registration and
// proves the public endpoints are reachable anonymously while the JWT routes are
// not. `middleware.Authenticate` short-circuits with 401 before any handler runs,
// which is exactly the Fastify `onRequest` ordering.
func TestHostsPublicVersusAuthenticatedRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterHosts(engine)

	public := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/hosts/install.sh"},
		{http.MethodGet, "/api/hosts/install.sh/short"},
		{http.MethodGet, "/api/hosts/cert/short"},
		{http.MethodGet, "/api/hosts/1/agent/logs/stream"},
	}
	for _, tc := range public {
		recorder := hostsTestRequest(engine, tc.method, tc.path, "")
		if recorder.Code == http.StatusUnauthorized && strings.Contains(recorder.Body.String(), `"code":"UNAUTHORIZED"`) {
			// The stream route answers 401 from its own handler when the ticket is
			// missing; that is the one public route that is *supposed* to 401.
			if !strings.Contains(tc.path, "agent/logs/stream") {
				t.Fatalf("%s %s must not require a JWT (got 401)", tc.method, tc.path)
			}
			continue
		}
		if recorder.Code == http.StatusUnauthorized {
			t.Fatalf("%s %s must not be blocked by the JWT middleware", tc.method, tc.path)
		}
	}

	private := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/hosts"},
		{http.MethodPost, "/api/hosts"},
		{http.MethodGet, "/api/hosts/1"},
		{http.MethodPatch, "/api/hosts/1"},
		{http.MethodDelete, "/api/hosts/1"},
		{http.MethodPost, "/api/hosts/1/verify"},
		{http.MethodPost, "/api/hosts/1/maintenance"},
		{http.MethodPost, "/api/hosts/1/extend-all"},
		{http.MethodGet, "/api/hosts/1/ops/audit/rules"},
		{http.MethodPost, "/api/hosts/1/instances/create-for-user"},
		{http.MethodGet, "/api/hosts/1/storage-pools"},
	}
	for _, tc := range private {
		recorder := hostsTestRequest(engine, tc.method, tc.path, `{}`)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s must be 401 without a token, got %d (%s)", tc.method, tc.path, recorder.Code, recorder.Body.String())
		}
	}
}

// TestHostsPublicInstallTokenReverse covers the one-time install-token rejections
// that do not need the database: a short token is rejected before any lookup.
func TestHostsPublicInstallTokenReverse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterHosts(engine)

	for _, path := range []string{"/api/hosts/install.sh/short", "/api/hosts/cert/short"} {
		recorder := hostsTestRequest(engine, http.MethodGet, path, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", path, recorder.Code)
		}
		if strings.TrimSpace(recorder.Body.String()) != "# Error: Invalid token format" {
			t.Fatalf("%s body = %q", path, recorder.Body.String())
		}
	}
}

// TestHostsAgentLogStreamOneTimeTicket proves the ticket is single use, bound to
// the host it was issued for, and expiring.
func TestHostsAgentLogStreamOneTimeTicket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterHosts(engine)

	ticket := HostsAgentLogTicket.Issue(1, 900001, time.Now().Unix(), "sid")
	if ticket == "" {
		t.Fatal("issue returned an empty ticket")
	}
	if !HostsAgentLogTicket.Consume(ticket, 900001) {
		t.Fatal("first consume must succeed")
	}
	if HostsAgentLogTicket.Consume(ticket, 900001) {
		t.Fatal("a consumed ticket must never be accepted twice")
	}

	wrongHost := HostsAgentLogTicket.Issue(1, 900002, time.Now().Unix(), "sid")
	if HostsAgentLogTicket.Consume(wrongHost, 900001) {
		t.Fatal("a ticket issued for another host must be rejected")
	}

	expired := HostsAgentLogTicket.Issue(1, 900001, time.Now().Unix(), "sid")
	hostsTicketStore.Lock()
	entry := hostsTicketStore.entries[expired]
	entry.expiresAt = time.Now().UTC().Add(-time.Second)
	hostsTicketStore.entries[expired] = entry
	hostsTicketStore.Unlock()
	if HostsAgentLogTicket.Consume(expired, 900001) {
		t.Fatal("an expired ticket must be rejected")
	}

	// The stream handler maps every rejected ticket to 401 UNAUTHORIZED.
	for _, path := range []string{
		"/api/hosts/900001/agent/logs/stream",
		"/api/hosts/900001/agent/logs/stream?ticket=not-a-ticket",
		"/api/hosts/900001/agent/logs/stream?ticket=" + ticket,
	} {
		recorder := hostsTestRequest(engine, http.MethodGet, path, "")
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", path, recorder.Code)
		}
	}
	// An invalid host id is rejected before the ticket is even looked at.
	recorder := hostsTestRequest(engine, http.MethodGet, "/api/hosts/abc/agent/logs/stream?ticket=x", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d, want 400", recorder.Code)
	}
}

// TestHostsRouteInventory pins the route count and the public paths.
func TestHostsRouteInventory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterHosts(engine)

	want := map[string]bool{
		"GET /api/hosts":                                                     false,
		"POST /api/hosts":                                                    false,
		"GET /api/hosts/install.sh":                                          false,
		"GET /api/hosts/install.sh/:token":                                   false,
		"GET /api/hosts/cert/:token":                                         false,
		"GET /api/hosts/:hostId/agent/logs/stream":                           false,
		"POST /api/hosts/:hostId/extend-all":                                 false,
		"DELETE /api/hosts/:hostId/instances/batch":                          false,
		"PATCH /api/hosts/:hostId/storage-pools/:poolName":                   false,
		"POST /api/hosts/:hostId/ops/instances/:instanceId/dangerous-action": false,
	}
	hostsRoutes := 0
	for _, route := range engine.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
		if strings.HasPrefix(route.Path, "/api/hosts") {
			hostsRoutes++
		}
	}
	for key, seen := range want {
		if !seen {
			t.Fatalf("route %s is not registered", key)
		}
	}
	// 65 Fastify declarations + the `/` alias registered twice (GET and POST) = 67.
	if hostsRoutes != 67 {
		t.Fatalf("hosts route count = %d, want 67", hostsRoutes)
	}
}

// TestHostsDerivePanelURLPrivateHostHeader fallback.
func TestHostsDerivePanelURLFromHostHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/probe", func(c *gin.Context) {
		c.String(http.StatusOK, hostsDerivePanelUrl(c))
	})
	recorder := hostsTestRequest(engine, http.MethodGet, "/probe", "")
	if strings.TrimSpace(recorder.Body.String()) != "http://example.com" {
		t.Fatalf("derived panel url = %q", recorder.Body.String())
	}
}

// TestHostsInjectInstallVariable mirrors the line-anchored injection.
func TestHostsInjectInstallVariable(t *testing.T) {
	script := "#!/bin/bash\nINJECT_PANEL_URL=\"\"\nINJECT_AGENT_INSTALL_TOKEN=\"\"\n"
	injected := hostsInjectInstallVariable(script, "INJECT_PANEL_URL", `https://panel.example.com`)
	if !strings.Contains(injected, `INJECT_PANEL_URL="https://panel.example.com"`) {
		t.Fatalf("panel url not injected: %s", injected)
	}
	if !strings.Contains(injected, `INJECT_AGENT_INSTALL_TOKEN=""`) {
		t.Fatalf("other variables must stay untouched: %s", injected)
	}
	for _, tc := range []struct{ input, want string }{
		{`a"b`, `a\"b`},
		{`a$b`, `a\$b`},
		{"a\\b", "a\\\\b"}, // one backslash -> two
		{"a`b", "a\\`b"},   // backtick -> backslash + backtick
	} {
		injected := hostsInjectInstallVariable(script, "INJECT_PANEL_URL", tc.input)
		wantLine := `INJECT_PANEL_URL="` + tc.want + `"`
		if !strings.Contains(injected, wantLine) {
			t.Fatalf("injection of %q = %q, want %q", tc.input, injected, wantLine)
		}
	}
}

// TestHostsBuildNodeInstallCommand pins the install command shape.
func TestHostsBuildNodeInstallCommand(t *testing.T) {
	command := hostsBuildNodeInstallCommand("https://panel.example.com/", "ait_token")
	want := "curl -fsSL 'https://panel.example.com/api/agent/install.sh' -o incus-node-install.sh && " +
		"sudo env INCUDAL_PANEL_URL='https://panel.example.com' INCUDAL_AGENT_INSTALL_TOKEN='ait_token' " +
		"INCUS_NODE_CHANNEL='canary' bash incus-node-install.sh"
	if command != want {
		t.Fatalf("install command = %q\nwant %q", command, want)
	}
}

// TestHostsAuditRuleNormalisation pins the audit rule coercion helpers and the
// builtin table (7 rules, all regex/process|startup).
func TestHostsAuditRuleNormalisation(t *testing.T) {
	if len(hostsBuiltinAuditRules) != 7 {
		t.Fatalf("builtin audit rule count = %d, want 7", len(hostsBuiltinAuditRules))
	}
	for _, rule := range hostsBuiltinAuditRules {
		if rule.Source != "builtin" || !rule.Enabled {
			t.Fatalf("rule %s must be an enabled builtin", rule.ID)
		}
		if rule.MatchType != "regex" {
			t.Fatalf("rule %s match type = %s, want regex", rule.ID, rule.MatchType)
		}
		if err := hostsValidateAuditRegex(rule.MatchType, rule.PatternText); err != nil {
			t.Fatalf("rule %s has an invalid regex: %v", rule.ID, err)
		}
		if len(rule.TargetTypes) == 0 {
			t.Fatalf("rule %s has no target types", rule.ID)
		}
	}
	if got := hostsNormalizeAuditSeverity("critical"); got != "medium" {
		t.Fatalf("unknown severity must normalise to medium, got %s", got)
	}
	if got := hostsNormalizeAuditMatchType("glob"); got != "contains" {
		t.Fatalf("unknown match type must normalise to contains, got %s", got)
	}
	if got := hostsNormalizeAuditTargetTypes([]any{"network", "bogus"}); len(got) != 1 || got[0] != "network" {
		t.Fatalf("target types = %v, want [network]", got)
	}
	if got := hostsNormalizeAuditTargetTypes("process"); len(got) != 1 || got[0] != "process" {
		t.Fatalf("non-array target types must fall back to [process], got %v", got)
	}
	if hostsNormalizeAuditFindingTarget("bogus") != nil {
		t.Fatal("unknown finding target must normalise to null")
	}
	if err := hostsValidateAuditRegex("regex", "("); err == nil {
		t.Fatal("an invalid regex must be reported")
	}
	if err := hostsValidateAuditRegex("contains", "("); err != nil {
		t.Fatalf("non-regex match types must not compile the pattern: %v", err)
	}
}

// TestHostsFormatStoragePoolCreateError pins the operator-facing translations.
func TestHostsFormatStoragePoolCreateError(t *testing.T) {
	zfs := hostsFormatStoragePoolCreateError(errHostsString("modprobe: module zfs not found"))
	if !strings.Contains(zfs, "宿主机未加载 ZFS 内核模块") {
		t.Fatalf("zfs message = %q", zfs)
	}
	lvm := hostsFormatStoragePoolCreateError(errHostsString("Required tool 'lvm' is missing"))
	if !strings.Contains(lvm, "宿主机缺少 LVM 管理工具") {
		t.Fatalf("lvm message = %q", lvm)
	}
	plain := hostsFormatStoragePoolCreateError(errHostsString("some other failure"))
	if plain != "some other failure" {
		t.Fatalf("plain message = %q", plain)
	}
}

type hostsTestError string

func (e hostsTestError) Error() string { return string(e) }

func errHostsString(message string) error { return hostsTestError(message) }
