# TASKS

Working notes and the roadmap. Everything user-facing is English, ASCII only.

## Now

- MCP host: OpenCode. `mcp-memory serve` speaks JSON-RPC over stdio and is
  launched by OpenCode through the npm wrapper or a directly built binary.
- Storage: SQLite (`memory.db`, FTS5 + sqlite-vec) plus an optional Bleve
  directory. With no embedder the index still serves FTS + Bleve and reports
  degradation.
- Install writes the OpenCode global config only. `OPENCODE_CONFIG` and
  `OPENCODE_CONFIG_DIR` are ignored on purpose (Orca and similar wrappers point
  them at generated hook files). `MCP_MEMORY_CONFIG` is the explicit override.

## Next: other MCP hosts

The binary is host-agnostic; only the install step knows about OpenCode. Plan:

- Add `install --host <name>` that writes the right file and a
  `--print-config` mode that prints the JSON snippet for any client:
  - OpenCode: `~/.config/opencode/opencode.json(c)` (current).
  - Claude Code: user or project `.mcp.json`.
  - Cursor: `~/.cursor/mcp.json`.
  - Generic stdio client: print the command/env block to paste.
- Keep the default behavior (global OpenCode config) unchanged.
- Document manual setup for hosts we do not write.

## Index

- Vectors exist only when the embedder (Ollama `bge-m3` at `MEMORY_EMBED_URL`)
  is reachable. Without it, chunks keep `embedded = 0` and the pending count
  grows; re-running `index` backfills vectors without re-chunking.
- `MEMORY_EMBED_DIM` fixes the `vec0` width. Changing it after indexing fails
  with an explicit mismatch error: use a fresh `MEMORY_DB` or restore the old
  dimension and reindex.
- Full rebuild: stop the MCP server, delete `memory.db` and `memory.bleve`,
  run `index`.
- Default data dir is Linux (`~/.local/share/opencode`). Add macOS and Windows
  defaults, and keep `MEMORY_*` documented as the override.
- Add a `status` CLI subcommand that mirrors the `memory_status` tool.

## Release and infra

- Release is cut by pushing a `v*` tag: the workflow builds six CGO-free
  targets, attaches archives + `checksums.txt` to a GitHub Release, and
  publishes `@devanboo/mcp-memory` to npm at the same version.
- npm auth is currently a granular token `mcp-memory-ci` with bypass 2FA and
  direct publish. It expires 2026-12-17, and npm removes direct publishing with
  bypass-2FA tokens in January 2027. Move to npm Trusted Publishing (OIDC) with
  `id-token: write` and drop the `NPM_TOKEN` secret before then.
- Header version: `-ldflags "-X github.com/anboo/mcp-memory/internal/version.Version=<tag>"`.

## Docs

- README describes the SQLite schema, the Bleve mapping, the full indexing
  pipeline and installation. Keep AGENTS.md and CONTRIBUTING.md in sync when
  commands, layout or configuration change.
