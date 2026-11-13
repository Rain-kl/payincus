// Heavy instance-task executors: clone, recreate and change_host.
//
// This file is the line-by-line port of executeCloneTask (instanceTaskWorker.ts
// 2317–2780), executeRecreateTask (1349–1742) and executeChangeHostTask
// (1748–2311). The step order, the progress markers, the database transitions and
// the compensation branches are the TypeScript's, because a partially executed
// resource-delivery task is worse than a failed one: the customer must never end
// up with a charged-but-missing instance, a half-migrated row, or a host whose
// resource counters are permanently wedged.
//
// Three invariants shape the code below.
//
//  1. **Reserve → execute → confirm, then compensate idempotently.** Every
//     allocation (IPv4 reservation, port, host capacity) is recorded before the
//     remote call, and the failure path releases it only after proving the remote
//     side is gone. A release is a set-based UPDATE/GREATEST/conditional DELETE, so
//     repeating it cannot double-decrement a counter (WAVE_D_SPEC §4.1).
//  2. **The database row is the commit point.** `databaseSwitched`/`dbSwitched`
//     guard the compensation exactly like the TypeScript flags: before the switch
//     the failure path may delete the replacement and release resources; after it
//     the old instance is already gone, so only reconciliation remains.
//  3. **UTC.** Every timestamp that reaches SQL is time.Now().UTC()
//     (WAVE_D_SPEC §0.2b).
//
// Testability: every database and Incus call goes through instanceTaskHeavySeams
// (function fields with production defaults). Unit tests install a fake seam set,
// so the whole workflow — including the compensation path — runs without a
// database or a real node. The seam is process-local and guarded by a mutex, the
// same pattern as SetTrafficCollector.
package workers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// init installs the three heavy executors. Registration lives in this file so the
// worker itself has no compile-time dependency on the provisioning flows and can
// still fail closed if an entry is removed (SetInstanceTaskExecutor(nil)).
func init() {
	SetInstanceTaskExecutor(models.InstanceTaskTypeClone, executeInstanceCloneTask)
	SetInstanceTaskExecutor(models.InstanceTaskTypeRecreate, executeInstanceRecreateTask)
	SetInstanceTaskExecutor(models.InstanceTaskTypeChangeHost, executeInstanceChangeHostTask)
}

// instanceTaskProxySite is the projection change_host needs from proxy_sites.
type instanceTaskProxySite struct {
	Domain string
}

// instanceTaskRecreateSwitch is the final instances row update of a recreate.
type instanceTaskRecreateSwitch struct {
	IncusID      string
	Image        string
	RootPassword string
	IPv4         string
	IPv6         *string
	StoragePool  *string
}

// instanceTaskHostSwitch is the change_host database switch. The traffic limit is
// resolved inside the transaction (it depends on the target host's multiplier),
// hence PackageID/BaseTrafficLimit instead of a resolved value.
type instanceTaskHostSwitch struct {
	TargetHostID        int32
	IncusID             string
	RootPassword        string
	IPv4                *string
	IPv6                *string
	StoragePool         *string
	PackageID           *int32
	BaseTrafficLimit    *int64
}

// instanceTaskClonePort mirrors one element of the TypeScript's newPortMappings.
type instanceTaskClonePort struct {
	Protocol    models.Protocol
	PublicPort  int32
	PrivatePort int32
	Remark      *string
}

// instanceTaskListenPattern mirrors /^(tcp|udp):(.+):(\d+)$/.
var instanceTaskListenPattern = regexp.MustCompile(`^(tcp|udp):(.+):(\d+)$`)

const (
	instanceTaskCloneCopyTimeout = 300 * time.Second
	instanceTaskCloneAlphabet    = "abcdefghijklmnopqrstuvwxyz0123456789"
	instanceTaskRecreateSSHPort  = 22
	instanceTaskMaxIPv6Attempts  = 50
)

// ==================== seams ====================

// instanceTaskHeavySeams is the complete side-effect surface of the three
// executors. Production defaults are installed once; tests replace the whole set.
type instanceTaskHeavySeams struct {
	// task row
	updateProgress       func(ctx context.Context, taskID int32, progress string) error
	setTaskNewInstanceID func(ctx context.Context, taskID, newInstanceID int32) error

	// reads
	getInstance      func(ctx context.Context, id int32) (*db.InstanceDetailRecord, error)
	getHost          func(ctx context.Context, id int32) (*db.HostsRecord, error)
	getClientForHost func(ctx context.Context, host *db.HostsRecord) (*lib.IncusClient, error)
	getPackage       func(ctx context.Context, id int32) (*db.PackageDetailRow, error)
	getNodeSelectors func(ctx context.Context, packageID int32) (json.RawMessage, error)
	getPlan          func(ctx context.Context, id int32) (*models.PackagePlan, error)
	getSSHKey        func(ctx context.Context, id int32) (*db.SshKeyData, error)
	getUserSSHKeys   func(ctx context.Context, userID int32) ([]db.SshKeyData, error)
	getCommands      func(ctx context.Context, ids []int32) ([]db.CustomInitCommandData, error)

	// network allocation
	allocateNatIPv4 func(ctx context.Context, hostID int32, instanceID *int32) (*string, error)
	ipv6Exists      func(ctx context.Context, address string) (bool, error)
	randomIPv6      func(subnet string) (string, error)
	allocatePort    func(ctx context.Context, hostID int32, protocol models.Protocol, excluded []int32) (int32, bool, error)
	createPort      func(ctx context.Context, input db.PortMappingCreateInput) (int32, error)
	listPorts       func(ctx context.Context, instanceID int32) ([]db.PortMappingRecord, error)
	createIP        func(ctx context.Context, data db.IpAddressesCreateData) error

	// resources
	reserveResources  func(ctx context.Context, options db.ReserveResourcesOptions) error
	rollbackResources func(ctx context.Context, options db.RollbackResourcesOptions) error
	selectAndReserve  func(ctx context.Context, options db.HostsReserveOptions) (*db.HostsSelectionRecord, error)
	calculateHostRes  func(ctx context.Context, hostID int32) (db.HostsInstanceResourceSum, error)
	updateHostRes     func(ctx context.Context, hostID int32, resources db.HostsResourceUpdate) error
	setHostNATPorts   func(ctx context.Context, hostID, count int32) error
	countSourcePorts  func(ctx context.Context, hostID int32) (int32, error)

	// storage / images / traffic
	hasStoragePool   func(ctx context.Context, hostID int32, name string) (bool, error)
	resolveNewPool   func(ctx context.Context, hostID int32, packageID *int32) (*string, error)
	imageAvailability func(ctx context.Context, alias string, hostID int32, options db.SystemImageAvailabilityOptions) (db.HostImageAvailability, error)
	resolveTraffic   func(ctx context.Context, client *gorm.DB, params lib.TrafficMultiplierResolveParams) (*int64, error)

	// instance rows
	createInstanceRow     func(ctx context.Context, input db.InstanceCreateInput) (int32, error)
	setInstanceIPv4       func(ctx context.Context, instanceID int32, address string) error
	setInstanceIPv6       func(ctx context.Context, instanceID int32, address string) error
	setInstanceStopped    func(ctx context.Context, instanceID int32) error
	findInstanceByIncusID func(ctx context.Context, incusID string) (int32, error)
	deleteInstanceRow     func(ctx context.Context, id int32) error
	cleanupRecreated      func(ctx context.Context, instanceID, hostID int32) (int32, error)
	deleteOldIPv4Rows     func(ctx context.Context, instanceID int32, address string) error
	deleteAllIPs          func(ctx context.Context, instanceID int32) error
	deleteIPv6Subnets     func(ctx context.Context, instanceID int32) error
	markInstanceError     func(ctx context.Context, instanceID int32) error
	resetCloudInit        func(ctx context.Context, instanceID int32) error
	deleteSnapshotRows    func(ctx context.Context, instanceID int32) error
	switchRecreated       func(ctx context.Context, instanceID int32, data instanceTaskRecreateSwitch) error
	switchHost            func(ctx context.Context, instanceID int32, data instanceTaskHostSwitch) error

	// proxy / caddy
	listProxySites  func(ctx context.Context, instanceID int32) ([]instanceTaskProxySite, error)
	deleteCaddySite func(ctx context.Context, host *db.HostsRecord, domains []string) error

	// side effects
	createLog        func(ctx context.Context, userID int32, action, content, result string, instanceID int32)
	sendNotification func(ctx context.Context, userID int32, event lib.EventType, data lib.EventData)

	// incus
	incusGetInstance    func(ctx context.Context, client *lib.IncusClient, name string) (*lib.IncusInstance, error)
	incusGetState       func(ctx context.Context, client *lib.IncusClient, name string) (incusStateNetwork, error)
	incusCreate         func(ctx context.Context, client *lib.IncusClient, config map[string]any, instanceID *int32) error
	incusStart          func(ctx context.Context, client *lib.IncusClient, name string) error
	incusStop           func(ctx context.Context, client *lib.IncusClient, name string, force bool, options lib.IncusInstanceBusyRetryOptions) error
	incusDelete         func(ctx context.Context, client *lib.IncusClient, name string) error
	incusUpdate         func(ctx context.Context, client *lib.IncusClient, name string, updates map[string]any) error
	incusCopy           func(ctx context.Context, client *lib.IncusClient, body map[string]any) error
	incusListSnapshots  func(ctx context.Context, client *lib.IncusClient, name string) ([]lib.IncusSnapshot, error)
	incusDeleteSnapshot func(ctx context.Context, client *lib.IncusClient, name, snapshot string) error

	// primitives
	encryptPassword func(plaintext string) (string, error)
	decryptPassword func(ciphertext string) string
	sleep           func(ctx context.Context, duration time.Duration) error
	newIncusID      func(userID int32) string
	now             func() time.Time
}

var (
	instanceTaskHeavySeamsMu  sync.RWMutex
	instanceTaskHeavySeamSet  = newInstanceTaskHeavySeams()
)

// instanceTaskHeavy returns the installed seam set.
func instanceTaskHeavy() *instanceTaskHeavySeams {
	instanceTaskHeavySeamsMu.RLock()
	defer instanceTaskHeavySeamsMu.RUnlock()
	return instanceTaskHeavySeamSet
}

// setInstanceTaskHeavySeams installs a seam set and returns a restore function.
// Tests use it to run the workflow without PostgreSQL or Incus.
func setInstanceTaskHeavySeams(seams *instanceTaskHeavySeams) func() {
	instanceTaskHeavySeamsMu.Lock()
	previous := instanceTaskHeavySeamSet
	instanceTaskHeavySeamSet = seams
	instanceTaskHeavySeamsMu.Unlock()
	return func() {
		instanceTaskHeavySeamsMu.Lock()
		instanceTaskHeavySeamSet = previous
		instanceTaskHeavySeamsMu.Unlock()
	}
}

