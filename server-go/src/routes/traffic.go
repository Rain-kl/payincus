// Port of server/src/routes/traffic.ts.
//
// app.ts registers this plugin with `{ prefix: '/api' }` and the module declares no
// module-root route, so every path below is absolute under /api and there is no
// trailing-slash alias to mount.
//
// ---------------------------------------------------------------------------
// SEAMS (Phase 7 must wire these to services/traffic-scheduler.ts)
// ---------------------------------------------------------------------------
// The TypeScript dynamically imports three scheduler functions. Their defaults
// below FAIL EXPLICITLY: a missing scheduler must never look like a successful
// reconciliation, because the same request paths move traffic counters and (on the
// monthly reset) money.
//
//	TrafficReconcileInstanceIDs  ← reconcileTrafficStateForInstanceIds(instanceIds)
//	TrafficReconcileUser         ← reconcileTrafficStateForUser(userId)
//	TrafficRunJob                ← runTrafficJob()
//
// Failure handling is NOT uniform in the TypeScript, and each call site below
// matches it:
//
//   - PUT /users/:userId/traffic/limit, PUT /instances/:id/traffic/limit and
//     POST /instances/:id/traffic/reset await the reconciliation and let a
//     failure abort the handler (500).
//   - POST /traffic/sync-package-limits catches the failure and only logs a warn,
//     so the sync still reports success.
//   - POST /traffic/collect starts runTrafficJob() fire-and-forget and returns
//     immediately.
//
// Everything money-adjacent is ported for real: the lock nesting lives in
// db.WithInstanceTrafficResetLock + db.ResetInstanceTrafficForOwnerInTransaction
// (table lock instance → sample → table lock user → transaction → advisory 4121 →
// advisory 4110 → balance charge + ledger → snapshot advance → counters reset).
package routes

import (
	"context"
	"errors"
	"log"
	"math"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// ==================== seams ====================

// errTrafficSchedulerUnavailable is the explicit "not wired yet" error every
// scheduler seam returns until Phase 7 ports services/traffic-scheduler.ts.
var errTrafficSchedulerUnavailable = errors.New(
	"traffic scheduler 未接线（server/src/services/traffic-scheduler.ts 属 Phase 7 worker）")

// TrafficReconcileInstanceIDs mirrors
// services/traffic-scheduler.reconcileTrafficStateForInstanceIds.
// SEAM: Phase 7 worker.
var TrafficReconcileInstanceIDs = func(ctx context.Context, instanceIDs []int32) error {
	return errTrafficSchedulerUnavailable
}

// TrafficReconcileUser mirrors services/traffic-scheduler.reconcileTrafficStateForUser.
// SEAM: Phase 7 worker.
var TrafficReconcileUser = func(ctx context.Context, userID int32) error {
	return errTrafficSchedulerUnavailable
}

// TrafficRunJob mirrors services/traffic-scheduler.runTrafficJob. The TypeScript
// takes no arguments; ctx is added so the Phase 7 job can honour cancellation.
// SEAM: Phase 7 worker.
var TrafficRunJob = func(ctx context.Context) error {
	return errTrafficSchedulerUnavailable
}

// ==================== helpers ====================

// trafficRespondInternal mirrors app.ts' generic error handler for an unhandled
// throw. The public text never carries internals (production wording, which is the
// convention every other port in this repo follows).
func trafficRespondInternal(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// trafficParseInt32RouteID mirrors parsePositiveId's intent (positive decimal,
// JavaScript safe integer) and then the Prisma Int range: an id that parses but
// exceeds int32 cannot address an Int column (P2020), and the Node route does not
// catch that error, so it surfaces as a 500.
//
// It uses lib.ParsePositiveRouteID, per the migration rules, which is stricter than
// traffic.ts' local helper in exactly one respect: `^[1-9]\d*$` rejects a
// leading-zero id such as "01", which the TypeScript's `^\d+$` + Number() would
// accept as 1. Zero, negative, fractional and non-numeric ids behave identically
// in both implementations. The strict parser is what all other ports use and what
// the route-id guard tests pin, so the divergence is deliberate; no conformance
// case exercises a leading-zero id.
func trafficParseInt32RouteID(value string) (id int32, ok bool, outOfRange bool) {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok {
		return 0, false, false
	}
	if parsed > math.MaxInt32 {
		return 0, false, true
	}
	return int32(parsed), true, false
}

// trafficSerializeBigInt mirrors serializeBigInt: traffic counters cross the wire
// as strings, never as JSON numbers.
func trafficSerializeBigInt(value *int64) any {
	if value == nil {
		return nil
	}
	return formatBigInt(*value)
}

// trafficSerializeBigIntValue is serializeBigInt for a non-nullable counter.
func trafficSerializeBigIntValue(value int64) string {
	return formatBigInt(value)
}

// formatBigInt renders an int64 as a decimal string (BigInt#toString).
func formatBigInt(value int64) string {
	return big.NewInt(value).String()
}

// trafficLimitFormatted mirrors `limit ? formatBytes(limit) : null`. The
// TypeScript tests truthiness of a bigint, so a zero limit formats as null while
// still serializing as "0".
func trafficLimitFormatted(limit *int64) any {
	if limit == nil || *limit == 0 {
		return nil
	}
	return services.FormatBytes(*limit)
}

// trafficResetDayOrDefault mirrors `host?.traffic_reset_day ?? 1`.
func trafficResetDayOrDefault(host *db.TrafficHost) int32 {
	if host == nil {
		return 1
	}
	return host.TrafficResetDay
}

// trafficReadMonthlyLimit mirrors `const { monthlyLimit } = request.body` plus
// parseNullablePostgresBigIntInput. ok=false means the response was already
// written.
func trafficReadMonthlyLimit(c *gin.Context) (limit *int64, ok bool) {
	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return nil, false
	}
	if body.Value == nil {
		// `request.body` is null/undefined in the TypeScript and destructuring it
		// throws, which app.ts renders as a 500.
		trafficRespondInternal(c)
		return nil, false
	}

	var raw any
	if object, isObject := body.Value.(map[string]any); isObject {
		raw = object["monthlyLimit"]
	}

	parsed, present, valid := lib.ParseNullablePostgresBigIntInput(raw)
	if !valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid monthly traffic limit")
		return nil, false
	}
	if !present {
		// null / undefined / "" all mean "set the column to NULL".
		return nil, true
	}
	converted := int64(parsed)
	return &converted, true
}

