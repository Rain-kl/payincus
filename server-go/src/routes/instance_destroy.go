// Port of server/src/routes/instance-destroy.ts.
//
// app.ts registers this module as `fastify.register(instanceDestroyRoutes, { prefix:
// '/api/instances' })`, so every path below is relative to /api/instances. Four
// routes, all on the Web (JWT) surface:
//
//	POST /batch/destroy-info   authenticateUser   10/min
//	POST /batch/destroy        authenticateUser    3/min
//	GET  /:id/destroy-info     authenticate       30/min
//	POST /:id/destroy          authenticate        5/min
//
// Money invariants (docs/WAVE_D_SPEC.md §0.3/§0.4) — this is the highest-risk part:
//
//   - the refund is a REAL balance credit. It runs in ONE transaction that takes
//     the destroy-billing advisory lock (4107, request user) before counting the
//     user's previous destroys, then the user-balance lock (4110) inside
//     db.InstanceBillingApplyBalanceTx, writes balance_logs with
//     balance_before/balance_after snapshots and the matching
//     instance_billing_records refund row, and finally inserts user_destroy_records.
//   - the AFF commission reversal joins the same transaction: commission already
//     paid to the referrer is clawed back proportionally to the refund, capped by
//     what is still un-reversed (USER_AFF_BALANCE_LOCK_NAMESPACE 4112).
//   - amounts are float64 only at the boundary: every rounding step is
//     `Number(x.toFixed(2))` (lib.JSFixed), so a preview and the settled amount
//     agree to the cent.
//   - `destroy-info` and `POST /destroy` share instanceDestroySettlementAmounts, so
//     the quoted refund cannot drift from the paid one.
//
// Idempotency + compensation (WAVE_D_SPEC §4.1):
//
//   - `claimInstanceForUserDestroy` is the idempotency gate: inside a transaction
//     it takes the non-blocking instance-operation lock (4106) and flips
//     status → 'deleted' only when the row still has the status the request read.
//     A concurrent or repeated destroy therefore finds `count == 0` and is skipped
//     with "实例正在销毁或已删除" instead of refunding twice.
//   - the Incus delete happens AFTER the claim and BEFORE any money movement. If it
//     fails, `restoreClaimedInstanceStatus` puts the original status back and the
//     request ends in 500 "销毁实例失败" — a failed node call can never leave the
//     instance marked deleted, and can never refund an instance that still exists.
//   - `settleUserDestroyBilling` re-counts the user's destroys under the 4107 lock,
//     so the fee tier is decided once even if two destroys race.
//
// lib/incus/incus-instances.ts#deleteInstance is ported inline below: QEMU log
// capture, 404-as-success DELETE, GPU release and the package-egress cleanup (row
// delete + host policy bump + the already-ported agent push). The only omission is
// that wrapper's busy-retry around the stop call — a busy node still ends up with a
// forced stop attempt, and a stop failure is only logged in the TypeScript anyway.
package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

const (
	// instanceDestroyFeeRate mirrors DESTROY_RULES.FEE_RATE (10% after the first
	// destroy; the first destroy of a user is free).
	instanceDestroyFeeRate = 0.10
	// instanceDestroyMaxPaidMonthlyTraffic mirrors
	// MAX_PAID_DESTROY_MONTHLY_TRAFFIC (5 GiB).
	instanceDestroyMaxPaidMonthlyTraffic int64 = 5 * 1024 * 1024 * 1024
	// instanceDestroyTrafficLimitReason mirrors DESTROY_TRAFFIC_LIMIT_REASON.
	instanceDestroyTrafficLimitReason = "当前月流量周期无法销毁，已用流量达到或超过 5G"
	// instanceDestroyBatchHiddenReason mirrors BATCH_HIDDEN_REASON: a missing
	// instance and another user's instance must be indistinguishable.
	instanceDestroyBatchHiddenReason = "实例不存在或无权操作"
	// instanceDestroyLogModule mirrors the literal module passed to createLog.
	instanceDestroyLogModule = "billing"
)

// ==================== wire types ====================

type instanceDestroyRefundView struct {
	RemainingDays  int     `json:"remainingDays"`
	RemainingValue float64 `json:"remainingValue"`
	FeeRate        float64 `json:"feeRate"`
	FeeAmount      float64 `json:"feeAmount"`
	RefundAmount   float64 `json:"refundAmount"`
	DestroyCount   int     `json:"destroyCount"`
	MaxRefundable  float64 `json:"maxRefundable"`
}

type instanceDestroyInstanceView struct {
	ID       int32   `json:"id"`
	Name     string  `json:"name"`
	HostName string  `json:"hostName"`
	PlanName *string `json:"planName"`
}

type instanceDestroyBatchPreviewItem struct {
	ID                  int32                       `json:"id"`
	Name                string                      `json:"name"`
	CanDestroy          bool                        `json:"canDestroy"`
	CannotDestroyReason string                      `json:"cannotDestroyReason"`
	IsFreeInstance      bool                        `json:"isFreeInstance"`
	IsFirstTime         bool                        `json:"isFirstTime"`
	FeeWaiverEligible   bool                        `json:"feeWaiverEligible"`
	Refund              instanceDestroyRefundView   `json:"refund"`
	Instance            instanceDestroyInstanceView `json:"instance"`
}

type instanceDestroyBatchResultItem struct {
	ID             int32    `json:"id"`
	Name           string   `json:"name"`
	Success        bool     `json:"success"`
	Skipped        *bool    `json:"skipped,omitempty"`
	Reason         *string  `json:"reason,omitempty"`
	RefundAmount   *float64 `json:"refundAmount,omitempty"`
	FeeAmount      *float64 `json:"feeAmount,omitempty"`
	IsFirstTime    *bool    `json:"isFirstTime,omitempty"`
	IsFreeInstance *bool    `json:"isFreeInstance,omitempty"`
}

type instanceDestroyRulesView struct {
	FeeRate float64 `json:"feeRate"`
}

// instanceDestroyInfoView is the GET /:id/destroy-info body.
type instanceDestroyInfoView struct {
	CanDestroy          bool                        `json:"canDestroy"`
	CannotDestroyReason string                      `json:"cannotDestroyReason"`
	IsFreeInstance      bool                        `json:"isFreeInstance"`
	IsFirstTime         bool                        `json:"isFirstTime"`
	IsErrorState        bool                        `json:"isErrorState"`
	FeeWaiverEligible   bool                        `json:"feeWaiverEligible"`
	Rules               instanceDestroyRulesView    `json:"rules"`
	Refund              instanceDestroyRefundView   `json:"refund"`
	Instance            instanceDestroyInstanceView `json:"instance"`
}

// ==================== loaded instance view ====================

type instanceDestroyView struct {
	Instance *models.Instance
	HostName string
	PlanName *string
}

// instanceDestroyLoadInstance mirrors
// `prisma.instance.findUnique({ where: { id }, include: { host, packagePlan } })`.
// It is (nil, nil) when the row is absent; a missing host is an error because
// Prisma's required relation would have made the TypeScript throw.
func instanceDestroyLoadInstance(ctx context.Context, instanceID int32) (*instanceDestroyView, error) {
	instance, err := db.GetInstanceForBilling(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if instance == nil {
		return nil, nil
	}

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		return nil, err
	}
	if host == nil {
		return nil, fmt.Errorf("实例 %d 的宿主机 %d 不存在", instanceID, instance.HostID)
	}

	view := &instanceDestroyView{Instance: instance, HostName: host.Name}
	if instance.PackagePlanID != nil {
		var plans []struct {
			Name string `gorm:"column:name"`
		}
		if err := db.DB.WithContext(ctx).Raw(
			`SELECT name FROM package_plans WHERE id = ? LIMIT 1`, *instance.PackagePlanID,
		).Scan(&plans).Error; err != nil {
			return nil, err
		}
		if len(plans) > 0 {
			name := plans[0].Name
			view.PlanName = &name
		}
	}
	return view, nil
}

// instanceDestroyUserDestroyCount mirrors `userDestroyRecord.count({ userId })`.
func instanceDestroyUserDestroyCount(ctx context.Context, userID int32) (int, error) {
	var rows []struct {
		Total int64 `gorm:"column:total"`
	}
	err := db.DB.WithContext(ctx).Raw(
		`SELECT COUNT(*)::bigint AS total FROM user_destroy_records WHERE user_id = ?`, userID,
	).Scan(&rows).Error
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return int(rows[0].Total), nil
}

// ==================== pure rule helpers ====================

// instanceDestroyIsExpiredAt mirrors isExpiredAt: `expiresAt <= now`.
func instanceDestroyIsExpiredAt(expiresAt *time.Time, now time.Time) bool {
	return expiresAt != nil && !expiresAt.After(now)
}

