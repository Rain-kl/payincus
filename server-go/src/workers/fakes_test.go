package workers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// recordingNotifier implements both worker notifier interfaces and records every
// event so the tests can assert the failure/retry/success paths without touching
// the database or the websocket hub.
type recordingNotifier struct {
	mu            sync.Mutex
	failures      []string
	started       []string
	stopped       []string
	restarted     []string
	rebuilt       []string
	restored      []string
	restoreFailed []string
	uploaded      []string
	uploadFailed  []string
	uploadRetry   []string
}

func (n *recordingNotifier) InstanceTaskFailed(ctx context.Context, notice instanceTaskFailureNotice, message string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.failures = append(n.failures, message)
}

func (n *recordingNotifier) InstanceStarted(ctx context.Context, userID int32, instanceName, hostName string, ipv4, ipv6 *string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.started = append(n.started, instanceName)
}

func (n *recordingNotifier) InstanceStopped(ctx context.Context, userID int32, instanceName, hostName string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stopped = append(n.stopped, instanceName)
}

func (n *recordingNotifier) InstanceRestarted(ctx context.Context, userID int32, instanceName, hostName string, ipv4, ipv6 *string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.restarted = append(n.restarted, instanceName)
}

func (n *recordingNotifier) InstanceRebuilt(ctx context.Context, userID int32, instanceName, hostName, image string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.rebuilt = append(n.rebuilt, instanceName)
}

func (n *recordingNotifier) BackupRestored(ctx context.Context, userID, instanceID int32, instanceName, backupName, hostName string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.restored = append(n.restored, instanceName)
}

func (n *recordingNotifier) BackupRestoreFailed(ctx context.Context, userID, instanceID int32, instanceName, message string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.restoreFailed = append(n.restoreFailed, message)
}

func (n *recordingNotifier) BackupUploaded(ctx context.Context, userID, instanceID int32, instanceName, backupName, storageName, fileName, hostName string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.uploaded = append(n.uploaded, fileName)
}

func (n *recordingNotifier) BackupUploadFailed(ctx context.Context, userID, instanceID int32, message string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.uploadFailed = append(n.uploadFailed, message)
}

func (n *recordingNotifier) BackupUploadRetry(ctx context.Context, userID, instanceID, attempt, max int32, message string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.uploadRetry = append(n.uploadRetry, message)
}

func (n *recordingNotifier) snapshot() recordingNotifierSnapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	return recordingNotifierSnapshot{
		failures:      append([]string(nil), n.failures...),
		started:       append([]string(nil), n.started...),
		stopped:       append([]string(nil), n.stopped...),
		restarted:     append([]string(nil), n.restarted...),
		rebuilt:       append([]string(nil), n.rebuilt...),
		restored:      append([]string(nil), n.restored...),
		restoreFailed: append([]string(nil), n.restoreFailed...),
		uploaded:      append([]string(nil), n.uploaded...),
		uploadFailed:  append([]string(nil), n.uploadFailed...),
		uploadRetry:   append([]string(nil), n.uploadRetry...),
	}
}

type recordingNotifierSnapshot struct {
	failures, started, stopped, restarted, rebuilt []string
	restored, restoreFailed                        []string
	uploaded, uploadFailed, uploadRetry            []string
}

// ==================== instance task fakes ====================

type fakeInstanceTaskStore struct {
	mu       sync.Mutex
	tasks    map[int32]*instanceTaskRecord
	order    []int32
	pending  []instanceTaskBrief
	heavy    bool
	anyBusy  bool
	claims   int
	progress map[int32][]string
	notices  []instanceTaskFailureNotice
	getErr   error
	claimErr error
}

func newFakeInstanceTaskStore(tasks ...*instanceTaskRecord) *fakeInstanceTaskStore {
	store := &fakeInstanceTaskStore{
		tasks:    map[int32]*instanceTaskRecord{},
		progress: map[int32][]string{},
	}
	for _, task := range tasks {
		if task.Status == "" {
			task.Status = "PENDING"
		}
		store.tasks[task.ID] = task
		store.order = append(store.order, task.ID)
		if task.Status == "PENDING" {
			store.pending = append(store.pending, instanceTaskBrief{ID: task.ID, HostID: task.HostID, TaskType: task.TaskType})
		}
	}
	return store
}

func (s *fakeInstanceTaskStore) ListPending(ctx context.Context) ([]instanceTaskBrief, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]instanceTaskBrief(nil), s.pending...), nil
}

