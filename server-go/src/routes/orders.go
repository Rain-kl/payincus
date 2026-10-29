// Port of server/src/routes/orders.ts.
//
// app.ts registers this module as `fastify.register(orderRoutes)` with **no
// prefix**, so every path below is the absolute route (`/api/orders`,
// `/api/admin/orders`, ...) exactly as written in the TypeScript.
//
// The order centre merges two independent tables (`recharge_records` and
// `instance_billing_records`) into one paginated feed, then attaches the
// `order_operation_cases` row (and its refund approval chain) for the page.
//
// Money is read from PostgreSQL `numeric` columns through models.Decimal and
// rendered as JSON **numbers**: the TypeScript calls toMoney()/Number() on every
// amount before returning it, so a Prisma-decimal string here would be wrong.
package routes

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== constants ====================

const (
	ordersMaxPageSize        = 100
	ordersSearchMaxLength    = 128
	ordersOperationTextMax   = 500
	ordersKeywordUserTake    = 100
	ordersRechargeTitle      = "余额充值"
	ordersDefaultBillingName = "实例账单"
	// ordersMaxRefundAmount mirrors MAX_REFUND_AMOUNT.
	ordersMaxRefundAmount = 99999999.99
)

var (
	// ordersRechargeStatuses mirrors RECHARGE_STATUSES.
	ordersRechargeStatuses = map[string]bool{
		"pending": true, "paid": true, "completed": true,
		"failed": true, "cancelled": true, "refunded": true,
	}
	// ordersBillingTypes mirrors BILLING_TYPES.
	ordersBillingTypes = map[string]bool{
		"newPurchase": true, "renew": true, "upgrade": true,
		"downgrade": true, "refund": true, "transfer_fee": true,
	}
	// ordersOperationStatuses mirrors ORDER_OPERATION_STATUSES.
	ordersOperationStatuses = map[string]bool{
		string(models.OrderOperationStatusPendingReview): true,
		string(models.OrderOperationStatusConfirmed):     true,
		string(models.OrderOperationStatusCompensated):   true,
		string(models.OrderOperationStatusClosed):        true,
	}
	// ordersBillingTitles mirrors titleByType in buildBillingOrder.
	ordersBillingTitles = map[string]string{
		"newPurchase":  "实例新购",
		"renew":        "实例续费",
		"upgrade":      "实例升级",
		"downgrade":    "实例降级",
		"refund":       "实例退款",
		"transfer_fee": "转移手续费",
	}

	ordersBillKeywordPattern = regexp.MustCompile(`(?i)^BILL-(\d+)$`)
	ordersDigitsPattern      = regexp.MustCompile(`^\d+$`)
	// ordersRefundAmountPattern mirrors /^\d+(\.\d{1,2})?$/.
	ordersRefundAmountPattern = regexp.MustCompile(`^\d+(\.\d{1,2})?$`)
)

// ==================== wire types ====================

// orderUserRef is the `{ id, username, email }` user projection.
type orderUserRef struct {
	ID       int32   `json:"id"`
	Username string  `json:"username"`
	Email    *string `json:"email"`
}

// orderProviderRef is the `{ id, name, type }` payment provider projection.
type orderProviderRef struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// orderProviderStatusSummary mirrors buildProviderStatusSummary.
type orderProviderStatusSummary struct {
	Provider      *orderProviderRef `json:"provider"`
	RawStatus     string            `json:"rawStatus"`
	PaymentMethod *string           `json:"paymentMethod"`
	TradeNo       *string           `json:"tradeNo"`
	CallbackAt    *string           `json:"callbackAt"`
}

// orderPackageRef is the outer package of a plan.
type orderPackageRef struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

// orderPackagePlanRef is the plan of an instance.
type orderPackagePlanRef struct {
	ID      int32            `json:"id"`
	Name    string           `json:"name"`
	Package *orderPackageRef `json:"package"`
}

// orderInstanceRef is the instance projection embedded in billing orders.
type orderInstanceRef struct {
	ID          int32                `json:"id"`
	Name        string               `json:"name"`
	PackagePlan *orderPackagePlanRef `json:"packagePlan"`
}

// orderView is one row of the merged order feed.
type orderView struct {
	ID                    string                      `json:"id"`
	SourceType            string                      `json:"sourceType"`
	SourceID              int32                       `json:"sourceId"`
	OrderNo               string                      `json:"orderNo"`
	Title                 string                      `json:"title"`
	Status                string                      `json:"status"`
	RawStatus             string                      `json:"rawStatus"`
	Amount                float64                     `json:"amount"`
	ActualAmount          *float64                    `json:"actualAmount"`
	Fee                   float64                     `json:"fee"`
	UserID                int32                       `json:"userId"`
	User                  *orderUserRef               `json:"user"`
	Provider              *orderProviderRef           `json:"provider"`
	ProviderStatusSummary *orderProviderStatusSummary `json:"providerStatusSummary"`
	PaymentMethod         *string                     `json:"paymentMethod"`
	TradeNo               *string                     `json:"tradeNo"`
	FailReason            *string                     `json:"failReason"`
	Instance              *orderInstanceRef           `json:"instance"`
	Months                *int32                      `json:"months"`
	PeriodStart           *string                     `json:"periodStart"`
	PeriodEnd             *string                     `json:"periodEnd"`
	Remark                *string                     `json:"remark"`
	CreatedAt             string                      `json:"createdAt"`
	CompletedAt           *string                     `json:"completedAt"`
	ExpiredAt             *string                     `json:"expiredAt"`

	// createdAtTime keeps the sort key alongside the rendered string.
	createdAtTime time.Time
}

// orderWithOperationCase appends the `operationCase` key (present, possibly null).
type orderWithOperationCase struct {
	orderView
	OperationCase *orderOperationCaseView `json:"operationCase"`
}

// ordersListResult mirrors listOrders' return shape.
type ordersListResult struct {
	Orders   []orderWithOperationCase `json:"orders"`
	Total    int64                    `json:"total"`
	Page     int                      `json:"page"`
	PageSize int                      `json:"pageSize"`
}

// orderOperationUserRef is the `{ id, username }` projection.
type orderOperationUserRef struct {
	ID       int32  `json:"id"`
	Username string `json:"username"`
}

// orderBalanceLogView mirrors the balanceLog sub-object of the orders serializer.
type orderBalanceLogView struct {
	ID            int32   `json:"id"`
	Type          string  `json:"type"`
	Amount        float64 `json:"amount"`
	BalanceBefore float64 `json:"balanceBefore"`
	BalanceAfter  float64 `json:"balanceAfter"`
}

// orderBalanceAdjustmentView mirrors serializeBalanceAdjustmentRequest in orders.ts
// (note: amounts are numbers here, unlike the balance route's serializer).
type orderBalanceAdjustmentView struct {
	ID           int32                  `json:"id"`
	UserID       int32                  `json:"userId"`
	User         *orderUserRef          `json:"user"`
	RequestedBy  *orderOperationUserRef `json:"requestedBy"`
	ReviewedBy   *orderOperationUserRef `json:"reviewedBy"`
	Amount       float64                `json:"amount"`
	RequestType  string                 `json:"requestType"`
	Status       string                 `json:"status"`
	SourceType   *string                `json:"sourceType"`
	SourceID     *int32                 `json:"sourceId"`
	OrderNo      *string                `json:"orderNo"`
	Reason       string                 `json:"reason"`
	ReviewRemark *string                `json:"reviewRemark"`
	BalanceLogID *int32                 `json:"balanceLogId"`
	BalanceLog   *orderBalanceLogView   `json:"balanceLog"`
	CreatedAt    string                 `json:"createdAt"`
	UpdatedAt    string                 `json:"updatedAt"`
	ReviewedAt   *string                `json:"reviewedAt"`
}

