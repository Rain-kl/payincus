package routes

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// decodeStorageBody runs a raw JSON document through the shared Fastify body
// decoder so the tests cover the same path the handlers use.
func decodeStorageBody(t *testing.T, raw string) *fastifyJSONBody {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("test payload is not valid JSON: %v", err)
	}
	return &fastifyJSONBody{Value: decoded, ObjectKeys: jsonTopLevelKeys([]byte(raw))}
}

// The create schema's messages and ordering are pinned to the real Fastify+AJV
// output (verified against fastify@5.6.2 with the default AJV options):
// `required` is evaluated first, in schema order, then properties in schema order.
func TestValidateCreateStorageBody(t *testing.T) {
	fields := storageCreateFields

	cases := []struct {
		name     string
		raw      string
		want     string
		wantPort *int32
	}{
		{"empty object reports the first required field", `{}`, "name is required", nil},
		{"missing type", `{"name":"a"}`, "type is required", nil},
		{"missing host", `{"name":"a","type":"S3"}`, "host is required", nil},
		{"missing both reports name first", `{"host":"h"}`, "name is required", nil},
		{"invalid enum", `{"name":"a","type":"NOPE","host":"h"}`, "must be equal to one of the allowed values", nil},
		{"empty name", `{"name":"","type":"S3","host":"h"}`, "name is too short", nil},
		{"name over 50", `{"name":"` + strings.Repeat("x", 51) + `","type":"S3","host":"h"}`, "name is too long", nil},
		{"empty host", `{"name":"a","type":"S3","host":""}`, "host is too short", nil},
		{"host over 255", `{"name":"a","type":"S3","host":"` + strings.Repeat("h", 256) + `"}`, "host is too long", nil},
		{"string port is coerced", `{"name":"a","type":"S3","host":"h","port":"22"}`, "", int32Ptr(22)},
		{"float port rejected", `{"name":"a","type":"S3","host":"h","port":"22.5"}`, "must be integer", nil},
		{"fractional port rejected", `{"name":"a","type":"S3","host":"h","port":22.5}`, "must be integer", nil},
		{"port 0 below the minimum", `{"name":"a","type":"S3","host":"h","port":0}`, "must be >= 1", nil},
		{"port above the maximum", `{"name":"a","type":"S3","host":"h","port":70000}`, "must be <= 65535", nil},
		{"millisecond array port unwraps", `{"name":"a","type":"S3","host":"h","port":[22]}`, "", int32Ptr(22)},
		{"boolean port coerces to 1", `{"name":"a","type":"S3","host":"h","port":true}`, "", int32Ptr(1)},
		{"null port is not an integer on create", `{"name":"a","type":"S3","host":"h","port":null}`, "must be >= 1", nil},
		{"isDefault string true coerces", `{"name":"a","type":"S3","host":"h","isDefault":"true"}`, "", nil},
		{"isDefault arbitrary string rejected", `{"name":"a","type":"S3","host":"h","isDefault":"yes"}`, "must be boolean", nil},
		{"array extra is not an object", `{"name":"a","type":"S3","host":"h","extra":[]}`, "must be object", nil},
		{"null extra is not an object on create", `{"name":"a","type":"S3","host":"h","extra":null}`, "must be object", nil},
		{"object extra accepted", `{"name":"a","type":"S3","host":"h","extra":{"bucket":"b"}}`, "", nil},
		{"unknown keys pass AJV (no additionalProperties:false)", `{"name":"a","type":"S3","host":"h","unknownField":1}`, "", nil},
		{"number name is stringified", `{"name":123,"type":"S3","host":"h"}`, "", nil},
		{"null name coerces to empty", `{"name":null,"type":"S3","host":"h"}`, "name is too short", nil},
		{"long password", `{"name":"a","type":"S3","host":"h","password":"` + strings.Repeat("p", 256) + `"}`, "password is too long", nil},
		{"long basePath", `{"name":"a","type":"S3","host":"h","basePath":"` + strings.Repeat("b", 256) + `"}`, "basePath is too long", nil},
		{"non-object root", `[1,2]`, "must be object", nil},
		{"null root", `null`, "must be object", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := decodeStorageBody(t, tc.raw)
			validated, message := validateAJVObject(body.Value, []string{"name", "type", "host"}, fields)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
			if tc.want != "" {
				return
			}
			port, _ := validated.nullableInt32("port")
			if tc.wantPort == nil {
				if port != nil {
					t.Fatalf("port = %v, want nil", *port)
				}
				return
			}
			if port == nil || *port != *tc.wantPort {
				t.Fatalf("port = %v, want %d", port, *tc.wantPort)
			}
		})
	}
}

