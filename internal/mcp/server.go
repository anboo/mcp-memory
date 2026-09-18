package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/anboo/mcp-memory/internal/version"
)

// serverName identifies this MCP server.
const serverName = "mcp-memory"

// instructions is the server-level prompt. It is returned in the initialize
// response and tells the model when and how to use the memory tools.
const instructions = `This server exposes the full OpenCode session history as a searchable memory.

When to use it:
- The task may depend on earlier work, decisions, failed attempts, or configuration from previous sessions.
- You need to recall why something was done, what was tried, or what the result was.
- The user refers to "we", "earlier", "last time", or a topic that is not in the current context.

How to use it:
1. Start with memory_search. Use a natural-language query plus exact identifiers (error strings, file names, ticket ids) in the same call when relevant. Pass "project" to avoid pulling in other projects. Results are snippets with coordinates.
2. Follow up with memory_read (session_id + position) to read the full original text and command output around a hit. Snippets never replace originals.
3. Use memory_session to see what a session covered, and memory_context to jump to a specific part id (prt_xxx).
4. Search several times with different wording. Recall is lexical plus semantic, not magic.
5. Use memory_status to check index health (counts, pending vectors, which sources are active).

Behavior notes:
- Search may be degraded to full-text only when embeddings or the secondary Bleve index are unavailable. The response has a "sources" field and a "degraded" flag.
- The index is read-only for originals: memory_read always reads from the OpenCode database directly, so it is never stale.
- Never invent coordinates: only use session_id and position values returned by memory_search or memory_session.`

// Server is the MCP server over the memory logic.
type Server struct {
	server *server.MCPServer
	deps   Dependencies
	lim    Limiter
}

// New creates an MCP server with the five memory tools.
func New(deps Dependencies, lim Limiter) (*Server, error) {
	if deps.Searcher == nil {
		return nil, fmt.Errorf("mcp: searcher is required")
	}
	if deps.SQLite == nil {
		return nil, fmt.Errorf("mcp: source database is required")
	}
	s := &Server{
		server: server.NewMCPServer(serverName, version.Version,
			server.WithInstructions(instructions)),
		deps: deps,
		lim:  lim,
	}
	s.registerTools()
	return s, nil
}

// ServeStdio serves MCP over stdio.
func (s *Server) ServeStdio() error {
	return server.ServeStdio(s.server)
}

// MCPServer exposes the underlying server for tests.
func (s *Server) MCPServer() *server.MCPServer { return s.server }

