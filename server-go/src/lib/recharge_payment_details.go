// Port of server/src/lib/recharge-payment-details.ts.
//
// `recharge_records.payment_details` is a JSON column carrying the structured
// gateway state (invoice currency/amount, crypto network, Antom minor amount,
// payment attempts). Every read is defensive: the column may be NULL, an old
// object, or a JSON string, and a malformed value must degrade to "no details"
// rather than break a payment callback.
package lib

import (
	"encoding/json"
	"strings"

	"gorm.io/datatypes"
)

// RechargeFlexibleNumber mirrors the places where the TS applies `Number(x)` to
// a value that may arrive as a JSON number *or* a numeric string. Non-numeric
// input becomes JS NaN, which the TS then rejects via Number.isFinite; Valid
// false carries the same meaning.
type RechargeFlexibleNumber struct {
	Value float64
	Valid bool
}

// UnmarshalJSON accepts numbers, numeric strings and null.
func (n *RechargeFlexibleNumber) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "" || text == "null" {
		n.Valid = false
		return nil
	}
	var number float64
	if err := json.Unmarshal(data, &number); err == nil {
		n.Value = number
		n.Valid = true
		return nil
	}
	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		if parsed, ok := jsParseFloatPrefix(strings.TrimSpace(asString)); ok {
			n.Value = parsed
			n.Valid = true
			return nil
		}
	}
	n.Valid = false
	return nil
}

// MarshalJSON writes the numeric value (the TS always stores numbers here).
func (n RechargeFlexibleNumber) MarshalJSON() ([]byte, error) {
	if !n.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(n.Value)
}

// NewRechargeNumber builds a valid flexible number.
func NewRechargeNumber(value float64) *RechargeFlexibleNumber {
	return &RechargeFlexibleNumber{Value: value, Valid: true}
}

// RechargeAmountDetails mirrors RechargePaymentDetails['recharge'].
type RechargeAmountDetails struct {
	Amount        *RechargeFlexibleNumber `json:"amount,omitempty"`
	PayableAmount *RechargeFlexibleNumber `json:"payableAmount,omitempty"`
	Fee           *RechargeFlexibleNumber `json:"fee,omitempty"`
	FeeRate       *RechargeFlexibleNumber `json:"feeRate,omitempty"`
	FeeFixed      *RechargeFlexibleNumber `json:"feeFixed,omitempty"`
	FeeMode       *string                 `json:"feeMode,omitempty"`
	PaymentMethod *string                 `json:"paymentMethod,omitempty"`
}

// RechargeManualDetails mirrors RechargePaymentDetails['manual'].
type RechargeManualDetails struct {
	Instructions *string `json:"instructions,omitempty"`
}

// RechargeHeleketDetails mirrors RechargePaymentDetails['heleket'].
type RechargeHeleketDetails struct {
	OrderID           *string                 `json:"orderId,omitempty"`
	InvoiceCurrency   *string                 `json:"invoiceCurrency,omitempty"`
	InvoiceAmount     *RechargeFlexibleNumber `json:"invoiceAmount,omitempty"`
	LifetimeSeconds   *RechargeFlexibleNumber `json:"lifetimeSeconds,omitempty"`
	Status            *string                 `json:"status,omitempty"`
	StatusDescription *string                 `json:"statusDescription,omitempty"`
	UUID              *string                 `json:"uuid,omitempty"`
	TxID              *string                 `json:"txid,omitempty"`
	PayerCurrency     *string                 `json:"payerCurrency,omitempty"`
	Network           *string                 `json:"network,omitempty"`
	PaymentMethodCode *string                 `json:"paymentMethodCode,omitempty"`
	PaymentAmount     *string                 `json:"paymentAmount,omitempty"`
	PaymentAmountUSD  *string                 `json:"paymentAmountUsd,omitempty"`
	MerchantAmount    *string                 `json:"merchantAmount,omitempty"`
	Address           *string                 `json:"address,omitempty"`
	From              *string                 `json:"from,omitempty"`
}

// RechargeAntomDetails mirrors RechargePaymentDetails['antom'].
type RechargeAntomDetails struct {
	PaymentRequestID  *string                 `json:"paymentRequestId,omitempty"`
	PaymentID         *string                 `json:"paymentId,omitempty"`
	Currency          *string                 `json:"currency,omitempty"`
	AmountMinor       *string                 `json:"amountMinor,omitempty"`
	CurrencyRate      *RechargeFlexibleNumber `json:"currencyRate,omitempty"`
	CurrencyExponent  *RechargeFlexibleNumber `json:"currencyExponent,omitempty"`
	Status            *string                 `json:"status,omitempty"`
	StatusDescription *string                 `json:"statusDescription,omitempty"`
}

