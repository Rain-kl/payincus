// Unit tests for the runtime-settings port. They live in package lib so they can
// exercise the unexported helpers directly.
//
// The database path is exercised without a real database by seeding the
// process-local config cache (lib/config_cache.go): a cached nil value means
// "config row absent", which is exactly what db.GetSystemConfig returns for a
// missing key, so the resolver's fallback chain can be tested end to end.
package lib

import (
	"context"
	"math"
	"testing"
)

func testStrPtr(value string) *string { return &value }

func TestEnvFirst(t *testing.T) {
	t.Setenv("RS_ENV_A", "  ")
	t.Setenv("RS_ENV_B", " value-b ")
	t.Setenv("RS_ENV_C", "value-c")

	cases := []struct {
		keys []string
		want string
	}{
		{[]string{"RS_ENV_MISSING"}, ""},
		{[]string{"RS_ENV_A"}, ""},                    // whitespace-only is skipped
		{[]string{"RS_ENV_A", "RS_ENV_B"}, "value-b"}, // first non-empty, trimmed
		{[]string{"RS_ENV_B", "RS_ENV_C"}, "value-b"},
		{[]string{}, ""},
	}
	for _, tc := range cases {
		if got := envFirst(tc.keys); got != tc.want {
			t.Errorf("envFirst(%v) = %q, want %q", tc.keys, got, tc.want)
		}
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"1", []string{"1"}},
		{"1,2,3", []string{"1", "2", "3"}},
		{" 1 , 2 ,, 3 ", []string{"1", "2", "3"}},
		{",,,", nil},
	}
	for _, tc := range cases {
		got := splitCSV(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", tc.input, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitCSV(%q)[%d] = %q, want %q", tc.input, i, got[i], tc.want[i])
			}
		}
	}
}

func TestJSToNumber(t *testing.T) {
	cases := []struct {
		input string
		want  float64
	}{
		{"", 0},    // Number("") === 0
		{"   ", 0}, // whitespace only
		{"42", 42},
		{" 42 ", 42},
		{"-7", -7},
		{"+7", 7},
		{"3.5", 3.5},
		{"1e3", 1000},
		{"0x10", 16},
		{"0b101", 5},
		{"0o17", 15},
		{"Infinity", math.Inf(1)},
		{"-Infinity", math.Inf(-1)},
		{"abc", math.NaN()},
		{"12abc", math.NaN()}, // Number() does not do parseInt-style prefix parsing
		{"1.2.3", math.NaN()},
	}
	for _, tc := range cases {
		got := jsToNumber(tc.input)
		switch {
		case math.IsNaN(tc.want):
			if !math.IsNaN(got) {
				t.Errorf("jsToNumber(%q) = %v, want NaN", tc.input, got)
			}
		case math.IsInf(tc.want, 0):
			if !math.IsInf(got, int(math.Copysign(1, tc.want))) {
				t.Errorf("jsToNumber(%q) = %v, want %v", tc.input, got, tc.want)
			}
		default:
			if got != tc.want {
				t.Errorf("jsToNumber(%q) = %v, want %v", tc.input, got, tc.want)
			}
		}
	}
}

func TestJSIntegerPredicates(t *testing.T) {
	if !isJSInteger(42) || !isJSInteger(-3) || !isJSInteger(0) {
		t.Error("whole numbers must be integers")
	}
	if isJSInteger(1.5) || isJSInteger(math.NaN()) || isJSInteger(math.Inf(1)) {
		t.Error("fractional/NaN/Inf must not be integers")
	}

	if !isJSSafeInteger(9007199254740991) {
		t.Error("Number.MAX_SAFE_INTEGER must be safe")
	}
	if isJSSafeInteger(9007199254740992) {
		t.Error("MAX_SAFE_INTEGER+1 must not be safe")
	}
	if isJSSafeInteger(1.5) {
		t.Error("non-integer must not be safe")
	}
}

