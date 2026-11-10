// Port of server/src/db/package-shares.ts (544 lines, 12 exports).
//
// Package sharing is the read/write surface behind eight of the package routes:
// the owner's share list, the "shared with me" list, and the access check that
// decides whether GET /api/packages/:id is 200 or 403. Two details are easy to
// lose and are therefore spelled out here:
//
//   - `quota_multiplier` is a Prisma Decimal. JavaScript tests the *object* for
//     truthiness, which is true even for 0.00, so the conversion to a number must
//     keep 0 as 0 and only map SQL NULL to null. Returning nil for 0 would make a
//     zero-quota share look unlimited.
//   - `getGlobalSharedPackages` normalises `global_max_instances` to 1 when the
//     column is missing or outside 1..5, because the response type has no nullable
//     variant. The instance-quota read (`getPackageShareForUser`) does the same.
//
// Ordering is database-defined exactly like the TypeScript: `getSharedToUser`,
// `getGlobalSharedPackages` and `getAccessiblePackageIds` issue no ORDER BY, while
// the owner-facing lists sort by created_at DESC.
package db

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ErrPackageSharesDatabaseNotInitialized keeps an unwired call site loud.
var ErrPackageSharesDatabaseNotInitialized = errors.New("packageShares: database not initialized")

// PackageShareInfo mirrors the PackageShareInfo interface (owner-facing lists).
type PackageShareInfo struct {
	ID                    int32     `json:"id"`
	PackageID             int32     `json:"packageId"`
	PackageName           string    `json:"packageName"`
	OwnerID               int32     `json:"ownerId"`
	OwnerUsername         string    `json:"ownerUsername"`
	SharedToID            int32     `json:"sharedToId"`
	SharedToUsername      string    `json:"sharedToUsername"`
	SharedToAvatarStyle   string    `json:"sharedToAvatarStyle"`
	SharedToAvatarBadgeID *string   `json:"sharedToAvatarBadgeId"`
	QuotaMultiplier       *float64  `json:"quotaMultiplier"`
	MaxInstances          *int32    `json:"maxInstances"`
	CreatedAt             time.Time `json:"createdAt"`
}

// SharedPackageInfo mirrors the SharedPackageInfo interface ("shared with me").
// Nested and Privileged are numbers (0/1) because the TypeScript converts the
// booleans before returning them.
type SharedPackageInfo struct {
	ID                    int32     `json:"id"`
	Name                  string    `json:"name"`
	Description           *string   `json:"description"`
	CPUMax                int32     `json:"cpuMax"`
	MemoryMax             int32     `json:"memoryMax"`
	DiskMax               int32     `json:"diskMax"`
	BandwidthMax          *int32    `json:"bandwidthMax"`
	NetworkMode           string    `json:"networkMode"`
	GPUShared             bool      `json:"gpuShared"`
	GPUDeviceIDs          []int32   `json:"gpuDeviceIds"`
	InstanceType          string    `json:"instanceType"`
	PortLimit             int32     `json:"portLimit"`
	SnapshotLimit         int32     `json:"snapshotLimit"`
	BackupLimit           int32     `json:"backupLimit"`
	SiteLimit             int32     `json:"siteLimit"`
	MonthlyTrafficLimit   *string   `json:"monthlyTrafficLimit"`
	Nested                int       `json:"nested"`
	Privileged            int       `json:"privileged"`
	AllowInstanceDeletion bool      `json:"allowInstanceDeletion"`
	HostIDs               []int32   `json:"hostIds"`
	OwnerID               int32     `json:"ownerId"`
	OwnerUsername         string    `json:"ownerUsername"`
	SharedAt              time.Time `json:"sharedAt"`
	IsGlobalShared        bool      `json:"isGlobalShared"`
	QuotaMultiplier       *float64  `json:"quotaMultiplier"`
	MaxInstances          *int32    `json:"maxInstances"`
	RequiredPackageID     *int32    `json:"requiredPackageId"`
	RequiredPackageName   *string   `json:"requiredPackageName"`
}

