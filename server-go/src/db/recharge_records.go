// Port of server/src/db/recharge-records.ts.
//
// The money-critical function here is CompleteRecharge. It reproduces the Node
// state machine exactly:
//
//  1. read the order; already `completed` -> return idempotently;
//     `cancelled`/`failed` -> error;
//  2. inside ONE transaction take the user-balance advisory lock (4110), then
//     run a *conditional* UPDATE `WHERE order_no = ? AND status IN ('pending','paid')`
//     and inspect RowsAffected. Zero rows means someone else moved the order —
//     re-read and either return the completed row idempotently or fail;
//  3. increment users.balance and write balance_logs (before/after snapshots) in
//     the same transaction.
//
// Reading the status and then writing without the conditional predicate (or
// without checking RowsAffected) is the classic double-credit bug; tests in
// recharge_records_test.go pin the single-credit behaviour against a real DB.
//
// all timestamps that reach SQL are UTC: Prisma writes UTC wall-clock into
// `timestamp(3) without time zone` and pgx encodes the wall-clock part of the
// passed time.Time, so a bare time.Now() would shift the value by the host's
// offset (see WAVE_D_SPEC §0.2b).
package db

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// rechargeStatuses mirrors RECHARGE_STATUSES.
var rechargeStatuses = map[models.RechargeStatus]bool{
	models.RechargeStatusPending:   true,
	models.RechargeStatusPaid:      true,
	models.RechargeStatusCompleted: true,
	models.RechargeStatusFailed:    true,
	models.RechargeStatusCancelled: true,
	models.RechargeStatusRefunded:  true,
}

// nanoidURLAlphabet mirrors nanoid's default urlAlphabet. The generated order
// number is uppercased, so the effective alphabet is a subset — that is fine, the
// contract is uniqueness + the R<base36-time><8 chars> shape.
const nanoidURLAlphabet = "useandom-26T198340PX75pxJACKVERYMINDBUSHWOLF_GQZbfghjklqvwyzrict"

// ==================== types ====================

// RechargeProviderRef is the {id,name,type} projection attached to a record.
type RechargeProviderRef struct {
	ID   int32                      `json:"id"`
	Name string                     `json:"name"`
	Type models.PaymentProviderType `json:"type"`
}

// RechargeRecordWithProvider mirrors RechargeRecordWithProvider.
type RechargeRecordWithProvider struct {
	models.RechargeRecord
	Provider *RechargeProviderRef `json:"provider"`
}

// CreateRechargeOrderInput mirrors CreateRechargeOrderInput.
type CreateRechargeOrderInput struct {
	OrderNo                string
	UserID                 int32
	ProviderID             int32
	Amount                 float64
	ActualAmount           *float64
	Fee                    float64
	PaymentMethod          *string
	IP                     *string
	UserAgent              *string
	ExpiredAt              *time.Time
	ProviderConfigSnapshot *string
	PaymentDetails         any
}

// UpdateRechargePaymentSelectionInput mirrors UpdateRechargePaymentSelectionInput.
type UpdateRechargePaymentSelectionInput struct {
	PaymentMethod  *string
	Fee            float64
	ActualAmount   float64
	PaymentDetails any
}

// RechargeRecordListOptions mirrors the pagination options.
type RechargeRecordListOptions struct {
	Page     int
	PageSize int
	Status   models.RechargeStatus
	UserID   int32
}

// RechargeRecordPage mirrors the paginated result shape.
type RechargeRecordPage struct {
	Records  []RechargeRecordWithProvider
	Total    int64
	Page     int
	PageSize int
}

// CompleteRechargeData mirrors the completeRecharge payload. The *Provided flags
// distinguish "leave unchanged" (Prisma undefined) from "set to null".
type CompleteRechargeData struct {
	TradeNo                *string
	TradeNoProvided        bool
	CallbackData           any
	CallbackDataProvided   bool
	ActualAmount           *float64
	PaymentDetails         any
	PaymentDetailsProvided bool
}