func (s *Server) registerTools() {
	searchTool := mcp.NewTool("memory_search",
		mcp.WithDescription(
			"Search the agent's full session history (hybrid: lexical FTS over raw and Russian-stemmed text, "+
				"optional vector KNN, optional Bleve secondary index, merged with RRF). "+
				"Returns snippets with session_id and position. Follow up with memory_read to see full texts. "+
				"Use a natural-language query and include exact identifiers (error strings, file names, ticket ids) "+
				"when you know them. Filter by project to avoid other projects. "+
				"The response lists which sources contributed and whether search is degraded."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Search query, for example 'why Kafka did not connect' or 'CLUSTERDOWN'")),
		mcp.WithString("project", mcp.Description("Filter by project path, for example /var/www/my-repo")),
		mcp.WithString("type", mcp.Description("Filter by part type: text | tool | patch | reasoning")),
		mcp.WithNumber("time_from", mcp.Description("Only parts newer than this time (epoch milliseconds)")),
		mcp.WithNumber("limit", mcp.Description("Maximum results (default 20, maximum 50)")),
	)
	s.server.AddTool(searchTool, s.handleSearch)

	readTool := mcp.NewTool("memory_read",
		mcp.WithDescription(
			"Read a window of original history around a coordinate. Returns FULL texts and command output "+
				"(not snippets). Use it after memory_search to see what was done and what the result was. "+
				"session_id and position come from memory_search."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session id from memory_search")),
		mcp.WithNumber("position", mcp.Required(), mcp.Description("Part position inside the session from memory_search")),
		mcp.WithNumber("before", mcp.Description("How many parts to read before the coordinate (default 5)")),
		mcp.WithNumber("after", mcp.Description("How many parts to read after the coordinate (default 10)")),
	)
	s.server.AddTool(readTool, s.handleRead)

	sessionTool := mcp.NewTool("memory_session",
		mcp.WithDescription(
			"Session metadata (title, agent, project, times) plus a map of messages with their part types. "+
				"No full texts: use it to decide which session to read in detail."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Session id")),
	)
	s.server.AddTool(sessionTool, s.handleSession)

	contextTool := mcp.NewTool("memory_context",
		mcp.WithDescription(
			"Same window as memory_read, but addressed by part id (prt_xxx) instead of a coordinate. "+
				"Useful when you already have a part id from a tool result or a previous read."),
		mcp.WithString("part_id", mcp.Required(), mcp.Description("Part id (prt_xxx)")),
		mcp.WithNumber("before", mcp.Description("How many parts to read before (default 5)")),
		mcp.WithNumber("after", mcp.Description("How many parts to read after (default 10)")),
	)
	s.server.AddTool(contextTool, s.handleContext)

	statusTool := mcp.NewTool("memory_status",
		mcp.WithDescription(
			"Index health: session/chunk/vector counts, pending vectors, embedding model and dimension, "+
				"last sync time, Bleve document count, and which retrieval sources are currently available. "+
				"Use it to check whether search can be semantic or is degraded to full-text only."),
	)
	s.server.AddTool(statusTool, s.handleStatus)
}

// args returns the tool arguments as a map.
func args(request mcp.CallToolRequest) map[string]any {
	if m, ok := request.Params.Arguments.(map[string]any); ok {
		return m
	}
	return nil
}

// argString reads a string argument, tolerating numeric and json.Number values.
func argString(request mcp.CallToolRequest, name string) string {
	v, ok := args(request)[name]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

// argInt reads an integer argument, tolerating float64, int64 and strings.
func argInt(request mcp.CallToolRequest, name string, def int64) int64 {
	v, ok := args(request)[name]
	if !ok {
		return def
	}
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return def
		}
		return n
	case string:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			return def
		}
		return n
	default:
		return def
	}
}

// respond marshals data as JSON text content.
func respond(data any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.NewTextContent(string(b)),
		},
	}, nil
}

func (s *Server) handleSearch(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	res, err := Search(ctx, s.deps, s.lim, SearchParams{
		Query:    argString(request, "query"),
		Project:  argString(request, "project"),
		Type:     argString(request, "type"),
		TimeFrom: argInt(request, "time_from", 0),
		Limit:    int(argInt(request, "limit", 0)),
	})
	if err != nil {
		return nil, fmt.Errorf("memory_search: %w", err)
	}
	return respond(res)
}

func (s *Server) handleRead(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	win, err := Read(ctx, s.deps, s.lim, ReadParams{
		SessionID: argString(request, "session_id"),
		Position:  int(argInt(request, "position", 0)),
		Before:    int(argInt(request, "before", 0)),
		After:     int(argInt(request, "after", 0)),
	})
	if err != nil {
		return nil, fmt.Errorf("memory_read: %w", err)
	}
	return respond(win)
}

func (s *Server) handleSession(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	info, err := Session(ctx, s.deps, s.lim, argString(request, "session_id"))
	if err != nil {
		return nil, fmt.Errorf("memory_session: %w", err)
	}
	return respond(info)
}

func (s *Server) handleContext(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	win, err := Context(ctx, s.deps, s.lim, ContextParams{
		PartID: argString(request, "part_id"),
		Before: int(argInt(request, "before", 0)),
		After:  int(argInt(request, "after", 0)),
	})
	if err != nil {
		return nil, fmt.Errorf("memory_context: %w", err)
	}
	return respond(win)
}

func (s *Server) handleStatus(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	info, err := Status(ctx, s.deps)
	if err != nil {
		return nil, fmt.Errorf("memory_status: %w", err)
	}
	return respond(info)
}