// newInstanceTaskHeavySeams wires every seam to its production implementation.
func newInstanceTaskHeavySeams() *instanceTaskHeavySeams {
	return &instanceTaskHeavySeams{
		updateProgress: func(ctx context.Context, taskID int32, progress string) error {
			return newPGStore().UpdateInstanceTaskProgress(ctx, taskID, progress)
		},
		setTaskNewInstanceID: func(ctx context.Context, taskID, newInstanceID int32) error {
			return newPGStore().SetInstanceTaskNewInstanceID(ctx, taskID, newInstanceID)
		},
		getInstance:      db.GetInstanceById,
		getHost:          db.HostsGetByID,
		getClientForHost: lib.GetIncusClientForHost,
		getPackage:       db.GetPackageById,
		getNodeSelectors: defaultInstanceTaskNodeSelectors,
		getPlan:          db.GetPackagePlanById,
		getSSHKey:        db.GetSSHKeyByID,
		getUserSSHKeys:   db.GetSSHKeysByUserID,
		getCommands:      db.GetCustomInitCommandsByIds,

		allocateNatIPv4: db.IpAddressesAllocateAndReserveNatIpv4,
		ipv6Exists:      db.IpAddressesExists,
		randomIPv6:      lib.GenerateRandomIPv6,
		allocatePort:    db.PortMappingsAllocatePort,
		createPort:      db.PortMappingsCreate,
		listPorts:       db.PortMappingsList,
		createIP: func(ctx context.Context, data db.IpAddressesCreateData) error {
			_, err := db.IpAddressesCreate(ctx, data)
			return err
		},

		reserveResources:  db.ReserveResources,
		rollbackResources: db.RollbackResources,
		selectAndReserve:  defaultInstanceTaskSelectAndReserve,
		calculateHostRes:  db.HostsCalculateResourcesFromInstances,
		updateHostRes: func(ctx context.Context, hostID int32, resources db.HostsResourceUpdate) error {
			return db.HostsUpdateResources(ctx, hostID, resources, nil)
		},
		setHostNATPorts:  defaultInstanceTaskSetHostNATPorts,
		countSourcePorts: defaultInstanceTaskCountSourcePorts,

		hasStoragePool: db.StoragePoolsHasStoragePool,
		resolveNewPool: db.StoragePoolsResolveNewInstancePool,
		imageAvailability: func(ctx context.Context, alias string, hostID int32, options db.SystemImageAvailabilityOptions) (db.HostImageAvailability, error) {
			return db.GetSystemImageAvailabilityForHost(ctx, alias, hostID, options)
		},
		resolveTraffic: func(ctx context.Context, client *gorm.DB, params lib.TrafficMultiplierResolveParams) (*int64, error) {
			if client == nil {
				client = db.DB
			}
			return lib.ResolveInstanceTrafficLimitForHost(ctx, client, params)
		},

		createInstanceRow:     db.CreateInstance,
		setInstanceIPv4:       func(ctx context.Context, instanceID int32, address string) error { return defaultInstanceTaskSetInstanceIP(ctx, instanceID, address, true) },
		setInstanceIPv6:       func(ctx context.Context, instanceID int32, address string) error { return defaultInstanceTaskSetInstanceIP(ctx, instanceID, address, false) },
		setInstanceStopped:    func(ctx context.Context, instanceID int32) error { return db.UpdateInstanceStatus(ctx, instanceID, models.InstanceStatusStopped, nil) },
		findInstanceByIncusID: defaultInstanceTaskFindInstanceByIncusID,
		deleteInstanceRow:     defaultInstanceTaskDeleteInstanceRow,
		cleanupRecreated:      defaultInstanceTaskCleanupRecreated,
		deleteOldIPv4Rows:     defaultInstanceTaskDeleteOldIPv4Rows,
		deleteAllIPs:          defaultInstanceTaskDeleteAllIPs,
		deleteIPv6Subnets:     defaultInstanceTaskDeleteIPv6Subnets,
		markInstanceError:     defaultInstanceTaskMarkError,
		resetCloudInit:        resetInstanceCloudInitState,
		deleteSnapshotRows:    defaultInstanceTaskDeleteSnapshotRows,
		switchRecreated:       defaultInstanceTaskSwitchRecreated,
		switchHost:            defaultInstanceTaskSwitchHost,

		listProxySites:  defaultInstanceTaskListProxySites,
		deleteCaddySite: defaultInstanceTaskDeleteCaddySites,

		createLog: func(ctx context.Context, userID int32, action, content, result string, instanceID int32) {
			db.CreateLog(ctx, notifierUserID(userID), "instance", action, content, result, db.LogOptionsWithInstance(&instanceID))
		},
		sendNotification: func(ctx context.Context, userID int32, event lib.EventType, data lib.EventData) {
			lib.SendNotification(ctx, int64(userID), event, data)
		},

		incusGetInstance: lib.GetInstance,
		incusGetState: func(ctx context.Context, client *lib.IncusClient, name string) (incusStateNetwork, error) {
			return getInstanceState(ctx, client, name)
		},
		incusCreate: func(ctx context.Context, client *lib.IncusClient, config map[string]any, instanceID *int32) error {
			_, err := lib.IncusInstanceCreate(ctx, client, config, instanceID)
			return err
		},
		incusStart: func(ctx context.Context, client *lib.IncusClient, name string) error {
			_, err := lib.IncusInstanceStart(ctx, client, name)
			return err
		},
		incusStop: func(ctx context.Context, client *lib.IncusClient, name string, force bool, options lib.IncusInstanceBusyRetryOptions) error {
			_, err := lib.IncusInstanceStop(ctx, client, name, force, options)
			return err
		},
		incusDelete: func(ctx context.Context, client *lib.IncusClient, name string) error {
			_, err := lib.IncusInstanceDelete(ctx, client, name)
			return err
		},
		incusUpdate: func(ctx context.Context, client *lib.IncusClient, name string, updates map[string]any) error {
			_, err := lib.UpdateInstance(ctx, client, name, updates)
			return err
		},
		incusCopy: func(ctx context.Context, client *lib.IncusClient, body map[string]any) error {
			// mirrors client.request('POST', '/1.0/instances', payload, 300000)
			_, err := client.Request(ctx, "POST", "/1.0/instances", body, instanceTaskCloneCopyTimeout, nil)
			return err
		},
		incusListSnapshots: lib.ListSnapshots,
		incusDeleteSnapshot: func(ctx context.Context, client *lib.IncusClient, name, snapshot string) error {
			_, err := lib.DeleteSnapshot(ctx, client, name, snapshot)
			return err
		},

		encryptPassword: lib.EncryptSensitiveData,
		decryptPassword: lib.DecryptSensitiveData,
		sleep:           sleepCtx,
		newIncusID:      defaultInstanceTaskNewIncusID,
		now:             func() time.Time { return time.Now().UTC() },
	}
}

// ==================== shared helpers ====================

// defaultInstanceTaskNewIncusID mirrors nanoid(customAlphabet(...), 8) prefixed
// with `u<userId>-`. Uniqueness, not unpredictability, is the requirement; the
// instance id is not a credential.
func defaultInstanceTaskNewIncusID(userID int32) string {
	var builder strings.Builder
	builder.Grow(8)
	for i := 0; i < 8; i++ {
		builder.WriteByte(instanceTaskCloneAlphabet[rand.Intn(len(instanceTaskCloneAlphabet))])
	}
	return fmt.Sprintf("u%d-%s", userID, builder.String())
}

