package routes

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/services"
)

// decodeSystemConfigBody mirrors the handler's decode step (the shared Fastify
// body decoder) so the tests exercise the same JSON values the routes see.
func decodeSystemConfigBody(t *testing.T, raw string) any {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("test payload is not valid JSON: %v", err)
	}
	return decoded
}

// The PUT body schema is validated by AJV with @fastify/ajv-compiler's defaults
// (coerceTypes:'array', useDefaults:true, removeAdditional:true, allErrors:false).
// Every expectation below was measured against ajv@8.17.1 with exactly those
// options, then rendered through app.ts' setErrorHandler:
//
//	field = err.instancePath?.replace('/', '') || err.params.missingProperty || 'field'
//	type     -> err.message ("must be object" / "must be array" / "must be string")
//	required -> `${field} is required`
func TestSystemConfigValidateUpdateBody(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantMessage string
		wantPairs   []systemConfigUpdatePair
	}{
		{name: "empty object", body: `{}`, wantMessage: "configs is required"},
		{name: "root null", body: `null`, wantMessage: "must be object"},
		{name: "root array", body: `[]`, wantMessage: "must be object"},
		{name: "root string", body: `"x"`, wantMessage: "must be object"},
		{name: "root number", body: `5`, wantMessage: "must be object"},
		{name: "root bool", body: `true`, wantMessage: "must be object"},

		// coerceTypes:'array' wraps scalars (null included) into a one element array,
		// and the element then fails the items' object type test.
		{name: "configs null", body: `{"configs": null}`, wantMessage: "must be object"},
		{name: "configs number", body: `{"configs": 5}`, wantMessage: "must be object"},
		{name: "configs string", body: `{"configs": "abc"}`, wantMessage: "must be object"},
		// A JSON object is not coercible to an array.
		{name: "configs object", body: `{"configs": {}}`, wantMessage: "must be array"},
		{name: "configs empty array", body: `{"configs": []}`, wantPairs: []systemConfigUpdatePair{}},

		{name: "item empty", body: `{"configs": [{}]}`, wantMessage: "configs/0 is required"},
		{name: "item null", body: `{"configs": [null]}`, wantMessage: "must be object"},
		{name: "item number", body: `{"configs": [5]}`, wantMessage: "must be object"},
		{name: "item string", body: `{"configs": ["x"]}`, wantMessage: "must be object"},
		{name: "item nested array", body: `{"configs": [[{"key":"a","value":"b"}]]}`, wantMessage: "must be object"},
		// `required` is evaluated before the item's properties, so a missing value
		// wins over the key's type error.
		{name: "item missing value", body: `{"configs": [{"key": "a"}]}`, wantMessage: "configs/0 is required"},
		{name: "item missing key", body: `{"configs": [{"value": "b"}]}`, wantMessage: "configs/0 is required"},
		{name: "item key uncoercible", body: `{"configs": [{"key": {}, "value": "b"}]}`, wantMessage: "must be string"},
		{name: "item key multi array", body: `{"configs": [{"key": ["a","b"], "value": "b"}]}`, wantMessage: "must be string"},
		{name: "second item index", body: `{"configs": [{"key":"a","value":"b"},{"key":"c"}]}`, wantMessage: "configs/1 is required"},

		{
			name: "values are coerced to strings",
			body: `{"configs": [{"key": 1, "value": 2}]}`,
			wantPairs: []systemConfigUpdatePair{
				{Key: "1", Value: "2"},
			},
		},
		{
			name: "null value coerces to empty string",
			body: `{"configs": [{"key": true, "value": null}]}`,
			wantPairs: []systemConfigUpdatePair{
				{Key: "true", Value: ""},
			},
		},
		{
			name: "single element array unwraps",
			body: `{"configs": [{"key": ["brand_name"], "value": ["Incudal"]}]}`,
			wantPairs: []systemConfigUpdatePair{
				{Key: "brand_name", Value: "Incudal"},
			},
		},
		{
			// removeAdditional only strips keys when the schema sets
			// additionalProperties:false, which it does not: unknown keys survive.
			name: "unknown keys survive but are ignored",
			body: `{"configs": [{"key": "brand_name", "value": "x", "extra": 1}], "extraTop": true}`,
			wantPairs: []systemConfigUpdatePair{
				{Key: "brand_name", Value: "x"},
			},
		},
		{
			name: "duplicates keep document order",
			body: `{"configs": [{"key":"brand_name","value":"a"},{"key":"brand_name","value":"b"}]}`,
			wantPairs: []systemConfigUpdatePair{
				{Key: "brand_name", Value: "a"},
				{Key: "brand_name", Value: "b"},
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			pairs, message := systemConfigValidateUpdateBody(decodeSystemConfigBody(t, testCase.body))
			if message != testCase.wantMessage {
				t.Fatalf("message = %q, want %q", message, testCase.wantMessage)
			}
			if testCase.wantMessage != "" {
				if pairs != nil {
					t.Fatalf("pairs = %v, want nil on validation failure", pairs)
				}
				return
			}
			if len(pairs) != len(testCase.wantPairs) {
				t.Fatalf("pairs = %v, want %v", pairs, testCase.wantPairs)
			}
			for index := range pairs {
				if pairs[index] != testCase.wantPairs[index] {
					t.Fatalf("pairs[%d] = %v, want %v", index, pairs[index], testCase.wantPairs[index])
				}
			}
		})
	}
}

