// Tests for the routes/system_update.go port (server/src/routes/system-update.ts).
//
// OTA is the highest-risk path in this repository (AGENTS.md §2.1), so the tests
// are weighted towards the refusal seam and towards proving the absence of side
// effects:
//
//   - the two mutating routes answer 501 OTA_DISABLED_ON_GO_BACKEND and leave the
//     task store untouched (the store interface has no write method at all, and
//     TestSystemUpdateSourceHasNoOTAExecutionSurface pins that the module's only
//     exec call is a read-only `git` invocation);
//   - the read routes keep Node's exact wire shape, validation messages and
//     authorization split (401 / 403 ADMIN_REQUIRED / 403 SUPER_ADMIN_REQUIRED);
//   - /check only talks to the injected checker: the production default performs
//     no network I/O and no `git fetch`, and the real GitHub path is exercised
//     against a fake HTTP doer.
//
// The DB-backed auth middleware needs two reads (users, token_invalidations) and
// the allowlist needs system_configs. auth_test.go already installs an in-memory
// driver for exactly those tables, so these tests reuse authInstallFakeDB instead
// of duplicating it. The update-task table is served by the store seam, which is
// what the refusal tests need to observe.
package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== fake store ====================

// systemUpdateFakeStore serves the read methods. It deliberately has no create /
// finish method: the production interface does not expose one, so a write from
// the refusal seam would not compile.
type systemUpdateFakeStore struct {
	tasks     []db.SystemUpdateTaskWithUser
	listCalls int
	getCalls  int
	lastLimit int
}

func (store *systemUpdateFakeStore) List(_ context.Context, limit int) ([]db.SystemUpdateTaskWithUser, error) {
	store.listCalls++
	store.lastLimit = limit
	return append([]db.SystemUpdateTaskWithUser(nil), store.tasks...), nil
}

func (store *systemUpdateFakeStore) Get(_ context.Context, id int32) (*db.SystemUpdateTaskWithUser, error) {
	store.getCalls++
	for _, task := range store.tasks {
		if task.ID == id {
			found := task
			return &found, nil
		}
	}
	return nil, nil
}

func systemUpdateInstallStore(t *testing.T, store systemUpdateTaskStore) {
	t.Helper()
	previous := systemUpdateTasks
	systemUpdateTasks = store
	t.Cleanup(func() { systemUpdateTasks = previous })
}

func systemUpdateInstallAllowlist(t *testing.T, ids ...int64) {
	t.Helper()
	allowed := make(map[int64]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	previous := systemUpdateAdminAllowlist
	systemUpdateAdminAllowlist = func(context.Context) (lib.CombinedAdminIDAllowlist, error) {
		return lib.CombinedAdminIDAllowlist{IDs: allowed, Configured: len(ids) > 0}, nil
	}
	t.Cleanup(func() { systemUpdateAdminAllowlist = previous })
}

func systemUpdateInstallChecker(t *testing.T, checker systemUpdateCheckRunner) {
	t.Helper()
	previous := systemUpdateChecker
	systemUpdateChecker = checker
	t.Cleanup(func() { systemUpdateChecker = previous })
}

// ==================== fixtures ====================

// systemUpdateFixtureTask returns a fully populated success row.
func systemUpdateFixtureTask() db.SystemUpdateTaskWithUser {
	fromVersion := "v1.4.0"
	backupPath := "/opt/incudal/backups/2026-10-01T12-00-00"
	logPath := "/opt/incudal/update-logs/system-update-996001.log"
	username := "admin"

	return db.SystemUpdateTaskWithUser{
		SystemUpdateTask: models.SystemUpdateTask{
			ID:              996001,
			TargetVersion:   "v1.5.8",
			FromVersion:     &fromVersion,
			Status:          models.SystemUpdateTaskStatusSuccess,
			StartedByUserID: 1,
			BackupPath:      &backupPath,
			LogPath:         &logPath,
			ErrorMessage:    nil,
			StartedAt:       systemUpdateTestTimePtr(2026, 10, 1, 12, 0, 0, 0),
			FinishedAt:      systemUpdateTestTimePtr(2026, 10, 1, 12, 5, 30, 250_000_000),
			CreatedAt:       systemUpdateTestTime(2026, 10, 1, 11, 59, 59, 0),
			UpdatedAt:       systemUpdateTestTime(2026, 10, 1, 12, 5, 30, 250_000_000),
		},
		StartedByUsername: &username,
	}
}

func systemUpdateTestTime(year int, month time.Month, day, hour, minute, second, nanosecond int) time.Time {
	return time.Date(year, month, day, hour, minute, second, nanosecond, time.UTC)
}

func systemUpdateTestTimePtr(year int, month time.Month, day, hour, minute, second, nanosecond int) *time.Time {
	moment := systemUpdateTestTime(year, month, day, hour, minute, second, nanosecond)
	return &moment
}

// systemUpdateAdminData is the fake auth DB for a super administrator named
// "admin" (the username fallback of canManageSystemUpdates).
func systemUpdateAdminData() *authFakeDBData {
	return &authFakeDBData{
		user: &authFakeUser{username: "admin", role: "admin", status: "active"},
	}
}

func systemUpdateAdminToken(t *testing.T) string {
	t.Helper()
	t.Setenv("JWT_SECRET", "system-update-test-secret")
	token, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID:       1,
		Username: "admin",
		Role:     "admin",
		Status:   "active",
		SID:      "system-update-session",
	})
	if err != nil {
		t.Fatalf("sign admin token: %v", err)
	}
	return token
}

// systemUpdateRouter builds a router with just this module mounted.
func systemUpdateRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterSystemUpdate(router)
	return router
}

// systemUpdateDo issues a request, JSON body optional.
func systemUpdateDo(router *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func systemUpdateDecode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return payload
}

func systemUpdateRequireJSONEqual(t *testing.T, label string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		t.Fatalf("%s:\n got: %s\nwant: %s", label, gotJSON, wantJSON)
	}
}

// ==================== route table and auth ====================