// instanceTaskISOString mirrors Date#toISOString: UTC with exactly three
// fractional digits.
func instanceTaskISOString(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// instanceTaskErrorText mirrors `err instanceof Error ? err.message : String(err)`.
func instanceTaskErrorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// instanceTaskNotFound mirrors the TypeScript's
// `msg.includes('not found') || msg.includes('Instance not found')` check plus the
// structured 404 IncusApiError.
func instanceTaskNotFound(err error) bool {
	if err == nil {
		return false
	}
	if lib.IsIncusNotFound(err) {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "not found") || strings.Contains(message, "Instance not found")
}

// instanceTaskFirstNonEmpty mirrors `a || b || c` for optional strings.
func instanceTaskFirstNonEmpty(values ...*string) *string {
	for _, value := range values {
		if value != nil && *value != "" {
			return value
		}
	}
	return nil
}

// instanceTaskResolveStoragePool mirrors resolveStoragePoolForExistingInstance:
// reuse the instance's recorded pool when the target host still has it, otherwise
// fall back to the usual new-instance resolution.
func instanceTaskResolveStoragePool(
	ctx context.Context,
	ops *instanceTaskHeavySeams,
	current *string,
	hostID int32,
	packageID *int32,
) (*string, error) {
	if current != nil && *current != "" {
		has, err := ops.hasStoragePool(ctx, hostID, *current)
		if err != nil {
			return nil, err
		}
		if has {
			return current, nil
		}
		return nil, nil
	}
	return ops.resolveNewPool(ctx, hostID, packageID)
}

// instanceTaskCustomInitCommands mirrors the recreate/rebuild command selection:
// only the owner's enabled commands compatible with the image distro run.
func instanceTaskCustomInitCommands(
	ctx context.Context,
	ops *instanceTaskHeavySeams,
	raw *string,
	userID int32,
	imageAlias string,
) string {
	if raw == nil || *raw == "" {
		return ""
	}
	var ids []int32
	if err := json.Unmarshal([]byte(*raw), &ids); err != nil {
		log.Printf("[InstanceTaskWorker] 解析自定义初始化命令失败: %v", err)
		return ""
	}
	if len(ids) == 0 {
		return ""
	}
	commands, err := ops.getCommands(ctx, ids)
	if err != nil {
		log.Printf("[InstanceTaskWorker] 解析自定义初始化命令失败: %v", err)
		return ""
	}
	distro := db.GetImageDistroFromAlias(imageAlias)
	valid := make([]db.CustomInitCommandData, 0, len(commands))
	for _, command := range commands {
		if command.UserID != userID || !command.Enabled {
			continue
		}
		for _, item := range command.Distros {
			if item == "all" || item == distro {
				valid = append(valid, command)
				break
			}
		}
	}
	if len(valid) == 0 {
		return ""
	}
	return db.MergeCustomInitCommandContents(valid)
}

// instanceTaskInstanceType resolves the Incus instance type from the package.
func instanceTaskInstanceType(pkg *db.PackageDetailRow) string {
	if pkg != nil && pkg.InstanceType == "vm" {
		return "virtual-machine"
	}
	return "container"
}

// instanceTaskSortDeviceNames gives deterministic device iteration (Go maps are
// unordered; JavaScript objects preserve insertion order). The port numbers are
// random either way, so only reproducibility is gained.
func instanceTaskSortDeviceNames(devices map[string]map[string]any) []string {
	names := make([]string, 0, len(devices))
	for name := range devices {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// instanceTaskSecureBootForVM mirrors `security.secureboot = 'false'` used by the
// build helper; it is already handled inside IncusInstanceBuildConfig.

// ==================== recreate ====================

// executeInstanceRecreateTask is the port of executeRecreateTask. It keeps the
// instance row, name, IP and billing, swaps the image and the Incus instance, and
// rewrites the network records only after the new instance is confirmed running.
func executeInstanceRecreateTask(ctx context.Context, rt *InstanceTaskRuntime) error {
	ops := instanceTaskHeavy()
	task, instance, host := rt.Task, rt.Instance, rt.Host

	imageAlias := instance.Image
	if task.ImageAlias != nil && *task.ImageAlias != "" {
		imageAlias = *task.ImageAlias
	}
	oldIncusID := instance.IncusID
	allowCancel := isAlpineImageAlias(&instance.Image) || isAlpineImageAlias(&imageAlias)

	if success, message := collectTrafficForRunningInstance(ctx, task.InstanceID); !success {
		log.Printf("[Recreate] 实例 %d 重建前即时采集流量失败: %s", task.InstanceID, message)
	}

	// 1. Force-stop the old instance (if it is running).
	if err := ops.updateProgress(ctx, task.ID, "stopping"); err != nil {
		return err
	}
	log.Printf("[Recreate] 停止旧实例 %s...", oldIncusID)
	if _, err := ops.incusStop(ctx, rt.Client, oldIncusID, false, busyStopOptions(allowCancel)); err != nil {
		// The instance may already be stopped or gone; continue like the TypeScript.
		log.Printf("[Recreate] 停止旧实例异常，继续执行: %v", err)
	} else {
		for attempt := 0; attempt < 30; attempt++ {
			if err := ops.sleep(ctx, time.Second); err != nil {
				return err
			}
			state, stateErr := ops.incusGetState(ctx, rt.Client, oldIncusID)
			if stateErr != nil {
				continue
			}
			if strings.ToLower(state.Status) == "stopped" {
				break
			}
		}
	}

	// A rebuilt instance runs cloud-init again: the old detection state is stale.
	if err := ops.resetCloudInit(ctx, task.InstanceID); err != nil {
		return err
	}

	// 2. Delete the old instance's snapshots (remote + rows). Any failure is a
	// warning: a leftover snapshot must not block a paid rebuild.
	if err := ops.updateProgress(ctx, task.ID, "cleaning_snapshots"); err != nil {
		return err
	}
	if err := func() error {
		snapshots, err := ops.incusListSnapshots(ctx, rt.Client, oldIncusID)
		if err != nil {
			return err
		}
		if len(snapshots) > 0 {
			log.Printf("[Recreate] 删除 %d 个快照...", len(snapshots))
		}
		for _, snapshot := range snapshots {
			if snapshot.Name == "" {
				continue
			}
			if _, err := ops.incusDeleteSnapshot(ctx, rt.Client, oldIncusID, snapshot.Name); err != nil {
				return err
			}
		}
		return ops.deleteSnapshotRows(ctx, task.InstanceID)
	}(); err != nil {
		log.Printf("[Recreate] 删除快照失败，继续执行: %v", err)
	}

	// 3. Instance type comes from the package (vm → virtual-machine).
	instanceType := "container"
	var pkg *db.PackageDetailRow
	if instance.PackageID != nil {
		loaded, err := ops.getPackage(ctx, *instance.PackageID)
		if err != nil {
			return err
		}
		pkg = loaded
		instanceType = instanceTaskInstanceType(pkg)
	}

	// 5. SSH key: the task's key when it belongs to the owner, else the owner's first.
	var sshKey *string
	if task.SSHKeyID != nil {
		record, err := ops.getSSHKey(ctx, *task.SSHKeyID)
		if err != nil {
			return err
		}
		if record != nil && record.UserID == instance.UserID {
			value := record.PublicKey
			sshKey = &value
		}
	}
	if sshKey == nil {
		keys, err := ops.getUserSSHKeys(ctx, instance.UserID)
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			value := keys[0].PublicKey
			sshKey = &value
		}
	}
	if sshKey == nil {
		log.Printf("[Recreate] Instance %d has no SSH key; continuing without injecting authorized_keys", task.InstanceID)
	}

	// 6. Custom init commands.
	extraShellCommands := instanceTaskCustomInitCommands(ctx, ops, task.CustomInitCommandIDs, instance.UserID, imageAlias)

	// 7. Allocate a fresh private IPv4. The reservation lands on the instance row,
	// so a crash between here and the switch leaves a visible, reclaimable row.
	if err := ops.updateProgress(ctx, task.ID, "creating"); err != nil {
		return err
	}
	newIPv4, allocErr := ops.allocateNatIPv4(ctx, host.ID, &task.InstanceID)
	if allocErr != nil {
		log.Printf("[Recreate] IPv4 分配失败: %v", allocErr)
		return fmt.Errorf("IPv4 分配失败: %s", instanceTaskErrorText(allocErr))
	}
	if newIPv4 == nil {
		return errors.New("IPv4 分配失败: IPv4 地址池已耗尽，无法分配新地址")
	}
	log.Printf("[Recreate] 分配并预留新的内网 IPv4: %s", *newIPv4)

	// 8. Password + cloud-init payload. The container branch is generated first;
	// the VM branch is regenerated inside the try (matching the TypeScript import).
	newPassword := lib.GenerateRandomPassword(16)
	var network *lib.IncusConfigStaticNetwork
	if instanceType != "virtual-machine" {
		network = &lib.IncusConfigStaticNetwork{
			IPAddress: *newIPv4 + "/22",
			Gateway:   "10.10.0.1",
			DNS:       []string{"10.10.0.1"},
		}
		if instance.IPv6 != nil && *instance.IPv6 != "" {
			network.IPv6Address = *instance.IPv6 + "/128"
			network.IPv6Gateway = "fe80::1"
		}
	}
	generated, err := lib.GenerateIncusConfig(lib.IncusConfigParams{
		InstanceName:       instance.Name,
		ImageAlias:         imageAlias,
		RootPassword:       newPassword,
		SSHKey:             sshKey,
		Type:               instanceType,
		NetworkMode:        string(instance.NetworkMode),
		Network:            network,
		ExtraShellCommands: extraShellCommands,
	})
	if err != nil {
		return err
	}
	configPayload := generated.ConfigPayload

	// 9. New Incus id.
	newIncusID := ops.newIncusID(task.UserID)
	log.Printf("[Recreate] 新实例 ID: %s", newIncusID)

	oldInstanceDeleted := false
	newInstanceCreated := false
	databaseSwitched := false

	runErr := func() error {
		// A VM needs the MAC-matched network-config, regenerated from the new id.
		if instanceType == "virtual-machine" {
			vmNetwork := &lib.IncusConfigStaticNetwork{
				IPAddress: *newIPv4 + "/22",
				Gateway:   "10.10.0.1",
				DNS:       []string{"10.10.0.1"},
			}
			if instance.IPv6 != nil && *instance.IPv6 != "" {
				vmNetwork.IPv6Address = *instance.IPv6
				if host.IPv6Gateway != nil {
					vmNetwork.IPv6Gateway = *host.IPv6Gateway
				}
			}
			vmResult, err := lib.IncusConfigGenerateVM(lib.IncusConfigVMParams{
				InstanceName:       instance.Name,
				InstanceIDSeed:     newIncusID,
				ImageAlias:         imageAlias,
				RootPassword:       newPassword,
				SSHKey:             sshKey,
				Network:            vmNetwork,
				ExtraShellCommands: extraShellCommands,
			})
			if err != nil {
				return err
			}
			configPayload = vmResult
			log.Printf("[Recreate] VM network-config 已更新: IPv4=%s, IPv6=%s", *newIPv4, instanceTaskValueOrNone(instance.IPv6))
		} else {
			log.Printf("[Recreate] Container network-config 已更新: IPv4=%s, IPv6=%s", *newIPv4, instanceTaskValueOrNone(instance.IPv6))
		}

		// 10. Storage pool: keep the instance's pool when the host still has it.
		storagePool, err := instanceTaskResolveStoragePool(ctx, ops, instance.StoragePoolName, host.ID, instance.PackageID)
		if err != nil {
			return err
		}
		if storagePool == nil {
			return errors.New("当前节点未配置可用于实例系统盘的存储池")
		}

		// 11. Build the replacement config.
		gpuPCIs, err := db.GpusGetInstancePCIs(ctx, task.InstanceID, host.ID)
		if err != nil {
			return err
		}
		ioMode := "throughput"
		nested, privileged := false, false
		if pkg != nil {
			if pkg.IOLimitMode != "" {
				ioMode = pkg.IOLimitMode
			}
			nested = pkg.Nested != 0
			privileged = pkg.Privileged != 0
		}
		hostInterface := "eth0"
		if host.IPv6ParentInterface != nil && *host.IPv6ParentInterface != "" {
			hostInterface = *host.IPv6ParentInterface
		}
		var ipv6Config *lib.NetworkPayloadIPv6Config
		if instance.IPv6 != nil && *instance.IPv6 != "" {
			ipv6Config = &lib.NetworkPayloadIPv6Config{PrimaryIP: *instance.IPv6}
		}
		var limitsRead, limitsWrite *string
		var limitsReadIops, limitsWriteIops *int32
		if ioMode == "throughput" {
			limitsRead = instance.LimitsRead
			limitsWrite = instance.LimitsWrite
		} else {
			limitsReadIops = instance.LimitsReadIops
			limitsWriteIops = instance.LimitsWriteIops
		}
		incusConfig, err := lib.IncusInstanceBuildConfig(lib.IncusInstanceConfigOptions{
			GPUPCIAddresses:         gpuPCIs,
			Name:                    newIncusID,
			Image:                   imageAlias,
			CPU:                     int(instance.CPU),
			Memory:                  int(instance.Memory),
			Disk:                    int(instance.Disk),
			CloudInitConfig:         &configPayload,
			NetworkMode:             string(instance.NetworkMode),
			Nested:                  nested,
			Privileged:              privileged,
			InstanceType:            instanceTaskIncusType(instanceType),
			StoragePool:             storagePool,
			IPv4Address:             newIPv4,
			IPv6Config:              ipv6Config,
			HostInterface:           &hostInterface,
			IPv6Address:             instance.IPv6,
			IPv6Gateway:             host.IPv6Gateway,
			LimitsRead:              limitsRead,
			LimitsWrite:             limitsWrite,
			LimitsReadIops:          limitsReadIops,
			LimitsWriteIops:         limitsWriteIops,
			LimitsIngress:           instance.LimitsIngress,
			LimitsEgress:            instance.LimitsEgress,
			LimitsProcesses:         instance.LimitsProcesses,
			LimitsCPUPriority:       instance.LimitsCPUPriority,
			BootAutostart:           instance.BootAutostart,
			BootAutostartPriority:   instance.BootAutostartPriority,
			BootAutostartDelay:      instance.BootAutostartDelay,
			BootHostShutdownTimeout: instance.BootHostShutdownTimeout,
		})
		if err != nil {
			return err
		}

		// 12. Delete the old instance. A 404 proves it is gone and counts as success;
		// any other failure aborts before the replacement is created.
		if err := ops.updateProgress(ctx, task.ID, "deleting_old"); err != nil {
			return err
		}
		log.Printf("[Recreate] 删除旧实例 %s（如果存在）...", oldIncusID)
		if _, err := ops.incusDelete(ctx, rt.Client, oldIncusID); err != nil {
			if instanceTaskNotFound(err) {
				log.Printf("[Recreate] 旧实例 %s 已不存在，继续执行", oldIncusID)
				oldInstanceDeleted = true
			} else {
				return fmt.Errorf("删除旧实例失败: %s", instanceTaskErrorText(err))
			}
		} else {
			log.Printf("[Recreate] 旧实例 %s 删除成功", oldIncusID)
			oldInstanceDeleted = true
		}

		// 12b. Create the replacement.
		log.Printf("[Recreate] 创建新实例 %s...", newIncusID)
		if err := ops.incusCreate(ctx, rt.Client, incusConfig, &task.InstanceID); err != nil {
			return err
		}
		newInstanceCreated = true

		if instanceType == "container" {
			if _, err := ops.incusUpdate(ctx, rt.Client, newIncusID, map[string]any{
				"config": map[string]string{
					"limits.memory.swap": lib.InstanceSwapResolveIncusValue(instance.SwapEnabled, instance.SwapSize),
				},
			}); err != nil {
				return err
			}
		}

		// 13. Start.
		if err := ops.updateProgress(ctx, task.ID, "starting"); err != nil {
			return err
		}
		log.Printf("[Recreate] 启动新实例 %s...", newIncusID)
		if _, err := ops.incusStart(ctx, rt.Client, newIncusID); err != nil {
			return err
		}

		// 14. Wait for the guest network (only the IPv6 value is persisted).
		var actualIPv6 *string
		wantIPv6 := needsRoutedIPv6(instance.NetworkMode)
		for attempt := 0; attempt < 30; attempt++ {
			if err := ops.sleep(ctx, 2*time.Second); err != nil {
				return err
			}
			state, stateErr := ops.incusGetState(ctx, rt.Client, newIncusID)
			if stateErr != nil {
				continue
			}
			stateIPv4 := extractIPv4(state)
			actualIPv6 = extractIPv6(state)
			if wantIPv6 {
				if stateIPv4 != nil && actualIPv6 != nil {
					break
				}
			} else if stateIPv4 != nil {
				break
			}
		}

		// 15. Database switch — the commit point.
		if err := ops.updateProgress(ctx, task.ID, "updating_database"); err != nil {
			return err
		}
		deletedPorts, err := ops.cleanupRecreated(ctx, task.InstanceID, instance.HostID)
		if err != nil {
			return err
		}
		if deletedPorts > 0 {
			log.Printf("[Recreate] 删除 %d 个端口映射记录并释放 NAT 端口计数", deletedPorts)
		}
		if instance.IPv4 != nil && *instance.IPv4 != "" && *instance.IPv4 != *newIPv4 {
			if err := ops.deleteOldIPv4Rows(ctx, task.InstanceID, *instance.IPv4); err != nil {
				return err
			}
			log.Printf("[Recreate] 删除旧 IPv4 记录: %s", *instance.IPv4)
		}
		if err := ops.createIP(ctx, db.IpAddressesCreateData{
			Address:    *newIPv4,
			Type:       models.IpTypeInet4,
			IsPrimary:  true,
			Device:     "eth0",
			InstanceID: task.InstanceID,
		}); err != nil {
			return err
		}
		log.Printf("[Recreate] 创建新 IPv4 记录: %s", *newIPv4)

		ipv6 := actualIPv6
		if ipv6 == nil {
			ipv6 = instance.IPv6
		}
		encryptedPassword, err := ops.encryptPassword(newPassword)
		if err != nil {
			return err
		}
		if err := ops.switchRecreated(ctx, task.InstanceID, instanceTaskRecreateSwitch{
			IncusID:      newIncusID,
			Image:        imageAlias,
			RootPassword: encryptedPassword,
			IPv4:         *newIPv4,
			IPv6:         ipv6,
			StoragePool:  storagePool,
		}); err != nil {
			return err
		}
		databaseSwitched = true

		reconcileTrafficState(ctx, []int32{task.InstanceID})
		log.Printf("[Recreate] 重建完成: %s -> %s, 新 IP: %s", oldIncusID, newIncusID, *newIPv4)

		notifySafely("recreate notification", func() {
			ops.sendNotification(ctx, task.UserID, lib.EventInstanceRebuilt, lib.EventData{
				"instanceName": instance.Name,
				"hostName":     host.Name,
				"newImage":     imageAlias,
			})
		})
		ops.createLog(ctx, task.UserID, "instance.recreate",
			fmt.Sprintf("Recreated instance \"%s\" with image %s (%s -> %s)", instance.Name, imageAlias, oldIncusID, newIncusID),
			"success", task.InstanceID)
		return nil
	}()

	if runErr == nil {
		return nil
	}

	// Compensation: before the database switch the replacement is disposable.
	if !databaseSwitched && newInstanceCreated {
		if _, stopErr := ops.incusStop(ctx, rt.Client, newIncusID, true, lib.IncusInstanceBusyRetryOptions{}); stopErr != nil {
			// The replacement may not have reached running state; deletion can proceed.
			log.Printf("[Recreate] 停止失败的新实例 %s 失败: %v", newIncusID, stopErr)
		}
		if _, deleteErr := ops.incusDelete(ctx, rt.Client, newIncusID); deleteErr != nil {
			log.Printf("[Recreate] 清理失败的新实例 %s 失败: %v", newIncusID, deleteErr)
		} else {
			log.Printf("[Recreate] 已清理失败的新实例 %s", newIncusID)
		}
	}
	if !databaseSwitched && oldInstanceDeleted {
		// The old instance is gone and no replacement was delivered: leave the row
		// in `error` with no IPs so an operator can see what happened.
		if _, err := ops.cleanupRecreated(ctx, task.InstanceID, instance.HostID); err != nil {
			log.Printf("[Recreate] 标记重建失败实例为 error 失败: %v", err)
		} else if err := ops.deleteAllIPs(ctx, task.InstanceID); err != nil {
			log.Printf("[Recreate] 标记重建失败实例为 error 失败: %v", err)
		} else if err := ops.deleteIPv6Subnets(ctx, task.InstanceID); err != nil {
			log.Printf("[Recreate] 标记重建失败实例为 error 失败: %v", err)
		} else if err := ops.markInstanceError(ctx, task.InstanceID); err != nil {
			log.Printf("[Recreate] 标记重建失败实例为 error 失败: %v", err)
		} else {
			log.Printf("[Recreate] 旧实例 %s 已删除但新实例未成功交付，数据库实例已标记为 error", oldIncusID)
		}
	}
	return runErr
}

// instanceTaskValueOrNone mirrors `${value || 'none'}`.
func instanceTaskValueOrNone(value *string) string {
	if value == nil || *value == "" {
		return "none"
	}
	return *value
}

// instanceTaskIncusType maps the worker's spelling onto the build helper's.
func instanceTaskIncusType(instanceType string) string {
	if instanceType == "virtual-machine" {
		return "vm"
	}
	return "container"
}

// ==================== change_host ====================

// executeInstanceChangeHostTask is the port of executeChangeHostTask. The target
// node is rt.Host/rt.Client; the source host is instance.host_id, and the source
// instance is only deleted after the database switch commits.
func executeInstanceChangeHostTask(ctx context.Context, rt *InstanceTaskRuntime) error {
	ops := instanceTaskHeavy()
	task, instance, targetHost := rt.Task, rt.Instance, rt.Host

	targetHostID := targetHost.ID
	if task.TargetHostID != nil && *task.TargetHostID != 0 {
		targetHostID = *task.TargetHostID
	}
	sourceHostID := instance.HostID
	oldIncusID := instance.IncusID
	imageAlias := instance.Image

	targetResourcesReserved := false
	dbSwitched := false
	newIncusID := ""

	if task.SSHKeyID == nil || *task.SSHKeyID == 0 {
		return errors.New("改节点失败：请选择 SSH 密钥")
	}
	if targetHostID != targetHost.ID {
		return errors.New("任务目标节点与队列节点不一致")
	}
	if targetHostID == sourceHostID {
		return errors.New("目标节点不能与当前节点相同")
	}

	sourceHost, err := ops.getHost(ctx, sourceHostID)
	if err != nil {
		return err
	}
	if sourceHost == nil {
		return errors.New("源节点不存在")
	}
	if instance.PackageID == nil || *instance.PackageID == 0 {
		return errors.New("该实例没有绑定套餐，无法改节点")
	}
	pkg, err := ops.getPackage(ctx, *instance.PackageID)
	if err != nil {
		return err
	}
	if pkg == nil {
		return errors.New("实例套餐不存在，无法改节点")
	}
	if !containsInt32(pkg.HostIDs, targetHostID) {
		return errors.New("目标节点不在该套餐绑定节点中")
	}

	packageInstanceType := pkg.InstanceType
	if packageInstanceType == "" {
		packageInstanceType = "container"
	}
	hostInstanceType := targetHost.InstanceType
	if hostInstanceType == "" {
		hostInstanceType = "container"
	}
	if packageInstanceType == "vm" && hostInstanceType == "container" {
		return errors.New("目标节点不支持 KVM 实例")
	}
	if packageInstanceType == "container" && hostInstanceType == "vm" {
		return errors.New("目标节点不支持 LXC 实例")
	}

	memory := int(instance.Memory)
	availability, err := ops.imageAvailability(ctx, imageAlias, targetHostID, db.SystemImageAvailabilityOptions{
		InstanceType: &packageInstanceType,
		Memory:       &memory,
	})
	if err != nil {
		return err
	}
	if !availability.OK {
		return fmt.Errorf("目标节点不可用该镜像: %s", availability.Reason)
	}

	keyRecord, err := ops.getSSHKey(ctx, *task.SSHKeyID)
	if err != nil {
		return err
	}
	if keyRecord == nil || keyRecord.UserID != instance.UserID {
		return errors.New("SSH 密钥不存在或不属于实例所有者")
	}

	rootPassword := lib.GenerateRandomPassword(16)
	if instance.RootPassword != nil && *instance.RootPassword != "" {
		if decrypted := ops.decryptPassword(*instance.RootPassword); decrypted != "" {
			rootPassword = decrypted
		}
	}

	runErr := func() error {
		if err := ops.updateProgress(ctx, task.ID, "checking_capacity"); err != nil {
			return err
		}
		ownerID := targetHost.UserID
		if pkg.UserID != 0 {
			ownerID = pkg.UserID
		}
		reservedHost, err := ops.selectAndReserve(ctx, db.HostsReserveOptions{
			PackageHostIDs: pkg.HostIDs,
			CPU:            int64(instance.CPU),
			Memory:         int64(instance.Memory),
			Disk:           int64(instance.Disk),
			HostID:         &targetHostID,
			OwnerID:        &ownerID,
			PortCount:      0,
			PackageID:      instance.PackageID,
		})
		if err != nil {
			return err
		}
		if reservedHost == nil {
			return errors.New("目标节点 CPU 或内存容量不足")
		}
		targetResourcesReserved = true

		if success, message := collectTrafficForRunningInstance(ctx, task.InstanceID); !success {
			log.Printf("[ChangeHost] 实例 %d 改节点前即时采集流量失败: %s", task.InstanceID, message)
		}
		if closed := lib.CloseInstanceSessions(task.InstanceID, "Instance is changing host"); closed > 0 {
			log.Printf("[ChangeHost] Closed %d terminal session(s) for instance %d", closed, task.InstanceID)
		}

		if err := ops.updateProgress(ctx, task.ID, "allocating_network"); err != nil {
			return err
		}
		// The instance row still points at the old host, so the reservation must be
		// written against the target host's ip_addresses pool.
		staticIPv4, err := ops.allocateNatIPv4(ctx, targetHostID, &task.InstanceID)
		if err != nil {
			return err
		}
		if staticIPv4 == nil {
			return errors.New("IPv4 地址池已耗尽，无法分配新地址")
		}

		var staticIPv6 *string
		needsIPv6 := needsRoutedIPv6(instance.NetworkMode)
		if needsIPv6 && targetHost.IPv6Subnet != nil && *targetHost.IPv6Subnet != "" {
			for attempt := 0; attempt < instanceTaskMaxIPv6Attempts; attempt++ {
				candidate, genErr := ops.randomIPv6(*targetHost.IPv6Subnet)
				if genErr != nil {
					return genErr
				}
				exists, existsErr := ops.ipv6Exists(ctx, candidate)
				if existsErr != nil {
					return existsErr
				}
				if !exists {
					value := candidate
					staticIPv6 = &value
					break
				}
			}
			if staticIPv6 == nil {
				return errors.New("IPv6 地址池已耗尽，无法分配新地址")
			}
		}

		instanceType := "container"
		if packageInstanceType == "vm" {
			instanceType = "virtual-machine"
		}
		newIncusID = ops.newIncusID(instance.UserID)

		generated, err := lib.GenerateIncusConfig(lib.IncusConfigParams{
			InstanceName: instance.Name,
			ImageAlias:   imageAlias,
			RootPassword: rootPassword,
			SSHKey:       &keyRecord.PublicKey,
			NetworkMode:  string(instance.NetworkMode),
			Type:         instanceType,
			Network: &lib.IncusConfigStaticNetwork{
				IPAddress:   *staticIPv4 + "/22",
				Gateway:     "10.10.0.1",
				DNS:         []string{"10.10.0.1"},
				IPv6Address: instanceTaskCIDRv6(staticIPv6, true),
				IPv6Gateway: instanceTaskIPv6Gateway(staticIPv6, "fe80::1"),
			},
		})
		if err != nil {
			return err
		}
		configPayload := generated.ConfigPayload

		if instanceType == "virtual-machine" {
			vmResult, err := lib.IncusConfigGenerateVM(lib.IncusConfigVMParams{
				InstanceName:   instance.Name,
				InstanceIDSeed: newIncusID,
				ImageAlias:     imageAlias,
				RootPassword:   rootPassword,
				SSHKey:         &keyRecord.PublicKey,
				Network: &lib.IncusConfigStaticNetwork{
					IPAddress:   *staticIPv4 + "/22",
					Gateway:     "10.10.0.1",
					DNS:         []string{"10.10.0.1"},
					IPv6Address: instanceTaskDeref(staticIPv6),
					IPv6Gateway: instanceTaskHostIPv6Gateway(staticIPv6, targetHost.IPv6Gateway),
				},
			})
			if err != nil {
				return err
			}
			configPayload = vmResult
		}

		storagePool := reservedHost.StoragePoolName
		if storagePool == nil || *storagePool == "" {
			return errors.New("目标节点未配置可用于实例系统盘的存储池")
		}

		ioMode := "throughput"
		if pkg.IOLimitMode != "" {
			ioMode = pkg.IOLimitMode
		}
		var ipv6Config *lib.NetworkPayloadIPv6Config
		if staticIPv6 != nil {
			ipv6Config = &lib.NetworkPayloadIPv6Config{PrimaryIP: *staticIPv6}
		}
		hostInterface := "eth0"
		if targetHost.IPv6ParentInterface != nil && *targetHost.IPv6ParentInterface != "" {
			hostInterface = *targetHost.IPv6ParentInterface
		}
		swapEnabled := instance.SwapEnabled
		var limitsRead, limitsWrite *string
		var limitsReadIops, limitsWriteIops *int32
		if ioMode == "throughput" {
			limitsRead = instance.LimitsRead
			limitsWrite = instance.LimitsWrite
		} else {
			limitsReadIops = instance.LimitsReadIops
			limitsWriteIops = instance.LimitsWriteIops
		}
		incusConfig, err := lib.IncusInstanceBuildConfig(lib.IncusInstanceConfigOptions{
			Name:                    newIncusID,
			Image:                   imageAlias,
			CPU:                     int(instance.CPU),
			Memory:                  int(instance.Memory),
			Disk:                    int(instance.Disk),
			SwapEnabled:             &swapEnabled,
			SwapSize:                instance.SwapSize,
			CloudInitConfig:         &configPayload,
			NetworkMode:             string(instance.NetworkMode),
			Nested:                  pkg.Nested != 0,
			Privileged:              pkg.Privileged != 0,
			InstanceType:            instanceTaskIncusType(instanceType),
			StoragePool:             storagePool,
			IPv4Address:             staticIPv4,
			IPv6Config:              ipv6Config,
			HostInterface:           &hostInterface,
			IPv6Address:             staticIPv6,
			IPv6Gateway:             targetHost.IPv6Gateway,
			LimitsRead:              limitsRead,
			LimitsWrite:             limitsWrite,
			LimitsReadIops:          limitsReadIops,
			LimitsWriteIops:         limitsWriteIops,
			LimitsIngress:           instance.LimitsIngress,
			LimitsEgress:            instance.LimitsEgress,
			LimitsProcesses:         instance.LimitsProcesses,
			LimitsCPUPriority:       instance.LimitsCPUPriority,
			BootAutostart:           instance.BootAutostart,
			BootAutostartPriority:   instance.BootAutostartPriority,
			BootAutostartDelay:      instance.BootAutostartDelay,
			BootHostShutdownTimeout: instance.BootHostShutdownTimeout,
		})
		if err != nil {
			return err
		}

		if err := ops.updateProgress(ctx, task.ID, "creating_on_target"); err != nil {
			return err
		}
		log.Printf("[ChangeHost] 在目标节点 %s 创建实例 %s...", targetHost.Name, newIncusID)
		if err := ops.incusCreate(ctx, rt.Client, incusConfig, &task.InstanceID); err != nil {
			return err
		}

		if err := ops.updateProgress(ctx, task.ID, "starting_target"); err != nil {
			return err
		}
		if _, err := ops.incusStart(ctx, rt.Client, newIncusID); err != nil {
			return err
		}

		actualIPv4 := staticIPv4
		actualIPv6 := staticIPv6
		maxAttempts, pollInterval := 30, 2*time.Second
		if instanceType == "virtual-machine" {
			maxAttempts, pollInterval = 60, 3*time.Second
		}
		for attempt := 0; attempt < maxAttempts; attempt++ {
			if err := ops.sleep(ctx, pollInterval); err != nil {
				return err
			}
			state, stateErr := ops.incusGetState(ctx, rt.Client, newIncusID)
			if stateErr != nil {
				continue
			}
			if value := extractIPv4(state); value != nil {
				actualIPv4 = value
			}
			if needsIPv6 {
				if value := extractIPv6(state); value != nil {
					actualIPv6 = value
				}
			}
			if needsIPv6 {
				if actualIPv4 != nil && actualIPv6 != nil {
					break
				}
			} else if actualIPv4 != nil {
				break
			}
		}

		if err := ops.updateProgress(ctx, task.ID, "switching_database"); err != nil {
			return err
		}

		proxySites, err := ops.listProxySites(ctx, task.InstanceID)
		if err != nil {
			return err
		}
		var baseTrafficLimit *int64
		if pkg.MonthlyTrafficLimit != nil && *pkg.MonthlyTrafficLimit != "" {
			if parsed, parseErr := strconv.ParseInt(*pkg.MonthlyTrafficLimit, 10, 64); parseErr == nil {
				value := parsed
				baseTrafficLimit = &value
			}
		}
		if instance.PackagePlanID != nil {
			plan, planErr := ops.getPlan(ctx, *instance.PackagePlanID)
			if planErr != nil {
				return planErr
			}
			if plan != nil && instance.PackageID != nil && plan.PackageID == *instance.PackageID {
				value := plan.TrafficLimit
				baseTrafficLimit = &value
			}
		}

		encryptedPassword, err := ops.encryptPassword(rootPassword)
		if err != nil {
			return err
		}
		if err := ops.switchHost(ctx, task.InstanceID, instanceTaskHostSwitch{
			TargetHostID:     targetHostID,
			IncusID:          newIncusID,
			RootPassword:     encryptedPassword,
			IPv4:             actualIPv4,
			IPv6:             actualIPv6,
			StoragePool:      storagePool,
			PackageID:        instance.PackageID,
			BaseTrafficLimit: baseTrafficLimit,
		}); err != nil {
			return err
		}
		dbSwitched = true

		if len(proxySites) > 0 && sourceHost.CaddyEnabled {
			domains := make([]string, 0, len(proxySites))
			for _, site := range proxySites {
				domains = append(domains, site.Domain)
			}
			if err := ops.deleteCaddySite(ctx, sourceHost, domains); err != nil {
				// The TypeScript lets a client-construction failure escape here: the
				// database is already switched, so the task fails after the move.
				return err
			}
		}

		if err := ops.updateProgress(ctx, task.ID, "deleting_old"); err != nil {
			return err
		}
		sourceClient, err := ops.getClientForHost(ctx, sourceHost)
		if err != nil {
			// Mirrors the outer try/catch: the client error is handled by the
			// "source instance not released" branch below.
			log.Printf("[ChangeHost] 获取源节点客户端失败: %v", err)
		} else {
			allowCancel := isAlpineImageAlias(&instance.Image)
			if _, stopErr := ops.incusStop(ctx, sourceClient, oldIncusID, true, busyStopOptions(allowCancel)); stopErr != nil {
				log.Printf("[ChangeHost] 停止源实例 %s 失败，继续尝试删除: %v", oldIncusID, stopErr)
			}
			if _, deleteErr := ops.incusDelete(ctx, sourceClient, oldIncusID); deleteErr != nil {
				if instanceTaskNotFound(deleteErr) {
					log.Printf("[ChangeHost] 源实例 %s 已不存在", oldIncusID)
					if err := instanceTaskReleaseSourceHost(ctx, ops, instance, sourceHostID, sourceHost, task.InstanceID); err != nil {
						log.Printf("[ChangeHost] 更新节点资源失败: %v", err)
					}
				} else {
					log.Printf("[ChangeHost] 源实例 %s 删除失败，需要人工清理: %s", oldIncusID, instanceTaskErrorText(deleteErr))
					if err := instanceTaskReconcileTargetOnly(ctx, ops, targetHostID); err != nil {
						log.Printf("[ChangeHost] 更新节点资源失败: %v", err)
					}
				}
			} else {
				log.Printf("[ChangeHost] 源实例 %s 已删除", oldIncusID)
				if err := instanceTaskReleaseSourceHost(ctx, ops, instance, sourceHostID, sourceHost, task.InstanceID); err != nil {
					log.Printf("[ChangeHost] 更新节点资源失败: %v", err)
				}
			}
		}

		reconcileTrafficState(ctx, []int32{task.InstanceID})

		notifyData := lib.EventData{
			"instanceName": instance.Name,
			"oldHostName":  sourceHost.Name,
			"newHostName":  targetHost.Name,
		}
		if actualIPv4 != nil && *actualIPv4 != "" {
			notifyData["ipv4"] = *actualIPv4
		}
		if actualIPv6 != nil && *actualIPv6 != "" {
			notifyData["ipv6"] = *actualIPv6
		}
		notifySafely("change host notification", func() {
			ops.sendNotification(ctx, instance.UserID, lib.EventInstanceHostChanged, notifyData)
		})
		ops.createLog(ctx, task.UserID, "instance.change_host",
			fmt.Sprintf("Changed instance \"%s\" host from \"%s\" to \"%s\" (%s -> %s)",
				instance.Name, sourceHost.Name, targetHost.Name, oldIncusID, newIncusID),
			"success", task.InstanceID)
		return nil
	}()

	if runErr == nil {
		return nil
	}

	if !dbSwitched {
		targetInstanceReleased := newIncusID == ""
		if newIncusID != "" {
			targetInstanceReleased = false
			if _, stopErr := ops.incusStop(ctx, rt.Client, newIncusID, true, lib.IncusInstanceBusyRetryOptions{}); stopErr != nil {
				log.Printf("[ChangeHost] 停止目标节点新实例 %s 失败: %v", newIncusID, stopErr)
			}
			if _, deleteErr := ops.incusDelete(ctx, rt.Client, newIncusID); deleteErr != nil {
				if instanceTaskNotFound(deleteErr) {
					targetInstanceReleased = true
				} else {
					log.Printf("[ChangeHost] 清理目标节点新实例 %s 失败: %v", newIncusID, deleteErr)
				}
			} else {
				targetInstanceReleased = true
			}
		}
		if targetResourcesReserved {
			if targetInstanceReleased {
				if err := ops.rollbackResources(ctx, db.RollbackResourcesOptions{
					HostID:    targetHostID,
					CPU:       instance.CPU,
					Memory:    instance.Memory,
					Disk:      instance.Disk,
					PortCount: 0,
				}); err != nil {
					log.Printf("[ChangeHost] 回滚目标节点资源失败: %v", err)
				}
			} else {
				log.Printf("[ChangeHost] 目标实例 %s 未确认清理，暂不释放目标节点 %s 的资源预占", newIncusID, targetHost.Name)
			}
		}
	}
	return runErr
}

// instanceTaskReleaseSourceHost mirrors the "sourceInstanceReleased" resource
// reconciliation block: release the old host's capacity, then recompute both
// hosts' counters as max(calculated, current) so a concurrent reservation is never
// lost.
func instanceTaskReleaseSourceHost(
	ctx context.Context,
	ops *instanceTaskHeavySeams,
	instance *db.InstanceDetailRecord,
	sourceHostID int32,
	sourceHost *db.HostsRecord,
	instanceID int32,
) error {
	deletedPorts, err := ops.countDeletedPortsForInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	if err := ops.rollbackResources(ctx, db.RollbackResourcesOptions{
		HostID:    sourceHostID,
		CPU:       instance.CPU,
		Memory:    instance.Memory,
		Disk:      instance.Disk,
		PortCount: deletedPorts,
	}); err != nil {
		return err
	}
	return instanceTaskReconcileBothHosts(ctx, ops, sourceHostID, instance.HostID)
}

// instanceTaskReconcileBothHosts mirrors the Promise.all block that updates both
// hosts' resource counters plus the source host's NAT port count.
func instanceTaskReconcileBothHosts(ctx context.Context, ops *instanceTaskHeavySeams, sourceHostID, targetHostID int32) error {
	actualSourcePorts, err := ops.countSourcePorts(ctx, sourceHostID)
	if err != nil {
		return err
	}
	sourceCalculated, err := ops.calculateHostRes(ctx, sourceHostID)
	if err != nil {
		return err
	}
	targetCalculated, err := ops.calculateHostRes(ctx, targetHostID)
	if err != nil {
		return err
	}
	sourceHost, err := ops.getHost(ctx, sourceHostID)
	if err != nil {
		return err
	}
	targetHost, err := ops.getHost(ctx, targetHostID)
	if err != nil {
		return err
	}
	sourceResources := db.HostsResourceUpdate{
		CPUUsed:    instanceTaskMaxInt64(sourceCalculated.CPUUsed, instanceTaskHostCPU(sourceHost)),
		MemoryUsed: instanceTaskMaxInt64(sourceCalculated.MemoryUsed, instanceTaskHostMemory(sourceHost)),
		DiskUsed:   instanceTaskMaxInt64(sourceCalculated.DiskUsed, instanceTaskHostDisk(sourceHost)),
	}
	targetResources := db.HostsResourceUpdate{
		CPUUsed:    instanceTaskMaxInt64(targetCalculated.CPUUsed, instanceTaskHostCPU(targetHost)),
		MemoryUsed: instanceTaskMaxInt64(targetCalculated.MemoryUsed, instanceTaskHostMemory(targetHost)),
		DiskUsed:   instanceTaskMaxInt64(targetCalculated.DiskUsed, instanceTaskHostDisk(targetHost)),
	}
	if err := ops.updateHostRes(ctx, sourceHostID, sourceResources); err != nil {
		return err
	}
	if err := ops.updateHostRes(ctx, targetHostID, targetResources); err != nil {
		return err
	}
	sourcePorts := actualSourcePorts
	if sourceHost != nil {
		if current, ok := instanceTaskHostNATPorts(ctx, ops, sourceHostID); ok && current > sourcePorts {
			sourcePorts = current
		}
	}
	return ops.setHostNATPorts(ctx, sourceHostID, sourcePorts)
}

// instanceTaskReconcileTargetOnly mirrors the "source instance not confirmed
// deleted" branch: it only refreshes the target host's counters.
func instanceTaskReconcileTargetOnly(ctx context.Context, ops *instanceTaskHeavySeams, targetHostID int32) error {
	calculated, err := ops.calculateHostRes(ctx, targetHostID)
	if err != nil {
		return err
	}
	current, err := ops.getHost(ctx, targetHostID)
	if err != nil {
		return err
	}
	return ops.updateHostRes(ctx, targetHostID, db.HostsResourceUpdate{
		CPUUsed:    instanceTaskMaxInt64(calculated.CPUUsed, instanceTaskHostCPU(current)),
		MemoryUsed: instanceTaskMaxInt64(calculated.MemoryUsed, instanceTaskHostMemory(current)),
		DiskUsed:   instanceTaskMaxInt64(calculated.DiskUsed, instanceTaskHostDisk(current)),
	})
}

func instanceTaskMaxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func instanceTaskHostCPU(host *db.HostsRecord) int64 {
	if host == nil {
		return 0
	}
	return int64(host.CPUUsed)
}

func instanceTaskHostMemory(host *db.HostsRecord) int64 {
	if host == nil {
		return 0
	}
	return int64(host.MemoryUsed)
}

func instanceTaskHostDisk(host *db.HostsRecord) int64 {
	if host == nil {
		return 0
	}
	return int64(host.DiskUsed)
}

// instanceTaskHostNATPorts reads the host's current NAT port counter through the
// same projection the seams use (the record carries no such column).
func instanceTaskHostNATPorts(ctx context.Context, ops *instanceTaskHeavySeams, hostID int32) (int32, bool) {
	return ops.hostNATPorts(ctx, hostID)
}

func instanceTaskCIDRv6(value *string, withSuffix bool) string {
	if value == nil || *value == "" {
		return ""
	}
	if withSuffix {
		return *value + "/128"
	}
	return *value
}

func instanceTaskIPv6Gateway(value *string, gateway string) string {
	if value == nil || *value == "" {
		return ""
	}
	return gateway
}

func instanceTaskHostIPv6Gateway(value *string, hostGateway *string) string {
	if value == nil || *value == "" || hostGateway == nil {
		return ""
	}
	return *hostGateway
}

func instanceTaskDeref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func containsInt32(values []int32, target int32) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// ==================== clone ====================

// executeInstanceCloneTask is the port of executeCloneTask: an Incus copy of the
// source instance, fresh public ports and private addresses, a new instance row,
// and resource reservation. Incus copies a stopped instance, so the clone is left
// stopped (matches `updateInstanceStatus(newInstanceId, 'stopped')`).
func executeInstanceCloneTask(ctx context.Context, rt *InstanceTaskRuntime) error {
	ops := instanceTaskHeavy()
	task, sourceInstance, host := rt.Task, rt.Instance, rt.Host

	if err := ops.updateProgress(ctx, task.ID, "cloning"); err != nil {
		return err
	}

	newInstanceName := sourceInstance.Name + "-clone"
	newIncusID := ops.newIncusID(task.UserID)

	sourcePortMappings, err := ops.listPorts(ctx, task.InstanceID)
	if err != nil {
		return err
	}

	resourcesReserved := false
	newPortMappingsCount := int32(0)

	runErr := func() error {
		// 1. Copy the instance (Incus copy API, 5 minute budget).
		copyPayload := map[string]any{
			"name": newIncusID,
			"source": map[string]any{
				"type":   "copy",
				"source": sourceInstance.IncusID,
			},
		}
		if err := ops.incusCopy(ctx, rt.Client, copyPayload); err != nil {
			return err
		}

		// 2. Rewrite every proxy device with a freshly allocated public port.
		newInstanceInfo, err := ops.incusGetInstance(ctx, rt.Client, newIncusID)
		if err != nil {
			return err
		}
		devices := newInstanceInfo.Devices
		if devices == nil {
			devices = map[string]map[string]any{}
		}
		updatedDevices := map[string]map[string]any{}
		newPortMappings := []instanceTaskClonePort{}
		selectedPorts := map[models.Protocol]map[int32]struct{}{
			models.ProtocolTcp: {},
			models.ProtocolUdp: {},
		}

		for _, deviceName := range instanceTaskSortDeviceNames(devices) {
			device := devices[deviceName]
			if device == nil {
				continue
			}
			deviceType, _ := device["type"].(string)
			listen, _ := device["listen"].(string)
			if deviceType != "proxy" || listen == "" {
				updatedDevices[deviceName] = device
				continue
			}
			listenMatch := instanceTaskListenPattern.FindStringSubmatch(listen)
			if listenMatch == nil {
				updatedDevices[deviceName] = device
				continue
			}
			protocol := models.Protocol(listenMatch[1])
			listenPort, _ := strconv.ParseInt(listenMatch[3], 10, 32)
			connect, _ := device["connect"].(string)
			privatePort := int32(80)
			if connectMatch := instanceTaskListenPattern.FindStringSubmatch(connect); connectMatch != nil {
				if parsed, parseErr := strconv.ParseInt(connectMatch[3], 10, 32); parseErr == nil {
					privatePort = int32(parsed)
				}
			} else if mapping := instanceTaskFindPortMapping(sourcePortMappings, protocol, int32(listenPort)); mapping != nil && mapping.PrivatePort != 0 {
				privatePort = mapping.PrivatePort
			}

			excluded := make([]int32, 0, len(selectedPorts[protocol]))
			for port := range selectedPorts[protocol] {
				excluded = append(excluded, port)
			}
			newPublicPort, ok, err := ops.allocatePort(ctx, host.ID, protocol, excluded)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("No available port for clone operation")
			}
			selectedPorts[protocol][newPublicPort] = struct{}{}

			fullHost, err := ops.getHost(ctx, host.ID)
			if err != nil {
				return err
			}
			var bindIP, publicIP, hostURL, hostIPAddress *string
			if fullHost != nil {
				bindIP = fullHost.NATBindIP
				publicIP = fullHost.NATPublicIP
				if fullHost.URL != "" {
					value := fullHost.URL
					hostURL = &value
				}
				hostIPAddress = fullHost.IPAddress
			}
			bindableIPv4 := lib.SelectBindableIpv4ListenAddress(bindIP, publicIP, hostURL, hostIPAddress)
			ipv6Mode := instanceTaskIsIPv6OnlyMode(string(sourceInstance.NetworkMode))
			cloneListenAddr := bindableIPv4
			if ipv6Mode {
				var hostIPv6 *string
				if fullHost != nil {
					hostIPv6 = instanceTaskFirstNonEmpty(fullHost.NATPublicIPv6, fullHost.IPv6Gateway, fullHost.IPAddress)
				}
				if hostIPv6 != nil && strings.Contains(*hostIPv6, ":") {
					cloneListenAddr = "[" + strings.Trim(*hostIPv6, "[]") + "]"
				}
			}
			cloneConnectAddr := "0.0.0.0"
			if ipv6Mode {
				cloneConnectAddr = "[::]"
			}
			proxyConfig := map[string]any{
				"type":    "proxy",
				"listen":  string(protocol) + ":" + cloneListenAddr + ":" + strconv.FormatInt(int64(newPublicPort), 10),
				"connect": string(protocol) + ":" + cloneConnectAddr + ":" + strconv.FormatInt(int64(privatePort), 10),
			}
			if !ipv6Mode {
				proxyConfig["nat"] = "true"
			}
			updatedDevices[deviceName] = proxyConfig

			var remark *string
			if mapping := instanceTaskFindPortMapping(sourcePortMappings, protocol, int32(listenPort)); mapping != nil {
				remark = mapping.Remark
			}
			newPortMappings = append(newPortMappings, instanceTaskClonePort{
				Protocol:    protocol,
				PublicPort:  newPublicPort,
				PrivatePort: privatePort,
				Remark:      remark,
			})
		}

		// 3. Fresh private IPv4. The instance row does not exist yet, so this is a
		// pure reservation; the address is written into eth0 before creation.
		newIPv4, err := ops.allocateNatIPv4(ctx, host.ID, nil)
		if err != nil {
			log.Printf("[Clone] IPv4 分配失败: %v", err)
			return err
		}
		if newIPv4 == nil {
			return errors.New("Failed to allocate IPv4 address, pool may be exhausted")
		}
		log.Printf("[Clone] 为新实例分配并预留内网 IPv4: %s", *newIPv4)
		if eth0, ok := updatedDevices["eth0"]; ok && eth0 != nil {
			eth0["ipv4.address"] = *newIPv4
		} else if original, ok := devices["eth0"]; ok && original != nil {
			copied := instanceTaskCopyDevice(original)
			copied["ipv4.address"] = *newIPv4
			updatedDevices["eth0"] = copied
		}

		// 4. Routed IPv6: a new address is mandatory (the copy would keep the old one).
		var newIPv6 *string
		if string(sourceInstance.NetworkMode) != "nat" && host.IPv6Subnet != nil && *host.IPv6Subnet != "" {
			if err := func() error {
				for attempt := 0; attempt < instanceTaskMaxIPv6Attempts; attempt++ {
					candidate, genErr := ops.randomIPv6(*host.IPv6Subnet)
					if genErr != nil {
						return genErr
					}
					exists, existsErr := ops.ipv6Exists(ctx, candidate)
					if existsErr != nil {
						return existsErr
					}
					if !exists {
						value := candidate
						newIPv6 = &value
						log.Printf("[Clone] 为新实例随机分配 IPv6: %s (尝试次数: %d)", value, attempt+1)
						break
					}
				}
				if newIPv6 == nil {
					return fmt.Errorf("无法为新实例找到可用的 IPv6 地址，已尝试 %d 次随机生成", instanceTaskMaxIPv6Attempts)
				}
				if eth1, ok := updatedDevices["eth1"]; ok && eth1 != nil {
					eth1["ipv6.address"] = *newIPv6
				} else if original, ok := devices["eth1"]; ok && original != nil {
					copied := instanceTaskCopyDevice(original)
					copied["ipv6.address"] = *newIPv6
					updatedDevices["eth1"] = copied
				} else {
					parent := "eth0"
					if host.IPv6ParentInterface != nil && *host.IPv6ParentInterface != "" {
						parent = *host.IPv6ParentInterface
					}
					updatedDevices["eth1"] = map[string]any{
						"type":                    "nic",
						"nictype":                 "routed",
						"parent":                  parent,
						"name":                    "eth1",
						"ipv6.address":            *newIPv6,
						"security.ipv6_filtering": "true",
						"security.mac_filtering":  "true",
					}
				}
				// The guest needs the new addresses in its cloud-init network-config.
				networkConfig := lib.GenerateCloudInitNetworkConfig(*newIPv4, []string{*newIPv6})
				if _, err := ops.incusUpdate(ctx, rt.Client, newIncusID, map[string]any{
					"config": map[string]string{"user.network-config": networkConfig},
				}); err != nil {
					return err
				}
				log.Printf("[Clone] 生成 network-config: IPv4=%s, IPv6=%s", *newIPv4, *newIPv6)
				return nil
			}(); err != nil {
				// The TypeScript swallows IPv6 failures here and continues with
				// newIPv6 = null; the device change it already made stays.
				log.Printf("[Clone] IPv6 计算失败: %v", err)
				newIPv6 = nil
			}
		}

		// 4b. Apply the device rewrite.
		if len(updatedDevices) > 0 {
			if _, err := ops.incusUpdate(ctx, rt.Client, newIncusID, map[string]any{"devices": updatedDevices}); err != nil {
				return err
			}
		}

		// 4c. NAT-only clone still needs the IPv4 network-config.
		if newIPv6 == nil {
			networkConfig := lib.GenerateCloudInitNetworkConfig(*newIPv4, nil)
			if _, err := ops.incusUpdate(ctx, rt.Client, newIncusID, map[string]any{
				"config": map[string]string{"user.network-config": networkConfig},
			}); err != nil {
				return err
			}
			log.Printf("[Clone] 生成 network-config (NAT 模式): IPv4=%s", *newIPv4)
		}

		// 5. Package snapshot.
		if sourceInstance.PackageID == nil {
			return errors.New("Package not found for source instance")
		}
		pkg, err := ops.getPackage(ctx, *sourceInstance.PackageID)
		if err != nil {
			return err
		}
		if pkg == nil {
			return errors.New("Package not found for source instance")
		}

		// 6. Instance row.
		nodeSelectors, err := ops.getNodeSelectors(ctx, pkg.ID)
		if err != nil {
			return err
		}
		portLimit := int32(20)
		if sourceInstance.PortLimit != nil && *sourceInstance.PortLimit != 0 {
			portLimit = *sourceInstance.PortLimit
		}
		snapshotSpecs := map[string]any{
			"packageId":    pkg.ID,
			"packageName":  pkg.Name,
			"cpuMax":       pkg.CPUMax,
			"memoryMax":    pkg.MemoryMax,
			"diskMax":      pkg.DiskMax,
			"networkMode":  string(sourceInstance.NetworkMode),
			"portLimit":    portLimit,
			"nested":       pkg.Nested != 0,
			"privileged":   pkg.Privileged != 0,
			"nodeSelectors": nodeSelectors,
			"createdAt":    instanceTaskISOString(ops.now()),
		}
		specsJSON, err := json.Marshal(snapshotSpecs)
		if err != nil {
			return err
		}

		var baseTrafficLimit *int64
		if pkg.MonthlyTrafficLimit != nil && *pkg.MonthlyTrafficLimit != "" {
			if parsed, parseErr := strconv.ParseInt(*pkg.MonthlyTrafficLimit, 10, 64); parseErr == nil {
				value := parsed
				baseTrafficLimit = &value
			}
		}
		if sourceInstance.PackagePlanID != nil {
			plan, planErr := ops.getPlan(ctx, *sourceInstance.PackagePlanID)
			if planErr != nil {
				return planErr
			}
			if plan != nil && plan.PackageID == *sourceInstance.PackageID {
				value := plan.TrafficLimit
				baseTrafficLimit = &value
			}
		}
		monthlyTrafficLimit, err := ops.resolveTraffic(ctx, nil, lib.TrafficMultiplierResolveParams{
			PackageID:        sourceInstance.PackageID,
			HostID:           sourceInstance.HostID,
			BaseTrafficLimit: baseTrafficLimit,
		})
		if err != nil {
			return err
		}

		sshPort := int32(instanceTaskRecreateSSHPort)
		swapEnabled := sourceInstance.SwapEnabled
		newInstanceID, err := ops.createInstanceRow(ctx, db.InstanceCreateInput{
			EgressSourceInstanceID: &task.InstanceID,
			IncusID:                newIncusID,
			Name:                   newInstanceName,
			UserID:                 task.UserID,
			HostID:                 sourceInstance.HostID,
			PackageID:              sourceInstance.PackageID,
			StoragePoolName:        sourceInstance.StoragePoolName,
			Image:                  sourceInstance.Image,
			CPU:                    sourceInstance.CPU,
			Memory:                 sourceInstance.Memory,
			Disk:                   sourceInstance.Disk,
			NetworkMode:            sourceInstance.NetworkMode,
			SnapshotSpecs:          specsJSON,
			SSHPort:                &sshPort,
			RootPassword:           sourceInstance.RootPassword,
			MonthlyTrafficLimit:    monthlyTrafficLimit,
			PortLimit:              sourceInstance.PortLimit,
			SnapshotLimit:          sourceInstance.SnapshotLimit,
			BackupLimit:            sourceInstance.BackupLimit,
			SiteLimit:              sourceInstance.SiteLimit,
			SwapEnabled:            &swapEnabled,
			SwapSize:               sourceInstance.SwapSize,
		})
		if err != nil {
			return err
		}

		// 8. Port mapping rows.
		for _, mapping := range newPortMappings {
			if _, err := ops.createPort(ctx, db.PortMappingCreateInput{
				InstanceID:  newInstanceID,
				HostID:      sourceInstance.HostID,
				Protocol:    mapping.Protocol,
				PublicPort:  mapping.PublicPort,
				PrivatePort: mapping.PrivatePort,
				Remark:      mapping.Remark,
			}); err != nil {
				return err
			}
		}

		// 9. IP rows + denormalized instance columns.
		if _, err := ops.createIPRecord(ctx, db.IpAddressesCreateData{
			Address:    *newIPv4,
			Type:       models.IpTypeInet4,
			IsPrimary:  true,
			Device:     "eth0",
			InstanceID: newInstanceID,
		}); err != nil {
			return err
		}
		if err := ops.setInstanceIPv4(ctx, newInstanceID, *newIPv4); err != nil {
			return err
		}
		log.Printf("[Clone] 新实例 %d IPv4 已更新: %s (device: eth0)", newInstanceID, *newIPv4)

		if newIPv6 != nil {
			if _, err := ops.createIPRecord(ctx, db.IpAddressesCreateData{
				Address:    *newIPv6,
				Type:       models.IpTypeInet6,
				IsPrimary:  true,
				Device:     "eth1",
				InstanceID: newInstanceID,
			}); err != nil {
				return err
			}
			if err := ops.setInstanceIPv6(ctx, newInstanceID, *newIPv6); err != nil {
				return err
			}
			log.Printf("[Clone] 新实例 %d IPv6 已更新: %s (device: eth1)", newInstanceID, *newIPv6)
		}

		// 10. Reserve host capacity (the disk is a copy, already allocated).
		newPortMappingsCount = int32(len(newPortMappings))
		if err := ops.reserveResources(ctx, db.ReserveResourcesOptions{
			HostID:               sourceInstance.HostID,
			CPU:                  sourceInstance.CPU,
			Memory:               sourceInstance.Memory,
			Disk:                 sourceInstance.Disk,
			PortCount:            newPortMappingsCount,
			StoragePoolName:      sourceInstance.StoragePoolName,
			DiskAlreadyAllocated: true,
		}); err != nil {
			return err
		}
		resourcesReserved = true

		// 11. A copy is stopped.
		if err := ops.setInstanceStopped(ctx, newInstanceID); err != nil {
			return err
		}
		if err := ops.setTaskNewInstanceID(ctx, task.ID, newInstanceID); err != nil {
			return err
		}

		notifySafely("clone notification", func() {
			ops.sendNotification(ctx, task.UserID, lib.EventInstanceCloned, lib.EventData{
				"instanceName":    sourceInstance.Name,
				"newInstanceName": newInstanceName,
				"hostName":        host.Name,
			})
		})
		ops.createLog(ctx, task.UserID, "instance.clone",
			fmt.Sprintf("Cloned instance \"%s\" to \"%s\" [new instance ID: %d]", sourceInstance.Name, newInstanceName, newInstanceID),
			"success", newInstanceID)
		return nil
	}()

	if runErr == nil {
		return nil
	}

	// Compensation: release the remote copy, then the row, then the reservation —
	// each step only when the previous one is confirmed.
	incusCloneReleased := false
	if _, deleteErr := ops.incusDelete(ctx, rt.Client, newIncusID); deleteErr != nil {
		if instanceTaskNotFound(deleteErr) {
			incusCloneReleased = true
		} else {
			log.Printf("[Clone] 清理失败 Incus 实例失败，保留数据库记录和资源占用以便人工处理: %s %v", newIncusID, deleteErr)
		}
	} else {
		log.Printf("[Clone] 已清理失败的 Incus 实例: %s", newIncusID)
		incusCloneReleased = true
	}

	cloneDBRecordExists := false
	cloneDBRecordRemoved := false
	existingID, findErr := ops.findInstanceByIncusID(ctx, newIncusID)
	if findErr != nil {
		log.Printf("[Clone] 清理 Instance 记录失败: %v", findErr)
	} else if existingID != 0 {
		cloneDBRecordExists = true
		if incusCloneReleased {
			if err := ops.deleteInstanceRow(ctx, existingID); err != nil {
				log.Printf("[Clone] 清理 Instance 记录失败: %v", err)
			} else {
				cloneDBRecordRemoved = true
				log.Printf("[Clone] 已清理失败的 Instance 记录: %d", existingID)
			}
		} else {
			if err := ops.markInstanceError(ctx, existingID); err != nil {
				log.Printf("[Clone] 清理 Instance 记录失败: %v", err)
			} else {
				log.Printf("[Clone] Incus 克隆实例 %s 未确认清理，数据库实例 %d 已标记为 error", newIncusID, existingID)
			}
		}
	}

	if resourcesReserved {
		if incusCloneReleased && (!cloneDBRecordExists || cloneDBRecordRemoved) {
			if err := ops.rollbackResources(ctx, db.RollbackResourcesOptions{
				HostID:    sourceInstance.HostID,
				CPU:       sourceInstance.CPU,
				Memory:    sourceInstance.Memory,
				Disk:      sourceInstance.Disk,
				PortCount: newPortMappingsCount,
			}); err != nil {
				log.Printf("[Clone] 回滚资源失败: %v", err)
			} else {
				log.Printf("[Clone] 已回滚宿主机资源")
			}
		} else {
			log.Printf("[Clone] 克隆实例 %s 未确认完全清理，暂不释放宿主机资源占用", newIncusID)
		}
	}
	return runErr
}

// instanceTaskFindPortMapping mirrors `sourcePortMappings.find(...)`.
func instanceTaskFindPortMapping(mappings []db.PortMappingRecord, protocol models.Protocol, publicPort int32) *db.PortMappingRecord {
	for index := range mappings {
		if mappings[index].Protocol == protocol && mappings[index].PublicPort == publicPort {
			return &mappings[index]
		}
	}
	return nil
}

// instanceTaskCopyDevice clones one Incus device map (the TypeScript spreads it).
func instanceTaskCopyDevice(device map[string]any) map[string]any {
	copied := make(map[string]any, len(device)+1)
	for key, value := range device {
		copied[key] = value
	}
	return copied
}

// instanceTaskIsIPv6OnlyMode mirrors
// `['ipv6_only', 'ipv6_nat'].includes(network_mode)`.
func instanceTaskIsIPv6OnlyMode(networkMode string) bool {
	return networkMode == string(models.NetworkModeIpv6Only) || networkMode == string(models.NetworkModeIpv6Nat)
}

// ==================== default database helpers ====================

// defaultInstanceTaskNodeSelectors mirrors `JSON.parse(pkg.node_selectors || '[]')`
// read as raw JSON so the caller can embed it in the snapshotted spec verbatim.
func defaultInstanceTaskNodeSelectors(ctx context.Context, packageID int32) (json.RawMessage, error) {
	if db.Pool() == nil {
		return nil, errors.New("workers: database not initialized")
	}
	var raw []byte
	if err := db.Pool().QueryRow(ctx, `SELECT node_selectors FROM packages WHERE id = $1`, packageID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, gorm.ErrRecordNotFound) {
			return json.RawMessage("[]"), nil
		}
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("[]"), nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("package node_selectors is not valid JSON")
	}
	return json.RawMessage(raw), nil
}

