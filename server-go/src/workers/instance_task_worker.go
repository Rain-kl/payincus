// Port of server/src/workers/instanceTaskWorker.ts — queue mechanics.
//
// One host serialises its instance mutations:
//
//   - light tasks (start/stop/restart) may run concurrently on a host, but the
//     worker claims at most LIGHT_TASK_CONCURRENCY (5) of them per tick and never
//     while a heavy task is PROCESSING;
//   - heavy tasks (rebuild/clone/recreate/change_host/create) are strictly serial
//     per host: the claim refuses if any other instance task is PROCESSING.
//
// The claim is a transaction that takes the per-host advisory lock, proves the
// shared locks (no restore/upload running), takes the per-instance advisory lock,
// and finally flips PENDING→PROCESSING with a conditional UPDATE. RowsAffected == 0
// means another worker won; this worker then executes nothing.
//
// Create is the one task type whose executor lives outside this package (the
// provisioning/route layer registers it), mirroring `setCreateTaskExecutor` in the
// TypeScript. Clone/recreate/change_host use the same registry but the built-in
// executors install themselves from instance_task_exec_heavy.go's init(); if a
// registry entry is removed they fail closed with an explicit reason instead of
// half-mutating a host.
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

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	instanceTaskPollInterval         = 3 * time.Second
	instanceTaskTimeoutCheckInterval = 60 * time.Second
	instanceTaskLightTimeout         = 5 * time.Minute
	instanceTaskHeavyTimeout         = 30 * time.Minute
	instanceTaskLightConcurrency     = 5
	instanceCreateTaskMaxRetries     = 5
)

// instanceTaskLightNames mirrors LIGHT_TASK_NAMES.
var instanceTaskLightNames = map[string]struct{}{"start": {}, "stop": {}, "restart": {}}

// instanceTaskHeavyNames mirrors HEAVY_TASK_NAMES.
var instanceTaskHeavyNames = map[string]struct{}{"rebuild": {}, "clone": {}, "recreate": {}, "change_host": {}, "create": {}}

// CreateTaskPayload mirrors CreateTaskPayload: the provisioning snapshot stored in
// instance_tasks.payload so a create task can be replayed after a node outage.
type CreateTaskPayload struct {
	Config    map[string]any `json:"config"`
	UserID    int32          `json:"userId"`
	Resources struct {
		CPU    int32 `json:"cpu"`
		Memory int32 `json:"memory"`
		Disk   int32 `json:"disk"`
	} `json:"resources"`
}

// CreateTaskExecutor mirrors CreateTaskExecutor.
type CreateTaskExecutor func(ctx context.Context, instanceID, hostID int32, payload CreateTaskPayload) error

// InstanceTaskHeavyExecutor executes one clone/recreate/change_host task.
//
// Unlike create, these three tasks are driven by the task row itself (image,
// sshKeyId, targetHostId, payload), not by a provisioning snapshot, so the
// executor receives the fully loaded runtime that executeTask already built: the
// task row, the instance row, the (source) host row and the Incus client. The
// TypeScript dispatch happens after the same prerequisites, so an executor never
// has to repeat the deleted/earliest/host checks.
type InstanceTaskHeavyExecutor func(ctx context.Context, rt *instanceTaskRuntime) error

var (
	executorRegistryMu sync.RWMutex
	createTaskExecutor CreateTaskExecutor
	heavyTaskExecutors = map[string]InstanceTaskHeavyExecutor{}
)

// SetCreateTaskExecutor mirrors setCreateTaskExecutor. The provisioning module
// calls it once at startup.
func SetCreateTaskExecutor(executor CreateTaskExecutor) {
	executorRegistryMu.Lock()
	defer executorRegistryMu.Unlock()
	createTaskExecutor = executor
}

// SetInstanceTaskExecutor registers the executor for a heavy task type
// (clone/recreate/change_host). The three built-in executors register themselves
// from instance_task_exec_heavy.go's init(); the hook stays useful for
// replacements (staging, tests) and matches the TypeScript's executor registry.
func SetInstanceTaskExecutor(taskType models.InstanceTaskType, executor InstanceTaskHeavyExecutor) {
	executorRegistryMu.Lock()
	defer executorRegistryMu.Unlock()
	if executor == nil {
		delete(heavyTaskExecutors, string(taskType))
		return
	}
	heavyTaskExecutors[string(taskType)] = executor
}

func registeredCreateExecutor() CreateTaskExecutor {
	executorRegistryMu.RLock()
	defer executorRegistryMu.RUnlock()
	return createTaskExecutor
}

