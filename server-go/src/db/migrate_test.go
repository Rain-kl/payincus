package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/rain-kl/payincus/server-go/src/migrations"
)

func openTestPostgresAdmin(t *testing.T) (baseURL string, sqlDB *sql.DB, ok bool) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return "", nil, false
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		t.Skipf("postgres ping failed: %v", err)
	}
	t.Cleanup(p.Close)
	return dsn, stdlib.OpenDBFromPool(p), true
}

func quotePGIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

func TestRunMigrationsFreshAndPrismaBridgedDatabases(t *testing.T) {
	baseDSN, adminSQL, ok := openTestPostgresAdmin(t)
	if !ok {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL Goose migration integration test")
	}
	defer adminSQL.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	tempDB := fmt.Sprintf("payincus_goose_test_%d", time.Now().UnixNano())
	if _, err := adminSQL.ExecContext(ctx, "CREATE DATABASE "+quotePGIdent(tempDB)); err != nil {
		t.Fatalf("create temp db %s: %v", tempDB, err)
	}
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		_, _ = adminSQL.ExecContext(cleanCtx, "DROP DATABASE IF EXISTS "+quotePGIdent(tempDB)+" WITH (FORCE)")
	})

	cfg, err := pgxpool.ParseConfig(baseDSN)
	if err != nil {
		t.Fatalf("parse base dsn: %v", err)
	}
	cfg.ConnConfig.Database = tempDB
	cfg.MaxConns = 4
	testPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect temp db: %v", err)
	}
	defer testPool.Close()

	testSQL := stdlib.OpenDBFromPool(testPool)
	defer testSQL.Close()

	// 1. Fresh empty database -> migrates 0 -> len(PrismaBaseline) automatically.
	report1, err := runMigrationsWithSQLDB(ctx, testSQL)
	if err != nil {
		t.Fatalf("fresh RunMigrations failed: %v", err)
	}
	wantVer := int64(len(migrations.PrismaBaseline))
	if report1.AppliedCount != len(migrations.PrismaBaseline) || report1.CurrentVersion != wantVer {
		t.Fatalf("fresh migration report = %+v, want AppliedCount=%d CurrentVersion=%d", report1, len(migrations.PrismaBaseline), wantVer)
	}

	// Verify idempotent re-run applies 0 migrations.
	report2, err := runMigrationsWithSQLDB(ctx, testSQL)
	if err != nil {
		t.Fatalf("idempotent RunMigrations failed: %v", err)
	}
	if report2.AppliedCount != 0 || report2.CurrentVersion != wantVer || report2.BridgedFromPrisma != 0 {
		t.Fatalf("idempotent report = %+v, want 0 applied/bridged at %d", report2, wantVer)
	}

	// 2. Simulate legacy Prisma-migrated database: drop goose_db_version, populate
	// `_prisma_migrations` with all 218 applied baseline rows, and re-run startup.
	if _, err := testSQL.ExecContext(ctx, `DROP TABLE "goose_db_version"`); err != nil {
		t.Fatalf("drop goose_db_version: %v", err)
	}
	if _, err := testSQL.ExecContext(ctx, `
		CREATE TABLE "_prisma_migrations" (
			"id" VARCHAR(36) NOT NULL,
			"checksum" VARCHAR(64) NOT NULL,
			"finished_at" TIMESTAMPTZ,
			"migration_name" VARCHAR(255) NOT NULL,
			"logs" TEXT,
			"rolled_back_at" TIMESTAMPTZ,
			"started_at" TIMESTAMPTZ NOT NULL DEFAULT now(),
			"applied_steps_count" INTEGER NOT NULL DEFAULT 1,
			PRIMARY KEY ("id")
		)
	`); err != nil {
		t.Fatalf("create _prisma_migrations: %v", err)
	}
	for _, entry := range migrations.PrismaBaseline {
		if _, err := testSQL.ExecContext(ctx, `
			INSERT INTO "_prisma_migrations" ("id", "checksum", "finished_at", "migration_name")
			VALUES ($1, 'dummy', now(), $2)
		`, fmt.Sprintf("id-%05d", entry.Version), entry.PrismaName); err != nil {
			t.Fatalf("insert prisma row %s: %v", entry.PrismaName, err)
		}
	}

	report3, err := runMigrationsWithSQLDB(ctx, testSQL)
	if err != nil {
		t.Fatalf("Prisma-bridged RunMigrations failed: %v", err)
	}
	if report3.BridgedFromPrisma != len(migrations.PrismaBaseline) || report3.AppliedCount != 0 || report3.CurrentVersion != wantVer {
		t.Fatalf("bridged report = %+v, want Bridged=%d Applied=0 CurrentVersion=%d", report3, len(migrations.PrismaBaseline), wantVer)
	}

	// Second run on bridged DB is also a clean no-op.
	report4, err := runMigrationsWithSQLDB(ctx, testSQL)
	if err != nil {
		t.Fatalf("post-bridge idempotent RunMigrations failed: %v", err)
	}
	if report4.AppliedCount != 0 || report4.BridgedFromPrisma != 0 || report4.CurrentVersion != wantVer {
		t.Fatalf("post-bridge idempotent report = %+v", report4)
	}

	// Verify unresolved failed Prisma migration blocks startup safely.
	if _, err := testSQL.ExecContext(ctx, `DROP TABLE "goose_db_version"`); err != nil {
		t.Fatalf("drop goose_db_version for unresolved check: %v", err)
	}
	if _, err := testSQL.ExecContext(ctx, `
		INSERT INTO "_prisma_migrations" ("id", "checksum", "finished_at", "migration_name", "rolled_back_at")
		VALUES ('unfinished-1', 'dummy', NULL, '20269999999999_broken', NULL)
	`); err != nil {
		t.Fatalf("insert unfinished prisma row: %v", err)
	}
	if _, err := runMigrationsWithSQLDB(ctx, testSQL); err == nil || !strings.Contains(err.Error(), "unresolved incomplete migration") {
		t.Fatalf("expected unresolved incomplete migration error, got %v", err)
	}

	// 3. Partial legacy upgrade: create a DB with the first 210 migrations marked applied in
	// `_prisma_migrations` (actually having run 1..210 DDL), and verify startup bridges 1..210
	// cleanly and runs ONLY the remaining 8 migrations (211..218).
	if len(migrations.PrismaBaseline) > 210 {
		partialDB := tempDB + "_partial"
		if _, err := adminSQL.ExecContext(ctx, "CREATE DATABASE "+quotePGIdent(partialDB)); err != nil {
			t.Fatalf("create partial db %s: %v", partialDB, err)
		}
		t.Cleanup(func() {
			cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cleanCancel()
			_, _ = adminSQL.ExecContext(cleanCtx, "DROP DATABASE IF EXISTS "+quotePGIdent(partialDB)+" WITH (FORCE)")
		})

		pcfg, err := pgxpool.ParseConfig(baseDSN)
		if err != nil {
			t.Fatalf("parse partial dsn: %v", err)
		}
		pcfg.ConnConfig.Database = partialDB
		pcfg.MaxConns = 4
		pPool, err := pgxpool.NewWithConfig(ctx, pcfg)
		if err != nil {
			t.Fatalf("connect partial db: %v", err)
		}
		defer pPool.Close()
		pSQL := stdlib.OpenDBFromPool(pPool)
		defer pSQL.Close()

		// Apply only 1..210 manually, record them in _prisma_migrations, drop goose_db_version.
		fsys, err := migrations.FS()
		if err != nil {
			t.Fatalf("migrations.FS(): %v", err)
		}
		if _, err := pSQL.ExecContext(ctx, `
			CREATE TABLE "_prisma_migrations" (
				"id" VARCHAR(36) NOT NULL PRIMARY KEY,
				"checksum" VARCHAR(64) NOT NULL,
				"finished_at" TIMESTAMPTZ,
				"migration_name" VARCHAR(255) NOT NULL,
				"rolled_back_at" TIMESTAMPTZ,
				"started_at" TIMESTAMPTZ NOT NULL DEFAULT now()
			)
		`); err != nil {
			t.Fatalf("create partial _prisma_migrations: %v", err)
		}
		// Run migrations 1..210 via GooseUpTo, then strip goose_db_version to simulate pre-Goose Prisma DB at 210.
		repUpTo, err := runMigrationsUpToWithSQLDB(ctx, pSQL, 210)
		if err != nil {
			t.Fatalf("seed partial 1..210: %v", err)
		}
		if repUpTo.CurrentVersion != 210 {
			t.Fatalf("partial seed version = %d, want 210", repUpTo.CurrentVersion)
		}
		if _, err := pSQL.ExecContext(ctx, `DROP TABLE "goose_db_version"`); err != nil {
			t.Fatalf("drop goose_db_version in partial db: %v", err)
		}
		for _, entry := range migrations.PrismaBaseline[:210] {
			if _, err := pSQL.ExecContext(ctx, `
				INSERT INTO "_prisma_migrations" ("id", "checksum", "finished_at", "migration_name")
				VALUES ($1, 'dummy', now(), $2)
			`, fmt.Sprintf("p-%05d", entry.Version), entry.PrismaName); err != nil {
				t.Fatalf("insert partial prisma row %s: %v", entry.PrismaName, err)
			}
		}
		_ = fsys

		partialRep, err := runMigrationsWithSQLDB(ctx, pSQL)
		if err != nil {
			t.Fatalf("partial Prisma-bridged RunMigrations failed: %v", err)
		}
		wantRemaining := len(migrations.PrismaBaseline) - 210
		if partialRep.BridgedFromPrisma != 210 || partialRep.AppliedCount != wantRemaining || partialRep.CurrentVersion != wantVer {
			t.Fatalf("partial bridged report = %+v, want Bridged=210 Applied=%d Current=%d", partialRep, wantRemaining, wantVer)
		}
	}
}
