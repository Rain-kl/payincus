// Port of server/src/workers/backupUploadWorker.ts.
//
// One host runs at most one high-IO job: a backup upload competes with restore
// tasks and every instance task for the host. The claim transaction checks all
// three tables before it flips a PENDING row to PROCESSING (see store.go).
//
// Retry policy is deliberately narrow: only transport-looking errors are retried
// (timeout / ECONNRESET / socket hang up / network / connection), at most
// MAX_RETRY_COUNT times. A rejected credential or a missing storage config fails
// the task immediately; retrying those would just burn quota.
//
// The 30-minute budget is enforced twice: a runtime timeout sweep fails rows whose
// started_at is older than 30 minutes (excluding ids this process is still
// running), and the runner re-checks the elapsed time at the start/end of the
// stream. A stuck TCP stream cannot hold the queue forever.
package workers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	uploadPollInterval         = 5 * time.Second
	uploadTimeout              = 30 * time.Minute
	uploadMaxRetryCount        = 2
	uploadRetryDelay           = 30 * time.Second
	uploadTimeoutCheckInterval = 60 * time.Second
	uploadRemoteNameTimeLayout = "20060102-150405"
)

// StorageProvider mirrors the subset of IStorageProvider the upload worker uses.
// The implementation belongs to the storage adapters (S3/WebDAV/FTP/SFTP), which
// are not part of this wave; production must register a factory via
// SetStorageProviderFactory before uploads can succeed.
type StorageProvider interface {
	UploadStream(ctx context.Context, reader io.Reader, filename string) error
	DeleteFile(ctx context.Context, filename string) error
}

// ErrStorageProviderNotRegistered is returned by the default factory. Failing
// closed is intentional: a backup must never be silently skipped because a
// provider is missing.
var ErrStorageProviderNotRegistered = errors.New("存储适配器尚未注册（Go 迁移中）")

var (
	storageProviderFactoryMu sync.RWMutex
	storageProviderFactory   = func(config *models.StorageConfig) (StorageProvider, error) {
		return nil, ErrStorageProviderNotRegistered
	}
)

// SetStorageProviderFactory installs the storage adapter factory used by the
// upload worker. It exists so the main program (or a test) can wire the real
// S3/WebDAV/FTP/SFTP providers without this package importing them.
func SetStorageProviderFactory(factory func(config *models.StorageConfig) (StorageProvider, error)) {
	storageProviderFactoryMu.Lock()
	defer storageProviderFactoryMu.Unlock()
	if factory == nil {
		storageProviderFactory = func(config *models.StorageConfig) (StorageProvider, error) {
			return nil, ErrStorageProviderNotRegistered
		}
		return
	}
	storageProviderFactory = factory
}

func currentStorageProviderFactory() func(*models.StorageConfig) (StorageProvider, error) {
	storageProviderFactoryMu.RLock()
	defer storageProviderFactoryMu.RUnlock()
	return storageProviderFactory
}

// uploadTaskStore is the queue seam.
type uploadTaskStore interface {
	ListPendingHostIDs(ctx context.Context) ([]int32, error)
	ClaimNext(ctx context.Context, hostID int32) (*uploadTaskRecord, error)
	GetTask(ctx context.Context, id int32) (*uploadTaskRecord, error)
	UpdateProgress(ctx context.Context, id int32, progress string) error
	MarkCompleted(ctx context.Context, id int32, remoteFileName string, fileSize *int64) (bool, error)
	Requeue(ctx context.Context, id int32) (bool, error)
	MarkFailed(ctx context.Context, id int32, message string) (bool, error)
	CleanupStale(ctx context.Context) (int64, error)
	CleanupTimeout(ctx context.Context, running map[int32]struct{}) (int64, error)
}

// uploadTaskRunner performs one upload. Tests replace it with a fake so no Incus
// or storage endpoint is ever contacted.
type uploadTaskRunner interface {
	Run(ctx context.Context, task *uploadTaskRecord) (remoteFileName string, fileSize *int64, err error)
}

type pgUploadTaskStore struct{}

func (pgUploadTaskStore) ListPendingHostIDs(ctx context.Context) ([]int32, error) {
	return newPGStore().ListPendingUploadHostIDs(ctx)
}

func (pgUploadTaskStore) ClaimNext(ctx context.Context, hostID int32) (*uploadTaskRecord, error) {
	return newPGStore().ClaimNextUploadTask(ctx, hostID)
}

func (pgUploadTaskStore) GetTask(ctx context.Context, id int32) (*uploadTaskRecord, error) {
	return newPGStore().GetUploadTask(ctx, id)
}

