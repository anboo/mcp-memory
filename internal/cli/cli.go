// Package cli implements the subcommands of the single opencode-memory-mcp
// binary.
//
// The same binary runs the MCP server (serve) and builds the index (index and
// the diagnostic commands), so there is nothing else to install or keep in
// sync.
package cli

import (
	"fmt"
	"os"

	"opencode-rag/internal/version"
)

const usage = `opencode-memory-mcp - agent memory over the OpenCode session history

Usage:
  opencode-memory-mcp [command]

Commands:
  serve            Run the MCP server over stdio. This is the default when
                   stdin is not a terminal, which is how MCP clients launch it.
  index            Build or update the local search index.
  sessions         Print a summary of the whole OpenCode database.
  session <id>     Dump the dialog of one session.
  version          Print the version.
  help             Print this help.

Run "opencode-memory-mcp <command> --help" for the flags of a command.
`

// Run dispatches args and returns the process exit code. With no arguments it
// serves MCP when stdin is a pipe (an MCP client), and prints help when stdin
// is a terminal (a human).
func Run(args []string) int {
	if len(args) == 0 {
		if stdinIsTTY() {
			fmt.Print(usage)
			return 0
		}
		return serve(nil)
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve", "mcp":
		return serve(rest)
	case "index":
		return indexCmd(rest)
	case "sessions", "all":
		return sessionsCmd(rest)
	case "session":
		return sessionCmd(rest)
	case "version", "--version", "-v":
		fmt.Println(version.Version)
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

// stdinIsTTY reports whether stdin is an interactive terminal.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
