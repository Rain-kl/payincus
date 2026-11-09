// Port of the db surface routes/batch-config.ts needs.
//
// batch-config.ts reaches into three unported db modules:
//
//   - db/instances.ts     updateInstanceResources / updateInstanceQuota /
//     updateInstanceSwap / updateInstanceConfig (+ the
//     projection of the findMany that includes package and
//     packagePlan)
//   - db/hosts.ts         incrementHostResourceUsage
//   - db/instance-tasks.ts getActiveTaskForInstance
//
// db/instances.go and db/hosts.go belong to the high-risk instance/host wave, so
// — following the db/permission_lookups.go precedent — this file contains narrow,
// prefixed statements instead of inventing those modules. Every update helper
// mirrors the TypeScript's "only touch provided keys" behaviour: Prisma's
// `data: {...}` object omits `undefined` fields, so an absent field must never
// reach the SET clause (otherwise it would clear a column the caller did not
// mention).
//
// The empty-update early return from the TypeScript
// (`if (Object.keys(updateData).length === 0) return`) is reproduced explicitly:
// writing an empty map would still bump updated_at and turn a no-op into a row
// change. The map builders below are pure so the tri-state mapping is unit
// testable without a database.
package db

import (
	"context"
	"errors"
	"time"
)

// batchConfigUpdateMissingMessage mirrors Prisma's P2025 text for a missing row.
// routes/batch-config.ts returns `err.message` verbatim in the per-instance
// failure item, so the wording is part of the API contract.
const batchConfigUpdateMissingMessage = "An operation failed because it depends on one or more records " +
	"that were required but not found. Record to update not found."

// ==================== read projections ====================

// BatchConfigInstance mirrors the instance shape updateSingleInstance receives,
// including the two includes the TypeScript query requests:
// `package: { select: { instanceType } }` and
// `packagePlan: { select: { swapSize } }`.
type BatchConfigInstance struct {
	ID          int32
	IncusID     string
	Name        string
	CPU         int32
	Memory      int32
	Disk        int32
	HostID      int32
	SwapEnabled bool
	SwapSize    *int32
	// PackageInstanceType is nil when the instance has no package (the include is
	// null), matching `instance.package?.instanceType`.
	PackageInstanceType *string
	// PackagePlanSwapSize is nil when the instance has no plan.
	PackagePlanSwapSize *int32
}

// BatchConfigListInstancesForHost mirrors the prisma.instance.findMany call:
// ids in the list, belonging to the host, excluding rows that are being created or
// already deleted.
func BatchConfigListInstancesForHost(ctx context.Context, hostID int32, instanceIDs []int32) ([]BatchConfigInstance, error) {
	var rows []struct {
		ID                  int32   `gorm:"column:id"`
		IncusID             string  `gorm:"column:incus_id"`
		Name                string  `gorm:"column:name"`
		CPU                 int32   `gorm:"column:cpu"`
		Memory              int32   `gorm:"column:memory"`
		Disk                int32   `gorm:"column:disk"`
		HostID              int32   `gorm:"column:host_id"`
		SwapEnabled         bool    `gorm:"column:swap_enabled"`
		SwapSize            *int32  `gorm:"column:swap_size"`
		PackageInstanceType *string `gorm:"column:package_instance_type"`
		PackagePlanSwapSize *int32  `gorm:"column:package_plan_swap_size"`
	}

	err := DB.WithContext(ctx).
		Table("instances AS i").
		Select(`i.id, i.incus_id, i.name, i.cpu, i.memory, i.disk, i.host_id,
		        i.swap_enabled, i.swap_size,
		        p.instance_type AS package_instance_type,
		        pp.swap_size AS package_plan_swap_size`).
		Joins("LEFT JOIN packages p ON p.id = i.package_id").
		Joins("LEFT JOIN package_plans pp ON pp.id = i.package_plan_id").
		Where("i.id IN ?", instanceIDs).
		Where("i.host_id = ?", hostID).
		Where("i.status::text NOT IN ('deleted', 'creating')").
		Order("i.id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}

	instances := make([]BatchConfigInstance, 0, len(rows))
	for _, row := range rows {
		instances = append(instances, BatchConfigInstance{
			ID:                  row.ID,
			IncusID:             row.IncusID,
			Name:                row.Name,
			CPU:                 row.CPU,
			Memory:              row.Memory,
			Disk:                row.Disk,
			HostID:              row.HostID,
			SwapEnabled:         row.SwapEnabled,
			SwapSize:            row.SwapSize,
			PackageInstanceType: row.PackageInstanceType,
			PackagePlanSwapSize: row.PackagePlanSwapSize,
		})
	}
	return instances, nil
}

// BatchConfigActiveTask mirrors the only field of InstanceTask the route reads.
type BatchConfigActiveTask struct {
	ID       int32
	TaskType string
}

// BatchConfigGetActiveTaskForInstance mirrors getActiveTaskForInstance: the newest
// PENDING/PROCESSING task for an instance, if any.
func BatchConfigGetActiveTaskForInstance(ctx context.Context, instanceID int32) (*BatchConfigActiveTask, error) {
	var rows []struct {
		ID       int32  `gorm:"column:id"`
		TaskType string `gorm:"column:task_type"`
	}
	err := DB.WithContext(ctx).
		Table("instance_tasks").
		Select("id, task_type").
		Where("instance_id = ? AND status::text IN ('PENDING', 'PROCESSING')", instanceID).
		Order("created_at DESC").
		Limit(1).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &BatchConfigActiveTask{ID: rows[0].ID, TaskType: rows[0].TaskType}, nil
}

// ==================== writes ====================

// batchConfigApplyInstanceUpdate runs one SET clause and maps "row vanished" to
// Prisma's P2025 text. updated_at is always bumped because every touched column
// belongs to a Prisma model with @updatedAt.
func batchConfigApplyInstanceUpdate(ctx context.Context, instanceID int32, updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}
	updates["updated_at"] = time.Now().UTC()

	result := DB.WithContext(ctx).
		Table("instances").
		Where("id = ?", instanceID).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New(batchConfigUpdateMissingMessage)
	}
	return nil
}