// The update schema makes port/username/password/basePath/extra nullable and has no
// `required` list, so an empty object is valid and explicit nulls survive.
func TestValidateUpdateStorageBody(t *testing.T) {
	fields := storageUpdateFields

	cases := []struct {
		name  string
		raw   string
		want  string
		check func(t *testing.T, body ajvValidated)
	}{
		{
			name: "empty object is valid",
			raw:  `{}`,
			check: func(t *testing.T, body ajvValidated) {
				if len(body.Present) != 0 {
					t.Fatalf("present = %v, want none", body.Present)
				}
			},
		},
		{
			name: "explicit null port is preserved",
			raw:  `{"port":null}`,
			check: func(t *testing.T, body ajvValidated) {
				port, present := body.nullableInt32("port")
				if !present || port != nil {
					t.Fatalf("port = %v (present=%v), want present+nil", port, present)
				}
			},
		},
		{
			name: "empty string port becomes null",
			raw:  `{"port":""}`,
			check: func(t *testing.T, body ajvValidated) {
				port, present := body.nullableInt32("port")
				if !present || port != nil {
					t.Fatalf("port = %v (present=%v), want present+nil", port, present)
				}
			},
		},
		{
			name: "string port on update still coerces",
			raw:  `{"port":"22"}`,
			check: func(t *testing.T, body ajvValidated) {
				port, present := body.nullableInt32("port")
				if !present || port == nil || *port != 22 {
					t.Fatalf("port = %v (present=%v), want 22", port, present)
				}
			},
		},
		{
			name: "non-numeric port string is rejected",
			raw:  `{"port":"abc"}`,
			want: "must be integer,null",
		},
		{
			name: "null username is preserved",
			raw:  `{"username":null}`,
			check: func(t *testing.T, body ajvValidated) {
				username, present := body.nullableString("username")
				if !present || username != nil {
					t.Fatalf("username = %v (present=%v), want present+nil", username, present)
				}
			},
		},
		{
			name: "number username is stringified",
			raw:  `{"username":123}`,
			check: func(t *testing.T, body ajvValidated) {
				username, present := body.nullableString("username")
				if !present || username == nil || *username != "123" {
					t.Fatalf("username = %v, want \"123\"", username)
				}
			},
		},
		{
			name: "null extra is preserved",
			raw:  `{"extra":null}`,
			check: func(t *testing.T, body ajvValidated) {
				if !body.Present["extra"] || body.Values["extra"] != nil {
					t.Fatalf("extra = %v (present=%v), want present+nil", body.Values["extra"], body.Present["extra"])
				}
			},
		},
		{
			name: "array extra is rejected on update too",
			raw:  `{"extra":[]}`,
			want: "must be object,null",
		},
		{
			name: "object extra is accepted",
			raw:  `{"extra":{"a":1}}`,
			check: func(t *testing.T, body ajvValidated) {
				if _, ok := body.Values["extra"].(map[string]any); !ok {
					t.Fatalf("extra = %#v, want map", body.Values["extra"])
				}
			},
		},
		{"invalid enum", `{"type":"nope"}`, "must be equal to one of the allowed values", nil},
		{"unknown update keys pass AJV", `{"junk":true}`, "", nil},
		{"empty name still rejected", `{"name":""}`, "name is too short", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := decodeStorageBody(t, tc.raw)
			validated, message := validateAJVObject(body.Value, nil, fields)
			if message != tc.want {
				t.Fatalf("message = %q, want %q", message, tc.want)
			}
			if tc.want == "" && tc.check != nil {
				tc.check(t, validated)
			}
		})
	}
}