// CompleteRechargeResult mirrors CompleteRechargeResult.
type CompleteRechargeResult struct {
	Record       models.RechargeRecord
	CompletedNow bool
}

// RechargeMetadataUpdate mirrors updateRechargeOrderMetadata's payload.
type RechargeMetadataUpdate struct {
	TradeNo                *string
	TradeNoProvided        bool
	CallbackData           any
	CallbackDataProvided   bool
	PaymentDetails         any
	PaymentDetailsProvided bool
}

// RechargeUserStats mirrors getUserRechargeStats' return type.
type RechargeUserStats struct {
	TotalRecharge float64 `json:"totalRecharge"`
	TotalCount    int64   `json:"totalCount"`
	PendingCount  int64   `json:"pendingCount"`
}

// RechargeSystemStats mirrors getSystemRechargeStats' return type.
type RechargeSystemStats struct {
	TotalRecharge float64 `json:"totalRecharge"`
	TotalCount    int64   `json:"totalCount"`
	PendingAmount float64 `json:"pendingAmount"`
	PendingCount  int64   `json:"pendingCount"`
	TodayRecharge float64 `json:"todayRecharge"`
	TodayCount    int64   `json:"todayCount"`
}

// ==================== helpers ====================

func rechargeToMoney(value any) float64 {
	if value == nil {
		return 0
	}
	text := fmt.Sprint(value)
	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// clampRechargePagination mirrors clampPagination (max 50).
func clampRechargePagination(page, pageSize *int) (int, int) {
	normalizedPage := 1
	if page != nil && *page > 0 {
		normalizedPage = *page
	}
	normalizedPageSize := 20
	if pageSize != nil {
		normalizedPageSize = *pageSize
		if normalizedPageSize < 1 {
			normalizedPageSize = 1
		}
		if normalizedPageSize > 50 {
			normalizedPageSize = 50
		}
	}
	return normalizedPage, normalizedPageSize
}

func normalizeRechargeStatus(status models.RechargeStatus) models.RechargeStatus {
	if rechargeStatuses[status] {
		return status
	}
	return ""
}

func rechargeDecimalFromFloat(value float64) models.Decimal {
	parsed, err := models.NewDecimalFromString(strconv.FormatFloat(value, 'f', -1, 64))
	if err != nil {
		return models.Decimal{}
	}
	return parsed
}

// rechargeJSONValue converts an arbitrary value into a JSON column payload.
func rechargeJSONValue(value any) datatypes.JSON {
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case datatypes.JSON:
		return typed
	case json.RawMessage:
		return datatypes.JSON(typed)
	case []byte:
		return datatypes.JSON(typed)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return datatypes.JSON(encoded)
}

func rechargeNumberString(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// rechargeBusinessLocation mirrors the default business timezone used by
// getTodayRange() (Asia/Shanghai). db cannot import lib; checkin.go keeps its own
// copy of this lookup for the same reason.
func rechargeBusinessLocation() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("Asia/Shanghai", 8*60*60)
	}
	return location
}

// rechargeTodayRangeUTC mirrors getTodayRange(): [today 00:00, tomorrow 00:00)
// in Asia/Shanghai, returned as UTC instants.
func rechargeTodayRangeUTC(now time.Time) (time.Time, time.Time) {
	location := rechargeBusinessLocation()
	local := now.In(location)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location).UTC()
	return start, start.AddDate(0, 0, 1)
}

