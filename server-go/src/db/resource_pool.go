// Port of server/src/db/resource-pool.ts plus the storage-capacity slice that
// applyResourcePoolToInstance depends on.
//
// The apply path is a one-shot consumption of user-owned resources, so it is
// serialized exactly like the TypeScript: ONE transaction holding
// pg_advisory_xact_lock(USER_RESOURCE_POOL_LOCK_NAMESPACE = 4114, userId) and then
// pg_advisory_xact_lock(INSTANCE_OPERATION_LOCK_NAMESPACE = 4106, instanceId), in
// that order. The pool debit is a conditional UPDATE (`... WHERE cpu >= $n`) whose
// affected-row count decides success, so a racing request can never push the pool
// below zero. Host usage is always incremented, never written from a stale read,
// and the conditional WHERE on cpu_used/memory_used rejects a claim that would
// oversell the node.
//
// Deliberate deviation, documented for the reviewer: the storage-capacity gate
// (lib/storage-capacity.ts -> evaluateHostService) reads two *process-local*
// inputs in Node that have no Go counterpart yet — hostTunnelManager.isTunnelOnline
// and hostHealth.isHostHealthy. This port evaluates every database-derivable
// condition (raw status, node-security lock/recovery flags, agent staleness,
// report availability, pool readiness/telemetry/capacity) and treats those two
// in-memory signals as available. A tunnel-enabled or health-degraded host is
// therefore not rejected here; the subsequent Incus PATCH surfaces the failure
// and the apply is left reconciliation-pending instead of silently succeeding.
//
// Not ported: addToResourcePool / adminGrantResource / systemGrantResource /
// lotteryGrantResource / deductFromResourcePool (grants and the legacy deduction
// path are owned by the admin, lottery and other modules; no ported route calls
// them).
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// Resource-pool constants, mirroring resource-pool.ts.
const (
	// ResourcePoolMaxApplyAmount mirrors MAX_RESOURCE_POOL_APPLY_AMOUNT.
	ResourcePoolMaxApplyAmount = 104_857_600
	// ResourcePoolIncusPendingPrefix mirrors RESOURCE_POOL_INCUS_PENDING_PREFIX:
	// an apply log whose Incus patch has not been confirmed yet carries this
	// prefix, which is what makes the claim replayable by the reconciler.
	ResourcePoolIncusPendingPrefix = "[incus-pending] "
)

// resourcePoolStorageMIB mirrors the MIB constant in storage-capacity.ts.
const resourcePoolStorageMIB = 1024 * 1024

// Resource-pool apply sentinel errors. The messages are byte-identical to the
// `throw new Error('...')` payloads in resource-pool.ts; routes/resource_pool.ts
// switches on the message.
var (
	ErrResourcePoolHostNotFound     = errors.New("HOST_NOT_FOUND")
	ErrResourcePoolHostInsufficient = errors.New("HOST_RESOURCES_INSUFFICIENT")
	ErrResourcePoolInvalidAmount    = errors.New("Invalid resource pool apply amount")
	ErrResourcePoolInvalidDebit     = errors.New("Invalid resource pool debit amount")
	ErrResourcePoolNegativeDelta    = errors.New("Resource amounts must be non-negative")
	// ErrResourcePoolInstanceGone mirrors Prisma P2025 on `instance.update`.
	ErrResourcePoolInstanceGone     = errors.New("resource pool: instance disappeared during apply")
	errResourcePoolDatabaseNotReady = errors.New("resource pool: database not initialized")
)

// resourcePoolInvalidTypeError mirrors `new Error(\`Invalid resource type: ${t}\`)`.
func resourcePoolInvalidTypeError(resourceType models.RedeemCodeType) error {
	return fmt.Errorf("Invalid resource type: %s", resourceType)
}

// ==================== types ====================

// ResourcePoolBalances mirrors getUserResourcePool. Every value is the
// `bigint.toString()` / `int.toString()` string form, not a JSON number.
type ResourcePoolBalances struct {
	CPU     string `json:"cpu"`
	Memory  string `json:"memory"`
	Disk    string `json:"disk"`
	Traffic string `json:"traffic"`
}

// ResourcePoolInstanceResources mirrors `instanceResources?: {cpu?, memory?, disk?}`.
type ResourcePoolInstanceResources struct {
	CPU    *int32
	Memory *int32
	Disk   *int32
}

// ResourcePoolHostResourceDelta mirrors `hostResourceDelta?: {cpuUsed?, memoryUsed?, diskUsed?}`.
type ResourcePoolHostResourceDelta struct {
	CPUUsed    *int32
	MemoryUsed *int32
	DiskUsed   *int32
}

// ResourcePoolApplyInput mirrors applyResourcePoolToInstance's argument object.
type ResourcePoolApplyInput struct {
	UserID              int32
	InstanceID          int32
	HostID              int32
	ResourceType        models.RedeemCodeType
	Amount              int32
	PoolDebitAmount     *int32
	Remark              *string
	InstanceResources   *ResourcePoolInstanceResources
	HostResourceDelta   *ResourcePoolHostResourceDelta
	MonthlyTrafficDelta *int64
}

// ResourcePoolLogInstanceRef mirrors the `{id, name}` instance projection.
type ResourcePoolLogInstanceRef struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

// ResourcePoolLogItem mirrors one entry of getResourcePoolLogs().records.
type ResourcePoolLogItem struct {
	ID           int32                       `json:"id"`
	Action       string                      `json:"action"`
	ResourceType string                      `json:"resourceType"`
	Amount       string                      `json:"amount"`
	Instance     *ResourcePoolLogInstanceRef `json:"instance"`
	Remark       *string                     `json:"remark"`
	CreatedAt    string                      `json:"createdAt"`
}

// ResourcePoolLogResult mirrors `{records, total}`.
type ResourcePoolLogResult struct {
	Records []ResourcePoolLogItem `json:"records"`
	Total   int64                 `json:"total"`
}

