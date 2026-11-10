package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// ==================== shared helpers ====================

// backupsTestEngine builds an engine with only this module mounted.
func backupsTestEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterBackups(engine)
	return engine
}

// backupsTestCall replays one request against an engine.
func backupsTestCall(engine *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

// backupsTestHandlerContext builds a gin context with claims already attached, so a
// handler can be exercised without the auth middleware (which needs a database).
func backupsTestHandlerContext(t *testing.T, method, path, body string, claims *lib.AccessTokenClaims) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	c.Request = httptest.NewRequest(method, path, reader)
	if body != "" {
		c.Request.Header.Set("Content-Type", "application/json")
	}
	if claims != nil {
		c.Set(middleware.ContextUserKey, claims)
	}
	return c, recorder
}

// backupsTestDecode unmarshals a JSON response body.
func backupsTestDecode(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response is not a JSON object: %v (%s)", err, recorder.Body.String())
	}
	return decoded
}

// backupsTestErrorCode returns the `code` field of an error envelope.
func backupsTestErrorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	decoded := backupsTestDecode(t, recorder)
	code, _ := decoded["code"].(string)
	return code
}

// ==================== route inventory ====================

// TestBackupsRouteInventory pins the 17 handlers of routes/backups.ts. The
// TypeScript file declares 17 (the task sheet listed 15 and omitted the export
// creation route and mislabelled POST /restore/:backupId); the source is
// authoritative. Any added/removed route must update this table.
func TestBackupsRouteInventory(t *testing.T) {
	engine := backupsTestEngine(t)

	want := map[string]bool{
		"GET /api/instances/:id/backups":                                false,
		"POST /api/instances/:id/backups":                               false,
		"DELETE /api/instances/:id/backups/:backupId":                   false,
		"GET /api/instances/:id/backup-policy":                          false,
		"PUT /api/instances/:id/backup-policy":                          false,
		"POST /api/instances/:id/backups/:backupId/export":              false,
		"GET /api/instances/:id/backups/export/:taskId/status":          false,
		"POST /api/instances/:id/backups/export/:taskId/download-token": false,
		"GET /api/instances/:id/backups/export/:taskId/download":        false,
		"POST /api/instances/:id/restore/:taskId":                       false,
		"GET /api/instances/:id/restore/:taskId":                        false,
		"POST /api/instances/:id/restore/:taskId/rollback":              false,
		"DELETE /api/instances/:id/restore/:taskId":                     false,
		"POST /api/instances/:id/backups/:backupId/upload-remote":       false,
		"GET /api/instances/:id/upload-tasks/:taskId":                   false,
		"DELETE /api/instances/:id/upload-tasks/:taskId":                false,
		"GET /api/instances/:id/upload-tasks/active":                    false,
	}

	count := 0
	for _, route := range engine.Routes() {
		if !strings.HasPrefix(route.Path, "/api/instances") {
			continue
		}
		count++
		key := route.Method + " " + route.Path
		if _, known := want[key]; known {
			want[key] = true
		}
	}
	for key, seen := range want {
		if !seen {
			t.Fatalf("route %s is not registered", key)
		}
	}
	if count != len(want) {
		t.Fatalf("backup route count = %d, want %d", count, len(want))
	}

	// The public download route must NOT sit behind an authentication middleware:
	// an unauthenticated request with no token is answered by the token check
	// (DOWNLOAD_TOKEN_REQUIRED), not by 401 Unauthorized.
	recorder := backupsTestCall(engine, http.MethodGet, "/api/instances/1/backups/export/x/download", "", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("public download status = %d, want 401", recorder.Code)
	}
	if got := backupsTestErrorCode(t, recorder); got != "DOWNLOAD_TOKEN_REQUIRED" {
		t.Fatalf("public download code = %q, want DOWNLOAD_TOKEN_REQUIRED", got)
	}
}

// ==================== pure helpers ====================

