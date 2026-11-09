// Port of server/src/routes/storage-encryption.ts, plus the ported halves of
// server/src/lib/storage/{read-model,session,lock,encryption,import}.ts and
// server/src/lib/node-security/vault.ts that this route needs.
//
// app.ts registers this plugin with no prefix, so the paths live directly under
// /api:
//
//	GET|POST|PUT /api/hosts/:id/storage-pools/:poolName/encryption
//	POST         /api/hosts/:id/storage-pools/:poolName/encryption/reveal
//
// The key material never leaves the control plane except through the audited
// reveal endpoint; the vault envelope (seal/unseal) is a byte-compatible port of
// node-security/vault.ts, so keys written by Node open in Go and vice versa.
//
// ---------------------------------------------------------------------------
// SEAMS (Phase 7 must wire these)
// ---------------------------------------------------------------------------
//  1. lib.IncusClient's pinned-fingerprint transport is NOT ported (node-security
//     wave), and withStorageSession always passes serverFingerprint. Every node
//     WRITE path therefore fails explicitly today ("节点身份固定校验…尚未移植"),
//     which the route surfaces as a 409 with nodeErrorDetail. The read paths
//     (GET, reveal) need no node access and work fully.
//  2. StorageEncryptionRequestHeartbeat / …AndWait replace
//     hostTunnelManager.requestHeartbeat / requestHeartbeatAndWait.
//  3. StorageEncryptionResumeInstances replaces node-security/recovery's
//     restoreNodeInstances. The guard around it (node/storage still locked) IS
//     ported, so a pool with no pending instances needs no seam at all.
package routes

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

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/hkdf"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
)

// ==================== seams ====================

// StorageEncryptionRequestHeartbeat mirrors hostTunnelManager.requestHeartbeat.
// SEAM: wire to the tunnel manager in Phase 7. A missed nudge is harmless — the
// next real heartbeat re-reads the same state.
var StorageEncryptionRequestHeartbeat = func(hostID int32) {}

// StorageEncryptionRequestHeartbeatAndWait mirrors
// hostTunnelManager.requestHeartbeatAndWait(hostId, timeout). The POST handler
// depends on it to confirm the unlocked state actually reached the panel, so the
// default FAILS EXPLICITLY (the handler then returns 504 instead of claiming
// success).
var StorageEncryptionRequestHeartbeatAndWait = func(ctx context.Context, hostID int32, timeout time.Duration) error {
	return fmt.Errorf("节点心跳确认（hostTunnelManager.requestHeartbeatAndWait）尚未移植")
}

// StorageEncryptionResumeInstances mirrors node-security/recovery's
// restoreNodeInstances call inside restoreStorageInstances. Default fails
// explicitly; it is only reached when the node reports pending resume instances.
var StorageEncryptionResumeInstances = func(
	ctx context.Context, node StorageEncryptionNode, dataset string, hostID int32, revision int32, instances []any,
) error {
	return fmt.Errorf("节点存储实例恢复（node-security/recovery）尚未移植")
}

// ==================== errors ====================

// StorageEncryptionError mirrors StorageEncryptionError(statusCode, message).
type StorageEncryptionError struct {
	StatusCode int
	Message    string
}

// Error implements error.
func (e *StorageEncryptionError) Error() string { return e.Message }

func newStorageEncryptionError(status int, message string) *StorageEncryptionError {
	return &StorageEncryptionError{StatusCode: status, Message: message}
}