// ResourcePoolLogOptions mirrors getResourcePoolLogs' options object.
type ResourcePoolLogOptions struct {
	Action       *string
	ResourceType *string
	Limit        int
	Offset       int
}

// ResourcePoolInstanceHost mirrors the host projection of getUserAllInstances.
type ResourcePoolInstanceHost struct {
	ID           int32   `json:"id"`
	Name         string  `json:"name"`
	Location     *string `json:"location"`
	CountryCode  string  `json:"countryCode"`
	InstanceType string  `json:"instanceType"`
}

// ResourcePoolInstanceRow mirrors one entry of getUserAllInstances.
type ResourcePoolInstanceRow struct {
	ID                  int32                    `json:"id"`
	Name                string                   `json:"name"`
	Status              string                   `json:"status"`
	CPU                 int32                    `json:"cpu"`
	Memory              int32                    `json:"memory"`
	Disk                int32                    `json:"disk"`
	MonthlyTrafficLimit *int64                   `json:"monthlyTrafficLimit"`
	PackagePlanID       *int32                   `json:"packagePlanId"`
	Host                ResourcePoolInstanceHost `json:"host"`
}

// ResourcePoolInstance is the instance projection the apply route needs.
type ResourcePoolInstance struct {
	ID      int32
	IncusID string
	Name    string
	UserID  int32
	HostID  int32
	Status  string
	CPU     int32
	Memory  int32
	Disk    int32
}

// ResourcePoolHostForApply is the host projection the apply route needs.
type ResourcePoolHostForApply struct {
	ID                 int32
	URL                string
	CertPath           *string
	KeyPath            *string
	EnableResourcePool bool
	InstanceType       models.InstanceType
}

// ResourcePoolPendingApplication mirrors one pending apply log entry.
type ResourcePoolPendingApplication struct {
	ID           int32
	ResourceType string
}

// ResourcePoolReconciliationCandidate mirrors one reconciliation candidate.
type ResourcePoolReconciliationCandidate struct {
	InstanceID          int32
	ResourceTypes       []string
	PendingApplications []ResourcePoolPendingApplication
}

// ResourcePoolStorageUnavailable reasons mirror StorageUnavailableReason.
const (
	ResourcePoolStorageUnavailable = "storage_unavailable"
	ResourcePoolStoragePoolMissing = "storage_pool_missing"
	ResourcePoolStorageDiskFull    = "disk_full"
)

// ResourcePoolStorageCapacity mirrors StorageCapacity.
type ResourcePoolStorageCapacity struct {
	PoolName    *string `json:"poolName"`
	TotalMB     int64   `json:"totalMB"`
	UsedMB      int64   `json:"usedMB"`
	AllocatedMB int64   `json:"allocatedMB"`
	AvailableMB int64   `json:"availableMB"`
	HeadroomMB  int64   `json:"headroomMB"`
	Reason      *string `json:"reason"`
}

// ==================== pool balance ====================

// ResourcePoolGetUserPool mirrors getUserResourcePool. The upsert (with an empty
// update, which still refreshes @updatedAt exactly like Prisma) guarantees a row
// exists so concurrent reads never hit a unique-constraint race.
func ResourcePoolGetUserPool(ctx context.Context, userID int32) (ResourcePoolBalances, error) {
	if Pool() == nil {
		return ResourcePoolBalances{}, errResourcePoolDatabaseNotReady
	}

	var cpu, memory, disk int32
	var traffic int64
	err := Pool().QueryRow(ctx,
		`INSERT INTO user_resource_pools (user_id, updated_at) VALUES ($1, NOW())
		 ON CONFLICT (user_id) DO UPDATE SET updated_at = NOW()
		 RETURNING cpu, memory, disk, traffic`, userID).Scan(&cpu, &memory, &disk, &traffic)
	if err != nil {
		return ResourcePoolBalances{}, err
	}

	return ResourcePoolBalances{
		CPU:     CheckinFormatInt64(int64(cpu)),
		Memory:  CheckinFormatInt64(int64(memory)),
		Disk:    CheckinFormatInt64(int64(disk)),
		Traffic: CheckinFormatInt64(traffic),
	}, nil
}

// ==================== apply ====================

// resourcePoolField mirrors RESOURCE_FIELD_MAP: resource type -> column.
func resourcePoolField(resourceType models.RedeemCodeType) (string, bool) {
	switch resourceType {
	case models.RedeemCodeTypeC:
		return "cpu", true
	case models.RedeemCodeTypeR:
		return "memory", true
	case models.RedeemCodeTypeD:
		return "disk", true
	case models.RedeemCodeTypeT:
		return "traffic", true
	}
	return "", false
}

// resourcePoolPendingRemark mirrors the remark expression of
// applyResourcePoolToInstance: non-traffic applies are stamped with the pending
// prefix so they survive a failed Incus patch as a replayable intent.
func resourcePoolPendingRemark(resourceType models.RedeemCodeType, remark *string) string {
	if resourceType == models.RedeemCodeTypeT {
		if remark == nil {
			return ""
		}
		return *remark
	}
	if remark == nil {
		return ResourcePoolIncusPendingPrefix
	}
	return ResourcePoolIncusPendingPrefix + *remark
}

