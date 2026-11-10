// Port of server/src/lib/heleket.ts.
//
// The security-critical part is verifyHeleketWebhookSignature: it recomputes
// MD5(base64(JSON(payload without "sign")) + apiKey) and compares it in constant
// time. Heleket signs the *serialized* body, so field order and escaping matter:
//
//   - JSON.stringify preserves the key insertion order of the received body, so
//     the Go verifier parses an ordered tree instead of a plain map (a map would
//     re-serialize with sorted keys and every genuine callback would fail);
//   - the TS then replaces every '/' with '\/', which encoding/json never does.
//
// Both details are reproduced below; they are covered by tests.
package lib

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HeleketConfig mirrors the TS interface.
type HeleketConfig struct {
	APIURL          string
	MerchantUUID    string
	APIKey          string
	Currency        string
	LifetimeSeconds int
}

// HeleketPayment mirrors the order/payment payload. Fields are read
// case-by-case from the JSON object, so the map representation below is only a
// convenience for callers that already have a typed value.
type HeleketPayment map[string]any

// HeleketPaymentService mirrors the services listing entry.
type HeleketPaymentService struct {
	Network     string         `json:"network"`
	Currency    string         `json:"currency"`
	IsAvailable bool           `json:"is_available"`
	IsEnabled   bool           `json:"is_enabled"`
	Limit       map[string]any `json:"limit,omitempty"`
	Commission  map[string]any `json:"commission,omitempty"`
}

// HeleketStructuredPaymentDetails mirrors HeleketStructuredPaymentDetails.
type HeleketStructuredPaymentDetails struct {
	Status            string  `json:"status"`
	StatusDescription string  `json:"statusDescription"`
	UUID              *string `json:"uuid"`
	TxID              *string `json:"txid"`
	PayerCurrency     *string `json:"payerCurrency"`
	Network           *string `json:"network"`
	PaymentMethodCode *string `json:"paymentMethodCode"`
	PaymentAmount     *string `json:"paymentAmount"`
	PaymentAmountUSD  *string `json:"paymentAmountUsd"`
	MerchantAmount    *string `json:"merchantAmount"`
	Address           *string `json:"address"`
	From              *string `json:"from"`
}

// HeleketPaymentState mirrors HeleketPaymentState.
type HeleketPaymentState string

const (
	HeleketPaymentStatePaid      HeleketPaymentState = "paid"
	HeleketPaymentStatePending   HeleketPaymentState = "pending"
	HeleketPaymentStateCancelled HeleketPaymentState = "cancelled"
	HeleketPaymentStateFailed    HeleketPaymentState = "failed"
)

const (
	heleketDefaultLifetimeSeconds = 3600
	heleketMinLifetimeSeconds     = 300
	heleketMaxLifetimeSeconds     = 43200
)

var (
	heleketPaidStatuses    = map[string]bool{"paid": true, "paid_over": true}
	heleketPendingStatuses = map[string]bool{
		"process": true, "confirm_check": true, "wrong_amount_waiting": true, "check": true,
	}
	heleketCancelledStatuses = map[string]bool{"cancel": true}
	heleketFailedStatuses    = map[string]bool{
		"wrong_amount": true, "fail": true, "system_fail": true, "refund_process": true,
		"refund_fail": true, "refund_paid": true, "locked": true,
	}
)

// ==================== ordered JSON ====================

// HeleketOrderedObject is a JSON object that preserves key insertion order, which
// is what JSON.parse + JSON.stringify round-trips in Node.
type HeleketOrderedObject struct {
	keys   []string
	values map[string]any
}

// NewHeleketOrderedObject returns an empty ordered object.
func NewHeleketOrderedObject() *HeleketOrderedObject {
	return &HeleketOrderedObject{values: map[string]any{}}
}

