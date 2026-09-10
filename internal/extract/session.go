package extract

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Session - строка таблицы session (см. arch-док A3.2).
type Session struct {
	ID           string
	ProjectID    string
	Slug         string
	Directory    string
	Title        string
	Version      string
	Agent        string
	Model        string // JSON из колонки model
	TimeCreated  int64  // epoch ms
	TimeUpdated  int64  // epoch ms
	TimeArchived *int64 // NULL у всех текущих сессий
	Compacted    bool   // вычисляется по наличию part type=compaction
	TailStartID  string // из compaction-части, пусто если нет
}

// ModelInfo - разобранная колонка session.model.
type ModelInfo struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant"`
}

const sessionCols = `id, project_id, slug, directory, title, version,
	agent, model, time_created, time_updated, time_archived`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var model sql.NullString
	if err := row.Scan(
		&s.ID, &s.ProjectID, &s.Slug, &s.Directory, &s.Title, &s.Version,
		&s.Agent, &model, &s.TimeCreated, &s.TimeUpdated, &s.TimeArchived,
	); err != nil {
		return s, fmt.Errorf("extract: scan session: %w", err)
	}
	if model.Valid {
		s.Model = model.String
	}
	return s, nil
}

// ListSessions возвращает все сессии, отсортированные по time_updated (свежие первыми).
func ListSessions(ctx context.Context, db *sql.DB) ([]Session, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM session ORDER BY time_updated DESC`)
	if err != nil {
		return nil, fmt.Errorf("extract: list sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSession возвращает одну сессию по id.
func GetSession(ctx context.Context, db *sql.DB, id string) (*Session, error) {
	row := db.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM session WHERE id = ?`, id)
	s, err := scanSession(row)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("extract: session %s не найдена", id)
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Model разбирает JSON колонки model. Пустая строка -> пустая структура.
func (s *Session) ModelInfo() ModelInfo {
	var m ModelInfo
	if s.Model != "" {
		_ = json.Unmarshal([]byte(s.Model), &m)
	}
	return m
}
