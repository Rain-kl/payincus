package services

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

const (
	storageEncryptionTestSecret   = "test-encryption-key-abcdefghijklmnopqrstuvwxyz-0123456789"
	storageEncryptionTestKey      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	storageEncryptionTestEnvelope = "v1:00112233445566778899aabb:bf7beb5362d47c13dc316bb8574e9f5e:100ba34af899f424dc89547a9d3fd0309acbba8921a3d5252cc9e34bde25f73b"
)

type storageEncryptionTestNode struct {
	handler func(method, path string, body any) (any, error)
	calls   []string
}

func (n *storageEncryptionTestNode) Request(ctx context.Context, method, path string, body any) (any, error) {
	n.calls = append(n.calls, method+" "+path)
	return n.handler(method, path, body)
}

type storageEncryptionTestVault struct {
	keys  map[string]StorageEncryptionStoredKey
	saved []StorageEncryptionStoredKey
}

func (v *storageEncryptionTestVault) Find(ctx context.Context, dataset string) (*StorageEncryptionStoredKey, error) {
	record, ok := v.keys[dataset]
	if !ok {
		return nil, nil
	}
	return &record, nil
}

func (v *storageEncryptionTestVault) Save(ctx context.Context, record StorageEncryptionStoredKey) error {
	v.keys[record.Dataset] = record
	v.saved = append(v.saved, record)
	return nil
}

type storageEncryptionTestPoolNode struct {
	driver      string
	config      map[string]any
	datasets    []any
	locks       map[string]any
	locked      bool
	extensions  []any
	poolMissing bool
}

func (p *storageEncryptionTestPoolNode) node() *storageEncryptionTestNode {
	return &storageEncryptionTestNode{handler: func(method, path string, body any) (any, error) {
		switch {
		case method == http.MethodGet && path == "/1.0/agent":
			return map[string]any{"api_extensions": p.extensions}, nil
		case method == http.MethodGet && path == "/1.0/storage-pools/data":
			if p.poolMissing {
				return map[string]any{}, nil
			}
			return map[string]any{"driver": p.driver, "config": p.config}, nil
		case method == http.MethodGet && path == "/1.0/agent/storage":
			return map[string]any{"locked": p.locked, "datasets": p.datasets, "locks": p.locks}, nil
		}
		return nil, errors.New("unexpected endpoint " + method + " " + path)
	}}
}

