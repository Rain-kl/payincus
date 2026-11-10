// Port of server/src/services/status-scheduler.ts.
//
// A high-availability oriented instance status reconciler:
//
//   - incremental sync: running instances every 2 minutes, everything else every
//     10 minutes (the tick is every minute; the interval filter does the work);
//   - batching: at most BATCH_SIZE instances per run, picking up to 3x that many
//     rows so the interval filter still has enough left after dropping fresh ones;
//   - host health: an unhealthy host is skipped without touching Incus, and the
//     response time feeds lib/incus/host-health;
//   - debounced error detection: lib.InstanceStatusObservations only lets an
//     "error" observation through after 30 s, because a VM reboot can briefly
//     report Error.
//
// Three behaviours are replicated deliberately and pinned by tests:
//
//  1. `*/1 * * * *` means the first run is at the next whole minute, never
//     immediately. StatusSchedulerNextRun is that calculation, and it works in UTC
//     so a UTC+8 host cannot shift the boundary.
//  2. The p-limit semaphore is created ONCE per job, so the "per host" concurrency
//     of 20 is really a global cap of 20 in-flight instance queries.
//  3. JOB_TIMEOUT_MS is advisory: the TypeScript rejects the await but the work
//     keeps running and isJobRunning is cleared, so the next tick may start another
//     job. That is an existing property of the Node scheduler; "fixing" it here
//     would change the load characteristics.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// Scheduler tuning constants, mirroring the TypeScript module.
const (
	// StatusSchedulerConcurrencyPerHost is the p-limit width. Despite the name it is
	// shared by the whole job (see the file comment).
	StatusSchedulerConcurrencyPerHost = 20
	// StatusSchedulerBatchSize is how many instances one run processes.
	StatusSchedulerBatchSize = 50
	// StatusSchedulerJobTimeout is the advisory whole-job budget (5 minutes).
	StatusSchedulerJobTimeout = 5 * time.Minute
	// StatusSchedulerInstanceTimeout is the per-instance state query budget.
	StatusSchedulerInstanceTimeout = 10 * time.Second
	// StatusSchedulerSyncIntervalRunning is the re-sync interval for running instances.
	StatusSchedulerSyncIntervalRunning int64 = 2 * 60 * 1000
	// StatusSchedulerSyncIntervalStopped is the re-sync interval for everything else.
	StatusSchedulerSyncIntervalStopped int64 = 10 * 60 * 1000
)

// StatusSchedulerHostStats mirrors the per-host entry of SyncStats.hostStats.
type StatusSchedulerHostStats struct {
	Checked         int     `json:"checked"`
	Changed         int     `json:"changed"`
	AvgResponseTime float64 `json:"avgResponseTime"`
}

// StatusSchedulerSyncStats mirrors the SyncStats interface.
//
// Divergence worth knowing: the TypeScript field is a Map, and JSON.stringify turns a
// Map into {} — so a route that returned getSyncStats() would emit an empty
// hostStats. Go serialises the entries. No route consumes getSyncStats() today; if
// one does, pick the wire shape deliberately instead of inheriting the {} accident.
type StatusSchedulerSyncStats struct {
	TotalChecked int                                `json:"totalChecked"`
	TotalChanged int                                `json:"totalChanged"`
	TotalErrors  int                                `json:"totalErrors"`
	HostStats    map[int32]StatusSchedulerHostStats `json:"hostStats"`
}

var (
	statusSchedulerMu        sync.Mutex
	statusSchedulerRunning   atomic.Bool
	statusSchedulerStarted   atomic.Bool
	statusSchedulerLastStats *StatusSchedulerSyncStats
	// statusSchedulerNow is Date.now(). Tests move it; the scheduler passes UTC.
	statusSchedulerNow = func() int64 { return time.Now().UTC().UnixMilli() }
)