// instanceDestroyCanDestroyExpiredSuspendedPaid mirrors
// canUserDestroyExpiredSuspendedPaidInstance: an expired, auto-suspended paid
// instance is destroyable (otherwise a suspension blocks the destroy forever).
func instanceDestroyCanDestroyExpiredSuspendedPaid(instance *models.Instance, now time.Time) bool {
	return instance.Status == models.InstanceStatusSuspended &&
		instance.SuspendReason != nil && *instance.SuspendReason == "expired" &&
		instance.PackagePlanID != nil &&
		instanceDestroyIsExpiredAt(instance.ExpiresAt, now)
}

// instanceDestroyCalculateFeeRate mirrors calculateFeeRate: the first destroy of a
// user is free, every later one costs 10%.
func instanceDestroyCalculateFeeRate(destroyCount int) float64 {
	if destroyCount == 0 {
		return 0
	}
	return instanceDestroyFeeRate
}

// instanceDestroyExceedsPaidDestroyTrafficLimit mirrors
// exceedsPaidDestroyTrafficLimit: `used >= 5 GiB`.
func instanceDestroyExceedsPaidDestroyTrafficLimit(monthlyTrafficUsed int64) bool {
	return monthlyTrafficUsed >= instanceDestroyMaxPaidMonthlyTraffic
}

// instanceDestroyRoundMoney mirrors roundMoney: `Number(value.toFixed(2))`.
func instanceDestroyRoundMoney(value float64) float64 {
	return lib.JSFixed(value, 2)
}

// instanceDestroyFixed2 mirrors `value.toFixed(2)` for messages.
func instanceDestroyFixed2(value float64) string {
	return strconv.FormatFloat(lib.JSFixed(value, 2), 'f', 2, 64)
}

// instanceDestroySettlementAmounts is THE fee/refund calculation. It is called by
// the quote (destroy-info / batch preview) AND by the settlement inside the 4107
// transaction, so the quoted refund and the paid refund cannot drift apart.
func instanceDestroySettlementAmounts(
	refundableValue float64, destroyCount int, feeWaiver bool,
) (feeRate, feeAmount, refundAmount float64, isFirstTime bool) {
	isFirstTime = destroyCount == 0
	if !feeWaiver {
		feeRate = instanceDestroyCalculateFeeRate(destroyCount)
		if !isFirstTime {
			feeAmount = instanceDestroyRoundMoney(refundableValue * feeRate)
		}
	}
	refundAmount = instanceDestroyRoundMoney(math.Max(0, refundableValue-feeAmount))
	return feeRate, feeAmount, refundAmount, isFirstTime
}

// instanceDestroyQuote is the shared preview/execute quote builder.
func instanceDestroyQuote(
	quote instanceBillingRefundQuote, destroyCount int, feeWaiver, isFreeInstance, isErrorState bool,
) instanceDestroyInfoView {
	feeRate, feeAmount, refundAmount, isFirstTime := instanceDestroySettlementAmounts(
		quote.Refundable, destroyCount, feeWaiver)

	view := instanceDestroyInfoView{
		CanDestroy:          true,
		CannotDestroyReason: "",
		IsFreeInstance:      isFreeInstance,
		IsFirstTime:         isFirstTime,
		IsErrorState:        isErrorState,
		FeeWaiverEligible:   isErrorState,
		Rules: instanceDestroyRulesView{
			FeeRate: instanceDestroyFeeRate,
		},
		Refund: instanceDestroyRefundView{
			RemainingDays:  quote.RemainingDays,
			RemainingValue: quote.RemainingValue,
			FeeRate:        feeRate,
			FeeAmount:      feeAmount,
			RefundAmount:   refundAmount,
			DestroyCount:   destroyCount,
			MaxRefundable:  quote.MaxRefundable,
		},
	}
	if feeWaiver {
		// `feeRate: feeWaiver ? 0 : DESTROY_RULES.FEE_RATE`
		view.Rules.FeeRate = 0
	}
	return view
}

// instanceDestroyIsPaidQuoteEligible mirrors the route guard
// `!isFreeInstance && instance.expiresAt && instance.billingPrice && instance.billingCycle`.
func instanceDestroyIsPaidQuoteEligible(instance *models.Instance, isFreeInstance bool) bool {
	return !isFreeInstance && instance.ExpiresAt != nil &&
		instance.BillingPrice != nil && instance.BillingCycle != nil
}

// instanceDestroyEmptyRefund mirrors the all-zero refund block.
func instanceDestroyEmptyRefund() instanceDestroyRefundView {
	return instanceDestroyRefundView{}
}

// ==================== blocking tasks ====================

type instanceDestroyBlockingTask struct {
	Code   string
	Reason string
}

// instanceDestroyGetBlockingTask mirrors getDestroyBlockingTask: a destroy cannot
// race an in-flight restore, backup upload or instance task.
func instanceDestroyGetBlockingTask(ctx context.Context, client *gorm.DB, instanceID int32) (*instanceDestroyBlockingTask, error) {
	if client == nil {
		client = db.DB
	}

	countActive := func(table string) (bool, error) {
		var rows []struct {
			Total int64 `gorm:"column:total"`
		}
		err := client.WithContext(ctx).Raw(
			`SELECT COUNT(*)::bigint AS total FROM `+table+
				` WHERE instance_id = ? AND status IN ('PENDING','PROCESSING')`, instanceID,
		).Scan(&rows).Error
		if err != nil {
			return false, err
		}
		return len(rows) > 0 && rows[0].Total > 0, nil
	}

	activeRestore, err := countActive("restore_tasks")
	if err != nil {
		return nil, err
	}
	activeUpload, err := countActive("backup_upload_tasks")
	if err != nil {
		return nil, err
	}
	activeInstanceTask, err := countActive("instance_tasks")
	if err != nil {
		return nil, err
	}

	if activeRestore {
		return &instanceDestroyBlockingTask{
			Code: "RESTORE_IN_PROGRESS", Reason: "该实例有正在进行的恢复任务，请等待完成或取消",
		}, nil
	}
	if activeUpload {
		return &instanceDestroyBlockingTask{
			Code: "UPLOAD_IN_PROGRESS", Reason: "该实例有正在进行的备份上传任务，请等待完成或取消",
		}, nil
	}
	if activeInstanceTask {
		return &instanceDestroyBlockingTask{
			Code: "TASK_IN_PROGRESS", Reason: "该实例有正在进行的操作任务，请等待完成",
		}, nil
	}
	return nil, nil
}

// ==================== AFF commission reversal ====================

type instanceDestroyAffBinding struct {
	AffCodeID int32
	UserID    int32
}

