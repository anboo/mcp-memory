// Package search implements hybrid retrieval over the local SQLite index.
//
// Up to four candidate lists are produced and merged with reciprocal rank
// fusion (RRF, k=60), the same merge the previous PostgreSQL backend used:
//
//   - FTS5 unicode61 over raw content;
//   - FTS5 unicode61 over the Snowball-russian stemmed copy;
//   - sqlite-vec KNN over embeddings (optional);
//   - Bleve lexical search (optional, secondary index).
//
// Filters (project, part type, time) are applied before the merge. Every
// source is optional on top of the SQLite FTS baseline; the call fails only
// when all sources fail.
package search

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"opencode-rag/internal/stem"
	"opencode-rag/internal/store"
)

// DefaultTopK is how many candidates each source contributes before merging.
const DefaultTopK = 50

// rrfK is the RRF smoothing constant.
const rrfK = 60

// Source names reported in the search result.
const (
	SourceFTS      = "fts_unicode"
	SourceFTSStem  = "fts_stem"
	SourceVector   = "vector"
	SourceBleve    = "bleve"
)

// Query holds search parameters. An empty Text is an error, Limit defaults to
// 20 and is capped at 50.
type Query struct {
	Text     string
	Project  string // filter by project_path, empty means all
	Type     string // filter by part_type: text|tool|patch|reasoning
	TimeFrom int64  // filter by time_created (epoch ms), 0 means all
	Limit    int
}

// Hit is a search result: a snippet plus the coordinate needed to read the
// original through memory_read.
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

// Result is the merged answer plus the list of sources that contributed.
type Result struct {
	Results  []Hit    `json:"results"`
	Sources  []string `json:"sources"`
	Degraded bool     `json:"degraded"`
	Note     string   `json:"note,omitempty"`
}

// VectorProvider embeds a query string. It is an interface so tests can
// substitute a deterministic implementation.
type VectorProvider interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// LexicalSource is an optional secondary lexical index (Bleve). It is defined
// here so the search package does not depend on the Bleve implementation; the
// concrete type is wired in internal/cli.
type LexicalSource interface {
	Search(ctx context.Context, q Query) ([]Hit, error)
}

// Searcher runs hybrid search over a store.
type Searcher struct {
	st   *store.Store
	vec  VectorProvider
	lex  LexicalSource
}

// New builds a searcher. vec and lex may be nil, in which case those sources
// are skipped.
func New(st *store.Store, vec VectorProvider, lex LexicalSource) *Searcher {
	return &Searcher{st: st, vec: vec, lex: lex}
}

// Search runs all available sources and merges them.
func (s *Searcher) Search(ctx context.Context, q Query) (Result, error) {
	var res Result
	if strings.TrimSpace(q.Text) == "" {
		return res, fmt.Errorf("search: query must not be empty")
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Limit > 50 {
		q.Limit = 50
	}

	db := s.st.DB()
	var lists [][]Hit
	var sources []string
	var notes []string
	attempted, succeeded := 0, 0

	// Source 1: raw FTS.
	attempted++
	if hits, err := ftsSearch(ctx, db, "fts_unicode", stem.Match(q.Text, false), q); err != nil {
		notes = append(notes, "fts_unicode: "+err.Error())
	} else {
		lists = append(lists, hits)
		sources = append(sources, SourceFTS)
		succeeded++
	}

	// Source 2: stemmed FTS.
	attempted++
	if hits, err := ftsSearch(ctx, db, "fts_stem", stem.Match(q.Text, true), q); err != nil {
		notes = append(notes, "fts_stem: "+err.Error())
	} else {
		lists = append(lists, hits)
		sources = append(sources, SourceFTSStem)
		succeeded++
	}

	// Source 3: vector KNN (optional).
	if s.vec != nil {
		attempted++
		if hits, err := s.vectorSearch(ctx, db, q); err != nil {
			notes = append(notes, "vector: "+err.Error())
		} else {
			lists = append(lists, hits)
			sources = append(sources, SourceVector)
			succeeded++
		}
	} else {
		notes = append(notes, "vector: disabled (no embedder)")
	}

	// Source 4: Bleve (optional).
	if s.lex != nil {
		attempted++
		if hits, err := s.lex.Search(ctx, q); err != nil {
			notes = append(notes, "bleve: "+err.Error())
		} else {
			lists = append(lists, hits)
			sources = append(sources, SourceBleve)
			succeeded++
		}
	} else {
		notes = append(notes, "bleve: disabled (index not available)")
	}

	if attempted > 0 && succeeded == 0 {
		return res, fmt.Errorf("search: all sources failed: %s", strings.Join(notes, "; "))
	}

	res.Results = rrfMerge(lists, q.Limit)
	if res.Results == nil {
		res.Results = []Hit{}
	}
	res.Sources = sources
	// Search is complete only when both optional sources contributed. Anything
	// less is reported as degraded so the caller knows semantic or secondary
	// lexical recall is missing.
	res.Degraded = !contains(sources, SourceVector) || !contains(sources, SourceBleve)
	if res.Degraded {
		res.Note = "degraded search: " + strings.Join(notes, "; ")
	}
	return res, nil
}

// ftsSearch queries one FTS5 table. table is a trusted constant.
func ftsSearch(ctx context.Context, db *sql.DB, table, match string, q Query) ([]Hit, error) {
	if match == "" {
		return nil, nil
	}
	where, args := filterSQL(q)
	query := fmt.Sprintf(`SELECT c.id, c.session_id, c.project_path, COALESCE(c.msg_role,''),
			c.part_type, COALESCE(c.tool,''), COALESCE(c.command,''), c.snippet,
			c.position, c.time_created
		FROM %s
		JOIN chunks c ON c.rowid = %s.rowid
		WHERE %s MATCH ?%s
		ORDER BY bm25(%s)
		LIMIT ?`, table, table, table, where, table)
	args = append([]any{match}, args...)
	args = append(args, DefaultTopK)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fts %s: %w", table, err)
	}
	defer rows.Close()
	return scanHits(rows, false)
}

