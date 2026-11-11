package workers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func newTestUploadWorker(store uploadTaskStore, runner uploadTaskRunner, notifier backupTaskNotifier, poll, timeout time.Duration) *backupUploadWorker {
	return &backupUploadWorker{
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

func TestBackupUploadWorkerClaimIsExclusive(t *testing.T) {
	task := &uploadTaskRecord{ID: 1, UserID: 3, InstanceID: 10, BackupID: 20, HostID: 5, StorageConfigID: 1}
	store := newFakeUploadStore(task)
	runner := &fakeUploadRunner{remoteName: "vm-1.tar.gz"}
	notifier := &recordingNotifier{}

	first := newTestUploadWorker(store, runner, notifier, time.Hour, time.Hour)
	second := newTestUploadWorker(store, runner, notifier, time.Hour, time.Hour)

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
	waitFor(t, 2*time.Second, func() bool {
		_, completed, _, _ := store.counters()
		return completed == 1
	})

	if claims, _, _, _ := store.counters(); claims != 1 {
		t.Fatalf("claims = %d, want 1", claims)
	}
	if got := runner.callCount(); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
}

func TestBackupUploadWorkerSuccess(t *testing.T) {
	task := &uploadTaskRecord{ID: 2, UserID: 3, InstanceID: 10, BackupID: 20, HostID: 5, StorageConfigID: 1}
	store := newFakeUploadStore(task)
	size := int64(1024)
	runner := &fakeUploadRunner{remoteName: "vm-1.tar.gz", fileSize: &size}
	notifier := &recordingNotifier{}
	worker := newTestUploadWorker(store, runner, notifier, time.Hour, time.Hour)

	worker.processQueue(context.Background())
	waitFor(t, 2*time.Second, func() bool {
		_, completed, _, _ := store.counters()
		return completed == 1
	})

	status, _, remote := store.statusOf(2)
	if status != "COMPLETED" {
		t.Fatalf("status = %q, want COMPLETED", status)
	}
	if remote == nil || *remote != "vm-1.tar.gz" {
		t.Fatalf("remote file = %v", remote)
	}
	waitFor(t, 2*time.Second, func() bool { return len(notifier.snapshot().uploaded) == 1 })
}

// TestBackupUploadWorkerRetryableFailureRequeues pins the retry policy: a
// transport error below the retry limit returns the task to PENDING, increments
// retry_count and logs an "info" retry entry.
func TestBackupUploadWorkerRetryableFailureRequeues(t *testing.T) {
	task := &uploadTaskRecord{ID: 3, UserID: 3, InstanceID: 10, BackupID: 20, HostID: 5, StorageConfigID: 1, RetryCount: 0}
	store := newFakeUploadStore(task)
	runner := &fakeUploadRunner{err: errors.New("read tcp: i/o timeout")}
	notifier := &recordingNotifier{}
	worker := newTestUploadWorker(store, runner, notifier, time.Hour, time.Hour)

	worker.processQueue(context.Background())
	waitFor(t, 2*time.Second, func() bool {
		_, _, _, requeued := store.counters()
		return requeued == 1
	})

	status, retryCount, _ := store.statusOf(3)
	if status != "PENDING" {
		t.Fatalf("status = %q, want PENDING", status)
	}
	if retryCount != 1 {
		t.Fatalf("retryCount = %d, want 1", retryCount)
	}
	waitFor(t, 2*time.Second, func() bool { return len(notifier.snapshot().uploadRetry) == 1 })
	if got := notifier.snapshot().uploadRetry[0]; got != "read tcp: i/o timeout" {
		t.Fatalf("retry log = %q", got)
	}
}

func TestBackupUploadWorkerNonRetryableFailure(t *testing.T) {
	task := &uploadTaskRecord{ID: 4, UserID: 3, InstanceID: 10, BackupID: 20, HostID: 5, StorageConfigID: 1, RetryCount: 0}
	store := newFakeUploadStore(task)
	runner := &fakeUploadRunner{err: errors.New("invalid credentials")}
	notifier := &recordingNotifier{}
	worker := newTestUploadWorker(store, runner, notifier, time.Hour, time.Hour)

	worker.processQueue(context.Background())
	waitFor(t, 2*time.Second, func() bool {
		_, _, failed, _ := store.counters()
		return failed == 1
	})

	if status, _, _ := store.statusOf(4); status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	waitFor(t, 2*time.Second, func() bool { return len(notifier.snapshot().uploadFailed) == 1 })
}

func TestBackupUploadWorkerRetryExhaustedFails(t *testing.T) {
	task := &uploadTaskRecord{ID: 5, UserID: 3, InstanceID: 10, BackupID: 20, HostID: 5, StorageConfigID: 1, RetryCount: uploadMaxRetryCount}
	store := newFakeUploadStore(task)
	runner := &fakeUploadRunner{err: errors.New("connection reset")}
	notifier := &recordingNotifier{}
	worker := newTestUploadWorker(store, runner, notifier, time.Hour, time.Hour)

	worker.processQueue(context.Background())
	waitFor(t, 2*time.Second, func() bool {
		_, _, failed, _ := store.counters()
		return failed == 1
	})

	if status, _, _ := store.statusOf(5); status != "FAILED" {
		t.Fatalf("status = %q, want FAILED", status)
	}
	if _, _, _, requeued := store.counters(); requeued != 0 {
		t.Fatalf("requeued = %d, want 0", requeued)
	}
}

func TestBackupUploadWorkerStopDrainsInFlight(t *testing.T) {
	task := &uploadTaskRecord{ID: 6, UserID: 3, InstanceID: 10, BackupID: 20, HostID: 5, StorageConfigID: 1}
	store := newFakeUploadStore(task)
	block := make(chan struct{})
	started := make(chan int32, 1)
	runner := &fakeUploadRunner{block: block, started: started, ignoreCtx: true, remoteName: "x.tar.gz"}
	worker := newTestUploadWorker(store, runner, &recordingNotifier{}, 5*time.Millisecond, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	worker.start(ctx, &wg)

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upload task never started")
	}

	cancel()
	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("stop returned before the in-flight upload finished")
	case <-time.After(40 * time.Millisecond):
	}

	close(block)
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return after the upload finished")
	}
	if claims, _, _, _ := store.counters(); claims != 1 {
		t.Fatalf("claims = %d, want 1", claims)
	}
}

func TestIsRetryableUploadError(t *testing.T) {
	retryable := []string{"i/o timeout", "read: ECONNRESET", "dial tcp: connection refused", "socket hang up", "network is unreachable"}
	for _, message := range retryable {
		if !isRetryableUploadError(errors.New(message)) {
			t.Errorf("%q should be retryable", message)
		}
	}
	for _, message := range []string{"invalid credentials", "备份不存在", "no such bucket"} {
		if isRetryableUploadError(errors.New(message)) {
			t.Errorf("%q must not be retryable", message)
		}
	}
}
