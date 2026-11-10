// Port of server/src/routes/recharge.ts — the recharge / payment module.
//
// Registered with absolute paths on the root router (app.ts does
// `fastify.register(rechargeRoutes)` without a prefix).
//
// Payment invariants this file must preserve (see docs/WAVE_D_SPEC.md §5):
//
//  1. Every callback is signature-verified through the ported SDKs
//     (lib/epay.go MD5|RSA, lib/heleket.go HMAC-MD5, lib/antom.go RSA-SHA256).
//     Nothing in the callback body (amount / status) is trusted before that.
//  2. The callback amount must match the order's payable amount (1 cent
//     tolerance). A mismatch is rejected with 400 and written to the audit log.
//  3. Credits are idempotent: `payment_callbacks` + the conditional UPDATE inside
//     db.CompleteRecharge mean a replayed callback credits at most once.
//  4. Money is computed through float64 only to mirror the TS `number` domain;
//     every toFixed boundary goes through lib.JSFixed / db's JS-exact helper.
//  5. Callback IP whitelisting, the payment-attempt suffix guard (`--PA-…`) and
//     the "order already completed -> SUCCESS" branch all stay in place; the
//     gateways retry until they see the success envelope.
package routes

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// ==================== constants ====================

const (
	// rechargeAmountToleranceCents mirrors AMOUNT_TOLERANCE_CENTS.
	rechargeAmountToleranceCents  = 1
	rechargeManualProviderType    = "manual"
	rechargeMaxTradeNoLength      = 128
	rechargeMaxAdminReasonLength  = 500
	rechargeMaxManualInstructions = 2000
	// rechargeCallbackBodyLimit mirrors the route's bodyLimit: 256 KiB.
	rechargeCallbackBodyLimit = 256 * 1024
)

var (
	rechargeSupportedProviderTypes = map[string]bool{
		"yipay": true, "heleket": true, "antom": true, rechargeManualProviderType: true,
	}
	// rechargeHeleketCallbackIPs mirrors HELEKET_CALLBACK_IPS.
	rechargeHeleketCallbackIPs = []string{"31.133.220.8"}
	rechargePositiveIDPattern  = regexp.MustCompile(`^[1-9]\d*$`)
	rechargeStatuses           = map[string]models.RechargeStatus{
		"pending": models.RechargeStatusPending, "paid": models.RechargeStatusPaid,
		"completed": models.RechargeStatusCompleted, "failed": models.RechargeStatusFailed,
		"cancelled": models.RechargeStatusCancelled, "refunded": models.RechargeStatusRefunded,
	}
	rechargePaymentAttemptSuffixPattern = regexp.MustCompile(`^(.*)--PA-([A-F0-9]{12})$`)
	rechargeProviderPaymentStatuses     = []string{"active", "disabled", "testing"}
)

// ==================== small helpers ====================

func rechargeISO(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func rechargeISOOrNil(value *time.Time) any {
	if value == nil {
		return nil
	}
	return rechargeISO(*value)
}

func rechargeStringOrNil(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func rechargeDecimalFloat(value models.Decimal) float64 {
	return value.InexactFloat64()
}

func rechargeDecimalPointer(value *models.Decimal) *float64 {
	if value == nil {
		return nil
	}
	converted := value.InexactFloat64()
	return &converted
}

func rechargeFixed2(value float64) string {
	return strconv.FormatFloat(lib.JSFixed(value, 2), 'f', 2, 64)
}

// rechargeAmountsMatch mirrors `Math.abs(paid - expected) > AMOUNT_TOLERANCE_CENTS / 100`.
func rechargeAmountsMatch(expected, actual float64) bool {
	return math.Abs(actual-expected) <= float64(rechargeAmountToleranceCents)/100
}

// rechargeAmountUnderpaid mirrors `paid + AMOUNT_TOLERANCE_CENTS / 100 < expected`.
func rechargeAmountUnderpaid(expected, actual float64) bool {
	return actual+float64(rechargeAmountToleranceCents)/100 < expected
}

// rechargeEncodeURIComponent mirrors JavaScript's encodeURIComponent.
func rechargeEncodeURIComponent(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '!' || c == '~' || c == '*' || c == '\'' || c == '(' || c == ')' {
			builder.WriteByte(c)
			continue
		}
		const hexDigits = "0123456789ABCDEF"
		builder.WriteByte('%')
		builder.WriteByte(hexDigits[c>>4])
		builder.WriteByte(hexDigits[c&0x0f])
	}
	return builder.String()
}

// rechargeParseFloat mirrors parseFloat for callback amounts.
func rechargeParseFloat(value string) (float64, bool) {
	trimmed := strings.TrimSpace(value)
	index := 0
	start := 0
	if index < len(trimmed) && (trimmed[index] == '+' || trimmed[index] == '-') {
		index++
	}
	digits := 0
	for index < len(trimmed) && trimmed[index] >= '0' && trimmed[index] <= '9' {
		index++
		digits++
	}
	if index < len(trimmed) && trimmed[index] == '.' {
		index++
		for index < len(trimmed) && trimmed[index] >= '0' && trimmed[index] <= '9' {
			index++
			digits++
		}
	}
	if digits == 0 {
		return math.NaN(), false
	}
	end := index
	if index < len(trimmed) && (trimmed[index] == 'e' || trimmed[index] == 'E') {
		exponentIndex := index + 1
		if exponentIndex < len(trimmed) && (trimmed[exponentIndex] == '+' || trimmed[exponentIndex] == '-') {
			exponentIndex++
		}
		exponentDigits := 0
		for exponentIndex < len(trimmed) && trimmed[exponentIndex] >= '0' && trimmed[exponentIndex] <= '9' {
			exponentIndex++
			exponentDigits++
		}
		if exponentDigits > 0 {
			end = exponentIndex
		}
	}
	parsed, err := strconv.ParseFloat(trimmed[start:end], 64)
	if err != nil {
		return math.NaN(), false
	}
	return parsed, true
}

// rechargeJSNumberString mirrors JS `${number}` for the messages the routes emit.
func rechargeJSNumberString(value float64) string {
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

func rechargeConfigString(config map[string]any, key string) string {
	if text, ok := config[key].(string); ok {
		return text
	}
	return ""
}

// rechargeBody decodes the JSON body as an object. Non-object bodies decode to nil
// (property access on null yields undefined in the TS, which routes treat as
// "parameter missing"). The body is restored for any later reader.
func rechargeBody(c *gin.Context) map[string]any {
	payload, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(payload))
	if len(bytes.TrimSpace(payload)) == 0 {
		return map[string]any{}
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil
	}
	return object
}

// rechargeJSONObject decodes raw JSON into an object.
func rechargeJSONObject(raw []byte) map[string]any {
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil
	}
	return object
}

// ==================== parameter parsers (mirrors the TS helpers) ====================

// parsePositiveID mirrors parsePositiveId for body/route values.
func rechargeParsePositiveID(value any) *int64 {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) || typed <= 0 || typed > 9007199254740991 {
			return nil
		}
		parsed := int64(typed)
		return &parsed
	case int:
		if typed <= 0 {
			return nil
		}
		parsed := int64(typed)
		return &parsed
	case int64:
		if typed <= 0 {
			return nil
		}
		return &typed
	case string:
		if !rechargePositiveIDPattern.MatchString(typed) {
			return nil
		}
		parsed, err := strconv.ParseInt(typed, 10, 64)
		if err != nil || parsed > 9007199254740991 {
			return nil
		}
		return &parsed
	default:
		return nil
	}
}

// rechargeParsePositiveJSONMoney mirrors parsePositiveJsonMoney: only JSON
// numbers are accepted (strings are rejected).
func rechargeParsePositiveJSONMoney(value any) *float64 {
	number, ok := value.(float64)
	if !ok {
		return nil
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 {
		return nil
	}
	return &number
}

// rechargeParseOptionalPositiveInteger mirrors parseOptionalPositiveInteger.
func rechargeParseOptionalPositiveInteger(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	parsed := rechargeParsePositiveID(value)
	if parsed == nil {
		return fallback
	}
	return int(*parsed)
}

// rechargeParseOptionalStatus mirrors parseOptionalRechargeStatus.
func rechargeParseOptionalStatus(value string) models.RechargeStatus {
	if status, ok := rechargeStatuses[value]; ok {
		return status
	}
	return ""
}

// rechargeOptionalTrimmedString mirrors normalizeOptionalTrimmedString. Null means
// the TS returned `null` (invalid input); Value nil means `undefined`.
type rechargeOptionalTrimmedString struct {
	Value *string
	Null  bool
}

func rechargeNormalizeOptionalTrimmedString(value any, maxLength int) rechargeOptionalTrimmedString {
	if value == nil {
		return rechargeOptionalTrimmedString{}
	}
	text, ok := value.(string)
	if !ok {
		return rechargeOptionalTrimmedString{Null: true}
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return rechargeOptionalTrimmedString{}
	}
	if len([]rune(trimmed)) > maxLength {
		return rechargeOptionalTrimmedString{Null: true}
	}
	return rechargeOptionalTrimmedString{Value: &trimmed}
}

// ==================== provider type helpers ====================

func rechargeIsProviderTypeSupported(providerType string) bool {
	return rechargeSupportedProviderTypes[providerType]
}

func rechargeUnsupportedProviderError(providerType string) string {
	return "支付渠道类型 " + providerType + " 当前未实现安全的充值流程，暂不支持启用"
}

func rechargeNormalizeManualInstructions(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	runes := []rune(trimmed)
	if len(runes) > rechargeMaxManualInstructions {
		runes = runes[:rechargeMaxManualInstructions]
	}
	result := string(runes)
	return &result
}

func rechargeBuildManualResponse(paymentDetails any) any {
	details := lib.ReadRechargePaymentDetails(paymentDetails)
	if details.Manual == nil {
		return nil
	}
	instructions := rechargeNormalizeManualInstructions(rechargeDerefString(details.Manual.Instructions))
	if instructions == nil {
		return nil
	}
	return gin.H{"instructions": *instructions}
}

func rechargeDerefString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// ==================== callback replay helpers ====================

// getTradeNoForIndex mirrors getTradeNoForIndex.
func rechargeTradeNoForIndex(orderNo string, tradeNo *string) string {
	if tradeNo != nil && strings.TrimSpace(*tradeNo) != "" {
		return strings.TrimSpace(*tradeNo)
	}
	return "__NO_TRADE_NO__" + orderNo
}

func rechargeNormalizeCallbackIP(ip string) string {
	value := strings.TrimSpace(ip)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "::ffff:") {
		return value[7:]
	}
	return value
}

func rechargeParseConfiguredCallbackIPs() []string {
	configured := strings.TrimSpace(os.Getenv("PAYMENT_CALLBACK_IP_WHITELIST"))
	if configured == "" {
		return nil
	}
	parts := strings.Split(configured, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if normalized := rechargeNormalizeCallbackIP(part); normalized != "" {
			result = append(result, normalized)
		}
	}
	return result
}

func rechargeProviderCallbackIPWhitelist(providerType string) []string {
	configured := rechargeParseConfiguredCallbackIPs()
	if len(configured) > 0 {
		return configured
	}
	if providerType == "heleket" {
		return rechargeHeleketCallbackIPs
	}
	return nil
}

// isIPInWhitelist mirrors isIpInWhitelist.
func rechargeIsIPInWhitelist(ip, providerType string) bool {
	if os.Getenv("PAYMENT_CALLBACK_SKIP_IP_WHITELIST") == "true" {
		return true
	}
	whitelist := rechargeProviderCallbackIPWhitelist(providerType)
	if len(whitelist) == 0 {
		return true
	}
	normalized := rechargeNormalizeCallbackIP(ip)
	for _, allowed := range whitelist {
		if allowed == normalized {
			return true
		}
	}
	return false
}

// ==================== config builders ====================

// buildEpayConfig mirrors buildEpayConfig.
func rechargeBuildEpayConfig(config map[string]any) (lib.EpayConfig, bool, string) {
	version := rechargeConfigString(config, "version")
	apiurl := rechargeConfigString(config, "apiurl")
	pid := rechargeConfigString(config, "pid")

	if version == "v1" {
		epayConfig := lib.EpayConfig{Version: lib.EpayVersionV1, APIURL: apiurl, PID: pid, Key: rechargeConfigString(config, "key")}
		if epayConfig.APIURL == "" || epayConfig.PID == "" || epayConfig.Key == "" {
			return epayConfig, false, "支付渠道配置不完整（V1版本需要接口地址、商户ID、密钥）"
		}
		return epayConfig, true, ""
	}

	epayConfig := lib.EpayConfig{
		Version:            lib.EpayVersionV2,
		APIURL:             apiurl,
		PID:                pid,
		PlatformPublicKey:  rechargeConfigString(config, "platform_public_key"),
		MerchantPrivateKey: rechargeConfigString(config, "merchant_private_key"),
	}
	if epayConfig.APIURL == "" || epayConfig.PID == "" || epayConfig.PlatformPublicKey == "" || epayConfig.MerchantPrivateKey == "" {
		return epayConfig, false, "支付渠道配置不完整（V2版本需要接口地址、商户ID、平台公钥、商户私钥）"
	}
	return epayConfig, true, ""
}

func rechargeGetProviderMethods(methods []string) []string {
	if methods == nil {
		return []string{}
	}
	result := make([]string, 0, len(methods))
	for _, method := range methods {
		if strings.TrimSpace(method) != "" {
			result = append(result, method)
		}
	}
	return result
}

// resolveRechargePaymentMethod mirrors resolveRechargePaymentMethod.
func rechargeResolvePaymentMethod(providerType string, methods []string, requested *string) *string {
	normalized := ""
	if requested != nil {
		normalized = strings.TrimSpace(*requested)
	}

	if providerType == "yipay" {
		available := methods
		if len(available) == 0 {
			available = []string{"alipay"}
		}
		if normalized != "" {
			for _, method := range available {
				if method == normalized {
					return &normalized
				}
			}
		}
		first := available[0]
		return &first
	}

	if normalized == "" {
		return nil
	}
	return &normalized
}

func rechargeNormalizePublicBaseURL(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

func rechargeIsLocalOrPrivateHostname(hostname string) bool {
	normalized := strings.ToLower(strings.TrimSpace(hostname))
	normalized = strings.Trim(normalized, "[]")
	normalized = strings.TrimSuffix(normalized, ".")

	if normalized == "" || normalized == "localhost" ||
		strings.HasSuffix(normalized, ".localhost") ||
		strings.HasSuffix(normalized, ".local") ||
		strings.HasSuffix(normalized, ".internal") {
		return true
	}
	if net.ParseIP(normalized) != nil && lib.IsIPPrivateOrReserved(normalized) {
		return true
	}
	return !strings.Contains(normalized, ".")
}

// validatePublicBaseURL mirrors validatePublicBaseUrl.
func rechargeValidatePublicBaseURL(value, configName string) (string, error) {
	normalized := rechargeNormalizePublicBaseURL(value)
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Scheme == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("%s must be a valid http(s) URL", configName)
	}
	if lib.IsProductionEnv() {
		if parsed.Scheme != "https" {
			return "", fmt.Errorf("%s must use HTTPS in production", configName)
		}
		if rechargeIsLocalOrPrivateHostname(parsed.Hostname()) {
			return "", fmt.Errorf("%s must use a public frontend hostname in production", configName)
		}
	}
	return normalized, nil
}

// getRechargeFrontendURL mirrors getRechargeFrontendUrl.
func rechargeFrontendURL() (string, error) {
	configured := ""
	if raw := os.Getenv("FRONTEND_URL"); raw != "" {
		if first := strings.Split(raw, ",")[0]; first != "" {
			configured = strings.TrimSpace(first)
		}
	}
	frontendURL := configured
	if frontendURL == "" && !lib.IsProductionEnv() {
		frontendURL = "http://localhost:3000"
	}
	if frontendURL == "" {
		return "", errors.New("FRONTEND_URL must be configured in production before creating recharge orders")
	}
	return rechargeValidatePublicBaseURL(frontendURL, "FRONTEND_URL")
}

// getRechargeCallbackBaseURL mirrors getRechargeCallbackBaseUrl.
func rechargeCallbackBaseURL() (string, error) {
	if callbackURL := strings.TrimSpace(os.Getenv("PAYMENT_CALLBACK_BASE_URL")); callbackURL != "" {
		return rechargeValidatePublicBaseURL(callbackURL, "PAYMENT_CALLBACK_BASE_URL")
	}
	return rechargeFrontendURL()
}

// getRechargeOrderExpiryAt mirrors getRechargeOrderExpiryAt.
func rechargeOrderExpiryAt(providerType string, config map[string]any) time.Time {
	now := time.Now().UTC()
	if providerType == "heleket" {
		heleketConfig, _, _ := lib.BuildHeleketConfig(config)
		return now.Add(time.Duration(heleketConfig.LifetimeSeconds) * time.Second)
	}
	if providerType == "antom" {
		antomConfig, _, _ := lib.BuildAntomConfig(config)
		return now.Add(time.Duration(antomConfig.SessionExpiryMinutes) * time.Minute)
	}
	return now.Add(30 * time.Minute)
}