// The send-test schema is {to: required, type string, format email}. app.ts maps
// the `format: email` keyword to its own message, and `type` to AJV's message.
func TestSystemConfigValidateSendTestBody(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantTo      string
		wantMessage string
	}{
		{name: "missing to", body: `{}`, wantMessage: "to is required"},
		{name: "root null", body: `null`, wantMessage: "must be object"},
		{name: "empty string", body: `{"to": ""}`, wantMessage: "Please enter a valid email address"},
		{name: "not an email", body: `{"to": "not-an-email"}`, wantMessage: "Please enter a valid email address"},
		{name: "number coerces then fails format", body: `{"to": 123}`, wantMessage: "Please enter a valid email address"},
		{name: "null coerces to empty then fails format", body: `{"to": null}`, wantMessage: "Please enter a valid email address"},
		{name: "object is not coercible", body: `{"to": {"a": 1}}`, wantMessage: "must be string"},
		{name: "multi value array", body: `{"to": ["x","y"]}`, wantMessage: "must be string"},
		{name: "padded address is not trimmed", body: `{"to": "  a@b.com  "}`, wantMessage: "Please enter a valid email address"},
		// ajv-formats' "full" mode is what @fastify/ajv-compiler installs; it requires
		// a dot in the domain and no consecutive dots in the local part.
		{name: "domain without dot", body: `{"to": "a@b"}`, wantMessage: "Please enter a valid email address"},
		{name: "double dot local part", body: `{"to": "a..b@example.com"}`, wantMessage: "Please enter a valid email address"},
		{name: "valid address", body: `{"to": "a@b.com"}`, wantTo: "a@b.com"},
		{name: "uppercase valid address", body: `{"to": "A@B.COM"}`, wantTo: "A@B.COM"},
		{name: "single element array unwraps", body: `{"to": ["a@b.com"]}`, wantTo: "a@b.com"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			to, message := systemConfigValidateSendTestBody(decodeSystemConfigBody(t, testCase.body))
			if message != testCase.wantMessage {
				t.Fatalf("message = %q, want %q", message, testCase.wantMessage)
			}
			if message == "" && to != testCase.wantTo {
				t.Fatalf("to = %q, want %q", to, testCase.wantTo)
			}
		})
	}
}

