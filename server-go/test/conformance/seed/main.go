// Command seed prepares the throwaway parity database for dual-run conformance.
//
// It is test tooling, never shipped: it only touches the database named in
// -dsn (or DATABASE_URL) and refuses to run against anything that is not a
// *_goparity database, because its whole purpose is to make a scratch database
// look like a fresh install.
//
// Why this exists: parity.sh needs two real accounts — an admin (for 200/4xx
// paths and admin-only endpoints) and a plain user (so admin-only 403s are
// exercised rather than collapsing into 401-vs-401). A `prisma migrate reset`
// drops both, and the conformance runs after it would silently degrade. Running
// this first makes every run start from the same two identities.
//
// Usage:
//
//	go run ./test/conformance/seed
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	dsnDefault := os.Getenv("DATABASE_URL")
	if dsnDefault == "" {
		dsnDefault = os.Getenv("PARITY_DATABASE_URL")
	}
	dsn := flag.String("dsn", dsnDefault, "parity database DSN (default $DATABASE_URL or $PARITY_DATABASE_URL)")
	adminUser := flag.String("admin-user", "admin", "admin username")
	adminPass := flag.String("admin-password", "admin123", "admin password (used only when the row is missing)")
	userUser := flag.String("user-user", "parityuser", "non-admin username; empty to skip")
	userPass := flag.String("user-password", "admin123", "non-admin password (used only when the row is missing)")
	flag.Parse()

	if *dsn == "" {
		fatal("no DSN: set DATABASE_URL or pass -dsn")
	}
	if !strings.Contains(*dsn, "goparity") {
		fatal("refusing to seed a database whose name does not contain \"goparity\"")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		fatal("connect: " + err.Error())
	}
	defer conn.Close(ctx)

	if err := ensureUser(ctx, conn, *adminUser, *adminPass, "admin"); err != nil {
		fatal("admin: " + err.Error())
	}
	if *userUser != "" {
		if err := ensureUser(ctx, conn, *userUser, *userPass, "user"); err != nil {
			fatal("user: " + err.Error())
		}
	}
}

// ensureUser inserts the account only when the username is absent. An existing
// row is never touched: a reset parity database is expected to be empty, but if
// someone seeded it by hand we must not overwrite their credentials.
func ensureUser(ctx context.Context, conn *pgx.Conn, username, password, role string) error {
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE username = $1)`, username).Scan(&exists); err != nil {
		return err
	}
	if exists {
		fmt.Printf("seed: %s already exists\n", username)
		return nil
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	// updated_at has no database default, and email stays NULL like the app's
	// own bootstrap does for the admin account.
	if _, err := conn.Exec(ctx, `
		INSERT INTO users (username, password_hash, role, status, updated_at)
		VALUES ($1, $2, $3::"UserRole", 'active'::"UserStatus", now())`,
		username, string(hash), role,
	); err != nil {
		return err
	}
	fmt.Printf("seed: created %s (role=%s)\n", username, role)
	return nil
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "seed: "+msg)
	os.Exit(1)
}
