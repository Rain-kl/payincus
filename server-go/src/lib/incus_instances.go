// Port of server/src/lib/incus/incus-instances.ts (1039 lines), the instance-facing
// half of server/src/lib/incus/incus-storage.ts, and the parts of
// server/src/lib/incus/incus-utils.ts that incus_client.go does not already own
// (mapInstanceStatus → MapInstanceStatus; waitForOperation → the faithful
// IncusInstanceWaitForOperation below, plus the buffered auto-wait inside
// IncusClient.Request).
//
// This is the resource-delivery module (docs/WAVE_D_SPEC.md §4.1). Two invariants
// drive the whole file:
//
//  1. Idempotency and error shape match Node exactly, because the compensation
//     path in routes/instances.go decides whether to refund based on the error it
//     receives. `createInstance` re-throws the node's error verbatim (an existing
//     instance is a 409 that reaches the compensation path), and `deleteInstance`
//     treats a 404 as success and returns nil.
//  2. Nothing here waits forever. The busy-retry loop is capped at 5 minutes with
//     a 2s poll, and every operation wait carries an explicit timeout; a node that
//     stops answering surfaces as an error, never as a hung goroutine.
//
// The TypeScript uses axios with a buffered response; lib/incus_client.go's
// Request is also buffered and auto-waits on `type: async` envelopes through
// WaitForOperation, so no streaming seam is needed. IncusInstanceWaitForOperation
// is the explicit, progress-logging variant for callers that already hold an
// operation URL (kept exported because the backups/restore waves need it).
//
// `scripts/` do not call this module and there is no HTTP surface of its own, so
// it is covered by unit tests with an injected HTTP round-tripper rather than the
// dual-run conformance suite. It has NOT been verified against a real Incus node.
package lib

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// IncusInstance is the projection the operational helpers need from
// GET /1.0/instances/{name}. Config and Devices stay raw because callers merge and
// rewrite them verbatim, exactly like the TypeScript does.
type IncusInstance struct {
	Name            string                    `json:"name"`
	Type            string                    `json:"type"`
	Status          string                    `json:"status"`
	StatusCode      int                       `json:"status_code"`
	Config          map[string]string         `json:"config,omitempty"`
	Devices         map[string]map[string]any `json:"devices,omitempty"`
	Profiles        []string                  `json:"profiles,omitempty"`
	ExpandedDevices map[string]map[string]any `json:"expanded_devices,omitempty"`
	State           json.RawMessage           `json:"state,omitempty"`
}

// IncusSnapshot mirrors IncusSnapshot.
type IncusSnapshot struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	Stateful    bool   `json:"stateful,omitempty"`
}

// IncusExecOutput mirrors IncusExecOutput.
type IncusExecOutput struct {
	Stdout   string          `json:"stdout"`
	Stderr   string          `json:"stderr"`
	Metadata json.RawMessage `json:"metadata"`
}

// IncusInstanceBusyRetryOptions mirrors BusyRetryOptions.
type IncusInstanceBusyRetryOptions struct {
	AllowCancelBusyUpdate bool
	// BusyUpdateCancelAfter is `busyUpdateCancelAfterMs`. Zero means the default
	// (45 seconds).
	BusyUpdateCancelAfter time.Duration
}

// IncusInstanceConfigOptions mirrors BuildInstanceConfigOptions. Pointer fields
// preserve the TypeScript's "undefined vs explicit value" distinction: nil means
// the option was not provided, which changes several branches (swap limits, boot
// settings, bandwidth).
type IncusInstanceConfigOptions struct {
	GPUPCIAddresses []string
	Name            string
	Image           string
	CPU             int
	Memory          int
	Disk            int
	SwapEnabled     *bool
	SwapSize        *int32
	SSHKey          string
	Password        string
	Nested          bool
	Privileged      bool
	CloudInitConfig *IncusConfigPayload
	NetworkMode     string
	InstanceType    string
	StoragePool     *string
	IPv4Address     *string
	IPv6Address     *string
	IPv6Gateway     *string
	NetworkBridge   *string
	IPv6Config      *NetworkPayloadIPv6Config
	HostInterface   *string

	LimitsRead              *string
	LimitsWrite             *string
	LimitsReadIops          *int32
	LimitsWriteIops         *int32
	LimitsIngress           *string
	LimitsEgress            *string
	LimitsProcesses         *int32
	LimitsCPUPriority       *int32
	BootAutostart           *bool
	BootAutostartPriority   *int32
	BootAutostartDelay      *int32
	BootHostShutdownTimeout *int32
}

// IncusInstanceBuildNetworkDevicesOptions mirrors BuildNetworkDevicesOptions.
type IncusInstanceBuildNetworkDevicesOptions struct {
	NetworkMode   string
	HostInterface string
	IPv6Config    *NetworkPayloadIPv6Config
	LimitsIngress *string
	LimitsEgress  *string
	NatBridge     string
	IPv4Address   *string
	Eth0Hwaddr    *string
	Eth1Hwaddr    *string
}

// IncusInstanceProvisionConfig mirrors routes' instancesCreateConfig field for
// field, so the wiring adapter in routes/instances.go is a pure projection.
type IncusInstanceProvisionConfig struct {
	Name                    string
	Image                   string
	CPU                     int32
	Memory                  int32
	Disk                    int32
	SwapEnabled             bool
	SwapSize                *int32
	CloudInitConfig         IncusConfigPayload
	NetworkMode             string
	Nested                  bool
	Privileged              bool
	InstanceType            string
	StoragePool             *string
	IPv4Address             *string
	IPv6Address             *string
	IPv6Gateway             *string
	HostInterface           string
	LimitsRead              *string
	LimitsWrite             *string
	LimitsReadIops          *int32
	LimitsWriteIops         *int32
	LimitsIngress           *string
	LimitsEgress            *string
	LimitsProcesses         *int32
	LimitsCPUPriority       *int32
	BootAutostart           *bool
	BootAutostartPriority   *int32
	BootAutostartDelay      *int32
	BootHostShutdownTimeout *int32
}

// IncusInstanceProvisionInput mirrors InstancesIncusProvisionInput.
type IncusInstanceProvisionInput struct {
	Host    *db.HostsRecord
	Config  IncusInstanceProvisionConfig
	GPUPCIs []string
}

// incusInstanceOperationTimeout is the default async-operation budget for the
// instance/snapshot calls below (30 minutes, matching the TypeScript default).
const incusInstanceOperationTimeout = 30 * time.Minute

// Busy-retry timings mirror INSTANCE_BUSY_* exactly. They are exported so the
// routes layer can assert the contract without re-declaring the numbers.
const (
	IncusInstanceBusyWaitTimeout   = 300 * time.Second
	IncusInstanceBusyPollInterval  = 2 * time.Second
	IncusInstanceBusyCancelCheck   = 30 * time.Second
	IncusInstanceBusyCancelStale   = 45 * time.Second
	incusInstanceBusyLogInterval   = 10 * time.Second
	incusInstanceQemuLogReadLimit  = 65536
	incusInstanceVMStartWaitRounds = 90
	incusInstanceVMStartWaitSleep  = 2 * time.Second
)

// incusInstanceBusyPattern mirrors /busy running a "([^"]+)" operation/i.
var incusInstanceBusyPattern = regexp.MustCompile(`(?i)busy running a "([^"]+)" operation`)

