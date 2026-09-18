package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/anboo/mcp-memory/internal/chunk"
	"github.com/anboo/mcp-memory/internal/extract"
)

type fakeEmbedder struct {
	dim int
}

func (f fakeEmbedder) Embed(_ context.Context, texts []string, _ int) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		v := make([]float32, f.dim)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}

func openTestStore(t *testing.T, dim int) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "memory.db"), dim, "test-model")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOpenAppliesMigrations(t *testing.T) {
	st := openTestStore(t, 4)
	if st.Dim() != 4 {
		t.Fatalf("dim = %d", st.Dim())
	}
	s, err := st.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if s.SchemaVersion != 1 {
		t.Fatalf("schema version = %d, want 1", s.SchemaVersion)
	}
}

func TestReplaceSessionChunksAndCounts(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, 4)

	if err := st.UpsertSession(ctx, SessionMeta{ID: "s1", ProjectPath: "/p1", Title: "t", TimeUpdated: 100}); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	chunks := []chunk.Chunk{
		{ID: "c1", SessionID: "s1", ProjectPath: "/p1", PartType: "text", Content: "hello one", Snippet: "hello", Position: 0},
		{ID: "c2", SessionID: "s1", ProjectPath: "/p1", PartType: "text", Content: "hello two", Snippet: "hello", Position: 1},
		{ID: "c3", SessionID: "s1", ProjectPath: "/p1", PartType: "patch", Content: "a.go", Snippet: "patch", Position: 2},
	}
	texts := map[string]string{"c1": "hello one", "c2": "hello two"}
	vectors := map[string][]float32{"c1": {1, 0, 0, 0}}

	if err := st.ReplaceSessionChunks(ctx, chunks, texts, vectors); err != nil {
		t.Fatalf("replace: %v", err)
	}

	if n, _ := st.ChunkCount(ctx); n != 3 {
		t.Fatalf("chunks = %d, want 3", n)
	}
	if n, _ := st.VectorCount(ctx); n != 1 {
		t.Fatalf("vectors = %d, want 1", n)
	}
	if n, _ := st.PendingVectorCount(ctx); n != 1 {
		t.Fatalf("pending = %d, want 1", n)
	}
	if n, _ := st.SessionCount(ctx); n != 1 {
		t.Fatalf("sessions = %d, want 1", n)
	}

	// Backfill computes the missing vector.
	n, err := st.BackfillVectors(ctx, fakeEmbedder{dim: 4}, 0)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 1 {
		t.Fatalf("backfilled = %d, want 1", n)
	}
	if n, _ := st.PendingVectorCount(ctx); n != 0 {
		t.Fatalf("pending after backfill = %d, want 0", n)
	}
	if n, _ := st.VectorCount(ctx); n != 2 {
		t.Fatalf("vectors after backfill = %d, want 2", n)
	}

	// Replacing with fewer chunks must remove the stale ones and their vectors.
	chunks = chunks[:1]
	if err := st.ReplaceSessionChunks(ctx, chunks, texts, vectors); err != nil {
		t.Fatalf("replace again: %v", err)
	}
	if n, _ := st.ChunkCount(ctx); n != 1 {
		t.Fatalf("chunks after replace = %d, want 1", n)
	}
	if n, _ := st.VectorCount(ctx); n != 1 {
		t.Fatalf("vectors after replace = %d, want 1", n)
	}
}

func TestNeedsIndexing(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, 4)

	// Unknown session.
	need, err := st.NeedsIndexing(ctx, "s1", 100)
	if err != nil || !need {
		t.Fatalf("new session: need=%v err=%v", need, err)
	}

	if err := st.SetSyncState(ctx, "s1", 100, "indexed", ""); err != nil {
		t.Fatalf("set state: %v", err)
	}
	if need, _ := st.NeedsIndexing(ctx, "s1", 100); need {
		t.Fatal("unchanged session should not need indexing")
	}
	if need, _ := st.NeedsIndexing(ctx, "s1", 200); !need {
		t.Fatal("newer session should need indexing")
	}

	if err := st.SetSyncState(ctx, "s1", 100, "error", "boom"); err != nil {
		t.Fatalf("set error state: %v", err)
	}
	if need, _ := st.NeedsIndexing(ctx, "s1", 100); !need {
		t.Fatal("failed session should be retried")
	}
	pending, err := st.PendingSessions(ctx)
	if err != nil || len(pending) != 1 || pending[0] != "s1" {
		t.Fatalf("pending = %v, err=%v", pending, err)
	}
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, 4)
	if err := st.UpsertSession(ctx, SessionMeta{ID: "s1", TimeUpdated: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSyncState(ctx, "s1", 1, "indexed", ""); err != nil {
		t.Fatal(err)
	}
	s, err := st.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if s.Sessions != 1 || s.EmbedDim != 4 || s.EmbedModel != "test-model" {
		t.Fatalf("unexpected status: %+v", s)
	}
	if s.LastSync == 0 {
		t.Fatal("last sync should be set")
	}
}

func TestMetaValue(t *testing.T) {
	st := openTestStore(t, 8)
	v, ok, err := st.MetaValue(context.Background(), "embed_dim")
	if err != nil || !ok || v != "8" {
		t.Fatalf("embed_dim = %q ok=%v err=%v", v, ok, err)
	}
}

func TestMetaFromSession(t *testing.T) {
	s := &extract.Session{ID: "s1", ProjectID: "p", Title: "t", Agent: "build", TimeUpdated: 9, Compacted: true, TailStartID: "prt_x"}
	m := MetaFromSession(s, "/work", 3)
	if m.ID != "s1" || m.ProjectPath != "/work" || m.PartsCount != 3 || !m.Compacted || m.TailStartID != "prt_x" {
		t.Fatalf("unexpected meta: %+v", m)
	}
}