// ResourcePoolApplyToInstance mirrors applyResourcePoolToInstance. It returns the
// created log id, or nil when the pool balance is insufficient (the TypeScript
// returns `null`, not an error).
func ResourcePoolApplyToInstance(ctx context.Context, input ResourcePoolApplyInput) (*int32, error) {
	if input.Amount <= 0 || input.Amount > ResourcePoolMaxApplyAmount {
		return nil, ErrResourcePoolInvalidAmount
	}
	poolDebitAmount := input.Amount
	if input.PoolDebitAmount != nil {
		poolDebitAmount = *input.PoolDebitAmount
	}
	if poolDebitAmount <= 0 || poolDebitAmount > input.Amount {
		return nil, ErrResourcePoolInvalidDebit
	}

	field, ok := resourcePoolField(input.ResourceType)
	if !ok {
		return nil, resourcePoolInvalidTypeError(input.ResourceType)
	}

	if Pool() == nil {
		return nil, errResourcePoolDatabaseNotReady
	}
	tx, err := Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Same namespace order as resource-pool.ts: user pool first, then instance.
	if err := AdvisoryTransactionLock(ctx, tx, UserResourcePoolLockNamespace, input.UserID); err != nil {
		return nil, err
	}
	if err := AdvisoryTransactionLock(ctx, tx, InstanceOperationLockNamespace, input.InstanceID); err != nil {
		return nil, err
	}

	// Conditional decrement: the row must still hold enough of the resource.
	// Raw SQL in the TypeScript ($executeRaw), so @updatedAt is NOT refreshed here.
	decrementSQL := fmt.Sprintf(
		`UPDATE user_resource_pools SET %[1]s = %[1]s - $1
		 WHERE user_id = $2 AND %[1]s >= $1`, field)
	tag, err := tx.Exec(ctx, decrementSQL, poolDebitAmount, input.UserID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		// Prisma commits a transaction whose callback returned null; nothing was
		// written, so rolling back is behaviourally identical.
		return nil, nil
	}

	remarkValue := resourcePoolPendingRemark(input.ResourceType, input.Remark)
	var remarkArg any
	if input.ResourceType == models.RedeemCodeTypeT && input.Remark == nil {
		remarkArg = nil
	} else {
		remarkArg = remarkValue
	}

	var logID int32
	if err := tx.QueryRow(ctx,
		`INSERT INTO resource_pool_logs (user_id, action, resource_type, amount, instance_id, remark, created_at)
		 VALUES ($1, 'apply', $2, $3, $4, $5, NOW()) RETURNING id`,
		input.UserID, string(input.ResourceType), -poolDebitAmount, input.InstanceID, remarkArg,
	).Scan(&logID); err != nil {
		return nil, err
	}

	if input.HostResourceDelta != nil {
		if err := resourcePoolApplyHostDelta(ctx, tx, input); err != nil {
			return nil, err
		}
	}

	if input.InstanceResources != nil {
		if err := resourcePoolUpdateInstanceResources(ctx, tx, input.InstanceID, input.InstanceResources); err != nil {
			return nil, err
		}
	}

	if input.MonthlyTrafficDelta != nil {
		// Raw SQL in the TypeScript: it does NOT touch updated_at.
		if _, err := tx.Exec(ctx,
			`UPDATE instances SET monthly_traffic_limit = COALESCE(monthly_traffic_limit, 0) + $1
			 WHERE id = $2`, *input.MonthlyTrafficDelta, input.InstanceID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &logID, nil
}

// resourcePoolApplyHostDelta mirrors the hostResourceDelta block: bounded,
// conditional, atomic increments for every dimension.
func resourcePoolApplyHostDelta(ctx context.Context, tx pgx.Tx, input ResourcePoolApplyInput) error {
	delta := input.HostResourceDelta
	cpuDelta := resourcePoolDerefInt32(delta.CPUUsed)
	memoryDelta := resourcePoolDerefInt32(delta.MemoryUsed)
	diskDelta := resourcePoolDerefInt32(delta.DiskUsed)
	if cpuDelta < 0 || memoryDelta < 0 || diskDelta < 0 {
		return ErrResourcePoolNegativeDelta
	}

	var cpuLimit, memoryLimit, storageSize int32
	err := tx.QueryRow(ctx,
		`SELECT cpu_allowance_max, memory_max, storage_size FROM hosts WHERE id = $1`, input.HostID,
	).Scan(&cpuLimit, &memoryLimit, &storageSize)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrResourcePoolHostNotFound
	}
	if err != nil {
		return err
	}
	if cpuLimit < 0 {
		cpuLimit = 0
	}
	if memoryLimit < 0 {
		memoryLimit = 0
	}

	if diskDelta > 0 {
		// Serialize against concurrent capacity claims on the same host.
		if _, err := tx.Exec(ctx, `SELECT id FROM hosts WHERE id = $1 FOR UPDATE`, input.HostID); err != nil {
			return err
		}
		storage, err := resourcePoolGetInstanceStorageCapacity(ctx, tx, input.InstanceID, diskDelta)
		if err != nil {
			return err
		}
		if storage == nil || storage.Reason != nil {
			return ErrResourcePoolHostInsufficient
		}
	}

	if (cpuLimit > 0 && cpuDelta > cpuLimit) || (memoryLimit > 0 && memoryDelta > memoryLimit) {
		return ErrResourcePoolHostInsufficient
	}

	args := []any{cpuDelta, memoryDelta, diskDelta, input.HostID}
	where := ` WHERE id = $4`
	if cpuLimit > 0 {
		args = append(args, cpuLimit-cpuDelta)
		where += fmt.Sprintf(" AND cpu_used <= $%d", len(args))
	}
	if memoryLimit > 0 {
		args = append(args, memoryLimit-memoryDelta)
		where += fmt.Sprintf(" AND memory_used <= $%d", len(args))
	}

	tag, err := tx.Exec(ctx,
		`UPDATE hosts
		    SET cpu_used = cpu_used + $1,
		        memory_used = memory_used + $2,
		        disk_used = disk_used + $3,
		        updated_at = NOW()`+where,
		args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrResourcePoolHostInsufficient
	}
	return nil
}

// resourcePoolUpdateInstanceResources mirrors `tx.instance.update({data})`.
func resourcePoolUpdateInstanceResources(ctx context.Context, tx pgx.Tx, instanceID int32, resources *ResourcePoolInstanceResources) error {
	sets := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if resources.CPU != nil {
		args = append(args, *resources.CPU)
		sets = append(sets, fmt.Sprintf("cpu = $%d", len(args)))
	}
	if resources.Memory != nil {
		args = append(args, *resources.Memory)
		sets = append(sets, fmt.Sprintf("memory = $%d", len(args)))
	}
	if resources.Disk != nil {
		args = append(args, *resources.Disk)
		sets = append(sets, fmt.Sprintf("disk = $%d", len(args)))
	}
	if len(sets) == 0 {
		return nil
	}
	// Prisma's update always refreshes @updatedAt.
	sets = append(sets, "updated_at = NOW()")
	args = append(args, instanceID)
	tag, err := tx.Exec(ctx,
		"UPDATE instances SET "+strings.Join(sets, ", ")+fmt.Sprintf(" WHERE id = $%d", len(args)), args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrResourcePoolInstanceGone
	}
	return nil
}

func resourcePoolDerefInt32(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

// ==================== storage capacity ====================

type resourcePoolStoragePoolRow struct {
	Name       string
	Driver     string
	Config     []byte
	Telemetry  []byte
	Status     string
	TotalBytes int64
	UsedBytes  int64
	ReportedAt *time.Time
}

type resourcePoolStorageInstanceRow struct {
	IncusID         string
	StoragePoolName *string
	Disk            int32
}

type resourcePoolHostStateRow struct {
	Status        string
	DiskUsed      int32
	TunnelEnabled bool
	NodeReport    []byte
	AgentStatus   *string
	AgentSeenAt   *time.Time
	AgentReport   []byte
}

// resourcePoolStorageCapacityEmpty mirrors the `empty(reason)` helper.
func resourcePoolStorageCapacityEmpty(reason string) *ResourcePoolStorageCapacity {
	value := reason
	return &ResourcePoolStorageCapacity{Reason: &value}
}

// resourcePoolGetInstanceStorageCapacity mirrors getInstanceStorageCapacity.
func resourcePoolGetInstanceStorageCapacity(
	ctx context.Context,
	q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
		Query(context.Context, string, ...any) (pgx.Rows, error)
	},
	instanceID int32,
	requiredMB int32,
) (*ResourcePoolStorageCapacity, error) {
	var hostID int32
	var storagePoolName *string
	err := q.QueryRow(ctx,
		`SELECT host_id, storage_pool_name FROM instances WHERE id = $1`, instanceID).Scan(&hostID, &storagePoolName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Instances without a reported root pool cannot be capacity-checked; the
	// TypeScript returns null and the caller rejects the claim.
	if storagePoolName == nil || *storagePoolName == "" {
		return nil, nil
	}
	return resourcePoolGetStorageCapacity(ctx, q, hostID, *storagePoolName, requiredMB)
}

// resourcePoolGetStorageCapacity is a faithful port of getStorageCapacity for the
// poolName+requiredMB call shape used here. The two process-local availability
// inputs of evaluateHostService (tunnel manager, host-health cache) are treated
// as available; see the package-level note in this file.
func resourcePoolGetStorageCapacity(
	ctx context.Context,
	q interface {
		QueryRow(context.Context, string, ...any) pgx.Row
		Query(context.Context, string, ...any) (pgx.Rows, error)
	},
	hostID int32,
	poolName string,
	requiredMB int32,
) (*ResourcePoolStorageCapacity, error) {
	var host resourcePoolHostStateRow
	err := q.QueryRow(ctx,
		`SELECT h.status::text AS status, h.disk_used, h.tunnel_enabled,
		        ns.report AS node_report,
		        a.status AS agent_status, a.last_seen_at AS agent_seen_at, a.last_report AS agent_report
		   FROM hosts h
		   LEFT JOIN node_security_states ns ON ns.host_id = h.id
		   LEFT JOIN host_agents a ON a.host_id = h.id
		  WHERE h.id = $1`, hostID,
	).Scan(&host.Status, &host.DiskUsed, &host.TunnelEnabled, &host.NodeReport,
		&host.AgentStatus, &host.AgentSeenAt, &host.AgentReport)
	if errors.Is(err, pgx.ErrNoRows) {
		return resourcePoolStorageCapacityEmpty(ResourcePoolStorageUnavailable), nil
	}
	if err != nil {
		return nil, err
	}
	// Node availability is checked before pool health and capacity.
	if host.Status != "online" || !resourcePoolHostServiceAvailable(host, time.Now()) {
		return resourcePoolStorageCapacityEmpty(ResourcePoolStorageUnavailable), nil
	}

	pools, err := resourcePoolLoadStoragePools(ctx, q, hostID)
	if err != nil {
		return nil, err
	}
	instances, err := resourcePoolLoadStorageInstances(ctx, q, hostID)
	if err != nil {
		return nil, err
	}

	candidates := make([]resourcePoolStoragePoolRow, 0, len(pools))
	for _, pool := range pools {
		if pool.Name == poolName {
			candidates = append(candidates, pool)
		}
	}
	if len(candidates) == 0 {
		return resourcePoolStorageCapacityEmpty(ResourcePoolStoragePoolMissing), nil
	}

	maxAge := resourcePoolStorageReportMaxAgeMs(host.AgentReport)
	now := time.Now()

	var unassigned int64
	{
		var allocated int64
		for _, instance := range instances {
			allocated += int64(instance.Disk)
		}
		unassigned = int64(host.DiskUsed) - allocated
		if unassigned < 0 {
			unassigned = 0
		}
	}

	choices := make([]ResourcePoolStorageCapacity, 0, len(candidates))
	for _, pool := range candidates {
		totalMB := pool.TotalBytes / resourcePoolStorageMIB
		usedMB := (pool.UsedBytes + resourcePoolStorageMIB - 1) / resourcePoolStorageMIB

		siblings := make([]resourcePoolStoragePoolRow, 0, len(pools))
		group := resourcePoolStorageGroup(pool)
		for _, candidate := range pools {
			if resourcePoolStorageGroup(candidate) == group {
				siblings = append(siblings, candidate)
			}
		}
		members := make(map[string]bool, len(siblings))
		var groupTotal int64
		for _, sibling := range siblings {
			members[sibling.Name] = true
			if candidateTotal := sibling.TotalBytes / resourcePoolStorageMIB; candidateTotal > groupTotal {
				groupTotal = candidateTotal
			}
		}

		var allocatedMB, groupAllocated int64
		for _, instance := range instances {
			if instance.StoragePoolName == nil || *instance.StoragePoolName == pool.Name {
				allocatedMB += int64(instance.Disk)
			}
			if instance.StoragePoolName == nil || members[*instance.StoragePoolName] {
				groupAllocated += int64(instance.Disk)
			}
		}

		headroomMB := resourcePoolStorageHeadroomMB(totalMB)

		var backing *resourcePoolStorageBacking
		var backingReserved int64
		if parsed := resourcePoolStorageBackingOf(pool.Telemetry); parsed != nil {
			backing = parsed
			backingNames := make(map[string]bool, len(pools))
			for _, candidate := range pools {
				candidateBacking := resourcePoolStorageBackingOf(candidate.Telemetry)
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

		availableMB := int64(0)
		if resourcePoolStoragePoolReady(pool, now, maxAge) {
			limits := []int64{
				totalMB - headroomMB - allocatedMB - unassigned,
				totalMB - usedMB - headroomMB,
				groupTotal - resourcePoolStorageHeadroomMB(groupTotal) - groupAllocated - unassigned,
			}
			if backing != nil {
				// Sparse vdevs share their backing filesystem: never promise its
				// remaining bytes twice.
				limits = append(limits, backing.AvailableBytes/resourcePoolStorageMIB-1024-backingReserved-unassigned)
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
			availableMB = minimum
		}

		choice := ResourcePoolStorageCapacity{
			PoolName:    &pool.Name,
			TotalMB:     totalMB,
			UsedMB:      usedMB,
			AllocatedMB: allocatedMB,
			AvailableMB: availableMB,
			HeadroomMB:  headroomMB,
		}
		switch {
		case !resourcePoolStoragePoolReady(pool, now, maxAge):
			reason := ResourcePoolStorageUnavailable
			choice.Reason = &reason
		case availableMB < int64(requiredMB):
			reason := ResourcePoolStorageDiskFull
			choice.Reason = &reason
		}
		choices = append(choices, choice)
	}

	// `choices.sort((a, b) => Number(!!a.reason) - Number(!!b.reason) || b.availableMB - a.availableMB || a.poolName.localeCompare(b.poolName))`
	sort.SliceStable(choices, func(i, j int) bool {
		iHasReason := choices[i].Reason != nil
		jHasReason := choices[j].Reason != nil
		if iHasReason != jHasReason {
			return !iHasReason
		}
		if choices[i].AvailableMB != choices[j].AvailableMB {
			return choices[i].AvailableMB > choices[j].AvailableMB
		}
		return resourcePoolDerefString(choices[i].PoolName) < resourcePoolDerefString(choices[j].PoolName)
	})

	return &choices[0], nil
}

type resourcePoolStorageBacking struct {
	AvailableBytes int64
	ID             string
}

func resourcePoolLoadStoragePools(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, hostID int32) ([]resourcePoolStoragePoolRow, error) {
	rows, err := q.Query(ctx,
		`SELECT name, driver, config, telemetry, status::text AS status, total_bytes, used_bytes, reported_at
		   FROM storage_pools
		  WHERE host_id = $1 AND purpose::text = 'instance_data'
		  ORDER BY name ASC`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pools := make([]resourcePoolStoragePoolRow, 0, 4)
	for rows.Next() {
		var pool resourcePoolStoragePoolRow
		if err := rows.Scan(&pool.Name, &pool.Driver, &pool.Config, &pool.Telemetry, &pool.Status,
			&pool.TotalBytes, &pool.UsedBytes, &pool.ReportedAt); err != nil {
			return nil, err
		}
		pools = append(pools, pool)
	}
	return pools, rows.Err()
}

func resourcePoolLoadStorageInstances(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, hostID int32) ([]resourcePoolStorageInstanceRow, error) {
	rows, err := q.Query(ctx,
		`SELECT incus_id, storage_pool_name, disk FROM instances
		  WHERE host_id = $1 AND status::text <> 'deleted'`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	instances := make([]resourcePoolStorageInstanceRow, 0, 16)
	for rows.Next() {
		var instance resourcePoolStorageInstanceRow
		if err := rows.Scan(&instance.IncusID, &instance.StoragePoolName, &instance.Disk); err != nil {
			return nil, err
		}
		instances = append(instances, instance)
	}
	return instances, rows.Err()
}

// resourcePoolHostServiceAvailable mirrors evaluateHostService(host).available for
// the database-derivable signals. The tunnel manager and the host-health cache are
// process-local in Node and are treated as available here (see the file header).
func resourcePoolHostServiceAvailable(host resourcePoolHostStateRow, now time.Time) bool {
	var state struct {
		Locked          bool `json:"locked"`
		RecoveryPending bool `json:"recoveryPending"`
		ResumeInstances []struct {
			Project string `json:"project"`
			Name    string `json:"name"`
		} `json:"resumeInstances"`
	}
	if len(host.NodeReport) > 0 {
		_ = json.Unmarshal(host.NodeReport, &state)
	}
	if state.Locked || state.RecoveryPending {
		return false
	}

	// deriveEffectiveHostStatus keeps 'maintenance' (an available state) and only
	// overrides the status when a tunnel is down, which this port cannot observe, so
	// the raw status column is authoritative here.
	var report struct {
		Incus *struct {
			Available *bool `json:"available"`
		} `json:"incus"`
		Instances *struct {
			Available *bool `json:"available"`
		} `json:"instances"`
		Metrics *struct {
			HeartbeatIntervalSeconds *float64 `json:"heartbeatIntervalSeconds"`
		} `json:"metrics"`
	}
	if len(host.AgentReport) > 0 {
		_ = json.Unmarshal(host.AgentReport, &report)
	}

	interval := 5.0
	if report.Metrics != nil && report.Metrics.HeartbeatIntervalSeconds != nil {
		value := *report.Metrics.HeartbeatIntervalSeconds
		if !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 5 && value <= 3600 {
			interval = value
		}
	}

	// Prisma's `host.agent` is null when the LEFT JOIN found no row; a present row
	// always has a non-null status column.
	hasAgent := host.AgentStatus != nil
	stale := false
	if hasAgent {
		limitSeconds := math.Max(120, interval*3)
		if *host.AgentStatus != "online" || host.AgentSeenAt == nil ||
			now.Sub(*host.AgentSeenAt) > time.Duration(limitSeconds*float64(time.Second)) {
			stale = true
		}
	}

	// `(!host.agent || report?.incus?.available === true)`: a present agent must
	// positively report availability.
	incusAvailable := true
	if hasAgent {
		incusAvailable = report.Incus != nil && report.Incus.Available != nil && *report.Incus.Available
	}
	// `report?.instances?.available !== false`
	instancesAvailable := true
	if report.Instances != nil && report.Instances.Available != nil {
		instancesAvailable = *report.Instances.Available
	}

	return (host.Status == "online" || host.Status == "maintenance") && !stale && incusAvailable && instancesAvailable
}

// resourcePoolStorageGroup mirrors group(pool) from storage-capacity.ts.
func resourcePoolStorageGroup(pool resourcePoolStoragePoolRow) string {
	config := map[string]any{}
	if len(pool.Config) > 0 {
		_ = json.Unmarshal(pool.Config, &config)
	}
	configString := func(key string) string {
		if value, ok := config[key].(string); ok {
			return value
		}
		return ""
	}

	switch pool.Driver {
	case "dir", "btrfs":
		return "filesystem:" + resourcePoolStorageFilesystemID(pool.Telemetry)
	case "zfs":
		value := configString("zfs.pool_name")
		if value != "" {
			return "zfs:" + strings.Split(value, "/")[0]
		}
		source := configString("source")
		if source != "" {
			return "zfs:" + strings.Split(source, "/")[0]
		}
		return "zfs:" + pool.Name
	case "lvm":
		value := configString("lvm.vg_name")
		if value != "" {
			return "lvm:" + value
		}
		if source := configString("source"); source != "" {
			return "lvm:" + source
		}
		return "lvm:" + pool.Name
	}
	return pool.Driver + ":" + pool.Name
}

// resourcePoolStorageFilesystemID mirrors
// `(pool.telemetry as {filesystemId?: string} | null)?.filesystemId || 'unknown'`.
func resourcePoolStorageFilesystemID(telemetry []byte) string {
	var parsed struct {
		FilesystemID *string `json:"filesystemId"`
	}
	if len(telemetry) > 0 {
		_ = json.Unmarshal(telemetry, &parsed)
	}
	if parsed.FilesystemID == nil || *parsed.FilesystemID == "" {
		return "unknown"
	}
	return *parsed.FilesystemID
}

func resourcePoolStorageBackingOf(telemetry []byte) *resourcePoolStorageBacking {
	var parsed struct {
		Backing *struct {
			AvailableBytes int64  `json:"availableBytes"`
			ID             string `json:"id"`
		} `json:"backing"`
	}
	if len(telemetry) == 0 {
		return nil
	}
	if err := json.Unmarshal(telemetry, &parsed); err != nil || parsed.Backing == nil {
		return nil
	}
	return &resourcePoolStorageBacking{AvailableBytes: parsed.Backing.AvailableBytes, ID: parsed.Backing.ID}
}

// resourcePoolStoragePoolReady mirrors the `ready(pool)` predicate.
func resourcePoolStoragePoolReady(pool resourcePoolStoragePoolRow, now time.Time, maxAge time.Duration) bool {
	if pool.Status != "ready" || pool.ReportedAt == nil || pool.TotalBytes <= 0 {
		return false
	}
	if now.Sub(*pool.ReportedAt) > maxAge {
		return false
	}
	config := map[string]any{}
	if len(pool.Config) > 0 {
		_ = json.Unmarshal(pool.Config, &config)
	}
	// `!config?._payincusImport` / `!config?._payincusDeletion`: JS truthiness, so an
	// explicit false (or 0/"") does not disqualify the pool.
	if !resourcePoolJSONFalsy(config["_payincusImport"]) {
		return false
	}
	if !resourcePoolJSONFalsy(config["_payincusDeletion"]) {
		return false
	}
	return true
}

// resourcePoolJSONFalsy mirrors JavaScript truthiness for a decoded JSON value.
func resourcePoolJSONFalsy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case bool:
		return !typed
	case float64:
		return typed == 0 || math.IsNaN(typed)
	case string:
		return typed == ""
	}
	return false
}

// resourcePoolStorageHeadroomMB mirrors storageHeadroomMB.
func resourcePoolStorageHeadroomMB(totalMB int64) int64 {
	headroom := (totalMB + 99) / 100 // Math.ceil(totalMB / 100)
	if headroom < 1024 {
		headroom = 1024
	}
	if headroom > 10240 {
		headroom = 10240
	}
	return headroom
}

// resourcePoolStorageReportMaxAgeMs mirrors storageReportMaxAgeMs.
func resourcePoolStorageReportMaxAgeMs(report []byte) time.Duration {
	interval := 5.0
	if len(report) > 0 {
		var parsed struct {
			Metrics *struct {
				HeartbeatIntervalSeconds *float64 `json:"heartbeatIntervalSeconds"`
			} `json:"metrics"`
		}
		if err := json.Unmarshal(report, &parsed); err == nil && parsed.Metrics != nil &&
			parsed.Metrics.HeartbeatIntervalSeconds != nil {
			value := *parsed.Metrics.HeartbeatIntervalSeconds
			if !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 5 && value <= 3600 {
				interval = value
			}
		}
	}
	milliseconds := interval * 3000
	if milliseconds < 120000 {
		milliseconds = 120000
	}
	return time.Duration(milliseconds * float64(time.Millisecond))
}

func resourcePoolDerefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// ==================== reconciliation ====================

// ResourcePoolGetApplyReconciliationCandidates mirrors
// getResourcePoolApplyReconciliationCandidates: every apply log that still carries
// the pending prefix on a live instance, grouped per instance.
func ResourcePoolGetApplyReconciliationCandidates(ctx context.Context) ([]ResourcePoolReconciliationCandidate, error) {
	if Pool() == nil {
		return nil, errResourcePoolDatabaseNotReady
	}

	rows, err := Pool().Query(ctx,
		`SELECT r.id, r.instance_id, r.resource_type::text AS resource_type
		   FROM resource_pool_logs r
		   JOIN instances i ON i.id = r.instance_id
		  WHERE r.action = 'apply'
		    AND r.instance_id IS NOT NULL
		    AND r.remark LIKE $1
		    AND i.status::text IN ('running','stopped')`, ResourcePoolIncusPendingPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	grouped := make(map[int32][]ResourcePoolPendingApplication)
	order := make([]int32, 0, 8)
	for rows.Next() {
		var id, instanceID int32
		var resourceType string
		if err := rows.Scan(&id, &instanceID, &resourceType); err != nil {
			return nil, err
		}
		if _, seen := grouped[instanceID]; !seen {
			order = append(order, instanceID)
		}
		grouped[instanceID] = append(grouped[instanceID], ResourcePoolPendingApplication{
			ID:           id,
			ResourceType: resourceType,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	candidates := make([]ResourcePoolReconciliationCandidate, 0, len(order))
	for _, instanceID := range order {
		pending := grouped[instanceID]
		seenTypes := make(map[string]bool, 4)
		resourceTypes := make([]string, 0, 4)
		for _, application := range pending {
			if !seenTypes[application.ResourceType] {
				seenTypes[application.ResourceType] = true
				resourceTypes = append(resourceTypes, application.ResourceType)
			}
		}
		candidates = append(candidates, ResourcePoolReconciliationCandidate{
			InstanceID:          instanceID,
			ResourceTypes:       resourceTypes,
			PendingApplications: pending,
		})
	}
	return candidates, nil
}

// ResourcePoolMarkAppliesReconciled mirrors markResourcePoolAppliesReconciled: it
// clears the pending prefix on exactly the given live apply logs. Empty input is
// a no-op, matching the early return.
func ResourcePoolMarkAppliesReconciled(ctx context.Context, logIDs []int32) error {
	if len(logIDs) == 0 {
		return nil
	}
	if Pool() == nil {
		return errResourcePoolDatabaseNotReady
	}

	rows, err := Pool().Query(ctx,
		`SELECT id, remark FROM resource_pool_logs
		  WHERE id = ANY($1) AND action = 'apply' AND remark LIKE $2`,
		logIDs, ResourcePoolIncusPendingPrefix+"%")
	if err != nil {
		return err
	}
	type logRow struct {
		ID     int32
		Remark *string
	}
	logs := make([]logRow, 0, len(logIDs))
	for rows.Next() {
		var entry logRow
		if err := rows.Scan(&entry.ID, &entry.Remark); err != nil {
			rows.Close()
			return err
		}
		logs = append(logs, entry)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(logs) == 0 {
		return nil
	}

	tx, err := Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	for _, entry := range logs {
		remark := entry.Remark
		if remark == nil {
			continue
		}
		stripped := strings.TrimPrefix(*remark, ResourcePoolIncusPendingPrefix)
		var value any
		if stripped == "" {
			value = nil
		} else {
			value = stripped
		}
		if _, err := tx.Exec(ctx, `UPDATE resource_pool_logs SET remark = $1 WHERE id = $2`, value, entry.ID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ==================== logs ====================

// ResourcePoolGetLogs mirrors getResourcePoolLogs.
func ResourcePoolGetLogs(ctx context.Context, userID int32, options ResourcePoolLogOptions) (ResourcePoolLogResult, error) {
	if Pool() == nil {
		return ResourcePoolLogResult{}, errResourcePoolDatabaseNotReady
	}

	arguments := []any{userID}
	where := " WHERE r.user_id = $1"
	if options.Action != nil {
		arguments = append(arguments, *options.Action)
		where += fmt.Sprintf(" AND r.action::text = $%d", len(arguments))
	}
	if options.ResourceType != nil {
		arguments = append(arguments, *options.ResourceType)
		where += fmt.Sprintf(" AND r.resource_type::text = $%d", len(arguments))
	}

	type logRow struct {
		ID           int32
		Action       string
		ResourceType string
		Amount       int32
		InstanceID   *int32
		InstanceName *string
		Remark       *string
		CreatedAt    time.Time
	}

	queryArgs := append(append([]any{}, arguments...), options.Limit, options.Offset)
	listSQL := `SELECT r.id, r.action::text AS action, r.resource_type::text AS resource_type, r.amount,
		           r.instance_id, i.name AS instance_name, r.remark, r.created_at
		      FROM resource_pool_logs r
		      LEFT JOIN instances i ON i.id = r.instance_id` + where +
		fmt.Sprintf(" ORDER BY r.created_at DESC LIMIT $%d OFFSET $%d", len(arguments)+1, len(arguments)+2)

	var rows []logRow
	if err := DB.WithContext(ctx).Raw(listSQL, queryArgs...).Scan(&rows).Error; err != nil {
		return ResourcePoolLogResult{}, err
	}

	countSQL := `SELECT COUNT(*) FROM resource_pool_logs r` + where
	var total int64
	if err := DB.WithContext(ctx).Raw(countSQL, arguments...).Scan(&total).Error; err != nil {
		return ResourcePoolLogResult{}, err
	}

	records := make([]ResourcePoolLogItem, 0, len(rows))
	for _, row := range rows {
		entry := ResourcePoolLogItem{
			ID:           row.ID,
			Action:       row.Action,
			ResourceType: row.ResourceType,
			Amount:       CheckinFormatInt64(int64(row.Amount)),
			Remark:       resourcePoolStripPendingPrefix(row.Remark),
			CreatedAt:    checkinISOTime(row.CreatedAt),
		}
		if row.InstanceID != nil {
			entry.Instance = &ResourcePoolLogInstanceRef{
				ID:   *row.InstanceID,
				Name: resourcePoolDerefString(row.InstanceName),
			}
		}
		records = append(records, entry)
	}

	return ResourcePoolLogResult{Records: records, Total: total}, nil
}

// resourcePoolStripPendingPrefix mirrors the remark normalization of
// getResourcePoolLogs: only a leading pending prefix is removed, everything else is
// returned untouched (null stays null).
func resourcePoolStripPendingPrefix(remark *string) *string {
	if remark == nil {
		return nil
	}
	if !strings.HasPrefix(*remark, ResourcePoolIncusPendingPrefix) {
		return remark
	}
	stripped := strings.TrimPrefix(*remark, ResourcePoolIncusPendingPrefix)
	return &stripped
}

// ResourcePoolLogSystemRedeemToInstance mirrors logSystemRedeemToInstance: it only
// records the redemption in the resource-pool log — the resource is applied
// directly to the instance, never added to the pool.
func ResourcePoolLogSystemRedeemToInstance(
	ctx context.Context,
	userID int32,
	resourceType models.RedeemCodeType,
	amount int32,
	instanceID int32,
	remark string,
) error {
	if Pool() == nil {
		return errResourcePoolDatabaseNotReady
	}
	_, err := Pool().Exec(ctx,
		`INSERT INTO resource_pool_logs (user_id, action, resource_type, amount, instance_id, remark, created_at)
		 VALUES ($1, 'system_redeem', $2, $3, $4, $5, NOW())`,
		userID, string(resourceType), amount, instanceID, remark)
	return err
}

// ==================== instances ====================

// ResourcePoolGetUserAllInstances mirrors getUserAllInstances: running/stopped
// instances on hosts that enabled the resource-pool feature.
func ResourcePoolGetUserAllInstances(ctx context.Context, userID int32) ([]ResourcePoolInstanceRow, error) {
	if Pool() == nil {
		return nil, errResourcePoolDatabaseNotReady
	}

	type row struct {
		ID                  int32
		Name                string
		Status              string
		CPU                 int32
		Memory              int32
		Disk                int32
		MonthlyTrafficLimit *int64
		PackagePlanID       *int32
		HostID              int32
		HostName            string
		HostLocation        *string
		HostCountryCode     string
		HostInstanceType    string
	}

	var rows []row
	if err := DB.WithContext(ctx).Raw(
		`SELECT i.id, i.name, i.status::text AS status, i.cpu, i.memory, i.disk,
		        i.monthly_traffic_limit, i.package_plan_id,
		        h.id AS host_id, h.name AS host_name, h.location AS host_location,
		        h.country_code AS host_country_code, h.instance_type::text AS host_instance_type
		   FROM instances i
		   JOIN hosts h ON h.id = i.host_id
		  WHERE i.user_id = ? AND i.status IN ('running','stopped') AND h.enable_resource_pool = TRUE
		  ORDER BY i.name ASC`, userID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}

	instances := make([]ResourcePoolInstanceRow, 0, len(rows))
	for _, item := range rows {
		instances = append(instances, ResourcePoolInstanceRow{
			ID:                  item.ID,
			Name:                item.Name,
			Status:              item.Status,
			CPU:                 item.CPU,
			Memory:              item.Memory,
			Disk:                item.Disk,
			MonthlyTrafficLimit: item.MonthlyTrafficLimit,
			PackagePlanID:       item.PackagePlanID,
			Host: ResourcePoolInstanceHost{
				ID:           item.HostID,
				Name:         item.HostName,
				Location:     item.HostLocation,
				CountryCode:  item.HostCountryCode,
				InstanceType: item.HostInstanceType,
			},
		})
	}
	return instances, nil
}

// ResourcePoolGetInstanceByID mirrors the db.getInstanceById projection used by the
// apply route. It returns nil when the instance does not exist.
func ResourcePoolGetInstanceByID(ctx context.Context, instanceID int64) (*ResourcePoolInstance, error) {
	type row struct {
		ID      int32
		IncusID string
		Name    string
		UserID  int32
		HostID  int32
		Status  string
		CPU     int32
		Memory  int32
		Disk    int32
	}
	var rows []row
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, incus_id, name, user_id, host_id, status::text AS status, cpu, memory, disk
		   FROM instances WHERE id = ? LIMIT 1`, instanceID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	item := rows[0]
	return &ResourcePoolInstance{
		ID:      item.ID,
		IncusID: item.IncusID,
		Name:    item.Name,
		UserID:  item.UserID,
		HostID:  item.HostID,
		Status:  item.Status,
		CPU:     item.CPU,
		Memory:  item.Memory,
		Disk:    item.Disk,
	}, nil
}

// ResourcePoolGetHostForApply mirrors the db.getHostById projection used by the
// apply route. It returns nil when the host does not exist.
func ResourcePoolGetHostForApply(ctx context.Context, hostID int64) (*ResourcePoolHostForApply, error) {
	type row struct {
		ID                 int32
		URL                string
		CertPath           *string
		KeyPath            *string
		EnableResourcePool bool
		InstanceType       string
	}
	var rows []row
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, url, cert_path, key_path, enable_resource_pool, instance_type::text AS instance_type
		   FROM hosts WHERE id = ? LIMIT 1`, hostID,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &ResourcePoolHostForApply{
		ID:                 rows[0].ID,
		URL:                rows[0].URL,
		CertPath:           rows[0].CertPath,
		KeyPath:            rows[0].KeyPath,
		EnableResourcePool: rows[0].EnableResourcePool,
		InstanceType:       models.InstanceType(rows[0].InstanceType),
	}, nil
}
