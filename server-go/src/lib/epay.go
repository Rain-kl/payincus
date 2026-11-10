// Port of server/src/lib/epay.ts — the 易支付 (Epay) SDK.
//
// Two wire generations exist and both are load-bearing for payment callbacks:
//
//   - V1 signs with MD5 over `k=v&...` (keys sorted, `sign`/`sign_type` and empty
//     values dropped) plus the merchant key, and only trusts trade_status
//     "TRADE_SUCCESS";
//   - V2 signs with RSA-SHA256 (PKCS#1 v1.5) over the same canonical string and
//     additionally rejects callbacks whose `timestamp` is more than 300 seconds
//     away from now.
//
// Signature verification is the only thing standing between an attacker and a
// free balance top-up, so this file must stay behaviourally identical to the TS:
// same canonical string, same key wrapping, same failure results, same errors.
package lib

import (
	"context"
	"crypto"
	"crypto/md5"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EpayVersion mirrors the EpayVersion union in epay.ts.
type EpayVersion string

const (
	EpayVersionV1 EpayVersion = "v1"
	EpayVersionV2 EpayVersion = "v2"
)

// EpayConfig is the flattened union of EpayConfigV1 / EpayConfigV2 /
// EpayConfigLegacy. The route layer builds it from the provider's decrypted
// config, so a single struct keeps createEpayClient's dispatch rules in one place.
type EpayConfig struct {
	Version            EpayVersion
	APIURL             string
	PID                string
	Key                string
	PlatformPublicKey  string
	MerchantPrivateKey string
}

// EpayPayParams mirrors PayParams.
type EpayPayParams struct {
	Type       string
	OutTradeNo string
	Name       string
	Money      string
	NotifyURL  string
	ReturnURL  string
}

// EpayVerifyResult mirrors VerifyResult.
type EpayVerifyResult struct {
	Valid        bool   `json:"valid"`
	TradeSuccess bool   `json:"tradeSuccess"`
	Error        string `json:"error,omitempty"`
}

// EpayOrderQueryResult mirrors OrderQueryResult. Optional fields are omitted when
// empty, matching the TS `undefined` behaviour under JSON serialization.
type EpayOrderQueryResult struct {
	Success    bool           `json:"success"`
	Paid       bool           `json:"paid"`
	TradeNo    string         `json:"tradeNo,omitempty"`
	OutTradeNo string         `json:"outTradeNo,omitempty"`
	Money      string         `json:"money,omitempty"`
	Type       string         `json:"type,omitempty"`
	Error      string         `json:"error,omitempty"`
	RawData    map[string]any `json:"rawData,omitempty"`
}

// EpayClient mirrors IEpayClient.
type EpayClient interface {
	GetPayLink(params EpayPayParams) string
	Verify(data map[string]any) bool
	VerifyWithStatus(data map[string]any) EpayVerifyResult
	QueryOrder(ctx context.Context, outTradeNo string) EpayOrderQueryResult
}

// ==================== shared helpers ====================

// epayJSString mirrors JS String(v) for the value shapes that can appear in a
// callback payload (verified against node: numbers stringify without a trailing
// ".0", booleans as "true"/"false", null as "null").
func epayJSString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberString(typed)
	case float32:
		return jsNumberString(float64(typed))
	case int:
		return strconv.Itoa(typed)
	case int32:
		return strconv.FormatInt(int64(typed), 10)
	case int64:
		return strconv.FormatInt(typed, 10)
	case json.Number:
		return typed.String()
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if item == nil {
				parts = append(parts, "")
				continue
			}
			parts = append(parts, epayJSString(item))
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(value)
	}
}

// jsNumberString renders a float64 the way JS stringification does for the
// values that occur in payment callbacks (integers have no ".0"). Extremely
// large/small exponents differ from V8's shortest round-trip algorithm, but the
// canonical payment fields are money strings supplied by the gateway.
func jsNumberString(value float64) string {
	if math.IsNaN(value) {
		return "NaN"
	}
	if math.IsInf(value, 0) {
		if value > 0 {
			return "Infinity"
		}
		return "-Infinity"
	}
	if value == math.Trunc(value) && math.Abs(value) < 1e21 {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// epaySignContent mirrors getSignContent: keys sorted, empty/null/sign/sign_type
// skipped, joined as "k=v". skipArrays selects the V2 variant, which also drops
// array values (V1 does not).
func epaySignContent(params map[string]any, skipArrays bool) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := params[key]
		if skipArrays {
			if _, isArray := value.([]any); isArray {
				continue
			}
		}
		if value == nil || key == "sign" || key == "sign_type" {
			continue
		}
		if text, ok := value.(string); ok && text == "" {
			continue
		}
		parts = append(parts, key+"="+epayJSString(value))
	}
	return strings.Join(parts, "&")
}