func TestParseIntJSValue(t *testing.T) {
	cases := []struct {
		input string
		want  int64
		ok    bool
	}{
		{"42", 42, true},
		{" 42 ", 42, true},
		{"-7", -7, true},
		{"+7", 7, true},
		{"12abc", 12, true},
		{"1.9", 1, true},
		{"abc", 0, false},
		{"", 0, false},
		{"-", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseIntJSValue(tc.input)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseIntJSValue(%q) = (%d, %v), want (%d, %v)", tc.input, got, ok, tc.want, tc.ok)
		}
	}
}

func TestGetRuntimeConfigString(t *testing.T) {
	ctx := context.Background()

	// Database wins, trimmed.
	SetCachedConfig("test_rs_string", testStrPtr("  from-db  "))
	t.Cleanup(func() { InvalidateCachedConfig("test_rs_string") })
	t.Setenv("RS_STRING_ENV", "from-env")
	if got, err := GetRuntimeConfigString(ctx, "test_rs_string", []string{"RS_STRING_ENV"}, "fallback"); err != nil || got != "from-db" {
		t.Fatalf("db path = (%q, %v), want from-db", got, err)
	}

	// Blank database value falls through to the environment.
	SetCachedConfig("test_rs_string_blank", testStrPtr("   "))
	t.Cleanup(func() { InvalidateCachedConfig("test_rs_string_blank") })
	if got, err := GetRuntimeConfigString(ctx, "test_rs_string_blank", []string{"RS_STRING_ENV"}, "fallback"); err != nil || got != "from-env" {
		t.Fatalf("env path = (%q, %v), want from-env", got, err)
	}

	// Nothing configured -> fallback.
	SetCachedConfig("test_rs_string_absent", nil)
	t.Cleanup(func() { InvalidateCachedConfig("test_rs_string_absent") })
	if got, err := GetRuntimeConfigString(ctx, "test_rs_string_absent", []string{"RS_STRING_MISSING"}, "fallback"); err != nil || got != "fallback" {
		t.Fatalf("fallback path = (%q, %v), want fallback", got, err)
	}
}

func TestGetRuntimeConfigBoolean(t *testing.T) {
	ctx := context.Background()

	dbCases := []struct {
		key   string
		value string
		want  bool
	}{
		{"test_rs_bool_db_a", "true", true},
		{"test_rs_bool_db_b", "1", true},
		{"test_rs_bool_db_c", "false", false},
		{"test_rs_bool_db_d", "0", false},
	}
	for _, tc := range dbCases {
		SetCachedConfig(tc.key, testStrPtr(tc.value))
		got, err := GetRuntimeConfigBoolean(ctx, tc.key, "RS_BOOL_ENV", !tc.want)
		InvalidateCachedConfig(tc.key)
		if err != nil || got != tc.want {
			t.Errorf("db %q -> (%v, %v), want %v", tc.value, got, err, tc.want)
		}
	}

	// A non-boolean database value falls through to the environment.
	SetCachedConfig("test_rs_bool_other", testStrPtr("maybe"))
	t.Cleanup(func() { InvalidateCachedConfig("test_rs_bool_other") })

	envCases := []struct {
		value string
		want  bool
	}{
		{"true", true},
		{"YES", true},
		{"1", true},
		{"false", false},
		{"No", false},
		{"0", false},
	}
	for _, tc := range envCases {
		t.Setenv("RS_BOOL_ENV", tc.value)
		got, err := GetRuntimeConfigBoolean(ctx, "test_rs_bool_other", "RS_BOOL_ENV", !tc.want)
		if err != nil || got != tc.want {
			t.Errorf("env %q -> (%v, %v), want %v", tc.value, got, err, tc.want)
		}
	}

	// Neither source -> fallback.
	t.Setenv("RS_BOOL_ENV", "garbage")
	if got, err := GetRuntimeConfigBoolean(ctx, "test_rs_bool_other", "RS_BOOL_ENV", true); err != nil || !got {
		t.Fatalf("fallback = (%v, %v), want true", got, err)
	}
}

