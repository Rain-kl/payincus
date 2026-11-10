// Port of server/src/db/payment-providers.ts.
//
// Storage shape (important): Prisma's `Json` column stores the result of
// encryptSensitiveData() — a *JSON string scalar*. Reading the column back
// therefore yields `"<ciphertext>"` (quotes included) rather than an object, and
// legacy rows may still hold a plain JSON object. decryptProviderConfig handles
// all three shapes and always degrades to {} instead of throwing.
//
// Import-cycle note: this file cannot import lib (lib imports db). Encryption is
// therefore delegated to a registered codec; lib/payment_provider_codec.go wires
// the real AES-GCM implementation in init(). Without a registered codec, reads
// still work for legacy plaintext rows and writes fail loudly rather than storing
// secrets in clear text.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// MaskedPaymentSecretValue mirrors MASKED_PAYMENT_SECRET_VALUE: the sentinel the
// admin UI submits to mean "keep the stored secret".
const MaskedPaymentSecretValue = "__SECRET_SET__"

// paymentProviderSecretKeys mirrors PAYMENT_PROVIDER_SECRET_KEYS.
var paymentProviderSecretKeys = []string{
	"key",
	"api_key",
	"secret_key",
	"webhook_secret",
	"merchant_private_key",
	"private_key",
	"signKey",
	"sign_key",
}

// ==================== sensitive-data codec seam ====================

var (
	paymentProviderCodecMu sync.RWMutex
	paymentProviderEncrypt func(string) (string, error)
	paymentProviderDecrypt func(string) string
)

// RegisterPaymentProviderSensitiveCodec wires lib.EncryptSensitiveData /
// lib.DecryptSensitiveData into the db layer (lib already imports db, so the
// dependency must point this way).
func RegisterPaymentProviderSensitiveCodec(encrypt func(string) (string, error), decrypt func(string) string) {
	paymentProviderCodecMu.Lock()
	defer paymentProviderCodecMu.Unlock()
	paymentProviderEncrypt = encrypt
	paymentProviderDecrypt = decrypt
}

func currentPaymentProviderCodec() (func(string) (string, error), func(string) string) {
	paymentProviderCodecMu.RLock()
	defer paymentProviderCodecMu.RUnlock()
	return paymentProviderEncrypt, paymentProviderDecrypt
}

// ==================== types ====================

