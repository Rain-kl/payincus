// Port of server/src/lib/security.ts.
//
// This is the security foundation for auth/users and friends: password policy,
// input validation, the in-process login-failure lock, sensitive-data
// encryption, TOTP 2FA, audit logging, refresh-token/session storage,
// single-use OAuth state / login codes and the in-memory export-task store.
//
// Single-instance invariant (AGENTS.md): the login-failure lock, the OAuth
// state/login-code nonce sets and the export-task store are process-local state
// and are only correct because the private backend runs exactly one Node
// process. A second replica would isolate that state, so these must be moved to
// PostgreSQL (or another shared store) before any horizontal scaling. All three
// stores are guarded by a sync.Mutex because routes and workers touch them
// concurrently.
//
// Functions already ported elsewhere are intentionally NOT redefined here:
//   - jwt.go:                    GetJwtSigningSecret, SignAccessToken, ...
//   - token_invalidation.go:     IsAccessTokenInvalidated, InvalidateUserAccessTokens, ...
//   - log_sanitizer.go:          SanitizeTokensInString, IsSensitiveField, SanitizeObject
//   - params.go:                 ParsePositiveRouteID, ParsePositiveIntegerQuery
//   - cookie_config.go:          IsProductionEnv
//   - operation_verification.go: maskEmail and the verification-code helpers
//
// checkJwtConfig lives in security_config.go (it is a re-export of
// security-config.ts), so it is not defined here.
package lib

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/skip2/go-qrcode"
	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// securityNowMs is swappable in tests so time-dependent behaviour (login lock,
// OAuth TTLs, export-task TTL) can be exercised deterministically. Production
// code must leave it as time.Now().UnixMilli.
var securityNowMs = func() int64 { return time.Now().UnixMilli() }

// ==================== 安全事件类型 ====================

// SecurityEventType mirrors the SecurityEventType enum.
type SecurityEventType string

const (
	SecurityEventLoginSuccess       SecurityEventType = "login_success"
	SecurityEventLoginFailed        SecurityEventType = "login_failed"
	SecurityEventRegisterSuccess    SecurityEventType = "register_success"
	SecurityEventLogout             SecurityEventType = "logout"
	SecurityEventUnauthorizedAccess SecurityEventType = "unauthorized_access"
	SecurityEventPermissionDenied   SecurityEventType = "permission_denied"
	SecurityEventRateLimitExceeded  SecurityEventType = "rate_limit_exceeded"
	SecurityEventSuspiciousActivity SecurityEventType = "suspicious_activity"
	SecurityEventAdminAction        SecurityEventType = "admin_action"
)

// ==================== 登录失败锁定 ====================

type loginAttempt struct {
	count       int
	lastAttempt int64
	lockedUntil *int64
}

// Login lock configuration, mirroring LOGIN_CONFIG.
const (
	loginMaxAttempts    = 5
	loginLockDurationMs = 15 * 60 * 1000
	loginAttemptWindow  = 5 * 60 * 1000
)

var loginAttempts = struct {
	mu      sync.Mutex
	entries map[string]loginAttempt
}{entries: make(map[string]loginAttempt)}

// LoginLockStatus mirrors `{ locked: boolean; retryAfter?: number }`.
type LoginLockStatus struct {
	Locked     bool  `json:"locked"`
	RetryAfter int64 `json:"retryAfter,omitempty"`
}

func loginAttemptKey(ip, username string) string {
	return ip + ":" + username
}

// CheckLoginLock mirrors checkLoginLock().
func CheckLoginLock(ip, username string) LoginLockStatus {
	key := loginAttemptKey(ip, username)

	loginAttempts.mu.Lock()
	defer loginAttempts.mu.Unlock()

	attempt, ok := loginAttempts.entries[key]
	if !ok {
		return LoginLockStatus{Locked: false}
	}

	now := securityNowMs()
	if attempt.lockedUntil != nil {
		if now < *attempt.lockedUntil {
			diff := *attempt.lockedUntil - now
			return LoginLockStatus{Locked: true, RetryAfter: (diff + 999) / 1000}
		}
		// Lock window elapsed: reset.
		delete(loginAttempts.entries, key)
	}
	return LoginLockStatus{Locked: false}
}

// RecordLoginFailure mirrors recordLoginFailure().
func RecordLoginFailure(ip, username string) {
	key := loginAttemptKey(ip, username)
	now := securityNowMs()

	loginAttempts.mu.Lock()
	defer loginAttempts.mu.Unlock()

	attempt, ok := loginAttempts.entries[key]
	if !ok || now-attempt.lastAttempt > loginAttemptWindow {
		loginAttempts.entries[key] = loginAttempt{count: 1, lastAttempt: now}
		return
	}

	attempt.count++
	attempt.lastAttempt = now
	if attempt.count >= loginMaxAttempts {
		lockedUntil := now + loginLockDurationMs
		attempt.lockedUntil = &lockedUntil
	}
	loginAttempts.entries[key] = attempt
}

// ClearLoginFailure mirrors clearLoginFailure().
func ClearLoginFailure(ip, username string) {
	key := loginAttemptKey(ip, username)

	loginAttempts.mu.Lock()
	defer loginAttempts.mu.Unlock()
	delete(loginAttempts.entries, key)
}

// ==================== 密码策略 ====================

// PasswordValidationResult mirrors PasswordValidationResult.
type PasswordValidationResult struct {
	Valid   bool   `json:"valid"`
	Message string `json:"message,omitempty"`
}

// ValidatePassword mirrors validatePassword(). Requires >= 8 characters with at
// least one uppercase letter, one lowercase letter and one digit.
func ValidatePassword(password string) PasswordValidationResult {
	length := utf8.RuneCountInString(password)
	if length < 8 {
		return PasswordValidationResult{Valid: false, Message: "Password must be at least 8 characters"}
	}
	if length > 128 {
		return PasswordValidationResult{Valid: false, Message: "Password cannot exceed 128 characters"}
	}
	if !passwordUpperPattern.MatchString(password) {
		return PasswordValidationResult{Valid: false, Message: "Password must contain at least one uppercase letter"}
	}
	if !passwordLowerPattern.MatchString(password) {
		return PasswordValidationResult{Valid: false, Message: "Password must contain at least one lowercase letter"}
	}
	if !passwordDigitPattern.MatchString(password) {
		return PasswordValidationResult{Valid: false, Message: "Password must contain at least one number"}
	}
	return PasswordValidationResult{Valid: true}
}

var (
	passwordUpperPattern = regexp.MustCompile(`[A-Z]`)
	passwordLowerPattern = regexp.MustCompile(`[a-z]`)
	passwordDigitPattern = regexp.MustCompile(`[0-9]`)
)

// ==================== 安全日志 ====================

// SecurityEventDetails mirrors the details argument of logSecurityEvent.
type SecurityEventDetails struct {
	IP        string
	UserAgent string
	Resource  string
	Action    string
	Reason    string
	Username  string
}

