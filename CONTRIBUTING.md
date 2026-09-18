# Contributing to opencode-rag

This document is for people changing this repository. It describes how the
system is put together, then how to build, test and run it.

## What this is

`opencode-rag` is agent memory over the full OpenCode session history.

OpenCode stores every session, message and message part in a single SQLite
database (`~/.local/share/opencode/opencode.db`). After a session is compacted,
the early context disappears from the model window. This project indexes the
history into a local index (`memory.db` plus an optional Bleve directory) and
gives the agent tools to search it and read the originals.

The main principle: **the MCP server stays dumb**. It exposes cheap primitives
(search and read); the current model decides what to search and how to reason.

## Architecture

```
            opencode.db (SQLite, read-only)
                       |
                       v
            +---------------------+
            |  indexer (Go)       |  full / incremental sync
            |  extract + chunk    |
            |  + embed (optional) |
            +----+-----------+----+
                 |           |
      same chunk stream      v
                 |     +---------------------+
                 |     | Bleve secondary     |
                 |     | lexical index       |
                 |     +---------------------+
                 v
      +-------------------------------+
      | memory.db (SQLite)            |
      |  chunks + FTS5 + sqlite-vec   |
      +---------------+---------------+
                      |
           +----------+----------+
           |                     |
           v                     v
    MCP server (Go)         memory_read:
    memory_search           reads originals
    memory_session          from opencode.db
    memory_context          (read-only)
    memory_status
```

Two stores are used on purpose:

- **`memory.db` (SQLite)** - the search index: chunks, FTS5 tables, sqlite-vec
  vectors, metadata and sync state. Fully local, no server.
- **`opencode.db` (read-only)** - the source of originals. `memory_read`
  returns full texts and command output, not truncated snippets.

## Repository layout

```
cmd/
  indexer/main.go        indexing and diagnostics
  mcp/main.go            MCP server (stdio)
internal/
  config/                configuration from env
  extract/               read SQLite: sessions, messages, parts, coordinates
  chunk/                 part -> chunk policy
  stem/                  Snowball-russian stemming and FTS match building
  embed/                 HTTP embedding client: batches, retries, disk cache
  migrate/               embedded migration runner, vec0 dimension handling
  store/                 SQLite repository: chunks, FTS, vectors, sync_state
  search/                hybrid search: FTS + vector + Bleve, RRF merge
  bleveidx/              Bleve secondary lexical index
  indexer/               indexing pipeline and progress reporting
  mcp/                   MCP tools: search/read/session/context/status
migrations/              numbered SQL migrations, embedded with go:embed
storage/                 embedding cache (not in git)
research/                research spike (gitignored, separate Go module)
```

## Requirements

- Go 1.26+
- Optional: an embedding server with the Ollama `/api/embed` endpoint
  (for example Ollama with `bge-m3`)

No CGO and no PostgreSQL are required.

## Build and test

```bash
go build ./...
go vet ./...
go test ./...
```

The tests use temporary SQLite files, a fake embedder and a temporary Bleve
index. They need no network, no database and no Ollama. Verify the CGO-free
build explicitly:

```bash
CGO_ENABLED=0 go build ./cmd/indexer ./cmd/mcp
```

### Running against a real opencode.db

```bash
# Diagnostics first: what is in the source database?
go run ./cmd/indexer --all
go run ./cmd/indexer --session ses_xxx

# Index a few sessions into a throwaway index.
MEMORY_DB=/tmp/memory.db MEMORY_BLEVE=/tmp/memory.bleve \
  go run ./cmd/indexer --index --limit 3

# Run the MCP server against that index.
MEMORY_DB=/tmp/memory.db MEMORY_BLEVE=/tmp/memory.bleve \
  go run ./cmd/mcp
```

`opencode.db` is opened with `mode=ro`. Never write to it, not even in a tool
test. The index file is the only writable artifact.

## Conventions

1. Do not break the read-only guarantee on `opencode.db`: no writes, ever.
2. Do not add LLM calls to the MCP server. It must stay a set of primitives.
3. Search must keep working without embeddings and without Bleve. New sources
   are optional on top of the SQLite FTS baseline and must degrade gracefully.
