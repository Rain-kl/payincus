// Port of server/src/routes/admin-billing.ts (Wave D step 4 — the highest-risk
// money surface in the backend: refunds, price changes, manual charges and the
// daily financial reconciliation).
//
// Route surface (22 routes, absolute paths, all admin-only):
//
//	GET    /api/admin/billing/overview
//	GET    /api/admin/billing/records
//	GET    /api/admin/billing/reconciliation
//	POST   /api/admin/billing/reconciliation/run
//	PATCH  /api/admin/billing/reconciliation/items/:id
//	GET    /api/admin/billing/reconciliation/export
//	GET    /api/admin/billing/instances
//	GET    /api/admin/billing/recharge-records
//	POST   /api/admin/billing/recharge-records/:id/sync          (10/min)
//	POST   /api/admin/instances/:id/suspend                      (20/min)
//	POST   /api/admin/instances/:id/unsuspend                    (20/min)
//	POST   /api/admin/instances/:id/extend                       (10/min)
//	POST   /api/admin/instances/:id/refund                       (10/min)
//	POST   /api/admin/instances/:id/delete-and-refund            (10/min)
//	POST   /api/admin/instances/:id/apply-aff                    (20/min)
//	POST   /api/admin/instances/:id/update-price                 (20/min)
//	POST   /api/admin/instances/:id/update-price/preview         (30/min)
//	POST   /api/admin/instances/batch-update-price/preview       (20/min)
//	POST   /api/admin/instances/batch-update-price               (10/min)
//	POST   /api/admin/instances/create                           (10/min)
//	GET    /api/admin/instances/:id/available-plans
//	POST   /api/admin/instances/:id/upgrade-plan                 (10/min)
//
// The 13 inline limits are ENFORCED BY NODE (see docs/GO_MIGRATION.md: limits
// declared on the route definition are live, limits injected from
// config/rate-limit.ts are dead). They are reproduced here per route, keyed by
// ip + method + pattern, exactly like @fastify/rate-limit.
//
// Money invariants (docs/WAVE_D_SPEC.md §0.3 / §0.4 / §5):
//
//   - every balance mutation runs in ONE transaction with the user-balance
//     advisory lock (4110); instance deletion additionally takes 4106;
//     balance_logs stores balance_before / balance_after snapshots;
//   - amounts use lib.JSFixed / lib.CalculatePlanChangeDetails / lib.AddMonths
//     (31-day months); decimals enter SQL as models.Decimal;
//   - the refund quote and the refund execution share ONE function
//     (adminBillingRefundQuote / adminBillingPriceAdjustmentQuote), the same
//     pattern instance-destroy uses;
//   - batch price update is all-or-nothing: one transaction, optimistic `version`
//     guards, and any failure rolls every instance and every balance back;
//   - reconciliation is idempotent per (date) and per (run, itemKey): a rerun
//     never duplicates an item and never resurrects an operator's decision.
//
// Deliberate additions over the TypeScript (documented, audit-only, never visible
// in an API response):
//
//  1. lib.LogAdminAction is written next to each createLog for money mutations
//     (退款 / 删除退款 / 延期 / 改价 / 批量改价 / 应用优惠码 / 升级方案 / 创建实例 /
//     对账 run+patch), so the admin audit trail cannot be lost if a route-specific
//     log line is ever reworded.
//  2. An exact-replay guard for refund and extend: an identical request (same
//     instance, same amount/days, same reason) inside 300s does not move money
//     twice. The TypeScript only relied on the refund cap, which does not protect
//     a partial refund submitted twice. Sequential different partial refunds are
//     unaffected.
package routes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// ==================== constants ====================

const (
	// adminBillingBusinessTimezoneOffsetMinutes mirrors
	// BUSINESS_TIMEZONE_OFFSET_MINUTES (Asia/Shanghai).
	adminBillingBusinessTimezoneOffsetMinutes = 8 * 60
	// adminBillingFinancialNoteMaxLength mirrors FINANCIAL_NOTE_MAX_LENGTH.
	adminBillingFinancialNoteMaxLength = 500
	// adminBillingMaxBatchPriceUpdateInstances mirrors
	// MAX_BATCH_PRICE_UPDATE_INSTANCES.
	adminBillingMaxBatchPriceUpdateInstances = 100
	// adminBillingReplayWindowSeconds bounds the exact-replay guard for refund and
	// extend (mirrors instance-billing's 300s replay window).
	adminBillingReplayWindowSeconds = 300
	// adminBillingLogModule mirrors the createLog module of admin actions.
	adminBillingLogModule = "admin"
)

// adminBillingRecordTypes mirrors BILLING_RECORD_TYPES.
var adminBillingRecordTypes = map[string]bool{
	"newPurchase": true, "renew": true, "upgrade": true,
	"downgrade": true, "refund": true, "transfer_fee": true,
}

// adminBillingInstanceStatuses mirrors INSTANCE_STATUSES.
var adminBillingInstanceStatuses = map[string]bool{
	"creating": true, "running": true, "stopped": true,
	"suspended": true, "error": true, "deleted": true,
}

// adminBillingRechargeStatuses mirrors RECHARGE_STATUSES.
var adminBillingRechargeStatuses = map[string]bool{
	"pending": true, "paid": true, "completed": true,
	"failed": true, "cancelled": true, "refunded": true,
}

// adminBillingReconciliationItemStatuses mirrors RECONCILIATION_ITEM_STATUSES.
var adminBillingReconciliationItemStatuses = map[string]bool{
	"discrepancy": true, "confirmed": true, "ignored": true,
}

// adminBillingReconciliationExportTypes mirrors RECONCILIATION_EXPORT_TYPES and
// keeps its order (the response lists them verbatim).
var adminBillingReconciliationExportTypes = []string{"orders", "balance_logs", "adjustments"}

var adminBillingReconciliationExportTypeSet = map[string]bool{
	"orders": true, "balance_logs": true, "adjustments": true,
}

// adminBillingRevenueTypes mirrors the `revenueTypes` tuple.
var adminBillingRevenueTypes = []string{"newPurchase", "renew", "upgrade", "transfer_fee"}

// ==================== parsing helpers ====================

// adminBillingParsePositiveInteger mirrors parsePositiveInteger:
//
//	const parsed = Number(value)
//	if (!Number.isInteger(parsed) || parsed <= 0) return fallback
//	return max === undefined ? parsed : Math.min(parsed, max)
//
// `max` < 0 means "no clamp" (TS passes undefined).
func adminBillingParsePositiveInteger(value string, fallback, max int) int {
	parsed, ok := adminBillingJSNumber(value)
	if !ok || parsed != math.Trunc(parsed) || parsed <= 0 {
		return fallback
	}
	if parsed > math.MaxInt32 {
		parsed = math.MaxInt32
	}
	result := int(parsed)
	if max >= 0 && result > max {
		result = max
	}
	return result
}

// adminBillingJSNumber mirrors Number(value) for a query-string value:
// "" and whitespace become 0, "1e3" becomes 1000, anything unparseable is
// reported as !ok (TS would produce NaN, which every caller treats the same way).
func adminBillingJSNumber(value string) (float64, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		// Number('') === 0 and Number(undefined) === NaN; both are rejected by the
		// integer/positivity check, so 0 is the honest equivalent.
		return 0, true
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

// adminBillingParseOptionalPositiveInteger mirrors
// parseOptionalPositiveInteger: absent or non-positive means "no filter".
func adminBillingParseOptionalPositiveInteger(value string, present bool) *int32 {
	if !present {
		return nil
	}
	parsed, ok := adminBillingJSNumber(value)
	if !ok || parsed != math.Trunc(parsed) || parsed <= 0 || parsed > math.MaxInt32 {
		return nil
	}
	result := int32(parsed)
	return &result
}

// adminBillingParseRouteID mirrors parsePositiveRouteId + the Prisma int bound.
// A pattern-valid but unrepresentable id yields (0, true): Node would hand it to
// Prisma, which then finds nothing (or throws); treating it as "not found" keeps
// the 404 branch reachable instead of inventing a 400.
func adminBillingParseRouteID(value string) (int32, bool) {
	parsed, ok := lib.ParsePositiveRouteID(value)
	if !ok {
		return 0, false
	}
	if parsed > math.MaxInt32 {
		return 0, true
	}
	return int32(parsed), true
}

// adminBillingNormalizeStringFilter mirrors normalizeStringFilter.
func adminBillingNormalizeStringFilter(value string, allowed map[string]bool) string {
	if value != "" && allowed[value] {
		return value
	}
	return ""
}

// adminBillingBody mirrors `request.body as {…}` for a JSON object. A non-object
// body (null / array / scalar) yields an empty map: the TypeScript destructures
// such bodies into undefined fields as well, except for `null`, which throws.
func adminBillingBody(body *fastifyJSONBody) map[string]any {
	if body == nil {
		return map[string]any{}
	}
	if object, ok := body.Value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

// adminBillingBodyNumber mirrors `typeof x === 'number' && Number.isFinite(x)`.
func adminBillingBodyNumber(object map[string]any, key string) (float64, bool) {
	value, ok := object[key]
	if !ok {
		return 0, false
	}
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false
	}
	return number, true
}

// adminBillingBodyInt mirrors `Number.isInteger(x)` where x is unknown.
func adminBillingBodyInt(object map[string]any, key string) (int, bool) {
	number, ok := adminBillingBodyNumber(object, key)
	if !ok || number != math.Trunc(number) || math.Abs(number) > math.MaxInt32 {
		return 0, false
	}
	return int(number), true
}

// adminBillingBodyString mirrors `typeof x === 'string'`.
func adminBillingBodyString(object map[string]any, key string) (*string, bool) {
	value, ok := object[key]
	if !ok {
		return nil, false
	}
	text, ok := value.(string)
	if !ok {
		return nil, false
	}
	return &text, true
}

// adminBillingBodyBool mirrors `value === true / value === false`.
func adminBillingBodyBool(object map[string]any, key string) (bool, bool) {
	value, ok := object[key]
	if !ok {
		return false, false
	}
	boolean, ok := value.(bool)
	return boolean, ok
}

// adminBillingBodyInt32Array mirrors an `number[]` field: every entry must be a
// positive integer (TS checks `Number.isInteger(id) && id > 0`). rawLength is the
// array length before validation, because the TypeScript checks the 100-item cap
// before it validates the entries.
func adminBillingBodyInt32Array(object map[string]any, key string) (ids []int32, rawLength int, present, valid bool) {
	raw, present := object[key]
	if !present || raw == nil {
		return nil, 0, false, true
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, 0, true, false
	}
	ids = make([]int32, 0, len(items))
	for _, item := range items {
		number, ok := item.(float64)
		if !ok || number != math.Trunc(number) || number <= 0 || number > math.MaxInt32 {
			return nil, len(items), true, false
		}
		ids = append(ids, int32(number))
	}
	return ids, len(items), true, true
}

// ==================== business date ====================

// adminBillingBusinessDate mirrors parseBusinessDate's result.
type adminBillingBusinessDate struct {
	DateString string
	// Date is the UTC midnight the DATE column stores.
	Date time.Time
	// Start / End are the [start, end) UTC instants of that Shanghai day.
	Start time.Time
	End   time.Time
}

// adminBillingParseBusinessDate mirrors parseBusinessDate. JavaScript's
// Date.UTC normalises out-of-range components and the result is compared back to
// the input string, so the effect is a strict calendar check — which is what
// time.Parse("2006-01-02") enforces here.
func adminBillingParseBusinessDate(value string) (*adminBillingBusinessDate, bool) {
	if len(value) != 10 || value[4] != '-' || value[7] != '-' {
		return nil, false
	}
	date, err := time.ParseInLocation("2006-01-02", value, time.UTC)
	if err != nil {
		return nil, false
	}
	if date.Format("2006-01-02") != value {
		return nil, false
	}
	start := date.Add(-time.Duration(adminBillingBusinessTimezoneOffsetMinutes) * time.Minute)
	return &adminBillingBusinessDate{
		DateString: value,
		Date:       date,
		Start:      start,
		End:        start.AddDate(0, 0, 1),
	}, true
}

// ==================== reconciliation helpers ====================

// adminBillingReconciliationValidStatus mirrors normalizeReconciliationStatus.
func adminBillingReconciliationValidStatus(value any) (string, bool) {
	text, ok := value.(string)
	if !ok || !adminBillingReconciliationItemStatuses[text] {
		return "", false
	}
	return text, true
}

// adminBillingNormalizeFinancialNote mirrors normalizeFinancialNote.
func adminBillingNormalizeFinancialNote(value any) (*string, error) {
	if value == nil {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, errors.New("备注必须是字符串")
	}
	note := strings.TrimSpace(text)
	if len(utf16.Encode([]rune(note))) > adminBillingFinancialNoteMaxLength {
		return nil, fmt.Errorf("备注不能超过 %d 字符", adminBillingFinancialNoteMaxLength)
	}
	if note == "" {
		return nil, nil
	}
	return &note, nil
}

// adminBillingResolveRunStatus mirrors resolveRunStatus.
func adminBillingResolveRunStatus(statuses []string) string {
	if len(statuses) == 0 {
		return "normal"
	}
	for _, status := range statuses {
		if status == "discrepancy" {
			return "discrepancy"
		}
	}
	for _, status := range statuses {
		if status == "confirmed" {
			return "confirmed"
		}
	}
	return "ignored"
}

// adminBillingUserView renders the `{id, username}` includes.
func adminBillingUserView(user *db.AdminBillingUserRef) any {
	if user == nil {
		return nil
	}
	return gin.H{"id": user.ID, "username": user.Username}
}

// adminBillingSerializeReconciliationItem mirrors one entry of
// serializeReconciliationRun's `items` map.
func adminBillingSerializeReconciliationItem(row db.AdminBillingReconciliationItem) gin.H {
	item := row.Item
	var amount any
	if item.Amount != nil {
		amount = item.Amount.InexactFloat64()
	}
	var handledAt, createdAt, updatedAt any
	if item.HandledAt != nil {
		handledAt = adminBillingISOTime(*item.HandledAt)
	}
	createdAt = adminBillingISOTime(item.CreatedAt)
	updatedAt = adminBillingISOTime(item.UpdatedAt)

	var detail any
	if len(item.Detail) > 0 {
		detail = json.RawMessage(item.Detail)
	}

	return gin.H{
		"id":         item.ID,
		"itemKey":    item.ItemKey,
		"itemType":   string(item.ItemType),
		"status":     string(item.Status),
		"sourceType": item.SourceType,
		"sourceId":   item.SourceID,
		"userId":     item.UserID,
		"user":       adminBillingUserView(row.User),
		"amount":     amount,
		"title":      item.Title,
		"detail":     detail,
		"note":       item.Note,
		"handledBy":  adminBillingUserView(row.HandledBy),
		"handledAt":  handledAt,
		"createdAt":  createdAt,
		"updatedAt":  updatedAt,
	}
}

// adminBillingSerializeReconciliationRun mirrors serializeReconciliationRun.
func adminBillingSerializeReconciliationRun(run *db.AdminBillingReconciliationRun) any {
	if run == nil {
		return nil
	}

	var summary any
	if len(run.Run.Summary) > 0 {
		summary = json.RawMessage(run.Run.Summary)
	}

	items := make([]gin.H, 0, len(run.Items))
	for _, item := range run.Items {
		items = append(items, adminBillingSerializeReconciliationItem(item))
	}

	return gin.H{
		"id":        run.Run.ID,
		"date":      run.Run.Date.UTC().Format("2006-01-02"),
		"status":    string(run.Run.Status),
		"summary":   summary,
		"createdBy": adminBillingUserView(run.CreatedBy),
		"updatedBy": adminBillingUserView(run.UpdatedBy),
		"createdAt": adminBillingISOTime(run.Run.CreatedAt),
		"updatedAt": adminBillingISOTime(run.Run.UpdatedAt),
		"items":     items,
	}
}

// ==================== CSV helpers ====================

// adminBillingMaskExportIdentifier mirrors maskExportIdentifier (UTF-16 units,
// like the JavaScript slice).
func adminBillingMaskExportIdentifier(value *string) string {
	if value == nil || *value == "" {
		return ""
	}
	units := utf16.Encode([]rune(*value))
	if len(units) <= 8 {
		return string(utf16.Decode(units[:2])) + "***"
	}
	head := string(utf16.Decode(units[:4]))
	tail := string(utf16.Decode(units[len(units)-4:]))
	return head + "***" + tail
}

// adminBillingCSVEscape mirrors csvEscape: null/undefined become empty, dates use
// toISOString, and values containing a quote, comma or newline are quoted with
// doubled quotes.
func adminBillingCSVEscape(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return adminBillingCSVQuote(typed)
	case time.Time:
		return adminBillingISOTime(typed)
	case *time.Time:
		if typed == nil {
			return ""
		}
		return adminBillingISOTime(*typed)
	case int:
		return strconv.Itoa(typed)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return adminBillingJSNumberString(typed)
	case *float64:
		if typed == nil {
			return ""
		}
		return adminBillingJSNumberString(*typed)
	case *int32:
		if typed == nil {
			return ""
		}
		return strconv.FormatInt(int64(*typed), 10)
	case *string:
		if typed == nil {
			return ""
		}
		return adminBillingCSVQuote(*typed)
	default:
		return adminBillingCSVQuote(fmt.Sprint(value))
	}
}

// adminBillingCSVQuote mirrors `/[",\n\r]/.test(text) ? `"${text.replace(/"/g,'""')}"` : text`.
func adminBillingCSVQuote(text string) string {
	if !strings.ContainsAny(text, "\",\n\r") {
		return text
	}
	return `"` + strings.ReplaceAll(text, `"`, `""`) + `"`
}

// adminBillingBuildCSV mirrors buildCsv: rows joined by '\n' plus a final '\n'.
func adminBillingBuildCSV(rows [][]any) string {
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		cells := make([]string, 0, len(row))
		for _, cell := range row {
			cells = append(cells, adminBillingCSVEscape(cell))
		}
		lines = append(lines, strings.Join(cells, ","))
	}
	return strings.Join(lines, "\n") + "\n"
}