// buildRechargeUrls mirrors buildRechargeUrls.
func rechargeURLs(providerID int64, orderNo string) (notifyURL, returnURL, successURL string, err error) {
	frontendURL, err := rechargeFrontendURL()
	if err != nil {
		return "", "", "", err
	}
	callbackBaseURL, err := rechargeCallbackBaseURL()
	if err != nil {
		return "", "", "", err
	}
	return callbackBaseURL + "/api/recharge/callback/" + strconv.FormatInt(providerID, 10),
		frontendURL + "/wallet",
		frontendURL + "/wallet?recharge=success&out_trade_no=" + rechargeEncodeURIComponent(orderNo),
		nil
}

// ==================== provider validation ====================

// validateActiveRechargeProvider mirrors validateActiveRechargeProvider.
func rechargeValidateActiveProvider(providerType string, config map[string]any) (bool, string) {
	if !rechargeIsProviderTypeSupported(providerType) {
		return false, rechargeUnsupportedProviderError(providerType)
	}
	switch providerType {
	case "yipay":
		_, valid, message := rechargeBuildEpayConfig(config)
		return valid, message
	case "heleket":
		_, valid, message := lib.BuildHeleketConfig(config)
		return valid, message
	case "antom":
		_, valid, message := lib.BuildAntomConfig(config)
		return valid, message
	case rechargeManualProviderType:
		if rechargeNormalizeManualInstructions(config["instructions"]) != nil {
			return true, ""
		}
		return false, "人工充值渠道必须填写付款说明"
	default:
		return false, rechargeUnsupportedProviderError(providerType)
	}
}

// validateYipayMethodFees mirrors validateYipayMethodFees.
func rechargeValidateYipayMethodFees(config map[string]any) (bool, string) {
	rawMethodFees, exists := config["methodFees"]
	if !exists || rawMethodFees == nil {
		return true, ""
	}
	methodFees, ok := rawMethodFees.(map[string]any)
	if !ok {
		return false, "支付方式手续费配置不合法"
	}
	for method, entry := range methodFees {
		if strings.TrimSpace(method) == "" {
			return false, "支付方式手续费配置不合法"
		}
		feeConfig, ok := entry.(map[string]any)
		if !ok {
			return false, "支付方式手续费配置不合法"
		}
		feeRate := float64(0)
		if raw, exists := feeConfig["feeRate"]; exists && raw != nil {
			feeRate = rechargeAnyNumber(raw)
		}
		feeFixed := float64(0)
		if raw, exists := feeConfig["feeFixed"]; exists && raw != nil {
			feeFixed = rechargeAnyNumber(raw)
		}
		if math.IsNaN(feeRate) || math.IsInf(feeRate, 0) || feeRate < 0 || feeRate > 1 {
			return false, "支付方式手续费费率需在 0% 到 100% 之间"
		}
		if math.IsNaN(feeFixed) || math.IsInf(feeFixed, 0) || feeFixed < 0 {
			return false, "支付方式固定手续费不能为负数"
		}
	}
	return true, ""
}

// rechargeAnyNumber mirrors Number(value) for the admin body values.
func rechargeAnyNumber(value any) float64 {
	switch typed := value.(type) {
	case nil:
		return 0
	case float64:
		return typed
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return math.NaN()
		}
		return parsed
	case bool:
		if typed {
			return 1
		}
		return 0
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return math.NaN()
		}
		return parsed
	default:
		return math.NaN()
	}
}

// validatePaymentProviderApiUrl mirrors validatePaymentProviderApiUrl.
func rechargeValidateProviderAPIURL(providerType string, config map[string]any) (bool, string, map[string]any) {
	if providerType != "yipay" && providerType != "heleket" {
		return true, "", config
	}
	apiurl, ok := config["apiurl"].(string)
	if !ok || strings.TrimSpace(apiurl) == "" {
		return true, "", config
	}
	label := "Epay API URL"
	if providerType == "heleket" {
		label = "Heleket API URL"
	}
	safeURL, err := lib.AssertSafeHTTPURL(context.Background(), apiurl, label)
	if err != nil {
		return false, err.Error(), config
	}
	normalized := strings.TrimRight(safeURL.String(), "/")
	next := map[string]any{}
	for key, value := range config {
		next[key] = value
	}
	next["apiurl"] = normalized
	return true, "", next
}

// validatePaymentProviderAdminInput mirrors validatePaymentProviderAdminInput.
func rechargeValidateProviderAdminInput(providerType string, config map[string]any, status string) (bool, string, map[string]any) {
	valid, message, normalized := rechargeValidateProviderAPIURL(providerType, config)
	if !valid {
		return false, message, normalized
	}

	if providerType == "yipay" {
		if methodValid, methodMessage := rechargeValidateYipayMethodFees(normalized); !methodValid {
			return false, methodMessage, normalized
		}
		if _, epayValid, epayMessage := rechargeBuildEpayConfig(normalized); !epayValid {
			return false, epayMessage, normalized
		}
	}
	if providerType == "heleket" {
		if _, valid, message := lib.BuildHeleketConfig(normalized); !valid {
			return false, message, normalized
		}
	}
	if providerType == "antom" {
		antomConfig, valid, message := lib.BuildAntomConfig(normalized)
		if !valid {
			return false, message, normalized
		}
		normalized["environment"] = string(antomConfig.Environment)
		normalized["apiurl"] = antomConfig.APIURL
		normalized["clientId"] = antomConfig.ClientID
		normalized["antom_public_key"] = antomConfig.AntomPublicKey
		normalized["merchant_private_key"] = antomConfig.MerchantPrivateKey
		normalized["keyVersion"] = antomConfig.KeyVersion
		normalized["currency"] = antomConfig.Currency
		normalized["currencyRate"] = antomConfig.CurrencyRate
		normalized["currencyExponent"] = antomConfig.CurrencyExponent
		normalized["merchantRegion"] = antomConfig.MerchantRegion
		normalized["settlementCurrency"] = antomConfig.SettlementCurrency
		normalized["merchantAccountId"] = antomConfig.MerchantAccountID
		normalized["locale"] = antomConfig.Locale
		normalized["orderDescription"] = antomConfig.OrderDescription
		normalized["sessionExpiryMinutes"] = antomConfig.SessionExpiryMinutes
	}
	if providerType == rechargeManualProviderType {
		instructions := rechargeNormalizeManualInstructions(normalized["instructions"])
		if instructions == nil {
			return false, "人工充值渠道必须填写付款说明", normalized
		}
		normalized["instructions"] = *instructions
	}

	if status == "active" {
		valid, message := rechargeValidateActiveProvider(providerType, normalized)
		return valid, message, normalized
	}
	return true, "", normalized
}

// verifyCallbackSignature mirrors verifyCallbackSignature (the fallback branch for
// provider types without a dedicated SDK).
func rechargeVerifyCallbackSignature(providerType string, config map[string]any, data map[string]any, signature string) bool {
	switch providerType {
	case "alipay_direct", "stripe":
		return false
	case "wechat_direct":
		signKey := rechargeConfigString(config, "signKey")
		if signKey == "" {
			return false
		}
		keys := make([]string, 0, len(data))
		for key := range data {
			if key != "sign" {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys)+1)
		for _, key := range keys {
			parts = append(parts, key+"="+rechargeJSValueString(data[key]))
		}
		signStr := strings.Join(parts, "&") + "&key=" + signKey
		sum := md5.Sum([]byte(signStr))
		return strings.ToUpper(hex.EncodeToString(sum[:])) == strings.ToUpper(signature)
	default:
		signKey := rechargeConfigString(config, "signKey")
		if signKey == "" {
			return false
		}
		keys := make([]string, 0, len(data))
		for key := range data {
			if key != "sign" {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+"="+rechargeJSValueString(data[key]))
		}
		signStr := strings.Join(parts, "&") + signKey
		sum := md5.Sum([]byte(signStr))
		computed := hex.EncodeToString(sum[:])
		return strings.ToLower(computed) == strings.ToLower(signature)
	}
}

// rechargeJSValueString mirrors JS string interpolation of a JSON value.
func rechargeJSValueString(value any) string {
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
		return rechargeJSNumberString(typed)
	case json.Number:
		return typed.String()
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, rechargeJSValueString(item))
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(value)
	}
}

// ==================== payment attempt state ====================

func rechargeResolveOrderNoFromGatewayOrderNo(gatewayOrderNo string) string {
	if match := rechargePaymentAttemptSuffixPattern.FindStringSubmatch(gatewayOrderNo); match != nil {
		return match[1]
	}
	return gatewayOrderNo
}

// rechargeReadPaymentAttemptState mirrors readRechargePaymentAttemptState.
func rechargeReadPaymentAttemptState(paymentDetails any) *lib.RechargePaymentAttemptState {
	details := lib.ReadRechargePaymentDetails(paymentDetails)
	if details.PaymentAttempt == nil {
		return nil
	}
	attempt := details.PaymentAttempt
	if attempt.Current.ID == "" || attempt.Current.GatewayOrderNo == "" || attempt.Current.CreatedAt == "" {
		return nil
	}
	superseded := make([]lib.RechargeSupersededPaymentAttempt, 0, len(attempt.Superseded))
	for _, item := range attempt.Superseded {
		if item.ID == "" || item.GatewayOrderNo == "" || item.CreatedAt == "" || item.SupersededAt == "" {
			continue
		}
		superseded = append(superseded, item)
	}
	return &lib.RechargePaymentAttemptState{Current: attempt.Current, Superseded: superseded}
}

// rechargeCurrentGatewayOrderNo mirrors getCurrentRechargeGatewayOrderNo.
func rechargeCurrentGatewayOrderNo(paymentDetails any, orderNo string) string {
	state := rechargeReadPaymentAttemptState(paymentDetails)
	if state == nil {
		return orderNo
	}
	current := state.Current.GatewayOrderNo
	if current != "" && rechargeResolveOrderNoFromGatewayOrderNo(current) == orderNo {
		return current
	}
	return orderNo
}

// rechargeIsCurrentPaymentAttempt mirrors isCurrentRechargePaymentAttempt.
func rechargeIsCurrentPaymentAttempt(paymentDetails any, orderNo, gatewayOrderNo string) bool {
	return rechargeCurrentGatewayOrderNo(paymentDetails, orderNo) == gatewayOrderNo
}

// rechargeBuildNextPaymentAttempt mirrors buildNextRechargePaymentAttempt.
func rechargeBuildNextPaymentAttempt(paymentDetails any, orderNo string) lib.RechargePaymentAttemptState {
	now := rechargeISO(time.Now())
	existing := rechargeReadPaymentAttemptState(paymentDetails)
	previous := lib.RechargePaymentAttempt{ID: "initial", GatewayOrderNo: orderNo, CreatedAt: now}
	if existing != nil {
		previous = existing.Current
	}
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		copy(buffer, []byte(strconv.FormatInt(time.Now().UnixNano(), 16)))
	}
	id := strings.ToUpper(hex.EncodeToString(buffer))

	superseded := make([]lib.RechargeSupersededPaymentAttempt, 0)
	if existing != nil {
		superseded = append(superseded, existing.Superseded...)
	}
	superseded = append(superseded, lib.RechargeSupersededPaymentAttempt{
		RechargePaymentAttempt: previous,
		SupersededAt:           now,
	})
	return lib.RechargePaymentAttemptState{
		Current: lib.RechargePaymentAttempt{
			ID:             id,
			GatewayOrderNo: orderNo + "--PA-" + id,
			CreatedAt:      now,
		},
		Superseded: superseded,
	}
}

func rechargeMergePaymentAttempt(paymentDetails any, attempt lib.RechargePaymentAttemptState) lib.RechargePaymentDetails {
	details := lib.ReadRechargePaymentDetails(paymentDetails)
	copied := attempt
	details.PaymentAttempt = &copied
	return details
}

// ==================== record view ====================

// rechargeRecordViewInput mirrors the argument object of buildRechargeRecordView.
type rechargeRecordViewInput struct {
	ID             int32
	OrderNo        string
	Amount         float64
	ActualAmount   *float64
	Fee            *float64
	Status         string
	Provider       *db.RechargeProviderRef
	PaymentMethod  *string
	PaymentDetails any
	TradeNo        *string
	FailReason     *string
	CreatedAt      time.Time
	ExpiredAt      *time.Time
	CompletedAt    *time.Time
}

// buildRechargeRecordView mirrors buildRechargeRecordView.
func buildRechargeRecordView(record rechargeRecordViewInput) gin.H {
	paymentInfo := lib.ExtractRechargePaymentDisplayInfo(record.PaymentDetails)
	details := lib.ReadRechargePaymentDetails(record.PaymentDetails)

	fee := 0.0
	if record.Fee != nil {
		fee = *record.Fee
	}
	payableAmount := record.Amount
	if details.Recharge != nil && details.Recharge.PayableAmount != nil && details.Recharge.PayableAmount.Valid {
		payableAmount = details.Recharge.PayableAmount.Value
	}

	var actualAmount any
	if record.ActualAmount != nil {
		actualAmount = *record.ActualAmount
	}

	var provider any
	if record.Provider != nil {
		provider = gin.H{"id": record.Provider.ID, "name": record.Provider.Name, "type": record.Provider.Type}
	}

	return gin.H{
		"id":                       record.ID,
		"orderNo":                  record.OrderNo,
		"amount":                   record.Amount,
		"payableAmount":            payableAmount,
		"actualAmount":             actualAmount,
		"fee":                      fee,
		"status":                   record.Status,
		"provider":                 provider,
		"paymentMethod":            rechargeStringOrNil(record.PaymentMethod),
		"actualPaymentMethod":      rechargeDerefString(paymentInfo.ActualPaymentMethod),
		"paymentCurrency":          rechargeDerefString(paymentInfo.PaymentCurrency),
		"paymentNetwork":           rechargeDerefString(paymentInfo.PaymentNetwork),
		"paymentUuid":              rechargeDerefString(paymentInfo.PaymentUUID),
		"paymentTxid":              rechargeDerefString(paymentInfo.PaymentTxID),
		"invoiceCurrency":          rechargeDerefString(paymentInfo.InvoiceCurrency),
		"gatewayStatus":            rechargeDerefString(paymentInfo.GatewayStatus),
		"gatewayStatusDescription": rechargeDerefString(paymentInfo.GatewayStatusDescription),
		"tradeNo":                  rechargeStringOrNil(record.TradeNo),
		"failReason":               rechargeStringOrNil(record.FailReason),
		"createdAt":                rechargeISO(record.CreatedAt),
		"expiredAt":                rechargeISOOrNil(record.ExpiredAt),
		"completedAt":              rechargeISOOrNil(record.CompletedAt),
	}
}

// rechargeViewFromRecord builds the view input from a DB record.
func rechargeViewFromRecord(record *db.RechargeRecordWithProvider) rechargeRecordViewInput {
	return rechargeRecordViewInput{
		ID:             record.ID,
		OrderNo:        record.OrderNo,
		Amount:         rechargeDecimalFloat(record.Amount),
		ActualAmount:   rechargeDecimalPointer(record.ActualAmount),
		Fee:            rechargeDecimalPointer(&record.Fee),
		Status:         string(record.Status),
		Provider:       record.Provider,
		PaymentMethod:  record.PaymentMethod,
		PaymentDetails: record.PaymentDetails,
		TradeNo:        record.TradeNo,
		FailReason:     record.FailReason,
		CreatedAt:      record.CreatedAt,
		ExpiredAt:      record.ExpiredAt,
		CompletedAt:    record.CompletedAt,
	}
}

// ==================== callback order extraction ====================

// extractRechargeOrderNoFromCallback mirrors extractRechargeOrderNoFromCallback.
func rechargeExtractOrderNoFromCallback(providerType string, callbackData map[string]any) string {
	switch providerType {
	case "yipay", "alipay_direct", "wechat_direct":
		if text, ok := callbackData["out_trade_no"].(string); ok {
			return text
		}
		return ""
	case "heleket":
		if text, ok := callbackData["order_id"].(string); ok {
			return text
		}
		return ""
	case "antom":
		if text, ok := callbackData["paymentRequestId"].(string); ok {
			return text
		}
		return ""
	case "stripe":
		data, _ := callbackData["data"].(map[string]any)
		object, _ := data["object"].(map[string]any)
		metadata, _ := object["metadata"].(map[string]any)
		if text, ok := metadata["orderNo"].(string); ok {
			return text
		}
		return ""
	default:
		if text, ok := callbackData["orderNo"].(string); ok && text != "" {
			return text
		}
		if text, ok := callbackData["order_no"].(string); ok && text != "" {
			return text
		}
		if text, ok := callbackData["out_trade_no"].(string); ok {
			return text
		}
		return ""
	}
}

// ==================== pay link creation ====================

