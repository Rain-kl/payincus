// Port of server/src/db/instance-tasks.ts (291 lines, 9 exports + 1 error class).
//
// Instance operations (start/stop/rebuild/clone/change_host/…) are serialised with
// the transaction-scoped advisory lock INSTANCE_OPERATION_LOCK_NAMESPACE (4106)
// keyed on the instance id. Two rules follow from that and must not be relaxed:
//
//  1. The lock and the row write share ONE transaction. pg_advisory_xact_lock is
//     released when its transaction ends, so taking it on the pool would scope it
//     to a single implicit statement and provide no mutual exclusion at all.
//  2. "No lock" is a conflict, not a retry loop: createInstanceTask throws
//     InstanceTaskConflictError (-> HTTP 409 TASK_IN_PROGRESS) and
//     cancelInstanceTask returns null. Both branches are reproduced here.
//
// Every timestamp written to SQL is UTC (WAVE_D_SPEC §0.2b). instance_tasks has no
// updated_at column, so the raw-insert guard does not apply; created_at is still
// written explicitly so a task queued by the Go process sorts identically against
// tasks queued by Node (`createdAt: { lte }` drives getTaskQueuePosition).
package db

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ErrInstanceTaskRecordNotFound mirrors Prisma P2025 for instance_tasks.update():
// the TypeScript uses update() (not updateMany), so a missing task throws.
var ErrInstanceTaskRecordNotFound = errors.New("instanceTasks: record to update not found")

// InstanceTaskConflictError mirrors the TypeScript class of the same name. Routes
// detect it with errors.As and answer 409 TASK_IN_PROGRESS; ActiveTask carries the
// blocking row when one exists (nil when the advisory lock itself was busy).
type InstanceTaskConflictError struct {
	ActiveTask *InstanceTaskWithDetails
}

// Error implements error. The message is the class's message verbatim.
func (e *InstanceTaskConflictError) Error() string { return "Instance has an active task" }

// InstanceTaskWithDetails mirrors the InstanceTaskWithDetails interface. Prisma
// returns the whole row (the interface omits `payload`, but the runtime object the
// routes serialise does not), so payload is part of the wire shape here too.
// Timestamps are ISO-8601 strings, exactly what JSON.stringify(Date) produces.
type InstanceTaskWithDetails struct {
	ID                   int32                     `json:"id"`
	InstanceID           int32                     `json:"instanceId"`
	HostID               int32                     `json:"hostId"`
	UserID               int32                     `json:"userId"`
	TaskType             models.InstanceTaskType   `json:"taskType"`
	Status               models.InstanceTaskStatus `json:"status"`
	Progress             *string                   `json:"progress"`
	RetryCount           int32                     `json:"retryCount"`
	ImageAlias           *string                   `json:"imageAlias"`
	SSHKeyID             *int32                    `json:"sshKeyId"`
	CustomInitCommandIDs *string                   `json:"customInitCommandIds"`
	TargetName           *string                   `json:"targetName"`
	TargetHostID         *int32                    `json:"targetHostId"`
	SnapshotName         *string                   `json:"snapshotName"`
	NewInstanceID        *int32                    `json:"newInstanceId"`
	Payload              json.RawMessage           `json:"payload"`
	Error                *string                   `json:"error"`
	CreatedAt            string                    `json:"createdAt"`
	StartedAt            *string                   `json:"startedAt"`
	FinishedAt           *string                   `json:"finishedAt"`
}

// CreateInstanceTaskData mirrors CreateInstanceTaskData.
type CreateInstanceTaskData struct {
	InstanceID           int32
	HostID               int32
	UserID               int32
	TaskType             models.InstanceTaskType
	ImageAlias           *string
	SSHKeyID             *int32
	CustomInitCommandIDs []int32
	TargetName           *string
	TargetHostID         *int32
	SnapshotName         *string
}

// InstanceTaskStatusUpdates mirrors the optional `updates` argument of
// updateInstanceTaskStatus. A field is written only when its Set flag is true: the
// TypeScript spreads the object, so an explicitly passed empty string is written
// while an absent key is skipped.
type InstanceTaskStatusUpdates struct {
	Progress      *string
	Error         *string
	StartedAt     *time.Time
	FinishedAt    *time.Time
	NewInstanceID *int32
}

