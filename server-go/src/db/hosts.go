// Port of server/src/db/hosts.ts (1238 lines, 20 exports, 1:1).
//
// This is the data layer every hosts route group depends on (the route file is
// ported separately). The whole module is ported, not a selection, because the
// instance/packages/instances waves call these functions directly.
//
// Concurrency contract that MUST NOT be "optimised" away
// ------------------------------------------------------
//  1. HostsSelectAndReserveWithLock locks the candidate rows with plain SQL
//     `FOR UPDATE` (server/src/db/hosts.ts:1057-1069) and reserves the resources
//     in the SAME transaction that read them (tx.host.update at :1195). It is a
//     row lock, not pg_advisory_xact_lock — the two have different concurrency
//     semantics (an advisory lock would not serialise against any other writer of
//     the hosts row). NOTE: docs/WAVE_D_SPEC.md §4.4 says `FOR UPDATE SKIP
//     LOCKED`; the code that actually runs does NOT use SKIP LOCKED (SKIP LOCKED
//     only exists in db/public-ipv4.ts, a different table and function). The
//     TypeScript wins here — see hostsBuildLockQuery.
//  2. HostsReservePlanUpgradeCapacityWithLock is an independent lock path (plan
//     upgrade reservation). It must run inside a caller transaction, lock the
//     host row with `FOR UPDATE`, and only write the projected usage when every
//     capacity check passed. The rejection reasons/results are byte-identical to
//     the TypeScript (`host_not_found` / `host_not_online` / `cpu_insufficient`
//     / `memory_insufficient` / `disk_insufficient` with the matching message).
//
// Time contract (WAVE_D_SPEC §0.2b)
// ---------------------------------
// Every timestamp written by hand here is time.Now().UTC(): Prisma persists the
// UTC wall clock into `timestamp(3) without time zone`, while pgx encodes a
// time.Time by its wall clock, so a bare time.Now() on this UTC+8 machine would
// store a value 8 hours in the future. Values are truncated to milliseconds so
// the column's timestamp(3) rounding cannot differ from Prisma's millisecond
// Date.
//
// Package boundary
// ----------------
// `db` must not import `lib` (lib imports db). The three lib/network-modes.ts
// predicates and db/public-ipv4.ts' countFreePublicIpv4ForHost are therefore
// reproduced privately below with the hosts prefix. Storage capacity is a
// registered provider: lib/tunnel_manager.go's init wires
// RegisterHostsStorageCapacityProvider to lib.GetStorageCapacity (the faithful
// port landed in lib/storage_capacity.go). Without a registered provider the
// gate is FAIL-CLOSED (reason `storage_unavailable`) — never "assume there is
// disk", which would oversell the host. That fail-closed branch is also what the
// db package's own tests exercise, because the db test binary does not link lib.
//
// JSON shape
// ----------
// The TypeScript returns hand-built objects whose keys are snake_case and whose
// timestamps are `.toISOString()` strings; different functions return different
// subsets (getAllHosts omits user_id, selectAndReserveHostWithLock omits
// disk_total / tunnel fields). Each subset has its own record type here so a
// marshalled result matches the Node response field-for-field.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// hostsDeletedInstanceStatus mirrors `status: { not: 'deleted' }`.
const hostsDeletedInstanceStatus = models.InstanceStatusDeleted

// Prisma's update()/delete() throw P2025 when the row is gone; GORM reports
// zero affected rows instead. The sentinel keeps that failure visible to routes.
var (
	// ErrHostsDatabaseNotInitialized is returned when db.Init has not run.
	ErrHostsDatabaseNotInitialized = errors.New("hosts: database not initialized")
	// ErrHostsRecordNotFound mirrors Prisma P2025 ("record to update not found").
	ErrHostsRecordNotFound = errors.New("hosts: record to update not found")
)

// Plan upgrade rejection reasons, mirroring PlanUpgradeCapacityReason.
const (
	HostsPlanUpgradeHostNotFound       = "host_not_found"
	HostsPlanUpgradeHostNotOnline      = "host_not_online"
	HostsPlanUpgradeCPUInsufficient    = "cpu_insufficient"
	HostsPlanUpgradeMemoryInsufficient = "memory_insufficient"
	HostsPlanUpgradeDiskInsufficient   = "disk_insufficient"
)

// ==================== records ====================

