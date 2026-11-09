// Port of server/src/routes/checkin.ts.
//
// app.ts registers this module as
// `fastify.register(checkinRoutes, { prefix: '/api/checkin' })`, so the six paths
// below are absolute under /api/checkin. Every route carries
// `onRequest: [fastify.authenticateUser]`; the Gin group installs
// middleware.AuthenticateUser(), and app.go parses the body after auth (Fastify's
// order), so `decodeJSONRequestBody` runs inside the handler exactly like the
// other ports.
//
// Two high-risk flows live here:
//
//   - POST /checkin: the daily point claim. Concurrency is enforced inside
//     db/checkin.go by pg_advisory_xact_lock(USER_POINTS_LOCK_NAMESPACE = 4111,
//     userId) plus the (user_id, date_key) unique index. Points are integers.
//   - POST /redeem: a system h-code consumption that patches Incus and then
//     records the usage atomically. It is wrapped in the same two
//     *table-level* distributed locks as the TypeScript
//     ("redeem-code:<id>" and, for batch codes, "redeem-batch:<batch>:user:<uid>",
//     120 s expiry / 10 s wait / 100 ms retry, released in reverse order) so the
//     external Incus PATCH cannot race another redemption of the same code.
//     Inside, db.ApplySystemRedeemCodeToInstance holds
//     REDEEM_CODE_LOCK_NAMESPACE (4104) + INSTANCE_OPERATION_LOCK_NAMESPACE (4106).
//
// No route in this file declares `prefixTrailingSlash`, and none has a '/' path,
// so Fastify's 'both' default adds no alias here — unlike /api/resource-pool.
package routes

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// checkinCodeTypeNames / checkinCodeTypeUnits mirror CODE_TYPE_NAMES /
// CODE_TYPE_UNITS. Only `en` is used by the success log; the zh labels are kept so
// the table stays a faithful copy of the TypeScript.
var (
	checkinCodeTypeNames = map[string]struct{ zh, en string }{
		"c": {zh: "CPU", en: "CPU"},
		"r": {zh: "内存", en: "Memory"},
		"d": {zh: "硬盘", en: "Disk"},
		"t": {zh: "流量", en: "Traffic"},
	}
	checkinCodeTypeUnits = map[string]string{
		"c": "%",
		"r": "MB",
		"d": "MB",
		"t": "GB",
	}
	// checkinRedeemResourceTypes mirrors `!['c','r','d','t'].includes(codeType)`.
	checkinRedeemResourceTypes = []string{"c", "r", "d", "t"}
)

// Distributed-lock timings of withSystemRedeemLocks.
const (
	checkinRedeemLockExpireMs        = 120_000
	checkinRedeemLockWaitTimeoutMs   = 10_000
	checkinRedeemLockRetryIntervalMs = 100
)

// ==================== response shapes ====================

// checkinClaimResponse mirrors the POST /checkin return object.
type checkinClaimResponse struct {
	Message        string `json:"message"`
	CodeType       string `json:"codeType"`
	CodeValue      int32  `json:"codeValue"`
	ToResourcePool bool   `json:"toResourcePool"`
	Points         int32  `json:"points"`
	BonusPoints    int32  `json:"bonusPoints"`
	CurrentPoints  int32  `json:"currentPoints"`
	DateKey        string `json:"dateKey"`
	StreakDays     int32  `json:"streakDays"`
}

// checkinRedeemResponse mirrors the POST /redeem return object.
type checkinRedeemResponse struct {
	Message        string `json:"message"`
	CodeType       string `json:"codeType"`
	CodeValue      int32  `json:"codeValue"`
	ActualAdded    int32  `json:"actualAdded"`
	InstanceID     int64  `json:"instanceId"`
	InstanceName   string `json:"instanceName"`
	IsSystemCode   bool   `json:"isSystemCode"`
	ToResourcePool bool   `json:"toResourcePool"`
}