func TestBackupsParseID(t *testing.T) {
	valid := map[string]int32{"1": 1, "940001": 940001, "2147483647": 2147483647}
	for raw, want := range valid {
		got, ok := backupsParseID(raw)
		if !ok || got != want {
			t.Errorf("backupsParseID(%q) = (%d,%v), want (%d,true)", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "0", "01", "-1", "1.5", "abc", " 1", "2147483648", "9007199254740992"} {
		if _, ok := backupsParseID(raw); ok {
			t.Errorf("backupsParseID(%q) accepted an invalid id", raw)
		}
	}
}

func TestBackupsIncusName(t *testing.T) {
	cases := map[string]string{
		"daily backup": "backup-daily-backup-1730000000123",
		"a:b/c.d":      "backup-a-b-c-d-1730000000123",
		"1-2_3":        "backup-1-2_3-1730000000123",
	}
	for input, want := range cases {
		if got := backupsIncusName(input, 1730000000123); got != want {
			t.Errorf("backupsIncusName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBackupsTempInstanceName(t *testing.T) {
	got := backupsTempInstanceName("parity-billing-920001", 1730000000123)
	if got != "parity-billing-920001-restore-1730000000123" {
		t.Fatalf("backupsTempInstanceName = %q", got)
	}
}

func TestBackupsRandomUUID(t *testing.T) {
	first := backupsRandomUUID()
	second := backupsRandomUUID()
	if first == second {
		t.Fatal("backupsRandomUUID returned the same value twice")
	}
	if len(first) != 36 || strings.Count(first, "-") != 4 {
		t.Fatalf("uuid shape = %q", first)
	}
	if first[14] != '4' {
		t.Fatalf("uuid version = %c, want 4 (%s)", first[14], first)
	}
	if !strings.ContainsRune("89ab", rune(first[19])) {
		t.Fatalf("uuid variant = %c, want 8/9/a/b (%s)", first[19], first)
	}
}

func TestBackupsDownloadTokenResourceID(t *testing.T) {
	if got := backupsDownloadTokenResourceID(940001, "task-1"); got != "940001:task-1" {
		t.Fatalf("resource id = %q", got)
	}
}

func TestBackupsEncodeURIComponent(t *testing.T) {
	cases := map[string]string{
		"alice-instance-backup-20231207-153045.tar.gz": "alice-instance-backup-20231207-153045.tar.gz",
		"a b":       "a%20b",
		`a"b`:       "a%22b",
		"a/b:c":     "a%2Fb%3Ac",
		"日本":        "%E6%97%A5%E6%9C%AC",
		"-_.!~*'()": "-_.!~*'()",
	}
	for input, want := range cases {
		if got := backupsEncodeURIComponent(input); got != want {
			t.Errorf("backupsEncodeURIComponent(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBackupsBackupDateString(t *testing.T) {
	value := time.Date(2023, 12, 7, 15, 30, 45, 123000000, time.UTC)
	if got := backupsBackupDateString(value); got != "20231207-153045" {
		t.Fatalf("date string = %q, want 20231207-153045", got)
	}
	// A non-UTC input is rendered in UTC, like toISOString().
	zone := time.FixedZone("UTC+8", 8*3600)
	local := time.Date(2023, 12, 7, 23, 30, 45, 0, zone)
	if got := backupsBackupDateString(local); got != "20231207-153045" {
		t.Fatalf("date string from UTC+8 = %q, want 20231207-153045", got)
	}
}

func TestBackupsDurationSeconds(t *testing.T) {
	started := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	finished := started.Add(90*time.Second + 500*time.Millisecond)
	now := started.Add(10 * time.Second)

	if got := backupsDurationSeconds(&started, &finished, now); got == nil || *got != 90 {
		t.Fatalf("finished duration = %v, want 90", got)
	}
	if got := backupsDurationSeconds(&started, nil, now); got == nil || *got != 10 {
		t.Fatalf("running duration = %v, want 10", got)
	}
	if got := backupsDurationSeconds(nil, &finished, now); got != nil {
		t.Fatalf("missing start duration = %v, want nil", got)
	}
	// Math.floor semantics for a negative remainder.
	if got := backupsDurationSeconds(&finished, &started, now); got == nil || *got != -91 {
		t.Fatalf("negative duration = %v, want -91", got)
	}
}

func TestBackupsPolicyView(t *testing.T) {
	view := backupsPolicyView(nil)
	if view.Enabled || view.IntervalMinutes != 1440 || view.LastRunAt != nil || view.NextRunAt != nil {
		t.Fatalf("nil policy view = %+v", view)
	}

	lastRun := time.Date(2024, 2, 3, 4, 5, 6, 7000000, time.UTC)
	nextRun := lastRun.Add(24 * time.Hour)
	view = backupsPolicyView(&db.BackupsPolicy{
		Enabled:         true,
		IntervalMinutes: 0, // `intervalMinutes || 1440`
		LastRunAt:       &lastRun,
		NextRunAt:       &nextRun,
	})
	if !view.Enabled || view.IntervalMinutes != 1440 {
		t.Fatalf("zero-interval view = %+v", view)
	}
	if view.LastRunAt == nil || *view.LastRunAt != "2024-02-03T04:05:06.007Z" {
		t.Fatalf("last_run_at = %v", view.LastRunAt)
	}
	if view.NextRunAt == nil || *view.NextRunAt != "2024-02-04T04:05:06.007Z" {
		t.Fatalf("next_run_at = %v", view.NextRunAt)
	}
}

func TestBackupsPolicyIntervalEnum(t *testing.T) {
	for _, allowed := range []float64{60, 360, 1440, 4320} {
		if !backupsPolicyIntervalAllowed(allowed) {
			t.Errorf("%v should be allowed", allowed)
		}
	}
	for _, rejected := range []float64{0, 5, 59, 61, 1441, 4321} {
		if backupsPolicyIntervalAllowed(rejected) {
			t.Errorf("%v should be rejected", rejected)
		}
	}
}

// ==================== AJV validation ====================

// backupsTestValidate runs the shared AJV replica and returns the first message.
func backupsTestValidate(t *testing.T, raw string, required []string, fields []ajvFieldSpec) (ajvValidated, string) {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("test payload is not JSON: %v", err)
	}
	return validateAJVObject(decoded, required, fields)
}

func TestBackupsCreateAJVValidation(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{}`, "name is required"},
		{`{"description":"d"}`, "name is required"},
		{`{"name":""}`, "name is too short"},
		{`{"name":"` + strings.Repeat("a", 51) + `"}`, "name is too long"},
		{`{"name":"ok","description":"` + strings.Repeat("d", 201) + `"}`, "description is too long"},
		{`[]`, "must be object"},
		{`null`, "must be object"},
		{`{"name":["ok"]}`, ""},                 // coerceTypes:'array' unwraps
		{`{"name":123}`, ""},                    // number → string
		{`{"name":"ok","unknown":{"a":1}}`, ""}, // no additionalProperties:false
	}
	for _, tc := range cases {
		_, message := backupsTestValidate(t, tc.body, []string{"name"}, backupsCreateFields)
		if message != tc.want {
			t.Errorf("create %s: message = %q, want %q", tc.body, message, tc.want)
		}
	}

	// Boundary values are accepted.
	for _, body := range []string{
		`{"name":"a"}`,
		`{"name":"` + strings.Repeat("a", 50) + `"}`,
		`{"name":"ok","description":"` + strings.Repeat("d", 200) + `"}`,
	} {
		if _, message := backupsTestValidate(t, body, []string{"name"}, backupsCreateFields); message != "" {
			t.Errorf("create %s should validate, got %q", body, message)
		}
	}
}

func TestBackupsPolicyAJVValidation(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{}`, "enabled is required"},
		{`{"enabled":"yes"}`, "must be boolean"},
		{`{"enabled":true,"intervalMinutes":10.5}`, "must be integer"},
		{`{"enabled":true,"intervalMinutes":"abc"}`, "must be integer"},
		{`[]`, "must be object"},
		{`{"enabled":"true"}`, ""}, // AJV coerces the string
		{`{"enabled":1}`, ""},      // 1 → true
		{`{"enabled":[true]}`, ""}, // unwrap
		{`{"enabled":true,"intervalMinutes":1440.0}`, ""},
	}
	for _, tc := range cases {
		_, message := backupsTestValidate(t, tc.body, []string{"enabled"}, backupsPolicyFields)
		if message != tc.want {
			t.Errorf("policy %s: message = %q, want %q", tc.body, message, tc.want)
		}
	}
}

func TestBackupsUploadRemoteAJVValidation(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{}`, ""},
		{`{"storageConfigId":940001}`, ""},
		{`{"storageConfigId":"940001"}`, ""}, // numeric string is coerced
		{`{"storageConfigId":10.5}`, "must be integer"},
		{`{"storageConfigId":"abc"}`, "must be integer"},
		{`{"storageConfigId":null}`, ""}, // null coerces to 0 (falsy → default config)
		{`[]`, "must be object"},
	}
	for _, tc := range cases {
		_, message := backupsTestValidate(t, tc.body, nil, backupsUploadRemoteFields)
		if message != tc.want {
			t.Errorf("upload %s: message = %q, want %q", tc.body, message, tc.want)
		}
	}
}

// The export/restore bodies use `{ type: 'object', properties: {} }`: the object
// type is enforced, nothing else.
func TestBackupsObjectOnlyBodySchemas(t *testing.T) {
	for _, body := range []string{`[]`, `null`, `"text"`, `1`} {
		if _, message := backupsTestValidate(t, body, nil, nil); message != "must be object" {
			t.Errorf("object-only %s: message = %q, want must be object", body, message)
		}
	}
	for _, body := range []string{`{}`, `{"anything":true}`} {
		if _, message := backupsTestValidate(t, body, nil, nil); message != "" {
			t.Errorf("object-only %s should validate, got %q", body, message)
		}
	}
}

// ==================== handler-direct: invalid ids / body ordering ====================

// TestBackupsHandlersRejectInvalidID exercises every handler with an invalid route
// id. None of these paths touch the database, so the test runs without one.
func TestBackupsHandlersRejectInvalidID(t *testing.T) {
	user := &lib.AccessTokenClaims{ID: 2, Role: "user", Username: "parityuser", Status: "active"}

	cases := []struct {
		name    string
		handler gin.HandlerFunc
		method  string
		path    string
		body    string
	}{
		{"list", backupsListHandler, http.MethodGet, "/api/instances/0/backups", ""},
		{"create", backupsCreateHandler, http.MethodPost, "/api/instances/0/backups", `{"name":"x"}`},
		{"delete", backupsDeleteHandler, http.MethodDelete, "/api/instances/0/backups/1", ""},
		{"policy-get", backupsGetPolicyHandler, http.MethodGet, "/api/instances/0/backup-policy", ""},
		{"policy-put", backupsPutPolicyHandler, http.MethodPut, "/api/instances/0/backup-policy", `{"enabled":false}`},
		{"export", backupsCreateExportHandler, http.MethodPost, "/api/instances/0/backups/1/export", `{}`},
		{"export-status", backupsExportStatusHandler, http.MethodGet, "/api/instances/0/backups/export/t/status", ""},
		{"download-token", backupsDownloadTokenHandler, http.MethodPost, "/api/instances/0/backups/export/t/download-token", ""},
		{"restore-create", backupsCreateRestoreHandler, http.MethodPost, "/api/instances/0/restore/1", `{}`},
		{"restore-get", backupsGetRestoreHandler, http.MethodGet, "/api/instances/0/restore/1", ""},
		{"rollback", backupsRollbackRestoreHandler, http.MethodPost, "/api/instances/0/restore/1/rollback", ""},
		{"restore-cancel", backupsCancelRestoreHandler, http.MethodDelete, "/api/instances/0/restore/1", ""},
		{"upload-remote", backupsUploadRemoteHandler, http.MethodPost, "/api/instances/0/backups/1/upload-remote", `{}`},
		{"upload-get", backupsGetUploadTaskHandler, http.MethodGet, "/api/instances/0/upload-tasks/1", ""},
		{"upload-cancel", backupsCancelUploadTaskHandler, http.MethodDelete, "/api/instances/0/upload-tasks/1", ""},
		{"upload-active", backupsActiveUploadTaskHandler, http.MethodGet, "/api/instances/0/upload-tasks/active", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := backupsTestHandlerContext(t, tc.method, tc.path, tc.body, user)
			// Route params are normally filled by gin; set them explicitly.
			c.Params = gin.Params{
				{Key: "id", Value: "0"},
				{Key: "backupId", Value: "1"},
				{Key: "taskId", Value: "1"},
			}
			tc.handler(c)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("%s status = %d, want 400 (%s)", tc.name, recorder.Code, recorder.Body.String())
			}
			if code := backupsTestErrorCode(t, recorder); code != "INVALID_ID" {
				t.Fatalf("%s code = %q, want INVALID_ID", tc.name, code)
			}
		})
	}
}

// TestBackupsBodyValidationRunsBeforeID reproduces Fastify's ordering: the body is
// parsed and validated before the handler looks at route params. A malformed body
// on an invalid-id URL therefore reports the body error.
func TestBackupsBodyValidationRunsBeforeID(t *testing.T) {
	user := &lib.AccessTokenClaims{ID: 2, Role: "user", Username: "parityuser", Status: "active"}

	cases := []struct {
		name    string
		handler gin.HandlerFunc
		method  string
		path    string
		body    string
		want    string
	}{
		{"create-missing-name", backupsCreateHandler, http.MethodPost, "/api/instances/0/backups", `{}`, "name is required"},
		{"policy-missing-enabled", backupsPutPolicyHandler, http.MethodPut, "/api/instances/0/backup-policy", `{}`, "enabled is required"},
		{"policy-bad-enum", backupsPutPolicyHandler, http.MethodPut, "/api/instances/0/backup-policy", `{"enabled":true,"intervalMinutes":5}`, "must be equal to one of the allowed values"},
		{"export-non-object", backupsCreateExportHandler, http.MethodPost, "/api/instances/0/backups/1/export", `[]`, "must be object"},
		{"restore-non-object", backupsCreateRestoreHandler, http.MethodPost, "/api/instances/0/restore/1", `[]`, "must be object"},
		{"upload-bad-integer", backupsUploadRemoteHandler, http.MethodPost, "/api/instances/0/backups/1/upload-remote", `{"storageConfigId":1.5}`, "must be integer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, recorder := backupsTestHandlerContext(t, tc.method, tc.path, tc.body, user)
			tc.handler(c)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
			}
			decoded := backupsTestDecode(t, recorder)
			if decoded["error"] != tc.want {
				t.Fatalf("error = %v, want %q", decoded["error"], tc.want)
			}
			if decoded["code"] != "VALIDATION_ERROR" {
				t.Fatalf("code = %v, want VALIDATION_ERROR", decoded["code"])
			}
		})
	}
}

// ==================== download token ====================

// TestBackupsDownloadTokenHTTPBranches covers the unauthenticated branches of the
// public download endpoint without touching the database.
func TestBackupsDownloadTokenHTTPBranches(t *testing.T) {
	engine := backupsTestEngine(t)

	recorder := backupsTestCall(engine, http.MethodGet, "/api/instances/940001/backups/export/t/download", "", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing dt status = %d, want 401", recorder.Code)
	}
	decoded := backupsTestDecode(t, recorder)
	if decoded["code"] != "DOWNLOAD_TOKEN_REQUIRED" ||
		decoded["error"] != "Download token required. Use POST /download-token to generate one." {
		t.Fatalf("missing dt body = %v", decoded)
	}

	recorder = backupsTestCall(engine, http.MethodGet, "/api/instances/940001/backups/export/t/download?dt=bogus", "", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("invalid dt status = %d, want 401", recorder.Code)
	}
	decoded = backupsTestDecode(t, recorder)
	if decoded["code"] != "DOWNLOAD_TOKEN_INVALID" || decoded["error"] != "Token not found or already used" {
		t.Fatalf("invalid dt body = %v", decoded)
	}

	// An invalid instance id is answered after the token-presence check (Fastify's
	// onRequest order: dt first, then parsePositiveRouteId) but before consuming.
	recorder = backupsTestCall(engine, http.MethodGet, "/api/instances/0/backups/export/t/download?dt=bogus", "", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d, want 400", recorder.Code)
	}
	if code := backupsTestErrorCode(t, recorder); code != "INVALID_ID" {
		t.Fatalf("invalid id code = %q", code)
	}
}

// TestBackupsDownloadTokenSingleUse pins the one-time contract at the store level:
// the first consume succeeds, the second reports the token as gone, and the token is
// bound to both the instance and the task.
func TestBackupsDownloadTokenSingleUse(t *testing.T) {
	resourceID := backupsDownloadTokenResourceID(940001, "task-abc")
	token := lib.GenerateDownloadToken(2, resourceID, lib.DownloadTokenResourceBackupExport, 300, 1)
	if token == "" {
		t.Fatal("GenerateDownloadToken returned an empty token")
	}

	if result := lib.ConsumeDownloadToken(token, backupsDownloadTokenResourceID(940002, "task-abc"), lib.DownloadTokenResourceBackupExport); result.Valid {
		t.Fatal("a token bound to instance 940001 must not authorize instance 940002")
	}
	// The mismatch does not consume the token; it is still valid for its own task.
	if result := lib.ConsumeDownloadToken(token, resourceID, lib.DownloadTokenResourceBackupExport); !result.Valid || result.UserID != 2 {
		t.Fatalf("first consume = %+v, want valid for user 2", result)
	}
	if result := lib.ConsumeDownloadToken(token, resourceID, lib.DownloadTokenResourceBackupExport); result.Valid {
		t.Fatal("a one-time token must be rejected on the second consume")
	} else if result.Error != "Token not found or already used" {
		t.Fatalf("second consume error = %q", result.Error)
	}
}

// TestBackupsDownloadTokenExpiry pins the "Token expired" branch and that the
// expired entry is dropped.
func TestBackupsDownloadTokenExpiry(t *testing.T) {
	resourceID := backupsDownloadTokenResourceID(940001, "task-expired")
	token := lib.GenerateDownloadToken(2, resourceID, lib.DownloadTokenResourceBackupExport, -1, 1)
	result := lib.ConsumeDownloadToken(token, resourceID, lib.DownloadTokenResourceBackupExport)
	if result.Valid || result.Error != "Token expired" {
		t.Fatalf("expired token result = %+v, want Token expired", result)
	}
}

// ==================== seams ====================

// TestBackupsSeamsFailClosed proves every unported Incus component returns an
// explicit error naming the missing module (never a silent success).
func TestBackupsSeamsFailClosed(t *testing.T) {
	ctx := context.Background()

	if _, _, err := BackupsOpenExportStream(ctx, nil, "instance", "backup"); err == nil {
		t.Fatal("BackupsOpenExportStream must fail closed")
	} else if !strings.Contains(err.Error(), "seam") {
		t.Fatalf("stream seam error = %v", err)
	}
	if exists, err := BackupsInstanceExists(ctx, nil, "instance"); err == nil || exists {
		t.Fatalf("BackupsInstanceExists = (%v,%v), want an explicit error", exists, err)
	}
	if err := BackupsStartInstance(ctx, nil, "instance"); err == nil {
		t.Fatal("BackupsStartInstance must fail closed")
	}
	if err := BackupsDeleteInstance(ctx, nil, "instance"); err == nil {
		t.Fatal("BackupsDeleteInstance must fail closed")
	}
}

// ==================== unauthenticated ====================

func TestBackupsUnauthenticated(t *testing.T) {
	engine := backupsTestEngine(t)

	type request struct {
		method string
		path   string
		body   string
	}
	requests := []request{
		{http.MethodGet, "/api/instances/1/backups", ""},
		{http.MethodPost, "/api/instances/1/backups", `{"name":"x"}`},
		{http.MethodDelete, "/api/instances/1/backups/1", ""},
		{http.MethodGet, "/api/instances/1/backup-policy", ""},
		{http.MethodPut, "/api/instances/1/backup-policy", `{"enabled":false}`},
		{http.MethodPost, "/api/instances/1/backups/1/export", `{}`},
		{http.MethodGet, "/api/instances/1/backups/export/t/status", ""},
		{http.MethodPost, "/api/instances/1/backups/export/t/download-token", ""},
		{http.MethodPost, "/api/instances/1/restore/1", `{}`},
		{http.MethodGet, "/api/instances/1/restore/1", ""},
		{http.MethodPost, "/api/instances/1/restore/1/rollback", ""},
		{http.MethodDelete, "/api/instances/1/restore/1", ""},
		{http.MethodPost, "/api/instances/1/backups/1/upload-remote", `{}`},
		{http.MethodGet, "/api/instances/1/upload-tasks/1", ""},
		{http.MethodDelete, "/api/instances/1/upload-tasks/1", ""},
		{http.MethodGet, "/api/instances/1/upload-tasks/active", ""},
	}
	for _, tc := range requests {
		recorder := backupsTestCall(engine, tc.method, tc.path, "", tc.body)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want 401 (%s)", tc.method, tc.path, recorder.Code, recorder.Body.String())
		}
		if decoded := backupsTestDecode(t, recorder); decoded["error"] != "Unauthorized" {
			t.Fatalf("%s %s body = %v, want Unauthorized", tc.method, tc.path, decoded)
		}
	}
}

// ==================== database-backed tests ====================

// backupsTestDatabase initialises db against the opt-in test database once per
// process. The gate mirrors src/db/packages_test.go: an explicit env flag, a local
// host and a database name containing "_test" (so a parity/production DSN can never
// be picked up by accident).
func backupsTestDatabase(t *testing.T) bool {
	t.Helper()
	if os.Getenv("PAYINCUS_GO_DB_TESTS") != "1" {
		t.Skip("set PAYINCUS_GO_DB_TESTS=1 and DATABASE_URL to run the database-backed backup tests")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set")
	}
	lower := strings.ToLower(dsn)
	if !strings.Contains(lower, "@127.0.0.1") && !strings.Contains(lower, "@localhost") {
		t.Fatalf("refusing to run against a non-local database: %s", dsn)
	}
	if !strings.Contains(lower, "_test") {
		t.Fatalf("refusing to run against a database whose name does not contain \"_test\": %s", dsn)
	}

	backupsTestDBOnce.Do(func() {
		backupsTestDBErr = db.Init(context.Background())
	})
	if backupsTestDBErr != nil {
		t.Fatalf("db.Init: %v", backupsTestDBErr)
	}
	return true
}

var (
	backupsTestDBOnce sync.Once
	backupsTestDBErr  error
)

// backupsTestUserIDs resolves the two conformance identities by name (never by a
// hard-coded id).
func backupsTestUserIDs(t *testing.T) (adminID, userID int32) {
	t.Helper()
	for _, row := range []struct {
		username string
		target   *int32
	}{
		{"admin", &adminID},
		{"parityuser", &userID},
	} {
		if err := db.DB.Raw("SELECT id FROM users WHERE username = ?", row.username).Scan(row.target).Error; err != nil {
			t.Fatalf("load user %s: %v", row.username, err)
		}
		if *row.target == 0 {
			t.Skipf("user %s is missing from the test database", row.username)
		}
	}
	return adminID, userID
}

// backupsTestToken signs a Web JWT for a user row.
func backupsTestToken(t *testing.T, userID int32, username, role string) string {
	t.Helper()
	t.Setenv("JWT_SECRET", "backups-test-secret")
	token, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID:       userID,
		Username: username,
		Role:     role,
		Status:   "active",
	})
	if err != nil {
		t.Fatalf("SignAccessToken: %v", err)
	}
	return token
}

// backupsTestFixture inserts the rows the database-backed tests read and removes
// them again. Ids live in the 9408xx range (the conformance fixtures use 9400xx and
// the other waves 900001/920001/930001) so concurrent fixtures never collide.
type backupsTestFixture struct {
	HostID           int32
	OwnerInstance    int32
	AdminInstance    int32
	IdleInstance     int32
	QueueInstance    int32
	ReadyBackup      int32
	QueueBackup      int32
	CreatingBackup   int32
	DeletedBackup    int32
	PendingRestore   int32
	CompletedRestore int32
	FailedRestore    int32
	StorageConfig    int32
	PendingUpload    int32
	CompletedUpload  int32
	ProcessingUpload int32
}

func backupsTestInsertFixture(t *testing.T) backupsTestFixture {
	t.Helper()
	adminID, userID := backupsTestUserIDs(t)

	fixture := backupsTestFixture{
		HostID:           940801,
		OwnerInstance:    940801,
		AdminInstance:    940802,
		IdleInstance:     940803,
		QueueInstance:    940804,
		ReadyBackup:      940801,
		QueueBackup:      940806,
		CreatingBackup:   940802,
		DeletedBackup:    940803,
		PendingRestore:   940801,
		CompletedRestore: 940802,
		FailedRestore:    940803,
		StorageConfig:    940801,
		PendingUpload:    940801,
		CompletedUpload:  940802,
		ProcessingUpload: 940803,
	}

	cleanup := func() {
		for _, statement := range []string{
			"DELETE FROM backup_upload_tasks WHERE id BETWEEN 940801 AND 940899",
			"DELETE FROM restore_tasks WHERE id BETWEEN 940801 AND 940899",
			"DELETE FROM backup_policies WHERE instance_id BETWEEN 940801 AND 940899",
			`DELETE FROM backups WHERE instance_id BETWEEN 940801 AND 940899
			 OR id BETWEEN 940801 AND 940899`,
			"DELETE FROM storage_configs WHERE id BETWEEN 940801 AND 940899",
			"DELETE FROM instances WHERE id BETWEEN 940801 AND 940899",
			"DELETE FROM hosts WHERE id BETWEEN 940801 AND 940899",
		} {
			if err := db.DB.Exec(statement).Error; err != nil {
				t.Logf("fixture cleanup %q: %v", statement, err)
			}
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	now := time.Now().UTC()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO hosts (id, user_id, name, url, status, cpu_used, memory_used, disk_used,
			cpu_allowance_max, memory_max, storage_size, storage_driver, storage_type,
			created_at, updated_at)
			VALUES (?, ?, 'backups-test-host', 'https://127.0.0.1:1', 'online', 0, 0, 0,
			10000, 1048576, 1000, 'zfs', 'loop', ?, ?)`,
			[]any{fixture.HostID, adminID, now, now}},
		{`INSERT INTO instances (id, incus_id, name, user_id, host_id, image, status,
			cpu, memory, disk, network_mode, backup_limit, snapshot_limit, port_limit,
			created_at, updated_at)
			VALUES (?, ?, 'backups-test-owner', ?, ?, 'images:debian/12', 'running',
			1, 1024, 10240, 'nat', 2, 2, 2, ?, ?)`,
			[]any{fixture.OwnerInstance, "backups-test-owner", userID, fixture.HostID, now, now}},
		{`INSERT INTO instances (id, incus_id, name, user_id, host_id, image, status,
			cpu, memory, disk, network_mode, backup_limit, snapshot_limit, port_limit,
			created_at, updated_at)
			VALUES (?, ?, 'backups-test-admin', ?, ?, 'images:debian/12', 'running',
			1, 1024, 10240, 'nat', 2, 2, 2, ?, ?)`,
			[]any{fixture.AdminInstance, "backups-test-admin", adminID, fixture.HostID, now, now}},
		{`INSERT INTO instances (id, incus_id, name, user_id, host_id, image, status,
			cpu, memory, disk, network_mode, backup_limit, snapshot_limit, port_limit,
			created_at, updated_at)
			VALUES (?, ?, 'backups-test-idle', ?, ?, 'images:debian/12', 'running',
			1, 1024, 10240, 'nat', 2, 2, 2, ?, ?)`,
			[]any{fixture.IdleInstance, "backups-test-idle", userID, fixture.HostID, now, now}},
		{`INSERT INTO instances (id, incus_id, name, user_id, host_id, image, status,
			cpu, memory, disk, network_mode, backup_limit, snapshot_limit, port_limit,
			created_at, updated_at)
			VALUES (?, ?, 'backups-test-queue', ?, ?, 'images:debian/12', 'running',
			1, 1024, 10240, 'nat', 2, 2, 2, ?, ?)`,
			[]any{fixture.QueueInstance, "backups-test-queue", userID, fixture.HostID, now, now}},
		{`INSERT INTO backups (id, instance_id, incus_name, name, description, size, status, created_at)
			VALUES (?, ?, 'backup-ready', 'ready-backup', 'fixture', 1024, 'ready', ?)`,
			[]any{fixture.ReadyBackup, fixture.OwnerInstance, now.Add(-time.Hour)}},
		{`INSERT INTO backups (id, instance_id, incus_name, name, description, size, status, created_at)
			VALUES (?, ?, 'backup-queue', 'queue-backup', NULL, 0, 'ready', ?)`,
			[]any{fixture.QueueBackup, fixture.QueueInstance, now.Add(-30 * time.Minute)}},
		{`INSERT INTO backups (id, instance_id, incus_name, name, description, size, status, created_at)
			VALUES (?, ?, 'backup-creating', 'creating-backup', NULL, 0, 'creating', ?)`,
			[]any{fixture.CreatingBackup, fixture.OwnerInstance, now}},
		{`INSERT INTO backups (id, instance_id, incus_name, name, description, size, status, created_at)
			VALUES (?, ?, 'backup-deleted', 'deleted-backup', NULL, 0, 'deleted', ?)`,
			[]any{fixture.DeletedBackup, fixture.OwnerInstance, now}},
		{`INSERT INTO backup_policies (id, instance_id, enabled, interval_minutes, next_run_at, created_at, updated_at)
			VALUES (940801, ?, true, 360, ?, ?, ?)`,
			[]any{fixture.OwnerInstance, now.Add(time.Hour), now, now}},
		{`INSERT INTO restore_tasks (id, instance_id, backup_id, host_id, user_id, status,
			temp_instance_name, original_instance_name, original_incus_id, created_at)
			VALUES (?, ?, ?, ?, ?, 'PENDING', 'temp-restore', 'backups-test-owner', 'backups-test-owner', ?)`,
			[]any{fixture.PendingRestore, fixture.OwnerInstance, fixture.ReadyBackup,
				fixture.HostID, userID, now.Add(-2 * time.Minute)}},
		{`INSERT INTO restore_tasks (id, instance_id, backup_id, host_id, user_id, status,
			temp_instance_name, original_instance_name, original_incus_id, error,
			created_at, started_at, finished_at)
			VALUES (?, ?, ?, ?, ?, 'COMPLETED', 'temp-restore', 'backups-test-owner', 'backups-test-owner', NULL,
			?, ?, ?)`,
			[]any{fixture.CompletedRestore, fixture.OwnerInstance, fixture.ReadyBackup,
				fixture.HostID, userID, now.Add(-10 * time.Minute),
				now.Add(-9 * time.Minute), now.Add(-8 * time.Minute)}},
		{`INSERT INTO restore_tasks (id, instance_id, backup_id, host_id, user_id, status,
			temp_instance_name, original_instance_name, original_incus_id, error,
			created_at, started_at, finished_at)
			VALUES (?, ?, ?, ?, ?, 'FAILED', 'temp-restore', 'backups-test-owner', 'backups-test-owner', 'boom',
			?, ?, ?)`,
			[]any{fixture.FailedRestore, fixture.OwnerInstance, fixture.ReadyBackup,
				fixture.HostID, userID, now.Add(-20 * time.Minute),
				now.Add(-19 * time.Minute), now.Add(-18 * time.Minute)}},
		// Ten extra PENDING restore tasks on the same host: the queue-length guard
		// (QUEUE_MAX_LENGTH = 10) must refuse a restore for another instance.
		{`INSERT INTO restore_tasks (id, instance_id, backup_id, host_id, user_id, status,
			temp_instance_name, original_instance_name, original_incus_id, created_at)
			SELECT g.id, ?, ?, ?, ?, 'PENDING', 'temp-restore',
			       'backups-test-owner', 'backups-test-owner',
			       ?::timestamp + (g.id - 940811) * interval '1 minute'
			  FROM generate_series(940811, 940820) AS g(id)`,
			[]any{fixture.OwnerInstance, fixture.ReadyBackup, fixture.HostID, userID,
				now.Add(-40 * time.Minute)}},
		{`INSERT INTO storage_configs (id, user_id, name, type, host, is_default, created_at, updated_at)
			VALUES (?, ?, 'backups-test-s3', 'S3', 's3.example.invalid', true, ?, ?)`,
			[]any{fixture.StorageConfig, userID, now, now}},
		{`INSERT INTO backup_upload_tasks (id, user_id, instance_id, backup_id, host_id, storage_config_id,
			status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 'PENDING', ?)`,
			[]any{fixture.PendingUpload, userID, fixture.OwnerInstance, fixture.ReadyBackup,
				fixture.HostID, fixture.StorageConfig, now.Add(-3 * time.Minute)}},
		{`INSERT INTO backup_upload_tasks (id, user_id, instance_id, backup_id, host_id, storage_config_id,
			status, remote_file_name, file_size, created_at, started_at, finished_at)
			VALUES (?, ?, ?, ?, ?, ?, 'COMPLETED', 'remote.tar.gz', 12345, ?, ?, ?)`,
			[]any{fixture.CompletedUpload, userID, fixture.OwnerInstance, fixture.ReadyBackup,
				fixture.HostID, fixture.StorageConfig, now.Add(-15 * time.Minute),
				now.Add(-14 * time.Minute), now.Add(-13 * time.Minute)}},
		{`INSERT INTO backup_upload_tasks (id, user_id, instance_id, backup_id, host_id, storage_config_id,
			status, created_at, started_at)
			VALUES (?, ?, ?, ?, ?, ?, 'PROCESSING', ?, ?)`,
			[]any{fixture.ProcessingUpload, userID, fixture.AdminInstance, fixture.ReadyBackup,
				fixture.HostID, fixture.StorageConfig, now.Add(-16 * time.Minute),
				now.Add(-16 * time.Minute)}},
	}

	for _, statement := range statements {
		if err := db.DB.Exec(statement.sql, statement.args...).Error; err != nil {
			t.Fatalf("fixture insert failed: %v\n%s", err, statement.sql)
		}
	}
	return fixture
}

// TestBackupsAuthAndOwnershipBranchesAgainstDatabase covers the auth/permission
// matrix that cannot be reached without real user/host/instance rows:
//
//	admin + authenticateUser → 403 USER_ONLY
//	non-owner user           → 403 FORBIDDEN
//	owner                    → 200
//	missing instance         → 404
func TestBackupsAuthAndOwnershipBranchesAgainstDatabase(t *testing.T) {
	backupsTestDatabase(t)
	fixture := backupsTestInsertFixture(t)
	adminID, userID := backupsTestUserIDs(t)

	engine := backupsTestEngine(t)
	adminToken := backupsTestToken(t, adminID, "admin", "admin")
	userToken := backupsTestToken(t, userID, "parityuser", "user")

	cases := []struct {
		name     string
		method   string
		path     string
		body     string
		token    string
		want     int
		wantCode string
	}{
		{"admin-is-user-only", http.MethodGet, fmt.Sprintf("/api/instances/%d/backups", fixture.OwnerInstance), "", adminToken, 403, "USER_ONLY"},
		{"owner-list", http.MethodGet, fmt.Sprintf("/api/instances/%d/backups", fixture.OwnerInstance), "", userToken, 200, ""},
		{"non-owner-list", http.MethodGet, fmt.Sprintf("/api/instances/%d/backups", fixture.AdminInstance), "", userToken, 403, "FORBIDDEN"},
		{"missing-instance", http.MethodGet, "/api/instances/999999/backups", "", userToken, 404, "INSTANCE_NOT_FOUND"},
		{"owner-policy-get", http.MethodGet, fmt.Sprintf("/api/instances/%d/backup-policy", fixture.OwnerInstance), "", userToken, 200, ""},
		{"owner-backup-delete-missing", http.MethodDelete, fmt.Sprintf("/api/instances/%d/backups/999999", fixture.OwnerInstance), "", userToken, 404, "BACKUP_NOT_FOUND"},
		{"owner-backup-delete-foreign", http.MethodDelete, fmt.Sprintf("/api/instances/%d/backups/%d", fixture.AdminInstance, fixture.ReadyBackup), "", userToken, 403, "FORBIDDEN"},
		{"non-owner-export", http.MethodPost, fmt.Sprintf("/api/instances/%d/backups/%d/export", fixture.AdminInstance, fixture.ReadyBackup), `{}`, userToken, 403, "FORBIDDEN"},
		{"missing-backup-export", http.MethodPost, fmt.Sprintf("/api/instances/%d/backups/999999/export", fixture.OwnerInstance), `{}`, userToken, 404, "BACKUP_NOT_FOUND"},
		{"export-status-missing-task", http.MethodGet, fmt.Sprintf("/api/instances/%d/backups/export/does-not-exist/status", fixture.OwnerInstance), "", userToken, 404, "EXPORT_TASK_NOT_FOUND"},
		{"download-token-missing-task", http.MethodPost, fmt.Sprintf("/api/instances/%d/backups/export/does-not-exist/download-token", fixture.OwnerInstance), "", userToken, 404, "EXPORT_TASK_NOT_FOUND"},
		{"download-token-admin-not-owner", http.MethodPost, fmt.Sprintf("/api/instances/%d/backups/export/does-not-exist/download-token", fixture.OwnerInstance), "", adminToken, 403, "FORBIDDEN"},
		{"restore-task-missing", http.MethodGet, fmt.Sprintf("/api/instances/%d/restore/999999", fixture.OwnerInstance), "", userToken, 404, ""},
		{"upload-task-missing", http.MethodGet, fmt.Sprintf("/api/instances/%d/upload-tasks/999999", fixture.OwnerInstance), "", userToken, 404, "BACKUP_UPLOAD_TASK_NOT_FOUND"},
		{"upload-active-owner", http.MethodGet, fmt.Sprintf("/api/instances/%d/upload-tasks/active", fixture.OwnerInstance), "", userToken, 200, ""},
		{"upload-active-idle", http.MethodGet, fmt.Sprintf("/api/instances/%d/upload-tasks/active", fixture.IdleInstance), "", userToken, 200, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := backupsTestCall(engine, tc.method, tc.path, tc.token, tc.body)
			if recorder.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", recorder.Code, tc.want, recorder.Body.String())
			}
			if tc.wantCode != "" {
				if code := backupsTestErrorCode(t, recorder); code != tc.wantCode {
					t.Fatalf("code = %q, want %q", code, tc.wantCode)
				}
			}
		})
	}
}

// TestBackupsListFiltersDeletedAgainstDatabase pins the projection: soft-deleted
// rows are excluded and newest-first ordering is preserved.
func TestBackupsListFiltersDeletedAgainstDatabase(t *testing.T) {
	backupsTestDatabase(t)
	fixture := backupsTestInsertFixture(t)
	_, userID := backupsTestUserIDs(t)

	engine := backupsTestEngine(t)
	token := backupsTestToken(t, userID, "parityuser", "user")
	recorder := backupsTestCall(engine, http.MethodGet,
		fmt.Sprintf("/api/instances/%d/backups", fixture.OwnerInstance), token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}

	var items []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
		t.Fatalf("list body: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("list length = %d, want 2 (deleted row must be hidden): %s", len(items), recorder.Body.String())
	}
	if items[0]["id"].(float64) != float64(fixture.CreatingBackup) {
		t.Fatalf("first item = %v, want the newest (creating) backup", items[0]["id"])
	}
	for _, item := range items {
		if item["status"] == "deleted" {
			t.Fatal("a soft-deleted backup leaked into the list")
		}
		if _, ok := item["created_at"].(string); !ok {
			t.Fatalf("created_at is not an ISO string: %v", item["created_at"])
		}
	}
}

// TestBackupsRestoreTaskReadAgainstDatabase covers the status projection, the
// deterministic duration, and the queue position.
func TestBackupsRestoreTaskReadAgainstDatabase(t *testing.T) {
	backupsTestDatabase(t)
	fixture := backupsTestInsertFixture(t)
	_, userID := backupsTestUserIDs(t)

	engine := backupsTestEngine(t)
	token := backupsTestToken(t, userID, "parityuser", "user")

	recorder := backupsTestCall(engine, http.MethodGet,
		fmt.Sprintf("/api/instances/%d/restore/%d", fixture.OwnerInstance, fixture.CompletedRestore), token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	decoded := backupsTestDecode(t, recorder)
	if decoded["status"] != "COMPLETED" {
		t.Fatalf("status = %v", decoded["status"])
	}
	if duration, ok := decoded["duration"].(float64); !ok || duration != 60 {
		t.Fatalf("duration = %v, want 60", decoded["duration"])
	}
	if decoded["queuePosition"].(float64) != 0 {
		t.Fatalf("completed task queuePosition = %v, want 0", decoded["queuePosition"])
	}
	if decoded["createdAt"] == "" || decoded["finishedAt"] == nil {
		t.Fatalf("timestamps missing: %v", decoded)
	}

	// PENDING: queue position counts the single older pending row + itself.
	recorder = backupsTestCall(engine, http.MethodGet,
		fmt.Sprintf("/api/instances/%d/restore/%d", fixture.OwnerInstance, fixture.PendingRestore), token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("pending status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	decoded = backupsTestDecode(t, recorder)
	if decoded["status"] != "PENDING" {
		t.Fatalf("pending status = %v", decoded["status"])
	}
	if decoded["duration"] != nil {
		t.Fatalf("pending duration = %v, want null", decoded["duration"])
	}
	if decoded["queuePosition"].(float64) < 1 {
		t.Fatalf("pending queuePosition = %v", decoded["queuePosition"])
	}
}

// TestBackupsRestoreQueueTooLongAgainstDatabase covers the host-wide queue guard:
// ten PENDING restore tasks on the host make the next restore answer 429 with the
// Node-formatted message.
func TestBackupsRestoreQueueTooLongAgainstDatabase(t *testing.T) {
	backupsTestDatabase(t)
	fixture := backupsTestInsertFixture(t)
	_, userID := backupsTestUserIDs(t)

	engine := backupsTestEngine(t)
	token := backupsTestToken(t, userID, "parityuser", "user")

	recorder := backupsTestCall(engine, http.MethodPost,
		fmt.Sprintf("/api/instances/%d/restore/%d", fixture.QueueInstance, fixture.QueueBackup),
		token, `{}`)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (%s)", recorder.Code, recorder.Body.String())
	}
	decoded := backupsTestDecode(t, recorder)
	if decoded["error"] != "QUEUE_TOO_LONG" {
		t.Fatalf("error = %v, want QUEUE_TOO_LONG", decoded["error"])
	}
	// 11 pending rows: the PENDING fixture plus the ten fillers.
	if message, _ := decoded["message"].(string); message != "队列已满（11/10），请稍后再试" {
		t.Fatalf("message = %q", message)
	}
}

// TestBackupsUploadRemoteActiveTaskAgainstDatabase covers the per-user active
// upload guard (409, with the existing task id).
func TestBackupsUploadRemoteActiveTaskAgainstDatabase(t *testing.T) {
	backupsTestDatabase(t)
	fixture := backupsTestInsertFixture(t)
	_, userID := backupsTestUserIDs(t)

	engine := backupsTestEngine(t)
	token := backupsTestToken(t, userID, "parityuser", "user")

	recorder := backupsTestCall(engine, http.MethodPost,
		fmt.Sprintf("/api/instances/%d/backups/%d/upload-remote", fixture.OwnerInstance, fixture.ReadyBackup),
		token, `{}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (%s)", recorder.Code, recorder.Body.String())
	}
	decoded := backupsTestDecode(t, recorder)
	if decoded["error"] != "BACKUP_UPLOAD_IN_PROGRESS" || decoded["message"] != "您有上传任务正在进行中" {
		t.Fatalf("body = %v", decoded)
	}
	if decoded["taskId"].(float64) != float64(fixture.PendingUpload) {
		t.Fatalf("taskId = %v, want %d", decoded["taskId"], fixture.PendingUpload)
	}
}

// TestBackupsUploadTaskStateMachineAgainstDatabase covers the terminal-state
// guards and the cancel transition (PENDING → FAILED with the Node error text).
func TestBackupsUploadTaskStateMachineAgainstDatabase(t *testing.T) {
	backupsTestDatabase(t)
	fixture := backupsTestInsertFixture(t)
	_, userID := backupsTestUserIDs(t)

	engine := backupsTestEngine(t)
	token := backupsTestToken(t, userID, "parityuser", "user")

	// A COMPLETED task cannot be cancelled.
	recorder := backupsTestCall(engine, http.MethodDelete,
		fmt.Sprintf("/api/instances/%d/upload-tasks/%d", fixture.OwnerInstance, fixture.CompletedUpload), token, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("completed cancel status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded := backupsTestDecode(t, recorder); decoded["error"] != "CANCEL_NOT_ALLOWED" {
		t.Fatalf("completed cancel body = %v", decoded)
	}

	// A PENDING task is cancelled: the row becomes FAILED with "用户取消".
	recorder = backupsTestCall(engine, http.MethodDelete,
		fmt.Sprintf("/api/instances/%d/upload-tasks/%d", fixture.OwnerInstance, fixture.PendingUpload), token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("pending cancel status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	decoded := backupsTestDecode(t, recorder)
	if decoded["success"] != true || decoded["message"] != "任务已取消" {
		t.Fatalf("pending cancel body = %v", decoded)
	}
	record, err := db.GetBackupUploadTaskByID(context.Background(), fixture.PendingUpload)
	if err != nil || record == nil {
		t.Fatalf("reload upload task: %v", err)
	}
	if record.Status != "FAILED" || record.Error == nil || *record.Error != "用户取消" || record.FinishedAt == nil {
		t.Fatalf("cancelled row = %+v, want FAILED/用户取消/finished_at", record)
	}

	// The second cancel is refused (no longer PENDING).
	recorder = backupsTestCall(engine, http.MethodDelete,
		fmt.Sprintf("/api/instances/%d/upload-tasks/%d", fixture.OwnerInstance, fixture.PendingUpload), token, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("second cancel status = %d, want 400", recorder.Code)
	}
}

// TestBackupsRestoreGuardBranchesAgainstDatabase covers the restore state machine
// guards: cancel only PENDING, rollback only FAILED, and the active-task 409.
func TestBackupsRestoreGuardBranchesAgainstDatabase(t *testing.T) {
	backupsTestDatabase(t)
	fixture := backupsTestInsertFixture(t)
	_, userID := backupsTestUserIDs(t)

	engine := backupsTestEngine(t)
	token := backupsTestToken(t, userID, "parityuser", "user")

	// Cancelling a COMPLETED task → CANCEL_NOT_ALLOWED.
	recorder := backupsTestCall(engine, http.MethodDelete,
		fmt.Sprintf("/api/instances/%d/restore/%d", fixture.OwnerInstance, fixture.CompletedRestore), token, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("cancel completed status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded := backupsTestDecode(t, recorder); decoded["error"] != "CANCEL_NOT_ALLOWED" {
		t.Fatalf("cancel completed body = %v", decoded)
	}

	// Rolling back a PENDING task → ROLLBACK_NOT_ALLOWED (before any Incus call).
	recorder = backupsTestCall(engine, http.MethodPost,
		fmt.Sprintf("/api/instances/%d/restore/%d/rollback", fixture.OwnerInstance, fixture.PendingRestore), token, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("rollback pending status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded := backupsTestDecode(t, recorder); decoded["error"] != "ROLLBACK_NOT_ALLOWED" {
		t.Fatalf("rollback pending body = %v", decoded)
	}

	// Rolling back the FAILED task reaches the unported Incus seam and fails
	// explicitly instead of pretending to have rolled back.
	recorder = backupsTestCall(engine, http.MethodPost,
		fmt.Sprintf("/api/instances/%d/restore/%d/rollback", fixture.OwnerInstance, fixture.FailedRestore), token, "")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("rollback failed status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	decoded := backupsTestDecode(t, recorder)
	if decoded["error"] != "ROLLBACK_FAILED" {
		t.Fatalf("rollback failed body = %v", decoded)
	}
	if message, _ := decoded["message"].(string); message == "" {
		t.Fatalf("rollback failure must carry a reason, got %v", decoded)
	}

	// A second restore request for an instance that already has an active task → 409.
	recorder = backupsTestCall(engine, http.MethodPost,
		fmt.Sprintf("/api/instances/%d/restore/%d", fixture.OwnerInstance, fixture.ReadyBackup), token, `{}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("active restore status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded := backupsTestDecode(t, recorder); decoded["error"] != "RESTORE_IN_PROGRESS" {
		t.Fatalf("active restore body = %v", decoded)
	}
}

// TestBackupsDownloadTokenRouteSingleUseAgainstDatabase drives the PUBLIC endpoint
// twice with the same token: the first request consumes it (and then fails on the
// unported streaming seam), the second is rejected as already used.
func TestBackupsDownloadTokenRouteSingleUseAgainstDatabase(t *testing.T) {
	backupsTestDatabase(t)
	fixture := backupsTestInsertFixture(t)
	_, userID := backupsTestUserIDs(t)

	engine := backupsTestEngine(t)

	taskID := "backups-test-export-task"
	lib.CreateExportTask(lib.CreateExportTaskInput{
		ID:              taskID,
		InstanceID:      fixture.OwnerInstance,
		BackupID:        fixture.ReadyBackup,
		IncusBackupName: "backup-ready",
		UserID:          userID,
		Status:          lib.ExportTaskStatusReady,
	})
	t.Cleanup(func() { lib.DeleteExportTask(taskID) })

	token := lib.GenerateDownloadToken(
		int64(userID),
		backupsDownloadTokenResourceID(fixture.OwnerInstance, taskID),
		lib.DownloadTokenResourceBackupExport,
		300,
		1,
	)
	path := fmt.Sprintf("/api/instances/%d/backups/export/%s/download?dt=%s", fixture.OwnerInstance, taskID, token)

	first := backupsTestCall(engine, http.MethodGet, path, "", "")
	if first.Code == http.StatusUnauthorized {
		t.Fatalf("first request must pass token validation, got 401 (%s)", first.Body.String())
	}
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first request should stop at the export seam with 500, got %d (%s)", first.Code, first.Body.String())
	}
	if code := backupsTestErrorCode(t, first); code != "BACKUP_EXPORT_FAILED" {
		t.Fatalf("first request code = %q, want BACKUP_EXPORT_FAILED", code)
	}

	second := backupsTestCall(engine, http.MethodGet, path, "", "")
	if second.Code != http.StatusUnauthorized {
		t.Fatalf("second request status = %d, want 401 (%s)", second.Code, second.Body.String())
	}
	decoded := backupsTestDecode(t, second)
	if decoded["code"] != "DOWNLOAD_TOKEN_INVALID" || decoded["error"] != "Token not found or already used" {
		t.Fatalf("second request body = %v", decoded)
	}
}

// TestBackupsUploadActiveTaskAgainstDatabase pins the nested `{task: ...}`
// projection, the null branch, and the owner-only boundary.
func TestBackupsUploadActiveTaskAgainstDatabase(t *testing.T) {
	backupsTestDatabase(t)
	fixture := backupsTestInsertFixture(t)
	adminID, userID := backupsTestUserIDs(t)

	engine := backupsTestEngine(t)
	token := backupsTestToken(t, userID, "parityuser", "user")

	// Idle instance: no pending/processing upload task.
	recorder := backupsTestCall(engine, http.MethodGet,
		fmt.Sprintf("/api/instances/%d/upload-tasks/active", fixture.IdleInstance), token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("active-none status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded := backupsTestDecode(t, recorder); decoded["task"] != nil {
		t.Fatalf("active-none body = %v, want {task:null}", decoded)
	}

	// Owner instance: the PENDING task is projected as the active task.
	recorder = backupsTestCall(engine, http.MethodGet,
		fmt.Sprintf("/api/instances/%d/upload-tasks/active", fixture.OwnerInstance), token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("active-owner status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	decoded := backupsTestDecode(t, recorder)
	task, ok := decoded["task"].(map[string]any)
	if !ok {
		t.Fatalf("active-owner task = %v", decoded["task"])
	}
	if task["taskId"].(float64) != float64(fixture.PendingUpload) || task["status"] != "PENDING" {
		t.Fatalf("active-owner task = %v", task)
	}
	if task["storageName"] != "backups-test-s3" || task["storageType"] != "S3" {
		t.Fatalf("active-owner storage = %v/%v", task["storageName"], task["storageType"])
	}
	if task["duration"] != nil {
		t.Fatalf("pending duration = %v, want null", task["duration"])
	}

	// The COMPLETED task projects fileSize as a decimal string and a numeric
	// duration.
	recorder = backupsTestCall(engine, http.MethodGet,
		fmt.Sprintf("/api/instances/%d/upload-tasks/%d", fixture.OwnerInstance, fixture.CompletedUpload), token, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("completed task status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	decoded = backupsTestDecode(t, recorder)
	if decoded["fileSize"] != "12345" {
		t.Fatalf("fileSize = %v (%T), want the string \"12345\"", decoded["fileSize"], decoded["fileSize"])
	}
	if decoded["duration"].(float64) != 60 {
		t.Fatalf("completed duration = %v, want 60", decoded["duration"])
	}

	// Someone else's instance is forbidden before the task lookup.
	recorder = backupsTestCall(engine, http.MethodGet,
		fmt.Sprintf("/api/instances/%d/upload-tasks/%d", fixture.AdminInstance, fixture.ProcessingUpload), token, "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-owner task status = %d (%s)", recorder.Code, recorder.Body.String())
	}

	// Admin is USER_ONLY on authenticateUser routes.
	adminToken := backupsTestToken(t, adminID, "admin", "admin")
	recorder = backupsTestCall(engine, http.MethodGet,
		fmt.Sprintf("/api/instances/%d/upload-tasks/%d", fixture.OwnerInstance, fixture.CompletedUpload), adminToken, "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("admin task status = %d (%s)", recorder.Code, recorder.Body.String())
	}
	if code := backupsTestErrorCode(t, recorder); code != "USER_ONLY" {
		t.Fatalf("admin task code = %q, want USER_ONLY", code)
	}
}