// Set inserts or overwrites a key. Overwriting keeps the original position,
// matching JS object assignment semantics.
func (o *HeleketOrderedObject) Set(key string, value any) {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// Get returns the value and whether the key exists.
func (o *HeleketOrderedObject) Get(key string) (any, bool) {
	value, ok := o.values[key]
	return value, ok
}

// Delete removes a key, preserving the order of the rest.
func (o *HeleketOrderedObject) Delete(key string) {
	if _, exists := o.values[key]; !exists {
		return
	}
	delete(o.values, key)
	for i, existing := range o.keys {
		if existing == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Keys returns the key order.
func (o *HeleketOrderedObject) Keys() []string { return append([]string(nil), o.keys...) }

// Len returns the number of keys.
func (o *HeleketOrderedObject) Len() int { return len(o.keys) }

// MarshalJSON serializes in insertion order with JS JSON.stringify escaping.
func (o *HeleketOrderedObject) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	if err := heleketWriteJSON(&buffer, o); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// ParseHeleketOrderedJSON parses arbitrary JSON keeping object key order.
func ParseHeleketOrderedJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := heleketDecodeOrderedValue(decoder)
	if err != nil {
		return nil, err
	}
	// Reject trailing garbage, mirroring JSON.parse.
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("invalid JSON: trailing data")
	}
	return value, nil
}

func heleketDecodeOrderedValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		if number, ok := token.(json.Number); ok {
			parsed, err := strconv.ParseFloat(number.String(), 64)
			if err != nil {
				return nil, err
			}
			return parsed, nil
		}
		return token, nil
	}
	switch delim {
	case '{':
		object := NewHeleketOrderedObject()
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid JSON object key")
			}
			value, err := heleketDecodeOrderedValue(decoder)
			if err != nil {
				return nil, err
			}
			object.Set(key, value)
		}
		if _, err := decoder.Token(); err != nil { // consume '}'
			return nil, err
		}
		return object, nil
	case '[':
		items := make([]any, 0)
		for decoder.More() {
			value, err := heleketDecodeOrderedValue(decoder)
			if err != nil {
				return nil, err
			}
			items = append(items, value)
		}
		if _, err := decoder.Token(); err != nil { // consume ']'
			return nil, err
		}
		return items, nil
	default:
		return nil, errors.New("invalid JSON delimiter")
	}
}

// heleketWriteJSON writes values the way JSON.stringify does (plus the '\r'
// escape), including the '/' -> '\/' replacement the Heleket signer requires.
func heleketWriteJSON(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		buffer.WriteString("null")
	case bool:
		if typed {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
	case string:
		heleketWriteJSONString(buffer, typed)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			buffer.WriteString("null")
			return nil
		}
		buffer.WriteString(jsNumberString(typed))
	case int:
		buffer.WriteString(strconv.Itoa(typed))
	case int32:
		buffer.WriteString(strconv.FormatInt(int64(typed), 10))
	case int64:
		buffer.WriteString(strconv.FormatInt(typed, 10))
	case json.Number:
		buffer.WriteString(typed.String())
	case []any:
		buffer.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				buffer.WriteByte(',')
			}
			if err := heleketWriteJSON(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
	case *HeleketOrderedObject:
		buffer.WriteByte('{')
		for i, key := range typed.keys {
			if i > 0 {
				buffer.WriteByte(',')
			}
			heleketWriteJSONString(buffer, key)
			buffer.WriteByte(':')
			if err := heleketWriteJSON(buffer, typed.values[key]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
	case map[string]any:
		// Fallback: sorted keys. Ordered inputs are produced by the parser or the
		// outbound request builders; a plain map cannot preserve JS insertion order.
		return errors.New("heleket: map values must be wrapped in HeleketOrderedObject to preserve key order")
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		buffer.Write(encoded)
	}
	return nil
}

// heleketWriteJSONString escapes like JSON.stringify + the signer's '/' rule.
func heleketWriteJSONString(buffer *bytes.Buffer, value string) {
	buffer.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			buffer.WriteString(`\"`)
		case '\\':
			buffer.WriteString(`\\`)
		case '\b':
			buffer.WriteString(`\b`)
		case '\f':
			buffer.WriteString(`\f`)
		case '\n':
			buffer.WriteString(`\n`)
		case '\r':
			buffer.WriteString(`\r`)
		case '\t':
			buffer.WriteString(`\t`)
		case '/':
			buffer.WriteString(`\/`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buffer, `\u%04x`, r)
				continue
			}
			buffer.WriteRune(r)
		}
	}
	buffer.WriteByte('"')
}

