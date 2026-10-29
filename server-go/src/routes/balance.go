// Port of server/src/routes/balance.ts.
//
// 用户余额查询 + 管理员余额管理（调账审批、直接调账、赠送）。余额计算本身在
// db/balance.go（advisory-lock 保护的事务内变动），本文件只做参数归一化、
// 权限判定、审计日志与邮件通知，与 TypeScript 逐行对齐。
//
// 金额形态（关键）：TypeScript 的 getUserBalance / getUserConsumeStats /
// getUsersTotalConsumeMap 都标注 Promise<number>，路由里又统一 `Number(...)`，
// 因此所有出站金额都是 **JSON 数字**，Go 侧必须是 float64 —— 不能是
// models.Decimal（那会序列化成 Prisma 风格字符串）。
package routes

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// Admin balance limits, mirroring the TypeScript constants.
const (
	balanceRemarkMaxLength  = 500
	balanceSourceMaxLength  = 64
	balanceOrderNoMaxLength = 128
)

// balanceAdjustmentAmountPattern mirrors /^-?\d+(\.\d{1,2})?$/.
var balanceAdjustmentAmountPattern = regexp.MustCompile(`^-?[0-9]+(\.[0-9]{1,2})?$`)

// balanceLogTypes mirrors BALANCE_LOG_TYPES — the allowlist for the `type` query
// filter. Anything else (including exchange_* and gift_card_issue) is ignored.
var balanceRouteLogTypes = map[string]models.BalanceLogType{
	string(models.BalanceLogTypeRecharge):       models.BalanceLogTypeRecharge,
	string(models.BalanceLogTypeConsume):        models.BalanceLogTypeConsume,
	string(models.BalanceLogTypeRefund):         models.BalanceLogTypeRefund,
	string(models.BalanceLogTypeAdminAdjust):    models.BalanceLogTypeAdminAdjust,
	string(models.BalanceLogTypeGift):           models.BalanceLogTypeGift,
	string(models.BalanceLogTypeTransferFee):    models.BalanceLogTypeTransferFee,
	string(models.BalanceLogTypeTransferRefund): models.BalanceLogTypeTransferRefund,
	string(models.BalanceLogTypeInviteGenerate): models.BalanceLogTypeInviteGenerate,
}

// balanceBillingRecordTypes mirrors BILLING_RECORD_TYPES.
var balanceBillingRecordTypes = map[string]models.BillingRecordType{
	string(models.BillingRecordTypeNewPurchase): models.BillingRecordTypeNewPurchase,
	string(models.BillingRecordTypeRenew):       models.BillingRecordTypeRenew,
	string(models.BillingRecordTypeUpgrade):     models.BillingRecordTypeUpgrade,
	string(models.BillingRecordTypeDowngrade):   models.BillingRecordTypeDowngrade,
	string(models.BillingRecordTypeRefund):      models.BillingRecordTypeRefund,
	string(models.BillingRecordTypeTransferFee): models.BillingRecordTypeTransferFee,
}

// balanceAdjustmentStatuses mirrors BALANCE_ADJUSTMENT_REQUEST_STATUSES.
var balanceAdjustmentStatuses = map[string]models.BalanceAdjustmentRequestStatus{
	string(models.BalanceAdjustmentRequestStatusPending):  models.BalanceAdjustmentRequestStatusPending,
	string(models.BalanceAdjustmentRequestStatusApproved): models.BalanceAdjustmentRequestStatusApproved,
	string(models.BalanceAdjustmentRequestStatusRejected): models.BalanceAdjustmentRequestStatusRejected,
}

// balanceAdjustmentRequestTypes mirrors BALANCE_ADJUSTMENT_REQUEST_TYPES.
var balanceAdjustmentRequestTypes = map[string]models.BalanceAdjustmentRequestType{
	string(models.BalanceAdjustmentRequestTypeManualAdjust): models.BalanceAdjustmentRequestTypeManualAdjust,
	string(models.BalanceAdjustmentRequestTypeRefund):       models.BalanceAdjustmentRequestTypeRefund,
}

// ==================== 工具 ====================

// balanceISOTime mirrors Date.prototype.toISOString().
func balanceISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func balanceOptionalISOTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := balanceISOTime(*value)
	return &formatted
}

// balanceNormalizeLogType mirrors normalizeBalanceLogType; invalid values become
// the zero value, which the DB layer treats as "no filter".
func balanceNormalizeLogType(value string) models.BalanceLogType {
	if value == "" {
		return ""
	}
	return balanceRouteLogTypes[value]
}

// balanceNormalizeBillingType mirrors normalizeBillingRecordType.
func balanceNormalizeBillingType(value string) models.BillingRecordType {
	if value == "" {
		return ""
	}
	return balanceBillingRecordTypes[value]
}

// balanceNormalizeAdjustmentStatus mirrors normalizeBalanceAdjustmentRequestStatus.
func balanceNormalizeAdjustmentStatus(value string) models.BalanceAdjustmentRequestStatus {
	if value == "" {
		return ""
	}
	return balanceAdjustmentStatuses[value]
}

// balanceParsePage mirrors parsePositiveIntegerQuery(page, 1): strict pattern,
// safe integer, otherwise the fallback.
func balanceParsePage(value string) int64 {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok {
		return 1
	}
	return parsed
}

