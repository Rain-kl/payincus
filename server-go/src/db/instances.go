// Port of server/src/db/instances.ts (761 lines, 17 exports).
//
// The whole module is ported. Six rules shape the translation:
//
//  1. UTC. Every value that reaches SQL as a parameter is time.Now().UTC() (or a
//     caller-supplied instant converted with .UTC()). A bare time.Now() on this
//     UTC+8 host persists a wall clock eight hours in the future — WAVE_D_SPEC
//     §0.2b, measured. The only place a bare NOW() appears is where the
//     TypeScript itself wrote NOW() (there is none in this file).
//  2. updated_at. instances.updated_at is NOT NULL with no database default
//     (Prisma fills @updatedAt client-side), so CreateInstance must name the
//     column explicitly and every UPDATE must bump it. TestRawInsertsSetUpdatedAt
//     enforces the INSERT half against schema.prisma.
//  3. Tri-state. The TypeScript guards are `field !== undefined`, which
//     distinguishes "leave the column alone" from "write NULL". A bare *T cannot
//     express the second case, so nullable columns use InstanceOptional.
//  4. Prisma's update() vs updateMany(). update() throws P2025 when the row is
//     gone; updateMany() silently matches zero rows. UpdateInstanceStatus keeps
//     that split exactly: status 'deleted' is a single-row UPDATE that also sets
//     gpu_released_at (ErrInstanceRecordNotFound on zero rows), every other
//     target goes through the filtered UPDATE that matches zero rows without an
//     error.
//  5. Empty updates return early. `Object.keys(updateData).length === 0` guards
//     exist because Prisma would otherwise run an empty UPDATE that only bumps
//     @updatedAt. Every helper below keeps the early return, so an empty call
//     does not touch updated_at (and therefore does not disturb expiry/renewal
//     bookkeeping that compares timestamps).
//  6. Egress binding shares the transaction. bindPackageEgress
//     (server/src/lib/egress/package.ts) takes the *single-argument* advisory
//     lock 74201 first, resolves the source instance or the package, validates
//     the policy, and only then the row is inserted. It runs in the same
//     transaction as the INSERT, in the same order. db must not import lib (lib
//     imports db), so the one lib-only dependency — hostTunnelManager's
//     supportsControl — is a registration hook that fails CLOSED until lib
//     installs it: a non-host egress mode without proof the host supports
//     network.apply.v2 is rejected, never silently accepted.
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

// ==================== errors ====================

// ErrInstanceRecordNotFound mirrors Prisma's P2025 ("record to update not
// found"). Prisma's update() throws where updateMany() does not, so the callers
// that rely on the throwing form get this sentinel instead of a silent no-op.
var ErrInstanceRecordNotFound = errors.New("instances: record to update not found")

// InstanceEgressInputError mirrors lib/egress's NetworkInputError: the message is
// the literal string the TypeScript throws and the route layer returns verbatim.
type InstanceEgressInputError struct {
	Message string
}

// Error implements error.
func (e *InstanceEgressInputError) Error() string { return e.Message }

// InstanceEgressHostSupportsControl is installed by lib (the tunnel manager lives
// there; db must never import lib). It mirrors
// `hostTunnelManager.supportsControl(hostId, capability)`.
//
// Unregistered means "no evidence the host can apply egress": binding a non-host
// egress mode then fails closed with the same message the TypeScript produces
// when the agent is offline. Do not default it to true.
var InstanceEgressHostSupportsControl func(hostID int32, capability string) bool

// instanceEgressLockNamespace mirrors the egress binding lock. It is the
// SINGLE-argument form pg_advisory_xact_lock(bigint) — a different lock space from
// every (namespace, key) pair in advisory_locks.go — so it is not declared there
// (see EgressAdvisoryLockNamespace in egress.go, same value, same warning).
const instanceEgressLockNamespace int64 = 74201

// instanceEgressControlCapability mirrors the capability string bindPackageEgress
// requires before an instance may inherit a non-host egress policy.
const instanceEgressControlCapability = "network.apply.v2"

// ==================== tri-state helper ====================

// InstanceOptional is a tri-state update field: Set=false means "undefined, leave
// the column alone"; Set=true with the zero Value means "explicit NULL" for
// pointer element types. Prisma distinguishes the two and the database columns
// are nullable, so a bare *T would silently drop an explicit clear.
//
// It intentionally mirrors HostsOptional (db/hosts.go) instead of sharing it: the
// two modules live in files owned by different agents and a rename in either must
// not break the other.
type InstanceOptional[T any] struct {
	Set   bool
	Value T
}

// InstanceValue marks a field as explicitly provided.
func InstanceValue[T any](value T) InstanceOptional[T] {
	return InstanceOptional[T]{Set: true, Value: value}
}

// InstanceNull marks a nullable field as explicitly cleared.
func InstanceNull[T any]() InstanceOptional[T] {
	var zero T
	return InstanceOptional[T]{Set: true, Value: zero}
}

// ==================== payload types ====================

// InstanceNetworkUpdate mirrors the `network` argument of updateInstanceStatus:
// nil means "no network change", a present field means "write this value (NULL
// included)".
type InstanceNetworkUpdate struct {
	IPv4 InstanceOptional[*string]
	IPv6 InstanceOptional[*string]
}

// InstanceUpdate mirrors the `data` object of updateInstance. image / name /
// status and swapEnabled are `field?: T` in the TypeScript (never null), so a
// bare pointer is enough. Everything else is `field?: T | null` and needs the
// tri-state.
type InstanceUpdate struct {
	Image           *string
	Name            *string
	Status          *models.InstanceStatus
	SSHPort         InstanceOptional[*int32]
	RootPassword    InstanceOptional[*string]
	PortLimit       InstanceOptional[*int32]
	SnapshotLimit   InstanceOptional[*int32]
	BackupLimit     InstanceOptional[*int32]
	SiteLimit       InstanceOptional[*int32]
	SwapEnabled     *bool
	SwapSize        InstanceOptional[*int32]
	StoragePoolName InstanceOptional[*string]
}

// InstanceQuotaUpdate mirrors updateInstanceQuota's argument object: every field
// is `number | null` and optional.
type InstanceQuotaUpdate struct {
	PortLimit     InstanceOptional[*int32]
	SnapshotLimit InstanceOptional[*int32]
	BackupLimit   InstanceOptional[*int32]
	SiteLimit     InstanceOptional[*int32]
}

// InstanceResourcesUpdate mirrors updateInstanceResources' argument object.
// MonthlyTrafficLimit is BigInt → int64 and nullable.
type InstanceResourcesUpdate struct {
	CPU                 *int32
	Memory              *int32
	Disk                *int32
	MonthlyTrafficLimit InstanceOptional[*int64]
}

// InstanceConfigUpdate mirrors updateInstanceConfig's argument object.
type InstanceConfigUpdate struct {
	LimitsRead              InstanceOptional[*string]
	LimitsWrite             InstanceOptional[*string]
	LimitsReadIOps          InstanceOptional[*int32]
	LimitsWriteIOps         InstanceOptional[*int32]
	LimitsIngress           InstanceOptional[*string]
	LimitsEgress            InstanceOptional[*string]
	LimitsProcesses         InstanceOptional[*int32]
	LimitsCPUPriority       InstanceOptional[*int32]
	BootAutostart           InstanceOptional[*bool]
	BootAutostartPriority   InstanceOptional[*int32]
	BootAutostartDelay      InstanceOptional[*int32]
	BootHostShutdownTimeout InstanceOptional[*int32]
}

// InstanceEffectiveBandwidth mirrors the optional third argument of
// updateInstanceConfig. Ingress/Egress are written to the *effective* columns
// (limits_ingress / limits_egress), not the configured_* override columns.
type InstanceEffectiveBandwidth struct {
	Ingress *string
	Egress  *string
}

// InstanceSwapUpdate mirrors updateInstanceSwap's argument object.
type InstanceSwapUpdate struct {
	SwapEnabled *bool
	SwapSize    InstanceOptional[*int32]
}

