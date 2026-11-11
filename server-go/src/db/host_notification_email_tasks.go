// Port of server/src/db/host-notification-email-tasks.ts.
//
// The host-notification email queue is a process-wide FIFO with a deliberate
// global throttle: claimNextHostNotificationEmailTask refuses to hand out a task
// while another task is PROCESSING *or* another task started less than
// HOST_NOTIFICATION_EMAIL_INTERVAL_MS ago. That is what turns a burst of "host
// announcement" fan-out into one email per minute instead of a hundred at once.
// The check and the claim run under one transaction-scoped advisory lock
// (HOST_NOTIFICATION_EMAIL_LOCK_NAMESPACE, key 1) so two workers cannot both pass
// the throttle check; without the lock the "recent task" SELECT and the UPDATE
// would interleave.
//
// Time: every time.Time bound into SQL is UTC (WAVE_D_SPEC §0.2b), truncated to
// milliseconds so a Go-written row compares byte-identically with a Node-written
// one in a `timestamp(3)` column.
package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// HostNotificationEmailIntervalMs mirrors HOST_NOTIFICATION_EMAIL_INTERVAL_MS.
	HostNotificationEmailIntervalMs = 60 * 1000
	// HostNotificationEmailRetryDelayMs mirrors
	// HOST_NOTIFICATION_EMAIL_RETRY_DELAY_MS (5 minutes).
	HostNotificationEmailRetryDelayMs = 5 * 60 * 1000
	// HostNotificationEmailMaxRetries mirrors HOST_NOTIFICATION_EMAIL_MAX_RETRIES.
	HostNotificationEmailMaxRetries = 3
	// HostNotificationEmailProcessingTimeoutMs mirrors
	// HOST_NOTIFICATION_EMAIL_PROCESSING_TIMEOUT_MS (10 minutes).
	HostNotificationEmailProcessingTimeoutMs = 10 * 60 * 1000
)

const hostNotificationEmailTaskSelectColumns = `id, user_id, host_id, email, username,
	host_name, title, content, status, retry_count, last_error, scheduled_for,
	created_at, started_at, finished_at`

func hostNotificationEmailTasksRequireDB() error {
	if DB == nil || Pool() == nil {
		return errors.New("host notification email tasks: database not initialized")
	}
	return nil
}

func hostNotificationEmailNow() time.Time {
	return time.Now().UTC().Truncate(time.Millisecond)
}

