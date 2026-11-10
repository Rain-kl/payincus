package db

import (
	"encoding/json"
	"testing"
)

// The five sensitive keys and the mask placeholder are a wire contract: the admin
// UI compares the placeholder to decide whether to send a new secret or keep the
// stored one. Masking must be byte-identical to server/src/db/system-config.ts.
func TestSystemConfigMaskValue(t *testing.T) {
	sensitive := []string{
		"smtp_password",
		"turnstile_secret_key",
		"ticket_image_lsky_token",
		"telegram_bot_token",
		"telegram_webhook_secret",
	}
	for _, key := range sensitive {
		if !SystemConfigIsSensitiveKey(key) {
			t.Fatalf("%s should be sensitive", key)
		}
		if got := SystemConfigMaskValue(key, "plaintext"); got != "********" {
			t.Errorf("mask(%s) = %q, want ********", key, got)
		}
		// An empty secret stays empty: getAllSystemConfigs only masks truthy values.
		if got := SystemConfigMaskValue(key, ""); got != "" {
			t.Errorf("mask(%s, \"\") = %q, want empty", key, got)
		}
		// Idempotent, so the route can re-apply the rule on the way out.
		if got := SystemConfigMaskValue(key, "********"); got != "********" {
			t.Errorf("mask(%s, placeholder) = %q, want ********", key, got)
		}
	}

	for _, key := range []string{
		"brand_name",
		"smtp_host",
		"smtp_username",
		"turnstile_site_key",
		"telegram_bot_username",
		"email_allowed_domains",
	} {
		if SystemConfigIsSensitiveKey(key) {
			t.Errorf("%s must not be sensitive", key)
		}
		if got := SystemConfigMaskValue(key, "visible"); got != "visible" {
			t.Errorf("mask(%s) = %q, want visible", key, got)
		}
	}
}

// serializeInviteCostOptions is JSON.stringify(normalizeInviteCostOptions(...)).
// The expected strings are the exact bytes the Node route stores.
func TestSystemConfigSerializeInviteCostOptions(t *testing.T) {
	cases := []struct {
		name    string
		options []UserInvitesCostOption
		want    string
	}{
		{
			name:    "defaults are appended when missing",
			options: []UserInvitesCostOption{},
			want:    `[{"resource":"balance","amount":0,"enabled":false},{"resource":"points","amount":0,"enabled":false}]`,
		},
		{
			name: "only points configured",
			options: []UserInvitesCostOption{
				{Resource: "points", Amount: 5, Enabled: true},
			},
			want: `[{"resource":"points","amount":5,"enabled":true},{"resource":"balance","amount":0,"enabled":false}]`,
		},
		{
			name: "balance is rounded to cents",
			options: []UserInvitesCostOption{
				{Resource: "balance", Amount: 1.239, Enabled: true},
			},
			want: `[{"resource":"balance","amount":1.24,"enabled":true},{"resource":"points","amount":0,"enabled":false}]`,
		},
		{
			name: "points are floored",
			options: []UserInvitesCostOption{
				{Resource: "points", Amount: 2.9, Enabled: true},
			},
			want: `[{"resource":"points","amount":2,"enabled":true},{"resource":"balance","amount":0,"enabled":false}]`,
		},
		{
			name: "order follows first appearance, duplicates keep the last value",
			options: []UserInvitesCostOption{
				{Resource: "points", Amount: 1, Enabled: false},
				{Resource: "balance", Amount: 2, Enabled: false},
				{Resource: "points", Amount: 3, Enabled: true},
			},
			want: `[{"resource":"points","amount":3,"enabled":true},{"resource":"balance","amount":2,"enabled":false}]`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := SystemConfigSerializeInviteCostOptions(testCase.options)
			if err != nil {
				t.Fatalf("serialize: %v", err)
			}
			if got != testCase.want {
				t.Fatalf("serialize = %s, want %s", got, testCase.want)
			}
			// The stored text must be valid JSON in the same shape.
			var decoded []map[string]any
			if err := json.Unmarshal([]byte(got), &decoded); err != nil {
				t.Fatalf("serialized value is not JSON: %v", err)
			}
			for _, entry := range decoded {
				for _, field := range []string{"resource", "amount", "enabled"} {
					if _, ok := entry[field]; !ok {
						t.Fatalf("serialized entry is missing %q: %v", field, entry)
					}
				}
			}
		})
	}
}

// Only balance and points have charge handlers; the route rejects everything else.
func TestSystemConfigIsSupportedInviteCostResource(t *testing.T) {
	for _, resource := range []string{"balance", "points"} {
		if !SystemConfigIsSupportedInviteCostResource(resource) {
			t.Errorf("%s should be supported", resource)
		}
	}
	for _, resource := range []string{"", "banana", "Balance", "wallet"} {
		if SystemConfigIsSupportedInviteCostResource(resource) {
			t.Errorf("%q should not be supported", resource)
		}
	}
}
