// Port of server/src/routes/resource-pool.ts.
//
// app.ts registers this module as
// `fastify.register(resourcePoolRoutes, { prefix: '/api/resource-pool' })`, so
// every path lives under /api/resource-pool and every route carries
// `onRequest: [fastify.authenticateUser]`. The plugin root is the only '/' route:
// Fastify's default prefixTrailingSlash is 'both' (fastify@5 lib/route.js
// `prefixTrailingSlash: (opts.prefixTrailingSlash || 'both')`), so BOTH
// GET /api/resource-pool and GET /api/resource-pool/ are mounted here.
//
// High-risk flow: POST /apply consumes the user's pool once. It runs inside the
// two *table-level* distributed locks of the TypeScript
// ("resource-pool:<userId>:<type>" then "instance:<instanceId>:resource-pool-apply",
// 120 s expiry / 10 s wait / 100 ms retry, released in reverse) and hands the
// atomic debit + host-capacity check to db.ResourcePoolApplyToInstance, which
// holds USER_RESOURCE_POOL_LOCK_NAMESPACE (4114) + INSTANCE_OPERATION_LOCK_NAMESPACE
// (4106) for the whole transaction. The DB intent commits BEFORE the Incus PATCH;
// a failed PATCH leaves the row stamped with "[incus-pending] " and is retried by
// the reconciler goroutine below.
//
// services/vip-benefits.ts is not ported as a package yet, so the slice this route
// needs (getUserContinuousVipBenefit + calculateVipResourcePoolCost + benefits
// config normalization) is reproduced here verbatim with resourcePool* names. The
// VIP bonus only reduces the pool debit; host-capacity accounting always uses the
// full delivered amount.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// Resource type labels/units, mirroring RESOURCE_TYPE_NAMES / RESOURCE_TYPE_UNITS.
var (
	resourcePoolTypeNames = map[string]string{
		"c": "CPU",
		"r": "内存",
		"d": "硬盘",
		"t": "流量",
	}
	resourcePoolTypeUnits = map[string]string{
		"c": "%",
		"r": "MB",
		"d": "MB",
		"t": "GB",
	}
	// resourcePoolActions mirrors RESOURCE_POOL_ACTIONS (the log filter allowlist).
	resourcePoolActions = []string{
		"checkin",
		"redeem",
		"admin_grant",
		"system_grant",
		"lottery",
		"apply",
		"system_redeem",
	}
	// resourcePoolResourceTypes mirrors RESOURCE_POOL_RESOURCE_TYPES. The `p`
	// (points) type can never enter the resource pool.
	resourcePoolResourceTypes = []string{"c", "r", "d", "t"}
)

// Reconciliation timings mirror RESOURCE_POOL_RECONCILIATION_INTERVAL_MS and the
// 10 s startup timeout.
const (
	resourcePoolReconciliationStartupDelay = 10 * time.Second
	resourcePoolReconciliationInterval     = 5 * time.Minute
	resourcePoolReconcileLockExpireMs      = 120_000
	resourcePoolReconcileLockWaitTimeoutMs = 1_000
	resourcePoolReconcileLockRetryMs       = 100
)

// Query-shape patterns mirror POSITIVE_INTEGER_QUERY_PATTERN /
// NON_NEGATIVE_INTEGER_QUERY_PATTERN.
var (
	resourcePoolPositiveIntegerPattern    = regexp.MustCompile(`^[1-9][0-9]*$`)
	resourcePoolNonNegativeIntegerPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	resourcePoolMaxSafeInteger            = int64(9007199254740991)
	resourcePoolReconciliationRunning     atomic.Bool
	resourcePoolReconciliationStartupOnce sync.Once
)

// ==================== VIP benefits (slice of services/vip-benefits.ts) ====================

// Resource-pool VIP constants, mirroring vip-benefits.ts.
const (
	resourcePoolMaxVipBenefitPercent  = 100
	resourcePoolMaxContinuousVipLevel = 5
	resourcePoolVipBenefitsConfigKey  = "vip_benefits_config"
)

// resourcePoolVipBenefitLevel mirrors VipContinuousBenefitLevel.
type resourcePoolVipBenefitLevel struct {
	OrderDiscountPercent     float64
	ExtraTrafficPercent      float64
	ResourcePoolBonusPercent float64
}

// resourcePoolVipBenefitsConfig mirrors VipContinuousBenefitsConfig.
type resourcePoolVipBenefitsConfig struct {
	Levels map[int]resourcePoolVipBenefitLevel
}

// resourcePoolDefaultVipBenefitsConfig mirrors DEFAULT_VIP_BENEFITS_CONFIG.
func resourcePoolDefaultVipBenefitsConfig() resourcePoolVipBenefitsConfig {
	return resourcePoolVipBenefitsConfig{Levels: map[int]resourcePoolVipBenefitLevel{
		1: {OrderDiscountPercent: 1, ExtraTrafficPercent: 2, ResourcePoolBonusPercent: 2},
		2: {OrderDiscountPercent: 2, ExtraTrafficPercent: 4, ResourcePoolBonusPercent: 4},
		3: {OrderDiscountPercent: 3, ExtraTrafficPercent: 6, ResourcePoolBonusPercent: 6},
		4: {OrderDiscountPercent: 4, ExtraTrafficPercent: 8, ResourcePoolBonusPercent: 8},
		5: {OrderDiscountPercent: 5, ExtraTrafficPercent: 10, ResourcePoolBonusPercent: 10},
	}}
}

