// Port of server/src/lib/operation-verification.ts.
//
// Sensitive-operation second-factor verification: account operations are verified
// by email, resource operations by the user's bound notification channel.
//
// Single-instance / concurrency note (AGENTS.md): unlike security.ts's login
// lock, the anti-replay guarantee here is *not* held in process memory. It is
// enforced by PostgreSQL through atomic conditional statements whose affected-row
// count is checked:
//   - verifyOperationCode            -> UPDATE ... WHERE verified = false AND ... (claim once)
//   - claimOperationVerificationRequirement -> DELETE ... WHERE verified = true (claim once)
//
// Request creation is additionally serialized per (userId, operationType,
// resourceId) by a transaction-scoped advisory lock. The Go port keeps exactly
// the same DB-atomic semantics, so no new shared in-process state is introduced
// and the single-instance invariant is preserved.
package lib

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// Verification-code configuration, mirroring VERIFICATION_CONFIG.
const (
	operationVerificationCodeLength        = 6
	operationVerificationExpiresInMinutes  = 10
	operationVerificationMaxAttempts       = 5
	operationVerificationAlreadySentWindow = 2 * time.Minute
	// A successful verification stays claimable for 10 minutes.
	operationVerificationValidWindow = 10 * time.Minute
	// operationVerificationRequestLockNamespace mirrors
	// OPERATION_VERIFICATION_REQUEST_LOCK_NAMESPACE from db/advisory-locks.ts.
	operationVerificationRequestLockNamespace = 4118
)

// Error codes surfaced through the result envelopes. These are returned verbatim
// by routes (see routes/verification.ts) rather than through lib.ErrXxx, so they
// stay plain strings to keep byte-compatible responses.
const (
	OperationVerificationErrorUserNotFound          = "USER_NOT_FOUND"
	OperationVerificationErrorEmailNotConfigured    = "EMAIL_NOT_CONFIGURED"
	OperationVerificationErrorNoNotificationChannel = "NO_NOTIFICATION_CHANNEL"
	OperationVerificationErrorSendFailed            = "SEND_FAILED"
	OperationVerificationErrorTooManyAttempts       = "TOO_MANY_ATTEMPTS"
	OperationVerificationErrorInvalidCode           = "INVALID_CODE"
)

var errOperationVerificationNoDatabase = errors.New("operation verification: database not initialized")

var accountOperations = map[models.OperationType]bool{
	models.OperationTypeChangePassword: true,
	models.OperationTypeDisable2fa:     true,
	models.OperationTypeChangeEmail:    true,
	models.OperationTypeDeleteAccount:  true,
}

var resourceOperations = map[models.OperationType]bool{
	models.OperationTypeDeleteInstance:    true,
	models.OperationTypeReinstallInstance: true,
	models.OperationTypeRecreateInstance:  true,
	models.OperationTypeTransferInstance:  true,
	models.OperationTypeDeleteSnapshot:    true,
	models.OperationTypeDeleteBackup:      true,
}

type operationName struct {
	zh string
	en string
}

// operationNames mirrors OPERATION_NAMES (display names used in notifications).
var operationNames = map[models.OperationType]operationName{
	models.OperationTypeChangePassword:    {zh: "修改密码", en: "Change Password"},
	models.OperationTypeDisable2fa:        {zh: "禁用双因素认证", en: "Disable 2FA"},
	models.OperationTypeChangeEmail:       {zh: "修改邮箱地址", en: "Change Email"},
	models.OperationTypeDeleteAccount:     {zh: "删除账户", en: "Delete Account"},
	models.OperationTypeDeleteInstance:    {zh: "删除实例", en: "Delete Instance"},
	models.OperationTypeReinstallInstance: {zh: "重装实例", en: "Reinstall Instance"},
	models.OperationTypeRecreateInstance:  {zh: "重建实例", en: "Recreate Instance"},
	models.OperationTypeTransferInstance:  {zh: "转移实例", en: "Transfer Instance"},
	models.OperationTypeDeleteSnapshot:    {zh: "删除快照", en: "Delete Snapshot"},
	models.OperationTypeDeleteBackup:      {zh: "删除备份", en: "Delete Backup"},
}