// createRechargePayUrl mirrors createRechargePayUrl.
func rechargeCreatePayURL(
	ctx context.Context,
	provider *db.PaymentProviderRecord,
	config map[string]any,
	orderNo string,
	amount float64,
	paymentMethod *string,
	notifyURL, successURL, returnURL string,
) (*string, error) {
	switch string(provider.Type) {
	case "yipay":
		epayConfig, valid, message := rechargeBuildEpayConfig(config)
		if !valid {
			if message == "" {
				message = "支付渠道配置不完整"
			}
			return nil, errors.New(message)
		}
		safeURL, err := lib.AssertSafeHTTPURL(ctx, epayConfig.APIURL, "Epay API URL")
		if err != nil {
			return nil, err
		}
		epayConfig.APIURL = strings.TrimRight(safeURL.String(), "/")
		client := lib.CreateEpayClient(epayConfig)
		method := "alipay"
		if paymentMethod != nil && *paymentMethod != "" {
			method = *paymentMethod
		}
		link := client.GetPayLink(lib.EpayPayParams{
			Type:       method,
			OutTradeNo: orderNo,
			Name:       "账户充值",
			Money:      rechargeFixed2(amount),
			NotifyURL:  notifyURL,
			ReturnURL:  successURL,
		})
		return &link, nil

	case "heleket":
		heleketConfig, valid, message := lib.BuildHeleketConfig(config)
		if !valid {
			if message == "" {
				message = "支付渠道配置不完整"
			}
			return nil, errors.New(message)
		}
		client := lib.CreateHeleketClient(heleketConfig)
		lifetime := heleketConfig.LifetimeSeconds
		invoice, err := client.CreateInvoice(ctx, lib.HeleketInvoiceRequest{
			Amount:      rechargeFixed2(amount),
			Currency:    heleketConfig.Currency,
			OrderID:     orderNo,
			Lifetime:    &lifetime,
			URLReturn:   returnURL,
			URLSuccess:  successURL,
			URLCallback: notifyURL,
		})
		if err != nil {
			return nil, err
		}
		if invoiceURL, ok := invoice["url"].(string); ok && strings.TrimSpace(invoiceURL) != "" {
			return &invoiceURL, nil
		}
		return nil, nil

	case "antom":
		antomConfig, valid, message := lib.BuildAntomConfig(config)
		if !valid {
			if message == "" {
				message = "Antom 支付渠道配置不完整"
			}
			return nil, errors.New(message)
		}
		client := lib.NewAntomClient(antomConfig)
		session, err := client.CreatePaymentSession(ctx, orderNo, amount, notifyURL, successURL)
		if err != nil {
			return nil, err
		}
		if session.Result == nil || session.Result.ResultStatus != "S" || session.Result.ResultCode != "SUCCESS" {
			message := "Antom 创建支付会话失败"
			if session.Result != nil && session.Result.ResultMessage != "" {
				message = session.Result.ResultMessage
			}
			return nil, errors.New(message)
		}
		if strings.TrimSpace(session.NormalURL) == "" {
			return nil, errors.New("Antom 未返回 Hosted Checkout 地址")
		}
		normalURL, err := lib.AssertSafeHTTPURL(ctx, session.NormalURL, "Antom Hosted Checkout URL")
		if err != nil {
			return nil, err
		}
		if normalURL.Scheme != "https" {
			return nil, errors.New("Antom Hosted Checkout 地址必须使用 HTTPS")
		}
		value := normalURL.String()
		return &value, nil
	}
	return nil, nil
}

// ==================== rate limiting ====================

type rechargeLimiterBucket struct {
	count   int
	resetAt time.Time
}

var rechargeLimiter = struct {
	mu      sync.Mutex
	buckets map[string]rechargeLimiterBucket
}{buckets: map[string]rechargeLimiterBucket{}}

// rechargeRateLimit mirrors the route's inline `config.rateLimit` (fixed window,
// per IP + route store). Node enforces these inline limits for real; the generated
// rate-limit table has no recharge entries.
func rechargeRateLimit(route, method string, maximum int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := middleware.ClientIP(c) + ":" + method + ":" + route
		now := time.Now()
		rechargeLimiter.mu.Lock()
		bucket, ok := rechargeLimiter.buckets[key]
		if !ok || now.After(bucket.resetAt) {
			bucket = rechargeLimiterBucket{count: 0, resetAt: now.Add(window)}
		}
		bucket.count++
		allowed := bucket.count <= maximum
		rechargeLimiter.buckets[key] = bucket
		rechargeLimiter.mu.Unlock()

		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "Too many requests, please try again later",
			})
			return
		}
		c.Next()
	}
}

// ResetRechargeRateLimits clears the process-local buckets (tests only).
func ResetRechargeRateLimits() {
	rechargeLimiter.mu.Lock()
	rechargeLimiter.buckets = map[string]rechargeLimiterBucket{}
	rechargeLimiter.mu.Unlock()
}

// ==================== route registration ====================

// RegisterRecharge mounts the recharge routes with absolute paths, matching
// `fastify.register(rechargeRoutes)` (no prefix).
func RegisterRecharge(r *gin.Engine) {
	r.GET("/api/recharge/providers", middleware.Authenticate(), rechargeListProviders)
	r.POST("/api/recharge/orders",
		rechargeRateLimit("/api/recharge/orders", http.MethodPost, 10, time.Minute),
		middleware.Authenticate(), rechargeCreateOrder)
	r.GET("/api/recharge/orders", middleware.Authenticate(), rechargeListOrders)
	r.GET("/api/recharge/orders/:orderNo", middleware.Authenticate(), rechargeOrderDetail)
	r.POST("/api/recharge/orders/:orderNo/cancel",
		rechargeRateLimit("/api/recharge/orders/:orderNo/cancel", http.MethodPost, 20, time.Minute),
		middleware.Authenticate(), rechargeCancelOrder)
	r.POST("/api/recharge/orders/:orderNo/repay",
		rechargeRateLimit("/api/recharge/orders/:orderNo/repay", http.MethodPost, 10, time.Minute),
		middleware.Authenticate(), rechargeRepayOrder)
	r.GET("/api/recharge/stats", middleware.Authenticate(), rechargeUserStats)
	r.POST("/api/recharge/orders/:orderNo/verify",
		rechargeRateLimit("/api/recharge/orders/:orderNo/verify", http.MethodPost, 30, time.Minute),
		middleware.Authenticate(), rechargeVerifyOrder)

	r.GET("/api/admin/payment-providers", middleware.Authenticate(), middleware.RequireAdmin(), rechargeAdminListProviders)
	r.POST("/api/admin/payment-providers",
		rechargeRateLimit("/api/admin/payment-providers", http.MethodPost, 10, time.Minute),
		middleware.Authenticate(), middleware.RequireAdmin(), rechargeAdminCreateProvider)
	r.PUT("/api/admin/payment-providers/:id",
		rechargeRateLimit("/api/admin/payment-providers/:id", http.MethodPut, 20, time.Minute),
		middleware.Authenticate(), middleware.RequireAdmin(), rechargeAdminUpdateProvider)
	r.PATCH("/api/admin/payment-providers/:id/status",
		rechargeRateLimit("/api/admin/payment-providers/:id/status", http.MethodPatch, 20, time.Minute),
		middleware.Authenticate(), middleware.RequireAdmin(), rechargeAdminUpdateProviderStatus)
	r.DELETE("/api/admin/payment-providers/:id",
		rechargeRateLimit("/api/admin/payment-providers/:id", http.MethodDelete, 10, time.Minute),
		middleware.Authenticate(), middleware.RequireAdmin(), rechargeAdminDeleteProvider)

	r.GET("/api/admin/recharge/orders", middleware.Authenticate(), middleware.RequireAdmin(), rechargeAdminListOrders)
	r.GET("/api/admin/recharge/stats", middleware.Authenticate(), middleware.RequireAdmin(), rechargeAdminStats)
	r.POST("/api/admin/recharge/orders/:orderNo/complete",
		rechargeRateLimit("/api/admin/recharge/orders/:orderNo/complete", http.MethodPost, 10, time.Minute),
		middleware.Authenticate(), middleware.RequireAdmin(), rechargeAdminCompleteOrder)
	r.POST("/api/admin/recharge/orders/:orderNo/fail",
		rechargeRateLimit("/api/admin/recharge/orders/:orderNo/fail", http.MethodPost, 10, time.Minute),
		middleware.Authenticate(), middleware.RequireAdmin(), rechargeAdminFailOrder)

	// Payment callbacks: public, signature-verified, 100/min.
	r.POST("/api/recharge/callback/:providerId",
		rechargeRateLimit("/api/recharge/callback/:providerId", http.MethodPost, 100, time.Minute),
		rechargeCallbackPOST)
	r.GET("/api/recharge/callback/:providerId",
		rechargeRateLimit("/api/recharge/callback/:providerId", http.MethodGet, 100, time.Minute),
		rechargeCallbackGET)
}

func init() {
	Register(RegisterRecharge)
}

// ==================== user: providers ====================

func rechargeListProviders(c *gin.Context) {
	ctx := c.Request.Context()
	freeSite, err := db.GetSystemConfigBoolean(ctx, "free_site_mode", false)
	if err != nil {
		log.Printf("获取支付渠道列表失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取支付渠道列表失败"})
		return
	}
	if freeSite {
		c.JSON(http.StatusOK, gin.H{"providers": []any{}})
		return
	}

	activeProviders, err := db.GetActivePaymentProviders(ctx)
	if err != nil {
		log.Printf("获取支付渠道列表失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取支付渠道列表失败"})
		return
	}

	providers := make([]gin.H, 0, len(activeProviders))
	for _, provider := range activeProviders {
		valid, message := rechargeValidateActiveProvider(string(provider.Type), provider.Config)
		if !valid {
			log.Printf("忽略未安全启用的支付渠道 providerId=%d type=%s error=%s", provider.ID, provider.Type, message)
			continue
		}
		var maxAmount any
		if provider.MaxAmount != nil {
			maxAmount = *provider.MaxAmount
		}
		var methods any = provider.MethodsRaw
		if methods == nil {
			methods = []string{}
		}
		providers = append(providers, gin.H{
			"id":         provider.ID,
			"name":       provider.Name,
			"type":       provider.Type,
			"methods":    methods,
			"methodFees": db.GetPaymentMethodFeeMap(provider),
			"minAmount":  provider.MinAmount,
			"maxAmount":  maxAmount,
			"feeRate":    provider.FeeRate,
			"feeFixed":   provider.FeeFixed,
		})
	}
	c.JSON(http.StatusOK, gin.H{"providers": providers})
}

// ==================== user: create order ====================

func rechargeCreateOrder(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	freeSite, err := db.GetSystemConfigBoolean(ctx, "free_site_mode", false)
	if err != nil {
		log.Printf("创建充值订单失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建充值订单失败"})
		return
	}
	if freeSite {
		c.JSON(http.StatusForbidden, gin.H{"error": "白嫖站已启用，充值功能不可用"})
		return
	}

	body := rechargeBody(c)
	if body == nil {
		body = map[string]any{}
	}
	providerIDValue := body["providerId"]
	amountValue, amountPresent := body["amount"]
	providerID := rechargeParsePositiveID(providerIDValue)
	rechargeAmount := rechargeParsePositiveJSONMoney(amountValue)

	if providerID == nil || !amountPresent {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数不完整"})
		return
	}
	if rechargeAmount == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "充值金额无效"})
		return
	}

	normalizedAmount := lib.JSFixed(*rechargeAmount, 2)
	if normalizedAmount <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "充值金额无效"})
		return
	}

	provider, err := rechargeLoadProvider(ctx, *providerID)
	if err != nil {
		log.Printf("创建充值订单失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建充值订单失败"})
		return
	}
	if provider == nil || provider.Status != models.PaymentProviderStatusActive {
		c.JSON(http.StatusBadRequest, gin.H{"error": "支付渠道不可用"})
		return
	}

	valid, validationMessage := rechargeValidateActiveProvider(string(provider.Type), provider.Config)
	if !valid {
		log.Printf("拒绝使用未安全实现的支付渠道创建订单 providerId=%d type=%s error=%s", provider.ID, provider.Type, validationMessage)
		if validationMessage == "" {
			validationMessage = "支付渠道不可用"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": validationMessage})
		return
	}

	if amountValid, amountMessage := db.ValidateRechargeAmount(*provider, normalizedAmount); !amountValid {
		c.JSON(http.StatusBadRequest, gin.H{"error": amountMessage})
		return
	}

	var requestedMethod *string
	if text, ok := body["paymentMethod"].(string); ok {
		requestedMethod = &text
	}
	selectedPaymentMethod := rechargeResolvePaymentMethod(string(provider.Type), rechargeGetProviderMethods(provider.Methods), requestedMethod)
	feeConfig := db.GetPaymentFeeConfig(*provider, selectedPaymentMethod)
	fee := db.CalculatePaymentFee(*provider, normalizedAmount, selectedPaymentMethod)
	payableAmount := db.CalculatePayableAmount(*provider, normalizedAmount, selectedPaymentMethod)
	actualAmount := db.CalculateActualAmount(*provider, normalizedAmount)
	if math.IsNaN(actualAmount) || math.IsInf(actualAmount, 0) || actualAmount <= 0 {
		log.Printf("拒绝创建实际到账金额无效的充值订单 providerId=%d amount=%v fee=%v actualAmount=%v",
			*providerID, normalizedAmount, fee, actualAmount)
		c.JSON(http.StatusBadRequest, gin.H{"error": "充值金额扣除手续费后必须大于 0 元"})
		return
	}

	expiredAt := rechargeOrderExpiryAt(string(provider.Type), provider.Config)
	orderNo := db.GenerateOrderNo()
	providerConfigSnapshot := lib.BuildRechargeProviderConfigSnapshot(string(provider.Type), provider.Config)

	var providerPaymentDetails lib.RechargePaymentDetails
	var detailsErr error
	switch string(provider.Type) {
	case "heleket":
		heleketConfig, _, _ := lib.BuildHeleketConfig(provider.Config)
		providerPaymentDetails = lib.BuildHeleketInvoicePaymentDetails(orderNo, payableAmount, heleketConfig)
	case "antom":
		antomConfig, _, _ := lib.BuildAntomConfig(provider.Config)
		providerPaymentDetails, detailsErr = lib.BuildAntomPaymentDetails(orderNo, payableAmount, antomConfig)
	case rechargeManualProviderType:
		instructions := rechargeNormalizeManualInstructions(provider.Config["instructions"])
		text := ""
		if instructions != nil {
			text = *instructions
		}
		providerPaymentDetails = lib.BuildManualRechargePaymentDetails(text)
	default:
		kind := string(provider.Type)
		providerPaymentDetails = lib.RechargePaymentDetails{Kind: &kind}
	}
	if detailsErr != nil {
		log.Printf("创建充值订单失败: %v", detailsErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建充值订单失败"})
		return
	}

	feeMode := "deduct"
	if provider.Type == models.PaymentProviderTypeYipay {
		feeMode = "surcharge"
	}
	paymentDetails := lib.MergeRechargeAmountDetails(providerPaymentDetails, lib.RechargeAmountMergeInput{
		Amount:        normalizedAmount,
		PayableAmount: payableAmount,
		Fee:           fee,
		FeeRate:       feeConfig.FeeRate,
		FeeFixed:      feeConfig.FeeFixed,
		FeeMode:       &feeMode,
		PaymentMethod: selectedPaymentMethod,
	})

	var ip *string
	if clientIP := middleware.ClientIP(c); clientIP != "" {
		ip = &clientIP
	}
	var userAgent *string
	if values := c.Request.Header.Values("User-Agent"); len(values) > 0 {
		joined := strings.Join(values, ", ")
		userAgent = &joined
	}

	order, err := db.CreateRechargeOrder(ctx, db.CreateRechargeOrderInput{
		OrderNo:                orderNo,
		UserID:                 user.ID,
		ProviderID:             int32(provider.ID),
		Amount:                 normalizedAmount,
		ActualAmount:           &actualAmount,
		Fee:                    fee,
		PaymentMethod:          selectedPaymentMethod,
		IP:                     ip,
		UserAgent:              userAgent,
		ExpiredAt:              &expiredAt,
		ProviderConfigSnapshot: &providerConfigSnapshot,
		PaymentDetails:         paymentDetails,
	})
	if err != nil {
		log.Printf("创建充值订单失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建充值订单失败"})
		return
	}

	notifyURL, returnURL, successURL, err := rechargeURLs(*providerID, order.OrderNo)
	if err != nil {
		log.Printf("创建充值订单失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建充值订单失败"})
		return
	}

	payURL, payErr := rechargeCreatePayURL(ctx, provider, provider.Config, order.OrderNo, payableAmount, selectedPaymentMethod, notifyURL, successURL, returnURL)
	if payErr != nil {
		// The order row stays behind (the TS does the same before throwing).
		log.Printf("生成支付链接失败 providerId=%d orderNo=%s error=%v", *providerID, order.OrderNo, payErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建充值订单失败"})
		return
	}

	response := gin.H{
		"order": gin.H{
			"orderNo":       order.OrderNo,
			"amount":        rechargeDecimalFloat(order.Amount),
			"payableAmount": payableAmount,
			"actualAmount":  rechargeDecimalFloatPtr(order.ActualAmount),
			"fee":           rechargeDecimalFloat(order.Fee),
			"status":        order.Status,
			"expiredAt":     rechargeISOOrNil(order.ExpiredAt),
			"createdAt":     rechargeISO(order.CreatedAt),
		},
		"provider": gin.H{
			"id":      provider.ID,
			"name":    provider.Name,
			"type":    provider.Type,
			"methods": provider.Methods,
		},
		"payUrl": payURL,
	}
	if provider.Type == models.PaymentProviderTypeManual {
		response["manualPayment"] = rechargeBuildManualResponse(paymentDetails)
	} else {
		response["manualPayment"] = nil
	}
	c.JSON(http.StatusOK, response)
}

