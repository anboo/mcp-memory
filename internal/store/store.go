// Package store is the local SQLite repository for the memory index
// (memory.db). It owns the chunks table, the external-content FTS tables, the
// sqlite-vec vector table, session metadata and per-session sync state.
//
// The OpenCode source database is never touched here: this package writes only
// to the dedicated index file.
package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"opencode-rag/internal/chunk"
	"opencode-rag/internal/extract"
	"opencode-rag/internal/migrate"
	"opencode-rag/internal/stem"
	"opencode-rag/migrations"

	_ "modernc.org/sqlite"
	_ "modernc.org/sqlite/vec" // registers the vec0 virtual table via auto extension
)

// Embedder is the minimal embedding interface used for vector backfill.
type Embedder interface {
	Embed(ctx context.Context, texts []string, batchSize int) ([][]float32, error)
}

// Store is an open handle to the local index database.
type Store struct {
	db  *sql.DB
	dim int
}

// Open opens (creating if needed) the SQLite index at path, applies pending
// migrations and ensures the vec0 table matches dim.
func Open(ctx context.Context, path string, dim int, model string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("store: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: create directory %s: %w", dir, err)
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	if err := migrate.Apply(ctx, db, migrations.FS); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate.EnsureVectorTable(ctx, db, dim, model); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dim: dim}, nil
}

// DB exposes the underlying handle for the search package.
func (s *Store) DB() *sql.DB { return s.db }

// Dim returns the configured embedding dimension.
func (s *Store) Dim() int { return s.dim }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SessionMeta is the row written to the sessions table.
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

// MetaFromSession adapts an extracted session to a store row.
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

