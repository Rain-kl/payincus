// Shared route parameter parsing helpers, mirroring the strict positive-id parsing
// that the guard tests pin down in the Node routes.
package lib

import (
	"regexp"
	"strconv"
)

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, matching Number.isSafeInteger.
const maxSafeInteger = 1<<53 - 1

var positiveRouteIDPattern = regexp.MustCompile(`^[1-9]\d*$`)

// ParsePositiveRouteID accepts only canonical positive decimal ids up to the
// JavaScript safe-integer range, returning ok=false otherwise.
func ParsePositiveRouteID(value string) (int64, bool) {
	if !positiveRouteIDPattern.MatchString(value) {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed > maxSafeInteger {
		return 0, false
	}
	return parsed, true
}

// ParsePositiveIntegerQuery mirrors the Node helper: invalid values fall back and
// valid ones are clamped to max.
func ParsePositiveIntegerQuery(value string, fallback, max int) int {
	if value == "" || !positiveRouteIDPattern.MatchString(value) {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed > maxSafeInteger {
		return fallback
	}
	if parsed > int64(max) {
		return max
	}
	return int(parsed)
}