func registeredHeavyExecutor(taskType string) InstanceTaskHeavyExecutor {
	executorRegistryMu.RLock()
	defer executorRegistryMu.RUnlock()
	return heavyTaskExecutors[taskType]
}

// RETRYABLE_CREATE_PATTERNS mirrors the TypeScript list. These are node-availability
// failures, not business failures: the customer keeps the reservation and the task
// goes back to PENDING.
var retryableCreatePatterns = []string{
	"HOST_OFFLINE",
	"HOST_UNREACHABLE",
	"节点离线",
	"节点不可用",
	"ECONNREFUSED",
	"ETIMEDOUT",
	"ECONNRESET",
	"socket hang up",
	"timeout",
	"Instance startup temporarily unavailable",
	"not ready",
	"still in progress",
}

func isRetryableCreateError(message string) bool {
	for _, pattern := range retryableCreatePatterns {
		// String#includes is case-sensitive; the patterns are the exact substrings
		// Node matches, including the upper-case HOST_* codes.
		if strings.Contains(message, pattern) {
			return true
		}
	}
	return false
}

// instanceTaskStore is the queue seam for instance_task_worker.go.
type instanceTaskStore interface {
	ListPending(ctx context.Context) ([]instanceTaskBrief, error)
	HostHasProcessingHeavyTask(ctx context.Context, hostID int32) (bool, error)
	HostHasProcessingInstanceTask(ctx context.Context, hostID int32) (bool, error)
	ClaimLight(ctx context.Context, hostID, taskID int32) (*instanceTaskRecord, error)
	ClaimNext(ctx context.Context, hostID int32) (*instanceTaskRecord, error)
	GetTask(ctx context.Context, id int32) (*instanceTaskRecord, error)
	UpdateProgress(ctx context.Context, id int32, progress string) error
	MarkCompleted(ctx context.Context, id int32) (bool, error)
	MarkFailed(ctx context.Context, id int32, message string) (bool, error)
	Requeue(ctx context.Context, id int32, message string) (bool, error)
	CleanupStale(ctx context.Context) ([]instanceTaskFailureNotice, error)
	CleanupTimeout(ctx context.Context, running map[int32]struct{}) ([]instanceTaskFailureNotice, error)
}

// instanceTaskExecutor performs one task's Incus work. The queue loop owns the
// status transitions; the executor only reports success or failure.
type instanceTaskExecutor interface {
	Execute(ctx context.Context, task *instanceTaskRecord) error
}

type pgInstanceTaskStore struct{}

func (pgInstanceTaskStore) ListPending(ctx context.Context) ([]instanceTaskBrief, error) {
	return newPGStore().ListPendingInstanceTasks(ctx)
}

func (pgInstanceTaskStore) HostHasProcessingHeavyTask(ctx context.Context, hostID int32) (bool, error) {
	return newPGStore().HostHasProcessingHeavyTask(ctx, hostID)
}

func (pgInstanceTaskStore) HostHasProcessingInstanceTask(ctx context.Context, hostID int32) (bool, error) {
	return newPGStore().HostHasProcessingInstanceTask(ctx, hostID)
}

func (pgInstanceTaskStore) ClaimLight(ctx context.Context, hostID, taskID int32) (*instanceTaskRecord, error) {
	return newPGStore().ClaimLightInstanceTask(ctx, hostID, taskID)
}

func (pgInstanceTaskStore) ClaimNext(ctx context.Context, hostID int32) (*instanceTaskRecord, error) {
	return newPGStore().ClaimNextInstanceTask(ctx, hostID)
}

func (pgInstanceTaskStore) GetTask(ctx context.Context, id int32) (*instanceTaskRecord, error) {
	return newPGStore().GetInstanceTask(ctx, id)
}

func (pgInstanceTaskStore) UpdateProgress(ctx context.Context, id int32, progress string) error {
	return newPGStore().UpdateInstanceTaskProgress(ctx, id, progress)
}

func (pgInstanceTaskStore) MarkCompleted(ctx context.Context, id int32) (bool, error) {
	return newPGStore().MarkInstanceTaskCompleted(ctx, id)
}

func (pgInstanceTaskStore) MarkFailed(ctx context.Context, id int32, message string) (bool, error) {
	return newPGStore().MarkInstanceTaskFailed(ctx, id, message)
}

func (pgInstanceTaskStore) Requeue(ctx context.Context, id int32, message string) (bool, error) {
	return newPGStore().RequeueInstanceTask(ctx, id, message)
}

func (pgInstanceTaskStore) CleanupStale(ctx context.Context) ([]instanceTaskFailureNotice, error) {
	return newPGStore().CleanupStaleInstanceTasks(ctx)
}

