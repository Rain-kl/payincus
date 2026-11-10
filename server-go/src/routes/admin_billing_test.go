// Tests for the admin-billing route port (server/src/routes/admin-billing.ts).
//
// Two layers, same split as the rest of the Wave D ports:
//
//  1. PURE tests (no database): the parsers, the CSV/BOM contract used by the
//     reconciliation export, the batch-price preview arithmetic, the four route
//     behaviours that must hold before any SQL runs (401 for every path, the
//     Fastify body rules, the 13 inline rate limits, route registration), and the
//     money-rounding helpers.
//
//  2. DATABASE-BACKED tests, opt-in through PAYINCUS_GO_DB_TESTS=1 with a LOCAL
//     DSN whose database name contains "_test" (the gate from
//     src/routes/backups_test.go). They prove the invariants that only exist in
//     SQL:
//
//     - the refund cap and the delete-and-refund quote come from the SAME function
//     as the price-change quote (preview == settled amount);
//     - a refund submitted twice credits once (exact-replay guard);
//     - a batch price update is all-or-nothing: a failure on the Nth instance
//     rolls back the (N-1) instances and every balance;
//     - reconciliation is idempotent per date and per item key: rerunning keeps
//     the run id, does not duplicate items, and preserves an operator's decision;
//     - a non-admin token gets 403 on an admin-only route.
package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== pure parser tests ====================

func TestAdminBillingParsePositiveInteger(t *testing.T) {
	cases := []struct {
		value    string
		fallback int
		max      int
		want     int
	}{
		{"", 1, -1, 1},
		{"3", 1, -1, 3},
		{"0", 1, -1, 1},
		{"-2", 1, -1, 1},
		{"1.5", 1, -1, 1},
		{"01", 1, -1, 1}, // integer 1 after Number(), so accepted
		{"250", 20, 100, 100},
		{"1e3", 1, -1, 1000},
		{"abc", 7, -1, 7},
		{" 4 ", 1, -1, 4},
	}
	for _, tc := range cases {
		if got := adminBillingParsePositiveInteger(tc.value, tc.fallback, tc.max); got != tc.want {
			t.Errorf("parsePositiveInteger(%q,%d,%d) = %d, want %d", tc.value, tc.fallback, tc.max, got, tc.want)
		}
	}
}

func TestAdminBillingParseRouteID(t *testing.T) {
	cases := []struct {
		value string
		id    int32
		valid bool
	}{
		{"1", 1, true},
		{"950001", 950001, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"01", 0, false},
		{"abc", 0, false},
		{"", 0, false},
		{"2147483648", 0, true}, // pattern-valid, beyond int32 → "not found"
	}
	for _, tc := range cases {
		id, valid := adminBillingParseRouteID(tc.value)
		if id != tc.id || valid != tc.valid {
			t.Errorf("parseRouteID(%q) = (%d,%t), want (%d,%t)", tc.value, id, valid, tc.id, tc.valid)
		}
	}
}

func TestAdminBillingParseOptionalPositiveInteger(t *testing.T) {
	if value := adminBillingParseOptionalPositiveInteger("", false); value != nil {
		t.Errorf("absent filter = %v, want nil", *value)
	}
	if value := adminBillingParseOptionalPositiveInteger("", true); value != nil {
		t.Errorf("empty filter = %v, want nil", *value)
	}
	if value := adminBillingParseOptionalPositiveInteger("0", true); value != nil {
		t.Errorf("zero filter = %v, want nil", *value)
	}
	value := adminBillingParseOptionalPositiveInteger("42", true)
	if value == nil || *value != 42 {
		t.Fatalf("42 filter = %v, want 42", value)
	}
}

func TestAdminBillingParseBusinessDate(t *testing.T) {
	parsed, ok := adminBillingParseBusinessDate("2026-03-15")
	if !ok {
		t.Fatal("2026-03-15 rejected")
	}
	if parsed.DateString != "2026-03-15" {
		t.Errorf("dateString = %q", parsed.DateString)
	}
	// Asia/Shanghai midnight is 16:00 UTC the previous day.
	wantStart := time.Date(2026, 3, 14, 16, 0, 0, 0, time.UTC)
	if !parsed.Start.Equal(wantStart) {
		t.Errorf("start = %s, want %s", parsed.Start, wantStart)
	}
	if !parsed.End.Equal(wantStart.Add(24 * time.Hour)) {
		t.Errorf("end = %s", parsed.End)
	}

	for _, invalid := range []string{"2026-3-15", "2026-03-32", "2026-02-30", "26-03-15", "", "2026/03/15", "2026-13-01"} {
		if _, ok := adminBillingParseBusinessDate(invalid); ok {
			t.Errorf("%q accepted", invalid)
		}
	}
}

func TestAdminBillingNormalizeFinancialNote(t *testing.T) {
	if note, err := adminBillingNormalizeFinancialNote(nil); note != nil || err != nil {
		t.Fatalf("nil note = (%v,%v)", note, err)
	}
	if note, err := adminBillingNormalizeFinancialNote("   "); note != nil || err != nil {
		t.Fatalf("blank note = (%v,%v)", note, err)
	}
	note, err := adminBillingNormalizeFinancialNote("  已核对  ")
	if err != nil || note == nil || *note != "已核对" {
		t.Fatalf("note = (%v,%v)", note, err)
	}
	if _, err := adminBillingNormalizeFinancialNote(42.0); err == nil || err.Error() != "备注必须是字符串" {
		t.Fatalf("numeric note error = %v", err)
	}
	if _, err := adminBillingNormalizeFinancialNote(strings.Repeat("a", 501)); err == nil ||
		err.Error() != "备注不能超过 500 字符" {
		t.Fatalf("long note error = %v", err)
	}
	// 500 UTF-16 units is the limit, and multi-byte runes count once each.
	if _, err := adminBillingNormalizeFinancialNote(strings.Repeat("备", 500)); err != nil {
		t.Fatalf("500 CJK chars rejected: %v", err)
	}
}