// rechargeProviderRef loads the {id,name,type} relation for a provider id.
func rechargeProviderRef(ctx context.Context, providerID int32) (*RechargeProviderRef, error) {
	var ref RechargeProviderRef
	err := DB.WithContext(ctx).
		Table("payment_providers").
		Select("id, name, type").
		Where("id = ?", providerID).
		Take(&ref).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

// rechargeAttachProviders bulk-loads provider refs for a page of records.
func rechargeAttachProviders(ctx context.Context, records []RechargeRecordWithProvider) error {
	missing := map[int32]bool{}
	for _, record := range records {
		if record.ProviderID != 0 {
			missing[record.ProviderID] = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	ids := make([]int32, 0, len(missing))
	for id := range missing {
		ids = append(ids, id)
	}
	var refs []RechargeProviderRef
	if err := DB.WithContext(ctx).
		Table("payment_providers").
		Select("id, name, type").
		Where("id IN ?", ids).
		Find(&refs).Error; err != nil {
		return err
	}
	byID := make(map[int32]RechargeProviderRef, len(refs))
	for _, ref := range refs {
		byID[ref.ID] = ref
	}
	for index := range records {
		if ref, ok := byID[records[index].ProviderID]; ok {
			copied := ref
			records[index].Provider = &copied
		}
	}
	return nil
}

// ==================== order number ====================

// GenerateOrderNo mirrors generateOrderNo: R + Date.now().toString(36) + nanoid(8),
// uppercased.
func GenerateOrderNo() string {
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 36)
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		// crypto/rand failing is unrecoverable; fall back to a time-derived suffix
		// so an order can still be created (uniqueness is enforced by the DB).
		suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
		for i := range buffer {
			buffer[i] = suffix[i%len(suffix)]
		}
		return strings.ToUpper("R" + timestamp + string(buffer))
	}
	random := make([]byte, 8)
	for i := range random {
		random[i] = nanoidURLAlphabet[int(buffer[i])%len(nanoidURLAlphabet)]
	}
	return strings.ToUpper("R" + timestamp + string(random))
}

// ==================== reads ====================

// GetRechargeRecordByOrderNo mirrors getRechargeRecordByOrderNo.
func GetRechargeRecordByOrderNo(ctx context.Context, orderNo string) (*RechargeRecordWithProvider, error) {
	var record models.RechargeRecord
	err := DB.WithContext(ctx).Where("order_no = ?", orderNo).First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := RechargeRecordWithProvider{RechargeRecord: record}
	ref, err := rechargeProviderRef(ctx, record.ProviderID)
	if err != nil {
		return nil, err
	}
	result.Provider = ref
	return &result, nil
}

// GetRechargeRecordByTradeNo mirrors getRechargeRecordByTradeNo.
func GetRechargeRecordByTradeNo(ctx context.Context, tradeNo string) (*models.RechargeRecord, error) {
	var record models.RechargeRecord
	err := DB.WithContext(ctx).Where("trade_no = ?", tradeNo).Order("id ASC").First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// GetRechargeRecordByID mirrors getRechargeRecordById.
func GetRechargeRecordByID(ctx context.Context, id int32) (*RechargeRecordWithProvider, error) {
	var record models.RechargeRecord
	err := DB.WithContext(ctx).First(&record, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := RechargeRecordWithProvider{RechargeRecord: record}
	ref, err := rechargeProviderRef(ctx, record.ProviderID)
	if err != nil {
		return nil, err
	}
	result.Provider = ref
	return &result, nil
}

// GetUserRechargeRecords mirrors getUserRechargeRecords.
func GetUserRechargeRecords(ctx context.Context, userID int32, options RechargeRecordListOptions) (RechargeRecordPage, error) {
	page, pageSize := clampRechargePagination(&options.Page, &options.PageSize)
	status := normalizeRechargeStatus(options.Status)

	query := DB.WithContext(ctx).Model(&models.RechargeRecord{}).Where("user_id = ?", userID)
	if status != "" {
		query = query.Where("status = ?", string(status))
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return RechargeRecordPage{}, err
	}

	var records []models.RechargeRecord
	if err := query.
		Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&records).Error; err != nil {
		return RechargeRecordPage{}, err
	}

	withProviders := make([]RechargeRecordWithProvider, 0, len(records))
	for _, record := range records {
		withProviders = append(withProviders, RechargeRecordWithProvider{RechargeRecord: record})
	}
	if err := rechargeAttachProviders(ctx, withProviders); err != nil {
		return RechargeRecordPage{}, err
	}
	return RechargeRecordPage{Records: withProviders, Total: total, Page: page, PageSize: pageSize}, nil
}

// GetAllRechargeRecords mirrors getAllRechargeRecords (the `user` relation it also
// includes is not consumed by any caller).
func GetAllRechargeRecords(ctx context.Context, options RechargeRecordListOptions) (RechargeRecordPage, error) {
	page, pageSize := clampRechargePagination(&options.Page, &options.PageSize)
	status := normalizeRechargeStatus(options.Status)

	query := DB.WithContext(ctx).Model(&models.RechargeRecord{})
	if status != "" {
		query = query.Where("status = ?", string(status))
	}
	if options.UserID > 0 {
		query = query.Where("user_id = ?", options.UserID)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return RechargeRecordPage{}, err
	}

	var records []models.RechargeRecord
	if err := query.
		Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&records).Error; err != nil {
		return RechargeRecordPage{}, err
	}

	withProviders := make([]RechargeRecordWithProvider, 0, len(records))
	for _, record := range records {
		withProviders = append(withProviders, RechargeRecordWithProvider{RechargeRecord: record})
	}
	if err := rechargeAttachProviders(ctx, withProviders); err != nil {
		return RechargeRecordPage{}, err
	}
	return RechargeRecordPage{Records: withProviders, Total: total, Page: page, PageSize: pageSize}, nil
}

// ==================== writes ====================

// CreateRechargeOrder mirrors createRechargeOrder.
func CreateRechargeOrder(ctx context.Context, input CreateRechargeOrderInput) (models.RechargeRecord, error) {
	orderNo := input.OrderNo
	if orderNo == "" {
		orderNo = GenerateOrderNo()
	}
	expiredAt := time.Now().UTC().Add(30 * time.Minute)
	if input.ExpiredAt != nil {
		expiredAt = input.ExpiredAt.UTC()
	}

	var actualAmount *models.Decimal
	if input.ActualAmount != nil {
		value := rechargeDecimalFromFloat(*input.ActualAmount)
		actualAmount = &value
	}

	record := models.RechargeRecord{
		UserID:                 input.UserID,
		ProviderID:             input.ProviderID,
		OrderNo:                orderNo,
		Amount:                 rechargeDecimalFromFloat(input.Amount),
		ActualAmount:           actualAmount,
		Fee:                    rechargeDecimalFromFloat(input.Fee),
		PaymentMethod:          input.PaymentMethod,
		Status:                 models.RechargeStatusPending,
		IP:                     input.IP,
		UserAgent:              input.UserAgent,
		ExpiredAt:              &expiredAt,
		ProviderConfigSnapshot: input.ProviderConfigSnapshot,
		PaymentDetails:         rechargeJSONValue(input.PaymentDetails),
	}
	if err := DB.WithContext(ctx).Create(&record).Error; err != nil {
		return models.RechargeRecord{}, err
	}
	return record, nil
}

// UpdateRechargeOrderMetadata mirrors updateRechargeOrderMetadata. The update is
// restricted to pending/paid orders, but the record is returned even when nothing
// matched (the TS re-reads unconditionally and only throws when the row is gone).
func UpdateRechargeOrderMetadata(ctx context.Context, orderNo string, data RechargeMetadataUpdate) (models.RechargeRecord, error) {
	updates := map[string]any{"updated_at": time.Now().UTC()}
	if data.TradeNoProvided {
		if data.TradeNo == nil {
			updates["trade_no"] = nil
		} else {
			updates["trade_no"] = *data.TradeNo
		}
	}
	if data.CallbackDataProvided {
		updates["callback_data"] = rechargeJSONValue(data.CallbackData)
		updates["callback_at"] = time.Now().UTC()
	}
	if data.PaymentDetailsProvided {
		updates["payment_details"] = rechargeJSONValue(data.PaymentDetails)
	}

	if err := DB.WithContext(ctx).
		Model(&models.RechargeRecord{}).
		Where("order_no = ? AND status IN ?", orderNo, []string{
			string(models.RechargeStatusPending), string(models.RechargeStatusPaid),
		}).
		Updates(updates).Error; err != nil {
		return models.RechargeRecord{}, err
	}

	var record models.RechargeRecord
	err := DB.WithContext(ctx).Where("order_no = ?", orderNo).First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.RechargeRecord{}, errors.New("订单不存在")
	}
	if err != nil {
		return models.RechargeRecord{}, err
	}
	return record, nil
}

