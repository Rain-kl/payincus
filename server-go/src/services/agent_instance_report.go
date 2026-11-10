// Port of server/src/services/agent-instance-report.ts.
//
// The node reports what it can see in Incus (status, counters, addresses). The
// panel stays the only authority for business state:
//
//   - only instances that already belong to this host are touched (a node cannot
//     report for another host's rows);
//   - status is only synced for rows currently running/stopped, so a read-only
//     heartbeat can never move an instance out of creating/error/deleted;
//   - traffic is only applied when BOTH the database row and the report say
//     running, and it goes through the same instance/user distributed locks and
//     traffic_snapshots baseline as the active collector (no double counting).
//
// Time: every time.Time that reaches SQL is UTC (WAVE_D_SPEC §0.2b). The
// daily_traffic bucket uses the Shanghai calendar day, matching db.UpsertDailyTraffic
// (the canonical helper for that table in this port).
package services

import (
	"context"
	"errors"
	"log"
	"math"
	"net"
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

// AgentInstanceReportMaxItems mirrors maxAgentInstanceReportItems.
const AgentInstanceReportMaxItems = 2000

// AgentInstanceReportConcurrency mirrors agentReportConcurrency (p-limit(8)).
const AgentInstanceReportConcurrency = 8

var (
	agentCounterPattern     = regexp.MustCompile(`^\d{1,30}$`)
	agentStoragePoolPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
)

// AgentInstanceReportResult mirrors AgentInstanceReportResult. TrafficDeltaBytes
// is a decimal string because it is a BigInt on the wire.
type AgentInstanceReportResult struct {
	Received          int    `json:"received"`
	Matched           int    `json:"matched"`
	StatusUpdated     int    `json:"statusUpdated"`
	TrafficUpdated    int    `json:"trafficUpdated"`
	TrafficDeltaBytes string `json:"trafficDeltaBytes"`
	Skipped           int    `json:"skipped"`
	Errors            int    `json:"errors"`
}

// agentInstanceItem mirrors NormalizedAgentInstanceItem. The address pointers
// distinguish "not reported" (nil pointer) from "reported as null" (pointer to
// nil string).
type agentInstanceItem struct {
	Name            string
	Status          *models.InstanceStatus
	RxBytes         *int64
	TxBytes         *int64
	IPv4            *string
	IPv4Set         bool
	IPv6            *string
	IPv6Set         bool
	StoragePoolName *string
}

// agentReportedInstance mirrors ReportedDbInstance.
type agentReportedInstance struct {
	ID      int32
	IncusID string
	UserID  int32
	Status  models.InstanceStatus
}

func agentReportIsRecord(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, false
	}
	return object, true
}

func agentReportString(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok
}

// agentSanitizeShortString mirrors sanitizeShortString.
func agentSanitizeShortString(value any, maxLength int) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", false
	}
	return agentSliceRunes(trimmed, maxLength), true
}

// agentSliceRunes mirrors String#slice: it counts UTF-16 units, so an astral
// character counts twice and is never split.
func agentSliceRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	units := 0
	for index, runeValue := range value {
		width := 1
		if runeValue > 0xFFFF {
			width = 2
		}
		if units+width > limit {
			return value[:index]
		}
		units += width
	}
	return value
}

// agentParseCounter mirrors parseCounter: accept a safe non-negative JSON
// number, or a decimal string of at most 30 digits. Values beyond int64 cannot
// be stored in the BigInt column, so they are dropped instead of erroring (the
// node never produces them; the TypeScript would fail the write).
func agentParseCounter(value any) *int64 {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) ||
			typed < 0 || typed > 9007199254740991 || typed != math.Trunc(typed) {
			return nil
		}
		parsed := int64(typed)
		return &parsed
	case int64:
		if typed < 0 {
			return nil
		}
		return &typed
	case int:
		if typed < 0 {
			return nil
		}
		parsed := int64(typed)
		return &parsed
	case string:
		trimmed := strings.TrimSpace(typed)
		if !agentCounterPattern.MatchString(trimmed) {
			return nil
		}
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return nil
		}
		return &parsed
	default:
		return nil
	}
}

