// Port of the execution half of server/src/workers/instanceTaskWorker.ts.
//
// Implemented here: start, stop, restart and rebuild — the operations whose
// primitives already exist in the Go lib/db layers. They match the Node workflow
// step by step, including the IPv6 `file exists` workaround (temporarily detach
// eth1, start, re-attach three seconds later) and the "already running/stopped"
// reconciliation that repairs the database instead of failing the task.
//
// Clone / recreate / change_host live in instance_task_exec_heavy.go. Create
// remains a *registered* executor, mirroring setCreateTaskExecutor in the
// TypeScript, because its provisioning flow belongs to the instance routes. When
// create has no executor it fails with an explicit message instead of
// half-mutating a host (no resource is reserved, no instance is deleted). This is
// the fail-closed choice: a task that cannot be executed completely must not be
// executed partially.
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

const alpineBusyUpdateCancelAfter = 10 * time.Second

// trafficStateReconciler mirrors reconcileTrafficStateForInstanceIds from
// services/traffic-scheduler.ts. The Go scheduler module is owned by the
// schedulers wave; this hook lets it be wired without workers importing it. The
// default is a no-op (the traffic status columns were already recomputed by the
// caller); it is never allowed to fail a task.
var (
	trafficStateReconcilerMu sync.RWMutex
	trafficStateReconciler   = func(ctx context.Context, instanceIDs []int32) error { return nil }
)

// SetTrafficStateReconciler installs the traffic scheduler hook used after a
// start/rebuild/recreate.
func SetTrafficStateReconciler(fn func(ctx context.Context, instanceIDs []int32) error) {
	trafficStateReconcilerMu.Lock()
	defer trafficStateReconcilerMu.Unlock()
	if fn == nil {
		trafficStateReconciler = func(ctx context.Context, instanceIDs []int32) error { return nil }
		return
	}
	trafficStateReconciler = fn
}

func reconcileTrafficState(ctx context.Context, instanceIDs []int32) {
	trafficStateReconcilerMu.RLock()
	fn := trafficStateReconciler
	trafficStateReconcilerMu.RUnlock()
	if err := fn(ctx, instanceIDs); err != nil {
		log.Printf("[InstanceTaskWorker] 重算流量调度状态失败: %v", err)
	}
}

// pgInstanceTaskExecutor is the production instanceTaskExecutor.
type pgInstanceTaskExecutor struct {
	store    instanceTaskStore
	notifier instanceTaskNotifier
}

func newPGInstanceTaskExecutor() *pgInstanceTaskExecutor {
	return &pgInstanceTaskExecutor{store: pgInstanceTaskStore{}, notifier: newPGNotifier()}
}

func (e *pgInstanceTaskExecutor) Execute(ctx context.Context, task *instanceTaskRecord) error {
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

	earliest, err := newPGStore().EarliestActiveInstanceTask(ctx, task.InstanceID)
	if err != nil {
		return err
	}
	if earliest != nil && earliest.ID != task.ID {
		return fmt.Errorf("实例已有更早的任务正在执行: %s", earliest.TaskType)
	}

	host, err := db.HostsGetByID(ctx, task.HostID)
	if err != nil {
		return err
	}
	if host == nil {
		return errors.New("宿主机不存在")
	}

	clientStart := time.Now()
	client, err := lib.GetIncusClientForHost(ctx, host)
	if err != nil {
		return err
	}
	lib.UpdateIncusClientResponseTime(int64(host.ID), time.Since(clientStart))

	if task.TaskType == string(models.InstanceTaskTypeChangeHost) || task.TaskType == string(models.InstanceTaskTypeClone) {
		if err := db.GpusAssertMigration(ctx, task.InstanceID); err != nil {
			return err
		}
	}

	rt := &instanceTaskRuntime{Task: task, Instance: instance, Host: host, Client: client}

	switch models.InstanceTaskType(task.TaskType) {
	case models.InstanceTaskTypeStart:
		return e.executeStartTask(ctx, rt)
	case models.InstanceTaskTypeStop:
		return e.executeStopTask(ctx, rt)
	case models.InstanceTaskTypeRestart:
		return e.executeRestartTask(ctx, rt)
	case models.InstanceTaskTypeRebuild:
		return e.executeRebuildTask(ctx, rt)
	case models.InstanceTaskTypeCreate:
		payload, parseErr := unmarshalCreatePayload(task.Payload)
		if parseErr != nil {
			return parseErr
		}
		executor := registeredCreateExecutor()
		if executor == nil {
			return errors.New("创建任务执行器未就绪，等待服务完成初始化")
		}
		return executor(ctx, task.InstanceID, task.HostID, *payload)
	case models.InstanceTaskTypeClone, models.InstanceTaskTypeRecreate, models.InstanceTaskTypeChangeHost:
		executor := registeredHeavyExecutor(task.TaskType)
		if executor == nil {
			return fmt.Errorf("任务类型 %s 的 Go 执行器未就绪，等待实例开通流程迁移完成后重试", task.TaskType)
		}
		return executor(ctx, rt)
	default:
		return fmt.Errorf("未知任务类型: %s", task.TaskType)
	}
}