// balanceParsePageSize mirrors parseClampedPositiveIntegerQuery(pageSize, 20, 100).
func balanceParsePageSize(value string) int {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok {
		return 20
	}
	if parsed > 100 {
		return 100
	}
	return int(parsed)
}

// balanceQueryPage clamps the page for the query layer while the response keeps
// the value the route parsed (the TypeScript handler passes it straight through to
// the DB helper). A JavaScript-safe page combined with pageSize<=100 always fits in
// a 64-bit offset, so this clamp never fires on 64-bit builds and cannot perturb
// the conformance diff.
func balanceQueryPage(page int64, pageSize int) int {
	if pageSize <= 0 {
		return int(page)
	}
	maxPage := int64(math.MaxInt)/int64(pageSize) + 1
	if page > maxPage {
		return int(maxPage)
	}
	return int(page)
}

// balanceParseRouteID mirrors parsePositiveRouteId for a path parameter.
func balanceParseRouteID(value string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok || parsed > math.MaxInt32 {
		return 0, false
	}
	return int32(parsed), true
}

// balanceLotteryGift mirrors `(lotteryGift === 'exclude' || lotteryGift === 'only') ? lotteryGift : undefined`.
func balanceLotteryGift(value string) string {
	if value == "exclude" || value == "only" {
		return value
	}
	return ""
}

// ==================== 输入归一化 ====================

// balanceInputError mirrors the `throw { error: '...' }` objects the TypeScript
// normalizers raise; the routes answer 400 with exactly `{ error }`.
type balanceInputError struct {
	Message string
}

func (e *balanceInputError) Error() string { return e.Message }

func balanceNewInputError(message string) *balanceInputError {
	return &balanceInputError{Message: message}
}

// balanceIsPlainRecord mirrors isPlainRecord: null and arrays are not records.
func balanceIsPlainRecord(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

// balanceNormalizeRemark mirrors normalizeAdminBalanceRemark.
func balanceNormalizeRemark(value any, present bool, required bool, field string) (*string, *balanceInputError) {
	if !present || value == nil {
		if required {
			return nil, balanceNewInputError("必须填写" + field)
		}
		return nil, nil
	}
	text, isString := value.(string)
	if !isString {
		return nil, balanceNewInputError(field + "必须是字符串")
	}
	remark := inboxJSTrim(text)
	if remark == "" {
		if required {
			return nil, balanceNewInputError("必须填写" + field)
		}
		return nil, nil
	}
	if jsStringLength(remark) > balanceRemarkMaxLength {
		return nil, balanceNewInputError(field + "不能超过 500 字符")
	}
	return &remark, nil
}

// balanceNormalizeOptionalShortText mirrors normalizeOptionalShortText.
func balanceNormalizeOptionalShortText(value any, present bool, field string, maxLength int) (*string, *balanceInputError) {
	if !present || value == nil {
		return nil, nil
	}
	text, isString := value.(string)
	if !isString {
		return nil, balanceNewInputError(field + "必须是字符串")
	}
	trimmed := inboxJSTrim(text)
	if trimmed == "" {
		return nil, nil
	}
	if jsStringLength(trimmed) > maxLength {
		return nil, balanceNewInputError(field + "不能超过 " + itoa(maxLength) + " 字符")
	}
	return &trimmed, nil
}

// balanceNormalizeOptionalPositiveSafeInteger mirrors
// normalizeOptionalPositiveSafeInteger. `value === ”` is also "absent", which a
// JSON body can never produce; it is kept for parity.
func balanceNormalizeOptionalPositiveSafeInteger(value any, present bool, field string) (*int32, *balanceInputError) {
	if !present || value == nil {
		return nil, nil
	}
	if text, isString := value.(string); isString && text == "" {
		return nil, nil
	}
	number, isNumber := value.(float64)
	if !isNumber || math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) || number <= 0 {
		return nil, balanceNewInputError(field + "必须是正整数")
	}
	if number > float64(maxSafeIntegerJS) || number > math.MaxInt32 {
		return nil, balanceNewInputError(field + "必须是正整数")
	}
	converted := int32(number)
	return &converted, nil
}

// balanceReadFiniteNumber mirrors the
// `typeof amount !== 'number' || !Number.isFinite(amount)` guard.
func balanceReadFiniteNumber(object map[string]any) (float64, bool) {
	raw, present := object["amount"]
	if !present || raw == nil {
		return 0, false
	}
	number, ok := raw.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return number, true
}

// balanceAdminAdjustInput mirrors AdminBalanceAdjustInput.
type balanceAdminAdjustInput struct {
	Amount float64
	Remark string
}

// balanceNormalizeAdminAdjustInput mirrors normalizeAdminBalanceAdjustInput.
func balanceNormalizeAdminAdjustInput(body any) (balanceAdminAdjustInput, *balanceInputError) {
	object, ok := balanceIsPlainRecord(body)
	if !ok {
		return balanceAdminAdjustInput{}, balanceNewInputError("请求参数无效")
	}
	amount, ok := balanceReadFiniteNumber(object)
	if !ok {
		return balanceAdminAdjustInput{}, balanceNewInputError("调整金额必须是有效的数字")
	}
	if amount == 0 {
		return balanceAdminAdjustInput{}, balanceNewInputError("调整金额不能为 0")
	}
	rawRemark, present := object["remark"]
	remark, remarkError := balanceNormalizeRemark(rawRemark, present, true, "调整原因")
	if remarkError != nil {
		return balanceAdminAdjustInput{}, remarkError
	}
	return balanceAdminAdjustInput{Amount: amount, Remark: *remark}, nil
}

