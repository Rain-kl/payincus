package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// ==================== setOperationTaskProgress / control ====================

func TestSetOperationTaskProgressAndControl(t *testing.T) {
	line := "downloading 42.5MiB/s"
	lib.SetOperationTaskProgress(1, &line)
	control := lib.OperationTaskControl(1)
	if control.Progress == nil || *control.Progress != "downloading 42.5MiB/s" {
		t.Fatalf("progress control = %+v", control)
	}
	if control.CanStop || control.Stopping {
		t.Fatalf("no live task attached, canStop/stopping must be false: %+v", control)
	}

	lib.SetOperationTaskProgress(1, nil)
	if control := lib.OperationTaskControl(1); control.Progress != nil {
		t.Fatalf("clearing progress must remove the entry: %+v", control)
	}

	long := strings.Repeat("a", 600)
	lib.SetOperationTaskProgress(2, &long)
	if control := lib.OperationTaskControl(2); control.Progress == nil || len([]rune(*control.Progress)) != 500 {
		t.Fatalf("progress must be capped at 500 runes: %+v", control)
	}
}

// ==================== stage key / node error formatting ====================

func TestTransferNodeProgressStageKey(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Downloading 42.5MiB/s", "Downloading"},
		// "12%" (plus its leading separator run) is stripped, leaving a double space,
		// exactly like the TypeScript replace chains.
		{"unpack 12% done", "unpack  done"},
		{"done", "done"},
	}
	for _, c := range cases {
		if got := transferNodeProgressStageKey(c.in); got != c.want {
			t.Fatalf("stageKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTransferFormatNodeBackupError(t *testing.T) {
	task := transferNodeTask{Status: "failed", Error: "boom"}
	if got := transferFormatNodeBackupError(task); got != "boom" {
		t.Fatalf("error-only = %q", got)
	}

	task.Error = ""
	task.Items = append(task.Items, struct {
		Name   string `json:"name,omitempty"`
		Status string `json:"status,omitempty"`
		Error  string `json:"error,omitempty"`
		File   string `json:"file,omitempty"`
		Bytes  int64  `json:"bytes,omitempty"`
	}{Name: "n1", Error: "item boom"})
	if got := transferFormatNodeBackupError(task); got != "item boom" {
		t.Fatalf("item-only = %q", got)
	}

	task.Error = "outer boom"
	if got := transferFormatNodeBackupError(task); got != "outer boom: item boom" {
		t.Fatalf("combined = %q", got)
	}

	task.Error = "outer boom item boom"
	if got := transferFormatNodeBackupError(task); got != "outer boom item boom" {
		t.Fatalf("nested duplicate must not be re-appended: %q", got)
	}
}

// ==================== parse helpers ====================

func TestTransferParseSizeToMB(t *testing.T) {
	cases := []struct {
		raw      any
		fallback int32
		want     int32
	}{
		{"512MiB", 512, 512},
		{"1GiB", 512, 1024},
		{"2G", 512, 512}, // bare "G" is not a listed unit → fallback (2GiB is)
		{"512", 512, 1},  // bare number counts as bytes
		{"", 512, 512},   // empty → fallback
		{nil, 10240, 10240},
		{"10TiB", 512, 1024 * 1024 * 10},
	}
	for _, c := range cases {
		if got := transferParseSizeToMB(c.raw, c.fallback); got != c.want {
			t.Fatalf("parseSize(%v) = %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestTransferParseCpuPercentage(t *testing.T) {
	if got := transferParseCpuPercentage(map[string]any{"limits.cpu.allowance": "50%"}); got != 50 {
		t.Fatalf("50%% = %d", got)
	}
	if got := transferParseCpuPercentage(map[string]any{"limits.cpu": "2"}); got != 200 {
		t.Fatalf("2 cores = %d", got)
	}
	if got := transferParseCpuPercentage(map[string]any{}); got != 100 {
		t.Fatalf("default = %d", got)
	}
}

func TestTransferExtractNetworkDetails(t *testing.T) {
	devices := map[string]map[string]any{
		"eth0": {"type": "nic", "ipv4.address": "10.10.0.5/24"},
		"eth1": {"type": "nic", "ipv6.address": "2001:db8::1"},
	}
	ipv4, ipv6, mode := transferExtractNetworkDetails(devices, nil)
	if ipv4 == nil || *ipv4 != "10.10.0.5" || ipv6 == nil || *ipv6 != "2001:db8::1" || mode != "nat_ipv6" {
		t.Fatalf("dual = (%v, %v, %s)", ipv4, ipv6, mode)
	}

	ipv4, ipv6, mode = transferExtractNetworkDetails(map[string]map[string]any{}, []byte(`{"network":{"eth0":{"addresses":[{"family":"inet6","scope":"global","address":"2001:db8::2"}]}}}`))
	if ipv4 != nil || ipv6 == nil || *ipv6 != "2001:db8::2" || mode != "ipv6_only" {
		t.Fatalf("state-only = (%v, %v, %s)", ipv4, ipv6, mode)
	}

	// link-scoped addresses are ignored
	ipv4, ipv6, mode = transferExtractNetworkDetails(map[string]map[string]any{}, []byte(`{"network":{"eth0":{"addresses":[{"family":"inet","scope":"link","address":"169.254.1.1"},{"family":"inet","scope":"global","address":"10.0.0.9"}]}}}`))
	if ipv4 == nil || *ipv4 != "10.0.0.9" || ipv6 != nil || mode != "nat" {
		t.Fatalf("scope filtering = (%v, %v, %s)", ipv4, ipv6, mode)
	}
}

func TestTransferExtractSnapshotFromRaw(t *testing.T) {
	raw := map[string]any{
		"type":   "virtual-machine",
		"status": "Running",
		"config": map[string]any{
			"limits.cpu.allowance": "25%",
			"limits.memory":        "1024MiB",
			"image.description":    "Debian 12",
		},
		"devices": map[string]any{
			"root": map[string]any{"type": "disk", "pool": "tank", "size": "20GiB"},
		},
	}
	metrics := transferExtractSnapshotFromRaw(raw, nil)
	if metrics.CPU != 25 || metrics.Memory != 1024 || metrics.Disk != 20480 {
		t.Fatalf("resources = %+v", metrics)
	}
	if metrics.StoragePoolName == nil || *metrics.StoragePoolName != "tank" {
		t.Fatalf("pool = %+v", metrics.StoragePoolName)
	}
	if metrics.Image != "Debian 12" || metrics.Status != "running" || metrics.InstanceType != "vm" {
		t.Fatalf("meta = %+v", metrics)
	}
	if metrics.NetworkMode != "nat" || metrics.IPv4 != nil || metrics.IPv6 != nil {
		t.Fatalf("network = %+v %+v %+v", metrics.NetworkMode, metrics.IPv4, metrics.IPv6)
	}
}

func TestTransferDecodeNodeTask(t *testing.T) {
	obj := map[string]any{"id": "abc", "status": "success", "progress": "done"}
	task := transferDecodeNodeTask(obj, "fallback")
	if task.ID != "abc" || task.Status != "success" || task.Progress != "done" {
		t.Fatalf("task = %+v", task)
	}

	task = transferDecodeNodeTask(map[string]any{}, "fallback")
	if task.ID != "fallback" || task.Status != "running" {
		t.Fatalf("defaulted task = %+v", task)
	}
}

func TestTransferTaskIDGeneration(t *testing.T) {
	id := transferGenerateTaskID()
	if !transferTaskIDPattern.MatchString(id) {
		t.Fatalf("generated id %q does not match the 32-hex schema", id)
	}
}

// ==================== AJV schemas ====================
//
// The handlers are mounted individually with fabricated admin claims (the same
// harness as hostsTestEngine): `middleware.Authenticate` needs the database, so a
// full-group test without one only ever observes 401 (pinned separately below).

func TestTransferExportBodySchema(t *testing.T) {
	engine := hostsTestEngine(http.MethodPost, "/api/admin/hosts/:hostId/instances/:instanceId/export", transferExportHandler)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing-targetPath", `{}`, "targetPath is required"},
		{"short-path", `{"targetPath":"/"}`, ""},
		{"bad-mode", `{"targetPath":"/tmp/x","mode":"tape"}`, ""},
		{"bad-id", `{"targetPath":"/tmp/x","id":"not-hex"}`, ""},
	}
	for _, c := range cases {
		rec := hostsTestRequest(engine, http.MethodPost, "/api/admin/hosts/1/instances/2/export", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body = %s", c.name, rec.Code, rec.Body.String())
		}
		if c.want != "" && !strings.Contains(rec.Body.String(), c.want) {
			t.Fatalf("%s: body = %s, want %q", c.name, rec.Body.String(), c.want)
		}
	}
}

func TestTransferImportBodySchema(t *testing.T) {
	engine := hostsTestEngine(http.MethodPost, "/api/admin/hosts/:hostId/instances/import", transferImportHandler)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing-filePath", `{}`, "filePath is required"},
		{"bad-name", `{"filePath":"/b.tar.gz","name":"bad_name"}`, ""},
		{"bad-validDays", `{"filePath":"/b.tar.gz","validDays":0}`, ""},
		{"huge-validDays", `{"filePath":"/b.tar.gz","validDays":36501}`, ""},
		{"bad-displayName", `{"filePath":"/b.tar.gz","displayName":""}`, ""},
	}
	for _, c := range cases {
		rec := hostsTestRequest(engine, http.MethodPost, "/api/admin/hosts/1/instances/import", c.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, body = %s", c.name, rec.Code, rec.Body.String())
		}
		if c.want != "" && !strings.Contains(rec.Body.String(), c.want) {
			t.Fatalf("%s: body = %s, want %q", c.name, rec.Body.String(), c.want)
		}
	}
}

func TestTransferRouteParamValidation(t *testing.T) {
	// Non-numeric / out-of-range hostId fails the integer params schema.
	listEngine := hostsTestEngine(http.MethodGet, "/api/admin/hosts/:hostId/instance-transfer", transferListHandler)
	for _, path := range []string{
		"/api/admin/hosts/abc/instance-transfer",
		"/api/admin/hosts/0/instance-transfer",
	} {
		rec := hostsTestRequest(listEngine, http.MethodGet, path, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET %s: status = %d, body = %s", path, rec.Code, rec.Body.String())
		}
	}

	// The incusName route param pattern (^[a-zA-Z0-9][a-zA-Z0-9-]*$, max 63).
	for _, name := range []string{"bad_name", "bad/name", "-leading", strings.Repeat("a", 64)} {
		if transferNamePattern.MatchString(name) && len(name) <= 63 {
			t.Fatalf("incusName %q must be rejected by the params pattern", name)
		}
	}
}

func TestTransferListRequiresAuth(t *testing.T) {
	// Without a token the middleware rejects with 401 before any handler runs.
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterAdminHostInstanceTransfer(engine)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/hosts/1/instance-transfer", nil)
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}
}

func TestTransferJSONDecodeOfNodeTaskItems(t *testing.T) {
	raw := []byte(`{"id":"t1","status":"failed","error":"outer","items":[{"name":"a","error":"inner"}]}`)
	var task transferNodeTask
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := transferFormatNodeBackupError(task); got != "outer: inner" {
		t.Fatalf("combined from JSON = %q", got)
	}
}