// LogSecurityEvent mirrors logSecurityEvent(). Failures are swallowed so a log
// write can never break the main business flow (CreateLog already does this).
func LogSecurityEvent(ctx context.Context, eventType SecurityEventType, userID *int64, details SecurityEventDetails) {
	action := string(eventType)
	content := ""

	switch eventType {
	case SecurityEventRegisterSuccess, SecurityEventLoginSuccess, SecurityEventLogout:
		content = securityFormatUserIP(details.Username, details.IP)
	case SecurityEventLoginFailed, SecurityEventRateLimitExceeded:
		parts := make([]string, 0, 3)
		if details.Username != "" {
			parts = append(parts, details.Username)
		}
		if details.Reason != "" {
			parts = append(parts, "[原因: "+details.Reason+"]")
		}
		if details.IP != "" {
			parts = append(parts, "[IP: "+details.IP+"]")
		}
		content = strings.Join(parts, " ")
	case SecurityEventSuspiciousActivity:
		if details.Action == "register" {
			action = "invalid_invite_code"
		} else {
			action = "suspicious_activity"
		}
		parts := make([]string, 0, 4)
		if details.Action != "" {
			parts = append(parts, "[操作: "+details.Action+"]")
		}
		if details.Reason != "" {
			parts = append(parts, "[原因: "+details.Reason+"]")
		}
		if details.Username != "" {
			parts = append(parts, "[用户: "+details.Username+"]")
		}
		if details.IP != "" {
			parts = append(parts, "[IP: "+details.IP+"]")
		}
		content = strings.Join(parts, " ")
		if content == "" {
			content = "可疑活动"
		}
	case SecurityEventPermissionDenied:
		content = details.Resource
	}

	result := db.LogResultSuccess
	if strings.Contains(string(eventType), "failed") ||
		strings.Contains(string(eventType), "denied") ||
		eventType == SecurityEventRateLimitExceeded ||
		eventType == SecurityEventSuspiciousActivity {
		result = db.LogResultWarning
	}

	db.CreateLog(ctx, userID, db.LogModuleSecurity, action, SanitizeTokensInString(content), result, db.CreateLogOptions{})
}

func securityFormatUserIP(username, ip string) string {
	parts := make([]string, 0, 2)
	if username != "" {
		parts = append(parts, username)
	}
	if ip != "" {
		parts = append(parts, "[IP: "+ip+"]")
	}
	return strings.Join(parts, " ")
}

// ==================== 输入清理与验证 ====================

// DANGEROUS_CHARS_REGEX: ' " \ / ? [ ] = + . < > ` ; : | ! @ # $ % ^ & * { } ~
var dangerousCharsRegex = regexp.MustCompile("['\"\\\\/?\\[\\]=+.<>`;:|!@#$%\\^&*{}~]")

// SAFE_NAME_REGEX: letters, digits, Chinese, hyphen, underscore, space, comma, parentheses.
var safeNameRegex = regexp.MustCompile(`^[\x{4e00}-\x{9fa5}a-zA-Z0-9\-_ ,()]+$`)

// STRICT_NAME_REGEX: must start with a letter/digit, then letters/digits/hyphen/underscore.
var strictNameRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9\-_]*$`)

var ipv4Regex = regexp.MustCompile(`^(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$`)

var domainRegex = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$|^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

var pathDangerousCharsRegex = regexp.MustCompile("['\"`<>|;$&]")

var instanceNameSanitizeRegex = regexp.MustCompile(`[^a-zA-Z0-9\-_]`)

var validInstanceNameRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9\-_]*$`)

// InputValidationResult mirrors InputValidationResult. Sanitized is a pointer so
// an absent value (undefined) stays distinct from an empty string, matching the
// TypeScript optional field.
type InputValidationResult struct {
	Valid     bool    `json:"valid"`
	Message   string  `json:"message,omitempty"`
	Sanitized *string `json:"sanitized,omitempty"`
}

func invalidInput(message string) InputValidationResult {
	return InputValidationResult{Valid: false, Message: message}
}

func validInput(sanitized string) InputValidationResult {
	return InputValidationResult{Valid: true, Sanitized: &sanitized}
}

// ContainsDangerousChars mirrors containsDangerousChars().
func ContainsDangerousChars(input string) bool {
	return dangerousCharsRegex.MatchString(input)
}

// RemoveDangerousChars mirrors removeDangerousChars().
func RemoveDangerousChars(input string) string {
	return dangerousCharsRegex.ReplaceAllString(input, "")
}

// ValidateName mirrors validateName(name, fieldName='Name', minLength=1, maxLength=64).
func ValidateName(name, fieldName string, minLength, maxLength int) InputValidationResult {
	if name == "" {
		return invalidInput(fieldName + " cannot be empty")
	}
	trimmed := strings.TrimSpace(name)
	length := utf8.RuneCountInString(trimmed)
	if length < minLength {
		return invalidInput(fmt.Sprintf("%s must be at least %d characters", fieldName, minLength))
	}
	if length > maxLength {
		return invalidInput(fmt.Sprintf("%s cannot exceed %d characters", fieldName, maxLength))
	}
	if ContainsDangerousChars(trimmed) {
		return invalidInput(fieldName + " contains illegal characters")
	}
	if !safeNameRegex.MatchString(trimmed) {
		return invalidInput(fieldName + " can only contain letters, numbers, Chinese, hyphens, underscores, spaces, commas and parentheses")
	}
	return validInput(trimmed)
}

// ValidateIdentifier mirrors validateIdentifier(input, fieldName='Identifier', minLength=2, maxLength=64).
func ValidateIdentifier(input, fieldName string, minLength, maxLength int) InputValidationResult {
	if input == "" {
		return invalidInput(fieldName + " cannot be empty")
	}
	trimmed := strings.TrimSpace(input)
	length := utf8.RuneCountInString(trimmed)
	if length < minLength {
		return invalidInput(fmt.Sprintf("%s must be at least %d characters", fieldName, minLength))
	}
	if length > maxLength {
		return invalidInput(fmt.Sprintf("%s cannot exceed %d characters", fieldName, maxLength))
	}
	if !strictNameRegex.MatchString(trimmed) {
		return invalidInput(fieldName + " can only contain letters, numbers, hyphens and underscores, and must start with a letter or number")
	}
	return validInput(trimmed)
}

// ValidateText mirrors validateText(text, fieldName='Content', maxLength=1000).
func ValidateText(text, fieldName string, maxLength int) InputValidationResult {
	if text == "" {
		return validInput("")
	}
	if utf8.RuneCountInString(text) > maxLength {
		return invalidInput(fmt.Sprintf("%s cannot exceed %d characters", fieldName, maxLength))
	}
	return validInput(strings.TrimSpace(RemoveDangerousChars(text)))
}

// ValidateURL mirrors validateUrl(url, fieldName='URL').
func ValidateURL(rawURL, fieldName string) InputValidationResult {
	if rawURL == "" {
		return invalidInput(fieldName + " cannot be empty")
	}
	trimmed := strings.TrimSpace(rawURL)

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return invalidInput(fieldName + " format is invalid")
	}
	if !strings.HasPrefix(trimmed, "http://") && !strings.HasPrefix(trimmed, "https://") {
		return invalidInput(fieldName + " must start with http:// or https://")
	}
	return validInput(trimmed)
}

// ValidateIPAddress mirrors validateIpAddress(ip, fieldName='IP address').
func ValidateIPAddress(ip, fieldName string) InputValidationResult {
	if ip == "" {
		return invalidInput(fieldName + " cannot be empty")
	}
	trimmed := normalizeHostAddress(ip)
	if !ipv4Regex.MatchString(trimmed) && !isValidIPv6Address(trimmed) {
		return invalidInput(fieldName + " format is invalid")
	}
	return validInput(trimmed)
}

// ValidateIPv4 mirrors validateIpv4(input, fieldName='IP address').
func ValidateIPv4(input, fieldName string) InputValidationResult {
	if input == "" {
		return invalidInput(fieldName + " cannot be empty")
	}
	trimmed := strings.TrimSpace(input)
	if ipv4Regex.MatchString(trimmed) {
		return validInput(trimmed)
	}
	return invalidInput(fieldName + " format is invalid, please enter a valid IPv4 address")
}

// ValidateIPOrDomain mirrors validateIpOrDomain(input, fieldName='IP address or domain').
func ValidateIPOrDomain(input, fieldName string) InputValidationResult {
	if input == "" {
		return invalidInput(fieldName + " cannot be empty")
	}
	trimmed := normalizeHostAddress(input)
	if utf8.RuneCountInString(trimmed) > 253 {
		return invalidInput(fieldName + " cannot exceed 253 characters")
	}
	if ipv4Regex.MatchString(trimmed) {
		return validInput(trimmed)
	}
	if isValidIPv6Address(trimmed) {
		return validInput(trimmed)
	}
	if domainRegex.MatchString(trimmed) {
		for _, label := range strings.Split(trimmed, ".") {
			if len(label) > 63 {
				return invalidInput(fieldName + " format is invalid: domain label cannot exceed 63 characters")
			}
		}
		return validInput(trimmed)
	}
	return invalidInput(fieldName + " format is invalid, please enter a valid IPv4 address, IPv6 address, or domain name")
}

// normalizeHostAddress mirrors normalizeHostAddress(): strip brackets around a
// bracketed IPv6 literal that contains a colon.
func normalizeHostAddress(input string) string {
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		if strings.Contains(inner, ":") {
			return inner
		}
	}
	return trimmed
}

// isValidIPv6Address mirrors isValidIpv6Address(), which relies on the URL
// parser accepting `http://[<input>]/`.
func isValidIPv6Address(input string) bool {
	if input == "" || !strings.Contains(input, ":") {
		return false
	}
	addr, err := netip.ParseAddr(input)
	if err != nil {
		return false
	}
	return addr.Is6()
}