// SharedPackageUsage mirrors getSharedPackageUsage's return value.
type SharedPackageUsage struct {
	InstanceCount int   `json:"instanceCount"`
	TotalCPU      int64 `json:"totalCpu"`
	TotalMemory   int64 `json:"totalMemory"`
}

// PackageShareForUser mirrors getPackageShareForUser's return value.
type PackageShareForUser struct {
	ID              int32    `json:"id"`
	QuotaMultiplier *float64 `json:"quotaMultiplier"`
	MaxInstances    *int32   `json:"maxInstances"`
	IsGlobalShared  bool     `json:"isGlobalShared"`
}

func packageSharesRequireDB() error {
	if Pool() == nil || DB == nil {
		return ErrPackageSharesDatabaseNotInitialized
	}
	return nil
}

// packageSharesNormalizeGlobalMaxInstances mirrors normalizeGlobalMaxInstances.
func packageSharesNormalizeGlobalMaxInstances(value *int32) int32 {
	if value != nil && *value >= 1 && *value <= 5 {
		return *value
	}
	return 1
}

// packageSharesQuotaNumber mirrors `s.quotaMultiplier ? Number(s.quotaMultiplier) : null`:
// a Prisma Decimal is a JS object, hence truthy even at 0.00, so only SQL NULL
// maps to null.
func packageSharesQuotaNumber(value *models.Decimal) *float64 {
	if value == nil {
		return nil
	}
	number := decimalToFloat64(*value)
	return &number
}

// packageSharesDecimal converts an optional JS number to the numeric column. The
// route already validated the 0.1..99.9 / one-decimal shape.
func packageSharesDecimal(value *float64) *models.Decimal {
	if value == nil {
		return nil
	}
	decimalValue := packageDecimalFromFloat(*value)
	return &decimalValue
}

// packageSharesHostIDsJSON parses the JSON array produced by
// `array_to_json(ARRAY(...))::text`; nil means "no rows" and collapses to [].
func packageSharesHostIDsJSON(raw *string) []int32 {
	if raw == nil {
		return []int32{}
	}
	return packageJSONInt32Array(raw)
}

// ==================== writes ====================

