// Port of server/src/lib/incus/incus-storage.ts.
//
// Storage-pool CRUD for the node API. The instance creation path only needs the
// pool name (it goes into the root device), so this module is a thin client used by
// the host storage-pool routes; it is kept next to incus_instances.go because both
// are the same "raw Incus object" family and share the client.
//
// INCUS RETURNS URLS, NOT OBJECTS: /1.0/storage-pools answers with a list of
// "/1.0/storage-pools/<name>" paths, so the list helper must follow up one GET per
// pool and degrade a single failed detail read to `{name, driver: 'unknown'}`
// instead of failing the whole listing — that fallback is user-visible in the admin
// UI and is reproduced exactly.
package lib

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// IncusStoragePool mirrors IncusStoragePool.
type IncusStoragePool struct {
	Name        string            `json:"name"`
	Driver      string            `json:"driver"`
	Status      string            `json:"status,omitempty"`
	Description string            `json:"description,omitempty"`
	UsedBy      []string          `json:"used_by,omitempty"`
	Config      map[string]string `json:"config,omitempty"`
}

// IncusStoragePoolSpace mirrors `resources.space`.
type IncusStoragePoolSpace struct {
	Used  int64 `json:"used"`
	Total int64 `json:"total"`
}

// IncusStoragePoolResources mirrors `{ space?: { used, total } }`.
type IncusStoragePoolResources struct {
	Space *IncusStoragePoolSpace `json:"space,omitempty"`
}

// IncusStorageCreatePoolOptions mirrors CreateStoragePoolOptions.
type IncusStorageCreatePoolOptions struct {
	Name        string
	Driver      string
	Config      map[string]string
	Description string
}

// IncusStorageUpdatePoolOptions mirrors UpdateStoragePoolOptions. A nil Config or
// Description means the field was not present in the request and must not be sent.
type IncusStorageUpdatePoolOptions struct {
	Config      map[string]string
	Description *string
}

// IncusStorageListPools mirrors listStoragePools.
func IncusStorageListPools(ctx context.Context, client *IncusClient) ([]IncusStoragePool, error) {
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/storage-pools", nil,
		incusInstanceOperationTimeout, nil)
	if err != nil {
		return nil, err
	}

	var poolURLs []string
	if err := json.Unmarshal(raw, &poolURLs); err != nil {
		return nil, NewIncusAPIError("节点返回的存储池列表格式无效", 502, nil)
	}
	if len(poolURLs) == 0 {
		return []IncusStoragePool{}, nil
	}

	pools := make([]IncusStoragePool, 0, len(poolURLs))
	for _, poolURL := range poolURLs {
		parts := strings.Split(poolURL, "/")
		name := parts[len(parts)-1]
		if name == "" {
			continue
		}

		pool, err := IncusStorageGetPool(ctx, client, name)
		if err != nil {
			// One unreadable pool must not take the list down.
			pools = append(pools, IncusStoragePool{Name: name, Driver: "unknown", Config: map[string]string{}})
			continue
		}
		pools = append(pools, *pool)
	}
	return pools, nil
}

// IncusStorageGetPool mirrors getStoragePool.
func IncusStorageGetPool(ctx context.Context, client *IncusClient, name string) (*IncusStoragePool, error) {
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/storage-pools/"+name, nil,
		incusInstanceOperationTimeout, nil)
	if err != nil {
		return nil, err
	}

	var pool IncusStoragePool
	if err := json.Unmarshal(raw, &pool); err != nil {
		return nil, NewIncusAPIError("节点返回的存储池数据格式无效", 502, nil)
	}
	return &pool, nil
}

// IncusStorageGetPoolResources mirrors getStoragePoolResources.
func IncusStorageGetPoolResources(ctx context.Context, client *IncusClient, name string) (*IncusStoragePoolResources, error) {
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/storage-pools/"+name+"/resources", nil,
		incusInstanceOperationTimeout, nil)
	if err != nil {
		return nil, err
	}

	var resources IncusStoragePoolResources
	if err := json.Unmarshal(raw, &resources); err != nil {
		return nil, NewIncusAPIError("节点返回的存储池资源数据格式无效", 502, nil)
	}
	return &resources, nil
}

// IncusStorageCreatePool mirrors createStoragePool.
func IncusStorageCreatePool(ctx context.Context, client *IncusClient, options IncusStorageCreatePoolOptions) error {
	config := options.Config
	if config == nil {
		config = map[string]string{}
	}
	body := map[string]any{
		"name":        options.Name,
		"driver":      options.Driver,
		"config":      config,
		"description": options.Description,
	}
	_, err := client.Request(ctx, http.MethodPost, "/1.0/storage-pools", body,
		incusInstanceOperationTimeout, body)
	return err
}

// IncusStorageDeletePool mirrors deleteStoragePool.
func IncusStorageDeletePool(ctx context.Context, client *IncusClient, name string) error {
	_, err := client.Request(ctx, http.MethodDelete, "/1.0/storage-pools/"+name, nil,
		incusInstanceOperationTimeout, nil)
	return err
}

// IncusStorageUpdatePool mirrors updateStoragePool. PATCH, so omitted fields are
// left untouched; an explicitly empty config map is still sent.
func IncusStorageUpdatePool(ctx context.Context, client *IncusClient, name string, options IncusStorageUpdatePoolOptions) error {
	body := map[string]any{}
	if options.Config != nil {
		body["config"] = options.Config
	}
	if options.Description != nil {
		body["description"] = *options.Description
	}
	_, err := client.Request(ctx, http.MethodPatch, "/1.0/storage-pools/"+name, body,
		incusInstanceOperationTimeout, body)
	return err
}
