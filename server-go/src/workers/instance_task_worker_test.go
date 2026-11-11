package workers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

func newTestInstanceWorker(store instanceTaskStore, executor instanceTaskExecutor, notifier instanceTaskNotifier, poll, timeout time.Duration) *instanceTaskWorker {
	return &instanceTaskWorker{
		store:      store,
		executor:   executor,
		notifier:   notifier,
		backoff:    newWorkerDBBackoff("test"),
		poll:       poll,
		timeout:    timeout,
		lightLimit: instanceTaskLightConcurrency,
		now:        time.Now,
		running:    map[int32]struct{}{},
	}
}

// TestInstanceTaskWorkerClaimIsExclusive pins the claim contract: two workers
// racing over the same PENDING row, only one of them may execute it. The fake
// store models the real conditional UPDATE (status must still be PENDING, checked
// and set under one mutex), so a second claim returns nil — exactly what
// RowsAffected == 0 produces against PostgreSQL.
func TestInstanceTaskWorkerClaimIsExclusive(t *testing.T) {
	task := &instanceTaskRecord{ID: 1, InstanceID: 10, HostID: 5, UserID: 3, TaskType: "start"}
	store := newFakeInstanceTaskStore(task)
	executor := &fakeInstanceExecutor{}
	notifier := &recordingNotifier{}

	first := newTestInstanceWorker(store, executor, notifier, time.Hour, time.Hour)
	second := newTestInstanceWorker(store, executor, notifier, time.Hour, time.Hour)

	briefs := []instanceTaskBrief{{ID: 1, HostID: 5, TaskType: "start"}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		first.processHostTasks(context.Background(), 5, briefs)
	}()
	go func() {
		defer wg.Done()
		second.processHostTasks(context.Background(), 5, briefs)
	}()
	wg.Wait()

	if got := store.claimCount(); got != 1 {
		t.Fatalf("claims = %d, want 1 (only one worker may win the row)", got)
	}
	if got := executor.callCount(); got != 1 {
		t.Fatalf("executions = %d, want 1", got)
	}
	if status, _, _ := store.statusOf(1); status != "COMPLETED" {
		t.Fatalf("status = %q, want COMPLETED", status)
	}
}

func TestInstanceTaskWorkerSuccessMarksCompleted(t *testing.T) {
	task := &instanceTaskRecord{ID: 7, InstanceID: 10, HostID: 5, UserID: 3, TaskType: "start"}
	store := newFakeInstanceTaskStore(task)
	notifier := &recordingNotifier{}
	worker := newTestInstanceWorker(store, &fakeInstanceExecutor{}, notifier, time.Hour, time.Hour)

	worker.processQueue(context.Background())

	status, _, _ := store.statusOf(7)
	if status != "COMPLETED" {
		t.Fatalf("status = %q, want COMPLETED", status)
	}
	if len(notifier.snapshot().failures) != 0 {
		t.Fatalf("unexpected failure notifications: %v", notifier.snapshot().failures)
	}
}

func TestInstanceTaskWorkerFailureMarksFailedAndNotifies(t *testing.T) {
	task := &instanceTaskRecord{ID: 8, InstanceID: 10, HostID: 5, UserID: 3, TaskType: "stop"}
	store := newFakeInstanceTaskStore(task)
	executor := &fakeInstanceExecutor{err: errors.New("incus exploded")}
	notifier := &recordingNotifier{}
	worker := newTestInstanceWorker(store, executor, notifier, time.Hour, time.Hour)

	worker.processQueue(context.Background())

	status, _, message := store.statusOf(8)
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	if message != "incus exploded" {
		t.Fatalf("error = %q, want the executor error", message)
	}
	failures := notifier.snapshot().failures
	if len(failures) != 1 || failures[0] != "incus exploded" {
		t.Fatalf("failure notifications = %v, want one carrying the error", failures)
	}
}

