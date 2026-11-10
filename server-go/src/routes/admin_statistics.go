// Port of server/src/routes/admin-statistics.ts.
//
// One endpoint: GET /api/admin/statistics/overview. The Node handler fans out
// ~58 independent queries through a single Promise.all; the Go port preserves
// that shape by starting every query in its own goroutine and failing the whole
// response on the first error (which is all Promise.all's contract reduces to).
//
// Two pieces of arithmetic are load-bearing and deliberately hand-written:
//
//   - The business-day / business-month windows. The TS builds them from
//     Date.UTC + a fixed 480-minute offset (UTC+8), not from the process
//     timezone, and the SQL buckets with `interval '8 hours'` for the same
//     reason. Every constant below keeps that fixed offset.
//   - roundMoney = Number(value.toFixed(2)). ECMAScript rounds the exact binary
//     value of the double with ties away from zero, so 1.005 -> 1, 2.675 ->
//     2.67, -1.005 -> -1 and 0.125 -> 0.13. math.Round is not equivalent.
//
// The raw SQL is copied verbatim from the TS source: the to_char bucketing and
// the COALESCE(completed_at, created_at) revenue attribution are the contract.
package routes

import (
	"context"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// Constants mirror admin-statistics.ts. They are named (not inlined) because the
// lookback windows are part of the response contract (meta.dailyDays /
// meta.monthlyMonths) and are asserted by the unit tests.
const (
	adminStatisticsBusinessTZOffsetMinutes = 8 * 60
	adminStatisticsDayMilliseconds         = 24 * 60 * 60 * 1000
	adminStatisticsDailyDays               = 30
	adminStatisticsMonthlyMonths           = 12
	adminStatisticsAgentStaleMinutes       = 30
	adminStatisticsTaskLookbackDays        = 1
	adminStatisticsRiskLookbackDays        = 7
)

// adminStatisticsDayDuration is one business day. Windows are built by adding
// whole days to the fixed-offset day start, exactly like the TS DAY_MS math.
const adminStatisticsDayDuration = time.Duration(adminStatisticsDayMilliseconds) * time.Millisecond

// adminStatisticsBusinessTZOffset is BUSINESS_TZ_OFFSET_MINUTES as a duration.
const adminStatisticsBusinessTZOffset = time.Duration(adminStatisticsBusinessTZOffsetMinutes) * time.Minute

// ==================== wire types ====================

// adminStatisticsSeriesPoint mirrors SeriesPoint.
type adminStatisticsSeriesPoint struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

// adminStatisticsOperationRisk mirrors OperationRisk. Severity is one of
// "info" | "warning" | "critical"; only "warning" and "critical" are produced.
type adminStatisticsOperationRisk struct {
	Key      string `json:"key"`
	Severity string `json:"severity"`
	Count    int64  `json:"count"`
}

type adminStatisticsMeta struct {
	Timezone      string `json:"timezone"`
	DailyDays     int    `json:"dailyDays"`
	MonthlyMonths int    `json:"monthlyMonths"`
}

type adminStatisticsUsersSection struct {
	Total           int64                        `json:"total"`
	DailyNewUsers   []adminStatisticsSeriesPoint `json:"dailyNewUsers"`
	MonthlyNewUsers []adminStatisticsSeriesPoint `json:"monthlyNewUsers"`
}

type adminStatisticsInstancesSection struct {
	Total                   int64                        `json:"total"`
	Active                  int64                        `json:"active"`
	Paid                    int64                        `json:"paid"`
	Free                    int64                        `json:"free"`
	DailyCreatedInstances   []adminStatisticsSeriesPoint `json:"dailyCreatedInstances"`
	MonthlyCreatedInstances []adminStatisticsSeriesPoint `json:"monthlyCreatedInstances"`
}

type adminStatisticsBillingTotals struct {
	Recharge   float64 `json:"recharge"`
	Consume    float64 `json:"consume"`
	Aff        float64 `json:"aff"`
	DestroyFee float64 `json:"destroyFee"`
}

type adminStatisticsBillingSection struct {
	Totals            adminStatisticsBillingTotals `json:"totals"`
	DailyRecharge     []adminStatisticsSeriesPoint `json:"dailyRecharge"`
	MonthlyRecharge   []adminStatisticsSeriesPoint `json:"monthlyRecharge"`
	DailyConsume      []adminStatisticsSeriesPoint `json:"dailyConsume"`
	MonthlyConsume    []adminStatisticsSeriesPoint `json:"monthlyConsume"`
	DailyAff          []adminStatisticsSeriesPoint `json:"dailyAff"`
	MonthlyAff        []adminStatisticsSeriesPoint `json:"monthlyAff"`
	DailyDestroyFee   []adminStatisticsSeriesPoint `json:"dailyDestroyFee"`
	MonthlyDestroyFee []adminStatisticsSeriesPoint `json:"monthlyDestroyFee"`
}

type adminStatisticsRevenueBlock struct {
	Today      float64 `json:"today"`
	Yesterday  float64 `json:"yesterday"`
	Last7Days  float64 `json:"last7Days"`
	Last30Days float64 `json:"last30Days"`
}

type adminStatisticsOrdersBlock struct {
	TodayTotal     int64 `json:"todayTotal"`
	TodaySuccess   int64 `json:"todaySuccess"`
	TodayFailed    int64 `json:"todayFailed"`
	TodayPending   int64 `json:"todayPending"`
	NeedsAttention int64 `json:"needsAttention"`
}

type adminStatisticsOperationsUsersBlock struct {
	NewToday    int64 `json:"newToday"`
	ActiveToday int64 `json:"activeToday"`
	PaidTotal   int64 `json:"paidTotal"`
}

type adminStatisticsOperationsInstancesBlock struct {
	NewToday     int64 `json:"newToday"`
	Running      int64 `json:"running"`
	Abnormal     int64 `json:"abnormal"`
	ExpiringSoon int64 `json:"expiringSoon"`
}

type adminStatisticsDeliveryBlock struct {
	PendingTasks   int64 `json:"pendingTasks"`
	FailedTasks24h int64 `json:"failedTasks24h"`
}

type adminStatisticsInfrastructureBlock struct {
	HostsTotal   int64 `json:"hostsTotal"`
	HostsOnline  int64 `json:"hostsOnline"`
	AgentsOnline int64 `json:"agentsOnline"`
	AgentsStale  int64 `json:"agentsStale"`
}

type adminStatisticsSupportBlock struct {
	OpenTickets            int64 `json:"openTickets"`
	UnreadInboxMessages    int64 `json:"unreadInboxMessages"`
	FailedNotifications24h int64 `json:"failedNotifications24h"`
	FailedEmails24h        int64 `json:"failedEmails24h"`
}

type adminStatisticsOperationsSection struct {
	Revenue        adminStatisticsRevenueBlock             `json:"revenue"`
	Orders         adminStatisticsOrdersBlock              `json:"orders"`
	Users          adminStatisticsOperationsUsersBlock     `json:"users"`
	Instances      adminStatisticsOperationsInstancesBlock `json:"instances"`
	Delivery       adminStatisticsDeliveryBlock            `json:"delivery"`
	Infrastructure adminStatisticsInfrastructureBlock      `json:"infrastructure"`
	Support        adminStatisticsSupportBlock             `json:"support"`
	Risks          []adminStatisticsOperationRisk          `json:"risks"`
}

type adminStatisticsLotteryItem struct {
	ID         int32   `json:"id"`
	Name       string  `json:"name"`
	CostPoints int32   `json:"costPoints"`
	TotalDraws int32   `json:"totalDraws"`
	StartAt    *string `json:"startAt"`
	EndAt      *string `json:"endAt"`
}

type adminStatisticsCheckinBlock struct {
	Enabled         bool  `json:"enabled"`
	MinPoints       int   `json:"minPoints"`
	MaxPoints       int   `json:"maxPoints"`
	RequireInstance bool  `json:"requireInstance"`
	TodayCheckins   int64 `json:"todayCheckins"`
}

type adminStatisticsMarketingSection struct {
	ActiveLotteries      []adminStatisticsLotteryItem `json:"activeLotteries"`
	TotalActiveLotteries int64                        `json:"totalActiveLotteries"`
	Checkin              adminStatisticsCheckinBlock  `json:"checkin"`
}

type adminStatisticsInstanceUser struct {
	ID       int32   `json:"id"`
	Username string  `json:"username"`
	Email    *string `json:"email"`
}

type adminStatisticsInstanceHost struct {
	ID          int32  `json:"id"`
	Name        string `json:"name"`
	CountryCode string `json:"countryCode"`
}

type adminStatisticsPackagePlan struct {
	Name   string `json:"name"`
	CPU    int32  `json:"cpu"`
	Memory int32  `json:"memory"`
	Disk   int32  `json:"disk"`
}

type adminStatisticsRecentInstance struct {
	ID          int32                       `json:"id"`
	Name        string                      `json:"name"`
	Status      string                      `json:"status"`
	CreatedAt   string                      `json:"createdAt"`
	User        adminStatisticsInstanceUser `json:"user"`
	Host        adminStatisticsInstanceHost `json:"host"`
	PackagePlan *adminStatisticsPackagePlan `json:"packagePlan"`
}

type adminStatisticsTicketUser struct {
	ID       int32  `json:"id"`
	Username string `json:"username"`
}

type adminStatisticsTicketHost struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

type adminStatisticsTicketInstance struct {
	ID   int32  `json:"id"`
	Name string `json:"name"`
}

type adminStatisticsPendingTicket struct {
	ID        int32                          `json:"id"`
	Subject   string                         `json:"subject"`
	Category  string                         `json:"category"`
	Priority  string                         `json:"priority"`
	Status    string                         `json:"status"`
	CreatedAt string                         `json:"createdAt"`
	User      adminStatisticsTicketUser      `json:"user"`
	Host      *adminStatisticsTicketHost     `json:"host"`
	Instance  *adminStatisticsTicketInstance `json:"instance"`
}

type adminStatisticsOverviewResponse struct {
	Meta            adminStatisticsMeta              `json:"meta"`
	Users           adminStatisticsUsersSection      `json:"users"`
	Instances       adminStatisticsInstancesSection  `json:"instances"`
	Billing         adminStatisticsBillingSection    `json:"billing"`
	Operations      adminStatisticsOperationsSection `json:"operations"`
	Marketing       adminStatisticsMarketingSection  `json:"marketing"`
	RecentInstances []adminStatisticsRecentInstance  `json:"recentInstances"`
	PendingTickets  []adminStatisticsPendingTicket   `json:"pendingTickets"`
}

// ==================== windows ====================

// adminStatisticsWindow mirrors { start, end, labels } from getDailyWindow /
// getMonthlyWindow. start/end are UTC instants; labels are YYYY-MM-DD or YYYY-MM
// in the fixed UTC+8 business day.
type adminStatisticsWindow struct {
	Start  time.Time
	End    time.Time
	Labels []string
}

// adminStatisticsDateRange mirrors { start, end }.
type adminStatisticsDateRange struct {
	Start time.Time
	End   time.Time
}

// adminStatisticsBusinessDayStartUTC mirrors getBusinessDayStartUtc: the current
// business day's 00:00 (UTC+8) as a UTC instant.
//
// The date label comes from the IANA zone (Asia/Shanghai, no DST) and the
// midnight is then shifted by the fixed 480-minute offset, exactly like the TS.
func adminStatisticsBusinessDayStartUTCAt(now time.Time) time.Time {
	label := lib.GetDateStringInTimezone(now, lib.BusinessTimezone)
	parsed, err := time.ParseInLocation("2006-01-02", label, time.UTC)
	if err != nil {
		// Unreachable: the label is produced by Format("2006-01-02").
		return time.Time{}
	}
	return parsed.Add(-adminStatisticsBusinessTZOffset)
}

// adminStatisticsBusinessMonthStartUTC mirrors getBusinessMonthStartUtc.
// Out-of-range months normalise exactly like Date.UTC (month 0 = December of the
// previous year, month 13 = January of the next one), which time.Date also does.
func adminStatisticsBusinessMonthStartUTC(year, month int) time.Time {
	return time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC).Add(-adminStatisticsBusinessTZOffset)
}

