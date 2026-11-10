// Tests for the recharge/payment primitives. The reference values marked
// "node:" were produced with the real TypeScript implementation
// (crypto.createHash/createSign, JSON.stringify) so they pin byte-level parity
// rather than merely self-consistency.
package lib

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ==================== helpers ====================

func testRSAKeyPair(t *testing.T) (*rsa.PrivateKey, string, string) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal private: %v", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatalf("marshal public: %v", err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}))
	publicPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	return privateKey, privatePEM, publicPEM
}

func testSignContent(t *testing.T, privateKey *rsa.PrivateKey, content string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(content))
	signature, err := rsa.SignPKCS1v15(nil, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return base64.StdEncoding.EncodeToString(signature)
}

// ==================== epay ====================

func TestEpayV1PayLinkMatchesNode(t *testing.T) {
	client := CreateEpayClient(EpayConfig{Version: EpayVersionV1, APIURL: "https://epay.example.com", PID: "1001", Key: "testkey"})
	link := client.GetPayLink(EpayPayParams{
		Type:       "alipay",
		OutTradeNo: "RABC123",
		Name:       "账户充值",
		Money:      "10.00",
		NotifyURL:  "https://cb.example/api/recharge/callback/1",
		ReturnURL:  "https://fe.example/wallet",
	})
	const want = "https://epay.example.com/submit.php?type=alipay&out_trade_no=RABC123&name=%E8%B4%A6%E6%88%B7%E5%85%85%E5%80%BC&money=10.00&notify_url=https%3A%2F%2Fcb.example%2Fapi%2Frecharge%2Fcallback%2F1&return_url=https%3A%2F%2Ffe.example%2Fwallet&pid=1001&sign=bad66d264c7190d2d5e670a7de35b598&sign_type=MD5"
	if link != want {
		t.Fatalf("pay link mismatch\n got: %s\nwant: %s", link, want)
	}
}

func TestEpayV1Verify(t *testing.T) {
	client := CreateEpayClient(EpayConfig{Version: EpayVersionV1, APIURL: "https://epay.example.com", PID: "1001", Key: "testkey"})

	// node: MD5(content + key) where content is the sorted non-empty params.
	const signature = "bad66d264c7190d2d5e670a7de35b598"
	valid := map[string]any{
		"type":         "alipay",
		"out_trade_no": "RABC123",
		"name":         "账户充值",
		"money":        "10.00",
		"notify_url":   "https://cb.example/api/recharge/callback/1",
		"return_url":   "https://fe.example/wallet",
		"pid":          "1001",
		"sign":         signature,
		"sign_type":    "MD5",
	}
	if !client.Verify(valid) {
		t.Fatal("expected a valid V1 signature")
	}
	result := client.VerifyWithStatus(valid)
	if !result.Valid || result.TradeSuccess {
		t.Fatalf("missing trade_status must be valid-but-not-successful: %+v", result)
	}
	if result.Error != "交易未成功: 未知状态" {
		t.Fatalf("unexpected error: %q", result.Error)
	}

	// node: the signed string now includes trade_status, so the signature changes.
	valid["trade_status"] = "TRADE_SUCCESS"
	valid["sign"] = "160708cc06ab770d237477fe5c6fc985"
	result = client.VerifyWithStatus(valid)
	if !result.Valid || !result.TradeSuccess {
		t.Fatalf("expected TRADE_SUCCESS to pass: %+v", result)
	}

	tampered := map[string]any{}
	for key, value := range valid {
		tampered[key] = value
	}
	tampered["money"] = "1000.00"
	if client.Verify(tampered) {
		t.Fatal("tampered amount must fail verification")
	}
	result = client.VerifyWithStatus(tampered)
	if result.Valid || result.Error != "签名验证失败" {
		t.Fatalf("expected 签名验证失败, got %+v", result)
	}
}

