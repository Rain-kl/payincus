// Port of server/src/routes/admin-host-instance-transfer.ts.
//
// Admin-only surface for moving instances between the panel and a host node:
//
//	GET  /api/admin/hosts/:hostId/instance-transfer                 — list node export/import tasks
//	POST /api/admin/hosts/:hostId/instances/:instanceId/export      — export one instance to a host path
//	GET  /api/admin/hosts/:hostId/import-packages                   — packages usable for import binding
//	POST /api/admin/hosts/:hostId/instances/import                  — start an async import (with preset)
//	GET  /api/admin/hosts/:hostId/import-storage-pools              — target storage pools for import
//	GET  /api/admin/hosts/:hostId/unassigned-instances              — instances on the node not in the panel
//	POST /api/admin/hosts/:hostId/unassigned-instances/:incusName/assign — adopt one to a user
//	PUT  /api/admin/hosts/:hostId/instances/:instanceId/package     — rebind a managed instance's package
//
// The node task polling loop mirrors the TypeScript's 2s setTimeout chain with a
// process-local goroutine; progress is fed to lib.SetOperationTaskProgress so the
// admin task list/SSE shows the same live line.
package routes

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

const (
	transferExt                 = "incus_instance_transfer_v1"
	transferBackupsPath         = "/1.0/agent/instance-backups"
	transferImportPath          = "/1.0/agent/instance-import"
	transferProgressLogInterval = 5 * time.Second
	transferMaxAttempts         = 3600
	transferPollInterval        = 2 * time.Second
)

var (
	transferTaskIDPattern         = regexp.MustCompile(`^[0-9a-f]{32}$`)
	transferTargetPathPattern     = regexp.MustCompile(`^/[^\x00\r\n]+$`)
	transferNamePattern           = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)
	transferFileExtPattern        = regexp.MustCompile(`(?i)\.(tar\.gz|tgz|tar\.zst|tar)$`)
	transferProgressUnitPattern   = regexp.MustCompile(`(?i)[\d.,]+\s*(?:[KMGTPE]?i?B|%)(?:/s)?`)
	transferProgressDigitsPattern = regexp.MustCompile(`\d+`)
)

// transferNodeTask mirrors NodeBackupTask.
type transferNodeTask struct {
	ID        string `json:"id"`
	Kind      string `json:"kind,omitempty"`
	Instance  string `json:"instance,omitempty"`
	FilePath  string `json:"filePath,omitempty"`
	Directory string `json:"directory,omitempty"`
	Status    string `json:"status"`
	Progress  string `json:"progress,omitempty"`
	Error     string `json:"error,omitempty"`
	Items     []struct {
		Name   string `json:"name,omitempty"`
		Status string `json:"status,omitempty"`
		Error  string `json:"error,omitempty"`
		File   string `json:"file,omitempty"`
		Bytes  int64  `json:"bytes,omitempty"`
	} `json:"items,omitempty"`
}

func transferGenerateTaskID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// transferNodeProgressStageKey mirrors getNodeProgressStageKey.
func transferNodeProgressStageKey(progress string) string {
	stripped := transferProgressUnitPattern.ReplaceAllString(progress, "")
	stripped = transferProgressDigitsPattern.ReplaceAllString(stripped, "")
	return strings.TrimSpace(stripped)
}

// transferFormatNodeBackupError mirrors formatNodeBackupError.
func transferFormatNodeBackupError(task transferNodeTask) string {
	itemErr := ""
	for _, item := range task.Items {
		if item.Error != "" {
			itemErr = item.Error
			break
		}
	}
	if itemErr != "" && task.Error != "" && !strings.Contains(task.Error, itemErr) {
		return task.Error + ": " + itemErr
	}
	if task.Error != "" {
		return task.Error
	}
	return itemErr
}

// transferClient is the seam the unit tests stub; production is
// lib.GetIncusClientForHost.
var transferGetClient = func(ctx context.Context, host *db.HostsRecord) (*lib.IncusClient, error) {
	return lib.GetIncusClientForHost(ctx, host)
}

// transferNodeRequest issues one request against the node and decodes JSON.
func transferNodeRequest(ctx context.Context, client *lib.IncusClient, method, path string, body any) (map[string]any, error) {
	raw, err := client.Request(ctx, method, path, body, 0, body)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	obj, _ := decoded.(map[string]any)
	if obj == nil {
		return map[string]any{}, nil
	}
	return obj, nil
}

// transferEnsureHostSupport mirrors ensureHostTransferSupport.
func transferEnsureHostSupport(ctx context.Context, hostID int32) (*db.HostsRecord, *lib.IncusClient, bool, error) {
	host, err := db.HostsGetByID(ctx, hostID)
	if err != nil {
		return nil, nil, false, err
	}
	if host == nil {
		return nil, nil, false, errTransferHostMissing
	}
	client, err := transferGetClient(ctx, host)
	if err != nil {
		return nil, nil, false, err
	}
	info, err := transferNodeRequest(ctx, client, http.MethodGet, "/1.0/agent", nil)
	if err != nil {
		return nil, nil, false, err
	}
	extensions, _ := info["api_extensions"].([]any)
	supported := false
	for _, ext := range extensions {
		if text, ok := ext.(string); ok && text == transferExt {
			supported = true
			break
		}
	}
	return host, client, supported, nil
}

var errTransferHostMissing = errors.New("节点不存在")

// transferResolveImportStoragePool mirrors resolveHostImportStoragePool.
func transferResolveImportStoragePool(ctx context.Context, hostID int32, client *lib.IncusClient, requestedPool string) (*string, *transferPoolError, error) {
	explicit := strings.TrimSpace(requestedPool)

	incusPoolsRaw, incusErr := client.Request(ctx, http.MethodGet, "/1.0/storage-pools?recursion=1", nil, 0, nil)
	var nodePools []string
	incusPoolsKnown := incusErr == nil
	if incusPoolsKnown {
		var decoded []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(incusPoolsRaw, &decoded)
		for _, pool := range decoded {
			if name := strings.TrimSpace(pool.Name); name != "" {
				nodePools = append(nodePools, name)
			}
		}
	}

	var defaultProfile map[string]any
	if explicit == "" {
		if profileRaw, profileErr := client.Request(ctx, http.MethodGet, "/1.0/profiles/default", nil, 0, nil); profileErr == nil {
			_ = json.Unmarshal(profileRaw, &defaultProfile)
		}
	}

	if explicit != "" {
		if incusPoolsKnown {
			found := false
			for _, name := range nodePools {
				if name == explicit {
					found = true
					break
				}
			}
			if !found {
				return nil, &transferPoolError{Code: 400, Message: fmt.Sprintf("目标存储池 %q 在该宿主机上不存在", explicit)}, nil
			}
		}
		pool := explicit
		return &pool, nil, nil
	}

	if defaultProfile != nil {
		if devices, ok := defaultProfile["devices"].(map[string]any); ok {
			hasDefaultRoot := false
			for _, rawDevice := range devices {
				if device, ok := rawDevice.(map[string]any); ok {
					if device["type"] == "disk" && device["path"] == "/" {
						hasDefaultRoot = true
						break
					}
				}
			}
			if hasDefaultRoot {
				return nil, nil, nil
			}
		}
	}

	type dbPoolRow struct {
		Name    string `gorm:"column:name"`
		Purpose string `gorm:"column:purpose"`
	}
	var dbPools []dbPoolRow
	if db.DB != nil {
		_ = db.DB.WithContext(ctx).Raw(
			`SELECT name, purpose::text AS purpose FROM storage_pools WHERE host_id = ? ORDER BY purpose ASC, created_at ASC`, hostID,
		).Scan(&dbPools).Error
	}
	availableOnNode := func(name string) bool { return !incusPoolsKnown || transferContainsString(nodePools, name) }

	var candidates []string
	for _, pool := range dbPools {
		if pool.Purpose == "instance_data" && pool.Name == "default" && availableOnNode(pool.Name) {
			candidates = append(candidates, pool.Name)
			break
		}
	}
	for _, pool := range dbPools {
		if pool.Purpose == "instance_data" && availableOnNode(pool.Name) {
			candidates = append(candidates, pool.Name)
			break
		}
	}
	for _, pool := range dbPools {
		if availableOnNode(pool.Name) {
			candidates = append(candidates, pool.Name)
			break
		}
	}
	if transferContainsString(nodePools, "default") {
		candidates = append(candidates, "default")
	}
	if len(nodePools) > 0 {
		candidates = append(candidates, nodePools[0])
	}
	for _, candidate := range candidates {
		if candidate != "" {
			pool := candidate
			return &pool, nil, nil
		}
	}

	if incusPoolsKnown && len(nodePools) == 0 {
		return nil, &transferPoolError{Code: 409, Message: "宿主机尚未配置可用存储池，且默认配置无根磁盘设备，请先在「存储」标签页创建存储池"}, nil
	}
	return nil, nil, nil
}

