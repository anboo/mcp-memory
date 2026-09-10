// Package mcp - MCP-сервер памяти агента (arch-док B4).
//
// Четыре инструмента, все без LLM внутри:
//
//	memory_search   - гибридный поиск: сниппеты + координаты
//	memory_read     - окно истории вокруг координаты (ПОЛНЫЕ text/output)
//	memory_session  - метаданные сессии + карта сообщений
//	memory_context  - окно вокруг конкретной части (по part_id)
//
// Аналитиком является текущая модель агента: она решает, что искать
// и что читать.
package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"opencode-rag/internal/extract"
	"opencode-rag/internal/search"
)

// Dependencies - внешние зависимости сервера.
type Dependencies struct {
	Searcher *search.Searcher // гибридный поиск по PG (не nil)
	SQLite   *sql.DB          // read-only opencode.db для чтения оригиналов
}

// Limiter - ограничения вывода (защита от раздувания контекста).
type Limiter struct {
	// MaxWindow - максимум частей в окне memory_read/memory_context.
	MaxWindow int
	// MaxToolOutput - максимум байт вывода tool на одну часть.
	MaxToolOutput int
	// MaxSearchResults - максимум результатов memory_search.
	MaxSearchResults int
}

// DefaultLimiter - разумные дефолты (arch-док B4.3, step-3).
func DefaultLimiter() Limiter {
	return Limiter{
		MaxWindow:        50,
		MaxToolOutput:    16 * 1024,
		MaxSearchResults: 20,
	}
}

// ---------------------------------------------------------------------------
// memory_search
// ---------------------------------------------------------------------------

