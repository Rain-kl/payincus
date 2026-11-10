// Port of server/src/db/packages.ts (1646 lines, 19 exports).
//
// A package is the sellable shape of an instance: its resource ceilings, its
// traffic allowance, its GPU/egress policy and the hosts that may deliver it.
// Several invariants here are resource promises, not conveniences:
//
//   - createPackage/updatePackage run in a transaction that takes the
//     single-argument egress advisory lock (74201) and, when a prerequisite is
//     involved, the package prerequisite lock (4201, key 1). The prerequisite
//     path additionally runs at SERIALIZABLE isolation, because the chain walk
//     reads other packages' required_package_id rows and must see a stable
//     snapshot. The non-prerequisite path keeps the default isolation, exactly
//     like the TypeScript.
//   - A prerequisite chain is validated for existence, ownership scope, self
//     reference and cycles *inside* the lock; a concurrent pair of updates that
//     each pass validation would otherwise be able to create A→B→A.
//   - An active package must resolve a system-disk pool on every bound host, and
//     a pool named by the caller must exist on that host with
//     purpose='instance_data'. Otherwise the package would be schedulable onto a
//     host that cannot store its root disk.
//   - Traffic multipliers are normalised through the same [0.001, 100] rule as
//     lib/traffic-multiplier.ts (db carries the private duplicate because
//     db must not import lib).
//
// db ↛ lib: the two lib capabilities this file needs (storage capacity and
// package egress validation) are reached through the Register* hooks below.
// lib/storage_capacity.go and lib/egress_package.go install them from init().
//
// Tri-state updates: the TypeScript distinguishes "key absent" from "explicit
// null" in updatePackage (`data.description !== undefined`,
// `data.egressProfileId === undefined ? current : ...`). Go's *T cannot, so
// scalar update slots use PackageField[T] (Present + Value). The three collection
// slots (hostIds, hostStoragePools, hostTrafficMultipliers) use nil = absent,
// non-nil = present (an empty slice/map is a real value).
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// packageNormalInstanceStatuses mirrors NORMAL_PACKAGE_INSTANCE_STATUSES.
var packageNormalInstanceStatuses = []string{"running", "stopped"}

// packageDefaultStoragePoolName mirrors DEFAULT_STORAGE_POOL_NAME.
const packageDefaultStoragePoolName = "default"

// ==================== errors ====================

var (
	// ErrPackageNotFound mirrors `new Error('Package not found')`.
	ErrPackageNotFound = errors.New("Package not found")
	// ErrPackageHostBindingRequired mirrors `new Error('Package must bind at least one host')`.
	ErrPackageHostBindingRequired = errors.New("Package must bind at least one host")
	// ErrPackageRecordNotFound mirrors Prisma P2025 on package.update/delete:
	// Prisma throws instead of updating zero rows.
	ErrPackageRecordNotFound = errors.New("packages: record not found")
	// ErrPackageDatabaseNotInitialized is returned when db.Init has not run.
	ErrPackageDatabaseNotInitialized = errors.New("packages: database not initialized")
)

// ==================== hooks (db ↛ lib) ====================

// PackageStorageCapacity mirrors lib.StorageCapacity without importing lib.
type PackageStorageCapacity struct {
	PoolName    *string
	TotalMB     int64
	UsedMB      int64
	AllocatedMB int64
	AvailableMB int64
	HeadroomMB  int64
	Reason      *string
}

// PackageStorageCapacityResolver is installed by lib/storage_capacity.go's
// init(). It resolves getStorageCapacity(hostId, { packageId, requiredMB }).
var PackageStorageCapacityResolver = func(ctx context.Context, hostID int32, packageID *int32, requiredMB int64) (PackageStorageCapacity, error) {
	return PackageStorageCapacity{}, errors.New("packages: storage capacity resolver is not registered")
}

// RegisterPackageStorageCapacityResolver wires lib.GetStorageCapacity into this
// package. lib calls it from init(); db must never import lib.
func RegisterPackageStorageCapacityResolver(resolver func(ctx context.Context, hostID int32, packageID *int32, requiredMB int64) (PackageStorageCapacity, error)) {
	if resolver != nil {
		PackageStorageCapacityResolver = resolver
	}
}

// PackageEgressValidationInput mirrors the object passed to
// lib.ValidatePackageEgress.
type PackageEgressValidationInput struct {
	EgressMode      *string
	EgressProfileID *int32
	NetworkMode     *string
	InstanceType    *string
	Privileged      bool
	HostIDs         []int32
}

// PackageEgressValidator is installed by lib/egress_package.go's init().
var PackageEgressValidator = func(ctx context.Context, client *gorm.DB, input PackageEgressValidationInput) error {
	return errors.New("packages: package egress validator is not registered")
}

// RegisterPackageEgressValidator wires lib.ValidatePackageEgress into this package.
func RegisterPackageEgressValidator(validator func(ctx context.Context, client *gorm.DB, input PackageEgressValidationInput) error) {
	if validator != nil {
		PackageEgressValidator = validator
	}
}

// ==================== wire types ====================

// PackageField mirrors a TypeScript update slot `field?: T | null`:
//
//   - Present=false            → the key was not sent; leave the column alone
//   - Present=true, Value=nil  → explicit null; write SQL NULL
//   - Present=true, Value!=nil → write the value
type PackageField[T any] struct {
	Present bool
	Value   *T
}

// PackageValue builds a present field holding value.
func PackageValue[T any](value T) PackageField[T] {
	return PackageField[T]{Present: true, Value: &value}
}

// PackageNull builds an explicitly-null field.
func PackageNull[T any]() PackageField[T] {
	return PackageField[T]{Present: true}
}

// UnmarshalJSON lets a route decode an update body straight into
// PackageUpdateData while preserving absent-vs-null.
func (f *PackageField[T]) UnmarshalJSON(data []byte) error {
	f.Present = true
	if string(data) == "null" {
		f.Value = nil
		return nil
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	f.Value = &value
	return nil
}

// MarshalJSON renders the field the way the TypeScript object would.
func (f PackageField[T]) MarshalJSON() ([]byte, error) {
	if f.Value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*f.Value)
}