// InstanceDestroyReverseAffCommissionForRefund mirrors
// reverseInstanceAffCommissionForRefund (exported: admin-billing imports it too).
//
// The TypeScript rounds every intermediate sum with Number(x.toFixed(2)); the same
// roundings are applied here so the clawback matches to the cent.
func InstanceDestroyReverseAffCommissionForRefund(
	ctx context.Context, tx *gorm.DB, instanceID int32, refundAmount float64, remark string,
) error {
	if math.IsNaN(refundAmount) || math.IsInf(refundAmount, 0) || refundAmount <= 0 {
		return nil
	}
	if tx == nil {
		return errors.New("返利冲正需要事务")
	}

	var bindingRows []instanceDestroyAffBinding
	if err := tx.WithContext(ctx).Raw(
		`SELECT ac.id AS aff_code_id, ac.user_id AS user_id
		   FROM aff_bindings ab JOIN aff_codes ac ON ac.id = ab.aff_code_id
		  WHERE ab.instance_id = ? LIMIT 1`, instanceID,
	).Scan(&bindingRows).Error; err != nil {
		return err
	}
	if len(bindingRows) == 0 {
		return nil
	}
	binding := bindingRows[0]

	var paidRows []struct {
		Total *string `gorm:"column:total"`
	}
	if err := tx.WithContext(ctx).Raw(
		`SELECT SUM(amount)::text AS total FROM instance_billing_records
		  WHERE instance_id = ? AND type IN ('newPurchase','renew','upgrade','downgrade') AND amount > 0`,
		instanceID,
	).Scan(&paidRows).Error; err != nil {
		return err
	}
	originalPaidAmount := 0.0
	if len(paidRows) > 0 && paidRows[0].Total != nil && *paidRows[0].Total != "" {
		parsed, err := decimal.NewFromString(*paidRows[0].Total)
		if err != nil {
			return err
		}
		originalPaidAmount = parsed.InexactFloat64()
	}
	if math.IsNaN(originalPaidAmount) || math.IsInf(originalPaidAmount, 0) || originalPaidAmount <= 0 {
		return nil
	}

	refundRatio := math.Min(1, refundAmount/originalPaidAmount)

	type affLogRow struct {
		Type           models.AffLogType `gorm:"column:type"`
		Amount         models.Decimal    `gorm:"column:amount"`
		OriginalAmount *models.Decimal   `gorm:"column:original_amount"`
	}
	var affLogs []affLogRow
	if err := tx.WithContext(ctx).Raw(
		`SELECT type, amount, original_amount FROM aff_logs
		  WHERE instance_id = ? AND aff_code_id = ? AND user_id = ? AND type IN ('new_purchase','renew')`,
		instanceID, binding.AffCodeID, binding.UserID,
	).Scan(&affLogs).Error; err != nil {
		return err
	}

	for _, logType := range []models.AffLogType{models.AffLogTypeNewPurchase, models.AffLogTypeRenew} {
		typeLogs := make([]affLogRow, 0)
		for _, row := range affLogs {
			if row.Type == logType {
				typeLogs = append(typeLogs, row)
			}
		}

		issuedCommission := 0.0
		alreadyReversed := 0.0
		for _, row := range typeLogs {
			amount := row.Amount.InexactFloat64()
			if amount > 0 {
				issuedCommission += amount
			}
			if amount < 0 {
				alreadyReversed += -amount
			}
		}
		issuedCommission = instanceDestroyRoundMoney(issuedCommission)
		alreadyReversed = instanceDestroyRoundMoney(alreadyReversed)
		remainingCommission := instanceDestroyRoundMoney(math.Max(0, issuedCommission-alreadyReversed))
		commissionAmount := math.Min(remainingCommission, instanceDestroyRoundMoney(issuedCommission*refundRatio))
		if commissionAmount <= 0 {
			continue
		}

		if err := db.AdvisoryTransactionLockTx(tx, db.UserAffBalanceLockNamespace, binding.UserID); err != nil {
			return err
		}

		var affUserRows []struct {
			AffBalance models.Decimal `gorm:"column:aff_balance"`
		}
		if err := tx.WithContext(ctx).Raw(
			`SELECT aff_balance FROM users WHERE id = ?`, binding.UserID,
		).Scan(&affUserRows).Error; err != nil {
			return err
		}
		if len(affUserRows) == 0 {
			return errors.New("记录不存在")
		}
		affBalanceBefore := affUserRows[0].AffBalance.InexactFloat64()

		if err := tx.WithContext(ctx).Exec(
			`UPDATE users SET aff_balance = aff_balance - ?, updated_at = ? WHERE id = ?`,
			decimal.NewFromFloat(commissionAmount).Round(2), time.Now().UTC(), binding.UserID,
		).Error; err != nil {
			return err
		}

		var affAfterRows []struct {
			AffBalance models.Decimal `gorm:"column:aff_balance"`
		}
		if err := tx.WithContext(ctx).Raw(
			`SELECT aff_balance FROM users WHERE id = ?`, binding.UserID,
		).Scan(&affAfterRows).Error; err != nil {
			return err
		}
		if len(affAfterRows) == 0 {
			return errors.New("记录不存在")
		}
		affBalanceAfter := affAfterRows[0].AffBalance.InexactFloat64()

		originalAmount := 0.0
		for _, row := range typeLogs {
			if row.Amount.InexactFloat64() > 0 && row.OriginalAmount != nil {
				originalAmount += math.Max(0, row.OriginalAmount.InexactFloat64())
			}
		}
		originalAmount = instanceDestroyRoundMoney(originalAmount)

		originalAmountValue := models.Decimal{Decimal: decimal.NewFromFloat(originalAmount).Round(2)}
		affCodeID := binding.AffCodeID
		instanceIDCopy := instanceID
		remarkCopy := remark
		affLog := models.AffLog{
			UserID:         binding.UserID,
			AffCodeID:      &affCodeID,
			InstanceID:     &instanceIDCopy,
			Type:           logType,
			Amount:         models.Decimal{Decimal: decimal.NewFromFloat(-commissionAmount).Round(2)},
			OriginalAmount: &originalAmountValue,
			BalanceBefore:  models.Decimal{Decimal: decimal.NewFromFloat(affBalanceBefore).Round(2)},
			BalanceAfter:   models.Decimal{Decimal: decimal.NewFromFloat(affBalanceAfter).Round(2)},
			Remark:         &remarkCopy,
		}
		if err := tx.WithContext(ctx).Create(&affLog).Error; err != nil {
			return err
		}

		if err := tx.WithContext(ctx).Exec(
			`UPDATE aff_codes SET total_earnings = total_earnings - ? WHERE id = ?`,
			decimal.NewFromFloat(commissionAmount).Round(2), binding.AffCodeID,
		).Error; err != nil {
			return err
		}

		if logType == models.AffLogTypeNewPurchase && commissionAmount >= remainingCommission {
			if err := tx.WithContext(ctx).Exec(
				`UPDATE aff_codes SET used_count = used_count - 1 WHERE id = ? AND used_count > 0`,
				binding.AffCodeID,
			).Error; err != nil {
				return err
			}
		}
	}

	return nil
}

// ==================== claim / compensation ====================