func TestEpayV1VerifyMissingSign(t *testing.T) {
	client := CreateEpayClient(EpayConfig{Version: EpayVersionV1, APIURL: "https://e", PID: "1", Key: "k"})
	if client.Verify(map[string]any{}) {
		t.Fatal("empty payload must not verify")
	}
	result := client.VerifyWithStatus(map[string]any{"out_trade_no": "x"})
	if result.Valid || result.TradeSuccess || result.Error != "缺少签名参数" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestEpayV2Verify(t *testing.T) {
	privateKey, privatePEM, publicPEM := testRSAKeyPair(t)
	client := CreateEpayClient(EpayConfig{
		Version:            EpayVersionV2,
		APIURL:             "https://epay.example.com",
		PID:                "1001",
		PlatformPublicKey:  publicPEM,
		MerchantPrivateKey: privatePEM,
	})

	timestamp := time.Now().Unix()
	payload := map[string]any{
		"out_trade_no": "RABC123",
		"money":        "10.00",
		"trade_status": "TRADE_SUCCESS",
		"timestamp":    strconv.FormatInt(timestamp, 10),
		"sign_type":    "RSA",
	}
	content := epaySignContent(payload, true)
	payload["sign"] = testSignContent(t, privateKey, content)

	result := client.VerifyWithStatus(payload)
	if !result.Valid || !result.TradeSuccess {
		t.Fatalf("expected valid RSA callback: %+v", result)
	}
	if !client.Verify(payload) {
		t.Fatal("Verify() must accept the same payload")
	}

	// Timestamp outside the 300 second window is rejected before the signature.
	stale := map[string]any{}
	for key, value := range payload {
		stale[key] = value
	}
	stale["timestamp"] = strconv.FormatInt(timestamp-400, 10)
	result = client.VerifyWithStatus(stale)
	if result.Valid || result.Error != "时间戳已过期" {
		t.Fatalf("expected 时间戳已过期, got %+v", result)
	}
	if client.Verify(stale) {
		t.Fatal("Verify() must reject a stale timestamp")
	}

	// Tampered amount breaks the signature.
	tampered := map[string]any{}
	for key, value := range payload {
		tampered[key] = value
	}
	tampered["money"] = "0.01"
	result = client.VerifyWithStatus(tampered)
	if result.Valid || result.Error != "RSA签名验证失败" {
		t.Fatalf("expected RSA签名验证失败, got %+v", result)
	}

	// A garbage signature is a failed verification, never a panic.
	broken := map[string]any{}
	for key, value := range payload {
		broken[key] = value
	}
	broken["sign"] = "!!!not-base64!!!"
	if client.Verify(broken) {
		t.Fatal("invalid base64 signature must fail")
	}
}

func TestEpayV2BareBase64KeysAndPayLink(t *testing.T) {
	privateKey, privatePEM, publicPEM := testRSAKeyPair(t)

	// Strip the PEM armour: the TS wraps bare base64 bodies itself.
	barePrivate := strings.Join(strings.Split(
		strings.TrimSpace(strings.NewReplacer("-----BEGIN PRIVATE KEY-----", "", "-----END PRIVATE KEY-----", "").Replace(privatePEM)),
		"\n"), "")
	barePublic := strings.Join(strings.Split(
		strings.TrimSpace(strings.NewReplacer("-----BEGIN PUBLIC KEY-----", "", "-----END PUBLIC KEY-----", "").Replace(publicPEM)),
		"\n"), "")

	client := CreateEpayClient(EpayConfig{
		Version:            EpayVersionV2,
		APIURL:             "https://epay.example.com",
		PID:                "1001",
		PlatformPublicKey:  barePublic,
		MerchantPrivateKey: barePrivate,
	})
	link := client.GetPayLink(EpayPayParams{
		Type:       "alipay",
		OutTradeNo: "RABC123",
		Name:       "账户充值",
		Money:      "10.00",
		NotifyURL:  "https://cb.example/api/recharge/callback/1",
		ReturnURL:  "https://fe.example/wallet",
	})
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse link: %v", err)
	}
	values := map[string]any{}
	for key, items := range parsed.Query() {
		if len(items) > 0 {
			values[key] = items[0]
		}
	}
	if _, ok := values["sign"].(string); !ok {
		t.Fatalf("pay link has no signature: %s", link)
	}
	if !client.Verify(values) {
		t.Fatalf("the generated V2 link must verify with the platform public key: %s", link)
	}
	_ = privateKey
}

func TestEpayV2QueryOrderUnsupported(t *testing.T) {
	client := CreateEpayClient(EpayConfig{Version: EpayVersionV2, APIURL: "https://e", PID: "1"})
	result := client.QueryOrder(context.Background(), "R1")
	if result.Success || result.Paid {
		t.Fatalf("V2 query must stay unsupported: %+v", result)
	}
	if result.Error != "V2版本暂不支持主动查询，请等待异步回调" {
		t.Fatalf("unexpected error: %q", result.Error)
	}
}