// resourcePoolNormalizeBenefitPercent mirrors normalizeBenefitPercent:
// finite, within [0, 100], rounded to 2 decimals. The second return value is
// false where the TypeScript throws VipBenefitError.
func resourcePoolNormalizeBenefitPercent(value any, field string) (float64, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > resourcePoolMaxVipBenefitPercent {
		return 0, false
	}
	return math.Round(number*100) / 100, true
}

// resourcePoolNormalizeVipBenefitsConfig mirrors normalizeVipBenefitsConfig. It
// returns false on the first invalid field (the TS throws).
func resourcePoolNormalizeVipBenefitsConfig(value any) (resourcePoolVipBenefitsConfig, bool) {
	input, _ := value.(map[string]any)
	levels, _ := input["levels"].(map[string]any)

	normalized := make(map[int]resourcePoolVipBenefitLevel, resourcePoolMaxContinuousVipLevel)
	for level := 1; level <= resourcePoolMaxContinuousVipLevel; level++ {
		levelConfig, _ := levels[strconv.Itoa(level)].(map[string]any)

		orderDiscount, ok := resourcePoolNormalizeBenefitPercent(levelConfig["orderDiscountPercent"], "order discount")
		if !ok {
			return resourcePoolVipBenefitsConfig{}, false
		}
		extraTraffic, ok := resourcePoolNormalizeBenefitPercent(levelConfig["extraTrafficPercent"], "extra traffic")
		if !ok {
			return resourcePoolVipBenefitsConfig{}, false
		}
		resourcePoolBonus, ok := resourcePoolNormalizeBenefitPercent(levelConfig["resourcePoolBonusPercent"], "resource pool bonus")
		if !ok {
			return resourcePoolVipBenefitsConfig{}, false
		}
		normalized[level] = resourcePoolVipBenefitLevel{
			OrderDiscountPercent:     orderDiscount,
			ExtraTrafficPercent:      extraTraffic,
			ResourcePoolBonusPercent: resourcePoolBonus,
		}
	}
	return resourcePoolVipBenefitsConfig{Levels: normalized}, true
}

// resourcePoolParseVipBenefitsConfigJSON mirrors parseVipBenefitsConfigJson:
// absent or unparseable/invalid JSON falls back to the defaults.
func resourcePoolParseVipBenefitsConfigJSON(value *string) resourcePoolVipBenefitsConfig {
	if value == nil || *value == "" {
		return resourcePoolDefaultVipBenefitsConfig()
	}
	var decoded any
	if err := json.Unmarshal([]byte(*value), &decoded); err != nil {
		return resourcePoolDefaultVipBenefitsConfig()
	}
	normalized, ok := resourcePoolNormalizeVipBenefitsConfig(decoded)
	if !ok {
		return resourcePoolDefaultVipBenefitsConfig()
	}
	return normalized
}

// resourcePoolGetVipBenefitsConfig mirrors getVipBenefitsConfig.
func resourcePoolGetVipBenefitsConfig(ctx context.Context) (resourcePoolVipBenefitsConfig, error) {
	raw, err := db.GetSystemConfig(ctx, resourcePoolVipBenefitsConfigKey, true)
	if err != nil {
		return resourcePoolVipBenefitsConfig{}, err
	}
	return resourcePoolParseVipBenefitsConfigJSON(raw), nil
}

// resourcePoolGetUserContinuousVipBenefit mirrors getUserContinuousVipBenefit.
func resourcePoolGetUserContinuousVipBenefit(ctx context.Context, userID int32) (float64, resourcePoolVipBenefitLevel, error) {
	metric, err := services.GetUserVipMetric(ctx)
	if err != nil {
		return 0, resourcePoolVipBenefitLevel{}, err
	}
	rules, err := services.GetVipRules(ctx, services.VipRuleTypeUser)
	if err != nil {
		return 0, resourcePoolVipBenefitLevel{}, err
	}
	stats, err := services.GetUserVipStats(ctx, userID)
	if err != nil {
		return 0, resourcePoolVipBenefitLevel{}, err
	}
	config, err := resourcePoolGetVipBenefitsConfig(ctx)
	if err != nil {
		return 0, resourcePoolVipBenefitLevel{}, err
	}

	level := services.CalculateUserVipLevelByMetric(rules, stats, metric)
	if level > 0 {
		bounded := int(math.Min(level, resourcePoolMaxContinuousVipLevel))
		if benefit, ok := config.Levels[bounded]; ok {
			return level, benefit, nil
		}
		return level, resourcePoolVipBenefitLevel{}, nil
	}
	return level, resourcePoolVipBenefitLevel{}, nil
}

