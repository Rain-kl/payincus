// Port of server/src/lib/operation-task-log.ts.
//
// Long-running node operations (instance create/destroy, node recovery) record
// their progress as rows in `operation_task_logs` so an admin can watch them live
// through /api/admin/tasks/:id/logs/stream.
//
// Two subsystems are deliberately NOT cached here:
//
//   - the AsyncLocalStorage task context: Go callers pass the *OperationTask
//     explicitly, which is clearer and equally correct in a single goroutine chain
//   - hostTunnelManager (the node log channel): it belongs to the WebSocket wave.
//     TunnelLogControl below is the seam it will fill; until then node log
//     collection reports "channel not connected", exactly like a Node process
//     whose tunnel is down.
//
// Process-local state (AGENTS.md single-instance invariant): `active` maps a task
// id to its live context so the admin API can answer "can this be stopped". A
// second backend replica would not see it, which is why this backend must stay a
// single process.
package lib

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

const (
	// operationTaskMaxLogs mirrors MAX_LOGS.
	operationTaskMaxLogs = 5000
	// operationTaskMaxPending mirrors the `ctx.pending >= 100` backpressure guard.
	operationTaskMaxPending = 100
	// operationTaskLogMaxRunes mirrors the `.slice(0, 8000)` on redactTaskLog.
	operationTaskLogMaxRunes = 8000
)

// OperationTaskKind mirrors the `'create' | 'destroy' | 'node_recover'` union.
type OperationTaskKind string

const (
	OperationTaskKindCreate      OperationTaskKind = "create"
	OperationTaskKindDestroy     OperationTaskKind = "destroy"
	OperationTaskKindNodeRecover OperationTaskKind = "node_recover"
)

// OperationTaskStatus mirrors the operation_tasks.status values.
const (
	OperationTaskStatusRunning     = "running"
	OperationTaskStatusSuccess     = "success"
	OperationTaskStatusFailed      = "failed"
	OperationTaskStatusCancelled   = "cancelled"
	OperationTaskStatusInterrupted = "interrupted"
)

// OperationTaskControlResult mirrors the shape returned by operationTaskControl().
type OperationTaskControlResult struct {
	CanStop  bool `json:"canStop"`
	Stopping bool `json:"stopping"`
}

// OperationTask is the live, process-local context of a running task.
//
// The TypeScript version keeps the DB writes on a promise chain (`ctx.queue`) to
// serialise them. Go does the same with a mutex plus a single writer goroutine per
// task, so log rows keep their arrival order without blocking the operation.
type OperationTask struct {
	ID       int32
	HostID   int32
	Name     string
	Kind     OperationTaskKind
	NodeLogs bool

	mu        sync.Mutex
	queue     []pendingOperationTaskLog
	writing   bool
	count     int
	pending   int
	dropped   int
	nodeAck   bool
	stopping  bool
	cancel    func() error
	cancelled bool
	detached  bool
}

type pendingOperationTaskLog struct {
	message string
	level   string
	source  string
}

var (
	operationTasksMu sync.Mutex
	operationTasks   = map[int32]*OperationTask{}
)

// TunnelLogControl is the seam for
// `hostTunnelManager.sendTaskLogControl(hostId, taskId, name, action)`. The
// WebSocket wave assigns it; the default reports "no channel", which makes the
// task log fall back to server-side progress only.
var TunnelLogControl = func(hostID int64, taskID int32, name, action string) bool {
	return false
}

// CurrentOperationTask returns the task attached to ctx, if any.
func CurrentOperationTask(ctx context.Context) *OperationTask {
	if ctx == nil {
		return nil
	}
	task, _ := ctx.Value(operationTaskContextKey{}).(*OperationTask)
	return task
}

type operationTaskContextKey struct{}

// OperationTaskContext attaches task to ctx for CurrentOperationTask.
func OperationTaskContext(ctx context.Context, task *OperationTask) context.Context {
	return context.WithValue(ctx, operationTaskContextKey{}, task)
}

// HasActiveOperationTask mirrors hasActiveOperationTask: is a task for this host
// and instance name still running?
func HasActiveOperationTask(hostID int32, name string) bool {
	operationTasksMu.Lock()
	defer operationTasksMu.Unlock()
	for _, task := range operationTasks {
		if task.HostID == hostID && task.Name == name {
			return true
		}
	}
	return false
}