// SharePackage mirrors sharePackage(packageId, ownerId, sharedToId, quota, max).
func SharePackage(ctx context.Context, packageID, ownerID, sharedToID int32, quotaMultiplier *float64, maxInstances *int32) (int32, error) {
	if err := packageSharesRequireDB(); err != nil {
		return 0, err
	}

	var shareID int32
	err := Pool().QueryRow(ctx, `INSERT INTO package_shares
		(package_id, owner_id, shared_to_id, quota_multiplier, max_instances)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		packageID, ownerID, sharedToID, packageSharesDecimal(quotaMultiplier), maxInstances,
	).Scan(&shareID)
	if err != nil {
		return 0, err
	}
	return shareID, nil
}

// UpdateShareQuota mirrors updateShareQuota: both columns are always written and
// the package id is part of the WHERE so a share cannot be retargeted.
func UpdateShareQuota(ctx context.Context, shareID, packageID int32, quotaMultiplier *float64, maxInstances *int32) (bool, error) {
	if err := packageSharesRequireDB(); err != nil {
		return false, err
	}

	tag, err := Pool().Exec(ctx, `UPDATE package_shares
		SET quota_multiplier = $3, max_instances = $4
		WHERE id = $1 AND package_id = $2`,
		shareID, packageID, packageSharesDecimal(quotaMultiplier), maxInstances,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UnsharePackage mirrors unsharePackage(packageId, sharedToId).
func UnsharePackage(ctx context.Context, packageID, sharedToID int32) (bool, error) {
	if err := packageSharesRequireDB(); err != nil {
		return false, err
	}

	tag, err := Pool().Exec(ctx, `DELETE FROM package_shares WHERE package_id = $1 AND shared_to_id = $2`,
		packageID, sharedToID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UnsharePackageAll mirrors unsharePackageAll(packageId).
func UnsharePackageAll(ctx context.Context, packageID int32) (int64, error) {
	if err := packageSharesRequireDB(); err != nil {
		return 0, err
	}

	tag, err := Pool().Exec(ctx, `DELETE FROM package_shares WHERE package_id = $1`, packageID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// UnsharePackageBetweenUsers mirrors unsharePackageBetweenUsers: both directions,
// summed.
func UnsharePackageBetweenUsers(ctx context.Context, userID1, userID2 int32) (int64, error) {
	if err := packageSharesRequireDB(); err != nil {
		return 0, err
	}

	var total int64
	for _, direction := range [][2]int32{{userID1, userID2}, {userID2, userID1}} {
		tag, err := Pool().Exec(ctx, `DELETE FROM package_shares WHERE owner_id = $1 AND shared_to_id = $2`,
			direction[0], direction[1])
		if err != nil {
			return 0, err
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

// IsPackageSharedTo mirrors isPackageSharedTo(packageId, userId).
func IsPackageSharedTo(ctx context.Context, packageID, userID int32) (bool, error) {
	if err := packageSharesRequireDB(); err != nil {
		return false, err
	}

	var exists bool
	if err := Pool().QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM package_shares WHERE package_id = $1 AND shared_to_id = $2)`,
		packageID, userID).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

// ==================== reads ====================

// GetSharedPackageUsage mirrors getSharedPackageUsage: only running/stopped
// instances count towards a share's quota.
func GetSharedPackageUsage(ctx context.Context, packageID, userID int32) (SharedPackageUsage, error) {
	if err := packageSharesRequireDB(); err != nil {
		return SharedPackageUsage{}, err
	}

	var row struct {
		InstanceCount int   `gorm:"column:instance_count"`
		TotalCPU      int64 `gorm:"column:total_cpu"`
		TotalMemory   int64 `gorm:"column:total_memory"`
	}
	err := DB.WithContext(ctx).Raw(`SELECT COUNT(*) AS instance_count,
			COALESCE(SUM(cpu), 0) AS total_cpu,
			COALESCE(SUM(memory), 0) AS total_memory
		FROM instances
		WHERE user_id = ? AND package_id = ? AND status IN ('running', 'stopped')`,
		userID, packageID).Scan(&row).Error
	if err != nil {
		return SharedPackageUsage{}, err
	}
	return SharedPackageUsage{
		InstanceCount: row.InstanceCount,
		TotalCPU:      row.TotalCPU,
		TotalMemory:   row.TotalMemory,
	}, nil
}

// GetPackageShareForUser mirrors getPackageShareForUser. A global-shared package
// that belongs to someone else reports its normalised global max instances; a
// friend share reports the per-share limits; anything else is nil.
func GetPackageShareForUser(ctx context.Context, packageID, userID int32) (*PackageShareForUser, error) {
	if err := packageSharesRequireDB(); err != nil {
		return nil, err
	}

	var pkg struct {
		ID                 int32  `gorm:"column:id"`
		GlobalShared       bool   `gorm:"column:global_shared"`
		GlobalMaxInstances *int32 `gorm:"column:global_max_instances"`
		UserID             int32  `gorm:"column:user_id"`
	}
	err := DB.WithContext(ctx).Raw(`SELECT id, global_shared, global_max_instances, user_id
		FROM packages WHERE id = ? LIMIT 1`, packageID).Scan(&pkg).Error
	if err != nil {
		return nil, err
	}
	if pkg.ID == 0 {
		return nil, nil
	}

	if pkg.GlobalShared && pkg.UserID != userID {
		maxInstances := packageSharesNormalizeGlobalMaxInstances(pkg.GlobalMaxInstances)
		return &PackageShareForUser{
			ID:              packageID,
			QuotaMultiplier: nil,
			MaxInstances:    &maxInstances,
			IsGlobalShared:  true,
		}, nil
	}

	if pkg.UserID != userID {
		var share struct {
			ID              int32           `gorm:"column:id"`
			QuotaMultiplier *models.Decimal `gorm:"column:quota_multiplier"`
			MaxInstances    *int32          `gorm:"column:max_instances"`
		}
		err := DB.WithContext(ctx).Raw(`SELECT id, quota_multiplier, max_instances
			FROM package_shares WHERE package_id = ? AND shared_to_id = ? LIMIT 1`,
			packageID, userID).Scan(&share).Error
		if err != nil {
			return nil, err
		}
		if share.ID != 0 {
			return &PackageShareForUser{
				ID:              share.ID,
				QuotaMultiplier: packageSharesQuotaNumber(share.QuotaMultiplier),
				MaxInstances:    share.MaxInstances,
				IsGlobalShared:  false,
			}, nil
		}
	}

	return nil, nil
}

// packageShareInfoColumns is the shared projection of the two owner-facing
// queries; the joins are identical apart from the filter column.
const packageShareInfoColumns = `SELECT ps.id, ps.package_id, ps.owner_id, ps.shared_to_id,
	ps.quota_multiplier, ps.max_instances, ps.created_at,
	p.name AS package_name, o.username AS owner_username,
	s.username AS shared_to_username, s.avatar_style AS shared_to_avatar_style,
	s.avatar_badge_id AS shared_to_avatar_badge_id
	FROM package_shares ps
	JOIN packages p ON p.id = ps.package_id
	JOIN users o ON o.id = ps.owner_id
	JOIN users s ON s.id = ps.shared_to_id`

func packageShareInfoRows(ctx context.Context, query string, args ...any) ([]PackageShareInfo, error) {
	var rows []struct {
		ID                    int32           `gorm:"column:id"`
		PackageID             int32           `gorm:"column:package_id"`
		OwnerID               int32           `gorm:"column:owner_id"`
		SharedToID            int32           `gorm:"column:shared_to_id"`
		QuotaMultiplier       *models.Decimal `gorm:"column:quota_multiplier"`
		MaxInstances          *int32          `gorm:"column:max_instances"`
		CreatedAt             time.Time       `gorm:"column:created_at"`
		PackageName           string          `gorm:"column:package_name"`
		OwnerUsername         string          `gorm:"column:owner_username"`
		SharedToUsername      string          `gorm:"column:shared_to_username"`
		SharedToAvatarStyle   string          `gorm:"column:shared_to_avatar_style"`
		SharedToAvatarBadgeID *string         `gorm:"column:shared_to_avatar_badge_id"`
	}
	if err := DB.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}

	result := make([]PackageShareInfo, 0, len(rows))
	for _, row := range rows {
		result = append(result, PackageShareInfo{
			ID:                    row.ID,
			PackageID:             row.PackageID,
			PackageName:           row.PackageName,
			OwnerID:               row.OwnerID,
			OwnerUsername:         row.OwnerUsername,
			SharedToID:            row.SharedToID,
			SharedToUsername:      row.SharedToUsername,
			SharedToAvatarStyle:   row.SharedToAvatarStyle,
			SharedToAvatarBadgeID: row.SharedToAvatarBadgeID,
			QuotaMultiplier:       packageSharesQuotaNumber(row.QuotaMultiplier),
			MaxInstances:          row.MaxInstances,
			CreatedAt:             row.CreatedAt,
		})
	}
	return result, nil
}