// orderOperationCaseView mirrors serializeOrderOperationCase.
type orderOperationCaseView struct {
	ID                         int32                       `json:"id"`
	SourceType                 string                      `json:"sourceType"`
	SourceID                   int32                       `json:"sourceId"`
	OrderNo                    *string                     `json:"orderNo"`
	UserID                     int32                       `json:"userId"`
	Status                     string                      `json:"status"`
	Reason                     string                      `json:"reason"`
	Result                     *string                     `json:"result"`
	RefundAmount               *float64                    `json:"refundAmount"`
	ProviderSummary            any                         `json:"providerSummary"`
	CreatedBy                  *orderOperationUserRef      `json:"createdBy"`
	UpdatedBy                  *orderOperationUserRef      `json:"updatedBy"`
	BalanceAdjustmentRequestID *int32                      `json:"balanceAdjustmentRequestId"`
	BalanceAdjustmentRequest   *orderBalanceAdjustmentView `json:"balanceAdjustmentRequest"`
	CreatedAt                  string                      `json:"createdAt"`
	UpdatedAt                  string                      `json:"updatedAt"`
}

// ==================== parsing helpers ====================

// ordersParsePositiveID mirrors parsePositiveId for string input: canonical
// positive decimal within the JS safe-integer range, else not-ok. The value is
// kept as int64 because an id beyond the int32 column range is syntactically
// valid in the TypeScript (it simply matches no row → 404) and must not be
// silently truncated into some other order's id.
func ordersParsePositiveID(value string) (int64, bool) {
	return lib.ParsePositiveRouteID(value)
}

// ordersNarrowOrderID maps a parsed JS-safe id onto the int32 column domain,
// reporting ok=false when the id cannot exist (so the caller answers 404).
func ordersNarrowOrderID(parsed int64) (int32, bool) {
	if parsed > math.MaxInt32 {
		return 0, false
	}
	return int32(parsed), true
}

// ordersParsePositiveQuery mirrors parsePositiveQuery: invalid values fall back.
func ordersParsePositiveQuery(value string, fallback int) int {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok {
		return fallback
	}
	return int(parsed)
}

// ordersParsePageSize mirrors parsePageSize: clamped to MAX_PAGE_SIZE.
func ordersParsePageSize(value string) int {
	pageSize := ordersParsePositiveQuery(value, 20)
	if pageSize > ordersMaxPageSize {
		return ordersMaxPageSize
	}
	return pageSize
}

// ordersNormalizeOrderType mirrors normalizeOrderType.
func ordersNormalizeOrderType(value string) string {
	if value == "recharge" || value == "instance_billing" {
		return value
	}
	return ""
}

// ordersNormalizeRechargeStatus mirrors normalizeRechargeStatus.
func ordersNormalizeRechargeStatus(value string) string {
	if value != "" && ordersRechargeStatuses[value] {
		return value
	}
	return ""
}

// ordersNormalizeBillingType mirrors normalizeBillingType.
func ordersNormalizeBillingType(value string) string {
	if value != "" && ordersBillingTypes[value] {
		return value
	}
	return ""
}

// ordersNormalizeOperationStatus mirrors normalizeOrderOperationStatus (defaults
// to pending_review).
func ordersNormalizeOperationStatus(value any) string {
	if text, ok := value.(string); ok && ordersOperationStatuses[text] {
		return text
	}
	return string(models.OrderOperationStatusPendingReview)
}

// ordersNormalizeSearch mirrors normalizeSearch: trim then slice(0, 128) over
// UTF-16 code units.
func ordersNormalizeSearch(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	trimmed = jsStringSlice(trimmed, 0, ordersSearchMaxLength)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// ordersParseDateQuery mirrors parseDateQuery: an unparseable value is ignored
// (not an error).
func ordersParseDateQuery(value string) *time.Time {
	if value == "" {
		return nil
	}
	parsed, ok := parseAPITokenJSDatetime(value)
	if !ok {
		return nil
	}
	return &parsed
}

// ==================== money helpers ====================

// ordersDerefString mirrors the `x ?? ”` fallbacks of the projection builders.
func ordersDerefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// ordersDecimalFromText parses a `numeric` column rendered as text.
func ordersDecimalFromText(raw string) models.Decimal {
	parsed, err := models.NewDecimalFromString(raw)
	if err != nil {
		return models.Decimal{Decimal: decimal.Zero}
	}
	return parsed
}

// ordersFormatJSNumber mirrors JavaScript's String(number) for the range these
// routes accept (0 < n <= MAX_REFUND_AMOUNT, far below 1e21) so the
// /^\d+(\.\d{1,2})?$/ pattern test sees the same text V8 produces.
func ordersFormatJSNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// ordersToMoney mirrors toMoney(): a Decimal becomes a JSON number.
func ordersToMoney(value models.Decimal) float64 {
	result, _ := value.Float64()
	return result
}

// ordersOptionalMoney mirrors `record.x === null ? null : toMoney(record.x)`.
func ordersOptionalMoney(raw *string) *float64 {
	if raw == nil {
		return nil
	}
	value := ordersToMoney(ordersDecimalFromText(*raw))
	return &value
}

// ordersMaskTradeNo mirrors maskTradeNo.
func ordersMaskTradeNo(value *string) *string {
	if value == nil || *value == "" {
		return nil
	}
	text := *value
	var masked string
	if len(text) <= 8 {
		if len(text) <= 2 {
			masked = text + "***"
		} else {
			masked = text[:2] + "***"
		}
		return &masked
	}
	masked = text[:4] + "***" + text[len(text)-4:]
	return &masked
}

// ordersISOTime mirrors Date#toISOString.
func ordersISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// ordersOptionalISOTime mirrors `value?.toISOString() ?? null`.
func ordersOptionalISOTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	rendered := ordersISOTime(*value)
	return &rendered
}

// ==================== row types ====================

type ordersRechargeRow struct {
	ID            int32      `gorm:"column:id"`
	UserID        int32      `gorm:"column:user_id"`
	OrderNo       string     `gorm:"column:order_no"`
	TradeNo       *string    `gorm:"column:trade_no"`
	Amount        string     `gorm:"column:amount_text"`
	ActualAmount  *string    `gorm:"column:actual_amount_text"`
	Fee           string     `gorm:"column:fee_text"`
	PaymentMethod *string    `gorm:"column:payment_method"`
	Status        string     `gorm:"column:status"`
	FailReason    *string    `gorm:"column:fail_reason"`
	CallbackAt    *time.Time `gorm:"column:callback_at"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	CompletedAt   *time.Time `gorm:"column:completed_at"`
	ExpiredAt     *time.Time `gorm:"column:expired_at"`
	ProviderID    *int32     `gorm:"column:provider_ref_id"`
	ProviderName  *string    `gorm:"column:provider_name"`
	ProviderType  *string    `gorm:"column:provider_type"`
	UserRefID     *int32     `gorm:"column:user_ref_id"`
	UserRefName   *string    `gorm:"column:user_ref_name"`
	UserRefEmail  *string    `gorm:"column:user_ref_email"`
}

type ordersBillingRow struct {
	ID           int32     `gorm:"column:id"`
	InstanceID   int32     `gorm:"column:instance_id"`
	UserID       int32     `gorm:"column:user_id"`
	Type         string    `gorm:"column:type"`
	Amount       string    `gorm:"column:amount_text"`
	Months       int32     `gorm:"column:months"`
	PeriodStart  time.Time `gorm:"column:period_start"`
	PeriodEnd    time.Time `gorm:"column:period_end"`
	Remark       *string   `gorm:"column:remark"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	InstanceRef  *int32    `gorm:"column:instance_ref_id"`
	InstanceName *string   `gorm:"column:instance_ref_name"`
	PlanID       *int32    `gorm:"column:plan_id"`
	PlanName     *string   `gorm:"column:plan_name"`
	PackageID    *int32    `gorm:"column:package_id"`
	PackageName  *string   `gorm:"column:package_name"`
}

// ==================== builders ====================

// ordersProviderStatus builds the nested provider summary (nil provider is kept).
func ordersProviderRef(row ordersRechargeRow) *orderProviderRef {
	if row.ProviderID == nil {
		return nil
	}
	providerType := ""
	if row.ProviderType != nil {
		providerType = *row.ProviderType
	}
	return &orderProviderRef{ID: *row.ProviderID, Name: ordersDerefString(row.ProviderName), Type: providerType}
}

// ordersBuildProviderStatusSummary mirrors buildProviderStatusSummary.
func ordersBuildProviderStatusSummary(row ordersRechargeRow) *orderProviderStatusSummary {
	return &orderProviderStatusSummary{
		Provider:      ordersProviderRef(row),
		RawStatus:     row.Status,
		PaymentMethod: row.PaymentMethod,
		TradeNo:       ordersMaskTradeNo(row.TradeNo),
		CallbackAt:    ordersOptionalISOTime(row.CallbackAt),
	}
}

