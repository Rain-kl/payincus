// Port of the inline cleanup block at the bottom of server/src/app.ts (lines
// ~870-1050). Node installs these with bare `setInterval`, outside any
// services/*-scheduler module:
//
//	cleanupStaleTransfers        once at startup
//	cleanupTimeoutTransfers      every 5 min
//	runCreateTimeoutCleanup      every 2 min + once at startup
//	runInstanceTaskCleanup       every 24 h + once at startup
//	runInboxCleanup              every 24 h + once at startup (keep 30 days)
//
// Behaviour replicated deliberately:
//
//   - the create sweeper only touches rows still in `creating`; the conditional
//     UPDATE is what protects a live `createInstanceAsync` from being rolled back
//     underneath itself;
//   - an instance with an in-flight operation task (this process's registry) is
//     skipped: the request owns its own deadline and failure cleanup;
//   - the sweeper's log message says "10 minutes" although the timeout is 30. That
//     text is user-visible in the audit log, so it is preserved verbatim;
//   - the refund path is idempotent (`already_refunded`) and uses the purchase
//     record amount, not the current price.
package services

import (
	"context"
	"log"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

const (
	// StartupCleanupTransferInterval is the 5-minute transfer timeout sweep.
	StartupCleanupTransferInterval = 5 * time.Minute
	// StartupCleanupCreateTimeout is CREATE_TIMEOUT_MS (30 minutes).
	StartupCleanupCreateTimeout = 30 * time.Minute
	// StartupCleanupCreateInterval is CREATE_TIMEOUT_CHECK_INTERVAL (2 minutes).
	StartupCleanupCreateInterval = 2 * time.Minute
	// StartupCleanupDailyInterval is the 24h instance-task / inbox period.
	StartupCleanupDailyInterval = 24 * time.Hour
	// StartupCleanupInboxRetentionDays matches cleanupOldMessages(30).
	StartupCleanupInboxRetentionDays = 30
)

// Next-run helpers (pure). The startup passes are separate explicit calls, so
// these only describe the recurring part.

// StartupCleanupTransferNextRun mirrors `setInterval(cleanupTimeoutTransfers, 5m)`.
func StartupCleanupTransferNextRun(now time.Time) time.Time {
	return now.UTC().Add(StartupCleanupTransferInterval)
}

// StartupCleanupCreateNextRun mirrors `setInterval(runCreateTimeoutCleanup, 2m)`.
func StartupCleanupCreateNextRun(now time.Time) time.Time {
	return now.UTC().Add(StartupCleanupCreateInterval)
}

// StartupCleanupDailyNextRun mirrors the two `setInterval(…, 24h)` cleanups.
func StartupCleanupDailyNextRun(now time.Time) time.Time {
	return now.UTC().Add(StartupCleanupDailyInterval)
}

// RunCleanupStaleTransfers mirrors the one-shot startup call. Returns the number of
// rows cleaned so the caller can log it.
func RunCleanupStaleTransfers(ctx context.Context) (int64, error) {
	count, err := db.CleanupStaleTransfers(ctx)
	if err != nil {
		log.Printf("[StartupCleanup] Stale transfer cleanup failed: %v", err)
		return 0, err
	}
	if count > 0 {
		log.Printf("[StartupCleanup] Cleaned %d stuck transfer requests", count)
	}
	return count, nil
}

// RunCleanupTimeoutTransfers mirrors the 5-minute sweep.
func RunCleanupTimeoutTransfers(ctx context.Context) {
	count, err := db.CleanupTimeoutTransfers(ctx)
	if err != nil {
		log.Printf("[StartupCleanup] Transfer timeout cleanup failed: %v", err)
		return
	}
	if count > 0 {
		log.Printf("[StartupCleanup] Cleaned %d timed-out transfer requests", count)
	}
}

// RunCreateTimeoutCleanup mirrors runCreateTimeoutCleanup.
func RunCreateTimeoutCleanup(ctx context.Context) {
	stuck, err := db.GetStuckCreatingInstances(ctx, StartupCleanupCreateTimeout.Milliseconds())
	if err != nil {
		log.Printf("[CreateTimeout] Cleanup failed: %v", err)
		return
	}

	cleaned := 0
	for _, instance := range stuck {
		// The live request owns its operation deadline and failure cleanup; this
		// sweeper only handles orphaned work (e.g. after a process restart).
		operationName := instance.IncusID
		if operationName == "" {
			operationName = instance.Name
		}
		if lib.HasActiveOperationTask(instance.HostID, operationName) {
			continue
		}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Printf("[CreateTimeout] Cleanup failed for instance %d: %v", instance.ID, recovered)
				}
			}()
			if runCreateTimeoutCleanupItem(ctx, instance) {
				cleaned++
			}
		}()
	}
	if cleaned > 0 {
		log.Printf("[CreateTimeout] Cleaned %d timed-out creating instances", cleaned)
	}
}

