// Port of server/src/db/restore-tasks.ts plus the two queue helpers that live in
// server/src/workers/restoreTaskWorker.ts (hasActiveRestoreTask, getQueuePosition).
//
// The restore *worker* itself is NOT ported (Wave E): this module only owns the
// task rows and their state machine. Status values are the Prisma enum
// (PENDING → PROCESSING → COMPLETED/FAILED) and live in the database, never in
// process memory, so a restart does not lose queued work.
//
// The queue is per host and shared with backup uploads: at most one PROCESSING
// restore/upload task per host, and a pending task's position counts older pending
// tasks plus (at most) one running task. getRestoreTaskQueuePosition mirrors
// workers/restoreTaskWorker.ts#getQueuePosition exactly, including the shared
// upload-task check.
//
// Time: every time.Time bound into SQL is UTC (WAVE_D_SPEC §0.2b).
package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// CreateRestoreTaskData mirrors CreateRestoreTaskData.
type CreateRestoreTaskData struct {
	InstanceID           int32
	BackupID             *int32
	HostID               int32
	UserID               int32
	TempInstanceName     string
	OriginalInstanceName string
	OriginalIncusID      string
}

const restoreTasksSelectColumns = `id, instance_id, backup_id, host_id, user_id, status,
	progress, retry_count, temp_instance_name, original_instance_name, original_incus_id,
	error, created_at, started_at, finished_at`

