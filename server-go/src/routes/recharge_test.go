// Recharge route tests.
//
// Two layers:
//
//  1. pure unit tests (no database) pinning the parsers, the amount tolerance and
//     the response shape of buildRechargeRecordView;
//  2. database-backed end-to-end callback tests. They drive the real Gin router
//     with a real MD5-signed callback against a real PostgreSQL, and assert the
//     money invariants: a bad signature or a mismatched amount must not credit,
//     a valid callback credits exactly once, a replay is a no-op, and a
//     concurrent replay still credits once.
//
// The DB tests are opt-in exactly like db/quota_operations_test.go:
// PAYINCUS_GO_DB_TESTS=1, a local DATABASE_URL, and a database whose name contains
// "_test". Nothing here can reach a production or parity database by accident.
package routes

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== pure tests ====================

func TestRechargeParsePositiveID(t *testing.T) {
	cases := []struct {
		value    any
		expected int64
		ok       bool
	}{
		{float64(1), 1, true},
		{float64(0), 0, false},
		{float64(-3), 0, false},
		{float64(1.5), 0, false},
		{"42", 42, true},
		{"0", 0, false},
		{"01", 0, false},
		{"-1", 0, false},
		{"1e3", 0, false},
		{"", 0, false},
		{nil, 0, false},
		{true, 0, false},
	}
	for _, testCase := range cases {
		got := rechargeParsePositiveID(testCase.value)
		if !testCase.ok {
			if got != nil {
				t.Fatalf("%v: expected nil, got %d", testCase.value, *got)
			}
			continue
		}
		if got == nil || *got != testCase.expected {
			t.Fatalf("%v: expected %d, got %v", testCase.value, testCase.expected, got)
		}
	}
	if got := rechargeParseOptionalPositiveInteger("abc", 20); got != 20 {
		t.Fatalf("invalid query must fall back, got %d", got)
	}
	if got := rechargeParseOptionalPositiveInteger("", 20); got != 20 {
		t.Fatalf("empty query must fall back, got %d", got)
	}
	if got := rechargeParseOptionalPositiveInteger("7", 20); got != 7 {
		t.Fatalf("valid query must win, got %d", got)
	}
}

func TestRechargeParsePositiveJSONMoney(t *testing.T) {
	if value := rechargeParsePositiveJSONMoney(float64(10.5)); value == nil || *value != 10.5 {
		t.Fatalf("numbers must be accepted: %v", value)
	}
	for _, value := range []any{float64(0), float64(-1), "10", nil, true, json.Number("10")} {
		if got := rechargeParsePositiveJSONMoney(value); got != nil {
			t.Fatalf("%v: only positive JSON numbers are accepted, got %v", value, *got)
		}
	}
}

func TestRechargeAmountTolerance(t *testing.T) {
	// AMOUNT_TOLERANCE_CENTS = 1 -> 0.01, inclusive.
	if !rechargeAmountsMatch(10, 10) {
		t.Fatal("equal amounts must match")
	}
	if !rechargeAmountsMatch(10, 10.01) || !rechargeAmountsMatch(10, 9.99) {
		t.Fatal("a one-cent difference must be tolerated")
	}
	if rechargeAmountsMatch(10, 10.02) || rechargeAmountsMatch(10, 9.98) {
		t.Fatal("more than one cent must not match")
	}
	if rechargeAmountUnderpaid(10, 9.99) {
		t.Fatal("9.99 within tolerance is not underpaid")
	}
	if rechargeAmountUnderpaid(10, 10.00) || rechargeAmountUnderpaid(10, 10.5) {
		t.Fatal("equal/over payment is not underpaid")
	}
	if !rechargeAmountUnderpaid(10, 9.98) {
		t.Fatal("9.98 is underpaid")
	}
}

