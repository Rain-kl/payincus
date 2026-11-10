// Port of server/src/lib/storage-capacity.ts (plus the two policy helpers it
// imports from storage-capacity-policy.ts and the availability half of
// host-service-status.ts).
//
// Storage capacity is a *resource promise*: `availableMB` is what the scheduler
// will sell, so every subtraction below is a deliberate reservation.
//
//   - totalMB floors, usedMB ceils, so a partially used GiB is never sold twice;
//   - `headroomMB` keeps 1–10 GiB free (1% of the pool, clamped) so a pool never
//     reaches 100%;
//   - instances whose root pool is unknown are charged to *every* candidate pool
//     ("unmapped instances reserve space conservatively until their root pool is
//     reported"), and so are all instances of the same filesystem/zfs/lvm group;
//   - sparse vdevs share a backing filesystem, so its remaining bytes are
//     promised once (minus a 1 GiB safety margin);
//   - a pool whose telemetry is stale or flagged `_payincusImport` /
//     `_payincusDeletion` reports `storage_unavailable` instead of a number.
//
// Numeric fidelity: the TypeScript computes with JS doubles
// (`Math.floor(Number(totalBytes) / MIB)`), so this file keeps the same float64
// arithmetic and only converts to int64 at the boundary. Integer division would
// differ for negative values and for byte counts above 2^53.
//
// The db package must not import lib, so db/packages.go reads this function
// through db.RegisterPackageStorageCapacityResolver (registered in init below).
package lib

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// StorageCapacityMIB mirrors MIB (1024 * 1024).
const StorageCapacityMIB = 1024 * 1024

// StorageUnavailableReason mirrors the StorageUnavailableReason union.
type StorageUnavailableReason string

// The three reasons mirror the TypeScript union members.
const (
	StorageUnavailable StorageUnavailableReason = "storage_unavailable"
	StoragePoolMissing StorageUnavailableReason = "storage_pool_missing"
	StorageDiskFull    StorageUnavailableReason = "disk_full"
)

// StorageCapacity mirrors the StorageCapacity interface. Field names are the
// camelCase wire shape the routes return unchanged.
type StorageCapacity struct {
	PoolName    *string                   `json:"poolName"`
	TotalMB     int64                     `json:"totalMB"`
	UsedMB      int64                     `json:"usedMB"`
	AllocatedMB int64                     `json:"allocatedMB"`
	AvailableMB int64                     `json:"availableMB"`
	HeadroomMB  int64                     `json:"headroomMB"`
	Reason      *StorageUnavailableReason `json:"reason"`
}

// StorageCapacityQuerier is satisfied by *pgxpool.Pool and pgx.Tx, mirroring the
// TypeScript `client: Prisma.TransactionClient | typeof prisma`.
type StorageCapacityQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// StorageCapacityOptions mirrors the options object of getStorageCapacity.
// RequiredMB is `requiredMB?: number`; every caller passes an integer disk size.
type StorageCapacityOptions struct {
	Client     StorageCapacityQuerier
	PackageID  *int32
	PoolName   *string
	RequiredMB *int64
}

// StorageCapacityTunnelIsOnline is the seam for
// `hostTunnelManager.isTunnelOnline(hostId)`. It defaults to the real tunnel
// manager (lib/tunnel_manager.go, same package) and stays overridable so the
// availability predicate can be tested without a live tunnel.
//
// A tunnel-enabled host with no live tunnel is effectively offline, so its pools
// report `storage_unavailable` rather than promising capacity nobody verified.
var StorageCapacityTunnelIsOnline = func(hostID int32) bool {
	return TunnelManagerInstance().IsTunnelOnline(hostID)
}

// StorageOverviewPool mirrors one element of getStorageOverview().pools.
type StorageOverviewPool struct {
	PoolName   string  `json:"poolName"`
	TotalMB    int64   `json:"totalMB"`
	UsedMB     int64   `json:"usedMB"`
	Status     string  `json:"status"`
	ReportedAt *string `json:"reportedAt"`
}