// balanceAdminAdjustmentRequestInput mirrors AdminBalanceAdjustmentRequestInput.
type balanceAdminAdjustmentRequestInput struct {
	Amount      float64
	RequestType models.BalanceAdjustmentRequestType
	Reason      string
	SourceType  *string
	SourceID    *int32
	OrderNo     *string
}

// balanceNormalizeAdjustmentRequestInput mirrors
// normalizeAdminBalanceAdjustmentRequestInput.
func balanceNormalizeAdjustmentRequestInput(body any) (balanceAdminAdjustmentRequestInput, *balanceInputError) {
	object, ok := balanceIsPlainRecord(body)
	if !ok {
		return balanceAdminAdjustmentRequestInput{}, balanceNewInputError("请求参数无效")
	}

	amount, ok := balanceReadFiniteNumber(object)
	if !ok {
		return balanceAdminAdjustmentRequestInput{}, balanceNewInputError("调账金额必须是有效的数字")
	}
	if amount == 0 {
		return balanceAdminAdjustmentRequestInput{}, balanceNewInputError("调账金额不能为 0")
	}
	// `/^-?\d+(\.\d{1,2})?$/.test(String(amount))`
	if !balanceAdjustmentAmountPattern.MatchString(formatTerminalCommandNumber(amount)) {
		return balanceAdminAdjustmentRequestInput{}, balanceNewInputError("调账金额最多保留两位小数")
	}

	requestType := models.BalanceAdjustmentRequestTypeManualAdjust
	if raw, present := object["requestType"]; present {
		if text, isString := raw.(string); isString {
			if normalized, known := balanceAdjustmentRequestTypes[text]; known {
				requestType = normalized
			}
		}
	}
	if requestType == models.BalanceAdjustmentRequestTypeRefund && amount <= 0 {
		return balanceAdminAdjustmentRequestInput{}, balanceNewInputError("退款申请金额必须大于 0")
	}

	rawReason, reasonPresent := object["reason"]
	reason, reasonError := balanceNormalizeRemark(rawReason, reasonPresent, true, "申请原因")
	if reasonError != nil {
		return balanceAdminAdjustmentRequestInput{}, reasonError
	}

	rawSourceType, sourceTypePresent := object["sourceType"]
	sourceType, sourceTypeError := balanceNormalizeOptionalShortText(rawSourceType, sourceTypePresent, "来源类型", balanceSourceMaxLength)
	if sourceTypeError != nil {
		return balanceAdminAdjustmentRequestInput{}, sourceTypeError
	}

	rawSourceID, sourceIDPresent := object["sourceId"]
	sourceID, sourceIDError := balanceNormalizeOptionalPositiveSafeInteger(rawSourceID, sourceIDPresent, "来源 ID")
	if sourceIDError != nil {
		return balanceAdminAdjustmentRequestInput{}, sourceIDError
	}

	rawOrderNo, orderNoPresent := object["orderNo"]
	orderNo, orderNoError := balanceNormalizeOptionalShortText(rawOrderNo, orderNoPresent, "订单号", balanceOrderNoMaxLength)
	if orderNoError != nil {
		return balanceAdminAdjustmentRequestInput{}, orderNoError
	}

	return balanceAdminAdjustmentRequestInput{
		Amount:      amount,
		RequestType: requestType,
		Reason:      *reason,
		SourceType:  sourceType,
		SourceID:    sourceID,
		OrderNo:     orderNo,
	}, nil
}

// balanceAdminAdjustmentReviewInput mirrors AdminBalanceAdjustmentReviewInput.
type balanceAdminAdjustmentReviewInput struct {
	Remark *string
}

// balanceNormalizeReviewInput mirrors
// normalizeAdminBalanceAdjustmentReviewInput.
func balanceNormalizeReviewInput(body any, present bool, required bool) (balanceAdminAdjustmentReviewInput, *balanceInputError) {
	if !present || body == nil {
		if required {
			return balanceAdminAdjustmentReviewInput{}, balanceNewInputError("必须填写审核备注")
		}
		return balanceAdminAdjustmentReviewInput{}, nil
	}
	object, ok := balanceIsPlainRecord(body)
	if !ok {
		return balanceAdminAdjustmentReviewInput{}, balanceNewInputError("请求参数无效")
	}
	rawRemark, remarkPresent := object["remark"]
	remark, remarkError := balanceNormalizeRemark(rawRemark, remarkPresent, required, "审核备注")
	if remarkError != nil {
		return balanceAdminAdjustmentReviewInput{}, remarkError
	}
	return balanceAdminAdjustmentReviewInput{Remark: remark}, nil
}

// balanceAdminGiftInput mirrors AdminBalanceGiftInput.
type balanceAdminGiftInput struct {
	Amount float64
	Remark *string
}

