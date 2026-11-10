// Port of server/src/services/instance-traffic-collector.ts plus the counter
// reader it depends on (server/src/lib/incus/incus-traffic.ts#getTrafficCounters),
// which is not part of the Go lib yet.
//
// Why the counter reader lives here: the destroy flow needs a *live* reading right
// before the instance goes away (otherwise the last partial window is lost). The
// TypeScript imports getTrafficCounters from lib/incus; the Go lib still has no
// equivalent (db.TrafficFetchInstanceCounters is an explicit-failure seam), so the
// reader is ported next to its only consumer for now. `db.TrafficFetchInstanceCounters`
// is deliberately NOT re-wired from this package: filling that seam would change the
// traffic-reset module's behaviour, which is outside this module's boundary. The
// exported functions below are ready to be moved into lib/incus_traffic.go later
// without touching callers.
//
// Fidelity notes:
//
//   - `calculateIncrement` treats a counter that moved backwards as 0 (baseline
//     rebuild after a reset), so a node reboot never charges the whole counter.
//   - `advanceTrafficSnapshot` is the shared baseline writer (db.AdvanceTrafficSnapshot);
//     a stale sample is dropped (`accepted=false`) and contributes nothing.
//   - counters are summed over BILLABLE interfaces only (eth0/eth1, or the VM NIC
//     MACs derived from the instance name), with the "looks like an external guest
//     NIC" fallback when Incus reports no eth0/eth1 at all.
//   - the daily bucket is the Shanghai calendar day, matching the established
//     db/traffic.go convention (Node's collector uses `new Date(y,m,d)`, i.e. the
//     process-local midnight; they coincide when TZ=Asia/Shanghai, which is the
//     deployment assumption).
//   - locking: instance key `traffic:instance:<id>` (serialises immediate vs
//     scheduled collection), then user key `traffic:user:<id>`, both with the
//     TypeScript's 30s/5s/50ms timings; the instance lock is renewed while the
//     Incus round-trip is in flight.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// InstanceTrafficInstanceLockPrefix mirrors `traffic:instance:${id}`.
	InstanceTrafficInstanceLockPrefix = "traffic:instance:"
	// InstanceTrafficUserLockPrefix mirrors `traffic:user:${id}`.
	InstanceTrafficUserLockPrefix = "traffic:user:"

	instanceTrafficDefaultExpireMs        = 30000
	instanceTrafficDefaultWaitTimeoutMs   = 5000
	instanceTrafficDefaultRetryIntervalMs = 50

	// instanceTrafficRenewFloorMs mirrors `Math.max(1000, ...)`.
	instanceTrafficRenewFloorMs = 1000
)

var (
	instanceTrafficDigitsPattern        = regexp.MustCompile(`^[0-9]+$`)
	instanceTrafficEthPattern           = regexp.MustCompile(`^eth[0-9]+$`)
	instanceTrafficExternalGuestPattern = regexp.MustCompile(`^en(o|p|s|x)[a-z0-9]+$`)
)

// CollectInstanceTrafficResult mirrors CollectInstanceTrafficResult.
type CollectInstanceTrafficResult struct {
	Success      bool
	Skipped      bool
	TotalDelta   int64
	CurrentUsage int64
	Error        string
}

// CollectTrafficWithClientInput mirrors CollectTrafficWithClientInput.
type CollectTrafficWithClientInput struct {
	InstanceID   int32
	UserID       int32
	IncusID      string
	Client       *lib.IncusClient
	CurrentUsage int64
}

// InstanceTrafficNetworkCounters mirrors NetworkCounters from incus-traffic.ts.
type InstanceTrafficNetworkCounters struct {
	RxBytes     int64
	TxBytes     int64
	RxPackets   int64
	TxPackets   int64
	CPUUsageRaw *int64
	SampledAt   time.Time
}

// ==================== state parsing (pure) ====================

