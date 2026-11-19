package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/rain-kl/payincus/server-go/src/migrations"
)

// incudalMigrationLockID is a dedicated 64-bit PostgreSQL session advisory lock
// key ("payincus" in ASCII hex) protecting Prisma baseline reconciliation and
// Goose version comparison across startup / concurrent instances.
const incudalMigrationLockID int64 = 0x706179696e637573

// MigrationReport summarizes the outcome of startup schema synchronization.
type MigrationReport struct {
	BridgedFromPrisma int
	AppliedCount      int
	PreviousVersion   int64
	CurrentVersion    int64
}

// RunMigrations reconciles any historical Prisma `_prisma_migrations` state into
// Goose's `goose_db_version` table and applies all pending embedded PostgreSQL
// migrations automatically before the backend opens for traffic.
func RunMigrations(ctx context.Context) (MigrationReport, error) {
	var report MigrationReport

	dsn := resolveDatabaseURL()
	if dsn == "" {
		return report, fmt.Errorf("DATABASE_URL 环境变量未设置")
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return report, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 2
	cfg.MinConns = 1
	cfg.ConnConfig.ConnectTimeout = time.Duration(envInt("DB_CONNECTION_TIMEOUT", 5000)) * time.Millisecond
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	// DDL migrations (e.g. adding indexes or backfilling columns) may legitimately
	// exceed the 30s query statement_timeout configured for request handlers.
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = envOr("MIGRATION_STATEMENT_TIMEOUT", "0")

	migPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return report, fmt.Errorf("create migration pool: %w", err)
	}
	defer migPool.Close()

	migSQLDB := stdlib.OpenDBFromPool(migPool)
	defer migSQLDB.Close()

	return runMigrationsWithSQLDB(ctx, migSQLDB)
}

func runMigrationsWithSQLDB(ctx context.Context, sqlDB *sql.DB) (MigrationReport, error) {
	return runMigrationsUpToWithSQLDB(ctx, sqlDB, 0)
}

func runMigrationsUpToWithSQLDB(ctx context.Context, sqlDB *sql.DB, targetVersion int64) (MigrationReport, error) {
	var report MigrationReport

	// Acquire a dedicated connection for the session-level reconciliation advisory lock
	// so unlock runs reliably on the exact same PostgreSQL backend session.
	lockConn, err := sqlDB.Conn(ctx)
	if err != nil {
		return report, fmt.Errorf("acquire migration lock connection: %w", err)
	}
	defer lockConn.Close()

	if _, err := lockConn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", incudalMigrationLockID); err != nil {
		return report, fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, unlockErr := lockConn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", incudalMigrationLockID); unlockErr != nil {
			log.Printf("warning: release migration advisory lock: %v", unlockErr)
		}
	}()

	fsys, err := migrations.FS()
	if err != nil {
		return report, fmt.Errorf("load embedded migrations: %w", err)
	}

	sessionLocker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return report, fmt.Errorf("create goose session locker: %w", err)
	}

	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		sqlDB,
		fsys,
		goose.WithSessionLocker(sessionLocker),
	)
	if err != nil {
		return report, fmt.Errorf("init goose provider: %w", err)
	}

	// Ensure goose_db_version exists before bridging historical Prisma rows.
	prevVersion, err := provider.GetDBVersion(ctx)
	if err != nil {
		return report, fmt.Errorf("read current schema version: %w", err)
	}
	report.PreviousVersion = prevVersion

	bridged, err := reconcilePrismaMigrations(ctx, sqlDB)
	if err != nil {
		return report, err
	}
	report.BridgedFromPrisma = bridged

	if bridged > 0 {
		if updatedPrev, err := provider.GetDBVersion(ctx); err == nil {
			report.PreviousVersion = updatedPrev
		}
	}

	var results []*goose.MigrationResult
	if targetVersion > 0 {
		results, err = provider.UpTo(ctx, targetVersion)
	} else {
		results, err = provider.Up(ctx)
	}
	if err != nil {
		return report, fmt.Errorf("apply database migrations: %w", err)
	}
	report.AppliedCount = len(results)

	curVersion, err := provider.GetDBVersion(ctx)
	if err != nil {
		return report, fmt.Errorf("read migrated schema version: %w", err)
	}
	report.CurrentVersion = curVersion

	if report.BridgedFromPrisma > 0 {
		log.Printf("reconciled %d historical Prisma migration(s) into goose_db_version", report.BridgedFromPrisma)
	}
	if report.AppliedCount > 0 {
		log.Printf("applied %d database migration(s); schema version is now %d", report.AppliedCount, report.CurrentVersion)
	} else {
		log.Printf("database schema is up to date (version %d)", report.CurrentVersion)
	}

	return report, nil
}