func TestRechargeFixed2(t *testing.T) {
	cases := map[float64]string{10: "10.00", 10.5: "10.50", 0.1 + 0.2: "0.30", 1.005: "1.00"}
	for value, expected := range cases {
		if got := rechargeFixed2(value); got != expected {
			t.Fatalf("rechargeFixed2(%v) = %s, want %s", value, got, expected)
		}
	}
}

func TestRechargeOptionalTrimmedString(t *testing.T) {
	if result := rechargeNormalizeOptionalTrimmedString(nil, 128); result.Value != nil || result.Null {
		t.Fatal("nil must be undefined")
	}
	if result := rechargeNormalizeOptionalTrimmedString("  ", 128); result.Value != nil || result.Null {
		t.Fatal("blank must be undefined")
	}
	if result := rechargeNormalizeOptionalTrimmedString(12, 128); !result.Null {
		t.Fatal("non-string must be null")
	}
	if result := rechargeNormalizeOptionalTrimmedString(strings.Repeat("a", 129), 128); !result.Null {
		t.Fatal("too long must be null")
	}
	result := rechargeNormalizeOptionalTrimmedString("  trade-1  ", 128)
	if result.Null || result.Value == nil || *result.Value != "trade-1" {
		t.Fatalf("unexpected normalisation: %+v", result)
	}
}

func TestRechargeBuildEpayConfig(t *testing.T) {
	_, valid, message := rechargeBuildEpayConfig(map[string]any{
		"version": "v1", "apiurl": "https://e.example", "pid": "1", "key": "k",
	})
	if !valid {
		t.Fatalf("v1 config must be valid: %s", message)
	}
	_, valid, message = rechargeBuildEpayConfig(map[string]any{"version": "v1", "apiurl": "https://e"})
	if valid || !strings.Contains(message, "V1版本") {
		t.Fatalf("incomplete v1 must be invalid: %v %q", valid, message)
	}
	_, valid, message = rechargeBuildEpayConfig(map[string]any{
		"apiurl": "https://e", "pid": "1", "platform_public_key": "p", "merchant_private_key": "m",
	})
	if !valid {
		t.Fatalf("v2 default must be valid: %s", message)
	}
	_, valid, message = rechargeBuildEpayConfig(map[string]any{"apiurl": "https://e", "pid": "1"})
	if valid || !strings.Contains(message, "V2版本") {
		t.Fatalf("incomplete v2 must be invalid: %v %q", valid, message)
	}
}

func TestRechargeResolvePaymentMethod(t *testing.T) {
	method := "wxpay"
	if got := rechargeResolvePaymentMethod("yipay", []string{"alipay", "wxpay"}, &method); got == nil || *got != "wxpay" {
		t.Fatal("yipay must honour a supported method")
	}
	unsupported := "card"
	if got := rechargeResolvePaymentMethod("yipay", []string{"alipay"}, &unsupported); got == nil || *got != "alipay" {
		t.Fatal("yipay must fall back to the first supported method")
	}
	if got := rechargeResolvePaymentMethod("yipay", nil, nil); got == nil || *got != "alipay" {
		t.Fatal("yipay defaults to alipay")
	}
	if got := rechargeResolvePaymentMethod("heleket", nil, nil); got != nil {
		t.Fatal("heleket without a request method stays undefined")
	}
	if got := rechargeResolvePaymentMethod("heleket", nil, &method); got == nil || *got != "wxpay" {
		t.Fatal("heleket passes the requested method through")
	}
}