func TestAdminBillingRoundMoney(t *testing.T) {
	cases := []struct {
		value float64
		want  float64
	}{
		{1.005, 1.0},  // binary 1.00499999…
		{2.675, 2.67}, // binary 2.67499999…
		{0.1 + 0.2, 0.3},
		{-1.005, -1.0},
		{30, 30},
		{0.004, 0},
	}
	for _, tc := range cases {
		if got := adminBillingRoundMoney(tc.value); got != tc.want {
			t.Errorf("roundMoney(%v) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestAdminBillingExceptionMessages(t *testing.T) {
	// The money messages are part of the contract (the UI shows them verbatim).
	if got := adminBillingFixed2(12.345); got != "12.35" {
		t.Errorf("fixed2(12.345) = %q", got)
	}
	if got := adminBillingJSNumberString(30); got != "30" {
		t.Errorf("numberString(30) = %q", got)
	}
	if got := adminBillingJSNumberString(30.5); got != "30.5" {
		t.Errorf("numberString(30.5) = %q", got)
	}
}

// ==================== CSV / export contract ====================

func TestAdminBillingCSVEscape(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{nil, ""},
		{"plain", "plain"},
		{"with,comma", `"with,comma"`},
		{`with"quote`, `"with""quote"`},
		{"line\nbreak", "\"line\nbreak\""},
		{30, "30"},
		{30.5, "30.5"},
		{int32(7), "7"},
		{time.Date(2026, 1, 2, 3, 4, 5, 6000000, time.UTC), "2026-01-02T03:04:05.006Z"},
	}
	for _, tc := range cases {
		if got := adminBillingCSVEscape(tc.value); got != tc.want {
			t.Errorf("csvEscape(%#v) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestAdminBillingBuildCSV(t *testing.T) {
	got := adminBillingBuildCSV([][]any{
		{"订单ID", "用户名"},
		{1, "a,b"},
	})
	want := "订单ID,用户名\n1,\"a,b\"\n"
	if got != want {
		t.Fatalf("buildCSV = %q, want %q", got, want)
	}
}

func TestAdminBillingMaskExportIdentifier(t *testing.T) {
	cases := []struct {
		value *string
		want  string
	}{
		{nil, ""},
		{adminBillingStringPtr(""), ""},
		{adminBillingStringPtr("12345678"), "12***"},
		{adminBillingStringPtr("123456789"), "1234***6789"},
		{adminBillingStringPtr("PARITY-TRADE-NO"), "PARI***E-NO"},
	}
	for _, tc := range cases {
		if got := adminBillingMaskExportIdentifier(tc.value); got != tc.want {
			t.Errorf("mask(%v) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestAdminBillingResolveRunStatus(t *testing.T) {
	cases := []struct {
		statuses []string
		want     string
	}{
		{nil, "normal"},
		{[]string{}, "normal"},
		{[]string{"discrepancy"}, "discrepancy"},
		{[]string{"confirmed", "discrepancy"}, "discrepancy"},
		{[]string{"confirmed", "ignored"}, "confirmed"},
		{[]string{"ignored", "ignored"}, "ignored"},
	}
	for _, tc := range cases {
		if got := adminBillingResolveRunStatus(tc.statuses); got != tc.want {
			t.Errorf("resolveRunStatus(%v) = %q, want %q", tc.statuses, got, tc.want)
		}
	}
}

// ==================== batch price-update parsing ====================

func TestAdminBillingParseBatchPriceUpdateBody(t *testing.T) {
	valid := map[string]any{
		"instanceIds": []any{float64(2), float64(1), float64(2)},
		"newPrice":    float64(45.678),
		"expectations": []any{
			map[string]any{"instanceId": float64(2), "version": float64(3)},
			map[string]any{"instanceId": float64(2), "version": float64(9)}, // duplicate → ignored
		},
	}
	request, err := adminBillingParseBatchPriceUpdateBody(valid)
	if err != nil {
		t.Fatalf("valid body rejected: %v", err)
	}
	if len(request.InstanceIDs) != 2 || request.InstanceIDs[0] != 2 || request.InstanceIDs[1] != 1 {
		t.Errorf("instanceIds = %v, want deduped order [2 1]", request.InstanceIDs)
	}
	if request.NewPrice != 45.68 {
		t.Errorf("newPrice = %v, want 45.68 (toFixed(2))", request.NewPrice)
	}
	if !request.SettleBalance {
		t.Error("settleBalance defaults to true")
	}
	if len(request.Expectations) != 1 || request.Expectations[0].Version != 3 {
		t.Errorf("expectations = %+v", request.Expectations)
	}

	invalid := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no ids", map[string]any{"newPrice": float64(1)}, "请选择要修改价格的实例"},
		{"empty ids", map[string]any{"instanceIds": []any{}, "newPrice": float64(1)}, "请选择要修改价格的实例"},
		{"bad id", map[string]any{"instanceIds": []any{float64(0)}, "newPrice": float64(1)}, "实例ID列表包含无效值"},
		{"fractional id", map[string]any{"instanceIds": []any{1.5}, "newPrice": float64(1)}, "实例ID列表包含无效值"},
		{"price string", map[string]any{"instanceIds": []any{float64(1)}, "newPrice": "5"}, "价格必须是 0-100000 之间的数字"},
		{"price too high", map[string]any{"instanceIds": []any{float64(1)}, "newPrice": float64(100001)}, "价格必须是 0-100000 之间的数字"},
		{"bad expectations", map[string]any{"instanceIds": []any{float64(1)}, "newPrice": float64(1), "expectations": "x"}, "预览版本参数无效"},
		{"unknown expectation id", map[string]any{
			"instanceIds":  []any{float64(1)},
			"newPrice":     float64(1),
			"expectations": []any{map[string]any{"instanceId": float64(2), "version": float64(0)}},
		}, "预览版本参数无效"},
	}
	for _, tc := range invalid {
		if _, err := adminBillingParseBatchPriceUpdateBody(tc.body); err == nil {
			t.Errorf("%s: accepted", tc.name)
		} else if err.Error() != tc.want {
			t.Errorf("%s: error = %q, want %q", tc.name, err.Error(), tc.want)
		}
	}

	tooMany := make([]any, 0, 101)
	for index := 0; index < 101; index++ {
		tooMany = append(tooMany, float64(index+1))
	}
	if _, err := adminBillingParseBatchPriceUpdateBody(map[string]any{
		"instanceIds": tooMany, "newPrice": float64(1),
	}); err == nil || err.Error() != "单次最多只能修改 100 个实例" {
		t.Errorf("101 ids error = %v", err)
	}
}

func TestAdminBillingBatchSummaryAndImpacts(t *testing.T) {
	items := []adminBillingBatchItem{
		{ID: 1, Status: "ready", PriceDiff: 10, User: &adminBillingBatchUser{ID: 7, Username: "b-user", Balance: 100}},
		{ID: 2, Status: "ready", PriceDiff: -4.5, User: &adminBillingBatchUser{ID: 7, Username: "b-user", Balance: 100}},
		{ID: 3, Status: "unchanged", PriceDiff: 0, User: &adminBillingBatchUser{ID: 9, Username: "A-user", Balance: 1}},
		{ID: 4, Status: "failed", PriceDiff: 0, Error: adminBillingStringPtr("实例不存在")},
	}

	summary := adminBillingSummarizeBatchItems(items, 5)
	if summary.SelectedCount != 5 || summary.ValidCount != 3 || summary.ChangedCount != 2 ||
		summary.UnchangedCount != 1 || summary.FailedCount != 1 {
		t.Fatalf("summary counts = %+v", summary)
	}
	if summary.TotalCharge != 10 || summary.TotalRefund != 4.5 || summary.NetAmount != 5.5 {
		t.Fatalf("summary money = %+v", summary)
	}

	impacts := adminBillingBuildBatchUserImpacts(items)
	if len(impacts) != 1 {
		t.Fatalf("impacts = %+v", impacts)
	}
	impact := impacts[0]
	if impact.UserID != 7 || impact.TotalCharge != 10 || impact.TotalRefund != 4.5 ||
		impact.NetDiff != 5.5 || impact.BalanceBefore != 100 || impact.BalanceAfter != 94.5 {
		t.Fatalf("impact = %+v", impact)
	}
	if impact.InsufficientBalance {
		t.Fatal("sufficient balance flagged")
	}

	// A negative balanceAfter flips insufficientBalance (the canSubmit gate).
	overspent := adminBillingBuildBatchUserImpacts([]adminBillingBatchItem{
		{ID: 5, Status: "ready", PriceDiff: 5, User: &adminBillingBatchUser{ID: 11, Username: "x", Balance: 1}},
	})
	if len(overspent) != 1 || !overspent[0].InsufficientBalance {
		t.Fatalf("overspent = %+v", overspent)
	}

	// Ordering is `localeCompare` on the username, so the lowercase name sorts after
	// the uppercase one under the ICU root collation.
	many := adminBillingBuildBatchUserImpacts([]adminBillingBatchItem{
		{ID: 6, Status: "ready", PriceDiff: 1, User: &adminBillingBatchUser{ID: 1, Username: "zeta", Balance: 10}},
		{ID: 7, Status: "ready", PriceDiff: 1, User: &adminBillingBatchUser{ID: 2, Username: "Alpha", Balance: 10}},
	})
	if many[0].Username != "Alpha" {
		t.Fatalf("impact order = %v", []string{many[0].Username, many[1].Username})
	}
}

// ==================== body / auth / routing ====================

func adminBillingTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterAdminBilling(router)
	return router
}

func adminBillingTestRequest(router *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	router.ServeHTTP(recorder, request)
	return recorder
}

// adminBillingAdminRoutes is the 22-route surface: the gin pattern (for
// registration) and a concrete path (for request-level checks).
var adminBillingAdminRoutes = []struct {
	method  string
	pattern string
	path    string
}{
	{http.MethodGet, "/api/admin/billing/overview", "/api/admin/billing/overview"},
	{http.MethodGet, "/api/admin/billing/records", "/api/admin/billing/records"},
	{http.MethodGet, "/api/admin/billing/reconciliation", "/api/admin/billing/reconciliation"},
	{http.MethodPost, "/api/admin/billing/reconciliation/run", "/api/admin/billing/reconciliation/run"},
	{http.MethodPatch, "/api/admin/billing/reconciliation/items/:id", "/api/admin/billing/reconciliation/items/1"},
	{http.MethodGet, "/api/admin/billing/reconciliation/export", "/api/admin/billing/reconciliation/export"},
	{http.MethodGet, "/api/admin/billing/instances", "/api/admin/billing/instances"},
	{http.MethodGet, "/api/admin/billing/recharge-records", "/api/admin/billing/recharge-records"},
	{http.MethodPost, "/api/admin/billing/recharge-records/:id/sync", "/api/admin/billing/recharge-records/1/sync"},
	{http.MethodPost, "/api/admin/instances/:id/suspend", "/api/admin/instances/1/suspend"},
	{http.MethodPost, "/api/admin/instances/:id/unsuspend", "/api/admin/instances/1/unsuspend"},
	{http.MethodPost, "/api/admin/instances/:id/extend", "/api/admin/instances/1/extend"},
	{http.MethodPost, "/api/admin/instances/:id/refund", "/api/admin/instances/1/refund"},
	{http.MethodPost, "/api/admin/instances/:id/delete-and-refund", "/api/admin/instances/1/delete-and-refund"},
	{http.MethodPost, "/api/admin/instances/:id/apply-aff", "/api/admin/instances/1/apply-aff"},
	{http.MethodPost, "/api/admin/instances/:id/update-price", "/api/admin/instances/1/update-price"},
	{http.MethodPost, "/api/admin/instances/:id/update-price/preview", "/api/admin/instances/1/update-price/preview"},
	{http.MethodPost, "/api/admin/instances/batch-update-price/preview", "/api/admin/instances/batch-update-price/preview"},
	{http.MethodPost, "/api/admin/instances/batch-update-price", "/api/admin/instances/batch-update-price"},
	{http.MethodPost, "/api/admin/instances/create", "/api/admin/instances/create"},
	{http.MethodGet, "/api/admin/instances/:id/available-plans", "/api/admin/instances/1/available-plans"},
	{http.MethodPost, "/api/admin/instances/:id/upgrade-plan", "/api/admin/instances/1/upgrade-plan"},
}

// TestAdminBillingRouteRegistration pins the surface: 22 absolute paths, and the
// static batch routes must not be swallowed by the /:id wildcard.
func TestAdminBillingRouteRegistration(t *testing.T) {
	router := adminBillingTestRouter()

	registered := map[string]bool{}
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	for _, want := range adminBillingAdminRoutes {
		key := want.method + " " + want.pattern
		if !registered[key] {
			t.Errorf("route missing: %s", key)
		}
	}

	total := 0
	for key := range registered {
		if strings.Contains(key, "/api/admin/billing/") || strings.Contains(key, "/api/admin/instances/") {
			total++
		}
	}
	if total != len(adminBillingAdminRoutes) {
		t.Errorf("registered %d admin-billing routes, want %d: %v", total, len(adminBillingAdminRoutes), registered)
	}
}

// TestAdminBillingRequiresAuthentication pins that NO route is reachable without a
// Web token (all 22 use authenticate + requireAdmin).
func TestAdminBillingRequiresAuthentication(t *testing.T) {
	router := adminBillingTestRouter()
	for _, route := range adminBillingAdminRoutes {
		recorder := adminBillingTestRequest(router, route.method, route.path, "", "")
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token = %d, want 401", route.method, route.path, recorder.Code)
		}
	}
}

// TestAdminBillingBodyRules pins the Fastify body contract the module inherits:
// auth runs before the parser (an invalid token wins over a malformed body), and a
// malformed/unparsable JSON body is a 400 rather than a handler-level error. There
// are NO AJV schemas in admin-billing.ts (verified: the file declares no `schema`
// blocks at all), so the parser is the only body-level validation surface.
func TestAdminBillingBodyRules(t *testing.T) {
	router := adminBillingTestRouter()

	recorder := adminBillingTestRequest(router, http.MethodPost, "/api/admin/instances/1/suspend", "not-a-token", `{`)
	if recorder.Code != http.StatusUnauthorized {
		// Auth runs before the body parser, so an invalid token must win.
		t.Fatalf("invalid token with malformed body = %d, want 401", recorder.Code)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/probe", func(c *gin.Context) {
		_, status, message := decodeJSONRequestBody(c)
		if status == 0 {
			c.JSON(http.StatusOK, gin.H{"parsed": true})
			return
		}
		c.JSON(status, gin.H{"error": message})
	})
	probe := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(rec, req)
		return rec
	}

	if got := probe(`{"days":1}`); got.Code != http.StatusOK {
		t.Fatalf("valid body = %d %s", got.Code, got.Body.String())
	}
	if got := probe(`{`); got.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", got.Code)
	}
	if got := probe(""); got.Code != http.StatusBadRequest {
		t.Fatalf("empty body = %d, want 400", got.Code)
	}
}

// TestAdminBillingRateLimits pins the 13 inline limits of the TypeScript and that
// each route has its own budget.
func TestAdminBillingRateLimits(t *testing.T) {
	ResetAdminBillingRateLimits()
	t.Cleanup(ResetAdminBillingRateLimits)
	gin.SetMode(gin.TestMode)

	router := gin.New()
	handler := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) }

	limits := []struct {
		method string
		path   string
		max    int
	}{
		{http.MethodPost, "/api/admin/billing/recharge-records/1/sync", 10},
		{http.MethodPost, "/api/admin/instances/1/suspend", 20},
		{http.MethodPost, "/api/admin/instances/1/unsuspend", 20},
		{http.MethodPost, "/api/admin/instances/1/extend", 10},
		{http.MethodPost, "/api/admin/instances/1/refund", 10},
		{http.MethodPost, "/api/admin/instances/1/delete-and-refund", 10},
		{http.MethodPost, "/api/admin/instances/1/apply-aff", 20},
		{http.MethodPost, "/api/admin/instances/1/update-price", 20},
		{http.MethodPost, "/api/admin/instances/1/update-price/preview", 30},
		{http.MethodPost, "/api/admin/instances/batch-update-price/preview", 20},
		{http.MethodPost, "/api/admin/instances/batch-update-price", 10},
		{http.MethodPost, "/api/admin/instances/create", 10},
		{http.MethodPost, "/api/admin/instances/1/upgrade-plan", 10},
	}
	for _, limit := range limits {
		switch limit.path {
		case "/api/admin/billing/recharge-records/1/sync":
			router.POST("/api/admin/billing/recharge-records/:id/sync",
				adminBillingRateLimit("/api/admin/billing/recharge-records/:id/sync", limit.max, time.Minute), handler)
		case "/api/admin/instances/batch-update-price/preview":
			router.POST("/api/admin/instances/batch-update-price/preview",
				adminBillingRateLimit("/api/admin/instances/batch-update-price/preview", limit.max, time.Minute), handler)
		case "/api/admin/instances/batch-update-price":
			router.POST("/api/admin/instances/batch-update-price",
				adminBillingRateLimit("/api/admin/instances/batch-update-price", limit.max, time.Minute), handler)
		case "/api/admin/instances/create":
			router.POST("/api/admin/instances/create",
				adminBillingRateLimit("/api/admin/instances/create", limit.max, time.Minute), handler)
		case "/api/admin/instances/1/update-price/preview":
			router.POST("/api/admin/instances/:id/update-price/preview",
				adminBillingRateLimit("/api/admin/instances/:id/update-price/preview", limit.max, time.Minute), handler)
		default:
			suffix := strings.TrimPrefix(limit.path, "/api/admin/instances/1")
			router.POST("/api/admin/instances/:id"+suffix,
				adminBillingRateLimit("/api/admin/instances/:id"+suffix, limit.max, time.Minute), handler)
		}
	}

	do := func(method, path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, nil)
		request.RemoteAddr = "198.51.100.7:5000"
		router.ServeHTTP(recorder, request)
		return recorder
	}

	for _, limit := range limits {
		for attempt := 1; attempt <= limit.max; attempt++ {
			if got := do(limit.method, limit.path).Code; got != http.StatusOK {
				t.Fatalf("%s attempt %d = %d, want 200", limit.path, attempt, got)
			}
		}
		exhausted := do(limit.method, limit.path)
		if exhausted.Code != http.StatusTooManyRequests {
			t.Fatalf("%s limit=%d → %d, want 429", limit.path, limit.max, exhausted.Code)
		}
		if !strings.Contains(exhausted.Body.String(), "Too many requests, please try again later") {
			t.Fatalf("%s 429 body = %s", limit.path, exhausted.Body.String())
		}
	}
}

