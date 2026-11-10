// Instance-billing data layer (port of the parts of server/src/db/billing-records.ts
// and server/src/db/billing-operations.ts that do not need the promo/vip services).
//
// Why this file exists: routes/instance_billing.go needs a handful of reads and
// writes that the already-ported db modules do not expose —
//
//   - the instance row with every billing column (getInstanceById returns a
//     trimmed record without billing_cycle / expires_at as time.Time / version),
//   - the billing-record page with its migration "lineage" (billing-records.ts),
//   - the auto-renew flag update (billing-operations.ts updateAutoRenew),
//   - one balance mutation that shares a *GORM* transaction with the promo
//     settlement.
//
// The last point deserves an explanation. Node runs performRenewal in ONE Prisma
// transaction: balance decrement + balance_log snapshot + instance expiry bump +
// instance_billing_records row + promo redemption/binding update. In Go the
// balance helper in db/balance.go is pgx-typed, while the promo settlement
// (services.PromoSettleOnRenew) is GORM-typed, so the two cannot share a
// transaction object. The instance-billing flow therefore opens a GORM
// transaction and applies the balance mutation here, with SQL that is a literal
// copy of changeBalanceInTransaction in db/balance.go: advisory lock on (4110,
// user), read balance, conditional decrement, re-read persisted balance, insert
// balance_log with balance_before/balance_after snapshots. Keep the two in sync.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// InstanceBillingRecordListOptions mirrors getInstanceBillingRecords' options.
//
// UserID is the Prisma `userId` filter: the route only sets it for the instance
// owner (a host owner may read the record history without the user filter), so a
// zero value means "no user filter" exactly like the TypeScript.
type InstanceBillingRecordListOptions struct {
	Page     int
	PageSize int
	Type     models.BillingRecordType
	UserID   int32
}

// InstanceBillingRecordPage mirrors getInstanceBillingRecords' return value.
type InstanceBillingRecordPage struct {
	Records  []models.InstanceBillingRecord
	Total    int64
	Page     int
	PageSize int
}

// InstanceBillingPeriod is the projection calculateInstanceRemainingRefundQuote
// reads from positive billing records.
type InstanceBillingPeriod struct {
	Amount      models.Decimal
	PeriodStart time.Time
	PeriodEnd   time.Time
}

// InstanceBillingBalanceChange is one balance mutation inside a GORM transaction.
// Delta is signed: negative charges the user, positive refunds them.
type InstanceBillingBalanceChange struct {
	UserID     int32
	InstanceID int32
	Delta      models.Decimal
	LogType    models.BalanceLogType
	Remark     string
}

// InstanceBillingBalanceResult carries the snapshot that must reach balance_logs.
type InstanceBillingBalanceResult struct {
	BalanceBefore models.Decimal
	BalanceAfter  models.Decimal
	BalanceLogID  int32
}

// TryInstanceBillingAdvisoryLock is the GORM-transaction flavour of
// TryAdvisoryTransactionLock (db/advisory_locks.go). The SQL — including the
// two-int namespace form — is identical; only the transaction handle differs,
// because services/promo_engine.go is GORM-typed and cannot join a pgx.Tx.
//
// A transaction-scoped lock is released when the transaction ends, so tx MUST be
// the same transaction that performs the guarded work.
func TryInstanceBillingAdvisoryLock(ctx context.Context, tx *gorm.DB, namespace, key int32) (bool, error) {
	return TryAdvisoryTransactionLockTx(tx.WithContext(ctx), namespace, key)
}

