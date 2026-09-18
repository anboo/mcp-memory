package search

import (
	"context"
	"errors"
	"testing"

	"github.com/anboo/mcp-memory/internal/chunk"
	"github.com/anboo/mcp-memory/internal/store"
)

func TestRRFMerge(t *testing.T) {
	first := []Hit{
		{ID: "a", Score: 0.9, TimeCreated: 100},
		{ID: "b", Score: 0.8, TimeCreated: 200},
		{ID: "c", Score: 0.7, TimeCreated: 300},
	}
	second := []Hit{
		{ID: "b", Score: 1.0, TimeCreated: 200}, // overlap
		{ID: "d", Score: 0.9, TimeCreated: 400}, // only here
	}

	merged := rrfMerge([][]Hit{first, second}, 10)
	if len(merged) != 4 {
		t.Fatalf("merged = %d, want 4", len(merged))
	}

	var bScore float64
	for _, h := range merged {
		if h.ID == "b" {
			bScore = h.Score
		}
	}
	if bScore <= 0 {
		t.Fatal("b not found in merge")
	}
	for _, h := range merged {
		if h.ID != "b" && h.Score >= bScore {
			t.Fatalf("b should rank highest because it is in both lists: %+v", h)
		}
	}
}

func TestRRFMergeLimit(t *testing.T) {
	list := make([]Hit, 10)
	for i := range list {
		list[i] = Hit{ID: string(rune('a' + i)), TimeCreated: int64(i)}
	}
	if got := rrfMerge([][]Hit{list}, 5); len(got) != 5 {
		t.Fatalf("limit not applied: %d", len(got))
	}
}

func TestRRFMergeEmpty(t *testing.T) {
	if got := rrfMerge(nil, 10); len(got) != 0 {
		t.Fatalf("empty lists must yield an empty result: %d", len(got))
	}
}

func TestRRFMergePrefersRicherHit(t *testing.T) {
	poor := []Hit{{ID: "x", TimeCreated: 1}}
	rich := []Hit{{ID: "x", Command: "rg foo", Snippet: "foo", TimeCreated: 1}}
	got := rrfMerge([][]Hit{poor, rich}, 10)
	if len(got) != 1 || got[0].Command != "rg foo" {
		t.Fatalf("expected the richer hit to win: %+v", got)
	}
}

// fakeEmbedder is a deterministic query embedder.
type fakeEmbedder struct {
	vec []float32
	err error
}

func (f *fakeEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return f.vec, f.err
}

func testStore(t *testing.T, dim int) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir()+"/memory.db", dim, "test-model")
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func insertChunks(t *testing.T, st *store.Store, chunks []chunk.Chunk, vectors map[string][]float32) {
	t.Helper()
	texts := map[string]string{}
	for i := range chunks {
		if chunk.NeedsEmbedding(&chunks[i]) {
			texts[chunks[i].ID] = chunk.EmbedContent(&chunks[i])
		}
	}
	if err := st.ReplaceSessionChunks(context.Background(), chunks, texts, vectors); err != nil {
		t.Fatalf("replace chunks: %v", err)
	}
}

func sampleChunks() []chunk.Chunk {
	return []chunk.Chunk{
		{ID: "c1", SessionID: "s1", ProjectPath: "/p1", PartType: "text",
			Content: "why Redis Cluster CLUSTERDOWN on node two", Snippet: "CLUSTERDOWN", Position: 0, TimeCreated: 100},
		{ID: "c2", SessionID: "s1", ProjectPath: "/p1", PartType: "text",
			Content: "почему не подключался Redis Cluster к узлу", Snippet: "redis", Position: 1, TimeCreated: 200},
		{ID: "c3", SessionID: "s2", ProjectPath: "/p2", PartType: "tool", Tool: "bash",
			Content: "bash: rg MEMORY_EMBED_DIM", Snippet: "rg", Position: 0, TimeCreated: 300},
	}
}

func TestSearchFTSOnly(t *testing.T) {
	st := testStore(t, 4)
	insertChunks(t, st, sampleChunks(), nil)

	// No embedder and no lexical source: FTS only.
	s := New(st, nil, nil)
	res, err := s.Search(context.Background(), Query{Text: "CLUSTERDOWN", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Results) == 0 {
		t.Fatal("expected an FTS hit")
	}
	if !contains(res.Sources, SourceFTS) {
		t.Fatalf("sources = %v, want fts_unicode", res.Sources)
	}
	if !res.Degraded {
		t.Fatal("FTS-only search should be reported as degraded")
	}
	if res.Results[0].ID != "c1" {
		t.Fatalf("unexpected top hit: %+v", res.Results[0])
	}
}

func TestSearchStemmedRussian(t *testing.T) {
	st := testStore(t, 4)
	insertChunks(t, st, sampleChunks(), nil)

	s := New(st, nil, nil)
	res, err := s.Search(context.Background(), Query{Text: "подключался", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Results) == 0 || res.Results[0].ID != "c2" {
		t.Fatalf("stemmed FTS did not find c2: %+v", res.Results)
	}
	if !contains(res.Sources, SourceFTSStem) {
		t.Fatalf("sources = %v, want fts_stem", res.Sources)
	}
}

func TestSearchFilters(t *testing.T) {
	st := testStore(t, 4)
	insertChunks(t, st, sampleChunks(), nil)

	s := New(st, nil, nil)
	res, err := s.Search(context.Background(), Query{Text: "Redis Cluster", Project: "/p2", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Results) != 0 {
		t.Fatalf("project filter should exclude all hits: %+v", res.Results)
	}
}

func TestSearchVectorAndDegradation(t *testing.T) {
	st := testStore(t, 4)
	vectors := map[string][]float32{
		"c1": {1, 0, 0, 0},
		"c2": {0, 1, 0, 0},
		"c3": {0, 0, 1, 0},
	}
	insertChunks(t, st, sampleChunks(), vectors)

	// Working embedder adds the vector source.
	s := New(st, &fakeEmbedder{vec: []float32{1, 0, 0, 0}}, nil)
	res, err := s.Search(context.Background(), Query{Text: "anything at all", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !contains(res.Sources, SourceVector) {
		t.Fatalf("sources = %v, want vector", res.Sources)
	}

	// Failing embedder degrades to FTS and reports it.
	s = New(st, &fakeEmbedder{err: errors.New("ollama down")}, nil)
	res, err = s.Search(context.Background(), Query{Text: "CLUSTERDOWN", Limit: 10})
	if err != nil {
		t.Fatalf("degraded search must not fail: %v", err)
	}
	if len(res.Results) == 0 {
		t.Fatal("expected FTS fallback results")
	}
	if !res.Degraded || res.Note == "" {
		t.Fatalf("expected a degraded note: %+v", res)
	}
}

func TestSearchEmptyQuery(t *testing.T) {
	st := testStore(t, 4)
	s := New(st, nil, nil)
	if _, err := s.Search(context.Background(), Query{Text: "   "}); err == nil {
		t.Fatal("empty query must be rejected")
	}
}

func TestSearchEmptyResultsAreSlice(t *testing.T) {
	st := testStore(t, 4)
	insertChunks(t, st, sampleChunks(), nil)
	s := New(st, nil, nil)
	res, err := s.Search(context.Background(), Query{Text: "zzzznomatch", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.Results == nil {
		t.Fatal("results must be an empty slice, not nil")
	}
}
