// Port of the subset of server/src/lib/incus/incus-instances.ts and
// incus-snapshots.ts that the checkin, batch-config, resource-pool and snapshots
// modules need.
//
// This is deliberately a slice: the full instance module (create/rebuild/delete,
// network wiring, cloud-init, VM MAC generation) belongs to the high-risk instance
// wave and will extend this file. Keeping the shared transport in
// incus_client.go and the operations here means those four modules can land without
// each inventing its own HTTP calls.
package lib

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
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

// incusInstanceOperationTimeout is the default async-operation budget for the
// instance/snapshot calls below.
const incusInstanceOperationTimeout = 30 * time.Minute

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

// UpdateInstance mirrors updateInstance(client, name, updates).
//
// The TypeScript merges `config` instead of replacing it, because Incus treats PUT
// as a full replacement: sending only the changed keys would silently wipe every
// other setting. `undefined` entries are dropped before the merge.
func UpdateInstance(ctx context.Context, client *IncusClient, name string, updates map[string]any) (json.RawMessage, error) {
	current, err := GetInstance(ctx, client, name)
	if err != nil {
		return nil, err
	}

	body := map[string]any{
		"name":     current.Name,
		"type":     current.Type,
		"profiles": current.Profiles,
		"config":   current.Config,
		"devices":  current.Devices,
	}

	// Merge config rather than replacing it.
	if patch, ok := updates["config"]; ok && patch != nil {
		merged := make(map[string]any, len(current.Config))
		for key, value := range current.Config {
			merged[key] = value
		}
		if patchMap, ok := patch.(map[string]any); ok {
			for key, value := range patchMap {
				merged[key] = value
			}
		} else if patchStrings, ok := patch.(map[string]string); ok {
			for key, value := range patchStrings {
				merged[key] = value
			}
		}
		body["config"] = merged
	}

	for key, value := range updates {
		if key == "config" || value == nil {
			continue
		}
		body[key] = value
	}

	return client.Request(ctx, http.MethodPut, "/1.0/instances/"+name, body, incusInstanceOperationTimeout, updates)
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