func rechargeDecimalFloatPtr(value *models.Decimal) any {
	if value == nil {
		return nil
	}
	return value.InexactFloat64()
}

// rechargeLoadProvider resolves a provider id (int64 from the request) to a record.
func rechargeLoadProvider(ctx context.Context, id int64) (*db.PaymentProviderRecord, error) {
	if id <= 0 || id > math.MaxInt32 {
		return nil, nil
	}
	return db.GetPaymentProviderByID(ctx, int32(id))
}

// ==================== user: list / detail ====================

func rechargeListOrders(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	freeSite, err := db.GetSystemConfigBoolean(ctx, "free_site_mode", false)
	if err != nil {
		log.Printf("获取充值记录失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取充值记录失败"})
		return
	}
	if freeSite {
		c.JSON(http.StatusForbidden, gin.H{"error": "白嫖站已启用，充值功能不可用"})
		return
	}

	page := rechargeParseOptionalPositiveInteger(c.Query("page"), 1)
	pageSize := rechargeParseOptionalPositiveInteger(c.Query("pageSize"), 20)
	status := rechargeParseOptionalStatus(c.Query("status"))
	result, err := db.GetUserRechargeRecords(ctx, user.ID, db.RechargeRecordListOptions{
		Page: page, PageSize: pageSize, Status: status,
	})
	if err != nil {
		log.Printf("获取充值记录失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取充值记录失败"})
		return
	}

	records := make([]gin.H, 0, len(result.Records))
	for index := range result.Records {
		records = append(records, buildRechargeRecordView(rechargeViewFromRecord(&result.Records[index])))
	}
	c.JSON(http.StatusOK, gin.H{
		"records":  records,
		"total":    result.Total,
		"page":     result.Page,
		"pageSize": result.PageSize,
	})
}

func rechargeOrderDetail(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	freeSite, err := db.GetSystemConfigBoolean(ctx, "free_site_mode", false)
	if err != nil {
		log.Printf("获取订单详情失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取订单详情失败"})
		return
	}
	if freeSite {
		c.JSON(http.StatusForbidden, gin.H{"error": "白嫖站已启用，充值功能不可用"})
		return
	}

	orderNo := c.Param("orderNo")
	record, err := db.GetRechargeRecordByOrderNo(ctx, orderNo)
	if err != nil {
		log.Printf("获取订单详情失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取订单详情失败"})
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "订单不存在"})
		return
	}
	if record.UserID != user.ID {
		c.JSON(http.StatusForbidden, gin.H{"error": "无权查看此订单"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"order": buildRechargeRecordView(rechargeViewFromRecord(record))})
}

// ==================== user: cancel ====================

func rechargeCancelOrder(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	orderNo := c.Param("orderNo")
	record, err := db.GetRechargeRecordByOrderNo(ctx, orderNo)
	if err != nil {
		log.Printf("取消订单失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "取消订单失败"})
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "订单不存在"})
		return
	}
	if record.UserID != user.ID {
		c.JSON(http.StatusForbidden, gin.H{"error": "无权操作此订单"})
		return
	}
	if record.Status != models.RechargeStatusPending {
		c.JSON(http.StatusBadRequest, gin.H{"error": "当前订单状态不允许取消"})
		return
	}
	if _, err := db.CancelRecharge(ctx, orderNo, nil, false, nil, false); err != nil {
		log.Printf("取消订单失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "取消订单失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "订单已取消"})
}

// ==================== user: repay ====================

