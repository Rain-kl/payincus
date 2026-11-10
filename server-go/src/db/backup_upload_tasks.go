// Port of server/src/db/backup-upload-tasks.ts (the functions the routes and the
// future worker need).
//
// Upload tasks share the per-host execution slot with restore tasks: the worker
// runs one PROCESSING restore-or-upload task per host, so both queue-position
// helpers look at both tables. A user may only have one PENDING/PROCESSING upload
// task at a time; that is enforced by tryAdvisoryTransactionLock on the
// USER_BACKUP_UPLOAD namespace (4113) inside the insert transaction, not by a
// pre-read, so two concurrent upload requests cannot both start.
//
// Status values are the Prisma enum (PENDING → PROCESSING → COMPLETED/FAILED) and
// live in the database, never in process memory.
//
// Time: every time.Time bound into SQL is UTC (WAVE_D_SPEC §0.2b).
package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// BackupUploadTasksRecord is the row projection of backup_upload_tasks. The two
// storage-config fields are filled only by the *_with_details reads (the
// TypeScript `include`s that relation there).
type BackupUploadTasksRecord struct {
	ID              int32
	UserID          int32
	InstanceID      int32
	BackupID        int32
	HostID          int32
	StorageConfigID int32
	Status          string
	Progress        *string
	RetryCount      int32
	RemoteFileName  *string
	FileSize        *int64
	Error           *string
	CreatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time

	StorageConfigName *string
	StorageConfigType *string
	StorageConfigHost *string
}

const backupUploadTasksSelectColumns = `t.id, t.user_id, t.instance_id, t.backup_id, t.host_id,
	t.storage_config_id, t.status, t.progress, t.retry_count, t.remote_file_name, t.file_size,
	t.error, t.created_at, t.started_at, t.finished_at`

const backupUploadTasksDetailsSelectColumns = backupUploadTasksSelectColumns + `,
	s.name, s.type, s.host`