// adminStatisticsFormatBusinessDateFromStart mirrors formatBusinessDateFromStart.
func adminStatisticsFormatBusinessDateFromStart(startUTC time.Time) string {
	return startUTC.Add(adminStatisticsBusinessTZOffset).UTC().Format("2006-01-02")
}

// adminStatisticsFormatBusinessMonthFromStart mirrors formatBusinessMonthFromStart.
func adminStatisticsFormatBusinessMonthFromStart(startUTC time.Time) string {
	return startUTC.Add(adminStatisticsBusinessTZOffset).UTC().Format("2006-01")
}

// adminStatisticsParseYearMonth mirrors parseYearMonth.
func adminStatisticsParseYearMonth(label string) (int, int) {
	parts := strings.Split(label, "-")
	if len(parts) < 2 {
		return 0, 0
	}
	year, _ := strconv.Atoi(parts[0])
	month, _ := strconv.Atoi(parts[1])
	return year, month
}

// adminStatisticsDailyWindowAt mirrors getDailyWindow.
func adminStatisticsDailyWindowAt(days int, now time.Time) adminStatisticsWindow {
	todayStart := adminStatisticsBusinessDayStartUTCAt(now)
	start := todayStart.Add(-time.Duration(days-1) * adminStatisticsDayDuration)
	end := todayStart.Add(adminStatisticsDayDuration)

	labels := make([]string, 0, days)
	for index := 0; index < days; index++ {
		labels = append(labels, adminStatisticsFormatBusinessDateFromStart(
			start.Add(time.Duration(index)*adminStatisticsDayDuration),
		))
	}
	return adminStatisticsWindow{Start: start, End: end, Labels: labels}
}

// adminStatisticsDailyWindow mirrors getDailyWindow with new Date() as the clock.
func adminStatisticsDailyWindow(days int) adminStatisticsWindow {
	return adminStatisticsDailyWindowAt(days, time.Now().UTC())
}

// adminStatisticsMonthlyWindowAt mirrors getMonthlyWindow.
func adminStatisticsMonthlyWindowAt(months int, now time.Time) adminStatisticsWindow {
	currentMonth := lib.GetMonthStringInTimezone(now, lib.BusinessTimezone)
	year, month := adminStatisticsParseYearMonth(currentMonth)

	start := adminStatisticsBusinessMonthStartUTC(year, month-months+1)
	end := adminStatisticsBusinessMonthStartUTC(year, month+1)

	labels := make([]string, 0, months)
	for index := 0; index < months; index++ {
		labels = append(labels, adminStatisticsFormatBusinessMonthFromStart(
			adminStatisticsBusinessMonthStartUTC(year, month-months+1+index),
		))
	}
	return adminStatisticsWindow{Start: start, End: end, Labels: labels}
}