func (pgInstanceTaskStore) CleanupTimeout(ctx context.Context, running map[int32]struct{}) ([]instanceTaskFailureNotice, error) {
	return newPGStore().CleanupTimeoutInstanceTasks(ctx, running)
}

type instanceTaskWorker struct {
	store      instanceTaskStore
	executor   instanceTaskExecutor
	notifier   instanceTaskNotifier
	backoff    *workerDBBackoff
	poll       time.Duration
	timeout    time.Duration
	lightLimit int
	now        func() time.Time

	mu      sync.Mutex
	running map[int32]struct{}
	wg      sync.WaitGroup
	started bool

	// launchMu guards wg.Add against the shutdown Wait. NudgeInstanceTaskWorker may
	// call processQueue from the agent route concurrently with a SIGTERM; without
	// this gate an Add could race with Wait (a documented WaitGroup misuse panic).
	launchMu sync.Mutex
	stopping bool
}

// stop marks the worker as stopped. The actual shutdown contract is the caller's
// cancel + Wait: the run loop returns on ctx.Done and waits for in-flight tasks
// before StartWorkers' WaitGroup is released.
func (w *instanceTaskWorker) stop() {
	w.mu.Lock()
	w.started = false
	w.mu.Unlock()
}

func newInstanceTaskWorker() *instanceTaskWorker {
	return &instanceTaskWorker{
		store:      pgInstanceTaskStore{},
		executor:   newPGInstanceTaskExecutor(),
		notifier:   newPGNotifier(),
		backoff:    newWorkerDBBackoff("InstanceTaskWorker"),
		poll:       instanceTaskPollInterval,
		timeout:    instanceTaskTimeoutCheckInterval,
		lightLimit: instanceTaskLightConcurrency,
		now:        time.Now,
		running:    map[int32]struct{}{},
	}
}

func (w *instanceTaskWorker) start(ctx context.Context, wg *sync.WaitGroup) {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		log.Printf("[InstanceTaskWorker] Worker already running")
		return
	}
	w.started = true
	w.mu.Unlock()

	log.Printf("[InstanceTaskWorker] 启动 Worker...")

	wg.Add(1)
	go func() {
		defer wg.Done()
		w.run(ctx)
	}()
}

func (w *instanceTaskWorker) run(ctx context.Context) {
	pollTicker := time.NewTicker(w.poll)
	defer pollTicker.Stop()
	timeoutTicker := time.NewTicker(w.timeout)
	defer timeoutTicker.Stop()

	// Wait for in-flight executions. The stopping gate is what makes this Wait safe
	// against a concurrent nudge: launch() refuses new work once the flag is set, and
	// both the flag and every Add are taken under launchMu.
	defer func() {
		w.launchMu.Lock()
		w.stopping = true
		w.launchMu.Unlock()
		w.wg.Wait()
	}()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[InstanceTaskWorker] Worker 已停止")
			return
		case <-pollTicker.C:
			w.processQueue(ctx)
		case <-timeoutTicker.C:
			if w.backoff.shouldSkip() {
				continue
			}
			notices, err := w.store.CleanupTimeout(ctx, w.runningSnapshot())
			if err != nil {
				log.Printf("[InstanceTaskWorker] 超时检查错误: %v", err)
				w.backoff.arm(err)
				continue
			}
			if len(notices) > 0 {
				log.Printf("[InstanceTaskWorker] 清理了 %d 个超时任务", len(notices))
				for _, notice := range notices {
					w.notifyFailure(ctx, notice, "任务执行超时")
				}
			}
		}
	}
}

func (w *instanceTaskWorker) runningSnapshot() map[int32]struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	snapshot := make(map[int32]struct{}, len(w.running))
	for id := range w.running {
		snapshot[id] = struct{}{}
	}
	return snapshot
}

// processQueue mirrors processQueue: one batch query, group by host, process hosts
// in parallel. It returns after every host's light work has finished; heavy tasks
// are launched asynchronously (their claim already reserved the host).
func (w *instanceTaskWorker) processQueue(ctx context.Context) {
	if w.backoff.shouldSkip() {
		return
	}
	startTime := w.now()

	pending, err := w.store.ListPending(ctx)
	if err != nil {
		log.Printf("[InstanceTaskWorker] Queue processing error: %v", err)
		w.backoff.arm(err)
		return
	}
	if len(pending) == 0 {
		return
	}

	byHost := map[int32][]instanceTaskBrief{}
	hostOrder := []int32{}
	for _, task := range pending {
		if _, seen := byHost[task.HostID]; !seen {
			hostOrder = append(hostOrder, task.HostID)
		}
		byHost[task.HostID] = append(byHost[task.HostID], task)
	}

	var hosts sync.WaitGroup
	for _, hostID := range hostOrder {
		hosts.Add(1)
		go func(hostID int32) {
			defer hosts.Done()
			w.processHostTasks(ctx, hostID, byHost[hostID])
		}(hostID)
	}
	hosts.Wait()

	if duration := w.now().Sub(startTime); duration > 100*time.Millisecond {
		log.Printf("[InstanceTaskWorker] Queue processed in %dms, pending: %d", duration.Milliseconds(), len(pending))
	}
}

