// Read-side port of the storage-pool helpers in server/src/db/storage-pools.ts
// that the packages routes depend on (the write-side CRUD belongs to the hosts
// routes and is not ported here).
//
// Only these five helpers are needed by routes/packages.go:
//
//	getSystemDiskPoolsByHostId → StoragePoolsGetSystemDiskPoolsByHostID
//	getRandomSystemDiskPool    → StoragePoolsGetRandomSystemDiskPool
//	isSystemDiskPool           → StoragePoolsIsSystemDiskPool
//	hasStoragePool             → StoragePoolsHasStoragePool
//	resolveStoragePoolForNewInstance → StoragePoolsResolveNewInstancePool
//
// resolveStoragePoolForNewInstance is a thin wrapper over lib/storage-capacity.ts.
// db must not import lib (see db/packages.go's header), so it goes through the
// PackageStorageCapacityResolver hook that lib/storage_capacity.go installs. Passing
// requiredMB=0 matches the TypeScript, which calls getStorageCapacity(hostId,
// { packageId }) without a required size.
package db

import (
	"context"
	"errors"
	"math/rand"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ErrStoragePoolsDatabaseNotInitialized keeps an unwired call site loud.
var ErrStoragePoolsDatabaseNotInitialized = errors.New("storagePools: database not initialized")

func storagePoolsRequireDB() error {
	if DB == nil {
		return ErrStoragePoolsDatabaseNotInitialized
	}
	return nil
}

// StoragePoolsGetSystemDiskPoolsByHostID mirrors getSystemDiskPoolsByHostId:
// purpose='instance_data', created_at ASC.
func StoragePoolsGetSystemDiskPoolsByHostID(ctx context.Context, hostID int32) ([]models.StoragePool, error) {
	if err := storagePoolsRequireDB(); err != nil {
		return nil, err
	}

	var pools []models.StoragePool
	err := DB.WithContext(ctx).
		Where("host_id = ? AND purpose = ?", hostID, string(models.StoragePurposeInstanceData)).
		Order("created_at ASC").
		Find(&pools).Error
	if err != nil {
		return nil, err
	}
	return pools, nil
}

// StoragePoolsGetRandomSystemDiskPool mirrors getRandomSystemDiskPool: nil when the
// host has no instance_data pool, otherwise a uniformly random pool name.
func StoragePoolsGetRandomSystemDiskPool(ctx context.Context, hostID int32) (*string, error) {
	pools, err := StoragePoolsGetSystemDiskPoolsByHostID(ctx, hostID)
	if err != nil {
		return nil, err
	}
	if len(pools) == 0 {
		return nil, nil
	}
	index := rand.Intn(len(pools)) //nolint:gosec // matches Math.random; not a secret
	name := pools[index].Name
	return &name, nil
}

// StoragePoolsHasStoragePool mirrors hasStoragePool: every pool name, including
// "default", must be registered in the database.
func StoragePoolsHasStoragePool(ctx context.Context, hostID int32, poolName string) (bool, error) {
	if poolName == "" {
		return false, nil
	}
	if err := storagePoolsRequireDB(); err != nil {
		return false, err
	}

	var count int64
	err := DB.WithContext(ctx).Model(&models.StoragePool{}).
		Where("host_id = ? AND name = ?", hostID, poolName).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// StoragePoolsIsSystemDiskPool mirrors isSystemDiskPool: the named pool must exist
// and be an instance_data pool.
func StoragePoolsIsSystemDiskPool(ctx context.Context, hostID int32, poolName string) (bool, error) {
	if poolName == "" {
		return false, nil
	}
	if err := storagePoolsRequireDB(); err != nil {
		return false, err
	}

	var count int64
	err := DB.WithContext(ctx).Model(&models.StoragePool{}).
		Where("host_id = ? AND name = ? AND purpose = ?", hostID, poolName, string(models.StoragePurposeInstanceData)).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// StoragePoolsGetPackageHostStoragePool mirrors getPackageHostStoragePool.
func StoragePoolsGetPackageHostStoragePool(ctx context.Context, packageID, hostID int32) (*string, error) {
	if err := storagePoolsRequireDB(); err != nil {
		return nil, err
	}

	var row struct {
		StoragePoolName *string `gorm:"column:storage_pool_name"`
	}
	err := DB.WithContext(ctx).Raw(`SELECT storage_pool_name FROM package_hosts
		WHERE package_id = ? AND host_id = ? LIMIT 1`, packageID, hostID).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	return row.StoragePoolName, nil
}

// StoragePoolsResolveNewInstancePool mirrors resolveStoragePoolForNewInstance: the
// capacity resolver owns the pool selection, and a non-nil reason means no pool can
// host the instance.
func StoragePoolsResolveNewInstancePool(ctx context.Context, hostID int32, packageID *int32) (*string, error) {
	capacity, err := PackageStorageCapacityResolver(ctx, hostID, packageID, 0)
	if err != nil {
		return nil, err
	}
	if capacity.Reason != nil {
		return nil, nil
	}
	return capacity.PoolName, nil
}
