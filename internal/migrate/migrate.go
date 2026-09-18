// Package migrate applies the embedded SQL migrations to the local SQLite
// index and manages the dimension-dependent vector table.
//
// Migrations are plain .sql files named <number>_<name>.sql. The runner
// applies them in numeric order inside a single transaction each and records
// progress in PRAGMA user_version, so re-running is a no-op and no external
// migration library is needed.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

type migration struct {
	version int
	name    string
	body    string
}

// Apply runs all pending migrations from fsys against db.
//
// Apply is idempotent: migrations whose version is already recorded in
// PRAGMA user_version are skipped.
func Apply(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	migs, err := load(fsys)
	if err != nil {
		return err
	}
	current, err := userVersion(ctx, db)
	if err != nil {
		return err
	}
	for _, m := range migs {
		if m.version <= current {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return err
		}
		current = m.version
	}
	return nil
}

// Version returns the current schema version stored in PRAGMA user_version.
func Version(ctx context.Context, db *sql.DB) (int, error) {
	return userVersion(ctx, db)
}

func load(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read migrations: %w", err)
	}
	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		v, err := parseVersion(e.Name())
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrate: duplicate version %d: %s and %s", v, prev, e.Name())
		}
		seen[v] = e.Name()

		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", e.Name(), err)
		}
		out = append(out, migration{version: v, name: e.Name(), body: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func parseVersion(name string) (int, error) {
	i := strings.IndexByte(name, '_')
	if i <= 0 {
		return 0, fmt.Errorf("migrate: bad migration name %q: want <number>_<name>.sql", name)
	}
	v, err := strconv.Atoi(name[:i])
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("migrate: bad migration version in %q: want a positive integer", name)
	}
	return v, nil
}

func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: begin %s: %w", m.name, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, m.body); err != nil {
		return fmt.Errorf("migrate: apply %s: %w", m.name, err)
	}
	// PRAGMA values cannot be bound, but the version is a validated integer.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("migrate: set user_version %d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate: commit %s: %w", m.name, err)
	}
	return nil
}

func userVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("migrate: read user_version: %w", err)
	}
	return v, nil
}

// EnsureVectorTable creates the sqlite-vec vec0 table for dim if it does not
// exist yet and records the dimension in meta.
//
// The dimension of a vec0 table is fixed at creation time, so it cannot be
// changed in place. If meta already records a different dimension the call
// fails with an actionable error instead of silently mixing vector spaces.
func EnsureVectorTable(ctx context.Context, db *sql.DB, dim int, model string) error {
	if dim <= 0 {
		return fmt.Errorf("migrate: embed dimension must be positive, got %d", dim)
	}
	stored, ok, err := metaValue(ctx, db, "embed_dim")
	if err != nil {
		return err
	}
	if ok {
		n, err := strconv.Atoi(stored)
		if err != nil {
			return fmt.Errorf("migrate: stored embed_dim %q is not a number", stored)
		}
		if n != dim {
			return fmt.Errorf(
				"migrate: vector dimension mismatch: index was built with embed_dim=%d but MEMORY_EMBED_DIM=%d; "+
					"use a fresh MEMORY_DB or set MEMORY_EMBED_DIM=%d and reindex", n, dim, n)
		}
	}

	ddl := fmt.Sprintf("CREATE VIRTUAL TABLE IF NOT EXISTS vec_chunks USING vec0(embedding float[%d])", dim)
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("migrate: create vec_chunks: %w", err)
	}
	if !ok {
		if err := setMeta(ctx, db, "embed_dim", strconv.Itoa(dim)); err != nil {
			return err
		}
	}
	if model != "" {
		if err := setMeta(ctx, db, "embed_model", model); err != nil {
			return err
		}
	}
	return nil
}

func metaValue(ctx context.Context, db *sql.DB, key string) (string, bool, error) {
	var v string
	err := db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("migrate: read meta %s: %w", key, err)
	}
	return v, true, nil
}

func setMeta(ctx context.Context, db *sql.DB, key, value string) error {
	_, err := db.ExecContext(ctx,
		"INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value)
	if err != nil {
		return fmt.Errorf("migrate: write meta %s: %w", key, err)
	}
	return nil
}