// TestInstanceTaskWorkerCreateRetryRequeues pins the create-only retry branch: a
// node-availability error returns the task to PENDING with retry_count+1 and does
// not notify the user (the reservation is still alive).
func TestInstanceTaskWorkerCreateRetryRequeues(t *testing.T) {
	task := &instanceTaskRecord{
		ID: 9, InstanceID: 10, HostID: 5, UserID: 3, TaskType: "create", RetryCount: 0,
		Payload: []byte(`{"config":{"a":1},"userId":3,"resources":{"cpu":1,"memory":512,"disk":1024}}`),
	}
	store := newFakeInstanceTaskStore(task)
	executor := &fakeInstanceExecutor{err: errors.New("HOST_OFFLINE: 节点离线")}
	notifier := &recordingNotifier{}
	worker := newTestInstanceWorker(store, executor, notifier, time.Hour, time.Hour)

	worker.processQueue(context.Background())
	waitFor(t, 2*time.Second, func() bool {
		status, _, _ := store.statusOf(9)
		return status == "PENDING"
	})

	status, retryCount, message := store.statusOf(9)
	if status != "PENDING" {
		t.Fatalf("status = %q, want PENDING", status)
	}
	if retryCount != 1 {
		t.Fatalf("retryCount = %d, want 1", retryCount)
	}
	if want := "等待节点恢复后重试（第 1/5 次）：HOST_OFFLINE: 节点离线"; message != want {
		t.Fatalf("error = %q, want %q", message, want)
	}
	if failures := notifier.snapshot().failures; len(failures) != 0 {
		t.Fatalf("retryable create must not notify the user, got %v", failures)
	}
}

func TestInstanceTaskWorkerCreateRetryExhaustedFails(t *testing.T) {
	task := &instanceTaskRecord{
		ID: 11, InstanceID: 10, HostID: 5, UserID: 3, TaskType: "create", RetryCount: instanceCreateTaskMaxRetries,
		Payload: []byte(`{"config":{"a":1},"userId":3,"resources":{"cpu":1,"memory":512,"disk":1024}}`),
	}
	store := newFakeInstanceTaskStore(task)
	executor := &fakeInstanceExecutor{err: errors.New("节点不可用")}
	notifier := &recordingNotifier{}
	worker := newTestInstanceWorker(store, executor, notifier, time.Hour, time.Hour)

	worker.processQueue(context.Background())
	waitFor(t, 2*time.Second, func() bool {
		status, _, _ := store.statusOf(11)
		return status == "FAILED"
	})

	status, _, message := store.statusOf(11)
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	if message != "节点不可用" {
		t.Fatalf("error = %q", message)
	}
	if failures := notifier.snapshot().failures; len(failures) != 1 {
		t.Fatalf("failure notifications = %v, want 1", failures)
	}
}

// TestInstanceTaskWorkerStopDrainsInFlight pins the stop contract: after cancel the
// loop stops claiming, but it does not return until the running task has finished.
func TestInstanceTaskWorkerStopDrainsInFlight(t *testing.T) {
	task := &instanceTaskRecord{ID: 12, InstanceID: 10, HostID: 5, UserID: 3, TaskType: "start"}
	store := newFakeInstanceTaskStore(task)
	block := make(chan struct{})
	started := make(chan int32, 1)
	executor := &fakeInstanceExecutor{block: block, started: started, ignoreCtx: true}
	notifier := &recordingNotifier{}
	worker := newTestInstanceWorker(store, executor, notifier, 5*time.Millisecond, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	worker.start(ctx, &wg)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never started the task")
	}

	cancel()
	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		t.Fatal("stop returned while an in-flight task was still running")
	case <-time.After(40 * time.Millisecond):
	}

	close(block)
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return after the in-flight task finished")
	}

	if got := executor.callCount(); got != 1 {
		t.Fatalf("executions = %d, want 1", got)
	}
	if got := store.claimCount(); got != 1 {
		t.Fatalf("claims = %d, want 1 (no new task after cancel)", got)
	}
}