// balanceNormalizeGiftInput mirrors normalizeAdminBalanceGiftInput.
func balanceNormalizeGiftInput(body any) (balanceAdminGiftInput, *balanceInputError) {
	object, ok := balanceIsPlainRecord(body)
	if !ok {
		return balanceAdminGiftInput{}, balanceNewInputError("请求参数无效")
	}
	amount, ok := balanceReadFiniteNumber(object)
	if !ok {
		return balanceAdminGiftInput{}, balanceNewInputError("赠送金额必须是有效的数字")
	}
	if amount <= 0 {
		return balanceAdminGiftInput{}, balanceNewInputError("赠送金额必须大于 0")
	}
	rawRemark, remarkPresent := object["remark"]
	remark, remarkError := balanceNormalizeRemark(rawRemark, remarkPresent, false, "赠送原因")
	if remarkError != nil {
		return balanceAdminGiftInput{}, remarkError
	}
	return balanceAdminGiftInput{Amount: amount, Remark: remark}, nil
}

// itoa renders an integer id for the audit-log template literals.
func itoa(value int) string {
	return strconv.Itoa(value)
}

// ==================== 序列化 ====================

// balanceSerializeAdjustmentRequest mirrors serializeBalanceAdjustmentRequest.
// The relations are re-projected by hand because the TypeScript selects differ
// per relation: `user` carries email, `requestedBy`/`reviewedBy` only id+username.
func balanceSerializeAdjustmentRequest(request db.BalanceAdjustmentRequestWithRelations) gin.H {
	out := gin.H{
		"id":           request.ID,
		"userId":       request.UserID,
		"user":         nil,
		"requestedBy":  nil,
		"reviewedBy":   nil,
		"amount":       balanceDecimalToFloat64(request.Amount),
		"requestType":  string(request.RequestType),
		"status":       string(request.Status),
		"sourceType":   request.SourceType,
		"sourceId":     request.SourceID,
		"orderNo":      request.OrderNo,
		"reason":       request.Reason,
		"reviewRemark": request.ReviewRemark,
		"balanceLogId": request.BalanceLogID,
		"balanceLog":   nil,
		"createdAt":    balanceISOTime(request.CreatedAt),
		"updatedAt":    balanceISOTime(request.UpdatedAt),
		"reviewedAt":   balanceOptionalISOTime(request.ReviewedAt),
	}

	if request.User != nil {
		out["user"] = gin.H{
			"id":       request.User.ID,
			"username": request.User.Username,
			"email":    request.User.Email,
		}
	}
	if request.RequestedBy != nil {
		out["requestedBy"] = gin.H{
			"id":       request.RequestedBy.ID,
			"username": request.RequestedBy.Username,
		}
	}
	if request.ReviewedBy != nil {
		out["reviewedBy"] = gin.H{
			"id":       request.ReviewedBy.ID,
			"username": request.ReviewedBy.Username,
		}
	}
	if request.BalanceLog != nil {
		out["balanceLog"] = balanceSerializeBalanceLog(*request.BalanceLog)
	}
	return out
}

// balanceSerializeBalanceLog mirrors the nested balanceLog projection.
func balanceSerializeBalanceLog(balanceLog models.BalanceLog) gin.H {
	return gin.H{
		"id":            balanceLog.ID,
		"type":          string(balanceLog.Type),
		"amount":        balanceDecimalToFloat64(balanceLog.Amount),
		"balanceBefore": balanceDecimalToFloat64(balanceLog.BalanceBefore),
		"balanceAfter":  balanceDecimalToFloat64(balanceLog.BalanceAfter),
	}
}

// balanceDecimalToFloat64 mirrors Number(decimal).
func balanceDecimalToFloat64(value models.Decimal) float64 {
	result, _ := value.Decimal.Float64()
	return result
}

// balanceSerializeLogRow mirrors the log mapping shared by the user and admin
// log endpoints.
func balanceSerializeLogRow(log db.BalanceLogWithInstance) gin.H {
	var instanceName any
	if log.Instance != nil {
		instanceName = log.Instance.Name
	}
	return gin.H{
		"id":            log.ID,
		"type":          string(log.Type),
		"amount":        balanceDecimalToFloat64(log.Amount),
		"balanceBefore": balanceDecimalToFloat64(log.BalanceBefore),
		"balanceAfter":  balanceDecimalToFloat64(log.BalanceAfter),
		"orderId":       log.OrderID,
		"instanceId":    log.InstanceID,
		"instanceName":  instanceName,
		"remark":        log.Remark,
		"createdAt":     balanceISOTime(log.CreatedAt),
	}
}

// ==================== 计费记录（db/billing-records.ts 的本地投影） ====================

// balanceBillingRecord mirrors getUserBillingRecords' row (relationships included).
type balanceBillingRecord struct {
	Record   models.InstanceBillingRecord
	Instance *db.BalanceInstanceRef
}

// balanceBillingStats mirrors getUserBillingStats' return shape.
type balanceBillingStats struct {
	TotalSpent    float64 `json:"totalSpent"`
	TotalRefunded float64 `json:"totalRefunded"`
	PurchaseCount int64   `json:"purchaseCount"`
	RenewCount    int64   `json:"renewCount"`
}