// ==================== database-backed tests ====================

var (
	adminBillingDBOnce sync.Once
	adminBillingDBErr  error
)

// adminBillingTestDB mirrors backupsTestDatabase: explicit opt-in, local host, and
// a database name containing "_test".
func adminBillingTestDB(t *testing.T) {
	t.Helper()
	if os.Getenv("PAYINCUS_GO_DB_TESTS") != "1" {
		t.Skip("set PAYINCUS_GO_DB_TESTS=1 and DATABASE_URL to run the database-backed admin-billing tests")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set")
	}
	lower := strings.ToLower(dsn)
	if !strings.Contains(lower, "@127.0.0.1") && !strings.Contains(lower, "@localhost") {
		t.Fatalf("refusing to run against a non-local database: %s", dsn)
	}
	if !strings.Contains(lower, "_test") {
		t.Fatalf("refusing to run against a database whose name does not contain \"_test\": %s", dsn)
	}
	adminBillingDBOnce.Do(func() { adminBillingDBErr = db.Init(context.Background()) })
	if adminBillingDBErr != nil {
		t.Fatalf("db.Init: %v", adminBillingDBErr)
	}
}

// adminBillingFixture is the seeded world of one DB test.
type adminBillingFixture struct {
	AdminID    int32
	UserID     int32
	HostID     int32
	PackageID  int32
	PlanID     int32
	InstanceA  int32
	InstanceB  int32
	PoorUserID int32
}