// GetPackageShares mirrors getPackageShares (owner view, created_at DESC).
func GetPackageShares(ctx context.Context, packageID int32) ([]PackageShareInfo, error) {
	if err := packageSharesRequireDB(); err != nil {
		return nil, err
	}
	return packageShareInfoRows(ctx, packageShareInfoColumns+
		` WHERE ps.package_id = ? ORDER BY ps.created_at DESC`, packageID)
}

// GetSharedByUser mirrors getSharedByUser (created_at DESC).
func GetSharedByUser(ctx context.Context, userID int32) ([]PackageShareInfo, error) {
	if err := packageSharesRequireDB(); err != nil {
		return nil, err
	}
	return packageShareInfoRows(ctx, packageShareInfoColumns+
		` WHERE ps.owner_id = ? ORDER BY ps.created_at DESC`, userID)
}

// sharedPackageColumns is the projection of getSharedToUser /
// getGlobalSharedPackages. `array_to_json(ARRAY(...))::text` is the int[]
// representation both sides agree on (same trick as db/gpus.go and db/packages.go).
const sharedPackageColumns = `SELECT p.id, p.name, p.description, p.cpu_max, p.memory_max,
	p.disk_max, p.bandwidth_max, p.network_mode, p.gpu_shared,
	array_to_json(p.gpu_device_ids)::text AS gpu_device_ids_json,
	p.instance_type, p.port_limit, p.snapshot_limit, p.backup_limit, p.site_limit,
	p.monthly_traffic_limit, p.nested, p.privileged, p.allow_instance_deletion,
	p.created_at, p.global_max_instances,
	p.user_id AS owner_id, u.username AS owner_username,
	array_to_json(ARRAY(SELECT ph.host_id FROM package_hosts ph WHERE ph.package_id = p.id))::text AS host_ids_json,
	rp.name AS required_package_name, p.required_package_id
	FROM packages p
	JOIN users u ON u.id = p.user_id
	LEFT JOIN packages rp ON rp.id = p.required_package_id`

