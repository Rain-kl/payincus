// Port of server/src/lib/cookie-config.ts (SEC005).
package lib

import (
	"fmt"
	"os"
	"strings"
)

// RefreshTokenCookieName is the cookie that carries the refresh token.
const RefreshTokenCookieName = "refreshToken"

// RefreshTokenCookieMaxAgeSeconds matches COOKIE_CONFIG.REFRESH_TOKEN.maxAge (30 days).
const RefreshTokenCookieMaxAgeSeconds = 30 * 24 * 60 * 60

// IsProductionEnv reports whether NODE_ENV is "production".
func IsProductionEnv() bool {
	return os.Getenv("NODE_ENV") == "production"
}

// GetCookieSecure resolves the Secure cookie attribute.
func GetCookieSecure() bool {
	if value := os.Getenv("COOKIE_SECURE"); value != "" {
		switch strings.ToLower(value) {
		case "true":
			return true
		case "false":
			return false
		}
		fmt.Fprintf(os.Stderr, "[Cookie Config] Invalid COOKIE_SECURE value: %s, using default\n", value)
	}
	return IsProductionEnv()
}

// GetCookieSameSite resolves the SameSite attribute (strict|lax|none).
func GetCookieSameSite() string {
	if value := os.Getenv("COOKIE_SAME_SITE"); value != "" {
		normalized := strings.ToLower(value)
		if normalized == "strict" || normalized == "lax" || normalized == "none" {
			if normalized == "none" && !GetCookieSecure() {
				fmt.Fprintln(os.Stderr, "[Cookie Config] SameSite=none requires Secure=true, falling back to lax")
				return "lax"
			}
			return normalized
		}
		fmt.Fprintf(os.Stderr, "[Cookie Config] Invalid COOKIE_SAME_SITE value: %s, using default\n", value)
	}
	if IsProductionEnv() {
		return "lax"
	}
	return "strict"
}

// GetCookieDomain returns the configured cookie domain, if any.
func GetCookieDomain() string {
	return os.Getenv("COOKIE_DOMAIN")
}

// CookieOptions is the serialized cookie configuration shared by set/clear paths.
type CookieOptions struct {
	HTTPOnly bool
	Secure   bool
	SameSite string
	Path     string
	MaxAge   int
	Domain   string
}

// GetRefreshTokenCookieOptions builds the refresh-token cookie options.
func GetRefreshTokenCookieOptions() CookieOptions {
	return CookieOptions{
		HTTPOnly: true,
		Secure:   GetCookieSecure(),
		SameSite: GetCookieSameSite(),
		Path:     "/",
		MaxAge:   RefreshTokenCookieMaxAgeSeconds,
		Domain:   GetCookieDomain(),
	}
}

// GetClearCookieOptions builds the options used when clearing the cookie.
func GetClearCookieOptions() CookieOptions {
	return CookieOptions{Path: "/", Domain: GetCookieDomain()}
}

// LogCookieConfig prints the effective cookie configuration at startup.
func LogCookieConfig() {
	config := GetRefreshTokenCookieOptions()
	domain := config.Domain
	if domain == "" {
		domain = "(not set)"
	}
	environment := "development"
	if IsProductionEnv() {
		environment = "production"
	}
	fmt.Printf("[Cookie Config] Current settings: secure=%t sameSite=%s domain=%s maxAge=%ds (%d days) environment=%s\n",
		config.Secure, config.SameSite, domain, config.MaxAge, config.MaxAge/86400, environment)
}