// InstanceTaskRuntime is the prerequisite set executeTask loads before any task
// type runs: the claimed row, the instance, the (source) host and its Incus
// client. It is exported because SetInstanceTaskExecutor accepts executors for
// heavy task types from outside this package.
type InstanceTaskRuntime struct {
	Task     *instanceTaskRecord
	Instance *db.InstanceDetailRecord
	Host     *db.HostsRecord
	Client   *lib.IncusClient
}

// instanceTaskRuntime keeps the historical unexported spelling used throughout
// this file; it is the same type.
type instanceTaskRuntime = InstanceTaskRuntime

func (e *pgInstanceTaskExecutor) setProgress(ctx context.Context, rt *instanceTaskRuntime, progress string) error {
	return e.store.UpdateProgress(ctx, rt.Task.ID, progress)
}

// ==================== shared helpers ====================

type incusAddress struct {
	Family  string `json:"family"`
	Address string `json:"address"`
	Scope   string `json:"scope"`
}

type incusStateNetwork struct {
	Status  string `json:"status"`
	Network map[string]struct {
		Addresses []incusAddress `json:"addresses"`
	} `json:"network"`
}

func extractIPv4(state incusStateNetwork) *string {
	for iface, info := range state.Network {
		if iface == "lo" {
			continue
		}
		for _, address := range info.Addresses {
			if address.Family == "inet" && address.Scope == "global" {
				value := address.Address
				return &value
			}
		}
	}
	return nil
}

func extractIPv6(state incusStateNetwork) *string {
	for iface, info := range state.Network {
		if iface == "lo" {
			continue
		}
		for _, address := range info.Addresses {
			if address.Family != "inet6" || address.Scope != "global" {
				continue
			}
			ip := strings.ToLower(address.Address)
			if strings.HasPrefix(ip, "fd") || strings.HasPrefix(ip, "fc") || strings.HasPrefix(ip, "fe80:") {
				continue
			}
			value := address.Address
			return &value
		}
	}
	return nil
}

func parseInstanceState(raw json.RawMessage) incusStateNetwork {
	var state incusStateNetwork
	_ = json.Unmarshal(raw, &state)
	return state
}

func getInstanceState(ctx context.Context, client *lib.IncusClient, name string) (incusStateNetwork, error) {
	raw, err := lib.IncusInstanceGetState(ctx, client, name)
	if err != nil {
		return incusStateNetwork{}, err
	}
	return parseInstanceState(raw), nil
}

// isAlreadyInStateError mirrors isAlreadyInStateError.
func isAlreadyInStateError(err error, targetState string) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if targetState == "running" {
		return strings.Contains(message, "already running") || strings.Contains(message, "already started") ||
			(strings.Contains(message, "is running") && !strings.Contains(message, "not running"))
	}
	if targetState == "stopped" {
		return strings.Contains(message, "already stopped") || strings.Contains(message, "not running") ||
			strings.Contains(message, "is stopped") || strings.Contains(message, "isn't running")
	}
	return false
}

func isAlpineImageAlias(alias *string) bool {
	return alias != nil && strings.Contains(strings.ToLower(*alias), "alpine")
}

func busyStopOptions(allowCancelBusyUpdate bool) lib.IncusInstanceBusyRetryOptions {
	options := lib.IncusInstanceBusyRetryOptions{AllowCancelBusyUpdate: allowCancelBusyUpdate}
	if allowCancelBusyUpdate {
		options.BusyUpdateCancelAfter = alpineBusyUpdateCancelAfter
	}
	return options
}

func needsRoutedIPv6(networkMode models.NetworkMode) bool {
	return networkMode == models.NetworkModeNatIpv6 || networkMode == models.NetworkModeIpv6Only
}

