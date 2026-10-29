// Port of server/src/db/ssh-keys.ts.
//
// The TypeScript module returns a hand-built SshKey view model (snake_case keys
// plus an ISO-8601 created_at string, see server/src/types/database.ts), so this
// file keeps the same shape instead of exposing the Prisma model directly.
package db

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm/clause"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// SshKeyData mirrors the TypeScript `SshKey` interface.
type SshKeyData struct {
	ID          int32  `json:"id"`
	UserID      int32  `json:"user_id"`
	Name        string `json:"name"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	CreatedAt   string `json:"created_at"`
}

// SshKeyCreateData mirrors the createSSHKey() argument object.
type SshKeyCreateData struct {
	UserID      int32
	Name        string
	PublicKey   string
	Fingerprint string
}

// mapSshKeyRow reproduces the object literal returned by the TypeScript module.
func mapSshKeyRow(key models.SshKey) SshKeyData {
	return SshKeyData{
		ID:          key.ID,
		UserID:      key.UserID,
		Name:        key.Name,
		PublicKey:   key.PublicKey,
		Fingerprint: key.Fingerprint,
		CreatedAt:   isoTimestamp(key.CreatedAt),
	}
}

// GetSSHKeysByUserID mirrors getSSHKeysByUserId(): newest first, no tiebreaker
// (Prisma issues `ORDER BY created_at DESC` only).
func GetSSHKeysByUserID(ctx context.Context, userID int32) ([]SshKeyData, error) {
	var keys []models.SshKey
	if err := DB.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Find(&keys).Error; err != nil {
		return nil, err
	}

	result := make([]SshKeyData, 0, len(keys))
	for _, key := range keys {
		result = append(result, mapSshKeyRow(key))
	}
	return result, nil
}

// GetSSHKeyByID mirrors getSSHKeyById(); it returns nil when the row is absent.
func GetSSHKeyByID(ctx context.Context, id int32) (*SshKeyData, error) {
	var key models.SshKey
	err := DB.WithContext(ctx).Where("id = ?", id).Limit(1).Find(&key).Error
	if err != nil {
		return nil, err
	}
	if key.ID == 0 {
		return nil, nil
	}
	data := mapSshKeyRow(key)
	return &data, nil
}

// CreateSSHKey mirrors createSSHKey(): it returns the new row id.
//
// created_at is set explicitly because the generated model carries no
// autoCreateTime tag, and GORM would otherwise insert the zero time instead of
// letting the column default fire (Prisma relies on `@default(now())`).
func CreateSSHKey(ctx context.Context, data SshKeyCreateData) (int32, error) {
	now := time.Now().UTC()
	key := models.SshKey{
		UserID:      data.UserID,
		Name:        data.Name,
		PublicKey:   data.PublicKey,
		Fingerprint: data.Fingerprint,
		CreatedAt:   now,
	}
	// Returning{} reads the inserted row back so created_at carries the database's
	// millisecond precision, matching what Prisma's create() reports.
	if err := DB.WithContext(ctx).Clauses(clause.Returning{}).Create(&key).Error; err != nil {
		return 0, err
	}
	return key.ID, nil
}

// DeleteSSHKey mirrors deleteSSHKey(). Prisma's delete rejects a missing row, so
// a zero-row delete is reported as an error here too.
func DeleteSSHKey(ctx context.Context, id int32) error {
	result := DB.WithContext(ctx).Where("id = ?", id).Delete(&models.SshKey{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("ssh key not found")
	}
	return nil
}