// checkinRedeemBusyBody mirrors the hand-written 409 body of REDEEM_CODE_BUSY
// (it is NOT the apiError envelope).
type checkinRedeemBusyBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// ==================== helpers ====================

// checkinError builds an error whose Error() is exactly the string the TypeScript
// throws (`throw new Error('...')`), so the shared message switch below works for
// both route-local and db-layer failures.
func checkinError(message string) error {
	return errors.New(message)
}

func checkinIntPtr(value int) *int { return &value }

// checkinLog mirrors createLog(userID, 'checkin', ...).
func checkinLog(ctx context.Context, userID int32, action, content, result string, options db.CreateLogOptions) {
	logUserID := int64(userID)
	db.CreateLog(ctx, &logUserID, "checkin", action, content, result, options)
}

// checkinRedeemLockKeys mirrors the lockKeys array of withSystemRedeemLocks: the
// code itself, plus the per-user batch guard when the code belongs to a batch.
func checkinRedeemLockKeys(redeemCodeID int64, userID int32, batchID *string) []string {
	lockKeys := []string{fmt.Sprintf("redeem-code:%d", redeemCodeID)}
	if batchID != nil && *batchID != "" {
		lockKeys = append(lockKeys, fmt.Sprintf("redeem-batch:%s:user:%d", *batchID, userID))
	}
	return lockKeys
}

// checkinWithRedeemLocks mirrors withSystemRedeemLocks: acquire every lock key in
// order (fail fast with REDEEM_CODE_BUSY), run fn, then release in reverse order.
func checkinWithRedeemLocks[T any](
	ctx context.Context,
	redeemCodeID int64,
	userID int32,
	batchID *string,
	fn func() (T, error),
) (T, error) {
	lockKeys := checkinRedeemLockKeys(redeemCodeID, userID, batchID)

	type heldLock struct {
		key     string
		ownerID string
	}
	acquired := make([]heldLock, 0, len(lockKeys))

	defer func() {
		for index := len(acquired) - 1; index >= 0; index-- {
			lib.ReleaseLock(ctx, acquired[index].key, acquired[index].ownerID)
		}
	}()

	for _, lockKey := range lockKeys {
		result, err := lib.AcquireLock(ctx, lockKey, lib.LockOptions{
			ExpireMs:        checkinIntPtr(checkinRedeemLockExpireMs),
			WaitTimeoutMs:   checkinIntPtr(checkinRedeemLockWaitTimeoutMs),
			RetryIntervalMs: checkinIntPtr(checkinRedeemLockRetryIntervalMs),
		})
		if err != nil {
			var zero T
			return zero, err
		}
		if !result.Success || result.OwnerID == "" {
			var zero T
			return zero, checkinError("REDEEM_CODE_BUSY")
		}
		acquired = append(acquired, heldLock{key: lockKey, ownerID: result.OwnerID})
	}

	return fn()
}

// timeNow is a seam for tests (the TypeScript compares against `new Date()`).
var timeNow = func() time.Time { return time.Now() }

// checkinRedeemTypeLabel mirrors `CODE_TYPE_NAMES[codeType]?.en || codeType` and
// `CODE_TYPE_UNITS[codeType] || ”`.
func checkinRedeemTypeLabel(codeType string) (string, string) {
	names, ok := checkinCodeTypeNames[codeType]
	if !ok {
		return codeType, ""
	}
	return names.en, checkinCodeTypeUnits[codeType]
}

// ==================== AJV schema ====================

