// Port of server/src/lib/trust-proxy-config.ts.
package lib

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

var trustProxyEnabledValues = map[string]bool{"true": true, "1": true, "yes": true, "on": true}
var trustProxyDisabledValues = map[string]bool{"false": true, "0": true, "no": true, "off": true}

// TrustProxyConfig describes how forwarded headers should be trusted.
// Enabled=false means proxy headers are ignored entirely.
type TrustProxyConfig struct {
	Enabled   bool
	Hops      int
	Addresses []string
}

func configuredTrustProxyHops() int {
	configured := strings.TrimSpace(os.Getenv("TRUST_PROXY_HOPS"))
	if configured != "" {
		if hops, err := strconv.Atoi(configured); err == nil && hops > 0 {
			return hops
		}
	}
	return 1
}

func isValidProxyAddress(value string) bool {
	slashIndex := strings.LastIndex(value, "/")
	if slashIndex == -1 {
		return net.ParseIP(value) != nil
	}

	address := value[:slashIndex]
	prefix := value[slashIndex+1:]
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	prefixLength, err := strconv.Atoi(prefix)
	if err != nil {
		return false
	}
	maxBits := 128
	if ip.To4() != nil {
		maxBits = 32
	}
	return prefixLength >= 0 && prefixLength <= maxBits
}

// GetTrustProxyConfig resolves the TRUST_PROXY env var into a config value.
func GetTrustProxyConfig() TrustProxyConfig {
	rawValue := os.Getenv("TRUST_PROXY")
	configured := strings.ToLower(strings.TrimSpace(rawValue))

	if configured == "" {
		return TrustProxyConfig{}
	}
	if trustProxyDisabledValues[configured] {
		return TrustProxyConfig{}
	}
	if trustProxyEnabledValues[configured] {
		return TrustProxyConfig{Enabled: true, Hops: configuredTrustProxyHops()}
	}
	if hops, err := strconv.Atoi(configured); err == nil && hops > 0 {
		return TrustProxyConfig{Enabled: true, Hops: hops}
	}

	parts := strings.Split(configured, ",")
	addresses := make([]string, 0, len(parts))
	allValid := true
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		addresses = append(addresses, trimmed)
		if !isValidProxyAddress(trimmed) {
			allValid = false
		}
	}
	if len(addresses) > 0 && allValid {
		return TrustProxyConfig{Enabled: true, Addresses: addresses}
	}

	fmt.Fprintf(os.Stderr, "[Trust Proxy] Invalid TRUST_PROXY value: %s, proxy headers will not be trusted\n", rawValue)
	return TrustProxyConfig{}
}

// GetTrustProxyEnabled reports whether any trust-proxy configuration is active.
func GetTrustProxyEnabled() bool {
	return GetTrustProxyConfig().Enabled
}