// ==================== config ====================

// parseHeleketLifetimeSeconds mirrors parseHeleketLifetimeSeconds.
func parseHeleketLifetimeSeconds(value any) int {
	var parsed float64
	switch typed := value.(type) {
	case float64:
		parsed = typed
	case float32:
		parsed = float64(typed)
	case int:
		parsed = float64(typed)
	case int32:
		parsed = float64(typed)
	case int64:
		parsed = float64(typed)
	case json.Number:
		converted, err := typed.Float64()
		if err != nil {
			parsed = math.NaN()
		} else {
			parsed = converted
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			parsed = math.NaN()
		} else {
			number, ok := parseIntJSValue(trimmed)
			if !ok {
				parsed = math.NaN()
			} else {
				parsed = float64(number)
			}
		}
	default:
		parsed = math.NaN()
	}

	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return heleketDefaultLifetimeSeconds
	}
	return int(math.Min(
		heleketMaxLifetimeSeconds,
		math.Max(heleketMinLifetimeSeconds, math.Trunc(parsed)),
	))
}

func heleketNormalizeString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

// BuildHeleketConfig mirrors buildHeleketConfig.
func BuildHeleketConfig(config map[string]any) (HeleketConfig, bool, string) {
	apiurl := heleketNormalizeString(config["apiurl"])
	if apiurl == "" {
		apiurl = "https://api.heleket.com"
	}
	merchantUUID, _ := config["merchant_uuid"].(string)
	apiKey, _ := config["api_key"].(string)
	currency := strings.ToUpper(heleketNormalizeString(config["currency"]))
	if currency == "" {
		currency = "CNY"
	}
	lifetimeValue := config["lifetime"]
	if lifetimeValue == nil {
		lifetimeValue = config["lifetime_seconds"]
	}

	heleketConfig := HeleketConfig{
		APIURL:          apiurl,
		MerchantUUID:    strings.TrimSpace(merchantUUID),
		APIKey:          strings.TrimSpace(apiKey),
		Currency:        currency,
		LifetimeSeconds: parseHeleketLifetimeSeconds(lifetimeValue),
	}
	if heleketConfig.APIURL == "" || heleketConfig.MerchantUUID == "" || heleketConfig.APIKey == "" {
		return heleketConfig, false, "支付渠道配置不完整（Heleket 需要接口地址、商户 UUID 和 API Key）"
	}
	return heleketConfig, true, ""
}

// normaliseHeleketAPIURL mirrors normalizeApiUrl.
func normaliseHeleketAPIURL(apiurl string) string {
	trimmed := strings.TrimSpace(apiurl)
	if strings.HasSuffix(trimmed, "/") {
		return trimmed[:len(trimmed)-1]
	}
	return trimmed
}

// SerializeHeleketPayload mirrors serializeHeleketPayload. It requires an
// ordered input; plain maps are rejected so nobody silently signs a reordered body.
func SerializeHeleketPayload(payload any) (string, error) {
	var buffer bytes.Buffer
	if err := heleketWriteJSON(&buffer, payload); err != nil {
		return "", err
	}
	return buffer.String(), nil
}

// SignHeleketPayload mirrors signHeleketPayload.
func SignHeleketPayload(payload any, apiKey string) (string, error) {
	serialized, err := SerializeHeleketPayload(payload)
	if err != nil {
		return "", err
	}
	digest := md5.Sum([]byte(base64.StdEncoding.EncodeToString([]byte(serialized)) + apiKey))
	return hex.EncodeToString(digest[:]), nil
}

// VerifyHeleketWebhookSignature mirrors verifyHeleketWebhookSignature.
func VerifyHeleketWebhookSignature(payload any, apiKey string) bool {
	object, ok := payload.(*HeleketOrderedObject)
	if !ok {
		// Callers that only have a map cannot reproduce the signed byte order;
		// failing closed is the only safe answer for a payment callback.
		return false
	}
	signValue, exists := object.Get("sign")
	if !exists {
		return false
	}
	sign, ok := signValue.(string)
	if !ok || sign == "" {
		return false
	}

	unsigned := NewHeleketOrderedObject()
	for _, key := range object.Keys() {
		if key == "sign" {
			continue
		}
		value, _ := object.Get(key)
		unsigned.Set(key, value)
	}

	expected, err := SignHeleketPayload(unsigned, apiKey)
	if err != nil {
		return false
	}
	if len(expected) != len(sign) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(sign)) == 1
}