const (
	adminBillingFixturePoorUserID = int32(996101)
	adminBillingFixtureHostID     = int32(996001)
	adminBillingFixturePackageID  = int32(996001)
	adminBillingFixturePlanID     = int32(996001)
	adminBillingFixturePlan2ID    = int32(996002)
	adminBillingFixtureInstA      = int32(996001)
	adminBillingFixtureInstB      = int32(996002)
	adminBillingFixturePoorName   = "parityadminbilling"
)

// adminBillingSeedFixture rebuilds the fixture rows. It deletes first so a failed
// previous run cannot poison the next one (this database is a throwaway test DB by
// construction — the gate above enforces the "_test" suffix).
func adminBillingSeedFixture(t *testing.T) adminBillingFixture {
	t.Helper()
	fixture := adminBillingFixture{
		HostID:    adminBillingFixtureHostID,
		PackageID: adminBillingFixturePackageID,
		PlanID:    adminBillingFixturePlanID,
		InstanceA: adminBillingFixtureInstA,
		InstanceB: adminBillingFixtureInstB,
	}

	if err := db.DB.Raw("SELECT id FROM users WHERE username = ?", "admin").Scan(&fixture.AdminID).Error; err != nil {
		t.Fatalf("load admin: %v", err)
	}
	if fixture.AdminID == 0 {
		t.Skip("admin user missing from the test database")
	}
	if err := db.DB.Raw("SELECT id FROM users WHERE username = ?", "parityuser").Scan(&fixture.UserID).Error; err != nil {
		t.Fatalf("load parityuser: %v", err)
	}
	if fixture.UserID == 0 {
		t.Skip("parityuser missing from the test database")
	}

	// Cleanup is by incus_id prefix so a row left behind by an earlier failed run
	// (including an instance that was created before the fixed ids were used) can
	// never block the host delete.
	cleanup := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM instance_billing_records WHERE instance_id IN
			(SELECT id FROM instances WHERE incus_id LIKE 'parity-adminbilling-%')`, nil},
		{`DELETE FROM balance_logs WHERE instance_id IN
			(SELECT id FROM instances WHERE incus_id LIKE 'parity-adminbilling-%')`, nil},
		{`DELETE FROM aff_bindings WHERE instance_id IN
			(SELECT id FROM instances WHERE incus_id LIKE 'parity-adminbilling-%')`, nil},
		{`DELETE FROM instances WHERE incus_id LIKE 'parity-adminbilling-%'`, nil},
		{`DELETE FROM package_plans WHERE id IN (?,?)`,
			[]any{adminBillingFixturePlanID, adminBillingFixturePlan2ID}},
		{`DELETE FROM packages WHERE id = ?`, []any{adminBillingFixturePackageID}},
		{`DELETE FROM hosts WHERE id = ?`, []any{adminBillingFixtureHostID}},
		{`DELETE FROM financial_reconciliation_items WHERE run_id IN
			(SELECT id FROM financial_reconciliation_runs WHERE date IN ('2020-01-15'::date, '2019-06-01'::date))`, nil},
		{`DELETE FROM financial_reconciliation_runs WHERE date IN ('2020-01-15'::date, '2019-06-01'::date)`, nil},
		{`DELETE FROM recharge_records WHERE order_no = ?`, []any{"PARITY-ADMINBILLING-1"}},
	}
	for _, statement := range cleanup {
		if err := db.DB.Exec(statement.query, statement.args...).Error; err != nil {
			t.Fatalf("cleanup %q: %v", statement.query, err)
		}
	}

	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO hosts (id, user_id, name, url, status, cpu_used, memory_used, disk_used,
			cpu_allowance_max, memory_max, storage_size, storage_driver, storage_type,
			instance_type, created_at, updated_at)
		 VALUES (?, ?, 'Parity AdminBilling Host', 'https://host.adminbilling.invalid:8443', 'online',
			0, 0, 0, 10000, 1048576, 1000, 'zfs', 'loop', 'container',
			now() - interval '40 days', now() - interval '40 days')`,
			[]any{adminBillingFixtureHostID, fixture.AdminID}},
		{`INSERT INTO packages (id, user_id, name, description, cpu_max, memory_max, disk_max,
			network_mode, instance_type, port_limit, snapshot_limit, backup_limit, site_limit,
			monthly_traffic_limit, active, created_at)
		 VALUES (?, ?, 'Parity AdminBilling Package', 'admin-billing fixture', 800, 4096, 60000,
			'nat', 'container', 20, 5, 3, 10, 1099511627776, true, now() - interval '40 days')`,
			[]any{adminBillingFixturePackageID, fixture.AdminID}},
		{`INSERT INTO package_plans (id, package_id, name, cpu, memory, disk, port_limit, snapshot_limit,
			backup_limit, site_limit, traffic_limit, traffic_limit_speed, price, billing_cycle,
			is_active, is_sold_out, sort_order, swap_size, created_at, updated_at)
		 VALUES (?, ?, 'Parity AdminBilling Plan', 1, 1024, 10240, 20, 5, 3, 10,
			1099511627776, '1Mbit', 3000.00, 1, true, false, 0, 0,
			now() - interval '40 days', now() - interval '40 days')`,
			[]any{adminBillingFixturePlanID, adminBillingFixturePackageID}},
		{`INSERT INTO package_plans (id, package_id, name, cpu, memory, disk, port_limit, snapshot_limit,
			backup_limit, site_limit, traffic_limit, traffic_limit_speed, price, billing_cycle,
			is_active, is_sold_out, sort_order, swap_size, created_at, updated_at)
		 VALUES (?, ?, 'Parity AdminBilling Plan 2', 2, 2048, 10240, 20, 5, 3, 10,
			1099511627776, '1Mbit', 6000.00, 1, true, false, 1, 0,
			now() - interval '40 days', now() - interval '40 days')`,
			[]any{adminBillingFixturePlan2ID, adminBillingFixturePackageID}},
	}
	for _, statement := range statements {
		if err := db.DB.Exec(statement.query, statement.args...).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// A second owner lets the batch settle per user; both start funded so the
	// preview passes and the failure has to come from the probe trigger.
	poor := struct {
		ID int32
	}{}
	if err := db.DB.Raw("SELECT id FROM users WHERE username = ?", adminBillingFixturePoorName).Scan(&poor.ID).Error; err != nil {
		t.Fatalf("load poor user: %v", err)
	}
	if poor.ID == 0 {
		// An explicit id keeps the fixture stable across runs and avoids depending
		// on the users sequence (the identity rows are copied in by the harness).
		if err := db.DB.Exec(
			`INSERT INTO users (id, username, password_hash, role, status, balance, avatar_style, created_at, updated_at)
			 VALUES (?, ?, 'x', 'user', 'active', 0, 'bigSmile', now(), now())`,
			adminBillingFixturePoorUserID, adminBillingFixturePoorName,
		).Error; err != nil {
			t.Fatalf("create poor user: %v", err)
		}
		poor.ID = adminBillingFixturePoorUserID
	} else if err := db.DB.Exec("UPDATE users SET balance = 1000, status = 'active' WHERE id = ?", poor.ID).Error; err != nil {
		t.Fatalf("reset poor user: %v", err)
	}
	fixture.PoorUserID = poor.ID

	// Reset both owners' balances to a known value.
	for _, ownerID := range []int32{fixture.UserID, fixture.PoorUserID} {
		if err := db.DB.Exec("UPDATE users SET balance = 1000 WHERE id = ?", ownerID).Error; err != nil {
			t.Fatalf("reset balance: %v", err)
		}
	}

	insertInstance := func(id int32, ownerID int32, name string) {
		if err := db.DB.Exec(`INSERT INTO instances (
				id, incus_id, name, user_id, host_id, package_id, package_plan_id, image, status,
				cpu, memory, disk, network_mode, billing_price, billing_cycle, expires_at,
				auto_renew, version, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'images:debian/12', 'running', 1, 1024, 10240, 'nat',
				30.00, 1, now() + interval '30 days', false, 0,
				now() - interval '40 days', now() - interval '1 day')`,
			id, fmt.Sprintf("parity-adminbilling-%d", id), name, ownerID, adminBillingFixtureHostID,
			adminBillingFixturePackageID, adminBillingFixturePlanID,
		).Error; err != nil {
			t.Fatalf("insert instance %d: %v", id, err)
		}
		// The billing lineage needs a positive purchase period so the refund quote
		// has a remaining value: half of a 60-day window is still unused.
		if err := db.DB.Exec(`INSERT INTO instance_billing_records
				(instance_id, user_id, type, amount, months, period_start, period_end, remark, created_at)
			VALUES (?, ?, 'newPurchase', 30.00, 1, now() - interval '30 days', now() + interval '30 days',
				'admin-billing fixture purchase', now() - interval '30 days')`,
			id, ownerID,
		).Error; err != nil {
			t.Fatalf("insert billing record: %v", err)
		}
	}
	insertInstance(adminBillingFixtureInstA, fixture.UserID, "parity-adminbilling-a")
	insertInstance(adminBillingFixtureInstB, fixture.PoorUserID, "parity-adminbilling-b")

	t.Cleanup(func() {
		_ = db.DB.Exec(`DROP TRIGGER IF EXISTS admin_billing_fail_update ON instances`).Error
		_ = db.DB.Exec(`DROP FUNCTION IF EXISTS admin_billing_fail_update()`).Error
	})
	return fixture
}