// InstanceBillingApplyBalanceTx mirrors the consume/refund half of
// performRenewal / performPlanChange: take the user-balance advisory lock, read
// the balance, apply the signed delta with a conditional UPDATE, re-read the
// persisted balance and write the audit row with balance_before/balance_after
// snapshots (never deltas).
//
// Error strings match server/src/db/billing-operations.ts:
// "用户不存在", "余额不足", "余额不足或并发冲突".
func InstanceBillingApplyBalanceTx(ctx context.Context, tx *gorm.DB, input InstanceBillingBalanceChange) (InstanceBillingBalanceResult, error) {
	if tx == nil {
		return InstanceBillingBalanceResult{}, errors.New("instance billing: balance change requires a transaction")
	}

	locked, err := TryInstanceBillingAdvisoryLock(ctx, tx, UserBalanceLockNamespace, input.UserID)
	if err != nil {
		return InstanceBillingBalanceResult{}, err
	}
	if !locked {
		return InstanceBillingBalanceResult{}, errors.New("余额正在处理，请稍后重试")
	}

	type balanceRow struct {
		Balance models.Decimal `gorm:"column:balance"`
	}
	var balanceRows []balanceRow
	err = tx.WithContext(ctx).Raw("SELECT balance FROM users WHERE id = ?", input.UserID).Scan(&balanceRows).Error
	if err != nil {
		return InstanceBillingBalanceResult{}, err
	}
	if len(balanceRows) == 0 {
		return InstanceBillingBalanceResult{}, errors.New("用户不存在")
	}
	balanceBefore := balanceRows[0].Balance

	delta := input.Delta.Decimal.Round(2)
	if delta.Sign() < 0 && balanceBefore.Decimal.Add(delta).Sign() < 0 {
		return InstanceBillingBalanceResult{}, errors.New("余额不足")
	}

	var result *gorm.DB
	if delta.Sign() < 0 {
		result = tx.WithContext(ctx).Exec(
			"UPDATE users SET balance = balance + ?, updated_at = ? WHERE id = ? AND balance >= ?",
			delta, time.Now().UTC(), input.UserID, delta.Abs())
	} else {
		result = tx.WithContext(ctx).Exec(
			"UPDATE users SET balance = balance + ?, updated_at = ? WHERE id = ?",
			delta, time.Now().UTC(), input.UserID)
	}
	if result.Error != nil {
		return InstanceBillingBalanceResult{}, result.Error
	}
	if result.RowsAffected == 0 {
		return InstanceBillingBalanceResult{}, errors.New("余额不足或并发冲突")
	}

	var balanceAfterRows []balanceRow
	if err := tx.WithContext(ctx).Raw("SELECT balance FROM users WHERE id = ?", input.UserID).Scan(&balanceAfterRows).Error; err != nil {
		return InstanceBillingBalanceResult{}, err
	}
	if len(balanceAfterRows) == 0 {
		return InstanceBillingBalanceResult{}, errors.New("用户不存在")
	}
	balanceAfter := balanceAfterRows[0].Balance

	balanceLog := InstanceBillingBalanceLogRow(input, balanceBefore, balanceAfter)
	if err := tx.WithContext(ctx).Create(&balanceLog).Error; err != nil {
		return InstanceBillingBalanceResult{}, err
	}

	return InstanceBillingBalanceResult{
		BalanceBefore: balanceBefore,
		BalanceAfter:  balanceAfter,
		BalanceLogID:  balanceLog.ID,
	}, nil
}

// InstanceBillingBalanceLogRow builds the balance_logs audit row from a balance
// change and the before/after snapshots. Keeping it separate makes the snapshot
// contract (balance_before / balance_after, never deltas) directly testable.
func InstanceBillingBalanceLogRow(input InstanceBillingBalanceChange, before, after models.Decimal) models.BalanceLog {
	instanceID := input.InstanceID
	remark := input.Remark
	return models.BalanceLog{
		UserID:        input.UserID,
		Type:          input.LogType,
		Amount:        models.Decimal{Decimal: input.Delta.Decimal.Round(2)},
		BalanceBefore: before,
		BalanceAfter:  after,
		InstanceID:    &instanceID,
		Remark:        &remark,
	}
}