// CompleteRecharge mirrors completeRecharge.
func CompleteRecharge(ctx context.Context, orderNo string, data CompleteRechargeData) (CompleteRechargeResult, error) {
	var record models.RechargeRecord
	err := DB.WithContext(ctx).Where("order_no = ?", orderNo).First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return CompleteRechargeResult{}, errors.New("订单不存在")
	}
	if err != nil {
		return CompleteRechargeResult{}, err
	}

	if record.Status == models.RechargeStatusCompleted {
		return CompleteRechargeResult{Record: record, CompletedNow: false}, nil
	}
	if record.Status == models.RechargeStatusCancelled || record.Status == models.RechargeStatusFailed {
		return CompleteRechargeResult{}, fmt.Errorf("订单状态异常：%s", record.Status)
	}

	creditAmount := 0.0
	switch {
	case data.ActualAmount != nil:
		creditAmount = *data.ActualAmount
	case record.ActualAmount != nil:
		creditAmount = record.ActualAmount.InexactFloat64()
	default:
		creditAmount = record.Amount.InexactFloat64()
	}
	if math.IsNaN(creditAmount) || math.IsInf(creditAmount, 0) || creditAmount <= 0 {
		return CompleteRechargeResult{}, errors.New("充值入账金额无效")
	}
	if creditAmount > 9007199254740991 {
		return CompleteRechargeResult{}, errors.New("充值入账金额无效")
	}

	var result CompleteRechargeResult
	now := time.Now().UTC()

	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Serialise every balance mutation of this user on the same transaction.
		if err := AdvisoryTransactionLockTx(tx, UserBalanceLockNamespace, record.UserID); err != nil {
			return err
		}

		updates := map[string]any{
			"status":        string(models.RechargeStatusCompleted),
			"completed_at":  now,
			"actual_amount": rechargeDecimalFromFloat(creditAmount),
			"updated_at":    now,
		}
		if data.TradeNoProvided {
			if data.TradeNo == nil {
				updates["trade_no"] = nil
			} else {
				updates["trade_no"] = *data.TradeNo
			}
		}
		if data.CallbackDataProvided {
			updates["callback_data"] = rechargeJSONValue(data.CallbackData)
			updates["callback_at"] = now
		}
		if data.PaymentDetailsProvided {
			updates["payment_details"] = rechargeJSONValue(data.PaymentDetails)
		} else {
			updates["payment_details"] = record.PaymentDetails
		}

		// Conditional state transition: only pending/paid may become completed.
		update := tx.Model(&models.RechargeRecord{}).
			Where("order_no = ? AND status IN ?", orderNo, []string{
				string(models.RechargeStatusPending), string(models.RechargeStatusPaid),
			}).
			Updates(updates)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected == 0 {
			var current models.RechargeRecord
			if err := tx.Where("order_no = ?", orderNo).First(&current).Error; err != nil {
				return err
			}
			if current.Status == models.RechargeStatusCompleted {
				result = CompleteRechargeResult{Record: current, CompletedNow: false}
				return nil
			}
			return errors.New("订单状态已变更，无法完成")
		}

		var updated models.RechargeRecord
		if err := tx.Where("order_no = ?", orderNo).First(&updated).Error; err != nil {
			return errors.New("订单更新异常")
		}

		creditDecimal := rechargeDecimalFromFloat(creditAmount)
		var balanceRow struct {
			Balance models.Decimal
		}
		if err := tx.Raw(
			"UPDATE users SET balance = balance + ?, updated_at = ? WHERE id = ? RETURNING balance",
			creditDecimal, now, record.UserID,
		).Scan(&balanceRow).Error; err != nil {
			return err
		}

		balanceAfter := balanceRow.Balance
		balanceBefore := models.Decimal{Decimal: balanceAfter.Decimal.Sub(creditDecimal.Decimal).Round(2)}
		remark := "充值：" + rechargeNumberString(record.Amount.InexactFloat64()) + " 元"

		if err := tx.Exec(
			`INSERT INTO balance_logs (user_id, type, amount, balance_before, balance_after, order_id, remark, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			record.UserID, string(models.BalanceLogTypeRecharge), creditDecimal, balanceBefore, balanceAfter,
			record.OrderNo, remark, now,
		).Error; err != nil {
			return err
		}

		result = CompleteRechargeResult{Record: updated, CompletedNow: true}
		return nil
	})
	if err != nil {
		return CompleteRechargeResult{}, err
	}
	return result, nil
}

// FailRecharge mirrors failRecharge.
func FailRecharge(
	ctx context.Context,
	orderNo, failReason string,
	callbackData any,
	callbackDataProvided bool,
	paymentDetails any,
	paymentDetailsProvided bool,
) (models.RechargeRecord, error) {
	updates := map[string]any{
		"status":      string(models.RechargeStatusFailed),
		"fail_reason": failReason,
		"updated_at":  time.Now().UTC(),
	}
	if callbackDataProvided {
		updates["callback_data"] = rechargeJSONValue(callbackData)
		updates["callback_at"] = time.Now().UTC()
	}
	if paymentDetailsProvided {
		updates["payment_details"] = rechargeJSONValue(paymentDetails)
	}

	if err := DB.WithContext(ctx).
		Model(&models.RechargeRecord{}).
		Where("order_no = ? AND status IN ?", orderNo, []string{
			string(models.RechargeStatusPending), string(models.RechargeStatusPaid),
		}).
		Updates(updates).Error; err != nil {
		return models.RechargeRecord{}, err
	}

	var record models.RechargeRecord
	err := DB.WithContext(ctx).Where("order_no = ?", orderNo).First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.RechargeRecord{}, errors.New("订单不存在")
	}
	if err != nil {
		return models.RechargeRecord{}, err
	}
	return record, nil
}

// CancelRecharge mirrors cancelRecharge.
func CancelRecharge(
	ctx context.Context,
	orderNo string,
	callbackData any,
	callbackDataProvided bool,
	paymentDetails any,
	paymentDetailsProvided bool,
) (models.RechargeRecord, error) {
	updates := map[string]any{
		"status":     string(models.RechargeStatusCancelled),
		"updated_at": time.Now().UTC(),
	}
	if callbackDataProvided {
		updates["callback_data"] = rechargeJSONValue(callbackData)
		updates["callback_at"] = time.Now().UTC()
	}
	if paymentDetailsProvided {
		updates["payment_details"] = rechargeJSONValue(paymentDetails)
	}

	if err := DB.WithContext(ctx).
		Model(&models.RechargeRecord{}).
		Where("order_no = ? AND status IN ?", orderNo, []string{
			string(models.RechargeStatusPending), string(models.RechargeStatusPaid),
		}).
		Updates(updates).Error; err != nil {
		return models.RechargeRecord{}, err
	}

	var record models.RechargeRecord
	err := DB.WithContext(ctx).Where("order_no = ?", orderNo).First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.RechargeRecord{}, errors.New("订单不存在")
	}
	if err != nil {
		return models.RechargeRecord{}, err
	}
	return record, nil
}

// UpdatePendingRechargePaymentSelection mirrors updatePendingRechargePaymentSelection.
func UpdatePendingRechargePaymentSelection(
	ctx context.Context,
	orderNo string,
	data UpdateRechargePaymentSelectionInput,
) (*models.RechargeRecord, error) {
	updates := map[string]any{
		"payment_method":  nil,
		"fee":             rechargeDecimalFromFloat(data.Fee),
		"actual_amount":   rechargeDecimalFromFloat(data.ActualAmount),
		"payment_details": rechargeJSONValue(data.PaymentDetails),
		"updated_at":      time.Now().UTC(),
	}
	if data.PaymentMethod != nil && *data.PaymentMethod != "" {
		updates["payment_method"] = *data.PaymentMethod
	}

	result := DB.WithContext(ctx).
		Model(&models.RechargeRecord{}).
		Where("order_no = ? AND status = ?", orderNo, string(models.RechargeStatusPending)).
		Updates(updates)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}

	var record models.RechargeRecord
	if err := DB.WithContext(ctx).Where("order_no = ?", orderNo).First(&record).Error; err != nil {
		return nil, err
	}
	return &record, nil
}

// GetExpiredPendingOrders mirrors getExpiredPendingOrders.
func GetExpiredPendingOrders(ctx context.Context) ([]models.RechargeRecord, error) {
	var records []models.RechargeRecord
	if err := DB.WithContext(ctx).
		Where("status = ? AND expired_at < ?", string(models.RechargeStatusPending), time.Now().UTC()).
		Find(&records).Error; err != nil {
		return nil, err
	}
	return records, nil
}

// ExpireOrders mirrors expireOrders.
func ExpireOrders(ctx context.Context, orderNos []string) (int64, error) {
	if len(orderNos) == 0 {
		return 0, nil
	}
	result := DB.WithContext(ctx).
		Model(&models.RechargeRecord{}).
		Where("order_no IN ? AND status = ?", orderNos, string(models.RechargeStatusPending)).
		Updates(map[string]any{
			"status":     string(models.RechargeStatusCancelled),
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// ==================== statistics ====================

// GetUserRechargeStats mirrors getUserRechargeStats.
func GetUserRechargeStats(ctx context.Context, userID int32) (RechargeUserStats, error) {
	var totalRow struct {
		Value models.Decimal
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(COALESCE(actual_amount, amount)), 0)::numeric AS value
		 FROM recharge_records
		 WHERE user_id = ? AND status = 'completed'`,
		userID,
	).Scan(&totalRow).Error; err != nil {
		return RechargeUserStats{}, err
	}

	var totalCount, pendingCount int64
	if err := DB.WithContext(ctx).Model(&models.RechargeRecord{}).
		Where("user_id = ? AND status = ?", userID, string(models.RechargeStatusCompleted)).
		Count(&totalCount).Error; err != nil {
		return RechargeUserStats{}, err
	}
	if err := DB.WithContext(ctx).Model(&models.RechargeRecord{}).
		Where("user_id = ? AND status = ?", userID, string(models.RechargeStatusPending)).
		Count(&pendingCount).Error; err != nil {
		return RechargeUserStats{}, err
	}

	return RechargeUserStats{
		TotalRecharge: totalRow.Value.InexactFloat64(),
		TotalCount:    totalCount,
		PendingCount:  pendingCount,
	}, nil
}

