// Port of server/src/routes/storage-encryption.ts.
//
// app.ts registers this plugin with no prefix, so the paths live directly under
// /api:
//
//	GET|POST|PUT /api/hosts/:id/storage-pools/:poolName/encryption
//	POST         /api/hosts/:id/storage-pools/:poolName/encryption/reveal
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// ==================== seams (wired to tunnel manager) ====================

// StorageEncryptionRequestHeartbeat mirrors hostTunnelManager.requestHeartbeat.
var StorageEncryptionRequestHeartbeat = func(hostID int32) {
	lib.TunnelManagerInstance().RequestHeartbeat(hostID)
}

// StorageEncryptionRequestHeartbeatAndWait mirrors
// hostTunnelManager.requestHeartbeatAndWait(hostId, timeout).
var StorageEncryptionRequestHeartbeatAndWait = func(ctx context.Context, hostID int32, timeout time.Duration) error {
	return lib.TunnelManagerInstance().RequestHeartbeatAndWait(ctx, hostID, timeout)
}

func storageEncryptionInternal(c *gin.Context, err error) {
	log.Printf("[StorageEncryption] %v", err)
	message := "Internal server error"
	if err != nil && err.Error() != "" {
		message = err.Error()
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": message})
}

var (
	storageEncryptionHostIDPattern   = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)
	storageEncryptionPoolNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
)

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
		c.JSON(http.StatusBadRequest, gin.H{"error": "节点 ID 无效"})
		return 0, "", false
	}
	return int32(parsed), poolName, true
}

func storageEncryptionRespondNodeError(c *gin.Context, err error, body any) {
	var storageError *services.StorageEncryptionError
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
		if key, present := validated.stringValue("key"); present && !services.IsValidStorageEncryptionKey(key) {
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
		state, err := services.StoredPoolEncryption(ctx, hostID, poolName)
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

	result, err := services.WithStorageSession[services.StorageEncryptionPoolState](ctx, hostID, true,
		func(node services.StorageEncryptionNode, vault services.StorageEncryptionKeyVault) (services.StorageEncryptionPoolState, error) {
			if method == http.MethodPut {
				return services.LockPool(ctx, node, poolName, vault, user.ID, hostID)
			}
			return services.UnlockAndBindPool(ctx, node, hostID, poolName, vault, key, false)
		})
	if err != nil {
		storageEncryptionRespondNodeError(c, err, body)
		return
	}

	if method == http.MethodPost {
		if err := StorageEncryptionRequestHeartbeatAndWait(ctx, hostID, 30*time.Second); err != nil {
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "节点已加载密钥，但未能确认最新存储状态；请点击节点测试立即上报后刷新"})
			return
		}
		confirmed, err := services.StoredPoolEncryption(ctx, hostID, poolName)
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
	c.JSON(http.StatusOK, result.ToMap())
}

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

	pool, err := services.GetStoragePoolRecord(ctx, hostID, poolName)
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
	var telemetry map[string]any
	if len(pool.Telemetry) > 0 {
		_ = json.Unmarshal(pool.Telemetry, &telemetry)
	}
	encryption, _ := telemetry["encryption"].(map[string]any)
	dataset, _ := encryption["encryptionRoot"].(string)
	if dataset == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "尚未收到此存储池的加密状态，请等待节点心跳上报后重试"})
		return
	}

	record, err := services.FindStoredStorageKey(ctx, hostID, dataset)
	if err != nil {
		storageEncryptionInternal(c, err)
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "此存储池的密钥尚未托管，无法领取"})
		return
	}

	key, err := services.StorageEncryptionUnseal(record.Ciphertext, services.StorageEncryptionKeyIdentity{
		HostID: hostID, Dataset: record.Dataset, Version: record.Version,
	}, services.StorageEncryptionSecret())
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "密钥校验失败，请检查控制端主密钥配置"})
		return
	}

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
