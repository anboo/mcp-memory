// Command mcp-memory is the single project binary. It serves MCP over
// stdio (serve) and builds the local search index (index), so there is only
// one file to install, update or keep in sync.
package main

import (
	"os"

	"github.com/anboo/mcp-memory/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