func rechargeRepayOrder(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	freeSite, err := db.GetSystemConfigBoolean(ctx, "free_site_mode", false)
	if err != nil {
		log.Printf("重新支付失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "重新支付失败"})
		return
	}
	if freeSite {
		c.JSON(http.StatusForbidden, gin.H{"error": "白嫖站已启用，充值功能不可用"})
		return
	}

	orderNo := c.Param("orderNo")
	body := rechargeBody(c)
	if body == nil {
		body = map[string]any{}
	}
	var requestedMethod *string
	if text, ok := body["paymentMethod"].(string); ok {
		requestedMethod = &text
	}

	record, err := db.GetRechargeRecordByOrderNo(ctx, orderNo)
	if err != nil {
		log.Printf("重新支付失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "重新支付失败"})
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "订单不存在"})
		return
	}
	if record.UserID != user.ID {
		c.JSON(http.StatusForbidden, gin.H{"error": "无权操作此订单"})
		return
	}
	if record.Status != models.RechargeStatusPending {
		c.JSON(http.StatusBadRequest, gin.H{"error": "当前订单状态不允许支付"})
		return
	}
	if record.ExpiredAt != nil && record.ExpiredAt.Before(time.Now()) {
		if _, err := db.CancelRecharge(ctx, orderNo, nil, false, nil, false); err != nil {
			log.Printf("重新支付失败: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "重新支付失败"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "订单已过期，请重新创建"})
		return
	}

	provider, err := rechargeLoadProvider(ctx, int64(record.ProviderID))
	if err != nil {
		log.Printf("重新支付失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "重新支付失败"})
		return
	}
	if provider == nil || provider.Status != models.PaymentProviderStatusActive {
		c.JSON(http.StatusBadRequest, gin.H{"error": "支付渠道不可用"})
		return
	}

	effectiveConfig, _ := lib.ResolveRechargeProviderConfigSnapshot(string(provider.Type), provider.Config, record.ProviderConfigSnapshot)
	valid, validationMessage := rechargeValidateActiveProvider(string(provider.Type), effectiveConfig)
	if !valid {
		log.Printf("拒绝使用未安全实现的支付渠道重新支付 orderNo=%s type=%s error=%s", orderNo, provider.Type, validationMessage)
		if validationMessage == "" {
			validationMessage = "支付渠道不可用"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": validationMessage})
		return
	}

	methodSource := requestedMethod
	if methodSource == nil {
		methodSource = record.PaymentMethod
	}
	selectedPaymentMethod := rechargeResolvePaymentMethod(string(provider.Type), rechargeGetProviderMethods(provider.Methods), methodSource)
	rechargeAmount := rechargeDecimalFloat(record.Amount)

	paymentMethodChanged := true
	if selectedPaymentMethod == nil {
		paymentMethodChanged = record.PaymentMethod != nil
	} else {
		paymentMethodChanged = record.PaymentMethod == nil || *selectedPaymentMethod != *record.PaymentMethod
	}

	fee := rechargeDecimalFloat(record.Fee)
	payableAmount := lib.GetRechargePayableAmount(lib.RechargePayableSource{
		Amount: rechargeAmount, PaymentDetails: record.PaymentDetails,
	})
	actualAmount := rechargeAmount
	if record.ActualAmount != nil {
		actualAmount = record.ActualAmount.InexactFloat64()
	}
	var paymentDetails any = record.PaymentDetails
	existingPaymentDetails := lib.ReadRechargePaymentDetails(record.PaymentDetails)

	var paymentAttempt *lib.RechargePaymentAttemptState
	if provider.Type != models.PaymentProviderTypeManual {
		attempt := rechargeBuildNextPaymentAttempt(record.PaymentDetails, record.OrderNo)
		paymentAttempt = &attempt
	}
	gatewayOrderNo := record.OrderNo
	if paymentAttempt != nil {
		gatewayOrderNo = paymentAttempt.Current.GatewayOrderNo
	}

	if paymentMethodChanged {
		pricingProvider := *provider
		pricingProvider.Config = effectiveConfig
		if existingPaymentDetails.Recharge != nil && existingPaymentDetails.Recharge.FeeRate != nil && existingPaymentDetails.Recharge.FeeRate.Valid {
			pricingProvider.FeeRate = existingPaymentDetails.Recharge.FeeRate.Value
		}
		if existingPaymentDetails.Recharge != nil && existingPaymentDetails.Recharge.FeeFixed != nil && existingPaymentDetails.Recharge.FeeFixed.Valid {
			pricingProvider.FeeFixed = existingPaymentDetails.Recharge.FeeFixed.Value
		}
		feeConfig := db.GetPaymentFeeConfig(pricingProvider, selectedPaymentMethod)
		fee = db.CalculatePaymentFee(pricingProvider, rechargeAmount, selectedPaymentMethod)
		payableAmount = db.CalculatePayableAmount(pricingProvider, rechargeAmount, selectedPaymentMethod)
		actualAmount = db.CalculateActualAmount(pricingProvider, rechargeAmount)
		if math.IsNaN(actualAmount) || math.IsInf(actualAmount, 0) || actualAmount <= 0 {
			log.Printf("拒绝重新支付实际到账金额无效的充值订单 orderNo=%s providerId=%d amount=%v fee=%v actualAmount=%v",
				orderNo, provider.ID, rechargeAmount, fee, actualAmount)
			c.JSON(http.StatusBadRequest, gin.H{"error": "充值金额扣除手续费后必须大于 0 元"})
			return
		}

		var providerPaymentDetails lib.RechargePaymentDetails
		var detailsErr error
		switch string(provider.Type) {
		case "heleket":
			heleketConfig, _, _ := lib.BuildHeleketConfig(effectiveConfig)
			providerPaymentDetails = lib.BuildHeleketInvoicePaymentDetails(gatewayOrderNo, payableAmount, heleketConfig)
		case "antom":
			antomConfig, _, _ := lib.BuildAntomConfig(effectiveConfig)
			providerPaymentDetails, detailsErr = lib.BuildAntomPaymentDetails(gatewayOrderNo, payableAmount, antomConfig)
		case rechargeManualProviderType:
			text := ""
			if instructions := rechargeNormalizeManualInstructions(effectiveConfig["instructions"]); instructions != nil {
				text = *instructions
			}
			providerPaymentDetails = lib.BuildManualRechargePaymentDetails(text)
		default:
			providerPaymentDetails = lib.ReadRechargePaymentDetails(record.PaymentDetails)
			if providerPaymentDetails.Kind == nil {
				kind := string(provider.Type)
				providerPaymentDetails.Kind = &kind
			}
		}
		if detailsErr != nil {
			log.Printf("重新支付失败: %v", detailsErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "重新支付失败"})
			return
		}

		feeMode := "deduct"
		if provider.Type == models.PaymentProviderTypeYipay {
			feeMode = "surcharge"
		}
		paymentDetails = lib.MergeRechargeAmountDetails(providerPaymentDetails, lib.RechargeAmountMergeInput{
			Amount:        rechargeAmount,
			PayableAmount: payableAmount,
			Fee:           fee,
			FeeRate:       feeConfig.FeeRate,
			FeeFixed:      feeConfig.FeeFixed,
			FeeMode:       &feeMode,
			PaymentMethod: selectedPaymentMethod,
		})
	}

	if paymentAttempt != nil {
		paymentDetails = rechargeMergePaymentAttempt(paymentDetails, *paymentAttempt)
	}

	notifyURL, returnURL, successURL, err := rechargeURLs(int64(provider.ID), record.OrderNo)
	if err != nil {
		log.Printf("重新支付失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "重新支付失败"})
		return
	}

	payURL, payErr := rechargeCreatePayURL(ctx, provider, effectiveConfig, gatewayOrderNo, payableAmount, selectedPaymentMethod, notifyURL, successURL, returnURL)
	if payErr != nil {
		log.Printf("重新生成支付链接失败 orderNo=%s providerId=%d error=%v", orderNo, provider.ID, payErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "重新支付失败"})
		return
	}
	if payURL == nil && provider.Type != models.PaymentProviderTypeManual {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不支持的支付渠道类型"})
		return
	}

	updatedRecord, err := db.UpdatePendingRechargePaymentSelection(ctx, orderNo, db.UpdateRechargePaymentSelectionInput{
		PaymentMethod:  selectedPaymentMethod,
		Fee:            fee,
		ActualAmount:   actualAmount,
		PaymentDetails: paymentDetails,
	})
	if err != nil {
		log.Printf("重新支付失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "重新支付失败"})
		return
	}
	if updatedRecord == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "订单状态已变化，请刷新后重试"})
		return
	}

	response := gin.H{
		"order": gin.H{
			"orderNo":       record.OrderNo,
			"amount":        rechargeAmount,
			"payableAmount": payableAmount,
			"actualAmount":  actualAmount,
			"fee":           fee,
			"paymentMethod": rechargeStringOrNil(selectedPaymentMethod),
			"status":        record.Status,
			"expiredAt":     rechargeISOOrNil(record.ExpiredAt),
		},
		"payUrl":                         payURL,
		"previousPaymentLinkInvalidated": paymentAttempt != nil,
	}
	if provider.Type == models.PaymentProviderTypeManual {
		response["manualPayment"] = rechargeBuildManualResponse(paymentDetails)
	} else {
		response["manualPayment"] = nil
	}
	c.JSON(http.StatusOK, response)
}

// ==================== user: stats ====================

func rechargeUserStats(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	stats, err := db.GetUserRechargeStats(ctx, user.ID)
	if err != nil {
		log.Printf("获取充值统计失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取充值统计失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"stats": stats})
}

// ==================== user: verify ====================

func rechargeVerifyOrder(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	orderNo := c.Param("orderNo")

	record, err := db.GetRechargeRecordByOrderNo(ctx, orderNo)
	if err != nil {
		log.Printf("验证订单状态失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "验证订单状态失败"})
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "订单不存在"})
		return
	}
	if record.UserID != user.ID {
		c.JSON(http.StatusForbidden, gin.H{"error": "无权操作此订单"})
		return
	}

	if record.Status == models.RechargeStatusCompleted {
		view := rechargeViewFromRecord(record)
		if record.ActualAmount == nil {
			view.ActualAmount = &view.Amount
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true, "verified": true, "status": "completed",
			"message": "充值已到账", "order": buildRechargeRecordView(view),
		})
		return
	}

	if record.Status == models.RechargeStatusCancelled || record.Status == models.RechargeStatusFailed {
		message := "支付失败"
		if record.Status == models.RechargeStatusCancelled {
			message = "订单已取消"
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true, "verified": false, "status": record.Status,
			"message": message, "order": buildRechargeRecordView(rechargeViewFromRecord(record)),
		})
		return
	}

	provider, err := rechargeLoadProvider(ctx, int64(record.ProviderID))
	if err != nil {
		log.Printf("验证订单状态失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "验证订单状态失败"})
		return
	}
	if provider == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "支付渠道不存在"})
		return
	}

	effectiveConfig, _ := lib.ResolveRechargeProviderConfigSnapshot(string(provider.Type), provider.Config, record.ProviderConfigSnapshot)
	rechargeAmount := rechargeDecimalFloat(record.Amount)
	orderAmount := lib.GetRechargePayableAmount(lib.RechargePayableSource{Amount: rechargeAmount, PaymentDetails: record.PaymentDetails})
	creditedAmount := rechargeAmount
	if record.ActualAmount != nil {
		creditedAmount = record.ActualAmount.InexactFloat64()
	}

	pendingResponse := func(message string, includeView bool) gin.H {
		if includeView {
			view := rechargeViewFromRecord(record)
			return gin.H{
				"success": true, "verified": false, "status": record.Status,
				"message": message, "order": buildRechargeRecordView(view),
			}
		}
		return gin.H{
			"success": true, "verified": false, "status": record.Status,
			"message": message,
			"order":   gin.H{"orderNo": record.OrderNo, "amount": rechargeAmount, "status": record.Status},
		}
	}
	rejectedResponse := func(message, status string) gin.H {
		return gin.H{
			"success": false, "verified": false, "status": status, "message": message,
			"order": gin.H{"orderNo": record.OrderNo, "amount": rechargeAmount, "status": record.Status},
		}
	}

	var tradeNo string
	var tradeNoIndex string
	var actualAmount = creditedAmount
	var callbackPayload map[string]any = map[string]any{"source": "verify_api"}
	var paymentDetails any = record.PaymentDetails
	gatewayOrderNo := rechargeCurrentGatewayOrderNo(record.PaymentDetails, record.OrderNo)

	providerType := string(provider.Type)
	if providerType != "yipay" && providerType != "heleket" && providerType != "antom" {
		c.JSON(http.StatusOK, gin.H{
			"success": true, "verified": false, "status": record.Status,
			"message": "订单待支付，请稍后重试",
			"order":   gin.H{"orderNo": record.OrderNo, "amount": rechargeAmount, "status": record.Status},
		})
		return
	}

	if providerType == "yipay" {
		epayConfig, valid, configError := rechargeBuildEpayConfig(effectiveConfig)
		if !valid {
			log.Printf("支付渠道配置不完整 orderNo=%s error=%s", orderNo, configError)
			c.JSON(http.StatusOK, pendingResponse("订单待支付，请稍后重试", false))
			return
		}
		client := lib.CreateEpayClient(epayConfig)
		queryResult := client.QueryOrder(ctx, gatewayOrderNo)
		log.Printf("易支付订单查询结果 orderNo=%s success=%t paid=%t", orderNo, queryResult.Success, queryResult.Paid)

		if !queryResult.Success {
			c.JSON(http.StatusOK, pendingResponse("订单待支付，请稍后重试", false))
			return
		}
		if !queryResult.Paid {
			response := pendingResponse("订单待支付", false)
			response["status"] = "pending"
			c.JSON(http.StatusOK, response)
			return
		}
		if !lib.IsRechargeGatewayOrderNoMatch(gatewayOrderNo, queryResult.OutTradeNo) {
			log.Printf("易支付订单查询返回订单号不匹配，拒绝主动完成充值 orderNo=%s gatewayOrderNo=%s", orderNo, queryResult.OutTradeNo)
			c.JSON(http.StatusOK, rejectedResponse("支付订单号校验失败，请等待异步回调或联系客服", "pending"))
			return
		}

		tradeNo = queryResult.TradeNo
		tradeNoIndex = rechargeTradeNoForIndex(orderNo, &tradeNo)

		paidAmount := 0.0
		if queryResult.Money != "" {
			paidAmount, _ = rechargeParseFloat(queryResult.Money)
		}
		if paidAmount > 0 && !rechargeAmountsMatch(orderAmount, paidAmount) {
			log.Printf("支付金额与订单金额不匹配，拒绝处理 orderNo=%s orderAmount=%v paidAmount=%v diff=%v",
				orderNo, orderAmount, paidAmount, math.Abs(paidAmount-orderAmount))
			c.JSON(http.StatusOK, rejectedResponse("支付金额与订单金额不匹配，请联系客服", "pending"))
			return
		}
		actualAmount = creditedAmount
		callbackPayload = map[string]any{"source": "verify_api", "queryResult": queryResult.RawData}

	} else if providerType == "heleket" {
		heleketConfig, valid, configError := lib.BuildHeleketConfig(effectiveConfig)
		if !valid {
			log.Printf("Heleket 支付渠道配置不完整 orderNo=%s error=%s", orderNo, configError)
			c.JSON(http.StatusOK, pendingResponse("订单待支付，请稍后重试", false))
			return
		}
		client := lib.CreateHeleketClient(heleketConfig)
		queryResult, queryErr := client.GetPaymentInfo(ctx, lib.HeleketPaymentInfoRequest{OrderID: gatewayOrderNo})
		if queryErr != nil {
			log.Printf("验证订单状态失败: %v", queryErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "验证订单状态失败"})
			return
		}
		status := lib.ExtractHeleketStatus(queryResult)
		paymentState := lib.GetHeleketPaymentState(queryResult)
		statusMessage := lib.GetHeleketStatusDescription(status)

		if !lib.IsRechargeGatewayOrderNoMatch(gatewayOrderNo, queryResult["order_id"]) {
			log.Printf("Heleket 订单查询返回订单号不匹配，拒绝主动完成充值 orderNo=%s gatewayOrderNo=%v", orderNo, queryResult["order_id"])
			c.JSON(http.StatusOK, rejectedResponse("支付订单号校验失败，请等待异步回调或联系客服", string(record.Status)))
			return
		}

		callbackPayload = map[string]any{"source": "verify_api", "heleketStatus": status, "queryResult": queryResult}
		paymentDetails = lib.MergeHeleketPaymentDetails(record.PaymentDetails, queryResult, &heleketConfig, &lib.RechargeHeleketMergeFallback{
			OrderNo: &gatewayOrderNo, InvoiceAmount: &orderAmount,
		})

		heleketTradeNo := rechargeHeleketTradeNo(queryResult)
		if paymentState == lib.HeleketPaymentStatePending {
			if _, err := db.UpdateRechargeOrderMetadata(ctx, orderNo, db.RechargeMetadataUpdate{
				TradeNo: heleketTradeNo, TradeNoProvided: true,
				PaymentDetails: paymentDetails, PaymentDetailsProvided: true,
			}); err != nil {
				log.Printf("验证订单状态失败: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "验证订单状态失败"})
				return
			}
			view := rechargeViewFromRecord(record)
			view.PaymentDetails = paymentDetails
			view.TradeNo = heleketTradeNo
			c.JSON(http.StatusOK, gin.H{
				"success": true, "verified": false, "status": "pending",
				"message": statusMessage, "order": buildRechargeRecordView(view),
			})
			return
		}
		if paymentState == lib.HeleketPaymentStateCancelled {
			if _, err := db.CancelRecharge(ctx, orderNo, callbackPayload, true, paymentDetails, true); err != nil {
				log.Printf("验证订单状态失败: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "验证订单状态失败"})
				return
			}
			view := rechargeViewFromRecord(record)
			view.Status = "cancelled"
			view.PaymentDetails = paymentDetails
			c.JSON(http.StatusOK, gin.H{
				"success": true, "verified": true, "status": "cancelled",
				"message": statusMessage, "order": buildRechargeRecordView(view),
			})
			return
		}
		if paymentState == lib.HeleketPaymentStateFailed {
			if _, err := db.FailRecharge(ctx, orderNo, statusMessage, callbackPayload, true, paymentDetails, true); err != nil {
				log.Printf("验证订单状态失败: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "验证订单状态失败"})
				return
			}
			view := rechargeViewFromRecord(record)
			view.Status = "failed"
			view.FailReason = &statusMessage
			view.PaymentDetails = paymentDetails
			c.JSON(http.StatusOK, gin.H{
				"success": true, "verified": true, "status": "failed",
				"message": statusMessage, "order": buildRechargeRecordView(view),
			})
			return
		}

		tradeNo = rechargeHeleketTradeNoString(queryResult, orderNo)
		tradeNoIndex = rechargeTradeNoForIndex(orderNo, &tradeNo)

		invoiceAmount := lib.GetHeleketInvoiceAmount(queryResult)
		if invoiceAmount == nil || *invoiceAmount <= 0 {
			log.Printf("Heleket 返回的支付金额无效 orderNo=%s", orderNo)
			response := pendingResponse("支付处理中，请稍后重试", false)
			response["status"] = "pending"
			c.JSON(http.StatusOK, response)
			return
		}
		if rechargeAmountUnderpaid(orderAmount, *invoiceAmount) {
			log.Printf("Heleket 支付金额低于订单金额，拒绝处理 orderNo=%s orderAmount=%v invoiceAmount=%v diff=%v",
				orderNo, orderAmount, *invoiceAmount, orderAmount-*invoiceAmount)
			c.JSON(http.StatusOK, rejectedResponse("支付金额不足，请联系客服", "pending"))
			return
		}
		actualAmount = creditedAmount

	} else if providerType == "antom" {
		antomConfig, valid, configError := lib.BuildAntomConfig(effectiveConfig)
		if !valid {
			log.Printf("Antom 支付渠道配置不完整 orderNo=%s error=%s", orderNo, configError)
			c.JSON(http.StatusOK, pendingResponse("订单待支付，请稍后重试", false))
			return
		}
		client := lib.NewAntomClient(antomConfig)
		queryResult, queryErr := client.InquiryPayment(ctx, gatewayOrderNo)
		if queryErr != nil {
			log.Printf("验证订单状态失败: %v", queryErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "验证订单状态失败"})
			return
		}

		if queryResult.Result == nil || queryResult.Result.ResultStatus != "S" {
			message := "订单待支付，请稍后重试"
			if queryResult.Result != nil && queryResult.Result.ResultMessage != "" {
				message = queryResult.Result.ResultMessage
			}
			c.JSON(http.StatusOK, pendingResponse(message, false))
			return
		}
		if !lib.IsRechargeGatewayOrderNoMatch(gatewayOrderNo, queryResult.PaymentRequestID) {
			log.Printf("Antom 查询返回订单号不匹配 orderNo=%s gatewayOrderNo=%s", orderNo, queryResult.PaymentRequestID)
			c.JSON(http.StatusOK, rejectedResponse("支付订单号校验失败，请等待异步回调或联系客服", string(record.Status)))
			return
		}

		queryMap := map[string]any{
			"paymentRequestId":     queryResult.PaymentRequestID,
			"paymentId":            queryResult.PaymentID,
			"paymentStatus":        queryResult.PaymentStatus,
			"paymentResultMessage": queryResult.PaymentResultMessage,
		}
		if queryResult.PaymentAmount != nil {
			queryMap["paymentAmount"] = map[string]any{"currency": queryResult.PaymentAmount.Currency, "value": queryResult.PaymentAmount.Value}
		}
		if queryResult.ActualPaymentAmount != nil {
			queryMap["actualPaymentAmount"] = map[string]any{"currency": queryResult.ActualPaymentAmount.Currency, "value": queryResult.ActualPaymentAmount.Value}
		}
		if queryResult.Result != nil {
			queryMap["result"] = map[string]any{
				"resultStatus": queryResult.Result.ResultStatus, "resultMessage": queryResult.Result.ResultMessage,
				"resultCode": queryResult.Result.ResultCode,
			}
		}
		paymentDetails = lib.MergeAntomPaymentDetails(record.PaymentDetails, queryMap, antomConfig, lib.RechargeAntomMergeFallback{
			OrderNo: gatewayOrderNo, AmountCNY: orderAmount,
		})
		callbackPayload = map[string]any{
			"source": "verify_api", "antomPaymentStatus": queryResult.PaymentStatus, "antomPaymentId": queryResult.PaymentID,
		}

		if queryResult.PaymentStatus == "" || queryResult.PaymentStatus == "PROCESSING" {
			var metadataTradeNo *string
			if queryResult.PaymentID != "" {
				metadataTradeNo = &queryResult.PaymentID
			}
			if _, err := db.UpdateRechargeOrderMetadata(ctx, orderNo, db.RechargeMetadataUpdate{
				TradeNo: metadataTradeNo, TradeNoProvided: true,
				PaymentDetails: paymentDetails, PaymentDetailsProvided: true,
			}); err != nil {
				log.Printf("验证订单状态失败: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "验证订单状态失败"})
				return
			}
			view := rechargeViewFromRecord(record)
			view.PaymentDetails = paymentDetails
			view.TradeNo = metadataTradeNo
			message := "订单待支付"
			if queryResult.PaymentResultMessage != "" {
				message = queryResult.PaymentResultMessage
			}
			c.JSON(http.StatusOK, gin.H{
				"success": true, "verified": false, "status": "pending",
				"message": message, "order": buildRechargeRecordView(view),
			})
			return
		}

		if queryResult.PaymentStatus == "FAIL" || queryResult.PaymentStatus == "CANCELLED" {
			message := "Antom payment " + strings.ToLower(queryResult.PaymentStatus)
			if queryResult.PaymentResultMessage != "" {
				message = queryResult.PaymentResultMessage
			}
			var terminalRecord models.RechargeRecord
			var terminalErr error
			if queryResult.PaymentStatus == "CANCELLED" {
				terminalRecord, terminalErr = db.CancelRecharge(ctx, orderNo, callbackPayload, true, paymentDetails, true)
			} else {
				terminalRecord, terminalErr = db.FailRecharge(ctx, orderNo, message, callbackPayload, true, paymentDetails, true)
			}
			if terminalErr != nil {
				log.Printf("验证订单状态失败: %v", terminalErr)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "验证订单状态失败"})
				return
			}
			tradeNo = queryResult.PaymentID
			tradeNoIndex = rechargeTradeNoForIndex(orderNo, &tradeNo)
			db.MarkPaymentCallbackProcessed(ctx, int32(provider.ID), orderNo, &tradeNoIndex, rechargeClientIPPtr(c))
			view := rechargeViewFromRecord(record)
			view.Status = string(terminalRecord.Status)
			view.ActualAmount = rechargeDecimalPointer(terminalRecord.ActualAmount)
			feeValue := terminalRecord.Fee.InexactFloat64()
			view.Fee = &feeValue
			view.PaymentDetails = paymentDetails
			if tradeNo == "" {
				view.TradeNo = nil
			} else {
				view.TradeNo = &tradeNo
			}
			view.FailReason = &message
			view.CompletedAt = terminalRecord.CompletedAt
			c.JSON(http.StatusOK, gin.H{
				"success": true, "verified": true, "status": string(terminalRecord.Status),
				"message": message, "order": buildRechargeRecordView(view),
			})
			return
		}

		if queryResult.PaymentStatus != "SUCCESS" {
			response := pendingResponse("支付处理中，请稍后重试", false)
			response["status"] = "pending"
			c.JSON(http.StatusOK, response)
			return
		}

		var paidAmount *lib.AntomAmount
		if queryResult.ActualPaymentAmount != nil {
			paidAmount = queryResult.ActualPaymentAmount
		} else if queryResult.PaymentAmount != nil {
			paidAmount = queryResult.PaymentAmount
		}
		expectedMinor := ""
		existing := lib.ReadRechargePaymentDetails(record.PaymentDetails)
		if existing.Antom != nil && existing.Antom.AmountMinor != nil {
			expectedMinor = *existing.Antom.AmountMinor
		}
		if expectedMinor == "" {
			computed, computeErr := lib.CalculateAntomMinorAmount(orderAmount, antomConfig)
			if computeErr != nil {
				log.Printf("验证订单状态失败: %v", computeErr)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "验证订单状态失败"})
				return
			}
			expectedMinor = computed
		}
		if paidAmount == nil || paidAmount.Currency != antomConfig.Currency || paidAmount.Value != expectedMinor {
			log.Printf("Antom 支付金额或币种不匹配，拒绝处理 orderNo=%s expectedCurrency=%s expectedMinor=%s",
				orderNo, antomConfig.Currency, expectedMinor)
			c.JSON(http.StatusOK, rejectedResponse("支付金额或币种不匹配，请联系客服", "pending"))
			return
		}
		tradeNo = queryResult.PaymentID
		if tradeNo == "" {
			tradeNo = orderNo
		}
		tradeNoIndex = rechargeTradeNoForIndex(orderNo, &tradeNo)
		actualAmount = creditedAmount
	}

	if db.IsPaymentCallbackProcessed(ctx, int32(provider.ID), orderNo, tradeNoIndex) {
		updatedRecord, err := db.GetRechargeRecordByOrderNo(ctx, orderNo)
		if err != nil {
			log.Printf("验证订单状态失败: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "验证订单状态失败"})
			return
		}
		view := rechargeViewFromRecord(record)
		if updatedRecord != nil {
			view = rechargeViewFromRecord(updatedRecord)
		}
		if view.ActualAmount == nil {
			view.ActualAmount = rechargeDecimalPointer(&record.Amount)
		}
		if view.Fee == nil {
			feeValue := record.Fee.InexactFloat64()
			view.Fee = &feeValue
		}
		view.PaymentDetails = paymentDetails
		if view.TradeNo == nil && tradeNo != "" {
			view.TradeNo = &tradeNo
		}
		now := time.Now()
		if view.CompletedAt == nil {
			view.CompletedAt = &now
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true, "verified": true, "status": view.Status,
			"message": "充值已到账", "order": buildRechargeRecordView(view),
		})
		return
	}

	displayAmount := actualAmount
	if displayAmount <= 0 {
		displayAmount = orderAmount
	}

	var metadataTradeNo *string
	if tradeNo != "" {
		metadataTradeNo = &tradeNo
	}
	completion, completeErr := db.CompleteRecharge(ctx, orderNo, db.CompleteRechargeData{
		TradeNo: metadataTradeNo, TradeNoProvided: true,
		ActualAmount: &actualAmount,
		CallbackData: callbackPayload, CallbackDataProvided: true,
		PaymentDetails: paymentDetails, PaymentDetailsProvided: true,
	})
	if completeErr != nil {
		currentRecord, readErr := db.GetRechargeRecordByOrderNo(ctx, orderNo)
		if readErr == nil && currentRecord != nil && currentRecord.Status == models.RechargeStatusCompleted {
			db.MarkPaymentCallbackProcessed(ctx, int32(provider.ID), orderNo, &tradeNoIndex, rechargeClientIPPtr(c))
			view := rechargeViewFromRecord(currentRecord)
			if view.ActualAmount == nil {
				view.ActualAmount = rechargeDecimalPointer(&record.Amount)
			}
			view.PaymentDetails = paymentDetails
			if view.TradeNo == nil && tradeNo != "" {
				view.TradeNo = &tradeNo
			}
			c.JSON(http.StatusOK, gin.H{
				"success": true, "verified": true, "status": "completed",
				"message": "充值已到账", "order": buildRechargeRecordView(view),
			})
			return
		}
		log.Printf("验证订单状态失败: %v", completeErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "验证订单状态失败"})
		return
	}

	db.MarkPaymentCallbackProcessed(ctx, int32(provider.ID), orderNo, &tradeNoIndex, rechargeClientIPPtr(c))

	if completion.CompletedNow {
		db.CreateLog(ctx, rechargeInt64Ptr(int64(record.UserID)), "user", "recharge.completed",
			"Recharge completed via verify API: order "+orderNo+", amount "+rechargeJSNumberString(displayAmount)+", tradeNo: "+rechargeTradeNoLabel(tradeNo),
			"success", db.CreateLogOptions{})
		rechargeNotifyRechargeSuccess(ctx, record.UserID, orderNo, displayAmount, tradeNo)
	}
	log.Printf("通过 verify API 完成充值 orderNo=%s tradeNo=%s completedNow=%t", orderNo, tradeNo, completion.CompletedNow)

	view := rechargeViewFromRecord(record)
	view.ActualAmount = &actualAmount
	view.Status = "completed"
	view.PaymentDetails = paymentDetails
	view.FailReason = nil
	if tradeNo == "" {
		view.TradeNo = nil
	} else {
		view.TradeNo = &tradeNo
	}
	now := time.Now()
	view.CompletedAt = &now
	c.JSON(http.StatusOK, gin.H{
		"success": true, "verified": true, "status": "completed",
		"message": "充值成功！余额已到账", "order": buildRechargeRecordView(view),
	})
}