// RequestVerificationResult mirrors the Node RequestVerificationResult.
type RequestVerificationResult struct {
	Success      bool                       `json:"success"`
	Channel      models.VerificationChannel `json:"channel,omitempty"`
	MaskedTarget string                     `json:"maskedTarget,omitempty"`
	ExpiresIn    int64                      `json:"expiresIn,omitempty"`
	Error        string                     `json:"error,omitempty"`
	ErrorCode    string                     `json:"errorCode,omitempty"`
}

// VerifyOperationResult mirrors the Node VerifyOperationResult.
type VerifyOperationResult struct {
	Success   bool   `json:"success"`
	Verified  bool   `json:"verified"`
	Error     string `json:"error,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"`
}

// OperationVerificationRequirement mirrors the {required, verified} envelope.
type OperationVerificationRequirement struct {
	Required bool `json:"required"`
	Verified bool `json:"verified"`
}

// OperationVerificationSendResult mirrors the Node {success, error?} return used
// by mailer.sendOperationVerificationEmail / notifier.sendVerificationNotification.
type OperationVerificationSendResult struct {
	Success bool
	Error   string
}

// OperationVerificationEmailData is the payload for the email sender seam.
type OperationVerificationEmailData struct {
	Username         string
	OperationName    string
	Code             string
	ExpiresInMinutes int
}

// OperationVerificationNotificationData is the payload for the notification sender seam.
type OperationVerificationNotificationData struct {
	OperationName    string
	Code             string
	ExpiresInMinutes int
}

// OperationVerificationEmailSender is the seam for lib/mailer.go's
// sendOperationVerificationEmail (not yet ported to Go). Wire it from the app
// wiring once that module lands; while nil, requestOperationVerification reports
// SEND_FAILED instead of silently succeeding.
var OperationVerificationEmailSender func(email string, data OperationVerificationEmailData) OperationVerificationSendResult

// OperationVerificationNotificationSender is the seam for lib/notifier.go's
// sendVerificationNotification (not yet ported to Go). Same wiring contract as
// OperationVerificationEmailSender.
var OperationVerificationNotificationSender func(userID int64, data OperationVerificationNotificationData) OperationVerificationSendResult

// IsAccountOperation mirrors isAccountOperation.
func IsAccountOperation(operationType models.OperationType) bool {
	return accountOperations[operationType]
}

// IsResourceOperation mirrors isResourceOperation.
func IsResourceOperation(operationType models.OperationType) bool {
	return resourceOperations[operationType]
}

// GetOperationName mirrors getOperationName(operationType, lang='zh').
func GetOperationName(operationType models.OperationType, lang string) string {
	name, ok := operationNames[operationType]
	if !ok {
		return string(operationType)
	}

	var value string
	switch lang {
	case "zh":
		value = name.zh
	case "en":
		value = name.en
	}
	if value == "" {
		return string(operationType)
	}
	return value
}

// generateVerificationCode mirrors crypto.randomInt(100000, 1000000).toString().
func generateVerificationCode() (string, error) {
	offset, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(offset.Int64()+100000, 10), nil
}

// hashVerificationCode mirrors hashVerificationCode: "sha256:<hex>".
func hashVerificationCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// verificationCodeMatches mirrors verificationCodeMatches: hashed codes compare by
// hash, legacy plaintext codes compare verbatim.
func verificationCodeMatches(storedCode, inputCode string) bool {
	if strings.HasPrefix(storedCode, "sha256:") {
		return storedCode == hashVerificationCode(inputCode)
	}
	return storedCode == inputCode
}

// matchVerificationCandidate mirrors the Node `candidates.find(...)`.
func matchVerificationCandidate(candidates []models.OperationVerification, code, codeHash string) (int, bool) {
	for index, candidate := range candidates {
		if candidate.Code == codeHash || verificationCodeMatches(candidate.Code, code) {
			return index, true
		}
	}
	return 0, false
}

