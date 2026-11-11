// Tests for restoreNodeInstances / enqueueNodeRecovery.
//
// The recovery flow is the highest-risk part of this module: it starts workloads
// on a node. These tests pin the identity checks (project/name/uuid/frozen), the
// workload-class allow-list, the freeze dance, the acknowledgement ordering and
// the cancellation semantics.
package lib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// recoveryFakeClient records every request and delegates the response.
type recoveryFakeClient struct {
	mu       sync.Mutex
	requests []string
	bodies   []any
	handler  func(method, path string, body any) (json.RawMessage, error)
}

func (f *recoveryFakeClient) Request(_ context.Context, method, path string, body any) (json.RawMessage, error) {
	f.mu.Lock()
	f.requests = append(f.requests, method+" "+path)
	f.bodies = append(f.bodies, body)
	f.mu.Unlock()
	if f.handler == nil {
		return json.RawMessage("{}"), nil
	}
	return f.handler(method, path, body)
}

func (f *recoveryFakeClient) requestPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func recoveryStateJSON(t *testing.T, revision int64, instances ...ResumeInstance) RecoveryState {
	t.Helper()
	payload := map[string]any{
		"locked":          false,
		"revision":        revision,
		"resumeInstances": instances,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	state, err := DecodeRecoveryState(raw)
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	return state
}

func frozenValue(value bool) *bool { return &value }

// TestRestoreNodeInstancesStartsAndFreezes drives the full happy path: one
// container that is already running, one frozen VM that must be started and then
// frozen again.
func TestRestoreNodeInstancesStartsAndFreezes(t *testing.T) {
	client := &recoveryFakeClient{}
	revision := int64(5)
	client.handler = func(method, path string, body any) (json.RawMessage, error) {
		switch {
		case method == "GET" && strings.Contains(path, "/1.0/instances/vm-1?project=default"):
			return json.RawMessage(`{"type":"container","config":{"volatile.uuid":"uuid-1"}}`), nil
		case method == "GET" && strings.Contains(path, "/1.0/instances/vm-1/state?project=default"):
			return json.RawMessage(`{"status":"Running"}`), nil
		case method == "GET" && strings.Contains(path, "/1.0/instances/vm-2?project=default"):
			return json.RawMessage(`{"type":"virtual-machine","config":{"volatile.uuid":"uuid-2"}}`), nil
		case method == "GET" && strings.Contains(path, "/1.0/instances/vm-2/state?project=default"):
			// Stopped until the start request, Frozen after the freeze request.
			for _, sent := range client.requestPaths() {
				if sent == "PUT /1.0/instances/vm-2/state?project=default" {
					return json.RawMessage(`{"status":"Frozen"}`), nil
				}
			}
			return json.RawMessage(`{"status":"Stopped"}`), nil
		case method == "PUT" && strings.HasPrefix(path, "/1.0/instances/vm-2/state"):
			return json.RawMessage(`{}`), nil
		case method == "POST" && path == "/1.0/agent/security/resume":
			revision++
			return json.RawMessage(fmt.Sprintf(`{"locked":false,"revision":%d,"resumeInstances":[]}`, revision)), nil
		}
		t.Fatalf("unexpected request %s %s", method, path)
		return nil, nil
	}

	initial := recoveryStateJSON(t, 5,
		ResumeInstance{Project: "default", Name: "vm-1", UUID: "uuid-1", Frozen: frozenValue(false)},
		ResumeInstance{Project: "default", Name: "vm-2", UUID: "uuid-2", Frozen: frozenValue(true)},
	)

	type progressCall struct{ completed, total, failed int }
	progress := make([]progressCall, 0, 2)

	state, failed, err := RestoreNodeInstances(context.Background(), client, initial,
		func(ResumeInstance) (bool, error) { return true, nil },
		RecoveryOptions{
			Types: []string{"container", "virtual-machine"},
			Progress: func(completed, total, failedCount int) {
				progress = append(progress, progressCall{completed, total, failedCount})
			},
		})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if failed != 0 {
		t.Fatalf("failed = %d, want 0", failed)
	}
	if state.Revision == nil || *state.Revision != 7 {
		t.Fatalf("final revision = %v, want 7", state.Revision)
	}

	want := []string{
		"GET /1.0/instances/vm-1?project=default",
		"GET /1.0/instances/vm-1/state?project=default",
		"POST /1.0/agent/security/resume",
		"GET /1.0/instances/vm-2?project=default",
		"GET /1.0/instances/vm-2/state?project=default",
		"PUT /1.0/instances/vm-2/state?project=default",
		"GET /1.0/instances/vm-2/state?project=default",
		"POST /1.0/agent/security/resume",
	}
	got := client.requestPaths()
	if len(got) != len(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("request[%d] = %q, want %q", index, got[index], want[index])
		}
	}

	// The second acknowledgement must carry the revision returned by the first.
	secondAck, ok := client.bodies[7].(map[string]any)
	if !ok {
		t.Fatalf("ack body = %#v", client.bodies[7])
	}
	if secondAck["revision"] != int64(6) {
		t.Fatalf("second ack revision = %#v, want 6", secondAck["revision"])
	}

	if len(progress) != 2 {
		t.Fatalf("progress calls = %v, want 2", progress)
	}
	if progress[0] != (progressCall{1, 2, 0}) || progress[1] != (progressCall{2, 2, 0}) {
		t.Fatalf("progress = %v", progress)
	}
}