// instanceDestroyClaimInstance mirrors claimInstanceForUserDestroy. It returns
// false when the lock is held or the status moved, which is what makes a repeated
// destroy a no-op instead of a second refund.
func instanceDestroyClaimInstance(ctx context.Context, instanceID, userID int32, currentStatus models.InstanceStatus) (bool, error) {
	claimed := false
	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := db.TryInstanceBillingAdvisoryLock(ctx, tx, db.InstanceOperationLockNamespace, instanceID)
		if err != nil {
			return err
		}
		if !locked {
			return nil
		}

		blocking, err := instanceDestroyGetBlockingTask(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if blocking != nil {
			return nil
		}

		result := tx.WithContext(ctx).Model(&models.Instance{}).
			Where("id = ? AND user_id = ? AND status = ?", instanceID, userID, string(currentStatus)).
			Updates(map[string]any{"status": string(models.InstanceStatusDeleted)})
		if result.Error != nil {
			return result.Error
		}
		claimed = result.RowsAffected == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

// instanceDestroyRestoreClaimedStatus mirrors restoreClaimedInstanceStatus: the
// compensation for a failed Incus delete.
func instanceDestroyRestoreClaimedStatus(ctx context.Context, instanceID, userID int32, originalStatus models.InstanceStatus) error {
	return db.DB.WithContext(ctx).Model(&models.Instance{}).
		Where("id = ? AND user_id = ? AND status = ?", instanceID, userID, string(models.InstanceStatusDeleted)).
		Updates(map[string]any{"status": string(originalStatus)}).Error
}

// ==================== Incus delete ====================

// InstanceDestroyGetIncusClient is a seam so the delete path can be tested without
// a node. Production keeps lib.GetIncusClient (pooled, fingerprint-aware).
var InstanceDestroyGetIncusClient = func(ctx context.Context, host *db.HostsRecord) (*lib.IncusClient, error) {
	return lib.GetIncusClient(ctx, lib.IncusClientOptions{
		URL:           host.URL,
		CertPath:      host.CertPath,
		KeyPath:       host.KeyPath,
		TunnelEnabled: host.TunnelEnabled,
		HostID:        int64(host.ID),
		TargetHost:    host.TargetHost,
		TargetPort:    int(host.TargetPort),
	})
}

// InstanceDestroyRunOperationTask is a seam for lib.RunOperationTask so tests can
// run the delete helper without writing operation-task rows.
var InstanceDestroyRunOperationTask = func(
	ctx context.Context, hostID int32, name string,
	action func(context.Context) error,
) error {
	return lib.RunOperationTask(ctx, hostID, name, lib.OperationTaskKindDestroy, action, nil)
}

// InstanceDestroyReleasePackageEgress is a seam for the Node helper of the same
// name (lib/egress/attachments.ts). It deletes the instance's egress attachment
// rows, bumps the host policy revision and asks the node to re-push.
var InstanceDestroyReleasePackageEgress = func(ctx context.Context, hostID int32, incusName string) error {
	return instanceDestroyReleasePackageEgress(ctx, hostID, incusName)
}

// instanceDestroyReleasePackageEgress mirrors releasePackageEgress.
func instanceDestroyReleasePackageEgress(ctx context.Context, hostID int32, incusName string) error {
	if hostID == 0 || db.Pool() == nil {
		return nil
	}
	firewall, err := json.Marshal(lib.DefaultFirewall())
	if err != nil {
		return err
	}

	changed := int64(0)
	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Single-argument lock 74201 then two-argument (74202, hostId), exactly like
	// the TypeScript (different lock spaces — see WAVE_D_SPEC §0.4).
	if _, err := tx.Exec(ctx, "SELECT 1 FROM pg_advisory_xact_lock(74201)"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM pg_advisory_xact_lock(74202, $1)", hostID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx,
		`DELETE FROM instance_egress_attachments WHERE host_id = $1 AND incus_name = $2`,
		hostID, incusName)
	if err != nil {
		return err
	}
	changed = tag.RowsAffected()
	if changed > 0 {
		if err := db.EgressHostNetworkPolicyBump(ctx, tx, hostID, firewall); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	if changed > 0 {
		// pushNetworkPolicy(hostId), mirroring the TypeScript call after the commit.
		// A failure only means the node re-syncs on its next policy push; the
		// TypeScript logs it and moves on ("出口绑定清理等待节点重连后同步").
		if _, pushErr := egressPushNetworkPolicy(ctx, hostID); pushErr != nil {
			log.Printf("出口绑定清理等待节点重连后同步: %v", pushErr)
		}
	}
	return nil
}

// instanceDestroyReleaseFailedGpuByName mirrors releaseFailedGpuByName.
func instanceDestroyReleaseFailedGpuByName(ctx context.Context, hostID int32, incusName string) error {
	if hostID == 0 {
		return nil
	}
	return db.DB.WithContext(ctx).Exec(
		`UPDATE instances SET gpu_released_at = ?
		  WHERE incus_id = ? AND host_id = ? AND status IN ('error','deleted')
		    AND gpu_device_id IS NOT NULL AND gpu_released_at IS NULL`,
		time.Now().UTC(), incusName, hostID,
	).Error
}

// instanceDestroyStopInstance mirrors the part of lib/incus#stopInstance the
// destroy flow relies on: skip when already stopped, otherwise PUT stop with the
// 60s timeout and `force` (busy-retry is not ported; the caller only warns).
func instanceDestroyStopInstance(ctx context.Context, client *lib.IncusClient, name string, force bool) error {
	raw, err := client.Request(ctx, http.MethodGet, "/1.0/instances/"+name+"/state", nil, 0, nil)
	if err != nil {
		return err
	}
	var state struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &state); err == nil && state.Status == "stopped" {
		return nil
	}
	_, err = client.Request(ctx, http.MethodPut, "/1.0/instances/"+name+"/state", map[string]any{
		"action":  "stop",
		"timeout": 60,
		"force":   force,
	}, 0, nil)
	return err
}

// instanceDestroyDeleteIncusInstance mirrors deleteIncusInstanceForUserDestroy.
// A 404 means "already gone" and is a success, exactly like the TypeScript.
func instanceDestroyDeleteIncusInstance(
	ctx context.Context, instance *models.Instance, logger *log.Logger,
) error {
	if lib.CurrentOperationTask(ctx) == nil {
		return InstanceDestroyRunOperationTask(ctx, instance.HostID, instance.IncusID, func(taskCtx context.Context) error {
			return instanceDestroyDeleteIncusInstance(taskCtx, instance, logger)
		})
	}
	lib.TaskLog(ctx, "info", "连接节点，准备停止并删除实例")

	host, err := db.HostsGetByID(ctx, instance.HostID)
	if err != nil {
		return err
	}
	if host == nil {
		return errors.New("宿主机不存在，无法删除实例")
	}

	client, err := InstanceDestroyGetIncusClient(ctx, host)
	if err != nil {
		return err
	}

	if instance.Status == models.InstanceStatusRunning {
		if stopErr := instanceDestroyStopInstance(ctx, client, instance.IncusID, true); stopErr != nil {
			logger.Printf("停止实例失败，继续尝试删除: %v", stopErr)
		}
	}

	// Capture the final QEMU log before Incus removes the log directory.
	if content, statusCode, readErr := client.ReadFile(
		ctx, "/1.0/instances/"+instance.IncusID+"/logs/qemu.log", 65536,
	); readErr == nil && (statusCode == http.StatusOK || statusCode == http.StatusPartialContent) && strings.TrimSpace(content) != "" {
		for _, line := range strings.Split(content, "\n") {
			if strings.TrimSpace(line) != "" {
				lib.TaskLog(ctx, "info", "[节点 QEMU 删除前日志] "+line)
			}
		}
	} else {
		lib.TaskLog(ctx, "warn", "删除前节点 QEMU 日志不可读，继续删除流程")
	}

	lib.TaskLog(ctx, "info", "向节点提交删除实例 "+instance.IncusID)
	if _, err := client.Request(ctx, http.MethodDelete, "/1.0/instances/"+instance.IncusID, nil, 0, nil); err != nil {
		if !lib.IsIncusNotFound(err) {
			return err
		}
	}

	// Post-delete side effects. Both mirror the TypeScript's deleteInstance body.
	if err := instanceDestroyReleaseFailedGpuByName(ctx, instance.HostID, instance.IncusID); err != nil {
		return err
	}
	if err := InstanceDestroyReleasePackageEgress(ctx, instance.HostID, instance.IncusID); err != nil {
		// The TypeScript swallows this one: the node re-syncs on reconnect.
		logger.Printf("出口绑定清理等待节点重连后同步: %v", err)
	}
	return nil
}

// ==================== settlement ====================

type instanceDestroyBillingResult struct {
	RefundAmount float64
	FeeAmount    float64
	IsFirstTime  bool
}

// instanceDestroySettleBilling mirrors settleUserDestroyBilling: fee tier, balance
// credit + logs + billing record, AFF clawback and the destroy record all land in
// one transaction.
func instanceDestroySettleBilling(
	ctx context.Context,
	requestUserID int32,
	instance *models.Instance,
	refundableValue float64,
	feeWaiver bool,
) (instanceDestroyBillingResult, error) {
	result := instanceDestroyBillingResult{}

	txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := db.AdvisoryTransactionLockTx(tx, db.UserDestroyBillingLockNamespace, requestUserID); err != nil {
			return err
		}

		destroyCount, err := instanceDestroyUserDestroyCountTx(ctx, tx, requestUserID)
		if err != nil {
			return err
		}
		_, feeAmount, refundAmount, isFirstTime := instanceDestroySettlementAmounts(
			refundableValue, destroyCount, feeWaiver)
		result.RefundAmount = refundAmount
		result.FeeAmount = feeAmount
		result.IsFirstTime = isFirstTime

		if refundAmount > 0 {
			suffix := "（首次销毁免手续费）"
			if feeWaiver {
				suffix = "（异常实例免手续费）"
			} else if feeAmount > 0 {
				suffix = "（手续费 ¥" + instanceDestroyFixed2(feeAmount) + "）"
			}
			balanceRemark := "用户销毁实例退款：" + instance.Name + suffix
			recordRemark := "用户销毁实例退款" + suffix

			balance, err := db.InstanceBillingApplyBalanceTx(ctx, tx, db.InstanceBillingBalanceChange{
				UserID:     instance.UserID,
				InstanceID: instance.ID,
				Delta:      models.Decimal{Decimal: decimal.NewFromFloat(refundAmount).Round(2)},
				LogType:    models.BalanceLogTypeRefund,
				Remark:     balanceRemark,
			})
			if err != nil {
				return err
			}

			now := time.Now().UTC()
			balanceLogID := balance.BalanceLogID
			billingRemark := recordRemark
			record := models.InstanceBillingRecord{
				InstanceID:   instance.ID,
				UserID:       instance.UserID,
				Type:         models.BillingRecordTypeRefund,
				Amount:       models.Decimal{Decimal: decimal.NewFromFloat(-refundAmount).Round(2)},
				Months:       0,
				PeriodStart:  now,
				PeriodEnd:    now,
				BalanceLogID: &balanceLogID,
				Remark:       &billingRemark,
			}
			if err := tx.WithContext(ctx).Create(&record).Error; err != nil {
				return err
			}

			if err := InstanceDestroyReverseAffCommissionForRefund(
				ctx, tx, instance.ID, refundAmount, "用户销毁实例返利冲正："+instance.Name,
			); err != nil {
				return err
			}
		}

		destroyRecord := models.UserDestroyRecord{
			UserID:       requestUserID,
			HostID:       instance.HostID,
			InstanceID:   instance.ID,
			InstanceName: instance.Name,
			DestroyedAt:  time.Now().UTC(),
			RefundAmount: models.Decimal{Decimal: decimal.NewFromFloat(refundAmount).Round(2)},
			FeeAmount:    models.Decimal{Decimal: decimal.NewFromFloat(feeAmount).Round(2)},
			IsFirstTime:  isFirstTime,
		}
		return tx.WithContext(ctx).Create(&destroyRecord).Error
	})
	if txErr != nil {
		return instanceDestroyBillingResult{}, txErr
	}
	return result, nil
}

// instanceDestroyUserDestroyCountTx is the transaction-scoped count used under the
// 4107 lock.
func instanceDestroyUserDestroyCountTx(ctx context.Context, tx *gorm.DB, userID int32) (int, error) {
	var rows []struct {
		Total int64 `gorm:"column:total"`
	}
	if err := tx.WithContext(ctx).Raw(
		`SELECT COUNT(*)::bigint AS total FROM user_destroy_records WHERE user_id = ?`, userID,
	).Scan(&rows).Error; err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return int(rows[0].Total), nil
}

// ==================== related-state cleanup ====================