type sharedPackageRow struct {
	ID                    int32     `gorm:"column:id"`
	Name                  string    `gorm:"column:name"`
	Description           *string   `gorm:"column:description"`
	CPUMax                int32     `gorm:"column:cpu_max"`
	MemoryMax             int32     `gorm:"column:memory_max"`
	DiskMax               int32     `gorm:"column:disk_max"`
	BandwidthMax          *int32    `gorm:"column:bandwidth_max"`
	NetworkMode           string    `gorm:"column:network_mode"`
	GPUShared             bool      `gorm:"column:gpu_shared"`
	GPUDeviceIDsJSON      *string   `gorm:"column:gpu_device_ids_json"`
	InstanceType          string    `gorm:"column:instance_type"`
	PortLimit             int32     `gorm:"column:port_limit"`
	SnapshotLimit         int32     `gorm:"column:snapshot_limit"`
	BackupLimit           int32     `gorm:"column:backup_limit"`
	SiteLimit             int32     `gorm:"column:site_limit"`
	MonthlyTrafficLimit   *int64    `gorm:"column:monthly_traffic_limit"`
	Nested                bool      `gorm:"column:nested"`
	Privileged            bool      `gorm:"column:privileged"`
	AllowInstanceDeletion bool      `gorm:"column:allow_instance_deletion"`
	CreatedAt             time.Time `gorm:"column:created_at"`
	GlobalMaxInstances    *int32    `gorm:"column:global_max_instances"`
	OwnerID               int32     `gorm:"column:owner_id"`
	OwnerUsername         string    `gorm:"column:owner_username"`
	HostIDsJSON           *string   `gorm:"column:host_ids_json"`
	RequiredPackageName   *string   `gorm:"column:required_package_name"`
	RequiredPackageID     *int32    `gorm:"column:required_package_id"`
}