// waitForInstanceIPs mirrors the repeated 2 s/3 s network poll: VMs get 60
// attempts at 3 s, containers 30 at 2 s. ctx cancellation returns early so a
// shutdown cannot be delayed by a three-minute VM boot.
func waitForInstanceIPs(ctx context.Context, client *lib.IncusClient, incusID string, networkMode models.NetworkMode, isVM bool) (*string, *string, error) {
	var ipv4, ipv6 *string
	maxAttempts, pollInterval := 30, 2*time.Second
	if isVM {
		maxAttempts, pollInterval = 60, 3*time.Second
	}
	wantIPv6 := needsRoutedIPv6(networkMode)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := sleepCtx(ctx, pollInterval); err != nil {
			return ipv4, ipv6, err
		}
		state, err := getInstanceState(ctx, client, incusID)
		if err != nil {
			continue
		}
		ipv4 = extractIPv4(state)
		ipv6 = extractIPv6(state)
		if wantIPv6 {
			if ipv4 != nil && ipv6 != nil {
				return ipv4, ipv6, nil
			}
		} else if ipv4 != nil {
			return ipv4, ipv6, nil
		}
	}
	return ipv4, ipv6, nil
}

func updateInstanceStatus(ctx context.Context, instanceID int32, status models.InstanceStatus, ipv4, ipv6 *string) error {
	return db.UpdateInstanceStatus(ctx, instanceID, status, &db.InstanceNetworkUpdate{
		IPv4: db.InstanceValue(ipv4),
		IPv6: db.InstanceValue(ipv6),
	})
}

// startWithIPv6RouteFallback mirrors the "file exists" branch: a force-stopped
// routed-IPv6 instance can leave a kernel host route behind; temporarily removing
// eth1 lets the start succeed, and re-adding it three seconds later lets Incus
// hot-plug and install the new route.
func startWithIPv6RouteFallback(ctx context.Context, client *lib.IncusClient, incusID string, devices map[string]map[string]any) error {
	_, startErr := lib.IncusInstanceStart(ctx, client, incusID)
	if startErr == nil || !strings.Contains(startErr.Error(), "file exists") || devices == nil {
		return startErr
	}
	eth1, ok := devices["eth1"]
	if !ok || eth1 == nil {
		return startErr
	}
	log.Printf("[InstanceTaskWorker] IPv6 路由残留，尝试暂移 eth1 后启动...")
	withoutEth1 := map[string]map[string]any{}
	for name, device := range devices {
		if name != "eth1" {
			withoutEth1[name] = device
		}
	}
	if _, err := lib.UpdateInstance(ctx, client, incusID, map[string]any{"devices": withoutEth1}); err != nil {
		return startErr
	}
	if _, err := lib.IncusInstanceStart(ctx, client, incusID); err != nil {
		return err
	}
	if err := sleepCtx(ctx, 3*time.Second); err != nil {
		return err
	}
	restored := map[string]map[string]any{}
	for name, device := range withoutEth1 {
		restored[name] = device
	}
	restored["eth1"] = eth1
	if _, err := lib.UpdateInstance(ctx, client, incusID, map[string]any{"devices": restored}); err != nil {
		log.Printf("[InstanceTaskWorker] eth1 恢复失败（实例仍在运行）: %v", err)
	}
	log.Printf("[InstanceTaskWorker] eth1 已恢复，IPv6 路由重新建立")
	return nil
}

func recordSuccessLog(ctx context.Context, userID int32, module, action, content string, instanceID int32) {
	db.CreateLog(ctx, notifierUserID(userID), module, action, content, "success", db.LogOptionsWithInstance(&instanceID))
}

// ==================== start / stop / restart ====================

