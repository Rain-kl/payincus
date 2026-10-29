// Port of server/src/lib/bigint-input.ts.
//
// Both helpers take `unknown` in TypeScript because the value comes straight out
// of a request body. The tri-state return is the whole point: `nil` means "the
// caller sent nothing / sent empty" (→ SQL NULL), while "invalid" means the
// caller sent something that is not a non-negative Postgres bigint. Callers must
// distinguish those two, so the second return value is an explicit validity flag
// rather than an error.
package lib

import (
	"regexp"
	"strings"
)

// PostgresBigIntMax mirrors POSTGRES_BIGINT_MAX (9223372036854775807n).
const PostgresBigIntMax uint64 = 9223372036854775807

// bigintInputPattern mirrors /^\d+$/ applied to the trimmed value.
var bigintInputPattern = regexp.MustCompile(`^\d+$`)

// ParseNullablePostgresBigIntInput mirrors parseNullablePostgresBigIntInput.
//
// Returns (value, present, valid):
//   - present=false          → the input was null/undefined/”  → SQL NULL
//   - present=true, valid=false → the input was malformed        → caller must 400
//   - present=true, valid=true  → value is the parsed bigint
//
// A non-string (number, bool, object) is *invalid* rather than coerced, exactly
// like the TypeScript `typeof value !== 'string'` guard.
func ParseNullablePostgresBigIntInput(value any) (result uint64, present bool, valid bool) {
	if value == nil {
		return 0, false, true
	}
	text, ok := value.(string)
	if !ok {
		return 0, true, false
	}
	if text == "" {
		return 0, false, true
	}
	return parseBigIntDigits(text)
}

// ParseRequiredPostgresBigIntInput mirrors parseRequiredPostgresBigIntInput.
//
// Unlike the nullable variant an empty string is *invalid* here, because the
// caller declared the field required.
func ParseRequiredPostgresBigIntInput(value any) (result uint64, valid bool) {
	if value == nil {
		return 0, false
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return 0, false
	}
	parsed, _, ok := parseBigIntDigits(text)
	if !ok {
		return 0, false
	}
	return parsed, true
}

// parseBigIntDigits trims, requires ^\d+$ and rejects anything above the bigint
// ceiling. Mirrors `const parsed = BigInt(trimmed); parsed <= POSTGRES_BIGINT_MAX`.
func parseBigIntDigits(text string) (uint64, bool, bool) {
	trimmed := strings.TrimSpace(text)
	if !bigintInputPattern.MatchString(trimmed) {
		return 0, true, false
	}

	// Accumulate by hand instead of via a widening parse: a 20+ digit input would
	// wrap uint64 (and could wrap back under the ceiling), so overflow is checked
	// before each multiply.
	var parsed uint64
	for _, digit := range []byte(trimmed) {
		value := uint64(digit - '0')
		if parsed > (PostgresBigIntMax-value)/10 {
			return 0, true, false
		}
		parsed = parsed*10 + value
	}
	return parsed, true, true
}