// trafficVipExtraTrafficQuota mirrors getVipExtraTrafficQuota from
// services/traffic-utils.ts.
func trafficVipExtraTrafficQuota(ctx context.Context, userID int32, baseLimit *int64) (int64, error) {
	_, benefit, err := services.VipGetUserContinuousVipBenefit(ctx, userID, nil)
	if err != nil {
		return 0, err
	}
	var base *big.Int
	if baseLimit != nil {
		base = big.NewInt(*baseLimit)
	}
	return services.VipCalculateExtraTraffic(base, benefit.ExtraTrafficPercent).Int64(), nil
}

// trafficUserSummary mirrors the response body shared by GET /me/traffic and
// GET /users/:userId/traffic.
func trafficUserSummary(quota *db.UserTrafficQuota) gin.H {
	effectiveLimit := lib.GetEffectiveLimit(quota.MonthlyTrafficLimit, quota.ExtraTrafficQuota)
	return gin.H{
		"monthlyUsed":           trafficSerializeBigIntValue(quota.MonthlyTrafficUsed),
		"monthlyUsedFormatted":  services.FormatBytes(quota.MonthlyTrafficUsed),
		"monthlyLimit":          trafficSerializeBigInt(effectiveLimit),
		"monthlyLimitFormatted": trafficLimitFormatted(effectiveLimit),
		"extraQuota":            trafficSerializeBigIntValue(quota.ExtraTrafficQuota),
		"trafficStatus":         string(quota.TrafficStatus),
		"percentage":            lib.CalculatePercentage(quota.MonthlyTrafficUsed, effectiveLimit),
	}
}

// trafficLoadInstanceForView mirrors the instance + permission preamble shared by
// GET /instances/:id/traffic and GET /instances/:id/traffic/history:
// admin, instance owner or host owner may read; a host lookup is performed for the
// host owner check and reused for the reset day.
func trafficLoadInstanceForView(c *gin.Context, instanceID int32) (*db.InstanceTrafficInfo, *db.TrafficHost, bool) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	instance, err := db.GetInstanceTrafficInfo(ctx, instanceID)
	if err != nil {
		trafficRespondInternal(c)
		return nil, nil, false
	}
	if instance == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Instance not found"})
		return nil, nil, false
	}

	var host *db.TrafficHost
	if claims.Role == "admin" || instance.UserID == claims.ID {
		// Admin and owner pass without a host lookup.
	} else if instance.HostID != nil {
		host, err = db.TrafficGetHostByID(ctx, *instance.HostID)
		if err != nil {
			trafficRespondInternal(c)
			return nil, nil, false
		}
		if host == nil || host.UserID != claims.ID {
			c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
			return nil, nil, false
		}
	} else {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
		return nil, nil, false
	}

	// `if (!host && instance.hostId) host = await db.getHostById(...)`.
	if host == nil && instance.HostID != nil {
		host, err = db.TrafficGetHostByID(ctx, *instance.HostID)
		if err != nil {
			trafficRespondInternal(c)
			return nil, nil, false
		}
	}
	return instance, host, true
}