// adminBillingToken signs a Web JWT for the given identity.
func adminBillingToken(t *testing.T, userID int32, username, role string) string {
	t.Helper()
	t.Setenv("JWT_SECRET", "admin-billing-test-secret")
	token, err := lib.SignAccessToken(lib.AccessTokenClaims{
		ID:       userID,
		Username: username,
		Role:     role,
		Status:   "active",
		SID:      "admin-billing-test",
		IssuedAt: time.Now().Unix(),
		Expires:  time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("SignAccessToken: %v", err)
	}
	return token
}

func adminBillingJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
	}
	return payload
}

// TestAdminBillingDBRefundQuoteIsTheExecutedAmount proves the "one quote" rule:
// the preview endpoint's priceDiff is exactly what the update-price endpoint
// settles (balance delta) for the same inputs.
func TestAdminBillingDBRefundQuoteIsTheExecutedAmount(t *testing.T) {
	adminBillingTestDB(t)
	ResetAdminBillingRateLimits()
	t.Cleanup(ResetAdminBillingRateLimits)
	fixture := adminBillingSeedFixture(t)
	router := adminBillingTestRouter()
	token := adminBillingToken(t, fixture.AdminID, "admin", "admin")

	instancePath := fmt.Sprintf("/api/admin/instances/%d", fixture.InstanceA)
	previewBody := `{"newPrice":45,"settleBalance":true}`
	preview := adminBillingTestRequest(router, http.MethodPost, instancePath+"/update-price/preview", token, previewBody)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", preview.Code, preview.Body.String())
	}
	previewPayload := adminBillingJSON(t, preview)
	priceDiff, ok := previewPayload["priceDiff"].(float64)
	if !ok {
		t.Fatalf("preview priceDiff missing: %s", preview.Body.String())
	}
	if priceDiff <= 0 {
		t.Fatalf("priceDiff = %v, want a positive top-up for a 30 → 45 increase", priceDiff)
	}

	var balanceBefore models.Decimal
	if err := db.DB.Raw("SELECT balance FROM users WHERE id = ?", fixture.UserID).Scan(&balanceBefore).Error; err != nil {
		t.Fatalf("read balance: %v", err)
	}

	apply := adminBillingTestRequest(router, http.MethodPost, instancePath+"/update-price", token, previewBody)
	if apply.Code != http.StatusOK {
		t.Fatalf("update-price = %d %s", apply.Code, apply.Body.String())
	}
	applyPayload := adminBillingJSON(t, apply)
	if applied, _ := applyPayload["priceDiff"].(float64); applied != priceDiff {
		t.Fatalf("applied priceDiff = %v, preview = %v", applyPayload["priceDiff"], priceDiff)
	}

	var balanceAfter models.Decimal
	if err := db.DB.Raw("SELECT balance FROM users WHERE id = ?", fixture.UserID).Scan(&balanceAfter).Error; err != nil {
		t.Fatalf("read balance: %v", err)
	}
	delta := balanceBefore.InexactFloat64() - balanceAfter.InexactFloat64()
	if math.Abs(delta-priceDiff) > 1e-9 {
		t.Fatalf("balance delta = %v, previewed priceDiff = %v", delta, priceDiff)
	}

	// The billing record and the balance log must carry the same amount, and the
	// log must hold before/after snapshots (never deltas).
	var logRow struct {
		Amount        models.Decimal `gorm:"column:amount"`
		BalanceBefore models.Decimal `gorm:"column:balance_before"`
		BalanceAfter  models.Decimal `gorm:"column:balance_after"`
	}
	if err := db.DB.Raw(`SELECT amount, balance_before, balance_after FROM balance_logs
		WHERE instance_id = ? ORDER BY id DESC LIMIT 1`, fixture.InstanceA).Scan(&logRow).Error; err != nil {
		t.Fatalf("read balance log: %v", err)
	}
	if logRow.Amount.InexactFloat64() != -priceDiff {
		t.Fatalf("balance log amount = %v, want %v", logRow.Amount.InexactFloat64(), -priceDiff)
	}
	if logRow.BalanceBefore.InexactFloat64() != balanceBefore.InexactFloat64() ||
		logRow.BalanceAfter.InexactFloat64() != balanceAfter.InexactFloat64() {
		t.Fatalf("snapshots = %v/%v, want %v/%v",
			logRow.BalanceBefore, logRow.BalanceAfter, balanceBefore, balanceAfter)
	}
}