// OperationTaskControl mirrors operationTaskControl.
func OperationTaskControl(id int32) OperationTaskControlResult {
	operationTasksMu.Lock()
	task := operationTasks[id]
	operationTasksMu.Unlock()

	if task == nil {
		return OperationTaskControlResult{}
	}

	task.mu.Lock()
	defer task.mu.Unlock()
	return OperationTaskControlResult{
		// Only expose cancellation while an actual node operation is attached.
		// Cleanup is deliberately not cancellable: it must finish releasing the
		// failed instance.
		CanStop:  task.cancel != nil && !task.stopping && !task.cancelled,
		Stopping: task.stopping || task.cancelled,
	}
}

// BindOperationCancellation mirrors bindOperationCancellation. The returned func
// unbinds, and is a no-op once the task is cancelled.
func BindOperationCancellation(ctx context.Context, cancel func() error) func() {
	task := CurrentOperationTask(ctx)
	if task == nil {
		return func() {}
	}

	task.mu.Lock()
	if task.cancelled {
		task.mu.Unlock()
		return func() {}
	}
	task.cancel = cancel
	task.mu.Unlock()

	return func() {
		task.mu.Lock()
		if task.cancel != nil {
			// Only unbind if the same cancel func is still installed.
			task.cancel = nil
		}
		task.mu.Unlock()
	}
}

// ErrOperationTaskNotStoppable mirrors the TypeScript message thrown when a task
// has no cancellable phase attached.
var ErrOperationTaskNotStoppable = errors.New("任务当前阶段无法停止，或停止请求已提交")

// StopOperationTask mirrors stopOperationTask.
func StopOperationTask(id int32) error {
	operationTasksMu.Lock()
	task := operationTasks[id]
	operationTasksMu.Unlock()

	if task == nil {
		return ErrOperationTaskNotStoppable
	}

	task.mu.Lock()
	if task.cancel == nil || task.stopping || task.cancelled {
		task.mu.Unlock()
		return ErrOperationTaskNotStoppable
	}
	cancel := task.cancel
	task.stopping = true
	task.mu.Unlock()

	task.enqueue("管理员请求停止，正在向节点发送取消信号。", "warn", "server")

	if err := cancel(); err != nil {
		task.enqueue(fmt.Sprintf("节点未接受停止请求：%s", err.Error()), "error", "server")
		task.mu.Lock()
		task.stopping = false
		task.mu.Unlock()
		return err
	}

	task.mu.Lock()
	task.cancelled = true
	task.stopping = false
	task.mu.Unlock()
	task.enqueue("节点已接受取消信号，等待操作结束及清理完成。", "warn", "server")
	return nil
}

// RedactTaskLog mirrors redactTaskLog: strip anything that looks like a
// credential, then any control sequences, then cap the length.
//
// The order is load-bearing: redaction must happen BEFORE truncation so a secret
// cannot survive on a boundary.
var (
	operationTaskPrivateKeyPattern = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?-----END [^-]*PRIVATE KEY-----`)
	operationTaskSecretPattern     = regexp.MustCompile(`(?i)((?:password|passwd|secret|token|authorization|cookie|user-data|cloud-init)["']?\s*[:=]\s*)(?:"[^"\n]*"|'[^'\n]*'|[^\s,;]+)`)
	operationTaskURLCredsPattern   = regexp.MustCompile(`(?i)(https?://)[^\s/@]+:[^\s/@]+@`)
	// ANSI terminal escapes captured from node logs.
	operationTaskANSIPattern = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]")
)

// RedactTaskLog mirrors redactTaskLog.
func RedactTaskLog(message string) string {
	redacted := SanitizeTokensInString(message)
	redacted = operationTaskPrivateKeyPattern.ReplaceAllString(redacted, "[REDACTED_KEY]")
	redacted = operationTaskSecretPattern.ReplaceAllString(redacted, "${1}[REDACTED]")
	redacted = operationTaskURLCredsPattern.ReplaceAllString(redacted, "${1}[REDACTED]@")
	redacted = operationTaskANSIPattern.ReplaceAllString(redacted, "")

	if runes := []rune(redacted); len(runes) > operationTaskLogMaxRunes {
		redacted = string(runes[:operationTaskLogMaxRunes])
	}
	return redacted
}

// TaskLog mirrors taskLog(level, args...): it always goes to the process log and,
// when a task is attached to ctx, also into the task's log rows.
func TaskLog(ctx context.Context, level string, args ...any) {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		if err, ok := arg.(error); ok {
			parts = append(parts, err.Error())
			continue
		}
		parts = append(parts, fmt.Sprint(arg))
	}

	message := RedactTaskLog(strings.Join(parts, " "))
	fmt.Printf("[%s] %s\n", level, message)

	if task := CurrentOperationTask(ctx); task != nil {
		task.enqueue(message, level, "server")
	}
}

