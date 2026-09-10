// Package extract читает opencode.db (SQLite) в режиме read-only.
//
// Источник данных: ~/.local/share/opencode/opencode.db.
// Только финальные проекции: session, message, part (не event - там
// промежуточные апдейты, см. tasks/opencode-arch.md раздел A2).
package extract

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // чистый Go драйвер, без cgo
)

// Open открывает базу строго на чтение.
// mode=ro: opencode работает с базой постоянно (WAL), запись запрещена.
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