func (s *fakeInstanceTaskStore) HostHasProcessingHeavyTask(ctx context.Context, hostID int32) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heavy, nil
}

func (s *fakeInstanceTaskStore) HostHasProcessingInstanceTask(ctx context.Context, hostID int32) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.anyBusy, nil
}

func (s *fakeInstanceTaskStore) ClaimLight(ctx context.Context, hostID, taskID int32) (*instanceTaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	task, ok := s.tasks[taskID]
	if !ok || task.HostID != hostID || s.heavy || s.anyBusy {
		return nil, nil
	}
	// The real store performs `UPDATE ... WHERE status='PENDING'`; the fake models
	// that compare-and-set under one mutex so two goroutines cannot both win.
	if task.Status != "PENDING" {
		return nil, nil
	}
	task.Status = "PROCESSING"
	s.claims++
	copy := *task
	return &copy, nil
}

func (s *fakeInstanceTaskStore) ClaimNext(ctx context.Context, hostID int32) (*instanceTaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	if s.anyBusy {
		return nil, nil
	}
	for _, id := range s.order {
		task := s.tasks[id]
		if task.HostID != hostID || task.Status != "PENDING" {
			continue
		}
		task.Status = "PROCESSING"
		s.claims++
		copy := *task
		return &copy, nil
	}
	return nil, nil
}

func (s *fakeInstanceTaskStore) GetTask(ctx context.Context, id int32) (*instanceTaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	task, ok := s.tasks[id]
	if !ok {
		return nil, nil
	}
	copy := *task
	return &copy, nil
}

func (s *fakeInstanceTaskStore) UpdateProgress(ctx context.Context, id int32, progress string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress[id] = append(s.progress[id], progress)
	return nil
}

func (s *fakeInstanceTaskStore) MarkCompleted(ctx context.Context, id int32) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok || task.Status != "PROCESSING" {
		return false, nil
	}
	task.Status = "COMPLETED"
	return true, nil
}

func (s *fakeInstanceTaskStore) MarkFailed(ctx context.Context, id int32, message string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok || task.Status != "PROCESSING" {
		return false, nil
	}
	task.Status = "FAILED"
	task.Error = &message
	return true, nil
}

func (s *fakeInstanceTaskStore) Requeue(ctx context.Context, id int32, message string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok || task.Status != "PROCESSING" {
		return false, nil
	}
	task.Status = "PENDING"
	task.RetryCount++
	task.Error = &message
	s.pending = append(s.pending, instanceTaskBrief{ID: task.ID, HostID: task.HostID, TaskType: task.TaskType})
	return true, nil
}

func (s *fakeInstanceTaskStore) CleanupStale(ctx context.Context) ([]instanceTaskFailureNotice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]instanceTaskFailureNotice(nil), s.notices...), nil
}

func (s *fakeInstanceTaskStore) CleanupTimeout(ctx context.Context, running map[int32]struct{}) ([]instanceTaskFailureNotice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]instanceTaskFailureNotice(nil), s.notices...), nil
}

func (s *fakeInstanceTaskStore) statusOf(id int32) (string, int32, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[id]
	if task == nil {
		return "", 0, ""
	}
	errMessage := ""
	if task.Error != nil {
		errMessage = *task.Error
	}
	return task.Status, task.RetryCount, errMessage
}

func (s *fakeInstanceTaskStore) claimCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims
}

type fakeInstanceExecutor struct {
	mu        sync.Mutex
	calls     []int32
	errByID   map[int32]error
	err       error
	block     chan struct{}
	started   chan int32
	ignoreCtx bool
	finished  int32
}

func (e *fakeInstanceExecutor) Execute(ctx context.Context, task *instanceTaskRecord) error {
	e.mu.Lock()
	e.calls = append(e.calls, task.ID)
	e.mu.Unlock()
	if e.started != nil {
		select {
		case e.started <- task.ID:
		default:
		}
	}
	if e.block != nil {
		if e.ignoreCtx {
			<-e.block
		} else {
			select {
			case <-e.block:
			case <-ctx.Done():
				// Mirror a real executor: shutdown cancels the Incus call, but the task
				// transition is still recorded by the queue loop.
			}
		}
	}
	if e.errByID != nil {
		if err, ok := e.errByID[task.ID]; ok {
			return err
		}
	}
	return e.err
}