// maskEmail mirrors `${localPart.substring(0,2)}***@${domain}`. JavaScript leaves
// `domain` as the literal "undefined" when the address has no "@", which is kept
// here for byte-compatible display strings.
func maskEmail(email string) string {
	parts := strings.Split(email, "@")
	localPart := ""
	if len(parts) > 0 {
		localPart = parts[0]
	}
	domain := "undefined"
	if len(parts) > 1 {
		domain = parts[1]
	}

	runes := []rune(localPart)
	if len(runes) > 2 {
		runes = runes[:2]
	}
	return string(runes) + "***@" + domain
}

// getOperationVerificationRequestLockKey mirrors
// getOperationVerificationRequestLockKey. The Node `?? 'account'` fallback is
// reproduced with resourceID == 0 meaning "absent" (resource ids are >= 1 per the
// route schema, so 0 is unreachable in practice).
func getOperationVerificationRequestLockKey(userID int64, operationType models.OperationType, resourceID int64) string {
	resource := "account"
	if resourceID != 0 {
		resource = strconv.FormatInt(resourceID, 10)
	}
	return fmt.Sprintf("%d:%s:%s", userID, operationType, resource)
}

// operationResourceClause renders the `resource_id = ?` / `resource_id IS NULL`
// predicate that Prisma produces for `resourceId: resourceId || null`.
func operationResourceClause(resourceID int64) (string, []any) {
	if resourceID == 0 {
		return "resource_id IS NULL", nil
	}
	return "resource_id = ?", []any{resourceID}
}

// advisoryTransactionLockString mirrors db/advisory-locks.ts's
// advisoryTransactionLockString. It is duplicated locally (unexported) only until
// server-go/src/db/advisory_locks.go lands; it should then be replaced by the
// shared db.AdvisoryTransactionLockString. Same namespace + hashtext(key) scheme.
func advisoryTransactionLockString(tx *gorm.DB, namespace int, key string) error {
	return tx.Exec(
		"WITH acquired_lock AS (SELECT pg_advisory_xact_lock(?, hashtext(?))) SELECT true AS locked FROM acquired_lock",
		namespace, key,
	).Error
}

// userNotificationChannel mirrors the {channel, channelId, target} tuple.
type userNotificationChannel struct {
	Channel   *models.VerificationChannel
	ChannelID *int32
	Target    string
}

// getUserNotificationChannel mirrors getUserNotificationChannel: first enabled
// channel by createdAt ASC, plus a masked display target.
func getUserNotificationChannel(ctx context.Context, userID int64) (userNotificationChannel, error) {
	if db.DB == nil {
		return userNotificationChannel{}, errOperationVerificationNoDatabase
	}

	var channel models.NotificationChannel
	err := db.DB.WithContext(ctx).
		Where("user_id = ? AND enabled = ?", userID, true).
		Order("created_at ASC").
		First(&channel).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return userNotificationChannel{}, nil
	}
	if err != nil {
		return userNotificationChannel{}, err
	}

	var parsed any
	if len(channel.Config) > 0 {
		if err := json.Unmarshal(channel.Config, &parsed); err != nil {
			return userNotificationChannel{}, err
		}
	}

	var target string
	switch channel.Type {
	case models.NotificationChannelTypeTelegram:
		chatID := "undefined"
		if config, ok := parsed.(map[string]any); ok {
			if value, ok := config["chatId"].(string); ok {
				chatID = value
			}
		}
		runes := []rune(chatID)
		if len(runes) > 6 {
			runes = runes[:6]
		}
		target = "Telegram (" + string(runes) + "***)"
	case models.NotificationChannelTypeDiscord:
		target = "Discord Webhook"
	case models.NotificationChannelTypeWebhook:
		target = "Webhook"
	default:
		target = string(channel.Type)
	}

	verificationChannel := models.VerificationChannel(channel.Type)
	channelID := channel.ID
	return userNotificationChannel{
		Channel:   &verificationChannel,
		ChannelID: &channelID,
		Target:    target,
	}, nil
}

// preparedOperationVerification mirrors the PreparedOperationVerification union.
type preparedOperationVerification struct {
	AlreadySent bool
	ExpiresIn   int64
	Code        string
	CodeHash    string
}

