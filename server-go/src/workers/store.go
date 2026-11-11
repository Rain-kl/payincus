// Persistence layer for the Wave E workers: task queues, claims and terminal
// transitions.
//
// Claim discipline (mirrors the TypeScript workers):
//
//   - The per-host claim takes the transaction-scoped advisory lock
//     HOST_TASK_CLAIM_LOCK_NAMESPACE (4101) keyed on the host id. It is
//     NON-blocking (pg_try_advisory_xact_lock): a losing worker reports "no work"
//     for this tick instead of queuing behind another claim.
//   - The per-instance claim also takes INSTANCE_OPERATION_LOCK_NAMESPACE (4106)
//     keyed on the instance id, so an instance with a restore/upload/create in
//     flight cannot be mutated by a second task.
//   - The row transition itself is a conditional UPDATE
//     (`WHERE id = $1 AND status = 'PENDING'`). RowsAffected == 0 means someone
//     else won the race (or cancelled the task); the worker must then do NOTHING,
//     never execute the task. This is what makes a crash-and-restart safe: a task
//     that was already claimed has status PROCESSING and cannot be claimed again.
//   - Every terminal transition is guarded by `status = 'PROCESSING'`, matching the
//     TypeScript's `updateMany({ where: { id, status: 'PROCESSING' } })`. A late
//     worker whose task was already failed by the timeout sweep cannot resurrect or
//     overwrite it.
//
// Time: all time.Time values bound into SQL are UTC and truncated to milliseconds
// (WAVE_D_SPEC §0.2b), so a Go-written row sorts and compares exactly like one
// written by the Node process into a timestamp(3) column.
package workers

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

func workerNow() time.Time {
	return time.Now().UTC().Truncate(time.Millisecond)
}

// heavyInstanceTaskTypes mirrors HEAVY_TASK_TYPES (rebuild/clone/recreate/
// change_host/create). Kept as []string because it is compared with
// `task_type::text = ANY($n)` — the column is a PostgreSQL enum, and comparing an
// enum to a text[] would need an explicit cast anyway.
var heavyInstanceTaskTypes = []string{"rebuild", "clone", "recreate", "change_host", "create"}

func isHeavyInstanceTaskType(taskType string) bool {
	for _, candidate := range heavyInstanceTaskTypes {
		if candidate == taskType {
			return true
		}
	}
	return false
}

// instanceTaskBrief mirrors the projection processQueue reads: `{ id, hostId,
// taskType }`.
type instanceTaskBrief struct {
	ID       int32
	HostID   int32
	TaskType string
}

// instanceTaskFailureNotice mirrors InstanceTaskFailureNotice.
type instanceTaskFailureNotice struct {
	ID         int32
	UserID     int32
	InstanceID int32
	HostID     int32
	TaskType   string
}

// instanceTaskRecord is the worker's view of one instance_tasks row. Timestamps
// are omitted: the worker never reads them (only MySQL-free conditional updates
// use them), which keeps the projection small.
type instanceTaskRecord struct {
	ID                   int32
	InstanceID           int32
	HostID               int32
	UserID               int32
	TaskType             string
	Status               string
	Progress             *string
	RetryCount           int32
	ImageAlias           *string
	SSHKeyID             *int32
	CustomInitCommandIDs *string
	TargetName           *string
	TargetHostID         *int32
	SnapshotName         *string
	NewInstanceID        *int32
	Payload              []byte
	Error                *string
}

const instanceTaskWorkerColumns = `id, instance_id, host_id, user_id, task_type::text,
	status::text, progress, retry_count, image_alias, ssh_key_id,
	custom_init_command_ids, target_name, target_host_id, snapshot_name,
	new_instance_id, payload, error`

