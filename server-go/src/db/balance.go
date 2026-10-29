// Port of server/src/db/balance.ts.
//
// Balance mutations are transaction-safe: every change takes the user-scoped
// PostgreSQL advisory lock (USER_BALANCE_LOCK_NAMESPACE) on the *same* pgx
// transaction, re-reads the balance, applies a guarded UPDATE and writes a
// balance_logs row. Because the lock is transaction-scoped, the whole critical
// section must run inside one pgx.Tx — see db/advisory_locks.go.
//
// Money is modelled with models.Decimal everywhere (JSON-serialized as a string,
// matching Prisma's Decimal), never float64.
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// maxBalanceAmount mirrors MAX_BALANCE_AMOUNT.
var maxBalanceAmount = decimal.RequireFromString("99999999.99")

// balanceLogTypes mirrors BALANCE_LOG_TYPES: the only types accepted by
// normalizeBalanceLogType (exchange_* / gift_card_issue are deliberately excluded).
var balanceLogTypes = map[models.BalanceLogType]bool{
	models.BalanceLogTypeRecharge:       true,
	models.BalanceLogTypeConsume:        true,
	models.BalanceLogTypeRefund:         true,
	models.BalanceLogTypeAdminAdjust:    true,
	models.BalanceLogTypeGift:           true,
	models.BalanceLogTypeTransferFee:    true,
	models.BalanceLogTypeTransferRefund: true,
	models.BalanceLogTypeInviteGenerate: true,
}

// ==================== types ====================

// BalanceInstanceRef is the {id, name} instance projection attached to balance logs.
type BalanceInstanceRef struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

// BalanceLogWithInstance mirrors BalanceLogWithInstance (`{...log, instance}`).
type BalanceLogWithInstance struct {
	models.BalanceLog
	Instance *BalanceInstanceRef `json:"instance"`
}

// BalanceLogListOptions mirrors getBalanceLogs' options object.
type BalanceLogListOptions struct {
	Page        int
	PageSize    int
	Type        models.BalanceLogType
	LotteryGift string // "exclude" | "only" | ""
}

// BalanceLogListResult mirrors getBalanceLogs' return shape.
type BalanceLogListResult struct {
	Logs     []BalanceLogWithInstance `json:"logs"`
	Total    int64                    `json:"total"`
	Page     int                      `json:"page"`
	PageSize int                      `json:"pageSize"`
}

// BalanceChangeInput mirrors BalanceChangeInput. Amount is signed: positive adds,
// negative subtracts.
type BalanceChangeInput struct {
	UserID     int32
	Type       models.BalanceLogType
	Amount     models.Decimal
	OrderID    *string
	InstanceID *int32
	Remark     *string
}

// BalanceChangeResult mirrors BalanceChangeResult.
type BalanceChangeResult struct {
	Success    bool               `json:"success"`
	BalanceLog *models.BalanceLog `json:"balanceLog,omitempty"`
	NewBalance *float64           `json:"newBalance,omitempty"`
	Error      string             `json:"error,omitempty"`
}

// BalanceAdjustmentUserRef is the {id, username, email} user projection.
type BalanceAdjustmentUserRef struct {
	ID       int32   `json:"id"`
	Username string  `json:"username"`
	Email    *string `json:"email"`
}

// BalanceAdjustmentRequestWithRelations mirrors BalanceAdjustmentRequestWithRelations.
// The outer relation fields shadow the identically named `gorm:"-"` fields on the
// embedded model, so JSON only carries the loaded relations.
type BalanceAdjustmentRequestWithRelations struct {
	models.BalanceAdjustmentRequest
	User        *BalanceAdjustmentUserRef `json:"user"`
	RequestedBy *BalanceAdjustmentUserRef `json:"requestedBy"`
	ReviewedBy  *BalanceAdjustmentUserRef `json:"reviewedBy"`
	BalanceLog  *models.BalanceLog        `json:"balanceLog"`
}

// BalanceAdjustmentRequestInput mirrors BalanceAdjustmentRequestInput.
type BalanceAdjustmentRequestInput struct {
	UserID            int32
	RequestedByUserID int32
	Amount            models.Decimal
	RequestType       models.BalanceAdjustmentRequestType
	Reason            string
	SourceType        *string
	SourceID          *int32
	OrderNo           *string
}

// BalanceAdjustmentRequestListOptions mirrors BalanceAdjustmentRequestListOptions.
// Zero values mean "not set".
type BalanceAdjustmentRequestListOptions struct {
	Page     int
	PageSize int
	Status   models.BalanceAdjustmentRequestStatus
	UserID   int32
}

// BalanceAdjustmentRequestPage mirrors getBalanceAdjustmentRequests' return shape.
type BalanceAdjustmentRequestPage struct {
	Requests []BalanceAdjustmentRequestWithRelations `json:"requests"`
	Total    int64                                   `json:"total"`
	Page     int                                     `json:"page"`
	PageSize int                                     `json:"pageSize"`
}