// InstanceCreateInput mirrors createInstance's argument object.
type InstanceCreateInput struct {
	IncusID                string
	Name                   string
	UserID                 int32
	HostID                 int32
	PackageID              *int32
	StoragePoolName        *string
	Image                  string
	CPU                    int32
	Memory                 int32
	Disk                   int32
	NetworkMode            models.NetworkMode
	EgressSourceInstanceID *int32
	SnapshotSpecs          json.RawMessage
	SSHPort                *int32
	RootPassword           *string
	MonthlyTrafficLimit    *int64
	PortLimit              *int32
	SnapshotLimit          *int32
	BackupLimit            *int32
	SiteLimit              *int32
	SwapEnabled            *bool
	SwapSize               *int32
}

// InstanceEgressBinding mirrors EgressBinding ({egressMode, egressProfileId}).
type InstanceEgressBinding struct {
	EgressMode      string
	EgressProfileID *int32
}

// SuspendInstanceData mirrors suspendInstance's argument object. SuspendedBy nil
// means "system auto-suspend", SuspendReason 'expired' means expiry.
type SuspendInstanceData struct {
	SuspendedBy   *int32
	SuspendReason string
}

// ==================== wire records ====================
//
// Key order and key set follow the object literals in instances.ts character for
// character: JSON key order is insertion order in JavaScript, and the routes
// return these objects directly.

// InstanceHostedRecord mirrors getInstancesByUserId's element type
// (`Instance & {host_name?, host_location?, host_country_code?}`). host_location
// is `?? undefined`, so it disappears from the JSON when the host has no
// location — hence omitempty.
type InstanceHostedRecord struct {
	ID                      int32                 `json:"id"`
	IncusID                 string                `json:"incus_id"`
	Name                    string                `json:"name"`
	UserID                  int32                 `json:"user_id"`
	HostID                  int32                 `json:"host_id"`
	PackageID               *int32                `json:"package_id"`
	PackagePlanID           *int32                `json:"package_plan_id"`
	StoragePoolName         *string               `json:"storage_pool_name"`
	IconBadgeID             *string               `json:"icon_badge_id"`
	Image                   string                `json:"image"`
	Status                  models.InstanceStatus `json:"status"`
	CPU                     int32                 `json:"cpu"`
	Memory                  int32                 `json:"memory"`
	Disk                    int32                 `json:"disk"`
	IPv4                    *string               `json:"ipv4"`
	IPv6                    *string               `json:"ipv6"`
	NetworkMode             models.NetworkMode    `json:"network_mode"`
	SSHPort                 *int32                `json:"ssh_port"`
	RootPassword            *string               `json:"root_password"`
	PortLimit               *int32                `json:"port_limit"`
	SnapshotLimit           *int32                `json:"snapshot_limit"`
	BackupLimit             *int32                `json:"backup_limit"`
	SiteLimit               *int32                `json:"site_limit"`
	SwapEnabled             bool                  `json:"swap_enabled"`
	SwapSize                *int32                `json:"swap_size"`
	MonthlyTrafficLimit     *string               `json:"monthly_traffic_limit"`
	LimitsRead              *string               `json:"limits_read"`
	LimitsWrite             *string               `json:"limits_write"`
	LimitsReadIops          *int32                `json:"limits_read_iops"`
	LimitsWriteIops         *int32                `json:"limits_write_iops"`
	LimitsIngress           *string               `json:"limits_ingress"`
	LimitsEgress            *string               `json:"limits_egress"`
	LimitsProcesses         *int32                `json:"limits_processes"`
	LimitsCPUPriority       *int32                `json:"limits_cpu_priority"`
	BootAutostart           *bool                 `json:"boot_autostart"`
	BootAutostartPriority   *int32                `json:"boot_autostart_priority"`
	BootAutostartDelay      *int32                `json:"boot_autostart_delay"`
	BootHostShutdownTimeout *int32                `json:"boot_host_shutdown_timeout"`
	ExpiresAt               *string               `json:"expires_at"`
	SuspendedAt             *string               `json:"suspended_at"`
	SuspendedBy             *int32                `json:"suspended_by"`
	SuspendReason           *string               `json:"suspend_reason"`
	CreatedAt               string                `json:"created_at"`
	UpdatedAt               string                `json:"updated_at"`
	HostName                string                `json:"host_name"`
	HostLocation            *string               `json:"host_location,omitempty"`
	HostCountryCode         string                `json:"host_country_code"`
}

// InstanceAdminRecord mirrors getAllInstances' element type
// (`Instance & {host_name?, host_country_code?, username?}`).
type InstanceAdminRecord struct {
	ID                      int32                 `json:"id"`
	IncusID                 string                `json:"incus_id"`
	Name                    string                `json:"name"`
	UserID                  int32                 `json:"user_id"`
	HostID                  int32                 `json:"host_id"`
	PackageID               *int32                `json:"package_id"`
	PackagePlanID           *int32                `json:"package_plan_id"`
	StoragePoolName         *string               `json:"storage_pool_name"`
	IconBadgeID             *string               `json:"icon_badge_id"`
	Image                   string                `json:"image"`
	Status                  models.InstanceStatus `json:"status"`
	CPU                     int32                 `json:"cpu"`
	Memory                  int32                 `json:"memory"`
	Disk                    int32                 `json:"disk"`
	IPv4                    *string               `json:"ipv4"`
	IPv6                    *string               `json:"ipv6"`
	NetworkMode             models.NetworkMode    `json:"network_mode"`
	SSHPort                 *int32                `json:"ssh_port"`
	RootPassword            *string               `json:"root_password"`
	PortLimit               *int32                `json:"port_limit"`
	SnapshotLimit           *int32                `json:"snapshot_limit"`
	BackupLimit             *int32                `json:"backup_limit"`
	SiteLimit               *int32                `json:"site_limit"`
	SwapEnabled             bool                  `json:"swap_enabled"`
	SwapSize                *int32                `json:"swap_size"`
	MonthlyTrafficLimit     *string               `json:"monthly_traffic_limit"`
	LimitsRead              *string               `json:"limits_read"`
	LimitsWrite             *string               `json:"limits_write"`
	LimitsReadIops          *int32                `json:"limits_read_iops"`
	LimitsWriteIops         *int32                `json:"limits_write_iops"`
	LimitsIngress           *string               `json:"limits_ingress"`
	LimitsEgress            *string               `json:"limits_egress"`
	LimitsProcesses         *int32                `json:"limits_processes"`
	LimitsCPUPriority       *int32                `json:"limits_cpu_priority"`
	BootAutostart           *bool                 `json:"boot_autostart"`
	BootAutostartPriority   *int32                `json:"boot_autostart_priority"`
	BootAutostartDelay      *int32                `json:"boot_autostart_delay"`
	BootHostShutdownTimeout *int32                `json:"boot_host_shutdown_timeout"`
	ExpiresAt               *string               `json:"expires_at"`
	SuspendedAt             *string               `json:"suspended_at"`
	SuspendedBy             *int32                `json:"suspended_by"`
	SuspendReason           *string               `json:"suspend_reason"`
	CreatedAt               string                `json:"created_at"`
	UpdatedAt               string                `json:"updated_at"`
	HostName                string                `json:"host_name"`
	HostCountryCode         string                `json:"host_country_code"`
	Username                string                `json:"username"`
}

