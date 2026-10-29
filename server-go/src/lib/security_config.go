// Port of server/src/lib/security-config.ts. security.ts re-exports
// checkJwtConfig, so the Node module is the single import site for startup config
// validation.
package lib

import (
	"os"
	"regexp"
	"strings"
)

var insecureSecretPatterns = []string{
	"dev-secret",
	"dev_secret",
	"development",
	"change-in-production",
	"change_me",
	"changeme",
	"replace",
	"generateuniquevaluebeforedeploy",
	"secret",
	"password",
	"123456",
	"test",
	"demo",
	"example",
	"default",
}

var (
	secretUpperRe   = regexp.MustCompile(`[A-Z]`)
	secretLowerRe   = regexp.MustCompile(`[a-z]`)
	secretNumberRe  = regexp.MustCompile(`[0-9]`)
	secretSpecialRe = regexp.MustCompile(`[!@#$%^&*()_+\-=\[\]{};':"\\|,.<>\/?]`)
)

// SecretCheckResult mirrors `{ valid: boolean; warnings: string[] }`.
type SecretCheckResult struct {
	Valid    bool
	Warnings []string
}

type secretRequirement struct {
	Name                        string
	Value                       string
	MissingProductionMessage    string
	MissingDevelopmentMessage   string
	WeakDevelopmentMessage      string
	WeakProductionMessage       string
	ComplexityDevelopmentMsg    string
	ComplexityProductionMessage string
}

// validateSecret mirrors the private validateSecret helper.
func validateSecret(input secretRequirement) SecretCheckResult {
	warnings := []string{}

	if input.Value == "" {
		if IsProductionEnv() {
			return SecretCheckResult{Valid: false, Warnings: []string{input.MissingProductionMessage}}
		}
		warnings = append(warnings, input.MissingDevelopmentMessage)
		return SecretCheckResult{Valid: true, Warnings: warnings}
	}

	if len(input.Value) < 32 {
		if IsProductionEnv() {
			return SecretCheckResult{Valid: false, Warnings: []string{input.WeakProductionMessage}}
		}
		warnings = append(warnings, input.WeakDevelopmentMessage)
	}

	secretLower := strings.ToLower(input.Value)
	matchedPattern := ""
	for _, pattern := range insecureSecretPatterns {
		if strings.Contains(secretLower, pattern) {
			matchedPattern = pattern
			break
		}
	}
	if matchedPattern != "" {
		if IsProductionEnv() {
			return SecretCheckResult{
				Valid:    false,
				Warnings: []string{"Cannot use insecure " + input.Name + " in production (contains '" + matchedPattern + "')"},
			}
		}
		warnings = append(warnings,
			"Using development "+input.Name+" (contains '"+matchedPattern+"'), not suitable for production")
	}

	charTypes := 0
	for _, re := range []*regexp.Regexp{secretUpperRe, secretLowerRe, secretNumberRe, secretSpecialRe} {
		if re.MatchString(input.Value) {
			charTypes++
		}
	}

	if charTypes < 3 {
		if IsProductionEnv() {
			return SecretCheckResult{Valid: false, Warnings: []string{input.ComplexityProductionMessage}}
		}
		warnings = append(warnings, input.ComplexityDevelopmentMsg)
	}

	return SecretCheckResult{Valid: true, Warnings: warnings}
}

// CheckJwtConfig mirrors checkJwtConfig(): validates JWT_SECRET, COOKIE_SECRET and
// ENCRYPTION_KEY against the current environment and returns accumulated warnings.
func CheckJwtConfig() SecretCheckResult {
	warnings := []string{}

	jwtCheck := validateSecret(secretRequirement{
		Name:                        "JWT_SECRET",
		Value:                       os.Getenv("JWT_SECRET"),
		MissingProductionMessage:    "JWT_SECRET not configured",
		MissingDevelopmentMessage:   "JWT_SECRET not configured",
		WeakDevelopmentMessage:      "JWT_SECRET should be at least 32 characters",
		WeakProductionMessage:       "JWT_SECRET must be at least 32 characters in production",
		ComplexityDevelopmentMsg:    "JWT_SECRET should contain uppercase, lowercase, numbers and special characters for better security",
		ComplexityProductionMessage: "JWT_SECRET in production should contain at least 3 types of characters (uppercase, lowercase, numbers, special)",
	})
	if !jwtCheck.Valid {
		return jwtCheck
	}
	warnings = append(warnings, jwtCheck.Warnings...)

	cookieCheck := validateSecret(secretRequirement{
		Name:                        "COOKIE_SECRET",
		Value:                       os.Getenv("COOKIE_SECRET"),
		MissingProductionMessage:    "COOKIE_SECRET must be configured in production",
		MissingDevelopmentMessage:   "COOKIE_SECRET not configured, will use fallback development value",
		WeakDevelopmentMessage:      "COOKIE_SECRET should be at least 32 characters",
		WeakProductionMessage:       "COOKIE_SECRET must be at least 32 characters in production",
		ComplexityDevelopmentMsg:    "COOKIE_SECRET should contain uppercase, lowercase, numbers and special characters for better security",
		ComplexityProductionMessage: "COOKIE_SECRET in production should contain at least 3 types of characters (uppercase, lowercase, numbers, special)",
	})
	if !cookieCheck.Valid {
		return cookieCheck
	}
	warnings = append(warnings, cookieCheck.Warnings...)

	encryptionCheck := validateSecret(secretRequirement{
		Name:                        "ENCRYPTION_KEY",
		Value:                       os.Getenv("ENCRYPTION_KEY"),
		MissingProductionMessage:    "ENCRYPTION_KEY must be configured in production",
		MissingDevelopmentMessage:   "ENCRYPTION_KEY not configured, will use JWT_SECRET as fallback",
		WeakDevelopmentMessage:      "ENCRYPTION_KEY should be at least 32 characters",
		WeakProductionMessage:       "ENCRYPTION_KEY must be at least 32 characters in production",
		ComplexityDevelopmentMsg:    "ENCRYPTION_KEY should contain uppercase, lowercase, numbers and special characters for better security",
		ComplexityProductionMessage: "ENCRYPTION_KEY in production should contain at least 3 types of characters (uppercase, lowercase, numbers, special)",
	})
	if !encryptionCheck.Valid {
		return encryptionCheck
	}
	warnings = append(warnings, encryptionCheck.Warnings...)

	return SecretCheckResult{Valid: true, Warnings: warnings}
}
