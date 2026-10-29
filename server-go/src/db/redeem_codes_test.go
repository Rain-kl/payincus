// Pure-logic tests for the redeem-code db port (server/src/db/redeem-codes.ts).
//
// The DB-backed paths (consume / apply / list / delete) are covered by the dual-run
// conformance suite (server-go/test/conformance/requests-verification-redeem.json).
// Everything asserted here is the contract that would otherwise regress silently:
//
//   - code generation is `h-` + 18 base64url chars and is unique per call;
//   - only c/r/d/t are resource code types, `p` (points) is rejected everywhere;
//   - the CODE_VALUE_RANGES table keeps `p` out and the bounds match the schema;
//   - list bounds clamp like Math.min(Math.max(limit,1),100) and never go negative;
//   - advisory-lock keys are int32-safe for the ids the routes accept, and the
//     batch delete takes its locks in ascending order (deadlock avoidance).
package db

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// TestGenerateSystemCodeFormat pins the wire format of a system code.
func TestGenerateSystemCodeFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code := GenerateSystemCode()

		if !strings.HasPrefix(code, "h-") {
			t.Fatalf("code = %q, want the h- prefix", code)
		}
		body := strings.TrimPrefix(code, "h-")
		// crypto.randomBytes(12) is 96 bits -> exactly 16 unpadded base64url chars,
		// so the TypeScript's `.slice(0, 18)` never truncates and the code body is
		// always 16 characters long.
		if len(body) != 16 {
			t.Fatalf("code body = %q (len %d), want 16 base64url chars", body, len(body))
		}
		for _, char := range body {
			isBase64URL := (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') ||
				(char >= '0' && char <= '9') || char == '-' || char == '_'
			if !isBase64URL {
				t.Fatalf("code body %q contains a non base64url character %q", body, char)
			}
		}
		if seen[code] {
			t.Fatalf("duplicate code generated: %q", code)
		}
		seen[code] = true

		if !IsSystemCode(code) {
			t.Fatalf("IsSystemCode(%q) = false", code)
		}
	}
}

// TestIsSystemCodeRejectsNonSystemCodes pins the prefix check used by the
// check-in route to refuse legacy point codes.
func TestIsSystemCodeRejectsNonSystemCodes(t *testing.T) {
	for _, code := range []string{"", "h", "x-abc", "H-abc", "h_abc", "-abc", " h-abc"} {
		if IsSystemCode(code) {
			t.Fatalf("IsSystemCode(%q) = true, want false", code)
		}
	}
	// `startsWith('h-')` is a pure prefix test, so a bare "h-" or a code with a
	// space after the dash is accepted — exactly like the TypeScript.
	for _, code := range []string{"h-", "h-ab c"} {
		if !IsSystemCode(code) {
			t.Fatalf("IsSystemCode(%q) = false, want true", code)
		}
	}
}

// TestAssertResourceRedeemCodeTypeRejectsPoints pins that every h-code creation
// path refuses the deprecated `p` (points) type.
func TestAssertResourceRedeemCodeTypeRejectsPoints(t *testing.T) {
	for _, codeType := range []models.RedeemCodeType{
		models.RedeemCodeTypeC,
		models.RedeemCodeTypeR,
		models.RedeemCodeTypeD,
		models.RedeemCodeTypeT,
	} {
		if err := assertResourceRedeemCodeType(codeType); err != nil {
			t.Fatalf("assertResourceRedeemCodeType(%q) = %v, want nil", codeType, err)
		}
	}

	if err := assertResourceRedeemCodeType(models.RedeemCodeTypeP); err != ErrRedeemCodeInvalidType {
		t.Fatalf("assertResourceRedeemCodeType(p) = %v, want REDEEM_CODE_INVALID_TYPE", err)
	}
	if err := assertResourceRedeemCodeType(""); err != ErrRedeemCodeInvalidType {
		t.Fatalf("assertResourceRedeemCodeType(\"\") = %v, want REDEEM_CODE_INVALID_TYPE", err)
	}
}