// agentNormalizeStatus mirrors normalizeStatus: the literal statuses win, then
// mapInstanceStatus, and only running/stopped/error are accepted.
func agentNormalizeStatus(value any) *models.InstanceStatus {
	raw, ok := agentSanitizeShortString(value, 40)
	if !ok {
		return nil
	}
	candidate := raw
	if candidate != "running" && candidate != "stopped" && candidate != "error" {
		candidate = lib.MapInstanceStatus(raw)
	}
	var status models.InstanceStatus
	switch candidate {
	case "running":
		status = models.InstanceStatusRunning
	case "stopped":
		status = models.InstanceStatusStopped
	case "error":
		status = models.InstanceStatusError
	default:
		return nil
	}
	return &status
}

// agentIsIPFamily mirrors `isIP(address) === family`.
func agentIsIPFamily(address string, family int) bool {
	parsed := net.ParseIP(address)
	if parsed == nil {
		return false
	}
	if family == 4 {
		return !strings.Contains(address, ":")
	}
	return strings.Contains(address, ":")
}

// agentNormalizeNetworkAddress mirrors normalizeNetworkAddress: the caller
// reports whether the key was present (set=false means leave the column alone);
// when present, a nil result means "write NULL". An unrecognised type is
// dropped, which the route treats as "not reported".
func agentNormalizeNetworkAddress(value any, family int) (string, bool) {
	if value == nil {
		return "", true
	}
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	address := strings.TrimSpace(text)
	if address == "" {
		return "", true
	}
	if len(address) > 128 || !agentIsIPFamily(address, family) {
		return "", false
	}
	return address, true
}

// agentNormalizeInstanceItems mirrors normalizeAgentInstanceItems.
func agentNormalizeInstanceItems(payload any) []agentInstanceItem {
	object, ok := agentReportIsRecord(payload)
	if !ok {
		return nil
	}
	rawItems, ok := object["items"].([]any)
	if !ok {
		return nil
	}
	if len(rawItems) > AgentInstanceReportMaxItems {
		rawItems = rawItems[:AgentInstanceReportMaxItems]
	}
	items := make([]agentInstanceItem, 0, len(rawItems))
	for _, rawItem := range rawItems {
		record, ok := agentReportIsRecord(rawItem)
		if !ok {
			continue
		}
		name, ok := agentSanitizeShortString(record["name"], 256)
		if !ok {
			continue
		}
		traffic, _ := agentReportIsRecord(record["traffic"])
		network, _ := agentReportIsRecord(record["network"])

		item := agentInstanceItem{
			Name:    name,
			Status:  agentNormalizeStatus(record["status"]),
			RxBytes: agentParseCounter(traffic["rxBytes"]),
			TxBytes: agentParseCounter(traffic["txBytes"]),
		}
		if value, present := network["ipv4"]; present {
			if address, keep := agentNormalizeNetworkAddress(value, 4); keep {
				item.IPv4Set = true
				if address != "" {
					copied := address
					item.IPv4 = &copied
				}
			}
		}
		if value, present := network["ipv6"]; present {
			if address, keep := agentNormalizeNetworkAddress(value, 6); keep {
				item.IPv6Set = true
				if address != "" {
					copied := address
					item.IPv6 = &copied
				}
			}
		}
		if poolName, ok := record["storagePoolName"].(string); ok && agentStoragePoolPattern.MatchString(poolName) {
			copied := poolName
			item.StoragePoolName = &copied
		}
		items = append(items, item)
	}
	return items
}

// agentNormalizeReportSampledAt mirrors normalizeReportSampledAt.
func agentNormalizeReportSampledAt(payload any, receivedAt time.Time) time.Time {
	object, ok := agentReportIsRecord(payload)
	if !ok {
		return receivedAt
	}
	text, ok := object["reportedAt"].(string)
	if !ok {
		return receivedAt
	}
	parsed, ok := agentStorageParseJSDate(text)
	if !ok {
		return receivedAt
	}
	return parsed
}

// agentShouldSyncStatus mirrors shouldSyncStatus.
func agentShouldSyncStatus(current models.InstanceStatus, reported *models.InstanceStatus) bool {
	if reported == nil {
		return false
	}
	return current == models.InstanceStatusRunning || current == models.InstanceStatusStopped
}

