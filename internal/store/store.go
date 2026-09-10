// Package store - репозиторий PostgreSQL/pgvector (arch-док B11).
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"opencode-rag/internal/chunk"
	"opencode-rag/internal/extract"
)

// Store - работа с индексом в PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
}

// New создаёт пул подключений.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close закрывает пул.
func (s *Store) Close() { s.pool.Close() }

// SessionMeta - метаданные сессии для таблицы sessions.
type SessionMeta struct {
	ID          string
	ProjectID   string
	ProjectPath string
	Title       string
	Agent       string
	Model       string
	Directory   string
	TimeCreated int64
	TimeUpdated int64
	Compacted   bool
	TailStartID string
	PartsCount  int
}

// UpsertSession сохраняет метаданные сессии.
func (s *Store) UpsertSession(ctx context.Context, m SessionMeta) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (id, project_id, project_path, title, agent, model,
			directory, time_created, time_updated, compacted, tail_start_id, parts_count)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (id) DO UPDATE SET
			project_id = EXCLUDED.project_id,
			project_path = EXCLUDED.project_path,
			title = EXCLUDED.title,
			agent = EXCLUDED.agent,
			model = EXCLUDED.model,
			time_updated = EXCLUDED.time_updated,
			compacted = EXCLUDED.compacted,
			tail_start_id = EXCLUDED.tail_start_id,
			parts_count = EXCLUDED.parts_count`,
		m.ID, m.ProjectID, m.ProjectPath, m.Title, m.Agent, m.Model,
		m.Directory, m.TimeCreated, m.TimeUpdated, m.Compacted, m.TailStartID, m.PartsCount)
	if err != nil {
		return fmt.Errorf("store: upsert session %s: %w", m.ID, err)
	}
	return nil
}

// ReplaceSessionChunks удаляет чанки сессии и вставляет новые (переиндексация).
func (s *Store) ReplaceSessionChunks(ctx context.Context, chunks []chunk.Chunk) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if len(chunks) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM chunks WHERE session_id = $1`, chunks[0].SessionID); err != nil {
			return fmt.Errorf("store: delete chunks: %w", err)
		}
	}

	batch := &pgx.Batch{}
	for i := range chunks {
		c := &chunks[i]
		var vec *pgvector.Vector
		if c.Embedding != nil {
			v := pgvector.NewVector(c.Embedding)
			vec = &v
		}
		batch.Queue(`
			INSERT INTO chunks (id, session_id, project_id, project_path, message_id,
				msg_role, part_type, tool, command, content, snippet, files,
				position, time_created, time_updated, truncated, embedding)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
			c.ID, c.SessionID, c.ProjectID, c.ProjectPath, c.MessageID,
			c.MsgRole, c.PartType, c.Tool, c.Command, c.Content, c.Snippet, c.Files,
			c.Position, c.TimeCreated, c.TimeUpdated, c.Truncated, vec)
	}

	br := tx.SendBatch(ctx, batch)
	for range chunks {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return fmt.Errorf("store: insert chunk: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return fmt.Errorf("store: close batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// ChunkCount возвращает количество чанков (диагностика).
func (s *Store) ChunkCount(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count: %w", err)
	}
	return n, nil
}

// SessionCount возвращает количество сессий в индексе.
func (s *Store) SessionCount(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count sessions: %w", err)
	}
	return n, nil
}

// ---- sync_state ----

// SetSyncState обновляет статус синхронизации сессии.
func (s *Store) SetSyncState(ctx context.Context, sessionID string, lastUpdated int64, status, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sync_state (session_id, last_time_updated, status, error, time_processed)
		VALUES ($1,$2,$3,$4, now())
		ON CONFLICT (session_id) DO UPDATE SET
			last_time_updated = EXCLUDED.last_time_updated,
			status = EXCLUDED.status,
			error = EXCLUDED.error,
			time_processed = now()`,
		sessionID, lastUpdated, status, errMsg)
	if err != nil {
		return fmt.Errorf("store: sync state %s: %w", sessionID, err)
	}
	return nil
}

// PendingSessions возвращает сессии из sync_state со статусом error (для ретрая).
func (s *Store) PendingSessions(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT session_id FROM sync_state WHERE status = 'error'`)
	if err != nil {
		return nil, fmt.Errorf("store: pending: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// NeedsIndexing проверяет, нужно ли индексировать сессию:
//   - нет записи в sync_state -> да
//   - статус 'error' -> да (ретрай упавших)
//   - time_updated новее последней индексации -> да
func (s *Store) NeedsIndexing(ctx context.Context, sessionID string, timeUpdated int64) (bool, error) {
	var last int64
	var status string
	err := s.pool.QueryRow(ctx,
		`SELECT last_time_updated, status FROM sync_state WHERE session_id = $1`,
		sessionID).Scan(&last, &status)
	if err == pgx.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: needs indexing %s: %w", sessionID, err)
	}
	if status == "error" {
		return true, nil
	}
	return timeUpdated > last, nil
}

// SessionMeta из extract.Session для store.
func MetaFromSession(s *extract.Session, projectPath string, partsCount int) SessionMeta {
	return SessionMeta{
		ID:          s.ID,
		ProjectID:   s.ProjectID,
		ProjectPath: projectPath,
		Title:       s.Title,
		Agent:       s.Agent,
		Model:       s.Model,
		Directory:   s.Directory,
		TimeCreated: s.TimeCreated,
		TimeUpdated: s.TimeUpdated,
		Compacted:   s.Compacted,
		TailStartID: s.TailStartID,
		PartsCount:  partsCount,
	}
}
