package routes

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// proxySitesTestInstance builds the narrow instance projection.
func proxySitesTestInstance(siteLimit *int32, packageID *int32) *db.ProxySitesInstanceRow {
	return &db.ProxySitesInstanceRow{
		ID:        1,
		UserID:    2,
		HostID:    3,
		PackageID: packageID,
		SiteLimit: siteLimit,
	}
}

// TestProxySitesSiteLimit pins `instance.site_limit ?? pkg?.site_limit ?? 10`.
// site_limit === 0 means "unlimited" and must NOT fall through to the package.
func TestProxySitesSiteLimit(t *testing.T) {
	int32Ptr := func(value int32) *int32 { return &value }

	unlimited := int32(0)
	if got := proxySitesSiteLimit(proxySitesTestInstance(&unlimited, int32Ptr(20)), &db.ProxySitesPackageRow{SiteLimit: 20}); got != 0 {
		t.Fatalf("site_limit=0 = %d, want 0 (unlimited must not inherit the package)", got)
	}

	instanceLimit := int32(5)
	if got := proxySitesSiteLimit(proxySitesTestInstance(&instanceLimit, int32Ptr(20)), &db.ProxySitesPackageRow{SiteLimit: 20}); got != 5 {
		t.Fatalf("site_limit=5 = %d, want 5", got)
	}

	if got := proxySitesSiteLimit(proxySitesTestInstance(nil, int32Ptr(20)), &db.ProxySitesPackageRow{SiteLimit: 20}); got != 20 {
		t.Fatalf("package fallback = %d, want 20", got)
	}

	if got := proxySitesSiteLimit(proxySitesTestInstance(nil, nil), nil); got != proxySitesDefaultSiteLimit {
		t.Fatalf("default = %d, want %d", got, proxySitesDefaultSiteLimit)
	}

	// A package id that no longer resolves behaves exactly like `pkg?.site_limit`
	// with pkg === null.
	if got := proxySitesSiteLimit(proxySitesTestInstance(nil, int32Ptr(99)), nil); got != proxySitesDefaultSiteLimit {
		t.Fatalf("missing package = %d, want %d", got, proxySitesDefaultSiteLimit)
	}
}

// TestProxySitesCreateBodyValidation pins the POST schema: required order,
// min/max length, integer bounds and the useDefaults behaviour of httpsEnabled.
func TestProxySitesCreateBodyValidation(t *testing.T) {
	decode := func(raw string) any {
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return value
	}

	cases := []struct {
		name    string
		body    string
		message string
	}{
		{"missing targetPort", `{"domain":"example.com"}`, "targetPort is required"},
		{"missing domain", `{"targetPort":80}`, "domain is required"},
		{"domain too short", `{"domain":"a.b","targetPort":80}`, "domain is too short"},
		{"domain not a string", `{"domain":{"x":1},"targetPort":80}`, "must be string"},
		{"port zero", `{"domain":"example.com","targetPort":0}`, "must be >= 1"},
		{"port too large", `{"domain":"example.com","targetPort":65536}`, "must be <= 65535"},
		{"port not integer", `{"domain":"example.com","targetPort":1.5}`, "must be integer"},
		{"port not numeric", `{"domain":"example.com","targetPort":"abc"}`, "must be integer"},
		{"remark too long", `{"domain":"example.com","targetPort":80,"remark":"` + proxySitesRepeat("x", 101) + `"}`, "remark is too long"},
		{"https not boolean", `{"domain":"example.com","targetPort":80,"httpsEnabled":"yes"}`, "must be boolean"},
		{"body not an object", `[]`, "must be object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, message := validateAJVObject(decode(tc.body), []string{"domain", "targetPort"}, proxySitesCreateFields)
			if message != tc.message {
				t.Fatalf("message = %q, want %q", message, tc.message)
			}
		})
	}

	// A well-formed body must pass and expose the coerced values.
	body, message := validateAJVObject(decode(`{"domain":"example.com","targetPort":"8080","httpsEnabled":false,"remark":" note "}`),
		[]string{"domain", "targetPort"}, proxySitesCreateFields)
	if message != "" {
		t.Fatalf("unexpected validation message %q", message)
	}
	if port, ok := body.Values["targetPort"].(float64); !ok || port != 8080 {
		t.Fatalf("targetPort coercion = %#v", body.Values["targetPort"])
	}
	if proxySitesHTTPSEnabledDefault(body, true) {
		t.Fatalf("explicit httpsEnabled=false was overridden by the default")
	}
	if remark, _ := body.stringValue("remark"); remark != " note " {
		t.Fatalf("remark = %q, want the untrimmed schema value", remark)
	}

	// useDefaults: an absent httpsEnabled takes the default, an explicit false wins.
	absentBody, message := validateAJVObject(decode(`{"domain":"example.com","targetPort":80}`),
		[]string{"domain", "targetPort"}, proxySitesCreateFields)
	if message != "" {
		t.Fatalf("unexpected validation message %q", message)
	}
	if !proxySitesHTTPSEnabledDefault(absentBody, true) {
		t.Fatalf("absent httpsEnabled did not take the default true")
	}
	if proxySitesHTTPSEnabledDefault(absentBody, false) {
		t.Fatalf("explicit fallback false was ignored")
	}
}

