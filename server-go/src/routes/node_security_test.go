// Tests for the admin node-security routes.
//
// Only paths that are rejected before any database or node access are exercised
// here: authentication, route registration, the params schema and the body
// schemas. The live behaviour (a real node, the report transaction, the recovery
// task) is covered by the dual-run conformance suite.
package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// TestNodeSecurityRoutesRegistered pins the eight mounted paths.
func TestNodeSecurityRoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterNodeSecurity(engine)

	want := []string{
		"POST /api/admin/hosts/:hostId/node-security/identity",
		"GET /api/admin/hosts/:hostId/node-security",
		"POST /api/admin/hosts/:hostId/node-security/policy",
		"POST /api/admin/hosts/:hostId/node-security/recover",
		"POST /api/admin/hosts/:hostId/node-security/lock",
		"POST /api/admin/hosts/:hostId/node-security/keys",
		"POST /api/admin/hosts/:hostId/node-security/keys/reveal",
		"POST /api/admin/hosts/:hostId/node-security/keys/unlock",
	}
	registered := map[string]bool{}
	for _, route := range engine.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	for _, path := range want {
		if !registered[path] {
			t.Fatalf("%s is not registered", path)
		}
	}
	if len(registered) != len(want) {
		t.Fatalf("registered %d routes, want %d: %v", len(registered), len(want), registered)
	}
}

// TestNodeSecurityRoutesRequireAdmin pins that the module is admin-only: no token
// answers 401 without touching the database.
func TestNodeSecurityRoutesRequireAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterNodeSecurity(engine)

	cases := []struct {
		method string
		path   string
	}{
		{"POST", "/api/admin/hosts/1/node-security/identity"},
		{"GET", "/api/admin/hosts/1/node-security"},
		{"POST", "/api/admin/hosts/1/node-security/policy"},
		{"POST", "/api/admin/hosts/1/node-security/recover"},
		{"POST", "/api/admin/hosts/1/node-security/lock"},
		{"POST", "/api/admin/hosts/1/node-security/keys"},
		{"POST", "/api/admin/hosts/1/node-security/keys/reveal"},
		{"POST", "/api/admin/hosts/1/node-security/keys/unlock"},
	}

	for _, testCase := range cases {
		request := httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(`{}`))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a token = %d, want 401", testCase.method, testCase.path, recorder.Code)
		}
	}
}

// nodeSecurityHandlerRequest drives one handler through gin with a pre-set admin
// user, so the handler never needs a token or a database for the rejected paths.
//
// path carries a concrete host id; the mounted pattern uses `:hostId`, the same
// wildcard name the rest of the /api/hosts tree uses.
func nodeSecurityHandlerRequest(t *testing.T, method, path, contentType, body string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)

	route := strings.Replace(path, "/api/admin/hosts/1/", "/api/admin/hosts/:hostId/", 1)
	engine := gin.New()
	engine.Handle(method, route, func(c *gin.Context) {
		c.Set(middleware.ContextUserKey, &lib.AccessTokenClaims{ID: 1, Role: "admin", Username: "root", Status: "active"})
		handler(c)
	})

	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func nodeSecurityAssertJSON(t *testing.T, raw, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(raw), &gotValue); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("expectation is not JSON: %v (%s)", err, want)
	}
	gotEncoded, _ := json.Marshal(gotValue)
	wantEncoded, _ := json.Marshal(wantValue)
	if string(gotEncoded) != string(wantEncoded) {
		t.Fatalf("response = %s, want %s", gotEncoded, wantEncoded)
	}
}

