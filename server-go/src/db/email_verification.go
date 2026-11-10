// Port of server/src/db/email-verification.ts (156 lines, 5 exports).
//
// Email verification codes back three flows: registration, password reset and
// email change. The security-critical properties, all preserved verbatim:
//
//   - Codes are generated with a cryptographically secure RNG in the inclusive
//     range 100000..999999 (crypto.randomInt(min, max + 1)).
//   - Only the sha256 digest is persisted (`sha256:<hex>`); legacy plaintext rows
//     are still accepted by verificationCodeMatches.
//   - Codes are single-use: verifyCode claims the row with a DELETE and reports
//     success only when exactly one row was removed, so concurrent submissions
//     cannot both succeed.
//   - Creating a code expires every other live code for the same
//     (email, purpose|'general') pair first, so only the newest code works while
//     the older rows remain for the hourly rate limit.
//   - At most five codes per email per purpose per rolling hour.
//
// Naming: the db package is flat and shared with parallel ports, so every new
// exported symbol carries the Auth prefix.
package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// AuthEmailVerificationPurpose mirrors EmailVerificationPurpose.
type AuthEmailVerificationPurpose string

const (
	// AuthEmailVerificationPurposeRegister mirrors 'register'.
	AuthEmailVerificationPurposeRegister AuthEmailVerificationPurpose = "register"
	// AuthEmailVerificationPurposePasswordReset mirrors 'password_reset'.
	AuthEmailVerificationPurposePasswordReset AuthEmailVerificationPurpose = "password_reset"
	// AuthEmailVerificationPurposeChangeEmail mirrors 'change_email'.
	AuthEmailVerificationPurposeChangeEmail AuthEmailVerificationPurpose = "change_email"
)

const (
	// authVerificationCodeTTLMinutes mirrors CODE_EXPIRATION_MINUTES.
	authVerificationCodeTTLMinutes = 10
	// authVerificationCodeTTL is the same window as a time.Duration.
	authVerificationCodeTTL = authVerificationCodeTTLMinutes * time.Minute
	// authVerificationMaxCodesPerHour mirrors MAX_CODES_PER_HOUR.
	authVerificationMaxCodesPerHour = 5
	// authVerificationHashPrefix mirrors the `sha256:` storage prefix.
	authVerificationHashPrefix = "sha256:"
	// authVerificationPurposeGeneral is the extra purpose matched by every read,
	// mirroring `purpose: { in: [purpose, 'general'] }`.
	authVerificationPurposeGeneral = "general"
	// authVerificationCodeMin/Max are the inclusive bounds of crypto.randomInt.
	authVerificationCodeMin = 100000
	authVerificationCodeMax = 999999
)

// AuthErrEmailVerificationNoDatabase is returned by the helpers when db.Init has
// not run, so an unwired call site fails loudly instead of panicking.
var AuthErrEmailVerificationNoDatabase = errors.New("email verification: database not initialized")

// AuthEmailVerificationCode mirrors createVerificationCode's { code, expiresAt }
// envelope. The code itself is only ever returned to the caller that created it;
// what is stored in the database is its digest.
type AuthEmailVerificationCode struct {
	Code      string
	ExpiresAt time.Time
}

func authRequireEmailVerificationDB() error {
	if DB == nil {
		return AuthErrEmailVerificationNoDatabase
	}
	return nil
}

// AuthGenerateVerificationCode mirrors generateVerificationCode: crypto.randomInt
// between 100000 and 999999 inclusive, rendered as a decimal string.
func AuthGenerateVerificationCode() (string, error) {
	span := big.NewInt(authVerificationCodeMax - authVerificationCodeMin + 1)
	offset, err := rand.Int(rand.Reader, span)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(offset.Int64()+authVerificationCodeMin, 10), nil
}

// AuthHashVerificationCode mirrors hashVerificationCode.
func AuthHashVerificationCode(code string) string {
	digest := sha256.Sum256([]byte(code))
	return authVerificationHashPrefix + hex.EncodeToString(digest[:])
}

// AuthVerificationCodeMatches mirrors verificationCodeMatches. Stored hashes must
// match the digest; legacy rows that still hold the plaintext code compare
// directly.
func AuthVerificationCodeMatches(storedCode, inputCode string) bool {
	if strings.HasPrefix(storedCode, authVerificationHashPrefix) {
		return storedCode == AuthHashVerificationCode(inputCode)
	}
	return storedCode == inputCode
}