// ordersMapRechargeStatus mirrors mapRechargeStatus.
func ordersMapRechargeStatus(status string) string {
	switch status {
	case "completed":
		return "completed"
	case "refunded":
		return "refunded"
	case "failed":
		return "failed"
	case "cancelled":
		return "cancelled"
	}
	return "pending"
}

// ordersMapBillingStatus mirrors mapBillingStatus.
func ordersMapBillingStatus(billingType string) string {
	if billingType == "refund" {
		return "refunded"
	}
	return "completed"
}

// ordersBuildRechargeOrder mirrors buildRechargeOrder.
func ordersBuildRechargeOrder(row ordersRechargeRow) orderView {
	var user *orderUserRef
	if row.UserRefID != nil {
		user = &orderUserRef{ID: *row.UserRefID, Username: ordersDerefString(row.UserRefName), Email: row.UserRefEmail}
	}

	return orderView{
		ID:                    "recharge:" + strconv.FormatInt(int64(row.ID), 10),
		SourceType:            "recharge",
		SourceID:              row.ID,
		OrderNo:               row.OrderNo,
		Title:                 ordersRechargeTitle,
		Status:                ordersMapRechargeStatus(row.Status),
		RawStatus:             row.Status,
		Amount:                ordersToMoney(ordersDecimalFromText(row.Amount)),
		ActualAmount:          ordersOptionalMoney(row.ActualAmount),
		Fee:                   ordersToMoney(ordersDecimalFromText(row.Fee)),
		UserID:                row.UserID,
		User:                  user,
		Provider:              ordersProviderRef(row),
		ProviderStatusSummary: ordersBuildProviderStatusSummary(row),
		PaymentMethod:         row.PaymentMethod,
		TradeNo:               row.TradeNo,
		FailReason:            row.FailReason,
		Instance:              nil,
		Months:                nil,
		PeriodStart:           nil,
		PeriodEnd:             nil,
		Remark:                nil,
		CreatedAt:             ordersISOTime(row.CreatedAt),
		CompletedAt:           ordersOptionalISOTime(row.CompletedAt),
		ExpiredAt:             ordersOptionalISOTime(row.ExpiredAt),
		createdAtTime:         row.CreatedAt,
	}
}

// ordersInstanceRefFromRow rebuilds the nested instance projection.
func ordersInstanceRefFromRow(row ordersBillingRow) *orderInstanceRef {
	if row.InstanceRef == nil {
		return nil
	}
	instance := &orderInstanceRef{ID: *row.InstanceRef, Name: ordersDerefString(row.InstanceName)}
	if row.PlanID != nil {
		plan := &orderPackagePlanRef{ID: *row.PlanID, Name: ordersDerefString(row.PlanName)}
		if row.PackageID != nil {
			plan.Package = &orderPackageRef{ID: *row.PackageID, Name: ordersDerefString(row.PackageName)}
		}
		instance.PackagePlan = plan
	}
	return instance
}

// ordersBuildBillingOrder mirrors buildBillingOrder.
func ordersBuildBillingOrder(row ordersBillingRow, user *orderUserRef) orderView {
	title, ok := ordersBillingTitles[row.Type]
	if !ok {
		title = ordersDefaultBillingName
	}
	amount := ordersToMoney(ordersDecimalFromText(row.Amount))
	amountCopy := amount
	months := row.Months
	periodStart := ordersISOTime(row.PeriodStart)
	periodEnd := ordersISOTime(row.PeriodEnd)
	createdAt := ordersISOTime(row.CreatedAt)
	completedAt := createdAt
	paymentMethod := "balance"

	return orderView{
		ID:                    "instance_billing:" + strconv.FormatInt(int64(row.ID), 10),
		SourceType:            "instance_billing",
		SourceID:              row.ID,
		OrderNo:               "BILL-" + strconv.FormatInt(int64(row.ID), 10),
		Title:                 title,
		Status:                ordersMapBillingStatus(row.Type),
		RawStatus:             row.Type,
		Amount:                amount,
		ActualAmount:          &amountCopy,
		Fee:                   0,
		UserID:                row.UserID,
		User:                  user,
		Provider:              nil,
		ProviderStatusSummary: nil,
		PaymentMethod:         &paymentMethod,
		TradeNo:               nil,
		FailReason:            nil,
		Instance:              ordersInstanceRefFromRow(row),
		Months:                &months,
		PeriodStart:           &periodStart,
		PeriodEnd:             &periodEnd,
		Remark:                row.Remark,
		CreatedAt:             createdAt,
		CompletedAt:           &completedAt,
		ExpiredAt:             nil,
		createdAtTime:         row.CreatedAt,
	}
}

// ==================== operation-case loading ====================

// ordersCasePair identifies one order-operation case.
type ordersCasePair struct {
	SourceType string
	SourceID   int32
}

func ordersCaseKey(sourceType string, sourceID int32) string {
	return sourceType + ":" + strconv.FormatInt(int64(sourceID), 10)
}

type ordersOperationCaseRow struct {
	ID                         int32     `gorm:"column:id"`
	SourceType                 string    `gorm:"column:source_type"`
	SourceID                   int32     `gorm:"column:source_id"`
	OrderNo                    *string   `gorm:"column:order_no"`
	UserID                     int32     `gorm:"column:user_id"`
	CreatedByUserID            int32     `gorm:"column:created_by_user_id"`
	UpdatedByUserID            *int32    `gorm:"column:updated_by_user_id"`
	BalanceAdjustmentRequestID *int32    `gorm:"column:balance_adjustment_request_id"`
	Status                     string    `gorm:"column:status"`
	Reason                     string    `gorm:"column:reason"`
	Result                     *string   `gorm:"column:result"`
	RefundAmount               *string   `gorm:"column:refund_amount_text"`
	ProviderSummary            *string   `gorm:"column:provider_summary_text"`
	CreatedAt                  time.Time `gorm:"column:created_at"`
	UpdatedAt                  time.Time `gorm:"column:updated_at"`
}