// resourcePoolCalculateVipResourcePoolCost mirrors calculateVipResourcePoolCost.
// The ceil division is exact decimal arithmetic: pool debits are integer resource
// units and must never be computed through float64.
func resourcePoolCalculateVipResourcePoolCost(amount int64, bonusPercent float64) (int64, error) {
	if amount <= 0 || amount > resourcePoolMaxSafeInteger {
		return 0, errors.New("INVALID_RESOURCE_POOL_AMOUNT")
	}
	boundedBonus := bonusPercent
	if math.IsNaN(boundedBonus) {
		boundedBonus = 0
	}
	if boundedBonus < 0 {
		boundedBonus = 0
	}
	if boundedBonus > resourcePoolMaxVipBenefitPercent {
		boundedBonus = resourcePoolMaxVipBenefitPercent
	}

	numerator := decimal.NewFromInt(amount).Mul(decimal.NewFromInt(100))
	denominator := decimal.NewFromInt(100).Add(decimal.NewFromFloat(boundedBonus))
	cost := numerator.Div(denominator).Ceil().IntPart()
	if cost < 1 {
		cost = 1
	}
	return cost, nil
}

// ==================== query helpers ====================

// resourcePoolParseClampedPositiveIntegerQuery mirrors
// parseClampedPositiveIntegerQuery: invalid values fall back, valid ones clamp.
func resourcePoolParseClampedPositiveIntegerQuery(value string, fallback, max int) int {
	if value == "" || !resourcePoolPositiveIntegerPattern.MatchString(value) {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed > resourcePoolMaxSafeInteger {
		return fallback
	}
	if parsed > int64(max) {
		return max
	}
	return int(parsed)
}

// resourcePoolParseNonNegativeIntegerQuery mirrors parseNonNegativeIntegerQuery.
func resourcePoolParseNonNegativeIntegerQuery(value string, fallback int) int {
	if value == "" || !resourcePoolNonNegativeIntegerPattern.MatchString(value) {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed > resourcePoolMaxSafeInteger {
		return fallback
	}
	// Prisma receives the raw skip value; clamp rather than overflow Go's int.
	if parsed > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(parsed)
}

// resourcePoolSingleQueryValue mirrors fast-querystring's scalar read: a repeated
// key is an array, which every allowlist/Has() check rejects.
func resourcePoolSingleQueryValue(query map[string][]string, key string) string {
	values, present := query[key]
	if !present || len(values) != 1 {
		return ""
	}
	return values[0]
}

// resourcePoolNormalizeAction mirrors normalizeResourcePoolAction.
func resourcePoolNormalizeAction(value string) *string {
	if value == "" || !ajvEnumContains(resourcePoolActions, value) {
		return nil
	}
	return &value
}

// resourcePoolNormalizeType mirrors normalizeResourcePoolType.
func resourcePoolNormalizeType(value string) *string {
	if value == "" || !ajvEnumContains(resourcePoolResourceTypes, value) {
		return nil
	}
	return &value
}

// ==================== response shapes ====================

// resourcePoolApplyResponse mirrors the POST /apply return object.
type resourcePoolApplyResponse struct {
	Message                     string  `json:"message"`
	ResourceType                string  `json:"resourceType"`
	Amount                      string  `json:"amount"`
	PoolDebitAmount             string  `json:"poolDebitAmount"`
	VipResourcePoolBonusPercent float64 `json:"vipResourcePoolBonusPercent"`
	InstanceID                  int64   `json:"instanceId"`
	InstanceName                string  `json:"instanceName"`
	ReconciliationPending       bool    `json:"reconciliationPending"`
}

// resourcePoolBusyBody mirrors the hand-written 409 body.
type resourcePoolBusyBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// resourcePoolInstanceHostResponse mirrors the host sub-object of GET /instances.
type resourcePoolInstanceHostResponse struct {
	ID          int32   `json:"id"`
	Name        string  `json:"name"`
	Location    *string `json:"location"`
	CountryCode string  `json:"countryCode"`
}

// resourcePoolInstanceResponse mirrors one entry of GET /instances.
type resourcePoolInstanceResponse struct {
	ID                  int32                            `json:"id"`
	Name                string                           `json:"name"`
	Status              string                           `json:"status"`
	CPU                 int32                            `json:"cpu"`
	Memory              int32                            `json:"memory"`
	Disk                int32                            `json:"disk"`
	MonthlyTrafficLimit *string                          `json:"monthlyTrafficLimit"`
	IsPaid              bool                             `json:"isPaid"`
	InstanceType        string                           `json:"instanceType"`
	Host                resourcePoolInstanceHostResponse `json:"host"`
}

// ==================== helpers ====================

// resourcePoolError builds an error whose Error() is the exact message the
// TypeScript throws, so the shared switch below can map it.
func resourcePoolError(message string) error {
	return errors.New(message)
}

func resourcePoolIntPtr(value int) *int { return &value }

// resourcePoolContains mirrors Array.prototype.includes.
func resourcePoolContains(values []string, candidate string) bool {
	return ajvEnumContains(values, candidate)
}

// resourcePoolApplyLockKeys mirrors withResourcePoolApplyLocks' lockKeys array:
// per user/resource-type first, then the instance guard.
func resourcePoolApplyLockKeys(userID int32, instanceID int64, resourceType string) []string {
	return []string{
		fmt.Sprintf("resource-pool:%d:%s", userID, resourceType),
		fmt.Sprintf("instance:%d:resource-pool-apply", instanceID),
	}
}

// resourcePoolWithApplyLocks mirrors withResourcePoolApplyLocks: two table-level
// distributed locks, acquired in order, released in reverse.
func resourcePoolWithApplyLocks[T any](
	ctx context.Context,
	userID int32,
	instanceID int64,
	resourceType string,
	fn func() (T, error),
) (T, error) {
	lockKeys := resourcePoolApplyLockKeys(userID, instanceID, resourceType)

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
			ExpireMs:        resourcePoolIntPtr(120_000),
			WaitTimeoutMs:   resourcePoolIntPtr(10_000),
			RetryIntervalMs: resourcePoolIntPtr(100),
		})
		if err != nil {
			var zero T
			return zero, err
		}
		if !result.Success || result.OwnerID == "" {
			var zero T
			return zero, resourcePoolError("RESOURCE_POOL_BUSY")
		}
		acquired = append(acquired, heldLock{key: lockKey, ownerID: result.OwnerID})
	}

	return fn()
}