// TestNodeSecurityHostIDParams pins the params schema including Fastify's
// string→number coercion, which accepts "01" as 1.
func TestNodeSecurityHostIDParams(t *testing.T) {
	cases := []struct {
		hostID       string
		wantStatus   int
		wantMessage  string
		wantAccepted bool
	}{
		{"1", 0, "", true},
		{"01", 0, "", true},  // Number("01") === 1
		{"1e3", 0, "", true}, // Number("1e3") === 1000
		{"abc", 400, "must be integer", false},
		{"", 400, "must be integer", false},
		{"1.5", 400, "must be integer", false},
		{"0", 400, "must be >= 1", false},
		{"-1", 400, "must be >= 1", false},
		{"2147483648", 400, "must be <= 2147483647", false},
	}

	gin.SetMode(gin.TestMode)
	for _, testCase := range cases {
		t.Run("hostId="+testCase.hostID, func(t *testing.T) {
			engine := gin.New()
			var (
				gotID int32
				ok    bool
			)
			engine.POST("/api/admin/hosts/:hostId/node-security/lock", func(c *gin.Context) {
				nodeSecurityNoStore(c)
				gotID, ok = nodeSecurityParseHostID(c)
			})
			request := httptest.NewRequest(http.MethodPost, "/api/admin/hosts/"+testCase.hostID+"/node-security/lock", nil)
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)

			if testCase.wantAccepted {
				if !ok {
					t.Fatalf("hostId %q was rejected: %s", testCase.hostID, recorder.Body.String())
				}
				if gotID <= 0 {
					t.Fatalf("hostId %q parsed as %d", testCase.hostID, gotID)
				}
				return
			}
			if ok {
				t.Fatalf("hostId %q was accepted as %d", testCase.hostID, gotID)
			}
			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, testCase.wantStatus)
			}
			nodeSecurityAssertJSON(t, recorder.Body.String(),
				`{"error":"`+testCase.wantMessage+`","code":"VALIDATION_ERROR"}`)
			// Every response in this module is uncacheable, including this one.
			if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Pragma") != "no-cache" {
				t.Fatalf("missing cache headers: %v", recorder.Header())
			}
		})
	}
}

// TestNodeSecurityBodySchemas pins the body validation messages and ordering.
func TestNodeSecurityBodySchemas(t *testing.T) {
	const (
		validFingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		validKey         = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		identityRoute    = "/api/admin/hosts/1/node-security/identity"
		keysRoute        = "/api/admin/hosts/1/node-security/keys"
		revealRoute      = "/api/admin/hosts/1/node-security/keys/reveal"
		policyRoute      = "/api/admin/hosts/1/node-security/policy"
		recoverRoute     = "/api/admin/hosts/1/node-security/recover"
		lockRoute        = "/api/admin/hosts/1/node-security/lock"
	)

	cases := []struct {
		name        string
		route       string
		body        string
		contentType string
		status      int
		error       string
	}{
		// identity
		{"identity missing fingerprint", identityRoute, `{}`, "application/json", 400, "fingerprint is required"},
		{"identity short fingerprint", identityRoute, `{"fingerprint":"abc"}`, "application/json", 400, "fingerprint format is invalid"},
		{"identity uppercase fingerprint", identityRoute, `{"fingerprint":"` + strings.ToUpper(validFingerprint) + `"}`, "application/json", 400, "fingerprint format is invalid"},
		{"identity not an object", identityRoute, `[]`, "application/json", 400, "must be object"},
		{"identity invalid json", identityRoute, `{`, "application/json", 400, "Body is not valid JSON but content-type is set to 'application/json'"},
		{"identity empty json body", identityRoute, ``, "application/json", 400, "Body cannot be empty when content-type is set to 'application/json'"},
		{"identity unsupported media type", identityRoute, `{}`, "text/csv", 415, "Unsupported Media Type: text/csv"},

		// keys
		{"keys missing dataset", keysRoute, `{"key":"` + validKey + `"}`, "application/json", 400, "dataset is required"},
		{"keys missing key", keysRoute, `{"dataset":"tank/x"}`, "application/json", 400, "key is required"},
		{"keys dataset too short", keysRoute, `{"dataset":"a/","key":"` + validKey + `"}`, "application/json", 400, "dataset is too short"},
		{"keys dataset too long", keysRoute, `{"dataset":"` + strings.Repeat("a", 201) + `","key":"` + validKey + `"}`, "application/json", 400, "dataset is too long"},
		{"keys dataset bad pattern", keysRoute, `{"dataset":"1abc","key":"` + validKey + `"}`, "application/json", 400, "dataset format is invalid"},
		// Note: "tank/../x" passes this schema in BOTH runtimes (the pattern allows
		// dots); vault.ts rejects `..` later, after the host lookup, as a 503.
		{"keys key bad pattern", keysRoute, `{"dataset":"tank/x","key":"zz"}`, "application/json", 400, "key format is invalid"},
		{"keys dataset coerced from number", keysRoute, `{"dataset":12,"key":"` + validKey + `"}`, "application/json", 400, "dataset is too short"},

		// keys/reveal (dataset only)
		{"reveal missing dataset", revealRoute, `{}`, "application/json", 400, "dataset is required"},
		{"reveal short dataset", revealRoute, `{"dataset":"ab"}`, "application/json", 400, "dataset is too short"},

		// policy
		{"policy missing enabled", policyRoute, `{"revision":1}`, "application/json", 400, "enabled is required"},
		{"policy missing revision", policyRoute, `{"enabled":true}`, "application/json", 400, "revision is required"},
		{"policy enabled not boolean", policyRoute, `{"enabled":"yes","revision":1}`, "application/json", 400, "must be boolean"},
		{"policy negative revision", policyRoute, `{"enabled":true,"revision":-1}`, "application/json", 400, "must be >= 0"},
		{"policy revision too large", policyRoute, `{"enabled":true,"revision":9007199254740992}`, "application/json", 400, "must be <= 9007199254740991"},
		{"policy revision fractional", policyRoute, `{"enabled":true,"revision":1.5}`, "application/json", 400, "must be integer"},

		// recover
		{"recover missing revision", recoverRoute, `{}`, "application/json", 400, "revision is required"},

		// lock has an empty body schema
		{"lock not an object", lockRoute, `"x"`, "application/json", 400, "must be object"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var handler gin.HandlerFunc
			switch testCase.route {
			case identityRoute:
				handler = nodeSecurityIdentityHandler
			case keysRoute:
				handler = nodeSecurityKeysHandler
			case revealRoute:
				handler = func(c *gin.Context) { nodeSecurityKeyActionHandler(c, "reveal") }
			case policyRoute:
				handler = func(c *gin.Context) { nodeSecurityActionHandler(c, "policy") }
			case recoverRoute:
				handler = func(c *gin.Context) { nodeSecurityActionHandler(c, "recover") }
			default:
				handler = func(c *gin.Context) { nodeSecurityActionHandler(c, "lock") }
			}

			recorder := nodeSecurityHandlerRequest(t, http.MethodPost,
				testCase.route, testCase.contentType, testCase.body, handler)
			if recorder.Code != testCase.status {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, testCase.status, recorder.Body.String())
			}

			var decoded map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, recorder.Body.String())
			}
			if decoded["error"] != testCase.error {
				t.Fatalf("error = %#v, want %q (%s)", decoded["error"], testCase.error, recorder.Body.String())
			}
			if testCase.status == 400 && testCase.error != "Body is not valid JSON but content-type is set to 'application/json'" &&
				testCase.error != "Body cannot be empty when content-type is set to 'application/json'" {
				if decoded["code"] != "VALIDATION_ERROR" {
					t.Fatalf("code = %#v, want VALIDATION_ERROR (%s)", decoded["code"], recorder.Body.String())
				}
			}
			if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Pragma") != "no-cache" {
				t.Fatalf("missing cache headers on a validation failure")
			}
		})
	}
}