type ordersAdjustmentRow struct {
	ID                int32      `gorm:"column:id"`
	UserID            int32      `gorm:"column:user_id"`
	RequestedByUserID int32      `gorm:"column:requested_by_user_id"`
	ReviewedByUserID  *int32     `gorm:"column:reviewed_by_user_id"`
	Amount            string     `gorm:"column:amount_text"`
	RequestType       string     `gorm:"column:request_type"`
	Status            string     `gorm:"column:status"`
	SourceType        *string    `gorm:"column:source_type"`
	SourceID          *int32     `gorm:"column:source_id"`
	OrderNo           *string    `gorm:"column:order_no"`
	Reason            string     `gorm:"column:reason"`
	ReviewRemark      *string    `gorm:"column:review_remark"`
	BalanceLogID      *int32     `gorm:"column:balance_log_id"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
	ReviewedAt        *time.Time `gorm:"column:reviewed_at"`
}

type ordersBalanceLogRow struct {
	ID            int32  `gorm:"column:id"`
	Type          string `gorm:"column:type"`
	Amount        string `gorm:"column:amount_text"`
	BalanceBefore string `gorm:"column:balance_before_text"`
	BalanceAfter  string `gorm:"column:balance_after_text"`
}

// ordersLoadOperationCases loads the case rows (with createdBy/updatedBy and the
// refund approval chain) for the given orders, keyed by "sourceType:sourceId".
func ordersLoadOperationCases(ctx context.Context, pairs []ordersCasePair) (map[string]*orderOperationCaseView, error) {
	result := map[string]*orderOperationCaseView{}
	if len(pairs) == 0 {
		return result, nil
	}

	conditions := make([]string, 0, len(pairs))
	args := make([]any, 0, len(pairs)*2)
	for _, pair := range pairs {
		conditions = append(conditions, "(source_type = ? AND source_id = ?)")
		args = append(args, pair.SourceType, pair.SourceID)
	}

	var rows []ordersOperationCaseRow
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT id, source_type, source_id, order_no, user_id, created_by_user_id, updated_by_user_id,"+
			" balance_adjustment_request_id, status::text AS status, reason, result,"+
			" refund_amount::text AS refund_amount_text, provider_summary::text AS provider_summary_text,"+
			" created_at, updated_at FROM order_operation_cases WHERE "+
			strings.Join(conditions, " OR "), args...,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return result, nil
	}

	userIDs := make([]int32, 0, len(rows)*3)
	requestIDs := make([]int32, 0, len(rows))
	for _, row := range rows {
		userIDs = append(userIDs, row.CreatedByUserID)
		if row.UpdatedByUserID != nil {
			userIDs = append(userIDs, *row.UpdatedByUserID)
		}
		if row.BalanceAdjustmentRequestID != nil {
			requestIDs = append(requestIDs, *row.BalanceAdjustmentRequestID)
		}
	}

	// The refund chain pulls in three more user ids plus the balance log.
	requests := map[int32]ordersAdjustmentRow{}
	if len(requestIDs) > 0 {
		var requestRows []ordersAdjustmentRow
		if err := db.DB.WithContext(ctx).Raw(
			"SELECT id, user_id, requested_by_user_id, reviewed_by_user_id, amount::text AS amount_text,"+
				" request_type::text AS request_type, status::text AS status, source_type, source_id, order_no,"+
				" reason, review_remark, balance_log_id, created_at, updated_at, reviewed_at"+
				" FROM balance_adjustment_requests WHERE id IN ?", requestIDs,
		).Scan(&requestRows).Error; err != nil {
			return nil, err
		}
		for _, row := range requestRows {
			requests[row.ID] = row
			userIDs = append(userIDs, row.UserID, row.RequestedByUserID)
			if row.ReviewedByUserID != nil {
				userIDs = append(userIDs, *row.ReviewedByUserID)
			}
		}
	}

	users := map[int32]orderOperationUserRef{}
	userEmails := map[int32]*string{}
	if len(userIDs) > 0 {
		var userRows []struct {
			ID       int32   `gorm:"column:id"`
			Username string  `gorm:"column:username"`
			Email    *string `gorm:"column:email"`
		}
		if err := db.DB.WithContext(ctx).Raw(
			"SELECT id, username, email FROM users WHERE id IN ?", userIDs,
		).Scan(&userRows).Error; err != nil {
			return nil, err
		}
		for _, row := range userRows {
			users[row.ID] = orderOperationUserRef{ID: row.ID, Username: row.Username}
			userEmails[row.ID] = row.Email
		}
	}

	logIDs := make([]int32, 0, len(requests))
	for _, row := range requests {
		if row.BalanceLogID != nil {
			logIDs = append(logIDs, *row.BalanceLogID)
		}
	}
	balanceLogs := map[int32]orderBalanceLogView{}
	if len(logIDs) > 0 {
		var logRows []ordersBalanceLogRow
		if err := db.DB.WithContext(ctx).Raw(
			"SELECT id, type::text AS type, amount::text AS amount_text,"+
				" balance_before::text AS balance_before_text, balance_after::text AS balance_after_text"+
				" FROM balance_logs WHERE id IN ?", logIDs,
		).Scan(&logRows).Error; err != nil {
			return nil, err
		}
		for _, row := range logRows {
			balanceLogs[row.ID] = orderBalanceLogView{
				ID:            row.ID,
				Type:          row.Type,
				Amount:        ordersToMoney(ordersDecimalFromText(row.Amount)),
				BalanceBefore: ordersToMoney(ordersDecimalFromText(row.BalanceBefore)),
				BalanceAfter:  ordersToMoney(ordersDecimalFromText(row.BalanceAfter)),
			}
		}
	}

	userRef := func(id int32) *orderOperationUserRef {
		ref, ok := users[id]
		if !ok {
			return nil
		}
		refCopy := ref
		return &refCopy
	}
	adjustmentUserRef := func(id int32) *orderUserRef {
		ref, ok := users[id]
		if !ok {
			return nil
		}
		email := userEmails[id]
		return &orderUserRef{ID: ref.ID, Username: ref.Username, Email: email}
	}

	for _, row := range rows {
		view := &orderOperationCaseView{
			ID:                         row.ID,
			SourceType:                 row.SourceType,
			SourceID:                   row.SourceID,
			OrderNo:                    row.OrderNo,
			UserID:                     row.UserID,
			Status:                     row.Status,
			Reason:                     row.Reason,
			Result:                     row.Result,
			RefundAmount:               ordersOptionalMoney(row.RefundAmount),
			ProviderSummary:            ordersDecodeJSONColumn(row.ProviderSummary),
			CreatedBy:                  userRef(row.CreatedByUserID),
			UpdatedBy:                  nil,
			BalanceAdjustmentRequestID: row.BalanceAdjustmentRequestID,
			CreatedAt:                  ordersISOTime(row.CreatedAt),
			UpdatedAt:                  ordersISOTime(row.UpdatedAt),
		}
		if row.UpdatedByUserID != nil {
			view.UpdatedBy = userRef(*row.UpdatedByUserID)
		}
		if row.BalanceAdjustmentRequestID != nil {
			if request, ok := requests[*row.BalanceAdjustmentRequestID]; ok {
				adjustment := &orderBalanceAdjustmentView{
					ID:           request.ID,
					UserID:       request.UserID,
					User:         adjustmentUserRef(request.UserID),
					RequestedBy:  userRef(request.RequestedByUserID),
					ReviewedBy:   nil,
					Amount:       ordersToMoney(ordersDecimalFromText(request.Amount)),
					RequestType:  request.RequestType,
					Status:       request.Status,
					SourceType:   request.SourceType,
					SourceID:     request.SourceID,
					OrderNo:      request.OrderNo,
					Reason:       request.Reason,
					ReviewRemark: request.ReviewRemark,
					BalanceLogID: request.BalanceLogID,
					CreatedAt:    ordersISOTime(request.CreatedAt),
					UpdatedAt:    ordersISOTime(request.UpdatedAt),
					ReviewedAt:   ordersOptionalISOTime(request.ReviewedAt),
				}
				if request.ReviewedByUserID != nil {
					adjustment.ReviewedBy = userRef(*request.ReviewedByUserID)
				}
				if request.BalanceLogID != nil {
					if balanceLog, ok := balanceLogs[*request.BalanceLogID]; ok {
						logCopy := balanceLog
						adjustment.BalanceLog = &logCopy
					}
				}
				view.BalanceAdjustmentRequest = adjustment
			}
		}
		result[ordersCaseKey(row.SourceType, row.SourceID)] = view
	}
	return result, nil
}

// ordersDecodeJSONColumn renders a JSONB column as the JSON value it holds.
func ordersDecodeJSONColumn(raw *string) any {
	if raw == nil {
		return nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(*raw), &decoded); err != nil {
		return nil
	}
	return decoded
}

// ==================== listing ====================

// ordersListOptions mirrors listOrders' options object.
type ordersListOptions struct {
	UserID      int32 // the filter value; meaningful only when UserIDSet
	UserIDSet   bool
	Page        int
	PageSize    int
	Type        string
	Status      string
	BillingType string
	IncludeUser bool
	Keyword     *string
	CreatedFrom *time.Time
	CreatedTo   *time.Time
}

// ordersCreatedAtConditions appends the createdAt filter, mirroring
// buildCreatedAtWhere.
func ordersCreatedAtConditions(column string, from, to *time.Time, conditions []string, args []any) ([]string, []any) {
	if from == nil && to == nil {
		return conditions, args
	}
	if from != nil {
		conditions = append(conditions, column+" >= ?")
		args = append(args, *from)
	}
	if to != nil {
		conditions = append(conditions, column+" <= ?")
		args = append(args, *to)
	}
	return conditions, args
}

// ordersKeywordUserIDs mirrors the `prisma.user.findMany` keyword lookup.
func ordersKeywordUserIDs(ctx context.Context, keyword string) ([]int32, error) {
	var rows []struct {
		ID int32 `gorm:"column:id"`
	}
	pattern := "%" + keyword + "%"
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT id FROM users WHERE username ILIKE ? OR email ILIKE ? ORDER BY id LIMIT ?",
		pattern, pattern, ordersKeywordUserTake,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	ids := make([]int32, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids, nil
}

// ordersParseBillingOrderKeyword mirrors parseBillingOrderKeyword.
func ordersParseBillingOrderKeyword(keyword *string) *int32 {
	if keyword == nil || *keyword == "" {
		return nil
	}
	raw := ""
	if match := ordersBillKeywordPattern.FindStringSubmatch(*keyword); match != nil {
		raw = match[1]
	} else if ordersDigitsPattern.MatchString(*keyword) {
		raw = *keyword
	}
	if raw == "" {
		return nil
	}
	parsed, ok := ordersParsePositiveID(raw)
	if !ok {
		return nil
	}
	narrowed, ok := ordersNarrowOrderID(parsed)
	if !ok {
		return nil
	}
	return &narrowed
}

// ordersInPlaceholders renders `IN (?, ?, ...)` arguments for a slice.
func ordersInPlaceholders[T any](values []T) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, len(values))
	for index := range values {
		parts[index] = "?"
	}
	return strings.Join(parts, ", ")
}

// listOrders mirrors listOrders: both source tables are queried independently,
// merged newest-first, sliced to the page and decorated with operation cases.
func listOrders(ctx context.Context, options ordersListOptions) (ordersListResult, error) {
	take := int64(options.Page) * int64(options.PageSize)
	includeRecharge := options.Type == "" || options.Type == "recharge"
	includeBilling := options.Type == "" || options.Type == "instance_billing"
	billingOrderID := ordersParseBillingOrderKeyword(options.Keyword)

	var keywordUserIDs []int32
	if options.Keyword != nil && options.IncludeUser {
		ids, err := ordersKeywordUserIDs(ctx, *options.Keyword)
		if err != nil {
			return ordersListResult{}, err
		}
		keywordUserIDs = ids
	}

	// ---- recharge records ----
	var rechargeTotal int64
	var rechargeRows []ordersRechargeRow
	if includeRecharge {
		conditions := make([]string, 0, 4)
		args := make([]any, 0, 8)
		if options.UserIDSet {
			conditions = append(conditions, "r.user_id = ?")
			args = append(args, options.UserID)
		}
		if options.Status != "" {
			conditions = append(conditions, "r.status = ?")
			args = append(args, options.Status)
		}
		conditions, args = ordersCreatedAtConditions("r.created_at", options.CreatedFrom, options.CreatedTo, conditions, args)
		if options.Keyword != nil {
			pattern := "%" + *options.Keyword + "%"
			clauses := []string{"r.order_no ILIKE ?", "r.trade_no ILIKE ?"}
			clauseArgs := []any{pattern, pattern}
			if len(keywordUserIDs) > 0 {
				clauses = append(clauses, "r.user_id IN ("+ordersInPlaceholders(keywordUserIDs)+")")
				for _, id := range keywordUserIDs {
					clauseArgs = append(clauseArgs, id)
				}
			}
			conditions = append(conditions, "("+strings.Join(clauses, " OR ")+")")
			args = append(args, clauseArgs...)
		}

		where := ""
		if len(conditions) > 0 {
			where = " WHERE " + strings.Join(conditions, " AND ")
		}

		if err := db.DB.WithContext(ctx).Raw(
			"SELECT COUNT(*) FROM recharge_records r"+where, args...,
		).Scan(&rechargeTotal).Error; err != nil {
			return ordersListResult{}, err
		}

		userJoin := ""
		userColumns := "NULL::int AS user_ref_id, NULL::text AS user_ref_name, NULL::text AS user_ref_email"
		if options.IncludeUser {
			userJoin = " LEFT JOIN users u ON u.id = r.user_id"
			userColumns = "u.id AS user_ref_id, u.username AS user_ref_name, u.email AS user_ref_email"
		}

		listArgs := append(append([]any{}, args...), take)
		if err := db.DB.WithContext(ctx).Raw(
			"SELECT r.id, r.user_id, r.order_no, r.trade_no,"+
				" r.amount::text AS amount_text, r.actual_amount::text AS actual_amount_text,"+
				" r.fee::text AS fee_text, r.payment_method, r.status::text AS status, r.fail_reason,"+
				" r.callback_at, r.created_at, r.completed_at, r.expired_at,"+
				" p.id AS provider_ref_id, p.name AS provider_name, p.type::text AS provider_type, "+
				userColumns+
				" FROM recharge_records r"+
				" LEFT JOIN payment_providers p ON p.id = r.provider_id"+userJoin+
				where+" ORDER BY r.created_at DESC LIMIT ?", listArgs...,
		).Scan(&rechargeRows).Error; err != nil {
			return ordersListResult{}, err
		}
	}

	// ---- instance billing records ----
	var billingTotal int64
	var billingRows []ordersBillingRow
	if includeBilling {
		conditions := make([]string, 0, 4)
		args := make([]any, 0, 8)
		if options.UserIDSet {
			conditions = append(conditions, "b.user_id = ?")
			args = append(args, options.UserID)
		}
		if options.BillingType != "" {
			conditions = append(conditions, "b.type = ?")
			args = append(args, options.BillingType)
		}
		conditions, args = ordersCreatedAtConditions("b.created_at", options.CreatedFrom, options.CreatedTo, conditions, args)
		if options.Keyword != nil {
			pattern := "%" + *options.Keyword + "%"
			clauses := make([]string, 0, 4)
			clauseArgs := make([]any, 0, 4)
			if billingOrderID != nil {
				clauses = append(clauses, "b.id = ?")
				clauseArgs = append(clauseArgs, *billingOrderID)
			}
			clauses = append(clauses, "b.remark ILIKE ?")
			clauseArgs = append(clauseArgs, pattern)
			clauses = append(clauses, "i.name ILIKE ?")
			clauseArgs = append(clauseArgs, pattern)
			if len(keywordUserIDs) > 0 {
				clauses = append(clauses, "b.user_id IN ("+ordersInPlaceholders(keywordUserIDs)+")")
				for _, id := range keywordUserIDs {
					clauseArgs = append(clauseArgs, id)
				}
			}
			conditions = append(conditions, "("+strings.Join(clauses, " OR ")+")")
			args = append(args, clauseArgs...)
		}

		where := ""
		if len(conditions) > 0 {
			where = " WHERE " + strings.Join(conditions, " AND ")
		}

		join := " LEFT JOIN instances i ON i.id = b.instance_id"
		if err := db.DB.WithContext(ctx).Raw(
			"SELECT COUNT(*) FROM instance_billing_records b"+join+where, args...,
		).Scan(&billingTotal).Error; err != nil {
			return ordersListResult{}, err
		}

		listArgs := append(append([]any{}, args...), take)
		if err := db.DB.WithContext(ctx).Raw(
			"SELECT b.id, b.instance_id, b.user_id, b.type::text AS type,"+
				" b.amount::text AS amount_text, b.months, b.period_start, b.period_end, b.remark, b.created_at,"+
				" i.id AS instance_ref_id, i.name AS instance_ref_name,"+
				" pp.id AS plan_id, pp.name AS plan_name, pk.id AS package_id, pk.name AS package_name"+
				" FROM instance_billing_records b"+join+
				" LEFT JOIN package_plans pp ON pp.id = i.package_plan_id"+
				" LEFT JOIN packages pk ON pk.id = pp.package_id"+
				where+" ORDER BY b.created_at DESC LIMIT ?", listArgs...,
		).Scan(&billingRows).Error; err != nil {
			return ordersListResult{}, err
		}
	}

	// ---- billing users (single batch, only for the admin feed) ----
	billingUsers := map[int32]*orderUserRef{}
	if options.IncludeUser && len(billingRows) > 0 {
		seen := map[int32]bool{}
		ids := make([]int32, 0, len(billingRows))
		for _, row := range billingRows {
			if !seen[row.UserID] {
				seen[row.UserID] = true
				ids = append(ids, row.UserID)
			}
		}
		var userRows []struct {
			ID       int32   `gorm:"column:id"`
			Username string  `gorm:"column:username"`
			Email    *string `gorm:"column:email"`
		}
		if err := db.DB.WithContext(ctx).Raw(
			"SELECT id, username, email FROM users WHERE id IN ?", ids,
		).Scan(&userRows).Error; err != nil {
			return ordersListResult{}, err
		}
		for _, row := range userRows {
			email := row.Email
			billingUsers[row.ID] = &orderUserRef{ID: row.ID, Username: row.Username, Email: email}
		}
	}

	combined := make([]orderView, 0, len(rechargeRows)+len(billingRows))
	for _, row := range rechargeRows {
		combined = append(combined, ordersBuildRechargeOrder(row))
	}
	for _, row := range billingRows {
		combined = append(combined, ordersBuildBillingOrder(row, billingUsers[row.UserID]))
	}

	// Array#sort is stable in V8, so equal timestamps keep the recharge-first
	// insertion order.
	sort.SliceStable(combined, func(i, j int) bool {
		return combined[i].createdAtTime.After(combined[j].createdAtTime)
	})

	start := (options.Page - 1) * options.PageSize
	if start < 0 {
		start = 0
	}
	if start > len(combined) {
		start = len(combined)
	}
	end := start + options.PageSize
	if end > len(combined) {
		end = len(combined)
	}
	pageOrders := combined[start:end]

	pairs := make([]ordersCasePair, 0, len(pageOrders))
	for _, order := range pageOrders {
		pairs = append(pairs, ordersCasePair{SourceType: order.SourceType, SourceID: order.SourceID})
	}
	cases, err := ordersLoadOperationCases(ctx, pairs)
	if err != nil {
		return ordersListResult{}, err
	}

	orders := make([]orderWithOperationCase, 0, len(pageOrders))
	for _, order := range pageOrders {
		orders = append(orders, orderWithOperationCase{
			orderView:     order,
			OperationCase: cases[ordersCaseKey(order.SourceType, order.SourceID)],
		})
	}

	return ordersListResult{
		Orders:   orders,
		Total:    rechargeTotal + billingTotal,
		Page:     options.Page,
		PageSize: options.PageSize,
	}, nil
}

// ==================== single-record loaders ====================

func ordersFindRecharge(ctx context.Context, id int32, userID int32, withUser bool) (*ordersRechargeRow, error) {
	userJoin := ""
	userColumns := "NULL::int AS user_ref_id, NULL::text AS user_ref_name, NULL::text AS user_ref_email"
	if withUser {
		userJoin = " LEFT JOIN users u ON u.id = r.user_id"
		userColumns = "u.id AS user_ref_id, u.username AS user_ref_name, u.email AS user_ref_email"
	}

	conditions := []string{"r.id = ?"}
	args := []any{id}
	if userID != 0 {
		conditions = append(conditions, "r.user_id = ?")
		args = append(args, userID)
	}

	var rows []ordersRechargeRow
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT r.id, r.user_id, r.order_no, r.trade_no,"+
			" r.amount::text AS amount_text, r.actual_amount::text AS actual_amount_text,"+
			" r.fee::text AS fee_text, r.payment_method, r.status::text AS status, r.fail_reason,"+
			" r.callback_at, r.created_at, r.completed_at, r.expired_at,"+
			" p.id AS provider_ref_id, p.name AS provider_name, p.type::text AS provider_type, "+
			userColumns+
			" FROM recharge_records r LEFT JOIN payment_providers p ON p.id = r.provider_id"+userJoin+
			" WHERE "+strings.Join(conditions, " AND ")+" LIMIT 1", args...,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func ordersFindBilling(ctx context.Context, id int32, userID int32) (*ordersBillingRow, error) {
	conditions := []string{"b.id = ?"}
	args := []any{id}
	if userID != 0 {
		conditions = append(conditions, "b.user_id = ?")
		args = append(args, userID)
	}

	var rows []ordersBillingRow
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT b.id, b.instance_id, b.user_id, b.type::text AS type,"+
			" b.amount::text AS amount_text, b.months, b.period_start, b.period_end, b.remark, b.created_at,"+
			" i.id AS instance_ref_id, i.name AS instance_ref_name,"+
			" pp.id AS plan_id, pp.name AS plan_name, pk.id AS package_id, pk.name AS package_name"+
			" FROM instance_billing_records b"+
			" LEFT JOIN instances i ON i.id = b.instance_id"+
			" LEFT JOIN package_plans pp ON pp.id = i.package_plan_id"+
			" LEFT JOIN packages pk ON pk.id = pp.package_id"+
			" WHERE "+strings.Join(conditions, " AND ")+" LIMIT 1", args...,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func ordersFindUser(ctx context.Context, id int32) (*orderUserRef, error) {
	var rows []struct {
		ID       int32   `gorm:"column:id"`
		Username string  `gorm:"column:username"`
		Email    *string `gorm:"column:email"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT id, username, email FROM users WHERE id = ? LIMIT 1", id,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &orderUserRef{ID: rows[0].ID, Username: rows[0].Username, Email: rows[0].Email}, nil
}