// GetInstanceForBilling returns the full instance row, or (nil, nil) when absent.
// Prisma's findUnique with no `select` is what performRenewal/performPlanChange
// operate on, so every billing column (billing_price, billing_cycle, expires_at,
// version, ...) has to be present.
func GetInstanceForBilling(ctx context.Context, id int32) (*models.Instance, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}
	var instance models.Instance
	err := DB.WithContext(ctx).First(&instance, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &instance, nil
}

// UpdateInstanceAutoRenew mirrors updateAutoRenew: a plain update that also bumps
// updated_at via GORM's autoUpdateTime (Prisma's @updatedAt does the same).
func UpdateInstanceAutoRenew(ctx context.Context, instanceID int32, autoRenew bool) error {
	if err := instancesRequireDB(); err != nil {
		return err
	}
	return DB.WithContext(ctx).
		Model(&models.Instance{}).
		Where("id = ?", instanceID).
		Update("auto_renew", autoRenew).Error
}

// ==================== billing records + lineage ====================

// instanceBillingLineageSeed mirrors BillingLineageSeed.
type instanceBillingLineageSeed struct {
	ID        int32
	UserID    int32
	HostID    int32
	Name      string
	ExpiresAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// instanceBillingMigrationData mirrors MigrationMessageData.
type instanceBillingMigrationData struct {
	InstanceID   *json.Number `json:"instanceId"`
	OldHostID    *json.Number `json:"oldHostId"`
	NewHostID    *json.Number `json:"newHostId"`
	InstanceName *string      `json:"instanceName"`
}

func instanceBillingParseInt(value *json.Number) (int32, bool) {
	if value == nil {
		return 0, false
	}
	parsed, err := value.Int64()
	if err != nil {
		return 0, false
	}
	return int32(parsed), true
}

// GetInstanceBillingLineageIDs mirrors getInstanceBillingLineageIds: the chain
// from the earliest migrated ancestor to the current instance. The TypeScript
// exists because a host migration can create a fresh instance row while the
// billing records stay attached to the deleted predecessor; without the lineage
// the record history and the refund quote would both restart at zero.
func GetInstanceBillingLineageIDs(ctx context.Context, instanceID int32) ([]int32, error) {
	if err := instancesRequireDB(); err != nil {
		return nil, err
	}

	seed, err := instanceBillingLineageSeedByID(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	lineage := make([]int32, 0, 4)
	visited := map[int32]bool{}
	current := seed
	for current != nil && !visited[current.ID] {
		lineage = append([]int32{current.ID}, lineage...)
		visited[current.ID] = true

		predecessorID, err := instanceBillingResolveMigratedSource(ctx, *current)
		if err != nil {
			return nil, err
		}
		if predecessorID == nil || visited[*predecessorID] {
			break
		}
		current, err = instanceBillingLineageSeedByID(ctx, *predecessorID)
		if err != nil {
			return nil, err
		}
	}

	if len(lineage) == 0 {
		return []int32{instanceID}, nil
	}
	return lineage, nil
}

func instanceBillingLineageSeedByID(ctx context.Context, instanceID int32) (*instanceBillingLineageSeed, error) {
	var rows []instanceBillingLineageSeed
	err := DB.WithContext(ctx).Raw(
		`SELECT id, user_id, host_id, name, expires_at, created_at, updated_at
		   FROM instances WHERE id = ?`, instanceID,
	).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// instanceBillingResolveMigratedSource mirrors resolveImmediateMigratedBillingSource.
func instanceBillingResolveMigratedSource(ctx context.Context, seed instanceBillingLineageSeed) (*int32, error) {
	type inboxRow struct {
		ID        int32           `gorm:"column:id"`
		Data      json.RawMessage `gorm:"column:data"`
		CreatedAt time.Time       `gorm:"column:created_at"`
	}
	var messages []inboxRow
	err := DB.WithContext(ctx).Raw(
		`SELECT id, data, created_at
		   FROM inbox_messages
		  WHERE user_id = ? AND event_type = 'instance_migrated'
		  ORDER BY created_at DESC`, seed.UserID,
	).Scan(&messages).Error
	if err != nil {
		return nil, err
	}

	for _, message := range messages {
		var data instanceBillingMigrationData
		if len(message.Data) > 0 {
			_ = json.Unmarshal(message.Data, &data)
		}

		dataInstanceID, ok := instanceBillingParseInt(data.InstanceID)
		if !ok || dataInstanceID != seed.ID {
			continue
		}
		oldHostID, ok := instanceBillingParseInt(data.OldHostID)
		if !ok {
			continue
		}
		if newHostID, hasNewHost := instanceBillingParseInt(data.NewHostID); hasNewHost && newHostID != seed.HostID {
			continue
		}

		expectedName := seed.Name
		if data.InstanceName != nil && len(*data.InstanceName) > 0 {
			expectedName = *data.InstanceName
		}
		windowStart := message.CreatedAt.Add(-30 * time.Minute)
		windowEnd := message.CreatedAt.Add(30 * time.Minute)

		type candidate struct {
			ID        int32      `gorm:"column:id"`
			ExpiresAt *time.Time `gorm:"column:expires_at"`
		}
		var candidates []candidate
		err := DB.WithContext(ctx).Raw(
			`SELECT id, expires_at
			   FROM instances
			  WHERE user_id = ? AND host_id = ? AND name = ? AND status = 'deleted'
			    AND id <> ? AND updated_at >= ? AND updated_at <= ?`,
			seed.UserID, oldHostID, expectedName, seed.ID, windowStart, windowEnd,
		).Scan(&candidates).Error
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			continue
		}

		type candidateWithCount struct {
			candidate
			RecordCount int64
		}
		withCounts := make([]candidateWithCount, 0, len(candidates))
		for _, item := range candidates {
			var count int64
			if err := DB.WithContext(ctx).Model(&models.InstanceBillingRecord{}).
				Where("instance_id = ?", item.ID).Count(&count).Error; err != nil {
				return nil, err
			}
			withCounts = append(withCounts, candidateWithCount{candidate: item, RecordCount: count})
		}
		if len(withCounts) == 1 {
			return &withCounts[0].ID, nil
		}

		preferred := make([]candidateWithCount, 0, len(withCounts))
		for _, item := range withCounts {
			if instanceBillingSameDate(item.ExpiresAt, seed.ExpiresAt) {
				preferred = append(preferred, item)
			}
		}
		if len(preferred) == 1 {
			return &preferred[0].ID, nil
		}

		withRecords := make([]candidateWithCount, 0, len(withCounts))
		for _, item := range withCounts {
			if item.RecordCount > 0 {
				withRecords = append(withRecords, item)
			}
		}
		if len(withRecords) == 1 {
			return &withRecords[0].ID, nil
		}

		final := withCounts
		if len(preferred) > 0 {
			final = preferred
		}
		if len(final) == 1 {
			return &final[0].ID, nil
		}
	}

	return nil, nil
}

func instanceBillingSameDate(left, right *time.Time) bool {
	if left == nil && right == nil {
		return true
	}
	if left == nil || right == nil {
		return false
	}
	return left.Equal(*right)
}

// GetInstanceBillingRecords mirrors getInstanceBillingRecords: newest first, page
// clamped to >=1 and pageSize to [1,100], optionally filtered by type and user.
func GetInstanceBillingRecords(ctx context.Context, instanceID int32, options InstanceBillingRecordListOptions) (InstanceBillingRecordPage, error) {
	page := options.Page
	if page < 1 {
		page = 1
	}
	pageSize := options.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	lineage, err := GetInstanceBillingLineageIDs(ctx, instanceID)
	if err != nil {
		return InstanceBillingRecordPage{}, err
	}

	build := func() *gorm.DB {
		query := DB.WithContext(ctx).Model(&models.InstanceBillingRecord{}).
			Where("instance_id IN ?", lineage)
		if options.UserID > 0 {
			query = query.Where("user_id = ?", options.UserID)
		}
		if options.Type != "" {
			query = query.Where("type = ?", string(options.Type))
		}
		return query
	}

	var total int64
	if err := build().Count(&total).Error; err != nil {
		return InstanceBillingRecordPage{}, err
	}

	var records []models.InstanceBillingRecord
	err = build().
		Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&records).Error
	if err != nil {
		return InstanceBillingRecordPage{}, err
	}

	return InstanceBillingRecordPage{Records: records, Total: total, Page: page, PageSize: pageSize}, nil
}

// GetInstanceBillingMaxRefundable mirrors getMaxRefundable's maxRefundable half:
// consumed (newPurchase/renew/upgrade, amount > 0) minus refunds (absolute), never
// below zero, rounded to 2 decimals. lastPaymentAmount is unused by the billing
// quote that this module needs, so it is not returned.
func GetInstanceBillingMaxRefundable(ctx context.Context, instanceID int32) (float64, error) {
	lineage, err := GetInstanceBillingLineageIDs(ctx, instanceID)
	if err != nil {
		return 0, err
	}
	instanceIDs := lineage
	if len(instanceIDs) == 0 {
		instanceIDs = []int32{instanceID}
	}

	// SUM over an empty set is NULL, so the aggregate is scanned as text and
	// parsed only when present (numeric -> text keeps the exact decimal).
	type totalRow struct {
		Total *string `gorm:"column:total"`
	}

	var consumedRows []totalRow
	err = DB.WithContext(ctx).Raw(
		`SELECT SUM(amount)::text AS total
		   FROM instance_billing_records
		  WHERE instance_id IN ? AND type IN ('newPurchase','renew','upgrade') AND amount > 0`,
		instanceIDs,
	).Scan(&consumedRows).Error
	if err != nil {
		return 0, err
	}
	consumed := 0.0
	if len(consumedRows) > 0 && consumedRows[0].Total != nil && *consumedRows[0].Total != "" {
		parsed, parseErr := decimal.NewFromString(*consumedRows[0].Total)
		if parseErr != nil {
			return 0, parseErr
		}
		consumed = parsed.InexactFloat64()
	}

	var refundedRows []totalRow
	err = DB.WithContext(ctx).Raw(
		`SELECT SUM(ABS(amount))::text AS total
		   FROM instance_billing_records
		  WHERE instance_id IN ? AND type = 'refund'`,
		instanceIDs,
	).Scan(&refundedRows).Error
	if err != nil {
		return 0, err
	}
	refunded := 0.0
	if len(refundedRows) > 0 && refundedRows[0].Total != nil && *refundedRows[0].Total != "" {
		parsed, parseErr := decimal.NewFromString(*refundedRows[0].Total)
		if parseErr != nil {
			return 0, parseErr
		}
		refunded = parsed.InexactFloat64()
	}

	value := consumed - refunded
	if value < 0 {
		value = 0
	}
	return value, nil
}

// GetInstanceBillingPositivePeriods mirrors the positiveBillingRecords query in
// calculateInstanceRemainingRefundQuote (lineage-wide, old purchases first).
func GetInstanceBillingPositivePeriods(ctx context.Context, instanceID int32) ([]InstanceBillingPeriod, error) {
	lineage, err := GetInstanceBillingLineageIDs(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if len(lineage) == 0 {
		lineage = []int32{instanceID}
	}
	var rows []InstanceBillingPeriod
	err = DB.WithContext(ctx).Raw(
		`SELECT amount, period_start, period_end
		   FROM instance_billing_records
		  WHERE instance_id IN ? AND type IN ('newPurchase','renew','upgrade') AND amount > 0
		  ORDER BY period_start ASC`,
		lineage,
	).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}