// TestProxySitesUpdateBodyValidation pins the PATCH schema, which has no required
// fields and no default for httpsEnabled.
func TestProxySitesUpdateBodyValidation(t *testing.T) {
	decode := func(raw string) any {
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return value
	}

	if _, message := validateAJVObject(decode(`{}`), nil, proxySitesUpdateFields); message != "" {
		t.Fatalf("empty object message = %q", message)
	}
	if _, message := validateAJVObject(decode(`{"targetPort":0}`), nil, proxySitesUpdateFields); message != "must be >= 1" {
		t.Fatalf("port zero message = %q", message)
	}
	if _, message := validateAJVObject(decode(`{"targetPort":70000}`), nil, proxySitesUpdateFields); message != "must be <= 65535" {
		t.Fatalf("port maximum message = %q", message)
	}
	if _, message := validateAJVObject(decode(`{"remark":"`+proxySitesRepeat("y", 101)+`"}`), nil, proxySitesUpdateFields); message != "remark is too long" {
		t.Fatalf("remark message = %q", message)
	}

	// Absence of httpsEnabled must stay distinguishable from false.
	body, message := validateAJVObject(decode(`{"httpsEnabled":false}`), nil, proxySitesUpdateFields)
	if message != "" {
		t.Fatalf("unexpected message %q", message)
	}
	if !body.Present["httpsEnabled"] {
		t.Fatalf("httpsEnabled=false was dropped instead of being marked present")
	}
	if _, present := body.Values["targetPort"]; present {
		t.Fatalf("absent targetPort leaked into the values")
	}
}

func proxySitesRepeat(value string, count int) string {
	result := make([]byte, 0, len(value)*count)
	for index := 0; index < count; index++ {
		result = append(result, value...)
	}
	return string(result)
}

// TestProxySitesTrimmedRemark pins `remark.trim() || null`.
func TestProxySitesTrimmedRemark(t *testing.T) {
	if got := proxySitesTrimmedRemark("  "); got != nil {
		t.Fatalf("whitespace-only remark = %q, want nil", *got)
	}
	if got := proxySitesTrimmedRemark(""); got != nil {
		t.Fatalf("empty remark = %q, want nil", *got)
	}
	got := proxySitesTrimmedRemark("  hello  ")
	if got == nil || *got != "hello" {
		t.Fatalf("trimmed remark = %#v", got)
	}
}

// TestProxySitesOptionalStringEqual pins the JS `!==` comparison used by the
// only-remark / no-change branches: nil equals nil, nil never equals a value.
func TestProxySitesOptionalStringEqual(t *testing.T) {
	stringPtr := func(value string) *string { return &value }

	if !proxySitesOptionalStringEqual(nil, nil) {
		t.Fatalf("nil vs nil must be equal")
	}
	if proxySitesOptionalStringEqual(nil, stringPtr("")) {
		t.Fatalf("nil must not equal \"\"")
	}
	if proxySitesOptionalStringEqual(stringPtr(""), nil) {
		t.Fatalf("\"\" must not equal nil")
	}
	if !proxySitesOptionalStringEqual(stringPtr("a"), stringPtr("a")) {
		t.Fatalf("equal values must compare equal")
	}
	if proxySitesOptionalStringEqual(stringPtr("a"), stringPtr("b")) {
		t.Fatalf("different values compared equal")
	}
}

