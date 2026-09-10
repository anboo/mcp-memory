// Package search - гибридный поиск по индексу памяти (arch-док B5).
//
// В одной PostgreSQL: pgvector (семантика) + tsvector (точные имена),
// merge через RRF (reciprocal rank fusion). Фильтры по project/type/time
// применяются в WHERE до слияния.
package search

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultTopK - сколько кандидатов берём из каждого источника.
const DefaultTopK = 50

// rrfK - константа сглаживания RRF (устойчив к разнокалиберным скорам).
const rrfK = 60

// Query - параметры поиска.
type Query struct {
	Text     string
	Project  string // фильтр по project_path, пусто = все
	Type     string // фильтр по part_type: text|tool|patch|reasoning
	TimeFrom int64  // фильтр по time_created (epoch ms), 0 = все
	Limit    int    // сколько результатов вернуть (default 20)
}

// Hit - результат поиска: сниппет + координаты (arch-док B4.1).
type Hit struct {
	ID          string  `json:"id"`
	SessionID   string  `json:"session_id"`
	ProjectPath string  `json:"project_path"`
	Role        string  `json:"role,omitempty"`
	PartType    string  `json:"part_type"`
	Tool        string  `json:"tool,omitempty"`
	Command     string  `json:"command,omitempty"`
	Score       float64 `json:"score"`
	Snippet     string  `json:"snippet"`
	Position    int     `json:"position"`
	TimeCreated int64   `json:"time_created"`
}

// VectorProvider - источник вектора запроса (embedding-клиент).
// Выделен в интерфейс, чтобы тесты могли подменять.
type VectorProvider interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// Searcher - гибридный поиск.
type Searcher struct {
	pool *pgxpool.Pool
	vec  VectorProvider
}

// New создаёт поисковик.
func New(pool *pgxpool.Pool, vec VectorProvider) *Searcher {
	return &Searcher{pool: pool, vec: vec}
}

// Search выполняет гибридный поиск: vector top-K + FTS top-K -> RRF merge.
func (s *Searcher) Search(ctx context.Context, q Query) ([]Hit, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Limit > 50 {
		q.Limit = 50
	}
	if q.Text == "" {
		return nil, fmt.Errorf("search: пустой query")
	}

	var vecHits, ftsHits []Hit
	var vecErr, ftsErr error

	// векторный поиск (семантика)
	if q.Text != "" {
		vecHits, vecErr = s.vectorSearch(ctx, q)
	}

	// полнотекстовый поиск (точные имена)
	ftsHits, ftsErr = s.ftsSearch(ctx, q)

	if vecErr != nil && ftsErr != nil {
		return nil, fmt.Errorf("search: vector: %v; fts: %v", vecErr, ftsErr)
	}
	if vecErr != nil {
		vecHits = nil // деградация: только FTS
	}
	if ftsErr != nil {
		ftsHits = nil // деградация: только vector
	}

	return rrfMerge(vecHits, ftsHits, q.Limit), nil
}

// vectorSearch - семантический поиск по pgvector (cosine).
func (s *Searcher) vectorSearch(ctx context.Context, q Query) ([]Hit, error) {
	vec, err := s.vec.EmbedQuery(ctx, q.Text)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	sql := `
		SELECT id, session_id, project_path, COALESCE(msg_role,''), part_type,
		       COALESCE(tool,''), COALESCE(command,''), snippet, position, time_created,
		       1 - (embedding <=> $1::vector) AS sim
		FROM chunks
		WHERE embedding IS NOT NULL
		  AND ($2::text IS NULL OR project_path = $2)
		  AND ($3::text IS NULL OR part_type = $3)
		  AND ($4::bigint IS NULL OR time_created >= $4)
		ORDER BY embedding <=> $1::vector
		LIMIT $5`

	return s.queryHits(ctx, sql, pgvectorVector(vec), q)
}

// ftsSearch - полнотекстовый поиск по tsvector (русский).
func (s *Searcher) ftsSearch(ctx context.Context, q Query) ([]Hit, error) {
	sql := `
		SELECT id, session_id, project_path, COALESCE(msg_role,''), part_type,
		       COALESCE(tool,''), COALESCE(command,''), snippet, position, time_created,
		       ts_rank(to_tsvector('russian', content), websearch_to_tsquery('russian', $1)) AS score
		FROM chunks
		WHERE to_tsvector('russian', content) @@ websearch_to_tsquery('russian', $1)
		  AND ($2::text IS NULL OR project_path = $2)
		  AND ($3::text IS NULL OR part_type = $3)
		  AND ($4::bigint IS NULL OR time_created >= $4)
		ORDER BY score DESC
		LIMIT $5`

	return s.queryHits(ctx, sql, q.Text, q)
}

// queryHits выполняет SQL и сканирует строки в Hit.
func (s *Searcher) queryHits(ctx context.Context, sql string, args ...any) ([]Hit, error) {
	q := args[len(args)-1].(Query)
	args = args[:len(args)-1]
	args = append(args,
		nilIfEmpty(q.Project),
		nilIfEmpty(q.Type),
		nilIfZero(q.TimeFrom),
		DefaultTopK)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Hit
	for rows.Next() {
		var h Hit
		var sim float64
		if err := rows.Scan(&h.ID, &h.SessionID, &h.ProjectPath, &h.Role,
			&h.PartType, &h.Tool, &h.Command, &h.Snippet, &h.Position,
			&h.TimeCreated, &sim); err != nil {
			return nil, err
		}
		h.Score = sim
		out = append(out, h)
	}
	return out, rows.Err()
}

// rrfMerge объединяет два списка по reciprocal rank fusion.
func rrfMerge(vecHits, ftsHits []Hit, limit int) []Hit {
	score := make(map[string]float64)
	byID := make(map[string]Hit)

	rank := func(hits []Hit) {
		for i, h := range hits {
			score[h.ID] += 1.0 / (rrfK + float64(i+1))
			byID[h.ID] = h
		}
	}
	rank(vecHits)
	rank(ftsHits)

	out := make([]Hit, 0, len(score))
	for id, s := range score {
		h := byID[id]
		h.Score = s
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score == out[j].Score {
			return out[i].TimeCreated > out[j].TimeCreated
		}
		return out[i].Score > out[j].Score
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func pgvectorVector(v []float32) any {
	return fmt.Sprintf("[%s]", formatVec(v))
}

func formatVec(v []float32) string {
	b := make([]byte, 0, len(v)*10)
	for i, f := range v {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, fmt.Sprintf("%.6f", f)...)
	}
	return string(b)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nilIfZero(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