// GetStatusSchedulerSyncStats mirrors getSyncStats.
func GetStatusSchedulerSyncStats() *StatusSchedulerSyncStats {
	statusSchedulerMu.Lock()
	defer statusSchedulerMu.Unlock()
	return statusSchedulerLastStats
}

// StatusSchedulerHostHealth mirrors one entry of getHostHealthStatus.
type StatusSchedulerHostHealth struct {
	LastResponseTime int64 `json:"lastResponseTime"`
	ErrorCount       int   `json:"errorCount"`
	Healthy          bool  `json:"healthy"`
}

// GetStatusSchedulerHostHealth mirrors getHostHealthStatus.
//
// The TypeScript returns a Map (which JSON.stringify would flatten to {}); the Go
// port returns the equivalent keyed map so a future admin route has real data.
func GetStatusSchedulerHostHealth() map[int32]StatusSchedulerHostHealth {
	result := make(map[int32]StatusSchedulerHostHealth)
	for hostID, health := range lib.GetAllHostHealth() {
		result[hostID] = StatusSchedulerHostHealth{
			LastResponseTime: health.LastResponseTime,
			ErrorCount:       health.ErrorCount,
			Healthy:          lib.IsHostHealthy(hostID),
		}
	}
	return result
}

// ==================== Sync selection (pure) ====================