// InstanceTasksOptions mirrors getUserInstanceTasks' options object.
type InstanceTasksOptions struct {
	Page     *int
	PageSize *int
	Status   []models.InstanceTaskStatus
}

// InstanceTasksPage mirrors getUserInstanceTasks' `{ items, total }`.
type InstanceTasksPage struct {
	Items []InstanceTaskWithDetails `json:"items"`
	Total int64                     `json:"total"`
}

// instanceTaskStatuses mirrors INSTANCE_TASK_STATUSES: the only four values
// normalizeInstanceTaskStatuses lets through.
var instanceTaskStatuses = []models.InstanceTaskStatus{
	models.InstanceTaskStatusPENDING,
	models.InstanceTaskStatusPROCESSING,
	models.InstanceTaskStatusCOMPLETED,
	models.InstanceTaskStatusFAILED,
}

// instanceTaskSelectColumns is the projection every task read shares, in the
// Prisma column order (which is also the JSON key order).
const instanceTaskSelectColumns = `id, instance_id, host_id, user_id, task_type, status,
	progress, retry_count, image_alias, ssh_key_id, custom_init_command_ids,
	target_name, target_host_id, snapshot_name, new_instance_id, payload, error,
	created_at, started_at, finished_at`

// instanceTaskRow is the scan target for instanceTaskSelectColumns.
type instanceTaskRow struct {
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
	CreatedAt            time.Time
	StartedAt            *time.Time
	FinishedAt           *time.Time
}

