// Port of server/src/db/storage-configs.ts.
//
// Unlike ssh-keys the TypeScript module returns the Prisma model untouched, so
// the JSON shape comes from models.StorageConfig (camelCase keys, `extra` as a
// raw JSON value, timestamps as RFC3339). Password encryption is the caller's job
// (routes/storage-configs.ts encrypts with lib.EncryptSensitiveData before it
// reaches this layer, exactly like the TypeScript).
package db

import (
	"context"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm/clause"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// StorageConfigCreateData mirrors the createStorageConfig() argument object.
type StorageConfigCreateData struct {
	UserID    int32
	Name      string
	Type      models.StorageType
	Host      string
	Port      *int32
	Username  *string
	Password  *string
	BasePath  *string
	Extra     datatypes.JSON
	IsDefault bool
}

// StorageConfigUpdateData mirrors the updateStorageConfig() argument object.
//
// The TypeScript spreads `{ ...rest }` into Prisma's update data, so "key absent"
// (leave the column alone) and "key present with null" (clear the column) are two
// different things. That distinction cannot be expressed with plain pointers, so
// every field carries a *Provided flag; a nil value together with the flag set
// means "explicitly null".
type StorageConfigUpdateData struct {
	NameProvided     bool
	Name             string
	TypeProvided     bool
	Type             models.StorageType
	HostProvided     bool
	Host             string
	PortProvided     bool
	Port             *int32
	UsernameProvided bool
	Username         *string
	PasswordProvided bool
	Password         *string
	BasePathProvided bool
	BasePath         *string
	// ExtraProvided with a nil Extra reproduces Prisma's `extra: undefined`
	// (omit), which is what the TypeScript does when it normalizes null → undefined.
	ExtraProvided bool
	Extra         datatypes.JSON
	IsDefault     bool
	// IsDefaultProvided mirrors `data.isDefault` being present in the body.
	IsDefaultProvided bool
}

// GetStorageConfigsByUserID mirrors getStorageConfigsByUserId(): default configs
// first, then newest first.
func GetStorageConfigsByUserID(ctx context.Context, userID int32) ([]models.StorageConfig, error) {
	var configs []models.StorageConfig
	if err := DB.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("is_default DESC, created_at DESC").
		Find(&configs).Error; err != nil {
		return nil, err
	}
	if configs == nil {
		configs = []models.StorageConfig{}
	}
	return configs, nil
}

// GetStorageConfigByID mirrors getStorageConfigById(); it returns nil when absent.
func GetStorageConfigByID(ctx context.Context, id int32) (*models.StorageConfig, error) {
	var config models.StorageConfig
	result := DB.WithContext(ctx).Where("id = ?", id).Limit(1).Find(&config)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &config, nil
}

// GetDefaultStorageConfig mirrors getDefaultStorageConfig().
func GetDefaultStorageConfig(ctx context.Context, userID int32) (*models.StorageConfig, error) {
	var config models.StorageConfig
	result := DB.WithContext(ctx).
		Where("user_id = ? AND is_default = ?", userID, true).
		Limit(1).
		Find(&config)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &config, nil
}

// CreateStorageConfig mirrors createStorageConfig(): when the new row is the
// default, every other default of the same user is cleared first.
//
// created_at/updated_at are set explicitly: the generated model has no
// autoCreateTime tag, so GORM would otherwise insert the zero time instead of
// letting the column defaults (@default(now()) / @updatedAt) fire.
func CreateStorageConfig(ctx context.Context, data StorageConfigCreateData) (models.StorageConfig, error) {
	if data.IsDefault {
		if err := clearDefaultStorageConfigs(ctx, data.UserID, 0); err != nil {
			return models.StorageConfig{}, err
		}
	}

	now := time.Now().UTC()
	config := models.StorageConfig{
		UserID:    data.UserID,
		Name:      data.Name,
		Type:      data.Type,
		Host:      data.Host,
		Port:      data.Port,
		Username:  data.Username,
		Password:  data.Password,
		BasePath:  data.BasePath,
		Extra:     data.Extra,
		IsDefault: data.IsDefault,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// Returning{} makes the INSERT read the row back, so createdAt/updatedAt come
	// from the database (millisecond precision, the column is timestamp(3)) exactly
	// as Prisma's create() does.
	if err := DB.WithContext(ctx).Clauses(clause.Returning{}).Create(&config).Error; err != nil {
		return models.StorageConfig{}, err
	}
	return config, nil
}

// UpdateStorageConfig mirrors updateStorageConfig(). As in the TypeScript, a
// missing row is an error (Prisma's findUnique + explicit throw).
func UpdateStorageConfig(ctx context.Context, id int32, data StorageConfigUpdateData) (models.StorageConfig, error) {
	existing, err := GetStorageConfigByID(ctx, id)
	if err != nil {
		return models.StorageConfig{}, err
	}
	if existing == nil {
		return models.StorageConfig{}, fmt.Errorf("存储配置不存在")
	}

	if data.IsDefaultProvided && data.IsDefault {
		if err := clearDefaultStorageConfigs(ctx, existing.UserID, id); err != nil {
			return models.StorageConfig{}, err
		}
	}

	updates := map[string]any{
		// Prisma's @updatedAt always bumps the column.
		"updated_at": time.Now().UTC(),
	}
	if data.NameProvided {
		updates["name"] = data.Name
	}
	if data.TypeProvided {
		updates["type"] = data.Type
	}
	if data.HostProvided {
		updates["host"] = data.Host
	}
	if data.PortProvided {
		updates["port"] = data.Port
	}
	if data.UsernameProvided {
		updates["username"] = data.Username
	}
	if data.PasswordProvided {
		updates["password"] = data.Password
	}
	if data.BasePathProvided {
		updates["base_path"] = data.BasePath
	}
	// Prisma receives `extra: undefined` when the TypeScript normalizes a null
	// patch to undefined, i.e. the column is left untouched.
	if data.ExtraProvided && data.Extra != nil {
		updates["extra"] = data.Extra
	}
	if data.IsDefaultProvided {
		updates["is_default"] = data.IsDefault
	}

	var config models.StorageConfig
	err = DB.WithContext(ctx).
		Model(&config).
		Clauses(clause.Returning{}).
		Where("id = ?", id).
		Updates(updates).Error
	if err != nil {
		return models.StorageConfig{}, err
	}
	return config, nil
}

// DeleteStorageConfig mirrors deleteStorageConfig().
func DeleteStorageConfig(ctx context.Context, id int32) error {
	result := DB.WithContext(ctx).Where("id = ?", id).Delete(&models.StorageConfig{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("存储配置 %d 不存在", id)
	}
	return nil
}

// SetDefaultStorageConfig mirrors setDefaultStorageConfig(): clear every default
// of the user, then flag the target row. The TypeScript runs these as two
// independent statements (not a transaction), so this does too.
//
// updated_at is written explicitly and in UTC: GORM's autoUpdateTime uses
// time.Now() in the process timezone, but the column is a `timestamp(3)` without
// time zone, so a local-time value would be stored skewed (and Date#toISOString in
// Node always renders UTC).
func SetDefaultStorageConfig(ctx context.Context, userID, id int32) error {
	now := time.Now().UTC()
	if err := DB.WithContext(ctx).
		Model(&models.StorageConfig{}).
		Where("user_id = ? AND is_default = ?", userID, true).
		Updates(map[string]any{"is_default": false, "updated_at": now}).Error; err != nil {
		return err
	}
	return DB.WithContext(ctx).
		Model(&models.StorageConfig{}).
		Where("id = ?", id).
		Updates(map[string]any{"is_default": true, "updated_at": now}).Error
}

// CountActiveTasksForStorageConfig mirrors countActiveTasksForStorageConfig():
// pending/processing backup upload tasks that reference the config.
func CountActiveTasksForStorageConfig(ctx context.Context, storageConfigID int32) (int64, error) {
	var count int64
	err := DB.WithContext(ctx).
		Model(&models.BackupUploadTask{}).
		Where("storage_config_id = ?", storageConfigID).
		Where("status IN ?", []string{
			string(models.BackupUploadTaskStatusPENDING),
			string(models.BackupUploadTaskStatusPROCESSING),
		}).
		Count(&count).Error
	if err != nil {
		return 0, err
	}
	return count, nil
}

// clearDefaultStorageConfigs mirrors `updateMany({ where: { userId, isDefault:
// true, id: { not: excludeID } }, data: { isDefault: false } })`. excludeID 0
// means "no exclusion", which is the create path.
//
// Like SetDefaultStorageConfig the timestamp is pinned to UTC so the `timestamp(3)`
// column holds the same instant Node's updatedAt would.
func clearDefaultStorageConfigs(ctx context.Context, userID, excludeID int32) error {
	query := DB.WithContext(ctx).
		Model(&models.StorageConfig{}).
		Where("user_id = ? AND is_default = ?", userID, true)
	if excludeID != 0 {
		query = query.Where("id <> ?", excludeID)
	}
	return query.Updates(map[string]any{
		"is_default": false,
		"updated_at": time.Now().UTC(),
	}).Error
}