// PaymentProviderRecord is the decrypted, numerically-normalised view of a
// payment_providers row. The TS returns the Prisma model and calls Number(...) on
// the Decimal fields at every use site; carrying float64 here reproduces that
// domain, while PaymentProviderResponse re-serialises the Decimal columns as
// strings for the wire.
type PaymentProviderRecord struct {
	ID         int32
	Name       string
	Type       models.PaymentProviderType
	Status     models.PaymentProviderStatus
	Config     map[string]any
	Methods    []string
	MethodsRaw any
	FeeRate    float64
	FeeFixed   float64
	MinAmount  float64
	MaxAmount  *float64
	SortOrder  int32
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// PaymentProviderResponse is the JSON projection of a provider, matching what
// Prisma + Fastify emit: Decimal columns as strings, DateTime as ISO-8601 with
// milliseconds, config masked.
type PaymentProviderResponse struct {
	ID        int32                        `json:"id"`
	Name      string                       `json:"name"`
	Type      models.PaymentProviderType   `json:"type"`
	Status    models.PaymentProviderStatus `json:"status"`
	Config    map[string]any               `json:"config"`
	Methods   any                          `json:"methods"`
	FeeRate   string                       `json:"feeRate"`
	FeeFixed  string                       `json:"feeFixed"`
	MinAmount string                       `json:"minAmount"`
	MaxAmount *string                      `json:"maxAmount"`
	SortOrder int32                        `json:"sortOrder"`
	CreatedAt string                       `json:"createdAt"`
	UpdatedAt string                       `json:"updatedAt"`
}

// CreatePaymentProviderInput mirrors CreatePaymentProviderInput.
type CreatePaymentProviderInput struct {
	Name              string
	Type              models.PaymentProviderType
	Status            *models.PaymentProviderStatus
	Config            map[string]any
	ConfigProvided    bool
	Methods           []string
	MethodsProvided   bool
	Financial         PaymentProviderFinancialInput
	SortOrder         int32
	SortOrderProvided bool
}

// UpdatePaymentProviderInput mirrors UpdatePaymentProviderInput with explicit
// presence tracking (TS `!== undefined`).
type UpdatePaymentProviderInput struct {
	Name            *string
	Status          *models.PaymentProviderStatus
	Config          map[string]any
	ConfigProvided  bool
	Methods         []string
	MethodsProvided bool
	Financial       PaymentProviderFinancialInput
	SortOrder       *int32
}

// PaymentMethodFeeConfig mirrors PaymentMethodFeeConfig.
type PaymentMethodFeeConfig struct {
	FeeRate  float64 `json:"feeRate"`
	FeeFixed float64 `json:"feeFixed"`
}

// PaymentMethodFeeMap mirrors the record returned by getPaymentMethodFeeMap.
type PaymentMethodFeeMap map[string]PaymentMethodFeeConfig

// PaymentProviderFinancialInput mirrors PaymentProviderFinancialInput while
// tracking which keys were present (Object.prototype.hasOwnProperty).
type PaymentProviderFinancialInput struct {
	Fields map[string]any
}

// NewPaymentProviderFinancialInput wraps a request body.
func NewPaymentProviderFinancialInput(fields map[string]any) PaymentProviderFinancialInput {
	if fields == nil {
		fields = map[string]any{}
	}
	return PaymentProviderFinancialInput{Fields: fields}
}

func (i PaymentProviderFinancialInput) has(field string) bool {
	_, ok := i.Fields[field]
	return ok
}

func (i PaymentProviderFinancialInput) value(field string) any { return i.Fields[field] }

// NormalizedPaymentProviderFinancialInput mirrors the TS normalised shape.
// MaxAmountSet distinguishes "explicitly null" from "not provided".
type NormalizedPaymentProviderFinancialInput struct {
	FeeRate      *float64
	FeeFixed     *float64
	MinAmount    *float64
	MaxAmount    *float64
	MaxAmountSet bool
}

// ==================== JS number / toFixed helpers ====================

// paymentProviderToNumber mirrors Number(value) for the shapes JSON bodies hold.
func paymentProviderToNumber(value any) float64 {
	switch typed := value.(type) {
	case nil:
		return 0 // Number(null) === 0
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
		parsed, err := typed.Float64()
		if err != nil {
			return math.NaN()
		}
		return parsed
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0 // Number('') === 0
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
	default:
		return math.NaN()
	}
}

// paymentProviderToFixed mirrors `Number(value.toFixed(precision))`: exact
// decimal rounding of the binary value, ties away from zero.
func paymentProviderToFixed(value float64, precision int) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) >= 1e21 {
		return value
	}
	exact := new(big.Rat).SetFloat64(value)
	if exact == nil {
		return value
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(precision)), nil)
	exact.Mul(exact, new(big.Rat).SetInt(scale))

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
	result, _ := new(big.Rat).SetFrac(quotient, scale).Float64()
	return result
}