func instanceTaskScanRow(scanner interface {
	Scan(dest ...any) error
}) (*instanceTaskRow, error) {
	var row instanceTaskRow
	err := scanner.Scan(
		&row.ID, &row.InstanceID, &row.HostID, &row.UserID, &row.TaskType, &row.Status,
		&row.Progress, &row.RetryCount, &row.ImageAlias, &row.SSHKeyID,
		&row.CustomInitCommandIDs, &row.TargetName, &row.TargetHostID, &row.SnapshotName,
		&row.NewInstanceID, &row.Payload, &row.Error, &row.CreatedAt, &row.StartedAt,
		&row.FinishedAt,
	)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (row instanceTaskRow) toRecord() InstanceTaskWithDetails {
	payload := json.RawMessage(row.Payload)
	if len(payload) == 0 {
		payload = json.RawMessage("null")
	}
	return InstanceTaskWithDetails{
		ID:                   row.ID,
		InstanceID:           row.InstanceID,
		HostID:               row.HostID,
		UserID:               row.UserID,
		TaskType:             models.InstanceTaskType(row.TaskType),
		Status:               models.InstanceTaskStatus(row.Status),
		Progress:             row.Progress,
		RetryCount:           row.RetryCount,
		ImageAlias:           row.ImageAlias,
		SSHKeyID:             row.SSHKeyID,
		CustomInitCommandIDs: row.CustomInitCommandIDs,
		TargetName:           row.TargetName,
		TargetHostID:         row.TargetHostID,
		SnapshotName:         row.SnapshotName,
		NewInstanceID:        row.NewInstanceID,
		Payload:              payload,
		Error:                row.Error,
		CreatedAt:            instancesISOTime(row.CreatedAt),
		StartedAt:            instancesOptionalISOTime(row.StartedAt),
		FinishedAt:           instancesOptionalISOTime(row.FinishedAt),
	}
}

func instanceTasksRequireDB() error {
	if Pool() == nil || DB == nil {
		return errors.New("instance tasks: database not initialized")
	}
	return nil
}

// instanceTaskIDTruthy mirrors the TypeScript's `data.x || null` guards: 0 is
// falsy and therefore stored as NULL.
func instanceTaskIDTruthy(value *int32) *int32 {
	if value == nil || *value == 0 {
		return nil
	}
	return value
}

// instanceTaskStringTruthy mirrors `data.imageAlias || null`: an empty string is
// falsy and therefore stored as NULL.
func instanceTaskStringTruthy(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	return value
}

// CreateInstanceTask mirrors createInstanceTask.
func CreateInstanceTask(ctx context.Context, data CreateInstanceTaskData) (*InstanceTaskWithDetails, error) {
	if err := instanceTasksRequireDB(); err != nil {
		return nil, err
	}

	// The GPU guard runs BEFORE the transaction and before the lock, exactly like
	// `assertGpuMigration` at the top of the TypeScript function.
	if data.TaskType == models.InstanceTaskTypeChangeHost || data.TaskType == models.InstanceTaskTypeClone {
		if err := GpusAssertMigration(ctx, data.InstanceID); err != nil {
			return nil, err
		}
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
		// The TypeScript throws without an activeTask here.
		return nil, &InstanceTaskConflictError{}
	}

	active, err := instanceTaskQueryActiveAsc(ctx, tx, data.InstanceID)
	if err != nil {
		return nil, err
	}
	if active != nil {
		return nil, &InstanceTaskConflictError{ActiveTask: active}
	}

	var customInitCommandIDs *string
	if data.CustomInitCommandIDs != nil {
		// `data.customInitCommandIds ? JSON.stringify(...) : null` — a present array
		// is stringified even when empty (an [] is truthy in JavaScript), only a
		// missing value becomes NULL.
		encoded, err := json.Marshal(data.CustomInitCommandIDs)
		if err != nil {
			return nil, err
		}
		text := string(encoded)
		customInitCommandIDs = &text
	}

	created, err := instanceTaskScanRow(tx.QueryRow(ctx, `INSERT INTO instance_tasks (
			instance_id, host_id, user_id, task_type, status, image_alias, ssh_key_id,
			custom_init_command_ids, target_name, target_host_id, snapshot_name, created_at
		) VALUES ($1, $2, $3, $4, 'PENDING', $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+instanceTaskSelectColumns,
		data.InstanceID, data.HostID, data.UserID, string(data.TaskType),
		instanceTaskStringTruthy(data.ImageAlias), instanceTaskIDTruthy(data.SSHKeyID),
		customInitCommandIDs, instanceTaskStringTruthy(data.TargetName),
		instanceTaskIDTruthy(data.TargetHostID), instanceTaskStringTruthy(data.SnapshotName),
		time.Now().UTC(),
	))
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	record := created.toRecord()
	return &record, nil
}

// instanceTaskQueryActiveAsc mirrors the conflict lookup inside createInstanceTask:
// the oldest active task (createdAt asc, id asc) is reported to the caller.
func instanceTaskQueryActiveAsc(ctx context.Context, tx pgx.Tx, instanceID int32) (*InstanceTaskWithDetails, error) {
	row, err := instanceTaskScanRow(tx.QueryRow(ctx, `SELECT `+instanceTaskSelectColumns+`
		FROM instance_tasks
		WHERE instance_id = $1 AND status IN ('PENDING', 'PROCESSING')
		ORDER BY created_at ASC, id ASC
		LIMIT 1`, instanceID))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record := row.toRecord()
	return &record, nil
}

// GetInstanceTaskById mirrors getInstanceTaskById: nil, nil when absent.
func GetInstanceTaskById(ctx context.Context, taskID int32) (*InstanceTaskWithDetails, error) {
	if err := instanceTasksRequireDB(); err != nil {
		return nil, err
	}
	row, err := instanceTaskScanRow(Pool().QueryRow(ctx, `SELECT `+instanceTaskSelectColumns+`
		FROM instance_tasks WHERE id = $1`, taskID))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record := row.toRecord()
	return &record, nil
}

// GetActiveTaskForInstance mirrors getActiveTaskForInstance: newest active task
// (createdAt desc) or nil.
func GetActiveTaskForInstance(ctx context.Context, instanceID int32) (*InstanceTaskWithDetails, error) {
	if err := instanceTasksRequireDB(); err != nil {
		return nil, err
	}
	row, err := instanceTaskScanRow(Pool().QueryRow(ctx, `SELECT `+instanceTaskSelectColumns+`
		FROM instance_tasks
		WHERE instance_id = $1 AND status IN ('PENDING', 'PROCESSING')
		ORDER BY created_at DESC
		LIMIT 1`, instanceID))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record := row.toRecord()
	return &record, nil
}

// instanceTasksClampPagination mirrors clampInstanceTaskPagination. pageSize is
// clamped to [1, 100] with a default of 20; page must be a positive integer.
func instanceTasksClampPagination(page, pageSize *int) (int, int) {
	normalizedPage := 1
	if page != nil && *page > 0 {
		normalizedPage = *page
	}
	normalizedPageSize := 20
	if pageSize != nil {
		value := *pageSize
		if value < 1 {
			value = 1
		}
		if value > 100 {
			value = 100
		}
		normalizedPageSize = value
	}
	return normalizedPage, normalizedPageSize
}

// instanceTasksNormalizeStatuses mirrors normalizeInstanceTaskStatuses: unknown
// values are dropped and an empty result means "no status filter".
func instanceTasksNormalizeStatuses(status []models.InstanceTaskStatus) []models.InstanceTaskStatus {
	if status == nil {
		return nil
	}
	normalized := make([]models.InstanceTaskStatus, 0, len(status))
	for _, value := range status {
		for _, allowed := range instanceTaskStatuses {
			if value == allowed {
				normalized = append(normalized, value)
				break
			}
		}
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

// GetUserInstanceTasks mirrors getUserInstanceTasks.
func GetUserInstanceTasks(ctx context.Context, userID int32, options InstanceTasksOptions) (InstanceTasksPage, error) {
	if err := instanceTasksRequireDB(); err != nil {
		return InstanceTasksPage{}, err
	}
	page, pageSize := instanceTasksClampPagination(options.Page, options.PageSize)
	statuses := instanceTasksNormalizeStatuses(options.Status)

	query := `SELECT ` + instanceTaskSelectColumns + ` FROM instance_tasks WHERE user_id = $1`
	countQuery := `SELECT COUNT(*) FROM instance_tasks WHERE user_id = $1`
	args := []any{userID}
	if len(statuses) > 0 {
		placeholders := make([]string, 0, len(statuses))
		for _, status := range statuses {
			args = append(args, string(status))
			placeholders = append(placeholders, "$"+strconv.Itoa(len(args)))
		}
		clause := " AND status IN (" + strings.Join(placeholders, ", ") + ")"
		query += clause
		countQuery += clause
	}
	query += ` ORDER BY created_at DESC LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)

	offset := (page - 1) * pageSize
	rows, err := Pool().Query(ctx, query, append(append([]any{}, args...), pageSize, offset)...)
	if err != nil {
		return InstanceTasksPage{}, err
	}
	defer rows.Close()

	items := make([]InstanceTaskWithDetails, 0, pageSize)
	for rows.Next() {
		row, err := instanceTaskScanRow(rows)
		if err != nil {
			return InstanceTasksPage{}, err
		}
		items = append(items, row.toRecord())
	}
	if err := rows.Err(); err != nil {
		return InstanceTasksPage{}, err
	}

	var total int64
	if err := Pool().QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return InstanceTasksPage{}, err
	}
	return InstanceTasksPage{Items: items, Total: total}, nil
}

// instanceTaskStatusUpdateAssignments builds the SET list of
// updateInstanceTaskStatus for the values that follow $1 (the task id). The
// TypeScript spreads `{ status, ...updates }`, so a present key is written even
// when its value is an empty string or zero, while an absent key is untouched —
// that is a presence test, not a truthiness test.
func instanceTaskStatusUpdateAssignments(
	status models.InstanceTaskStatus,
	updates *InstanceTaskStatusUpdates,
) ([]string, []any) {
	sets := []string{"status = $2"}
	values := []any{string(status)}
	add := func(column string, value any) {
		values = append(values, value)
		sets = append(sets, column+" = $"+strconv.Itoa(len(values)+1))
	}
	if updates != nil {
		if updates.Progress != nil {
			add("progress", *updates.Progress)
		}
		if updates.Error != nil {
			add("error", *updates.Error)
		}
		if updates.StartedAt != nil {
			add("started_at", updates.StartedAt.UTC())
		}
		if updates.FinishedAt != nil {
			add("finished_at", updates.FinishedAt.UTC())
		}
		if updates.NewInstanceID != nil {
			add("new_instance_id", *updates.NewInstanceID)
		}
	}
	return sets, values
}

// UpdateInstanceTaskStatus mirrors updateInstanceTaskStatus.
func UpdateInstanceTaskStatus(
	ctx context.Context,
	taskID int32,
	status models.InstanceTaskStatus,
	updates *InstanceTaskStatusUpdates,
) (*InstanceTaskWithDetails, error) {
	if err := instanceTasksRequireDB(); err != nil {
		return nil, err
	}

	sets, values := instanceTaskStatusUpdateAssignments(status, updates)
	args := append([]any{taskID}, values...)

	row, err := instanceTaskScanRow(Pool().QueryRow(ctx, `UPDATE instance_tasks SET `+
		strings.Join(sets, ", ")+` WHERE id = $1 RETURNING `+instanceTaskSelectColumns, args...))
	if isNoRows(err) {
		return nil, ErrInstanceTaskRecordNotFound
	}
	if err != nil {
		return nil, err
	}
	record := row.toRecord()
	return &record, nil
}

// UpdateInstanceTaskProgress mirrors updateInstanceTaskProgress.
func UpdateInstanceTaskProgress(ctx context.Context, taskID int32, progress string) error {
	if err := instanceTasksRequireDB(); err != nil {
		return err
	}
	result := DB.WithContext(ctx).Exec(
		"UPDATE instance_tasks SET progress = ? WHERE id = ?", progress, taskID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrInstanceTaskRecordNotFound
	}
	return nil
}

// GetTaskQueuePosition mirrors getTaskQueuePosition: 0 when the task does not
// exist, otherwise the number of PENDING tasks on the host created at or before it
// (the task itself included, as in the TypeScript).
func GetTaskQueuePosition(ctx context.Context, taskID, hostID int32) (int64, error) {
	if err := instanceTasksRequireDB(); err != nil {
		return 0, err
	}
	var createdAt *time.Time
	err := Pool().QueryRow(ctx, `SELECT created_at FROM instance_tasks WHERE id = $1`, taskID).Scan(&createdAt)
	if isNoRows(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if createdAt == nil {
		return 0, nil
	}
	var position int64
	if err := Pool().QueryRow(ctx, `SELECT COUNT(*) FROM instance_tasks
		WHERE host_id = $1 AND status = 'PENDING' AND created_at <= $2`,
		hostID, createdAt.UTC()).Scan(&position); err != nil {
		return 0, err
	}
	return position, nil
}

// CleanupOldTasks mirrors cleanupOldTasks: delete COMPLETED/FAILED tasks finished
// more than seven days ago and return the deleted count.
func CleanupOldTasks(ctx context.Context) (int64, error) {
	if err := instanceTasksRequireDB(); err != nil {
		return 0, err
	}
	sevenDaysAgo := time.Now().UTC().Add(-7 * 24 * time.Hour)
	result := DB.WithContext(ctx).Exec(
		`DELETE FROM instance_tasks WHERE status IN ('COMPLETED', 'FAILED') AND finished_at < ?`,
		sevenDaysAgo)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// CancelInstanceTask mirrors cancelInstanceTask: nil when the task is missing, not
// PENDING, the lock is busy, or the conditional update lost a race.
func CancelInstanceTask(ctx context.Context, taskID int32) (*InstanceTaskWithDetails, error) {
	if err := instanceTasksRequireDB(); err != nil {
		return nil, err
	}

	var (
		instanceID int32
		status     string
	)
	err := Pool().QueryRow(ctx, `SELECT instance_id, status FROM instance_tasks WHERE id = $1`, taskID).
		Scan(&instanceID, &status)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if models.InstanceTaskStatus(status) != models.InstanceTaskStatusPENDING {
		return nil, nil
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := TryAdvisoryTransactionLock(ctx, tx, InstanceOperationLockNamespace, instanceID)
	if err != nil {
		return nil, err
	}
	if !locked {
		// The TypeScript returns null from inside the transaction; nothing was
		// written, so rolling back here is equivalent and cheaper.
		return nil, nil
	}

	tag, err := tx.Exec(ctx, `UPDATE instance_tasks SET status = 'FAILED', error = '用户取消',
			finished_at = $2
		WHERE id = $1 AND status = 'PENDING'`, taskID, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, nil
	}

	row, err := instanceTaskScanRow(tx.QueryRow(ctx, `SELECT `+instanceTaskSelectColumns+`
		FROM instance_tasks WHERE id = $1`, taskID))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	record := row.toRecord()
	return &record, nil
}