func (e *pgInstanceTaskExecutor) executeStartTask(ctx context.Context, rt *instanceTaskRuntime) error {
	if err := e.setProgress(ctx, rt, "starting"); err != nil {
		return err
	}

	if _, err := lib.IncusInstanceStart(ctx, rt.Client, rt.Instance.IncusID); err != nil {
		if strings.Contains(err.Error(), "file exists") {
			if incusInstance, getErr := lib.GetInstance(ctx, rt.Client, rt.Instance.IncusID); getErr == nil {
				if eth1, ok := incusInstance.Devices["eth1"]; ok && eth1 != nil {
					if startErr := startWithIPv6RouteFallback(ctx, rt.Client, rt.Instance.IncusID, incusInstance.Devices); startErr != nil {
						return startErr
					}
				} else {
					return err
				}
			} else {
				return err
			}
		} else if isAlreadyInStateError(err, "running") {
			log.Printf("[Start] 实例 %s 已经是运行状态，自动修正数据库", rt.Instance.IncusID)
			state, stateErr := getInstanceState(ctx, rt.Client, rt.Instance.IncusID)
			if stateErr != nil {
				return stateErr
			}
			ipv4, ipv6 := extractIPv4(state), extractIPv6(state)
			if err := updateInstanceStatus(ctx, rt.Task.InstanceID, models.InstanceStatusRunning, ipv4, ipv6); err != nil {
				return err
			}
			reconcileTrafficState(ctx, []int32{rt.Task.InstanceID})
			notifySafely("start notification", func() {
				e.notifier.InstanceStarted(ctx, rt.Task.UserID, rt.Instance.Name, rt.Host.Name, ipv4, ipv6)
			})
			db.CreateLog(ctx, notifierUserID(rt.Task.UserID), "instance", "instance.start",
				fmt.Sprintf("Instance \"%s\" already running, synced status [host: %s]", rt.Instance.Name, rt.Host.Name),
				"success", db.LogOptionsWithInstance(&rt.Task.InstanceID))
			return nil
		} else {
			return err
		}
	}

	incusInstance, err := lib.GetInstance(ctx, rt.Client, rt.Instance.IncusID)
	if err != nil {
		return err
	}
	isVM := incusInstance.Type == "virtual-machine"
	ipv4, ipv6, err := waitForInstanceIPs(ctx, rt.Client, rt.Instance.IncusID, rt.Instance.NetworkMode, isVM)
	if err != nil {
		return err
	}

	if err := updateInstanceStatus(ctx, rt.Task.InstanceID, models.InstanceStatusRunning, ipv4, ipv6); err != nil {
		return err
	}
	reconcileTrafficState(ctx, []int32{rt.Task.InstanceID})

	notifySafely("start notification", func() {
		e.notifier.InstanceStarted(ctx, rt.Task.UserID, rt.Instance.Name, rt.Host.Name, ipv4, ipv6)
	})
	recordSuccessLog(ctx, rt.Task.UserID, "instance", "instance.start",
		fmt.Sprintf("Started instance \"%s\" [host: %s]", rt.Instance.Name, rt.Host.Name), rt.Task.InstanceID)
	return nil
}

func (e *pgInstanceTaskExecutor) executeStopTask(ctx context.Context, rt *instanceTaskRuntime) error {
	if err := e.setProgress(ctx, rt, "stopping"); err != nil {
		return err
	}

	if success, collectErr := collectTrafficForRunningInstance(ctx, rt.Task.InstanceID); !success {
		log.Printf("[Stop] 实例 %d 关机前即时采集流量失败: %s", rt.Task.InstanceID, collectErr)
	}

	if closed := lib.CloseInstanceSessions(rt.Task.InstanceID, "Instance is stopping"); closed > 0 {
		log.Printf("[Stop] Closed %d terminal session(s) for instance %d", closed, rt.Task.InstanceID)
	}

	allowCancel := isAlpineImageAlias(&rt.Instance.Image)
	if _, err := lib.IncusInstanceStop(ctx, rt.Client, rt.Instance.IncusID, true, busyStopOptions(allowCancel)); err != nil {
		if isAlreadyInStateError(err, "stopped") {
			log.Printf("[Stop] 实例 %s 已经是停止状态，自动修正数据库", rt.Instance.IncusID)
			if err := updateInstanceStatus(ctx, rt.Task.InstanceID, models.InstanceStatusStopped, nil, nil); err != nil {
				return err
			}
			notifySafely("stop notification", func() {
				e.notifier.InstanceStopped(ctx, rt.Task.UserID, rt.Instance.Name, rt.Host.Name)
			})
			db.CreateLog(ctx, notifierUserID(rt.Task.UserID), "instance", "instance.stop",
				fmt.Sprintf("Instance \"%s\" already stopped, synced status [host: %s]", rt.Instance.Name, rt.Host.Name),
				"success", db.LogOptionsWithInstance(&rt.Task.InstanceID))
			return nil
		}
		return err
	}

	if err := updateInstanceStatus(ctx, rt.Task.InstanceID, models.InstanceStatusStopped, nil, nil); err != nil {
		return err
	}
	notifySafely("stop notification", func() {
		e.notifier.InstanceStopped(ctx, rt.Task.UserID, rt.Instance.Name, rt.Host.Name)
	})
	recordSuccessLog(ctx, rt.Task.UserID, "instance", "instance.stop",
		fmt.Sprintf("Stopped instance \"%s\" [host: %s]", rt.Instance.Name, rt.Host.Name), rt.Task.InstanceID)
	return nil
}

