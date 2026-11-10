// Port of server/src/lib/recharge-order-guard.ts.
//
// Re-payment creates gateway order numbers of the form "<orderNo>--PA-<12 hex>";
// these helpers normalise and compare them so a callback for a superseded payment
// attempt can never credit the order.
package lib

import "strings"

// NormalizeRechargeGatewayOrderNo mirrors normalizeRechargeGatewayOrderNo.
func NormalizeRechargeGatewayOrderNo(value any) *string {
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

// IsRechargeGatewayOrderNoMatch mirrors isRechargeGatewayOrderNoMatch.
func IsRechargeGatewayOrderNoMatch(expectedOrderNo string, gatewayOrderNo any) bool {
	normalized := NormalizeRechargeGatewayOrderNo(gatewayOrderNo)
	return normalized != nil && *normalized == expectedOrderNo
}