// TestProxySitesFirstNonEmpty pins the `a || b` address chain.
func TestProxySitesFirstNonEmpty(t *testing.T) {
	stringPtr := func(value string) *string { return &value }

	if got := proxySitesFirstNonEmpty(nil, nil); got != nil {
		t.Fatalf("nil chain = %#v", got)
	}
	if got := proxySitesFirstNonEmpty(stringPtr(""), stringPtr("")); got != nil {
		t.Fatalf("empty strings must be falsy")
	}
	if got := proxySitesFirstNonEmpty(stringPtr(""), stringPtr("203.0.113.10")); got == nil || *got != "203.0.113.10" {
		t.Fatalf("fallback = %#v", got)
	}
	if got := proxySitesFirstNonEmpty(stringPtr("198.51.100.1"), stringPtr("203.0.113.10")); got == nil || *got != "198.51.100.1" {
		t.Fatalf("nat_public_ip must win = %#v", got)
	}
}

// TestProxySitesCertificateFailure pins the branch order of the TS error analysis.
func TestProxySitesCertificateFailure(t *testing.T) {
	cases := []struct {
		message string
		status  string
		hint    string
	}{
		{"Unable to resolve hostname (ENOTFOUND)", "dns_error", "DNS 未解析，请检查域名是否正确指向服务器"},
		{"getaddrinfo ENOTFOUND example.com", "dns_error", "DNS 未解析，请检查域名是否正确指向服务器"},
		{"connect ECONNREFUSED 203.0.113.10:443", "connection_refused", "连接被拒绝，请检查服务器 443 端口是否开放"},
		{"连接超时", "timeout", "连接超时，请检查网络连接"},
		{"socket timeout", "timeout", "连接超时，请检查网络连接"},
		// timeout is checked before handshake, exactly like the TS branch order.
		{"TLS handshake timeout", "timeout", "连接超时，请检查网络连接"},
		{"tls: first record does not look like a TLS handshake", "cert_pending", "证书正在申请中，请稍后重试"},
		{"SSL routines: wrong version number", "cert_pending", "证书正在申请中，请稍后重试"},
		{"x509: certificate signed by unknown authority", "cert_pending", "证书正在申请中，请稍后重试"},
		{"self signed certificate in certificate chain", "cert_pending", "证书正在申请中，请稍后重试"},
		{"read ECONNRESET", "cert_pending", "证书可能正在申请中，请稍后重试"},
		{"something unexpected", "error", ""},
	}
	for _, tc := range cases {
		status, hint := proxySitesCertificateFailure(errSprintf(tc.message))
		if status != tc.status || hint != tc.hint {
			t.Fatalf("failure(%q) = (%q,%q), want (%q,%q)", tc.message, status, hint, tc.status, tc.hint)
		}
	}
}

// TestProxySitesNormalizeNetError pins the Go -> Node error translation the
// certificate classifier depends on.
func TestProxySitesNormalizeNetError(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	normalized := proxySitesNormalizeNetError(refused, "203.0.113.10")
	if normalized.Error() != "connect ECONNREFUSED 203.0.113.10:443" {
		t.Fatalf("refused message = %q", normalized.Error())
	}

	reset := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	if got := proxySitesNormalizeNetError(reset, "203.0.113.10").Error(); got != "read ECONNRESET" {
		t.Fatalf("reset message = %q", got)
	}

	timeout := &net.DNSError{IsTimeout: true, Name: "example.com"}
	if got := proxySitesNormalizeNetError(timeout, "203.0.113.10").Error(); got != "连接超时" {
		t.Fatalf("timeout message = %q", got)
	}

	deadline := proxySitesNormalizeNetError(context.DeadlineExceeded, "203.0.113.10")
	if deadline.Error() != "连接超时" {
		t.Fatalf("deadline message = %q", deadline.Error())
	}

	other := context.Canceled
	if got := proxySitesNormalizeNetError(other, "203.0.113.10"); got != other {
		t.Fatalf("unrelated error must pass through, got %v", got)
	}
}