func (row sharedPackageRow) toSharedPackageInfo() SharedPackageInfo {
	instanceType := "container"
	if row.InstanceType == "vm" {
		instanceType = "vm"
	}
	nested := 0
	if row.Nested {
		nested = 1
	}
	privileged := 0
	if row.Privileged {
		privileged = 1
	}
	var monthlyTrafficLimit *string
	if row.MonthlyTrafficLimit != nil {
		text := strconv.FormatInt(*row.MonthlyTrafficLimit, 10)
		monthlyTrafficLimit = &text
	}
	return SharedPackageInfo{
		ID:                    row.ID,
		Name:                  row.Name,
		Description:           row.Description,
		CPUMax:                row.CPUMax,
		MemoryMax:             row.MemoryMax,
		DiskMax:               row.DiskMax,
		BandwidthMax:          row.BandwidthMax,
		NetworkMode:           row.NetworkMode,
		GPUShared:             row.GPUShared,
		GPUDeviceIDs:          packageJSONInt32Array(row.GPUDeviceIDsJSON),
		InstanceType:          instanceType,
		PortLimit:             row.PortLimit,
		SnapshotLimit:         row.SnapshotLimit,
		BackupLimit:           row.BackupLimit,
		SiteLimit:             row.SiteLimit,
		MonthlyTrafficLimit:   monthlyTrafficLimit,
		Nested:                nested,
		Privileged:            privileged,
		AllowInstanceDeletion: row.AllowInstanceDeletion,
		HostIDs:               packageSharesHostIDsJSON(row.HostIDsJSON),
		OwnerID:               row.OwnerID,
		OwnerUsername:         row.OwnerUsername,
		SharedAt:              row.CreatedAt,
		RequiredPackageID:     row.RequiredPackageID,
		RequiredPackageName:   row.RequiredPackageName,
	}
}