// TestSystemUpdateRegistersAllRoutes pins the seven routes system-update.ts
// declares under /api/admin/system-update.
func TestSystemUpdateRegistersAllRoutes(t *testing.T) {
	router := systemUpdateRouter(t)

	expected := map[string]bool{
		"GET /api/admin/system-update/version":   true,
		"GET /api/admin/system-update/check":     true,
		"GET /api/admin/system-update/tasks":     true,
		"GET /api/admin/system-update/tasks/:id": true,
		// find-my-way matches /tasks/ as :id="" and routes it to the same handler;
		// the literal alias reproduces that (measured against the Node backend).
		"GET /api/admin/system-update/tasks/":              true,
		"GET /api/admin/system-update/tasks/:id/logs":      true,
		"POST /api/admin/system-update/start":              true,
		"POST /api/admin/system-update/tasks/:id/rollback": true,
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

// TestSystemUpdateAnonymousIs401 proves every route (reads included) is wired to
// authenticateAdmin. Fastify answers the missing token with a bare
// `{ error: 'Unauthorized' }`.
func TestSystemUpdateAnonymousIs401(t *testing.T) {
	router := systemUpdateRouter(t)

	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/admin/system-update/version"},
		{http.MethodGet, "/api/admin/system-update/check"},
		{http.MethodGet, "/api/admin/system-update/tasks"},
		{http.MethodGet, "/api/admin/system-update/tasks/1"},
		{http.MethodGet, "/api/admin/system-update/tasks/1/logs"},
		{http.MethodPost, "/api/admin/system-update/start"},
		{http.MethodPost, "/api/admin/system-update/tasks/1/rollback"},
	}

	for _, tc := range cases {
		recorder := systemUpdateDo(router, tc.method, tc.path, "", `{"targetVersion":"v1.5.8"}`)
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", tc.method, tc.path, recorder.Code)
			continue
		}
		if payload := systemUpdateDecode(t, recorder); payload["error"] != "Unauthorized" {
			t.Errorf("%s %s: error = %v", tc.method, tc.path, payload["error"])
		}
	}
}

// TestSystemUpdateNonAdminIs403 proves a valid non-admin token is rejected by
// authenticateAdmin before any handler runs.
func TestSystemUpdateNonAdminIs403(t *testing.T) {
	t.Setenv("JWT_SECRET", "system-update-test-secret")
	authInstallFakeDB(t, &authFakeDBData{
		user: &authFakeUser{username: "parityuser", role: "user", status: "active"},
	})

	token, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID: 2, Username: "parityuser", Role: "user", Status: "active", SID: "non-admin-session",
	})
	if err != nil {
		t.Fatalf("sign non-admin token: %v", err)
	}

	router := systemUpdateRouter(t)
	for _, path := range []string{
		"/api/admin/system-update/version",
		"/api/admin/system-update/tasks",
		"/api/admin/system-update/tasks/1",
		"/api/admin/system-update/tasks/1/logs",
	} {
		recorder := systemUpdateDo(router, http.MethodGet, path, token, "")
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", path, recorder.Code)
		}
		if payload := systemUpdateDecode(t, recorder); payload["code"] != "ADMIN_REQUIRED" {
			t.Fatalf("%s: code = %v, want ADMIN_REQUIRED", path, payload["code"])
		}
	}

	recorder := systemUpdateDo(router, http.MethodPost, "/api/admin/system-update/start", token, `{"targetVersion":"v1.5.8"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("start: status = %d, want 403", recorder.Code)
	}
	if payload := systemUpdateDecode(t, recorder); payload["code"] != "ADMIN_REQUIRED" {
		t.Fatalf("start: code = %v, want ADMIN_REQUIRED", payload["code"])
	}
}

// TestSystemUpdateCanManageSystemUpdates pins the allowlist rules:
// role admin → allowlist (when configured) → username "admin" fallback.
func TestSystemUpdateCanManageSystemUpdates(t *testing.T) {
	ctx := context.Background()
	t.Setenv("SYSTEM_UPDATE_ALLOWED_ADMIN_IDS", "")

	lib.InvalidateCachedConfig("system_update_allowed_admin_ids")
	authInstallFakeDB(t, &authFakeDBData{config: map[string]string{}})

	if allowed, err := systemUpdateCanManage(ctx, &lib.AccessTokenClaims{ID: 1, Username: "admin", Role: "admin"}); err != nil || !allowed {
		t.Fatalf("username fallback: allowed=%v err=%v, want true", allowed, err)
	}
	if allowed, err := systemUpdateCanManage(ctx, &lib.AccessTokenClaims{ID: 2, Username: "ops", Role: "admin"}); err != nil || allowed {
		t.Fatalf("non-administrator username: allowed=%v err=%v, want false", allowed, err)
	}
	if allowed, err := systemUpdateCanManage(ctx, &lib.AccessTokenClaims{ID: 1, Username: "admin", Role: "user"}); err != nil || allowed {
		t.Fatalf("role user: allowed=%v err=%v, want false", allowed, err)
	}

	// A configured allowlist takes over: the literal username no longer helps.
	lib.InvalidateCachedConfig("system_update_allowed_admin_ids")
	authInstallFakeDB(t, &authFakeDBData{config: map[string]string{
		"system_update_allowed_admin_ids": "7, 8",
	}})

	if allowed, err := systemUpdateCanManage(ctx, &lib.AccessTokenClaims{ID: 8, Username: "ops", Role: "admin"}); err != nil || !allowed {
		t.Fatalf("configured allowlist hit: allowed=%v err=%v, want true", allowed, err)
	}
	if allowed, err := systemUpdateCanManage(ctx, &lib.AccessTokenClaims{ID: 9, Username: "admin", Role: "admin"}); err != nil || allowed {
		t.Fatalf("configured allowlist miss: allowed=%v err=%v, want false", allowed, err)
	}
}

// ==================== /version ====================

// TestSystemUpdateVersionMetadata covers the version.json + local git fallback
// chain. The temp directory has no git repository, so every git lookup returns
// not-ok and the file metadata decides the result.
func TestSystemUpdateVersionMetadata(t *testing.T) {
	root := t.TempDir()
	t.Setenv("INCUDAL_APP_DIR", root)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"version":"1.5.8"}`), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	// No version.json: version falls back to v<package version>.
	metadata := systemUpdateCurrentVersionMetadata(ctx, root)
	want := systemUpdateVersionMetadata{Version: "v1.5.8", Changelog: []string{}}
	systemUpdateRequireJSONEqual(t, "package.json fallback", metadata, want)

	// version.json wins, changelog arrays are trimmed but keep list markers
	// (Node only strips markers for the string form).
	versionJSON := `{
	  "version": "v9.8.7-test",
	  "gitTag": "v9.8.7",
	  "gitCommit": "abcdef123456",
	  "buildTime": "2026-01-02T03:04:05.000Z",
	  "deployedAt": null,
	  "changelog": ["- first change", "*  second change", "  ", "third"]
	}`
	if err := os.WriteFile(filepath.Join(root, "version.json"), []byte(versionJSON), 0o644); err != nil {
		t.Fatalf("write version.json: %v", err)
	}

	metadata = systemUpdateCurrentVersionMetadata(ctx, root)
	if metadata.Version != "v9.8.7-test" {
		t.Fatalf("version = %q", metadata.Version)
	}
	if metadata.GitTag == nil || *metadata.GitTag != "v9.8.7" {
		t.Fatalf("gitTag = %v", metadata.GitTag)
	}
	if metadata.GitCommit == nil || *metadata.GitCommit != "abcdef123456" {
		t.Fatalf("gitCommit = %v", metadata.GitCommit)
	}
	if metadata.BuildTime == nil || *metadata.BuildTime != "2026-01-02T03:04:05.000Z" {
		t.Fatalf("buildTime = %v", metadata.BuildTime)
	}
	if metadata.DeployedAt != nil {
		t.Fatalf("deployedAt = %v, want nil", metadata.DeployedAt)
	}
	systemUpdateRequireJSONEqual(t, "changelog", metadata.Changelog,
		[]string{"- first change", "*  second change", "third"})
}