// errSprintf builds a plain error without importing fmt twice in the test file.
func errSprintf(message string) error { return &proxySitesTestError{message} }

type proxySitesTestError struct{ message string }

func (e *proxySitesTestError) Error() string { return e.message }

// TestProxySitesISOTime pins the wire timestamp shape.
func TestProxySitesISOTime(t *testing.T) {
	value := time.Date(2026, 1, 2, 3, 4, 5, 678_000_000, time.UTC)
	if got := proxySitesISOTime(value); got != "2026-01-02T03:04:05.678Z" {
		t.Fatalf("iso = %q", got)
	}
	// Non-UTC values are rendered in UTC, like Date#toISOString.
	zone := time.FixedZone("UTC+8", 8*3600)
	if got := proxySitesISOTime(time.Date(2026, 1, 2, 11, 4, 5, 0, zone)); got != "2026-01-02T03:04:05.000Z" {
		t.Fatalf("zoned iso = %q", got)
	}
}

// TestProxySitesRouteID pins the strict positive-id rule plus the int4 bound:
// 0, negative, leading-zero, non-numeric and beyond-int4 ids all answer 400
// INVALID_ID, never a bare strconv error or a wrapped database error.
func TestProxySitesRouteID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	accepted := map[string]int32{"1": 1, "42": 42, "2147483647": 2147483647}
	for raw, want := range accepted {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Params = gin.Params{{Key: "id", Value: raw}}
		got, ok := proxySitesRouteID(context, "id")
		if !ok || got != want {
			t.Fatalf("proxySitesRouteID(%q) = (%d,%v), want (%d,true)", raw, got, ok, want)
		}
	}

	for _, raw := range []string{"", "0", "-1", "01", "1.5", "abc", " 1", "2147483648", "9007199254740991"} {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Params = gin.Params{{Key: "id", Value: raw}}
		if _, ok := proxySitesRouteID(context, "id"); ok {
			t.Fatalf("proxySitesRouteID(%q) accepted an invalid id", raw)
		}
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status for %q = %d, want 400", raw, recorder.Code)
		}
		if recorder.Body.String() != `{"error":"Invalid ID","code":"INVALID_ID"}` {
			t.Fatalf("body for %q = %s", raw, recorder.Body.String())
		}
	}
}

// TestProxySitesRoutes pins the mounted path/method set. The TypeScript registers
// the module with prefix '/api' and declares no route at the prefix root, so
// there must be exactly eight routes and no trailing-slash duplicates.
func TestProxySitesRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterProxySites(router)

	expected := map[string]bool{
		"GET /api/instances/:id/sites":                     false,
		"POST /api/instances/:id/sites":                    false,
		"DELETE /api/instances/:id/sites/:siteId":          false,
		"POST /api/instances/:id/sites/:siteId/check-dns":  false,
		"POST /api/instances/:id/sites/:siteId/refresh":    false,
		"PATCH /api/instances/:id/sites/:siteId":           false,
		"POST /api/instances/:id/sites/:siteId/toggle":     false,
		"GET /api/instances/:id/sites/:siteId/certificate": false,
	}
	routes := router.Routes()
	if len(routes) != len(expected) {
		t.Fatalf("registered %d routes, want %d: %#v", len(routes), len(expected), routes)
	}
	for _, route := range routes {
		key := route.Method + " " + route.Path
		if _, ok := expected[key]; !ok {
			t.Fatalf("unexpected route %q", key)
		}
		expected[key] = true
	}
	for key, seen := range expected {
		if !seen {
			t.Fatalf("missing route %q", key)
		}
	}
}