func (e *fakeInstanceExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

// ==================== restore / upload fakes ====================

type fakeRestoreStore struct {
	mu           sync.Mutex
	tasks        map[int32]*restoreTaskRecord
	order        []int32
	pending      []int32
	claims       int
	completed    int
	failed       int
	progress     []string
	notices      []instanceTaskFailureNotice
	timeoutCalls int
}

func newFakeRestoreStore(tasks ...*restoreTaskRecord) *fakeRestoreStore {
	store := &fakeRestoreStore{tasks: map[int32]*restoreTaskRecord{}}
	for _, task := range tasks {
		if task.Status == "" {
			task.Status = "PENDING"
		}
		store.tasks[task.ID] = task
		store.order = append(store.order, task.ID)
		store.pending = append(store.pending, task.HostID)
	}
	return store
}

func (s *fakeRestoreStore) ListPendingHostIDs(ctx context.Context) ([]int32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int32(nil), s.pending...), nil
}

func (s *fakeRestoreStore) ClaimNext(ctx context.Context, hostID int32) (*restoreTaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		task := s.tasks[id]
		if task.HostID != hostID || task.Status != "PENDING" {
			continue
		}
		task.Status = "PROCESSING"
		s.claims++
		copy := *task
		return &copy, nil
	}
	return nil, nil
}

func (s *fakeRestoreStore) GetTask(ctx context.Context, id int32) (*restoreTaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return nil, nil
	}
	copy := *task
	return &copy, nil
}

func (s *fakeRestoreStore) UpdateProgress(ctx context.Context, id int32, progress string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress = append(s.progress, progress)
	return nil
}

func (s *fakeRestoreStore) MarkCompleted(ctx context.Context, id int32) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok || task.Status != "PROCESSING" {
		return false, nil
	}
	task.Status = "COMPLETED"
	s.completed++
	return true, nil
}

func (s *fakeRestoreStore) MarkFailed(ctx context.Context, id int32, message string, progress *string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok || task.Status != "PROCESSING" {
		return false, nil
	}
	task.Status = "FAILED"
	task.Error = &message
	task.Progress = progress
	s.failed++
	return true, nil
}

func (s *fakeRestoreStore) CleanupStale(ctx context.Context) (int64, error) { return 0, nil }

func (s *fakeRestoreStore) CleanupTimeout(ctx context.Context, running map[int32]struct{}) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timeoutCalls++
	return 0, nil
}

func (s *fakeRestoreStore) counters() (claims, completed, failed, timeoutCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims, s.completed, s.failed, s.timeoutCalls
}

func (s *fakeRestoreStore) statusOf(id int32) (string, *string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[id]
	if task == nil {
		return "", nil
	}
	return task.Status, task.Progress
}

type fakeRestoreRunner struct {
	mu        sync.Mutex
	runs      int
	err       error
	block     chan struct{}
	started   chan int32
	ignoreCtx bool
}

func (r *fakeRestoreRunner) Run(ctx context.Context, task *restoreTaskRecord) error {
	r.mu.Lock()
	r.runs++
	r.mu.Unlock()
	if r.started != nil {
		select {
		case r.started <- task.ID:
		default:
		}
	}
	if r.block != nil {
		if r.ignoreCtx {
			<-r.block
		} else {
			select {
			case <-r.block:
			case <-ctx.Done():
			}
		}
	}
	return r.err
}

func (r *fakeRestoreRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs
}

type fakeUploadStore struct {
	mu        sync.Mutex
	tasks     map[int32]*uploadTaskRecord
	order     []int32
	pending   []int32
	claims    int
	completed int
	failed    int
	requeued  int
	progress  []string
}

func newFakeUploadStore(tasks ...*uploadTaskRecord) *fakeUploadStore {
	store := &fakeUploadStore{tasks: map[int32]*uploadTaskRecord{}}
	for _, task := range tasks {
		if task.Status == "" {
			task.Status = "PENDING"
		}
		store.tasks[task.ID] = task
		store.order = append(store.order, task.ID)
		store.pending = append(store.pending, task.HostID)
	}
	return store
}

func (s *fakeUploadStore) ListPendingHostIDs(ctx context.Context) ([]int32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int32(nil), s.pending...), nil
}

func (s *fakeUploadStore) ClaimNext(ctx context.Context, hostID int32) (*uploadTaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		task := s.tasks[id]
		if task.HostID != hostID || task.Status != "PENDING" {
			continue
		}
		task.Status = "PROCESSING"
		s.claims++
		copy := *task
		return &copy, nil
	}
	return nil, nil
}

func (s *fakeUploadStore) GetTask(ctx context.Context, id int32) (*uploadTaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return nil, nil
	}
	copy := *task
	return &copy, nil
}

