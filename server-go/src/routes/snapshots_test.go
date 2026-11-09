package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// snapshotsDecodeBody runs a raw JSON document through the shared Fastify body
// decoder so the tests exercise the same entry point the handlers use.
func snapshotsDecodeBody(t *testing.T, raw string) *fastifyJSONBody {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("test payload is not valid JSON: %v", err)
	}
	return &fastifyJSONBody{Value: decoded, ObjectKeys: jsonTopLevelKeys([]byte(raw))}
}

// TestSnapshotsSanitizeIncusNameSegment pins the `/[^a-zA-Z0-9-_]/g` replacement.
// In a JavaScript character class `-` between `0-9` and `_` is a literal, and the
// regex walks UTF-16 code units (no `u` flag), so an emoji becomes two dashes.
func TestSnapshotsSanitizeIncusNameSegment(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"backup", "backup"},
		{"a b", "a-b"},
		{"a:b", "a-b"},
		{"a/b.c", "a-b-c"},
		{"1-2_3", "1-2_3"},
		{"a-b", "a-b"},
		{"a_b", "a_b"},
		{"日本", "--"}, // two BMP code points
		{"😀", "--"},  // one astral character == two UTF-16 units
		{"abc😀def", "abc--def"},
		{"", ""},
	}

	for _, tc := range cases {
		if got := snapshotsSanitizeIncusNameSegment(tc.input); got != tc.want {
			t.Errorf("snapshotsSanitizeIncusNameSegment(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestSnapshotsIncusName pins `snap-${sanitized}-${Date.now()}`.
func TestSnapshotsIncusName(t *testing.T) {
	got := snapshotsIncusName("my backup", int64(1730000000123))
	want := "snap-my-backup-1730000000123"
	if got != want {
		t.Fatalf("snapshotsIncusName = %q, want %q", got, want)
	}
}

// TestSnapshotsISOTime pins the wire format of every timestamp (JS toISOString).
func TestSnapshotsISOTime(t *testing.T) {
	value := time.Date(2026, 1, 2, 3, 4, 5, 678*int(time.Millisecond), time.FixedZone("UTC+8", 8*3600))
	if got := snapshotsISOTime(value); got != "2026-01-01T19:04:05.678Z" {
		t.Fatalf("snapshotsISOTime = %q", got)
	}
}

// TestSnapshotsParseID pins the strict positive-id parsing (guard invariant).
func TestSnapshotsParseID(t *testing.T) {
	valid := map[string]int32{"1": 1, "42": 42, "2147483647": 2147483647}
	for raw, want := range valid {
		got, ok := snapshotsParseID(raw)
		if !ok || got != want {
			t.Errorf("snapshotsParseID(%q) = (%d,%v), want (%d,true)", raw, got, ok, want)
		}
	}

	invalid := []string{"", "0", "01", "-1", "1.5", "abc", " 1", "1 ", "2147483648", "9007199254740992"}
	for _, raw := range invalid {
		if got, ok := snapshotsParseID(raw); ok {
			t.Errorf("snapshotsParseID(%q) = (%d,true), want invalid", raw, got)
		}
	}
}

// TestSnapshotsCreateLockKey pins the distributed-lock key namespace.
func TestSnapshotsCreateLockKey(t *testing.T) {
	if got := snapshotsCreateLockKey(77); got != "auto-policy:snapshot:77" {
		t.Fatalf("snapshotsCreateLockKey = %q", got)
	}
}

// TestSnapshotsCreateBodyValidation pins the create schema's messages and their
// order (required first, then properties in schema order, first error wins).
func TestSnapshotsCreateBodyValidation(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty object", `{}`, "name is required"},
		{"name empty", `{"name":""}`, "name is too short"},
		{"name too long", `{"name":"` + strings.Repeat("x", 51) + `"}`, "name is too long"},
		{"description too long", `{"name":"a","description":"` + strings.Repeat("d", 201) + `"}`, "description is too long"},
		{"name wrong type", `{"name":{}}`, "must be string"},
		{"name null coerces to empty", `{"name":null}`, "name is too short"},
		{"name number is stringified", `{"name":123}`, ""},
		{"description null becomes empty string", `{"name":"a","description":null}`, ""},
		{"non-object root", `[1,2]`, "must be object"},
		{"null root", `null`, "must be object"},
		{"unknown keys pass AJV", `{"name":"a","extra":true}`, ""},
		{"single-element array unwraps", `{"name":["ok"]}`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := snapshotsDecodeBody(t, tc.raw)
			validated, message := validateAJVObject(body.Value, []string{"name"}, snapshotsCreateFields)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
			if tc.want != "" {
				return
			}
			if _, present := validated.stringValue("name"); !present {
				t.Fatalf("expected name to be present after validation")
			}
		})
	}
}

// TestSnapshotsPolicyBodyValidation pins the policy PUT schema.
func TestSnapshotsPolicyBodyValidation(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		want        string
		wantEnabled bool
	}{
		{name: "missing enabled", raw: `{}`, want: "enabled is required"},
		{name: "enabled wrong type", raw: `{"enabled":"yes"}`, want: "must be boolean"},
		{name: "enabled string true coerces", raw: `{"enabled":"true"}`, wantEnabled: true},
		{name: "enabled null coerces false", raw: `{"enabled":null}`},
		{name: "interval wrong type", raw: `{"enabled":true,"intervalMinutes":"abc"}`, want: "must be integer"},
		{name: "interval fractional", raw: `{"enabled":true,"intervalMinutes":10.5}`, want: "must be integer"},
		{name: "interval string coerces", raw: `{"enabled":true,"intervalMinutes":"60"}`, wantEnabled: true},
		{name: "interval enum rejects", raw: `{"enabled":true,"intervalMinutes":5}`, want: "must be equal to one of the allowed values"},
		{name: "interval enum allows 1440", raw: `{"enabled":true,"intervalMinutes":1440}`, wantEnabled: true},
		{name: "non-object root", raw: `[]`, want: "must be object"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := snapshotsDecodeBody(t, tc.raw)
			validated, message := validateAJVObject(body.Value, []string{"enabled"}, snapshotsPolicyFields)
			// The handler applies the enum check after the type validation.
			if message == "" {
				if raw, present := validated.Values["intervalMinutes"]; present {
					number, _ := raw.(float64)
					if !snapshotsIntervalAllowed(number) {
						message = "must be equal to one of the allowed values"
					}
				}
			}
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
			if tc.want != "" {
				return
			}
			if got := validated.boolValue("enabled"); got != tc.wantEnabled {
				t.Errorf("enabled = %v, want %v", got, tc.wantEnabled)
			}
		})
	}
}