// isHttpImageUrl was verified against the live Node runtime (new URL + protocol
// check) for every input below; the WHATWG parser accepts more spellings than
// RFC 3986 (no slash, one slash, three slashes, backslashes) and rejects an
// invalid port or an empty host.
func TestSystemConfigIsHTTPImageURL(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"http://example.com", true},
		{"https://example.com/a.png", true},
		{"HTTP://EXAMPLE.COM/x", true},
		{" http://example.com ", true},
		{"http:///path", true},
		{"https:example.com", true},
		{"http:/example.com", true},
		{"http://user:pass@example.com/x", true},
		{"http://[::1]:8080/x", true},
		{"https://例え.jp/x", true},
		{`https://example.com\x`, true},

		{"http://", false},
		{"/logo.svg", false},
		{"logo.svg", false},
		{"", false},
		{"http://exa mple.com", false},
		{"ftp://example.com/x", false},
		{"javascript:alert(1)", false},
		{"http://example.com:abc/x", false},
		{"http://?x", false},
		{"http://#x", false},
		{"http://example.com:99999/x", false},
	}

	for _, testCase := range cases {
		if got := systemConfigIsHTTPImageURL(testCase.value); got != testCase.want {
			t.Errorf("isHttpImageURL(%q) = %v, want %v", testCase.value, got, testCase.want)
		}
	}
}

// GET / must never serialise a secret. db.GetAllSystemConfigs already masks; this
// pass is idempotent and independently asserted so a future projection change
// cannot leak a stored secret to the admin UI.
func TestSystemConfigMaskItemsHidesSecrets(t *testing.T) {
	secretKeys := []string{
		"smtp_password",
		"turnstile_secret_key",
		"ticket_image_lsky_token",
		"telegram_bot_token",
		"telegram_webhook_secret",
	}

	items := make([]db.SystemConfigItem, 0, len(secretKeys)+3)
	for _, key := range secretKeys {
		items = append(items, db.SystemConfigItem{ID: 1, Key: key, Value: "plaintext-" + key, Type: "secret"})
	}
	items = append(items,
		db.SystemConfigItem{ID: 2, Key: "smtp_password_empty", Value: "", Type: "secret"},
		db.SystemConfigItem{ID: 3, Key: "brand_name", Value: "Incudal", Type: "string"},
		db.SystemConfigItem{ID: 4, Key: "email_allowed_domains", Value: "gmail.com,outlook.com", Type: "string"},
	)

	masked := systemConfigMaskItems(items)
	if len(masked) != len(items) {
		t.Fatalf("masked %d items, want %d", len(masked), len(items))
	}

	byKey := map[string]string{}
	for _, item := range masked {
		byKey[item.Key] = item.Value
	}
	for _, key := range secretKeys {
		if byKey[key] != "********" {
			t.Errorf("%s = %q, want ********", key, byKey[key])
		}
	}
	if byKey["brand_name"] != "Incudal" {
		t.Errorf("brand_name = %q, want Incudal", byKey["brand_name"])
	}
	if byKey["email_allowed_domains"] != "gmail.com,outlook.com" {
		t.Errorf("email_allowed_domains = %q, want the stored value", byKey["email_allowed_domains"])
	}

	// Idempotent: masking the already-masked rows changes nothing.
	again := systemConfigMaskItems(masked)
	for index := range again {
		if again[index].Value != masked[index].Value {
			t.Fatalf("re-masking changed %s: %q -> %q", again[index].Key, masked[index].Value, again[index].Value)
		}
	}

	// The masked rows must also serialise without the plaintext anywhere.
	encoded, err := json.Marshal(systemConfigListResponse{Configs: masked})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range secretKeys {
		if containsString(string(encoded), "plaintext-"+key) {
			t.Fatalf("serialised response contains the %s secret", key)
		}
	}
}

func containsString(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}