// ApproveBalanceAdjustmentResult mirrors approveBalanceAdjustmentRequest's return shape.
type ApproveBalanceAdjustmentResult struct {
	Request    BalanceAdjustmentRequestWithRelations `json:"request"`
	NewBalance float64                               `json:"newBalance"`
}

// UserConsumeStats mirrors getUserConsumeStats' return shape.
type UserConsumeStats struct {
	TotalRecharge       float64 `json:"totalRecharge"`
	TotalConsume        float64 `json:"totalConsume"`
	TotalRefund         float64 `json:"totalRefund"`
	TotalDestroyedValue float64 `json:"totalDestroyedValue"`
}

// ==================== helpers ====================

func zeroDecimal() models.Decimal {
	return models.Decimal{Decimal: decimal.Zero}
}

// decimalToFloat64 mirrors the JS `Number(decimal)` / `toNumber()` conversions the
// Node balance layer applies before returning money to callers. Functions whose
// TypeScript signature is `Promise<number>` must return float64 so the JSON is a
// number, not a Prisma-style decimal string.
func decimalToFloat64(value models.Decimal) float64 {
	result, _ := value.Decimal.Float64()
	return result
}

// normalizeBalanceAmount mirrors normalizeBalanceAmount: round to 2 decimals and
// reject zero or out-of-range amounts.
func normalizeBalanceAmount(value models.Decimal) (models.Decimal, error) {
	normalized := value.Round(2)
	if normalized.IsZero() || normalized.Abs().GreaterThan(maxBalanceAmount) {
		return models.Decimal{}, errors.New("余额变动金额无效")
	}
	return models.Decimal{Decimal: normalized}, nil
}

// clampPagination mirrors clampPagination with its 20/100 defaults. A zero
// page/pageSize means "undefined" in the Node options object.
func clampPagination(page, pageSize int) (int, int) {
	clampedPage := 1
	if page > 0 {
		clampedPage = page
	}

	clampedPageSize := 20
	if pageSize != 0 {
		clampedPageSize = pageSize
		if clampedPageSize < 1 {
			clampedPageSize = 1
		}
		if clampedPageSize > 100 {
			clampedPageSize = 100
		}
	}
	return clampedPage, clampedPageSize
}

// normalizeBalanceLogType mirrors normalizeBalanceLogType.
func normalizeBalanceLogType(logType models.BalanceLogType) models.BalanceLogType {
	if logType != "" && balanceLogTypes[logType] {
		return logType
	}
	return ""
}

// beginBalanceTx starts the pgx transaction every lock-guarded mutation needs.
func beginBalanceTx(ctx context.Context) (pgx.Tx, error) {
	if pool == nil {
		return nil, errors.New("database not initialized")
	}
	return pool.Begin(ctx)
}

// ==================== reads ====================

// GetUserBalance mirrors getUserBalance: the user's balance, or zero when the
// user does not exist. TypeScript returns `Promise<number>`.
func GetUserBalance(ctx context.Context, userID int32) (float64, error) {
	var user models.User
	err := DB.WithContext(ctx).Select("balance").Where("id = ?", userID).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return decimalToFloat64(user.Balance), nil
}

