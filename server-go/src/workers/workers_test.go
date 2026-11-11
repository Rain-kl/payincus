package workers

import (
	"context"
	"errors"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestStartWorkersDisabledStartsNothing pins DISABLE_WORKERS=1: StartWorkers must
// return immediately, start no goroutine, and not touch the database (which is not
// even initialized in this test).
func TestStartWorkersDisabledStartsNothing(t *testing.T) {
	t.Setenv("DISABLE_WORKERS", "1")
	atomic.StoreInt32(&activeWorkers, 0)
	setGlobalInstanceWorker(nil)

	stop := StartWorkers(context.Background())

	if got := atomic.LoadInt32(&activeWorkers); got != 0 {
		t.Fatalf("activeWorkers = %d, want 0", got)
	}
	globalInstanceWorkerMu.RLock()
	worker := globalInstanceWorker
	globalInstanceWorkerMu.RUnlock()
	if worker != nil {
		t.Fatal("DISABLE_WORKERS must not install the global instance worker")
	}

	// The returned stop is a no-op and must be safe to call twice.
	stop()
	stop()
}

func TestWorkersDisabledEnv(t *testing.T) {
	t.Setenv("DISABLE_WORKERS", "1")
	if !workersDisabled() {
		t.Fatal("DISABLE_WORKERS=1 must disable workers")
	}
	t.Setenv("DISABLE_WORKERS", "0")
	if workersDisabled() {
		t.Fatal("DISABLE_WORKERS=0 is not the documented disable value")
	}
}

// TestStartWorkerLoopsStopsAllLoops exercises the same start/stop contract as
// StartWorkers with fake-backed workers, and asserts the stop closure is
// idempotent.
func TestStartWorkerLoopsStopsAllLoops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	instanceStore := newFakeInstanceTaskStore()
	restoreStore := newFakeRestoreStore()
	uploadStore := newFakeUploadStore()
	emailStore := &fakeEmailStore{}

	set := &workerSet{
		instance: newTestInstanceWorker(instanceStore, &fakeInstanceExecutor{}, &recordingNotifier{}, 10*time.Millisecond, time.Hour),
		restore:  newTestRestoreWorker(restoreStore, &fakeRestoreRunner{}, &recordingNotifier{}, 10*time.Millisecond, time.Hour),
		upload:   newTestUploadWorker(uploadStore, &fakeUploadRunner{}, &recordingNotifier{}, 10*time.Millisecond, time.Hour),
		email:    newTestEmailWorker(emailStore, &fakeEmailSender{}, 10*time.Millisecond, time.Hour),
	}
	stop := startWorkerLoops(ctx, set)

	time.Sleep(30 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not wait for the loops to exit")
	}

	// Idempotent: a second call must return without deadlocking.
	stop()
}

func TestWorkerDBBackoff(t *testing.T) {
	t.Setenv("DB_WORKER_BACKOFF_MS", "20")
	backoff := newWorkerDBBackoff("test")

	if backoff.shouldSkip() {
		t.Fatal("a fresh backoff must not skip")
	}
	backoff.arm(errors.New("INSUFFICIENT_BALANCE"))
	if backoff.shouldSkip() {
		t.Fatal("a business error must not arm the backoff")
	}
	backoff.arm(&pgconn.PgError{Code: "53300"})
	if !backoff.shouldSkip() {
		t.Fatal("too_many_connections must arm the backoff")
	}
	time.Sleep(40 * time.Millisecond)
	if backoff.shouldSkip() {
		t.Fatal("backoff should have expired")
	}

	backoff.arm(errors.New("remaining connection slots are reserved"))
	if !backoff.shouldSkip() {
		t.Fatal("the message classifier must arm the backoff")
	}
	backoff.reset()
	if backoff.shouldSkip() {
		t.Fatal("reset must clear the backoff")
	}
}