// resourcePoolIncusClient builds the Incus client for a host row.
func resourcePoolIncusClient(ctx context.Context, host *db.ResourcePoolHostForApply) (*lib.IncusClient, error) {
	return lib.GetIncusClient(ctx, lib.IncusClientOptions{
		URL:      host.URL,
		CertPath: host.CertPath,
		KeyPath:  host.KeyPath,
		HostID:   int64(host.ID),
	})
}

// resourcePoolIntFromInt32 widens the int32 resource projections to the *int the
// Incus helpers take.
func resourcePoolIntFromInt32(value *int32) *int {
	if value == nil {
		return nil
	}
	converted := int(*value)
	return &converted
}

// resourcePoolPatchAndConfirm mirrors the try block around the Incus PATCH: patch,
// then clear the pending marker. Any error is reported to the caller, which turns
// it into reconciliationPending.
func resourcePoolPatchAndConfirm(
	ctx context.Context,
	host *db.ResourcePoolHostForApply,
	instance *db.ResourcePoolInstance,
	resources *db.ResourcePoolInstanceResources,
	applyLogID int32,
) error {
	client, err := resourcePoolIncusClient(ctx, host)
	if err != nil {
		return err
	}
	if _, err := lib.PatchInstanceResources(ctx, client, instance.IncusID,
		resourcePoolIntFromInt32(resources.CPU),
		resourcePoolIntFromInt32(resources.Memory),
		resourcePoolIntFromInt32(resources.Disk)); err != nil {
		return err
	}
	return db.ResourcePoolMarkAppliesReconciled(ctx, []int32{applyLogID})
}