// Credentials must never be part of an API response. This asserts the structural
// guarantee (no Password field on any response view) rather than a runtime sample.
func TestStorageResponseViewsOmitPassword(t *testing.T) {
	for name, sample := range map[string]any{
		"list":   storageConfigListResponse{},
		"create": storageConfigCreatedResponse{},
		"update": storageConfigUpdatedResponse{},
	} {
		encoded, err := json.Marshal(sample)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if strings.Contains(strings.ToLower(string(encoded)), "password") {
			t.Fatalf("%s response leaks a password field: %s", name, encoded)
		}
	}
}

// The stored secret is written through lib.EncryptSensitiveData, which emits
// `iv:tag:ciphertext` and round-trips through DecryptSensitiveData.
func TestStoragePasswordEncryptionRoundTrip(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-secret-for-storage-configs")
	t.Setenv("ENCRYPTION_KEY", "")

	secret := "s3-secret-access-key"
	encrypted, err := lib.EncryptSensitiveData(secret)
	if err != nil {
		t.Fatalf("EncryptSensitiveData: %v", err)
	}
	if encrypted == secret {
		t.Fatal("ciphertext equals plaintext")
	}
	if strings.Count(encrypted, ":") != 2 {
		t.Fatalf("ciphertext layout = %q, want iv:tag:ct", encrypted)
	}
	if got := lib.DecryptSensitiveData(encrypted); got != secret {
		t.Fatalf("DecryptSensitiveData() = %q, want %q", got, secret)
	}

	// An empty plaintext is never encrypted by the routes (they store NULL), but the
	// primitive itself must still round-trip.
	emptyEncrypted, err := lib.EncryptSensitiveData("")
	if err != nil {
		t.Fatalf("EncryptSensitiveData(\"\"): %v", err)
	}
	if got := lib.DecryptSensitiveData(emptyEncrypted); got != "" {
		t.Fatalf("empty round trip = %q", got)
	}
}

// normalizeS3Extra keeps credentials out of `extra`: only bucket/region/
// forcePathStyle survive.
func TestNormalizeS3ExtraStripsCredentials(t *testing.T) {
	normalized, err := normalizeS3Extra(map[string]any{
		"bucket":         "  my-bucket  ",
		"accessKey":      "AKIASECRET",
		"secretKey":      "SUPERSECRET",
		"forcePathStyle": true,
	})
	if err != nil {
		t.Fatalf("normalizeS3Extra: %v", err)
	}
	if len(normalized) != 3 {
		t.Fatalf("normalized extra = %v, want exactly bucket/region/forcePathStyle", normalized)
	}
	if normalized["bucket"] != "my-bucket" {
		t.Fatalf("bucket = %v", normalized["bucket"])
	}
	if normalized["region"] != "auto" {
		t.Fatalf("default region = %v, want auto", normalized["region"])
	}
	if normalized["forcePathStyle"] != true {
		t.Fatalf("forcePathStyle = %v", normalized["forcePathStyle"])
	}
	encoded, _ := json.Marshal(normalized)
	if strings.Contains(string(encoded), "SECRET") {
		t.Fatalf("credentials leaked into extra: %s", encoded)
	}

	// Invalid buckets are rejected.
	for _, bucket := range []string{"", " ", "-bad", "bad-", strings.Repeat("a", 129), "bad_bucket"} {
		if _, err := normalizeS3Extra(map[string]any{"bucket": bucket}); err == nil {
			t.Errorf("bucket %q must be rejected", bucket)
		}
	}
	if _, err := normalizeS3Extra(map[string]any{"bucket": "ok", "region": strings.Repeat("r", 65)}); err == nil {
		t.Error("over-long region must be rejected")
	}
}