// StatusSchedulerInstance is one row of getInstancesToSync (the instance plus the
// host fields and the user id the sync needs).
type StatusSchedulerInstance struct {
	ID           int32      `gorm:"column:id"`
	IncusID      string     `gorm:"column:incus_id"`
	Name         string     `gorm:"column:name"`
	Status       string     `gorm:"column:status"`
	HostID       int32      `gorm:"column:host_id"`
	UserID       int32      `gorm:"column:user_id"`
	LastSyncedAt *time.Time `gorm:"column:last_synced_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
	HostName     string     `gorm:"column:host_name"`
	HostURL      string     `gorm:"column:host_url"`
	HostCertPath *string    `gorm:"column:host_cert_path"`
	HostKeyPath  *string    `gorm:"column:host_key_path"`
}

// StatusSchedulerSyncIntervalMs mirrors the interval ternary: running → 2 min,
// anything else → 10 min.
func StatusSchedulerSyncIntervalMs(status string) int64 {
	if status == string(models.InstanceStatusRunning) {
		return StatusSchedulerSyncIntervalRunning
	}
	return StatusSchedulerSyncIntervalStopped
}

// StatusSchedulerDueForSync mirrors
// `now - (lastSyncedAt ?? updatedAt).getTime() > interval`.
//
// The comparison is strict: exactly one interval after the last sync is still "not
// due". Times are absolute unix milliseconds, so the host timezone cannot shift it.
func StatusSchedulerDueForSync(lastSyncMillis, nowMillis int64, status string) bool {
	return nowMillis-lastSyncMillis > StatusSchedulerSyncIntervalMs(status)
}

// StatusSchedulerLastSyncMillis mirrors `(lastSyncedAt ?? updatedAt).getTime()`.
func StatusSchedulerLastSyncMillis(instance StatusSchedulerInstance) int64 {
	if instance.LastSyncedAt != nil {
		return instance.LastSyncedAt.UTC().UnixMilli()
	}
	return instance.UpdatedAt.UTC().UnixMilli()
}

// StatusSchedulerFilterDue mirrors the filter + slice(0, batchSize) step.
func StatusSchedulerFilterDue(instances []StatusSchedulerInstance, nowMillis int64, batchSize int) []StatusSchedulerInstance {
	due := make([]StatusSchedulerInstance, 0, len(instances))
	for _, instance := range instances {
		if StatusSchedulerDueForSync(StatusSchedulerLastSyncMillis(instance), nowMillis, instance.Status) {
			due = append(due, instance)
		}
	}
	if batchSize >= 0 && len(due) > batchSize {
		return due[:batchSize]
	}
	return due
}

// StatusSchedulerHostGroup is one bucket of the by-host grouping.
type StatusSchedulerHostGroup struct {
	HostID    int32
	Instances []StatusSchedulerInstance
}

// StatusSchedulerGroupByHost mirrors the Map grouping: hosts appear in the order
// their first instance appeared, instances keep their relative order.
func StatusSchedulerGroupByHost(instances []StatusSchedulerInstance) []StatusSchedulerHostGroup {
	byHost := make(map[int32]int)
	groups := make([]StatusSchedulerHostGroup, 0)
	for _, instance := range instances {
		index, ok := byHost[instance.HostID]
		if !ok {
			index = len(groups)
			byHost[instance.HostID] = index
			groups = append(groups, StatusSchedulerHostGroup{HostID: instance.HostID})
		}
		groups[index].Instances = append(groups[index].Instances, instance)
	}
	return groups
}

// ==================== Sync decision (pure) ====================

// StatusSchedulerAction is what syncInstanceStatus should do after a successful
// state read.
type StatusSchedulerAction int

const (
	// StatusSchedulerActionSkip means "return {changed:false}" without a write.
	StatusSchedulerActionSkip StatusSchedulerAction = iota
	// StatusSchedulerActionApplyTransition writes the observed status.
	StatusSchedulerActionApplyTransition
	// StatusSchedulerActionTouch only refreshes lastSyncedAt.
	StatusSchedulerActionTouch
)

// StatusSchedulerGuard mirrors the guard sequence of syncInstanceStatus.
type StatusSchedulerGuard struct {
	Action StatusSchedulerAction
	// Announce is set when the transition is running → stopped; the caller still has
	// to re-check for an active operation before notifying (the TypeScript does).
	Announce bool
}

// StatusSchedulerEvaluate runs the guard sequence that decides whether an observed
// Incus status may be written.
//
// observedStatus is the already-mapped status; "" means the node returned no status
// at all and "unknown" is lib.MapInstanceStatus's fallback. Both forget the
// observation window and skip.
func StatusSchedulerEvaluate(
	observations *lib.InstanceStatusObservations,
	instanceID int32,
	currentStatus string,
	observedStatus string,
	hasActiveOperation bool,
	nowMillis int64,
) StatusSchedulerGuard {
	if observedStatus == "" || observedStatus == "unknown" {
		observations.Forget(instanceID)
		return StatusSchedulerGuard{Action: StatusSchedulerActionSkip}
	}
	if hasActiveOperation {
		observations.Forget(instanceID)
		return StatusSchedulerGuard{Action: StatusSchedulerActionSkip}
	}
	if !observations.ShouldApply(instanceID, currentStatus, observedStatus, nowMillis) {
		return StatusSchedulerGuard{Action: StatusSchedulerActionSkip}
	}
	if currentStatus != observedStatus {
		return StatusSchedulerGuard{
			Action:   StatusSchedulerActionApplyTransition,
			Announce: currentStatus == string(models.InstanceStatusRunning) && observedStatus == string(models.InstanceStatusStopped),
		}
	}
	return StatusSchedulerGuard{Action: StatusSchedulerActionTouch}
}

// ==================== Runtime scheduling ====================

// StatusSchedulerNextRun mirrors the next fire time of node-cron's `*/1 * * * *`:
// the next whole minute, computed in UTC so the host timezone is irrelevant.
//
// A `now` that sits exactly on a minute boundary returns the following minute, which
// is what cron does (it does not re-fire immediately).
func StatusSchedulerNextRun(now time.Time) time.Time {
	return now.UTC().Truncate(time.Minute).Add(time.Minute)
}

// StatusSchedulerStart mirrors startStatusScheduler: idempotent, first run at the
// next minute boundary, then every minute.
func StartStatusScheduler(ctx context.Context) {
	if !statusSchedulerStarted.CompareAndSwap(false, true) {
		return
	}

	go func() {
		for {
			next := StatusSchedulerNextRun(time.Now().UTC())
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				// node-cron invokes the callback and the TypeScript attaches
				// `.catch(console.error)`; runStatusSyncJob already swallows its errors,
				// so a goroutine is enough.
				go RunStatusSyncJob(ctx)
			}
		}
	}()

	log.Printf("[StatusSync] Scheduler started (check interval: 1 minute, running sync: 2 min, stopped sync: 10 min)")
}

// statusSchedulerIsJobRunning exposes the module flag to the in-package tests; the
// TypeScript keeps it private and so does the exported surface here.
func statusSchedulerIsJobRunning() bool { return statusSchedulerRunning.Load() }

// statusSchedulerResetForTest / statusSchedulerSetRunningForTest let the tests drive
// the idempotence and re-entrancy guards without starting the scheduler loop.
func statusSchedulerResetForTest() { statusSchedulerStarted.Store(false) }
func statusSchedulerSetRunningForTest(value bool) {
	statusSchedulerRunning.Store(value)
}

// ==================== DB and Incus boundary ====================

// HasActiveInstanceOperation mirrors hasActiveInstanceOperation.
//
// True when any of instance_tasks / restore_tasks / backup_upload_tasks still has a
// PENDING or PROCESSING row for the instance. During those operations Incus briefly
// deletes and recreates the instance, so a missing incus_id is expected and must not
// be read as "instance was deleted".
func HasActiveInstanceOperation(ctx context.Context, instanceID int32) (bool, error) {
	if db.DB == nil {
		return false, errors.New("database not initialised")
	}

	statuses := []string{
		string(models.InstanceTaskStatusPENDING), string(models.InstanceTaskStatusPROCESSING),
	}

	var instanceTasks int64
	if err := db.DB.WithContext(ctx).Model(&models.InstanceTask{}).
		Where("instance_id = ? AND status IN ?", instanceID, statuses).
		Count(&instanceTasks).Error; err != nil {
		return false, err
	}
	if instanceTasks > 0 {
		return true, nil
	}

	var restoreTasks int64
	if err := db.DB.WithContext(ctx).Model(&models.RestoreTask{}).
		Where("instance_id = ? AND status IN ?", instanceID, statuses).
		Count(&restoreTasks).Error; err != nil {
		return false, err
	}
	if restoreTasks > 0 {
		return true, nil
	}

	var uploadTasks int64
	if err := db.DB.WithContext(ctx).Model(&models.BackupUploadTask{}).
		Where("instance_id = ? AND status IN ?", instanceID, statuses).
		Count(&uploadTasks).Error; err != nil {
		return false, err
	}
	return uploadTasks > 0, nil
}

// statusSchedulerLoadBatch mirrors the prisma.instance.findMany of getInstancesToSync.
//
// The TypeScript orders by `status asc` (PostgreSQL string order: error, running,
// stopped) and then `updatedAt desc`, and takes batchSize*3 rows because the interval
// filter below discards most of them.
func statusSchedulerLoadBatch(ctx context.Context, batchSize int) ([]StatusSchedulerInstance, error) {
	if db.DB == nil {
		return nil, errors.New("database not initialised")
	}
	instances := make([]StatusSchedulerInstance, 0)
	err := db.DB.WithContext(ctx).
		Table("instances").
		Select(`instances.id, instances.incus_id, instances.name, instances.status,
			instances.host_id, instances.user_id, instances.last_synced_at, instances.updated_at,
			hosts.name AS host_name, hosts.url AS host_url,
			hosts.cert_path AS host_cert_path, hosts.key_path AS host_key_path`).
		Joins("JOIN hosts ON hosts.id = instances.host_id").
		// creating is deliberately excluded: the timeout sweeper and createInstanceAsync
		// own those rows.
		Where("instances.status IN ?", []string{
			string(models.InstanceStatusRunning), string(models.InstanceStatusStopped), string(models.InstanceStatusError),
		}).
		Where("hosts.status = ?", string(models.HostStatusOnline)).
		Order("instances.status asc").
		Order("instances.updated_at desc").
		Limit(batchSize * 3).
		Scan(&instances).Error
	if err != nil {
		return nil, err
	}
	return instances, nil
}

// statusSchedulerGetInstancesToSync mirrors getInstancesToSync.
func statusSchedulerGetInstancesToSync(ctx context.Context, batchSize int) ([]StatusSchedulerInstance, error) {
	candidates, err := statusSchedulerLoadBatch(ctx, batchSize)
	if err != nil {
		return nil, err
	}
	return StatusSchedulerFilterDue(candidates, statusSchedulerNow(), batchSize), nil
}

// StatusSchedulerSyncResult mirrors the object syncInstanceStatus returns.
type StatusSchedulerSyncResult struct {
	Changed bool   `json:"changed"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Error   string `json:"error,omitempty"`
}