type transferPoolError struct {
	Code    int
	Message string
}

func transferContainsString(values []string, candidate string) bool {
	for _, v := range values {
		if v == candidate {
			return true
		}
	}
	return false
}

// ==================== metrics extraction ====================

// transferParseSizeToMB mirrors parseSizeToMB (accepts B..TiB, default MB).
func transferParseSizeToMB(raw any, fallbackMB int32) int32 {
	text, ok := raw.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return fallbackMB
	}
	match := regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*(B|KB|KIB|MB|MIB|GB|GIB|TB|TIB)?$`).FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return fallbackMB
	}
	num, err := strconvParseFloat(match[1])
	if err != nil || num <= 0 {
		return fallbackMB
	}
	unit := strings.ToUpper(match[2])
	if unit == "" {
		unit = "B"
	}
	switch unit {
	case "B":
		return maxInt32One(int64(math.Round(num / (1024 * 1024))))
	case "KB", "KIB":
		return maxInt32One(int64(math.Round(num / 1024)))
	case "MB", "MIB":
		return maxInt32One(int64(math.Round(num)))
	case "GB", "GIB":
		return maxInt32One(int64(math.Round(num * 1024)))
	case "TB", "TIB":
		return maxInt32One(int64(math.Round(num * 1024 * 1024)))
	default:
		return fallbackMB
	}
}

func maxInt32One(v int64) int32 {
	if v < 1 {
		return 1
	}
	return int32(v)
}

func strconvParseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%g", &f)
	return f, err
}

// transferParseCpuPercentage mirrors parseCpuPercentage.
func transferParseCpuPercentage(config map[string]any) int32 {
	if raw, ok := config["limits.cpu.allowance"].(string); ok {
		trimmed := strings.TrimSpace(raw)
		if strings.HasSuffix(trimmed, "%") {
			if val, err := strconvParseFloat(strings.TrimSuffix(trimmed, "%")); err == nil && val > 0 {
				return maxInt32One(int64(val))
			}
		}
	}
	if raw, ok := config["limits.cpu"].(string); ok {
		if cores, err := strconvParseFloat(strings.TrimSpace(raw)); err == nil && cores > 0 {
			return maxInt32One(int64(cores * 100))
		}
	}
	return 100
}

func transferStripCIDR(addr string) *string {
	clean := strings.TrimSpace(strings.Split(addr, "/")[0])
	if clean == "" {
		return nil
	}
	return &clean
}

// transferExtractNetworkDetails mirrors extractNetworkDetails.
func transferExtractNetworkDetails(devices map[string]map[string]any, stateRaw []byte) (ipv4, ipv6 *string, networkMode string) {
	for _, dev := range devices {
		if dev["type"] != "nic" {
			continue
		}
		if ipv4 == nil {
			if raw, ok := dev["ipv4.address"].(string); ok {
				ipv4 = transferStripCIDR(raw)
			}
		}
		if ipv6 == nil {
			if raw, ok := dev["ipv6.address"].(string); ok {
				ipv6 = transferStripCIDR(raw)
			}
		}
	}
	if len(stateRaw) > 0 {
		var state struct {
			Network map[string]struct {
				Addresses []struct {
					Family  string `json:"family"`
					Scope   string `json:"scope"`
					Address string `json:"address"`
				} `json:"addresses"`
			} `json:"network"`
		}
		if json.Unmarshal(stateRaw, &state) == nil {
			for ifName, info := range state.Network {
				if ifName == "lo" {
					continue
				}
				for _, addr := range info.Addresses {
					if addr.Scope == "link" || addr.Scope == "host" || addr.Address == "" {
						continue
					}
					if addr.Family == "inet" && ipv4 == nil {
						v := addr.Address
						ipv4 = &v
					}
					if addr.Family == "inet6" && ipv6 == nil {
						v := addr.Address
						ipv6 = &v
					}
				}
			}
		}
	}
	networkMode = "nat"
	if ipv6 != nil && ipv4 == nil {
		networkMode = "ipv6_only"
	} else if ipv6 != nil && ipv4 != nil {
		networkMode = "nat_ipv6"
	}
	return ipv4, ipv6, networkMode
}

// transferExtractSnapshotFromRaw mirrors extractSnapshotFromIncus.
func transferExtractSnapshotFromRaw(inst map[string]any, stateRaw []byte) services.ExtractedIncusMetrics {
	config := map[string]any{}
	if expanded, ok := inst["expanded_config"].(map[string]any); ok && expanded != nil {
		config = expanded
	} else if base, ok := inst["config"].(map[string]any); ok && base != nil {
		config = base
	}
	devices := map[string]map[string]any{}
	if expanded, ok := inst["expanded_devices"].(map[string]any); ok && expanded != nil {
		for name, raw := range expanded {
			if entry, ok := raw.(map[string]any); ok {
				devices[name] = entry
			}
		}
	} else if base, ok := inst["devices"].(map[string]any); ok && base != nil {
		for name, raw := range base {
			if entry, ok := raw.(map[string]any); ok {
				devices[name] = entry
			}
		}
	}

	var diskFallback int32 = 10240
	var rootSize any
	var storagePoolName *string
	if root, ok := devices["root"]; ok {
		rootSize = root["size"]
		if pool, ok := root["pool"].(string); ok && strings.TrimSpace(pool) != "" {
			p := strings.TrimSpace(pool)
			storagePoolName = &p
		}
	}

	ipv4, ipv6, networkMode := transferExtractNetworkDetails(devices, stateRaw)
	image := "imported"
	if desc, ok := config["image.description"].(string); ok && desc != "" {
		image = desc
	} else if osName, ok := config["image.os"].(string); ok && osName != "" {
		image = osName
	}
	status := "stopped"
	if s, _ := inst["status"].(string); strings.EqualFold(s, "running") {
		status = "running"
	}
	instanceType := "container"
	if t, _ := inst["type"].(string); t == "virtual-machine" {
		instanceType = "vm"
	}

	return services.ExtractedIncusMetrics{
		CPU:             transferParseCpuPercentage(config),
		Memory:          transferParseSizeToMB(config["limits.memory"], 512),
		Disk:            transferParseSizeToMB(rootSize, diskFallback),
		StoragePoolName: storagePoolName,
		IPv4:            ipv4,
		IPv6:            ipv6,
		NetworkMode:     modelsNetworkMode(networkMode),
		Image:           image,
		Status:          status,
		InstanceType:    instanceType,
	}
}

func modelsNetworkMode(mode string) models.NetworkMode {
	return models.NetworkMode(mode)
}

// ==================== GPU sanitization ====================

// transferSanitizeImportedGpuDevices mirrors sanitizeImportedInstanceGpuDevices.
func transferSanitizeImportedGpuDevices(ctx context.Context, client *lib.IncusClient, hostID int32, instanceName string) []string {
	live, err := lib.IncusInstanceFetchRaw(ctx, client, instanceName)
	if err != nil || live == nil {
		return nil
	}
	if len(lib.ListIncusGpuDevices(live)) == 0 {
		return nil
	}

	resourcesRaw, resourcesErr := client.Request(ctx, http.MethodGet, "/1.0/resources", nil, 0, nil)
	var resources any
	if resourcesErr == nil && len(resourcesRaw) > 0 {
		var decoded any
		if json.Unmarshal(resourcesRaw, &decoded) == nil {
			resources = decoded
		}
	}
	hostPcis := lib.ExtractResourceGpuPciAddresses(resources)
	if hostPcis == nil {
		if db.DB == nil {
			return nil
		}
		var pciRows []string
		_ = db.DB.WithContext(ctx).Raw(
			`SELECT pci_address FROM host_gpus WHERE host_id = ? AND available = true`, hostID,
		).Scan(&pciRows).Error
		if len(pciRows) == 0 && resources == nil {
			return nil
		}
		hostPcis = map[string]struct{}{}
		for _, pci := range pciRows {
			if trimmed := strings.ToLower(strings.TrimSpace(pci)); trimmed != "" {
				hostPcis[trimmed] = struct{}{}
			}
		}
	}

	removed := lib.SanitizeMissingHostGpuDevices(live, hostPcis)
	if len(removed) == 0 {
		return nil
	}
	if _, err := client.Request(ctx, http.MethodPut, "/1.0/instances/"+url.PathEscape(instanceName), live, 0, live); err != nil {
		return nil
	}
	return removed
}

// transferLogImportedGpuSanitization mirrors logImportedGpuSanitization.
func transferLogImportedGpuSanitization(ctx context.Context, opTaskID, hostID int32, targetName string, existingClient *lib.IncusClient) {
	cleanName := strings.TrimSpace(targetName)
	if cleanName == "" {
		return
	}
	client := existingClient
	if client == nil {
		host, err := db.HostsGetByID(ctx, hostID)
		if err != nil || host == nil {
			return
		}
		client, err = transferGetClient(ctx, host)
		if err != nil {
			return
		}
	}
	removed := transferSanitizeImportedGpuDevices(ctx, client, hostID, cleanName)
	if len(removed) == 0 {
		return
	}
	if db.DB == nil {
		return
	}
	_ = db.DB.WithContext(ctx).Exec(`
		INSERT INTO operation_task_logs (task_id, source, level, message, created_at)
		VALUES (?, 'server', 'warn', ?, NOW())`, opTaskID,
		fmt.Sprintf("已自动清理与当前宿主机不兼容的残留 GPU 设备：%s", strings.Join(removed, ", ")),
	).Error
}

// ==================== node task tracking ====================

// transferTaskLog writes one operation_task_logs row (best effort).
func transferTaskLog(ctx context.Context, taskID int32, source, level, message string) {
	if db.DB == nil {
		return
	}
	_ = db.DB.WithContext(ctx).Exec(`
		INSERT INTO operation_task_logs (task_id, source, level, message, created_at)
		VALUES (?, ?, ?, ?, NOW())`, taskID, source, level, message).Error
}

// transferCreateOperationTask mirrors prisma.operationTask.create.
func transferCreateOperationTask(ctx context.Context, hostID int32, instanceName, kind, status string, errText *string) (int32, error) {
	if db.DB == nil {
		return 0, errors.New("database not initialized")
	}
	finishedAt := any(nil)
	if status != "running" {
		finishedAt = time.Now().UTC()
	}
	var id int32
	if err := db.DB.WithContext(ctx).Raw(`
		INSERT INTO operation_tasks (host_id, instance_name, kind, status, error, created_at, finished_at)
		VALUES (?, ?, ?, ?, ?, NOW(), ?)
		RETURNING id`, hostID, instanceName, kind, status, errText, finishedAt,
	).Scan(&id).Error; err != nil {
		return 0, err
	}
	return id, nil
}

// transferSetTaskStatus mirrors operationTask.updateMany on the given status guard.
func transferSetTaskStatus(ctx context.Context, taskID int32, fromStatus, toStatus string, errText *string, finished bool) {
	if db.DB == nil {
		return
	}
	finishedAt := any(nil)
	if finished {
		finishedAt = time.Now().UTC()
	}
	_ = db.DB.WithContext(ctx).Exec(`
		UPDATE operation_tasks SET status = ?, error = ?, finished_at = ?, updated_at = NOW()
		WHERE id = ? AND status = ?`, toStatus, errText, finishedAt, taskID, fromStatus).Error
}

// transferTrackNodeTask mirrors trackNodeTransferTask.
func transferTrackNodeTask(
	ctx context.Context,
	hostID int32,
	instanceName string,
	importedInstanceName *string,
	kind lib.OperationTaskKind,
	targetPath string,
	storagePool *string,
	task transferNodeTask,
	auditUserID *int32,
) (int32, error) {
	actionLabel := "导出"
	if kind == lib.OperationTaskKindInstanceImport {
		actionLabel = "导入"
	}
	initialStatus := "failed"
	switch task.Status {
	case "running":
		initialStatus = "running"
	case "success":
		initialStatus = "success"
	}
	var initialErr *string
	if initialStatus != "running" {
		if text := transferFormatNodeBackupError(task); text != "" {
			initialErr = &text
		}
	}

	opTaskID, err := transferCreateOperationTask(ctx, hostID, instanceName, string(kind), initialStatus, initialErr)
	if err != nil {
		return 0, err
	}

	poolDetail := ""
	if storagePool != nil {
		poolDetail = "，目标存储池 " + *storagePool
	}
	transferTaskLog(ctx, opTaskID, "server", "info",
		fmt.Sprintf("开始%s实例 %s（节点任务 %s，路径 %s%s）", actionLabel, instanceName, task.ID, targetPath, poolDetail))

	if initialStatus != "running" {
		errText := transferFormatNodeBackupError(task)
		level := "error"
		message := errText
		if initialStatus == "success" {
			level = "info"
			filePath := targetPath
			if task.FilePath != "" {
				filePath = task.FilePath
			}
			message = fmt.Sprintf("%s完成：%s", actionLabel, filePath)
		}
		if message == "" {
			message = fmt.Sprintf("%s失败", actionLabel)
		}
		transferTaskLog(ctx, opTaskID, "node", level, message)
		if kind == lib.OperationTaskKindInstanceImport {
			resolvedName := strings.TrimSpace(task.Instance)
			if resolvedName == "" && importedInstanceName != nil {
				resolvedName = strings.TrimSpace(*importedInstanceName)
			}
			if initialStatus == "success" {
				transferLogImportedGpuSanitization(ctx, opTaskID, hostID, resolvedName, nil)
			}
			transferFinalizeImportedPreset(ctx, opTaskID, hostID, task.ID, initialStatus, resolvedName, nil, auditUserID)
		}
		return opTaskID, nil
	}

	// Live task: poll the node every 2s from a detached goroutine, mirroring the
	// unref'd setTimeout chain (max 3600 attempts, then 'interrupted').
	go transferPollNodeTask(context.WithoutCancel(ctx), hostID, instanceName, importedInstanceName, kind, targetPath, storagePool, task, opTaskID, auditUserID)
	return opTaskID, nil
}

func transferPollNodeTask(
	ctx context.Context,
	hostID int32,
	instanceName string,
	importedInstanceName *string,
	kind lib.OperationTaskKind,
	targetPath string,
	storagePool *string,
	task transferNodeTask,
	opTaskID int32,
	auditUserID *int32,
) {
	lib.SetOperationTaskProgress(opTaskID, nil)
	actionLabel := "导出"
	if kind == lib.OperationTaskKindInstanceImport {
		actionLabel = "导入"
	}

	lastProgress := strings.TrimSpace(task.Progress)
	var lastProgressLogAt time.Time
	if lastProgress != "" {
		lib.SetOperationTaskProgress(opTaskID, &lastProgress)
		lastProgressLogAt = time.Now().UTC()
		transferTaskLog(ctx, opTaskID, "node", "info", lib.RedactTaskLog(lastProgress))
	}

	for attempts := 1; attempts <= transferMaxAttempts; attempts++ {
		host, err := db.HostsGetByID(ctx, hostID)
		if err != nil || host == nil {
			lib.SetOperationTaskProgress(opTaskID, nil)
			if kind == lib.OperationTaskKindInstanceImport {
				_, _ = services.ConsumePendingImportTaskPreset(ctx, task.ID)
			}
			errText := "宿主机不存在"
			transferSetTaskStatus(ctx, opTaskID, "running", "failed", &errText, true)
			return
		}
		client, clientErr := transferGetClient(ctx, host)
		if clientErr == nil {
			data, dataErr := transferNodeRequest(ctx, client, http.MethodGet, transferBackupsPath, nil)
			if dataErr == nil {
				tasksRaw, _ := data["tasks"].([]any)
				var found *transferNodeTask
				for _, raw := range tasksRaw {
					encoded, encErr := json.Marshal(raw)
					if encErr != nil {
						continue
					}
					var candidate transferNodeTask
					if json.Unmarshal(encoded, &candidate) == nil && candidate.ID == task.ID {
						found = &candidate
						break
					}
				}
				if found == nil {
					if attempts >= 5 {
						lib.SetOperationTaskProgress(opTaskID, nil)
						if kind == lib.OperationTaskKindInstanceImport {
							_, _ = services.ConsumePendingImportTaskPreset(ctx, task.ID)
						}
						errText := "节点未返回该任务状态"
						transferSetTaskStatus(ctx, opTaskID, "running", "interrupted", &errText, true)
						return
					}
				} else if found.Status != "running" {
					lib.SetOperationTaskProgress(opTaskID, nil)
					finalStatus := "failed"
					if found.Status == "success" {
						finalStatus = "success"
					}
					errText := transferFormatNodeBackupError(*found)
					level := "error"
					message := errText
					if finalStatus == "success" {
						level = "info"
						filePath := targetPath
						if found.FilePath != "" {
							filePath = found.FilePath
						}
						message = fmt.Sprintf("%s完成：%s", actionLabel, filePath)
					}
					if message == "" {
						message = fmt.Sprintf("%s失败", actionLabel)
					}
					transferTaskLog(ctx, opTaskID, "node", level, message)
					if kind == lib.OperationTaskKindInstanceImport {
						resolvedName := strings.TrimSpace(found.Instance)
						if resolvedName == "" && importedInstanceName != nil {
							resolvedName = strings.TrimSpace(*importedInstanceName)
						}
						if finalStatus == "success" {
							transferLogImportedGpuSanitization(ctx, opTaskID, hostID, resolvedName, client)
						}
						transferFinalizeImportedPreset(ctx, opTaskID, hostID, task.ID, finalStatus, resolvedName, client, auditUserID)
					}
					var errPtr *string
					if finalStatus != "success" {
						if errText == "" {
							errText = fmt.Sprintf("%s失败", actionLabel)
						}
						errPtr = &errText
					}
					transferSetTaskStatus(ctx, opTaskID, "running", finalStatus, errPtr, true)
					return
				} else {
					progressText := strings.TrimSpace(found.Progress)
					if progressText != "" {
						lib.SetOperationTaskProgress(opTaskID, &progressText)
					} else {
						lib.SetOperationTaskProgress(opTaskID, nil)
					}
					now := time.Now().UTC()
					if progressText != "" && progressText != lastProgress &&
						(now.Sub(lastProgressLogAt) >= transferProgressLogInterval ||
							transferNodeProgressStageKey(progressText) != transferNodeProgressStageKey(lastProgress)) {
						lastProgress = progressText
						lastProgressLogAt = now
						transferTaskLog(ctx, opTaskID, "node", "info", lib.RedactTaskLog(progressText))
					}
				}
			}
		}
		// Transient poll failures keep waiting, like the TS catch {}.

		if attempts >= transferMaxAttempts {
			lib.SetOperationTaskProgress(opTaskID, nil)
			errText := "轮询超时，请检查节点实际状态"
			transferSetTaskStatus(ctx, opTaskID, "running", "interrupted", &errText, true)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(transferPollInterval):
		}
	}
}

// transferFinalizeImportedPreset mirrors finalizeImportedInstancePreset.
func transferFinalizeImportedPreset(
	ctx context.Context,
	opTaskID, hostID int32,
	nodeTaskID string,
	finalStatus string,
	incusName string,
	client *lib.IncusClient,
	auditUserID *int32,
) {
	preset, err := services.ConsumePendingImportTaskPreset(ctx, nodeTaskID)
	if err != nil || finalStatus != "success" || preset == nil {
		return
	}

	cleanName := strings.TrimSpace(incusName)
	if cleanName == "" {
		if preset.PackageID != nil || preset.UserID != nil {
			transferTaskLog(ctx, opTaskID, "server", "warn",
				"导入已完成，但节点未返回实例名称，无法自动绑定套餐或用户；请前往未分配实例列表手动处理")
		}
		return
	}

	if preset.UserID != nil {
		if client == nil {
			host, hostErr := db.HostsGetByID(ctx, hostID)
			if hostErr == nil && host != nil {
				client, _ = transferGetClient(ctx, host)
			}
		}
		if client == nil {
			reason := "宿主机不存在"
			transferPresetAssignFailure(ctx, opTaskID, hostID, cleanName, preset, reason)
			return
		}

		instRaw, instErr := lib.IncusInstanceFetchRaw(ctx, client, cleanName)
		if instErr != nil {
			transferPresetAssignFailure(ctx, opTaskID, hostID, cleanName, preset, instErr.Error())
			return
		}
		stateRaw, _ := lib.IncusInstanceGetState(ctx, client, cleanName)
		metrics := transferExtractSnapshotFromRaw(instRaw, stateRaw)

		audit := auditUserID
		if audit == nil {
			audit = preset.UserID
		}
		registered, regErr := services.RegisterImportedHostInstance(ctx, hostID, client, cleanName, *preset.UserID, preset.DisplayName,
			services.PackageBindingInput{
				PackageID: preset.PackageID,
				PlanID:    preset.PlanID,
				ValidDays: preset.ValidDays,
				ExpiresAt: preset.ExpiresAt,
			}, &metrics, audit, "import_auto", "", false)
		if regErr != nil {
			transferPresetAssignFailure(ctx, opTaskID, hostID, cleanName, preset, regErr.Error())
			return
		}
		if !registered.AlreadyAssigned {
			pkgText := "（未选择套餐，按免费实例处理）"
			if registered.PackageName != nil {
				pkgText = fmt.Sprintf("，已绑定套餐 %s", *registered.PackageName)
				if registered.PlanName != nil {
					pkgText += " / " + *registered.PlanName
				}
			}
			transferTaskLog(ctx, opTaskID, "server", "info",
				fmt.Sprintf("导入后已自动分配给用户 #%d%s", *preset.UserID, pkgText))
		}
		return
	}

	if preset.PackageID != nil || preset.PlanID != nil || preset.ValidDays != nil || preset.ExpiresAt != nil {
		_ = services.SaveUnassignedInstancePreset(ctx, hostID, cleanName, *preset)
		pkgLabel := "未挂载套餐"
		if preset.PackageName != nil {
			pkgLabel = *preset.PackageName
			if preset.PlanName != nil {
				pkgLabel += " / " + *preset.PlanName
			}
		} else if preset.PackageID != nil {
			pkgLabel = fmt.Sprintf("套餐 #%d", *preset.PackageID)
		}
		transferTaskLog(ctx, opTaskID, "server", "info",
			fmt.Sprintf("已保存目标套餐预设（%s），请在未分配实例列表分配用户时生效", pkgLabel))
	}
}

func transferPresetAssignFailure(ctx context.Context, opTaskID, hostID int32, incusName string, preset *services.PendingImportPreset, reason string) {
	if preset.PackageID != nil || preset.PlanID != nil || preset.ValidDays != nil || preset.ExpiresAt != nil {
		_ = services.SaveUnassignedInstancePreset(ctx, hostID, incusName, *preset)
	}
	transferTaskLog(ctx, opTaskID, "server", "warn",
		fmt.Sprintf("导入成功，但自动分配用户/绑定套餐失败（%s）；已保留预设，请在未分配实例列表手动分配", reason))
}

// ==================== handlers ====================

// transferNodeErrorStatus mirrors the [400,403,404,409,503] IncusApiError mapping.
func transferNodeErrorStatus(err error) int {
	var incusErr *lib.IncusApiError
	if errors.As(err, &incusErr) {
		switch incusErr.StatusCode {
		case 400, 403, 404, 409, 503:
			return incusErr.StatusCode
		}
	}
	return http.StatusBadGateway
}

// transferListHandler mirrors GET /api/admin/hosts/:hostId/instance-transfer.
func transferListHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	ctx := c.Request.Context()
	hostID, ok := transferRouteHostID(c)
	if !ok {
		return
	}
	host, client, supported, err := transferEnsureHostSupport(ctx, hostID)
	if err != nil {
		if errors.Is(err, errTransferHostMissing) {
			c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
			return
		}
		c.JSON(transferNodeErrorStatus(err), gin.H{"error": lib.NodeErrorDetail(err, "获取节点迁移任务失败", nil)})
		return
	}
	_ = host
	if !supported {
		c.JSON(http.StatusOK, gin.H{"supported": false, "tasks": []any{}})
		return
	}
	data, err := transferNodeRequest(ctx, client, http.MethodGet, transferBackupsPath, nil)
	if err != nil {
		c.JSON(transferNodeErrorStatus(err), gin.H{"error": lib.NodeErrorDetail(err, "获取节点迁移任务失败", nil)})
		return
	}
	tasks, _ := data["tasks"].([]any)
	if tasks == nil {
		tasks = []any{}
	}
	c.JSON(http.StatusOK, gin.H{"supported": true, "tasks": tasks})
}

// transferExportHandler mirrors POST /api/admin/hosts/:hostId/instances/:instanceId/export.
func transferExportHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	ctx := c.Request.Context()
	hostID, ok := transferRouteHostID(c)
	if !ok {
		return
	}
	instanceID, ok := transferRouteIntParam(c, "instanceId")
	if !ok {
		return
	}

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		respondAJVValidation(c, message)
		return
	}
	validated, message := validateAJVObject(decoded.Value, []string{"targetPath"}, []ajvFieldSpec{
		{Name: "id", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Pattern: transferTaskIDPattern},
		{Name: "targetPath", Kind: ajvKindString, MinLength: 2, MaxLength: 4096, Pattern: transferTargetPathPattern},
		{Name: "mode", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"directory", "file"}},
	})
	if message != "" {
		respondAJVValidation(c, message)
		return
	}
	user := middleware.CurrentUser(c)

	var instance struct {
		ID      int32  `gorm:"column:id"`
		IncusID string `gorm:"column:incus_id"`
		Name    string `gorm:"column:name"`
	}
	if db.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, incus_id, name FROM instances WHERE id = ? AND host_id = ? AND status <> 'deleted' LIMIT 1`,
		instanceID, hostID,
	).Scan(&instance).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if instance.ID == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在或不属于该宿主机"})
		return
	}

	host, client, supported, err := transferEnsureHostSupport(ctx, hostID)
	if err != nil {
		if errors.Is(err, errTransferHostMissing) {
			c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
			return
		}
		c.JSON(transferNodeErrorStatus(err), gin.H{"error": lib.NodeErrorDetail(err, "导出实例失败", nil)})
		return
	}
	_ = host
	if !supported {
		c.JSON(http.StatusConflict, gin.H{"error": "节点程序版本暂不支持单实例导出，请先升级节点程序包"})
		return
	}

	targetPath, _ := validated.stringValue("targetPath")
	targetPath = strings.TrimSpace(targetPath)
	mode, _ := validated.stringValue("mode")
	isFileMode := mode == "file" || (mode == "" && transferFileExtPattern.MatchString(targetPath))

	taskID, hasID := validated.stringValue("id")
	if !hasID || taskID == "" {
		taskID = transferGenerateTaskID()
	}
	payload := map[string]any{
		"id":       taskID,
		"instance": instance.IncusID,
	}
	if isFileMode {
		payload["filePath"] = targetPath
	} else {
		payload["directory"] = targetPath
	}

	taskObj, err := transferNodeRequest(ctx, client, http.MethodPost, transferBackupsPath, payload)
	if err != nil {
		c.JSON(transferNodeErrorStatus(err), gin.H{"error": lib.NodeErrorDetail(err, "导出实例失败", nil)})
		return
	}
	task := transferDecodeNodeTask(taskObj, taskID)

	opTaskID, trackErr := transferTrackNodeTask(ctx, hostID, instance.IncusID, nil, lib.OperationTaskKindInstanceExport, targetPath, nil, task, nil)
	if trackErr != nil {
		log.Printf("[Transfer] export tracking failed for %s: %v", instance.IncusID, trackErr)
	}

	uid := int64(user.ID)
	db.CreateLog(ctx, &uid, db.LogModuleHost, "host.instance.export",
		fmt.Sprintf("节点 #%d 管理员导出实例 %s (%s) 至 %s", hostID, instance.Name, instance.IncusID, targetPath),
		db.LogResultSuccess, db.CreateLogOptions{})

	result := map[string]any{}
	for k, v := range taskObj {
		result[k] = v
	}
	result["operationTaskId"] = opTaskID
	c.JSON(http.StatusOK, result)
}