// The admin audit entry redacts the seven keys the TypeScript lists, while the
// ordinary log line only carries the count.
func TestSystemConfigAuditValuesRedactsSecrets(t *testing.T) {
	pairs := []systemConfigUpdatePair{
		{Key: "smtp_password", Value: "hunter2"},
		{Key: "turnstile_secret_key", Value: "secret-key"},
		{Key: "ticket_image_lsky_token", Value: "lsky-token"},
		{Key: "telegram_bot_token", Value: "123:abc"},
		{Key: "telegram_group_chat_id", Value: "-100123"},
		{Key: "telegram_vip_group_chat_id", Value: "-100456"},
		{Key: "telegram_webhook_secret", Value: "webhook-secret"},
		{Key: "brand_name", Value: "Incudal"},
		{Key: "smtp_host", Value: "smtp.example.com"},
	}

	values := systemConfigAuditValues(pairs)
	if len(values) != len(pairs) {
		t.Fatalf("audit values = %d, want %d", len(values), len(pairs))
	}

	redacted := map[string]bool{
		"smtp_password":              true,
		"turnstile_secret_key":       true,
		"ticket_image_lsky_token":    true,
		"telegram_bot_token":         true,
		"telegram_group_chat_id":     true,
		"telegram_vip_group_chat_id": true,
		"telegram_webhook_secret":    true,
	}
	for index, entry := range values {
		if entry.Key != pairs[index].Key {
			t.Fatalf("entry %d key = %q, want %q", index, entry.Key, pairs[index].Key)
		}
		if redacted[entry.Key] {
			if entry.Value != "***REDACTED***" {
				t.Errorf("%s = %q, want ***REDACTED***", entry.Key, entry.Value)
			}
			continue
		}
		if entry.Value != pairs[index].Value {
			t.Errorf("%s = %q, want %q", entry.Key, entry.Value, pairs[index].Value)
		}
	}

	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"hunter2", "secret-key", "lsky-token", "123:abc", "-100123", "-100456", "webhook-secret"} {
		if containsString(string(encoded), secret) {
			t.Fatalf("audit payload contains a secret: %s", secret)
		}
	}
}

// Writing config must drop the process-local cache, otherwise every other module
// keeps reading the old value until the TTL expires. The route's persist step goes
// through db.UpdateSystemConfigs, whose empty-update path needs no database and
// still invalidates everything — so this is assertable without one.
func TestSystemConfigPersistUpdatesInvalidatesCache(t *testing.T) {
	seed := "stale-brand"
	lib.SetCachedConfig("brand_name", &seed)
	lib.SetCachedConfig("smtp_host", nil)
	if _, ok := lib.GetCachedConfig("brand_name"); !ok {
		t.Fatal("cache seed failed")
	}

	if err := systemConfigPersistUpdates(context.Background(), nil); err != nil {
		t.Fatalf("persist: %v", err)
	}

	if value, ok := lib.GetCachedConfig("brand_name"); ok {
		t.Fatalf("brand_name still cached as %v after the write", *value)
	}
	if _, ok := lib.GetCachedConfig("smtp_host"); ok {
		t.Fatal("negative cache entry survived the write")
	}
	if stats := lib.GetCacheStats(); stats.Size != 0 {
		t.Fatalf("cache size = %d after invalidation, want 0", stats.Size)
	}
}

// The six virtual defaults are what the admin UI sees for keys initSystemConfig
// never seeds. Their values must match the TypeScript constants byte for byte.
func TestSystemConfigVirtualDefaultsMatchTypeScript(t *testing.T) {
	defaults := systemConfigVirtualDefaults()
	if len(defaults) != 6 {
		t.Fatalf("virtual default count = %d, want 6", len(defaults))
	}

	byKey := map[string]systemConfigVirtualDefault{}
	for _, fallback := range defaults {
		byKey[fallback.Key] = fallback
	}
	want := map[string]struct {
		value string
		typ   string
	}{
		"traffic_overage_throttle_speed": {"1Mbit", "string"},
		"ticket_auto_close_enabled":      {"true", "boolean"},
		"ticket_auto_close_hours":        {"24", "number"},
		"aff_commission_rate":            {"0.05", "number"},
		"aff_discount_rate":              {"0.05", "number"},
		services.VipBenefitsConfigKey:    {services.VipDefaultBenefitsConfigJSON, "json"},
	}
	if len(want) != len(byKey) {
		t.Fatalf("virtual default keys = %d, want %d", len(byKey), len(want))
	}
	for key, expected := range want {
		fallback, ok := byKey[key]
		if !ok {
			t.Fatalf("missing virtual default %q", key)
		}
		if fallback.Value != expected.value {
			t.Errorf("%s value = %q, want %q", key, fallback.Value, expected.value)
		}
		if fallback.Type != expected.typ {
			t.Errorf("%s type = %q, want %q", key, fallback.Type, expected.typ)
		}
		if fallback.Label == "" || fallback.Description == "" {
			t.Errorf("%s is missing its label/description", key)
		}
	}

	// JSON.stringify(DEFAULT_VIP_BENEFITS_CONFIG) has no spaces and keeps levels
	// 1..5 in order.
	const wantVip = `{"levels":{"1":{"orderDiscountPercent":1,"extraTrafficPercent":2,"resourcePoolBonusPercent":2},"2":{"orderDiscountPercent":2,"extraTrafficPercent":4,"resourcePoolBonusPercent":4},"3":{"orderDiscountPercent":3,"extraTrafficPercent":6,"resourcePoolBonusPercent":6},"4":{"orderDiscountPercent":4,"extraTrafficPercent":8,"resourcePoolBonusPercent":8},"5":{"orderDiscountPercent":5,"extraTrafficPercent":10,"resourcePoolBonusPercent":10}}}`
	if services.VipDefaultBenefitsConfigJSON != wantVip {
		t.Fatalf("DEFAULT_VIP_BENEFITS_CONFIG_JSON drifted:\n got %s\nwant %s",
			services.VipDefaultBenefitsConfigJSON, wantVip)
	}
}