// statusSchedulerInstanceState is the slice of GET /1.0/instances/{name}/state we read.
type statusSchedulerInstanceState struct {
	Status string `json:"status"`
}

// syncInstanceStatus mirrors syncInstanceStatus.
func syncInstanceStatus(ctx context.Context, instance StatusSchedulerInstance) StatusSchedulerSyncResult {
	start := time.Now()

	if !lib.IsHostHealthy(instance.HostID) {
		return StatusSchedulerSyncResult{Error: "Host unhealthy, skipped"}
	}

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		// The TypeScript's try/catch covers this lookup too, so a failure takes the
		// same path (host error counter, not-found downgrade check).
		return statusSchedulerHandleError(ctx, instance, err)
	}
	if host == nil {
		return StatusSchedulerSyncResult{Error: "Host not found"}
	}

	client, err := lib.GetIncusClient(ctx, lib.IncusClientOptions{
		URL:           host.URL,
		CertPath:      host.CertPath,
		KeyPath:       host.KeyPath,
		TunnelEnabled: host.TunnelEnabled,
		HostID:        int64(host.ID),
		TargetHost:    host.TargetHost,
		TargetPort:    int(host.TargetPort),
	})
	if err != nil {
		return statusSchedulerHandleError(ctx, instance, err)
	}

	queryCtx, cancel := context.WithTimeout(ctx, StatusSchedulerInstanceTimeout)
	raw, err := client.Request(queryCtx, http.MethodGet, "/1.0/instances/"+instance.IncusID+"/state", nil, StatusSchedulerInstanceTimeout, nil)
	cancel()
	if err != nil {
		return statusSchedulerHandleError(ctx, instance, err)
	}

	responseTime := time.Since(start).Milliseconds()
	lib.RecordHostSuccess(instance.HostID, responseTime)

	var state statusSchedulerInstanceState
	if err := json.Unmarshal(raw, &state); err != nil {
		lib.InstanceStatusObservationsShared.Forget(instance.ID)
		return StatusSchedulerSyncResult{Error: "invalid state payload: " + err.Error()}
	}

	incusStatus := lib.MapInstanceStatus(state.Status)

	hasActiveOperation, err := HasActiveInstanceOperation(ctx, instance.ID)
	if err != nil {
		lib.InstanceStatusObservationsShared.Forget(instance.ID)
		return statusSchedulerHandleError(ctx, instance, err)
	}

	guard := StatusSchedulerEvaluate(
		lib.InstanceStatusObservationsShared, instance.ID,
		instance.Status, incusStatus, hasActiveOperation, statusSchedulerNow(),
	)

	switch guard.Action {
	case StatusSchedulerActionSkip:
		return StatusSchedulerSyncResult{Changed: false}

	case StatusSchedulerActionTouch:
		// The status matches; refresh lastSyncedAt so the interval filter moves on.
		result := db.DB.WithContext(ctx).Model(&models.Instance{}).
			Where("id = ? AND status <> ?", instance.ID, string(models.InstanceStatusDeleted)).
			Updates(map[string]any{"last_synced_at": time.Now().UTC()})
		if result.Error != nil {
			return statusSchedulerHandleError(ctx, instance, result.Error)
		}
		return StatusSchedulerSyncResult{Changed: false}

	default: // StatusSchedulerActionApplyTransition
		update := db.DB.WithContext(ctx).Model(&models.Instance{}).
			Where("id = ? AND incus_id = ? AND host_id = ? AND status = ?",
				instance.ID, instance.IncusID, instance.HostID, instance.Status).
			Updates(map[string]any{
				"status":         incusStatus,
				"last_synced_at": time.Now().UTC(),
			})
		if update.Error != nil {
			return statusSchedulerHandleError(ctx, instance, update.Error)
		}
		if update.RowsAffected == 0 {
			// Someone else moved the row first; the observation window stays as it is.
			return StatusSchedulerSyncResult{Changed: false}
		}

		if guard.Announce {
			// A rebuild/migration/restore/backup upload stops the instance on purpose;
			// re-check so those do not page the user as an unexpected stop.
			activeAfterStop, opErr := HasActiveInstanceOperation(ctx, instance.ID)
			if opErr != nil || activeAfterStop {
				log.Printf("[StatusSync] Instance %d stopped during an active operation, skip unexpected-stop notification", instance.ID)
			} else {
				result := lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceUnexpectedStop, lib.EventData{
					"instanceName": instance.Name,
					"hostName":     instance.HostName,
				})
				if result.Failed > 0 {
					log.Printf("[StatusSync] Failed to send notification: sent=%d failed=%d", result.Sent, result.Failed)
				}
			}
		}

		return StatusSchedulerSyncResult{Changed: true, From: instance.Status, To: incusStatus}
	}
}