// adminStatisticsMonthlyWindow mirrors getMonthlyWindow with new Date() as the clock.
func adminStatisticsMonthlyWindow(months int) adminStatisticsWindow {
	return adminStatisticsMonthlyWindowAt(months, time.Now().UTC())
}

// adminStatisticsBusinessDayRangeAt mirrors getBusinessDayRange.
func adminStatisticsBusinessDayRangeAt(offsetDays int, now time.Time) adminStatisticsDateRange {
	todayStart := adminStatisticsBusinessDayStartUTCAt(now)
	start := todayStart.Add(time.Duration(offsetDays) * adminStatisticsDayDuration)
	return adminStatisticsDateRange{Start: start, End: start.Add(adminStatisticsDayDuration)}
}

// adminStatisticsBusinessDayRange mirrors getBusinessDayRange(offsetDays).
func adminStatisticsBusinessDayRange(offsetDays int) adminStatisticsDateRange {
	return adminStatisticsBusinessDayRangeAt(offsetDays, time.Now().UTC())
}

// adminStatisticsBusinessRollingRangeAt mirrors getBusinessRollingRange.
func adminStatisticsBusinessRollingRangeAt(days int, now time.Time) adminStatisticsDateRange {
	todayStart := adminStatisticsBusinessDayStartUTCAt(now)
	return adminStatisticsDateRange{
		Start: todayStart.Add(-time.Duration(days-1) * adminStatisticsDayDuration),
		End:   todayStart.Add(adminStatisticsDayDuration),
	}
}

// adminStatisticsBusinessRollingRange mirrors getBusinessRollingRange(days).
func adminStatisticsBusinessRollingRange(days int) adminStatisticsDateRange {
	return adminStatisticsBusinessRollingRangeAt(days, time.Now().UTC())
}

// ==================== numeric helpers ====================

// adminStatisticsDecimalToNumber mirrors toNumber for the decimal text Postgres
// returns for a ::numeric column (Prisma hands the TS code a Decimal object that
// it stringifies and parseFloat's; pgx hands Go the same decimal text).
func adminStatisticsDecimalToNumber(value models.Decimal) float64 {
	parsed, err := strconv.ParseFloat(value.String(), 64)
	if err != nil || math.IsNaN(parsed) {
		return 0
	}
	return parsed
}

// adminStatisticsRoundMoney mirrors roundMoney: Number(value.toFixed(2)).
//
// V8's toFixed rounds the exact binary value of the double with ties away from
// zero -- (0.5).toFixed(0) === "1", (-0.125).toFixed(2) === "-0.13",
// (2.675).toFixed(2) === "2.67", (1.005).toFixed(2) === "1.00". big.Rat exposes
// that exact value, so the result matches V8 for every finite input. The final
// zero normalisation matches JSON.stringify, which prints -0 as "0".
func adminStatisticsRoundMoney(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) >= 1e21 {
		// toFixed falls back to ToString above 1e21, and Number(ToString(x)) is x.
		return value
	}

	exact := new(big.Rat).SetFloat64(value)
	if exact == nil {
		return value
	}
	exact.Mul(exact, big.NewRat(100, 1))

	// Truncate toward zero, then round away from zero when the remainder is at
	// least half a cent.
	quotient, remainder := new(big.Int).QuoRem(exact.Num(), exact.Denom(), new(big.Int))
	twiceRemainder := new(big.Int).Abs(remainder)
	twiceRemainder.Lsh(twiceRemainder, 1)
	if twiceRemainder.Cmp(exact.Denom()) >= 0 {
		if exact.Num().Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	}

	rounded, _ := new(big.Rat).SetFrac(quotient, big.NewInt(100)).Float64()
	if rounded == 0 {
		return 0
	}
	return rounded
}

// adminStatisticsBucketRow is one (bucket, value) row of a grouped query.
type adminStatisticsBucketRow struct {
	Bucket string
	Value  float64
}

// adminStatisticsFillSeries mirrors fillSeries: labels drive the output order and
// missing buckets become 0.
func adminStatisticsFillSeries(rows []adminStatisticsBucketRow, labels []string, money bool) []adminStatisticsSeriesPoint {
	rowMap := make(map[string]float64, len(rows))
	for _, row := range rows {
		rowMap[row.Bucket] = row.Value
	}

	points := make([]adminStatisticsSeriesPoint, 0, len(labels))
	for _, label := range labels {
		value := rowMap[label]
		if money {
			value = adminStatisticsRoundMoney(value)
		}
		points = append(points, adminStatisticsSeriesPoint{Label: label, Value: value})
	}
	return points
}

// adminStatisticsScalarValue mirrors scalarValue(rows, money).
func adminStatisticsScalarValue(value float64, money bool) float64 {
	if money {
		return adminStatisticsRoundMoney(value)
	}
	return value
}

// adminStatisticsAddRisk mirrors addRisk.
func adminStatisticsAddRisk(risks *[]adminStatisticsOperationRisk, condition bool, key, severity string, count int64) {
	if condition {
		*risks = append(*risks, adminStatisticsOperationRisk{Key: key, Severity: severity, Count: count})
	}
}

// adminStatisticsISOTimestamp renders an instant the way Date#toISOString does.
func adminStatisticsISOTimestamp(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// adminStatisticsOptionalISOTimestamp mirrors `value ? value.toISOString() : null`.
func adminStatisticsOptionalISOTimestamp(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := adminStatisticsISOTimestamp(*value)
	return &formatted
}

// ==================== query helpers ====================

// adminStatisticsBatch mirrors Promise.all: every query starts immediately and
// the first error fails the handler.
type adminStatisticsBatch struct {
	wg       sync.WaitGroup
	mu       sync.Mutex
	firstErr error
}

func (batch *adminStatisticsBatch) run(query func() error) {
	batch.wg.Add(1)
	go func() {
		defer batch.wg.Done()
		if err := query(); err != nil {
			batch.mu.Lock()
			if batch.firstErr == nil {
				batch.firstErr = err
			}
			batch.mu.Unlock()
		}
	}()
}

func (batch *adminStatisticsBatch) wait() error {
	batch.wg.Wait()
	batch.mu.Lock()
	defer batch.mu.Unlock()
	return batch.firstErr
}

// adminStatisticsCount runs one count query built by the caller.
func adminStatisticsCount(query *gorm.DB) (int64, error) {
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

// adminStatisticsMoneyScalar mirrors scalarValue(rows, true) over a SUM query.
func adminStatisticsMoneyScalar(ctx context.Context, sql string, args ...any) (float64, error) {
	var row struct {
		Value models.Decimal `gorm:"column:value"`
	}
	if err := db.DB.WithContext(ctx).Raw(sql, args...).Scan(&row).Error; err != nil {
		return 0, err
	}
	return adminStatisticsDecimalToNumber(row.Value), nil
}

// adminStatisticsCountScalar mirrors scalarValue(rows) over a COUNT query.
func adminStatisticsCountScalar(ctx context.Context, sql string, args ...any) (int64, error) {
	var row struct {
		Value int64 `gorm:"column:value"`
	}
	if err := db.DB.WithContext(ctx).Raw(sql, args...).Scan(&row).Error; err != nil {
		return 0, err
	}
	return row.Value, nil
}

// adminStatisticsCountBuckets reads a `bucket, COUNT(*)::int` grouped query.
func adminStatisticsCountBuckets(ctx context.Context, sql string, args ...any) ([]adminStatisticsBucketRow, error) {
	var rows []struct {
		Bucket string `gorm:"column:bucket"`
		Value  int64  `gorm:"column:value"`
	}
	if err := db.DB.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}

	result := make([]adminStatisticsBucketRow, 0, len(rows))
	for _, row := range rows {
		result = append(result, adminStatisticsBucketRow{Bucket: row.Bucket, Value: float64(row.Value)})
	}
	return result, nil
}

// adminStatisticsMoneyBuckets reads a `bucket, SUM(...)::numeric` grouped query.
func adminStatisticsMoneyBuckets(ctx context.Context, sql string, args ...any) ([]adminStatisticsBucketRow, error) {
	var rows []struct {
		Bucket string         `gorm:"column:bucket"`
		Value  models.Decimal `gorm:"column:value"`
	}
	if err := db.DB.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}

	result := make([]adminStatisticsBucketRow, 0, len(rows))
	for _, row := range rows {
		result = append(result, adminStatisticsBucketRow{Bucket: row.Bucket, Value: adminStatisticsDecimalToNumber(row.Value)})
	}
	return result, nil
}