// ==================== status helpers ====================

// ExtractHeleketStatus mirrors extractHeleketStatus (`status ?? payment_status`).
func ExtractHeleketStatus(payment map[string]any) string {
	if value, ok := payment["status"]; ok && value != nil {
		if text, ok := value.(string); ok {
			return text
		}
	}
	if value, ok := payment["payment_status"]; ok && value != nil {
		if text, ok := value.(string); ok {
			return text
		}
	}
	return ""
}

// IsHeleketPaidStatus mirrors isHeleketPaidStatus.
func IsHeleketPaidStatus(status string) bool { return heleketPaidStatuses[status] }

// GetHeleketPaymentState mirrors getHeleketPaymentState.
func GetHeleketPaymentState(payment map[string]any) HeleketPaymentState {
	status := ExtractHeleketStatus(payment)
	isFinal, _ := payment["is_final"].(bool)

	if heleketPaidStatuses[status] {
		return HeleketPaymentStatePaid
	}
	if heleketCancelledStatuses[status] {
		return HeleketPaymentStateCancelled
	}
	if heleketFailedStatuses[status] {
		return HeleketPaymentStateFailed
	}
	if heleketPendingStatuses[status] {
		return HeleketPaymentStatePending
	}
	if isFinal {
		return HeleketPaymentStateFailed
	}
	return HeleketPaymentStatePending
}

// GetHeleketStatusDescription mirrors getHeleketStatusDescription.
func GetHeleketStatusDescription(status string) string {
	switch status {
	case "paid":
		return "支付成功，金额与账单一致"
	case "paid_over":
		return "支付成功，用户超额付款"
	case "wrong_amount":
		return "支付金额不足"
	case "process":
		return "支付处理中"
	case "confirm_check":
		return "链上已发现交易，等待确认数"
	case "wrong_amount_waiting":
		return "支付金额不足，等待补付"
	case "check":
		return "等待链上出现交易"
	case "fail":
		return "支付失败"
	case "cancel":
		return "支付已取消"
	case "system_fail":
		return "支付系统异常"
	case "refund_process":
		return "退款处理中"
	case "refund_fail":
		return "退款失败"
	case "refund_paid":
		return "已退款"
	case "locked":
		return "资金因 AML 风控被锁定"
	default:
		if status == "" {
			return "未知支付状态"
		}
		return status
	}
}

// heleketTrimmedString mirrors `typeof x === 'string' && x.trim() ? x.trim() : null`.
func heleketTrimmedString(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// ExtractHeleketPaymentDetails mirrors extractHeleketPaymentDetails.
func ExtractHeleketPaymentDetails(payment map[string]any) HeleketStructuredPaymentDetails {
	status := ExtractHeleketStatus(payment)
	payerCurrency := heleketTrimmedString(payment["payer_currency"])
	if payerCurrency != nil {
		upper := strings.ToUpper(*payerCurrency)
		payerCurrency = &upper
	}
	network := heleketTrimmedString(payment["network"])
	if network != nil {
		upper := strings.ToUpper(*network)
		network = &upper
	}
	var paymentMethodCode *string
	if payerCurrency != nil {
		code := *payerCurrency
		if network != nil {
			code = *payerCurrency + "@" + *network
		}
		paymentMethodCode = &code
	}

	return HeleketStructuredPaymentDetails{
		Status:            status,
		StatusDescription: GetHeleketStatusDescription(status),
		UUID:              heleketTrimmedString(payment["uuid"]),
		TxID:              heleketTrimmedString(payment["txid"]),
		PayerCurrency:     payerCurrency,
		Network:           network,
		PaymentMethodCode: paymentMethodCode,
		PaymentAmount:     heleketTrimmedString(payment["payment_amount"]),
		PaymentAmountUSD:  heleketTrimmedString(payment["payment_amount_usd"]),
		MerchantAmount:    heleketTrimmedString(payment["merchant_amount"]),
		Address:           heleketTrimmedString(payment["address"]),
		From:              heleketTrimmedString(payment["from"]),
	}
}

// GetHeleketInvoiceAmount mirrors getHeleketInvoiceAmount: a positive parseFloat
// of the string `amount` field, otherwise nil.
func GetHeleketInvoiceAmount(payment map[string]any) *float64 {
	raw, ok := payment["amount"].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	parsed, ok := jsParseFloatPrefix(strings.TrimSpace(raw))
	if !ok || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
		return nil
	}
	return &parsed
}