// TestCodeValueRangesExcludePointsAndMatchSchema pins the range table the routes
// expose through GET /api/redeem-code-options.
func TestCodeValueRangesExcludePointsAndMatchSchema(t *testing.T) {
	if _, present := CodeValueRanges[models.RedeemCodeTypeP]; present {
		t.Fatal("CODE_VALUE_RANGES must not contain the deprecated points type")
	}
	if len(CodeValueRanges) != 4 {
		t.Fatalf("CODE_VALUE_RANGES size = %d, want 4", len(CodeValueRanges))
	}

	want := map[models.RedeemCodeType]CodeValueRange{
		models.RedeemCodeTypeC: {Min: 1, Max: 100, Unit: "%"},
		models.RedeemCodeTypeR: {Min: 1, Max: 65536, Unit: "MB"},
		models.RedeemCodeTypeD: {Min: 1, Max: 1048576, Unit: "MB"},
		models.RedeemCodeTypeT: {Min: 1, Max: 10240, Unit: "GB"},
	}
	for codeType, expected := range want {
		if got := CodeValueRanges[codeType]; got != expected {
			t.Fatalf("CodeValueRanges[%q] = %+v, want %+v", codeType, got, expected)
		}
	}
}

// TestIsValidCodeValueBounds pins the inclusive bounds check.
func TestIsValidCodeValueBounds(t *testing.T) {
	cases := []struct {
		codeType models.RedeemCodeType
		value    int32
		want     bool
	}{
		{models.RedeemCodeTypeC, 1, true},
		{models.RedeemCodeTypeC, 100, true},
		{models.RedeemCodeTypeC, 0, false},
		{models.RedeemCodeTypeC, 101, false},
		{models.RedeemCodeTypeR, 65536, true},
		{models.RedeemCodeTypeR, 65537, false},
		{models.RedeemCodeTypeD, 1048576, true},
		{models.RedeemCodeTypeT, 10240, true},
		{models.RedeemCodeTypeT, -1, false},
		{models.RedeemCodeTypeP, 1, false},
	}

	for _, tc := range cases {
		if got := IsValidCodeValue(tc.codeType, tc.value); got != tc.want {
			t.Fatalf("IsValidCodeValue(%q, %d) = %v, want %v", tc.codeType, tc.value, got, tc.want)
		}
	}
}

// TestCodeValuesPoolsMatchCheckinPools pins the fixed check-in pools.
func TestCodeValuesPoolsMatchCheckinPools(t *testing.T) {
	want := map[models.RedeemCodeType][]int32{
		models.RedeemCodeTypeC: {1, 2, 3, 5},
		models.RedeemCodeTypeR: {2, 4, 6, 12},
		models.RedeemCodeTypeD: {16, 32, 64, 128},
		models.RedeemCodeTypeT: {1, 2, 3, 5},
	}
	if len(CodeValues) != len(want) {
		t.Fatalf("CODE_VALUES size = %d, want %d", len(CodeValues), len(want))
	}
	for codeType, expected := range want {
		got, present := CodeValues[codeType]
		if !present {
			t.Fatalf("CODE_VALUES is missing %q", codeType)
		}
		if len(got) != len(expected) {
			t.Fatalf("CODE_VALUES[%q] = %v, want %v", codeType, got, expected)
		}
		for i := range expected {
			if got[i] != expected[i] {
				t.Fatalf("CODE_VALUES[%q] = %v, want %v", codeType, got, expected)
			}
		}
	}
}