var checkinRedeemBodyFields = []ajvFieldSpec{
	{Name: "redeemCode", Kind: ajvKindString, MinLength: 5, MaxLength: 30, Minimum: math.NaN(), Maximum: math.NaN()},
	{Name: "instanceId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
}

// validateCheckinRedeemBodyAJV mirrors the /redeem body schema
// (`required: ['redeemCode','instanceId']`).
//
// There is deliberately no root-array unwrap: measured against fastify@5.6.2, a
// single-element root array (`[{"redeemCode":"h-12345","instanceId":1}]`) fails
// with "must be object". AJV's coerceTypes:'array' unwrap applies to *properties*,
// not to the root document.
func validateCheckinRedeemBodyAJV(decoded any) (ajvValidated, string) {
	return validateAJVObject(decoded, []string{"redeemCode", "instanceId"}, checkinRedeemBodyFields)
}

// ==================== routes ====================

// RegisterCheckin mounts the check-in routes under /api/checkin.
func RegisterCheckin(r *gin.Engine) {
	group := r.Group("/api/checkin", middleware.AuthenticateUser())

	// ==================== 获取签到状态 ====================
	group.GET("/status", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		status, err := db.CheckinGetStatus(c.Request.Context(), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, status)
	})

	// ==================== 执行签到 ====================
	group.POST("/checkin", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		clientIP := middleware.ClientIP(c)
		var userAgent *string
		if header := c.GetHeader("User-Agent"); header != "" {
			userAgent = &header
		}

		result, err := db.CheckinPerformDailyPointsCheckin(ctx, user.ID, db.CheckinMeta{
			IPAddress: &clientIP,
			UserAgent: userAgent,
		})
		if err != nil {
			checkinRespondClaimError(c, user.ID, err)
			return
		}

		checkinLog(ctx, user.ID, "checkin.success",
			"Daily check-in successful. Points +"+db.CheckinFormatInt(int(result.Points))+
				", streak: "+db.CheckinFormatInt(int(result.StreakDays)),
			"success", db.CreateLogOptions{})

		c.JSON(http.StatusOK, checkinClaimResponse{
			Message:        "Check-in successful",
			CodeType:       "p",
			CodeValue:      result.Points,
			ToResourcePool: false,
			Points:         result.Points,
			BonusPoints:    result.Points,
			CurrentPoints:  result.CurrentPoints,
			DateKey:        result.DateKey,
			StreakDays:     result.StreakDays,
		})
	})

	// ==================== 兑换兑换码 ====================
	// 仅支持系统码（h-前缀）：必须指定 instanceId，直接应用到实例
	group.POST("/redeem", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		decoded, status, parseMessage := decodeJSONRequestBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": parseMessage})
			return
		}
		validated, validationMessage := validateCheckinRedeemBodyAJV(decoded.Value)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		redeemCode, _ := validated.stringValue("redeemCode")
		instanceIDNumber, _ := validated.Values["instanceId"].(float64)
		instanceID := int64(instanceIDNumber)
		trimmedCode := strings.TrimSpace(redeemCode)

		// 仅支持系统码（h-前缀）
		if !db.IsSystemCode(trimmedCode) {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeInvalidFormat, "")
			return
		}

		systemCodeRecord, err := db.GetRedeemCodeByCode(ctx, trimmedCode)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if systemCodeRecord == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrRedeemCodeNotFound, "")
			return
		}

		// 检查兑换码是否启用
		if !systemCodeRecord.Enabled {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeDisabled, "")
			return
		}
		if systemCodeRecord.TargetUserID != nil && *systemCodeRecord.TargetUserID != user.ID {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "This redeem code is assigned to another user")
			return
		}
		// 检查兑换码是否过期
		if systemCodeRecord.ExpiresAt != nil && systemCodeRecord.ExpiresAt.Before(timeNow()) {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeExpired, "")
			return
		}
		// 检查兑换码是否已达到最大使用次数
		if systemCodeRecord.UsedCount >= systemCodeRecord.MaxUses {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeExhausted, "")
			return
		}
		// 检查用户是否已使用过该兑换码
		hasUsed, err := db.HasUserUsedCode(ctx, int64(systemCodeRecord.ID), int64(user.ID))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if hasUsed {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeAlreadyUsedByUser, "")
			return
		}
		// 检查用户是否已使用过同批次的其他兑换码
		if systemCodeRecord.BatchID != nil && *systemCodeRecord.BatchID != "" {
			hasUsedBatch, err := db.HasUserUsedBatch(ctx, *systemCodeRecord.BatchID, int64(user.ID))
			if err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
				return
			}
			if hasUsedBatch {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeBatchLimit, "")
				return
			}
		}

		// 获取目标实例
		instance, err := db.CheckinGetInstanceByID(ctx, instanceID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if instance == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
			return
		}
		// 检查实例是否属于当前用户
		if int64(instance.UserID) != int64(user.ID) {
			lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
			return
		}
		// 检查实例状态
		if instance.Status != "running" && instance.Status != "stopped" {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceStatusInvalid, "")
			return
		}
		// 检查实例是否属于该宿主机
		if instance.HostID != systemCodeRecord.HostID {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeHostMismatch, "")
			return
		}

		// 不再检查套餐上限，允许资源无限叠加

		response, err := checkinWithRedeemLocks(ctx, int64(systemCodeRecord.ID), user.ID, systemCodeRecord.BatchID,
			func() (checkinRedeemResponse, error) {
				return checkinApplySystemRedeem(ctx, user, instanceID, trimmedCode)
			})
		if err != nil {
			checkinRespondRedeemError(c, user.ID, instanceID, trimmedCode, err)
			return
		}
		c.JSON(http.StatusOK, response)
	})

	// ==================== 获取用户可用实例列表 ====================
	group.GET("/instances", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		instances, err := db.CheckinGetUserInstancesForRedeem(c.Request.Context(), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"instances": instances})
	})

	// ==================== 获取签到记录 ====================
	group.GET("/records", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		query := c.Request.URL.Query()

		limit, message := redeemCodeQueryInteger(query, "limit", 20, 1, 100)
		if message != "" {
			respondAJVValidation(c, message)
			return
		}
		offset, message := redeemCodeQueryInteger(query, "offset", 0, 0, math.NaN())
		if message != "" {
			respondAJVValidation(c, message)
			return
		}

		result, err := db.CheckinGetRecords(c.Request.Context(), user.ID, limit, offset)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, result)
	})

	// ==================== 获取兑换记录 ====================
	group.GET("/redeems", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		query := c.Request.URL.Query()

		limit, message := redeemCodeQueryInteger(query, "limit", 20, 1, 100)
		if message != "" {
			respondAJVValidation(c, message)
			return
		}
		offset, message := redeemCodeQueryInteger(query, "offset", 0, 0, math.NaN())
		if message != "" {
			respondAJVValidation(c, message)
			return
		}

		result, err := db.CheckinGetRedeemRecords(c.Request.Context(), user.ID, limit, offset)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, result)
	})
}