// reconcilePrismaMigrations copies completed historical Prisma migration
// records from `_prisma_migrations` into `goose_db_version` so upgrading from
// the legacy Node/Prisma runtime never re-runs already applied baseline DDL.
func reconcilePrismaMigrations(ctx context.Context, sqlDB *sql.DB) (int, error) {
	var hasPrismaTable bool
	if err := sqlDB.QueryRowContext(ctx, `SELECT to_regclass('public._prisma_migrations') IS NOT NULL`).Scan(&hasPrismaTable); err != nil {
		return 0, fmt.Errorf("check _prisma_migrations existence: %w", err)
	}
	if !hasPrismaTable {
		return 0, nil
	}

	var unfinishedName string
	err := sqlDB.QueryRowContext(ctx, `
		SELECT migration_name
		FROM "_prisma_migrations"
		WHERE finished_at IS NULL AND rolled_back_at IS NULL
		ORDER BY started_at ASC, migration_name ASC
		LIMIT 1
	`).Scan(&unfinishedName)
	if err == nil {
		return 0, fmt.Errorf("refusing auto-migration: _prisma_migrations has unresolved incomplete migration %q", unfinishedName)
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("inspect unfinished _prisma_migrations rows: %w", err)
	}

	rows, err := sqlDB.QueryContext(ctx, `
		SELECT DISTINCT migration_name
		FROM "_prisma_migrations"
		WHERE finished_at IS NOT NULL AND rolled_back_at IS NULL
	`)
	if err != nil {
		return 0, fmt.Errorf("query applied _prisma_migrations: %w", err)
	}
	defer rows.Close()

	var appliedNames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return 0, fmt.Errorf("scan _prisma_migrations row: %w", err)
		}
		appliedNames = append(appliedNames, name)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate _prisma_migrations rows: %w", err)
	}
	if len(appliedNames) == 0 {
		return 0, nil
	}

	existingRows, err := sqlDB.QueryContext(ctx, `
		SELECT version_id
		FROM "goose_db_version"
		WHERE is_applied = true
	`)
	if err != nil {
		return 0, fmt.Errorf("query goose_db_version rows: %w", err)
	}
	defer existingRows.Close()

	recorded := make(map[int64]struct{})
	for existingRows.Next() {
		var versionID int64
		if err := existingRows.Scan(&versionID); err != nil {
			return 0, fmt.Errorf("scan goose_db_version row: %w", err)
		}
		recorded[versionID] = struct{}{}
	}
	if err := existingRows.Err(); err != nil {
		return 0, fmt.Errorf("iterate goose_db_version rows: %w", err)
	}

	var toInsert []int64
	for _, name := range appliedNames {
		versionID, ok := migrations.LookupPrismaVersion(name)
		if !ok {
			return 0, fmt.Errorf("applied Prisma migration %q is unknown to embedded Goose baseline", name)
		}
		if _, exists := recorded[versionID]; !exists {
			toInsert = append(toInsert, versionID)
			recorded[versionID] = struct{}{}
		}
	}
	if len(toInsert) == 0 {
		return 0, nil
	}

	sort.Slice(toInsert, func(i, j int) bool { return toInsert[i] < toInsert[j] })

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin Prisma->Goose bridge transaction: %w", err)
	}
	defer tx.Rollback()

	for _, versionID := range toInsert {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO "goose_db_version" (version_id, is_applied)
			VALUES ($1, true)
		`, versionID); err != nil {
			return 0, fmt.Errorf("insert bridged goose_db_version %d: %w", versionID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit Prisma->Goose bridge transaction: %w", err)
	}
	return len(toInsert), nil
}