// InstanceDetailRecord mirrors getInstanceById's return type. The literal order
// matters: host_name / host_country_code / nat_public_ip / host_storage_driver /
// last_ipv6_reassign_at / package_plan_id / auto_renew / billing_price all come
// AFTER updated_at, so the struct follows that order even though it splits fields
// that "belong together".
type InstanceDetailRecord struct {
	ID                         int32                 `json:"id"`
	IncusID                    string                `json:"incus_id"`
	Name                       string                `json:"name"`
	UserID                     int32                 `json:"user_id"`
	HostID                     int32                 `json:"host_id"`
	PackageID                  *int32                `json:"package_id"`
	StoragePoolName            *string               `json:"storage_pool_name"`
	IconBadgeID                *string               `json:"icon_badge_id"`
	Image                      string                `json:"image"`
	Status                     models.InstanceStatus `json:"status"`
	CPU                        int32                 `json:"cpu"`
	Memory                     int32                 `json:"memory"`
	Disk                       int32                 `json:"disk"`
	IPv4                       *string               `json:"ipv4"`
	IPv6                       *string               `json:"ipv6"`
	NetworkMode                models.NetworkMode    `json:"network_mode"`
	SSHPort                    *int32                `json:"ssh_port"`
	RootPassword               *string               `json:"root_password"`
	PortLimit                  *int32                `json:"port_limit"`
	SnapshotLimit              *int32                `json:"snapshot_limit"`
	BackupLimit                *int32                `json:"backup_limit"`
	SiteLimit                  *int32                `json:"site_limit"`
	SwapEnabled                bool                  `json:"swap_enabled"`
	SwapSize                   *int32                `json:"swap_size"`
	MonthlyTrafficLimit        *string               `json:"monthly_traffic_limit"`
	LimitsRead                 *string               `json:"limits_read"`
	LimitsWrite                *string               `json:"limits_write"`
	LimitsReadIops             *int32                `json:"limits_read_iops"`
	LimitsWriteIops            *int32                `json:"limits_write_iops"`
	LimitsIngress              *string               `json:"limits_ingress"`
	LimitsEgress               *string               `json:"limits_egress"`
	LimitsProcesses            *int32                `json:"limits_processes"`
	LimitsCPUPriority          *int32                `json:"limits_cpu_priority"`
	BootAutostart              *bool                 `json:"boot_autostart"`
	BootAutostartPriority      *int32                `json:"boot_autostart_priority"`
	BootAutostartDelay         *int32                `json:"boot_autostart_delay"`
	BootHostShutdownTimeout    *int32                `json:"boot_host_shutdown_timeout"`
	ExpiresAt                  *string               `json:"expires_at"`
	SuspendedAt                *string               `json:"suspended_at"`
	SuspendedBy                *int32                `json:"suspended_by"`
	SuspendReason              *string               `json:"suspend_reason"`
	CloudInitState             *string               `json:"cloud_init_state"`
	CloudInitSource            *string               `json:"cloud_init_source"`
	CloudInitLastCheckedAt     *string               `json:"cloud_init_last_checked_at"`
	CloudInitCompletedAt       *string               `json:"cloud_init_completed_at"`
	CloudInitManualCompletedAt *string               `json:"cloud_init_manual_completed_at"`
	CloudInitManualCompletedBy *int32                `json:"cloud_init_manual_completed_by"`
	CreatedAt                  string                `json:"created_at"`
	UpdatedAt                  string                `json:"updated_at"`
	HostName                   string                `json:"host_name"`
	HostCountryCode            string                `json:"host_country_code"`
	NATPublicIP                *string               `json:"nat_public_ip,omitempty"`
	HostStorageDriver          string                `json:"host_storage_driver"`
	LastIPv6ReassignAt         *string               `json:"last_ipv6_reassign_at"`
	PackagePlanID              *int32                `json:"package_plan_id"`
	AutoRenew                  bool                  `json:"auto_renew"`
	BillingPrice               *float64              `json:"billing_price"`
}

// InstanceConfigRecord mirrors getInstanceConfig's return type. limits_ingress /
// limits_egress are the *configured* override columns, exactly like the
// TypeScript (it reads configuredLimitsIngress/configuredLimitsEgress).
type InstanceConfigRecord struct {
	LimitsRead              *string `json:"limits_read"`
	LimitsWrite             *string `json:"limits_write"`
	LimitsReadIops          *int32  `json:"limits_read_iops"`
	LimitsWriteIops         *int32  `json:"limits_write_iops"`
	LimitsIngress           *string `json:"limits_ingress"`
	LimitsEgress            *string `json:"limits_egress"`
	LimitsProcesses         *int32  `json:"limits_processes"`
	LimitsCPUPriority       *int32  `json:"limits_cpu_priority"`
	BootAutostart           *bool   `json:"boot_autostart"`
	BootAutostartPriority   *int32  `json:"boot_autostart_priority"`
	BootAutostartDelay      *int32  `json:"boot_autostart_delay"`
	BootHostShutdownTimeout *int32  `json:"boot_host_shutdown_timeout"`
}

// InstanceStuckCreatingRecord mirrors getStuckCreatingInstances' element type.
// created_at is a Date in the TypeScript, so it stays time.Time here (the
// scheduler compares instants; no route serialises this record).
type InstanceStuckCreatingRecord struct {
	ID          int32     `json:"id"`
	IncusID     string    `json:"incus_id"`
	Name        string    `json:"name"`
	HostID      int32     `json:"host_id"`
	UserID      int32     `json:"user_id"`
	CPU         int32     `json:"cpu"`
	Memory      int32     `json:"memory"`
	Disk        int32     `json:"disk"`
	NetworkMode string    `json:"network_mode"`
	PortLimit   *int32    `json:"port_limit"`
	CreatedAt   time.Time `json:"created_at"`
}

// InstanceExpiringRecord mirrors getExpiringInstances' element type.
type InstanceExpiringRecord struct {
	ID        int32     `json:"id"`
	Name      string    `json:"name"`
	UserID    int32     `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Status    string    `json:"status"`
}

// InstanceExpiredRecord mirrors getExpiredUnsuspendedInstances' element type.
type InstanceExpiredRecord struct {
	ID        int32     `json:"id"`
	Name      string    `json:"name"`
	UserID    int32     `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ==================== plumbing ====================

// instancesRequireDB keeps an unwired call site loud instead of panicking.
func instancesRequireDB() error {
	if Pool() == nil || DB == nil {
		return errors.New("instances: database not initialized")
	}
	return nil
}

// instancesISOTime mirrors Date.prototype.toISOString(): UTC, millisecond
// precision, always six fractional digits collapsed to three.
func instancesISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// instancesOptionalISOTime mirrors `value?.toISOString() ?? null`.
func instancesOptionalISOTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := instancesISOTime(*value)
	return &formatted
}

// instancesMonthlyTrafficLimitString mirrors
// `inst.monthlyTrafficLimit ? inst.monthlyTrafficLimit.toString() : null`.
// A BigInt 0n is FALSY in JavaScript, so a stored zero must render as null, not
// as the string "0".
func instancesMonthlyTrafficLimitString(value *int64) *string {
	if value == nil || *value == 0 {
		return nil
	}
	text := strconv.FormatInt(*value, 10)
	return &text
}

// instancesSelectColumns is the projection every full instance read shares. It is
// the union of the three selects in the TypeScript; each converter below picks
// the keys its object literal exposes.
const instancesSelectColumns = `i.id, i.incus_id, i.name, i.user_id, i.host_id,
	i.package_id, i.package_plan_id, i.storage_pool_name, i.icon_badge_id, i.image,
	i.status, i.cpu, i.memory, i.disk, i.ipv4, i.ipv6, i.network_mode, i.ssh_port,
	i.root_password, i.port_limit, i.snapshot_limit, i.backup_limit, i.site_limit,
	i.swap_enabled, i.swap_size, i.monthly_traffic_limit, i.limits_read, i.limits_write,
	i.limits_read_iops, i.limits_write_iops, i.limits_ingress, i.limits_egress,
	i.limits_processes, i.limits_cpu_priority, i.boot_autostart, i.boot_autostart_priority,
	i.boot_autostart_delay, i.boot_host_shutdown_timeout, i.expires_at, i.suspended_at,
	i.suspended_by, i.suspend_reason, i.cloud_init_state, i.cloud_init_source,
	i.cloud_init_last_checked_at, i.cloud_init_completed_at, i.cloud_init_manual_completed_at,
	i.cloud_init_manual_completed_by, i.created_at, i.updated_at, i.last_ipv6_reassign_at,
	i.auto_renew, i.billing_price, h.name AS host_name, h.location AS host_location,
	h.country_code AS host_country_code, h.nat_public_ip AS host_nat_public_ip,
	h.storage_driver AS host_storage_driver, u.username AS username`