// instanceTrafficInterfaceCounters mirrors InstanceStateNetwork['counters']. The
// fields are `any` because Incus returns numbers for some builds and strings for
// others; counterToBigInt accepts both.
type instanceTrafficInterfaceCounters struct {
	BytesReceived   any `json:"bytes_received"`
	BytesSent       any `json:"bytes_sent"`
	PacketsReceived any `json:"packets_received"`
	PacketsSent     any `json:"packets_sent"`
}

type instanceTrafficNetworkInterface struct {
	Hwaddr   any                               `json:"hwaddr"`
	Counters *instanceTrafficInterfaceCounters `json:"counters"`
}

type instanceTrafficState struct {
	Network map[string]instanceTrafficNetworkInterface `json:"network"`
	CPU     *struct {
		Usage any `json:"usage"`
	} `json:"cpu"`
}

// instanceTrafficCounterToInt mirrors counterToBigInt:
//
//   - a string is parsed only when it is all digits, otherwise 0;
//   - a number is floored and must be finite and > 0, otherwise 0.
//
// BigInteger precision is not observable here: the column is bigint, so a value
// above int64 would have failed the Prisma write too. Such a value is treated as 0
// (the same "corrupt counter" stance as a non-numeric string) instead of wrapping.
func instanceTrafficCounterToInt(value any) int64 {
	switch typed := value.(type) {
	case string:
		if !instanceTrafficDigitsPattern.MatchString(typed) {
			return 0
		}
		parsed, err := strconv.ParseInt(typed, 10, 64)
		if err != nil {
			return 0
		}
		return parsed
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed <= 0 {
			return 0
		}
		if typed >= math.MaxInt64 {
			return 0
		}
		return int64(math.Floor(typed))
	case int64:
		if typed <= 0 {
			return 0
		}
		return typed
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0
		}
		return instanceTrafficCounterToInt(parsed)
	}
	return 0
}

// instanceTrafficNormalizeMac mirrors normalizeMac.
func instanceTrafficNormalizeMac(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := strings.ToLower(strings.TrimSpace(text))
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// instanceTrafficVmNicMacs mirrors generateVmNicMacs from
// server/src/lib/vm-network-identifiers.ts, reusing lib's ported derivation
// (lib.VMNetworkGenerateMacs) instead of keeping a second copy of the hash.
func instanceTrafficVmNicMac(seed, nicLabel string) string {
	return lib.VMNetworkGenerateMac(seed, nicLabel)
}

func instanceTrafficBillableVmMacs(instanceName string) map[string]bool {
	macs := lib.VMNetworkGenerateMacs(instanceName)
	return map[string]bool{
		strings.ToLower(macs.Eth0): true,
		strings.ToLower(macs.Eth1): true,
	}
}

// instanceTrafficIsBillableInterface mirrors isBillableNetworkInterface.
func instanceTrafficIsBillableInterface(name string, data instanceTrafficNetworkInterface, billableVmMacs map[string]bool) bool {
	if name == "lo" {
		return false
	}
	if name == "eth0" || name == "eth1" {
		return true
	}
	mac := instanceTrafficNormalizeMac(data.Hwaddr)
	return mac != nil && billableVmMacs[*mac]
}

// instanceTrafficIsLikelyExternalGuestInterface mirrors
// isLikelyExternalGuestInterface.
func instanceTrafficIsLikelyExternalGuestInterface(name string) bool {
	normalized := strings.ToLower(name)
	return instanceTrafficEthPattern.MatchString(normalized) ||
		instanceTrafficExternalGuestPattern.MatchString(normalized)
}

func instanceTrafficAddCounters(
	totals *InstanceTrafficNetworkCounters, counters *instanceTrafficInterfaceCounters,
) {
	if counters == nil {
		return
	}
	totals.RxBytes += instanceTrafficCounterToInt(counters.BytesReceived)
	totals.TxBytes += instanceTrafficCounterToInt(counters.BytesSent)
	totals.RxPackets += instanceTrafficCounterToInt(counters.PacketsReceived)
	totals.TxPackets += instanceTrafficCounterToInt(counters.PacketsSent)
}

// InstanceTrafficCountersFromState mirrors getTrafficCountersFromState.
func InstanceTrafficCountersFromState(instanceName string, raw []byte) (InstanceTrafficNetworkCounters, error) {
	var state instanceTrafficState
	if err := json.Unmarshal(raw, &state); err != nil {
		return InstanceTrafficNetworkCounters{}, err
	}

	totals := InstanceTrafficNetworkCounters{SampledAt: time.Now().UTC()}
	if state.CPU != nil {
		if usage := instanceTrafficCounterToInt(state.CPU.Usage); usage != 0 {
			totals.CPUUsageRaw = &usage
		}
	}

	billableVmMacs := instanceTrafficBillableVmMacs(instanceName)
	fallbackNames := make([]string, 0)
	hasStrictBillableInterface := false

	for name, data := range state.Network {
		if instanceTrafficIsBillableInterface(name, data, billableVmMacs) {
			hasStrictBillableInterface = true
			instanceTrafficAddCounters(&totals, data.Counters)
			continue
		}
		if instanceTrafficIsLikelyExternalGuestInterface(name) {
			fallbackNames = append(fallbackNames, name)
		}
	}

	if !hasStrictBillableInterface && len(fallbackNames) > 0 {
		// Deterministic order: the TypeScript iterates the object's insertion order,
		// and this branch only sums counters (addition is commutative), so sorting
		// changes nothing observable.
		sortStrings(fallbackNames)
		for _, name := range fallbackNames {
			instanceTrafficAddCounters(&totals, state.Network[name].Counters)
		}
	}

	return totals, nil
}

// sortStrings is a tiny insertion sort to keep the dependency surface small.
func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for current := index; current > 0 && values[current] < values[current-1]; current-- {
			values[current], values[current-1] = values[current-1], values[current]
		}
	}
}