// adminBillingJSNumberString mirrors `String(number)` for CSV cells.
func adminBillingJSNumberString(value float64) string {
	if math.IsNaN(value) {
		return "NaN"
	}
	if math.IsInf(value, 0) {
		if value > 0 {
			return "Infinity"
		}
		return "-Infinity"
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// adminBillingISOTime mirrors Date#toISOString (millisecond precision, UTC).
func adminBillingISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// adminBillingISOTimePtr mirrors `date?.toISOString()`.
func adminBillingISOTimePtr(value *time.Time) any {
	if value == nil {
		return nil
	}
	return adminBillingISOTime(*value)
}

// adminBillingFixed2 mirrors `value.toFixed(2)` for message strings: ALWAYS two
// decimals ("0.00", "30.50"), never the shortest form.
func adminBillingFixed2(value float64) string {
	return strconv.FormatFloat(lib.JSFixed(value, 2), 'f', 2, 64)
}

// adminBillingRoundMoney mirrors roundMoney: `Number(value.toFixed(2))`.
func adminBillingRoundMoney(value float64) float64 {
	return lib.JSFixed(value, 2)
}

// adminBillingDecimal mirrors `new Prisma.Decimal(value.toFixed(2))`.
func adminBillingDecimal(value float64) models.Decimal {
	return models.Decimal{Decimal: decimal.NewFromFloat(lib.JSFixed(value, 2))}
}

// adminBillingLocaleCompare approximates `a.localeCompare(b)` with the ICU root
// collation: case-insensitive first, lowercase member first on a case tie.
func adminBillingLocaleCompare(left, right string) int {
	if left == right {
		return 0
	}
	lowerLeft, lowerRight := strings.ToLower(left), strings.ToLower(right)
	if lowerLeft != lowerRight {
		return strings.Compare(lowerLeft, lowerRight)
	}
	if lowerLeft == left {
		return -1
	}
	return 1
}

// ==================== money quotes (ONE source for preview + execution) ====================

// adminBillingRefundQuote is the refund quote shared by
// POST /instances/:id/refund (cap), POST /instances/:id/delete-and-refund
// (remaining value) and the price-adjustment quote. It delegates to the
// already-ported instance-billing quote (calculateInstanceRemainingRefundQuote),
// so the admin and user-facing refund paths cannot drift
// (docs/WAVE_D_SPEC §5: "退款报价与执行必须同一函数").
func adminBillingRefundQuote(ctx context.Context, instance *models.Instance) (instanceBillingRefundQuote, error) {
	return instanceBillingCalculateRefundQuote(ctx, instance)
}

// adminBillingPriceAdjustmentQuote mirrors calculateInstancePriceAdjustmentQuote.
//
// The price-change preview and the execution both call this function, and the
// batch preview/execute does too, so a quote can never be previewed with one
// formula and settled with another.
type adminBillingPriceAdjustmentQuote struct {
	OldPrice      float64
	NewPrice      float64
	BillingCycle  int32
	RemainingDays float64
	DiscountRate  float64
	PriceDiff     float64
}

func adminBillingPriceAdjustmentQuoteFor(
	ctx context.Context,
	instance *models.Instance,
	newPrice float64,
	settleBalance bool,
) (adminBillingPriceAdjustmentQuote, error) {
	oldPrice := 0.0
	if instance.BillingPrice != nil {
		oldPrice = instance.BillingPrice.InexactFloat64()
	}
	roundedNewPrice := adminBillingRoundMoney(newPrice)
	billingCycle := int32(1)
	if instance.BillingCycle != nil {
		billingCycle = *instance.BillingCycle
	}

	discountRate := 0.0
	affBinding, err := db.AffGetInstanceBinding(ctx, instance.ID, nil)
	if err != nil {
		return adminBillingPriceAdjustmentQuote{}, err
	}
	if affBinding != nil && affBinding.AffCode.Enabled {
		discountRate = affBinding.AffCode.DiscountRate.InexactFloat64()
	}

	remainingDays := 0.0
	priceDiff := 0.0
	if settleBalance && instance.ExpiresAt != nil {
		remainingDays = lib.CalculateRemainingDaysPrecise(*instance.ExpiresAt, time.Now().UTC())
		if remainingDays > 0 {
			paidQuote, err := adminBillingRefundQuote(ctx, instance)
			if err != nil {
				return adminBillingPriceAdjustmentQuote{}, err
			}
			priceDiff = lib.CalculatePlanChangeDetails(
				oldPrice, int(billingCycle),
				roundedNewPrice, int(billingCycle),
				remainingDays, discountRate,
				paidQuote.RemainingValue, paidQuote.MaxRefundable,
			).PriceDiff

			promoRefund, err := services.PromoGetRefundEligibility(ctx, instance.ID, nil)
			if err != nil {
				return adminBillingPriceAdjustmentQuote{}, err
			}
			// A promo binding forbids refunding, so a cheaper price must not credit.
			if !promoRefund.IsRefundable && (priceDiff < 0 || roundedNewPrice < oldPrice) {
				priceDiff = 0
			}
		}
	}

	return adminBillingPriceAdjustmentQuote{
		OldPrice:      oldPrice,
		NewPrice:      roundedNewPrice,
		BillingCycle:  billingCycle,
		RemainingDays: remainingDays,
		DiscountRate:  discountRate,
		PriceDiff:     priceDiff,
	}, nil
}

// adminBillingPlanChangeQuote mirrors calculatePlanChange with
// `{preciseRemainingDays: true, minRemainingDays: <caller value>}`. The
// instance-billing route hardcodes a 15-day floor; the admin routes pass null,
// which is why this is a separate entry point over the same lib helper.
type adminBillingPlanChangeQuote struct {
	OldPlan        models.PackagePlan
	NewPlan        models.PackagePlan
	RemainingDays  float64
	OldDailyPrice  float64
	NewDailyPrice  float64
	RemainingValue float64
	NewPlanCost    float64
	DiscountRate   float64
	DiscountAmount float64
	PriceDiff      float64
	IsUpgrade      bool
	CanChange      bool
	CannotReason   *string
}

func adminBillingPlanChange(
	ctx context.Context,
	instance *models.Instance,
	newPlan models.PackagePlan,
	minRemainingDays *float64,
) (adminBillingPlanChangeQuote, error) {
	if instance.PackagePlanID == nil {
		return adminBillingPlanChangeQuote{}, errors.New("免费实例不支持升降级")
	}
	oldPlan, err := db.GetPackagePlanById(ctx, *instance.PackagePlanID)
	if err != nil {
		return adminBillingPlanChangeQuote{}, err
	}
	if oldPlan == nil {
		return adminBillingPlanChangeQuote{}, errors.New("原方案不存在")
	}
	if instance.ExpiresAt == nil {
		return adminBillingPlanChangeQuote{}, errors.New("实例到期时间不存在")
	}

	remainingDays := lib.CalculateRemainingDaysPrecise(*instance.ExpiresAt, time.Now().UTC())
	canChange := true
	var cannotReason *string
	if minRemainingDays != nil && remainingDays < *minRemainingDays {
		canChange = false
		reason := "remaining_days_insufficient"
		cannotReason = &reason
	}

	discountRate := 0.0
	affBinding, err := db.AffGetInstanceBinding(ctx, instance.ID, nil)
	if err != nil {
		return adminBillingPlanChangeQuote{}, err
	}
	if affBinding != nil {
		discountRate = affBinding.AffCode.DiscountRate.InexactFloat64()
	}

	oldCyclePrice := 0.0
	if instance.BillingPrice != nil {
		oldCyclePrice = instance.BillingPrice.InexactFloat64()
	}
	newCyclePrice := newPlan.Price.InexactFloat64() / 100 // 分转元

	paidQuote, err := adminBillingRefundQuote(ctx, instance)
	if err != nil {
		return adminBillingPlanChangeQuote{}, err
	}

	cycle := int32(1)
	if instance.BillingCycle != nil {
		cycle = *instance.BillingCycle
	}

	details := lib.CalculatePlanChangeDetails(
		oldCyclePrice, int(cycle),
		newCyclePrice, int(newPlan.BillingCycle),
		remainingDays, discountRate,
		paidQuote.RemainingValue, paidQuote.MaxRefundable,
	)

	priceDiff := details.PriceDiff
	promoRefund, err := services.PromoGetRefundEligibility(ctx, instance.ID, nil)
	if err != nil {
		return adminBillingPlanChangeQuote{}, err
	}
	if !promoRefund.IsRefundable && (priceDiff < 0 || !details.IsUpgrade) {
		priceDiff = 0
	}

	return adminBillingPlanChangeQuote{
		OldPlan:        *oldPlan,
		NewPlan:        newPlan,
		RemainingDays:  remainingDays,
		OldDailyPrice:  details.OldDailyPrice,
		NewDailyPrice:  details.NewDailyPrice,
		RemainingValue: details.RemainingValue,
		NewPlanCost:    details.NewPlanCost,
		DiscountRate:   discountRate,
		DiscountAmount: details.DiscountAmount,
		PriceDiff:      priceDiff,
		IsUpgrade:      details.IsUpgrade,
		CanChange:      canChange,
		CannotReason:   cannotReason,
	}, nil
}

// ==================== batch price update model ====================

// adminBillingBatchPriceError mirrors BatchPriceUpdateError: an HTTP status plus
// an optional preview snapshot that is echoed back with the error.
type adminBillingBatchPriceError struct {
	Message    string
	StatusCode int
	Preview    *adminBillingBatchPreview
}

func (e *adminBillingBatchPriceError) Error() string { return e.Message }

// adminBillingBatchUser mirrors the `item.user` projection.
type adminBillingBatchUser struct {
	ID       int32
	Username string
	Balance  float64
}

// adminBillingBatchItem mirrors BatchPriceUpdateItem.
type adminBillingBatchItem struct {
	ID              int32
	Name            *string
	User            *adminBillingBatchUser
	OldPrice        *float64
	NewPrice        float64
	BillingCycle    *int32
	RemainingDays   float64
	PriceDiff       float64
	DiscountRate    float64
	Status          string
	Error           *string
	InstanceVersion *int32
	// InstanceVersionPresent distinguishes "absent" (failed item) from 0.
	InstanceVersionPresent bool
}

// adminBillingBatchUserImpact mirrors BatchPriceUpdateUserImpact.
type adminBillingBatchUserImpact struct {
	UserID              int32
	Username            string
	BalanceBefore       float64
	BalanceAfter        float64
	TotalCharge         float64
	TotalRefund         float64
	NetDiff             float64
	InsufficientBalance bool
}

// adminBillingBatchPreview mirrors BatchPriceUpdatePreview.
type adminBillingBatchPreview struct {
	Summary     adminBillingBatchSummary
	CanSubmit   bool
	Items       []adminBillingBatchItem
	UserImpacts []adminBillingBatchUserImpact
}

// adminBillingBatchSummary mirrors the preview summary block.
type adminBillingBatchSummary struct {
	SelectedCount  int
	ValidCount     int
	ChangedCount   int
	UnchangedCount int
	FailedCount    int
	TotalCharge    float64
	TotalRefund    float64
	NetAmount      float64
}

// adminBillingBatchExpectation mirrors BatchPriceUpdateExpectation.
type adminBillingBatchExpectation struct {
	InstanceID int32
	Version    int
}

// adminBillingBatchRequest is parseBatchPriceUpdateBody's result.
type adminBillingBatchRequest struct {
	InstanceIDs   []int32
	NewPrice      float64
	SettleBalance bool
	Expectations  []adminBillingBatchExpectation
}

// adminBillingParseBatchPriceUpdateBody mirrors parseBatchPriceUpdateBody.
func adminBillingParseBatchPriceUpdateBody(body map[string]any) (adminBillingBatchRequest, error) {
	if body == nil {
		return adminBillingBatchRequest{}, &adminBillingBatchPriceError{Message: "请求参数无效", StatusCode: 400}
	}

	instanceIDs, rawLength, present, valid := adminBillingBodyInt32Array(body, "instanceIds")
	if !present || rawLength == 0 {
		return adminBillingBatchRequest{}, &adminBillingBatchPriceError{Message: "请选择要修改价格的实例", StatusCode: 400}
	}
	// The 100-item cap is checked before the entries are validated, like the TS.
	if rawLength > adminBillingMaxBatchPriceUpdateInstances {
		return adminBillingBatchRequest{}, &adminBillingBatchPriceError{
			Message:    fmt.Sprintf("单次最多只能修改 %d 个实例", adminBillingMaxBatchPriceUpdateInstances),
			StatusCode: 400,
		}
	}
	if !valid {
		return adminBillingBatchRequest{}, &adminBillingBatchPriceError{Message: "实例ID列表包含无效值", StatusCode: 400}
	}

	deduped := make([]int32, 0, len(instanceIDs))
	seen := map[int32]bool{}
	for _, id := range instanceIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		deduped = append(deduped, id)
	}
	if len(deduped) == 0 {
		return adminBillingBatchRequest{}, &adminBillingBatchPriceError{Message: "请选择要修改价格的实例", StatusCode: 400}
	}

	newPrice, ok := adminBillingBodyNumber(body, "newPrice")
	if !ok || newPrice < 0 || newPrice > 100000 {
		return adminBillingBatchRequest{}, &adminBillingBatchPriceError{Message: "价格必须是 0-100000 之间的数字", StatusCode: 400}
	}

	expectations := []adminBillingBatchExpectation{}
	if raw, present := body["expectations"]; present && raw != nil {
		items, ok := raw.([]any)
		if !ok {
			return adminBillingBatchRequest{}, &adminBillingBatchPriceError{Message: "预览版本参数无效", StatusCode: 400}
		}
		for _, item := range items {
			object, ok := item.(map[string]any)
			if !ok {
				return adminBillingBatchRequest{}, &adminBillingBatchPriceError{Message: "预览版本参数无效", StatusCode: 400}
			}
			instanceID, version, ok := adminBillingParseExpectation(object)
			if !ok || !seen[instanceID] {
				return adminBillingBatchRequest{}, &adminBillingBatchPriceError{Message: "预览版本参数无效", StatusCode: 400}
			}
			duplicate := false
			for _, existing := range expectations {
				if existing.InstanceID == instanceID {
					duplicate = true
					break
				}
			}
			if !duplicate {
				expectations = append(expectations, adminBillingBatchExpectation{InstanceID: instanceID, Version: version})
			}
		}
	}

	settleBalance := true
	if value, ok := adminBillingBodyBool(body, "settleBalance"); ok {
		settleBalance = value
	}

	return adminBillingBatchRequest{
		InstanceIDs:   deduped,
		NewPrice:      adminBillingRoundMoney(newPrice),
		SettleBalance: settleBalance,
		Expectations:  expectations,
	}, nil
}

// adminBillingParseExpectation mirrors the per-entry validation of `expectations`.
func adminBillingParseExpectation(object map[string]any) (int32, int, bool) {
	instanceID, ok := adminBillingBodyNumber(object, "instanceId")
	if !ok || instanceID != math.Trunc(instanceID) || math.Abs(instanceID) > math.MaxInt32 {
		return 0, 0, false
	}
	version, ok := adminBillingBodyNumber(object, "version")
	if !ok || version != math.Trunc(version) || version < 0 || version > math.MaxInt32 {
		return 0, 0, false
	}
	return int32(instanceID), int(version), true
}

// adminBillingSummarizeBatchItems mirrors summarizeBatchPriceItems.
func adminBillingSummarizeBatchItems(items []adminBillingBatchItem, selectedCount int) adminBillingBatchSummary {
	summary := adminBillingBatchSummary{SelectedCount: selectedCount}
	for _, item := range items {
		if item.Status == "failed" {
			summary.FailedCount++
			continue
		}
		if item.Status == "unchanged" {
			summary.UnchangedCount++
		} else {
			summary.ChangedCount++
		}
		if item.PriceDiff > 0 {
			summary.TotalCharge += item.PriceDiff
		} else if item.PriceDiff < 0 {
			summary.TotalRefund += math.Abs(item.PriceDiff)
		}
	}
	summary.ValidCount = len(items) - summary.FailedCount
	summary.TotalCharge = adminBillingRoundMoney(summary.TotalCharge)
	summary.TotalRefund = adminBillingRoundMoney(summary.TotalRefund)
	summary.NetAmount = adminBillingRoundMoney(summary.TotalCharge - summary.TotalRefund)
	return summary
}

// adminBillingBuildBatchUserImpacts mirrors buildBatchUserImpacts.
func adminBillingBuildBatchUserImpacts(items []adminBillingBatchItem) []adminBillingBatchUserImpact {
	type impact struct {
		userID        int32
		username      string
		balanceBefore float64
		totalCharge   float64
		totalRefund   float64
		netDiff       float64
	}
	order := make([]int32, 0, len(items))
	impacts := map[int32]*impact{}
	for _, item := range items {
		if item.Status == "failed" || item.User == nil || item.PriceDiff == 0 {
			continue
		}
		existing, ok := impacts[item.User.ID]
		if !ok {
			existing = &impact{
				userID:        item.User.ID,
				username:      item.User.Username,
				balanceBefore: item.User.Balance,
			}
			impacts[item.User.ID] = existing
			order = append(order, item.User.ID)
		}
		if item.PriceDiff > 0 {
			existing.totalCharge += item.PriceDiff
		} else if item.PriceDiff < 0 {
			existing.totalRefund += math.Abs(item.PriceDiff)
		}
		existing.netDiff += item.PriceDiff
	}

	result := make([]adminBillingBatchUserImpact, 0, len(order))
	for _, userID := range order {
		entry := impacts[userID]
		totalCharge := adminBillingRoundMoney(entry.totalCharge)
		totalRefund := adminBillingRoundMoney(entry.totalRefund)
		netDiff := adminBillingRoundMoney(entry.netDiff)
		balanceAfter := adminBillingRoundMoney(entry.balanceBefore - netDiff)
		result = append(result, adminBillingBatchUserImpact{
			UserID:              entry.userID,
			Username:            entry.username,
			BalanceBefore:       adminBillingRoundMoney(entry.balanceBefore),
			BalanceAfter:        balanceAfter,
			TotalCharge:         totalCharge,
			TotalRefund:         totalRefund,
			NetDiff:             netDiff,
			InsufficientBalance: balanceAfter < -0.0001,
		})
	}
	sort.SliceStable(result, func(i, j int) bool {
		return adminBillingLocaleCompare(result[i].Username, result[j].Username) < 0
	})
	return result
}

// adminBillingBuildBatchPreview mirrors buildBatchPriceUpdatePreview on the
// shared pool.
func adminBillingBuildBatchPreview(
	ctx context.Context,
	instanceIDs []int32,
	newPrice float64,
	settleBalance bool,
) (*adminBillingBatchPreview, error) {
	return adminBillingBuildBatchPreviewInTx(ctx, db.DB, instanceIDs, newPrice, settleBalance)
}

// adminBillingBuildBatchPreviewInTx is the same quote pass on the caller's
// transaction (the TypeScript passes its tx client to the preview builder so the
// optimistic version read belongs to the same snapshot as the writes).
func adminBillingBuildBatchPreviewInTx(
	ctx context.Context,
	client *gorm.DB,
	instanceIDs []int32,
	newPrice float64,
	settleBalance bool,
) (*adminBillingBatchPreview, error) {
	type instanceRow struct {
		models.Instance
		Username *string         `gorm:"column:user_username"`
		Balance  *models.Decimal `gorm:"column:user_balance"`
	}

	var rows []instanceRow
	if len(instanceIDs) > 0 {
		if err := client.WithContext(ctx).Raw(
			`SELECT i.*, u.username AS user_username, u.balance AS user_balance
			   FROM instances i LEFT JOIN users u ON u.id = i.user_id
			  WHERE i.id IN ?`, instanceIDs,
		).Scan(&rows).Error; err != nil {
			return nil, err
		}
	}
	instanceMap := make(map[int32]instanceRow, len(rows))
	for _, row := range rows {
		instanceMap[row.ID] = row
	}

	items := make([]adminBillingBatchItem, 0, len(instanceIDs))
	for _, instanceID := range instanceIDs {
		row, ok := instanceMap[instanceID]
		if !ok {
			items = append(items, adminBillingBatchItem{
				ID:       instanceID,
				NewPrice: newPrice,
				Status:   "failed",
				Error:    adminBillingStringPtr("实例不存在"),
			})
			continue
		}

		var user *adminBillingBatchUser
		if row.Username != nil {
			balance := 0.0
			if row.Balance != nil {
				balance = row.Balance.InexactFloat64()
			}
			user = &adminBillingBatchUser{ID: row.UserID, Username: *row.Username, Balance: balance}
		}
		oldPriceValue := 0.0
		if row.BillingPrice != nil {
			oldPriceValue = row.BillingPrice.InexactFloat64()
		}
		oldPrice := oldPriceValue

		if row.Status == models.InstanceStatusDeleted {
			items = append(items, adminBillingBatchItem{
				ID:           row.ID,
				Name:         &row.Name,
				User:         user,
				OldPrice:     &oldPrice,
				NewPrice:     newPrice,
				BillingCycle: row.BillingCycle,
				Status:       "failed",
				Error:        adminBillingStringPtr("实例已删除"),
			})
			continue
		}
		if row.PackagePlanID == nil {
			items = append(items, adminBillingBatchItem{
				ID:           row.ID,
				Name:         &row.Name,
				User:         user,
				OldPrice:     &oldPrice,
				NewPrice:     newPrice,
				BillingCycle: row.BillingCycle,
				Status:       "failed",
				Error:        adminBillingStringPtr("免费实例不支持修改价格"),
			})
			continue
		}

		instance := row.Instance
		quote, err := adminBillingPriceAdjustmentQuoteFor(ctx, &instance, newPrice, settleBalance)
		if err != nil {
			return nil, err
		}
		status := "ready"
		if quote.OldPrice == quote.NewPrice {
			status = "unchanged"
		}
		oldPricePtr := quote.OldPrice
		version := instance.Version
		items = append(items, adminBillingBatchItem{
			ID:                     instance.ID,
			Name:                   &instance.Name,
			User:                   user,
			OldPrice:               &oldPricePtr,
			NewPrice:               quote.NewPrice,
			BillingCycle:           &quote.BillingCycle,
			RemainingDays:          quote.RemainingDays,
			PriceDiff:              quote.PriceDiff,
			DiscountRate:           quote.DiscountRate,
			Status:                 status,
			InstanceVersion:        &version,
			InstanceVersionPresent: true,
		})
	}

	summary := adminBillingSummarizeBatchItems(items, len(instanceIDs))
	userImpacts := adminBillingBuildBatchUserImpacts(items)
	canSubmit := summary.ValidCount > 0 &&
		summary.FailedCount == 0 &&
		!adminBillingAnyInsufficientBalance(userImpacts)

	return &adminBillingBatchPreview{
		Summary:     summary,
		CanSubmit:   canSubmit,
		Items:       items,
		UserImpacts: userImpacts,
	}, nil
}

func adminBillingAnyInsufficientBalance(impacts []adminBillingBatchUserImpact) bool {
	for _, impact := range impacts {
		if impact.InsufficientBalance {
			return true
		}
	}
	return false
}

// adminBillingSerializeBatchPreview renders the preview for the wire.
func adminBillingSerializeBatchPreview(preview *adminBillingBatchPreview) any {
	if preview == nil {
		return nil
	}
	items := make([]gin.H, 0, len(preview.Items))
	for _, item := range preview.Items {
		entry := gin.H{
			"id":            item.ID,
			"name":          item.Name,
			"user":          adminBillingSerializeBatchUser(item.User),
			"oldPrice":      item.OldPrice,
			"newPrice":      item.NewPrice,
			"billingCycle":  item.BillingCycle,
			"remainingDays": item.RemainingDays,
			"priceDiff":     item.PriceDiff,
			"discountRate":  item.DiscountRate,
			"status":        item.Status,
		}
		if item.Error != nil {
			entry["error"] = *item.Error
		}
		if item.InstanceVersionPresent {
			entry["instanceVersion"] = *item.InstanceVersion
		}
		items = append(items, entry)
	}

	impacts := make([]gin.H, 0, len(preview.UserImpacts))
	for _, impact := range preview.UserImpacts {
		impacts = append(impacts, gin.H{
			"userId":              impact.UserID,
			"username":            impact.Username,
			"balanceBefore":       impact.BalanceBefore,
			"balanceAfter":        impact.BalanceAfter,
			"totalCharge":         impact.TotalCharge,
			"totalRefund":         impact.TotalRefund,
			"netDiff":             impact.NetDiff,
			"insufficientBalance": impact.InsufficientBalance,
		})
	}

	return gin.H{
		"summary": gin.H{
			"selectedCount":  preview.Summary.SelectedCount,
			"validCount":     preview.Summary.ValidCount,
			"changedCount":   preview.Summary.ChangedCount,
			"unchangedCount": preview.Summary.UnchangedCount,
			"failedCount":    preview.Summary.FailedCount,
			"totalCharge":    preview.Summary.TotalCharge,
			"totalRefund":    preview.Summary.TotalRefund,
			"netAmount":      preview.Summary.NetAmount,
		},
		"canSubmit":   preview.CanSubmit,
		"items":       items,
		"userImpacts": impacts,
	}
}

func adminBillingSerializeBatchUser(user *adminBillingBatchUser) any {
	if user == nil {
		return nil
	}
	return gin.H{"id": user.ID, "username": user.Username, "balance": user.Balance}
}

func adminBillingStringPtr(value string) *string { return &value }

// ==================== GET /api/admin/billing/overview ====================

// adminBillingOverviewHandler mirrors GET /api/admin/billing/overview.
func adminBillingOverviewHandler(c *gin.Context) {
	ctx := c.Request.Context()
	now := time.Now().UTC()

	today, tomorrow := lib.GetTodayRange(now, lib.BusinessTimezone)
	thisMonthStart := lib.GetThisMonthStart(now, lib.BusinessTimezone)
	lastMonthStart, thisMonthExclusive := lib.GetLastMonthRange(now, lib.BusinessTimezone)
	// Prisma's `lte: lastMonthEnd` where lastMonthEnd = thisMonthStart - 1ms.
	lastMonthEnd := thisMonthExclusive.Add(-time.Millisecond)
	expiringDeadline := now.Add(7 * 24 * time.Hour)

	overview, err := db.GetAdminBillingOverview(ctx, db.AdminBillingOverviewInput{
		TodayStart:       today,
		Tomorrow:         tomorrow,
		ThisMonthStart:   thisMonthStart,
		LastMonthStart:   lastMonthStart,
		LastMonthEnd:     lastMonthEnd,
		Now:              now,
		ExpiringDeadline: expiringDeadline,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取计费概览失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"overview": gin.H{
			"totalRevenue":             overview.TotalRevenue,
			"thisMonthRevenue":         overview.ThisMonthRevenue,
			"lastMonthRevenue":         overview.LastMonthRevenue,
			"todayRevenue":             overview.TodayRevenue,
			"totalRefunds":             overview.TotalRefunds,
			"netRevenue":               overview.TotalRevenue + overview.TotalRefundsRaw,
			"paidInstancesCount":       overview.PaidInstancesCount,
			"activePaidInstancesCount": overview.ActivePaidInstancesCount,
			"suspendedCount":           overview.SuspendedCount,
			"expiringCount":            overview.ExpiringCount,
			"revenueMix": gin.H{
				"direct": gin.H{
					"totalAmount":     overview.TotalRevenue,
					"thisMonthAmount": overview.ThisMonthRevenue,
					"todayAmount":     overview.TodayRevenue,
				},
			},
			"recharge": gin.H{
				"totalAmount":     overview.RechargeTotal.Amount,
				"totalCount":      overview.RechargeTotal.Count,
				"thisMonthAmount": overview.RechargeThisMonth.Amount,
				"thisMonthCount":  overview.RechargeThisMonth.Count,
				"todayAmount":     overview.RechargeToday.Amount,
				"todayCount":      overview.RechargeToday.Count,
			},
			"aff": gin.H{
				"totalCommission":     overview.Aff.TotalCommission,
				"totalOrders":         overview.Aff.TotalOrders,
				"thisMonthCommission": overview.Aff.ThisMonthCommission,
				"totalConverted":      overview.Aff.TotalConverted,
				"pendingConvertCount": overview.Aff.PendingConvertCount,
			},
		},
	})
}

// ==================== GET /api/admin/billing/records ====================

// adminBillingRecordsHandler mirrors GET /api/admin/billing/records.
func adminBillingRecordsHandler(c *gin.Context) {
	ctx := c.Request.Context()

	pageNum := adminBillingParsePositiveInteger(c.Query("page"), 1, -1)
	size := adminBillingParsePositiveInteger(c.Query("pageSize"), 20, 100)

	options := db.AdminBillingBillingRecordListOptions{
		Page:       pageNum,
		PageSize:   size,
		Type:       adminBillingNormalizeStringFilter(c.Query("type"), adminBillingRecordTypes),
		UserID:     adminBillingParseOptionalPositiveInteger(c.Query("userId"), c.Query("userId") != ""),
		InstanceID: adminBillingParseOptionalPositiveInteger(c.Query("instanceId"), c.Query("instanceId") != ""),
	}

	records, total, err := db.GetAdminBillingBillingRecords(ctx, options)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取扣费记录失败"})
		return
	}

	views := make([]gin.H, 0, len(records))
	for _, row := range records {
		var instance any
		if row.Instance != nil {
			instance = gin.H{"id": row.Instance.ID, "name": row.Instance.Name}
		}
		var user any
		if row.User != nil {
			user = gin.H{"id": row.User.ID, "username": row.User.Username}
		}
		views = append(views, gin.H{
			"id":          row.Record.ID,
			"type":        string(row.Record.Type),
			"amount":      row.Record.Amount.InexactFloat64(),
			"months":      row.Record.Months,
			"periodStart": adminBillingISOTimePtr(&row.Record.PeriodStart),
			"periodEnd":   adminBillingISOTimePtr(&row.Record.PeriodEnd),
			"remark":      row.Record.Remark,
			"createdAt":   adminBillingISOTime(row.Record.CreatedAt),
			"instance":    instance,
			"user":        user,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"records":  views,
		"total":    total,
		"page":     pageNum,
		"pageSize": size,
	})
}

// ==================== GET /api/admin/billing/reconciliation ====================

// adminBillingReconciliationHandler mirrors GET /api/admin/billing/reconciliation.
func adminBillingReconciliationHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)
	if actor == nil || actor.Role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "需要财务只读或管理员权限", "code": "FINANCE_VIEW_REQUIRED"})
		return
	}

	dateValue := c.Query("date")
	if dateValue == "" {
		dateValue = lib.GetDateStringInTimezone(time.Now(), lib.BusinessTimezone)
	}
	parsed, ok := adminBillingParseBusinessDate(dateValue)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "对账日期格式必须是 YYYY-MM-DD"})
		return
	}

	run, err := db.GetAdminBillingReconciliationRun(ctx, parsed.DateString)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取财务对账结果失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"run":     adminBillingSerializeReconciliationRun(run),
		"date":    parsed.DateString,
		"exports": adminBillingReconciliationExportTypes,
	})
}

