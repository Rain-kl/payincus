// Port of server/src/workers/restoreTaskWorker.ts.
//
// The restore workflow is the most dangerous of the four queues: it replaces the
// instance's root filesystem. The order is copied verbatim from Node:
//
//	stop original → restore into a temp instance → delete old backups/snapshots →
//	delete original → rename temp → start → rewrite DB rows → mark COMPLETED
//
// The failure path is the part operators rely on: if the original instance cannot
// be confirmed to still exist in Incus, the temp instance is the only copy of the
// data and is deliberately LEFT IN PLACE. The task is failed with progress
// "manual_intervention" so it is unmistakable in the UI.
package workers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	restorePollInterval         = 5 * time.Second
	restoreTimeout              = 10 * time.Minute
	restoreTimeoutCheckInterval = 60 * time.Second
)

// restoreProgressManualIntervention mirrors the TypeScript literal.
const restoreProgressManualIntervention = "manual_intervention"

// restoreTaskStore is the queue seam.
type restoreTaskStore interface {
	ListPendingHostIDs(ctx context.Context) ([]int32, error)
	ClaimNext(ctx context.Context, hostID int32) (*restoreTaskRecord, error)
	GetTask(ctx context.Context, id int32) (*restoreTaskRecord, error)
	UpdateProgress(ctx context.Context, id int32, progress string) error
	MarkCompleted(ctx context.Context, id int32) (bool, error)
	MarkFailed(ctx context.Context, id int32, message string, progress *string) (bool, error)
	CleanupStale(ctx context.Context) (int64, error)
	CleanupTimeout(ctx context.Context, running map[int32]struct{}) (int64, error)
}

// restoreTaskRunner performs one restore. Tests use a fake.
type restoreTaskRunner interface {
	Run(ctx context.Context, task *restoreTaskRecord) error
}

type pgRestoreTaskStore struct{}

func (pgRestoreTaskStore) ListPendingHostIDs(ctx context.Context) ([]int32, error) {
	return newPGStore().ListPendingRestoreHostIDs(ctx)
}

func (pgRestoreTaskStore) ClaimNext(ctx context.Context, hostID int32) (*restoreTaskRecord, error) {
	return newPGStore().ClaimNextRestoreTask(ctx, hostID)
}

func (pgRestoreTaskStore) GetTask(ctx context.Context, id int32) (*restoreTaskRecord, error) {
	return newPGStore().GetRestoreTask(ctx, id)
}

func (pgRestoreTaskStore) UpdateProgress(ctx context.Context, id int32, progress string) error {
	return newPGStore().UpdateRestoreTaskProgress(ctx, id, progress)
}

func (pgRestoreTaskStore) MarkCompleted(ctx context.Context, id int32) (bool, error) {
	return newPGStore().MarkRestoreTaskCompleted(ctx, id)
}

func (pgRestoreTaskStore) MarkFailed(ctx context.Context, id int32, message string, progress *string) (bool, error) {
	return newPGStore().MarkRestoreTaskFailed(ctx, id, message, progress)
}

func (pgRestoreTaskStore) CleanupStale(ctx context.Context) (int64, error) {
	return newPGStore().CleanupStaleRestoreTasks(ctx)
}

func (pgRestoreTaskStore) CleanupTimeout(ctx context.Context, running map[int32]struct{}) (int64, error) {
	return newPGStore().CleanupTimeoutRestoreTasks(ctx, running)
}

// restoreFailure carries the optional progress marker through the runner error.
type restoreFailure struct {
	message  string
	progress *string
}

func (e *restoreFailure) Error() string { return e.message }

type restoreTaskWorker struct {
	store        restoreTaskStore
	runner       restoreTaskRunner
	notifier     backupTaskNotifier
	backoff      *workerDBBackoff
	poll         time.Duration
	timeoutCheck time.Duration
	now          func() time.Time

	mu      sync.Mutex
	running map[int32]struct{}
	wg      sync.WaitGroup
	started bool
}

// stop marks the worker as stopped. The actual shutdown contract is the caller's
// cancel + Wait: the run loop returns on ctx.Done and waits for in-flight tasks
// before StartWorkers' WaitGroup is released.
func (w *restoreTaskWorker) stop() {
	w.mu.Lock()
	w.started = false
	w.mu.Unlock()
}