// InstanceTrafficGetCounters mirrors getTrafficCounters(client, instanceName):
// GET /1.0/instances/<name>/state and sum the billable interfaces.
func InstanceTrafficGetCounters(ctx context.Context, client *lib.IncusClient, instanceName string) (InstanceTrafficNetworkCounters, error) {
	if client == nil {
		return InstanceTrafficNetworkCounters{}, errors.New("Incus 客户端未连接")
	}
	raw, err := client.Request(ctx, "GET", "/1.0/instances/"+instanceName+"/state", nil, 0, nil)
	if err != nil {
		return InstanceTrafficNetworkCounters{}, err
	}
	return InstanceTrafficCountersFromState(instanceName, raw)
}

// ==================== counter application ====================

type instanceTrafficApplyResult struct {
	TotalDelta   int64
	CurrentUsage int64
	Skipped      bool
}

// instanceTrafficLockKey mirrors getTrafficLockKey.
func InstanceTrafficLockKey(instanceID int32) string {
	return InstanceTrafficInstanceLockPrefix + strconv.FormatInt(int64(instanceID), 10)
}

// InstanceTrafficUserLockKey mirrors getUserTrafficLockKey.
func InstanceTrafficUserLockKey(userID int32) string {
	return InstanceTrafficUserLockPrefix + strconv.FormatInt(int64(userID), 10)
}

func instanceTrafficIntPtr(value int) *int { return &value }

func instanceTrafficLockOptions(overrides lib.LockOptions) lib.LockOptions {
	options := lib.LockOptions{
		ExpireMs:        instanceTrafficIntPtr(instanceTrafficDefaultExpireMs),
		WaitTimeoutMs:   instanceTrafficIntPtr(instanceTrafficDefaultWaitTimeoutMs),
		RetryIntervalMs: instanceTrafficIntPtr(instanceTrafficDefaultRetryIntervalMs),
	}
	if overrides.ExpireMs != nil {
		options.ExpireMs = overrides.ExpireMs
	}
	if overrides.WaitTimeoutMs != nil {
		options.WaitTimeoutMs = overrides.WaitTimeoutMs
	}
	if overrides.RetryIntervalMs != nil {
		options.RetryIntervalMs = overrides.RetryIntervalMs
	}
	if overrides.OwnerID != "" {
		options.OwnerID = overrides.OwnerID
	}
	return options
}