// StorageOverview mirrors getStorageOverview's return shape.
type StorageOverview struct {
	TotalMB     int64                 `json:"totalMB"`
	UsedMB      int64                 `json:"usedMB"`
	AllocatedMB int64                 `json:"allocatedMB"`
	Pools       []StorageOverviewPool `json:"pools"`
}

type storageCapacityHostRow struct {
	Status        string
	TunnelEnabled bool
	DiskUsed      int32
	NodeReport    []byte
	AgentStatus   *string
	AgentSeenAt   *time.Time
	AgentReport   []byte
}

type storageCapacityPoolRow struct {
	ID         int32
	Name       string
	Driver     string
	Config     []byte
	Status     string
	TotalBytes int64
	UsedBytes  int64
	Telemetry  []byte
	ReportedAt *time.Time
}

type storageCapacityInstanceRow struct {
	IncusID         string
	StoragePoolName *string
	Disk            int32
}

// storageCapacityEmpty mirrors the `empty(reason)` helper.
func storageCapacityEmpty(reason StorageUnavailableReason) StorageCapacity {
	value := reason
	return StorageCapacity{Reason: &value}
}

// storageCapacityClient resolves the client the same way the TypeScript
// `options.client || prisma` does.
func storageCapacityClient(options StorageCapacityOptions) (StorageCapacityQuerier, error) {
	if options.Client != nil {
		return options.Client, nil
	}
	pool := db.Pool()
	if pool == nil {
		return nil, errors.New("storage capacity: database not initialized")
	}
	return pool, nil
}

// init installs the capacity reader into db/packages.go's sold-out checks. db must
// not import lib, so the dependency is inverted through the Register* hook (the
// same pattern as db.RegisterSystemConfigCache).
func init() {
	db.RegisterPackageStorageCapacityResolver(func(ctx context.Context, hostID int32, packageID *int32, requiredMB int64) (db.PackageStorageCapacity, error) {
		capacity, err := GetStorageCapacity(ctx, hostID, StorageCapacityOptions{
			PackageID:  packageID,
			RequiredMB: &requiredMB,
		})
		if err != nil {
			return db.PackageStorageCapacity{}, err
		}
		var reason *string
		if capacity.Reason != nil {
			value := string(*capacity.Reason)
			reason = &value
		}
		return db.PackageStorageCapacity{
			PoolName:    capacity.PoolName,
			TotalMB:     capacity.TotalMB,
			UsedMB:      capacity.UsedMB,
			AllocatedMB: capacity.AllocatedMB,
			AvailableMB: capacity.AvailableMB,
			HeadroomMB:  capacity.HeadroomMB,
			Reason:      reason,
		}, nil
	})
}

