// Port of server/src/lib/node-security/recovery.ts and
// server/src/lib/node-security/recovery-task.ts.
//
// restoreNodeInstances re-establishes the workloads a node had running before it
// lost state. The node keeps every pending identity listed until the panel
// acknowledges it, so an interrupted recovery can always be retried from where it
// stopped — which is also why a cancelled or failed item is NOT acknowledged.
//
// The security checks are load-bearing and must not be softened:
//
//   - the initial state must be unlocked, safe-integer revisioned and carry an
//     array of resume instances, otherwise nothing is attempted
//   - each workload must still match its recorded identity: same project, same
//     name, same `volatile.uuid` inside the instance config. A replaced or
//     re-created instance with a recycled name must not receive the old disk.
//   - only the configured instance types (container / virtual-machine) may be
//     started, so a recovery cannot resurrect an arbitrary workload class
//   - the final state must be exactly Frozen (frozen item) or Running
//
// enqueueNodeRecovery persists acceptance BEFORE scheduling any node request: a
// failed insert must never unlock a node.
package lib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// ResumeInstance mirrors the TypeScript interface: one pending workload identity.
//
// Frozen is a pointer so an absent field is distinguishable from `false`; the
// TypeScript rejects a non-boolean `frozen` ("invalid workload identity") and Go's
// zero value would otherwise accept a missing one.
type ResumeInstance struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	UUID    string `json:"uuid"`
	Frozen  *bool  `json:"frozen"`
}

// RecoveryState mirrors the node's recovery document.
//
// Locked and Revision are pointers for the same presence reason: the TypeScript
// requires a boolean and a safe integer before it will touch a single instance.
// raw keeps the exact document the node returned, so a state round-trip into the
// report never drops fields this struct does not model.
type RecoveryState struct {
	Locked          *bool            `json:"locked"`
	Revision        *int64           `json:"revision"`
	ResumeInstances []ResumeInstance `json:"resumeInstances"`

	raw json.RawMessage
}

// RawJSON returns the exact bytes the node returned (or a re-encoding for a
// state built in Go).
func (s RecoveryState) RawJSON() json.RawMessage {
	if len(s.raw) > 0 {
		return s.raw
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage("null")
	}
	return encoded
}

// NodeRecoveryRestoreError is the cancellation sentinel, mirroring
// `throw new Error('节点恢复任务已取消')`.
var ErrNodeRecoveryCancelled = errors.New("节点恢复任务已取消")

// RecoveryClient is the subset of the node client restoreNodeInstances needs.
type RecoveryClient interface {
	Request(ctx context.Context, method, path string, body any) (json.RawMessage, error)
}

// RecoveryOptions mirrors the TypeScript options bag.
type RecoveryOptions struct {
	// Progress is called once per instance in the finally block, i.e. even for an
	// acknowledged skip.
	Progress func(completed, total, failed int)
	// Cancelled is polled before each instance and after a failed one.
	Cancelled func() bool
	// Types is the allowed workload class list; nil means ['container'].
	Types []string
}

// DecodeRecoveryState decodes a node recovery document, keeping the original
// bytes for a lossless report round-trip.
func DecodeRecoveryState(raw []byte) (RecoveryState, error) {
	var state RecoveryState
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	state.raw = append(json.RawMessage(nil), raw...)
	return state, nil
}

// RestoreNodeInstances mirrors restoreNodeInstances(client, initial, allowed, options).
//
// It returns the last acknowledged state and the number of instances that did NOT
// recover. A non-zero failed count is not an error: the caller decides, and the
// node keeps every unacknowledged identity for a retry.
func RestoreNodeInstances(
	ctx context.Context,
	client RecoveryClient,
	initial RecoveryState,
	allowed func(ResumeInstance) (bool, error),
	options RecoveryOptions,
) (RecoveryState, int, error) {
	state := initial
	failed := 0

	if (state.Locked != nil && *state.Locked) || state.Revision == nil {
		return state, 0, errors.New("invalid recovery state")
	}
	// `Number.isSafeInteger(state.revision)` alone: a negative revision is accepted
	// here exactly as in the TypeScript (parseReport is the one that clamps >= 0).
	if *state.Revision > nodeReportSafeIntegerMax || *state.Revision < -nodeReportSafeIntegerMax ||
		state.ResumeInstances == nil {
		return state, 0, errors.New("invalid recovery state")
	}

	types := options.Types
	if types == nil {
		types = []string{"container"}
	}

	total := len(initial.ResumeInstances)
	completed := 0

	for _, item := range initial.ResumeInstances {
		if options.Cancelled != nil && options.Cancelled() {
			return state, failed, ErrNodeRecoveryCancelled
		}

		restoreErr := restoreNodeInstance(ctx, client, item, &state, allowed, types)

		if restoreErr != nil {
			failed++
			if options.Cancelled != nil && options.Cancelled() {
				// The TypeScript's throw still runs the finally block, so progress
				// observes this item before the cancellation propagates.
				completed++
				if options.Progress != nil {
					options.Progress(completed, total, failed)
				}
				return state, failed, ErrNodeRecoveryCancelled
			}
		}

		completed++
		if options.Progress != nil {
			options.Progress(completed, total, failed)
		}
	}

	return state, failed, nil
}