// TestSnapshotsIntervalAllowed pins the enum values.
func TestSnapshotsIntervalAllowed(t *testing.T) {
	for _, allowed := range []float64{10, 60, 360, 1440} {
		if !snapshotsIntervalAllowed(allowed) {
			t.Errorf("interval %v should be allowed", allowed)
		}
	}
	for _, rejected := range []float64{0, 1, 5, 59, 361, 1441, -10} {
		if snapshotsIntervalAllowed(rejected) {
			t.Errorf("interval %v should be rejected", rejected)
		}
	}
}

// TestSnapshotsListView pins the list JSON keys and the created_at format.
func TestSnapshotsListView(t *testing.T) {
	description := "before upgrade"
	createdAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

	view := snapshotsListView([]db.SnapshotsSnapshot{
		{ID: 3, IncusName: "snap-a-1", Name: "a", Description: &description, Stateful: true, Size: 42, CreatedAt: createdAt},
		{ID: 4, IncusName: "snap-b-2", Name: "b", Description: nil, Stateful: false, Size: 0, CreatedAt: createdAt},
	})

	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"id":3,"name":"a","incusName":"snap-a-1","description":"before upgrade","stateful":true,"size":42,"created_at":"2026-03-04T05:06:07.000Z"},` +
		`{"id":4,"name":"b","incusName":"snap-b-2","description":null,"stateful":false,"size":0,"created_at":"2026-03-04T05:06:07.000Z"}]`
	if string(encoded) != want {
		t.Fatalf("list JSON mismatch:\n got: %s\nwant: %s", encoded, want)
	}
}

// TestSnapshotsListViewEmptyIsArray pins the empty-list shape ([] not null).
func TestSnapshotsListViewEmptyIsArray(t *testing.T) {
	encoded, err := json.Marshal(snapshotsListView(nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("empty list = %s, want []", encoded)
	}
}

// TestSnapshotsPolicyView pins the default policy projection and `|| 360`.
func TestSnapshotsPolicyView(t *testing.T) {
	defaults := snapshotsPolicyView(nil)
	if defaults.Enabled || defaults.IntervalMinutes != 360 || defaults.LastRunAt != nil || defaults.NextRunAt != nil {
		t.Fatalf("nil policy view = %+v", defaults)
	}

	lastRun := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	nextRun := time.Date(2026, 5, 6, 13, 8, 9, 0, time.UTC)
	view := snapshotsPolicyView(&db.SnapshotsPolicy{
		Enabled:         true,
		IntervalMinutes: 0, // `0 || 360`
		LastRunAt:       &lastRun,
		NextRunAt:       &nextRun,
	})
	if !view.Enabled || view.IntervalMinutes != 360 {
		t.Fatalf("zero interval should fall back to 360: %+v", view)
	}
	if view.LastRunAt == nil || *view.LastRunAt != "2026-05-06T07:08:09.000Z" {
		t.Fatalf("last_run_at = %v", view.LastRunAt)
	}
	if view.NextRunAt == nil || *view.NextRunAt != "2026-05-06T13:08:09.000Z" {
		t.Fatalf("next_run_at = %v", view.NextRunAt)
	}

	encoded, err := json.Marshal(defaults)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != `{"enabled":false,"interval_minutes":360,"last_run_at":null,"next_run_at":null}` {
		t.Fatalf("default policy JSON = %s", encoded)
	}
}

// TestSnapshotsCreateResponseOmitsAbsentDescription pins JSON.stringify's
// undefined-key drop: a snapshot created without a description has no
// `description` key, while an explicit empty string is kept.
func TestSnapshotsCreateResponseOmitsAbsentDescription(t *testing.T) {
	withoutDescription, err := json.Marshal(snapshotsCreatedSnapshot{ID: 1, Name: "n", IncusName: "snap-n-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(withoutDescription) != `{"id":1,"name":"n","incusName":"snap-n-1","stateful":false}` {
		t.Fatalf("absent description = %s", withoutDescription)
	}

	empty := ""
	withEmpty, err := json.Marshal(snapshotsCreatedSnapshot{ID: 1, Name: "n", IncusName: "snap-n-1", Description: &empty})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(withEmpty) != `{"id":1,"name":"n","incusName":"snap-n-1","description":"","stateful":false}` {
		t.Fatalf("empty description = %s", withEmpty)
	}
}

// TestSnapshotsRoutesRegistered pins the mounted paths and verbs.
func TestSnapshotsRoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterSnapshots(engine)

	want := map[string]bool{
		"GET /api/instances/:id/snapshots":                      false,
		"POST /api/instances/:id/snapshots":                     false,
		"DELETE /api/instances/:id/snapshots/:snapshotId":       false,
		"POST /api/instances/:id/snapshots/:snapshotId/restore": false,
		"GET /api/instances/:id/snapshot-policy":                false,
		"PUT /api/instances/:id/snapshot-policy":                false,
	}
	for _, route := range engine.Routes() {
		key := route.Method + " " + route.Path
		if _, tracked := want[key]; tracked {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("route %s is not registered", key)
		}
	}
}

// TestSnapshotsRoutesCoexistWithInstanceRoutes guards the gin wildcard naming:
// routes/instances.go will mount /api/instances/:id/... and gin panics when two
// routes use different wildcard names at the same position.
func TestSnapshotsRoutesCoexistWithInstanceRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterSnapshots(engine)

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("mounting /api/instances/:id conflicted with the snapshot routes: %v", recovered)
		}
	}()
	engine.GET("/api/instances/:id", func(c *gin.Context) {})
	engine.DELETE("/api/instances/:id", func(c *gin.Context) {})
	engine.POST("/api/instances/:id/start", func(c *gin.Context) {})
	engine.GET("/api/instances/tasks/:taskId", func(c *gin.Context) {})
}

// TestSnapshotsAuthUser pins the claims → permission adapter.
func TestSnapshotsAuthUser(t *testing.T) {
	user := snapshotsAuthUser(&lib.AccessTokenClaims{ID: 7, Role: "user", Username: "alice"})
	if user.ID != 7 || user.Role != "user" || user.Username != "alice" {
		t.Fatalf("snapshotsAuthUser = %+v", user)
	}
}

// ==================== handler-level checks (no database) ====================

// snapshotsRunCreateHandler drives the real create handler through gin so the
// response envelope and the body-before-id ordering are asserted, not just the
// schema helper.
func snapshotsRunCreateHandler(t *testing.T, instanceID, contentType, body string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/api/instances/:id/snapshots", func(c *gin.Context) {
		c.Set(middleware.ContextUserKey, &lib.AccessTokenClaims{ID: 1, Role: "user", Username: "alice"})
		snapshotsCreateHandler(c)
	})

	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	request := httptest.NewRequest(http.MethodPost, "/api/instances/"+instanceID+"/snapshots", reader)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

// snapshotsAssertJSON compares a response body to an expected JSON object without
// depending on key order (gin.H is a map, so Go sorts its keys).
func snapshotsAssertJSON(t *testing.T, raw, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(raw), &gotValue); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("expected value is not JSON: %v (%s)", err, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("body = %s, want %s", raw, want)
	}
}

func TestSnapshotsCreateHandlerValidation(t *testing.T) {
	cases := []struct {
		name        string
		instanceID  string
		contentType string
		body        string
		wantStatus  int
		wantBody    string
	}{
		{
			name: "validation before id", instanceID: "0", contentType: "application/json", body: `{}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"name is required","code":"VALIDATION_ERROR"}`,
		},
		{
			name: "empty name", instanceID: "1", contentType: "application/json", body: `{"name":""}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"name is too short","code":"VALIDATION_ERROR"}`,
		},
		{
			name: "non-object body", instanceID: "1", contentType: "application/json", body: `[]`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"must be object","code":"VALIDATION_ERROR"}`,
		},
		{
			name: "valid body then invalid id", instanceID: "0", contentType: "application/json", body: `{"name":"ok"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"Invalid ID","code":"INVALID_ID"}`,
		},
		{
			name: "no content type no body", instanceID: "1",
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"must be object","code":"VALIDATION_ERROR"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := snapshotsRunCreateHandler(t, tc.instanceID, tc.contentType, tc.body)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", status, tc.wantStatus, body)
			}
			snapshotsAssertJSON(t, body, tc.wantBody)
		})
	}
}