// ==================== 用户流量 API ====================

// trafficGetMyTraffic mirrors GET /api/me/traffic.
func trafficGetMyTraffic(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	quota, err := db.GetUserTrafficQuota(ctx, claims.ID)
	if err != nil {
		trafficRespondInternal(c)
		return
	}
	if quota == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User quota not found"})
		return
	}
	c.JSON(http.StatusOK, trafficUserSummary(quota))
}

// trafficGetUserTraffic mirrors GET /api/users/:userId/traffic (admin).
func trafficGetUserTraffic(c *gin.Context) {
	ctx := c.Request.Context()

	userID, ok, outOfRange := trafficParseInt32RouteID(c.Param("userId"))
	if outOfRange {
		trafficRespondInternal(c)
		return
	}
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	quota, err := db.GetUserTrafficQuota(ctx, userID)
	if err != nil {
		trafficRespondInternal(c)
		return
	}
	if quota == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User quota not found"})
		return
	}
	c.JSON(http.StatusOK, trafficUserSummary(quota))
}

// trafficUpdateUserTrafficLimit mirrors PUT /api/users/:userId/traffic/limit (admin).
func trafficUpdateUserTrafficLimit(c *gin.Context) {
	ctx := c.Request.Context()

	userID, ok, outOfRange := trafficParseInt32RouteID(c.Param("userId"))
	if outOfRange {
		trafficRespondInternal(c)
		return
	}
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid user ID"})
		return
	}

	limit, ok := trafficReadMonthlyLimit(c)
	if !ok {
		return
	}

	if err := db.UpdateUserTrafficLimit(ctx, userID, limit); err != nil {
		trafficRespondInternal(c)
		return
	}

	quota, err := db.GetUserTrafficQuota(ctx, userID)
	if err != nil {
		trafficRespondInternal(c)
		return
	}
	if quota != nil {
		effectiveLimit := lib.GetEffectiveLimit(quota.MonthlyTrafficLimit, quota.ExtraTrafficQuota)
		status := lib.CalculateUserTrafficStatus(quota.MonthlyTrafficUsed, effectiveLimit)
		if err := db.UpdateUserTrafficStatus(ctx, userID, models.TrafficStatus(status)); err != nil {
			trafficRespondInternal(c)
			return
		}
	}

	// Fatal in the TypeScript: the await rejects and the handler aborts.
	if err := TrafficReconcileUser(ctx, userID); err != nil {
		log.Printf("[Traffic] reconcileTrafficStateForUser(%d) failed: %v", userID, err)
		trafficRespondInternal(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ==================== 实例流量 API ====================

// trafficGetInstanceTraffic mirrors GET /api/instances/:id/traffic.
func trafficGetInstanceTraffic(c *gin.Context) {
	ctx := c.Request.Context()

	instanceID, ok, outOfRange := trafficParseInt32RouteID(c.Param("id"))
	if outOfRange {
		trafficRespondInternal(c)
		return
	}
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid instance ID"})
		return
	}

	instance, host, ok := trafficLoadInstanceForView(c, instanceID)
	if !ok {
		return
	}

	trafficResetDay := trafficResetDayOrDefault(host)
	periodStart, periodEnd := lib.GetTrafficPeriod(int(trafficResetDay), time.Now().UTC())

	vipExtraTrafficQuota, err := trafficVipExtraTrafficQuota(ctx, instance.UserID, instance.MonthlyTrafficLimit)
	if err != nil {
		trafficRespondInternal(c)
		return
	}
	effectiveInstanceLimit := lib.GetEffectiveLimit(instance.MonthlyTrafficLimit, vipExtraTrafficQuota)

	c.JSON(http.StatusOK, gin.H{
		"monthlyUsed":           trafficSerializeBigIntValue(instance.MonthlyTrafficUsed),
		"monthlyUsedFormatted":  services.FormatBytes(instance.MonthlyTrafficUsed),
		"monthlyLimit":          trafficSerializeBigInt(effectiveInstanceLimit),
		"monthlyLimitFormatted": trafficLimitFormatted(effectiveInstanceLimit),
		"vipExtraQuota":         trafficSerializeBigIntValue(vipExtraTrafficQuota),
		"trafficStatus":         string(instance.TrafficStatus),
		"percentage":            lib.CalculatePercentage(instance.MonthlyTrafficUsed, effectiveInstanceLimit),
		"trafficResetDay":       trafficResetDay,
		"trafficResetPrice":     instance.TrafficResetPriceYuan(),
		"periodStart":           lib.FormatLocalDate(periodStart),
		"periodEnd":             lib.FormatLocalDate(periodEnd),
	})
}

// trafficGetInstanceTrafficHistory mirrors
// GET /api/instances/:id/traffic/history.
func trafficGetInstanceTrafficHistory(c *gin.Context) {
	ctx := c.Request.Context()

	instanceID, ok, outOfRange := trafficParseInt32RouteID(c.Param("id"))
	if outOfRange {
		trafficRespondInternal(c)
		return
	}
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid instance ID"})
		return
	}

	_, host, ok := trafficLoadInstanceForView(c, instanceID)
	if !ok {
		return
	}

	trafficResetDay := trafficResetDayOrDefault(host)
	periodStart, periodEnd := lib.GetTrafficPeriod(int(trafficResetDay), time.Now().UTC())

	// `const days = request.query.days ? parsePositiveId(request.query.days) : null`.
	// A repeated parameter is an array in Fastify and fails the digit test, so it
	// is treated as absent (singleQueryValue returns "").
	var days *int
	if raw := singleQueryValue(c.Request.URL.Query(), "days"); raw != "" {
		if parsed, valid := lib.ParsePositiveRouteID(raw); valid {
			converted := int(parsed)
			days = &converted
		}
	}

	var history []models.DailyTraffic
	var err error
	if days != nil {
		history, err = db.GetDailyTraffic(ctx, instanceID, *days)
	} else {
		history, err = db.GetDailyTrafficByPeriod(ctx, instanceID, periodStart)
	}
	if err != nil {
		trafficRespondInternal(c)
		return
	}

	data := make([]gin.H, 0, len(history))
	for _, record := range history {
		total := record.RxTotal + record.TxTotal
		data = append(data, gin.H{
			"date":           lib.FormatLocalDate(record.Date),
			"rxTotal":        trafficSerializeBigIntValue(record.RxTotal),
			"txTotal":        trafficSerializeBigIntValue(record.TxTotal),
			"rxFormatted":    services.FormatBytes(record.RxTotal),
			"txFormatted":    services.FormatBytes(record.TxTotal),
			"total":          trafficSerializeBigIntValue(total),
			"totalFormatted": services.FormatBytes(total),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"trafficResetDay": trafficResetDay,
		"periodStart":     lib.FormatLocalDate(periodStart),
		"periodEnd":       lib.FormatLocalDate(periodEnd),
		"data":            data,
	})
}

// trafficUpdateInstanceTrafficLimit mirrors
// PUT /api/instances/:id/traffic/limit (admin).
func trafficUpdateInstanceTrafficLimit(c *gin.Context) {
	ctx := c.Request.Context()

	instanceID, ok, outOfRange := trafficParseInt32RouteID(c.Param("id"))
	if outOfRange {
		trafficRespondInternal(c)
		return
	}
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid instance ID"})
		return
	}

	limit, ok := trafficReadMonthlyLimit(c)
	if !ok {
		return
	}

	if err := db.UpdateInstanceTrafficLimit(ctx, instanceID, limit); err != nil {
		trafficRespondInternal(c)
		return
	}

	instance, err := db.GetInstanceTrafficInfo(ctx, instanceID)
	if err != nil {
		trafficRespondInternal(c)
		return
	}
	if instance != nil {
		vipExtraTrafficQuota, err := trafficVipExtraTrafficQuota(ctx, instance.UserID, instance.MonthlyTrafficLimit)
		if err != nil {
			trafficRespondInternal(c)
			return
		}
		effectiveInstanceLimit := lib.GetEffectiveLimit(instance.MonthlyTrafficLimit, vipExtraTrafficQuota)
		status := lib.CalculateInstanceTrafficStatus(instance.MonthlyTrafficUsed, effectiveInstanceLimit)
		if err := db.UpdateInstanceTrafficStatus(ctx, instanceID, models.TrafficStatus(status)); err != nil {
			trafficRespondInternal(c)
			return
		}
	}

	// Fatal in the TypeScript.
	if err := TrafficReconcileInstanceIDs(ctx, []int32{instanceID}); err != nil {
		log.Printf("[Traffic] reconcileTrafficStateForInstanceIds([%d]) failed: %v", instanceID, err)
		trafficRespondInternal(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

// trafficResetInstanceTraffic mirrors POST /api/instances/:id/traffic/reset.
//
// Two paths: an admin or host owner resets without charging, while the instance
// owner (who is not an admin) goes through the locked charge-and-reset transaction.
func trafficResetInstanceTraffic(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	instanceID, ok, outOfRange := trafficParseInt32RouteID(c.Param("id"))
	if outOfRange {
		trafficRespondInternal(c)
		return
	}
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid instance ID"})
		return
	}

	instance, err := db.GetInstanceTrafficInfo(ctx, instanceID)
	if err != nil {
		trafficRespondInternal(c)
		return
	}
	if instance == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Instance not found"})
		return
	}

	isAdmin := claims.Role == "admin"
	isInstanceOwner := instance.UserID == claims.ID
	if !isAdmin && !isInstanceOwner {
		if instance.HostID == nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
			return
		}
		host, err := db.TrafficGetHostByID(ctx, *instance.HostID)
		if err != nil {
			trafficRespondInternal(c)
			return
		}
		if host == nil || host.UserID != claims.ID {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "Only admin, host owner, or instance owner can reset instance traffic",
			})
			return
		}
	}

	price := 0.0
	shouldChargeOwner := isInstanceOwner && !isAdmin
	if shouldChargeOwner {
		result, err := db.WithInstanceTrafficResetLock(ctx, instanceID,
			func(txCtx context.Context, tx pgx.Tx, sample db.TrafficSnapshotSample) (float64, error) {
				return db.ResetInstanceTrafficForOwnerInTransaction(txCtx, tx, instanceID, claims.ID, sample)
			})
		if err != nil {
			message := err.Error()
			// `message.includes('余额不足') ? 400 : message.includes('正在处理') ? 409 : 400`
			statusCode := http.StatusBadRequest
			if strings.Contains(message, "正在处理") && !strings.Contains(message, "余额不足") {
				statusCode = http.StatusConflict
			}
			c.JSON(statusCode, gin.H{"error": message})
			return
		}
		price = result
	} else {
		// Errors propagate to the generic handler in the TypeScript.
		if _, err := db.ResetInstanceMonthlyTraffic(ctx, instanceID); err != nil {
			trafficRespondInternal(c)
			return
		}
	}

	// Fatal in the TypeScript.
	if err := TrafficReconcileInstanceIDs(ctx, []int32{instanceID}); err != nil {
		log.Printf("[Traffic] reconcileTrafficStateForInstanceIds([%d]) failed: %v", instanceID, err)
		trafficRespondInternal(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Instance traffic reset completed",
		"price":   price,
	})
}