// RechargePaymentAttempt mirrors one gateway payment attempt.
type RechargePaymentAttempt struct {
	ID             string `json:"id"`
	GatewayOrderNo string `json:"gatewayOrderNo"`
	CreatedAt      string `json:"createdAt"`
}

// RechargeSupersededPaymentAttempt adds the invalidation timestamp.
type RechargeSupersededPaymentAttempt struct {
	RechargePaymentAttempt
	SupersededAt string `json:"supersededAt"`
}

// RechargePaymentAttemptState mirrors RechargePaymentAttemptState.
type RechargePaymentAttemptState struct {
	Current    RechargePaymentAttempt             `json:"current"`
	Superseded []RechargeSupersededPaymentAttempt `json:"superseded"`
}

// RechargePaymentDetails mirrors RechargePaymentDetails.
type RechargePaymentDetails struct {
	Kind           *string                      `json:"kind,omitempty"`
	Recharge       *RechargeAmountDetails       `json:"recharge,omitempty"`
	Manual         *RechargeManualDetails       `json:"manual,omitempty"`
	Heleket        *RechargeHeleketDetails      `json:"heleket,omitempty"`
	Antom          *RechargeAntomDetails        `json:"antom,omitempty"`
	PaymentAttempt *RechargePaymentAttemptState `json:"paymentAttempt,omitempty"`
	// Extra carries top-level keys this port does not model (forward
	// compatibility with gateway/plugin supplied metadata).
	Extra map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes the known keys and keeps unknown ones in Extra.
func (d *RechargePaymentDetails) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	decoded := RechargePaymentDetails{}
	if value, ok := raw["kind"]; ok {
		var kind string
		if err := json.Unmarshal(value, &kind); err == nil {
			decoded.Kind = &kind
		}
	}
	if value, ok := raw["recharge"]; ok {
		var recharge RechargeAmountDetails
		if err := json.Unmarshal(value, &recharge); err == nil {
			decoded.Recharge = &recharge
		}
	}
	if value, ok := raw["manual"]; ok {
		var manual RechargeManualDetails
		if err := json.Unmarshal(value, &manual); err == nil {
			decoded.Manual = &manual
		}
	}
	if value, ok := raw["heleket"]; ok {
		var heleket RechargeHeleketDetails
		if err := json.Unmarshal(value, &heleket); err == nil {
			decoded.Heleket = &heleket
		}
	}
	if value, ok := raw["antom"]; ok {
		var antom RechargeAntomDetails
		if err := json.Unmarshal(value, &antom); err == nil {
			decoded.Antom = &antom
		}
	}
	if value, ok := raw["paymentAttempt"]; ok {
		var attempt RechargePaymentAttemptState
		if err := json.Unmarshal(value, &attempt); err == nil {
			decoded.PaymentAttempt = &attempt
		}
	}
	for key, value := range raw {
		switch key {
		case "kind", "recharge", "manual", "heleket", "antom", "paymentAttempt":
			continue
		}
		if decoded.Extra == nil {
			decoded.Extra = map[string]json.RawMessage{}
		}
		decoded.Extra[key] = value
	}
	*d = decoded
	return nil
}

// MarshalJSON writes the known keys plus Extra.
func (d RechargePaymentDetails) MarshalJSON() ([]byte, error) {
	object := map[string]any{}
	type plain RechargePaymentDetails
	encoded, err := json.Marshal(plain(d))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(encoded, &object); err != nil {
		return nil, err
	}
	delete(object, "Extra")
	for key, value := range d.Extra {
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			continue
		}
		object[key] = decoded
	}
	return json.Marshal(object)
}

// RechargePaymentDisplayInfo mirrors RechargePaymentDisplayInfo.
type RechargePaymentDisplayInfo struct {
	InvoiceCurrency          *string `json:"invoiceCurrency"`
	PaymentCurrency          *string `json:"paymentCurrency"`
	PaymentNetwork           *string `json:"paymentNetwork"`
	ActualPaymentMethod      *string `json:"actualPaymentMethod"`
	PaymentUUID              *string `json:"paymentUuid"`
	PaymentTxID              *string `json:"paymentTxid"`
	GatewayStatus            *string `json:"gatewayStatus"`
	GatewayStatusDescription *string `json:"gatewayStatusDescription"`
}

