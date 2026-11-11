package workers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

func newTestEmailWorker(store hostNotificationEmailStore, sender hostNotificationEmailSender, poll, timeout time.Duration) *hostNotificationEmailWorker {
	return &hostNotificationEmailWorker{
		store:   store,
		sender:  sender,
		backoff: newWorkerDBBackoff("test"),
		poll:    poll,
		timeout: timeout,
		now:     time.Now,
	}
}

func pendingEmailTask() *models.HostNotificationEmailTask {
	return &models.HostNotificationEmailTask{
		ID: 1, UserID: 3, HostID: 5, Email: "user@example.com", Username: "u",
		HostName: "hk-1", Title: "维护", Content: "今晚维护", Status: "PENDING", RetryCount: 0,
	}
}

// TestHostNotificationEmailWorkerClaimIsExclusive pins the in-process throttle:
// two concurrent processQueue calls claim at most one task per interval.
func TestHostNotificationEmailWorkerClaimIsExclusive(t *testing.T) {
	store := &fakeEmailStore{task: pendingEmailTask()}
	sender := &fakeEmailSender{result: lib.EmailSendResult{Success: true}}
	worker := newTestEmailWorker(store, sender, time.Hour, time.Hour)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		worker.processQueue(context.Background())
	}()
	go func() {
		defer wg.Done()
		worker.processQueue(context.Background())
	}()
	wg.Wait()

	claims, sent, _, _, _ := store.counters()
	if claims != 1 {
		t.Fatalf("claims = %d, want 1", claims)
	}
	if sent != 1 {
		t.Fatalf("sent = %d, want 1", sent)
	}
	if got := sender.callCount(); got != 1 {
		t.Fatalf("sender calls = %d, want 1", got)
	}
}

func TestHostNotificationEmailWorkerSuccessMarksSent(t *testing.T) {
	store := &fakeEmailStore{task: pendingEmailTask()}
	sender := &fakeEmailSender{result: lib.EmailSendResult{Success: true}}
	worker := newTestEmailWorker(store, sender, time.Hour, time.Hour)

	worker.processQueue(context.Background())

	_, sent, retried, _, _ := store.counters()
	if sent != 1 || retried != 0 {
		t.Fatalf("sent=%d retried=%d, want 1/0", sent, retried)
	}
}

func TestHostNotificationEmailWorkerFailureRetries(t *testing.T) {
	store := &fakeEmailStore{task: pendingEmailTask()}
	sender := &fakeEmailSender{result: lib.EmailSendResult{Success: false, Error: "smtp down"}}
	worker := newTestEmailWorker(store, sender, time.Hour, time.Hour)

	worker.processQueue(context.Background())

	_, sent, retried, reason, _ := store.counters()
	if sent != 0 || retried != 1 {
		t.Fatalf("sent=%d retried=%d, want 0/1", sent, retried)
	}
	if reason != "smtp down" {
		t.Fatalf("retry reason = %q", reason)
	}
	if store.task.Status != "PENDING" || store.task.RetryCount != 1 {
		t.Fatalf("task = %s retry=%d, want PENDING/1", store.task.Status, store.task.RetryCount)
	}
}

func TestHostNotificationEmailWorkerFailureWithoutMessage(t *testing.T) {
	store := &fakeEmailStore{task: pendingEmailTask()}
	sender := &fakeEmailSender{result: lib.EmailSendResult{Success: false}}
	worker := newTestEmailWorker(store, sender, time.Hour, time.Hour)

	worker.processQueue(context.Background())

	_, _, retried, reason, _ := store.counters()
	if retried != 1 || reason != "Unknown email error" {
		t.Fatalf("retried=%d reason=%q, want 1/Unknown email error", retried, reason)
	}
}

func TestHostNotificationEmailWorkerStopStopsPolling(t *testing.T) {
	store := &fakeEmailStore{task: pendingEmailTask()}
	sender := &fakeEmailSender{result: lib.EmailSendResult{Success: true}}
	worker := newTestEmailWorker(store, sender, 5*time.Millisecond, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	worker.start(ctx, &wg)
	waitFor(t, 2*time.Second, func() bool {
		_, sent, _, _, _ := store.counters()
		return sent == 1
	})
	cancel()
	wg.Wait()
	claimsAfterStop, _, _, _, _ := store.counters()
	time.Sleep(30 * time.Millisecond)
	claimsLater, _, _, _, _ := store.counters()
	if claimsAfterStop != claimsLater {
		t.Fatalf("claims changed after stop: %d -> %d", claimsAfterStop, claimsLater)
	}
}

func TestHostNotificationEmailWorkerTimeoutCleanupRequeues(t *testing.T) {
	store := &fakeEmailStore{task: pendingEmailTask(), staleCount: 2}
	worker := newTestEmailWorker(store, &fakeEmailSender{}, time.Hour, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	worker.start(ctx, &wg)
	waitFor(t, 2*time.Second, func() bool {
		_, _, _, _, timeoutCalls := store.counters()
		return timeoutCalls >= 1
	})
	cancel()
	wg.Wait()
}

func TestHostNotificationEmailWorkerDisabledClaimStillCallsSenderOnce(t *testing.T) {
	// An empty queue must not call the SMTP layer at all.
	store := &fakeEmailStore{}
	sender := &fakeEmailSender{result: lib.EmailSendResult{Success: true}}
	worker := newTestEmailWorker(store, sender, time.Hour, time.Hour)

	worker.processQueue(context.Background())

	if got := sender.callCount(); got != 0 {
		t.Fatalf("sender calls = %d, want 0 for an empty queue", got)
	}
}