// epayValueIsEmpty mirrors the `v === null || v === undefined || v === ”` guard
// used when building signed request parameters.
func epayValueIsEmpty(value any) bool {
	if value == nil {
		return true
	}
	if text, ok := value.(string); ok && text == "" {
		return true
	}
	return false
}

// epayRequestPair keeps JS object insertion order for the query string. The
// signature itself sorts keys, but the generated pay link must keep the field
// order the TS literal produces.
type epayRequestPair struct {
	Key   string
	Value string
}

// wrapPEMBody mirrors `key.match(/.{1,64}/g)?.join('\n')`.
func wrapPEMBody(body string) string {
	lines := make([]string, 0, len(body)/64+1)
	for len(body) > 64 {
		lines = append(lines, body[:64])
		body = body[64:]
	}
	if body != "" {
		lines = append(lines, body)
	}
	return strings.Join(lines, "\n")
}

// normalizePEMFullKey accepts either a full PEM document or a bare base64 body
// and returns something pem.Decode can read.
func normalizePEMFullKey(value, label, kind string) ([]byte, error) {
	trimmed := strings.TrimSpace(strings.ReplaceAll(value, `\n`, "\n"))
	if trimmed == "" {
		return nil, errors.New(label + "不能为空")
	}
	if strings.Contains(trimmed, "-----BEGIN ") {
		return []byte(trimmed), nil
	}
	body := strings.Join(strings.Fields(trimmed), "")
	document := "-----BEGIN " + kind + "-----\n" + wrapPEMBody(body) + "\n-----END " + kind + "-----"
	return []byte(document), nil
}

// parseRSAPublicKey mirrors the key material the TS wraps as "PUBLIC KEY".
func parseRSAPublicKey(value, label string) (*rsa.PublicKey, error) {
	document, err := normalizePEMFullKey(value, label, "PUBLIC KEY")
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(document)
	if block == nil {
		return nil, errors.New(label + "格式不合法")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New(label + "格式不合法")
	}
	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New(label + "格式不合法")
	}
	return publicKey, nil
}

// parseRSAPrivateKey mirrors the key material the TS wraps as "PRIVATE KEY"
// (PKCS#8). Node also accepts PKCS#1 bodies; that legacy shape is handled as a
// fallback so old provider rows keep working.
func parseRSAPrivateKey(value, label string) (*rsa.PrivateKey, error) {
	document, err := normalizePEMFullKey(value, label, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(document)
	if block == nil {
		return nil, errors.New(label + "格式不合法")
	}
	if parsed, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes); parseErr == nil {
		if privateKey, ok := parsed.(*rsa.PrivateKey); ok {
			return privateKey, nil
		}
	}
	if privateKey, parseErr := x509.ParsePKCS1PrivateKey(block.Bytes); parseErr == nil {
		return privateKey, nil
	}
	return nil, errors.New(label + "格式不合法")
}

// noRedirectClient mirrors fetch's redirect: "manual": the SDK must never follow
// a redirect, because the target was validated for the original URL only.
func noRedirectClient() *http.Client {
	clone := *SafeOutboundHTTPClient()
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone
}

// ==================== V1 (MD5) ====================

type epayCoreV1 struct {
	apiurl   string
	pid      string
	key      string
	signType string
}

func newEpayCoreV1(config EpayConfig) *epayCoreV1 {
	apiurl := config.APIURL
	if !strings.HasSuffix(apiurl, "/") {
		apiurl += "/"
	}
	return &epayCoreV1{apiurl: apiurl, pid: config.PID, key: config.Key, signType: "MD5"}
}