func TestEpayV1QueryOrderParsing(t *testing.T) {
	// The network layer refuses loopback targets on purpose (SSRF guard), so the
	// wire parsing is exercised directly instead of through httptest.
	paid := parseEpayOrderQueryResponse(200, []byte(`{"code":1,"status":1,"trade_no":"T9","out_trade_no":"R1","money":"10.00","type":"alipay"}`))
	if !paid.Success || !paid.Paid || paid.TradeNo != "T9" || paid.OutTradeNo != "R1" || paid.Money != "10.00" {
		t.Fatalf("unexpected paid result: %+v", paid)
	}
	unpaid := parseEpayOrderQueryResponse(200, []byte(`{"code":1,"status":0}`))
	if !unpaid.Success || unpaid.Paid {
		t.Fatalf("unexpected unpaid result: %+v", unpaid)
	}
	refused := parseEpayOrderQueryResponse(200, []byte(`{"code":0,"msg":"订单不存在","status":0}`))
	if refused.Success || refused.Error != "订单不存在" || refused.RawData == nil {
		t.Fatalf("unexpected refused result: %+v", refused)
	}
	defaultMessage := parseEpayOrderQueryResponse(200, []byte(`{"code":0}`))
	if defaultMessage.Error != "查询失败" {
		t.Fatalf("unexpected default message: %q", defaultMessage.Error)
	}
	badJSON := parseEpayOrderQueryResponse(200, []byte(`not json`))
	if badJSON.Success {
		t.Fatal("invalid JSON must not report success")
	}
	httpError := parseEpayOrderQueryResponse(502, nil)
	if httpError.Success || httpError.Error != "HTTP错误: 502" {
		t.Fatalf("unexpected http error result: %+v", httpError)
	}
}

func TestEpayV1QueryOrderRejectsPrivateTargets(t *testing.T) {
	client := CreateEpayClient(EpayConfig{Version: EpayVersionV1, APIURL: "http://127.0.0.1:9", PID: "1", Key: "k"})
	result := client.QueryOrder(context.Background(), "R1")
	if result.Success || result.Error == "" {
		t.Fatalf("a private target must fail closed: %+v", result)
	}
}

// ==================== heleket ====================

func TestHeleketSerializationMatchesNode(t *testing.T) {
	// node: JSON.stringify({amount,currency,order_id,url_callback}).replace(/\//g,'\\/')
	raw := []byte(`{"amount":"10.00","currency":"CNY","order_id":"R123","url_callback":"https://x/y","sign":"deadbeef"}`)
	parsed, err := ParseHeleketOrderedJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	object, ok := parsed.(*HeleketOrderedObject)
	if !ok {
		t.Fatalf("expected an ordered object, got %T", parsed)
	}
	object.Delete("sign")
	serialized, err := SerializeHeleketPayload(object)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	const want = `{"amount":"10.00","currency":"CNY","order_id":"R123","url_callback":"https:\/\/x\/y"}`
	if serialized != want {
		t.Fatalf("serialization mismatch\n got: %s\nwant: %s", serialized, want)
	}
	// node: MD5(base64(serialized) + apiKey)
	signature, err := SignHeleketPayload(object, "apikey")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if signature != "851a405b69daeebd9b4791bcfe9aab7f" {
		t.Fatalf("signature mismatch: %s", signature)
	}
}

func TestHeleketWebhookSignature(t *testing.T) {
	raw := []byte(`{"amount":"10.00","currency":"CNY","order_id":"R123","url_callback":"https://x/y","sign":"851a405b69daeebd9b4791bcfe9aab7f"}`)
	parsed, err := ParseHeleketOrderedJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !VerifyHeleketWebhookSignature(parsed, "apikey") {
		t.Fatal("expected the node reference signature to verify")
	}
	if VerifyHeleketWebhookSignature(parsed, "wrong-key") {
		t.Fatal("a different api key must fail")
	}

	// Any byte change must fail, including a reordered body (the signature binds order).
	reordered, err := ParseHeleketOrderedJSON([]byte(`{"currency":"CNY","amount":"10.00","order_id":"R123","url_callback":"https://x/y","sign":"851a405b69daeebd9b4791bcfe9aab7f"}`))
	if err != nil {
		t.Fatalf("parse reordered: %v", err)
	}
	if VerifyHeleketWebhookSignature(reordered, "apikey") {
		t.Fatal("a reordered payload must not verify: key order is signed")
	}

	if VerifyHeleketWebhookSignature(map[string]any{"sign": "x"}, "apikey") {
		t.Fatal("map payloads cannot reproduce the signed order and must fail closed")
	}
	if VerifyHeleketWebhookSignature(parsed, "") {
		t.Fatal("an empty api key must fail")
	}
}

