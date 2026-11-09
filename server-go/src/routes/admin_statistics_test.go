package routes

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== roundMoney ====================

// TestAdminStatisticsRoundMoney pins Number(value.toFixed(2)). The values below
// are the classic binary-representation traps plus the exact-tie cases; the
// expectations were produced with Node:
//
//	(1.005).toFixed(2)  === "1.00"   (the double is 1.00499999999999989)
//	(2.675).toFixed(2)  === "2.67"   (the double is 2.67499999999999982)
//	(-1.005).toFixed(2) === "-1.00"
//	(0.125).toFixed(2)  === "0.13"   (exact tie, V8 rounds away from zero)
//	(-0.125).toFixed(2) === "-0.13"
//	(1.0050000000000001).toFixed(2) === "1.01"
func TestAdminStatisticsRoundMoney(t *testing.T) {
	cases := []struct {
		name  string
		input float64
		want  float64
	}{
		{"1.005 rounds down because the double is below the midpoint", 1.005, 1},
		{"2.675 rounds down for the same reason", 2.675, 2.67},
		{"-1.005 rounds toward zero", -1.005, -1},
		{"1.0050000000000001 is above the midpoint", 1.0050000000000001, 1.01},
		{"0.005 is above the midpoint", 0.005, 0.01},
		{"100.005 is below the midpoint", 100.005, 100},
		{"1234.565 is above the midpoint", 1234.565, 1234.57},
		{"-1234.565 is below the midpoint", -1234.565, -1234.57},
		{"exact tie rounds away from zero", 0.125, 0.13},
		{"negative exact tie rounds away from zero", -0.125, -0.13},
		{"2.125 tie rounds up", 2.125, 2.13},
		{"-2.125 tie rounds down", -2.125, -2.13},
		{"0.5 tie rounds up", 0.5, 0.5},
		{"2.5 tie rounds up", 2.5, 2.5},
		{"0.30000000000000004 keeps two digits", 0.1 + 0.2, 0.3},
		{"zero", 0, 0},
		{"tiny positive collapses to zero", 1e-9, 0},
		{"tiny negative collapses to zero", -0.001, 0},
		{"1e20 is below the toFixed fallback", 1e20, 1e20},
		{"1e21 uses the ToString fallback and stays unchanged", 1e21, 1e21},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := adminStatisticsRoundMoney(tc.input)
			if got != tc.want {
				t.Fatalf("roundMoney(%v) = %v, want %v", tc.input, got, tc.want)
			}
			if tc.want == 0 && math.Signbit(got) {
				t.Fatalf("roundMoney(%v) = -0, want +0 (JSON.stringify prints -0 as 0)", tc.input)
			}
		})
	}
}

// ==================== numeric conversion ====================