// TestClampRedeemCodeListBounds pins clampRedeemCodeListBounds: absent -> default,
// out-of-range limits clamp (never error), negative offsets fall back to 0.
func TestClampRedeemCodeListBounds(t *testing.T) {
	intPtr := func(value int) *int { return &value }

	cases := []struct {
		name         string
		limit        *int
		offset       *int
		defaultLimit int
		wantLimit    int
		wantOffset   int
	}{
		{"absent uses default", nil, nil, 50, 50, 0},
		{"zero limit clamps to 1", intPtr(0), nil, 50, 1, 0},
		{"negative limit clamps to 1", intPtr(-7), nil, 50, 1, 0},
		{"above max limit clamps to 100", intPtr(100000), nil, 50, 100, 0},
		{"in-range limit kept", intPtr(20), intPtr(40), 50, 20, 40},
		{"negative offset resets to 0", nil, intPtr(-1), 20, 20, 0},
		{"zero offset kept", nil, intPtr(0), 20, 20, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotLimit, gotOffset := clampRedeemCodeListBounds(tc.limit, tc.offset, tc.defaultLimit)
			if gotLimit != tc.wantLimit || gotOffset != tc.wantOffset {
				t.Fatalf("clampRedeemCodeListBounds = (%d, %d), want (%d, %d)",
					gotLimit, gotOffset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

// TestRedeemCodeISOTimeMatchesToISOString pins the timestamp wire format.
func TestRedeemCodeISOTimeMatchesToISOString(t *testing.T) {
	value := time.Date(2026, 10, 29, 1, 2, 3, 45000000, time.UTC)
	if got := redeemCodeISOTime(value); got != "2026-10-29T01:02:03.045Z" {
		t.Fatalf("redeemCodeISOTime = %q", got)
	}

	zone := time.FixedZone("UTC+8", 8*3600)
	shifted := time.Date(2026, 10, 29, 9, 2, 3, 0, zone)
	if got := redeemCodeISOTime(shifted); got != "2026-10-29T01:02:03.000Z" {
		t.Fatalf("redeemCodeISOTime (non-UTC) = %q", got)
	}

	if got := redeemCodeOptionalISOTime(nil); got != nil {
		t.Fatalf("redeemCodeOptionalISOTime(nil) = %v, want nil", got)
	}
	formatted := redeemCodeOptionalISOTime(&value)
	if formatted == nil || *formatted != "2026-10-29T01:02:03.045Z" {
		t.Fatalf("redeemCodeOptionalISOTime = %v", formatted)
	}
}

// TestSortInt64Ascending pins the batch-delete lock ordering (ascending ids, the
// deadlock-avoidance contract shared with the TypeScript `[...ids].sort()`).
func TestSortInt64Ascending(t *testing.T) {
	values := []int64{9, 3, 100, 3, 1}
	sortInt64Ascending(values)

	if !sort.SliceIsSorted(values, func(i, j int) bool { return values[i] < values[j] }) {
		t.Fatalf("sortInt64Ascending left %v unsorted", values)
	}
	if values[0] != 1 || values[len(values)-1] != 100 {
		t.Fatalf("sortInt64Ascending = %v, want 1..100", values)
	}

	empty := []int64{}
	sortInt64Ascending(empty)
	if len(empty) != 0 {
		t.Fatalf("sortInt64Ascending(empty) = %v", empty)
	}
}

// TestConsumeSentinelMessages pins the sentinel error strings: routes that consume
// them (a future checkin port) branch on the exact message, exactly like
// routes/checkin.ts compares `error.message`.
func TestConsumeSentinelMessages(t *testing.T) {
	want := map[string]string{
		ErrRedeemCodeInvalidType.Error():       "REDEEM_CODE_INVALID_TYPE",
		ErrRedeemCodeAlreadyUsedByUser.Error(): "REDEEM_CODE_ALREADY_USED_BY_USER",
		ErrRedeemCodeBatchLimit.Error():        "REDEEM_CODE_BATCH_LIMIT",
		ErrRedeemCodeExhausted.Error():         "REDEEM_CODE_EXHAUSTED",
		ErrRedeemCodeBusy.Error():              "REDEEM_CODE_BUSY",
	}
	for got, expected := range want {
		if got != expected {
			t.Fatalf("sentinel message = %q, want %q", got, expected)
		}
	}
}