// ==================== POST /api/admin/billing/reconciliation/run ====================

// adminBillingReconciliationRunHandler mirrors POST /reconciliation/run.
func adminBillingReconciliationRunHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)
	if actor == nil || actor.Role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "财务只读角色不能运行或修改对账", "code": "FINANCE_READONLY"})
		return
	}

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object := adminBillingBody(body)

	dateValue := lib.GetDateStringInTimezone(time.Now(), lib.BusinessTimezone)
	if raw, present := object["date"]; present && raw != nil {
		text, ok := raw.(string)
		if !ok {
			// `date || default` keeps a truthy non-string, then the YYYY-MM-DD regex
			// rejects it.
			c.JSON(http.StatusBadRequest, gin.H{"error": "对账日期格式必须是 YYYY-MM-DD"})
			return
		}
		if text != "" {
			dateValue = text
		}
	}

	parsed, ok := adminBillingParseBusinessDate(dateValue)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "对账日期格式必须是 YYYY-MM-DD"})
		return
	}

	run, err := db.RunAdminBillingReconciliation(ctx, db.AdminBillingReconciliationRunInput{
		DateString: parsed.DateString,
		Start:      parsed.Start,
		End:        parsed.End,
		ActorID:    actor.ID,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "运行财务对账失败"})
		return
	}

	db.CreateLog(ctx, adminBillingInt64Ptr(int64(actor.ID)), adminBillingLogModule, "billing.reconciliation.run",
		fmt.Sprintf("运行 %s 财务对账，状态 %s", parsed.DateString, run.Run.Status),
		db.LogResultSuccess, db.CreateLogOptions{})
	adminBillingAudit(ctx, c, actor, "billing.reconciliation.run",
		fmt.Sprintf("运行 %s 财务对账，状态 %s", parsed.DateString, run.Run.Status),
		"financial_reconciliation_run", int64(run.Run.ID), fmt.Sprint(run.Run.ID), "")

	c.JSON(http.StatusOK, gin.H{"run": adminBillingSerializeReconciliationRun(run)})
}

// ==================== PATCH /api/admin/billing/reconciliation/items/:id ====================

// adminBillingReconciliationItemHandler mirrors PATCH /reconciliation/items/:id.
func adminBillingReconciliationItemHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)
	if actor == nil || actor.Role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "财务只读角色不能修改对账差异", "code": "FINANCE_READONLY"})
		return
	}

	itemID, valid := adminBillingParseRouteID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "差异项 ID 无效"})
		return
	}

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object := adminBillingBody(body)

	reconciliationStatus, ok := adminBillingReconciliationValidStatus(object["status"])
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "差异状态无效"})
		return
	}

	note, err := adminBillingNormalizeFinancialNote(object["note"])
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	item, runStatus, err := db.UpdateAdminBillingReconciliationItem(ctx, itemID, reconciliationStatus, note, actor.ID)
	if err != nil {
		if errors.Is(err, db.ErrAdminBillingReconciliationItemNotFound) {
			// Prisma P2025 surfaces as a 500 through the route's catch-all.
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新财务对账差异失败"})
		return
	}

	db.CreateLog(ctx, adminBillingInt64Ptr(int64(actor.ID)), adminBillingLogModule, "billing.reconciliation.item.update",
		fmt.Sprintf("更新财务对账差异 #%d 为 %s", itemID, reconciliationStatus),
		db.LogResultSuccess, db.CreateLogOptions{})
	adminBillingAudit(ctx, c, actor, "billing.reconciliation.item.update",
		fmt.Sprintf("更新财务对账差异 #%d 为 %s", itemID, reconciliationStatus),
		"financial_reconciliation_item", int64(itemID), fmt.Sprint(itemID), reconciliationStatus)

	c.JSON(http.StatusOK, gin.H{
		"item":      adminBillingSerializeReconciliationItem(item),
		"runStatus": runStatus,
	})
}

// ==================== GET /api/admin/billing/reconciliation/export ====================