// ReadRechargePaymentDetails mirrors readRechargePaymentDetails. Non-object
// inputs (including JSON strings) yield an empty struct, exactly like the TS.
func ReadRechargePaymentDetails(value any) RechargePaymentDetails {
	switch typed := value.(type) {
	case RechargePaymentDetails:
		return typed
	case *RechargePaymentDetails:
		if typed == nil {
			return RechargePaymentDetails{}
		}
		return *typed
	case datatypes.JSON:
		return rechargeDetailsFromJSONBytes(typed)
	case json.RawMessage:
		return rechargeDetailsFromJSONBytes(typed)
	case []byte:
		return rechargeDetailsFromJSONBytes(typed)
	case map[string]any:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return RechargePaymentDetails{}
		}
		return rechargeDetailsFromJSONBytes(encoded)
	default:
		return RechargePaymentDetails{}
	}
}

func rechargeDetailsFromJSONBytes(raw []byte) RechargePaymentDetails {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || !strings.HasPrefix(trimmed, "{") {
		return RechargePaymentDetails{}
	}
	var details RechargePaymentDetails
	if err := json.Unmarshal([]byte(trimmed), &details); err != nil {
		return RechargePaymentDetails{}
	}
	return details
}

// BuildManualRechargePaymentDetails mirrors buildManualRechargePaymentDetails.
func BuildManualRechargePaymentDetails(instructions string) RechargePaymentDetails {
	kind := "manual"
	return RechargePaymentDetails{
		Kind:   &kind,
		Manual: &RechargeManualDetails{Instructions: &instructions},
	}
}

// BuildHeleketInvoicePaymentDetails mirrors buildHeleketInvoicePaymentDetails.
func BuildHeleketInvoicePaymentDetails(orderNo string, amount float64, config HeleketConfig) RechargePaymentDetails {
	kind := "heleket"
	currency := config.Currency
	if currency == "" {
		currency = "CNY"
	}
	return RechargePaymentDetails{
		Kind: &kind,
		Heleket: &RechargeHeleketDetails{
			OrderID:         &orderNo,
			InvoiceCurrency: &currency,
			InvoiceAmount:   NewRechargeNumber(amount),
			LifetimeSeconds: NewRechargeNumber(float64(config.LifetimeSeconds)),
		},
	}
}

// BuildAntomPaymentDetails mirrors buildAntomPaymentDetails. The minor amount
// calculation can fail, which the TS expresses as a thrown error.
func BuildAntomPaymentDetails(orderNo string, amountCNY float64, config AntomConfig) (RechargePaymentDetails, error) {
	minorAmount, err := CalculateAntomMinorAmount(amountCNY, config)
	if err != nil {
		return RechargePaymentDetails{}, err
	}
	kind := "antom"
	status := "PENDING"
	currency := config.Currency
	return RechargePaymentDetails{
		Kind: &kind,
		Antom: &RechargeAntomDetails{
			PaymentRequestID: &orderNo,
			Currency:         &currency,
			AmountMinor:      &minorAmount,
			CurrencyRate:     NewRechargeNumber(config.CurrencyRate),
			CurrencyExponent: NewRechargeNumber(float64(config.CurrencyExponent)),
			Status:           &status,
		},
	}, nil
}

// RechargeAmountMergeInput mirrors the input of mergeRechargeAmountDetails.
type RechargeAmountMergeInput struct {
	Amount        float64
	PayableAmount float64
	Fee           float64
	FeeRate       float64
	FeeFixed      float64
	FeeMode       *string
	PaymentMethod *string
}

// MergeRechargeAmountDetails mirrors mergeRechargeAmountDetails.
func MergeRechargeAmountDetails(existingValue any, input RechargeAmountMergeInput) RechargePaymentDetails {
	existing := ReadRechargePaymentDetails(existingValue)
	recharge := &RechargeAmountDetails{}
	if existing.Recharge != nil {
		copied := *existing.Recharge
		recharge = &copied
	}
	recharge.Amount = NewRechargeNumber(input.Amount)
	recharge.PayableAmount = NewRechargeNumber(input.PayableAmount)
	recharge.Fee = NewRechargeNumber(input.Fee)
	recharge.FeeRate = NewRechargeNumber(input.FeeRate)
	recharge.FeeFixed = NewRechargeNumber(input.FeeFixed)
	feeMode := "surcharge"
	if input.FeeMode != nil && *input.FeeMode != "" {
		feeMode = *input.FeeMode
	}
	recharge.FeeMode = &feeMode
	if input.PaymentMethod != nil && *input.PaymentMethod != "" {
		method := *input.PaymentMethod
		recharge.PaymentMethod = &method
	} else {
		recharge.PaymentMethod = nil
	}
	existing.Recharge = recharge
	return existing
}