// ==================== operation input ====================

// ordersOperationInput is the normalized body of the operation endpoint.
type ordersOperationInput struct {
	Status              string
	Reason              string
	Result              *string
	RefundAmount        *float64
	RefundAmountDecimal models.Decimal
	CreateRefundRequest bool
}

// ordersOperationError is the thrown `{ error: message }` object.
type ordersOperationError struct {
	Message string
}

// ordersNormalizeOperationText mirrors normalizeOperationText.
func ordersNormalizeOperationText(value any, present bool, field string, required bool) (*string, *ordersOperationError) {
	if !present || value == nil {
		if required {
			return nil, &ordersOperationError{Message: "必须填写" + field}
		}
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, &ordersOperationError{Message: field + "必须是字符串"}
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		if required {
			return nil, &ordersOperationError{Message: "必须填写" + field}
		}
		return nil, nil
	}
	// String#length counts UTF-16 code units.
	if utf16Length(trimmed) > ordersOperationTextMax {
		return nil, &ordersOperationError{
			Message: field + "不能超过 " + strconv.Itoa(ordersOperationTextMax) + " 字符",
		}
	}
	return &trimmed, nil
}

// ordersNormalizeRefundAmount mirrors normalizeRefundAmount.
func ordersNormalizeRefundAmount(value any, present bool, required bool) (*float64, *ordersOperationError) {
	missing := !present || value == nil
	if text, ok := value.(string); ok && text == "" {
		missing = true
	}
	if missing {
		if required {
			return nil, &ordersOperationError{Message: "必须填写退款金额"}
		}
		return nil, nil
	}

	number, ok := value.(float64)
	if !ok || math.IsInf(number, 0) || math.IsNaN(number) {
		return nil, &ordersOperationError{Message: "退款金额必须是有效数字"}
	}
	// JS String(number) keeps at most two decimals only for values that pass the
	// pattern; exponent forms (1e21) fail it exactly as in the original.
	if number <= 0 || number > ordersMaxRefundAmount || !ordersRefundAmountPattern.MatchString(ordersFormatJSNumber(number)) {
		return nil, &ordersOperationError{Message: "退款金额必须大于 0，最多两位小数"}
	}
	rounded := decimal.NewFromFloat(number).Round(2)
	result, _ := rounded.Float64()
	return &result, nil
}

