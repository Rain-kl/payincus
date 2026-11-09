// Port of server/src/routes/users.ts (1572 lines, 17 routes).
//
// Mounting notes (they are observable on the wire):
//
//   - app.ts registers this plugin with `{ prefix: '/api/users' }` and the list
//     route is declared as `'/'`. Fastify's `prefixTrailingSlash` defaults to
//     'both', so `/api/users` and `/api/users/` are BOTH valid. Gin does not
//     redirect by default here (RedirectTrailingSlash is off), so the list route is
//     registered twice; every other path is declared without a leading slash in the
//     TypeScript and therefore has a single spelling.
//   - Gin requires ONE wildcard name per path position tree-wide. `routes/traffic.go`
//     (Wave B) already registers `/api/users/:userId/traffic`, so the wildcard here
//     MUST be ":userId" — using ":id" panics at startup with
//     ':id' in new path '/api/users/:id' conflicts with existing wildcard ':userId'
//     (verified against gin v1.12.0).
//
// Ported seams for modules that are not in Wave D yet:
//
//   - `lib/terminal-proxy.ts#closeUserSessions` → UsersCloseUserSessions (below).
//   - `lib/email-domain.ts#validateEmailDomain` → usersValidateEmailDomain (below);
//     the white-list config keys are already seeded in db/system_config.go.
//   - `db/email-verification.ts` createVerificationCode/verifyCode for the
//     `change_email` purpose → usersCreateVerificationCode/usersVerifyCode (below).
//   - `lib/demo-safety.ts#shouldRedactDemoLoginRecords/redactDemoLoginRecord` →
//     composed from the exported lib.IsDemoRequest / lib.IsDemoAccount primitives.
//
// Passwords use golang.org/x/crypto/bcrypt (cost 12) instead of bcryptjs. The two
// implementations cross-verify ($2a$/$2b$), which is what login needs, and the
// digest format is the same.
package routes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/middleware"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// usersSearchFields mirrors USER_SEARCH_FIELDS.
var usersSearchFields = []string{"username", "id", "email"}

// usersMaxDetectedUsersPerGroup mirrors `maxUsersPerGroup` in detect-linked-accounts.
const usersMaxDetectedUsersPerGroup = 20

// usersPasswordBcryptCost mirrors `bcrypt.hash(password, 12)`.
const usersPasswordBcryptCost = 12

// UsersCloseUserSessions mirrors closeUserSessions(userId, reason) from
// server/src/lib/terminal-proxy.ts, which is not ported yet (the WebSocket terminal
// belongs to Wave E). The default returns 0, which is exactly what the TypeScript
// reports for a user with no live terminal session. The terminal module should
// assign this variable from its init().
var UsersCloseUserSessions = func(userID int32, reason string) int { return 0 }

// ==================== parsing helpers ====================

// usersParseUserSearchFields mirrors parseUserSearchFields.
func usersParseUserSearchFields(raw *string) []string {
	if raw == nil || *raw == "" {
		return append([]string(nil), usersSearchFields...)
	}

	valid := map[string]bool{}
	for _, field := range usersSearchFields {
		valid[field] = true
	}

	requested := make([]string, 0, 3)
	for _, field := range strings.Split(*raw, ",") {
		trimmed := strings.TrimSpace(field)
		if valid[trimmed] {
			requested = append(requested, trimmed)
		}
	}
	if len(requested) == 0 {
		return append([]string(nil), usersSearchFields...)
	}

	// Canonical order, mirroring `USER_SEARCH_FIELDS.filter(...)`.
	ordered := make([]string, 0, len(requested))
	for _, field := range usersSearchFields {
		for _, candidate := range requested {
			if candidate == field {
				ordered = append(ordered, field)
				break
			}
		}
	}
	return ordered
}