// normalizeStorageExtra requires an access key and a secret for S3 only.
func TestNormalizeStorageExtraS3Requirements(t *testing.T) {
	s3 := string(models.StorageTypeS3)

	if _, err := normalizeStorageExtra(s3, map[string]any{"bucket": "bb"}, nil, true); err == nil {
		t.Error("missing access key must be rejected for S3")
	}
	blank := "   "
	if _, err := normalizeStorageExtra(s3, map[string]any{"bucket": "bb"}, &blank, true); err == nil {
		t.Error("blank access key must be rejected for S3")
	}
	accessKey := "AKIA"
	if _, err := normalizeStorageExtra(s3, map[string]any{"bucket": "bb"}, &accessKey, false); err == nil {
		t.Error("missing secret key must be rejected for S3")
	}
	if _, err := normalizeStorageExtra(s3, map[string]any{"bucket": "bb"}, &accessKey, true); err != nil {
		t.Errorf("valid S3 config rejected: %v", err)
	}

	// Non-S3 types pass `extra` through untouched, even without credentials.
	extra := map[string]any{"anything": "goes"}
	got, err := normalizeStorageExtra(string(models.StorageTypeFTP), extra, nil, false)
	if err != nil {
		t.Fatalf("FTP extra: %v", err)
	}
	if got["anything"] != "goes" {
		t.Fatalf("FTP extra was altered: %v", got)
	}
}

// normalizeStorageBasePath mirrors storage/path.ts exactly, including the trailing
// slash and the relative-segment rejection.
func TestNormalizeStorageBasePath(t *testing.T) {
	str := func(value string) *string { return &value }

	cases := []struct {
		name    string
		input   *string
		want    string
		wantErr bool
	}{
		{"nil", nil, "", false},
		{"empty", str(""), "", false},
		{"whitespace only", str("   "), "", false},
		{"simple", str("backups"), "backups/", false},
		{"absolute", str("/backups"), "/backups/", false},
		{"already has a trailing slash", str("/backups/"), "/backups/", false},
		{"collapses empty segments", str("/a//b///c"), "/a/b/c/", false},
		{"root only", str("/"), "/", false},
		{"dot segment", str("/a/./b"), "", true},
		{"dotdot segment", str("/a/../b"), "", true},
		{"backslash", str("/a\\b"), "", true},
		{"nul byte", str("/a\x00b"), "", true},
		{"trims surrounding spaces", str("  /a/b  "), "/a/b/", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeStorageBasePath(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got != tc.want {
				t.Fatalf("normalizeStorageBasePath() = %q, want %q", got, tc.want)
			}
		})
	}
}

// jsonTopLevelKeys preserves document order, which the Prisma validation-message
// replica depends on.
func TestJSONTopLevelKeysPreservesOrder(t *testing.T) {
	keys := jsonTopLevelKeys([]byte(`{"z":1,"a":2,"m":3}`))
	want := []string{"z", "a", "m"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys = %v, want %v", keys, want)
		}
	}
	if got := jsonTopLevelKeys([]byte(`[1,2]`)); got != nil {
		t.Fatalf("array document keys = %v, want nil", got)
	}
}

// storageUnknownUpdateKeys flags the keys Prisma would reject for
// storageConfig.update. Fastify/AJV does not strip them (the schema has no
// `additionalProperties:false`), so the TS handler spreads `...rest` into Prisma,
// which is what produces the 500 this replica reproduces.
func TestStorageUnknownUpdateKeys(t *testing.T) {
	body := decodeStorageBody(t, `{"name":"a","junk":1,"nope":"x"}`)
	unknown := storageUnknownUpdateKeys(body)
	if len(unknown) != 2 || unknown[0] != "junk" || unknown[1] != "nope" {
		t.Fatalf("unknown = %v, want [junk nope]", unknown)
	}

	known := decodeStorageBody(t, `{"name":"a","type":"S3","host":"h","port":1,"isDefault":true}`)
	if unknown := storageUnknownUpdateKeys(known); len(unknown) != 0 {
		t.Fatalf("unknown = %v, want none", unknown)
	}
}