// instanceTrafficApplyCounters mirrors applyTrafficCounters.
func instanceTrafficApplyCounters(
	ctx context.Context, instanceID, userID int32, counters InstanceTrafficNetworkCounters,
) (instanceTrafficApplyResult, error) {
	normalizedDate := db.ShanghaiDateOnly(time.Now().UTC(), 0)

	userLockResult := lib.WithLock(ctx, InstanceTrafficUserLockKey(userID), func() (*instanceTrafficApplyResult, error) {
		return instanceTrafficApplyCountersTx(ctx, instanceID, userID, counters, normalizedDate)
	}, instanceTrafficLockOptions(lib.LockOptions{}))

	if !userLockResult.Success || userLockResult.Result == nil {
		message := userLockResult.Error
		if message == "" {
			message = "用户流量重置锁获取失败"
		}
		return instanceTrafficApplyResult{}, errors.New(message)
	}
	return *userLockResult.Result, nil
}

// instanceTrafficApplyCountersTx is the Prisma `$transaction` body of
// applyTrafficCounters.
func instanceTrafficApplyCountersTx(
	ctx context.Context, instanceID, userID int32, counters InstanceTrafficNetworkCounters, normalizedDate time.Time,
) (*instanceTrafficApplyResult, error) {
	pool := db.Pool()
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var monthlyTrafficUsed int64
	var status models.InstanceStatus
	var hostID int32
	err = tx.QueryRow(ctx,
		`SELECT monthly_traffic_used, status::text AS status, host_id FROM instances WHERE id = $1`, instanceID).
		Scan(&monthlyTrafficUsed, &status, &hostID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("实例不存在")
	}
	if err != nil {
		return nil, err
	}

	if status != models.InstanceStatusRunning {
		return &instanceTrafficApplyResult{TotalDelta: 0, CurrentUsage: monthlyTrafficUsed, Skipped: true}, nil
	}

	advanced, err := db.AdvanceTrafficSnapshot(ctx, tx, instanceID, db.TrafficSnapshotSample{
		RxRaw:        counters.RxBytes,
		TxRaw:        counters.TxBytes,
		RxPacketsRaw: instanceTrafficInt64Ptr(counters.RxPackets),
		TxPacketsRaw: instanceTrafficInt64Ptr(counters.TxPackets),
		CPUUsageRaw:  counters.CPUUsageRaw,
		SampledAt:    counters.SampledAt,
		Source:       "active-collector",
	})
	if err != nil {
		return nil, err
	}
	if !advanced.Accepted {
		return &instanceTrafficApplyResult{TotalDelta: 0, CurrentUsage: monthlyTrafficUsed, Skipped: true}, nil
	}

	rxIncrement := int64(0)
	txIncrement := int64(0)
	if advanced.Previous != nil {
		rxIncrement = lib.CalculateIncrement(counters.RxBytes, advanced.Previous.RxRaw)
		txIncrement = lib.CalculateIncrement(counters.TxBytes, advanced.Previous.TxRaw)
	}
	totalDelta := rxIncrement + txIncrement

	currentUsage := monthlyTrafficUsed
	if totalDelta > 0 {
		err = tx.QueryRow(ctx,
			`UPDATE instances SET monthly_traffic_used = monthly_traffic_used + $2, updated_at = NOW()
			  WHERE id = $1 RETURNING monthly_traffic_used`, instanceID, totalDelta).
			Scan(&currentUsage)
		if err != nil {
			return nil, err
		}

		// `tx.userQuota.updateMany` (user_quotas has no updatedAt column).
		if _, err := tx.Exec(ctx,
			`UPDATE user_quotas SET monthly_traffic_used = monthly_traffic_used + $2 WHERE user_id = $1`,
			userID, totalDelta,
		); err != nil {
			return nil, err
		}

		// `tx.dailyTraffic.upsert` keyed by @@unique([instanceId, date]).
		if _, err := tx.Exec(ctx, `
			INSERT INTO daily_traffic (instance_id, date, rx_total, tx_total)
			VALUES ($1, $2::date, $3, $4)
			ON CONFLICT (instance_id, date) DO UPDATE
			SET rx_total = daily_traffic.rx_total + EXCLUDED.rx_total,
			    tx_total = daily_traffic.tx_total + EXCLUDED.tx_total`,
			instanceID, normalizedDate, rxIncrement, txIncrement,
		); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &instanceTrafficApplyResult{TotalDelta: totalDelta, CurrentUsage: currentUsage}, nil
}

func instanceTrafficInt64Ptr(value int64) *int64 { return &value }

// ==================== collection entry points ====================

// CollectTrafficForInstanceWithClient mirrors collectTrafficForInstanceWithClient:
// one full read-and-commit while holding the instance lock, renewing it for the
// duration of the Incus round-trip so a slow node cannot let a second collector in.
func CollectTrafficForInstanceWithClient(
	ctx context.Context, input CollectTrafficWithClientInput, overrides lib.LockOptions,
) CollectInstanceTrafficResult {
	options := instanceTrafficLockOptions(overrides)
	lockKey := InstanceTrafficLockKey(input.InstanceID)

	lockResult, err := lib.AcquireLock(ctx, lockKey, options)
	if err != nil || !lockResult.Success || lockResult.OwnerID == "" {
		message := ""
		if err != nil {
			message = err.Error()
		} else {
			message = lockResult.Error
		}
		if message == "" {
			message = "流量采集锁获取失败"
		}
		return CollectInstanceTrafficResult{
			Success: false, Skipped: false, TotalDelta: 0,
			CurrentUsage: input.CurrentUsage, Error: message,
		}
	}

	expireMs := instanceTrafficDefaultExpireMs
	if options.ExpireMs != nil {
		expireMs = *options.ExpireMs
	}
	renewInterval := expireMs / 3
	if renewInterval < instanceTrafficRenewFloorMs {
		renewInterval = instanceTrafficRenewFloorMs
	}

	stopRenew := make(chan struct{})
	var renewWait sync.WaitGroup
	renewWait.Add(1)
	go func() {
		defer renewWait.Done()
		ticker := time.NewTicker(time.Duration(renewInterval) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopRenew:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Mirrors the .catch(console.warn) of the renewal timer.
				_ = lib.ExtendLock(ctx, lockKey, lockResult.OwnerID, options.ExpireMs)
			}
		}
	}()

	defer func() {
		close(stopRenew)
		renewWait.Wait()
		lib.ReleaseLock(ctx, lockKey, lockResult.OwnerID)
	}()

	counters, counterErr := InstanceTrafficGetCounters(ctx, input.Client, input.IncusID)
	if counterErr != nil {
		return CollectInstanceTrafficResult{
			Success: false, Skipped: false, TotalDelta: 0,
			CurrentUsage: input.CurrentUsage, Error: counterErr.Error(),
		}
	}

	applied, applyErr := instanceTrafficApplyCounters(ctx, input.InstanceID, input.UserID, counters)
	if applyErr != nil {
		return CollectInstanceTrafficResult{
			Success: false, Skipped: false, TotalDelta: 0,
			CurrentUsage: input.CurrentUsage, Error: applyErr.Error(),
		}
	}

	return CollectInstanceTrafficResult{
		Success:      true,
		Skipped:      applied.Skipped,
		TotalDelta:   applied.TotalDelta,
		CurrentUsage: applied.CurrentUsage,
	}
}