func TestHeleketWebhookSignatureTampered(t *testing.T) {
	raw := []byte(`{"amount":"10.00","currency":"CNY","order_id":"R123","sign":"851a405b69daeebd9b4791bcfe9aab7f"}`)
	parsed, err := ParseHeleketOrderedJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	object := parsed.(*HeleketOrderedObject)
	object.Set("amount", "1000.00")
	if VerifyHeleketWebhookSignature(object, "apikey") {
		t.Fatal("tampered amount must fail verification")
	}
}

func TestHeleketStatusHelpers(t *testing.T) {
	if !IsHeleketPaidStatus("paid") || !IsHeleketPaidStatus("paid_over") || IsHeleketPaidStatus("process") {
		t.Fatal("paid status set mismatch")
	}
	if GetHeleketPaymentState(map[string]any{"status": "paid"}) != HeleketPaymentStatePaid {
		t.Fatal("paid state mismatch")
	}
	if GetHeleketPaymentState(map[string]any{"status": "cancel"}) != HeleketPaymentStateCancelled {
		t.Fatal("cancelled state mismatch")
	}
	if GetHeleketPaymentState(map[string]any{"status": "system_fail"}) != HeleketPaymentStateFailed {
		t.Fatal("failed state mismatch")
	}
	if GetHeleketPaymentState(map[string]any{"status": "process"}) != HeleketPaymentStatePending {
		t.Fatal("pending state mismatch")
	}
	if GetHeleketPaymentState(map[string]any{"status": "unknown", "is_final": true}) != HeleketPaymentStateFailed {
		t.Fatal("is_final unknown must be failed")
	}
	if GetHeleketPaymentState(map[string]any{"status": "unknown"}) != HeleketPaymentStatePending {
		t.Fatal("unknown non-final must be pending")
	}
	if GetHeleketPaymentState(map[string]any{"payment_status": "paid"}) != HeleketPaymentStatePaid {
		t.Fatal("payment_status fallback mismatch")
	}
	if GetHeleketStatusDescription("") != "未知支付状态" || GetHeleketStatusDescription("weird") != "weird" {
		t.Fatal("status description fallback mismatch")
	}
}

func TestHeleketInvoiceAmountParsing(t *testing.T) {
	cases := []struct {
		value    any
		expected *float64
	}{
		{"10.00", ptrFloat(10)},
		{" 10.5abc", ptrFloat(10.5)},
		{"0", nil},
		{"-1", nil},
		{"abc", nil},
		{12.5, nil}, // TS only accepts the string form
		{nil, nil},
	}
	for _, testCase := range cases {
		got := GetHeleketInvoiceAmount(map[string]any{"amount": testCase.value})
		if testCase.expected == nil {
			if got != nil {
				t.Fatalf("amount %v: expected nil, got %v", testCase.value, *got)
			}
			continue
		}
		if got == nil || *got != *testCase.expected {
			t.Fatalf("amount %v: expected %v, got %v", testCase.value, *testCase.expected, got)
		}
	}
}

func TestHeleketBuildConfig(t *testing.T) {
	config, valid, message := BuildHeleketConfig(map[string]any{
		"apiurl":        "https://api.heleket.example",
		"merchant_uuid": " uuid ",
		"api_key":       " key ",
		"currency":      "cny",
		"lifetime":      "90",
	})
	if !valid || message != "" {
		t.Fatalf("expected valid config, got %q", message)
	}
	if config.Currency != "CNY" || config.MerchantUUID != "uuid" || config.APIKey != "key" {
		t.Fatalf("unexpected normalisation: %+v", config)
	}
	if config.LifetimeSeconds != 300 {
		t.Fatalf("lifetime must clamp to 300, got %d", config.LifetimeSeconds)
	}

	_, valid, message = BuildHeleketConfig(map[string]any{"apiurl": "https://x"})
	if valid || !strings.Contains(message, "配置不完整") {
		t.Fatalf("missing merchant must be invalid: %v %q", valid, message)
	}

	defaults, valid, _ := BuildHeleketConfig(map[string]any{"merchant_uuid": "u", "api_key": "k"})
	if !valid || defaults.APIURL != "https://api.heleket.com" || defaults.Currency != "CNY" || defaults.LifetimeSeconds != 3600 {
		t.Fatalf("defaults mismatch: %+v", defaults)
	}

	for _, lifetime := range []any{100000, "120000"} {
		clamped, _, _ := BuildHeleketConfig(map[string]any{"merchant_uuid": "u", "api_key": "k", "lifetime": lifetime})
		if clamped.LifetimeSeconds != 43200 {
			t.Fatalf("lifetime %v must clamp to 43200, got %d", lifetime, clamped.LifetimeSeconds)
		}
	}
}

