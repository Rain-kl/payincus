// Port of server/src/db/snapshots.ts plus the narrow read projections that
// routes/snapshots.go needs from db/instances.ts, db/hosts.ts, db/transfers.ts
// and db/quota-operations.ts.
//
// Why projections: db/instances.go, db/hosts.go and db/transfers.ts belong to the
// high-risk instance/host wave and are not ported yet. Following the
// db/permission_lookups.go precedent, the helpers below select ONLY the columns
// the route reads, so they cannot drift into owning business logic. The host
// projection is shared with routes/batch_config.go (both modules are ported in one
// change and both read a host row only to build an IncusClient).
//
// Quota accounting is a security boundary. snapshots.ts never decrements a
// counter: an instance's snapshot quota IS the number of `snapshots` rows
// (quota-operations.ts `checkSnapshotQuota` counts them). So the row inserted by
// SnapshotsCreate consumes one slot and the row removed by SnapshotsDelete
// releases one, and the create route re-checks the count *inside* the
// `auto-policy:snapshot:<instanceId>` distributed lock — the same ordering the
// TypeScript uses so that concurrent manual snapshots cannot both pass a
// pre-lock check.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// Prisma's P2025 texts, reproduced because routes surface the db error message
// verbatim (snapshot delete / batch-config per-item errors).
const (
	snapshotsDeleteMissingMessage = "An operation failed because it depends on one or more records " +
		"that were required but not found. Record to delete does not exist."
	snapshotsUpdateMissingMessage = "An operation failed because it depends on one or more records " +
		"that were required but not found. Record to update not found."
)

// ==================== projections shared with routes/batch_config.go ====================

// SnapshotsHost is the projection of getHostById that the two routes read:
// ownership (for the batch-config permission check / permission model), the
// display name used in notifications and logs, and the mTLS material for
// getIncusClient.
type SnapshotsHost struct {
	ID       int32
	UserID   int32
	Name     string
	URL      string
	CertPath *string
	KeyPath  *string
}

// SnapshotsGetHostByID mirrors the subset of getHostById the routes consume.
func SnapshotsGetHostByID(ctx context.Context, hostID int32) (*SnapshotsHost, error) {
	var rows []struct {
		ID       int32   `gorm:"column:id"`
		UserID   int32   `gorm:"column:user_id"`
		Name     string  `gorm:"column:name"`
		URL      string  `gorm:"column:url"`
		CertPath *string `gorm:"column:cert_path"`
		KeyPath  *string `gorm:"column:key_path"`
	}
	err := DB.WithContext(ctx).
		Table("hosts").
		Select("id, user_id, name, url, cert_path, key_path").
		Where("id = ?", hostID).
		Limit(1).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &SnapshotsHost{
		ID:       rows[0].ID,
		UserID:   rows[0].UserID,
		Name:     rows[0].Name,
		URL:      rows[0].URL,
		CertPath: rows[0].CertPath,
		KeyPath:  rows[0].KeyPath,
	}, nil
}

// SnapshotsInstance is the projection of getInstanceById that routes/snapshots.go
// reads: identity, ownership, the Incus name and the three status/quota fields.
type SnapshotsInstance struct {
	ID            int32
	IncusID       string
	Name          string
	UserID        int32
	HostID        int32
	Status        models.InstanceStatus
	SuspendReason *string
	SnapshotLimit *int32
}

// SnapshotsGetInstanceByID mirrors the subset of getInstanceById the route reads.
func SnapshotsGetInstanceByID(ctx context.Context, instanceID int32) (*SnapshotsInstance, error) {
	var rows []struct {
		ID            int32                 `gorm:"column:id"`
		IncusID       string                `gorm:"column:incus_id"`
		Name          string                `gorm:"column:name"`
		UserID        int32                 `gorm:"column:user_id"`
		HostID        int32                 `gorm:"column:host_id"`
		Status        models.InstanceStatus `gorm:"column:status"`
		SuspendReason *string               `gorm:"column:suspend_reason"`
		SnapshotLimit *int32                `gorm:"column:snapshot_limit"`
	}
	err := DB.WithContext(ctx).
		Table("instances").
		Select("id, incus_id, name, user_id, host_id, status, suspend_reason, snapshot_limit").
		Where("id = ?", instanceID).
		Limit(1).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	return &SnapshotsInstance{
		ID:            row.ID,
		IncusID:       row.IncusID,
		Name:          row.Name,
		UserID:        row.UserID,
		HostID:        row.HostID,
		Status:        row.Status,
		SuspendReason: row.SuspendReason,
		SnapshotLimit: row.SnapshotLimit,
	}, nil
}

