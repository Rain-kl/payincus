// Unit tests for the instances route port.
//
// The tests are pure on purpose: DB behaviour (advisory locks, transaction rollback,
// conformance) is covered by test/conformance/requests-instances.json, while the
// behaviour that needs a live Incus node is deliberately excluded from parity
// (GO_MIGRATION.md "已知待办": the two stacks produce different network error text).
//
// What is pinned here:
//   - the 34-route surface and its wildcard name (`:id`), which is shared with the
//     snapshots / backups / instance-billing / instance-destroy modules;
//   - anonymous requests answer 401 for every route (the Web JWT surface);
//   - the strict route-id parser (400 INVALID_ID) and the 404 split;
//   - the AJV body validators used by the write routes;
//   - the presentation overlay that rewrites running/stopped/error on unavailable
//     hosts (and the matching SQL filter);
//   - the compensation path: the `creating -> error` gate makes it idempotent and
//     the resource/IP/refund side effects only run when the gate is claimed;
//   - the create-config projection (io_limit_mode, bandwidth override, swap only for
//     containers, port-limit fallback).
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// instancesTestEngine builds a router with only this module registered.
func instancesTestEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterInstances(engine)
	return engine
}

// TestInstancesRouteList pins the exact 34-route surface.
func TestInstancesRouteList(t *testing.T) {
	engine := instancesTestEngine(t)

	want := map[string]bool{
		"GET /api/instances":                                    false,
		"PATCH /api/instances/:id/order":                        false,
		"GET /api/instances/available-hosts":                    false,
		"POST /api/instances":                                   false,
		"GET /api/instances/:id":                                false,
		"GET /api/instances/:id/password":                       false,
		"GET /api/instances/:id/stats":                          false,
		"POST /api/instances/:id/start":                         false,
		"POST /api/instances/:id/stop":                          false,
		"POST /api/instances/:id/restart":                       false,
		"POST /api/instances/:id/suspend":                       false,
		"POST /api/instances/:id/unsuspend":                     false,
		"POST /api/instances/:id/sync-status":                   false,
		"GET /api/instances/:id/change-host-options":            false,
		"POST /api/instances/:id/change-host":                   false,
		"POST /api/instances/:id/rebuild":                       false,
		"POST /api/instances/:id/recreate":                      false,
		"DELETE /api/instances/:id":                             false,
		"POST /api/instances/:id/ports":                         false,
		"DELETE /api/instances/:id/ports/:portId":               false,
		"PATCH /api/instances/:id/quota":                        false,
		"PATCH /api/instances/:id/config":                       false,
		"POST /api/instances/:id/clone":                         false,
		"PATCH /api/instances/:id/rename":                       false,
		"GET /api/instances/:id/config":                         false,
		"POST /api/instances/:id/swap/enable":                   false,
		"POST /api/instances/:id/swap/disable":                  false,
		"POST /api/instances/:id/boost-processes":               false,
		"GET /api/instances/:id/task":                           false,
		"GET /api/instances/tasks/:taskId":                      false,
		"DELETE /api/instances/tasks/:taskId":                   false,
		"GET /api/instances/:id/cloud-init-status":              false,
		"POST /api/instances/:id/cloud-init-manual-placeholder": true, // sentinel, removed below
		"POST /api/instances/:id/reassign-ipv6":                 false,
	}
	// The cloud-init manual-complete route is the 33rd entry; keep the map literal
	// readable by declaring it separately.
	delete(want, "POST /api/instances/:id/cloud-init-manual-placeholder")
	want["POST /api/instances/:id/cloud-init-status/manual-complete"] = false

	seen := map[string]bool{}
	for _, route := range engine.Routes() {
		if !strings.HasPrefix(route.Path, "/api/instances") {
			continue
		}
		key := route.Method + " " + route.Path
		if _, expected := want[key]; !expected {
			t.Errorf("unexpected route %s", key)
			continue
		}
		seen[key] = true
	}
	if len(want) != 34 {
		t.Fatalf("fixture lists %d routes, expected 34", len(want))
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("missing route %s", key)
		}
	}
	if len(seen) != 34 {
		t.Errorf("registered %d instance routes, want 34", len(seen))
	}
}