// agentShouldApplyTraffic mirrors shouldApplyTraffic.
func agentShouldApplyTraffic(current models.InstanceStatus, item agentInstanceItem) bool {
	if item.Status == nil || *item.Status != models.InstanceStatusRunning ||
		item.RxBytes == nil || item.TxBytes == nil {
		return false
	}
	return current == models.InstanceStatusRunning
}

// agentTrafficLockOptions mirrors trafficLockOptions.
func agentTrafficLockOptions() lib.LockOptions {
	expire := 30000
	wait := 3000
	retry := 50
	return lib.LockOptions{ExpireMs: &expire, WaitTimeoutMs: &wait, RetryIntervalMs: &retry}
}

// agentApplyReportedTraffic mirrors applyReportedTrafficCounters.
func agentApplyReportedTraffic(
	ctx context.Context, instance agentReportedInstance, item agentInstanceItem, now, sampledAt time.Time,
) (bool, int64, error) {
	if !agentShouldApplyTraffic(instance.Status, item) {
		return false, 0, nil
	}

	lockResult := lib.WithLock(ctx, InstanceTrafficLockKey(instance.ID), func() (*agentTrafficApply, error) {
		return agentApplyReportedTrafficLocked(ctx, instance, item, now, sampledAt)
	}, agentTrafficLockOptions())
	if !lockResult.Success || lockResult.Result == nil {
		message := lockResult.Error
		if message == "" {
			message = "Agent traffic lock acquisition failed"
		}
		return false, 0, errors.New(message)
	}
	return lockResult.Result.Updated, lockResult.Result.Delta, nil
}

// agentTrafficApply is the inner transaction result.
type agentTrafficApply struct {
	Updated bool
	Delta   int64
}

// agentApplyReportedTrafficLocked mirrors the `withLock(getUserTrafficLockKey…,
// () => prisma.$transaction(...))` body.
func agentApplyReportedTrafficLocked(
	ctx context.Context, instance agentReportedInstance, item agentInstanceItem, now, sampledAt time.Time,
) (*agentTrafficApply, error) {
	normalizedDate := db.ShanghaiDateOnly(now.UTC(), 0)

	userLockResult := lib.WithLock(ctx, InstanceTrafficUserLockKey(instance.UserID), func() (*agentTrafficApply, error) {
		return agentApplyReportedTrafficTx(ctx, instance, item, normalizedDate, sampledAt)
	}, agentTrafficLockOptions())
	if !userLockResult.Success || userLockResult.Result == nil {
		message := userLockResult.Error
		if message == "" {
			message = "Agent user traffic lock acquisition failed"
		}
		return nil, errors.New(message)
	}
	return userLockResult.Result, nil
}