func TestRechargeRecordViewShape(t *testing.T) {
	created := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	completed := created.Add(time.Minute)
	details := json.RawMessage(`{"recharge":{"payableAmount":10.3},"heleket":{"invoiceCurrency":"CNY","payerCurrency":"USDT","network":"TRX","paymentMethodCode":"USDT@TRX","uuid":"u1","txid":"t1","status":"paid","statusDescription":"ok"}}`)
	provider := &db.RechargeProviderRef{ID: 7, Name: "支付宝", Type: models.PaymentProviderTypeYipay}
	view := buildRechargeRecordView(rechargeRecordViewInput{
		ID: 1, OrderNo: "R1", Amount: 10, Fee: float64Ptr(0.3), Status: "completed",
		Provider: provider, PaymentDetails: datatypes.JSON(details),
		CreatedAt: created, CompletedAt: &completed,
	})

	if view["payableAmount"] != 10.3 {
		t.Fatalf("payableAmount must come from paymentDetails: %v", view["payableAmount"])
	}
	if view["actualAmount"] != nil {
		t.Fatalf("missing actualAmount must be null: %v", view["actualAmount"])
	}
	if view["createdAt"] != "2026-11-10T12:00:00.000Z" || view["completedAt"] != "2026-11-10T12:01:00.000Z" {
		t.Fatalf("timestamps must be ISO with milliseconds: %v / %v", view["createdAt"], view["completedAt"])
	}
	if view["expiredAt"] != nil || view["tradeNo"] != nil || view["failReason"] != nil {
		t.Fatal("absent optional fields must be null")
	}
	if view["actualPaymentMethod"] != "USDT@TRX" || view["paymentNetwork"] != "TRX" || view["paymentUuid"] != "u1" {
		t.Fatalf("display info mismatch: %v", view)
	}
	providerView, ok := view["provider"].(gin.H)
	if !ok || providerView["id"] != int32(7) || providerView["name"] != "支付宝" {
		t.Fatalf("provider projection mismatch: %v", view["provider"])
	}
	if view["fee"] != 0.3 {
		t.Fatalf("fee mismatch: %v", view["fee"])
	}
}

func float64Ptr(value float64) *float64 { return &value }

// ==================== database-backed tests ====================

const (
	rechargeTestProviderID  int32 = 995001
	rechargeTestProviderKey       = "fake-e2e-key-not-a-real-secret"
)

