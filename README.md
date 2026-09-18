# mcp-memory

Agent memory over the full OpenCode session history.

OpenCode stores every session, message and message part in a single SQLite
database (`~/.local/share/opencode/opencode.db`). Once a session is compacted,
the earlier context is gone from the model window. This project indexes that
history into a local search index and exposes it through MCP, so an agent can
ask questions like:

> What did we already try with Redis Cluster in this project, why did the first
> approach fail, and what did we end up with?

and reconstruct a coherent answer from its own past work.

The design principle is simple: **the MCP server stays dumb**. It exposes cheap
search and read primitives; the current model decides what to look up, reads the
original context, and does the reasoning. Nothing is pre-summarized.

## Architecture

```
              OpenCode SQLite (read-only)
                        |
                        v
              +---------------------+
              |  indexer (Go)       |  full or incremental sync
              |  extract + chunk    |
              |  + embed (optional) |
              +----+-----------+----+
                   |           |
        same chunk stream      |
                   |           v
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
             |
             v
      OpenCode agent (the current model decides what to search)
```

Two stores are used on purpose:

- **`memory.db` (SQLite)** - the search index: FTS5 full text, sqlite-vec
  vectors, metadata and coordinates. A single local file, no server.
- **`opencode.db` (read-only)** - the source of originals. `memory_read`
  returns full texts and command output, not truncated index snippets.

The optional **Bleve** index adds a stronger lexical layer for Russian natural
language. It can be absent: search then degrades to SQLite only.

## Requirements

- To use the npm wrapper: Node 18+ (for `npx`). No Go needed.
- To build from source: Go 1.26+.
- Optional: an embedding server exposing the Ollama `/api/embed` endpoint
  (for example Ollama with `bge-m3`). Without it the index is FTS-only and
  vectors stay pending until the server is available.

No CGO and no external database are required. `CGO_ENABLED=0` works.

## Quick start

### 1. Install with one command

```bash
npx -y @anboo/mcp-memory install
```

This downloads the prebuilt CGO-free binary for your platform, caches it under
`~/.cache/mcp-memory/`, and adds the `memory` MCP server to your
global `opencode.json(c)`. It never overwrites an existing `memory` entry
unless you pass `--force`. Restart OpenCode afterwards.

Then build the index once (see step 3):

```bash
npx -y @anboo/mcp-memory index
```

`npx` resolves the latest published version, so upgrades are automatic. If you
do not want Node, download the archive for your platform from the GitHub
Releases page (it contains a single binary, `mcp-memory`), or build
from source in step 2.

### 2. Build from source (alternative)

```bash
go build -o bin/mcp-memory ./cmd/mcp-memory
```

One binary contains everything: the MCP server and the indexer are subcommands.

### 3. Index the history

The examples use the npm wrapper; for a local build replace
`npx -y @anboo/mcp-memory` with `bin/mcp-memory`.

```bash
# Zero-config local mode. MEMORY_SQLITE defaults to the standard OpenCode path.
npx -y @anboo/mcp-memory index

# Index only a few sessions (useful for a first try).
npx -y @anboo/mcp-memory index --limit 3

# Index one project only.
npx -y @anboo/mcp-memory index --project /var/www/my-repo
```

Re-running `index` is incremental: a session is re-indexed only when its
`time_updated` is newer than the last sync, or its previous attempt failed.

Useful commands and flags:

```bash
npx -y @anboo/mcp-memory sessions        # whole database summary (diagnostics)
npx -y @anboo/mcp-memory session ses_xxx # dump one session dialog (diagnostics)
npx -y @anboo/mcp-memory index --limit 10          # cap the number of sessions
npx -y @anboo/mcp-memory index --no-bleve          # skip the Bleve index
npx -y @anboo/mcp-memory index --embed-pause 150ms # throttle embedding batches
```

### 4. Start the embedding server (optional)

```bash
ollama serve
ollama pull bge-m3
```

When Ollama is not running, the indexer logs a warning, writes the FTS rows
and leaves vectors pending. A later run with the embedder available backfills
them.

### 5. Run the MCP server

The server speaks MCP over stdio. `install` writes this entry for you; it is
shown here for reference, both for the npm wrapper and for a locally built
binary:

```json
{
  "mcp": {
    "memory": {
      "type": "local",
      "command": ["npx", "-y", "@anboo/mcp-memory"],
      "enabled": true,
      "timeout": 20000
    }
  }
}
```

```json
{
  "mcp": {
    "memory": {
      "type": "local",
      "command": ["/var/www/mcp-memory/bin/mcp-memory", "serve"],
      "enabled": true
    }
  }
}
```

The npm command launches `serve` for you; the local binary uses the explicit
`serve` subcommand. All path and embedder settings come from the environment
below and have defaults, so the entry can be minimal. If you need non-default
paths, add an `environment` block:

```json
"environment": {
  "MEMORY_SQLITE": "/home/you/.local/share/opencode/opencode.db",
  "MEMORY_DB": "/home/you/.local/share/opencode/memory.db",
  "MEMORY_BLEVE": "/home/you/.local/share/opencode/memory.bleve",
  "MEMORY_EMBED_URL": "http://localhost:11434",
  "MEMORY_EMBED_DIM": "1024"
}
```

## Configuration

All configuration comes from environment variables.