func rechargeInt64Ptr(value int64) *int64 { return &value }

func rechargeClientIPPtr(c *gin.Context) *string {
	ip := middleware.ClientIP(c)
	return &ip
}

func rechargeTradeNoLabel(tradeNo string) string {
	if tradeNo == "" {
		return "N/A"
	}
	return tradeNo
}

func rechargeHeleketTradeNo(payment map[string]any) *string {
	if text, ok := payment["uuid"].(string); ok && strings.TrimSpace(text) != "" {
		return &text
	}
	if text, ok := payment["txid"].(string); ok && strings.TrimSpace(text) != "" {
		return &text
	}
	return nil
}

func rechargeHeleketTradeNoString(payment map[string]any, orderNo string) string {
	for _, key := range []string{"uuid", "txid", "order_id"} {
		if text, ok := payment[key].(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return orderNo
}

// rechargeNotifyRechargeSuccess mirrors the inbox + email side effects that run
// after a credit. Failures are logged and never undo the credit.
func rechargeNotifyRechargeSuccess(ctx context.Context, userID int32, orderNo string, amount float64, tradeNo string) {
	_, err := db.CreateInboxMessage(ctx, db.CreateInboxMessageData{
		UserID:    userID,
		EventType: "recharge_success",
		Title:     "充值到账通知",
		Content: "您的充值已到账！\n充值金额：￥" + rechargeFixed2(amount) + "\n订单号：" + orderNo +
			"\n交易号：" + rechargeTradeNoLabel(tradeNo),
		Data: map[string]any{"orderNo": orderNo, "amount": amount, "tradeNo": tradeNo},
	})
	if err != nil {
		log.Printf("发送充值成功通知失败 orderNo=%s error=%v", orderNo, err)
		return
	}

	user, err := db.FindUserById(ctx, userID)
	if err != nil || user == nil || user.Email == nil || *user.Email == "" {
		return
	}
	balance, err := db.GetUserBalance(ctx, userID)
	if err != nil {
		return
	}
	var tradeNoPointer *string
	if tradeNo != "" {
		tradeNoPointer = &tradeNo
	}
	lib.SendRechargeSuccessEmail(ctx, *user.Email, lib.RechargeSuccessEmailData{
		Username: user.Username, Amount: amount, OrderNo: orderNo,
		TradeNo: tradeNoPointer, NewBalance: balance, Time: time.Now(),
	})
}

// ==================== admin: payment providers ====================

func rechargeAdminListProviders(c *gin.Context) {
	providers, err := db.GetAllPaymentProviders(c.Request.Context())
	if err != nil {
		log.Printf("获取支付渠道列表失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取支付渠道列表失败"})
		return
	}
	response := make([]db.PaymentProviderResponse, 0, len(providers))
	for _, provider := range providers {
		response = append(response, db.SanitizePaymentProviderForResponse(provider))
	}
	c.JSON(http.StatusOK, gin.H{"providers": response})
}

func rechargeAdminCreateProvider(c *gin.Context) {
	ctx := c.Request.Context()
	body := rechargeBody(c)
	if body == nil {
		body = map[string]any{}
	}
	name, _ := body["name"].(string)
	providerType := rechargeConfigString(body, "type")
	if name == "" || providerType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数不完整"})
		return
	}

	var inputConfig map[string]any
	if raw, exists := body["config"]; exists && raw != nil {
		if object, ok := raw.(map[string]any); ok {
			inputConfig = object
		}
	}
	if inputConfig == nil {
		inputConfig = map[string]any{}
	}
	inputStatus := "disabled"
	if raw, ok := body["status"].(string); ok && raw != "" {
		inputStatus = raw
	}

	valid, message, normalizedConfig := rechargeValidateProviderAdminInput(providerType, inputConfig, inputStatus)
	if !valid {
		if message == "" {
			message = "支付渠道配置不合法"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": message})
		return
	}

	financialInput := db.NewPaymentProviderFinancialInput(body)
	financialValid, financialMessage, financialData := db.ValidatePaymentProviderFinancialInput(financialInput, true, true)
	if !financialValid {
		if financialMessage == "" {
			financialMessage = "支付渠道金额配置不合法"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": financialMessage})
		return
	}

	input := db.CreatePaymentProviderInput{
		Name:            name,
		Type:            models.PaymentProviderType(providerType),
		Config:          normalizedConfig,
		ConfigProvided:  true,
		Financial:       financialInput,
		MethodsProvided: true,
	}
	if status, ok := body["status"].(string); ok && status != "" {
		typed := models.PaymentProviderStatus(status)
		input.Status = &typed
	}
	input.Methods = rechargeStringSlice(body["methods"])
	if raw, exists := body["sortOrder"]; exists {
		input.SortOrder = int32(rechargeAnyNumber(raw))
		input.SortOrderProvided = true
	}
	_ = financialData

	provider, err := db.CreatePaymentProvider(ctx, input)
	if err != nil {
		log.Printf("创建支付渠道失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "创建支付渠道失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"provider": db.SanitizePaymentProviderForResponse(provider)})
}

func rechargeStringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func rechargeProviderStatusPointer(value string) *models.PaymentProviderStatus {
	if value == "" {
		return nil
	}
	typed := models.PaymentProviderStatus(value)
	return &typed
}

func rechargeAdminUpdateProvider(c *gin.Context) {
	ctx := c.Request.Context()
	providerID := rechargeParsePositiveID(c.Param("id"))
	if providerID == nil || *providerID > math.MaxInt32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的渠道ID"})
		return
	}
	body := rechargeBody(c)
	if body == nil {
		body = map[string]any{}
	}

	existing, err := db.GetPaymentProviderByID(ctx, int32(*providerID))
	if err != nil {
		log.Printf("更新支付渠道失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新支付渠道失败"})
		return
	}
	if existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "支付渠道不存在"})
		return
	}

	inputConfig, configProvided := body["config"]
	var mergedConfig map[string]any
	if configProvided && inputConfig != nil {
		nextConfig, ok := inputConfig.(map[string]any)
		if !ok {
			nextConfig = map[string]any{}
		}
		mergedConfig = db.MergePaymentProviderConfigForUpdate(nextConfig, existing.Config)
	} else if configProvided {
		mergedConfig = db.MergePaymentProviderConfigForUpdate(map[string]any{}, existing.Config)
	} else {
		mergedConfig = existing.Config
	}

	mergedStatus := string(existing.Status)
	if status, ok := body["status"].(string); ok && status != "" {
		mergedStatus = status
	}
	valid, message, normalizedConfig := rechargeValidateProviderAdminInput(string(existing.Type), mergedConfig, mergedStatus)
	if !valid {
		if message == "" {
			message = "支付渠道配置不合法"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": message})
		return
	}

	mergedFinancialFields := map[string]any{}
	mergedFinancialFields["feeRate"] = rechargeAnyNumberOrExisting(body["feeRate"], existing.FeeRate)
	mergedFinancialFields["feeFixed"] = rechargeAnyNumberOrExisting(body["feeFixed"], existing.FeeFixed)
	mergedFinancialFields["minAmount"] = rechargeAnyNumberOrExisting(body["minAmount"], existing.MinAmount)
	if raw, exists := body["maxAmount"]; exists {
		mergedFinancialFields["maxAmount"] = raw
	} else if existing.MaxAmount != nil {
		mergedFinancialFields["maxAmount"] = *existing.MaxAmount
	} else {
		mergedFinancialFields["maxAmount"] = nil
	}
	mergedValid, mergedMessage, _ := db.ValidatePaymentProviderFinancialInput(
		db.NewPaymentProviderFinancialInput(mergedFinancialFields), true, true)
	if !mergedValid {
		if mergedMessage == "" {
			mergedMessage = "支付渠道金额配置不合法"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": mergedMessage})
		return
	}

	patchValid, patchMessage, _ := db.ValidatePaymentProviderFinancialInput(
		db.NewPaymentProviderFinancialInput(body), false, false)
	if !patchValid {
		if patchMessage == "" {
			patchMessage = "支付渠道金额配置不合法"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": patchMessage})
		return
	}

	input := db.UpdatePaymentProviderInput{
		Config:         normalizedConfig,
		ConfigProvided: configProvided,
		Financial:      db.NewPaymentProviderFinancialInput(body),
	}
	if name, ok := body["name"].(string); ok {
		input.Name = &name
	}
	if status := rechargeProviderStatusPointer(rechargeConfigString(body, "status")); status != nil {
		input.Status = status
	}
	if _, exists := body["methods"]; exists {
		input.Methods = rechargeStringSlice(body["methods"])
		input.MethodsProvided = true
	}
	if raw, exists := body["sortOrder"]; exists {
		value := int32(rechargeAnyNumber(raw))
		input.SortOrder = &value
	}

	provider, err := db.UpdatePaymentProvider(ctx, int32(*providerID), input)
	if err != nil {
		log.Printf("更新支付渠道失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新支付渠道失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"provider": db.SanitizePaymentProviderForResponse(provider)})
}

func rechargeAnyNumberOrExisting(value any, fallback float64) any {
	if value == nil {
		return fallback
	}
	return value
}

func rechargeAdminUpdateProviderStatus(c *gin.Context) {
	ctx := c.Request.Context()
	providerID := rechargeParsePositiveID(c.Param("id"))
	if providerID == nil || *providerID > math.MaxInt32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的渠道ID"})
		return
	}
	body := rechargeBody(c)
	if body == nil {
		body = map[string]any{}
	}
	status := rechargeConfigString(body, "status")
	statusValid := false
	for _, allowed := range rechargeProviderPaymentStatuses {
		if allowed == status {
			statusValid = true
			break
		}
	}
	if !statusValid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的状态值"})
		return
	}

	existing, err := db.GetPaymentProviderByID(ctx, int32(*providerID))
	if err != nil {
		log.Printf("更新支付渠道状态失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新支付渠道状态失败"})
		return
	}
	if existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "支付渠道不存在"})
		return
	}

	financialFields := map[string]any{
		"feeRate":   existing.FeeRate,
		"feeFixed":  existing.FeeFixed,
		"minAmount": existing.MinAmount,
	}
	if existing.MaxAmount != nil {
		financialFields["maxAmount"] = *existing.MaxAmount
	} else {
		financialFields["maxAmount"] = nil
	}
	financialValid, financialMessage, _ := db.ValidatePaymentProviderFinancialInput(
		db.NewPaymentProviderFinancialInput(financialFields), true, true)
	if !financialValid {
		if financialMessage == "" {
			financialMessage = "支付渠道金额配置不合法"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": financialMessage})
		return
	}

	valid, message, _ := rechargeValidateProviderAdminInput(string(existing.Type), existing.Config, status)
	if !valid {
		if message == "" {
			message = "支付渠道配置不合法"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": message})
		return
	}

	input := db.UpdatePaymentProviderInput{Status: rechargeProviderStatusPointer(status)}
	provider, err := db.UpdatePaymentProvider(ctx, int32(*providerID), input)
	if err != nil {
		log.Printf("更新支付渠道状态失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新支付渠道状态失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"provider": db.SanitizePaymentProviderForResponse(provider)})
}

func rechargeAdminDeleteProvider(c *gin.Context) {
	ctx := c.Request.Context()
	providerID := rechargeParsePositiveID(c.Param("id"))
	if providerID == nil || *providerID > math.MaxInt32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的渠道ID"})
		return
	}
	existing, err := db.GetPaymentProviderByID(ctx, int32(*providerID))
	if err != nil {
		log.Printf("删除支付渠道失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除支付渠道失败"})
		return
	}
	if existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "支付渠道不存在"})
		return
	}
	if err := db.DeletePaymentProvider(ctx, int32(*providerID)); err != nil {
		log.Printf("删除支付渠道失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除支付渠道失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "支付渠道已删除"})
}

// ==================== admin: orders / stats ====================

func rechargeAdminListOrders(c *gin.Context) {
	ctx := c.Request.Context()
	page := rechargeParseOptionalPositiveInteger(c.Query("page"), 1)
	pageSize := rechargeParseOptionalPositiveInteger(c.Query("pageSize"), 20)
	status := rechargeParseOptionalStatus(c.Query("status"))
	var userID int32
	if value := c.Query("userId"); value != "" {
		if parsed := rechargeParsePositiveID(value); parsed != nil && *parsed <= math.MaxInt32 {
			userID = int32(*parsed)
		}
	}

	result, err := db.GetAllRechargeRecords(ctx, db.RechargeRecordListOptions{
		Page: page, PageSize: pageSize, Status: status, UserID: userID,
	})
	if err != nil {
		log.Printf("获取充值记录失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取充值记录失败"})
		return
	}

	records := make([]gin.H, 0, len(result.Records))
	for _, record := range result.Records {
		var actualAmount any
		if record.ActualAmount != nil {
			actualAmount = record.ActualAmount.InexactFloat64()
		}
		records = append(records, gin.H{
			"id":           record.ID,
			"orderNo":      record.OrderNo,
			"userId":       record.UserID,
			"amount":       record.Amount.InexactFloat64(),
			"actualAmount": actualAmount,
			"fee":          record.Fee.InexactFloat64(),
			"status":       record.Status,
			"provider":     record.Provider,
			"createdAt":    rechargeISO(record.CreatedAt),
			"completedAt":  rechargeISOOrNil(record.CompletedAt),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"records": records, "total": result.Total, "page": result.Page, "pageSize": result.PageSize,
	})
}

func rechargeAdminStats(c *gin.Context) {
	ctx := c.Request.Context()
	startRaw := c.Query("start")
	endRaw := c.Query("end")
	var start, end *time.Time
	if startRaw != "" && endRaw != "" {
		parsedStart, err := rechargeParseJSDate(startRaw)
		if err != nil {
			log.Printf("获取充值统计失败: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "获取充值统计失败"})
			return
		}
		parsedEnd, err := rechargeParseJSDate(endRaw)
		if err != nil {
			log.Printf("获取充值统计失败: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "获取充值统计失败"})
			return
		}
		start, end = &parsedStart, &parsedEnd
	}
	stats, err := db.GetSystemRechargeStats(ctx, start, end)
	if err != nil {
		log.Printf("获取充值统计失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "获取充值统计失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"stats": stats})
}

// rechargeParseJSDate mirrors `new Date(value)`: ISO-8601 timestamps and plain
// YYYY-MM-DD dates (interpreted as UTC midnight). Anything else is Invalid Date,
// which Prisma rejects with a 500 at the route.
func rechargeParseJSDate(value string) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02",
	}
	for _, layout := range formats {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid date: %s", value)
}

// ==================== admin: complete / fail ====================

func rechargeAdminCompleteOrder(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	orderNo := c.Param("orderNo")
	body := rechargeBody(c)
	if body == nil {
		body = map[string]any{}
	}

	tradeNoInput := rechargeNormalizeOptionalTrimmedString(body["tradeNo"], rechargeMaxTradeNoLength)
	if tradeNoInput.Null {
		c.JSON(http.StatusBadRequest, gin.H{"error": "交易号格式无效或过长"})
		return
	}
	var actualAmount *float64
	if raw, exists := body["actualAmount"]; exists && raw != nil {
		actualAmount = rechargeParsePositiveJSONMoney(raw)
		if actualAmount == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "入账金额必须大于 0"})
			return
		}
	} else if raw, exists := body["actualAmount"]; exists {
		// Explicit null: Number(null) is 0, which fails the > 0 check.
		c.JSON(http.StatusBadRequest, gin.H{"error": "入账金额必须大于 0"})
		_ = raw
		return
	}

	record, err := db.GetRechargeRecordByOrderNo(ctx, orderNo)
	if err != nil {
		log.Printf("手动完成订单失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "手动完成订单失败"})
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "订单不存在"})
		return
	}
	if record.Status == models.RechargeStatusCompleted {
		c.JSON(http.StatusBadRequest, gin.H{"error": "订单已完成"})
		return
	}
	if record.Status == models.RechargeStatusCancelled || record.Status == models.RechargeStatusFailed || record.Status == models.RechargeStatusRefunded {
		c.JSON(http.StatusBadRequest, gin.H{"error": "订单已取消、失败或已退款"})
		return
	}

	completion, err := db.CompleteRecharge(ctx, orderNo, db.CompleteRechargeData{
		TradeNo:              tradeNoInput.Value,
		TradeNoProvided:      tradeNoInput.Value != nil,
		ActualAmount:         actualAmount,
		CallbackData:         map[string]any{"manual": true, "operator": user.Username},
		CallbackDataProvided: true,
	})
	if err != nil {
		log.Printf("手动完成订单失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "手动完成订单失败"})
		return
	}
	if !completion.CompletedNow {
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "订单已完成，无需重复处理"})
		return
	}

	db.CreateLog(ctx, rechargeInt64Ptr(int64(user.ID)), "admin", "recharge.manual_complete",
		"Admin manually completed recharge order "+orderNo+" for user "+strconv.Itoa(int(record.UserID))+
			", amount: "+rechargeJSNumberString(record.Amount.InexactFloat64()),
		"success", db.CreateLogOptions{})
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "订单已手动完成"})
}