// runCreateTimeoutCleanupItem mirrors one iteration of the loop. Returns true when
// the row was claimed and compensated.
func runCreateTimeoutCleanupItem(ctx context.Context, instance db.InstanceStuckCreatingRecord) bool {
	log.Printf("[CreateTimeout] Cleaning timed-out creating instance: %s (ID: %d)", instance.Name, instance.ID)

	claimed, err := db.ClaimCreatingInstanceAsError(ctx, instance.ID)
	if err != nil {
		log.Printf("[CreateTimeout] Failed to mark instance %d as error: %v", instance.ID, err)
		return false
	}
	if !claimed {
		// createInstanceAsync's failure rollback moved the row first.
		log.Printf("[CreateTimeout] Instance %s status already changed by another process, skipping", instance.Name)
		return false
	}

	operationName := instance.IncusID
	if operationName == "" {
		operationName = instance.Name
	}
	if err := db.FailRunningCreateOperationTasks(ctx, instance.HostID, operationName,
		"实例创建超过 30 分钟，触发超时清理；请核对节点状态"); err != nil {
		log.Printf("[CreateTimeout] Failed to update operation task: %v", err)
	}

	portCount := int32(0)
	if lib.NetworkModeAllowsPortMapping(instance.NetworkMode) && instance.PortLimit != nil {
		portCount = *instance.PortLimit
	}
	if err := db.RollbackResources(ctx, db.RollbackResourcesOptions{
		HostID:    instance.HostID,
		CPU:       instance.CPU,
		Memory:    instance.Memory,
		Disk:      instance.Disk,
		PortCount: portCount,
	}); err != nil {
		log.Printf("[CreateTimeout] Resource rollback failed: %v", err)
	} else {
		log.Printf("[CreateTimeout] Instance %s resources rolled back", instance.Name)
	}

	if lib.NetworkModeNeedsPublicIpv4(instance.NetworkMode) {
		if err := releaseInstancePublicIPv4(ctx, instance.ID); err != nil {
			log.Printf("[CreateTimeout] Failed to release dedicated IPv4: %v", err)
		} else {
			log.Printf("[CreateTimeout] Instance %s dedicated IPv4 released", instance.Name)
		}
	}

	compensation, compErr := db.CompensateFailedInstancePurchase(ctx, instance.ID, instance.UserID, instance.HostID)
	if compErr != nil {
		log.Printf("[CreateTimeout] Accounting compensation failed: %v", compErr)
	} else if compensation.Refunded {
		log.Printf("[CreateTimeout] Instance %s creation timeout refunded ¥%.2f", instance.Name, compensation.RefundAmount)
	} else if compensation.Reason != "not_paid_purchase" {
		log.Printf("[CreateTimeout] Instance %s creation timeout needs no refund: %s", instance.Name, compensation.Reason)
	}

	// Best effort: the container may not exist (the create never reached Incus).
	if host, hostErr := db.HostsGetByID(ctx, instance.HostID); hostErr != nil {
		log.Printf("[CreateTimeout] Failed to load host for residual cleanup: %v", hostErr)
	} else if host != nil {
		if client, clientErr := lib.GetIncusClientForHost(ctx, host); clientErr != nil {
			log.Printf("[CreateTimeout] Residual container cleanup skipped: %v", clientErr)
		} else if _, deleteErr := lib.IncusInstanceDelete(ctx, client, instance.IncusID); deleteErr != nil {
			log.Printf("[CreateTimeout] Residual container cleanup failed (may not exist)")
		} else {
			log.Printf("[CreateTimeout] Residual container %s cleaned", instance.IncusID)
		}
	}

	hostName := ""
	if host, hostErr := db.HostsGetByID(ctx, instance.HostID); hostErr == nil && host != nil {
		hostName = host.Name
	}
	data := lib.EventData{"instanceName": instance.Name}
	if hostName != "" {
		data["hostName"] = hostName
	}
	lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceCreateTimeout, data)

	// Verbatim "10 minutes": see the file comment.
	db.CreateLog(ctx, int64Pointer(int64(instance.UserID)), "instance", "instance.create_timeout",
		"Instance \""+instance.Name+"\" creation timed out after 10 minutes", "failed",
		db.LogOptionsWithInstance(&instance.ID))

	return true
}