// processHostTasks mirrors processHostTasks.
func (w *instanceTaskWorker) processHostTasks(ctx context.Context, hostID int32, tasks []instanceTaskBrief) {
	light := make([]instanceTaskBrief, 0, len(tasks))
	heavy := make([]instanceTaskBrief, 0, len(tasks))
	for _, task := range tasks {
		if _, ok := instanceTaskLightNames[task.TaskType]; ok {
			light = append(light, task)
		} else if _, ok := instanceTaskHeavyNames[task.TaskType]; ok {
			heavy = append(heavy, task)
		}
	}

	runningHeavy, err := w.store.HostHasProcessingHeavyTask(ctx, hostID)
	if err != nil {
		log.Printf("[InstanceTaskWorker] Queue processing error: %v", err)
		w.backoff.arm(err)
		return
	}
	if runningHeavy {
		return
	}

	if len(light) > 0 {
		limit := w.lightLimit
		if limit <= 0 {
			limit = instanceTaskLightConcurrency
		}
		if len(light) > limit {
			light = light[:limit]
		}

		claimedIDs := make([]int32, 0, len(light))
		for _, task := range light {
			if ctx.Err() != nil {
				return
			}
			claimed, claimErr := w.store.ClaimLight(ctx, hostID, task.ID)
			if claimErr != nil {
				log.Printf("[InstanceTaskWorker] Queue processing error: %v", claimErr)
				w.backoff.arm(claimErr)
				continue
			}
			if claimed != nil {
				claimedIDs = append(claimedIDs, claimed.ID)
			}
		}

		sem := make(chan struct{}, limit)
		var lightWG sync.WaitGroup
		for _, taskID := range claimedIDs {
			taskID := taskID
			lightWG.Add(1)
			if !w.launch(func() {
				defer lightWG.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				w.execute(ctx, taskID)
			}) {
				// Shutdown won the race; the timeout sweep owns the claimed row.
				lightWG.Done()
			}
		}
		lightWG.Wait()
	}

	if len(heavy) > 0 && ctx.Err() == nil {
		runningAny, checkErr := w.store.HostHasProcessingInstanceTask(ctx, hostID)
		if checkErr != nil {
			log.Printf("[InstanceTaskWorker] Queue processing error: %v", checkErr)
			w.backoff.arm(checkErr)
			return
		}
		if runningAny {
			return
		}

		claimed, claimErr := w.store.ClaimNext(ctx, hostID)
		if claimErr != nil {
			log.Printf("[InstanceTaskWorker] Queue processing error: %v", claimErr)
			w.backoff.arm(claimErr)
			return
		}
		if claimed != nil {
			w.launch(func() {
				w.execute(ctx, claimed.ID)
			})
		}
	}
}