// GetBalanceLogs mirrors getBalanceLogs, including the lottery-gift filters and
// the attached instance projection.
func GetBalanceLogs(ctx context.Context, userID int32, options BalanceLogListOptions) (BalanceLogListResult, error) {
	page, pageSize := clampPagination(options.Page, options.PageSize)
	logType := normalizeBalanceLogType(options.Type)
	skip := (page - 1) * pageSize

	build := func() *gorm.DB {
		query := DB.WithContext(ctx).Model(&models.BalanceLog{}).Where("user_id = ?", userID)
		if logType != "" {
			query = query.Where("type = ?", string(logType))
		}
		switch options.LotteryGift {
		case "exclude":
			// 抽奖赠送特征：type='gift' AND remark 包含 '抽奖中奖'
			query = query.Where("NOT (type = ? AND remark LIKE ?)", string(models.BalanceLogTypeGift), "%抽奖中奖%")
		case "only":
			query = query.Where("type = ? AND remark LIKE ?", string(models.BalanceLogTypeGift), "%抽奖中奖%")
		}
		return query
	}

	var total int64
	if err := build().Count(&total).Error; err != nil {
		return BalanceLogListResult{}, err
	}

	var logs []models.BalanceLog
	if err := build().Order("created_at DESC").Offset(skip).Limit(pageSize).Find(&logs).Error; err != nil {
		return BalanceLogListResult{}, err
	}

	instanceIDs := make([]int32, 0, len(logs))
	for _, log := range logs {
		if log.InstanceID != nil {
			instanceIDs = append(instanceIDs, *log.InstanceID)
		}
	}
	instanceMap := map[int32]BalanceInstanceRef{}
	if len(instanceIDs) > 0 {
		var instances []models.Instance
		if err := DB.WithContext(ctx).Select("id", "name").Where("id IN ?", instanceIDs).Find(&instances).Error; err != nil {
			return BalanceLogListResult{}, err
		}
		for _, instance := range instances {
			instanceMap[instance.ID] = BalanceInstanceRef{ID: instance.ID, Name: instance.Name}
		}
	}

	items := make([]BalanceLogWithInstance, 0, len(logs))
	for _, log := range logs {
		item := BalanceLogWithInstance{BalanceLog: log}
		if log.InstanceID != nil {
			if ref, ok := instanceMap[*log.InstanceID]; ok {
				refCopy := ref
				item.Instance = &refCopy
			}
		}
		items = append(items, item)
	}

	return BalanceLogListResult{Logs: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// GetBalanceAdjustmentRequests mirrors getBalanceAdjustmentRequests.
func GetBalanceAdjustmentRequests(ctx context.Context, options BalanceAdjustmentRequestListOptions) (BalanceAdjustmentRequestPage, error) {
	page, pageSize := clampPagination(options.Page, options.PageSize)
	skip := (page - 1) * pageSize

	build := func() *gorm.DB {
		query := DB.WithContext(ctx).Model(&models.BalanceAdjustmentRequest{})
		if options.Status != "" {
			query = query.Where("status = ?", string(options.Status))
		}
		if options.UserID != 0 {
			query = query.Where("user_id = ?", options.UserID)
		}
		return query
	}

	var total int64
	if err := build().Count(&total).Error; err != nil {
		return BalanceAdjustmentRequestPage{}, err
	}

	var requests []models.BalanceAdjustmentRequest
	if err := build().Order("created_at DESC").Offset(skip).Limit(pageSize).Find(&requests).Error; err != nil {
		return BalanceAdjustmentRequestPage{}, err
	}

	withRelations, err := loadBalanceAdjustmentRequestRelations(ctx, requests)
	if err != nil {
		return BalanceAdjustmentRequestPage{}, err
	}

	return BalanceAdjustmentRequestPage{Requests: withRelations, Total: total, Page: page, PageSize: pageSize}, nil
}

// ==================== balance mutations ====================

type balanceChangeOutcome struct {
	balanceLog models.BalanceLog
	newBalance models.Decimal
}

type balanceLogInsert struct {
	userID        int32
	logType       models.BalanceLogType
	amount        models.Decimal
	balanceBefore models.Decimal
	balanceAfter  models.Decimal
	orderID       *string
	instanceID    *int32
	remark        *string
}

// insertBalanceLog writes the audit row and returns it with its generated id.
func insertBalanceLog(ctx context.Context, tx pgx.Tx, input balanceLogInsert) (models.BalanceLog, error) {
	const query = `INSERT INTO balance_logs
		(user_id, type, amount, balance_before, balance_after, order_id, instance_id, remark, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		RETURNING id, created_at`

	var id int32
	var createdAt time.Time
	err := tx.QueryRow(ctx, query,
		input.userID,
		string(input.logType),
		input.amount,
		input.balanceBefore,
		input.balanceAfter,
		input.orderID,
		input.instanceID,
		input.remark,
	).Scan(&id, &createdAt)
	if err != nil {
		return models.BalanceLog{}, err
	}

	return models.BalanceLog{
		ID:            id,
		UserID:        input.userID,
		Type:          input.logType,
		Amount:        input.amount,
		BalanceBefore: input.balanceBefore,
		BalanceAfter:  input.balanceAfter,
		OrderID:       input.orderID,
		InstanceID:    input.instanceID,
		Remark:        input.remark,
		CreatedAt:     createdAt,
	}, nil
}

// changeBalanceInTransaction mirrors changeBalanceInTransaction. It must be called
// with a pgx.Tx so the advisory lock and the UPDATE share one transaction.
func changeBalanceInTransaction(ctx context.Context, tx pgx.Tx, input BalanceChangeInput) (balanceChangeOutcome, error) {
	normalizedAmount, err := normalizeBalanceAmount(input.Amount)
	if err != nil {
		return balanceChangeOutcome{}, err
	}

	locked, err := TryAdvisoryTransactionLock(ctx, tx, UserBalanceLockNamespace, input.UserID)
	if err != nil {
		return balanceChangeOutcome{}, err
	}
	if !locked {
		return balanceChangeOutcome{}, errors.New("余额正在处理，请稍后重试")
	}

	var balanceBefore models.Decimal
	err = tx.QueryRow(ctx, "SELECT balance FROM users WHERE id = $1", input.UserID).Scan(&balanceBefore)
	if errors.Is(err, pgx.ErrNoRows) {
		return balanceChangeOutcome{}, errors.New("用户不存在")
	}
	if err != nil {
		return balanceChangeOutcome{}, err
	}

	balanceAfter := models.Decimal{Decimal: balanceBefore.Decimal.Add(normalizedAmount.Decimal).Round(2)}

	if normalizedAmount.Sign() < 0 && balanceAfter.Sign() < 0 {
		return balanceChangeOutcome{}, errors.New("余额不足")
	}
	if balanceAfter.Abs().GreaterThan(maxBalanceAmount) {
		return balanceChangeOutcome{}, errors.New("余额超出系统允许范围")
	}

	var rowsAffected int64
	if normalizedAmount.Sign() < 0 {
		tag, execErr := tx.Exec(ctx,
			"UPDATE users SET balance = balance + $1, updated_at = NOW() WHERE id = $2 AND balance >= $3",
			normalizedAmount, input.UserID, normalizedAmount.Abs())
		if execErr != nil {
			return balanceChangeOutcome{}, execErr
		}
		rowsAffected = tag.RowsAffected()
	} else {
		tag, execErr := tx.Exec(ctx,
			"UPDATE users SET balance = balance + $1, updated_at = NOW() WHERE id = $2",
			normalizedAmount, input.UserID)
		if execErr != nil {
			return balanceChangeOutcome{}, execErr
		}
		rowsAffected = tag.RowsAffected()
	}
	if rowsAffected == 0 {
		return balanceChangeOutcome{}, errors.New("余额不足或并发冲突")
	}

	var persistedBalanceAfter models.Decimal
	err = tx.QueryRow(ctx, "SELECT balance FROM users WHERE id = $1", input.UserID).Scan(&persistedBalanceAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return balanceChangeOutcome{}, errors.New("用户不存在")
	}
	if err != nil {
		return balanceChangeOutcome{}, err
	}

	balanceLog, err := insertBalanceLog(ctx, tx, balanceLogInsert{
		userID:        input.UserID,
		logType:       input.Type,
		amount:        normalizedAmount,
		balanceBefore: balanceBefore,
		balanceAfter:  persistedBalanceAfter,
		orderID:       input.OrderID,
		instanceID:    input.InstanceID,
		remark:        input.Remark,
	})
	if err != nil {
		return balanceChangeOutcome{}, err
	}

	return balanceChangeOutcome{balanceLog: balanceLog, newBalance: persistedBalanceAfter}, nil
}

// ChangeBalance mirrors changeBalance: it never returns an error, surfacing
// failures through BalanceChangeResult.Error instead.
func ChangeBalance(ctx context.Context, input BalanceChangeInput) BalanceChangeResult {
	tx, err := beginBalanceTx(ctx)
	if err != nil {
		return BalanceChangeResult{Success: false, Error: err.Error()}
	}
	defer func() { _ = tx.Rollback(ctx) }()

	outcome, err := changeBalanceInTransaction(ctx, tx, input)
	if err != nil {
		return BalanceChangeResult{Success: false, Error: err.Error()}
	}
	if err := tx.Commit(ctx); err != nil {
		return BalanceChangeResult{Success: false, Error: err.Error()}
	}

	balanceLog := outcome.balanceLog
	newBalance := decimalToFloat64(outcome.newBalance)
	return BalanceChangeResult{Success: true, BalanceLog: &balanceLog, NewBalance: &newBalance}
}

// ==================== adjustment requests ====================

func loadBalanceAdjustmentRequestRelations(ctx context.Context, requests []models.BalanceAdjustmentRequest) ([]BalanceAdjustmentRequestWithRelations, error) {
	userIDs := make([]int32, 0, len(requests)*3)
	balanceLogIDs := make([]int32, 0, len(requests))
	for _, request := range requests {
		userIDs = append(userIDs, request.UserID, request.RequestedByUserID)
		if request.ReviewedByUserID != nil {
			userIDs = append(userIDs, *request.ReviewedByUserID)
		}
		if request.BalanceLogID != nil {
			balanceLogIDs = append(balanceLogIDs, *request.BalanceLogID)
		}
	}

	users := map[int32]BalanceAdjustmentUserRef{}
	if len(userIDs) > 0 {
		var rows []models.User
		if err := DB.WithContext(ctx).Select("id", "username", "email").Where("id IN ?", userIDs).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			users[row.ID] = BalanceAdjustmentUserRef{ID: row.ID, Username: row.Username, Email: row.Email}
		}
	}

	balanceLogs := map[int32]models.BalanceLog{}
	if len(balanceLogIDs) > 0 {
		var rows []models.BalanceLog
		if err := DB.WithContext(ctx).Where("id IN ?", balanceLogIDs).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			balanceLogs[row.ID] = row
		}
	}

	results := make([]BalanceAdjustmentRequestWithRelations, 0, len(requests))
	for _, request := range requests {
		item := BalanceAdjustmentRequestWithRelations{BalanceAdjustmentRequest: request}
		if user, ok := users[request.UserID]; ok {
			userCopy := user
			item.User = &userCopy
		}
		if user, ok := users[request.RequestedByUserID]; ok {
			userCopy := user
			item.RequestedBy = &userCopy
		}
		if request.ReviewedByUserID != nil {
			if user, ok := users[*request.ReviewedByUserID]; ok {
				userCopy := user
				item.ReviewedBy = &userCopy
			}
		}
		if request.BalanceLogID != nil {
			if log, ok := balanceLogs[*request.BalanceLogID]; ok {
				logCopy := log
				item.BalanceLog = &logCopy
			}
		}
		results = append(results, item)
	}
	return results, nil
}