// paymentProviderNumberString renders a number like JS `${value}`.
func paymentProviderNumberString(value float64) string {
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

// paymentProviderDecimalString mirrors Prisma Decimal JSON: trailing zeros are
// trimmed but integers keep no decimal point.
func paymentProviderDecimalString(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// ==================== financial validation ====================

type paymentProviderMoneyOptions struct {
	Min           float64
	Max           *float64
	AllowEqualMin bool
	Precision     int
}

// normalizePaymentProviderMoneyNumber mirrors normalizeMoneyNumber.
func normalizePaymentProviderMoneyNumber(value any, label string, options paymentProviderMoneyOptions) (float64, error) {
	amount := paymentProviderToNumber(value)
	minOK := amount > options.Min
	if options.AllowEqualMin {
		minOK = amount >= options.Min
	}
	precision := options.Precision
	if precision == 0 {
		precision = 2
	}

	if math.IsNaN(amount) || math.IsInf(amount, 0) || !minOK {
		return 0, fmt.Errorf("%s不合法", label)
	}
	if options.Max != nil && amount > *options.Max {
		return 0, fmt.Errorf("%s不合法", label)
	}

	normalizedAmount := paymentProviderToFixed(amount, precision)
	normalizedMinOK := normalizedAmount > options.Min
	if options.AllowEqualMin {
		normalizedMinOK = normalizedAmount >= options.Min
	}
	if !normalizedMinOK {
		return 0, fmt.Errorf("%s不合法", label)
	}
	return normalizedAmount, nil
}

// NormalizePaymentProviderFinancialInput mirrors normalizePaymentProviderFinancialInput.
func NormalizePaymentProviderFinancialInput(
	input PaymentProviderFinancialInput,
	fillDefaults bool,
	validateRange bool,
) (NormalizedPaymentProviderFinancialInput, error) {
	data := NormalizedPaymentProviderFinancialInput{}

	if input.has("feeRate") || fillDefaults {
		raw := input.value("feeRate")
		if raw == nil {
			raw = 0
		}
		maxRate := 1.0
		value, err := normalizePaymentProviderMoneyNumber(raw, "手续费费率", paymentProviderMoneyOptions{
			Min: 0, Max: &maxRate, AllowEqualMin: true, Precision: 4,
		})
		if err != nil {
			return data, err
		}
		data.FeeRate = &value
	}

	if input.has("feeFixed") || fillDefaults {
		raw := input.value("feeFixed")
		if raw == nil {
			raw = 0
		}
		value, err := normalizePaymentProviderMoneyNumber(raw, "固定手续费", paymentProviderMoneyOptions{
			Min: 0, AllowEqualMin: true,
		})
		if err != nil {
			return data, err
		}
		data.FeeFixed = &value
	}

	if input.has("minAmount") || fillDefaults {
		raw := input.value("minAmount")
		if raw == nil {
			raw = 1
		}
		value, err := normalizePaymentProviderMoneyNumber(raw, "最小充值金额", paymentProviderMoneyOptions{Min: 0})
		if err != nil {
			return data, err
		}
		data.MinAmount = &value
	}

	if input.has("maxAmount") || fillDefaults {
		raw := input.value("maxAmount")
		data.MaxAmountSet = true
		if raw == nil || raw == "" {
			data.MaxAmount = nil
		} else {
			value, err := normalizePaymentProviderMoneyNumber(raw, "最大充值金额", paymentProviderMoneyOptions{Min: 0})
			if err != nil {
				return data, err
			}
			data.MaxAmount = &value
		}
	}

	var effectiveMinAmount *float64
	if data.MinAmount != nil {
		effectiveMinAmount = data.MinAmount
	} else if fillDefaults {
		one := 1.0
		effectiveMinAmount = &one
	}
	if validateRange && data.MaxAmount != nil && effectiveMinAmount != nil && *data.MaxAmount < *effectiveMinAmount {
		return data, errors.New("最大充值金额不能小于最小充值金额")
	}
	return data, nil
}

// ValidatePaymentProviderFinancialInput mirrors validatePaymentProviderFinancialInput.
func ValidatePaymentProviderFinancialInput(
	input PaymentProviderFinancialInput,
	fillDefaults bool,
	validateRange bool,
) (bool, string, NormalizedPaymentProviderFinancialInput) {
	data, err := NormalizePaymentProviderFinancialInput(input, fillDefaults, validateRange)
	if err != nil {
		return false, err.Error(), NormalizedPaymentProviderFinancialInput{}
	}
	return true, "", data
}

// ==================== config codec ====================

// encodePaymentProviderConfig mirrors encryptProviderConfig: JSON -> AES-GCM,
// stored as a JSON string scalar (what Prisma does with a string value).
func encodePaymentProviderConfig(config map[string]any) (datatypes.JSON, error) {
	encoded, err := json.Marshal(config)
	if err != nil {
		return datatypes.JSON([]byte("{}")), err
	}
	encrypt, _ := currentPaymentProviderCodec()
	if encrypt == nil {
		// Fail loudly: silently persisting gateway secrets in clear text is worse
		// than refusing the write. Only reachable in a process that never imported
		// lib, which the server entrypoint always does.
		return nil, errors.New("payment provider sensitive-data codec is not registered")
	}
	ciphertext, err := encrypt(string(encoded))
	if err != nil {
		return nil, err
	}
	quoted, err := json.Marshal(ciphertext)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(quoted), nil
}

// decodePaymentProviderConfig mirrors decryptProviderConfig.
func decodePaymentProviderConfig(raw datatypes.JSON) map[string]any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return map[string]any{}
	}

	// Plain JSON object (legacy rows).
	if strings.HasPrefix(trimmed, "{") {
		var object map[string]any
		if err := json.Unmarshal([]byte(trimmed), &object); err == nil && object != nil {
			return object
		}
		return map[string]any{}
	}

	// JSON string scalar: either the encrypted payload or a legacy JSON string.
	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if err := json.Unmarshal([]byte(trimmed), &text); err != nil {
			return map[string]any{}
		}
		return decodePaymentProviderConfigString(text)
	}

	return map[string]any{}
}