| Variable            | Default                                          | Description                                      |
|---------------------|--------------------------------------------------|--------------------------------------------------|
| `MEMORY_SQLITE`     | `~/.local/share/opencode/opencode.db`            | Path to the OpenCode database (read-only)        |
| `MEMORY_DB`         | `~/.local/share/opencode/memory.db`              | Path to the SQLite index                          |
| `MEMORY_BLEVE`      | `~/.local/share/opencode/memory.bleve`           | Path to the Bleve index directory                |
| `MEMORY_EMBED_URL`  | `http://localhost:11434`                         | Base URL of the embedding server                 |
| `MEMORY_EMBED_DIM`  | `1024`                                           | Embedding vector dimension                       |
| `MEMORY_EMBED_MODEL`| `bge-m3`                                         | Embedding model name                             |
| `MEMORY_EMBED_CACHE`| `./storage/embeddings`                           | On-disk embedding cache                          |

`MEMORY_EMBED_DIM` fixes the width of the `vec0` vector table. Changing it
after indexing fails with an explicit error; use a fresh `MEMORY_DB` (or set
the old dimension back) and reindex.

## MCP tools

All tools are cheap and contain no LLM calls.

### `memory_search`

Hybrid search over the whole history. Returns snippets with `session_id` and
`position` for follow-up reads, plus a `sources` list and a `degraded` flag.

Arguments: `query` (required), `project`, `type`
(`text|tool|patch|reasoning`), `time_from` (epoch ms), `limit` (default 20,
max 50).

### `memory_read`

Random access into the original history: a window of parts around a coordinate.
Returns full text and command output, not snippets.

Arguments: `session_id` (required), `position` (required), `before` (default 5),
`after` (default 10). The window is capped at 50 parts and tool output at 16 KiB
per part.

### `memory_session`

Session metadata plus a map of messages and their part types. No full texts, so
the agent can see which topics a session covered before reading it.

### `memory_context`

The same window as `memory_read`, but addressed by `part_id` (`prt_xxx`)
instead of `(session_id, position)`.

### `memory_status`

Index health: session/chunk/vector counts, pending vectors, embedding model and
dimension, last sync time, Bleve document count, and which retrieval sources are
currently available.

Typical research loop:

```
memory_search(query)          -> snippets + coordinates + sources
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

Four candidate lists are produced and merged with reciprocal rank fusion
(RRF, `k = 60`):

- FTS5 `unicode61` over raw content;
- FTS5 over a Snowball-russian stemmed copy of the content;
- sqlite-vec `vec0` exact KNN over embeddings (optional);
- Bleve lexical search (optional secondary index).

Filters by project, part type and time are applied before the merge.

Graceful degradation:

| Situation                          | Behavior                                        |
|------------------------------------|-------------------------------------------------|
| Embedder unavailable               | FTS + Bleve only; vectors stay pending          |
| Bleve index missing or locked      | SQLite only                                     |
| Both unavailable                   | FTS only, `degraded: true` in the response      |
| All sources fail                   | Error is returned                               |

The response always lists the sources that contributed, so the model knows how
much to trust the recall.

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

## Migrations

Schema changes live in numbered `.sql` files under `migrations/`, embedded into
the binary with `go:embed` and applied by `internal/migrate` in numeric order,
one transaction each. Progress is tracked in `PRAGMA user_version`, so applying
is idempotent and no migration library is needed.

The `vec0` vector table is created by Go code because its dimension comes from
configuration. The dimension is recorded in the `meta` table. If
`MEMORY_EMBED_DIM` changes after the index was built, startup fails with an
actionable mismatch error instead of silently mixing vector spaces.

To add a migration, create `migrations/0002_<name>.sql`; never edit an applied
migration in place.

## Bleve secondary index

Bleve is a local, CGO-free lexical index. Its mapping uses:

- `content` with the Russian analyzer (morphology, stop words);
- `content_exact` with the simple analyzer (exact identifiers, no stemming);
- stored `session_id`, `position`, `project_path`, `part_type`, `role`, `tool`,
  `time_created`, `snippet` for coordinates and display.

Bleve's own vector path (which requires CGO and a build tag) is intentionally
not used; vectors live in sqlite-vec. The index is optional: if its directory
is missing or locked by another process, search degrades to SQLite only. The
indexer creates and updates it; the MCP server only opens it.

## Project layout

```
cmd/
  mcp-memory/   # the single binary; dispatches to subcommands
internal/
  cli/                   # subcommands: serve (MCP) and index/sessions/session
  config/                # env configuration
  extract/               # read opencode.db: sessions, messages, parts, coordinates
  chunk/                 # part -> chunk policy
  stem/                  # Snowball-russian stemming and FTS match building
  embed/                 # HTTP embedding client, batching, retries, disk cache
  migrate/               # embedded migration runner and vec0 dimension handling
  store/                 # SQLite repository: chunks, FTS, vectors, sync_state
  search/                # hybrid search: FTS + vector + Bleve, RRF merge
  bleveidx/              # Bleve secondary lexical index
  indexer/               # indexing pipeline and progress reporting
  mcp/                   # MCP tools: search/read/session/context/status
  version/               # build version, set at link time
npm/                     # npm wrapper: downloads the release binary, writes config
.github/workflows/       # release workflow: binaries + checksums + npm publish
migrations/              # numbered SQL migrations (embedded)
storage/                 # embedding cache (not in git)
research/                # research spike (gitignored, separate Go module)
```

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

Tests do not require a database, network or Ollama. They use temporary
SQLite files, a fake embedder and a temporary Bleve index. CGO is not needed:

```bash
CGO_ENABLED=0 go build ./cmd/mcp-memory

# npm wrapper tests (config merge, asset naming)
cd npm && npm install && npm test
```

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
   (WAL), so the connection must never write. Only `memory.db` is writable.