// HostsSummaryRecord mirrors the object getAllHosts builds. It deliberately has
// no user_id / storage_* / ipv6_* / enable_api keys: the TypeScript object
// literal does not set them, so they are absent from the JSON response.
type HostsSummaryRecord struct {
	ID              int32   `json:"id"`
	Name            string  `json:"name"`
	URL             string  `json:"url"`
	Location        *string `json:"location"`
	CountryCode     string  `json:"country_code"`
	Architecture    string  `json:"architecture"`
	Status          string  `json:"status"`
	CertPath        *string `json:"cert_path"`
	KeyPath         *string `json:"key_path"`
	NATPublicIP     *string `json:"nat_public_ip"`
	NATPublicIPv6   *string `json:"nat_public_ipv6"`
	NATBindIP       *string `json:"nat_bind_ip"`
	NATBindIPv6     *string `json:"nat_bind_ipv6"`
	NATPortStart    *int32  `json:"nat_port_start"`
	NATPortEnd      *int32  `json:"nat_port_end"`
	CPUUsed         int32   `json:"cpu_used"`
	MemoryUsed      int32   `json:"memory_used"`
	DiskUsed        int32   `json:"disk_used"`
	CPUAllowanceMax int32   `json:"cpu_allowance_max"`
	MemoryMax       int32   `json:"memory_max"`
	StorageSize     int32   `json:"storage_size"`
	TunnelEnabled   bool    `json:"tunnel_enabled"`
	TargetHost      string  `json:"target_host"`
	TargetPort      int32   `json:"target_port"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
}

// HostsRecord mirrors the object getHostById builds (the full wire projection).
type HostsRecord struct {
	ID                  int32   `json:"id"`
	UserID              int32   `json:"user_id"`
	Name                string  `json:"name"`
	URL                 string  `json:"url"`
	Location            *string `json:"location"`
	CountryCode         string  `json:"country_code"`
	Architecture        string  `json:"architecture"`
	Status              string  `json:"status"`
	CertPath            *string `json:"cert_path"`
	KeyPath             *string `json:"key_path"`
	NATPublicIP         *string `json:"nat_public_ip"`
	NATPublicIPv6       *string `json:"nat_public_ipv6"`
	NATBindIP           *string `json:"nat_bind_ip"`
	NATBindIPv6         *string `json:"nat_bind_ipv6"`
	NATPortStart        *int32  `json:"nat_port_start"`
	NATPortEnd          *int32  `json:"nat_port_end"`
	CPUUsed             int32   `json:"cpu_used"`
	MemoryUsed          int32   `json:"memory_used"`
	DiskUsed            int32   `json:"disk_used"`
	IPAddress           *string `json:"ip_address"`
	StorageDriver       string  `json:"storage_driver"`
	StorageType         string  `json:"storage_type"`
	StoragePath         *string `json:"storage_path"`
	StorageSize         int32   `json:"storage_size"`
	IPv6Mode            int32   `json:"ipv6_mode"`
	IPv6Subnet          *string `json:"ipv6_subnet"`
	IPv6Gateway         *string `json:"ipv6_gateway"`
	IPv6ParentInterface *string `json:"ipv6_parent_interface"`
	EnableAPI           bool    `json:"enable_api"`
	SysctlConfig        *string `json:"sysctl_config"`
	CPUAllowanceMax     int32   `json:"cpu_allowance_max"`
	MemoryMax           int32   `json:"memory_max"`
	InstanceType        string  `json:"instance_type"`
	CaddyEnabled        bool    `json:"caddy_enabled"`
	CaddyUsername       *string `json:"caddy_username"`
	CaddyPassword       *string `json:"caddy_password"`
	CaddyPort           int32   `json:"caddy_port"`
	TransferEnabled     bool    `json:"transfer_enabled"`
	TrafficResetDay     int32   `json:"traffic_reset_day"`
	NotifyPurchase      bool    `json:"notify_purchase"`
	NotifyRenew         bool    `json:"notify_renew"`
	NotifyDestroy       bool    `json:"notify_destroy"`
	EnableResourcePool  bool    `json:"enable_resource_pool"`
	Announcement        *string `json:"announcement"`
	ProbeURL            *string `json:"probe_url"`
	TunnelEnabled       bool    `json:"tunnel_enabled"`
	TargetHost          string  `json:"target_host"`
	TargetPort          int32   `json:"target_port"`
	CreatedAt           string  `json:"created_at"`
	UpdatedAt           string  `json:"updated_at"`
}

// HostsUserRecord mirrors the smaller object getHostByUserAndName builds.
type HostsUserRecord struct {
	ID            int32   `json:"id"`
	UserID        int32   `json:"user_id"`
	Name          string  `json:"name"`
	URL           string  `json:"url"`
	Location      *string `json:"location"`
	CountryCode   string  `json:"country_code"`
	Architecture  string  `json:"architecture"`
	Status        string  `json:"status"`
	CertPath      *string `json:"cert_path"`
	KeyPath       *string `json:"key_path"`
	NATPublicIP   *string `json:"nat_public_ip"`
	NATPublicIPv6 *string `json:"nat_public_ipv6"`
	NATBindIP     *string `json:"nat_bind_ip"`
	NATBindIPv6   *string `json:"nat_bind_ipv6"`
	NATPortStart  *int32  `json:"nat_port_start"`
	NATPortEnd    *int32  `json:"nat_port_end"`
	CPUUsed       int32   `json:"cpu_used"`
	MemoryUsed    int32   `json:"memory_used"`
	DiskUsed      int32   `json:"disk_used"`
	TunnelEnabled bool    `json:"tunnel_enabled"`
	TargetHost    string  `json:"target_host"`
	TargetPort    int32   `json:"target_port"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

// HostsInstallTokenRecord mirrors the object getHostByInstallToken builds.
// install_token_expire is an ISO-8601 string (the TypeScript returns a Date,
// which JSON.stringify renders via toISOString), null when the column is NULL.
type HostsInstallTokenRecord struct {
	ID                 int32   `json:"id"`
	Name               string  `json:"name"`
	StorageDriver      string  `json:"storage_driver"`
	StorageType        string  `json:"storage_type"`
	StoragePath        *string `json:"storage_path"`
	StorageSize        int32   `json:"storage_size"`
	IPv6Mode           int32   `json:"ipv6_mode"`
	IPv6Subnet         *string `json:"ipv6_subnet"`
	IPv6Gateway        *string `json:"ipv6_gateway"`
	EnableAPI          bool    `json:"enable_api"`
	SysctlConfig       *string `json:"sysctl_config"`
	InstallTokenExpire *string `json:"install_token_expire"`
}

// HostsSelectionRecord mirrors the object selectAvailableHost /
// selectAndReserveHostWithLock build for the caller.
//
// The two functions return different key sets:
//   - selectAvailableHost always sets cpu_allowance_max, memory_max, disk_total,
//     tunnel_enabled, target_host and target_port;
//   - selectAndReserveHostWithLock omits disk_total and the tunnel fields, and
//     omits cpu_allowance_max / memory_max when the raw column is NULL
//     (`?? undefined` disappears from JSON).
//
// Pointers + omitempty reproduce exactly that: the fields are emitted when set
// (even when the value is 0 / false / ""), and absent when left nil.
type HostsSelectionRecord struct {
	ID                  int32   `json:"id"`
	Name                string  `json:"name"`
	URL                 string  `json:"url"`
	Location            *string `json:"location"`
	CountryCode         string  `json:"country_code"`
	Status              string  `json:"status"`
	CertPath            *string `json:"cert_path"`
	KeyPath             *string `json:"key_path"`
	NATPublicIP         *string `json:"nat_public_ip"`
	NATPortStart        *int32  `json:"nat_port_start"`
	NATPortEnd          *int32  `json:"nat_port_end"`
	CPUUsed             int64   `json:"cpu_used"`
	MemoryUsed          int64   `json:"memory_used"`
	DiskTotal           *int64  `json:"disk_total,omitempty"`
	StoragePoolName     *string `json:"storage_pool_name"`
	DiskUsed            int64   `json:"disk_used"`
	CPUAllowanceMax     *int32  `json:"cpu_allowance_max,omitempty"`
	MemoryMax           *int32  `json:"memory_max,omitempty"`
	IPv6Mode            int32   `json:"ipv6_mode"`
	IPv6Subnet          *string `json:"ipv6_subnet"`
	IPv6Gateway         *string `json:"ipv6_gateway"`
	IPv6ParentInterface *string `json:"ipv6_parent_interface"`
	InstanceType        string  `json:"instance_type"`
	TunnelEnabled       *bool   `json:"tunnel_enabled,omitempty"`
	TargetHost          *string `json:"target_host,omitempty"`
	TargetPort          *int32  `json:"target_port,omitempty"`
	CreatedAt           string  `json:"created_at"`
	UpdatedAt           string  `json:"updated_at"`
}

// HostsInstanceResourceSum mirrors the `{cpuUsed, memoryUsed, diskUsed}` object
// returned by calculateHostResourcesFromInstances. The values are JS numbers:
// int64 keeps sums of Int columns exact without wrapping at 2^31.
type HostsInstanceResourceSum struct {
	CPUUsed    int64 `json:"cpuUsed"`
	MemoryUsed int64 `json:"memoryUsed"`
	DiskUsed   int64 `json:"diskUsed"`
}

// HostsInstanceSpec mirrors Pick<Instance, 'id' | 'hostId' | 'cpu' | 'memory' | 'disk'>.
type HostsInstanceSpec struct {
	ID     int32
	HostID int32
	CPU    int32
	Memory int32
	Disk   int32
}

// HostsPlanSpec mirrors Pick<PackagePlan, 'cpu' | 'memory' | 'disk'>.
type HostsPlanSpec struct {
	CPU    int32
	Memory int32
	Disk   int32
}

// HostsCapacityHost is the host projection buildPlanUpgradeCapacityResult reads.
// The resource columns are nullable here on purpose: the TypeScript reads them
// through `?? 0`, so a NULL column degrades to 0 instead of failing the scan.
type HostsCapacityHost struct {
	ID              int32
	Status          string
	CPUUsed         *int32
	MemoryUsed      *int32
	DiskUsed        *int32
	CPUAllowanceMax *int32
	MemoryMax       *int32
	StorageSize     *int32
}

// HostsPlanUpgradeCapacityCheck mirrors PlanUpgradeCapacityCheck.
type HostsPlanUpgradeCapacityCheck struct {
	CanUpgrade     bool                `json:"canUpgrade"`
	Reason         *string             `json:"reason"`
	Message        *string             `json:"message"`
	HostID         int32               `json:"hostId"`
	Deltas         HostsResourceTriple `json:"deltas"`
	CurrentUsage   HostsResourceTriple `json:"currentUsage"`
	ProjectedUsage HostsResourceTriple `json:"projectedUsage"`
	Limits         HostsNullableTriple `json:"limits"`
	Available      HostsNullableTriple `json:"available"`
}

// HostsResourceTriple mirrors `{cpu:number, memory:number, disk:number}`.
type HostsResourceTriple struct {
	CPU    int64 `json:"cpu"`
	Memory int64 `json:"memory"`
	Disk   int64 `json:"disk"`
}

// HostsNullableTriple mirrors `{cpu:number|null, memory:number|null, disk:number|null}`.
type HostsNullableTriple struct {
	CPU    *int64 `json:"cpu"`
	Memory *int64 `json:"memory"`
	Disk   *int64 `json:"disk"`
}

// ==================== create / update payloads ====================

// HostsOptional is a tri-state field: Set=false means "undefined, leave the
// column alone"; Set=true with Value=nil (for pointer element types) means
// "explicit null". Prisma distinguishes the two, so a bare *T would lose it.
type HostsOptional[T any] struct {
	Set   bool
	Value T
}

// HostsValue marks a field as explicitly provided.
func HostsValue[T any](value T) HostsOptional[T] {
	return HostsOptional[T]{Set: true, Value: value}
}

// HostsNull marks a nullable field as explicitly cleared.
func HostsNull[T any]() HostsOptional[T] {
	var zero T
	return HostsOptional[T]{Set: true, Value: zero}
}

// HostsCreateData mirrors the inline argument object of createHost.
type HostsCreateData struct {
	UserID              int32
	Name                string
	URL                 string
	Location            *string
	CountryCode         *string
	Architecture        *string
	Tags                []string
	CertPath            *string
	KeyPath             *string
	NATPublicIP         *string
	NATPublicIPv6       *string
	NATBindIP           *string
	NATBindIPv6         *string
	NATPortStart        *int32
	NATPortEnd          *int32
	IPAddress           *string
	StorageDriver       *string
	StorageType         *string
	StoragePath         *string
	StorageSize         *int32
	IPv6Mode            *int32
	IPv6Subnet          *string
	IPv6Gateway         *string
	IPv6ParentInterface *string
	EnableAPI           *bool // nil defaults to true (`data.enableApi !== undefined ? … : true`)
	SysctlConfig        *string
	TunnelEnabled       *bool
	TargetHost          *string
	TargetPort          *int32
	InstallToken        *string
	InstallTokenExpire  *time.Time
	IsInstalled         *bool
	CertDownloadCount   *int32
	CertDownloadExpire  *time.Time
	CPUAllowanceMax     *int32
	MemoryMax           *int32
	InstanceType        *string
}

// HostsUpdateData mirrors the inline argument object of updateHost.
type HostsUpdateData struct {
	Name                HostsOptional[string]
	URL                 HostsOptional[string]
	Location            HostsOptional[*string]
	CountryCode         HostsOptional[string]
	Tags                HostsOptional[[]string]
	CertPath            HostsOptional[*string]
	KeyPath             HostsOptional[*string]
	NATPublicIP         HostsOptional[*string]
	NATPublicIPv6       HostsOptional[*string]
	NATBindIP           HostsOptional[*string]
	NATBindIPv6         HostsOptional[*string]
	NATPortStart        HostsOptional[*int32]
	NATPortEnd          HostsOptional[*int32]
	IPv6ParentInterface HostsOptional[*string]
	IPv6Subnet          HostsOptional[*string]
	TrafficResetDay     HostsOptional[int32]
	NotifyPurchase      HostsOptional[bool]
	NotifyRenew         HostsOptional[bool]
	NotifyDestroy       HostsOptional[bool]
	TunnelEnabled       HostsOptional[bool]
	TargetHost          HostsOptional[string]
	TargetPort          HostsOptional[int32]
}

// HostsResourceUpdate mirrors the resources argument of updateHostResources.
type HostsResourceUpdate struct {
	CPUUsed         *int64
	MemoryUsed      *int64
	DiskUsed        *int64
	CPUAllowanceMax *int64
	MemoryMax       *int64
	InstanceType    *string
	Architecture    *string
}

// HostsResourceDeltas mirrors the deltas argument of incrementHostResourceUsage.
type HostsResourceDeltas struct {
	CPUUsed    int64
	MemoryUsed int64
	DiskUsed   int64
}

// HostsSelectOptions mirrors the options object of selectAvailableHost.
type HostsSelectOptions struct {
	PackageHostIDs []int32
	NodeSelectors  []string
	CPU            int64
	Memory         int64
	Disk           int64
	HostID         *int32
	OwnerID        *int32
	PackageID      *int32
	NetworkMode    *string
}

// HostsReserveOptions mirrors the options object of selectAndReserveHostWithLock.
type HostsReserveOptions struct {
	PackageHostIDs []int32
	NodeSelectors  []string
	CPU            int64
	Memory         int64
	Disk           int64
	HostID         *int32
	OwnerID        *int32
	PortCount      int32
	PackagePlanID  *int32
	PackageID      *int32
	NetworkMode    *string
}

// HostsStorageCapacityOptions mirrors the options of getStorageCapacity.
type HostsStorageCapacityOptions struct {
	PackageID  *int32
	PoolName   *string
	RequiredMB *int64
}

// HostsStorageCapacityProvider computes getStorageCapacity for one host. It is a
// registration hook because the implementation belongs to lib/storage-capacity.ts
// (which depends on host-service-status + storage-capacity-policy) and `db`
// cannot import `lib`. See the package comment for the fail-closed default.
type HostsStorageCapacityProvider func(
	ctx context.Context,
	client *gorm.DB,
	hostID int32,
	options HostsStorageCapacityOptions,
) (ResourcePoolStorageCapacity, error)

var hostsStorageCapacityProvider HostsStorageCapacityProvider

// RegisterHostsStorageCapacityProvider wires the real getStorageCapacity
// implementation into the db layer. Passing nil restores the fail-closed default.
func RegisterHostsStorageCapacityProvider(provider HostsStorageCapacityProvider) {
	hostsStorageCapacityProvider = provider
}

// ==================== helpers ====================

func hostsClient(client *gorm.DB) *gorm.DB {
	if client == nil {
		return DB
	}
	return client
}

func hostsRequireDB() error {
	if DB == nil {
		return ErrHostsDatabaseNotInitialized
	}
	return nil
}

// hostsNow mirrors a Prisma client-side timestamp, truncated to the millisecond
// precision of the timestamp(3) columns.
func hostsNow() time.Time {
	return time.Now().UTC().Truncate(time.Millisecond)
}

// hostsISOTime mirrors Date.prototype.toISOString (UTC, millisecond precision).
func hostsISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// hostsISOTimePtr mirrors `value?.toISOString() ?? null`.
func hostsISOTimePtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := hostsISOTime(*value)
	return &formatted
}