// adminBillingReconciliationExportHandler mirrors GET /reconciliation/export.
func adminBillingReconciliationExportHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)
	if actor == nil || actor.Role != "admin" {
		c.JSON(http.StatusForbidden, gin.H{"error": "需要财务只读或管理员权限", "code": "FINANCE_VIEW_REQUIRED"})
		return
	}

	dateValue := c.Query("date")
	if dateValue == "" {
		dateValue = lib.GetDateStringInTimezone(time.Now(), lib.BusinessTimezone)
	}
	parsed, ok := adminBillingParseBusinessDate(dateValue)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "对账日期格式必须是 YYYY-MM-DD"})
		return
	}

	exportType := c.Query("type")
	if !adminBillingReconciliationExportTypeSet[exportType] {
		c.JSON(http.StatusBadRequest, gin.H{"error": "导出类型无效"})
		return
	}

	var rows [][]any
	var err error
	switch exportType {
	case "orders":
		rows, err = adminBillingExportOrders(ctx, parsed)
	case "balance_logs":
		rows, err = adminBillingExportBalanceLogs(ctx, parsed)
	default:
		rows, err = adminBillingExportAdjustments(ctx, parsed)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "导出财务对账 CSV 失败"})
		return
	}

	fileName := "payincus-reconciliation-" + parsed.DateString + "-" + exportType + ".csv"
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="`+fileName+`"`)
	// The BOM is part of the response body in the TypeScript (`\uFEFF${csv}`), so it
	// must stay here for byte-for-byte parity.
	c.String(http.StatusOK, "\uFEFF"+adminBillingBuildCSV(rows))
}

func adminBillingExportOrders(ctx context.Context, parsed *adminBillingBusinessDate) ([][]any, error) {
	records, err := db.GetAdminBillingExportOrders(ctx, parsed.Start, parsed.End)
	if err != nil {
		return nil, err
	}
	rows := make([][]any, 0, len(records)+1)
	rows = append(rows, []any{
		"订单ID", "订单号", "用户ID", "用户名", "金额", "到账金额", "手续费", "状态",
		"支付渠道", "支付方式", "交易号(脱敏)", "创建时间", "完成时间",
	})
	for _, record := range records {
		var actualAmount any
		if record.ActualAmount != nil {
			actualAmount = *record.ActualAmount
		}
		var completedAt any
		if record.CompletedAt != nil {
			completedAt = *record.CompletedAt
		}
		rows = append(rows, []any{
			record.ID,
			record.OrderNo,
			record.UserID,
			record.Username,
			record.Amount,
			actualAmount,
			record.Fee,
			record.Status,
			record.ProviderName,
			adminBillingStringOrEmpty(record.PaymentMethod),
			adminBillingMaskExportIdentifier(record.TradeNo),
			record.CreatedAt,
			completedAt,
		})
	}
	return rows, nil
}

func adminBillingExportBalanceLogs(ctx context.Context, parsed *adminBillingBusinessDate) ([][]any, error) {
	logs, err := db.GetAdminBillingExportBalanceLogs(ctx, parsed.Start, parsed.End)
	if err != nil {
		return nil, err
	}
	rows := make([][]any, 0, len(logs)+1)
	rows = append(rows, []any{
		"流水ID", "用户ID", "用户名", "类型", "金额", "变更前", "变更后",
		"订单号(脱敏)", "实例ID", "备注", "创建时间",
	})
	for _, log := range logs {
		var instanceID any
		if log.InstanceID != nil {
			instanceID = *log.InstanceID
		}
		rows = append(rows, []any{
			log.ID,
			log.UserID,
			log.Username,
			log.Type,
			log.Amount,
			log.BalanceBefore,
			log.BalanceAfter,
			adminBillingMaskExportIdentifier(log.OrderID),
			instanceID,
			adminBillingStringOrEmpty(log.Remark),
			log.CreatedAt,
		})
	}
	return rows, nil
}

func adminBillingExportAdjustments(ctx context.Context, parsed *adminBillingBusinessDate) ([][]any, error) {
	requests, err := db.GetAdminBillingExportAdjustments(ctx, parsed.Start, parsed.End)
	if err != nil {
		return nil, err
	}
	rows := make([][]any, 0, len(requests)+1)
	rows = append(rows, []any{
		"审批ID", "用户ID", "用户名", "金额", "类型", "状态", "来源类型", "来源ID",
		"订单号(脱敏)", "申请人", "审核人", "原因", "审核备注", "余额流水ID", "创建时间", "审核时间",
	})
	for _, item := range requests {
		var sourceType any
		if item.SourceType != nil {
			sourceType = *item.SourceType
		}
		var sourceID any
		if item.SourceID != nil {
			sourceID = *item.SourceID
		}
		var balanceLogID any
		if item.BalanceLogID != nil {
			balanceLogID = *item.BalanceLogID
		}
		var reviewedAt any
		if item.ReviewedAt != nil {
			reviewedAt = *item.ReviewedAt
		}
		rows = append(rows, []any{
			item.ID,
			item.UserID,
			item.Username,
			item.Amount,
			item.RequestType,
			item.Status,
			sourceType,
			sourceID,
			adminBillingMaskExportIdentifier(item.OrderNo),
			item.RequestedByUsername,
			item.ReviewedByUsername,
			item.Reason,
			adminBillingStringOrEmpty(item.ReviewRemark),
			balanceLogID,
			item.CreatedAt,
			reviewedAt,
		})
	}
	return rows, nil
}

// ==================== GET /api/admin/billing/instances ====================

// adminBillingInstancesHandler mirrors GET /api/admin/billing/instances.
func adminBillingInstancesHandler(c *gin.Context) {
	ctx := c.Request.Context()
	now := time.Now().UTC()

	pageNum := adminBillingParsePositiveInteger(c.Query("page"), 1, -1)
	size := adminBillingParsePositiveInteger(c.Query("pageSize"), 20, 100)

	options := db.AdminBillingPaidInstanceListOptions{
		Page:       pageNum,
		PageSize:   size,
		Status:     adminBillingNormalizeStringFilter(c.Query("status"), adminBillingInstanceStatuses),
		Expiring:   c.Query("expiring") == "true",
		HostID:     adminBillingParseOptionalPositiveInteger(c.Query("hostId"), c.Query("hostId") != ""),
		Now:        now,
		ExpiryCeil: now.Add(7 * 24 * time.Hour),
	}
	if search := strings.TrimSpace(c.Query("search")); search != "" {
		units := utf16.Encode([]rune(search))
		if len(units) > 128 {
			search = string(utf16.Decode(units[:128]))
		}
		options.Search = search
	}

	instances, total, err := db.GetAdminBillingPaidInstances(ctx, options)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取付费实例列表失败"})
		return
	}

	hosts, err := db.GetAdminBillingPaidInstanceHosts(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取付费实例列表失败"})
		return
	}

	views := make([]gin.H, 0, len(instances))
	for _, row := range instances {
		instance := row.Instance

		var remainingDays any
		if instance.ExpiresAt != nil {
			diffMs := float64(instance.ExpiresAt.UnixMilli() - now.UnixMilli())
			remainingDays = int64(math.Ceil(diffMs / (1000 * 60 * 60 * 24)))
		}

		instanceTypeLabel := "PRO"
		if row.Package != nil && row.Package.InstanceType == "vm" {
			instanceTypeLabel = "PRIME"
		}

		affDiscountRate := 0.0
		if row.HasAffBinding && row.AffCodeEnabled {
			affDiscountRate = row.AffDiscountRate
		}

		var packageView any
		if row.Package != nil {
			packageView = gin.H{
				"id":           row.Package.ID,
				"name":         row.Package.Name,
				"instanceType": row.Package.InstanceType,
			}
		}
		var planView any
		if row.PackagePlan != nil {
			planView = gin.H{"id": row.PackagePlan.ID, "name": row.PackagePlan.Name}
		}
		var billingPrice any
		if instance.BillingPrice != nil {
			billingPrice = instance.BillingPrice.InexactFloat64()
		}
		var avatarBadgeID any
		if row.User.AvatarBadgeID != nil {
			avatarBadgeID = *row.User.AvatarBadgeID
		}

		entry := gin.H{
			"id":      instance.ID,
			"incusId": instance.IncusID,
			"name":    instance.Name,
			"status":  string(instance.Status),
			"user": gin.H{
				"id":            row.User.ID,
				"username":      row.User.Username,
				"email":         row.User.Email,
				"avatarStyle":   row.User.AvatarStyle,
				"avatarBadgeId": avatarBadgeID,
				"balance":       row.User.Balance,
			},
			"host": gin.H{
				"id":           row.Host.ID,
				"name":         row.Host.Name,
				"userId":       row.Host.UserID,
				"instanceType": row.Host.InstanceType,
				"user":         gin.H{"role": row.Host.UserRole},
			},
			"package":           packageView,
			"packagePlan":       planView,
			"packagePlanId":     instance.PackagePlanID,
			"billingPrice":      billingPrice,
			"billingCycle":      instance.BillingCycle,
			"createdAt":         adminBillingISOTime(instance.CreatedAt),
			"remainingDays":     remainingDays,
			"instanceTypeLabel": instanceTypeLabel,
			"autoRenew":         instance.AutoRenew,
			"suspendReason":     instance.SuspendReason,
			"hasAffBinding":     row.HasAffBinding,
			"affDiscountRate":   affDiscountRate,
		}
		// `x?.toISOString()` drops the key when the value is null.
		if instance.ExpiresAt != nil {
			entry["expiresAt"] = adminBillingISOTime(*instance.ExpiresAt)
		}
		if instance.SuspendedAt != nil {
			entry["suspendedAt"] = adminBillingISOTime(*instance.SuspendedAt)
		}
		views = append(views, entry)
	}

	// Sort the rows themselves, then render: comparing hosts[i] while swapping
	// hostViews would sort by the wrong indices.
	sortedHosts := make([]db.AdminBillingHostRef, len(hosts))
	copy(sortedHosts, hosts)
	sort.SliceStable(sortedHosts, func(i, j int) bool {
		return adminBillingLocaleCompare(sortedHosts[i].Name, sortedHosts[j].Name) < 0
	})
	hostViews := make([]gin.H, 0, len(sortedHosts))
	for _, host := range sortedHosts {
		hostViews = append(hostViews, gin.H{"id": host.ID, "name": host.Name})
	}

	c.JSON(http.StatusOK, gin.H{
		"instances": views,
		"total":     total,
		"page":      pageNum,
		"pageSize":  size,
		"hosts":     hostViews,
	})
}

// ==================== GET /api/admin/billing/recharge-records ====================

// adminBillingRechargeRecordsHandler mirrors GET /api/admin/billing/recharge-records.
func adminBillingRechargeRecordsHandler(c *gin.Context) {
	ctx := c.Request.Context()

	pageNum := adminBillingParsePositiveInteger(c.Query("page"), 1, -1)
	size := adminBillingParsePositiveInteger(c.Query("pageSize"), 20, 100)

	options := db.RechargeRecordListOptions{
		Page:     pageNum,
		PageSize: size,
		Status:   models.RechargeStatus(adminBillingNormalizeStringFilter(c.Query("status"), adminBillingRechargeStatuses)),
	}
	if userID := adminBillingParseOptionalPositiveInteger(c.Query("userId"), c.Query("userId") != ""); userID != nil {
		options.UserID = *userID
	}

	result, err := db.GetAllRechargeRecords(ctx, options)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取充值记录列表失败"})
		return
	}

	userIDs := make([]int32, 0, len(result.Records))
	for _, record := range result.Records {
		userIDs = append(userIDs, record.UserID)
	}
	users := map[int32]db.AdminBillingUserRef{}
	if len(userIDs) > 0 {
		refs, err := db.GetAdminBillingUserRefs(ctx, userIDs)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "获取充值记录列表失败"})
			return
		}
		users = refs
	}

	views := make([]gin.H, 0, len(result.Records))
	for _, record := range result.Records {
		paymentInfo := lib.ExtractRechargePaymentDisplayInfo(record.PaymentDetails)
		amount := record.Amount.InexactFloat64()
		payableAmount := lib.GetRechargePayableAmount(lib.RechargePayableSource{
			Amount:         amount,
			PaymentDetails: record.PaymentDetails,
		})

		var actualAmount any
		if record.ActualAmount != nil {
			actualAmount = record.ActualAmount.InexactFloat64()
		}
		var user any
		if ref, ok := users[record.UserID]; ok {
			user = gin.H{"id": ref.ID, "username": ref.Username}
		}
		var provider any
		if record.Provider != nil {
			provider = gin.H{
				"id":          record.Provider.ID,
				"name":        record.Provider.Name,
				"displayName": record.Provider.Name,
				"type":        string(record.Provider.Type),
			}
		}

		entry := gin.H{
			"id":                       record.ID,
			"orderNo":                  record.OrderNo,
			"userId":                   record.UserID,
			"user":                     user,
			"amount":                   amount,
			"payableAmount":            payableAmount,
			"actualAmount":             actualAmount,
			"fee":                      record.Fee.InexactFloat64(),
			"status":                   string(record.Status),
			"payMethod":                record.PaymentMethod,
			"actualPaymentMethod":      paymentInfo.ActualPaymentMethod,
			"paymentCurrency":          paymentInfo.PaymentCurrency,
			"paymentNetwork":           paymentInfo.PaymentNetwork,
			"paymentUuid":              paymentInfo.PaymentUUID,
			"paymentTxid":              paymentInfo.PaymentTxID,
			"invoiceCurrency":          paymentInfo.InvoiceCurrency,
			"gatewayStatus":            paymentInfo.GatewayStatus,
			"gatewayStatusDescription": paymentInfo.GatewayStatusDescription,
			"provider":                 provider,
			"tradeNo":                  record.TradeNo,
			"createdAt":                adminBillingISOTime(record.CreatedAt),
			"paidAt":                   adminBillingISOTimePtr(record.CompletedAt),
			"completedAt":              adminBillingISOTimePtr(record.CompletedAt),
			"expiresAt":                adminBillingISOTimePtr(record.ExpiredAt),
		}
		views = append(views, entry)
	}

	c.JSON(http.StatusOK, gin.H{
		"records":  views,
		"total":    result.Total,
		"page":     result.Page,
		"pageSize": result.PageSize,
	})
}

// adminBillingInt64Ptr and adminBillingStringOrEmpty are tiny wire helpers.
func adminBillingInt64Ptr(value int64) *int64 { return &value }

func adminBillingStringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// adminBillingAudit writes the admin audit trail for a money mutation. The
// TypeScript only writes createLog; this adds lib.LogAdminAction (module "admin")
// so the audit metadata survives any future rewording of the human log line. It
// never changes an API response.
func adminBillingAudit(
	ctx context.Context,
	c *gin.Context,
	actor *lib.AccessTokenClaims,
	action, reason, resourceType string,
	resourceID int64,
	resourceName, detail string,
) {
	if actor == nil {
		return
	}
	metadata := map[string]any{}
	if detail != "" {
		metadata["detail"] = detail
	}
	lib.LogAdminAction(ctx, int64(actor.ID), action, lib.AuditLogDetails{
		IP:           middleware.ClientIP(c),
		UserAgent:    c.GetHeader("User-Agent"),
		ResourceType: resourceType,
		ResourceID:   adminBillingInt64Ptr(resourceID),
		ResourceName: resourceName,
		Reason:       reason,
		Metadata:     metadata,
	})
}

// ==================== POST /api/admin/instances/:id/suspend ====================

// adminBillingSuspendHandler mirrors POST /:id/suspend.
func adminBillingSuspendHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)

	instanceID, valid := adminBillingParseRouteID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的实例ID"})
		return
	}

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object := adminBillingBody(body)
	reason := "管理员封停"
	if raw, ok := adminBillingBodyString(object, "reason"); ok && raw != nil && *raw != "" {
		reason = *raw
	}

	loaded, err := db.GetAdminBillingInstanceContext(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "封停实例失败"})
		return
	}
	if loaded == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
		return
	}
	instance := loaded.Instance
	if instance.Status == models.InstanceStatusDeleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已删除"})
		return
	}
	if instance.Status == models.InstanceStatusSuspended {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已被封停"})
		return
	}

	// Stop the workload first, best effort: a node that is down must not block the
	// suspension, otherwise an abusive instance could never be suspended.
	if instance.Status == models.InstanceStatusRunning {
		adminBillingStopInstanceBestEffort(ctx, instance.HostID, instance.IncusID, false)
	}

	suspendedBy := actor.ID
	if err := db.SuspendInstance(ctx, instanceID, db.SuspendInstanceData{
		SuspendedBy:   &suspendedBy,
		SuspendReason: reason,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "封停实例失败"})
		return
	}

	db.CreateLog(ctx, adminBillingInt64Ptr(int64(actor.ID)), adminBillingLogModule, "instance.admin_suspend",
		fmt.Sprintf(`Admin suspended instance "%s" (User: %s): %s`, instance.Name, loaded.User.Username, reason),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	adminBillingAudit(ctx, c, actor, "instance.admin_suspend",
		fmt.Sprintf(`Admin suspended instance "%s": %s`, instance.Name, reason),
		"instance", int64(instanceID), instance.Name, reason)

	lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceSuspended, lib.EventData{
		"instanceName": instance.Name,
		"reason":       reason,
	})

	c.JSON(http.StatusOK, gin.H{"success": true, "message": "实例已封停"})
}

// ==================== POST /api/admin/instances/:id/unsuspend ====================

// adminBillingUnsuspendHandler mirrors POST /:id/unsuspend.
func adminBillingUnsuspendHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)

	instanceID, valid := adminBillingParseRouteID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的实例ID"})
		return
	}

	loaded, err := db.GetAdminBillingInstanceContext(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "解封实例失败"})
		return
	}
	if loaded == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
		return
	}
	instance := loaded.Instance
	if instance.Status != models.InstanceStatusSuspended {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例未被封停"})
		return
	}

	if err := db.UnsuspendInstance(ctx, instanceID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "解封实例失败"})
		return
	}

	db.CreateLog(ctx, adminBillingInt64Ptr(int64(actor.ID)), adminBillingLogModule, "instance.admin_unsuspend",
		fmt.Sprintf(`Admin unsuspended instance "%s" (User: %s)`, instance.Name, loaded.User.Username),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	adminBillingAudit(ctx, c, actor, "instance.admin_unsuspend",
		fmt.Sprintf(`Admin unsuspended instance "%s"`, instance.Name),
		"instance", int64(instanceID), instance.Name, "")

	lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceUnsuspended, lib.EventData{
		"instanceName": instance.Name,
	})

	c.JSON(http.StatusOK, gin.H{"success": true, "message": "实例已解封"})
}

// ==================== POST /api/admin/instances/:id/extend ====================

// adminBillingExtendHandler mirrors POST /:id/extend.
func adminBillingExtendHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)

	instanceID, valid := adminBillingParseRouteID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数无效，延期天数必须在 1-365 之间"})
		return
	}

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object := adminBillingBody(body)

	days, daysOK := adminBillingBodyInt(object, "days")
	if !daysOK || days < 1 || days > 365 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数无效，延期天数必须在 1-365 之间"})
		return
	}
	var reason *string
	if raw, ok := adminBillingBodyString(object, "reason"); ok {
		reason = raw
	}
	freeExtend := false
	if value, ok := adminBillingBodyBool(object, "freeExtend"); ok {
		freeExtend = value
	}
	reasonText := "手动延期"
	if reason != nil && strings.TrimSpace(*reason) != "" {
		reasonText = *reason
	}

	loaded, err := db.GetAdminBillingInstanceContext(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "延期实例失败"})
		return
	}
	if loaded == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
		return
	}
	instance := loaded.Instance
	if instance.Status == models.InstanceStatusDeleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已删除"})
		return
	}
	// Only paid instances can be extended.
	// Only paid instances can be extended. The TypeScript guard is
	// `!packagePlanId || !billingPrice`, and a Prisma Decimal("0") is still an
	// object (truthy), so a zero-priced paid instance takes the free-extend path
	// instead of being rejected.
	if instance.PackagePlanID == nil || instance.BillingPrice == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "免费实例无需延期"})
		return
	}

	billingCycle := int32(1)
	if instance.BillingCycle != nil {
		billingCycle = *instance.BillingCycle
	}
	dailyPrice := lib.CalculateDailyPrice(instance.BillingPrice.InexactFloat64(), int(billingCycle))
	extendAmount := lib.JSFixed(dailyPrice*float64(days), 2)
	if freeExtend {
		extendAmount = 0
	}

	now := time.Now().UTC()
	baseDate := now
	if instance.ExpiresAt != nil && instance.ExpiresAt.After(now) {
		baseDate = *instance.ExpiresAt
	}
	newExpiresAt := baseDate.Add(time.Duration(days) * 24 * time.Hour)

	if !freeExtend && extendAmount > 0 {
		userBalance := loaded.User.Balance
		if userBalance < extendAmount {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("用户余额不足，需要 %s 元，当前余额 %s 元",
					adminBillingFixed2(extendAmount), adminBillingFixed2(userBalance)),
			})
			return
		}
	}

	paid := !freeExtend && extendAmount > 0
	balanceRemark := fmt.Sprintf("管理员延期 %d 天：%s", days, reasonText)
	storedRemark := fmt.Sprintf("管理员延期 %d 天（操作者: %s）", days, actor.Username)
	if !paid {
		storedRemark = fmt.Sprintf("管理员免费延期 %d 天（操作者: %s）：%s", days, actor.Username, reasonText)
	}

	idempotent := false
	var balanceLogID *int32
	err = db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if paid {
			// Serialise with every other balance mutation for this user.
			locked, lockErr := db.TryAdvisoryTransactionLockTx(tx, db.UserBalanceLockNamespace, instance.UserID)
			if lockErr != nil {
				return lockErr
			}
			if !locked {
				return errAdminBillingBalanceConflict
			}
		}

		// Exact-replay guard: nothing in the TypeScript flow is idempotent, so a
		// double submit would charge and extend twice.
		replay, replayErr := adminBillingFindReplayRecord(ctx, tx, instanceID, "renew", extendAmount, storedRemark)
		if replayErr != nil {
			return replayErr
		}
		if replay != nil {
			idempotent = true
			return nil
		}

		if paid {
			balance, err := db.InstanceBillingApplyBalanceTx(ctx, tx, db.InstanceBillingBalanceChange{
				UserID:     instance.UserID,
				InstanceID: instanceID,
				Delta:      adminBillingDecimal(-extendAmount),
				LogType:    models.BalanceLogTypeConsume,
				Remark:     balanceRemark,
			})
			if err != nil {
				return err
			}
			balanceLogID = &balance.BalanceLogID
		}

		updates := map[string]any{
			"expires_at": newExpiresAt,
			"updated_at": now,
		}
		// An expiry suspension is lifted by the extension itself.
		if instance.Status == models.InstanceStatusSuspended &&
			instance.SuspendReason != nil && *instance.SuspendReason == "expired" {
			updates["status"] = string(models.InstanceStatusStopped)
			updates["suspended_at"] = nil
			updates["suspended_by"] = nil
			updates["suspend_reason"] = nil
		}
		if err := tx.WithContext(ctx).Model(&models.Instance{}).
			Where("id = ?", instanceID).Updates(updates).Error; err != nil {
			return err
		}

		record := models.InstanceBillingRecord{
			InstanceID:   instanceID,
			UserID:       instance.UserID,
			Type:         models.BillingRecordTypeRenew,
			Amount:       adminBillingDecimal(extendAmount),
			Months:       0,
			PeriodStart:  baseDate,
			PeriodEnd:    newExpiresAt,
			BalanceLogID: balanceLogID,
			Remark:       &storedRemark,
		}
		return tx.WithContext(ctx).Create(&record).Error
	})
	if err != nil {
		switch {
		case errors.Is(err, errAdminBillingBalanceConflict):
			c.JSON(http.StatusConflict, gin.H{"error": "用户余额正在处理，请稍后重试"})
		case adminBillingIsBalanceInsufficient(err):
			c.JSON(http.StatusBadRequest, gin.H{"error": "用户余额不足"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "延期实例失败"})
		}
		return
	}

	if idempotent {
		// Replay: the first call already charged and extended; report the same
		// payload without writing a second ledger row or notification.
		c.JSON(http.StatusOK, gin.H{
			"success":      true,
			"message":      fmt.Sprintf("实例已延期 %d 天", days),
			"amount":       extendAmount,
			"newExpiresAt": adminBillingISOTime(newExpiresAt),
		})
		return
	}

	db.CreateLog(ctx, adminBillingInt64Ptr(int64(actor.ID)), adminBillingLogModule, "instance.admin_extend",
		fmt.Sprintf(`Admin extended instance "%s" by %d days (User: %s, Amount: %s)`,
			instance.Name, days, loaded.User.Username, adminBillingFixed2(extendAmount)),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	adminBillingAudit(ctx, c, actor, "instance.admin_extend",
		fmt.Sprintf(`Admin extended instance "%s" by %d days`, instance.Name, days),
		"instance", int64(instanceID), instance.Name,
		fmt.Sprintf("days=%d amount=%s free=%t", days, adminBillingFixed2(extendAmount), freeExtend))

	lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceExtended, lib.EventData{
		"instanceName": instance.Name,
		"days":         float64(days),
		"newExpiresAt": adminBillingISOTime(newExpiresAt),
		"amount":       extendAmount,
	})

	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"message":      fmt.Sprintf("实例已延期 %d 天", days),
		"amount":       extendAmount,
		"newExpiresAt": adminBillingISOTime(newExpiresAt),
	})
}

// ==================== POST /api/admin/instances/:id/refund ====================

// adminBillingRefundHandler mirrors POST /:id/refund.
func adminBillingRefundHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)

	instanceID, valid := adminBillingParseRouteID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的实例ID"})
		return
	}

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object := adminBillingBody(body)

	amount, ok := adminBillingBodyNumber(object, "amount")
	if !ok || amount <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "退款金额必须大于0"})
		return
	}
	reasonRaw, ok := adminBillingBodyString(object, "reason")
	if !ok || reasonRaw == nil || len(strings.TrimSpace(*reasonRaw)) < 2 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写退款原因"})
		return
	}
	reason := *reasonRaw

	loaded, err := db.GetAdminBillingInstanceContext(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "退款失败"})
		return
	}
	if loaded == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
		return
	}
	instance := loaded.Instance

	maxRefundable, err := db.GetInstanceBillingMaxRefundable(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "退款失败"})
		return
	}
	if amount > maxRefundable {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("退款金额超过可退上限，最多可退 %s 元", adminBillingFixed2(maxRefundable)),
		})
		return
	}

	recordRemark := fmt.Sprintf("管理员退款：%s（操作者: %s）", reason, actor.Username)
	balanceRemark := "管理员退款：" + reason

	idempotent := false
	err = db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, lockErr := db.TryAdvisoryTransactionLockTx(tx, db.UserBalanceLockNamespace, instance.UserID)
		if lockErr != nil {
			return lockErr
		}
		if !locked {
			return errAdminBillingBalanceConflict
		}

		replay, replayErr := adminBillingFindReplayRecord(ctx, tx, instanceID, "refund", -amount, recordRemark)
		if replayErr != nil {
			return replayErr
		}
		if replay != nil {
			// Already applied: never credit twice.
			idempotent = true
			return nil
		}

		// Re-check the paid-amount cap inside the transaction, under the lock. The
		// TypeScript only checks it before the transaction, so two concurrent
		// partial refunds can each pass and together exceed what was paid
		// (docs/WAVE_D_SPEC §5 "退款必须有上限（已付金额）"). Sequential requests are
		// unaffected: the pre-transaction check already produced the same answer.
		currentCap, capErr := db.GetInstanceBillingMaxRefundable(ctx, instanceID)
		if capErr != nil {
			return capErr
		}
		if amount > currentCap {
			return &adminBillingRefundCapError{MaxRefundable: currentCap}
		}

		balance, err := db.InstanceBillingApplyBalanceTx(ctx, tx, db.InstanceBillingBalanceChange{
			UserID:     instance.UserID,
			InstanceID: instanceID,
			Delta:      adminBillingDecimal(amount),
			LogType:    models.BalanceLogTypeRefund,
			Remark:     balanceRemark,
		})
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		balanceLogID := balance.BalanceLogID
		record := models.InstanceBillingRecord{
			InstanceID:   instanceID,
			UserID:       instance.UserID,
			Type:         models.BillingRecordTypeRefund,
			Amount:       adminBillingDecimal(-amount),
			Months:       0,
			PeriodStart:  now,
			PeriodEnd:    now,
			BalanceLogID: &balanceLogID,
			Remark:       &recordRemark,
		}
		if err := tx.WithContext(ctx).Create(&record).Error; err != nil {
			return err
		}

		return InstanceDestroyReverseAffCommissionForRefund(ctx, tx, instanceID, amount,
			"管理员退款返利冲正："+instance.Name)
	})
	if err != nil {
		var capError *adminBillingRefundCapError
		switch {
		case errors.As(err, &capError):
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("退款金额超过可退上限，最多可退 %s 元", adminBillingFixed2(capError.MaxRefundable)),
			})
		case errors.Is(err, errAdminBillingBalanceConflict):
			c.JSON(http.StatusConflict, gin.H{"error": "用户余额正在处理，请稍后重试"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "退款失败"})
		}
		return
	}

	db.CreateLog(ctx, adminBillingInt64Ptr(int64(actor.ID)), adminBillingLogModule, "instance.admin_refund",
		fmt.Sprintf(`Admin refunded %s for instance "%s" (User: %s): %s`,
			adminBillingFixed2(amount), instance.Name, loaded.User.Username, reason),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	adminBillingAudit(ctx, c, actor, "instance.admin_refund",
		fmt.Sprintf(`Admin refunded %s for instance "%s": %s`, adminBillingFixed2(amount), instance.Name, reason),
		"instance", int64(instanceID), instance.Name, reason)

	lib.SendNotification(ctx, int64(instance.UserID), lib.EventRefundCompleted, lib.EventData{
		"instanceName": instance.Name,
		"amount":       amount,
		"reason":       reason,
	})

	remaining := maxRefundable - amount
	if idempotent {
		// The first call already reduced the cap; recompute so a replay reports the
		// same remaining budget instead of subtracting twice.
		if current, err := db.GetInstanceBillingMaxRefundable(ctx, instanceID); err == nil {
			remaining = current
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success":       true,
		"message":       fmt.Sprintf("已退款 %s 元", adminBillingJSNumberString(amount)),
		"amount":        amount,
		"maxRefundable": remaining,
	})
}

// ==================== POST /api/admin/instances/:id/delete-and-refund ====================

// adminBillingClaimInstanceForAdminDelete mirrors claimInstanceForAdminDelete: the
// 4106 lock plus a `creating|running|… → deleted` compare-and-set is the
// idempotency barrier for the destructive path (a replay finds 'deleted' and 400s
// before touching money).
func adminBillingClaimInstanceForAdminDelete(
	ctx context.Context, instanceID int32, currentStatus models.InstanceStatus,
) (bool, error) {
	claimed := false
	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		locked, err := db.TryAdvisoryTransactionLockTx(tx, db.InstanceOperationLockNamespace, instanceID)
		if err != nil {
			return err
		}
		if !locked {
			return nil
		}

		var activeTasks int64
		if err := tx.WithContext(ctx).Raw(
			`SELECT (SELECT COUNT(*) FROM restore_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING'))
			      + (SELECT COUNT(*) FROM backup_upload_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING'))
			      + (SELECT COUNT(*) FROM instance_tasks WHERE instance_id = ? AND status IN ('PENDING','PROCESSING')) AS total`,
			instanceID, instanceID, instanceID,
		).Scan(&activeTasks).Error; err != nil {
			return err
		}
		if activeTasks > 0 {
			return nil
		}

		result := tx.WithContext(ctx).Exec(
			`UPDATE instances SET status = 'deleted', updated_at = ? WHERE id = ? AND status = ?`,
			time.Now().UTC(), instanceID, string(currentStatus),
		)
		if result.Error != nil {
			return result.Error
		}
		claimed = result.RowsAffected == 1
		return nil
	})
	return claimed, err
}

// adminBillingDeleteAndRefundHandler mirrors POST /:id/delete-and-refund.
func adminBillingDeleteAndRefundHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)

	instanceID, valid := adminBillingParseRouteID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的实例ID"})
		return
	}

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object := adminBillingBody(body)

	reasonRaw, _ := adminBillingBodyString(object, "reason")
	if reasonRaw == nil || len(strings.TrimSpace(*reasonRaw)) < 2 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写删除原因"})
		return
	}
	reason := *reasonRaw

	refundTypeRaw, _ := adminBillingBodyString(object, "refundType")
	if refundTypeRaw == nil || (*refundTypeRaw != "remaining" && *refundTypeRaw != "full") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择退款方式"})
		return
	}
	refundType := *refundTypeRaw
	databaseOnly := false
	if value, ok := adminBillingBodyBool(object, "databaseOnly"); ok {
		databaseOnly = value
	}

	loaded, err := db.GetAdminBillingInstanceContext(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除并退款失败"})
		return
	}
	if loaded == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
		return
	}
	instance := loaded.Instance
	if instance.Status == models.InstanceStatusDeleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已删除"})
		return
	}

	hostName := ""
	if host, err := db.HostsGetByID(ctx, instance.HostID); err == nil && host != nil {
		hostName = host.Name
	}

	maxRefundable, err := db.GetInstanceBillingMaxRefundable(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除并退款失败"})
		return
	}

	refundAmount := 0.0
	refundLabel := "按剩余价值"
	if refundType == "full" {
		refundAmount = maxRefundable
		refundLabel = "全额"
	} else {
		quote, err := adminBillingRefundQuote(ctx, &instance)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除并退款失败"})
			return
		}
		refundAmount = quote.Refundable
	}

	claimed, err := adminBillingClaimInstanceForAdminDelete(ctx, instanceID, instance.Status)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除并退款失败"})
		return
	}
	if !claimed {
		c.JSON(http.StatusConflict, gin.H{"error": "实例正在删除或已删除"})
		return
	}

	// Settle the books BEFORE deleting anything from the node: if the ledger write
	// fails the instance is restored, so a deleted instance can never be unpaid.
	if refundAmount > 0 {
		balanceRemark := fmt.Sprintf("管理员删除并退款（%s）：%s", refundLabel, reason)
		recordRemark := fmt.Sprintf("管理员删除并退款（%s）：%s（操作者: %s）", refundLabel, reason, actor.Username)
		settleErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			locked, lockErr := db.TryAdvisoryTransactionLockTx(tx, db.UserBalanceLockNamespace, instance.UserID)
			if lockErr != nil {
				return lockErr
			}
			if !locked {
				return errAdminBillingBalanceConflict
			}

			balance, err := db.InstanceBillingApplyBalanceTx(ctx, tx, db.InstanceBillingBalanceChange{
				UserID:     instance.UserID,
				InstanceID: instanceID,
				Delta:      adminBillingDecimal(refundAmount),
				LogType:    models.BalanceLogTypeRefund,
				Remark:     balanceRemark,
			})
			if err != nil {
				return err
			}

			now := time.Now().UTC()
			balanceLogID := balance.BalanceLogID
			record := models.InstanceBillingRecord{
				InstanceID:   instanceID,
				UserID:       instance.UserID,
				Type:         models.BillingRecordTypeRefund,
				Amount:       adminBillingDecimal(-refundAmount),
				Months:       0,
				PeriodStart:  now,
				PeriodEnd:    now,
				BalanceLogID: &balanceLogID,
				Remark:       &recordRemark,
			}
			if err := tx.WithContext(ctx).Create(&record).Error; err != nil {
				return err
			}

			return InstanceDestroyReverseAffCommissionForRefund(ctx, tx, instanceID, refundAmount,
				"管理员删除并退款返利冲正："+instance.Name)
		})
		if settleErr != nil {
			// Undo the claim so the operator can retry.
			_ = db.DB.WithContext(ctx).Exec(
				`UPDATE instances SET status = ?, updated_at = ? WHERE id = ? AND status = 'deleted'`,
				string(instance.Status), time.Now().UTC(), instanceID,
			).Error
			if errors.Is(settleErr, errAdminBillingBalanceConflict) {
				c.JSON(http.StatusConflict, gin.H{"error": "用户余额正在处理，请稍后重试"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "删除并退款失败"})
			return
		}
	}

	// Stop the workload (best effort, skipped on the database-only path).
	if instance.Status == models.InstanceStatusRunning && !databaseOnly {
		adminBillingStopInstanceBestEffort(ctx, instance.HostID, instance.IncusID, true)
	}

	// Related rows are best effort (the TS wraps them in try/catch), but the host
	// counters are released unconditionally afterwards, exactly like the original:
	// the port count is read before the cleanup, rollbackResources always runs, and
	// the host is then recomputed from the surviving instances.
	var portMappingsCount int64
	_ = db.DB.WithContext(ctx).Raw(
		`SELECT COUNT(*)::bigint AS total FROM port_mappings WHERE instance_id = ?`, instanceID,
	).Scan(&portMappingsCount).Error
	_, _ = instanceDestroyCleanupRelated(ctx, instanceID, nil)
	_ = instanceDestroyRollbackHostResources(ctx, &instance, portMappingsCount)
	_ = instanceDestroyRefreshHostResources(ctx, instance.HostID)

	if !databaseOnly {
		if host, err := db.HostsGetByID(ctx, instance.HostID); err == nil && host != nil {
			if client, err := instancesIncusClient(ctx, host); err == nil {
				_ = instancesIncusDelete(ctx, client, instance.IncusID)
			}
		}
	}

	logAction := "instance.admin_delete_refund"
	logPrefix := ""
	if databaseOnly {
		logAction = "instance.admin_delete_refund_db_only"
		logPrefix = " (database only)"
	}
	db.CreateLog(ctx, adminBillingInt64Ptr(int64(actor.ID)), adminBillingLogModule, logAction,
		fmt.Sprintf(`Admin deleted%s and refunded instance "%s" (User: %s, RefundType: %s, Amount: ¥%s): %s`,
			logPrefix, instance.Name, loaded.User.Username, refundType, adminBillingFixed2(refundAmount), reason),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	adminBillingAudit(ctx, c, actor, logAction,
		fmt.Sprintf(`Admin deleted%s and refunded instance "%s": %s`,
			logPrefix, instance.Name, reason),
		"instance", int64(instanceID), instance.Name,
		fmt.Sprintf("refundType=%s refundAmount=%s databaseOnly=%t",
			refundType, adminBillingFixed2(refundAmount), databaseOnly))

	refundLabelText := "按剩余价值退款"
	if refundType == "full" {
		refundLabelText = "全额退款"
	}
	lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstanceDeletedRefunded, lib.EventData{
		"instanceName": instance.Name,
		"refundAmount": refundAmount,
		"refundType":   refundLabelText,
		"reason":       reason,
	})

	if loaded.User.Email != nil && *loaded.User.Email != "" {
		lib.SendInstanceDestroyRefundEmail(ctx, *loaded.User.Email, lib.InstanceDestroyRefundEmailData{
			Username:      loaded.User.Username,
			InstanceName:  instance.Name,
			RefundAmount:  refundAmount,
			RefundType:    refundLabelText,
			Reason:        reason,
			IsUserDestroy: false,
		})
	}

	modeText := ""
	if databaseOnly {
		modeText = "（仅数据库）"
	}
	responseMessage := "实例已删除" + modeText
	if refundAmount > 0 {
		responseMessage += "，已退款 ¥" + adminBillingFixed2(refundAmount)
	}
	if hostName == "" {
		_ = hostName
	}

	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"message":      responseMessage,
		"refundAmount": refundAmount,
		"refundType":   refundType,
	})
}

// ==================== POST /api/admin/instances/:id/apply-aff ====================

// adminBillingApplyAffHandler mirrors POST /:id/apply-aff.
func adminBillingApplyAffHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)

	instanceID, valid := adminBillingParseRouteID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的实例ID"})
		return
	}

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object := adminBillingBody(body)
	affCodeInput, _ := adminBillingBodyString(object, "affCode")
	if affCodeInput == nil || len(strings.TrimSpace(*affCodeInput)) < 3 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请输入有效的AFF优惠码"})
		return
	}

	loaded, err := db.GetAdminBillingInstanceContext(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "应用AFF优惠码失败"})
		return
	}
	if loaded == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
		return
	}
	instance := loaded.Instance
	if instance.Status == models.InstanceStatusDeleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已删除"})
		return
	}
	if instance.PackagePlanID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "免费实例不支持应用优惠码"})
		return
	}

	existing, err := db.AffGetInstanceBinding(ctx, instanceID, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "应用AFF优惠码失败"})
		return
	}
	if existing != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该实例已绑定优惠码，无法重复应用"})
		return
	}

	code := strings.ToUpper(strings.TrimSpace(*affCodeInput))
	var affCode models.AffCode
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT `+adminBillingAffCodeColumns+` FROM aff_codes WHERE code = ? LIMIT 1`, code,
	).Scan(&affCode).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "应用AFF优惠码失败"})
		return
	}
	if affCode.ID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "优惠码不存在"})
		return
	}
	if affCode.UserID == instance.UserID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能为用户应用其自己的优惠码"})
		return
	}

	var ownerUsername string
	if owner, err := db.FindUserById(ctx, affCode.UserID); err == nil && owner != nil {
		ownerUsername = owner.Username
	}

	if err := db.AffCreateBinding(ctx, instanceID, affCode.ID, nil); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "应用AFF优惠码失败"})
		return
	}

	db.CreateLog(ctx, adminBillingInt64Ptr(int64(actor.ID)), adminBillingLogModule, "instance.admin_apply_aff",
		fmt.Sprintf(`Admin applied AFF code "%s" (Owner: %s) to instance "%s" (User: %s)`,
			affCode.Code, ownerUsername, instance.Name, loaded.User.Username),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	adminBillingAudit(ctx, c, actor, "instance.admin_apply_aff",
		fmt.Sprintf(`Admin applied AFF code "%s" to instance "%s"`, affCode.Code, instance.Name),
		"instance", int64(instanceID), instance.Name, "affCode="+affCode.Code)

	discountRate := affCode.DiscountRate.InexactFloat64()
	discountPercent := int64(math.Round(discountRate * 100))
	lib.SendNotification(ctx, int64(instance.UserID), lib.EventAffApplied, lib.EventData{
		"instanceName":    instance.Name,
		"discountPercent": float64(discountPercent),
	})

	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"message":      fmt.Sprintf("已为实例应用 %d%% 续费折扣", discountPercent),
		"discountRate": discountRate,
	})
}

// adminBillingAffCodeColumns mirrors affCodeColumns in db/aff.go for the one
// direct lookup this route needs.
const adminBillingAffCodeColumns = `id, code, user_id, package_plan_id, discount_rate, commission_rate,
	enabled, used_count, total_earnings, created_at`

// ==================== shared small helpers ====================

// adminBillingRefundCapError is the in-transaction cap rejection (the same 400
// message the pre-transaction check produces).
type adminBillingRefundCapError struct {
	MaxRefundable float64
}

func (e *adminBillingRefundCapError) Error() string {
	return fmt.Sprintf("退款金额超过可退上限，最多可退 %s 元", adminBillingFixed2(e.MaxRefundable))
}