// The numeric/string helpers reproduce JavaScript's semantics for the values the
// route writes back to the table.
func TestSystemConfigNumericHelpers(t *testing.T) {
	positiveCases := []struct {
		raw    string
		max    int64
		want   int64
		wantOK bool
	}{
		{"1", 2147483647, 1, true},
		{" 42 ", 2147483647, 42, true},
		{"007", 2147483647, 0, false},
		{"0", 2147483647, 0, false},
		{"-1", 2147483647, 0, false},
		{"1.5", 2147483647, 0, false},
		{"2147483648", 2147483647, 0, false},
		{"9007199254740991", 9007199254740991, 9007199254740991, true},
		{"9007199254740992", 9007199254740991, 0, false},
		{"99999999999999999999", 9007199254740991, 0, false},
	}
	for _, testCase := range positiveCases {
		got, ok := systemConfigParsePositiveConfigInteger(testCase.raw, testCase.max)
		if ok != testCase.wantOK || (ok && got != testCase.want) {
			t.Errorf("parsePositiveConfigInteger(%q) = (%d, %v), want (%d, %v)",
				testCase.raw, got, ok, testCase.want, testCase.wantOK)
		}
	}

	nonNegativeCases := []struct {
		raw    string
		want   int64
		wantOK bool
	}{
		{"0", 0, true},
		{"0.5", 0, false},
		{"01", 0, false},
		{"65535", 65535, true},
		{" 12 ", 12, true},
	}
	for _, testCase := range nonNegativeCases {
		got, ok := systemConfigParseNonNegativeConfigInteger(testCase.raw, 65535)
		if ok != testCase.wantOK || (ok && got != testCase.want) {
			t.Errorf("parseNonNegativeConfigInteger(%q) = (%d, %v), want (%d, %v)",
				testCase.raw, got, ok, testCase.want, testCase.wantOK)
		}
	}

	decimalCases := []struct {
		raw    string
		want   float64
		wantOK bool
	}{
		{"0", 0, true},
		{"12.34", 12.34, true},
		{"007", 7, true},
		{"12.345", 0, false},
		{"-1", 0, false},
		{"1e3", 0, false},
		{"99999999.99", 99999999.99, true},
		{"100000000", 0, false},
	}
	for _, testCase := range decimalCases {
		got, ok := systemConfigParseDecimalMoneyConfig(testCase.raw, systemConfigMaxRegisterGiftBalance)
		if ok != testCase.wantOK || (ok && got != testCase.want) {
			t.Errorf("parseDecimalMoneyConfig(%q) = (%v, %v), want (%v, %v)",
				testCase.raw, got, ok, testCase.want, testCase.wantOK)
		}
	}

	rateCases := []struct {
		raw    string
		max    float64
		want   float64
		wantOK bool
	}{
		{"0", 0.5, 0, true},
		{"0.05", 0.5, 0.05, true},
		{"0.0500", 0.5, 0.05, true},
		{"0.51", 0.5, 0, false},
		{"0.96", 0.95, 0, false},
		{"1", 0.5, 0, false},
		{"", 0.5, 0, false},
		{".5", 0.5, 0, false},
	}
	for _, testCase := range rateCases {
		got, ok := systemConfigParseRateConfig(testCase.raw, testCase.max)
		if ok != testCase.wantOK || (ok && got != testCase.want) {
			t.Errorf("parseRateConfig(%q) = (%v, %v), want (%v, %v)",
				testCase.raw, got, ok, testCase.want, testCase.wantOK)
		}
	}

	// JavaScript number formatting for the exact values this route can emit.
	numberCases := []struct {
		value float64
		want  string
	}{
		{0, "0"},
		{0.05, "0.05"},
		{1, "1"},
		{7, "7"},
		{99999999.99, "99999999.99"},
		{0.000001, "0.000001"},
		{1e-7, "1e-7"},
		{1e21, "1e+21"},
	}
	for _, testCase := range numberCases {
		if got := systemConfigFormatJSNumber(testCase.value); got != testCase.want {
			t.Errorf("formatJSNumber(%v) = %q, want %q", testCase.value, got, testCase.want)
		}
	}
}

