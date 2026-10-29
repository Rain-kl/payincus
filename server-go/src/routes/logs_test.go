package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// TestLogsParsePositiveInteger mirrors parsePositiveInteger: only canonical
// decimal integers in [1, MAX_SAFE_INTEGER] are accepted.
func TestLogsParsePositiveInteger(t *testing.T) {
	accepted := []string{"1", "42", "9007199254740991"}
	for _, raw := range accepted {
		parsed, ok := logsParsePositiveInteger(raw)
		if !ok {
			t.Fatalf("%q must be accepted", raw)
		}
		if raw == "42" && parsed != 42 {
			t.Fatalf("parsed = %d, want 42", parsed)
		}
	}

	rejected := []string{"", "0", "01", "-1", "1.0", "1e3", "abc", " 1", "1 ", "+1", "9007199254740992"}
	for _, raw := range rejected {
		if _, ok := logsParsePositiveInteger(raw); ok {
			t.Fatalf("%q must be rejected", raw)
		}
	}
}

// TestLogsParsePositiveIntegerQuery mirrors parsePositiveIntegerQuery /
// parseClampedPositiveIntegerQuery.
func TestLogsParsePositiveIntegerQuery(t *testing.T) {
	cases := []struct {
		raw      string
		fallback int
		want     int
	}{
		{"", 1, 1},
		{"0", 1, 1},
		{"abc", 1, 1},
		{"3", 1, 3},
		{"07", 1, 1},
	}
	for _, tc := range cases {
		if got := logsParsePositiveIntegerQuery(tc.raw, tc.fallback); got != tc.want {
			t.Errorf("parsePositiveIntegerQuery(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}

	if got := logsParseClampedPositiveIntegerQuery("500", 20, 100); got != 100 {
		t.Errorf("clamped(500) = %d, want 100", got)
	}
	if got := logsParseClampedPositiveIntegerQuery("50", 20, 100); got != 50 {
		t.Errorf("clamped(50) = %d, want 50", got)
	}
	if got := logsParseClampedPositiveIntegerQuery("0", 20, 100); got != 20 {
		t.Errorf("clamped(0) = %d, want 20", got)
	}
}

// TestLogsNormalizeFilter mirrors normalizeLogFilter: trim, slice to maxLength
// (UTF-16 units), and "not provided" for an empty result.
func TestLogsNormalizeFilter(t *testing.T) {
	value, ok := logsNormalizeFilter("", false, 64)
	if !ok || value != nil {
		t.Fatalf("empty filter = %#v / %v, want nil/true", value, ok)
	}

	value, ok = logsNormalizeFilter("   ", false, 64)
	if !ok || value != nil {
		t.Fatalf("blank filter = %#v / %v, want nil/true", value, ok)
	}

	value, ok = logsNormalizeFilter("  abc  ", false, 64)
	if !ok || value == nil || *value != "abc" {
		t.Fatalf("padded filter = %#v / %v, want \"abc\"", value, ok)
	}

	// String#slice counts UTF-16 units, so 100 astral characters are cut in half
	// at maxLength 100.
	value, ok = logsNormalizeFilter(strings.Repeat("😀", 100), false, 100)
	if !ok || value == nil {
		t.Fatal("astral filter must normalize")
	}
	if got := len([]rune(*value)); got != 50 {
		t.Fatalf("astral slice kept %d runes, want 50", got)
	}

	// A duplicated query key makes the TypeScript throw (500).
	if _, ok := logsNormalizeFilter("x", true, 64); ok {
		t.Fatal("array filter value must report failure")
	}
}

// TestLogsCSVEscape mirrors csvEscape().
func TestLogsCSVEscape(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"plain", "plain"},
		{"", ""},
		{"a,b", `"a,b"`},
		{`say "hi"`, `"say ""hi"""`},
		{"line\nbreak", "\"line\nbreak\""},
		{"cr\rhere", "\"cr\rhere\""},
		{"时间", "时间"},
	}
	for _, tc := range cases {
		if got := logsCSVEscape(tc.input); got != tc.want {
			t.Errorf("csvEscape(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestLogsBuildAuditCSV pins the header row, the column order and the
// 系统 fallback for rows without a username.
func TestLogsBuildAuditCSV(t *testing.T) {
	username := "alice"
	emptyUsername := ""
	rows := []db.AuditLogRow{
		{
			ID:                   1,
			Username:             &username,
			Module:               "instance",
			Action:               "instance.create",
			Content:              `created "vm 1"`,
			Result:               db.LogResultSuccess,
			CreatedAt:            "2026-01-02T03:04:05.000Z",
			RiskLevel:            "low",
			RiskTitle:            "普通操作",
			ApprovalRequired:     false,
			VerificationRequired: false,
			BatchSensitive:       false,
		},
		{
			ID:                   2,
			Username:             nil,
			Module:               "system",
			Action:               "system.config_update",
			Content:              "a,b",
			Result:               db.LogResultSuccess,
			CreatedAt:            "2026-01-02T03:04:06.000Z",
			RiskLevel:            "high",
			RiskTitle:            "系统配置变更",
			ApprovalRequired:     true,
			VerificationRequired: true,
			BatchSensitive:       false,
		},
		{
			ID:                   3,
			Username:             &emptyUsername,
			Module:               "user",
			Action:               "user.delete",
			Content:              "x",
			Result:               db.LogResultFailed,
			CreatedAt:            "2026-01-02T03:04:07.000Z",
			RiskLevel:            "critical",
			RiskTitle:            "删除用户",
			ApprovalRequired:     true,
			VerificationRequired: true,
			BatchSensitive:       true,
		},
	}

	csv := logsBuildAuditCSV(rows)
	lines := strings.Split(csv, "\n")
	if len(lines) != 4 {
		t.Fatalf("line count = %d, want 4:\n%s", len(lines), csv)
	}

	const header = "时间,用户,模块,操作,风险等级,风险说明,需要审批,需要验证码,批量敏感,结果,内容"
	if lines[0] != header {
		t.Fatalf("header = %q, want %q", lines[0], header)
	}

	wantFirst := `2026-01-02T03:04:05.000Z,alice,instance,instance.create,low,普通操作,否,否,否,success,"created ""vm 1"""`
	if lines[1] != wantFirst {
		t.Fatalf("row 1 = %q, want %q", lines[1], wantFirst)
	}

	wantSecond := `2026-01-02T03:04:06.000Z,系统,system,system.config_update,high,系统配置变更,是,是,否,success,"a,b"`
	if lines[2] != wantSecond {
		t.Fatalf("row 2 = %q, want %q", lines[2], wantSecond)
	}

	wantThird := `2026-01-02T03:04:07.000Z,系统,user,user.delete,critical,删除用户,是,是,是,failed,x`
	if lines[3] != wantThird {
		t.Fatalf("row 3 = %q, want %q", lines[3], wantThird)
	}
}

// TestLogsBuildAuditCSVEmpty pins the header-only output for an empty export.
func TestLogsBuildAuditCSVEmpty(t *testing.T) {
	csv := logsBuildAuditCSV(nil)
	if strings.Contains(csv, "\n") {
		t.Fatalf("empty export must be a single header line, got %q", csv)
	}
	if !strings.HasPrefix(csv, "时间,用户,模块") {
		t.Fatalf("unexpected header %q", csv)
	}
}

// TestLogsQueryValue documents the Fastify query semantics the handlers rely on:
// absent vs single vs repeated.
func TestLogsQueryValue(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		url       string
		present   bool
		isArray   bool
		wantValue string
	}{
		{"/api/logs", false, false, ""},
		{"/api/logs?instanceId=", true, false, ""},
		{"/api/logs?instanceId=7", true, false, "7"},
		{"/api/logs?instanceId=1&instanceId=2", true, true, ""},
	}

	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		context.Request = httptest.NewRequest(http.MethodGet, tc.url, nil)

		value, present, isArray := logsQueryValue(context, "instanceId")
		if present != tc.present || isArray != tc.isArray || value != tc.wantValue {
			t.Errorf("%s: got (%q, present=%v, array=%v), want (%q, %v, %v)",
				tc.url, value, present, isArray, tc.wantValue, tc.present, tc.isArray)
		}
	}
}

// TestLogsUserModules pins the module list a regular user sees.
func TestLogsUserModules(t *testing.T) {
	want := []string{"security", "instance", "snapshot", "backup", "personal", "ssh_key", "notification"}
	if strings.Join(logsUserModules, ",") != strings.Join(want, ",") {
		t.Fatalf("user modules = %#v, want %#v", logsUserModules, want)
	}
}

func TestLogsYesNo(t *testing.T) {
	if logsYesNo(true) != "是" || logsYesNo(false) != "否" {
		t.Fatal("yes/no mapping changed")
	}
}

// TestLogsRouteRegistration pins the mount points: Fastify registers both
// "/api/logs" and "/api/logs/" for a plugin root, and the two static sub-paths.
func TestLogsRouteRegistration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterLogs(router)

	want := map[string]bool{
		"GET /api/logs":                  false,
		"GET /api/logs/":                 false,
		"GET /api/logs/modules":          false,
		"GET /api/logs/risk-definitions": false,
		"GET /api/logs/audit/export":     false,
	}
	for _, route := range router.Routes() {
		key := route.Method + " " + route.Path
		if _, tracked := want[key]; tracked {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("missing route %q", key)
		}
	}

	// Anonymous requests are rejected by the auth middleware, not the handler.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous list status = %d, want 401", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "Unauthorized") {
		t.Fatalf("anonymous body = %q", body)
	}
}
