package workers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func newTestRestoreWorker(store restoreTaskStore, runner restoreTaskRunner, notifier backupTaskNotifier, poll, timeout time.Duration) *restoreTaskWorker {
	return &restoreTaskWorker{
		store:        store,
		runner:       runner,
		notifier:     notifier,
		backoff:      newWorkerDBBackoff("test"),
		poll:         poll,
		timeoutCheck: timeout,
		now:          time.Now,
		running:      map[int32]struct{}{},
	}
}

func TestRestoreTaskWorkerClaimIsExclusive(t *testing.T) {
	task := &restoreTaskRecord{ID: 1, InstanceID: 10, HostID: 5, UserID: 3, OriginalInstanceName: "vm", OriginalIncusID: "u3-abc"}
	store := newFakeRestoreStore(task)
	runner := &fakeRestoreRunner{}
	notifier := &recordingNotifier{}

	first := newTestRestoreWorker(store, runner, notifier, time.Hour, time.Hour)
	second := newTestRestoreWorker(store, runner, notifier, time.Hour, time.Hour)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		first.processQueue(context.Background())
	}()
	go func() {
		defer wg.Done()
		second.processQueue(context.Background())
	}()
	wg.Wait()
	// processQueue launches execution asynchronously; wait for the status to settle.
	waitFor(t, 2*time.Second, func() bool {
		status, _ := store.statusOf(1)
		return status == "COMPLETED"
	})

	if claims, _, _, _ := store.counters(); claims != 1 {
		t.Fatalf("claims = %d, want 1", claims)
	}
	if got := runner.callCount(); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
}

func TestRestoreTaskWorkerSuccessNotifies(t *testing.T) {
	task := &restoreTaskRecord{ID: 2, InstanceID: 10, HostID: 5, UserID: 3, OriginalInstanceName: "vm", OriginalIncusID: "u3-abc"}
	store := newFakeRestoreStore(task)
	notifier := &recordingNotifier{}
	worker := newTestRestoreWorker(store, &fakeRestoreRunner{}, notifier, time.Hour, time.Hour)

	worker.processQueue(context.Background())
	waitFor(t, 2*time.Second, func() bool {
		_, completed, _, _ := store.counters()
		return completed == 1
	})

	if status, _ := store.statusOf(2); status != "COMPLETED" {
		t.Fatalf("status = %q, want COMPLETED", status)
	}
	waitFor(t, 2*time.Second, func() bool { return len(notifier.snapshot().restored) == 1 })
}

func TestRestoreTaskWorkerFailureCarriesManualIntervention(t *testing.T) {
	task := &restoreTaskRecord{ID: 3, InstanceID: 10, HostID: 5, UserID: 3, OriginalInstanceName: "vm", OriginalIncusID: "u3-abc"}
	store := newFakeRestoreStore(task)
	progress := restoreProgressManualIntervention
	runner := &fakeRestoreRunner{err: &restoreFailure{message: "restore failed; temp kept", progress: &progress}}
	notifier := &recordingNotifier{}
	worker := newTestRestoreWorker(store, runner, notifier, time.Hour, time.Hour)

	worker.processQueue(context.Background())
	waitFor(t, 2*time.Second, func() bool {
		_, _, failed, _ := store.counters()
		return failed == 1
	})

	status, gotProgress := store.statusOf(3)
	if status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	if gotProgress == nil || *gotProgress != restoreProgressManualIntervention {
		t.Fatalf("progress = %v, want manual_intervention", gotProgress)
	}
	waitFor(t, 2*time.Second, func() bool { return len(notifier.snapshot().restoreFailed) == 1 })
}

func TestRestoreTaskWorkerStopDrainsInFlight(t *testing.T) {
	task := &restoreTaskRecord{ID: 4, InstanceID: 10, HostID: 5, UserID: 3, OriginalInstanceName: "vm", OriginalIncusID: "u3-abc"}
	store := newFakeRestoreStore(task)
	block := make(chan struct{})
	started := make(chan int32, 1)
	runner := &fakeRestoreRunner{block: block, started: started, ignoreCtx: true}
	worker := newTestRestoreWorker(store, runner, &recordingNotifier{}, 5*time.Millisecond, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	worker.start(ctx, &wg)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("restore task never started")
	}

	cancel()
	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("stop returned before the in-flight restore finished")
	case <-time.After(40 * time.Millisecond):
	}

	close(block)
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return after the restore finished")
	}
	if claims, _, _, _ := store.counters(); claims != 1 {
		t.Fatalf("claims = %d, want 1", claims)
	}
}

func TestRestoreTaskWorkerTimeoutCleanup(t *testing.T) {
	store := newFakeRestoreStore()
	worker := newTestRestoreWorker(store, &fakeRestoreRunner{}, &recordingNotifier{}, time.Hour, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	worker.start(ctx, &wg)
	waitFor(t, 2*time.Second, func() bool {
		_, _, _, timeoutCalls := store.counters()
		return timeoutCalls >= 1
	})
	cancel()
	wg.Wait()
}

func TestRestoreTaskWorkerRunnerErrorWithoutMarker(t *testing.T) {
	task := &restoreTaskRecord{ID: 5, InstanceID: 10, HostID: 5, UserID: 3, OriginalInstanceName: "vm", OriginalIncusID: "u3-abc"}
	store := newFakeRestoreStore(task)
	worker := newTestRestoreWorker(store, &fakeRestoreRunner{err: errors.New("宿主机不存在")}, &recordingNotifier{}, time.Hour, time.Hour)

	worker.processQueue(context.Background())
	waitFor(t, 2*time.Second, func() bool {
		_, _, failed, _ := store.counters()
		return failed == 1
	})

	_, progress := store.statusOf(5)
	if progress != nil {
		t.Fatalf("progress = %v, want nil", progress)
	}
}
