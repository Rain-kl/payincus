// Command prismatogoose converts server/prisma/migrations SQL history into
// numbered embedded Goose migrations and a baseline manifest mapping Prisma
// migration names to Goose version IDs.
//
// Usage: go run ./tools/prismatogoose -migrations ../server/prisma/migrations -sql-out src/migrations/sql -manifest-out src/migrations/manifest_gen.go
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type migrationEntry struct {
	version    int64
	prismaName string
	gooseFile  string
	sqlPath    string
}

var concurrentlyRe = regexp.MustCompile(`(?i)\bCONCURRENTLY\b`)

func main() {
	migrationsDir := flag.String("migrations", "../server/prisma/migrations", "path to server/prisma/migrations")
	sqlOutDir := flag.String("sql-out", "src/migrations/sql", "output directory for Goose SQL files")
	manifestOut := flag.String("manifest-out", "src/migrations/manifest_gen.go", "output Go file for Prisma->Goose baseline map")
	flag.Parse()

	entries, err := loadPrismaMigrations(*migrationsDir)
	if err != nil {
		fatalf("load migrations: %v", err)
	}
	if len(entries) == 0 {
		fatalf("no migrations found under %s", *migrationsDir)
	}

	if err := os.MkdirAll(*sqlOutDir, 0o755); err != nil {
		fatalf("mkdir %s: %v", *sqlOutDir, err)
	}

	expectedFiles := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		rawSQL, err := os.ReadFile(entry.sqlPath)
		if err != nil {
			fatalf("read %s: %v", entry.sqlPath, err)
		}
		converted := convertPrismaSQLToGoose(entry.prismaName, string(rawSQL))
		targetPath := filepath.Join(*sqlOutDir, entry.gooseFile)
		if err := os.WriteFile(targetPath, []byte(converted), 0o644); err != nil {
			fatalf("write %s: %v", targetPath, err)
		}
		expectedFiles[entry.gooseFile] = struct{}{}
	}

	// Remove stale generated baseline files only within the baseline range
	// (leaving manually authored higher-version Goose migrations intact).
	maxBaselineVersion := entries[len(entries)-1].version
	existingDirEntries, err := os.ReadDir(*sqlOutDir)
	if err != nil {
		fatalf("readdir %s: %v", *sqlOutDir, err)
	}
	for _, item := range existingDirEntries {
		if item.IsDir() {
			continue
		}
		name := item.Name()
		if _, ok := expectedFiles[name]; ok {
			continue
		}
		if v, ok := parseGooseVersionPrefix(name); ok && v <= maxBaselineVersion {
			_ = os.Remove(filepath.Join(*sqlOutDir, name))
		}
	}

	if err := writeManifest(*manifestOut, entries); err != nil {
		fatalf("write manifest: %v", err)
	}

	fmt.Printf("prismatogoose: converted %d migrations into %s\n", len(entries), *sqlOutDir)
}

func parseGooseVersionPrefix(name string) (int64, bool) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok || prefix == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

func prismaSortKey(dirName string) (paddedTS string, full string) {
	prefix, _, _ := strings.Cut(dirName, "_")
	for len(prefix) < 14 {
		prefix += "0"
	}
	return prefix, dirName
}

func sanitizeSlug(dirName string) string {
	_, slug, ok := strings.Cut(dirName, "_")
	if !ok || slug == "" {
		slug = dirName
	}
	var b strings.Builder
	for _, r := range strings.ToLower(slug) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else if r == '-' || r == ' ' {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "migration"
	}
	return out
}

func loadPrismaMigrations(root string) ([]migrationEntry, error) {
	items, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	var dirNames []string
	for _, item := range items {
		if !item.IsDir() {
			continue
		}
		dirNames = append(dirNames, item.Name())
	}

	sort.Slice(dirNames, func(i, j int) bool {
		ki, ni := prismaSortKey(dirNames[i])
		kj, nj := prismaSortKey(dirNames[j])
		if ki != kj {
			return ki < kj
		}
		return ni < nj
	})

	entries := make([]migrationEntry, 0, len(dirNames))
	for idx, name := range dirNames {
		sqlPath := filepath.Join(root, name, "migration.sql")
		if _, err := os.Stat(sqlPath); err != nil {
			return nil, fmt.Errorf("migration %s missing migration.sql: %w", name, err)
		}
		ver := int64(idx + 1)
		gooseFile := fmt.Sprintf("%05d_%s.sql", ver, sanitizeSlug(name))
		entries = append(entries, migrationEntry{
			version:    ver,
			prismaName: name,
			gooseFile:  gooseFile,
			sqlPath:    sqlPath,
		})
	}
	return entries, nil
}