func (pgUploadTaskStore) UpdateProgress(ctx context.Context, id int32, progress string) error {
	return newPGStore().UpdateUploadTaskProgress(ctx, id, progress)
}

func (pgUploadTaskStore) MarkCompleted(ctx context.Context, id int32, remoteFileName string, fileSize *int64) (bool, error) {
	return newPGStore().MarkUploadTaskCompleted(ctx, id, remoteFileName, fileSize)
}

func (pgUploadTaskStore) Requeue(ctx context.Context, id int32) (bool, error) {
	return newPGStore().RequeueUploadTask(ctx, id)
}

func (pgUploadTaskStore) MarkFailed(ctx context.Context, id int32, message string) (bool, error) {
	return newPGStore().MarkUploadTaskFailed(ctx, id, message)
}

func (pgUploadTaskStore) CleanupStale(ctx context.Context) (int64, error) {
	return newPGStore().CleanupStaleUploadTasks(ctx)
}

func (pgUploadTaskStore) CleanupTimeout(ctx context.Context, running map[int32]struct{}) (int64, error) {
	return newPGStore().CleanupTimeoutUploadTasks(ctx, running)
}

// backupUploadWorker mirrors the module-level state of the TypeScript: two
// interval handles plus the runningUploadTaskIds set that protects live tasks from
// the timeout sweep.
type backupUploadWorker struct {
	store        uploadTaskStore
	runner       uploadTaskRunner
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
func (w *backupUploadWorker) stop() {
	w.mu.Lock()
	w.started = false
	w.mu.Unlock()
}

func newBackupUploadWorker() *backupUploadWorker {
	return &backupUploadWorker{
		store:        pgUploadTaskStore{},
		runner:       &pgUploadTaskRunner{},
		notifier:     newPGNotifier(),
		backoff:      newWorkerDBBackoff("UploadWorker"),
		poll:         uploadPollInterval,
		timeoutCheck: uploadTimeoutCheckInterval,
		now:          time.Now,
		running:      map[int32]struct{}{},
	}
}

func (w *backupUploadWorker) start(ctx context.Context, wg *sync.WaitGroup) {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		log.Printf("[UploadWorker] Worker 已在运行")
		return
	}
	w.started = true
	w.mu.Unlock()

	log.Printf("[UploadWorker] 启动备份上传调度器")

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.run(ctx)
	}()
}

func (w *backupUploadWorker) run(ctx context.Context) {
	pollTicker := time.NewTicker(w.poll)
	defer pollTicker.Stop()
	timeoutTicker := time.NewTicker(w.timeoutCheck)
	defer timeoutTicker.Stop()

	// Every task launched by processQueue is registered on w.wg. On shutdown we
	// stop claiming, cancel ctx, and wait for those executions to unwind.
	defer w.wg.Wait()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[UploadWorker] 备份上传调度器已停止")
			return
		case <-pollTicker.C:
			w.processQueue(ctx)
		case <-timeoutTicker.C:
			if w.backoff.shouldSkip() {
				continue
			}
			count, err := w.store.CleanupTimeout(ctx, w.runningSnapshot())
			if err != nil {
				log.Printf("[UploadWorker] 超时检查错误: %v", err)
				w.backoff.arm(err)
				continue
			}
			if count > 0 {
				log.Printf("[UploadWorker] 清理了 %d 个超时任务", count)
			}
		}
	}
}

func (w *backupUploadWorker) runningSnapshot() map[int32]struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	snapshot := make(map[int32]struct{}, len(w.running))
	for id := range w.running {
		snapshot[id] = struct{}{}
	}
	return snapshot
}

