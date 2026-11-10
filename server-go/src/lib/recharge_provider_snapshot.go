// Port of server/src/lib/recharge-provider-snapshot.ts.
//
// An order captures the provider configuration that existed when it was created
// (encrypted with the same AES-GCM helper as other sensitive data) so a later
// callback or re-payment is validated against the same gateway credentials, even
// if an admin rotated them in the meantime.
package lib

import (
	"encoding/json"
	"strings"
	"time"
)

// RechargeProviderConfigSnapshot mirrors the TS interface.
type RechargeProviderConfigSnapshot struct {
	Type       string         `json:"type"`
	Config     map[string]any `json:"config"`
	CapturedAt string         `json:"capturedAt"`
}

// BuildRechargeProviderConfigSnapshot mirrors buildRechargeProviderConfigSnapshot.
func BuildRechargeProviderConfigSnapshot(providerType string, config map[string]any) string {
	payload := map[string]any{
		"type":       providerType,
		"config":     config,
		"capturedAt": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	encrypted, err := EncryptSensitiveData(string(encoded))
	if err != nil {
		return ""
	}
	return encrypted
}

// ParseRechargeProviderConfigSnapshot mirrors parseRechargeProviderConfigSnapshot.
func ParseRechargeProviderConfigSnapshot(value any) *RechargeProviderConfigSnapshot {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return nil
	}
	decrypted := DecryptSensitiveData(text)
	if decrypted == "" {
		return nil
	}
	var parsed struct {
		Type       string          `json:"type"`
		Config     json.RawMessage `json:"config"`
		CapturedAt string          `json:"capturedAt"`
	}
	if err := json.Unmarshal([]byte(decrypted), &parsed); err != nil {
		return nil
	}
	trimmedConfig := strings.TrimSpace(string(parsed.Config))
	if parsed.Type == "" || trimmedConfig == "" || !strings.HasPrefix(trimmedConfig, "{") {
		return nil
	}
	var config map[string]any
	if err := json.Unmarshal(parsed.Config, &config); err != nil || config == nil {
		return nil
	}
	return &RechargeProviderConfigSnapshot{
		Type:       parsed.Type,
		Config:     config,
		CapturedAt: parsed.CapturedAt,
	}
}

// ResolveRechargeProviderConfigSnapshot mirrors resolveRechargeProviderConfigSnapshot.
func ResolveRechargeProviderConfigSnapshot(
	providerType string,
	currentConfig map[string]any,
	snapshotValue any,
) (map[string]any, string) {
	snapshot := ParseRechargeProviderConfigSnapshot(snapshotValue)
	if snapshot != nil && snapshot.Type == providerType {
		return snapshot.Config, "snapshot"
	}
	return currentConfig, "provider"
}