func rechargeAdminFailOrder(c *gin.Context) {
	ctx := c.Request.Context()
	user := middleware.CurrentUser(c)
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}
	orderNo := c.Param("orderNo")
	body := rechargeBody(c)
	if body == nil {
		body = map[string]any{}
	}

	reasonInput := rechargeNormalizeOptionalTrimmedString(body["reason"], rechargeMaxAdminReasonLength)
	if reasonInput.Null {
		c.JSON(http.StatusBadRequest, gin.H{"error": "失败原因格式无效或过长"})
		return
	}
	failReason := "管理员标记失败"
	if reasonInput.Value != nil {
		failReason = *reasonInput.Value
	}

	record, err := db.GetRechargeRecordByOrderNo(ctx, orderNo)
	if err != nil {
		log.Printf("标记订单失败失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "标记订单失败"})
		return
	}
	if record == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "订单不存在"})
		return
	}
	if record.Status != models.RechargeStatusPending && record.Status != models.RechargeStatusPaid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "当前订单状态不允许标记失败"})
		return
	}

	if _, err := db.FailRecharge(ctx, orderNo, failReason,
		map[string]any{"manual": true, "operator": user.Username}, true, nil, false); err != nil {
		log.Printf("标记订单失败失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "标记订单失败"})
		return
	}

	db.CreateLog(ctx, rechargeInt64Ptr(int64(user.ID)), "admin", "recharge.manual_fail",
		"Admin marked recharge order "+orderNo+" as failed: "+failReason, "success", db.CreateLogOptions{})
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "订单已标记失败"})
}

// ==================== callbacks ====================

func rechargeCallbackPOST(c *gin.Context) {
	rawBody, status, message := rechargeReadCallbackBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return
	}
	callbackData := rechargeJSONObject(rawBody)
	if callbackData == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "回调数据格式不合法"})
		return
	}
	rechargeHandlePaymentCallback(c, c.Param("providerId"), callbackData, rawBody)
}

func rechargeReadCallbackBody(c *gin.Context) ([]byte, int, string) {
	// Fastify only parses JSON bodies; a text/plain callback reaches the handler as
	// a string, which normalizePaymentCallbackPayload() rejects as 回调数据格式不合法.
	contentType := c.GetHeader("Content-Type")
	if semicolon := strings.IndexByte(contentType, ';'); semicolon >= 0 {
		contentType = contentType[:semicolon]
	}
	if !strings.EqualFold(strings.TrimSpace(contentType), "application/json") {
		return nil, http.StatusBadRequest, "回调数据格式不合法"
	}
	limited := io.LimitReader(c.Request.Body, int64(rechargeCallbackBodyLimit)+1)
	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, http.StatusInternalServerError, "回调处理失败"
	}
	if len(payload) > rechargeCallbackBodyLimit {
		return nil, http.StatusRequestEntityTooLarge, "Request body is too large"
	}
	c.Request.Body = io.NopCloser(strings.NewReader(string(payload)))
	return payload, 0, ""
}

func rechargeCallbackGET(c *gin.Context) {
	// Fastify parses the query string into an object; repeated keys become arrays.
	query := c.Request.URL.Query()
	callbackData := make(map[string]any, len(query))
	for key, values := range query {
		if len(values) == 1 {
			callbackData[key] = values[0]
			continue
		}
		items := make([]any, 0, len(values))
		for _, value := range values {
			items = append(items, value)
		}
		callbackData[key] = items
	}
	rechargeHandlePaymentCallback(c, c.Param("providerId"), callbackData, nil)
}