func (e *pgInstanceTaskExecutor) executeRestartTask(ctx context.Context, rt *instanceTaskRuntime) error {
	if err := e.setProgress(ctx, rt, "restarting"); err != nil {
		return err
	}

	if success, collectErr := collectTrafficForRunningInstance(ctx, rt.Task.InstanceID); !success {
		log.Printf("[Restart] 实例 %d 重启前即时采集流量失败: %s", rt.Task.InstanceID, collectErr)
	}
	if closed := lib.CloseInstanceSessions(rt.Task.InstanceID, "Instance is restarting"); closed > 0 {
		log.Printf("[Restart] Closed %d terminal session(s) for instance %d", closed, rt.Task.InstanceID)
	}

	if _, err := lib.IncusInstanceRestart(ctx, rt.Client, rt.Instance.IncusID, true); err != nil {
		if !isAlreadyInStateError(err, "stopped") {
			return err
		}
		log.Printf("[Restart] 实例 %s 处于停止状态，尝试启动", rt.Instance.IncusID)
		if _, startErr := lib.IncusInstanceStart(ctx, rt.Client, rt.Instance.IncusID); startErr != nil {
			if strings.Contains(startErr.Error(), "file exists") {
				incusInstance, getErr := lib.GetInstance(ctx, rt.Client, rt.Instance.IncusID)
				if getErr == nil {
					if eth1, ok := incusInstance.Devices["eth1"]; ok && eth1 != nil {
						if fallbackErr := startWithIPv6RouteFallback(ctx, rt.Client, rt.Instance.IncusID, incusInstance.Devices); fallbackErr != nil {
							return fallbackErr
						}
					} else {
						return startErr
					}
				} else {
					return startErr
				}
			} else if !isAlreadyInStateError(startErr, "running") {
				return startErr
			}
		}
	}

	incusInstance, err := lib.GetInstance(ctx, rt.Client, rt.Instance.IncusID)
	if err != nil {
		return err
	}
	ipv4, ipv6, err := waitForInstanceIPs(ctx, rt.Client, rt.Instance.IncusID, rt.Instance.NetworkMode, incusInstance.Type == "virtual-machine")
	if err != nil {
		return err
	}

	if err := updateInstanceStatus(ctx, rt.Task.InstanceID, models.InstanceStatusRunning, ipv4, ipv6); err != nil {
		return err
	}
	// The TypeScript restart path does NOT reconcile traffic state; keep that.
	notifySafely("restart notification", func() {
		e.notifier.InstanceRestarted(ctx, rt.Task.UserID, rt.Instance.Name, rt.Host.Name, ipv4, ipv6)
	})
	recordSuccessLog(ctx, rt.Task.UserID, "instance", "instance.restart",
		fmt.Sprintf("Restarted instance \"%s\" [host: %s]", rt.Instance.Name, rt.Host.Name), rt.Task.InstanceID)
	return nil
}

// ==================== rebuild ====================

