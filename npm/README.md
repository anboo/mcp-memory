# mcp-memory

Agent memory over the full OpenCode session history, served over MCP (stdio).

This npm package is a thin wrapper. On first run it downloads the prebuilt,
CGO-free Go binary for your platform from the matching GitHub Release, caches
it, and runs it. There is no compiler and no database server involved.

## Install

```bash
npx -y @devanboo/mcp-memory install
```

`install` downloads the binary and adds the `memory` MCP server to your global
`opencode.json(c)`. Restart OpenCode afterwards.

Then build the index once:

```bash
npx -y @devanboo/mcp-memory index
```

## Commands

```bash
npx -y @devanboo/mcp-memory install     # download the binary, write MCP config
npx -y @devanboo/mcp-memory index       # build or update the local search index
npx -y @devanboo/mcp-memory serve       # run the MCP server (the MCP default)
npx -y @devanboo/mcp-memory sessions    # summary of the OpenCode database
npx -y @devanboo/mcp-memory session ID  # dump one session
npx -y @devanboo/mcp-memory version
```

An MCP client launches the wrapper with no arguments; it then runs the binary
with `serve`. Nothing else is written to stdout, so the protocol stays clean.

## Environment

| Variable                   | Description                                        |
|----------------------------|----------------------------------------------------|
| `MCP_MEMORY_VERSION`  | Release version to download (default: this package)|
| `MCP_MEMORY_REPO`     | GitHub repo `owner/name` to download from          |
| `MCP_MEMORY_BASE_URL` | Full base URL for release assets (mirror)          |
| `MCP_MEMORY_CACHE`    | Directory to extract the binary into               |
| `MCP_MEMORY_CONFIG`   | Config file used by `install` (default: global)    |

Runtime configuration (database paths, embedder) uses the binary's own
variables: `MEMORY_SQLITE`, `MEMORY_DB`, `MEMORY_BLEVE`, `MEMORY_EMBED_URL`,
`MEMORY_EMBED_DIM`, `MEMORY_EMBED_MODEL`, `MEMORY_EMBED_CACHE`. All have
defaults.

See the repository for the full documentation:
https://github.com/anboo/mcp-memory