// TestAdminBillingDBRefundIsIdempotent proves a doubled refund request credits
// once (the TypeScript would credit twice: nothing guards a partial refund).
func TestAdminBillingDBRefundIsIdempotent(t *testing.T) {
	adminBillingTestDB(t)
	ResetAdminBillingRateLimits()
	t.Cleanup(ResetAdminBillingRateLimits)
	fixture := adminBillingSeedFixture(t)
	router := adminBillingTestRouter()
	token := adminBillingToken(t, fixture.AdminID, "admin", "admin")

	body := `{"amount":10,"reason":"重复提交测试"}`
	path := fmt.Sprintf("/api/admin/instances/%d/refund", fixture.InstanceA)

	first := adminBillingTestRequest(router, http.MethodPost, path, token, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first refund = %d %s", first.Code, first.Body.String())
	}
	second := adminBillingTestRequest(router, http.MethodPost, path, token, body)
	if second.Code != http.StatusOK {
		t.Fatalf("second refund = %d %s", second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replay response differs:\n  first  = %s\n  second = %s", first.Body.String(), second.Body.String())
	}

	var refundCount int64
	if err := db.DB.Raw(`SELECT COUNT(*) FROM instance_billing_records
		WHERE instance_id = ? AND type = 'refund'`, fixture.InstanceA).Scan(&refundCount).Error; err != nil {
		t.Fatalf("count refund records: %v", err)
	}
	if refundCount != 1 {
		t.Fatalf("refund records = %d, want 1 (a replay must not credit twice)", refundCount)
	}

	var balance models.Decimal
	if err := db.DB.Raw("SELECT balance FROM users WHERE id = ?", fixture.UserID).Scan(&balance).Error; err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if balance.InexactFloat64() != 1010 {
		t.Fatalf("balance = %v, want 1010 (one 10 credit)", balance.InexactFloat64())
	}

	// The cap is enforced before any write.
	over := adminBillingTestRequest(router, http.MethodPost, path, token, `{"amount":1000,"reason":"超限测试"}`)
	if over.Code != http.StatusBadRequest || !strings.Contains(over.Body.String(), "退款金额超过可退上限") {
		t.Fatalf("over-cap refund = %d %s", over.Code, over.Body.String())
	}
}

// TestAdminBillingDBConcurrentRefundsStayUnderThePaidCap proves the paid-amount
// ceiling holds under concurrency: two simultaneous partial refunds that each fit
// the cap cannot together exceed it (the second transaction re-checks the cap under
// the 4110 lock).
func TestAdminBillingDBConcurrentRefundsStayUnderThePaidCap(t *testing.T) {
	adminBillingTestDB(t)
	ResetAdminBillingRateLimits()
	t.Cleanup(ResetAdminBillingRateLimits)
	fixture := adminBillingSeedFixture(t)
	router := adminBillingTestRouter()
	token := adminBillingToken(t, fixture.AdminID, "admin", "admin")

	cap, err := db.GetInstanceBillingMaxRefundable(context.Background(), fixture.InstanceA)
	if err != nil {
		t.Fatalf("cap: %v", err)
	}
	each := cap * 2 / 3
	if each <= 0 {
		t.Fatalf("cap %v too small for the concurrency probe", cap)
	}

	path := fmt.Sprintf("/api/admin/instances/%d/refund", fixture.InstanceA)
	bodies := []string{
		fmt.Sprintf(`{"amount":%v,"reason":"并发退款甲"}`, each),
		fmt.Sprintf(`{"amount":%v,"reason":"并发退款乙"}`, each),
	}

	results := make([]*httptest.ResponseRecorder, len(bodies))
	var wg sync.WaitGroup
	for index, body := range bodies {
		wg.Add(1)
		go func(index int, body string) {
			defer wg.Done()
			results[index] = adminBillingTestRequest(router, http.MethodPost, path, token, body)
		}(index, body)
	}
	wg.Wait()

	okCount := 0
	for _, result := range results {
		switch result.Code {
		case http.StatusOK:
			okCount++
		case http.StatusBadRequest:
			// The in-transaction cap rejection.
			if !strings.Contains(result.Body.String(), "退款金额超过可退上限") {
				t.Fatalf("unexpected 400 body: %s", result.Body.String())
			}
		case http.StatusConflict:
			// The 4110 try-lock loser: the TypeScript answers 409 BALANCE_CONFLICT
			// when another balance mutation holds the lock.
			if !strings.Contains(result.Body.String(), "用户余额正在处理") {
				t.Fatalf("unexpected 409 body: %s", result.Body.String())
			}
		default:
			t.Fatalf("unexpected status %d: %s", result.Code, result.Body.String())
		}
	}
	if okCount != 1 {
		t.Fatalf("successful refunds = %d, want exactly 1 (cap = %v, each = %v)", okCount, cap, each)
	}

	var refunded models.Decimal
	if err := db.DB.Raw(`SELECT COALESCE(SUM(ABS(amount)),0)::numeric FROM instance_billing_records
		WHERE instance_id = ? AND type = 'refund'`, fixture.InstanceA).Scan(&refunded).Error; err != nil {
		t.Fatalf("sum refunds: %v", err)
	}
	if refunded.InexactFloat64() > cap+1e-9 {
		t.Fatalf("total refunded %v exceeds the paid cap %v", refunded.InexactFloat64(), cap)
	}

	var balance models.Decimal
	if err := db.DB.Raw("SELECT balance FROM users WHERE id = ?", fixture.UserID).Scan(&balance).Error; err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if math.Abs(balance.InexactFloat64()-(1000+refunded.InexactFloat64())) > 1e-9 {
		t.Fatalf("balance = %v, want 1000 + %v", balance.InexactFloat64(), refunded.InexactFloat64())
	}
}

// TestAdminBillingDBBatchPriceUpdateIsAtomic proves a failure on the second
// instance rolls the first one back: the trigger installed on `instances` makes the
// second version bump raise, so neither price nor any balance may move.
func TestAdminBillingDBBatchPriceUpdateIsAtomic(t *testing.T) {
	adminBillingTestDB(t)
	ResetAdminBillingRateLimits()
	t.Cleanup(ResetAdminBillingRateLimits)
	fixture := adminBillingSeedFixture(t)
	router := adminBillingTestRouter()
	token := adminBillingToken(t, fixture.AdminID, "admin", "admin")

	previewBody := fmt.Sprintf(`{"instanceIds":[%d,%d],"newPrice":40,"settleBalance":true}`,
		fixture.InstanceA, fixture.InstanceB)
	preview := adminBillingTestRequest(router, http.MethodPost,
		"/api/admin/instances/batch-update-price/preview", token, previewBody)
	if preview.Code != http.StatusOK {
		t.Fatalf("batch preview = %d %s", preview.Code, preview.Body.String())
	}
	previewPayload := adminBillingJSON(t, preview)
	items, _ := previewPayload["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("preview items = %d", len(items))
	}

	var expectations []map[string]any
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		expectations = append(expectations, map[string]any{
			"instanceId": item["id"],
			"version":    item["instanceVersion"],
		})
	}
	applyBody, err := json.Marshal(map[string]any{
		"instanceIds":   []any{fixture.InstanceA, fixture.InstanceB},
		"newPrice":      40,
		"settleBalance": true,
		"expectations":  expectations,
	})
	if err != nil {
		t.Fatalf("marshal apply body: %v", err)
	}

	if err := db.DB.Exec(`
		CREATE OR REPLACE FUNCTION admin_billing_fail_update() RETURNS trigger AS $$
		BEGIN
			IF NEW.id = ` + fmt.Sprint(fixture.InstanceB) + ` THEN
				RAISE EXCEPTION 'admin-billing atomicity probe';
			END IF;
			RETURN NEW;
		END; $$ LANGUAGE plpgsql`).Error; err != nil {
		t.Fatalf("create probe function: %v", err)
	}
	if err := db.DB.Exec(`CREATE TRIGGER admin_billing_fail_update
		BEFORE UPDATE ON instances FOR EACH ROW EXECUTE FUNCTION admin_billing_fail_update()`).Error; err != nil {
		t.Fatalf("create probe trigger: %v", err)
	}

	apply := adminBillingTestRequest(router, http.MethodPost,
		"/api/admin/instances/batch-update-price", token, string(applyBody))
	if apply.Code != http.StatusInternalServerError {
		t.Fatalf("batch apply = %d %s, want 500", apply.Code, apply.Body.String())
	}

	_ = db.DB.Exec(`DROP TRIGGER IF EXISTS admin_billing_fail_update ON instances`).Error
	_ = db.DB.Exec(`DROP FUNCTION IF EXISTS admin_billing_fail_update()`).Error

	// Nothing moved: neither price, nor version, nor balance.
	for _, instanceID := range []int32{fixture.InstanceA, fixture.InstanceB} {
		var row struct {
			BillingPrice models.Decimal `gorm:"column:billing_price"`
			Version      int32          `gorm:"column:version"`
		}
		if err := db.DB.Raw("SELECT billing_price, version FROM instances WHERE id = ?", instanceID).
			Scan(&row).Error; err != nil {
			t.Fatalf("read instance %d: %v", instanceID, err)
		}
		if row.BillingPrice.InexactFloat64() != 30 {
			t.Fatalf("instance %d price = %v, want 30 (rollback)", instanceID, row.BillingPrice)
		}
		if row.Version != 0 {
			t.Fatalf("instance %d version = %d, want 0 (rollback)", instanceID, row.Version)
		}
	}
	var refunds int64
	if err := db.DB.Raw(`SELECT COUNT(*) FROM instance_billing_records
		WHERE instance_id IN (?,?) AND type IN ('upgrade','refund') AND created_at > now() - interval '1 hour'`,
		fixture.InstanceA, fixture.InstanceB).Scan(&refunds).Error; err != nil {
		t.Fatalf("count records: %v", err)
	}
	if refunds != 0 {
		t.Fatalf("billing records written during a rolled-back batch: %d", refunds)
	}

	for _, ownerID := range []int32{fixture.UserID, fixture.PoorUserID} {
		var balance models.Decimal
		if err := db.DB.Raw("SELECT balance FROM users WHERE id = ?", ownerID).Scan(&balance).Error; err != nil {
			t.Fatalf("read balance %d: %v", ownerID, err)
		}
		if balance.InexactFloat64() != 1000 {
			t.Fatalf("user %d balance = %v, want 1000 (rollback)", ownerID, balance.InexactFloat64())
		}
	}
}