// ==================== antom ====================

func TestAntomSignAndVerifyContent(t *testing.T) {
	_, privatePEM, publicPEM := testRSAKeyPair(t)
	content := AntomCreateContentToSign("POST", "/ams/api/v1/payments/inquiryPayment", "client_1", "1700000000000", `{"paymentRequestId":"R1"}`)
	signature, err := SignAntomContent(content, privatePEM)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	header := "algorithm=RSA256, keyVersion=1, signature=" + url.QueryEscape(signature)
	if !VerifyAntomContent(content, header, publicPEM) {
		t.Fatal("expected the signature to verify")
	}
	if VerifyAntomContent(content+"x", header, publicPEM) {
		t.Fatal("tampered content must fail")
	}
	if VerifyAntomContent(content, "algorithm=RSA256", publicPEM) {
		t.Fatal("a header without signature= must fail")
	}
	if VerifyAntomContent(content, header, "not-a-key") {
		t.Fatal("a malformed public key must fail closed")
	}
}

func TestAntomWebhookVerification(t *testing.T) {
	_, privatePEM, publicPEM := testRSAKeyPair(t)
	config := AntomConfig{ClientID: "client_1", AntomPublicKey: publicPEM, MerchantPrivateKey: privatePEM, Currency: "USD", CurrencyRate: 1, CurrencyExponent: 2}
	rawBody := `{"notifyType":"PAYMENT_RESULT","paymentRequestId":"R1"}`
	content := AntomCreateContentToSign("POST", "/api/recharge/callback/7", "client_1", "1700000000000", rawBody)
	signature, err := SignAntomContent(content, privatePEM)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	header := "algorithm=RSA256, keyVersion=1, signature=" + url.QueryEscape(signature)

	if !VerifyAntomWebhook(config, "POST", "/api/recharge/callback/7", "1700000000000", "client_1", header, rawBody) {
		t.Fatal("expected a valid webhook")
	}
	if VerifyAntomWebhook(config, "POST", "/api/recharge/callback/7", "1700000000000", "other", header, rawBody) {
		t.Fatal("client id mismatch must fail")
	}
	if VerifyAntomWebhook(config, "POST", "/api/recharge/callback/7", "1700000000000", "client_1", header, rawBody+" ") {
		t.Fatal("raw body change must fail")
	}
	if VerifyAntomWebhook(config, "POST", "/api/recharge/callback/7", "", "client_1", header, rawBody) {
		t.Fatal("missing request time must fail")
	}
}

func TestAntomAmountConversion(t *testing.T) {
	config := AntomConfig{Currency: "USD", CurrencyRate: 7.2, CurrencyExponent: 2}
	minor, err := CalculateAntomMinorAmount(10, config)
	if err != nil || minor != "7200" {
		t.Fatalf("minor amount mismatch: %s %v", minor, err)
	}
	// JS Math.round(0.015 * 10) === 0 -> invalid (must be > 0)
	if _, err := CalculateAntomMinorAmount(0.00001, config); err == nil {
		t.Fatal("a zero minor amount must be rejected")
	}

	zeroDecimal := AntomConfig{Currency: "JPY", CurrencyRate: 1, CurrencyExponent: 0}
	if value, err := CalculateAntomMinorAmount(10.4, zeroDecimal); err != nil || value != "10" {
		t.Fatalf("exponent 0 mismatch: %s %v", value, err)
	}

	threeDecimal := AntomConfig{Currency: "KWD", CurrencyRate: 1, CurrencyExponent: 3}
	if value, err := CalculateAntomMinorAmount(1.2345, threeDecimal); err != nil || value != "1235" {
		t.Fatalf("exponent 3 rounding mismatch: %s %v", value, err)
	}

	if got := AntomMinorAmountToCNY("7200", config); got == nil || *got != 10 {
		t.Fatalf("minor->cny mismatch: %v", got)
	}
	if got := AntomMinorAmountToCNY("7201", config); got == nil || *got != 10 {
		t.Fatalf("rounding to 2dp expected: %v", got)
	}
	for _, invalid := range []any{"0", "-1", "abc", "1.5", 100, nil} {
		if got := AntomMinorAmountToCNY(invalid, config); got != nil {
			t.Fatalf("invalid minor %v must yield nil, got %v", invalid, *got)
		}
	}
}