// ==================== 管理员操作 ====================

// trafficGetHostTrafficHistory mirrors GET /api/hosts/:hostId/traffic/history.
func trafficGetHostTrafficHistory(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	hostID, ok, outOfRange := trafficParseInt32RouteID(c.Param("hostId"))
	if outOfRange {
		trafficRespondInternal(c)
		return
	}
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid host ID"})
		return
	}

	host, err := db.TrafficGetHostByID(ctx, hostID)
	if err != nil {
		trafficRespondInternal(c)
		return
	}
	if host == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Host not found"})
		return
	}

	if claims.Role != "admin" && host.UserID != claims.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	trafficResetDay := trafficResetDayOrDefault(host)
	periodStart, periodEnd := lib.GetTrafficPeriod(int(trafficResetDay), time.Now().UTC())

	history, err := db.GetHostDailyTraffic(ctx, hostID, int(trafficResetDay))
	if err != nil {
		trafficRespondInternal(c)
		return
	}
	summary, err := db.GetHostTrafficSummary(ctx, hostID)
	if err != nil {
		trafficRespondInternal(c)
		return
	}

	data := make([]gin.H, 0, len(history))
	for _, record := range history {
		total := record.RxTotal + record.TxTotal
		data = append(data, gin.H{
			"date":           lib.FormatLocalDate(record.Date),
			"rxTotal":        trafficSerializeBigIntValue(record.RxTotal),
			"txTotal":        trafficSerializeBigIntValue(record.TxTotal),
			"rxFormatted":    services.FormatBytes(record.RxTotal),
			"txFormatted":    services.FormatBytes(record.TxTotal),
			"total":          trafficSerializeBigIntValue(total),
			"totalFormatted": services.FormatBytes(total),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"trafficResetDay": trafficResetDay,
		"periodStart":     lib.FormatLocalDate(periodStart),
		"periodEnd":       lib.FormatLocalDate(periodEnd),
		"data":            data,
		"summary": gin.H{
			"totalUsed":           trafficSerializeBigIntValue(summary.TotalUsed),
			"totalUsedFormatted":  services.FormatBytes(summary.TotalUsed),
			"totalLimit":          trafficSerializeBigIntValue(summary.TotalLimit),
			"totalLimitFormatted": services.FormatBytes(summary.TotalLimit),
		},
	})
}

