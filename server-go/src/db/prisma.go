// Port of server/src/db/prisma.ts — one pgx pool shared by GORM and raw SQL.
package db

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

var (
	pool *pgxpool.Pool
	// DB is the GORM handle backed by the shared pgx pool.
	DB *gorm.DB
)

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return fallback
}

// resolveDatabaseURL mirrors the DATABASE_URL synthesis from POSTGRES_* parts.
func resolveDatabaseURL() string {
	if value := os.Getenv("DATABASE_URL"); value != "" {
		return value
	}
	if os.Getenv("POSTGRES_PASSWORD") == "" {
		return ""
	}
	dsn := fmt.Sprintf("postgresql://%s:%s@%s:%s/%s",
		envOr("POSTGRES_USER", "payincus"),
		os.Getenv("POSTGRES_PASSWORD"),
		envOr("POSTGRES_HOST", "127.0.0.1"),
		envOr("POSTGRES_PORT", "5432"),
		envOr("POSTGRES_DB", "payincus"),
	)
	os.Setenv("DATABASE_URL", dsn)
	return dsn
}

// Init opens the shared pool and the GORM handle. It must be called once at startup.
func Init(ctx context.Context) error {
	dsn := resolveDatabaseURL()
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL 环境变量未设置")
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = int32(envInt("DB_POOL_MAX", 20))
	cfg.MinConns = int32(envInt("DB_POOL_MIN", 5))
	cfg.MaxConnIdleTime = time.Duration(envInt("DB_IDLE_TIMEOUT", 30000)) * time.Millisecond
	cfg.ConnConfig.ConnectTimeout = time.Duration(envInt("DB_CONNECTION_TIMEOUT", 5000)) * time.Millisecond
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.Itoa(envInt("DB_STATEMENT_TIMEOUT", 30000))

	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	DB, err = gorm.Open(postgres.New(postgres.Config{
		Conn:                 sqlDB,
		PreferSimpleProtocol: true,
	}), &gorm.Config{
		Logger:                 gormlogger.Default.LogMode(gormlogger.Error),
		SkipDefaultTransaction: true,
	})
	if err != nil {
		return fmt.Errorf("open gorm: %w", err)
	}

	return nil
}

// Pool exposes the raw pgx pool for hand-written SQL.
func Pool() *pgxpool.Pool { return pool }

// PoolStats mirrors getDbPoolStats().
func PoolStats() (total, idle, waiting int) {
	if pool == nil {
		return 0, 0, 0
	}
	stat := pool.Stat()
	return int(stat.TotalConns()), int(stat.IdleConns()), int(stat.EmptyAcquireCount())
}

// Close releases the pool, mirroring closePrismaDatabase().
func Close() {
	if pool != nil {
		pool.Close()
	}
}