// ordersNormalizeOperationInput mirrors normalizeOrderOperationInput.
func ordersNormalizeOperationInput(body any) (ordersOperationInput, *ordersOperationError) {
	record, ok := body.(map[string]any)
	if !ok {
		return ordersOperationInput{}, &ordersOperationError{Message: "请求参数无效"}
	}

	createRefundRequest := false
	if flag, ok := record["createRefundRequest"].(bool); ok && flag {
		createRefundRequest = true
	}

	reason, reasonErr := ordersNormalizeOperationText(record["reason"], helpBodyHas(record, "reason"), "处理原因", true)
	if reasonErr != nil {
		return ordersOperationInput{}, reasonErr
	}
	result, resultErr := ordersNormalizeOperationText(record["result"], helpBodyHas(record, "result"), "处理结果", false)
	if resultErr != nil {
		return ordersOperationInput{}, resultErr
	}
	refundAmount, refundErr := ordersNormalizeRefundAmount(
		record["refundAmount"], helpBodyHas(record, "refundAmount"), createRefundRequest,
	)
	if refundErr != nil {
		return ordersOperationInput{}, refundErr
	}

	input := ordersOperationInput{
		Status:              ordersNormalizeOperationStatus(record["status"]),
		Reason:              *reason,
		Result:              result,
		RefundAmount:        refundAmount,
		CreateRefundRequest: createRefundRequest,
	}
	if refundAmount != nil {
		input.RefundAmountDecimal = models.Decimal{Decimal: decimal.NewFromFloat(*refundAmount).Round(2)}
	}
	return input, nil
}