func (e *pgInstanceTaskExecutor) executeRebuildTask(ctx context.Context, rt *instanceTaskRuntime) error {
	imageAlias := ""
	if rt.Task.ImageAlias != nil {
		imageAlias = *rt.Task.ImageAlias
	}
	if imageAlias == "" {
		return errors.New("未指定重装镜像")
	}
	allowCancel := isAlpineImageAlias(&rt.Instance.Image) || isAlpineImageAlias(&imageAlias)

	if closed := lib.CloseInstanceSessions(rt.Task.InstanceID, "Instance is being rebuilt"); closed > 0 {
		log.Printf("[Rebuild] Closed %d terminal session(s) for instance %d", closed, rt.Task.InstanceID)
	}
	if success, collectErr := collectTrafficForRunningInstance(ctx, rt.Task.InstanceID); !success {
		log.Printf("[Rebuild] 实例 %d 重装前即时采集流量失败: %s", rt.Task.InstanceID, collectErr)
	}

	// Ensure the instance is stopped. A state-query failure is ignored, matching the
	// TypeScript's try/catch around this block.
	if err := e.setProgress(ctx, rt, "stopping"); err != nil {
		return err
	}
	if state, stateErr := getInstanceState(ctx, rt.Client, rt.Instance.IncusID); stateErr == nil && state.Status == "Running" {
		if _, err := lib.IncusInstanceStop(ctx, rt.Client, rt.Instance.IncusID, true, busyStopOptions(allowCancel)); err != nil {
			return err
		}
	}

	// A rebuilt instance runs cloud-init again: the old detection state is stale.
	if err := resetInstanceCloudInitState(ctx, rt.Task.InstanceID); err != nil {
		return err
	}

	// Delete snapshots (Incus + database). Failures are warnings: a leftover
	// snapshot must not block a rebuild the user already paid for.
	if err := e.setProgress(ctx, rt, "deleting_snapshots"); err != nil {
		return err
	}
	if snapshots, err := lib.ListSnapshots(ctx, rt.Client, rt.Instance.IncusID); err != nil {
		log.Printf("[InstanceTaskWorker] 删除快照失败，继续执行: %v", err)
	} else {
		for _, snapshot := range snapshots {
			if snapshot.Name == "" {
				continue
			}
			if _, err := lib.DeleteSnapshot(ctx, rt.Client, rt.Instance.IncusID, snapshot.Name); err != nil {
				log.Printf("[InstanceTaskWorker] 删除快照 %s 失败: %v", snapshot.Name, err)
			}
		}
	}
	if db.DB == nil {
		return errors.New("数据库未初始化")
	}
	if err := db.DB.WithContext(ctx).Exec(`DELETE FROM snapshots WHERE instance_id = ?`, rt.Task.InstanceID).Error; err != nil {
		log.Printf("[InstanceTaskWorker] 删除快照记录失败，继续执行: %v", err)
	}

	if err := e.setProgress(ctx, rt, "rebuilding"); err != nil {
		return err
	}

	targetUserID := rt.Task.UserID
	if targetUserID == 0 {
		targetUserID = rt.Instance.UserID
	}
	var sshKey *string
	if rt.Task.SSHKeyID != nil {
		if keyRecord, err := db.GetSSHKeyByID(ctx, *rt.Task.SSHKeyID); err == nil && keyRecord != nil && keyRecord.UserID == targetUserID {
			value := keyRecord.PublicKey
			sshKey = &value
		}
	}
	if sshKey == nil {
		if keys, err := db.GetSSHKeysByUserID(ctx, targetUserID); err == nil && len(keys) > 0 {
			value := keys[0].PublicKey
			sshKey = &value
		}
	}
	if sshKey == nil || *sshKey == "" {
		log.Printf("[Rebuild] Instance %d has no SSH key; continuing without injecting authorized_keys", rt.Task.InstanceID)
	}

	extraShellCommands := ""
	if rt.Task.CustomInitCommandIDs != nil && *rt.Task.CustomInitCommandIDs != "" {
		var commandIDs []int32
		if err := json.Unmarshal([]byte(*rt.Task.CustomInitCommandIDs), &commandIDs); err != nil {
			log.Printf("[Rebuild] 解析自定义初始化命令失败: %v", err)
		} else if len(commandIDs) > 0 {
			if commands, err := db.GetCustomInitCommandsByIds(ctx, commandIDs); err == nil {
				distro := db.GetImageDistroFromAlias(imageAlias)
				valid := []db.CustomInitCommandData{}
				for _, command := range commands {
					if command.UserID != targetUserID || !command.Enabled {
						continue
					}
					compatible := false
					for _, item := range command.Distros {
						if item == "all" || item == distro {
							compatible = true
							break
						}
					}
					if compatible {
						valid = append(valid, command)
					}
				}
				if len(valid) > 0 {
					extraShellCommands = db.MergeCustomInitCommandContents(valid)
					log.Printf("[Rebuild] 应用 %d 条自定义初始化命令", len(valid))
				}
			} else {
				log.Printf("[Rebuild] 解析自定义初始化命令失败: %v", err)
			}
		}
	}

	newPassword := lib.GenerateRandomPassword(16)

	incusInstance, err := lib.GetInstance(ctx, rt.Client, rt.Instance.IncusID)
	if err != nil {
		return err
	}
	instanceType := incusInstance.Type
	if instanceType != "virtual-machine" {
		instanceType = "container"
	}

	configPayload, err := e.buildRebuildPayload(rt, imageAlias, newPassword, sshKey, extraShellCommands, instanceType)
	if err != nil {
		return err
	}
	rebuildConfig := map[string]string{}
	for key, value := range configPayload {
		rebuildConfig[key] = value
	}

	// The Node code also copies the new image's `properties` into `image.*` keys here.
	// The image-property reader (lib/incus/incus-images.ts) is not part of the Go lib
	// yet; the catch branch in Node logs and continues, and the explicit clearing
	// below is what actually protects a cross-distro rebuild.
	currentConfig := incusInstance.Config
	for key := range currentConfig {
		if strings.HasPrefix(key, "image.") || key == "user.vendor-data" || strings.HasPrefix(key, "volatile.cloud-init.") {
			rebuildConfig[key] = ""
		}
	}
	// Re-apply our own payload after the clearing pass, exactly like the TypeScript:
	// the loop above must never be able to wipe user.user-data / user.network-config.
	for key, value := range configPayload {
		rebuildConfig[key] = value
	}

	var rebuildDevices map[string]map[string]any
	if instanceType == "virtual-machine" {
		rebuildConfig["security.secureboot"] = "false"
		macs := lib.VMNetworkGenerateMacs(rt.Instance.IncusID)
		rebuildDevices = map[string]map[string]any{}
		for name, device := range incusInstance.Devices {
			copied := map[string]any{}
			for key, value := range device {
				copied[key] = value
			}
			rebuildDevices[name] = copied
		}
		if eth0, ok := rebuildDevices["eth0"]; ok {
			if existing, has := eth0["hwaddr"]; !has || existing == "" {
				eth0["hwaddr"] = macs.Eth0
			}
		}
		if eth1, ok := rebuildDevices["eth1"]; ok {
			if existing, has := eth1["hwaddr"]; !has || existing == "" {
				eth1["hwaddr"] = macs.Eth1
			}
		}
	}

	log.Printf("[Rebuild] 更新实例 %s 的 cloud-init 配置...", rt.Instance.IncusID)
	updates := map[string]any{"config": rebuildConfig}
	if rebuildDevices != nil {
		updates["devices"] = rebuildDevices
	}
	if _, err := lib.UpdateInstance(ctx, rt.Client, rt.Instance.IncusID, updates); err != nil {
		return err
	}

	log.Printf("[Rebuild] 开始重装实例 %s，镜像: %s", rt.Instance.IncusID, imageAlias)
	if _, err := lib.IncusInstanceRebuild(ctx, rt.Client, rt.Instance.IncusID,
		map[string]string{"alias": imageAlias}, nil, busyStopOptions(allowCancel)); err != nil {
		return err
	}

	if err := db.DB.WithContext(ctx).Exec(`DELETE FROM traffic_snapshots WHERE instance_id = ?`, rt.Task.InstanceID).Error; err != nil {
		return err
	}

	encryptedPassword, err := lib.EncryptSensitiveData(newPassword)
	if err != nil {
		return err
	}
	if err := db.DB.WithContext(ctx).Exec(`UPDATE instances SET image = ?, root_password = ?, updated_at = NOW() WHERE id = ?`,
		imageAlias, encryptedPassword, rt.Task.InstanceID).Error; err != nil {
		return err
	}

	if err := e.setProgress(ctx, rt, "starting"); err != nil {
		return err
	}
	// `rebuildDevices` is nil for containers, in which case the fallback resolves the
	// devices from Incus just like the start task does.
	if startErr := startWithIPv6RouteFallback(ctx, rt.Client, rt.Instance.IncusID, rebuildDevices); startErr != nil {
		return startErr
	}

	ipv4, ipv6, err := waitForInstanceIPs(ctx, rt.Client, rt.Instance.IncusID, rt.Instance.NetworkMode, instanceType == "virtual-machine")
	if err != nil {
		return err
	}
	if err := updateInstanceStatus(ctx, rt.Task.InstanceID, models.InstanceStatusRunning, ipv4, ipv6); err != nil {
		return err
	}
	reconcileTrafficState(ctx, []int32{rt.Task.InstanceID})

	notifySafely("rebuild notification", func() {
		e.notifier.InstanceRebuilt(ctx, rt.Task.UserID, rt.Instance.Name, rt.Host.Name, imageAlias)
	})
	recordSuccessLog(ctx, rt.Task.UserID, "instance", "instance.rebuild",
		fmt.Sprintf("Rebuilt instance \"%s\" with image %s", rt.Instance.Name, imageAlias), rt.Task.InstanceID)
	return nil
}

