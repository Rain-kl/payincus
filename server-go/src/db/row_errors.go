// Row-error helpers.
//
// Two database access styles coexist in this package and they report "no row"
// differently:
//
//   - pgx queries (Pool().QueryRow, tx.QueryRow) return pgx.ErrNoRows;
//   - GORM queries (DB.Raw(...).Row().Scan) go through database/sql and return
//     sql.ErrNoRows.
//
// Comparing a GORM scan against pgx.ErrNoRows silently turns "row not found" into
// a 500, which is what happened to the invite lookups (Node answers 404). Always
// use isNoRows for a scan whose row source may be either style.
package db

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5"
)

// isNoRows reports whether err means "the query matched no rows", accepting both
// error values the two access styles produce.
func isNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows)
}
