// Port of server/src/services/storage-encryption-loader.ts.
package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
)

const storageEncryptionLoaderInterval = 30 * time.Second

func automaticStorageNeeded(ctx context.Context, node StorageEncryptionNode, hostID int32, name string, vault StorageEncryptionKeyVault) (bool, error) {
	secRaw, err := node.Request(ctx, http.MethodGet, "/1.0/agent/security", nil)
	if err != nil {
		return false, err
	}
	secObj, _ := storageEncryptionAnyObject(secRaw)
	if secObj["locked"] != false || agentStorageJSTruthy(secObj["recoveryPending"]) {
		return false, nil
	}
	readNode, err := NodeForPendingPool(ctx, node, hostID, name)
	if err != nil {
		return false, err
	}
	state, err := PoolEncryption(ctx, readNode, name, vault)
	if err != nil {
		return false, err
	}
	if !state.Encrypted || state.ManualLocked || !state.KeySaved {
		return false, nil
	}
	if state.State == "unlocked" && !state.PendingImport && state.PendingRecovery == 0 {
		return false, nil
	}
	return true, nil
}

// AutoLoadStoragePool mirrors autoLoadStoragePool(node, hostId, name, vault).
func AutoLoadStoragePool(ctx context.Context, node StorageEncryptionNode, hostID int32, name string, vault StorageEncryptionKeyVault) (string, error) {
	needed, err := automaticStorageNeeded(ctx, node, hostID, name, vault)
	if err != nil {
		return "", err
	}
	if !needed {
		return "skipped", nil
	}
	if _, err := UnlockAndBindPool(ctx, node, hostID, name, vault, "", true); err != nil {
		return "", err
	}
	return "loaded", nil
}

type storageEncryptionLoaderState struct {
	mu     sync.Mutex
	failed map[string]bool
}

var defaultStorageEncryptionLoader = &storageEncryptionLoaderState{
	failed: map[string]bool{},
}

type zfsLoaderPoolRow struct {
	HostID    int32  `gorm:"column:host_id"`
	Name      string `gorm:"column:name"`
	Config    []byte `gorm:"column:config"`
	Telemetry []byte `gorm:"column:telemetry"`
}

func runStorageEncryptionLoaderRound(ctx context.Context) {
	if ctx.Err() != nil || db.DB == nil {
		return
	}
	var pools []zfsLoaderPoolRow
	if err := db.DB.WithContext(ctx).Raw(`
		SELECT host_id, name, config, telemetry
		FROM storage_pools
		WHERE driver = 'zfs'
		ORDER BY id ASC`).Scan(&pools).Error; err != nil {
		log.Printf("[StorageEncryption] 无法读取存储记录，本轮自动加载已跳过")
		return
	}
	if len(pools) == 0 {
		return
	}

	hostIDs := make([]int32, 0, len(pools))
	seen := map[int32]bool{}
	for _, p := range pools {
		if !seen[p.HostID] {
			seen[p.HostID] = true
			hostIDs = append(hostIDs, p.HostID)
		}
	}
	statuses, err := LoadHostServiceStatuses(ctx, hostIDs)
	if err != nil {
		log.Printf("[StorageEncryption] 无法读取存储记录，本轮自动加载已跳过")
		return
	}

	for _, pool := range pools {
		if ctx.Err() != nil {
			return
		}
		var cfg map[string]any
		if len(pool.Config) > 0 {
			_ = json.Unmarshal(pool.Config, &cfg)
		}
		if _, hasDel := cfg[storageDeletionMarker]; hasDel {
			continue
		}
		var telemetry map[string]any
		if len(pool.Telemetry) > 0 {
			_ = json.Unmarshal(pool.Telemetry, &telemetry)
		}
		reported, _ := telemetry["encryption"].(map[string]any)
		svc, ok := statuses[pool.HostID]
		if !ok || !svc.Available || !agentStorageJSTruthy(reported["encrypted"]) || agentStorageJSTruthy(reported["manualLocked"]) {
			continue
		}
		stateStr, _ := reported["state"].(string)
		pendingRec := 0
		if v, ok := reported["pendingRecovery"].(float64); ok {
			pendingRec = int(v)
		}
		if stateStr == "unlocked" && pendingRec == 0 && !agentStorageJSTruthy(cfg[storagePendingImportMarker]) {
			continue
		}

		identity := fmt.Sprintf("%d/%s", pool.HostID, pool.Name)
		encryptionRoot, _ := reported["encryptionRoot"].(string)
		if encryptionRoot == "" {
			continue
		}
		storedKey, err := FindStoredStorageKey(ctx, pool.HostID, encryptionRoot)
		if err != nil || storedKey == nil {
			continue
		}

		_, err = WithStorageSession[string](ctx, pool.HostID, true, func(node StorageEncryptionNode, vault StorageEncryptionKeyVault) (string, error) {
			return AutoLoadStoragePool(ctx, node, pool.HostID, pool.Name, vault)
		})
		if err != nil {
			defaultStorageEncryptionLoader.mu.Lock()
			firstFailure := !defaultStorageEncryptionLoader.failed[identity]
			defaultStorageEncryptionLoader.failed[identity] = true
			defaultStorageEncryptionLoader.mu.Unlock()
			if firstFailure {
				log.Printf("[StorageEncryption] 节点 #%d 存储 %s 自动加载未完成；请检查节点或托管状态", pool.HostID, pool.Name)
			}
			continue
		}
		defaultStorageEncryptionLoader.mu.Lock()
		delete(defaultStorageEncryptionLoader.failed, identity)
		defaultStorageEncryptionLoader.mu.Unlock()
	}
}
