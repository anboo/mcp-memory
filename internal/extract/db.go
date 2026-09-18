// Package extract reads opencode.db (SQLite) in read-only mode.
//
// Source: ~/.local/share/opencode/opencode.db. Only final projections are
// read: session, message, part. The event table (intermediate updates) is not
// used.
package extract

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

// Open opens the database strictly read-only.
// mode=ro: OpenCode keeps writing to this database (WAL), so writes are
// forbidden here and must never be attempted.
func Open(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("extract: open %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("extract: ping %s: %w", path, err)
	}
	return db, nil
}
