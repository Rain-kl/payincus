// Port of server/src/lib/storage/{pool-directory,provision,import,delete,resize}.ts
// and the storage-pool CRUD orchestration in server/src/routes/hosts.ts.
package services

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
)

const storageDeletionMarker = "_payincusDeletion"

var (
	poolDirValidCharsPattern = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
	poolDirForbiddenPrefix   = regexp.MustCompile(`^/(dev|proc|sys)(/|$)`)
	zfsPoolNamePattern       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,39}$`)
	storagePoolNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	loopSizePattern          = regexp.MustCompile(`^([1-9][0-9]*)(GiB|TiB)$`)
	incusNotFoundTextPattern = regexp.MustCompile(`(?i)\bnot found\b|不存在|does not exist`)
)

// IsValidPoolDirectory mirrors isValidPoolDirectory(directory).
func IsValidPoolDirectory(directory string) bool {
	if !poolDirValidCharsPattern.MatchString(directory) || len(directory) > 255 || strings.HasSuffix(directory, "/") {
		return false
	}
	for _, seg := range strings.Split(directory, "/")[1:] {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return !poolDirForbiddenPrefix.MatchString(directory)
}

func parseLoopSizeGiB(size string) (int64, bool) {
	m := loopSizePattern.FindStringSubmatch(size)
	if len(m) != 3 {
		return 0, false
	}
	base, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || base < 1 {
		return 0, false
	}
	mult := int64(1)
	if m[2] == "TiB" {
		mult = 1024
	}
	if base > 65536/mult {
		return 0, false
	}
	gib := base * mult
	if gib < 1 || gib > 65536 {
		return 0, false
	}
	return gib, true
}

func storageDatasetWithin(dataset, source string) bool {
	return dataset == source || strings.HasPrefix(dataset, source+"/")
}

func isIncusPoolNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	var incusErr *lib.IncusApiError
	if errors.As(err, &incusErr) && incusErr.StatusCode == http.StatusNotFound {
		return true
	}
	return incusNotFoundTextPattern.MatchString(err.Error())
}

// ==================== Encrypted ZFS Provision (provision.ts) ====================

// EncryptedPoolOptions mirrors EncryptedPoolOptions in provision.ts.
type EncryptedPoolOptions struct {
	Name        string
	Source      string
	Size        string
	Directory   string
	Preallocate bool
	UseLoop     bool
	ZFSPoolName string
	Purpose     string
	Description string
}

// ProvisionEncryptedPool mirrors provisionEncryptedPool(hostId, input).
func ProvisionEncryptedPool(ctx context.Context, hostID int32, input EncryptedPoolOptions) (map[string]any, error) {
	return provisionEncryptedPoolWithSession(ctx, hostID, input, WithStorageSession[map[string]any])
}

func provisionEncryptedPoolWithSession(
	ctx context.Context,
	hostID int32,
	input EncryptedPoolOptions,
	withSession func(context.Context, int32, bool, func(StorageEncryptionNode, StorageEncryptionKeyVault) (map[string]any, error)) (map[string]any, error),
) (map[string]any, error) {
	poolName := strings.TrimSpace(input.ZFSPoolName)
	if poolName == "" {
		poolName = input.Name
	}
	if !zfsPoolNamePattern.MatchString(poolName) || !storagePoolNamePattern.MatchString(input.Name) {
		return nil, NewStorageEncryptionError(400, "存储池名称格式不正确")
	}
	device := strings.TrimSpace(input.Source)
	size := strings.TrimSpace(input.Size)
	if (device != "") == (size != "") || (input.UseLoop && device != "") {
		return nil, NewStorageEncryptionError(400, "请选择一项：专用空磁盘或文件池容量")
	}
	directory := strings.TrimSpace(input.Directory)
	if directory != "" && (device != "" || size == "") {
		return nil, NewStorageEncryptionError(400, "自定义目录仅适用于 Loop 文件池")
	}
	if directory != "" && !IsValidPoolDirectory(directory) {
		return nil, NewStorageEncryptionError(400, "存储目录必须是规范的绝对路径，且不能位于 /dev、/proc、/sys")
	}
	var sizeGiB int64
	if device != "" && (!strings.HasPrefix(device, "/dev/") || strings.Contains(device, "..")) {
		return nil, NewStorageEncryptionError(400, "创建 ZFS 需要 /dev 下的专用空磁盘；已有存储请使用导入")
	}
	if size != "" {
		var ok bool
		sizeGiB, ok = parseLoopSizeGiB(size)
		if !ok {
			if !loopSizePattern.MatchString(size) {
				return nil, NewStorageEncryptionError(400, "文件池容量请使用 GiB 或 TiB")
			}
			return nil, NewStorageEncryptionError(400, "文件池容量必须为 1–65536 GiB")
		}
	}

	purpose := input.Purpose
	if purpose == "" {
		purpose = "instance_data"
	}

	return withSession(ctx, hostID, true, func(node StorageEncryptionNode, vault StorageEncryptionKeyVault) (map[string]any, error) {
		infoRaw, err := node.Request(ctx, http.MethodGet, "/1.0/agent", nil)
		if err != nil {
			return nil, err
		}
		info, _ := storageEncryptionAnyObject(infoRaw)
		extensions, _ := info["api_extensions"].([]any)
		if !storageEncryptionAnyContains(extensions, "incus_storage_encryption_v1") {
			return nil, NewStorageEncryptionError(409, "节点尚不支持自动加密存储创建，请先更新节点集成包")
		}
		if directory != "" && !storageEncryptionAnyContains(extensions, "incus_storage_pool_dir_v1") {
			return nil, NewStorageEncryptionError(409, "节点尚不支持自定义存储目录，请先更新节点集成包")
		}
		if size != "" && !storageEncryptionAnyContains(extensions, "incus_storage_pool_alloc_v1") {
			return nil, NewStorageEncryptionError(409, "节点尚不支持按需分配的文件池，请先更新节点集成包")
		}
		securityRaw, err := node.Request(ctx, http.MethodGet, "/1.0/agent/security", nil)
		if err != nil {
			return nil, err
		}
		security, _ := storageEncryptionAnyObject(securityRaw)
		if security["locked"] != false {
			return nil, NewStorageEncryptionError(423, "节点安全锁定中，不能创建存储池")
		}

		dataset := poolName + "/incus"
		existing, err := vault.Find(ctx, dataset)
		if err != nil {
			return nil, err
		}
		var key string
		version := int32(1)
		if existing != nil {
			if existing.Version != 0 {
				version = existing.Version
			}
			unsealed, unsealErr := StorageEncryptionUnseal(existing.Ciphertext, StorageEncryptionKeyIdentity{
				HostID: hostID, Dataset: dataset, Version: existing.Version,
			}, StorageEncryptionSecret())
			if unsealErr != nil {
				return nil, NewStorageEncryptionError(503, "已有初始化密钥无法读取，请检查控制端主密钥配置")
			}
			key = unsealed
		} else {
			buf := make([]byte, 32)
			if _, err := rand.Read(buf); err != nil {
				return nil, err
			}
			key = hex.EncodeToString(buf)
		}

		fp, err := StorageEncryptionFingerprint(key)
		if err != nil {
			return nil, err
		}
		ciphertext, err := StorageEncryptionSeal(key, StorageEncryptionKeyIdentity{
			HostID: hostID, Dataset: dataset, Version: version,
		}, StorageEncryptionSecret())
		if err != nil {
			return nil, NewStorageEncryptionError(503, "控制端密钥托管未就绪，未创建存储池")
		}
		if err := vault.Save(ctx, StorageEncryptionStoredKey{
			Dataset: dataset, Fingerprint: fp, Ciphertext: ciphertext, Version: version,
		}); err != nil {
			return nil, err
		}

		secretForRedact := key
		defer func() {
			secretForRedact = ""
		}()

		err = func() error {
			pathsRaw, err := node.Request(ctx, http.MethodGet, "/1.0/storage-pools", nil)
			if err != nil {
				return err
			}
			paths, ok := pathsRaw.([]any)
			if !ok {
				return errors.New("invalid storage pool list")
			}
			poolPath := "/1.0/storage-pools/" + url.PathEscape(input.Name)
			if !storageEncryptionAnyContains(paths, poolPath) {
				reqBody := map[string]any{
					"name":        poolName,
					"device":      device,
					"sizeGiB":     sizeGiB,
					"preallocate": size != "" && input.Preallocate,
					"key":         key,
				}
				if directory != "" {
					reqBody["directory"] = directory
				}
				stateRaw, err := node.Request(ctx, http.MethodPost, "/1.0/agent/storage/pool", reqBody)
				if err != nil {
					return err
				}
				stateObj, _ := storageEncryptionAnyObject(stateRaw)
				targets, _ := stateObj["targets"].([]any)
				confirmed := false
				for _, rawT := range targets {
					tObj, _ := storageEncryptionAnyObject(rawT)
					if storageEncryptionStringField(tObj, "dataset") == dataset &&
						storageEncryptionStringField(tObj, "fingerprint") == fp {
						confirmed = true
						break
					}
				}
				if !confirmed {
					return errors.New("node did not confirm dataset identity")
				}
				if _, err := node.Request(ctx, http.MethodPost, "/1.0/storage-pools", map[string]any{
					"name":        input.Name,
					"driver":      "zfs",
					"config":      map[string]any{"source": dataset},
					"description": input.Description,
				}); err != nil {
					return err
				}
			}
			key = ""
			state, err := PoolEncryption(ctx, node, input.Name, vault)
			if err != nil {
				return err
			}
			if !state.Encrypted || state.EncryptionRoot != dataset || state.State != "unlocked" {
				return errors.New("storage identity changed")
			}
			var descPtr *string
			if input.Description != "" {
				descCopy := input.Description
				descPtr = &descCopy
			}
			cfgBytes, err := json.Marshal(map[string]any{"source": dataset})
			if err != nil {
				return err
			}
			return upsertStoragePoolIfMissing(ctx, hostID, input.Name, "zfs", purpose, descPtr, cfgBytes)
		}()
		key = ""
		if err != nil {
			detail := lib.NodeErrorDetail(err, "节点未返回具体错误", map[string]any{"key": secretForRedact})
			status := 409
			var incusErr *lib.IncusApiError
			if errors.As(err, &incusErr) && incusErr.StatusCode != http.StatusUnauthorized && incusErr.StatusCode > 0 {
				status = incusErr.StatusCode
			}
			return nil, NewStorageEncryptionError(status,
				fmt.Sprintf("加密存储初始化未全部完成：%s；密钥已保留，使用相同名称和参数重试可继续，请勿删除已有数据", detail))
		}
		return map[string]any{
			"name":      input.Name,
			"encrypted": true,
			"source":    dataset,
			"keySaved":  true,
			"message":   "加密 ZFS 存储池已创建，密钥已自动保存",
		}, nil
	})
}

// ==================== Encrypted ZFS Import (import.ts) ====================

// EncryptedPoolImportInput mirrors importEncryptedPool input.
type EncryptedPoolImportInput struct {
	Name        string
	Source      string
	Purpose     string
	Description string
}

// ImportEncryptedPool mirrors importEncryptedPool(hostId, input).
// Returns (nil, nil) when the discovered pool/children have no ZFS encryption.
func ImportEncryptedPool(ctx context.Context, hostID int32, input EncryptedPoolImportInput) (map[string]any, error) {
	return importEncryptedPoolWithSession(ctx, hostID, input, WithStorageSession[map[string]any])
}

func importEncryptedPoolWithSession(
	ctx context.Context,
	hostID int32,
	input EncryptedPoolImportInput,
	withSession func(context.Context, int32, bool, func(StorageEncryptionNode, StorageEncryptionKeyVault) (map[string]any, error)) (map[string]any, error),
) (map[string]any, error) {
	if !storagePoolNamePattern.MatchString(input.Name) || (input.Source != "" && !IsValidStorageDataset(input.Source)) {
		return nil, NewStorageEncryptionError(400, "请输入有效的存储池名和 ZFS 数据集名称")
	}
	purpose := input.Purpose
	if purpose == "" {
		purpose = "instance_data"
	}

	return withSession(ctx, hostID, true, func(node StorageEncryptionNode, vault StorageEncryptionKeyVault) (map[string]any, error) {
		infoRaw, err := node.Request(ctx, http.MethodGet, "/1.0/agent", nil)
		if err != nil {
			return nil, err
		}
		info, _ := storageEncryptionAnyObject(infoRaw)
		extensions, _ := info["api_extensions"].([]any)
		if !storageEncryptionAnyContains(extensions, "incus_storage_encryption_v1") {
			return nil, NewStorageEncryptionError(409, "节点尚不支持加密卷导入，请先更新节点集成包")
		}
		pathsRaw, err := node.Request(ctx, http.MethodGet, "/1.0/storage-pools", nil)
		if err != nil {
			return nil, err
		}
		paths, ok := pathsRaw.([]any)
		if !ok {
			return nil, NewStorageEncryptionError(502, "无法读取节点存储列表")
		}
		poolPath := "/1.0/storage-pools/" + url.PathEscape(input.Name)
		requestedSource := input.Source
		if requestedSource == "" {
			requestedSource = input.Name
		}
		if storageEncryptionAnyContains(paths, poolPath) {
			actualRaw, err := node.Request(ctx, http.MethodGet, poolPath, nil)
			if err != nil {
				return nil, err
			}
			actual, _ := storageEncryptionAnyObject(actualRaw)
			config, _ := storageEncryptionAnyObject(actual["config"])
			source := storageEncryptionStringField(config, "zfs.pool_name")
			if source == "" {
				source = storageEncryptionStringField(config, "source")
			}
			if storageEncryptionStringField(actual, "driver") != "zfs" || source == "" || (input.Source != "" && input.Source != source) {
				return nil, NewStorageEncryptionError(409, "已存在的 Incus 存储池与指定来源不一致")
			}
			requestedSource = source
		}

		inventoryRaw, err := node.Request(ctx, http.MethodGet, "/1.0/agent/storage", nil)
		if err != nil {
			return nil, err
		}
		inventory, _ := storageEncryptionAnyObject(inventoryRaw)
		datasets, ok := inventory["datasets"].([]any)
		if !ok {
			return nil, NewStorageEncryptionError(502, "节点返回的加密卷信息无效")
		}

		var item map[string]any
		for _, raw := range datasets {
			d, ok := storageEncryptionAnyObject(raw)
			if ok && storageEncryptionStringField(d, "dataset") == requestedSource {
				item = d
				break
			}
		}
		if item == nil {
			return nil, NewStorageEncryptionError(409, "节点暂未发现此数据集，请确认外部 ZFS 池已导入且在线")
		}

		algorithm := storageEncryptionStringField(item, "algorithm")
		if algorithm == "off" || algorithm == "-" {
			var roots []map[string]any
			prefix := requestedSource + "/"
			for _, raw := range datasets {
				d, ok := storageEncryptionAnyObject(raw)
				if !ok {
					continue
				}
				ds := storageEncryptionStringField(d, "dataset")
				root := storageEncryptionStringField(d, "encryptionRoot")
				alg := storageEncryptionStringField(d, "algorithm")
				if strings.HasPrefix(ds, prefix) && root == ds && alg != "off" && alg != "-" {
					roots = append(roots, d)
				}
			}
			if len(roots) > 1 {
				return nil, NewStorageEncryptionError(400, "此池包含多个加密数据集，请在存储源中指定需要导入的数据集")
			}
			if len(roots) == 0 {
				return nil, nil
			}
			item = roots[0]
		}

		encryptionRoot := storageEncryptionStringField(item, "encryptionRoot")
		itemDataset := storageEncryptionStringField(item, "dataset")
		if !IsValidStorageDataset(encryptionRoot) {
			return nil, NewStorageEncryptionError(502, "节点返回的加密根无效")
		}

		existingRec, existingPending, err := storagePendingRecord(ctx, hostID, input.Name)
		if err != nil {
			return nil, err
		}
		if existingRec != nil && (existingPending == nil || existingPending.Source != itemDataset) {
			return nil, NewStorageEncryptionError(409, "此存储池名称已登记，请从存储列表管理")
		}

		var descPtr *string
		if input.Description != "" {
			descCopy := input.Description
			descPtr = &descCopy
		}
		cfgBytes, err := json.Marshal(map[string]any{
			"source": itemDataset,
			storagePendingImportMarker: map[string]any{
				"source":         itemDataset,
				"encryptionRoot": encryptionRoot,
			},
		})
		if err != nil {
			return nil, err
		}
		if err := upsertStoragePoolIfMissing(ctx, hostID, input.Name, "zfs", purpose, descPtr, cfgBytes); err != nil {
			return nil, err
		}

		stored, err := vault.Find(ctx, encryptionRoot)
		if err != nil {
			return nil, err
		}
		secRaw, err := node.Request(ctx, http.MethodGet, "/1.0/agent/security", nil)
		if err != nil {
			return nil, err
		}
		secObj, _ := storageEncryptionAnyObject(secRaw)
		if stored != nil && secObj["locked"] == false {
			ready, err := UnlockAndBindPool(ctx, node, hostID, input.Name, vault, "", true)
			if err != nil {
				detail := lib.NodeErrorDetail(err, "请检查节点状态", nil)
				return map[string]any{
					"name":          input.Name,
					"imported":      true,
					"pendingImport": true,
					"encrypted":     true,
					"source":        itemDataset,
					"message":       fmt.Sprintf("加密卷已添加，但自动加载或绑定未完成：%s；请在存储页继续处理", detail),
				}, nil
			}
			out := ready.ToMap()
			out["name"] = input.Name
			out["imported"] = true
			out["pendingImport"] = false
			out["message"] = "加密存储已导入，并使用保存的密钥解锁"
			return out, nil
		}

		return map[string]any{
			"name":          input.Name,
			"imported":      true,
			"pendingImport": true,
			"encrypted":     true,
			"source":        itemDataset,
			"message":       "加密卷已添加，请点击“解密磁盘”输入密钥并完成导入",
		}, nil
	})
}

// ==================== Managed ZFS Deletion (delete.ts) ====================

type storageDeletionRoot struct {
	Dataset string `json:"dataset"`
	GUID    string `json:"guid"`
}

type storageDeletionKeyRef struct {
	ID          int32  `json:"id"`
	Dataset     string `json:"dataset"`
	Fingerprint string `json:"fingerprint"`
}

type storageDeletionIntent struct {
	Source string                  `json:"source"`
	Token  string                  `json:"token"`
	Roots  []storageDeletionRoot   `json:"roots"`
	Keys   []storageDeletionKeyRef `json:"keys"`
}

// DeleteManagedZfsPool mirrors deleteManagedZfsPool(hostId, poolName).
func DeleteManagedZfsPool(ctx context.Context, hostID int32, poolName string) (map[string]any, error) {
	return deleteManagedZfsPoolWithSession(ctx, hostID, poolName, WithStorageSession[map[string]any])
}

func deleteManagedZfsPoolWithSession(
	ctx context.Context,
	hostID int32,
	poolName string,
	withSession func(context.Context, int32, bool, func(StorageEncryptionNode, StorageEncryptionKeyVault) (map[string]any, error)) (map[string]any, error),
) (map[string]any, error) {
	return withSession(ctx, hostID, true, func(node StorageEncryptionNode, _ StorageEncryptionKeyVault) (map[string]any, error) {
		infoRaw, err := node.Request(ctx, http.MethodGet, "/1.0/agent", nil)
		if err != nil {
			return nil, err
		}
		info, _ := storageEncryptionAnyObject(infoRaw)
		extensions, _ := info["api_extensions"].([]any)
		if !storageEncryptionAnyContains(extensions, "incus_storage_encryption_v1") {
			return nil, NewStorageEncryptionError(409, "节点尚不支持存储密钥同步清理，请先更新节点集成包")
		}

		record, err := GetStoragePoolRecord(ctx, hostID, poolName)
		if err != nil {
			return nil, err
		}
		var config map[string]any
		if record != nil {
			config, _ = storageDecodeObject(record.Config)
		} else {
			config = map[string]any{}
		}

		var intent *storageDeletionIntent
		if rawIntent, ok := config[storageDeletionMarker]; ok && rawIntent != nil {
			b, err := json.Marshal(rawIntent)
			if err == nil {
				var parsed storageDeletionIntent
				if json.Unmarshal(b, &parsed) == nil {
					intent = &parsed
				}
			}
		}

		pathsRaw, err := node.Request(ctx, http.MethodGet, "/1.0/storage-pools", nil)
		if err != nil {
			return nil, err
		}
		paths, ok := pathsRaw.([]any)
		if !ok {
			return nil, NewStorageEncryptionError(502, "无法核验节点存储池列表")
		}
		poolPath := "/1.0/storage-pools/" + url.PathEscape(poolName)
		exists := storageEncryptionAnyContains(paths, poolPath)

		if !exists && intent == nil {
			if agentStorageJSTruthy(config[storagePendingImportMarker]) {
				if err := deleteStoragePoolRow(ctx, hostID, poolName); err != nil {
					return nil, err
				}
				return map[string]any{"message": "已移除待导入记录；外部数据集及已有托管密钥均已保留"}, nil
			}
			if record == nil {
				return nil, NewStorageEncryptionError(404, "节点存储池不存在，未删除任何托管密钥")
			}

			sourceHint := strings.TrimSpace(storageEncryptionStringField(config, "zfs.pool_name"))
			if sourceHint == "" {
				sourceHint = strings.TrimSpace(storageEncryptionStringField(config, "source"))
			}

			secRaw, _ := node.Request(ctx, http.MethodGet, "/1.0/agent/security", nil)
			storageRaw, _ := node.Request(ctx, http.MethodGet, "/1.0/agent/storage", nil)
			secObj, _ := storageEncryptionAnyObject(secRaw)
			storageObj, _ := storageEncryptionAnyObject(storageRaw)
			secTargets, _ := secObj["targets"].([]any)
			storageDatasets, _ := storageObj["datasets"].([]any)

			stillOnNode := false
			if sourceHint != "" {
				for _, raw := range secTargets {
					t, _ := storageEncryptionAnyObject(raw)
					if ds := storageEncryptionStringField(t, "dataset"); ds != "" && storageDatasetWithin(ds, sourceHint) {
						stillOnNode = true
						break
					}
				}
				if !stillOnNode {
					for _, raw := range storageDatasets {
						d, _ := storageEncryptionAnyObject(raw)
						if ds := storageEncryptionStringField(d, "dataset"); ds != "" && storageDatasetWithin(ds, sourceHint) {
							stillOnNode = true
							break
						}
					}
				}
			}

			if err := cleanupMissingPoolRecords(ctx, hostID, poolName, sourceHint, !stillOnNode); err != nil {
				return nil, err
			}
			msg := "节点上已不存在该存储池，已清理控制端记录"
			if stillOnNode {
				msg = "节点上已不存在该 Incus 存储池，已清理控制端记录并保留节点数据集托管密钥"
			}
			return map[string]any{"missingOnNode": true, "message": msg}, nil
		}

		if intent == nil {
			poolRaw, err := node.Request(ctx, http.MethodGet, poolPath, nil)
			if err != nil {
				return nil, err
			}
			poolObj, _ := storageEncryptionAnyObject(poolRaw)
			poolCfg, _ := storageEncryptionAnyObject(poolObj["config"])
			source := storageEncryptionStringField(poolCfg, "zfs.pool_name")
			if source == "" {
				source = storageEncryptionStringField(poolCfg, "source")
			}
			if storageEncryptionStringField(poolObj, "driver") != "zfs" || !IsValidStorageDataset(source) {
				return nil, NewStorageEncryptionError(409, "无法确定待删除 ZFS 数据集身份，未执行删除")
			}

			stateRaw, err := node.Request(ctx, http.MethodGet, "/1.0/agent/security", nil)
			if err != nil {
				return nil, err
			}
			stateObj, _ := storageEncryptionAnyObject(stateRaw)
			targetsRaw, hasTargets := stateObj["targets"]
			var targets []any
			if targetsRaw != nil {
				arr, ok := targetsRaw.([]any)
				if !ok {
					return nil, NewStorageEncryptionError(502, "无法读取节点存储登记信息")
				}
				targets = arr
			} else if !hasTargets {
				return nil, NewStorageEncryptionError(502, "无法读取节点存储登记信息")
			}

			dbKeys, err := listNodeStorageKeysWithin(ctx, hostID, source)
			if err != nil {
				return nil, err
			}

			var roots []storageDeletionRoot
			seenRoot := map[string]bool{}
			for _, rawT := range targets {
				tObj, _ := storageEncryptionAnyObject(rawT)
				ds := storageEncryptionStringField(tObj, "dataset")
				guid := storageEncryptionStringField(tObj, "guid")
				if ds != "" && storageDatasetWithin(ds, source) {
					roots = append(roots, storageDeletionRoot{Dataset: ds, GUID: guid})
					seenRoot[ds] = true
				}
			}
			for _, k := range dbKeys {
				if !seenRoot[k.Dataset] {
					roots = append(roots, storageDeletionRoot{Dataset: k.Dataset, GUID: ""})
					seenRoot[k.Dataset] = true
				}
			}

			tokBytes := make([]byte, 16)
			if _, err := rand.Read(tokBytes); err != nil {
				return nil, err
			}
			intent = &storageDeletionIntent{
				Source: source,
				Token:  hex.EncodeToString(tokBytes),
				Roots:  roots,
				Keys:   dbKeys,
			}

			purpose := "instance_data"
			if record != nil && record.Purpose != "" {
				purpose = record.Purpose
			}
			checkpointCfg := map[string]any{}
			for k, v := range poolCfg {
				checkpointCfg[k] = v
			}
			checkpointCfg[storageDeletionMarker] = intent
			cfgBytes, err := json.Marshal(checkpointCfg)
			if err != nil {
				return nil, err
			}
			if err := upsertStoragePoolDeletionCheckpoint(ctx, hostID, poolName, "zfs", purpose, cfgBytes); err != nil {
				return nil, err
			}
		}

		if intent.Source == "" || intent.Token == "" {
			return nil, NewStorageEncryptionError(409, "存储删除检查点无效，未清理密钥")
		}
		for _, r := range intent.Roots {
			if !storageDatasetWithin(r.Dataset, intent.Source) {
				return nil, NewStorageEncryptionError(409, "存储删除检查点无效，未清理密钥")
			}
		}
		for _, k := range intent.Keys {
			if !storageDatasetWithin(k.Dataset, intent.Source) {
				return nil, NewStorageEncryptionError(409, "存储删除检查点无效，未清理密钥")
			}
		}

		if exists {
			currentRaw, err := node.Request(ctx, http.MethodGet, poolPath, nil)
			if err != nil {
				return nil, err
			}
			currentObj, _ := storageEncryptionAnyObject(currentRaw)
			currentCfg, _ := storageEncryptionAnyObject(currentObj["config"])
			currentSource := storageEncryptionStringField(currentCfg, "zfs.pool_name")
			if currentSource == "" {
				currentSource = storageEncryptionStringField(currentCfg, "source")
			}
			if currentSource != intent.Source {
				return nil, NewStorageEncryptionError(409, "存储池来源已变化，拒绝继续原删除操作")
			}
			if _, err := node.Request(ctx, http.MethodDelete, poolPath, nil); err != nil {
				return nil, err
			}
		}

		if err := func() error {
			for _, target := range intent.Roots {
				if _, err := node.Request(ctx, http.MethodPost, "/1.0/agent/storage/forget", target); err != nil {
					return err
				}
			}
			return finalizeManagedPoolDeletion(ctx, hostID, poolName, intent)
		}(); err != nil {
			return nil, NewStorageEncryptionError(409, "节点存储已删除，但登记或密钥清理尚未完成；再次删除可继续清理，不会重删磁盘")
		}

		return map[string]any{"message": "存储池已删除，对应密钥与节点登记已同步清理"}, nil
	})
}

// ==================== Pool Resize (resize.ts) ====================

// ResizeStoragePool mirrors resizeStoragePool(node, name, size).
func ResizeStoragePool(ctx context.Context, node StorageEncryptionNode, name, size string) error {
	path := "/1.0/storage-pools/" + url.PathEscape(name)
	poolRaw, err := node.Request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	poolObj, _ := storageEncryptionAnyObject(poolRaw)
	cfg, _ := storageEncryptionAnyObject(poolObj["config"])
	source := storageEncryptionStringField(cfg, "zfs.pool_name")
	if source == "" {
		source = storageEncryptionStringField(cfg, "source")
	}
	cfgSize := storageEncryptionStringField(cfg, "size")
	if storageEncryptionStringField(poolObj, "driver") == "zfs" && strings.HasSuffix(source, "/incus") && cfgSize == "" {
		sizeGiB, ok := parseLoopSizeGiB(size)
		if !ok {
			return NewStorageEncryptionError(400, "文件池容量必须为 1–65536 GiB，请使用 GiB 或 TiB")
		}
		infoRaw, err := node.Request(ctx, http.MethodGet, "/1.0/agent", nil)
		if err != nil {
			return err
		}
		infoObj, _ := storageEncryptionAnyObject(infoRaw)
		extensions, _ := infoObj["api_extensions"].([]any)
		if !storageEncryptionAnyContains(extensions, "incus_storage_pool_resize_v1") {
			return NewStorageEncryptionError(409, "节点尚不支持加密 Loop 存储扩容，请先更新节点集成包")
		}
		_, err = node.Request(ctx, http.MethodPost, "/1.0/agent/storage/resize", map[string]any{
			"dataset": source,
			"sizeGiB": sizeGiB,
		})
		return err
	}
	_, err = node.Request(ctx, http.MethodPatch, path, map[string]any{
		"config": map[string]any{"size": size},
	})
	return err
}

// ==================== High-level Storage Pool CRUD Handlers ====================

// HostStoragePoolCreateInput mirrors the resolved create/import parameters.
type HostStoragePoolCreateInput struct {
	HostID          int32
	UserID          int32
	HostName        string
	Name            string
	Driver          string
	Source          string
	Size            string
	LoopDirectory   string
	Preallocate     bool
	UseLoop         bool
	ZFSPoolName     string
	LVMVGName       string
	LVMThinpoolName string
	LVMUseThinpool  *bool
	Description     *string
	Purpose         string
	UseExisting     bool
	ExistingSource  string
}

// CreateHostStoragePool executes POST /:hostId/storage-pools.
func CreateHostStoragePool(ctx context.Context, host *db.HostsRecord, input HostStoragePoolCreateInput) (any, int, error) {
	desc := ""
	if input.Description != nil {
		desc = *input.Description
	}
	purpose := input.Purpose
	if purpose == "" {
		purpose = "instance_data"
	}

	if input.Driver == "zfs" && !input.UseExisting {
		created, err := ProvisionEncryptedPool(ctx, input.HostID, EncryptedPoolOptions{
			Name:        input.Name,
			Source:      input.Source,
			Size:        input.Size,
			Directory:   input.LoopDirectory,
			Preallocate: input.Preallocate,
			UseLoop:     input.UseLoop,
			ZFSPoolName: input.ZFSPoolName,
			Purpose:     purpose,
			Description: desc,
		})
		if err != nil {
			return nil, 0, err
		}
		uid := int64(input.UserID)
		db.CreateLog(ctx, &uid, db.LogModuleHost, "storage.create",
			fmt.Sprintf("Created encrypted ZFS storage on host #%d", input.HostID), db.LogResultSuccess, db.CreateLogOptions{})
		return created, http.StatusCreated, nil
	}

	if input.Driver == "zfs" && input.UseExisting {
		imported, err := ImportEncryptedPool(ctx, input.HostID, EncryptedPoolImportInput{
			Name:        input.Name,
			Source:      input.ExistingSource,
			Purpose:     purpose,
			Description: desc,
		})
		if err != nil {
			return nil, 0, err
		}
		if imported != nil {
			return imported, http.StatusCreated, nil
		}
	}

	client, err := lib.GetIncusClientForHost(ctx, host)
	if err != nil {
		return nil, 0, err
	}
	defer client.Close()

	if input.UseExisting {
		existingRec, err := GetStoragePoolRecord(ctx, input.HostID, input.Name)
		if err != nil {
			return nil, 0, err
		}
		if existingRec != nil {
			return map[string]any{"error": fmt.Sprintf("存储池 %q 已在系统中关联", input.Name)}, http.StatusBadRequest, nil
		}

		poolInfo, err := lib.IncusStorageGetPool(ctx, client, input.Name)
		if err == nil && poolInfo != nil {
			linkedDesc := input.Description
			if linkedDesc == nil && poolInfo.Description != "" {
				d := poolInfo.Description
				linkedDesc = &d
			}
			cfg := poolInfo.Config
			if cfg == nil {
				cfg = map[string]string{}
			}
			cfgBytes, err := json.Marshal(cfg)
			if err != nil {
				return nil, 0, err
			}
			if err := insertStoragePoolRow(ctx, input.HostID, input.Name, poolInfo.Driver, purpose, linkedDesc, cfgBytes); err != nil {
				return nil, 0, err
			}
			uid := int64(input.UserID)
			db.CreateLog(ctx, &uid, db.LogModuleHost, "storage.link",
				fmt.Sprintf("Linked existing storage pool %q (%s) on host %q", input.Name, poolInfo.Driver, host.Name),
				db.LogResultSuccess, db.CreateLogOptions{})
			_, _ = SyncHostStorageBaseline(ctx, input.HostID, client)
			return map[string]any{"message": "存储池关联成功", "name": input.Name, "driver": poolInfo.Driver}, http.StatusCreated, nil
		}

		if input.Driver == "" {
			return map[string]any{
				"error": fmt.Sprintf("存储池 %q 在 Incus 中不存在。如需导入底层已有的存储池，请选择驱动类型并提供存储源名称。", input.Name),
			}, http.StatusBadRequest, nil
		}
		if (input.Driver == "btrfs" || input.Driver == "dir") && input.ExistingSource == "" {
			return map[string]any{
				"error": fmt.Sprintf("%s 类型必须提供底层存储源路径", strings.ToUpper(input.Driver)),
			}, http.StatusBadRequest, nil
		}

		importConfig := map[string]string{}
		sourceValue := input.ExistingSource
		if sourceValue == "" {
			sourceValue = input.Name
		}
		importConfig["source"] = sourceValue
		if input.Driver == "lvm" {
			importConfig["lvm.use_thinpool"] = "false"
		}

		if err := lib.IncusStorageCreatePool(ctx, client, lib.IncusStorageCreatePoolOptions{
			Name:   input.Name,
			Driver: input.Driver,
			Config: importConfig,
		}); err != nil {
			return nil, 0, err
		}
		importedInfo, _ := lib.IncusStorageGetPool(ctx, client, input.Name)
		persistedCfg := importConfig
		if importedInfo != nil && len(importedInfo.Config) > 0 {
			persistedCfg = importedInfo.Config
		}
		cfgBytes, err := json.Marshal(persistedCfg)
		if err != nil {
			return nil, 0, err
		}
		if err := insertStoragePoolRow(ctx, input.HostID, input.Name, input.Driver, purpose, input.Description, cfgBytes); err != nil {
			return nil, 0, err
		}
		uid := int64(input.UserID)
		db.CreateLog(ctx, &uid, db.LogModuleHost, "storage.import",
			fmt.Sprintf("Imported existing %s storage %q as pool %q on host %q", strings.ToUpper(input.Driver), sourceValue, input.Name, host.Name),
			db.LogResultSuccess, db.CreateLogOptions{})
		_, _ = SyncHostStorageBaseline(ctx, input.HostID, client)
		return map[string]any{
			"message":  "存储池导入成功",
			"name":     input.Name,
			"driver":   input.Driver,
			"imported": true,
		}, http.StatusCreated, nil
	}

	if input.Driver == "" {
		return map[string]any{"error": "创建新存储池需要指定驱动类型"}, http.StatusBadRequest, nil
	}
	trimmedSource := strings.TrimSpace(input.Source)
	trimmedSize := strings.TrimSpace(input.Size)
	supportsLoop := input.Driver == "zfs" || input.Driver == "lvm" || input.Driver == "btrfs"
	if supportsLoop && trimmedSource != "" && trimmedSize != "" {
		return map[string]any{"error": "不能同时指定 source 和 size，请明确选择物理路径或 Loop 文件模式"}, http.StatusBadRequest, nil
	}
	loopMode := supportsLoop && (input.UseLoop || (trimmedSource == "" && trimmedSize != ""))
	storageSource, storageSize := trimmedSource, ""
	if loopMode {
		storageSource, storageSize = "", trimmedSize
	}

	config := map[string]string{}
	switch input.Driver {
	case "lvm":
		if storageSource != "" {
			config["source"] = storageSource
		} else if storageSize != "" {
			config["size"] = storageSize
		} else {
			return map[string]any{"error": "LVM 需要指定 source（物理盘）或 size（Loop 文件）"}, http.StatusBadRequest, nil
		}
		if input.LVMVGName != "" {
			config["lvm.vg_name"] = input.LVMVGName
		}
		if input.LVMThinpoolName != "" {
			config["lvm.thinpool_name"] = input.LVMThinpoolName
		}
		if input.LVMUseThinpool == nil || *input.LVMUseThinpool {
			config["lvm.use_thinpool"] = "true"
		} else {
			config["lvm.use_thinpool"] = "false"
		}
	case "btrfs":
		if storageSource != "" {
			config["source"] = storageSource
		} else if storageSize != "" {
			config["size"] = storageSize
		} else {
			return map[string]any{"error": "Btrfs 需要指定 source（物理盘）或 size（Loop 文件）"}, http.StatusBadRequest, nil
		}
	case "dir":
		if storageSource != "" && storageSource != "/var/lib/incus/storage-pools/"+input.Name {
			config["source"] = storageSource
		}
	}

	if err := lib.IncusStorageCreatePool(ctx, client, lib.IncusStorageCreatePoolOptions{
		Name:   input.Name,
		Driver: input.Driver,
		Config: config,
	}); err != nil {
		return nil, 0, err
	}
	actual, err := lib.IncusStorageGetPool(ctx, client, input.Name)
	persistedCfg := config
	if err == nil && actual != nil && actual.Config != nil {
		persistedCfg = actual.Config
	}
	cfgBytes, err := json.Marshal(persistedCfg)
	if err != nil {
		return nil, 0, err
	}
	if err := insertStoragePoolRow(ctx, input.HostID, input.Name, input.Driver, purpose, input.Description, cfgBytes); err != nil {
		return nil, 0, err
	}
	uid := int64(input.UserID)
	db.CreateLog(ctx, &uid, db.LogModuleHost, "storage.create",
		fmt.Sprintf("Created %s storage pool %q on host %q", strings.ToUpper(input.Driver), input.Name, host.Name),
		db.LogResultSuccess, db.CreateLogOptions{})
	_, _ = SyncHostStorageBaseline(ctx, input.HostID, client)
	return map[string]any{
		"message": "存储池创建成功",
		"name":    input.Name,
		"driver":  input.Driver,
	}, http.StatusCreated, nil
}

// DeleteHostStoragePool executes DELETE /:hostId/storage-pools/:poolName.
func DeleteHostStoragePool(ctx context.Context, host *db.HostsRecord, poolName string) (map[string]any, error) {
	recorded, err := GetStoragePoolRecord(ctx, host.ID, poolName)
	if err != nil {
		return nil, err
	}
	encryptedDeletion := recorded != nil && recorded.Driver == "zfs"
	missingOnNode := false
	message := "存储池删除成功"

	if !encryptedDeletion {
		client, err := lib.GetIncusClientForHost(ctx, host)
		if err != nil {
			return nil, err
		}
		defer client.Close()

		actual, err := lib.IncusStorageGetPool(ctx, client, poolName)
		if err != nil {
			if isIncusPoolNotFoundErr(err) && recorded != nil {
				missingOnNode = true
				message = "节点上已不存在该存储池，已从控制端移除记录"
			} else {
				return nil, err
			}
		} else if actual != nil {
			encryptedDeletion = actual.Driver == "zfs"
			if !encryptedDeletion {
				if err := lib.IncusStorageDeletePool(ctx, client, poolName); err != nil {
					if isIncusPoolNotFoundErr(err) && recorded != nil {
						missingOnNode = true
						message = "节点上已不存在该存储池，已从控制端移除记录"
					} else {
						return nil, err
					}
				}
			}
		}
		if !encryptedDeletion {
			if err := deleteStoragePoolRow(ctx, host.ID, poolName); err != nil {
				return nil, err
			}
			return map[string]any{"message": message, "missingOnNode": missingOnNode}, nil
		}
	}

	deleted, err := DeleteManagedZfsPool(ctx, host.ID, poolName)
	if err != nil {
		return nil, err
	}
	if m, ok := deleted["message"].(string); ok && m != "" {
		message = m
	}
	missingOnNode = deleted["missingOnNode"] == true
	return map[string]any{"message": message, "missingOnNode": missingOnNode}, nil
}

// UpdateHostStoragePool executes PATCH /:hostId/storage-pools/:poolName.
func UpdateHostStoragePool(
	ctx context.Context,
	host *db.HostsRecord,
	poolName string,
	size, description, purpose *string,
) error {
	pool, err := GetStoragePoolRecord(ctx, host.ID, poolName)
	if err != nil {
		return err
	}
	if pool == nil {
		return NewStorageEncryptionError(404, "存储池不存在")
	}

	var syncClient *lib.IncusClient
	if (size != nil && *size != "") || description != nil {
		client, err := lib.GetIncusClientForHost(ctx, host)
		if err != nil {
			return err
		}
		defer client.Close()
		syncClient = client

		if size != nil && *size != "" {
			if err := ResizeStoragePool(ctx, storageEncryptionIncusNode{client: client}, poolName, *size); err != nil {
				return err
			}
		}
		if description != nil {
			if err := lib.IncusStorageUpdatePool(ctx, client, poolName, lib.IncusStorageUpdatePoolOptions{
				Description: description,
			}); err != nil {
				return err
			}
		}
	}

	if description != nil || purpose != nil {
		if err := updateStoragePoolMeta(ctx, host.ID, poolName, description, purpose); err != nil {
			return err
		}
	}

	if size != nil && *size != "" && syncClient != nil {
		_, _ = SyncHostStorageBaseline(ctx, host.ID, syncClient)
	}
	return nil
}

// ==================== Storage Pool SQL Helpers ====================

func insertStoragePoolRow(ctx context.Context, hostID int32, name, driver, purpose string, description *string, config []byte) error {
	if db.Pool() == nil {
		return errors.New("database not initialized")
	}
	_, err := db.Pool().Exec(ctx, `INSERT INTO storage_pools (host_id, name, driver, purpose, description, config, created_at, updated_at)
		VALUES ($1, $2, $3, $4::"StoragePurpose", $5, $6::jsonb, NOW(), NOW())`,
		hostID, name, driver, purpose, description, string(config))
	return err
}

func upsertStoragePoolIfMissing(ctx context.Context, hostID int32, name, driver, purpose string, description *string, config []byte) error {
	if db.Pool() == nil {
		return errors.New("database not initialized")
	}
	_, err := db.Pool().Exec(ctx, `INSERT INTO storage_pools (host_id, name, driver, purpose, description, config, created_at, updated_at)
		VALUES ($1, $2, $3, $4::"StoragePurpose", $5, $6::jsonb, NOW(), NOW())
		ON CONFLICT (host_id, name) DO NOTHING`,
		hostID, name, driver, purpose, description, string(config))
	return err
}

func upsertStoragePoolDeletionCheckpoint(ctx context.Context, hostID int32, name, driver, purpose string, config []byte) error {
	if db.Pool() == nil {
		return errors.New("database not initialized")
	}
	_, err := db.Pool().Exec(ctx, `INSERT INTO storage_pools (host_id, name, driver, purpose, config, created_at, updated_at)
		VALUES ($1, $2, $3, $4::"StoragePurpose", $5::jsonb, NOW(), NOW())
		ON CONFLICT (host_id, name) DO UPDATE SET config = EXCLUDED.config, updated_at = NOW()`,
		hostID, name, driver, purpose, string(config))
	return err
}

func updateStoragePoolMeta(ctx context.Context, hostID int32, name string, description, purpose *string) error {
	if db.Pool() == nil {
		return errors.New("database not initialized")
	}
	if description != nil && purpose != nil {
		_, err := db.Pool().Exec(ctx, `UPDATE storage_pools SET description = $3, purpose = $4::"StoragePurpose", updated_at = NOW()
			WHERE host_id = $1 AND name = $2`, hostID, name, *description, *purpose)
		return err
	}
	if description != nil {
		_, err := db.Pool().Exec(ctx, `UPDATE storage_pools SET description = $3, updated_at = NOW()
			WHERE host_id = $1 AND name = $2`, hostID, name, *description)
		return err
	}
	if purpose != nil {
		_, err := db.Pool().Exec(ctx, `UPDATE storage_pools SET purpose = $3::"StoragePurpose", updated_at = NOW()
			WHERE host_id = $1 AND name = $2`, hostID, name, *purpose)
		return err
	}
	return nil
}

func deleteStoragePoolRow(ctx context.Context, hostID int32, name string) error {
	if db.Pool() == nil {
		return errors.New("database not initialized")
	}
	_, err := db.Pool().Exec(ctx, `DELETE FROM storage_pools WHERE host_id = $1 AND name = $2`, hostID, name)
	return err
}

func listNodeStorageKeysWithin(ctx context.Context, hostID int32, source string) ([]storageDeletionKeyRef, error) {
	if db.Pool() == nil {
		return nil, errors.New("database not initialized")
	}
	rows, err := db.Pool().Query(ctx, `SELECT id, dataset, fingerprint FROM node_storage_keys
		WHERE host_id = $1 AND (dataset = $2 OR dataset LIKE $3)`,
		hostID, source, source+"/%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []storageDeletionKeyRef
	for rows.Next() {
		var k storageDeletionKeyRef
		if err := rows.Scan(&k.ID, &k.Dataset, &k.Fingerprint); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func cleanupMissingPoolRecords(ctx context.Context, hostID int32, poolName, sourceHint string, deleteKeys bool) error {
	if db.Pool() == nil {
		return errors.New("database not initialized")
	}
	tx, err := db.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if sourceHint != "" && deleteKeys {
		if _, err := tx.Exec(ctx, `DELETE FROM node_storage_keys
			WHERE host_id = $1 AND (dataset = $2 OR dataset LIKE $3)`,
			hostID, sourceHint, sourceHint+"/%"); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM storage_pools WHERE host_id = $1 AND name = $2`, hostID, poolName); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func finalizeManagedPoolDeletion(ctx context.Context, hostID int32, poolName string, intent *storageDeletionIntent) error {
	if db.Pool() == nil {
		return errors.New("database not initialized")
	}
	tx, err := db.Pool().BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, key := range intent.Keys {
		var curHostID int32
		var curDataset, curFingerprint string
		err := tx.QueryRow(ctx, `SELECT host_id, dataset, fingerprint FROM node_storage_keys WHERE id = $1`, key.ID).
			Scan(&curHostID, &curDataset, &curFingerprint)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return err
		}
		if curHostID != hostID || curDataset != key.Dataset || curFingerprint != key.Fingerprint {
			return errors.New("key identity changed")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM node_storage_keys WHERE id = $1`, key.ID); err != nil {
			return err
		}
	}

	var cfgRaw []byte
	err = tx.QueryRow(ctx, `SELECT config FROM storage_pools WHERE host_id = $1 AND name = $2`, hostID, poolName).
		Scan(&cfgRaw)
	if err != nil {
		return err
	}
	cfgObj, _ := storageDecodeObject(cfgRaw)
	markerObj, _ := storageEncryptionAnyObject(cfgObj[storageDeletionMarker])
	if storageEncryptionStringField(markerObj, "token") != intent.Token {
		return errors.New("deletion checkpoint changed")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM storage_pools WHERE host_id = $1 AND name = $2`, hostID, poolName); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
