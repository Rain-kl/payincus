// Port of server/src/db/quota-operations.ts.
//
// Two different kinds of quota live here:
//
//   - User-level counters (hosts / friends / packages) are *stored* on
//     user_quotas and updated with atomic increments. decrementUserQuotaUsed does
//     NOT clamp at zero: that is what the TypeScript actually does (its file
//     header claims Math.max(0, ...) but the body uses a plain Prisma
//     `{decrement: 1}`), so the comment and the code disagree upstream. The code is
//     authoritative here; clamping would silently change balances.
//   - Instance-level quotas (ports / snapshots / backups) are *derived*: the usage
//     is the row count, so no counter can drift. That is why the backup create
//     route re-counts inside the transaction that inserts the row
//     (db/backups.go createBackupWithQuotaReservation).
//
// reserveResources / rollbackResources are the host-capacity half: they run under
// SERIALIZABLE with the host row locked FOR UPDATE and use conditional atomic
// updates, because a read-then-write would lose concurrent reservations.
//
// Time: every time.Time bound into SQL is UTC (WAVE_D_SPEC §0.2b).
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// QuotaTransactionTimeoutMS mirrors QUOTA_TRANSACTION_OPTIONS.timeout (10s).
const QuotaTransactionTimeoutMS = 10000

var errQuotaDatabaseNotInitialized = errors.New("database not initialized")

func quotaRequireDB() error {
	if DB == nil {
		return errQuotaDatabaseNotInitialized
	}
	return nil
}

// quotaUpdateMissing is Prisma's P2025 text for `userQuota.update` on a user with
// no quota row.
const quotaUpdateMissing = "An operation failed because it depends on one or more records " +
	"that were required but not found. Record to update not found."

// QuotaCheckResult mirrors the `{allowed, current, limit, message?}` object every
// check*Quota function returns.
type QuotaCheckResult struct {
	Allowed bool
	Current int32
	Limit   int32
	Message string
}

// IncrementUserQuotaUsed mirrors incrementUserQuotaUsed (atomic increment).
func IncrementUserQuotaUsed(ctx context.Context, userID int32, data UserQuotaUsedUpdate) error {
	if err := quotaRequireDB(); err != nil {
		return err
	}
	if data.HostUsed == nil && data.FriendUsed == nil && data.PackageUsed == nil {
		// `if (Object.keys(updateData).length === 0) return`
		return nil
	}

	assignments := ""
	args := []any{}
	add := func(column string, value *int32) {
		if value == nil {
			return
		}
		if assignments != "" {
			assignments += ", "
		}
		assignments += column + " = " + column + " + $" + strconv.Itoa(len(args)+1)
		args = append(args, *value)
	}
	add("host_used", data.HostUsed)
	add("friend_used", data.FriendUsed)
	add("package_used", data.PackageUsed)
	args = append(args, userID)

	tag, err := Pool().Exec(ctx, "UPDATE user_quotas SET "+assignments+" WHERE user_id = $"+strconv.Itoa(len(args)),
		args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New(quotaUpdateMissing)
	}
	return nil
}

// DecrementUserQuotaUsed mirrors decrementUserQuotaUsed. The switch has no default
// in the TypeScript, so an unknown type produces an empty `data` object; that is a
// no-op here (Prisma's generated SQL for an empty update is a no-op too).
func DecrementUserQuotaUsed(ctx context.Context, userID int32, kind string) error {
	if err := quotaRequireDB(); err != nil {
		return err
	}

	column := ""
	switch kind {
	case "host":
		column = "host_used"
	case "friend":
		column = "friend_used"
	case "package":
		column = "package_used"
	default:
		return nil
	}

	tag, err := Pool().Exec(ctx, "UPDATE user_quotas SET "+column+" = "+column+" - 1 WHERE user_id = $1", userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New(quotaUpdateMissing)
	}
	return nil
}

// ReserveResourcesOptions mirrors reserveResources' argument object.
type ReserveResourcesOptions struct {
	HostID               int32
	CPU                  int32
	Memory               int32
	Disk                 int32
	PortCount            int32
	StoragePoolName      *string
	DiskAlreadyAllocated bool
}