// ordersRespondInvalidOperationID mirrors the distinct 400 bodies of the detail
// (English) and operation (Chinese) endpoints.
func ordersRespondInvalidOrderID(c *gin.Context, message string) {
	c.JSON(http.StatusBadRequest, gin.H{"error": message, "code": "INVALID_ORDER_ID"})
}

// ==================== route registration ====================

// RegisterOrders mounts the order-centre routes. app.ts registers this module
// without a prefix, so every path is absolute.
func RegisterOrders(r *gin.Engine) {
	user := r.Group("/api/orders", middleware.AuthenticateUser())
	admin := r.Group("/api/admin/orders", middleware.AuthenticateAdmin())

	// GET /api/orders — the caller's own orders.
	user.GET("", func(c *gin.Context) {
		query := c.Request.URL.Query()
		page := ordersParsePositiveQuery(query.Get("page"), 1)
		pageSize := ordersParsePageSize(query.Get("pageSize"))
		orderType := ordersNormalizeOrderType(query.Get("type"))
		keyword := ordersNormalizeSearch(query.Get("keyword"))

		status := ""
		billingType := ""
		if orderType != "instance_billing" {
			status = ordersNormalizeRechargeStatus(query.Get("status"))
		}
		if orderType != "recharge" {
			billingType = ordersNormalizeBillingType(query.Get("status"))
		}

		result, err := listOrders(c.Request.Context(), ordersListOptions{
			UserID:      middleware.CurrentUser(c).ID,
			UserIDSet:   true,
			Page:        page,
			PageSize:    pageSize,
			Type:        orderType,
			Status:      status,
			BillingType: billingType,
			IncludeUser: false,
			Keyword:     keyword,
			CreatedFrom: ordersParseDateQuery(query.Get("createdFrom")),
			CreatedTo:   ordersParseDateQuery(query.Get("createdTo")),
		})
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		c.JSON(http.StatusOK, result)
	})

	// GET /api/orders/:type/:id — one of the caller's own orders.
	user.GET("/:type/:id", func(c *gin.Context) {
		orderType := ordersNormalizeOrderType(c.Param("type"))
		parsedID, ok := ordersParsePositiveID(c.Param("id"))
		if orderType == "" || !ok {
			ordersRespondInvalidOrderID(c, "Invalid order id")
			return
		}
		id, representable := ordersNarrowOrderID(parsedID)
		if !representable {
			helpRespondInternalError(c)
			return
		}

		ctx := c.Request.Context()
		userID := middleware.CurrentUser(c).ID

		if orderType == "recharge" {
			row, err := ordersFindRecharge(ctx, id, userID, false)
			if err != nil {
				helpRespondInternalError(c)
				return
			}
			if row == nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": "ORDER_NOT_FOUND"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"order": ordersBuildRechargeOrder(*row)})
			return
		}

		row, err := ordersFindBilling(ctx, id, userID)
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		if row == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": "ORDER_NOT_FOUND"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"order": ordersBuildBillingOrder(*row, nil)})
	})

	// GET /api/admin/orders — every order, with the buyer attached.
	admin.GET("", func(c *gin.Context) {
		query := c.Request.URL.Query()
		page := ordersParsePositiveQuery(query.Get("page"), 1)
		pageSize := ordersParsePageSize(query.Get("pageSize"))
		orderType := ordersNormalizeOrderType(query.Get("type"))
		keyword := ordersNormalizeSearch(query.Get("keyword"))

		var userID int32
		userIDSet := false
		if parsed, ok := ordersParsePositiveID(query.Get("userId")); ok {
			narrowed, representable := ordersNarrowOrderID(parsed)
			if !representable {
				// Prisma rejects an Int argument outside the 32-bit range, so the Node
				// route fails with a 500 rather than an empty page.
				helpRespondInternalError(c)
				return
			}
			userID = narrowed
			userIDSet = true
		}

		status := ""
		billingType := ""
		if orderType != "instance_billing" {
			status = ordersNormalizeRechargeStatus(query.Get("status"))
		}
		if orderType != "recharge" {
			billingType = ordersNormalizeBillingType(query.Get("status"))
		}

		result, err := listOrders(c.Request.Context(), ordersListOptions{
			UserID:      userID,
			UserIDSet:   userIDSet,
			Page:        page,
			PageSize:    pageSize,
			Type:        orderType,
			Status:      status,
			BillingType: billingType,
			IncludeUser: true,
			Keyword:     keyword,
			CreatedFrom: ordersParseDateQuery(query.Get("createdFrom")),
			CreatedTo:   ordersParseDateQuery(query.Get("createdTo")),
		})
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		c.JSON(http.StatusOK, result)
	})

	// GET /api/admin/orders/:type/:id — any order plus its operation case.
	admin.GET("/:type/:id", func(c *gin.Context) {
		orderType := ordersNormalizeOrderType(c.Param("type"))
		parsedID, ok := ordersParsePositiveID(c.Param("id"))
		if orderType == "" || !ok {
			ordersRespondInvalidOrderID(c, "Invalid order id")
			return
		}
		id, representable := ordersNarrowOrderID(parsedID)
		if !representable {
			helpRespondInternalError(c)
			return
		}

		ctx := c.Request.Context()
		pairs := []ordersCasePair{{SourceType: orderType, SourceID: id}}

		if orderType == "recharge" {
			row, err := ordersFindRecharge(ctx, id, 0, true)
			if err != nil {
				helpRespondInternalError(c)
				return
			}
			if row == nil {
				c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": "ORDER_NOT_FOUND"})
				return
			}
			cases, err := ordersLoadOperationCases(ctx, pairs)
			if err != nil {
				helpRespondInternalError(c)
				return
			}
			c.JSON(http.StatusOK, gin.H{"order": orderWithOperationCase{
				orderView:     ordersBuildRechargeOrder(*row),
				OperationCase: cases[ordersCaseKey(orderType, id)],
			}})
			return
		}

		row, err := ordersFindBilling(ctx, id, 0)
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		if row == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Order not found", "code": "ORDER_NOT_FOUND"})
			return
		}
		user, err := ordersFindUser(ctx, row.UserID)
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		cases, err := ordersLoadOperationCases(ctx, pairs)
		if err != nil {
			helpRespondInternalError(c)
			return
		}
		c.JSON(http.StatusOK, gin.H{"order": orderWithOperationCase{
			orderView:     ordersBuildBillingOrder(*row, user),
			OperationCase: cases[ordersCaseKey(orderType, id)],
		}})
	})

	// POST /api/admin/orders/:type/:id/operation — record the operational decision
	// and, optionally, open a refund approval request.
	admin.POST("/:type/:id/operation", func(c *gin.Context) {
		orderType := ordersNormalizeOrderType(c.Param("type"))
		parsedID, ok := ordersParsePositiveID(c.Param("id"))
		if orderType == "" || !ok {
			ordersRespondInvalidOrderID(c, "订单 ID 无效")
			return
		}
		id, representable := ordersNarrowOrderID(parsedID)
		if !representable {
			helpRespondInternalError(c)
			return
		}

		decoded, status, message := decodeJSONRequestBody(c)
		if status != 0 {
			c.JSON(status, gin.H{"error": message})
			return
		}
		input, inputErr := ordersNormalizeOperationInput(decoded.Value)
		if inputErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": inputErr.Message})
			return
		}

		ctx := c.Request.Context()

		var rechargeRow *ordersRechargeRow
		var billingRow *ordersBillingRow
		if orderType == "recharge" {
			row, err := ordersFindRecharge(ctx, id, 0, false)
			if err != nil {
				helpRespondInternalError(c)
				return
			}
			rechargeRow = row
		} else {
			row, err := ordersFindBilling(ctx, id, 0)
			if err != nil {
				helpRespondInternalError(c)
				return
			}
			billingRow = row
		}
		if rechargeRow == nil && billingRow == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "订单不存在", "code": "ORDER_NOT_FOUND"})
			return
		}

		orderUserID := int32(0)
		orderAmount := ""
		orderNo := ""
		if rechargeRow != nil {
			orderUserID = rechargeRow.UserID
			orderAmount = rechargeRow.Amount
			orderNo = rechargeRow.OrderNo
		}
		if billingRow != nil {
			orderUserID = billingRow.UserID
			orderAmount = billingRow.Amount
			orderNo = "BILL-" + strconv.FormatInt(int64(billingRow.ID), 10)
		}

		if input.CreateRefundRequest && rechargeRow != nil &&
			rechargeRow.Status != "completed" && rechargeRow.Status != "refunded" {
			c.JSON(http.StatusConflict, gin.H{
				"error": "仅已完成或已退款的充值订单可登记退款",
				"code":  "RECHARGE_NOT_CREDITED",
			})
			return
		}

		// refundLimit = toMoney(actualAmount ?? amount) for recharges, else amount.
		var refundLimit models.Decimal
		if rechargeRow != nil {
			if rechargeRow.ActualAmount != nil {
				refundLimit = ordersDecimalFromText(*rechargeRow.ActualAmount)
			} else {
				refundLimit = ordersDecimalFromText(rechargeRow.Amount)
			}
		} else {
			refundLimit = ordersDecimalFromText(orderAmount)
		}

		if input.RefundAmountDecimal.Decimal.GreaterThan(refundLimit.Decimal) {
			errorMessage := "退款金额不能超过订单金额"
			if rechargeRow != nil {
				errorMessage = "退款金额不能超过实际到账金额"
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": errorMessage})
			return
		}

		var existingPendingRefundID *int32
		if input.CreateRefundRequest {
			var rows []struct {
				ID int32 `gorm:"column:id"`
			}
			if err := db.DB.WithContext(ctx).Raw(
				"SELECT id FROM balance_adjustment_requests"+
					" WHERE request_type = ? AND status = ? AND source_type = ? AND source_id = ? LIMIT 1",
				"refund", "pending", orderType, id,
			).Scan(&rows).Error; err != nil {
				helpRespondInternalError(c)
				return
			}
			if len(rows) > 0 {
				existing := rows[0].ID
				existingPendingRefundID = &existing
			}
		}
		if existingPendingRefundID != nil {
			c.JSON(http.StatusConflict, gin.H{
				"error": "该订单已有待审核退款审批 #" + strconv.FormatInt(int64(*existingPendingRefundID), 10),
			})
			return
		}

		actorID := middleware.CurrentUser(c).ID

		var adjustmentView *orderBalanceAdjustmentView
		var adjustmentID *int32
		if input.CreateRefundRequest {
			sourceType := orderType
			sourceID := id
			orderNoValue := orderNo
			created, err := db.CreateBalanceAdjustmentRequest(ctx, db.BalanceAdjustmentRequestInput{
				UserID:            orderUserID,
				RequestedByUserID: actorID,
				Amount:            input.RefundAmountDecimal,
				RequestType:       models.BalanceAdjustmentRequestTypeRefund,
				Reason:            input.Reason,
				SourceType:        &sourceType,
				SourceID:          &sourceID,
				OrderNo:           &orderNoValue,
			})
			if err != nil {
				helpRespondInternalError(c)
				return
			}
			createdID := created.ID
			adjustmentID = &createdID
			adjustmentView = ordersAdjustmentViewFromCreated(created)
		}

		// providerSummary is the recharge summary object, or Prisma.JsonNull (a
		// JSONB 'null' value, not SQL NULL) for billing orders.
		providerSummary := "null"
		if rechargeRow != nil {
			encoded, err := json.Marshal(ordersBuildProviderStatusSummary(*rechargeRow))
			if err != nil {
				helpRespondInternalError(c)
				return
			}
			providerSummary = string(encoded)
		}

		var refundAmountValue any
		if input.RefundAmount != nil {
			refundAmountValue = *input.RefundAmount
		}
		var resultValue any
		if input.Result != nil {
			resultValue = *input.Result
		}
		var adjustmentValue any
		if adjustmentID != nil {
			adjustmentValue = *adjustmentID
		}

		now := time.Now().UTC()
		if err := db.DB.WithContext(ctx).Exec(
			"INSERT INTO order_operation_cases (source_type, source_id, order_no, user_id, created_by_user_id,"+
				" updated_by_user_id, balance_adjustment_request_id, status, reason, result, refund_amount,"+
				" provider_summary, created_at, updated_at)"+
				" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?::jsonb, ?, ?)"+
				" ON CONFLICT (source_type, source_id) DO UPDATE SET"+
				" status = EXCLUDED.status,"+
				" reason = EXCLUDED.reason,"+
				// Prisma skips `undefined`, so an absent result keeps the stored value.
				" result = COALESCE(EXCLUDED.result, order_operation_cases.result),"+
				" refund_amount = EXCLUDED.refund_amount,"+
				" provider_summary = EXCLUDED.provider_summary,"+
				" updated_by_user_id = EXCLUDED.updated_by_user_id,"+
				" balance_adjustment_request_id = COALESCE(EXCLUDED.balance_adjustment_request_id,"+
				" order_operation_cases.balance_adjustment_request_id),"+
				" updated_at = EXCLUDED.updated_at",
			orderType, id, orderNo, orderUserID, actorID, actorID, adjustmentValue,
			input.Status, input.Reason, resultValue, refundAmountValue, providerSummary, now, now,
		).Error; err != nil {
			helpRespondInternalError(c)
			return
		}

		cases, err := ordersLoadOperationCases(ctx, []ordersCasePair{{SourceType: orderType, SourceID: id}})
		if err != nil {
			helpRespondInternalError(c)
			return
		}

		actor := int64(actorID)
		if adjustmentView != nil {
			db.CreateLog(ctx, &actor, "admin", "order.operation.refund_request",
				"登记订单 "+orderNo+" 退款审批 #"+strconv.FormatInt(int64(adjustmentView.ID), 10)+
					"，金额 "+formatTerminalCommandNumber(*input.RefundAmount)+
					"，状态 "+input.Status,
				db.LogResultSuccess, db.CreateLogOptions{})
		} else {
			db.CreateLog(ctx, &actor, "admin", "order.operation.update",
				"更新订单 "+orderNo+" 运营处理状态为 "+input.Status,
				db.LogResultSuccess, db.CreateLogOptions{})
		}

		responseMessage := "订单运营处理记录已保存"
		responseStatus := http.StatusOK
		if adjustmentView != nil {
			responseMessage = "退款登记已提交调账审批，等待审核执行"
			responseStatus = http.StatusCreated
		}

		c.JSON(responseStatus, gin.H{
			"message":           responseMessage,
			"operationCase":     cases[ordersCaseKey(orderType, id)],
			"adjustmentRequest": adjustmentView,
		})
	})
}