func rechargeTestDatabase(t *testing.T) context.Context {
	t.Helper()
	if os.Getenv("PAYINCUS_GO_DB_TESTS") != "1" {
		t.Skip("set PAYINCUS_GO_DB_TESTS=1 and DATABASE_URL to run the database-backed recharge tests")
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

	ctx := context.Background()
	rechargeTestInit.Do(func() { rechargeTestInitErr = db.Init(ctx) })
	if rechargeTestInitErr != nil {
		t.Fatalf("db.Init: %v", rechargeTestInitErr)
	}
	return ctx
}

var (
	rechargeTestInit    sync.Once
	rechargeTestInitErr error
)

func rechargeTestUser(t *testing.T) int32 {
	t.Helper()
	var id int32
	if err := db.DB.Raw("SELECT id FROM users WHERE username = 'parityuser'").Scan(&id).Error; err != nil {
		t.Fatalf("load parityuser: %v", err)
	}
	if id == 0 {
		t.Skip("parityuser is missing from the test database")
	}
	return id
}

func rechargeTestSeedProvider(t *testing.T) {
	t.Helper()
	config := `{"version":"v1","apiurl":"https://epay.e2e.invalid","pid":"1001","key":"` + rechargeTestProviderKey + `"}`
	now := time.Now().UTC()
	err := db.DB.Exec(`
		INSERT INTO payment_providers (id, name, type, status, config, methods, fee_rate, fee_fixed,
		                               min_amount, max_amount, sort_order, created_at, updated_at)
		VALUES (?, 'Recharge E2E', 'yipay', 'active', ?::jsonb, '["alipay"]'::jsonb, 0, 0, 1, NULL, 0, ?, ?)
		ON CONFLICT (id) DO UPDATE SET status = 'active', config = EXCLUDED.config, updated_at = EXCLUDED.updated_at`,
		rechargeTestProviderID, config, now, now).Error
	if err != nil {
		t.Fatalf("seed provider: %v", err)
	}
}

func rechargeTestOrder(t *testing.T, ctx context.Context, userID int32, orderNo string, amount string) {
	t.Helper()
	now := time.Now().UTC()
	expiry := now.Add(30 * time.Minute)
	err := db.DB.Exec(`
		INSERT INTO recharge_records (user_id, provider_id, order_no, amount, actual_amount, fee,
		                              payment_method, status, payment_details, created_at, updated_at, expired_at)
		VALUES (?, ?, ?, ?::numeric, ?::numeric, 0, 'alipay', 'pending',
		        jsonb_build_object('recharge', jsonb_build_object('payableAmount', ?::numeric)), ?, ?, ?)`,
		userID, rechargeTestProviderID, orderNo, amount, amount, amount, now, now, expiry).Error
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
}

func rechargeTestCleanup(t *testing.T, userID int32, orderNos []string, initialBalance models.Decimal) {
	t.Helper()
	t.Cleanup(func() {
		for _, orderNo := range orderNos {
			db.DB.Exec("DELETE FROM payment_callbacks WHERE order_no = ?", orderNo)
			db.DB.Exec("DELETE FROM balance_logs WHERE order_id = ?", orderNo)
			db.DB.Exec("DELETE FROM inbox_messages WHERE user_id = ? AND content LIKE ?", userID, "%"+orderNo+"%")
			db.DB.Exec("DELETE FROM recharge_records WHERE order_no = ?", orderNo)
		}
		db.DB.Exec("DELETE FROM payment_providers WHERE id = ?", rechargeTestProviderID)
		db.DB.Exec("UPDATE users SET balance = ? WHERE id = ?", initialBalance, userID)
	})
}

func rechargeTestBalance(t *testing.T, userID int32) models.Decimal {
	t.Helper()
	var row struct{ Balance models.Decimal }
	if err := db.DB.Raw("SELECT balance FROM users WHERE id = ?", userID).Scan(&row).Error; err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return row.Balance
}

func rechargeTestSignV1(data map[string]any) string {
	keys := make([]string, 0, len(data))
	for key := range data {
		if key == "sign" || key == "sign_type" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := data[key]
		if value == nil {
			continue
		}
		if text, ok := value.(string); ok && text == "" {
			continue
		}
		parts = append(parts, key+"="+rechargeJSValueString(value))
	}
	sum := md5.Sum([]byte(strings.Join(parts, "&") + rechargeTestProviderKey))
	return hex.EncodeToString(sum[:])
}

func rechargeTestCallbackBody(orderNo, tradeNo, money string) map[string]any {
	body := map[string]any{
		"pid":          "1001",
		"trade_no":     tradeNo,
		"out_trade_no": orderNo,
		"type":         "alipay",
		"name":         "账户充值",
		"money":        money,
		"trade_status": "TRADE_SUCCESS",
		"sign_type":    "MD5",
	}
	body["sign"] = rechargeTestSignV1(body)
	return body
}

func rechargeTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRecharge(router)
	return router
}