// ==================== handler ====================

// adminStatisticsOverviewHandler ports GET /api/admin/statistics/overview.
func adminStatisticsOverviewHandler(c *gin.Context) {
	ctx := c.Request.Context()

	dailyWindow := adminStatisticsDailyWindow(adminStatisticsDailyDays)
	monthlyWindow := adminStatisticsMonthlyWindow(adminStatisticsMonthlyMonths)
	todayRange := adminStatisticsBusinessDayRange(0)
	yesterdayRange := adminStatisticsBusinessDayRange(-1)
	last7DaysRange := adminStatisticsBusinessRollingRange(7)
	last30DaysRange := adminStatisticsBusinessRollingRange(30)

	// JS Dates have millisecond resolution; truncating keeps every derived
	// boundary an exact millisecond like the Node handler.
	now := time.Now().UTC().Truncate(time.Millisecond)
	agentFreshAfter := now.Add(-time.Duration(adminStatisticsAgentStaleMinutes) * time.Minute)
	taskLookbackAfter := now.Add(-time.Duration(adminStatisticsTaskLookbackDays) * adminStatisticsDayDuration)
	riskLookbackAfter := now.Add(-time.Duration(adminStatisticsRiskLookbackDays) * adminStatisticsDayDuration)
	expiringBefore := now.Add(time.Duration(adminStatisticsRiskLookbackDays) * adminStatisticsDayDuration)
	todayDateKey := lib.GetDateStringInTimezone(now, lib.BusinessTimezone)

	var (
		totalUsers               int64
		totalInstances           int64
		activeInstances          int64
		paidInstances            int64
		freeInstances            int64
		totalRecharge            float64
		totalConsume             float64
		totalAff                 float64
		totalDestroyFee          float64
		dailyUsers               []adminStatisticsBucketRow
		monthlyUsers             []adminStatisticsBucketRow
		dailyInstances           []adminStatisticsBucketRow
		monthlyInstances         []adminStatisticsBucketRow
		dailyRecharge            []adminStatisticsBucketRow
		monthlyRecharge          []adminStatisticsBucketRow
		dailyConsume             []adminStatisticsBucketRow
		monthlyConsume           []adminStatisticsBucketRow
		dailyAff                 []adminStatisticsBucketRow
		monthlyAff               []adminStatisticsBucketRow
		dailyDestroyFee          []adminStatisticsBucketRow
		monthlyDestroyFee        []adminStatisticsBucketRow
		todayRevenue             float64
		yesterdayRevenue         float64
		last7DaysRevenue         float64
		last30DaysRevenue        float64
		todayOrders              int64
		todaySuccessfulOrders    int64
		todayFailedOrders        int64
		todayPendingOrders       int64
		attentionOrders          int64
		todayNewUsers            int64
		todayActiveUsers         int64
		paidUsers                int64
		todayNewInstances        int64
		runningInstances         int64
		abnormalInstances        int64
		expiringInstances        int64
		pendingDeliveryTasks     int64
		failedDeliveryTasks      int64
		totalHosts               int64
		onlineHosts              int64
		onlineAgents             int64
		staleAgents              int64
		openTickets              int64
		unreadInboxMessages      int64
		failedNotificationLogs   int64
		failedEmailTasks         int64
		activePaymentProviders   int64
		enabledNotificationChans int64
		smtpEnabledConfigs       int64
		failedUpdateTasks        int64
		diskUpdateErrorTasks     int64
		activeLotteries          []adminStatisticsLotteryItem
		totalActiveLotteries     int64
		todayCheckins            int64
		checkinSettings          db.CheckinDailySettings
		recentInstances          []adminStatisticsRecentInstance
		pendingTickets           []adminStatisticsPendingTicket
	)

	batch := &adminStatisticsBatch{}

	// ---- user / instance totals ----
	batch.run(func() (err error) {
		totalUsers, err = adminStatisticsCount(db.DB.WithContext(ctx).Model(&models.User{}))
		return
	})
	batch.run(func() (err error) {
		totalInstances, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.Instance{}).Where("status <> ?", "deleted"),
		)
		return
	})
	batch.run(func() (err error) {
		activeInstances, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.Instance{}).Where("status NOT IN ?", []string{"deleted", "suspended"}),
		)
		return
	})
	batch.run(func() (err error) {
		paidInstances, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.Instance{}).
				Where("status <> ?", "deleted").
				Where("package_plan_id IS NOT NULL"),
		)
		return
	})
	batch.run(func() (err error) {
		freeInstances, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.Instance{}).
				Where("status <> ?", "deleted").
				Where("package_plan_id IS NULL"),
		)
		return
	})

	// ---- lifetime money totals ----
	batch.run(func() (err error) {
		totalRecharge, err = adminStatisticsMoneyScalar(ctx, `
			SELECT COALESCE(SUM(COALESCE(actual_amount, amount)), 0)::numeric AS value
			FROM recharge_records
			WHERE status = 'completed'
		`)
		return
	})
	batch.run(func() (err error) {
		totalConsume, err = adminStatisticsMoneyScalar(ctx, `
			SELECT COALESCE(SUM(ABS(amount)), 0)::numeric AS value
			FROM balance_logs
			WHERE type IN ('consume', 'transfer_fee')
		`)
		return
	})
	batch.run(func() (err error) {
		totalAff, err = adminStatisticsMoneyScalar(ctx, `
			SELECT COALESCE(SUM(amount), 0)::numeric AS value
			FROM aff_logs
			WHERE type IN ('new_purchase', 'renew')
		`)
		return
	})
	batch.run(func() (err error) {
		totalDestroyFee, err = adminStatisticsMoneyScalar(ctx, `
			SELECT COALESCE(SUM(fee_amount), 0)::numeric AS value
			FROM user_destroy_records
		`)
		return
	})

	// ---- 30-day / 12-month series ----
	batch.run(func() (err error) {
		dailyUsers, err = adminStatisticsCountBuckets(ctx, `
			SELECT to_char(created_at + interval '8 hours', 'YYYY-MM-DD') AS bucket, COUNT(*)::int AS value
			FROM users
			WHERE created_at >= ? AND created_at < ?
			GROUP BY to_char(created_at + interval '8 hours', 'YYYY-MM-DD')
			ORDER BY bucket
		`, dailyWindow.Start, dailyWindow.End)
		return
	})
	batch.run(func() (err error) {
		monthlyUsers, err = adminStatisticsCountBuckets(ctx, `
			SELECT to_char(created_at + interval '8 hours', 'YYYY-MM') AS bucket, COUNT(*)::int AS value
			FROM users
			WHERE created_at >= ? AND created_at < ?
			GROUP BY to_char(created_at + interval '8 hours', 'YYYY-MM')
			ORDER BY bucket
		`, monthlyWindow.Start, monthlyWindow.End)
		return
	})
	batch.run(func() (err error) {
		dailyInstances, err = adminStatisticsCountBuckets(ctx, `
			SELECT to_char(created_at + interval '8 hours', 'YYYY-MM-DD') AS bucket, COUNT(*)::int AS value
			FROM instances
			WHERE created_at >= ? AND created_at < ?
			GROUP BY to_char(created_at + interval '8 hours', 'YYYY-MM-DD')
			ORDER BY bucket
		`, dailyWindow.Start, dailyWindow.End)
		return
	})
	batch.run(func() (err error) {
		monthlyInstances, err = adminStatisticsCountBuckets(ctx, `
			SELECT to_char(created_at + interval '8 hours', 'YYYY-MM') AS bucket, COUNT(*)::int AS value
			FROM instances
			WHERE created_at >= ? AND created_at < ?
			GROUP BY to_char(created_at + interval '8 hours', 'YYYY-MM')
			ORDER BY bucket
		`, monthlyWindow.Start, monthlyWindow.End)
		return
	})
	batch.run(func() (err error) {
		dailyRecharge, err = adminStatisticsMoneyBuckets(ctx, `
			SELECT to_char(COALESCE(completed_at, created_at) + interval '8 hours', 'YYYY-MM-DD') AS bucket,
			       COALESCE(SUM(COALESCE(actual_amount, amount)), 0)::numeric AS value
			FROM recharge_records
			WHERE status = 'completed'
			  AND COALESCE(completed_at, created_at) >= ?
			  AND COALESCE(completed_at, created_at) < ?
			GROUP BY to_char(COALESCE(completed_at, created_at) + interval '8 hours', 'YYYY-MM-DD')
			ORDER BY bucket
		`, dailyWindow.Start, dailyWindow.End)
		return
	})
	batch.run(func() (err error) {
		monthlyRecharge, err = adminStatisticsMoneyBuckets(ctx, `
			SELECT to_char(COALESCE(completed_at, created_at) + interval '8 hours', 'YYYY-MM') AS bucket,
			       COALESCE(SUM(COALESCE(actual_amount, amount)), 0)::numeric AS value
			FROM recharge_records
			WHERE status = 'completed'
			  AND COALESCE(completed_at, created_at) >= ?
			  AND COALESCE(completed_at, created_at) < ?
			GROUP BY to_char(COALESCE(completed_at, created_at) + interval '8 hours', 'YYYY-MM')
			ORDER BY bucket
		`, monthlyWindow.Start, monthlyWindow.End)
		return
	})
	batch.run(func() (err error) {
		dailyConsume, err = adminStatisticsMoneyBuckets(ctx, `
			SELECT to_char(created_at + interval '8 hours', 'YYYY-MM-DD') AS bucket,
			       COALESCE(SUM(ABS(amount)), 0)::numeric AS value
			FROM balance_logs
			WHERE type IN ('consume', 'transfer_fee')
			  AND created_at >= ?
			  AND created_at < ?
			GROUP BY to_char(created_at + interval '8 hours', 'YYYY-MM-DD')
			ORDER BY bucket
		`, dailyWindow.Start, dailyWindow.End)
		return
	})
	batch.run(func() (err error) {
		monthlyConsume, err = adminStatisticsMoneyBuckets(ctx, `
			SELECT to_char(created_at + interval '8 hours', 'YYYY-MM') AS bucket,
			       COALESCE(SUM(ABS(amount)), 0)::numeric AS value
			FROM balance_logs
			WHERE type IN ('consume', 'transfer_fee')
			  AND created_at >= ?
			  AND created_at < ?
			GROUP BY to_char(created_at + interval '8 hours', 'YYYY-MM')
			ORDER BY bucket
		`, monthlyWindow.Start, monthlyWindow.End)
		return
	})
	batch.run(func() (err error) {
		dailyAff, err = adminStatisticsMoneyBuckets(ctx, `
			SELECT to_char(created_at + interval '8 hours', 'YYYY-MM-DD') AS bucket,
			       COALESCE(SUM(amount), 0)::numeric AS value
			FROM aff_logs
			WHERE type IN ('new_purchase', 'renew')
			  AND created_at >= ?
			  AND created_at < ?
			GROUP BY to_char(created_at + interval '8 hours', 'YYYY-MM-DD')
			ORDER BY bucket
		`, dailyWindow.Start, dailyWindow.End)
		return
	})
	batch.run(func() (err error) {
		monthlyAff, err = adminStatisticsMoneyBuckets(ctx, `
			SELECT to_char(created_at + interval '8 hours', 'YYYY-MM') AS bucket,
			       COALESCE(SUM(amount), 0)::numeric AS value
			FROM aff_logs
			WHERE type IN ('new_purchase', 'renew')
			  AND created_at >= ?
			  AND created_at < ?
			GROUP BY to_char(created_at + interval '8 hours', 'YYYY-MM')
			ORDER BY bucket
		`, monthlyWindow.Start, monthlyWindow.End)
		return
	})
	batch.run(func() (err error) {
		dailyDestroyFee, err = adminStatisticsMoneyBuckets(ctx, `
			SELECT to_char(destroyed_at + interval '8 hours', 'YYYY-MM-DD') AS bucket,
			       COALESCE(SUM(fee_amount), 0)::numeric AS value
			FROM user_destroy_records
			WHERE destroyed_at >= ?
			  AND destroyed_at < ?
			GROUP BY to_char(destroyed_at + interval '8 hours', 'YYYY-MM-DD')
			ORDER BY bucket
		`, dailyWindow.Start, dailyWindow.End)
		return
	})
	batch.run(func() (err error) {
		monthlyDestroyFee, err = adminStatisticsMoneyBuckets(ctx, `
			SELECT to_char(destroyed_at + interval '8 hours', 'YYYY-MM') AS bucket,
			       COALESCE(SUM(fee_amount), 0)::numeric AS value
			FROM user_destroy_records
			WHERE destroyed_at >= ?
			  AND destroyed_at < ?
			GROUP BY to_char(destroyed_at + interval '8 hours', 'YYYY-MM')
			ORDER BY bucket
		`, monthlyWindow.Start, monthlyWindow.End)
		return
	})

	// ---- revenue windows ----
	batch.run(func() (err error) {
		todayRevenue, err = adminStatisticsMoneyScalar(ctx, `
			SELECT COALESCE(SUM(COALESCE(actual_amount, amount)), 0)::numeric AS value
			FROM recharge_records
			WHERE status = 'completed'
			  AND COALESCE(completed_at, created_at) >= ?
			  AND COALESCE(completed_at, created_at) < ?
		`, todayRange.Start, todayRange.End)
		return
	})
	batch.run(func() (err error) {
		yesterdayRevenue, err = adminStatisticsMoneyScalar(ctx, `
			SELECT COALESCE(SUM(COALESCE(actual_amount, amount)), 0)::numeric AS value
			FROM recharge_records
			WHERE status = 'completed'
			  AND COALESCE(completed_at, created_at) >= ?
			  AND COALESCE(completed_at, created_at) < ?
		`, yesterdayRange.Start, yesterdayRange.End)
		return
	})
	batch.run(func() (err error) {
		last7DaysRevenue, err = adminStatisticsMoneyScalar(ctx, `
			SELECT COALESCE(SUM(COALESCE(actual_amount, amount)), 0)::numeric AS value
			FROM recharge_records
			WHERE status = 'completed'
			  AND COALESCE(completed_at, created_at) >= ?
			  AND COALESCE(completed_at, created_at) < ?
		`, last7DaysRange.Start, last7DaysRange.End)
		return
	})
	batch.run(func() (err error) {
		last30DaysRevenue, err = adminStatisticsMoneyScalar(ctx, `
			SELECT COALESCE(SUM(COALESCE(actual_amount, amount)), 0)::numeric AS value
			FROM recharge_records
			WHERE status = 'completed'
			  AND COALESCE(completed_at, created_at) >= ?
			  AND COALESCE(completed_at, created_at) < ?
		`, last30DaysRange.Start, last30DaysRange.End)
		return
	})

	// ---- order health ----
	batch.run(func() (err error) {
		todayOrders, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.RechargeRecord{}).
				Where("created_at >= ? AND created_at < ?", todayRange.Start, todayRange.End),
		)
		return
	})
	batch.run(func() (err error) {
		todaySuccessfulOrders, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.RechargeRecord{}).
				Where("status = ?", "completed").
				Where("created_at >= ? AND created_at < ?", todayRange.Start, todayRange.End),
		)
		return
	})
	batch.run(func() (err error) {
		todayFailedOrders, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.RechargeRecord{}).
				Where("status IN ?", []string{"failed", "cancelled", "refunded"}).
				Where("created_at >= ? AND created_at < ?", todayRange.Start, todayRange.End),
		)
		return
	})
	batch.run(func() (err error) {
		todayPendingOrders, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.RechargeRecord{}).
				Where("status IN ?", []string{"pending", "paid"}).
				Where("created_at >= ? AND created_at < ?", todayRange.Start, todayRange.End),
		)
		return
	})
	batch.run(func() (err error) {
		attentionOrders, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.RechargeRecord{}).
				Where("status IN ?", []string{"paid", "failed"}).
				Where("updated_at >= ?", riskLookbackAfter),
		)
		return
	})

	// ---- user activity ----
	batch.run(func() (err error) {
		todayNewUsers, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.User{}).
				Where("created_at >= ? AND created_at < ?", todayRange.Start, todayRange.End),
		)
		return
	})
	batch.run(func() (err error) {
		todayActiveUsers, err = adminStatisticsCountScalar(ctx, `
			SELECT COUNT(DISTINCT user_id)::int AS value
			FROM login_records
			WHERE created_at >= ?
			  AND created_at < ?
		`, todayRange.Start, todayRange.End)
		return
	})
	batch.run(func() (err error) {
		paidUsers, err = adminStatisticsCountScalar(ctx, `
			SELECT COUNT(DISTINCT user_id)::int AS value
			FROM recharge_records
			WHERE status = 'completed'
		`)
		return
	})

	// ---- instance health ----
	batch.run(func() (err error) {
		todayNewInstances, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.Instance{}).
				Where("created_at >= ? AND created_at < ?", todayRange.Start, todayRange.End),
		)
		return
	})
	batch.run(func() (err error) {
		runningInstances, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.Instance{}).Where("status = ?", "running"),
		)
		return
	})
	batch.run(func() (err error) {
		abnormalInstances, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.Instance{}).Where("status = ?", "error"),
		)
		return
	})
	batch.run(func() (err error) {
		expiringInstances, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.Instance{}).
				Where("status <> ?", "deleted").
				Where("expires_at >= ? AND expires_at < ?", now, expiringBefore),
		)
		return
	})

	// ---- delivery ----
	batch.run(func() (err error) {
		pendingDeliveryTasks, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.InstanceTask{}).
				Where("status IN ?", []string{"PENDING", "PROCESSING"}),
		)
		return
	})
	batch.run(func() (err error) {
		failedDeliveryTasks, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.InstanceTask{}).
				Where("status = ?", "FAILED").
				Where("created_at >= ?", taskLookbackAfter),
		)
		return
	})

	// ---- infrastructure ----
	batch.run(func() (err error) {
		totalHosts, err = adminStatisticsCount(db.DB.WithContext(ctx).Model(&models.Host{}))
		return
	})
	batch.run(func() (err error) {
		onlineHosts, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.Host{}).Where("status = ?", "online"),
		)
		return
	})
	batch.run(func() (err error) {
		onlineAgents, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.HostAgent{}).
				Where("enabled = ?", true).
				Where("status = ?", "online").
				Where("last_seen_at >= ?", agentFreshAfter),
		)
		return
	})
	// The parenthesised OR must live in a single Where: GORM joins separate
	// Where calls with AND and only wraps the whole group in one pair of parens,
	// which would turn `a AND (b OR c)` into `(a AND b) OR c`.
	batch.run(func() (err error) {
		staleAgents, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.HostAgent{}).
				Where("enabled = ? AND (status <> ? OR last_seen_at IS NULL OR last_seen_at < ?)", true, "online", agentFreshAfter),
		)
		return
	})

	// ---- support ----
	batch.run(func() (err error) {
		openTickets, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.Ticket{}).
				Where("status IN ?", []string{"open", "in_progress"}),
		)
		return
	})
	batch.run(func() (err error) {
		unreadInboxMessages, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.InboxMessage{}).Where("is_read = ?", false),
		)
		return
	})
	batch.run(func() (err error) {
		failedNotificationLogs, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.NotificationLog{}).
				Where("status = ?", "failed").
				Where("created_at >= ?", taskLookbackAfter),
		)
		return
	})
	batch.run(func() (err error) {
		failedEmailTasks, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.HostNotificationEmailTask{}).
				Where("status = ?", "FAILED").
				Where("created_at >= ?", taskLookbackAfter),
		)
		return
	})

	// ---- payment / notification configuration ----
	batch.run(func() (err error) {
		activePaymentProviders, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.PaymentProvider{}).Where("status = ?", "active"),
		)
		return
	})
	batch.run(func() (err error) {
		enabledNotificationChans, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.NotificationChannel{}).Where("enabled = ?", true),
		)
		return
	})
	batch.run(func() (err error) {
		smtpEnabledConfigs, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.SystemConfig{}).
				Where("key = ?", "smtp_enabled").
				Where("value IN ?", []string{"true", "1", "yes", "on"}),
		)
		return
	})

	// ---- OTA health ----
	batch.run(func() (err error) {
		failedUpdateTasks, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.SystemUpdateTask{}).
				Where("status = ?", "failed").
				Where("created_at >= ?", riskLookbackAfter),
		)
		return
	})
	batch.run(func() (err error) {
		diskUpdateErrorTasks, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.SystemUpdateTask{}).
				Where("created_at >= ? AND (error_message LIKE ? OR error_message LIKE ? OR error_message LIKE ?)",
					riskLookbackAfter, "%No space left%", "%磁盘空间不足%", "%磁碟空間不足%"),
		)
		return
	})

	// ---- marketing ----
	batch.run(func() (err error) {
		activeLotteries, err = adminStatisticsActiveLotteries(ctx, now)
		return
	})
	batch.run(func() (err error) {
		totalActiveLotteries, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.Lottery{}).
				Where("is_active = ?", true).
				Where("(end_at IS NULL OR end_at >= ?)", now),
		)
		return
	})
	batch.run(func() (err error) {
		todayCheckins, err = adminStatisticsCount(
			db.DB.WithContext(ctx).Model(&models.DailyCheckin{}).Where("date_key = ?", todayDateKey),
		)
		return
	})
	batch.run(func() (err error) {
		checkinSettings, err = db.CheckinGetDailyCheckinSettings(ctx)
		return
	})

	// ---- recent activity lists ----
	batch.run(func() (err error) {
		recentInstances, err = adminStatisticsRecentInstances(ctx)
		return
	})
	batch.run(func() (err error) {
		pendingTickets, err = adminStatisticsPendingTickets(ctx)
		return
	})

	if err := batch.wait(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取统计数据失败"})
		return
	}

	// ---- derived risk list (same order as the TS addRisk calls) ----
	risks := make([]adminStatisticsOperationRisk, 0, 9)
	adminStatisticsAddRisk(&risks, activePaymentProviders == 0, "payment_provider_missing", "critical", activePaymentProviders)
	adminStatisticsAddRisk(&risks, smtpEnabledConfigs == 0, "smtp_missing", "warning", smtpEnabledConfigs)
	adminStatisticsAddRisk(&risks, enabledNotificationChans == 0, "notification_channel_missing", "warning", enabledNotificationChans)
	adminStatisticsAddRisk(&risks, totalHosts > 0 && onlineHosts < totalHosts, "host_offline", "critical", totalHosts-onlineHosts)
	adminStatisticsAddRisk(&risks, staleAgents > 0, "agent_stale", "critical", staleAgents)
	adminStatisticsAddRisk(&risks, failedDeliveryTasks > 0, "delivery_failed", "critical", failedDeliveryTasks)
	adminStatisticsAddRisk(&risks, attentionOrders > 0, "payment_attention", "warning", attentionOrders)
	adminStatisticsAddRisk(&risks, failedUpdateTasks > 0, "ota_failed", "critical", failedUpdateTasks)
	adminStatisticsAddRisk(&risks, diskUpdateErrorTasks > 0, "disk_update_error", "critical", diskUpdateErrorTasks)

	response := adminStatisticsOverviewResponse{
		Meta: adminStatisticsMeta{
			Timezone:      lib.BusinessTimezone,
			DailyDays:     adminStatisticsDailyDays,
			MonthlyMonths: adminStatisticsMonthlyMonths,
		},
		Users: adminStatisticsUsersSection{
			Total:           totalUsers,
			DailyNewUsers:   adminStatisticsFillSeries(dailyUsers, dailyWindow.Labels, false),
			MonthlyNewUsers: adminStatisticsFillSeries(monthlyUsers, monthlyWindow.Labels, false),
		},
		Instances: adminStatisticsInstancesSection{
			Total:                   totalInstances,
			Active:                  activeInstances,
			Paid:                    paidInstances,
			Free:                    freeInstances,
			DailyCreatedInstances:   adminStatisticsFillSeries(dailyInstances, dailyWindow.Labels, false),
			MonthlyCreatedInstances: adminStatisticsFillSeries(monthlyInstances, monthlyWindow.Labels, false),
		},
		Billing: adminStatisticsBillingSection{
			Totals: adminStatisticsBillingTotals{
				Recharge:   adminStatisticsScalarValue(totalRecharge, true),
				Consume:    adminStatisticsScalarValue(totalConsume, true),
				Aff:        adminStatisticsScalarValue(totalAff, true),
				DestroyFee: adminStatisticsScalarValue(totalDestroyFee, true),
			},
			DailyRecharge:     adminStatisticsFillSeries(dailyRecharge, dailyWindow.Labels, true),
			MonthlyRecharge:   adminStatisticsFillSeries(monthlyRecharge, monthlyWindow.Labels, true),
			DailyConsume:      adminStatisticsFillSeries(dailyConsume, dailyWindow.Labels, true),
			MonthlyConsume:    adminStatisticsFillSeries(monthlyConsume, monthlyWindow.Labels, true),
			DailyAff:          adminStatisticsFillSeries(dailyAff, dailyWindow.Labels, true),
			MonthlyAff:        adminStatisticsFillSeries(monthlyAff, monthlyWindow.Labels, true),
			DailyDestroyFee:   adminStatisticsFillSeries(dailyDestroyFee, dailyWindow.Labels, true),
			MonthlyDestroyFee: adminStatisticsFillSeries(monthlyDestroyFee, monthlyWindow.Labels, true),
		},
		Operations: adminStatisticsOperationsSection{
			Revenue: adminStatisticsRevenueBlock{
				Today:      adminStatisticsScalarValue(todayRevenue, true),
				Yesterday:  adminStatisticsScalarValue(yesterdayRevenue, true),
				Last7Days:  adminStatisticsScalarValue(last7DaysRevenue, true),
				Last30Days: adminStatisticsScalarValue(last30DaysRevenue, true),
			},
			Orders: adminStatisticsOrdersBlock{
				TodayTotal:     todayOrders,
				TodaySuccess:   todaySuccessfulOrders,
				TodayFailed:    todayFailedOrders,
				TodayPending:   todayPendingOrders,
				NeedsAttention: attentionOrders,
			},
			Users: adminStatisticsOperationsUsersBlock{
				NewToday:    todayNewUsers,
				ActiveToday: todayActiveUsers,
				PaidTotal:   paidUsers,
			},
			Instances: adminStatisticsOperationsInstancesBlock{
				NewToday:     todayNewInstances,
				Running:      runningInstances,
				Abnormal:     abnormalInstances,
				ExpiringSoon: expiringInstances,
			},
			Delivery: adminStatisticsDeliveryBlock{
				PendingTasks:   pendingDeliveryTasks,
				FailedTasks24h: failedDeliveryTasks,
			},
			Infrastructure: adminStatisticsInfrastructureBlock{
				HostsTotal:   totalHosts,
				HostsOnline:  onlineHosts,
				AgentsOnline: onlineAgents,
				AgentsStale:  staleAgents,
			},
			Support: adminStatisticsSupportBlock{
				OpenTickets:            openTickets,
				UnreadInboxMessages:    unreadInboxMessages,
				FailedNotifications24h: failedNotificationLogs,
				FailedEmails24h:        failedEmailTasks,
			},
			Risks: risks,
		},
		Marketing: adminStatisticsMarketingSection{
			ActiveLotteries:      activeLotteries,
			TotalActiveLotteries: totalActiveLotteries,
			Checkin: adminStatisticsCheckinBlock{
				Enabled:         checkinSettings.Enabled,
				MinPoints:       checkinSettings.MinPoints,
				MaxPoints:       checkinSettings.MaxPoints,
				RequireInstance: checkinSettings.RequireInstance,
				TodayCheckins:   todayCheckins,
			},
		},
		RecentInstances: recentInstances,
		PendingTickets:  pendingTickets,
	}

	c.JSON(http.StatusOK, response)
}