// RechargePayableSource mirrors the record shape getRechargePayableAmount reads.
type RechargePayableSource struct {
	Amount         float64
	PaymentDetails any
}

// GetRechargePayableAmount mirrors getRechargePayableAmount.
func GetRechargePayableAmount(record RechargePayableSource) float64 {
	details := ReadRechargePaymentDetails(record.PaymentDetails)
	if details.Recharge != nil && details.Recharge.PayableAmount != nil && details.Recharge.PayableAmount.Valid {
		amount := details.Recharge.PayableAmount.Value
		if amount > 0 {
			return amount
		}
	}
	return record.Amount
}

// RechargeHeleketMergeFallback mirrors the optional fallback argument.
type RechargeHeleketMergeFallback struct {
	OrderNo       *string
	InvoiceAmount *float64
}

// MergeHeleketPaymentDetails mirrors mergeHeleketPaymentDetails.
func MergeHeleketPaymentDetails(
	existingValue any,
	payment map[string]any,
	config *HeleketConfig,
	fallback *RechargeHeleketMergeFallback,
) RechargePaymentDetails {
	existing := ReadRechargePaymentDetails(existingValue)
	details := ExtractHeleketPaymentDetails(payment)
	invoiceAmount := GetHeleketInvoiceAmount(payment)

	merged := &RechargeHeleketDetails{}
	if existing.Heleket != nil {
		copied := *existing.Heleket
		merged = &copied
	}
	switch {
	case fallback != nil && fallback.OrderNo != nil && *fallback.OrderNo != "":
		orderNo := *fallback.OrderNo
		merged.OrderID = &orderNo
	case merged.OrderID != nil:
	default:
		merged.OrderID = nil
	}
	if config != nil && config.Currency != "" {
		currency := config.Currency
		merged.InvoiceCurrency = &currency
	}
	if invoiceAmount != nil {
		merged.InvoiceAmount = NewRechargeNumber(*invoiceAmount)
	} else if fallback != nil && fallback.InvoiceAmount != nil {
		merged.InvoiceAmount = NewRechargeNumber(*fallback.InvoiceAmount)
	}
	if config != nil {
		merged.LifetimeSeconds = NewRechargeNumber(float64(config.LifetimeSeconds))
	}
	merged.Status = rechargeStringPointer(details.Status)
	merged.StatusDescription = &details.StatusDescription
	merged.UUID = details.UUID
	merged.TxID = details.TxID
	merged.PayerCurrency = details.PayerCurrency
	merged.Network = details.Network
	merged.PaymentMethodCode = details.PaymentMethodCode
	merged.PaymentAmount = details.PaymentAmount
	merged.PaymentAmountUSD = details.PaymentAmountUSD
	merged.MerchantAmount = details.MerchantAmount
	merged.Address = details.Address
	merged.From = details.From

	kind := "heleket"
	existing.Kind = &kind
	existing.Heleket = merged
	return existing
}

// RechargeAntomMergeFallback mirrors the mandatory fallback argument.
type RechargeAntomMergeFallback struct {
	OrderNo   string
	AmountCNY float64
}

