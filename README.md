# opencode-rag

Agent memory over the full OpenCode session history.

OpenCode stores every session, message and message part in a single SQLite
database (`~/.local/share/opencode/opencode.db`). Once a session is compacted,
the earlier context is gone from the model window. This project makes that
history searchable again through MCP, so an agent can ask questions like:

> What did we already try with Redis Cluster in this project, why did the first
> approach fail, and what did we end up with?

and reconstruct a coherent answer from its own past work.

The design principle is simple: **the MCP server stays dumb**. It exposes cheap
search and read primitives; the current model decides what to look up, reads the
original context, and does the reasoning. Nothing is pre-summarized.

## How it works

```
              OpenCode SQLite (read-only)
                        |
                        v
              +---------------------+
              |  Indexer (Go)       |  full or incremental sync
              |  extract + chunk    |
              |  + embed (HTTP)     |
              +----------+----------+
                         |
                chunks + embeddings
                         |
                         v
              +---------------------+
              | PostgreSQL+pgvector |
              |  chunks (vector+FTS+meta)
              +----------+----------+
                         |
            +------------+------------+
            |                         |
            v                         v
   MCP Server (Go)            memory_read:
   memory_search              reads originals
   memory_session             from opencode.db
   memory_context             (read-only)
            |
            v
      OpenCode agent (the current model decides what to search)
```

Two stores are used on purpose:

- **PostgreSQL + pgvector** - the search index: vectors, full text, metadata
  and coordinates. Returns candidates fast.
- **SQLite `opencode.db` (read-only)** - the source of originals. `memory_read`
  returns full texts and command output, not truncated index snippets.

## Requirements

- Go 1.26+
- Docker (for PostgreSQL with pgvector) or any PostgreSQL 16+ with the
  `vector` extension
- An embedding server exposing the Ollama `/api/embed` endpoint (for example
  Ollama with `bge-m3` pulled)

## Quick start

### 1. Start PostgreSQL

```bash
docker compose up -d
```

This starts `pgvector/pgvector:pg16` on port `5434` with user/password/db all
set to `memory`.

### 2. Apply the schema

```bash
psql "postgres://memory:memory@localhost:5434/memory?sslmode=disable" \
  -f migrations/001_init.sql
```

### 3. Start the embedding server

The indexer and the MCP server both call an HTTP embedding endpoint. With
Ollama:

```bash
ollama serve
ollama pull bge-m3
```

### 4. Build

```bash
go build -o bin/indexer ./cmd/indexer
go build -o bin/mcp ./cmd/mcp
```

### 5. Index the history

```bash
export MEMORY_PG="postgres://memory:memory@localhost:5434/memory?sslmode=disable"
export MEMORY_EMBED_DIM=1024
bin/indexer --index
```

Useful flags:

```bash
bin/indexer --all               # summary of the whole database (diagnostics)
bin/indexer --session ses_xxx   # dump one session dialog (diagnostics)
bin/indexer --index --project /var/www/my-repo   # index one project only
bin/indexer --index --limit 10  # cap the number of sessions this run
```

Re-running `--index` is incremental: a session is re-indexed only when its
`time_updated` is newer than the last sync, or its previous attempt failed.

### 6. Run the MCP server

The server speaks MCP over stdio. Example `opencode.json` entry:

```json
{
  "mcp": {
    "memory": {
      "type": "local",
      "command": ["/var/www/opencode-rag/bin/mcp"],
      "enabled": true,
      "environment": {
        "MEMORY_PG": "postgres://memory:memory@localhost:5434/memory?sslmode=disable",
        "MEMORY_EMBED_DIM": "1024"
      }
    }
  }
}
```

## Configuration

All configuration comes from environment variables.

| Variable           | Default                                          | Description                                  |
|--------------------|--------------------------------------------------|----------------------------------------------|
| `MEMORY_SQLITE`    | `~/.local/share/opencode/opencode.db`            | Path to the OpenCode SQLite database (read-only) |
| `MEMORY_PG`        | (empty)                                          | PostgreSQL DSN. Required for indexing and MCP |
| `MEMORY_EMBED_URL` | `http://localhost:11434`                         | Base URL of the embedding server             |
| `MEMORY_EMBED_DIM` | `768`                                            | Embedding vector dimension                   |

Note: the shipped migration declares `vector(1024)`, which matches `bge-m3`.
Set `MEMORY_EMBED_DIM=1024` to match, otherwise the insert will fail on the
vector column.

## MCP tools

All four tools are cheap and contain no LLM calls.

### `memory_search`