// instanceDestroyCleanupRelated mirrors the best-effort deleteMany block. The
// TypeScript wraps it in try/catch and only warns, because the instance is already
// gone from the node at that point.
func instanceDestroyCleanupRelated(ctx context.Context, instanceID int32, logger *log.Logger) (int64, error) {
	var portMappings int64
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT COUNT(*)::bigint AS total FROM port_mappings WHERE instance_id = ?`, instanceID,
	).Scan(&portMappings).Error; err != nil {
		return 0, err
	}

	statements := []string{
		`DELETE FROM snapshots WHERE instance_id = ?`,
		`DELETE FROM backups WHERE instance_id = ?`,
		`DELETE FROM port_mappings WHERE instance_id = ?`,
		`DELETE FROM proxy_sites WHERE instance_id = ?`,
		`DELETE FROM daily_traffic WHERE instance_id = ?`,
		`DELETE FROM ip_addresses WHERE instance_id = ?`,
		`DELETE FROM ipv6_subnets WHERE instance_id = ?`,
		`DELETE FROM restore_tasks WHERE instance_id = ? AND status IN ('COMPLETED','FAILED')`,
		`DELETE FROM backup_upload_tasks WHERE instance_id = ? AND status IN ('COMPLETED','FAILED')`,
		`DELETE FROM instance_tasks WHERE instance_id = ? AND status IN ('COMPLETED','FAILED')`,
	}
	for _, statement := range statements {
		if err := db.DB.WithContext(ctx).Exec(statement, instanceID).Error; err != nil {
			return portMappings, err
		}
	}

	if err := db.DB.WithContext(ctx).Exec(
		`UPDATE instance_transfers SET status = 'cancelled', cancelled_at = ?
		  WHERE instance_id = ? AND status = 'pending'`,
		time.Now().UTC(), instanceID,
	).Error; err != nil {
		return portMappings, err
	}
	return portMappings, nil
}

// instanceDestroyRollbackHostResources mirrors db.rollbackResources: one atomic
// GREATEST(...) statement so concurrent releases cannot lose updates.
func instanceDestroyRollbackHostResources(
	ctx context.Context, instance *models.Instance, portCount int64,
) error {
	if instance.CPU < 0 || instance.Memory < 0 || instance.Disk < 0 || portCount < 0 {
		return errors.New("Resource amounts must be non-negative")
	}
	if portCount > 0 {
		return db.DB.WithContext(ctx).Exec(
			`UPDATE hosts
			    SET cpu_used = GREATEST(cpu_used - ?, 0),
			        memory_used = GREATEST(memory_used - ?, 0),
			        disk_used = GREATEST(disk_used - ?, 0),
			        nat_ports_used_count = GREATEST(nat_ports_used_count - ?, 0)
			  WHERE id = ?`,
			instance.CPU, instance.Memory, instance.Disk, portCount, instance.HostID,
		).Error
	}
	return db.DB.WithContext(ctx).Exec(
		`UPDATE hosts
		    SET cpu_used = GREATEST(cpu_used - ?, 0),
		        memory_used = GREATEST(memory_used - ?, 0),
		        disk_used = GREATEST(disk_used - ?, 0)
		  WHERE id = ?`,
		instance.CPU, instance.Memory, instance.Disk, instance.HostID,
	).Error
}

// instanceDestroyRefreshHostResources mirrors calculateHostResourcesFromInstances +
// updateHostResources + the host natPortsUsedCount recount.
func instanceDestroyRefreshHostResources(ctx context.Context, hostID int32) error {
	used, err := db.HostsCalculateResourcesFromInstances(ctx, hostID)
	if err != nil {
		return err
	}
	cpuUsed, memoryUsed, diskUsed := used.CPUUsed, used.MemoryUsed, used.DiskUsed
	if err := db.HostsUpdateResources(ctx, hostID, db.HostsResourceUpdate{
		CPUUsed:    &cpuUsed,
		MemoryUsed: &memoryUsed,
		DiskUsed:   &diskUsed,
	}, nil); err != nil {
		return err
	}

	var actualPorts int64
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT COUNT(*)::bigint AS total
		   FROM port_mappings pm JOIN instances i ON i.id = pm.instance_id
		  WHERE i.host_id = ? AND i.status <> 'deleted'`, hostID,
	).Scan(&actualPorts).Error; err != nil {
		return err
	}
	return db.DB.WithContext(ctx).Exec(
		`UPDATE hosts SET nat_ports_used_count = ? WHERE id = ?`, actualPorts, hostID,
	).Error
}

// ==================== per-instance execution ====================

type instanceDestroyRunInput struct {
	User           *lib.AccessTokenClaims
	Instance       *models.Instance
	HostName       string
	IsFreeInstance bool
	FeeWaiver      bool
	Refundable     float64
	// DestroyCount is the user's destroy history read before the claim, exactly like
	// `destroyRecords.length` in the TypeScript: a free instance never settles, so its
	// reported isFirstTime comes from this value.
	DestroyCount int
	Logger       *log.Logger
}

// InstanceDestroyClaim is a seam for the idempotency gate.
var InstanceDestroyClaim = instanceDestroyClaimInstance

// InstanceDestroyRestoreClaimed is a seam for the compensation step.
var InstanceDestroyRestoreClaimed = instanceDestroyRestoreClaimedStatus

// InstanceDestroyDeleteIncus is a seam for the node delete.
var InstanceDestroyDeleteIncus = instanceDestroyDeleteIncusInstance

// InstanceDestroySettleBilling is a seam for the money movement.
var InstanceDestroySettleBilling = instanceDestroySettleBilling

// instanceDestroyRunDestroyedInstance is the post-claim half of
// executeDestroyForUser, split out so the compensation path is unit-testable.
//
// The returned error is reserved for failures the TypeScript would let escape the
// handler (a claim/DB error). Everything after the claim is reported as a failed
// item, exactly like the TypeScript's inner try/catch that answers
// "销毁实例失败".
func instanceDestroyRunDestroyedInstance(ctx context.Context, input instanceDestroyRunInput) (instanceDestroyBatchResultItem, error) {
	instance := input.Instance
	user := input.User
	logger := input.Logger
	if logger == nil {
		logger = log.Default()
	}

	claimed, err := InstanceDestroyClaim(ctx, instance.ID, user.ID, instance.Status)
	if err != nil {
		return instanceDestroyBatchResultItem{}, err
	}
	if !claimed {
		reason := "实例正在销毁或已删除"
		skipped := true
		return instanceDestroyBatchResultItem{
			ID: instance.ID, Name: instance.Name, Success: false, Skipped: &skipped, Reason: &reason,
		}, nil
	}

	if deleteErr := InstanceDestroyDeleteIncus(ctx, instance, logger); deleteErr != nil {
		// Compensation: the row must not stay 'deleted' when the node still owns it.
		if restoreErr := InstanceDestroyRestoreClaimed(ctx, instance.ID, user.ID, instance.Status); restoreErr != nil {
			logger.Printf("恢复实例状态失败 (instance=%d): %v", instance.ID, restoreErr)
		}
		logger.Printf("销毁实例失败: %v", deleteErr)
		return instanceDestroyFailure(instance), nil
	}

	refundAmount := 0.0
	feeAmount := 0.0
	isFirstTime := input.DestroyCount == 0
	if !input.IsFreeInstance {
		settled, settleErr := InstanceDestroySettleBilling(
			ctx, user.ID, instance, input.Refundable, input.FeeWaiver)
		if settleErr != nil {
			logger.Printf("销毁实例失败: %v", settleErr)
			return instanceDestroyFailure(instance), nil
		}
		refundAmount = settled.RefundAmount
		feeAmount = settled.FeeAmount
		isFirstTime = settled.IsFirstTime
	}

	if finalizeErr := InstanceDestroyFinalize(ctx, instanceDestroyFinalizeInput{
		User:           user,
		Instance:       instance,
		HostName:       input.HostName,
		RefundAmount:   refundAmount,
		FeeAmount:      feeAmount,
		IsFreeInstance: input.IsFreeInstance,
		Logger:         logger,
	}); finalizeErr != nil {
		logger.Printf("销毁实例失败: %v", finalizeErr)
		return instanceDestroyFailure(instance), nil
	}

	return instanceDestroyBatchResultItem{
		ID:             instance.ID,
		Name:           instance.Name,
		Success:        true,
		RefundAmount:   &refundAmount,
		FeeAmount:      &feeAmount,
		IsFirstTime:    &isFirstTime,
		IsFreeInstance: &input.IsFreeInstance,
	}, nil
}

// instanceDestroyFinalizeInput is the post-settlement bookkeeping payload.
type instanceDestroyFinalizeInput struct {
	User           *lib.AccessTokenClaims
	Instance       *models.Instance
	HostName       string
	RefundAmount   float64
	FeeAmount      float64
	IsFreeInstance bool
	Logger         *log.Logger
}

