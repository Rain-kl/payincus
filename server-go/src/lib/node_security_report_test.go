// Tests for the node-security report allow-list.
//
// These pin the security contract: whatever an agent sends, only the enumerated
// scalars survive, and every clamp in server/src/lib/node-security/report.ts is
// reproduced. The database half (the advisory lock, the revision guard, the
// 30-day cleanup) is pinned by db/node_security_test.go plus the dual-run
// conformance suite.
package lib

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// nodeReportFixture builds a minimal valid report input and lets a test override
// the pieces it cares about.
func nodeReportFixture(t *testing.T, mutate func(state, event map[string]any)) map[string]any {
	t.Helper()
	state := map[string]any{
		"enabled":  true,
		"locked":   false,
		"revision": float64(7),
	}
	event := map[string]any{
		"id":       strings.Repeat("a", 32),
		"time":     "2026-01-02T03:04:05.000Z",
		"uid":      float64(0),
		"pid":      float64(4242),
		"command":  "list",
		"critical": false,
		"blocked":  false,
	}
	if mutate != nil {
		mutate(state, event)
	}
	// Round-trip through JSON so the fixture carries the same Go types a real
	// `json.Unmarshal` produces (every number is a float64), which is the only
	// shape ParseNodeReport ever sees.
	encoded, err := json.Marshal(map[string]any{"state": state, "events": []any{event}})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	var roundTripped map[string]any
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return roundTripped
}

func decodeNodeReportJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("fixture is not valid JSON: %v (%s)", err, raw)
	}
	return decoded
}

func TestIncusCLICommandAllowList(t *testing.T) {
	commands := IncusCLICommands()
	if len(commands) != 49 {
		t.Fatalf("allow-list has %d commands, want 49 (the TypeScript literal)", len(commands))
	}
	for _, expected := range []string{"help", "exec", "list", "unknown_option", "operation_read"} {
		if !IsIncusCLICommand(expected) {
			t.Fatalf("%q is missing from the allow-list", expected)
		}
	}
	for _, rejected := range []string{"", "LIST", "rm", "dd", "list ", "curl"} {
		if IsIncusCLICommand(rejected) {
			t.Fatalf("%q must not be on the allow-list", rejected)
		}
	}

	// The returned slice is a copy: a caller cannot widen the allow-list.
	commands[0] = "hacked"
	if !IsIncusCLICommand("help") {
		t.Fatal("IncusCLICommands() leaked the internal allow-list")
	}
}

// TestParseNodeReportDropsArbitraryFields is the core allow-list assertion.
func TestParseNodeReportDropsArbitraryFields(t *testing.T) {
	input := nodeReportFixture(t, func(state, event map[string]any) {
		state["unexpected"] = "do not keep me"
		state["commandPayload"] = map[string]any{"password": "hunter2"}
		state["results"] = []any{map[string]any{
			"target":   "tank/data",
			"success":  true,
			"detail":   "complete",
			"secret":   "leaked?",
			"rawError": strings.Repeat("x", 5000),
		}}
		state["resumeInstances"] = []any{map[string]any{
			"project": "default",
			"name":    "vm-1",
			"uuid":    "should-not-survive",
			"frozen":  true,
			"extra":   "no",
		}}
		event["payload"] = "rm -rf /"
		event["env"] = map[string]any{"TOKEN": "secret"}
	})

	report, events, err := ParseNodeReport(input)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	decoded := decodeNodeReportJSON(t, string(encoded))

	allowed := []string{
		"enabled", "locked", "recoveryPending", "startupRecovering",
		"revision", "results", "resumeInstances", "reason",
	}
	if len(decoded) != len(allowed) {
		t.Fatalf("report has %d keys, want exactly %d (%s)", len(decoded), len(allowed), string(encoded))
	}
	for _, key := range allowed {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("report is missing %q: %s", key, string(encoded))
		}
	}
	if strings.Contains(string(encoded), "hunter2") || strings.Contains(string(encoded), "leaked?") ||
		strings.Contains(string(encoded), "should-not-survive") {
		t.Fatalf("report retained an agent-provided field: %s", string(encoded))
	}

	result := decoded["results"].([]any)[0].(map[string]any)
	if len(result) != 3 {
		t.Fatalf("result kept extra keys: %v", result)
	}
	instance := decoded["resumeInstances"].([]any)[0].(map[string]any)
	if len(instance) != 2 {
		t.Fatalf("resume instance kept extra keys: %v", instance)
	}

	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	eventJSON, err := json.Marshal(events[0])
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	eventDecoded := decodeNodeReportJSON(t, string(eventJSON))
	if len(eventDecoded) != 7 {
		t.Fatalf("event has %d keys, want 7: %s", len(eventDecoded), string(eventJSON))
	}
}