// errAdminBillingBalanceConflict mirrors the BALANCE_CONFLICT sentinel that the
// TypeScript throws when the 4110 advisory lock is already held.
var errAdminBillingBalanceConflict = errors.New("BALANCE_CONFLICT: 用户余额正在处理，请稍后重试")

// adminBillingIsBalanceInsufficient reports the "余额不足" family of errors from
// db.InstanceBillingApplyBalanceTx (which returns strings, not sentinels, so the
// admin routes can keep the TypeScript's message-based mapping).
func adminBillingIsBalanceInsufficient(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "余额不足")
}

// adminBillingFindReplayRecord looks for an identical billing record written inside
// the replay window. `amount` is compared against the stored amount (positive for
// renew, negative for refund).
func adminBillingFindReplayRecord(
	ctx context.Context, tx *gorm.DB, instanceID int32, recordType string, amount float64, remark string,
) (*models.InstanceBillingRecord, error) {
	since := time.Now().UTC().Add(-adminBillingReplayWindowSeconds * time.Second)
	var rows []models.InstanceBillingRecord
	if err := tx.WithContext(ctx).
		Where("instance_id = ? AND type = ? AND amount = ? AND remark = ? AND created_at >= ?",
			instanceID, recordType, adminBillingDecimal(amount), remark, since).
		Order("id DESC").Limit(1).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// adminBillingStopInstanceBestEffort stops an instance on its node, ignoring every
// failure (a down node must not block an admin action).
func adminBillingStopInstanceBestEffort(ctx context.Context, hostID int32, incusID string, force bool) {
	host, err := db.HostsGetByID(ctx, hostID)
	if err != nil || host == nil {
		return
	}
	client, err := instancesIncusClient(ctx, host)
	if err != nil {
		return
	}
	_ = instancesIncusAction(ctx, client, incusID, "stop", force)
}

// ==================== POST /api/admin/instances/:id/update-price ====================

// adminBillingUpdatePriceHandler mirrors POST /:id/update-price.
func adminBillingUpdatePriceHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	if _, ok := body.Value.(map[string]any); !ok {
		// `!request.body || typeof request.body !== 'object'` — the body-shape check
		// runs before the route id is parsed, exactly like the TypeScript.
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数无效"})
		return
	}
	object := adminBillingBody(body)

	instanceID, valid := adminBillingParseRouteID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的实例ID"})
		return
	}

	newPrice, newPriceOK := adminBillingBodyNumber(object, "newPrice")
	if !newPriceOK || newPrice < 0 || newPrice > 100000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "价格必须是 0-100000 之间的数字"})
		return
	}
	settleBalance := false
	if value, ok := adminBillingBodyBool(object, "settleBalance"); ok {
		settleBalance = value
	}
	var expectedVersion *int
	if raw, present := object["expectedVersion"]; present && raw != nil {
		version, ok := adminBillingBodyInt(object, "expectedVersion")
		if !ok || version < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "预览版本参数无效"})
			return
		}
		expectedVersion = &version
	}

	loaded, err := db.GetAdminBillingInstanceContext(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "修改实例价格失败"})
		return
	}
	if loaded == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
		return
	}
	instance := loaded.Instance
	if instance.Status == models.InstanceStatusDeleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已删除"})
		return
	}
	if instance.PackagePlanID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "免费实例不支持修改价格"})
		return
	}

	oldPrice := 0.0
	if instance.BillingPrice != nil {
		oldPrice = instance.BillingPrice.InexactFloat64()
	}
	roundedNewPrice := adminBillingRoundMoney(newPrice)

	if expectedVersion != nil && instance.Version != int32(*expectedVersion) {
		c.JSON(http.StatusConflict, gin.H{"error": "实例计费信息已变化，请刷新后重试"})
		return
	}

	if oldPrice == roundedNewPrice {
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "价格未变更", "priceDiff": 0.0})
		return
	}

	var priceDiff, remainingDays float64
	billingCycle := int32(1)
	if instance.BillingCycle != nil {
		billingCycle = *instance.BillingCycle
	}
	comparisonVersion := instance.Version
	if expectedVersion != nil {
		comparisonVersion = int32(*expectedVersion)
	}

	txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var currentRows []models.Instance
		if err := tx.WithContext(ctx).Where("id = ?", instanceID).Limit(1).Find(&currentRows).Error; err != nil {
			return err
		}
		if len(currentRows) == 0 {
			return &adminBillingBatchPriceError{Message: "实例不存在", StatusCode: http.StatusNotFound}
		}
		currentInstance := currentRows[0]
		if currentInstance.Status == models.InstanceStatusDeleted {
			return &adminBillingBatchPriceError{Message: "实例已删除", StatusCode: http.StatusBadRequest}
		}
		if currentInstance.PackagePlanID == nil {
			return &adminBillingBatchPriceError{Message: "免费实例不支持修改价格", StatusCode: http.StatusBadRequest}
		}

		currentOldPrice := 0.0
		if currentInstance.BillingPrice != nil {
			currentOldPrice = currentInstance.BillingPrice.InexactFloat64()
		}
		if currentOldPrice != oldPrice || currentInstance.Version != comparisonVersion {
			return &adminBillingBatchPriceError{Message: "实例计费信息已变化，请刷新后重试", StatusCode: http.StatusConflict}
		}

		quote, err := adminBillingPriceAdjustmentQuoteFor(ctx, &currentInstance, roundedNewPrice, settleBalance)
		if err != nil {
			return err
		}
		priceDiff = quote.PriceDiff
		remainingDays = quote.RemainingDays
		billingCycle = quote.BillingCycle

		userBalance := loaded.User.Balance
		if priceDiff > 0 && priceDiff > userBalance {
			return &adminBillingBatchPriceError{
				Message: fmt.Sprintf("用户余额不足，需补交 ¥%s，当前余额 ¥%s",
					adminBillingFixed2(priceDiff), adminBillingFixed2(userBalance)),
				StatusCode: http.StatusBadRequest,
			}
		}

		now := time.Now().UTC()
		update := tx.WithContext(ctx).Exec(
			`UPDATE instances SET billing_price = ?, version = version + 1, updated_at = ?
			  WHERE id = ? AND version = ?`,
			adminBillingDecimal(roundedNewPrice), now, instanceID, currentInstance.Version,
		)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return &adminBillingBatchPriceError{Message: "实例计费信息已变化，请刷新后重试", StatusCode: http.StatusConflict}
		}

		if !settleBalance || priceDiff == 0 {
			return nil
		}

		locked, lockErr := db.TryAdvisoryTransactionLockTx(tx, db.UserBalanceLockNamespace, currentInstance.UserID)
		if lockErr != nil {
			return lockErr
		}
		if !locked {
			return &adminBillingBatchPriceError{Message: "用户余额正在处理，请稍后重试", StatusCode: http.StatusConflict}
		}

		var balanceResult db.InstanceBillingBalanceResult
		if priceDiff > 0 {
			result, err := db.InstanceBillingApplyBalanceTx(ctx, tx, db.InstanceBillingBalanceChange{
				UserID:     currentInstance.UserID,
				InstanceID: instanceID,
				Delta:      adminBillingDecimal(-priceDiff),
				LogType:    models.BalanceLogTypeConsume,
				Remark:     fmt.Sprintf("管理员调整实例价格，补交差价 ¥%s", adminBillingFixed2(priceDiff)),
			})
			if err != nil {
				return &adminBillingBatchPriceError{
					Message:    fmt.Sprintf("用户余额不足，需补交 ¥%s", adminBillingFixed2(priceDiff)),
					StatusCode: http.StatusBadRequest,
				}
			}
			balanceResult = result
		} else {
			result, err := db.InstanceBillingApplyBalanceTx(ctx, tx, db.InstanceBillingBalanceChange{
				UserID:     currentInstance.UserID,
				InstanceID: instanceID,
				Delta:      adminBillingDecimal(math.Abs(priceDiff)),
				LogType:    models.BalanceLogTypeRefund,
				Remark:     fmt.Sprintf("管理员调整实例价格，退还差价 ¥%s", adminBillingFixed2(math.Abs(priceDiff))),
			})
			if err != nil {
				return err
			}
			balanceResult = result
		}

		recordType := models.BillingRecordTypeUpgrade
		recordAmount := math.Abs(priceDiff)
		if priceDiff < 0 {
			recordType = models.BillingRecordTypeRefund
			recordAmount = -math.Abs(priceDiff)
		}
		periodEnd := now
		if currentInstance.ExpiresAt != nil {
			periodEnd = *currentInstance.ExpiresAt
		}
		balanceLogID := balanceResult.BalanceLogID
		remark := fmt.Sprintf("价格调整: ¥%s → ¥%s",
			adminBillingFixed2(oldPrice), adminBillingFixed2(roundedNewPrice))
		record := models.InstanceBillingRecord{
			InstanceID:   instanceID,
			UserID:       currentInstance.UserID,
			Type:         recordType,
			Amount:       adminBillingDecimal(recordAmount),
			Months:       0,
			PeriodStart:  now,
			PeriodEnd:    periodEnd,
			BalanceLogID: &balanceLogID,
			Remark:       &remark,
		}
		return tx.WithContext(ctx).Create(&record).Error
	})
	if txErr != nil {
		var priceError *adminBillingBatchPriceError
		if errors.As(txErr, &priceError) {
			c.JSON(priceError.StatusCode, gin.H{"error": priceError.Message})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "修改实例价格失败"})
		return
	}

	db.CreateLog(ctx, adminBillingInt64Ptr(int64(actor.ID)), adminBillingLogModule, "instance.admin_update_price",
		fmt.Sprintf(`Admin updated instance "%s" price: ¥%s → ¥%s, settle: %t, diff: ¥%s`,
			instance.Name, adminBillingFixed2(oldPrice), adminBillingFixed2(roundedNewPrice),
			settleBalance, adminBillingFixed2(priceDiff)),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	adminBillingAudit(ctx, c, actor, "instance.admin_update_price",
		fmt.Sprintf(`Admin updated instance "%s" price: ¥%s → ¥%s`,
			instance.Name, adminBillingFixed2(oldPrice), adminBillingFixed2(roundedNewPrice)),
		"instance", int64(instanceID), instance.Name,
		fmt.Sprintf("settle=%t diff=%s", settleBalance, adminBillingFixed2(priceDiff)))

	if settleBalance && priceDiff != 0 {
		lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstancePriceChanged, lib.EventData{
			"instanceName": instance.Name,
			"oldPrice":     oldPrice,
			"newPrice":     roundedNewPrice,
			"priceDiff":    math.Abs(priceDiff),
			"isIncrease":   priceDiff > 0,
		})
	}

	responseMessage := "价格已更新"
	if priceDiff > 0 {
		responseMessage = fmt.Sprintf("价格已更新，已从用户余额扣除 ¥%s", adminBillingFixed2(priceDiff))
	} else if priceDiff < 0 {
		responseMessage = fmt.Sprintf("价格已更新，已退还用户 ¥%s", adminBillingFixed2(math.Abs(priceDiff)))
	}

	c.JSON(http.StatusOK, gin.H{
		"success":       true,
		"message":       responseMessage,
		"oldPrice":      oldPrice,
		"newPrice":      roundedNewPrice,
		"priceDiff":     priceDiff,
		"remainingDays": remainingDays,
		"billingCycle":  billingCycle,
	})
}

// ==================== POST /api/admin/instances/:id/update-price/preview ====================

// adminBillingUpdatePricePreviewHandler mirrors POST /:id/update-price/preview.
func adminBillingUpdatePricePreviewHandler(c *gin.Context) {
	ctx := c.Request.Context()

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	if _, ok := body.Value.(map[string]any); !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求参数无效"})
		return
	}
	object := adminBillingBody(body)

	instanceID, valid := adminBillingParseRouteID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的实例ID"})
		return
	}

	newPrice, ok := adminBillingBodyNumber(object, "newPrice")
	if !ok || newPrice < 0 || newPrice > 100000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "价格必须是 0-100000 之间的数字"})
		return
	}
	settleBalance := false
	if value, ok := adminBillingBodyBool(object, "settleBalance"); ok {
		settleBalance = value
	}

	loaded, err := db.GetAdminBillingInstanceContext(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "预览修改实例价格失败"})
		return
	}
	if loaded == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
		return
	}
	instance := loaded.Instance
	if instance.Status == models.InstanceStatusDeleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已删除"})
		return
	}
	if instance.PackagePlanID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "免费实例不支持修改价格"})
		return
	}

	quote, err := adminBillingPriceAdjustmentQuoteFor(ctx, &instance, newPrice, settleBalance)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "预览修改实例价格失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"oldPrice":        quote.OldPrice,
		"newPrice":        quote.NewPrice,
		"billingCycle":    quote.BillingCycle,
		"remainingDays":   quote.RemainingDays,
		"priceDiff":       quote.PriceDiff,
		"discountRate":    quote.DiscountRate,
		"userBalance":     loaded.User.Balance,
		"instanceVersion": instance.Version,
	})
}

// ==================== POST /api/admin/instances/batch-update-price/preview ====================

// adminBillingBatchUpdatePricePreviewHandler mirrors
// POST /batch-update-price/preview.
func adminBillingBatchUpdatePricePreviewHandler(c *gin.Context) {
	ctx := c.Request.Context()

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	request, err := adminBillingParseBatchPriceUpdateBody(adminBillingBody(body))
	if err != nil {
		var priceError *adminBillingBatchPriceError
		if errors.As(err, &priceError) {
			payload := gin.H{"error": priceError.Message}
			if priceError.Preview != nil {
				payload["preview"] = adminBillingSerializeBatchPreview(priceError.Preview)
			}
			c.JSON(priceError.StatusCode, payload)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "批量预览修改实例价格失败"})
		return
	}

	preview, err := adminBillingBuildBatchPreview(ctx, request.InstanceIDs, request.NewPrice, request.SettleBalance)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "批量预览修改实例价格失败"})
		return
	}
	c.JSON(http.StatusOK, adminBillingSerializeBatchPreview(preview))
}

// ==================== POST /api/admin/instances/batch-update-price ====================

// adminBillingBatchUpdatePriceHandler mirrors POST /batch-update-price.
//
// Atomicity: the preview, every version bump and every balance settlement happen in
// ONE transaction, so a failure on the tenth instance rolls back the first nine
// (the TypeScript does exactly the same; it is NOT a per-instance commit loop).
func adminBillingBatchUpdatePriceHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	request, err := adminBillingParseBatchPriceUpdateBody(adminBillingBody(body))
	if err != nil {
		adminBillingRespondBatchPriceError(c, err)
		return
	}

	var preview *adminBillingBatchPreview
	var notificationItems []adminBillingBatchItem
	txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The TypeScript runs the preview builder on the transaction client, so the
		// quote reads inside the same snapshot as the writes below.
		currentPreview, err := adminBillingBuildBatchPreviewInTx(ctx, tx, request.InstanceIDs, request.NewPrice, request.SettleBalance)
		if err != nil {
			return err
		}

		versioned := make([]adminBillingBatchItem, 0, len(currentPreview.Items))
		for _, item := range currentPreview.Items {
			if item.Status != "failed" {
				versioned = append(versioned, item)
			}
		}
		expectedVersions := map[int32]int{}
		for _, expectation := range request.Expectations {
			expectedVersions[expectation.InstanceID] = expectation.Version
		}
		missing := false
		for _, item := range versioned {
			if _, ok := expectedVersions[item.ID]; !ok {
				missing = true
				break
			}
		}
		if missing {
			return &adminBillingBatchPriceError{
				Message:    "请先刷新预览后再提交",
				StatusCode: http.StatusBadRequest,
				Preview:    currentPreview,
			}
		}

		stale := false
		for _, item := range versioned {
			expected := expectedVersions[item.ID]
			if item.InstanceVersion == nil || expected != int(*item.InstanceVersion) {
				stale = true
				break
			}
		}
		if stale {
			return &adminBillingBatchPriceError{
				Message:    "实例计费信息已变化，请刷新预览后重试",
				StatusCode: http.StatusConflict,
				Preview:    currentPreview,
			}
		}

		if !currentPreview.CanSubmit {
			return &adminBillingBatchPriceError{
				Message:    "批量修改价格预览未通过，请检查失败项或用户余额",
				StatusCode: http.StatusBadRequest,
				Preview:    currentPreview,
			}
		}

		changedItems := make([]adminBillingBatchItem, 0, len(currentPreview.Items))
		changedIDs := make([]int32, 0, len(currentPreview.Items))
		for _, item := range currentPreview.Items {
			if item.Status == "ready" {
				changedItems = append(changedItems, item)
				changedIDs = append(changedIDs, item.ID)
			}
		}

		periodEnds := map[int32]*time.Time{}
		if len(changedIDs) > 0 {
			var rows []struct {
				ID        int32      `gorm:"column:id"`
				ExpiresAt *time.Time `gorm:"column:expires_at"`
			}
			if err := tx.WithContext(ctx).Raw(
				`SELECT id, expires_at FROM instances WHERE id IN ?`, changedIDs,
			).Scan(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				periodEnds[row.ID] = row.ExpiresAt
			}
		}

		now := time.Now().UTC()
		for _, item := range changedItems {
			update := tx.WithContext(ctx).Exec(
				`UPDATE instances SET billing_price = ?, version = version + 1, updated_at = ?
				  WHERE id = ? AND version = ?`,
				adminBillingDecimal(item.NewPrice), now, item.ID, *item.InstanceVersion,
			)
			if update.Error != nil {
				return update.Error
			}
			if update.RowsAffected != 1 {
				return &adminBillingBatchPriceError{
					Message:    fmt.Sprintf("实例 #%d 计费信息已变化，请刷新后重试", item.ID),
					StatusCode: http.StatusConflict,
					Preview:    currentPreview,
				}
			}
		}

		if request.SettleBalance {
			settlementItems := make([]adminBillingBatchItem, 0, len(changedItems))
			for _, item := range changedItems {
				if item.User != nil && item.PriceDiff != 0 {
					settlementItems = append(settlementItems, item)
				}
			}
			sort.SliceStable(settlementItems, func(i, j int) bool {
				if settlementItems[i].User.ID != settlementItems[j].User.ID {
					return settlementItems[i].User.ID < settlementItems[j].User.ID
				}
				return settlementItems[i].PriceDiff < settlementItems[j].PriceDiff
			})

			for _, item := range settlementItems {
				user := *item.User
				locked, lockErr := db.TryAdvisoryTransactionLockTx(tx, db.UserBalanceLockNamespace, user.ID)
				if lockErr != nil {
					return lockErr
				}
				if !locked {
					return &adminBillingBatchPriceError{
						Message:    fmt.Sprintf("用户 %s 余额正在处理，请稍后重试", user.Username),
						StatusCode: http.StatusConflict,
						Preview:    currentPreview,
					}
				}

				if item.PriceDiff > 0 {
					result, err := db.InstanceBillingApplyBalanceTx(ctx, tx, db.InstanceBillingBalanceChange{
						UserID:     user.ID,
						InstanceID: item.ID,
						Delta:      adminBillingDecimal(-item.PriceDiff),
						LogType:    models.BalanceLogTypeConsume,
						Remark:     fmt.Sprintf("管理员批量调整实例价格，补交差价 ¥%s", adminBillingFixed2(item.PriceDiff)),
					})
					if err != nil {
						return &adminBillingBatchPriceError{
							Message:    fmt.Sprintf("用户 %s 余额不足，请刷新预览后重试", user.Username),
							StatusCode: http.StatusBadRequest,
							Preview:    currentPreview,
						}
					}
					_ = result
				} else {
					refundAmount := math.Abs(item.PriceDiff)
					if _, err := db.InstanceBillingApplyBalanceTx(ctx, tx, db.InstanceBillingBalanceChange{
						UserID:     user.ID,
						InstanceID: item.ID,
						Delta:      adminBillingDecimal(refundAmount),
						LogType:    models.BalanceLogTypeRefund,
						Remark:     fmt.Sprintf("管理员批量调整实例价格，退还差价 ¥%s", adminBillingFixed2(refundAmount)),
					}); err != nil {
						return err
					}
				}

				// Re-read the balance snapshot written by ApplyBalanceTx so the billing
				// record points at the exact same audit row.
				var latest models.BalanceLog
				remark := fmt.Sprintf("管理员批量调整实例价格，补交差价 ¥%s", adminBillingFixed2(item.PriceDiff))
				if item.PriceDiff < 0 {
					remark = fmt.Sprintf("管理员批量调整实例价格，退还差价 ¥%s", adminBillingFixed2(math.Abs(item.PriceDiff)))
				}
				if err := tx.WithContext(ctx).
					Where("user_id = ? AND instance_id = ? AND remark = ?", user.ID, item.ID, remark).
					Order("id DESC").Limit(1).Find(&latest).Error; err != nil {
					return err
				}
				var balanceLogID *int32
				if latest.ID != 0 {
					balanceLogID = &latest.ID
				}

				periodEnd := now
				if value, ok := periodEnds[item.ID]; ok && value != nil {
					periodEnd = *value
				}
				recordType := models.BillingRecordTypeUpgrade
				recordAmount := math.Abs(item.PriceDiff)
				if item.PriceDiff < 0 {
					recordType = models.BillingRecordTypeRefund
					recordAmount = -math.Abs(item.PriceDiff)
				}
				recordRemark := fmt.Sprintf("批量价格调整: ¥%s → ¥%s",
					adminBillingFixed2(adminBillingDerefFloat(item.OldPrice)), adminBillingFixed2(item.NewPrice))
				record := models.InstanceBillingRecord{
					InstanceID:   item.ID,
					UserID:       user.ID,
					Type:         recordType,
					Amount:       adminBillingDecimal(recordAmount),
					Months:       0,
					PeriodStart:  now,
					PeriodEnd:    periodEnd,
					BalanceLogID: balanceLogID,
					Remark:       &recordRemark,
				}
				if err := tx.WithContext(ctx).Create(&record).Error; err != nil {
					return err
				}
			}
		}

		for _, item := range changedItems {
			if item.User != nil && item.PriceDiff != 0 {
				notificationItems = append(notificationItems, item)
			}
		}
		preview = currentPreview
		return nil
	})
	if txErr != nil {
		adminBillingRespondBatchPriceError(c, txErr)
		return
	}

	db.CreateLog(ctx, adminBillingInt64Ptr(int64(actor.ID)), adminBillingLogModule, "instance.admin_batch_update_price",
		fmt.Sprintf("Admin batch updated %d instance prices to ¥%s, settle: %t, charge: ¥%s, refund: ¥%s",
			preview.Summary.ChangedCount, adminBillingFixed2(request.NewPrice), request.SettleBalance,
			adminBillingFixed2(preview.Summary.TotalCharge), adminBillingFixed2(preview.Summary.TotalRefund)),
		db.LogResultSuccess, db.CreateLogOptions{})
	adminBillingAudit(ctx, c, actor, "instance.admin_batch_update_price",
		fmt.Sprintf("Admin batch updated %d instance prices to ¥%s",
			preview.Summary.ChangedCount, adminBillingFixed2(request.NewPrice)),
		"instance_batch", 0, "", fmt.Sprintf("charge=%s refund=%s",
			adminBillingFixed2(preview.Summary.TotalCharge), adminBillingFixed2(preview.Summary.TotalRefund)))

	for _, item := range notificationItems {
		name := fmt.Sprintf("#%d", item.ID)
		if item.Name != nil {
			name = *item.Name
		}
		lib.SendNotification(ctx, int64(item.User.ID), lib.EventInstancePriceChanged, lib.EventData{
			"instanceName": name,
			"oldPrice":     adminBillingDerefFloat(item.OldPrice),
			"newPrice":     item.NewPrice,
			"priceDiff":    math.Abs(item.PriceDiff),
			"isIncrease":   item.PriceDiff > 0,
		})
	}

	responseMessage := "价格未变更"
	if preview.Summary.ChangedCount > 0 {
		responseMessage = fmt.Sprintf("批量价格修改完成，已更新 %d 个实例", preview.Summary.ChangedCount)
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": responseMessage,
		"preview": adminBillingSerializeBatchPreview(preview),
	})
}