func TestStorageEncryptionVaultMatchesNode(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", storageEncryptionTestSecret)
	identity := StorageEncryptionKeyIdentity{HostID: 4, Dataset: "tank/incus", Version: 1}

	unsealed, err := StorageEncryptionUnseal(storageEncryptionTestEnvelope, identity, storageEncryptionTestSecret)
	if err != nil {
		t.Fatalf("unseal Node envelope: %v", err)
	}
	if unsealed != storageEncryptionTestKey {
		t.Fatalf("unsealed key = %q", unsealed)
	}

	fingerprint, err := StorageEncryptionFingerprint(storageEncryptionTestKey)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if fingerprint != "e0e77a507412b120f6ede61f62295b1a7b2ff19d3dcc8f7253e51663470c888e" {
		t.Fatalf("fingerprint = %q", fingerprint)
	}

	sealed, err := StorageEncryptionSeal(storageEncryptionTestKey, identity, storageEncryptionTestSecret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	parts := strings.Split(sealed, ":")
	if len(parts) != 4 || parts[0] != "v1" {
		t.Fatalf("envelope shape = %q", sealed)
	}
	if !regexp.MustCompile(`^[0-9a-f]{24}$`).MatchString(parts[1]) {
		t.Fatalf("iv = %q", parts[1])
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(parts[2]) {
		t.Fatalf("tag = %q", parts[2])
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(parts[3]) {
		t.Fatalf("ciphertext = %q", parts[3])
	}
	roundTrip, err := StorageEncryptionUnseal(sealed, identity, storageEncryptionTestSecret)
	if err != nil || roundTrip != storageEncryptionTestKey {
		t.Fatalf("round trip = %q, %v", roundTrip, err)
	}

	for _, wrong := range []StorageEncryptionKeyIdentity{
		{HostID: 5, Dataset: "tank/incus", Version: 1},
		{HostID: 4, Dataset: "tank/other", Version: 1},
		{HostID: 4, Dataset: "tank/incus", Version: 2},
	} {
		if _, err := StorageEncryptionUnseal(sealed, wrong, storageEncryptionTestSecret); err == nil {
			t.Fatalf("identity %+v accepted", wrong)
		}
	}
	if _, err := StorageEncryptionUnseal(sealed+"00", identity, storageEncryptionTestSecret); err == nil {
		t.Fatalf("malformed envelope accepted")
	}
	if _, err := StorageEncryptionSeal("not-a-key", identity, storageEncryptionTestSecret); err == nil {
		t.Fatalf("malformed key sealed")
	}
	if _, err := StorageEncryptionSeal(storageEncryptionTestKey, identity, "short"); err == nil {
		t.Fatalf("weak secret accepted")
	}
	if _, err := StorageEncryptionUnseal(storageEncryptionTestEnvelope, identity, ""); err == nil {
		t.Fatalf("empty secret accepted")
	}
}

func TestStorageEncryptionPoolEncryptionPlain(t *testing.T) {
	node := (&storageEncryptionTestPoolNode{
		driver:     "dir",
		config:     map[string]any{},
		extensions: []any{"incus_storage_encryption_v1"},
	}).node()
	vault := &storageEncryptionTestVault{keys: map[string]StorageEncryptionStoredKey{}}

	state, err := PoolEncryption(context.Background(), node, "data", vault)
	if err != nil {
		t.Fatalf("poolEncryption: %v", err)
	}
	if state.Encrypted || state.State != "plain" {
		t.Fatalf("state = %+v", state)
	}
	payload := state.ToMap()
	if len(payload) != 5 || payload["encrypted"] != false || payload["keySaved"] != false {
		t.Fatalf("plain payload = %+v", payload)
	}
}

func TestStorageEncryptionPoolEncryptionRejections(t *testing.T) {
	cases := []struct {
		name    string
		node    *storageEncryptionTestPoolNode
		status  int
		message string
	}{
		{
			name:    "missing-extension",
			node:    &storageEncryptionTestPoolNode{driver: "zfs", extensions: []any{}},
			status:  409,
			message: "节点尚不支持存储加密管理，请先更新节点集成包",
		},
		{
			name: "absolute-source",
			node: &storageEncryptionTestPoolNode{
				driver:     "zfs",
				config:     map[string]any{"source": "/dev/zvol/foo"},
				extensions: []any{"incus_storage_encryption_v1"},
			},
			status:  409,
			message: "无法确定此存储池的 ZFS 数据集，请刷新存储配置",
		},
		{
			name: "dataset-missing",
			node: &storageEncryptionTestPoolNode{
				driver:     "zfs",
				config:     map[string]any{"zfs.pool_name": "tank/incus"},
				datasets:   []any{},
				extensions: []any{"incus_storage_encryption_v1"},
			},
			status:  409,
			message: "ZFS 数据集暂不可见，可能未导入或已离线，请检查节点状态",
		},
		{
			name: "invalid-key-status",
			node: &storageEncryptionTestPoolNode{
				driver:     "zfs",
				config:     map[string]any{"zfs.pool_name": "tank/incus"},
				datasets:   []any{map[string]any{"dataset": "tank/incus", "encryptionRoot": "tank/incus", "algorithm": "aes-256-gcm", "keyStatus": "weird"}},
				extensions: []any{"incus_storage_encryption_v1"},
			},
			status:  502,
			message: "节点返回的加密状态无效",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			vault := &storageEncryptionTestVault{keys: map[string]StorageEncryptionStoredKey{}}
			_, err := PoolEncryption(context.Background(), testCase.node.node(), "data", vault)
			var storageError *StorageEncryptionError
			if !errors.As(err, &storageError) || storageError.StatusCode != testCase.status || storageError.Message != testCase.message {
				t.Fatalf("error = %v, want %d %q", err, testCase.status, testCase.message)
			}
		})
	}
}

func TestStorageEncryptionPoolEncryptionEncrypted(t *testing.T) {
	node := (&storageEncryptionTestPoolNode{
		driver:     "zfs",
		config:     map[string]any{"zfs.pool_name": "tank/incus"},
		locked:     true,
		extensions: []any{"incus_storage_encryption_v1"},
		datasets: []any{map[string]any{
			"dataset": "tank/incus", "encryptionRoot": "tank/incus",
			"algorithm": "aes-256-gcm", "keyStatus": "unavailable", "keyFormat": "hex",
		}},
		locks: map[string]any{"tank/incus": map[string]any{
			"locked": true, "complete": false, "resumeInstances": []any{map[string]any{}, map[string]any{}},
		}},
	}).node()
	vault := &storageEncryptionTestVault{keys: map[string]StorageEncryptionStoredKey{
		"tank/incus": {Dataset: "tank/incus", Fingerprint: "x", Ciphertext: "y", Version: 1},
	}}

	state, err := PoolEncryption(context.Background(), node, "data", vault)
	if err != nil {
		t.Fatalf("poolEncryption: %v", err)
	}
	if !state.Encrypted || state.State != "locked" || !state.KeySaved {
		t.Fatalf("state = %+v", state)
	}
	if !state.NodeLocked || !state.ManualLocked || !state.LockPending || state.PendingRecovery != 2 {
		t.Fatalf("lock state = %+v", state)
	}
	if state.Dataset != "tank/incus" || state.EncryptionRoot != "tank/incus" || state.KeyFormat != "hex" {
		t.Fatalf("dataset state = %+v", state)
	}
	payload := state.ToMap()
	for _, key := range []string{"nodeLocked", "pendingImport", "manualLocked", "lockPending", "pendingRecovery", "encrypted", "state", "dataset", "encryptionRoot", "keySaved", "keyFormat"} {
		if _, present := payload[key]; !present {
			t.Fatalf("payload missing %q: %+v", key, payload)
		}
	}

	node = (&storageEncryptionTestPoolNode{
		driver:     "zfs",
		config:     map[string]any{"source": "tank/incus"},
		extensions: []any{"incus_storage_encryption_v1"},
		datasets: []any{map[string]any{
			"dataset": "tank/incus", "algorithm": "off",
		}},
	}).node()
	plain, err := PoolEncryption(context.Background(), node, "data", vault)
	if err != nil {
		t.Fatalf("plain poolEncryption: %v", err)
	}
	if plain.Encrypted || plain.State != "plain" || plain.Dataset != "tank/incus" {
		t.Fatalf("plain state = %+v", plain)
	}
}

func TestStorageEncryptionUnlockPoolErrors(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", storageEncryptionTestSecret)
	base := func(keyStatus string, keyFormat string, manualLocked bool) *storageEncryptionTestPoolNode {
		return &storageEncryptionTestPoolNode{
			driver:     "zfs",
			config:     map[string]any{"zfs.pool_name": "tank/incus"},
			extensions: []any{"incus_storage_encryption_v1"},
			datasets: []any{map[string]any{
				"dataset": "tank/incus", "encryptionRoot": "tank/incus",
				"algorithm": "aes-256-gcm", "keyStatus": keyStatus, "keyFormat": keyFormat,
			}},
			locks: map[string]any{"tank/incus": map[string]any{"locked": manualLocked, "complete": false}},
		}
	}

	cases := []struct {
		name      string
		node      *storageEncryptionTestPoolNode
		key       string
		automatic bool
		vault     *storageEncryptionTestVault
		status    int
		message   string
	}{
		{
			name:    "plain-pool",
			node:    &storageEncryptionTestPoolNode{driver: "dir", extensions: []any{"incus_storage_encryption_v1"}},
			status:  409,
			message: "此存储池没有 ZFS 加密，不能执行解锁",
		},
		{
			name:      "manual-locked-automatic",
			node:      base("unavailable", "hex", true),
			automatic: true,
			status:    423,
			message:   "存储已手动锁盘，必须手动解密",
		},
		{
			name:    "unsupported-key-format",
			node:    base("unavailable", "base64", false),
			status:  409,
			message: "此加密卷使用的密钥格式暂不支持，请先在节点转换为 hex 密钥格式",
		},
		{
			name:    "no-saved-key",
			node:    base("unavailable", "hex", false),
			status:  409,
			message: "请先输入此加密卷的密钥并保存",
		},
		{
			name:    "malformed-supplied-key",
			node:    base("unavailable", "hex", false),
			key:     "not-a-key",
			status:  400,
			message: "请输入 64 位十六进制 ZFS 密钥",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			vault := testCase.vault
			if vault == nil {
				vault = &storageEncryptionTestVault{keys: map[string]StorageEncryptionStoredKey{}}
			}
			_, err := UnlockPool(context.Background(), testCase.node.node(), "data", 4, vault,
				testCase.key, StorageEncryptionUnlockOptions{Automatic: testCase.automatic})
			var storageError *StorageEncryptionError
			if !errors.As(err, &storageError) || storageError.StatusCode != testCase.status || storageError.Message != testCase.message {
				t.Fatalf("error = %v, want %d %q", err, testCase.status, testCase.message)
			}
		})
	}
}

func TestStorageEncryptionUnlockPoolSuccess(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", storageEncryptionTestSecret)

	keyStatus := "unavailable"
	var posted map[string]any
	node := &storageEncryptionTestNode{handler: func(method, path string, body any) (any, error) {
		switch {
		case method == http.MethodGet && path == "/1.0/agent":
			return map[string]any{"api_extensions": []any{"incus_storage_encryption_v1"}}, nil
		case method == http.MethodGet && path == "/1.0/storage-pools/data":
			return map[string]any{"driver": "zfs", "config": map[string]any{"zfs.pool_name": "tank/incus"}}, nil
		case method == http.MethodGet && path == "/1.0/agent/storage":
			return map[string]any{"locked": false, "datasets": []any{map[string]any{
				"dataset": "tank/incus", "encryptionRoot": "tank/incus",
				"algorithm": "aes-256-gcm", "keyStatus": keyStatus, "keyFormat": "hex",
			}}}, nil
		case method == http.MethodGet && path == "/1.0/agent/security":
			return map[string]any{"locked": false, "revision": float64(1), "targets": []any{}, "storageLocks": map[string]any{}}, nil
		case method == http.MethodPost && path == "/1.0/agent/storage/register":
			posted, _ = body.(map[string]any)
			keyStatus = "available"
			return map[string]any{}, nil
		}
		return nil, errors.New("unexpected endpoint " + method + " " + path)
	}}
	vault := &storageEncryptionTestVault{keys: map[string]StorageEncryptionStoredKey{}}

	state, err := UnlockPool(context.Background(), node, "data", 4, vault,
		storageEncryptionTestKey, StorageEncryptionUnlockOptions{})
	if err != nil {
		t.Fatalf("unlockPool: %v", err)
	}
	if !state.Encrypted || state.State != "unlocked" || !state.KeySaved {
		t.Fatalf("state = %+v", state)
	}
	if posted["dataset"] != "tank/incus" || posted["key"] != storageEncryptionTestKey {
		t.Fatalf("posted body = %+v", posted)
	}
	if len(vault.saved) != 1 || vault.saved[0].Version != 1 {
		t.Fatalf("vault saved = %+v", vault.saved)
	}
	if vault.saved[0].Ciphertext == storageEncryptionTestKey {
		t.Fatalf("ciphertext leaked the plaintext key")
	}
	unsealed, err := StorageEncryptionUnseal(vault.saved[0].Ciphertext,
		StorageEncryptionKeyIdentity{HostID: 4, Dataset: "tank/incus", Version: 1}, storageEncryptionTestSecret)
	if err != nil || unsealed != storageEncryptionTestKey {
		t.Fatalf("stored envelope = %q, %v", unsealed, err)
	}
}

func TestStorageEncryptionUnlockPoolRefusesDifferentKey(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", storageEncryptionTestSecret)
	node := (&storageEncryptionTestPoolNode{
		driver:     "zfs",
		config:     map[string]any{"zfs.pool_name": "tank/incus"},
		extensions: []any{"incus_storage_encryption_v1"},
		datasets: []any{map[string]any{
			"dataset": "tank/incus", "encryptionRoot": "tank/incus",
			"algorithm": "aes-256-gcm", "keyStatus": "unavailable", "keyFormat": "hex",
		}},
	}).node()
	sealed, err := StorageEncryptionSeal(storageEncryptionTestKey,
		StorageEncryptionKeyIdentity{HostID: 4, Dataset: "tank/incus", Version: 1}, storageEncryptionTestSecret)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	fingerprint, err := StorageEncryptionFingerprint(storageEncryptionTestKey)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	vault := &storageEncryptionTestVault{keys: map[string]StorageEncryptionStoredKey{
		"tank/incus": {Dataset: "tank/incus", Ciphertext: sealed, Fingerprint: fingerprint, Version: 1},
	}}

	otherKey := strings.Repeat("b", 64)
	_, err = UnlockPool(context.Background(), node, "data", 4, vault, otherKey, StorageEncryptionUnlockOptions{})
	var storageError *StorageEncryptionError
	if !errors.As(err, &storageError) || storageError.StatusCode != 409 ||
		storageError.Message != "已有不同的托管密钥，不能直接覆盖，请先核对存储卷身份" {
		t.Fatalf("error = %v", err)
	}
	if len(vault.saved) != 0 {
		t.Fatalf("vault overwrote a different key")
	}
}

func TestRestoreStorageInstancesRewritesResumeAndChecksDiskPool(t *testing.T) {
	origFind := storageRecoveryFindInstance
	defer func() { storageRecoveryFindInstance = origFind }()
	storageRecoveryFindInstance = func(ctx context.Context, hostID int32, incusID string) (*db.NodeRecoveryInstance, error) {
		return &db.NodeRecoveryInstance{Status: "running"}, nil
	}

	instanceStatus := "Stopped"
	var resumedBody map[string]any
	node := &storageEncryptionTestNode{handler: func(method, path string, body any) (any, error) {
		switch {
		case method == http.MethodGet && path == "/1.0/agent/security":
			return map[string]any{
				"locked":   false,
				"revision": float64(5),
				"storageLocks": map[string]any{
					"tank/incus": map[string]any{
						"locked": false,
						"resumeInstances": []any{
							map[string]any{"project": "default", "name": "c1", "uuid": "u1", "frozen": false},
						},
					},
				},
			}, nil
		case method == http.MethodGet && path == "/1.0/instances/c1?project=default":
			return map[string]any{
				"type":   "container",
				"config": map[string]any{"volatile.uuid": "u1"},
				"expanded_devices": map[string]any{
					"root": map[string]any{"type": "disk", "pool": "encpool"},
				},
			}, nil
		case method == http.MethodGet && path == "/1.0/storage-pools/encpool":
			return map[string]any{
				"driver": "zfs",
				"config": map[string]any{"zfs.pool_name": "tank/incus"},
			}, nil
		case method == http.MethodGet && path == "/1.0/instances/c1/state?project=default":
			return map[string]any{"status": instanceStatus}, nil
		case method == http.MethodPut && path == "/1.0/instances/c1/state?project=default":
			instanceStatus = "Running"
			return map[string]any{}, nil
		case method == http.MethodPost && path == "/1.0/agent/storage/resume":
			resumedBody, _ = body.(map[string]any)
			return map[string]any{"locked": false, "revision": float64(5), "resumeInstances": []any{}}, nil
		}
		return nil, errors.New("unexpected " + method + " " + path)
	}}

	if err := RestoreStorageInstances(context.Background(), node, "tank/incus", 7); err != nil {
		t.Fatalf("RestoreStorageInstances: %v", err)
	}
	inst, ok := resumedBody["instance"].(lib.ResumeInstance)
	if resumedBody == nil || resumedBody["dataset"] != "tank/incus" || !ok || inst.Name != "c1" {
		t.Fatalf("resumedBody = %+v", resumedBody)
	}
}
