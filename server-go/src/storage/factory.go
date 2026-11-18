// Port of server/src/storage/factory.ts.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rain-kl/payincus/server-go/src/lib"
	"github.com/rain-kl/payincus/server-go/src/models"
)

// Create instantiates a Provider from a persisted StorageConfig, decrypting its password.
func Create(config *models.StorageConfig) (Provider, error) {
	if config == nil {
		return nil, errors.New("存储配置不能为空")
	}
	var decrypted *string
	if config.Password != nil && *config.Password != "" {
		plain := lib.DecryptSensitiveData(*config.Password)
		decrypted = &plain
	}
	var extra map[string]any
	if len(config.Extra) > 0 {
		_ = json.Unmarshal(config.Extra, &extra)
	}
	return NewProvider(ConfigData{
		Type:     config.Type,
		Host:     config.Host,
		Port:     config.Port,
		Username: config.Username,
		Password: decrypted,
		BasePath: config.BasePath,
		Extra:    extra,
	})
}

// NewProvider constructs the matching Provider from already-decrypted ConfigData.
func NewProvider(config ConfigData) (Provider, error) {
	switch config.Type {
	case models.StorageTypeWEBDAV:
		return newWebDavProvider(config)
	case models.StorageTypeFTP:
		return newFtpProvider(config)
	case models.StorageTypeSFTP:
		return newSftpProvider(config)
	case models.StorageTypeS3:
		return newS3Provider(config)
	default:
		return nil, fmt.Errorf("未知的存储类型: %s", config.Type)
	}
}

// TestConnection tests a plaintext ConfigData without saving it to the database.
func TestConnection(ctx context.Context, config ConfigData) error {
	provider, err := NewProvider(config)
	if err != nil {
		return err
	}
	return provider.TestConnection(ctx)
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

var storageAssertSafeTarget = lib.AssertSafeStorageTarget