// adminBillingRespondBatchPriceError mirrors the two catch blocks of the batch
// handlers.
func adminBillingRespondBatchPriceError(c *gin.Context, err error) {
	var priceError *adminBillingBatchPriceError
	if errors.As(err, &priceError) {
		payload := gin.H{"error": priceError.Message}
		if priceError.Preview != nil {
			payload["preview"] = adminBillingSerializeBatchPreview(priceError.Preview)
		}
		c.JSON(priceError.StatusCode, payload)
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "批量修改实例价格失败"})
}

func adminBillingDerefFloat(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

// ==================== GET /api/admin/instances/:id/available-plans ====================

// adminBillingAvailablePlansHandler mirrors GET /:id/available-plans.
func adminBillingAvailablePlansHandler(c *gin.Context) {
	ctx := c.Request.Context()

	instanceID, valid := adminBillingParseRouteID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的实例ID"})
		return
	}

	loaded, err := db.GetAdminBillingInstanceContext(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取可升级方案列表失败"})
		return
	}
	if loaded == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
		return
	}
	instance := loaded.Instance
	if instance.PackagePlanID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该实例不是付费实例，无法切换方案"})
		return
	}
	currentPlan, err := db.GetPackagePlanById(ctx, *instance.PackagePlanID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取可升级方案列表失败"})
		return
	}
	if currentPlan == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该实例不是付费实例，无法切换方案"})
		return
	}

	var packageInstanceType string
	if instance.PackageID != nil {
		if pkg, err := db.GetPackageById(ctx, *instance.PackageID); err == nil && pkg != nil {
			packageInstanceType = pkg.InstanceType
		}
	}

	allPlans, err := db.GetAdminBillingAvailablePlans(ctx, *instance.PackageID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取可升级方案列表失败"})
		return
	}

	availablePlans := make([]gin.H, 0, len(allPlans))
	for _, plan := range allPlans {
		if plan.ID == currentPlan.ID {
			continue
		}
		preview, err := adminBillingPlanChange(ctx, &instance, plan, nil)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "获取可升级方案列表失败"})
			return
		}
		if !preview.IsUpgrade {
			continue
		}
		capacity, err := db.HostsCheckPlanUpgradeCapacity(ctx,
			db.HostsInstanceSpec{ID: instance.ID, HostID: instance.HostID, CPU: instance.CPU, Memory: instance.Memory, Disk: instance.Disk},
			db.HostsPlanSpec{CPU: plan.CPU, Memory: plan.Memory, Disk: plan.Disk},
			nil,
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "获取可升级方案列表失败"})
			return
		}

		priceCents := plan.Price.InexactFloat64()
		billingCycle := plan.BillingCycle
		if billingCycle <= 0 {
			billingCycle = 1
		}
		swapSize := plan.SwapSize
		if packageInstanceType == "vm" {
			swapSize = 0
		}

		var cannotUpgradeReason any
		var resourceWarnings any
		if !capacity.CanUpgrade {
			reason := ""
			if capacity.Reason != nil {
				reason = *capacity.Reason
			}
			cannotUpgradeReason = reason
			resourceWarnings = []string{adminBillingCapacityWarning(reason)}
		}

		availablePlans = append(availablePlans, gin.H{
			"id":                  plan.ID,
			"name":                plan.Name,
			"description":         plan.Description,
			"price":               priceCents / 100,
			"billingCycle":        plan.BillingCycle,
			"monthlyPrice":        adminBillingRoundMoney(priceCents / float64(billingCycle) / 100),
			"cpu":                 plan.CPU,
			"memory":              plan.Memory,
			"disk":                plan.Disk,
			"portLimit":           plan.PortLimit,
			"snapshotLimit":       plan.SnapshotLimit,
			"backupLimit":         plan.BackupLimit,
			"siteLimit":           plan.SiteLimit,
			"swapSize":            swapSize,
			"trafficLimit":        strconv.FormatInt(plan.TrafficLimit, 10),
			"isSoldOut":           plan.IsSoldOut,
			"slaGuarantee":        plan.SlaGuarantee,
			"priceDiff":           preview.PriceDiff,
			"canUpgrade":          capacity.CanUpgrade,
			"cannotUpgradeReason": cannotUpgradeReason,
			"resourceWarnings":    resourceWarnings,
			"resourceCapacity":    capacity,
		})
	}

	remainingDays := 0.0
	if instance.ExpiresAt != nil {
		remainingDays = math.Max(0, float64(instance.ExpiresAt.UnixMilli()-time.Now().UTC().UnixMilli())/(1000*60*60*24))
	}

	currentPrice := currentPlan.Price.InexactFloat64() / 100
	if instance.BillingPrice != nil {
		currentPrice = instance.BillingPrice.InexactFloat64()
	}
	currentCycle := currentPlan.BillingCycle
	if instance.BillingCycle != nil && *instance.BillingCycle > 0 {
		currentCycle = *instance.BillingCycle
	}

	c.JSON(http.StatusOK, gin.H{
		"currentPlan": gin.H{
			"id":           currentPlan.ID,
			"name":         currentPlan.Name,
			"price":        currentPrice,
			"billingCycle": currentPlan.BillingCycle,
			"monthlyPrice": adminBillingRoundMoney(lib.CalculateMonthlyPrice(currentPrice, int(currentCycle))),
			"cpu":          currentPlan.CPU,
			"memory":       currentPlan.Memory,
			"disk":         currentPlan.Disk,
		},
		"remainingDays":  remainingDays,
		"availablePlans": availablePlans,
		"userBalance":    loaded.User.Balance,
	})
}

// adminBillingCapacityWarning mirrors the resourceWarnings ternary chain.
func adminBillingCapacityWarning(reason string) string {
	switch reason {
	case "cpu_insufficient":
		return "实例所在节点 CPU 配额不足"
	case "memory_insufficient":
		return "实例所在节点内存不足"
	case "disk_insufficient":
		return "实例所在节点磁盘容量不足"
	default:
		return "实例所在节点资源不足"
	}
}

// ==================== POST /api/admin/instances/:id/upgrade-plan ====================

// adminBillingUpgradePlanHandler mirrors POST /:id/upgrade-plan.
func adminBillingUpgradePlanHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)

	instanceID, valid := adminBillingParseRouteID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的实例ID"})
		return
	}

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object := adminBillingBody(body)
	newPlanID, ok := adminBillingBodyInt(object, "newPlanId")
	if !ok || newPlanID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的新方案ID"})
		return
	}

	loaded, err := db.GetAdminBillingInstanceContext(ctx, instanceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "升级方案失败"})
		return
	}
	if loaded == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "实例不存在"})
		return
	}
	instance := loaded.Instance
	if instance.PackagePlanID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该实例不是付费实例，无法切换方案"})
		return
	}
	currentPlan, err := db.GetPackagePlanById(ctx, *instance.PackagePlanID)
	if err != nil || currentPlan == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该实例不是付费实例，无法切换方案"})
		return
	}
	if instance.Status == models.InstanceStatusDeleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已删除"})
		return
	}

	newPlan, err := db.GetPackagePlanById(ctx, int32(newPlanID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "升级方案失败"})
		return
	}
	if newPlan == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "新方案不存在"})
		return
	}
	if !newPlan.IsActive {
		c.JSON(http.StatusBadRequest, gin.H{"error": "新方案已下架"})
		return
	}
	if newPlan.IsSoldOut {
		c.JSON(http.StatusBadRequest, gin.H{"error": "新方案已售罄", "code": "PLAN_SOLD_OUT"})
		return
	}
	if instance.PackageID == nil || newPlan.PackageID != *instance.PackageID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "新方案不属于同一套餐"})
		return
	}

	quote, err := adminBillingPlanChange(ctx, &instance, *newPlan, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "升级方案失败"})
		return
	}
	if quote.RemainingDays <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "实例已过期，请先续费后再升级方案"})
		return
	}
	if !quote.IsUpgrade {
		c.JSON(http.StatusBadRequest, gin.H{"error": "只能升级到更高价格的方案"})
		return
	}

	trafficLimitSpeed := newPlan.TrafficLimitSpeed
	normalizedSpeed := services.TrafficBandwidthNormalizePlanTrafficLimitSpeed(&trafficLimitSpeed)
	if normalizedSpeed.Invalid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "新方案带宽配置无效"})
		return
	}
	normalizedLimit := normalizedSpeed.Value
	normalizedString := ""
	if normalizedLimit != nil {
		normalizedString = *normalizedLimit
	}

	priceDifference := quote.PriceDiff
	userBalance := loaded.User.Balance
	if priceDifference > userBalance {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf("用户余额不足，需要 ¥%s，当前余额 ¥%s",
				adminBillingFixed2(priceDifference), adminBillingFixed2(userBalance)),
		})
		return
	}

	ingressString := ""
	if instance.LimitsIngress != nil {
		ingressString = *instance.LimitsIngress
	}
	egressString := ""
	if instance.LimitsEgress != nil {
		egressString = *instance.LimitsEgress
	}
	resourcesChanged := newPlan.CPU != currentPlan.CPU ||
		newPlan.Memory != currentPlan.Memory ||
		newPlan.Disk != currentPlan.Disk ||
		normalizedString != ingressString ||
		normalizedString != egressString

	var hostDetails *db.HostsRecord
	if host, err := db.HostsGetByID(ctx, instance.HostID); err == nil {
		hostDetails = host
	}

	txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := db.GpusAssertPlanChange(ctx, tx, instanceID, int32(newPlanID)); err != nil {
			return err
		}

		balanceBefore := userBalance
		balanceAfter := balanceBefore

		monthlyTrafficLimit, err := lib.ResolveInstanceTrafficLimitForHost(ctx, tx, lib.TrafficMultiplierResolveParams{
			PackageID:        instance.PackageID,
			HostID:           instance.HostID,
			BaseTrafficLimit: &newPlan.TrafficLimit,
		})
		if err != nil {
			return err
		}

		capacity, err := db.HostsReservePlanUpgradeCapacityWithLock(ctx, tx,
			db.HostsInstanceSpec{ID: instance.ID, HostID: instance.HostID, CPU: instance.CPU, Memory: instance.Memory, Disk: instance.Disk},
			db.HostsPlanSpec{CPU: newPlan.CPU, Memory: newPlan.Memory, Disk: newPlan.Disk},
		)
		if err != nil {
			return err
		}
		if !capacity.CanUpgrade {
			reason := "unknown"
			if capacity.Reason != nil {
				reason = *capacity.Reason
			}
			return &adminBillingHostCapacityError{Reason: reason}
		}

		var balanceLogID int32
		if priceDifference > 0 {
			locked, lockErr := db.TryAdvisoryTransactionLockTx(tx, db.UserBalanceLockNamespace, instance.UserID)
			if lockErr != nil {
				return lockErr
			}
			if !locked {
				return errAdminBillingBalanceConflict
			}

			result, err := db.InstanceBillingApplyBalanceTx(ctx, tx, db.InstanceBillingBalanceChange{
				UserID:     instance.UserID,
				InstanceID: instanceID,
				Delta:      adminBillingDecimal(-priceDifference),
				LogType:    models.BalanceLogTypeConsume,
				Remark: fmt.Sprintf("方案升级：%s → %s，剩余 %d 天补差价",
					currentPlan.Name, newPlan.Name, int(math.Ceil(quote.RemainingDays))),
			})
			if err != nil {
				return err
			}
			balanceLogID = result.BalanceLogID
			balanceAfter = result.BalanceAfter.InexactFloat64()
			balanceBefore = adminBillingRoundMoney(balanceAfter + priceDifference)
		} else {
			// A non-positive difference still writes the audit row: the TypeScript
			// creates balance_logs unconditionally with amount -priceDifference.
			remark := fmt.Sprintf("方案升级：%s → %s，剩余 %d 天补差价",
				currentPlan.Name, newPlan.Name, int(math.Ceil(quote.RemainingDays)))
			entry := models.BalanceLog{
				UserID:        instance.UserID,
				Type:          models.BalanceLogTypeConsume,
				Amount:        adminBillingDecimal(-priceDifference),
				BalanceBefore: adminBillingDecimal(balanceBefore),
				BalanceAfter:  adminBillingDecimal(balanceAfter),
				InstanceID:    &instanceID,
				Remark:        &remark,
			}
			if err := tx.WithContext(ctx).Create(&entry).Error; err != nil {
				return err
			}
			balanceLogID = entry.ID
		}

		swapSize := instance.SwapSize
		swapEnabled := instance.SwapEnabled
		if instance.PackageID != nil {
			if pkg, err := db.GetPackageById(ctx, *instance.PackageID); err == nil && pkg != nil && pkg.InstanceType == "vm" {
				swapEnabled = false
				swapSize = nil
			} else if lib.InstanceSwapShouldSyncWithPlan(instance.SwapSize, &currentPlan.SwapSize) {
				value := newPlan.SwapSize
				swapSize = &value
			}
		}

		trafficStatus := lib.CalculateInstanceTrafficStatus(instance.MonthlyTrafficUsed, monthlyTrafficLimit)
		billingPrice := adminBillingDecimal(newPlan.Price.InexactFloat64() / 100)

		updates := map[string]any{
			"package_plan_id":       newPlan.ID,
			"cpu":                   newPlan.CPU,
			"memory":                newPlan.Memory,
			"disk":                  newPlan.Disk,
			"port_limit":            newPlan.PortLimit,
			"snapshot_limit":        newPlan.SnapshotLimit,
			"backup_limit":          newPlan.BackupLimit,
			"site_limit":            newPlan.SiteLimit,
			"swap_enabled":          swapEnabled,
			"swap_size":             swapSize,
			"monthly_traffic_limit": monthlyTrafficLimit,
			"limits_ingress":        normalizedLimit,
			"limits_egress":         normalizedLimit,
			"traffic_status":        trafficStatus,
			"billing_price":         billingPrice,
			"billing_cycle":         newPlan.BillingCycle,
			"updated_at":            time.Now().UTC(),
		}
		if err := tx.WithContext(ctx).Model(&models.Instance{}).
			Where("id = ?", instanceID).Updates(updates).Error; err != nil {
			return err
		}

		periodEnd := time.Now().UTC()
		if instance.ExpiresAt != nil {
			periodEnd = *instance.ExpiresAt
		}
		remark := fmt.Sprintf("方案升级：%s → %s（操作者: %s）", currentPlan.Name, newPlan.Name, actor.Username)
		record := models.InstanceBillingRecord{
			InstanceID:   instanceID,
			UserID:       instance.UserID,
			Type:         models.BillingRecordTypeUpgrade,
			Amount:       adminBillingDecimal(priceDifference),
			Months:       0,
			PeriodStart:  time.Now().UTC(),
			PeriodEnd:    periodEnd,
			BalanceLogID: &balanceLogID,
			Remark:       &remark,
		}
		return tx.WithContext(ctx).Create(&record).Error
	})
	if txErr != nil {
		var capacityErr *adminBillingHostCapacityError
		var gpuErr *db.GpusError
		switch {
		case errors.Is(txErr, errAdminBillingBalanceConflict):
			c.JSON(http.StatusConflict, gin.H{"error": "用户余额正在处理，请稍后重试"})
		case adminBillingIsBalanceInsufficient(txErr):
			c.JSON(http.StatusBadRequest, gin.H{"error": "用户余额不足"})
		case errors.As(txErr, &gpuErr):
			c.JSON(http.StatusConflict, gin.H{"error": gpuErr.Message, "code": "GPU_UNAVAILABLE"})
		case errors.As(txErr, &capacityErr):
			c.JSON(http.StatusBadRequest, gin.H{
				"error":  adminBillingCapacityWarning(capacityErr.Reason),
				"code":   "HOST_RESOURCES_INSUFFICIENT",
				"reason": capacityErr.Reason,
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "升级方案失败"})
		}
		return
	}

	// Sync the node with the new resources (best effort: the database is the source
	// of truth and a node failure must not fail the upgrade).
	if resourcesChanged && hostDetails != nil {
		if client, err := instancesIncusClient(ctx, hostDetails); err == nil {
			cpu, memory, disk := int(newPlan.CPU), int(newPlan.Memory), int(newPlan.Disk)
			if _, err := lib.PatchInstanceResources(ctx, client, instance.IncusID, &cpu, &memory, &disk); err == nil {
				_ = lib.RestoreBandwidth(ctx, client, instance.IncusID, normalizedLimit, normalizedLimit)
			}
		}
	}

	db.CreateLog(ctx, adminBillingInt64Ptr(int64(actor.ID)), adminBillingLogModule, "instance.upgrade_plan",
		fmt.Sprintf(`Admin upgraded instance "%s" plan: %s → %s (User: %s, PriceDiff: ¥%s)`,
			instance.Name, currentPlan.Name, newPlan.Name, loaded.User.Username, adminBillingFixed2(priceDifference)),
		db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	adminBillingAudit(ctx, c, actor, "instance.upgrade_plan",
		fmt.Sprintf(`Admin upgraded instance "%s" plan: %s → %s`,
			instance.Name, currentPlan.Name, newPlan.Name),
		"instance", int64(instanceID), instance.Name,
		fmt.Sprintf("priceDiff=%s", adminBillingFixed2(priceDifference)))

	lib.SendNotification(ctx, int64(instance.UserID), lib.EventInstancePlanChanged, lib.EventData{
		"instanceName": instance.Name,
		"oldPlanName":  currentPlan.Name,
		"newPlanName":  newPlan.Name,
		"priceDiff":    priceDifference,
	})

	c.JSON(http.StatusOK, gin.H{
		"success":         true,
		"message":         fmt.Sprintf("方案已升级，补差价 ¥%s", adminBillingFixed2(priceDifference)),
		"priceDifference": priceDifference,
		"oldPlan":         gin.H{"id": currentPlan.ID, "name": currentPlan.Name},
		"newPlan":         gin.H{"id": newPlan.ID, "name": newPlan.Name},
		"resourcesSynced": resourcesChanged,
	})
}

// adminBillingHostCapacityError mirrors the `HOST_RESOURCES_INSUFFICIENT:<reason>`
// sentinel thrown inside the upgrade transaction.
type adminBillingHostCapacityError struct {
	Reason string
}

func (e *adminBillingHostCapacityError) Error() string {
	return "HOST_RESOURCES_INSUFFICIENT:" + e.Reason
}

// ==================== POST /api/admin/billing/recharge-records/:id/sync ====================

// adminBillingBuildEpayConfig mirrors the LOCAL buildEpayConfig of
// admin-billing.ts. It deliberately does not reuse rechargeBuildEpayConfig: the
// admin route answers with the bare '支付渠道配置不完整' message, while recharge.ts
// appends the version-specific hint.
func adminBillingBuildEpayConfig(config map[string]any) (lib.EpayConfig, bool, string) {
	version, _ := config["version"].(string)
	stringField := func(key string) string {
		value, _ := config[key].(string)
		return value
	}

	if version == "v1" {
		epayConfig := lib.EpayConfig{
			Version: lib.EpayVersionV1,
			APIURL:  stringField("apiurl"),
			PID:     stringField("pid"),
			Key:     stringField("key"),
		}
		if epayConfig.APIURL == "" || epayConfig.PID == "" || epayConfig.Key == "" {
			return epayConfig, false, "支付渠道配置不完整"
		}
		return epayConfig, true, ""
	}

	epayConfig := lib.EpayConfig{
		Version:            lib.EpayVersionV2,
		APIURL:             stringField("apiurl"),
		PID:                stringField("pid"),
		PlatformPublicKey:  stringField("platform_public_key"),
		MerchantPrivateKey: stringField("merchant_private_key"),
	}
	if epayConfig.APIURL == "" || epayConfig.PID == "" ||
		epayConfig.PlatformPublicKey == "" || epayConfig.MerchantPrivateKey == "" {
		return epayConfig, false, "支付渠道配置不完整"
	}
	return epayConfig, true, ""
}

// adminBillingTradeNoForIndex mirrors getTradeNoForIndex: a missing gateway trade
// number is normalised into a deterministic pseudo-number so the
// (provider, order, trade) unique index still dedupes.
func adminBillingTradeNoForIndex(orderNo string, tradeNo *string) string {
	if tradeNo != nil && strings.TrimSpace(*tradeNo) != "" {
		return strings.TrimSpace(*tradeNo)
	}
	return "__NO_TRADE_NO__" + orderNo
}