type sqlStatement struct {
	raw             string
	hasDollarQuote  bool
	isTxControlOnly bool
	hasConcurrently bool
}

// convertPrismaSQLToGoose splits raw PostgreSQL DDL into top-level statements,
// drops standalone BEGIN;/COMMIT; transaction wrappers (redundant inside Goose
// transactions), marks CONCURRENTLY migrations NO TRANSACTION, and wraps any
// dollar-quoted / PL/pgSQL statements in Goose StatementBegin/StatementEnd.
func convertPrismaSQLToGoose(prismaName, raw string) string {
	cleaned := strings.TrimPrefix(raw, "\ufeff")
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")

	stmts := splitTopLevelSQL(cleaned)
	noTx := false
	for _, st := range stmts {
		if st.hasConcurrently {
			noTx = true
			break
		}
	}

	var out bytes.Buffer
	out.WriteString("-- Code generated by tools/prismatogoose from server/prisma/migrations/")
	out.WriteString(prismaName)
	out.WriteString("/migration.sql\n")
	out.WriteString("-- +goose Up\n")
	if noTx {
		out.WriteString("-- +goose NO TRANSACTION\n")
	}

	wroteAny := false
	for _, st := range stmts {
		if st.isTxControlOnly {
			continue
		}
		trimmed := strings.TrimSpace(st.raw)
		if trimmed == "" {
			continue
		}
		if wroteAny {
			out.WriteString("\n")
		}
		if st.hasDollarQuote {
			out.WriteString("-- +goose StatementBegin\n")
			out.WriteString(trimmed)
			out.WriteString("\n-- +goose StatementEnd\n")
		} else {
			out.WriteString(trimmed)
			out.WriteString("\n")
		}
		wroteAny = true
	}

	if !wroteAny {
		// Preserve empty / comment-only Prisma migrations cleanly as a valid no-op.
		out.WriteString("SELECT 1;\n")
	}
	return out.String()
}

func matchDollarTag(s string, pos int) (tag string, ok bool) {
	if pos >= len(s) || s[pos] != '$' {
		return "", false
	}
	end := pos + 1
	if end < len(s) && s[end] == '$' {
		return "$$", true
	}
	if end >= len(s) || (!isTagStart(s[end])) {
		return "", false
	}
	end++
	for end < len(s) && isTagCont(s[end]) {
		end++
	}
	if end < len(s) && s[end] == '$' {
		return s[pos : end+1], true
	}
	return "", false
}

func isTagStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

func isTagCont(b byte) bool {
	return isTagStart(b) || (b >= '0' && b <= '9')
}