// adminStatisticsActiveLotteries mirrors lottery.findMany({ ..., take: 5 }).
func adminStatisticsActiveLotteries(ctx context.Context, now time.Time) ([]adminStatisticsLotteryItem, error) {
	var rows []struct {
		ID         int32      `gorm:"column:id"`
		Name       string     `gorm:"column:name"`
		CostPoints int32      `gorm:"column:cost_points"`
		TotalDraws int32      `gorm:"column:total_draws"`
		StartAt    *time.Time `gorm:"column:start_at"`
		EndAt      *time.Time `gorm:"column:end_at"`
	}
	err := db.DB.WithContext(ctx).Raw(`
		SELECT id, name, cost_points, total_draws, start_at, end_at
		FROM lotteries
		WHERE is_active = true
		  AND (end_at IS NULL OR end_at >= ?)
		ORDER BY created_at DESC
		LIMIT 5
	`, now).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	items := make([]adminStatisticsLotteryItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, adminStatisticsLotteryItem{
			ID:         row.ID,
			Name:       row.Name,
			CostPoints: row.CostPoints,
			TotalDraws: row.TotalDraws,
			StartAt:    adminStatisticsOptionalISOTimestamp(row.StartAt),
			EndAt:      adminStatisticsOptionalISOTimestamp(row.EndAt),
		})
	}
	return items, nil
}