// ordersAdjustmentViewFromCreated mirrors serializeBalanceAdjustmentRequest for the
// freshly created request (which already carries its relations).
func ordersAdjustmentViewFromCreated(created db.BalanceAdjustmentRequestWithRelations) *orderBalanceAdjustmentView {
	view := &orderBalanceAdjustmentView{
		ID:           created.ID,
		UserID:       created.UserID,
		Amount:       ordersToMoney(created.Amount),
		RequestType:  string(created.RequestType),
		Status:       string(created.Status),
		SourceType:   created.SourceType,
		SourceID:     created.SourceID,
		OrderNo:      created.OrderNo,
		Reason:       created.Reason,
		ReviewRemark: created.ReviewRemark,
		BalanceLogID: created.BalanceLogID,
		CreatedAt:    ordersISOTime(created.CreatedAt),
		UpdatedAt:    ordersISOTime(created.UpdatedAt),
		ReviewedAt:   ordersOptionalISOTime(created.ReviewedAt),
	}
	if created.User != nil {
		view.User = &orderUserRef{ID: created.User.ID, Username: created.User.Username, Email: created.User.Email}
	}
	if created.RequestedBy != nil {
		view.RequestedBy = &orderOperationUserRef{ID: created.RequestedBy.ID, Username: created.RequestedBy.Username}
	}
	if created.ReviewedBy != nil {
		view.ReviewedBy = &orderOperationUserRef{ID: created.ReviewedBy.ID, Username: created.ReviewedBy.Username}
	}
	if created.BalanceLog != nil {
		view.BalanceLog = &orderBalanceLogView{
			ID:            created.BalanceLog.ID,
			Type:          string(created.BalanceLog.Type),
			Amount:        ordersToMoney(created.BalanceLog.Amount),
			BalanceBefore: ordersToMoney(created.BalanceLog.BalanceBefore),
			BalanceAfter:  ordersToMoney(created.BalanceLog.BalanceAfter),
		}
	}
	return view
}

func init() {
	Register(RegisterOrders)
}