// TestInstancesRoutesRequireAuth pins the 401 branch: every route authenticates
// with the Web JWT surface, so an anonymous request must never reach a handler.
func TestInstancesRoutesRequireAuth(t *testing.T) {
	engine := instancesTestEngine(t)
	for _, route := range engine.Routes() {
		if !strings.HasPrefix(route.Path, "/api/instances") {
			continue
		}
		path := strings.NewReplacer(":id", "1", ":portId", "1", ":taskId", "1").Replace(route.Path)
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(route.Method, path, strings.NewReader("{}"))
		request.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token = %d, want 401 (body: %s)",
				route.Method, path, recorder.Code, recorder.Body.String())
		}
	}
}

// TestInstancesParseRouteID pins the strict `^[1-9]\d*$` parser used by every route.
func TestInstancesParseRouteID(t *testing.T) {
	for _, tc := range []struct {
		value string
		id    int32
		valid bool
	}{
		{"1", 1, true},
		{"960001", 960001, true},
		{"2147483647", 2147483647, true},
		{"2147483648", 0, true}, // syntactically valid, cannot exist in int4
		{"99999999999999999999", 0, true},
		{"0", 0, false},
		{"01", 0, false},
		{"-1", 0, false},
		{"1.0", 0, false},
		{"abc", 0, false},
		{"", 0, false},
	} {
		id, valid := instancesParseRouteID(tc.value)
		if id != tc.id || valid != tc.valid {
			t.Errorf("instancesParseRouteID(%q) = (%d,%v), want (%d,%v)", tc.value, id, valid, tc.id, tc.valid)
		}
	}
}