func TestIsTransientDatabaseError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"business", errors.New("实例不存在"), false},
		{"shutdown", context.Canceled, false},
		{"pg too many clients", &pgconn.PgError{Code: "53300"}, true},
		{"pg admin shutdown", &pgconn.PgError{Code: "57P01"}, true},
		{"pg serialization", &pgconn.PgError{Code: "40001"}, true},
		{"message timeout", errors.New("timeout exceeded when trying to connect"), true},
		{"message terminated", errors.New("terminating connection due to administrator command"), true},
		{"message refused", errors.New("dial tcp: connection refused"), true},
	}
	for _, tc := range cases {
		if got := isTransientDatabaseError(tc.err); got != tc.want {
			t.Errorf("%s: isTransientDatabaseError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestParsePositiveInt(t *testing.T) {
	cases := map[string]int{
		"":     15,
		"abc":  15,
		"0":    15,
		"-5":   15,
		"250":  250,
		" 42 ": 42,
	}
	for input, want := range cases {
		if got := parsePositiveInt(input, 15); got != want {
			t.Errorf("parsePositiveInt(%q) = %d, want %d", input, got, want)
		}
	}
}

// TestWorkerSetStopWaitsForRunningTask is the workerSet-level version of the drain
// test: a fake instance worker is mid-execution when stop is called, and stop must
// not return until the task is released.
func TestWorkerSetStopWaitsForRunningTask(t *testing.T) {
	task := &instanceTaskRecord{ID: 1, InstanceID: 10, HostID: 5, UserID: 3, TaskType: "start"}
	store := newFakeInstanceTaskStore(task)
	block := make(chan struct{})
	started := make(chan int32, 1)
	executor := &fakeInstanceExecutor{block: block, started: started, ignoreCtx: true}

	ctx, cancel := context.WithCancel(context.Background())
	set := &workerSet{
		instance: newTestInstanceWorker(store, executor, &recordingNotifier{}, 5*time.Millisecond, time.Hour),
		restore:  newTestRestoreWorker(newFakeRestoreStore(), &fakeRestoreRunner{}, &recordingNotifier{}, time.Hour, time.Hour),
		upload:   newTestUploadWorker(newFakeUploadStore(), &fakeUploadRunner{}, &recordingNotifier{}, time.Hour, time.Hour),
		email:    newTestEmailWorker(&fakeEmailStore{}, &fakeEmailSender{}, time.Hour, time.Hour),
	}
	stop := startWorkerLoops(ctx, set)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("instance task never started")
	}

	cancel()
	var once sync.Once
	stopped := make(chan struct{})
	go func() {
		stop()
		once.Do(func() { close(stopped) })
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned while a task was still running")
	case <-time.After(40 * time.Millisecond):
	}

	close(block)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return after the task finished")
	}
}

// TestClaimSQLUsesConditionalTransitions is a source guard, in the spirit of
// db/raw_insert_guard_test.go. The unit tests above use a fake that models a
// compare-and-set; this guard proves the production SQL really is a conditional
// transition (`WHERE ... status = 'PENDING'` on claims, `status = 'PROCESSING'` on
// terminal writes) and that every claim takes both advisory locks. Without that
// WHERE clause the fake would still pass while two workers could double-execute.
func TestClaimSQLUsesConditionalTransitions(t *testing.T) {
	source, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	text := string(source)

	claims := []struct {
		name    string
		pattern string
		want    int
	}{
		{"claim flips PENDING rows only", `(?s)SET status = 'PROCESSING'.*?WHERE id = \$1 AND status = 'PENDING'`, 3},
		{"completion flips PROCESSING rows only", `(?s)SET status = 'COMPLETED'.*?WHERE id = \$1 AND status = 'PROCESSING'`, 3},
		{"failure flips PROCESSING rows only", `(?s)SET status = 'FAILED'.*?WHERE id = \$1 AND status = 'PROCESSING'`, 3},
		{"host claim advisory lock", `HostTaskClaimLockNamespace`, 3},
		{"instance claim advisory lock", `InstanceOperationLockNamespace`, 3},
	}
	for _, check := range claims {
		matches := regexp.MustCompile(check.pattern).FindAllStringIndex(text, -1)
		if len(matches) < check.want {
			t.Errorf("%s: found %d guarded statements, want at least %d", check.name, len(matches), check.want)
		}
	}

	emailSource, err := os.ReadFile("../db/host_notification_email_tasks.go")
	if err != nil {
		t.Fatalf("read host_notification_email_tasks.go: %v", err)
	}
	if !regexp.MustCompile(`(?s)SET status = 'PROCESSING'.*?WHERE id = \$1 AND status = 'PENDING'`).
		Match(emailSource) {
		t.Error("host notification email claim is not guarded by status = 'PENDING'")
	}
	if !regexp.MustCompile(`retry_count = \$4`).Match(emailSource) {
		t.Error("host notification email retry/fail is not guarded by retry_count")
	}
}