// restoreTasksScan reads one restore_tasks row.
func restoreTasksScan(row pgx.Row) (*RestoreTasksRecord, error) {
	var record RestoreTasksRecord
	err := row.Scan(&record.ID, &record.InstanceID, &record.BackupID, &record.HostID,
		&record.UserID, &record.Status, &record.Progress, &record.RetryCount,
		&record.TempInstanceName, &record.OriginalInstanceName, &record.OriginalIncusID,
		&record.Error, &record.CreatedAt, &record.StartedAt, &record.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// RestoreTasksRecord is the row projection used by the routes. Raw time values are
// kept so callers can render them exactly like `Date#toISOString()`.
type RestoreTasksRecord struct {
	ID                   int32
	InstanceID           int32
	BackupID             *int32
	HostID               int32
	UserID               int32
	Status               string
	Progress             *string
	RetryCount           int32
	TempInstanceName     *string
	OriginalInstanceName string
	OriginalIncusID      string
	Error                *string
	CreatedAt            time.Time
	StartedAt            *time.Time
	FinishedAt           *time.Time
}

// restoreTasksRequireDB guards against calling before db.Init.
func restoreTasksRequireDB() error {
	if DB == nil || Pool() == nil {
		return errors.New("database not initialized")
	}
	return nil
}

// CreateRestoreTask mirrors createRestoreTask: take the per-instance advisory
// lock, refuse when an active task already exists, otherwise insert PENDING.
// Returns nil, nil when an active task won the race (the route answers 409).
func CreateRestoreTask(ctx context.Context, data CreateRestoreTaskData) (*int32, error) {
	if err := restoreTasksRequireDB(); err != nil {
		return nil, err
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := TryAdvisoryTransactionLock(ctx, tx, InstanceOperationLockNamespace, data.InstanceID)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, nil
	}

	var activeID int32
	err = tx.QueryRow(ctx, `SELECT id FROM restore_tasks
		WHERE instance_id = $1 AND status IN ('PENDING', 'PROCESSING') LIMIT 1`,
		data.InstanceID).Scan(&activeID)
	if err == nil {
		return nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var taskID int32
	err = tx.QueryRow(ctx, `INSERT INTO restore_tasks
		(instance_id, backup_id, host_id, user_id, temp_instance_name,
		 original_instance_name, original_incus_id, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'PENDING', $8)
		RETURNING id`,
		data.InstanceID, data.BackupID, data.HostID, data.UserID, data.TempInstanceName,
		data.OriginalInstanceName, data.OriginalIncusID,
		time.Now().UTC().Truncate(time.Millisecond)).Scan(&taskID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &taskID, nil
}

// GetRestoreTaskByID mirrors getRestoreTaskById.
func GetRestoreTaskByID(ctx context.Context, id int32) (*RestoreTasksRecord, error) {
	if err := restoreTasksRequireDB(); err != nil {
		return nil, err
	}
	return restoreTasksScan(Pool().QueryRow(ctx,
		`SELECT `+restoreTasksSelectColumns+` FROM restore_tasks WHERE id = $1`, id))
}

// GetRestoreTasksByInstanceID mirrors getRestoreTasksByInstanceId (newest 20).
func GetRestoreTasksByInstanceID(ctx context.Context, instanceID int32) ([]RestoreTasksRecord, error) {
	if err := restoreTasksRequireDB(); err != nil {
		return nil, err
	}
	rows, err := Pool().Query(ctx, `SELECT `+restoreTasksSelectColumns+` FROM restore_tasks
		WHERE instance_id = $1 ORDER BY created_at DESC LIMIT 20`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []RestoreTasksRecord{}
	for rows.Next() {
		record, err := restoreTasksScan(rows)
		if err != nil {
			return nil, err
		}
		if record != nil {
			records = append(records, *record)
		}
	}
	return records, rows.Err()
}

// UpdateRestoreTaskStatus mirrors updateRestoreTaskStatus. `error` is always
// written (null when empty, matching `error: error ?? null`), and finished_at is
// stamped only for the two terminal statuses.
func UpdateRestoreTaskStatus(ctx context.Context, id int32, status string, errMessage *string) (*RestoreTasksRecord, error) {
	if err := restoreTasksRequireDB(); err != nil {
		return nil, err
	}

	var finishedAt *time.Time
	if status == "COMPLETED" || status == "FAILED" {
		now := time.Now().UTC().Truncate(time.Millisecond)
		finishedAt = &now
	}

	tag, err := Pool().Exec(ctx, `UPDATE restore_tasks
		SET status = $2, error = $3, finished_at = $4
		WHERE id = $1`, id, status, errMessage, finishedAt)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, errors.New(backupsUpdateMissingMessage)
	}
	return GetRestoreTaskByID(ctx, id)
}

// CancelRestoreTask mirrors cancelRestoreTask: only a PENDING row can be
// cancelled, and only one caller wins the guarded UPDATE.
func CancelRestoreTask(ctx context.Context, id int32) (*RestoreTasksRecord, error) {
	if err := restoreTasksRequireDB(); err != nil {
		return nil, err
	}

	task, err := GetRestoreTaskByID(ctx, id)
	if err != nil || task == nil || task.Status != "PENDING" {
		return nil, err
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := TryAdvisoryTransactionLock(ctx, tx, InstanceOperationLockNamespace, task.InstanceID)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, nil
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	tag, err := tx.Exec(ctx, `UPDATE restore_tasks
		SET status = 'FAILED', error = '用户取消', finished_at = $2
		WHERE id = $1 AND status = 'PENDING'`, id, now)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return GetRestoreTaskByID(ctx, id)
}

// HasActiveRestoreTask mirrors restoreTaskWorker.ts#hasActiveRestoreTask.
func HasActiveRestoreTask(ctx context.Context, instanceID int32) (*int32, error) {
	if err := restoreTasksRequireDB(); err != nil {
		return nil, err
	}
	var id int32
	err := Pool().QueryRow(ctx, `SELECT id FROM restore_tasks
		WHERE instance_id = $1 AND status IN ('PENDING', 'PROCESSING') LIMIT 1`,
		instanceID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// GetRestoreTaskQueuePosition mirrors restoreTaskWorker.ts#getQueuePosition:
// 0 unless the task is PENDING, otherwise 1 + older pending restore tasks +
// (at most) one earlier PROCESSING restore/upload task on the same host.
func GetRestoreTaskQueuePosition(ctx context.Context, taskID int32) (int32, error) {
	if err := restoreTasksRequireDB(); err != nil {
		return 0, err
	}

	var hostID int32
	var createdAt time.Time
	var status string
	err := Pool().QueryRow(ctx, `SELECT host_id, created_at, status FROM restore_tasks
		WHERE id = $1`, taskID).Scan(&hostID, &createdAt, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if status != "PENDING" {
		return 0, nil
	}

	var processingRestoreAt *time.Time
	err = Pool().QueryRow(ctx, `SELECT created_at FROM restore_tasks
		WHERE host_id = $1 AND status = 'PROCESSING' ORDER BY created_at ASC LIMIT 1`,
		hostID).Scan(&processingRestoreAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}

	var processingUploadAt *time.Time
	err = Pool().QueryRow(ctx, `SELECT created_at FROM backup_upload_tasks
		WHERE host_id = $1 AND status = 'PROCESSING' ORDER BY created_at ASC LIMIT 1`,
		hostID).Scan(&processingUploadAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}

	hasProcessingTask := (processingRestoreAt != nil && processingRestoreAt.Before(createdAt)) ||
		(processingUploadAt != nil && processingUploadAt.Before(createdAt))
	processingCount := int32(0)
	if hasProcessingTask {
		processingCount = 1
	}

	var pendingCount int32
	if err := Pool().QueryRow(ctx, `SELECT count(*)::int FROM restore_tasks
		WHERE host_id = $1 AND status = 'PENDING' AND created_at < $2`,
		hostID, createdAt).Scan(&pendingCount); err != nil {
		return 0, err
	}

	return processingCount + pendingCount + 1, nil
}

// CountPendingRestoreTasksForHost mirrors the route's inline
// `prisma.restoreTask.count({ where: { hostId, status: 'PENDING' } })` (queue
// length guard, QUEUE_MAX_LENGTH = 10).
func CountPendingRestoreTasksForHost(ctx context.Context, hostID int32) (int32, error) {
	if err := restoreTasksRequireDB(); err != nil {
		return 0, err
	}
	var count int32
	if err := Pool().QueryRow(ctx, `SELECT count(*)::int FROM restore_tasks
		WHERE host_id = $1 AND status = 'PENDING'`, hostID).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}