// statusSchedulerHandleError mirrors the catch block of syncInstanceStatus.
//
// A "not found" error means the instance disappeared from Incus. Two guards keep
// accounting intact:
//   - error-state rows may have no Incus instance but still carry refund rights, so
//     they are never downgraded to deleted;
//   - an in-flight operation deletes and recreates the instance, so a missing incus_id
//     there is expected.
func statusSchedulerHandleError(ctx context.Context, instance StatusSchedulerInstance, err error) StatusSchedulerSyncResult {
	lib.InstanceStatusObservationsShared.Forget(instance.ID)
	errorMessage := err.Error()
	lib.RecordHostError(instance.HostID)

	if strings.Contains(errorMessage, "not found") || strings.Contains(errorMessage, "Instance not found") {
		if instance.Status == string(models.InstanceStatusError) {
			return StatusSchedulerSyncResult{Changed: false, Error: errorMessage}
		}
		activeOperation, opErr := HasActiveInstanceOperation(ctx, instance.ID)
		if opErr == nil && activeOperation {
			log.Printf("[StatusSync] Instance %d not found in Incus but has an active operation, skip deleted downgrade", instance.ID)
			return StatusSchedulerSyncResult{Changed: false, Error: "Active operation in progress, delete downgrade skipped"}
		}
		log.Printf("[StatusSync] Instance %d not found in Incus, marking as deleted", instance.ID)
		if updateErr := db.UpdateInstanceStatus(ctx, instance.ID, models.InstanceStatusDeleted, nil); updateErr != nil {
			return StatusSchedulerSyncResult{Changed: false, Error: updateErr.Error()}
		}
		return StatusSchedulerSyncResult{Changed: true, From: instance.Status, To: string(models.InstanceStatusDeleted)}
	}

	return StatusSchedulerSyncResult{Changed: false, Error: errorMessage}
}