// adminStatisticsRecentInstances mirrors instance.findMany({ ..., take: 5 }).
func adminStatisticsRecentInstances(ctx context.Context) ([]adminStatisticsRecentInstance, error) {
	var rows []struct {
		ID          int32     `gorm:"column:id"`
		Name        string    `gorm:"column:name"`
		Status      string    `gorm:"column:status"`
		CreatedAt   time.Time `gorm:"column:created_at"`
		UserID      int32     `gorm:"column:user_id"`
		Username    string    `gorm:"column:username"`
		Email       *string   `gorm:"column:email"`
		HostID      int32     `gorm:"column:host_id"`
		HostName    string    `gorm:"column:host_name"`
		CountryCode string    `gorm:"column:country_code"`
		PlanID      *int32    `gorm:"column:plan_id"`
		PlanName    *string   `gorm:"column:plan_name"`
		PlanCPU     *int32    `gorm:"column:plan_cpu"`
		PlanMemory  *int32    `gorm:"column:plan_memory"`
		PlanDisk    *int32    `gorm:"column:plan_disk"`
	}
	err := db.DB.WithContext(ctx).Raw(`
		SELECT
			i.id,
			i.name,
			i.status::text AS status,
			i.created_at,
			u.id AS user_id,
			u.username AS username,
			u.email AS email,
			h.id AS host_id,
			h.name AS host_name,
			h.country_code AS country_code,
			p.id AS plan_id,
			p.name AS plan_name,
			p.cpu AS plan_cpu,
			p.memory AS plan_memory,
			p.disk AS plan_disk
		FROM instances i
		JOIN users u ON u.id = i.user_id
		JOIN hosts h ON h.id = i.host_id
		LEFT JOIN package_plans p ON p.id = i.package_plan_id
		WHERE i.status <> 'deleted'
		ORDER BY i.created_at DESC
		LIMIT 5
	`).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	instances := make([]adminStatisticsRecentInstance, 0, len(rows))
	for _, row := range rows {
		item := adminStatisticsRecentInstance{
			ID:        row.ID,
			Name:      row.Name,
			Status:    row.Status,
			CreatedAt: adminStatisticsISOTimestamp(row.CreatedAt),
			User: adminStatisticsInstanceUser{
				ID:       row.UserID,
				Username: row.Username,
				Email:    row.Email,
			},
			Host: adminStatisticsInstanceHost{
				ID:          row.HostID,
				Name:        row.HostName,
				CountryCode: row.CountryCode,
			},
		}
		if row.PlanID != nil {
			plan := adminStatisticsPackagePlan{Name: "", CPU: 0, Memory: 0, Disk: 0}
			if row.PlanName != nil {
				plan.Name = *row.PlanName
			}
			if row.PlanCPU != nil {
				plan.CPU = *row.PlanCPU
			}
			if row.PlanMemory != nil {
				plan.Memory = *row.PlanMemory
			}
			if row.PlanDisk != nil {
				plan.Disk = *row.PlanDisk
			}
			item.PackagePlan = &plan
		}
		instances = append(instances, item)
	}
	return instances, nil
}