// agentApplyReportedTrafficTx mirrors the Prisma transaction body: re-read the
// status, advance the snapshot, then add the delta to instance/user/daily rows.
func agentApplyReportedTrafficTx(
	ctx context.Context, instance agentReportedInstance, item agentInstanceItem, normalizedDate, sampledAt time.Time,
) (*agentTrafficApply, error) {
	pool := db.Pool()
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var currentStatus models.InstanceStatus
	err = tx.QueryRow(ctx, `SELECT status::text FROM instances WHERE id = $1`, instance.ID).Scan(&currentStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &agentTrafficApply{Updated: false, Delta: 0}, nil
		}
		return nil, err
	}
	if !agentShouldApplyTraffic(currentStatus, item) {
		return &agentTrafficApply{Updated: false, Delta: 0}, nil
	}

	advanced, err := db.AdvanceTrafficSnapshot(ctx, tx, instance.ID, db.TrafficSnapshotSample{
		RxRaw:     *item.RxBytes,
		TxRaw:     *item.TxBytes,
		SampledAt: sampledAt,
		Source:    lib.TrafficSourceAgentReport,
	})
	if err != nil {
		return nil, err
	}
	if !advanced.Accepted {
		return &agentTrafficApply{Updated: false, Delta: 0}, nil
	}

	rxIncrement := int64(0)
	txIncrement := int64(0)
	if advanced.Previous != nil {
		rxIncrement = lib.CalculateIncrement(*item.RxBytes, advanced.Previous.RxRaw)
		txIncrement = lib.CalculateIncrement(*item.TxBytes, advanced.Previous.TxRaw)
	}
	totalDelta := rxIncrement + txIncrement

	if totalDelta > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE instances SET monthly_traffic_used = monthly_traffic_used + $2, updated_at = $3 WHERE id = $1`,
			instance.ID, totalDelta, time.Now().UTC()); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE user_quotas SET monthly_traffic_used = monthly_traffic_used + $2 WHERE user_id = $1`,
			instance.UserID, totalDelta); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO daily_traffic (instance_id, date, rx_total, tx_total)
			VALUES ($1, $2::date, $3, $4)
			ON CONFLICT (instance_id, date) DO UPDATE
			SET rx_total = daily_traffic.rx_total + EXCLUDED.rx_total,
			    tx_total = daily_traffic.tx_total + EXCLUDED.tx_total`,
			instance.ID, normalizedDate, rxIncrement, txIncrement); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &agentTrafficApply{Updated: true, Delta: totalDelta}, nil
}

// agentBuildStatusUpdateData mirrors buildStatusUpdateData.
type agentStatusUpdate struct {
	Status      models.InstanceStatus
	LastSynced  time.Time
	IPv4        *string
	IPv4Present bool
	IPv6        *string
	IPv6Present bool
}

func agentBuildStatusUpdateData(item agentInstanceItem, now time.Time) *agentStatusUpdate {
	if item.Status == nil {
		return nil
	}
	update := &agentStatusUpdate{Status: *item.Status, LastSynced: now}
	if item.IPv4Set {
		update.IPv4 = item.IPv4
		update.IPv4Present = true
	}
	if item.IPv6Set {
		update.IPv6 = item.IPv6
		update.IPv6Present = true
	}
	return update
}

// agentProcessOneInstanceReport mirrors processOneAgentInstanceReport.
func agentProcessOneInstanceReport(
	ctx context.Context, instance agentReportedInstance, item agentInstanceItem, now, sampledAt time.Time,
) (matched, statusUpdated, trafficUpdated int, deltaBytes string, skipped, errorsCount int) {
	delta := int64(0)
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				errorsCount++
				log.Printf("[AgentReport] Failed to process reported instance: %v", recovered)
			}
		}()

		trafficInstance := instance
		if agentShouldSyncStatus(instance.Status, item.Status) {
			if update := agentBuildStatusUpdateData(item, now); update != nil {
				query := `UPDATE instances SET status = $2, last_synced_at = $3, updated_at = $4`
				args := []any{instance.ID, string(update.Status), update.LastSynced, time.Now().UTC()}
				if update.IPv4Present {
					query += `, ipv4 = $` + strconv.Itoa(len(args)+1)
					args = append(args, update.IPv4)
				}
				if update.IPv6Present {
					query += `, ipv6 = $` + strconv.Itoa(len(args)+1)
					args = append(args, update.IPv6)
				}
				query += ` WHERE id = $1 AND status IN ('running', 'stopped')`
				tag, err := db.Pool().Exec(ctx, query, args...)
				if err != nil {
					errorsCount++
					log.Printf("[AgentReport] Failed to process reported instance: %v", err)
					return
				}
				if tag.RowsAffected() > 0 {
					statusUpdated = int(tag.RowsAffected())
					trafficInstance.Status = update.Status
				}
			}
		} else {
			skipped++
		}

		updated, trafficDelta, err := agentApplyReportedTraffic(ctx, trafficInstance, item, now, sampledAt)
		if err != nil {
			errorsCount++
			log.Printf("[AgentReport] Failed to process reported instance %d: %v", instance.ID, err)
			return
		}
		if updated {
			trafficUpdated = 1
			delta = trafficDelta
		}
	}()

	matched = 1
	return matched, statusUpdated, trafficUpdated, strconv.FormatInt(delta, 10), skipped, errorsCount
}

// ProcessAgentInstanceReport mirrors processAgentInstanceReport.
func ProcessAgentInstanceReport(ctx context.Context, hostID int32, payload any) (AgentInstanceReportResult, error) {
	if db.Pool() == nil {
		return AgentInstanceReportResult{}, errors.New("database not initialized")
	}
	receivedAt := time.Now().UTC()
	sampledAt := agentNormalizeReportSampledAt(payload, receivedAt)
	items := agentNormalizeInstanceItems(payload)
	result := AgentInstanceReportResult{Received: len(items), TrafficDeltaBytes: "0"}
	if len(items) == 0 {
		return result, nil
	}

	incusIDs := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		if seen[item.Name] {
			continue
		}
		seen[item.Name] = true
		incusIDs = append(incusIDs, item.Name)
	}

	registeredPools, err := agentRegisteredStoragePools(ctx, hostID)
	if err != nil {
		return result, err
	}
	for _, item := range items {
		if item.StoragePoolName == nil || !registeredPools[*item.StoragePoolName] {
			continue
		}
		if _, err := db.Pool().Exec(ctx, `
			UPDATE instances SET storage_pool_name = $1, updated_at = $2
			 WHERE host_id = $3 AND incus_id = $4 AND storage_pool_name IS NULL AND status <> 'deleted'`,
			*item.StoragePoolName, time.Now().UTC(), hostID, item.Name); err != nil {
			return result, err
		}
	}

	rows, err := db.Pool().Query(ctx, `
		SELECT id, incus_id, user_id, status::text FROM instances
		 WHERE host_id = $1 AND incus_id = ANY($2) AND status <> 'deleted'`, hostID, incusIDs)
	if err != nil {
		return result, err
	}
	instanceByIncusID := make(map[string]agentReportedInstance, len(items))
	for rows.Next() {
		var record agentReportedInstance
		if err := rows.Scan(&record.ID, &record.IncusID, &record.UserID, &record.Status); err != nil {
			rows.Close()
			return result, err
		}
		if _, exists := instanceByIncusID[record.IncusID]; !exists {
			instanceByIncusID[record.IncusID] = record
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()

	// p-limit(agentReportConcurrency) equivalent: at most 8 items in flight, and
	// the aggregate only depends on the per-item results, not their order.
	type itemResult struct {
		matched        int
		statusUpdated  int
		trafficUpdated int
		deltaBytes     string
		skipped        int
		errors         int
	}
	results := make([]itemResult, len(items))
	semaphore := make(chan struct{}, AgentInstanceReportConcurrency)
	var wg sync.WaitGroup
	for index := range items {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			item := items[index]
			instance, ok := instanceByIncusID[item.Name]
			if !ok {
				results[index] = itemResult{skipped: 1, deltaBytes: "0"}
				return
			}
			matched, statusUpdated, trafficUpdated, deltaBytes, skipped, errorsCount :=
				agentProcessOneInstanceReport(ctx, instance, item, receivedAt, sampledAt)
			results[index] = itemResult{
				matched:        matched,
				statusUpdated:  statusUpdated,
				trafficUpdated: trafficUpdated,
				deltaBytes:     deltaBytes,
				skipped:        skipped,
				errors:         errorsCount,
			}
		}(index)
	}
	wg.Wait()

	trafficDelta := int64(0)
	for _, itemResult := range results {
		result.Matched += itemResult.matched
		result.StatusUpdated += itemResult.statusUpdated
		result.TrafficUpdated += itemResult.trafficUpdated
		result.Skipped += itemResult.skipped
		result.Errors += itemResult.errors
		if parsed, err := strconv.ParseInt(itemResult.deltaBytes, 10, 64); err == nil {
			trafficDelta += parsed
		}
	}
	result.TrafficDeltaBytes = strconv.FormatInt(trafficDelta, 10)
	return result, nil
}

// agentRegisteredStoragePools mirrors
// `prisma.storagePool.findMany({ where: { hostId }, select: { name: true } })`.
func agentRegisteredStoragePools(ctx context.Context, hostID int32) (map[string]bool, error) {
	rows, err := db.Pool().Query(ctx, `SELECT name FROM storage_pools WHERE host_id = $1`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pools := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		pools[name] = true
	}
	return pools, rows.Err()
}