// balanceGetBillingRecords mirrors getUserBillingRecords (ORDER BY createdAt DESC,
// with the instance {id,name} relation).
func balanceGetBillingRecords(ctx context.Context, userID int32, page, pageSize int, recordType models.BillingRecordType) ([]balanceBillingRecord, int64, error) {
	build := func() *gorm.DB {
		query := db.DB.WithContext(ctx).Model(&models.InstanceBillingRecord{}).Where("user_id = ?", userID)
		if recordType != "" {
			query = query.Where("type = ?", string(recordType))
		}
		return query
	}

	var total int64
	if err := build().Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var rows []models.InstanceBillingRecord
	if err := build().
		Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&rows).Error; err != nil {
		return nil, 0, err
	}

	instanceIDs := make([]int32, 0, len(rows))
	for _, row := range rows {
		instanceIDs = append(instanceIDs, row.InstanceID)
	}
	instanceMap := map[int32]db.BalanceInstanceRef{}
	if len(instanceIDs) > 0 {
		var instances []models.Instance
		if err := db.DB.WithContext(ctx).Select("id", "name").Where("id IN ?", instanceIDs).Find(&instances).Error; err != nil {
			return nil, 0, err
		}
		for _, instance := range instances {
			instanceMap[instance.ID] = db.BalanceInstanceRef{ID: instance.ID, Name: instance.Name}
		}
	}

	records := make([]balanceBillingRecord, 0, len(rows))
	for _, row := range rows {
		record := balanceBillingRecord{Record: row}
		if ref, ok := instanceMap[row.InstanceID]; ok {
			refCopy := ref
			record.Instance = &refCopy
		}
		records = append(records, record)
	}
	return records, total, nil
}

// balanceGetBillingStats mirrors getUserBillingStats.
func balanceGetBillingStats(ctx context.Context, userID int32) (balanceBillingStats, error) {
	var spending struct {
		Total models.Decimal `gorm:"column:total"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(amount), 0)::numeric AS total FROM instance_billing_records
		 WHERE user_id = ? AND type IN ('newPurchase', 'renew', 'upgrade')`, userID,
	).Scan(&spending).Error; err != nil {
		return balanceBillingStats{}, err
	}

	var refunds struct {
		Total models.Decimal `gorm:"column:total"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT COALESCE(SUM(ABS(amount)), 0)::numeric AS total FROM instance_billing_records
		 WHERE user_id = ? AND type = 'refund'`, userID,
	).Scan(&refunds).Error; err != nil {
		return balanceBillingStats{}, err
	}

	var purchaseCount int64
	if err := db.DB.WithContext(ctx).Model(&models.InstanceBillingRecord{}).
		Where("user_id = ? AND type = ?", userID, string(models.BillingRecordTypeNewPurchase)).
		Count(&purchaseCount).Error; err != nil {
		return balanceBillingStats{}, err
	}

	var renewCount int64
	if err := db.DB.WithContext(ctx).Model(&models.InstanceBillingRecord{}).
		Where("user_id = ? AND type = ?", userID, string(models.BillingRecordTypeRenew)).
		Count(&renewCount).Error; err != nil {
		return balanceBillingStats{}, err
	}

	return balanceBillingStats{
		TotalSpent:    balanceDecimalToFloat64(spending.Total),
		TotalRefunded: balanceDecimalToFloat64(refunds.Total),
		PurchaseCount: purchaseCount,
		RenewCount:    renewCount,
	}, nil
}