// snapshotsRunPolicyHandler drives the real policy PUT handler.
func snapshotsRunPolicyHandler(t *testing.T, instanceID, body string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.PUT("/api/instances/:id/snapshot-policy", func(c *gin.Context) {
		c.Set(middleware.ContextUserKey, &lib.AccessTokenClaims{ID: 1, Role: "user", Username: "alice"})
		snapshotsPutPolicyHandler(c)
	})

	request := httptest.NewRequest(http.MethodPut, "/api/instances/"+instanceID+"/snapshot-policy", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

func TestSnapshotsPolicyHandlerValidation(t *testing.T) {
	cases := []struct {
		name       string
		instanceID string
		body       string
		wantStatus int
		wantBody   string
	}{
		{
			name: "missing enabled", instanceID: "1", body: `{}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"enabled is required","code":"VALIDATION_ERROR"}`,
		},
		{
			name: "bad interval type", instanceID: "1", body: `{"enabled":true,"intervalMinutes":10.5}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"must be integer","code":"VALIDATION_ERROR"}`,
		},
		{
			name: "bad interval enum", instanceID: "1", body: `{"enabled":true,"intervalMinutes":5}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"must be equal to one of the allowed values","code":"VALIDATION_ERROR"}`,
		},
		{
			name: "validation before id", instanceID: "0", body: `{"enabled":false}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":"Invalid ID","code":"INVALID_ID"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := snapshotsRunPolicyHandler(t, tc.instanceID, tc.body)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", status, tc.wantStatus, body)
			}
			snapshotsAssertJSON(t, body, tc.wantBody)
		})
	}
}