func rechargeTestDo(t *testing.T, router *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestRechargeCallbackValidatesSignatureAndAmount(t *testing.T) {
	ctx := rechargeTestDatabase(t)
	userID := rechargeTestUser(t)
	rechargeTestSeedProvider(t)
	router := rechargeTestRouter()

	initialBalance := rechargeTestBalance(t, userID)
	orderNo := "RECHARGE-E2E-" + time.Now().UTC().Format("20060102150405.000000000")
	rechargeTestOrder(t, ctx, userID, orderNo, "10.00")
	rechargeTestCleanup(t, userID, []string{orderNo}, initialBalance)

	// 1. Wrong signature: the amount is otherwise valid, but the sign is forged.
	forged := rechargeTestCallbackBody(orderNo, "T-forged", "10.00")
	forged["sign"] = "00000000000000000000000000000000"
	response := rechargeTestDo(t, router, http.MethodPost, "/api/recharge/callback/"+strconv.Itoa(int(rechargeTestProviderID)), forged)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "签名验证失败") {
		t.Fatalf("forged signature must be rejected: %d %s", response.Code, response.Body.String())
	}

	// 2. Valid signature, mismatched amount (off by 2 cents > 1 cent tolerance).
	mismatched := rechargeTestCallbackBody(orderNo, "T-mismatch", "10.02")
	response = rechargeTestDo(t, router, http.MethodPost, "/api/recharge/callback/"+strconv.Itoa(int(rechargeTestProviderID)), mismatched)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "支付金额与订单金额不匹配") {
		t.Fatalf("amount mismatch must be rejected: %d %s", response.Code, response.Body.String())
	}

	var status string
	db.DB.Raw("SELECT status::text FROM recharge_records WHERE order_no = ?", orderNo).Scan(&status)
	if status != "pending" {
		t.Fatalf("a rejected callback must not change the order status, got %s", status)
	}
	if balance := rechargeTestBalance(t, userID); !balance.Decimal.Equal(initialBalance.Decimal) {
		t.Fatalf("a rejected callback must not credit: before=%s after=%s", initialBalance, balance)
	}

	// 3. Valid signature and amount -> one credit.
	valid := rechargeTestCallbackBody(orderNo, "T-valid", "10.00")
	response = rechargeTestDo(t, router, http.MethodPost, "/api/recharge/callback/"+strconv.Itoa(int(rechargeTestProviderID)), valid)
	if response.Code != http.StatusOK || response.Body.String() != "success" {
		t.Fatalf("valid V1 callback must answer plain 'success': %d %s", response.Code, response.Body.String())
	}
	expectedBalance := models.Decimal{Decimal: initialBalance.Decimal.Add(decimalFromStringForTest(t, "10.00").Decimal)}
	after := rechargeTestBalance(t, userID)
	if !after.Decimal.Equal(expectedBalance.Decimal) {
		t.Fatalf("balance must increase by exactly 10.00: before=%s after=%s", initialBalance, after)
	}
	db.DB.Raw("SELECT status::text FROM recharge_records WHERE order_no = ?", orderNo).Scan(&status)
	if status != "completed" {
		t.Fatalf("valid callback must complete the order, got %s", status)
	}
	var logCount int64
	db.DB.Raw("SELECT COUNT(*) FROM balance_logs WHERE order_id = ? AND type = 'recharge'", orderNo).Scan(&logCount)
	if logCount != 1 {
		t.Fatalf("exactly one balance log expected, got %d", logCount)
	}

	// 4. Replay of the same callback: 200 but no second credit.
	response = rechargeTestDo(t, router, http.MethodPost, "/api/recharge/callback/"+strconv.Itoa(int(rechargeTestProviderID)), valid)
	if response.Code != http.StatusOK || response.Body.String() != "success" {
		t.Fatalf("replay must answer success: %d %s", response.Code, response.Body.String())
	}
	if balance := rechargeTestBalance(t, userID); !balance.Decimal.Equal(after.Decimal) {
		t.Fatalf("replay must not credit twice: first=%s after-replay=%s", after, balance)
	}
	db.DB.Raw("SELECT COUNT(*) FROM balance_logs WHERE order_id = ?", orderNo).Scan(&logCount)
	if logCount != 1 {
		t.Fatalf("replay must not add a balance log, got %d", logCount)
	}
	var callbackCount int64
	db.DB.Raw("SELECT COUNT(*) FROM payment_callbacks WHERE order_no = ?", orderNo).Scan(&callbackCount)
	if callbackCount != 1 {
		t.Fatalf("exactly one payment_callbacks row expected, got %d", callbackCount)
	}
}