// SnapshotsHasPendingTransfer mirrors hasPendingTransfer: any transfer in
// `pending` or `processing` blocks snapshot mutations.
func SnapshotsHasPendingTransfer(ctx context.Context, instanceID int32) (bool, error) {
	var count int64
	err := DB.WithContext(ctx).
		Table("instance_transfers").
		Where("instance_id = ? AND status::text IN ('pending', 'processing')", instanceID).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ==================== snapshots ====================

// SnapshotsSnapshot mirrors the Snapshot record db/snapshots.ts returns. Note the
// TypeScript normalizes `stateful` to `s.stateful ? 1 : 0`; the route turns it
// back into a boolean with Boolean(...), so the bool is kept here.
type SnapshotsSnapshot struct {
	ID          int32
	InstanceID  int32
	IncusName   string
	Name        string
	Description *string
	Stateful    bool
	Size        int32
	CreatedAt   time.Time
}

const snapshotsColumns = "id, instance_id, incus_name, name, description, stateful, size, created_at"

// SnapshotsListByInstanceID mirrors getSnapshotsByInstanceId.
func SnapshotsListByInstanceID(ctx context.Context, instanceID int32) ([]SnapshotsSnapshot, error) {
	var rows []struct {
		ID          int32     `gorm:"column:id"`
		InstanceID  int32     `gorm:"column:instance_id"`
		IncusName   string    `gorm:"column:incus_name"`
		Name        string    `gorm:"column:name"`
		Description *string   `gorm:"column:description"`
		Stateful    bool      `gorm:"column:stateful"`
		Size        int32     `gorm:"column:size"`
		CreatedAt   time.Time `gorm:"column:created_at"`
	}
	err := DB.WithContext(ctx).
		Table("snapshots").
		Select(snapshotsColumns).
		Where("instance_id = ?", instanceID).
		Order("created_at DESC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}

	snapshots := make([]SnapshotsSnapshot, 0, len(rows))
	for _, row := range rows {
		snapshots = append(snapshots, SnapshotsSnapshot{
			ID:          row.ID,
			InstanceID:  row.InstanceID,
			IncusName:   row.IncusName,
			Name:        row.Name,
			Description: row.Description,
			Stateful:    row.Stateful,
			Size:        row.Size,
			CreatedAt:   row.CreatedAt,
		})
	}
	return snapshots, nil
}

// SnapshotsGetByID mirrors getSnapshotById.
func SnapshotsGetByID(ctx context.Context, id int32) (*SnapshotsSnapshot, error) {
	var rows []struct {
		ID          int32     `gorm:"column:id"`
		InstanceID  int32     `gorm:"column:instance_id"`
		IncusName   string    `gorm:"column:incus_name"`
		Name        string    `gorm:"column:name"`
		Description *string   `gorm:"column:description"`
		Stateful    bool      `gorm:"column:stateful"`
		Size        int32     `gorm:"column:size"`
		CreatedAt   time.Time `gorm:"column:created_at"`
	}
	err := DB.WithContext(ctx).
		Table("snapshots").
		Select(snapshotsColumns).
		Where("id = ?", id).
		Limit(1).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	return &SnapshotsSnapshot{
		ID:          row.ID,
		InstanceID:  row.InstanceID,
		IncusName:   row.IncusName,
		Name:        row.Name,
		Description: row.Description,
		Stateful:    row.Stateful,
		Size:        row.Size,
		CreatedAt:   row.CreatedAt,
	}, nil
}

// SnapshotsCreateData mirrors the createSnapshot argument object. Description is
// `description ?? null` and Stateful is `stateful ?? false` in the TypeScript, so
// nil means SQL NULL / false.
type SnapshotsCreateData struct {
	InstanceID  int32
	IncusName   string
	Name        string
	Description *string
	Stateful    bool
}

// SnapshotsCreate mirrors createSnapshot and returns the new row id.
//
// created_at is left to the column default (Prisma @default(now())), which is why
// this is an explicit INSERT rather than a GORM Create (GORM would substitute a
// client-side clock for the identically named field).
func SnapshotsCreate(ctx context.Context, data SnapshotsCreateData) (int32, error) {
	var id int32
	row := DB.WithContext(ctx).Raw(
		`INSERT INTO snapshots (instance_id, incus_name, name, description, stateful)
		 VALUES (?, ?, ?, ?, ?)
		 RETURNING id`,
		data.InstanceID, data.IncusName, data.Name, data.Description, data.Stateful,
	).Row()
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// SnapshotsUpdateSize mirrors updateSnapshotSize.
func SnapshotsUpdateSize(ctx context.Context, id int32, size int32) error {
	result := DB.WithContext(ctx).
		Table("snapshots").
		Where("id = ?", id).
		Update("size", size)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New(snapshotsUpdateMissingMessage)
	}
	return nil
}

// SnapshotsDelete mirrors deleteSnapshot. A missing row is an error, because
// Prisma's `delete` throws P2025 (the route turns it into a 500).
func SnapshotsDelete(ctx context.Context, id int32) error {
	// An explicit DELETE keeps the affected-row count (Prisma's P2025 trigger)
	// unambiguous; the table has no updated_at column.
	result := DB.WithContext(ctx).Exec("DELETE FROM snapshots WHERE id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New(snapshotsDeleteMissingMessage)
	}
	return nil
}

// ==================== snapshot policies ====================

// SnapshotsPolicy mirrors a SnapshotPolicy row.
type SnapshotsPolicy struct {
	ID              int32
	InstanceID      int32
	Enabled         bool
	IntervalMinutes int32
	LastRunAt       *time.Time
	NextRunAt       *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// SnapshotsGetPolicy mirrors getSnapshotPolicy (`findUnique({ where: { instanceId } })`).
func SnapshotsGetPolicy(ctx context.Context, instanceID int32) (*SnapshotsPolicy, error) {
	var rows []struct {
		ID              int32      `gorm:"column:id"`
		InstanceID      int32      `gorm:"column:instance_id"`
		Enabled         bool       `gorm:"column:enabled"`
		IntervalMinutes int32      `gorm:"column:interval_minutes"`
		LastRunAt       *time.Time `gorm:"column:last_run_at"`
		NextRunAt       *time.Time `gorm:"column:next_run_at"`
		CreatedAt       time.Time  `gorm:"column:created_at"`
		UpdatedAt       time.Time  `gorm:"column:updated_at"`
	}
	err := DB.WithContext(ctx).
		Table("snapshot_policies").
		Select("id, instance_id, enabled, interval_minutes, last_run_at, next_run_at, created_at, updated_at").
		Where("instance_id = ?", instanceID).
		Limit(1).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	return &SnapshotsPolicy{
		ID:              row.ID,
		InstanceID:      row.InstanceID,
		Enabled:         row.Enabled,
		IntervalMinutes: row.IntervalMinutes,
		LastRunAt:       row.LastRunAt,
		NextRunAt:       row.NextRunAt,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}, nil
}

// snapshotsAddMinutesLocal reproduces `const d = new Date(); d.setMinutes(d.getMinutes() + n)`.
//
// JavaScript's setMinutes works on *local wall-clock* fields and lets the date
// roll over, so this uses time.Date with the local location instead of an
// absolute Add. The two differ only when the local zone has a DST transition
// inside the interval; wall-clock arithmetic is the faithful choice.
func snapshotsAddMinutesLocal(from time.Time, minutes int32) time.Time {
	local := from.In(time.Local)
	return time.Date(
		local.Year(), local.Month(), local.Day(),
		local.Hour(), local.Minute()+int(minutes), local.Second(), local.Nanosecond(),
		time.Local,
	)
}

// SnapshotsUpsertPolicy mirrors upsertSnapshotPolicy.
//
// Faithful quirk: the create branch writes nextRunAt = now + intervalMinutes while
// the update branch does NOT touch next_run_at at all (the TypeScript update
// payload only carries enabled/intervalMinutes). Existing policies therefore keep
// their old next_run_at until the scheduler recomputes it.
func SnapshotsUpsertPolicy(ctx context.Context, instanceID int32, enabled bool, intervalMinutes int32) error {
	now := time.Now().UTC()
	nextRunAt := snapshotsAddMinutesLocal(now, intervalMinutes)
	// Prisma stores DateTime as UTC wall-clock in a `timestamp(3)` column, and pgx
	// discards the zone (it writes the time's own wall-clock fields). Converting
	// first keeps the stored instants identical to the Node backend's.
	now = now.UTC()
	nextRunAt = nextRunAt.UTC()

	return DB.WithContext(ctx).Exec(
		`INSERT INTO snapshot_policies (instance_id, enabled, interval_minutes, next_run_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (instance_id) DO UPDATE
		 SET enabled = EXCLUDED.enabled,
		     interval_minutes = EXCLUDED.interval_minutes,
		     updated_at = EXCLUDED.updated_at`,
		instanceID, enabled, intervalMinutes, nextRunAt, now, now,
	).Error
}

// SnapshotsUpdatePolicyLastRun mirrors updateSnapshotPolicyLastRun.
func SnapshotsUpdatePolicyLastRun(ctx context.Context, instanceID int32) error {
	policy, err := SnapshotsGetPolicy(ctx, instanceID)
	if err != nil {
		return err
	}
	if policy == nil {
		return nil
	}

	now := time.Now().UTC()
	nextRunAt := snapshotsAddMinutesLocal(now, policy.IntervalMinutes)
	// Same UTC normalization as SnapshotsUpsertPolicy.
	now = now.UTC()
	nextRunAt = nextRunAt.UTC()

	return DB.WithContext(ctx).
		Table("snapshot_policies").
		Where("instance_id = ?", instanceID).
		Updates(map[string]any{"last_run_at": now, "next_run_at": nextRunAt}).Error
}

// SnapshotsListPendingPolicies mirrors getPendingSnapshotPolicies.
func SnapshotsListPendingPolicies(ctx context.Context) ([]SnapshotsPolicy, error) {
	now := time.Now().UTC()
	var rows []struct {
		ID              int32      `gorm:"column:id"`
		InstanceID      int32      `gorm:"column:instance_id"`
		Enabled         bool       `gorm:"column:enabled"`
		IntervalMinutes int32      `gorm:"column:interval_minutes"`
		LastRunAt       *time.Time `gorm:"column:last_run_at"`
		NextRunAt       *time.Time `gorm:"column:next_run_at"`
		CreatedAt       time.Time  `gorm:"column:created_at"`
		UpdatedAt       time.Time  `gorm:"column:updated_at"`
	}
	err := DB.WithContext(ctx).
		Table("snapshot_policies").
		Select("id, instance_id, enabled, interval_minutes, last_run_at, next_run_at, created_at, updated_at").
		Where("enabled = ?", true).
		Where("next_run_at IS NULL OR next_run_at <= ?", now).
		Order("next_run_at ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}

	policies := make([]SnapshotsPolicy, 0, len(rows))
	for _, row := range rows {
		policies = append(policies, SnapshotsPolicy{
			ID:              row.ID,
			InstanceID:      row.InstanceID,
			Enabled:         row.Enabled,
			IntervalMinutes: row.IntervalMinutes,
			LastRunAt:       row.LastRunAt,
			NextRunAt:       row.NextRunAt,
			CreatedAt:       row.CreatedAt,
			UpdatedAt:       row.UpdatedAt,
		})
	}
	return policies, nil
}

// ==================== quota ====================

// SnapshotsQuotaResult mirrors checkSnapshotQuota's return value. Message is the
// empty string where the TypeScript leaves it undefined (allowed results).
type SnapshotsQuotaResult struct {
	Allowed bool
	Current int32
	Limit   int32
	Message string
}

// snapshotsQuotaDenialMessage mirrors the two denial messages of
// checkSnapshotQuota (the allowed branch leaves `message` undefined).
func snapshotsQuotaDenialMessage(current, limit int32) string {
	if limit == 0 {
		return "Snapshot quota not allocated for this instance"
	}
	return fmt.Sprintf("Instance snapshot quota full (%d/%d)", current, limit)
}

// SnapshotsCheckSnapshotQuota mirrors checkSnapshotQuota(userId, instanceId).
// The userId argument is ignored by the TypeScript (`_userId`), so it is absent
// here: the quota is per instance, not per user.
//
// A NULL snapshot_limit behaves like 0 and yields the "not allocated" denial.
func SnapshotsCheckSnapshotQuota(ctx context.Context, instanceID int32) (SnapshotsQuotaResult, error) {
	var rows []struct {
		SnapshotLimit *int32 `gorm:"column:snapshot_limit"`
		Current       int32  `gorm:"column:snapshot_count"`
	}
	err := DB.WithContext(ctx).
		Raw(`SELECT i.snapshot_limit,
		            (SELECT COUNT(*) FROM snapshots s WHERE s.instance_id = i.id) AS snapshot_count
		     FROM instances i
		     WHERE i.id = ?
		     LIMIT 1`, instanceID).
		Scan(&rows).Error
	if err != nil {
		return SnapshotsQuotaResult{}, err
	}
	if len(rows) == 0 {
		return SnapshotsQuotaResult{Message: "Instance not found"}, nil
	}

	limit := int32(0)
	if rows[0].SnapshotLimit != nil {
		limit = *rows[0].SnapshotLimit
	}
	current := rows[0].Current

	if limit == 0 {
		return SnapshotsQuotaResult{
			Current: current,
			Limit:   0,
			Message: snapshotsQuotaDenialMessage(current, limit),
		}, nil
	}
	if current >= limit {
		return SnapshotsQuotaResult{
			Current: current,
			Limit:   limit,
			Message: snapshotsQuotaDenialMessage(current, limit),
		}, nil
	}
	return SnapshotsQuotaResult{Allowed: true, Current: current, Limit: limit}, nil
}