func TestGetRuntimeConfigNumber(t *testing.T) {
	ctx := context.Background()

	// Database value inside the range wins.
	SetCachedConfig("test_rs_num_db", testStrPtr("42"))
	t.Cleanup(func() { InvalidateCachedConfig("test_rs_num_db") })
	t.Setenv("RS_NUM_ENV", "7")
	if got, err := GetRuntimeConfigNumber(ctx, "test_rs_num_db", "RS_NUM_ENV", 5, 1, 100); err != nil || got != 42 {
		t.Fatalf("db in range = (%d, %v), want 42", got, err)
	}

	// Database value outside the range falls through to the environment.
	SetCachedConfig("test_rs_num_range", testStrPtr("999"))
	t.Cleanup(func() { InvalidateCachedConfig("test_rs_num_range") })
	if got, err := GetRuntimeConfigNumber(ctx, "test_rs_num_range", "RS_NUM_ENV", 5, 1, 100); err != nil || got != 7 {
		t.Fatalf("db out of range -> env = (%d, %v), want 7", got, err)
	}

	// Non-numeric database value falls through to the environment.
	SetCachedConfig("test_rs_num_nan", testStrPtr("abc"))
	t.Cleanup(func() { InvalidateCachedConfig("test_rs_num_nan") })
	if got, err := GetRuntimeConfigNumber(ctx, "test_rs_num_nan", "RS_NUM_ENV", 5, 1, 100); err != nil || got != 7 {
		t.Fatalf("db non-numeric -> env = (%d, %v), want 7", got, err)
	}

	// An env var set to the empty string is Number("") === 0, which is in range.
	t.Setenv("RS_NUM_ENV", "")
	if got, err := GetRuntimeConfigNumber(ctx, "test_rs_num_nan", "RS_NUM_ENV", 5, 0, 100); err != nil || got != 0 {
		t.Fatalf("empty env = (%d, %v), want 0", got, err)
	}

	// Nothing usable -> fallback.
	t.Setenv("RS_NUM_ENV", "not-a-number")
	if got, err := GetRuntimeConfigNumber(ctx, "test_rs_num_nan", "RS_NUM_ENV", 5, 1, 100); err != nil || got != 5 {
		t.Fatalf("fallback = (%d, %v), want 5", got, err)
	}
}

func TestGetCombinedAdminIDAllowlist(t *testing.T) {
	ctx := context.Background()

	SetCachedConfig("test_rs_admins", testStrPtr("1, 2 ,3"))
	t.Cleanup(func() { InvalidateCachedConfig("test_rs_admins") })
	t.Setenv("RS_ADMIN_ENV", "3,4,abc,0,-5,2")

	result, err := GetCombinedAdminIDAllowlist(ctx, "test_rs_admins", []string{"RS_ADMIN_ENV"})
	if err != nil {
		t.Fatalf("GetCombinedAdminIDAllowlist: %v", err)
	}
	if !result.Configured {
		t.Error("Configured should be true when a source is present")
	}
	want := map[int64]bool{1: true, 2: true, 3: true, 4: true}
	if len(result.IDs) != len(want) {
		t.Fatalf("ids = %v, want %v", result.IDs, want)
	}
	for id := range want {
		if !result.Contains(id) {
			t.Errorf("expected id %d in allowlist", id)
		}
	}
	for _, invalid := range []int64{0, -5, 5} {
		if result.Contains(invalid) {
			t.Errorf("id %d must not be in the allowlist", invalid)
		}
	}

	// No source -> empty, not configured.
	SetCachedConfig("test_rs_admins_empty", nil)
	t.Cleanup(func() { InvalidateCachedConfig("test_rs_admins_empty") })
	empty, err := GetCombinedAdminIDAllowlist(ctx, "test_rs_admins_empty", []string{"RS_ADMIN_MISSING"})
	if err != nil {
		t.Fatalf("GetCombinedAdminIDAllowlist: %v", err)
	}
	if empty.Configured || len(empty.IDs) != 0 {
		t.Fatalf("empty result = %+v, want not configured and no ids", empty)
	}
}