// normalizeAdminIdCsv: the CSV must be deduplicated, re-joined without spaces, and
// reject anything that is not a positive safe integer.
func TestSystemConfigNormalizeAdminIDCSV(t *testing.T) {
	cases := []struct {
		raw    string
		want   string
		wantOK bool
	}{
		{"", "", true},
		{"   ", "", true},
		{"1", "1", true},
		{" 1 , 2 ,1 ", "1,2", true},
		{"3,2,1", "3,2,1", true},
		{"0", "", false},
		{"-1", "", false},
		{"1,,2", "", false},
		{"1,", "", false},
		{"abc", "", false},
		{"1.5", "", false},
		{"9007199254740992", "", false},
	}
	for _, testCase := range cases {
		got, ok := systemConfigNormalizeAdminIDCSV(testCase.raw)
		if ok != testCase.wantOK || (ok && got != testCase.want) {
			t.Errorf("normalizeAdminIDCSV(%q) = (%q, %v), want (%q, %v)",
				testCase.raw, got, ok, testCase.want, testCase.wantOK)
		}
	}
}

// Only the domain of the recipient reaches the log/response, sanitised the same way.
func TestSystemConfigGetEmailDomainForAudit(t *testing.T) {
	cases := []struct {
		email string
		want  string
	}{
		{"user@example.com", "example.com"},
		{"User@Example.COM", "example.com"},
		{"user@a@b@sub.example.com", "sub.example.com"},
		{"user@", "unknown"},
		{"", "unknown"},
		{"user@exa_mple.com", "example.com"},
		{"user@example.com ", "example.com"},
	}
	for _, testCase := range cases {
		if got := systemConfigGetEmailDomainForAudit(testCase.email); got != testCase.want {
			t.Errorf("getEmailDomainForAudit(%q) = %q, want %q", testCase.email, got, testCase.want)
		}
	}
}

// The PUT key lists must stay in sync with the TypeScript: every key the route
// classifies is also whitelisted, and the high-risk pair is a subset.
func TestSystemConfigKeyClassifications(t *testing.T) {
	classified := map[string]bool{}
	for _, set := range []map[string]bool{
		systemConfigBooleanKeys,
		systemConfigStringKeys,
		systemConfigJSONKeys,
		systemConfigNumberKeys,
		systemConfigDecimalKeys,
		systemConfigRateKeys,
	} {
		for key := range set {
			if classified[key] {
				t.Errorf("%s is classified twice", key)
			}
			classified[key] = true
		}
	}
	// transfer_fee is handled by its own branch, not by a key set.
	classified["transfer_fee"] = true

	for key := range classified {
		if !systemConfigValidKeys[key] {
			t.Errorf("%s is classified but not whitelisted", key)
		}
	}
	for key := range systemConfigValidKeys {
		if !classified[key] {
			t.Errorf("%s is whitelisted but has no validation branch", key)
		}
	}
	for key := range systemConfigHighRiskAdminIDKeys {
		if !systemConfigValidKeys[key] {
			t.Errorf("%s is high risk but not whitelisted", key)
		}
	}
	if len(systemConfigHighRiskAdminIDKeys) != 2 {
		t.Errorf("high risk key count = %d, want 2", len(systemConfigHighRiskAdminIDKeys))
	}
}