// GetStorageCapacity mirrors getStorageCapacity(hostId, options).
func GetStorageCapacity(ctx context.Context, hostID int32, options StorageCapacityOptions) (StorageCapacity, error) {
	client, err := storageCapacityClient(options)
	if err != nil {
		return StorageCapacity{}, err
	}

	// Node availability is checked before pool health and capacity. No node I/O
	// is permitted here.
	var host storageCapacityHostRow
	err = client.QueryRow(ctx,
		`SELECT h.status::text, h.tunnel_enabled, h.disk_used,
		        ns.report AS node_report,
		        a.status AS agent_status, a.last_seen_at AS agent_seen_at, a.last_report AS agent_report
		   FROM hosts h
		   LEFT JOIN node_security_states ns ON ns.host_id = h.id
		   LEFT JOIN host_agents a ON a.host_id = h.id
		  WHERE h.id = $1`, hostID,
	).Scan(&host.Status, &host.TunnelEnabled, &host.DiskUsed, &host.NodeReport,
		&host.AgentStatus, &host.AgentSeenAt, &host.AgentReport)
	if errors.Is(err, pgx.ErrNoRows) {
		return storageCapacityEmpty(StorageUnavailable), nil
	}
	if err != nil {
		return StorageCapacity{}, err
	}

	nowMS := time.Now().UTC().UnixMilli()
	if host.Status != "online" || !storageCapacityHostServiceAvailable(hostID, host, nowMS) {
		return storageCapacityEmpty(StorageUnavailable), nil
	}

	pools, err := storageCapacityLoadPools(ctx, client, hostID)
	if err != nil {
		return StorageCapacity{}, err
	}
	instances, err := storageCapacityLoadInstances(ctx, client, hostID)
	if err != nil {
		return StorageCapacity{}, err
	}

	// `options.packageId ? findUnique(...) : null` — 0 is falsy in JS too.
	var binding *string
	if options.PackageID != nil && *options.PackageID != 0 {
		var poolName *string
		err := client.QueryRow(ctx,
			`SELECT storage_pool_name FROM package_hosts WHERE package_id = $1 AND host_id = $2`,
			*options.PackageID, hostID,
		).Scan(&poolName)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return storageCapacityEmpty(StoragePoolMissing), nil
		case err != nil:
			return StorageCapacity{}, err
		}
		binding = poolName
	}

	// `options.poolName || binding?.storagePoolName` — an empty string falls back.
	var selected *string
	if options.PoolName != nil && *options.PoolName != "" {
		selected = options.PoolName
	} else if binding != nil && *binding != "" {
		selected = binding
	}

	candidates := make([]storageCapacityPoolRow, 0, len(pools))
	for _, pool := range pools {
		if selected == nil || pool.Name == *selected {
			candidates = append(candidates, pool)
		}
	}
	if len(candidates) == 0 {
		return storageCapacityEmpty(StoragePoolMissing), nil
	}

	maxAgeMS := StorageReportMaxAgeMS(host.AgentReport)
	requiredMB := int64(1)
	if options.RequiredMB != nil {
		requiredMB = *options.RequiredMB
	}

	choices := make([]StorageCapacity, 0, len(candidates))
	for _, pool := range candidates {
		choices = append(choices, storageCapacityChoice(pool, pools, instances, host.DiskUsed, nowMS, maxAgeMS, requiredMB))
	}

	// choices.sort((a, b) => Number(!!a.reason) - Number(!!b.reason)
	//   || b.availableMB - a.availableMB || a.poolName.localeCompare(b.poolName))
	sort.SliceStable(choices, func(i, j int) bool {
		left, right := choices[i], choices[j]
		leftHasReason, rightHasReason := left.Reason != nil, right.Reason != nil
		if leftHasReason != rightHasReason {
			return !leftHasReason
		}
		if left.AvailableMB != right.AvailableMB {
			return left.AvailableMB > right.AvailableMB
		}
		return storageCapacityLocaleCompare(storageCapacityString(left.PoolName), storageCapacityString(right.PoolName)) < 0
	})
	return choices[0], nil
}

