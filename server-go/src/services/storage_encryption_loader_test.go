package services

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestAutoLoadStoragePool(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", storageEncryptionTestSecret)
	sealed, err := StorageEncryptionSeal(storageEncryptionTestKey,
		StorageEncryptionKeyIdentity{HostID: 4, Dataset: "tank/incus", Version: 1}, storageEncryptionTestSecret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	fingerprint, err := StorageEncryptionFingerprint(storageEncryptionTestKey)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	t.Run("skips-when-node-locked", func(t *testing.T) {
		node := &storageEncryptionTestNode{handler: func(method, path string, body any) (any, error) {
			if method == http.MethodGet && path == "/1.0/agent/security" {
				return map[string]any{"locked": true}, nil
			}
			return nil, errors.New("unexpected " + method + " " + path)
		}}
		vault := &storageEncryptionTestVault{keys: map[string]StorageEncryptionStoredKey{}}
		res, err := AutoLoadStoragePool(context.Background(), node, 4, "data", vault)
		if err != nil || res != "skipped" {
			t.Fatalf("got (%q, %v), want (skipped, nil)", res, err)
		}
	})

	t.Run("skips-when-manual-locked", func(t *testing.T) {
		origGetPool := storageGetPoolRecord
		defer func() { storageGetPoolRecord = origGetPool }()
		storageGetPoolRecord = func(ctx context.Context, hostID int32, name string) (*StoragePoolRow, error) {
			return nil, nil
		}
		node := &storageEncryptionTestNode{handler: func(method, path string, body any) (any, error) {
			switch {
			case method == http.MethodGet && path == "/1.0/agent/security":
				return map[string]any{"locked": false}, nil
			case method == http.MethodGet && path == "/1.0/agent":
				return map[string]any{"api_extensions": []any{"incus_storage_encryption_v1"}}, nil
			case method == http.MethodGet && path == "/1.0/storage-pools/data":
				return map[string]any{"driver": "zfs", "config": map[string]any{"zfs.pool_name": "tank/incus"}}, nil
			case method == http.MethodGet && path == "/1.0/agent/storage":
				return map[string]any{
					"locked": false,
					"datasets": []any{map[string]any{
						"dataset": "tank/incus", "encryptionRoot": "tank/incus",
						"algorithm": "aes-256-gcm", "keyStatus": "unavailable", "keyFormat": "hex",
					}},
					"locks": map[string]any{
						"tank/incus": map[string]any{"locked": true, "complete": true},
					},
				}, nil
			}
			return nil, errors.New("unexpected " + method + " " + path)
		}}
		vault := &storageEncryptionTestVault{keys: map[string]StorageEncryptionStoredKey{
			"tank/incus": {Dataset: "tank/incus", Fingerprint: fingerprint, Ciphertext: sealed, Version: 1},
		}}
		res, err := AutoLoadStoragePool(context.Background(), node, 4, "data", vault)
		if err != nil || res != "skipped" {
			t.Fatalf("got (%q, %v), want (skipped, nil)", res, err)
		}
	})
}