// The string-key normaliser is table-driven here so the exact accept/reject edge
// cases of each key stay pinned (all values are checked before any DB access).
func TestSystemConfigNormalizeStringValue(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		value  string
		want   string
		wantOK bool
	}{
		{"throttle lowercase", "traffic_overage_throttle_speed", " 1mbit ", "1mbit", true},
		{"throttle decimal", "traffic_overage_throttle_speed", "1.5Gbit", "1.5Gbit", true},
		{"throttle zero", "traffic_overage_throttle_speed", "0Mbit", "", false},
		{"throttle unit", "traffic_overage_throttle_speed", "1Kbit", "", false},

		{"brand name trimmed", "brand_name", "  Incudal  ", "Incudal", true},
		{"brand logo path", "brand_logo_url", " /logo.svg ", "/logo.svg", true},
		{"brand logo url", "brand_logo_url", "https://cdn.example.com/x.png", "https://cdn.example.com/x.png", true},
		{"brand logo relative", "brand_logo_url", "logo.svg", "", false},

		{"promo image url", "popup_promo_image_url", "https://cdn.example.com/x.png", "https://cdn.example.com/x.png", true},
		{"promo image relative", "popup_promo_image_url", "/x.png", "", false},

		// A syntactically valid package id is resolved against the package table, so
		// the accept path needs a database and is covered by the conformance run
		// instead; the two rejections below return before touching it.
		{"promo package id zero", "popup_promo_package_id", "0", "", false},
		{"promo package id text", "popup_promo_package_id", "abc", "", false},

		{"lsky v1", "ticket_image_lsky_api_version", "v1", "v1", true},
		{"lsky v3", "ticket_image_lsky_api_version", "v3", "", false},

		{"join mode any", "telegram_group_join_mode", "any", "any", true},
		{"join mode uppercase", "telegram_group_join_mode", "ANY", "", false},

		{"webhook secret keep placeholder", "telegram_webhook_secret", "********", "********", true},
		{"webhook secret valid", "telegram_webhook_secret", "abc_DEF-123", "abc_DEF-123", true},
		{"webhook secret invalid", "telegram_webhook_secret", "abc.def", "", false},
		{"webhook secret empty", "telegram_webhook_secret", "", "", true},

		{"admin ids clear", "system_update_allowed_admin_ids", "  ", "", true},
		{"admin ids dedupe", "system_update_allowed_admin_ids", "1, 2,1", "1,2", true},
		{"admin ids invalid", "payincus_gift_card_admin_ids", "0", "", false},

		{"plain string untouched", "smtp_host", "  smtp.example.com  ", "  smtp.example.com  ", true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok, err := systemConfigNormalizeStringValue(context.Background(), testCase.key, testCase.value)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != testCase.wantOK || (ok && got != testCase.want) {
				t.Fatalf("normalize(%s, %q) = (%q, %v), want (%q, %v)",
					testCase.key, testCase.value, got, ok, testCase.want, testCase.wantOK)
			}
		})
	}
}