func TestAntomBuildConfig(t *testing.T) {
	_, privatePEM, publicPEM := testRSAKeyPair(t)
	config, valid, message := BuildAntomConfig(map[string]any{
		"environment":          "production",
		"apiurl":               "https://open-sea-global.alipay.com",
		"clientId":             "client_1",
		"antom_public_key":     publicPEM,
		"merchant_private_key": privatePEM,
		"currency":             "usd",
		"currencyRate":         "7.2",
		"currencyExponent":     "2",
		"keyVersion":           "2",
		"sessionExpiryMinutes": "15",
		"orderDescription":     "充值",
	})
	if !valid {
		t.Fatalf("expected valid antom config: %s", message)
	}
	if config.Environment != AntomEnvironmentProduction || config.Currency != "USD" ||
		config.CurrencyRate != 7.2 || config.CurrencyExponent != 2 || config.KeyVersion != 2 ||
		config.SessionExpiryMinutes != 15 || config.OrderDescription != "充值" {
		t.Fatalf("unexpected config: %+v", config)
	}

	invalidCases := []map[string]any{
		{"clientId": "x", "antom_public_key": publicPEM, "merchant_private_key": privatePEM},
		{"clientId": "client_1", "antom_public_key": publicPEM, "merchant_private_key": privatePEM, "apiurl": "https://evil.example"},
		{"clientId": "client_1", "antom_public_key": publicPEM, "merchant_private_key": privatePEM, "apiurl": "http://open-sea-global.alipay.com"},
		{"clientId": "client_1", "antom_public_key": publicPEM, "merchant_private_key": privatePEM, "currency": "US"},
		{"clientId": "client_1", "antom_public_key": publicPEM, "merchant_private_key": privatePEM, "currencyRate": 0},
		{"clientId": "client_1", "antom_public_key": publicPEM, "merchant_private_key": privatePEM, "currencyExponent": 4},
		{"clientId": "client_1", "antom_public_key": publicPEM, "merchant_private_key": privatePEM, "keyVersion": 1000},
		{"clientId": "client_1", "antom_public_key": publicPEM, "merchant_private_key": privatePEM, "sessionExpiryMinutes": 4},
		{"clientId": "client_1", "antom_public_key": "", "merchant_private_key": privatePEM},
		{"clientId": "client_1", "antom_public_key": publicPEM, "merchant_private_key": "not-pem"},
	}
	for index, input := range invalidCases {
		if _, valid, _ := BuildAntomConfig(input); valid {
			t.Fatalf("case %d must be invalid", index)
		}
	}

	// A bare base64 key body is wrapped into a PEM document by the builder.
	barePublic := strings.Join(strings.Split(strings.TrimSpace(strings.NewReplacer(
		"-----BEGIN PUBLIC KEY-----", "", "-----END PUBLIC KEY-----", "").Replace(publicPEM)), "\n"), "")
	barePrivate := strings.Join(strings.Split(strings.TrimSpace(strings.NewReplacer(
		"-----BEGIN PRIVATE KEY-----", "", "-----END PRIVATE KEY-----", "").Replace(privatePEM)), "\n"), "")
	wrapped, valid, message := BuildAntomConfig(map[string]any{
		"clientId": "client_1", "antom_public_key": barePublic, "merchant_private_key": barePrivate,
	})
	if !valid {
		t.Fatalf("bare base64 keys must be wrapped: %s", message)
	}
	if !strings.HasPrefix(wrapped.AntomPublicKey, "-----BEGIN PUBLIC KEY-----") {
		t.Fatal("public key was not wrapped")
	}
}

// ==================== recharge payment details ====================