func newRestoreTaskWorker() *restoreTaskWorker {
	return &restoreTaskWorker{
		store:        pgRestoreTaskStore{},
		runner:       &pgRestoreTaskRunner{},
		notifier:     newPGNotifier(),
		backoff:      newWorkerDBBackoff("RestoreWorker"),
		poll:         restorePollInterval,
		timeoutCheck: restoreTimeoutCheckInterval,
		now:          time.Now,
		running:      map[int32]struct{}{},
	}
}

func (w *restoreTaskWorker) start(ctx context.Context, wg *sync.WaitGroup) {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		log.Printf("[RestoreWorker] Worker 已在运行")
		return
	}
	w.started = true
	w.mu.Unlock()

	log.Printf("[RestoreWorker] 启动恢复任务调度器")

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.run(ctx)
	}()
}

func (w *restoreTaskWorker) run(ctx context.Context) {
	pollTicker := time.NewTicker(w.poll)
	defer pollTicker.Stop()
	timeoutTicker := time.NewTicker(w.timeoutCheck)
	defer timeoutTicker.Stop()

	defer w.wg.Wait()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[RestoreWorker] 恢复任务调度器已停止")
			return
		case <-pollTicker.C:
			w.processQueue(ctx)
		case <-timeoutTicker.C:
			if w.backoff.shouldSkip() {
				continue
			}
			count, err := w.store.CleanupTimeout(ctx, w.runningSnapshot())
			if err != nil {
				log.Printf("[RestoreWorker] 超时检查错误: %v", err)
				w.backoff.arm(err)
				continue
			}
			if count > 0 {
				log.Printf("[RestoreWorker] 清理了 %d 个超时任务", count)
			}
		}
	}
}

func (w *restoreTaskWorker) runningSnapshot() map[int32]struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	snapshot := make(map[int32]struct{}, len(w.running))
	for id := range w.running {
		snapshot[id] = struct{}{}
	}
	return snapshot
}

func (w *restoreTaskWorker) processQueue(ctx context.Context) {
	if w.backoff.shouldSkip() {
		return
	}

	hostIDs, err := w.store.ListPendingHostIDs(ctx)
	if err != nil {
		log.Printf("[RestoreWorker] Queue processing error: %v", err)
		w.backoff.arm(err)
		return
	}

	for _, hostID := range hostIDs {
		if ctx.Err() != nil {
			return
		}
		claimed, err := w.store.ClaimNext(ctx, hostID)
		if err != nil {
			log.Printf("[RestoreWorker] Queue processing error: %v", err)
			w.backoff.arm(err)
			continue
		}
		if claimed == nil {
			continue
		}

		w.wg.Add(1)
		go func(taskID int32) {
			defer w.wg.Done()
			w.execute(ctx, taskID)
		}(claimed.ID)
	}
}

func (w *restoreTaskWorker) execute(ctx context.Context, taskID int32) {
	w.mu.Lock()
	w.running[taskID] = struct{}{}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.running, taskID)
		w.mu.Unlock()
	}()

	task, err := w.store.GetTask(ctx, taskID)
	if err != nil {
		log.Printf("[RestoreWorker] Task %d failed: %v", taskID, err)
		w.backoff.arm(err)
		return
	}
	if task == nil {
		log.Printf("[RestoreWorker] Task %d not found", taskID)
		return
	}

	log.Printf("[RestoreWorker] 开始执行任务 %d", taskID)

	runErr := w.runner.Run(ctx, task)
	if runErr != nil {
		errorMessage := runErr.Error()
		log.Printf("[RestoreWorker] Task %d failed: %s", taskID, errorMessage)

		var progress *string
		var failure *restoreFailure
		if errors.As(runErr, &failure) {
			progress = failure.progress
		}

		failed, storeErr := w.store.MarkFailed(ctx, taskID, errorMessage, progress)
		if storeErr != nil {
			log.Printf("[RestoreWorker] Task %d: 失败状态写入失败: %v", taskID, storeErr)
			w.backoff.arm(storeErr)
			return
		}
		if !failed {
			log.Printf("[RestoreWorker] Task %d: 已不处于 PROCESSING，跳过失败状态覆盖", taskID)
			return
		}
		notifySafely("restore failure log", func() {
			w.notifier.BackupRestoreFailed(ctx, task.UserID, task.InstanceID, task.OriginalInstanceName, errorMessage)
		})
		return
	}

	completed, err := w.store.MarkCompleted(ctx, taskID)
	if err != nil {
		log.Printf("[RestoreWorker] Task %d: 完成状态写入失败: %v", taskID, err)
		w.backoff.arm(err)
		return
	}
	if !completed {
		log.Printf("[RestoreWorker] Task %d: 已不处于 PROCESSING，跳过完成状态覆盖", taskID)
		return
	}

	log.Printf("[RestoreWorker] Task %d: 恢复完成", taskID)

	backupName := "未知"
	if task.BackupID != nil {
		if backup, lookupErr := db.GetBackupByID(ctx, *task.BackupID); lookupErr == nil && backup != nil {
			backupName = backup.Name
		}
	}
	hostName := "未知"
	if host, lookupErr := db.HostsGetByID(ctx, task.HostID); lookupErr == nil && host != nil {
		hostName = host.Name
	}
	notifySafely("restore notification", func() {
		w.notifier.BackupRestored(ctx, task.UserID, task.InstanceID, task.OriginalInstanceName, backupName, hostName)
	})
}