func scanHostNotificationEmailTask(scanner interface {
	Scan(dest ...any) error
}) (*models.HostNotificationEmailTask, error) {
	var task models.HostNotificationEmailTask
	err := scanner.Scan(&task.ID, &task.UserID, &task.HostID, &task.Email, &task.Username,
		&task.HostName, &task.Title, &task.Content, &task.Status, &task.RetryCount,
		&task.LastError, &task.ScheduledFor, &task.CreatedAt, &task.StartedAt, &task.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// CleanupStaleHostNotificationEmailTasks mirrors cleanupStaleHostNotificationEmailTasks:
// a task stuck in PROCESSING (never started, or started longer than timeoutMs
// ago) goes back to PENDING and becomes immediately eligible. Unlike the other
// queues this one requeues instead of failing: an announcement is still worth
// sending after a crash.
func CleanupStaleHostNotificationEmailTasks(ctx context.Context, timeoutMs int64) (int64, error) {
	if err := hostNotificationEmailTasksRequireDB(); err != nil {
		return 0, err
	}
	if timeoutMs <= 0 {
		timeoutMs = HostNotificationEmailProcessingTimeoutMs
	}
	now := hostNotificationEmailNow()
	threshold := now.Add(-time.Duration(timeoutMs) * time.Millisecond)

	tag, err := Pool().Exec(ctx, `UPDATE host_notification_email_tasks
		SET status = 'PENDING', scheduled_for = $1, last_error = '邮件任务处理超时，已重新入队',
			started_at = NULL, finished_at = NULL
		WHERE status = 'PROCESSING' AND (started_at IS NULL OR started_at < $2)`,
		now, threshold)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CleanupTimeoutHostNotificationEmailTasks mirrors
// cleanupTimeoutHostNotificationEmailTasks: the runtime variant, which only looks
// at rows that actually started (a PROCESSING row with started_at NULL is left to
// the startup cleanup so a just-claimed task is not requeued mid-send).
func CleanupTimeoutHostNotificationEmailTasks(ctx context.Context, timeoutMs int64) (int64, error) {
	if err := hostNotificationEmailTasksRequireDB(); err != nil {
		return 0, err
	}
	if timeoutMs <= 0 {
		timeoutMs = HostNotificationEmailProcessingTimeoutMs
	}
	now := hostNotificationEmailNow()
	threshold := now.Add(-time.Duration(timeoutMs) * time.Millisecond)

	tag, err := Pool().Exec(ctx, `UPDATE host_notification_email_tasks
		SET status = 'PENDING', scheduled_for = $1, last_error = '邮件任务处理超时，已重新入队',
			started_at = NULL, finished_at = NULL
		WHERE status = 'PROCESSING' AND started_at < $2`,
		now, threshold)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ClaimNextHostNotificationEmailTask mirrors
// claimNextHostNotificationEmailTask. It returns nil, nil when the throttle is
// active or the queue is empty — both are normal, not errors.
//
// The advisory lock is NON-blocking (tryAdvisoryTransactionLock): a concurrent
// claim simply loses and returns nil, which is exactly the TypeScript behaviour.
func ClaimNextHostNotificationEmailTask(ctx context.Context) (*models.HostNotificationEmailTask, error) {
	if err := hostNotificationEmailTasksRequireDB(); err != nil {
		return nil, err
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := TryAdvisoryTransactionLock(ctx, tx,
		HostNotificationEmailLockNamespace, HostNotificationEmailLockKey)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, nil
	}

	now := hostNotificationEmailNow()
	recentThreshold := now.Add(-time.Duration(HostNotificationEmailIntervalMs) * time.Millisecond)

	var throttledID int32
	err = tx.QueryRow(ctx, `SELECT id FROM host_notification_email_tasks
		WHERE status = 'PROCESSING' OR started_at >= $1
		LIMIT 1`, recentThreshold).Scan(&throttledID)
	if err == nil {
		return nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	task, err := scanHostNotificationEmailTask(tx.QueryRow(ctx, `SELECT `+
		hostNotificationEmailTaskSelectColumns+` FROM host_notification_email_tasks
		WHERE status = 'PENDING' AND scheduled_for <= $1
		ORDER BY scheduled_for ASC, created_at ASC LIMIT 1`, now))
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, nil
	}

	claimed, err := scanHostNotificationEmailTask(tx.QueryRow(ctx, `UPDATE host_notification_email_tasks
		SET status = 'PROCESSING', started_at = $2, finished_at = NULL, last_error = NULL
		WHERE id = $1 AND status = 'PENDING'
		RETURNING `+hostNotificationEmailTaskSelectColumns, task.ID, now))
	if err != nil {
		return nil, err
	}
	if claimed == nil {
		// Lost the row between the SELECT and the UPDATE: treat as no work.
		return nil, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return claimed, nil
}

// MarkHostNotificationEmailTaskSent mirrors markHostNotificationEmailTaskSent.
// The `status = 'PROCESSING'` guard means a task requeued by the timeout cleanup
// is never overwritten as SENT by a worker that finished late.
func MarkHostNotificationEmailTaskSent(ctx context.Context, taskID int32) (bool, error) {
	if err := hostNotificationEmailTasksRequireDB(); err != nil {
		return false, err
	}
	tag, err := Pool().Exec(ctx, `UPDATE host_notification_email_tasks
		SET status = 'SENT', last_error = NULL, finished_at = $2
		WHERE id = $1 AND status = 'PROCESSING'`, taskID, hostNotificationEmailNow())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RetryOrFailHostNotificationEmailTask mirrors
// retryOrFailHostNotificationEmailTask. Both branches are guarded by
// `retry_count = currentRetryCount`, so a duplicate delivery cannot bump the
// counter twice or resurrect a task that already reached a terminal state.
func RetryOrFailHostNotificationEmailTask(ctx context.Context, taskID int32, currentRetryCount int32, errorMessage string) (bool, error) {
	if err := hostNotificationEmailTasksRequireDB(); err != nil {
		return false, err
	}
	now := hostNotificationEmailNow()

	if currentRetryCount >= HostNotificationEmailMaxRetries {
		tag, err := Pool().Exec(ctx, `UPDATE host_notification_email_tasks
			SET status = 'FAILED', last_error = $3, finished_at = $2
			WHERE id = $1 AND status = 'PROCESSING' AND retry_count = $4`,
			taskID, now, errorMessage, currentRetryCount)
		if err != nil {
			return false, err
		}
		return tag.RowsAffected() > 0, nil
	}

	scheduledFor := now.Add(time.Duration(HostNotificationEmailRetryDelayMs) * time.Millisecond)
	tag, err := Pool().Exec(ctx, `UPDATE host_notification_email_tasks
		SET status = 'PENDING', retry_count = retry_count + 1, last_error = $3,
			scheduled_for = $2, finished_at = NULL
		WHERE id = $1 AND status = 'PROCESSING' AND retry_count = $4`,
		taskID, scheduledFor, errorMessage, currentRetryCount)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// GetHostNotificationEmailTaskByID is used by tests and by the worker after a
// claim to re-read the row; the TypeScript has no export for this, so it is kept
// unexported-prefix-free but harmless.
func GetHostNotificationEmailTaskByID(ctx context.Context, id int32) (*models.HostNotificationEmailTask, error) {
	if err := hostNotificationEmailTasksRequireDB(); err != nil {
		return nil, err
	}
	return scanHostNotificationEmailTask(Pool().QueryRow(ctx, `SELECT `+
		hostNotificationEmailTaskSelectColumns+` FROM host_notification_email_tasks WHERE id = $1`, id))
}
