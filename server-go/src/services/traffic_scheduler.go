// Port of server/src/services/traffic-scheduler.ts.
//
// Responsibilities, in the order the job executes them:
//
//  1. every 3 minutes collect per-instance traffic counters from each online host
//     (incremental: only the instance lock protects the read-and-commit, so a
//     second replica cannot double count);
//  2. fold the collected deltas into an in-memory usage map so the quota decision
//     does not need a second SELECT;
//  3. throttle instances/users over their effective limit, restore bandwidth for
//     instances that came back under it, and raise 80% warnings;
//  4. at 00:05 Asia/Shanghai reset the monthly counters of hosts whose
//     traffic_reset_day is today;
//  5. at 03:00 / 03:30 / 04:00 prune daily traffic, notification logs, refresh
//     tokens, verification codes, distributed locks, operation verifications,
//     token invalidations and 90-day-old operation logs.
//
// Behaviours replicated deliberately:
//
//   - JOB_TIMEOUT is advisory. The TypeScript's withTimeout rejects the promise
//     while the work keeps running; there is no cancellation. Cancelling here
//     would abort in-flight DB writes mid-transaction, so the Go job also only
//     stops waiting (see RunTrafficJob).
//   - HOST_CONCURRENCY (3) and CONCURRENCY_PER_HOST (10) are both read from the
//     environment on every call, exactly like the TypeScript module-level
//     `process.env` reads. The defaults are the post-incident values; do not raise
//     them without measuring a real fleet.
//   - the warning claim is a lease: if delivery fails the claim is released so the
//     next tick can retry. Dropping that rollback would silence the warning for a
//     whole month.
package services