// resourcePoolApplyLocked mirrors the closure passed to withResourcePoolApplyLocks.
func resourcePoolApplyLocked(
	ctx context.Context,
	userID int32,
	instanceID int64,
	resourceType string,
	amount int32,
) (resourcePoolApplyResponse, error) {
	instance, err := db.ResourcePoolGetInstanceByID(ctx, instanceID)
	if err != nil {
		return resourcePoolApplyResponse{}, err
	}
	if instance == nil {
		return resourcePoolApplyResponse{}, resourcePoolError("INSTANCE_NOT_FOUND")
	}
	if int64(instance.UserID) != int64(userID) {
		return resourcePoolApplyResponse{}, resourcePoolError("FORBIDDEN")
	}
	if instance.Status != "running" && instance.Status != "stopped" {
		return resourcePoolApplyResponse{}, resourcePoolError("INSTANCE_STATUS_INVALID")
	}

	host, err := db.ResourcePoolGetHostForApply(ctx, int64(instance.HostID))
	if err != nil {
		return resourcePoolApplyResponse{}, err
	}
	if host == nil {
		return resourcePoolApplyResponse{}, resourcePoolError("HOST_NOT_FOUND")
	}
	if !host.EnableResourcePool {
		return resourcePoolApplyResponse{}, resourcePoolError("RESOURCE_POOL_DISABLED")
	}

	_, benefit, err := resourcePoolGetUserContinuousVipBenefit(ctx, userID)
	if err != nil {
		return resourcePoolApplyResponse{}, err
	}
	poolDebitAmount, err := resourcePoolCalculateVipResourcePoolCost(int64(amount), benefit.ResourcePoolBonusPercent)
	if err != nil {
		return resourcePoolApplyResponse{}, err
	}

	isVM := host.InstanceType == models.InstanceTypeVm
	if resourceType == "c" && isVM && amount%100 != 0 {
		return resourcePoolApplyResponse{}, resourcePoolError("RESOURCE_POOL_KVM_CPU_MULTIPLE")
	}
	if resourceType == "r" && isVM && amount%128 != 0 {
		return resourcePoolApplyResponse{}, resourcePoolError("RESOURCE_POOL_KVM_MEMORY_MULTIPLE")
	}
	if resourceType == "d" && isVM && amount%1024 != 0 {
		return resourcePoolApplyResponse{}, resourcePoolError("RESOURCE_POOL_KVM_DISK_MULTIPLE")
	}
	if (resourceType == "r" || resourceType == "d") && isVM && instance.Status != "stopped" {
		return resourcePoolApplyResponse{}, resourcePoolError("RESOURCE_POOL_VM_MUST_STOP")
	}

	remark := "应用到实例 " + instance.Name
	debit := int32(poolDebitAmount)

	var resourcesToPatch *db.ResourcePoolInstanceResources
	var applyLogID *int32

	switch resourceType {
	case "c":
		newCPU := instance.CPU + amount
		logID, err := db.ResourcePoolApplyToInstance(ctx, db.ResourcePoolApplyInput{
			UserID:            userID,
			InstanceID:        instanceID32(instanceID),
			HostID:            instance.HostID,
			ResourceType:      models.RedeemCodeTypeC,
			Amount:            amount,
			PoolDebitAmount:   &debit,
			Remark:            &remark,
			InstanceResources: &db.ResourcePoolInstanceResources{CPU: checkinInt32Ptr(newCPU)},
			HostResourceDelta: &db.ResourcePoolHostResourceDelta{CPUUsed: checkinInt32Ptr(amount)},
		})
		if err != nil {
			return resourcePoolApplyResponse{}, err
		}
		if logID == nil {
			return resourcePoolApplyResponse{}, resourcePoolError("RESOURCE_POOL_INSUFFICIENT")
		}
		applyLogID = logID
		resourcesToPatch = &db.ResourcePoolInstanceResources{CPU: checkinInt32Ptr(newCPU)}

	case "r":
		newMemory := instance.Memory + amount
		logID, err := db.ResourcePoolApplyToInstance(ctx, db.ResourcePoolApplyInput{
			UserID:            userID,
			InstanceID:        instanceID32(instanceID),
			HostID:            instance.HostID,
			ResourceType:      models.RedeemCodeTypeR,
			Amount:            amount,
			PoolDebitAmount:   &debit,
			Remark:            &remark,
			InstanceResources: &db.ResourcePoolInstanceResources{Memory: checkinInt32Ptr(newMemory)},
			HostResourceDelta: &db.ResourcePoolHostResourceDelta{MemoryUsed: checkinInt32Ptr(amount)},
		})
		if err != nil {
			return resourcePoolApplyResponse{}, err
		}
		if logID == nil {
			return resourcePoolApplyResponse{}, resourcePoolError("RESOURCE_POOL_INSUFFICIENT")
		}
		applyLogID = logID
		resourcesToPatch = &db.ResourcePoolInstanceResources{Memory: checkinInt32Ptr(newMemory)}

	case "d":
		newDisk := instance.Disk + amount
		logID, err := db.ResourcePoolApplyToInstance(ctx, db.ResourcePoolApplyInput{
			UserID:            userID,
			InstanceID:        instanceID32(instanceID),
			HostID:            instance.HostID,
			ResourceType:      models.RedeemCodeTypeD,
			Amount:            amount,
			PoolDebitAmount:   &debit,
			Remark:            &remark,
			InstanceResources: &db.ResourcePoolInstanceResources{Disk: checkinInt32Ptr(newDisk)},
			HostResourceDelta: &db.ResourcePoolHostResourceDelta{DiskUsed: checkinInt32Ptr(amount)},
		})
		if err != nil {
			return resourcePoolApplyResponse{}, err
		}
		if logID == nil {
			return resourcePoolApplyResponse{}, resourcePoolError("RESOURCE_POOL_INSUFFICIENT")
		}
		applyLogID = logID
		resourcesToPatch = &db.ResourcePoolInstanceResources{Disk: checkinInt32Ptr(newDisk)}

	case "t":
		trafficBytes := int64(amount) * int64(1024*1024*1024)
		logID, err := db.ResourcePoolApplyToInstance(ctx, db.ResourcePoolApplyInput{
			UserID:              userID,
			InstanceID:          instanceID32(instanceID),
			HostID:              instance.HostID,
			ResourceType:        models.RedeemCodeTypeT,
			Amount:              amount,
			PoolDebitAmount:     &debit,
			Remark:              &remark,
			MonthlyTrafficDelta: &trafficBytes,
		})
		if err != nil {
			return resourcePoolApplyResponse{}, err
		}
		if logID == nil {
			return resourcePoolApplyResponse{}, resourcePoolError("RESOURCE_POOL_INSUFFICIENT")
		}
		applyLogID = logID
	}

	reconciliationPending := false
	if resourcesToPatch != nil {
		if err := resourcePoolPatchAndConfirm(ctx, host, instance, resourcesToPatch, *applyLogID); err != nil {
			reconciliationPending = true
			log.Printf("Resource-pool DB apply committed but Incus patch confirmation failed; reconciliation will retry (instanceId=%d): %v",
				instanceID, err)
		}
	}

	typeName, ok := resourcePoolTypeNames[resourceType]
	if !ok {
		typeName = resourceType
	}
	unit := resourcePoolTypeUnits[resourceType]

	logUserID := int64(userID)
	db.CreateLog(ctx, &logUserID, "resource_pool", "apply.success",
		"Applied "+db.CheckinFormatInt(int(amount))+unit+" "+typeName+" to instance "+instance.Name,
		"success", db.LogOptionsWithInstance(checkinInt32Ptr(instance.ID)))

	return resourcePoolApplyResponse{
		Message:                     "Resource applied successfully",
		ResourceType:                resourceType,
		Amount:                      db.CheckinFormatInt64(int64(amount)),
		PoolDebitAmount:             db.CheckinFormatInt64(poolDebitAmount),
		VipResourcePoolBonusPercent: benefit.ResourcePoolBonusPercent,
		InstanceID:                  instanceID,
		InstanceName:                instance.Name,
		ReconciliationPending:       reconciliationPending,
	}, nil
}

