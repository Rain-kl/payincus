// Port of server/src/storage/types.ts.
package storage

import (
	"context"
	"io"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// Provider mirrors IStorageProvider in server/src/storage/types.ts.
type Provider interface {
	UploadStream(ctx context.Context, reader io.Reader, filename string) error
	DownloadStream(ctx context.Context, filename string) (io.ReadCloser, error)
	TestConnection(ctx context.Context) error
	DeleteFile(ctx context.Context, filename string) error
	ListFiles(ctx context.Context, path string) ([]string, error)
}

// ConfigData mirrors StorageConfigData (with decrypted plaintext password).
type ConfigData struct {
	Type     models.StorageType
	Host     string
	Port     *int32
	Username *string
	Password *string
	BasePath *string
	Extra    map[string]any
}