// GetSystemRechargeStats mirrors getSystemRechargeStats.
func GetSystemRechargeStats(ctx context.Context, start, end *time.Time) (RechargeSystemStats, error) {
	today, tomorrow := rechargeTodayRangeUTC(time.Now())

	var totalRow struct {
		Value models.Decimal
	}
	if start != nil && end != nil {
		if err := DB.WithContext(ctx).Raw(
			`SELECT COALESCE(SUM(COALESCE(actual_amount, amount)), 0)::numeric AS value
			 FROM recharge_records
			 WHERE status = 'completed' AND completed_at >= ? AND completed_at <= ?`,
			start.UTC(), end.UTC(),
		).Scan(&totalRow).Error; err != nil {
			return RechargeSystemStats{}, err
		}
	} else {
		if err := DB.WithContext(ctx).Raw(
			`SELECT COALESCE(SUM(COALESCE(actual_amount, amount)), 0)::numeric AS value
			 FROM recharge_records WHERE status = 'completed'`,
		).Scan(&totalRow).Error; err != nil {
			return RechargeSystemStats{}, err
		}
	}

	completedQuery := DB.WithContext(ctx).Model(&models.RechargeRecord{}).
		Where("status = ?", string(models.RechargeStatusCompleted))
	if start != nil && end != nil {
		completedQuery = completedQuery.Where("completed_at >= ? AND completed_at <= ?", start.UTC(), end.UTC())
	}
	var totalCount int64
	if err := completedQuery.Count(&totalCount).Error; err != nil {
		return RechargeSystemStats{}, err
	}

	var pendingRow struct {
		Total models.Decimal
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(amount), 0)::numeric AS total FROM recharge_records WHERE status = 'pending'`,
	).Scan(&pendingRow).Error; err != nil {
		return RechargeSystemStats{}, err
	}
	var pendingCount int64
	if err := DB.WithContext(ctx).Model(&models.RechargeRecord{}).
		Where("status = ?", string(models.RechargeStatusPending)).
		Count(&pendingCount).Error; err != nil {
		return RechargeSystemStats{}, err
	}

	var todayRow struct {
		Value models.Decimal
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(COALESCE(actual_amount, amount)), 0)::numeric AS value
		 FROM recharge_records
		 WHERE status = 'completed' AND completed_at >= ? AND completed_at < ?`,
		today, tomorrow,
	).Scan(&todayRow).Error; err != nil {
		return RechargeSystemStats{}, err
	}
	var todayCount int64
	if err := DB.WithContext(ctx).Model(&models.RechargeRecord{}).
		Where("status = ? AND completed_at >= ? AND completed_at < ?",
			string(models.RechargeStatusCompleted), today, tomorrow).
		Count(&todayCount).Error; err != nil {
		return RechargeSystemStats{}, err
	}

	return RechargeSystemStats{
		TotalRecharge: totalRow.Value.InexactFloat64(),
		TotalCount:    totalCount,
		PendingAmount: pendingRow.Total.InexactFloat64(),
		PendingCount:  pendingCount,
		TodayRecharge: todayRow.Value.InexactFloat64(),
		TodayCount:    todayCount,
	}, nil
}