// jsParseFloatPrefix mirrors parseFloat: the longest numeric prefix, or false
// when there is none.
func jsParseFloatPrefix(value string) (float64, bool) {
	index := 0
	for index < len(value) && (value[index] == ' ' || value[index] == '\t' || value[index] == '\n' || value[index] == '\r') {
		index++
	}
	start := index
	if index < len(value) && (value[index] == '+' || value[index] == '-') {
		index++
	}
	digits := 0
	for index < len(value) && value[index] >= '0' && value[index] <= '9' {
		index++
		digits++
	}
	if index < len(value) && value[index] == '.' {
		index++
		for index < len(value) && value[index] >= '0' && value[index] <= '9' {
			index++
			digits++
		}
	}
	if digits == 0 {
		return 0, false
	}
	end := index
	if index < len(value) && (value[index] == 'e' || value[index] == 'E') {
		exponentIndex := index + 1
		if exponentIndex < len(value) && (value[exponentIndex] == '+' || value[exponentIndex] == '-') {
			exponentIndex++
		}
		exponentDigits := 0
		for exponentIndex < len(value) && value[exponentIndex] >= '0' && value[exponentIndex] <= '9' {
			exponentIndex++
			exponentDigits++
		}
		if exponentDigits > 0 {
			end = exponentIndex
		}
	}
	parsed, err := strconv.ParseFloat(value[start:end], 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// ==================== client ====================

// HeleketInvoiceRequest is the outbound create-invoice payload; the field order
// below is the order JSON.stringify produces in the TS, which is what gets signed.
type HeleketInvoiceRequest struct {
	Amount         string
	Currency       string
	OrderID        string
	Lifetime       *int
	URLReturn      string
	URLSuccess     string
	URLCallback    string
	AdditionalData any
	IsRefresh      *bool
}

// ToOrderedObject converts the outbound request into the signed ordered form,
// skipping unset optional fields exactly like JSON.stringify skips undefined.
func (r HeleketInvoiceRequest) ToOrderedObject() *HeleketOrderedObject {
	object := NewHeleketOrderedObject()
	object.Set("amount", r.Amount)
	object.Set("currency", r.Currency)
	object.Set("order_id", r.OrderID)
	if r.Lifetime != nil {
		object.Set("lifetime", *r.Lifetime)
	}
	if r.URLReturn != "" {
		object.Set("url_return", r.URLReturn)
	}
	if r.URLSuccess != "" {
		object.Set("url_success", r.URLSuccess)
	}
	if r.URLCallback != "" {
		object.Set("url_callback", r.URLCallback)
	}
	if r.AdditionalData != nil {
		object.Set("additional_data", r.AdditionalData)
	}
	if r.IsRefresh != nil {
		object.Set("is_refresh", *r.IsRefresh)
	}
	return object
}

// HeleketPaymentInfoRequest mirrors HeleketPaymentInfoRequest. Only one of the
// two identifiers is normally supplied; both absent would be rejected by the API.
type HeleketPaymentInfoRequest struct {
	UUID    string
	OrderID string
}

// ToOrderedObject builds the signed payload preserving TS literal order.
func (r HeleketPaymentInfoRequest) ToOrderedObject() *HeleketOrderedObject {
	object := NewHeleketOrderedObject()
	if r.UUID != "" {
		object.Set("uuid", r.UUID)
	}
	if r.OrderID != "" {
		object.Set("order_id", r.OrderID)
	}
	return object
}

// HeleketClient mirrors the HeleketClient interface.
type HeleketClient interface {
	CreateInvoice(ctx context.Context, input HeleketInvoiceRequest) (map[string]any, error)
	GetPaymentInfo(ctx context.Context, input HeleketPaymentInfoRequest) (map[string]any, error)
	ListServices(ctx context.Context) ([]HeleketPaymentService, error)
	VerifyWebhookSignature(payload any) bool
}

type heleketClient struct {
	config HeleketConfig
}

// CreateHeleketClient mirrors createHeleketClient.
func CreateHeleketClient(config HeleketConfig) HeleketClient {
	return &heleketClient{config: config}
}

// heleketRequest mirrors heleketRequest.
func heleketRequest(ctx context.Context, config HeleketConfig, path string, payload *HeleketOrderedObject) (json.RawMessage, error) {
	body, err := SerializeHeleketPayload(payload)
	if err != nil {
		return nil, err
	}
	signature, err := SignHeleketPayload(payload, config.APIKey)
	if err != nil {
		return nil, err
	}

	safeBaseURL, err := AssertSafeHTTPURL(ctx, config.APIURL, "Heleket API URL")
	if err != nil {
		return nil, err
	}
	requestURL := normaliseHeleketAPIURL(safeBaseURL.String()) + path

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("merchant", config.MerchantUUID)
	request.Header.Set("sign", signature)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	timeoutClient := *noRedirectClient()
	timeoutClient.Timeout = 15 * time.Second
	response, err := timeoutClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	rawBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("Heleket 返回了无法解析的响应（HTTP %d）", response.StatusCode)
	}

	var data struct {
		State   *float64            `json:"state"`
		Result  json.RawMessage     `json:"result"`
		Message string              `json:"message"`
		Errors  map[string][]string `json:"errors"`
	}
	if err := json.Unmarshal(rawBody, &data); err != nil {
		return nil, fmt.Errorf("Heleket 返回了无法解析的响应（HTTP %d）", response.StatusCode)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 || data.State == nil || *data.State != 0 || len(data.Result) == 0 {
		return nil, errors.New(heleketFormatError(data.Message, data.Errors, fmt.Sprintf("Heleket 请求失败（HTTP %d）", response.StatusCode)))
	}
	return data.Result, nil
}

// heleketFormatError mirrors formatHeleketError.
func heleketFormatError(message string, errorsMap map[string][]string, fallback string) string {
	if strings.TrimSpace(message) != "" {
		return message
	}
	for _, values := range errorsMap {
		if len(values) > 0 && values[0] != "" {
			return values[0]
		}
	}
	return fallback
}

// CreateInvoice mirrors createHeleketClient().createInvoice.
func (c *heleketClient) CreateInvoice(ctx context.Context, input HeleketInvoiceRequest) (map[string]any, error) {
	raw, err := heleketRequest(ctx, c.config, "/v1/payment", input.ToOrderedObject())
	if err != nil {
		return nil, err
	}
	var payment map[string]any
	if err := json.Unmarshal(raw, &payment); err != nil {
		return nil, err
	}
	return payment, nil
}

// GetPaymentInfo mirrors createHeleketClient().getPaymentInfo.
func (c *heleketClient) GetPaymentInfo(ctx context.Context, input HeleketPaymentInfoRequest) (map[string]any, error) {
	raw, err := heleketRequest(ctx, c.config, "/v1/payment/info", input.ToOrderedObject())
	if err != nil {
		return nil, err
	}
	var payment map[string]any
	if err := json.Unmarshal(raw, &payment); err != nil {
		return nil, err
	}
	return payment, nil
}

// ListServices mirrors createHeleketClient().listServices.
func (c *heleketClient) ListServices(ctx context.Context) ([]HeleketPaymentService, error) {
	raw, err := heleketRequest(ctx, c.config, "/v1/payment/services", NewHeleketOrderedObject())
	if err != nil {
		return nil, err
	}
	var services []HeleketPaymentService
	if err := json.Unmarshal(raw, &services); err != nil {
		return nil, err
	}
	return services, nil
}

// VerifyWebhookSignature mirrors createHeleketClient().verifyWebhookSignature.
func (c *heleketClient) VerifyWebhookSignature(payload any) bool {
	return VerifyHeleketWebhookSignature(payload, c.config.APIKey)
}