func sharedPackageRows(ctx context.Context, query string, args ...any) ([]sharedPackageRow, error) {
	var rows []sharedPackageRow
	if err := DB.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// GetSharedToUser mirrors getSharedToUser: shares addressed to the user, active
// packages only, created_at DESC.
func GetSharedToUser(ctx context.Context, userID int32) ([]SharedPackageInfo, error) {
	if err := packageSharesRequireDB(); err != nil {
		return nil, err
	}

	type shareRow struct {
		ShareID         int32           `gorm:"column:share_id"`
		QuotaMultiplier *models.Decimal `gorm:"column:quota_multiplier"`
		MaxInstances    *int32          `gorm:"column:max_instances"`
		CreatedAt       time.Time       `gorm:"column:created_at"`
		PackageID       int32           `gorm:"column:package_id"`
		Active          bool            `gorm:"column:active"`
	}
	var shares []shareRow
	err := DB.WithContext(ctx).Raw(`SELECT ps.id AS share_id, ps.quota_multiplier,
			ps.max_instances, ps.created_at, ps.package_id, p.active
		FROM package_shares ps
		JOIN packages p ON p.id = ps.package_id
		WHERE ps.shared_to_id = ?
		ORDER BY ps.created_at DESC`, userID).Scan(&shares).Error
	if err != nil {
		return nil, err
	}

	packageIDs := make([]int32, 0, len(shares))
	shareByPackage := make(map[int32]shareRow, len(shares))
	for _, share := range shares {
		if !share.Active {
			continue
		}
		packageIDs = append(packageIDs, share.PackageID)
		if _, exists := shareByPackage[share.PackageID]; !exists {
			shareByPackage[share.PackageID] = share
		}
	}
	if len(packageIDs) == 0 {
		return []SharedPackageInfo{}, nil
	}

	placeholders, packageArgs := int32Placeholders(packageIDs)
	rows, err := sharedPackageRows(ctx, sharedPackageColumns+
		` WHERE p.id IN (`+placeholders+`)`, packageArgs...)
	if err != nil {
		return nil, err
	}
	byID := make(map[int32]SharedPackageInfo, len(rows))
	for _, row := range rows {
		byID[row.ID] = row.toSharedPackageInfo()
	}

	// Preserve share order (created_at DESC), not the IN-list order.
	result := make([]SharedPackageInfo, 0, len(packageIDs))
	for _, packageID := range packageIDs {
		pkg, ok := byID[packageID]
		if !ok {
			continue
		}
		share := shareByPackage[packageID]
		pkg.SharedAt = share.CreatedAt
		pkg.IsGlobalShared = false
		pkg.QuotaMultiplier = packageSharesQuotaNumber(share.QuotaMultiplier)
		pkg.MaxInstances = share.MaxInstances
		result = append(result, pkg)
	}
	return result, nil
}

// GetGlobalSharedPackages mirrors getGlobalSharedPackages: active global-shared
// packages owned by an admin, created_at DESC, with the normalised global max and
// the package's created_at as sharedAt.
func GetGlobalSharedPackages(ctx context.Context) ([]SharedPackageInfo, error) {
	if err := packageSharesRequireDB(); err != nil {
		return nil, err
	}

	rows, err := sharedPackageRows(ctx, sharedPackageColumns+
		` WHERE p.global_shared = true AND p.active = true
		  AND EXISTS (SELECT 1 FROM users u2 WHERE u2.id = p.user_id AND u2.role::text = 'admin')
		  ORDER BY p.created_at DESC`)
	if err != nil {
		return nil, err
	}

	result := make([]SharedPackageInfo, 0, len(rows))
	for _, row := range rows {
		pkg := row.toSharedPackageInfo()
		pkg.IsGlobalShared = true
		pkg.QuotaMultiplier = nil
		maxInstances := packageSharesNormalizeGlobalMaxInstances(row.GlobalMaxInstances)
		pkg.MaxInstances = &maxInstances
		result = append(result, pkg)
	}
	return result, nil
}

// GetAccessiblePackageIds mirrors getAccessiblePackageIds: own active packages
// plus active global-shared packages owned by someone else, de-duplicated in that
// order.
func GetAccessiblePackageIds(ctx context.Context, userID int32) ([]int32, error) {
	if err := packageSharesRequireDB(); err != nil {
		return nil, err
	}

	var ownIDs []int32
	if err := DB.WithContext(ctx).Raw(`SELECT id FROM packages WHERE user_id = ? AND active = true`,
		userID).Scan(&ownIDs).Error; err != nil {
		return nil, err
	}
	var publicIDs []int32
	if err := DB.WithContext(ctx).Raw(`SELECT id FROM packages
		WHERE global_shared = true AND active = true AND user_id <> ?`, userID).Scan(&publicIDs).Error; err != nil {
		return nil, err
	}

	seen := make(map[int32]bool, len(ownIDs)+len(publicIDs))
	result := make([]int32, 0, len(ownIDs)+len(publicIDs))
	for _, id := range append(ownIDs, publicIDs...) {
		if seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	return result, nil
}

// CanUserAccessPackage mirrors canUserAccessPackage: owner always, non-owner only
// when the package is active and either globally shared or shared with the user.
func CanUserAccessPackage(ctx context.Context, userID, packageID int32) (bool, error) {
	if err := packageSharesRequireDB(); err != nil {
		return false, err
	}

	var pkg struct {
		ID           int32 `gorm:"column:id"`
		UserID       int32 `gorm:"column:user_id"`
		Active       bool  `gorm:"column:active"`
		GlobalShared bool  `gorm:"column:global_shared"`
	}
	if err := DB.WithContext(ctx).Raw(`SELECT id, user_id, active, global_shared
		FROM packages WHERE id = ? LIMIT 1`, packageID).Scan(&pkg).Error; err != nil {
		return false, err
	}
	if pkg.ID == 0 {
		return false, nil
	}
	if pkg.UserID == userID {
		return true, nil
	}
	if !pkg.Active {
		return false, nil
	}
	if pkg.GlobalShared {
		return true, nil
	}
	return IsPackageSharedTo(ctx, packageID, userID)
}