// TestProxySitesSuspendedGuard pins the suspended/expired error codes.
func TestProxySitesSuspendedGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	expired := "expired"
	if !proxySitesRespondSuspended(context, string(models.InstanceStatusSuspended), &expired) {
		t.Fatalf("suspended instance was not rejected")
	}
	if recorder.Code != http.StatusForbidden ||
		recorder.Body.String() != `{"error":"Instance is suspended due to expiration, please renew to unsuspend","code":"INSTANCE_SUSPENDED_EXPIRED"}` {
		t.Fatalf("expired body = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	context, _ = gin.CreateTestContext(recorder)
	other := "manual"
	if !proxySitesRespondSuspended(context, string(models.InstanceStatusSuspended), &other) {
		t.Fatalf("suspended instance was not rejected")
	}
	if recorder.Code != http.StatusForbidden ||
		recorder.Body.String() != `{"error":"Instance is suspended and cannot be started","code":"INSTANCE_SUSPENDED"}` {
		t.Fatalf("suspended body = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	context, _ = gin.CreateTestContext(recorder)
	if proxySitesRespondSuspended(context, string(models.InstanceStatusRunning), nil) {
		t.Fatalf("running instance was rejected")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("running wrote a response: %d", recorder.Code)
	}
}

// TestProxySitesUpdateFieldPresence pins AJV's null -> "" coercion for the PATCH
// remark field and the `trim() || null` normalisation that follows it.
func TestProxySitesUpdateFieldPresence(t *testing.T) {
	var value any
	if err := json.Unmarshal([]byte(`{"remark":null}`), &value); err != nil {
		t.Fatalf("decode: %v", err)
	}
	body, message := validateAJVObject(value, nil, proxySitesUpdateFields)
	if message != "" {
		t.Fatalf("unexpected message %q", message)
	}
	raw, present := body.stringValue("remark")
	if !present {
		t.Fatalf("null remark must be coerced to a present empty string")
	}
	if raw != "" {
		t.Fatalf("null remark coerced to %q", raw)
	}
	if got := proxySitesTrimmedRemark(raw); got != nil {
		t.Fatalf("null remark must normalize to nil, got %#v", got)
	}
}

// TestProxySitesFormatHelpers pins the string interpolation used in log content.
func TestProxySitesFormatHelpers(t *testing.T) {
	if proxySitesFormatBool(true) != "true" || proxySitesFormatBool(false) != "false" {
		t.Fatalf("bool formatting mismatch")
	}
	if proxySitesFormatInt32(80) != "80" || proxySitesFormatInt32(0) != "0" {
		t.Fatalf("int formatting mismatch")
	}
	if proxySitesFormatPort(443) != "443" {
		t.Fatalf("port formatting mismatch")
	}
}

// TestProxySitesContainsString pins Array.prototype.includes semantics.
func TestProxySitesContainsString(t *testing.T) {
	values := []string{"203.0.113.10", "2001:db8::1"}
	if !proxySitesContainsString(values, "2001:db8::1") {
		t.Fatalf("exact match not found")
	}
	if proxySitesContainsString(values, "203.0.113.1") {
		t.Fatalf("prefix must not match")
	}
	if proxySitesContainsString(nil, "") {
		t.Fatalf("nil slice must not match")
	}
}

// TestProxySitesCaddyHostRef pins the port adaptation: caddy_port 8444 is the
// retired default and must be normalised to 2019 by resolveCaddyAdminPort, while
// any other port is passed through unchanged.
func TestProxySitesCaddyHostRef(t *testing.T) {
	ref := proxySitesCaddyHostRef(db.ProxySitesHostRef{ID: 7, CaddyEnabled: true, CaddyPort: 8444})
	if ref.ID != 7 || ref.CaddyEnabled == nil || !*ref.CaddyEnabled || ref.CaddyPort == nil || *ref.CaddyPort != 8444 {
		t.Fatalf("ref = %#v", ref)
	}
	if got := lib.ResolveCaddyAdminPort(*ref.CaddyPort); got != 2019 {
		t.Fatalf("resolved port for 8444 = %d, want 2019", got)
	}

	custom := proxySitesCaddyHostRef(db.ProxySitesHostRef{ID: 8, CaddyEnabled: true, CaddyPort: 2020})
	if got := lib.ResolveCaddyAdminPort(*custom.CaddyPort); got != 2020 {
		t.Fatalf("resolved port for 2020 = %d, want 2020", got)
	}

	disabled := proxySitesCaddyHostRef(db.ProxySitesHostRef{ID: 9, CaddyEnabled: false, CaddyPort: 2019})
	if disabled.CaddyEnabled == nil || *disabled.CaddyEnabled {
		t.Fatalf("disabled flag lost: %#v", disabled)
	}
}