4. Comments, log messages, error strings and docs are in English. Use ASCII
   punctuation only: hyphen `-`, straight quotes, three periods. No em dashes,
   smart quotes or other Unicode typography.
5. Keep dependencies minimal and the production path CGO-free.
6. Write tests for new behavior. Prefer real temporary SQLite files and fakes
   over mocks of internal types.

## Migrations

Schema changes go into a new numbered file under `migrations/`, for example
`migrations/0002_add_foo.sql`. The runner applies files in numeric order, one
transaction each, and records progress in `PRAGMA user_version`.

Rules:

1. Never edit an already applied migration; add a new one.
2. Migrations must be idempotent on re-apply (the runner skips applied
   versions, but write statements defensively with `IF NOT EXISTS`).
3. The `vec0` table dimension depends on `MEMORY_EMBED_DIM` and is created by
   `migrate.EnsureVectorTable`, not by a static migration. The chosen dimension
   is stored in the `meta` table; a mismatch must fail loudly, never silently.
4. Add or update tests in `internal/migrate` when the runner or dimension
   handling changes.

## Internals worth knowing

### Incremental indexing

`store.NeedsIndexing` returns true when:

- there is no `sync_state` row (new session);
- the status is `error` (retry a failed session);
- `session.time_updated` is newer than the last indexed value (changed).

`indexSession` writes chunks, vectors and metadata, then sets the status.
A failed embedding is not fatal: the FTS rows are written and the vectors stay
pending, so the next run can backfill them.

### Concurrent SQLite + Bleve indexing

The main goroutine chunks a session once and writes it to SQLite. The same
`[]chunk.Chunk` is sent over a small buffered channel to a Bleve goroutine that
deletes the session's old documents and adds the new ones. Chunking happens
exactly once. A Bleve error is counted and reported but never stops the SQLite
path.

Progress is logged with `log/slog` to stderr: a header, a line per indexed or
failed session, a periodic summary (every 15 seconds) and a final summary with
counts, rate and ETA.

### Chunking policy

| Type | content | embedding | FTS | snippet |
|---|---|---|---|---|
| `text` | `TrimSpace(Text)` | yes | yes | 300 runes |
| `tool` | `"tool: cmd\noutput"` | yes | yes | 300 runes |
| `reasoning` | `TrimSpace(Text)` | no | yes | 300 runes |
| `patch` | files joined by `\n` | no | yes | `patch: f1, f2` |
| `step-*` / `compaction` / `file` | - | - | - | - |

Tool output is capped at `MaxToolOutputLen = 4096` runes for `content`.
Embeddings use `EmbedContent`, the first `MaxEmbedContentLen = 6000` runes of
`content`; FTS sees the full content. `sanitizeUTF8` replaces broken UTF-8 with
U+FFFD and removes control characters (tab, newline and carriage return are
kept).

### Search and degradation

`internal/search` builds up to four candidate lists (raw FTS, stemmed FTS,
vector KNN, Bleve), applies filters before the merge and combines them with
RRF (`k = 60`), deduplicating by chunk id. The result lists the contributing
sources and sets `degraded` when the vector or Bleve source is absent. The call
fails only when every source fails.

### Coordinates

`position` is computed by ordering parts on `(message.time_created, part.rowid)`.
Parts of one message can share `time_created`, so `rowid` is the only stable
tie breaker. The index stores the coordinate; `memory_read` recomputes it from
the source so search and read always agree.

### Compaction detection

`extract.MarkCompacted` looks for a `compaction` part, parses
`tail_start_id` and sets `Session.Compacted`. The compaction part itself is not
indexed. This lets the agent know that history before `tail_start_id` was
compacted but is still searchable.

## Commit and review rules

1. Keep commits short and focused, matching the repository style.
2. Do not commit `storage/`, `bin/`, `research/` or local notes.
3. Do not introduce Cyrillic into production code, migrations or docs. Russian
   is allowed only as test data for the Russian analyzers.
