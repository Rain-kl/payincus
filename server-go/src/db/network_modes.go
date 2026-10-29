// Port of the NetworkMode normalization embedded in server/src/db/prisma.ts.
// It is idempotent and a no-op when the NetworkMode enum does not exist yet.
package db

import (
	"context"
	"fmt"
	"os"
	"strings"
)

var currentNetworkModes = []string{
	"nat", "nat_ipv6", "nat_ipv6_nat", "ipv6_only", "ipv6_nat", "public_ipv4", "public_ipv4_ipv6",
}

var legacyNetworkModeMap = map[string]string{
	"nat":              "nat",
	"nat_ipv6":         "nat_ipv6",
	"ipv4":             "nat",
	"ipv6":             "nat_ipv6",
	"dual":             "nat_ipv6",
	"ipv4_ipv6":        "nat_ipv6",
	"nat_ipv6_nat":     "nat_ipv6_nat",
	"ipv6_nat":         "ipv6_nat",
	"ipv6_only":        "ipv6_only",
	"public_ipv4":      "public_ipv4",
	"public_ipv4_ipv6": "public_ipv4_ipv6",
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func quoteIdent(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

// NormalizeLegacyNetworkModes rewrites deprecated NetworkMode values and adds any
// missing enum members, matching normalizeLegacyNetworkModes() in the Node backend.
func NormalizeLegacyNetworkModes(ctx context.Context) error {
	if pool == nil {
		return fmt.Errorf("db pool not initialized")
	}

	enumRows, err := pool.Query(ctx, `
		SELECT e.enumlabel
		FROM pg_type t
		JOIN pg_enum e ON t.oid = e.enumtypid
		WHERE t.typname = 'NetworkMode'
		ORDER BY e.enumsortorder`)
	if err != nil {
		return err
	}
	var initialEnumValues []string
	for enumRows.Next() {
		var label string
		if err := enumRows.Scan(&label); err != nil {
			enumRows.Close()
			return err
		}
		initialEnumValues = append(initialEnumValues, label)
	}
	enumRows.Close()
	if len(initialEnumValues) == 0 {
		return nil
	}

	type columnRef struct{ schema, table, column string }
	colRows, err := pool.Query(ctx, `
		SELECT table_schema, table_name, column_name
		FROM information_schema.columns
		WHERE udt_schema = 'public'
		  AND udt_name = 'NetworkMode'
		ORDER BY table_schema, table_name, ordinal_position`)
	if err != nil {
		return err
	}
	var columns []columnRef
	for colRows.Next() {
		var ref columnRef
		if err := colRows.Scan(&ref.schema, &ref.table, &ref.column); err != nil {
			colRows.Close()
			return err
		}
		columns = append(columns, ref)
	}
	colRows.Close()
	if len(columns) == 0 {
		return nil
	}

	var legacyModes []string
	for mode := range legacyNetworkModeMap {
		if !contains(currentNetworkModes, mode) {
			legacyModes = append(legacyModes, mode)
		}
	}
	legacyModesSQL := "'" + strings.Join(legacyModes, "', '") + "'"

	var missingEnumValues []string
	for _, mode := range currentNetworkModes {
		if !contains(initialEnumValues, mode) {
			missingEnumValues = append(missingEnumValues, mode)
		}
	}
	for _, mode := range missingEnumValues {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TYPE "NetworkMode" ADD VALUE IF NOT EXISTS '%s'`, mode)); err != nil {
			return err
		}
	}

	updatedLegacyRows := false
	hasPackages := false
	hasInstances := false
	for _, column := range columns {
		if column.schema == "public" && column.table == "packages" && column.column == "network_mode" {
			hasPackages = true
		}
		if column.schema == "public" && column.table == "instances" && column.column == "network_mode" {
			hasInstances = true
		}
		qualified := quoteIdent(column.schema) + "." + quoteIdent(column.table)
		columnName := quoteIdent(column.column)
		sql := fmt.Sprintf(`
			UPDATE %s
			SET %s = CASE %s::text
				WHEN 'ipv4' THEN 'nat'::"NetworkMode"
				WHEN 'ipv6' THEN 'nat_ipv6'::"NetworkMode"
				WHEN 'dual' THEN 'nat_ipv6'::"NetworkMode"
				WHEN 'ipv4_ipv6' THEN 'nat_ipv6'::"NetworkMode"
				WHEN 'nat_ipv6_nat' THEN 'nat_ipv6_nat'::"NetworkMode"
				WHEN 'ipv6_nat' THEN 'ipv6_nat'::"NetworkMode"
				WHEN 'ipv6_only' THEN 'ipv6_only'::"NetworkMode"
				WHEN 'public_ipv4' THEN 'public_ipv4'::"NetworkMode"
				WHEN 'public_ipv4_ipv6' THEN 'public_ipv4_ipv6'::"NetworkMode"
				ELSE %s
			END
			WHERE %s::text IN (%s)`, qualified, columnName, columnName, columnName, columnName, legacyModesSQL)
		tag, err := pool.Exec(ctx, sql)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			updatedLegacyRows = true
		}
	}

	if hasPackages {
		if _, err := pool.Exec(ctx, `ALTER TABLE "packages" ALTER COLUMN "network_mode" SET DEFAULT 'nat'`); err != nil {
			return err
		}
	}
	if hasInstances {
		if _, err := pool.Exec(ctx, `ALTER TABLE "instances" ALTER COLUMN "network_mode" SET DEFAULT 'nat'`); err != nil {
			return err
		}
	}

	if len(missingEnumValues) > 0 || updatedLegacyRows {
		fmt.Fprintf(os.Stderr, "[Prisma] Normalized legacy NetworkMode values (missing=%v, updatedRows=%t)\n", missingEnumValues, updatedLegacyRows)
	}
	return nil
}