// rechargeHandlePaymentCallback mirrors handlePaymentCallback.
func rechargeHandlePaymentCallback(c *gin.Context, providerIDParam string, callbackData map[string]any, rawBody []byte) {
	ctx := c.Request.Context()
	clientIP := middleware.ClientIP(c)

	providerID := rechargeParsePositiveID(providerIDParam)
	if providerID == nil || *providerID > math.MaxInt32 {
		log.Printf("无效的支付渠道 ID providerId=%s", providerIDParam)
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的支付渠道"})
		return
	}

	provider, err := db.GetPaymentProviderByID(ctx, int32(*providerID))
	if err != nil {
		log.Printf("支付回调处理失败: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "回调处理失败"})
		return
	}
	if provider == nil {
		log.Printf("支付渠道不存在 providerId=%s", providerIDParam)
		c.JSON(http.StatusNotFound, gin.H{"error": "支付渠道不存在"})
		return
	}

	if !rechargeIsIPInWhitelist(clientIP, string(provider.Type)) {
		log.Printf("支付回调 IP 不在白名单内 ip=%s providerId=%s providerType=%s", clientIP, providerIDParam, provider.Type)
		c.JSON(http.StatusForbidden, gin.H{"error": "拒绝访问"})
		return
	}

	preliminaryGatewayOrderNo := rechargeExtractOrderNoFromCallback(string(provider.Type), callbackData)
	preliminaryOrderNo := ""
	if preliminaryGatewayOrderNo != "" {
		preliminaryOrderNo = rechargeResolveOrderNoFromGatewayOrderNo(preliminaryGatewayOrderNo)
	}
	var preliminaryRecord *db.RechargeRecordWithProvider
	if preliminaryOrderNo != "" {
		preliminaryRecord, err = db.GetRechargeRecordByOrderNo(ctx, preliminaryOrderNo)
		if err != nil {
			log.Printf("支付回调处理失败: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "回调处理失败"})
			return
		}
	}

	var snapshotValue any
	if preliminaryRecord != nil {
		snapshotValue = preliminaryRecord.ProviderConfigSnapshot
	}
	config, configSource := lib.ResolveRechargeProviderConfigSnapshot(string(provider.Type), provider.Config, snapshotValue)
	valid, validationMessage := rechargeValidateActiveProvider(string(provider.Type), config)
	if !valid {
		log.Printf("拒绝处理未安全实现的支付回调 providerId=%s type=%s error=%s configSource=%s",
			providerIDParam, provider.Type, validationMessage, configSource)
		if validationMessage == "" {
			validationMessage = "支付渠道未安全启用"
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": validationMessage})
		return
	}
	epayVersion := rechargeConfigString(config, "version")
	if epayVersion == "" {
		epayVersion = "v2"
	}
	providerType := string(provider.Type)

	heleketPaymentStatus := ""
	heleketPaymentState := lib.HeleketPaymentStatePending
	var antomConfig *lib.AntomConfig
	var antomNotification *lib.AntomPaymentNotification

	switch providerType {
	case "yipay":
		epayConfig, valid, _ := rechargeBuildEpayConfig(config)
		if !valid {
			log.Printf("支付渠道配置不完整 providerId=%s", providerIDParam)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "支付渠道配置不完整"})
			return
		}
		client := lib.CreateEpayClient(epayConfig)
		result := client.VerifyWithStatus(callbackData)
		if !result.Valid {
			log.Printf("支付回调签名验证失败 providerId=%s ip=%s error=%s", providerIDParam, clientIP, result.Error)
			message := result.Error
			if message == "" {
				message = "签名验证失败"
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": message})
			return
		}
		if !result.TradeSuccess {
			log.Printf("交易未成功，跳过处理 providerId=%s tradeStatus=%v", providerIDParam, callbackData["trade_status"])
			rechargeEpaySuccessResponse(c, epayVersion)
			return
		}

	case "heleket":
		heleketConfig, valid, _ := lib.BuildHeleketConfig(config)
		if !valid {
			log.Printf("Heleket 支付渠道配置不完整 providerId=%s", providerIDParam)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "支付渠道配置不完整"})
			return
		}
		payload, parseErr := rechargeHeleketOrderedPayload(callbackData, rawBody, c.Request.URL.RawQuery)
		if parseErr != nil || !lib.VerifyHeleketWebhookSignature(payload, heleketConfig.APIKey) {
			log.Printf("Heleket 支付回调签名验证失败 providerId=%s ip=%s", providerIDParam, clientIP)
			c.JSON(http.StatusBadRequest, gin.H{"error": "签名验证失败"})
			return
		}
		heleketPaymentStatus = lib.ExtractHeleketStatus(callbackData)
		heleketPaymentState = lib.GetHeleketPaymentState(callbackData)

	case "antom":
		builtConfig, valid, _ := lib.BuildAntomConfig(config)
		if !valid {
			log.Printf("Antom 支付渠道配置不完整 providerId=%s", providerIDParam)
			c.JSON(http.StatusInternalServerError, lib.AntomNotificationResponseFor(false))
			return
		}
		antomConfig = &builtConfig
		notification := lib.AntomPaymentNotification{
			NotifyType: rechargeConfigString(callbackData, "notifyType"),
		}
		if text, ok := callbackData["paymentRequestId"].(string); ok {
			notification.PaymentRequestID = text
		}
		if text, ok := callbackData["paymentId"].(string); ok {
			notification.PaymentID = text
		}
		notification.PaymentAmount = rechargeAntomAmount(callbackData["paymentAmount"])
		notification.ActualPaymentAmount = rechargeAntomAmount(callbackData["actualPaymentAmount"])
		if result, ok := callbackData["result"].(map[string]any); ok {
			notification.Result = &lib.AntomResult{
				ResultCode:    rechargeStringFromMap(result, "resultCode"),
				ResultMessage: rechargeStringFromMap(result, "resultMessage"),
				ResultStatus:  rechargeStringFromMap(result, "resultStatus"),
			}
		}
		antomNotification = &notification

		requestURI := c.Request.RequestURI
		if index := strings.IndexByte(requestURI, '?'); index >= 0 {
			requestURI = requestURI[:index]
		}
		signatureValid := lib.VerifyAntomWebhook(
			builtConfig,
			c.Request.Method,
			requestURI,
			strings.Join(c.Request.Header.Values("request-time"), ","),
			strings.Join(c.Request.Header.Values("client-id"), ","),
			strings.Join(c.Request.Header.Values("signature"), ","),
			string(rawBody),
		)
		if !signatureValid {
			log.Printf("Antom 支付回调签名验证失败 providerId=%s ip=%s", providerIDParam, clientIP)
			c.JSON(http.StatusBadRequest, lib.AntomNotificationResponseFor(false))
			return
		}
		if notification.NotifyType != "PAYMENT_RESULT" {
			log.Printf("忽略非 PAYMENT_RESULT 的 Antom 通知 providerId=%s notifyType=%s", providerIDParam, notification.NotifyType)
			c.JSON(http.StatusOK, lib.AntomNotificationResponseFor(true))
			return
		}

	default:
		signature := ""
		if text, ok := callbackData["sign"].(string); ok && text != "" {
			signature = text
		} else if text, ok := callbackData["signature"].(string); ok && text != "" {
			signature = text
		} else {
			signature = strings.Join(c.Request.Header.Values("x-signature"), ",")
		}
		if !rechargeVerifyCallbackSignature(providerType, config, callbackData, signature) {
			log.Printf("支付回调签名验证失败 providerId=%s ip=%s", providerIDParam, clientIP)
			c.JSON(http.StatusBadRequest, gin.H{"error": "签名验证失败"})
			return
		}
	}

	// ---- extract order info per provider ----
	var orderNo string
	var tradeNo *string
	var actualAmount *float64

	switch providerType {
	case "yipay":
		orderNo, _ = callbackData["out_trade_no"].(string)
		if text, ok := callbackData["trade_no"].(string); ok {
			tradeNo = &text
		}
		if money, ok := callbackData["money"].(string); ok && money != "" {
			parsed, _ := rechargeParseFloat(money)
			actualAmount = &parsed
		}
	case "heleket":
		orderNo, _ = callbackData["order_id"].(string)
		heleketTradeNo := ""
		if text, ok := callbackData["uuid"].(string); ok && strings.TrimSpace(text) != "" {
			heleketTradeNo = text
		} else if text, ok := callbackData["txid"].(string); ok && strings.TrimSpace(text) != "" {
			heleketTradeNo = text
		} else if text, ok := callbackData["order_id"].(string); ok && strings.TrimSpace(text) != "" {
			heleketTradeNo = text
		}
		if heleketTradeNo != "" {
			tradeNo = &heleketTradeNo
		}
		if heleketPaymentState == lib.HeleketPaymentStatePaid {
			actualAmount = lib.GetHeleketInvoiceAmount(callbackData)
		}
	case "antom":
		if antomConfig != nil && antomNotification != nil {
			orderNo = antomNotification.PaymentRequestID
			if antomNotification.PaymentID != "" {
				tradeNo = &antomNotification.PaymentID
			}
			antomAmount := antomNotification.ActualPaymentAmount
			if antomAmount == nil {
				antomAmount = antomNotification.PaymentAmount
			}
			if antomAmount != nil && antomAmount.Currency == antomConfig.Currency {
				actualAmount = lib.AntomMinorAmountToCNY(antomAmount.Value, *antomConfig)
			}
		}
	case "alipay_direct":
		orderNo, _ = callbackData["out_trade_no"].(string)
		if text, ok := callbackData["trade_no"].(string); ok {
			tradeNo = &text
		}
		if text, ok := callbackData["total_amount"].(string); ok && text != "" {
			parsed, _ := rechargeParseFloat(text)
			actualAmount = &parsed
		}
	case "wechat_direct":
		orderNo, _ = callbackData["out_trade_no"].(string)
		if text, ok := callbackData["transaction_id"].(string); ok {
			tradeNo = &text
		}
		if raw, ok := callbackData["total_fee"]; ok {
			parsed := rechargeAnyNumber(raw) / 100
			actualAmount = &parsed
		}
	case "stripe":
		data, _ := callbackData["data"].(map[string]any)
		object, _ := data["object"].(map[string]any)
		metadata, _ := object["metadata"].(map[string]any)
		if text, ok := metadata["orderNo"].(string); ok {
			orderNo = text
		}
		if text, ok := object["id"].(string); ok {
			tradeNo = &text
		}
		if raw, ok := object["amount"]; ok {
			parsed := rechargeAnyNumber(raw) / 100
			actualAmount = &parsed
		}
	default:
		for _, key := range []string{"orderNo", "order_no", "out_trade_no"} {
			if text, ok := callbackData[key].(string); ok && text != "" {
				orderNo = text
				break
			}
		}
		for _, key := range []string{"tradeNo", "trade_no", "transaction_id"} {
			if text, ok := callbackData[key].(string); ok && text != "" {
				value := text
				tradeNo = &value
				break
			}
		}
		if number, ok := callbackData["amount"].(float64); ok {
			actualAmount = &number
		}
	}

	if orderNo == "" {
		log.Printf("回调数据缺少订单号 providerId=%s", providerIDParam)
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少订单号"})
		return
	}

	gatewayOrderNo := orderNo
	orderNo = rechargeResolveOrderNoFromGatewayOrderNo(gatewayOrderNo)
	tradeNoIndex := rechargeTradeNoForIndex(gatewayOrderNo, tradeNo)

	if db.IsPaymentCallbackProcessed(ctx, int32(*providerID), orderNo, tradeNoIndex) {
		log.Printf("重复回调，忽略 orderNo=%s", orderNo)
		rechargeCallbackSuccessResponse(c, providerType, epayVersion)
		return
	}

	record := preliminaryRecord
	if record == nil || record.OrderNo != orderNo {
		record, err = db.GetRechargeRecordByOrderNo(ctx, orderNo)
		if err != nil {
			log.Printf("支付回调处理失败: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "回调处理失败"})
			return
		}
	}
	if record == nil {
		log.Printf("订单不存在 orderNo=%s", orderNo)
		c.JSON(http.StatusNotFound, gin.H{"error": "订单不存在"})
		return
	}
	if record.ProviderID != int32(*providerID) {
		log.Printf("支付渠道不匹配 orderNo=%s expected=%d actual=%d", orderNo, record.ProviderID, *providerID)
		c.JSON(http.StatusBadRequest, gin.H{"error": "支付渠道不匹配"})
		return
	}

	if !rechargeIsCurrentPaymentAttempt(record.PaymentDetails, record.OrderNo, gatewayOrderNo) {
		db.MarkPaymentCallbackProcessed(ctx, int32(*providerID), record.OrderNo, &tradeNoIndex, &clientIP)
		log.Printf("支付回调来自已失效的支付尝试，拒绝入账 orderNo=%s gatewayOrderNo=%s", record.OrderNo, gatewayOrderNo)
		rechargeCallbackSuccessResponse(c, providerType, epayVersion)
		return
	}

	var paymentDetails any = record.PaymentDetails

	if record.Status == models.RechargeStatusCompleted {
		db.MarkPaymentCallbackProcessed(ctx, int32(*providerID), orderNo, &tradeNoIndex, &clientIP)
		log.Printf("订单已完成，幂等返回 orderNo=%s", orderNo)
		rechargeCallbackSuccessResponse(c, providerType, epayVersion)
		return
	}

	if providerType == "antom" && antomConfig != nil && antomNotification != nil {
		orderAmount := lib.GetRechargePayableAmount(lib.RechargePayableSource{Amount: rechargeDecimalFloat(record.Amount), PaymentDetails: record.PaymentDetails})
		notificationMap := map[string]any{
			"paymentRequestId": antomNotification.PaymentRequestID,
			"paymentId":        antomNotification.PaymentID,
			"notifyType":       antomNotification.NotifyType,
		}
		if antomNotification.PaymentAmount != nil {
			notificationMap["paymentAmount"] = map[string]any{"currency": antomNotification.PaymentAmount.Currency, "value": antomNotification.PaymentAmount.Value}
		}
		if antomNotification.ActualPaymentAmount != nil {
			notificationMap["actualPaymentAmount"] = map[string]any{"currency": antomNotification.ActualPaymentAmount.Currency, "value": antomNotification.ActualPaymentAmount.Value}
		}
		if antomNotification.Result != nil {
			notificationMap["result"] = map[string]any{
				"resultCode":    antomNotification.Result.ResultCode,
				"resultMessage": antomNotification.Result.ResultMessage,
				"resultStatus":  antomNotification.Result.ResultStatus,
			}
		}
		paymentDetails = lib.MergeAntomPaymentDetails(record.PaymentDetails, notificationMap, *antomConfig, lib.RechargeAntomMergeFallback{
			OrderNo: orderNo, AmountCNY: orderAmount,
		})

		resultStatus := ""
		resultMessage := ""
		resultCode := ""
		if antomNotification.Result != nil {
			resultStatus = antomNotification.Result.ResultStatus
			resultMessage = antomNotification.Result.ResultMessage
			resultCode = antomNotification.Result.ResultCode
		}
		if resultStatus == "U" {
			if _, err := db.UpdateRechargeOrderMetadata(ctx, orderNo, db.RechargeMetadataUpdate{
				TradeNo: tradeNo, TradeNoProvided: tradeNo != nil,
				CallbackData: callbackData, CallbackDataProvided: true,
				PaymentDetails: paymentDetails, PaymentDetailsProvided: true,
			}); err != nil {
				log.Printf("支付回调处理失败: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "回调处理失败"})
				return
			}
			log.Printf("Antom 处理中通知已记录 orderNo=%s", orderNo)
			c.JSON(http.StatusOK, lib.AntomNotificationResponseFor(true))
			return
		}
		if resultStatus == "F" {
			if record.Status == models.RechargeStatusPending || record.Status == models.RechargeStatusPaid {
				failReason := "Antom payment failed"
				if resultMessage != "" {
					failReason = resultMessage
				}
				if _, err := db.FailRecharge(ctx, orderNo, failReason, callbackData, true, paymentDetails, true); err != nil {
					log.Printf("支付回调处理失败: %v", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "回调处理失败"})
					return
				}
			}
			db.MarkPaymentCallbackProcessed(ctx, int32(*providerID), orderNo, &tradeNoIndex, &clientIP)
			log.Printf("Antom 失败通知已同步 orderNo=%s", orderNo)
			c.JSON(http.StatusOK, lib.AntomNotificationResponseFor(true))
			return
		}
		if resultStatus != "S" || resultCode != "SUCCESS" {
			log.Printf("Antom 通知结果状态不合法 orderNo=%s", orderNo)
			c.JSON(http.StatusBadRequest, lib.AntomNotificationResponseFor(false))
			return
		}

		var paidAmount *lib.AntomAmount
		if antomNotification.ActualPaymentAmount != nil {
			paidAmount = antomNotification.ActualPaymentAmount
		} else if antomNotification.PaymentAmount != nil {
			paidAmount = antomNotification.PaymentAmount
		}
		existing := lib.ReadRechargePaymentDetails(record.PaymentDetails)
		expectedMinor := ""
		if existing.Antom != nil && existing.Antom.AmountMinor != nil {
			expectedMinor = *existing.Antom.AmountMinor
		}
		if expectedMinor == "" {
			computed, computeErr := lib.CalculateAntomMinorAmount(orderAmount, *antomConfig)
			if computeErr != nil {
				log.Printf("支付回调处理失败: %v", computeErr)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "回调处理失败"})
				return
			}
			expectedMinor = computed
		}
		if paidAmount == nil || paidAmount.Currency != antomConfig.Currency || paidAmount.Value != expectedMinor {
			log.Printf("Antom 回调支付金额或币种不匹配 orderNo=%s expectedCurrency=%s expectedMinor=%s",
				orderNo, antomConfig.Currency, expectedMinor)
			c.JSON(http.StatusBadRequest, lib.AntomNotificationResponseFor(false))
			return
		}
	}

	shouldValidatePaidAmount := !(providerType == "heleket" && heleketPaymentState != lib.HeleketPaymentStatePaid)
	if shouldValidatePaidAmount && (actualAmount == nil || math.IsNaN(*actualAmount) || math.IsInf(*actualAmount, 0) || *actualAmount <= 0) {
		log.Printf("回调数据缺少有效支付金额 providerId=%s orderNo=%s", providerIDParam, orderNo)
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少有效支付金额"})
		return
	}
	paidActualAmount := 0.0
	if actualAmount != nil {
		paidActualAmount = *actualAmount
	}

	if providerType == "heleket" && heleketPaymentState != lib.HeleketPaymentStatePaid {
		heleketConfig, _, _ := lib.BuildHeleketConfig(config)
		orderAmount := lib.GetRechargePayableAmount(lib.RechargePayableSource{Amount: rechargeDecimalFloat(record.Amount), PaymentDetails: record.PaymentDetails})
		paymentDetails = lib.MergeHeleketPaymentDetails(record.PaymentDetails, callbackData, &heleketConfig, &lib.RechargeHeleketMergeFallback{
			OrderNo: &orderNo, InvoiceAmount: &orderAmount,
		})
		heleketCallbackPayload := map[string]any{}
		for key, value := range callbackData {
			heleketCallbackPayload[key] = value
		}
		heleketCallbackPayload["heleketStatus"] = heleketPaymentStatus

		if heleketPaymentState == lib.HeleketPaymentStatePending {
			if _, err := db.UpdateRechargeOrderMetadata(ctx, orderNo, db.RechargeMetadataUpdate{
				TradeNo: tradeNo, TradeNoProvided: true,
				CallbackData: heleketCallbackPayload, CallbackDataProvided: true,
				PaymentDetails: paymentDetails, PaymentDetailsProvided: true,
			}); err != nil {
				log.Printf("支付回调处理失败: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "回调处理失败"})
				return
			}
			log.Printf("Heleket 待处理回调已记录到本地订单 orderNo=%s heleketPaymentStatus=%s", orderNo, heleketPaymentStatus)
			c.JSON(http.StatusOK, gin.H{"code": "SUCCESS", "message": "OK"})
			return
		}

		if record.Status == models.RechargeStatusPending || record.Status == models.RechargeStatusPaid {
			if heleketPaymentState == lib.HeleketPaymentStateCancelled {
				if _, err := db.CancelRecharge(ctx, orderNo, heleketCallbackPayload, true, paymentDetails, true); err != nil {
					log.Printf("支付回调处理失败: %v", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "回调处理失败"})
					return
				}
			} else {
				failReason := lib.GetHeleketStatusDescription(heleketPaymentStatus)
				if _, err := db.FailRecharge(ctx, orderNo, failReason, heleketCallbackPayload, true, paymentDetails, true); err != nil {
					log.Printf("支付回调处理失败: %v", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "回调处理失败"})
					return
				}
			}
		}

		db.MarkPaymentCallbackProcessed(ctx, int32(*providerID), orderNo, &tradeNoIndex, &clientIP)
		log.Printf("Heleket 终态回调已同步到本地订单 orderNo=%s heleketPaymentStatus=%s state=%s", orderNo, heleketPaymentStatus, heleketPaymentState)
		c.JSON(http.StatusOK, gin.H{"code": "SUCCESS", "message": "OK"})
		return
	}

	rechargeAmount := rechargeDecimalFloat(record.Amount)
	expectedAmount := lib.GetRechargePayableAmount(lib.RechargePayableSource{Amount: rechargeAmount, PaymentDetails: record.PaymentDetails})
	creditedAmount := rechargeAmount
	if record.ActualAmount != nil {
		creditedAmount = record.ActualAmount.InexactFloat64()
	}

	if providerType == "heleket" {
		heleketConfig, _, _ := lib.BuildHeleketConfig(config)
		paymentDetails = lib.MergeHeleketPaymentDetails(record.PaymentDetails, callbackData, &heleketConfig, &lib.RechargeHeleketMergeFallback{
			OrderNo: &orderNo, InvoiceAmount: &expectedAmount,
		})
		if rechargeAmountUnderpaid(expectedAmount, paidActualAmount) {
			log.Printf("Heleket 支付金额低于订单金额，拒绝处理 orderNo=%s expected=%v actual=%v diff=%v",
				orderNo, expectedAmount, paidActualAmount, expectedAmount-paidActualAmount)
			db.CreateLog(ctx, rechargeInt64Ptr(int64(record.UserID)), "system", "recharge.amount_mismatch",
				"Underpaid recharge rejected: order "+orderNo+", expected "+rechargeJSNumberString(expectedAmount)+
					", actual "+rechargeJSNumberString(paidActualAmount), "warning", db.CreateLogOptions{})
			c.JSON(http.StatusBadRequest, gin.H{"error": "支付金额不足"})
			return
		}
	} else if providerType != "antom" {
		diff := math.Abs(paidActualAmount - expectedAmount)
		if !rechargeAmountsMatch(expectedAmount, paidActualAmount) {
			log.Printf("支付金额与订单金额不匹配，拒绝处理 orderNo=%s expected=%v actual=%v diff=%v",
				orderNo, expectedAmount, paidActualAmount, diff)
			db.CreateLog(ctx, rechargeInt64Ptr(int64(record.UserID)), "system", "recharge.amount_mismatch",
				"Amount mismatch rejected: order "+orderNo+", expected "+rechargeJSNumberString(expectedAmount)+
					", actual "+rechargeJSNumberString(paidActualAmount), "warning", db.CreateLogOptions{})
			c.JSON(http.StatusBadRequest, gin.H{"error": "支付金额与订单金额不匹配"})
			return
		}
	}

	if record.ExpiredAt != nil && record.ExpiredAt.Before(time.Now()) {
		log.Printf("订单已过本地过期时间，但回调已验签验额，按真实到账继续入账 orderNo=%s expiredAt=%s",
			orderNo, rechargeISO(*record.ExpiredAt))
	}

	if record.Status != models.RechargeStatusPending && record.Status != models.RechargeStatusPaid {
		log.Printf("订单状态不允许完成 orderNo=%s status=%s", orderNo, record.Status)
		c.JSON(http.StatusBadRequest, gin.H{"error": "订单状态异常"})
		return
	}

	completion, completeErr := db.CompleteRecharge(ctx, orderNo, db.CompleteRechargeData{
		TradeNo: tradeNo, TradeNoProvided: tradeNo != nil,
		ActualAmount: &creditedAmount,
		CallbackData: callbackData, CallbackDataProvided: true,
		PaymentDetails: paymentDetails, PaymentDetailsProvided: true,
	})
	if completeErr != nil {
		currentRecord, readErr := db.GetRechargeRecordByOrderNo(ctx, orderNo)
		if readErr == nil && currentRecord != nil && currentRecord.Status == models.RechargeStatusCompleted {
			db.MarkPaymentCallbackProcessed(ctx, int32(*providerID), orderNo, &tradeNoIndex, &clientIP)
			rechargeCallbackSuccessResponse(c, providerType, epayVersion)
			return
		}
		log.Printf("支付回调处理失败: %v", completeErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "回调处理失败"})
		return
	}

	db.MarkPaymentCallbackProcessed(ctx, int32(*providerID), orderNo, &tradeNoIndex, &clientIP)

	if completion.CompletedNow {
		db.CreateLog(ctx, rechargeInt64Ptr(int64(record.UserID)), "user", "recharge.completed",
			"Recharge completed: order "+orderNo+", amount "+rechargeJSNumberString(creditedAmount)+
				", tradeNo: "+rechargeTradeNoLabel(rechargeDerefStringString(tradeNo)),
			"success", db.CreateLogOptions{})
		rechargeNotifyRechargeSuccess(ctx, record.UserID, orderNo, creditedAmount, rechargeDerefStringString(tradeNo))
	}

	log.Printf("支付回调处理成功 orderNo=%s tradeNo=%s completedNow=%t", orderNo, rechargeDerefStringString(tradeNo), completion.CompletedNow)
	rechargeCallbackSuccessResponse(c, providerType, epayVersion)
}

func rechargeDerefStringString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func rechargeStringFromMap(object map[string]any, key string) string {
	text, _ := object[key].(string)
	return text
}

func rechargeAntomAmount(value any) *lib.AntomAmount {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	currency, _ := object["currency"].(string)
	amount, _ := object["value"].(string)
	return &lib.AntomAmount{Currency: currency, Value: amount}
}

// rechargeHeleketOrderedPayload builds the ordered JSON tree the Heleket signer
// needs. POST callbacks use the raw body (order preserved); GET callbacks are
// reconstructed from the raw query string, whose pair order is what fast-querystring
// (Node) preserves when it builds request.query. Heleket only posts JSON, so the
// GET path exists for API completeness and fails closed if the order differs.
func rechargeHeleketOrderedPayload(callbackData map[string]any, rawBody []byte, rawQuery string) (any, error) {
	if len(rawBody) > 0 {
		parsed, err := lib.ParseHeleketOrderedJSON(rawBody)
		if err != nil {
			return nil, err
		}
		if _, ok := parsed.(*lib.HeleketOrderedObject); !ok {
			return nil, errors.New("heleket payload is not an object")
		}
		return parsed, nil
	}

	object := lib.NewHeleketOrderedObject()
	if strings.TrimSpace(rawQuery) == "" {
		// No raw query available: fall back to the decoded map. The signature will
		// only match if the gateway's field order was alphabetical, which keeps the
		// check fail-closed rather than silently accepting a reordered body.
		keys := make([]string, 0, len(callbackData))
		for key := range callbackData {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			object.Set(key, callbackData[key])
		}
		return object, nil
	}

	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		keyPart := pair
		valuePart := ""
		if index := strings.IndexByte(pair, '='); index >= 0 {
			keyPart, valuePart = pair[:index], pair[index+1:]
		}
		key, err := url.QueryUnescape(keyPart)
		if err != nil {
			return nil, err
		}
		value, err := url.QueryUnescape(valuePart)
		if err != nil {
			return nil, err
		}
		if existing, ok := object.Get(key); ok {
			if items, isArray := existing.([]any); isArray {
				object.Set(key, append(items, value))
			} else {
				object.Set(key, []any{existing, value})
			}
			continue
		}
		object.Set(key, value)
	}
	return object, nil
}

// rechargeEpaySuccessResponse mirrors the yipay success envelope.
func rechargeEpaySuccessResponse(c *gin.Context, epayVersion string) {
	if epayVersion == "v1" {
		c.String(http.StatusOK, "success")
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": "SUCCESS", "message": "OK"})
}

// rechargeCallbackSuccessResponse mirrors the per-provider success envelope.
func rechargeCallbackSuccessResponse(c *gin.Context, providerType, epayVersion string) {
	switch providerType {
	case "yipay":
		rechargeEpaySuccessResponse(c, epayVersion)
	case "alipay_direct":
		c.String(http.StatusOK, "success")
	case "wechat_direct":
		c.JSON(http.StatusOK, gin.H{"return_code": "SUCCESS", "return_msg": "OK"})
	case "antom":
		c.JSON(http.StatusOK, lib.AntomNotificationResponseFor(true))
	default:
		c.JSON(http.StatusOK, gin.H{"code": "SUCCESS", "message": "OK"})
	}
}