func TestRechargePaymentDetailsRoundTrip(t *testing.T) {
	if details := ReadRechargePaymentDetails(nil); details.Kind != nil {
		t.Fatal("nil must read as empty details")
	}
	if details := ReadRechargePaymentDetails("not-an-object"); details.Heleket != nil {
		t.Fatal("strings must read as empty details (TS returns {})")
	}
	if details := ReadRechargePaymentDetails([]any{1, 2}); details.Antom != nil {
		t.Fatal("arrays must read as empty details")
	}

	raw := []byte(`{"kind":"heleket","paymentAttempt":{"current":{"id":"A","gatewayOrderNo":"R1--PA-A","createdAt":"t"},"superseded":[]},"recharge":{"payableAmount":"12.34","feeMode":"deduct"},"unknown":1}`)
	details := ReadRechargePaymentDetails(json.RawMessage(raw))
	if details.Kind == nil || *details.Kind != "heleket" {
		t.Fatalf("kind not parsed: %+v", details)
	}
	if details.Recharge == nil || details.Recharge.PayableAmount == nil || !details.Recharge.PayableAmount.Valid ||
		details.Recharge.PayableAmount.Value != 12.34 {
		t.Fatalf("payableAmount parsing failed (numeric strings must be accepted): %+v", details.Recharge)
	}
	if details.PaymentAttempt == nil || details.PaymentAttempt.Current.GatewayOrderNo != "R1--PA-A" {
		t.Fatalf("paymentAttempt lost: %+v", details.PaymentAttempt)
	}
	if _, ok := details.Extra["unknown"]; !ok {
		t.Fatal("unknown top-level keys must be preserved")
	}

	encoded, err := json.Marshal(details)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"unknown":1`) || !strings.Contains(string(encoded), `"paymentAttempt"`) {
		t.Fatalf("marshaling dropped data: %s", string(encoded))
	}

	if payable := GetRechargePayableAmount(RechargePayableSource{Amount: 10, PaymentDetails: json.RawMessage(raw)}); payable != 12.34 {
		t.Fatalf("payable amount must prefer paymentDetails: %v", payable)
	}
	if payable := GetRechargePayableAmount(RechargePayableSource{Amount: 10}); payable != 10 {
		t.Fatalf("payable amount must fall back to amount: %v", payable)
	}
	if payable := GetRechargePayableAmount(RechargePayableSource{Amount: 10, PaymentDetails: json.RawMessage(`{"recharge":{"payableAmount":0}}`)}); payable != 10 {
		t.Fatalf("a non-positive payableAmount must fall back: %v", payable)
	}
}

func TestRechargeMergeAmountDetailsKeepsOtherKeys(t *testing.T) {
	existing := json.RawMessage(`{"kind":"yipay","paymentAttempt":{"current":{"id":"A","gatewayOrderNo":"R1--PA-A","createdAt":"t"},"superseded":[]}}`)
	merged := MergeRechargeAmountDetails(existing, RechargeAmountMergeInput{
		Amount:        10,
		PayableAmount: 10.3,
		Fee:           0.3,
		FeeRate:       0.03,
		FeeFixed:      0,
	})
	if merged.Kind == nil || *merged.Kind != "yipay" {
		t.Fatal("kind must be preserved")
	}
	if merged.PaymentAttempt == nil || merged.PaymentAttempt.Current.ID != "A" {
		t.Fatal("paymentAttempt must be preserved")
	}
	if merged.Recharge == nil || merged.Recharge.FeeMode == nil || *merged.Recharge.FeeMode != "surcharge" {
		t.Fatalf("default fee mode mismatch: %+v", merged.Recharge)
	}
	if merged.Recharge.PaymentMethod != nil {
		t.Fatal("empty payment method must be null")
	}
	method := "alipay"
	merged = MergeRechargeAmountDetails(merged, RechargeAmountMergeInput{Amount: 10, PayableAmount: 10, FeeMode: &method, PaymentMethod: &method})
	if merged.Recharge.FeeMode == nil || *merged.Recharge.FeeMode != "alipay" {
		t.Fatalf("explicit fee mode must win: %+v", merged.Recharge)
	}
	if merged.Recharge.PaymentMethod == nil || *merged.Recharge.PaymentMethod != "alipay" {
		t.Fatal("payment method must be stored")
	}
}

func TestRechargeMergeHeleketAndAntom(t *testing.T) {
	config := HeleketConfig{Currency: "CNY", LifetimeSeconds: 3600}
	payment := map[string]any{
		"uuid":           "uuid-1",
		"order_id":       "R1",
		"amount":         "10.00",
		"payer_currency": "usdt",
		"network":        "trx",
		"txid":           "tx-1",
		"status":         "paid",
		"is_final":       true,
		"payment_amount": "1.5",
	}
	fallbackOrder := "R1"
	merged := MergeHeleketPaymentDetails(nil, payment, &config, &RechargeHeleketMergeFallback{OrderNo: &fallbackOrder})
	if merged.Kind == nil || *merged.Kind != "heleket" || merged.Heleket == nil {
		t.Fatalf("kind mismatch: %+v", merged)
	}
	if merged.Heleket.UUID == nil || *merged.Heleket.UUID != "uuid-1" || *merged.Heleket.PayerCurrency != "USDT" ||
		*merged.Heleket.Network != "TRX" || *merged.Heleket.PaymentMethodCode != "USDT@TRX" {
		t.Fatalf("heleket details mismatch: %+v", merged.Heleket)
	}
	if merged.Heleket.InvoiceAmount == nil || merged.Heleket.InvoiceAmount.Value != 10 {
		t.Fatalf("invoice amount mismatch: %+v", merged.Heleket.InvoiceAmount)
	}
	if merged.Heleket.Status == nil || *merged.Heleket.Status != "paid" {
		t.Fatal("status mismatch")
	}

	antomConfig := AntomConfig{Currency: "USD", CurrencyRate: 7.2, CurrencyExponent: 2}
	notification := map[string]any{
		"paymentRequestId": "R2",
		"paymentId":        "P2",
		"actualPaymentAmount": map[string]any{
			"currency": "USD",
			"value":    "7200",
		},
		"result": map[string]any{"resultStatus": "S", "resultMessage": "Success"},
	}
	antomMerged := MergeAntomPaymentDetails(nil, notification, antomConfig, RechargeAntomMergeFallback{OrderNo: "R2", AmountCNY: 10})
	if antomMerged.Kind == nil || *antomMerged.Kind != "antom" || antomMerged.Antom == nil {
		t.Fatalf("antom kind mismatch: %+v", antomMerged)
	}
	if *antomMerged.Antom.Currency != "USD" || *antomMerged.Antom.AmountMinor != "7200" {
		t.Fatalf("antom amount mismatch: %+v", antomMerged.Antom)
	}
	if antomMerged.Antom.Status == nil || *antomMerged.Antom.Status != "S" {
		t.Fatalf("antom status mismatch: %+v", antomMerged.Antom.Status)
	}
	if antomMerged.Antom.CurrencyRate == nil || antomMerged.Antom.CurrencyRate.Value != 7.2 {
		t.Fatal("antom currency rate mismatch")
	}
}

func TestRechargePaymentDisplayInfo(t *testing.T) {
	display := ExtractRechargePaymentDisplayInfo(json.RawMessage(`{"heleket":{"invoiceCurrency":"CNY","payerCurrency":"USDT","network":"TRX","paymentMethodCode":"USDT@TRX","uuid":"u","txid":"t","status":"paid","statusDescription":"ok"}}`))
	if display.InvoiceCurrency == nil || *display.InvoiceCurrency != "CNY" ||
		display.PaymentCurrency == nil || *display.PaymentCurrency != "USDT" ||
		display.PaymentNetwork == nil || *display.PaymentNetwork != "TRX" ||
		display.PaymentUUID == nil || *display.PaymentUUID != "u" ||
		display.PaymentTxID == nil || *display.PaymentTxID != "t" {
		t.Fatalf("heleket display mismatch: %+v", display)
	}

	display = ExtractRechargePaymentDisplayInfo(json.RawMessage(`{"antom":{"currency":"USD","paymentId":"P1","status":"SUCCESS","statusDescription":"done"}}`))
	if display.InvoiceCurrency == nil || *display.InvoiceCurrency != "USD" ||
		display.PaymentCurrency == nil || *display.PaymentCurrency != "USD" ||
		display.PaymentUUID == nil || *display.PaymentUUID != "P1" ||
		display.GatewayStatus == nil || *display.GatewayStatus != "SUCCESS" {
		t.Fatalf("antom display mismatch: %+v", display)
	}
}

// ==================== recharge order guard ====================

func TestRechargeOrderGuard(t *testing.T) {
	if NormalizeRechargeGatewayOrderNo(12) != nil {
		t.Fatal("non-strings normalise to nil")
	}
	if value := NormalizeRechargeGatewayOrderNo("  R1  "); value == nil || *value != "R1" {
		t.Fatal("trim mismatch")
	}
	if NormalizeRechargeGatewayOrderNo("   ") != nil {
		t.Fatal("blank normalises to nil")
	}
	if !IsRechargeGatewayOrderNoMatch("R1", " R1 ") {
		t.Fatal("trimmed match expected")
	}
	if IsRechargeGatewayOrderNoMatch("R1", "R2") || IsRechargeGatewayOrderNoMatch("R1", nil) {
		t.Fatal("mismatch expected")
	}
}

func ptrFloat(value float64) *float64 { return &value }