// incusInstanceGbitPattern mirrors /^(\d+(?:\.\d+)?)\s*Gbit$/i.
var incusInstanceGbitPattern = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)\s*Gbit$`)

// incusInstanceProgressKeys mirrors the metadata keys waitForOperation logs.
var incusInstanceProgressKeys = map[string]bool{
	"download_progress": true,
	"fs_progress":       true,
	"create_instance_from_image_unpack_progress": true,
	"progress":        true,
	"unpack_progress": true,
}

// ==================== instance reads ====================

// GetInstance mirrors getInstance(client, name).
func GetInstance(ctx context.Context, client *IncusClient, name string) (*IncusInstance, error) {
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/instances/"+name, nil, incusInstanceOperationTimeout, nil)
	if err != nil {
		return nil, err
	}

	var instance IncusInstance
	if err := json.Unmarshal(raw, &instance); err != nil {
		return nil, NewIncusAPIError("节点返回的实例数据格式无效", 502, nil)
	}
	return &instance, nil
}

// incusInstanceFetchRaw mirrors `getInstance(client, name)` when the caller spreads
// the whole object back into a PUT. A typed projection would silently drop every
// field it does not know (description, expanded_config, ...), and Incus PUT is a
// full replacement, so the raw object is the only faithful carrier.
func incusInstanceFetchRaw(ctx context.Context, client *IncusClient, name string) (map[string]any, error) {
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/instances/"+name, nil, incusInstanceOperationTimeout, nil)
	if err != nil {
		return nil, err
	}
	var instance map[string]any
	if err := json.Unmarshal(raw, &instance); err != nil {
		return nil, NewIncusAPIError("节点返回的实例数据格式无效", 502, nil)
	}
	return instance, nil
}

// IncusInstanceGetState mirrors getInstanceState(client, name).
func IncusInstanceGetState(ctx context.Context, client *IncusClient, name string) (json.RawMessage, error) {
	return client.Request(ctx, http.MethodGet, "/1.0/instances/"+name+"/state", nil, incusInstanceOperationTimeout, nil)
}

// IncusInstanceList mirrors listInstances(client): GET /1.0/instances returns URL
// paths, so every entry is read with a follow-up getInstance.
func IncusInstanceList(ctx context.Context, client *IncusClient) ([]IncusInstance, error) {
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/instances", nil, incusInstanceOperationTimeout, nil)
	if err != nil {
		return nil, err
	}

	var urls []string
	if err := json.Unmarshal(raw, &urls); err != nil {
		return nil, NewIncusAPIError("节点返回的实例列表格式无效", 502, nil)
	}

	instances := make([]IncusInstance, 0, len(urls))
	for _, instanceURL := range urls {
		parts := strings.Split(instanceURL, "/")
		name := parts[len(parts)-1]
		instance, err := GetInstance(ctx, client, name)
		if err != nil {
			return nil, err
		}
		instances = append(instances, *instance)
	}
	return instances, nil
}

// ==================== instance writes ====================

// IncusInstanceCreate mirrors createInstance(client, config, instanceId).
//
// The egress protection runs before the POST, and a failure re-checks whether the
// instance exists: a timeout or a duplicate create may already have left one on
// the node, and its egress binding must be retained until Incus confirms absence.
func IncusInstanceCreate(ctx context.Context, client *IncusClient, config map[string]any, instanceID *int32) (json.RawMessage, error) {
	name, _ := config["name"].(string)
	action := func(taskCtx context.Context) (json.RawMessage, error) {
		protected, err := EgressAttachmentProtectProvisionedNetwork(taskCtx, client, config, instanceID)
		if err == nil {
			result, requestErr := client.Request(taskCtx, http.MethodPost, "/1.0/instances", protected,
				incusInstanceOperationTimeout, protected)
			if requestErr == nil {
				return result, nil
			}
			err = requestErr
		}

		// Best-effort: only release the binding when the node confirms the instance
		// does not exist. Any other failure keeps it.
		if _, checkErr := client.Request(taskCtx, http.MethodGet, "/1.0/instances/"+url.PathEscape(name),
			nil, incusInstanceOperationTimeout, nil); checkErr != nil {
			if IsIncusNotFound(checkErr) {
				_ = EgressAttachmentRelease(taskCtx, client, name)
			}
		}
		return nil, err
	}

	if CurrentOperationTask(ctx) == nil && taskDBReady() {
		var result json.RawMessage
		hostID := int32(client.options.HostID)
		runErr := RunOperationTask(ctx, hostID, name, OperationTaskKindCreate, func(taskCtx context.Context) error {
			var innerErr error
			result, innerErr = action(taskCtx)
			return innerErr
		}, nil)
		return result, runErr
	}
	return action(ctx)
}

// IncusInstanceStart mirrors startInstance(client, name): ensure the package egress
// binding, re-apply the GPU passthrough for GPU instances, then PUT start.
func IncusInstanceStart(ctx context.Context, client *IncusClient, name string) (json.RawMessage, error) {
	if err := EgressAttachmentEnsureBeforeStart(ctx, client, name); err != nil {
		return nil, err
	}

	hostID := int32(client.options.HostID)
	if hostID != 0 {
		instanceID, found, err := incusInstanceFindGPUInstance(ctx, hostID, name)
		if err != nil {
			return nil, err
		}
		if found {
			pci, err := db.GpusGetInstancePCIs(ctx, instanceID, hostID)
			if err != nil {
				return nil, err
			}
			live, err := incusInstanceFetchRaw(ctx, client, name)
			if err != nil {
				return nil, err
			}
			// The TypeScript passes `pci!`; an empty list therefore also fails, just
			// with the GPU validation message instead of a undefined.length TypeError.
			if err := ApplyGpuDevice(live, pci); err != nil {
				return nil, err
			}
			if _, err := client.Request(ctx, http.MethodPut, "/1.0/instances/"+name, live,
				incusInstanceOperationTimeout, live); err != nil {
				return nil, err
			}
		}
	}

	body := map[string]any{"action": "start", "timeout": 60}
	return client.Request(ctx, http.MethodPut, "/1.0/instances/"+name+"/state", body,
		incusInstanceOperationTimeout, body)
}

// IncusInstanceStop mirrors stopInstance(client, name, force, options).
//
// A container that is already stopped is left alone: Alpine images occasionally
// keep a stuck `update` operation, and sending a second stop then fails even
// though the workload is down.
func IncusInstanceStop(ctx context.Context, client *IncusClient, name string, force bool,
	options IncusInstanceBusyRetryOptions) (json.RawMessage, error) {
	return incusInstanceRunWithBusyRetry(ctx, client, name, "stop the instance",
		func() (json.RawMessage, error) {
			state, err := IncusInstanceGetState(ctx, client, name)
			if err != nil {
				return nil, err
			}
			if incusInstanceExtractStatus(state) == "stopped" {
				return state, nil
			}
			body := map[string]any{"action": "stop", "timeout": 60, "force": force}
			return client.Request(ctx, http.MethodPut, "/1.0/instances/"+name+"/state", body,
				incusInstanceOperationTimeout, body)
		}, options)
}

// IncusInstanceRestart mirrors restartInstance(client, name, force).
func IncusInstanceRestart(ctx context.Context, client *IncusClient, name string, force bool) (json.RawMessage, error) {
	body := map[string]any{"action": "restart", "timeout": 60, "force": force}
	return client.Request(ctx, http.MethodPut, "/1.0/instances/"+name+"/state", body,
		incusInstanceOperationTimeout, body)
}

// IncusInstanceFreeze mirrors freezeInstance(client, name).
func IncusInstanceFreeze(ctx context.Context, client *IncusClient, name string) (json.RawMessage, error) {
	body := map[string]any{"action": "freeze", "timeout": 60}
	return client.Request(ctx, http.MethodPut, "/1.0/instances/"+name+"/state", body,
		incusInstanceOperationTimeout, body)
}

// IncusInstanceUnfreeze mirrors unfreezeInstance(client, name).
func IncusInstanceUnfreeze(ctx context.Context, client *IncusClient, name string) (json.RawMessage, error) {
	body := map[string]any{"action": "unfreeze", "timeout": 30}
	return client.Request(ctx, http.MethodPut, "/1.0/instances/"+name+"/state", body,
		incusInstanceOperationTimeout, body)
}

// IncusInstanceRebuild mirrors rebuildInstance(client, name, imageSource, config,
// options). A `images:` alias is pinned to the public simplestreams remote; every
// other source is forwarded verbatim.
func IncusInstanceRebuild(ctx context.Context, client *IncusClient, name string,
	imageSource map[string]string, config map[string]string,
	options IncusInstanceBusyRetryOptions) (json.RawMessage, error) {
	source := map[string]any{}
	if alias := imageSource["alias"]; strings.HasPrefix(alias, "images:") {
		source["type"] = "image"
		source["mode"] = "pull"
		source["server"] = "https://images.linuxcontainers.org"
		source["protocol"] = "simplestreams"
		source["alias"] = strings.Replace(alias, "images:", "", 1)
	} else {
		source["type"] = "image"
		for key, value := range imageSource {
			if value != "" {
				source[key] = value
			}
		}
	}

	payload := map[string]any{"source": source}
	if len(config) > 0 {
		payload["config"] = config
	}

	// rebuild is slow: the TypeScript gives it 5 minutes.
	return incusInstanceRunWithBusyRetry(ctx, client, name, "rebuild the instance",
		func() (json.RawMessage, error) {
			return client.Request(ctx, http.MethodPost, "/1.0/instances/"+name+"/rebuild", payload,
				300*time.Second, payload)
		}, options)
}

// IncusInstanceDelete mirrors deleteInstance(client, name). A 404 means the instance
// is already gone: it releases the local GPU/egress bookkeeping and returns nil so
// the compensation path does not treat an idempotent delete as a failure.
func IncusInstanceDelete(ctx context.Context, client *IncusClient, name string) (json.RawMessage, error) {
	action := func(taskCtx context.Context) (json.RawMessage, error) {
		// Capture the final QEMU error before Incus removes the log directory.
		logPath := "/1.0/instances/" + url.PathEscape(name) + "/logs/qemu.log"
		content, statusCode, readErr := client.ReadFile(taskCtx, logPath, incusInstanceQemuLogReadLimit)
		if readErr != nil {
			TaskLog(taskCtx, "warn", "删除前节点 QEMU 日志不可读，继续删除流程")
		} else if statusCode == http.StatusOK || statusCode == http.StatusPartialContent {
			if strings.TrimSpace(content) != "" {
				for _, line := range strings.Split(content, "\n") {
					if strings.TrimSpace(line) != "" {
						TaskLog(taskCtx, "info", "[节点 QEMU 删除前日志] "+line)
					}
				}
			}
		}

		TaskLog(taskCtx, "info", "向节点提交删除实例 "+name)
		result, err := client.Request(taskCtx, http.MethodDelete, "/1.0/instances/"+name, nil,
			incusInstanceOperationTimeout, nil)
		if err != nil {
			if !IsIncusNotFound(err) {
				return nil, err
			}
			if gpuErr := incusInstanceReleaseFailedGpuByName(taskCtx, client, name); gpuErr != nil {
				return nil, gpuErr
			}
			_ = EgressAttachmentRelease(taskCtx, client, name)
			return nil, nil
		}

		if gpuErr := incusInstanceReleaseFailedGpuByName(taskCtx, client, name); gpuErr != nil {
			return nil, gpuErr
		}
		_ = EgressAttachmentRelease(taskCtx, client, name)
		return result, nil
	}

	if CurrentOperationTask(ctx) == nil && taskDBReady() {
		var result json.RawMessage
		hostID := int32(client.options.HostID)
		runErr := RunOperationTask(ctx, hostID, name, OperationTaskKindDestroy, func(taskCtx context.Context) error {
			var innerErr error
			result, innerErr = action(taskCtx)
			return innerErr
		}, nil)
		return result, runErr
	}
	return action(ctx)
}

// UpdateInstance mirrors updateInstance(client, name, updates).
//
// The TypeScript merges `config` instead of replacing it, because Incus treats PUT
// as a full replacement: sending only the changed keys would silently wipe every
// other setting. `undefined` entries are dropped before the merge. The whole
// current instance object is spread back, so `description` survives a PUT.
func UpdateInstance(ctx context.Context, client *IncusClient, name string, updates map[string]any) (json.RawMessage, error) {
	return incusInstanceRunWithBusyRetry(ctx, client, name, "update the instance configuration",
		func() (json.RawMessage, error) {
			current, err := incusInstanceFetchRaw(ctx, client, name)
			if err != nil {
				return nil, err
			}

			sanitized := make(map[string]any, len(updates))
			for key, value := range updates {
				if value == nil {
					continue
				}
				sanitized[key] = value
			}

			if patch, ok := sanitized["config"]; ok && current["config"] != nil {
				merged := make(map[string]any)
				if existing, ok := current["config"].(map[string]any); ok {
					for key, value := range existing {
						merged[key] = value
					}
				}
				switch typed := patch.(type) {
				case map[string]any:
					for key, value := range typed {
						merged[key] = value
					}
				case map[string]string:
					for key, value := range typed {
						merged[key] = value
					}
				}
				sanitized["config"] = merged
			}

			body := make(map[string]any, len(current)+len(sanitized))
			for key, value := range current {
				body[key] = value
			}
			for key, value := range sanitized {
				body[key] = value
			}

			return client.Request(ctx, http.MethodPut, "/1.0/instances/"+name, body,
				incusInstanceOperationTimeout, updates)
		}, IncusInstanceBusyRetryOptions{})
}

// PatchInstanceResources mirrors patchInstanceResources(client, name, resources).
//
// It sends PATCH (not PUT) so only the named limits move. A memory change is
// expressed as "NNNMB" and a disk change rewrites the root device while preserving
// every other attribute it already had — dropping those attributes would detach the
// storage pool.
func PatchInstanceResources(
	ctx context.Context,
	client *IncusClient,
	name string,
	cpu *int,
	memory *int,
	disk *int,
) (json.RawMessage, error) {
	patch := map[string]any{}
	config := map[string]any{}

	if cpu != nil {
		config["limits.cpu"] = formatAllowanceCores(*cpu)
		config["limits.cpu.allowance"] = formatAllowancePercent(*cpu)
	}
	if memory != nil {
		config["limits.memory"] = formatMemoryLimit(*memory)
	}
	if len(config) > 0 {
		patch["config"] = config
	}

	if disk != nil {
		current, err := GetInstance(ctx, client, name)
		if err != nil {
			return nil, err
		}

		root := map[string]any{}
		if existing, ok := current.Devices["root"]; ok {
			for key, value := range existing {
				root[key] = value
			}
		}
		root["path"] = "/"
		if _, ok := root["pool"]; !ok {
			root["pool"] = "default"
		}
		root["type"] = "disk"
		root["size"] = formatDiskSize(*disk)

		patch["devices"] = map[string]any{"root": root}
	}

	return client.Request(ctx, http.MethodPatch, "/1.0/instances/"+name, patch, incusInstanceOperationTimeout, patch)
}

// IncusInstanceUpdateIpv6Addresses mirrors updateInstanceIpv6Addresses.
//
// PATCH (not PUT) so the volatile runtime keys can be cleared in the same request
// that sets ipv6.address; a separate PUT would deadlock on Incus' validation.
func IncusInstanceUpdateIpv6Addresses(ctx context.Context, client *IncusClient, instanceName string,
	ipv6List []string, deviceName, networkName string, restart bool) (json.RawMessage, error) {
	if deviceName == "" {
		deviceName = "eth0"
	}
	if networkName == "" {
		networkName = "incusbr0"
	}

	instance, err := GetInstance(ctx, client, instanceName)
	if err != nil {
		return nil, err
	}

	eth0Config := map[string]any{}
	if existing, ok := instance.Devices[deviceName]; ok {
		for key, value := range existing {
			eth0Config[key] = value
		}
	}

	addresses := make([]string, 0, len(ipv6List))
	for _, address := range ipv6List {
		trimmed := strings.TrimSpace(address)
		if trimmed == "" {
			continue
		}
		addresses = append(addresses, trimmed)
	}
	ipString := strings.Join(addresses, ",")

	state, stateErr := IncusInstanceGetState(ctx, client, instanceName)
	isRunning := false
	if stateErr == nil {
		var decoded struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(state, &decoded) == nil && decoded.Status == "Running" {
			isRunning = true
		}
	}

	device := eth0Config
	device["name"] = deviceName
	device["type"] = "nic"
	device["network"] = networkName
	device["ipv6.address"] = ipString
	device["security.ipv6_filtering"] = "true"
	// Explicit null: Incus treats PATCH as incremental, so a leftover empty
	// ipv4.address would keep failing validation as "Item \"\": Not an IP address".
	device["ipv4.address"] = nil

	patch := map[string]any{
		"devices": map[string]any{deviceName: device},
	}
	if isRunning {
		patch["config"] = map[string]any{
			"volatile." + deviceName + ".host_name":               nil,
			"volatile." + deviceName + ".hwaddr":                  nil,
			"volatile." + deviceName + ".last_state.ip_addresses": nil,
		}
	}

	result, err := client.Request(ctx, http.MethodPatch, "/1.0/instances/"+instanceName, patch,
		incusInstanceOperationTimeout, patch)
	if err != nil {
		return nil, err
	}

	if restart && isRunning {
		// A failed restart must not fail the update: the config is already applied.
		if _, restartErr := IncusInstanceRestart(ctx, client, instanceName, false); restartErr != nil {
			log.Printf("[updateInstanceIpv6Addresses] 重启容器 %s 失败: %v", instanceName, restartErr)
		}
	}
	return result, nil
}

// IncusInstanceAddDevice mirrors addDevice(client, instanceName, deviceName, device).
func IncusInstanceAddDevice(ctx context.Context, client *IncusClient, instanceName, deviceName string,
	device map[string]any) (json.RawMessage, error) {
	instance, err := incusInstanceFetchRaw(ctx, client, instanceName)
	if err != nil {
		return nil, err
	}

	devices := map[string]any{}
	if existing, ok := instance["devices"].(map[string]any); ok {
		for key, value := range existing {
			devices[key] = value
		}
	}
	devices[deviceName] = device

	body := make(map[string]any, len(instance)+1)
	for key, value := range instance {
		body[key] = value
	}
	body["devices"] = devices
	return client.Request(ctx, http.MethodPut, "/1.0/instances/"+instanceName, body,
		incusInstanceOperationTimeout, body)
}

// IncusInstanceRemoveDevice mirrors removeDevice(client, instanceName, deviceName).
// A missing device is a no-op that returns nil, not an error.
func IncusInstanceRemoveDevice(ctx context.Context, client *IncusClient, instanceName, deviceName string) (json.RawMessage, error) {
	instance, err := incusInstanceFetchRaw(ctx, client, instanceName)
	if err != nil {
		return nil, err
	}

	existingDevices, ok := instance["devices"].(map[string]any)
	if !ok {
		return nil, nil
	}
	if _, present := existingDevices[deviceName]; !present {
		return nil, nil
	}

	devices := make(map[string]any, len(existingDevices))
	for key, value := range existingDevices {
		if key == deviceName {
			continue
		}
		devices[key] = value
	}

	body := make(map[string]any, len(instance)+1)
	for key, value := range instance {
		body[key] = value
	}
	body["devices"] = devices
	return client.Request(ctx, http.MethodPut, "/1.0/instances/"+instanceName, body,
		incusInstanceOperationTimeout, body)
}

// ==================== config builder ====================

// IncusInstanceBuildConfig mirrors buildInstanceConfig(options).
//
// Everything is byte-identical to the TypeScript: the five network modes, the
// bridged/routed split, the Gbit→Mbit normalisation of legacy bandwidth values, the
// cloud-init fallback, and the routed-IPv6 network-config override that is skipped
// for VMs (their MAC-matched config must not be replaced).
func IncusInstanceBuildConfig(options IncusInstanceConfigOptions) (map[string]any, error) {
	name := options.Name
	instanceType := options.InstanceType
	if instanceType == "" {
		instanceType = "container"
	}
	networkMode := options.NetworkMode
	if networkMode == "" {
		networkMode = "nat"
	}

	storagePool := ""
	if options.StoragePool != nil {
		storagePool = *options.StoragePool
	}
	if storagePool == "" {
		storagePool = "default"
	}

	hostInterface := ""
	if options.HostInterface != nil {
		hostInterface = *options.HostInterface
	}
	if hostInterface == "" {
		hostInterface = "eth0"
	}

	networkBridge := ""
	if options.NetworkBridge != nil {
		networkBridge = *options.NetworkBridge
	}
	if networkBridge == "" {
		networkBridge = "incusbr0"
	}

	normalizedIngress := incusInstanceNormalizeBandwidth(options.LimitsIngress)
	normalizedEgress := incusInstanceNormalizeBandwidth(options.LimitsEgress)

	configObj := map[string]any{
		"limits.cpu":           strconv.Itoa(AllowanceToCores(options.CPU)),
		"limits.cpu.allowance": strconv.Itoa(options.CPU) + "%",
		"limits.memory":        strconv.Itoa(options.Memory) + "MB",
	}

	if instanceType == "container" && (options.SwapEnabled != nil || options.SwapSize != nil) {
		swapEnabled := options.SwapEnabled != nil && *options.SwapEnabled
		configObj["limits.memory.swap"] = InstanceSwapResolveIncusValue(swapEnabled, options.SwapSize)
	}

	rootDevice := map[string]any{
		"path": "/",
		"pool": storagePool,
		"type": "disk",
		"size": strconv.Itoa(options.Disk) + "MB",
	}

	// Incus allows only one kind per direction: bandwidth or IOPS. Bandwidth wins.
	if options.LimitsRead != nil && *options.LimitsRead != "" {
		rootDevice["limits.read"] = *options.LimitsRead
	} else if options.LimitsReadIops != nil {
		rootDevice["limits.read"] = strconv.FormatInt(int64(*options.LimitsReadIops), 10) + "iops"
	}
	if options.LimitsWrite != nil && *options.LimitsWrite != "" {
		rootDevice["limits.write"] = *options.LimitsWrite
	} else if options.LimitsWriteIops != nil {
		rootDevice["limits.write"] = strconv.FormatInt(int64(*options.LimitsWriteIops), 10) + "iops"
	}

	if options.LimitsProcesses != nil {
		configObj["limits.processes"] = strconv.FormatInt(int64(*options.LimitsProcesses), 10)
	}
	// limits.cpu.priority takes 0-10 directly; Incus maps it to cgroups/nice itself.
	if options.LimitsCPUPriority != nil {
		configObj["limits.cpu.priority"] = strconv.FormatInt(int64(*options.LimitsCPUPriority), 10)
	}

	if options.BootAutostart != nil {
		if *options.BootAutostart {
			configObj["boot.autostart"] = "true"
		} else {
			configObj["boot.autostart"] = "false"
		}
	}
	if options.BootAutostartPriority != nil {
		configObj["boot.autostart.priority"] = strconv.FormatInt(int64(*options.BootAutostartPriority), 10)
	}
	if options.BootAutostartDelay != nil {
		configObj["boot.autostart.delay"] = strconv.FormatInt(int64(*options.BootAutostartDelay), 10)
	}
	if options.BootHostShutdownTimeout != nil {
		configObj["boot.host_shutdown_timeout"] = strconv.FormatInt(int64(*options.BootHostShutdownTimeout), 10)
	}

	devices := map[string]any{"root": rootDevice}

	if instanceType == "vm" {
		// Some cloud images (RHEL family) need the agent config ISO at boot.
		devices["agent"] = map[string]any{"type": "disk", "source": "agent:config"}
	}

	effectiveIpv6Config := options.IPv6Config
	if effectiveIpv6Config == nil && incusInstanceNonEmpty(options.IPv6Address) {
		effectiveIpv6Config = &NetworkPayloadIPv6Config{PrimaryIP: *options.IPv6Address}
	}

	var eth0Mac, eth1Mac string
	if instanceType == "vm" {
		eth0Mac, eth1Mac = networkPayloadGenerateVmNicMacs(name)
	}

	// New architecture: a static IPv4 (all creation paths) or routed IPv6.
	useNewNetworkArchitecture := incusInstanceNonEmpty(options.IPv4Address) ||
		(NetworkModeNeedsRoutedIpv6(networkMode) && effectiveIpv6Config != nil && hostInterface != "")

	if useNewNetworkArchitecture {
		networkDevices := IncusInstanceBuildNetworkDevices(IncusInstanceBuildNetworkDevicesOptions{
			NetworkMode:   networkMode,
			HostInterface: hostInterface,
			IPv6Config:    effectiveIpv6Config,
			LimitsIngress: normalizedIngress,
			LimitsEgress:  normalizedEgress,
			NatBridge:     networkBridge,
			IPv4Address:   options.IPv4Address,
			Eth0Hwaddr:    incusInstanceStringPtrOrNil(eth0Mac),
			Eth1Hwaddr:    incusInstanceStringPtrOrNil(eth1Mac),
		})
		for deviceName, device := range networkDevices {
			devices[deviceName] = device
		}
	} else {
		// Legacy single-NIC NAT layout, kept for backwards compatibility.
		needsExplicitNetwork := instanceType == "vm" ||
			incusInstanceNonEmpty(options.IPv4Address) ||
			incusInstanceNonEmpty(options.IPv6Address) ||
			incusInstanceNonEmpty(normalizedIngress) ||
			incusInstanceNonEmpty(normalizedEgress) ||
			networkMode != "nat"

		if needsExplicitNetwork {
			eth0Device := map[string]any{
				"type":    "nic",
				"name":    "eth0",
				"network": networkBridge,
			}
			if eth0Mac != "" {
				eth0Device["hwaddr"] = eth0Mac
			}
			if incusInstanceNonEmpty(options.IPv4Address) {
				eth0Device["ipv4.address"] = *options.IPv4Address
			}
			if incusInstanceNonEmpty(normalizedIngress) {
				eth0Device["limits.ingress"] = *normalizedIngress
			}
			if incusInstanceNonEmpty(normalizedEgress) {
				eth0Device["limits.egress"] = *normalizedEgress
			}
			devices["eth0"] = eth0Device
		}
	}

	// Image source: an "images:" prefix pulls from the public simplestreams remote.
	var source map[string]any
	if strings.HasPrefix(options.Image, "images:") {
		source = map[string]any{
			"type":     "image",
			"mode":     "pull",
			"server":   "https://images.linuxcontainers.org",
			"protocol": "simplestreams",
			"alias":    strings.Replace(options.Image, "images:", "", 1),
		}
	} else {
		source = map[string]any{"type": "image", "alias": options.Image}
	}

	instanceTypeName := "container"
	if instanceType == "vm" {
		instanceTypeName = "virtual-machine"
	}

	config := map[string]any{
		"name":    name,
		"type":    instanceTypeName,
		"source":  source,
		"config":  configObj,
		"devices": devices,
	}

	// Allow DD/secure-boot changes inside the guest.
	if instanceType == "vm" {
		configObj["security.secureboot"] = "false"
	}
	if options.Nested {
		configObj["security.nesting"] = "true"
	}
	if options.Privileged {
		configObj["security.privileged"] = "true"
	}

	if options.CloudInitConfig != nil && options.CloudInitConfig.UserUserData != "" {
		configObj["user.user-data"] = options.CloudInitConfig.UserUserData
		// The generator already emits network-config (needed for RHEL/Alpine DHCPv6).
		if options.CloudInitConfig.UserNetworkConfig != "" {
			configObj["user.network-config"] = options.CloudInitConfig.UserNetworkConfig
		}
	} else {
		configObj["user.user-data"] = incusInstanceLegacyCloudInit(name, options.SSHKey, options.Password)
	}

	// The dual-NIC architecture needs a v2 network-config with eth0 + eth1. The
	// container generator's v1 config only covers eth0, and VM configs are
	// MAC-matched, so both are excluded here.
	if NetworkModeNeedsRoutedIpv6(networkMode) && NetworkModeNeedsNatIpv4(networkMode) &&
		effectiveIpv6Config != nil && incusInstanceNonEmpty(options.IPv4Address) && instanceType != "vm" {
		ipv6List := make([]string, 0, 1+len(effectiveIpv6Config.ExtraIPs))
		ipv6List = append(ipv6List, effectiveIpv6Config.PrimaryIP)
		ipv6List = append(ipv6List, effectiveIpv6Config.ExtraIPs...)
		configObj["user.network-config"] = networkPayloadGenerateCloudInitNetworkConfig(*options.IPv4Address, ipv6List)
	}

	if len(options.GPUPCIAddresses) > 0 {
		if err := ApplyGpuDevice(config, options.GPUPCIAddresses); err != nil {
			return nil, err
		}
	}

	return config, nil
}

// IncusInstanceBuildNetworkDevices mirrors buildNetworkDevices(options).
//
// eth0 is bridged for NAT modes and routed for the public-IPv4 modes; eth1 is the
// routed IPv6 NIC and only exists for the routed-IPv6 modes. security.*_filtering
// is bridged-only, so routed devices must not carry it.
func IncusInstanceBuildNetworkDevices(options IncusInstanceBuildNetworkDevicesOptions) map[string]map[string]string {
	natBridge := options.NatBridge
	if natBridge == "" {
		natBridge = "incusbr0"
	}

	devices := map[string]map[string]string{}

	eth0Device := map[string]string{"type": "nic", "name": "eth0"}
	if NetworkModeNeedsPublicIpv4(options.NetworkMode) {
		eth0Device["nictype"] = "routed"
		eth0Device["parent"] = options.HostInterface
	} else {
		eth0Device["nictype"] = "bridged"
		eth0Device["parent"] = natBridge
	}
	if incusInstanceNonEmpty(options.IPv4Address) {
		eth0Device["ipv4.address"] = *options.IPv4Address
	}
	if incusInstanceNonEmpty(options.Eth0Hwaddr) {
		eth0Device["hwaddr"] = *options.Eth0Hwaddr
	}
	if incusInstanceNonEmpty(options.LimitsIngress) {
		eth0Device["limits.ingress"] = *options.LimitsIngress
	}
	if incusInstanceNonEmpty(options.LimitsEgress) {
		eth0Device["limits.egress"] = *options.LimitsEgress
	}
	devices["eth0"] = eth0Device

	if NetworkModeNeedsRoutedIpv6(options.NetworkMode) && options.IPv6Config != nil {
		allIPs := make([]string, 0, 1+len(options.IPv6Config.ExtraIPs))
		allIPs = append(allIPs, options.IPv6Config.PrimaryIP)
		allIPs = append(allIPs, options.IPv6Config.ExtraIPs...)

		eth1Device := map[string]string{
			"type":         "nic",
			"nictype":      "routed",
			"parent":       options.HostInterface,
			"name":         "eth1",
			"ipv6.address": strings.Join(allIPs, ","),
		}
		if incusInstanceNonEmpty(options.Eth1Hwaddr) {
			eth1Device["hwaddr"] = *options.Eth1Hwaddr
		}
		if options.IPv6Config.Subnet != nil && *options.IPv6Config.Subnet != "" {
			eth1Device["ipv6.routes"] = *options.IPv6Config.Subnet
		}
		devices["eth1"] = eth1Device
	}

	return devices
}

// incusInstanceLegacyCloudInit mirrors the pre-generator cloud-init branch: root
// password, SSH key and the sshd PermitRootLogin fix-ups.
func incusInstanceLegacyCloudInit(name, sshKey, password string) string {
	lines := []string{
		"#cloud-config",
		"hostname: " + name,
		"manage_etc_hosts: true",
	}
	if password != "" {
		lines = append(lines,
			"chpasswd:",
			"  expire: false",
			"  list:",
			"    - root:"+password,
			"ssh_pwauth: true",
		)
	}
	if sshKey != "" {
		lines = append(lines,
			"ssh_authorized_keys:",
			"  - "+strings.TrimSpace(sshKey),
		)
	}
	lines = append(lines,
		"disable_root: false",
		"runcmd:",
		"  - sed -i 's/#PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config",
		"  - sed -i 's/PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config",
		"  - systemctl restart sshd || service ssh restart || true",
	)
	return strings.Join(lines, "\n")
}

// incusInstanceNormalizeBandwidth mirrors normalizeBandwidth: legacy Gbit values
// are converted to Mbit because Incus support for Gbit varies by version. A falsy
// input stays nil so `if (limitsIngress)` keeps its meaning.
func incusInstanceNormalizeBandwidth(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	match := incusInstanceGbitPattern.FindStringSubmatch(*value)
	if match == nil {
		return value
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return value
	}
	converted := strconv.FormatInt(int64(math.Round(number*1000)), 10) + "Mbit"
	return &converted
}

// incusInstanceNonEmpty mirrors JavaScript truthiness for an optional string.
func incusInstanceNonEmpty(value *string) bool {
	return value != nil && *value != ""
}

// incusInstanceStringPtrOrNil returns nil for "" so optional knobs stay absent
// from the JSON body instead of becoming an empty string.
func incusInstanceStringPtrOrNil(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// ==================== exec / console / bridge ====================

// IncusInstanceExec mirrors exec(client, instanceName, command, options, timeout).
func IncusInstanceExec(ctx context.Context, client *IncusClient, instanceName string, command any,
	options map[string]any, timeout time.Duration) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}

	var commandList []string
	switch typed := command.(type) {
	case string:
		commandList = []string{"/bin/sh", "-c", typed}
	case []string:
		commandList = typed
	case []any:
		commandList = make([]string, 0, len(typed))
		for _, entry := range typed {
			text, ok := entry.(string)
			if !ok {
				return nil, NewIncusAPIError("Invalid exec command", 400, command)
			}
			commandList = append(commandList, text)
		}
	default:
		return nil, NewIncusAPIError("Invalid exec command", 400, command)
	}

	body := map[string]any{
		"command":            commandList,
		"wait-for-websocket": false,
		"interactive":        false,
	}
	for key, value := range options {
		body[key] = value
	}

	return client.Request(ctx, http.MethodPost, "/1.0/instances/"+instanceName+"/exec", body, timeout, body)
}

// incusInstanceExtractOutputFilePath mirrors extractOutputFilePath(metadata, fd).
func incusInstanceExtractOutputFilePath(metadata json.RawMessage, fd string) string {
	if len(metadata) == 0 {
		return ""
	}
	var decoded struct {
		Output map[string]any `json:"output"`
	}
	if err := json.Unmarshal(metadata, &decoded); err != nil {
		return ""
	}
	path, _ := decoded.Output[fd].(string)
	return path
}

// IncusInstanceExecWithOutput mirrors execWithOutput: `record-output: true` then
// read both files. A failed read yields an empty string, exactly like the
// TypeScript's `.catch(() => ”)`.
func IncusInstanceExecWithOutput(ctx context.Context, client *IncusClient, instanceName string,
	command any, options map[string]any, timeout time.Duration) (IncusExecOutput, error) {
	execOptions := map[string]any{"record-output": true}
	for key, value := range options {
		execOptions[key] = value
	}

	metadata, err := IncusInstanceExec(ctx, client, instanceName, command, execOptions, timeout)
	if err != nil {
		return IncusExecOutput{}, err
	}

	read := func(fd string) string {
		path := incusInstanceExtractOutputFilePath(metadata, fd)
		if path == "" {
			return ""
		}
		content, _, readErr := client.ReadFile(ctx, path, 0)
		if readErr != nil {
			return ""
		}
		return content
	}

	return IncusExecOutput{
		Stdout:   read("1"),
		Stderr:   read("2"),
		Metadata: metadata,
	}, nil
}

// IncusInstanceConsoleWebSocketURL mirrors getConsoleWebSocketUrl.
func IncusInstanceConsoleWebSocketURL(client *IncusClient, instanceName string) string {
	wsURL := strings.Replace(client.baseURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	return wsURL + "/1.0/instances/" + instanceName + "/console?wait-for-websocket=true"
}

// IncusInstanceEnsureBridgeIpv6 mirrors ensureBridgeIpv6: a defensive re-check for
// the bridge-level IPv6 NAT modes. Any failure is logged and swallowed because
// instance creation must not fail on it.
func IncusInstanceEnsureBridgeIpv6(ctx context.Context, client *IncusClient, networkMode, bridgeName string) {
	if networkMode != string(NetworkModeIpv6Nat) && networkMode != string(NetworkModeNatIpv6Nat) {
		return
	}
	if bridgeName == "" {
		bridgeName = "incusbr0"
	}

	raw, err := client.Request(ctx, http.MethodGet, "/1.0/networks/"+bridgeName, nil,
		incusInstanceOperationTimeout, nil)
	if err != nil {
		log.Printf("[ensureBridgeIpv6] 网桥 IPv6 校验失败（%s）: %v", bridgeName, err)
		return
	}

	var bridge struct {
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal(raw, &bridge); err != nil {
		log.Printf("[ensureBridgeIpv6] 网桥 IPv6 校验失败（%s）: %v", bridgeName, err)
		return
	}

	needsFix := bridge.Config["ipv6.address"] == "none" || bridge.Config["ipv6.nat"] != "true"
	if !needsFix {
		return
	}

	body := map[string]any{
		"config": map[string]any{
			"ipv6.address": "auto",
			"ipv6.nat":     "true",
			"ipv6.dhcp":    "true",
		},
	}
	if _, err := client.Request(ctx, http.MethodPatch, "/1.0/networks/"+bridgeName, body,
		incusInstanceOperationTimeout, body); err != nil {
		log.Printf("[ensureBridgeIpv6] 网桥 IPv6 校验失败（%s）: %v", bridgeName, err)
		return
	}
	log.Printf("[ensureBridgeIpv6] 已自动为网桥 %s 启用 IPv6 NAT（网络模式: %s）", bridgeName, networkMode)
}

// ==================== operation waiting ====================

// incusInstanceOperationPayload is the /wait projection with presence-aware fields:
// the TypeScript rejects a response whose status is not a string or whose
// status_code is not a number, which a plain int cannot express.
type incusInstanceOperationPayload struct {
	Status      *string         `json:"status"`
	StatusCode  *int            `json:"status_code"`
	Description string          `json:"description"`
	Metadata    json.RawMessage `json:"metadata"`
	Err         string          `json:"err"`
	Error       string          `json:"error"`
}

// IncusInstanceWaitForOperation mirrors waitForOperation(client, operationUrl,
// timeout, signal, requestBody).
//
// The node long-polls `{operation}/wait?timeout=10`, so the loop has no extra
// sleep. Cancellation is bound to the operation task: stopping the task re-reads
// the operation and issues the DELETE only when Incus says it may be cancelled.
func IncusInstanceWaitForOperation(ctx context.Context, client *IncusClient, operationURL string,
	timeout time.Duration, requestBody any) (json.RawMessage, error) {
	if timeout <= 0 {
		timeout = incusInstanceOperationTimeout
	}
	seconds := int(math.Round(timeout.Seconds()))
	TaskLog(ctx, "info", RedactNodeText(fmt.Sprintf("Incus 操作 %s，等待节点执行（超时 %d 秒）", operationURL, seconds), requestBody))

	startTime := time.Now()
	lastProgress := ""

	unbind := BindOperationCancellation(ctx, func() error {
		raw, err := client.Request(ctx, http.MethodGet, operationURL, nil, timeout, requestBody)
		if err != nil {
			return OperationError(err, requestBody)
		}
		var operation incusInstanceOperationPayload
		if err := json.Unmarshal(raw, &operation); err != nil {
			return OperationError(err, requestBody)
		}
		status := ""
		if operation.Status != nil {
			status = *operation.Status
		}
		if status != "Running" && status != "Pending" {
			return errors.New("节点操作已结束或正在取消，请刷新任务状态")
		}
		var mayCancel bool
		var probe map[string]any
		if json.Unmarshal(raw, &probe) == nil {
			if value, ok := probe["may_cancel"].(bool); ok {
				mayCancel = value
			}
		}
		if !mayCancel {
			return errors.New("Incus 不支持取消当前操作，无法强行停止；请等待操作完成")
		}
		if _, err := client.Request(ctx, http.MethodDelete, operationURL, nil, timeout, requestBody); err != nil {
			return OperationError(err, requestBody)
		}
		return nil
	})
	defer unbind()

	for time.Since(startTime) < timeout {
		if err := ctx.Err(); err != nil {
			return nil, &NodeOperationAborted{Kind: "AbortError", Message: err.Error()}
		}

		raw, err := client.Request(ctx, http.MethodGet, operationURL+"/wait?timeout=10", nil, timeout, requestBody)
		if err != nil {
			return nil, OperationError(err, requestBody)
		}

		var operation incusInstanceOperationPayload
		if err := json.Unmarshal(raw, &operation); err != nil || operation.Status == nil || operation.StatusCode == nil {
			return nil, NewIncusAPIError("节点异步操作响应格式无效", 502, requestBody)
		}

		progress := incusInstanceDescribeProgress(raw, operation, requestBody)
		if progress != lastProgress {
			TaskLog(ctx, "info", progress)
			lastProgress = progress
		}

		switch *operation.Status {
		case "Cancelled":
			return nil, NewIncusAPIError("节点操作已取消，执行失败清理", 409, requestBody)
		case "Success":
			return operation.Metadata, nil
		case "Failure":
			message := operation.Err
			if message == "" {
				message = operation.Error
			}
			if message == "" {
				message = "操作失败"
			}
			return nil, NewIncusAPIError(message, *operation.StatusCode, requestBody)
		}

		if *operation.StatusCode >= 400 {
			message := operation.Err
			if message == "" {
				message = operation.Error
			}
			if message == "" {
				message = "节点操作失败"
			}
			return nil, NewIncusAPIError(message, *operation.StatusCode, requestBody)
		}
	}

	return nil, NewIncusAPIError(fmt.Sprintf("Operation timeout (%d seconds)", seconds), 504, requestBody)
}

// IncusInstanceWaitForOperationLong mirrors waitForOperationLong: the same wait with
// the TypeScript's 10-minute default. Unused inside this module (the TypeScript
// export is unused too), kept for parity so callers can name the intent.
func IncusInstanceWaitForOperationLong(ctx context.Context, client *IncusClient, operationURL string,
	requestBody any) (json.RawMessage, error) {
	return IncusInstanceWaitForOperation(ctx, client, operationURL, 10*time.Minute, requestBody)
}

// incusInstanceDescribeProgress mirrors the progress line: description + status +
// the whitelisted metadata counters, redacted and capped at 1000 runes each.
func incusInstanceDescribeProgress(raw json.RawMessage, operation incusInstanceOperationPayload, requestBody any) string {
	parts := make([]string, 0, 4)
	if operation.Description != "" {
		parts = append(parts, RedactNodeText(operation.Description, requestBody))
	}
	if operation.Status != nil && *operation.Status != "" {
		parts = append(parts, RedactNodeText(*operation.Status, requestBody))
	}

	if len(operation.Metadata) > 0 {
		var metadata map[string]any
		if json.Unmarshal(operation.Metadata, &metadata) == nil {
			// Deterministic order: the TypeScript iterates object insertion order,
			// which Go maps do not preserve. Only the log text is affected.
			keys := make([]string, 0, len(metadata))
			for key := range metadata {
				if incusInstanceProgressKeys[key] {
					keys = append(keys, key)
				}
			}
			incusInstanceSortStrings(keys)
			for _, key := range keys {
				text := RedactNodeText(fmt.Sprintf("%v", metadata[key]), requestBody)
				if runes := []rune(text); len(runes) > 1000 {
					text = string(runes[:1000])
				}
				parts = append(parts, key+": "+text)
			}
		}
	}

	return strings.Join(parts, " · ")
}

// incusInstanceSortStrings is a tiny insertion sort so this file does not need the
// sort import for a handful of progress keys.
func incusInstanceSortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for current := index; current > 0 && values[current] < values[current-1]; current-- {
			values[current], values[current-1] = values[current-1], values[current]
		}
	}
}

// ==================== busy retry ====================

// incusInstanceRunWithBusyRetry mirrors runInstanceActionWithBusyRetry.
//
// Incus serialises operations per instance: an "update" left behind by a previous
// call makes stop/rebuild/update fail with `busy running a "..." operation`. The
// retry waits up to 5 minutes, logging every 10s, and can cancel a stale update
// operation when the caller allows it.
func incusInstanceRunWithBusyRetry[T any](ctx context.Context, client *IncusClient, instanceName, actionLabel string,
	action func() (T, error), options IncusInstanceBusyRetryOptions) (T, error) {
	var zero T

	busySince := time.Now()
	var lastLogAt time.Time
	var lastCancelCheckAt time.Time
	lastBusyOperation := ""
	cancelAfter := options.BusyUpdateCancelAfter
	if cancelAfter <= 0 {
		cancelAfter = IncusInstanceBusyCancelStale
	}

	for time.Since(busySince) <= IncusInstanceBusyWaitTimeout {
		result, err := action()
		if err == nil {
			return result, nil
		}

		busyOperation := incusInstanceExtractBusyOperationName(err)
		if busyOperation == "" {
			return zero, err
		}
		lastBusyOperation = busyOperation

		now := time.Now()
		elapsed := now.Sub(busySince)
		remaining := IncusInstanceBusyWaitTimeout - elapsed
		if remaining < 0 {
			remaining = 0
		}

		activeOperationDescription := ""
		if operations, listErr := incusInstanceListActiveOperations(ctx, client, instanceName); listErr == nil && len(operations) > 0 {
			activeOperationDescription = incusInstanceDescribeOperations(operations)
		}

		if now.Sub(lastLogAt) >= incusInstanceBusyLogInterval {
			details := ""
			if activeOperationDescription != "" {
				details = " activeOps=[" + activeOperationDescription + "]"
			}
			log.Printf("[Incus] Instance %s is busy running a %q operation while trying to %s; elapsed=%ds remaining=%ds%s",
				instanceName, busyOperation, actionLabel,
				int(math.Ceil(elapsed.Seconds())), int(math.Ceil(remaining.Seconds())), details)
			lastLogAt = now
		}

		if options.AllowCancelBusyUpdate &&
			strings.ToLower(busyOperation) == "update" &&
			elapsed >= cancelAfter &&
			now.Sub(lastCancelCheckAt) >= IncusInstanceBusyCancelCheck {
			lastCancelCheckAt = now
			cancelled, cancelErr := incusInstanceCancelStaleUpdateOperations(ctx, client, instanceName)
			if cancelErr != nil {
				log.Printf("[Incus] Failed stale update cancellation check for %s: %v", instanceName, cancelErr)
			} else if cancelled > 0 {
				log.Printf("[Incus] Cancelled %d stale update operation(s) for %s", cancelled, instanceName)
			}
		}

		if remaining <= 0 {
			if activeOperationDescription != "" {
				return zero, fmt.Errorf("Instance %q remained busy after waiting to %s: %s",
					instanceName, actionLabel, activeOperationDescription)
			}
			return zero, fmt.Errorf("Instance %q remained busy running a %q operation while trying to %s",
				instanceName, busyOperation, actionLabel)
		}

		sleep := IncusInstanceBusyPollInterval
		if remaining < sleep {
			sleep = remaining
		}
		select {
		case <-ctx.Done():
			return zero, &NodeOperationAborted{Kind: "AbortError", Message: ctx.Err().Error()}
		case <-time.After(sleep):
		}
	}

	if lastBusyOperation != "" {
		return zero, fmt.Errorf("Instance %q remained busy running a %q operation while trying to %s",
			instanceName, lastBusyOperation, actionLabel)
	}
	return zero, fmt.Errorf("Failed to %s", actionLabel)
}

// incusInstanceExtractBusyOperationName mirrors extractBusyOperationName.
func incusInstanceExtractBusyOperationName(err error) string {
	if err == nil {
		return ""
	}
	match := incusInstanceBusyPattern.FindStringSubmatch(err.Error())
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

// incusInstanceListActiveOperations mirrors listActiveInstanceOperations.
func incusInstanceListActiveOperations(ctx context.Context, client *IncusClient, instanceName string) ([]map[string]any, error) {
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/operations?recursion=1", nil,
		incusInstanceOperationTimeout, nil)
	if err != nil {
		return nil, err
	}

	operations, err := incusInstanceNormalizeOperationsPayload(raw)
	if err != nil {
		return nil, err
	}

	active := make([]map[string]any, 0, len(operations))
	for _, operation := range operations {
		if incusInstanceIsOperationRunning(operation) &&
			incusInstanceOperationTouchesInstance(operation["resources"], instanceName) {
			active = append(active, operation)
		}
	}
	return active, nil
}

// incusInstanceNormalizeOperationsPayload mirrors normalizeOperationsPayload: an
// array, or the values of an object keyed by operation id.
func incusInstanceNormalizeOperationsPayload(raw json.RawMessage) ([]map[string]any, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, NewIncusAPIError("节点返回的操作列表格式无效", 502, nil)
	}

	operations := make([]map[string]any, 0)
	switch typed := decoded.(type) {
	case []any:
		for _, entry := range typed {
			if operation, ok := entry.(map[string]any); ok {
				operations = append(operations, operation)
			}
		}
	case map[string]any:
		for _, entry := range typed {
			if operation, ok := entry.(map[string]any); ok {
				operations = append(operations, operation)
			}
		}
	}
	return operations, nil
}

// incusInstanceIsOperationRunning mirrors isOperationRunning.
func incusInstanceIsOperationRunning(operation map[string]any) bool {
	status, _ := operation["status"].(string)
	switch strings.ToLower(status) {
	case "success", "failure", "cancelled", "canceled":
		return false
	}
	if code, ok := operation["status_code"].(float64); ok {
		return code < 200
	}
	return true
}

// incusInstanceOperationTouchesInstance mirrors operationTouchesInstance.
func incusInstanceOperationTouchesInstance(resources any, instanceName string) bool {
	instancePath := "/1.0/instances/" + instanceName
	switch typed := resources.(type) {
	case string:
		return typed == instancePath || strings.HasPrefix(typed, instancePath)
	case []any:
		for _, entry := range typed {
			if incusInstanceOperationTouchesInstance(entry, instanceName) {
				return true
			}
		}
		return false
	case map[string]any:
		for _, entry := range typed {
			if incusInstanceOperationTouchesInstance(entry, instanceName) {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// incusInstanceDescribeOperation mirrors describeOperation.
func incusInstanceDescribeOperation(operation map[string]any) string {
	for _, key := range []string{"description", "class", "id"} {
		if value, ok := operation[key].(string); ok && value != "" {
			return value
		}
	}
	return "operation"
}

// incusInstanceDescribeOperations mirrors describeOperations.
func incusInstanceDescribeOperations(operations []map[string]any) string {
	parts := make([]string, 0, len(operations))
	for _, operation := range operations {
		parts = append(parts, incusInstanceDescribeOperation(operation))
	}
	return strings.Join(parts, ", ")
}

// incusInstanceGetOperationPath mirrors getOperationPath.
func incusInstanceGetOperationPath(operation map[string]any) string {
	id, _ := operation["id"].(string)
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, "/1.0/operations/") {
		return id
	}
	return "/1.0/operations/" + id
}

// incusInstanceOperationAgeMs mirrors operationAgeMs: created_at wins, updated_at is
// the fallback, an unparsable timestamp counts as age 0.
func incusInstanceOperationAgeMs(operation map[string]any) time.Duration {
	parse := func(key string) (time.Time, bool) {
		value, _ := operation[key].(string)
		if value == "" {
			return time.Time{}, false
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, false
		}
		return parsed, true
	}
	if created, ok := parse("created_at"); ok {
		return time.Since(created)
	}
	if updated, ok := parse("updated_at"); ok {
		return time.Since(updated)
	}
	return 0
}

// incusInstanceIsUpdateOperation mirrors isUpdateOperation.
func incusInstanceIsUpdateOperation(operation map[string]any) bool {
	description, _ := operation["description"].(string)
	if strings.Contains(strings.ToLower(description), "update") {
		return true
	}
	metadata, _ := operation["metadata"].(map[string]any)
	action, _ := metadata["action"].(string)
	return strings.ToLower(action) == "update"
}

// incusInstanceCancelStaleUpdateOperations mirrors cancelStaleUpdateOperations.
func incusInstanceCancelStaleUpdateOperations(ctx context.Context, client *IncusClient, instanceName string) (int, error) {
	activeOperations, err := incusInstanceListActiveOperations(ctx, client, instanceName)
	if err != nil {
		return 0, err
	}

	cancelled := 0
	for _, operation := range activeOperations {
		if !incusInstanceIsUpdateOperation(operation) {
			continue
		}
		if mayCancel, ok := operation["may_cancel"].(bool); ok && !mayCancel {
			continue
		}
		if incusInstanceOperationAgeMs(operation) < IncusInstanceBusyCancelStale {
			continue
		}
		operationPath := incusInstanceGetOperationPath(operation)
		if operationPath == "" {
			continue
		}
		if _, err := client.Request(ctx, http.MethodDelete, operationPath, nil,
			incusInstanceOperationTimeout, nil); err != nil {
			log.Printf("[Incus] Failed to cancel stale update operation %v for %s: %v",
				operation["id"], instanceName, err)
			continue
		}
		cancelled++
	}
	return cancelled, nil
}

// incusInstanceExtractStatus mirrors extractInstanceStatus: trimmed, lowercased.
func incusInstanceExtractStatus(state json.RawMessage) string {
	if len(state) == 0 {
		return ""
	}
	var decoded struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(state, &decoded); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(decoded.Status))
}

// ==================== provisioning seam implementation ====================

// IncusInstanceProvision mirrors the Incus half of createInstanceAsync:
// buildInstanceConfig → createInstance → startInstance, plus the 180s VM boot wait.
// The routes layer keeps the DB side (status flip, IP rows, port mapping, mail).
func IncusInstanceProvision(ctx context.Context, client *IncusClient, input IncusInstanceProvisionInput) error {
	if client == nil {
		return errors.New("Incus 客户端未初始化")
	}
	config := input.Config
	hostName := ""
	if input.Host != nil {
		hostName = input.Host.Name
	}

	TaskLog(ctx, "info", fmt.Sprintf("[Provisioning] 正在构建 Incus 配置... 实例=%s 宿主机=%s", config.Name, hostName))

	var ipv6Config *NetworkPayloadIPv6Config
	if incusInstanceNonEmpty(config.IPv6Address) {
		ipv6Config = &NetworkPayloadIPv6Config{PrimaryIP: *config.IPv6Address}
	}

	swapEnabled := config.SwapEnabled
	hostInterface := config.HostInterface
	if hostInterface == "" {
		hostInterface = "eth0"
	}

	incusConfig, err := IncusInstanceBuildConfig(IncusInstanceConfigOptions{
		GPUPCIAddresses:         input.GPUPCIs,
		Name:                    config.Name,
		Image:                   config.Image,
		CPU:                     int(config.CPU),
		Memory:                  int(config.Memory),
		Disk:                    int(config.Disk),
		SwapEnabled:             &swapEnabled,
		SwapSize:                config.SwapSize,
		CloudInitConfig:         &config.CloudInitConfig,
		NetworkMode:             config.NetworkMode,
		Nested:                  config.Nested,
		Privileged:              config.Privileged,
		InstanceType:            config.InstanceType,
		StoragePool:             config.StoragePool,
		IPv4Address:             config.IPv4Address,
		IPv6Address:             config.IPv6Address,
		IPv6Gateway:             config.IPv6Gateway,
		IPv6Config:              ipv6Config,
		HostInterface:           &hostInterface,
		LimitsRead:              config.LimitsRead,
		LimitsWrite:             config.LimitsWrite,
		LimitsReadIops:          config.LimitsReadIops,
		LimitsWriteIops:         config.LimitsWriteIops,
		LimitsIngress:           config.LimitsIngress,
		LimitsEgress:            config.LimitsEgress,
		LimitsProcesses:         config.LimitsProcesses,
		LimitsCPUPriority:       config.LimitsCPUPriority,
		BootAutostart:           config.BootAutostart,
		BootAutostartPriority:   config.BootAutostartPriority,
		BootAutostartDelay:      config.BootAutostartDelay,
		BootHostShutdownTimeout: config.BootHostShutdownTimeout,
	})
	if err != nil {
		return err
	}

	TaskLog(ctx, "info", fmt.Sprintf("[Provisioning] 开始创建实例 %s on %s（CPU=%d, Memory=%dMB, Disk=%dMB）",
		config.Name, hostName, config.CPU, config.Memory, config.Disk))

	// The TypeScript has no instanceId here beyond the row it was created with;
	// preparePackageEgress falls back to (hostId, incusId) when it is undefined, so
	// nil is the faithful equivalent of the async path.
	if _, err := IncusInstanceCreate(ctx, client, incusConfig, nil); err != nil {
		return err
	}

	instanceTypeLabel := "容器"
	if config.InstanceType == "vm" {
		instanceTypeLabel = "虚拟机"
	}
	TaskLog(ctx, "info", fmt.Sprintf("[Provisioning] 实例 %s %s创建完成", config.Name, instanceTypeLabel))

	if _, err := IncusInstanceStart(ctx, client, config.Name); err != nil {
		return err
	}
	TaskLog(ctx, "info", "[Provisioning] 实例 "+config.Name+" 启动命令已发送")

	// A VM needs the whole BIOS → kernel → cloud-init → agent chain; a container
	// starts instantly. 90 rounds x 2s = 180s, then continue anyway.
	if config.InstanceType == "vm" {
		TaskLog(ctx, "info", "[Provisioning] VM "+config.Name+" 等待 QEMU 启动完成...")
		started := false
		for round := 0; round < incusInstanceVMStartWaitRounds; round++ {
			select {
			case <-ctx.Done():
				return &NodeOperationAborted{Kind: "AbortError", Message: ctx.Err().Error()}
			case <-time.After(incusInstanceVMStartWaitSleep):
			}

			state, stateErr := IncusInstanceGetState(ctx, client, config.Name)
			if stateErr != nil {
				// Keep waiting: the guest may still be booting.
				continue
			}
			var decoded struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(state, &decoded) == nil && decoded.Status == "Running" {
				TaskLog(ctx, "info", fmt.Sprintf("[Provisioning] VM %s 已确认启动 (耗时 %ds)",
					config.Name, (round+1)*2))
				started = true
				break
			}
			if round%10 == 0 {
				status := decoded.Status
				if status == "" {
					status = "unknown"
				}
				TaskLog(ctx, "info", fmt.Sprintf("[Provisioning] VM %s 等待启动中... (%ds, 状态: %s)",
					config.Name, (round+1)*2, status))
			}
		}
		if !started {
			TaskLog(ctx, "warn", fmt.Sprintf("[Provisioning] VM %s 启动等待超时 (180s)，继续流程", config.Name))
		}
	}

	return nil
}

// IncusInstanceCleanup mirrors the best-effort deleteInstance cleanup the
// compensation path runs after a failed creation.
func IncusInstanceCleanup(ctx context.Context, client *IncusClient, instanceName string) error {
	_, err := IncusInstanceDelete(ctx, client, instanceName)
	return err
}

// incusInstanceFindGPUInstance mirrors
// `prisma.instance.findFirst({ where: { incusId, hostId, status: { not: 'deleted' },
// gpuDeviceId: { not: null } }, select: { id: true } })`.
func incusInstanceFindGPUInstance(ctx context.Context, hostID int32, name string) (int32, bool, error) {
	if db.DB == nil {
		return 0, false, errors.New("数据库未初始化")
	}
	var id int32
	err := db.DB.WithContext(ctx).Raw(`SELECT id FROM instances
		WHERE incus_id = ? AND host_id = ? AND status <> 'deleted' AND gpu_device_id IS NOT NULL
		LIMIT 1`, name, hostID).Scan(&id).Error
	if err != nil {
		return 0, false, err
	}
	return id, id != 0, nil
}

// incusInstanceReleaseFailedGpuByName mirrors releaseFailedGpuByName: a failed or
// deleted instance releases its GPU reservation so the device can be resold.
func incusInstanceReleaseFailedGpuByName(ctx context.Context, client *IncusClient, name string) error {
	hostID := int32(client.options.HostID)
	if hostID == 0 || db.DB == nil {
		return nil
	}
	return db.DB.WithContext(ctx).Exec(`UPDATE instances SET gpu_released_at = ?
		WHERE incus_id = ? AND host_id = ? AND status IN ('error','deleted')
		  AND gpu_device_id IS NOT NULL AND gpu_released_at IS NULL`,
		time.Now().UTC(), name, hostID).Error
}

// ==================== formatting helpers ====================

// formatAllowanceCores mirrors `String(allowanceToCores(resources.cpu))`.
func formatAllowanceCores(allowance int) string {
	return itoaSmall(AllowanceToCores(allowance))
}

// formatAllowancePercent mirrors `${resources.cpu}%`.
func formatAllowancePercent(allowance int) string {
	return itoaSmall(allowance) + "%"
}

// formatMemoryLimit mirrors `${resources.memory}MB`.
func formatMemoryLimit(memory int) string {
	return itoaSmall(memory) + "MB"
}

// formatDiskSize mirrors `${resources.disk}MB`.
func formatDiskSize(disk int) string {
	return itoaSmall(disk) + "MB"
}

// itoaSmall formats a non-negative int without pulling strconv into this file's
// import list (it is used for short numeric config values only).
func itoaSmall(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}

// ==================== snapshots ====================

// ListSnapshots mirrors listSnapshots(client, instanceName).
func ListSnapshots(ctx context.Context, client *IncusClient, instanceName string) ([]IncusSnapshot, error) {
	raw, err := client.Request(ctx, http.MethodGet,
		"/1.0/instances/"+instanceName+"/snapshots", nil, incusInstanceOperationTimeout, nil)
	if err != nil {
		return nil, err
	}

	var snapshots []IncusSnapshot
	if err := json.Unmarshal(raw, &snapshots); err != nil {
		return nil, NewIncusAPIError("节点返回的快照列表格式无效", 502, nil)
	}
	return snapshots, nil
}

// GetSnapshot mirrors getSnapshot(client, instanceName, snapshotName).
func GetSnapshot(ctx context.Context, client *IncusClient, instanceName, snapshotName string) (*IncusSnapshot, error) {
	raw, err := client.Request(ctx, http.MethodGet,
		"/1.0/instances/"+instanceName+"/snapshots/"+snapshotName, nil, incusInstanceOperationTimeout, nil)
	if err != nil {
		return nil, err
	}

	var snapshot IncusSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, NewIncusAPIError("节点返回的快照数据格式无效", 502, nil)
	}
	return &snapshot, nil
}

// CreateSnapshot mirrors createSnapshot(client, instanceName, snapshotName).
func CreateSnapshot(ctx context.Context, client *IncusClient, instanceName, snapshotName string) (json.RawMessage, error) {
	body := map[string]any{"name": snapshotName}
	return client.Request(ctx, http.MethodPost,
		"/1.0/instances/"+instanceName+"/snapshots", body, incusInstanceOperationTimeout, body)
}

// DeleteSnapshot mirrors deleteSnapshot(client, instanceName, snapshotName).
func DeleteSnapshot(ctx context.Context, client *IncusClient, instanceName, snapshotName string) (json.RawMessage, error) {
	return client.Request(ctx, http.MethodDelete,
		"/1.0/instances/"+instanceName+"/snapshots/"+snapshotName, nil, incusInstanceOperationTimeout, nil)
}

// RestoreSnapshot mirrors restoreSnapshot(client, instanceName, snapshotName).
//
// Incus restores by PUTting `{restore: "<snapshot>"}` to the instance itself, not
// to the snapshot endpoint.
func RestoreSnapshot(ctx context.Context, client *IncusClient, instanceName, snapshotName string) (json.RawMessage, error) {
	body := map[string]any{"restore": snapshotName}
	return client.Request(ctx, http.MethodPut,
		"/1.0/instances/"+instanceName, body, incusInstanceOperationTimeout, body)
}

// ErrIncusInstanceNotFound reports a 404 from the node, so callers can map it to
// their own NOT_FOUND response instead of a generic 500.
var ErrIncusInstanceNotFound = errors.New("incus: instance not found")

// IsIncusNotFound reports whether err is an Incus 404.
func IsIncusNotFound(err error) bool {
	var apiErr *IncusApiError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound
	}
	return false
}
