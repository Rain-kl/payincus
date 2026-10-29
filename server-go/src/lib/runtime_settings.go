// Port of server/src/lib/runtime-settings.ts.
//
// Runtime knobs resolve in the order "database -> environment -> fallback":
// administrators tune values in system_config, while the environment variables are
// the deployment-level default. Every read goes through db.GetSystemConfig, so
// non-sensitive keys are served from the process-local config cache
// (lib/config_cache.go) exactly as in the TypeScript original.
package lib

import (
	"context"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/rain-kl/payincus/server-go/src/db"
)

// envFirst mirrors envFirst(): the first non-empty, trimmed environment value.
func envFirst(keys []string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

// splitCSV mirrors splitCsv: split on commas, trim, drop empties.
func splitCSV(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// GetRuntimeConfigString mirrors getRuntimeConfigString.
func GetRuntimeConfigString(ctx context.Context, configKey string, envKeys []string, fallback string) (string, error) {
	dbValue, err := db.GetSystemConfig(ctx, configKey, true)
	if err != nil {
		return "", err
	}
	if dbValue != nil {
		if trimmed := strings.TrimSpace(*dbValue); trimmed != "" {
			return trimmed, nil
		}
	}
	if envValue := envFirst(envKeys); envValue != "" {
		return envValue, nil
	}
	return fallback, nil
}

// GetRuntimeConfigBoolean mirrors getRuntimeConfigBoolean. The database value is
// matched raw (not trimmed), and the environment value is trimmed and lower-cased,
// just like the TypeScript implementation.
func GetRuntimeConfigBoolean(ctx context.Context, configKey, envKey string, fallback bool) (bool, error) {
	dbValue, err := db.GetSystemConfig(ctx, configKey, true)
	if err != nil {
		return false, err
	}
	if dbValue != nil {
		switch *dbValue {
		case "true", "1":
			return true, nil
		case "false", "0":
			return false, nil
		}
	}

	switch strings.ToLower(strings.TrimSpace(os.Getenv(envKey))) {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	}

	return fallback, nil
}

// GetRuntimeConfigNumber mirrors getRuntimeConfigNumber. The database path uses
// parseInt semantics plus an integer/range guard; the environment path uses
// Number() semantics (so an env var set to the empty string yields 0, exactly like
// Number("")).
func GetRuntimeConfigNumber(ctx context.Context, configKey, envKey string, fallback, min, max int) (int, error) {
	dbValue, err := db.GetSystemConfig(ctx, configKey, true)
	if err != nil {
		return 0, err
	}
	if dbValue != nil {
		if parsed, ok := parseIntJSValue(*dbValue); ok && parsed >= int64(min) && parsed <= int64(max) {
			return int(parsed), nil
		}
	}

	if envRaw, present := os.LookupEnv(envKey); present {
		envValue := jsToNumber(envRaw)
		if isJSInteger(envValue) && envValue >= float64(min) && envValue <= float64(max) {
			return int(envValue), nil
		}
	}

	return fallback, nil
}

// CombinedAdminIDAllowlist mirrors the getCombinedAdminIdAllowlist() return shape.
type CombinedAdminIDAllowlist struct {
	IDs        map[int64]bool
	Configured bool
}

// Contains reports whether id is in the allowlist.
func (a CombinedAdminIDAllowlist) Contains(id int64) bool {
	return a.IDs[id]
}

// GetCombinedAdminIDAllowlist mirrors getCombinedAdminIdAllowlist: it merges the
// database value and the environment value (comma-separated admin ids) into one
// set, and reports whether either source supplied a value.
func GetCombinedAdminIDAllowlist(ctx context.Context, configKey string, envKeys []string) (CombinedAdminIDAllowlist, error) {
	dbValue, err := db.GetSystemConfig(ctx, configKey, true)
	if err != nil {
		return CombinedAdminIDAllowlist{}, err
	}

	rawValues := make([]string, 0, 2)
	if dbValue != nil && strings.TrimSpace(*dbValue) != "" {
		rawValues = append(rawValues, *dbValue)
	}
	if envValue := envFirst(envKeys); envValue != "" {
		rawValues = append(rawValues, envValue)
	}

	ids := make(map[int64]bool)
	for _, raw := range rawValues {
		for _, part := range splitCSV(raw) {
			value := jsToNumber(part)
			if isJSSafeInteger(value) && value > 0 {
				ids[int64(value)] = true
			}
		}
	}

	return CombinedAdminIDAllowlist{IDs: ids, Configured: len(rawValues) > 0}, nil
}

// jsToNumber mirrors the JavaScript Number(value) coercion for the string inputs
// this package sees. NaN is returned for anything Number() would turn into NaN.
func jsToNumber(value string) float64 {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		// Number("") === 0 and Number("   ") === 0.
		return 0
	}

	switch trimmed {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}

	lower := strings.ToLower(trimmed)
	if len(lower) > 2 && lower[0] == '0' {
		switch lower[1] {
		case 'x', 'b', 'o':
			base := 16
			switch lower[1] {
			case 'b':
				base = 2
			case 'o':
				base = 8
			}
			parsed, err := strconv.ParseUint(lower[2:], base, 64)
			if err != nil {
				return math.NaN()
			}
			return float64(parsed)
		}
	}

	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return math.NaN()
	}
	return parsed
}

// isJSInteger mirrors Number.isInteger.
func isJSInteger(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value == math.Trunc(value)
}

// isJSSafeInteger mirrors Number.isSafeInteger.
func isJSSafeInteger(value float64) bool {
	return isJSInteger(value) && math.Abs(value) <= float64(maxSafeInteger)
}

// parseIntJSValue mirrors parseInt(value, 10) with an explicit "not a number"
// signal (db's own parseIntJS is unexported and cannot be reused from lib).
func parseIntJSValue(value string) (int64, bool) {
	trimmed := strings.TrimLeft(value, " \t\n\r\v\f")
	if trimmed == "" {
		return 0, false
	}

	end := 0
	if trimmed[0] == '+' || trimmed[0] == '-' {
		end = 1
	}
	digitsStart := end
	for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
		end++
	}
	if end == digitsStart {
		return 0, false
	}

	parsed, err := strconv.ParseInt(trimmed[:end], 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}