// restoreNodeInstance runs the try-block body for one instance, returning any
// error so the caller can count it as failed (the TypeScript's catch).
func restoreNodeInstance(
	ctx context.Context,
	client RecoveryClient,
	item ResumeInstance,
	state *RecoveryState,
	allowed func(ResumeInstance) (bool, error),
	types []string,
) error {
	// `!item || typeof item.project !== 'string' || !item.project || ...`
	if item.Project == "" || item.Name == "" || item.UUID == "" || item.Frozen == nil {
		return errors.New("invalid workload identity")
	}

	query := "?project=" + jsEncodeURIComponent(item.Project)
	base := "/1.0/instances/" + jsEncodeURIComponent(item.Name)

	acknowledged := func() error {
		if state.Revision == nil {
			return errors.New("invalid recovery state")
		}
		payload := map[string]any{
			"revision": *state.Revision,
			"instance": item,
		}
		raw, err := client.Request(ctx, "POST", "/1.0/agent/security/resume", payload)
		if err != nil {
			return err
		}
		next, err := DecodeRecoveryState(raw)
		if err != nil {
			return err
		}
		*state = next
		return nil
	}

	if allowed != nil {
		permitted, err := allowed(item)
		if err != nil {
			return err
		}
		if !permitted {
			// Acknowledging a workload we deliberately do not restore is what stops
			// the node from reporting it again forever.
			return acknowledged()
		}
	}

	rawInstance, err := client.Request(ctx, "GET", base+query, nil)
	if err != nil {
		return err
	}
	var instance struct {
		Config map[string]string `json:"config"`
		Type   string            `json:"type"`
	}
	if err := json.Unmarshal(rawInstance, &instance); err != nil {
		return err
	}
	if !nodeRecoveryTypeAllowed(types, instance.Type) ||
		instance.Config["volatile.uuid"] != item.UUID {
		return errors.New("workload identity changed")
	}

	current, err := readNodeInstanceState(ctx, client, base, query)
	if err != nil {
		return err
	}
	if current == "Stopped" {
		// IncusClient waits for the native asynchronous operation before returning,
		// so the state read below observes the final result.
		if _, err := client.Request(ctx, "PUT", base+"/state"+query, map[string]any{
			"action": "start", "timeout": 30, "force": false, "stateful": false,
		}); err != nil {
			return err
		}
		if current, err = readNodeInstanceState(ctx, client, base, query); err != nil {
			return err
		}
	}

	frozen := item.Frozen != nil && *item.Frozen
	if frozen && current == "Running" {
		if _, err := client.Request(ctx, "PUT", base+"/state"+query, map[string]any{
			"action": "freeze", "timeout": 30, "force": false, "stateful": false,
		}); err != nil {
			return err
		}
		if current, err = readNodeInstanceState(ctx, client, base, query); err != nil {
			return err
		}
	}

	expected := "Running"
	if frozen {
		expected = "Frozen"
	}
	if current != expected {
		return errors.New("workload did not recover")
	}

	return acknowledged()
}

// readNodeInstanceState mirrors readState(): GET {base}/state{query} → status.
func readNodeInstanceState(ctx context.Context, client RecoveryClient, base, query string) (string, error) {
	raw, err := client.Request(ctx, "GET", base+"/state"+query, nil)
	if err != nil {
		return "", err
	}
	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	return result.Status, nil
}

// nodeRecoveryTypeAllowed mirrors `(options.types || ['container']).includes(instance.type || ”)`.
func nodeRecoveryTypeAllowed(types []string, instanceType string) bool {
	for _, candidate := range types {
		if candidate == instanceType {
			return true
		}
	}
	return false
}

// ==================== enqueueNodeRecovery ====================

// EnqueueNodeRecovery mirrors enqueueNodeRecovery(hostId, action).
//
// Acceptance is persisted first (under pg_advisory_xact_lock(700261, hostId) so
// two admins cannot create two tasks); only the call that created the row runs
// action. The action runs on a background context because the HTTP response is
// already sent by then — the request context would be cancelled.
func EnqueueNodeRecovery(ctx context.Context, hostID int32, action func(context.Context) error) (int32, error) {
	taskID, created, err := db.EnqueueNodeRecoveryTask(ctx, hostID)
	if err != nil {
		return 0, err
	}
	if !created {
		return taskID, nil
	}

	go func() {
		runCtx := context.Background()
		taskIDCopy := taskID
		err := RunOperationTask(
			runCtx,
			hostID,
			fmt.Sprintf("节点 #%d", hostID),
			OperationTaskKindNodeRecover,
			action,
			&taskIDCopy,
		)
		if err != nil {
			// Mirrors the `.catch(() => taskLog('error', ...))`: the failure is already
			// recorded on the task row by RunOperationTask.
			TaskLog(runCtx, "error", fmt.Sprintf("节点 #%d 恢复任务未完成，请检查任务日志并重试", hostID))
		}
	}()

	return taskID, nil
}

// LatestNodeRecovery mirrors latestNodeRecovery(hostId).
func LatestNodeRecovery(ctx context.Context, hostID int32) (*db.NodeRecoveryTaskView, error) {
	return db.LatestNodeRecoveryTask(ctx, hostID)
}

// ==================== JavaScript encodeURIComponent ====================

// jsEncodeURIComponent mirrors JavaScript's encodeURIComponent.
//
// Go's url.QueryEscape differs twice: it encodes '!', '\”, '(', ')' and '*'
// (which JavaScript leaves alone) and it turns a space into '+'. Instance and
// project names reach an Incus path here, so the encoding has to match.
func jsEncodeURIComponent(value string) string {
	const hex = "0123456789ABCDEF"

	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		char := value[index]
		switch {
		case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			builder.WriteByte(char)
		case char == '-', char == '_', char == '.', char == '!', char == '~',
			char == '*', char == '\'', char == '(', char == ')':
			builder.WriteByte(char)
		default:
			builder.WriteByte('%')
			builder.WriteByte(hex[char>>4])
			builder.WriteByte(hex[char&0x0F])
		}
	}
	return builder.String()
}