// ==================== handlers ====================

// checkinRespondClaimError mirrors the catch block of POST /checkin.
func checkinRespondClaimError(c *gin.Context, userID int32, err error) {
	ctx := c.Request.Context()
	message := err.Error()

	switch message {
	case "CHECKIN_DISABLED":
		checkinLog(ctx, userID, "checkin.disabled", "Daily check-in is disabled", "failed", db.CreateLogOptions{})
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrFeatureDisabled, "签到功能暂未开放")
		return
	case "CHECKIN_ALREADY_TODAY":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrCheckinAlreadyToday, "")
		return
	case "CHECKIN_NO_INSTANCE":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrCheckinNoInstance, "")
		return
	case "CHECKIN_USER_BLOCKED":
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "账号状态异常，无法签到")
		return
	}

	checkinLog(ctx, userID, "checkin.failed", "Daily check-in failed: "+message, "failed", db.CreateLogOptions{})
	lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, message)
}

// checkinApplySystemRedeem mirrors the closure passed to withSystemRedeemLocks: it
// re-reads the code and the instance under the lock, patches Incus (with rollback
// on a DB failure) and records the consumption.
func checkinApplySystemRedeem(
	ctx context.Context,
	user *lib.AccessTokenClaims,
	instanceID int64,
	trimmedCode string,
) (checkinRedeemResponse, error) {
	// The unlocked pre-checks are advisory only; re-read everything under the lock.
	currentCodeRecord, err := db.GetRedeemCodeByCode(ctx, trimmedCode)
	if err != nil {
		return checkinRedeemResponse{}, err
	}
	if currentCodeRecord == nil {
		return checkinRedeemResponse{}, checkinError("REDEEM_CODE_NOT_FOUND")
	}
	if !currentCodeRecord.Enabled {
		return checkinRedeemResponse{}, checkinError("REDEEM_CODE_DISABLED")
	}
	if currentCodeRecord.TargetUserID != nil && *currentCodeRecord.TargetUserID != user.ID {
		return checkinRedeemResponse{}, checkinError("REDEEM_CODE_TARGET_USER_MISMATCH")
	}
	if currentCodeRecord.ExpiresAt != nil && currentCodeRecord.ExpiresAt.Before(timeNow()) {
		return checkinRedeemResponse{}, checkinError("REDEEM_CODE_EXPIRED")
	}
	if currentCodeRecord.UsedCount >= currentCodeRecord.MaxUses {
		return checkinRedeemResponse{}, checkinError("REDEEM_CODE_EXHAUSTED")
	}
	hasUsed, err := db.HasUserUsedCode(ctx, int64(currentCodeRecord.ID), int64(user.ID))
	if err != nil {
		return checkinRedeemResponse{}, err
	}
	if hasUsed {
		return checkinRedeemResponse{}, checkinError("REDEEM_CODE_ALREADY_USED_BY_USER")
	}
	if currentCodeRecord.BatchID != nil && *currentCodeRecord.BatchID != "" {
		hasUsedBatch, err := db.HasUserUsedBatch(ctx, *currentCodeRecord.BatchID, int64(user.ID))
		if err != nil {
			return checkinRedeemResponse{}, err
		}
		if hasUsedBatch {
			return checkinRedeemResponse{}, checkinError("REDEEM_CODE_BATCH_LIMIT")
		}
	}

	currentInstance, err := db.CheckinGetInstanceByID(ctx, instanceID)
	if err != nil {
		return checkinRedeemResponse{}, err
	}
	if currentInstance == nil {
		return checkinRedeemResponse{}, checkinError("INSTANCE_NOT_FOUND")
	}
	if int64(currentInstance.UserID) != int64(user.ID) {
		return checkinRedeemResponse{}, checkinError("FORBIDDEN")
	}
	if currentInstance.Status != "running" && currentInstance.Status != "stopped" {
		return checkinRedeemResponse{}, checkinError("INSTANCE_STATUS_INVALID")
	}
	if currentInstance.HostID != currentCodeRecord.HostID {
		return checkinRedeemResponse{}, checkinError("REDEEM_CODE_HOST_MISMATCH")
	}

	codeType := string(currentCodeRecord.CodeType)
	codeValue := currentCodeRecord.CodeValue
	actualAdded := codeValue
	if !ajvEnumContains(checkinRedeemResourceTypes, codeType) {
		return checkinRedeemResponse{}, checkinError("REDEEM_CODE_INVALID_FORMAT")
	}

	var patchedRollback func() error

	applyErr := func() error {
		if codeType == "c" {
			newCPU := currentInstance.CPU + codeValue
			client, err := checkinIncusClient(ctx, currentInstance.HostID)
			if err != nil {
				return err
			}
			if _, err := lib.PatchInstanceResources(ctx, client, currentInstance.IncusID, checkinIntPtr(int(newCPU)), nil, nil); err != nil {
				return err
			}
			original := int(currentInstance.CPU)
			patchedRollback = func() error {
				_, err := lib.PatchInstanceResources(ctx, client, currentInstance.IncusID, &original, nil, nil)
				return err
			}
			return db.ApplySystemRedeemCodeToInstance(ctx, db.SystemRedeemCodeApplication{
				RedeemCodeID: int64(currentCodeRecord.ID),
				UserID:       int64(user.ID),
				InstanceID:   instanceID,
				HostID:       int64(currentInstance.HostID),
				BatchID:      currentCodeRecord.BatchID,
				InstanceResources: &db.SystemRedeemInstanceResources{
					CPU: checkinInt32Ptr(newCPU),
				},
				HostResourceDelta: &db.SystemRedeemHostResourceDelta{
					CPUUsed: checkinInt32Ptr(actualAdded),
				},
			})
		}

		if codeType == "r" {
			newMemory := currentInstance.Memory + codeValue
			client, err := checkinIncusClient(ctx, currentInstance.HostID)
			if err != nil {
				return err
			}
			if _, err := lib.PatchInstanceResources(ctx, client, currentInstance.IncusID, nil, checkinIntPtr(int(newMemory)), nil); err != nil {
				return err
			}
			original := int(currentInstance.Memory)
			patchedRollback = func() error {
				_, err := lib.PatchInstanceResources(ctx, client, currentInstance.IncusID, nil, &original, nil)
				return err
			}
			return db.ApplySystemRedeemCodeToInstance(ctx, db.SystemRedeemCodeApplication{
				RedeemCodeID: int64(currentCodeRecord.ID),
				UserID:       int64(user.ID),
				InstanceID:   instanceID,
				HostID:       int64(currentInstance.HostID),
				BatchID:      currentCodeRecord.BatchID,
				InstanceResources: &db.SystemRedeemInstanceResources{
					Memory: checkinInt32Ptr(newMemory),
				},
				HostResourceDelta: &db.SystemRedeemHostResourceDelta{
					MemoryUsed: checkinInt32Ptr(actualAdded),
				},
			})
		}

		if codeType == "d" {
			newDisk := currentInstance.Disk + codeValue
			client, err := checkinIncusClient(ctx, currentInstance.HostID)
			if err != nil {
				return err
			}
			if _, err := lib.PatchInstanceResources(ctx, client, currentInstance.IncusID, nil, nil, checkinIntPtr(int(newDisk))); err != nil {
				return err
			}
			original := int(currentInstance.Disk)
			patchedRollback = func() error {
				_, err := lib.PatchInstanceResources(ctx, client, currentInstance.IncusID, nil, nil, &original)
				return err
			}
			return db.ApplySystemRedeemCodeToInstance(ctx, db.SystemRedeemCodeApplication{
				RedeemCodeID: int64(currentCodeRecord.ID),
				UserID:       int64(user.ID),
				InstanceID:   instanceID,
				HostID:       int64(currentInstance.HostID),
				BatchID:      currentCodeRecord.BatchID,
				InstanceResources: &db.SystemRedeemInstanceResources{
					Disk: checkinInt32Ptr(newDisk),
				},
				HostResourceDelta: &db.SystemRedeemHostResourceDelta{
					DiskUsed: checkinInt32Ptr(actualAdded),
				},
			})
		}

		// codeType == 't'
		trafficBytes := int64(codeValue) * int64(1024*1024*1024)
		return db.ApplySystemRedeemCodeToInstance(ctx, db.SystemRedeemCodeApplication{
			RedeemCodeID:           int64(currentCodeRecord.ID),
			UserID:                 int64(user.ID),
			InstanceID:             instanceID,
			HostID:                 int64(currentInstance.HostID),
			BatchID:                currentCodeRecord.BatchID,
			ExtraTrafficQuotaDelta: &trafficBytes,
		})
	}()

	if applyErr != nil {
		if patchedRollback != nil {
			if rollbackErr := patchedRollback(); rollbackErr != nil {
				log.Printf("Failed to roll back Incus resource patch after redeem DB failure (instanceId=%d): %v",
					instanceID, rollbackErr)
			}
		}
		return checkinRedeemResponse{}, applyErr
	}

	typeName, unit := checkinRedeemTypeLabel(codeType)
	checkinLog(ctx, user.ID, "redeem.success",
		"Redeemed system code "+trimmedCode+" for instance \""+currentInstance.Name+"\": "+
			typeName+" +"+db.CheckinFormatInt(int(actualAdded))+unit,
		"success", db.LogOptionsWithInstance(checkinInt32Ptr(currentInstance.ID)))

	// 记录到资源池日志（不加资源池，因为资源直接应用到实例）
	if err := db.ResourcePoolLogSystemRedeemToInstance(ctx, user.ID, models.RedeemCodeType(codeType),
		actualAdded, currentInstance.ID, "系统兑换码 "+trimmedCode+" 应用到 "+currentInstance.Name); err != nil {
		return checkinRedeemResponse{}, err
	}

	return checkinRedeemResponse{
		Message:        "Redeem successful",
		CodeType:       codeType,
		CodeValue:      codeValue,
		ActualAdded:    actualAdded,
		InstanceID:     instanceID,
		InstanceName:   currentInstance.Name,
		IsSystemCode:   true,
		ToResourcePool: false,
	}, nil
}