// TestRestoreNodeInstancesStartsStoppedInstance covers the start branch.
func TestRestoreNodeInstancesStartsStoppedInstance(t *testing.T) {
	client := &recoveryFakeClient{}
	started := false
	client.handler = func(method, path string, _ any) (json.RawMessage, error) {
		switch {
		case method == "GET" && strings.HasSuffix(path, "/state?project=default"):
			if started {
				return json.RawMessage(`{"status":"Running"}`), nil
			}
			return json.RawMessage(`{"status":"Stopped"}`), nil
		case method == "PUT":
			started = true
			return json.RawMessage(`{}`), nil
		case method == "GET":
			return json.RawMessage(`{"type":"container","config":{"volatile.uuid":"u"}}`), nil
		case method == "POST":
			return json.RawMessage(`{"locked":false,"revision":2,"resumeInstances":[]}`), nil
		}
		return nil, errors.New("unexpected")
	}

	initial := recoveryStateJSON(t, 1,
		ResumeInstance{Project: "default", Name: "vm", UUID: "u", Frozen: frozenValue(false)})

	putBody := any(nil)
	_, failed, err := RestoreNodeInstances(context.Background(), client, initial,
		func(ResumeInstance) (bool, error) { return true, nil },
		RecoveryOptions{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if failed != 0 {
		t.Fatalf("failed = %d", failed)
	}
	for index, path := range client.requestPaths() {
		if strings.HasPrefix(path, "PUT ") {
			putBody = client.bodies[index]
		}
	}
	body, ok := putBody.(map[string]any)
	if !ok {
		t.Fatalf("start body = %#v", putBody)
	}
	if body["action"] != "start" || body["timeout"] != 30 || body["force"] != false || body["stateful"] != false {
		t.Fatalf("start body = %#v", body)
	}
}

// TestRestoreNodeInstancesAcknowledgesSkippedItems pins that a workload the panel
// deliberately does not restore is acknowledged (so the node stops listing it).
func TestRestoreNodeInstancesAcknowledgesSkippedItems(t *testing.T) {
	client := &recoveryFakeClient{}
	client.handler = func(method, path string, _ any) (json.RawMessage, error) {
		if method == "POST" {
			return json.RawMessage(`{"locked":false,"revision":2}`), nil
		}
		t.Fatalf("a skipped item must not be probed: %s %s", method, path)
		return nil, nil
	}

	initial := recoveryStateJSON(t, 1,
		ResumeInstance{Project: "default", Name: "gone", UUID: "u", Frozen: frozenValue(false)})

	_, failed, err := RestoreNodeInstances(context.Background(), client, initial,
		func(ResumeInstance) (bool, error) { return false, nil },
		RecoveryOptions{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if failed != 0 {
		t.Fatalf("failed = %d, want 0", failed)
	}
	got := client.requestPaths()
	if len(got) != 1 || got[0] != "POST /1.0/agent/security/resume" {
		t.Fatalf("requests = %v", got)
	}
}

// TestRestoreNodeInstancesRejectsIdentityChanges pins the anti-hijack checks.
func TestRestoreNodeInstancesRejectsIdentityChanges(t *testing.T) {
	cases := []struct {
		name     string
		instance string
		types    []string
		wantPUT  bool
	}{
		{"uuid changed", `{"type":"container","config":{"volatile.uuid":"other"}}`, nil, false},
		{"uuid missing", `{"type":"container","config":{}}`, nil, false},
		{"config missing", `{"type":"container"}`, nil, false},
		{"wrong type", `{"type":"virtual-machine","config":{"volatile.uuid":"u"}}`, nil, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client := &recoveryFakeClient{}
			acked := false
			client.handler = func(method, path string, _ any) (json.RawMessage, error) {
				if method == "POST" {
					acked = true
					return json.RawMessage(`{"locked":false,"revision":2}`), nil
				}
				return json.RawMessage(testCase.instance), nil
			}
			initial := recoveryStateJSON(t, 1,
				ResumeInstance{Project: "default", Name: "vm", UUID: "u", Frozen: frozenValue(false)})

			_, failed, err := RestoreNodeInstances(context.Background(), client, initial,
				func(ResumeInstance) (bool, error) { return true, nil },
				RecoveryOptions{})
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			if failed != 1 {
				t.Fatalf("failed = %d, want 1", failed)
			}
			if acked {
				t.Fatal("a workload whose identity changed must not be acknowledged")
			}
		})
	}
}

// TestRestoreNodeInstancesRejectsIncompleteIdentity pins the per-item validation.
func TestRestoreNodeInstancesRejectsIncompleteIdentity(t *testing.T) {
	cases := []struct {
		name  string
		item  ResumeInstance
		fails bool
	}{
		{"empty project", ResumeInstance{Name: "n", UUID: "u", Frozen: frozenValue(false)}, true},
		{"empty name", ResumeInstance{Project: "p", UUID: "u", Frozen: frozenValue(false)}, true},
		{"empty uuid", ResumeInstance{Project: "p", Name: "n", Frozen: frozenValue(false)}, true},
		{"missing frozen", ResumeInstance{Project: "p", Name: "n", UUID: "u"}, true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client := &recoveryFakeClient{}
			initial := recoveryStateJSON(t, 1, testCase.item)
			_, failed, err := RestoreNodeInstances(context.Background(), client, initial,
				func(ResumeInstance) (bool, error) { return true, nil },
				RecoveryOptions{})
			if err != nil {
				t.Fatalf("restore: %v", err)
			}
			if (failed == 1) != testCase.fails {
				t.Fatalf("failed = %d, want failure=%v", failed, testCase.fails)
			}
			if len(client.requestPaths()) != 0 {
				t.Fatalf("requests = %v, want none", client.requestPaths())
			}
		})
	}
}

// TestRestoreNodeInstancesRejectsInvalidInitialState pins the guard clause.
func TestRestoreNodeInstancesRejectsInvalidInitialState(t *testing.T) {
	locked := true
	revision := int64(1)
	zero := int64(0)

	cases := map[string]RecoveryState{
		"locked":              {Locked: &locked, Revision: &revision, ResumeInstances: []ResumeInstance{}},
		"revision missing":    {ResumeInstances: []ResumeInstance{}},
		"instances not array": {Revision: &revision},
		"revision unsafe":     {Revision: func() *int64 { v := nodeReportSafeIntegerMax + 1; return &v }(), ResumeInstances: []ResumeInstance{}},
	}

	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			client := &recoveryFakeClient{}
			_, failed, err := RestoreNodeInstances(context.Background(), client, state,
				func(ResumeInstance) (bool, error) { return true, nil }, RecoveryOptions{})
			if err == nil || err.Error() != "invalid recovery state" {
				t.Fatalf("err = %v, want invalid recovery state", err)
			}
			if failed != 0 {
				t.Fatalf("failed = %d", failed)
			}
			if len(client.requestPaths()) != 0 {
				t.Fatal("an invalid state must not touch the node")
			}
		})
	}

	// A negative revision is a safe integer and is accepted here (parseReport is
	// the caller that clamps >= 0), and an empty list is a valid no-op.
	t.Run("negative revision and empty list", func(t *testing.T) {
		client := &recoveryFakeClient{}
		state, failed, err := RestoreNodeInstances(context.Background(), client,
			RecoveryState{Revision: &zero, ResumeInstances: []ResumeInstance{}},
			func(ResumeInstance) (bool, error) { return true, nil }, RecoveryOptions{})
		if err != nil || failed != 0 || state.Revision != &zero {
			t.Fatalf("err=%v failed=%d", err, failed)
		}
	})
}