// UpsertSession stores session metadata.
func (s *Store) UpsertSession(ctx context.Context, m SessionMeta) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, project_id, project_path, title, agent, model,
			directory, time_created, time_updated, compacted, tail_start_id, parts_count)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (id) DO UPDATE SET
			project_id = excluded.project_id,
			project_path = excluded.project_path,
			title = excluded.title,
			agent = excluded.agent,
			model = excluded.model,
			directory = excluded.directory,
			time_created = excluded.time_created,
			time_updated = excluded.time_updated,
			compacted = excluded.compacted,
			tail_start_id = excluded.tail_start_id,
			parts_count = excluded.parts_count`,
		m.ID, m.ProjectID, m.ProjectPath, m.Title, m.Agent, m.Model,
		m.Directory, m.TimeCreated, m.TimeUpdated, boolInt(m.Compacted), m.TailStartID, m.PartsCount)
	if err != nil {
		return fmt.Errorf("store: upsert session %s: %w", m.ID, err)
	}
	return nil
}

// ReplaceSessionChunks deletes and re-inserts every chunk of a session in one
// transaction. embedTexts holds the text sent to the embedder per chunk id
// (nil for chunk types that are not embedded); vectors holds the resulting
// vectors. A nil vectors map is valid and leaves vectors pending.
func (s *Store) ReplaceSessionChunks(ctx context.Context, chunks []chunk.Chunk, embedTexts map[string]string, vectors map[string][]float32) error {
	if len(chunks) == 0 {
		return nil
	}
	sessionID := chunks[0].SessionID

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback()

	// vec_chunks is a virtual table and does not participate in cascades, so
	// vectors must be removed before their chunks.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM vec_chunks WHERE rowid IN (SELECT rowid FROM chunks WHERE session_id = ?)`,
		sessionID); err != nil {
		return fmt.Errorf("store: delete vectors for %s: %w", sessionID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("store: delete chunks for %s: %w", sessionID, err)
	}

	insChunk, err := tx.PrepareContext(ctx, `INSERT INTO chunks (
		id, session_id, project_id, project_path, message_id, msg_role, part_type,
		tool, command, content, snippet, files, position, time_created, time_updated,
		truncated, embed_text, stemmed, embedded)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("store: prepare chunk insert: %w", err)
	}
	defer insChunk.Close()

	insVec, err := tx.PrepareContext(ctx, `INSERT INTO vec_chunks(rowid, embedding) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("store: prepare vector insert: %w", err)
	}
	defer insVec.Close()

	for i := range chunks {
		c := &chunks[i]
		files, _ := json.Marshal(c.Files)
		embedText := embedTexts[c.ID]
		var vector []float32
		embedded := 0
		if vectors != nil {
			if v, ok := vectors[c.ID]; ok && len(v) > 0 {
				vector = v
				embedded = 1
			}
		}

		res, err := insChunk.ExecContext(ctx,
			c.ID, c.SessionID, c.ProjectID, c.ProjectPath, c.MessageID, c.MsgRole, c.PartType,
			nullStr(c.Tool), nullStr(c.Command), c.Content, c.Snippet, string(files),
			c.Position, c.TimeCreated, c.TimeUpdated, boolInt(c.Truncated), nullStr(embedText),
			stem.Text(c.Content), embedded)
		if err != nil {
			return fmt.Errorf("store: insert chunk %s: %w", c.ID, err)
		}
		if vector != nil {
			rowid, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("store: last insert id for %s: %w", c.ID, err)
			}
			if _, err := insVec.ExecContext(ctx, rowid, VecBlob(vector)); err != nil {
				return fmt.Errorf("store: insert vector %s: %w", c.ID, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit chunks for %s: %w", sessionID, err)
	}
	return nil
}

// Counts used by status and diagnostics.

func (s *Store) ChunkCount(ctx context.Context) (int64, error) {
	return s.scalar(ctx, `SELECT COUNT(*) FROM chunks`)
}

func (s *Store) SessionCount(ctx context.Context) (int64, error) {
	return s.scalar(ctx, `SELECT COUNT(*) FROM sessions`)
}

func (s *Store) VectorCount(ctx context.Context) (int64, error) {
	return s.scalar(ctx, `SELECT COUNT(*) FROM vec_chunks`)
}

// PendingVectorCount is the number of chunks that have embed_text but no
// vector yet (for example because the embedder was unavailable).
func (s *Store) PendingVectorCount(ctx context.Context) (int64, error) {
	return s.scalar(ctx,
		`SELECT COUNT(*) FROM chunks WHERE embedded = 0 AND embed_text IS NOT NULL AND embed_text <> ''`)
}

func (s *Store) scalar(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return n, nil
}

// MetaValue reads a key from the meta table.
func (s *Store) MetaValue(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: meta %s: %w", key, err)
	}
	return v, true, nil
}

// Status - health snapshot used by the memory_status MCP tool.
type Status struct {
	Sessions       int64
	Chunks         int64
	Vectors        int64
	PendingVectors int64
	EmbedDim       int
	EmbedModel     string
	LastSync       int64
	SchemaVersion  int
}

// Status collects index health.
func (s *Store) Status(ctx context.Context) (Status, error) {
	var st Status
	var err error
	if st.Sessions, err = s.SessionCount(ctx); err != nil {
		return st, err
	}
	if st.Chunks, err = s.ChunkCount(ctx); err != nil {
		return st, err
	}
	if st.Vectors, err = s.VectorCount(ctx); err != nil {
		return st, err
	}
	if st.PendingVectors, err = s.PendingVectorCount(ctx); err != nil {
		return st, err
	}
	dimStr, ok, err := s.MetaValue(ctx, "embed_dim")
	if err != nil {
		return st, err
	}
	if ok {
		fmt.Sscanf(dimStr, "%d", &st.EmbedDim)
	}
	model, _, err := s.MetaValue(ctx, "embed_model")
	if err != nil {
		return st, err
	}
	st.EmbedModel = model
	if st.SchemaVersion, err = migrate.Version(ctx, s.db); err != nil {
		return st, err
	}
	var last sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(time_processed) FROM sync_state`).Scan(&last); err != nil {
		return st, fmt.Errorf("store: last sync: %w", err)
	}
	st.LastSync = last.Int64
	return st, nil
}

// ---- sync_state ----

// SetSyncState records the sync status of a session.
func (s *Store) SetSyncState(ctx context.Context, sessionID string, lastUpdated int64, status, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sync_state (session_id, last_time_updated, status, error, time_processed)
		VALUES (?,?,?,?,?)
		ON CONFLICT (session_id) DO UPDATE SET
			last_time_updated = excluded.last_time_updated,
			status = excluded.status,
			error = excluded.error,
			time_processed = excluded.time_processed`,
		sessionID, lastUpdated, status, nullStr(errMsg), time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("store: sync state %s: %w", sessionID, err)
	}
	return nil
}