// hostsMaxInt64 mirrors Math.max for the resource sums.
func hostsMaxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

// hostsInt32FromJS converts a JS number to the int column type. A value outside
// the PostgreSQL INT4 range makes the Node backend fail the UPDATE (22003), so
// Go must fail instead of silently wrapping.
func hostsInt32FromJS(value int64, column string) (int32, error) {
	if value > math.MaxInt32 || value < math.MinInt32 {
		return 0, fmt.Errorf("hosts: %s value %d is out of range for an integer column", column, value)
	}
	return int32(value), nil
}

// hostsJSONTags mirrors `(data.tags || []) as any` on a Json column.
func hostsJSONTags(tags []string) datatypes.JSON {
	if tags == nil {
		tags = []string{}
	}
	raw, err := json.Marshal(tags)
	if err != nil {
		return datatypes.JSON("[]")
	}
	return datatypes.JSON(raw)
}

// hostsTagsOf mirrors `(host.tags as string[]) || []`.
func hostsTagsOf(raw datatypes.JSON) []string {
	if len(raw) == 0 {
		return nil
	}
	var tags []string
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil
	}
	return tags
}

// hostsHasAllTags mirrors `nodeSelectors.every(tag => hostTags.includes(tag))`.
func hostsHasAllTags(hostTags, selectors []string) bool {
	for _, selector := range selectors {
		found := false
		for _, tag := range hostTags {
			if tag == selector {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// hostsCPUQuotaAllows mirrors the CPU gate of both selection functions:
//
//	A host without a positive cpu_allowance_max is unusable, and the requested
//	CPU must fit in the remaining quota (exactly `used + cpu > max` rejects, so a
//	request that lands exactly on the limit passes).
func hostsCPUQuotaAllows(allowanceMax *int32, used, requested int64) bool {
	if allowanceMax == nil || *allowanceMax <= 0 {
		return false
	}
	return used+requested <= int64(*allowanceMax)
}

// hostsMemoryQuotaAllows mirrors the memory gate (same shape as the CPU gate).
func hostsMemoryQuotaAllows(memoryMax *int32, used, requested int64) bool {
	if memoryMax == nil || *memoryMax <= 0 {
		return false
	}
	return used+requested <= int64(*memoryMax)
}

// hostsRoutedIpv6Configured mirrors `(!host.ipv6_subnet || !host.ipv6_parent_interface)`:
// an empty string is falsy in the TypeScript as well, so it counts as missing.
func hostsRoutedIpv6Configured(subnet, parentInterface *string) bool {
	return subnet != nil && *subnet != "" && parentInterface != nil && *parentInterface != ""
}

// hostsPublicIpv4Allows mirrors `freePublicIpv4Count <= 0 → continue`.
func hostsPublicIpv4Allows(freeCount int64) bool {
	return freeCount > 0
}

// hostsNATPortsAllow mirrors the NAT port gate of selectAndReserveHostWithLock:
// the pool is nat_port_end - nat_port_start + 1 (0 when either bound is NULL) and
// the request must not exceed what is left.
func hostsNATPortsAllow(natPortStart, natPortEnd *int32, used, portCount int32) bool {
	if portCount <= 0 {
		return true
	}
	total := int64(0)
	if natPortStart != nil && natPortEnd != nil {
		total = int64(*natPortEnd) - int64(*natPortStart) + 1
	}
	return int64(used)+int64(portCount) <= total
}

// hostsNormalizeNetworkMode mirrors lib.normalizeNetworkMode (db cannot import lib).
func hostsNormalizeNetworkMode(value *string) string {
	if value != nil {
		switch *value {
		case "nat_ipv6", "nat_ipv6_nat", "ipv6_only", "ipv6_nat", "public_ipv4", "public_ipv4_ipv6":
			return *value
		}
	}
	return "nat"
}

// hostsNetworkModeNeedsPublicIpv4 mirrors networkModeNeedsPublicIpv4.
func hostsNetworkModeNeedsPublicIpv4(mode *string) bool {
	normalized := hostsNormalizeNetworkMode(mode)
	return normalized == "public_ipv4" || normalized == "public_ipv4_ipv6"
}

// hostsNetworkModeNeedsRoutedIpv6 mirrors networkModeNeedsRoutedIpv6.
func hostsNetworkModeNeedsRoutedIpv6(mode *string) bool {
	normalized := hostsNormalizeNetworkMode(mode)
	return normalized == "nat_ipv6" || normalized == "ipv6_only" || normalized == "public_ipv4_ipv6"
}

// hostsCountFreePublicIpv4 mirrors db/public-ipv4.ts countFreePublicIpv4ForHost
// (db cannot import lib, and db/public_ipv4.go does not exist yet).
func hostsCountFreePublicIpv4(ctx context.Context, client *gorm.DB, hostID int32) (int64, error) {
	var row struct {
		Count int64 `gorm:"column:count"`
	}
	err := hostsClient(client).WithContext(ctx).Raw(
		`SELECT COUNT(*)::bigint AS count
		   FROM public_ipv4_addresses a
		   LEFT JOIN public_ipv4_pools p ON p.id = a.pool_id
		  WHERE a.host_id = ?
		    AND a.status = 'free'
		    AND (a.pool_id IS NULL OR p.enabled = true)`, hostID,
	).Scan(&row).Error
	if err != nil {
		return 0, err
	}
	return row.Count, nil
}

// hostsStorageCapacity mirrors getStorageCapacity through the registered provider.
func hostsStorageCapacity(
	ctx context.Context,
	client *gorm.DB,
	hostID int32,
	options HostsStorageCapacityOptions,
) (ResourcePoolStorageCapacity, error) {
	if hostsStorageCapacityProvider == nil {
		// Fail closed: without the storage-capacity policy we cannot prove there is
		// free disk, and pretending there is would oversell the host.
		reason := ResourcePoolStorageUnavailable
		return ResourcePoolStorageCapacity{Reason: &reason}, nil
	}
	return hostsStorageCapacityProvider(ctx, client, hostID, options)
}

// hostsInstanceStorageCapacity mirrors getInstanceStorageCapacity.
func hostsInstanceStorageCapacity(
	ctx context.Context,
	client *gorm.DB,
	instanceID int32,
	requiredMB int64,
) (*ResourcePoolStorageCapacity, error) {
	var row struct {
		HostID          int32   `gorm:"column:host_id"`
		StoragePoolName *string `gorm:"column:storage_pool_name"`
	}
	err := hostsClient(client).WithContext(ctx).
		Table("instances").
		Select("host_id, storage_pool_name").
		Where("id = ?", instanceID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Instances without a reported root pool cannot be capacity-checked; the
	// TypeScript returns null and the caller treats it as "0 MB available".
	if row.StoragePoolName == nil || *row.StoragePoolName == "" {
		return nil, nil
	}
	capacity, err := hostsStorageCapacity(ctx, client, row.HostID, HostsStorageCapacityOptions{
		PoolName:   row.StoragePoolName,
		RequiredMB: &requiredMB,
	})
	if err != nil {
		return nil, err
	}
	return &capacity, nil
}

// ==================== reads ====================

// HostsGetAll mirrors getAllHosts.
func HostsGetAll(ctx context.Context) ([]HostsSummaryRecord, error) {
	if err := hostsRequireDB(); err != nil {
		return nil, err
	}

	var hosts []models.Host
	if err := DB.WithContext(ctx).Order("id ASC").Find(&hosts).Error; err != nil {
		return nil, err
	}

	records := make([]HostsSummaryRecord, 0, len(hosts))
	for _, host := range hosts {
		records = append(records, hostsSummaryRecord(host))
	}
	return records, nil
}

func hostsSummaryRecord(host models.Host) HostsSummaryRecord {
	return HostsSummaryRecord{
		ID:              host.ID,
		Name:            host.Name,
		URL:             host.URL,
		Location:        host.Location,
		CountryCode:     host.CountryCode,
		Architecture:    host.Architecture,
		Status:          string(host.Status),
		CertPath:        host.CertPath,
		KeyPath:         host.KeyPath,
		NATPublicIP:     host.NATPublicIP,
		NATPublicIPv6:   host.NATPublicIPv6,
		NATBindIP:       host.NATBindIP,
		NATBindIPv6:     host.NATBindIPv6,
		NATPortStart:    host.NATPortStart,
		NATPortEnd:      host.NATPortEnd,
		CPUUsed:         host.CPUUsed,
		MemoryUsed:      host.MemoryUsed,
		DiskUsed:        host.DiskUsed,
		CPUAllowanceMax: host.CPUAllowanceMax,
		MemoryMax:       host.MemoryMax,
		StorageSize:     host.StorageSize,
		TunnelEnabled:   host.TunnelEnabled,
		TargetHost:      host.TargetHost,
		TargetPort:      host.TargetPort,
		CreatedAt:       hostsISOTime(host.CreatedAt),
		UpdatedAt:       hostsISOTime(host.UpdatedAt),
	}
}

// HostsHasAvailable mirrors hasAvailableHosts. A nil userID skips the owner
// filter; a non-nil 0 filters on user_id = 0 (`userId !== undefined`).
func HostsHasAvailable(ctx context.Context, userID *int32) (bool, error) {
	if err := hostsRequireDB(); err != nil {
		return false, err
	}

	query := DB.WithContext(ctx).Model(&models.Host{}).Where("status <> ?", string(models.HostStatusOffline))
	if userID != nil {
		query = query.Where("user_id = ?", *userID)
	}

	var count int64
	if err := query.Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// HostsGetByID mirrors getHostById. A missing row returns (nil, nil), matching
// the TypeScript's `if (!host) return null`.
func HostsGetByID(ctx context.Context, id int32) (*HostsRecord, error) {
	if err := hostsRequireDB(); err != nil {
		return nil, err
	}

	var host models.Host
	err := DB.WithContext(ctx).Where("id = ?", id).Take(&host).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	record := hostsRecord(host)
	return &record, nil
}

func hostsRecord(host models.Host) HostsRecord {
	return HostsRecord{
		ID:                  host.ID,
		UserID:              host.UserID,
		Name:                host.Name,
		URL:                 host.URL,
		Location:            host.Location,
		CountryCode:         host.CountryCode,
		Architecture:        host.Architecture,
		Status:              string(host.Status),
		CertPath:            host.CertPath,
		KeyPath:             host.KeyPath,
		NATPublicIP:         host.NATPublicIP,
		NATPublicIPv6:       host.NATPublicIPv6,
		NATBindIP:           host.NATBindIP,
		NATBindIPv6:         host.NATBindIPv6,
		NATPortStart:        host.NATPortStart,
		NATPortEnd:          host.NATPortEnd,
		CPUUsed:             host.CPUUsed,
		MemoryUsed:          host.MemoryUsed,
		DiskUsed:            host.DiskUsed,
		IPAddress:           host.IPAddress,
		StorageDriver:       host.StorageDriver,
		StorageType:         host.StorageType,
		StoragePath:         host.StoragePath,
		StorageSize:         host.StorageSize,
		IPv6Mode:            host.IPv6Mode,
		IPv6Subnet:          host.IPv6Subnet,
		IPv6Gateway:         host.IPv6Gateway,
		IPv6ParentInterface: host.IPv6ParentInterface,
		EnableAPI:           host.EnableAPI,
		SysctlConfig:        host.SysctlConfig,
		CPUAllowanceMax:     host.CPUAllowanceMax,
		MemoryMax:           host.MemoryMax,
		InstanceType:        string(host.InstanceType),
		CaddyEnabled:        host.CaddyEnabled,
		CaddyUsername:       host.CaddyUsername,
		CaddyPassword:       host.CaddyPassword,
		CaddyPort:           host.CaddyPort,
		TransferEnabled:     host.TransferEnabled,
		TrafficResetDay:     host.TrafficResetDay,
		NotifyPurchase:      host.NotifyPurchase,
		NotifyRenew:         host.NotifyRenew,
		NotifyDestroy:       host.NotifyDestroy,
		EnableResourcePool:  host.EnableResourcePool,
		Announcement:        host.Announcement,
		ProbeURL:            host.ProbeURL,
		TunnelEnabled:       host.TunnelEnabled,
		TargetHost:          host.TargetHost,
		TargetPort:          host.TargetPort,
		CreatedAt:           hostsISOTime(host.CreatedAt),
		UpdatedAt:           hostsISOTime(host.UpdatedAt),
	}
}

// HostsGetByUserAndName mirrors getHostByUserAndName (the userId_name unique key).
func HostsGetByUserAndName(ctx context.Context, userID int32, name string) (*HostsUserRecord, error) {
	if err := hostsRequireDB(); err != nil {
		return nil, err
	}

	var host models.Host
	err := DB.WithContext(ctx).Where("user_id = ? AND name = ?", userID, name).Take(&host).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	record := HostsUserRecord{
		ID:            host.ID,
		UserID:        host.UserID,
		Name:          host.Name,
		URL:           host.URL,
		Location:      host.Location,
		CountryCode:   host.CountryCode,
		Architecture:  host.Architecture,
		Status:        string(host.Status),
		CertPath:      host.CertPath,
		KeyPath:       host.KeyPath,
		NATPublicIP:   host.NATPublicIP,
		NATPublicIPv6: host.NATPublicIPv6,
		NATBindIP:     host.NATBindIP,
		NATBindIPv6:   host.NATBindIPv6,
		NATPortStart:  host.NATPortStart,
		NATPortEnd:    host.NATPortEnd,
		CPUUsed:       host.CPUUsed,
		MemoryUsed:    host.MemoryUsed,
		DiskUsed:      host.DiskUsed,
		TunnelEnabled: host.TunnelEnabled,
		TargetHost:    host.TargetHost,
		TargetPort:    host.TargetPort,
		CreatedAt:     hostsISOTime(host.CreatedAt),
		UpdatedAt:     hostsISOTime(host.UpdatedAt),
	}
	return &record, nil
}

// HostsGetByInstallToken mirrors getHostByInstallToken (the install_token unique key).
func HostsGetByInstallToken(ctx context.Context, token string) (*HostsInstallTokenRecord, error) {
	if err := hostsRequireDB(); err != nil {
		return nil, err
	}

	var host models.Host
	err := DB.WithContext(ctx).Where("install_token = ?", token).Take(&host).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &HostsInstallTokenRecord{
		ID:                 host.ID,
		Name:               host.Name,
		StorageDriver:      host.StorageDriver,
		StorageType:        host.StorageType,
		StoragePath:        host.StoragePath,
		StorageSize:        host.StorageSize,
		IPv6Mode:           host.IPv6Mode,
		IPv6Subnet:         host.IPv6Subnet,
		IPv6Gateway:        host.IPv6Gateway,
		EnableAPI:          host.EnableAPI,
		SysctlConfig:       host.SysctlConfig,
		InstallTokenExpire: hostsISOTimePtr(host.InstallTokenExpire),
	}, nil
}

// ==================== writes ====================

// hostsCreateValues mirrors the createData literal of createHost (pure, so the
// defaults are unit-tested without a database).
//
// Indentation is kept from the original literal for reviewability.
func hostsCreateValues(data HostsCreateData, now time.Time) map[string]any {
	return map[string]any{
		"user_id":               data.UserID,
		"name":                  data.Name,
		"url":                   data.URL,
		"location":              data.Location,
		"country_code":          hostsStringDefault(data.CountryCode, "us"),
		"architecture":          hostsStringDefault(data.Architecture, "x86_64"),
		"tags":                  hostsJSONTags(data.Tags),
		"cert_path":             data.CertPath,
		"key_path":              data.KeyPath,
		"nat_public_ip":         data.NATPublicIP,
		"nat_public_ipv6":       data.NATPublicIPv6,
		"nat_bind_ip":           data.NATBindIP,
		"nat_bind_ipv6":         data.NATBindIPv6,
		"nat_port_start":        data.NATPortStart,
		"nat_port_end":          data.NATPortEnd,
		"ip_address":            data.IPAddress,
		"storage_driver":        hostsStringDefault(data.StorageDriver, "zfs"),
		"storage_type":          hostsStringDefault(data.StorageType, "loop"),
		"storage_path":          data.StoragePath,
		"storage_size":          hostsInt32Default(data.StorageSize, 60),
		"ipv6_mode":             hostsInt32Default(data.IPv6Mode, 1),
		"ipv6_subnet":           data.IPv6Subnet,
		"ipv6_gateway":          data.IPv6Gateway,
		"ipv6_parent_interface": data.IPv6ParentInterface,
		"enable_api":            hostsBoolDefaultTrue(data.EnableAPI),
		"sysctl_config":         data.SysctlConfig,
		"tunnel_enabled":        hostsBoolDefault(data.TunnelEnabled, false),
		"target_host":           hostsStringDefault(data.TargetHost, "127.0.0.1"),
		"target_port":           hostsInt32Default(data.TargetPort, 8443),
		"install_token":         data.InstallToken,
		"install_token_expire":  data.InstallTokenExpire,
		"is_installed":          hostsBoolDefault(data.IsInstalled, false),
		"cert_download_count":   hostsInt32Default(data.CertDownloadCount, 0),
		"cert_download_expire":  data.CertDownloadExpire,
		"cpu_allowance_max":     hostsInt32Default(data.CPUAllowanceMax, 0),
		"memory_max":            hostsInt32Default(data.MemoryMax, 0),
		"instance_type":         hostsStringDefault(data.InstanceType, "container"),
		"status":                string(models.HostStatusOffline),
		"created_at":            now,
		"updated_at":            now,
	}
}

// HostsCreate mirrors createHost and returns the new host id.
//
// The insert is built from an explicit column map (not a models.Host struct) so
// the columns createHost does not set keep their database defaults — a struct
// insert would write caddy_port=0, traffic_reset_day=0, notify_purchase=false …
// and silently change behaviour.
func HostsCreate(ctx context.Context, data HostsCreateData, client *gorm.DB) (int32, error) {
	if err := hostsRequireDB(); err != nil {
		return 0, err
	}

	values := hostsCreateValues(data, hostsNow())

	columns := make([]string, 0, len(values))
	for column := range values {
		columns = append(columns, column)
	}
	sort.Strings(columns)
	placeholders := make([]string, len(columns))
	args := make([]any, len(columns))
	for index, column := range columns {
		placeholders[index] = "?"
		args[index] = values[column]
	}

	var id int32
	statement := "INSERT INTO hosts (" + strings.Join(columns, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ") RETURNING id"
	if err := hostsClient(client).WithContext(ctx).Raw(statement, args...).Scan(&id).Error; err != nil {
		return 0, err
	}
	return id, nil
}

func hostsStringDefault(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

func hostsInt32Default(value *int32, fallback int32) int32 {
	if value == nil {
		return fallback
	}
	return *value
}

func hostsBoolDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func hostsBoolDefaultTrue(value *bool) bool {
	if value == nil {
		return true
	}
	return *value
}

// hostsUpdateValues mirrors the updateData literal of updateHost (pure).
//
// An empty map is the `Object.keys(updateData).length === 0` early return: the
// caller must not touch the row (Prisma would only bump @updatedAt).
func hostsUpdateValues(data HostsUpdateData) map[string]any {
	values := map[string]any{}
	if data.Name.Set {
		values["name"] = data.Name.Value
	}
	if data.URL.Set {
		values["url"] = data.URL.Value
	}
	if data.Location.Set {
		values["location"] = data.Location.Value
	}
	if data.CountryCode.Set {
		values["country_code"] = data.CountryCode.Value
	}
	if data.Tags.Set {
		values["tags"] = hostsJSONTags(data.Tags.Value)
	}
	if data.CertPath.Set {
		values["cert_path"] = data.CertPath.Value
	}
	if data.KeyPath.Set {
		values["key_path"] = data.KeyPath.Value
	}
	if data.NATPublicIP.Set {
		values["nat_public_ip"] = data.NATPublicIP.Value
	}
	if data.NATPublicIPv6.Set {
		values["nat_public_ipv6"] = data.NATPublicIPv6.Value
	}
	if data.NATBindIP.Set {
		values["nat_bind_ip"] = data.NATBindIP.Value
	}
	if data.NATBindIPv6.Set {
		values["nat_bind_ipv6"] = data.NATBindIPv6.Value
	}
	if data.NATPortStart.Set {
		values["nat_port_start"] = data.NATPortStart.Value
	}
	if data.NATPortEnd.Set {
		values["nat_port_end"] = data.NATPortEnd.Value
	}
	if data.IPv6ParentInterface.Set {
		values["ipv6_parent_interface"] = data.IPv6ParentInterface.Value
	}
	if data.IPv6Subnet.Set {
		values["ipv6_subnet"] = data.IPv6Subnet.Value
	}
	if data.TrafficResetDay.Set {
		values["traffic_reset_day"] = data.TrafficResetDay.Value
	}
	if data.NotifyPurchase.Set {
		values["notify_purchase"] = data.NotifyPurchase.Value
	}
	if data.NotifyRenew.Set {
		values["notify_renew"] = data.NotifyRenew.Value
	}
	if data.NotifyDestroy.Set {
		values["notify_destroy"] = data.NotifyDestroy.Value
	}
	if data.TunnelEnabled.Set {
		values["tunnel_enabled"] = data.TunnelEnabled.Value
	}
	if data.TargetHost.Set {
		values["target_host"] = data.TargetHost.Value
	}
	if data.TargetPort.Set {
		values["target_port"] = data.TargetPort.Value
	}

	return values
}

// HostsUpdate mirrors updateHost. The empty-update early return is preserved:
// Prisma would otherwise run an UPDATE that only bumps updated_at.
func HostsUpdate(ctx context.Context, id int32, data HostsUpdateData, client *gorm.DB) error {
	if err := hostsRequireDB(); err != nil {
		return err
	}

	values := hostsUpdateValues(data)
	if len(values) == 0 {
		return nil
	}
	values["updated_at"] = hostsNow()

	return hostsExecExpectRow(
		hostsClient(client).WithContext(ctx).Model(&models.Host{}).Where("id = ?", id).Updates(values),
	)
}

// HostsUpdateStatus mirrors updateHostStatus.
func HostsUpdateStatus(ctx context.Context, id int32, status models.HostStatus) error {
	if err := hostsRequireDB(); err != nil {
		return err
	}

	return hostsExecExpectRow(
		DB.WithContext(ctx).Model(&models.Host{}).Where("id = ?", id).Updates(map[string]any{
			"status":     string(status),
			"updated_at": hostsNow(),
		}),
	)
}

// HostsCalculateResourcesFromInstances mirrors calculateHostResourcesFromInstances
// (SUM of every non-deleted instance on the host).
func HostsCalculateResourcesFromInstances(ctx context.Context, hostID int32) (HostsInstanceResourceSum, error) {
	if err := hostsRequireDB(); err != nil {
		return HostsInstanceResourceSum{}, err
	}

	var row struct {
		CPUUsed    int64 `gorm:"column:cpu_used"`
		MemoryUsed int64 `gorm:"column:memory_used"`
		DiskUsed   int64 `gorm:"column:disk_used"`
	}
	err := DB.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(cpu), 0) AS cpu_used,
		        COALESCE(SUM(memory), 0) AS memory_used,
		        COALESCE(SUM(disk), 0) AS disk_used
		   FROM instances
		  WHERE host_id = ? AND status <> ?`, hostID, string(hostsDeletedInstanceStatus),
	).Scan(&row).Error
	if err != nil {
		return HostsInstanceResourceSum{}, err
	}

	return HostsInstanceResourceSum{CPUUsed: row.CPUUsed, MemoryUsed: row.MemoryUsed, DiskUsed: row.DiskUsed}, nil
}

// hostsResourceUpdateValues mirrors the updateData literal of
// updateHostResources (pure; an empty map is the early return).
func hostsResourceUpdateValues(resources HostsResourceUpdate) (map[string]any, error) {
	values := map[string]any{}
	var err error
	if resources.CPUUsed != nil {
		if values["cpu_used"], err = hostsInt32FromJS(*resources.CPUUsed, "cpu_used"); err != nil {
			return nil, err
		}
	}
	if resources.MemoryUsed != nil {
		if values["memory_used"], err = hostsInt32FromJS(*resources.MemoryUsed, "memory_used"); err != nil {
			return nil, err
		}
	}
	if resources.DiskUsed != nil {
		if values["disk_used"], err = hostsInt32FromJS(*resources.DiskUsed, "disk_used"); err != nil {
			return nil, err
		}
	}
	if resources.CPUAllowanceMax != nil {
		if values["cpu_allowance_max"], err = hostsInt32FromJS(*resources.CPUAllowanceMax, "cpu_allowance_max"); err != nil {
			return nil, err
		}
	}
	if resources.MemoryMax != nil {
		if values["memory_max"], err = hostsInt32FromJS(*resources.MemoryMax, "memory_max"); err != nil {
			return nil, err
		}
	}
	if resources.InstanceType != nil {
		values["instance_type"] = *resources.InstanceType
	}
	if resources.Architecture != nil {
		values["architecture"] = *resources.Architecture
	}

	return values, nil
}

// HostsUpdateResources mirrors updateHostResources (empty update early-returns).
func HostsUpdateResources(ctx context.Context, id int32, resources HostsResourceUpdate, client *gorm.DB) error {
	if err := hostsRequireDB(); err != nil {
		return err
	}

	values, err := hostsResourceUpdateValues(resources)
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return nil
	}
	values["updated_at"] = hostsNow()

	return hostsExecExpectRow(
		hostsClient(client).WithContext(ctx).Model(&models.Host{}).Where("id = ?", id).Updates(values),
	)
}

// HostsIncrementResourceUsage mirrors incrementHostResourceUsage: atomic
// increments inside a transaction (never a stale read-modify-write).
func HostsIncrementResourceUsage(ctx context.Context, id int32, deltas HostsResourceDeltas) error {
	if err := hostsRequireDB(); err != nil {
		return err
	}

	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return hostsExecExpectRow(
			tx.Model(&models.Host{}).Where("id = ?", id).Updates(map[string]any{
				"cpu_used":    gorm.Expr("cpu_used + ?", deltas.CPUUsed),
				"memory_used": gorm.Expr("memory_used + ?", deltas.MemoryUsed),
				"disk_used":   gorm.Expr("disk_used + ?", deltas.DiskUsed),
				"updated_at":  hostsNow(),
			}),
		)
	})
}

// HostsIncrementPortCount mirrors incrementHostPortCount (count defaults to 1).
func HostsIncrementPortCount(ctx context.Context, hostID int32, count ...int32) error {
	if err := hostsRequireDB(); err != nil {
		return err
	}

	return hostsExecExpectRow(
		DB.WithContext(ctx).Model(&models.Host{}).Where("id = ?", hostID).Updates(map[string]any{
			"nat_ports_used_count": gorm.Expr("nat_ports_used_count + ?", hostsExtraCount(count)),
			"updated_at":           hostsNow(),
		}),
	)
}

// HostsDecrementPortCount mirrors decrementHostPortCount. It reads the current
// value first (so the count never goes below 0) exactly like the TypeScript —
// including its non-atomic read-then-write window.
func HostsDecrementPortCount(ctx context.Context, hostID int32, count ...int32) error {
	if err := hostsRequireDB(); err != nil {
		return err
	}

	var row struct {
		NATPortsUsedCount int32 `gorm:"column:nat_ports_used_count"`
	}
	err := DB.WithContext(ctx).Model(&models.Host{}).
		Select("nat_ports_used_count").
		Where("id = ?", hostID).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	newCount := int64(row.NATPortsUsedCount) - int64(hostsExtraCount(count))
	if newCount < 0 {
		newCount = 0
	}
	value, err := hostsInt32FromJS(newCount, "nat_ports_used_count")
	if err != nil {
		return err
	}

	return hostsExecExpectRow(
		DB.WithContext(ctx).Model(&models.Host{}).Where("id = ?", hostID).Updates(map[string]any{
			"nat_ports_used_count": value,
			"updated_at":           hostsNow(),
		}),
	)
}

func hostsExtraCount(count []int32) int32 {
	if len(count) == 0 {
		return 1
	}
	return count[0]
}

// HostsDelete mirrors deleteHost: port mappings first (RESTRICT), then every
// instance on the host (CASCADE removes snapshots/backups/…), then the host
// (CASCADE removes package_hosts). The TypeScript runs the three deletes
// sequentially without a transaction, so this does too.
func HostsDelete(ctx context.Context, id int32) error {
	if err := hostsRequireDB(); err != nil {
		return err
	}

	if err := DB.WithContext(ctx).Where("host_id = ?", id).Delete(&models.PortMapping{}).Error; err != nil {
		return err
	}
	if err := DB.WithContext(ctx).Where("host_id = ?", id).Delete(&models.Instance{}).Error; err != nil {
		return err
	}
	return hostsExecExpectRow(DB.WithContext(ctx).Where("id = ?", id).Delete(&models.Host{}))
}

// HostsInstanceCount mirrors getInstanceCountByHost.
func HostsInstanceCount(ctx context.Context, hostID int32) (int64, error) {
	if err := hostsRequireDB(); err != nil {
		return 0, err
	}

	var count int64
	err := DB.WithContext(ctx).Model(&models.Instance{}).
		Where("host_id = ? AND status <> ?", hostID, string(hostsDeletedInstanceStatus)).
		Count(&count).Error
	if err != nil {
		return 0, err
	}
	return count, nil
}

// HostsInstances mirrors getInstancesByHost (full rows, camelCase JSON, ordered
// by id ascending).
func HostsInstances(ctx context.Context, hostID int32) ([]models.Instance, error) {
	if err := hostsRequireDB(); err != nil {
		return nil, err
	}

	instances := make([]models.Instance, 0, 8)
	err := DB.WithContext(ctx).
		Where("host_id = ? AND status <> ?", hostID, string(hostsDeletedInstanceStatus)).
		Order("id ASC").
		Find(&instances).Error
	if err != nil {
		return nil, err
	}
	return instances, nil
}

// hostsExecExpectRow maps a zero-row UPDATE/DELETE to ErrHostsRecordNotFound
// (Prisma P2025).
func hostsExecExpectRow(result *gorm.DB) error {
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrHostsRecordNotFound
	}
	return nil
}

// ==================== plan upgrade capacity ====================

// hostsBuildPlanUpgradeCapacityResult mirrors buildPlanUpgradeCapacityResult
// (pure — every branch is unit-testable without a database).
func hostsBuildPlanUpgradeCapacityResult(
	instance HostsInstanceSpec,
	newPlan HostsPlanSpec,
	host *HostsCapacityHost,
	aggregate HostsInstanceResourceSum,
	diskAvailableMB int64,
) HostsPlanUpgradeCapacityCheck {
	deltas := HostsResourceTriple{
		CPU:    int64(newPlan.CPU) - int64(instance.CPU),
		Memory: int64(newPlan.Memory) - int64(instance.Memory),
		Disk:   int64(newPlan.Disk) - int64(instance.Disk),
	}

	zeroUsage := HostsResourceTriple{}
	emptyLimits := HostsNullableTriple{}
	emptyAvailable := HostsNullableTriple{}

	if host == nil {
		reason := HostsPlanUpgradeHostNotFound
		message := "HOST_NOT_FOUND"
		return HostsPlanUpgradeCapacityCheck{
			CanUpgrade:     false,
			Reason:         &reason,
			Message:        &message,
			HostID:         instance.HostID,
			Deltas:         deltas,
			CurrentUsage:   zeroUsage,
			ProjectedUsage: zeroUsage,
			Limits:         emptyLimits,
			Available:      emptyAvailable,
		}
	}

	currentUsage := HostsResourceTriple{
		CPU:    hostsMaxInt64(aggregate.CPUUsed, int64(hostsDerefInt32(host.CPUUsed))),
		Memory: hostsMaxInt64(aggregate.MemoryUsed, int64(hostsDerefInt32(host.MemoryUsed))),
		Disk:   hostsMaxInt64(aggregate.DiskUsed, int64(hostsDerefInt32(host.DiskUsed))),
	}
	projectedUsage := HostsResourceTriple{
		CPU:    currentUsage.CPU + deltas.CPU,
		Memory: currentUsage.Memory + deltas.Memory,
		Disk:   currentUsage.Disk + deltas.Disk,
	}
	limits := HostsNullableTriple{
		CPU:    hostsPositiveInt32(host.CPUAllowanceMax),
		Memory: hostsPositiveInt32(host.MemoryMax),
		Disk:   hostsInt64Ptr(currentUsage.Disk + diskAvailableMB),
	}
	available := HostsNullableTriple{
		CPU:    hostsNonNegativeAvailable(limits.CPU, currentUsage.CPU),
		Memory: hostsNonNegativeAvailable(limits.Memory, currentUsage.Memory),
		Disk:   hostsNonNegativeAvailable(limits.Disk, currentUsage.Disk),
	}

	if host.Status != string(models.HostStatusOnline) {
		reason := HostsPlanUpgradeHostNotOnline
		message := "HOST_NOT_ONLINE"
		return HostsPlanUpgradeCapacityCheck{
			CanUpgrade: false, Reason: &reason, Message: &message, HostID: host.ID,
			Deltas: deltas, CurrentUsage: currentUsage, ProjectedUsage: projectedUsage,
			Limits: limits, Available: available,
		}
	}

	if deltas.CPU > 0 && (limits.CPU == nil || projectedUsage.CPU > *limits.CPU) {
		reason := HostsPlanUpgradeCPUInsufficient
		message := "CPU_INSUFFICIENT"
		return HostsPlanUpgradeCapacityCheck{
			CanUpgrade: false, Reason: &reason, Message: &message, HostID: host.ID,
			Deltas: deltas, CurrentUsage: currentUsage, ProjectedUsage: projectedUsage,
			Limits: limits, Available: available,
		}
	}

	if deltas.Memory > 0 && (limits.Memory == nil || projectedUsage.Memory > *limits.Memory) {
		reason := HostsPlanUpgradeMemoryInsufficient
		message := "MEMORY_INSUFFICIENT"
		return HostsPlanUpgradeCapacityCheck{
			CanUpgrade: false, Reason: &reason, Message: &message, HostID: host.ID,
			Deltas: deltas, CurrentUsage: currentUsage, ProjectedUsage: projectedUsage,
			Limits: limits, Available: available,
		}
	}

	if deltas.Disk > 0 && (limits.Disk == nil || projectedUsage.Disk > *limits.Disk) {
		reason := HostsPlanUpgradeDiskInsufficient
		message := "DISK_INSUFFICIENT"
		return HostsPlanUpgradeCapacityCheck{
			CanUpgrade: false, Reason: &reason, Message: &message, HostID: host.ID,
			Deltas: deltas, CurrentUsage: currentUsage, ProjectedUsage: projectedUsage,
			Limits: limits, Available: available,
		}
	}

	return HostsPlanUpgradeCapacityCheck{
		CanUpgrade: true, Reason: nil, Message: nil, HostID: host.ID,
		Deltas: deltas, CurrentUsage: currentUsage, ProjectedUsage: projectedUsage,
		Limits: limits, Available: available,
	}
}

func hostsDerefInt32(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

func hostsInt64Ptr(value int64) *int64 { return &value }

// hostsPositiveInt32 mirrors `value && value > 0 ? value : null`.
func hostsPositiveInt32(value *int32) *int64 {
	if value == nil || *value <= 0 {
		return nil
	}
	converted := int64(*value)
	return &converted
}

// hostsNonNegativeAvailable mirrors `limit === null ? null : Math.max(0, limit - usage)`.
func hostsNonNegativeAvailable(limit *int64, usage int64) *int64 {
	if limit == nil {
		return nil
	}
	available := *limit - usage
	if available < 0 {
		available = 0
	}
	return &available
}

// HostsCheckPlanUpgradeCapacity mirrors checkPlanUpgradeCapacity.
func HostsCheckPlanUpgradeCapacity(
	ctx context.Context,
	instance HostsInstanceSpec,
	newPlan HostsPlanSpec,
	client *gorm.DB,
) (HostsPlanUpgradeCapacityCheck, error) {
	if err := hostsRequireDB(); err != nil {
		return HostsPlanUpgradeCapacityCheck{}, err
	}

	host, err := hostsCapacityHost(ctx, client, instance.HostID)
	if err != nil {
		return HostsPlanUpgradeCapacityCheck{}, err
	}
	aggregate, err := hostsInstanceAggregate(ctx, client, instance.HostID)
	if err != nil {
		return HostsPlanUpgradeCapacityCheck{}, err
	}

	requiredMB := int64(newPlan.Disk) - int64(instance.Disk)
	if requiredMB < 0 {
		requiredMB = 0
	}
	storage, err := hostsInstanceStorageCapacity(ctx, client, instance.ID, requiredMB)
	if err != nil {
		return HostsPlanUpgradeCapacityCheck{}, err
	}
	var diskAvailableMB int64
	if storage != nil {
		diskAvailableMB = storage.AvailableMB
	}

	return hostsBuildPlanUpgradeCapacityResult(instance, newPlan, host, aggregate, diskAvailableMB), nil
}

// HostsReservePlanUpgradeCapacityWithLock mirrors
// reservePlanUpgradeCapacityWithLock.
//
// tx MUST be a transaction: the host row is locked with `FOR UPDATE`, the
// instance aggregate and the storage capacity are read on the same transaction,
// and the projected usage is written before the lock is released. Passing nil is
// an error rather than a silent read without the lock.
func HostsReservePlanUpgradeCapacityWithLock(
	ctx context.Context,
	tx *gorm.DB,
	instance HostsInstanceSpec,
	newPlan HostsPlanSpec,
) (HostsPlanUpgradeCapacityCheck, error) {
	if err := hostsRequireDB(); err != nil {
		return HostsPlanUpgradeCapacityCheck{}, err
	}
	if tx == nil {
		return HostsPlanUpgradeCapacityCheck{}, errors.New("hosts: reservePlanUpgradeCapacityWithLock requires a transaction")
	}

	var hosts []hostsCapacityHostRow
	err := tx.WithContext(ctx).Raw(
		`SELECT id, status::text AS status, cpu_used AS "cpuUsed", memory_used AS "memoryUsed", disk_used AS "diskUsed",
		        cpu_allowance_max AS "cpuAllowanceMax", memory_max AS "memoryMax", storage_size AS "storageSize"
		   FROM hosts
		  WHERE id = ?
		    FOR UPDATE`, instance.HostID,
	).Scan(&hosts).Error
	if err != nil {
		return HostsPlanUpgradeCapacityCheck{}, err
	}

	aggregate, err := hostsInstanceAggregate(ctx, tx, instance.HostID)
	if err != nil {
		return HostsPlanUpgradeCapacityCheck{}, err
	}

	requiredMB := int64(newPlan.Disk) - int64(instance.Disk)
	if requiredMB < 0 {
		requiredMB = 0
	}
	storage, err := hostsInstanceStorageCapacity(ctx, tx, instance.ID, requiredMB)
	if err != nil {
		return HostsPlanUpgradeCapacityCheck{}, err
	}
	var diskAvailableMB int64
	if storage != nil {
		diskAvailableMB = storage.AvailableMB
	}

	var host *HostsCapacityHost
	if len(hosts) > 0 {
		converted := hosts[0].capacityHost()
		host = &converted
	}

	capacity := hostsBuildPlanUpgradeCapacityResult(instance, newPlan, host, aggregate, diskAvailableMB)
	if !capacity.CanUpgrade {
		return capacity, nil
	}

	if capacity.Deltas.CPU != 0 || capacity.Deltas.Memory != 0 || capacity.Deltas.Disk != 0 {
		projectedCPU := capacity.ProjectedUsage.CPU
		if projectedCPU < 0 {
			projectedCPU = 0
		}
		projectedMemory := capacity.ProjectedUsage.Memory
		if projectedMemory < 0 {
			projectedMemory = 0
		}
		projectedDisk := capacity.ProjectedUsage.Disk
		if projectedDisk < 0 {
			projectedDisk = 0
		}
		cpuValue, err := hostsInt32FromJS(projectedCPU, "cpu_used")
		if err != nil {
			return HostsPlanUpgradeCapacityCheck{}, err
		}
		memoryValue, err := hostsInt32FromJS(projectedMemory, "memory_used")
		if err != nil {
			return HostsPlanUpgradeCapacityCheck{}, err
		}
		diskValue, err := hostsInt32FromJS(projectedDisk, "disk_used")
		if err != nil {
			return HostsPlanUpgradeCapacityCheck{}, err
		}
		if err := hostsExecExpectRow(
			tx.WithContext(ctx).Model(&models.Host{}).Where("id = ?", instance.HostID).Updates(map[string]any{
				"cpu_used":    cpuValue,
				"memory_used": memoryValue,
				"disk_used":   diskValue,
				"updated_at":  hostsNow(),
			}),
		); err != nil {
			return HostsPlanUpgradeCapacityCheck{}, err
		}
	}

	return capacity, nil
}

// hostsCapacityHostRow is the raw projection the FOR UPDATE query returns; the
// aliases match the TypeScript's `$queryRaw` result keys.
type hostsCapacityHostRow struct {
	ID              int32  `gorm:"column:id"`
	Status          string `gorm:"column:status"`
	CPUUsed         *int32 `gorm:"column:cpuUsed"`
	MemoryUsed      *int32 `gorm:"column:memoryUsed"`
	DiskUsed        *int32 `gorm:"column:diskUsed"`
	CPUAllowanceMax *int32 `gorm:"column:cpuAllowanceMax"`
	MemoryMax       *int32 `gorm:"column:memoryMax"`
	StorageSize     *int32 `gorm:"column:storageSize"`
}

func (row hostsCapacityHostRow) capacityHost() HostsCapacityHost {
	return HostsCapacityHost{
		ID:              row.ID,
		Status:          row.Status,
		CPUUsed:         row.CPUUsed,
		MemoryUsed:      row.MemoryUsed,
		DiskUsed:        row.DiskUsed,
		CPUAllowanceMax: row.CPUAllowanceMax,
		MemoryMax:       row.MemoryMax,
		StorageSize:     row.StorageSize,
	}
}

// hostsCapacityHost reads the same projection via Prisma's findUnique select.
func hostsCapacityHost(ctx context.Context, client *gorm.DB, hostID int32) (*HostsCapacityHost, error) {
	var rows []hostsCapacityHostRow
	err := hostsClient(client).WithContext(ctx).
		Table("hosts").
		Select(`id, status::text AS status, cpu_used AS "cpuUsed", memory_used AS "memoryUsed",
		        disk_used AS "diskUsed", cpu_allowance_max AS "cpuAllowanceMax", memory_max AS "memoryMax",
		        storage_size AS "storageSize"`).
		Where("id = ?", hostID).
		Limit(1).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	host := rows[0].capacityHost()
	return &host, nil
}

// hostsInstanceAggregate mirrors `instance.aggregate({_sum:{cpu,memory,disk}})`
// with `_sum.x ?? 0` folded into COALESCE.
func hostsInstanceAggregate(ctx context.Context, client *gorm.DB, hostID int32) (HostsInstanceResourceSum, error) {
	var row struct {
		CPUUsed    int64 `gorm:"column:cpu_used"`
		MemoryUsed int64 `gorm:"column:memory_used"`
		DiskUsed   int64 `gorm:"column:disk_used"`
	}
	err := hostsClient(client).WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(cpu), 0) AS cpu_used,
		        COALESCE(SUM(memory), 0) AS memory_used,
		        COALESCE(SUM(disk), 0) AS disk_used
		   FROM instances
		  WHERE host_id = ? AND status <> ?`, hostID, string(hostsDeletedInstanceStatus),
	).Scan(&row).Error
	if err != nil {
		return HostsInstanceResourceSum{}, err
	}
	return HostsInstanceResourceSum{CPUUsed: row.CPUUsed, MemoryUsed: row.MemoryUsed, DiskUsed: row.DiskUsed}, nil
}

// ==================== host selection ====================

// HostsSelectAvailable mirrors selectAvailableHost.
//
// NAT port checks are intentionally absent (the Node comment says the same): the
// port mapping table records allocations, the host counter is not a gate here.
func HostsSelectAvailable(ctx context.Context, options HostsSelectOptions) (*HostsSelectionRecord, error) {
	if err := hostsRequireDB(); err != nil {
		return nil, err
	}

	query := DB.WithContext(ctx).Model(&models.Host{}).Where("status = ?", string(models.HostStatusOnline))

	if options.HostID != nil && *options.HostID != 0 {
		if len(options.PackageHostIDs) > 0 && !hostsContainsInt32(options.PackageHostIDs, *options.HostID) {
			log.Printf("[selectAvailableHost] 安全拒绝: hostId=%d 不在套餐绑定的宿主机列表 %v 中", *options.HostID, options.PackageHostIDs)
			return nil, nil
		}
		query = query.Where("id = ?", *options.HostID)
	} else if len(options.PackageHostIDs) > 0 {
		query = query.Where("id IN ?", options.PackageHostIDs)
	} else if options.OwnerID != nil && *options.OwnerID != 0 {
		query = query.Where("user_id = ?", *options.OwnerID)
	}

	var hosts []models.Host
	if err := query.Order("memory_used ASC").Order("cpu_used ASC").Find(&hosts).Error; err != nil {
		return nil, err
	}

	hostIDs := make([]int32, 0, len(hosts))
	for _, host := range hosts {
		hostIDs = append(hostIDs, host.ID)
	}
	sums, err := hostsInstanceSumsByHost(ctx, DB, hostIDs)
	if err != nil {
		return nil, err
	}

	for _, host := range hosts {
		if len(options.NodeSelectors) > 0 && !hostsHasAllTags(hostsTagsOf(host.Tags), options.NodeSelectors) {
			continue
		}

		sum := sums[host.ID]
		cpuUsedEffective := hostsMaxInt64(sum.CPUUsed, int64(host.CPUUsed))
		memoryUsedEffective := hostsMaxInt64(sum.MemoryUsed, int64(host.MemoryUsed))
		diskUsedEffective := hostsMaxInt64(sum.DiskUsed, int64(host.DiskUsed))

		// A host without a quota cap is unusable (a quota is mandatory).
		cpuQuotaMax := host.CPUAllowanceMax
		if !hostsCPUQuotaAllows(&cpuQuotaMax, cpuUsedEffective, options.CPU) {
			continue
		}
		memoryQuotaMax := host.MemoryMax
		if !hostsMemoryQuotaAllows(&memoryQuotaMax, memoryUsedEffective, options.Memory) {
			continue
		}

		storage, err := hostsStorageCapacity(ctx, DB, host.ID, HostsStorageCapacityOptions{
			PackageID:  options.PackageID,
			RequiredMB: &options.Disk,
		})
		if err != nil {
			return nil, err
		}
		if storage.Reason != nil {
			continue
		}

		if hostsNetworkModeNeedsRoutedIpv6(options.NetworkMode) && !hostsRoutedIpv6Configured(host.IPv6Subnet, host.IPv6ParentInterface) {
			continue
		}

		if hostsNetworkModeNeedsPublicIpv4(options.NetworkMode) {
			freeCount, err := hostsCountFreePublicIpv4(ctx, DB, host.ID)
			if err != nil {
				return nil, err
			}
			if !hostsPublicIpv4Allows(freeCount) {
				continue
			}
		}

		diskTotal := storage.TotalMB
		cpuAllowanceMax := host.CPUAllowanceMax
		memoryMax := host.MemoryMax
		instanceType := string(host.InstanceType)
		tunnelEnabled := host.TunnelEnabled
		targetHost := host.TargetHost
		targetPort := host.TargetPort
		return &HostsSelectionRecord{
			ID:                  host.ID,
			Name:                host.Name,
			URL:                 host.URL,
			Location:            host.Location,
			CountryCode:         host.CountryCode,
			Status:              string(host.Status),
			CertPath:            host.CertPath,
			KeyPath:             host.KeyPath,
			NATPublicIP:         host.NATPublicIP,
			NATPortStart:        host.NATPortStart,
			NATPortEnd:          host.NATPortEnd,
			CPUUsed:             cpuUsedEffective,
			MemoryUsed:          memoryUsedEffective,
			DiskTotal:           &diskTotal,
			StoragePoolName:     storage.PoolName,
			DiskUsed:            diskUsedEffective,
			CPUAllowanceMax:     &cpuAllowanceMax,
			MemoryMax:           &memoryMax,
			IPv6Mode:            host.IPv6Mode,
			IPv6Subnet:          host.IPv6Subnet,
			IPv6Gateway:         host.IPv6Gateway,
			IPv6ParentInterface: host.IPv6ParentInterface,
			InstanceType:        instanceType,
			TunnelEnabled:       &tunnelEnabled,
			TargetHost:          &targetHost,
			TargetPort:          &targetPort,
			CreatedAt:           hostsISOTime(host.CreatedAt),
			UpdatedAt:           hostsISOTime(host.UpdatedAt),
		}, nil
	}

	return nil, nil
}

func hostsContainsInt32(values []int32, needle int32) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// hostsInstanceSumsByHost mirrors the `include: { instances: { select: {cpu,
// memory, disk} } }` relation: one grouped aggregate returns the same sums.
func hostsInstanceSumsByHost(ctx context.Context, client *gorm.DB, hostIDs []int32) (map[int32]HostsInstanceResourceSum, error) {
	sums := make(map[int32]HostsInstanceResourceSum, len(hostIDs))
	if len(hostIDs) == 0 {
		return sums, nil
	}

	var rows []struct {
		HostID     int32 `gorm:"column:host_id"`
		CPUUsed    int64 `gorm:"column:cpu_used"`
		MemoryUsed int64 `gorm:"column:memory_used"`
		DiskUsed   int64 `gorm:"column:disk_used"`
	}
	err := hostsClient(client).WithContext(ctx).Raw(
		`SELECT host_id,
		        COALESCE(SUM(cpu), 0) AS cpu_used,
		        COALESCE(SUM(memory), 0) AS memory_used,
		        COALESCE(SUM(disk), 0) AS disk_used
		   FROM instances
		  WHERE host_id IN ? AND status <> ?
		  GROUP BY host_id`, hostIDs, string(hostsDeletedInstanceStatus),
	).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		sums[row.HostID] = HostsInstanceResourceSum{CPUUsed: row.CPUUsed, MemoryUsed: row.MemoryUsed, DiskUsed: row.DiskUsed}
	}
	return sums, nil
}

// hostsLockQueryColumns is the exact SELECT list of selectAndReserveHostWithLock.
const hostsLockQueryColumns = `SELECT id, name, url, location, country_code, status, cert_path, key_path,
           nat_public_ip, nat_port_start, nat_port_end, 
           cpu_used, memory_used, disk_used,
           cpu_allowance_max, memory_max, storage_size,
           ipv6_mode, ipv6_subnet, ipv6_gateway, ipv6_parent_interface,
           instance_type, tags, nat_ports_used_count,
           created_at, updated_at
    FROM hosts
    WHERE `

// hostsBuildLockQuery mirrors the `lockQuery` construction:
//
//	WHERE ${whereConditions.join(' AND ')}
//	ORDER BY memory_used ASC, cpu_used ASC
//	FOR UPDATE
//
// It is a pure function so the SQL shape (plain FOR UPDATE row lock, no
// advisory lock, no SKIP LOCKED) can be asserted in tests.
func hostsBuildLockQuery(hostID *int32, packageHostIDs []int32, ownerID *int32) (string, []any) {
	conditions := []string{"status = 'online'"}
	params := make([]any, 0, 1)

	switch {
	case hostID != nil && *hostID != 0:
		conditions = append(conditions, fmt.Sprintf("id = $%d", len(params)+1))
		params = append(params, *hostID)
	case len(packageHostIDs) > 0:
		conditions = append(conditions, fmt.Sprintf("id = ANY($%d::int[])", len(params)+1))
		params = append(params, packageHostIDs)
	case ownerID != nil && *ownerID != 0:
		conditions = append(conditions, fmt.Sprintf("user_id = $%d", len(params)+1))
		params = append(params, *ownerID)
	}

	statement := hostsLockQueryColumns + strings.Join(conditions, " AND ") + `
    ORDER BY memory_used ASC, cpu_used ASC
    FOR UPDATE`
	return statement, params
}

// hostsLockRow mirrors the row type of the lock query.
type hostsLockRow struct {
	ID                  int32          `gorm:"column:id"`
	Name                string         `gorm:"column:name"`
	URL                 string         `gorm:"column:url"`
	Location            *string        `gorm:"column:location"`
	CountryCode         string         `gorm:"column:country_code"`
	Status              string         `gorm:"column:status"`
	CertPath            *string        `gorm:"column:cert_path"`
	KeyPath             *string        `gorm:"column:key_path"`
	NATPublicIP         *string        `gorm:"column:nat_public_ip"`
	NATPortStart        *int32         `gorm:"column:nat_port_start"`
	NATPortEnd          *int32         `gorm:"column:nat_port_end"`
	CPUUsed             int32          `gorm:"column:cpu_used"`
	MemoryUsed          int32          `gorm:"column:memory_used"`
	DiskUsed            int32          `gorm:"column:disk_used"`
	CPUAllowanceMax     *int32         `gorm:"column:cpu_allowance_max"`
	MemoryMax           *int32         `gorm:"column:memory_max"`
	StorageSize         *int32         `gorm:"column:storage_size"`
	IPv6Mode            int32          `gorm:"column:ipv6_mode"`
	IPv6Subnet          *string        `gorm:"column:ipv6_subnet"`
	IPv6Gateway         *string        `gorm:"column:ipv6_gateway"`
	IPv6ParentInterface *string        `gorm:"column:ipv6_parent_interface"`
	InstanceType        string         `gorm:"column:instance_type"`
	Tags                datatypes.JSON `gorm:"column:tags"`
	NATPortsUsedCount   int32          `gorm:"column:nat_ports_used_count"`
	CreatedAt           time.Time      `gorm:"column:created_at"`
	UpdatedAt           time.Time      `gorm:"column:updated_at"`
}

// HostsSelectAndReserveWithLock mirrors selectAndReserveHostWithLock.
//
// tx MUST be a transaction. The row lock (`FOR UPDATE`), the per-host resource
// aggregate, the storage/IPv4 checks and the reservation UPDATE all run on the
// same transaction, in that order — changing any of the four changes the
// concurrency semantics of instance creation.
func HostsSelectAndReserveWithLock(
	ctx context.Context,
	tx *gorm.DB,
	options HostsReserveOptions,
) (*HostsSelectionRecord, error) {
	if err := hostsRequireDB(); err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, errors.New("hosts: selectAndReserveHostWithLock requires a transaction")
	}

	gpuPolicy, err := GpusGetPolicy(ctx, tx, options.PackageID, options.PackagePlanID)
	if err != nil {
		return nil, err
	}

	if options.HostID != nil && *options.HostID != 0 && len(options.PackageHostIDs) > 0 && !hostsContainsInt32(options.PackageHostIDs, *options.HostID) {
		log.Printf("[selectAndReserveHostWithLock] 安全拒绝: hostId=%d 不在套餐绑定列表中", *options.HostID)
		return nil, nil
	}

	lockQuery, params := hostsBuildLockQuery(options.HostID, options.PackageHostIDs, options.OwnerID)

	var hosts []hostsLockRow
	if err := tx.WithContext(ctx).Raw(lockQuery, params...).Scan(&hosts).Error; err != nil {
		return nil, err
	}

	for _, host := range hosts {
		if gpuPolicy.GpuShared {
			available, err := GpusFindAvailable(ctx, tx, host.ID, gpuPolicy, nil)
			if err != nil {
				return nil, err
			}
			if len(available) == 0 {
				continue
			}
		}

		if len(options.NodeSelectors) > 0 && !hostsHasAllTags(hostsTagsOf(host.Tags), options.NodeSelectors) {
			continue
		}

		sum, err := hostsInstanceAggregate(ctx, tx, host.ID)
		if err != nil {
			return nil, err
		}

		cpuUsedEffective := hostsMaxInt64(sum.CPUUsed, int64(host.CPUUsed))
		memoryUsedEffective := hostsMaxInt64(sum.MemoryUsed, int64(host.MemoryUsed))
		diskUsedEffective := hostsMaxInt64(sum.DiskUsed, int64(host.DiskUsed))

		if !hostsCPUQuotaAllows(host.CPUAllowanceMax, cpuUsedEffective, options.CPU) {
			continue
		}
		if !hostsMemoryQuotaAllows(host.MemoryMax, memoryUsedEffective, options.Memory) {
			continue
		}

		storage, err := hostsStorageCapacity(ctx, tx, host.ID, HostsStorageCapacityOptions{
			PackageID:  options.PackageID,
			RequiredMB: &options.Disk,
		})
		if err != nil {
			return nil, err
		}
		if storage.Reason != nil {
			continue
		}

		if hostsNetworkModeNeedsRoutedIpv6(options.NetworkMode) && !hostsRoutedIpv6Configured(host.IPv6Subnet, host.IPv6ParentInterface) {
			continue
		}

		if hostsNetworkModeNeedsPublicIpv4(options.NetworkMode) {
			freeCount, err := hostsCountFreePublicIpv4(ctx, tx, host.ID)
			if err != nil {
				return nil, err
			}
			if !hostsPublicIpv4Allows(freeCount) {
				continue
			}
		}

		if !hostsNATPortsAllow(host.NATPortStart, host.NATPortEnd, host.NATPortsUsedCount, options.PortCount) {
			continue
		}

		projectedCPU, err := hostsInt32FromJS(cpuUsedEffective+options.CPU, "cpu_used")
		if err != nil {
			return nil, err
		}
		projectedMemory, err := hostsInt32FromJS(memoryUsedEffective+options.Memory, "memory_used")
		if err != nil {
			return nil, err
		}
		projectedDisk, err := hostsInt32FromJS(diskUsedEffective+options.Disk, "disk_used")
		if err != nil {
			return nil, err
		}

		values := map[string]any{
			"cpu_used":    projectedCPU,
			"memory_used": projectedMemory,
			"disk_used":   projectedDisk,
			"updated_at":  hostsNow(),
		}
		if options.PortCount > 0 {
			ports, err := hostsInt32FromJS(int64(host.NATPortsUsedCount)+int64(options.PortCount), "nat_ports_used_count")
			if err != nil {
				return nil, err
			}
			values["nat_ports_used_count"] = ports
		}
		if err := hostsExecExpectRow(
			tx.WithContext(ctx).Model(&models.Host{}).Where("id = ?", host.ID).Updates(values),
		); err != nil {
			return nil, err
		}

		return &HostsSelectionRecord{
			ID:                  host.ID,
			Name:                host.Name,
			URL:                 host.URL,
			Location:            host.Location,
			CountryCode:         host.CountryCode,
			Status:              host.Status,
			CertPath:            host.CertPath,
			KeyPath:             host.KeyPath,
			NATPublicIP:         host.NATPublicIP,
			NATPortStart:        host.NATPortStart,
			NATPortEnd:          host.NATPortEnd,
			CPUUsed:             cpuUsedEffective + options.CPU,
			MemoryUsed:          memoryUsedEffective + options.Memory,
			DiskUsed:            diskUsedEffective + options.Disk,
			StoragePoolName:     storage.PoolName,
			CPUAllowanceMax:     host.CPUAllowanceMax,
			MemoryMax:           host.MemoryMax,
			IPv6Mode:            host.IPv6Mode,
			IPv6Subnet:          host.IPv6Subnet,
			IPv6Gateway:         host.IPv6Gateway,
			IPv6ParentInterface: host.IPv6ParentInterface,
			InstanceType:        host.InstanceType,
			CreatedAt:           hostsISOTime(host.CreatedAt),
			UpdatedAt:           hostsISOTime(host.UpdatedAt),
		}, nil
	}

	return nil, nil
}
