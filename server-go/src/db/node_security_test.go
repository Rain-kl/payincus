// Unit tests for the pure half of db/node_security.go.
//
// Everything that needs a live database (the advisory-lock transactions, the
// upserts and the 30-day cleanup) is covered by the dual-run conformance suite;
// these tests pin the values that must never drift and the decision functions the
// report transaction is built on.
package db

import (
	"encoding/json"
	"math"
	"testing"

	"gorm.io/datatypes"

	"github.com/rain-kl/payincus/server-go/src/models"
)

func TestNodeSecurityAdvisoryLockNamespaces(t *testing.T) {
	// Cross-process contract with server/src/lib/node-security/*.ts. A different
	// value silently drops the mutual exclusion during the parallel run.
	if NodeSecurityReportLockNamespace != 700260 {
		t.Fatalf("NodeSecurityReportLockNamespace = %d, want 700260", NodeSecurityReportLockNamespace)
	}
	if NodeRecoveryEnqueueLockNamespace != 700261 {
		t.Fatalf("NodeRecoveryEnqueueLockNamespace = %d, want 700261", NodeRecoveryEnqueueLockNamespace)
	}
	if NodeRecoveryKind != "node_recover" {
		t.Fatalf("NodeRecoveryKind = %q, want node_recover", NodeRecoveryKind)
	}
}

func TestNodeReportRevisionOf(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    float64
		wantNaN bool
	}{
		{"empty", "", 0, false},
		{"object with revision", `{"revision":5}`, 5, false},
		{"object without revision", `{"locked":true}`, 0, false},
		{"revision null", `{"revision":null}`, 0, false},
		{"revision string", `{"revision":"7"}`, 7, false},
		{"revision empty string", `{"revision":""}`, 0, false},
		{"revision boolean", `{"revision":true}`, 1, false},
		{"array is not an object", `[1,2]`, 0, false},
		{"scalar is not an object", `"x"`, 0, false},
		{"invalid json", `{`, math.NaN(), true},
		{"revision object", `{"revision":{}}`, math.NaN(), true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := NodeReportRevisionOf([]byte(testCase.raw))
			if testCase.wantNaN {
				if !math.IsNaN(got) {
					t.Fatalf("revision = %v, want NaN", got)
				}
				return
			}
			if got != testCase.want {
				t.Fatalf("revision = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestNodeSecurityShouldPersistReport pins the delayed-delivery guard.
func TestNodeSecurityShouldPersistReport(t *testing.T) {
	cases := []struct {
		name             string
		revision         int64
		reason           string
		previousRevision float64
		want             bool
	}{
		{"newer revision persists", 6, "", 5, true},
		{"equal revision persists", 5, "", 5, true},
		{"older revision is dropped", 4, "", 5, false},
		{"reason does not rescue an older revision", 4, "daemon_restart", 5, false},
		{"state_unavailable always persists", 4, "state_unavailable", 5, true},
		{"audit_unavailable always persists", 4, "audit_unavailable", 5, true},
		{"first report always persists", 0, "", 0, true},
		{"NaN durable revision blocks everything", 9, "panel_request", math.NaN(), false},
		{"NaN durable revision still takes unavailable", 9, "state_unavailable", math.NaN(), true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := NodeSecurityShouldPersistReport(testCase.revision, testCase.reason, testCase.previousRevision)
			if got != testCase.want {
				t.Fatalf("persist = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestNodeSecurityApplyRecoveryBlocked pins that a blocked node can never be
// persisted as unlocked, and that the override also rewrites the reason.
func TestNodeSecurityApplyRecoveryBlocked(t *testing.T) {
	incoming := NodeSecurityReport{Enabled: true, Locked: false, Revision: 9, Reason: "panel_request"}

	blocked := NodeSecurityApplyRecoveryBlocked(incoming, true)
	if !blocked.Locked || blocked.Reason != "startup_verification" {
		t.Fatalf("blocked report = %+v", blocked)
	}
	if blocked.Revision != 9 || !blocked.Enabled {
		t.Fatalf("override must not touch other fields: %+v", blocked)
	}

	untouched := NodeSecurityApplyRecoveryBlocked(incoming, false)
	if untouched.Locked || untouched.Reason != "panel_request" {
		t.Fatalf("unblocked report = %+v", untouched)
	}
}

func TestNodeSecurityStoredReport(t *testing.T) {
	cases := []struct {
		name  string
		state *models.NodeSecurityState
		want  string
	}{
		{"nil state", nil, ""},
		{"empty report", &models.NodeSecurityState{Report: datatypes.JSON(nil)}, ""},
		{"empty object", &models.NodeSecurityState{Report: datatypes.JSON(`{}`)}, ""},
		{"array", &models.NodeSecurityState{Report: datatypes.JSON(`[{"locked":true}]`)}, ""},
		{"scalar", &models.NodeSecurityState{Report: datatypes.JSON(`5`)}, ""},
		{"locked report", &models.NodeSecurityState{Report: datatypes.JSON(`{"locked":true,"revision":3}`)}, `{"locked":true,"revision":3}`},
		{"unlocked report", &models.NodeSecurityState{Report: datatypes.JSON(`{"locked":false}`)}, `{"locked":false}`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := NodeSecurityStoredReport(testCase.state)
			if string(got) != testCase.want {
				t.Fatalf("stored report = %q, want %q", string(got), testCase.want)
			}
		})
	}
}

// TestNodeSecurityReportJSONShape pins the persisted report document: the field
// names are the API/column contract and must stay camelCase and complete.
func TestNodeSecurityReportJSONShape(t *testing.T) {
	report := NodeSecurityReport{
		Enabled:           true,
		Locked:            false,
		RecoveryPending:   true,
		StartupRecovering: false,
		Revision:          4,
		Results:           []NodeSecurityResult{},
		ResumeInstances:   []NodeSecurityResumeInstance{},
		Reason:            "cli:exec_blocked",
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"enabled":true,"locked":false,"recoveryPending":true,"startupRecovering":false,` +
		`"revision":4,"results":[],"resumeInstances":[],"reason":"cli:exec_blocked"}`
	if string(encoded) != want {
		t.Fatalf("report JSON = %s, want %s", encoded, want)
	}
}