// defaultInstanceTaskSelectAndReserve mirrors the prisma.$transaction +
// selectAndReserveHostWithLock pair (Serializable, 15 s budget).
func defaultInstanceTaskSelectAndReserve(ctx context.Context, options db.HostsReserveOptions) (*db.HostsSelectionRecord, error) {
	if db.DB == nil {
		return nil, errors.New("workers: database not initialized")
	}
	txCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	tx := db.DB.WithContext(txCtx).Begin(&sql.TxOptions{Isolation: sql.LevelSerializable})
	if tx.Error != nil {
		return nil, tx.Error
	}
	defer func() { _ = tx.Rollback() }()

	host, err := db.HostsSelectAndReserveWithLock(ctx, tx, options)
	if err != nil {
		return nil, err
	}
	if host == nil {
		// A nil selection wrote nothing; rolling back is explicit for clarity.
		_ = tx.Rollback()
		return nil, nil
	}
	if err := tx.Commit().Error; err != nil {
		return nil, err
	}
	return host, nil
}

// defaultInstanceTaskSetHostNATPorts mirrors `host.update({natPortsUsedCount})`.
func defaultInstanceTaskSetHostNATPorts(ctx context.Context, hostID, count int32) error {
	if db.DB == nil {
		return errors.New("workers: database not initialized")
	}
	result := db.DB.WithContext(ctx).Exec(`UPDATE hosts SET nat_ports_used_count = ?, updated_at = ? WHERE id = ?`,
		count, time.Now().UTC(), hostID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("host not found")
	}
	return nil
}