// releaseInstancePublicIPv4 mirrors releaseReservedPublicIpv4ForFailedCreation:
// the pgx release runs in its own transaction.
func releaseInstancePublicIPv4(ctx context.Context, instanceID int32) error {
	if db.Pool() == nil {
		return nil
	}
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := db.ReleasePublicIpv4ForInstance(ctx, tx, instanceID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RunInstanceTaskCleanup mirrors runInstanceTaskCleanup.
func RunInstanceTaskCleanup(ctx context.Context) {
	deleted, err := db.CleanupOldTasks(ctx)
	if err != nil {
		log.Printf("[StartupCleanup] Instance task cleanup failed: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("[StartupCleanup] Instance task cleanup removed %d expired tasks", deleted)
	}
}

// RunInboxCleanup mirrors runInboxCleanup.
func RunInboxCleanup(ctx context.Context) {
	deleted, err := db.CleanupOldMessages(ctx, StartupCleanupInboxRetentionDays)
	if err != nil {
		log.Printf("[StartupCleanup] Inbox cleanup failed: %v", err)
		return
	}
	if deleted > 0 {
		log.Printf("[StartupCleanup] Inbox cleanup removed %d old messages", deleted)
	}
}

// startStartupCleanup wires the four inline cleanups.
//
// The startup passes run as a single job so their failures are isolated from the
// recurring ticks and so `stop()` can wait for them like any other job.
func startStartupCleanup(group *schedulerGroup) {
	group.spawn("StartupCleanupInitial", func(ctx context.Context) error {
		if _, err := RunCleanupStaleTransfers(ctx); err != nil {
			// The TypeScript ignores this failure and keeps booting.
			log.Printf("[StartupCleanup] Continuing despite stale transfer cleanup failure")
		}
		RunCreateTimeoutCleanup(ctx)
		RunInstanceTaskCleanup(ctx)
		RunInboxCleanup(ctx)
		return nil
	}, nil)

	group.startIntervalLoop("TransferTimeoutCleanup", StartupCleanupTransferInterval, false, func(ctx context.Context) error {
		RunCleanupTimeoutTransfers(ctx)
		return nil
	}, nil)
	group.startIntervalLoop("CreateTimeoutCleanup", StartupCleanupCreateInterval, false, func(ctx context.Context) error {
		RunCreateTimeoutCleanup(ctx)
		return nil
	}, nil)
	group.startIntervalLoop("InstanceTaskCleanup", StartupCleanupDailyInterval, false, func(ctx context.Context) error {
		RunInstanceTaskCleanup(ctx)
		return nil
	}, nil)
	group.startIntervalLoop("InboxCleanup", StartupCleanupDailyInterval, false, func(ctx context.Context) error {
		RunInboxCleanup(ctx)
		return nil
	}, nil)
}