// ==================== running-instance lookup ====================

type instanceTrafficInstanceRow struct {
	ID                 int32                 `gorm:"column:id"`
	Status             models.InstanceStatus `gorm:"column:status"`
	UserID             int32                 `gorm:"column:user_id"`
	IncusID            string                `gorm:"column:incus_id"`
	MonthlyTrafficUsed int64                 `gorm:"column:monthly_traffic_used"`
}

type instanceTrafficHostRow struct {
	ID       int32   `gorm:"column:id"`
	URL      string  `gorm:"column:url"`
	CertPath *string `gorm:"column:cert_path"`
	KeyPath  *string `gorm:"column:key_path"`
	Status   string  `gorm:"column:status"`
}

// CollectTrafficForRunningInstance mirrors collectTrafficForRunningInstance: read
// the last increment off a running instance and persist it before the caller takes
// the instance down. Every failure is reported as a result value, never as a Go
// error, because callers only log it.
func CollectTrafficForRunningInstance(
	ctx context.Context, instanceID int32, overrides lib.LockOptions,
) CollectInstanceTrafficResult {
	if db.DB == nil {
		return CollectInstanceTrafficResult{Error: "实例不存在"}
	}

	var instance instanceTrafficInstanceRow
	err := db.DB.WithContext(ctx).Raw(
		`SELECT id, status::text AS status, user_id, incus_id, monthly_traffic_used
		   FROM instances WHERE id = ? LIMIT 1`,
		instanceID,
	).Scan(&instance).Error
	if err != nil {
		return CollectInstanceTrafficResult{Error: err.Error()}
	}
	if instance.ID == 0 {
		return CollectInstanceTrafficResult{Error: "实例不存在"}
	}
	if instance.Status != models.InstanceStatusRunning {
		return CollectInstanceTrafficResult{
			Success: true, Skipped: true, TotalDelta: 0, CurrentUsage: instance.MonthlyTrafficUsed,
		}
	}

	var host instanceTrafficHostRow
	err = db.DB.WithContext(ctx).Raw(
		`SELECT h.id, h.url, h.cert_path, h.key_path, h.status::text AS status
		   FROM hosts h JOIN instances i ON i.host_id = h.id
		  WHERE i.id = ? LIMIT 1`, instanceID,
	).Scan(&host).Error
	if err != nil {
		return CollectInstanceTrafficResult{
			TotalDelta: 0, CurrentUsage: instance.MonthlyTrafficUsed, Error: err.Error(),
		}
	}
	if host.ID == 0 {
		return CollectInstanceTrafficResult{
			TotalDelta: 0, CurrentUsage: instance.MonthlyTrafficUsed, Error: "宿主机不存在，无法立即采集流量",
		}
	}
	if host.Status != string(models.HostStatusOnline) {
		return CollectInstanceTrafficResult{
			TotalDelta: 0, CurrentUsage: instance.MonthlyTrafficUsed, Error: "宿主机离线，无法立即采集流量",
		}
	}
	if host.CertPath == nil || host.KeyPath == nil {
		return CollectInstanceTrafficResult{
			TotalDelta: 0, CurrentUsage: instance.MonthlyTrafficUsed, Error: "宿主机证书未配置，无法立即采集流量",
		}
	}

	client, clientErr := lib.GetIncusClient(ctx, lib.IncusClientOptions{
		URL:      host.URL,
		CertPath: host.CertPath,
		KeyPath:  host.KeyPath,
		HostID:   int64(host.ID),
	})
	if clientErr != nil {
		return CollectInstanceTrafficResult{
			TotalDelta: 0, CurrentUsage: instance.MonthlyTrafficUsed, Error: clientErr.Error(),
		}
	}

	return CollectTrafficForInstanceWithClient(ctx, CollectTrafficWithClientInput{
		InstanceID:   instanceID,
		UserID:       instance.UserID,
		IncusID:      instance.IncusID,
		Client:       client,
		CurrentUsage: instance.MonthlyTrafficUsed,
	}, overrides)
}