// ReserveResources mirrors reserveResources.
//
// Order is load-bearing: lock the host row, prove there is storage headroom (the
// fail-closed provider — `storage.reason` aborts the reservation), re-read the
// limits, then apply a conditional atomic update. The condition in the UPDATE is
// what makes concurrency safe even if a concurrent writer slipped in between the
// read and the write.
func ReserveResources(ctx context.Context, options ReserveResourcesOptions) error {
	// The TypeScript validates the amounts before opening the transaction.
	if options.CPU < 0 || options.Memory < 0 || options.Disk < 0 || options.PortCount < 0 {
		return errors.New("Resource amounts must be non-negative")
	}
	if err := quotaRequireDB(); err != nil {
		return err
	}

	txCtx, cancel := context.WithTimeout(ctx, QuotaTransactionTimeoutMS*time.Millisecond)
	defer cancel()

	tx := DB.WithContext(txCtx).Begin(&sql.TxOptions{Isolation: sql.LevelSerializable})
	if tx.Error != nil {
		return tx.Error
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.Exec("SELECT id FROM hosts WHERE id = ? FOR UPDATE", options.HostID).Error; err != nil {
		return err
	}

	requiredMB := int64(options.Disk)
	if options.DiskAlreadyAllocated {
		requiredMB = 0
	}
	storage, err := hostsStorageCapacity(txCtx, tx, options.HostID, HostsStorageCapacityOptions{
		PoolName:   options.StoragePoolName,
		RequiredMB: &requiredMB,
	})
	if err != nil {
		return err
	}
	if storage.Reason != nil {
		return fmt.Errorf("HOST_RESOURCES_INSUFFICIENT: %s", *storage.Reason)
	}

	var host struct {
		CPUAllowanceMax *int32 `gorm:"column:cpu_allowance_max"`
		MemoryMax       *int32 `gorm:"column:memory_max"`
		NATPortStart    *int32 `gorm:"column:nat_port_start"`
		NATPortEnd      *int32 `gorm:"column:nat_port_end"`
	}
	result := tx.Raw(`SELECT cpu_allowance_max, memory_max, nat_port_start, nat_port_end
		FROM hosts WHERE id = ?`, options.HostID).Scan(&host)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("Host not found")
	}

	cpuLimit := int32(0)
	if host.CPUAllowanceMax != nil {
		cpuLimit = *host.CPUAllowanceMax
	}
	memoryLimit := int32(0)
	if host.MemoryMax != nil {
		memoryLimit = *host.MemoryMax
	}

	if cpuLimit > 0 && options.CPU > cpuLimit {
		return fmt.Errorf("Host CPU exceeded: requested %d/%d", options.CPU, cpuLimit)
	}
	if memoryLimit > 0 && options.Memory > memoryLimit {
		return fmt.Errorf("Host memory exceeded: requested %d/%d", options.Memory, memoryLimit)
	}

	// `(host.natPortStart && host.natPortEnd) ? end - start + 1 : 0`: JavaScript
	// truthiness treats 0 as absent, hence the explicit non-zero checks.
	natPortsTotal := int32(0)
	if host.NATPortStart != nil && *host.NATPortStart != 0 && host.NATPortEnd != nil && *host.NATPortEnd != 0 {
		natPortsTotal = *host.NATPortEnd - *host.NATPortStart + 1
	}
	if options.PortCount > 0 && options.PortCount > natPortsTotal {
		return fmt.Errorf("Host ports exceeded: requested %d/%d", options.PortCount, natPortsTotal)
	}

	where := "id = ?"
	args := []any{options.CPU, options.Memory, options.Disk}
	assignments := "cpu_used = cpu_used + ?, memory_used = memory_used + ?, disk_used = disk_used + ?"
	if options.PortCount > 0 {
		assignments += ", nat_ports_used_count = nat_ports_used_count + ?"
		args = append(args, options.PortCount)
	}
	args = append(args, options.HostID)
	if cpuLimit > 0 {
		where += " AND cpu_used <= ?"
		args = append(args, cpuLimit-options.CPU)
	}
	if memoryLimit > 0 {
		where += " AND memory_used <= ?"
		args = append(args, memoryLimit-options.Memory)
	}
	if options.PortCount > 0 {
		where += " AND nat_ports_used_count <= ?"
		args = append(args, natPortsTotal-options.PortCount)
	}

	result = tx.Exec("UPDATE hosts SET "+assignments+" WHERE "+where, args...)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("Host resources exceeded or host was updated concurrently")
	}

	return tx.Commit().Error
}

// RollbackResourcesOptions mirrors rollbackResources' argument object.
type RollbackResourcesOptions struct {
	HostID    int32
	CPU       int32
	Memory    int32
	Disk      int32
	PortCount int32
}