// execute mirrors executeTask: load the row, run the executor, then move the row
// to COMPLETED/FAILED (or back to PENDING for a retryable create failure).
func (w *instanceTaskWorker) execute(ctx context.Context, taskID int32) {
	taskStart := w.now()

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
		log.Printf("[InstanceTaskWorker] Task %d failed: %v", taskID, err)
		w.backoff.arm(err)
		return
	}
	if task == nil {
		log.Printf("[InstanceTaskWorker] Task %d not found", taskID)
		return
	}

	log.Printf("[InstanceTaskWorker] 开始执行任务 %d (%s)", taskID, task.TaskType)

	execErr := w.executor.Execute(ctx, task)
	if execErr == nil {
		completed, completeErr := w.store.MarkCompleted(ctx, taskID)
		if completeErr != nil {
			log.Printf("[InstanceTaskWorker] 任务 %d 完成状态写入失败: %v", taskID, completeErr)
			w.backoff.arm(completeErr)
			return
		}
		if !completed {
			log.Printf("[InstanceTaskWorker] 任务 %d 已不处于 PROCESSING，跳过完成状态覆盖", taskID)
		}
		log.Printf("[InstanceTaskWorker] 任务 %d (%s) 完成，耗时 %dms", taskID, task.TaskType, w.now().Sub(taskStart).Milliseconds())
		return
	}

	errorMessage := execErr.Error()
	log.Printf("[InstanceTaskWorker] 任务 %d 失败: %s", taskID, errorMessage)

	if task.TaskType == string(models.InstanceTaskTypeCreate) &&
		isRetryableCreateError(errorMessage) && task.RetryCount < instanceCreateTaskMaxRetries {
		message := fmt.Sprintf("等待节点恢复后重试（第 %d/%d 次）：%s",
			task.RetryCount+1, instanceCreateTaskMaxRetries, errorMessage)
		requeued, requeueErr := w.store.Requeue(ctx, taskID, message)
		if requeueErr != nil {
			log.Printf("[InstanceTaskWorker] 创建任务 %d 重新入队失败: %v", taskID, requeueErr)
			w.backoff.arm(requeueErr)
			return
		}
		if requeued {
			log.Printf("[InstanceTaskWorker] 创建任务 %d 因节点不可用回到队列，第 %d 次重试",
				taskID, task.RetryCount+1)
			lib.RecordIncusClientError(int64(task.HostID))
			return
		}
		log.Printf("[InstanceTaskWorker] 任务 %d 已不处于 PROCESSING，跳过重新入队", taskID)
	}

	failed, failErr := w.store.MarkFailed(ctx, taskID, errorMessage)
	if failErr != nil {
		log.Printf("[InstanceTaskWorker] 任务 %d 失败状态写入失败: %v", taskID, failErr)
		w.backoff.arm(failErr)
		return
	}
	if !failed {
		log.Printf("[InstanceTaskWorker] 任务 %d 已不处于 PROCESSING，跳过失败状态覆盖", taskID)
	}

	lib.RecordIncusClientError(int64(task.HostID))
	log.Printf("[InstanceTaskWorker] 任务 %d (%s) 失败，耗时 %dms: %s",
		taskID, task.TaskType, w.now().Sub(taskStart).Milliseconds(), errorMessage)

	w.notifyFailure(ctx, instanceTaskFailureNotice{
		ID:         task.ID,
		UserID:     task.UserID,
		InstanceID: task.InstanceID,
		HostID:     task.HostID,
		TaskType:   task.TaskType,
	}, errorMessage)
}

// launch registers a task goroutine unless shutdown has begun. The bool return
// lets the caller unwind its own bookkeeping when the launch is refused.
func (w *instanceTaskWorker) launch(fn func()) bool {
	w.launchMu.Lock()
	defer w.launchMu.Unlock()
	if w.stopping {
		return false
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		fn()
	}()
	return true
}

func (w *instanceTaskWorker) notifyFailure(ctx context.Context, notice instanceTaskFailureNotice, message string) {
	notifySafely("instance task failure notification", func() {
		w.notifier.InstanceTaskFailed(ctx, notice, message)
	})
}

// sleepCtx mirrors the TypeScript's `setTimeout` waits but returns early when the
// worker is shutting down, so stop() does not have to wait out a 180 s VM poll.
func sleepCtx(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// unmarshalCreatePayload mirrors the `task.payload as CreateTaskPayload` guard.
func unmarshalCreatePayload(payload []byte) (*CreateTaskPayload, error) {
	if len(payload) == 0 {
		return nil, errors.New("创建任务缺少参数快照，无法重试")
	}
	var parsed CreateTaskPayload
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, errors.New("创建任务缺少参数快照，无法重试")
	}
	if parsed.Config == nil {
		return nil, errors.New("创建任务缺少参数快照，无法重试")
	}
	return &parsed, nil
}

// globalInstanceWorker lets the agent-report path nudge the queue immediately
// (mirrors nudgeInstanceTaskWorker). It is nil until StartWorkers runs.
var (
	globalInstanceWorkerMu sync.RWMutex
	globalInstanceWorker   *instanceTaskWorker
)

func setGlobalInstanceWorker(worker *instanceTaskWorker) {
	globalInstanceWorkerMu.Lock()
	globalInstanceWorker = worker
	globalInstanceWorkerMu.Unlock()
}

// NudgeInstanceTaskWorker mirrors nudgeInstanceTaskWorker: a node that just
// reported back should not wait for the next 3 s tick before its queued creates
// resume. Safe to call when workers are disabled.
func NudgeInstanceTaskWorker() {
	globalInstanceWorkerMu.RLock()
	worker := globalInstanceWorker
	globalInstanceWorkerMu.RUnlock()
	if worker == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	worker.processQueue(ctx)
}