// The replicated Prisma message must keep its shape: invocation name, the where
// block, the data block in document order, the caret under the offending key and
// the trailing "Unknown argument" line.
func TestPrismaUnknownArgumentMessage(t *testing.T) {
	body := decodeStorageBody(t, `{"name":"a","junk":1}`)
	message := prismaUnknownArgumentMessage(42, body, "junk")

	for _, fragment := range []string{
		"Invalid `prisma.storageConfig.update()` invocation:",
		"id: 42",
		"    name: \"a\",",
		"    junk: 1,",
		"    ~~~~",
		"?   name?: String | StringFieldUpdateOperationsInput,",
		"Unknown argument `junk`. Available options are marked with ?.",
	} {
		if !strings.Contains(message, fragment) {
			t.Fatalf("message is missing %q:\n%s", fragment, message)
		}
	}

	// apiError caps details at 256 characters; the truncation must not panic and must
	// keep the leading part of the message.
	apiError := lib.APIErrorFor(lib.ErrStorageConfigUpdateFailed, message)
	if apiError.Code != lib.ErrStorageConfigUpdateFailed {
		t.Fatalf("code = %q", apiError.Code)
	}
	if runes := []rune(apiError.Details); len(runes) > 256 {
		t.Fatalf("details length = %d, want <= 256", len(runes))
	}
	if !strings.HasPrefix(apiError.Details, "\nInvalid `prisma.storageConfig.update()` invocation:") {
		t.Fatalf("details prefix = %q", apiError.Details)
	}
}

// Prototype-poisoning payloads are rejected by secure-json-parse before any handler
// logic runs, so decodeJSONRequestBody must treat them as invalid JSON.
func TestHasForbiddenPrototypeKey(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  bool
	}{
		{"plain object", map[string]any{"a": "b"}, false},
		{"__proto__ key", map[string]any{"__proto__": map[string]any{"x": 1}}, true},
		{"constructor.prototype", map[string]any{"constructor": map[string]any{"prototype": map[string]any{"x": 1}}}, true},
		{"constructor alone is fine", map[string]any{"constructor": "x"}, false},
		{"nested", map[string]any{"a": []any{map[string]any{"__proto__": 1}}}, true},
		{"array root", []any{map[string]any{"__proto__": 1}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasForbiddenPrototypeKey(tc.value); got != tc.want {
				t.Fatalf("hasForbiddenPrototypeKey() = %v, want %v", got, tc.want)
			}
		})
	}
}