Hybrid semantic + exact search over the whole history. Returns snippets with
coordinates for follow-up reads.

Arguments: `query` (required), `project`, `type`
(`text|tool|patch|reasoning`), `time_from` (epoch ms), `limit` (default 20,
max 50).

### `memory_read`

Random access into the original history: a window of parts around a coordinate.
Returns full text and command output, not snippets.

Arguments: `session_id` (required), `position` (required), `before` (default 5),
`after` (default 10). The total window is capped at 50 parts and tool output is
truncated at 16 KiB per part.

### `memory_session`

Session metadata plus a map of messages and their part types. No full texts, so
the agent can see which topics a session covered before reading it.

Arguments: `session_id` (required).

### `memory_context`

The same window as `memory_read`, but addressed by `part_id` (`prt_xxx`)
instead of `(session_id, position)`.

Arguments: `part_id` (required), `before`, `after`.

Typical research loop:

```
memory_search(query)          -> snippets + coordinates
      |
      +-- need context -> memory_read(session, position, before, after)
      |                        |
      |                        v
      |                   hypothesis -> search again -> read ...
      |
      +-- answer found
```

This is grep, then read a range, then grep again - except the file being
searched is the agent's own history.

## Search model

PostgreSQL provides both vectors (pgvector) and full text (`tsvector`), so
hybrid search runs in a single database.

- Vector search (cosine) catches semantics: "why does Kafka not connect".
- Full-text search catches exact names: `CLUSTERDOWN`, `tree.sql`, `RISKS-123`.
- Filters by project, part type and time are applied in `WHERE` before merging.
- The two result lists are merged in Go with reciprocal rank fusion (RRF,
  `k = 60`), which is robust to differently scaled scores.
- If one source fails, search degrades gracefully to the other.

## What gets indexed

| Part type                       | Indexed | Content                    | Embedding | FTS |
|---------------------------------|---------|----------------------------|-----------|-----|
| `text`                          | yes     | part text                  | yes       | yes |
| `tool`                          | yes     | tool + command + output    | yes       | yes |
| `patch`                         | yes     | file list                  | no        | yes |
| `reasoning`                     | yes     | text                       | no        | yes |
| `step-start` / `step-finish` / `compaction` | no | -                | -         | -   |

Tool output is capped at 4 KiB in `content`; embeddings use at most 6000 runes
of the content, while full-text search sees the whole content. Reasoning is
excluded from embeddings because it is noisy, but kept in FTS because it often
contains exact names and hypotheses.

`position` is a stable per-session coordinate assigned by ordering parts on
`(message time_created, part rowid)`.

## Project layout

```
cmd/
  indexer/main.go        # full / incremental indexing and diagnostics
  mcp/main.go            # MCP server (stdio)
internal/
  config/                # env configuration
  extract/               # read SQLite: sessions, messages, parts, coordinates
  chunk/                 # part -> chunk policy (what to index and how)
  embed/                 # HTTP embedding client, batching, retries, disk cache
  store/                 # pgx repository: chunks, sessions, sync_state
  search/                # hybrid search: vector + FTS, RRF merge
  mcp/                   # MCP tools: search/read/session/context
migrations/              # PostgreSQL schema
storage/                 # embedding cache (not in git)
```

## Database schema

- `chunks` - the search index: one row per indexed part, with `content`,
  `snippet`, `files`, `position`, `time_created`, `embedding vector`, plus a
  GIN index on `to_tsvector('russian', content)` and an HNSW index on the
  embedding.
- `sessions` - session metadata (title, agent, model, project, times,
  compaction info).
- `sync_state` - per-session sync status (`pending | indexed | error`) and the
  last indexed `time_updated`, used for incremental runs.
- `traces` - reserved for MCP call tracing.

## Development

```bash
go test ./...
go vet ./...
```

Most packages have unit tests and do not require a database. The SQLite and
PostgreSQL paths are exercised through their public interfaces with fakes.

## Design notes

1. **No pre-summarization.** Summarizing ahead of time loses information: a
   detail that looks unimportant today may be the answer next month. The raw
   source is kept losslessly.
2. **Relevance is decided at query time.** The current model knows the question,
   so it does the research. The MCP server only provides primitives.
3. **Snippets never replace originals.** `memory_read` returns full text and
   output, otherwise the model cannot see the exact line it needs.
4. **No context auto-injection.** Memory is not injected into every turn. The
   agent gets tools plus a short system prompt and decides when to search.
5. **Read OpenCode's SQLite in read-only mode.** OpenCode keeps writing to it
   (WAL), so the connection must never write.