func TestInstanceTaskWorkerTimeoutCleanupNotifies(t *testing.T) {
	notice := instanceTaskFailureNotice{ID: 20, UserID: 3, InstanceID: 10, HostID: 5, TaskType: "rebuild"}
	store := newFakeInstanceTaskStore()
	store.notices = []instanceTaskFailureNotice{notice}
	notifier := &recordingNotifier{}
	worker := newTestInstanceWorker(store, &fakeInstanceExecutor{}, notifier, time.Hour, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	worker.start(ctx, &wg)

	waitFor(t, 2*time.Second, func() bool {
		return len(notifier.snapshot().failures) == 1
	})
	cancel()
	wg.Wait()

	if got := notifier.snapshot().failures[0]; got != "任务执行超时" {
		t.Fatalf("failure reason = %q, want 任务执行超时", got)
	}
}

// TestInstanceTaskWorkerHeavyBlocksLight pins the per-host ordering: a running
// heavy task suppresses light claims for the same host.
func TestInstanceTaskWorkerHeavyBlocksLight(t *testing.T) {
	task := &instanceTaskRecord{ID: 21, InstanceID: 10, HostID: 5, UserID: 3, TaskType: "start"}
	store := newFakeInstanceTaskStore(task)
	store.heavy = true
	executor := &fakeInstanceExecutor{}
	worker := newTestInstanceWorker(store, executor, &recordingNotifier{}, time.Hour, time.Hour)

	worker.processHostTasks(context.Background(), 5, []instanceTaskBrief{{ID: 21, HostID: 5, TaskType: "start"}})

	if got := store.claimCount(); got != 0 {
		t.Fatalf("claims = %d, want 0 while a heavy task is PROCESSING", got)
	}
	if got := executor.callCount(); got != 0 {
		t.Fatalf("executions = %d, want 0", got)
	}
}

func TestIsRetryableCreateError(t *testing.T) {
	retryable := []string{"HOST_OFFLINE", "node said: 节点离线", "dial tcp: i/o timeout", "socket hang up"}
	for _, message := range retryable {
		if !isRetryableCreateError(message) {
			t.Errorf("%q should be retryable", message)
		}
	}
	nonRetryable := []string{"余额不足", "INSUFFICIENT_BALANCE", "参数错误"}
	for _, message := range nonRetryable {
		if isRetryableCreateError(message) {
			t.Errorf("%q must not be retryable", message)
		}
	}
}

func TestIsAlreadyInStateError(t *testing.T) {
	cases := []struct {
		message string
		target  string
		want    bool
	}{
		{"The container is already running", "running", true},
		{"instance is not running", "running", false},
		{"The instance is already stopped", "stopped", true},
		{"instance is not running", "stopped", true},
		{"permission denied", "stopped", false},
	}
	for _, tc := range cases {
		got := isAlreadyInStateError(fmt.Errorf("%s", tc.message), tc.target)
		if got != tc.want {
			t.Errorf("isAlreadyInStateError(%q, %q) = %v, want %v", tc.message, tc.target, got, tc.want)
		}
	}
}

func TestExtractInstanceAddresses(t *testing.T) {
	state := parseInstanceState([]byte(`{
		"status": "Running",
		"network": {
			"lo": {"addresses": [{"family": "inet", "address": "127.0.0.1", "scope": "local"}]},
			"eth0": {"addresses": [
				{"family": "inet6", "address": "fe80::1", "scope": "link"},
				{"family": "inet", "address": "10.10.0.5", "scope": "global"},
				{"family": "inet6", "address": "fd00::5", "scope": "global"},
				{"family": "inet6", "address": "2001:db8::5", "scope": "global"}
			]}
		}
	}`))

	ipv4 := extractIPv4(state)
	if ipv4 == nil || *ipv4 != "10.10.0.5" {
		t.Fatalf("ipv4 = %v, want 10.10.0.5", ipv4)
	}
	ipv6 := extractIPv6(state)
	if ipv6 == nil || *ipv6 != "2001:db8::5" {
		t.Fatalf("ipv6 = %v, want 2001:db8::5 (ULA and link-local skipped)", ipv6)
	}
	if state.Status != "Running" {
		t.Fatalf("status = %q, want Running", state.Status)
	}
}

func TestUnmarshalCreatePayload(t *testing.T) {
	if _, err := unmarshalCreatePayload(nil); err == nil {
		t.Fatal("nil payload must be rejected")
	}
	if _, err := unmarshalCreatePayload([]byte(`{"userId":1}`)); err == nil {
		t.Fatal("payload without config must be rejected")
	}
	payload, err := unmarshalCreatePayload([]byte(`{"config":{"x":1},"userId":3,"resources":{"cpu":2,"memory":1024,"disk":2048}}`))
	if err != nil {
		t.Fatalf("valid payload: %v", err)
	}
	if payload.UserID != 3 || payload.Resources.CPU != 2 {
		t.Fatalf("parsed payload = %+v", payload)
	}
}

func TestInstanceTaskExecutorRequiresRegisteredExecutor(t *testing.T) {
	executor := newPGInstanceTaskExecutor()
	for _, taskType := range []string{string(models.InstanceTaskTypeClone), string(models.InstanceTaskTypeRecreate), string(models.InstanceTaskTypeChangeHost)} {
		err := executor.Execute(context.Background(), &instanceTaskRecord{ID: 1, InstanceID: 1, HostID: 1, TaskType: taskType})
		if err == nil {
			t.Fatalf("%s must fail closed without a registered executor", taskType)
		}
	}
}

// TestInstanceTaskWorkerNoDoubleExecutionAfterCrash models a restart: a row left
// PROCESSING is never claimed again (the stale sweep fails it first), and a second
// claim attempt returns nil.
func TestInstanceTaskWorkerNoDoubleExecutionAfterCrash(t *testing.T) {
	task := &instanceTaskRecord{ID: 30, InstanceID: 10, HostID: 5, UserID: 3, TaskType: "start", Status: "PROCESSING"}
	store := newFakeInstanceTaskStore(task)
	executor := &fakeInstanceExecutor{}
	worker := newTestInstanceWorker(store, executor, &recordingNotifier{}, time.Hour, time.Hour)

	worker.processQueue(context.Background())

	if got := executor.callCount(); got != 0 {
		t.Fatalf("executions = %d, want 0 for a row already PROCESSING", got)
	}
	if got := store.claimCount(); got != 0 {
		t.Fatalf("claims = %d, want 0", got)
	}
}

func TestInstanceTaskWorkerClaimErrorArmsBackoff(t *testing.T) {
	task := &instanceTaskRecord{ID: 31, InstanceID: 10, HostID: 5, UserID: 3, TaskType: "start"}
	store := newFakeInstanceTaskStore(task)
	store.claimErr = errors.New("too many clients")
	worker := newTestInstanceWorker(store, &fakeInstanceExecutor{}, &recordingNotifier{}, time.Hour, time.Hour)

	worker.processQueue(context.Background())
	worker.processQueue(context.Background())

	if !worker.backoff.shouldSkip() {
		t.Fatal("a transient claim error must arm the database backoff")
	}
	if got := store.claimCount(); got != 0 {
		t.Fatalf("claims = %d", got)
	}
}

// TestInstanceTaskWorkerLateCompletionDoesNotOverwriteTimeout pins the guarded
// terminal transitions: if the timeout sweep already failed the row while the
// executor was still running, a late "success" must not resurrect it.
func TestInstanceTaskWorkerLateCompletionDoesNotOverwriteTimeout(t *testing.T) {
	task := &instanceTaskRecord{ID: 40, InstanceID: 10, HostID: 5, UserID: 3, TaskType: "start"}
	store := newFakeInstanceTaskStore(task)
	executor := &lateFailureExecutor{store: store, taskID: 40}
	worker := newTestInstanceWorker(store, executor, &recordingNotifier{}, time.Hour, time.Hour)

	worker.processQueue(context.Background())

	if status, _, _ := store.statusOf(40); status != "FAILED" {
		t.Fatalf("status = %q, want FAILED (late completion must not overwrite)", status)
	}
}

// lateFailureExecutor simulates the timeout sweep winning the race: it flips the
// row to FAILED while "executing", then reports success to the queue loop.
type lateFailureExecutor struct {
	store  *fakeInstanceTaskStore
	taskID int32
}

func (e *lateFailureExecutor) Execute(ctx context.Context, task *instanceTaskRecord) error {
	e.store.mu.Lock()
	if row, ok := e.store.tasks[e.taskID]; ok {
		row.Status = "FAILED"
		message := "任务执行超时"
		row.Error = &message
	}
	e.store.mu.Unlock()
	return nil
}