// AuthCreateVerificationCode mirrors createVerificationCode. A nil result with a
// nil error means the per-email hourly limit was reached (the route answers 429).
func AuthCreateVerificationCode(
	ctx context.Context,
	email string,
	purpose AuthEmailVerificationPurpose,
) (*AuthEmailVerificationCode, error) {
	if err := authRequireEmailVerificationDB(); err != nil {
		return nil, err
	}

	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	now := time.Now().UTC()
	oneHourAgo := now.Add(-time.Hour)

	var recentCount int64
	if err := DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM email_verification_codes
		  WHERE email = ? AND purpose = ? AND created_at >= ?`,
		normalizedEmail, string(purpose), oneHourAgo,
	).Scan(&recentCount).Error; err != nil {
		return nil, err
	}
	if recentCount >= authVerificationMaxCodesPerHour {
		return nil, nil
	}

	// Expire every other pending code for this purpose (including the generic
	// 'general' rows) so only the newest code can be claimed. The rows are kept:
	// they feed the hourly rate limit above.
	if err := DB.WithContext(ctx).Exec(
		`UPDATE email_verification_codes
		    SET expires_at = ?
		  WHERE email = ? AND purpose IN (?, ?) AND expires_at > ?`,
		now, normalizedEmail, string(purpose), authVerificationPurposeGeneral, now,
	).Error; err != nil {
		return nil, err
	}

	code, err := AuthGenerateVerificationCode()
	if err != nil {
		return nil, err
	}
	expiresAt := now.Add(authVerificationCodeTTL)

	if err := DB.WithContext(ctx).Exec(
		`INSERT INTO email_verification_codes (email, purpose, code, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		normalizedEmail, string(purpose), AuthHashVerificationCode(code), expiresAt, now,
	).Error; err != nil {
		return nil, err
	}

	return &AuthEmailVerificationCode{Code: code, ExpiresAt: expiresAt}, nil
}

// AuthVerifyCode mirrors verifyCode: find the newest live candidate that matches,
// then claim it atomically by deleting exactly that row.
func AuthVerifyCode(
	ctx context.Context,
	email string,
	code string,
	purpose AuthEmailVerificationPurpose,
) (bool, error) {
	if err := authRequireEmailVerificationDB(); err != nil {
		return false, err
	}

	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	codeHash := AuthHashVerificationCode(code)
	now := time.Now().UTC()

	var candidates []struct {
		ID   int32  `gorm:"column:id"`
		Code string `gorm:"column:code"`
	}
	if err := DB.WithContext(ctx).Raw(
		`SELECT id, code FROM email_verification_codes
		  WHERE email = ? AND purpose IN (?, ?) AND expires_at > ?
		  ORDER BY created_at DESC`,
		normalizedEmail, string(purpose), authVerificationPurposeGeneral, now,
	).Scan(&candidates).Error; err != nil {
		return false, err
	}

	matchedID := int32(0)
	for _, candidate := range candidates {
		if candidate.Code == codeHash || AuthVerificationCodeMatches(candidate.Code, code) {
			matchedID = candidate.ID
			break
		}
	}
	if matchedID == 0 {
		return false, nil
	}

	// Claim the code atomically so repeated concurrent submissions fail cleanly.
	result := DB.WithContext(ctx).Exec("DELETE FROM email_verification_codes WHERE id = ?", matchedID)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// AuthCleanupExpiredCodes mirrors cleanupExpiredCodes (periodic cleanup job).
func AuthCleanupExpiredCodes(ctx context.Context) (int64, error) {
	if err := authRequireEmailVerificationDB(); err != nil {
		return 0, err
	}
	result := DB.WithContext(ctx).Exec(
		"DELETE FROM email_verification_codes WHERE expires_at < ?", time.Now().UTC(),
	)
	return result.RowsAffected, result.Error
}

// AuthHasPendingCode mirrors hasPendingCode.
func AuthHasPendingCode(
	ctx context.Context,
	email string,
	purpose AuthEmailVerificationPurpose,
) (bool, error) {
	if err := authRequireEmailVerificationDB(); err != nil {
		return false, err
	}

	normalizedEmail := strings.ToLower(strings.TrimSpace(email))
	var count int64
	if err := DB.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM email_verification_codes
		  WHERE email = ? AND purpose IN (?, ?) AND expires_at > ?`,
		normalizedEmail, string(purpose), authVerificationPurposeGeneral, time.Now().UTC(),
	).Scan(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}