// instanceID32 narrows a route id for the int4 instance columns.
func instanceID32(instanceID int64) int32 {
	if instanceID > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(instanceID)
}

// resourcePoolRespondApplyError mirrors the catch block of POST /apply.
func resourcePoolRespondApplyError(c *gin.Context, userID int32, instanceID int64, err error) {
	ctx := c.Request.Context()
	message := err.Error()

	switch message {
	case "INSTANCE_NOT_FOUND":
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
	case "FORBIDDEN":
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
	case "INSTANCE_STATUS_INVALID":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInstanceStatusInvalid, "")
	case "HOST_NOT_FOUND":
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrHostNotFound, "")
	case "RESOURCE_POOL_DISABLED":
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrFeatureDisabled, "该节点未开启资源池玩法")
	case "RESOURCE_POOL_KVM_CPU_MULTIPLE":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrResourcePoolKvmCpuMultiple, "")
	case "RESOURCE_POOL_KVM_MEMORY_MULTIPLE":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrResourcePoolKvmMemoryMultiple, "")
	case "RESOURCE_POOL_KVM_DISK_MULTIPLE":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrResourcePoolKvmDiskMultiple, "")
	case "RESOURCE_POOL_VM_MUST_STOP":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrResourcePoolVmMustStop, "")
	case "RESOURCE_POOL_INSUFFICIENT":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrResourcePoolInsufficient, "")
	case "HOST_RESOURCES_INSUFFICIENT":
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrHostResourcesInsufficient,
			"Host capacity is insufficient for this resource claim")
	case "RESOURCE_POOL_BUSY":
		c.JSON(http.StatusConflict, resourcePoolBusyBody{
			Error:   "RESOURCE_POOL_BUSY",
			Message: "Resource pool apply is busy, please retry",
		})
	default:
		db.CreateLog(ctx, int64Ptr(userID), "resource_pool", "apply.failed",
			"Failed to apply resource to instance "+db.CheckinFormatInt64(instanceID)+": "+message,
			"failed", db.LogOptionsWithInstance(checkinInt32Ptr(instanceID32(instanceID))))
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrInternalError, message)
	}
}

func int64Ptr(value int32) *int64 {
	converted := int64(value)
	return &converted
}

// ==================== reconciliation ====================

// resourcePoolRunReconciliation mirrors runReconciliation: single-flight, never
// panics out of the goroutine.
func resourcePoolRunReconciliation() {
	if !resourcePoolReconciliationRunning.CompareAndSwap(false, true) {
		return
	}
	defer resourcePoolReconciliationRunning.Store(false)

	resourcePoolReconcileApplies(context.Background())
}

// resourcePoolStartReconciliation mirrors the setTimeout(10s) + setInterval(5m)
// pair registered by the plugin. Unlike Node's unref'd timers this goroutine also
// ends with the process; it is single-flight per Go process.
func resourcePoolStartReconciliation() {
	resourcePoolReconciliationStartupOnce.Do(func() {
		go func() {
			timer := time.NewTimer(resourcePoolReconciliationStartupDelay)
			defer timer.Stop()
			<-timer.C
			resourcePoolRunReconciliation()

			ticker := time.NewTicker(resourcePoolReconciliationInterval)
			defer ticker.Stop()
			for range ticker.C {
				resourcePoolRunReconciliation()
			}
		}()
	})
}