// InstanceDestroyFinalize is a seam for the cleanup/host-resource/log/notify block
// so the money path can be tested without a database.
var InstanceDestroyFinalize = instanceDestroyFinalizeDestroyedInstance

// instanceDestroyFinalizeDestroyedInstance mirrors the tail of executeDestroyForUser:
// best-effort related-row cleanup, host resource release, operation log, inbox
// notification, refund email and the package release notification.
func instanceDestroyFinalizeDestroyedInstance(ctx context.Context, input instanceDestroyFinalizeInput) error {
	instance := input.Instance
	logger := input.Logger
	if logger == nil {
		logger = log.Default()
	}

	portMappings, cleanupErr := instanceDestroyCleanupRelated(ctx, instance.ID, logger)
	if cleanupErr != nil {
		logger.Printf("清理关联数据失败: %v", cleanupErr)
	}

	if err := instanceDestroyRollbackHostResources(ctx, instance, portMappings); err != nil {
		return err
	}
	if err := instanceDestroyRefreshHostResources(ctx, instance.HostID); err != nil {
		return err
	}

	db.CreateLog(ctx, instanceDestroyInt64Pointer(int64(input.User.ID)), instanceDestroyLogModule, "instance.user_destroy",
		fmt.Sprintf(`User destroyed instance "%s" (Host: %s, Refund: ¥%s, Fee: ¥%s)`,
			instance.Name, input.HostName,
			instanceDestroyFixed2(input.RefundAmount), instanceDestroyFixed2(input.FeeAmount)),
		db.LogResultSuccess, db.CreateLogOptions{})

	lib.SendNotification(ctx, int64(input.User.ID), lib.EventInstanceDeletedRefunded, lib.EventData{
		"instanceName":   instance.Name,
		"refundAmount":   input.RefundAmount,
		"feeAmount":      input.FeeAmount,
		"isFreeInstance": input.IsFreeInstance,
	})

	if !input.IsFreeInstance {
		if email := instanceDestroyUserEmail(ctx, instance.UserID); email != "" {
			fee := input.FeeAmount
			result := lib.SendInstanceDestroyRefundEmail(ctx, email, lib.InstanceDestroyRefundEmailData{
				Username:      instanceDestroyUsername(ctx, instance.UserID),
				InstanceName:  instance.Name,
				RefundAmount:  input.RefundAmount,
				FeeAmount:     &fee,
				RefundType:    "余额退款",
				IsUserDestroy: true,
			})
			if !result.Success {
				logger.Printf("发送销毁退款邮件失败: %s", result.Error)
			}
		}
	}

	if instance.PackageID != nil {
		// Fire-and-forget in the TypeScript; the Go notifier never fails the request.
		go lib.SendReleaseNotification(context.WithoutCancel(ctx), lib.ReleaseNotificationParams{
			PackageID: *instance.PackageID,
			CPU:       instance.CPU,
			Memory:    instance.Memory,
			Source:    "用户销毁实例",
		})
	}
	return nil
}

func instanceDestroyFailure(instance *models.Instance) instanceDestroyBatchResultItem {
	reason := "销毁实例失败"
	return instanceDestroyBatchResultItem{
		ID: instance.ID, Name: instance.Name, Success: false, Reason: &reason,
	}
}

func instanceDestroyInt64Pointer(value int64) *int64 { return &value }

func instanceDestroyUserEmail(ctx context.Context, userID int32) string {
	var rows []struct {
		Email *string `gorm:"column:email"`
	}
	if err := db.DB.WithContext(ctx).Raw(`SELECT email FROM users WHERE id = ?`, userID).Scan(&rows).Error; err != nil {
		return ""
	}
	if len(rows) == 0 || rows[0].Email == nil {
		return ""
	}
	return *rows[0].Email
}

func instanceDestroyUsername(ctx context.Context, userID int32) string {
	var rows []struct {
		Username string `gorm:"column:username"`
	}
	if err := db.DB.WithContext(ctx).Raw(`SELECT username FROM users WHERE id = ?`, userID).Scan(&rows).Error; err != nil {
		return ""
	}
	if len(rows) == 0 {
		return ""
	}
	return rows[0].Username
}

// ==================== batch preview ====================

// instanceDestroyBuildBatchPreviewItem mirrors buildBatchDestroyPreviewItem.
func instanceDestroyBuildBatchPreviewItem(ctx context.Context, userID, instanceID int32) (instanceDestroyBatchPreviewItem, error) {
	hiddenView := func() instanceDestroyBatchPreviewItem {
		return instanceDestroyBatchPreviewItem{
			ID:                  instanceID,
			Name:                fmt.Sprintf("#%d", instanceID),
			CanDestroy:          false,
			CannotDestroyReason: instanceDestroyBatchHiddenReason,
			IsFreeInstance:      true,
			IsFirstTime:         false,
			FeeWaiverEligible:   false,
			Refund:              instanceDestroyEmptyRefund(),
			Instance: instanceDestroyInstanceView{
				ID: instanceID, Name: fmt.Sprintf("#%d", instanceID), HostName: "-", PlanName: nil,
			},
		}
	}

	view, err := instanceDestroyLoadInstance(ctx, instanceID)
	if err != nil {
		return instanceDestroyBatchPreviewItem{}, err
	}
	if view == nil || view.Instance.UserID != userID {
		return hiddenView(), nil
	}

	instance := view.Instance
	base := instanceDestroyBatchPreviewItem{
		ID:                  instance.ID,
		Name:                instance.Name,
		CanDestroy:          false,
		CannotDestroyReason: "",
		IsFreeInstance:      instance.PackagePlanID == nil,
		IsFirstTime:         false,
		FeeWaiverEligible:   false,
		Refund:              instanceDestroyEmptyRefund(),
		Instance: instanceDestroyInstanceView{
			ID: instance.ID, Name: instance.Name, HostName: view.HostName, PlanName: view.PlanName,
		},
	}

	if instance.Status == models.InstanceStatusDeleted {
		base.CannotDestroyReason = "实例已删除"
		return base, nil
	}
	if instance.Status == models.InstanceStatusCreating {
		base.CannotDestroyReason = "实例正在创建中，无法销毁"
		return base, nil
	}
	now := time.Now().UTC()
	if instance.Status == models.InstanceStatusSuspended &&
		!instanceDestroyCanDestroyExpiredSuspendedPaid(instance, now) {
		base.CannotDestroyReason = "实例已被封停，无法销毁，请先联系管理员解封"
		return base, nil
	}

	isErrorState := instance.Status == models.InstanceStatusError
	feeWaiver := isErrorState
	isFreeInstance := instance.PackagePlanID == nil
	destroyCount, err := instanceDestroyUserDestroyCount(ctx, userID)
	if err != nil {
		return instanceDestroyBatchPreviewItem{}, err
	}

	quote := instanceBillingRefundQuote{}
	if instanceDestroyIsPaidQuoteEligible(instance, isFreeInstance) {
		quote, err = instanceBillingCalculateRefundQuote(ctx, instance)
		if err != nil {
			return instanceDestroyBatchPreviewItem{}, err
		}
	}

	info := instanceDestroyQuote(quote, destroyCount, feeWaiver, isFreeInstance, isErrorState)
	base.CanDestroy = true
	// The TypeScript runs this check INSIDE the `!isFreeInstance && expiresAt &&
	// billingPrice && billingCycle` block, so a paid instance without a complete
	// billing setup is never blocked by traffic here (POST /destroy has no such
	// guard, see the destroy handler).
	if instanceDestroyIsPaidQuoteEligible(instance, isFreeInstance) &&
		!instanceDestroyCanDestroyExpiredSuspendedPaid(instance, now) &&
		instanceDestroyExceedsPaidDestroyTrafficLimit(instance.MonthlyTrafficUsed) {
		base.CanDestroy = false
		base.CannotDestroyReason = instanceDestroyTrafficLimitReason
	}
	base.IsFreeInstance = isFreeInstance
	base.IsFirstTime = info.IsFirstTime
	base.FeeWaiverEligible = info.FeeWaiverEligible
	base.Refund = info.Refund
	return base, nil
}

// ==================== batch execute ====================