func (s *fakeUploadStore) UpdateProgress(ctx context.Context, id int32, progress string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.progress = append(s.progress, progress)
	return nil
}

func (s *fakeUploadStore) MarkCompleted(ctx context.Context, id int32, remoteFileName string, fileSize *int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok || task.Status != "PROCESSING" {
		return false, nil
	}
	task.Status = "COMPLETED"
	task.RemoteFileName = &remoteFileName
	task.FileSize = fileSize
	s.completed++
	return true, nil
}

func (s *fakeUploadStore) Requeue(ctx context.Context, id int32) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok || task.Status != "PROCESSING" {
		return false, nil
	}
	task.Status = "PENDING"
	task.RetryCount++
	task.Progress = nil
	s.requeued++
	return true, nil
}

func (s *fakeUploadStore) MarkFailed(ctx context.Context, id int32, message string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok || task.Status != "PROCESSING" {
		return false, nil
	}
	task.Status = "FAILED"
	task.Error = &message
	s.failed++
	return true, nil
}

func (s *fakeUploadStore) CleanupStale(ctx context.Context) (int64, error) { return 0, nil }

func (s *fakeUploadStore) CleanupTimeout(ctx context.Context, running map[int32]struct{}) (int64, error) {
	return 0, nil
}

func (s *fakeUploadStore) counters() (claims, completed, failed, requeued int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims, s.completed, s.failed, s.requeued
}

func (s *fakeUploadStore) statusOf(id int32) (string, int32, *string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.tasks[id]
	if task == nil {
		return "", 0, nil
	}
	return task.Status, task.RetryCount, task.RemoteFileName
}

type fakeUploadRunner struct {
	mu         sync.Mutex
	runs       int
	err        error
	remoteName string
	fileSize   *int64
	block      chan struct{}
	started    chan int32
	ignoreCtx  bool
}

func (r *fakeUploadRunner) Run(ctx context.Context, task *uploadTaskRecord) (string, *int64, error) {
	r.mu.Lock()
	r.runs++
	r.mu.Unlock()
	if r.started != nil {
		select {
		case r.started <- task.ID:
		default:
		}
	}
	if r.block != nil {
		if r.ignoreCtx {
			<-r.block
		} else {
			select {
			case <-r.block:
			case <-ctx.Done():
			}
		}
	}
	if r.err != nil {
		return "", nil, r.err
	}
	return r.remoteName, r.fileSize, nil
}

func (r *fakeUploadRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs
}

// ==================== host notification fakes ====================

type fakeEmailStore struct {
	mu          sync.Mutex
	task        *models.HostNotificationEmailTask
	claims      int
	sent        int
	retried     int
	retryReason string
	staleCount  int64
	timeoutSeen int64
}

func (s *fakeEmailStore) ClaimNext(ctx context.Context) (*models.HostNotificationEmailTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.task == nil || s.task.Status != "PENDING" {
		return nil, nil
	}
	s.task.Status = "PROCESSING"
	s.claims++
	copy := *s.task
	return &copy, nil
}

func (s *fakeEmailStore) MarkSent(ctx context.Context, taskID int32) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.task == nil || s.task.ID != taskID || s.task.Status != "PROCESSING" {
		return false, nil
	}
	s.task.Status = "SENT"
	s.sent++
	return true, nil
}

func (s *fakeEmailStore) RetryOrFail(ctx context.Context, taskID, currentRetryCount int32, message string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.task == nil || s.task.ID != taskID || s.task.Status != "PROCESSING" {
		return false, nil
	}
	if currentRetryCount >= 3 {
		s.task.Status = "FAILED"
	} else {
		s.task.Status = "PENDING"
		s.task.RetryCount++
	}
	s.retried++
	s.retryReason = message
	return true, nil
}

func (s *fakeEmailStore) CleanupStale(ctx context.Context, timeoutMs int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.staleCount, nil
}

func (s *fakeEmailStore) CleanupTimeout(ctx context.Context, timeoutMs int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timeoutSeen++
	return s.staleCount, nil
}

func (s *fakeEmailStore) counters() (claims, sent, retried int, reason string, timeoutCalls int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims, s.sent, s.retried, s.retryReason, s.timeoutSeen
}

type fakeEmailSender struct {
	mu      sync.Mutex
	calls   int
	result  lib.EmailSendResult
	block   chan struct{}
	started chan struct{}
}

func (s *fakeEmailSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fakeEmailSender) Send(ctx context.Context, email string, data lib.HostAnnouncementEmailData) lib.EmailSendResult {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
		}
	}
	return s.result
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