// adminStatisticsPendingTickets mirrors ticket.findMany({ ..., take: 5 }).
func adminStatisticsPendingTickets(ctx context.Context) ([]adminStatisticsPendingTicket, error) {
	var rows []struct {
		ID           int32     `gorm:"column:id"`
		Subject      string    `gorm:"column:subject"`
		Category     string    `gorm:"column:category"`
		Priority     string    `gorm:"column:priority"`
		Status       string    `gorm:"column:status"`
		CreatedAt    time.Time `gorm:"column:created_at"`
		UserID       int32     `gorm:"column:user_id"`
		Username     string    `gorm:"column:username"`
		HostID       *int32    `gorm:"column:host_id"`
		HostName     *string   `gorm:"column:host_name"`
		InstanceID   *int32    `gorm:"column:instance_id"`
		InstanceName *string   `gorm:"column:instance_name"`
	}
	err := db.DB.WithContext(ctx).Raw(`
		SELECT
			t.id,
			t.subject,
			t.category,
			t.priority::text AS priority,
			t.status::text AS status,
			t.created_at,
			u.id AS user_id,
			u.username AS username,
			h.id AS host_id,
			h.name AS host_name,
			i.id AS instance_id,
			i.name AS instance_name
		FROM tickets t
		JOIN users u ON u.id = t.user_id
		LEFT JOIN hosts h ON h.id = t.host_id
		LEFT JOIN instances i ON i.id = t.instance_id
		WHERE t.status IN ('open', 'in_progress')
		ORDER BY t.created_at DESC
		LIMIT 5
	`).Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	tickets := make([]adminStatisticsPendingTicket, 0, len(rows))
	for _, row := range rows {
		item := adminStatisticsPendingTicket{
			ID:        row.ID,
			Subject:   row.Subject,
			Category:  row.Category,
			Priority:  row.Priority,
			Status:    row.Status,
			CreatedAt: adminStatisticsISOTimestamp(row.CreatedAt),
			User: adminStatisticsTicketUser{
				ID:       row.UserID,
				Username: row.Username,
			},
		}
		if row.HostID != nil {
			host := adminStatisticsTicketHost{ID: *row.HostID}
			if row.HostName != nil {
				host.Name = *row.HostName
			}
			item.Host = &host
		}
		if row.InstanceID != nil {
			instance := adminStatisticsTicketInstance{ID: *row.InstanceID}
			if row.InstanceName != nil {
				instance.Name = *row.InstanceName
			}
			item.Instance = &instance
		}
		tickets = append(tickets, item)
	}
	return tickets, nil
}

// RegisterAdminStatistics mounts GET /api/admin/statistics/overview. The Node
// route uses onRequest: [authenticate, requireAdmin], so a signed-in non-admin
// gets 403 ADMIN_REQUIRED; the Go chain keeps that order.
func RegisterAdminStatistics(r *gin.Engine) {
	group := r.Group("/api/admin/statistics")
	group.GET("/overview", middleware.Authenticate(), middleware.RequireAdmin(), adminStatisticsOverviewHandler)
}

func init() {
	Register(RegisterAdminStatistics)
}