// SearchParams - аргументы memory_search.
type SearchParams struct {
	Query    string `json:"query"`
	Project  string `json:"project,omitempty"`
	Type     string `json:"type,omitempty"`
	TimeFrom int64  `json:"time_from,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// Search выполняет memory_search: сниппеты + координаты.
func Search(ctx context.Context, deps Dependencies, lim Limiter, p SearchParams) ([]search.Hit, error) {
	if p.Query == "" {
		return nil, fmt.Errorf("memory_search: пустой query")
	}
	if p.Limit <= 0 || p.Limit > lim.MaxSearchResults {
		p.Limit = lim.MaxSearchResults
	}
	return deps.Searcher.Search(ctx, search.Query{
		Text:     p.Query,
		Project:  p.Project,
		Type:     p.Type,
		TimeFrom: p.TimeFrom,
		Limit:    p.Limit,
	})
}

// ---------------------------------------------------------------------------
// memory_read / memory_context
// ---------------------------------------------------------------------------

// WindowPart - одна часть в окне истории.
type WindowPart struct {
	ID          string   `json:"id"`
	Role        string   `json:"role,omitempty"`
	Type        string   `json:"type"`
	Tool        string   `json:"tool,omitempty"`
	Command     string   `json:"command,omitempty"`
	Text        string   `json:"text,omitempty"`
	Output      string   `json:"output,omitempty"`
	Files       []string `json:"files,omitempty"`
	Position    int      `json:"position"`
	TimeCreated int64    `json:"time_created"`
}

// ReadWindow - окно истории вокруг координаты.
type ReadWindow struct {
	SessionID string       `json:"session_id"`
	Position  int          `json:"position"`
	Parts     []WindowPart `json:"parts"`
	Compacted bool         `json:"compacted"`
}

// ReadParams - аргументы memory_read.
type ReadParams struct {
	SessionID string `json:"session_id"`
	Position  int    `json:"position"`
	Before    int    `json:"before,omitempty"`
	After     int    `json:"after,omitempty"`
}

// Read выполняет memory_read: окно частей вокруг position с полными текстами.
func Read(ctx context.Context, deps Dependencies, lim Limiter, p ReadParams) (*ReadWindow, error) {
	if p.SessionID == "" {
		return nil, fmt.Errorf("memory_read: пустой session_id")
	}
	if p.Before < 0 || p.After < 0 {
		return nil, fmt.Errorf("memory_read: before/after не могут быть отрицательными")
	}
	if p.Before == 0 {
		p.Before = 5
	}
	if p.After == 0 {
		p.After = 10
	}
	if p.Before+p.After > lim.MaxWindow {
		return nil, fmt.Errorf("memory_read: before+after > %d (лимит окна)", lim.MaxWindow)
	}

	parts, err := extract.PartsWithPosition(ctx, deps.SQLite, p.SessionID)
	if err != nil {
		return nil, err
	}

	// найти индекс части с нужной позицией
	idx := -1
	for i := range parts {
		if parts[i].Position == p.Position {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("memory_read: позиция %d не найдена в сессии %s (частей: %d)",
			p.Position, p.SessionID, len(parts))
	}

	from := idx - p.Before
	if from < 0 {
		from = 0
	}
	to := idx + p.After + 1
	if to > len(parts) {
		to = len(parts)
	}

	// компакция сессии
	s := &extract.Session{ID: p.SessionID}
	allParts := make([]extract.Part, len(parts))
	for i := range parts {
		allParts[i] = parts[i].Part
	}
	extract.MarkCompacted(s, allParts)

	win := &ReadWindow{
		SessionID: p.SessionID,
		Position:  p.Position,
		Compacted: s.Compacted,
	}

	// карта ролей сообщений
	msgs, err := extract.ListMessages(ctx, deps.SQLite, p.SessionID)
	if err != nil {
		return nil, err
	}
	roles := make(map[string]string, len(msgs))
	for _, m := range msgs {
		roles[m.ID] = m.Role
	}

	for i := from; i < to; i++ {
		wp, err := toWindowPart(&parts[i], roles, lim.MaxToolOutput)
		if err != nil {
			return nil, err
		}
		win.Parts = append(win.Parts, wp)
	}
	return win, nil
}

// ContextParams - аргументы memory_context.
type ContextParams struct {
	PartID string `json:"part_id"`
	Before int    `json:"before,omitempty"`
	After  int    `json:"after,omitempty"`
}

// Context выполняет memory_context: окно вокруг конкретной части.
func Context(ctx context.Context, deps Dependencies, lim Limiter, p ContextParams) (*ReadWindow, error) {
	if p.PartID == "" {
		return nil, fmt.Errorf("memory_context: пустой part_id")
	}

	// найти часть и её сессию
	var sessionID string
	var position int
	part, err := findPart(ctx, deps.SQLite, p.PartID)
	if err != nil {
		return nil, err
	}
	sessionID = part.SessionID

	parts, err := extract.PartsWithPosition(ctx, deps.SQLite, sessionID)
	if err != nil {
		return nil, err
	}
	for i := range parts {
		if parts[i].ID == p.PartID {
			position = parts[i].Position
			break
		}
	}

	return Read(ctx, deps, lim, ReadParams{
		SessionID: sessionID,
		Position:  position,
		Before:    p.Before,
		After:     p.After,
	})
}

// ---------------------------------------------------------------------------
// memory_session
// ---------------------------------------------------------------------------

// SessionInfo - результат memory_session.
type SessionInfo struct {
	SessionID   string       `json:"session_id"`
	Title       string       `json:"title"`
	Agent       string       `json:"agent,omitempty"`
	Model       string       `json:"model,omitempty"`
	ProjectID   string       `json:"project_id,omitempty"`
	ProjectPath string       `json:"project_path,omitempty"`
	TimeCreated int64        `json:"time_created"`
	TimeUpdated int64        `json:"time_updated"`
	Compacted   bool         `json:"compacted"`
	TailStartID string       `json:"tail_start_id,omitempty"`
	Messages    []SessionMsg `json:"messages"`
}

// SessionMsg - сообщение в карте сессии (без полных текстов).
type SessionMsg struct {
	Role      string   `json:"role"`
	Time      int64    `json:"time_created"`
	PartTypes []string `json:"part_types"`
}

// Session выполняет memory_session: метаданные + карта сообщений.
func Session(ctx context.Context, deps Dependencies, _ Limiter, sessionID string) (*SessionInfo, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("memory_session: пустой session_id")
	}

	s, err := extract.GetSession(ctx, deps.SQLite, sessionID)
	if err != nil {
		return nil, err
	}

	projects, err := extract.ProjectMap(ctx, deps.SQLite)
	if err != nil {
		return nil, err
	}

	parts, err := extract.PartsWithPosition(ctx, deps.SQLite, sessionID)
	if err != nil {
		return nil, err
	}
	allParts := make([]extract.Part, len(parts))
	for i := range parts {
		allParts[i] = parts[i].Part
	}
	extract.MarkCompacted(s, allParts)

	// группировка типов частей по сообщениям
	typesByMsg := make(map[string][]string)
	timeByMsg := make(map[string]int64)
	for i := range parts {
		p := &parts[i]
		typesByMsg[p.MessageID] = append(typesByMsg[p.MessageID], p.Type)
		if _, ok := timeByMsg[p.MessageID]; !ok {
			timeByMsg[p.MessageID] = p.TimeCreated
		}
	}

	msgs, err := extract.ListMessages(ctx, deps.SQLite, sessionID)
	if err != nil {
		return nil, err
	}

	info := &SessionInfo{
		SessionID:   s.ID,
		Title:       s.Title,
		Agent:       s.Agent,
		Model:       s.Model,
		ProjectID:   s.ProjectID,
		ProjectPath: projects[s.ProjectID],
		TimeCreated: s.TimeCreated,
		TimeUpdated: s.TimeUpdated,
		Compacted:   s.Compacted,
		TailStartID: s.TailStartID,
	}
	for _, m := range msgs {
		info.Messages = append(info.Messages, SessionMsg{
			Role:      m.Role,
			Time:      m.TimeCreated,
			PartTypes: typesByMsg[m.ID],
		})
	}
	return info, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// findPart ищет часть по id.
func findPart(ctx context.Context, db *sql.DB, id string) (*extract.Part, error) {
	var p extract.Part
	var data string
	err := db.QueryRowContext(ctx,
		`SELECT id, message_id, session_id, time_created, time_updated, data
		 FROM part WHERE id = ?`, id).
		Scan(&p.ID, &p.MessageID, &p.SessionID, &p.TimeCreated, &p.TimeUpdated, &data)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("часть %s не найдена", id)
	}
	if err != nil {
		return nil, err
	}
	p.Raw = json.RawMessage(data)
	p.Type = p.TypeOf()
	return &p, nil
}

// toWindowPart превращает часть в JSON-представление с полным содержимым.
func toWindowPart(p *extract.PartWithPos, roles map[string]string, maxToolOutput int) (WindowPart, error) {
	wp := WindowPart{
		ID:          p.ID,
		Role:        roles[p.MessageID],
		Type:        p.Type,
		Position:    p.Position,
		TimeCreated: p.TimeCreated,
	}
	switch p.Type {
	case extract.PartTypeText:
		t, err := p.ParseText()
		if err != nil {
			return wp, err
		}
		wp.Text = t.Text
	case extract.PartTypeReasoning:
		t, err := p.ParseReasoning()
		if err != nil {
			return wp, err
		}
		wp.Text = t.Text
	case extract.PartTypeTool:
		t, err := p.ParseTool()
		if err != nil {
			return wp, err
		}
		wp.Tool = t.Tool
		wp.Command = t.State.Input.Command
		wp.Output = t.State.Output
		if maxToolOutput > 0 && len(wp.Output) > maxToolOutput {
			runes := []rune(wp.Output)
			if len(runes) > maxToolOutput {
				wp.Output = string(runes[:maxToolOutput]) + "...[truncated]"
			}
		}
	case extract.PartTypePatch:
		t, err := p.ParsePatch()
		if err != nil {
			return wp, err
		}
		wp.Files = t.Files
	case extract.PartTypeStepStart, extract.PartTypeStepFinish, extract.PartTypeCompaction:
		// маркеры, текст не нужен
	default:
		// прочие типы: без текста
	}
	return wp, nil
}