// buildRequestParam mirrors buildRequestParam: request fields in declaration
// order, then pid/sign/sign_type.
func (c *epayCoreV1) buildRequestParam(params EpayPayParams) []epayRequestPair {
	candidates := []epayRequestPair{
		{Key: "type", Value: params.Type},
		{Key: "out_trade_no", Value: params.OutTradeNo},
		{Key: "name", Value: params.Name},
		{Key: "money", Value: params.Money},
		{Key: "notify_url", Value: params.NotifyURL},
		{Key: "return_url", Value: params.ReturnURL},
	}
	values := make(map[string]any, len(candidates)+3)
	result := make([]epayRequestPair, 0, len(candidates)+3)
	for _, candidate := range candidates {
		if epayValueIsEmpty(candidate.Value) {
			continue
		}
		result = append(result, candidate)
		values[candidate.Key] = candidate.Value
	}
	values["pid"] = c.pid
	result = append(result, epayRequestPair{Key: "pid", Value: c.pid})
	sign := c.getSign(values)
	result = append(result, epayRequestPair{Key: "sign", Value: sign})
	result = append(result, epayRequestPair{Key: "sign_type", Value: c.signType})
	return result
}

func (c *epayCoreV1) getSign(params map[string]any) string {
	sum := md5.Sum([]byte(epaySignContent(params, false) + c.key))
	return hex.EncodeToString(sum[:])
}

// GetPayLink mirrors getPayLink.
func (c *epayCoreV1) GetPayLink(params EpayPayParams) string {
	requrl := c.apiurl + "submit.php"
	signed := c.buildRequestParam(params)
	parts := make([]string, 0, len(signed))
	for _, pair := range signed {
		parts = append(parts, pair.Key+"="+encodeURIComponent(pair.Value))
	}
	return requrl + "?" + strings.Join(parts, "&")
}

// Verify mirrors verify: signature only, no trade-status check.
func (c *epayCoreV1) Verify(data map[string]any) bool {
	if len(data) == 0 {
		return false
	}
	sign, ok := data["sign"].(string)
	if !ok || sign == "" {
		return false
	}
	return c.getSign(data) == sign
}

// VerifyWithStatus mirrors verifyWithStatus for V1.
func (c *epayCoreV1) VerifyWithStatus(data map[string]any) EpayVerifyResult {
	if len(data) == 0 {
		return EpayVerifyResult{Valid: false, TradeSuccess: false, Error: "缺少签名参数"}
	}
	sign, ok := data["sign"].(string)
	if !ok || sign == "" {
		return EpayVerifyResult{Valid: false, TradeSuccess: false, Error: "缺少签名参数"}
	}
	if c.getSign(data) != sign {
		return EpayVerifyResult{Valid: false, TradeSuccess: false, Error: "签名验证失败"}
	}
	tradeStatus, _ := data["trade_status"].(string)
	if tradeStatus != "TRADE_SUCCESS" {
		label := tradeStatus
		if label == "" {
			label = "未知状态"
		}
		return EpayVerifyResult{Valid: true, TradeSuccess: false, Error: "交易未成功: " + label}
	}
	return EpayVerifyResult{Valid: true, TradeSuccess: true}
}

// QueryOrder mirrors EpayCoreV1.queryOrder. The error strings are part of the
// API contract because the verify route surfaces them through its JSON.
func (c *epayCoreV1) QueryOrder(ctx context.Context, outTradeNo string) EpayOrderQueryResult {
	baseURL, err := AssertSafeHTTPURL(ctx, c.apiurl, "Epay API URL")
	if err != nil {
		return EpayOrderQueryResult{Success: false, Paid: false, Error: err.Error()}
	}
	apiurl := strings.TrimRight(baseURL.String(), "/") + "/"
	requestURL := apiurl + "api.php?act=order&pid=" + encodeURIComponent(c.pid) +
		"&key=" + encodeURIComponent(c.key) + "&out_trade_no=" + encodeURIComponent(outTradeNo)

	response, err := SafeFetch(ctx, requestURL, &SafeFetchInit{
		Method:  http.MethodGet,
		Headers: http.Header{"Accept": []string{"application/json"}},
	}, "Epay API URL")
	if err != nil {
		return EpayOrderQueryResult{Success: false, Paid: false, Error: err.Error()}
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return EpayOrderQueryResult{Success: false, Paid: false, Error: fmt.Sprintf("HTTP错误: %d", response.StatusCode)}
	}

	rawBody, err := io.ReadAll(response.Body)
	if err != nil {
		return EpayOrderQueryResult{Success: false, Paid: false, Error: err.Error()}
	}
	return parseEpayOrderQueryResponse(response.StatusCode, rawBody)
}