// executeStatusSyncJob mirrors executeStatusSyncJob.
func executeStatusSyncJob(ctx context.Context) error {
	start := time.Now()

	instances, err := statusSchedulerGetInstancesToSync(ctx, StatusSchedulerBatchSize)
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		log.Printf("[StatusSync] No instances need sync")
		return nil
	}

	log.Printf("[StatusSync] Processing %d instances...", len(instances))

	groups := StatusSchedulerGroupByHost(instances)

	// One semaphore for the whole job: the TypeScript creates the p-limit once and
	// shares it across hosts, so the effective cap is global.
	semaphore := make(chan struct{}, StatusSchedulerConcurrencyPerHost)
	hostStats := make([]StatusSchedulerHostStats, len(groups))
	var totalErrors int64
	var jobWait sync.WaitGroup

	for index := range groups {
		jobWait.Add(1)
		go func(index int) {
			defer jobWait.Done()

			group := groups[index]
			hostStart := time.Now()
			var checked, changed int
			var counters sync.Mutex
			var hostWait sync.WaitGroup

			for _, instance := range group.Instances {
				hostWait.Add(1)
				go func(instance StatusSchedulerInstance) {
					defer hostWait.Done()
					semaphore <- struct{}{}
					defer func() { <-semaphore }()

					result := syncInstanceStatus(ctx, instance)

					counters.Lock()
					checked++
					if result.Changed {
						changed++
						log.Printf("[StatusSync] Instance %d (%s): %s → %s", instance.ID, instance.Name, result.From, result.To)
					}
					counters.Unlock()
					if result.Error != "" {
						atomic.AddInt64(&totalErrors, 1)
					}
				}(instance)
			}
			hostWait.Wait()

			duration := time.Since(hostStart)
			average := 0.0
			if checked > 0 {
				average = float64(duration.Milliseconds()) / float64(checked)
			}
			counters.Lock()
			hostStats[index] = StatusSchedulerHostStats{Checked: checked, Changed: changed, AvgResponseTime: average}
			counters.Unlock()
		}(index)
	}
	jobWait.Wait()

	stats := &StatusSchedulerSyncStats{TotalErrors: int(atomic.LoadInt64(&totalErrors)), HostStats: map[int32]StatusSchedulerHostStats{}}
	for index, group := range groups {
		entry := hostStats[index]
		stats.HostStats[group.HostID] = entry
		stats.TotalChecked += entry.Checked
		stats.TotalChanged += entry.Changed
	}

	statusSchedulerMu.Lock()
	statusSchedulerLastStats = stats
	statusSchedulerMu.Unlock()

	log.Printf("[StatusSync] Completed in %dms: checked=%d, changed=%d, errors=%d",
		time.Since(start).Milliseconds(), stats.TotalChecked, stats.TotalChanged, stats.TotalErrors)
	return nil
}

// RunStatusSyncJob mirrors runStatusSyncJob.
//
// The job timeout is advisory on purpose: the TypeScript abandons the await but the
// work keeps going, and the re-entrancy flag is cleared so the next tick may start
// another job. Cancelling the context would be a behaviour change (it would abort
// in-flight DB writes), so the deadline only stops *waiting*.
func RunStatusSyncJob(ctx context.Context) {
	if !statusSchedulerRunning.CompareAndSwap(false, true) {
		log.Printf("[StatusSync] Previous job still running, skipping...")
		return
	}
	defer statusSchedulerRunning.Store(false)

	done := make(chan error, 1)
	go func() {
		done <- executeStatusSyncJob(ctx)
	}()

	timer := time.NewTimer(StatusSchedulerJobTimeout)
	defer timer.Stop()

	select {
	case err := <-done:
		if err != nil {
			log.Printf("[StatusSync] Job failed: %v", err)
		}
	case <-timer.C:
		log.Printf("[StatusSync] Job timed out")
	case <-ctx.Done():
		log.Printf("[StatusSync] Job cancelled: %v", ctx.Err())
	}
}
