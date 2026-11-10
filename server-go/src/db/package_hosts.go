// Port of server/src/db/package-hosts.ts.
//
// package_hosts is the binding table between a package and the hosts that can
// deliver it: (package_id, host_id) is unique, and a package must keep at least
// one binding or it can never be scheduled. The "at least one" rule is enforced
// here (not only in the routes) because three different callers write bindings.
//
// Faithful non-atomicity: the TypeScript's setPackageHosts deletes every binding
// and then re-creates the list as two independent statements, without a
// transaction. That window is real in Node, so it is reproduced rather than
// "improved" — a caller that needs atomicity must wrap this in its own
// transaction (db/packages.go does exactly that for updatePackage).
//
// Note getPackageHostIds is exported by BOTH server/src/db/packages.ts and
// server/src/db/package-hosts.ts with identical bodies; the Go port has one
// function (this one) that serves both call sites.
package db

import (
	"context"
	"errors"
	"strings"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ErrPackageHostRequired mirrors `new Error('Package must have at least one host bound')`.
var ErrPackageHostRequired = errors.New("Package must have at least one host bound")

// ErrPackageHostBindingNotFound mirrors Prisma P2025 on the composite unique key
// (package_id, host_id): Prisma's delete throws when no row matches, it does not
// silently delete zero rows.
var ErrPackageHostBindingNotFound = errors.New("package_hosts: binding not found")

// PackageHostSummary mirrors the element type of getPackageHosts.
type PackageHostSummary struct {
	ID       int32   `json:"id"`
	Name     string  `json:"name"`
	Location *string `json:"location"`
	Status   string  `json:"status"`
}

// packageHostsDatabaseReady mirrors the implicit requirement that prisma is
// initialised; a nil handle fails loudly instead of panicking.
func packageHostsDatabaseReady() error {
	if DB == nil {
		return errors.New("package_hosts: database not initialized")
	}
	return nil
}

// GetPackageHostIds mirrors getPackageHostIds.
func GetPackageHostIds(ctx context.Context, packageID int32) ([]int32, error) {
	if err := packageHostsDatabaseReady(); err != nil {
		return nil, err
	}
	hostIDs := make([]int32, 0, 8)
	err := DB.WithContext(ctx).Raw(
		`SELECT host_id FROM package_hosts WHERE package_id = ? ORDER BY id ASC`, packageID,
	).Scan(&hostIDs).Error
	if err != nil {
		return nil, err
	}
	return hostIDs, nil
}

// GetPackageHosts mirrors getPackageHosts: the bound hosts with the fields the
// package admin UI lists.
func GetPackageHosts(ctx context.Context, packageID int32) ([]PackageHostSummary, error) {
	if err := packageHostsDatabaseReady(); err != nil {
		return nil, err
	}
	rows := make([]PackageHostSummary, 0, 8)
	err := DB.WithContext(ctx).Raw(
		`SELECT h.id AS id, h.name AS name, h.location AS location, h.status::text AS status
		   FROM package_hosts ph
		   JOIN hosts h ON h.id = ph.host_id
		  WHERE ph.package_id = ?
		  ORDER BY ph.id ASC`, packageID,
	).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// SetPackageHosts mirrors setPackageHosts: replace every binding.
//
// The two statements are NOT wrapped in a transaction, matching the TypeScript.
func SetPackageHosts(ctx context.Context, packageID int32, hostIDs []int32) error {
	// The TypeScript checks the list before touching Prisma, so the error does not
	// depend on a live connection.
	if len(hostIDs) == 0 {
		return ErrPackageHostRequired
	}
	if err := packageHostsDatabaseReady(); err != nil {
		return err
	}

	if err := DB.WithContext(ctx).Exec(
		`DELETE FROM package_hosts WHERE package_id = ?`, packageID,
	).Error; err != nil {
		return err
	}

	statement, args := packageHostsInsertStatement(packageID, hostIDs)
	return DB.WithContext(ctx).Exec(statement, args...).Error
}

// packageHostsInsertStatement mirrors `createMany({ data, skipDuplicates: true })`:
// only (package_id, host_id) is written, so the column defaults
// (traffic_multiplier = 1.0, created_at = now()) apply exactly like Prisma
// omitting those fields. The conflict target is the @@unique([packageId, hostId])
// key, so a duplicated host in the caller's list collapses instead of failing.
func packageHostsInsertStatement(packageID int32, hostIDs []int32) (string, []any) {
	values := make([]string, 0, len(hostIDs))
	args := make([]any, 0, len(hostIDs)*2)
	for _, hostID := range hostIDs {
		values = append(values, "(?, ?)")
		args = append(args, packageID, hostID)
	}
	return `INSERT INTO package_hosts (package_id, host_id) VALUES ` + strings.Join(values, ", ") +
		` ON CONFLICT (package_id, host_id) DO NOTHING`, args
}

// packageHostAddStatement mirrors `upsert({ where: { packageId_hostId }, create,
// update: {} })`: an existing binding is left untouched (no columns change), so
// DO NOTHING is the exact equivalent.
const packageHostAddStatement = `INSERT INTO package_hosts (package_id, host_id) VALUES (?, ?)
	 ON CONFLICT (package_id, host_id) DO NOTHING`

// AddPackageHost mirrors addPackageHost's upsert with `update: {}`: an existing
// binding keeps its storage pool and traffic multiplier untouched.
func AddPackageHost(ctx context.Context, packageID, hostID int32) error {
	if err := packageHostsDatabaseReady(); err != nil {
		return err
	}
	return DB.WithContext(ctx).Exec(packageHostAddStatement, packageID, hostID).Error
}

// RemovePackageHost mirrors removePackageHost: refuse to drop the last binding,
// then delete the composite key (P2025 → error when it is already gone).
func RemovePackageHost(ctx context.Context, packageID, hostID int32) error {
	if err := packageHostsDatabaseReady(); err != nil {
		return err
	}

	var currentCount int64
	if err := DB.WithContext(ctx).Raw(
		`SELECT count(*) FROM package_hosts WHERE package_id = ?`, packageID,
	).Scan(&currentCount).Error; err != nil {
		return err
	}
	if currentCount <= 1 {
		return ErrPackageHostRequired
	}

	tag := DB.WithContext(ctx).Exec(
		`DELETE FROM package_hosts WHERE package_id = ? AND host_id = ?`, packageID, hostID,
	)
	if tag.Error != nil {
		return tag.Error
	}
	if tag.RowsAffected == 0 {
		return ErrPackageHostBindingNotFound
	}
	return nil
}

// HasPackageHosts mirrors hasPackageHosts.
func HasPackageHosts(ctx context.Context, packageID int32) (bool, error) {
	if err := packageHostsDatabaseReady(); err != nil {
		return false, err
	}
	var count int64
	if err := DB.WithContext(ctx).Raw(
		`SELECT count(*) FROM package_hosts WHERE package_id = ?`, packageID,
	).Scan(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetHostPackages mirrors getHostPackages: which packages are bound to a host.
func GetHostPackages(ctx context.Context, hostID int32) ([]int32, error) {
	if err := packageHostsDatabaseReady(); err != nil {
		return nil, err
	}
	packageIDs := make([]int32, 0, 8)
	err := DB.WithContext(ctx).Raw(
		`SELECT package_id FROM package_hosts WHERE host_id = ? ORDER BY id ASC`, hostID,
	).Scan(&packageIDs).Error
	if err != nil {
		return nil, err
	}
	return packageIDs, nil
}

// packageHostsUpdateStoragePool mirrors `packageHost.update({ where:
// { packageId_hostId }, data: { storagePoolName } })`. Prisma throws P2025 when
// the binding does not exist, so zero affected rows is an error here too.
func packageHostsUpdateStoragePool(tx *gorm.DB, packageID, hostID int32, storagePoolName *string) error {
	result := tx.Exec(
		`UPDATE package_hosts SET storage_pool_name = ? WHERE package_id = ? AND host_id = ?`,
		storagePoolName, packageID, hostID,
	)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrPackageHostBindingNotFound
	}
	return nil
}

// packageHostsUpdateTrafficMultiplier mirrors `packageHost.update({ where:
// { packageId_hostId }, data: { trafficMultiplier } })`.
func packageHostsUpdateTrafficMultiplier(tx *gorm.DB, packageID, hostID int32, multiplier models.Decimal) error {
	result := tx.Exec(
		`UPDATE package_hosts SET traffic_multiplier = ? WHERE package_id = ? AND host_id = ?`,
		multiplier, packageID, hostID,
	)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrPackageHostBindingNotFound
	}
	return nil
}