// checkinIncusClient builds the Incus client for a host id.
func checkinIncusClient(ctx context.Context, hostID int32) (*lib.IncusClient, error) {
	host, err := db.CheckinGetHostByID(ctx, int64(hostID))
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, checkinError("HOST_NOT_FOUND")
	}
	return lib.GetIncusClient(ctx, lib.IncusClientOptions{
		URL:      host.URL,
		CertPath: host.CertPath,
		KeyPath:  host.KeyPath,
		HostID:   int64(host.ID),
	})
}

// checkinRespondRedeemError mirrors the catch block of POST /redeem.
func checkinRespondRedeemError(c *gin.Context, userID int32, instanceID int64, trimmedCode string, err error) {
	ctx := c.Request.Context()
	message := err.Error()

	switch message {
	case "REDEEM_CODE_NOT_FOUND":
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrRedeemCodeNotFound, "")
	case "REDEEM_CODE_DISABLED":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeDisabled, "")
	case "REDEEM_CODE_EXPIRED":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeExpired, "")
	case "REDEEM_CODE_INVALID_FORMAT":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeInvalidFormat, "")
	case "REDEEM_CODE_EXHAUSTED":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeExhausted, "")
	case "REDEEM_CODE_ALREADY_USED_BY_USER":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeAlreadyUsedByUser, "")
	case "REDEEM_CODE_BATCH_LIMIT":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeBatchLimit, "")
	case "REDEEM_CODE_BUSY":
		c.JSON(http.StatusConflict, checkinRedeemBusyBody{
			Error:   "REDEEM_CODE_BUSY",
			Message: "Redeem code is busy, please retry",
		})
	case "INSTANCE_NOT_FOUND":
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
	case "FORBIDDEN":
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
	case "REDEEM_CODE_TARGET_USER_MISMATCH":
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "This redeem code is assigned to another user")
	case "INSTANCE_STATUS_INVALID":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceStatusInvalid, "")
	case "REDEEM_CODE_HOST_MISMATCH":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrRedeemCodeHostMismatch, "")
	case "HOST_NOT_FOUND":
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
	default:
		checkinLog(ctx, userID, "redeem.failed",
			"Failed to redeem code "+trimmedCode+": "+message, "failed",
			db.LogOptionsWithInstance(checkinInt32Ptr(int32(instanceID))))
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, message)
	}
}

// int32Ptr is the shared address-of helper for the int32 projections.
func checkinInt32Ptr(value int32) *int32 { return &value }

func init() {
	Register(RegisterCheckin)
}
