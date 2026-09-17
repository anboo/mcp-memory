package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// readParts возвращает окно оригинальных частей вокруг координаты.
// Это аналог memory_read: индекс вернул (session_id, position), а полный
// текст берётся из SQLite, а не из индекса.
func readParts(ctx context.Context, db *sql.DB, sessionID string, pos, before, after int) ([]PartWithPos, error) {
	all, err := PartsWithPosition(ctx, db, sessionID)
	if err != nil {
		return nil, err
	}
	if pos < 0 || pos >= len(all) {
		return nil, fmt.Errorf("position %d вне диапазона 0..%d", pos, len(all)-1)
	}
	lo := pos - before
	if lo < 0 {
		lo = 0
	}
	hi := pos + after
	if hi > len(all)-1 {
		hi = len(all) - 1
	}
	return all[lo : hi+1], nil
}

// renderFull возвращает полный текст части без обрезки вывода (для read).
func renderFull(p *Part, maxToolOutput int) string {
	content, _, _, files, ok := renderPart(p, maxToolOutput)
	if !ok {
		return ""
	}
	if p.Type == "patch" {
		return "patch: " + strings.Join(files, ", ")
	}
	return content
}