// ValidateFilePath mirrors validateFilePath(path, fieldName='文件路径').
func ValidateFilePath(path, fieldName string) InputValidationResult {
	if path == "" {
		return invalidInput(fieldName + "不能为空")
	}
	trimmed := strings.TrimSpace(path)
	if strings.Contains(trimmed, "..") || strings.Contains(trimmed, "\x00") {
		return invalidInput(fieldName + "包含非法字符")
	}
	if pathDangerousCharsRegex.MatchString(trimmed) {
		return invalidInput(fieldName + "包含非法字符")
	}
	return validInput(trimmed)
}

// SanitizeInstanceName mirrors sanitizeInstanceName().
func SanitizeInstanceName(name string) string {
	return instanceNameSanitizeRegex.ReplaceAllString(name, "")
}

// IsValidInstanceName mirrors isValidInstanceName().
func IsValidInstanceName(name string) bool {
	return validInstanceNameRegex.MatchString(name)
}

// InputValidation mirrors one entry of validateInputs.
type InputValidation struct {
	Value     any
	Validator func() InputValidationResult
}

// ValidateInputs mirrors validateInputs().
func ValidateInputs(validations []InputValidation) InputValidationResult {
	for _, validation := range validations {
		result := validation.Validator()
		if !result.Valid {
			return result
		}
	}
	return InputValidationResult{Valid: true}
}

// ==================== Refresh Token 管理 (数据库) ====================

// Refresh token configuration, mirroring REFRESH_TOKEN_CONFIG.
const refreshTokenExpiresInSeconds = 30 * 24 * 60 * 60

// RefreshTokenStoragePrefix prefixes the sha256 digest under which refresh
// tokens are stored (REFRESH_TOKEN_STORAGE_PREFIX).
const RefreshTokenStoragePrefix = "sha256:"

// GetRefreshTokenStorageKey mirrors getRefreshTokenStorageKey(): returns the
// token itself when it is already a `sha256:<64 hex>` key, otherwise the hashed
// storage key.
func GetRefreshTokenStorageKey(token string) string {
	if strings.HasPrefix(token, RefreshTokenStoragePrefix) && len(token) == len(RefreshTokenStoragePrefix)+64 {
		return token
	}
	sum := sha256.Sum256([]byte(token))
	return RefreshTokenStoragePrefix + hex.EncodeToString(sum[:])
}

// GetRefreshTokenSessionID mirrors getRefreshTokenSessionId(): the first 32 hex
// chars of the digest, used as the public session identifier.
func GetRefreshTokenSessionID(token string) string {
	storageKey := GetRefreshTokenStorageKey(token)
	return storageKey[len(RefreshTokenStoragePrefix) : len(RefreshTokenStoragePrefix)+32]
}

// getRefreshTokenLookupKeys mirrors getRefreshTokenLookupKeys(): hashed key
// first, plus the raw token for legacy plaintext rows.
func getRefreshTokenLookupKeys(token string) []string {
	storageKey := GetRefreshTokenStorageKey(token)
	if storageKey == token {
		return []string{storageKey}
	}
	return []string{storageKey, token}
}

// RefreshTokenData mirrors RefreshTokenData; timestamps are milliseconds since
// the epoch (matching Date.prototype.getTime()).
type RefreshTokenData struct {
	UserID       int32
	Username     string
	Role         models.UserRole
	CreatedAt    int64
	ExpiresAt    int64
	IP           string
	UserAgent    string
	LastActiveAt int64
}

// SessionInfo mirrors SessionInfo; it is serialised to the frontend verbatim.
type SessionInfo struct {
	Token        string `json:"token"`
	IP           string `json:"ip"`
	UserAgent    string `json:"userAgent"`
	CreatedAt    int64  `json:"createdAt"`
	LastActiveAt int64  `json:"lastActiveAt"`
	IsCurrent    bool   `json:"isCurrent"`
}

// CleanupExpiredRefreshTokens mirrors cleanupExpiredRefreshTokens().
func CleanupExpiredRefreshTokens(ctx context.Context) (int64, error) {
	if db.DB == nil {
		return 0, nil
	}
	result := db.DB.WithContext(ctx).
		Where("expires_at < ?", time.Now()).
		Delete(&models.RefreshToken{})
	return result.RowsAffected, result.Error
}

// CleanupExpiredTokenInvalidations mirrors cleanupExpiredTokenInvalidations():
// drop invalidation markers older than 30 days.
func CleanupExpiredTokenInvalidations(ctx context.Context) (int64, error) {
	if db.DB == nil {
		return 0, nil
	}
	thirtyDaysAgo := time.Now().Unix() - 30*24*60*60
	result := db.DB.WithContext(ctx).
		Where("invalidated_at < ?", thirtyDaysAgo).
		Delete(&models.TokenInvalidation{})
	return result.RowsAffected, result.Error
}

// GenerateRefreshToken mirrors generateRefreshToken(): `rt_` + 32 random bytes
// as base64url, persisted under its sha256 storage key.
func GenerateRefreshToken(ctx context.Context, userID int32, username string, role models.UserRole, ip, userAgent string) (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	token := "rt_" + base64.RawURLEncoding.EncodeToString(buf)
	tokenHash := GetRefreshTokenStorageKey(token)

	now := time.Now()
	expiresAt := now.Add(time.Duration(refreshTokenExpiresInSeconds) * time.Second)
	if ip == "" {
		ip = "unknown"
	}
	if userAgent == "" {
		userAgent = "unknown"
	}

	record := models.RefreshToken{
		Token:        tokenHash,
		UserID:       userID,
		Username:     username,
		Role:         role,
		IP:           &ip,
		UserAgent:    &userAgent,
		CreatedAt:    now,
		ExpiresAt:    expiresAt,
		LastActiveAt: now,
	}
	if err := db.DB.WithContext(ctx).Create(&record).Error; err != nil {
		return "", err
	}
	return token, nil
}

