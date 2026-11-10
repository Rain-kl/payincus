// Port of server/src/lib/antom.ts (Antom / Alipay global, "Antom" gateway).
//
// Two independent signature checks live here:
//
//   - request signing: RSA-SHA256 over "POST <path>\n<clientId>.<requestTime>.<body>";
//   - webhook verification: the same content construction, using the headers
//     (client-id / request-time / signature) plus the *raw* request body. The raw
//     body is mandatory: re-marshalling parsed JSON would change bytes and break
//     the signature.
//
// HMAC-style comparison is not applicable (RSA), but the verification is still
// fail-closed: every parse/key error returns false.
package lib

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AntomEnvironment mirrors AntomEnvironment.
type AntomEnvironment string

const (
	AntomEnvironmentSandbox    AntomEnvironment = "sandbox"
	AntomEnvironmentProduction AntomEnvironment = "production"
)

const (
	antomDefaultEndpoint  = "https://open-sea-global.alipay.com"
	antomDefaultTimeoutMS = 15000
	antomDefaultCurrency  = "USD"
	antomDefaultExponent  = 2
	antomDefaultRate      = 1.0
	antomDefaultExpiry    = 30
	antomDefaultOrderDesc = "PayIncus account recharge"
)

var (
	antomEndpointHosts = map[string]bool{
		"open.antglobal-us.com":      true,
		"open-na-global.alipay.com":  true,
		"open-na.alipay.com":         true,
		"open-sea-global.alipay.com": true,
		"open-sea.alipay.com":        true,
		"open-de-global.alipay.com":  true,
		"open-eu.alipay.com":         true,
	}
	antomCurrencyPattern  = regexp.MustCompile(`^[A-Z]{3}$`)
	antomRegionPattern    = regexp.MustCompile(`^[A-Z]{2}$`)
	antomClientIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{3,64}$`)
	antomPaymentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	antomSignaturePattern = regexp.MustCompile(`(?i)(?:^|,\s*)signature=([^,\s]+)`)
)

// AntomConfig mirrors AntomConfig.
type AntomConfig struct {
	Environment          AntomEnvironment
	APIURL               string
	ClientID             string
	AntomPublicKey       string
	MerchantPrivateKey   string
	KeyVersion           int
	Currency             string
	CurrencyRate         float64
	CurrencyExponent     int
	MerchantRegion       string
	SettlementCurrency   string
	MerchantAccountID    string
	Locale               string
	OrderDescription     string
	SessionExpiryMinutes int
}

// AntomAmount mirrors AntomAmount.
type AntomAmount struct {
	Currency string `json:"currency"`
	Value    string `json:"value"`
}

// AntomResult mirrors AntomResult.
type AntomResult struct {
	ResultCode    string `json:"resultCode,omitempty"`
	ResultMessage string `json:"resultMessage,omitempty"`
	ResultStatus  string `json:"resultStatus,omitempty"`
}

// AntomPaymentSessionResponse mirrors AntomPaymentSessionResponse.
type AntomPaymentSessionResponse struct {
	NormalURL                string       `json:"normalUrl,omitempty"`
	PaymentSessionData       string       `json:"paymentSessionData,omitempty"`
	PaymentSessionExpiryTime string       `json:"paymentSessionExpiryTime,omitempty"`
	PaymentSessionID         string       `json:"paymentSessionId,omitempty"`
	Result                   *AntomResult `json:"result,omitempty"`
}

// AntomPaymentInquiryResponse mirrors AntomPaymentInquiryResponse.
type AntomPaymentInquiryResponse struct {
	Result               *AntomResult `json:"result,omitempty"`
	PaymentStatus        string       `json:"paymentStatus,omitempty"`
	PaymentResultCode    string       `json:"paymentResultCode,omitempty"`
	PaymentResultMessage string       `json:"paymentResultMessage,omitempty"`
	PaymentRequestID     string       `json:"paymentRequestId,omitempty"`
	PaymentID            string       `json:"paymentId,omitempty"`
	PaymentAmount        *AntomAmount `json:"paymentAmount,omitempty"`
	ActualPaymentAmount  *AntomAmount `json:"actualPaymentAmount,omitempty"`
}

// AntomPaymentNotification mirrors AntomPaymentNotification.
type AntomPaymentNotification struct {
	NotifyType          string       `json:"notifyType,omitempty"`
	PaymentRequestID    string       `json:"paymentRequestId,omitempty"`
	PaymentID           string       `json:"paymentId,omitempty"`
	PaymentAmount       *AntomAmount `json:"paymentAmount,omitempty"`
	ActualPaymentAmount *AntomAmount `json:"actualPaymentAmount,omitempty"`
	Result              *AntomResult `json:"result,omitempty"`
}

// AntomNotificationResponse mirrors antonNotificationResponse.
type AntomNotificationResponse struct {
	Result AntomNotificationResult `json:"result"`
}

// AntomNotificationResult is the inner result object of the notification reply.
type AntomNotificationResult struct {
	ResultCode    string `json:"resultCode"`
	ResultStatus  string `json:"resultStatus"`
	ResultMessage string `json:"resultMessage"`
}

// ==================== helpers ====================

func antomNormalizeString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

// antomNormalizePEM mirrors normalizePem: accept a full PEM document, convert
// literal "\n" sequences, or wrap a bare base64 body at 64 characters.
func antomNormalizePEM(value, label, kind string) (string, error) {
	trimmed := strings.TrimSpace(strings.ReplaceAll(value, `\n`, "\n"))
	if trimmed == "" {
		return "", errors.New(label + "不能为空")
	}
	if strings.Contains(trimmed, "-----BEGIN ") {
		return trimmed, nil
	}
	body := strings.Join(strings.Fields(trimmed), "")
	if !regexp.MustCompile(`^[A-Za-z0-9+/=]+$`).MatchString(body) {
		return "", errors.New(label + "格式不合法")
	}
	return "-----BEGIN " + kind + "-----\n" + wrapPEMBody(body) + "\n-----END " + kind + "-----", nil
}

// antomNormalizeEndpoint mirrors normalizeEndpoint.
func antomNormalizeEndpoint(value any) (string, error) {
	candidate := antomNormalizeString(value)
	if candidate == "" {
		candidate = antomDefaultEndpoint
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Scheme == "" {
		return "", errors.New("Antom API 地址不合法")
	}
	if parsed.Scheme != "https" || !antomEndpointHosts[strings.ToLower(parsed.Hostname())] {
		return "", errors.New("Antom API 地址必须使用官方 HTTPS 网关")
	}
	if parsed.Path != "/" && parsed.Path != "" {
		return "", errors.New("Antom API 地址只能填写官方网关域名，不要包含 API 路径")
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

// antomNumber mirrors `Number(value)` for the config fields (strings accepted).
func antomNumber(value any, fallback float64) float64 {
	switch typed := value.(type) {
	case nil:
		return fallback
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int32:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		parsed, err := strconv.ParseFloat(string(typed), 64)
		if err != nil {
			return math.NaN()
		}
		return parsed
	case string:
		return jsToNumber(typed)
	default:
		return math.NaN()
	}
}

// antomPickConfigValue mirrors `input.a ?? input.b`.
func antomPickConfigValue(input map[string]any, camel, snake string) any {
	if value, ok := input[camel]; ok && value != nil {
		return value
	}
	return input[snake]
}

// BuildAntomConfig mirrors buildAntomConfig.
func BuildAntomConfig(input map[string]any) (config AntomConfig, valid bool, errMessage string) {
	fallback := AntomConfig{
		Environment:          AntomEnvironmentSandbox,
		APIURL:               antomDefaultEndpoint,
		KeyVersion:           1,
		Currency:             antomDefaultCurrency,
		CurrencyRate:         antomDefaultRate,
		CurrencyExponent:     antomDefaultExponent,
		OrderDescription:     antomDefaultOrderDesc,
		SessionExpiryMinutes: antomDefaultExpiry,
	}

	fail := func(message string) (AntomConfig, bool, string) {
		return fallback, false, message
	}

	environment := AntomEnvironmentSandbox
	if input["environment"] == "production" {
		environment = AntomEnvironmentProduction
	}

	clientID := antomNormalizeString(antomPickConfigValue(input, "clientId", "client_id"))
	if !antomClientIDPattern.MatchString(clientID) {
		return fail("Antom Client ID 不合法")
	}

	currency := strings.ToUpper(antomNormalizeString(input["currency"]))
	if currency == "" {
		currency = antomDefaultCurrency
	}
	if !antomCurrencyPattern.MatchString(currency) {
		return fail("Antom 支付币种必须是三位大写代码")
	}

	currencyRate := antomNumber(antomPickConfigValue(input, "currencyRate", "currency_rate"), antomDefaultRate)
	if math.IsNaN(currencyRate) || math.IsInf(currencyRate, 0) || currencyRate <= 0 || currencyRate > 1_000_000 {
		return fail("Antom 币种换算率不合法")
	}

	currencyExponentFloat := antomNumber(antomPickConfigValue(input, "currencyExponent", "currency_exponent"), antomDefaultExponent)
	if !isJSSafeInteger(currencyExponentFloat) || currencyExponentFloat < 0 || currencyExponentFloat > 3 {
		return fail("Antom 币种小数位必须是 0 到 3 的整数")
	}

	keyVersionFloat := antomNumber(antomPickConfigValue(input, "keyVersion", "key_version"), 1)
	if !isJSSafeInteger(keyVersionFloat) || keyVersionFloat < 0 || keyVersionFloat > 999 {
		return fail("Antom Key Version 不合法")
	}

	sessionExpiryFloat := antomNumber(antomPickConfigValue(input, "sessionExpiryMinutes", "session_expiry_minutes"), antomDefaultExpiry)
	if !isJSSafeInteger(sessionExpiryFloat) || sessionExpiryFloat < 5 || sessionExpiryFloat > 60 {
		return fail("Antom 支付会话有效期必须在 5 到 60 分钟之间")
	}

	merchantRegion := strings.ToUpper(antomNormalizeString(antomPickConfigValue(input, "merchantRegion", "merchant_region")))
	if merchantRegion != "" && !antomRegionPattern.MatchString(merchantRegion) {
		return fail("Antom 商户地区必须是两位代码")
	}

	settlementCurrency := strings.ToUpper(antomNormalizeString(antomPickConfigValue(input, "settlementCurrency", "settlement_currency")))
	if settlementCurrency != "" && !antomCurrencyPattern.MatchString(settlementCurrency) {
		return fail("Antom 结算币种必须是三位大写代码")
	}

	merchantAccountID := antomNormalizeString(antomPickConfigValue(input, "merchantAccountId", "merchant_account_id"))
	if len([]rune(merchantAccountID)) > 64 {
		return fail("Antom Merchant Account ID 过长")
	}

	locale := antomNormalizeString(input["locale"])
	if len([]rune(locale)) > 16 {
		return fail("Antom 页面语言代码过长")
	}

	orderDescription := antomNormalizeString(antomPickConfigValue(input, "orderDescription", "order_description"))
	if orderDescription == "" {
		orderDescription = antomDefaultOrderDesc
	}
	if len([]rune(orderDescription)) > 256 {
		return fail("Antom 订单描述过长")
	}

	apiurl, err := antomNormalizeEndpoint(input["apiurl"])
	if err != nil {
		return fail(err.Error())
	}

	antomPublicKey, err := antomNormalizePEM(
		antomNormalizeString(antomPickConfigValue(input, "antom_public_key", "antomPublicKey")),
		"Antom 公钥", "PUBLIC KEY",
	)
	if err != nil {
		return fail(err.Error())
	}
	merchantPrivateKey, err := antomNormalizePEM(
		antomNormalizeString(antomPickConfigValue(input, "merchant_private_key", "merchantPrivateKey")),
		"商户私钥", "PRIVATE KEY",
	)
	if err != nil {
		return fail(err.Error())
	}

	return AntomConfig{
		Environment:          environment,
		APIURL:               apiurl,
		ClientID:             clientID,
		AntomPublicKey:       antomPublicKey,
		MerchantPrivateKey:   merchantPrivateKey,
		KeyVersion:           int(keyVersionFloat),
		Currency:             currency,
		CurrencyRate:         currencyRate,
		CurrencyExponent:     int(currencyExponentFloat),
		MerchantRegion:       merchantRegion,
		SettlementCurrency:   settlementCurrency,
		MerchantAccountID:    merchantAccountID,
		Locale:               locale,
		OrderDescription:     orderDescription,
		SessionExpiryMinutes: int(sessionExpiryFloat),
	}, true, ""
}

// AntomAPIPath mirrors getApiPath.
func AntomAPIPath(config AntomConfig, endpoint string) string {
	prefix := "/ams/api"
	if config.Environment == AntomEnvironmentSandbox {
		prefix = "/ams/sandbox/api"
	}
	return prefix + "/v1/payments/" + endpoint
}

// AntomCreateContentToSign mirrors createContentToSign.
func AntomCreateContentToSign(method, uri, clientID, requestTime, body string) string {
	return strings.ToUpper(method) + " " + uri + "\n" + clientID + "." + requestTime + "." + body
}

// parseAntomSignatureValue mirrors parseSignatureValue.
func parseAntomSignatureValue(header string) (string, bool) {
	match := antomSignaturePattern.FindStringSubmatch(header)
	if match == nil || match[1] == "" {
		return "", false
	}
	decoded, err := url.PathUnescape(match[1])
	if err != nil {
		return "", false
	}
	return decoded, true
}

// SignAntomContent mirrors signAntomContent (RSA-SHA256, base64).
func SignAntomContent(content, merchantPrivateKey string) (string, error) {
	privateKey, err := parseAntomRSAPrivateKey(merchantPrivateKey)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(content))
	signature, err := rsa.SignPKCS1v15(nil, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(signature), nil
}

// VerifyAntomContent mirrors verifyAntomContent.
func VerifyAntomContent(content, signatureHeader, antomPublicKey string) bool {
	signature, ok := parseAntomSignatureValue(signatureHeader)
	if !ok {
		return false
	}
	publicKey, err := parseAntomRSAPublicKey(antomPublicKey)
	if err != nil {
		return false
	}
	decoded, err := base64DecodeLoose(signature)
	if err != nil {
		return false
	}
	digest := sha256.Sum256([]byte(content))
	return rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], decoded) == nil
}

// parseAntomRSAPublicKey parses the normalized "PUBLIC KEY" PEM document.
func parseAntomRSAPublicKey(pemDocument string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemDocument))
	if block == nil {
		return nil, errors.New("Antom 公钥格式不合法")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("Antom 公钥格式不合法")
	}
	return publicKey, nil
}

// parseAntomRSAPrivateKey parses the normalized "PRIVATE KEY" PEM document,
// accepting both PKCS#8 and (legacy) PKCS#1 bodies.
func parseAntomRSAPrivateKey(pemDocument string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemDocument))
	if block == nil {
		return nil, errors.New("商户私钥格式不合法")
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if privateKey, ok := parsed.(*rsa.PrivateKey); ok {
			return privateKey, nil
		}
	}
	if privateKey, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return privateKey, nil
	}
	return nil, errors.New("商户私钥格式不合法")
}

// CalculateAntomMinorAmount mirrors calculateAntomMinorAmount.
func CalculateAntomMinorAmount(amountCNY float64, config AntomConfig) (string, error) {
	multiplier := math.Pow(10, float64(config.CurrencyExponent))
	minor := math.Round(amountCNY * config.CurrencyRate * multiplier)
	if math.IsNaN(minor) || math.IsInf(minor, 0) || math.Abs(minor) > 9007199254740991 || minor <= 0 {
		return "", errors.New("Antom 支付金额换算结果不合法")
	}
	return strconv.FormatInt(int64(minor), 10), nil
}

// AntomMinorAmountToCNY mirrors antonMinorAmountToCny.
func AntomMinorAmountToCNY(value any, config AntomConfig) *float64 {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	if !regexp.MustCompile(`^\d+$`).MatchString(text) {
		return nil
	}
	minor, err := strconv.ParseFloat(text, 64)
	if err != nil || !isJSSafeInteger(minor) || minor <= 0 {
		return nil
	}
	amountCNY := minor / math.Pow(10, float64(config.CurrencyExponent)) / config.CurrencyRate
	result := math.Round(amountCNY*100) / 100
	return &result
}

// VerifyAntomWebhook mirrors verifyAntomWebhook.
func VerifyAntomWebhook(config AntomConfig, method, requestURI, requestTime, clientID, signature, rawBody string) bool {
	if clientID != config.ClientID || requestTime == "" || rawBody == "" {
		return false
	}
	content := AntomCreateContentToSign(method, requestURI, clientID, requestTime, rawBody)
	return VerifyAntomContent(content, signature, config.AntomPublicKey)
}

// AntomNotificationResponseFor mirrors antonNotificationResponse.
func AntomNotificationResponseFor(success bool) AntomNotificationResponse {
	if success {
		return AntomNotificationResponse{Result: AntomNotificationResult{
			ResultCode: "SUCCESS", ResultStatus: "S", ResultMessage: "Success",
		}}
	}
	return AntomNotificationResponse{Result: AntomNotificationResult{
		ResultCode: "FAIL", ResultStatus: "F", ResultMessage: "Failed",
	}}
}

// ==================== client ====================

// AntomClient mirrors the AntomClient class.
type AntomClient struct {
	Config  AntomConfig
	Fetcher func(ctx context.Context, request *http.Request) (*http.Response, error)
}

// NewAntomClient builds a client with the shared no-redirect safe transport.
func NewAntomClient(config AntomConfig) *AntomClient {
	return &AntomClient{Config: config}
}

func (c *AntomClient) fetch(ctx context.Context, request *http.Request) (*http.Response, error) {
	if c.Fetcher != nil {
		return c.Fetcher(ctx, request)
	}
	client := *noRedirectClient()
	client.Timeout = time.Duration(antomDefaultTimeoutMS) * time.Millisecond
	return client.Do(request)
}

// call mirrors AntomClient.call: sign the request, verify the response signature.
func (c *AntomClient) call(ctx context.Context, endpoint string, payload map[string]any) (json.RawMessage, error) {
	path := AntomAPIPath(c.Config, endpoint)
	safeURL, err := AssertSafeHTTPURL(ctx, c.Config.APIURL+path, "Antom API URL")
	if err != nil {
		return nil, err
	}
	if !antomEndpointHosts[strings.ToLower(safeURL.Hostname())] {
		return nil, errors.New("Antom API 地址不在官方网关白名单中")
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	body := string(bodyBytes)
	requestTime := strconv.FormatInt(time.Now().UnixMilli(), 10)
	content := AntomCreateContentToSign("POST", path, c.Config.ClientID, requestTime, body)
	signature, err := SignAntomContent(content, c.Config.MerchantPrivateKey)
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, safeURL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("content-type", "application/json; charset=UTF-8")
	request.Header.Set("client-id", c.Config.ClientID)
	request.Header.Set("request-time", requestTime)
	request.Header.Set("signature", "algorithm=RSA256, keyVersion="+strconv.Itoa(c.Config.KeyVersion)+", signature="+url.QueryEscape(signature))

	response, err := c.fetch(ctx, request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, errors.New("Antom API 请求超时")
		}
		var netError net.Error
		if errors.As(err, &netError) && netError.Timeout() {
			return nil, errors.New("Antom API 请求超时")
		}
		return nil, err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Antom API 返回 HTTP %d", response.StatusCode)
	}

	responseClientID := response.Header.Get("client-id")
	responseTime := response.Header.Get("response-time")
	responseSignature := response.Header.Get("signature")
	responseContent := AntomCreateContentToSign("POST", path, responseClientID, responseTime, string(responseBody))
	if responseClientID != c.Config.ClientID || responseTime == "" ||
		!VerifyAntomContent(responseContent, responseSignature, c.Config.AntomPublicKey) {
		return nil, errors.New("Antom API 响应签名验证失败")
	}

	var probe any
	if err := json.Unmarshal(responseBody, &probe); err != nil {
		return nil, errors.New("Antom API 返回了无效 JSON")
	}
	return responseBody, nil
}

// CreatePaymentSession mirrors AntomClient.createPaymentSession.
func (c *AntomClient) CreatePaymentSession(ctx context.Context, orderNo string, amountCNY float64, notifyURL, redirectURL string) (*AntomPaymentSessionResponse, error) {
	if !antomPaymentIDPattern.MatchString(orderNo) {
		return nil, errors.New("Antom 订单号不合法")
	}
	amountValue, err := CalculateAntomMinorAmount(amountCNY, c.Config)
	if err != nil {
		return nil, err
	}
	amount := map[string]any{"currency": c.Config.Currency, "value": amountValue}
	payload := map[string]any{
		"productCode":      "CASHIER_PAYMENT",
		"productScene":     "CHECKOUT_PAYMENT",
		"paymentRequestId": orderNo,
		"paymentAmount":    amount,
		"order": map[string]any{
			"referenceOrderId": orderNo,
			"orderDescription": c.Config.OrderDescription,
			"orderAmount":      amount,
		},
		"paymentNotifyUrl":         notifyURL,
		"paymentRedirectUrl":       redirectURL,
		"paymentSessionExpiryTime": time.Now().Add(time.Duration(c.Config.SessionExpiryMinutes) * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z"),
		"env":                      map[string]any{"terminalType": "WEB"},
	}
	if c.Config.MerchantRegion != "" {
		payload["merchantRegion"] = c.Config.MerchantRegion
	}
	if c.Config.SettlementCurrency != "" {
		payload["settlementStrategy"] = map[string]any{"settlementCurrency": c.Config.SettlementCurrency}
	}
	if c.Config.MerchantAccountID != "" {
		payload["merchantAccountId"] = c.Config.MerchantAccountID
	}
	if c.Config.Locale != "" {
		payload["locale"] = c.Config.Locale
	}

	raw, err := c.call(ctx, "createPaymentSession", payload)
	if err != nil {
		return nil, err
	}
	var parsed AntomPaymentSessionResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, errors.New("Antom API 返回了无效 JSON")
	}
	return &parsed, nil
}

// InquiryPayment mirrors AntomClient.inquiryPayment.
func (c *AntomClient) InquiryPayment(ctx context.Context, paymentRequestID string) (*AntomPaymentInquiryResponse, error) {
	if !antomPaymentIDPattern.MatchString(paymentRequestID) {
		return nil, errors.New("Antom 订单号不合法")
	}
	raw, err := c.call(ctx, "inquiryPayment", map[string]any{"paymentRequestId": paymentRequestID})
	if err != nil {
		return nil, err
	}
	var parsed AntomPaymentInquiryResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, errors.New("Antom API 返回了无效 JSON")
	}
	return &parsed, nil
}
