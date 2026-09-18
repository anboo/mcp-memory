# opencode-memory-mcp

Agent memory over the full OpenCode session history, served over MCP (stdio).

This npm package is a thin wrapper. On first run it downloads the prebuilt,
CGO-free Go binary for your platform from the matching GitHub Release, caches
it, and runs it. There is no compiler and no database server involved.

## Install

```bash
npx -y opencode-memory-mcp install
```

`install` downloads the binary and adds the `memory` MCP server to your global
`opencode.json(c)`. Restart OpenCode afterwards.

Then build the index once:

```bash
npx -y opencode-memory-mcp index
```

## Commands

```bash
npx -y opencode-memory-mcp install     # download the binary, write MCP config
npx -y opencode-memory-mcp index       # build or update the local search index
npx -y opencode-memory-mcp serve       # run the MCP server (the MCP default)
npx -y opencode-memory-mcp sessions    # summary of the OpenCode database
npx -y opencode-memory-mcp session ID  # dump one session
npx -y opencode-memory-mcp version
```

An MCP client launches the wrapper with no arguments; it then runs the binary
with `serve`. Nothing else is written to stdout, so the protocol stays clean.

## Environment

| Variable                   | Description                                        |
|----------------------------|----------------------------------------------------|
| `OPENCODE_MEMORY_VERSION`  | Release version to download (default: this package)|
| `OPENCODE_MEMORY_REPO`     | GitHub repo `owner/name` to download from          |
| `OPENCODE_MEMORY_BASE_URL` | Full base URL for release assets (mirror)          |
| `OPENCODE_MEMORY_CACHE`    | Directory to extract the binary into               |
| `OPENCODE_CONFIG`          | `opencode.json` path used by `install`             |

Runtime configuration (database paths, embedder) uses the binary's own
variables: `MEMORY_SQLITE`, `MEMORY_DB`, `MEMORY_BLEVE`, `MEMORY_EMBED_URL`,
`MEMORY_EMBED_DIM`, `MEMORY_EMBED_MODEL`, `MEMORY_EMBED_CACHE`. All have
defaults.

See the repository for the full documentation:
https://github.com/anboo/opencode-memory-mcp