// RollbackResources mirrors rollbackResources: one atomic UPDATE with GREATEST(...,
// 0) so concurrent releases cannot drive a counter negative.
func RollbackResources(ctx context.Context, options RollbackResourcesOptions) error {
	// The TypeScript validates the amounts before opening the transaction.
	if options.CPU < 0 || options.Memory < 0 || options.Disk < 0 || options.PortCount < 0 {
		return errors.New("Resource amounts must be non-negative")
	}
	if err := quotaRequireDB(); err != nil {
		return err
	}

	txCtx, cancel := context.WithTimeout(ctx, QuotaTransactionTimeoutMS*time.Millisecond)
	defer cancel()

	tx := DB.WithContext(txCtx).Begin(&sql.TxOptions{Isolation: sql.LevelSerializable})
	if tx.Error != nil {
		return tx.Error
	}
	defer func() { _ = tx.Rollback() }()

	if options.PortCount > 0 {
		err := tx.Exec(`UPDATE hosts SET
			cpu_used = GREATEST(cpu_used - ?, 0),
			memory_used = GREATEST(memory_used - ?, 0),
			disk_used = GREATEST(disk_used - ?, 0),
			nat_ports_used_count = GREATEST(nat_ports_used_count - ?, 0)
			WHERE id = ?`,
			options.CPU, options.Memory, options.Disk, options.PortCount, options.HostID).Error
		if err != nil {
			return err
		}
	} else {
		err := tx.Exec(`UPDATE hosts SET
			cpu_used = GREATEST(cpu_used - ?, 0),
			memory_used = GREATEST(memory_used - ?, 0),
			disk_used = GREATEST(disk_used - ?, 0)
			WHERE id = ?`,
			options.CPU, options.Memory, options.Disk, options.HostID).Error
		if err != nil {
			return err
		}
	}

	return tx.Commit().Error
}

// quotaUserCounters reads one user_quotas row's two counters.
func quotaUserCounters(ctx context.Context, userID int32, limitColumn, usedColumn string) (limit, used int32, found bool, err error) {
	err = Pool().QueryRow(ctx,
		"SELECT "+limitColumn+", "+usedColumn+" FROM user_quotas WHERE user_id = $1", userID).
		Scan(&limit, &used)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, false, nil
		}
		return 0, 0, false, err
	}
	return limit, used, true, nil
}

// CheckHostQuota mirrors checkHostQuota.
func CheckHostQuota(ctx context.Context, userID int32) (QuotaCheckResult, error) {
	if err := quotaRequireDB(); err != nil {
		return QuotaCheckResult{}, err
	}
	limit, used, found, err := quotaUserCounters(ctx, userID, "host_limit", "host_used")
	if err != nil {
		return QuotaCheckResult{}, err
	}
	if !found {
		return QuotaCheckResult{Message: "User quota not found"}, nil
	}
	return quotaCounterCheck(used, limit, "Host"), nil
}

// CheckFriendQuota mirrors checkFriendQuota.
func CheckFriendQuota(ctx context.Context, userID int32) (QuotaCheckResult, error) {
	if err := quotaRequireDB(); err != nil {
		return QuotaCheckResult{}, err
	}
	limit, used, found, err := quotaUserCounters(ctx, userID, "friend_limit", "friend_used")
	if err != nil {
		return QuotaCheckResult{}, err
	}
	if !found {
		return QuotaCheckResult{Message: "User quota not found"}, nil
	}
	return quotaCounterCheck(used, limit, "Friend"), nil
}

// CheckPackageQuota mirrors checkPackageQuota.
func CheckPackageQuota(ctx context.Context, userID int32) (QuotaCheckResult, error) {
	if err := quotaRequireDB(); err != nil {
		return QuotaCheckResult{}, err
	}
	limit, used, found, err := quotaUserCounters(ctx, userID, "package_limit", "package_used")
	if err != nil {
		return QuotaCheckResult{}, err
	}
	if !found {
		return QuotaCheckResult{Message: "User quota not found"}, nil
	}
	return quotaCounterCheck(used, limit, "Package"), nil
}

// quotaCounterCheck is the shared body of the three user-level checks: a zero
// limit means the feature is not authorized at all, and used >= limit is full.
func quotaCounterCheck(used, limit int32, feature string) QuotaCheckResult {
	if limit == 0 {
		return QuotaCheckResult{
			Current: used,
			Limit:   0,
			Message: feature + " feature is not authorized. Please contact administrator.",
		}
	}
	if used >= limit {
		return QuotaCheckResult{
			Current: used,
			Limit:   limit,
			Message: fmt.Sprintf("%s quota full (%d/%d)", feature, used, limit),
		}
	}
	return QuotaCheckResult{Allowed: true, Current: used, Limit: limit}
}

