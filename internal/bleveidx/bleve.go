// Package bleveidx is the optional secondary lexical index, built on Bleve.
//
// It is a local, CGO-free full-text index with a Russian analyzer for
// morphology and a simple analyzer for exact identifiers. Bleve's own vector
// path (which needs CGO and a build tag) is deliberately not used: the vector
// layer lives in sqlite-vec.
//
// The index is optional at runtime. When its directory is missing or locked,
// callers degrade to the SQLite FTS tables.
package bleveidx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/simple"
	"github.com/blevesearch/bleve/v2/analysis/lang/ru"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"

	"opencode-rag/internal/chunk"
	"opencode-rag/internal/search"
)

// Index wraps an open Bleve index.
type Index struct {
	idx bleve.Index
}

// OpenOrCreate opens the index at path, creating it when the directory does
// not exist yet.
func OpenOrCreate(path string, storeContent bool) (*Index, error) {
	if path == "" {
		return nil, fmt.Errorf("bleve: empty index path")
	}
	if _, err := os.Stat(path); err == nil {
		return Open(path)
	}
	return Create(path, storeContent)
}

// Open opens an existing index.
func Open(path string) (*Index, error) {
	idx, err := bleve.Open(path)
	if err != nil {
		return nil, fmt.Errorf("bleve: open %s: %w", path, err)
	}
	return &Index{idx: idx}, nil
}