// prepareOperationVerification mirrors the prisma.$transaction block of
// requestOperationVerification.
func prepareOperationVerification(
	ctx context.Context,
	userID int64,
	operationType models.OperationType,
	resourceID int64,
	resourceType string,
	channel models.VerificationChannel,
) (preparedOperationVerification, error) {
	var prepared preparedOperationVerification

	err := db.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := advisoryTransactionLockString(tx, operationVerificationRequestLockNamespace, getOperationVerificationRequestLockKey(userID, operationType, resourceID)); err != nil {
			return err
		}

		now := time.Now().UTC()
		resourceClause, resourceArgs := operationResourceClause(resourceID)

		// 3. 清理过期的验证码
		if err := tx.
			Where("user_id = ? AND expires_at < ?", userID, now).
			Delete(&models.OperationVerification{}).Error; err != nil {
			return err
		}

		// 4. 检查是否有未过期的同类型验证码（防止频繁请求）
		findArgs := append([]any{userID, operationType}, resourceArgs...)
		findArgs = append(findArgs, false, now)

		var existing models.OperationVerification
		findErr := tx.
			Where("user_id = ? AND operation_type = ? AND "+resourceClause+" AND verified = ? AND expires_at > ?", findArgs...).
			Order("created_at DESC").
			First(&existing).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}

		if findErr == nil {
			twoMinutesAgo := now.Add(-operationVerificationAlreadySentWindow)
			if existing.CreatedAt.After(twoMinutesAgo) {
				deleteArgs := append([]any{userID, operationType}, resourceArgs...)
				deleteArgs = append(deleteArgs, false, now, existing.ID)
				if err := tx.
					Where("user_id = ? AND operation_type = ? AND "+resourceClause+" AND verified = ? AND expires_at > ? AND id <> ?", deleteArgs...).
					Delete(&models.OperationVerification{}).Error; err != nil {
					return err
				}

				prepared.AlreadySent = true
				prepared.ExpiresIn = int64(math.Ceil(existing.ExpiresAt.Sub(now).Seconds()))
				return nil
			}

			// 删除旧的验证码。使用 deleteMany 避免并发请求重复删除时抛出 not found。
			deleteArgs := append([]any{userID, operationType}, resourceArgs...)
			deleteArgs = append(deleteArgs, false, now)
			if err := tx.
				Where("user_id = ? AND operation_type = ? AND "+resourceClause+" AND verified = ? AND expires_at > ?", deleteArgs...).
				Delete(&models.OperationVerification{}).Error; err != nil {
				return err
			}
		}

		// 5. 生成新验证码
		code, err := generateVerificationCode()
		if err != nil {
			return err
		}
		codeHash := hashVerificationCode(code)
		expiresAt := now.Add(time.Duration(operationVerificationExpiresInMinutes) * time.Minute)

		var resourceIDPtr *int32
		if resourceID != 0 {
			value := int32(resourceID)
			resourceIDPtr = &value
		}
		var resourceTypePtr *string
		if resourceType != "" {
			resourceTypePtr = &resourceType
		}

		// 6. 存储验证码
		record := models.OperationVerification{
			UserID:        int32(userID),
			OperationType: operationType,
			Code:          codeHash,
			Channel:       channel,
			ResourceID:    resourceIDPtr,
			ResourceType:  resourceTypePtr,
			ExpiresAt:     expiresAt,
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}

		prepared.Code = code
		prepared.CodeHash = codeHash
		return nil
	})
	if err != nil {
		return preparedOperationVerification{}, err
	}
	return prepared, nil
}

