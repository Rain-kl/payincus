// Runtime overrides for the generated rate-limit tables, mirroring the Node
// backend's RATE_LIMIT_CONFIG / RATE_LIMIT_DISABLED environment variables.
//
// RATE_LIMIT_CONFIG points at a JSON file:
//
//	{ "global": { "max": 300, "timeWindow": "1 minute" },
//	  "rules": [ { "path": "/api/auth/login", "method": "POST", "max": 5,
//	               "timeWindow": "1 minute" } ] }
//
// rules merge by (path, method): an existing entry is replaced, a new key is
// appended. A malformed file is a startup error, never a silent skip.
//
// RATE_LIMIT_DISABLED=1 (or "true") turns every limiter off — the parity test
// harness relies on it so the two backends under comparison never 429.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// rateLimitOverridesApplied guards ApplyRateLimitOverrides so double calls
// cannot re-apply file deltas on the mutated tables.
var rateLimitOverridesApplied bool

// ApplyRateLimitOverrides loads RATE_LIMIT_CONFIG into the generated tables.
// It runs once at startup; calling it again is a no-op.
func ApplyRateLimitOverrides() error {
	if rateLimitOverridesApplied {
		return nil
	}
	rateLimitOverridesApplied = true

	path := os.Getenv("RATE_LIMIT_CONFIG")
	if path == "" {
		return nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("RATE_LIMIT_CONFIG 指向的文件无法读取: %s (%w)", path, err)
	}

	var parsed struct {
		Global *struct {
			Max        *int    `json:"max"`
			TimeWindow *string `json:"timeWindow"`
		} `json:"global"`
		Rules []RateLimitRule `json:"rules"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("RATE_LIMIT_CONFIG 文件不是合法 JSON: %s (%w)", path, err)
	}

	if parsed.Global != nil {
		if parsed.Global.Max != nil {
			GlobalRateLimit.Max = *parsed.Global.Max
		}
		if parsed.Global.TimeWindow != nil {
			GlobalRateLimit.TimeWindow = *parsed.Global.TimeWindow
		}
	}

	for _, override := range parsed.Rules {
		if override.Path == "" || override.Method == "" || override.Max == 0 || override.TimeWindow == "" {
			return fmt.Errorf("RATE_LIMIT_CONFIG 规则缺少必填字段 (path/method/max/timeWindow): %+v", override)
		}
		replaced := false
		for i := range RateLimitRules {
			if RateLimitRules[i].Path == override.Path && RateLimitRules[i].Method == override.Method {
				RateLimitRules[i] = override
				replaced = true
				break
			}
		}
		if !replaced {
			RateLimitRules = append(RateLimitRules, override)
		}
	}
	return nil
}

// RateLimitDisabled reports whether RATE_LIMIT_DISABLED is set to the two
// spellings the codebase treats as "on" (same rule as DISABLE_SCHEDULERS).
func RateLimitDisabled() bool {
	switch os.Getenv("RATE_LIMIT_DISABLED") {
	case "1", "true":
		return true
	default:
		return false
	}
}