func TestAdminStatisticsDecimalToNumber(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
	}{
		{"0", 0},
		{"0.00", 0},
		{"123.45", 123.45},
		{"-7.5", -7.5},
		{"1e3", 1000},
	}
	for _, tc := range cases {
		value, err := models.NewDecimalFromString(tc.raw)
		if err != nil {
			t.Fatalf("NewDecimalFromString(%q): %v", tc.raw, err)
		}
		if got := adminStatisticsDecimalToNumber(value); got != tc.want {
			t.Fatalf("decimalToNumber(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// ==================== series filling ====================

func TestAdminStatisticsFillSeries(t *testing.T) {
	rows := []adminStatisticsBucketRow{
		{Bucket: "2026-03-04", Value: 2.675},
		{Bucket: "2026-03-02", Value: 1.005},
	}
	labels := []string{"2026-03-01", "2026-03-02", "2026-03-03", "2026-03-04"}

	money := adminStatisticsFillSeries(rows, labels, true)
	wantMoney := []float64{0, 1, 0, 2.67}
	for index, point := range money {
		if point.Label != labels[index] {
			t.Fatalf("money series label[%d] = %q, want %q (order follows labels, not rows)", index, point.Label, labels[index])
		}
		if point.Value != wantMoney[index] {
			t.Fatalf("money series value[%d] = %v, want %v", index, point.Value, wantMoney[index])
		}
	}

	counts := adminStatisticsFillSeries(rows, labels, false)
	wantCounts := []float64{0, 1.005, 0, 2.675}
	for index, point := range counts {
		if point.Value != wantCounts[index] {
			t.Fatalf("count series value[%d] = %v, want %v", index, point.Value, wantCounts[index])
		}
	}

	// An empty result-set still yields one point per label, never null.
	empty := adminStatisticsFillSeries(nil, labels, false)
	if len(empty) != len(labels) {
		t.Fatalf("empty series length = %d, want %d", len(empty), len(labels))
	}
}

// ==================== business windows ====================

// TestAdminStatisticsDailyWindow pins the fixed UTC+8 day bucketing: the window
// must not depend on the process timezone.
func TestAdminStatisticsDailyWindow(t *testing.T) {
	// 2026-03-15 02:30 UTC == 2026-03-15 10:30 in Asia/Shanghai.
	now := time.Date(2026, 3, 15, 2, 30, 0, 0, time.UTC)
	window := adminStatisticsDailyWindowAt(30, now)

	wantStart := time.Date(2026, 2, 13, 16, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 3, 15, 16, 0, 0, 0, time.UTC)
	if !window.Start.Equal(wantStart) {
		t.Fatalf("start = %s, want %s", window.Start, wantStart)
	}
	if !window.End.Equal(wantEnd) {
		t.Fatalf("end = %s, want %s", window.End, wantEnd)
	}
	if len(window.Labels) != 30 {
		t.Fatalf("len(labels) = %d, want 30", len(window.Labels))
	}
	if window.Labels[0] != "2026-02-14" || window.Labels[29] != "2026-03-15" {
		t.Fatalf("labels = %q .. %q, want 2026-02-14 .. 2026-03-15", window.Labels[0], window.Labels[29])
	}
	if window.Labels[1] != "2026-02-15" || window.Labels[28] != "2026-03-14" {
		t.Fatalf("labels not consecutive at the edges: %q / %q", window.Labels[1], window.Labels[28])
	}

	// Same instant expressed in a +08 fixed zone must give the identical window.
	shifted := time.Date(2026, 3, 15, 10, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	sameWindow := adminStatisticsDailyWindowAt(30, shifted)
	if !sameWindow.Start.Equal(window.Start) || !sameWindow.End.Equal(window.End) {
		t.Fatalf("window depends on the input zone: %s..%s vs %s..%s",
			sameWindow.Start, sameWindow.End, window.Start, window.End)
	}
}

// TestAdminStatisticsDailyWindowAcrossMidnight pins that the window rolls over
// at 00:00 China time, not at UTC midnight.
func TestAdminStatisticsDailyWindowAcrossMidnight(t *testing.T) {
	// 2026-03-15 16:30 UTC == 2026-03-16 00:30 in Asia/Shanghai.
	now := time.Date(2026, 3, 15, 16, 30, 0, 0, time.UTC)
	window := adminStatisticsDailyWindowAt(30, now)

	if got := adminStatisticsFormatBusinessDateFromStart(window.Start.Add(29 * adminStatisticsDayDuration)); got != "2026-03-16" {
		t.Fatalf("last label = %q, want 2026-03-16", got)
	}
	if !window.End.Equal(time.Date(2026, 3, 16, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("end = %s, want 2026-03-16T16:00:00Z", window.End)
	}
}

func TestAdminStatisticsMonthlyWindow(t *testing.T) {
	// 2026-03-15 02:30 UTC == 2026-03-15 10:30 in Asia/Shanghai.
	now := time.Date(2026, 3, 15, 2, 30, 0, 0, time.UTC)
	window := adminStatisticsMonthlyWindowAt(12, now)

	if len(window.Labels) != 12 {
		t.Fatalf("len(labels) = %d, want 12", len(window.Labels))
	}
	if window.Labels[0] != "2025-04" || window.Labels[11] != "2026-03" {
		t.Fatalf("labels = %q .. %q, want 2025-04 .. 2026-03", window.Labels[0], window.Labels[11])
	}
	if !window.Start.Equal(time.Date(2025, 3, 31, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("start = %s, want 2025-03-31T16:00:00Z", window.Start)
	}
	if !window.End.Equal(time.Date(2026, 3, 31, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("end = %s, want 2026-03-31T16:00:00Z", window.End)
	}

	// January 2026: twelve labels start at 2025-02 and end at 2026-01.
	january := time.Date(2026, 1, 5, 0, 30, 0, 0, time.UTC)
	januaryWindow := adminStatisticsMonthlyWindowAt(12, january)
	if januaryWindow.Labels[0] != "2025-02" || januaryWindow.Labels[11] != "2026-01" {
		t.Fatalf("january labels = %q .. %q, want 2025-02 .. 2026-01", januaryWindow.Labels[0], januaryWindow.Labels[11])
	}
	if !januaryWindow.Start.Equal(time.Date(2025, 1, 31, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("january start = %s, want 2025-01-31T16:00:00Z", januaryWindow.Start)
	}
}

// TestAdminStatisticsBusinessMonthStartUTC pins Date.UTC's month normalisation,
// which getMonthlyWindow relies on for months before the current one.
func TestAdminStatisticsBusinessMonthStartUTC(t *testing.T) {
	cases := []struct {
		year, month int
		want        time.Time
	}{
		{2026, 3, time.Date(2026, 2, 28, 16, 0, 0, 0, time.UTC)},
		{2026, 0, time.Date(2025, 11, 30, 16, 0, 0, 0, time.UTC)},  // month 0 = December 2025
		{2026, 13, time.Date(2026, 12, 31, 16, 0, 0, 0, time.UTC)}, // month 13 = January 2027
		{2026, -10, time.Date(2025, 1, 31, 16, 0, 0, 0, time.UTC)}, // month -10 = February 2025
	}
	for _, tc := range cases {
		if got := adminStatisticsBusinessMonthStartUTC(tc.year, tc.month); !got.Equal(tc.want) {
			t.Fatalf("monthStartUTC(%d, %d) = %s, want %s", tc.year, tc.month, got, tc.want)
		}
	}
}

func TestAdminStatisticsBusinessDayRanges(t *testing.T) {
	now := time.Date(2026, 3, 15, 2, 30, 0, 0, time.UTC)
	todayStart := time.Date(2026, 3, 14, 16, 0, 0, 0, time.UTC)

	today := adminStatisticsBusinessDayRangeAt(0, now)
	if !today.Start.Equal(todayStart) || !today.End.Equal(todayStart.Add(24*time.Hour)) {
		t.Fatalf("today = %s..%s, want %s..%s", today.Start, today.End, todayStart, todayStart.Add(24*time.Hour))
	}

	yesterday := adminStatisticsBusinessDayRangeAt(-1, now)
	if !yesterday.Start.Equal(todayStart.Add(-24*time.Hour)) || !yesterday.End.Equal(todayStart) {
		t.Fatalf("yesterday = %s..%s, want %s..%s", yesterday.Start, yesterday.End, todayStart.Add(-24*time.Hour), todayStart)
	}

	last7 := adminStatisticsBusinessRollingRangeAt(7, now)
	if !last7.Start.Equal(todayStart.Add(-6*24*time.Hour)) || !last7.End.Equal(todayStart.Add(24*time.Hour)) {
		t.Fatalf("last7 = %s..%s", last7.Start, last7.End)
	}

	last30 := adminStatisticsBusinessRollingRangeAt(30, now)
	if !last30.Start.Equal(todayStart.Add(-29*24*time.Hour)) || !last30.End.Equal(todayStart.Add(24*time.Hour)) {
		t.Fatalf("last30 = %s..%s", last30.Start, last30.End)
	}
}

func TestAdminStatisticsParseYearMonth(t *testing.T) {
	year, month := adminStatisticsParseYearMonth("2026-03")
	if year != 2026 || month != 3 {
		t.Fatalf("parseYearMonth = (%d, %d), want (2026, 3)", year, month)
	}
	year, month = adminStatisticsParseYearMonth("2026-12")
	if year != 2026 || month != 12 {
		t.Fatalf("parseYearMonth = (%d, %d), want (2026, 12)", year, month)
	}
}

// ==================== timestamps ====================

func TestAdminStatisticsISOTimestamp(t *testing.T) {
	value := time.Date(2026, 1, 29, 12, 34, 56, 789000000, time.UTC)
	if got := adminStatisticsISOTimestamp(value); got != "2026-01-29T12:34:56.789Z" {
		t.Fatalf("isoTimestamp = %q, want 2026-01-29T12:34:56.789Z", got)
	}

	zone := time.FixedZone("UTC+8", 8*60*60)
	shifted := time.Date(2026, 1, 29, 20, 34, 56, 0, zone)
	if got := adminStatisticsISOTimestamp(shifted); got != "2026-01-29T12:34:56.000Z" {
		t.Fatalf("isoTimestamp = %q, want 2026-01-29T12:34:56.000Z", got)
	}

	if got := adminStatisticsOptionalISOTimestamp(nil); got != nil {
		t.Fatalf("optional timestamp for nil = %v, want nil", *got)
	}
	formatted := adminStatisticsOptionalISOTimestamp(&value)
	if formatted == nil || *formatted != "2026-01-29T12:34:56.789Z" {
		t.Fatalf("optional timestamp = %v", formatted)
	}
}

// ==================== risks ====================

// TestAdminStatisticsAddRisk pins the order and contents of operations.risks,
// which the admin dashboard renders verbatim.
func TestAdminStatisticsAddRisk(t *testing.T) {
	risks := make([]adminStatisticsOperationRisk, 0, 9)
	adminStatisticsAddRisk(&risks, false, "payment_provider_missing", "critical", 0)
	adminStatisticsAddRisk(&risks, true, "smtp_missing", "warning", 0)
	adminStatisticsAddRisk(&risks, true, "host_offline", "critical", 2)
	adminStatisticsAddRisk(&risks, false, "agent_stale", "critical", 0)

	want := []adminStatisticsOperationRisk{
		{Key: "smtp_missing", Severity: "warning", Count: 0},
		{Key: "host_offline", Severity: "critical", Count: 2},
	}
	if len(risks) != len(want) {
		t.Fatalf("len(risks) = %d, want %d", len(risks), len(want))
	}
	for index, risk := range risks {
		if risk != want[index] {
			t.Fatalf("risk[%d] = %+v, want %+v", index, risk, want[index])
		}
	}
}

// ==================== Promise.all fan-out ====================

// TestAdminStatisticsBatchRunsAllQueries mirrors Promise.all: a failing query
// does not stop the others, and the first error is surfaced to the handler.
func TestAdminStatisticsBatchRunsAllQueries(t *testing.T) {
	var executed int64
	sentinel := errors.New("boom")

	batch := &adminStatisticsBatch{}
	for index := 0; index < 8; index++ {
		failing := index == 3
		batch.run(func() error {
			atomic.AddInt64(&executed, 1)
			if failing {
				return sentinel
			}
			return nil
		})
	}

	err := batch.wait()
	if !errors.Is(err, sentinel) {
		t.Fatalf("batch.wait() = %v, want %v", err, sentinel)
	}
	if got := atomic.LoadInt64(&executed); got != 8 {
		t.Fatalf("executed = %d, want 8 (a failure must not cancel the other queries)", got)
	}
}

// ==================== wire contract ====================

// TestAdminStatisticsResponseKeys pins the response shape against the Node
// handler's return statement: same keys, same nesting, arrays never null.
func TestAdminStatisticsResponseKeys(t *testing.T) {
	response := adminStatisticsOverviewResponse{
		Users: adminStatisticsUsersSection{
			DailyNewUsers:   []adminStatisticsSeriesPoint{{Label: "2026-03-15", Value: 1}},
			MonthlyNewUsers: []adminStatisticsSeriesPoint{{Label: "2026-03", Value: 1}},
		},
		Instances: adminStatisticsInstancesSection{
			DailyCreatedInstances:   []adminStatisticsSeriesPoint{},
			MonthlyCreatedInstances: []adminStatisticsSeriesPoint{},
		},
		Billing: adminStatisticsBillingSection{
			DailyRecharge:     []adminStatisticsSeriesPoint{},
			MonthlyRecharge:   []adminStatisticsSeriesPoint{},
			DailyConsume:      []adminStatisticsSeriesPoint{},
			MonthlyConsume:    []adminStatisticsSeriesPoint{},
			DailyAff:          []adminStatisticsSeriesPoint{},
			MonthlyAff:        []adminStatisticsSeriesPoint{},
			DailyDestroyFee:   []adminStatisticsSeriesPoint{},
			MonthlyDestroyFee: []adminStatisticsSeriesPoint{},
		},
		Operations: adminStatisticsOperationsSection{
			Risks: []adminStatisticsOperationRisk{{Key: "host_offline", Severity: "critical", Count: 1}},
		},
		Marketing: adminStatisticsMarketingSection{
			ActiveLotteries: []adminStatisticsLotteryItem{{ID: 1, Name: "spring", CostPoints: 100, TotalDraws: 5}},
		},
		RecentInstances: []adminStatisticsRecentInstance{{
			ID:        1,
			Name:      "vm-1",
			Status:    "running",
			CreatedAt: "2026-03-15T00:00:00.000Z",
			User:      adminStatisticsInstanceUser{ID: 1, Username: "alice"},
			Host:      adminStatisticsInstanceHost{ID: 2, Name: "hk-1", CountryCode: "HK"},
		}},
		PendingTickets: []adminStatisticsPendingTicket{{
			ID:        1,
			Subject:   "help",
			Category:  "general",
			Priority:  "normal",
			Status:    "open",
			CreatedAt: "2026-03-15T00:00:00.000Z",
			User:      adminStatisticsTicketUser{ID: 1, Username: "alice"},
		}},
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	adminStatisticsTestCaseAssertKeys(t, "root", decoded, []string{
		"meta", "users", "instances", "billing", "operations", "marketing", "recentInstances", "pendingTickets",
	})
	adminStatisticsTestCaseAssertKeys(t, "meta", adminStatisticsTestCaseChild(decoded, "meta"), []string{"timezone", "dailyDays", "monthlyMonths"})
	adminStatisticsTestCaseAssertKeys(t, "users", adminStatisticsTestCaseChild(decoded, "users"), []string{"total", "dailyNewUsers", "monthlyNewUsers"})
	adminStatisticsTestCaseAssertKeys(t, "instances", adminStatisticsTestCaseChild(decoded, "instances"), []string{
		"total", "active", "paid", "free", "dailyCreatedInstances", "monthlyCreatedInstances",
	})
	adminStatisticsTestCaseAssertKeys(t, "billing", adminStatisticsTestCaseChild(decoded, "billing"), []string{
		"totals", "dailyRecharge", "monthlyRecharge", "dailyConsume", "monthlyConsume",
		"dailyAff", "monthlyAff", "dailyDestroyFee", "monthlyDestroyFee",
	})
	adminStatisticsTestCaseAssertKeys(t, "billing.totals", adminStatisticsTestCaseChild(decoded, "billing", "totals"), []string{
		"recharge", "consume", "aff", "destroyFee",
	})
	adminStatisticsTestCaseAssertKeys(t, "operations", adminStatisticsTestCaseChild(decoded, "operations"), []string{
		"revenue", "orders", "users", "instances", "delivery", "infrastructure", "support", "risks",
	})
	adminStatisticsTestCaseAssertKeys(t, "operations.revenue", adminStatisticsTestCaseChild(decoded, "operations", "revenue"), []string{
		"today", "yesterday", "last7Days", "last30Days",
	})
	adminStatisticsTestCaseAssertKeys(t, "operations.orders", adminStatisticsTestCaseChild(decoded, "operations", "orders"), []string{
		"todayTotal", "todaySuccess", "todayFailed", "todayPending", "needsAttention",
	})
	adminStatisticsTestCaseAssertKeys(t, "operations.users", adminStatisticsTestCaseChild(decoded, "operations", "users"), []string{
		"newToday", "activeToday", "paidTotal",
	})
	adminStatisticsTestCaseAssertKeys(t, "operations.instances", adminStatisticsTestCaseChild(decoded, "operations", "instances"), []string{
		"newToday", "running", "abnormal", "expiringSoon",
	})
	adminStatisticsTestCaseAssertKeys(t, "operations.delivery", adminStatisticsTestCaseChild(decoded, "operations", "delivery"), []string{
		"pendingTasks", "failedTasks24h",
	})
	adminStatisticsTestCaseAssertKeys(t, "operations.infrastructure", adminStatisticsTestCaseChild(decoded, "operations", "infrastructure"), []string{
		"hostsTotal", "hostsOnline", "agentsOnline", "agentsStale",
	})
	adminStatisticsTestCaseAssertKeys(t, "operations.support", adminStatisticsTestCaseChild(decoded, "operations", "support"), []string{
		"openTickets", "unreadInboxMessages", "failedNotifications24h", "failedEmails24h",
	})
	adminStatisticsTestCaseAssertKeys(t, "operations.risks[0]", adminStatisticsTestCaseFirst(decoded, "operations", "risks"), []string{"key", "severity", "count"})
	adminStatisticsTestCaseAssertKeys(t, "marketing", adminStatisticsTestCaseChild(decoded, "marketing"), []string{
		"activeLotteries", "totalActiveLotteries", "checkin",
	})
	adminStatisticsTestCaseAssertKeys(t, "marketing.checkin", adminStatisticsTestCaseChild(decoded, "marketing", "checkin"), []string{
		"enabled", "minPoints", "maxPoints", "requireInstance", "todayCheckins",
	})
	adminStatisticsTestCaseAssertKeys(t, "marketing.activeLotteries[0]", adminStatisticsTestCaseFirst(decoded, "marketing", "activeLotteries"), []string{
		"id", "name", "costPoints", "totalDraws", "startAt", "endAt",
	})
	adminStatisticsTestCaseAssertKeys(t, "recentInstances[0]", adminStatisticsTestCaseFirst(decoded, "recentInstances"), []string{
		"id", "name", "status", "createdAt", "user", "host", "packagePlan",
	})
	adminStatisticsTestCaseAssertKeys(t, "recentInstances[0].user", adminStatisticsTestCaseChild(adminStatisticsTestCaseFirst(decoded, "recentInstances"), "user"), []string{
		"id", "username", "email",
	})
	adminStatisticsTestCaseAssertKeys(t, "recentInstances[0].host", adminStatisticsTestCaseChild(adminStatisticsTestCaseFirst(decoded, "recentInstances"), "host"), []string{
		"id", "name", "countryCode",
	})
	adminStatisticsTestCaseAssertKeys(t, "pendingTickets[0]", adminStatisticsTestCaseFirst(decoded, "pendingTickets"), []string{
		"id", "subject", "category", "priority", "status", "createdAt", "user", "host", "instance",
	})
	adminStatisticsTestCaseAssertKeys(t, "pendingTickets[0].user", adminStatisticsTestCaseChild(adminStatisticsTestCaseFirst(decoded, "pendingTickets"), "user"), []string{
		"id", "username",
	})
	adminStatisticsTestCaseAssertKeys(t, "series point", adminStatisticsTestCaseFirst(decoded, "users", "dailyNewUsers"), []string{"label", "value"})

	// Nulls the TS emits for absent optional relations.
	recent := adminStatisticsTestCaseFirst(decoded, "recentInstances")
	if recent["packagePlan"] != nil {
		t.Fatalf("recentInstances[0].packagePlan = %v, want null", recent["packagePlan"])
	}
	ticket := adminStatisticsTestCaseFirst(decoded, "pendingTickets")
	if ticket["host"] != nil || ticket["instance"] != nil {
		t.Fatalf("pendingTickets nullable relations = %v / %v, want null", ticket["host"], ticket["instance"])
	}
	lottery := adminStatisticsTestCaseFirst(decoded, "marketing", "activeLotteries")
	if lottery["startAt"] != nil || lottery["endAt"] != nil {
		t.Fatalf("lottery startAt/endAt = %v / %v, want null", lottery["startAt"], lottery["endAt"])
	}

	// Empty slices must serialise as [] rather than null.
	if got := adminStatisticsTestCaseChild(decoded, "instances")["dailyCreatedInstances"]; got == nil {
		t.Fatalf("empty series serialised as null, want []")
	}
}

func adminStatisticsTestCaseAssertKeys(t *testing.T, name string, object map[string]any, want []string) {
	t.Helper()
	if object == nil {
		t.Fatalf("%s: not an object", name)
	}
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if len(got) != len(sorted) {
		t.Fatalf("%s keys = %v, want %v", name, got, sorted)
	}
	for index := range got {
		if got[index] != sorted[index] {
			t.Fatalf("%s keys = %v, want %v", name, got, sorted)
		}
	}
}

func adminStatisticsTestCaseChild(value map[string]any, path ...string) map[string]any {
	current := value
	for _, key := range path {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func adminStatisticsTestCaseFirst(value map[string]any, path ...string) map[string]any {
	parent := adminStatisticsTestCaseChild(value, path[:len(path)-1]...)
	if parent == nil {
		return nil
	}
	items, ok := parent[path[len(path)-1]].([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	item, _ := items[0].(map[string]any)
	return item
}
