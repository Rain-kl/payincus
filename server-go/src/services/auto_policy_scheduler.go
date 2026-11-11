// Port of server/src/services/auto-policy-scheduler.ts.
//
// Every 5 minutes the scheduler executes the due snapshot policies:
//
//   - concurrency 5 for the whole job (the TypeScript creates one p-limit at module
//     scope, so it is NOT per-host — a fleet with two busy hosts still runs five
//     snapshots at a time);
//   - per-instance distributed lock `auto-policy:snapshot:<id>` (30 min expiry, 100ms
//     wait), so a slow snapshot cannot be started twice;
//   - 3 attempts with a linearly increasing delay (5s, 10s);
//   - quota rotation: when the instance already holds `snapshotLimit` snapshots the
//     oldest `auto-*` row is deleted first, falling back to the oldest manual one;
//   - the policy's `next_run_at` is advanced on success AND after all retries fail,
//     so a permanently broken instance cannot spin forever (the failure is reported
//     by notification + log instead).
//
// The snapshot name is derived from UTC (`toISOString()` in the TypeScript), which
// keeps names identical between a UTC+8 dev box and the UTC production container.
package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// AutoPolicySchedulerConcurrency is the module-level pLimit(5).
	AutoPolicySchedulerConcurrency = 5
	// AutoPolicySchedulerMaxRetries is MAX_RETRIES.
	AutoPolicySchedulerMaxRetries = 3
	// AutoPolicySchedulerRetryDelay is RETRY_DELAY (attempt N waits N * delay).
	AutoPolicySchedulerRetryDelay = 5 * time.Second
	// AutoPolicyLockExpireMs is AUTO_POLICY_LOCK_EXPIRE_MS (30 minutes).
	AutoPolicyLockExpireMs = 30 * 60 * 1000
	// AutoPolicyLockWaitMs is AUTO_POLICY_LOCK_WAIT_MS.
	AutoPolicyLockWaitMs = 100
	// AutoPolicyLockRetryMs is the retry interval passed to acquireLock.
	AutoPolicyLockRetryMs = 50
)