func scanInstanceTaskRecord(scanner interface {
	Scan(dest ...any) error
}) (*instanceTaskRecord, error) {
	var task instanceTaskRecord
	err := scanner.Scan(&task.ID, &task.InstanceID, &task.HostID, &task.UserID,
		&task.TaskType, &task.Status, &task.Progress, &task.RetryCount, &task.ImageAlias,
		&task.SSHKeyID, &task.CustomInitCommandIDs, &task.TargetName, &task.TargetHostID,
		&task.SnapshotName, &task.NewInstanceID, &task.Payload, &task.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// restoreTaskRecord is the worker's view of one restore_tasks row.
type restoreTaskRecord struct {
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
}

const restoreTaskWorkerColumns = `id, instance_id, backup_id, host_id, user_id,
	status::text, progress, retry_count, temp_instance_name, original_instance_name,
	original_incus_id, error`

func scanRestoreTaskRecord(scanner interface {
	Scan(dest ...any) error
}) (*restoreTaskRecord, error) {
	var task restoreTaskRecord
	err := scanner.Scan(&task.ID, &task.InstanceID, &task.BackupID, &task.HostID,
		&task.UserID, &task.Status, &task.Progress, &task.RetryCount,
		&task.TempInstanceName, &task.OriginalInstanceName, &task.OriginalIncusID,
		&task.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// uploadTaskRecord is the worker's view of one backup_upload_tasks row.
type uploadTaskRecord struct {
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
}

const uploadTaskWorkerColumns = `id, user_id, instance_id, backup_id, host_id,
	storage_config_id, status::text, progress, retry_count, remote_file_name,
	file_size, error`

func scanUploadTaskRecord(scanner interface {
	Scan(dest ...any) error
}) (*uploadTaskRecord, error) {
	var task uploadTaskRecord
	err := scanner.Scan(&task.ID, &task.UserID, &task.InstanceID, &task.BackupID,
		&task.HostID, &task.StorageConfigID, &task.Status, &task.Progress,
		&task.RetryCount, &task.RemoteFileName, &task.FileSize, &task.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// pgStore is the production implementation of the four queue interfaces.
type pgStore struct{}

func newPGStore() *pgStore { return &pgStore{} }

func workerRequireDB() error {
	if db.Pool() == nil {
		return errors.New("workers: database not initialized")
	}
	return nil
}

// ==================== instance tasks ====================

func (s *pgStore) ListPendingInstanceTasks(ctx context.Context) ([]instanceTaskBrief, error) {
	if err := workerRequireDB(); err != nil {
		return nil, err
	}
	rows, err := db.Pool().Query(ctx, `SELECT id, host_id, task_type::text
		FROM instance_tasks WHERE status = 'PENDING' ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	briefs := []instanceTaskBrief{}
	for rows.Next() {
		var brief instanceTaskBrief
		if err := rows.Scan(&brief.ID, &brief.HostID, &brief.TaskType); err != nil {
			return nil, err
		}
		briefs = append(briefs, brief)
	}
	return briefs, rows.Err()
}

// EarliestActiveInstanceTask mirrors the "instance already has an earlier task"
// guard inside executeTask: PENDING/PROCESSING tasks ordered by createdAt then id.
func (s *pgStore) EarliestActiveInstanceTask(ctx context.Context, instanceID int32) (*instanceTaskBrief, error) {
	if err := workerRequireDB(); err != nil {
		return nil, err
	}
	var brief instanceTaskBrief
	err := db.Pool().QueryRow(ctx, `SELECT id, host_id, task_type::text FROM instance_tasks
		WHERE instance_id = $1 AND status IN ('PENDING', 'PROCESSING')
		ORDER BY created_at ASC, id ASC LIMIT 1`, instanceID).
		Scan(&brief.ID, &brief.HostID, &brief.TaskType)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &brief, nil
}

func (s *pgStore) HostHasProcessingHeavyTask(ctx context.Context, hostID int32) (bool, error) {
	if err := workerRequireDB(); err != nil {
		return false, err
	}
	var exists bool
	err := db.Pool().QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM instance_tasks
		WHERE host_id = $1 AND status = 'PROCESSING' AND task_type::text = ANY($2))`,
		hostID, heavyInstanceTaskTypes).Scan(&exists)
	return exists, err
}

func (s *pgStore) HostHasProcessingInstanceTask(ctx context.Context, hostID int32) (bool, error) {
	if err := workerRequireDB(); err != nil {
		return false, err
	}
	var exists bool
	err := db.Pool().QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM instance_tasks WHERE host_id = $1 AND status = 'PROCESSING')`,
		hostID).Scan(&exists)
	return exists, err
}

// hostHasSharedProcessingTask covers the restore/upload "shared lock" checks the
// instance worker performs: one heavy IO job (restore or upload) per host blocks
// instance tasks on that host.
func (s *pgStore) hostHasSharedProcessingTask(ctx context.Context, tx pgx.Tx, hostID int32) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM restore_tasks WHERE host_id = $1 AND status = 'PROCESSING')
		OR EXISTS(SELECT 1 FROM backup_upload_tasks WHERE host_id = $1 AND status = 'PROCESSING')`,
		hostID).Scan(&exists)
	return exists, err
}

func (s *pgStore) claimInstanceTask(
	ctx context.Context,
	hostID int32,
	specificTaskID *int32,
) (*instanceTaskRecord, error) {
	if err := workerRequireDB(); err != nil {
		return nil, err
	}

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := db.TryAdvisoryTransactionLock(ctx, tx, db.HostTaskClaimLockNamespace, hostID)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, nil
	}

	// 1. No heavy task may be running on this host.
	if specificTaskID != nil {
		var heavy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM instance_tasks
			WHERE host_id = $1 AND status = 'PROCESSING' AND task_type::text = ANY($2))`,
			hostID, heavyInstanceTaskTypes).Scan(&heavy); err != nil {
			return nil, err
		}
		if heavy {
			return nil, nil
		}
	} else {
		var running bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
			SELECT 1 FROM instance_tasks WHERE host_id = $1 AND status = 'PROCESSING')`,
			hostID).Scan(&running); err != nil {
			return nil, err
		}
		if running {
			return nil, nil
		}
	}

	// 2. Shared lock: no restore/upload may be running on this host.
	sharedBusy, err := s.hostHasSharedProcessingTask(ctx, tx, hostID)
	if err != nil {
		return nil, err
	}
	if sharedBusy {
		return nil, nil
	}

	// 3. Pick the task. The light variant already knows which id the queue scan
	//    selected; the heavy variant takes the oldest PENDING row.
	var task *instanceTaskRecord
	if specificTaskID != nil {
		task, err = scanInstanceTaskRecord(tx.QueryRow(ctx, `SELECT `+instanceTaskWorkerColumns+`
			FROM instance_tasks WHERE id = $1 AND host_id = $2 AND status = 'PENDING'`,
			*specificTaskID, hostID))
		if err != nil {
			return nil, err
		}
	} else {
		task, err = scanInstanceTaskRecord(tx.QueryRow(ctx, `SELECT `+instanceTaskWorkerColumns+`
			FROM instance_tasks WHERE host_id = $1 AND status = 'PENDING'
			ORDER BY created_at ASC, id ASC LIMIT 1`, hostID))
		if err != nil {
			return nil, err
		}
	}
	if task == nil {
		return nil, nil
	}

	// 4. Serialise per instance and never operate on a deleted instance.
	instanceLocked, err := db.TryAdvisoryTransactionLock(ctx, tx, db.InstanceOperationLockNamespace, task.InstanceID)
	if err != nil {
		return nil, err
	}
	if !instanceLocked {
		return nil, nil
	}
	var instanceStatus string
	err = tx.QueryRow(ctx, `SELECT status::text FROM instances WHERE id = $1`, task.InstanceID).Scan(&instanceStatus)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && instanceStatus == string(models.InstanceStatusDeleted)) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// 5. Conditional transition. Two workers cannot both see RowsAffected == 1.
	claimed, err := scanInstanceTaskRecord(tx.QueryRow(ctx, `UPDATE instance_tasks
		SET status = 'PROCESSING', started_at = $2
		WHERE id = $1 AND status = 'PENDING'
		RETURNING `+instanceTaskWorkerColumns, task.ID, workerNow()))
	if err != nil {
		return nil, err
	}
	if claimed == nil {
		return nil, nil
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return claimed, nil
}

// ClaimLightInstanceTask mirrors claimLightTask(hostId, taskId).
func (s *pgStore) ClaimLightInstanceTask(ctx context.Context, hostID, taskID int32) (*instanceTaskRecord, error) {
	return s.claimInstanceTask(ctx, hostID, &taskID)
}

// ClaimNextInstanceTask mirrors claimNextTask(hostId).
func (s *pgStore) ClaimNextInstanceTask(ctx context.Context, hostID int32) (*instanceTaskRecord, error) {
	return s.claimInstanceTask(ctx, hostID, nil)
}

func (s *pgStore) GetInstanceTask(ctx context.Context, id int32) (*instanceTaskRecord, error) {
	if err := workerRequireDB(); err != nil {
		return nil, err
	}
	return scanInstanceTaskRecord(db.Pool().QueryRow(ctx, `SELECT `+instanceTaskWorkerColumns+`
		FROM instance_tasks WHERE id = $1`, id))
}

func (s *pgStore) UpdateInstanceTaskProgress(ctx context.Context, id int32, progress string) error {
	if err := workerRequireDB(); err != nil {
		return err
	}
	_, err := db.Pool().Exec(ctx, `UPDATE instance_tasks SET progress = $2
		WHERE id = $1 AND status = 'PROCESSING'`, id, progress)
	return err
}

func (s *pgStore) MarkInstanceTaskCompleted(ctx context.Context, id int32) (bool, error) {
	if err := workerRequireDB(); err != nil {
		return false, err
	}
	tag, err := db.Pool().Exec(ctx, `UPDATE instance_tasks
		SET status = 'COMPLETED', finished_at = $2
		WHERE id = $1 AND status = 'PROCESSING'`, id, workerNow())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *pgStore) MarkInstanceTaskFailed(ctx context.Context, id int32, message string) (bool, error) {
	if err := workerRequireDB(); err != nil {
		return false, err
	}
	tag, err := db.Pool().Exec(ctx, `UPDATE instance_tasks
		SET status = 'FAILED', error = $2, finished_at = $3
		WHERE id = $1 AND status = 'PROCESSING'`, id, message, workerNow())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RequeueInstanceTask mirrors the create-task retry branch: retry_count is
// incremented, started_at cleared and the reason stored so the UI can show why the
// task is waiting for its node.
func (s *pgStore) RequeueInstanceTask(ctx context.Context, id int32, message string) (bool, error) {
	if err := workerRequireDB(); err != nil {
		return false, err
	}
	tag, err := db.Pool().Exec(ctx, `UPDATE instance_tasks
		SET status = 'PENDING', retry_count = retry_count + 1, started_at = NULL, error = $2
		WHERE id = $1 AND status = 'PROCESSING'`, id, message)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *pgStore) SetInstanceTaskNewInstanceID(ctx context.Context, id, newInstanceID int32) error {
	if err := workerRequireDB(); err != nil {
		return err
	}
	_, err := db.Pool().Exec(ctx, `UPDATE instance_tasks SET new_instance_id = $2 WHERE id = $1`,
		id, newInstanceID)
	return err
}

// staleInstanceTaskIDs returns the rows the startup/runtime sweep would fail.
func (s *pgStore) staleInstanceTaskIDs(ctx context.Context, includeNeverStarted bool, excludeIDs []int32) ([]int32, error) {
	heavyTimeout := workerNow().Add(-30 * time.Minute)
	lightTimeout := workerNow().Add(-5 * time.Minute)

	query := `SELECT id FROM instance_tasks WHERE status = 'PROCESSING' AND (
		(task_type::text = ANY($1) AND started_at < $2) OR
		(task_type::text <> ALL($1) AND started_at < $3)`
	args := []any{heavyInstanceTaskTypes, heavyTimeout, lightTimeout}
	if includeNeverStarted {
		query += ` OR started_at IS NULL`
	}
	query += `)`
	if len(excludeIDs) > 0 {
		args = append(args, excludeIDs)
		query += ` AND NOT (id = ANY($4))`
	}

	rows, err := db.Pool().Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := []int32{}
	for rows.Next() {
		var id int32
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *pgStore) failInstanceTasks(ctx context.Context, ids []int32, message string) ([]instanceTaskFailureNotice, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := db.Pool().Query(ctx, `UPDATE instance_tasks
		SET status = 'FAILED', error = $2, finished_at = $1
		WHERE id = ANY($3) AND status = 'PROCESSING'
		RETURNING id, user_id, instance_id, host_id, task_type::text`,
		workerNow(), message, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	notices := []instanceTaskFailureNotice{}
	for rows.Next() {
		var notice instanceTaskFailureNotice
		if err := rows.Scan(&notice.ID, &notice.UserID, &notice.InstanceID, &notice.HostID, &notice.TaskType); err != nil {
			return nil, err
		}
		notices = append(notices, notice)
	}
	return notices, rows.Err()
}

// CleanupStaleInstanceTasks mirrors cleanupStaleTasks: called once at startup.
// Unlike the runtime sweep it also fails PROCESSING rows with started_at NULL (a
// crash between "claim" and "started_at" is what the TS guards against here).
func (s *pgStore) CleanupStaleInstanceTasks(ctx context.Context) ([]instanceTaskFailureNotice, error) {
	if err := workerRequireDB(); err != nil {
		return nil, err
	}
	ids, err := s.staleInstanceTaskIDs(ctx, true, nil)
	if err != nil {
		return nil, err
	}
	return s.failInstanceTasks(ctx, ids, "系统启动清理超时任务")
}

// CleanupTimeoutInstanceTasks mirrors cleanupTimeoutTasks: the runtime sweep never
// touches a task this process is currently executing (runningTaskIds).
func (s *pgStore) CleanupTimeoutInstanceTasks(ctx context.Context, running map[int32]struct{}) ([]instanceTaskFailureNotice, error) {
	if err := workerRequireDB(); err != nil {
		return nil, err
	}
	excluded := make([]int32, 0, len(running))
	for id := range running {
		excluded = append(excluded, id)
	}
	ids, err := s.staleInstanceTaskIDs(ctx, false, excluded)
	if err != nil {
		return nil, err
	}
	return s.failInstanceTasks(ctx, ids, "任务执行超时")
}

// ==================== restore tasks ====================

func (s *pgStore) ListPendingRestoreHostIDs(ctx context.Context) ([]int32, error) {
	if err := workerRequireDB(); err != nil {
		return nil, err
	}
	rows, err := db.Pool().Query(ctx, `SELECT DISTINCT host_id FROM restore_tasks WHERE status = 'PENDING'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hostIDs := []int32{}
	for rows.Next() {
		var hostID int32
		if err := rows.Scan(&hostID); err != nil {
			return nil, err
		}
		hostIDs = append(hostIDs, hostID)
	}
	return hostIDs, rows.Err()
}

func (s *pgStore) ClaimNextRestoreTask(ctx context.Context, hostID int32) (*restoreTaskRecord, error) {
	if err := workerRequireDB(); err != nil {
		return nil, err
	}

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := db.TryAdvisoryTransactionLock(ctx, tx, db.HostTaskClaimLockNamespace, hostID)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, nil
	}

	busyChecks := []string{
		`SELECT EXISTS(SELECT 1 FROM restore_tasks WHERE host_id = $1 AND status = 'PROCESSING')`,
		`SELECT EXISTS(SELECT 1 FROM backup_upload_tasks WHERE host_id = $1 AND status = 'PROCESSING')`,
		`SELECT EXISTS(SELECT 1 FROM instance_tasks WHERE host_id = $1 AND status = 'PROCESSING')`,
	}
	for _, query := range busyChecks {
		var busy bool
		if err := tx.QueryRow(ctx, query, hostID).Scan(&busy); err != nil {
			return nil, err
		}
		if busy {
			return nil, nil
		}
	}

	task, err := scanRestoreTaskRecord(tx.QueryRow(ctx, `SELECT `+restoreTaskWorkerColumns+`
		FROM restore_tasks WHERE host_id = $1 AND status = 'PENDING'
		ORDER BY created_at ASC, id ASC LIMIT 1`, hostID))
	if err != nil || task == nil {
		return nil, err
	}

	instanceLocked, err := db.TryAdvisoryTransactionLock(ctx, tx, db.InstanceOperationLockNamespace, task.InstanceID)
	if err != nil {
		return nil, err
	}
	if !instanceLocked {
		return nil, nil
	}
	var instanceStatus string
	err = tx.QueryRow(ctx, `SELECT status::text FROM instances WHERE id = $1`, task.InstanceID).Scan(&instanceStatus)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && instanceStatus == string(models.InstanceStatusDeleted)) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	claimed, err := scanRestoreTaskRecord(tx.QueryRow(ctx, `UPDATE restore_tasks
		SET status = 'PROCESSING', started_at = $2
		WHERE id = $1 AND status = 'PENDING'
		RETURNING `+restoreTaskWorkerColumns, task.ID, workerNow()))
	if err != nil || claimed == nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return claimed, nil
}

func (s *pgStore) GetRestoreTask(ctx context.Context, id int32) (*restoreTaskRecord, error) {
	if err := workerRequireDB(); err != nil {
		return nil, err
	}
	return scanRestoreTaskRecord(db.Pool().QueryRow(ctx, `SELECT `+restoreTaskWorkerColumns+`
		FROM restore_tasks WHERE id = $1`, id))
}

func (s *pgStore) UpdateRestoreTaskProgress(ctx context.Context, id int32, progress string) error {
	if err := workerRequireDB(); err != nil {
		return err
	}
	_, err := db.Pool().Exec(ctx, `UPDATE restore_tasks SET progress = $2
		WHERE id = $1 AND status = 'PROCESSING'`, id, progress)
	return err
}

func (s *pgStore) MarkRestoreTaskCompleted(ctx context.Context, id int32) (bool, error) {
	if err := workerRequireDB(); err != nil {
		return false, err
	}
	tag, err := db.Pool().Exec(ctx, `UPDATE restore_tasks
		SET status = 'COMPLETED', finished_at = $2
		WHERE id = $1 AND status = 'PROCESSING'`, id, workerNow())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// MarkRestoreTaskFailed mirrors the TypeScript's failure updateMany, including the
// optional progress = 'manual_intervention' marker that tells operators a temp
// instance was left behind on purpose.
func (s *pgStore) MarkRestoreTaskFailed(ctx context.Context, id int32, message string, progress *string) (bool, error) {
	if err := workerRequireDB(); err != nil {
		return false, err
	}
	tag, err := db.Pool().Exec(ctx, `UPDATE restore_tasks
		SET status = 'FAILED', error = $2, progress = COALESCE($3, progress), finished_at = $4
		WHERE id = $1 AND status = 'PROCESSING'`, id, message, progress, workerNow())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *pgStore) cleanupRestoreTasks(ctx context.Context, message string, includeNeverStarted bool, excludeIDs []int32) (int64, error) {
	query := `UPDATE restore_tasks SET status = 'FAILED', error = $1, finished_at = $2
		WHERE status = 'PROCESSING' AND started_at < $3`
	args := []any{message, workerNow(), workerNow().Add(-10 * time.Minute)}
	if includeNeverStarted {
		query += ` OR (status = 'PROCESSING' AND started_at IS NULL)`
	}
	if len(excludeIDs) > 0 {
		args = append(args, excludeIDs)
		query = `UPDATE restore_tasks SET status = 'FAILED', error = $1, finished_at = $2
			WHERE status = 'PROCESSING' AND (started_at < $3`
		if includeNeverStarted {
			query += ` OR started_at IS NULL`
		}
		query += `) AND NOT (id = ANY($4))`
	}
	tag, err := db.Pool().Exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CleanupStaleRestoreTasks mirrors cleanupStaleTasks.
func (s *pgStore) CleanupStaleRestoreTasks(ctx context.Context) (int64, error) {
	if err := workerRequireDB(); err != nil {
		return 0, err
	}
	return s.cleanupRestoreTasks(ctx, "系统重启，任务中断", false, nil)
}

// CleanupTimeoutRestoreTasks mirrors cleanupTimeoutTasks.
func (s *pgStore) CleanupTimeoutRestoreTasks(ctx context.Context, running map[int32]struct{}) (int64, error) {
	if err := workerRequireDB(); err != nil {
		return 0, err
	}
	excluded := make([]int32, 0, len(running))
	for id := range running {
		excluded = append(excluded, id)
	}
	return s.cleanupRestoreTasks(ctx, "任务执行超时 (10 分钟)", false, excluded)
}

// ==================== backup upload tasks ====================

func (s *pgStore) ListPendingUploadHostIDs(ctx context.Context) ([]int32, error) {
	if err := workerRequireDB(); err != nil {
		return nil, err
	}
	rows, err := db.Pool().Query(ctx, `SELECT DISTINCT host_id FROM backup_upload_tasks WHERE status = 'PENDING'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hostIDs := []int32{}
	for rows.Next() {
		var hostID int32
		if err := rows.Scan(&hostID); err != nil {
			return nil, err
		}
		hostIDs = append(hostIDs, hostID)
	}
	return hostIDs, rows.Err()
}

func (s *pgStore) ClaimNextUploadTask(ctx context.Context, hostID int32) (*uploadTaskRecord, error) {
	if err := workerRequireDB(); err != nil {
		return nil, err
	}

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := db.TryAdvisoryTransactionLock(ctx, tx, db.HostTaskClaimLockNamespace, hostID)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, nil
	}

	busyChecks := []string{
		`SELECT EXISTS(SELECT 1 FROM backup_upload_tasks WHERE host_id = $1 AND status = 'PROCESSING')`,
		`SELECT EXISTS(SELECT 1 FROM restore_tasks WHERE host_id = $1 AND status = 'PROCESSING')`,
		`SELECT EXISTS(SELECT 1 FROM instance_tasks WHERE host_id = $1 AND status = 'PROCESSING')`,
	}
	for _, query := range busyChecks {
		var busy bool
		if err := tx.QueryRow(ctx, query, hostID).Scan(&busy); err != nil {
			return nil, err
		}
		if busy {
			return nil, nil
		}
	}

	task, err := scanUploadTaskRecord(tx.QueryRow(ctx, `SELECT `+uploadTaskWorkerColumns+`
		FROM backup_upload_tasks WHERE host_id = $1 AND status = 'PENDING'
		ORDER BY created_at ASC, id ASC LIMIT 1`, hostID))
	if err != nil || task == nil {
		return nil, err
	}

	instanceLocked, err := db.TryAdvisoryTransactionLock(ctx, tx, db.InstanceOperationLockNamespace, task.InstanceID)
	if err != nil {
		return nil, err
	}
	if !instanceLocked {
		return nil, nil
	}
	var instanceStatus string
	err = tx.QueryRow(ctx, `SELECT status::text FROM instances WHERE id = $1`, task.InstanceID).Scan(&instanceStatus)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && instanceStatus == string(models.InstanceStatusDeleted)) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	claimed, err := scanUploadTaskRecord(tx.QueryRow(ctx, `UPDATE backup_upload_tasks
		SET status = 'PROCESSING', started_at = $2
		WHERE id = $1 AND status = 'PENDING'
		RETURNING `+uploadTaskWorkerColumns, task.ID, workerNow()))
	if err != nil || claimed == nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return claimed, nil
}

func (s *pgStore) GetUploadTask(ctx context.Context, id int32) (*uploadTaskRecord, error) {
	if err := workerRequireDB(); err != nil {
		return nil, err
	}
	return scanUploadTaskRecord(db.Pool().QueryRow(ctx, `SELECT `+uploadTaskWorkerColumns+`
		FROM backup_upload_tasks WHERE id = $1`, id))
}

func (s *pgStore) UpdateUploadTaskProgress(ctx context.Context, id int32, progress string) error {
	if err := workerRequireDB(); err != nil {
		return err
	}
	_, err := db.Pool().Exec(ctx, `UPDATE backup_upload_tasks SET progress = $2
		WHERE id = $1 AND status = 'PROCESSING'`, id, progress)
	return err
}

func (s *pgStore) MarkUploadTaskCompleted(ctx context.Context, id int32, remoteFileName string, fileSize *int64) (bool, error) {
	if err := workerRequireDB(); err != nil {
		return false, err
	}
	tag, err := db.Pool().Exec(ctx, `UPDATE backup_upload_tasks
		SET status = 'COMPLETED', remote_file_name = $2, file_size = $3, finished_at = $4
		WHERE id = $1 AND status = 'PROCESSING'`, id, remoteFileName, fileSize, workerNow())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *pgStore) RequeueUploadTask(ctx context.Context, id int32) (bool, error) {
	if err := workerRequireDB(); err != nil {
		return false, err
	}
	tag, err := db.Pool().Exec(ctx, `UPDATE backup_upload_tasks
		SET status = 'PENDING', retry_count = retry_count + 1, progress = NULL, started_at = NULL
		WHERE id = $1 AND status = 'PROCESSING'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *pgStore) MarkUploadTaskFailed(ctx context.Context, id int32, message string) (bool, error) {
	if err := workerRequireDB(); err != nil {
		return false, err
	}
	tag, err := db.Pool().Exec(ctx, `UPDATE backup_upload_tasks
		SET status = 'FAILED', error = $2, finished_at = $3
		WHERE id = $1 AND status = 'PROCESSING'`, id, message, workerNow())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// cleanupUploadTasks mirrors cleanupStaleUploadTasks / cleanupTimeoutUploadTasks.
// Both only touch rows whose started_at is older than the 30-minute budget; the
// startup variant is called without the running-task exclusion because nothing is
// running yet in this process.
func (s *pgStore) cleanupUploadTasks(ctx context.Context, message string, excludeIDs []int32) (int64, error) {
	query := `UPDATE backup_upload_tasks SET status = 'FAILED', error = $1, finished_at = $2
		WHERE status = 'PROCESSING' AND started_at < $3`
	args := []any{message, workerNow(), workerNow().Add(-30 * time.Minute)}
	if len(excludeIDs) > 0 {
		args = append(args, excludeIDs)
		query += ` AND NOT (id = ANY($4))`
	}
	tag, err := db.Pool().Exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CleanupStaleUploadTasks mirrors cleanupStaleUploadTasks.
func (s *pgStore) CleanupStaleUploadTasks(ctx context.Context) (int64, error) {
	if err := workerRequireDB(); err != nil {
		return 0, err
	}
	return s.cleanupUploadTasks(ctx, "系统重启，任务中断", nil)
}

// CleanupTimeoutUploadTasks mirrors cleanupTimeoutUploadTasks.
func (s *pgStore) CleanupTimeoutUploadTasks(ctx context.Context, running map[int32]struct{}) (int64, error) {
	if err := workerRequireDB(); err != nil {
		return 0, err
	}
	excluded := make([]int32, 0, len(running))
	for id := range running {
		excluded = append(excluded, id)
	}
	return s.cleanupUploadTasks(ctx, "任务执行超时 (30 分钟)", excluded)
}
