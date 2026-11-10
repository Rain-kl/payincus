// Port of server/src/db/backups.ts plus the narrow read projections
// routes/backups.go needs (the instance/host rows are read through
// db/instances.go and db/hosts.go, which already exist).
//
// Quota accounting is a security boundary. An instance's backup quota IS the
// number of `backups` rows with status in ('creating','ready') — see
// db/quota-operations.ts checkBackupQuota. Creating a manual backup therefore has
// to (a) take the per-instance advisory lock, (b) re-count inside that
// transaction, and (c) insert the row in the same transaction; otherwise two
// concurrent requests both pass a pre-lock count and overshoot the limit.
// createBackupWithQuotaReservation is that atomic step and is what the POST route
// uses (the pre-check in quota-operations is only a fast path).
//
// Time: every time.Time bound into SQL is UTC (WAVE_D_SPEC §0.2b).
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// BackupsDeleteMissingMessage mirrors Prisma's P2025 text for update/delete on a
// missing row. routes surface db errors verbatim inside BACKUP_*_FAILED details.
const backupsUpdateMissingMessage = "An operation failed because it depends on one or more records " +
	"that were required but not found. Record to update not found."

// BackupsRecord mirrors the object db/backups.ts builds
// (`created_at` / `expires_at` are already ISO-8601 strings there, because the
// TypeScript calls toISOString()).
type BackupsRecord struct {
	ID          int32      `json:"id"`
	InstanceID  int32      `json:"instance_id"`
	IncusName   string     `json:"incus_name"`
	Name        string     `json:"name"`
	Description *string    `json:"description"`
	Size        int32      `json:"size"`
	Status      string     `json:"status"`
	CreatedAt   string     `json:"created_at"`
	ExpiresAt   *string    `json:"expires_at"`
	createdRaw  time.Time  `json:"-"`
	expiresRaw  *time.Time `json:"-"`
}

// backupsISOTime mirrors Date#toISOString() for a timestamp(3) column.
func backupsISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// backupsRequireDB guards against calling before db.Init.
func backupsRequireDB() error {
	if DB == nil || Pool() == nil {
		return errors.New("database not initialized")
	}
	return nil
}

// backupsScanRecord converts one row of the backups table.
func backupsScanRecord(row pgx.Row) (BackupsRecord, error) {
	var record BackupsRecord
	var createdAt time.Time
	var expiresAt *time.Time
	err := row.Scan(&record.ID, &record.InstanceID, &record.IncusName, &record.Name,
		&record.Description, &record.Size, &record.Status, &createdAt, &expiresAt)
	if err != nil {
		return record, err
	}
	record.CreatedAt = backupsISOTime(createdAt)
	if expiresAt != nil {
		formatted := backupsISOTime(*expiresAt)
		record.ExpiresAt = &formatted
	}
	return record, nil
}

const backupsSelectColumns = `id, instance_id, incus_name, name, description, size, status,
	created_at, expires_at`

