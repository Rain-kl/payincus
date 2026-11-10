// Additive exports used by the system-config route. Nothing in this file changes
// the behaviour of the existing db/system_config.go and db/user_invites.go
// functions; it only exposes two pieces of them that routes/system_config.go
// needs.
//
//  1. Secret masking. getAllSystemConfigs() already replaces every non-empty
//     sensitive value with "********" before serialising a config row. The route
//     re-applies the same rule on the way out (defense in depth, idempotent), and
//     SystemConfigMaskValue is the single definition both sides agree on.
//
//  2. Invite-cost pricing. The TypeScript route imports
//     normalizeInviteCostOptions / serializeInviteCostOptions /
//     isSupportedInviteCostResource from lib/invite-pricing.ts. Those behaviours
//     already exist here as the private helpers of db/user_invites.go (ported with
//     that module), so this file exposes them under the SystemConfig prefix
//     instead of duplicating the rules — a second copy could drift and silently
//     accept a cost option the invite module would reject at generation time.
package db

import (
	"encoding/json"
)

// ==================== secret masking ====================

// SystemConfigIsSensitiveKey reports whether a config key holds a secret
// (SENSITIVE_CONFIG_KEYS in the TypeScript).
func SystemConfigIsSensitiveKey(key string) bool {
	return sensitiveConfigKeys[key]
}

// SystemConfigMaskValue applies getAllSystemConfigs' masking rule to one value:
// a sensitive key with a non-empty value becomes the mask placeholder, everything
// else is returned unchanged. Masking an already-masked value is a no-op.
func SystemConfigMaskValue(key, value string) string {
	if sensitiveConfigKeys[key] && value != "" {
		return maskedSecretPlaceholder
	}
	return value
}

// ==================== invite-cost pricing ====================

// SystemConfigIsSupportedInviteCostResource mirrors isSupportedInviteCostResource:
// the resource must have a charge handler, i.e. balance or points.
func SystemConfigIsSupportedInviteCostResource(resource string) bool {
	_, _, ok := userInvitesCostLabel(resource)
	return ok
}

// SystemConfigNormalizeInviteCostOptions mirrors normalizeInviteCostOptions: the
// default options are appended for any missing resource, duplicates collapse to
// their first position with the last value, and each amount is rounded/floored by
// the per-resource handler.
func SystemConfigNormalizeInviteCostOptions(options []UserInvitesCostOption) []UserInvitesCostOption {
	return userInvitesNormalizeCostOptions(options)
}

// SystemConfigSerializeInviteCostOptions mirrors serializeInviteCostOptions:
// JSON.stringify(normalizeInviteCostOptions(options)). The field order of
// UserInvitesCostOption matches the TypeScript object literal, so the stored text
// is byte-identical. An error is only possible for non-finite amounts, which the
// HTTP input cannot produce (JSON has no NaN/Infinity).
func SystemConfigSerializeInviteCostOptions(options []UserInvitesCostOption) (string, error) {
	encoded, err := json.Marshal(userInvitesNormalizeCostOptions(options))
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