// TestInstancesServiceStatusOverlay pins instanceServiceStatus.
func TestInstancesServiceStatusOverlay(t *testing.T) {
	available := &hostsHostServiceStatus{Status: "online", Available: true}
	offline := &hostsHostServiceStatus{Status: "offline", Available: false}
	pending := &hostsHostServiceStatus{Status: "online", Available: true, PendingInstances: []string{"u1-abc"}}

	for _, tc := range []struct {
		name    string
		status  string
		host    *hostsHostServiceStatus
		incusID string
		want    string
	}{
		{"running-on-available", "running", available, "x", "running"},
		{"running-on-offline", "running", offline, "x", "error"},
		{"stopped-on-nil-host", "stopped", nil, "x", "error"},
		{"error-on-offline", "error", offline, "x", "error"},
		{"suspended-never-rewritten", "suspended", offline, "x", "suspended"},
		{"creating-never-rewritten", "creating", offline, "x", "creating"},
		{"pending-instance", "running", pending, "u1-abc", "error"},
		{"not-pending-instance", "running", pending, "u1-def", "running"},
		{"empty-name-pending", "stopped", pending, "", "stopped"},
	} {
		if got := instancesServiceStatus(tc.status, tc.host, tc.incusID); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestInstancesServiceStatusFilterSQL pins the SQL shape of the presentation filter.
func TestInstancesServiceStatusFilterSQL(t *testing.T) {
	statuses := map[int32]hostsHostServiceStatus{
		2: {Status: "online", Available: true, PendingInstances: []string{"a", "b"}},
		1: {Status: "offline", Available: false},
	}

	fragment, args := instancesServiceStatusFilterSQL("running", statuses)
	// Placeholder order: status, offline host id, then the pending host id and its
	// two pending-instance names.
	wantArgs := []any{"running", int32(1), int32(2), "a", "b"}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for index := range wantArgs {
		if args[index] != wantArgs[index] {
			t.Errorf("args[%d] = %v, want %v (all args: %v)", index, args[index], wantArgs[index], args)
		}
	}
	if !strings.Contains(fragment, "NOT (") || !strings.Contains(fragment, "i.host_id = ?") {
		t.Errorf("running filter = %q", fragment)
	}

	errorFragment, errorArgs := instancesServiceStatusFilterSQL("error", statuses)
	if !strings.Contains(errorFragment, "i.status = 'error' OR") {
		t.Errorf("error filter = %q", errorFragment)
	}
	if len(errorArgs) != 4 || errorArgs[0] != int32(1) || errorArgs[1] != int32(2) {
		t.Errorf("error args = %v, want the two unavailable clauses (no status prefix)", errorArgs)
	}

	emptyFragment, emptyArgs := instancesServiceStatusFilterSQL("running", map[int32]hostsHostServiceStatus{})
	if emptyFragment != "i.status = ?" || len(emptyArgs) != 1 {
		t.Errorf("empty-host filter = %q %v", emptyFragment, emptyArgs)
	}
}

// TestInstancesCreateBodyValidation pins the Fastify body schema of POST /.
func TestInstancesCreateBodyValidation(t *testing.T) {
	fields := []ajvFieldSpec{
		instancesStringField("name", 64),
		instancesIntegerField("packageId", math.NaN(), math.NaN()),
		instancesStringField("image", -1),
		instancesIntegerField("cpu", 15, 10000),
		instancesIntegerField("memory", 128, 524288),
		instancesIntegerField("disk", 512, 104857600),
		instancesIntegerField("hostId", 1, math.NaN()),
		instancesIntegerField("sshKeyId", math.NaN(), math.NaN()),
		instancesStringField("sshKey", -1),
		instancesStringField("password", -1),
		instancesIntegerField("planId", 1, math.NaN()),
		instancesStringField("idempotencyKey", 128),
		instancesStringField("turnstileToken", 2048),
		instancesStringField("promoCode", 32),
		instancesBooleanField("autoRemotePort"),
	}
	required := []string{"packageId", "image", "hostId"}

	if _, message := validateAJVObject([]any{}, required, fields); message != "must be object" {
		t.Errorf("non-object message = %q", message)
	}
	if _, message := validateAJVObject(map[string]any{"image": "images:debian/12", "hostId": 1}, required, fields); message != "packageId is required" {
		t.Errorf("missing required message = %q", message)
	}
	coerced, message := validateAJVObject(map[string]any{
		"packageId": "12", "image": "images:debian/12", "hostId": "7", "cpu": "15", "autoRemotePort": "true",
	}, required, fields)
	if message != "" {
		t.Fatalf("valid body rejected: %s", message)
	}
	if instancesCoercedNumber(coerced, "packageId") != 12 || instancesCoercedNumber(coerced, "hostId") != 7 {
		t.Errorf("string→integer coercion failed: %#v", coerced.Values)
	}
	if value, _ := coerced.Values["autoRemotePort"].(bool); !value {
		t.Errorf("string→boolean coercion failed: %#v", coerced.Values["autoRemotePort"])
	}
	if _, message := validateAJVObject(map[string]any{"packageId": 1, "image": "i", "hostId": 1, "cpu": 14}, required, fields); message == "" {
		t.Error("cpu below minimum must fail")
	}
	if _, message := validateAJVObject(map[string]any{"packageId": 1, "image": "i", "hostId": 1, "name": strings.Repeat("x", 65)}, required, fields); message != "name is too long" {
		t.Errorf("name maxLength message = %q", message)
	}
	if _, message := validateAJVObject(map[string]any{"packageId": 1, "image": "i", "hostId": 1, "cpu": 15.5}, required, fields); message != "must be integer" {
		t.Errorf("fractional cpu message = %q", message)
	}
}

// TestInstancesCustomInitArrayValidation pins the array field (AJV has no spec kind
// for it in ajvFieldSpec, so instancesValidateIntArray carries the semantics).
func TestInstancesCustomInitArrayValidation(t *testing.T) {
	if values, ok := instancesValidateIntArray(nil, 20); !ok || values != nil {
		t.Errorf("nil must be valid/empty")
	}
	if values, ok := instancesValidateIntArray([]any{float64(1), "2"}, 20); !ok || len(values) != 2 || values[1] != 2 {
		t.Errorf("array coercion = %v ok=%v", values, ok)
	}
	if _, ok := instancesValidateIntArray([]any{1.5}, 20); ok {
		t.Error("fractional item must fail")
	}
	tooMany := make([]any, 21)
	for index := range tooMany {
		tooMany[index] = float64(index + 1)
	}
	if _, ok := instancesValidateIntArray(tooMany, 20); ok {
		t.Error("21 items must fail maxItems=20")
	}
}

// TestInstancesQuotaAndRenameSchemas pins the write-route schemas.
func TestInstancesQuotaAndRenameSchemas(t *testing.T) {
	quotaFields := []ajvFieldSpec{
		{Name: "portLimit", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 1000},
		{Name: "snapshotLimit", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 1000},
		{Name: "backupLimit", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 1000},
		{Name: "siteLimit", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: 1000},
	}
	// Decode through JSON so numbers are float64 exactly like the request path.
	var quotaBody any
	if err := json.Unmarshal([]byte(`{"portLimit":null,"snapshotLimit":0,"siteLimit":"0"}`), &quotaBody); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	values, message := validateAJVObject(quotaBody, nil, quotaFields)
	if message != "" {
		t.Fatalf("quota body rejected: %s", message)
	}
	if limit, present := instancesAJVNullableInt32(values, "portLimit"); !present || limit != nil {
		t.Errorf("portLimit null lost: present=%v value=%v", present, limit)
	}
	if limit, present := instancesAJVNullableInt32(values, "snapshotLimit"); !present || limit == nil || *limit != 0 {
		t.Errorf("snapshotLimit 0 lost: %v", limit)
	}
	var zeroPort any
	_ = json.Unmarshal([]byte(`{"portLimit":0}`), &zeroPort)
	if _, message := validateAJVObject(zeroPort, nil, quotaFields); message != "must be >= 1" {
		t.Errorf("portLimit 0 message = %q, want must be >= 1", message)
	}
	var fractional any
	_ = json.Unmarshal([]byte(`{"backupLimit":1.5}`), &fractional)
	if _, message := validateAJVObject(fractional, nil, quotaFields); message != "must be integer,null" {
		t.Errorf("fractional backupLimit message = %q", message)
	}

	nameValues, message := validateAJVObject(map[string]any{"name": "ok"}, []string{"name"},
		[]ajvFieldSpec{instancesStringField("name", 100)})
	if message != "" {
		t.Fatalf("rename body rejected: %s", message)
	}
	if instancesCoercedString(nameValues, "name") != "ok" {
		t.Errorf("rename name lost")
	}
	if _, message := validateAJVObject(map[string]any{}, []string{"name"},
		[]ajvFieldSpec{instancesStringField("name", 100)}); message != "name is required" {
		t.Errorf("rename missing-name message = %q", message)
	}
}

// TestInstancesHostCompatibility pins the container/vm/both matrix.
func TestInstancesHostCompatibility(t *testing.T) {
	container := "container"
	vm := "vm"
	both := "both"
	var none *string
	for _, tc := range []struct {
		host *string
		pkg  string
		want bool
	}{
		{&container, "container", true},
		{&container, "vm", false},
		{&vm, "vm", true},
		{&vm, "container", false},
		{&both, "container", true},
		{&both, "vm", true},
		{none, "container", true},
		{none, "vm", false},
	} {
		if got := instancesHostCompatibleWithPackageType(tc.host, tc.pkg); got != tc.want {
			t.Errorf("host=%v pkg=%s: got %v, want %v", tc.host, tc.pkg, got, tc.want)
		}
	}
}

// TestInstancesAutoName pins the generated-name shape.
func TestInstancesAutoName(t *testing.T) {
	country := "US"
	name := instancesGenerateAutoInstanceName(42, &country)
	parts := strings.Split(name, "-")
	if len(parts) != 3 {
		t.Fatalf("name %q is not region-tail-timestamp-random", name)
	}
	if parts[0] != "us042" {
		t.Errorf("region prefix = %q, want us042", parts[0])
	}
	if len(parts[1]) != 14 {
		t.Errorf("timestamp = %q, want 14 digits", parts[1])
	}
	if len(parts[2]) != 4 {
		t.Errorf("random suffix = %q, want 4 chars", parts[2])
	}
	if got := instancesNameRegionPrefix(nil); got != "incus" {
		t.Errorf("nil country prefix = %q", got)
	}
	if got := instancesNameRegionPrefix(instancesStringPtr("!!")); got != "incus" {
		t.Errorf("empty-after-normalization prefix = %q", got)
	}
	if got := instancesNameRegionPrefix(instancesStringPtr("averylongcountry")); got != "averyl" {
		t.Errorf("long prefix = %q", got)
	}
}

// TestInstancesCompensationIdempotentGate proves the `creating -> error` gate: a
// second run must not touch resources, IPs or money.
func TestInstancesCompensationIdempotentGate(t *testing.T) {
	original := instancesCompensation
	t.Cleanup(func() { instancesCompensation = original })

	rollbackCalls := 0
	releaseCalls := 0
	refundCalls := 0
	instancesCompensation = instancesCompensationDeps{
		ClaimCreatingToError: func(context.Context, int32) (bool, error) { return false, nil },
		RollbackResources: func(context.Context, db.RollbackResourcesOptions) error {
			rollbackCalls++
			return nil
		},
		ReleasePublicIPv4: func(context.Context, int32) error {
			releaseCalls++
			return nil
		},
		CompensatePurchase: func(context.Context, int32, int32, int32) (instancesPurchaseCompensation, error) {
			refundCalls++
			return instancesPurchaseCompensation{Refunded: true, RefundAmount: 10}, nil
		},
	}

	outcome := instancesCompensateFailedCreation(context.Background(), instancesCompensationInput{
		InstanceID: 1, HostID: 1, UserID: 1,
		Resources:   instancesResources{CPU: 15, Memory: 128, Disk: 512},
		NetworkMode: "nat", PortLimit: 20,
	})
	if outcome.Claimed {
		t.Error("unclaimed compensation must report Claimed=false")
	}
	if outcome.RefundStatus != "already_handled" {
		t.Errorf("refund status = %q, want already_handled", outcome.RefundStatus)
	}
	if rollbackCalls != 0 || releaseCalls != 0 || refundCalls != 0 {
		t.Errorf("side effects after unclaimed gate: rollback=%d release=%d refund=%d",
			rollbackCalls, releaseCalls, refundCalls)
	}
}

// TestInstancesCompensationRollsBackQuotaAndIPv4 proves the claimed path: host
// resources roll back with the NAT port count, the public IPv4 is released, and the
// purchase compensation status is mapped.
func TestInstancesCompensationRollsBackQuotaAndIPv4(t *testing.T) {
	original := instancesCompensation
	t.Cleanup(func() { instancesCompensation = original })

	var captured db.RollbackResourcesOptions
	releaseCalls := 0
	instancesCompensation = instancesCompensationDeps{
		ClaimCreatingToError: func(context.Context, int32) (bool, error) { return true, nil },
		RollbackResources: func(_ context.Context, options db.RollbackResourcesOptions) error {
			captured = options
			return nil
		},
		ReleasePublicIPv4: func(context.Context, int32) error {
			releaseCalls++
			return nil
		},
		CompensatePurchase: func(context.Context, int32, int32, int32) (instancesPurchaseCompensation, error) {
			return instancesPurchaseCompensation{Refunded: true, RefundAmount: 30, Reason: ""}, nil
		},
	}

	outcome := instancesCompensateFailedCreation(context.Background(), instancesCompensationInput{
		InstanceID: 9, HostID: 3, UserID: 5,
		Resources:   instancesResources{CPU: 100, Memory: 2048, Disk: 10240},
		NetworkMode: "nat", PortLimit: 20,
	})
	if !outcome.Claimed || outcome.RefundStatus != "refunded" || outcome.RefundAmount != 30 {
		t.Errorf("outcome = %#v", outcome)
	}
	if captured.HostID != 3 || captured.CPU != 100 || captured.Memory != 2048 || captured.Disk != 10240 {
		t.Errorf("rollback resources = %#v", captured)
	}
	if captured.PortCount != 20 {
		t.Errorf("NAT rollback port count = %d, want 20", captured.PortCount)
	}
	if releaseCalls != 1 {
		t.Errorf("IPv4 release calls = %d, want 1", releaseCalls)
	}

	captured = db.RollbackResourcesOptions{}
	instancesCompensateFailedCreation(context.Background(), instancesCompensationInput{
		InstanceID: 10, HostID: 3, UserID: 5,
		Resources:   instancesResources{CPU: 15, Memory: 128, Disk: 512},
		NetworkMode: "ipv6_only", PortLimit: 20,
	})
	if captured.PortCount != 0 {
		t.Errorf("port count for ipv6_only = %d, want 0", captured.PortCount)
	}

	// Refund status mapping.
	for _, tc := range []struct {
		compensation instancesPurchaseCompensation
		want         string
	}{
		{instancesPurchaseCompensation{Refunded: false, Reason: "not_paid_purchase"}, "not_required"},
		{instancesPurchaseCompensation{Refunded: false, Reason: "already_refunded"}, "already_refunded"},
		{instancesPurchaseCompensation{Refunded: false, Reason: "invalid_purchase_amount"}, "not_completed"},
		{instancesPurchaseCompensation{Refunded: true, RefundAmount: 1}, "refunded"},
	} {
		instancesCompensation.CompensatePurchase = func(context.Context, int32, int32, int32) (instancesPurchaseCompensation, error) {
			return tc.compensation, nil
		}
		outcome := instancesCompensateFailedCreation(context.Background(), instancesCompensationInput{
			InstanceID: 11, HostID: 3, UserID: 5, NetworkMode: "nat",
		})
		if outcome.RefundStatus != tc.want {
			t.Errorf("reason %q → status %q, want %q", tc.compensation.Reason, outcome.RefundStatus, tc.want)
		}
	}
}

// TestInstancesCompensationSurvivesPurchaseError pins the failed-refund branch.
func TestInstancesCompensationSurvivesPurchaseError(t *testing.T) {
	original := instancesCompensation
	t.Cleanup(func() { instancesCompensation = original })

	instancesCompensation = instancesCompensationDeps{
		ClaimCreatingToError: func(context.Context, int32) (bool, error) { return true, nil },
		RollbackResources:    func(context.Context, db.RollbackResourcesOptions) error { return errors.New("rollback boom") },
		ReleasePublicIPv4:    func(context.Context, int32) error { return errors.New("ip boom") },
		CompensatePurchase: func(context.Context, int32, int32, int32) (instancesPurchaseCompensation, error) {
			return instancesPurchaseCompensation{}, errors.New("refund boom")
		},
	}
	outcome := instancesCompensateFailedCreation(context.Background(), instancesCompensationInput{
		InstanceID: 12, HostID: 1, UserID: 1, NetworkMode: "nat",
	})
	if outcome.RefundStatus != "failed" || !outcome.Claimed || outcome.RefundError != "refund boom" {
		t.Errorf("outcome = %#v", outcome)
	}
}

// TestInstancesCreateConfigProjection pins the createConfig literal: io_limit_mode
// filters the IO limits, the plan bandwidth overrides the package values, swap is
// only passed for containers, and the port limit falls back to 20.
func TestInstancesCreateConfigProjection(t *testing.T) {
	pkg := &db.PackageDetailRow{
		ID: 1, Name: "pkg", InstanceType: "container",
		PortLimit: 0, Nested: 1, Privileged: 0,
		IOLimitMode: "throughput",
		LimitsRead:  "100MB", LimitsWrite: "100MB",
		LimitsReadIops: 500, LimitsWriteIops: 500,
		LimitsIngress: "300Mbit", LimitsEgress: "300Mbit",
		LimitsProcesses: 500, LimitsCPUPriority: 10,
		BootAutostart: true, BootAutostartPriority: 20,
		BootAutostartDelay: 15, BootHostShutdownTimeout: 30,
	}
	plan := instancesCreatePlan{
		Package: pkg, Plan: &models.PackagePlan{SwapSize: 512},
		CPU: 15, Memory: 128, Disk: 512, InstanceType: "container",
		NetworkMode: "nat", IncusID: "u1-abcdefgh", Image: "images:debian/12",
		MetaData: lib.IncusConfigMetaData{SSHPort: 22},
	}
	planBandwidth := "100Mbit"
	provisioned := instancesProvisioned{
		IPv4: instancesStringPtr("10.10.0.5"), IPv6: instancesStringPtr("2001:db8::5"),
		StoragePool:   instancesStringPtr("default"),
		ConfigPayload: lib.IncusConfigPayload{UserUserData: "#cloud-config"},
	}
	config := instancesBuildCreateConfig(plan, provisioned, instancesBoolPtr(false))

	if config.PortLimit != 20 {
		t.Errorf("port limit fallback = %d, want 20", config.PortLimit)
	}
	if config.LimitsRead == nil || *config.LimitsRead != "100MB" {
		t.Errorf("throughput mode must pass limitsRead: %v", config.LimitsRead)
	}
	if config.LimitsReadIops != nil {
		t.Errorf("throughput mode must not pass IOPS: %v", config.LimitsReadIops)
	}
	if config.SwapSize == nil || *config.SwapSize != 512 {
		t.Errorf("container swap size = %v", config.SwapSize)
	}
	if config.SSHPort == nil || *config.SSHPort != 22 {
		t.Errorf("ssh port = %v", config.SSHPort)
	}
	if config.IPv4Address == nil || *config.IPv4Address != "10.10.0.5" {
		t.Errorf("ipv4 = %v", config.IPv4Address)
	}

	plan.PlanBandwidthLimit = &planBandwidth
	config = instancesBuildCreateConfig(plan, provisioned, nil)
	if config.LimitsIngress == nil || *config.LimitsIngress != "100Mbit" ||
		config.LimitsEgress == nil || *config.LimitsEgress != "100Mbit" {
		t.Errorf("plan bandwidth override = %v/%v", config.LimitsIngress, config.LimitsEgress)
	}

	plan.InstanceType = "vm"
	plan.Package.IOLimitMode = "iops"
	config = instancesBuildCreateConfig(plan, provisioned, nil)
	if config.SwapSize != nil {
		t.Errorf("VM must not receive panel swap: %v", config.SwapSize)
	}
	if config.LimitsReadIops == nil || *config.LimitsReadIops != 500 {
		t.Errorf("iops mode must pass limitsReadIops: %v", config.LimitsReadIops)
	}
	if config.LimitsRead != nil {
		t.Errorf("iops mode must not pass throughput read: %v", config.LimitsRead)
	}
}

// TestInstancesTrafficMultiplierHelpers pins the effective-limit formatting.
func TestInstancesTrafficMultiplierHelpers(t *testing.T) {
	limit := int64(1 << 40)
	if got := instancesApplyTrafficMultiplier(&limit, 2); got == nil || *got != "2199023255552" {
		t.Errorf("2x multiplier = %v", got)
	}
	if got := instancesApplyTrafficMultiplier(nil, 2); got != nil {
		t.Errorf("nil base must stay nil, got %v", got)
	}
	if got := instancesBigIntStringToInt64(nil); got != nil {
		t.Errorf("nil string must stay nil")
	}
	if got := instancesBigIntStringToInt64(instancesStringPtr("123")); got == nil || *got != 123 {
		t.Errorf("numeric string = %v", got)
	}
	if got := instancesBigIntStringToInt64(instancesStringPtr("x")); got != nil {
		t.Errorf("garbage must be nil, got %v", got)
	}
}

// TestInstancesCreateAbortPayload pins the abort envelope used by the create path.
func TestInstancesCreateAbortPayload(t *testing.T) {
	abort := instancesAbortWith(http.StatusServiceUnavailable, lib.ErrHostResourcesInsufficient, "boom")
	if abort.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d", abort.Status)
	}
	payload, ok := abort.Payload.(lib.APIError)
	if !ok {
		t.Fatalf("payload type %T", abort.Payload)
	}
	if payload.Code != lib.ErrHostResourcesInsufficient || payload.Details != "boom" {
		t.Errorf("payload = %#v", payload)
	}
	if payload.Error != lib.ErrorMessages[lib.ErrHostResourcesInsufficient] {
		t.Errorf("message not taken from the generated catalogue: %q", payload.Error)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["code"] != string(lib.ErrHostResourcesInsufficient) {
		t.Errorf("json code = %v", decoded["code"])
	}
}

// TestInstancesIsUniqueViolation pins the P2002 → 23505 mapping.
func TestInstancesIsUniqueViolation(t *testing.T) {
	if instancesIsUniqueViolation(nil) {
		t.Error("nil must not be a unique violation")
	}
	if instancesIsUniqueViolation(errors.New("boom")) {
		t.Error("plain error must not be a unique violation")
	}
	if !instancesIsUniqueViolation(errors.New("duplicate key value violates unique constraint \"instances_idempotency_key_key\"")) {
		t.Error("duplicate-key text must be detected")
	}
}

// TestInstancesRouteMethodsAreUnique guards against a duplicate registration that
// would panic inside Gin at init time (belt and braces for the route-list test).
func TestInstancesRouteMethodsAreUnique(t *testing.T) {
	engine := instancesTestEngine(t)
	seen := map[string]int{}
	for _, route := range engine.Routes() {
		seen[route.Method+" "+route.Path]++
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if seen[key] != 1 {
			t.Errorf("route %s registered %d times", key, seen[key])
		}
	}
}