func TestParseNodeReportClamps(t *testing.T) {
	// 128 events is the documented maximum; 129 is rejected outright.
	t.Run("events ceiling", func(t *testing.T) {
		makeEvents := func(count int) []any {
			events := make([]any, 0, count)
			for index := 0; index < count; index++ {
				events = append(events, map[string]any{
					"id":       fmt.Sprintf("%032x", index),
					"time":     "2026-01-02T03:04:05Z",
					"uid":      float64(1),
					"pid":      float64(2),
					"command":  "exec",
					"critical": true,
					"blocked":  false,
				})
			}
			return events
		}

		input := map[string]any{
			"state":  map[string]any{"enabled": true, "locked": false, "revision": float64(1)},
			"events": makeEvents(128),
		}
		if _, events, err := ParseNodeReport(input); err != nil || len(events) != 128 {
			t.Fatalf("128 events: len=%d err=%v, want 128 and nil", len(events), err)
		}

		input["events"] = makeEvents(129)
		if _, _, err := ParseNodeReport(input); err == nil || err.Error() != "invalid events" {
			t.Fatalf("129 events: err=%v, want invalid events", err)
		}

		input["events"] = map[string]any{}
		if _, _, err := ParseNodeReport(input); err == nil || err.Error() != "invalid events" {
			t.Fatalf("object events: err=%v, want invalid events", err)
		}
	})

	t.Run("results clamp", func(t *testing.T) {
		results := make([]any, 0, 300)
		for index := 0; index < 300; index++ {
			results = append(results, map[string]any{
				"target":  strings.Repeat("z", 300),
				"success": true,
				"detail":  "not_configured",
			})
		}
		input := nodeReportFixture(t, func(state, _ map[string]any) {
			state["results"] = results
		})
		report, _, err := ParseNodeReport(input)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(report.Results) != 256 {
			t.Fatalf("results = %d, want 256", len(report.Results))
		}
		if len(report.Results[0].Target) != 200 {
			t.Fatalf("target length = %d, want 200", len(report.Results[0].Target))
		}
		if report.Results[0].Detail != "not_configured" || !report.Results[0].Success {
			t.Fatalf("result = %+v", report.Results[0])
		}
	})

	t.Run("results detail enum and strict success", func(t *testing.T) {
		input := nodeReportFixture(t, func(state, _ map[string]any) {
			state["results"] = []any{
				map[string]any{"target": "a", "success": "true", "detail": "complete"},
				map[string]any{"target": "", "success": 1, "detail": "weird"},
				map[string]any{"target": 12, "success": true, "detail": "failed"},
				map[string]any{"target": true, "detail": "not_configured"},
				"not-an-object",
			}
		})
		report, _, err := ParseNodeReport(input)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(report.Results) != 5 {
			t.Fatalf("results = %d, want 5", len(report.Results))
		}
		// `success: "true"` is not `=== true`.
		if report.Results[0].Success || report.Results[0].Detail != "complete" {
			t.Fatalf("result[0] = %+v", report.Results[0])
		}
		if report.Results[1].Detail != "failed" || report.Results[1].Success {
			t.Fatalf("result[1] = %+v", report.Results[1])
		}
		// String(12) === "12"
		if report.Results[2].Target != "12" {
			t.Fatalf("result[2].target = %q, want 12", report.Results[2].Target)
		}
		// String(true) === "true"
		if report.Results[3].Target != "true" {
			t.Fatalf("result[3].target = %q, want true", report.Results[3].Target)
		}
		if report.Results[4].Target != "" || report.Results[4].Detail != "failed" {
			t.Fatalf("result[4] = %+v", report.Results[4])
		}
	})

	t.Run("resume instances clamp and filter", func(t *testing.T) {
		instances := make([]any, 0, nodeReportMaxResumeInstances+1)
		for index := 0; index < nodeReportMaxResumeInstances+1; index++ {
			instances = append(instances, map[string]any{
				"project": "default",
				"name":    strings.Repeat("n", 300),
			})
		}
		input := nodeReportFixture(t, func(state, _ map[string]any) {
			state["resumeInstances"] = instances
		})
		report, _, err := ParseNodeReport(input)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(report.ResumeInstances) != 10000 {
			t.Fatalf("resumeInstances = %d, want 10000", len(report.ResumeInstances))
		}
		if len(report.ResumeInstances[0].Name) != 255 {
			t.Fatalf("name length = %d, want 255", len(report.ResumeInstances[0].Name))
		}
	})

	t.Run("resume instance filter keeps empty strings and drops non-strings", func(t *testing.T) {
		input := nodeReportFixture(t, func(state, _ map[string]any) {
			state["resumeInstances"] = []any{
				map[string]any{"project": "", "name": ""},
				map[string]any{"project": 1, "name": "n"},
				map[string]any{"project": "p"},
				nil,
				map[string]any{"project": "p", "name": "n"},
			}
		})
		report, _, err := ParseNodeReport(input)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(report.ResumeInstances) != 2 {
			t.Fatalf("resumeInstances = %+v, want 2 entries", report.ResumeInstances)
		}
		if report.ResumeInstances[0].Project != "" || report.ResumeInstances[0].Name != "" {
			t.Fatalf("empty-string identity was dropped: %+v", report.ResumeInstances[0])
		}
	})

	t.Run("reason gate", func(t *testing.T) {
		cases := []struct {
			input any
			want  string
		}{
			{"startup_verification", "startup_verification"},
			{"daemon_restart", "daemon_restart"},
			{"panel_request", "panel_request"},
			{"state_unavailable", "state_unavailable"},
			{"audit_unavailable", "audit_unavailable"},
			{"cli:exec_blocked", "cli:exec_blocked"},
			{"cli:a_b_c", "cli:a_b_c"},
			{"cli:UPPER", ""},
			{"cli:", ""},
			{"cli:a-b", ""},
			{"arbitrary", ""},
			{"", ""},
			{float64(3), ""},
			{nil, ""},
			{"cli:" + strings.Repeat("a", 100), "cli:" + strings.Repeat("a", 60)},
		}
		for _, testCase := range cases {
			input := nodeReportFixture(t, func(state, _ map[string]any) {
				state["reason"] = testCase.input
			})
			report, _, err := ParseNodeReport(input)
			if err != nil {
				t.Fatalf("reason %v: %v", testCase.input, err)
			}
			if report.Reason != testCase.want {
				t.Fatalf("reason %v = %q, want %q", testCase.input, report.Reason, testCase.want)
			}
		}
	})
}