// mediaTypeIs mirrors Fastify's prefix match, so "application/jsonfoo" is not JSON.
func TestMediaTypeIs(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"application/json ; charset=utf-8", true},
		{"application/jsonfoo", false},
		{"application/vnd.api+json", false},
		{"application/xml", false},
	}
	for _, tc := range cases {
		if got := mediaTypeIs(strings.ToLower(tc.header), "application/json"); got != tc.want {
			t.Errorf("mediaTypeIs(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

// ajvStringToInteger mirrors JS `data == +data && !(data % 1)`.
func TestAJVStringToInteger(t *testing.T) {
	cases := []struct {
		input string
		want  float64
		ok    bool
		inf   bool
	}{
		{"22", 22, true, false},
		{"022", 22, true, false},
		{"22.0", 22, true, false},
		{"2.2e1", 22, true, false},
		{" 22 ", 22, true, false},
		{"0x10", 16, true, false},
		{"0b101", 5, true, false},
		{"0o17", 15, true, false},
		{"-5", -5, true, false},
		{"+5", 5, true, false},
		{"22.5", 0, false, false},
		{"", 0, false, false},
		{"abc", 0, false, false},
		{"1_000", 0, false, false},
		{"NaN", 0, false, false},
		{"Infinity", 0, true, true},
		{"-0x10", 0, false, false},
	}
	for _, tc := range cases {
		got, ok := ajvStringToInteger(tc.input)
		if ok != tc.ok {
			t.Errorf("ajvStringToInteger(%q) ok = %v, want %v", tc.input, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if tc.inf {
			if !math.IsInf(got, 1) {
				t.Errorf("ajvStringToInteger(%q) = %v, want +Inf", tc.input, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("ajvStringToInteger(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// jsStringLength counts UTF-16 code units, which is what AJV's length keywords use.
func TestJSStringLength(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"", 0},
		{"abc", 3},
		{"\U0001F600", 2},
		{"a\U0001F600b", 4},
		{"中文", 2},
	}
	for _, tc := range cases {
		if got := jsStringLength(tc.input); got != tc.want {
			t.Errorf("jsStringLength(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func int32Ptr(value int32) *int32 { return &value }

// nullableNonEmpty mirrors `normalized || null`, i.e. "" becomes a NULL column.
func TestNullableNonEmpty(t *testing.T) {
	if got := nullableNonEmpty(""); got != nil {
		t.Errorf("nullableNonEmpty(\"\") = %#v, want nil", got)
	}
	got := nullableNonEmpty("/backups/")
	if got == nil || *got != "/backups/" {
		t.Fatalf("nullableNonEmpty() = %#v", got)
	}
}

// storageISOTime mirrors Date#toISOString() (UTC, millisecond precision).
func TestStorageISOTime(t *testing.T) {
	value := time.Date(2026, 10, 29, 20, 34, 56, 0, time.FixedZone("CST", 8*3600))
	if got := storageISOTime(value); got != "2026-10-29T12:34:56.000Z" {
		t.Fatalf("storageISOTime() = %q", got)
	}
	value = time.Date(2026, 1, 2, 3, 4, 5, 500_000_000, time.UTC)
	if got := storageISOTime(value); got != "2026-01-02T03:04:05.500Z" {
		t.Fatalf("storageISOTime() = %q", got)
	}
}

// jsonFromAny turns a decoded request `extra` object into the column payload.
func TestJSONFromAny(t *testing.T) {
	if got := jsonFromAny(nil); got != nil {
		t.Errorf("nil = %#v, want nil", got)
	}

	encoded := jsonFromAny(map[string]any{"bucket": "b", "forcePathStyle": true})
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["bucket"] != "b" || decoded["forcePathStyle"] != true {
		t.Fatalf("decoded = %#v", decoded)
	}

	existing := datatypes.JSON(`{"a":1}`)
	if got := jsonFromAny(existing); string(got) != string(existing) {
		t.Fatalf("existing JSON = %s, want %s", got, existing)
	}
}

// extraAsMap converts the stored column back to the map the S3 helpers consume.
func TestExtraAsMap(t *testing.T) {
	if got := extraAsMap(nil); got != nil {
		t.Errorf("nil extra = %#v, want nil", got)
	}
	if got := extraAsMap(datatypes.JSON{}); got != nil {
		t.Errorf("empty extra = %#v, want nil", got)
	}

	decoded := extraAsMap(datatypes.JSON(`{"bucket":"b","region":"auto","forcePathStyle":true}`))
	if decoded["bucket"] != "b" || decoded["region"] != "auto" || decoded["forcePathStyle"] != true {
		t.Fatalf("decoded extra = %#v", decoded)
	}

	// Non-object / invalid JSON must not panic and yields nil.
	if got := extraAsMap(datatypes.JSON(`[1,2]`)); got != nil {
		t.Errorf("array extra = %#v, want nil", got)
	}
	if got := extraAsMap(datatypes.JSON(`not json`)); got != nil {
		t.Errorf("invalid extra = %#v, want nil", got)
	}
}

// prismaRenderValue must quote strings and pass scalars through, matching how
// Prisma prints the argument object.
func TestPrismaRenderValue(t *testing.T) {
	cases := []struct {
		input any
		want  string
	}{
		{nil, "null"},
		{true, "true"},
		{float64(1), "1"},
		{float64(1.5), "1.5"},
		{"a", `"a"`},
		{`a"b`, `"a\"b"`},
		{map[string]any{"a": float64(1)}, `{"a":1}`},
	}
	for _, tc := range cases {
		if got := prismaRenderValue(tc.input); got != tc.want {
			t.Errorf("prismaRenderValue(%#v) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
