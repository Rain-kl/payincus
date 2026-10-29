// Port of server/src/config/rate-limit.ts helpers. The rule/whitelist data lives in
// rate_limit_gen.go, generated from the TypeScript source.
package config

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ruleCache   sync.Map // string -> *regexp.Regexp
	prefixCache sync.Map // string -> *regexp.Regexp
)

// pathPattern compiles a rule path where '*' matches a single path segment.
func pathPattern(pattern string) *regexp.Regexp {
	if cached, ok := ruleCache.Load(pattern); ok {
		return cached.(*regexp.Regexp)
	}
	compiled := regexp.MustCompile("^" + segmentPattern(pattern) + "$")
	ruleCache.Store(pattern, compiled)
	return compiled
}

// prefixPattern compiles a whitelist prefix, anchored at the start only.
func prefixPattern(prefix string) *regexp.Regexp {
	if cached, ok := prefixCache.Load(prefix); ok {
		return cached.(*regexp.Regexp)
	}
	compiled := regexp.MustCompile("^" + segmentPattern(prefix))
	prefixCache.Store(prefix, compiled)
	return compiled
}

func segmentPattern(pattern string) string {
	parts := strings.Split(pattern, "*")
	quoted := make([]string, len(parts))
	for i, part := range parts {
		quoted[i] = regexp.QuoteMeta(part)
	}
	return strings.Join(quoted, "[^/]+")
}

// FindRateLimitRule returns the first rule matching path+method, or nil.
func FindRateLimitRule(path, method string) *RateLimitRule {
	for i := range RateLimitRules {
		rule := &RateLimitRules[i]
		if rule.Method != "*" && rule.Method != method {
			continue
		}
		if pathPattern(rule.Path).MatchString(path) {
			return rule
		}
	}
	return nil
}

// IsWhitelisted reports whether a path bypasses rate limiting entirely.
func IsWhitelisted(path string) bool {
	for _, exact := range RateLimitWhitelistExactPaths {
		if path == exact {
			return true
		}
	}
	for _, prefix := range RateLimitWhitelistPathPrefixes {
		if prefixPattern(prefix).MatchString(path) {
			return true
		}
	}
	return false
}

// ParseTimeWindow converts a fastify-style window ("1 minute", "30 seconds", ...)
// into a duration. Unknown units fall back to one minute.
func ParseTimeWindow(value string) time.Duration {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return time.Minute
	}
	amount, err := strconv.Atoi(fields[0])
	if err != nil || amount <= 0 {
		return time.Minute
	}
	unit := ""
	if len(fields) > 1 {
		unit = strings.ToLower(fields[1])
	}
	switch {
	case strings.HasPrefix(unit, "second"):
		return time.Duration(amount) * time.Second
	case strings.HasPrefix(unit, "minute"):
		return time.Duration(amount) * time.Minute
	case strings.HasPrefix(unit, "hour"):
		return time.Duration(amount) * time.Hour
	case strings.HasPrefix(unit, "day"):
		return time.Duration(amount) * 24 * time.Hour
	default:
		return time.Minute
	}
}