// instanceDestroyExecuteForUser mirrors executeDestroyForUser. A non-nil error is
// a failure the TypeScript would let escape the batch loop (the batch handler then
// answers 500 instead of a per-item result).
func instanceDestroyExecuteForUser(
	ctx context.Context, user *lib.AccessTokenClaims, instanceID int32, logger *log.Logger,
) (instanceDestroyBatchResultItem, error) {
	if logger == nil {
		logger = log.Default()
	}

	view, err := instanceDestroyLoadInstance(ctx, instanceID)
	if err != nil {
		return instanceDestroyBatchResultItem{}, err
	}
	if view == nil || view.Instance.UserID != user.ID {
		reason := instanceDestroyBatchHiddenReason
		return instanceDestroyBatchResultItem{
			ID: instanceID, Name: fmt.Sprintf("#%d", instanceID), Success: false, Reason: &reason,
		}, nil
	}

	instance := view.Instance
	if instance.Status == models.InstanceStatusDeleted {
		reason, skipped := "实例已删除", true
		return instanceDestroyBatchResultItem{
			ID: instance.ID, Name: instance.Name, Success: false, Skipped: &skipped, Reason: &reason,
		}, nil
	}
	if instance.Status == models.InstanceStatusCreating {
		reason, skipped := "实例正在创建中，无法销毁", true
		return instanceDestroyBatchResultItem{
			ID: instance.ID, Name: instance.Name, Success: false, Skipped: &skipped, Reason: &reason,
		}, nil
	}
	now := time.Now().UTC()
	if instance.Status == models.InstanceStatusSuspended &&
		!instanceDestroyCanDestroyExpiredSuspendedPaid(instance, now) {
		reason, skipped := "实例已被封停，无法销毁，请先联系管理员解封", true
		return instanceDestroyBatchResultItem{
			ID: instance.ID, Name: instance.Name, Success: false, Skipped: &skipped, Reason: &reason,
		}, nil
	}

	blocking, err := instanceDestroyGetBlockingTask(ctx, nil, instanceID)
	if err != nil {
		return instanceDestroyBatchResultItem{}, err
	}
	if blocking != nil {
		reason, skipped := blocking.Reason, true
		return instanceDestroyBatchResultItem{
			ID: instance.ID, Name: instance.Name, Success: false, Skipped: &skipped, Reason: &reason,
		}, nil
	}

	isFreeInstance := instance.PackagePlanID == nil
	feeWaiver := instance.Status == models.InstanceStatusError

	destroyCount, err := instanceDestroyUserDestroyCount(ctx, user.ID)
	if err != nil {
		return instanceDestroyBatchResultItem{}, err
	}

	currentMonthlyTrafficUsed := instance.MonthlyTrafficUsed
	if !isFreeInstance && instance.Status == models.InstanceStatusRunning {
		collectResult := services.CollectTrafficForRunningInstance(ctx, instanceID, lib.LockOptions{})
		if !collectResult.Success {
			logger.Printf("销毁前即时采集流量失败 (instance=%d): %s", instanceID, collectResult.Error)
		} else {
			currentMonthlyTrafficUsed = collectResult.CurrentUsage
		}
	}

	if !isFreeInstance && !instanceDestroyCanDestroyExpiredSuspendedPaid(instance, now) &&
		instanceDestroyExceedsPaidDestroyTrafficLimit(currentMonthlyTrafficUsed) {
		reason, skipped := instanceDestroyTrafficLimitReason, true
		return instanceDestroyBatchResultItem{
			ID: instance.ID, Name: instance.Name, Success: false, Skipped: &skipped, Reason: &reason,
		}, nil
	}

	refundableValue := 0.0
	if instanceDestroyIsPaidQuoteEligible(instance, isFreeInstance) {
		quote, quoteErr := instanceBillingCalculateRefundQuote(ctx, instance)
		if quoteErr != nil {
			return instanceDestroyBatchResultItem{}, quoteErr
		}
		if quote.IsPaid {
			refundableValue = quote.Refundable
		}
	}

	return instanceDestroyRunDestroyedInstance(ctx, instanceDestroyRunInput{
		User:           user,
		Instance:       instance,
		HostName:       view.HostName,
		IsFreeInstance: isFreeInstance,
		FeeWaiver:      feeWaiver,
		Refundable:     refundableValue,
		DestroyCount:   destroyCount,
		Logger:         logger,
	})
}

// ==================== rate limiting (route-level config) ====================

// instanceDestroyLimiter mirrors the route-level `config: { rateLimit: ... }` of the
// TypeScript. Those four limits are not in config/rate-limit.ts, so the generated
// middleware table has no rule for them and the global limit would otherwise
// apply — a destructive endpoint must keep its own budget.
type instanceDestroyLimiterStore struct {
	mu      sync.Mutex
	buckets map[string]instanceDestroyLimiterBucket
}

type instanceDestroyLimiterBucket struct {
	count   int
	resetAt time.Time
}

var instanceDestroyLimiter = &instanceDestroyLimiterStore{buckets: map[string]instanceDestroyLimiterBucket{}}

// instanceDestroyRateLimit is a fixed-window limiter keyed by
// ip:method:route-pattern, matching @fastify/rate-limit's per-route store.
func instanceDestroyRateLimit(route string, max int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := middleware.ClientIP(c) + ":" + c.Request.Method + ":" + route
		now := time.Now()
		instanceDestroyLimiter.mu.Lock()
		bucket, ok := instanceDestroyLimiter.buckets[key]
		if !ok || now.After(bucket.resetAt) {
			bucket = instanceDestroyLimiterBucket{count: 0, resetAt: now.Add(window)}
		}
		bucket.count++
		allowed := bucket.count <= max
		instanceDestroyLimiter.buckets[key] = bucket
		instanceDestroyLimiter.mu.Unlock()

		if !allowed {
			// @fastify/rate-limit's errorResponseBuilder message.
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "Too many requests, please try again later",
			})
			return
		}
		c.Next()
	}
}

// ResetInstanceDestroyRateLimits clears the process-local buckets (tests only).
func ResetInstanceDestroyRateLimits() {
	instanceDestroyLimiter.mu.Lock()
	instanceDestroyLimiter.buckets = map[string]instanceDestroyLimiterBucket{}
	instanceDestroyLimiter.mu.Unlock()
}

// ==================== route parsing ====================

// instanceDestroyParseRouteID mirrors the route's `Number(request.params.id)`:
// a non-numeric id is 400 INVALID_ID, an out-of-range id would have crashed Prisma
// with a 500 and is treated the same here.
func instanceDestroyParseRouteID(c *gin.Context) (int32, bool) {
	raw := strings.TrimSpace(c.Param("id"))
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return 0, false
	}
	if value != math.Trunc(value) || value < math.MinInt32 || value > math.MaxInt32 {
		// Prisma rejects a non-integer/overflow Int argument → Fastify surfaces 500.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return 0, false
	}
	return int32(value), true
}

// instanceDestroyValidateInstanceIDs mirrors the route's body schema
// (`instanceIds: { type: 'array', items: { type: 'integer' }, minItems: 1,
// maxItems: 100 }`) under Fastify's default AJV options, which include
// `coerceTypes: 'array'`. That last detail is observable: a scalar body value is
// wrapped into a single-element array, so `{"instanceIds": "950001"}` is accepted
// and means `[950001]` (measured against the Node reference).
//
// The returned slice is deduplicated in first-seen order, like
// `Array.from(new Set(instanceIds))`.
func instanceDestroyValidateInstanceIDs(object map[string]any) ([]int32, string) {
	raw, present := object["instanceIds"]
	if !present {
		return nil, "instanceIds is required"
	}

	items, ok := raw.([]any)
	if !ok {
		switch typed := raw.(type) {
		case string, float64, bool:
			items = []any{typed}
		default:
			return nil, "must be array"
		}
	}
	if len(items) < 1 {
		return nil, "must NOT have fewer than 1 items"
	}
	if len(items) > 100 {
		return nil, "must NOT have more than 100 items"
	}

	seen := map[int32]bool{}
	unique := make([]int32, 0, len(items))
	for _, item := range items {
		value, ok := instanceBillingAJVInteger(item)
		if !ok {
			return nil, "must be integer"
		}
		converted := int32(value)
		if !seen[converted] {
			seen[converted] = true
			unique = append(unique, converted)
		}
	}
	return unique, ""
}

// ==================== handlers ====================