// defaultInstanceTaskCountSourcePorts mirrors
// `prisma.portMapping.count({ where: { instance: { hostId, status: { not: 'deleted' } } } })`.
func defaultInstanceTaskCountSourcePorts(ctx context.Context, hostID int32) (int32, error) {
	if db.Pool() == nil {
		return 0, errors.New("workers: database not initialized")
	}
	var count int32
	err := db.Pool().QueryRow(ctx, `SELECT COUNT(*)::int4 FROM port_mappings pm
		JOIN instances i ON i.id = pm.instance_id
		WHERE i.host_id = $1 AND i.status::text <> 'deleted'`, hostID).Scan(&count)
	return count, err
}

// defaultInstanceTaskSetInstanceIP mirrors the two single-field instance updates
// the clone path performs after creating an ip_addresses row.
func defaultInstanceTaskSetInstanceIP(ctx context.Context, instanceID int32, address string, ipv4 bool) error {
	if db.DB == nil {
		return errors.New("workers: database not initialized")
	}
	column := "ipv6"
	if ipv4 {
		column = "ipv4"
	}
	result := db.DB.WithContext(ctx).Exec(`UPDATE instances SET `+column+` = ?, updated_at = ? WHERE id = ?`,
		address, time.Now().UTC(), instanceID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return db.ErrInstanceRecordNotFound
	}
	return nil
}