// backupUploadTasksScan reads a plain row.
func backupUploadTasksScan(row pgx.Row) (*BackupUploadTasksRecord, error) {
	var record BackupUploadTasksRecord
	err := row.Scan(&record.ID, &record.UserID, &record.InstanceID, &record.BackupID,
		&record.HostID, &record.StorageConfigID, &record.Status, &record.Progress,
		&record.RetryCount, &record.RemoteFileName, &record.FileSize, &record.Error,
		&record.CreatedAt, &record.StartedAt, &record.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// backupUploadTasksScanDetails reads a row joined with its storage config.
func backupUploadTasksScanDetails(row pgx.Row) (*BackupUploadTasksRecord, error) {
	var record BackupUploadTasksRecord
	err := row.Scan(&record.ID, &record.UserID, &record.InstanceID, &record.BackupID,
		&record.HostID, &record.StorageConfigID, &record.Status, &record.Progress,
		&record.RetryCount, &record.RemoteFileName, &record.FileSize, &record.Error,
		&record.CreatedAt, &record.StartedAt, &record.FinishedAt,
		&record.StorageConfigName, &record.StorageConfigType, &record.StorageConfigHost)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// backupUploadTasksRequireDB guards against calling before db.Init.
func backupUploadTasksRequireDB() error {
	if DB == nil || Pool() == nil {
		return errors.New("database not initialized")
	}
	return nil
}

// CreateBackupUploadTask mirrors createBackupUploadTask: one transaction, a
// per-user advisory lock (try-lock, so the loser gets nil instead of blocking), and
// an active-task re-check inside the lock.
func CreateBackupUploadTask(
	ctx context.Context,
	userID, instanceID, backupID, hostID, storageConfigID int32,
) (*BackupUploadTasksRecord, error) {
	if err := backupUploadTasksRequireDB(); err != nil {
		return nil, err
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := TryAdvisoryTransactionLock(ctx, tx, UserBackupUploadLockNamespace, userID)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, nil
	}

	var activeID int32
	err = tx.QueryRow(ctx, `SELECT id FROM backup_upload_tasks
		WHERE user_id = $1 AND status IN ('PENDING', 'PROCESSING') LIMIT 1`, userID).Scan(&activeID)
	if err == nil {
		return nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var taskID int32
	err = tx.QueryRow(ctx, `INSERT INTO backup_upload_tasks
		(user_id, instance_id, backup_id, host_id, storage_config_id, status, created_at)
		VALUES ($1, $2, $3, $4, $5, 'PENDING', $6)
		RETURNING id`,
		userID, instanceID, backupID, hostID, storageConfigID,
		time.Now().UTC().Truncate(time.Millisecond)).Scan(&taskID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return GetBackupUploadTaskByID(ctx, taskID)
}

// GetBackupUploadTaskByID mirrors getBackupUploadTaskById.
func GetBackupUploadTaskByID(ctx context.Context, id int32) (*BackupUploadTasksRecord, error) {
	if err := backupUploadTasksRequireDB(); err != nil {
		return nil, err
	}
	return backupUploadTasksScan(Pool().QueryRow(ctx,
		`SELECT `+backupUploadTasksSelectColumns+` FROM backup_upload_tasks t WHERE t.id = $1`, id))
}

// GetBackupUploadTaskWithDetails mirrors getBackupUploadTaskWithDetails.
func GetBackupUploadTaskWithDetails(ctx context.Context, id int32) (*BackupUploadTasksRecord, error) {
	if err := backupUploadTasksRequireDB(); err != nil {
		return nil, err
	}
	return backupUploadTasksScanDetails(Pool().QueryRow(ctx,
		`SELECT `+backupUploadTasksDetailsSelectColumns+`
		FROM backup_upload_tasks t JOIN storage_configs s ON s.id = t.storage_config_id
		WHERE t.id = $1`, id))
}

// HasActiveUploadTask mirrors hasActiveUploadTask.
func HasActiveUploadTask(ctx context.Context, userID int32) (*BackupUploadTasksRecord, error) {
	if err := backupUploadTasksRequireDB(); err != nil {
		return nil, err
	}
	return backupUploadTasksScan(Pool().QueryRow(ctx,
		`SELECT `+backupUploadTasksSelectColumns+` FROM backup_upload_tasks t
		WHERE t.user_id = $1 AND t.status IN ('PENDING', 'PROCESSING') LIMIT 1`, userID))
}

// GetActiveUploadTaskForInstance mirrors getActiveUploadTaskForInstance.
func GetActiveUploadTaskForInstance(ctx context.Context, instanceID int32) (*BackupUploadTasksRecord, error) {
	if err := backupUploadTasksRequireDB(); err != nil {
		return nil, err
	}
	return backupUploadTasksScanDetails(Pool().QueryRow(ctx,
		`SELECT `+backupUploadTasksDetailsSelectColumns+`
		FROM backup_upload_tasks t JOIN storage_configs s ON s.id = t.storage_config_id
		WHERE t.instance_id = $1 AND t.status IN ('PENDING', 'PROCESSING')
		ORDER BY t.created_at DESC LIMIT 1`, instanceID))
}

// UpdateBackupUploadTaskStatus mirrors updateBackupUploadTaskStatus. Only the
// fields the worker writes are supported (the TypeScript spreads an optional
// object of the same shape).
func UpdateBackupUploadTaskStatus(
	ctx context.Context,
	id int32,
	status string,
	remoteFileName *string,
	fileSize *int64,
	errMessage *string,
	startedAt, finishedAt *time.Time,
) (*BackupUploadTasksRecord, error) {
	if err := backupUploadTasksRequireDB(); err != nil {
		return nil, err
	}
	tag, err := Pool().Exec(ctx, `UPDATE backup_upload_tasks SET
		status = $2,
		remote_file_name = COALESCE($3, remote_file_name),
		file_size = COALESCE($4, file_size),
		error = COALESCE($5, error),
		started_at = COALESCE($6, started_at),
		finished_at = COALESCE($7, finished_at)
		WHERE id = $1`, id, status, remoteFileName, fileSize, errMessage, startedAt, finishedAt)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, errors.New(backupsUpdateMissingMessage)
	}
	return GetBackupUploadTaskByID(ctx, id)
}

// CancelBackupUploadTask mirrors cancelBackupUploadTask: only PENDING, guarded by
// the instance advisory lock and a status-conditioned UPDATE so exactly one caller
// wins.
func CancelBackupUploadTask(ctx context.Context, id int32) (*BackupUploadTasksRecord, error) {
	if err := backupUploadTasksRequireDB(); err != nil {
		return nil, err
	}

	task, err := GetBackupUploadTaskByID(ctx, id)
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
	tag, err := tx.Exec(ctx, `UPDATE backup_upload_tasks
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
	return GetBackupUploadTaskByID(ctx, id)
}

// GetUploadTaskQueuePosition mirrors getUploadTaskQueuePosition.
func GetUploadTaskQueuePosition(ctx context.Context, taskID int32) (int32, error) {
	if err := backupUploadTasksRequireDB(); err != nil {
		return 0, err
	}

	var hostID int32
	var createdAt time.Time
	var status string
	err := Pool().QueryRow(ctx, `SELECT host_id, created_at, status FROM backup_upload_tasks
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

	var processingUploadAt *time.Time
	err = Pool().QueryRow(ctx, `SELECT created_at FROM backup_upload_tasks
		WHERE host_id = $1 AND status = 'PROCESSING' ORDER BY created_at ASC LIMIT 1`,
		hostID).Scan(&processingUploadAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}

	var processingRestoreAt *time.Time
	err = Pool().QueryRow(ctx, `SELECT created_at FROM restore_tasks
		WHERE host_id = $1 AND status = 'PROCESSING' ORDER BY created_at ASC LIMIT 1`,
		hostID).Scan(&processingRestoreAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}

	hasProcessingTask := (processingUploadAt != nil && processingUploadAt.Before(createdAt)) ||
		(processingRestoreAt != nil && processingRestoreAt.Before(createdAt))
	processingCount := int32(0)
	if hasProcessingTask {
		processingCount = 1
	}

	var pendingCount int32
	if err := Pool().QueryRow(ctx, `SELECT count(*)::int FROM backup_upload_tasks
		WHERE host_id = $1 AND status = 'PENDING' AND created_at < $2`,
		hostID, createdAt).Scan(&pendingCount); err != nil {
		return 0, err
	}

	return processingCount + pendingCount + 1, nil
}

// CountPendingUploadTasksForHost mirrors the route's inline
// `prisma.backupUploadTask.count({ where: { hostId, status: 'PENDING' } })`
// (queue length guard, QUEUE_MAX_LENGTH = 10).
func CountPendingUploadTasksForHost(ctx context.Context, hostID int32) (int32, error) {
	if err := backupUploadTasksRequireDB(); err != nil {
		return 0, err
	}
	var count int32
	if err := Pool().QueryRow(ctx, `SELECT count(*)::int FROM backup_upload_tasks
		WHERE host_id = $1 AND status = 'PENDING'`, hostID).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}