// resourcePoolReconcileApplies mirrors reconcileResourcePoolApplies.
func resourcePoolReconcileApplies(ctx context.Context) {
	// In a DB-less process (unit tests) there is nothing to reconcile; this also
	// keeps the startup goroutine from logging noise.
	if db.Pool() == nil {
		return
	}

	candidates, err := db.ResourcePoolGetApplyReconciliationCandidates(ctx)
	if err != nil {
		log.Printf("Resource-pool reconciliation scan failed; will retry: %v", err)
		return
	}

	for _, candidate := range candidates {
		lockKey := fmt.Sprintf("instance:%d:resource-pool-apply", candidate.InstanceID)
		lock, err := lib.AcquireLock(ctx, lockKey, lib.LockOptions{
			ExpireMs:        resourcePoolIntPtr(resourcePoolReconcileLockExpireMs),
			WaitTimeoutMs:   resourcePoolIntPtr(resourcePoolReconcileLockWaitTimeoutMs),
			RetryIntervalMs: resourcePoolIntPtr(resourcePoolReconcileLockRetryMs),
		})
		if err != nil {
			log.Printf("Resource-pool reconciliation lock failed (instanceId=%d): %v", candidate.InstanceID, err)
			continue
		}
		if !lock.Success || lock.OwnerID == "" {
			continue
		}

		func() {
			defer lib.ReleaseLock(ctx, lockKey, lock.OwnerID)
			resourcePoolReconcileCandidate(ctx, candidate)
		}()
	}
}

// resourcePoolReconcileCandidate mirrors the body of the candidate loop: bring
// Incus to the DB's desired state, then clear the pending markers it confirmed.
func resourcePoolReconcileCandidate(ctx context.Context, candidate db.ResourcePoolReconciliationCandidate) {
	instance, err := db.ResourcePoolGetInstanceByID(ctx, int64(candidate.InstanceID))
	if err != nil {
		log.Printf("Resource-pool Incus reconciliation failed; will retry (instanceId=%d): %v", candidate.InstanceID, err)
		return
	}
	if instance == nil || (instance.Status != "running" && instance.Status != "stopped") {
		return
	}

	host, err := db.ResourcePoolGetHostForApply(ctx, int64(instance.HostID))
	if err != nil || host == nil || !host.EnableResourcePool {
		if err != nil {
			log.Printf("Resource-pool Incus reconciliation failed; will retry (instanceId=%d): %v", candidate.InstanceID, err)
		}
		return
	}

	client, err := resourcePoolIncusClient(ctx, host)
	if err != nil {
		log.Printf("Resource-pool Incus reconciliation failed; will retry (instanceId=%d): %v", candidate.InstanceID, err)
		return
	}

	incusInstance, err := lib.GetInstance(ctx, client, instance.IncusID)
	if err != nil {
		log.Printf("Resource-pool Incus reconciliation failed; will retry (instanceId=%d): %v", candidate.InstanceID, err)
		return
	}

	var cpu, memory, disk *int
	reconciledTypes := map[string]bool{}

	if resourcePoolContains(candidate.ResourceTypes, "c") {
		reconciledTypes["c"] = true
		expected := db.CheckinFormatInt(int(instance.CPU)) + "%"
		if value, present := incusInstance.Config["limits.cpu.allowance"]; !present || value != expected {
			cpu = resourcePoolIntPtr(int(instance.CPU))
		}
	}

	canReconcileStoppedOnlyResources := host.InstanceType != models.InstanceTypeVm || instance.Status == "stopped"
	if canReconcileStoppedOnlyResources && resourcePoolContains(candidate.ResourceTypes, "r") {
		reconciledTypes["r"] = true
		expected := db.CheckinFormatInt(int(instance.Memory)) + "MB"
		if value, present := incusInstance.Config["limits.memory"]; !present || value != expected {
			memory = resourcePoolIntPtr(int(instance.Memory))
		}
	}

	rootDevice := incusInstance.Devices["root"]
	if canReconcileStoppedOnlyResources && resourcePoolContains(candidate.ResourceTypes, "d") {
		reconciledTypes["d"] = true
		expected := db.CheckinFormatInt(int(instance.Disk)) + "MB"
		if rootDevice == nil {
			disk = resourcePoolIntPtr(int(instance.Disk))
		} else if size, ok := rootDevice["size"].(string); !ok || size != expected {
			disk = resourcePoolIntPtr(int(instance.Disk))
		}
	}

	if cpu != nil || memory != nil || disk != nil {
		if _, err := lib.PatchInstanceResources(ctx, client, instance.IncusID, cpu, memory, disk); err != nil {
			log.Printf("Resource-pool Incus reconciliation failed; will retry (instanceId=%d): %v", candidate.InstanceID, err)
			return
		}
		log.Printf("Reconciled resource-pool Incus configuration from DB state (instanceId=%d)", instance.ID)
	}

	pendingIDs := make([]int32, 0, len(candidate.PendingApplications))
	for _, application := range candidate.PendingApplications {
		if reconciledTypes[application.ResourceType] {
			pendingIDs = append(pendingIDs, application.ID)
		}
	}
	if err := db.ResourcePoolMarkAppliesReconciled(ctx, pendingIDs); err != nil {
		log.Printf("Resource-pool Incus reconciliation failed; will retry (instanceId=%d): %v", candidate.InstanceID, err)
	}
}

