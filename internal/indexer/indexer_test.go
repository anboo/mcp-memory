package indexer

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/anboo/mcp-memory/internal/bleveidx"
	"github.com/anboo/mcp-memory/internal/extract"
	"github.com/anboo/mcp-memory/internal/search"
	"github.com/anboo/mcp-memory/internal/store"

	_ "modernc.org/sqlite"
	_ "modernc.org/sqlite/vec"
)

type fakeEmbedder struct{ dim int }

func (f fakeEmbedder) Embed(_ context.Context, texts []string, _ int) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		v := make([]float32, f.dim)
		v[0] = 1
		out[i] = v
	}
	return out, nil
}

// newFakeSource builds a minimal opencode.db with one project and one session
// containing a text and a tool part.
func newFakeSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	ddl := []string{
		`CREATE TABLE project (id TEXT PRIMARY KEY, worktree TEXT, vcs TEXT, name TEXT)`,
		`CREATE TABLE session (id TEXT PRIMARY KEY, project_id TEXT, slug TEXT, directory TEXT,
			title TEXT, version TEXT, agent TEXT, model TEXT, time_created INTEGER,
			time_updated INTEGER, time_archived INTEGER)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER,
			time_updated INTEGER, data TEXT)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT,
			time_created INTEGER, time_updated INTEGER, data TEXT)`,
	}
	for _, q := range ddl {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	stmt := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO project (id, worktree, vcs, name) VALUES ('p1', '/work/demo', 'git', 'demo')`, nil},
		{`INSERT INTO session (id, project_id, slug, directory, title, version, agent, model,
			time_created, time_updated, time_archived)
			VALUES ('s1','p1','demo','/work/demo','redis troubleshooting','0.1','build','{}', 100, 100, NULL)`, nil},
		{`INSERT INTO message (id, session_id, time_created, time_updated, data)
			VALUES ('m1','s1',1,1,'{"role":"user"}')`, nil},
		{`INSERT INTO message (id, session_id, time_created, time_updated, data)
			VALUES ('m2','s1',2,2,'{"role":"assistant"}')`, nil},
		{`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
			VALUES ('prt_1','m1','s1',1,1,'{"type":"text","text":"why Redis Cluster CLUSTERDOWN" }')`, nil},
		{`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
			VALUES ('prt_2','m2','s1',2,2,'{"type":"text","text":"check node two state" }')`, nil},
		{`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
			VALUES ('prt_3','m2','s1',2,2,'{"type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"redis-cli info"},"output":"cluster_state:fail"}}')`, nil},
	}
	for _, s := range stmt {
		if _, err := db.Exec(s.q, s.args...); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}
	return path
}

func testReporter() *Reporter {
	return NewReporter(slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond)
}

// bumpSource updates a session's time_updated so the next run considers it
// changed. It opens the source read-write, unlike production code.
func bumpSource(t *testing.T, path, sessionID string, timeUpdated int64) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open source rw: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE session SET time_updated = ? WHERE id = ?`, timeUpdated, sessionID); err != nil {
		t.Fatalf("update session: %v", err)
	}
}

func TestRunIndexesSQLiteAndBleve(t *testing.T) {
	ctx := context.Background()
	srcPath := newFakeSource(t)
	src, err := extract.Open(srcPath)
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer src.Close()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "memory.db"), 4, "test-model")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	b, err := bleveidx.Create(filepath.Join(t.TempDir(), "index.bleve"), true)
	if err != nil {
		t.Fatalf("create bleve: %v", err)
	}
	defer b.Close()

	stats, err := Run(ctx, Config{
		Source:   src,
		Store:    st,
		Bleve:    b,
		Embed:    fakeEmbedder{dim: 4},
		Progress: testReporter(),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Indexed != 1 || stats.Failed != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if stats.Chunks != 3 {
		t.Fatalf("chunks = %d, want 3", stats.Chunks)
	}
	if n, _ := st.ChunkCount(ctx); n != 3 {
		t.Fatalf("store chunks = %d, want 3", n)
	}
	if n, _ := st.VectorCount(ctx); n != 3 {
		t.Fatalf("vectors = %d, want 3 (all parts are embeddable)", n)
	}
	if n, _ := st.PendingVectorCount(ctx); n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
	docs, err := b.DocCount()
	if err != nil {
		t.Fatalf("bleve doc count: %v", err)
	}
	if docs != 3 {
		t.Fatalf("bleve docs = %d, want 3", docs)
	}

	// A second run is incremental and skips the unchanged session.
	stats, err = Run(ctx, Config{Source: src, Store: st, Bleve: b, Embed: fakeEmbedder{dim: 4}, Progress: testReporter()})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if stats.Indexed != 0 || stats.Skipped != 1 {
		t.Fatalf("second run should skip: %+v", stats)
	}

	// Touch the session: it is reindexed and its old Bleve documents are
	// replaced rather than duplicated.
	bumpSource(t, srcPath, "s1", 300)
	stats, err = Run(ctx, Config{Source: src, Store: st, Bleve: b, Embed: fakeEmbedder{dim: 4}, Progress: testReporter()})
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if stats.Indexed != 1 {
		t.Fatalf("changed session should be reindexed: %+v", stats)
	}
	if docs, _ := b.DocCount(); docs != 3 {
		t.Fatalf("bleve docs after reindex = %d, want 3", docs)
	}

	// Search across both lexical sources still returns the chunk.
	searcher := search.New(st, nil, b)
	res, err := searcher.Search(ctx, search.Query{Text: "CLUSTERDOWN", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Results) == 0 {
		t.Fatal("expected a search hit")
	}
}

func TestRunWithoutEmbedderLeavesVectorsPending(t *testing.T) {
	ctx := context.Background()
	src, err := extract.Open(newFakeSource(t))
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	defer src.Close()

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "memory.db"), 4, "test-model")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	stats, err := Run(ctx, Config{Source: src, Store: st, Progress: testReporter()})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.Indexed != 1 || stats.Embedded != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if n, _ := st.PendingVectorCount(ctx); n != 3 {
		t.Fatalf("pending = %d, want 3", n)
	}
	if n, _ := st.VectorCount(ctx); n != 0 {
		t.Fatalf("vectors = %d, want 0", n)
	}

	// FTS must work even without vectors.
	searcher := search.New(st, nil, nil)
	res, err := searcher.Search(ctx, search.Query{Text: "CLUSTERDOWN", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Results) == 0 {
		t.Fatal("FTS-only path returned no results")
	}
	if res.Sources == nil {
		t.Fatal("sources must be reported")
	}
}