// CheckPortQuota mirrors checkPortQuota: system-managed mappings (the automatic
// 22/3389 forwarding) do not consume a user slot.
func CheckPortQuota(ctx context.Context, instanceID int32) (QuotaCheckResult, error) {
	if err := quotaRequireDB(); err != nil {
		return QuotaCheckResult{}, err
	}

	var limit *int32
	var current int32
	err := Pool().QueryRow(ctx, `SELECT i.port_limit,
		(SELECT count(*)::int FROM port_mappings p
		  WHERE p.instance_id = i.id AND p.system_managed = false)
		FROM instances i WHERE i.id = $1`, instanceID).Scan(&limit, &current)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return QuotaCheckResult{Message: "Instance not found"}, nil
		}
		return QuotaCheckResult{}, err
	}

	effectiveLimit := int32(0)
	if limit != nil {
		effectiveLimit = *limit
	}
	if effectiveLimit == 0 {
		return QuotaCheckResult{
			Current: current,
			Limit:   0,
			Message: "Port quota not allocated for this instance",
		}, nil
	}
	if current >= effectiveLimit {
		return QuotaCheckResult{
			Current: current,
			Limit:   effectiveLimit,
			Message: fmt.Sprintf("Instance port quota full (%d/%d)", current, effectiveLimit),
		}, nil
	}
	return QuotaCheckResult{Allowed: true, Current: current, Limit: effectiveLimit}, nil
}

// CheckSnapshotQuota mirrors checkSnapshotQuota. NOTE: routes/snapshots.go has its
// own SnapshotsCheckSnapshotQuota because it was ported before this module; the
// semantics are identical and both count snapshot rows.
func CheckSnapshotQuota(ctx context.Context, instanceID int32) (QuotaCheckResult, error) {
	if err := quotaRequireDB(); err != nil {
		return QuotaCheckResult{}, err
	}

	var limit *int32
	var current int32
	err := Pool().QueryRow(ctx, `SELECT i.snapshot_limit,
		(SELECT count(*)::int FROM snapshots s WHERE s.instance_id = i.id)
		FROM instances i WHERE i.id = $1`, instanceID).Scan(&limit, &current)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return QuotaCheckResult{Message: "Instance not found"}, nil
		}
		return QuotaCheckResult{}, err
	}

	effectiveLimit := int32(0)
	if limit != nil {
		effectiveLimit = *limit
	}
	if effectiveLimit == 0 {
		return QuotaCheckResult{
			Current: current,
			Limit:   0,
			Message: "Snapshot quota not allocated for this instance",
		}, nil
	}
	if current >= effectiveLimit {
		return QuotaCheckResult{
			Current: current,
			Limit:   effectiveLimit,
			Message: fmt.Sprintf("Instance snapshot quota full (%d/%d)", current, effectiveLimit),
		}, nil
	}
	return QuotaCheckResult{Allowed: true, Current: current, Limit: effectiveLimit}, nil
}

// CheckBackupQuota mirrors checkBackupQuota: only rows in creating/ready consume a
// slot, so error/deleted backups give the slot back.
func CheckBackupQuota(ctx context.Context, instanceID int32) (QuotaCheckResult, error) {
	if err := quotaRequireDB(); err != nil {
		return QuotaCheckResult{}, err
	}

	var limit *int32
	var current int32
	err := Pool().QueryRow(ctx, `SELECT i.backup_limit,
		(SELECT count(*)::int FROM backups b
		  WHERE b.instance_id = i.id AND b.status IN ('creating', 'ready'))
		FROM instances i WHERE i.id = $1`, instanceID).Scan(&limit, &current)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return QuotaCheckResult{Message: "Instance not found"}, nil
		}
		return QuotaCheckResult{}, err
	}

	effectiveLimit := int32(0)
	if limit != nil {
		effectiveLimit = *limit
	}
	if effectiveLimit == 0 {
		return QuotaCheckResult{
			Current: current,
			Limit:   0,
			Message: "Backup quota not allocated for this instance",
		}, nil
	}
	if current >= effectiveLimit {
		return QuotaCheckResult{
			Current: current,
			Limit:   effectiveLimit,
			Message: fmt.Sprintf("Instance backup quota full (%d/%d)", current, effectiveLimit),
		}, nil
	}
	return QuotaCheckResult{Allowed: true, Current: current, Limit: effectiveLimit}, nil
}

// SyncUserQuotaUsed mirrors syncUserQuotaUsed (recalculate then overwrite).
func SyncUserQuotaUsed(ctx context.Context, userID int32) error {
	used, err := CalculateUserQuotaUsed(ctx, userID)
	if err != nil {
		return err
	}
	return UpdateUserQuotaUsed(ctx, userID, used)
}