// PackagePrerequisiteValidationResult mirrors PackagePrerequisiteValidationResult.
type PackagePrerequisiteValidationResult struct {
	Valid   bool   `json:"valid"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// PackageDependency mirrors getPackagesDependingOnPackage's element.
type PackageDependency struct {
	ID   int32  `gorm:"column:id" json:"id"`
	Name string `gorm:"column:name" json:"name"`
}

// PackageIncompatible mirrors getIncompatiblePackagesByHostTypeChange's element.
type PackageIncompatible struct {
	ID           int32  `gorm:"column:id" json:"id"`
	Name         string `gorm:"column:name" json:"name"`
	InstanceType string `gorm:"column:instance_type" json:"instanceType"`
}

// PackageHostDetail mirrors getPackageHostsDetail's element.
type PackageHostDetail struct {
	ID              int32  `gorm:"column:id" json:"id"`
	Name            string `gorm:"column:name" json:"name"`
	CountryCode     string `gorm:"column:country_code" json:"countryCode"`
	CPUAllowanceMax int32  `gorm:"column:cpu_allowance_max" json:"cpuAllowanceMax"`
	MemoryMax       int32  `gorm:"column:memory_max" json:"memoryMax"`
	CPUUsed         int32  `gorm:"column:cpu_used" json:"cpuUsed"`
	MemoryUsed      int32  `gorm:"column:memory_used" json:"memoryUsed"`
}

// PackageQuotaIncrease mirrors increaseHostQuota's return value.
type PackageQuotaIncrease struct {
	CPUAllowanceMax int32 `gorm:"column:cpu_allowance_max" json:"cpuAllowanceMax"`
	MemoryMax       int32 `gorm:"column:memory_max" json:"memoryMax"`
}

// PackageListOptions mirrors getAllPackages' options object.
type PackageListOptions struct {
	UserID            *int32
	ExcludeUserID     *int32
	OwnerRole         *string
	IncludeOwnerEmail bool
}

// PackageListRow mirrors getAllPackages' element. Field order matches the
// TypeScript object literal so a raw JSON diff stays readable.
type PackageListRow struct {
	ID                      int32              `json:"id"`
	UserID                  int32              `json:"user_id"`
	Name                    string             `json:"name"`
	Description             *string            `json:"description"`
	CPUMax                  int32              `json:"cpu_max"`
	MemoryMax               int32              `json:"memory_max"`
	DiskMax                 int32              `json:"disk_max"`
	BandwidthMax            *int32             `json:"bandwidth_max"`
	NetworkMode             string             `json:"network_mode"`
	InstanceType            string             `json:"instance_type"`
	EgressMode              string             `json:"egress_mode"`
	EgressProfileID         *int32             `json:"egress_profile_id"`
	GPUShared               bool               `json:"gpu_shared"`
	GPUDeviceIDs            []int32            `json:"gpu_device_ids"`
	Privileged              int                `json:"privileged"`
	Nested                  int                `json:"nested"`
	Active                  int                `json:"active"`
	PortLimit               int32              `json:"port_limit"`
	SnapshotLimit           int32              `json:"snapshot_limit"`
	BackupLimit             int32              `json:"backup_limit"`
	SiteLimit               int32              `json:"site_limit"`
	MonthlyTrafficLimit     *string            `json:"monthly_traffic_limit"`
	TrafficResetPrice       float64            `json:"traffic_reset_price"`
	IOLimitMode             string             `json:"io_limit_mode"`
	LimitsRead              string             `json:"limits_read"`
	LimitsWrite             string             `json:"limits_write"`
	LimitsReadIops          int32              `json:"limits_read_iops"`
	LimitsWriteIops         int32              `json:"limits_write_iops"`
	LimitsIngress           string             `json:"limits_ingress"`
	LimitsEgress            string             `json:"limits_egress"`
	LimitsProcesses         int32              `json:"limits_processes"`
	LimitsCPUPriority       int32              `json:"limits_cpu_priority"`
	BootAutostart           bool               `json:"boot_autostart"`
	BootAutostartPriority   int32              `json:"boot_autostart_priority"`
	BootAutostartDelay      int32              `json:"boot_autostart_delay"`
	BootHostShutdownTimeout int32              `json:"boot_host_shutdown_timeout"`
	GlobalShared            bool               `json:"global_shared"`
	GlobalQuotaMultiplier   *float64           `json:"global_quota_multiplier"`
	GlobalMaxInstances      *int32             `json:"global_max_instances,omitempty"`
	RequiredPackageID       *int32             `json:"required_package_id"`
	RequiredPackageName     *string            `json:"required_package_name"`
	AllowInstanceDeletion   bool               `json:"allow_instance_deletion"`
	CreatedAt               string             `json:"created_at"`
	OwnerUsername           *string            `json:"owner_username,omitempty"`
	OwnerEmail              any                `json:"owner_email,omitempty"`
	OwnerAvatarStyle        any                `json:"owner_avatar_style,omitempty"`
	OwnerAvatarBadgeID      any                `json:"owner_avatar_badge_id,omitempty"`
	IsOwn                   bool               `json:"is_own"`
	HostIDs                 []int32            `json:"host_ids"`
	HostStoragePools        map[string]*string `json:"host_storage_pools"`
	HostTrafficMultipliers  map[string]float64 `json:"host_traffic_multipliers"`
}

// PackageDetailRow mirrors getPackageById's return shape (no owner fields).
type PackageDetailRow struct {
	ID                      int32              `json:"id"`
	UserID                  int32              `json:"user_id"`
	Name                    string             `json:"name"`
	Description             *string            `json:"description"`
	CPUMax                  int32              `json:"cpu_max"`
	MemoryMax               int32              `json:"memory_max"`
	DiskMax                 int32              `json:"disk_max"`
	BandwidthMax            *int32             `json:"bandwidth_max"`
	NetworkMode             string             `json:"network_mode"`
	InstanceType            string             `json:"instance_type"`
	EgressMode              string             `json:"egress_mode"`
	EgressProfileID         *int32             `json:"egress_profile_id"`
	GPUShared               bool               `json:"gpu_shared"`
	GPUDeviceIDs            []int32            `json:"gpu_device_ids"`
	Privileged              int                `json:"privileged"`
	Nested                  int                `json:"nested"`
	Active                  int                `json:"active"`
	PortLimit               int32              `json:"port_limit"`
	SnapshotLimit           int32              `json:"snapshot_limit"`
	BackupLimit             int32              `json:"backup_limit"`
	SiteLimit               int32              `json:"site_limit"`
	MonthlyTrafficLimit     *string            `json:"monthly_traffic_limit"`
	TrafficResetPrice       float64            `json:"traffic_reset_price"`
	IOLimitMode             string             `json:"io_limit_mode"`
	LimitsRead              string             `json:"limits_read"`
	LimitsWrite             string             `json:"limits_write"`
	LimitsReadIops          int32              `json:"limits_read_iops"`
	LimitsWriteIops         int32              `json:"limits_write_iops"`
	LimitsIngress           string             `json:"limits_ingress"`
	LimitsEgress            string             `json:"limits_egress"`
	LimitsProcesses         int32              `json:"limits_processes"`
	LimitsCPUPriority       int32              `json:"limits_cpu_priority"`
	BootAutostart           bool               `json:"boot_autostart"`
	BootAutostartPriority   int32              `json:"boot_autostart_priority"`
	BootAutostartDelay      int32              `json:"boot_autostart_delay"`
	BootHostShutdownTimeout int32              `json:"boot_host_shutdown_timeout"`
	GlobalShared            bool               `json:"global_shared"`
	GlobalQuotaMultiplier   *float64           `json:"global_quota_multiplier"`
	GlobalMaxInstances      *int32             `json:"global_max_instances,omitempty"`
	RequiredPackageID       *int32             `json:"required_package_id"`
	RequiredPackageName     *string            `json:"required_package_name"`
	AllowInstanceDeletion   bool               `json:"allow_instance_deletion"`
	CreatedAt               string             `json:"created_at"`
	HostIDs                 []int32            `json:"host_ids"`
	HostStoragePools        map[string]*string `json:"host_storage_pools"`
	HostTrafficMultipliers  map[string]float64 `json:"host_traffic_multipliers"`
}

// PackageBasicRow mirrors getPackageByName/getPackageByUserAndName's return
// shape (the `Package` interface of types/database.ts, without traffic_reset_price
// and without the global/required extras the TypeScript does not set there).
type PackageBasicRow struct {
	ID                      int32   `json:"id"`
	UserID                  int32   `json:"user_id"`
	Name                    string  `json:"name"`
	Description             *string `json:"description"`
	CPUMax                  int32   `json:"cpu_max"`
	MemoryMax               int32   `json:"memory_max"`
	DiskMax                 int32   `json:"disk_max"`
	BandwidthMax            *int32  `json:"bandwidth_max"`
	NetworkMode             string  `json:"network_mode"`
	InstanceType            string  `json:"instance_type"`
	EgressMode              string  `json:"egress_mode"`
	EgressProfileID         *int32  `json:"egress_profile_id"`
	GPUShared               bool    `json:"gpu_shared"`
	GPUDeviceIDs            []int32 `json:"gpu_device_ids"`
	Privileged              int     `json:"privileged"`
	Nested                  int     `json:"nested"`
	Active                  int     `json:"active"`
	PortLimit               int32   `json:"port_limit"`
	SnapshotLimit           int32   `json:"snapshot_limit"`
	BackupLimit             int32   `json:"backup_limit"`
	SiteLimit               int32   `json:"site_limit"`
	MonthlyTrafficLimit     *string `json:"monthly_traffic_limit"`
	IOLimitMode             string  `json:"io_limit_mode"`
	LimitsRead              string  `json:"limits_read"`
	LimitsWrite             string  `json:"limits_write"`
	LimitsReadIops          int32   `json:"limits_read_iops"`
	LimitsWriteIops         int32   `json:"limits_write_iops"`
	LimitsIngress           string  `json:"limits_ingress"`
	LimitsEgress            string  `json:"limits_egress"`
	LimitsProcesses         int32   `json:"limits_processes"`
	LimitsCPUPriority       int32   `json:"limits_cpu_priority"`
	BootAutostart           bool    `json:"boot_autostart"`
	BootAutostartPriority   int32   `json:"boot_autostart_priority"`
	BootAutostartDelay      int32   `json:"boot_autostart_delay"`
	BootHostShutdownTimeout int32   `json:"boot_host_shutdown_timeout"`
	RequiredPackageID       *int32  `json:"required_package_id"`
	AllowInstanceDeletion   bool    `json:"allow_instance_deletion"`
	CreatedAt               string  `json:"created_at"`
}

// PackageCreateData mirrors createPackage's `data` object. Optional scalars use
// pointers; on create an absent field and an explicit null collapse to the same
// default, exactly like the TypeScript's `?? default`.
type PackageCreateData struct {
	UserID                  int32
	Name                    string
	Description             *string
	CPUMax                  int32
	MemoryMax               int32
	DiskMax                 int32
	BandwidthMax            *int32
	NetworkMode             *string
	InstanceType            *string
	HostIDs                 []int32
	HostStoragePools        map[string]*string
	HostTrafficMultipliers  map[string]any
	EgressMode              *string
	EgressProfileID         *int32
	GPUShared               *bool
	GPUDeviceIDs            []int32
	Privileged              *bool
	Nested                  *bool
	Active                  *bool
	PortLimit               *int32
	SnapshotLimit           *int32
	BackupLimit             *int32
	SiteLimit               *int32
	MonthlyTrafficLimit     *int64
	TrafficResetPrice       *float64
	IOLimitMode             *string
	LimitsRead              *string
	LimitsWrite             *string
	LimitsReadIops          *int32
	LimitsWriteIops         *int32
	LimitsIngress           *string
	LimitsEgress            *string
	LimitsProcesses         *int32
	LimitsCPUPriority       *int32
	BootAutostart           *bool
	BootAutostartPriority   *int32
	BootAutostartDelay      *int32
	BootHostShutdownTimeout *int32
	GlobalShared            *bool
	GlobalQuotaMultiplier   *float64
	GlobalMaxInstances      *int32
	RequiredPackageID       *int32
	AllowInstanceDeletion   *bool
}

// PackageUpdateData mirrors updatePackage's `data` object.
type PackageUpdateData struct {
	Name                    PackageField[string]
	Description             PackageField[string]
	CPUMax                  PackageField[int32]
	MemoryMax               PackageField[int32]
	DiskMax                 PackageField[int32]
	BandwidthMax            PackageField[int32]
	NetworkMode             PackageField[string]
	InstanceType            PackageField[string]
	HostIDs                 []int32
	HostStoragePools        map[string]*string
	HostTrafficMultipliers  map[string]any
	EgressMode              PackageField[string]
	EgressProfileID         PackageField[int32]
	GPUShared               PackageField[bool]
	GPUDeviceIDs            PackageField[[]int32]
	Privileged              PackageField[bool]
	Nested                  PackageField[bool]
	Active                  PackageField[bool]
	PortLimit               PackageField[int32]
	SnapshotLimit           PackageField[int32]
	BackupLimit             PackageField[int32]
	SiteLimit               PackageField[int32]
	MonthlyTrafficLimit     PackageField[int64]
	TrafficResetPrice       PackageField[float64]
	IOLimitMode             PackageField[string]
	LimitsRead              PackageField[string]
	LimitsWrite             PackageField[string]
	LimitsReadIops          PackageField[int32]
	LimitsWriteIops         PackageField[int32]
	LimitsIngress           PackageField[string]
	LimitsEgress            PackageField[string]
	LimitsProcesses         PackageField[int32]
	LimitsCPUPriority       PackageField[int32]
	BootAutostart           PackageField[bool]
	BootAutostartPriority   PackageField[int32]
	BootAutostartDelay      PackageField[int32]
	BootHostShutdownTimeout PackageField[int32]
	GlobalShared            PackageField[bool]
	GlobalQuotaMultiplier   PackageField[float64]
	GlobalMaxInstances      PackageField[int32]
	RequiredPackageID       PackageField[int32]
	AllowInstanceDeletion   PackageField[bool]
}

// ==================== row projections ====================

type packageRow struct {
	ID                      int32          `gorm:"column:id"`
	UserID                  int32          `gorm:"column:user_id"`
	Name                    string         `gorm:"column:name"`
	Description             *string        `gorm:"column:description"`
	CPUMax                  int32          `gorm:"column:cpu_max"`
	MemoryMax               int32          `gorm:"column:memory_max"`
	DiskMax                 int32          `gorm:"column:disk_max"`
	BandwidthMax            *int32         `gorm:"column:bandwidth_max"`
	NetworkMode             string         `gorm:"column:network_mode"`
	InstanceType            string         `gorm:"column:instance_type"`
	EgressMode              string         `gorm:"column:egress_mode"`
	EgressProfileID         *int32         `gorm:"column:egress_profile_id"`
	GPUShared               bool           `gorm:"column:gpu_shared"`
	GPUDeviceIDsJSON        *string        `gorm:"column:gpu_device_ids_json"`
	Privileged              bool           `gorm:"column:privileged"`
	Nested                  bool           `gorm:"column:nested"`
	Active                  bool           `gorm:"column:active"`
	PortLimit               int32          `gorm:"column:port_limit"`
	SnapshotLimit           int32          `gorm:"column:snapshot_limit"`
	BackupLimit             int32          `gorm:"column:backup_limit"`
	SiteLimit               int32          `gorm:"column:site_limit"`
	MonthlyTrafficLimit     *int64         `gorm:"column:monthly_traffic_limit"`
	TrafficResetPrice       models.Decimal `gorm:"column:traffic_reset_price"`
	IOLimitMode             string         `gorm:"column:io_limit_mode"`
	LimitsRead              string         `gorm:"column:limits_read"`
	LimitsWrite             string         `gorm:"column:limits_write"`
	LimitsReadIops          int32          `gorm:"column:limits_read_iops"`
	LimitsWriteIops         int32          `gorm:"column:limits_write_iops"`
	LimitsIngress           string         `gorm:"column:limits_ingress"`
	LimitsEgress            string         `gorm:"column:limits_egress"`
	LimitsProcesses         int32          `gorm:"column:limits_processes"`
	LimitsCPUPriority       int32          `gorm:"column:limits_cpu_priority"`
	BootAutostart           bool           `gorm:"column:boot_autostart"`
	BootAutostartPriority   int32          `gorm:"column:boot_autostart_priority"`
	BootAutostartDelay      int32          `gorm:"column:boot_autostart_delay"`
	BootHostShutdownTimeout int32          `gorm:"column:boot_host_shutdown_timeout"`
	GlobalShared            bool           `gorm:"column:global_shared"`
	GlobalQuotaMultiplier   *string        `gorm:"column:global_quota_multiplier"`
	GlobalMaxInstances      *int32         `gorm:"column:global_max_instances"`
	RequiredPackageID       *int32         `gorm:"column:required_package_id"`
	RequiredPackageName     *string        `gorm:"column:required_package_name"`
	AllowInstanceDeletion   bool           `gorm:"column:allow_instance_deletion"`
	CreatedAt               time.Time      `gorm:"column:created_at"`
	OwnerUsername           *string        `gorm:"column:owner_username"`
	OwnerEmail              *string        `gorm:"column:owner_email"`
	OwnerAvatarStyle        *string        `gorm:"column:owner_avatar_style"`
	OwnerAvatarBadgeID      *string        `gorm:"column:owner_avatar_badge_id"`
}

// packageSelectColumns is the projection shared by getAllPackages and
// getPackageById. `array_to_json(...)::text` avoids GORM's slice handling (the
// same workaround as db/gpus.go), and enum columns are cast to text so they scan
// into string.
const packageSelectColumns = `p.id, p.user_id, p.name, p.description, p.cpu_max, p.memory_max, p.disk_max,
	p.bandwidth_max, p.network_mode::text AS network_mode, p.instance_type::text AS instance_type,
	p.egress_mode, p.egress_profile_id, p.gpu_shared,
	array_to_json(p.gpu_device_ids)::text AS gpu_device_ids_json,
	p.privileged, p.nested, p.active, p.port_limit, p.snapshot_limit, p.backup_limit, p.site_limit,
	p.monthly_traffic_limit, p.traffic_reset_price,
	p.io_limit_mode, p.limits_read, p.limits_write, p.limits_read_iops, p.limits_write_iops,
	p.limits_ingress, p.limits_egress, p.limits_processes, p.limits_cpu_priority,
	p.boot_autostart, p.boot_autostart_priority, p.boot_autostart_delay, p.boot_host_shutdown_timeout,
	p.global_shared, p.global_quota_multiplier::text AS global_quota_multiplier, p.global_max_instances,
	p.required_package_id, rp.name AS required_package_name,
	p.allow_instance_deletion, p.created_at,
	u.username AS owner_username, u.email AS owner_email,
	u.avatar_style AS owner_avatar_style, u.avatar_badge_id AS owner_avatar_badge_id`

const packageSelectFrom = ` FROM packages p
	LEFT JOIN users u ON u.id = p.user_id
	LEFT JOIN packages rp ON rp.id = p.required_package_id`

type packageHostRow struct {
	PackageID         int32          `gorm:"column:package_id"`
	HostID            int32          `gorm:"column:host_id"`
	StoragePoolName   *string        `gorm:"column:storage_pool_name"`
	TrafficMultiplier models.Decimal `gorm:"column:traffic_multiplier"`
}

// ==================== helpers ====================

// packageClient mirrors `client: Prisma.TransactionClient | typeof prisma`.
func packageClient(client *gorm.DB) (*gorm.DB, error) {
	if client != nil {
		return client, nil
	}
	if DB == nil {
		return nil, ErrPackageDatabaseNotInitialized
	}
	return DB, nil
}

// packageISOTime mirrors Date.prototype.toISOString() (UTC, millisecond precision).
func packageISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// packageDecimalFromFloat mirrors Prisma's number → Decimal conversion: the
// shortest round-trip decimal representation of the double.
func packageDecimalFromFloat(value float64) models.Decimal {
	return models.Decimal{Decimal: decimal.NewFromFloat(value)}
}

// packageDecimalPtrFromFloat mirrors `number | null` → `Decimal?`.
func packageDecimalPtrFromFloat(value *float64) *models.Decimal {
	if value == nil {
		return nil
	}
	converted := packageDecimalFromFloat(*value)
	return &converted
}

// packageDecimalFloat converts a Decimal column to the JS number the wire shape
// uses (`Number(pkg.trafficResetPrice)`).
func packageDecimalFloat(value models.Decimal) float64 {
	result, _ := value.Decimal.Float64()
	return result
}

// packageInt32ArrayLiteral renders a Postgres int4[] literal ("{1,2,3}"). GORM
// expands slice arguments into one placeholder per element, which cannot be
// assigned to an array column, so array parameters are passed as a literal with
// an explicit ::int4[] cast.
func packageInt32ArrayLiteral(values []int32) string {
	if len(values) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.FormatInt(int64(value), 10))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// packageJSONInt32Array parses `array_to_json(integer[])::text`.
func packageJSONInt32Array(raw *string) []int32 {
	if raw == nil || *raw == "" || *raw == "null" {
		return []int32{}
	}
	var values []int32
	if err := json.Unmarshal([]byte(*raw), &values); err != nil || values == nil {
		return []int32{}
	}
	return values
}

// packageJSNumber mirrors `Number(text)` for the host-id object keys a caller
// can send. It returns ok=false for NaN.
func packageJSNumber(text string) (float64, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, true
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(parsed) {
		return 0, false
	}
	return parsed, true
}

// packageCanonicalHostKey mirrors `String(Number(hostIdKey))`: JavaScript object
// keys are canonicalised through the numeric value ("01" → "1").
func packageCanonicalHostKey(raw string) (string, error) {
	number, ok := packageJSNumber(raw)
	if !ok {
		return "", fmt.Errorf("Invalid hostStoragePools key: %s", raw)
	}
	return strconv.FormatFloat(number, 'f', -1, 64), nil
}

// packageHostKey renders a host id the way JavaScript's String(hostId) does.
func packageHostKey(hostID int32) string {
	return strconv.FormatInt(int64(hostID), 10)
}

// packageHostKeyInt parses a canonical host key back to the int32 Prisma's
// composite key requires. A non-integral key cannot address a binding.
func packageHostKeyInt(key string) (int32, error) {
	number, ok := packageJSNumber(key)
	if !ok || number != math.Trunc(number) || number < math.MinInt32 || number > math.MaxInt32 {
		return 0, fmt.Errorf("Invalid host key: %s", key)
	}
	return int32(number), nil
}

// packageNormalizeHostStoragePools mirrors normalizeHostStoragePools.
func packageNormalizeHostStoragePools(hostStoragePools map[string]*string) (map[string]*string, error) {
	normalized := make(map[string]*string, len(hostStoragePools))
	for rawKey, poolName := range hostStoragePools {
		key, err := packageCanonicalHostKey(rawKey)
		if err != nil {
			return nil, fmt.Errorf("Invalid hostStoragePools key: %s", rawKey)
		}
		var trimmed *string
		if poolName != nil {
			value := strings.TrimSpace(*poolName)
			if value != "" {
				trimmed = &value
			}
		}
		normalized[key] = trimmed
	}
	return normalized, nil
}

// packageNormalizeHostTrafficMultipliers mirrors normalizeHostTrafficMultipliers.
// The multiplier values go through the db-local copy of
// normalizeTrafficMultiplier (db must not import lib).
func packageNormalizeHostTrafficMultipliers(hostTrafficMultipliers map[string]any) (map[string]float64, error) {
	normalized := make(map[string]float64, len(hostTrafficMultipliers))
	for rawKey, multiplier := range hostTrafficMultipliers {
		key, err := packageCanonicalHostKey(rawKey)
		if err != nil {
			return nil, fmt.Errorf("Invalid hostTrafficMultipliers key: %s", rawKey)
		}
		value, err := NormalizeTrafficMultiplier(multiplier)
		if err != nil {
			return nil, err
		}
		normalized[key] = value
	}
	return normalized, nil
}

// packageValidateHostTrafficMultipliers mirrors validateHostTrafficMultipliers.
func packageValidateHostTrafficMultipliers(hostIDs []int32, hostTrafficMultipliers map[string]float64) error {
	allowed := make(map[int32]bool, len(hostIDs))
	for _, hostID := range hostIDs {
		allowed[hostID] = true
	}
	for key := range hostTrafficMultipliers {
		number, ok := packageJSNumber(key)
		if !ok {
			return fmt.Errorf("Traffic multiplier mapping references an unbound host: %s", key)
		}
		hostID := int32(number)
		if number != math.Trunc(number) || !allowed[hostID] {
			return fmt.Errorf("Traffic multiplier mapping references an unbound host: %s", key)
		}
	}
	return nil
}

// packageResolveHostStoragePool mirrors the per-host decision inside
// resolvePackageHostStoragePools:
//
//	requestedHasHost ? (requestedPool || preferred) : (currentPool || preferred)
func packageResolveHostStoragePool(hostID int32, requestedHasHost bool, requestedPool, currentPool, preferredPool *string, active bool) (*string, error) {
	pick := func(values ...*string) *string {
		for _, value := range values {
			if value != nil && *value != "" {
				return value
			}
		}
		return nil
	}

	var poolName *string
	if requestedHasHost {
		poolName = pick(requestedPool, preferredPool)
	} else {
		poolName = pick(currentPool, preferredPool)
	}
	if active && poolName == nil {
		return nil, fmt.Errorf("Active package must bind a system disk storage pool on host %d", hostID)
	}
	return poolName, nil
}

// packageGetPreferredSystemDiskPoolsByHost mirrors getPreferredSystemDiskPoolsByHost:
// the first instance_data pool per host, with 'default' preferred when present.
func packageGetPreferredSystemDiskPoolsByHost(ctx context.Context, client *gorm.DB, hostIDs []int32) (map[int32]string, error) {
	preferred := make(map[int32]string, len(hostIDs))
	if len(hostIDs) == 0 {
		return preferred, nil
	}

	var pools []struct {
		HostID int32  `gorm:"column:host_id"`
		Name   string `gorm:"column:name"`
	}
	// `orderBy: [{ hostId: asc }, { createdAt: asc }]`; id is added as a stable
	// tie-break for rows created in the same millisecond.
	if err := client.WithContext(ctx).Raw(
		`SELECT host_id, name FROM storage_pools
		  WHERE host_id IN ? AND purpose = 'instance_data'
		  ORDER BY host_id ASC, created_at ASC, id ASC`, hostIDs,
	).Scan(&pools).Error; err != nil {
		return nil, err
	}

	for _, pool := range pools {
		// `if (!existing || pool.name === DEFAULT_STORAGE_POOL_NAME)`
		if _, hasExisting := preferred[pool.HostID]; !hasExisting || pool.Name == packageDefaultStoragePoolName {
			preferred[pool.HostID] = pool.Name
		}
	}
	return preferred, nil
}

// packageValidateHostStoragePools mirrors validateHostStoragePools: every named
// pool must exist on that host with purpose='instance_data'.
func packageValidateHostStoragePools(ctx context.Context, client *gorm.DB, hostIDs []int32, hostStoragePools map[string]*string) error {
	allowed := make(map[int32]bool, len(hostIDs))
	for _, hostID := range hostIDs {
		allowed[hostID] = true
	}

	type requestedPool struct {
		hostID int32
		name   string
	}
	requested := make([]requestedPool, 0, len(hostStoragePools))
	for key, poolName := range hostStoragePools {
		number, ok := packageJSNumber(key)
		if !ok {
			return fmt.Errorf("Storage pool mapping references an unbound host: %s", key)
		}
		hostID := int32(number)
		if number != math.Trunc(number) || !allowed[hostID] {
			return fmt.Errorf("Storage pool mapping references an unbound host: %s", key)
		}
		if poolName != nil && *poolName != "" {
			requested = append(requested, requestedPool{hostID: hostID, name: *poolName})
		}
	}
	if len(requested) == 0 {
		return nil
	}

	// Prisma builds one OR branch per requested (hostId, name, purpose) triple.
	clauses := make([]string, 0, len(requested))
	args := make([]any, 0, len(requested)*2)
	for _, pool := range requested {
		clauses = append(clauses, "(host_id = ? AND name = ?)")
		args = append(args, pool.hostID, pool.name)
	}
	var matched []struct {
		HostID int32  `gorm:"column:host_id"`
		Name   string `gorm:"column:name"`
	}
	if err := client.WithContext(ctx).Raw(
		`SELECT host_id, name FROM storage_pools
		  WHERE purpose = 'instance_data' AND (`+strings.Join(clauses, " OR ")+`)`, args...,
	).Scan(&matched).Error; err != nil {
		return err
	}

	matchedSet := make(map[string]bool, len(matched))
	for _, pool := range matched {
		matchedSet[packageHostKey(pool.HostID)+":"+pool.Name] = true
	}
	for _, pool := range requested {
		if !matchedSet[packageHostKey(pool.hostID)+":"+pool.name] {
			return fmt.Errorf("Storage pool %q is not available as a system disk pool on host %d", pool.name, pool.hostID)
		}
	}
	return nil
}

// packageResolvePackageHostStoragePools mirrors resolvePackageHostStoragePools.
func packageResolvePackageHostStoragePools(
	ctx context.Context,
	client *gorm.DB,
	hostIDs []int32,
	requested map[string]*string,
	current map[int32]*string,
	active bool,
) (map[string]*string, error) {
	preferredPools, err := packageGetPreferredSystemDiskPoolsByHost(ctx, client, hostIDs)
	if err != nil {
		return nil, err
	}

	resolved := make(map[string]*string, len(hostIDs))
	for _, hostID := range hostIDs {
		key := packageHostKey(hostID)
		requestedPool, requestedHasHost := requested[key]
		currentPool := current[hostID]

		var preferred *string
		if value, ok := preferredPools[hostID]; ok {
			preferred = &value
		}
		poolName, err := packageResolveHostStoragePool(hostID, requestedHasHost, requestedPool, currentPool, preferred, active)
		if err != nil {
			return nil, err
		}
		resolved[key] = poolName
	}

	if err := packageValidateHostStoragePools(ctx, client, hostIDs, resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

// packageBuildHostStoragePoolMap mirrors buildHostStoragePoolMap.
func packageBuildHostStoragePoolMap(packageHosts []packageHostRow) map[string]*string {
	result := make(map[string]*string, len(packageHosts))
	for _, binding := range packageHosts {
		result[packageHostKey(binding.HostID)] = binding.StoragePoolName
	}
	return result
}

// packageBuildHostTrafficMultiplierMap mirrors buildHostTrafficMultiplierMap.
func packageBuildHostTrafficMultiplierMap(packageHosts []packageHostRow) (map[string]float64, error) {
	result := make(map[string]float64, len(packageHosts))
	for _, binding := range packageHosts {
		multiplier, err := NormalizeTrafficMultiplier(binding.TrafficMultiplier)
		if err != nil {
			return nil, err
		}
		result[packageHostKey(binding.HostID)] = multiplier
	}
	return result, nil
}

// packageLoadHostBindings mirrors `include: { packageHosts: { select: ... } }`.
func packageLoadHostBindings(ctx context.Context, client *gorm.DB, packageIDs []int32) (map[int32][]packageHostRow, error) {
	bindings := make(map[int32][]packageHostRow, len(packageIDs))
	if len(packageIDs) == 0 {
		return bindings, nil
	}
	rows := make([]packageHostRow, 0, len(packageIDs))
	if err := client.WithContext(ctx).Raw(
		`SELECT package_id, host_id, storage_pool_name, traffic_multiplier
		   FROM package_hosts WHERE package_id IN ? ORDER BY id ASC`, packageIDs,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		bindings[row.PackageID] = append(bindings[row.PackageID], row)
	}
	return bindings, nil
}

// packageListFromRow maps a packageRow to getAllPackages' wire shape.
func packageListFromRow(row packageRow, bindings []packageHostRow, options PackageListOptions) (PackageListRow, error) {
	pools := packageBuildHostStoragePoolMap(bindings)
	multipliers, err := packageBuildHostTrafficMultiplierMap(bindings)
	if err != nil {
		return PackageListRow{}, err
	}
	hostIDs := make([]int32, 0, len(bindings))
	for _, binding := range bindings {
		hostIDs = append(hostIDs, binding.HostID)
	}

	result := PackageListRow{
		ID:                      row.ID,
		UserID:                  row.UserID,
		Name:                    row.Name,
		Description:             row.Description,
		CPUMax:                  row.CPUMax,
		MemoryMax:               row.MemoryMax,
		DiskMax:                 row.DiskMax,
		BandwidthMax:            row.BandwidthMax,
		NetworkMode:             row.NetworkMode,
		InstanceType:            row.InstanceType,
		EgressMode:              row.EgressMode,
		EgressProfileID:         row.EgressProfileID,
		GPUShared:               row.GPUShared,
		GPUDeviceIDs:            packageJSONInt32Array(row.GPUDeviceIDsJSON),
		Privileged:              packageBoolInt(row.Privileged),
		Nested:                  packageBoolInt(row.Nested),
		Active:                  packageBoolInt(row.Active),
		PortLimit:               row.PortLimit,
		SnapshotLimit:           row.SnapshotLimit,
		BackupLimit:             row.BackupLimit,
		SiteLimit:               row.SiteLimit,
		MonthlyTrafficLimit:     packageBigIntString(row.MonthlyTrafficLimit),
		TrafficResetPrice:       packageDecimalFloat(row.TrafficResetPrice),
		IOLimitMode:             row.IOLimitMode,
		LimitsRead:              row.LimitsRead,
		LimitsWrite:             row.LimitsWrite,
		LimitsReadIops:          row.LimitsReadIops,
		LimitsWriteIops:         row.LimitsWriteIops,
		LimitsIngress:           row.LimitsIngress,
		LimitsEgress:            row.LimitsEgress,
		LimitsProcesses:         row.LimitsProcesses,
		LimitsCPUPriority:       row.LimitsCPUPriority,
		BootAutostart:           row.BootAutostart,
		BootAutostartPriority:   row.BootAutostartPriority,
		BootAutostartDelay:      row.BootAutostartDelay,
		BootHostShutdownTimeout: row.BootHostShutdownTimeout,
		GlobalShared:            row.GlobalShared,
		GlobalQuotaMultiplier:   packageDecimalPtrFromText(row.GlobalQuotaMultiplier),
		GlobalMaxInstances:      row.GlobalMaxInstances,
		RequiredPackageID:       row.RequiredPackageID,
		RequiredPackageName:     row.RequiredPackageName,
		AllowInstanceDeletion:   row.AllowInstanceDeletion,
		CreatedAt:               packageISOTime(row.CreatedAt),
		OwnerUsername:           row.OwnerUsername,
		IsOwn:                   true,
		HostIDs:                 hostIDs,
		HostStoragePools:        pools,
		HostTrafficMultipliers:  multipliers,
	}
	if options.IncludeOwnerEmail {
		// `includeOwnerEmail ? pkg.user?.email : undefined` — a missing user or a
		// null email must serialise as null, not vanish, once the caller asked for
		// the owner email. A typed nil pointer inside `any` marshals as null while
		// an untyped nil is omitted by omitempty.
		if row.OwnerEmail != nil {
			result.OwnerEmail = *row.OwnerEmail
		} else {
			result.OwnerEmail = (*string)(nil)
		}
		if row.OwnerAvatarStyle != nil {
			result.OwnerAvatarStyle = *row.OwnerAvatarStyle
		} else {
			result.OwnerAvatarStyle = (*string)(nil)
		}
		if row.OwnerAvatarBadgeID != nil {
			result.OwnerAvatarBadgeID = *row.OwnerAvatarBadgeID
		} else {
			result.OwnerAvatarBadgeID = (*string)(nil)
		}
	}
	return result, nil
}

// packageDetailFromRow maps a packageRow to getPackageById's wire shape.
func packageDetailFromRow(row packageRow, bindings []packageHostRow) (PackageDetailRow, error) {
	pools := packageBuildHostStoragePoolMap(bindings)
	multipliers, err := packageBuildHostTrafficMultiplierMap(bindings)
	if err != nil {
		return PackageDetailRow{}, err
	}
	hostIDs := make([]int32, 0, len(bindings))
	for _, binding := range bindings {
		hostIDs = append(hostIDs, binding.HostID)
	}

	return PackageDetailRow{
		ID:                      row.ID,
		UserID:                  row.UserID,
		Name:                    row.Name,
		Description:             row.Description,
		CPUMax:                  row.CPUMax,
		MemoryMax:               row.MemoryMax,
		DiskMax:                 row.DiskMax,
		BandwidthMax:            row.BandwidthMax,
		NetworkMode:             row.NetworkMode,
		InstanceType:            row.InstanceType,
		EgressMode:              row.EgressMode,
		EgressProfileID:         row.EgressProfileID,
		GPUShared:               row.GPUShared,
		GPUDeviceIDs:            packageJSONInt32Array(row.GPUDeviceIDsJSON),
		Privileged:              packageBoolInt(row.Privileged),
		Nested:                  packageBoolInt(row.Nested),
		Active:                  packageBoolInt(row.Active),
		PortLimit:               row.PortLimit,
		SnapshotLimit:           row.SnapshotLimit,
		BackupLimit:             row.BackupLimit,
		SiteLimit:               row.SiteLimit,
		MonthlyTrafficLimit:     packageBigIntString(row.MonthlyTrafficLimit),
		TrafficResetPrice:       packageDecimalFloat(row.TrafficResetPrice),
		IOLimitMode:             row.IOLimitMode,
		LimitsRead:              row.LimitsRead,
		LimitsWrite:             row.LimitsWrite,
		LimitsReadIops:          row.LimitsReadIops,
		LimitsWriteIops:         row.LimitsWriteIops,
		LimitsIngress:           row.LimitsIngress,
		LimitsEgress:            row.LimitsEgress,
		LimitsProcesses:         row.LimitsProcesses,
		LimitsCPUPriority:       row.LimitsCPUPriority,
		BootAutostart:           row.BootAutostart,
		BootAutostartPriority:   row.BootAutostartPriority,
		BootAutostartDelay:      row.BootAutostartDelay,
		BootHostShutdownTimeout: row.BootHostShutdownTimeout,
		GlobalShared:            row.GlobalShared,
		GlobalQuotaMultiplier:   packageDecimalPtrFromText(row.GlobalQuotaMultiplier),
		GlobalMaxInstances:      row.GlobalMaxInstances,
		RequiredPackageID:       row.RequiredPackageID,
		RequiredPackageName:     row.RequiredPackageName,
		AllowInstanceDeletion:   row.AllowInstanceDeletion,
		CreatedAt:               packageISOTime(row.CreatedAt),
		HostIDs:                 hostIDs,
		HostStoragePools:        pools,
		HostTrafficMultipliers:  multipliers,
	}, nil
}

// packageBasicFromRow maps a packageRow to the `Package` wire shape.
func packageBasicFromRow(row packageRow) PackageBasicRow {
	return PackageBasicRow{
		ID:                      row.ID,
		UserID:                  row.UserID,
		Name:                    row.Name,
		Description:             row.Description,
		CPUMax:                  row.CPUMax,
		MemoryMax:               row.MemoryMax,
		DiskMax:                 row.DiskMax,
		BandwidthMax:            row.BandwidthMax,
		NetworkMode:             row.NetworkMode,
		InstanceType:            row.InstanceType,
		EgressMode:              row.EgressMode,
		EgressProfileID:         row.EgressProfileID,
		GPUShared:               row.GPUShared,
		GPUDeviceIDs:            packageJSONInt32Array(row.GPUDeviceIDsJSON),
		Privileged:              packageBoolInt(row.Privileged),
		Nested:                  packageBoolInt(row.Nested),
		Active:                  packageBoolInt(row.Active),
		PortLimit:               row.PortLimit,
		SnapshotLimit:           row.SnapshotLimit,
		BackupLimit:             row.BackupLimit,
		SiteLimit:               row.SiteLimit,
		MonthlyTrafficLimit:     packageBigIntString(row.MonthlyTrafficLimit),
		IOLimitMode:             row.IOLimitMode,
		LimitsRead:              row.LimitsRead,
		LimitsWrite:             row.LimitsWrite,
		LimitsReadIops:          row.LimitsReadIops,
		LimitsWriteIops:         row.LimitsWriteIops,
		LimitsIngress:           row.LimitsIngress,
		LimitsEgress:            row.LimitsEgress,
		LimitsProcesses:         row.LimitsProcesses,
		LimitsCPUPriority:       row.LimitsCPUPriority,
		BootAutostart:           row.BootAutostart,
		BootAutostartPriority:   row.BootAutostartPriority,
		BootAutostartDelay:      row.BootAutostartDelay,
		BootHostShutdownTimeout: row.BootHostShutdownTimeout,
		RequiredPackageID:       row.RequiredPackageID,
		AllowInstanceDeletion:   row.AllowInstanceDeletion,
		CreatedAt:               packageISOTime(row.CreatedAt),
	}
}

// packageBoolInt mirrors `value ? 1 : 0`.
func packageBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// packageBigIntString mirrors `bigint.toString()` / `?? null`.
func packageBigIntString(value *int64) *string {
	if value == nil {
		return nil
	}
	rendered := strconv.FormatInt(*value, 10)
	return &rendered
}

// packageDecimalPtrFromText mirrors `Decimal | null` → `number | null` for a
// numeric column read as text (the text form is the exact decimal, so the
// conversion matches `Number(Decimal)`).
func packageDecimalPtrFromText(value *string) *float64 {
	if value == nil {
		return nil
	}
	parsed, err := decimal.NewFromString(*value)
	if err != nil {
		return nil
	}
	converted, _ := parsed.Float64()
	return &converted
}

// ==================== queries ====================

// GetAllPackages mirrors getAllPackages.
func GetAllPackages(ctx context.Context, activeOnly bool, options PackageListOptions) ([]PackageListRow, error) {
	client, err := packageClient(nil)
	if err != nil {
		return nil, err
	}

	where := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if activeOnly {
		where = append(where, "p.active = ?")
		args = append(args, true)
	}
	// The TypeScript assigns whereCondition.userId twice; excludeUserId wins and
	// silently drops userId.
	if options.ExcludeUserID != nil && *options.ExcludeUserID != 0 {
		where = append(where, "p.user_id <> ?")
		args = append(args, *options.ExcludeUserID)
	} else if options.UserID != nil && *options.UserID != 0 {
		where = append(where, "p.user_id = ?")
		args = append(args, *options.UserID)
	}
	if options.OwnerRole != nil {
		where = append(where, "EXISTS (SELECT 1 FROM users u2 WHERE u2.id = p.user_id AND u2.role::text = ?)")
		args = append(args, *options.OwnerRole)
	}

	query := "SELECT " + packageSelectColumns + packageSelectFrom
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY p.id ASC"

	var rows []packageRow
	if err := client.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}

	packageIDs := make([]int32, 0, len(rows))
	for _, row := range rows {
		packageIDs = append(packageIDs, row.ID)
	}
	bindings, err := packageLoadHostBindings(ctx, client, packageIDs)
	if err != nil {
		return nil, err
	}

	result := make([]PackageListRow, 0, len(rows))
	for _, row := range rows {
		mapped, err := packageListFromRow(row, bindings[row.ID], options)
		if err != nil {
			return nil, err
		}
		result = append(result, mapped)
	}
	return result, nil
}

// GetPackageById mirrors getPackageById: (nil, nil) when the row is absent.
func GetPackageById(ctx context.Context, id int32) (*PackageDetailRow, error) {
	client, err := packageClient(nil)
	if err != nil {
		return nil, err
	}

	var rows []packageRow
	if err := client.WithContext(ctx).Raw(
		"SELECT "+packageSelectColumns+packageSelectFrom+" WHERE p.id = ? LIMIT 1", id,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	bindings, err := packageLoadHostBindings(ctx, client, []int32{id})
	if err != nil {
		return nil, err
	}
	mapped, err := packageDetailFromRow(rows[0], bindings[id])
	if err != nil {
		return nil, err
	}
	return &mapped, nil
}

// GetPackageByName mirrors getPackageByName. Prisma's findFirst without an
// orderBy returns an arbitrary row; id ASC makes the Go choice deterministic
// (names are only unique per owner).
func GetPackageByName(ctx context.Context, name string) (*PackageBasicRow, error) {
	client, err := packageClient(nil)
	if err != nil {
		return nil, err
	}
	var rows []packageRow
	if err := client.WithContext(ctx).Raw(
		"SELECT "+packageSelectColumns+packageSelectFrom+" WHERE p.name = ? ORDER BY p.id ASC LIMIT 1", name,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	mapped := packageBasicFromRow(rows[0])
	return &mapped, nil
}

// GetPackageByUserAndName mirrors getPackageByUserAndName (the @@unique([userId, name]) key).
func GetPackageByUserAndName(ctx context.Context, userID int32, name string) (*PackageBasicRow, error) {
	client, err := packageClient(nil)
	if err != nil {
		return nil, err
	}
	var rows []packageRow
	if err := client.WithContext(ctx).Raw(
		"SELECT "+packageSelectColumns+packageSelectFrom+
			" WHERE p.user_id = ? AND p.name = ? ORDER BY p.id ASC LIMIT 1", userID, name,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	mapped := packageBasicFromRow(rows[0])
	return &mapped, nil
}

// ==================== advisory locks ====================

// packageEgressAdvisoryLock takes the SINGLE-argument egress lock 74201, the
// same statement server/src/db/packages.ts issues before every package write.
// The one-argument form lives in a different lock space than
// AdvisoryTransactionLock's two-int form; do not "reuse" that helper.
func packageEgressAdvisoryLock(tx *gorm.DB) error {
	return tx.Exec("SELECT 1 FROM pg_advisory_xact_lock(?::bigint)", EgressAdvisoryLockNamespace).Error
}

// packagePrerequisiteLock mirrors acquirePackagePrerequisiteLock:
// `pg_advisory_xact_lock(PACKAGE_PREREQUISITE_LOCK_NAMESPACE, 1)`. It must run on
// the transaction that performs the chain validation, otherwise PostgreSQL
// releases it at the end of the implicit single-statement transaction.
func packagePrerequisiteLock(tx *gorm.DB) error {
	return tx.Exec(
		"WITH acquired_lock AS (SELECT pg_advisory_xact_lock(?, 1)) SELECT true AS locked FROM acquired_lock",
		PackagePrerequisiteLockNamespace,
	).Error
}

// ==================== prerequisite validation ====================

// packagePrerequisiteReader abstracts the one lookup the chain walk needs, so
// every rejection branch is unit-testable without a database.
type packagePrerequisiteReader interface {
	packageRequiredIDAndOwner(ctx context.Context, packageID int32) (requiredPackageID *int32, ownerUserID int32, found bool, err error)
}

type packageGormPrerequisiteReader struct {
	client *gorm.DB
}

func (reader packageGormPrerequisiteReader) packageRequiredIDAndOwner(ctx context.Context, packageID int32) (*int32, int32, bool, error) {
	var row struct {
		ID                int32  `gorm:"column:id"`
		RequiredPackageID *int32 `gorm:"column:required_package_id"`
		UserID            int32  `gorm:"column:user_id"`
	}
	if err := reader.client.WithContext(ctx).Raw(
		`SELECT id, required_package_id, user_id FROM packages WHERE id = ? LIMIT 1`, packageID,
	).Scan(&row).Error; err != nil {
		return nil, 0, false, err
	}
	if row.ID == 0 {
		return nil, 0, false, nil
	}
	return row.RequiredPackageID, row.UserID, true, nil
}

// packageValidatePrerequisiteReferenceWithClient mirrors
// validatePackagePrerequisiteReferenceWithClient.
func packageValidatePrerequisiteReferenceWithClient(
	ctx context.Context,
	reader packagePrerequisiteReader,
	packageID *int32,
	requiredPackageID *int32,
	ownerUserID *int32,
) (PackagePrerequisiteValidationResult, error) {
	if requiredPackageID == nil {
		return PackagePrerequisiteValidationResult{Valid: true}, nil
	}
	if *requiredPackageID <= 0 {
		return PackagePrerequisiteValidationResult{
			Valid:   false,
			Code:    "PACKAGE_PREREQUISITE_INVALID",
			Message: "Required package is invalid",
		}, nil
	}
	if packageID != nil && *requiredPackageID == *packageID {
		return PackagePrerequisiteValidationResult{
			Valid:   false,
			Code:    "PACKAGE_PREREQUISITE_SELF",
			Message: "Package cannot require itself",
		}, nil
	}

	cursor := requiredPackageID
	visited := make(map[int32]bool)
	for cursor != nil {
		if packageID != nil && *cursor == *packageID {
			return PackagePrerequisiteValidationResult{
				Valid:   false,
				Code:    "PACKAGE_PREREQUISITE_CYCLE",
				Message: "Package prerequisite cannot form a cycle",
			}, nil
		}
		if visited[*cursor] {
			return PackagePrerequisiteValidationResult{
				Valid:   false,
				Code:    "PACKAGE_PREREQUISITE_CYCLE",
				Message: "Package prerequisite cannot form a cycle",
			}, nil
		}
		visited[*cursor] = true

		nextRequired, owner, found, err := reader.packageRequiredIDAndOwner(ctx, *cursor)
		if err != nil {
			return PackagePrerequisiteValidationResult{}, err
		}
		if !found {
			return PackagePrerequisiteValidationResult{
				Valid:   false,
				Code:    "PACKAGE_PREREQUISITE_INVALID",
				Message: "Required package does not exist",
			}, nil
		}
		if ownerUserID != nil && owner != *ownerUserID {
			return PackagePrerequisiteValidationResult{
				Valid:   false,
				Code:    "PACKAGE_PREREQUISITE_SCOPE",
				Message: "Required package must belong to the same owner",
			}, nil
		}
		cursor = nextRequired
	}
	return PackagePrerequisiteValidationResult{Valid: true}, nil
}

// ValidatePackagePrerequisiteReference mirrors validatePackagePrerequisiteReference.
func ValidatePackagePrerequisiteReference(ctx context.Context, packageID *int32, requiredPackageID *int32, ownerUserID *int32) (PackagePrerequisiteValidationResult, error) {
	client, err := packageClient(nil)
	if err != nil {
		return PackagePrerequisiteValidationResult{}, err
	}
	return packageValidatePrerequisiteReferenceWithClient(ctx, packageGormPrerequisiteReader{client: client}, packageID, requiredPackageID, ownerUserID)
}

// ValidatePackagePrerequisiteReferenceLocked mirrors
// validatePackagePrerequisiteReferenceLocked: the whole chain walk runs under
// the 4201 lock at SERIALIZABLE isolation.
func ValidatePackagePrerequisiteReferenceLocked(ctx context.Context, packageID *int32, requiredPackageID *int32, ownerUserID *int32) (PackagePrerequisiteValidationResult, error) {
	client, err := packageClient(nil)
	if err != nil {
		return PackagePrerequisiteValidationResult{}, err
	}

	var result PackagePrerequisiteValidationResult
	err = client.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := packagePrerequisiteLock(tx); err != nil {
			return err
		}
		var err error
		result, err = packageValidatePrerequisiteReferenceWithClient(ctx, packageGormPrerequisiteReader{client: tx}, packageID, requiredPackageID, ownerUserID)
		return err
	}, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return PackagePrerequisiteValidationResult{}, err
	}
	return result, nil
}

// packageInsertColumns mirrors the `data` object of createPackage in table-column
// order.
var packageInsertColumns = []string{
	"egress_mode", "egress_profile_id", "user_id", "name", "description", "cpu_max", "memory_max", "disk_max",
	"bandwidth_max", "network_mode", "instance_type", "gpu_shared", "gpu_device_ids", "privileged", "nested", "active",
	"port_limit", "snapshot_limit", "backup_limit", "site_limit", "monthly_traffic_limit", "traffic_reset_price",
	"io_limit_mode", "limits_read", "limits_write", "limits_read_iops", "limits_write_iops",
	"limits_ingress", "limits_egress", "limits_processes", "limits_cpu_priority",
	"boot_autostart", "boot_autostart_priority", "boot_autostart_delay", "boot_host_shutdown_timeout",
	"global_shared", "global_quota_multiplier", "global_max_instances", "required_package_id",
	"allow_instance_deletion",
}

// packageInsertStatement is derived from packageInsertColumns.
var packageInsertStatement = packageBuildInsertStatement()

func packageBuildInsertStatement() string {
	placeholders := make([]string, len(packageInsertColumns))
	for index, column := range packageInsertColumns {
		if column == "gpu_device_ids" {
			// GORM expands a slice argument into one placeholder per element, which
			// cannot be assigned to an array column, so the array is passed as a
			// literal with an explicit cast.
			placeholders[index] = "?::int4[]"
			continue
		}
		placeholders[index] = "?"
	}
	return "INSERT INTO packages (" + strings.Join(packageInsertColumns, ", ") +
		") VALUES (" + strings.Join(placeholders, ", ") + ") RETURNING id"
}

// ==================== create ====================

// CreatePackage mirrors createPackage. The prerequisite path runs at
// SERIALIZABLE isolation; the plain path keeps the default isolation. Both take
// the egress lock 74201 first.
func CreatePackage(ctx context.Context, data PackageCreateData, isAdmin bool) (int32, error) {
	client, err := packageClient(nil)
	if err != nil {
		return 0, err
	}

	var createdID int32
	if data.RequiredPackageID != nil {
		err := client.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := packageEgressAdvisoryLock(tx); err != nil {
				return err
			}
			if err := packagePrerequisiteLock(tx); err != nil {
				return err
			}
			ownerID := data.UserID
			validation, err := packageValidatePrerequisiteReferenceWithClient(
				ctx, packageGormPrerequisiteReader{client: tx}, nil, data.RequiredPackageID, &ownerID,
			)
			if err != nil {
				return err
			}
			if !validation.Valid {
				return fmt.Errorf("%s: %s", validation.Code, validation.Message)
			}
			createdID, err = packageCreateUnchecked(ctx, tx, data, isAdmin)
			return err
		}, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return 0, err
		}
		return createdID, nil
	}

	err = client.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := packageEgressAdvisoryLock(tx); err != nil {
			return err
		}
		var err error
		createdID, err = packageCreateUnchecked(ctx, tx, data, isAdmin)
		return err
	})
	if err != nil {
		return 0, err
	}
	return createdID, nil
}

// packageCreateUnchecked mirrors createPackageUnchecked.
func packageCreateUnchecked(ctx context.Context, client *gorm.DB, data PackageCreateData, isAdmin bool) (int32, error) {
	if len(data.HostIDs) == 0 {
		return 0, ErrPackageHostBindingRequired
	}

	// Administrators may bind any host; owners may bind only their own.
	var hostIDs []int32
	hostQuery := `SELECT id FROM hosts WHERE id IN ?`
	hostArgs := []any{data.HostIDs}
	if !isAdmin {
		hostQuery += ` AND user_id = ?`
		hostArgs = append(hostArgs, data.UserID)
	}
	if err := client.WithContext(ctx).Raw(hostQuery, hostArgs...).Scan(&hostIDs).Error; err != nil {
		return 0, err
	}
	if len(hostIDs) != len(data.HostIDs) {
		if isAdmin {
			return 0, errors.New("Some hosts do not exist")
		}
		return 0, errors.New("All hosts must belong to the package owner")
	}

	normalizedPools, err := packageNormalizeHostStoragePools(data.HostStoragePools)
	if err != nil {
		return 0, err
	}
	normalizedMultipliers, err := packageNormalizeHostTrafficMultipliers(data.HostTrafficMultipliers)
	if err != nil {
		return 0, err
	}
	active := data.Active == nil || *data.Active
	resolvedPools, err := packageResolvePackageHostStoragePools(ctx, client, data.HostIDs, normalizedPools, nil, active)
	if err != nil {
		return 0, err
	}
	if err := packageValidateHostTrafficMultipliers(data.HostIDs, normalizedMultipliers); err != nil {
		return 0, err
	}

	gpuShared := data.GPUShared != nil && *data.GPUShared
	gpuDeviceIDs := data.GPUDeviceIDs
	if gpuDeviceIDs == nil {
		gpuDeviceIDs = []int32{}
	}
	instanceType := "container"
	if data.InstanceType != nil {
		instanceType = *data.InstanceType
	}
	privileged := data.Privileged != nil && *data.Privileged
	if err := GpusValidateSelection(ctx, client, GpusSelectionInput{
		GpuShared:    gpuShared,
		GpuDeviceIDs: gpuDeviceIDs,
		InstanceType: instanceType,
		Privileged:   privileged,
		HostIDs:      data.HostIDs,
	}); err != nil {
		return 0, err
	}

	if err := PackageEgressValidator(ctx, client, PackageEgressValidationInput{
		EgressMode:      data.EgressMode,
		EgressProfileID: data.EgressProfileID,
		NetworkMode:     data.NetworkMode,
		InstanceType:    data.InstanceType,
		Privileged:      privileged,
		HostIDs:         data.HostIDs,
	}); err != nil {
		return 0, err
	}

	egressMode := "host"
	if data.EgressMode != nil {
		egressMode = *data.EgressMode
	}
	networkMode := "nat"
	if data.NetworkMode != nil {
		networkMode = *data.NetworkMode
	}
	description := data.Description
	bandwidthMax := data.BandwidthMax
	trafficResetPrice := 0.0
	if data.TrafficResetPrice != nil {
		trafficResetPrice = *data.TrafficResetPrice
	}
	defaultString := func(value *string, fallback string) string {
		if value == nil {
			return fallback
		}
		return *value
	}
	defaultInt32 := func(value *int32, fallback int32) int32 {
		if value == nil {
			return fallback
		}
		return *value
	}
	defaultBool := func(value *bool, fallback bool) bool {
		if value == nil {
			return fallback
		}
		return *value
	}

	var createdID int32
	insertValues := map[string]any{
		"egress_mode":                egressMode,
		"egress_profile_id":          data.EgressProfileID,
		"user_id":                    data.UserID,
		"name":                       data.Name,
		"description":                description,
		"cpu_max":                    data.CPUMax,
		"memory_max":                 data.MemoryMax,
		"disk_max":                   data.DiskMax,
		"bandwidth_max":              bandwidthMax,
		"network_mode":               networkMode,
		"instance_type":              instanceType,
		"gpu_shared":                 gpuShared,
		"gpu_device_ids":             packageInt32ArrayLiteral(gpuDeviceIDs),
		"privileged":                 privileged,
		"nested":                     defaultBool(data.Nested, false),
		"active":                     active,
		"port_limit":                 defaultInt32(data.PortLimit, 20),
		"snapshot_limit":             defaultInt32(data.SnapshotLimit, 5),
		"backup_limit":               defaultInt32(data.BackupLimit, 3),
		"site_limit":                 defaultInt32(data.SiteLimit, 10),
		"monthly_traffic_limit":      data.MonthlyTrafficLimit,
		"traffic_reset_price":        packageDecimalFromFloat(trafficResetPrice),
		"io_limit_mode":              defaultString(data.IOLimitMode, "throughput"),
		"limits_read":                defaultString(data.LimitsRead, "100MB"),
		"limits_write":               defaultString(data.LimitsWrite, "100MB"),
		"limits_read_iops":           defaultInt32(data.LimitsReadIops, 500),
		"limits_write_iops":          defaultInt32(data.LimitsWriteIops, 500),
		"limits_ingress":             defaultString(data.LimitsIngress, "300Mbit"),
		"limits_egress":              defaultString(data.LimitsEgress, "300Mbit"),
		"limits_processes":           defaultInt32(data.LimitsProcesses, 500),
		"limits_cpu_priority":        defaultInt32(data.LimitsCPUPriority, 10),
		"boot_autostart":             defaultBool(data.BootAutostart, true),
		"boot_autostart_priority":    defaultInt32(data.BootAutostartPriority, 20),
		"boot_autostart_delay":       defaultInt32(data.BootAutostartDelay, 15),
		"boot_host_shutdown_timeout": defaultInt32(data.BootHostShutdownTimeout, 30),
		"global_shared":              defaultBool(data.GlobalShared, false),
		"global_quota_multiplier":    packageDecimalPtrFromFloat(data.GlobalQuotaMultiplier),
		"global_max_instances":       data.GlobalMaxInstances,
		"required_package_id":        data.RequiredPackageID,
		"allow_instance_deletion":    defaultBool(data.AllowInstanceDeletion, true),
	}
	// The statement and the argument list are both derived from
	// packageInsertColumns, so a column can never drift out of alignment with its
	// placeholder.
	insertArgs := make([]any, 0, len(packageInsertColumns))
	for _, column := range packageInsertColumns {
		insertArgs = append(insertArgs, insertValues[column])
	}
	err = client.WithContext(ctx).Raw(packageInsertStatement, insertArgs...).Scan(&createdID).Error
	if err != nil {
		return 0, err
	}

	// `packageHosts: { create: [...] }` — one binding per host, with the resolved
	// pool and the normalised multiplier (default 1).
	values := make([]string, 0, len(data.HostIDs))
	args := make([]any, 0, len(data.HostIDs)*4)
	for _, hostID := range data.HostIDs {
		multiplier := 1.0
		if value, ok := normalizedMultipliers[packageHostKey(hostID)]; ok {
			multiplier = value
		}
		values = append(values, "(?, ?, ?, ?)")
		args = append(args, createdID, hostID, resolvedPools[packageHostKey(hostID)], packageDecimalFromFloat(multiplier))
	}
	if err := client.WithContext(ctx).Exec(
		`INSERT INTO package_hosts (package_id, host_id, storage_pool_name, traffic_multiplier) VALUES `+
			strings.Join(values, ", "), args...,
	).Error; err != nil {
		return 0, err
	}
	return createdID, nil
}

// ==================== update ====================

// UpdatePackage mirrors updatePackage.
func UpdatePackage(ctx context.Context, id int32, data PackageUpdateData, isAdmin bool) error {
	client, err := packageClient(nil)
	if err != nil {
		return err
	}

	if data.RequiredPackageID.Present {
		return client.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := packageEgressAdvisoryLock(tx); err != nil {
				return err
			}
			if err := packagePrerequisiteLock(tx); err != nil {
				return err
			}

			var pkg struct {
				UserID int32 `gorm:"column:user_id"`
			}
			if err := tx.WithContext(ctx).Raw(
				`SELECT user_id FROM packages WHERE id = ? LIMIT 1`, id,
			).Scan(&pkg).Error; err != nil {
				return err
			}
			if pkg.UserID == 0 {
				return ErrPackageNotFound
			}

			validation, err := packageValidatePrerequisiteReferenceWithClient(
				ctx, packageGormPrerequisiteReader{client: tx}, &id, data.RequiredPackageID.Value, &pkg.UserID,
			)
			if err != nil {
				return err
			}
			if !validation.Valid {
				return fmt.Errorf("%s: %s", validation.Code, validation.Message)
			}
			return packageUpdateUnchecked(ctx, tx, id, data, isAdmin)
		}, &sql.TxOptions{Isolation: sql.LevelSerializable})
	}

	return client.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := packageEgressAdvisoryLock(tx); err != nil {
			return err
		}
		return packageUpdateUnchecked(ctx, tx, id, data, isAdmin)
	})
}

// packageCurrentPackage is the projection updatePackageUnchecked reads before
// validating the GPU/egress policy.
type packageCurrentPackage struct {
	ID              int32
	UserID          int32
	GPUShared       bool
	GPUDeviceIDs    []int32
	InstanceType    string
	Privileged      bool
	EgressMode      string
	EgressProfileID *int32
	NetworkMode     string
	Active          bool
	PackageHostIDs  []int32
	Plans           []gpusPolicyPlanRow
}

func packageLoadCurrentPackage(ctx context.Context, client *gorm.DB, id int32) (*packageCurrentPackage, error) {
	var row struct {
		ID              int32   `gorm:"column:id"`
		UserID          int32   `gorm:"column:user_id"`
		GPUShared       bool    `gorm:"column:gpu_shared"`
		GPUDeviceIDs    *string `gorm:"column:gpu_device_ids_json"`
		InstanceType    string  `gorm:"column:instance_type"`
		Privileged      bool    `gorm:"column:privileged"`
		EgressMode      string  `gorm:"column:egress_mode"`
		EgressProfileID *int32  `gorm:"column:egress_profile_id"`
		NetworkMode     string  `gorm:"column:network_mode"`
		Active          bool    `gorm:"column:active"`
	}
	if err := client.WithContext(ctx).Raw(
		`SELECT id, user_id, gpu_shared, array_to_json(gpu_device_ids)::text AS gpu_device_ids_json,
		        instance_type::text AS instance_type, privileged, egress_mode, egress_profile_id,
		        network_mode::text AS network_mode, active
		   FROM packages WHERE id = ? LIMIT 1`, id,
	).Scan(&row).Error; err != nil {
		return nil, err
	}
	if row.ID == 0 {
		return nil, nil
	}

	var plans []gpusPolicyPlanRow
	if err := client.WithContext(ctx).Raw(
		`SELECT id, package_id, gpu_shared, array_to_json(gpu_device_ids)::text AS gpu_device_ids_json,
		        is_active, is_sold_out
		   FROM package_plans WHERE package_id = ? ORDER BY id ASC`, id,
	).Scan(&plans).Error; err != nil {
		return nil, err
	}

	var bindings []packageHostRow
	if err := client.WithContext(ctx).Raw(
		`SELECT package_id, host_id, storage_pool_name, traffic_multiplier
		   FROM package_hosts WHERE package_id = ? ORDER BY id ASC`, id,
	).Scan(&bindings).Error; err != nil {
		return nil, err
	}
	hostIDs := make([]int32, 0, len(bindings))
	for _, binding := range bindings {
		hostIDs = append(hostIDs, binding.HostID)
	}

	return &packageCurrentPackage{
		ID:              row.ID,
		UserID:          row.UserID,
		GPUShared:       row.GPUShared,
		GPUDeviceIDs:    packageJSONInt32Array(row.GPUDeviceIDs),
		InstanceType:    row.InstanceType,
		Privileged:      row.Privileged,
		EgressMode:      row.EgressMode,
		EgressProfileID: row.EgressProfileID,
		NetworkMode:     row.NetworkMode,
		Active:          row.Active,
		PackageHostIDs:  hostIDs,
		Plans:           plans,
	}, nil
}

// packageUpdateUnchecked mirrors updatePackageUnchecked.
func packageUpdateUnchecked(ctx context.Context, client *gorm.DB, id int32, data PackageUpdateData, isAdmin bool) error {
	current, err := packageLoadCurrentPackage(ctx, client, id)
	if err != nil {
		return err
	}
	if current == nil {
		return ErrPackageNotFound
	}

	// `data.gpuShared ?? current.gpuShared` etc. — an explicit null falls back.
	nextPolicy := GpusPolicy{GpuShared: current.GPUShared, GpuDeviceIDs: current.GPUDeviceIDs}
	if data.GPUShared.Present && data.GPUShared.Value != nil {
		nextPolicy.GpuShared = *data.GPUShared.Value
	}
	if data.GPUDeviceIDs.Present && data.GPUDeviceIDs.Value != nil {
		nextPolicy.GpuDeviceIDs = *data.GPUDeviceIDs.Value
	}
	nextInstanceType := current.InstanceType
	if data.InstanceType.Present && data.InstanceType.Value != nil {
		nextInstanceType = *data.InstanceType.Value
	}
	nextPrivileged := current.Privileged
	if data.Privileged.Present && data.Privileged.Value != nil {
		nextPrivileged = *data.Privileged.Value
	}
	gpuHostIDs := current.PackageHostIDs
	if data.HostIDs != nil {
		gpuHostIDs = data.HostIDs
	}

	if err := GpusValidateSelection(ctx, client, GpusSelectionInput{
		GpuShared:    nextPolicy.GpuShared,
		GpuDeviceIDs: nextPolicy.GpuDeviceIDs,
		InstanceType: nextInstanceType,
		Privileged:   nextPrivileged,
		HostIDs:      gpuHostIDs,
	}); err != nil {
		return err
	}
	for index := range current.Plans {
		policy := gpusResolvePolicy(nextPolicy, &current.Plans[index])
		if err := GpusValidateSelection(ctx, client, GpusSelectionInput{
			GpuShared:    policy.GpuShared,
			GpuDeviceIDs: policy.GpuDeviceIDs,
			InstanceType: nextInstanceType,
			Privileged:   nextPrivileged,
			HostIDs:      gpuHostIDs,
		}); err != nil {
			return err
		}
	}

	egressMode := current.EgressMode
	if data.EgressMode.Present && data.EgressMode.Value != nil {
		egressMode = *data.EgressMode.Value
	}
	egressProfileID := current.EgressProfileID
	if data.EgressProfileID.Present {
		egressProfileID = data.EgressProfileID.Value
	}
	networkMode := current.NetworkMode
	if data.NetworkMode.Present && data.NetworkMode.Value != nil {
		networkMode = *data.NetworkMode.Value
	}
	instanceTypeForEgress := current.InstanceType
	if data.InstanceType.Present && data.InstanceType.Value != nil {
		instanceTypeForEgress = *data.InstanceType.Value
	}
	privilegedForEgress := current.Privileged
	if data.Privileged.Present && data.Privileged.Value != nil {
		privilegedForEgress = *data.Privileged.Value
	}
	if err := PackageEgressValidator(ctx, client, PackageEgressValidationInput{
		EgressMode:      &egressMode,
		EgressProfileID: egressProfileID,
		NetworkMode:     &networkMode,
		InstanceType:    &instanceTypeForEgress,
		Privileged:      privilegedForEgress,
		HostIDs:         gpuHostIDs,
	}); err != nil {
		return err
	}

	// Build the package UPDATE from the present fields only. An empty update is
	// skipped entirely: Prisma would otherwise run an UPDATE for nothing.
	sets := make([]string, 0, 24)
	args := make([]any, 0, 24)
	addString := func(column string, field PackageField[string]) {
		if field.Present {
			sets = append(sets, column+" = ?")
			args = append(args, field.Value)
		}
	}
	addInt32 := func(column string, field PackageField[int32]) {
		if field.Present {
			sets = append(sets, column+" = ?")
			args = append(args, field.Value)
		}
	}
	addBool := func(column string, field PackageField[bool]) {
		if field.Present {
			sets = append(sets, column+" = ?")
			args = append(args, field.Value)
		}
	}

	addString("name", data.Name)
	addString("description", data.Description)
	addInt32("cpu_max", data.CPUMax)
	addInt32("memory_max", data.MemoryMax)
	addInt32("disk_max", data.DiskMax)
	addInt32("bandwidth_max", data.BandwidthMax)
	if data.NetworkMode.Present {
		sets = append(sets, "network_mode = ?")
		args = append(args, data.NetworkMode.Value)
	}
	if data.InstanceType.Present {
		sets = append(sets, "instance_type = ?")
		args = append(args, data.InstanceType.Value)
	}
	addString("egress_mode", data.EgressMode)
	if data.EgressProfileID.Present {
		sets = append(sets, "egress_profile_id = ?")
		args = append(args, data.EgressProfileID.Value)
	}
	addBool("gpu_shared", data.GPUShared)
	if data.GPUDeviceIDs.Present {
		deviceIDs := []int32{}
		if data.GPUDeviceIDs.Value != nil {
			deviceIDs = *data.GPUDeviceIDs.Value
		}
		sets = append(sets, "gpu_device_ids = ?::int4[]")
		args = append(args, packageInt32ArrayLiteral(deviceIDs))
	}
	addBool("privileged", data.Privileged)
	addBool("nested", data.Nested)
	addBool("active", data.Active)
	addInt32("port_limit", data.PortLimit)
	addInt32("snapshot_limit", data.SnapshotLimit)
	addInt32("backup_limit", data.BackupLimit)
	addInt32("site_limit", data.SiteLimit)
	if data.MonthlyTrafficLimit.Present {
		sets = append(sets, "monthly_traffic_limit = ?")
		args = append(args, data.MonthlyTrafficLimit.Value)
	}
	if data.TrafficResetPrice.Present {
		sets = append(sets, "traffic_reset_price = ?")
		if data.TrafficResetPrice.Value == nil {
			args = append(args, nil)
		} else {
			args = append(args, packageDecimalFromFloat(*data.TrafficResetPrice.Value))
		}
	}
	addString("io_limit_mode", data.IOLimitMode)
	addString("limits_read", data.LimitsRead)
	addString("limits_write", data.LimitsWrite)
	addInt32("limits_read_iops", data.LimitsReadIops)
	addInt32("limits_write_iops", data.LimitsWriteIops)
	addString("limits_ingress", data.LimitsIngress)
	addString("limits_egress", data.LimitsEgress)
	addInt32("limits_processes", data.LimitsProcesses)
	addInt32("limits_cpu_priority", data.LimitsCPUPriority)
	addBool("boot_autostart", data.BootAutostart)
	addInt32("boot_autostart_priority", data.BootAutostartPriority)
	addInt32("boot_autostart_delay", data.BootAutostartDelay)
	addInt32("boot_host_shutdown_timeout", data.BootHostShutdownTimeout)
	addBool("global_shared", data.GlobalShared)
	if data.GlobalQuotaMultiplier.Present {
		sets = append(sets, "global_quota_multiplier = ?")
		args = append(args, packageDecimalPtrFromFloat(data.GlobalQuotaMultiplier.Value))
	}
	if data.GlobalMaxInstances.Present {
		sets = append(sets, "global_max_instances = ?")
		args = append(args, data.GlobalMaxInstances.Value)
	}
	if data.RequiredPackageID.Present {
		sets = append(sets, "required_package_id = ?")
		args = append(args, data.RequiredPackageID.Value)
	}
	addBool("allow_instance_deletion", data.AllowInstanceDeletion)

	explicitActiveTrue := data.Active.Present && data.Active.Value != nil && *data.Active.Value
	needsPackageHostUpdate := data.HostIDs != nil || data.HostStoragePools != nil ||
		data.HostTrafficMultipliers != nil || explicitActiveTrue
	if needsPackageHostUpdate {
		if err := packageUpdateHostBindings(ctx, client, id, data, isAdmin); err != nil {
			return err
		}
	}

	if len(sets) > 0 {
		query := "UPDATE packages SET " + strings.Join(sets, ", ") + " WHERE id = ?"
		args = append(args, id)
		result := client.WithContext(ctx).Exec(query, args...)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrPackageRecordNotFound
		}
	}
	return nil
}

// packageUpdateHostBindings mirrors the `needsPackageHostUpdate` block of
// updatePackageUnchecked.
func packageUpdateHostBindings(ctx context.Context, client *gorm.DB, id int32, data PackageUpdateData, isAdmin bool) error {
	var pkg struct {
		UserID int32 `gorm:"column:user_id"`
		Active bool  `gorm:"column:active"`
	}
	var bindings []packageHostRow
	if err := client.WithContext(ctx).Raw(
		`SELECT user_id, active FROM packages WHERE id = ? LIMIT 1`, id,
	).Scan(&pkg).Error; err != nil {
		return err
	}
	if pkg.UserID == 0 {
		return ErrPackageNotFound
	}
	if err := client.WithContext(ctx).Raw(
		`SELECT package_id, host_id, storage_pool_name, traffic_multiplier
		   FROM package_hosts WHERE package_id = ? ORDER BY id ASC`, id,
	).Scan(&bindings).Error; err != nil {
		return err
	}

	currentPools := make(map[int32]*string, len(bindings))
	currentMultipliers := make(map[int32]float64, len(bindings))
	effectiveHostIDs := make([]int32, 0, len(bindings))
	for _, binding := range bindings {
		currentPools[binding.HostID] = binding.StoragePoolName
		multiplier, err := NormalizeTrafficMultiplier(binding.TrafficMultiplier)
		if err != nil {
			return err
		}
		currentMultipliers[binding.HostID] = multiplier
		effectiveHostIDs = append(effectiveHostIDs, binding.HostID)
	}
	if data.HostIDs != nil {
		effectiveHostIDs = data.HostIDs
	}
	if len(effectiveHostIDs) == 0 {
		return ErrPackageHostBindingRequired
	}

	if data.HostIDs != nil {
		var hostIDs []int32
		hostQuery := `SELECT id FROM hosts WHERE id IN ?`
		hostArgs := []any{effectiveHostIDs}
		if !isAdmin {
			hostQuery += ` AND user_id = ?`
			hostArgs = append(hostArgs, pkg.UserID)
		}
		if err := client.WithContext(ctx).Raw(hostQuery, hostArgs...).Scan(&hostIDs).Error; err != nil {
			return err
		}
		if len(hostIDs) != len(effectiveHostIDs) {
			if isAdmin {
				return errors.New("Some hosts do not exist")
			}
			return errors.New("All hosts must belong to the package owner")
		}
	}

	normalizedPools, err := packageNormalizeHostStoragePools(data.HostStoragePools)
	if err != nil {
		return err
	}
	normalizedMultipliers, err := packageNormalizeHostTrafficMultipliers(data.HostTrafficMultipliers)
	if err != nil {
		return err
	}

	active := pkg.Active
	if data.Active.Present && data.Active.Value != nil {
		active = *data.Active.Value
	}
	resolvedPools, err := packageResolvePackageHostStoragePools(ctx, client, effectiveHostIDs, normalizedPools, currentPools, active)
	if err != nil {
		return err
	}
	if err := packageValidateHostTrafficMultipliers(effectiveHostIDs, normalizedMultipliers); err != nil {
		return err
	}

	if data.HostIDs != nil {
		if err := client.WithContext(ctx).Exec(
			`DELETE FROM package_hosts WHERE package_id = ?`, id,
		).Error; err != nil {
			return err
		}

		values := make([]string, 0, len(effectiveHostIDs))
		args := make([]any, 0, len(effectiveHostIDs)*4)
		for _, hostID := range effectiveHostIDs {
			multiplier := 1.0
			if value, ok := normalizedMultipliers[packageHostKey(hostID)]; ok {
				multiplier = value
			} else if value, ok := currentMultipliers[hostID]; ok {
				multiplier = value
			}
			values = append(values, "(?, ?, ?, ?)")
			args = append(args, id, hostID, resolvedPools[packageHostKey(hostID)], packageDecimalFromFloat(multiplier))
		}
		return client.WithContext(ctx).Exec(
			`INSERT INTO package_hosts (package_id, host_id, storage_pool_name, traffic_multiplier) VALUES `+
				strings.Join(values, ", "), args...,
		).Error
	}

	explicitActiveTrue := data.Active.Present && data.Active.Value != nil && *data.Active.Value
	shouldUpdateStoragePools := data.HostStoragePools != nil || explicitActiveTrue
	poolsToPersist := normalizedPools
	if shouldUpdateStoragePools {
		poolsToPersist = resolvedPools
	}
	poolKeys := make([]string, 0, len(poolsToPersist))
	for key := range poolsToPersist {
		poolKeys = append(poolKeys, key)
	}
	sort.Strings(poolKeys)
	for _, key := range poolKeys {
		hostID, err := packageHostKeyInt(key)
		if err != nil {
			return err
		}
		if err := packageHostsUpdateStoragePool(client, id, hostID, poolsToPersist[key]); err != nil {
			return err
		}
	}

	multiplierKeys := make([]string, 0, len(normalizedMultipliers))
	for key := range normalizedMultipliers {
		multiplierKeys = append(multiplierKeys, key)
	}
	sort.Strings(multiplierKeys)
	for _, key := range multiplierKeys {
		hostID, err := packageHostKeyInt(key)
		if err != nil {
			return err
		}
		if err := packageHostsUpdateTrafficMultiplier(client, id, hostID, packageDecimalFromFloat(normalizedMultipliers[key])); err != nil {
			return err
		}
	}
	return nil
}

// ==================== delete / counts ====================

// DeletePackage mirrors deletePackage. Prisma's delete throws P2025 when the row
// does not exist, so zero affected rows is an error.
func DeletePackage(ctx context.Context, id int32) error {
	client, err := packageClient(nil)
	if err != nil {
		return err
	}
	result := client.WithContext(ctx).Exec(`DELETE FROM packages WHERE id = ?`, id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrPackageRecordNotFound
	}
	return nil
}

// GetInstanceCountByPackage mirrors getInstanceCountByPackage.
func GetInstanceCountByPackage(ctx context.Context, packageID int32) (int64, error) {
	client, err := packageClient(nil)
	if err != nil {
		return 0, err
	}
	var count int64
	if err := client.WithContext(ctx).Raw(
		`SELECT count(*) FROM instances WHERE package_id = ? AND status <> 'deleted'`, packageID,
	).Scan(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// GetNormalInstanceCountsByPackages mirrors getNormalInstanceCountsByPackages.
func GetNormalInstanceCountsByPackages(ctx context.Context, packageIDs []int32) (map[int32]int64, error) {
	client, err := packageClient(nil)
	if err != nil {
		return nil, err
	}

	seen := make(map[int32]bool, len(packageIDs))
	validPackageIDs := make([]int32, 0, len(packageIDs))
	for _, id := range packageIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		validPackageIDs = append(validPackageIDs, id)
	}
	result := make(map[int32]int64)
	if len(validPackageIDs) == 0 {
		return result, nil
	}

	var plans []struct {
		ID        int32 `gorm:"column:id"`
		PackageID int32 `gorm:"column:package_id"`
	}
	if err := client.WithContext(ctx).Raw(
		`SELECT id, package_id FROM package_plans WHERE package_id IN ?`, validPackageIDs,
	).Scan(&plans).Error; err != nil {
		return nil, err
	}
	planPackage := make(map[int32]int32, len(plans))
	planIDs := make([]int32, 0, len(plans))
	for _, plan := range plans {
		planPackage[plan.ID] = plan.PackageID
		planIDs = append(planIDs, plan.ID)
	}

	var directRows []struct {
		PackageID int32 `gorm:"column:package_id"`
		Count     int64 `gorm:"column:count"`
	}
	if err := client.WithContext(ctx).Raw(
		`SELECT package_id, count(*) AS count FROM instances
		  WHERE package_id IN ? AND status IN ?
		  GROUP BY package_id`, validPackageIDs, packageNormalInstanceStatuses,
	).Scan(&directRows).Error; err != nil {
		return nil, err
	}
	for _, row := range directRows {
		result[row.PackageID] = row.Count
	}

	if len(planIDs) == 0 {
		return result, nil
	}
	var planOnlyRows []struct {
		PackagePlanID int32 `gorm:"column:package_plan_id"`
		Count         int64 `gorm:"column:count"`
	}
	if err := client.WithContext(ctx).Raw(
		`SELECT package_plan_id, count(*) AS count FROM instances
		  WHERE package_id IS NULL AND package_plan_id IN ? AND status IN ?
		  GROUP BY package_plan_id`, planIDs, packageNormalInstanceStatuses,
	).Scan(&planOnlyRows).Error; err != nil {
		return nil, err
	}
	for _, row := range planOnlyRows {
		packageID, ok := planPackage[row.PackagePlanID]
		if !ok {
			continue
		}
		result[packageID] += row.Count
	}
	return result, nil
}

// UserHasNormalInstanceForPackage mirrors userHasNormalInstanceForPackage.
func UserHasNormalInstanceForPackage(ctx context.Context, userID, packageID int32, client *gorm.DB) (bool, error) {
	query, err := packageClient(client)
	if err != nil {
		return false, err
	}
	var count int64
	if err := query.WithContext(ctx).Raw(
		`SELECT count(*) FROM instances i
		  WHERE i.user_id = ? AND i.status IN ?
		    AND (i.package_id = ? OR EXISTS (
		          SELECT 1 FROM package_plans pp WHERE pp.id = i.package_plan_id AND pp.package_id = ?))`,
		userID, packageNormalInstanceStatuses, packageID, packageID,
	).Scan(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// GetPackagesDependingOnPackage mirrors getPackagesDependingOnPackage.
func GetPackagesDependingOnPackage(ctx context.Context, packageID int32) ([]PackageDependency, error) {
	client, err := packageClient(nil)
	if err != nil {
		return nil, err
	}
	result := make([]PackageDependency, 0, 4)
	if err := client.WithContext(ctx).Raw(
		`SELECT id, name FROM packages WHERE required_package_id = ? ORDER BY id ASC`, packageID,
	).Scan(&result).Error; err != nil {
		return nil, err
	}
	return result, nil
}

// ==================== host lookups ====================

// GetIncompatiblePackagesByHostTypeChange mirrors
// getIncompatiblePackagesByHostTypeChange.
func GetIncompatiblePackagesByHostTypeChange(ctx context.Context, hostID int32, newInstanceType string) ([]PackageIncompatible, error) {
	if newInstanceType == "both" {
		return []PackageIncompatible{}, nil
	}
	client, err := packageClient(nil)
	if err != nil {
		return nil, err
	}

	var rows []struct {
		ID           int32  `gorm:"column:id"`
		Name         string `gorm:"column:name"`
		InstanceType string `gorm:"column:instance_type"`
	}
	if err := client.WithContext(ctx).Raw(
		`SELECT p.id, p.name, p.instance_type::text AS instance_type
		   FROM package_hosts ph
		   JOIN packages p ON p.id = ph.package_id
		  WHERE ph.host_id = ?
		  ORDER BY ph.id ASC`, hostID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	result := make([]PackageIncompatible, 0, len(rows))
	for _, row := range rows {
		if row.InstanceType == newInstanceType || row.InstanceType == "both" {
			continue
		}
		result = append(result, PackageIncompatible{ID: row.ID, Name: row.Name, InstanceType: row.InstanceType})
	}
	return result, nil
}

// GetPackageHostsDetail mirrors getPackageHostsDetail.
func GetPackageHostsDetail(ctx context.Context, packageID int32) ([]PackageHostDetail, error) {
	client, err := packageClient(nil)
	if err != nil {
		return nil, err
	}
	result := make([]PackageHostDetail, 0, 4)
	if err := client.WithContext(ctx).Raw(
		`SELECT h.id, h.name, h.country_code, h.cpu_allowance_max, h.memory_max, h.cpu_used, h.memory_used
		   FROM package_hosts ph
		   JOIN hosts h ON h.id = ph.host_id
		  WHERE ph.package_id = ?
		  ORDER BY ph.id ASC`, packageID,
	).Scan(&result).Error; err != nil {
		return nil, err
	}
	return result, nil
}

// IncreaseHostQuota mirrors increaseHostQuota.
func IncreaseHostQuota(ctx context.Context, hostID int32, cpuAdd, memoryAdd int32) (PackageQuotaIncrease, error) {
	client, err := packageClient(nil)
	if err != nil {
		return PackageQuotaIncrease{}, err
	}

	var result PackageQuotaIncrease
	row := client.WithContext(ctx).Raw(
		`UPDATE hosts
		    SET cpu_allowance_max = cpu_allowance_max + ?, memory_max = memory_max + ?, updated_at = ?
		  WHERE id = ?
		  RETURNING cpu_allowance_max, memory_max`,
		cpuAdd, memoryAdd, time.Now().UTC(), hostID,
	).Scan(&result)
	if row.Error != nil {
		return PackageQuotaIncrease{}, row.Error
	}
	if row.RowsAffected == 0 {
		return PackageQuotaIncrease{}, ErrPackageRecordNotFound
	}
	return result, nil
}

// ==================== sold-out checks ====================

type packageSoldOutHost struct {
	ID              int32  `gorm:"column:id"`
	Status          string `gorm:"column:status"`
	CPUAllowanceMax int32  `gorm:"column:cpu_allowance_max"`
	MemoryMax       int32  `gorm:"column:memory_max"`
	StorageSize     int32  `gorm:"column:storage_size"`
}

type packageSoldOutInstance struct {
	HostID int32 `gorm:"column:host_id"`
	CPU    int32 `gorm:"column:cpu"`
	Memory int32 `gorm:"column:memory"`
	Disk   int32 `gorm:"column:disk"`
}

type packageSoldOutPlan struct {
	PackageID int32 `gorm:"column:package_id"`
	CPU       int32 `gorm:"column:cpu"`
	Memory    int32 `gorm:"column:memory"`
	Disk      int32 `gorm:"column:disk"`
	IsSoldOut bool  `gorm:"column:is_sold_out"`
}

// packageLoadSoldOutHosts mirrors the packageHost.findMany(...include.host...)
// shape both sold-out checks use.
func packageLoadSoldOutHosts(ctx context.Context, client *gorm.DB, packageIDs []int32) (map[int32][]packageSoldOutHost, map[int32][]packageSoldOutInstance, error) {
	hostsByPackage := make(map[int32][]packageSoldOutHost, len(packageIDs))
	instancesByHost := make(map[int32][]packageSoldOutInstance)

	var bindings []struct {
		PackageID int32 `gorm:"column:package_id"`
		HostID    int32 `gorm:"column:host_id"`
	}
	if err := client.WithContext(ctx).Raw(
		`SELECT package_id, host_id FROM package_hosts WHERE package_id IN ? ORDER BY id ASC`, packageIDs,
	).Scan(&bindings).Error; err != nil {
		return nil, nil, err
	}
	if len(bindings) == 0 {
		return hostsByPackage, instancesByHost, nil
	}

	hostIDs := make([]int32, 0, len(bindings))
	for _, binding := range bindings {
		hostIDs = append(hostIDs, binding.HostID)
	}
	var hosts []packageSoldOutHost
	if err := client.WithContext(ctx).Raw(
		`SELECT id, status::text AS status, cpu_allowance_max, memory_max, storage_size
		   FROM hosts WHERE id IN ?`, hostIDs,
	).Scan(&hosts).Error; err != nil {
		return nil, nil, err
	}
	hostByID := make(map[int32]packageSoldOutHost, len(hosts))
	for _, host := range hosts {
		hostByID[host.ID] = host
	}
	for _, binding := range bindings {
		host, ok := hostByID[binding.HostID]
		if !ok {
			continue
		}
		hostsByPackage[binding.PackageID] = append(hostsByPackage[binding.PackageID], host)
	}

	var instances []packageSoldOutInstance
	if err := client.WithContext(ctx).Raw(
		`SELECT host_id, cpu, memory, disk FROM instances
		  WHERE host_id IN ? AND status <> 'deleted'`, hostIDs,
	).Scan(&instances).Error; err != nil {
		return nil, nil, err
	}
	for _, instance := range instances {
		instancesByHost[instance.HostID] = append(instancesByHost[instance.HostID], instance)
	}
	return hostsByPackage, instancesByHost, nil
}

// packageSoldOutMinimums mirrors the minCpu/minMemory/minDisk block: paid
// packages use the smallest *available* plan, free packages use the create
// defaults 15% CPU / 128MB / 512MB. `soldOut` reports the
// "every active plan is manually sold out" case.
func packageSoldOutMinimums(plans []packageSoldOutPlan) (minCPU, minMemory, minDisk int32, soldOut bool) {
	if len(plans) == 0 {
		return 15, 128, 512, false
	}
	available := make([]packageSoldOutPlan, 0, len(plans))
	for _, plan := range plans {
		if !plan.IsSoldOut {
			available = append(available, plan)
		}
	}
	if len(available) == 0 {
		return 0, 0, 0, true
	}
	minCPU, minMemory, minDisk = available[0].CPU, available[0].Memory, available[0].Disk
	for _, plan := range available[1:] {
		if plan.CPU < minCPU {
			minCPU = plan.CPU
		}
		if plan.Memory < minMemory {
			minMemory = plan.Memory
		}
		if plan.Disk < minDisk {
			minDisk = plan.Disk
		}
	}
	return minCPU, minMemory, minDisk, false
}

// packageHostCanSell mirrors the per-host capacity predicate shared by both
// sold-out checks.
func packageHostCanSell(
	ctx context.Context,
	host packageSoldOutHost,
	instances []packageSoldOutInstance,
	packageID int32,
	gpuOffers []GpusPolicy,
	minCPU, minMemory, minDisk int32,
	availablePlans []packageSoldOutPlan,
) (bool, error) {
	availableOnHost, err := GpusOffersAvailableOnHost(ctx, host.ID, gpuOffers)
	if err != nil {
		return false, err
	}
	if !availableOnHost {
		return false, nil
	}
	if host.Status != "online" {
		return false, nil
	}

	storage, err := PackageStorageCapacityResolver(ctx, host.ID, &packageID, int64(minDisk))
	if err != nil {
		return false, err
	}
	if storage.Reason != nil {
		return false, nil
	}

	if host.CPUAllowanceMax == 0 || host.MemoryMax == 0 {
		return false, nil
	}

	var cpuUsed, memoryUsed int32
	for _, instance := range instances {
		cpuUsed += instance.CPU
		memoryUsed += instance.Memory
	}
	cpuAvailable := host.CPUAllowanceMax - cpuUsed
	memoryAvailable := host.MemoryMax - memoryUsed
	diskAvailable := storage.AvailableMB

	if cpuAvailable < minCPU || memoryAvailable < minMemory || diskAvailable < int64(minDisk) {
		return false, nil
	}
	if len(availablePlans) == 0 {
		return true, nil
	}
	for _, plan := range availablePlans {
		if cpuAvailable >= plan.CPU && memoryAvailable >= plan.Memory && diskAvailable >= int64(plan.Disk) {
			return true, nil
		}
	}
	return false, nil
}

// CheckPackageSoldOut mirrors checkPackageSoldOut.
func CheckPackageSoldOut(ctx context.Context, packageID int32) (bool, error) {
	client, err := packageClient(nil)
	if err != nil {
		return false, err
	}

	gpuOffersByPackage, err := GpusOffersForPackages(ctx, []int32{packageID})
	if err != nil {
		return false, err
	}
	gpuOffers := gpuOffersByPackage[packageID]
	if gpuOffers == nil {
		gpuOffers = []GpusPolicy{}
	}

	hostsByPackage, instancesByHost, err := packageLoadSoldOutHosts(ctx, client, []int32{packageID})
	if err != nil {
		return false, err
	}
	packageHosts := hostsByPackage[packageID]
	// No bound host means the package can never be scheduled.
	if len(packageHosts) == 0 {
		return true, nil
	}

	var plans []packageSoldOutPlan
	if err := client.WithContext(ctx).Raw(
		`SELECT cpu, memory, disk, is_sold_out FROM package_plans
		  WHERE package_id = ? AND is_active = true
		  ORDER BY cpu ASC, memory ASC`, packageID,
	).Scan(&plans).Error; err != nil {
		return false, err
	}
	availablePlans := make([]packageSoldOutPlan, 0, len(plans))
	for _, plan := range plans {
		if !plan.IsSoldOut {
			availablePlans = append(availablePlans, plan)
		}
	}

	minCPU, minMemory, minDisk, soldOut := packageSoldOutMinimums(plans)
	if soldOut {
		return true, nil
	}

	for _, host := range packageHosts {
		canSell, err := packageHostCanSell(ctx, host, instancesByHost[host.ID], packageID, gpuOffers, minCPU, minMemory, minDisk, availablePlans)
		if err != nil {
			return false, err
		}
		if canSell {
			return false, nil
		}
	}
	return true, nil
}

// CheckPackagesSoldOut mirrors checkPackagesSoldOut: one pass over all bindings,
// plans and GPU offers, then the same predicate per package.
func CheckPackagesSoldOut(ctx context.Context, packageIDs []int32) (map[int32]bool, error) {
	result := make(map[int32]bool, len(packageIDs))
	if len(packageIDs) == 0 {
		return result, nil
	}
	client, err := packageClient(nil)
	if err != nil {
		return nil, err
	}

	gpuOffersByPackage, err := GpusOffersForPackages(ctx, packageIDs)
	if err != nil {
		return nil, err
	}
	hostsByPackage, instancesByHost, err := packageLoadSoldOutHosts(ctx, client, packageIDs)
	if err != nil {
		return nil, err
	}

	var allPlans []packageSoldOutPlan
	if err := client.WithContext(ctx).Raw(
		`SELECT package_id, cpu, memory, disk, is_sold_out FROM package_plans
		  WHERE package_id IN ? AND is_active = true`, packageIDs,
	).Scan(&allPlans).Error; err != nil {
		return nil, err
	}
	plansByPackage := make(map[int32][]packageSoldOutPlan, len(packageIDs))
	for _, plan := range allPlans {
		plansByPackage[plan.PackageID] = append(plansByPackage[plan.PackageID], plan)
	}

	for _, packageID := range packageIDs {
		packageHosts := hostsByPackage[packageID]
		plans := plansByPackage[packageID]
		availablePlans := make([]packageSoldOutPlan, 0, len(plans))
		for _, plan := range plans {
			if !plan.IsSoldOut {
				availablePlans = append(availablePlans, plan)
			}
		}

		if len(packageHosts) == 0 {
			result[packageID] = true
			continue
		}
		minCPU, minMemory, minDisk, soldOut := packageSoldOutMinimums(plans)
		if soldOut {
			result[packageID] = true
			continue
		}

		gpuOffers := gpuOffersByPackage[packageID]
		if gpuOffers == nil {
			gpuOffers = []GpusPolicy{}
		}
		hasAvailableHost := false
		for _, host := range packageHosts {
			canSell, err := packageHostCanSell(ctx, host, instancesByHost[host.ID], packageID, gpuOffers, minCPU, minMemory, minDisk, availablePlans)
			if err != nil {
				return nil, err
			}
			if canSell {
				hasAvailableHost = true
				break
			}
		}
		result[packageID] = !hasAvailableHost
	}
	return result, nil
}