// MergeAntomPaymentDetails mirrors mergeAntomPaymentDetails.
func MergeAntomPaymentDetails(
	existingValue any,
	payment map[string]any,
	config AntomConfig,
	fallback RechargeAntomMergeFallback,
) RechargePaymentDetails {
	existing := ReadRechargePaymentDetails(existingValue)
	amount := rechargeAmountObject(payment["actualPaymentAmount"])
	if amount == nil {
		amount = rechargeAmountObject(payment["paymentAmount"])
	}

	var status *string
	var statusDescription *string
	if _, hasPaymentStatus := payment["paymentStatus"]; hasPaymentStatus {
		if value, ok := payment["paymentStatus"].(string); ok && value != "" {
			status = &value
		}
		if value, ok := payment["paymentResultMessage"].(string); ok && value != "" {
			statusDescription = &value
		}
		if statusDescription == nil {
			if resultStatus := rechargeResultMessage(payment["result"]); resultStatus != "" {
				statusDescription = &resultStatus
			}
		}
	} else {
		if resultStatus := rechargeResultStatus(payment["result"]); resultStatus != "" {
			status = &resultStatus
		}
		if resultMessage := rechargeResultMessage(payment["result"]); resultMessage != "" {
			statusDescription = &resultMessage
		}
	}

	merged := &RechargeAntomDetails{}
	if existing.Antom != nil {
		copied := *existing.Antom
		merged = &copied
	}

	paymentRequestID := rechargeStringValue(payment["paymentRequestId"])
	if paymentRequestID == "" && merged.PaymentRequestID != nil {
		paymentRequestID = *merged.PaymentRequestID
	}
	if paymentRequestID == "" {
		paymentRequestID = fallback.OrderNo
	}
	merged.PaymentRequestID = &paymentRequestID

	paymentID := rechargeStringValue(payment["paymentId"])
	if paymentID == "" && merged.PaymentID != nil {
		paymentID = *merged.PaymentID
	}
	if paymentID == "" {
		merged.PaymentID = nil
	} else {
		merged.PaymentID = &paymentID
	}

	currency := ""
	if amount != nil {
		currency = rechargeStringValue(amount["currency"])
	}
	if currency == "" && merged.Currency != nil {
		currency = *merged.Currency
	}
	if currency == "" {
		currency = config.Currency
	}
	merged.Currency = &currency

	amountMinor := ""
	if amount != nil {
		amountMinor = rechargeStringValue(amount["value"])
	}
	if amountMinor == "" && merged.AmountMinor != nil {
		amountMinor = *merged.AmountMinor
	}
	if amountMinor == "" {
		if computed, err := CalculateAntomMinorAmount(fallback.AmountCNY, config); err == nil {
			amountMinor = computed
		}
	}
	if amountMinor == "" {
		merged.AmountMinor = nil
	} else {
		merged.AmountMinor = &amountMinor
	}

	if merged.CurrencyRate == nil {
		merged.CurrencyRate = NewRechargeNumber(config.CurrencyRate)
	}
	if merged.CurrencyExponent == nil {
		merged.CurrencyExponent = NewRechargeNumber(float64(config.CurrencyExponent))
	}
	merged.Status = status
	merged.StatusDescription = statusDescription

	kind := "antom"
	existing.Kind = &kind
	existing.Antom = merged
	return existing
}

// ExtractRechargePaymentDisplayInfo mirrors extractRechargePaymentDisplayInfo.
func ExtractRechargePaymentDisplayInfo(value any) RechargePaymentDisplayInfo {
	details := ReadRechargePaymentDetails(value)
	var info RechargePaymentDisplayInfo
	if details.Heleket != nil {
		info.InvoiceCurrency = details.Heleket.InvoiceCurrency
		info.PaymentCurrency = details.Heleket.PayerCurrency
		info.PaymentNetwork = details.Heleket.Network
		info.ActualPaymentMethod = details.Heleket.PaymentMethodCode
		info.PaymentUUID = details.Heleket.UUID
		info.PaymentTxID = details.Heleket.TxID
		info.GatewayStatus = details.Heleket.Status
		info.GatewayStatusDescription = details.Heleket.StatusDescription
	}
	if details.Antom != nil {
		if info.InvoiceCurrency == nil {
			info.InvoiceCurrency = details.Antom.Currency
		}
		if info.PaymentCurrency == nil {
			info.PaymentCurrency = details.Antom.Currency
		}
		if info.PaymentUUID == nil {
			info.PaymentUUID = details.Antom.PaymentID
		}
		if info.GatewayStatus == nil {
			info.GatewayStatus = details.Antom.Status
		}
		if info.GatewayStatusDescription == nil {
			info.GatewayStatusDescription = details.Antom.StatusDescription
		}
	}
	return info
}

// ==================== small helpers ====================

func rechargeStringValue(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}

func rechargeStringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func rechargeAmountObject(value any) map[string]any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return object
}

func rechargeResultStatus(value any) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	return rechargeStringValue(object["resultStatus"])
}

func rechargeResultMessage(value any) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	return rechargeStringValue(object["resultMessage"])
}