// VerifyRefreshToken mirrors verifyRefreshToken(). It looks up the hashed key
// first, then falls back to the legacy plaintext row and upgrades it in place.
// Any failure yields nil (matching the Node try/catch).
func VerifyRefreshToken(ctx context.Context, token, currentIP, currentUserAgent string) *RefreshTokenData {
	if db.DB == nil {
		return nil
	}
	storageKey := GetRefreshTokenStorageKey(token)

	var record models.RefreshToken
	err := db.DB.WithContext(ctx).Where("token = ?", storageKey).First(&record).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if storageKey == token {
			return nil
		}
		if err := db.DB.WithContext(ctx).Where("token = ?", token).First(&record).Error; err != nil {
			return nil
		}
	}

	now := securityNowMs()
	expiresAt := record.ExpiresAt.UnixMilli()
	if now > expiresAt {
		_ = db.DB.WithContext(ctx).Where("id = ?", record.ID).Delete(&models.RefreshToken{}).Error
		return nil
	}

	if record.Token == token && storageKey != token {
		_ = db.DB.WithContext(ctx).
			Model(&models.RefreshToken{}).
			Where("id = ?", record.ID).
			Update("token", storageKey).Error
	}

	data := &RefreshTokenData{
		UserID:       record.UserID,
		Username:     record.Username,
		Role:         record.Role,
		CreatedAt:    record.CreatedAt.UnixMilli(),
		ExpiresAt:    expiresAt,
		LastActiveAt: record.LastActiveAt.UnixMilli(),
	}
	if record.IP != nil {
		data.IP = *record.IP
	}
	if record.UserAgent != nil {
		data.UserAgent = *record.UserAgent
	}
	return data
}

// RevokeRefreshToken mirrors revokeRefreshToken().
func RevokeRefreshToken(ctx context.Context, token string) bool {
	if db.DB == nil {
		return false
	}
	result := db.DB.WithContext(ctx).
		Where("token IN ?", getRefreshTokenLookupKeys(token)).
		Delete(&models.RefreshToken{})
	if result.Error != nil {
		return false
	}
	return result.RowsAffected > 0
}

// RevokeAllUserRefreshTokens mirrors revokeAllUserRefreshTokens().
func RevokeAllUserRefreshTokens(ctx context.Context, userID int32) (int64, error) {
	if db.DB == nil {
		return 0, nil
	}
	result := db.DB.WithContext(ctx).
		Where("user_id = ?", userID).
		Delete(&models.RefreshToken{})
	return result.RowsAffected, result.Error
}

// GetUserSessions mirrors getUserSessions(): every non-expired session for the
// user, most recently active first.
func GetUserSessions(ctx context.Context, userID int32, currentToken string) ([]SessionInfo, error) {
	if db.DB == nil {
		return []SessionInfo{}, nil
	}

	var tokens []models.RefreshToken
	if err := db.DB.WithContext(ctx).
		Where("user_id = ? AND expires_at > ?", userID, time.Now()).
		Order("last_active_at DESC").
		Find(&tokens).Error; err != nil {
		return nil, err
	}

	currentKeys := make(map[string]bool)
	if currentToken != "" {
		for _, key := range getRefreshTokenLookupKeys(currentToken) {
			currentKeys[key] = true
		}
	}

	sessions := make([]SessionInfo, 0, len(tokens))
	for _, token := range tokens {
		ip := "unknown"
		if token.IP != nil {
			ip = *token.IP
		}
		userAgent := "unknown"
		if token.UserAgent != nil {
			userAgent = *token.UserAgent
		}
		sessions = append(sessions, SessionInfo{
			Token:        GetRefreshTokenSessionID(token.Token) + "...",
			IP:           ip,
			UserAgent:    userAgent,
			CreatedAt:    token.CreatedAt.UnixMilli(),
			LastActiveAt: token.LastActiveAt.UnixMilli(),
			IsCurrent:    currentKeys[token.Token],
		})
	}
	return sessions, nil
}