// trafficCollect mirrors POST /api/traffic/collect (admin).
//
// The TypeScript starts the job with `runTrafficJob().catch(console.error)` and
// returns immediately, so the endpoint reports success even when the job fails.
func trafficCollect(c *gin.Context) {
	// The request context is cancelled as soon as the response is written, so the
	// fire-and-forget job must not inherit it.
	jobCtx := context.Background()
	go func() {
		if err := TrafficRunJob(jobCtx); err != nil {
			log.Printf("[Traffic] traffic collection job failed: %v", err)
		}
	}()

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Traffic collection job started",
	})
}

// trafficSyncPackageLimits mirrors POST /api/traffic/sync-package-limits (admin).
func trafficSyncPackageLimits(c *gin.Context) {
	ctx := c.Request.Context()

	result, err := db.SyncPackageTrafficLimitsToInstances(ctx, nil)
	if err != nil {
		trafficRespondInternal(c)
		return
	}

	if len(result.InstanceIDs) > 0 {
		// Non-fatal in the TypeScript: the failure is only logged.
		if err := TrafficReconcileInstanceIDs(ctx, result.InstanceIDs); err != nil {
			log.Printf("[Traffic] Package traffic limits synced, immediate traffic state reconciliation failed: %v", err)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"message":      "Package traffic limits synced to instances",
		"updatedCount": result.Count,
	})
}