// instanceDestroyBatchDestroyInfo handles POST /api/instances/batch/destroy-info.
func instanceDestroyBatchDestroyInfo(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object, validationMessage := instanceBillingBodyObject(body.Value)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	uniqueIDs, validationMessage := instanceDestroyValidateInstanceIDs(object)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	if len(uniqueIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择至少一个实例"})
		return
	}

	items := make([]instanceDestroyBatchPreviewItem, 0, len(uniqueIDs))
	for _, instanceID := range uniqueIDs {
		item, err := instanceDestroyBuildBatchPreviewItem(ctx, user.ID, instanceID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// instanceDestroyBatchDestroy handles POST /api/instances/batch/destroy.
func instanceDestroyBatchDestroy(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object, validationMessage := instanceBillingBodyObject(body.Value)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	uniqueIDs, validationMessage := instanceDestroyValidateInstanceIDs(object)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}
	if len(uniqueIDs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择至少一个实例"})
		return
	}

	logger := log.Default()
	results := make([]instanceDestroyBatchResultItem, 0, len(uniqueIDs))
	for _, instanceID := range uniqueIDs {
		item, err := instanceDestroyExecuteForUser(ctx, user, instanceID, logger)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		results = append(results, item)
	}

	successCount := 0
	skippedCount := 0
	for _, item := range results {
		if item.Success {
			successCount++
		}
		if item.Skipped != nil && *item.Skipped {
			skippedCount++
		}
	}
	failedCount := len(results) - successCount - skippedCount

	result := db.LogResultSuccess
	if failedCount > 0 {
		result = db.LogResultWarning
	}
	db.CreateLog(ctx, instanceDestroyInt64Pointer(int64(user.ID)), instanceDestroyLogModule, "instance.batch_destroy",
		fmt.Sprintf("Batch destroy processed %d instance(s): success=%d, skipped=%d, failed=%d",
			len(results), successCount, skippedCount, failedCount),
		result, db.CreateLogOptions{})

	responseMessage := "没有实例完成销毁"
	if successCount > 0 {
		responseMessage = "批量销毁已处理"
	}
	c.JSON(http.StatusOK, gin.H{
		"message":      responseMessage,
		"successCount": successCount,
		"skippedCount": skippedCount,
		"failedCount":  failedCount,
		"results":      results,
	})
}

// instanceDestroyInfo handles GET /api/instances/:id/destroy-info.
func instanceDestroyInfo(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, ok := instanceDestroyParseRouteID(c)
	if !ok {
		return
	}

	view, err := instanceDestroyLoadInstance(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if view == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance := view.Instance
	if instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	instanceBody := instanceDestroyInstanceView{
		ID: instance.ID, Name: instance.Name, HostName: view.HostName, PlanName: view.PlanName,
	}

	if instance.Status == models.InstanceStatusDeleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已删除", "canDestroy": false})
		return
	}
	if instance.Status == models.InstanceStatusCreating {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例正在创建中，无法销毁", "canDestroy": false})
		return
	}
	now := time.Now().UTC()
	if instance.Status == models.InstanceStatusSuspended &&
		!instanceDestroyCanDestroyExpiredSuspendedPaid(instance, now) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已被封停，无法销毁，请先联系管理员解封", "canDestroy": false})
		return
	}

	isErrorState := instance.Status == models.InstanceStatusError
	feeWaiver := c.Query("feeWaiver") == "error" && isErrorState
	isFreeInstance := instance.PackagePlanID == nil

	destroyCount, err := instanceDestroyUserDestroyCount(ctx, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	quote := instanceBillingRefundQuote{}
	if instanceDestroyIsPaidQuoteEligible(instance, isFreeInstance) {
		quote, err = instanceBillingCalculateRefundQuote(ctx, instance)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
	}

	info := instanceDestroyQuote(quote, destroyCount, feeWaiver, isFreeInstance, isErrorState)
	info.Instance = instanceBody
	// Inside the paid-quote block in the TypeScript (see the batch preview note).
	if instanceDestroyIsPaidQuoteEligible(instance, isFreeInstance) &&
		!instanceDestroyCanDestroyExpiredSuspendedPaid(instance, now) &&
		instanceDestroyExceedsPaidDestroyTrafficLimit(instance.MonthlyTrafficUsed) {
		info.CanDestroy = false
		info.CannotDestroyReason = instanceDestroyTrafficLimitReason
	}
	c.JSON(http.StatusOK, info)
}

// instanceDestroyDestroy handles POST /api/instances/:id/destroy.
func instanceDestroyDestroy(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)

	instanceID, ok := instanceDestroyParseRouteID(c)
	if !ok {
		return
	}

	view, err := instanceDestroyLoadInstance(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if view == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrInstanceNotFound, "")
		return
	}
	instance := view.Instance
	if instance.UserID != user.ID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}
	if instance.Status == models.InstanceStatusDeleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已删除"})
		return
	}
	if instance.Status == models.InstanceStatusCreating {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例正在创建中，无法销毁"})
		return
	}
	now := time.Now().UTC()
	if instance.Status == models.InstanceStatusSuspended &&
		!instanceDestroyCanDestroyExpiredSuspendedPaid(instance, now) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已被封停，无法销毁，请先联系管理员解封"})
		return
	}

	blocking, err := instanceDestroyGetBlockingTask(ctx, nil, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}
	if blocking != nil {
		c.JSON(http.StatusConflict, gin.H{"error": blocking.Reason, "code": blocking.Code})
		return
	}

	isFreeInstance := instance.PackagePlanID == nil
	isErrorState := instance.Status == models.InstanceStatusError
	feeWaiver := c.Query("feeWaiver") == "error" && isErrorState

	destroyCount, err := instanceDestroyUserDestroyCount(ctx, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	currentMonthlyTrafficUsed := instance.MonthlyTrafficUsed
	if !isFreeInstance && instance.Status == models.InstanceStatusRunning {
		collectResult := services.CollectTrafficForRunningInstance(ctx, instanceID, lib.LockOptions{})
		if !collectResult.Success {
			log.Printf("销毁前即时采集流量失败 (instance=%d): %s", instanceID, collectResult.Error)
		} else {
			currentMonthlyTrafficUsed = collectResult.CurrentUsage
		}
	}

	if !isFreeInstance && !instanceDestroyCanDestroyExpiredSuspendedPaid(instance, now) &&
		instanceDestroyExceedsPaidDestroyTrafficLimit(currentMonthlyTrafficUsed) {
		lib.RespondAPIError(c, http.StatusBadRequest,
			lib.ErrInstanceDestroyTrafficLimitExceeded, instanceDestroyTrafficLimitReason)
		return
	}

	refundableValue := 0.0
	if instanceDestroyIsPaidQuoteEligible(instance, isFreeInstance) {
		quote, quoteErr := instanceBillingCalculateRefundQuote(ctx, instance)
		if quoteErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
			return
		}
		if quote.IsPaid {
			refundableValue = quote.Refundable
		}
	}

	result, runErr := instanceDestroyRunDestroyedInstance(ctx, instanceDestroyRunInput{
		User:           user,
		Instance:       instance,
		HostName:       view.HostName,
		IsFreeInstance: isFreeInstance,
		FeeWaiver:      feeWaiver,
		Refundable:     refundableValue,
		DestroyCount:   destroyCount,
		Logger:         log.Default(),
	})
	if runErr != nil {
		// Outside the TypeScript's inner try/catch → Fastify's generic 500.
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
		return
	}

	if !result.Success {
		// The TypeScript answers 409 only for the claim race; every other failure
		// (Incus, settlement) falls through to its 500 catch block.
		if result.Skipped != nil && *result.Skipped && result.Reason != nil &&
			*result.Reason == "实例正在销毁或已删除" {
			c.JSON(http.StatusConflict, gin.H{
				"error": *result.Reason,
				"code":  "INSTANCE_DESTROY_IN_PROGRESS",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "销毁实例失败"})
		return
	}

	refundAmount := 0.0
	if result.RefundAmount != nil {
		refundAmount = *result.RefundAmount
	}
	feeAmount := 0.0
	if result.FeeAmount != nil {
		feeAmount = *result.FeeAmount
	}
	responseMessage := "实例已销毁"
	if !isFreeInstance {
		responseMessage = "实例已销毁"
		if refundAmount > 0 {
			responseMessage += "，已退款 ¥" + instanceDestroyFixed2(refundAmount)
		}
		if feeAmount > 0 {
			responseMessage += "（手续费 ¥" + instanceDestroyFixed2(feeAmount) + "）"
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success":        true,
		"message":        responseMessage,
		"refundAmount":   refundAmount,
		"feeAmount":      feeAmount,
		"isFirstTime":    result.IsFirstTime != nil && *result.IsFirstTime,
		"isFreeInstance": isFreeInstance,
	})
}

// ==================== route registration ====================

// RegisterInstanceDestroy mounts the instance-destroy routes under /api/instances,
// matching `fastify.register(instanceDestroyRoutes, { prefix: '/api/instances' })`.
func RegisterInstanceDestroy(r *gin.Engine) {
	group := r.Group("/api/instances")

	group.POST("/batch/destroy-info",
		instanceDestroyRateLimit("/batch/destroy-info", 10, time.Minute),
		middleware.AuthenticateUser(), instanceDestroyBatchDestroyInfo)
	group.POST("/batch/destroy",
		instanceDestroyRateLimit("/batch/destroy", 3, time.Minute),
		middleware.AuthenticateUser(), instanceDestroyBatchDestroy)
	group.GET("/:id/destroy-info",
		instanceDestroyRateLimit("/:id/destroy-info", 30, time.Minute),
		middleware.Authenticate(), instanceDestroyInfo)
	group.POST("/:id/destroy",
		instanceDestroyRateLimit("/:id/destroy", 5, time.Minute),
		middleware.Authenticate(), instanceDestroyDestroy)
}

func init() {
	Register(RegisterInstanceDestroy)
}
