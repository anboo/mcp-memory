package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"opencode-rag/internal/search"
)

// Server - MCP-сервер поверх логики из mcp.go.
type Server struct {
	server *server.MCPServer
	deps   Dependencies
	lim    Limiter
}

// New создаёт MCP-сервер с четырьмя инструментами памяти.
func New(deps Dependencies, lim Limiter) (*Server, error) {
	if deps.Searcher == nil {
		return nil, fmt.Errorf("mcp: Searcher не задан")
	}
	if deps.SQLite == nil {
		return nil, fmt.Errorf("mcp: SQLite не задан")
	}
	s := &Server{
		server: server.NewMCPServer("opencode-memory", "0.1.0"),
		deps:   deps,
		lim:    lim,
	}
	s.registerTools()
	return s, nil
}

// ServeStdio запускает сервер на stdio (как запускает opencode).
func (s *Server) ServeStdio() error {
	return server.ServeStdio(s.server)
}

// registerTools регистрирует инструменты memory_search/read/session/context.
func (s *Server) registerTools() {
	searchTool := mcp.NewTool("memory_search",
		mcp.WithDescription("Гибридный поиск по истории всех сессий агента (векторный + полнотекстовый). "+
			"Возвращает сниппеты с координатами (position) для дальнейшего чтения через memory_read. "+
			"Используй, когда задача зависит от прошлой работы, решений, неудачных попыток или "+
			"конфигурации. Фильтруй по project, чтобы не вытаскивать чужие сессии."),
		mcp.WithString("query", mcp.Required(), mcp.Description("Поисковый запрос, например: 'почему не подключался Kafka' или 'CLUSTERDOWN'")),
		mcp.WithString("project", mcp.Description("Фильтр по пути проекта, например /var/www/betboom/risks")),
		mcp.WithString("type", mcp.Description("Фильтр по типу части: text|tool|patch|reasoning")),
		mcp.WithNumber("time_from", mcp.Description("Только части новее этого времени (epoch ms)")),
		mcp.WithNumber("limit", mcp.Description("Максимум результатов (по умолчанию 20, максимум 50)")),
	)
	s.server.AddTool(searchTool, s.handleSearch)

	readTool := mcp.NewTool("memory_read",
		mcp.WithDescription("Читает окно оригинальной истории сессии вокруг координаты position. "+
			"Возвращает ПОЛНЫЕ тексты и выводы команд (не сниппеты). Используй после memory_search, "+
			"чтобы увидеть контекст вокруг найденного места: что делали, какие команды выполняли, "+
			"какой был результат."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("ID сессии из результата memory_search")),
		mcp.WithNumber("position", mcp.Required(), mcp.Description("Координата части внутри сессии из результата memory_search")),
		mcp.WithNumber("before", mcp.Description("Сколько частей прочитать до (по умолчанию 5)")),
		mcp.WithNumber("after", mcp.Description("Сколько частей прочитать после (по умолчанию 10)")),
	)
	s.server.AddTool(readTool, s.handleRead)

	sessionTool := mcp.NewTool("memory_session",
		mcp.WithDescription("Метаданные сессии: заголовок, агент, проект, время, карта сообщений "+
			"с типами частей. Без полных текстов - для ориентира, какие темы в сессии были."),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("ID сессии")),
	)
	s.server.AddTool(sessionTool, s.handleSession)

	contextTool := mcp.NewTool("memory_context",
		mcp.WithDescription("Окно истории вокруг конкретной части по её ID (prt_xxx). "+
			"Аналог memory_read, но ищет по part_id, а не по координате."),
		mcp.WithString("part_id", mcp.Required(), mcp.Description("ID части (prt_xxx) из результата поиска")),
		mcp.WithNumber("before", mcp.Description("Сколько частей прочитать до (по умолчанию 5)")),
		mcp.WithNumber("after", mcp.Description("Сколько частей прочитать после (по умолчанию 10)")),
	)
	s.server.AddTool(contextTool, s.handleContext)
}

// arg - достаёт аргумент из MCP-запроса.
func arg[T any](request mcp.CallToolRequest, name string, def T) T {
	args, ok := request.Params.Arguments.(map[string]any)
	if !ok {
		return def
	}
	if v, ok := args[name]; ok {
		if t, ok := v.(T); ok {
			return t
		}
	}
	return def
}

// respond - JSON-ответ в текстовом контенте.
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
	params := SearchParams{
		Query:    arg[string](request, "query", ""),
		Project:  arg[string](request, "project", ""),
		Type:     arg[string](request, "type", ""),
		TimeFrom: int64(arg[float64](request, "time_from", 0)),
		Limit:    int(arg[float64](request, "limit", 0)),
	}
	hits, err := Search(ctx, s.deps, s.lim, params)
	if err != nil {
		return nil, fmt.Errorf("memory_search: %w", err)
	}
	if hits == nil {
		hits = []search.Hit{} // пустой массив, а не null
	}
	return respond(map[string]any{"results": hits})
}

func (s *Server) handleRead(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	win, err := Read(ctx, s.deps, s.lim, ReadParams{
		SessionID: arg[string](request, "session_id", ""),
		Position:  int(arg[float64](request, "position", 0)),
		Before:    int(arg[float64](request, "before", 0)),
		After:     int(arg[float64](request, "after", 0)),
	})
	if err != nil {
		return nil, fmt.Errorf("memory_read: %w", err)
	}
	return respond(win)
}

func (s *Server) handleSession(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	info, err := Session(ctx, s.deps, s.lim, arg[string](request, "session_id", ""))
	if err != nil {
		return nil, fmt.Errorf("memory_session: %w", err)
	}
	return respond(info)
}

func (s *Server) handleContext(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	win, err := Context(ctx, s.deps, s.lim, ContextParams{
		PartID: arg[string](request, "part_id", ""),
		Before: int(arg[float64](request, "before", 0)),
		After:  int(arg[float64](request, "after", 0)),
	})
	if err != nil {
		return nil, fmt.Errorf("memory_context: %w", err)
	}
	return respond(win)
}