func decodePaymentProviderConfigString(text string) map[string]any {
	if strings.Contains(text, ":") {
		_, decrypt := currentPaymentProviderCodec()
		if decrypt != nil {
			if decrypted := decrypt(text); decrypted != "" {
				var object map[string]any
				if err := json.Unmarshal([]byte(decrypted), &object); err == nil && object != nil {
					return object
				}
			}
		}
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(text), &object); err == nil && object != nil {
		return object
	}
	return map[string]any{}
}

// ==================== masks / merges ====================

func hasExistingSecretValue(value any) bool {
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) != ""
}

func shouldPreserveSecretValue(value any) bool {
	if value == nil {
		return true
	}
	if text, ok := value.(string); ok {
		return text == "" || text == MaskedPaymentSecretValue
	}
	return false
}

// maskProviderConfig mirrors maskProviderConfig: non-empty secrets become "" plus
// `<key>Configured = true`.
func maskProviderConfig(config map[string]any) map[string]any {
	masked := make(map[string]any, len(config))
	for key, value := range config {
		if paymentProviderIsSecretKey(key) && hasExistingSecretValue(value) {
			masked[key] = ""
			masked[key+"Configured"] = true
			continue
		}
		masked[key] = value
	}
	return masked
}

func paymentProviderIsSecretKey(key string) bool {
	for _, secret := range paymentProviderSecretKeys {
		if secret == key {
			return true
		}
	}
	return false
}

// MergePaymentProviderConfigForUpdate mirrors mergePaymentProviderConfigForUpdate.
func MergePaymentProviderConfigForUpdate(nextConfig, existingConfig map[string]any) map[string]any {
	merged := make(map[string]any, len(nextConfig))
	for key, value := range nextConfig {
		merged[key] = value
	}
	for _, secret := range paymentProviderSecretKeys {
		if shouldPreserveSecretValue(nextConfig[secret]) && hasExistingSecretValue(existingConfig[secret]) {
			merged[secret] = existingConfig[secret]
		}
		delete(merged, secret+"Configured")
	}
	return merged
}