// transferImportHandler mirrors POST /api/admin/hosts/:hostId/instances/import.
func transferImportHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	ctx := c.Request.Context()
	hostID, ok := transferRouteHostID(c)
	if !ok {
		return
	}

	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		respondAJVValidation(c, message)
		return
	}
	validated, message := validateAJVObject(decoded.Value, []string{"filePath"}, []ajvFieldSpec{
		{Name: "id", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Pattern: transferTaskIDPattern},
		{Name: "filePath", Kind: ajvKindString, MinLength: 2, MaxLength: 4096, Pattern: transferTargetPathPattern},
		{Name: "name", Kind: ajvKindString, MinLength: 1, MaxLength: 63, Pattern: transferNamePattern},
		{Name: "storagePool", Kind: ajvKindString, MinLength: 1, MaxLength: 64},
		{Name: "packageId", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 2147483647},
		{Name: "planId", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 2147483647},
		{Name: "validDays", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 36500},
		{Name: "expiresAt", Kind: ajvKindNullableString, MinLength: -1, MaxLength: 64},
		{Name: "userId", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 2147483647},
		{Name: "displayName", Kind: ajvKindNullableString, MinLength: 1, MaxLength: 64},
	})
	if message != "" {
		respondAJVValidation(c, message)
		return
	}
	user := middleware.CurrentUser(c)

	host, client, supported, err := transferEnsureHostSupport(ctx, hostID)
	if err != nil {
		if errors.Is(err, errTransferHostMissing) {
			c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
			return
		}
		c.JSON(transferNodeErrorStatus(err), gin.H{"error": lib.NodeErrorDetail(err, "导入实例失败", nil)})
		return
	}
	if !supported {
		c.JSON(http.StatusConflict, gin.H{"error": "节点程序版本暂不支持实例导入，请先升级节点程序包"})
		return
	}

	storagePoolParam, _ := validated.stringValue("storagePool")
	pool, poolErr, err := transferResolveImportStoragePool(ctx, hostID, client, storagePoolParam)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导入实例失败"})
		return
	}
	if poolErr != nil {
		c.JSON(poolErr.Code, gin.H{"error": poolErr.Message})
		return
	}

	hostInstanceType := ""
	if host.InstanceType == "container" || host.InstanceType == "vm" {
		hostInstanceType = host.InstanceType
	}

	packageID, _ := validated.nullableInt32("packageId")
	planID, planIDPresent := validated.nullableInt32("planId")
	validDays, _ := validated.nullableInt32("validDays")
	expiresAt, _ := validated.nullableString("expiresAt")
	targetUserID, _ := validated.nullableInt32("userId")

	validatedBinding, err := services.ValidatePackageBindingSelection(ctx, hostID, services.PackageBindingInput{
		PackageID: packageID,
		PlanID:    planID,
		ValidDays: validDays,
		ExpiresAt: expiresAt,
	}, hostInstanceType, targetUserID)
	if err != nil {
		var bindingErr *services.PackageBindingError
		if errors.As(err, &bindingErr) {
			c.JSON(bindingErr.StatusCode, gin.H{"error": bindingErr.Message})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导入实例失败"})
		return
	}

	taskID, hasID := validated.stringValue("id")
	if !hasID || taskID == "" {
		taskID = transferGenerateTaskID()
	}
	filePath, _ := validated.stringValue("filePath")
	payload := map[string]any{
		"id":       taskID,
		"filePath": strings.TrimSpace(filePath),
	}
	if name, present := validated.stringValue("name"); present && strings.TrimSpace(name) != "" {
		payload["name"] = strings.TrimSpace(name)
	}
	if pool != nil {
		payload["storagePool"] = *pool
	}

	taskObj, err := transferNodeRequest(ctx, client, http.MethodPost, transferImportPath, payload)
	if err != nil {
		c.JSON(transferNodeErrorStatus(err), gin.H{"error": lib.NodeErrorDetail(err, "导入实例失败", nil)})
		return
	}
	task := transferDecodeNodeTask(taskObj, taskID)
	if task.ID == "" {
		task.ID = taskID
	}

	shouldSavePreset := (packageID != nil && *packageID != 0) || (targetUserID != nil && *targetUserID != 0) ||
		(validDays != nil && *validDays != 0) || (expiresAt != nil && strings.TrimSpace(*expiresAt) != "")
	if shouldSavePreset && task.ID != "" {
		presetPlanID := planID
		if !planIDPresent {
			presetPlanID = validatedBinding.ResolvedPlanID
		}
		var displayName *string
		if dn, present := validated.nullableString("displayName"); present && dn != nil && strings.TrimSpace(*dn) != "" {
			d := strings.TrimSpace(*dn)
			displayName = &d
		}
		var presetExpiresAt *string
		if expiresAt != nil && strings.TrimSpace(*expiresAt) != "" {
			e := strings.TrimSpace(*expiresAt)
			presetExpiresAt = &e
		}
		incusName := ""
		if name, present := payload["name"].(string); present {
			incusName = name
		} else if task.Instance != "" {
			incusName = strings.TrimSpace(task.Instance)
		}
		var incusNamePtr *string
		if incusName != "" {
			incusNamePtr = &incusName
		}
		fp := strings.TrimSpace(filePath)
		if err := services.SavePendingImportTaskPreset(ctx, task.ID, hostID, incusNamePtr, &fp, services.PendingImportPreset{
			PackageID:   packageID,
			PlanID:      presetPlanID,
			PackageName: validatedBinding.PackageName,
			PlanName:    validatedBinding.PlanName,
			ValidDays:   validDays,
			ExpiresAt:   presetExpiresAt,
			UserID:      targetUserID,
			DisplayName: displayName,
			CreatedAt:   time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		}); err != nil {
			log.Printf("[Transfer] save import preset failed for task %s: %v", task.ID, err)
		}
	}

	var importedName *string
	if name, present := payload["name"].(string); present {
		importedName = &name
	}
	auditUID := user.ID
	opTaskID, trackErr := transferTrackNodeTask(ctx, hostID, func() string {
		if importedName != nil {
			return *importedName
		}
		return strings.TrimSpace(filePath)
	}(), importedName, lib.OperationTaskKindInstanceImport, strings.TrimSpace(filePath), pool, task, &auditUID)
	if trackErr != nil {
		log.Printf("[Transfer] import tracking failed for task %s: %v", task.ID, trackErr)
	}

	pkgAuditNote := ""
	if validatedBinding.PackageName != nil {
		pkgAuditNote = "，预设套餐 " + *validatedBinding.PackageName
		if validatedBinding.PlanName != nil {
			pkgAuditNote += " / " + *validatedBinding.PlanName
		}
	}
	uid := int64(user.ID)
	nameNote := ""
	if importedName != nil {
		nameNote = fmt.Sprintf(" (%s)", *importedName)
	}
	db.CreateLog(ctx, &uid, db.LogModuleHost, "host.instance.import",
		fmt.Sprintf("节点 #%d 管理员从路径 %s 导入实例%s%s", hostID, strings.TrimSpace(filePath), nameNote, pkgAuditNote),
		db.LogResultSuccess, db.CreateLogOptions{})

	result := map[string]any{}
	for k, v := range taskObj {
		result[k] = v
	}
	result["operationTaskId"] = opTaskID
	c.JSON(http.StatusOK, result)
}

