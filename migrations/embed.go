// Package migrations embeds the numbered SQL migration files that build the
// local SQLite index.
package migrations

import "embed"

// FS holds the .sql migration files. Files are named <number>_<name>.sql and
// are applied in numeric order by internal/migrate.
//
//go:embed *.sql
var FS embed.FS