// enqueue appends one log row, mirroring the TypeScript `enqueue`.
func (t *OperationTask) enqueue(message, level, source string) {
	if t.ID == 0 || t.count >= operationTaskMaxLogs {
		return
	}

	t.mu.Lock()
	if t.pending >= operationTaskMaxPending {
		t.dropped++
		t.mu.Unlock()
		return
	}
	t.pending++

	// `ctx.count++ === MAX_LOGS - 1` is a POST-increment comparison: the message is
	// swapped for the row whose pre-increment index is MAX-1, i.e. the last row.
	text := message
	if t.count == operationTaskMaxLogs-1 {
		text = "日志已达到上限，后续仅保留任务最终状态。"
	}
	t.count++
	t.queue = append(t.queue, pendingOperationTaskLog{message: text, level: level, source: source})
	shouldStart := !t.writing
	if shouldStart {
		t.writing = true
	}
	t.mu.Unlock()

	if shouldStart {
		go t.drain()
	}
}

// drain writes queued rows sequentially so their order matches arrival order,
// mirroring the TypeScript `ctx.queue.then(...)` chain.
func (t *OperationTask) drain() {
	for {
		t.mu.Lock()
		if len(t.queue) == 0 {
			t.writing = false
			t.mu.Unlock()
			return
		}
		batch := t.queue
		t.queue = nil
		t.mu.Unlock()

		for _, entry := range batch {
			// A transient task (ID 0) has nowhere to persist, and an unconfigured
			// handle means tests are running without a database. Neither may panic:
			// a failed log write must never break the operation.
			if t.ID != 0 && taskDBReady() {
				row := models.OperationTaskLog{
					TaskID:  t.ID,
					Message: entry.message,
					Level:   entry.level,
					Source:  entry.source,
				}
				// A failed log write must never break the operation, exactly like the
				// `.catch()` in the TypeScript.
				taskDB().Create(&row)
			}

			t.mu.Lock()
			t.pending--
			t.mu.Unlock()
		}
	}
}