// storageEncryptionInternal reports an unexpected failure.
func storageEncryptionInternal(c *gin.Context, err error) {
	message := "Internal server error"
	if !lib.IsProductionEnv() && err != nil {
		message = err.Error()
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": message})
}

// ==================== vault (node-security/vault.ts) ====================

// StorageEncryptionKeyIdentity mirrors KeyIdentity.
type StorageEncryptionKeyIdentity struct {
	HostID  int32
	Dataset string
	Version int32
}

// storageEncryptionDatasetPattern mirrors datasetPattern.
var storageEncryptionDatasetPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]*(\/[A-Za-z0-9_.:-]+)*$`)

// storageEncryptionKeyPattern mirrors /^[0-9a-f]{64}$/.
var storageEncryptionKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// storageEncryptionIVPattern mirrors /^[0-9a-f]{24}$/ (12-byte GCM nonce).
var storageEncryptionIVPattern = regexp.MustCompile(`^[0-9a-f]{24}$`)

// storageEncryptionTagPattern mirrors /^[0-9a-f]{32}$/ (16-byte GCM tag).
var storageEncryptionTagPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// StorageEncryptionFingerprint mirrors fingerprint(key): sha256 of the raw key
// bytes. It rejects a malformed key, exactly like the TypeScript.
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

// storageEncryptionIdentity mirrors identity(value): the AAD of the envelope.
func storageEncryptionIdentity(identity StorageEncryptionKeyIdentity) ([]byte, error) {
	if identity.HostID < 1 || identity.Version < 1 ||
		!storageEncryptionDatasetPattern.MatchString(identity.Dataset) ||
		strings.Contains(identity.Dataset, "..") || len(identity.Dataset) > 200 {
		return nil, errors.New("无效的存储身份")
	}
	encoded, err := json.Marshal([]any{identity.HostID, identity.Dataset, identity.Version})
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// storageEncryptionMaster mirrors master(secret): HKDF-SHA256 over ENCRYPTION_KEY
// with the fixed salt/info labels. A short secret is rejected — never silently
// stretched — because the envelope is a custody boundary.
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
	// Node: AES-GCM 16-byte tag appended to the ciphertext.
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

// storageEncryptionSecret mirrors `process.env.ENCRYPTION_KEY || ”`. The vault
// deliberately does NOT fall back to JWT_SECRET (unlike lib.EncryptSensitiveData)
// because rotating the control-plane master key must be an explicit operation.
func storageEncryptionSecret() string {
	return os.Getenv("ENCRYPTION_KEY")
}

// ==================== node abstraction ====================

// StorageEncryptionNode mirrors the TypeScript `StorageNode` structural type: a
// node request function. Decoding to `any` keeps the nodeForPendingPool decorator
// able to rewrite one response.
type StorageEncryptionNode interface {
	Request(ctx context.Context, method, path string, body any) (any, error)
}

// storageEncryptionIncusNode adapts lib.IncusClient to the node contract.
type storageEncryptionIncusNode struct {
	client *lib.IncusClient
}

// Request implements StorageEncryptionNode.
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

// storageEncryptionAnyObject mirrors `typeof value === 'object' && !Array.isArray`.
func storageEncryptionAnyObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

// storageEncryptionStringField mirrors `object[key]` when it is a string.
func storageEncryptionStringField(object map[string]any, key string) string {
	text, _ := object[key].(string)
	return text
}

// storageEncryptionBoolField mirrors `object[key] === true`.
func storageEncryptionBoolField(object map[string]any, key string) bool {
	return object[key] == true
}

// ==================== pool encryption ====================

// storageEncryptionPoolState is the typed form of poolEncryption()'s return value.
type storageEncryptionPoolState struct {
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

// toJSON mirrors the exact shape poolEncryption returns on the wire: the plain
// branch only has the five keys, the encrypted branch has all eleven.
func (s storageEncryptionPoolState) toJSON() gin.H {
	if !s.Encrypted {
		return gin.H{
			"encrypted":      false,
			"state":          "plain",
			"dataset":        s.Dataset,
			"encryptionRoot": s.EncryptionRoot,
			"keySaved":       false,
		}
	}
	return gin.H{
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

// storageEncryptionStoredKey mirrors StoredKey.
type storageEncryptionStoredKey struct {
	Dataset     string
	Ciphertext  string
	Fingerprint string
	Version     int32
}

// storageEncryptionKeyVault mirrors StorageKeyVault.
type storageEncryptionKeyVault interface {
	Find(ctx context.Context, dataset string) (*storageEncryptionStoredKey, error)
	Save(ctx context.Context, record storageEncryptionStoredKey) error
}

// storageEncryptionPoolEncryption mirrors poolEncryption(node, poolName, vault).
func storageEncryptionPoolEncryption(
	ctx context.Context, node StorageEncryptionNode, poolName string, vault storageEncryptionKeyVault,
) (storageEncryptionPoolState, error) {
	infoRaw, err := node.Request(ctx, http.MethodGet, "/1.0/payincus", nil)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}
	info, ok := storageEncryptionAnyObject(infoRaw)
	if !ok {
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "节点尚不支持存储加密管理，请先更新节点集成包")
	}
	extensions, ok := info["api_extensions"].([]any)
	if !ok || !storageEncryptionAnyContains(extensions, "payincus_storage_encryption_v1") {
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "节点尚不支持存储加密管理，请先更新节点集成包")
	}

	poolRaw, err := node.Request(ctx, http.MethodGet, "/1.0/storage-pools/"+url.PathEscape(poolName), nil)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}
	pool, ok := storageEncryptionAnyObject(poolRaw)
	if !ok {
		return storageEncryptionPoolState{}, newStorageEncryptionError(502, "节点返回的存储池信息无效")
	}
	if storageEncryptionStringField(pool, "driver") != "zfs" {
		return storageEncryptionPoolState{State: "plain"}, nil
	}
	config, _ := storageEncryptionAnyObject(pool["config"])
	source := storageEncryptionStringField(config, "zfs.pool_name")
	if source == "" {
		source = storageEncryptionStringField(config, "source")
	}
	if source == "" || strings.HasPrefix(source, "/") {
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "无法确定此存储池的 ZFS 数据集，请刷新存储配置")
	}

	inventoryRaw, err := node.Request(ctx, http.MethodGet, "/1.0/payincus/storage", nil)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}
	inventory, ok := storageEncryptionAnyObject(inventoryRaw)
	if !ok {
		return storageEncryptionPoolState{}, newStorageEncryptionError(502, "节点返回的加密状态无效")
	}
	datasets, ok := inventory["datasets"].([]any)
	if !ok {
		return storageEncryptionPoolState{}, newStorageEncryptionError(502, "节点返回的加密状态无效")
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
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "ZFS 数据集暂不可见，可能未导入或已离线，请检查节点状态")
	}
	algorithm := storageEncryptionStringField(item, "algorithm")
	if algorithm == "off" || algorithm == "-" {
		return storageEncryptionPoolState{Dataset: source, State: "plain"}, nil
	}
	encryptionRoot := storageEncryptionStringField(item, "encryptionRoot")
	keyStatus := storageEncryptionStringField(item, "keyStatus")
	if encryptionRoot == "" || (keyStatus != "available" && keyStatus != "unavailable") || !strings.HasPrefix(algorithm, "aes-") {
		return storageEncryptionPoolState{}, newStorageEncryptionError(502, "节点返回的加密状态无效")
	}

	stored, err := vault.Find(ctx, encryptionRoot)
	if err != nil {
		return storageEncryptionPoolState{}, err
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
	return storageEncryptionPoolState{
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

// storageEncryptionAnyContains mirrors Array.prototype.includes for []any.
func storageEncryptionAnyContains(values []any, candidate string) bool {
	for _, value := range values {
		if text, ok := value.(string); ok && text == candidate {
			return true
		}
	}
	return false
}

// storageEncryptionUnlockOptions mirrors `{ automatic?: boolean }`.
type storageEncryptionUnlockOptions struct {
	Automatic bool
}

// storageEncryptionUnlockPool mirrors unlockPool(node, poolName, hostId, vault, key, options).
func storageEncryptionUnlockPool(
	ctx context.Context,
	node StorageEncryptionNode,
	poolName string,
	hostID int32,
	vault storageEncryptionKeyVault,
	suppliedKey string,
	options storageEncryptionUnlockOptions,
) (storageEncryptionPoolState, error) {
	state, err := storageEncryptionPoolEncryption(ctx, node, poolName, vault)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}
	if !state.Encrypted {
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "此存储池没有 ZFS 加密，不能执行解锁")
	}
	if options.Automatic && state.ManualLocked {
		return storageEncryptionPoolState{}, newStorageEncryptionError(423, "存储已手动锁盘，必须手动解密")
	}
	if state.KeyFormat != "hex" {
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "此加密卷使用的密钥格式暂不支持，请先在节点转换为 hex 密钥格式")
	}
	existing, err := vault.Find(ctx, state.EncryptionRoot)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}

	key := suppliedKey
	if key == "" {
		if existing == nil {
			return storageEncryptionPoolState{}, newStorageEncryptionError(409, "请先输入此加密卷的密钥并保存")
		}
		unsealed, err := StorageEncryptionUnseal(existing.Ciphertext, StorageEncryptionKeyIdentity{
			HostID: hostID, Dataset: existing.Dataset, Version: existing.Version,
		}, storageEncryptionSecret())
		if err != nil {
			return storageEncryptionPoolState{}, newStorageEncryptionError(503, "已保存密钥无法读取，请检查控制端主密钥配置")
		}
		key = unsealed
	}
	if !storageEncryptionKeyPattern.MatchString(key) {
		return storageEncryptionPoolState{}, newStorageEncryptionError(400, "请输入 64 位十六进制 ZFS 密钥")
	}
	fingerprint, err := StorageEncryptionFingerprint(key)
	if err != nil {
		return storageEncryptionPoolState{}, newStorageEncryptionError(400, "请输入 64 位十六进制 ZFS 密钥")
	}
	if existing != nil && existing.Fingerprint != fingerprint {
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "已有不同的托管密钥，不能直接覆盖，请先核对存储卷身份")
	}
	version := int32(1)
	if existing != nil && existing.Version != 0 {
		version = existing.Version
	}
	ciphertext, err := StorageEncryptionSeal(key, StorageEncryptionKeyIdentity{
		HostID: hostID, Dataset: state.EncryptionRoot, Version: version,
	}, storageEncryptionSecret())
	if err != nil {
		return storageEncryptionPoolState{}, newStorageEncryptionError(503, "控制端密钥托管未就绪，请检查主密钥配置")
	}

	nodeStateRaw, err := node.Request(ctx, http.MethodGet, "/1.0/payincus/security", nil)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}
	nodeState, ok := storageEncryptionAnyObject(nodeStateRaw)
	if !ok {
		return storageEncryptionPoolState{}, newStorageEncryptionError(502, "无法核验节点存储注册状态")
	}
	lockedValue, lockedIsBool := nodeState["locked"].(bool)
	if !lockedIsBool {
		return storageEncryptionPoolState{}, newStorageEncryptionError(502, "无法核验节点存储注册状态")
	}
	targetsRaw, targetsPresent := nodeState["targets"]
	var targets []any
	if targetsRaw != nil {
		array, ok := targetsRaw.([]any)
		if !ok {
			return storageEncryptionPoolState{}, newStorageEncryptionError(502, "无法核验节点存储注册状态")
		}
		targets = array
	} else if !targetsPresent {
		return storageEncryptionPoolState{}, newStorageEncryptionError(502, "无法核验节点存储注册状态")
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
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "密钥与节点登记的加密卷不匹配")
	}
	if registered == nil && lockedValue {
		return storageEncryptionPoolState{}, newStorageEncryptionError(423, "节点安全锁定中，不能登记新的加密卷")
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
	if _, err := node.Request(ctx, http.MethodPost, "/1.0/payincus/storage/"+action,
		map[string]any{"dataset": state.EncryptionRoot, "key": key}); err != nil {
		return storageEncryptionPoolState{}, err
	}
	if err := vault.Save(ctx, storageEncryptionStoredKey{
		Dataset: state.EncryptionRoot, Fingerprint: fingerprint, Ciphertext: ciphertext, Version: version,
	}); err != nil {
		var storageError *StorageEncryptionError
		if errors.As(err, &storageError) {
			return storageEncryptionPoolState{}, err
		}
		return storageEncryptionPoolState{}, newStorageEncryptionError(503, "节点已加载密钥，但控制端保存失败，请保留密钥后重试")
	}
	key = ""

	if !options.Automatic || state.PendingRecovery > 0 {
		if err := storageEncryptionRestoreStorageInstances(ctx, node, state.EncryptionRoot, hostID); err != nil {
			return storageEncryptionPoolState{}, err
		}
	}

	verified, err := storageEncryptionPoolEncryption(ctx, node, poolName, vault)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}
	if !verified.Encrypted || verified.State != "unlocked" || !verified.KeySaved {
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "节点尚未确认解锁完成，请刷新状态后重试")
	}
	return verified, nil
}

// storageEncryptionRestoreStorageInstances mirrors restoreStorageInstances: the
// guard is ported, the per-instance resume loop is a seam.
func storageEncryptionRestoreStorageInstances(
	ctx context.Context, node StorageEncryptionNode, dataset string, hostID int32,
) error {
	raw, err := node.Request(ctx, http.MethodGet, "/1.0/payincus/security", nil)
	if err != nil {
		return err
	}
	state, ok := storageEncryptionAnyObject(raw)
	if !ok {
		return nil
	}
	storageLocks, _ := storageEncryptionAnyObject(state["storageLocks"])
	lock, _ := storageEncryptionAnyObject(storageLocks[dataset])
	resume, _ := lock["resumeInstances"].([]any)
	if len(resume) == 0 {
		return nil
	}
	if storageEncryptionBoolField(state, "locked") || storageEncryptionBoolField(lock, "locked") {
		return newStorageEncryptionError(423, "存储密钥已加载，但节点或存储仍被锁定，实例尚未恢复")
	}
	revision, _ := state["revision"].(float64)
	return StorageEncryptionResumeInstances(ctx, node, dataset, hostID, int32(revision), resume)
}

// storageEncryptionLockPool mirrors lockPool(node, poolName, vault, actorId, hostId).
func storageEncryptionLockPool(
	ctx context.Context,
	node StorageEncryptionNode,
	poolName string,
	vault storageEncryptionKeyVault,
	actorID int32,
	hostID int32,
) (storageEncryptionPoolState, error) {
	state, err := storageEncryptionPoolEncryption(ctx, node, poolName, vault)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}
	if !state.Encrypted {
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "此磁盘未配置 ZFS 加密，不能卸载密钥锁盘")
	}
	if !state.KeySaved {
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "请先保存并校验解锁密钥，再执行锁盘")
	}
	stored, err := vault.Find(ctx, state.EncryptionRoot)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}
	custodyValid := false
	if stored != nil {
		unsealed, unsealErr := StorageEncryptionUnseal(stored.Ciphertext, StorageEncryptionKeyIdentity{
			HostID: hostID, Dataset: stored.Dataset, Version: stored.Version,
		}, storageEncryptionSecret())
		if unsealErr == nil {
			fingerprint, fingerprintErr := StorageEncryptionFingerprint(unsealed)
			custodyValid = fingerprintErr == nil && fingerprint == stored.Fingerprint
		}
	}
	if !custodyValid {
		return storageEncryptionPoolState{}, newStorageEncryptionError(503, "托管密钥暂不可用，未停止实例或锁盘，请先检查控制端主密钥配置")
	}

	registrationRaw, err := node.Request(ctx, http.MethodGet, "/1.0/payincus/security", nil)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}
	registration, ok := storageEncryptionAnyObject(registrationRaw)
	if !ok {
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "托管密钥与节点加密卷登记不一致，未停止实例或锁盘")
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
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "托管密钥与节点加密卷登记不一致，未停止实例或锁盘")
	}

	// Audit before any stop/unload. Unlike createLog this INSERT is allowed to
	// throw, because the TypeScript aborts the lock when the audit row cannot be
	// written.
	if err := storageEncryptionInsertAuditLog(ctx, actorID, "storage.lock.request",
		fmt.Sprintf("节点 #%d 管理员锁定存储池 %s", hostID, poolName)); err != nil {
		return storageEncryptionPoolState{}, err
	}
	if _, err := node.Request(ctx, http.MethodPost, "/1.0/payincus/storage/lock-pool",
		map[string]any{"dataset": state.EncryptionRoot}); err != nil {
		return storageEncryptionPoolState{}, err
	}

	verified, err := storageEncryptionPoolEncryption(ctx, node, poolName, vault)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}
	if !verified.Encrypted || verified.State != "locked" || !verified.ManualLocked || verified.LockPending {
		return storageEncryptionPoolState{}, newStorageEncryptionError(409, "锁盘未完成，请检查相关实例和存储占用后重试")
	}
	return verified, nil
}

// ==================== pending imports ====================

// storageEncryptionPendingImport mirrors PendingImport.
type storageEncryptionPendingImport struct {
	Source         string
	EncryptionRoot string
}

// storageEncryptionValidSource mirrors validSource(s).
func storageEncryptionValidSource(value string) bool {
	return storageEncryptionDatasetPattern.MatchString(value) &&
		!strings.Contains(value, "..") && len(value) <= 200
}

// storageEncryptionPendingRecord mirrors pendingRecord(hostId, poolName).
func storageEncryptionPendingRecord(ctx context.Context, hostID int32, poolName string) ([]byte, *storageEncryptionPendingImport, error) {
	pool, err := storageEncryptionGetPool(ctx, hostID, poolName)
	if err != nil {
		return nil, nil, err
	}
	if pool == nil {
		return nil, nil, nil
	}
	config, _ := storageEncryptionDecodeObject(pool.Config)
	rawPending, ok := config["_payincusImport"].(map[string]any)
	if !ok {
		return pool.Config, nil, nil
	}
	pending := &storageEncryptionPendingImport{
		Source:         storageEncryptionStringField(rawPending, "source"),
		EncryptionRoot: storageEncryptionStringField(rawPending, "encryptionRoot"),
	}
	if !storageEncryptionValidSource(pending.Source) || !storageEncryptionValidSource(pending.EncryptionRoot) {
		return nil, nil, newStorageEncryptionError(409, "待导入存储记录无效，请检查节点数据集身份")
	}
	return pool.Config, pending, nil
}

// storageEncryptionPendingNode mirrors nodeForPendingPool: a decorator that makes
// a pending encrypted import visible before Incus can mount the locked dataset.
type storageEncryptionPendingNode struct {
	inner    StorageEncryptionNode
	poolName string
	pending  *storageEncryptionPendingImport
}

// Request implements StorageEncryptionNode.
func (n storageEncryptionPendingNode) Request(ctx context.Context, method, path string, body any) (any, error) {
	poolPath := "/1.0/storage-pools/" + url.PathEscape(n.poolName)
	if method == http.MethodGet && path == poolPath {
		pathsRaw, err := n.inner.Request(ctx, http.MethodGet, "/1.0/storage-pools", nil)
		if err != nil {
			return nil, err
		}
		paths, ok := pathsRaw.([]any)
		if !ok {
			return nil, newStorageEncryptionError(502, "无法读取节点存储列表")
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
			return nil, newStorageEncryptionError(409, "同名存储池来源已变化，拒绝绑定待导入卷")
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

// storageEncryptionUnlockAndBindPool mirrors unlockAndBindPool.
func storageEncryptionUnlockAndBindPool(
	ctx context.Context,
	node StorageEncryptionNode,
	hostID int32,
	poolName string,
	vault storageEncryptionKeyVault,
	key string,
) (storageEncryptionPoolState, error) {
	_, pending, err := storageEncryptionPendingRecord(ctx, hostID, poolName)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}
	decorated := node
	if pending != nil {
		decorated = storageEncryptionPendingNode{inner: node, poolName: poolName, pending: pending}
	}

	result, err := storageEncryptionUnlockPool(ctx, decorated, poolName, hostID, vault, key, storageEncryptionUnlockOptions{})
	if err != nil {
		return storageEncryptionPoolState{}, err
	}

	record, pendingAfter, err := storageEncryptionPendingRecord(ctx, hostID, poolName)
	if err != nil {
		return storageEncryptionPoolState{}, err
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
			if pool, poolErr := storageEncryptionGetPool(ctx, hostID, poolName); poolErr == nil && pool != nil && pool.Description != nil {
				description = *pool.Description
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
		return storageEncryptionUpdatePoolConfig(ctx, hostID, poolName, encoded)
	}(); err != nil {
		return storageEncryptionPoolState{}, newStorageEncryptionError(409,
			"密钥已加载并保存，但 Incus 存储绑定未完成："+lib.NodeErrorDetail(err, "节点未返回具体错误", map[string]any{"key": key})+"；再次解密可继续绑定")
	}

	return storageEncryptionPoolEncryption(ctx, node, poolName, vault)
}

// ==================== DB access (no owned db file for this module) ====================

// storageEncryptionPoolRow is the storage_pools projection this module reads.
type storageEncryptionPoolRow struct {
	ID          int32
	HostID      int32
	Name        string
	Driver      string
	Description *string
	Config      []byte
	Telemetry   []byte
	Status      string
	ReportedAt  *time.Time
}

// storageEncryptionGetPool mirrors
// `storagePool.findUnique({ where: { hostId_name: { hostId, name } } })`.
func storageEncryptionGetPool(ctx context.Context, hostID int32, name string) (*storageEncryptionPoolRow, error) {
	if db.Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	var pool storageEncryptionPoolRow
	var config, telemetry []byte
	err := db.Pool().QueryRow(ctx, `SELECT id, host_id, name, driver, description, config, telemetry, status, reported_at
		FROM storage_pools WHERE host_id = $1 AND name = $2`, hostID, name).
		Scan(&pool.ID, &pool.HostID, &pool.Name, &pool.Driver, &pool.Description, &config, &telemetry, &pool.Status, &pool.ReportedAt)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	pool.Config = config
	pool.Telemetry = telemetry
	return &pool, nil
}

// storageEncryptionDecodeObject decodes a jsonb document into an object.
func storageEncryptionDecodeObject(raw []byte) (map[string]any, error) {
	object := map[string]any{}
	if len(raw) == 0 {
		return object, nil
	}
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return map[string]any{}, err
	}
	return object, nil
}

// storageEncryptionStoredPoolEncryption mirrors storedPoolEncryption(hostId, name).
func storageEncryptionStoredPoolEncryption(ctx context.Context, hostID int32, name string) (gin.H, error) {
	pool, err := storageEncryptionGetPool(ctx, hostID, name)
	if err != nil {
		return nil, err
	}
	if pool == nil {
		return nil, newStorageEncryptionError(404, "存储池不存在")
	}
	if pool.Driver != "zfs" {
		return gin.H{"encrypted": false, "state": "plain", "dataset": "", "encryptionRoot": "", "keySaved": false}, nil
	}
	telemetry, _ := storageEncryptionDecodeObject(pool.Telemetry)
	state, ok := storageEncryptionAnyObject(telemetry["encryption"])
	if !ok {
		return nil, newStorageEncryptionError(409, "尚未收到节点存储状态，请验证节点或等待心跳上报")
	}

	result := gin.H{}
	for key, value := range state {
		result[key] = value
	}
	if egressJSTruthy(state["encrypted"]) {
		result["state"] = state["state"]
	} else {
		result["state"] = "plain"
	}

	keySaved := false
	if encryptionRoot := storageEncryptionStringField(state, "encryptionRoot"); encryptionRoot != "" {
		stored, err := storageEncryptionFindStoredKey(ctx, hostID, encryptionRoot)
		if err != nil {
			return nil, err
		}
		keySaved = stored != nil
	}
	result["keySaved"] = keySaved

	config, _ := storageEncryptionDecodeObject(pool.Config)
	result["pendingImport"] = egressJSTruthy(config["_payincusImport"])
	return result, nil
}

// storageEncryptionFindStoredKey mirrors
// `nodeStorageKey.findUnique({ where: { hostId_dataset } })`.
func storageEncryptionFindStoredKey(ctx context.Context, hostID int32, dataset string) (*storageEncryptionStoredKey, error) {
	if db.Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	var record storageEncryptionStoredKey
	err := db.Pool().QueryRow(ctx, `SELECT dataset, ciphertext, fingerprint, version
		FROM node_storage_keys WHERE host_id = $1 AND dataset = $2`, hostID, dataset).
		Scan(&record.Dataset, &record.Ciphertext, &record.Fingerprint, &record.Version)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &record, nil
}

// storageEncryptionMarkPoolsPending mirrors
// `storagePool.updateMany({ where: { hostId }, data: { status: 'pending', reportedAt: new Date() } })`.
func storageEncryptionMarkPoolsPending(ctx context.Context, hostID int32) error {
	_, err := db.Pool().Exec(ctx, `UPDATE storage_pools
		SET status = 'pending', reported_at = NOW(), updated_at = NOW()
		WHERE host_id = $1`, hostID)
	return err
}

// storageEncryptionUpdatePoolConfig mirrors
// `storagePool.update({ where: { id }, data: { config: actual.config } })`.
func storageEncryptionUpdatePoolConfig(ctx context.Context, hostID int32, name string, config []byte) error {
	_, err := db.Pool().Exec(ctx, `UPDATE storage_pools
		SET config = $3, updated_at = NOW()
		WHERE host_id = $1 AND name = $2`, hostID, name, string(config))
	return err
}

// storageEncryptionNodeSecurityFingerprint mirrors
// `nodeSecurityState.findUnique({ where: { hostId } })`'s fingerprint column.
func storageEncryptionNodeSecurityFingerprint(ctx context.Context, hostID int32) (*string, error) {
	if db.Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	var fingerprint *string
	err := db.Pool().QueryRow(ctx, `SELECT server_fingerprint FROM node_security_states WHERE host_id = $1`, hostID).
		Scan(&fingerprint)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return fingerprint, nil
}

// storageEncryptionInsertAuditLog mirrors the direct `prisma.log.create` in
// lockPool (which is allowed to fail and abort the operation).
func storageEncryptionInsertAuditLog(ctx context.Context, userID int32, action, content string) error {
	_, err := db.Pool().Exec(ctx, `INSERT INTO logs (user_id, module, action, content, result)
		VALUES ($1, 'security', $2, $3, 'success')`, userID, action, content)
	return err
}

// isNoRows reports pgx's ErrNoRows.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// ==================== storage session ====================

// storageEncryptionBusy mirrors the per-host `busy` set. Single process, so an
// in-memory guard is exact.
var storageEncryptionBusy = struct {
	sync.Mutex
	hosts map[int32]bool
}{hosts: map[int32]bool{}}

// storageEncryptionDBVault is the StorageKeyVault backed by node_storage_keys.
type storageEncryptionDBVault struct {
	hostID int32
}

// Find implements storageEncryptionKeyVault.
func (v storageEncryptionDBVault) Find(ctx context.Context, dataset string) (*storageEncryptionStoredKey, error) {
	return storageEncryptionFindStoredKey(ctx, v.hostID, dataset)
}

// Save implements storageEncryptionKeyVault: upsert, refuse to overwrite a
// different key, then compare-and-set the ciphertext on the exact version.
func (v storageEncryptionDBVault) Save(ctx context.Context, record storageEncryptionStoredKey) error {
	if db.Pool() == nil {
		return errors.New("database not initialized")
	}
	var storedFingerprint string
	err := db.Pool().QueryRow(ctx, `INSERT INTO node_storage_keys
		(host_id, dataset, fingerprint, ciphertext, version)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (host_id, dataset) DO UPDATE SET dataset = node_storage_keys.dataset
		RETURNING fingerprint`,
		v.hostID, record.Dataset, record.Fingerprint, record.Ciphertext, record.Version).Scan(&storedFingerprint)
	if err != nil {
		return err
	}
	if storedFingerprint != record.Fingerprint {
		return newStorageEncryptionError(409, "已有不同密钥，不能直接覆盖")
	}
	tag, err := db.Pool().Exec(ctx, `UPDATE node_storage_keys SET ciphertext = $4
		WHERE host_id = $1 AND dataset = $2 AND fingerprint = $3 AND version = $5`,
		v.hostID, record.Dataset, record.Fingerprint, record.Ciphertext, record.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return newStorageEncryptionError(409, "存储密钥状态已变化，请刷新后重试")
	}
	return nil
}

// storageEncryptionWithStorageSession mirrors withStorageSession(hostId, write, fn).
func storageEncryptionWithStorageSession(
	ctx context.Context,
	hostID int32,
	write bool,
	fn func(node StorageEncryptionNode, vault storageEncryptionKeyVault) (storageEncryptionPoolState, error),
) (storageEncryptionPoolState, error) {
	if write {
		storageEncryptionBusy.Lock()
		if storageEncryptionBusy.hosts[hostID] {
			storageEncryptionBusy.Unlock()
			return storageEncryptionPoolState{}, newStorageEncryptionError(409, "节点正在执行存储操作，请稍后重试")
		}
		storageEncryptionBusy.hosts[hostID] = true
		storageEncryptionBusy.Unlock()
	}

	var client *lib.IncusClient
	defer func() {
		if client != nil {
			_ = client.Close()
		}
		if write {
			cleanupCtx := context.WithoutCancel(ctx)
			if err := storageEncryptionMarkPoolsPending(cleanupCtx, hostID); err != nil {
				storageEncryptionLogCleanup(hostID, err)
			}
			storageEncryptionBusy.Lock()
			delete(storageEncryptionBusy.hosts, hostID)
			storageEncryptionBusy.Unlock()
			StorageEncryptionRequestHeartbeat(hostID)
		}
	}()

	host, err := db.EgressHostByID(ctx, hostID)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}
	if host == nil {
		return storageEncryptionPoolState{}, newStorageEncryptionError(404, "节点不存在")
	}
	identity, err := storageEncryptionNodeSecurityFingerprint(ctx, hostID)
	if err != nil {
		return storageEncryptionPoolState{}, err
	}
	if identity == nil || *identity == "" {
		return storageEncryptionPoolState{}, newStorageEncryptionError(412, "节点身份尚未核验，不能传输存储密钥")
	}
	if write {
		if err := storageEncryptionMarkPoolsPending(ctx, hostID); err != nil {
			return storageEncryptionPoolState{}, err
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
		return storageEncryptionPoolState{}, err
	}

	return fn(storageEncryptionIncusNode{client: client}, storageEncryptionDBVault{hostID: hostID})
}

// storageEncryptionLogCleanup keeps the cleanup failure visible without changing
// the caller's result (mirrors the TypeScript swallowing close/update errors).
func storageEncryptionLogCleanup(hostID int32, err error) {
	log.Printf("[StorageEncryption] session cleanup for host %d failed: %v", hostID, err)
}

// ==================== handlers ====================

// storageEncryptionHostIDPattern mirrors the params schema.
var storageEncryptionHostIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

// storageEncryptionPoolNamePattern mirrors the params schema.
var storageEncryptionPoolNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// storageEncryptionParams mirrors the params schema
// `{ type: object, additionalProperties: false, required: [id, poolName],
//
//	properties: { id: {type: string, pattern: '^[1-9][0-9]{0,9}$'},
//	              poolName: {type: string, pattern: '^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$'} } }`.
func storageEncryptionParams(c *gin.Context) (int32, string, bool) {
	rawID := c.Param("hostId")
	if !storageEncryptionHostIDPattern.MatchString(rawID) {
		respondAJVValidation(c, "id format is invalid")
		return 0, "", false
	}
	poolName := c.Param("poolName")
	if !storageEncryptionPoolNamePattern.MatchString(poolName) {
		respondAJVValidation(c, "poolName format is invalid")
		return 0, "", false
	}
	parsed, ok := lib.ParsePositiveRouteID(rawID)
	if !ok || parsed > 2147483647 {
		// The handler-level check: a 10-digit id above the Int32 range.
		c.JSON(http.StatusBadRequest, gin.H{"error": "节点 ID 无效"})
		return 0, "", false
	}
	return int32(parsed), poolName, true
}

// storageEncryptionRespondNodeError mirrors the route's catch block.
func storageEncryptionRespondNodeError(c *gin.Context, err error, body any) {
	var storageError *StorageEncryptionError
	if errors.As(err, &storageError) {
		c.JSON(storageError.StatusCode, gin.H{"error": storageError.Message})
		return
	}
	status := http.StatusConflict
	var incusError *lib.IncusApiError
	if errors.As(err, &incusError) {
		status = incusError.StatusCode
		if status == http.StatusUnauthorized {
			status = http.StatusBadGateway
		}
	}
	c.JSON(status, gin.H{"error": lib.NodeErrorDetail(err, "节点存储操作未完成，请检查连接与状态", body)})
}

// storageEncryptionHandler mirrors the GET/POST/PUT registration loop.
func storageEncryptionHandler(c *gin.Context, method string) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")

	hostID, poolName, ok := storageEncryptionParams(c)
	if !ok {
		return
	}

	var body any
	if method != http.MethodGet {
		decoded, status, bodyMessage := decodeJSONRequestBody(c)
		if status != 0 {
			respondAJVValidation(c, bodyMessage)
			return
		}
		validated, message := validateAJVObject(decoded.Value, nil, []ajvFieldSpec{
			{Name: "key", Kind: ajvKindString, MinLength: -1, MaxLength: -1},
		})
		if message != "" {
			respondAJVValidation(c, message)
			return
		}
		if key, present := validated.stringValue("key"); present && !storageEncryptionKeyPattern.MatchString(key) {
			respondAJVValidation(c, "key format is invalid")
			return
		}
		body = decoded.Value
	}

	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	host, err := db.EgressHostByID(ctx, hostID)
	if err != nil {
		storageEncryptionInternal(c, err)
		return
	}
	if host == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
		return
	}
	if user.Role != "admin" && user.ID != host.UserID {
		c.JSON(http.StatusForbidden, gin.H{"error": "无权管理此节点存储"})
		return
	}

	if method == http.MethodGet {
		state, err := storageEncryptionStoredPoolEncryption(ctx, hostID, poolName)
		if err != nil {
			storageEncryptionRespondNodeError(c, err, body)
			return
		}
		c.JSON(http.StatusOK, state)
		return
	}

	key := ""
	if object, ok := body.(map[string]any); ok {
		if text, ok := object["key"].(string); ok {
			key = text
		}
	}

	result, err := storageEncryptionWithStorageSession(ctx, hostID, true,
		func(node StorageEncryptionNode, vault storageEncryptionKeyVault) (storageEncryptionPoolState, error) {
			if method == http.MethodPut {
				return storageEncryptionLockPool(ctx, node, poolName, vault, user.ID, hostID)
			}
			return storageEncryptionUnlockAndBindPool(ctx, node, hostID, poolName, vault, key)
		})
	if err != nil {
		storageEncryptionRespondNodeError(c, err, body)
		return
	}

	if method == http.MethodPost {
		// The node already confirmed the key state, but the persisted telemetry the
		// list view reads is written by the next heartbeat. Wait for it so a caller
		// never reports success while the pool still reads as locked.
		if err := StorageEncryptionRequestHeartbeatAndWait(ctx, hostID, 30*time.Second); err != nil {
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "节点已加载密钥，但未能确认最新存储状态；请点击节点测试立即上报后刷新"})
			return
		}
		confirmed, err := storageEncryptionStoredPoolEncryption(ctx, hostID, poolName)
		if err != nil {
			storageEncryptionRespondNodeError(c, err, body)
			return
		}
		if egressJSTruthy(confirmed["encrypted"]) && confirmed["state"] != "unlocked" {
			c.JSON(http.StatusConflict, gin.H{
				"error":  fmt.Sprintf("节点已加载密钥，但存储状态仍为 %v；请刷新后重试或检查节点日志", confirmed["state"]),
				"detail": confirmed,
			})
			return
		}
		c.JSON(http.StatusOK, confirmed)
		return
	}

	StorageEncryptionRequestHeartbeat(hostID)
	c.JSON(http.StatusOK, result.toJSON())
}