func RegisterAdminHostInstanceTransfer(r *gin.Engine) {
	group := r.Group("/api/admin/hosts", middleware.Authenticate(), middleware.RequireAdmin())

	// GET /api/admin/hosts/:hostId/instance-transfer
	group.GET("/:hostId/instance-transfer", transferListHandler)

	// POST /api/admin/hosts/:hostId/instances/:instanceId/export
	group.POST("/:hostId/instances/:instanceId/export", transferExportHandler)

	// POST /api/admin/hosts/:hostId/instances/import
	group.POST("/:hostId/instances/import", transferImportHandler)

	// GET /api/admin/hosts/:hostId/import-packages
	group.GET("/:hostId/import-packages", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		ctx := c.Request.Context()
		hostID, ok := transferRouteHostID(c)
		if !ok {
			return
		}
		packages, err := services.ListImportablePackagesForHost(ctx, hostID)
		if err != nil {
			var bindingErr *services.PackageBindingError
			if errors.As(err, &bindingErr) {
				c.JSON(bindingErr.StatusCode, gin.H{"error": bindingErr.Message})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "获取可挂载套餐列表失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"packages": packages})
	})

	// GET /api/admin/hosts/:hostId/import-storage-pools
	group.GET("/:hostId/import-storage-pools", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		ctx := c.Request.Context()
		hostID, ok := transferRouteHostID(c)
		if !ok {
			return
		}
		if db.DB == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		host, err := db.HostsGetByID(ctx, hostID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if host == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
			return
		}

		type dbPoolRow struct {
			Name    string `gorm:"column:name"`
			Driver  string `gorm:"column:driver"`
			Purpose string `gorm:"column:purpose"`
			Status  string `gorm:"column:status"`
		}
		var dbPools []dbPoolRow
		if err := db.DB.WithContext(ctx).Raw(
			`SELECT name, driver, purpose::text AS purpose, status FROM storage_pools WHERE host_id = ? ORDER BY purpose ASC, created_at ASC`,
			hostID,
		).Scan(&dbPools).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		dbMap := map[string]dbPoolRow{}
		for _, pool := range dbPools {
			dbMap[pool.Name] = pool
		}

		var livePools []struct {
			Name   string `json:"name"`
			Driver string `json:"driver"`
			Status string `json:"status"`
		}
		liveKnown := false
		client, clientErr := transferGetClient(ctx, host)
		if clientErr == nil {
			if raw, rawErr := client.Request(ctx, http.MethodGet, "/1.0/storage-pools?recursion=1", nil, 0, nil); rawErr == nil {
				if json.Unmarshal(raw, &livePools) == nil {
					liveKnown = true
				}
			}
		}
		if !liveKnown && len(dbPools) == 0 {
			if clientErr != nil {
				c.JSON(transferNodeErrorStatus(clientErr), gin.H{"error": lib.NodeErrorDetail(clientErr, "获取宿主机存储池失败", nil)})
				return
			}
			c.JSON(http.StatusBadGateway, gin.H{"error": lib.NodeErrorDetail(errors.New("节点返回的存储池列表格式无效"), "获取宿主机存储池失败", nil)})
			return
		}

		type poolView struct {
			Name    string  `json:"name"`
			Driver  string  `json:"driver"`
			Purpose *string `json:"purpose"`
			Status  string  `json:"status"`
		}
		var rawList []poolView
		if liveKnown {
			nodeNames := map[string]bool{}
			for _, pool := range livePools {
				name := strings.TrimSpace(pool.Name)
				if name == "" {
					continue
				}
				nodeNames[name] = true
				dbEntry, hasDB := dbMap[name]
				driver := strings.TrimSpace(pool.Driver)
				if driver == "" && hasDB {
					driver = dbEntry.Driver
				}
				if driver == "" {
					driver = "unknown"
				}
				var purpose *string
				if hasDB {
					p := dbEntry.Purpose
					purpose = &p
				}
				status := strings.TrimSpace(pool.Status)
				if status == "" && hasDB {
					status = dbEntry.Status
				}
				if status == "" {
					status = "Created"
				}
				rawList = append(rawList, poolView{Name: name, Driver: driver, Purpose: purpose, Status: status})
			}
		} else {
			for _, pool := range dbPools {
				if pool.Status == "missing" {
					continue
				}
				p := pool.Purpose
				rawList = append(rawList, poolView{Name: pool.Name, Driver: pool.Driver, Purpose: &p, Status: pool.Status})
			}
		}

		sort.SliceStable(rawList, func(i, j int) bool {
			aScore, bScore := 0, 0
			if rawList[i].Purpose != nil && *rawList[i].Purpose == "instance_storage" {
				aScore = 1
			}
			if rawList[j].Purpose != nil && *rawList[j].Purpose == "instance_storage" {
				bScore = 1
			}
			if aScore != bScore {
				return aScore < bScore
			}
			if rawList[i].Name == "default" && rawList[j].Name != "default" {
				return true
			}
			if rawList[j].Name == "default" && rawList[i].Name != "default" {
				return false
			}
			return rawList[i].Name < rawList[j].Name
		})

		c.JSON(http.StatusOK, gin.H{"pools": rawList})
	})

	// GET /api/admin/hosts/:hostId/unassigned-instances
	group.GET("/:hostId/unassigned-instances", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		ctx := c.Request.Context()
		hostID, ok := transferRouteHostID(c)
		if !ok {
			return
		}
		if db.DB == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		host, err := db.HostsGetByID(ctx, hostID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if host == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
			return
		}

		client, err := transferGetClient(ctx, host)
		if err != nil {
			c.JSON(transferNodeErrorStatus(err), gin.H{"error": lib.NodeErrorDetail(err, "读取宿主机无归属实例失败", nil)})
			return
		}
		incusInstances, err := lib.IncusInstanceList(ctx, client)
		if err != nil {
			c.JSON(transferNodeErrorStatus(err), gin.H{"error": lib.NodeErrorDetail(err, "读取宿主机无归属实例失败", nil)})
			return
		}
		var managedNames []string
		_ = db.DB.WithContext(ctx).Raw(
			`SELECT incus_id FROM instances WHERE host_id = ? AND status <> 'deleted'`, hostID,
		).Scan(&managedNames).Error
		managedSet := map[string]bool{}
		for _, name := range managedNames {
			managedSet[name] = true
		}

		type unassignedView struct {
			IncusName       string                        `json:"incusName"`
			InstanceType    string                        `json:"instanceType"`
			Status          string                        `json:"status"`
			CPU             int32                         `json:"cpu"`
			Memory          int32                         `json:"memory"`
			Disk            int32                         `json:"disk"`
			StoragePoolName *string                       `json:"storagePoolName"`
			IPv4            *string                       `json:"ipv4"`
			IPv6            *string                       `json:"ipv6"`
			NetworkMode     string                        `json:"networkMode"`
			Image           string                        `json:"image"`
			CreatedAt       *string                       `json:"createdAt"`
			Preset          *services.PendingImportPreset `json:"preset"`
		}
		unassigned := make([]unassignedView, 0)
		incusNames := make([]string, 0)
		for _, inst := range incusInstances {
			if managedSet[inst.Name] {
				continue
			}
			metrics := transferExtractSnapshotFromRaw(transferInstanceToRawMap(inst), nil)
			incusNames = append(incusNames, inst.Name)
			unassigned = append(unassigned, unassignedView{
				IncusName:       inst.Name,
				InstanceType:    metrics.InstanceType,
				Status:          metrics.Status,
				CPU:             metrics.CPU,
				Memory:          metrics.Memory,
				Disk:            metrics.Disk,
				StoragePoolName: metrics.StoragePoolName,
				IPv4:            metrics.IPv4,
				IPv6:            metrics.IPv6,
				NetworkMode:     string(metrics.NetworkMode),
				Image:           metrics.Image,
			})
		}

		presets, presetErr := services.ListUnassignedInstancePresetsForHost(ctx, hostID, incusNames)
		if presetErr != nil {
			presets = map[string]services.PendingImportPreset{}
		}
		for i := range unassigned {
			if preset, ok := presets[unassigned[i].IncusName]; ok {
				p := preset
				unassigned[i].Preset = &p
			}
		}

		c.JSON(http.StatusOK, gin.H{"instances": unassigned})
	})

	// POST /api/admin/hosts/:hostId/unassigned-instances/:incusName/assign
	group.POST("/:hostId/unassigned-instances/:incusName/assign", func(c *gin.Context) {
		ctx := c.Request.Context()
		hostID, ok := transferRouteHostID(c)
		if !ok {
			return
		}
		incusName := c.Param("incusName")
		if !transferNamePattern.MatchString(incusName) || len(incusName) > 63 {
			respondAJVValidation(c, "params/incusName must match pattern \"^[a-zA-Z0-9][a-zA-Z0-9-]*$\"")
			return
		}

		decoded, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			respondAJVValidation(c, message)
			return
		}
		validated, message := validateAJVObject(decoded.Value, []string{"userId"}, []ajvFieldSpec{
			{Name: "userId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 2147483647},
			{Name: "name", Kind: ajvKindString, MinLength: 1, MaxLength: 64},
			{Name: "packageId", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 2147483647},
			{Name: "planId", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 2147483647},
			{Name: "validDays", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 36500},
			{Name: "expiresAt", Kind: ajvKindNullableString, MinLength: -1, MaxLength: 64},
		})
		if message != "" {
			respondAJVValidation(c, message)
			return
		}
		user := middleware.CurrentUser(c)

		if db.DB == nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		host, err := db.HostsGetByID(ctx, hostID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if host == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
			return
		}

		userIDValue, _ := validated.Values["userId"].(float64)
		targetUserID := int32(userIDValue)
		var targetUser struct {
			ID       int32   `gorm:"column:id"`
			Username string  `gorm:"column:username"`
			Email    *string `gorm:"column:email"`
		}
		if err := db.DB.WithContext(ctx).Raw(`SELECT id, username, email FROM users WHERE id = ? LIMIT 1`, targetUserID).Scan(&targetUser).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if targetUser.ID == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "目标用户不存在"})
			return
		}

		var existingID int32
		_ = db.DB.WithContext(ctx).Raw(
			`SELECT id FROM instances WHERE host_id = ? AND incus_id = ? AND status <> 'deleted' LIMIT 1`, hostID, incusName,
		).Scan(&existingID).Error
		if existingID != 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "该实例已归属于面板用户，无需重复分配"})
			return
		}

		savedPreset, _ := services.GetUnassignedInstancePreset(ctx, hostID, incusName)

		// hasExplicitPackageInput: packageId present in the request body.
		packageID, packageIDPresent := validated.nullableInt32("packageId")
		planID, _ := validated.nullableInt32("planId")
		validDays, validDaysPresent := validated.nullableInt32("validDays")
		expiresAt, expiresAtPresent := validated.nullableString("expiresAt")

		effective := services.PackageBindingInput{PackageID: packageID, PlanID: planID}
		if packageIDPresent {
			effective.ValidDays = validDays
			effective.ExpiresAt = expiresAt
		} else {
			if savedPreset != nil {
				effective.PackageID = savedPreset.PackageID
				effective.PlanID = savedPreset.PlanID
			}
			if validDaysPresent {
				effective.ValidDays = validDays
			} else if savedPreset != nil {
				effective.ValidDays = savedPreset.ValidDays
			}
			if expiresAtPresent {
				effective.ExpiresAt = expiresAt
			} else if savedPreset != nil {
				effective.ExpiresAt = savedPreset.ExpiresAt
			}
		}

		client, err := transferGetClient(ctx, host)
		if err != nil {
			c.JSON(transferNodeErrorStatus(err), gin.H{"error": lib.NodeErrorDetail(err, "分配无归属实例失败", nil)})
			return
		}

		instRaw, err := lib.IncusInstanceFetchRaw(ctx, client, incusName)
		if err != nil {
			c.JSON(transferNodeErrorStatus(err), gin.H{"error": lib.NodeErrorDetail(err, "分配无归属实例失败", nil)})
			return
		}
		stateRaw, _ := lib.IncusInstanceGetState(ctx, client, incusName)
		metrics := transferExtractSnapshotFromRaw(instRaw, stateRaw)

		if _, err := services.ValidatePackageBindingSelection(ctx, hostID, effective, metrics.InstanceType, nil); err != nil {
			var bindingErr *services.PackageBindingError
			if errors.As(err, &bindingErr) {
				c.JSON(bindingErr.StatusCode, gin.H{"error": bindingErr.Message})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "分配无归属实例失败"})
			return
		}

		cleanedGpuDevices := lib.ListIncusGpuDevices(instRaw)
		if lib.ClearGpuDevices(instRaw) {
			if _, err := client.Request(ctx, http.MethodPut, "/1.0/instances/"+url.PathEscape(incusName), instRaw, 0, instRaw); err != nil {
				c.JSON(transferNodeErrorStatus(err), gin.H{"error": lib.NodeErrorDetail(err, "分配无归属实例失败", nil)})
				return
			}
		}
		cleanedNote := ""
		if len(cleanedGpuDevices) > 0 {
			labels := make([]string, 0, len(cleanedGpuDevices))
			for _, device := range cleanedGpuDevices {
				if device.Pci != nil {
					labels = append(labels, fmt.Sprintf("%s(%s)", device.Name, *device.Pci))
				} else {
					labels = append(labels, device.Name)
				}
			}
			cleanedNote = fmt.Sprintf("（已清理未登记 GPU 设备: %s）", strings.Join(labels, ", "))
		}

		var displayName *string
		if name, present := validated.stringValue("name"); present && strings.TrimSpace(name) != "" {
			n := strings.TrimSpace(name)
			displayName = &n
		}
		auditUID := user.ID
		registered, err := services.RegisterImportedHostInstance(ctx, hostID, client, incusName, targetUser.ID, displayName,
			effective, &metrics, &auditUID, "manual_assign", cleanedNote, true)
		if err != nil {
			var bindingErr *services.PackageBindingError
			if errors.As(err, &bindingErr) {
				if bindingErr.Code == "ALREADY_ASSIGNED" {
					c.JSON(http.StatusConflict, gin.H{"error": "该实例已归属于面板用户"})
					return
				}
				c.JSON(bindingErr.StatusCode, gin.H{"error": bindingErr.Message})
				return
			}
			if err.Error() == "ALREADY_ASSIGNED" {
				c.JSON(http.StatusConflict, gin.H{"error": "该实例已归属于面板用户"})
				return
			}
			c.JSON(transferNodeErrorStatus(err), gin.H{"error": lib.NodeErrorDetail(err, "分配无归属实例失败", nil)})
			return
		}

		_, _ = services.ConsumeUnassignedInstancePreset(ctx, hostID, incusName)

		c.JSON(http.StatusOK, gin.H{"instance": registered})
	})

	// PUT /api/admin/hosts/:hostId/instances/:instanceId/package
	group.PUT("/:hostId/instances/:instanceId/package", func(c *gin.Context) {
		ctx := c.Request.Context()
		hostID, ok := transferRouteHostID(c)
		if !ok {
			return
		}
		instanceID, ok := transferRouteIntParam(c, "instanceId")
		if !ok {
			return
		}

		decoded, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			respondAJVValidation(c, message)
			return
		}
		validated, message := validateAJVObject(decoded.Value, nil, []ajvFieldSpec{
			{Name: "packageId", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 2147483647},
			{Name: "planId", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 2147483647},
			{Name: "validDays", Kind: ajvKindNullableInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: 36500},
			{Name: "expiresAt", Kind: ajvKindNullableString, MinLength: -1, MaxLength: 64},
		})
		if message != "" {
			respondAJVValidation(c, message)
			return
		}
		user := middleware.CurrentUser(c)

		packageID, _ := validated.nullableInt32("packageId")
		planID, planIDPresent := validated.nullableInt32("planId")
		var planIDPtr *int32
		if planIDPresent {
			planIDPtr = planID
		}
		validDays, _ := validated.nullableInt32("validDays")
		expiresAt, _ := validated.nullableString("expiresAt")

		auditUID := user.ID
		instance, err := services.RebindInstancePackage(ctx, hostID, instanceID, services.PackageBindingInput{
			PackageID: packageID,
			PlanID:    planIDPtr,
			ValidDays: validDays,
			ExpiresAt: expiresAt,
		}, &auditUID, "")
		if err != nil {
			var bindingErr *services.PackageBindingError
			if errors.As(err, &bindingErr) {
				c.JSON(bindingErr.StatusCode, gin.H{"error": bindingErr.Message})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "绑定实例套餐失败"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"instance": instance})
	})
}