// defaultInstanceTaskFindInstanceByIncusID mirrors
// `prisma.instance.findFirst({ where: { incusId } })` → id (0 when absent).
func defaultInstanceTaskFindInstanceByIncusID(ctx context.Context, incusID string) (int32, error) {
	if db.Pool() == nil {
		return 0, errors.New("workers: database not initialized")
	}
	var id int32
	err := db.Pool().QueryRow(ctx, `SELECT id FROM instances WHERE incus_id = $1 LIMIT 1`, incusID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return id, err
}

// defaultInstanceTaskDeleteInstanceRow mirrors `prisma.instance.delete`.
func defaultInstanceTaskDeleteInstanceRow(ctx context.Context, id int32) error {
	if db.Pool() == nil {
		return errors.New("workers: database not initialized")
	}
	tag, err := db.Pool().Exec(ctx, `DELETE FROM instances WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return db.ErrInstanceRecordNotFound
	}
	return nil
}

// defaultInstanceTaskCleanupRecreated mirrors cleanupRecreatedInstanceRecords:
// one transaction that deletes the instance's dependent rows and releases its NAT
// port share from the host counter.
func defaultInstanceTaskCleanupRecreated(ctx context.Context, instanceID, hostID int32) (int32, error) {
	if db.Pool() == nil {
		return 0, errors.New("workers: database not initialized")
	}
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var deletedPorts int32
	if err := tx.QueryRow(ctx, `WITH deleted AS (
		DELETE FROM port_mappings WHERE instance_id = $1 RETURNING 1
	) SELECT COUNT(*)::int4 FROM deleted`, instanceID).Scan(&deletedPorts); err != nil {
		return 0, err
	}

	if deletedPorts > 0 {
		tag, err := tx.Exec(ctx, `UPDATE hosts SET nat_ports_used_count = nat_ports_used_count - $2,
			updated_at = $3 WHERE id = $1 AND nat_ports_used_count >= $2`,
			hostID, deletedPorts, time.Now().UTC())
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			// The host counter had already been reset by another path; Prisma's
			// update() throws when the host row itself is gone, so keep that error.
			tag, err := tx.Exec(ctx, `UPDATE hosts SET nat_ports_used_count = 0, updated_at = $2 WHERE id = $1`,
				hostID, time.Now().UTC())
			if err != nil {
				return 0, err
			}
			if tag.RowsAffected() == 0 {
				return 0, errors.New("host not found")
			}
		}
	}

	for _, statement := range []string{
		`DELETE FROM snapshots WHERE instance_id = $1`,
		`DELETE FROM backups WHERE instance_id = $1`,
		`DELETE FROM proxy_sites WHERE instance_id = $1`,
		`DELETE FROM backup_policies WHERE instance_id = $1`,
		`DELETE FROM snapshot_policies WHERE instance_id = $1`,
		`DELETE FROM traffic_snapshots WHERE instance_id = $1`,
	} {
		if _, err := tx.Exec(ctx, statement, instanceID); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return deletedPorts, nil
}

func defaultInstanceTaskDeleteOldIPv4Rows(ctx context.Context, instanceID int32, address string) error {
	if db.Pool() == nil {
		return errors.New("workers: database not initialized")
	}
	_, err := db.Pool().Exec(ctx, `DELETE FROM ip_addresses
		WHERE instance_id = $1 AND type = 'inet4' AND address = $2`, instanceID, address)
	return err
}

func defaultInstanceTaskDeleteAllIPs(ctx context.Context, instanceID int32) error {
	if db.Pool() == nil {
		return errors.New("workers: database not initialized")
	}
	_, err := db.Pool().Exec(ctx, `DELETE FROM ip_addresses WHERE instance_id = $1`, instanceID)
	return err
}

func defaultInstanceTaskDeleteIPv6Subnets(ctx context.Context, instanceID int32) error {
	if db.Pool() == nil {
		return errors.New("workers: database not initialized")
	}
	_, err := db.Pool().Exec(ctx, `DELETE FROM ipv6_subnets WHERE instance_id = $1`, instanceID)
	return err
}

// defaultInstanceTaskMarkError mirrors the "old instance gone, replacement not
// delivered" terminal state: status error with both addresses cleared.
func defaultInstanceTaskMarkError(ctx context.Context, instanceID int32) error {
	if db.DB == nil {
		return errors.New("workers: database not initialized")
	}
	result := db.DB.WithContext(ctx).Exec(`UPDATE instances
		SET status = 'error', ipv4 = NULL, ipv6 = NULL, updated_at = ?
		WHERE id = ?`, time.Now().UTC(), instanceID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return db.ErrInstanceRecordNotFound
	}
	return nil
}

func defaultInstanceTaskDeleteSnapshotRows(ctx context.Context, instanceID int32) error {
	if db.DB == nil {
		return errors.New("workers: database not initialized")
	}
	return db.DB.WithContext(ctx).Exec(`DELETE FROM snapshots WHERE instance_id = ?`, instanceID).Error
}

// defaultInstanceTaskSwitchRecreated is recreate's commit point.
func defaultInstanceTaskSwitchRecreated(ctx context.Context, instanceID int32, data instanceTaskRecreateSwitch) error {
	if db.DB == nil {
		return errors.New("workers: database not initialized")
	}
	result := db.DB.WithContext(ctx).Exec(`UPDATE instances
		SET incus_id = ?, image = ?, root_password = ?, status = 'running',
		    ipv4 = ?, ipv6 = ?, storage_pool_name = ?, updated_at = ?
		WHERE id = ?`,
		data.IncusID, data.Image, data.RootPassword, data.IPv4, data.IPv6,
		data.StoragePool, time.Now().UTC(), instanceID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return db.ErrInstanceRecordNotFound
	}
	return nil
}

// defaultInstanceTaskSwitchHost is change_host's commit point: dependent rows are
// dropped, the IPv6 ownership is proven under the allocation lock, the instance
// row moves, and the new address rows are written — all in one transaction.
func defaultInstanceTaskSwitchHost(ctx context.Context, instanceID int32, data instanceTaskHostSwitch) error {
	if db.DB == nil {
		return errors.New("workers: database not initialized")
	}
	return db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		monthlyTrafficLimit, err := defaultResolveTrafficTx(ctx, tx, lib.TrafficMultiplierResolveParams{
			PackageID:        data.PackageID,
			HostID:           data.TargetHostID,
			BaseTrafficLimit: data.BaseTrafficLimit,
		})
		if err != nil {
			return err
		}

		var monthlyTrafficUsed int64
		if err := tx.Raw(`SELECT monthly_traffic_used FROM instances WHERE id = ?`, instanceID).
			Row().Scan(&monthlyTrafficUsed); err != nil {
			if errors.Is(err, sql.ErrNoRows) || errors.Is(err, gorm.ErrRecordNotFound) {
				return db.ErrInstanceRecordNotFound
			}
			return err
		}

		for _, statement := range []string{
			`DELETE FROM port_mappings WHERE instance_id = ?`,
			`DELETE FROM snapshots WHERE instance_id = ?`,
			`DELETE FROM backups WHERE instance_id = ?`,
			`DELETE FROM restore_tasks WHERE instance_id = ? AND status IN ('COMPLETED', 'FAILED')`,
			`DELETE FROM backup_upload_tasks WHERE instance_id = ? AND status IN ('COMPLETED', 'FAILED')`,
			`DELETE FROM proxy_sites WHERE instance_id = ?`,
			`DELETE FROM backup_policies WHERE instance_id = ?`,
			`DELETE FROM snapshot_policies WHERE instance_id = ?`,
			`DELETE FROM traffic_snapshots WHERE instance_id = ?`,
			`DELETE FROM ip_addresses WHERE instance_id = ?`,
			`DELETE FROM ipv6_subnets WHERE instance_id = ?`,
		} {
			if err := tx.Exec(statement, instanceID).Error; err != nil {
				return err
			}
		}

		if data.IPv6 != nil && *data.IPv6 != "" {
			if err := db.AdvisoryTransactionLockStringTx(tx, db.IPAddressAllocationLockNamespace, *data.IPv6); err != nil {
				return err
			}
			var conflictingID int32
			err := tx.Raw(`SELECT id FROM instances
				WHERE id <> ? AND status::text <> 'deleted' AND (
					ipv6 = ? OR EXISTS (
						SELECT 1 FROM ip_addresses ia
						WHERE ia.instance_id = instances.id AND ia.address = ? AND ia.type = 'inet6'
					)
				) LIMIT 1`, instanceID, *data.IPv6, *data.IPv6).Row().Scan(&conflictingID)
			if err == nil {
				return fmt.Errorf("IP address %s is already assigned to instance %d", *data.IPv6, conflictingID)
			}
			if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}

		trafficStatus := lib.CalculateInstanceTrafficStatus(monthlyTrafficUsed, monthlyTrafficLimit)
		result := tx.Exec(`UPDATE instances
			SET host_id = ?, incus_id = ?, root_password = ?, status = 'running',
			    ipv4 = ?, ipv6 = ?, storage_pool_name = ?, monthly_traffic_limit = ?,
			    traffic_status = ?, cloud_init_state = NULL, cloud_init_source = NULL,
			    cloud_init_last_checked_at = NULL, cloud_init_completed_at = NULL,
			    cloud_init_manual_completed_at = NULL, cloud_init_manual_completed_by = NULL,
			    updated_at = ?
			WHERE id = ?`,
			data.TargetHostID, data.IncusID, data.RootPassword, data.IPv4, data.IPv6,
			data.StoragePool, monthlyTrafficLimit, trafficStatus, time.Now().UTC(), instanceID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return db.ErrInstanceRecordNotFound
		}

		if data.IPv4 != nil && *data.IPv4 != "" {
			if err := tx.Exec(`INSERT INTO ip_addresses
				(address, type, is_primary, is_custom, device, host_id, instance_id)
				VALUES (?, 'inet4', TRUE, FALSE, 'eth0', ?, ?)`,
				*data.IPv4, data.TargetHostID, instanceID).Error; err != nil {
				return err
			}
		}
		if data.IPv6 != nil && *data.IPv6 != "" {
			if err := tx.Exec(`INSERT INTO ip_addresses
				(address, type, is_primary, is_custom, device, host_id, instance_id)
				VALUES (?, 'inet6', TRUE, FALSE, 'eth1', ?, ?)`,
				*data.IPv6, data.TargetHostID, instanceID).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// defaultResolveTrafficTx resolves the traffic multiplier on the caller's
// transaction (change_host does it inside the switch transaction, like the TS).
func defaultResolveTrafficTx(ctx context.Context, tx *gorm.DB, params lib.TrafficMultiplierResolveParams) (*int64, error) {
	if params.PackageID == nil || *params.PackageID == 0 {
		return params.BaseTrafficLimit, nil
	}
	return lib.ResolveInstanceTrafficLimitForHost(ctx, tx, params)
}

// defaultInstanceTaskListProxySites mirrors
// `prisma.proxySite.findMany({ where: { instanceId } })` projected to domains.
func defaultInstanceTaskListProxySites(ctx context.Context, instanceID int32) ([]instanceTaskProxySite, error) {
	if db.Pool() == nil {
		return nil, errors.New("workers: database not initialized")
	}
	rows, err := db.Pool().Query(ctx, `SELECT domain FROM proxy_sites WHERE instance_id = $1`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sites := []instanceTaskProxySite{}
	for rows.Next() {
		var site instanceTaskProxySite
		if err := rows.Scan(&site.Domain); err != nil {
			return nil, err
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}

// defaultInstanceTaskDeleteCaddySites mirrors the Caddy cleanup block: one client
// per host, per-site failures are warnings (the DB rows are already gone).
func defaultInstanceTaskDeleteCaddySites(ctx context.Context, host *db.HostsRecord, domains []string) error {
	if host == nil {
		return nil
	}
	port := int(host.CaddyPort)
	client, err := lib.GetCaddyClientForHost(lib.CaddyHostRef{
		ID:           host.ID,
		CaddyEnabled: &host.CaddyEnabled,
		CaddyPort:    &port,
	}, 0)
	if err != nil {
		return err
	}
	for _, domain := range domains {
		if err := client.DeleteSite(ctx, domain); err != nil {
			log.Printf("[ChangeHost] 删除 Caddy 站点 %s 失败，数据库记录已清理: %v", domain, err)
		}
	}
	return nil
}
