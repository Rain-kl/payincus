package lib

import (
	"strings"
	"testing"
)

// TestRedactTaskLogStripsSecrets pins that a secret cannot survive the redaction
// pass, in any of the forms the TypeScript covers.
func TestRedactTaskLogStripsSecrets(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		forbidden string
	}{
		{"password assignment", "config password=hunter2 done", "hunter2"},
		{"token json pair", `{"token":"abc123secret"}`, "abc123secret"},
		{"secret with quotes", `secret: "topsecretvalue"`, "topsecretvalue"},
		{"authorization header", "authorization: Bearer sk-live-123456", "sk-live-123456"},
		{"cookie pair", "cookie=sessionvalue123", "sessionvalue123"},
		{"url credentials", "https://user:passw0rd@example.com/x", "passw0rd"},
		// `[^\s,;]+` stops at the first space, so an unquoted cloud-init value
		// only loses its first token. Verified against real Node.
		{"cloud-init unquoted", "cloud-init: AAAAKEY rest", "AAAAKEY"},
		{"cloud-init quoted", `cloud-init: "AAAAKEY"`, "AAAAKEY"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			redacted := RedactTaskLog(tc.input)
			if strings.Contains(redacted, tc.forbidden) {
				t.Fatalf("secret %q survived redaction: %q", tc.forbidden, redacted)
			}
			if !strings.Contains(redacted, "[REDACTED") {
				t.Fatalf("expected a [REDACTED…] marker, got %q", redacted)
			}
		})
	}
}

// TestRedactTaskLogStripsPrivateKeys pins the PEM block handler.
func TestRedactTaskLogStripsPrivateKeys(t *testing.T) {
	input := "before\n-----BEGIN RSA PRIVATE KEY-----\nMIIabc123\n-----END RSA PRIVATE KEY-----\nafter"
	redacted := RedactTaskLog(input)

	if strings.Contains(redacted, "MIIabc123") {
		t.Fatalf("key body survived: %q", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED_KEY]") {
		t.Fatalf("expected [REDACTED_KEY], got %q", redacted)
	}
	if !strings.Contains(redacted, "before") || !strings.Contains(redacted, "after") {
		t.Fatalf("surrounding text lost: %q", redacted)
	}
}

// TestRedactTaskLogStripsANSI pins that control sequences from captured node logs
// are removed (they corrupt the admin log viewer).
func TestRedactTaskLogStripsANSI(t *testing.T) {
	input := "\x1b[31merror\x1b[0m plain text"
	redacted := RedactTaskLog(input)

	if strings.Contains(redacted, "\x1b") {
		t.Fatalf("ANSI escape survived: %q", redacted)
	}
	if !strings.Contains(redacted, "error") || !strings.Contains(redacted, "plain text") {
		t.Fatalf("payload lost: %q", redacted)
	}
}

// TestRedactTaskLogTruncatesAfterRedacting pins the ORDER: redaction happens
// before truncation, so a secret straddling the 8000-rune boundary is still gone.
func TestRedactTaskLogTruncatesAfterRedacting(t *testing.T) {
	padding := strings.Repeat("x", operationTaskLogMaxRunes-30)
	input := padding + " password=hunter2"

	redacted := RedactTaskLog(input)
	if strings.Contains(redacted, "hunter2") {
		t.Fatal("secret survived because truncation ran before redaction")
	}
	if runes := []rune(redacted); len(runes) > operationTaskLogMaxRunes {
		t.Fatalf("length = %d, want <= %d", len(runes), operationTaskLogMaxRunes)
	}
}

// TestRedactTaskLogLeavesCleanText pins that ordinary text is untouched apart from
// whitespace normalisation by the token sanitiser.
func TestRedactTaskLogLeavesCleanText(t *testing.T) {
	input := "created instance vm-1 with 2 vcpu"
	if got := RedactTaskLog(input); got != input {
		t.Fatalf("clean text altered: %q -> %q", input, got)
	}
}

// TestOperationTaskControlUnknownTask pins that an unknown id reports nothing.
func TestOperationTaskControlUnknownTask(t *testing.T) {
	result := OperationTaskControl(987654)
	if result.CanStop || result.Stopping {
		t.Fatalf("unknown task = %#v, want zero", result)
	}
}

// TestStopOperationTaskUnknownTask pins the error for a task that is not running.
func TestStopOperationTaskUnknownTask(t *testing.T) {
	if err := StopOperationTask(987654); err == nil {
		t.Fatal("expected an error for an unknown task")
	}
}

// TestHasActiveOperationTask pins the lookup used to prevent duplicate operations
// on the same instance.
func TestHasActiveOperationTask(t *testing.T) {
	task := &OperationTask{ID: 4242, HostID: 9, Name: "vm-x"}
	operationTasksMu.Lock()
	operationTasks[task.ID] = task
	operationTasksMu.Unlock()
	defer func() {
		operationTasksMu.Lock()
		delete(operationTasks, task.ID)
		operationTasksMu.Unlock()
	}()

	if !HasActiveOperationTask(9, "vm-x") {
		t.Fatal("expected the task to be found")
	}
	if HasActiveOperationTask(9, "other") {
		t.Fatal("wrong name must not match")
	}
	if HasActiveOperationTask(8, "vm-x") {
		t.Fatal("wrong host must not match")
	}
}

