// Port of server/src/workers/hostNotificationEmailWorker.ts.
//
// The queue itself (throttle, retry schedule, advisory lock) lives in
// db/host_notification_email_tasks.go; this file is the polling loop.
//
// Two behaviours are copied on purpose:
//
//   - POLL_INTERVAL is 5 s, but the queue only releases one task per
//     HOST_NOTIFICATION_EMAIL_INTERVAL_MS (60 s). The poll is just how quickly a
//     due task is noticed.
//   - The timeout sweep requeues (not fails) a task stuck in PROCESSING for 10
//     minutes. An announcement that never went out is still valid later.
package workers

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	hostNotificationEmailPollInterval      = 5 * time.Second
	hostNotificationEmailTimeoutInterval   = 60 * time.Second
	hostNotificationEmailProcessingTimeout = db.HostNotificationEmailProcessingTimeoutMs
)

// hostNotificationEmailStore is the queue seam.
type hostNotificationEmailStore interface {
	ClaimNext(ctx context.Context) (*models.HostNotificationEmailTask, error)
	MarkSent(ctx context.Context, taskID int32) (bool, error)
	RetryOrFail(ctx context.Context, taskID, currentRetryCount int32, message string) (bool, error)
	CleanupStale(ctx context.Context, timeoutMs int64) (int64, error)
	CleanupTimeout(ctx context.Context, timeoutMs int64) (int64, error)
}

// hostNotificationEmailSender is the mail seam. Tests replace it; production uses
// lib.SendHostAnnouncementEmail, which is itself a no-op-safe wrapper around the
// SMTP settings (never called by tests).
type hostNotificationEmailSender interface {
	Send(ctx context.Context, email string, data lib.HostAnnouncementEmailData) lib.EmailSendResult
}

type pgHostNotificationEmailStore struct{}

func (pgHostNotificationEmailStore) ClaimNext(ctx context.Context) (*models.HostNotificationEmailTask, error) {
	return db.ClaimNextHostNotificationEmailTask(ctx)
}

func (pgHostNotificationEmailStore) MarkSent(ctx context.Context, taskID int32) (bool, error) {
	return db.MarkHostNotificationEmailTaskSent(ctx, taskID)
}

func (pgHostNotificationEmailStore) RetryOrFail(ctx context.Context, taskID, currentRetryCount int32, message string) (bool, error) {
	return db.RetryOrFailHostNotificationEmailTask(ctx, taskID, currentRetryCount, message)
}

func (pgHostNotificationEmailStore) CleanupStale(ctx context.Context, timeoutMs int64) (int64, error) {
	return db.CleanupStaleHostNotificationEmailTasks(ctx, timeoutMs)
}

func (pgHostNotificationEmailStore) CleanupTimeout(ctx context.Context, timeoutMs int64) (int64, error) {
	return db.CleanupTimeoutHostNotificationEmailTasks(ctx, timeoutMs)
}

type pgHostNotificationEmailSender struct{}

func (pgHostNotificationEmailSender) Send(ctx context.Context, email string, data lib.HostAnnouncementEmailData) lib.EmailSendResult {
	return lib.SendHostAnnouncementEmail(ctx, email, data)
}

// hostNotificationEmailWorker mirrors the module-level interval handles and the
// `isProcessing` guard of the TypeScript. All state is per instance so tests can
// run workers in parallel without cross-talk.
type hostNotificationEmailWorker struct {
	store    hostNotificationEmailStore
	sender   hostNotificationEmailSender
	backoff  *workerDBBackoff
	poll     time.Duration
	timeout  time.Duration
	now      func() time.Time
	inFlight atomic.Bool
	mu       sync.Mutex
	running  bool
}

func newHostNotificationEmailWorker() *hostNotificationEmailWorker {
	return &hostNotificationEmailWorker{
		store:   pgHostNotificationEmailStore{},
		sender:  pgHostNotificationEmailSender{},
		backoff: newWorkerDBBackoff("HostNotifyEmailWorker"),
		poll:    hostNotificationEmailPollInterval,
		timeout: hostNotificationEmailTimeoutInterval,
		now:     time.Now,
	}
}