// autoPolicyDelay is a seam: retry tests must not sleep 15 seconds.
var autoPolicyDelay = func(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// AutoPolicySchedulerNextRun mirrors `schedule('*/5 * * * *')`.
func AutoPolicySchedulerNextRun(now time.Time) time.Time {
	return schedulerEveryNMinutesNextRun(now, 5)
}

// AutoPolicyLockKey mirrors getAutoSnapshotPolicyLockKey.
func AutoPolicyLockKey(instanceID int32) string {
	return fmt.Sprintf("auto-policy:snapshot:%d", instanceID)
}

// AutoPolicySnapshotName mirrors generateName: `<prefix>-YYYYMMDD-HHMMSS` in UTC.
//
// The TypeScript slices the mangled ISO string to 15 characters, which drops the
// milliseconds. Same layout, same truncation.
func AutoPolicySnapshotName(prefix string, now time.Time) string {
	return prefix + "-" + now.UTC().Format("20060102-150405")
}

// AutoPolicyInstanceEligible mirrors the shared guard: the instance must still be
// running or stopped and its host must be online.
func AutoPolicyInstanceEligible(status models.InstanceStatus, hostStatus models.HostStatus) bool {
	if hostStatus != models.HostStatusOnline {
		return false
	}
	return status == models.InstanceStatusRunning || status == models.InstanceStatusStopped
}

// AutoPolicyPolicyDue mirrors `nextRunAt === null || nextRunAt > now` (the
// scheduler skips policies whose next run moved into the future).
func AutoPolicyPolicyDue(enabled bool, nextRunAt *time.Time, now time.Time) bool {
	if !enabled {
		return false
	}
	return nextRunAt == nil || !nextRunAt.After(now)
}

// autoPolicyGetClient mirrors getIncusClient: both certificate paths are required.
func autoPolicyGetClient(ctx context.Context, host db.SnapshotPolicyHost) *lib.IncusClient {
	if host.CertPath == nil || *host.CertPath == "" || host.KeyPath == nil || *host.KeyPath == "" {
		return nil
	}
	client, err := lib.GetIncusClient(ctx, lib.IncusClientOptions{
		URL:      host.URL,
		CertPath: host.CertPath,
		KeyPath:  host.KeyPath,
		HostID:   int64(host.ID),
	})
	if err != nil {
		log.Printf("[AutoPolicy] Failed to connect to host: %v", err)
		return nil
	}
	return client
}

// executeAutoSnapshot mirrors executeAutoSnapshot. It never returns an error: the
// TypeScript logs every failure path and the job must continue with other
// instances.
func executeAutoSnapshot(ctx context.Context, policy db.SnapshotPolicyDue) {
	instance := policy.Instance
	snapshotLimit := instance.SnapshotLimit
	if snapshotLimit == nil || *snapshotLimit <= 0 {
		log.Printf("[AutoPolicy] Instance %d has no snapshot quota, skipping", instance.ID)
		return
	}

	lastError := ""
	for attempt := 1; attempt <= AutoPolicySchedulerMaxRetries; attempt++ {
		if err := autoPolicyAttemptSnapshot(ctx, policy); err != nil {
			lastError = err.Error()
			log.Printf("[AutoPolicy] Auto snapshot failed for instance %d (attempt %d): %s", instance.ID, attempt, lastError)
			if attempt < AutoPolicySchedulerMaxRetries {
				autoPolicyDelay(ctx, AutoPolicySchedulerRetryDelay*time.Duration(attempt))
			}
			continue
		}
		return
	}

	log.Printf("[AutoPolicy] Auto snapshot failed after %d attempts for instance %d", AutoPolicySchedulerMaxRetries, instance.ID)
	if err := db.SnapshotsUpdatePolicyLastRun(ctx, instance.ID); err != nil {
		log.Printf("[AutoPolicy] Failed to advance policy for instance %d: %v", instance.ID, err)
	}
	lib.SendNotification(ctx, int64(instance.UserID), lib.EventSnapshotFailed, lib.EventData{
		"instanceName": instance.Name,
		"error":        lastError,
		"hostName":     instance.Host.Name,
	})
	db.CreateLog(ctx, int64FromInt32(instance.UserID), "snapshot", "snapshot.auto_create",
		fmt.Sprintf("Auto snapshot failed for instance \"%s\": %s", instance.Name, lastError),
		"failed", db.LogOptionsWithInstance(&instance.ID))
}

// autoPolicyAttemptSnapshot is one attempt of executeAutoSnapshot.
func autoPolicyAttemptSnapshot(ctx context.Context, policy db.SnapshotPolicyDue) error {
	instance := policy.Instance
	snapshotLimit := *instance.SnapshotLimit
	log.Printf("[AutoPolicy] Creating auto snapshot for instance %d", instance.ID)

	client := autoPolicyGetClient(ctx, instance.Host)
	if client == nil {
		return errors.New("Failed to connect to host")
	}

	snapshotName := AutoPolicySnapshotName("auto", time.Now().UTC())

	currentSnapshots, err := db.SnapshotsListByInstanceIDAsc(ctx, instance.ID)
	if err != nil {
		return err
	}

	deletedSnapshotName := ""
	if int32(len(currentSnapshots)) >= snapshotLimit {
		// Prefer rotating an automatic snapshot; fall back to the oldest manual one.
		var target *db.SnapshotsSnapshot
		for index := range currentSnapshots {
			if len(currentSnapshots[index].Name) >= 5 && currentSnapshots[index].Name[:5] == "auto-" {
				target = &currentSnapshots[index]
				break
			}
		}
		if target == nil && len(currentSnapshots) > 0 {
			target = &currentSnapshots[0]
		}
		if target != nil {
			if _, err := lib.DeleteSnapshot(ctx, client, instance.IncusID, target.IncusName); err != nil {
				log.Printf("[AutoPolicy] Failed to delete old snapshot: %v", err)
				return fmt.Errorf("failed to delete old snapshot: %s", target.Name)
			}
			if err := db.SnapshotsDelete(ctx, target.ID); err != nil {
				log.Printf("[AutoPolicy] Failed to delete old snapshot row: %v", err)
				return fmt.Errorf("failed to delete old snapshot: %s", target.Name)
			}
			deletedSnapshotName = target.Name
			log.Printf("[AutoPolicy] Deleted old snapshot to make room: %s", target.Name)
		}
	}

	if _, err := lib.CreateSnapshot(ctx, client, instance.IncusID, snapshotName); err != nil {
		return err
	}

	description := "自动快照"
	if _, err := db.SnapshotsCreate(ctx, db.SnapshotsCreateData{
		InstanceID:  instance.ID,
		IncusName:   snapshotName,
		Name:        snapshotName,
		Description: &description,
		Stateful:    false,
	}); err != nil {
		return err
	}

	if err := db.SnapshotsUpdatePolicyLastRun(ctx, instance.ID); err != nil {
		log.Printf("[AutoPolicy] Failed to advance policy for instance %d: %v", instance.ID, err)
	}

	if deletedSnapshotName != "" {
		lib.SendNotification(ctx, int64(instance.UserID), lib.EventAutoSnapshotRotated, lib.EventData{
			"instanceName": instance.Name,
			"snapshotName": snapshotName,
			"deletedName":  deletedSnapshotName,
			"quotaLimit":   snapshotLimit,
			"hostName":     instance.Host.Name,
		})
	} else {
		lib.SendNotification(ctx, int64(instance.UserID), lib.EventAutoSnapshot, lib.EventData{
			"instanceName": instance.Name,
			"snapshotName": snapshotName,
			"hostName":     instance.Host.Name,
		})
	}

	content := fmt.Sprintf("Auto snapshot \"%s\" created for instance \"%s\"", snapshotName, instance.Name)
	if deletedSnapshotName != "" {
		content += fmt.Sprintf(" (deleted old: %s)", deletedSnapshotName)
	}
	db.CreateLog(ctx, int64FromInt32(instance.UserID), "snapshot", "snapshot.auto_create",
		content, "success", db.LogOptionsWithInstance(&instance.ID))

	log.Printf("[AutoPolicy] Auto snapshot created for instance %d: %s", instance.ID, snapshotName)
	return nil
}

// executeLockedAutoSnapshot mirrors executeLockedAutoSnapshot.
func executeLockedAutoSnapshot(ctx context.Context, candidate db.SnapshotPolicyDue) {
	instanceID := candidate.InstanceID
	lockKey := AutoPolicyLockKey(instanceID)
	lockResult, err := lib.AcquireLock(ctx, lockKey, lib.LockOptions{
		ExpireMs:        intPointer(AutoPolicyLockExpireMs),
		WaitTimeoutMs:   intPointer(AutoPolicyLockWaitMs),
		RetryIntervalMs: intPointer(AutoPolicyLockRetryMs),
	})
	if err != nil || !lockResult.Success || lockResult.OwnerID == "" {
		log.Printf("[AutoPolicy] Skip auto snapshot for instance %d: lock already held", instanceID)
		return
	}
	defer lib.ReleaseLock(ctx, lockKey, lockResult.OwnerID)

	now := time.Now().UTC()
	policy, err := db.SnapshotsGetPolicyWithInstance(ctx, instanceID)
	if err != nil {
		log.Printf("[AutoPolicy] Skip auto snapshot for instance %d: %v", instanceID, err)
		return
	}
	if policy == nil || !AutoPolicyPolicyDue(policy.Enabled, policy.NextRunAt, now) {
		log.Printf("[AutoPolicy] Skip auto snapshot for instance %d: policy no longer due", instanceID)
		return
	}
	if !AutoPolicyInstanceEligible(policy.Instance.Status, policy.Instance.Host.Status) {
		log.Printf("[AutoPolicy] Skip auto snapshot for instance %d: instance or host no longer eligible", instanceID)
		return
	}

	executeAutoSnapshot(ctx, *policy)
}

// RunAutoSnapshotJob mirrors runAutoSnapshotJob.
func RunAutoSnapshotJob(ctx context.Context) {
	log.Printf("[AutoPolicy] Starting auto snapshot job...")
	start := time.Now()

	now := time.Now().UTC()
	candidates, err := db.SnapshotsListDuePolicies(ctx, now)
	if err != nil {
		log.Printf("[AutoPolicy] Auto snapshot job failed: %v", err)
		return
	}

	valid := make([]db.SnapshotPolicyDue, 0, len(candidates))
	for _, policy := range candidates {
		if AutoPolicyInstanceEligible(policy.Instance.Status, policy.Instance.Host.Status) {
			valid = append(valid, policy)
		}
	}
	log.Printf("[AutoPolicy] Found %d snapshot policies to execute", len(valid))

	semaphore := make(chan struct{}, AutoPolicySchedulerConcurrency)
	var wait sync.WaitGroup
	for _, policy := range valid {
		wait.Add(1)
		go func(policy db.SnapshotPolicyDue) {
			defer wait.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			runIsolated("AutoPolicy", func() { executeLockedAutoSnapshot(ctx, policy) })
		}(policy)
	}
	wait.Wait()

	log.Printf("[AutoPolicy] Auto snapshot job completed in %dms", time.Since(start).Milliseconds())
}

// startAutoPolicyScheduler wires the 5-minute cron entry.
func startAutoPolicyScheduler(group *schedulerGroup) {
	group.startLoop("AutoPolicy", AutoPolicySchedulerNextRun, func(ctx context.Context) error {
		RunAutoSnapshotJob(ctx)
		return nil
	}, nil)
}

func int64Pointer(value int64) *int64 { return &value }