// vectorSearch runs exact KNN over vec_chunks, then applies filters in Go.
func (s *Searcher) vectorSearch(ctx context.Context, db *sql.DB, q Query) ([]Hit, error) {
	vec, err := s.vec.EmbedQuery(ctx, q.Text)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vec) == 0 {
		return nil, fmt.Errorf("empty query vector")
	}
	if s.st.Dim() > 0 && len(vec) != s.st.Dim() {
		return nil, fmt.Errorf("query vector dim %d does not match index dim %d", len(vec), s.st.Dim())
	}

	fetch := DefaultTopK
	if q.Project != "" || q.Type != "" || q.TimeFrom != 0 {
		fetch = DefaultTopK * 5
	}
	rows, err := db.QueryContext(ctx, `SELECT c.id, c.session_id, c.project_path, COALESCE(c.msg_role,''),
			c.part_type, COALESCE(c.tool,''), COALESCE(c.command,''), c.snippet,
			c.position, c.time_created, sub.distance
		FROM (SELECT rowid, distance FROM vec_chunks
		      WHERE embedding MATCH ? ORDER BY distance LIMIT ?) sub
		JOIN chunks c ON c.rowid = sub.rowid
		ORDER BY sub.distance`, store.VecBlob(vec), fetch)
	if err != nil {
		return nil, fmt.Errorf("knn: %w", err)
	}
	defer rows.Close()
	hits, err := scanHits(rows, true)
	if err != nil {
		return nil, err
	}
	hits = applyFilters(hits, q)
	if len(hits) > DefaultTopK {
		hits = hits[:DefaultTopK]
	}
	return hits, nil
}

func filterSQL(q Query) (string, []any) {
	var b strings.Builder
	var args []any
	if q.Project != "" {
		b.WriteString(" AND c.project_path = ?")
		args = append(args, q.Project)
	}
	if q.Type != "" {
		b.WriteString(" AND c.part_type = ?")
		args = append(args, q.Type)
	}
	if q.TimeFrom != 0 {
		b.WriteString(" AND c.time_created >= ?")
		args = append(args, q.TimeFrom)
	}
	return b.String(), args
}

func applyFilters(hits []Hit, q Query) []Hit {
	if q.Project == "" && q.Type == "" && q.TimeFrom == 0 {
		return hits
	}
	out := hits[:0]
	for _, h := range hits {
		if q.Project != "" && h.ProjectPath != q.Project {
			continue
		}
		if q.Type != "" && h.PartType != q.Type {
			continue
		}
		if q.TimeFrom != 0 && h.TimeCreated < q.TimeFrom {
			continue
		}
		out = append(out, h)
	}
	return out
}

func scanHits(rows *sql.Rows, withScore bool) ([]Hit, error) {
	var out []Hit
	for rows.Next() {
		var h Hit
		if withScore {
			var d float64
			if err := rows.Scan(&h.ID, &h.SessionID, &h.ProjectPath, &h.Role, &h.PartType,
				&h.Tool, &h.Command, &h.Snippet, &h.Position, &h.TimeCreated, &d); err != nil {
				return nil, err
			}
			h.Score = d
		} else {
			if err := rows.Scan(&h.ID, &h.SessionID, &h.ProjectPath, &h.Role, &h.PartType,
				&h.Tool, &h.Command, &h.Snippet, &h.Position, &h.TimeCreated); err != nil {
				return nil, err
			}
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// rrfMerge merges candidate lists with reciprocal rank fusion.
func rrfMerge(lists [][]Hit, limit int) []Hit {
	score := make(map[string]float64)
	byID := make(map[string]Hit)
	for _, list := range lists {
		for i, h := range list {
			score[h.ID] += 1.0 / (rrfK + float64(i+1))
			prev, seen := byID[h.ID]
			if !seen || richer(h, prev) {
				byID[h.ID] = h
			}
		}
	}
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
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// richer prefers the hit that carries more display metadata when the same
// chunk is returned by several sources.
func richer(a, b Hit) bool {
	if a.Command != "" && b.Command == "" {
		return true
	}
	if a.Snippet != "" && b.Snippet == "" {
		return true
	}
	return false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
