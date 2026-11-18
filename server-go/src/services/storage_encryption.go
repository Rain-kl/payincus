// Port of server/src/lib/storage/{encryption,import,recovery,session}.ts and
// server/src/lib/node-security/vault.ts for shared use by HTTP routes,
// storage pool operations, and the background encryption auto-loader.
package services

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/hkdf"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

const storagePendingImportMarker = "_payincusImport"

// StorageEncryptionError mirrors StorageEncryptionError(statusCode, message).
type StorageEncryptionError struct {
	StatusCode int
	Message    string
}

// Error implements error.
func (e *StorageEncryptionError) Error() string { return e.Message }

// NewStorageEncryptionError constructs a StorageEncryptionError.
func NewStorageEncryptionError(status int, message string) *StorageEncryptionError {
	return &StorageEncryptionError{StatusCode: status, Message: message}
}

// ==================== vault (node-security/vault.ts) ====================

// StorageEncryptionKeyIdentity mirrors KeyIdentity.
type StorageEncryptionKeyIdentity struct {
	HostID  int32
	Dataset string
	Version int32
}

var (
	storageEncryptionDatasetPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]*(\/[A-Za-z0-9_.:-]+)*$`)
	storageEncryptionKeyPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	storageEncryptionIVPattern      = regexp.MustCompile(`^[0-9a-f]{24}$`)
	storageEncryptionTagPattern     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	storageEncryptionInstancePathRe = regexp.MustCompile(`^/1\.0/instances/[^/]+\?`)
)

// IsValidStorageEncryptionKey reports whether s matches 64 lowercase hex chars.
func IsValidStorageEncryptionKey(s string) bool {
	return storageEncryptionKeyPattern.MatchString(s)
}

// IsValidStorageDataset reports whether s is a valid ZFS dataset name without "..".
func IsValidStorageDataset(s string) bool {
	return storageEncryptionDatasetPattern.MatchString(s) && !strings.Contains(s, "..") && len(s) <= 200
}

// StorageEncryptionFingerprint mirrors fingerprint(key): sha256 of raw key bytes.
func StorageEncryptionFingerprint(key string) (string, error) {
	if !storageEncryptionKeyPattern.MatchString(key) {
		return "", errors.New("需要 64 位十六进制随机 ZFS 密钥")
	}
	raw, err := hex.DecodeString(key)
	if err != nil {
		return "", errors.New("需要 64 位十六进制随机 ZFS 密钥")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func storageEncryptionIdentity(identity StorageEncryptionKeyIdentity) ([]byte, error) {
	if identity.HostID < 1 || identity.Version < 1 || !IsValidStorageDataset(identity.Dataset) {
		return nil, errors.New("无效的存储身份")
	}
	encoded, err := json.Marshal([]any{identity.HostID, identity.Dataset, identity.Version})
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func storageEncryptionMaster(secret string) ([]byte, error) {
	if secret == "" || len(secret) < 32 {
		return nil, errors.New("控制端 ENCRYPTION_KEY 未配置或强度不足")
	}
	reader := hkdf.New(sha256.New, []byte(secret), []byte("payincus-node-storage"), []byte("vault-v1"))
	material := make([]byte, 32)
	if _, err := io.ReadFull(reader, material); err != nil {
		return nil, err
	}
	return material, nil
}

// StorageEncryptionSeal mirrors seal(key, value, secret).
func StorageEncryptionSeal(key string, identity StorageEncryptionKeyIdentity, secret string) (string, error) {
	if _, err := StorageEncryptionFingerprint(key); err != nil {
		return "", err
	}
	material, err := storageEncryptionMaster(secret)
	if err != nil {
		return "", err
	}
	defer func() {
		for index := range material {
			material[index] = 0
		}
	}()

	block, err := aes.NewCipher(material)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	aad, err := storageEncryptionIdentity(identity)
	if err != nil {
		return "", err
	}
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	plaintext, err := hex.DecodeString(key)
	if err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, iv, plaintext, aad)
	tag := sealed[len(sealed)-16:]
	ciphertext := sealed[:len(sealed)-16]
	return strings.Join([]string{"v1", hex.EncodeToString(iv), hex.EncodeToString(tag), hex.EncodeToString(ciphertext)}, ":"), nil
}

// StorageEncryptionUnseal mirrors unseal(envelope, value, secret).
func StorageEncryptionUnseal(envelope string, identity StorageEncryptionKeyIdentity, secret string) (string, error) {
	parts := strings.Split(envelope, ":")
	if len(parts) != 4 || parts[0] != "v1" ||
		!storageEncryptionIVPattern.MatchString(parts[1]) ||
		!storageEncryptionTagPattern.MatchString(parts[2]) ||
		!storageEncryptionKeyPattern.MatchString(parts[3]) {
		return "", errors.New("存储密钥格式不兼容")
	}
	material, err := storageEncryptionMaster(secret)
	if err != nil {
		return "", errors.New("存储密钥校验失败")
	}
	defer func() {
		for index := range material {
			material[index] = 0
		}
	}()
	block, err := aes.NewCipher(material)
	if err != nil {
		return "", errors.New("存储密钥校验失败")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", errors.New("存储密钥校验失败")
	}
	aad, err := storageEncryptionIdentity(identity)
	if err != nil {
		return "", errors.New("存储密钥校验失败")
	}
	iv, err := hex.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("存储密钥校验失败")
	}
	tag, err := hex.DecodeString(parts[2])
	if err != nil {
		return "", errors.New("存储密钥校验失败")
	}
	ciphertext, err := hex.DecodeString(parts[3])
	if err != nil {
		return "", errors.New("存储密钥校验失败")
	}
	sealed := append(append([]byte{}, ciphertext...), tag...)
	plaintext, err := aead.Open(nil, iv, sealed, aad)
	if err != nil {
		return "", errors.New("存储密钥校验失败")
	}
	return hex.EncodeToString(plaintext), nil
}

// StorageEncryptionSecret reads ENCRYPTION_KEY directly without falling back to JWT_SECRET.
func StorageEncryptionSecret() string {
	return os.Getenv("ENCRYPTION_KEY")
}

// ==================== node abstraction ====================

// StorageEncryptionNode mirrors the TypeScript StorageNode structural contract.
type StorageEncryptionNode interface {
	Request(ctx context.Context, method, path string, body any) (any, error)
}

type storageEncryptionIncusNode struct {
	client *lib.IncusClient
}

// NewStorageEncryptionIncusNode wraps an existing IncusClient as StorageEncryptionNode.
func NewStorageEncryptionIncusNode(client *lib.IncusClient) StorageEncryptionNode {
	return storageEncryptionIncusNode{client: client}
}

func (n storageEncryptionIncusNode) Request(ctx context.Context, method, path string, body any) (any, error) {
	raw, err := n.client.Request(ctx, method, path, body, 0, body)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func storageEncryptionAnyObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

func storageEncryptionStringField(object map[string]any, key string) string {
	text, _ := object[key].(string)
	return text
}

func storageEncryptionBoolField(object map[string]any, key string) bool {
	return object[key] == true
}

func storageEncryptionAnyContains(values []any, candidate string) bool {
	for _, value := range values {
		if text, ok := value.(string); ok && text == candidate {
			return true
		}
	}
	return false
}

// ==================== pool encryption ====================

// StorageEncryptionPoolState is the typed form of poolEncryption()'s return value.
type StorageEncryptionPoolState struct {
	NodeLocked      bool
	PendingImport   bool
	ManualLocked    bool
	LockPending     bool
	PendingRecovery int
	Encrypted       bool
	State           string
	Dataset         string
	EncryptionRoot  string
	KeySaved        bool
	KeyFormat       string
}

// ToMap mirrors the wire JSON shape of poolEncryption().
func (s StorageEncryptionPoolState) ToMap() map[string]any {
	if !s.Encrypted {
		return map[string]any{
			"encrypted":      false,
			"state":          "plain",
			"dataset":        s.Dataset,
			"encryptionRoot": s.EncryptionRoot,
			"keySaved":       false,
		}
	}
	return map[string]any{
		"nodeLocked":      s.NodeLocked,
		"pendingImport":   s.PendingImport,
		"manualLocked":    s.ManualLocked,
		"lockPending":     s.LockPending,
		"pendingRecovery": s.PendingRecovery,
		"encrypted":       true,
		"state":           s.State,
		"dataset":         s.Dataset,
		"encryptionRoot":  s.EncryptionRoot,
		"keySaved":        s.KeySaved,
		"keyFormat":       s.KeyFormat,
	}
}

// StorageEncryptionStoredKey mirrors StoredKey.
type StorageEncryptionStoredKey struct {
	Dataset     string
	Ciphertext  string
	Fingerprint string
	Version     int32
}

// StorageEncryptionKeyVault mirrors StorageKeyVault.
type StorageEncryptionKeyVault interface {
	Find(ctx context.Context, dataset string) (*StorageEncryptionStoredKey, error)
	Save(ctx context.Context, record StorageEncryptionStoredKey) error
}

// PoolEncryption mirrors poolEncryption(node, poolName, vault).
func PoolEncryption(
	ctx context.Context, node StorageEncryptionNode, poolName string, vault StorageEncryptionKeyVault,
) (StorageEncryptionPoolState, error) {
	infoRaw, err := node.Request(ctx, http.MethodGet, "/1.0/agent", nil)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}
	info, ok := storageEncryptionAnyObject(infoRaw)
	if !ok {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "节点尚不支持存储加密管理，请先更新节点集成包")
	}
	extensions, ok := info["api_extensions"].([]any)
	if !ok || !storageEncryptionAnyContains(extensions, "incus_storage_encryption_v1") {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "节点尚不支持存储加密管理，请先更新节点集成包")
	}

	poolRaw, err := node.Request(ctx, http.MethodGet, "/1.0/storage-pools/"+url.PathEscape(poolName), nil)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}
	pool, ok := storageEncryptionAnyObject(poolRaw)
	if !ok {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(502, "节点返回的存储池信息无效")
	}
	if storageEncryptionStringField(pool, "driver") != "zfs" {
		return StorageEncryptionPoolState{State: "plain"}, nil
	}
	config, _ := storageEncryptionAnyObject(pool["config"])
	source := storageEncryptionStringField(config, "zfs.pool_name")
	if source == "" {
		source = storageEncryptionStringField(config, "source")
	}
	if source == "" || strings.HasPrefix(source, "/") {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "无法确定此存储池的 ZFS 数据集，请刷新存储配置")
	}

	inventoryRaw, err := node.Request(ctx, http.MethodGet, "/1.0/agent/storage", nil)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}
	inventory, ok := storageEncryptionAnyObject(inventoryRaw)
	if !ok {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(502, "节点返回的加密状态无效")
	}
	datasets, ok := inventory["datasets"].([]any)
	if !ok {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(502, "节点返回的加密状态无效")
	}
	var item map[string]any
	for _, raw := range datasets {
		dataset, ok := storageEncryptionAnyObject(raw)
		if !ok {
			continue
		}
		if storageEncryptionStringField(dataset, "dataset") == source {
			item = dataset
			break
		}
	}
	if item == nil {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "ZFS 数据集暂不可见，可能未导入或已离线，请检查节点状态")
	}
	algorithm := storageEncryptionStringField(item, "algorithm")
	if algorithm == "off" || algorithm == "-" {
		return StorageEncryptionPoolState{Dataset: source, State: "plain"}, nil
	}
	encryptionRoot := storageEncryptionStringField(item, "encryptionRoot")
	keyStatus := storageEncryptionStringField(item, "keyStatus")
	if encryptionRoot == "" || (keyStatus != "available" && keyStatus != "unavailable") || !strings.HasPrefix(algorithm, "aes-") {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(502, "节点返回的加密状态无效")
	}

	stored, err := vault.Find(ctx, encryptionRoot)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}

	locks, _ := storageEncryptionAnyObject(inventory["locks"])
	lock, _ := storageEncryptionAnyObject(locks[encryptionRoot])
	pendingRecovery := 0
	if resume, ok := lock["resumeInstances"].([]any); ok {
		pendingRecovery = len(resume)
	}

	state := "locked"
	if keyStatus == "available" {
		state = "unlocked"
	}
	return StorageEncryptionPoolState{
		NodeLocked:      storageEncryptionBoolField(inventory, "locked"),
		PendingImport:   storageEncryptionBoolField(pool, "pendingImport"),
		ManualLocked:    storageEncryptionBoolField(lock, "locked"),
		LockPending:     storageEncryptionBoolField(lock, "locked") && lock["complete"] != true,
		PendingRecovery: pendingRecovery,
		Encrypted:       true,
		State:           state,
		Dataset:         source,
		EncryptionRoot:  encryptionRoot,
		KeySaved:        stored != nil,
		KeyFormat:       storageEncryptionStringField(item, "keyFormat"),
	}, nil
}

// StorageEncryptionUnlockOptions mirrors `{ automatic?: boolean }`.
type StorageEncryptionUnlockOptions struct {
	Automatic bool
}

// UnlockPool mirrors unlockPool(node, poolName, hostId, vault, key, options).
func UnlockPool(
	ctx context.Context,
	node StorageEncryptionNode,
	poolName string,
	hostID int32,
	vault StorageEncryptionKeyVault,
	suppliedKey string,
	options StorageEncryptionUnlockOptions,
) (StorageEncryptionPoolState, error) {
	state, err := PoolEncryption(ctx, node, poolName, vault)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}
	if !state.Encrypted {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "此存储池没有 ZFS 加密，不能执行解锁")
	}
	if options.Automatic && state.ManualLocked {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(423, "存储已手动锁盘，必须手动解密")
	}
	if state.KeyFormat != "hex" {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "此加密卷使用的密钥格式暂不支持，请先在节点转换为 hex 密钥格式")
	}
	existing, err := vault.Find(ctx, state.EncryptionRoot)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}

	key := suppliedKey
	if key == "" {
		if existing == nil {
			return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "请先输入此加密卷的密钥并保存")
		}
		unsealed, err := StorageEncryptionUnseal(existing.Ciphertext, StorageEncryptionKeyIdentity{
			HostID: hostID, Dataset: existing.Dataset, Version: existing.Version,
		}, StorageEncryptionSecret())
		if err != nil {
			return StorageEncryptionPoolState{}, NewStorageEncryptionError(503, "已保存密钥无法读取，请检查控制端主密钥配置")
		}
		key = unsealed
	}
	if !storageEncryptionKeyPattern.MatchString(key) {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(400, "请输入 64 位十六进制 ZFS 密钥")
	}
	fingerprint, err := StorageEncryptionFingerprint(key)
	if err != nil {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(400, "请输入 64 位十六进制 ZFS 密钥")
	}
	if existing != nil && existing.Fingerprint != fingerprint {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "已有不同的托管密钥，不能直接覆盖，请先核对存储卷身份")
	}
	version := int32(1)
	if existing != nil && existing.Version != 0 {
		version = existing.Version
	}
	ciphertext, err := StorageEncryptionSeal(key, StorageEncryptionKeyIdentity{
		HostID: hostID, Dataset: state.EncryptionRoot, Version: version,
	}, StorageEncryptionSecret())
	if err != nil {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(503, "控制端密钥托管未就绪，请检查主密钥配置")
	}

	nodeStateRaw, err := node.Request(ctx, http.MethodGet, "/1.0/agent/security", nil)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}
	nodeState, ok := storageEncryptionAnyObject(nodeStateRaw)
	if !ok {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(502, "无法核验节点存储注册状态")
	}
	lockedValue, lockedIsBool := nodeState["locked"].(bool)
	if !lockedIsBool {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(502, "无法核验节点存储注册状态")
	}
	targetsRaw, targetsPresent := nodeState["targets"]
	var targets []any
	if targetsRaw != nil {
		array, ok := targetsRaw.([]any)
		if !ok {
			return StorageEncryptionPoolState{}, NewStorageEncryptionError(502, "无法核验节点存储注册状态")
		}
		targets = array
	} else if !targetsPresent {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(502, "无法核验节点存储注册状态")
	}

	var registered map[string]any
	for _, raw := range targets {
		target, ok := storageEncryptionAnyObject(raw)
		if !ok {
			continue
		}
		if storageEncryptionStringField(target, "dataset") == state.EncryptionRoot {
			registered = target
			break
		}
	}
	if registered != nil && storageEncryptionStringField(registered, "fingerprint") != fingerprint {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "密钥与节点登记的加密卷不匹配")
	}
	if registered == nil && lockedValue {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(423, "节点安全锁定中，不能登记新的加密卷")
	}

	action := "register"
	if registered != nil {
		switch {
		case state.ManualLocked:
			action = "unlock-pool"
		case options.Automatic:
			action = "auto-unlock"
		default:
			action = "unlock"
		}
	}
	if _, err := node.Request(ctx, http.MethodPost, "/1.0/agent/storage/"+action,
		map[string]any{"dataset": state.EncryptionRoot, "key": key}); err != nil {
		return StorageEncryptionPoolState{}, err
	}
	if err := vault.Save(ctx, StorageEncryptionStoredKey{
		Dataset: state.EncryptionRoot, Fingerprint: fingerprint, Ciphertext: ciphertext, Version: version,
	}); err != nil {
		var storageError *StorageEncryptionError
		if errors.As(err, &storageError) {
			return StorageEncryptionPoolState{}, err
		}
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(503, "节点已加载密钥，但控制端保存失败，请保留密钥后重试")
	}
	key = ""

	if !options.Automatic || state.PendingRecovery > 0 {
		if err := RestoreStorageInstances(ctx, node, state.EncryptionRoot, hostID); err != nil {
			return StorageEncryptionPoolState{}, err
		}
	}

	verified, err := PoolEncryption(ctx, node, poolName, vault)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}
	if !verified.Encrypted || verified.State != "unlocked" || !verified.KeySaved {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "节点尚未确认解锁完成，请刷新状态后重试")
	}
	return verified, nil
}

// ==================== storage recovery (lib/storage/recovery.ts) ====================

type storageRecoveryAdapter struct {
	node    StorageEncryptionNode
	dataset string
}

func (a storageRecoveryAdapter) Request(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	if path == "/1.0/agent/security/resume" {
		merged := map[string]any{"dataset": a.dataset}
		if bodyMap, ok := body.(map[string]any); ok {
			for k, v := range bodyMap {
				merged[k] = v
			}
		}
		res, err := a.node.Request(ctx, http.MethodPost, "/1.0/agent/storage/resume", merged)
		if err != nil {
			return nil, err
		}
		return json.Marshal(res)
	}
	value, err := a.node.Request(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	if method == http.MethodGet && storageEncryptionInstancePathRe.MatchString(path) {
		instanceObj, _ := value.(map[string]any)
		devices, _ := instanceObj["expanded_devices"].(map[string]any)
		related := false
		for _, rawDevice := range devices {
			dev, ok := rawDevice.(map[string]any)
			if !ok {
				continue
			}
			devType, _ := dev["type"].(string)
			poolName, _ := dev["pool"].(string)
			if devType != "disk" || poolName == "" {
				continue
			}
			poolRaw, err := a.node.Request(ctx, http.MethodGet, "/1.0/storage-pools/"+url.PathEscape(poolName), nil)
			if err != nil {
				return nil, err
			}
			poolObj, _ := poolRaw.(map[string]any)
			cfg, _ := poolObj["config"].(map[string]any)
			source, _ := cfg["zfs.pool_name"].(string)
			if source == "" {
				source, _ = cfg["source"].(string)
			}
			if source == a.dataset || strings.HasPrefix(source, a.dataset+"/") {
				related = true
				break
			}
		}
		if !related {
			return nil, errors.New("workload no longer uses the locked storage")
		}
	}
	return json.Marshal(value)
}

var (
	storageRecoveryNow          = func() time.Time { return time.Now().UTC() }
	storageRecoveryFindInstance = db.FindInstanceForNodeRecovery
	storageGetPoolRecord        = GetStoragePoolRecord
)

// RestoreStorageInstances mirrors restoreStorageInstances(node, dataset, hostId).
func RestoreStorageInstances(ctx context.Context, node StorageEncryptionNode, dataset string, hostID int32) error {
	raw, err := node.Request(ctx, http.MethodGet, "/1.0/agent/security", nil)
	if err != nil {
		return err
	}
	state, ok := storageEncryptionAnyObject(raw)
	if !ok {
		return nil
	}
	storageLocks, _ := storageEncryptionAnyObject(state["storageLocks"])
	lock, _ := storageEncryptionAnyObject(storageLocks[dataset])
	resumeRaw, _ := lock["resumeInstances"].([]any)
	if len(resumeRaw) == 0 {
		return nil
	}
	if storageEncryptionBoolField(state, "locked") || storageEncryptionBoolField(lock, "locked") {
		return NewStorageEncryptionError(423, "存储密钥已加载，但节点或存储仍被锁定，实例尚未恢复")
	}

	resumeBytes, err := json.Marshal(resumeRaw)
	if err != nil {
		return err
	}
	var resumeItems []lib.ResumeInstance
	if err := json.Unmarshal(resumeBytes, &resumeItems); err != nil {
		return NewStorageEncryptionError(502, "节点返回的恢复实例列表格式无效")
	}
	revFloat, _ := state["revision"].(float64)
	revision := int64(revFloat)
	lockedFalse := false
	initial := lib.RecoveryState{
		Locked:          &lockedFalse,
		Revision:        &revision,
		ResumeInstances: resumeItems,
	}

	adapter := storageRecoveryAdapter{node: node, dataset: dataset}
	allow := func(item lib.ResumeInstance) (bool, error) {
		if item.Project != "default" {
			return true, nil
		}
		inst, err := storageRecoveryFindInstance(ctx, hostID, item.Name)
		if err != nil {
			return false, err
		}
		if inst == nil {
			return true, nil
		}
		switch inst.Status {
		case "running", "stopped", "error":
		default:
			return false, nil
		}
		if inst.ExpiresAt != nil && !inst.ExpiresAt.After(storageRecoveryNow()) {
			return false, nil
		}
		return true, nil
	}

	_, failed, err := lib.RestoreNodeInstances(ctx, adapter, initial, allow, lib.RecoveryOptions{
		Types: []string{"container", "virtual-machine"},
	})
	if err != nil {
		return err
	}
	if failed > 0 {
		return NewStorageEncryptionError(409, fmt.Sprintf("磁盘已解锁，但 %d 个相关实例尚未恢复；再次解密可继续恢复", failed))
	}
	return nil
}

// LockPool mirrors lockPool(node, poolName, vault, actorId, hostId).
func LockPool(
	ctx context.Context,
	node StorageEncryptionNode,
	poolName string,
	vault StorageEncryptionKeyVault,
	actorID int32,
	hostID int32,
) (StorageEncryptionPoolState, error) {
	state, err := PoolEncryption(ctx, node, poolName, vault)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}
	if !state.Encrypted {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "此磁盘未配置 ZFS 加密，不能卸载密钥锁盘")
	}
	if !state.KeySaved {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "请先保存并校验解锁密钥，再执行锁盘")
	}
	stored, err := vault.Find(ctx, state.EncryptionRoot)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}
	custodyValid := false
	if stored != nil {
		unsealed, unsealErr := StorageEncryptionUnseal(stored.Ciphertext, StorageEncryptionKeyIdentity{
			HostID: hostID, Dataset: stored.Dataset, Version: stored.Version,
		}, StorageEncryptionSecret())
		if unsealErr == nil {
			fingerprint, fingerprintErr := StorageEncryptionFingerprint(unsealed)
			custodyValid = fingerprintErr == nil && fingerprint == stored.Fingerprint
		}
	}
	if !custodyValid {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(503, "托管密钥暂不可用，未停止实例或锁盘，请先检查控制端主密钥配置")
	}

	registrationRaw, err := node.Request(ctx, http.MethodGet, "/1.0/agent/security", nil)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}
	registration, ok := storageEncryptionAnyObject(registrationRaw)
	if !ok {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "托管密钥与节点加密卷登记不一致，未停止实例或锁盘")
	}
	targets, _ := registration["targets"].([]any)
	matched := false
	for _, raw := range targets {
		target, ok := storageEncryptionAnyObject(raw)
		if !ok {
			continue
		}
		if storageEncryptionStringField(target, "dataset") == state.EncryptionRoot &&
			storageEncryptionStringField(target, "fingerprint") == stored.Fingerprint {
			matched = true
			break
		}
	}
	if !matched {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "托管密钥与节点加密卷登记不一致，未停止实例或锁盘")
	}

	if err := InsertStorageEncryptionAuditLog(ctx, actorID, "storage.lock.request",
		fmt.Sprintf("节点 #%d 管理员锁定存储池 %s", hostID, poolName)); err != nil {
		return StorageEncryptionPoolState{}, err
	}
	if _, err := node.Request(ctx, http.MethodPost, "/1.0/agent/storage/lock-pool",
		map[string]any{"dataset": state.EncryptionRoot}); err != nil {
		return StorageEncryptionPoolState{}, err
	}

	verified, err := PoolEncryption(ctx, node, poolName, vault)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}
	if !verified.Encrypted || verified.State != "locked" || !verified.ManualLocked || verified.LockPending {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409, "锁盘未完成，请检查相关实例和存储占用后重试")
	}
	return verified, nil
}

// ==================== pending imports (lib/storage/import.ts) ====================

// StoragePendingImport mirrors PendingImport.
type StoragePendingImport struct {
	Source         string
	EncryptionRoot string
}

func storagePendingRecord(ctx context.Context, hostID int32, poolName string) (*StoragePoolRow, *StoragePendingImport, error) {
	pool, err := storageGetPoolRecord(ctx, hostID, poolName)
	if err != nil {
		return nil, nil, err
	}
	if pool == nil {
		return nil, nil, nil
	}
	config, _ := storageDecodeObject(pool.Config)
	rawPending, ok := config[storagePendingImportMarker].(map[string]any)
	if !ok {
		return pool, nil, nil
	}
	pending := &StoragePendingImport{
		Source:         storageEncryptionStringField(rawPending, "source"),
		EncryptionRoot: storageEncryptionStringField(rawPending, "encryptionRoot"),
	}
	if !IsValidStorageDataset(pending.Source) || !IsValidStorageDataset(pending.EncryptionRoot) {
		return nil, nil, NewStorageEncryptionError(409, "待导入存储记录无效，请检查节点数据集身份")
	}
	return pool, pending, nil
}

type storagePendingNode struct {
	inner    StorageEncryptionNode
	poolName string
	pending  *StoragePendingImport
}

func (n storagePendingNode) Request(ctx context.Context, method, path string, body any) (any, error) {
	poolPath := "/1.0/storage-pools/" + url.PathEscape(n.poolName)
	if method == http.MethodGet && path == poolPath {
		pathsRaw, err := n.inner.Request(ctx, http.MethodGet, "/1.0/storage-pools", nil)
		if err != nil {
			return nil, err
		}
		paths, ok := pathsRaw.([]any)
		if !ok {
			return nil, NewStorageEncryptionError(502, "无法读取节点存储列表")
		}
		if !storageEncryptionAnyContains(paths, poolPath) {
			return map[string]any{
				"driver":        "zfs",
				"config":        map[string]any{"source": n.pending.Source},
				"pendingImport": true,
			}, nil
		}
		actualRaw, err := n.inner.Request(ctx, http.MethodGet, poolPath, nil)
		if err != nil {
			return nil, err
		}
		actual, _ := storageEncryptionAnyObject(actualRaw)
		config, _ := storageEncryptionAnyObject(actual["config"])
		source := storageEncryptionStringField(config, "zfs.pool_name")
		if source == "" {
			source = storageEncryptionStringField(config, "source")
		}
		if storageEncryptionStringField(actual, "driver") != "zfs" || source != n.pending.Source {
			return nil, NewStorageEncryptionError(409, "同名存储池来源已变化，拒绝绑定待导入卷")
		}
		response := map[string]any{}
		for key, value := range actual {
			response[key] = value
		}
		response["pendingImport"] = true
		return response, nil
	}
	return n.inner.Request(ctx, method, path, body)
}

// NodeForPendingPool mirrors nodeForPendingPool(node, hostId, poolName).
func NodeForPendingPool(ctx context.Context, node StorageEncryptionNode, hostID int32, poolName string) (StorageEncryptionNode, error) {
	_, pending, err := storagePendingRecord(ctx, hostID, poolName)
	if err != nil {
		return nil, err
	}
	if pending == nil {
		return node, nil
	}
	return storagePendingNode{inner: node, poolName: poolName, pending: pending}, nil
}

// UnlockAndBindPool mirrors unlockAndBindPool(node, hostId, poolName, vault, key, automatic).
func UnlockAndBindPool(
	ctx context.Context,
	node StorageEncryptionNode,
	hostID int32,
	poolName string,
	vault StorageEncryptionKeyVault,
	key string,
	automatic bool,
) (StorageEncryptionPoolState, error) {
	decorated, err := NodeForPendingPool(ctx, node, hostID, poolName)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}

	result, err := UnlockPool(ctx, decorated, poolName, hostID, vault, key, StorageEncryptionUnlockOptions{Automatic: automatic})
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}

	record, pendingAfter, err := storagePendingRecord(ctx, hostID, poolName)
	if err != nil {
		return StorageEncryptionPoolState{}, err
	}
	if pendingAfter == nil || record == nil || (result.Encrypted && result.NodeLocked) {
		return result, nil
	}

	poolPath := "/1.0/storage-pools/" + url.PathEscape(poolName)
	if err := func() error {
		pathsRaw, err := node.Request(ctx, http.MethodGet, "/1.0/storage-pools", nil)
		if err != nil {
			return err
		}
		paths, ok := pathsRaw.([]any)
		if !ok {
			return errors.New("node storage list is invalid")
		}
		if !storageEncryptionAnyContains(paths, poolPath) {
			description := ""
			if record.Description != nil {
				description = *record.Description
			}
			if _, err := node.Request(ctx, http.MethodPost, "/1.0/storage-pools", map[string]any{
				"name":        poolName,
				"driver":      "zfs",
				"config":      map[string]any{"source": pendingAfter.Source},
				"description": description,
			}); err != nil {
				return err
			}
		}
		actualRaw, err := node.Request(ctx, http.MethodGet, poolPath, nil)
		if err != nil {
			return err
		}
		actual, _ := storageEncryptionAnyObject(actualRaw)
		config, _ := storageEncryptionAnyObject(actual["config"])
		source := storageEncryptionStringField(config, "zfs.pool_name")
		if source == "" {
			source = storageEncryptionStringField(config, "source")
		}
		if storageEncryptionStringField(actual, "driver") != "zfs" || source != pendingAfter.Source {
			return errors.New("source changed")
		}
		encoded, err := json.Marshal(actual["config"])
		if err != nil {
			return err
		}
		return UpdateStoragePoolConfig(ctx, hostID, poolName, encoded)
	}(); err != nil {
		return StorageEncryptionPoolState{}, NewStorageEncryptionError(409,
			"密钥已加载并保存，但 Incus 存储绑定未完成："+lib.NodeErrorDetail(err, "节点未返回具体错误", map[string]any{"key": key})+"；再次解密可继续绑定")
	}

	return PoolEncryption(ctx, node, poolName, vault)
}

// ==================== DB helpers ====================

// StoragePoolRow mirrors the storage_pools projection used across storage operations.
type StoragePoolRow struct {
	ID          int32
	HostID      int32
	Name        string
	Driver      string
	Purpose     string
	Description *string
	Config      []byte
	Telemetry   []byte
	Status      string
	ReportedAt  *time.Time
}

// GetStoragePoolRecord mirrors storagePool.findUnique({ where: { hostId_name: { hostId, name } } }).
func GetStoragePoolRecord(ctx context.Context, hostID int32, name string) (*StoragePoolRow, error) {
	if db.Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	var pool StoragePoolRow
	var config, telemetry []byte
	err := db.Pool().QueryRow(ctx, `SELECT id, host_id, name, driver, purpose::text, description, config, telemetry, status, reported_at
		FROM storage_pools WHERE host_id = $1 AND name = $2`, hostID, name).
		Scan(&pool.ID, &pool.HostID, &pool.Name, &pool.Driver, &pool.Purpose, &pool.Description, &config, &telemetry, &pool.Status, &pool.ReportedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	pool.Config = config
	pool.Telemetry = telemetry
	return &pool, nil
}

func storageDecodeObject(raw []byte) (map[string]any, error) {
	object := map[string]any{}
	if len(raw) == 0 {
		return object, nil
	}
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return map[string]any{}, err
	}
	return object, nil
}

// StoredPoolEncryption mirrors storedPoolEncryption(hostId, name).
func StoredPoolEncryption(ctx context.Context, hostID int32, name string) (map[string]any, error) {
	pool, err := GetStoragePoolRecord(ctx, hostID, name)
	if err != nil {
		return nil, err
	}
	if pool == nil {
		return nil, NewStorageEncryptionError(404, "存储池不存在")
	}
	if pool.Driver != "zfs" {
		return map[string]any{"encrypted": false, "state": "plain", "dataset": "", "encryptionRoot": "", "keySaved": false}, nil
	}
	telemetry, _ := storageDecodeObject(pool.Telemetry)
	state, ok := storageEncryptionAnyObject(telemetry["encryption"])
	if !ok {
		return nil, NewStorageEncryptionError(409, "尚未收到节点存储状态，请验证节点或等待心跳上报")
	}

	result := map[string]any{}
	for key, value := range state {
		result[key] = value
	}
	if agentStorageJSTruthy(state["encrypted"]) {
		result["state"] = state["state"]
	} else {
		result["state"] = "plain"
	}

	keySaved := false
	if encryptionRoot := storageEncryptionStringField(state, "encryptionRoot"); encryptionRoot != "" {
		stored, err := FindStoredStorageKey(ctx, hostID, encryptionRoot)
		if err != nil {
			return nil, err
		}
		keySaved = stored != nil
	}
	result["keySaved"] = keySaved

	config, _ := storageDecodeObject(pool.Config)
	result["pendingImport"] = agentStorageJSTruthy(config[storagePendingImportMarker])
	return result, nil
}

// FindStoredStorageKey mirrors nodeStorageKey.findUnique({ where: { hostId_dataset } }).
func FindStoredStorageKey(ctx context.Context, hostID int32, dataset string) (*StorageEncryptionStoredKey, error) {
	if db.Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	var record StorageEncryptionStoredKey
	err := db.Pool().QueryRow(ctx, `SELECT dataset, ciphertext, fingerprint, version
		FROM node_storage_keys WHERE host_id = $1 AND dataset = $2`, hostID, dataset).
		Scan(&record.Dataset, &record.Ciphertext, &record.Fingerprint, &record.Version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &record, nil
}

func storageEncryptionMarkPoolsPending(ctx context.Context, hostID int32) error {
	if db.Pool() == nil {
		return nil
	}
	_, err := db.Pool().Exec(ctx, `UPDATE storage_pools
		SET status = 'pending', reported_at = NOW(), updated_at = NOW()
		WHERE host_id = $1`, hostID)
	return err
}

// UpdateStoragePoolConfig updates config JSONB on storage_pools for (hostID, name).
func UpdateStoragePoolConfig(ctx context.Context, hostID int32, name string, config []byte) error {
	if db.Pool() == nil {
		return errors.New("database not initialized")
	}
	_, err := db.Pool().Exec(ctx, `UPDATE storage_pools
		SET config = $3::jsonb, updated_at = NOW()
		WHERE host_id = $1 AND name = $2`, hostID, name, string(config))
	return err
}

func storageEncryptionNodeSecurityFingerprint(ctx context.Context, hostID int32) (*string, error) {
	if db.Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	var fingerprint *string
	err := db.Pool().QueryRow(ctx, `SELECT server_fingerprint FROM node_security_states WHERE host_id = $1`, hostID).
		Scan(&fingerprint)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return fingerprint, nil
}

// InsertStorageEncryptionAuditLog mirrors strict prisma.log.create inside lockPool.
func InsertStorageEncryptionAuditLog(ctx context.Context, userID int32, action, content string) error {
	if db.Pool() == nil {
		return errors.New("database not initialized")
	}
	_, err := db.Pool().Exec(ctx, `INSERT INTO logs (user_id, module, action, content, result)
		VALUES ($1, 'security', $2, $3, 'success')`, userID, action, content)
	return err
}

// ==================== storage session ====================

var storageEncryptionBusy = struct {
	sync.Mutex
	hosts map[int32]bool
}{hosts: map[int32]bool{}}

// StorageEncryptionDBVault is the StorageKeyVault backed by node_storage_keys.
type StorageEncryptionDBVault struct {
	HostID int32
}

// Find implements StorageEncryptionKeyVault.
func (v StorageEncryptionDBVault) Find(ctx context.Context, dataset string) (*StorageEncryptionStoredKey, error) {
	return FindStoredStorageKey(ctx, v.HostID, dataset)
}

// Save implements StorageEncryptionKeyVault: upsert, refuse to overwrite a
// different key, then compare-and-set the ciphertext on the exact version.
func (v StorageEncryptionDBVault) Save(ctx context.Context, record StorageEncryptionStoredKey) error {
	if db.Pool() == nil {
		return errors.New("database not initialized")
	}
	var storedFingerprint string
	err := db.Pool().QueryRow(ctx, `INSERT INTO node_storage_keys
		(host_id, dataset, fingerprint, ciphertext, version)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (host_id, dataset) DO UPDATE SET dataset = node_storage_keys.dataset
		RETURNING fingerprint`,
		v.HostID, record.Dataset, record.Fingerprint, record.Ciphertext, record.Version).Scan(&storedFingerprint)
	if err != nil {
		return err
	}
	if storedFingerprint != record.Fingerprint {
		return NewStorageEncryptionError(409, "已有不同密钥，不能直接覆盖")
	}
	tag, err := db.Pool().Exec(ctx, `UPDATE node_storage_keys SET ciphertext = $4
		WHERE host_id = $1 AND dataset = $2 AND fingerprint = $3 AND version = $5`,
		v.HostID, record.Dataset, record.Fingerprint, record.Ciphertext, record.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return NewStorageEncryptionError(409, "存储密钥状态已变化，请刷新后重试")
	}
	return nil
}

// WithStorageSession mirrors withStorageSession(hostId, write, fn) for typed results.
func WithStorageSession[T any](
	ctx context.Context,
	hostID int32,
	write bool,
	fn func(node StorageEncryptionNode, vault StorageEncryptionKeyVault) (T, error),
) (T, error) {
	var zero T
	if write {
		storageEncryptionBusy.Lock()
		if storageEncryptionBusy.hosts[hostID] {
			storageEncryptionBusy.Unlock()
			return zero, NewStorageEncryptionError(409, "节点正在执行存储操作，请稍后重试")
		}
		storageEncryptionBusy.hosts[hostID] = true
		storageEncryptionBusy.Unlock()
	}

	var client *lib.IncusClient
	connected := false
	defer func() {
		syncedBaseline := false
		if client != nil {
			if write && connected {
				cleanupCtx := context.WithoutCancel(ctx)
				if _, err := SyncHostStorageBaseline(cleanupCtx, hostID, client, SyncHostStorageBaselineOptions{SkipPoolReload: true}); err == nil {
					syncedBaseline = true
				}
			}
			_ = client.Close()
		}
		if write {
			if !syncedBaseline {
				cleanupCtx := context.WithoutCancel(ctx)
				if err := storageEncryptionMarkPoolsPending(cleanupCtx, hostID); err != nil {
					log.Printf("[StorageEncryption] session cleanup for host %d failed: %v", hostID, err)
				}
			}
			storageEncryptionBusy.Lock()
			delete(storageEncryptionBusy.hosts, hostID)
			storageEncryptionBusy.Unlock()
			lib.TunnelManagerInstance().RequestHeartbeat(hostID)
		}
	}()

	host, err := db.EgressHostByID(ctx, hostID)
	if err != nil {
		return zero, err
	}
	if host == nil {
		return zero, NewStorageEncryptionError(404, "节点不存在")
	}
	identity, err := storageEncryptionNodeSecurityFingerprint(ctx, hostID)
	if err != nil {
		return zero, err
	}
	if identity == nil || *identity == "" {
		return zero, NewStorageEncryptionError(412, "节点身份尚未核验，不能传输存储密钥")
	}
	if write {
		if err := storageEncryptionMarkPoolsPending(ctx, hostID); err != nil {
			return zero, err
		}
	}

	client = lib.NewIncusClient(lib.IncusClientOptions{
		URL:               host.URL,
		CertPath:          host.CertPath,
		KeyPath:           host.KeyPath,
		HostID:            int64(hostID),
		TunnelEnabled:     host.TunnelEnabled,
		TargetHost:        host.TargetHost,
		TargetPort:        int(host.TargetPort),
		ServerFingerprint: *identity,
	})
	if _, err := client.Connect(ctx); err != nil {
		return zero, err
	}
	connected = true

	return fn(storageEncryptionIncusNode{client: client}, StorageEncryptionDBVault{HostID: hostID})
}