import (
	"context"
	"log"
	"math/big"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// TrafficScheduler tuning, mirroring the TypeScript constants.
const (
	// TrafficSchedulerJobTimeout is JOB_TIMEOUT_MS: 15 minutes.
	TrafficSchedulerJobTimeout = 15 * time.Minute
	// TrafficSchedulerDefaultConcurrencyPerHost is the post-incident default (was
	// 100 before the production outage).
	TrafficSchedulerDefaultConcurrencyPerHost = 10
	// TrafficSchedulerDefaultHostConcurrency is how many hosts are processed at
	// the same time.
	TrafficSchedulerDefaultHostConcurrency = 3
	// TrafficSchedulerCollectWaitTimeoutMs / RetryIntervalMs / ExpireMs mirror the
	// override object passed to collectTrafficForInstanceWithClient.
	TrafficSchedulerCollectWaitTimeoutMs = 200
	TrafficSchedulerCollectRetryMs       = 50
	TrafficSchedulerCollectExpireMs      = 30000
)

// NOTE: the TypeScript traffic scheduler has NO `isRunning` flag. Overlapping
// ticks are allowed and made safe by the per-instance traffic lock inside
// collectTrafficForInstanceWithClient; the 15-minute timeout only stops waiting.
// No Go-side overlap guard is added here on purpose (faithfulness over "fixing").

// trafficSchedulerNow is Date.now() in UTC; tests move it.
var trafficSchedulerNow = func() time.Time { return time.Now().UTC() }

func trafficSchedulerPositiveIntEnv(name string, fallback int) int {
	parsed, err := strconv.Atoi(os.Getenv(name))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// TrafficSchedulerConcurrencyPerHost mirrors CONCURRENCY_PER_HOST.
func TrafficSchedulerConcurrencyPerHost() int {
	return trafficSchedulerPositiveIntEnv("TRAFFIC_CONCURRENCY_PER_HOST", TrafficSchedulerDefaultConcurrencyPerHost)
}

// TrafficSchedulerHostConcurrency mirrors HOST_CONCURRENCY.
func TrafficSchedulerHostConcurrency() int {
	return trafficSchedulerPositiveIntEnv("TRAFFIC_HOST_CONCURRENCY", TrafficSchedulerDefaultHostConcurrency)
}

// ==================== next-run (pure) ====================

// TrafficSchedulerNextRun mirrors `schedule('*/3 * * * *')`: the next multiple of
// three minutes on the UTC clock, never the current instant.
func TrafficSchedulerNextRun(now time.Time) time.Time {
	return schedulerEveryNMinutesNextRun(now, 3)
}

// TrafficSchedulerDailyResetNextRun mirrors `schedule('5 0 * * *', …,
// { timezone: TRAFFIC_TIME_ZONE })`.
func TrafficSchedulerDailyResetNextRun(now time.Time) time.Time {
	return schedulerDailyInLocationNextRun(now, trafficLocation(), 0, 5)
}

// TrafficSchedulerCleanupNextRun mirrors `schedule('0 3 * * *')`.
func TrafficSchedulerCleanupNextRun(now time.Time) time.Time {
	return schedulerDailyUTCNextRun(now, 3, 0)
}

// TrafficSchedulerNotificationCleanupNextRun mirrors `schedule('30 3 * * *')`.
func TrafficSchedulerNotificationCleanupNextRun(now time.Time) time.Time {
	return schedulerDailyUTCNextRun(now, 3, 30)
}

// TrafficSchedulerSystemCleanupNextRun mirrors `schedule('0 4 * * *')`.
func TrafficSchedulerSystemCleanupNextRun(now time.Time) time.Time {
	return schedulerDailyUTCNextRun(now, 4, 0)
}

// ==================== host client ====================

// trafficSchedulerGetHostClient mirrors getHostClient. A host without both
// certificate paths is skipped: Incus always requires mTLS, and the pool would
// fail anyway with a less actionable error.
func trafficSchedulerGetHostClient(ctx context.Context, host *db.RunningTrafficHost) *lib.IncusClient {
	if host == nil || host.CertPath == nil || *host.CertPath == "" ||
		host.KeyPath == nil || *host.KeyPath == "" {
		return nil
	}
	start := time.Now()
	client, err := lib.GetIncusClientFromPool(ctx, lib.IncusClientOptions{
		URL:      host.URL,
		CertPath: host.CertPath,
		KeyPath:  host.KeyPath,
		HostID:   int64(host.ID),
	})
	if err != nil {
		log.Printf("[Traffic] Failed to get client for host %d: %v", host.ID, err)
		lib.RecordIncusClientError(int64(host.ID))
		return nil
	}
	lib.UpdateIncusClientResponseTime(int64(host.ID), time.Since(start))
	return client
}

// ==================== per-instance decisions ====================

// trafficSchedulerQuotaDecision is the shared arithmetic of checkAndThrottle and
// checkAndRestore:
//
//	userEffectiveLimit      = userLimit + userExtraQuota
//	instanceBaseWithVip     = instanceLimit + vipExtraTraffic
//	instanceLimit           = instanceBaseWithVip + userExtraQuota
//	instanceUsesExtraTraffic= instanceBaseWithVip != nil && used >= instanceBaseWithVip
//	extraTrafficExhausted   = userExtraQuota > 0 && userExtraUsed >= userExtraQuota
type trafficSchedulerQuotaDecision struct {
	UserEffectiveLimit       *int64
	InstanceLimit            *int64
	InstanceUsesExtraTraffic bool
	ExtraTrafficExhausted    bool
}

// TrafficSchedulerEvaluateQuota is the pure arithmetic above. Keeping it exported
// lets the boundary tests cover null limits and the extra-quota interaction
// without a database.
func TrafficSchedulerEvaluateQuota(
	userLimit *int64,
	userExtraQuota int64,
	userExtraUsed int64,
	instanceLimit *int64,
	instanceUsed int64,
	vipExtraTraffic int64,
) trafficSchedulerQuotaDecision {
	userEffective := lib.GetEffectiveLimit(userLimit, userExtraQuota)
	baseWithVip := lib.GetEffectiveLimit(instanceLimit, vipExtraTraffic)
	instanceEffective := lib.GetEffectiveLimit(baseWithVip, userExtraQuota)
	return trafficSchedulerQuotaDecision{
		UserEffectiveLimit:       userEffective,
		InstanceLimit:            instanceEffective,
		InstanceUsesExtraTraffic: baseWithVip != nil && instanceUsed >= *baseWithVip,
		ExtraTrafficExhausted:    userExtraQuota > 0 && userExtraUsed >= userExtraQuota,
	}
}

// TrafficSchedulerUserOverLimit mirrors `isOverLimit(userUsed, userEffective)`.
func (d trafficSchedulerQuotaDecision) UserOverLimit(userUsed int64) bool {
	return lib.IsOverLimit(userUsed, d.UserEffectiveLimit)
}

// TrafficSchedulerInstanceOverLimit mirrors the instance-level predicate,
// including the extra-quota exhaustion clause.
func (d trafficSchedulerQuotaDecision) InstanceOverLimit(instanceUsed int64) bool {
	return lib.IsOverLimit(instanceUsed, d.InstanceLimit) ||
		(d.InstanceUsesExtraTraffic && d.ExtraTrafficExhausted)
}

// TrafficSchedulerVipExtraTraffic mirrors getVipExtraTrafficQuota from
// services/traffic-utils.ts (routing helper in routes/traffic.go does the same
// three lines; duplicating them here avoids services → routes, which is illegal).
func TrafficSchedulerVipExtraTraffic(ctx context.Context, userID int32, baseLimit *int64) (int64, error) {
	_, benefit, err := VipGetUserContinuousVipBenefit(ctx, userID, nil)
	if err != nil {
		return 0, err
	}
	base := baseLimit
	return VipCalculateExtraTraffic(int64PointerToBig(base), benefit.ExtraTrafficPercent).Int64(), nil
}

// ==================== reconciliation ====================

// trafficSchedulerContext is the mutable copy of an instance used to evaluate the
// post-collection usage. The TypeScript builds it with object spread so the
// original DB row stays untouched; the same is true here.
type trafficSchedulerContext struct {
	Instance     db.RunningTrafficInstance
	InstanceUsed int64
	UserUsed     int64
	ExtraUsed    int64
}

// trafficSchedulerCheckAndThrottle mirrors checkAndThrottle.
func trafficSchedulerCheckAndThrottle(ctx context.Context, candidate trafficSchedulerContext, client *lib.IncusClient) {
	instance := candidate.Instance
	if instance.User == nil || instance.User.Quota == nil || instance.Host == nil {
		return
	}
	quota := instance.User.Quota

	vipExtra, err := TrafficSchedulerVipExtraTraffic(ctx, instance.UserID, instance.MonthlyTrafficLimit)
	if err != nil {
		log.Printf("[Traffic] Failed to resolve VIP traffic quota for user %d: %v", instance.UserID, err)
		return
	}
	decision := TrafficSchedulerEvaluateQuota(
		quota.MonthlyTrafficLimit, quota.ExtraTrafficQuota, candidate.ExtraUsed,
		instance.MonthlyTrafficLimit, candidate.InstanceUsed, vipExtra,
	)

	userOver := decision.UserOverLimit(candidate.UserUsed)
	instanceOver := decision.InstanceOverLimit(candidate.InstanceUsed)
	if !userOver && !instanceOver {
		// Under the limit: raise the 80% warnings. Warnings deliberately do not run
		// for instances that are already throttled — the throttle notification is the
		// more urgent message.
		trafficSchedulerRaiseWarnings(ctx, candidate, decision)
		return
	}

	shouldNotifyThrottled, err := db.MarkInstanceTrafficLimitedIfNeeded(ctx, instance.ID)
	if err != nil {
		log.Printf("[Traffic] Failed to mark instance %d as LIMITED: %v", instance.ID, err)
		return
	}
	if _, err := TrafficBandwidthReconcileEffective(ctx, instance.ID, client); err != nil {
		log.Printf("[Traffic] Failed to reconcile bandwidth for instance %d: %v", instance.ID, err)
		return
	}
	if userOver && quota.TrafficStatus != models.TrafficStatusLIMITED {
		if err := db.UpdateUserTrafficStatus(ctx, instance.UserID, models.TrafficStatusLIMITED); err != nil {
			log.Printf("[Traffic] Failed to mark user %d as LIMITED: %v", instance.UserID, err)
		}
	}
	if shouldNotifyThrottled {
		sendTrafficThrottledNotification(ctx, instance.UserID, instance.Name, instance.Host.Name)
	}
	log.Printf("[Traffic] Reconciled throttled instance %d (%s)", instance.ID, instance.IncusID)
}

// trafficSchedulerRaiseWarnings mirrors the warning half of checkAndThrottle.
//
// The claim order matters: mark → send → on failure release. A crash between mark
// and send silences one month of warnings, which the TypeScript accepts; releasing
// on a *delivery* failure is the retry path it does implement.
func trafficSchedulerRaiseWarnings(ctx context.Context, candidate trafficSchedulerContext, decision trafficSchedulerQuotaDecision) {
	instance := candidate.Instance
	quota := instance.User.Quota

	if userWarning := lib.IsWarningThreshold(candidate.UserUsed, decision.UserEffectiveLimit); userWarning && decision.UserEffectiveLimit != nil {
		claimedAt := trafficSchedulerNow()
		shouldNotify, err := db.MarkUserTrafficWarningIfNeeded(ctx, instance.UserID, claimedAt)
		if err != nil {
			log.Printf("[Traffic] Failed to claim warning for user %d: %v", instance.UserID, err)
		} else if shouldNotify {
			delivered := sendTrafficWarningNotification(ctx, instance.UserID, candidate.UserUsed, *decision.UserEffectiveLimit, "")
			if delivered {
				log.Printf("[Traffic] Sent warning to user %d", instance.UserID)
			} else {
				if err := db.ReleaseUserTrafficWarningLease(ctx, instance.UserID, claimedAt, quota.TrafficWarningSentAt, quota.TrafficStatus); err != nil {
					log.Printf("[Traffic] Failed to release warning lease for user %d: %v", instance.UserID, err)
				}
				log.Printf("[Traffic] Warning delivery failed for user %d; claim released for retry", instance.UserID)
			}
		}
	}

	if instanceWarning := lib.IsWarningThreshold(candidate.InstanceUsed, decision.InstanceLimit); instanceWarning && decision.InstanceLimit != nil && instance.TrafficStatus == models.TrafficStatusNORMAL {
		shouldNotify, err := db.MarkInstanceTrafficWarningIfNeeded(ctx, instance.ID)
		if err != nil {
			log.Printf("[Traffic] Failed to claim instance warning for %d: %v", instance.ID, err)
			return
		}
		if !shouldNotify {
			return
		}
		delivered := sendTrafficWarningNotification(ctx, instance.UserID, candidate.InstanceUsed, *decision.InstanceLimit, instance.Name)
		if delivered {
			log.Printf("[Traffic] Sent warning for instance %d to user %d", instance.ID, instance.UserID)
			return
		}
		if err := db.ReleaseInstanceTrafficWarningLease(ctx, instance.ID); err != nil {
			log.Printf("[Traffic] Failed to release instance warning lease for %d: %v", instance.ID, err)
		}
		log.Printf("[Traffic] Warning delivery failed for instance %d; claim released for retry", instance.ID)
	}
}

// trafficSchedulerCheckAndRestore mirrors checkAndRestore.
func trafficSchedulerCheckAndRestore(ctx context.Context, candidate trafficSchedulerContext, client *lib.IncusClient) {
	instance := candidate.Instance
	if instance.User == nil || instance.User.Quota == nil {
		return
	}
	quota := instance.User.Quota

	vipExtra, err := TrafficSchedulerVipExtraTraffic(ctx, instance.UserID, instance.MonthlyTrafficLimit)
	if err != nil {
		log.Printf("[Traffic] Failed to resolve VIP traffic quota for user %d: %v", instance.UserID, err)
		return
	}
	decision := TrafficSchedulerEvaluateQuota(
		quota.MonthlyTrafficLimit, quota.ExtraTrafficQuota, candidate.ExtraUsed,
		instance.MonthlyTrafficLimit, candidate.InstanceUsed, vipExtra,
	)

	userUnder := !decision.UserOverLimit(candidate.UserUsed)
	instanceUnder := !decision.InstanceOverLimit(candidate.InstanceUsed)
	if !userUnder || !instanceUnder {
		return
	}

	if instance.TrafficStatus == models.TrafficStatusLIMITED {
		if err := db.UpdateInstanceTrafficStatus(ctx, instance.ID, models.TrafficStatusNORMAL); err != nil {
			log.Printf("[Traffic] Failed to clear LIMITED for instance %d: %v", instance.ID, err)
			return
		}
	}
	if _, err := TrafficBandwidthReconcileEffective(ctx, instance.ID, client); err != nil {
		log.Printf("[Traffic] Failed to restore bandwidth for instance %d: %v", instance.ID, err)
		return
	}

	desired := models.TrafficStatus(lib.CalculateUserTrafficStatus(candidate.UserUsed, decision.UserEffectiveLimit))
	if quota.TrafficStatus != desired {
		if err := db.UpdateUserTrafficStatus(ctx, instance.UserID, desired); err != nil {
			log.Printf("[Traffic] Failed to update user %d traffic status: %v", instance.UserID, err)
			return
		}
	}
	log.Printf("[Traffic] Restored bandwidth for instance %d", instance.ID)
}

// trafficSchedulerHostGroups mirrors the Map grouping used by reconcileTrafficState
// and executeTrafficJob: online hosts only, first-seen host order.
type trafficSchedulerHostGroup struct {
	HostID    int32
	Host      *db.RunningTrafficHost
	Instances []db.RunningTrafficInstance
}

func trafficSchedulerGroupOnline(instances []db.RunningTrafficInstance) []trafficSchedulerHostGroup {
	index := make(map[int32]int)
	groups := make([]trafficSchedulerHostGroup, 0)
	for _, instance := range instances {
		if instance.Host == nil || instance.Host.Status != models.HostStatusOnline {
			continue
		}
		position, ok := index[instance.HostID]
		if !ok {
			position = len(groups)
			index[instance.HostID] = position
			groups = append(groups, trafficSchedulerHostGroup{HostID: instance.HostID, Host: instance.Host})
		}
		groups[position].Instances = append(groups[position].Instances, instance)
	}
	return groups
}

// trafficSchedulerAsContexts converts DB rows into the mutable evaluation form.
func trafficSchedulerAsContexts(instances []db.RunningTrafficInstance) []trafficSchedulerContext {
	result := make([]trafficSchedulerContext, 0, len(instances))
	for _, instance := range instances {
		ctx := trafficSchedulerContext{Instance: instance, InstanceUsed: instance.MonthlyTrafficUsed}
		if instance.User != nil && instance.User.Quota != nil {
			ctx.UserUsed = instance.User.Quota.MonthlyTrafficUsed
			ctx.ExtraUsed = instance.User.Quota.ExtraTrafficUsed
		}
		result = append(result, ctx)
	}
	return result
}

// trafficSchedulerReconcileState mirrors reconcileTrafficState: one client per
// online host, CONCURRENCY_PER_HOST slots, throttle then restore per instance.
func trafficSchedulerReconcileState(ctx context.Context, instances []db.RunningTrafficInstance) {
	if len(instances) == 0 {
		return
	}
	perHost := TrafficSchedulerConcurrencyPerHost()
	for _, group := range trafficSchedulerGroupOnline(instances) {
		client := trafficSchedulerGetHostClient(ctx, group.Host)
		if client == nil {
			continue
		}
		semaphore := make(chan struct{}, perHost)
		var wait sync.WaitGroup
		for _, instance := range group.Instances {
			wait.Add(1)
			go func(candidate trafficSchedulerContext) {
				defer wait.Done()
				semaphore <- struct{}{}
				defer func() { <-semaphore }()
				runIsolated("Traffic", func() {
					trafficSchedulerCheckAndThrottle(ctx, candidate, client)
					trafficSchedulerCheckAndRestore(ctx, candidate, client)
				})
			}(trafficSchedulerContext{
				Instance:     instance,
				InstanceUsed: instance.MonthlyTrafficUsed,
				UserUsed:     trafficSchedulerQuotaUserUsed(instance),
				ExtraUsed:    trafficSchedulerQuotaExtraUsed(instance),
			})
		}
		wait.Wait()
	}
}

func trafficSchedulerQuotaUserUsed(instance db.RunningTrafficInstance) int64 {
	if instance.User != nil && instance.User.Quota != nil {
		return instance.User.Quota.MonthlyTrafficUsed
	}
	return 0
}

func trafficSchedulerQuotaExtraUsed(instance db.RunningTrafficInstance) int64 {
	if instance.User != nil && instance.User.Quota != nil {
		return instance.User.Quota.ExtraTrafficUsed
	}
	return 0
}

// ReconcileTrafficStateForInstanceIDs mirrors reconcileTrafficStateForInstanceIds.
func ReconcileTrafficStateForInstanceIDs(ctx context.Context, instanceIDs []int32) error {
	instances, err := db.GetRunningInstancesForTrafficByIDs(ctx, instanceIDs)
	if err != nil {
		return err
	}
	trafficSchedulerReconcileState(ctx, instances)
	return nil
}

// ReconcileTrafficStateForUser mirrors reconcileTrafficStateForUser.
func ReconcileTrafficStateForUser(ctx context.Context, userID int32) error {
	instances, err := db.GetRunningInstancesForTrafficByUserID(ctx, userID)
	if err != nil {
		return err
	}
	trafficSchedulerReconcileState(ctx, instances)
	return nil
}

// ==================== job ====================

// trafficSchedulerCollectResult mirrors the {deltas, success, error} object of
// collectHostTraffic.
type trafficSchedulerCollectResult struct {
	Deltas  []trafficSchedulerDelta
	Success int
	Errors  int
}

// trafficSchedulerDelta mirrors TrafficDelta.
type trafficSchedulerDelta struct {
	InstanceID int32
	TotalDelta int64
}

// trafficSchedulerCollectInstance mirrors collectInstanceTraffic.
func trafficSchedulerCollectInstance(ctx context.Context, instance db.RunningTrafficInstance, client *lib.IncusClient) *trafficSchedulerDelta {
	result := CollectTrafficForInstanceWithClient(ctx, CollectTrafficWithClientInput{
		InstanceID:   instance.ID,
		UserID:       instance.UserID,
		IncusID:      instance.IncusID,
		Client:       client,
		CurrentUsage: instance.MonthlyTrafficUsed,
	}, lib.LockOptions{
		WaitTimeoutMs:   intPointer(TrafficSchedulerCollectWaitTimeoutMs),
		RetryIntervalMs: intPointer(TrafficSchedulerCollectRetryMs),
		ExpireMs:        intPointer(TrafficSchedulerCollectExpireMs),
	})
	if !result.Success {
		log.Printf("[Traffic] Failed to collect traffic for instance %d: %s", instance.ID, result.Error)
		return nil
	}
	return &trafficSchedulerDelta{InstanceID: instance.ID, TotalDelta: result.TotalDelta}
}

// trafficSchedulerCollectHost mirrors collectHostTraffic.
func trafficSchedulerCollectHost(ctx context.Context, group trafficSchedulerHostGroup) trafficSchedulerCollectResult {
	client := trafficSchedulerGetHostClient(ctx, group.Host)
	if client == nil {
		return trafficSchedulerCollectResult{Errors: len(group.Instances)}
	}

	semaphore := make(chan struct{}, TrafficSchedulerConcurrencyPerHost())
	results := make([]*trafficSchedulerDelta, len(group.Instances))
	var wait sync.WaitGroup
	for index, instance := range group.Instances {
		wait.Add(1)
		go func(index int, instance db.RunningTrafficInstance) {
			defer wait.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			runIsolated("Traffic", func() {
				results[index] = trafficSchedulerCollectInstance(ctx, instance, client)
			})
		}(index, instance)
	}
	wait.Wait()

	outcome := trafficSchedulerCollectResult{Deltas: make([]trafficSchedulerDelta, 0, len(results))}
	for _, delta := range results {
		if delta == nil {
			outcome.Errors++
			continue
		}
		outcome.Deltas = append(outcome.Deltas, *delta)
		outcome.Success++
	}
	return outcome
}

// executeTrafficJob mirrors executeTrafficJob.
func executeTrafficJob(ctx context.Context, start time.Time) error {
	instances, err := db.GetRunningInstancesForTraffic(ctx)
	if err != nil {
		return err
	}
	log.Printf("[Traffic] Found %d running instances", len(instances))
	if len(instances) == 0 {
		return nil
	}

	instanceByID := make(map[int32]db.RunningTrafficInstance, len(instances))
	for _, instance := range instances {
		instanceByID[instance.ID] = instance
	}

	groups := trafficSchedulerGroupOnline(instances)

	// Hosts are processed with HOST_CONCURRENCY slots; each result carries its own
	// per-host stats so the aggregate stays deterministic.
	hostLimit := make(chan struct{}, TrafficSchedulerHostConcurrency())
	hostResults := make([]trafficSchedulerCollectResult, len(groups))
	var hostWait sync.WaitGroup
	for index, group := range groups {
		hostWait.Add(1)
		go func(index int, group trafficSchedulerHostGroup) {
			defer hostWait.Done()
			hostLimit <- struct{}{}
			defer func() { <-hostLimit }()
			hostResults[index] = trafficSchedulerCollectHost(ctx, group)
		}(index, group)
	}
	hostWait.Wait()

	deltas := make([]trafficSchedulerDelta, 0)
	successCount, errorCount := 0, 0
	for _, result := range hostResults {
		deltas = append(deltas, result.Deltas...)
		successCount += result.Success
		errorCount += result.Errors
	}

	// Which instances need a quota decision?
	//   1) every instance with a traffic delta;
	//   2) every instance already LIMITED (a quota top-up may allow a restore).
	toCheck := make(map[int32]struct{}, len(deltas))
	order := make([]int32, 0, len(deltas))
	add := func(id int32) {
		if _, seen := toCheck[id]; seen {
			return
		}
		toCheck[id] = struct{}{}
		order = append(order, id)
	}
	for _, delta := range deltas {
		add(delta.InstanceID)
	}
	for _, instance := range instances {
		if instance.TrafficStatus == models.TrafficStatusLIMITED ||
			(instance.User != nil && instance.User.Quota != nil && instance.User.Quota.TrafficStatus == models.TrafficStatusLIMITED) {
			add(instance.ID)
		}
	}

	if len(order) > 0 {
		instanceUpdatedUsage := make(map[int32]int64, len(instances))
		userUpdatedUsage := make(map[int32]int64, len(instances))
		for _, instance := range instances {
			instanceUpdatedUsage[instance.ID] = instance.MonthlyTrafficUsed
			if instance.User != nil && instance.User.Quota != nil {
				if _, ok := userUpdatedUsage[instance.UserID]; !ok {
					userUpdatedUsage[instance.UserID] = instance.User.Quota.MonthlyTrafficUsed
				}
			}
		}
		for _, delta := range deltas {
			if delta.TotalDelta <= 0 {
				continue
			}
			instance, ok := instanceByID[delta.InstanceID]
			if !ok {
				continue
			}
			instanceUpdatedUsage[delta.InstanceID] += delta.TotalDelta
			if instance.User != nil && instance.User.Quota != nil {
				userUpdatedUsage[instance.UserID] += delta.TotalDelta
			}
		}

		// extra_traffic_used is recomputed in SQL for every affected user because the
		// definition covers all of the user's non-deleted instances, not just the ones
		// with a delta this tick.
		affectedUsers := make(map[int32]struct{})
		for _, instanceID := range order {
			if instance, ok := instanceByID[instanceID]; ok && instance.User != nil && instance.User.Quota != nil {
				affectedUsers[instance.UserID] = struct{}{}
			}
		}
		extraByUser := make(map[int32]int64, len(affectedUsers))
		for userID := range affectedUsers {
			value, err := db.SyncUserExtraTrafficUsed(ctx, userID)
			if err != nil {
				log.Printf("[Traffic] Failed to sync extra traffic for user %d: %v", userID, err)
				continue
			}
			extraByUser[userID] = value
		}

		updated := make([]db.RunningTrafficInstance, 0, len(order))
		for _, instanceID := range order {
			instance, ok := instanceByID[instanceID]
			if !ok || instance.Host == nil || instance.Host.Status != models.HostStatusOnline {
				continue
			}
			instance.MonthlyTrafficUsed = instanceUpdatedUsage[instanceID]
			if instance.User != nil && instance.User.Quota != nil {
				quotaCopy := *instance.User.Quota
				quotaCopy.MonthlyTrafficUsed = userUpdatedUsage[instance.UserID]
				if extra, ok := extraByUser[instance.UserID]; ok {
					quotaCopy.ExtraTrafficUsed = extra
				}
				userCopy := *instance.User
				userCopy.Quota = &quotaCopy
				instance.User = &userCopy
			}
			updated = append(updated, instance)
		}
		trafficSchedulerReconcileState(ctx, updated)
	}

	log.Printf("[Traffic] Job completed in %dms - instances: %d, success: %d, errors: %d",
		time.Since(start).Milliseconds(), len(deltas), successCount, errorCount)
	if os.Getenv("DEBUG_TRAFFIC") == "true" {
		log.Printf("[Traffic] Connection pool status: %v", lib.GetIncusPoolStatus())
	}
	return nil
}

// RunTrafficJob mirrors runTrafficJob.
//
// The timeout is advisory (see the file comment): the execution keeps running in
// the background after the wait expires, and the returned error is swallowed by
// the caller exactly like `.catch(console.error)`.
func RunTrafficJob(ctx context.Context) {
	start := trafficSchedulerNow()
	log.Printf("[Traffic] Starting traffic collection job...")

	done := make(chan error, 1)
	go func() {
		done <- executeTrafficJob(ctx, start)
	}()

	timer := time.NewTimer(TrafficSchedulerJobTimeout)
	defer timer.Stop()

	select {
	case err := <-done:
		if err != nil {
			log.Printf("[Traffic] Job failed: %v", err)
		}
	case <-timer.C:
		log.Printf("[Traffic] Job timed out after %dms", time.Since(start).Milliseconds())
	case <-ctx.Done():
		log.Printf("[Traffic] Job cancelled: %v", ctx.Err())
	}
}

// RunDailyHostTrafficResetJob mirrors runDailyHostTrafficResetJob.
func RunDailyHostTrafficResetJob(ctx context.Context) {
	today := 0
	_, _, today = db.ShanghaiDateParts(trafficSchedulerNow())
	log.Printf("[Traffic] Starting daily host traffic reset check (today is day %d)...", today)

	hosts, err := db.TrafficHostsByResetDay(ctx, today)
	if err != nil {
		log.Printf("[Traffic] Daily host traffic reset failed: %v", err)
		return
	}
	if len(hosts) == 0 {
		log.Printf("[Traffic] No hosts need traffic reset today")
		return
	}

	hostIDs := make([]int32, 0, len(hosts))
	names := make([]string, 0, len(hosts))
	for _, host := range hosts {
		hostIDs = append(hostIDs, host.ID)
		names = append(names, host.Name)
	}
	log.Printf("[Traffic] Found %d hosts to reset: %v", len(hosts), names)

	if _, err := db.ResetHostInstancesMonthlyTraffic(ctx, hostIDs); err != nil {
		log.Printf("[Traffic] Daily host traffic reset failed: %v", err)
		return
	}

	instances, err := db.GetRunningInstancesForTraffic(ctx)
	if err != nil {
		log.Printf("[Traffic] Daily host traffic reset failed: %v", err)
		return
	}
	wanted := make(map[int32]struct{}, len(hostIDs))
	for _, hostID := range hostIDs {
		wanted[hostID] = struct{}{}
	}
	toReconcile := make([]db.RunningTrafficInstance, 0, len(instances))
	for _, instance := range instances {
		if _, ok := wanted[instance.HostID]; ok {
			toReconcile = append(toReconcile, instance)
		}
	}
	trafficSchedulerReconcileState(ctx, toReconcile)
	log.Printf("[Traffic] Daily host traffic reset completed for %d hosts", len(hosts))
}

// RunMonthlyResetJob keeps the deprecated alias.
func RunMonthlyResetJob(ctx context.Context) { RunDailyHostTrafficResetJob(ctx) }

// RunTrafficCleanupJob mirrors runCleanupJob (retention 35 days).
func RunTrafficCleanupJob(ctx context.Context) {
	log.Printf("[Traffic] Starting cleanup job...")
	deleted, err := db.CleanOldDailyTraffic(ctx, 35)
	if err != nil {
		log.Printf("[Traffic] Cleanup failed: %v", err)
		return
	}
	log.Printf("[Traffic] Cleaned up %d old daily traffic records", deleted)
}

// RunNotificationCleanupJob mirrors runNotificationCleanupJob (30 days).
func RunNotificationCleanupJob(ctx context.Context) {
	log.Printf("[Traffic] Starting notification log cleanup job...")
	deleted, err := db.CleanOldNotificationLogs(ctx, 30)
	if err != nil {
		log.Printf("[Traffic] Notification log cleanup failed: %v", err)
		return
	}
	log.Printf("[Traffic] Cleaned up %d old notification logs", deleted)
}

// RunSystemCleanupJob mirrors runSystemCleanupJob.
//
// Faithful quirk: the TypeScript wraps the whole sequence in one try/catch, so a
// failure in the first step skips the rest. Each step is logged individually here
// but the sequence still stops on the first hard error — that is the Node
// behaviour and it is pinned by TestTrafficSchedulerSystemCleanupOrder.
func RunSystemCleanupJob(ctx context.Context) {
	log.Printf("[System] Starting system cleanup job...")

	steps := []struct {
		name  string
		run   func(context.Context) (int64, error)
		label string
	}{
		{"refresh tokens", lib.CleanupExpiredRefreshTokens, "expired refresh tokens"},
		{"email verification codes", db.AuthCleanupExpiredCodes, "expired email verification codes"},
		{"distributed locks", lib.CleanupExpiredLocks, "expired distributed locks"},
		{"operation verifications", lib.CleanupExpiredVerifications, "expired operation verifications"},
		{"token invalidations", lib.CleanupExpiredTokenInvalidations, "expired token invalidations"},
	}
	for _, step := range steps {
		deleted, err := step.run(ctx)
		if err != nil {
			log.Printf("[System] System cleanup failed at %s: %v", step.name, err)
			return
		}
		if deleted > 0 {
			log.Printf("[System] Cleaned up %d %s", deleted, step.label)
		}
	}

	if deleted, err := db.CleanOldLogs(ctx, 90); err != nil {
		log.Printf("[System] System cleanup failed at operation logs: %v", err)
		return
	} else if deleted > 0 {
		log.Printf("[System] Cleaned up %d old operation logs", deleted)
	}

	log.Printf("[System] System cleanup job completed")
}

// ==================== scheduler entry ====================

// startTrafficScheduler wires the five traffic cron entries.
func startTrafficScheduler(group *schedulerGroup) {
	group.startLoop("Traffic", TrafficSchedulerNextRun, func(ctx context.Context) error {
		RunTrafficJob(ctx)
		return nil
	}, nil)
	group.startLoop("TrafficDailyReset", TrafficSchedulerDailyResetNextRun, func(ctx context.Context) error {
		RunDailyHostTrafficResetJob(ctx)
		return nil
	}, nil)
	group.startLoop("TrafficCleanup", TrafficSchedulerCleanupNextRun, func(ctx context.Context) error {
		RunTrafficCleanupJob(ctx)
		return nil
	}, nil)
	group.startLoop("TrafficNotificationCleanup", TrafficSchedulerNotificationCleanupNextRun, func(ctx context.Context) error {
		RunNotificationCleanupJob(ctx)
		return nil
	}, nil)
	group.startLoop("SystemCleanup", TrafficSchedulerSystemCleanupNextRun, func(ctx context.Context) error {
		RunSystemCleanupJob(ctx)
		return nil
	}, nil)
}

func intPointer(value int) *int { return &value }

func int64PointerToBig(value *int64) *big.Int {
	if value == nil {
		return nil
	}
	return big.NewInt(*value)
}