// adminBillingSyncRechargeHandler mirrors POST /recharge-records/:id/sync.
func adminBillingSyncRechargeHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)

	recordID, valid := adminBillingParseRouteID(c.Param("id"))
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的记录ID"})
		return
	}

	record, err := db.GetRechargeRecordByID(ctx, recordID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "同步失败，请稍后重试"})
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "充值记录不存在"})
		return
	}

	if record.Status == models.RechargeStatusCompleted {
		c.JSON(http.StatusOK, gin.H{
			"success": true, "synced": false, "status": "completed",
			"message": "订单已完成，无需同步",
		})
		return
	}
	if record.Status == models.RechargeStatusCancelled || record.Status == models.RechargeStatusFailed {
		word := "失败"
		if record.Status == models.RechargeStatusCancelled {
			word = "取消"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "订单已" + word + "，无法同步"})
		return
	}
	if record.Provider == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "支付渠道不存在"})
		return
	}

	provider, err := db.GetPaymentProviderByID(ctx, record.ProviderID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "同步失败，请稍后重试"})
		return
	}
	if provider == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "支付渠道不存在"})
		return
	}
	providerType := string(provider.Type)
	if providerType != "yipay" && providerType != "heleket" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该支付渠道不支持手动同步"})
		return
	}

	effectiveConfig, _ := lib.ResolveRechargeProviderConfigSnapshot(
		providerType, provider.Config, record.ProviderConfigSnapshot)

	rechargeAmount := record.Amount.InexactFloat64()
	orderAmount := lib.GetRechargePayableAmount(lib.RechargePayableSource{
		Amount:         rechargeAmount,
		PaymentDetails: record.PaymentDetails,
	})
	creditedAmount := rechargeAmount
	if record.ActualAmount != nil {
		creditedAmount = record.ActualAmount.InexactFloat64()
	}

	var tradeNo string
	var tradeNoForIdx string
	actualAmount := creditedAmount
	callbackPayload := map[string]any{"source": "admin_sync", "adminId": actor.ID}
	var paymentDetails any = record.PaymentDetails

	switch providerType {
	case "yipay":
		epayConfig, valid, configError := adminBillingBuildEpayConfig(effectiveConfig)
		if !valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": configError})
			return
		}
		client := lib.CreateEpayClient(epayConfig)
		queryResult := client.QueryOrder(ctx, record.OrderNo)

		if !queryResult.Success {
			c.JSON(http.StatusOK, gin.H{
				"success": true, "synced": false, "status": string(record.Status),
				"message": "查询支付平台失败，请稍后重试",
			})
			return
		}
		if !queryResult.Paid {
			c.JSON(http.StatusOK, gin.H{
				"success": true, "synced": false, "status": "pending",
				"message": "支付平台返回订单未支付",
			})
			return
		}

		tradeNo = queryResult.TradeNo
		tradeNoForIdx = adminBillingTradeNoForIndex(record.OrderNo, &tradeNo)

		paidAmount := 0.0
		if queryResult.Money != "" {
			paidAmount, _ = adminBillingJSNumber(queryResult.Money)
		}
		if paidAmount > 0 && math.Abs(paidAmount-orderAmount) > 0.01 {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("支付金额与订单金额不匹配（应付: %s，实付: %s）",
					adminBillingJSNumberString(orderAmount), adminBillingJSNumberString(paidAmount)),
			})
			return
		}

		actualAmount = creditedAmount
		callbackPayload = map[string]any{
			"source":      "admin_sync",
			"queryResult": lib.SanitizeObject(queryResult.RawData),
			"adminId":     actor.ID,
		}
	case "heleket":
		heleketConfig, valid, configError := lib.BuildHeleketConfig(effectiveConfig)
		if !valid {
			c.JSON(http.StatusBadRequest, gin.H{"error": configError})
			return
		}
		client := lib.CreateHeleketClient(heleketConfig)
		queryResult, err := client.GetPaymentInfo(ctx, lib.HeleketPaymentInfoRequest{OrderID: record.OrderNo})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "同步失败，请稍后重试"})
			return
		}
		status := lib.ExtractHeleketStatus(queryResult)
		paymentState := lib.GetHeleketPaymentState(queryResult)
		statusMessage := lib.GetHeleketStatusDescription(status)

		callbackPayload = map[string]any{
			"source":        "admin_sync",
			"heleketStatus": status,
			"queryResult":   lib.SanitizeObject(queryResult),
			"adminId":       actor.ID,
		}
		mergedDetails := lib.MergeHeleketPaymentDetails(record.PaymentDetails, queryResult, &heleketConfig,
			&lib.RechargeHeleketMergeFallback{
				OrderNo:       adminBillingStringPtr(record.OrderNo),
				InvoiceAmount: &orderAmount,
			})
		paymentDetails = mergedDetails

		uuid, _ := queryResult["uuid"].(string)
		txid, _ := queryResult["txid"].(string)
		var heleketTradeNo *string
		if strings.TrimSpace(uuid) != "" {
			heleketTradeNo = &uuid
		} else if strings.TrimSpace(txid) != "" {
			heleketTradeNo = &txid
		}

		if paymentState == lib.HeleketPaymentStatePending {
			if _, err := db.UpdateRechargeOrderMetadata(ctx, record.OrderNo, db.RechargeMetadataUpdate{
				TradeNo:                heleketTradeNo,
				TradeNoProvided:        true,
				PaymentDetails:         paymentDetails,
				PaymentDetailsProvided: true,
			}); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "同步失败，请稍后重试"})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"success": true, "synced": false, "status": "pending",
				"message": statusMessage,
			})
			return
		}
		if paymentState == lib.HeleketPaymentStateCancelled {
			if _, err := db.CancelRecharge(ctx, record.OrderNo, callbackPayload, true, paymentDetails, true); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "同步失败，请稍后重试"})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"success": true, "synced": false, "status": "cancelled",
				"message": statusMessage,
			})
			return
		}
		if paymentState == lib.HeleketPaymentStateFailed {
			if _, err := db.FailRecharge(ctx, record.OrderNo, statusMessage, callbackPayload, true, paymentDetails, true); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "同步失败，请稍后重试"})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"success": true, "synced": false, "status": "failed",
				"message": statusMessage,
			})
			return
		}

		orderID, _ := queryResult["order_id"].(string)
		switch {
		case strings.TrimSpace(uuid) != "":
			tradeNo = strings.TrimSpace(uuid)
		case strings.TrimSpace(txid) != "":
			tradeNo = strings.TrimSpace(txid)
		case strings.TrimSpace(orderID) != "":
			tradeNo = strings.TrimSpace(orderID)
		default:
			tradeNo = record.OrderNo
		}
		tradeNoForIdx = adminBillingTradeNoForIndex(record.OrderNo, &tradeNo)

		invoiceAmount := lib.GetHeleketInvoiceAmount(queryResult)
		if invoiceAmount == nil || math.IsNaN(*invoiceAmount) || math.IsInf(*invoiceAmount, 0) || *invoiceAmount <= 0 {
			c.JSON(http.StatusOK, gin.H{
				"success": true, "synced": false, "status": string(record.Status),
				"message": "支付金额异常，请稍后重试",
			})
			return
		}
		if *invoiceAmount+0.01 < orderAmount {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": fmt.Sprintf("支付金额不足（应付: %s，账单金额: %s）",
					adminBillingJSNumberString(orderAmount), adminBillingJSNumberString(*invoiceAmount)),
			})
			return
		}
		actualAmount = creditedAmount
	}

	if db.IsPaymentCallbackProcessed(ctx, provider.ID, record.OrderNo, tradeNoForIdx) {
		updated, _ := db.GetRechargeRecordByID(ctx, recordID)
		status := "completed"
		if updated != nil {
			status = string(updated.Status)
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true, "synced": false, "status": status,
			"message": "订单已处理过",
		})
		return
	}

	completion, err := db.CompleteRecharge(ctx, record.OrderNo, db.CompleteRechargeData{
		TradeNo:                &tradeNo,
		TradeNoProvided:        true,
		ActualAmount:           &actualAmount,
		CallbackData:           callbackPayload,
		CallbackDataProvided:   true,
		PaymentDetails:         paymentDetails,
		PaymentDetailsProvided: true,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "同步失败，请稍后重试"})
		return
	}

	db.MarkPaymentCallbackProcessed(ctx, provider.ID, record.OrderNo, &tradeNoForIdx,
		adminBillingStringPtr("admin_sync"))

	if !completion.CompletedNow {
		c.JSON(http.StatusOK, gin.H{
			"success": true, "synced": false, "status": "completed",
			"message": "订单已处理过",
		})
		return
	}

	username := ""
	if owner, err := db.FindUserById(ctx, record.UserID); err == nil && owner != nil {
		username = owner.Username
	}
	db.CreateLog(ctx, adminBillingInt64Ptr(int64(actor.ID)), "user", "admin.recharge.sync",
		fmt.Sprintf("Admin synced recharge order: %s, user: %s, amount: %s, tradeNo: %s",
			record.OrderNo, username, adminBillingJSNumberString(actualAmount),
			adminBillingTradeNoForDisplay(tradeNo)),
		db.LogResultSuccess, db.CreateLogOptions{})
	adminBillingAudit(ctx, c, actor, "admin.recharge.sync",
		fmt.Sprintf("Admin synced recharge order: %s", record.OrderNo),
		"recharge_record", int64(recordID), record.OrderNo,
		fmt.Sprintf("amount=%s tradeNo=%s", adminBillingJSNumberString(actualAmount), tradeNo))

	// The inbox message is best effort: a notification failure must not undo a
	// completed recharge.
	_, _ = db.CreateInboxMessage(ctx, db.CreateInboxMessageData{
		UserID:    record.UserID,
		EventType: "recharge_success",
		Title:     "充值到账通知",
		Content: fmt.Sprintf("您的充值已到账！\n充值金额：￥%s\n订单号：%s\n交易号：%s",
			adminBillingFixed2(actualAmount), record.OrderNo,
			adminBillingTradeNoForDisplay(tradeNo)),
		Data: gin.H{
			"orderNo": record.OrderNo,
			"amount":  actualAmount,
			"tradeNo": tradeNo,
		},
	})

	c.JSON(http.StatusOK, gin.H{
		"success": true, "synced": true, "status": "completed",
		"message": "同步成功，充值已到账",
	})
}

// adminBillingTradeNoForDisplay mirrors `tradeNo || 'N/A'`.
func adminBillingTradeNoForDisplay(tradeNo string) string {
	if tradeNo == "" {
		return "N/A"
	}
	return tradeNo
}

// ==================== POST /api/admin/instances/create ====================

// adminBillingCreateInstanceHandler mirrors POST /api/admin/instances/create: an
// admin opens an instance for another user, either as a paid plan (optionally
// charging the first month) or as a free gift.
//
// The provisioning itself is asynchronous, exactly like the TypeScript: the row is
// inserted with status 'creating' inside one SERIALIZABLE transaction (host
// reservation + 74201 egress lock + optional first-month charge), the response is
// 202, and the node work runs in the background with the existing compensation
// path (routes/instances.go) so a failed creation refunds the charge.
func adminBillingCreateInstanceHandler(c *gin.Context) {
	ctx := c.Request.Context()
	actor := middleware.CurrentUser(c)

	body, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		middleware.AbortFastifyBody(c, status, message)
		return
	}
	object := adminBillingBody(body)

	username, _ := adminBillingBodyString(object, "username")
	name, _ := adminBillingBodyString(object, "name")
	image, _ := adminBillingBodyString(object, "image")
	packageIDValue, packageIDOK := adminBillingBodyNumber(object, "packageId")
	hostIDValue, hostIDOK := adminBillingBodyNumber(object, "hostId")

	if username == nil || *username == "" || name == nil || *name == "" ||
		!packageIDOK || packageIDValue <= 0 || image == nil || *image == "" ||
		!hostIDOK || hostIDValue <= 0 || math.Abs(hostIDValue) > math.MaxInt32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少必填参数"})
		return
	}
	packageID := int32(packageIDValue)
	hostID := int32(hostIDValue)

	nameValidation := lib.ValidateName(*name, "Instance name", 2, 64)
	if !nameValidation.Valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": nameValidation.Message})
		return
	}

	var planID *int32
	if raw, present := object["planId"]; present && raw != nil {
		value, ok := adminBillingBodyNumber(object, "planId")
		if !ok || value != math.Trunc(value) || value <= 0 || value > math.MaxInt32 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "缺少必填参数"})
			return
		}
		plan := int32(value)
		planID = &plan
	}

	chargeFirstMonth := true
	if value, ok := adminBillingBodyBool(object, "chargeFirstMonth"); ok {
		chargeFirstMonth = value
	}
	cpu := int32(15)
	if value, ok := adminBillingBodyInt(object, "cpu"); ok && value > 0 {
		cpu = int32(value)
	}
	memory := int32(128)
	if value, ok := adminBillingBodyInt(object, "memory"); ok && value > 0 {
		memory = int32(value)
	}
	disk := int32(512)
	if value, ok := adminBillingBodyInt(object, "disk"); ok && value > 0 {
		disk = int32(value)
	}
	customInitCommandIDs, _, _, customCommandsValid := adminBillingBodyInt32Array(object, "customInitCommandIds")
	if !customCommandsValid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少必填参数"})
		return
	}

	targetUser, err := db.FindUserByUsername(ctx, *username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}
	if targetUser == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf(`用户 "%s" 不存在`, *username)})
		return
	}
	if string(targetUser.Status) != "active" {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf(`用户 "%s" 状态异常，无法创建实例`, *username)})
		return
	}

	sshKeys, err := db.GetSSHKeysByUserID(ctx, targetUser.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}
	if len(sshKeys) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": fmt.Sprintf(`用户 "%s" 没有 SSH 密钥，请先让用户添加 SSH 密钥`, *username),
		})
		return
	}
	sshKey := sshKeys[0].PublicKey

	pkg, err := db.GetPackageById(ctx, packageID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}
	if pkg == nil || pkg.Active == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "套餐不存在或已禁用"})
		return
	}

	var selectedPlan *models.PackagePlan
	var billing *db.CreateBillingResult
	if planID != nil {
		selectedPlan, err = db.GetPackagePlanById(ctx, *planID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
			return
		}
		if selectedPlan == nil || selectedPlan.PackageID != packageID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "方案不存在或不属于该套餐"})
			return
		}
		if !selectedPlan.IsActive {
			c.JSON(http.StatusBadRequest, gin.H{"error": "方案已下架"})
			return
		}
		if selectedPlan.IsSoldOut {
			c.JSON(http.StatusBadRequest, gin.H{"error": "方案已售罄", "code": "PLAN_SOLD_OUT"})
			return
		}

		quote := db.CalculateCreateBilling(*selectedPlan, time.Now().UTC())
		billing = &quote
		if chargeFirstMonth {
			userBalance, err := db.GetUserBalance(ctx, targetUser.ID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
				return
			}
			if userBalance < billing.TotalPrice {
				c.JSON(http.StatusBadRequest, gin.H{
					"error": fmt.Sprintf("用户余额不足，需要 ¥%s，当前余额 ¥%s",
						adminBillingFixed2(billing.TotalPrice), adminBillingFixed2(userBalance)),
				})
				return
			}
		}
	}

	requestedCPU := cpu
	requestedMemory := memory
	requestedDisk := disk
	if selectedPlan != nil {
		requestedCPU = selectedPlan.CPU
		requestedMemory = selectedPlan.Memory
		requestedDisk = selectedPlan.Disk
	}

	if ok, err := db.IsValidSystemImage(ctx, *image); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	} else if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "镜像不存在"})
		return
	}

	instanceType := pkg.InstanceType
	if instanceType == "" {
		instanceType = "container"
	}
	if ok, err := db.IsImageCompatibleWithInstanceType(ctx, *image, instanceType); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	} else if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "镜像类型与套餐类型不兼容"})
		return
	}
	if ok, err := db.IsImageCompatibleWithMemory(ctx, *image, int(requestedMemory)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	} else if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "128MB内存只能使用 Alpine/Debian 镜像"})
		return
	}

	nodeSelectors, err := instancesLoadNodeSelectors(ctx, packageID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}
	ownerID := pkg.UserID
	preCheckHost, err := db.HostsSelectAvailable(ctx, db.HostsSelectOptions{
		PackageHostIDs: pkg.HostIDs,
		NodeSelectors:  nodeSelectors,
		CPU:            int64(requestedCPU),
		Memory:         int64(requestedMemory),
		Disk:           int64(requestedDisk),
		HostID:         &hostID,
		OwnerID:        &ownerID,
		PackageID:      &packageID,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}
	if preCheckHost == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "指定宿主机不可用"})
		return
	}

	hostInstanceType := preCheckHost.InstanceType
	if hostInstanceType == "" {
		hostInstanceType = "container"
	}
	if instanceType == "vm" && hostInstanceType == "container" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "套餐要求虚拟机，但节点只支持容器"})
		return
	}
	if instanceType == "container" && hostInstanceType == "vm" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "套餐要求容器，但节点只支持虚拟机"})
		return
	}

	availabilityMemory := int(requestedMemory)
	availability, err := db.GetSystemImageAvailabilityForHost(ctx, *image, preCheckHost.ID, db.SystemImageAvailabilityOptions{
		InstanceType: &instanceType,
		Memory:       &availabilityMemory,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}
	if !availability.OK {
		switch availability.Reason {
		case "host_not_found":
			c.JSON(http.StatusNotFound, gin.H{"error": "宿主机不存在"})
		case "image_not_found":
			c.JSON(http.StatusBadRequest, gin.H{"error": "镜像不存在"})
		case "memory_incompatible":
			c.JSON(http.StatusBadRequest, gin.H{"error": "128MB内存只能使用 Alpine/Debian 镜像"})
		case "instance_type_mismatch":
			c.JSON(http.StatusBadRequest, gin.H{"error": "镜像类型与套餐类型不兼容"})
		case "host_instance_type_mismatch":
			c.JSON(http.StatusBadRequest, gin.H{"error": "套餐类型与宿主机不兼容"})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "选择的镜像在当前节点不可用"})
		}
		return
	}

	extraShellCommands := ""
	if len(customInitCommandIDs) > 0 {
		validation, err := db.ValidateCustomInitCommandsOwnership(ctx, customInitCommandIDs, targetUser.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
			return
		}
		if validation.Valid && len(validation.Commands) > 0 {
			distro := db.GetImageDistroFromAlias(*image)
			compatible := make([]db.CustomInitCommandData, 0, len(validation.Commands))
			for _, command := range validation.Commands {
				for _, item := range command.Distros {
					if item == "all" || item == distro {
						compatible = append(compatible, command)
						break
					}
				}
			}
			if len(compatible) > 0 {
				extraShellCommands = db.MergeCustomInitCommandContents(compatible)
			}
		}
	}

	networkMode := string(lib.NormalizeNetworkMode(pkg.NetworkMode))
	if instanceType == "vm" && (networkMode == "nat_ipv6_nat" || networkMode == "ipv6_nat") {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "KVM packages do not support IPv4 NAT & IPv6 NAT or IPv6 NAT network modes",
			"code":  string(lib.ErrInvalidParams),
		})
		return
	}

	autoPassword := lib.GenerateRandomPassword(16)
	generated, err := lib.GenerateIncusConfig(lib.IncusConfigParams{
		InstanceName:       *name,
		ImageAlias:         *image,
		RootPassword:       autoPassword,
		SSHKey:             &sshKey,
		NetworkMode:        networkMode,
		Type:               adminBillingIncusConfigType(instanceType),
		ExtraShellCommands: extraShellCommands,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}

	incusID := fmt.Sprintf("u%d-%s", targetUser.ID, instancesNanoid8())
	rootPasswordEnc, err := lib.EncryptSensitiveData(generated.MetaData.RootPassword)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}

	var packageTrafficLimit *int64
	if pkg.MonthlyTrafficLimit != nil && *pkg.MonthlyTrafficLimit != "" {
		if parsed, err := strconv.ParseInt(*pkg.MonthlyTrafficLimit, 10, 64); err == nil {
			packageTrafficLimit = &parsed
		}
	}

	now := time.Now().UTC()
	snapshotSpecs := map[string]any{
		"packageId":         pkg.ID,
		"packageName":       pkg.Name,
		"planId":            nil,
		"planName":          nil,
		"cpuMax":            pkg.CPUMax,
		"memoryMax":         pkg.MemoryMax,
		"diskMax":           pkg.DiskMax,
		"networkMode":       networkMode,
		"portLimit":         pkg.PortLimit,
		"nested":            pkg.Nested != 0,
		"privileged":        pkg.Privileged != 0,
		"nodeSelectors":     nodeSelectors,
		"createdAt":         adminBillingISOTime(now),
		"adminGift":         true,
		"giftBy":            actor.ID,
		"isPaidInstance":    selectedPlan != nil,
		"chargedFirstMonth": selectedPlan != nil && chargeFirstMonth,
	}
	if selectedPlan != nil {
		snapshotSpecs["planId"] = selectedPlan.ID
		snapshotSpecs["planName"] = selectedPlan.Name
		portLimit := selectedPlan.PortLimit
		if portLimit == 0 {
			portLimit = pkg.PortLimit
			if portLimit == 0 {
				portLimit = 20
			}
		}
		snapshotSpecs["portLimit"] = portLimit
	}
	snapshotJSON, err := json.Marshal(snapshotSpecs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建失败"})
		return
	}

	plan := instancesCreatePlan{
		UserID:              targetUser.ID,
		PackageID:           packageID,
		Package:             pkg,
		Plan:                selectedPlan,
		Name:                *name,
		IncusID:             incusID,
		Image:               *image,
		CPU:                 requestedCPU,
		Memory:              requestedMemory,
		Disk:                requestedDisk,
		NetworkMode:         networkMode,
		InstanceType:        instanceType,
		PackagePlanID:       planID,
		SnapshotSpecs:       snapshotJSON,
		MetaData:            generated.MetaData,
		RootPasswordEnc:     &rootPasswordEnc,
		Billing:             billing,
		PackageTrafficLimit: packageTrafficLimit,
		HostID:              hostID,
	}

	var instanceID int32
	var lockedHost *db.HostsSelectionRecord
	txErr := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Shared policy references must be serialised before the host row locks
		// (single-argument advisory lock 74201, a different lock space).
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", int64(74201)).Error; err != nil {
			return err
		}

		portCount := int32(0)
		if lib.NetworkModeAllowsPortMapping(networkMode) {
			portCount = pkg.PortLimit
			if selectedPlan != nil && selectedPlan.PortLimit != 0 {
				portCount = selectedPlan.PortLimit
			}
		}
		reserved, err := db.HostsSelectAndReserveWithLock(ctx, tx, db.HostsReserveOptions{
			PackageHostIDs: pkg.HostIDs,
			NodeSelectors:  nodeSelectors,
			CPU:            int64(requestedCPU),
			Memory:         int64(requestedMemory),
			Disk:           int64(requestedDisk),
			HostID:         &hostID,
			OwnerID:        &ownerID,
			PackageID:      &packageID,
			PackagePlanID:  planID,
			PortCount:      portCount,
			NetworkMode:    &networkMode,
		})
		if err != nil {
			return err
		}
		if reserved == nil {
			return errAdminBillingHostResourcesInsufficient
		}
		lockedHost = reserved

		baseTrafficLimit := packageTrafficLimit
		if selectedPlan != nil {
			value := selectedPlan.TrafficLimit
			baseTrafficLimit = &value
		}
		effectiveTrafficLimit, err := lib.ResolveInstanceTrafficLimitForHost(ctx, tx, lib.TrafficMultiplierResolveParams{
			PackageID:        &packageID,
			HostID:           reserved.ID,
			BaseTrafficLimit: baseTrafficLimit,
		})
		if err != nil {
			return err
		}

		var portLimit, snapshotLimit, backupLimit, siteLimit *int32
		var swapSize *int32
		var limitsIngress, limitsEgress *string
		if selectedPlan != nil {
			portLimit = instancesInt32Ptr(selectedPlan.PortLimit)
			snapshotLimit = instancesInt32Ptr(selectedPlan.SnapshotLimit)
			backupLimit = instancesInt32Ptr(selectedPlan.BackupLimit)
			siteLimit = instancesInt32Ptr(selectedPlan.SiteLimit)
			if instanceType == "container" && selectedPlan.SwapSize != 0 {
				value := selectedPlan.SwapSize
				swapSize = &value
			}
			speed := selectedPlan.TrafficLimitSpeed
			normalized := services.TrafficBandwidthNormalizePlanTrafficLimitSpeed(&speed)
			if normalized.Invalid {
				return errAdminBillingInvalidPlanTrafficSpeed
			}
			if normalized.Value != nil {
				limitsIngress = normalized.Value
				limitsEgress = normalized.Value
			}
		} else {
			if pkg.PortLimit != 0 {
				portLimit = instancesInt32Ptr(pkg.PortLimit)
			}
			if pkg.SnapshotLimit != 0 {
				snapshotLimit = instancesInt32Ptr(pkg.SnapshotLimit)
			}
			if pkg.BackupLimit != 0 {
				backupLimit = instancesInt32Ptr(pkg.BackupLimit)
			}
			if pkg.SiteLimit != 0 {
				siteLimit = instancesInt32Ptr(pkg.SiteLimit)
			}
		}

		var balanceLogID *int32
		if selectedPlan != nil && billing != nil && chargeFirstMonth {
			result, err := db.InstanceBillingApplyBalanceTx(ctx, tx, db.InstanceBillingBalanceChange{
				UserID:     targetUser.ID,
				InstanceID: 0,
				Delta:      adminBillingDecimal(-billing.TotalPrice),
				LogType:    models.BalanceLogTypeConsume,
				Remark: fmt.Sprintf("管理员开通付费实例（%d个月）：%s - %s",
					billing.BillingCycle, pkg.Name, selectedPlan.Name),
			})
			if err != nil {
				return err
			}
			balanceLogID = &result.BalanceLogID
		}

		binding, err := instancesTxBindPackageEgress(ctx, tx, &packageID, reserved.ID, nil)
		if err != nil {
			return err
		}
		gpuReservation, err := db.GpusReserveInstanceGpu(ctx, tx, db.GpusReserveInput{
			HostID:    reserved.ID,
			PackageID: &packageID,
			PlanID:    planID,
		})
		if err != nil {
			return err
		}

		var expiresAt *time.Time
		var billingPrice *models.Decimal
		var billingCycle *int32
		if billing != nil {
			value := billing.ExpiresAt.UTC()
			expiresAt = &value
			price := adminBillingDecimal(billing.Price)
			billingPrice = &price
			cycle := billing.BillingCycle
			billingCycle = &cycle
		}

		err = tx.Raw(`INSERT INTO instances (
				egress_mode, egress_profile_id, incus_id, name, user_id, host_id,
				storage_pool_name, package_id, package_plan_id, gpu_device_id, image, cpu, memory, disk,
				network_mode, snapshotted_specs, ssh_port, root_password, status,
				monthly_traffic_limit, port_limit, snapshot_limit, backup_limit, site_limit,
				swap_enabled, swap_size, limits_ingress, limits_egress, expires_at, billing_price,
				billing_cycle, auto_renew, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?::jsonb, ?, ?, 'creating',
				?, ?, ?, ?, ?, false, ?, ?, ?, ?, ?, ?, false, ?, ?)
			RETURNING id`,
			binding.EgressMode, binding.EgressProfileID, incusID, *name, targetUser.ID, reserved.ID,
			reserved.StoragePoolName, packageID, selectedPlanIDValue(selectedPlan), gpuReservation.GpuDeviceID,
			*image, requestedCPU, requestedMemory, requestedDisk,
			networkMode, string(snapshotJSON), generated.MetaData.SSHPort, rootPasswordEnc,
			effectiveTrafficLimit, portLimit, snapshotLimit, backupLimit, siteLimit,
			swapSize, limitsIngress, limitsEgress, expiresAt, billingPrice, billingCycle, now, now,
		).Scan(&instanceID).Error
		if err != nil {
			return err
		}

		for _, gpuID := range gpuReservation.GpuDeviceIDs {
			if err := tx.Exec(`INSERT INTO instance_gpu_allocations (instance_id, gpu_device_id)
				VALUES (?, ?) ON CONFLICT DO NOTHING`, instanceID, gpuID).Error; err != nil {
				return err
			}
		}

		if balanceLogID != nil {
			if err := tx.Exec(`UPDATE balance_logs SET instance_id = ? WHERE id = ?`,
				instanceID, *balanceLogID).Error; err != nil {
				return err
			}
		}

		if selectedPlan != nil && billing != nil {
			amount := 0.0
			remark := "管理员赠送首月（未扣费）"
			if chargeFirstMonth {
				amount = billing.TotalPrice
				remark = fmt.Sprintf("管理员开通 %d 个月", billing.BillingCycle)
			}
			record := models.InstanceBillingRecord{
				InstanceID:   instanceID,
				UserID:       targetUser.ID,
				Type:         models.BillingRecordTypeNewPurchase,
				Amount:       adminBillingDecimal(amount),
				Months:       billing.BillingCycle,
				PeriodStart:  now,
				PeriodEnd:    billing.ExpiresAt.UTC(),
				BalanceLogID: balanceLogID,
				Remark:       &remark,
			}
			return tx.WithContext(ctx).Create(&record).Error
		}
		return nil
	}, newSerializableTxOptions())
	if txErr != nil {
		switch {
		case errors.Is(txErr, errAdminBillingHostResourcesInsufficient):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "宿主机资源不足或已被占用"})
		case errors.Is(txErr, errAdminBillingInvalidPlanTrafficSpeed):
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid plan traffic limit speed"})
		case adminBillingIsBalanceInsufficient(txErr):
			c.JSON(http.StatusBadRequest, gin.H{"error": "用户余额不足"})
		case errors.Is(txErr, errAdminBillingBalanceConflict):
			c.JSON(http.StatusConflict, gin.H{"error": "用户余额正在处理，请稍后重试"})
		default:
			var gpuError *db.GpusError
			if errors.As(txErr, &gpuError) {
				c.JSON(http.StatusConflict, gin.H{"error": gpuError.Message, "code": "GPU_UNAVAILABLE"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": txErr.Error()})
		}
		return
	}

	// ---- post-commit: IPs, cloud-init, storage pool, async provisioning ----
	provisioned, ok := adminBillingProvisionAfterCommit(c, &plan, lockedHost, instanceID, &sshKey, extraShellCommands)
	if !ok {
		return
	}

	config := instancesBuildCreateConfig(plan, provisioned, nil)
	go func() {
		if err := instancesCreateInstanceAsync(context.Background(), instanceID, lockedHost, config, targetUser.ID,
			instancesResources{CPU: requestedCPU, Memory: requestedMemory, Disk: requestedDisk}); err != nil {
			fmt.Printf("[Admin Create Instance] 实例 %d 创建失败: %v\n", instanceID, err)
		}
	}()

	logDetail := fmt.Sprintf(`Admin created free instance "%s" for user %s (Package: %s, Free Gift)`,
		*name, targetUser.Username, pkg.Name)
	if selectedPlan != nil {
		charged := "Free first month"
		if chargeFirstMonth {
			charged = "¥" + adminBillingFixed2(billing.TotalPrice)
		}
		logDetail = fmt.Sprintf(`Admin created paid instance "%s" for user %s (Package: %s, Plan: %s, Charged: %s)`,
			*name, targetUser.Username, pkg.Name, selectedPlan.Name, charged)
	}
	db.CreateLog(ctx, adminBillingInt64Ptr(int64(actor.ID)), adminBillingLogModule, "instance.admin_create",
		logDetail, db.LogResultSuccess, db.LogOptionsWithInstance(&instanceID))
	adminBillingAudit(ctx, c, actor, "instance.admin_create", logDetail,
		"instance", int64(instanceID), *name, fmt.Sprintf("packageId=%d planId=%v", packageID, planID))

	resourceInfo := fmt.Sprintf("CPU %d%% | 内存 %s | 磁盘 %s",
		requestedCPU, adminBillingFormatMemory(requestedMemory), adminBillingFormatDisk(requestedDisk))

	notifyTitle := "收到实例赠送"
	notifyContent := fmt.Sprintf("管理员为您创建了一台实例：%s\n套餐：%s\n配置：%s\n节点：%s",
		*name, pkg.Name, resourceInfo, lockedHost.Name)
	eventType := "instance_gift"
	if selectedPlan != nil {
		eventType = "instance_paid_created"
		notifyTitle = "开通付费实例"
		if chargeFirstMonth {
			notifyContent = fmt.Sprintf("管理员为您开通了一台付费实例：%s\n套餐：%s\n方案：%s\n配置：%s\n节点：%s\n扣费：¥%s\n到期时间：%s",
				*name, pkg.Name, selectedPlan.Name, resourceInfo, lockedHost.Name,
				adminBillingFixed2(billing.TotalPrice), adminBillingFormatDate(billing.ExpiresAt))
		} else {
			notifyTitle = "收到付费实例赠送"
			notifyContent = fmt.Sprintf("管理员为您赠送了一台付费实例：%s\n套餐：%s\n方案：%s\n配置：%s\n节点：%s\n首月免费，到期时间：%s\n次月将按方案价格正常计费",
				*name, pkg.Name, selectedPlan.Name, resourceInfo, lockedHost.Name,
				adminBillingFormatDate(billing.ExpiresAt))
		}
	}

	_, _ = db.CreateInboxMessage(ctx, db.CreateInboxMessageData{
		UserID:    targetUser.ID,
		EventType: eventType,
		Title:     notifyTitle,
		Content:   notifyContent,
		Data: gin.H{
			"instanceId":   instanceID,
			"instanceName": *name,
			"packageName":  pkg.Name,
			"planName":     adminBillingPlanName(selectedPlan),
			"cpu":          requestedCPU,
			"memory":       requestedMemory,
			"disk":         requestedDisk,
			"hostName":     lockedHost.Name,
			"isPaid":       selectedPlan != nil,
			"charged":      chargeFirstMonth,
			"amount":       adminBillingBillingTotal(billing),
			"expiresAt":    adminBillingBillingExpiresISO(billing),
		},
	})

	if targetUser.Email != nil && *targetUser.Email != "" {
		planName := ""
		if selectedPlan != nil {
			planName = selectedPlan.Name
		}
		var expiresAt *time.Time
		if billing != nil {
			value := billing.ExpiresAt.UTC()
			expiresAt = &value
		}
		amount := adminBillingBillingTotal(billing)
		_ = lib.SendAdminInstanceCreatedEmail(ctx, *targetUser.Email, lib.AdminInstanceCreatedEmailData{
			Username:     targetUser.Username,
			InstanceName: *name,
			PackageName:  pkg.Name,
			PlanName:     planName,
			HostName:     lockedHost.Name,
			CPU:          float64(requestedCPU),
			Memory:       float64(requestedMemory),
			Disk:         float64(requestedDisk),
			IsPaid:       selectedPlan != nil,
			Charged:      chargeFirstMonth,
			Amount:       &amount,
			ExpiresAt:    expiresAt,
		})
	}

	c.JSON(http.StatusAccepted, gin.H{
		"message": "实例创建中",
		"instance": gin.H{
			"id":           instanceID,
			"name":         *name,
			"incusId":      incusID,
			"host":         lockedHost.Name,
			"status":       "creating",
			"user":         gin.H{"id": targetUser.ID, "username": targetUser.Username},
			"sshPort":      generated.MetaData.SSHPort,
			"rootPassword": generated.MetaData.RootPassword,
			"isPaid":       selectedPlan != nil,
			"planName":     adminBillingPlanName(selectedPlan),
			"charged":      chargeFirstMonth,
			"amount":       adminBillingBillingTotal(billing),
			"expiresAt":    adminBillingBillingExpiresISO(billing),
		},
	})
}

