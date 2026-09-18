# AGENTS.md

Instructions for coding agents working in this repository.

## What this repo is

`opencode-rag` turns the full OpenCode session history into a local search
index and exposes it through an MCP server. Production retrieval is a single
local SQLite file (`memory.db`) with FTS5 plus sqlite-vec, an optional Bleve
secondary lexical index, and an optional HTTP embedder. There is no PostgreSQL
in the default path.

The `research/` directory is a gitignored spike with its own Go module. Do not
build it, import it from production code, or rely on it. It is not part of the
shipped product.

## Hard rules

1. `opencode.db` is read-only. Never write to it. It is opened with
   `mode=ro&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)` in
   `internal/extract/db.go`. Any change that writes to it is a bug.
2. Only `MEMORY_DB` (`memory.db`) is a writable artifact.
3. Production code must build with `CGO_ENABLED=0`. Do not enable Bleve's
   vector/FAISS path.
4. English only in code comments, log messages, error strings and docs. ASCII
   punctuation only: hyphen `-`, straight quotes, three periods. No em dashes,
   smart quotes or other Unicode typography.
5. Do not add LLM calls to the MCP server.
6. Search must keep working with FTS only (no embedder, no Bleve) and must
   report degradation in the response.

## Exact commands

Root module (all of these must pass):

```bash
go build ./...
go vet ./...
go test ./...

# CGO-free production binaries:
CGO_ENABLED=0 go build -o bin/indexer ./cmd/indexer
CGO_ENABLED=0 go build -o bin/mcp ./cmd/mcp
```

Run a single package's tests:

```bash
go test ./internal/store/ ./internal/search/ ./internal/migrate/ ./internal/indexer/ ./internal/mcp/
```

Research module (optional, separate, gitignored):

```bash
cd research && go build ./...
```

## How to run it locally

```bash
# Diagnostics against the real database.
go run ./cmd/indexer --all
go run ./cmd/indexer --session ses_xxx

# Index a few sessions into a throwaway index (never touches opencode.db).
MEMORY_DB=/tmp/memory.db MEMORY_BLEVE=/tmp/memory.bleve \
  go run ./cmd/indexer --index --limit 3

# Serve MCP over stdio against that index.
MEMORY_DB=/tmp/memory.db MEMORY_BLEVE=/tmp/memory.bleve \
  go run ./cmd/mcp
```

The MCP server speaks JSON-RPC over stdio: send `initialize`, `tools/list`,
`tools/call`. It starts even when the index is empty or the optional sources
are unavailable.

## Layout

```
cmd/indexer/       indexing CLI and diagnostics
cmd/mcp/           MCP server over stdio
internal/config/   environment configuration
internal/extract/  read opencode.db (read-only): sessions, messages, parts
internal/chunk/    part -> chunk policy
internal/stem/     Snowball-russian stemming and FTS match building
internal/embed/    Ollama /api/embed client with batching, retries, disk cache
internal/migrate/  embedded migration runner and vec0 dimension handling
internal/store/    SQLite repository (chunks, FTS5, vec0, sync_state, status)
internal/search/   hybrid retrieval: FTS + vector + Bleve, RRF merge k=60
internal/bleveidx/ Bleve secondary lexical index (CGO-free, no vector path)
internal/indexer/  indexing pipeline, concurrent Bleve goroutine, progress
internal/mcp/      MCP tools: search/read/session/context/status
migrations/        numbered .sql files, embedded via go:embed
```

## Migrations

- Add a new numbered file `migrations/NNNN_name.sql`. Never edit an applied
  migration.
- The runner `internal/migrate` applies files in numeric order, one
  transaction each, and stores progress in `PRAGMA user_version`.
- `sqlite-vec`'s `vec0` table dimension is fixed at creation. It is created by
  `migrate.EnsureVectorTable` from `MEMORY_EMBED_DIM`; the dimension is stored
  in the `meta` table. A mismatch must fail with a clear error telling the user
  to reindex; never silently change it.
- Keep migration tests green: apply from empty, idempotent re-apply, dimension
  mismatch.

## Testing conventions

- Use temporary SQLite files (`t.TempDir()`), a fake embedder and a temporary
  Bleve index. No network, no Ollama.
- The source fixture for indexer tests must create the SQLite database in WAL
  mode before `extract.Open` reads it, otherwise the read-only connection fails
  with "attempt to write a readonly database".
- Prefer exercising public interfaces over mocking internals.

## Definition of done

- `go build ./...`, `go vet ./...`, `go test ./...` pass.
- `CGO_ENABLED=0` build works.
- No writes to `opencode.db`.
- New retrieval sources degrade gracefully and are reported in `sources`.
- Docs updated when behavior, configuration or commands change.