// ==================== AJV schema ====================

var resourcePoolApplyBodyFields = []ajvFieldSpec{
	{Name: "instanceId", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: math.NaN()},
	{Name: "resourceType", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Minimum: math.NaN(), Maximum: math.NaN(), Enum: resourcePoolResourceTypes},
	{Name: "amount", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 1, Maximum: db.ResourcePoolMaxApplyAmount},
}

// validateResourcePoolApplyBodyAJV mirrors the /apply body schema
// (`required: ['instanceId','resourceType','amount']`).
//
// There is deliberately no root-array unwrap: measured against fastify@5.6.2, a
// single-element root array (`[{"instanceId":1,...}]`) fails with "must be
// object". AJV's coerceTypes:'array' unwrap applies to *properties*, not to the
// root document.
func validateResourcePoolApplyBodyAJV(decoded any) (ajvValidated, string) {
	return validateAJVObject(decoded,
		[]string{"instanceId", "resourceType", "amount"}, resourcePoolApplyBodyFields)
}

// ==================== routes ====================

// RegisterResourcePool mounts the resource-pool routes under /api/resource-pool.
func RegisterResourcePool(r *gin.Engine) {
	resourcePoolStartReconciliation()

	group := r.Group("/api/resource-pool", middleware.AuthenticateUser())

	// ==================== 获取用户资源池 ====================
	// Fastify's prefixTrailingSlash default ('both') serves the bare prefix and the
	// trailing-slash form; mount both, exactly like the TypeScript plugin root.
	getPool := func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		pool, err := db.ResourcePoolGetUserPool(c.Request.Context(), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, pool)
	}
	group.GET("", getPool)
	group.GET("/", getPool)

	// ==================== 应用资源到实例 ====================
	group.POST("/apply", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		decoded, status, parseMessage := decodeJSONRequestBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": parseMessage})
			return
		}
		validated, validationMessage := validateResourcePoolApplyBodyAJV(decoded.Value)
		if validationMessage != "" {
			respondAJVValidation(c, validationMessage)
			return
		}

		instanceIDNumber, _ := validated.Values["instanceId"].(float64)
		instanceID := int64(instanceIDNumber)
		resourceType, _ := validated.stringValue("resourceType")
		amountNumber, _ := validated.Values["amount"].(float64)
		amount := int32(amountNumber)

		response, err := resourcePoolWithApplyLocks(ctx, user.ID, instanceID, resourceType,
			func() (resourcePoolApplyResponse, error) {
				return resourcePoolApplyLocked(ctx, user.ID, instanceID, resourceType, amount)
			})
		if err != nil {
			resourcePoolRespondApplyError(c, user.ID, instanceID, err)
			return
		}
		c.JSON(http.StatusOK, response)
	})

	// ==================== 获取资源池变动记录 ====================
	group.GET("/logs", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		query := c.Request.URL.Query()

		result, err := db.ResourcePoolGetLogs(c.Request.Context(), user.ID, db.ResourcePoolLogOptions{
			Action:       resourcePoolNormalizeAction(resourcePoolSingleQueryValue(query, "action")),
			ResourceType: resourcePoolNormalizeType(resourcePoolSingleQueryValue(query, "resourceType")),
			Limit:        resourcePoolParseClampedPositiveIntegerQuery(resourcePoolSingleQueryValue(query, "limit"), 20, 100),
			Offset:       resourcePoolParseNonNegativeIntegerQuery(resourcePoolSingleQueryValue(query, "offset"), 0),
		})
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		c.JSON(http.StatusOK, result)
	})

	// ==================== 获取用户可应用资源的实例列表 ====================
	group.GET("/instances", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		rows, err := db.ResourcePoolGetUserAllInstances(c.Request.Context(), user.ID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}

		instances := make([]resourcePoolInstanceResponse, 0, len(rows))
		for _, inst := range rows {
			var monthlyTrafficLimit *string
			if inst.MonthlyTrafficLimit != nil {
				formatted := db.CheckinFormatInt64(*inst.MonthlyTrafficLimit)
				monthlyTrafficLimit = &formatted
			}
			instances = append(instances, resourcePoolInstanceResponse{
				ID:                  inst.ID,
				Name:                inst.Name,
				Status:              inst.Status,
				CPU:                 inst.CPU,
				Memory:              inst.Memory,
				Disk:                inst.Disk,
				MonthlyTrafficLimit: monthlyTrafficLimit,
				IsPaid:              inst.PackagePlanID != nil,
				InstanceType:        inst.Host.InstanceType,
				Host: resourcePoolInstanceHostResponse{
					ID:          inst.Host.ID,
					Name:        inst.Host.Name,
					Location:    inst.Host.Location,
					CountryCode: inst.Host.CountryCode,
				},
			})
		}

		c.JSON(http.StatusOK, gin.H{"instances": instances})
	})
}

func init() {
	Register(RegisterResourcePool)
}