// processQueue mirrors processQueue: group by host, claim at most one task per
// host, and execute it asynchronously so one slow upload does not block the other
// hosts (the TypeScript does not await executeUploadTask either).
func (w *backupUploadWorker) processQueue(ctx context.Context) {
	if w.backoff.shouldSkip() {
		return
	}

	hostIDs, err := w.store.ListPendingHostIDs(ctx)
	if err != nil {
		log.Printf("[UploadWorker] Queue processing error: %v", err)
		w.backoff.arm(err)
		return
	}

	for _, hostID := range hostIDs {
		if ctx.Err() != nil {
			return
		}
		claimed, err := w.store.ClaimNext(ctx, hostID)
		if err != nil {
			log.Printf("[UploadWorker] Queue processing error: %v", err)
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

// execute mirrors executeUploadTask. The task row is re-read because the claim
// row only carries the queue projection (and, importantly, the retry counter the
// retry decision is based on).
func (w *backupUploadWorker) execute(ctx context.Context, taskID int32) {
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
		log.Printf("[UploadWorker] Task %d failed: %v", taskID, err)
		w.backoff.arm(err)
		return
	}
	if task == nil {
		log.Printf("[UploadWorker] Task %d not found", taskID)
		return
	}

	log.Printf("[UploadWorker] 开始执行任务 %d", taskID)

	remoteFileName, fileSize, runErr := w.runner.Run(ctx, task)
	if runErr != nil {
		errorMessage := runErr.Error()
		log.Printf("[UploadWorker] Task %d failed: %s", taskID, errorMessage)

		canRetry := isRetryableUploadError(runErr) && task.RetryCount < uploadMaxRetryCount
		if canRetry {
			log.Printf("[UploadWorker] Task %d: 可重试错误，将在 %d 秒后重试 (第 %d/%d 次)",
				taskID, int(uploadRetryDelay/time.Second), task.RetryCount+1, uploadMaxRetryCount)
			requeued, storeErr := w.store.Requeue(ctx, taskID)
			if storeErr != nil {
				log.Printf("[UploadWorker] Task %d: 重试状态写入失败: %v", taskID, storeErr)
				w.backoff.arm(storeErr)
				return
			}
			if !requeued {
				log.Printf("[UploadWorker] Task %d: 已不处于 PROCESSING，跳过重试状态覆盖", taskID)
				return
			}
			notifySafely("upload retry log", func() {
				w.notifier.BackupUploadRetry(ctx, task.UserID, task.InstanceID, task.RetryCount+1, uploadMaxRetryCount, errorMessage)
			})
			return
		}

		failed, storeErr := w.store.MarkFailed(ctx, taskID, errorMessage)
		if storeErr != nil {
			log.Printf("[UploadWorker] Task %d: 失败状态写入失败: %v", taskID, storeErr)
			w.backoff.arm(storeErr)
			return
		}
		if !failed {
			log.Printf("[UploadWorker] Task %d: 已不处于 PROCESSING，跳过失败状态覆盖", taskID)
			return
		}
		notifySafely("upload failure log", func() {
			w.notifier.BackupUploadFailed(ctx, task.UserID, task.InstanceID, errorMessage)
		})
		return
	}

	completed, err := w.store.MarkCompleted(ctx, taskID, remoteFileName, fileSize)
	if err != nil {
		log.Printf("[UploadWorker] Task %d: 完成状态写入失败: %v", taskID, err)
		w.backoff.arm(err)
		return
	}
	if !completed {
		log.Printf("[UploadWorker] Task %d: 已不处于 PROCESSING，跳过完成状态覆盖", taskID)
		return
	}

	log.Printf("[UploadWorker] Task %d: 上传完成", taskID)

	// The notification needs names, not ids. Read them only after the task is
	// safely COMPLETED so a lookup failure cannot fail a successful upload.
	instanceName, backupName, hostName, storageName := w.uploadNames(ctx, task)
	notifySafely("upload notification", func() {
		w.notifier.BackupUploaded(ctx, task.UserID, task.InstanceID, instanceName, backupName, storageName, remoteFileName, hostName)
	})
}

func (w *backupUploadWorker) uploadNames(ctx context.Context, task *uploadTaskRecord) (instanceName, backupName, hostName, storageName string) {
	instanceName = fmt.Sprintf("实例 #%d", task.InstanceID)
	backupName = fmt.Sprintf("备份 #%d", task.BackupID)
	hostName = "未知"
	storageName = fmt.Sprintf("存储 #%d", task.StorageConfigID)

	if instance, err := db.GetInstanceById(ctx, task.InstanceID); err == nil && instance != nil {
		instanceName = instance.Name
	}
	if backup, err := db.GetBackupByID(ctx, task.BackupID); err == nil && backup != nil {
		backupName = backup.Name
	}
	if host, err := db.HostsGetByID(ctx, task.HostID); err == nil && host != nil {
		hostName = host.Name
	}
	var config models.StorageConfig
	if db.DB != nil {
		if err := db.DB.WithContext(ctx).Select("name").First(&config, "id = ?", task.StorageConfigID).Error; err == nil {
			storageName = config.Name
		}
	}
	return instanceName, backupName, hostName, storageName
}

// isRetryableUploadError mirrors isRetryableError.
func isRetryableUploadError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, needle := range []string{
		"timeout", "econnreset", "econnrefused", "enotfound",
		"socket hang up", "network", "connection",
	} {
		if strings.Contains(message, needle) {
			return true
		}
	}
	return false
}

// pgUploadTaskRunner is the production runner. Everything in it is fail-closed:
// it validates the row set before touching Incus, updates progress before each
// stage, and attempts remote cleanup exactly once when the stream fails after a
// file name was chosen.
type pgUploadTaskRunner struct{}

var uploadRemoteNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9\-_.]`)

func (r *pgUploadTaskRunner) Run(ctx context.Context, task *uploadTaskRecord) (remoteFileName string, fileSize *int64, runErr error) {
	startTime := time.Now()

	// Any failure after the provider and the remote name exist removes the partial
	// archive, mirroring the TypeScript catch block (which covers progress/DB errors
	// too, not just stream errors). Cleanup errors are logged, never surfaced.
	var providerForCleanup StorageProvider
	defer func() {
		if runErr == nil || providerForCleanup == nil || remoteFileName == "" {
			return
		}
		notifySafely("upload remote cleanup", func() {
			if err := providerForCleanup.DeleteFile(context.WithoutCancel(ctx), remoteFileName); err != nil {
				log.Printf("[UploadWorker] Task %d: 清理远程文件失败: %v", task.ID, err)
			} else {
				log.Printf("[UploadWorker] Task %d: 已清理远程文件 %s", task.ID, remoteFileName)
			}
		})
	}()

	instance, err := db.GetInstanceById(ctx, task.InstanceID)
	if err != nil {
		return "", nil, err
	}
	if instance == nil {
		return "", nil, errors.New("实例不存在")
	}
	if instance.Status == models.InstanceStatusDeleted {
		return "", nil, errors.New("实例已删除，任务取消")
	}
	backup, err := db.GetBackupByID(ctx, task.BackupID)
	if err != nil {
		return "", nil, err
	}
	if backup == nil {
		return "", nil, errors.New("备份不存在")
	}
	host, err := db.HostsGetByID(ctx, task.HostID)
	if err != nil {
		return "", nil, err
	}
	if host == nil {
		return "", nil, errors.New("宿主机不存在")
	}

	var storageConfig models.StorageConfig
	if db.DB == nil {
		return "", nil, errors.New("存储配置不存在")
	}
	if err := db.DB.WithContext(ctx).First(&storageConfig, "id = ?", task.StorageConfigID).Error; err != nil {
		return "", nil, errors.New("存储配置不存在")
	}

	var user models.User
	userName := "unknown"
	if err := db.DB.WithContext(ctx).Select("username").First(&user, "id = ?", task.UserID).Error; err == nil && user.Username != "" {
		userName = user.Username
	}

	if err := newPGStore().UpdateUploadTaskProgress(ctx, task.ID, "preparing"); err != nil {
		return "", nil, err
	}

	client, err := lib.GetIncusClientForHost(ctx, host)
	if err != nil {
		return "", nil, err
	}

	provider, err := currentStorageProviderFactory()(&storageConfig)
	if err != nil {
		return "", nil, err
	}
	providerForCleanup = provider

	dateStr := time.Now().UTC().Format(uploadRemoteNameTimeLayout)
	remoteFileName = uploadRemoteNameSanitizer.ReplaceAllString(
		fmt.Sprintf("%s-%s-%s-%s.tar.gz", userName, instance.IncusID, backup.IncusName, dateStr), "_")

	log.Printf("[UploadWorker] Task %d: 开始上传 %s 到 %s", task.ID, remoteFileName, storageConfig.Name)

	if err := newPGStore().UpdateUploadTaskProgress(ctx, task.ID, "uploading"); err != nil {
		return remoteFileName, nil, err
	}

	stream, err := lib.GetBackupExportStream(ctx, client, instance.IncusID, backup.IncusName)
	if err != nil {
		return remoteFileName, nil, err
	}

	checkTimeout := func() error {
		if time.Since(startTime) > uploadTimeout {
			return errors.New("上传操作超时 (30 分钟)")
		}
		return nil
	}
	if err := checkTimeout(); err != nil {
		_ = stream.Body.Close()
		return remoteFileName, nil, err
	}

	uploadErr := provider.UploadStream(ctx, stream.Body, remoteFileName)
	closeErr := stream.Body.Close()
	if uploadErr == nil {
		uploadErr = closeErr
	}
	if uploadErr != nil {
		return remoteFileName, nil, uploadErr
	}

	if err := checkTimeout(); err != nil {
		return remoteFileName, nil, err
	}

	if err := newPGStore().UpdateUploadTaskProgress(ctx, task.ID, "finalizing"); err != nil {
		return remoteFileName, nil, err
	}

	return remoteFileName, stream.ContentLength, nil
}