// parseEpayOrderQueryResponse mirrors the body handling of EpayCoreV1.queryOrder
// (split out so the wire contract is unit-testable without real networking).
func parseEpayOrderQueryResponse(statusCode int, rawBody []byte) EpayOrderQueryResult {
	if statusCode < 200 || statusCode >= 300 {
		return EpayOrderQueryResult{Success: false, Paid: false, Error: fmt.Sprintf("HTTP错误: %d", statusCode)}
	}
	var data map[string]any
	if err := json.Unmarshal(rawBody, &data); err != nil {
		return EpayOrderQueryResult{Success: false, Paid: false, Error: err.Error()}
	}

	// `data.code !== 1`: strict number comparison in JS.
	if code, ok := data["code"].(float64); !ok || code != 1 {
		message, _ := data["msg"].(string)
		if message == "" {
			message = "查询失败"
		}
		return EpayOrderQueryResult{Success: false, Paid: false, Error: message, RawData: data}
	}

	// status: 1 = paid, 0 = unpaid (strict number comparison).
	status, _ := data["status"].(float64)
	result := EpayOrderQueryResult{
		Success: true,
		Paid:    status == 1,
		RawData: data,
	}
	if value, ok := data["trade_no"].(string); ok {
		result.TradeNo = value
	}
	if value, ok := data["out_trade_no"].(string); ok {
		result.OutTradeNo = value
	}
	if value, ok := data["money"].(string); ok {
		result.Money = value
	}
	if value, ok := data["type"].(string); ok {
		result.Type = value
	}
	return result
}

// ==================== V2 (RSA) ====================

type epayCoreV2 struct {
	apiurl             string
	pid                string
	platformPublicKey  string
	merchantPrivateKey string
	signType           string
}

func newEpayCoreV2(config EpayConfig) *epayCoreV2 {
	apiurl := config.APIURL
	if !strings.HasSuffix(apiurl, "/") {
		apiurl += "/"
	}
	return &epayCoreV2{
		apiurl:             apiurl,
		pid:                config.PID,
		platformPublicKey:  config.PlatformPublicKey,
		merchantPrivateKey: config.MerchantPrivateKey,
		signType:           "RSA",
	}
}

func (c *epayCoreV2) buildRequestParam(params EpayPayParams) []epayRequestPair {
	candidates := []epayRequestPair{
		{Key: "type", Value: params.Type},
		{Key: "out_trade_no", Value: params.OutTradeNo},
		{Key: "name", Value: params.Name},
		{Key: "money", Value: params.Money},
		{Key: "notify_url", Value: params.NotifyURL},
		{Key: "return_url", Value: params.ReturnURL},
	}
	values := make(map[string]any, len(candidates)+4)
	result := make([]epayRequestPair, 0, len(candidates)+4)
	for _, candidate := range candidates {
		if epayValueIsEmpty(candidate.Value) {
			continue
		}
		result = append(result, candidate)
		values[candidate.Key] = candidate.Value
	}
	values["pid"] = c.pid
	result = append(result, epayRequestPair{Key: "pid", Value: c.pid})

	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	values["timestamp"] = timestamp
	result = append(result, epayRequestPair{Key: "timestamp", Value: timestamp})

	sign := c.getSign(values)
	result = append(result, epayRequestPair{Key: "sign", Value: sign})
	result = append(result, epayRequestPair{Key: "sign_type", Value: c.signType})
	return result
}