// TestRestoreNodeInstancesCancellation pins both cancellation polls.
func TestRestoreNodeInstancesCancellation(t *testing.T) {
	t.Run("before the first instance", func(t *testing.T) {
		client := &recoveryFakeClient{}
		initial := recoveryStateJSON(t, 1,
			ResumeInstance{Project: "default", Name: "vm", UUID: "u", Frozen: frozenValue(false)})
		_, _, err := RestoreNodeInstances(context.Background(), client, initial,
			func(ResumeInstance) (bool, error) { return true, nil },
			RecoveryOptions{Cancelled: func() bool { return true }})
		if !errors.Is(err, ErrNodeRecoveryCancelled) {
			t.Fatalf("err = %v, want cancelled", err)
		}
		if len(client.requestPaths()) != 0 {
			t.Fatal("a cancelled recovery must not touch the node")
		}
	})

	t.Run("after a failed instance", func(t *testing.T) {
		client := &recoveryFakeClient{}
		client.handler = func(method, _ string, _ any) (json.RawMessage, error) {
			if method == "GET" {
				return json.RawMessage(`{"type":"container","config":{"volatile.uuid":"different"}}`), nil
			}
			return json.RawMessage(`{}`), nil
		}
		calls := 0
		initial := recoveryStateJSON(t, 1,
			ResumeInstance{Project: "default", Name: "vm", UUID: "u", Frozen: frozenValue(false)})
		progress := 0
		var lastFailed int
		_, failed, err := RestoreNodeInstances(context.Background(), client, initial,
			func(ResumeInstance) (bool, error) { return true, nil },
			RecoveryOptions{
				Cancelled: func() bool { calls++; return calls > 1 },
				Progress:  func(_, _, failedCount int) { progress++; lastFailed = failedCount },
			})
		if !errors.Is(err, ErrNodeRecoveryCancelled) {
			t.Fatalf("err = %v, want cancelled", err)
		}
		if failed != 1 {
			t.Fatalf("failed = %d, want 1", failed)
		}
		// The finally block still reports the failed item before the cancellation
		// propagates.
		if progress != 1 || lastFailed != 1 {
			t.Fatalf("progress calls = %d, last failed = %d, want 1 and 1", progress, lastFailed)
		}
	})
}

