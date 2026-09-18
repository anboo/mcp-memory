package migrate

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"opencode-rag/migrations"

	_ "modernc.org/sqlite"
	_ "modernc.org/sqlite/vec"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/index.db?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestApplyFromEmpty(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	if err := Apply(ctx, db, migrations.FS); err != nil {
		t.Fatalf("apply: %v", err)
	}
	v, err := Version(ctx, db)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if v != 1 {
		t.Fatalf("user_version = %d, want 1", v)
	}
	for _, name := range []string{"chunks", "chunks_ai", "sessions", "sync_state", "meta", "fts_unicode", "fts_stem"} {
		var n int
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n)
		if err != nil {
			t.Fatalf("lookup %s: %v", name, err)
		}
		if n == 0 {
			t.Fatalf("object %s was not created", name)
		}
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)

	if err := Apply(ctx, db, migrations.FS); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := Apply(ctx, db, migrations.FS); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	v, _ := Version(ctx, db)
	if v != 1 {
		t.Fatalf("user_version = %d after re-apply, want 1", v)
	}

	// FTS triggers must still work after a re-apply.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO chunks (id, session_id, project_path, part_type, content, snippet, position, time_created, time_updated, stemmed)
		 VALUES ('c1','s1','/p','text','hello world','hello',0,1,1,'hello world')`); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fts_unicode WHERE fts_unicode MATCH 'hello'`).Scan(&n); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if n != 1 {
		t.Fatalf("fts rows = %d, want 1", n)
	}
}

func TestEnsureVectorTable(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	if err := Apply(ctx, db, migrations.FS); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if err := EnsureVectorTable(ctx, db, 8, "test-model"); err != nil {
		t.Fatalf("create vector table: %v", err)
	}
	var dim string
	if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'embed_dim'`).Scan(&dim); err != nil {
		t.Fatalf("read dim: %v", err)
	}
	if dim != "8" {
		t.Fatalf("embed_dim = %q, want 8", dim)
	}

	// Same dimension is fine and must not recreate or error.
	if err := EnsureVectorTable(ctx, db, 8, "test-model"); err != nil {
		t.Fatalf("re-ensure same dim: %v", err)
	}

	// Different dimension must fail with an actionable message.
	err := EnsureVectorTable(ctx, db, 16, "test-model")
	if err == nil {
		t.Fatal("expected a dimension mismatch error")
	}
	if !strings.Contains(err.Error(), "mismatch") || !strings.Contains(err.Error(), "16") {
		t.Fatalf("unhelpful mismatch error: %v", err)
	}

	// Non-positive dimensions are rejected.
	if err := EnsureVectorTable(ctx, db, 0, ""); err == nil {
		t.Fatal("expected an error for dim 0")
	}
}

func TestParseVersion(t *testing.T) {
	if v, err := parseVersion("0001_init.sql"); err != nil || v != 1 {
		t.Fatalf("parse 0001_init.sql = %d, %v", v, err)
	}
	if _, err := parseVersion("init.sql"); err == nil {
		t.Fatal("expected an error for a name without a numeric prefix")
	}
	if _, err := parseVersion("0000_noop.sql"); err == nil {
		t.Fatal("expected an error for version 0")
	}
}