func (c *epayCoreV2) getSign(params map[string]any) string {
	content := epaySignContent(params, true)
	privateKey, err := parseRSAPrivateKey(c.merchantPrivateKey, "商户私钥")
	if err != nil {
		return ""
	}
	digest := sha256.Sum256([]byte(content))
	signature, err := rsa.SignPKCS1v15(nil, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(signature)
}

// GetPayLink mirrors EpayCoreV2.getPayLink.
func (c *epayCoreV2) GetPayLink(params EpayPayParams) string {
	requrl := c.apiurl + "api/pay/submit"
	signed := c.buildRequestParam(params)
	parts := make([]string, 0, len(signed))
	for _, pair := range signed {
		parts = append(parts, pair.Key+"="+encodeURIComponent(pair.Value))
	}
	return requrl + "?" + strings.Join(parts, "&")
}

// rsaPublicVerify mirrors rsaPublicVerify. Any key/parse problem is a failed
// verification, never an error the caller has to handle (the TS catches).
func (c *epayCoreV2) rsaPublicVerify(content, signature string) bool {
	publicKey, err := parseRSAPublicKey(c.platformPublicKey, "平台公钥")
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

// Verify mirrors verify for V2 (signature + timestamp window).
func (c *epayCoreV2) Verify(data map[string]any) bool {
	if len(data) == 0 {
		return false
	}
	sign, ok := data["sign"].(string)
	if !ok || sign == "" {
		return false
	}
	if timestamp, ok := data["timestamp"].(string); ok && timestamp != "" {
		// parseInt(timestamp, 10): a numeric prefix is enough, trailing junk is
		// ignored and a fully non-numeric value yields NaN (comparison false).
		if parsed, ok := parseIntJSValue(timestamp); ok {
			nowSeconds := float64(time.Now().UnixMilli()) / 1000
			if math.Abs(nowSeconds-float64(parsed)) > 300 {
				return false
			}
		}
	}
	return c.rsaPublicVerify(epaySignContent(data, true), sign)
}

// VerifyWithStatus mirrors verifyWithStatus for V2.
func (c *epayCoreV2) VerifyWithStatus(data map[string]any) EpayVerifyResult {
	if len(data) == 0 {
		return EpayVerifyResult{Valid: false, TradeSuccess: false, Error: "缺少签名参数"}
	}
	sign, ok := data["sign"].(string)
	if !ok || sign == "" {
		return EpayVerifyResult{Valid: false, TradeSuccess: false, Error: "缺少签名参数"}
	}
	if timestamp, ok := data["timestamp"].(string); ok && timestamp != "" {
		// parseInt(timestamp, 10): numeric prefix, trailing junk ignored.
		if parsed, ok := parseIntJSValue(timestamp); ok {
			nowSeconds := float64(time.Now().UnixMilli()) / 1000
			if math.Abs(nowSeconds-float64(parsed)) > 300 {
				return EpayVerifyResult{Valid: false, TradeSuccess: false, Error: "时间戳已过期"}
			}
		}
	}
	if !c.rsaPublicVerify(epaySignContent(data, true), sign) {
		return EpayVerifyResult{Valid: false, TradeSuccess: false, Error: "RSA签名验证失败"}
	}
	tradeStatus, _ := data["trade_status"].(string)
	if tradeStatus != "TRADE_SUCCESS" {
		label := tradeStatus
		if label == "" {
			label = "未知状态"
		}
		return EpayVerifyResult{Valid: true, TradeSuccess: false, Error: "交易未成功: " + label}
	}
	return EpayVerifyResult{Valid: true, TradeSuccess: true}
}

// QueryOrder mirrors EpayCoreV2.queryOrder: V2 has no MD5 key, so the standard
// query API is unavailable and callers must wait for the async notify_url.
func (c *epayCoreV2) QueryOrder(_ context.Context, _ string) EpayOrderQueryResult {
	return EpayOrderQueryResult{
		Success: false,
		Paid:    false,
		Error:   "V2版本暂不支持主动查询，请等待异步回调",
	}
}

// ==================== factory ====================

// CreateEpayClient mirrors createEpayClient.
func CreateEpayClient(config EpayConfig) EpayClient {
	if config.Version == EpayVersionV1 {
		return newEpayCoreV1(config)
	}
	// Legacy detection: a `key` without `platform_public_key`.
	if config.Version == "" && config.Key != "" && config.PlatformPublicKey == "" {
		return newEpayCoreV1(config)
	}
	return newEpayCoreV2(config)
}

// EpayCore is retained as the V2 alias, mirroring `export const EpayCore = EpayCoreV2`.
func EpayCore(config EpayConfig) EpayClient { return newEpayCoreV2(config) }

// base64DecodeLoose mirrors Buffer.from(value, 'base64'): Node accepts standard
// and URL-safe alphabets, ignores whitespace and tolerates missing padding.
func base64DecodeLoose(value string) ([]byte, error) {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		case '-':
			return '+'
		case '_':
			return '/'
		default:
			return r
		}
	}, value)
	if remainder := len(cleaned) % 4; remainder != 0 {
		cleaned += strings.Repeat("=", 4-remainder)
	}
	return base64.StdEncoding.DecodeString(cleaned)
}