func splitTopLevelSQL(src string) []sqlStatement {
	var stmts []sqlStatement
	start := 0
	hasDollar := false
	i := 0
	n := len(src)

	appendStmt := func(end int) {
		raw := src[start:end]
		active := strings.TrimSpace(stripSQLComments(raw))
		upperActive := strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(active), ";"))
		isTxOnly := !hasDollar && (upperActive == "BEGIN" || upperActive == "COMMIT" || upperActive == "START TRANSACTION")
		hasConc := concurrentlyRe.MatchString(active)
		stmts = append(stmts, sqlStatement{
			raw:             raw,
			hasDollarQuote:  hasDollar,
			isTxControlOnly: isTxOnly,
			hasConcurrently: hasConc,
		})
		start = end
		hasDollar = false
	}

	for i < n {
		c := src[i]
		// Line comment --
		if c == '-' && i+1 < n && src[i+1] == '-' {
			i += 2
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		// Nested block comment /* ... */
		if c == '/' && i+1 < n && src[i+1] == '*' {
			depth := 1
			i += 2
			for i < n && depth > 0 {
				if src[i] == '/' && i+1 < n && src[i+1] == '*' {
					depth++
					i += 2
				} else if src[i] == '*' && i+1 < n && src[i+1] == '/' {
					depth--
					i += 2
				} else {
					i++
				}
			}
			continue
		}
		// Escape string E'...'
		if (c == 'E' || c == 'e') && i+1 < n && src[i+1] == '\'' {
			i += 2
			for i < n {
				if src[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if src[i] == '\'' {
					if i+1 < n && src[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}
		// Standard string '...'
		if c == '\'' {
			i++
			for i < n {
				if src[i] == '\'' {
					if i+1 < n && src[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}
		// Quoted identifier "..."
		if c == '"' {
			i++
			for i < n {
				if src[i] == '"' {
					if i+1 < n && src[i+1] == '"' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}
		// Dollar-quoted literal $tag$...$tag$
		if c == '$' {
			if tag, ok := matchDollarTag(src, i); ok {
				hasDollar = true
				i += len(tag)
				closeIdx := strings.Index(src[i:], tag)
				if closeIdx >= 0 {
					i += closeIdx + len(tag)
				} else {
					i = n
				}
				continue
			}
		}
		if c == ';' {
			appendStmt(i + 1)
			i++
			continue
		}
		i++
	}

	if start < n && strings.TrimSpace(src[start:]) != "" {
		appendStmt(n)
	}
	return stmts
}

func stripSQLComments(s string) string {
	var b strings.Builder
	i := 0
	n := len(s)
	for i < n {
		if s[i] == '-' && i+1 < n && s[i+1] == '-' {
			i += 2
			for i < n && s[i] != '\n' {
				i++
			}
			continue
		}
		if s[i] == '/' && i+1 < n && s[i+1] == '*' {
			depth := 1
			i += 2
			for i < n && depth > 0 {
				if s[i] == '/' && i+1 < n && s[i+1] == '*' {
					depth++
					i += 2
				} else if s[i] == '*' && i+1 < n && s[i+1] == '/' {
					depth--
					i += 2
				} else {
					i++
				}
			}
			continue
		}
		if unicode.IsSpace(rune(s[i])) {
			b.WriteByte(' ')
		} else {
			b.WriteByte(s[i])
		}
		i++
	}
	return b.String()
}

func writeManifest(outPath string, entries []migrationEntry) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("// Code generated by tools/prismatogoose from server/prisma/migrations. DO NOT EDIT.\n\n")
	buf.WriteString("package migrations\n\n")
	buf.WriteString("// PrismaBaselineMigration records the sequential Goose version assigned to\n")
	buf.WriteString("// each historical Prisma migration so existing databases can bridge seamlessly.\n")
	buf.WriteString("type PrismaBaselineMigration struct {\n")
	buf.WriteString("\tVersion    int64\n")
	buf.WriteString("\tPrismaName string\n")
	buf.WriteString("\tGooseFile  string\n")
	buf.WriteString("}\n\n")
	buf.WriteString("// PrismaBaseline lists all historical Prisma migrations in exact execution order.\n")
	buf.WriteString("var PrismaBaseline = []PrismaBaselineMigration{\n")
	for _, e := range entries {
		buf.WriteString(fmt.Sprintf("\t{Version: %d, PrismaName: %q, GooseFile: %q},\n", e.version, e.prismaName, e.gooseFile))
	}
	buf.WriteString("}\n\n")
	buf.WriteString("var prismaBaselineByName = func() map[string]int64 {\n")
	buf.WriteString("\tm := make(map[string]int64, len(PrismaBaseline))\n")
	buf.WriteString("\tfor _, entry := range PrismaBaseline {\n")
	buf.WriteString("\t\tm[entry.PrismaName] = entry.Version\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\treturn m\n")
	buf.WriteString("}()\n\n")
	buf.WriteString("// LookupPrismaVersion returns the Goose version assigned to a historical Prisma migration.\n")
	buf.WriteString("func LookupPrismaVersion(prismaName string) (int64, bool) {\n")
	buf.WriteString("\tv, ok := prismaBaselineByName[prismaName]\n")
	buf.WriteString("\treturn v, ok\n")
	buf.WriteString("}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("format generated manifest: %w", err)
	}
	return os.WriteFile(outPath, formatted, 0o644)
}

func fatalf(formatStr string, args ...any) {
	fmt.Fprintf(os.Stderr, "prismatogoose: "+formatStr+"\n", args...)
	os.Exit(1)
}