// storageCapacityChoice mirrors the body of the `candidates.map(pool => ...)`
// callback in getStorageCapacity. It is a pure function of the pool, its
// siblings, the host's instances and the clock, so the unit conversion and
// reservation arithmetic can be tested without a database.
func storageCapacityChoice(
	pool storageCapacityPoolRow,
	pools []storageCapacityPoolRow,
	instances []storageCapacityInstanceRow,
	hostDiskUsed int32,
	nowMS int64,
	maxAgeMS float64,
	requiredMB int64,
) StorageCapacity {
	totalMB := int64(math.Floor(float64(pool.TotalBytes) / StorageCapacityMIB))
	usedMB := int64(math.Ceil(float64(pool.UsedBytes) / StorageCapacityMIB))

	group := storageCapacityGroup(pool)
	siblings := make([]storageCapacityPoolRow, 0, len(pools))
	members := make(map[string]bool, len(pools))
	for _, candidate := range pools {
		if storageCapacityGroup(candidate) == group {
			siblings = append(siblings, candidate)
			members[candidate.Name] = true
		}
	}

	// Unmapped instances reserve space conservatively until their root pool is
	// reported.
	var allocatedMB, groupAllocated int64
	for _, instance := range instances {
		if instance.StoragePoolName == nil || *instance.StoragePoolName == pool.Name {
			allocatedMB += int64(instance.Disk)
		}
		if instance.StoragePoolName == nil || members[*instance.StoragePoolName] {
			groupAllocated += int64(instance.Disk)
		}
	}

	headroomMB := StorageHeadroomMB(totalMB)
	groupTotal := int64(0)
	for _, sibling := range siblings {
		if value := int64(math.Floor(float64(sibling.TotalBytes) / StorageCapacityMIB)); value > groupTotal {
			groupTotal = value
		}
	}

	backing := storageCapacityBackingOf(pool.Telemetry)
	var backingReserved int64
	if backing != nil {
		backingNames := make(map[string]bool, len(pools))
		for _, candidate := range pools {
			candidateBacking := storageCapacityBackingOf(candidate.Telemetry)
			if candidateBacking != nil && candidateBacking.ID == backing.ID {
				backingNames[candidate.Name] = true
			}
		}
		for _, instance := range instances {
			if instance.StoragePoolName == nil || backingNames[*instance.StoragePoolName] {
				backingReserved += int64(instance.Disk)
			}
		}
	}

	// `unassigned = Math.max(0, host.diskUsed - sum(instance.disk))`
	var allocatedByDisk int64
	for _, instance := range instances {
		allocatedByDisk += int64(instance.Disk)
	}
	unassigned := int64(hostDiskUsed) - allocatedByDisk
	if unassigned < 0 {
		unassigned = 0
	}

	availableMB := int64(0)
	if storageCapacityPoolReady(pool, nowMS, maxAgeMS) {
		limits := []float64{
			float64(totalMB - headroomMB - allocatedMB - unassigned),
			float64(totalMB - usedMB - headroomMB),
			float64(groupTotal - StorageHeadroomMB(groupTotal) - groupAllocated - unassigned),
		}
		if backing != nil {
			// Sparse vdevs share their backing filesystem: never promise its
			// remaining bytes twice.
			limits = append(limits, math.Floor(float64(backing.AvailableBytes)/StorageCapacityMIB)-1024-float64(backingReserved)-float64(unassigned))
		}
		minimum := limits[0]
		for _, candidate := range limits[1:] {
			if candidate < minimum {
				minimum = candidate
			}
		}
		if minimum < 0 {
			minimum = 0
		}
		availableMB = int64(math.Floor(minimum))
	}

	choice := StorageCapacity{
		PoolName:    &pool.Name,
		TotalMB:     totalMB,
		UsedMB:      usedMB,
		AllocatedMB: allocatedMB,
		AvailableMB: availableMB,
		HeadroomMB:  headroomMB,
	}
	switch {
	case !storageCapacityPoolReady(pool, nowMS, maxAgeMS):
		reason := StorageUnavailable
		choice.Reason = &reason
	case availableMB < requiredMB:
		reason := StorageDiskFull
		choice.Reason = &reason
	}
	return choice
}

// GetStorageOverview mirrors getStorageOverview(hostId).
//
// Last known totals survive outages/locks; allocation gates independently reject
// unavailable telemetry, so this deliberately does not filter by status.
func GetStorageOverview(ctx context.Context, hostID int32) (StorageOverview, error) {
	pool := db.Pool()
	if pool == nil {
		return StorageOverview{}, errors.New("storage capacity: database not initialized")
	}

	pools, err := storageCapacityLoadPools(ctx, pool, hostID)
	if err != nil {
		return StorageOverview{}, err
	}
	instances, err := storageCapacityLoadInstances(ctx, pool, hostID)
	if err != nil {
		return StorageOverview{}, err
	}

	overview := StorageOverview{Pools: make([]StorageOverviewPool, 0, len(pools))}
	for _, item := range pools {
		totalMB := int64(math.Floor(float64(item.TotalBytes) / StorageCapacityMIB))
		usedMB := int64(math.Ceil(float64(item.UsedBytes) / StorageCapacityMIB))
		overview.TotalMB += totalMB
		overview.UsedMB += usedMB
		entry := StorageOverviewPool{
			PoolName: item.Name,
			TotalMB:  totalMB,
			UsedMB:   usedMB,
			Status:   item.Status,
		}
		if item.ReportedAt != nil {
			formatted := item.ReportedAt.UTC().Format("2006-01-02T15:04:05.000Z")
			entry.ReportedAt = &formatted
		}
		overview.Pools = append(overview.Pools, entry)
	}
	for _, instance := range instances {
		overview.AllocatedMB += int64(instance.Disk)
	}
	return overview, nil
}