// Create builds a fresh index, replacing any existing directory.
func Create(path string, storeContent bool) (*Index, error) {
	if path == "" {
		return nil, fmt.Errorf("bleve: empty index path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("bleve: create parent dir: %w", err)
	}
	if err := os.RemoveAll(path); err != nil {
		return nil, fmt.Errorf("bleve: clear index dir %s: %w", path, err)
	}
	idx, err := bleve.New(path, buildMapping(storeContent))
	if err != nil {
		return nil, fmt.Errorf("bleve: create %s: %w", path, err)
	}
	return &Index{idx: idx}, nil
}

// Close closes the index.
func (i *Index) Close() error {
	if i == nil || i.idx == nil {
		return nil
	}
	return i.idx.Close()
}

// DocCount returns the number of indexed documents.
func (i *Index) DocCount() (uint64, error) {
	if i == nil || i.idx == nil {
		return 0, fmt.Errorf("bleve: index not open")
	}
	n, err := i.idx.DocCount()
	if err != nil {
		return 0, fmt.Errorf("bleve: doc count: %w", err)
	}
	return n, nil
}

// DeleteSession removes every document of a session. Bleve has no
// delete-by-query on the Index interface, so the session's document ids are
// collected with a term query and deleted in a batch.
func (i *Index) DeleteSession(ctx context.Context, sessionID string) error {
	if i == nil || i.idx == nil {
		return fmt.Errorf("bleve: index not open")
	}
	if sessionID == "" {
		return nil
	}
	tq := bleve.NewTermQuery(sessionID)
	tq.SetField("session_id")

	// Always query from offset 0: each round deletes every match it received,
	// so the next search returns the remaining documents.
	const page = 1000
	for round := 0; round < 1_000_000; round++ {
		req := bleve.NewSearchRequestOptions(bleve.NewConjunctionQuery(tq), page, 0, false)
		res, err := i.idx.SearchInContext(ctx, req)
		if err != nil {
			return fmt.Errorf("bleve: delete session scan: %w", err)
		}
		if len(res.Hits) == 0 {
			return nil
		}
		batch := i.idx.NewBatch()
		for _, h := range res.Hits {
			batch.Delete(h.ID)
		}
		if err := i.idx.Batch(batch); err != nil {
			return fmt.Errorf("bleve: delete session batch: %w", err)
		}
		if len(res.Hits) < page {
			return nil
		}
	}
	return fmt.Errorf("bleve: delete session %s: too many documents", sessionID)
}

// AddSession indexes all chunks of one session. Doc id equals chunk id, so
// re-indexing the same chunk overwrites it.
func (i *Index) AddSession(ctx context.Context, chunks []chunk.Chunk) (int, error) {
	if i == nil || i.idx == nil {
		return 0, fmt.Errorf("bleve: index not open")
	}
	batch := i.idx.NewBatch()
	n := 0
	for j := range chunks {
		c := &chunks[j]
		if c.Content == "" {
			continue
		}
		if err := batch.Index(c.ID, docFromChunk(c)); err != nil {
			return n, fmt.Errorf("bleve: index %s: %w", c.ID, err)
		}
		n++
		if n%500 == 0 {
			if err := i.idx.Batch(batch); err != nil {
				return n, fmt.Errorf("bleve: flush batch: %w", err)
			}
			batch = i.idx.NewBatch()
		}
	}
	if batch.Size() > 0 {
		if err := i.idx.Batch(batch); err != nil {
			return n, fmt.Errorf("bleve: flush batch: %w", err)
		}
	}
	return n, nil
}

// doc is the Bleve document shape. Fields listed as stored are returned in
// search hits, which is what lets a hit carry its (session_id, position)
// coordinate without a second lookup.
type doc struct {
	Content      string `json:"content"`
	ContentExact string `json:"content_exact"`
	SessionID    string `json:"session_id"`
	ProjectPath  string `json:"project_path"`
	PartType     string `json:"part_type"`
	Role         string `json:"role"`
	Tool         string `json:"tool"`
	Command      string `json:"command"`
	Snippet      string `json:"snippet"`
	Position     int    `json:"position"`
	TimeCreated  int64  `json:"time_created"`
}

func docFromChunk(c *chunk.Chunk) doc {
	return doc{
		Content:      c.Content,
		ContentExact: c.Content,
		SessionID:    c.SessionID,
		ProjectPath:  c.ProjectPath,
		PartType:     c.PartType,
		Role:         c.MsgRole,
		Tool:         c.Tool,
		Command:      c.Command,
		Snippet:      c.Snippet,
		Position:     c.Position,
		TimeCreated:  c.TimeCreated,
	}
}

func buildMapping(storeContent bool) mapping.IndexMapping {
	im := bleve.NewIndexMapping()
	im.DefaultAnalyzer = ru.AnalyzerName

	dm := bleve.NewDocumentMapping()

	content := bleve.NewTextFieldMapping()
	content.Analyzer = ru.AnalyzerName
	content.Store = storeContent
	dm.AddFieldMappingsAt("content", content)

	contentExact := bleve.NewTextFieldMapping()
	contentExact.Analyzer = simple.Name
	contentExact.Store = false
	contentExact.IncludeInAll = false
	dm.AddFieldMappingsAt("content_exact", contentExact)

	for _, f := range []string{"session_id", "project_path", "part_type", "role", "tool"} {
		fm := bleve.NewKeywordFieldMapping()
		fm.Store = true
		dm.AddFieldMappingsAt(f, fm)
	}

	command := bleve.NewTextFieldMapping()
	command.Analyzer = simple.Name
	command.Store = true
	dm.AddFieldMappingsAt("command", command)

	snippet := bleve.NewTextFieldMapping()
	snippet.Index = false
	snippet.Store = true
	snippet.IncludeInAll = false
	dm.AddFieldMappingsAt("snippet", snippet)

	position := bleve.NewNumericFieldMapping()
	position.Store = true
	dm.AddFieldMappingsAt("position", position)

	timeCreated := bleve.NewNumericFieldMapping()
	timeCreated.Store = true
	dm.AddFieldMappingsAt("time_created", timeCreated)

	im.DefaultMapping = dm
	return im
}

// Search implements search.LexicalSource. The query is a disjunction over the
// Russian-analyzed content and the exact (lowercased) content; filters are
// conjunctive.
func (i *Index) Search(ctx context.Context, q search.Query) ([]search.Hit, error) {
	if i == nil || i.idx == nil {
		return nil, fmt.Errorf("bleve: index not open")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = search.DefaultTopK
	}
	if limit > search.DefaultTopK {
		limit = search.DefaultTopK
	}

	ruq := bleve.NewMatchQuery(q.Text)
	ruq.SetField("content")
	exact := bleve.NewMatchQuery(q.Text)
	exact.SetField("content_exact")
	conjuncts := []query.Query{bleve.NewDisjunctionQuery(ruq, exact)}

	if q.Project != "" {
		tq := bleve.NewTermQuery(q.Project)
		tq.SetField("project_path")
		conjuncts = append(conjuncts, tq)
	}
	if q.Type != "" {
		tq := bleve.NewTermQuery(q.Type)
		tq.SetField("part_type")
		conjuncts = append(conjuncts, tq)
	}
	if q.TimeFrom > 0 {
		from := float64(q.TimeFrom)
		inc := true
		nq := bleve.NewNumericRangeInclusiveQuery(&from, nil, &inc, nil)
		nq.SetField("time_created")
		conjuncts = append(conjuncts, nq)
	}

	req := bleve.NewSearchRequestOptions(bleve.NewConjunctionQuery(conjuncts...), limit, 0, false)
	req.Fields = []string{"*"}
	res, err := i.idx.SearchInContext(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("bleve: search: %w", err)
	}

	out := make([]search.Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, search.Hit{
			ID:          h.ID,
			SessionID:   strField(h.Fields, "session_id"),
			ProjectPath: strField(h.Fields, "project_path"),
			Role:        strField(h.Fields, "role"),
			PartType:    strField(h.Fields, "part_type"),
			Tool:        strField(h.Fields, "tool"),
			Command:     strField(h.Fields, "command"),
			Snippet:     strField(h.Fields, "snippet"),
			Position:    int(numField(h.Fields, "position")),
			TimeCreated: int64(numField(h.Fields, "time_created")),
			Score:       h.Score,
		})
	}
	return out, nil
}

func strField(fields map[string]any, name string) string {
	if v, ok := fields[name]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func numField(fields map[string]any, name string) float64 {
	if v, ok := fields[name]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int64:
			return float64(n)
		}
	}
	return 0
}