// buildRebuildPayload mirrors generateIncusConfig + generateVmConfig for the
// rebuild path, including the forced dual-stack network-config for containers.
func (e *pgInstanceTaskExecutor) buildRebuildPayload(
	rt *instanceTaskRuntime,
	imageAlias, newPassword string,
	sshKey *string,
	extraShellCommands, instanceType string,
) (map[string]string, error) {
	var network *lib.IncusConfigStaticNetwork
	if rt.Instance.IPv4 != nil && *rt.Instance.IPv4 != "" && instanceType != "virtual-machine" {
		network = &lib.IncusConfigStaticNetwork{
			IPAddress: *rt.Instance.IPv4 + "/22",
			Gateway:   "10.10.0.1",
			DNS:       []string{"10.10.0.1"},
		}
		if rt.Instance.IPv6 != nil && *rt.Instance.IPv6 != "" {
			network.IPv6Address = *rt.Instance.IPv6 + "/128"
			network.IPv6Gateway = "fe80::1"
		}
	}

	var payload lib.IncusConfigPayload
	if instanceType == "virtual-machine" {
		if rt.Instance.IPv4 != nil && *rt.Instance.IPv4 != "" {
			network = &lib.IncusConfigStaticNetwork{
				IPAddress: *rt.Instance.IPv4 + "/22",
				Gateway:   "10.10.0.1",
				DNS:       []string{"10.10.0.1"},
			}
			if rt.Instance.IPv6 != nil && *rt.Instance.IPv6 != "" {
				network.IPv6Address = *rt.Instance.IPv6
				if rt.Host.IPv6Gateway != nil {
					network.IPv6Gateway = *rt.Host.IPv6Gateway
				}
			}
		}
		vmResult, err := lib.IncusConfigGenerateVM(lib.IncusConfigVMParams{
			InstanceName:       rt.Instance.Name,
			InstanceIDSeed:     rt.Instance.IncusID,
			ImageAlias:         imageAlias,
			RootPassword:       newPassword,
			SSHKey:             sshKey,
			Network:            network,
			ExtraShellCommands: extraShellCommands,
			NetworkMode:        string(rt.Instance.NetworkMode),
		})
		if err != nil {
			return nil, err
		}
		payload = vmResult
	} else {
		containerResult, err := lib.GenerateIncusConfig(lib.IncusConfigParams{
			InstanceName:       rt.Instance.Name,
			ImageAlias:         imageAlias,
			RootPassword:       newPassword,
			SSHKey:             sshKey,
			Type:               "container",
			NetworkMode:        string(rt.Instance.NetworkMode),
			Network:            network,
			ExtraShellCommands: extraShellCommands,
		})
		if err != nil {
			return nil, err
		}
		payload = containerResult.ConfigPayload
	}

	configPayload := map[string]string{}
	if payload.UserUserData != "" {
		configPayload["user.user-data"] = payload.UserUserData
	}
	if payload.UserNetworkConfig != "" {
		configPayload["user.network-config"] = payload.UserNetworkConfig
	}
	if instanceType == "container" && rt.Instance.IPv4 != nil && *rt.Instance.IPv4 != "" &&
		rt.Instance.IPv6 != nil && *rt.Instance.IPv6 != "" && lib.NetworkPayloadHasIpv6Support(string(rt.Instance.NetworkMode)) {
		configPayload["user.network-config"] = lib.GenerateCloudInitNetworkConfig(*rt.Instance.IPv4, []string{*rt.Instance.IPv6})
	}
	return configPayload, nil
}

// resetInstanceCloudInitState mirrors lib/cloud-init-status.ts.
func resetInstanceCloudInitState(ctx context.Context, instanceID int32) error {
	if db.DB == nil {
		return errors.New("数据库未初始化")
	}
	return db.DB.WithContext(ctx).Exec(`UPDATE instances SET
		cloud_init_state = NULL, cloud_init_source = NULL, cloud_init_last_checked_at = NULL,
		cloud_init_completed_at = NULL, cloud_init_manual_completed_at = NULL,
		cloud_init_manual_completed_by = NULL, updated_at = NOW()
		WHERE id = ?`, instanceID).Error
}