// resolveStoragePoolForExistingInstance mirrors
// db/storage-pools.ts#resolveStoragePoolForExistingInstance: keep the instance's
// current pool when the host still has it, otherwise pick a new system-disk pool
// for the package.
func resolveStoragePoolForExistingInstance(ctx context.Context, instanceID, hostID int32, packageID *int32) (*string, error) {
	instance, err := db.GetInstanceById(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if instance != nil && instance.StoragePoolName != nil && *instance.StoragePoolName != "" {
		exists, hasErr := db.StoragePoolsHasStoragePool(ctx, hostID, *instance.StoragePoolName)
		if hasErr != nil {
			return nil, hasErr
		}
		if exists {
			return instance.StoragePoolName, nil
		}
		return nil, nil
	}
	return db.StoragePoolsResolveNewInstancePool(ctx, hostID, packageID)
}

// pgRestoreTaskRunner is the production restore executor.
type pgRestoreTaskRunner struct{}

func (r *pgRestoreTaskRunner) Run(ctx context.Context, task *restoreTaskRecord) (runErr error) {
	// Mirrors the TypeScript catch block: on any failure, decide whether the temp
	// instance may be deleted. That decision needs Incus, so it lives here (where the
	// client exists) rather than in the queue loop. The returned error carries the
	// "manual_intervention" marker when the temp copy must be kept.
	defer func() {
		if runErr == nil {
			return
		}
		reason := cleanupAfterRestore(ctx, task)
		runErr = wrapRestoreFailure(runErr.Error(), reason)
	}()

	startTime := time.Now()

	host, err := db.HostsGetByID(ctx, task.HostID)
	if err != nil {
		return err
	}
	if host == nil {
		return errors.New("宿主机不存在")
	}
	instance, err := db.GetInstanceById(ctx, task.InstanceID)
	if err != nil {
		return err
	}
	if instance == nil {
		return errors.New("实例不存在")
	}
	if instance.Status == models.InstanceStatusDeleted {
		return errors.New("实例已删除，任务取消")
	}

	var backup *db.BackupsRecord
	if task.BackupID != nil {
		backup, err = db.GetBackupByID(ctx, *task.BackupID)
		if err != nil {
			return err
		}
		if backup == nil {
			return errors.New("备份不存在")
		}
	}

	client, err := lib.GetIncusClientForHost(ctx, host)
	if err != nil {
		return err
	}

	checkTimeout := func() error {
		if time.Since(startTime) > restoreTimeout {
			return errors.New("恢复操作超时 (10 分钟)")
		}
		return nil
	}

	// 3. Stop the original. A stop failure is only a warning in the TypeScript: the
	// replacement path deletes the instance anyway.
	if err := newPGStore().UpdateRestoreTaskProgress(ctx, task.ID, "stopping"); err != nil {
		return err
	}
	log.Printf("[RestoreWorker] Task %d: 停止原实例 %s", task.ID, task.OriginalIncusID)
	if success, collectErr := collectTrafficForRunningInstance(ctx, task.InstanceID); !success {
		log.Printf("[RestoreWorker] Task %d: 恢复前即时采集流量失败: %s", task.ID, collectErr)
	}
	if _, stopErr := lib.IncusInstanceStop(ctx, client, task.OriginalIncusID, true, lib.IncusInstanceBusyRetryOptions{}); stopErr != nil {
		log.Printf("[RestoreWorker] Task %d: 停止实例警告: %v", task.ID, stopErr)
	}
	if err := checkTimeout(); err != nil {
		return err
	}

	// 4. Storage pool of the existing instance.
	pool, err := resolveStoragePoolForExistingInstance(ctx, task.InstanceID, task.HostID, instance.PackageID)
	if err != nil {
		return err
	}
	if pool == nil || *pool == "" {
		return errors.New("宿主机未配置系统盘存储池，无法恢复实例")
	}
	log.Printf("[RestoreWorker] Task %d: 使用存储池 %s", task.ID, *pool)

	// 5. Stream the backup into a temp instance.
	if err := newPGStore().UpdateRestoreTaskProgress(ctx, task.ID, "restoring"); err != nil {
		return err
	}
	if backup == nil {
		return errors.New("备份记录不存在")
	}
	if task.TempInstanceName == nil || *task.TempInstanceName == "" {
		return errors.New("临时实例名称缺失")
	}
	log.Printf("[RestoreWorker] Task %d: 恢复到临时实例 %s", task.ID, *task.TempInstanceName)

	stream, err := lib.GetBackupExportStream(ctx, client, task.OriginalIncusID, backup.IncusName)
	if err != nil {
		return err
	}
	restoreErr := lib.RestoreFromStream(ctx, lib.RestoreFromStreamParams{
		Client:     client,
		Backup:     stream.Body,
		TargetName: *task.TempInstanceName,
		PoolName:   *pool,
	})
	_ = stream.Body.Close()
	if restoreErr != nil {
		return restoreErr
	}
	if err := checkTimeout(); err != nil {
		return err
	}

	// 6. Clean the original's Incus backups and snapshots. Individual failures are
	// warnings: the original is going away, and a failed delete must not abort the
	// restore after the data has been written to the temp instance.
	if err := newPGStore().UpdateRestoreTaskProgress(ctx, task.ID, "cleaning"); err != nil {
		return err
	}
	log.Printf("[RestoreWorker] Task %d: 清理原实例的备份和快照", task.ID)
	if incusBackups, listErr := lib.ListIncusBackups(ctx, client, task.OriginalIncusID); listErr != nil {
		log.Printf("[RestoreWorker] Task %d: 清理备份/快照时出错: %v", task.ID, listErr)
	} else {
		for _, item := range incusBackups {
			if item.Name == "" {
				continue
			}
			if _, deleteErr := lib.DeleteIncusBackup(ctx, client, task.OriginalIncusID, item.Name); deleteErr != nil {
				log.Printf("[RestoreWorker] Task %d: 删除 Incus 备份失败: %v", task.ID, deleteErr)
			}
		}
	}
	if incusSnapshots, listErr := lib.ListSnapshots(ctx, client, task.OriginalIncusID); listErr != nil {
		log.Printf("[RestoreWorker] Task %d: 清理备份/快照时出错: %v", task.ID, listErr)
	} else {
		for _, item := range incusSnapshots {
			if item.Name == "" {
				continue
			}
			if _, deleteErr := lib.DeleteSnapshot(ctx, client, task.OriginalIncusID, item.Name); deleteErr != nil {
				log.Printf("[RestoreWorker] Task %d: 删除 Incus 快照失败: %v", task.ID, deleteErr)
			}
		}
	}

	// 7. Replace the original with the restored copy.
	if err := newPGStore().UpdateRestoreTaskProgress(ctx, task.ID, "replacing"); err != nil {
		return err
	}
	log.Printf("[RestoreWorker] Task %d: 删除原实例 %s", task.ID, task.OriginalIncusID)
	if _, err := lib.IncusInstanceDelete(ctx, client, task.OriginalIncusID); err != nil {
		return err
	}
	if err := checkTimeout(); err != nil {
		return err
	}

	log.Printf("[RestoreWorker] Task %d: 重命名 %s -> %s", task.ID, *task.TempInstanceName, task.OriginalIncusID)
	if err := lib.RenameIncusInstance(ctx, client, *task.TempInstanceName, task.OriginalIncusID); err != nil {
		return err
	}

	if err := deleteTrafficSnapshots(ctx, task.InstanceID); err != nil {
		return err
	}

	// 9. Start the restored instance.
	if err := newPGStore().UpdateRestoreTaskProgress(ctx, task.ID, "starting"); err != nil {
		return err
	}
	log.Printf("[RestoreWorker] Task %d: 启动实例", task.ID)
	if _, err := lib.IncusInstanceStart(ctx, client, task.OriginalIncusID); err != nil {
		return err
	}

	// 10. The old backups/snapshots no longer exist on the restored instance.
	if err := markInstanceBackupsDeleted(ctx, task.InstanceID); err != nil {
		return err
	}
	if err := deleteInstanceSnapshots(ctx, task.InstanceID); err != nil {
		return err
	}

	// 11. Rename + restore provenance, before COMPLETED so a failure here cannot
	// report success.
	backupName := "unknown"
	var backupID *int32
	if backup != nil {
		backupName = backup.Name
		backupID = &backup.ID
	}
	restoredFrom, _ := json.Marshal(map[string]any{
		"backupId":   backupID,
		"backupName": backupName,
		"restoredAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	newName := fmt.Sprintf("%s | restored:%s", task.OriginalInstanceName, backupName)
	if db.DB == nil {
		return errors.New("数据库未初始化")
	}
	result := db.DB.WithContext(ctx).Exec(`UPDATE instances
		SET name = ?, storage_pool_name = ?, restored_from = ?::jsonb, updated_at = NOW()
		WHERE id = ? AND status::text <> 'deleted'`,
		newName, *pool, string(restoredFrom), task.InstanceID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("实例已删除，恢复结果不再写入")
	}

	return nil
}

// cleanupAfterRestore mirrors the TypeScript's catch block. It is exported to the
// runner's failure path via this helper so the "keep the temp instance" rule lives
// in exactly one place.
func cleanupAfterRestore(ctx context.Context, task *restoreTaskRecord) (manualInterventionReason string) {
	if task.TempInstanceName == nil || *task.TempInstanceName == "" {
		return ""
	}

	host, err := db.HostsGetByID(ctx, task.HostID)
	if err != nil || host == nil {
		return fmt.Sprintf("无法确认 Incus 原实例 %s 是否存在，已保留临时实例 %s，需要人工介入核查",
			task.OriginalIncusID, *task.TempInstanceName)
	}
	client, err := lib.GetIncusClientForHost(ctx, host)
	if err != nil {
		return fmt.Sprintf("无法安全确认 Incus 原实例 %s 仍存在，已保留临时实例 %s，需要人工介入核查",
			task.OriginalIncusID, *task.TempInstanceName)
	}

	originalExists := lib.InstanceExists(ctx, client, task.OriginalIncusID)
	tempExists := lib.InstanceExists(ctx, client, *task.TempInstanceName)
	if !originalExists {
		reason := fmt.Sprintf("Incus 原实例 %s 已不存在或无法确认，已保留临时实例 %s，需要人工介入恢复",
			task.OriginalIncusID, *task.TempInstanceName)
		log.Printf("[RestoreWorker] Task %d: %s", task.ID, reason)
		return reason
	}
	if tempExists {
		if _, deleteErr := lib.IncusInstanceDelete(ctx, client, *task.TempInstanceName); deleteErr != nil {
			log.Printf("[RestoreWorker] Task %d: 清理临时实例失败: %v", task.ID, deleteErr)
			return fmt.Sprintf("无法清理临时实例 %s，需要人工介入核查", *task.TempInstanceName)
		}
		log.Printf("[RestoreWorker] Task %d: 已清理临时实例 %s", task.ID, *task.TempInstanceName)
	}
	return ""
}

// wrapRestoreFailure builds the final error text: the original message plus the
// manual-intervention note, exactly like `${errorMessage}; ${reason}`.
func wrapRestoreFailure(message, reason string) *restoreFailure {
	if reason == "" {
		return &restoreFailure{message: message}
	}
	progress := restoreProgressManualIntervention
	return &restoreFailure{
		message:  message + "; " + reason,
		progress: &progress,
	}
}

func deleteTrafficSnapshots(ctx context.Context, instanceID int32) error {
	if db.DB == nil {
		return errors.New("数据库未初始化")
	}
	return db.DB.WithContext(ctx).Exec(`DELETE FROM traffic_snapshots WHERE instance_id = ?`, instanceID).Error
}

func markInstanceBackupsDeleted(ctx context.Context, instanceID int32) error {
	if db.DB == nil {
		return errors.New("数据库未初始化")
	}
	return db.DB.WithContext(ctx).Exec(`UPDATE backups SET status = 'deleted' WHERE instance_id = ?`, instanceID).Error
}

func deleteInstanceSnapshots(ctx context.Context, instanceID int32) error {
	if db.DB == nil {
		return errors.New("数据库未初始化")
	}
	return db.DB.WithContext(ctx).Exec(`DELETE FROM snapshots WHERE instance_id = ?`, instanceID).Error
}

// isNotFoundInstanceError mirrors the two string checks the TS uses after
// deleteInstance.
func isNotFoundInstanceError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "not found") || strings.Contains(message, "Instance not found")
}