// RegisterTraffic mounts the traffic routes under /api, matching
// `fastify.register(trafficRoutes, { prefix: '/api' })`.
//
// Gin requires one wildcard name for every route sharing a path prefix, and the
// already-ported instance modules register "/api/instances/:id/...", so the
// instance routes below spell the parameter ":id" even though traffic.ts calls it
// ":instanceId". The hosts tree already uses ":hostId" and /users/:userId is unique
// to this module, so those keep the TypeScript names.
func RegisterTraffic(r *gin.Engine) {
	group := r.Group("/api")

	// ==================== 用户流量 API ====================
	group.GET("/me/traffic", middleware.AuthenticateUser(), trafficGetMyTraffic)
	group.GET("/users/:userId/traffic", middleware.AuthenticateAdmin(), trafficGetUserTraffic)
	group.PUT("/users/:userId/traffic/limit", middleware.AuthenticateAdmin(), trafficUpdateUserTrafficLimit)

	// ==================== 实例流量 API ====================
	group.GET("/instances/:id/traffic", middleware.Authenticate(), trafficGetInstanceTraffic)
	group.GET("/instances/:id/traffic/history", middleware.Authenticate(), trafficGetInstanceTrafficHistory)
	group.PUT("/instances/:id/traffic/limit", middleware.AuthenticateAdmin(), trafficUpdateInstanceTrafficLimit)
	group.POST("/instances/:id/traffic/reset", middleware.Authenticate(), trafficResetInstanceTraffic)

	// ==================== 管理员操作 ====================
	group.GET("/hosts/:hostId/traffic/history", middleware.Authenticate(), trafficGetHostTrafficHistory)
	group.POST("/traffic/collect", middleware.AuthenticateAdmin(), trafficCollect)
	group.POST("/traffic/sync-package-limits", middleware.AuthenticateAdmin(), trafficSyncPackageLimits)
}

func init() {
	Register(RegisterTraffic)
}