// SanitizePaymentProviderForResponse mirrors sanitizePaymentProviderForResponse.
func SanitizePaymentProviderForResponse(provider PaymentProviderRecord) PaymentProviderResponse {
	config := provider.Config
	if config == nil {
		config = map[string]any{}
	}
	response := PaymentProviderResponse{
		ID:        provider.ID,
		Name:      provider.Name,
		Type:      provider.Type,
		Status:    provider.Status,
		Config:    maskProviderConfig(config),
		Methods:   provider.MethodsRaw,
		FeeRate:   paymentProviderDecimalString(provider.FeeRate),
		FeeFixed:  paymentProviderDecimalString(provider.FeeFixed),
		MinAmount: paymentProviderDecimalString(provider.MinAmount),
		SortOrder: provider.SortOrder,
		CreatedAt: provider.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		UpdatedAt: provider.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	if response.Methods == nil {
		response.Methods = []string{}
	}
	if provider.MaxAmount != nil {
		text := paymentProviderDecimalString(*provider.MaxAmount)
		response.MaxAmount = &text
	}
	return response
}

// ==================== row mapping ====================

func paymentProviderParseMethods(raw datatypes.JSON) ([]string, any) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return []string{}, []string{}
	}
	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return []string{}, []string{}
	}
	items, isArray := decoded.([]any)
	if !isArray {
		return []string{}, decoded
	}
	methods := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			methods = append(methods, text)
		}
	}
	return methods, decoded
}

func paymentProviderRecordFromRow(row models.PaymentProvider) PaymentProviderRecord {
	methods, rawMethods := paymentProviderParseMethods(row.Methods)
	record := PaymentProviderRecord{
		ID:         row.ID,
		Name:       row.Name,
		Type:       row.Type,
		Status:     row.Status,
		Config:     decodePaymentProviderConfig(row.Config),
		Methods:    methods,
		MethodsRaw: rawMethods,
		FeeRate:    decimalToFloat64(row.FeeRate),
		FeeFixed:   decimalToFloat64(row.FeeFixed),
		MinAmount:  decimalToFloat64(row.MinAmount),
		SortOrder:  row.SortOrder,
		CreatedAt:  row.CreatedAt,
		UpdatedAt:  row.UpdatedAt,
	}
	if row.MaxAmount != nil {
		value := decimalToFloat64(*row.MaxAmount)
		record.MaxAmount = &value
	}
	return record
}

func paymentProviderDecimalsForFinancial(data NormalizedPaymentProviderFinancialInput) (feeRate, feeFixed, minAmount models.Decimal, maxAmount *models.Decimal) {
	feeRate = decimalZero()
	if data.FeeRate != nil {
		feeRate = decimalFromFloat(*data.FeeRate)
	}
	feeFixed = decimalZero()
	if data.FeeFixed != nil {
		feeFixed = decimalFromFloat(*data.FeeFixed)
	}
	minAmount = decimalOne()
	if data.MinAmount != nil {
		minAmount = decimalFromFloat(*data.MinAmount)
	}
	if data.MaxAmountSet && data.MaxAmount != nil {
		value := decimalFromFloat(*data.MaxAmount)
		maxAmount = &value
	}
	return feeRate, feeFixed, minAmount, maxAmount
}

// ==================== queries ====================

// GetAllPaymentProviders mirrors getAllPaymentProviders (decrypted configs,
// sort_order then id).
func GetAllPaymentProviders(ctx context.Context) ([]PaymentProviderRecord, error) {
	var rows []models.PaymentProvider
	if err := DB.WithContext(ctx).Order("sort_order ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	records := make([]PaymentProviderRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, paymentProviderRecordFromRow(row))
	}
	return records, nil
}

// GetActivePaymentProviders mirrors getActivePaymentProviders.
func GetActivePaymentProviders(ctx context.Context) ([]PaymentProviderRecord, error) {
	var rows []models.PaymentProvider
	if err := DB.WithContext(ctx).
		Where("status = ?", string(models.PaymentProviderStatusActive)).
		Order("sort_order ASC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	records := make([]PaymentProviderRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, paymentProviderRecordFromRow(row))
	}
	return records, nil
}