// CreateBalanceAdjustmentRequest mirrors createBalanceAdjustmentRequest.
func CreateBalanceAdjustmentRequest(ctx context.Context, input BalanceAdjustmentRequestInput) (BalanceAdjustmentRequestWithRelations, error) {
	amount, err := normalizeBalanceAmount(input.Amount)
	if err != nil {
		return BalanceAdjustmentRequestWithRelations{}, err
	}

	row := models.BalanceAdjustmentRequest{
		UserID:            input.UserID,
		RequestedByUserID: input.RequestedByUserID,
		Amount:            amount,
		RequestType:       input.RequestType,
		Status:            models.BalanceAdjustmentRequestStatusPending,
		SourceType:        input.SourceType,
		SourceID:          input.SourceID,
		OrderNo:           input.OrderNo,
		Reason:            input.Reason,
	}
	if err := DB.WithContext(ctx).Create(&row).Error; err != nil {
		return BalanceAdjustmentRequestWithRelations{}, err
	}

	withRelations, err := loadBalanceAdjustmentRequestRelations(ctx, []models.BalanceAdjustmentRequest{row})
	if err != nil {
		return BalanceAdjustmentRequestWithRelations{}, err
	}
	return withRelations[0], nil
}

// balanceAdjustmentRequestRecord is the raw row read inside the pgx transaction.
type balanceAdjustmentRequestRecord struct {
	id                int32
	userID            int32
	requestedByUserID int32
	reviewedByUserID  *int32
	amount            models.Decimal
	requestType       string
	status            string
	sourceType        *string
	sourceID          *int32
	orderNo           *string
	reason            string
	reviewRemark      *string
	balanceLogID      *int32
	createdAt         time.Time
	updatedAt         time.Time
	reviewedAt        *time.Time
}

