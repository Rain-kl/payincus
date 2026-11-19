// Package migrations embeds the sequential PostgreSQL schema migrations
// executed automatically by Goose at startup.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed sql/*.sql
var embeddedSQL embed.FS

// FS returns the embedded migration filesystem rooted at the sql/*.sql directory.
func FS() (fs.FS, error) {
	return fs.Sub(embeddedSQL, "sql")
}
