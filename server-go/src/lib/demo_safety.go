// Port of server/src/lib/demo-safety.ts. Pure functions over request facts so the
// same logic can be unit-tested and reused by the Gin middleware.
package lib

import (
	"regexp"
	"strings"
)

var demoHosts = map[string]bool{
	"demo.payincus.com":      true,
	"demoadmin.payincus.com": true,
}

var demoUsernames = map[string]bool{"admin": true, "demo": true}

var demoEmails = map[string]bool{
	"admin@payincus.com": true,
	"demo@payincus.com":  true,
}

// Demo redaction constants.
const (
	DemoRedactedIP              = "已隐藏"
	DemoAccountProtectedMessage = "Demo account credentials cannot be changed"
	DemoReadOnlyMessage         = "Demo site is read-only"
)

var demoAllowedMutations = map[string]bool{
	"POST /api/auth/login":   true,
	"POST /api/auth/logout":  true,
	"POST /api/auth/refresh": true,
}

var sensitiveKeyParts = []string{
	"password", "passwordhash", "secret", "token", "apikey", "accesskey",
	"privatekey", "publickey", "webhook", "merchantkey", "merchantsecret",
	"clientsecret", "botkey", "bottoken", "sshkey", "credential",
}

var sensitiveIPKeys = map[string]bool{
	"ip": true, "ipv4": true, "ipv6": true, "ipaddress": true, "clientip": true,
	"remoteip": true, "loginip": true, "publicip": true, "bindip": true,
	"natpublicip": true, "natbindip": true, "hostname": true, "hostaddress": true,
	"sshhost": true, "sshport": true,
}

var (
	trailingPortRe = regexp.MustCompile(`:\d+$`)
	ipv4Re         = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\b`)
	emailRe        = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
	keyNoiseRe     = regexp.MustCompile(`[-_\s]`)
)

// NormalizeHost lowercases a Host/X-Forwarded-Host value and strips any port.
func NormalizeHost(value string) string {
	if value == "" {
		return ""
	}
	first := strings.Split(value, ",")[0]
	first = trailingPortRe.ReplaceAllString(strings.TrimSpace(first), "")
	return strings.ToLower(first)
}

// IsDemoRequest reports whether the request targets a demo host.
func IsDemoRequest(forwardedHost, host string) bool {
	return demoHosts[NormalizeHost(forwardedHost)] || demoHosts[NormalizeHost(host)]
}

// IsDemoMutationAllowed reports whether a mutating method is whitelisted on demo hosts.
func IsDemoMutationAllowed(method, url string) bool {
	path := url
	if idx := strings.IndexByte(url, '?'); idx >= 0 {
		path = url[:idx]
	}
	return demoAllowedMutations[strings.ToUpper(method)+" "+path]
}

// ShouldBlockDemoMutation reports whether a demo request must be rejected as read-only.
func ShouldBlockDemoMutation(forwardedHost, host, method, url string) bool {
	if !IsDemoRequest(forwardedHost, host) {
		return false
	}
	if !strings.HasPrefix(url, "/api/") {
		return false
	}
	upper := strings.ToUpper(method)
	if upper != "POST" && upper != "PUT" && upper != "PATCH" && upper != "DELETE" {
		return false
	}
	return !IsDemoMutationAllowed(method, url)
}

// IsDemoAccount reports whether the account is one of the seeded demo accounts.
func IsDemoAccount(username, email string) bool {
	return demoUsernames[strings.ToLower(username)] || demoEmails[strings.ToLower(email)]
}

// ShouldProtectDemoAccount reports whether demo credentials must be frozen.
func ShouldProtectDemoAccount(forwardedHost, host, username, email string) bool {
	return IsDemoRequest(forwardedHost, host) && IsDemoAccount(username, email)
}

func isSensitiveDemoKey(key string) bool {
	normalized := strings.ToLower(keyNoiseRe.ReplaceAllString(key, ""))
	if sensitiveIPKeys[normalized] {
		return true
	}
	for _, part := range sensitiveKeyParts {
		if strings.Contains(normalized, part) {
			return true
		}
	}
	return false
}

func redactDemoStringValue(value string) string {
	value = ipv4Re.ReplaceAllString(value, DemoRedactedIP)
	return emailRe.ReplaceAllString(value, DemoRedactedIP)
}

func redactDemoJSONValue(value any, key string, depth int) any {
	if depth > 12 {
		return value
	}

	if key != "" && isSensitiveDemoKey(key) {
		return DemoRedactedIP
	}

	switch v := value.(type) {
	case string:
		return redactDemoStringValue(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactDemoJSONValue(item, "", depth+1)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for entryKey, entryValue := range v {
			out[entryKey] = redactDemoJSONValue(entryValue, entryKey, depth+1)
		}
		return out
	default:
		return value
	}
}

// RedactDemoJSONPayload scrubs IPs, emails and sensitive keys from a decoded JSON body.
func RedactDemoJSONPayload(payload any) any {
	return redactDemoJSONValue(payload, "", 0)
}
