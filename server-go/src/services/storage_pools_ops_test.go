package services

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestIsValidPoolDirectory(t *testing.T) {
	valid := []string{"/var/lib/pools", "/srv/incus_data/pool-1.0"}
	for _, d := range valid {
		if !IsValidPoolDirectory(d) {
			t.Fatalf("IsValidPoolDirectory(%q) = false, want true", d)
		}
	}
	invalid := []string{
		"",
		"relative/path",
		"/var/lib/pools/",
		"/var//pools",
		"/var/./pools",
		"/var/../pools",
		"/dev",
		"/dev/sda",
		"/proc/1",
		"/sys/fs",
		"/var/pools with space",
	}
	for _, d := range invalid {
		if IsValidPoolDirectory(d) {
			t.Fatalf("IsValidPoolDirectory(%q) = true, want false", d)
		}
	}
}

func TestResizeStoragePoolBranches(t *testing.T) {
	t.Run("encrypted-loop-resize", func(t *testing.T) {
		var resizePosted map[string]any
		node := &storageEncryptionTestNode{handler: func(method, path string, body any) (any, error) {
			switch {
			case method == http.MethodGet && path == "/1.0/storage-pools/encpool":
				return map[string]any{"driver": "zfs", "config": map[string]any{"source": "tank/incus"}}, nil
			case method == http.MethodGet && path == "/1.0/agent":
				return map[string]any{"api_extensions": []any{"incus_storage_pool_resize_v1"}}, nil
			case method == http.MethodPost && path == "/1.0/agent/storage/resize":
				resizePosted, _ = body.(map[string]any)
				return map[string]any{}, nil
			}
			return nil, errors.New("unexpected " + method + " " + path)
		}}

		if err := ResizeStoragePool(context.Background(), node, "encpool", "2TiB"); err != nil {
			t.Fatalf("ResizeStoragePool: %v", err)
		}
		if resizePosted["dataset"] != "tank/incus" || resizePosted["sizeGiB"] != int64(2048) {
			t.Fatalf("resizePosted = %+v", resizePosted)
		}
	})

	t.Run("native-incus-patch-resize", func(t *testing.T) {
		var patched map[string]any
		node := &storageEncryptionTestNode{handler: func(method, path string, body any) (any, error) {
			switch {
			case method == http.MethodGet && path == "/1.0/storage-pools/lvmpool":
				return map[string]any{"driver": "lvm", "config": map[string]any{"size": "10GiB"}}, nil
			case method == http.MethodPatch && path == "/1.0/storage-pools/lvmpool":
				patched, _ = body.(map[string]any)
				return map[string]any{}, nil
			}
			return nil, errors.New("unexpected " + method + " " + path)
		}}

		if err := ResizeStoragePool(context.Background(), node, "lvmpool", "20GiB"); err != nil {
			t.Fatalf("ResizeStoragePool: %v", err)
		}
		cfg, _ := patched["config"].(map[string]any)
		if cfg["size"] != "20GiB" {
			t.Fatalf("patched = %+v", patched)
		}
	})
}

func TestProvisionEncryptedPoolValidationAndExtensionGuards(t *testing.T) {
	noopSession := func(ctx context.Context, hostID int32, write bool, fn func(StorageEncryptionNode, StorageEncryptionKeyVault) (map[string]any, error)) (map[string]any, error) {
		return nil, errors.New("session should not be opened on validation error")
	}

	_, err := provisionEncryptedPoolWithSession(context.Background(), 1, EncryptedPoolOptions{
		Name: "bad/name",
	}, noopSession)
	var se *StorageEncryptionError
	if !errors.As(err, &se) || se.StatusCode != 400 {
		t.Fatalf("expected 400 validation error, got %v", err)
	}

	_, err = provisionEncryptedPoolWithSession(context.Background(), 1, EncryptedPoolOptions{
		Name:   "pool1",
		Source: "/dev/sdb",
		Size:   "10GiB",
	}, noopSession)
	if !errors.As(err, &se) || se.StatusCode != 400 {
		t.Fatalf("expected 400 mutually exclusive source/size, got %v", err)
	}

	_, err = provisionEncryptedPoolWithSession(context.Background(), 1, EncryptedPoolOptions{
		Name:      "pool1",
		Size:      "10GiB",
		Directory: "/dev/shm/bad",
	}, noopSession)
	if !errors.As(err, &se) || se.StatusCode != 400 {
		t.Fatalf("expected 400 forbidden directory, got %v", err)
	}
}
