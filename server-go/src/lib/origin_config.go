// Port of server/src/lib/origin-config.ts.
package lib

import (
	"net/url"
	"os"
	"strings"
)

func parseOriginList(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// NormalizeOrigin reduces an origin URL to scheme://host[:port]; empty when unparseable.
func NormalizeOrigin(origin string) string {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// GetConfiguredOrigins returns the deduplicated, normalized origins from env config.
func GetConfiguredOrigins() []string {
	joined := strings.Join([]string{
		os.Getenv("CORS_ORIGINS"),
		os.Getenv("FRONTEND_URL"),
		os.Getenv("ADMIN_FRONTEND_URL"),
	}, ",")

	seen := map[string]bool{}
	result := []string{}
	for _, raw := range parseOriginList(joined) {
		normalized := NormalizeOrigin(raw)
		if normalized == "" || seen[normalized] {
			continue
		}
		seen[normalized] = true
		result = append(result, normalized)
	}
	return result
}

func defaultDevOrigins() []string {
	return []string{
		"http://localhost:3000",
		"http://127.0.0.1:3000",
		"http://localhost:3002",
		"http://127.0.0.1:3002",
	}
}

// GetCorsOrigins returns the allowed CORS origins. An empty slice means "deny all",
// mirroring the `false` return in the Node implementation.
func GetCorsOrigins() []string {
	configured := GetConfiguredOrigins()
	if IsProductionEnv() {
		return configured
	}
	seen := map[string]bool{}
	for _, origin := range configured {
		seen[origin] = true
	}
	result := append([]string{}, configured...)
	for _, origin := range defaultDevOrigins() {
		if !seen[origin] {
			seen[origin] = true
			result = append(result, origin)
		}
	}
	return result
}

// GetAllowedWebSocketOrigins returns the origins permitted to open WebSockets.
func GetAllowedWebSocketOrigins() []string {
	configured := GetConfiguredOrigins()
	if IsProductionEnv() {
		return configured
	}
	seen := map[string]bool{}
	for _, origin := range configured {
		seen[origin] = true
	}
	result := append([]string{}, configured...)
	extra := append(defaultDevOrigins(), "http://localhost:3001", "http://127.0.0.1:3001")
	for _, origin := range extra {
		if !seen[origin] {
			seen[origin] = true
			result = append(result, origin)
		}
	}
	return result
}