// ==================== payment callback replay guard ====================

// IsPaymentCallbackProcessed mirrors the prisma.paymentCallback.findUnique check
// used to make callbacks idempotent across process restarts. A database error
// reads as "not processed" (the TS catches and returns false).
func IsPaymentCallbackProcessed(ctx context.Context, providerID int32, orderNo, tradeNo string) bool {
	var count int64
	err := DB.WithContext(ctx).Table("payment_callbacks").
		Where("provider_id = ? AND order_no = ? AND trade_no = ?", providerID, orderNo, tradeNo).
		Count(&count).Error
	if err != nil {
		return false
	}
	return count > 0
}

// MarkPaymentCallbackProcessed mirrors markCallbackProcessed: a unique-index
// conflict (concurrent duplicate) is ignored.
func MarkPaymentCallbackProcessed(ctx context.Context, providerID int32, orderNo string, tradeNo *string, ip *string) {
	var tradeNoValue any
	if tradeNo != nil {
		tradeNoValue = *tradeNo
	}
	var ipValue any
	if ip != nil {
		ipValue = *ip
	}
	_ = DB.WithContext(ctx).Exec(
		`INSERT INTO payment_callbacks (provider_id, order_no, trade_no, callback_ip, processed, created_at)
		 VALUES (?, ?, ?, ?, true, ?)
		 ON CONFLICT DO NOTHING`,
		providerID, orderNo, tradeNoValue, ipValue, time.Now().UTC(),
	).Error
}