// instanceRow is the scan target for instancesSelectColumns.
type instanceRow struct {
	ID                         int32
	IncusID                    string
	Name                       string
	UserID                     int32
	HostID                     int32
	PackageID                  *int32
	PackagePlanID              *int32
	StoragePoolName            *string
	IconBadgeID                *string
	Image                      string
	Status                     string
	CPU                        int32
	Memory                     int32
	Disk                       int32
	IPv4                       *string
	IPv6                       *string
	NetworkMode                string
	SSHPort                    *int32
	RootPassword               *string
	PortLimit                  *int32
	SnapshotLimit              *int32
	BackupLimit                *int32
	SiteLimit                  *int32
	SwapEnabled                bool
	SwapSize                   *int32
	MonthlyTrafficLimit        *int64
	LimitsRead                 *string
	LimitsWrite                *string
	LimitsReadIops             *int32
	LimitsWriteIops            *int32
	LimitsIngress              *string
	LimitsEgress               *string
	LimitsProcesses            *int32
	LimitsCPUPriority          *int32
	BootAutostart              *bool
	BootAutostartPriority      *int32
	BootAutostartDelay         *int32
	BootHostShutdownTimeout    *int32
	ExpiresAt                  *time.Time
	SuspendedAt                *time.Time
	SuspendedBy                *int32
	SuspendReason              *string
	CloudInitState             *string
	CloudInitSource            *string
	CloudInitLastCheckedAt     *time.Time
	CloudInitCompletedAt       *time.Time
	CloudInitManualCompletedAt *time.Time
	CloudInitManualCompletedBy *int32
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
	LastIPv6ReassignAt         *time.Time
	AutoRenew                  bool
	BillingPrice               *models.Decimal
	HostName                   string
	HostLocation               *string
	HostCountryCode            string
	HostNATPublicIP            *string
	HostStorageDriver          string
	Username                   string
}

