package middleware

import (
	"net"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/rain-kl/payincus/server-go/src/lib"
)

// ClientIP resolves the caller IP honouring the TRUST_PROXY configuration, mirroring
// fastify's request.ip semantics.
func ClientIP(c *gin.Context) string {
	remote := hostOnly(c.Request.RemoteAddr)
	config := lib.GetTrustProxyConfig()
	if !config.Enabled {
		return remote
	}

	forwarded := c.GetHeader("X-Forwarded-For")
	if forwarded == "" {
		return remote
	}

	chain := []string{}
	for _, part := range strings.Split(forwarded, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			chain = append(chain, trimmed)
		}
	}
	if len(chain) == 0 {
		return remote
	}

	if len(config.Addresses) > 0 {
		// Walk from the nearest hop outward, skipping trusted proxy addresses.
		for i := len(chain) - 1; i >= 0; i-- {
			if !isTrustedProxy(chain[i], config.Addresses) {
				return chain[i]
			}
		}
		return remote
	}

	hops := config.Hops
	if hops < 1 {
		hops = 1
	}
	if len(chain) >= hops {
		return chain[len(chain)-hops]
	}
	return chain[0]
}

func hostOnly(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}

func isTrustedProxy(address string, trusted []string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	for _, entry := range trusted {
		if strings.Contains(entry, "/") {
			if _, cidr, err := net.ParseCIDR(entry); err == nil && cidr.Contains(ip) {
				return true
			}
			continue
		}
		if parsed := net.ParseIP(entry); parsed != nil && parsed.Equal(ip) {
			return true
		}
	}
	return false
}