// TestNodeSecurityDatasetOrdering documents why this module has its own body
// validator: AJV evaluates minLength before pattern, so a value that is both too
// short and malformed reports "too short". Verified against ajv@8.17.1 compiled
// with Fastify's default options (coerceTypes:'array', removeAdditional:true,
// allErrors:false).
func TestNodeSecurityDatasetOrdering(t *testing.T) {
	value := map[string]any{"dataset": "a/"}
	_, message := nodeSecurityValidateBody(value, []string{"dataset"}, nodeSecurityDatasetFields)
	if message != "dataset is too short" {
		t.Fatalf("message = %q, want dataset is too short", message)
	}
}

// TestNodeSecurityLockBodyAcceptsAnything pins the empty lock schema: any object
// validates, and the forwarded payload is empty (additionalProperties:false).
func TestNodeSecurityLockBodyAcceptsAnything(t *testing.T) {
	body, message := nodeSecurityValidateBody(
		map[string]any{"revision": float64(3), "enabled": true},
		nil,
		nil,
	)
	if message != "" {
		t.Fatalf("message = %q, want none", message)
	}
	payload := nodeSecurityPayload(body, nil)
	if len(payload) != 0 {
		t.Fatalf("payload = %v, want {}", payload)
	}
	if encoded, _ := json.Marshal(payload); string(encoded) != "{}" {
		t.Fatalf("payload JSON = %s, want {}", encoded)
	}
}

// TestNodeSecurityPolicyPayload pins that only declared properties are forwarded.
func TestNodeSecurityPolicyPayload(t *testing.T) {
	body, message := nodeSecurityValidateBody(
		map[string]any{"enabled": true, "revision": float64(4), "unknown": "dropped"},
		[]string{"enabled", "revision"},
		nodeSecurityPolicyFields,
	)
	if message != "" {
		t.Fatalf("message = %q", message)
	}
	payload := nodeSecurityPayload(body, nodeSecurityPolicyFields)
	if len(payload) != 2 || payload["enabled"] != true || payload["revision"] != float64(4) {
		t.Fatalf("payload = %v", payload)
	}
}