// PendingSessions returns sessions whose last attempt failed.
func (s *Store) PendingSessions(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_id FROM sync_state WHERE status = 'error' ORDER BY session_id`)
	if err != nil {
		return nil, fmt.Errorf("store: pending sessions: %w", err)
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

// NeedsIndexing reports whether a session must be (re)indexed:
//   - no sync_state row: new session;
//   - status error: retry a failed session;
//   - session.time_updated newer than the last indexed value: changed.
func (s *Store) NeedsIndexing(ctx context.Context, sessionID string, timeUpdated int64) (bool, error) {
	var last int64
	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT last_time_updated, status FROM sync_state WHERE session_id = ?`,
		sessionID).Scan(&last, &status)
	if err == sql.ErrNoRows {
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

// ---- vector backfill ----

// BackfillVectors computes vectors for chunks that have embed_text but no
// vector. It is best effort: callers may ignore an error and keep the index
// FTS-only.
func (s *Store) BackfillVectors(ctx context.Context, emb Embedder, limit int) (int, error) {
	if emb == nil {
		return 0, nil
	}
	query := `SELECT rowid, id, embed_text FROM chunks
	          WHERE embedded = 0 AND embed_text IS NOT NULL AND embed_text <> ''
	          ORDER BY rowid`
	var args []any
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("store: backfill query: %w", err)
	}
	type task struct {
		rowid int64
		id    string
		text  string
	}
	var tasks []task
	for rows.Next() {
		var t task
		if err := rows.Scan(&t.rowid, &t.id, &t.text); err != nil {
			rows.Close()
			return 0, err
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(tasks) == 0 {
		return 0, nil
	}

	texts := make([]string, len(tasks))
	for i := range tasks {
		texts[i] = tasks[i].text
	}
	vecs, err := emb.Embed(ctx, texts, 64)
	if err != nil {
		return 0, fmt.Errorf("store: backfill embed: %w", err)
	}
	if len(vecs) != len(tasks) {
		return 0, fmt.Errorf("store: backfill got %d vectors for %d texts", len(vecs), len(tasks))
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: backfill begin: %w", err)
	}
	defer tx.Rollback()
	insVec, err := tx.PrepareContext(ctx, `INSERT INTO vec_chunks(rowid, embedding) VALUES (?, ?)`)
	if err != nil {
		return 0, err
	}
	defer insVec.Close()
	upd, err := tx.PrepareContext(ctx, `UPDATE chunks SET embedded = 1 WHERE rowid = ?`)
	if err != nil {
		return 0, err
	}
	defer upd.Close()
	for i := range tasks {
		if _, err := insVec.ExecContext(ctx, tasks[i].rowid, VecBlob(vecs[i])); err != nil {
			return 0, err
		}
		if _, err := upd.ExecContext(ctx, tasks[i].rowid); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: backfill commit: %w", err)
	}
	return len(tasks), nil
}

// VecBlob packs a float32 vector as little-endian bytes for sqlite-vec.
func VecBlob(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// BlobVec unpacks a sqlite-vec blob back into a float32 vector.
func BlobVec(b []byte) []float32 {
	n := len(b) / 4
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
