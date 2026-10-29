// Access-token invalidation backed by the token_invalidations table.
// Port of isAccessTokenInvalidated / invalidateUserAccessTokens / invalidateSessionAccessToken.
package lib

import (
	"context"
	"time"

	"gorm.io/gorm/clause"

	"github.com/rain-kl/payincus/server-go/src/db"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// UserLevelSessionID marks a user-wide invalidation (all sessions).
const UserLevelSessionID = "__USER_LEVEL__"

// IsAccessTokenInvalidated reports whether a token issued at tokenIssuedAt (seconds)
// is invalidated for the user/session. Database errors fail open (return false),
// matching the Node behaviour.
func IsAccessTokenInvalidated(ctx context.Context, userID int32, tokenIssuedAt int64, sessionID string) bool {
	if db.DB == nil {
		return false
	}

	if sessionID != "" {
		var record models.TokenInvalidation
		err := db.DB.WithContext(ctx).
			Where("user_id = ? AND session_id = ?", userID, sessionID).
			First(&record).Error
		if err == nil && tokenIssuedAt <= int64(record.InvalidatedAt) {
			return true
		}
	}

	var userLevel models.TokenInvalidation
	err := db.DB.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, UserLevelSessionID).
		First(&userLevel).Error
	if err == nil && tokenIssuedAt <= int64(userLevel.InvalidatedAt) {
		return true
	}

	return false
}

func upsertInvalidation(ctx context.Context, userID int32, sessionID string, invalidatedAt int32) error {
	record := models.TokenInvalidation{
		UserID:        userID,
		SessionID:     &sessionID,
		InvalidatedAt: invalidatedAt,
	}
	return db.DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "session_id"}},
		DoUpdates: clause.Assignments(map[string]any{"invalidated_at": invalidatedAt}),
	}).Create(&record).Error
}

// InvalidateUserAccessTokens invalidates every access token for a user.
func InvalidateUserAccessTokens(ctx context.Context, userID int32) {
	now := int32(time.Now().Unix())
	_ = upsertInvalidation(ctx, userID, UserLevelSessionID, now)
}

// InvalidateSessionAccessToken invalidates access tokens for a single session.
func InvalidateSessionAccessToken(ctx context.Context, userID int32, sessionID string) {
	now := int32(time.Now().Unix())
	_ = upsertInvalidation(ctx, userID, sessionID, now)
}
