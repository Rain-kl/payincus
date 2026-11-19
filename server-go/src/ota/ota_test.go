package ota

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildPostgresEnvFromDatabaseURLDoesNotLeakArgs(t *testing.T) {
	env, err := buildPostgresEnvFromDatabaseURL("postgresql://payincus:s3cr3t%21@127.0.0.1:5433/payincus_prod?sslmode=require")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{
		"PGHOST":     "127.0.0.1",
		"PGPORT":     "5433",
		"PGUSER":     "payincus",
		"PGPASSWORD": "s3cr3t!",
		"PGDATABASE": "payincus_prod",
		"PGSSLMODE":  "require",
	}
	got := make(map[string]string, len(env))
	for _, entry := range env {
		for k := range want {
			prefix := k + "="
			if len(entry) >= len(prefix) && entry[:len(prefix)] == prefix {
				got[k] = entry[len(prefix):]
			}
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("env[%s] = %q, want %q", k, got[k], v)
		}
	}
}

func TestAssertSafeManagedPath(t *testing.T) {
	if err := assertSafeManagedPath("/opt/incudal/releases", "/opt/incudal", "SYSTEM_UPDATE_RELEASES_DIR"); err != nil {
		t.Fatalf("valid child path rejected: %v", err)
	}
	if err := assertSafeManagedPath("/opt/incudal", "/opt/incudal", "SYSTEM_UPDATE_RELEASES_DIR"); err == nil {
		t.Fatal("installDir itself must be rejected as managed child path")
	}
	if err := assertSafeManagedPath("/tmp/outside", "/opt/incudal", "SYSTEM_UPDATE_DOWNLOAD_DIR"); err == nil {
		t.Fatal("outside path must be rejected")
	}
}

func TestAssertArtifactStagingGoLayout(t *testing.T) {
	dir := t.TempDir()
	required := []string{
		"package.json",
		"client/dist/user/index.html",
		"client/dist/admin/index.html",
		"server/incudal-server",
		"server/templates/install.sh",
		"scripts/verify-split-host.sh",
		"scripts/verify-production-readiness.sh",
		"scripts/verify-log-header-exposure.sh",
		".env.example",
		"version.json",
	}
	for _, rel := range required {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := assertArtifactStaging(dir); err != nil {
		t.Fatalf("complete Go staging rejected: %v", err)
	}

	_ = os.Remove(filepath.Join(dir, "server/incudal-server"))
	if err := assertArtifactStaging(dir); err == nil {
		t.Fatal("staging missing server/incudal-server should fail")
	}
}