// RequestOperationVerification mirrors requestOperationVerification.
func RequestOperationVerification(
	ctx context.Context,
	userID int64,
	operationType models.OperationType,
	resourceID int64,
	resourceType string,
) (RequestVerificationResult, error) {
	if db.DB == nil {
		return RequestVerificationResult{}, errOperationVerificationNoDatabase
	}

	// 1. 获取用户信息
	var user models.User
	err := db.DB.WithContext(ctx).
		Select("id", "email", "username").
		Where("id = ?", userID).
		First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RequestVerificationResult{
			Success:   false,
			Error:     "User not found",
			ErrorCode: OperationVerificationErrorUserNotFound,
		}, nil
	}
	if err != nil {
		return RequestVerificationResult{}, err
	}

	// 2. 确定验证渠道
	var channel models.VerificationChannel
	var maskedTarget string

	if IsAccountOperation(operationType) {
		if user.Email == nil || *user.Email == "" {
			return RequestVerificationResult{
				Success:   false,
				Error:     "Email not configured",
				ErrorCode: OperationVerificationErrorEmailNotConfigured,
			}, nil
		}
		channel = models.VerificationChannelEmail
		maskedTarget = maskEmail(*user.Email)
	} else {
		notifyChannel, err := getUserNotificationChannel(ctx, userID)
		if err != nil {
			return RequestVerificationResult{}, err
		}
		if notifyChannel.Channel == nil {
			return RequestVerificationResult{
				Success:   false,
				Error:     "No notification channel configured",
				ErrorCode: OperationVerificationErrorNoNotificationChannel,
			}, nil
		}
		channel = *notifyChannel.Channel
		maskedTarget = notifyChannel.Target
		if maskedTarget == "" {
			maskedTarget = string(channel)
		}
	}

	prepared, err := prepareOperationVerification(ctx, userID, operationType, resourceID, resourceType, channel)
	if err != nil {
		return RequestVerificationResult{}, err
	}

	if prepared.AlreadySent {
		return RequestVerificationResult{
			Success:      true,
			Channel:      channel,
			MaskedTarget: maskedTarget,
			ExpiresIn:    prepared.ExpiresIn,
			Error:        "Verification code already sent, please wait",
		}, nil
	}

	// 7. 发送验证码
	operationName := GetOperationName(operationType, "zh")

	if channel == models.VerificationChannelEmail {
		result := OperationVerificationSendResult{
			Success: false,
			Error:   "operation verification email sender not configured",
		}
		if OperationVerificationEmailSender != nil {
			result = OperationVerificationEmailSender(*user.Email, OperationVerificationEmailData{
				Username:         user.Username,
				OperationName:    operationName,
				Code:             prepared.Code,
				ExpiresInMinutes: operationVerificationExpiresInMinutes,
			})
		}
		if !result.Success {
			if err := deleteOperationVerificationByCode(ctx, userID, operationType, prepared.CodeHash); err != nil {
				return RequestVerificationResult{}, err
			}
			return RequestVerificationResult{
				Success:   false,
				Error:     result.Error,
				ErrorCode: OperationVerificationErrorSendFailed,
			}, nil
		}
	} else {
		result := OperationVerificationSendResult{
			Success: false,
			Error:   "operation verification notification sender not configured",
		}
		if OperationVerificationNotificationSender != nil {
			result = OperationVerificationNotificationSender(userID, OperationVerificationNotificationData{
				OperationName:    operationName,
				Code:             prepared.Code,
				ExpiresInMinutes: operationVerificationExpiresInMinutes,
			})
		}
		if !result.Success {
			if err := deleteOperationVerificationByCode(ctx, userID, operationType, prepared.CodeHash); err != nil {
				return RequestVerificationResult{}, err
			}
			return RequestVerificationResult{
				Success:   false,
				Error:     result.Error,
				ErrorCode: OperationVerificationErrorSendFailed,
			}, nil
		}
	}

	return RequestVerificationResult{
		Success:      true,
		Channel:      channel,
		MaskedTarget: maskedTarget,
		ExpiresIn:    int64(operationVerificationExpiresInMinutes) * 60,
	}, nil
}

// deleteOperationVerificationByCode mirrors the cleanup after a send failure.
func deleteOperationVerificationByCode(ctx context.Context, userID int64, operationType models.OperationType, codeHash string) error {
	return db.DB.WithContext(ctx).
		Where("user_id = ? AND operation_type = ? AND code = ?", userID, operationType, codeHash).
		Delete(&models.OperationVerification{}).Error
}