// TestSystemUpdateNormalizeChangelog pins the string form: split on newlines,
// strip a leading "- " / "* " marker, trim, drop empties, cap at 50.
func TestSystemUpdateNormalizeChangelog(t *testing.T) {
	got := systemUpdateNormalizeChangelog("first\n- second\r\n*   third  \n\n")
	systemUpdateRequireJSONEqual(t, "string form", got, []string{"first", "second", "third"})

	lines := make([]any, 0, 60)
	for i := 0; i < 60; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	if trimmed := systemUpdateNormalizeChangelog(lines); len(trimmed) != 50 {
		t.Fatalf("cap = %d, want 50", len(trimmed))
	}
}

// TestSystemUpdateVersionEndpoint proves the handler wraps the metadata under
// `version`, exactly like `{ version: await getCurrentVersionMetadata() }`.
func TestSystemUpdateVersionEndpoint(t *testing.T) {
	root := t.TempDir()
	t.Setenv("INCUDAL_APP_DIR", root)
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"version":"1.5.8"}`), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}

	token := systemUpdateAdminToken(t)
	authInstallFakeDB(t, systemUpdateAdminData())
	systemUpdateInstallAllowlist(t)

	recorder := systemUpdateDo(systemUpdateRouter(t), http.MethodGet, "/api/admin/system-update/version", token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	payload := systemUpdateDecode(t, recorder)
	systemUpdateRequireJSONEqual(t, "version body", payload, map[string]any{
		"version": map[string]any{
			"version":    "v1.5.8",
			"gitTag":     nil,
			"gitCommit":  nil,
			"buildTime":  nil,
			"deployedAt": nil,
			"changelog":  []any{},
		},
	})
}

// ==================== /tasks ====================

// TestSystemUpdateTasksShape pins the serialized task shape and the `take: 30`
// limit of listSystemUpdateTasks(30).
func TestSystemUpdateTasksShape(t *testing.T) {
	token := systemUpdateAdminToken(t)
	authInstallFakeDB(t, systemUpdateAdminData())
	systemUpdateInstallAllowlist(t)

	store := &systemUpdateFakeStore{tasks: []db.SystemUpdateTaskWithUser{systemUpdateFixtureTask()}}
	systemUpdateInstallStore(t, store)

	router := systemUpdateRouter(t)

	recorder := systemUpdateDo(router, http.MethodGet, "/api/admin/system-update/tasks", token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if store.lastLimit != 30 {
		t.Fatalf("limit = %d, want 30", store.lastLimit)
	}

	payload := systemUpdateDecode(t, recorder)
	systemUpdateRequireJSONEqual(t, "tasks body", payload, map[string]any{
		"tasks": []any{map[string]any{
			"id":                float64(996001),
			"targetVersion":     "v1.5.8",
			"fromVersion":       "v1.4.0",
			"status":            "success",
			"startedByUserId":   float64(1),
			"startedByUsername": "admin",
			"backupPath":        "/opt/incudal/backups/2026-10-01T12-00-00",
			"logPath":           "/opt/incudal/update-logs/system-update-996001.log",
			"errorMessage":      nil,
			"startedAt":         "2026-10-01T12:00:00.000Z",
			"finishedAt":        "2026-10-01T12:05:30.250Z",
			"createdAt":         "2026-10-01T11:59:59.000Z",
			"updatedAt":         "2026-10-01T12:05:30.250Z",
		}},
	})

	// One task by id.
	recorder = systemUpdateDo(router, http.MethodGet, "/api/admin/system-update/tasks/996001", token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	single := systemUpdateDecode(t, recorder)
	task, ok := single["task"].(map[string]any)
	if !ok {
		t.Fatalf("task = %v", single["task"])
	}
	if task["id"] != float64(996001) || task["targetVersion"] != "v1.5.8" {
		t.Fatalf("task = %v", task)
	}

	// Absent row → 404 TASK_NOT_FOUND.
	recorder = systemUpdateDo(router, http.MethodGet, "/api/admin/system-update/tasks/996999", token, "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", recorder.Code)
	}
	systemUpdateRequireJSONEqual(t, "missing task", systemUpdateDecode(t, recorder), map[string]any{
		"error": "Task not found",
		"code":  "TASK_NOT_FOUND",
	})

	// Empty list is `[]`, never null.
	empty := &systemUpdateFakeStore{}
	systemUpdateInstallStore(t, empty)
	recorder = systemUpdateDo(systemUpdateRouter(t), http.MethodGet, "/api/admin/system-update/tasks", token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("empty list status = %d", recorder.Code)
	}
	systemUpdateRequireJSONEqual(t, "empty list", systemUpdateDecode(t, recorder),
		map[string]any{"tasks": []any{}})
}

// TestSystemUpdateParsePositiveID pins parsePositiveId: Number(value), then
// Number.isSafeInteger(id) && id > 0. Note "01" and "1.0" are VALID here — unlike
// the strict params schemas used by other admin routes, system-update.ts has no
// params schema and calls Number() directly.
func TestSystemUpdateParsePositiveID(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
		ok   bool
	}{
		{"1", 1, true},
		{"01", 1, true},
		{"1.0", 1, true},
		{"1e2", 100, true},
		{"0x10", 16, true},
		{" 2 ", 2, true},
		{"+3", 3, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"1.5", 0, false},
		{"abc", 0, false},
		{"", 0, false},
		{"1_0", 0, false},
		{"NaN", 0, false},
		{"inf", 0, false},
		{"9007199254740991", 9007199254740991, true},
		{"9007199254740992", 0, false},
	}

	for _, tc := range cases {
		got, ok := systemUpdateParsePositiveID(tc.raw)
		if ok != tc.ok {
			t.Errorf("parsePositiveID(%q) ok = %v, want %v", tc.raw, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parsePositiveID(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestSystemUpdateInvalidTaskIDIs400 covers the route-level envelope for ids that
// parse to null.
func TestSystemUpdateInvalidTaskIDIs400(t *testing.T) {
	token := systemUpdateAdminToken(t)
	authInstallFakeDB(t, systemUpdateAdminData())
	systemUpdateInstallAllowlist(t)
	systemUpdateInstallStore(t, &systemUpdateFakeStore{tasks: []db.SystemUpdateTaskWithUser{systemUpdateFixtureTask()}})

	router := systemUpdateRouter(t)
	for _, id := range []string{"0", "abc", "1.5", "-2"} {
		recorder := systemUpdateDo(router, http.MethodGet, "/api/admin/system-update/tasks/"+id, token, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("id %q: status = %d, want 400", id, recorder.Code)
		}
		systemUpdateRequireJSONEqual(t, "invalid id "+id, systemUpdateDecode(t, recorder), map[string]any{
			"error": "Invalid task id",
			"code":  "INVALID_TASK_ID",
		})
	}
}

// ==================== /tasks/:id/logs ====================

// TestSystemUpdateLogsEndpoint covers the single-snapshot log response: it is NOT
// an SSE stream, so the body closes the exchange (application/json, one read).
func TestSystemUpdateLogsEndpoint(t *testing.T) {
	root := t.TempDir()
	t.Setenv("INCUDAL_APP_DIR", root)
	logDir := filepath.Join(root, "update-logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}
	logPath := filepath.Join(logDir, "system-update-996001.log")
	if err := os.WriteFile(logPath, []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	token := systemUpdateAdminToken(t)
	authInstallFakeDB(t, systemUpdateAdminData())
	systemUpdateInstallAllowlist(t)

	task := systemUpdateFixtureTask()
	task.LogPath = &logPath
	systemUpdateInstallStore(t, &systemUpdateFakeStore{tasks: []db.SystemUpdateTaskWithUser{task}})

	recorder := systemUpdateDo(systemUpdateRouter(t), http.MethodGet,
		"/api/admin/system-update/tasks/996001/logs", token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("content-type = %q, want application/json (no SSE stream)", contentType)
	}
	systemUpdateRequireJSONEqual(t, "logs body", systemUpdateDecode(t, recorder),
		map[string]any{"logs": "line one\nline two\n"})
}

// TestSystemUpdateLogsTailAndFallbacks covers:
//   - a log file larger than 200k units is truncated to its last 200k units;
//   - a NULL/empty log_path answers { logs: "" };
//   - a log_path outside the update log directory is refused with 403 (the
//     endpoint must never read arbitrary files);
//   - a missing file answers { logs: "" } like readFile's catch branch.
func TestSystemUpdateLogsTailAndFallbacks(t *testing.T) {
	root := t.TempDir()
	t.Setenv("INCUDAL_APP_DIR", root)
	logDir := filepath.Join(root, "update-logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}

	bigPath := filepath.Join(logDir, "system-update-996001.log")
	big := strings.Repeat("a", 250000)
	if err := os.WriteFile(bigPath, []byte(big), 0o644); err != nil {
		t.Fatalf("write big log: %v", err)
	}

	token := systemUpdateAdminToken(t)
	authInstallFakeDB(t, systemUpdateAdminData())
	systemUpdateInstallAllowlist(t)
	router := systemUpdateRouter(t)

	bigTask := systemUpdateFixtureTask()
	bigTask.LogPath = &bigPath
	systemUpdateInstallStore(t, &systemUpdateFakeStore{tasks: []db.SystemUpdateTaskWithUser{bigTask}})

	recorder := systemUpdateDo(router, http.MethodGet, "/api/admin/system-update/tasks/996001/logs", token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("big log status = %d", recorder.Code)
	}
	logs, _ := systemUpdateDecode(t, recorder)["logs"].(string)
	if len(logs) != 200000 {
		t.Fatalf("tail length = %d, want 200000", len(logs))
	}

	// NULL log_path.
	nullTask := systemUpdateFixtureTask()
	nullTask.LogPath = nil
	systemUpdateInstallStore(t, &systemUpdateFakeStore{tasks: []db.SystemUpdateTaskWithUser{nullTask}})
	recorder = systemUpdateDo(router, http.MethodGet, "/api/admin/system-update/tasks/996001/logs", token, "")
	systemUpdateRequireJSONEqual(t, "null log path", systemUpdateDecode(t, recorder),
		map[string]any{"logs": ""})

	// Empty string log_path.
	emptyTask := systemUpdateFixtureTask()
	emptyPath := ""
	emptyTask.LogPath = &emptyPath
	systemUpdateInstallStore(t, &systemUpdateFakeStore{tasks: []db.SystemUpdateTaskWithUser{emptyTask}})
	recorder = systemUpdateDo(router, http.MethodGet, "/api/admin/system-update/tasks/996001/logs", token, "")
	systemUpdateRequireJSONEqual(t, "empty log path", systemUpdateDecode(t, recorder),
		map[string]any{"logs": ""})

	// Outside the log directory.
	outsideTask := systemUpdateFixtureTask()
	outsidePath := "/etc/passwd"
	outsideTask.LogPath = &outsidePath
	systemUpdateInstallStore(t, &systemUpdateFakeStore{tasks: []db.SystemUpdateTaskWithUser{outsideTask}})
	recorder = systemUpdateDo(router, http.MethodGet, "/api/admin/system-update/tasks/996001/logs", token, "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("outside path status = %d, want 403", recorder.Code)
	}
	systemUpdateRequireJSONEqual(t, "outside log path", systemUpdateDecode(t, recorder), map[string]any{
		"error": "Log path is outside update log directory",
		"code":  "LOG_PATH_FORBIDDEN",
	})

	// Missing file inside the log directory.
	missingTask := systemUpdateFixtureTask()
	missingPath := filepath.Join(logDir, "does-not-exist.log")
	missingTask.LogPath = &missingPath
	systemUpdateInstallStore(t, &systemUpdateFakeStore{tasks: []db.SystemUpdateTaskWithUser{missingTask}})
	recorder = systemUpdateDo(router, http.MethodGet, "/api/admin/system-update/tasks/996001/logs", token, "")
	systemUpdateRequireJSONEqual(t, "missing log file", systemUpdateDecode(t, recorder),
		map[string]any{"logs": ""})
}

// TestSystemUpdateLogsRequiresSuperAdmin pins the second authorization gate: an
// authenticated administrator who is not on the OTA allowlist gets
// 403 SUPER_ADMIN_REQUIRED (reads of /version do not have this gate).
func TestSystemUpdateLogsRequiresSuperAdmin(t *testing.T) {
	t.Setenv("JWT_SECRET", "system-update-test-secret")
	authInstallFakeDB(t, &authFakeDBData{
		user: &authFakeUser{username: "ops", role: "admin", status: "active"},
	})
	systemUpdateInstallAllowlist(t, 999)
	systemUpdateInstallStore(t, &systemUpdateFakeStore{tasks: []db.SystemUpdateTaskWithUser{systemUpdateFixtureTask()}})

	token, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID: 2, Username: "ops", Role: "admin", Status: "active", SID: "ops-session",
	})
	if err != nil {
		t.Fatalf("sign ops token: %v", err)
	}

	recorder := systemUpdateDo(systemUpdateRouter(t), http.MethodGet,
		"/api/admin/system-update/tasks/996001/logs", token, "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", recorder.Code, recorder.Body.String())
	}
	systemUpdateRequireJSONEqual(t, "super admin required", systemUpdateDecode(t, recorder), map[string]any{
		"error": "Super administrator privileges required",
		"code":  "SUPER_ADMIN_REQUIRED",
	})
}

// ==================== /start and /rollback refusal seam ====================

// TestSystemUpdateStartRefusesWithoutSideEffects is the core OTA guard: a fully
// valid /start request stops at 501 and touches nothing.
func TestSystemUpdateStartRefusesWithoutSideEffects(t *testing.T) {
	token := systemUpdateAdminToken(t)
	authInstallFakeDB(t, systemUpdateAdminData())
	systemUpdateInstallAllowlist(t)

	store := &systemUpdateFakeStore{tasks: []db.SystemUpdateTaskWithUser{systemUpdateFixtureTask()}}
	systemUpdateInstallStore(t, store)

	router := systemUpdateRouter(t)
	recorder := systemUpdateDo(router, http.MethodPost, "/api/admin/system-update/start", token,
		`{"targetVersion":"v1.5.8"}`)

	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body %s)", recorder.Code, recorder.Body.String())
	}
	payload := systemUpdateDecode(t, recorder)
	if payload["code"] != "OTA_DISABLED_ON_GO_BACKEND" {
		t.Fatalf("code = %v, want OTA_DISABLED_ON_GO_BACKEND", payload["code"])
	}
	if message, _ := payload["error"].(string); message == "" {
		t.Fatalf("error message is empty: %v", payload)
	}

	// No side effects: the store is not even read, and the row count is unchanged.
	if store.listCalls != 0 || store.getCalls != 0 {
		t.Fatalf("store was called (list=%d get=%d); /start must not touch the database",
			store.listCalls, store.getCalls)
	}
	if len(store.tasks) != 1 {
		t.Fatalf("task rows changed: %d", len(store.tasks))
	}
}

// TestSystemUpdateRollbackRefusesWithoutSideEffects: a valid rollback request
// reads the task for its existence/backup checks, then stops at 501.
func TestSystemUpdateRollbackRefusesWithoutSideEffects(t *testing.T) {
	token := systemUpdateAdminToken(t)
	authInstallFakeDB(t, systemUpdateAdminData())
	systemUpdateInstallAllowlist(t)

	store := &systemUpdateFakeStore{tasks: []db.SystemUpdateTaskWithUser{systemUpdateFixtureTask()}}
	systemUpdateInstallStore(t, store)

	recorder := systemUpdateDo(systemUpdateRouter(t), http.MethodPost,
		"/api/admin/system-update/tasks/996001/rollback", token, "")
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body %s)", recorder.Code, recorder.Body.String())
	}
	if payload := systemUpdateDecode(t, recorder); payload["code"] != "OTA_DISABLED_ON_GO_BACKEND" {
		t.Fatalf("code = %v, want OTA_DISABLED_ON_GO_BACKEND", payload["code"])
	}
	if store.getCalls != 1 {
		t.Fatalf("get calls = %d, want the single existence check", store.getCalls)
	}
	if len(store.tasks) != 1 || store.tasks[0].Status != models.SystemUpdateTaskStatusSuccess {
		t.Fatalf("task rows changed: %+v", store.tasks)
	}

	// "01" is Number("01") === 1 in Node, so it resolves to the same task.
	recorder = systemUpdateDo(systemUpdateRouter(t), http.MethodPost,
		"/api/admin/system-update/tasks/0996001/rollback", token, "")
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("leading-zero id status = %d, want 501", recorder.Code)
	}
}

// TestSystemUpdateRollbackValidation covers the checks that run before the
// refusal: invalid id, missing task and a task without a backup path.
func TestSystemUpdateRollbackValidation(t *testing.T) {
	token := systemUpdateAdminToken(t)
	authInstallFakeDB(t, systemUpdateAdminData())
	systemUpdateInstallAllowlist(t)

	noBackup := systemUpdateFixtureTask()
	noBackup.ID = 996002
	noBackup.BackupPath = nil
	systemUpdateInstallStore(t, &systemUpdateFakeStore{tasks: []db.SystemUpdateTaskWithUser{
		systemUpdateFixtureTask(), noBackup,
	}})

	router := systemUpdateRouter(t)

	recorder := systemUpdateDo(router, http.MethodPost, "/api/admin/system-update/tasks/abc/rollback", token, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d, want 400", recorder.Code)
	}

	recorder = systemUpdateDo(router, http.MethodPost, "/api/admin/system-update/tasks/999999/rollback", token, "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing task status = %d, want 404", recorder.Code)
	}
	systemUpdateRequireJSONEqual(t, "missing rollback task", systemUpdateDecode(t, recorder),
		map[string]any{"error": "Task not found", "code": "TASK_NOT_FOUND"})

	recorder = systemUpdateDo(router, http.MethodPost, "/api/admin/system-update/tasks/996002/rollback", token, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing backup status = %d, want 400 (body %s)", recorder.Code, recorder.Body.String())
	}
	systemUpdateRequireJSONEqual(t, "missing backup", systemUpdateDecode(t, recorder), map[string]any{
		"error": "Task has no backup path",
		"code":  "BACKUP_MISSING",
	})
}

// TestSystemUpdateStartValidation pins the AJV messages and the INVALID_TARGET_VERSION
// branch. Every case stops before the refusal, so Node and Go agree on the whole
// validation surface.
func TestSystemUpdateStartValidation(t *testing.T) {
	token := systemUpdateAdminToken(t)
	authInstallFakeDB(t, systemUpdateAdminData())
	systemUpdateInstallAllowlist(t)
	store := &systemUpdateFakeStore{}
	systemUpdateInstallStore(t, store)

	router := systemUpdateRouter(t)

	validationCases := []struct {
		name string
		body string
		want string
	}{
		{"missing", `{}`, "targetVersion is required"},
		{"too short", `{"targetVersion":"v1.2"}`, "targetVersion is too short"},
		{"too long", `{"targetVersion":"` + strings.Repeat("a", 65) + `"}`, "targetVersion is too long"},
		{"null coerces to empty string", `{"targetVersion":null}`, "targetVersion is too short"},
		{"unknown keys are removed", `{"targetVersion":"v1.5.8","evil":1}`, ""},
	}

	for _, tc := range validationCases {
		recorder := systemUpdateDo(router, http.MethodPost, "/api/admin/system-update/start", token, tc.body)
		if tc.want == "" {
			// Unknown keys are stripped by Fastify's `removeAdditional: true`, so the
			// request reaches the refusal.
			if recorder.Code != http.StatusNotImplemented {
				t.Fatalf("%s: status = %d, want 501 (body %s)", tc.name, recorder.Code, recorder.Body.String())
			}
			continue
		}
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body %s)", tc.name, recorder.Code, recorder.Body.String())
		}
		systemUpdateRequireJSONEqual(t, tc.name, systemUpdateDecode(t, recorder), map[string]any{
			"error": tc.want,
			"code":  "VALIDATION_ERROR",
		})
	}

	invalidTagCases := []string{
		`{"targetVersion":"1.2.3"}`,
		`{"targetVersion":"v1.2"}`,
		`{"targetVersion":"vv1.2.3"}`,
		`{"targetVersion":12345}`,
	}
	for _, body := range invalidTagCases {
		recorder := systemUpdateDo(router, http.MethodPost, "/api/admin/system-update/start", token, body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body %s)", body, recorder.Code, recorder.Body.String())
		}
		payload := systemUpdateDecode(t, recorder)
		if payload["code"] != "INVALID_TARGET_VERSION" && payload["code"] != "VALIDATION_ERROR" {
			t.Fatalf("%s: code = %v", body, payload["code"])
		}
	}

	if store.listCalls != 0 || store.getCalls != 0 {
		t.Fatalf("store was called (list=%d get=%d); validation paths must not write", store.listCalls, store.getCalls)
	}
}

// TestSystemUpdateStartRequiresSuperAdmin proves the allowlist gate runs before
// the schema, like Node's handler order (requireUpdateManager → body schema is
// actually the reverse in Fastify — onRequest first, then validation — and the
// super-admin check is INSIDE the handler, after validation. This test uses a
// body that passes validation so both orders agree on 403.)
func TestSystemUpdateStartRequiresSuperAdmin(t *testing.T) {
	t.Setenv("JWT_SECRET", "system-update-test-secret")
	authInstallFakeDB(t, &authFakeDBData{
		user: &authFakeUser{username: "ops", role: "admin", status: "active"},
	})
	systemUpdateInstallAllowlist(t, 999)
	systemUpdateInstallStore(t, &systemUpdateFakeStore{})

	token, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID: 2, Username: "ops", Role: "admin", Status: "active", SID: "ops-session",
	})
	if err != nil {
		t.Fatalf("sign ops token: %v", err)
	}

	recorder := systemUpdateDo(systemUpdateRouter(t), http.MethodPost,
		"/api/admin/system-update/start", token, `{"targetVersion":"v1.5.8"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", recorder.Code, recorder.Body.String())
	}
	if payload := systemUpdateDecode(t, recorder); payload["code"] != "SUPER_ADMIN_REQUIRED" {
		t.Fatalf("code = %v, want SUPER_ADMIN_REQUIRED", payload["code"])
	}

	// Fastify validates the body schema before the handler runs, so an invalid body
	// is a 400 even for an administrator outside the allowlist.
	recorder = systemUpdateDo(systemUpdateRouter(t), http.MethodPost,
		"/api/admin/system-update/start", token, `{}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid body before allowlist: status = %d, want 400 (body %s)",
			recorder.Code, recorder.Body.String())
	}
	if payload := systemUpdateDecode(t, recorder); payload["error"] != "targetVersion is required" {
		t.Fatalf("invalid body before allowlist: error = %v", payload["error"])
	}
}

// TestSystemUpdateSourceHasNoOTAExecutionSurface is the static guard behind the
// "no script was called" claim: the module may only exec `git` (read-only version
// metadata) and must not contain any of the Node updater entry points, systemctl
// calls or filesystem mutations.
func TestSystemUpdateSourceHasNoOTAExecutionSurface(t *testing.T) {
	sources := []string{"system_update.go", filepath.Join("..", "db", "system_update_tasks.go")}
	forbidden := []string{
		"run-system-update-task",
		"rollback-system-update-task",
		"systemctl",
		"incudal-online-update",
		"incudal-online-rollback",
		"os.WriteFile",
		"os.Rename",
		"os.Remove",
		"os.MkdirAll",
		"os.StartProcess",
		"syscall.Exec",
	}

	for _, file := range sources {
		payload, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(payload)
		for _, token := range forbidden {
			if strings.Contains(text, token) {
				t.Errorf("%s contains forbidden OTA execution token %q", file, token)
			}
		}
	}

	payload, err := os.ReadFile("system_update.go")
	if err != nil {
		t.Fatalf("read system_update.go: %v", err)
	}
	text := string(payload)

	// Exactly one exec: the read-only git helper.
	if count := strings.Count(text, "exec.CommandContext("); count != 1 {
		t.Fatalf("exec.CommandContext occurrences = %d, want exactly 1 (the git helper)", count)
	}
	if !strings.Contains(text, `exec.CommandContext(commandContext, "git", args...)`) {
		t.Fatalf("the only exec call must be the read-only git helper")
	}
}

// ==================== /check ====================

// systemUpdateRecordingChecker records how /check used the seam.
type systemUpdateRecordingChecker struct {
	calls    int
	root     string
	current  systemUpdateVersionMetadata
	response systemUpdateCheckResult
}

func (checker *systemUpdateRecordingChecker) Check(_ context.Context, root string, current systemUpdateVersionMetadata) (systemUpdateCheckResult, error) {
	checker.calls++
	checker.root = root
	checker.current = current
	return checker.response, nil
}

// TestSystemUpdateCheckUsesInjectedChecker proves /check is fully behind the
// seam, so the test never reaches the network.
func TestSystemUpdateCheckUsesInjectedChecker(t *testing.T) {
	root := t.TempDir()
	t.Setenv("INCUDAL_APP_DIR", root)

	token := systemUpdateAdminToken(t)
	authInstallFakeDB(t, systemUpdateAdminData())
	systemUpdateInstallAllowlist(t)

	ota := systemUpdateOtaRelease{
		ManifestAvailable: true,
		ManifestURL:       systemUpdatePtr("https://github.com/x/y/releases/download/v2.0.0/ota-manifest.json"),
		Artifacts:         []systemUpdateOtaArtifact{},
		Error:             nil,
	}
	checker := &systemUpdateRecordingChecker{response: systemUpdateCheckResult{
		Current: systemUpdateVersionMetadata{Version: "v1.5.8", Changelog: []string{}},
		Latest: &systemUpdateAvailableUpdate{
			Version:   "v2.0.0",
			Changelog: []string{},
			OTA:       ota,
		},
		Updates: []systemUpdateAvailableUpdate{{
			Version:   "v2.0.0",
			Changelog: []string{},
			OTA:       ota,
		}},
		UpdateAvailable:     true,
		RepositoryAvailable: true,
		RepositoryError:     nil,
	}}
	systemUpdateInstallChecker(t, checker)

	recorder := systemUpdateDo(systemUpdateRouter(t), http.MethodGet, "/api/admin/system-update/check", token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if checker.calls != 1 {
		t.Fatalf("checker calls = %d, want 1", checker.calls)
	}
	if checker.root != root {
		t.Fatalf("checker root = %q, want %q", checker.root, root)
	}

	payload := systemUpdateDecode(t, recorder)
	if payload["canManageUpdates"] != true {
		t.Fatalf("canManageUpdates = %v, want true", payload["canManageUpdates"])
	}
	if payload["updateAvailable"] != true || payload["repositoryAvailable"] != true {
		t.Fatalf("flags = %v", payload)
	}
	latest, ok := payload["latest"].(map[string]any)
	if !ok || latest["version"] != "v2.0.0" {
		t.Fatalf("latest = %v", payload["latest"])
	}
	if updates, ok := payload["updates"].([]any); !ok || len(updates) != 1 {
		t.Fatalf("updates = %v", payload["updates"])
	}
}

// TestSystemUpdateDefaultCheckIsNetworkFree pins the production default: the
// disabled checker, no error, and an explicit repositoryAvailable=false so the UI
// never shows a false "you are up to date".
func TestSystemUpdateDefaultCheckIsNetworkFree(t *testing.T) {
	if _, ok := systemUpdateChecker.(systemUpdateDisabledChecker); !ok {
		t.Fatalf("default checker = %T, want systemUpdateDisabledChecker", systemUpdateChecker)
	}

	checker := systemUpdateDisabledChecker{}
	result, err := checker.Check(context.Background(), "/nonexistent", systemUpdateVersionMetadata{
		Version:   "v1.5.8",
		Changelog: []string{},
	})
	if err != nil {
		t.Fatalf("disabled checker: %v", err)
	}
	if result.RepositoryAvailable {
		t.Fatal("disabled checker must report repositoryAvailable=false")
	}
	if result.RepositoryError == nil || *result.RepositoryError == "" {
		t.Fatal("disabled checker must explain itself in repositoryError")
	}
	if result.Latest != nil || len(result.Updates) != 0 || result.UpdateAvailable {
		t.Fatalf("disabled checker must not report updates: %+v", result)
	}
	if result.Current.Version != "v1.5.8" {
		t.Fatalf("current = %+v", result.Current)
	}
}

// systemUpdateFakeHTTPDoer serves canned GitHub responses and records requests.
type systemUpdateFakeHTTPDoer struct {
	requests []*http.Request
	routes   map[string]string
}

func (doer *systemUpdateFakeHTTPDoer) Do(request *http.Request) (*http.Response, error) {
	doer.requests = append(doer.requests, request)
	body, found := doer.routes[request.URL.String()]
	if !found {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader(`{"message":"not found"}`)),
			Header:     make(http.Header),
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

// TestSystemUpdateGitHubCheckerWithFakeClient exercises the real GitHub code path
// entirely against an injected HTTP client. No test in this file opens a socket.
func TestSystemUpdateGitHubCheckerWithFakeClient(t *testing.T) {
	t.Setenv("SYSTEM_UPDATE_RELEASE_REPOSITORY", "Rain-kl/payincus")
	t.Setenv("SYSTEM_UPDATE_RELEASE_TOKEN", "ghp_test_token")
	t.Setenv("GITHUB_TOKEN", "")

	sha := strings.Repeat("a1", 32)
	releasesURL := "https://api.github.com/repos/Rain-kl/payincus/releases?per_page=30"
	releaseURL := "https://api.github.com/repos/Rain-kl/payincus/releases/tags/v2.0.0"
	manifestURL := "https://github.com/Rain-kl/payincus/releases/download/v2.0.0/ota-manifest.json"

	doer := &systemUpdateFakeHTTPDoer{routes: map[string]string{
		releasesURL: `[
		  {"tag_name":"v2.0.0","draft":false,"prerelease":false},
		  {"tag_name":"v2.0.1","draft":true},
		  {"tag_name":"v1.9.0","prerelease":true},
		  {"tag_name":"nonsense"},
		  {"tag_name":"v1.5.8","draft":false,"prerelease":false}
		]`,
		releaseURL: `{"assets":[
		  {"name":"ota-manifest.json","browser_download_url":"` + manifestURL + `"}
		]}`,
		manifestURL: `{"artifacts":[
		  {"name":"incudal-v2.0.0-linux-amd64.tar.gz","platform":"linux","arch":"amd64",
		   "url":"https://github.com/x/y/releases/download/v2.0.0/a.tar.gz",
		   "sha256":"` + strings.ToUpper(sha) + `","size":1024},
		  {"name":"broken","platform":"linux","arch":"arm64","url":"https://x/y","sha256":"zz","size":1}
		]}`,
	}}

	checker := systemUpdateNewGitHubChecker(doer)
	result, err := checker.Check(context.Background(), "/nonexistent", systemUpdateVersionMetadata{
		Version:   "v1.5.8",
		GitTag:    systemUpdatePtr("v1.5.8"),
		Changelog: []string{},
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !result.RepositoryAvailable || result.RepositoryError != nil {
		t.Fatalf("repository flags = %+v", result)
	}
	if result.Latest == nil || result.Latest.Version != "v2.0.0" {
		t.Fatalf("latest = %+v", result.Latest)
	}
	if len(result.Updates) != 1 || result.Updates[0].Version != "v2.0.0" {
		t.Fatalf("updates = %+v", result.Updates)
	}
	ota := result.Updates[0].OTA
	if !ota.ManifestAvailable || ota.Error != nil {
		t.Fatalf("ota = %+v", ota)
	}
	if len(ota.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v", ota.Artifacts)
	}
	artifact := ota.Artifacts[0]
	if artifact.Sha256 != sha {
		t.Fatalf("sha256 = %q, want lower-cased %q", artifact.Sha256, sha)
	}
	if artifact.Size == nil || *artifact.Size != 1024 {
		t.Fatalf("size = %v", artifact.Size)
	}

	// The release token only ever goes to a trusted host.
	if len(doer.requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(doer.requests))
	}
	for _, request := range doer.requests {
		if !systemUpdateIsTrustedReleaseHost(request.URL.String()) {
			t.Fatalf("request to untrusted host %s", request.URL.String())
		}
		if request.Header.Get("authorization") != "Bearer ghp_test_token" {
			t.Fatalf("missing release token on %s", request.URL.String())
		}
		if request.Header.Get("accept") != "application/vnd.github+json" {
			t.Fatalf("accept header = %q", request.Header.Get("accept"))
		}
	}
}

// TestSystemUpdateGitHubCheckerUnavailable pins the Releases-API failure branch:
// a nil tag list becomes repositoryAvailable=false with Node's literal message.
func TestSystemUpdateGitHubCheckerUnavailable(t *testing.T) {
	doer := &systemUpdateFakeHTTPDoer{routes: map[string]string{}}
	checker := systemUpdateNewGitHubChecker(doer)

	result, err := checker.Check(context.Background(), "/nonexistent", systemUpdateVersionMetadata{
		Version:   "v1.5.8",
		Changelog: []string{},
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result.RepositoryAvailable {
		t.Fatal("repositoryAvailable must be false")
	}
	if result.RepositoryError == nil || *result.RepositoryError != systemUpdateGitHubUnavailableMessage {
		t.Fatalf("repositoryError = %v", result.RepositoryError)
	}
}

// TestSystemUpdateCheckRejectedForNonManagerAdmin shows /check still answers 200
// with canManageUpdates=false for an administrator outside the allowlist; unlike
// /logs it does not 403, matching Node.
func TestSystemUpdateCheckRejectedForNonManagerAdmin(t *testing.T) {
	t.Setenv("JWT_SECRET", "system-update-test-secret")
	authInstallFakeDB(t, &authFakeDBData{
		user: &authFakeUser{username: "ops", role: "admin", status: "active"},
	})
	systemUpdateInstallAllowlist(t, 999)
	systemUpdateInstallChecker(t, &systemUpdateRecordingChecker{response: systemUpdateCheckResult{
		Current: systemUpdateVersionMetadata{Version: "v1.5.8", Changelog: []string{}},
		Updates: []systemUpdateAvailableUpdate{},
	}})

	token, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID: 2, Username: "ops", Role: "admin", Status: "active", SID: "ops-session",
	})
	if err != nil {
		t.Fatalf("sign ops token: %v", err)
	}

	recorder := systemUpdateDo(systemUpdateRouter(t), http.MethodGet, "/api/admin/system-update/check", token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if payload := systemUpdateDecode(t, recorder); payload["canManageUpdates"] != false {
		t.Fatalf("canManageUpdates = %v, want false", payload["canManageUpdates"])
	}
}
