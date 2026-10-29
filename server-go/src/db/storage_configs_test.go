package db

import (
	"encoding/json"
	"testing"

	"gorm.io/datatypes"

	"github.com/rain-kl/payincus/server-go/src/models"
)

// The StorageConfig model is returned to clients as-is, so its JSON shape is the
// contract. The password column is part of the model (the DB accessor must read it
// for the connection test), but routes never serialize the model itself -- see
// routes/storage_configs_test.go TestStorageResponseViewsOmitPassword.
func TestStorageConfigModelJSONShape(t *testing.T) {
	config := models.StorageConfig{
		ID:        9,
		UserID:    4,
		Name:      "nas",
		Type:      models.StorageTypeWEBDAV,
		Host:      "nas.example.com",
		Port:      int32Ptr(5006),
		Username:  strPtr("alice"),
		BasePath:  strPtr("/backups/"),
		Extra:     datatypes.JSON(`{"bucket":"b","region":"auto","forcePathStyle":false}`),
		IsDefault: true,
	}

	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// camelCase Prisma field names, non-null values preserved.
	for key, want := range map[string]any{
		"id":        float64(9),
		"userId":    float64(4),
		"name":      "nas",
		"type":      "WEBDAV",
		"host":      "nas.example.com",
		"port":      float64(5006),
		"username":  "alice",
		"basePath":  "/backups/",
		"isDefault": true,
	} {
		if decoded[key] != want {
			t.Errorf("%s = %#v, want %#v", key, decoded[key], want)
		}
	}

	// `extra` is embedded JSON, not a string.
	extra, ok := decoded["extra"].(map[string]any)
	if !ok {
		t.Fatalf("extra = %#v, want an embedded object", decoded["extra"])
	}
	if extra["bucket"] != "b" || extra["region"] != "auto" || extra["forcePathStyle"] != false {
		t.Fatalf("extra = %#v", extra)
	}
}

func int32Ptr(value int32) *int32 { return &value }

func strPtr(value string) *string { return &value }