// storageEncryptionReveal mirrors POST .../encryption/reveal.
func storageEncryptionReveal(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")

	hostID, poolName, ok := storageEncryptionParams(c)
	if !ok {
		return
	}
	user := middleware.CurrentUser(c)
	ctx := c.Request.Context()

	host, err := db.EgressHostByID(ctx, hostID)
	if err != nil {
		storageEncryptionInternal(c, err)
		return
	}
	if host == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "节点不存在"})
		return
	}
	if user.Role != "admin" && user.ID != host.UserID {
		c.JSON(http.StatusForbidden, gin.H{"error": "无权管理此节点存储"})
		return
	}

	pool, err := storageEncryptionGetPool(ctx, hostID, poolName)
	if err != nil {
		storageEncryptionInternal(c, err)
		return
	}
	if pool == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "存储池不存在"})
		return
	}
	if pool.Driver != "zfs" {
		c.JSON(http.StatusConflict, gin.H{"error": "此存储池未加密，没有可领取的密钥"})
		return
	}
	telemetry, _ := storageEncryptionDecodeObject(pool.Telemetry)
	encryption, _ := storageEncryptionAnyObject(telemetry["encryption"])
	dataset := storageEncryptionStringField(encryption, "encryptionRoot")
	if dataset == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "尚未收到此存储池的加密状态，请等待节点心跳上报后重试"})
		return
	}

	record, err := storageEncryptionFindStoredKey(ctx, hostID, dataset)
	if err != nil {
		storageEncryptionInternal(c, err)
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "此存储池的密钥尚未托管，无法领取"})
		return
	}

	key, err := StorageEncryptionUnseal(record.Ciphertext, StorageEncryptionKeyIdentity{
		HostID: hostID, Dataset: record.Dataset, Version: record.Version,
	}, storageEncryptionSecret())
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "密钥校验失败，请检查控制端主密钥配置"})
		return
	}

	// createLog never throws (it logs internally), so the TypeScript's 503 branch is
	// unreachable in both runtimes.
	userID := int64(user.ID)
	db.CreateLog(ctx, &userID, db.LogModuleSecurity, "storage.key.reveal",
		fmt.Sprintf("节点 #%d 管理员领取存储池 %s 密钥", hostID, poolName), db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"key": key, "fingerprint": record.Fingerprint, "dataset": record.Dataset})
}

// RegisterStorageEncryption mounts the storage-encryption routes (no prefix).
func RegisterStorageEncryption(r *gin.Engine) {
	base := "/api/hosts/:hostId/storage-pools/:poolName/encryption"
	auth := middleware.Authenticate()
	r.GET(base, auth, func(c *gin.Context) { storageEncryptionHandler(c, http.MethodGet) })
	r.POST(base, auth, func(c *gin.Context) { storageEncryptionHandler(c, http.MethodPost) })
	r.PUT(base, auth, func(c *gin.Context) { storageEncryptionHandler(c, http.MethodPut) })
	r.POST(base+"/reveal", auth, storageEncryptionReveal)
}

func init() {
	Register(RegisterStorageEncryption)
}