// balanceGetUser mirrors db.findUserById. db/users.ts has not been ported, so the
// project is taken here.
func balanceGetUser(ctx context.Context, id int32) (*models.User, error) {
	var user models.User
	err := db.DB.WithContext(ctx).Select("id", "username", "email").First(&user, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// ==================== 响应辅助 ====================

func balanceInternalError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

func balanceRespondInputError(c *gin.Context, inputError *balanceInputError) {
	c.JSON(http.StatusBadRequest, gin.H{"error": inputError.Message})
}

// balanceLogContent formats a JavaScript number the way a template literal does.
func balanceLogAmount(amount float64) string {
	sign := ""
	if amount > 0 {
		sign = "+"
	}
	return sign + formatTerminalCommandNumber(amount)
}

func balanceDerefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// balanceSendAdjustedEmail mirrors the fire-and-forget
// `try { await sendBalanceAdjustedEmail(...) } catch { console.warn }` blocks: a
// mail failure never changes the response.
func balanceSendAdjustedEmail(ctx context.Context, email, username string, amount float64, remark string, newBalance float64, logPrefix string) {
	if email == "" {
		return
	}
	result := lib.SendBalanceAdjustedEmail(ctx, email, lib.BalanceAdjustedEmailData{
		Username:   username,
		Amount:     amount,
		Remark:     remark,
		NewBalance: newBalance,
		Time:       time.Now(),
	})
	if !result.Success {
		log.Printf("%s 发送邮件失败: %s", logPrefix, result.Error)
	}
}

// balanceDecimalFromFloat mirrors the JS number → Prisma Decimal conversion.
func balanceDecimalFromFloat(value float64) models.Decimal {
	return models.Decimal{Decimal: decimal.NewFromFloat(value)}
}

// RegisterBalance mounts /api/balance.
func RegisterBalance(r *gin.Engine) {
	group := r.Group("/api/balance", middleware.Authenticate())

	// ==================== 用户余额 API ====================

	// GET /api/balance/me
	group.GET("/me", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		balanceAmount, err := db.GetUserBalance(ctx, user.ID)
		if err != nil {
			balanceInternalError(c)
			return
		}
		stats, err := db.GetUserConsumeStats(ctx, user.ID)
		if err != nil {
			balanceInternalError(c)
			return
		}

		c.JSON(http.StatusOK, gin.H{"balance": gin.H{
			"balance":        balanceAmount,
			"frozen":         0, // 暂无冻结功能
			"totalRecharge":  stats.TotalRecharge,
			"totalConsume":   stats.TotalConsume,
			"destroyedValue": stats.TotalDestroyedValue,
		}})
	})

	// GET /api/balance/me/logs
	group.GET("/me/logs", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		query := c.Request.URL.Query()

		page := balanceParsePage(singleQueryValue(query, "page"))
		pageSize := balanceParsePageSize(singleQueryValue(query, "pageSize"))

		result, err := db.GetBalanceLogs(c.Request.Context(), user.ID, db.BalanceLogListOptions{
			Page:        balanceQueryPage(page, pageSize),
			PageSize:    pageSize,
			Type:        balanceNormalizeLogType(singleQueryValue(query, "type")),
			LotteryGift: balanceLotteryGift(singleQueryValue(query, "lotteryGift")),
		})
		if err != nil {
			balanceInternalError(c)
			return
		}

		records := make([]gin.H, 0, len(result.Logs))
		for _, entry := range result.Logs {
			records = append(records, balanceSerializeLogRow(entry))
		}

		c.JSON(http.StatusOK, gin.H{
			"records":  records,
			"total":    result.Total,
			"page":     page,
			"pageSize": pageSize,
		})
	})

	// GET /api/balance/me/billing
	group.GET("/me/billing", func(c *gin.Context) {
		user := middleware.CurrentUser(c)
		query := c.Request.URL.Query()
		ctx := c.Request.Context()

		page := balanceParsePage(singleQueryValue(query, "page"))
		pageSize := balanceParsePageSize(singleQueryValue(query, "pageSize"))

		rows, total, err := balanceGetBillingRecords(ctx, user.ID, balanceQueryPage(page, pageSize), pageSize, balanceNormalizeBillingType(singleQueryValue(query, "type")))
		if err != nil {
			balanceInternalError(c)
			return
		}
		stats, err := balanceGetBillingStats(ctx, user.ID)
		if err != nil {
			balanceInternalError(c)
			return
		}

		records := make([]gin.H, 0, len(rows))
		for _, row := range rows {
			var instance any
			if row.Instance != nil {
				instance = gin.H{"id": row.Instance.ID, "name": row.Instance.Name}
			}
			records = append(records, gin.H{
				"id":          row.Record.ID,
				"instanceId":  row.Record.InstanceID,
				"instance":    instance,
				"type":        string(row.Record.Type),
				"amount":      balanceDecimalToFloat64(row.Record.Amount),
				"months":      row.Record.Months,
				"periodStart": balanceISOTime(row.Record.PeriodStart),
				"periodEnd":   balanceISOTime(row.Record.PeriodEnd),
				"remark":      row.Record.Remark,
				"createdAt":   balanceISOTime(row.Record.CreatedAt),
			})
		}

		c.JSON(http.StatusOK, gin.H{
			"records":  records,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
			"stats":    stats,
		})
	})

	// ==================== 管理员余额管理 API ====================

	// GET /api/balance/admin/:userId
	group.GET("/admin/:userId", middleware.RequireAdmin(), func(c *gin.Context) {
		userID, ok := balanceParseRouteID(c.Param("userId"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		ctx := c.Request.Context()
		targetUser, err := balanceGetUser(ctx, userID)
		if err != nil {
			balanceInternalError(c)
			return
		}
		if targetUser == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
			return
		}

		balance, err := db.GetUserBalance(ctx, userID)
		if err != nil {
			balanceInternalError(c)
			return
		}
		stats, err := db.GetUserConsumeStats(ctx, userID)
		if err != nil {
			balanceInternalError(c)
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"userId":              userID,
			"username":            targetUser.Username,
			"balance":             balance,
			"totalRecharge":       stats.TotalRecharge,
			"totalConsume":        stats.TotalConsume,
			"totalRefund":         stats.TotalRefund,
			"totalDestroyedValue": stats.TotalDestroyedValue,
		})
	})

	// GET /api/balance/admin/:userId/logs
	group.GET("/admin/:userId/logs", middleware.RequireAdmin(), func(c *gin.Context) {
		userID, ok := balanceParseRouteID(c.Param("userId"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		query := c.Request.URL.Query()
		page := balanceParsePage(singleQueryValue(query, "page"))
		pageSize := balanceParsePageSize(singleQueryValue(query, "pageSize"))

		result, err := db.GetBalanceLogs(c.Request.Context(), userID, db.BalanceLogListOptions{
			Page:        balanceQueryPage(page, pageSize),
			PageSize:    pageSize,
			Type:        balanceNormalizeLogType(singleQueryValue(query, "type")),
			LotteryGift: balanceLotteryGift(singleQueryValue(query, "lotteryGift")),
		})
		if err != nil {
			balanceInternalError(c)
			return
		}

		logs := make([]gin.H, 0, len(result.Logs))
		for _, entry := range result.Logs {
			logs = append(logs, balanceSerializeLogRow(entry))
		}

		c.JSON(http.StatusOK, gin.H{
			"logs":     logs,
			"total":    result.Total,
			"page":     page,
			"pageSize": pageSize,
		})
	})

	// GET /api/balance/admin/adjustment-requests
	group.GET("/admin/adjustment-requests", middleware.RequireAdmin(), func(c *gin.Context) {
		query := c.Request.URL.Query()
		page := balanceParsePage(singleQueryValue(query, "page"))
		pageSize := balanceParsePageSize(singleQueryValue(query, "pageSize"))

		options := db.BalanceAdjustmentRequestListOptions{
			Page:     balanceQueryPage(page, pageSize),
			PageSize: pageSize,
			Status:   balanceNormalizeAdjustmentStatus(singleQueryValue(query, "status")),
		}
		// `userId ? parsePositiveRouteId(userId) || undefined : undefined`
		if rawUserID := singleQueryValue(query, "userId"); rawUserID != "" {
			if parsed, ok := balanceParseRouteID(rawUserID); ok {
				options.UserID = parsed
			}
		}

		result, err := db.GetBalanceAdjustmentRequests(c.Request.Context(), options)
		if err != nil {
			balanceInternalError(c)
			return
		}

		requests := make([]gin.H, 0, len(result.Requests))
		for _, request := range result.Requests {
			requests = append(requests, balanceSerializeAdjustmentRequest(request))
		}

		c.JSON(http.StatusOK, gin.H{
			"requests": requests,
			"total":    result.Total,
			"page":     page,
			"pageSize": pageSize,
		})
	})

	// POST /api/balance/admin/:userId/adjustment-requests
	group.POST("/admin/:userId/adjustment-requests", middleware.RequireAdmin(), func(c *gin.Context) {
		body, ok := inboxReadBody(c)
		if !ok {
			return
		}

		userID, ok := balanceParseRouteID(c.Param("userId"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		input, inputError := balanceNormalizeAdjustmentRequestInput(body)
		if inputError != nil {
			balanceRespondInputError(c, inputError)
			return
		}

		actor := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		targetUser, err := balanceGetUser(ctx, userID)
		if err != nil {
			balanceInternalError(c)
			return
		}
		if targetUser == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
			return
		}

		adjustmentRequest, err := db.CreateBalanceAdjustmentRequest(ctx, db.BalanceAdjustmentRequestInput{
			UserID:            userID,
			RequestedByUserID: actor.ID,
			Amount:            balanceDecimalFromFloat(input.Amount),
			RequestType:       input.RequestType,
			Reason:            input.Reason,
			SourceType:        input.SourceType,
			SourceID:          input.SourceID,
			OrderNo:           input.OrderNo,
		})
		if err != nil {
			balanceInternalError(c)
			return
		}

		actorID := int64(actor.ID)
		db.CreateLog(ctx, &actorID, "admin", "balance.adjustment_request.create",
			"创建调账审批申请 #"+itoa(int(adjustmentRequest.ID))+"，用户 "+targetUser.Username+
				"，金额 "+balanceLogAmount(input.Amount)+"，原因："+input.Reason,
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusCreated, gin.H{
			"message": "调账申请已提交，等待审核执行",
			"request": balanceSerializeAdjustmentRequest(adjustmentRequest),
		})
	})

	// POST /api/balance/admin/adjustment-requests/:requestId/approve
	group.POST("/admin/adjustment-requests/:requestId/approve", middleware.RequireAdmin(), func(c *gin.Context) {
		body, present, ok := balanceReadOptionalBody(c)
		if !ok {
			return
		}

		requestID, ok := balanceParseRouteID(c.Param("requestId"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		input, inputError := balanceNormalizeReviewInput(body, present, false)
		if inputError != nil {
			balanceRespondInputError(c, inputError)
			return
		}

		actor := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		result, err := db.ApproveBalanceAdjustmentRequest(ctx, requestID, actor.ID, input.Remark)
		if err != nil {
			// `error instanceof Error ? error.message : '调账审批失败'`
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		requestAmount := balanceDecimalToFloat64(result.Request.Amount)
		username := ""
		if result.Request.User != nil {
			username = result.Request.User.Username
		}

		actorID := int64(actor.ID)
		db.CreateLog(ctx, &actorID, "admin", "balance.adjustment_request.approve",
			"审批通过调账申请 #"+itoa(int(requestID))+"，用户 "+username+
				"，金额 "+balanceLogAmount(requestAmount)+"，新余额 "+formatTerminalCommandNumber(result.NewBalance),
			db.LogResultSuccess, db.CreateLogOptions{})

		if result.Request.User != nil && result.Request.User.Email != nil && *result.Request.User.Email != "" {
			balanceSendAdjustedEmail(ctx, *result.Request.User.Email, result.Request.User.Username,
				requestAmount, result.Request.Reason, result.NewBalance, "[调账审批]")
		}

		c.JSON(http.StatusOK, gin.H{
			"message":    "调账申请已通过并执行",
			"newBalance": result.NewBalance,
			"request":    balanceSerializeAdjustmentRequest(result.Request),
		})
	})

	// POST /api/balance/admin/adjustment-requests/:requestId/reject
	group.POST("/admin/adjustment-requests/:requestId/reject", middleware.RequireAdmin(), func(c *gin.Context) {
		body, present, ok := balanceReadOptionalBody(c)
		if !ok {
			return
		}

		requestID, ok := balanceParseRouteID(c.Param("requestId"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		// The reject route requires the review remark.
		input, inputError := balanceNormalizeReviewInput(body, present, true)
		if inputError != nil {
			balanceRespondInputError(c, inputError)
			return
		}

		actor := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		adjustmentRequest, err := db.RejectBalanceAdjustmentRequest(ctx, requestID, actor.ID, balanceDerefString(input.Remark))
		if err != nil {
			// `error instanceof Error ? error.message : '调账申请驳回失败'`
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		username := ""
		if adjustmentRequest.User != nil {
			username = adjustmentRequest.User.Username
		}

		actorID := int64(actor.ID)
		db.CreateLog(ctx, &actorID, "admin", "balance.adjustment_request.reject",
			"驳回调账申请 #"+itoa(int(requestID))+"，用户 "+username+"，原因："+balanceDerefString(input.Remark),
			db.LogResultSuccess, db.CreateLogOptions{})

		c.JSON(http.StatusOK, gin.H{
			"message": "调账申请已驳回",
			"request": balanceSerializeAdjustmentRequest(adjustmentRequest),
		})
	})

	// POST /api/balance/admin/:userId/adjust
	group.POST("/admin/:userId/adjust", middleware.RequireAdmin(), func(c *gin.Context) {
		body, ok := inboxReadBody(c)
		if !ok {
			return
		}

		userID, ok := balanceParseRouteID(c.Param("userId"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		input, inputError := balanceNormalizeAdminAdjustInput(body)
		if inputError != nil {
			balanceRespondInputError(c, inputError)
			return
		}

		actor := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		targetUser, err := balanceGetUser(ctx, userID)
		if err != nil {
			balanceInternalError(c)
			return
		}
		if targetUser == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
			return
		}

		result := db.AdminAdjustBalance(ctx, userID, balanceDecimalFromFloat(input.Amount),
			"[管理员 "+actor.Username+"] "+input.Remark)
		if !result.Success {
			c.JSON(http.StatusBadRequest, gin.H{"error": result.Error})
			return
		}

		actorID := int64(actor.ID)
		db.CreateLog(ctx, &actorID, "admin", "balance.adjust",
			"Adjusted balance for user "+targetUser.Username+": "+balanceLogAmount(input.Amount)+", reason: "+input.Remark,
			db.LogResultSuccess, db.CreateLogOptions{})

		newBalance := 0.0
		if result.NewBalance != nil {
			newBalance = *result.NewBalance
		}
		if targetUser.Email != nil && *targetUser.Email != "" {
			balanceSendAdjustedEmail(ctx, *targetUser.Email, targetUser.Username,
				input.Amount, input.Remark, newBalance, "[余额调整]")
		}

		var balanceLog any
		if result.BalanceLog != nil {
			balanceLog = balanceSerializeBalanceLog(*result.BalanceLog)
		}

		c.JSON(http.StatusOK, gin.H{
			"message":    "余额调整成功",
			"newBalance": result.NewBalance,
			"balanceLog": balanceLog,
		})
	})

	// POST /api/balance/admin/:userId/gift
	group.POST("/admin/:userId/gift", middleware.RequireAdmin(), func(c *gin.Context) {
		body, ok := inboxReadBody(c)
		if !ok {
			return
		}

		userID, ok := balanceParseRouteID(c.Param("userId"))
		if !ok {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
			return
		}

		input, inputError := balanceNormalizeGiftInput(body)
		if inputError != nil {
			balanceRespondInputError(c, inputError)
			return
		}

		actor := middleware.CurrentUser(c)
		ctx := c.Request.Context()

		targetUser, err := balanceGetUser(ctx, userID)
		if err != nil {
			balanceInternalError(c)
			return
		}
		if targetUser == nil {
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
			return
		}

		balanceRemark := balanceDerefString(input.Remark)
		if balanceRemark == "" {
			balanceRemark = "管理员 " + actor.Username + " 赠送"
		}
		remarkForLog := balanceDerefString(input.Remark)

		result := db.GiftBalance(ctx, userID, balanceDecimalFromFloat(input.Amount), &balanceRemark)
		if !result.Success {
			c.JSON(http.StatusBadRequest, gin.H{"error": result.Error})
			return
		}

		actorID := int64(actor.ID)
		db.CreateLog(ctx, &actorID, "admin", "balance.gift",
			"Gifted "+formatTerminalCommandNumber(input.Amount)+" to user "+targetUser.Username,
			db.LogResultSuccess, db.CreateLogOptions{})

		emailRemark := remarkForLog
		if emailRemark == "" {
			emailRemark = "管理员赠送"
		}
		newBalance := 0.0
		if result.NewBalance != nil {
			newBalance = *result.NewBalance
		}
		if targetUser.Email != nil && *targetUser.Email != "" {
			balanceSendAdjustedEmail(ctx, *targetUser.Email, targetUser.Username,
				input.Amount, emailRemark, newBalance, "[余额赠送]")
		}

		c.JSON(http.StatusOK, gin.H{
			"message":    "赠送成功",
			"newBalance": result.NewBalance,
		})
	})
}

// balanceReadOptionalBody mirrors the approve/reject routes, which declare
// `Body: { remark?: string }` without a schema: Fastify's parser still runs, but
// a request with no body at all leaves request.body undefined.
func balanceReadOptionalBody(c *gin.Context) (any, bool, bool) {
	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return nil, false, false
	}
	if body.Value == nil {
		return nil, false, true
	}
	return body.Value, true, true
}

func init() {
	Register(RegisterBalance)
}