// transferRouteHostID parses the :hostId param (integer, 1..2147483647).
func transferRouteHostID(c *gin.Context) (int32, bool) {
	raw := c.Param("hostId")
	parsed, err := parsePositiveInt64(raw)
	if err != nil || parsed < 1 || parsed > 2147483647 {
		respondAJVValidation(c, "params/hostId "+ajvIntMessage(err, parsed))
		return 0, false
	}
	return int32(parsed), true
}

func transferRouteIntParam(c *gin.Context, name string) (int32, bool) {
	raw := c.Param(name)
	parsed, err := parsePositiveInt64(raw)
	if err != nil || parsed < 1 || parsed > 2147483647 {
		respondAJVValidation(c, "params/"+name+" "+ajvIntMessage(err, parsed))
		return 0, false
	}
	return int32(parsed), true
}

func parsePositiveInt64(raw string) (int64, error) {
	var v int64
	_, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &v)
	return v, err
}

func ajvIntMessage(err error, parsed int64) string {
	if err != nil {
		return "must be integer"
	}
	if parsed < 1 {
		return "must be >= 1"
	}
	return "must be <= 2147483647"
}

// transferDecodeNodeTask re-marshals a node task map into the typed shape,
// defaulting the id to fallback and an empty status to "running".
func transferDecodeNodeTask(obj map[string]any, fallbackID string) transferNodeTask {
	encoded, err := json.Marshal(obj)
	if err == nil {
		var task transferNodeTask
		if json.Unmarshal(encoded, &task) == nil {
			if task.ID == "" {
				task.ID = fallbackID
			}
			if task.Status == "" {
				task.Status = "running"
			}
			return task
		}
	}
	return transferNodeTask{ID: fallbackID, Status: "running"}
}

// transferInstanceToRawMap converts a typed IncusInstance back to the raw map the
// snapshot extractor reads (config/devices/expanded_*).
func transferInstanceToRawMap(inst lib.IncusInstance) map[string]any {
	encoded, err := json.Marshal(inst)
	if err != nil {
		return map[string]any{}
	}
	var decoded map[string]any
	if json.Unmarshal(encoded, &decoded) != nil {
		return map[string]any{}
	}
	return decoded
}

func init() {
	Register(RegisterAdminHostInstanceTransfer)
}