// instancesScanRow scans one projection row. Any pgx row satisfies it (QueryRow
// and Rows both expose Scan).
func instancesScanRow(scanner interface {
	Scan(dest ...any) error
}) (*instanceRow, error) {
	var row instanceRow
	err := scanner.Scan(
		&row.ID, &row.IncusID, &row.Name, &row.UserID, &row.HostID,
		&row.PackageID, &row.PackagePlanID, &row.StoragePoolName, &row.IconBadgeID, &row.Image,
		&row.Status, &row.CPU, &row.Memory, &row.Disk, &row.IPv4, &row.IPv6, &row.NetworkMode, &row.SSHPort,
		&row.RootPassword, &row.PortLimit, &row.SnapshotLimit, &row.BackupLimit, &row.SiteLimit,
		&row.SwapEnabled, &row.SwapSize, &row.MonthlyTrafficLimit, &row.LimitsRead, &row.LimitsWrite,
		&row.LimitsReadIops, &row.LimitsWriteIops, &row.LimitsIngress, &row.LimitsEgress,
		&row.LimitsProcesses, &row.LimitsCPUPriority, &row.BootAutostart, &row.BootAutostartPriority,
		&row.BootAutostartDelay, &row.BootHostShutdownTimeout, &row.ExpiresAt, &row.SuspendedAt,
		&row.SuspendedBy, &row.SuspendReason, &row.CloudInitState, &row.CloudInitSource,
		&row.CloudInitLastCheckedAt, &row.CloudInitCompletedAt, &row.CloudInitManualCompletedAt,
		&row.CloudInitManualCompletedBy, &row.CreatedAt, &row.UpdatedAt, &row.LastIPv6ReassignAt,
		&row.AutoRenew, &row.BillingPrice, &row.HostName, &row.HostLocation,
		&row.HostCountryCode, &row.HostNATPublicIP, &row.HostStorageDriver, &row.Username,
	)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// instancesQuery runs a full-instance SELECT and returns the scanned rows.
func instancesQuery(ctx context.Context, query string, args ...any) ([]instanceRow, error) {
	rows, err := Pool().Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]instanceRow, 0, 16)
	for rows.Next() {
		row, err := instancesScanRow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// ==================== row converters ====================

func (row instanceRow) toHostedRecord() InstanceHostedRecord {
	return InstanceHostedRecord{
		ID:                      row.ID,
		IncusID:                 row.IncusID,
		Name:                    row.Name,
		UserID:                  row.UserID,
		HostID:                  row.HostID,
		PackageID:               row.PackageID,
		PackagePlanID:           row.PackagePlanID,
		StoragePoolName:         row.StoragePoolName,
		IconBadgeID:             row.IconBadgeID,
		Image:                   row.Image,
		Status:                  models.InstanceStatus(row.Status),
		CPU:                     row.CPU,
		Memory:                  row.Memory,
		Disk:                    row.Disk,
		IPv4:                    row.IPv4,
		IPv6:                    row.IPv6,
		NetworkMode:             models.NetworkMode(row.NetworkMode),
		SSHPort:                 row.SSHPort,
		RootPassword:            row.RootPassword,
		PortLimit:               row.PortLimit,
		SnapshotLimit:           row.SnapshotLimit,
		BackupLimit:             row.BackupLimit,
		SiteLimit:               row.SiteLimit,
		SwapEnabled:             row.SwapEnabled,
		SwapSize:                row.SwapSize,
		MonthlyTrafficLimit:     instancesMonthlyTrafficLimitString(row.MonthlyTrafficLimit),
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
		ExpiresAt:               instancesOptionalISOTime(row.ExpiresAt),
		SuspendedAt:             instancesOptionalISOTime(row.SuspendedAt),
		SuspendedBy:             row.SuspendedBy,
		SuspendReason:           row.SuspendReason,
		CreatedAt:               instancesISOTime(row.CreatedAt),
		UpdatedAt:               instancesISOTime(row.UpdatedAt),
		HostName:                row.HostName,
		HostLocation:            row.HostLocation,
		HostCountryCode:         row.HostCountryCode,
	}
}

func (row instanceRow) toAdminRecord() InstanceAdminRecord {
	return InstanceAdminRecord{
		ID:                      row.ID,
		IncusID:                 row.IncusID,
		Name:                    row.Name,
		UserID:                  row.UserID,
		HostID:                  row.HostID,
		PackageID:               row.PackageID,
		PackagePlanID:           row.PackagePlanID,
		StoragePoolName:         row.StoragePoolName,
		IconBadgeID:             row.IconBadgeID,
		Image:                   row.Image,
		Status:                  models.InstanceStatus(row.Status),
		CPU:                     row.CPU,
		Memory:                  row.Memory,
		Disk:                    row.Disk,
		IPv4:                    row.IPv4,
		IPv6:                    row.IPv6,
		NetworkMode:             models.NetworkMode(row.NetworkMode),
		SSHPort:                 row.SSHPort,
		RootPassword:            row.RootPassword,
		PortLimit:               row.PortLimit,
		SnapshotLimit:           row.SnapshotLimit,
		BackupLimit:             row.BackupLimit,
		SiteLimit:               row.SiteLimit,
		SwapEnabled:             row.SwapEnabled,
		SwapSize:                row.SwapSize,
		MonthlyTrafficLimit:     instancesMonthlyTrafficLimitString(row.MonthlyTrafficLimit),
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
		ExpiresAt:               instancesOptionalISOTime(row.ExpiresAt),
		SuspendedAt:             instancesOptionalISOTime(row.SuspendedAt),
		SuspendedBy:             row.SuspendedBy,
		SuspendReason:           row.SuspendReason,
		CreatedAt:               instancesISOTime(row.CreatedAt),
		UpdatedAt:               instancesISOTime(row.UpdatedAt),
		HostName:                row.HostName,
		HostCountryCode:         row.HostCountryCode,
		Username:                row.Username,
	}
}

func (row instanceRow) toDetailRecord() InstanceDetailRecord {
	return InstanceDetailRecord{
		ID:                         row.ID,
		IncusID:                    row.IncusID,
		Name:                       row.Name,
		UserID:                     row.UserID,
		HostID:                     row.HostID,
		PackageID:                  row.PackageID,
		StoragePoolName:            row.StoragePoolName,
		IconBadgeID:                row.IconBadgeID,
		Image:                      row.Image,
		Status:                     models.InstanceStatus(row.Status),
		CPU:                        row.CPU,
		Memory:                     row.Memory,
		Disk:                       row.Disk,
		IPv4:                       row.IPv4,
		IPv6:                       row.IPv6,
		NetworkMode:                models.NetworkMode(row.NetworkMode),
		SSHPort:                    row.SSHPort,
		RootPassword:               row.RootPassword,
		PortLimit:                  row.PortLimit,
		SnapshotLimit:              row.SnapshotLimit,
		BackupLimit:                row.BackupLimit,
		SiteLimit:                  row.SiteLimit,
		SwapEnabled:                row.SwapEnabled,
		SwapSize:                   row.SwapSize,
		MonthlyTrafficLimit:        instancesMonthlyTrafficLimitString(row.MonthlyTrafficLimit),
		LimitsRead:                 row.LimitsRead,
		LimitsWrite:                row.LimitsWrite,
		LimitsReadIops:             row.LimitsReadIops,
		LimitsWriteIops:            row.LimitsWriteIops,
		LimitsIngress:              row.LimitsIngress,
		LimitsEgress:               row.LimitsEgress,
		LimitsProcesses:            row.LimitsProcesses,
		LimitsCPUPriority:          row.LimitsCPUPriority,
		BootAutostart:              row.BootAutostart,
		BootAutostartPriority:      row.BootAutostartPriority,
		BootAutostartDelay:         row.BootAutostartDelay,
		BootHostShutdownTimeout:    row.BootHostShutdownTimeout,
		ExpiresAt:                  instancesOptionalISOTime(row.ExpiresAt),
		SuspendedAt:                instancesOptionalISOTime(row.SuspendedAt),
		SuspendedBy:                row.SuspendedBy,
		SuspendReason:              row.SuspendReason,
		CloudInitState:             row.CloudInitState,
		CloudInitSource:            row.CloudInitSource,
		CloudInitLastCheckedAt:     instancesOptionalISOTime(row.CloudInitLastCheckedAt),
		CloudInitCompletedAt:       instancesOptionalISOTime(row.CloudInitCompletedAt),
		CloudInitManualCompletedAt: instancesOptionalISOTime(row.CloudInitManualCompletedAt),
		CloudInitManualCompletedBy: row.CloudInitManualCompletedBy,
		CreatedAt:                  instancesISOTime(row.CreatedAt),
		UpdatedAt:                  instancesISOTime(row.UpdatedAt),
		HostName:                   row.HostName,
		HostCountryCode:            row.HostCountryCode,
		NATPublicIP:                row.HostNATPublicIP,
		HostStorageDriver:          row.HostStorageDriver,
		LastIPv6ReassignAt:         instancesOptionalISOTime(row.LastIPv6ReassignAt),
		PackagePlanID:              row.PackagePlanID,
		AutoRenew:                  row.AutoRenew,
		BillingPrice:               instancesOptionalDecimalNumber(row.BillingPrice),
	}
}

// instancesOptionalDecimalNumber mirrors `value ? Number(value) : null`. Prisma's
// Decimal is an object, so it is always truthy — only SQL NULL yields null. A
// stored 0.00 therefore renders as 0, not null.
func instancesOptionalDecimalNumber(value *models.Decimal) *float64 {
	if value == nil {
		return nil
	}
	asFloat := decimalToFloat64(*value)
	return &asFloat
}

// ==================== reads ====================

// GetInstancesByUserId mirrors getInstancesByUserId: every non-deleted instance
// of one user, newest id first, with the host display fields joined in.
func GetInstancesByUserId(ctx context.Context, userID int32) ([]InstanceHostedRecord, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	rows, err := instancesQuery(ctx, `SELECT `+instancesSelectColumns+`
		FROM instances i
		JOIN hosts h ON h.id = i.host_id
		JOIN users u ON u.id = i.user_id
		WHERE i.user_id = $1 AND i.status <> 'deleted'
		ORDER BY i.id DESC`, userID)
	if err != nil {
		return nil, err
	}
	result := make([]InstanceHostedRecord, 0, len(rows))
	for _, row := range rows {
		result = append(result, row.toHostedRecord())
	}
	return result, nil
}

// GetAllInstances mirrors getAllInstances: every non-deleted instance, newest id
// first, with host name / country code and the owning username.
func GetAllInstances(ctx context.Context) ([]InstanceAdminRecord, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	rows, err := instancesQuery(ctx, `SELECT `+instancesSelectColumns+`
		FROM instances i
		JOIN hosts h ON h.id = i.host_id
		JOIN users u ON u.id = i.user_id
		WHERE i.status <> 'deleted'
		ORDER BY i.id DESC`)
	if err != nil {
		return nil, err
	}
	result := make([]InstanceAdminRecord, 0, len(rows))
	for _, row := range rows {
		result = append(result, row.toAdminRecord())
	}
	return result, nil
}

// GetInstanceById mirrors getInstanceById: nil, nil when the id does not exist.
func GetInstanceById(ctx context.Context, id int32) (*InstanceDetailRecord, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	row, err := instancesScanRow(Pool().QueryRow(ctx, `SELECT `+instancesSelectColumns+`
		FROM instances i
		JOIN hosts h ON h.id = i.host_id
		JOIN users u ON u.id = i.user_id
		WHERE i.id = $1`, id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record := row.toDetailRecord()
	return &record, nil
}

// instanceConfigSelect reads the CONFIGURED ingress/egress override columns, not
// the effective limits_ingress/limits_egress columns. The two families are
// different: configured_* is what the user asked for, limits_* is what is applied
// right now (updateInstanceConfig writes them from different arguments).
const instanceConfigSelect = `SELECT limits_read, limits_write, limits_read_iops,
		limits_write_iops, configured_limits_ingress, configured_limits_egress,
		limits_processes, limits_cpu_priority, boot_autostart,
		boot_autostart_priority, boot_autostart_delay, boot_host_shutdown_timeout
	FROM instances WHERE id = $1`

// GetInstanceConfig mirrors getInstanceConfig: the override columns only, nil
// when the instance does not exist.
func GetInstanceConfig(ctx context.Context, instanceID int32) (*InstanceConfigRecord, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	var record InstanceConfigRecord
	err := Pool().QueryRow(ctx, instanceConfigSelect, instanceID).Scan(
		&record.LimitsRead, &record.LimitsWrite, &record.LimitsReadIops, &record.LimitsWriteIops,
		&record.LimitsIngress, &record.LimitsEgress, &record.LimitsProcesses, &record.LimitsCPUPriority,
		&record.BootAutostart, &record.BootAutostartPriority, &record.BootAutostartDelay,
		&record.BootHostShutdownTimeout,
	)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// GetStuckCreatingInstances mirrors getStuckCreatingInstances: instances still in
// `creating` whose created_at is older than now - timeoutMs.
func GetStuckCreatingInstances(ctx context.Context, timeoutMs int64) ([]InstanceStuckCreatingRecord, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	threshold := time.Now().UTC().Add(-time.Duration(timeoutMs) * time.Millisecond)
	rows, err := Pool().Query(ctx, `SELECT id, incus_id, name, host_id, user_id, cpu,
			memory, disk, network_mode, port_limit, created_at
		FROM instances
		WHERE status = 'creating' AND created_at < $1`, threshold)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]InstanceStuckCreatingRecord, 0, 8)
	for rows.Next() {
		var record InstanceStuckCreatingRecord
		if err := rows.Scan(&record.ID, &record.IncusID, &record.Name, &record.HostID,
			&record.UserID, &record.CPU, &record.Memory, &record.Disk, &record.NetworkMode,
			&record.PortLimit, &record.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// GetExpiringInstances mirrors getExpiringInstances: expires_at <= beforeDate and
// neither deleted nor suspended.
func GetExpiringInstances(ctx context.Context, beforeDate time.Time) ([]InstanceExpiringRecord, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	rows, err := Pool().Query(ctx, `SELECT id, name, user_id, expires_at, status
		FROM instances
		WHERE expires_at <= $1 AND status NOT IN ('deleted', 'suspended')`, beforeDate.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]InstanceExpiringRecord, 0, 16)
	for rows.Next() {
		var (
			record    InstanceExpiringRecord
			expiresAt *time.Time
		)
		if err := rows.Scan(&record.ID, &record.Name, &record.UserID, &expiresAt, &record.Status); err != nil {
			return nil, err
		}
		if expiresAt != nil {
			record.ExpiresAt = *expiresAt
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// GetExpiredUnsuspendedInstances mirrors getExpiredUnsuspendedInstances: already
// expired but neither deleted nor suspended yet (the suspend scheduler's input).
func GetExpiredUnsuspendedInstances(ctx context.Context) ([]InstanceExpiredRecord, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	rows, err := Pool().Query(ctx, `SELECT id, name, user_id, expires_at
		FROM instances
		WHERE expires_at <= $1 AND status NOT IN ('deleted', 'suspended')`, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]InstanceExpiredRecord, 0, 16)
	for rows.Next() {
		var (
			record    InstanceExpiredRecord
			expiresAt *time.Time
		)
		if err := rows.Scan(&record.ID, &record.Name, &record.UserID, &expiresAt); err != nil {
			return nil, err
		}
		if expiresAt != nil {
			record.ExpiresAt = *expiresAt
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// ==================== create ====================

// CreateInstance mirrors createInstance: insert the `creating` row and return its
// id. The egress binding is resolved inside the same transaction, before the
// insert, exactly like `bindPackageEgress(tx, …)` spread into `data`.
func CreateInstance(ctx context.Context, input InstanceCreateInput) (int32, error) {
	if err := instancesRequireDB(); err != nil {
		return 0, err
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	binding, err := instancesBindPackageEgress(ctx, tx, input.PackageID, input.HostID, input.EgressSourceInstanceID)
	if err != nil {
		return 0, err
	}

	snapshotSpecs := input.SnapshotSpecs
	if len(snapshotSpecs) == 0 || string(snapshotSpecs) == "null" {
		// `data.snapshotSpecs || {}` — undefined/null/"" all fall back to {}.
		snapshotSpecs = json.RawMessage(`{}`)
	}

	swapEnabled := false
	if input.SwapEnabled != nil {
		swapEnabled = *input.SwapEnabled
	}

	now := time.Now().UTC()
	var instanceID int32
	// instances.updated_at is NOT NULL with no database default (Prisma's
	// @updatedAt fills it client-side) — omitting it is a runtime not-null
	// violation, which TestRawInsertsSetUpdatedAt exists to prevent. created_at is
	// written explicitly for the same reason every other timestamp here is: the
	// column is `timestamp without time zone` and only an explicit UTC value is
	// guaranteed to match what the Node backend stores (WAVE_D_SPEC §0.2b).
	err = tx.QueryRow(ctx, `INSERT INTO instances (
			egress_mode, egress_profile_id, incus_id, name, user_id, host_id, package_id,
			storage_pool_name, image, cpu, memory, disk, network_mode, snapshotted_specs,
			ssh_port, root_password, status, monthly_traffic_limit, port_limit,
			snapshot_limit, backup_limit, site_limit, swap_enabled, swap_size,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
			$15, $16, 'creating', $17, $18, $19, $20, $21, $22, $23, $24, $24)
		RETURNING id`,
		binding.EgressMode, binding.EgressProfileID, input.IncusID, input.Name, input.UserID,
		input.HostID, input.PackageID, input.StoragePoolName, input.Image, input.CPU,
		input.Memory, input.Disk, string(input.NetworkMode), string(snapshotSpecs),
		input.SSHPort, input.RootPassword, input.MonthlyTrafficLimit, input.PortLimit,
		input.SnapshotLimit, input.BackupLimit, input.SiteLimit, swapEnabled,
		input.SwapSize, now,
	).Scan(&instanceID)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return instanceID, nil
}

// ==================== egress binding (mirrors lib/egress/package.ts) ====================

// instancesIDPresent mirrors the TypeScript's truthiness checks on optional ids
// (`sourceId ? …`, `packageId ? …`): a present-but-zero id is treated as absent.
func instancesIDPresent(id *int32) bool {
	return id != nil && *id != 0
}

// instancesBindPackageEgress mirrors bindPackageEgress. It must run on the same
// transaction as the instance INSERT: the advisory lock is transaction-scoped and
// the resolved policy identity has to commit together with the row.
//
// The order is load-bearing:
//  1. single-argument pg_advisory_xact_lock(74201) — shared with the Node process,
//     a different lock space from every (namespace, key) advisory lock;
//  2. read the source instance (clone/upgrade) and the package;
//  3. reject a vanished source (`sourceId && !source`) or a package that is gone
//     with no source to fall back on;
//  4. for a non-host mode: require the host agent capability, validate the
//     package policy (only when inheriting from the package), then re-check the
//     source instance's profile on the target host.
func instancesBindPackageEgress(
	ctx context.Context,
	tx pgx.Tx,
	packageID *int32,
	hostID int32,
	sourceInstanceID *int32,
) (InstanceEgressBinding, error) {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", instanceEgressLockNamespace); err != nil {
		return InstanceEgressBinding{}, err
	}

	var source *InstanceEgressBinding
	sourcePresent := instancesIDPresent(sourceInstanceID)
	if sourcePresent {
		var (
			egressMode      string
			egressProfileID *int32
		)
		err := tx.QueryRow(ctx, `SELECT egress_mode, egress_profile_id FROM instances WHERE id = $1`,
			*sourceInstanceID).Scan(&egressMode, &egressProfileID)
		if err != nil && !isNoRows(err) {
			return InstanceEgressBinding{}, err
		}
		if err == nil {
			source = &InstanceEgressBinding{EgressMode: egressMode, EgressProfileID: egressProfileID}
		}
	}

	type packageEgressPolicy struct {
		EgressMode      string
		EgressProfileID *int32
		NetworkMode     string
		InstanceType    string
		Privileged      bool
	}
	var pkg *packageEgressPolicy
	packagePresent := instancesIDPresent(packageID)
	if packagePresent {
		var policy packageEgressPolicy
		err := tx.QueryRow(ctx, `SELECT egress_mode, egress_profile_id, network_mode,
				instance_type, privileged FROM packages WHERE id = $1`, *packageID).
			Scan(&policy.EgressMode, &policy.EgressProfileID, &policy.NetworkMode,
				&policy.InstanceType, &policy.Privileged)
		if err != nil && !isNoRows(err) {
			return InstanceEgressBinding{}, err
		}
		if err == nil {
			pkg = &policy
		}
	}

	if (sourcePresent && source == nil) || (packagePresent && pkg == nil && source == nil) {
		return InstanceEgressBinding{}, &InstanceEgressInputError{Message: "Egress binding source no longer exists"}
	}

	binding := InstanceEgressBinding{EgressMode: "host", EgressProfileID: nil}
	switch {
	case source != nil:
		binding = *source
	case pkg != nil:
		binding = InstanceEgressBinding{EgressMode: pkg.EgressMode, EgressProfileID: pkg.EgressProfileID}
	}

	if binding.EgressMode != "host" {
		if InstanceEgressHostSupportsControl == nil ||
			!InstanceEgressHostSupportsControl(hostID, instanceEgressControlCapability) {
			return InstanceEgressBinding{}, &InstanceEgressInputError{
				Message: "The host needs an online Agent supporting package egress",
			}
		}
		if source == nil {
			if err := instancesValidatePackageEgress(ctx, tx,
				pkg.EgressMode, pkg.EgressProfileID, pkg.NetworkMode, pkg.InstanceType,
				pkg.Privileged, []int32{hostID}); err != nil {
				return InstanceEgressBinding{}, err
			}
		}
		// `source?.egressProfileId` — a zero would be falsy in the TypeScript, and id
		// 0 is never a real profile (primary keys start at 1).
		if source != nil && source.EgressProfileID != nil && *source.EgressProfileID != 0 {
			var (
				active         bool
				allowedHostIDs []int32
			)
			err := tx.QueryRow(ctx, `SELECT active, allowed_host_ids FROM egress_profiles WHERE id = $1`,
				*source.EgressProfileID).Scan(&active, &allowedHostIDs)
			if err != nil && !isNoRows(err) {
				return InstanceEgressBinding{}, err
			}
			if err != nil || !active || !instancesContainsInt32(allowedHostIDs, hostID) {
				return InstanceEgressBinding{}, &InstanceEgressInputError{
					Message: "The instance egress profile is not permitted on the target host",
				}
			}
		}
	}

	return binding, nil
}

// instancesValidatePackageEgress mirrors validatePackageEgress.
func instancesValidatePackageEgress(
	ctx context.Context,
	tx pgx.Tx,
	egressMode string,
	egressProfileID *int32,
	networkMode string,
	instanceType string,
	privileged bool,
	hostIDs []int32,
) error {
	if egressMode == "" {
		egressMode = "host"
	}
	switch egressMode {
	case "host", "direct", "profile":
	default:
		return &InstanceEgressInputError{Message: "Invalid package egress mode"}
	}
	if egressMode != "profile" && egressProfileID != nil {
		return &InstanceEgressInputError{Message: "Only proxy mode may select an egress profile"}
	}
	if egressMode == "host" {
		return nil
	}
	if networkMode == "" {
		networkMode = "nat"
	}
	if instanceType == "" {
		instanceType = "container"
	}
	if networkMode != "nat" || instanceType != "container" || privileged {
		return &InstanceEgressInputError{
			Message: "Package egress currently requires an unprivileged IPv4 NAT container",
		}
	}
	if egressMode == "profile" {
		var (
			found          bool
			active         bool
			allowedHostIDs []int32
		)
		if egressProfileID != nil {
			err := tx.QueryRow(ctx, `SELECT active, allowed_host_ids FROM egress_profiles WHERE id = $1`,
				*egressProfileID).Scan(&active, &allowedHostIDs)
			if err != nil && !isNoRows(err) {
				return err
			}
			found = err == nil
		}
		permitted := found && active
		if permitted {
			for _, hostID := range hostIDs {
				if !instancesContainsInt32(allowedHostIDs, hostID) {
					permitted = false
					break
				}
			}
		}
		if !permitted {
			return &InstanceEgressInputError{
				Message: "Egress profile is inactive or not permitted on the selected hosts",
			}
		}
	}
	return nil
}

// instancesContainsInt32 mirrors Array.prototype.includes for int32 slices.
func instancesContainsInt32(values []int32, target int32) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// ==================== updates ====================

// instanceUpdateAssignments builds the SET list for updateInstance.
func instanceUpdateAssignments(data InstanceUpdate) ([]string, []any) {
	sets := make([]string, 0, 12)
	args := make([]any, 0, 12)

	if data.Image != nil {
		sets = append(sets, "image = ?")
		args = append(args, *data.Image)
	}
	if data.Name != nil {
		sets = append(sets, "name = ?")
		args = append(args, *data.Name)
	}
	if data.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, string(*data.Status))
	}
	if data.SSHPort.Set {
		sets = append(sets, "ssh_port = ?")
		args = append(args, data.SSHPort.Value)
	}
	if data.RootPassword.Set {
		sets = append(sets, "root_password = ?")
		args = append(args, data.RootPassword.Value)
	}
	if data.PortLimit.Set {
		sets = append(sets, "port_limit = ?")
		args = append(args, data.PortLimit.Value)
	}
	if data.SnapshotLimit.Set {
		sets = append(sets, "snapshot_limit = ?")
		args = append(args, data.SnapshotLimit.Value)
	}
	if data.BackupLimit.Set {
		sets = append(sets, "backup_limit = ?")
		args = append(args, data.BackupLimit.Value)
	}
	if data.SiteLimit.Set {
		sets = append(sets, "site_limit = ?")
		args = append(args, data.SiteLimit.Value)
	}
	if data.SwapEnabled != nil {
		sets = append(sets, "swap_enabled = ?")
		args = append(args, *data.SwapEnabled)
	}
	if data.SwapSize.Set {
		sets = append(sets, "swap_size = ?")
		args = append(args, data.SwapSize.Value)
	}
	if data.StoragePoolName.Set {
		sets = append(sets, "storage_pool_name = ?")
		args = append(args, data.StoragePoolName.Value)
	}
	return sets, args
}

// instanceQuotaAssignments builds the SET list for updateInstanceQuota, which
// routes into updateInstance with only the four quota keys.
func instanceQuotaAssignments(data InstanceQuotaUpdate) ([]string, []any) {
	update := InstanceUpdate{
		PortLimit:     data.PortLimit,
		SnapshotLimit: data.SnapshotLimit,
		BackupLimit:   data.BackupLimit,
		SiteLimit:     data.SiteLimit,
	}
	// Order follows the TypeScript literal: portLimit, snapshotLimit, backupLimit,
	// siteLimit (updateInstance then re-applies its own order, which is the same).
	return instanceUpdateAssignments(update)
}

// instanceStatusAssignments builds the SET list for updateInstanceStatus.
func instanceStatusAssignments(status models.InstanceStatus, network *InstanceNetworkUpdate) ([]string, []any) {
	sets := make([]string, 0, 3)
	args := make([]any, 0, 3)
	sets = append(sets, "status = ?")
	args = append(args, string(status))
	if network != nil {
		if network.IPv4.Set {
			sets = append(sets, "ipv4 = ?")
			args = append(args, network.IPv4.Value)
		}
		if network.IPv6.Set {
			sets = append(sets, "ipv6 = ?")
			args = append(args, network.IPv6.Value)
		}
	}
	return sets, args
}

// instanceStatusUpdateBlocked mirrors the `status: { notIn: ['deleted',
// 'suspended'] }` filter of updateInstanceStatus' updateMany branch. It is the
// single source for both the SQL filter and the Go-side predicate below, so the
// two cannot drift.
var instanceStatusUpdateBlocked = []models.InstanceStatus{
	models.InstanceStatusDeleted,
	models.InstanceStatusSuspended,
}

// instanceStatusUpdateFilterSQL renders the blocked-status list as the SQL filter
// appended to the updateMany branch.
func instanceStatusUpdateFilterSQL() string {
	quoted := make([]string, 0, len(instanceStatusUpdateBlocked))
	for _, status := range instanceStatusUpdateBlocked {
		quoted = append(quoted, "'"+string(status)+"'")
	}
	return "status NOT IN (" + strings.Join(quoted, ", ") + ")"
}

// instanceStatusTransitionApplies is the Go view of the same rule: 'deleted'
// always applies (single-row update), everything else only while the current row
// is not one of the blocked statuses. A transition that does not apply is a
// silent no-op in the TypeScript (updateMany does not throw).
func instanceStatusTransitionApplies(current, target models.InstanceStatus) bool {
	if target == models.InstanceStatusDeleted {
		return true
	}
	return !instancesContainsStatus(instanceStatusUpdateBlocked, current)
}

// instancesContainsStatus mirrors Array.prototype.includes for status slices.
func instancesContainsStatus(values []models.InstanceStatus, target models.InstanceStatus) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// UpdateInstance mirrors updateInstance. An empty patch returns without touching
// the row (and therefore without bumping updated_at).
func UpdateInstance(ctx context.Context, id int32, data InstanceUpdate) error {
	if err := instancesRequireDB(); err != nil {
		return err
	}
	sets, args := instanceUpdateAssignments(data)
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = NOW()")
	args = append(args, id)
	result := DB.WithContext(ctx).Exec(
		"UPDATE instances SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrInstanceRecordNotFound
	}
	return nil
}

// UpdateInstanceQuota mirrors updateInstanceQuota: it forwards to updateInstance
// with the four quota keys and nothing else.
func UpdateInstanceQuota(ctx context.Context, instanceID int32, data InstanceQuotaUpdate) error {
	return UpdateInstance(ctx, instanceID, InstanceUpdate{
		PortLimit:     data.PortLimit,
		SnapshotLimit: data.SnapshotLimit,
		BackupLimit:   data.BackupLimit,
		SiteLimit:     data.SiteLimit,
	})
}

// UpdateInstanceStatus mirrors updateInstanceStatus.
//
// status 'deleted' uses Prisma's update() semantics: one row, and a missing row is
// an error (ErrInstanceRecordNotFound); it also stamps gpu_released_at from a UTC
// clock so a freed GPU seat is visible immediately.
//
// Every other status updates only rows that are neither deleted nor suspended, and
// matching zero rows is not an error — the TypeScript's updateMany is silent.
func UpdateInstanceStatus(
	ctx context.Context,
	id int32,
	status models.InstanceStatus,
	network *InstanceNetworkUpdate,
) error {
	if err := instancesRequireDB(); err != nil {
		return err
	}

	sets, args := instanceStatusAssignments(status, network)

	if status == models.InstanceStatusDeleted {
		sets = append(sets, "gpu_released_at = ?", "updated_at = NOW()")
		args = append(args, time.Now().UTC(), id)
		result := DB.WithContext(ctx).Exec(
			"UPDATE instances SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrInstanceRecordNotFound
		}
		return nil
	}

	sets = append(sets, "updated_at = NOW()")
	args = append(args, id)
	return DB.WithContext(ctx).Exec(
		"UPDATE instances SET "+strings.Join(sets, ", ")+
			" WHERE id = ? AND "+instanceStatusUpdateFilterSQL(), args...).Error
}

// instanceResourcesAssignments builds the SET list for updateInstanceResources.
func instanceResourcesAssignments(data InstanceResourcesUpdate) ([]string, []any) {
	sets := make([]string, 0, 4)
	args := make([]any, 0, 4)
	if data.CPU != nil {
		sets = append(sets, "cpu = ?")
		args = append(args, *data.CPU)
	}
	if data.Memory != nil {
		sets = append(sets, "memory = ?")
		args = append(args, *data.Memory)
	}
	if data.Disk != nil {
		sets = append(sets, "disk = ?")
		args = append(args, *data.Disk)
	}
	if data.MonthlyTrafficLimit.Set {
		sets = append(sets, "monthly_traffic_limit = ?")
		args = append(args, data.MonthlyTrafficLimit.Value)
	}
	return sets, args
}

// UpdateInstanceResources mirrors updateInstanceResources (CPU / memory / disk /
// monthly traffic limit).
func UpdateInstanceResources(ctx context.Context, id int32, data InstanceResourcesUpdate) error {
	if err := instancesRequireDB(); err != nil {
		return err
	}
	sets, args := instanceResourcesAssignments(data)
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = NOW()")
	args = append(args, id)
	result := DB.WithContext(ctx).Exec(
		"UPDATE instances SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrInstanceRecordNotFound
	}
	return nil
}

// instanceConfigAssignments builds the SET list for updateInstanceConfig.
// The effective bandwidth overrides are applied last, exactly like the
// TypeScript: `limitsIngress` targets configured_limits_ingress, while
// effectiveBandwidth.ingress targets limits_ingress.
func instanceConfigAssignments(
	data InstanceConfigUpdate,
	effectiveBandwidth *InstanceEffectiveBandwidth,
) ([]string, []any) {
	sets := make([]string, 0, 14)
	args := make([]any, 0, 14)

	if data.LimitsRead.Set {
		sets = append(sets, "limits_read = ?")
		args = append(args, data.LimitsRead.Value)
	}
	if data.LimitsWrite.Set {
		sets = append(sets, "limits_write = ?")
		args = append(args, data.LimitsWrite.Value)
	}
	if data.LimitsReadIOps.Set {
		sets = append(sets, "limits_read_iops = ?")
		args = append(args, data.LimitsReadIOps.Value)
	}
	if data.LimitsWriteIOps.Set {
		sets = append(sets, "limits_write_iops = ?")
		args = append(args, data.LimitsWriteIOps.Value)
	}
	if data.LimitsIngress.Set {
		sets = append(sets, "configured_limits_ingress = ?")
		args = append(args, data.LimitsIngress.Value)
	}
	if data.LimitsEgress.Set {
		sets = append(sets, "configured_limits_egress = ?")
		args = append(args, data.LimitsEgress.Value)
	}
	if data.LimitsProcesses.Set {
		sets = append(sets, "limits_processes = ?")
		args = append(args, data.LimitsProcesses.Value)
	}
	if data.LimitsCPUPriority.Set {
		sets = append(sets, "limits_cpu_priority = ?")
		args = append(args, data.LimitsCPUPriority.Value)
	}
	if data.BootAutostart.Set {
		sets = append(sets, "boot_autostart = ?")
		args = append(args, data.BootAutostart.Value)
	}
	if data.BootAutostartPriority.Set {
		sets = append(sets, "boot_autostart_priority = ?")
		args = append(args, data.BootAutostartPriority.Value)
	}
	if data.BootAutostartDelay.Set {
		sets = append(sets, "boot_autostart_delay = ?")
		args = append(args, data.BootAutostartDelay.Value)
	}
	if data.BootHostShutdownTimeout.Set {
		sets = append(sets, "boot_host_shutdown_timeout = ?")
		args = append(args, data.BootHostShutdownTimeout.Value)
	}
	if effectiveBandwidth != nil && effectiveBandwidth.Ingress != nil {
		sets = append(sets, "limits_ingress = ?")
		args = append(args, *effectiveBandwidth.Ingress)
	}
	if effectiveBandwidth != nil && effectiveBandwidth.Egress != nil {
		sets = append(sets, "limits_egress = ?")
		args = append(args, *effectiveBandwidth.Egress)
	}
	return sets, args
}

// UpdateInstanceConfig mirrors updateInstanceConfig. An empty patch (and no
// effective bandwidth) returns without touching the row.
func UpdateInstanceConfig(
	ctx context.Context,
	instanceID int32,
	data InstanceConfigUpdate,
	effectiveBandwidth *InstanceEffectiveBandwidth,
) error {
	if err := instancesRequireDB(); err != nil {
		return err
	}
	sets, args := instanceConfigAssignments(data, effectiveBandwidth)
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = NOW()")
	args = append(args, instanceID)
	result := DB.WithContext(ctx).Exec(
		"UPDATE instances SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrInstanceRecordNotFound
	}
	return nil
}

// instanceSwapAssignments builds the SET list for updateInstanceSwap.
func instanceSwapAssignments(data InstanceSwapUpdate) ([]string, []any) {
	sets := make([]string, 0, 2)
	args := make([]any, 0, 2)
	if data.SwapEnabled != nil {
		sets = append(sets, "swap_enabled = ?")
		args = append(args, *data.SwapEnabled)
	}
	if data.SwapSize.Set {
		sets = append(sets, "swap_size = ?")
		args = append(args, data.SwapSize.Value)
	}
	return sets, args
}

// UpdateInstanceSwap mirrors updateInstanceSwap.
func UpdateInstanceSwap(ctx context.Context, instanceID int32, data InstanceSwapUpdate) error {
	if err := instancesRequireDB(); err != nil {
		return err
	}
	sets, args := instanceSwapAssignments(data)
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = NOW()")
	args = append(args, instanceID)
	result := DB.WithContext(ctx).Exec(
		"UPDATE instances SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrInstanceRecordNotFound
	}
	return nil
}

// SuspendInstance mirrors suspendInstance: status 'suspended' plus the audit
// fields, in one update. suspended_by nil means the system did it.
func SuspendInstance(ctx context.Context, instanceID int32, data SuspendInstanceData) error {
	if err := instancesRequireDB(); err != nil {
		return err
	}
	result := DB.WithContext(ctx).Exec(
		`UPDATE instances SET status = 'suspended', suspended_at = ?, suspended_by = ?,
			suspend_reason = ?, updated_at = NOW() WHERE id = ?`,
		time.Now().UTC(), data.SuspendedBy, data.SuspendReason, instanceID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrInstanceRecordNotFound
	}
	return nil
}

// UnsuspendInstance mirrors unsuspendInstance: back to 'stopped' and clear all
// three suspension markers.
func UnsuspendInstance(ctx context.Context, instanceID int32) error {
	if err := instancesRequireDB(); err != nil {
		return err
	}
	result := DB.WithContext(ctx).Exec(
		`UPDATE instances SET status = 'stopped', suspended_at = NULL, suspended_by = NULL,
			suspend_reason = NULL, updated_at = NOW() WHERE id = ?`, instanceID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrInstanceRecordNotFound
	}
	return nil
}

// UpdateInstanceExpiry mirrors updateInstanceExpiry: a nil expiresAt clears the
// column (free instance, never expires).
func UpdateInstanceExpiry(ctx context.Context, instanceID int32, expiresAt *time.Time) error {
	if err := instancesRequireDB(); err != nil {
		return err
	}
	var value *time.Time
	if expiresAt != nil {
		utc := expiresAt.UTC()
		value = &utc
	}
	result := DB.WithContext(ctx).Exec(
		"UPDATE instances SET expires_at = ?, updated_at = NOW() WHERE id = ?", value, instanceID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrInstanceRecordNotFound
	}
	return nil
}