// waitForQueue mirrors `await ctx.queue`, so the task never finishes before its
// last rows are persisted.
func (t *OperationTask) waitForQueue() {
	for {
		t.mu.Lock()
		busy := t.writing || len(t.queue) > 0
		t.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// operationTaskDB is the handle used for task/log persistence. package lib already
// imports package db, so it can read db.DB directly; the indirection exists so
// tests can substitute a handle without touching global state.
var operationTaskDB func() *gorm.DB

// OperationTaskDB returns the task-log handle. db.Init must have run.
func OperationTaskDB() *gorm.DB {
	if operationTaskDB != nil {
		return operationTaskDB()
	}
	return db.DB
}

// SetOperationTaskDB overrides the task-log handle (tests only).
func SetOperationTaskDB(provider func() *gorm.DB) {
	operationTaskDB = provider
}

// taskDB is the internal shorthand.
func taskDB() *gorm.DB { return OperationTaskDB() }

// taskDBReady reports whether a usable handle exists, so a transient task or a
// test without a database degrades to "log to stdout only" instead of panicking.
func taskDBReady() bool {
	if operationTaskDB != nil {
		return operationTaskDB() != nil
	}
	return db.DB != nil
}

// RunOperationTask mirrors runOperationTask: create the task row, run action with
// the task attached, then persist the outcome.
//
// A nested call (a task already attached to ctx) runs the action directly, which
// is what the TypeScript does with `if (context.getStore()) return action()`.
func RunOperationTask(
	ctx context.Context,
	hostID int32,
	name string,
	kind OperationTaskKind,
	action func(context.Context) error,
	queuedTaskID *int32,
) error {
	if CurrentOperationTask(ctx) != nil {
		return action(ctx)
	}

	var taskID int32
	if queuedTaskID != nil {
		taskID = *queuedTaskID
	} else {
		row := models.OperationTask{
			HostID:       hostID,
			InstanceName: name,
			Kind:         string(kind),
			Status:       OperationTaskStatusRunning,
		}
		if err := taskDB().Create(&row).Error; err != nil {
			if kind == OperationTaskKindNodeRecover {
				return errors.New("无法持久化节点恢复任务")
			}
			// Mirror the TypeScript: keep the business flow alive, just without a
			// diagnostic task.
			fmt.Println("[OperationTask] 无法创建诊断任务，继续执行原业务流程；请检查数据库迁移和连接")
			return action(ctx)
		}
		taskID = row.ID
	}

	task := &OperationTask{
		ID:       taskID,
		HostID:   hostID,
		Name:     name,
		Kind:     kind,
		NodeLogs: kind != OperationTaskKindNodeRecover,
	}

	operationTasksMu.Lock()
	operationTasks[taskID] = task
	operationTasksMu.Unlock()

	taskCtx := OperationTaskContext(ctx, task)

	switch kind {
	case OperationTaskKindCreate:
		task.enqueue("开始创建实例", "info", "server")
	case OperationTaskKindDestroy:
		task.enqueue("开始销毁实例", "info", "server")
	case OperationTaskKindNodeRecover:
		task.enqueue("开始恢复节点原运行状态", "info", "server")
	}

	collecting := false
	if task.NodeLogs {
		collecting = TunnelLogControl(int64(hostID), taskID, name, "start")
		if collecting {
			task.enqueue("已请求节点采集 Incus / QEMU 日志（需要支持任务日志的 Agent）。", "info", "server")
		} else {
			task.enqueue("节点日志通道未连接，仅记录服务端步骤和 Incus 操作进度。", "warn", "server")
		}
	}

	var ackTimer *time.Timer
	if collecting {
		ackTimer = time.AfterFunc(5*time.Second, func() {
			task.mu.Lock()
			acknowledged := task.nodeAck
			task.mu.Unlock()
			if !acknowledged {
				task.enqueue("节点未确认日志采集，请检查 Agent 版本和日志读取权限；服务端步骤继续记录。", "warn", "server")
			}
		})
	}

	detach := func() {
		task.mu.Lock()
		if task.detached {
			task.mu.Unlock()
			return
		}
		task.detached = true
		task.mu.Unlock()

		if ackTimer != nil {
			ackTimer.Stop()
		}
		operationTasksMu.Lock()
		delete(operationTasks, taskID)
		operationTasksMu.Unlock()
		if task.NodeLogs {
			TunnelLogControl(int64(hostID), taskID, name, "stop")
		}
	}

	// drainAndDetach lets the node's last periodic QEMU sample arrive before the
	// task is closed.
	drain := func() {
		task.mu.Lock()
		acknowledged := task.nodeAck
		dropped := task.dropped
		task.mu.Unlock()

		if collecting && !acknowledged {
			task.enqueue("未收到节点日志确认，本任务仅有服务端日志；请核对 Agent 版本和权限。", "warn", "server")
		}
		if collecting && acknowledged {
			time.Sleep(2100 * time.Millisecond)
		}
		task.waitForQueue()
		if dropped > 0 {
			task.enqueue(fmt.Sprintf("日志写入繁忙，已丢弃 %d 条节点日志。", dropped), "warn", "server")
			task.waitForQueue()
		}
	}

	runErr := action(taskCtx)
	drain()
	detach()

	if runErr != nil {
		message := RedactTaskLog(runErr.Error())
		task.enqueue(message, "error", "server")
		task.waitForQueue()

		status := OperationTaskStatusFailed
		task.mu.Lock()
		if task.cancelled {
			status = OperationTaskStatusCancelled
		}
		task.mu.Unlock()

		now := time.Now().UTC()
		taskDB().Model(&models.OperationTask{}).
			Where("id = ? AND status = ?", taskID, OperationTaskStatusRunning).
			Updates(map[string]any{
				"status":      status,
				"error":       message,
				"finished_at": now,
			})
		return runErr
	}

	task.enqueue("任务执行完成", "info", "server")
	task.waitForQueue()
	now := time.Now().UTC()
	taskDB().Model(&models.OperationTask{}).
		Where("id = ? AND status = ?", taskID, OperationTaskStatusRunning).
		Updates(map[string]any{"status": OperationTaskStatusSuccess, "finished_at": now})
	return nil
}

// RecoverInterruptedOperationTasks mirrors recoverInterruptedOperationTasks: a
// process restart must not leave tasks looking alive, nor claim the node stopped.
func RecoverInterruptedOperationTasks(ctx context.Context) error {
	var tasks []models.OperationTask
	if err := taskDB().WithContext(ctx).
		Select("id").
		Where("status = ?", OperationTaskStatusRunning).
		Find(&tasks).Error; err != nil {
		return err
	}

	for _, task := range tasks {
		now := time.Now().UTC()
		err := taskDB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			row := models.OperationTaskLog{
				TaskID:  task.ID,
				Source:  "server",
				Level:   "error",
				Message: "后端重启，任务执行结果未知；请检查节点实际状态后再操作。",
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			return tx.Model(&models.OperationTask{}).
				Where("id = ?", task.ID).
				Updates(map[string]any{
					"status":      OperationTaskStatusInterrupted,
					"error":       "后端重启，执行结果未知",
					"finished_at": now,
				}).Error
		})
		if err != nil {
			return err
		}
	}
	return nil
}