// TestAdminBillingDBReconciliationIsIdempotent proves the run is unique per date,
// items are stable across reruns, and an operator's decision survives a rerun.
func TestAdminBillingDBReconciliationIsIdempotent(t *testing.T) {
	adminBillingTestDB(t)
	ResetAdminBillingRateLimits()
	t.Cleanup(ResetAdminBillingRateLimits)
	fixture := adminBillingSeedFixture(t)
	router := adminBillingTestRouter()
	token := adminBillingToken(t, fixture.AdminID, "admin", "admin")

	// One completed recharge with no matching balance log, placed inside the
	// 2020-01-15 business day so no other row can join the window.
	orderNo := "PARITY-ADMINBILLING-1"
	var providerID int32
	if err := db.DB.Raw("SELECT id FROM payment_providers LIMIT 1").Scan(&providerID).Error; err != nil || providerID == 0 {
		t.Skip("no payment provider row in the test database")
	}
	if err := db.DB.Exec(`INSERT INTO recharge_records
			(user_id, provider_id, order_no, amount, actual_amount, fee, status, completed_at, created_at, updated_at)
		 VALUES (?, ?, ?, 30.00, 30.00, 0, 'completed', '2020-01-15 04:00:00', '2020-01-15 04:00:00', '2020-01-15 04:00:00')`,
		fixture.UserID, providerID, orderNo).Error; err != nil {
		t.Fatalf("seed recharge: %v", err)
	}
	t.Cleanup(func() { _ = db.DB.Exec("DELETE FROM recharge_records WHERE order_no = ?", orderNo).Error })

	runBody := `{"date":"2020-01-15"}`
	first := adminBillingTestRequest(router, http.MethodPost, "/api/admin/billing/reconciliation/run", token, runBody)
	if first.Code != http.StatusOK {
		t.Fatalf("first run = %d %s", first.Code, first.Body.String())
	}
	firstRun, _ := adminBillingJSON(t, first)["run"].(map[string]any)
	firstID := int32(firstRun["id"].(float64))
	firstItems, _ := firstRun["items"].([]any)
	if len(firstItems) != 1 {
		t.Fatalf("first run items = %d, want 1 (the seeded recharge)", len(firstItems))
	}
	if status, _ := firstRun["status"].(string); status != "discrepancy" {
		t.Fatalf("first run status = %v", firstRun["status"])
	}

	second := adminBillingTestRequest(router, http.MethodPost, "/api/admin/billing/reconciliation/run", token, runBody)
	if second.Code != http.StatusOK {
		t.Fatalf("second run = %d %s", second.Code, second.Body.String())
	}
	secondRun, _ := adminBillingJSON(t, second)["run"].(map[string]any)
	if int32(secondRun["id"].(float64)) != firstID {
		t.Fatalf("rerun created a second run (%v → %v)", firstID, secondRun["id"])
	}
	secondItems, _ := secondRun["items"].([]any)
	if len(secondItems) != len(firstItems) {
		t.Fatalf("rerun duplicated items: %d → %d", len(firstItems), len(secondItems))
	}
	firstItemID := int32(firstItems[0].(map[string]any)["id"].(float64))
	if int32(secondItems[0].(map[string]any)["id"].(float64)) != firstItemID {
		t.Fatalf("rerun replaced the item (key must be stable)")
	}

	// An operator confirms the item; a rerun must keep that decision.
	patch := adminBillingTestRequest(router, http.MethodPatch,
		fmt.Sprintf("/api/admin/billing/reconciliation/items/%d", firstItemID), token,
		`{"status":"confirmed","note":"已核对"}`)
	if patch.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", patch.Code, patch.Body.String())
	}
	if runStatus, _ := adminBillingJSON(t, patch)["runStatus"].(string); runStatus != "confirmed" {
		t.Fatalf("runStatus after confirm = %v", adminBillingJSON(t, patch)["runStatus"])
	}

	third := adminBillingTestRequest(router, http.MethodPost, "/api/admin/billing/reconciliation/run", token, runBody)
	if third.Code != http.StatusOK {
		t.Fatalf("third run = %d %s", third.Code, third.Body.String())
	}
	thirdRun, _ := adminBillingJSON(t, third)["run"].(map[string]any)
	items, _ := thirdRun["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items after confirm + rerun = %d", len(items))
	}
	item := items[0].(map[string]any)
	if status, _ := item["status"].(string); status != "confirmed" {
		t.Fatalf("operator decision lost on rerun: status = %v", item["status"])
	}
	if note, _ := item["note"].(string); note != "已核对" {
		t.Fatalf("operator note lost on rerun: %v", item["note"])
	}
	if status, _ := thirdRun["status"].(string); status != "confirmed" {
		t.Fatalf("run status = %v, want confirmed", thirdRun["status"])
	}

	// A day with nothing to reconcile yields a `normal` run.
	empty := adminBillingTestRequest(router, http.MethodPost, "/api/admin/billing/reconciliation/run", token,
		`{"date":"2019-06-01"}`)
	if empty.Code != http.StatusOK {
		t.Fatalf("empty run = %d %s", empty.Code, empty.Body.String())
	}
	emptyRun, _ := adminBillingJSON(t, empty)["run"].(map[string]any)
	if status, _ := emptyRun["status"].(string); status != "normal" {
		t.Fatalf("empty run status = %v, want normal", emptyRun["status"])
	}
	if items, _ := emptyRun["items"].([]any); len(items) != 0 {
		t.Fatalf("empty run items = %d", len(items))
	}
	t.Cleanup(func() {
		_ = db.DB.Exec(`DELETE FROM financial_reconciliation_items WHERE run_id IN
			(SELECT id FROM financial_reconciliation_runs WHERE date = '2019-06-01'::date)`).Error
		_ = db.DB.Exec(`DELETE FROM financial_reconciliation_runs WHERE date = '2019-06-01'::date`).Error
	})
}