func TestRechargeCallbackCreditsOrderAmountNotCallbackAmount(t *testing.T) {
	ctx := rechargeTestDatabase(t)
	userID := rechargeTestUser(t)
	rechargeTestSeedProvider(t)
	router := rechargeTestRouter()

	initialBalance := rechargeTestBalance(t, userID)
	orderNo := "RECHARGE-E2E-TOL-" + time.Now().UTC().Format("20060102150405.000000000")
	rechargeTestOrder(t, ctx, userID, orderNo, "5.00")
	rechargeTestCleanup(t, userID, []string{orderNo}, initialBalance)

	// 5.01 is within the one-cent tolerance but the credit must stay at the order
	// amount (5.00): the callback body is never used as the credited amount.
	body := rechargeTestCallbackBody(orderNo, "T-tolerance", "5.01")
	response := rechargeTestDo(t, router, http.MethodPost, "/api/recharge/callback/"+strconv.Itoa(int(rechargeTestProviderID)), body)
	if response.Code != http.StatusOK {
		t.Fatalf("tolerated amount must pass: %d %s", response.Code, response.Body.String())
	}
	after := rechargeTestBalance(t, userID)
	expected := models.Decimal{Decimal: initialBalance.Decimal.Add(decimalFromStringForTest(t, "5.00").Decimal)}
	if !after.Decimal.Equal(expected.Decimal) {
		t.Fatalf("credit must be the order actual_amount (5.00), got before=%s after=%s", initialBalance, after)
	}
}

func TestRechargeCallbackConcurrentReplayCreditsOnce(t *testing.T) {
	ctx := rechargeTestDatabase(t)
	userID := rechargeTestUser(t)
	rechargeTestSeedProvider(t)
	router := rechargeTestRouter()

	initialBalance := rechargeTestBalance(t, userID)
	orderNo := "RECHARGE-E2E-CONC-" + time.Now().UTC().Format("20060102150405.000000000")
	rechargeTestOrder(t, ctx, userID, orderNo, "7.00")
	rechargeTestCleanup(t, userID, []string{orderNo}, initialBalance)

	body := rechargeTestCallbackBody(orderNo, "T-concurrent", "7.00")
	encodedBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for index := 0; index < 2; index++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			request := httptest.NewRequest(http.MethodPost,
				"/api/recharge/callback/"+strconv.Itoa(int(rechargeTestProviderID)), bytes.NewReader(encodedBody))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			codes[slot] = recorder.Code
		}(index)
	}
	wg.Wait()
	for slot, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("concurrent callback %d must answer 200, got %d", slot, code)
		}
	}

	after := rechargeTestBalance(t, userID)
	expected := models.Decimal{Decimal: initialBalance.Decimal.Add(decimalFromStringForTest(t, "7.00").Decimal)}
	if !after.Decimal.Equal(expected.Decimal) {
		t.Fatalf("concurrent replay must credit once: before=%s after=%s", initialBalance, after)
	}
	var logCount int64
	db.DB.Raw("SELECT COUNT(*) FROM balance_logs WHERE order_id = ?", orderNo).Scan(&logCount)
	if logCount != 1 {
		t.Fatalf("concurrent replay must produce one balance log, got %d", logCount)
	}
	var status string
	db.DB.Raw("SELECT status::text FROM recharge_records WHERE order_no = ?", orderNo).Scan(&status)
	if status != "completed" {
		t.Fatalf("order must be completed, got %s", status)
	}
}

