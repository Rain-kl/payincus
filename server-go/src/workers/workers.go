// Wave E worker entry point.
//
// StartWorkers is the single place the server starts background task processing.
// It is deliberately NOT wired into cmd/server/main.go yet: the integration owner
// adds the two lines (start + defer stop) after reviewing this wave.
//
// The four workers are process-local polling loops, exactly like the Node backend.
// AGENTS.md requires the private backend to run as ONE process; the in-memory
// state these loops keep (running task ids, the host-notification throttle lock,
// the per-host claim semaphore) is therefore authoritative. Do NOT replace them
// with a distributed lock to "support replicas" — that is an architecture change,
// not a port.
//
// DISABLE_WORKERS=1 makes StartWorkers a no-op. The conformance harness and unit
// tests use it to boot the app without claiming real tasks; it returns a stop
// function that is safe to call.
package workers

import (
	"context"
	"log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// workersDisabled mirrors the DISABLE_WORKERS=1 escape hatch.
func workersDisabled() bool {
	return os.Getenv("DISABLE_WORKERS") == "1"
}

// activeWorkers is exposed to tests: it counts the loops StartWorkers actually
// started, so "DISABLE_WORKERS returned immediately" can be asserted without
// guessing from goroutine counts.
var activeWorkers int32

// StartWorkers 启动全部 worker，返回的 stop 会等待在途任务退出。
//
// Startup order mirrors server/src/app.ts:
//
//  1. run the four crash-recovery sweeps (instance/restore/upload stale tasks are
//     failed after their timeout; host-notification tasks are requeued);
//  2. only then start the polling loops, so a task that was PROCESSING when the
//     previous process died is never claimed twice.
func StartWorkers(ctx context.Context) (stop func()) {
	if workersDisabled() {
		log.Printf("[workers] DISABLE_WORKERS=1: background workers disabled")
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if !startupDatabaseReady() {
		// Workers are pure database pollers; without the shared pool they would just
		// log an error every tick. Refuse loudly instead of spinning.
		log.Printf("[workers] database not initialized; background workers not started")
		return func() {}
	}

	runStartupCleanups(ctx)
	if ctx.Err() != nil {
		return func() {}
	}

	workerCtx, cancel := context.WithCancel(ctx)

	instanceWorker := newInstanceTaskWorker()
	restoreWorker := newRestoreTaskWorker()
	uploadWorker := newBackupUploadWorker()
	emailWorker := newHostNotificationEmailWorker()

	stopLoops := startWorkerLoops(workerCtx, &workerSet{
		instance: instanceWorker,
		restore:  restoreWorker,
		upload:   uploadWorker,
		email:    emailWorker,
	})
	setGlobalInstanceWorker(instanceWorker)
	atomic.StoreInt32(&activeWorkers, 4)

	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			// Every worker's run loop waits for its own in-flight executions before
			// returning (defer wg.Wait / defer w.wg.Wait), so this Wait is what makes
			// the stop contract "no new task is processed; in-flight tasks finish".
			stopLoops()
			setGlobalInstanceWorker(nil)
			atomic.StoreInt32(&activeWorkers, 0)
		})
	}
}

// workerSet groups the four loops so tests can substitute fakes while exercising
// the same start/stop contract StartWorkers gives the server.
type workerSet struct {
	instance *instanceTaskWorker
	restore  *restoreTaskWorker
	upload   *backupUploadWorker
	email    *hostNotificationEmailWorker
}

// startWorkerLoops starts the four loops and returns the shutdown closure, which
// blocks until every loop and every in-flight task has returned. Cancelling the
// context is the caller's job (StartWorkers owns it); the closure is idempotent so
// a SIGTERM handler and a deferred test cleanup can both call it.
func startWorkerLoops(ctx context.Context, set *workerSet) (stop func()) {
	var wg sync.WaitGroup
	set.instance.start(ctx, &wg)
	set.restore.start(ctx, &wg)
	set.upload.start(ctx, &wg)
	set.email.start(ctx, &wg)

	var once sync.Once
	return func() {
		once.Do(func() {
			wg.Wait()
			set.instance.stop()
			set.restore.stop()
			set.upload.stop()
			set.email.stop()
			log.Printf("[workers] all background workers stopped")
		})
	}
}

// startupDatabaseReady reports whether db.Init has run. The workers never open a
// connection themselves: like the Node workers they borrow the shared pool.
func startupDatabaseReady() bool { return db.Pool() != nil }

// dbCleanupStaleHostNotificationEmails is the queue's startup sweep (requeues a
// task left PROCESSING by a crash instead of failing it).
func dbCleanupStaleHostNotificationEmails(ctx context.Context) (int64, error) {
	return db.CleanupStaleHostNotificationEmailTasks(ctx, db.HostNotificationEmailProcessingTimeoutMs)
}

// runStartupCleanups mirrors the four `cleanupStale*` awaits in app.ts. A failure
// is logged and swallowed: a transient database error must not prevent the HTTP
// server from starting, and the runtime timeout sweeps will retry the same rows.
func runStartupCleanups(ctx context.Context) {
	if dbReady := startupDatabaseReady(); !dbReady {
		log.Printf("[workers] database not initialized; skipping startup cleanups")
		return
	}

	if notices, err := newPGStore().CleanupStaleInstanceTasks(ctx); err != nil {
		log.Printf("[InstanceTaskWorker] 清理僵尸任务失败: %v", err)
	} else if len(notices) > 0 {
		log.Printf("[InstanceTaskWorker] 清理了 %d 个僵尸任务", len(notices))
		notifier := newPGNotifier()
		for _, notice := range notices {
			notifySafely("startup instance failure notification", func() {
				notifier.InstanceTaskFailed(ctx, notice, "系统启动清理超时任务")
			})
		}
	}

	if count, err := newPGStore().CleanupStaleRestoreTasks(ctx); err != nil {
		log.Printf("[RestoreWorker] 清理僵尸任务失败: %v", err)
	} else if count > 0 {
		log.Printf("[RestoreWorker] 清理了 %d 个僵尸任务", count)
	}

	if count, err := newPGStore().CleanupStaleUploadTasks(ctx); err != nil {
		log.Printf("[UploadWorker] 清理僵尸任务失败: %v", err)
	} else if count > 0 {
		log.Printf("[UploadWorker] 清理了 %d 个僵尸任务", count)
	}

	if count, err := dbCleanupStaleHostNotificationEmails(ctx); err != nil {
		log.Printf("[HostNotifyEmailWorker] 清理僵尸任务失败: %v", err)
	} else if count > 0 {
		log.Printf("[HostNotifyEmailWorker] 重新入队了 %d 个宿主机通知邮件任务", count)
	}
}