// usersParseBooleanQuery mirrors parseBooleanQuery.
func usersParseBooleanQuery(raw *string) bool {
	if raw == nil || *raw == "" {
		return false
	}
	switch strings.ToLower(*raw) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// usersParsePositiveIntegerQuery mirrors parsePositiveIntegerQuery (no upper clamp;
// the very large max makes lib's clamping a no-op).
func usersParsePositiveIntegerQuery(raw string, fallback int) int {
	return lib.ParsePositiveIntegerQuery(raw, fallback, math.MaxInt32)
}

// usersParseClampedPositiveIntegerQuery mirrors parseClampedPositiveIntegerQuery.
func usersParseClampedPositiveIntegerQuery(raw string, fallback, max int) int {
	return lib.ParsePositiveIntegerQuery(raw, fallback, max)
}

// usersQueryValue returns the query parameter and whether it was present, so the
// `= ”` vs absent distinction of `request.query` is preserved.
func usersQueryValue(c *gin.Context, name string) *string {
	values, present := c.Request.URL.Query()[name]
	if !present || len(values) == 0 {
		return nil
	}
	value := values[0]
	return &value
}

// ==================== email helpers ====================

// usersEmailFormatRegex is ajv-formats' email format in "full" mode, which is what
// @fastify/ajv-compiler installs (mode defaults to "full").
var usersEmailFormatRegex = regexp.MustCompile(`(?i)^[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

// usersEmailFormatValid mirrors AJV's `format: 'email'` check.
func usersEmailFormatValid(value string) bool {
	return usersEmailFormatRegex.MatchString(value)
}

// usersDefaultAllowedEmailDomains mirrors DEFAULT_ALLOWED_DOMAINS.
var usersDefaultAllowedEmailDomains = []string{
	"gmail.com",
	"outlook.com", "hotmail.com", "live.com", "msn.com",
	"icloud.com", "me.com", "mac.com",
	"yahoo.com",
	"zoho.com",
	"proton.me", "pm.me", "protonmail.com", "protonmail.ch",
	"fastmail.com",
	"tuta.com", "tutanota.com", "tuta.io", "keemail.me",
	"posteo.de", "posteo.net",
	"disroot.org",
	"riseup.net",
}

// usersExtractEmailDomain mirrors extractEmailDomain.
func usersExtractEmailDomain(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// usersAllowedEmailDomains mirrors getAllowedEmailDomains.
func usersAllowedEmailDomains(ctx context.Context) ([]string, error) {
	raw, err := db.GetSystemConfig(ctx, "email_allowed_domains", false)
	if err != nil {
		return nil, err
	}
	if raw == nil || *raw == "" {
		return append([]string(nil), usersDefaultAllowedEmailDomains...), nil
	}
	domains := make([]string, 0, 8)
	for _, domain := range strings.Split(*raw, ",") {
		trimmed := strings.ToLower(strings.TrimSpace(domain))
		if trimmed != "" {
			domains = append(domains, trimmed)
		}
	}
	return domains, nil
}

// usersValidateEmailDomain mirrors validateEmailDomain.
func usersValidateEmailDomain(ctx context.Context, email string) (domain string, valid bool, err error) {
	enabled, err := db.GetSystemConfigBoolean(ctx, "email_domain_whitelist_enabled", false)
	if err != nil {
		return "", false, err
	}
	if !enabled {
		return "", true, nil
	}

	domain = usersExtractEmailDomain(email)
	if domain == "" {
		return "", false, nil
	}

	allowed, err := usersAllowedEmailDomains(ctx)
	if err != nil {
		return "", false, err
	}
	for _, candidate := range allowed {
		if candidate == domain {
			return domain, true, nil
		}
	}
	return domain, false, nil
}

// usersEmailValidationError carries apiError(code, details) from
// validateEmailChangeTarget.
type usersEmailValidationError struct {
	Code    lib.ErrorCode
	Details string
}

// usersValidateEmailChangeTarget mirrors validateEmailChangeTarget.
func usersValidateEmailChangeTarget(
	ctx context.Context,
	rawEmail string,
	currentEmail *string,
	userID int32,
) (string, *usersEmailValidationError, error) {
	if rawEmail == "" || !strings.Contains(rawEmail, "@") {
		return "", &usersEmailValidationError{Code: lib.ErrInvalidEmail}, nil
	}

	emailWithoutAllowed := strings.ReplaceAll(strings.ReplaceAll(rawEmail, "@", ""), ".", "")
	if lib.ContainsDangerousChars(emailWithoutAllowed) {
		return "", &usersEmailValidationError{Code: lib.ErrEmailContainsIllegal}, nil
	}

	normalizedEmail := strings.ToLower(strings.TrimSpace(rawEmail))
	normalizedCurrentEmail := ""
	if currentEmail != nil {
		normalizedCurrentEmail = strings.ToLower(strings.TrimSpace(*currentEmail))
	}
	if normalizedCurrentEmail != "" && normalizedEmail == normalizedCurrentEmail {
		return "", &usersEmailValidationError{
			Code:    lib.ErrInvalidParams,
			Details: "New email must be different from current email",
		}, nil
	}

	domain, valid, err := usersValidateEmailDomain(ctx, normalizedEmail)
	if err != nil {
		return "", nil, err
	}
	if !valid {
		return "", &usersEmailValidationError{Code: lib.ErrEmailDomainNotAllowed, Details: domain}, nil
	}

	existingUser, err := db.FindUserByEmail(ctx, normalizedEmail)
	if err != nil {
		return "", nil, err
	}
	if existingUser != nil && existingUser.ID != userID {
		return "", &usersEmailValidationError{Code: lib.ErrEmailAlreadyRegistered}, nil
	}

	return normalizedEmail, nil, nil
}

// ==================== demo helpers ====================

// usersDemoHosts mirrors the extraction used by middleware/demo_safety.go.
func usersDemoHosts(c *gin.Context) (forwardedHost, host string) {
	return c.GetHeader("X-Forwarded-Host"), c.Request.Host
}

// usersShouldProtectDemoAccount mirrors shouldProtectDemoAccount(request, user).
func usersShouldProtectDemoAccount(c *gin.Context, user *db.UserRecord) bool {
	forwarded, host := usersDemoHosts(c)
	email := ""
	if user.Email != nil {
		email = *user.Email
	}
	return lib.ShouldProtectDemoAccount(forwarded, host, user.Username, email)
}

// usersShouldRedactDemoLoginRecords mirrors shouldRedactDemoLoginRecords, which is
// simply an alias of shouldProtectDemoAccount in lib/demo-safety.ts.
func usersShouldRedactDemoLoginRecords(c *gin.Context, user *db.UserRecord) bool {
	return usersShouldProtectDemoAccount(c, user)
}

// usersRedactDemoLoginRecord mirrors redactDemoLoginRecord.
func usersRedactDemoLoginRecord(record db.LoginRecordItem) db.LoginRecordItem {
	record.IP = lib.DemoRedactedIP
	record.Country = nil
	record.Region = nil
	record.City = nil
	record.Isp = nil
	record.Timezone = nil
	record.UserAgent = nil
	return record
}

// ==================== response/timestamp helpers ====================

// usersISOTime mirrors Date.prototype.toISOString().
func usersISOTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// usersRespondInternal mirrors the 5xx branch of app.ts' setErrorHandler in
// production: no internal detail leaks.
func usersRespondInternal(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Internal server error"})
}

// usersRespondVerificationRequired mirrors the three
// `{ error, code: 'VERIFICATION_REQUIRED', operationType }` responses.
func usersRespondVerificationRequired(c *gin.Context, operationType models.OperationType) {
	c.JSON(http.StatusForbidden, gin.H{
		"error":         "Sensitive operation requires verification",
		"code":          "VERIFICATION_REQUIRED",
		"operationType": string(operationType),
	})
}

// usersOperationVerified mirrors `isOperationVerified(userId, type)` (resourceId
// omitted → null in the TypeScript, which operationResourceClause treats as 0).
func usersOperationVerified(ctx context.Context, userID int32, operationType models.OperationType) (bool, error) {
	return lib.IsOperationVerified(ctx, int64(userID), operationType, 0)
}

// usersClaimOperationVerification mirrors claimOperationVerificationRequirement.
func usersClaimOperationVerification(ctx context.Context, userID int32, operationType models.OperationType) (lib.OperationVerificationRequirement, error) {
	return lib.ClaimOperationVerificationRequirement(ctx, int64(userID), operationType, 0)
}

// ==================== body validation ====================

// usersDecodedBody decodes the request body. ok=false means the response has
// already been written (Fastify's parser errors).
func usersDecodedBody(c *gin.Context) (any, bool) {
	decoded, status, message := decodeJSONRequestBody(c)
	if status != 0 {
		c.JSON(status, gin.H{"error": message})
		return nil, false
	}
	return decoded.Value, true
}

// usersBodyObject applies AJV's root `type: 'object'` check. A missing body is
// `undefined` in Fastify, which the validator receives as `null` → "must be object".
func usersBodyObject(decoded any) (map[string]any, string) {
	if decoded == nil {
		return nil, "must be object"
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, "must be object"
	}
	return object, ""
}

// usersValidateEmailProperty validates the `email` property in schema order,
// including `format: 'email'` which ajvFieldSpec does not model. It returns the
// coerced value, whether it was present, and the AJV-style message.
func usersValidateEmailProperty(object map[string]any) (string, bool, string) {
	raw, present := object["email"]
	if !present {
		return "", false, ""
	}

	coerced, message := ajvCoerceField(ajvFieldSpec{Name: "email", Kind: ajvKindString, MinLength: -1, MaxLength: -1}, raw)
	if message != "" {
		return "", true, message
	}
	text, _ := coerced.(string)
	if !usersEmailFormatValid(text) {
		return "", true, "Please enter a valid email address"
	}
	return text, true, ""
}

// usersAJVMessage rewrites the one minLength message app.ts special-cases.
func usersAJVMessage(message string) string {
	if message == "password is too short" {
		// app.ts: minLength on `password` always renders this text (even though the
		// route schemas use minLength 6).
		return "Password must be at least 8 characters"
	}
	return message
}

// usersAvatarStyleEnum mirrors the avatarStyle enum of the PATCH /:id schema.
func usersAvatarStyleEnum() []string {
	return append([]string{""}, db.AvatarStyles...)
}

// usersValidatePatchBody mirrors the PATCH /:id body schema:
// email (string, format email), password (string, minLength 6),
// emailCode (string, minLength 6, maxLength 6), avatarStyle (string, enum).
//
// Properties are validated in schema order and only the first failure is reported
// (allErrors: false).
func usersValidatePatchBody(decoded any) (email string, emailPresent bool, password *string, emailCode *string, avatarStyle *string, message string) {
	object, message := usersBodyObject(decoded)
	if message != "" {
		return "", false, nil, nil, nil, message
	}

	email, emailPresent, message = usersValidateEmailProperty(object)
	if message != "" {
		return "", false, nil, nil, nil, message
	}

	validated, message := validateAJVObject(decoded, nil, []ajvFieldSpec{
		{Name: "password", Kind: ajvKindString, MinLength: 6, MaxLength: -1},
		{Name: "emailCode", Kind: ajvKindString, MinLength: 6, MaxLength: 6},
		{Name: "avatarStyle", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: usersAvatarStyleEnum()},
	})
	if message != "" {
		return "", false, nil, nil, nil, usersAJVMessage(message)
	}

	if value, present := validated.stringValue("password"); present {
		password = &value
	}
	if value, present := validated.stringValue("emailCode"); present {
		emailCode = &value
	}
	if value, present := validated.stringValue("avatarStyle"); present {
		avatarStyle = &value
	}
	return email, emailPresent, password, emailCode, avatarStyle, ""
}

// usersValidateChangeEmailBody mirrors POST /:id/change-email/send-code's schema:
// required ['email'], properties email (string, format email).
func usersValidateChangeEmailBody(decoded any) (string, string) {
	object, message := usersBodyObject(decoded)
	if message != "" {
		return "", message
	}
	if _, present := object["email"]; !present {
		return "", "email is required"
	}
	email, _, message := usersValidateEmailProperty(object)
	return email, message
}

// ==================== email verification (change_email) ====================
// Narrow port of the parts of db/email-verification.ts that this module uses.
// db/email-verification.go does not exist yet; when it lands it should replace
// these helpers.

const (
	usersVerificationPurposeChangeEmail = "change_email"
	usersVerificationPurposeGeneral     = "general"
	usersVerificationCodeTTL            = 10 * time.Minute
	usersVerificationMaxCodesPerHour    = 5
	usersVerificationHashPrefix         = "sha256:"
)

// usersVerificationCode mirrors createVerificationCode's { code, expiresAt }.
type usersVerificationCode struct {
	Code      string
	ExpiresAt time.Time
}

// usersGenerateVerificationCode mirrors generateVerificationCode: crypto.randomInt
// between 100000 and 999999 inclusive.
func usersGenerateVerificationCode() (string, error) {
	limit := big.NewInt(900000)
	offset, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(offset.Int64()+100000, 10), nil
}

// usersHashVerificationCode mirrors hashVerificationCode.
func usersHashVerificationCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return usersVerificationHashPrefix + hex.EncodeToString(sum[:])
}

// usersVerificationCodeMatches mirrors verificationCodeMatches.
func usersVerificationCodeMatches(storedCode, inputCode string) bool {
	if strings.HasPrefix(storedCode, usersVerificationHashPrefix) {
		return storedCode == usersHashVerificationCode(inputCode)
	}
	return storedCode == inputCode
}

// usersCreateVerificationCode mirrors createVerificationCode for change_email.
// (nil, nil) means the per-email hourly limit was hit (route → 429).
func usersCreateVerificationCode(ctx context.Context, email string) (*usersVerificationCode, error) {
	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	now := time.Now().UTC()
	oneHourAgo := now.Add(-time.Hour)

	var recentCount int64
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM email_verification_codes
		  WHERE email = ? AND purpose = ? AND created_at >= ?`,
		normalizedEmail, usersVerificationPurposeChangeEmail, oneHourAgo,
	).Scan(&recentCount).Error; err != nil {
		return nil, err
	}
	if recentCount >= usersVerificationMaxCodesPerHour {
		return nil, nil
	}

	// Expire pending codes so only the newest works while recent rows remain for
	// rate limiting.
	if err := db.DB.WithContext(ctx).Exec(
		`UPDATE email_verification_codes
		    SET expires_at = ?
		  WHERE email = ? AND purpose IN (?, ?) AND expires_at > ?`,
		now, normalizedEmail, usersVerificationPurposeChangeEmail, usersVerificationPurposeGeneral, now,
	).Error; err != nil {
		return nil, err
	}

	code, err := usersGenerateVerificationCode()
	if err != nil {
		return nil, err
	}
	expiresAt := now.Add(usersVerificationCodeTTL)

	if err := db.DB.WithContext(ctx).Exec(
		`INSERT INTO email_verification_codes (email, purpose, code, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		normalizedEmail, usersVerificationPurposeChangeEmail, usersHashVerificationCode(code), expiresAt, now,
	).Error; err != nil {
		return nil, err
	}

	return &usersVerificationCode{Code: code, ExpiresAt: expiresAt}, nil
}

// usersVerifyCode mirrors verifyCode: find the newest matching live code, then
// claim it atomically by deleting exactly that row.
func usersVerifyCode(ctx context.Context, email, code string) (bool, error) {
	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	codeHash := usersHashVerificationCode(code)
	now := time.Now().UTC()

	var candidates []struct {
		ID   int32  `gorm:"column:id"`
		Code string `gorm:"column:code"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		`SELECT id, code FROM email_verification_codes
		  WHERE email = ? AND purpose IN (?, ?) AND expires_at > ?
		  ORDER BY created_at DESC`,
		normalizedEmail, usersVerificationPurposeChangeEmail, usersVerificationPurposeGeneral, now,
	).Scan(&candidates).Error; err != nil {
		return false, err
	}

	var matchedID int32
	for _, candidate := range candidates {
		if candidate.Code == codeHash || usersVerificationCodeMatches(candidate.Code, code) {
			matchedID = candidate.ID
			break
		}
	}
	if matchedID == 0 {
		return false, nil
	}

	result := db.DB.WithContext(ctx).Exec("DELETE FROM email_verification_codes WHERE id = ?", matchedID)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// ==================== role-change transaction ====================

var (
	errUsersRoleLockBusy    = errors.New("USER_ADMIN_ROLE_LOCK_BUSY")
	errUsersRoleUserMissing = errors.New("USER_NOT_FOUND")
	errUsersRoleNotActive   = errors.New("TARGET_NOT_ACTIVE")
	errUsersRoleLastAdmin   = errors.New("LAST_ACTIVE_ADMIN")
)

// usersRoleUpdateResult mirrors the route-local RoleUpdateResult type.
type usersRoleUpdateResult struct {
	Changed         bool
	Username        string
	PreviousRole    models.UserRole
	Role            models.UserRole
	RevokedSessions int64
}

// usersUpdateUserRole mirrors the `prisma.$transaction` body of PATCH /:id/role:
// take the USER_ADMIN_ROLE_LOCK_NAMESPACE lock, validate, update the role, drop all
// refresh tokens and bump the user-level access-token invalidation — all atomically.
func usersUpdateUserRole(ctx context.Context, userID int32, role models.UserRole) (usersRoleUpdateResult, error) {
	if db.Pool() == nil {
		return usersRoleUpdateResult{}, db.ErrUsersDatabaseNotInitialized
	}

	tx, err := db.Pool().Begin(ctx)
	if err != nil {
		return usersRoleUpdateResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	locked, err := db.TryAdvisoryTransactionLock(ctx, tx, db.UserAdminRoleLockNamespace, 1)
	if err != nil {
		return usersRoleUpdateResult{}, err
	}
	if !locked {
		return usersRoleUpdateResult{}, errUsersRoleLockBusy
	}

	var username string
	var previousRoleText string
	var statusText string
	err = tx.QueryRow(ctx,
		"SELECT username, role::text, status::text FROM users WHERE id = $1", userID,
	).Scan(&username, &previousRoleText, &statusText)
	if errors.Is(err, pgx.ErrNoRows) {
		return usersRoleUpdateResult{}, errUsersRoleUserMissing
	}
	if err != nil {
		return usersRoleUpdateResult{}, err
	}
	previousRole := models.UserRole(previousRoleText)
	status := models.UserStatus(statusText)

	if role == previousRole {
		if err := tx.Commit(ctx); err != nil {
			return usersRoleUpdateResult{}, err
		}
		return usersRoleUpdateResult{Changed: false, Username: username, PreviousRole: previousRole, Role: previousRole}, nil
	}

	if role == models.UserRoleAdmin && status != models.UserStatusActive {
		return usersRoleUpdateResult{}, errUsersRoleNotActive
	}

	if previousRole == models.UserRoleAdmin && role == models.UserRoleUser && status == models.UserStatusActive {
		var activeAdminCount int64
		if err := tx.QueryRow(ctx,
			"SELECT COUNT(*) FROM users WHERE role = 'admin' AND status = 'active'",
		).Scan(&activeAdminCount); err != nil {
			return usersRoleUpdateResult{}, err
		}
		if activeAdminCount <= 1 {
			return usersRoleUpdateResult{}, errUsersRoleLastAdmin
		}
	}

	if _, err := tx.Exec(ctx, "UPDATE users SET role = $1, updated_at = NOW() WHERE id = $2", string(role), userID); err != nil {
		return usersRoleUpdateResult{}, err
	}

	revoked, err := tx.Exec(ctx, "DELETE FROM refresh_tokens WHERE user_id = $1", userID)
	if err != nil {
		return usersRoleUpdateResult{}, err
	}

	invalidatedAt := int32(time.Now().Unix())
	if _, err := tx.Exec(ctx,
		`INSERT INTO token_invalidations (user_id, session_id, invalidated_at, created_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (user_id, session_id)
		 DO UPDATE SET invalidated_at = EXCLUDED.invalidated_at`,
		userID, lib.UserLevelSessionID, invalidatedAt,
	); err != nil {
		return usersRoleUpdateResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return usersRoleUpdateResult{}, err
	}

	return usersRoleUpdateResult{
		Changed:         true,
		Username:        username,
		PreviousRole:    previousRole,
		Role:            role,
		RevokedSessions: revoked.RowsAffected(),
	}, nil
}

// ==================== registration ====================

// RegisterUsers mounts /api/users.
func RegisterUsers(r *gin.Engine) {
	group := r.Group("/api/users")

	// 获取用户列表 (管理员，支持分页和搜索)
	// Fastify prefixTrailingSlash='both' → "/api/users" and "/api/users/" both route here.
	group.GET("", middleware.AuthenticateAdmin(), usersList)
	group.GET("/", middleware.AuthenticateAdmin(), usersList)

	// 按用户名精确查找用户（管理员，用于创建实例等场景）
	group.GET("/lookup", middleware.AuthenticateAdmin(), usersLookup)

	// 检测关联账号 must be declared before the ":userId" subtree for readability;
	// gin resolves static segments first regardless.
	group.GET("/detect-linked-accounts", middleware.AuthenticateAdmin(), usersDetectLinkedAccounts)

	// 获取用户详情
	group.GET("/:userId", middleware.Authenticate(), usersDetail)

	// 发送修改邮箱验证码（登录态，发送到新邮箱）
	group.POST("/:userId/change-email/send-code", middleware.Authenticate(), usersChangeEmailSendCode)

	// 更新用户信息
	group.PATCH("/:userId", middleware.Authenticate(), usersUpdate)

	// Change user role (admin)
	group.PATCH("/:userId/role", middleware.AuthenticateAdmin(), usersUpdateRole)

	// 重算用户配额使用量 (管理员)
	group.POST("/:userId/quota/recalculate", middleware.AuthenticateAdmin(), usersRecalculateQuota)

	// 用户自行增加配额
	group.POST("/me/quota/increase", middleware.AuthenticateUser(), usersIncreaseQuota)

	// 封禁/解封用户 (管理员)
	group.PATCH("/:userId/status", middleware.AuthenticateAdmin(), usersUpdateStatus)

	// 删除用户 (管理员)
	group.DELETE("/:userId", middleware.AuthenticateAdmin(), usersDelete)

	// 管理员获取指定用户的所有会话
	group.GET("/:userId/sessions", middleware.AuthenticateAdmin(), usersSessions)

	// 管理员撤销指定用户的所有会话
	group.POST("/:userId/revoke-sessions", middleware.AuthenticateAdmin(), usersRevokeSessions)

	// 管理员重置用户密码
	group.POST("/:userId/reset-password", middleware.AuthenticateAdmin(), usersResetPassword)

	// 管理员取消用户的 2FA
	group.POST("/:userId/disable-2fa", middleware.AuthenticateAdmin(), usersDisable2FA)

	// 管理员解除用户的 OAuth 绑定
	group.DELETE("/:userId/oauth/:provider", middleware.AuthenticateAdmin(), usersUnbindOAuth)

	// 管理员获取用户登录记录
	group.GET("/:userId/login-records", middleware.AuthenticateAdmin(), usersLoginRecords)
}

// ==================== GET /api/users ====================

// usersListQuotaView mirrors the `quota` object the list route always emits.
type usersListQuotaView struct {
	HostLimit           int32                `json:"hostLimit"`
	HostUsed            int32                `json:"hostUsed"`
	FriendLimit         int32                `json:"friendLimit"`
	FriendUsed          int32                `json:"friendUsed"`
	PackageLimit        int32                `json:"packageLimit"`
	PackageUsed         int32                `json:"packageUsed"`
	MonthlyTrafficLimit *string              `json:"monthlyTrafficLimit"`
	MonthlyTrafficUsed  string               `json:"monthlyTrafficUsed"`
	ExtraTrafficQuota   string               `json:"extraTrafficQuota"`
	TrafficStatus       models.TrafficStatus `json:"trafficStatus"`
}

// usersListView is one element of the list response.
type usersListView struct {
	ID                int32              `json:"id"`
	Username          string             `json:"username"`
	Email             *string            `json:"email"`
	Role              models.UserRole    `json:"role"`
	Status            models.UserStatus  `json:"status"`
	AvatarStyle       string             `json:"avatarStyle"`
	AvatarBadgeID     *string            `json:"avatarBadgeId"`
	TwoFAEnabled      bool               `json:"twoFAEnabled"`
	InstanceCount     int64              `json:"instanceCount"`
	HasGithubBinding  bool               `json:"hasGithubBinding"`
	Quota             usersListQuotaView `json:"quota"`
	CreatedAt         string             `json:"createdAt"`
	LastLogin         *usersLastLogin    `json:"lastLogin"`
	Balance           float64            `json:"balance"`
	TotalConsume      float64            `json:"totalConsume"`
	Points            int32              `json:"points"`
	TotalEarnedPoints int32              `json:"totalEarnedPoints"`
}

// usersLastLogin mirrors the lastLogin object.
type usersLastLogin struct {
	IP        string  `json:"ip"`
	Country   *string `json:"country"`
	City      *string `json:"city"`
	CreatedAt string  `json:"createdAt"`
}

// usersList mirrors the GET / handler.
func usersList(c *gin.Context) {
	ctx := c.Request.Context()

	pageRaw := ""
	if value := usersQueryValue(c, "page"); value != nil {
		pageRaw = *value
	}
	pageSizeRaw := ""
	if value := usersQueryValue(c, "pageSize"); value != nil {
		pageSizeRaw = *value
	}
	search := ""
	if value := usersQueryValue(c, "search"); value != nil {
		search = *value
	}

	result, err := db.GetUsersPaginated(ctx, db.UsersPaginatedOptions{
		Page:         usersParsePositiveIntegerQuery(pageRaw, 1),
		PageSize:     usersParseClampedPositiveIntegerQuery(pageSizeRaw, 20, 100),
		Search:       search,
		SearchFields: usersParseUserSearchFields(usersQueryValue(c, "searchFields")),
		ExactMatch:   usersParseBooleanQuery(usersQueryValue(c, "exact")),
	})
	if err != nil {
		usersRespondInternal(c)
		return
	}

	userIDs := make([]int32, 0, len(result.Items))
	for _, item := range result.Items {
		userIDs = append(userIDs, item.ID)
	}

	// The TypeScript issues one is2FAEnabled / getInstancesByUserId / hasOAuthBinding /
	// getUserBalance query per row. The batched equivalents below are the same result
	// set without the N+1.
	twoFAMap, err := usersTwoFAStatusMap(ctx, userIDs)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	instanceCountMap, err := db.CountInstancesByUserIDs(ctx, userIDs)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	githubMap, err := usersGithubBindingMap(ctx, userIDs)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	quotaMap, err := db.GetUserQuotaMap(ctx, userIDs)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	lastLoginMap, err := db.GetLastLoginRecordsForUsers(ctx, userIDs)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	balanceMap, err := usersBalanceMap(ctx, userIDs)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	consumeMap, err := db.GetUsersTotalConsumeMap(ctx, userIDs, nil)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	pointsMap, err := usersPointsMap(ctx, userIDs)
	if err != nil {
		usersRespondInternal(c)
		return
	}

	users := make([]usersListView, 0, len(result.Items))
	for _, item := range result.Items {
		quota := usersListQuotaView{
			MonthlyTrafficUsed: "0",
			ExtraTrafficQuota:  "0",
			TrafficStatus:      models.TrafficStatusNORMAL,
		}
		if record, ok := quotaMap[item.ID]; ok {
			quota.HostLimit = record.HostLimit
			quota.HostUsed = record.HostUsed
			quota.FriendLimit = record.FriendLimit
			quota.FriendUsed = record.FriendUsed
			quota.PackageLimit = record.PackageLimit
			quota.PackageUsed = record.PackageUsed
			quota.MonthlyTrafficUsed = strconv.FormatInt(record.MonthlyTrafficUsed, 10)
			quota.ExtraTrafficQuota = strconv.FormatInt(record.ExtraTrafficQuota, 10)
			if record.TrafficStatus != "" {
				quota.TrafficStatus = record.TrafficStatus
			}
			if record.MonthlyTrafficLimit != nil {
				limit := strconv.FormatInt(*record.MonthlyTrafficLimit, 10)
				quota.MonthlyTrafficLimit = &limit
			}
		}

		var lastLogin *usersLastLogin
		if record := lastLoginMap[item.ID]; record != nil {
			redacted := record
			if usersShouldRedactDemoLoginRecords(c, &item) {
				copied := usersRedactDemoLoginRecord(*record)
				redacted = &copied
			}
			lastLogin = &usersLastLogin{
				IP:        redacted.IP,
				Country:   redacted.Country,
				City:      redacted.City,
				CreatedAt: usersISOTime(redacted.CreatedAt),
			}
		}

		points := pointsMap[item.ID]
		users = append(users, usersListView{
			ID:                item.ID,
			Username:          item.Username,
			Email:             item.Email,
			Role:              item.Role,
			Status:            item.Status,
			AvatarStyle:       item.AvatarStyle,
			AvatarBadgeID:     item.AvatarBadgeID,
			TwoFAEnabled:      twoFAMap[item.ID],
			InstanceCount:     instanceCountMap[item.ID],
			HasGithubBinding:  githubMap[item.ID],
			Quota:             quota,
			CreatedAt:         item.CreatedAt,
			LastLogin:         lastLogin,
			Balance:           balanceMap[item.ID],
			TotalConsume:      consumeMap[item.ID],
			Points:            points.Points,
			TotalEarnedPoints: points.TotalEarned,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"users":      users,
		"total":      result.Total,
		"page":       result.Page,
		"pageSize":   result.PageSize,
		"totalPages": result.TotalPages,
	})
}

// usersTwoFAStatusMap batches `is2FAEnabled(id)` for the list route.
func usersTwoFAStatusMap(ctx context.Context, userIDs []int32) (map[int32]bool, error) {
	result := make(map[int32]bool, len(userIDs))
	if len(userIDs) == 0 {
		return result, nil
	}
	var rows []struct {
		ID               int32 `gorm:"column:id"`
		TwoFactorEnabled bool  `gorm:"column:two_factor_enabled"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT id, two_factor_enabled FROM users WHERE id IN ?", userIDs,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.ID] = row.TwoFactorEnabled
	}
	return result, nil
}

// usersGithubBindingMap batches `hasOAuthBinding(id, 'github')`.
func usersGithubBindingMap(ctx context.Context, userIDs []int32) (map[int32]bool, error) {
	result := make(map[int32]bool, len(userIDs))
	if len(userIDs) == 0 {
		return result, nil
	}
	var rows []struct {
		UserID int32 `gorm:"column:user_id"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT user_id FROM user_oauth_bindings WHERE provider = 'github' AND user_id IN ?", userIDs,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.UserID] = true
	}
	return result, nil
}

// usersBalanceMap batches `getUserBalance(id)` (a missing user is 0).
func usersBalanceMap(ctx context.Context, userIDs []int32) (map[int32]float64, error) {
	result := make(map[int32]float64, len(userIDs))
	if len(userIDs) == 0 {
		return result, nil
	}
	var rows []struct {
		ID      int32          `gorm:"column:id"`
		Balance models.Decimal `gorm:"column:balance"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT id, balance FROM users WHERE id IN ?", userIDs,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		value, _ := row.Balance.Decimal.Float64()
		result[row.ID] = value
	}
	return result, nil
}

// usersPointsEntry is the {points, totalEarned} projection.
type usersPointsEntry struct {
	Points      int32
	TotalEarned int32
}

// usersPointsMap batches the userPoints lookup of the list route.
func usersPointsMap(ctx context.Context, userIDs []int32) (map[int32]usersPointsEntry, error) {
	result := make(map[int32]usersPointsEntry, len(userIDs))
	if len(userIDs) == 0 {
		return result, nil
	}
	var rows []struct {
		UserID      int32 `gorm:"column:user_id"`
		Points      int32 `gorm:"column:points"`
		TotalEarned int32 `gorm:"column:total_earned"`
	}
	if err := db.DB.WithContext(ctx).Raw(
		"SELECT user_id, points, total_earned FROM user_points WHERE user_id IN ?", userIDs,
	).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		result[row.UserID] = usersPointsEntry{Points: row.Points, TotalEarned: row.TotalEarned}
	}
	return result, nil
}

// ==================== GET /api/users/lookup ====================

// usersLookup mirrors GET /lookup.
func usersLookup(c *gin.Context) {
	ctx := c.Request.Context()

	username := ""
	if value := usersQueryValue(c, "username"); value != nil {
		username = *value
	}
	if strings.TrimSpace(username) == "" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Username is required")
		return
	}

	var rows []struct {
		ID       int32             `gorm:"column:id"`
		Username string            `gorm:"column:username"`
		Email    *string           `gorm:"column:email"`
		Role     models.UserRole   `gorm:"column:role"`
		Status   models.UserStatus `gorm:"column:status"`
		Balance  models.Decimal    `gorm:"column:balance"`
	}
	err := db.DB.WithContext(ctx).Raw(
		`SELECT id, username, email, role::text AS role, status::text AS status, balance
		   FROM users WHERE username = ? LIMIT 1`, strings.TrimSpace(username),
	).Scan(&rows).Error
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if len(rows) == 0 {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	balance, _ := rows[0].Balance.Decimal.Float64()
	c.JSON(http.StatusOK, gin.H{
		"user": gin.H{
			"id":       rows[0].ID,
			"username": rows[0].Username,
			"email":    rows[0].Email,
			"role":     rows[0].Role,
			"status":   rows[0].Status,
			"balance":  balance,
		},
	})
}

// ==================== GET /api/users/:id ====================

// usersDetailQuota mirrors the detail route's quota object.
type usersDetailQuota struct {
	HostLimit           int32                `json:"hostLimit"`
	HostUsed            int32                `json:"hostUsed"`
	FriendLimit         int32                `json:"friendLimit"`
	FriendUsed          int32                `json:"friendUsed"`
	PackageLimit        int32                `json:"packageLimit"`
	PackageUsed         int32                `json:"packageUsed"`
	MonthlyTrafficLimit *string              `json:"monthlyTrafficLimit"`
	MonthlyTrafficUsed  string               `json:"monthlyTrafficUsed"`
	ExtraTrafficQuota   string               `json:"extraTrafficQuota"`
	TrafficStatus       models.TrafficStatus `json:"trafficStatus"`
}

// usersDetailView mirrors the `user` object of GET /:id.
type usersDetailView struct {
	ID            int32             `json:"id"`
	Username      string            `json:"username"`
	Email         *string           `json:"email"`
	Role          models.UserRole   `json:"role"`
	Status        models.UserStatus `json:"status"`
	AvatarStyle   string            `json:"avatarStyle"`
	AvatarBadgeID *string           `json:"avatarBadgeId"`
	Quota         *usersDetailQuota `json:"quota"`
	InstanceCount int64             `json:"instanceCount"`
	CreatedAt     string            `json:"createdAt"`
}

// usersDetail mirrors GET /:id.
func usersDetail(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	userID, ok := lib.ParsePositiveRouteID(c.Param("userId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	// 普通用户只能查看自己
	if claims.Role != "admin" && int64(claims.ID) != userID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	user, err := db.FindUserById(ctx, int32(userID))
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	quota, err := db.GetUserQuota(ctx, user.ID)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	instanceCount, err := db.CountInstancesByUser(ctx, user.ID)
	if err != nil {
		usersRespondInternal(c)
		return
	}

	var quotaView *usersDetailQuota
	if quota != nil {
		view := usersDetailQuota{
			HostLimit:          quota.HostLimit,
			HostUsed:           quota.HostUsed,
			FriendLimit:        quota.FriendLimit,
			FriendUsed:         quota.FriendUsed,
			PackageLimit:       quota.PackageLimit,
			PackageUsed:        quota.PackageUsed,
			MonthlyTrafficUsed: strconv.FormatInt(quota.MonthlyTrafficUsed, 10),
			ExtraTrafficQuota:  strconv.FormatInt(quota.ExtraTrafficQuota, 10),
			TrafficStatus:      quota.TrafficStatus,
		}
		if quota.MonthlyTrafficLimit != nil {
			limit := strconv.FormatInt(*quota.MonthlyTrafficLimit, 10)
			view.MonthlyTrafficLimit = &limit
		}
		quotaView = &view
	}

	c.JSON(http.StatusOK, gin.H{
		"user": usersDetailView{
			ID:            user.ID,
			Username:      user.Username,
			Email:         user.Email,
			Role:          user.Role,
			Status:        user.Status,
			AvatarStyle:   user.AvatarStyle,
			AvatarBadgeID: user.AvatarBadgeID,
			Quota:         quotaView,
			InstanceCount: instanceCount,
			CreatedAt:     user.CreatedAt,
		},
	})
}

// ==================== POST /api/users/:id/change-email/send-code ====================

// usersChangeEmailSendCode mirrors POST /:id/change-email/send-code.
func usersChangeEmailSendCode(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	// Fastify validates the body schema in preValidation, i.e. BEFORE the handler
	// runs, so a schema error outranks an invalid route id.
	decoded, bodyOK := usersDecodedBody(c)
	if !bodyOK {
		return
	}
	email, validationMessage := usersValidateChangeEmailBody(decoded)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	userID, ok := lib.ParsePositiveRouteID(c.Param("userId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	// Only the account owner may start an email change (admins included).
	if int64(claims.ID) != userID {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	user, err := db.FindUserById(ctx, int32(userID))
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	smtpEnabled, err := lib.IsSMTPEnabled(ctx)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if !smtpEnabled {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailVerificationDisabled, "")
		return
	}

	if user.Email != nil {
		verified, err := usersOperationVerified(ctx, int32(userID), models.OperationTypeChangeEmail)
		if err != nil {
			usersRespondInternal(c)
			return
		}
		if !verified {
			usersRespondVerificationRequired(c, models.OperationTypeChangeEmail)
			return
		}
	}

	normalizedEmail, validationErr, err := usersValidateEmailChangeTarget(ctx, email, user.Email, user.ID)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if validationErr != nil {
		lib.RespondAPIError(c, http.StatusBadRequest, validationErr.Code, validationErr.Details)
		return
	}

	code, err := usersCreateVerificationCode(ctx, normalizedEmail)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if code == nil {
		lib.RespondAPIError(c, http.StatusTooManyRequests, lib.ErrTooManyVerificationRequests, "")
		return
	}

	sendResult := lib.SendVerificationEmail(ctx, normalizedEmail, code.Code, 10, lib.VerificationPurposeChangeEmail)
	if !sendResult.Success {
		lib.RespondAPIError(c, http.StatusInternalServerError, lib.ErrEmailSendFailed, "")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":   "Verification code sent",
		"expiresAt": usersISOTime(code.ExpiresAt),
	})
}

// ==================== PATCH /api/users/:id ====================

// usersUpdate mirrors PATCH /:id.
func usersUpdate(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	// Fastify validates the body schema in preValidation, i.e. BEFORE the handler
	// runs, so a schema error outranks an invalid route id.
	decoded, bodyOK := usersDecodedBody(c)
	if !bodyOK {
		return
	}
	email, emailPresent, password, emailCode, avatarStyle, validationMessage := usersValidatePatchBody(decoded)
	if validationMessage != "" {
		respondAJVValidation(c, validationMessage)
		return
	}

	userID, ok := lib.ParsePositiveRouteID(c.Param("userId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	isSelf := int64(claims.ID) == userID
	// 只能修改自己或管理员可修改任意用户
	if claims.Role != "admin" && !isSelf {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, "")
		return
	}

	user, err := db.FindUserById(ctx, int32(userID))
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	if password != nil && usersShouldProtectDemoAccount(c, user) {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, lib.DemoAccountProtectedMessage)
		return
	}

	shouldClaimPasswordVerification := isSelf && password != nil
	emailDiffers := emailPresent && !usersEmailEquals(email, user.Email)
	shouldClaimEmailVerification := isSelf && emailDiffers && user.Email != nil

	// 敏感操作二次验证：修改密码或邮箱时需要验证（管理员修改他人时跳过）
	if isSelf {
		if password != nil {
			// 验证当前密码（防止会话劫持）
			currentPassword, _ := usersRawBodyField(decoded, "currentPassword")
			if currentPassword == "" {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Current password is required")
				return
			}
			if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(currentPassword)) != nil {
				lib.RespondAPIError(c, http.StatusUnauthorized, lib.ErrInvalidCredentials, "Current password is incorrect")
				return
			}

			verified, err := usersOperationVerified(ctx, int32(userID), models.OperationTypeChangePassword)
			if err != nil {
				usersRespondInternal(c)
				return
			}
			if !verified {
				usersRespondVerificationRequired(c, models.OperationTypeChangePassword)
				return
			}
		}

		if emailDiffers {
			if user.Email != nil {
				verified, err := usersOperationVerified(ctx, int32(userID), models.OperationTypeChangeEmail)
				if err != nil {
					usersRespondInternal(c)
					return
				}
				if !verified {
					usersRespondVerificationRequired(c, models.OperationTypeChangeEmail)
					return
				}
			}
			if emailCode == nil || *emailCode == "" {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrEmailCodeRequired, "")
				return
			}
		}
	}

	updates := db.UserUpdate{}

	if emailPresent {
		normalizedEmail, validationErr, err := usersValidateEmailChangeTarget(ctx, email, user.Email, user.ID)
		if err != nil {
			usersRespondInternal(c)
			return
		}
		if validationErr != nil {
			lib.RespondAPIError(c, http.StatusBadRequest, validationErr.Code, validationErr.Details)
			return
		}

		if isSelf && emailDiffers {
			codeValue := ""
			if emailCode != nil {
				codeValue = *emailCode
			}
			verified, err := usersVerifyCode(ctx, normalizedEmail, codeValue)
			if err != nil {
				usersRespondInternal(c)
				return
			}
			if !verified {
				lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidEmailCode, "")
				return
			}
		}

		updates.Email = &normalizedEmail
	}

	if password != nil {
		passwordCheck := lib.ValidatePassword(*password)
		if !passwordCheck.Valid {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPasswordTooWeak, passwordCheck.Message)
			return
		}

		hashed, err := bcrypt.GenerateFromPassword([]byte(*password), usersPasswordBcryptCost)
		if err != nil {
			usersRespondInternal(c)
			return
		}
		hashedValue := string(hashed)
		updates.PasswordHash = &hashedValue
	}

	if avatarStyle != nil {
		updates.AvatarStyle = avatarStyle
	}

	// The Prisma version only writes when at least one field changed
	// (`if (Object.keys(updates).length > 0)`).
	if updates.Email == nil && updates.PasswordHash == nil && updates.AvatarStyle == nil {
		c.JSON(http.StatusOK, gin.H{
			"message":        "User info updated",
			"reauthRequired": password != nil && isSelf,
		})
		return
	}

	if shouldClaimPasswordVerification {
		verification, err := usersClaimOperationVerification(ctx, int32(userID), models.OperationTypeChangePassword)
		if err != nil {
			usersRespondInternal(c)
			return
		}
		if !verification.Verified {
			usersRespondVerificationRequired(c, models.OperationTypeChangePassword)
			return
		}
	}
	if shouldClaimEmailVerification {
		verification, err := usersClaimOperationVerification(ctx, int32(userID), models.OperationTypeChangeEmail)
		if err != nil {
			usersRespondInternal(c)
			return
		}
		if !verification.Verified {
			usersRespondVerificationRequired(c, models.OperationTypeChangeEmail)
			return
		}
	}

	if err := db.UpdateUser(ctx, user.ID, updates); err != nil {
		usersRespondInternal(c)
		return
	}

	if password != nil {
		_, _ = lib.RevokeAllUserRefreshTokens(ctx, user.ID)
		lib.InvalidateUserAccessTokens(ctx, user.ID)
		UsersCloseUserSessions(user.ID, "Password updated")
	}

	updateActions := make([]string, 0, 3)
	if emailPresent {
		updateActions = append(updateActions, "email")
	}
	if password != nil {
		updateActions = append(updateActions, "password")
	}
	if avatarStyle != nil {
		updateActions = append(updateActions, "avatarStyle")
	}

	prefix := ""
	if claims.Role == "admin" && !isSelf {
		prefix = `Admin updated user "` + user.Username + `":`
	}
	db.CreateLog(ctx, usersInt64Ptr(int64(claims.ID)), "personal", "profile.update",
		prefix+" "+strings.Join(updateActions, ", "), db.LogResultSuccess, db.CreateLogOptions{})

	if claims.Role == "admin" && !isSelf {
		lib.LogAdminAction(ctx, int64(claims.ID), "user.profile.update", lib.AuditLogDetails{
			IP:             middleware.ClientIP(c),
			UserAgent:      c.GetHeader("User-Agent"),
			TargetUserID:   usersInt32Ptr(user.ID),
			TargetUsername: user.Username,
			ResourceType:   "user",
			Metadata:       map[string]any{"fields": updateActions},
			Reason:         "Admin updated user profile fields: " + strings.Join(updateActions, ", "),
		})
	}

	if isSelf && password != nil {
		lib.SendNotification(ctx, int64(user.ID), lib.EventPasswordChanged, lib.EventData{"instanceName": ""})
	}

	c.JSON(http.StatusOK, gin.H{
		"message":        "User info updated",
		"reauthRequired": password != nil && isSelf,
	})
}

// usersEmailEquals mirrors `email !== user.email` for the coerced string and the
// nullable stored email.
func usersEmailEquals(email string, stored *string) bool {
	if stored == nil {
		return false
	}
	return email == *stored
}

// usersRawBodyField reads a property that is NOT part of the route schema
// (`currentPassword`), which Fastify leaves in place because `additionalProperties`
// is unset. Non-string values are `undefined` in the TypeScript.
func usersRawBodyField(decoded any, name string) (string, bool) {
	object, ok := decoded.(map[string]any)
	if !ok {
		return "", false
	}
	value, present := object[name]
	if !present {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	return text, true
}

func usersInt64Ptr(value int64) *int64 { return &value }

func usersInt32Ptr(value int32) *int32 { return &value }

// ==================== PATCH /api/users/:id/role ====================

// usersUpdateRole mirrors PATCH /:id/role.
func usersUpdateRole(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	// Body schema validation precedes the handler (Fastify preValidation).
	decoded, bodyOK := usersDecodedBody(c)
	if !bodyOK {
		return
	}
	validated, message := validateAJVObject(decoded, []string{"role"}, []ajvFieldSpec{
		{Name: "role", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"admin", "user"}},
	})
	if message != "" {
		respondAJVValidation(c, message)
		return
	}
	roleValue, _ := validated.stringValue("role")

	userID, ok := lib.ParsePositiveRouteID(c.Param("userId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	if int64(claims.ID) == userID {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrCannotModifySelf, "")
		return
	}

	result, err := usersUpdateUserRole(ctx, int32(userID), models.UserRole(roleValue))
	if err != nil {
		switch {
		case errors.Is(err, errUsersRoleUserMissing):
			lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		case errors.Is(err, errUsersRoleNotActive):
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Only active users can be promoted to admin")
		case errors.Is(err, errUsersRoleLastAdmin):
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Cannot remove the last active admin")
		case errors.Is(err, errUsersRoleLockBusy):
			lib.RespondAPIError(c, http.StatusConflict, lib.ErrInvalidParams, "Another admin role update is in progress, please retry")
		default:
			usersRespondInternal(c)
		}
		return
	}

	if !result.Changed {
		c.JSON(http.StatusOK, gin.H{
			"message": "User role unchanged",
			"role":    result.Role,
		})
		return
	}

	if closedTerminals := UsersCloseUserSessions(int32(userID), "User role changed by admin"); closedTerminals > 0 {
		log.Printf("[Role] Closed %d terminal session(s) for user %d", closedTerminals, userID)
	}

	db.CreateLog(ctx, usersInt64Ptr(int64(claims.ID)), "user", "user.role.update",
		`Changed user "`+result.Username+`" (ID: `+strconv.FormatInt(userID, 10)+`) role from `+
			string(result.PreviousRole)+" to "+string(result.Role),
		db.LogResultSuccess, db.CreateLogOptions{})

	lib.LogAdminAction(ctx, int64(claims.ID), "user.role.update", lib.AuditLogDetails{
		IP:             middleware.ClientIP(c),
		UserAgent:      c.GetHeader("User-Agent"),
		TargetUserID:   usersInt32Ptr(int32(userID)),
		TargetUsername: result.Username,
		ResourceType:   "user",
		Metadata: map[string]any{
			"previousRole":    string(result.PreviousRole),
			"nextRole":        string(result.Role),
			"revokedSessions": result.RevokedSessions,
		},
		Reason: "Admin changed user role from " + string(result.PreviousRole) + " to " + string(result.Role),
	})

	c.JSON(http.StatusOK, gin.H{
		"message":         "User role updated",
		"role":            result.Role,
		"revokedSessions": result.RevokedSessions,
	})
}

// ==================== POST /api/users/:id/quota/recalculate ====================

// usersRecalculateQuota mirrors POST /:id/quota/recalculate.
func usersRecalculateQuota(c *gin.Context) {
	ctx := c.Request.Context()

	userID, ok := lib.ParsePositiveRouteID(c.Param("userId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	user, err := db.FindUserById(ctx, int32(userID))
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	actualUsed, err := db.CalculateUserQuotaUsed(ctx, user.ID)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if err := db.UpdateUserQuotaUsed(ctx, user.ID, actualUsed); err != nil {
		usersRespondInternal(c)
		return
	}

	hostUsed := int32(0)
	friendUsed := int32(0)
	packageUsed := int32(0)
	if actualUsed.HostUsed != nil {
		hostUsed = *actualUsed.HostUsed
	}
	if actualUsed.FriendUsed != nil {
		friendUsed = *actualUsed.FriendUsed
	}
	if actualUsed.PackageUsed != nil {
		packageUsed = *actualUsed.PackageUsed
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Quota recalculated",
		"quota": gin.H{
			"hostUsed":    hostUsed,
			"friendUsed":  friendUsed,
			"packageUsed": packageUsed,
		},
	})
}

// ==================== POST /api/users/me/quota/increase ====================

// usersIncreaseQuota mirrors POST /me/quota/increase.
func usersIncreaseQuota(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	decoded, bodyOK := usersDecodedBody(c)
	if !bodyOK {
		return
	}
	validated, message := validateAJVObject(decoded, nil, []ajvFieldSpec{
		{Name: "hostLimit", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: math.NaN()},
		{Name: "friendLimit", Kind: ajvKindInteger, MinLength: -1, MaxLength: -1, Minimum: 0, Maximum: math.NaN()},
	})
	if message != "" {
		respondAJVValidation(c, message)
		return
	}

	_, hostPresent := validated.Present["hostLimit"]
	_, friendPresent := validated.Present["friendLimit"]

	providedTypes := 0
	if hostPresent {
		providedTypes++
	}
	if friendPresent {
		providedTypes++
	}
	if providedTypes != 1 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaIncreaseTypeRequired, "")
		return
	}

	currentQuota, err := db.GetUserQuota(ctx, claims.ID)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if currentQuota == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrQuotaInfoNotFound, "")
		return
	}

	quotaType := ""
	increaseAmount := int32(0)
	currentUsed := int32(0)
	currentLimit := int32(0)
	expectedIncrease := int32(0)

	if hostPresent {
		hostLimit, _ := validated.nullableInt32("hostLimit")
		if hostLimit == nil {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaIncreaseStepInvalid, "")
			return
		}
		quotaType = "host"
		currentUsed = currentQuota.HostUsed
		currentLimit = currentQuota.HostLimit
		increaseAmount = *hostLimit - currentLimit
		expectedIncrease = 5
	} else {
		friendLimit, _ := validated.nullableInt32("friendLimit")
		if friendLimit == nil {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaIncreaseStepInvalid, "")
			return
		}
		quotaType = "friend"
		currentUsed = currentQuota.FriendUsed
		currentLimit = currentQuota.FriendLimit
		increaseAmount = *friendLimit - currentLimit
		expectedIncrease = 10
	}

	if increaseAmount != expectedIncrease {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaIncreaseStepInvalid, "")
		return
	}

	if currentLimit == 0 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaUsageUnavailable, "")
		return
	}

	usagePercent := (float64(currentUsed) / float64(currentLimit)) * 100
	if usagePercent < 50 {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrQuotaUsageTooLow, "")
		return
	}

	limitedValue := currentLimit + expectedIncrease
	if quotaType == "host" {
		if err := db.UpdateUserQuotaLimits(ctx, claims.ID, db.UserQuotaUpdate{HostLimit: &limitedValue}); err != nil {
			usersRespondInternal(c)
			return
		}
	} else {
		if err := db.UpdateUserQuotaLimits(ctx, claims.ID, db.UserQuotaUpdate{FriendLimit: &limitedValue}); err != nil {
			usersRespondInternal(c)
			return
		}
	}

	typeNames := map[string]string{"host": "宿主机", "friend": "好友"}
	newLimit := currentLimit + expectedIncrease
	db.CreateLog(ctx, usersInt64Ptr(int64(claims.ID)), "personal", "quota.increase",
		"用户自行增加"+typeNames[quotaType]+"配额："+strconv.FormatInt(int64(currentLimit), 10)+
			" → "+strconv.FormatInt(int64(newLimit), 10),
		db.LogResultSuccess, db.CreateLogOptions{})

	c.JSON(http.StatusOK, gin.H{"message": "QUOTA_INCREASED"})
}

// ==================== PATCH /api/users/:id/status ====================

// usersUpdateStatus mirrors PATCH /:id/status (ban / unban).
func usersUpdateStatus(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	// Body schema validation precedes the handler (Fastify preValidation).
	decoded, bodyOK := usersDecodedBody(c)
	if !bodyOK {
		return
	}
	validated, message := validateAJVObject(decoded, []string{"status"}, []ajvFieldSpec{
		{Name: "status", Kind: ajvKindString, MinLength: -1, MaxLength: -1, Enum: []string{"active", "banned"}},
		{Name: "reason", Kind: ajvKindString, MinLength: -1, MaxLength: 500},
	})
	if message != "" {
		respondAJVValidation(c, message)
		return
	}
	statusValue, _ := validated.stringValue("status")
	reason, reasonPresent := validated.stringValue("reason")

	userID, ok := lib.ParsePositiveRouteID(c.Param("userId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	// 不能封禁自己
	if int64(claims.ID) == userID {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrCannotModifySelf, "")
		return
	}

	user, err := db.FindUserById(ctx, int32(userID))
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	// 不能封禁管理员
	if user.Role == models.UserRoleAdmin && statusValue == "banned" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrCannotBanAdmin, "")
		return
	}

	var banReason *string
	if reasonPresent && reason != "" {
		banReason = &reason
	}
	if err := db.UpdateUserStatus(ctx, user.ID, models.UserStatus(statusValue), banReason); err != nil {
		usersRespondInternal(c)
		return
	}

	// 封禁时必须同时失效该用户的全部令牌与终端会话（route 层，与 TypeScript 一致）。
	if statusValue == "banned" {
		_, _ = lib.RevokeAllUserRefreshTokens(ctx, user.ID)
		lib.InvalidateUserAccessTokens(ctx, user.ID)
		if closedSessions := UsersCloseUserSessions(user.ID, "User account banned"); closedSessions > 0 {
			log.Printf("[Ban] Closed %d terminal session(s) for user %d", closedSessions, user.ID)
		}
	}

	action := "user.unban"
	if statusValue == "banned" {
		action = "user.ban"
	}
	content := "Unbanned"
	if statusValue == "banned" {
		content = "Banned"
	}
	content += ` user "` + user.Username + `" (ID: ` + strconv.FormatInt(userID, 10) + ")"
	if reasonPresent && reason != "" {
		content += " - Reason: " + reason
	}
	db.CreateLog(ctx, usersInt64Ptr(int64(claims.ID)), "user", action, content, db.LogResultSuccess, db.CreateLogOptions{})

	adminReason := "Admin unbanned user"
	if statusValue == "banned" {
		adminReason = "Admin banned user"
		if reasonPresent && reason != "" {
			adminReason = reason
		}
	}
	lib.LogAdminAction(ctx, int64(claims.ID), action, lib.AuditLogDetails{
		IP:             middleware.ClientIP(c),
		UserAgent:      c.GetHeader("User-Agent"),
		TargetUserID:   usersInt32Ptr(user.ID),
		TargetUsername: user.Username,
		ResourceType:   "user",
		Reason:         adminReason,
	})

	// 封禁邮件 fire-and-forget: the TypeScript does not await it and logs failures.
	if statusValue == "banned" && reasonPresent && reason != "" && user.Email != nil {
		email := *user.Email
		username := user.Username
		banReasonValue := reason
		go func() {
			result := lib.SendBanNotificationEmail(context.Background(), email, lib.BanNotificationEmailData{
				Username: username,
				Reason:   banReasonValue,
			})
			if !result.Success {
				log.Printf("[Ban] Failed to send ban notification email: %s", result.Error)
			}
		}()
	}

	response := "User unbanned"
	if statusValue == "banned" {
		response = "User banned"
	}
	c.JSON(http.StatusOK, gin.H{"message": response})
}

// ==================== DELETE /api/users/:id ====================

// usersDelete mirrors DELETE /:id.
func usersDelete(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	userID, ok := lib.ParsePositiveRouteID(c.Param("userId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	// 不能删除自己
	if int64(claims.ID) == userID {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrCannotDeleteSelf, "")
		return
	}

	user, err := db.FindUserById(ctx, int32(userID))
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	// 不能删除管理员
	if user.Role == models.UserRoleAdmin {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrCannotDeleteAdmin, "")
		return
	}

	blockers, err := db.GetUserDeletionBlockers(ctx, user.ID)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if blockers.HasBlockers() {
		if blockers.BalanceNonZero {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrUserHasResources, "用户主余额非零，请先清零或结算")
			return
		}
		if blockers.BalanceLogs > 0 || blockers.RechargeRecords > 0 || blockers.LoginRecords > 0 || blockers.RedeemCodeUsages > 0 {
			lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrUserHasResources, "该用户有资金/审计流水，不能直接删除")
			return
		}
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrUserHasResources,
			"instances="+strconv.FormatInt(blockers.Instances, 10)+
				", hosts="+strconv.FormatInt(blockers.Hosts, 10)+
				", packages="+strconv.FormatInt(blockers.Packages, 10))
		return
	}

	if err := db.DeleteUser(ctx, user.ID); err != nil {
		if errors.Is(err, db.ErrUserDeletionConflict) {
			lib.RespondAPIError(c, http.StatusConflict, lib.ErrUserHasResources, "该用户存在关联记录，无法删除")
			return
		}
		usersRespondInternal(c)
		return
	}

	db.CreateLog(ctx, usersInt64Ptr(int64(claims.ID)), "user", "user.delete",
		`Deleted user "`+user.Username+`" (ID: `+strconv.FormatInt(userID, 10)+", role: "+string(user.Role)+")",
		db.LogResultSuccess, db.CreateLogOptions{})

	lib.LogAdminAction(ctx, int64(claims.ID), "user.delete", lib.AuditLogDetails{
		IP:             middleware.ClientIP(c),
		UserAgent:      c.GetHeader("User-Agent"),
		TargetUserID:   usersInt32Ptr(user.ID),
		TargetUsername: user.Username,
		ResourceType:   "user",
		Reason:         "Admin deleted user account",
	})

	c.JSON(http.StatusOK, gin.H{"message": "User deleted"})
}

// ==================== GET /api/users/:id/sessions ====================

// usersSessions mirrors GET /:id/sessions.
func usersSessions(c *gin.Context) {
	ctx := c.Request.Context()

	userID, ok := lib.ParsePositiveRouteID(c.Param("userId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	user, err := db.FindUserById(ctx, int32(userID))
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	sessions, err := lib.GetUserSessions(ctx, user.ID, "")
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if sessions == nil {
		sessions = []lib.SessionInfo{}
	}
	c.JSON(http.StatusOK, gin.H{"sessions": sessions})
}

// ==================== POST /api/users/:id/revoke-sessions ====================

// usersRevokeSessions mirrors POST /:id/revoke-sessions.
func usersRevokeSessions(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	userID, ok := lib.ParsePositiveRouteID(c.Param("userId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	user, err := db.FindUserById(ctx, int32(userID))
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	count, err := lib.RevokeAllUserRefreshTokens(ctx, user.ID)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	lib.InvalidateUserAccessTokens(ctx, user.ID)

	if closedTerminals := UsersCloseUserSessions(user.ID, "Sessions revoked by admin"); closedTerminals > 0 {
		log.Printf("[RevokeSession] Closed %d terminal session(s) for user %d", closedTerminals, user.ID)
	}

	db.CreateLog(ctx, usersInt64Ptr(int64(claims.ID)), "user", "user.revoke_sessions",
		"Revoked "+strconv.FormatInt(count, 10)+` sessions for user "`+user.Username+`" (ID: `+strconv.FormatInt(userID, 10)+")",
		db.LogResultSuccess, db.CreateLogOptions{})

	lib.LogAdminAction(ctx, int64(claims.ID), "user.revoke_sessions", lib.AuditLogDetails{
		IP:             middleware.ClientIP(c),
		UserAgent:      c.GetHeader("User-Agent"),
		TargetUserID:   usersInt32Ptr(user.ID),
		TargetUsername: user.Username,
		ResourceType:   "session",
		Metadata:       map[string]any{"revokedCount": count},
	})

	c.JSON(http.StatusOK, gin.H{"message": "SESSIONS_REVOKED", "revokedCount": count})
}

// ==================== POST /api/users/:id/reset-password ====================

// usersResetPassword mirrors POST /:id/reset-password (admin sets the password;
// plaintext is never echoed).
func usersResetPassword(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	// Body schema validation precedes the handler (Fastify preValidation).
	decoded, bodyOK := usersDecodedBody(c)
	if !bodyOK {
		return
	}
	validated, message := validateAJVObject(decoded, []string{"password"}, []ajvFieldSpec{
		{Name: "password", Kind: ajvKindString, MinLength: 6, MaxLength: -1},
	})
	if message != "" {
		respondAJVValidation(c, usersAJVMessage(message))
		return
	}
	password, _ := validated.stringValue("password")

	userID, ok := lib.ParsePositiveRouteID(c.Param("userId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	user, err := db.FindUserById(ctx, int32(userID))
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	if int64(claims.ID) == userID {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrCannotModifySelf, "")
		return
	}

	if usersShouldProtectDemoAccount(c, user) {
		lib.RespondAPIError(c, http.StatusForbidden, lib.ErrForbidden, lib.DemoAccountProtectedMessage)
		return
	}

	passwordCheck := lib.ValidatePassword(password)
	if !passwordCheck.Valid {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrPasswordTooWeak, passwordCheck.Message)
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(password), usersPasswordBcryptCost)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	hashedValue := string(hashed)
	if err := db.UpdateUser(ctx, user.ID, db.UserUpdate{PasswordHash: &hashedValue}); err != nil {
		usersRespondInternal(c)
		return
	}

	// 撤销用户所有会话，强制重新登录
	_, _ = lib.RevokeAllUserRefreshTokens(ctx, user.ID)
	lib.InvalidateUserAccessTokens(ctx, user.ID)
	UsersCloseUserSessions(user.ID, "Password reset by admin")

	db.CreateLog(ctx, usersInt64Ptr(int64(claims.ID)), "user", "user.reset_password",
		`Admin reset password for user "`+user.Username+`" (ID: `+strconv.FormatInt(userID, 10)+")",
		db.LogResultSuccess, db.CreateLogOptions{})

	lib.LogAdminAction(ctx, int64(claims.ID), "user.reset_password", lib.AuditLogDetails{
		IP:             middleware.ClientIP(c),
		UserAgent:      c.GetHeader("User-Agent"),
		TargetUserID:   usersInt32Ptr(user.ID),
		TargetUsername: user.Username,
		ResourceType:   "credential",
		Reason:         "Admin initiated password reset",
	})

	c.JSON(http.StatusOK, gin.H{
		"message":  "Password reset successfully",
		"username": user.Username,
	})
}

// ==================== POST /api/users/:id/disable-2fa ====================

// usersDisable2FA mirrors POST /:id/disable-2fa.
func usersDisable2FA(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	userID, ok := lib.ParsePositiveRouteID(c.Param("userId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	user, err := db.FindUserById(ctx, int32(userID))
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	if int64(claims.ID) == userID {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrCannotModifySelf, "")
		return
	}

	enabled, err := db.Is2FAEnabled(ctx, user.ID)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if !enabled {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrTwoFaNotEnabled, "")
		return
	}

	// 禁用 2FA（清除所有 2FA 数据）
	if err := db.Disable2FAComplete(ctx, user.ID); err != nil {
		usersRespondInternal(c)
		return
	}

	_, _ = lib.RevokeAllUserRefreshTokens(ctx, user.ID)
	lib.InvalidateUserAccessTokens(ctx, user.ID)
	UsersCloseUserSessions(user.ID, "2FA disabled by admin")

	db.CreateLog(ctx, usersInt64Ptr(int64(claims.ID)), "user", "user.disable_2fa",
		`Admin disabled 2FA for user "`+user.Username+`" (ID: `+strconv.FormatInt(userID, 10)+")",
		db.LogResultSuccess, db.CreateLogOptions{})

	lib.LogAdminAction(ctx, int64(claims.ID), "user.disable_2fa", lib.AuditLogDetails{
		IP:             middleware.ClientIP(c),
		UserAgent:      c.GetHeader("User-Agent"),
		TargetUserID:   usersInt32Ptr(user.ID),
		TargetUsername: user.Username,
		ResourceType:   "2fa",
		Reason:         "Admin disabled user 2FA",
	})

	c.JSON(http.StatusOK, gin.H{"message": "2FA disabled successfully"})
}

// ==================== DELETE /api/users/:id/oauth/:provider ====================

// usersUnbindOAuth mirrors DELETE /:id/oauth/:provider.
func usersUnbindOAuth(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	userID, ok := lib.ParsePositiveRouteID(c.Param("userId"))
	provider := c.Param("provider")

	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	// 验证 provider 参数
	if provider != "github" && provider != "google" {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "Invalid OAuth provider")
		return
	}

	user, err := db.FindUserById(ctx, int32(userID))
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	if int64(claims.ID) == userID {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrCannotModifySelf, "")
		return
	}

	hasBound, err := db.HasOAuthBinding(ctx, user.ID, provider)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if !hasBound {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidParams, "User has not bound this OAuth provider")
		return
	}

	if err := db.DeleteOAuthBinding(ctx, user.ID, provider); err != nil {
		usersRespondInternal(c)
		return
	}

	_, _ = lib.RevokeAllUserRefreshTokens(ctx, user.ID)
	lib.InvalidateUserAccessTokens(ctx, user.ID)
	UsersCloseUserSessions(user.ID, provider+" OAuth unbound by admin")

	db.CreateLog(ctx, usersInt64Ptr(int64(claims.ID)), "user", "user.unbind_oauth",
		"Admin unbound "+provider+` OAuth for user "`+user.Username+`" (ID: `+strconv.FormatInt(userID, 10)+")",
		db.LogResultSuccess, db.CreateLogOptions{})

	lib.LogAdminAction(ctx, int64(claims.ID), "user.unbind_oauth", lib.AuditLogDetails{
		IP:             middleware.ClientIP(c),
		UserAgent:      c.GetHeader("User-Agent"),
		TargetUserID:   usersInt32Ptr(user.ID),
		TargetUsername: user.Username,
		ResourceType:   "oauth",
		Metadata:       map[string]any{"provider": provider},
		Reason:         "Admin unbound " + provider + " OAuth",
	})

	c.JSON(http.StatusOK, gin.H{"message": provider + " OAuth binding removed successfully"})
}

// ==================== GET /api/users/:id/login-records ====================

// usersLoginRecordView mirrors the mapped record of GET /:id/login-records.
type usersLoginRecordView struct {
	ID        int32   `json:"id"`
	IP        string  `json:"ip"`
	Country   *string `json:"country"`
	Region    *string `json:"region"`
	City      *string `json:"city"`
	Isp       *string `json:"isp"`
	Timezone  *string `json:"timezone"`
	UserAgent *string `json:"userAgent"`
	CreatedAt string  `json:"createdAt"`
}

// usersLoginRecords mirrors GET /:id/login-records.
func usersLoginRecords(c *gin.Context) {
	ctx := c.Request.Context()

	userID, ok := lib.ParsePositiveRouteID(c.Param("userId"))
	if !ok {
		lib.RespondAPIError(c, http.StatusBadRequest, lib.ErrInvalidId, "")
		return
	}

	user, err := db.FindUserById(ctx, int32(userID))
	if err != nil {
		usersRespondInternal(c)
		return
	}
	if user == nil {
		lib.RespondAPIError(c, http.StatusNotFound, lib.ErrUserNotFound, "")
		return
	}

	pageRaw := ""
	if value := usersQueryValue(c, "page"); value != nil {
		pageRaw = *value
	}
	pageSizeRaw := ""
	if value := usersQueryValue(c, "pageSize"); value != nil {
		pageSizeRaw = *value
	}
	page := usersParsePositiveIntegerQuery(pageRaw, 1)
	pageSize := usersParseClampedPositiveIntegerQuery(pageSizeRaw, 20, 50)

	records, err := db.GetUserLoginRecords(ctx, user.ID, page, pageSize)
	if err != nil {
		usersRespondInternal(c)
		return
	}

	shouldRedact := usersShouldRedactDemoLoginRecords(c, user)
	views := make([]usersLoginRecordView, 0, len(records.Records))
	for _, record := range records.Records {
		if shouldRedact {
			record = usersRedactDemoLoginRecord(record)
		}
		views = append(views, usersLoginRecordView{
			ID:        record.ID,
			IP:        record.IP,
			Country:   record.Country,
			Region:    record.Region,
			City:      record.City,
			Isp:       record.Isp,
			Timezone:  record.Timezone,
			UserAgent: record.UserAgent,
			CreatedAt: usersISOTime(record.CreatedAt),
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"records":    views,
		"total":      records.Total,
		"page":       records.Page,
		"pageSize":   records.PageSize,
		"totalPages": records.TotalPages,
	})
}

// ==================== GET /api/users/detect-linked-accounts ====================

// usersLinkedAccountUserView mirrors one user inside a linked-account group.
type usersLinkedAccountUserView struct {
	ID         int32   `json:"id"`
	Username   string  `json:"username"`
	Email      *string `json:"email"`
	Status     string  `json:"status"`
	LoginCount int64   `json:"loginCount,omitempty"`
	LastLogin  string  `json:"lastLogin,omitempty"`
	CreatedAt  string  `json:"createdAt,omitempty"`
}

// usersIPGroupView mirrors an ipGroups element.
type usersIPGroupView struct {
	IP          string                       `json:"ip"`
	UserCount   int                          `json:"userCount"`
	TotalLogins int64                        `json:"totalLogins"`
	Users       []usersLinkedAccountUserView `json:"users"`
}

// usersPatternGroupView mirrors emailGroups / usernameGroups elements.
type usersPatternGroupView struct {
	Pattern   string                       `json:"pattern"`
	UserCount int                          `json:"userCount"`
	Users     []usersLinkedAccountUserView `json:"users"`
}

// usersDetectLinkedAccounts mirrors GET /detect-linked-accounts.
func usersDetectLinkedAccounts(c *gin.Context) {
	ctx := c.Request.Context()
	claims := middleware.CurrentUser(c)

	startTime := time.Now()

	daysRaw := ""
	if value := usersQueryValue(c, "days"); value != nil {
		daysRaw = *value
	}
	limitRaw := ""
	if value := usersQueryValue(c, "limit"); value != nil {
		limitRaw = *value
	}
	// 参数校验：限制检测范围在 1-365 天之间，防止查询性能问题
	days := usersParseClampedPositiveIntegerQuery(daysRaw, 90, 365)
	limit := usersParseClampedPositiveIntegerQuery(limitRaw, 50, 100)

	ipGroups, err := db.GetSharedIPGroups(ctx, days, 2, limit, usersMaxDetectedUsersPerGroup)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	emailGroups, err := db.GetSimilarEmailGroups(ctx, limit, usersMaxDetectedUsersPerGroup)
	if err != nil {
		usersRespondInternal(c)
		return
	}
	usernameGroups, err := db.GetSimilarUsernameGroups(ctx, limit, usersMaxDetectedUsersPerGroup)
	if err != nil {
		usersRespondInternal(c)
		return
	}

	durationMs := time.Since(startTime).Milliseconds()

	db.CreateLog(ctx, usersInt64Ptr(int64(claims.ID)), "user", "admin.detect_linked_accounts",
		"Admin detected linked accounts: "+strconv.Itoa(len(ipGroups))+" IP groups, "+
			strconv.Itoa(len(emailGroups))+" email groups, "+strconv.Itoa(len(usernameGroups))+
			" username groups, limit "+strconv.Itoa(limit)+", max users per group "+
			strconv.Itoa(usersMaxDetectedUsersPerGroup)+" ("+strconv.FormatInt(durationMs, 10)+"ms)",
		db.LogResultSuccess, db.CreateLogOptions{})

	ipViews := make([]usersIPGroupView, 0, len(ipGroups))
	for _, group := range ipGroups {
		users := make([]usersLinkedAccountUserView, 0, len(group.Users))
		for _, user := range group.Users {
			users = append(users, usersLinkedAccountUserView{
				ID:         user.ID,
				Username:   user.Username,
				Email:      user.Email,
				Status:     user.Status,
				LoginCount: user.LoginCount,
				LastLogin:  usersISOTime(user.LastLogin),
			})
		}
		ipViews = append(ipViews, usersIPGroupView{
			IP:          group.IP,
			UserCount:   len(group.Users),
			TotalLogins: group.TotalLogins,
			Users:       users,
		})
	}

	emailViews := make([]usersPatternGroupView, 0, len(emailGroups))
	for _, group := range emailGroups {
		users := make([]usersLinkedAccountUserView, 0, len(group.Users))
		for _, user := range group.Users {
			users = append(users, usersLinkedAccountUserView{
				ID:        user.ID,
				Username:  user.Username,
				Email:     user.Email,
				Status:    user.Status,
				CreatedAt: usersISOTime(user.CreatedAt),
			})
		}
		emailViews = append(emailViews, usersPatternGroupView{Pattern: group.Pattern, UserCount: len(group.Users), Users: users})
	}

	usernameViews := make([]usersPatternGroupView, 0, len(usernameGroups))
	for _, group := range usernameGroups {
		users := make([]usersLinkedAccountUserView, 0, len(group.Users))
		for _, user := range group.Users {
			users = append(users, usersLinkedAccountUserView{
				ID:        user.ID,
				Username:  user.Username,
				Email:     user.Email,
				Status:    user.Status,
				CreatedAt: usersISOTime(user.CreatedAt),
			})
		}
		usernameViews = append(usernameViews, usersPatternGroupView{Pattern: group.Pattern, UserCount: len(group.Users), Users: users})
	}

	c.JSON(http.StatusOK, gin.H{
		"detectedAt":       usersISOTime(time.Now()),
		"durationMs":       durationMs,
		"days":             days,
		"limit":            limit,
		"maxUsersPerGroup": usersMaxDetectedUsersPerGroup,
		"summary": gin.H{
			"ipGroups":       len(ipGroups),
			"emailGroups":    len(emailGroups),
			"usernameGroups": len(usernameGroups),
		},
		"ipGroups":       ipViews,
		"emailGroups":    emailViews,
		"usernameGroups": usernameViews,
	})
}

func init() {
	Register(RegisterUsers)
}