func TestRechargeCreateOrderManualProvider(t *testing.T) {
	rechargeTestDatabase(t)
	userID := rechargeTestUser(t)

	now := time.Now().UTC()
	err := db.DB.Exec(`
		INSERT INTO payment_providers (id, name, type, status, config, methods, fee_rate, fee_fixed,
		                               min_amount, max_amount, sort_order, created_at, updated_at)
		VALUES (995002, 'Recharge Manual E2E', 'manual', 'active',
		        '{"instructions":"转账后联系客服"}'::jsonb, '[]'::jsonb, 0, 0, 1, NULL, 0, ?, ?)
		ON CONFLICT (id) DO UPDATE SET status = 'active', config = EXCLUDED.config, updated_at = EXCLUDED.updated_at`,
		now, now).Error
	if err != nil {
		t.Fatalf("seed manual provider: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/recharge/orders", func(c *gin.Context) {
		c.Set(middleware.ContextUserKey, &lib.AccessTokenClaims{
			ID: userID, Username: "parityuser", Role: "user", Status: "active",
		})
		rechargeCreateOrder(c)
	})

	var createdOrderNo string
	t.Cleanup(func() {
		if createdOrderNo != "" {
			db.DB.Exec("DELETE FROM recharge_records WHERE order_no = ?", createdOrderNo)
		}
		db.DB.Exec("DELETE FROM payment_providers WHERE id = 995002")
	})

	// A JSON string amount is rejected (the TS requires a number).
	response := rechargeTestDo(t, router, http.MethodPost, "/api/recharge/orders", map[string]any{
		"providerId": 995002, "amount": "10.00",
	})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "充值金额无效") {
		t.Fatalf("string amount must be rejected: %d %s", response.Code, response.Body.String())
	}

	response = rechargeTestDo(t, router, http.MethodPost, "/api/recharge/orders", map[string]any{
		"providerId": 995002, "amount": 10,
	})
	if response.Code != http.StatusOK {
		t.Fatalf("manual order must be created: %d %s", response.Code, response.Body.String())
	}
	var payload struct {
		Order struct {
			OrderNo       string  `json:"orderNo"`
			Amount        float64 `json:"amount"`
			PayableAmount float64 `json:"payableAmount"`
			ActualAmount  float64 `json:"actualAmount"`
			Fee           float64 `json:"fee"`
			Status        string  `json:"status"`
			CreatedAt     string  `json:"createdAt"`
			ExpiredAt     string  `json:"expiredAt"`
		} `json:"order"`
		PayURL        *string `json:"payUrl"`
		ManualPayment *struct {
			Instructions string `json:"instructions"`
		} `json:"manualPayment"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if payload.Order.OrderNo == "" || payload.Order.Amount != 10 || payload.Order.PayableAmount != 10 ||
		payload.Order.ActualAmount != 10 || payload.Order.Fee != 0 || payload.Order.Status != "pending" {
		t.Fatalf("unexpected order payload: %+v", payload.Order)
	}
	if payload.PayURL != nil {
		t.Fatalf("manual providers must not return a pay URL: %v", *payload.PayURL)
	}
	if payload.ManualPayment == nil || payload.ManualPayment.Instructions != "转账后联系客服" {
		t.Fatalf("manual payment instructions missing: %+v", payload.ManualPayment)
	}
	if payload.Order.CreatedAt == "" || payload.Order.ExpiredAt == "" {
		t.Fatalf("timestamps missing: %+v", payload.Order)
	}
	createdOrderNo = payload.Order.OrderNo

	var record struct {
		Status                 string
		PaymentDetails         datatypes.JSON
		ProviderConfigSnapshot *string
	}
	if err := db.DB.Raw("SELECT status::text AS status, payment_details, provider_config_snapshot FROM recharge_records WHERE order_no = ?",
		createdOrderNo).Scan(&record).Error; err != nil {
		t.Fatalf("read created order: %v", err)
	}
	if record.Status != "pending" {
		t.Fatalf("order status must be pending, got %s", record.Status)
	}
	if record.ProviderConfigSnapshot == nil || *record.ProviderConfigSnapshot == "" {
		t.Fatal("provider config snapshot must be captured (encrypted)")
	}
	snapshot := lib.ParseRechargeProviderConfigSnapshot(*record.ProviderConfigSnapshot)
	if snapshot == nil || snapshot.Type != "manual" || snapshot.Config["instructions"] != "转账后联系客服" {
		t.Fatalf("provider config snapshot must decrypt to the provider config: %+v", snapshot)
	}
	details := lib.ReadRechargePaymentDetails(record.PaymentDetails)
	if details.Manual == nil || details.Manual.Instructions == nil || *details.Manual.Instructions != "转账后联系客服" {
		t.Fatalf("payment details missing manual instructions: %+v", details)
	}
	if details.Recharge == nil || details.Recharge.PayableAmount == nil || details.Recharge.PayableAmount.Value != 10 {
		t.Fatalf("payment details missing recharge amount: %+v", details.Recharge)
	}
}

func TestRechargeProviderConfigEncryptionRoundTrip(t *testing.T) {
	ctx := rechargeTestDatabase(t)
	secretKey := "parity-fake-secret-" + strconv.FormatInt(time.Now().UnixNano(), 10)

	provider, err := db.CreatePaymentProvider(ctx, db.CreatePaymentProviderInput{
		Name: "Recharge Crypto E2E", Type: models.PaymentProviderTypeYipay,
		Config: map[string]any{
			"version": "v1", "apiurl": "https://epay.e2e.invalid", "pid": "PID-1", "key": secretKey,
		},
		ConfigProvided:  true,
		Methods:         []string{"alipay"},
		MethodsProvided: true,
		Financial:       db.NewPaymentProviderFinancialInput(map[string]any{"feeRate": 0.01, "feeFixed": 0.2}),
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	t.Cleanup(func() { db.DB.Exec("DELETE FROM payment_providers WHERE id = ?", provider.ID) })

	if provider.Config["key"] != secretKey || provider.FeeRate != 0.01 || provider.FeeFixed != 0.2 {
		t.Fatalf("decrypted record mismatch: %+v", provider)
	}
	var rawConfig string
	if err := db.DB.Raw("SELECT config::text FROM payment_providers WHERE id = ?", provider.ID).Scan(&rawConfig).Error; err != nil {
		t.Fatalf("read raw config: %v", err)
	}
	if !strings.HasPrefix(rawConfig, `"`) {
		t.Fatalf("config must be stored as a JSON string scalar, got %s", rawConfig)
	}
	if strings.Contains(rawConfig, secretKey) {
		t.Fatal("the secret must never be stored in clear text")
	}

	// A masked/empty secret in an update keeps the stored value. The merge is the
	// route's job (mergePaymentProviderConfigForUpdate); db.updatePaymentProvider
	// only encrypts what it is given.
	mergedConfig := db.MergePaymentProviderConfigForUpdate(map[string]any{
		"version": "v1", "apiurl": "https://epay.e2e.invalid", "pid": "PID-2", "key": db.MaskedPaymentSecretValue,
	}, provider.Config)
	updated, err := db.UpdatePaymentProvider(ctx, provider.ID, db.UpdatePaymentProviderInput{
		Config:         mergedConfig,
		ConfigProvided: true,
		Financial:      db.NewPaymentProviderFinancialInput(map[string]any{}),
	})
	if err != nil {
		t.Fatalf("update provider: %v", err)
	}
	if updated.Config["key"] != secretKey || updated.Config["pid"] != "PID-2" {
		t.Fatalf("masked secret must be preserved: %+v", updated.Config)
	}

	// Snapshot round trip uses the same AES-GCM codec.
	snapshot := lib.BuildRechargeProviderConfigSnapshot("yipay", map[string]any{"pid": "PID-2", "key": secretKey})
	parsed := lib.ParseRechargeProviderConfigSnapshot(snapshot)
	if parsed == nil || parsed.Type != "yipay" || parsed.Config["key"] != secretKey {
		t.Fatalf("snapshot round trip failed: %+v", parsed)
	}
	config, source := lib.ResolveRechargeProviderConfigSnapshot("heleket", map[string]any{"pid": "current"}, snapshot)
	if source != "provider" || config["pid"] != "current" {
		t.Fatalf("a snapshot of another provider type must not be used: %v %v", source, config)
	}
}

// ==================== test helpers ====================

func decimalFromStringForTest(t *testing.T, value string) models.Decimal {
	t.Helper()
	parsed, err := models.NewDecimalFromString(value)
	if err != nil {
		t.Fatalf("parse decimal %s: %v", value, err)
	}
	return parsed
}