// The JSON keys are re-serialised with their normalised shape; anything the
// TypeScript rejects must be rejected here too.
func TestSystemConfigNormalizeJSONValue(t *testing.T) {
	inviteCases := []struct {
		name   string
		value  string
		want   string
		wantOK bool
	}{
		{
			name:   "default options round-trip",
			value:  `[{"resource":"balance","amount":0,"enabled":false},{"resource":"points","amount":0,"enabled":false}]`,
			want:   `[{"resource":"balance","amount":0,"enabled":false},{"resource":"points","amount":0,"enabled":false}]`,
			wantOK: true,
		},
		{
			// points must be a safe integer (rejected below); balance is rounded to
			// cents when the value is re-serialised.
			name:   "balance is rounded to cents",
			value:  `[{"resource":"balance","amount":1.239,"enabled":true},{"resource":"points","amount":3,"enabled":true}]`,
			want:   `[{"resource":"balance","amount":1.24,"enabled":true},{"resource":"points","amount":3,"enabled":true}]`,
			wantOK: true,
		},
		{
			name:   "missing resources get defaults appended",
			value:  `[{"resource":"points","amount":5,"enabled":true}]`,
			want:   `[{"resource":"points","amount":5,"enabled":true},{"resource":"balance","amount":0,"enabled":false}]`,
			wantOK: true,
		},
		{
			name:   "unknown resource",
			value:  `[{"resource":"banana","amount":1,"enabled":true}]`,
			wantOK: false,
		},
		{
			name:   "string amount",
			value:  `[{"resource":"balance","amount":"1","enabled":true}]`,
			wantOK: false,
		},
		{
			name:   "negative amount",
			value:  `[{"resource":"balance","amount":-1,"enabled":true}]`,
			wantOK: false,
		},
		{
			name:   "points must be integral",
			value:  `[{"resource":"points","amount":1.5,"enabled":true}]`,
			wantOK: false,
		},
		{
			name:   "balance over the cap",
			value:  `[{"resource":"balance","amount":100000000,"enabled":true}]`,
			wantOK: false,
		},
		{
			name:   "not an array",
			value:  `{"resource":"balance"}`,
			wantOK: false,
		},
		{
			name:   "element is not an object",
			value:  `[1]`,
			wantOK: false,
		},
		{
			name:   "invalid json",
			value:  `[`,
			wantOK: false,
		},
	}
	for _, testCase := range inviteCases {
		t.Run("invite/"+testCase.name, func(t *testing.T) {
			got, ok := systemConfigNormalizeJSONValue("invite_generation_costs", testCase.value)
			if ok != testCase.wantOK || (ok && got != testCase.want) {
				t.Fatalf("normalize(%q) = (%q, %v), want (%q, %v)",
					testCase.value, got, ok, testCase.want, testCase.wantOK)
			}
		})
	}

	vipCases := []struct {
		name   string
		value  string
		wantOK bool
	}{
		{
			// normalizeVipBenefitsConfig normalises levels 1..5 unconditionally, so a
			// config that omits a level throws (→ 400) rather than inheriting defaults.
			name:   "partial config is rejected",
			value:  `{"levels":{"1":{"orderDiscountPercent":10,"extraTrafficPercent":20,"resourcePoolBonusPercent":30}}}`,
			wantOK: false,
		},
		{
			name:   "full config is accepted",
			value:  `{"levels":{"1":{"orderDiscountPercent":1,"extraTrafficPercent":2,"resourcePoolBonusPercent":2},"2":{"orderDiscountPercent":2,"extraTrafficPercent":4,"resourcePoolBonusPercent":4},"3":{"orderDiscountPercent":3,"extraTrafficPercent":6,"resourcePoolBonusPercent":6},"4":{"orderDiscountPercent":4,"extraTrafficPercent":8,"resourcePoolBonusPercent":8},"5":{"orderDiscountPercent":5,"extraTrafficPercent":10,"resourcePoolBonusPercent":10}}}`,
			wantOK: true,
		},
		{
			name:   "percent above 100 is rejected",
			value:  `{"levels":{"1":{"orderDiscountPercent":101,"extraTrafficPercent":0,"resourcePoolBonusPercent":0}}}`,
			wantOK: false,
		},
		{
			name:   "negative percent is rejected",
			value:  `{"levels":{"1":{"orderDiscountPercent":-1,"extraTrafficPercent":0,"resourcePoolBonusPercent":0}}}`,
			wantOK: false,
		},
		{
			name:   "invalid json",
			value:  `nope`,
			wantOK: false,
		},
	}
	for _, testCase := range vipCases {
		t.Run("vip/"+testCase.name, func(t *testing.T) {
			got, ok := systemConfigNormalizeJSONValue(services.VipBenefitsConfigKey, testCase.value)
			if ok != testCase.wantOK {
				t.Fatalf("normalize(%q) = (%q, %v), want ok=%v", testCase.value, got, ok, testCase.wantOK)
			}
			if ok {
				// The normalised value must round-trip as JSON (it is what gets stored).
				var decoded any
				if err := json.Unmarshal([]byte(got), &decoded); err != nil {
					t.Fatalf("normalised value is not JSON: %v", err)
				}
			}
		})
	}
}