// TestAdminBillingDBDeleteAndRefundSettlesOnce proves the destructive path:
// the claim (4106 + status CAS) happens once, the refund equals the shared quote,
// a replay is rejected with "实例已删除", and the instance is only refunded once.
func TestAdminBillingDBDeleteAndRefundSettlesOnce(t *testing.T) {
	adminBillingTestDB(t)
	ResetAdminBillingRateLimits()
	t.Cleanup(ResetAdminBillingRateLimits)
	fixture := adminBillingSeedFixture(t)
	router := adminBillingTestRouter()
	token := adminBillingToken(t, fixture.AdminID, "admin", "admin")

	// No email on the owner: the route sends a best-effort refund email and the
	// test must not depend on an SMTP server.
	if err := db.DB.Exec("UPDATE users SET email = NULL WHERE id = ?", fixture.PoorUserID).Error; err != nil {
		t.Fatalf("clear email: %v", err)
	}

	// The quote the route must settle: remaining value of instance B's period.
	var instance models.Instance
	if err := db.DB.Raw("SELECT * FROM instances WHERE id = ?", fixture.InstanceB).Scan(&instance).Error; err != nil {
		t.Fatalf("load instance: %v", err)
	}
	quote, err := adminBillingRefundQuote(context.Background(), &instance)
	if err != nil {
		t.Fatalf("refund quote: %v", err)
	}
	if quote.RemainingValue <= 0 || quote.Refundable <= 0 {
		t.Fatalf("fixture has no remaining value: %+v", quote)
	}

	body := fmt.Sprintf(`{"refundType":"remaining","reason":"parity delete","databaseOnly":true}`)
	path := fmt.Sprintf("/api/admin/instances/%d/delete-and-refund", fixture.InstanceB)
	first := adminBillingTestRequest(router, http.MethodPost, path, token, body)
	if first.Code != http.StatusOK {
		t.Fatalf("delete-and-refund = %d %s", first.Code, first.Body.String())
	}
	payload := adminBillingJSON(t, first)
	refunded, _ := payload["refundAmount"].(float64)
	if math.Abs(refunded-quote.Refundable) > 1e-9 {
		t.Fatalf("refunded = %v, quote = %v", refunded, quote.Refundable)
	}

	var status models.InstanceStatus
	if err := db.DB.Raw("SELECT status FROM instances WHERE id = ?", fixture.InstanceB).Scan(&status).Error; err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != models.InstanceStatusDeleted {
		t.Fatalf("status = %s, want deleted", status)
	}

	var balance models.Decimal
	if err := db.DB.Raw("SELECT balance FROM users WHERE id = ?", fixture.PoorUserID).Scan(&balance).Error; err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if math.Abs(balance.InexactFloat64()-(1000+quote.Refundable)) > 1e-9 {
		t.Fatalf("balance = %v, want 1000 + %v", balance.InexactFloat64(), quote.Refundable)
	}

	var refunds int64
	if err := db.DB.Raw(`SELECT COUNT(*) FROM instance_billing_records
		WHERE instance_id = ? AND type = 'refund'`, fixture.InstanceB).Scan(&refunds).Error; err != nil {
		t.Fatalf("count refunds: %v", err)
	}
	if refunds != 1 {
		t.Fatalf("refund records = %d, want 1", refunds)
	}

	// The claim is the idempotency barrier: a replay finds status 'deleted'.
	second := adminBillingTestRequest(router, http.MethodPost, path, token, body)
	if second.Code != http.StatusBadRequest || !strings.Contains(second.Body.String(), "实例已删除") {
		t.Fatalf("replay = %d %s", second.Code, second.Body.String())
	}
	if err := db.DB.Raw("SELECT balance FROM users WHERE id = ?", fixture.PoorUserID).Scan(&balance).Error; err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if math.Abs(balance.InexactFloat64()-(1000+quote.Refundable)) > 1e-9 {
		t.Fatalf("replay credited again: balance = %v", balance.InexactFloat64())
	}
}

// TestAdminBillingDBNonAdminForbidden proves the requireAdmin layer: a valid token
// for a role=user account is a 403, not a 401 and not a handler run.
func TestAdminBillingDBNonAdminForbidden(t *testing.T) {
	adminBillingTestDB(t)
	ResetAdminBillingRateLimits()
	t.Cleanup(ResetAdminBillingRateLimits)
	fixture := adminBillingSeedFixture(t)
	router := adminBillingTestRouter()
	token := adminBillingToken(t, fixture.UserID, "parityuser", "user")

	for _, route := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/admin/billing/overview", ""},
		{http.MethodGet, "/api/admin/billing/records", ""},
		{http.MethodGet, "/api/admin/billing/reconciliation", ""},
		{http.MethodPost, "/api/admin/billing/reconciliation/run", `{"date":"2020-01-15"}`},
		{http.MethodPost, fmt.Sprintf("/api/admin/instances/%d/refund", fixture.InstanceA),
			`{"amount":1,"reason":"x"}`},
		{http.MethodPost, fmt.Sprintf("/api/admin/instances/%d/update-price", fixture.InstanceA),
			`{"newPrice":1,"settleBalance":false}`},
		{http.MethodPost, "/api/admin/instances/batch-update-price", `{}`},
		{http.MethodPost, "/api/admin/instances/create", `{}`},
	} {
		recorder := adminBillingTestRequest(router, route.method, route.path, token, route.body)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s %s as role=user = %d %s, want 403",
				route.method, route.path, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "Admin privileges required") {
			t.Errorf("%s %s body = %s", route.method, route.path, recorder.Body.String())
		}
	}
}