// adminBillingProvisionAfterCommit mirrors the post-transaction block of the
// TypeScript admin create flow. It differs from the user-facing provisioning in one
// documented way: a NAT IPv4 allocation failure is swallowed (console.error in the
// original) instead of aborting, while a missing public IPv4 aborts with 503 and
// unwinds the charge.
func adminBillingProvisionAfterCommit(
	c *gin.Context,
	plan *instancesCreatePlan,
	host *db.HostsSelectionRecord,
	instanceID int32,
	sshKey *string,
	extraShellCommands string,
) (instancesProvisioned, bool) {
	ctx := c.Request.Context()
	provisioned := instancesProvisioned{HostInterface: "eth0", ConfigPayload: lib.IncusConfigPayload{
		UserUserData:      "",
		UserNetworkConfig: "",
	}}

	// The base payload was generated before the transaction with the same inputs.
	basePayload, err := lib.GenerateIncusConfig(lib.IncusConfigParams{
		InstanceName:       plan.Name,
		ImageAlias:         plan.Image,
		RootPassword:       plan.MetaData.RootPassword,
		SSHKey:             sshKey,
		NetworkMode:        plan.NetworkMode,
		Type:               adminBillingIncusConfigType(plan.InstanceType),
		ExtraShellCommands: extraShellCommands,
	})
	if err == nil {
		provisioned.ConfigPayload = basePayload.ConfigPayload
	}

	var staticIPv4, staticIPv6 *string
	var publicAssignment *db.PublicIpv4Assignment

	if lib.NetworkModeNeedsNatIpv4(plan.NetworkMode) {
		// Swallowed exactly like the TypeScript: a failed NAT allocation leaves the
		// instance without a static address but does not abort the creation.
		if allocated, err := db.IpAddressesAllocateAndReserveNatIpv4(ctx, host.ID, &instanceID); err == nil && allocated != nil {
			staticIPv4 = allocated
		}
	} else if lib.NetworkModeNeedsPublicIpv4(plan.NetworkMode) {
		tx, err := db.Pool().Begin(ctx)
		if err != nil {
			adminBillingAbortProvisioning(c, plan, host, instanceID)
			return provisioned, false
		}
		assignment, reserveErr := db.ReservePublicIpv4ForInstance(ctx, tx, db.PublicIpv4ReserveInput{
			HostID:     host.ID,
			InstanceID: instanceID,
		})
		if reserveErr != nil {
			_ = tx.Rollback(ctx)
			adminBillingAbortProvisioning(c, plan, host, instanceID)
			return provisioned, false
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			adminBillingAbortProvisioning(c, plan, host, instanceID)
			return provisioned, false
		}
		if assignment == nil {
			adminBillingAbortProvisioning(c, plan, host, instanceID)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": fmt.Sprintf("宿主机 %s 没有可用独立 IPv4 地址", host.Name),
			})
			return provisioned, false
		}
		publicAssignment = assignment
		staticIPv4 = &assignment.Address
	}

	if host.IPv6Subnet != nil && *host.IPv6Subnet != "" && lib.NetworkModeNeedsRoutedIpv6(plan.NetworkMode) {
		for attempts := 0; attempts < 50; attempts++ {
			candidate, genErr := lib.GenerateRandomIPv6(*host.IPv6Subnet)
			if genErr != nil {
				break
			}
			exists, existsErr := db.IpAddressesExists(ctx, candidate)
			if existsErr != nil {
				break
			}
			if !exists {
				value := candidate
				staticIPv6 = &value
				break
			}
		}
	}

	ipv4CIDR := ""
	if publicAssignment != nil {
		ipv4CIDR = fmt.Sprintf("%s/%d", publicAssignment.Address, publicAssignment.PrefixLength)
	} else if staticIPv4 != nil {
		ipv4CIDR = *staticIPv4 + instancesDefaultNATIPv4CIDR
	}
	ipv4Gateway := instancesDefaultNATIPv4Gateway
	var ipv4DNS []string
	if publicAssignment != nil {
		if publicAssignment.Gateway != "" {
			ipv4Gateway = publicAssignment.Gateway
		}
		if len(publicAssignment.DNS) > 0 {
			ipv4DNS = publicAssignment.DNS
		}
	}
	if len(ipv4DNS) == 0 {
		ipv4DNS = []string{instancesDefaultNATIPv4Gateway}
	}

	if plan.InstanceType == "vm" {
		var network *lib.IncusConfigStaticNetwork
		if staticIPv4 != nil && ipv4CIDR != "" {
			network = &lib.IncusConfigStaticNetwork{
				IPAddress: ipv4CIDR,
				Gateway:   ipv4Gateway,
				DNS:       ipv4DNS,
			}
			if staticIPv6 != nil {
				network.IPv6Address = *staticIPv6
				if host.IPv6Gateway != nil {
					network.IPv6Gateway = *host.IPv6Gateway
				}
			}
		}
		vmPayload, vmErr := lib.IncusConfigGenerateVM(lib.IncusConfigVMParams{
			InstanceName:       plan.Name,
			InstanceIDSeed:     plan.IncusID,
			ImageAlias:         plan.Image,
			RootPassword:       plan.MetaData.RootPassword,
			SSHKey:             sshKey,
			Network:            network,
			ExtraShellCommands: extraShellCommands,
			NetworkMode:        plan.NetworkMode,
		})
		if vmErr == nil {
			provisioned.ConfigPayload = vmPayload
		}
	} else if staticIPv4 != nil && ipv4CIDR != "" {
		network := &lib.IncusConfigStaticNetwork{
			IPAddress: ipv4CIDR,
			Gateway:   ipv4Gateway,
			DNS:       ipv4DNS,
		}
		if staticIPv6 != nil {
			network.IPv6Address = *staticIPv6 + "/128"
			network.IPv6Gateway = "fe80::1"
		}
		containerResult, genErr := lib.GenerateIncusConfig(lib.IncusConfigParams{
			InstanceName:       plan.Name,
			ImageAlias:         plan.Image,
			RootPassword:       plan.MetaData.RootPassword,
			SSHKey:             sshKey,
			NetworkMode:        plan.NetworkMode,
			Type:               "container",
			Network:            network,
			ExtraShellCommands: extraShellCommands,
		})
		if genErr == nil {
			provisioned.ConfigPayload = containerResult.ConfigPayload
		}
	}

	if host.StoragePoolName == nil || *host.StoragePoolName == "" {
		adminBillingAbortProvisioning(c, plan, host, instanceID)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": fmt.Sprintf("STORAGE_POOL_UNAVAILABLE: 宿主机 %s 未配置可用于实例系统盘的存储池", host.Name),
		})
		return provisioned, false
	}
	provisioned.StoragePool = host.StoragePoolName
	if err := db.DB.WithContext(ctx).Exec(
		`UPDATE instances SET storage_pool_name = ?, updated_at = ? WHERE id = ?`,
		provisioned.StoragePool, time.Now().UTC(), instanceID,
	).Error; err != nil {
		adminBillingAbortProvisioning(c, plan, host, instanceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return provisioned, false
	}

	provisioned.IPv4 = staticIPv4
	provisioned.IPv6 = staticIPv6
	provisioned.PublicIPv4 = publicAssignment
	if host.IPv6Gateway != nil {
		provisioned.IPv6Gateway = host.IPv6Gateway
	}
	if host.IPv6ParentInterface != nil && *host.IPv6ParentInterface != "" {
		provisioned.HostInterface = *host.IPv6ParentInterface
	}
	return provisioned, true
}

// adminBillingAbortProvisioning mirrors the compensation block of the TypeScript
// catch: mark the row error, release the reserved resources and refund the charge.
func adminBillingAbortProvisioning(c *gin.Context, plan *instancesCreatePlan, host *db.HostsSelectionRecord, instanceID int32) {
	ctx := c.Request.Context()
	portLimit := plan.Package.PortLimit
	if plan.Plan != nil && plan.Plan.PortLimit != 0 {
		portLimit = plan.Plan.PortLimit
	}
	if !lib.NetworkModeAllowsPortMapping(plan.NetworkMode) {
		portLimit = 0
	}
	instancesCompensateFailedCreation(ctx, instancesCompensationInput{
		InstanceID:   instanceID,
		HostID:       host.ID,
		UserID:       plan.UserID,
		Resources:    instancesResources{CPU: plan.CPU, Memory: plan.Memory, Disk: plan.Disk},
		NetworkMode:  plan.NetworkMode,
		PortLimit:    portLimit,
		InstanceName: plan.IncusID,
		ErrorMessage: "管理员创建实例失败，已自动回滚",
	})
	_ = db.GpusReleaseFailed(ctx, instanceID)
}

// adminBillingFormatMemory mirrors the notify formatter (MB above 1024 → GB).
func adminBillingFormatMemory(mb int32) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

// adminBillingFormatDisk mirrors the notify formatter (`toFixed(0)` for GB).
func adminBillingFormatDisk(mb int32) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.0f GB", float64(mb)/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}

// adminBillingFormatDate mirrors `date.toLocaleDateString()` for the notify text.
// The process runs with TZ=Asia/Shanghai in production and the parity harness; the
// business zone is used explicitly so the string never depends on the host TZ.
func adminBillingFormatDate(value time.Time) string {
	location, err := time.LoadLocation(lib.BusinessTimezone)
	if err != nil {
		location = time.UTC
	}
	return value.In(location).Format("2006/1/2")
}

func adminBillingPlanName(plan *models.PackagePlan) any {
	if plan == nil {
		return nil
	}
	return plan.Name
}

func adminBillingBillingTotal(billing *db.CreateBillingResult) float64 {
	if billing == nil {
		return 0
	}
	return billing.TotalPrice
}

func adminBillingBillingExpiresISO(billing *db.CreateBillingResult) any {
	if billing == nil {
		return nil
	}
	return adminBillingISOTime(billing.ExpiresAt.UTC())
}

// adminBillingIncusConfigType mirrors `type: effectiveInstanceType === 'vm' ?
// 'virtual-machine' : 'container'`.
func adminBillingIncusConfigType(instanceType string) string {
	if instanceType == "vm" {
		return "virtual-machine"
	}
	return "container"
}

func selectedPlanIDValue(plan *models.PackagePlan) *int32 {
	if plan == nil {
		return nil
	}
	value := plan.ID
	return &value
}

// errAdminBillingHostResourcesInsufficient mirrors the HOST_RESOURCES_INSUFFICIENT
// sentinel of the create transaction.
var errAdminBillingHostResourcesInsufficient = errors.New("HOST_RESOURCES_INSUFFICIENT")

// errAdminBillingInvalidPlanTrafficSpeed mirrors the `undefined` return of
// normalizePlanTrafficLimitSpeed that the create route turns into a 400.
var errAdminBillingInvalidPlanTrafficSpeed = errors.New("Invalid plan traffic limit speed")

// newSerializableTxOptions mirrors `{isolationLevel: Serializable, timeout: 15000}`.
func newSerializableTxOptions() *sql.TxOptions {
	return &sql.TxOptions{Isolation: sql.LevelSerializable}
}

// ==================== route-level rate limiting ====================

// adminBillingLimiterStore is the process-local fixed-window store for the 13
// inline `config: { rateLimit }` declarations of admin-billing.ts. Those limits are
// enforced by Node (they are part of the route definition, unlike the rules
// injected from config/rate-limit.ts), so every one of them must be reproduced:
// keyed by ip + method + route pattern, like @fastify/rate-limit.
type adminBillingLimiterStore struct {
	mu      sync.Mutex
	buckets map[string]adminBillingLimiterBucket
}

type adminBillingLimiterBucket struct {
	count   int
	resetAt time.Time
}

var adminBillingLimiter = &adminBillingLimiterStore{buckets: map[string]adminBillingLimiterBucket{}}

// adminBillingRateLimit builds the middleware for one route.
func adminBillingRateLimit(route string, max int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := middleware.ClientIP(c) + ":" + c.Request.Method + ":" + route
		now := time.Now()

		adminBillingLimiter.mu.Lock()
		bucket, ok := adminBillingLimiter.buckets[key]
		if !ok || now.After(bucket.resetAt) {
			bucket = adminBillingLimiterBucket{count: 0, resetAt: now.Add(window)}
		}
		bucket.count++
		allowed := bucket.count <= max
		adminBillingLimiter.buckets[key] = bucket
		adminBillingLimiter.mu.Unlock()

		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "Too many requests, please try again later",
			})
			return
		}
		c.Next()
	}
}

// ResetAdminBillingRateLimits clears the process-local buckets (tests only).
func ResetAdminBillingRateLimits() {
	adminBillingLimiter.mu.Lock()
	adminBillingLimiter.buckets = map[string]adminBillingLimiterBucket{}
	adminBillingLimiter.mu.Unlock()
}

// ==================== route registration ====================

// RegisterAdminBilling mounts the 22 admin-billing routes on their ABSOLUTE paths
// (admin-billing.ts declares them at the root, not under a prefix).
func RegisterAdminBilling(r *gin.Engine) {
	admin := r.Group("/api/admin")
	adminAuth := []gin.HandlerFunc{middleware.Authenticate(), middleware.RequireAdmin()}

	// ---- billing reads (no inline limit in the TypeScript) ----
	billing := admin.Group("/billing", adminAuth...)
	billing.GET("/overview", adminBillingOverviewHandler)
	billing.GET("/records", adminBillingRecordsHandler)
	billing.GET("/reconciliation", adminBillingReconciliationHandler)
	billing.POST("/reconciliation/run", adminBillingReconciliationRunHandler)
	billing.PATCH("/reconciliation/items/:id", adminBillingReconciliationItemHandler)
	billing.GET("/reconciliation/export", adminBillingReconciliationExportHandler)
	billing.GET("/instances", adminBillingInstancesHandler)
	billing.GET("/recharge-records", adminBillingRechargeRecordsHandler)
	billing.POST("/recharge-records/:id/sync",
		adminBillingRateLimit("/api/admin/billing/recharge-records/:id/sync", 10, time.Minute),
		adminBillingSyncRechargeHandler)

	// ---- instance operations (13 inline limits in total, 9 of them here) ----
	instances := admin.Group("/instances", adminAuth...)
	instances.POST("/:id/suspend",
		adminBillingRateLimit("/api/admin/instances/:id/suspend", 20, time.Minute),
		adminBillingSuspendHandler)
	instances.POST("/:id/unsuspend",
		adminBillingRateLimit("/api/admin/instances/:id/unsuspend", 20, time.Minute),
		adminBillingUnsuspendHandler)
	instances.POST("/:id/extend",
		adminBillingRateLimit("/api/admin/instances/:id/extend", 10, time.Minute),
		adminBillingExtendHandler)
	instances.POST("/:id/refund",
		adminBillingRateLimit("/api/admin/instances/:id/refund", 10, time.Minute),
		adminBillingRefundHandler)
	instances.POST("/:id/delete-and-refund",
		adminBillingRateLimit("/api/admin/instances/:id/delete-and-refund", 10, time.Minute),
		adminBillingDeleteAndRefundHandler)
	instances.POST("/:id/apply-aff",
		adminBillingRateLimit("/api/admin/instances/:id/apply-aff", 20, time.Minute),
		adminBillingApplyAffHandler)
	instances.POST("/:id/update-price",
		adminBillingRateLimit("/api/admin/instances/:id/update-price", 20, time.Minute),
		adminBillingUpdatePriceHandler)
	instances.POST("/:id/update-price/preview",
		adminBillingRateLimit("/api/admin/instances/:id/update-price/preview", 30, time.Minute),
		adminBillingUpdatePricePreviewHandler)
	instances.POST("/batch-update-price/preview",
		adminBillingRateLimit("/api/admin/instances/batch-update-price/preview", 20, time.Minute),
		adminBillingBatchUpdatePricePreviewHandler)
	instances.POST("/batch-update-price",
		adminBillingRateLimit("/api/admin/instances/batch-update-price", 10, time.Minute),
		adminBillingBatchUpdatePriceHandler)
	instances.POST("/create",
		adminBillingRateLimit("/api/admin/instances/create", 10, time.Minute),
		adminBillingCreateInstanceHandler)
	instances.GET("/:id/available-plans", adminBillingAvailablePlansHandler)
	instances.POST("/:id/upgrade-plan",
		adminBillingRateLimit("/api/admin/instances/:id/upgrade-plan", 10, time.Minute),
		adminBillingUpgradePlanHandler)
}

func init() {
	Register(RegisterAdminBilling)
}