// BatchConfigInstanceResourcesUpdate mirrors updateInstanceResources.
type BatchConfigInstanceResourcesUpdate struct {
	CPUProvided    bool
	CPU            int32
	MemoryProvided bool
	Memory         int32
	DiskProvided   bool
	Disk           int32
	// MonthlyTrafficLimitProvided with a nil value is an explicit NULL.
	MonthlyTrafficLimitProvided bool
	MonthlyTrafficLimit         *uint64
}

// batchConfigInstanceResourceUpdates is the pure `updateData` builder of
// updateInstanceResources.
func batchConfigInstanceResourceUpdates(data BatchConfigInstanceResourcesUpdate) map[string]any {
	updates := map[string]any{}
	if data.CPUProvided {
		updates["cpu"] = data.CPU
	}
	if data.MemoryProvided {
		updates["memory"] = data.Memory
	}
	if data.DiskProvided {
		updates["disk"] = data.Disk
	}
	if data.MonthlyTrafficLimitProvided {
		if data.MonthlyTrafficLimit == nil {
			updates["monthly_traffic_limit"] = nil
		} else {
			// Postgres bigint; the parser caps the value at 2^63-1.
			updates["monthly_traffic_limit"] = int64(*data.MonthlyTrafficLimit)
		}
	}
	return updates
}

// BatchConfigUpdateInstanceResources mirrors updateInstanceResources.
func BatchConfigUpdateInstanceResources(ctx context.Context, instanceID int32, data BatchConfigInstanceResourcesUpdate) error {
	return batchConfigApplyInstanceUpdate(ctx, instanceID, batchConfigInstanceResourceUpdates(data))
}

// BatchConfigInstanceQuotaUpdate mirrors updateInstanceQuota.
type BatchConfigInstanceQuotaUpdate struct {
	PortLimitProvided     bool
	PortLimit             *int32
	SnapshotLimitProvided bool
	SnapshotLimit         *int32
	BackupLimitProvided   bool
	BackupLimit           *int32
	SiteLimitProvided     bool
	SiteLimit             *int32
}

// batchConfigInstanceQuotaUpdates is the pure `updateData` builder of
// updateInstanceQuota.
func batchConfigInstanceQuotaUpdates(data BatchConfigInstanceQuotaUpdate) map[string]any {
	updates := map[string]any{}
	if data.PortLimitProvided {
		updates["port_limit"] = data.PortLimit
	}
	if data.SnapshotLimitProvided {
		updates["snapshot_limit"] = data.SnapshotLimit
	}
	if data.BackupLimitProvided {
		updates["backup_limit"] = data.BackupLimit
	}
	if data.SiteLimitProvided {
		updates["site_limit"] = data.SiteLimit
	}
	return updates
}

// BatchConfigUpdateInstanceQuota mirrors updateInstanceQuota (which delegates to
// updateInstance).
func BatchConfigUpdateInstanceQuota(ctx context.Context, instanceID int32, data BatchConfigInstanceQuotaUpdate) error {
	return batchConfigApplyInstanceUpdate(ctx, instanceID, batchConfigInstanceQuotaUpdates(data))
}

// BatchConfigInstanceSwapUpdate mirrors updateInstanceSwap.
type BatchConfigInstanceSwapUpdate struct {
	SwapEnabledProvided bool
	SwapEnabled         bool
	SwapSizeProvided    bool
	SwapSize            *int32
}

// batchConfigInstanceSwapUpdates is the pure `updateData` builder of
// updateInstanceSwap. SwapSizeProvided with a nil value stores NULL
// (`data.swapSize ?? null`).
func batchConfigInstanceSwapUpdates(data BatchConfigInstanceSwapUpdate) map[string]any {
	updates := map[string]any{}
	if data.SwapEnabledProvided {
		updates["swap_enabled"] = data.SwapEnabled
	}
	if data.SwapSizeProvided {
		updates["swap_size"] = data.SwapSize
	}
	return updates
}

