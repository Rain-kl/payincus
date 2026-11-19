package migrations

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestEmbeddedBaselineMatchesPrismaMigrations(t *testing.T) {
	prismaDir := filepath.Join("..", "..", "..", "server", "prisma", "migrations")
	entries, err := os.ReadDir(prismaDir)
	if err != nil {
		t.Skipf("prisma migrations dir unavailable (%v)", err)
	}

	var prismaNames []string
	for _, entry := range entries {
		if entry.IsDir() {
			prismaNames = append(prismaNames, entry.Name())
		}
	}
	if len(prismaNames) != len(PrismaBaseline) {
		t.Fatalf("PrismaBaseline has %d entries, want %d matching server/prisma/migrations", len(PrismaBaseline), len(prismaNames))
	}

	sort.Strings(prismaNames)
	for _, name := range prismaNames {
		if _, ok := LookupPrismaVersion(name); !ok {
			t.Fatalf("Prisma migration %q missing from PrismaBaseline manifest", name)
		}
	}

	fsys, err := FS()
	if err != nil {
		t.Fatalf("migrations.FS(): %v", err)
	}
	for _, item := range PrismaBaseline {
		raw, err := fs.ReadFile(fsys, item.GooseFile)
		if err != nil {
			t.Fatalf("missing embedded SQL %s: %v", item.GooseFile, err)
		}
		text := string(raw)
		if !strings.Contains(text, "-- +goose Up") {
			t.Fatalf("%s missing -- +goose Up directive", item.GooseFile)
		}
		if strings.Contains(text, "$$") && !strings.Contains(text, "-- +goose StatementBegin") {
			t.Fatalf("%s contains dollar-quoted PL/pgSQL without -- +goose StatementBegin", item.GooseFile)
		}
		if matched := regexp.MustCompile(`(?im)^\s*(BEGIN|COMMIT)\s*;`).FindString(text); matched != "" {
			// Ensure top-level BEGIN;/COMMIT; transaction wrappers outside $$ blocks were stripped.
			strippedDollar := regexp.MustCompile(`(?s)\$\$.*?\$\$`).ReplaceAllString(text, "")
			if regexp.MustCompile(`(?im)^\s*(BEGIN|COMMIT)\s*;`).MatchString(strippedDollar) {
				t.Fatalf("%s still contains standalone BEGIN;/COMMIT; outside dollar quotes", item.GooseFile)
			}
		}
	}
}