// recordFailedVerificationAttempt mirrors recordFailedVerificationAttempt.
// It reports whether the verification is now exhausted. Any error while
// incrementing is treated as exhausted, matching the Node try/catch.
func recordFailedVerificationAttempt(ctx context.Context, verificationID int32) (bool, error) {
	var attempts int32
	row := db.DB.WithContext(ctx).
		Raw("UPDATE operation_verifications SET attempts = attempts + 1 WHERE id = ? RETURNING attempts", verificationID).
		Row()
	if err := row.Scan(&attempts); err != nil {
		return true, nil
	}

	if attempts >= operationVerificationMaxAttempts {
		if err := db.DB.WithContext(ctx).
			Where("id = ? AND verified = ?", verificationID, false).
			Delete(&models.OperationVerification{}).Error; err != nil {
			return false, err
		}
		return true, nil
	}

	return false, nil
}

// VerifyOperationCode mirrors verifyOperationCode.
func VerifyOperationCode(
	ctx context.Context,
	userID int64,
	operationType models.OperationType,
	code string,
	resourceID int64,
) (VerifyOperationResult, error) {
	if db.DB == nil {
		return VerifyOperationResult{}, errOperationVerificationNoDatabase
	}

	now := time.Now().UTC()
	codeHash := hashVerificationCode(code)
	resourceClause, resourceArgs := operationResourceClause(resourceID)

	findArgs := append([]any{userID, operationType}, resourceArgs...)
	findArgs = append(findArgs, false, now, operationVerificationMaxAttempts)

	var candidates []models.OperationVerification
	if err := db.DB.WithContext(ctx).
		Where("user_id = ? AND operation_type = ? AND "+resourceClause+" AND verified = ? AND expires_at > ? AND attempts < ?", findArgs...).
		Order("created_at DESC").
		Find(&candidates).Error; err != nil {
		return VerifyOperationResult{}, err
	}

	index, found := matchVerificationCandidate(candidates, code, codeHash)
	if !found {
		if len(candidates) > 0 {
			exhausted, err := recordFailedVerificationAttempt(ctx, candidates[0].ID)
			if err != nil {
				return VerifyOperationResult{}, err
			}
			if exhausted {
				return VerifyOperationResult{
					Success:   false,
					Verified:  false,
					Error:     "Too many verification attempts, please request a new code",
					ErrorCode: OperationVerificationErrorTooManyAttempts,
				}, nil
			}
		}

		return VerifyOperationResult{
			Success:   false,
			Verified:  false,
			Error:     "Invalid or expired verification code",
			ErrorCode: OperationVerificationErrorInvalidCode,
		}, nil
	}

	// 标记为已验证。使用条件 updateMany 避免候选记录被并发清理/耗尽时抛出 500。
	verification := candidates[index]
	verifiedAt := time.Now().UTC()

	claimArgs := []any{verification.ID, userID, operationType}
	claimArgs = append(claimArgs, resourceArgs...)
	claimArgs = append(claimArgs, false, verifiedAt, operationVerificationMaxAttempts)

	result := db.DB.WithContext(ctx).
		Model(&models.OperationVerification{}).
		Where("id = ? AND user_id = ? AND operation_type = ? AND "+resourceClause+" AND verified = ? AND expires_at > ? AND attempts < ?", claimArgs...).
		Updates(map[string]any{"verified": true, "verified_at": verifiedAt})
	if result.Error != nil {
		return VerifyOperationResult{}, result.Error
	}

	if result.RowsAffected != 1 {
		return VerifyOperationResult{
			Success:   false,
			Verified:  false,
			Error:     "Invalid or expired verification code",
			ErrorCode: OperationVerificationErrorInvalidCode,
		}, nil
	}

	return VerifyOperationResult{Success: true, Verified: true}, nil
}