// RevokeSessionByTokenPrefix mirrors revokeSessionByTokenPrefix().
func RevokeSessionByTokenPrefix(ctx context.Context, userID int32, tokenPrefix string) (bool, error) {
	if db.DB == nil {
		return false, nil
	}
	// JS String.replace('...', '') removes only the first occurrence.
	prefix := strings.Replace(tokenPrefix, "...", "", 1)

	var tokens []models.RefreshToken
	if err := db.DB.WithContext(ctx).Where("user_id = ?", userID).Find(&tokens).Error; err != nil {
		return false, err
	}

	for _, token := range tokens {
		if strings.HasPrefix(token.Token, RefreshTokenStoragePrefix+prefix) ||
			strings.HasPrefix(token.Token, prefix) ||
			strings.HasPrefix(GetRefreshTokenSessionID(token.Token), prefix) {
			if err := db.DB.WithContext(ctx).Where("id = ?", token.ID).Delete(&models.RefreshToken{}).Error; err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// UpdateSessionActivity mirrors updateSessionActivity(): bump lastActiveAt and
// extend the session to a fresh 30-day window. Errors yield a nil time (the
// Node `null`) so callers can ignore them.
func UpdateSessionActivity(ctx context.Context, token string) (*time.Time, error) {
	if db.DB == nil {
		return nil, nil
	}
	now := time.Now()
	newExpiresAt := now.Add(time.Duration(refreshTokenExpiresInSeconds) * time.Second)

	result := db.DB.WithContext(ctx).
		Model(&models.RefreshToken{}).
		Where("token IN ?", getRefreshTokenLookupKeys(token)).
		Updates(map[string]any{"last_active_at": now, "expires_at": newExpiresAt})
	if result.Error != nil {
		return nil, result.Error
	}
	return &newExpiresAt, nil
}

// ==================== 敏感数据加密 ====================

// Encryption configuration, mirroring ENCRYPTION_CONFIG.
const (
	encryptionIVLength  = 16
	encryptionTagLength = 16
)

// getEncryptionKey mirrors getEncryptionKey(): ENCRYPTION_KEY, or (outside
// production) JWT_SECRET / 'default-encryption-key', hashed to 32 bytes.
func getEncryptionKey() ([]byte, error) {
	secret := os.Getenv("ENCRYPTION_KEY")
	if secret == "" {
		if IsProductionEnv() {
			secret = ""
		} else {
			secret = os.Getenv("JWT_SECRET")
			if secret == "" {
				secret = "default-encryption-key"
			}
		}
	}
	if secret == "" {
		return nil, errors.New("ENCRYPTION_KEY is required in production")
	}
	sum := sha256.Sum256([]byte(secret))
	return sum[:], nil
}

// EncryptSensitiveData mirrors encryptSensitiveData(): AES-256-GCM with a
// 16-byte IV, returning `ivHex:tagHex:ciphertextHex`. This layout is
// byte-compatible with Node's crypto.createCipheriv('aes-256-gcm', ...) so the
// two backends can read each other's ciphertext.
func EncryptSensitiveData(plaintext string) (string, error) {
	key, err := getEncryptionKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, encryptionIVLength)
	if err != nil {
		return "", err
	}

	iv := make([]byte, encryptionIVLength)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}

	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	tagStart := len(sealed) - gcm.Overhead()
	tag := sealed[tagStart:]
	ciphertext := sealed[:tagStart]

	return hex.EncodeToString(iv) + ":" + hex.EncodeToString(tag) + ":" + hex.EncodeToString(ciphertext), nil
}

// DecryptSensitiveData mirrors decryptSensitiveData(): any failure returns the
// input unchanged (it may be unencrypted legacy data).
func DecryptSensitiveData(ciphertext string) string {
	parts := strings.Split(ciphertext, ":")
	if len(parts) != 3 {
		return ciphertext
	}

	key, err := getEncryptionKey()
	if err != nil {
		return ciphertext
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return ciphertext
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, encryptionIVLength)
	if err != nil {
		return ciphertext
	}

	iv, err := hex.DecodeString(parts[0])
	if err != nil {
		return ciphertext
	}
	tag, err := hex.DecodeString(parts[1])
	if err != nil {
		return ciphertext
	}
	ciphertextBytes, err := hex.DecodeString(parts[2])
	if err != nil {
		return ciphertext
	}

	sealed := make([]byte, 0, len(ciphertextBytes)+len(tag))
	sealed = append(sealed, ciphertextBytes...)
	sealed = append(sealed, tag...)

	plaintext, err := gcm.Open(nil, iv, sealed, nil)
	if err != nil {
		return ciphertext
	}
	return string(plaintext)
}

// IsEncrypted mirrors isEncrypted(): exactly three colon-separated parts whose
// first two have the IV/tag hex length.
func IsEncrypted(data string) bool {
	parts := strings.Split(data, ":")
	if len(parts) != 3 {
		return false
	}
	return len(parts[0]) == encryptionIVLength*2 && len(parts[1]) == encryptionTagLength*2
}

// ==================== 双因素认证 (2FA) ====================

// Two-factor configuration, mirroring TWO_FACTOR_CONFIG.
const (
	twoFactorIssuer      = "Incudal"
	twoFactorPeriod      = 30
	twoFactorDigits      = 6
	twoFactorSecretBytes = 10
)

var twoFactorBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Generate2FASecret mirrors generate2FASecret(): otplib's default
// authenticator.generateSecret() is base32(10 random bytes), unpadded.
func Generate2FASecret() (string, error) {
	buf := make([]byte, twoFactorSecretBytes)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	return twoFactorBase32.EncodeToString(buf), nil
}

// Generate2FAKeyURI mirrors generate2FAKeyUri(). The layout is byte-identical to
// otplib's authenticator.keyuri(accountName, issuer, secret):
//
//	otpauth://totp/<issuer>:<account>?secret=<secret>&period=30&digits=6&algorithm=SHA1&issuer=<issuer>
func Generate2FAKeyURI(username, secret string) string {
	return "otpauth://totp/" + encodeURIComponent(twoFactorIssuer) + ":" + encodeURIComponent(username) +
		"?secret=" + secret +
		fmt.Sprintf("&period=%d&digits=%d&algorithm=SHA1&issuer=%s", twoFactorPeriod, twoFactorDigits, encodeURIComponent(twoFactorIssuer))
}

// Generate2FAQRCode mirrors generate2FAQRCode(): a PNG data URL of the otpauth
// URI. The encoded payload matches the Node `qrcode` output; the PNG bytes are
// not byte-identical (different encoder/compression) but the dimensions match
// the Node defaults (ECC level M, margin 4, scale 4).
func Generate2FAQRCode(username, secret string) (string, error) {
	uri := Generate2FAKeyURI(username, secret)
	code, err := qrcode.New(uri, qrcode.Medium)
	if err != nil {
		return "", err
	}
	size := len(code.Bitmap()) * 4
	png, err := code.PNG(size)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

// Verify2FAToken mirrors verify2FAToken(). otplib defaults to window 0 (only the
// current 30s step), reproduced here with Skew 0.
func Verify2FAToken(token, secret string) bool {
	valid, err := totp.ValidateCustom(token, secret, time.Now().UTC(), totp.ValidateOpts{
		Period:    twoFactorPeriod,
		Skew:      0,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		return false
	}
	return valid
}

// recoveryCodeAlphabet mirrors the alphabet in generateRecoveryCodes.
const recoveryCodeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// DefaultRecoveryCodeCount is the default count of generateRecoveryCodes.
const DefaultRecoveryCodeCount = 8

// GenerateRecoveryCodes mirrors generateRecoveryCodes(count=8). With no argument
// it produces 8 codes, each `XXXX-XXXX` from the A-Z0-9 alphabet.
func GenerateRecoveryCodes(counts ...int) ([]string, error) {
	count := DefaultRecoveryCodeCount
	if len(counts) > 0 {
		count = counts[0]
	}

	codes := make([]string, 0, count)
	for i := 0; i < count; i++ {
		first, err := randomRecoveryCodeSegment()
		if err != nil {
			return nil, err
		}
		second, err := randomRecoveryCodeSegment()
		if err != nil {
			return nil, err
		}
		codes = append(codes, first+"-"+second)
	}
	return codes, nil
}

func randomRecoveryCodeSegment() (string, error) {
	var builder strings.Builder
	builder.Grow(4)
	for i := 0; i < 4; i++ {
		index, err := rand.Int(rand.Reader, big.NewInt(int64(len(recoveryCodeAlphabet))))
		if err != nil {
			return "", err
		}
		builder.WriteByte(recoveryCodeAlphabet[index.Int64()])
	}
	return builder.String(), nil
}

// ==================== 导出任务内存存储 ====================

// Export task store, mirroring exportTaskStore (process-local, single instance).
const exportTaskTTLMs = 30 * 60 * 1000

// ExportTaskStatus mirrors ExportTaskData['status'].
type ExportTaskStatus string

const (
	ExportTaskStatusCreating    ExportTaskStatus = "creating"
	ExportTaskStatusReady       ExportTaskStatus = "ready"
	ExportTaskStatusDownloading ExportTaskStatus = "downloading"
	ExportTaskStatusCompleted   ExportTaskStatus = "completed"
	ExportTaskStatusError       ExportTaskStatus = "error"
)

// ExportTaskData mirrors ExportTaskData.
type ExportTaskData struct {
	ID              string           `json:"id"`
	InstanceID      int32            `json:"instanceId"`
	BackupID        int32            `json:"backupId"`
	IncusBackupName string           `json:"incusBackupName"`
	UserID          int32            `json:"userId"`
	Status          ExportTaskStatus `json:"status"`
	Error           string           `json:"error,omitempty"`
	CreatedAt       int64            `json:"createdAt"`
	ExpiresAt       int64            `json:"expiresAt"`
}

// CreateExportTaskInput mirrors Omit<ExportTaskData, 'createdAt' | 'expiresAt'>.
type CreateExportTaskInput struct {
	ID              string
	InstanceID      int32
	BackupID        int32
	IncusBackupName string
	UserID          int32
	Status          ExportTaskStatus
}

var exportTaskStore = struct {
	mu    sync.Mutex
	tasks map[string]ExportTaskData
}{tasks: make(map[string]ExportTaskData)}

// CreateExportTask mirrors createExportTask().
func CreateExportTask(input CreateExportTaskInput) ExportTaskData {
	now := securityNowMs()
	task := ExportTaskData{
		ID:              input.ID,
		InstanceID:      input.InstanceID,
		BackupID:        input.BackupID,
		IncusBackupName: input.IncusBackupName,
		UserID:          input.UserID,
		Status:          input.Status,
		CreatedAt:       now,
		ExpiresAt:       now + exportTaskTTLMs,
	}

	exportTaskStore.mu.Lock()
	defer exportTaskStore.mu.Unlock()
	exportTaskStore.tasks[input.ID] = task
	return task
}

// GetExportTask mirrors getExportTask(); expired tasks are dropped on read.
func GetExportTask(taskID string) *ExportTaskData {
	exportTaskStore.mu.Lock()
	defer exportTaskStore.mu.Unlock()

	task, ok := exportTaskStore.tasks[taskID]
	if !ok {
		return nil
	}
	if securityNowMs() > task.ExpiresAt {
		delete(exportTaskStore.tasks, taskID)
		return nil
	}
	return &task
}

// UpdateExportTaskStatus mirrors updateExportTaskStatus().
func UpdateExportTaskStatus(taskID string, status ExportTaskStatus, errMessage string) bool {
	exportTaskStore.mu.Lock()
	defer exportTaskStore.mu.Unlock()

	task, ok := exportTaskStore.tasks[taskID]
	if !ok {
		return false
	}
	if securityNowMs() > task.ExpiresAt {
		delete(exportTaskStore.tasks, taskID)
		return false
	}

	task.Status = status
	if errMessage != "" {
		task.Error = errMessage
	}
	exportTaskStore.tasks[taskID] = task
	return true
}

// DeleteExportTask mirrors deleteExportTask().
func DeleteExportTask(taskID string) bool {
	exportTaskStore.mu.Lock()
	defer exportTaskStore.mu.Unlock()

	_, ok := exportTaskStore.tasks[taskID]
	delete(exportTaskStore.tasks, taskID)
	return ok
}

// CleanupExpiredExportTasks drops expired tasks and returns how many were
// removed. The Node module does this on a 5-minute interval; the port exposes it
// as an explicit call so it can be wired to the scheduler without leaking a
// package-level goroutine.
func CleanupExpiredExportTasks() int {
	now := securityNowMs()

	exportTaskStore.mu.Lock()
	defer exportTaskStore.mu.Unlock()

	removed := 0
	for id, task := range exportTaskStore.tasks {
		if task.ExpiresAt < now {
			delete(exportTaskStore.tasks, id)
			removed++
		}
	}
	return removed
}

// ==================== OAuth State 验证（JWT 签名，无需存储） ====================

// OAuth state TTL, mirroring OAUTH_STATE_TTL_MS.
const oauthStateTTLMs = 10 * 60 * 1000

// OAuthStateMode mirrors the 'login' | 'bind' union.
type OAuthStateMode string

const (
	OAuthStateModeLogin OAuthStateMode = "login"
	OAuthStateModeBind  OAuthStateMode = "bind"
)

// OAuthBindSession mirrors the bindSession argument of generateOAuthState.
type OAuthBindSession struct {
	UserID    int32
	IssuedAt  int64
	SessionID *string
}

// OAuthStateData mirrors OAuthStateData. Field order matches the
// JSON.stringify insertion order so the signed payload is byte-identical.
type OAuthStateData struct {
	Mode          OAuthStateMode `json:"mode"`
	Redirect      string         `json:"redirect"`
	Timestamp     int64          `json:"timestamp"`
	Nonce         string         `json:"nonce"`
	UserID        *int32         `json:"userId,omitempty"`
	UserIssuedAt  *int64         `json:"userIssuedAt,omitempty"`
	UserSessionID *string        `json:"userSessionId,omitempty"`
}

// getOAuthStateSecret mirrors getOAuthStateSecret().
func getOAuthStateSecret() (string, error) {
	return GetJwtSigningSecret("OAuth state and login-code signing")
}

// signOAuthPayload produces the base64url HMAC-SHA256 signature over the payload.
func signOAuthPayload(payload string) (string, error) {
	secret, err := getOAuthStateSecret()
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// timingSafeStringEqual mirrors timingSafeStringEqual().
func timingSafeStringEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// encodeURIComponent mirrors JavaScript's encodeURIComponent.
func encodeURIComponent(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '!' || c == '~' || c == '*' || c == '\'' || c == '(' || c == ')' {
			builder.WriteByte(c)
			continue
		}
		const hexDigits = "0123456789ABCDEF"
		builder.WriteByte('%')
		builder.WriteByte(hexDigits[c>>4])
		builder.WriteByte(hexDigits[c&0x0f])
	}
	return builder.String()
}

// nonceStore is a process-local set of consumed nonces with per-entry expiry
// (single-instance invariant).
type nonceStore struct {
	mu      sync.Mutex
	entries map[string]int64
}

func newNonceStore() *nonceStore {
	return &nonceStore{entries: make(map[string]int64)}
}

func (s *nonceStore) has(nonce string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	expiresAt, ok := s.entries[nonce]
	if !ok {
		return false
	}
	if expiresAt <= securityNowMs() {
		delete(s.entries, nonce)
		return false
	}
	return true
}

func (s *nonceStore) mark(nonce string, expiresAt int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[nonce] = expiresAt
}

func (s *nonceStore) cleanup() int {
	now := securityNowMs()

	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for nonce, expiresAt := range s.entries {
		if expiresAt <= now {
			delete(s.entries, nonce)
			removed++
		}
	}
	return removed
}

var usedOAuthStateNonces = newNonceStore()
var usedOAuthLoginCodeNonces = newNonceStore()

// CleanupExpiredOAuthStateNonces drops expired consumed-state nonces.
func CleanupExpiredOAuthStateNonces() int {
	return usedOAuthStateNonces.cleanup()
}

// CleanupExpiredOAuthLoginCodeNonces drops expired consumed-login-code nonces.
func CleanupExpiredOAuthLoginCodeNonces() int {
	return usedOAuthLoginCodeNonces.cleanup()
}

// GenerateOAuthState mirrors generateOAuthState(): base64url(JSON) + "." +
// base64url(HMAC-SHA256).
func GenerateOAuthState(mode OAuthStateMode, redirect string, bindSession *OAuthBindSession) (string, error) {
	nonceBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, nonceBytes); err != nil {
		return "", err
	}

	data := OAuthStateData{
		Mode:      mode,
		Redirect:  redirect,
		Timestamp: securityNowMs(),
		Nonce:     hex.EncodeToString(nonceBytes),
	}
	if bindSession != nil {
		userID := bindSession.UserID
		issuedAt := bindSession.IssuedAt
		data.UserID = &userID
		data.UserIssuedAt = &issuedAt
		data.UserSessionID = bindSession.SessionID
	}

	encoded, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(encoded)
	signature, err := signOAuthPayload(payload)
	if err != nil {
		return "", err
	}
	return payload + "." + signature, nil
}

// VerifyAndConsumeOAuthState mirrors verifyAndConsumeOAuthState(): verifies the
// signature and timestamp, rejects replayed nonces and returns nil on any
// failure.
func VerifyAndConsumeOAuthState(stateToken string) *OAuthStateData {
	parts := strings.Split(stateToken, ".")
	if len(parts) != 2 {
		return nil
	}
	payload, signature := parts[0], parts[1]

	expectedSignature, err := signOAuthPayload(payload)
	if err != nil {
		return nil
	}
	if !timingSafeStringEqual(signature, expectedSignature) {
		return nil
	}

	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	var data OAuthStateData
	if err := json.Unmarshal(decoded, &data); err != nil {
		return nil
	}

	if securityNowMs()-data.Timestamp > oauthStateTTLMs {
		return nil
	}
	if usedOAuthStateNonces.has(data.Nonce) {
		return nil
	}
	usedOAuthStateNonces.mark(data.Nonce, data.Timestamp+oauthStateTTLMs)
	return &data
}

// ==================== 增强审计日志 ====================

// AuditLogDetails mirrors AuditLogDetails.
type AuditLogDetails struct {
	IP             string         `json:"ip,omitempty"`
	UserAgent      string         `json:"userAgent,omitempty"`
	TargetUserID   *int32         `json:"targetUserId,omitempty"`
	TargetUsername string         `json:"targetUsername,omitempty"`
	ResourceType   string         `json:"resourceType,omitempty"`
	ResourceID     any            `json:"resourceId,omitempty"`
	ResourceName   string         `json:"resourceName,omitempty"`
	OldValue       any            `json:"oldValue,omitempty"`
	NewValue       any            `json:"newValue,omitempty"`
	Reason         string         `json:"reason,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

// LogAuditEvent mirrors logAuditEvent().
func LogAuditEvent(ctx context.Context, userID *int64, module, action string, details AuditLogDetails) {
	contentParts := make([]string, 0, 3)
	if details.TargetUsername != "" {
		contentParts = append(contentParts, "target: "+details.TargetUsername)
	}
	if details.ResourceType != "" && details.ResourceName != "" {
		contentParts = append(contentParts, details.ResourceType+": "+details.ResourceName)
	}
	if details.Reason != "" {
		contentParts = append(contentParts, "reason: "+details.Reason)
	}
	content := strings.Join(contentParts, ", ")

	db.CreateLog(ctx, userID, module, action, SanitizeTokensInString(content), db.LogResultSuccess, db.CreateLogOptions{})

	if os.Getenv("AUDIT_LOG_VERBOSE") == "true" {
		entry := map[string]any{
			"timestamp": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
			"userId":    userID,
			"module":    module,
			"action":    action,
		}
		if details.IP != "" {
			entry["ip"] = details.IP
		}
		if details.UserAgent != "" {
			entry["userAgent"] = details.UserAgent
		}
		if details.TargetUserID != nil {
			entry["targetUserId"] = *details.TargetUserID
		}
		if details.TargetUsername != "" {
			entry["targetUsername"] = details.TargetUsername
		}
		if details.ResourceType != "" {
			entry["resourceType"] = details.ResourceType
		}
		if details.ResourceID != nil {
			entry["resourceId"] = details.ResourceID
		}
		if details.ResourceName != "" {
			entry["resourceName"] = details.ResourceName
		}
		if details.OldValue != nil {
			entry["oldValue"] = details.OldValue
		}
		if details.NewValue != nil {
			entry["newValue"] = details.NewValue
		}
		if details.Reason != "" {
			entry["reason"] = details.Reason
		}
		if details.Metadata != nil {
			entry["metadata"] = details.Metadata
		}
		serialized, err := json.Marshal(entry)
		if err == nil {
			fmt.Fprintln(os.Stdout, "[AUDIT]", string(serialized))
		}
	}
}

// LogAdminAction mirrors logAdminAction().
func LogAdminAction(ctx context.Context, adminID int64, action string, details AuditLogDetails) {
	LogAuditEvent(ctx, &adminID, "admin", action, withAuditMetadata(details, "isAdminAction"))
}

// LogSensitiveOperation mirrors logSensitiveOperation().
func LogSensitiveOperation(ctx context.Context, userID int64, operation string, details AuditLogDetails) {
	LogAuditEvent(ctx, &userID, "security", operation, withAuditMetadata(details, "isSensitive"))
}

// withAuditMetadata returns a copy of details whose metadata carries the given
// flag set to true (mirrors the `{ ...details.metadata, [flag]: true }` spread).
func withAuditMetadata(details AuditLogDetails, flag string) AuditLogDetails {
	metadata := make(map[string]any, len(details.Metadata)+1)
	for key, value := range details.Metadata {
		metadata[key] = value
	}
	metadata[flag] = true
	details.Metadata = metadata
	return details
}

// ==================== 异地登录检测 ====================

// NewLoginLocation mirrors `{ isNewIp, isNewDevice }`.
type NewLoginLocation struct {
	IsNewIP     bool `json:"isNewIp"`
	IsNewDevice bool `json:"isNewDevice"`
}

var loginUserAgentBrowserPattern = regexp.MustCompile(`(?i)(Chrome|Firefox|Safari|Edge|Opera|MSIE|Trident)`)

var (
	loginUAWindows10Pattern = regexp.MustCompile(`Windows NT 10`)
	loginUAWindowsPattern   = regexp.MustCompile(`Windows`)
	loginUAMacPattern       = regexp.MustCompile(`Mac OS X|Macintosh`)
	loginUAIOSPattern       = regexp.MustCompile(`iPhone|iPad|iPod`)
	loginUAAndroidPattern   = regexp.MustCompile(`Android`)
	loginUALinuxPattern     = regexp.MustCompile(`Linux`)
	loginUAChromeOSPattern  = regexp.MustCompile(`CrOS`)
	loginUAMobilePattern    = regexp.MustCompile(`Mobile|Android|iPhone|iPad`)
)

// normalizeLoginUserAgent mirrors the inner normalizeUserAgent of
// detectNewLoginLocation. Note the browser match is leftmost (so an Edge UA,
// which also contains "Chrome", is reported as chrome) exactly like the JS regex.
func normalizeLoginUserAgent(userAgent string) string {
	browser := "unknown"
	if match := loginUserAgentBrowserPattern.FindStringSubmatch(userAgent); match != nil {
		browser = strings.ToLower(match[1])
	}

	os := "unknown"
	switch {
	case loginUAWindows10Pattern.MatchString(userAgent):
		os = "windows"
	case loginUAWindowsPattern.MatchString(userAgent):
		os = "windows"
	case loginUAMacPattern.MatchString(userAgent):
		os = "macos"
	case loginUAIOSPattern.MatchString(userAgent):
		os = "ios"
	case loginUAAndroidPattern.MatchString(userAgent):
		os = "android"
	case loginUALinuxPattern.MatchString(userAgent):
		os = "linux"
	case loginUAChromeOSPattern.MatchString(userAgent):
		os = "chromeos"
	}

	device := "desktop"
	if loginUAMobilePattern.MatchString(userAgent) {
		device = "mobile"
	}
	return browser + "|" + os + "|" + device
}

// DetectNewLoginLocation mirrors detectNewLoginLocation(): compares the current
// IP/User-Agent against the user's last 30 days of login records. Detection
// failures are logged and reported as "not new" so they never block login.
func DetectNewLoginLocation(ctx context.Context, userID int32, currentIP, currentUserAgent string) NewLoginLocation {
	if db.DB == nil {
		return NewLoginLocation{}
	}

	thirtyDaysAgo := time.Now().Add(-30 * 24 * time.Hour)
	var records []models.LoginRecord
	err := db.DB.WithContext(ctx).
		Select("ip", "user_agent").
		Where("user_id = ? AND created_at >= ?", userID, thirtyDaysAgo).
		Order("created_at DESC").
		Limit(50).
		Find(&records).Error
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to detect new login location: %v\n", err)
		return NewLoginLocation{}
	}

	if len(records) == 0 {
		return NewLoginLocation{IsNewIP: false, IsNewDevice: false}
	}

	knownIPs := make(map[string]bool)
	for _, record := range records {
		if record.IP != "" {
			knownIPs[record.IP] = true
		}
	}
	isNewIP := !knownIPs[currentIP]

	currentFingerprint := normalizeLoginUserAgent(currentUserAgent)
	knownFingerprints := make(map[string]bool)
	for _, record := range records {
		if record.UserAgent != nil && *record.UserAgent != "" {
			knownFingerprints[normalizeLoginUserAgent(*record.UserAgent)] = true
		}
	}
	isNewDevice := !knownFingerprints[currentFingerprint]

	return NewLoginLocation{IsNewIP: isNewIP, IsNewDevice: isNewDevice}
}

// ==================== OAuth 一次性登录码 ====================

// OAuth login-code TTL, mirroring OAUTH_LOGIN_CODE_TTL_MS.
const oauthLoginCodeTTLMs = 60 * 1000

// OAuthLoginCodeInput mirrors Omit<OAuthLoginCodeData, 'timestamp' | 'nonce'>.
type OAuthLoginCodeInput struct {
	UserID   int32
	Username string
	Role     string
}

// OAuthLoginCodeData mirrors OAuthLoginCodeData. Field order matches the
// JSON.stringify insertion order.
type OAuthLoginCodeData struct {
	UserID    int32  `json:"userId"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	Timestamp int64  `json:"timestamp"`
	Nonce     string `json:"nonce"`
}

// GenerateOAuthLoginCode mirrors generateOAuthLoginCode().
func GenerateOAuthLoginCode(input OAuthLoginCodeInput) (string, error) {
	nonceBytes := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, nonceBytes); err != nil {
		return "", err
	}

	data := OAuthLoginCodeData{
		UserID:    input.UserID,
		Username:  input.Username,
		Role:      input.Role,
		Timestamp: securityNowMs(),
		Nonce:     hex.EncodeToString(nonceBytes),
	}

	encoded, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(encoded)
	signature, err := signOAuthPayload(payload)
	if err != nil {
		return "", err
	}
	return payload + "." + signature, nil
}

// VerifyAndConsumeOAuthLoginCode mirrors verifyAndConsumeOAuthLoginCode().
func VerifyAndConsumeOAuthLoginCode(code string) *OAuthLoginCodeData {
	parts := strings.Split(code, ".")
	if len(parts) != 2 {
		return nil
	}
	payload, signature := parts[0], parts[1]

	expectedSignature, err := signOAuthPayload(payload)
	if err != nil {
		return nil
	}
	if !timingSafeStringEqual(signature, expectedSignature) {
		return nil
	}

	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	var data OAuthLoginCodeData
	if err := json.Unmarshal(decoded, &data); err != nil {
		return nil
	}

	if securityNowMs()-data.Timestamp > oauthLoginCodeTTLMs {
		return nil
	}
	if usedOAuthLoginCodeNonces.has(data.Nonce) {
		return nil
	}
	usedOAuthLoginCodeNonces.mark(data.Nonce, data.Timestamp+oauthLoginCodeTTLMs)
	return &data
}

// ==================== 设备绑定 (AUTH002) ====================

// Device-binding configuration, mirroring DEVICE_BINDING_CONFIG. Like the
// TypeScript module these are resolved once at load time.
var (
	deviceBindingStrictMode     = os.Getenv("DEVICE_BINDING_STRICT") == "true"
	deviceBindingCheckUserAgent = os.Getenv("DEVICE_BINDING_CHECK_UA") != "false"
)

// DeviceBindingResult mirrors `{ valid: boolean; reason?: string }`.
type DeviceBindingResult struct {
	Valid  bool   `json:"valid"`
	Reason string `json:"reason,omitempty"`
}

var (
	deviceBindingFirefoxPattern = regexp.MustCompile(`Firefox`)
	deviceBindingEdgePattern    = regexp.MustCompile(`Edg`)
	deviceBindingChromePattern  = regexp.MustCompile(`Chrome`)
	deviceBindingSafariPattern  = regexp.MustCompile(`Safari`)
	deviceBindingOperaPattern   = regexp.MustCompile(`Opera`)
	deviceBindingWin10Pattern   = regexp.MustCompile(`Windows NT 10`)
	deviceBindingWindowsPattern = regexp.MustCompile(`Windows NT`)
	deviceBindingMacPattern     = regexp.MustCompile(`Mac OS X|Macintosh`)
	deviceBindingIOSPattern     = regexp.MustCompile(`iPhone|iPad|iPod`)
	deviceBindingAndroidPattern = regexp.MustCompile(`Android`)
	deviceBindingLinuxPattern   = regexp.MustCompile(`Linux`)
	deviceBindingMobilePattern  = regexp.MustCompile(`Mobile|Android|iPhone|iPad`)
)

// normalizeUserAgentForBinding mirrors normalizeUserAgentForBinding().
func normalizeUserAgentForBinding(userAgent string) string {
	if userAgent == "" || userAgent == "unknown" {
		return "unknown"
	}

	browser := "unknown"
	switch {
	case deviceBindingFirefoxPattern.MatchString(userAgent):
		browser = "firefox"
	case deviceBindingEdgePattern.MatchString(userAgent):
		browser = "edge"
	case deviceBindingChromePattern.MatchString(userAgent):
		browser = "chrome"
	case deviceBindingSafariPattern.MatchString(userAgent):
		browser = "safari"
	case deviceBindingOperaPattern.MatchString(userAgent):
		browser = "opera"
	}

	osName := "unknown"
	switch {
	case deviceBindingWin10Pattern.MatchString(userAgent):
		osName = "win10"
	case deviceBindingWindowsPattern.MatchString(userAgent):
		osName = "windows"
	case deviceBindingMacPattern.MatchString(userAgent):
		osName = "macos"
	case deviceBindingIOSPattern.MatchString(userAgent):
		osName = "ios"
	case deviceBindingAndroidPattern.MatchString(userAgent):
		osName = "android"
	case deviceBindingLinuxPattern.MatchString(userAgent):
		osName = "linux"
	}

	device := "d"
	if deviceBindingMobilePattern.MatchString(userAgent) {
		device = "m"
	}
	return browser + "|" + osName + "|" + device
}

// ValidateDeviceBinding mirrors validateDeviceBinding() (AUTH002). Note the
// feature is currently unused by the routes but is part of the module surface.
func ValidateDeviceBinding(storedIP, storedUserAgent, currentIP, currentUserAgent string) DeviceBindingResult {
	if deviceBindingStrictMode {
		if storedIP != "" && currentIP != "" && storedIP != currentIP {
			return DeviceBindingResult{Valid: false, Reason: fmt.Sprintf("IP mismatch: expected %s, got %s", storedIP, currentIP)}
		}
	}

	if deviceBindingCheckUserAgent {
		storedFingerprint := normalizeUserAgentForBinding(storedUserAgent)
		currentFingerprint := normalizeUserAgentForBinding(currentUserAgent)
		if storedFingerprint != "unknown" && currentFingerprint != "unknown" && storedFingerprint != currentFingerprint {
			return DeviceBindingResult{
				Valid:  false,
				Reason: fmt.Sprintf("Device fingerprint mismatch: expected %s, got %s", storedFingerprint, currentFingerprint),
			}
		}
	}

	return DeviceBindingResult{Valid: true}
}