// TestOperationTaskContextRoundTrip pins the context attachment.
func TestOperationTaskContextRoundTrip(t *testing.T) {
	if CurrentOperationTask(t.Context()) != nil {
		t.Fatal("a bare context must yield no task")
	}

	task := &OperationTask{ID: 7}
	ctx := OperationTaskContext(t.Context(), task)
	if got := CurrentOperationTask(ctx); got != task {
		t.Fatalf("CurrentOperationTask = %#v, want %#v", got, task)
	}
}

// TestBindOperationCancellationWithoutTask pins the no-op contract.
func TestBindOperationCancellationWithoutTask(t *testing.T) {
	unbind := BindOperationCancellation(t.Context(), func() error { return nil })
	unbind() // must not panic
}

// TestBindOperationCancellationSetsCanStop pins the control state transitions.
func TestBindOperationCancellationSetsCanStop(t *testing.T) {
	task := &OperationTask{ID: 5555}
	operationTasksMu.Lock()
	operationTasks[task.ID] = task
	operationTasksMu.Unlock()
	defer func() {
		operationTasksMu.Lock()
		delete(operationTasks, task.ID)
		operationTasksMu.Unlock()
	}()

	ctx := OperationTaskContext(t.Context(), task)
	if OperationTaskControl(task.ID).CanStop {
		t.Fatal("no cancel bound yet, so CanStop must be false")
	}

	unbind := BindOperationCancellation(ctx, func() error { return nil })
	if !OperationTaskControl(task.ID).CanStop {
		t.Fatal("after binding, CanStop must be true")
	}

	unbind()
	if OperationTaskControl(task.ID).CanStop {
		t.Fatal("after unbinding, CanStop must be false")
	}
}

// TestStopOperationTaskMarksCancelled pins the success path and the Stopping flag.
func TestStopOperationTaskMarksCancelled(t *testing.T) {
	task := &OperationTask{ID: 6666}
	operationTasksMu.Lock()
	operationTasks[task.ID] = task
	operationTasksMu.Unlock()
	defer func() {
		operationTasksMu.Lock()
		delete(operationTasks, task.ID)
		operationTasksMu.Unlock()
	}()

	called := false
	BindOperationCancellation(OperationTaskContext(t.Context(), task), func() error {
		called = true
		return nil
	})

	if err := StopOperationTask(task.ID); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("the cancel func was not invoked")
	}
	control := OperationTaskControl(task.ID)
	if control.CanStop {
		t.Fatal("a cancelled task must not be stoppable again")
	}
	if !control.Stopping {
		t.Fatal("a cancelled task must report Stopping")
	}
}

// TestEnqueueRespectsMaxLogs pins the 5000-row cap and the final-state message.
func TestEnqueueRespectsMaxLogs(t *testing.T) {
	// A non-zero id is required: `if (!ctx.id || ctx.count >= MAX_LOGS) return` runs
	// first, so a transient task (id 0) never queues anything. No DB handle is
	// configured in this test, so drain persists nothing.
	task := &OperationTask{ID: 9001}
	task.count = operationTaskMaxLogs - 1

	task.enqueue("last message", "info", "server")
	// The drain goroutine starts as soon as something is queued, so every read below
	// must take the same mutex the writer takes.
	task.mu.Lock()
	count := task.count
	queueLen := len(task.queue)
	var message string
	if queueLen > 0 {
		message = task.queue[0].message
	}
	task.mu.Unlock()

	if count != operationTaskMaxLogs {
		t.Fatalf("count = %d, want %d", count, operationTaskMaxLogs)
	}
	if queueLen != 1 {
		t.Fatalf("queue length = %d, want 1", queueLen)
	}
	if !strings.Contains(message, "日志已达到上限") {
		t.Fatalf("final message = %q", message)
	}

	// Any further enqueue is dropped by the count cap.
	task.enqueue("dropped", "info", "server")
	task.waitForQueue()
	task.mu.Lock()
	afterLen := len(task.queue)
	task.mu.Unlock()
	if afterLen != 0 {
		t.Fatalf("queue grew past the cap: %d", afterLen)
	}
}

// TestEnqueueBackpressureDropsWhenPending pins the 100-pending guard.
func TestEnqueueBackpressureDropsWhenPending(t *testing.T) {
	task := &OperationTask{ID: 9002}
	task.pending = operationTaskMaxPending

	task.enqueue("should be dropped", "info", "server")
	task.mu.Lock()
	queueLen := len(task.queue)
	dropped := task.dropped
	task.mu.Unlock()
	if queueLen != 0 {
		t.Fatalf("queue length = %d, want 0", queueLen)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
}