// IsOperationVerified mirrors isOperationVerified (verification valid 10 minutes).
func IsOperationVerified(
	ctx context.Context,
	userID int64,
	operationType models.OperationType,
	resourceID int64,
) (bool, error) {
	if db.DB == nil {
		return false, errOperationVerificationNoDatabase
	}

	tenMinutesAgo := time.Now().UTC().Add(-operationVerificationValidWindow)
	resourceClause, resourceArgs := operationResourceClause(resourceID)

	args := append([]any{userID, operationType}, resourceArgs...)
	args = append(args, true, tenMinutesAgo)

	var record models.OperationVerification
	err := db.DB.WithContext(ctx).
		Where("user_id = ? AND operation_type = ? AND "+resourceClause+" AND verified = ? AND verified_at > ?", args...).
		First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// CheckOperationVerificationRequirement mirrors checkOperationVerificationRequirement.
func CheckOperationVerificationRequirement(
	ctx context.Context,
	userID int64,
	operationType models.OperationType,
	resourceID int64,
) (OperationVerificationRequirement, error) {
	required, err := operationVerificationRequired(ctx, userID, operationType)
	if err != nil {
		return OperationVerificationRequirement{}, err
	}
	if !required {
		return OperationVerificationRequirement{Required: false, Verified: true}, nil
	}

	verified, err := IsOperationVerified(ctx, userID, operationType, resourceID)
	if err != nil {
		return OperationVerificationRequirement{}, err
	}
	return OperationVerificationRequirement{Required: true, Verified: verified}, nil
}

// ClaimOperationVerificationRequirement mirrors claimOperationVerificationRequirement:
// atomically consumes one already-verified record so concurrent requests cannot
// reuse it.
func ClaimOperationVerificationRequirement(
	ctx context.Context,
	userID int64,
	operationType models.OperationType,
	resourceID int64,
) (OperationVerificationRequirement, error) {
	if db.DB == nil {
		return OperationVerificationRequirement{}, errOperationVerificationNoDatabase
	}

	required, err := operationVerificationRequired(ctx, userID, operationType)
	if err != nil {
		return OperationVerificationRequirement{}, err
	}
	if !required {
		return OperationVerificationRequirement{Required: false, Verified: true}, nil
	}

	tenMinutesAgo := time.Now().UTC().Add(-operationVerificationValidWindow)
	resourceClause, resourceArgs := operationResourceClause(resourceID)

	args := append([]any{userID, operationType}, resourceArgs...)
	args = append(args, true, tenMinutesAgo)

	result := db.DB.WithContext(ctx).
		Where("user_id = ? AND operation_type = ? AND "+resourceClause+" AND verified = ? AND verified_at > ?", args...).
		Delete(&models.OperationVerification{})
	if result.Error != nil {
		return OperationVerificationRequirement{}, result.Error
	}

	return OperationVerificationRequirement{Required: true, Verified: result.RowsAffected > 0}, nil
}

// ConsumeOperationVerification mirrors consumeOperationVerification (called after
// the operation completes).
func ConsumeOperationVerification(
	ctx context.Context,
	userID int64,
	operationType models.OperationType,
	resourceID int64,
) error {
	if db.DB == nil {
		return errOperationVerificationNoDatabase
	}

	resourceClause, resourceArgs := operationResourceClause(resourceID)
	args := append([]any{userID, operationType}, resourceArgs...)
	args = append(args, true)

	return db.DB.WithContext(ctx).
		Where("user_id = ? AND operation_type = ? AND "+resourceClause+" AND verified = ?", args...).
		Delete(&models.OperationVerification{}).Error
}

// IsResourceVerificationRequired mirrors isResourceVerificationRequired.
func IsResourceVerificationRequired(ctx context.Context, userID int64) (bool, error) {
	channel, err := getUserNotificationChannel(ctx, userID)
	if err != nil {
		return false, err
	}
	return channel.Channel != nil, nil
}

// CleanupExpiredVerifications mirrors cleanupExpiredVerifications.
func CleanupExpiredVerifications(ctx context.Context) (int64, error) {
	if db.DB == nil {
		return 0, errOperationVerificationNoDatabase
	}

	result := db.DB.WithContext(ctx).
		Where("expires_at < ?", time.Now().UTC()).
		Delete(&models.OperationVerification{})
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

// operationVerificationRequired mirrors
// `isAccountOperation(operationType) || await isResourceVerificationRequired(userId)`.
func operationVerificationRequired(ctx context.Context, userID int64, operationType models.OperationType) (bool, error) {
	if IsAccountOperation(operationType) {
		return true, nil
	}
	return IsResourceVerificationRequired(ctx, userID)
}