func (w *hostNotificationEmailWorker) start(ctx context.Context, wg *sync.WaitGroup) {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		log.Printf("[HostNotifyEmailWorker] Worker already running")
		return
	}
	w.running = true
	w.mu.Unlock()

	log.Printf("[HostNotifyEmailWorker] Starting worker")

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.run(ctx)
	}()
}

func (w *hostNotificationEmailWorker) stop() {
	w.mu.Lock()
	w.running = false
	w.mu.Unlock()
}

func (w *hostNotificationEmailWorker) run(ctx context.Context) {
	pollTicker := time.NewTicker(w.poll)
	defer pollTicker.Stop()
	timeoutTicker := time.NewTicker(w.timeout)
	defer timeoutTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[HostNotifyEmailWorker] Worker stopped")
			return
		case <-pollTicker.C:
			w.processQueue(ctx)
		case <-timeoutTicker.C:
			if w.backoff.shouldSkip() {
				continue
			}
			count, err := w.store.CleanupTimeout(ctx, hostNotificationEmailProcessingTimeout)
			if err != nil {
				log.Printf("[HostNotifyEmailWorker] Timeout cleanup failed: %v", err)
				w.backoff.arm(err)
				continue
			}
			if count > 0 {
				log.Printf("[HostNotifyEmailWorker] Requeued %d timed-out task(s)", count)
			}
		}
	}
}

// processQueue mirrors processQueue. The isProcessing guard keeps a slow SMTP
// handshake from stacking up: at most one send is in flight per process, which is
// also what the queue's throttle assumes.
func (w *hostNotificationEmailWorker) processQueue(ctx context.Context) {
	if w.backoff.shouldSkip() {
		return
	}
	if !w.inFlight.CompareAndSwap(false, true) {
		return
	}
	defer w.inFlight.Store(false)

	var current *models.HostNotificationEmailTask
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("[HostNotifyEmailWorker] Queue processing error: %v", recovered)
			w.backoff.arm(errPanic(recovered))
			if current != nil {
				if _, err := w.store.RetryOrFail(ctx, current.ID, current.RetryCount, errPanic(recovered).Error()); err != nil {
					log.Printf("[HostNotifyEmailWorker] Failed to recover task %d: %v", current.ID, err)
				}
			}
		}
	}()

	task, err := w.store.ClaimNext(ctx)
	if err != nil {
		log.Printf("[HostNotifyEmailWorker] Queue processing error: %v", err)
		w.backoff.arm(err)
		return
	}
	if task == nil {
		return
	}
	current = task

	log.Printf("[HostNotifyEmailWorker] Sending queued email task %d to %s", task.ID, task.Email)

	result := w.sender.Send(ctx, task.Email, lib.HostAnnouncementEmailData{
		Username: task.Username,
		HostName: task.HostName,
		Title:    task.Title,
		Content:  task.Content,
	})

	if result.Success {
		if _, err := w.store.MarkSent(ctx, task.ID); err != nil {
			log.Printf("[HostNotifyEmailWorker] Failed to mark task %d sent: %v", task.ID, err)
			w.backoff.arm(err)
			return
		}
		log.Printf("[HostNotifyEmailWorker] Task %d sent successfully", task.ID)
		return
	}

	errorMessage := result.Error
	if errorMessage == "" {
		errorMessage = "Unknown email error"
	}
	if _, err := w.store.RetryOrFail(ctx, task.ID, task.RetryCount, errorMessage); err != nil {
		log.Printf("[HostNotifyEmailWorker] Failed to record failure for task %d: %v", task.ID, err)
		w.backoff.arm(err)
		return
	}
	log.Printf("[HostNotifyEmailWorker] Task %d failed: %s", task.ID, errorMessage)
}

// errPanic converts a recovered panic into an error so the backoff classifier and
// the retry reason get a message. Panics are programming errors, so the backoff
// classifier will not treat them as transient.
func errPanic(recovered any) error {
	if err, ok := recovered.(error); ok {
		return err
	}
	return &panicError{value: recovered}
}

type panicError struct{ value any }

func (e *panicError) Error() string { return "panic: " + errString(e.value) }

func errString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return "unknown"
}