func (r balanceAdjustmentRequestRecord) toModel() models.BalanceAdjustmentRequest {
	return models.BalanceAdjustmentRequest{
		ID:                r.id,
		UserID:            r.userID,
		RequestedByUserID: r.requestedByUserID,
		ReviewedByUserID:  r.reviewedByUserID,
		Amount:            r.amount,
		RequestType:       models.BalanceAdjustmentRequestType(r.requestType),
		Status:            models.BalanceAdjustmentRequestStatus(r.status),
		SourceType:        r.sourceType,
		SourceID:          r.sourceID,
		OrderNo:           r.orderNo,
		Reason:            r.reason,
		ReviewRemark:      r.reviewRemark,
		BalanceLogID:      r.balanceLogID,
		CreatedAt:         r.createdAt,
		UpdatedAt:         r.updatedAt,
		ReviewedAt:        r.reviewedAt,
	}
}

const balanceAdjustmentRequestSelect = `id, user_id, requested_by_user_id, reviewed_by_user_id, amount,
	request_type::text, status::text, source_type, source_id, order_no, reason, review_remark,
	balance_log_id, created_at, updated_at, reviewed_at`

func getBalanceAdjustmentRequestRecord(ctx context.Context, tx pgx.Tx, id int32) (balanceAdjustmentRequestRecord, bool, error) {
	var record balanceAdjustmentRequestRecord
	err := tx.QueryRow(ctx,
		"SELECT "+balanceAdjustmentRequestSelect+" FROM balance_adjustment_requests WHERE id = $1", id,
	).Scan(
		&record.id, &record.userID, &record.requestedByUserID, &record.reviewedByUserID,
		&record.amount, &record.requestType, &record.status, &record.sourceType, &record.sourceID,
		&record.orderNo, &record.reason, &record.reviewRemark, &record.balanceLogID,
		&record.createdAt, &record.updatedAt, &record.reviewedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return balanceAdjustmentRequestRecord{}, false, nil
	}
	if err != nil {
		return balanceAdjustmentRequestRecord{}, false, err
	}
	return record, true, nil
}