// GetInstanceStorageCapacity mirrors getInstanceStorageCapacity(instanceId,
// requiredMB, client = prisma). An instance without a reported root pool cannot
// be capacity-checked; the TypeScript returns null and the caller rejects.
func GetInstanceStorageCapacity(ctx context.Context, instanceID int32, requiredMB int64, client StorageCapacityQuerier) (*StorageCapacity, error) {
	if client == nil {
		client = db.Pool()
		if client == nil {
			return nil, errors.New("storage capacity: database not initialized")
		}
	}

	var hostID int32
	var storagePoolName *string
	err := client.QueryRow(ctx,
		`SELECT host_id, storage_pool_name FROM instances WHERE id = $1`, instanceID).
		Scan(&hostID, &storagePoolName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// `if (!instance?.storagePoolName) return null` — an empty string is falsy.
	if storagePoolName == nil || *storagePoolName == "" {
		return nil, nil
	}

	capacity, err := GetStorageCapacity(ctx, hostID, StorageCapacityOptions{
		Client:     client,
		PoolName:   storagePoolName,
		RequiredMB: &requiredMB,
	})
	if err != nil {
		return nil, err
	}
	return &capacity, nil
}

// StorageHeadroomMB mirrors storageHeadroomMB: keep 1% of the pool free, clamped
// to [1024, 10240] MB.
func StorageHeadroomMB(totalMB int64) int64 {
	return int64(math.Min(10240, math.Max(1024, math.Ceil(float64(totalMB)/100))))
}

// StorageReportMaxAgeMS mirrors storageReportMaxAgeMs: the pool report is stale
// after max(120s, heartbeat * 3), where the heartbeat is clamped to [5, 3600]s.
func StorageReportMaxAgeMS(report []byte) float64 {
	interval := 5.0
	if len(report) > 0 {
		var parsed struct {
			Metrics *struct {
				HeartbeatIntervalSeconds any `json:"heartbeatIntervalSeconds"`
			} `json:"metrics"`
		}
		if err := json.Unmarshal(report, &parsed); err == nil && parsed.Metrics != nil {
			value := storageCapacityJSNumber(parsed.Metrics.HeartbeatIntervalSeconds)
			if !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 5 && value <= 3600 {
				interval = value
			}
		}
	}
	return math.Max(120000, interval*3000)
}

// storageCapacityLoadPools mirrors
// `storagePool.findMany({ where: { hostId, purpose: 'instance_data' }, orderBy: { name: 'asc' } })`.
func storageCapacityLoadPools(ctx context.Context, client StorageCapacityQuerier, hostID int32) ([]storageCapacityPoolRow, error) {
	rows, err := client.Query(ctx,
		`SELECT id, name, driver, config, status, total_bytes, used_bytes, telemetry, reported_at
		   FROM storage_pools
		  WHERE host_id = $1 AND purpose = 'instance_data'
		  ORDER BY name ASC`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pools := make([]storageCapacityPoolRow, 0, 8)
	for rows.Next() {
		var pool storageCapacityPoolRow
		if err := rows.Scan(&pool.ID, &pool.Name, &pool.Driver, &pool.Config, &pool.Status,
			&pool.TotalBytes, &pool.UsedBytes, &pool.Telemetry, &pool.ReportedAt); err != nil {
			return nil, err
		}
		pools = append(pools, pool)
	}
	return pools, rows.Err()
}

// storageCapacityLoadInstances mirrors
// `instance.findMany({ where: { hostId, status: { not: 'deleted' } }, select: { incusId, storagePoolName, disk } })`.
func storageCapacityLoadInstances(ctx context.Context, client StorageCapacityQuerier, hostID int32) ([]storageCapacityInstanceRow, error) {
	rows, err := client.Query(ctx,
		`SELECT incus_id, storage_pool_name, disk
		   FROM instances
		  WHERE host_id = $1 AND status <> 'deleted'
		  ORDER BY id ASC`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	instances := make([]storageCapacityInstanceRow, 0, 16)
	for rows.Next() {
		var instance storageCapacityInstanceRow
		if err := rows.Scan(&instance.IncusID, &instance.StoragePoolName, &instance.Disk); err != nil {
			return nil, err
		}
		instances = append(instances, instance)
	}
	return instances, rows.Err()
}

// storageCapacityGroup mirrors group(pool): pools that share a physical
// filesystem/vdev/volume group must be accounted together.
func storageCapacityGroup(pool storageCapacityPoolRow) string {
	config := storageCapacityJSONObject(pool.Config)
	configString := func(key string) string {
		value, _ := config[key].(string)
		return value
	}

	switch pool.Driver {
	case "dir", "btrfs":
		filesystemID, _ := storageCapacityJSONObject(pool.Telemetry)["filesystemId"].(string)
		if filesystemID == "" {
			filesystemID = "unknown"
		}
		return "filesystem:" + filesystemID
	case "zfs":
		// `config['zfs.pool_name']?.split('/')[0] || config.source?.split('/')[0] || pool.name`
		// The `||` applies to the split *result*, so an absolute path ("/dev/sda"
		// → "") falls through to the next candidate.
		if segment := strings.Split(configString("zfs.pool_name"), "/")[0]; segment != "" {
			return "zfs:" + segment
		}
		if segment := strings.Split(configString("source"), "/")[0]; segment != "" {
			return "zfs:" + segment
		}
		return "zfs:" + pool.Name
	case "lvm":
		if value := configString("lvm.vg_name"); value != "" {
			return "lvm:" + value
		}
		if value := configString("source"); value != "" {
			return "lvm:" + value
		}
		return "lvm:" + pool.Name
	}
	return pool.Driver + ":" + pool.Name
}

type storageCapacityBacking struct {
	AvailableBytes int64
	ID             string
}

// storageCapacityBackingOf mirrors
// `(pool.telemetry as { backing?: { availableBytes: number; id: string } } | null)?.backing`.
func storageCapacityBackingOf(telemetry []byte) *storageCapacityBacking {
	backing, ok := storageCapacityJSONObject(telemetry)["backing"].(map[string]any)
	if !ok {
		return nil
	}
	available, availableOK := backing["availableBytes"].(float64)
	id, idOK := backing["id"].(string)
	if !availableOK || !idOK {
		return nil
	}
	return &storageCapacityBacking{AvailableBytes: int64(available), ID: id}
}

// storageCapacityPoolReady mirrors `ready(pool)`.
func storageCapacityPoolReady(pool storageCapacityPoolRow, nowMS int64, maxAgeMS float64) bool {
	if pool.Status != "ready" || pool.ReportedAt == nil {
		return false
	}
	// `Date.now() - reportedAt.getTime() <= maxAge` — a future timestamp passes.
	if float64(nowMS-pool.ReportedAt.UTC().UnixMilli()) > maxAgeMS {
		return false
	}
	config := storageCapacityJSONObject(pool.Config)
	if storageCapacityJSTruthy(config["_payincusImport"]) || storageCapacityJSTruthy(config["_payincusDeletion"]) {
		return false
	}
	return pool.TotalBytes > 0
}

// storageCapacityHostServiceAvailable mirrors
// `evaluateHostService(host).available` for the DB-derivable inputs. The tunnel
// manager is a seam (StorageCapacityTunnelIsOnline) and the host-health cache is
// the real lib.IsHostHealthy.
func storageCapacityHostServiceAvailable(hostID int32, host storageCapacityHostRow, nowMS int64) bool {
	var state struct {
		Locked          bool `json:"locked"`
		RecoveryPending bool `json:"recoveryPending"`
	}
	if len(host.NodeReport) > 0 {
		_ = json.Unmarshal(host.NodeReport, &state)
	}

	// deriveEffectiveHostStatus
	status := host.Status
	if state.Locked {
		status = "locked"
	} else if host.Status != "maintenance" && host.TunnelEnabled && !StorageCapacityTunnelIsOnline(hostID) {
		status = "offline"
	}

	var report struct {
		Incus *struct {
			Available *bool `json:"available"`
		} `json:"incus"`
		Instances *struct {
			Available *bool `json:"available"`
		} `json:"instances"`
		Metrics *struct {
			HeartbeatIntervalSeconds any `json:"heartbeatIntervalSeconds"`
		} `json:"metrics"`
	}
	if len(host.AgentReport) > 0 {
		_ = json.Unmarshal(host.AgentReport, &report)
	}

	interval := 5.0
	if report.Metrics != nil {
		value := storageCapacityJSNumber(report.Metrics.HeartbeatIntervalSeconds)
		if !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 5 && value <= 3600 {
			interval = value
		}
	}

	// Prisma's `host.agent` is null when the LEFT JOIN found no row; a present row
	// always has a non-null status column.
	hasAgent := host.AgentStatus != nil
	stale := false
	if hasAgent {
		limitMS := math.Max(120, interval*3) * 1000
		if *host.AgentStatus != "online" || host.AgentSeenAt == nil ||
			float64(nowMS-host.AgentSeenAt.UTC().UnixMilli()) > limitMS {
			stale = true
		}
	}

	// `(!host.agent || report?.incus?.available === true)`
	incusAvailable := true
	if hasAgent {
		incusAvailable = report.Incus != nil && report.Incus.Available != nil && *report.Incus.Available
	}
	// `report?.instances?.available !== false`
	instancesAvailable := true
	if report.Instances != nil && report.Instances.Available != nil {
		instancesAvailable = *report.Instances.Available
	}

	return !state.RecoveryPending &&
		(status == "online" || status == "maintenance") &&
		(!host.TunnelEnabled || StorageCapacityTunnelIsOnline(hostID)) &&
		!stale &&
		incusAvailable &&
		instancesAvailable &&
		IsHostHealthy(hostID)
}

// storageCapacityJSONObject decodes a jsonb column into a map, treating
// null/garbage as an empty object (`pool.config || {}`).
func storageCapacityJSONObject(raw []byte) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded == nil {
		return map[string]any{}
	}
	return decoded
}

// storageCapacityJSTruthy mirrors JavaScript truthiness for JSON values.
func storageCapacityJSTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case float64:
		return typed != 0 && !math.IsNaN(typed)
	case string:
		return typed != ""
	default:
		return true
	}
}

// storageCapacityJSNumber mirrors `Number(value)` for the report fields a node
// can send (number, numeric string, boolean). Anything else is NaN.
func storageCapacityJSNumber(value any) float64 {
	switch typed := value.(type) {
	case nil:
		return math.NaN()
	case float64:
		return typed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return math.NaN()
		}
		return parsed
	case bool:
		if typed {
			return 1
		}
		return 0
	default:
		return math.NaN()
	}
}

// storageCapacityString dereferences a pool name for the sort tie-break.
func storageCapacityString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// storageCapacityLocaleCompare approximates `String.prototype.localeCompare` with
// the ICU root collation Node uses by default: letters compare case-insensitively
// first, and the lowercase member of a case pair sorts first. This only breaks
// ties between equally usable pools, so an approximation cannot change whether a
// pool is considered available.
func storageCapacityLocaleCompare(left, right string) int {
	if left == right {
		return 0
	}
	lowerLeft, lowerRight := strings.ToLower(left), strings.ToLower(right)
	if lowerLeft != lowerRight {
		return strings.Compare(lowerLeft, lowerRight)
	}
	// Same letters, different case: lowercase first (ICU root order).
	if strings.ToLower(left) == left {
		return -1
	}
	return 1
}