// BatchConfigUpdateInstanceSwap mirrors updateInstanceSwap.
func BatchConfigUpdateInstanceSwap(ctx context.Context, instanceID int32, data BatchConfigInstanceSwapUpdate) error {
	return batchConfigApplyInstanceUpdate(ctx, instanceID, batchConfigInstanceSwapUpdates(data))
}

// BatchConfigInstanceAdvancedConfigUpdate mirrors updateInstanceConfig.
//
// Note the column mapping quirk the TypeScript inherits from the schema:
// limitsIngress / limitsEgress are written to configured_limits_ingress /
// configured_limits_egress (the `limits_ingress` / `limits_egress` columns hold the
// *effective* values and are only written through the effectiveBandwidth
// argument, which this route never passes).
type BatchConfigInstanceAdvancedConfigUpdate struct {
	LimitsReadProvided              bool
	LimitsRead                      *string
	LimitsWriteProvided             bool
	LimitsWrite                     *string
	LimitsReadIopsProvided          bool
	LimitsReadIops                  *int32
	LimitsWriteIopsProvided         bool
	LimitsWriteIops                 *int32
	LimitsIngressProvided           bool
	LimitsIngress                   *string
	LimitsEgressProvided            bool
	LimitsEgress                    *string
	LimitsProcessesProvided         bool
	LimitsProcesses                 *int32
	LimitsCPUPriorityProvided       bool
	LimitsCPUPriority               *int32
	BootAutostartProvided           bool
	BootAutostart                   *bool
	BootAutostartPriorityProvided   bool
	BootAutostartPriority           *int32
	BootAutostartDelayProvided      bool
	BootAutostartDelay              *int32
	BootHostShutdownTimeoutProvided bool
	BootHostShutdownTimeout         *int32
}

// batchConfigInstanceAdvancedConfigUpdates is the pure `updateData` builder of
// updateInstanceConfig (without the effectiveBandwidth argument).
func batchConfigInstanceAdvancedConfigUpdates(data BatchConfigInstanceAdvancedConfigUpdate) map[string]any {
	updates := map[string]any{}
	if data.LimitsReadProvided {
		updates["limits_read"] = data.LimitsRead
	}
	if data.LimitsWriteProvided {
		updates["limits_write"] = data.LimitsWrite
	}
	if data.LimitsReadIopsProvided {
		updates["limits_read_iops"] = data.LimitsReadIops
	}
	if data.LimitsWriteIopsProvided {
		updates["limits_write_iops"] = data.LimitsWriteIops
	}
	if data.LimitsIngressProvided {
		updates["configured_limits_ingress"] = data.LimitsIngress
	}
	if data.LimitsEgressProvided {
		updates["configured_limits_egress"] = data.LimitsEgress
	}
	if data.LimitsProcessesProvided {
		updates["limits_processes"] = data.LimitsProcesses
	}
	if data.LimitsCPUPriorityProvided {
		updates["limits_cpu_priority"] = data.LimitsCPUPriority
	}
	if data.BootAutostartProvided {
		updates["boot_autostart"] = data.BootAutostart
	}
	if data.BootAutostartPriorityProvided {
		updates["boot_autostart_priority"] = data.BootAutostartPriority
	}
	if data.BootAutostartDelayProvided {
		updates["boot_autostart_delay"] = data.BootAutostartDelay
	}
	if data.BootHostShutdownTimeoutProvided {
		updates["boot_host_shutdown_timeout"] = data.BootHostShutdownTimeout
	}
	return updates
}

// BatchConfigUpdateInstanceConfig mirrors updateInstanceConfig without the
// effectiveBandwidth argument (the batch route never passes one).
func BatchConfigUpdateInstanceConfig(ctx context.Context, instanceID int32, data BatchConfigInstanceAdvancedConfigUpdate) error {
	return batchConfigApplyInstanceUpdate(ctx, instanceID, batchConfigInstanceAdvancedConfigUpdates(data))
}

// BatchConfigIncrementHostResourceUsage mirrors incrementHostResourceUsage.
//
// The TypeScript accumulates the per-instance deltas across the whole batch and
// applies them once after every worker finished (the comment claims atomicity; the
// statement below is a single atomic increments update, so concurrent batches
// cannot lose updates). Negative deltas are allowed — a downgrade lowers usage.
func BatchConfigIncrementHostResourceUsage(ctx context.Context, hostID int32, cpuUsed, memoryUsed, diskUsed int64) error {
	if cpuUsed == 0 && memoryUsed == 0 && diskUsed == 0 {
		return nil
	}
	return DB.WithContext(ctx).Exec(
		`UPDATE hosts
		 SET cpu_used = cpu_used + ?::bigint,
		     memory_used = memory_used + ?::bigint,
		     disk_used = disk_used + ?::bigint,
		     updated_at = ?
		 WHERE id = ?`,
		cpuUsed, memoryUsed, diskUsed, time.Now().UTC(), hostID,
	).Error
}