// ApproveBalanceAdjustmentRequest mirrors approveBalanceAdjustmentRequest: claim
// the pending request, then apply the balance change in the same transaction.
func ApproveBalanceAdjustmentRequest(ctx context.Context, requestID, reviewerUserID int32, reviewRemark *string) (ApproveBalanceAdjustmentResult, error) {
	tx, err := beginBalanceTx(ctx)
	if err != nil {
		return ApproveBalanceAdjustmentResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	record, found, err := getBalanceAdjustmentRequestRecord(ctx, tx, requestID)
	if err != nil {
		return ApproveBalanceAdjustmentResult{}, err
	}
	if !found {
		return ApproveBalanceAdjustmentResult{}, errors.New("调账申请不存在")
	}
	if record.status != string(models.BalanceAdjustmentRequestStatusPending) {
		return ApproveBalanceAdjustmentResult{}, errors.New("调账申请已处理")
	}

	claimSQL := `UPDATE balance_adjustment_requests
		SET status = 'approved', reviewed_by_user_id = $2, reviewed_at = NOW(), updated_at = NOW()`
	claimArgs := []any{requestID, reviewerUserID}
	if reviewRemark != nil {
		claimSQL += `, review_remark = $3`
		claimArgs = append(claimArgs, *reviewRemark)
	}
	claimSQL += ` WHERE id = $1 AND status = 'pending'`

	claimed, err := tx.Exec(ctx, claimSQL, claimArgs...)
	if err != nil {
		return ApproveBalanceAdjustmentResult{}, err
	}
	if claimed.RowsAffected() != 1 {
		return ApproveBalanceAdjustmentResult{}, errors.New("调账申请已处理")
	}

	isRechargeBalanceReclaim := record.requestType == string(models.BalanceAdjustmentRequestTypeRefund) &&
		record.sourceType != nil && *record.sourceType == "recharge"

	logType := models.BalanceLogTypeAdminAdjust
	if record.requestType == string(models.BalanceAdjustmentRequestTypeRefund) && record.amount.Sign() > 0 && !isRechargeBalanceReclaim {
		logType = models.BalanceLogTypeRefund
	}
	balanceChangeAmount := record.amount
	if isRechargeBalanceReclaim {
		balanceChangeAmount = models.Decimal{Decimal: record.amount.Abs().Neg()}
	}

	remarkParts := []string{fmt.Sprintf("[审批通过] %s", record.reason)}
	if record.orderNo != nil && *record.orderNo != "" {
		remarkParts = append(remarkParts, "订单 "+*record.orderNo)
	}
	if isRechargeBalanceReclaim {
		remarkParts = append(remarkParts, "充值退款收回余额")
	}
	if reviewRemark != nil && *reviewRemark != "" {
		remarkParts = append(remarkParts, "审核备注: "+*reviewRemark)
	}
	remark := strings.Join(remarkParts, "；")

	var orderID *string
	if record.orderNo != nil && *record.orderNo != "" {
		orderID = record.orderNo
	}

	outcome, err := changeBalanceInTransaction(ctx, tx, BalanceChangeInput{
		UserID:  record.userID,
		Type:    logType,
		Amount:  balanceChangeAmount,
		OrderID: orderID,
		Remark:  &remark,
	})
	if err != nil {
		return ApproveBalanceAdjustmentResult{}, err
	}

	if _, err := tx.Exec(ctx,
		"UPDATE balance_adjustment_requests SET balance_log_id = $2, updated_at = NOW() WHERE id = $1",
		requestID, outcome.balanceLog.ID,
	); err != nil {
		return ApproveBalanceAdjustmentResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return ApproveBalanceAdjustmentResult{}, err
	}

	updated, err := getBalanceAdjustmentRequest(ctx, requestID)
	if err != nil {
		return ApproveBalanceAdjustmentResult{}, err
	}
	return ApproveBalanceAdjustmentResult{Request: updated, NewBalance: decimalToFloat64(outcome.newBalance)}, nil
}

// RejectBalanceAdjustmentRequest mirrors rejectBalanceAdjustmentRequest.
func RejectBalanceAdjustmentRequest(ctx context.Context, requestID, reviewerUserID int32, reviewRemark string) (BalanceAdjustmentRequestWithRelations, error) {
	tx, err := beginBalanceTx(ctx)
	if err != nil {
		return BalanceAdjustmentRequestWithRelations{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	claimed, err := tx.Exec(ctx, `UPDATE balance_adjustment_requests
		SET status = 'rejected', reviewed_by_user_id = $2, reviewed_at = NOW(), review_remark = $3, updated_at = NOW()
		WHERE id = $1 AND status = 'pending'`,
		requestID, reviewerUserID, reviewRemark,
	)
	if err != nil {
		return BalanceAdjustmentRequestWithRelations{}, err
	}
	if claimed.RowsAffected() != 1 {
		return BalanceAdjustmentRequestWithRelations{}, errors.New("调账申请不存在或已处理")
	}

	if _, found, err := getBalanceAdjustmentRequestRecord(ctx, tx, requestID); err != nil {
		return BalanceAdjustmentRequestWithRelations{}, err
	} else if !found {
		return BalanceAdjustmentRequestWithRelations{}, errors.New("调账申请不存在")
	}

	if err := tx.Commit(ctx); err != nil {
		return BalanceAdjustmentRequestWithRelations{}, err
	}

	return getBalanceAdjustmentRequest(ctx, requestID)
}

// getBalanceAdjustmentRequest reloads a request plus its relations.
func getBalanceAdjustmentRequest(ctx context.Context, requestID int32) (BalanceAdjustmentRequestWithRelations, error) {
	var request models.BalanceAdjustmentRequest
	if err := DB.WithContext(ctx).Where("id = ?", requestID).First(&request).Error; err != nil {
		return BalanceAdjustmentRequestWithRelations{}, err
	}
	withRelations, err := loadBalanceAdjustmentRequestRelations(ctx, []models.BalanceAdjustmentRequest{request})
	if err != nil {
		return BalanceAdjustmentRequestWithRelations{}, err
	}
	return withRelations[0], nil
}

// ==================== convenience wrappers ====================

// RechargeToBalance mirrors rechargeToBalance.
func RechargeToBalance(ctx context.Context, userID int32, amount models.Decimal, orderID string, remark *string) BalanceChangeResult {
	finalRemark := remark
	if finalRemark == nil || *finalRemark == "" {
		defaultRemark := fmt.Sprintf("充值订单 %s", orderID)
		finalRemark = &defaultRemark
	}
	return ChangeBalance(ctx, BalanceChangeInput{
		UserID:  userID,
		Type:    models.BalanceLogTypeRecharge,
		Amount:  amount,
		OrderID: &orderID,
		Remark:  finalRemark,
	})
}

// ConsumeBalance mirrors consumeBalance (amount is forced negative).
func ConsumeBalance(ctx context.Context, userID int32, amount models.Decimal, instanceID int32, remark *string) BalanceChangeResult {
	return ChangeBalance(ctx, BalanceChangeInput{
		UserID:     userID,
		Type:       models.BalanceLogTypeConsume,
		Amount:     models.Decimal{Decimal: amount.Abs().Neg()},
		InstanceID: &instanceID,
		Remark:     remark,
	})
}

// RefundToBalance mirrors refundToBalance (amount is forced positive).
func RefundToBalance(ctx context.Context, userID int32, amount models.Decimal, instanceID int32, remark *string) BalanceChangeResult {
	return ChangeBalance(ctx, BalanceChangeInput{
		UserID:     userID,
		Type:       models.BalanceLogTypeRefund,
		Amount:     models.Decimal{Decimal: amount.Abs()},
		InstanceID: &instanceID,
		Remark:     remark,
	})
}

// AdminAdjustBalance mirrors adminAdjustBalance.
func AdminAdjustBalance(ctx context.Context, userID int32, amount models.Decimal, remark string) BalanceChangeResult {
	return ChangeBalance(ctx, BalanceChangeInput{
		UserID: userID,
		Type:   models.BalanceLogTypeAdminAdjust,
		Amount: amount,
		Remark: &remark,
	})
}

// GiftBalance mirrors giftBalance (amount is forced positive).
func GiftBalance(ctx context.Context, userID int32, amount models.Decimal, remark *string) BalanceChangeResult {
	return ChangeBalance(ctx, BalanceChangeInput{
		UserID: userID,
		Type:   models.BalanceLogTypeGift,
		Amount: models.Decimal{Decimal: amount.Abs()},
		Remark: remark,
	})
}

// ==================== aggregation ====================

// GetUsersTotalConsumeMap mirrors getUsersTotalConsumeMap: net consume per user
// (instance consume minus instance refunds), preferring the balance delta so
// historical rows with inconsistent signs are still counted correctly.
func GetUsersTotalConsumeMap(ctx context.Context, userIDs []int32, client *gorm.DB) (map[int32]float64, error) {
	if client == nil {
		client = DB
	}

	seen := map[int32]bool{}
	validUserIDs := make([]int32, 0, len(userIDs))
	for _, id := range userIDs {
		if id > 0 && !seen[id] {
			seen[id] = true
			validUserIDs = append(validUserIDs, id)
		}
	}

	result := map[int32]float64{}
	if len(validUserIDs) == 0 {
		return result, nil
	}

	placeholders := make([]string, len(validUserIDs))
	args := make([]any, len(validUserIDs))
	for i, id := range validUserIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := `SELECT
			user_id AS user_id,
			GREATEST(COALESCE(SUM(
				CASE
					WHEN type = 'consume' THEN
						CASE WHEN balance_before > balance_after THEN balance_before - balance_after ELSE ABS(amount) END
					WHEN type = 'refund' THEN
						-1 * (CASE WHEN balance_after > balance_before THEN balance_after - balance_before ELSE ABS(amount) END)
					ELSE 0
				END
			), 0), 0)::numeric AS total_consume
		FROM balance_logs
		WHERE user_id IN (` + strings.Join(placeholders, ", ") + `)
			AND (type = 'consume' OR (type = 'refund' AND instance_id IS NOT NULL))
		GROUP BY user_id`

	var rows []struct {
		UserID       int32          `gorm:"column:user_id"`
		TotalConsume models.Decimal `gorm:"column:total_consume"`
	}
	if err := client.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.UserID] = decimalToFloat64(row.TotalConsume)
	}
	return result, nil
}

// GetUserTotalConsume mirrors getUserTotalConsume.
func GetUserTotalConsume(ctx context.Context, userID int32, client *gorm.DB) (float64, error) {
	consumeMap, err := GetUsersTotalConsumeMap(ctx, []int32{userID}, client)
	if err != nil {
		return 0, err
	}
	if value, ok := consumeMap[userID]; ok {
		return value, nil
	}
	return 0, nil
}

// HasEnoughBalance mirrors hasEnoughBalance.
func HasEnoughBalance(ctx context.Context, userID int32, amount float64) (bool, error) {
	balance, err := GetUserBalance(ctx, userID)
	if err != nil {
		return false, err
	}
	return balance >= amount, nil
}

// GetUserConsumeStats mirrors getUserConsumeStats.
func GetUserConsumeStats(ctx context.Context, userID int32) (UserConsumeStats, error) {
	var sums []struct {
		LogType string         `gorm:"column:log_type"`
		Total   models.Decimal `gorm:"column:total"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT type::text AS log_type, COALESCE(SUM(amount), 0)::numeric AS total
			FROM balance_logs WHERE user_id = ? GROUP BY type`, userID,
	).Scan(&sums).Error; err != nil {
		return UserConsumeStats{}, err
	}

	var destroyed struct {
		Total models.Decimal `gorm:"column:total"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(amount), 0)::numeric AS total FROM balance_logs
			WHERE user_id = ? AND type = 'refund' AND amount > 0 AND remark LIKE ?`,
		userID, "%用户销毁实例退款%",
	).Scan(&destroyed).Error; err != nil {
		return UserConsumeStats{}, err
	}

	totalConsume, err := GetUserTotalConsume(ctx, userID, nil)
	if err != nil {
		return UserConsumeStats{}, err
	}

	stats := UserConsumeStats{
		TotalConsume:        totalConsume,
		TotalDestroyedValue: decimalToFloat64(destroyed.Total),
	}
	for _, sum := range sums {
		switch models.BalanceLogType(sum.LogType) {
		case models.BalanceLogTypeRecharge:
			stats.TotalRecharge = decimalToFloat64(sum.Total)
		case models.BalanceLogTypeRefund:
			stats.TotalRefund = decimalToFloat64(sum.Total)
		}
	}
	return stats, nil
}

// GetInstanceTotalConsume mirrors getInstanceTotalConsume (absolute value).
func GetInstanceTotalConsume(ctx context.Context, instanceID int32) (float64, error) {
	var result struct {
		Total models.Decimal `gorm:"column:total"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(amount), 0)::numeric AS total FROM balance_logs
			WHERE instance_id = ? AND type = 'consume'`, instanceID,
	).Scan(&result).Error; err != nil {
		return 0, err
	}
	absolute, _ := result.Total.Abs().Float64()
	return absolute, nil
}

// GetInstanceTotalRefund mirrors getInstanceTotalRefund.
func GetInstanceTotalRefund(ctx context.Context, instanceID int32) (float64, error) {
	var result struct {
		Total models.Decimal `gorm:"column:total"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(amount), 0)::numeric AS total FROM balance_logs
			WHERE instance_id = ? AND type = 'refund'`, instanceID,
	).Scan(&result).Error; err != nil {
		return 0, err
	}
	return decimalToFloat64(result.Total), nil
}

// GetInstanceRefundableAmount mirrors getInstanceRefundableAmount.
func GetInstanceRefundableAmount(ctx context.Context, instanceID int32) (float64, error) {
	totalConsume, err := GetInstanceTotalConsume(ctx, instanceID)
	if err != nil {
		return 0, err
	}
	totalRefund, err := GetInstanceTotalRefund(ctx, instanceID)
	if err != nil {
		return 0, err
	}

	if totalConsume-totalRefund < 0 {
		return 0, nil
	}
	return totalConsume - totalRefund, nil
}