// GetBackupsByInstanceID mirrors getBackupsByInstanceId: every row of the instance
// except status='deleted', newest first.
func GetBackupsByInstanceID(ctx context.Context, instanceID int32) ([]BackupsRecord, error) {
	if err := backupsRequireDB(); err != nil {
		return nil, err
	}
	rows, err := Pool().Query(ctx, `SELECT `+backupsSelectColumns+` FROM backups
		WHERE instance_id = $1 AND status <> 'deleted'
		ORDER BY created_at DESC`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []BackupsRecord{}
	for rows.Next() {
		record, err := backupsScanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// GetBackupByID mirrors getBackupById: nil, nil when the row does not exist.
func GetBackupByID(ctx context.Context, id int32) (*BackupsRecord, error) {
	if err := backupsRequireDB(); err != nil {
		return nil, err
	}
	record, err := backupsScanRecord(Pool().QueryRow(ctx,
		`SELECT `+backupsSelectColumns+` FROM backups WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// BackupsCreateData mirrors createBackup's argument object.
type BackupsCreateData struct {
	InstanceID  int32
	IncusName   string
	Name        string
	Description *string
	ExpiresAt   *time.Time
}

// CreateBackup mirrors createBackup. Status is always 'creating'.
func CreateBackup(ctx context.Context, data BackupsCreateData) (int32, error) {
	if err := backupsRequireDB(); err != nil {
		return 0, err
	}
	var id int32
	err := Pool().QueryRow(ctx, `INSERT INTO backups
		(instance_id, incus_name, name, description, expires_at, status, created_at)
		VALUES ($1, $2, $3, $4, $5, 'creating', $6)
		RETURNING id`,
		data.InstanceID, data.IncusName, data.Name, data.Description, data.ExpiresAt,
		time.Now().UTC().Truncate(time.Millisecond)).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// BackupsQuotaReservationResult mirrors BackupQuotaReservationResult.
type BackupsQuotaReservationResult struct {
	Allowed  bool
	BackupID *int32
	Current  int32
	Limit    int32
	Message  string
}

// BackupsQuotaDenialMessage mirrors the two denial texts of checkBackupQuota.
func BackupsQuotaDenialMessage(current, limit int32) string {
	if limit == 0 {
		return "Backup quota not allocated for this instance"
	}
	return fmt.Sprintf("Instance backup quota full (%d/%d)", current, limit)
}

// CreateBackupWithQuotaReservation mirrors createBackupWithQuotaReservation.
//
// One transaction: advisory lock on the instance → read backup_limit → count
// consumed slots → refuse or insert. The lock is transaction-scoped and released
// by COMMIT/ROLLBACK, so it serialises concurrent reservations for the same
// instance without blocking other instances.
func CreateBackupWithQuotaReservation(
	ctx context.Context,
	data BackupsCreateData,
) (BackupsQuotaReservationResult, error) {
	if err := backupsRequireDB(); err != nil {
		return BackupsQuotaReservationResult{}, err
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return BackupsQuotaReservationResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := AdvisoryTransactionLock(ctx, tx, InstanceOperationLockNamespace, data.InstanceID); err != nil {
		return BackupsQuotaReservationResult{}, err
	}

	var limit *int32
	err = tx.QueryRow(ctx, `SELECT backup_limit FROM instances WHERE id = $1`, data.InstanceID).Scan(&limit)
	if errors.Is(err, pgx.ErrNoRows) {
		return BackupsQuotaReservationResult{Message: "Instance not found"}, nil
	}
	if err != nil {
		return BackupsQuotaReservationResult{}, err
	}

	var current int32
	if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM backups
		WHERE instance_id = $1 AND status IN ('creating', 'ready')`, data.InstanceID).Scan(&current); err != nil {
		return BackupsQuotaReservationResult{}, err
	}

	effectiveLimit := int32(0)
	if limit != nil {
		effectiveLimit = *limit
	}

	if effectiveLimit == 0 {
		return BackupsQuotaReservationResult{
			Current: current,
			Limit:   0,
			Message: BackupsQuotaDenialMessage(current, 0),
		}, nil
	}
	if current >= effectiveLimit {
		return BackupsQuotaReservationResult{
			Current: current,
			Limit:   effectiveLimit,
			Message: BackupsQuotaDenialMessage(current, effectiveLimit),
		}, nil
	}

	var backupID int32
	err = tx.QueryRow(ctx, `INSERT INTO backups
		(instance_id, incus_name, name, description, expires_at, status, created_at)
		VALUES ($1, $2, $3, $4, $5, 'creating', $6)
		RETURNING id`,
		data.InstanceID, data.IncusName, data.Name, data.Description, data.ExpiresAt,
		time.Now().UTC().Truncate(time.Millisecond)).Scan(&backupID)
	if err != nil {
		return BackupsQuotaReservationResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return BackupsQuotaReservationResult{}, err
	}

	return BackupsQuotaReservationResult{
		Allowed:  true,
		BackupID: &backupID,
		Current:  current + 1,
		Limit:    effectiveLimit,
	}, nil
}

// UpdateBackupStatus mirrors updateBackupStatus. The `_error` argument of the
// TypeScript is intentionally absent: the backups table has no error column and
// Node discards it.
func UpdateBackupStatus(ctx context.Context, id int32, status string) error {
	if err := backupsRequireDB(); err != nil {
		return err
	}
	tag, err := Pool().Exec(ctx, `UPDATE backups SET status = $2 WHERE id = $1`, id, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New(backupsUpdateMissingMessage)
	}
	return nil
}

// UpdateBackupSize mirrors updateBackupSize.
func UpdateBackupSize(ctx context.Context, id int32, size int32) error {
	if err := backupsRequireDB(); err != nil {
		return err
	}
	tag, err := Pool().Exec(ctx, `UPDATE backups SET size = $2 WHERE id = $1`, id, size)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New(backupsUpdateMissingMessage)
	}
	return nil
}

// DefaultStaleCreatingBackupAgeMS mirrors DEFAULT_STALE_CREATING_BACKUP_AGE_MS.
const DefaultStaleCreatingBackupAgeMS int64 = 24 * 60 * 60 * 1000

// CleanupStaleCreatingBackups mirrors cleanupStaleCreatingBackups.
func CleanupStaleCreatingBackups(ctx context.Context, maxAgeMs int64) (int64, error) {
	if err := backupsRequireDB(); err != nil {
		return 0, err
	}
	cutoff := time.Now().UTC().Add(-time.Duration(maxAgeMs) * time.Millisecond).Truncate(time.Millisecond)
	tag, err := Pool().Exec(ctx, `UPDATE backups SET status = 'error'
		WHERE status = 'creating' AND created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteBackup mirrors deleteBackup: a soft delete, so the row stays for history.
func DeleteBackup(ctx context.Context, id int32) error {
	if err := backupsRequireDB(); err != nil {
		return err
	}
	tag, err := Pool().Exec(ctx, `UPDATE backups SET status = 'deleted' WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New(backupsUpdateMissingMessage)
	}
	return nil
}

// BackupsPolicy mirrors the subset of BackupPolicy the routes and the scheduler
// read. last_run_at / next_run_at keep their raw time so the route can format them
// with toISOString semantics.
type BackupsPolicy struct {
	ID              int32
	InstanceID      int32
	Enabled         bool
	IntervalMinutes int32
	LastRunAt       *time.Time
	NextRunAt       *time.Time
}

// GetBackupPolicy mirrors getBackupPolicy: nil when the instance has no policy.
func GetBackupPolicy(ctx context.Context, instanceID int32) (*BackupsPolicy, error) {
	if err := backupsRequireDB(); err != nil {
		return nil, err
	}
	var policy BackupsPolicy
	err := Pool().QueryRow(ctx, `SELECT id, instance_id, enabled, interval_minutes,
		last_run_at, next_run_at FROM backup_policies WHERE instance_id = $1`, instanceID).
		Scan(&policy.ID, &policy.InstanceID, &policy.Enabled, &policy.IntervalMinutes,
			&policy.LastRunAt, &policy.NextRunAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &policy, nil
}

// UpsertBackupPolicy mirrors upsertBackupPolicy.
//
// Faithfully preserved quirk: `next_run_at` is computed (now + interval) only on
// INSERT. The UPDATE branch leaves next_run_at untouched, so changing
// interval_minutes does not reschedule the next run — the scheduler's
// updateBackupPolicyLastRun does that after each run.
func UpsertBackupPolicy(ctx context.Context, instanceID int32, enabled bool, intervalMinutes int32) error {
	if err := backupsRequireDB(); err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	nextRunAt := now.Add(time.Duration(intervalMinutes) * time.Minute)

	_, err := Pool().Exec(ctx, `INSERT INTO backup_policies
		(instance_id, enabled, interval_minutes, next_run_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (instance_id) DO UPDATE SET
			enabled = EXCLUDED.enabled,
			interval_minutes = EXCLUDED.interval_minutes,
			updated_at = EXCLUDED.updated_at`,
		instanceID, enabled, intervalMinutes, nextRunAt, now)
	return err
}

// UpdateBackupPolicyLastRun mirrors updateBackupPolicyLastRun.
func UpdateBackupPolicyLastRun(ctx context.Context, instanceID int32) error {
	if err := backupsRequireDB(); err != nil {
		return err
	}
	policy, err := GetBackupPolicy(ctx, instanceID)
	if err != nil || policy == nil {
		return err
	}

	now := time.Now().UTC().Truncate(time.Millisecond)
	nextRunAt := now.Add(time.Duration(policy.IntervalMinutes) * time.Minute)
	_, err = Pool().Exec(ctx, `UPDATE backup_policies
		SET last_run_at = $2, next_run_at = $3, updated_at = $2
		WHERE instance_id = $1`, instanceID, now, nextRunAt)
	return err
}

// GetPendingBackupPolicies mirrors getPendingBackupPolicies: enabled policies
// whose next_run_at is NULL or already due, soonest first.
func GetPendingBackupPolicies(ctx context.Context) ([]BackupsPolicy, error) {
	if err := backupsRequireDB(); err != nil {
		return nil, err
	}
	rows, err := Pool().Query(ctx, `SELECT id, instance_id, enabled, interval_minutes,
		last_run_at, next_run_at FROM backup_policies
		WHERE enabled = true AND (next_run_at IS NULL OR next_run_at <= $1)
		ORDER BY next_run_at ASC`, time.Now().UTC().Truncate(time.Millisecond))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	policies := []BackupsPolicy{}
	for rows.Next() {
		var policy BackupsPolicy
		if err := rows.Scan(&policy.ID, &policy.InstanceID, &policy.Enabled,
			&policy.IntervalMinutes, &policy.LastRunAt, &policy.NextRunAt); err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}