// GetPaymentProviderByID mirrors getPaymentProviderById.
func GetPaymentProviderByID(ctx context.Context, id int32) (*PaymentProviderRecord, error) {
	var row models.PaymentProvider
	err := DB.WithContext(ctx).First(&row, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record := paymentProviderRecordFromRow(row)
	return &record, nil
}

// GetPaymentProviderByType mirrors getPaymentProviderByType.
func GetPaymentProviderByType(ctx context.Context, providerType models.PaymentProviderType) (*PaymentProviderRecord, error) {
	var row models.PaymentProvider
	err := DB.WithContext(ctx).
		Where("type = ? AND status = ?", string(providerType), string(models.PaymentProviderStatusActive)).
		Order("id ASC").
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record := paymentProviderRecordFromRow(row)
	return &record, nil
}

// ==================== mutations ====================

// CreatePaymentProvider mirrors createPaymentProvider.
func CreatePaymentProvider(ctx context.Context, input CreatePaymentProviderInput) (PaymentProviderRecord, error) {
	config := input.Config
	if !input.ConfigProvided || config == nil {
		config = map[string]any{}
	}
	encodedConfig, err := encodePaymentProviderConfig(config)
	if err != nil {
		return PaymentProviderRecord{}, err
	}
	financial, err := NormalizePaymentProviderFinancialInput(input.Financial, true, true)
	if err != nil {
		return PaymentProviderRecord{}, err
	}
	feeRate, feeFixed, minAmount, maxAmount := paymentProviderDecimalsForFinancial(financial)

	status := models.PaymentProviderStatusDisabled
	if input.Status != nil && *input.Status != "" {
		status = *input.Status
	}
	methods := []string{}
	if input.MethodsProvided && input.Methods != nil {
		methods = input.Methods
	}
	methodsJSON, err := json.Marshal(methods)
	if err != nil {
		return PaymentProviderRecord{}, err
	}

	row := models.PaymentProvider{
		Name:      input.Name,
		Type:      input.Type,
		Status:    status,
		Config:    encodedConfig,
		Methods:   datatypes.JSON(methodsJSON),
		FeeRate:   feeRate,
		FeeFixed:  feeFixed,
		MinAmount: minAmount,
		MaxAmount: maxAmount,
		SortOrder: input.SortOrder,
	}
	if err := DB.WithContext(ctx).Create(&row).Error; err != nil {
		return PaymentProviderRecord{}, err
	}
	return paymentProviderRecordFromRow(row), nil
}

// UpdatePaymentProvider mirrors updatePaymentProvider. A missing row is an error
// (Prisma P2025), which the route surfaces as a 500 after its own existence check.
func UpdatePaymentProvider(ctx context.Context, id int32, input UpdatePaymentProviderInput) (PaymentProviderRecord, error) {
	updates := map[string]any{}
	if input.Name != nil {
		updates["name"] = *input.Name
	}
	if input.Status != nil {
		updates["status"] = string(*input.Status)
	}
	if input.ConfigProvided {
		config := input.Config
		if config == nil {
			config = map[string]any{}
		}
		encoded, err := encodePaymentProviderConfig(config)
		if err != nil {
			return PaymentProviderRecord{}, err
		}
		updates["config"] = encoded
	}
	if input.MethodsProvided {
		methods := input.Methods
		if methods == nil {
			methods = []string{}
		}
		encoded, err := json.Marshal(methods)
		if err != nil {
			return PaymentProviderRecord{}, err
		}
		updates["methods"] = datatypes.JSON(encoded)
	}
	financial, err := NormalizePaymentProviderFinancialInput(input.Financial, false, false)
	if err != nil {
		return PaymentProviderRecord{}, err
	}
	if financial.FeeRate != nil {
		updates["fee_rate"] = decimalFromFloat(*financial.FeeRate)
	}
	if financial.FeeFixed != nil {
		updates["fee_fixed"] = decimalFromFloat(*financial.FeeFixed)
	}
	if financial.MinAmount != nil {
		updates["min_amount"] = decimalFromFloat(*financial.MinAmount)
	}
	if financial.MaxAmountSet {
		if financial.MaxAmount == nil {
			updates["max_amount"] = nil
		} else {
			updates["max_amount"] = decimalFromFloat(*financial.MaxAmount)
		}
	}
	if input.SortOrder != nil {
		updates["sort_order"] = *input.SortOrder
	}

	if len(updates) > 0 {
		result := DB.WithContext(ctx).Model(&models.PaymentProvider{}).Where("id = ?", id).Updates(updates)
		if result.Error != nil {
			return PaymentProviderRecord{}, result.Error
		}
		if result.RowsAffected == 0 {
			return PaymentProviderRecord{}, errors.New("payment provider not found")
		}
	}

	var row models.PaymentProvider
	if err := DB.WithContext(ctx).First(&row, id).Error; err != nil {
		return PaymentProviderRecord{}, err
	}
	return paymentProviderRecordFromRow(row), nil
}

// EnablePaymentProvider mirrors enablePaymentProvider (returns the raw row, as the
// TS version does not decrypt it).
func EnablePaymentProvider(ctx context.Context, id int32) (models.PaymentProvider, error) {
	return paymentProviderSetStatus(ctx, id, models.PaymentProviderStatusActive)
}

// DisablePaymentProvider mirrors disablePaymentProvider.
func DisablePaymentProvider(ctx context.Context, id int32) (models.PaymentProvider, error) {
	return paymentProviderSetStatus(ctx, id, models.PaymentProviderStatusDisabled)
}

func paymentProviderSetStatus(ctx context.Context, id int32, status models.PaymentProviderStatus) (models.PaymentProvider, error) {
	result := DB.WithContext(ctx).Model(&models.PaymentProvider{}).Where("id = ?", id).Update("status", string(status))
	if result.Error != nil {
		return models.PaymentProvider{}, result.Error
	}
	if result.RowsAffected == 0 {
		return models.PaymentProvider{}, errors.New("payment provider not found")
	}
	var row models.PaymentProvider
	if err := DB.WithContext(ctx).First(&row, id).Error; err != nil {
		return models.PaymentProvider{}, err
	}
	return row, nil
}

// DeletePaymentProvider mirrors deletePaymentProvider.
func DeletePaymentProvider(ctx context.Context, id int32) error {
	result := DB.WithContext(ctx).Where("id = ?", id).Delete(&models.PaymentProvider{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("payment provider not found")
	}
	return nil
}

// ==================== fee helpers ====================

// CalculateFee mirrors calculateFee.
func CalculateFee(provider PaymentProviderRecord, amount float64) float64 {
	return paymentProviderToFixed(amount*provider.FeeRate+provider.FeeFixed, 2)
}

func normalizeMethodFeeEntry(entry any) PaymentMethodFeeConfig {
	if number, ok := entry.(float64); ok {
		if !math.IsNaN(number) && !math.IsInf(number, 0) && number > 0 {
			return PaymentMethodFeeConfig{FeeRate: number}
		}
		return PaymentMethodFeeConfig{}
	}
	if number, ok := entry.(json.Number); ok {
		parsed, err := number.Float64()
		if err == nil && parsed > 0 {
			return PaymentMethodFeeConfig{FeeRate: parsed}
		}
		return PaymentMethodFeeConfig{}
	}
	object, ok := entry.(map[string]any)
	if !ok {
		return PaymentMethodFeeConfig{}
	}
	feeRate := paymentProviderToNumber(object["feeRate"])
	if object["feeRate"] == nil {
		feeRate = 0
	}
	feeFixed := paymentProviderToNumber(object["feeFixed"])
	if object["feeFixed"] == nil {
		feeFixed = 0
	}
	normalized := PaymentMethodFeeConfig{}
	if !math.IsNaN(feeRate) && feeRate > 0 && feeRate <= 1 {
		normalized.FeeRate = feeRate
	}
	if !math.IsNaN(feeFixed) && feeFixed > 0 {
		normalized.FeeFixed = feeFixed
	}
	return normalized
}

// GetPaymentMethodFeeMap mirrors getPaymentMethodFeeMap.
func GetPaymentMethodFeeMap(provider PaymentProviderRecord) PaymentMethodFeeMap {
	result := PaymentMethodFeeMap{}
	rawMethodFees, ok := provider.Config["methodFees"].(map[string]any)
	if !ok {
		return result
	}
	for method, entry := range rawMethodFees {
		normalizedMethod := strings.TrimSpace(method)
		if normalizedMethod == "" {
			continue
		}
		result[normalizedMethod] = normalizeMethodFeeEntry(entry)
	}
	return result
}

// GetPaymentFeeConfig mirrors getPaymentFeeConfig.
func GetPaymentFeeConfig(provider PaymentProviderRecord, paymentMethod *string) PaymentMethodFeeConfig {
	method := ""
	if provider.Type == models.PaymentProviderTypeYipay && paymentMethod != nil {
		method = strings.TrimSpace(*paymentMethod)
	}
	if method != "" {
		if methodFee, ok := GetPaymentMethodFeeMap(provider)[method]; ok {
			return methodFee
		}
	}
	feeRate := provider.FeeRate
	if math.IsNaN(feeRate) {
		feeRate = 0
	}
	feeFixed := provider.FeeFixed
	if math.IsNaN(feeFixed) {
		feeFixed = 0
	}
	return PaymentMethodFeeConfig{FeeRate: feeRate, FeeFixed: feeFixed}
}

func isSurchargeFeeProvider(provider PaymentProviderRecord) bool {
	return provider.Type == models.PaymentProviderTypeYipay
}

// CalculatePaymentFee mirrors calculatePaymentFee.
func CalculatePaymentFee(provider PaymentProviderRecord, amount float64, paymentMethod *string) float64 {
	feeConfig := GetPaymentFeeConfig(provider, paymentMethod)
	return paymentProviderToFixed(amount*feeConfig.FeeRate+feeConfig.FeeFixed, 2)
}

// CalculatePayableAmount mirrors calculatePayableAmount.
func CalculatePayableAmount(provider PaymentProviderRecord, amount float64, paymentMethod *string) float64 {
	if !isSurchargeFeeProvider(provider) {
		return paymentProviderToFixed(amount, 2)
	}
	fee := CalculatePaymentFee(provider, amount, paymentMethod)
	return paymentProviderToFixed(amount+fee, 2)
}

// CalculateActualAmount mirrors calculateActualAmount.
func CalculateActualAmount(provider PaymentProviderRecord, amount float64) float64 {
	if !isSurchargeFeeProvider(provider) {
		fee := CalculateFee(provider, amount)
		return paymentProviderToFixed(amount-fee, 2)
	}
	return paymentProviderToFixed(amount, 2)
}

// ValidateRechargeAmount mirrors validateRechargeAmount.
func ValidateRechargeAmount(provider PaymentProviderRecord, amount float64) (bool, string) {
	minAmount := provider.MinAmount
	maxAmount := provider.MaxAmount

	if amount < minAmount {
		return false, "充值金额不能低于 " + paymentProviderNumberString(minAmount) + " 元"
	}
	if maxAmount != nil && amount > *maxAmount {
		return false, "充值金额不能超过 " + paymentProviderNumberString(*maxAmount) + " 元"
	}
	return true, ""
}

// GetProviderMethods mirrors getProviderMethods(provider).
func GetProviderMethods(provider PaymentProviderRecord) []string {
	if provider.Methods == nil {
		return []string{}
	}
	return provider.Methods
}

// ==================== decimal helpers ====================

func decimalZero() models.Decimal { return models.Decimal{} }

func decimalOne() models.Decimal {
	value, _ := models.NewDecimalFromString("1")
	return value
}