func TestParseNodeReportRejectsInvalidState(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  string
	}{
		{"nil input", nil, "invalid report"},
		{"string input", "nope", "invalid report"},
		{"array input", []any{}, "invalid state"},
		{"missing state", map[string]any{"events": []any{}}, "invalid state"},
		{"null state", map[string]any{"state": nil, "events": []any{}}, "invalid state"},
		{"string state", map[string]any{"state": "x", "events": []any{}}, "invalid state"},
		{"enabled missing", map[string]any{"state": map[string]any{"locked": false, "revision": float64(1)}, "events": []any{}}, "invalid state"},
		{"enabled not bool", map[string]any{"state": map[string]any{"enabled": "true", "locked": false, "revision": float64(1)}, "events": []any{}}, "invalid state"},
		{"locked missing", map[string]any{"state": map[string]any{"enabled": true, "revision": float64(1)}, "events": []any{}}, "invalid state"},
		{"revision missing", map[string]any{"state": map[string]any{"enabled": true, "locked": false}, "events": []any{}}, "invalid state"},
		{"revision negative", map[string]any{"state": map[string]any{"enabled": true, "locked": false, "revision": float64(-1)}, "events": []any{}}, "invalid state"},
		{"revision fractional", map[string]any{"state": map[string]any{"enabled": true, "locked": false, "revision": 1.5}, "events": []any{}}, "invalid state"},
		{"revision unsafe", map[string]any{"state": map[string]any{"enabled": true, "locked": false, "revision": float64(1 << 53)}, "events": []any{}}, "invalid state"},
		{"revision string", map[string]any{"state": map[string]any{"enabled": true, "locked": false, "revision": "1"}, "events": []any{}}, "invalid state"},
		{"events missing", map[string]any{"state": map[string]any{"enabled": true, "locked": false, "revision": float64(1)}}, "invalid events"},
		{"events null", map[string]any{"state": map[string]any{"enabled": true, "locked": false, "revision": float64(1)}, "events": nil}, "invalid events"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := ParseNodeReport(testCase.input)
			if err == nil || err.Error() != testCase.want {
				t.Fatalf("err = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestParseNodeReportRejectsInvalidEvent(t *testing.T) {
	mutations := map[string]func(event map[string]any){
		"short id":           func(event map[string]any) { event["id"] = strings.Repeat("a", 31) },
		"uppercase id":       func(event map[string]any) { event["id"] = strings.Repeat("A", 32) },
		"non-string id":      func(event map[string]any) { event["id"] = float64(1) },
		"unknown command":    func(event map[string]any) { event["command"] = "rm" },
		"non-string command": func(event map[string]any) { event["command"] = float64(1) },
		"critical string":    func(event map[string]any) { event["critical"] = "true" },
		"blocked missing":    func(event map[string]any) { delete(event, "blocked") },
		"uid fractional":     func(event map[string]any) { event["uid"] = 1.5 },
		"uid negative":       func(event map[string]any) { event["uid"] = float64(-1) },
		"uid too large":      func(event map[string]any) { event["uid"] = float64(4294967296) },
		"uid string":         func(event map[string]any) { event["uid"] = "0" },
		"pid fractional":     func(event map[string]any) { event["pid"] = 2.5 },
		"pid too large":      func(event map[string]any) { event["pid"] = float64(2147483648) },
		"time missing":       func(event map[string]any) { delete(event, "time") },
		"time not a string":  func(event map[string]any) { event["time"] = float64(1) },
		"time unparsable":    func(event map[string]any) { event["time"] = "yesterday" },
		"time NaN-able":      func(event map[string]any) { event["time"] = "2026-13-45T99:99:99Z" },
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			_, _, err := ParseNodeReport(nodeReportFixture(t, func(_, event map[string]any) { mutate(event) }))
			if err == nil || err.Error() != "invalid event" {
				t.Fatalf("err = %v, want invalid event", err)
			}
		})
	}

	t.Run("event is not an object", func(t *testing.T) {
		input := map[string]any{
			"state":  map[string]any{"enabled": true, "locked": false, "revision": float64(1)},
			"events": []any{nil},
		}
		if _, _, err := ParseNodeReport(input); err == nil || err.Error() != "invalid event" {
			t.Fatalf("err = %v, want invalid event", err)
		}
	})
}

func TestParseNodeReportAcceptsBoundaryValues(t *testing.T) {
	input := nodeReportFixture(t, func(state, event map[string]any) {
		state["revision"] = float64(nodeReportSafeIntegerMax)
		event["uid"] = float64(nodeReportMaxUID)
		event["pid"] = float64(nodeReportMaxPID)
		event["time"] = "2026-01-02 03:04:05"
	})
	report, events, err := ParseNodeReport(input)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if report.Revision != nodeReportSafeIntegerMax {
		t.Fatalf("revision = %d", report.Revision)
	}
	if events[0].UID != nodeReportMaxUID || events[0].PID != nodeReportMaxPID {
		t.Fatalf("event = %+v", events[0])
	}
}

func TestJsTruncateUTF16(t *testing.T) {
	cases := []struct {
		name  string
		value string
		limit int
		want  string
	}{
		{"shorter than the limit", "abc", 5, "abc"},
		{"exactly the limit", "abcde", 5, "abcde"},
		{"truncated", "abcdef", 5, "abcde"},
		{"astral counts twice", "a\U0001F600b", 1, "a"},
		{"astral pair is not split", "a\U0001F600b", 2, "a"},
		{"astral fits", "a\U0001F600b", 3, "a\U0001F600"},
		{"zero limit", "abc", 0, ""},
	}
	for _, testCase := range cases {
		if got := jsTruncateUTF16(testCase.value, testCase.limit); got != testCase.want {
			t.Fatalf("%s: jsTruncateUTF16(%q, %d) = %q, want %q",
				testCase.name, testCase.value, testCase.limit, got, testCase.want)
		}
	}
}

// TestRecordReportParsesBeforeTouchingTheDatabase pins that an invalid payload is
// rejected without a database handle: parseReport runs before the transaction.
func TestRecordReportParsesBeforeTouchingTheDatabase(t *testing.T) {
	if _, err := RecordReport(context.Background(), 1, "not-an-object"); err == nil || err.Error() != "invalid report" {
		t.Fatalf("err = %v, want invalid report", err)
	}
}
