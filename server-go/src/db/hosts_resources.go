// Host resource helper reads/writes used by the billing scheduler's expiry-delete
// job and by the instance-create timeout sweeper.
//
// The TypeScript functions live in db/hosts.ts, but that file is a shared asset for
// the hosts module; these two small aggregates are added here so the scheduler does
// not have to modify it.
package db

import (
	"context"
	"errors"

	"gorm.io/gorm"
)

// HostResourceSum mirrors calculateHostResourcesFromInstances.
type HostResourceSum struct {
	CPUUsed    int32
	MemoryUsed int32
	DiskUsed   int32
}

// CalculateHostResourcesFromInstances mirrors
// `prisma.instance.aggregate({ where: { hostId, status: { not: 'deleted' } }, _sum:
// { cpu, memory, disk } })`.
//
// COALESCE keeps a host with no instances at 0 instead of NULL.
func CalculateHostResourcesFromInstances(ctx context.Context, hostID int32) (HostResourceSum, error) {
	if DB == nil {
		return HostResourceSum{}, errors.New("database not initialized")
	}
	var sum HostResourceSum
	err := DB.WithContext(ctx).Table("instances").
		Select("COALESCE(SUM(cpu), 0) AS cpu_used, COALESCE(SUM(memory), 0) AS memory_used, COALESCE(SUM(disk), 0) AS disk_used").
		Where("host_id = ? AND status <> ?", hostID, "deleted").
		Scan(&sum).Error
	if err != nil {
		return HostResourceSum{}, err
	}
	return sum, nil
}

// UpdateHostResources mirrors updateHostResources. A nil value means "leave the
// column alone"; the TypeScript's empty-update early return is preserved.
func UpdateHostResources(ctx context.Context, hostID int32, cpuUsed, memoryUsed, diskUsed *int32) error {
	if DB == nil {
		return errors.New("database not initialized")
	}
	updates := map[string]any{}
	if cpuUsed != nil {
		updates["cpu_used"] = *cpuUsed
	}
	if memoryUsed != nil {
		updates["memory_used"] = *memoryUsed
	}
	if diskUsed != nil {
		updates["disk_used"] = *diskUsed
	}
	if len(updates) == 0 {
		return nil
	}
	return DB.WithContext(ctx).Table("hosts").Where("id = ?", hostID).Updates(updates).Error
}

// CountInstancePortMappings mirrors
// `prisma.portMapping.count({ where: { instanceId } })`.
func CountInstancePortMappings(ctx context.Context, instanceID int32) (int64, error) {
	if DB == nil {
		return 0, errors.New("database not initialized")
	}
	var count int64
	err := DB.WithContext(ctx).Table("port_mappings").Where("instance_id = ?", instanceID).Count(&count).Error
	return count, err
}

// CountHostActivePortMappings mirrors the nested count used to recompute
// `nat_ports_used_count`:
//
//	where: { instance: { hostId, status: { not: 'deleted' } } }
func CountHostActivePortMappings(ctx context.Context, hostID int32) (int64, error) {
	if DB == nil {
		return 0, errors.New("database not initialized")
	}
	var count int64
	err := DB.WithContext(ctx).Table("port_mappings AS pm").
		Joins("JOIN instances i ON i.id = pm.instance_id").
		Where("i.host_id = ? AND i.status <> ?", hostID, "deleted").
		Count(&count).Error
	return count, err
}

// UpdateHostNATPortsUsed mirrors
// `prisma.host.update({ where: { id }, data: { natPortsUsedCount } })`.
func UpdateHostNATPortsUsed(ctx context.Context, hostID int32, used int64) error {
	if DB == nil {
		return errors.New("database not initialized")
	}
	return DB.WithContext(ctx).Table("hosts").Where("id = ?", hostID).
		Update("nat_ports_used_count", used).Error
}

// DeleteInstanceNetworkAllocations mirrors the transaction in
// processExpiryDelete:
//
//	tx.portMapping.deleteMany({ where: { instanceId } })
//	tx.ipAddress.deleteMany({ where: { instanceId } })
//	tx.ipv6Subnet.deleteMany({ where: { instanceId } })
//	releasePublicIpv4ForInstance(tx, instanceId)
//
// Divergence: db.ReleasePublicIpv4ForInstance is pgx-typed (it needs the FOR UPDATE
// locking semantics of the public pool), so the release runs in its own
// transaction after the three deletes instead of sharing the GORM one. Both steps
// are idempotent, so a crash in between leaves no leaked address: the release is
// unconditional on `instance_id`.
func DeleteInstanceNetworkAllocations(ctx context.Context, instanceID int32) error {
	if DB == nil {
		return errors.New("database not initialized")
	}
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM port_mappings WHERE instance_id = ?", instanceID).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM ip_addresses WHERE instance_id = ?", instanceID).Error; err != nil {
			return err
		}
		return tx.Exec("DELETE FROM ipv6_subnets WHERE instance_id = ?", instanceID).Error
	})
	if err != nil {
		return err
	}
	if Pool() == nil {
		return errors.New("database not initialized")
	}
	tx, err := Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := ReleasePublicIpv4ForInstance(ctx, tx, instanceID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
