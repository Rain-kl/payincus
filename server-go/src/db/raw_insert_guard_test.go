// Guard: every raw INSERT into a table whose `updated_at` is NOT NULL without a
// database default must set that column.
//
// Prisma fills `@updatedAt` client-side, so the column has no DEFAULT in the
// schema. A raw INSERT that omits it fails at *runtime* with a not-null
// violation, and the conformance harness only notices if some case happens to
// exercise that write path — registration did not, so the missing column in
// CreateUser/CreateRegisteredUser surfaced only when the auth port tried to
// register a user for real.
//
// The table list is derived from server/prisma/schema.prisma rather than
// hardcoded, so a new model is covered automatically. INSERTs whose column list
// is assembled at runtime are skipped: the checker cannot see the final list.
package db

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var (
	prismaModelBlock = regexp.MustCompile(`(?m)^model\s+(\w+)\s*\{`)
	prismaTableMap   = regexp.MustCompile(`@@map\("([^"]+)"\)`)
	// Only a required updatedAt column becomes NOT NULL without a default.
	prismaRequiredUpdatedAt = regexp.MustCompile(`(?m)^\s*updatedAt\s+DateTime\s+@updatedAt`)
	rawInsert               = regexp.MustCompile(`(?is)INSERT\s+INTO\s+"?(\w+)"?\s*\(([^)]*)\)`)
	// A literal column list contains only identifiers, quotes, commas and space.
	// Anything else (string concatenation, strings.Join, ...) is built at runtime.
	literalColumnList = regexp.MustCompile(`^[\s"\w,]+$`)
)

// requiredUpdatedAtTables parses schema.prisma and returns the table names whose
// updated_at is mandatory and has no database default.
func requiredUpdatedAtTables(t *testing.T) map[string]bool {
	t.Helper()
	path := filepath.Join("..", "..", "..", "server", "prisma", "schema.prisma")
	schema, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(schema)

	tables := map[string]bool{}
	matches := prismaModelBlock.FindAllStringSubmatchIndex(text, -1)
	for i, m := range matches {
		bodyEnd := len(text)
		if i+1 < len(matches) {
			bodyEnd = matches[i+1][0]
		}
		body := text[m[1]:bodyEnd]
		if !prismaRequiredUpdatedAt.MatchString(body) {
			continue
		}
		table := prismaTableMap.FindStringSubmatch(body)
		if table == nil {
			t.Fatalf("model %s has a required updatedAt but no @@map", text[m[2]:m[3]])
		}
		tables[table[1]] = true
	}
	if len(tables) == 0 {
		t.Fatal("parsed no tables: schema.prisma format changed?")
	}
	return tables
}

// scanRawInserts returns one message per literal INSERT that targets a guarded
// table without naming updated_at.
func scanRawInserts(file, text string, tables map[string]bool) []string {
	var offenders []string
	for _, m := range rawInsert.FindAllStringSubmatchIndex(text, -1) {
		table := strings.ToLower(text[m[2]:m[3]])
		if !tables[table] {
			continue
		}
		columns := text[m[4]:m[5]]
		if !literalColumnList.MatchString(columns) {
			continue
		}
		if strings.Contains(strings.ToLower(columns), "updated_at") {
			continue
		}
		line := strings.Count(text[:m[0]], "\n") + 1
		offenders = append(offenders, file+":"+strconv.Itoa(line)+" INSERT INTO "+table+" omits updated_at")
	}
	return offenders
}

func TestRawInsertsSetUpdatedAt(t *testing.T) {
	tables := requiredUpdatedAtTables(t)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	sort.Strings(files)

	var offenders []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		offenders = append(offenders, scanRawInserts(file, string(source), tables)...)
	}

	if len(offenders) > 0 {
		t.Fatalf("raw INSERTs missing updated_at (runtime not-null violation):\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestScanRawInsertsSelfCheck proves the guard can actually fail: both the
// offending and the corrected form are exercised, plus the dynamic form that must
// be ignored.
func TestScanRawInsertsSelfCheck(t *testing.T) {
	tables := map[string]bool{"users": true, "hosts": true}

	bad := "INSERT INTO users (username, email) VALUES ($1, $2)"
	if got := scanRawInserts("x.go", bad, tables); len(got) != 1 {
		t.Fatalf("expected the omission to be flagged, got %v", got)
	}

	good := "INSERT INTO users (username, email, updated_at) VALUES ($1, $2, $3)"
	if got := scanRawInserts("x.go", good, tables); len(got) != 0 {
		t.Fatalf("expected the explicit column to pass, got %v", got)
	}

	dynamic := `statement := "INSERT INTO hosts (" + strings.Join(columns, ", ") + ") VALUES (?)\n"`
	if got := scanRawInserts("x.go", dynamic, tables); len(got) != 0 {
		t.Fatalf("dynamic column lists must be skipped, got %v", got)
	}

	unguarded := "INSERT INTO notes (body) VALUES ($1)"
	if got := scanRawInserts("x.go", unguarded, tables); len(got) != 0 {
		t.Fatalf("tables without a required updated_at must be ignored, got %v", got)
	}
}
