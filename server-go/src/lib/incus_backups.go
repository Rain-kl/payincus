// Port of server/src/lib/incus/incus-backups.ts (list / get / create / delete).
//
// This module is a thin wrapper over IncusClient.Request, exactly like the
// TypeScript. The streaming export (getBackupExportStream) is intentionally NOT
// ported here: it needs a streaming HTTP request with a dedicated mTLS
// transport, and the shared client (lib/incus_client.go) only exposes a buffered
// Request/ReadAll API and is owned by the substrate owner. routes/backups.go
// exposes it as a fail-closed seam instead of pretending to stream.
//
// The restore helpers of server/src/lib/incus/incus-restore.ts
// (restoreFromStream / renameInstance / instanceExists) and
// incus-instances.ts#startInstance/#deleteInstance are also not ported: they run
// egress-package and GPU side effects that belong to the instances wave. Rollback
// therefore fails closed with an explicit seam error.
package lib

import (
	"context"
	"encoding/json"
	"net/http"
)

// IncusBackup mirrors types/incus.ts IncusBackup.
type IncusBackup struct {
	Name                 string  `json:"name"`
	CreatedAt            string  `json:"created_at"`
	ExpiresAt            *string `json:"expires_at,omitempty"`
	InstanceOnly         bool    `json:"instance_only"`
	OptimizedStorage     bool    `json:"optimized_storage"`
	CompressionAlgorithm string  `json:"compression_algorithm"`
}

// IncusCreateBackupOptions mirrors CreateBackupOptions.
type IncusCreateBackupOptions struct {
	InstanceOnly     *bool
	OptimizedStorage *bool
	Compression      *string
}

// ListIncusBackups mirrors listBackups.
func ListIncusBackups(ctx context.Context, client *IncusClient, instanceName string) ([]IncusBackup, error) {
	data, err := client.Request(ctx, http.MethodGet,
		"/1.0/instances/"+instanceName+"/backups", nil, 0, nil)
	if err != nil {
		return nil, err
	}
	var backups []IncusBackup
	if len(data) > 0 {
		if err := json.Unmarshal(data, &backups); err != nil {
			return nil, err
		}
	}
	if backups == nil {
		backups = []IncusBackup{}
	}
	return backups, nil
}

// GetIncusBackup mirrors getBackup.
func GetIncusBackup(ctx context.Context, client *IncusClient, instanceName, backupName string) (IncusBackup, error) {
	var backup IncusBackup
	data, err := client.Request(ctx, http.MethodGet,
		"/1.0/instances/"+instanceName+"/backups/"+backupName, nil, 0, nil)
	if err != nil {
		return backup, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &backup); err != nil {
			return backup, err
		}
	}
	return backup, nil
}

// CreateIncusBackup mirrors createBackup. Defaults reproduce
// `options.instanceOnly !== false` / `options.optimizedStorage !== false` /
// `options.compression || 'gzip'`.
func CreateIncusBackup(
	ctx context.Context,
	client *IncusClient,
	instanceName string,
	backupName string,
	options IncusCreateBackupOptions,
) (json.RawMessage, error) {
	instanceOnly := true
	if options.InstanceOnly != nil {
		instanceOnly = *options.InstanceOnly
	}
	optimizedStorage := true
	if options.OptimizedStorage != nil {
		optimizedStorage = *options.OptimizedStorage
	}
	compression := "gzip"
	if options.Compression != nil && *options.Compression != "" {
		compression = *options.Compression
	}

	return client.Request(ctx, http.MethodPost,
		"/1.0/instances/"+instanceName+"/backups", map[string]any{
			"name":                  backupName,
			"instance_only":         instanceOnly,
			"optimized_storage":     optimizedStorage,
			"compression_algorithm": compression,
		}, 0, nil)
}

// DeleteIncusBackup mirrors deleteBackup.
func DeleteIncusBackup(ctx context.Context, client *IncusClient, instanceName, backupName string) (json.RawMessage, error) {
	return client.Request(ctx, http.MethodDelete,
		"/1.0/instances/"+instanceName+"/backups/"+backupName, nil, 0, nil)
}