// TestRestoreNodeInstancesUnrecoveredStatus pins the final-state check.
func TestRestoreNodeInstancesUnrecoveredStatus(t *testing.T) {
	client := &recoveryFakeClient{}
	client.handler = func(method, _ string, _ any) (json.RawMessage, error) {
		switch method {
		case "GET":
			if strings.Contains(strings.Join(client.requestPaths(), " "), "/state") {
				return json.RawMessage(`{"status":"Error"}`), nil
			}
			return json.RawMessage(`{"type":"container","config":{"volatile.uuid":"u"}}`), nil
		}
		return json.RawMessage(`{"locked":false,"revision":2}`), nil
	}
	initial := recoveryStateJSON(t, 1,
		ResumeInstance{Project: "default", Name: "vm", UUID: "u", Frozen: frozenValue(true)})

	_, failed, err := RestoreNodeInstances(context.Background(), client, initial,
		func(ResumeInstance) (bool, error) { return true, nil }, RecoveryOptions{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if failed != 1 {
		t.Fatalf("failed = %d, want 1", failed)
	}
}

// TestRestoreNodeInstancesEncodesProjectAndName pins the URL encoding of the
// identity the node will act on.
func TestRestoreNodeInstancesEncodesProjectAndName(t *testing.T) {
	client := &recoveryFakeClient{}
	client.handler = func(method, path string, _ any) (json.RawMessage, error) {
		if method == "POST" {
			return json.RawMessage(`{"locked":false,"revision":2}`), nil
		}
		if strings.Contains(path, "/state") {
			return json.RawMessage(`{"status":"Running"}`), nil
		}
		return json.RawMessage(`{"type":"container","config":{"volatile.uuid":"u"}}`), nil
	}
	initial := recoveryStateJSON(t, 1,
		ResumeInstance{Project: "team a!", Name: "vm 1", UUID: "u", Frozen: frozenValue(false)})

	if _, _, err := RestoreNodeInstances(context.Background(), client, initial,
		func(ResumeInstance) (bool, error) { return true, nil }, RecoveryOptions{}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got := client.requestPaths()
	if got[0] != "GET /1.0/instances/vm%201?project=team%20a!" {
		t.Fatalf("request = %q", got[0])
	}
}

func TestJsEncodeURIComponent(t *testing.T) {
	cases := map[string]string{
		"vm-1":        "vm-1",
		"team a":      "team%20a",
		"a!b":         "a!b",
		"it's":        "it's",
		"(x)*":        "(x)*",
		"a~b_c.d-e":   "a~b_c.d-e",
		"100%":        "100%25",
		"a+b":         "a%2Bb",
		"ü":           "%C3%BC",
		"/path":       "%2Fpath",
		"line\nbreak": "line%0Abreak",
		"#?&=":        "%23%3F%26%3D",
	}
	for input, want := range cases {
		if got := jsEncodeURIComponent(input); got != want {
			t.Fatalf("jsEncodeURIComponent(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestNodeRecoveryKindMatchesOperationTask pins the literal shared with
// lib.OperationTaskKindNodeRecover (db cannot import lib, so the equality is
// asserted from this side).
func TestNodeRecoveryKindMatchesOperationTask(t *testing.T) {
	if db.NodeRecoveryKind != string(OperationTaskKindNodeRecover) {
		t.Fatalf("db.NodeRecoveryKind = %q, OperationTaskKindNodeRecover = %q",
			db.NodeRecoveryKind, OperationTaskKindNodeRecover)
	}
}

// TestDecodeRecoveryStatePreservesRawJSON pins the lossless round-trip used when a
// state is written back as a report.
func TestDecodeRecoveryStatePreservesRawJSON(t *testing.T) {
	raw := `{"locked":false,"revision":3,"resumeInstances":[],"futureField":{"x":1}}`
	state, err := DecodeRecoveryState([]byte(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(state.RawJSON()) != raw {
		t.Fatalf("RawJSON = %s, want %s", state.RawJSON(), raw)
	}
}
