// Package mcp implements the agent memory MCP tools.
//
// There are five tools, all without any LLM call inside:
//
//	memory_search   - hybrid search: snippets plus coordinates
//	memory_read     - window of history around a coordinate (full text/output)
//	memory_session  - session metadata plus a map of messages
//	memory_context  - window around a specific part (by part id)
//	memory_status   - index health: counts, sources, degradation state
//
// The current agent model is the researcher: it decides what to look up,
// reads the originals and does the reasoning.
package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"opencode-rag/internal/extract"
	"opencode-rag/internal/search"
	"opencode-rag/internal/store"
)

// StatusProvider reports index health.
type StatusProvider interface {
	Status(ctx context.Context) (store.Status, error)
}

// DocCounter reports the number of documents in the optional Bleve index.
type DocCounter interface {
	DocCount() (uint64, error)
}

// Dependencies are the external dependencies of the server.
type Dependencies struct {
	Searcher *search.Searcher
	SQLite   *sql.DB
	Status   StatusProvider
	Bleve    DocCounter
	// VectorQuery is true when a query embedder is available at runtime.
	VectorQuery bool
}

// Limiter caps output sizes to protect the model context window.
type Limiter struct {
	// MaxWindow is the maximum number of parts in a memory_read window.
	MaxWindow int
	// MaxToolOutput is the maximum tool output per part, in runes.
	MaxToolOutput int
	// MaxSearchResults is the maximum number of memory_search results.
	MaxSearchResults int
}

// DefaultLimiter returns reasonable defaults.
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

// SearchParams are the arguments of memory_search.
type SearchParams struct {
	Query    string `json:"query"`
	Project  string `json:"project,omitempty"`
	Type     string `json:"type,omitempty"`
	TimeFrom int64  `json:"time_from,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// Search runs memory_search.
func Search(ctx context.Context, deps Dependencies, lim Limiter, p SearchParams) (search.Result, error) {
	if p.Query == "" {
		return search.Result{}, fmt.Errorf("memory_search: query is required")
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

// WindowPart is one part in a history window.
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

// ReadWindow is a window of history around a coordinate.
type ReadWindow struct {
	SessionID string       `json:"session_id"`
	Position  int          `json:"position"`
	Parts     []WindowPart `json:"parts"`
	Compacted bool         `json:"compacted"`
}

// ReadParams are the arguments of memory_read.
type ReadParams struct {
	SessionID string `json:"session_id"`
	Position  int    `json:"position"`
	Before    int    `json:"before,omitempty"`
	After     int    `json:"after,omitempty"`
}

// Read runs memory_read: a window of parts around position with full texts.
func Read(ctx context.Context, deps Dependencies, lim Limiter, p ReadParams) (*ReadWindow, error) {
	if p.SessionID == "" {
		return nil, fmt.Errorf("memory_read: session_id is required")
	}
	if p.Position < 0 {
		return nil, fmt.Errorf("memory_read: position must not be negative")
	}
	if p.Before < 0 || p.After < 0 {
		return nil, fmt.Errorf("memory_read: before and after must not be negative")
	}
	if p.Before == 0 {
		p.Before = 5
	}
	if p.After == 0 {
		p.After = 10
	}
	if p.Before+p.After > lim.MaxWindow {
		return nil, fmt.Errorf("memory_read: before+after must not exceed %d parts", lim.MaxWindow)
	}

	parts, err := extract.PartsWithPosition(ctx, deps.SQLite, p.SessionID)
	if err != nil {
		return nil, err
	}

	idx := -1
	for i := range parts {
		if parts[i].Position == p.Position {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("memory_read: position %d not found in session %s (%d parts)",
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

	s := &extract.Session{ID: p.SessionID}
	allParts := make([]extract.Part, len(parts))
	for i := range parts {
		allParts[i] = parts[i].Part
	}
	extract.MarkCompacted(s, allParts)

	win := &ReadWindow{
		SessionID: p.SessionID,
		Position:  p.Position,
		Parts:     []WindowPart{},
		Compacted: s.Compacted,
	}

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

// ContextParams are the arguments of memory_context.
type ContextParams struct {
	PartID string `json:"part_id"`
	Before int    `json:"before,omitempty"`
	After  int    `json:"after,omitempty"`
}

// Context runs memory_context: a window around a specific part.
func Context(ctx context.Context, deps Dependencies, lim Limiter, p ContextParams) (*ReadWindow, error) {
	if p.PartID == "" {
		return nil, fmt.Errorf("memory_context: part_id is required")
	}

	part, err := findPart(ctx, deps.SQLite, p.PartID)
	if err != nil {
		return nil, err
	}

	parts, err := extract.PartsWithPosition(ctx, deps.SQLite, part.SessionID)
	if err != nil {
		return nil, err
	}
	position := -1
	for i := range parts {
		if parts[i].ID == p.PartID {
			position = parts[i].Position
			break
		}
	}
	if position < 0 {
		return nil, fmt.Errorf("memory_context: part %s not found in its session", p.PartID)
	}

	return Read(ctx, deps, lim, ReadParams{
		SessionID: part.SessionID,
		Position:  position,
		Before:    p.Before,
		After:     p.After,
	})
}

// ---------------------------------------------------------------------------
// memory_session
// ---------------------------------------------------------------------------

// SessionInfo is the result of memory_session.
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

// SessionMsg is one message in the session map (without full texts).
type SessionMsg struct {
	Role      string   `json:"role"`
	Time      int64    `json:"time_created"`
	PartTypes []string `json:"part_types"`
}

// Session runs memory_session: metadata plus a map of messages.
func Session(ctx context.Context, deps Dependencies, _ Limiter, sessionID string) (*SessionInfo, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("memory_session: session_id is required")
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
		Messages:    []SessionMsg{},
	}
	for _, m := range msgs {
		parts := typesByMsg[m.ID]
		if parts == nil {
			parts = []string{}
		}
		info.Messages = append(info.Messages, SessionMsg{
			Role:      m.Role,
			Time:      m.TimeCreated,
			PartTypes: parts,
		})
	}
	return info, nil
}

// ---------------------------------------------------------------------------
// memory_status
// ---------------------------------------------------------------------------

// SourceState describes which retrieval sources are currently available.
type SourceState struct {
	FTS      bool `json:"fts"`
	FTSStem  bool `json:"fts_stem"`
	Vector   bool `json:"vector"`
	Bleve    bool `json:"bleve"`
	Degraded bool `json:"degraded"`
}

// StatusInfo is the result of memory_status.
type StatusInfo struct {
	Index  StatusCounts `json:"index"`
	Bleve  StatusBleve  `json:"bleve"`
	Source SourceState  `json:"sources"`
	Note   string       `json:"note,omitempty"`
}

// StatusCounts is the SQLite index health.
type StatusCounts struct {
	Sessions       int64  `json:"sessions"`
	Chunks         int64  `json:"chunks"`
	Vectors        int64  `json:"vectors"`
	PendingVectors int64  `json:"pending_vectors"`
	EmbedDim       int    `json:"embed_dim"`
	EmbedModel     string `json:"embed_model,omitempty"`
	LastSync       int64  `json:"last_sync"`
	SchemaVersion  int    `json:"schema_version"`
}

// StatusBleve is the optional Bleve index health.
type StatusBleve struct {
	Available bool   `json:"available"`
	Docs      uint64 `json:"docs"`
	Error     string `json:"error,omitempty"`
}

// Status runs memory_status.
func Status(ctx context.Context, deps Dependencies) (StatusInfo, error) {
	var out StatusInfo
	if deps.Status == nil {
		return out, fmt.Errorf("memory_status: status provider is not configured")
	}
	st, err := deps.Status.Status(ctx)
	if err != nil {
		return out, err
	}
	out.Index = StatusCounts{
		Sessions:       st.Sessions,
		Chunks:         st.Chunks,
		Vectors:        st.Vectors,
		PendingVectors: st.PendingVectors,
		EmbedDim:       st.EmbedDim,
		EmbedModel:     st.EmbedModel,
		LastSync:       st.LastSync,
		SchemaVersion:  st.SchemaVersion,
	}
	if deps.Bleve != nil {
		n, err := deps.Bleve.DocCount()
		if err != nil {
			out.Bleve = StatusBleve{Available: false, Error: err.Error()}
		} else {
			out.Bleve = StatusBleve{Available: true, Docs: n}
		}
	} else {
		out.Bleve = StatusBleve{Available: false, Error: "no bleve index configured"}
	}

	out.Source.FTS = true
	out.Source.FTSStem = true
	out.Source.Vector = deps.VectorQuery && st.Vectors > 0
	out.Source.Bleve = out.Bleve.Available && out.Bleve.Docs > 0

	out.Note = "FTS (raw + stemmed) is always available; vector and bleve are optional"
	if !deps.VectorQuery {
		out.Note += "; query embedder unavailable, vector search is disabled"
	}
	if st.PendingVectors > 0 {
		out.Note += fmt.Sprintf("; %d chunks have pending vectors (run the indexer once embeddings are reachable)", st.PendingVectors)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func findPart(ctx context.Context, db *sql.DB, id string) (*extract.Part, error) {
	var p extract.Part
	var data string
	err := db.QueryRowContext(ctx,
		`SELECT id, message_id, session_id, time_created, time_updated, data
		 FROM part WHERE id = ?`, id).
		Scan(&p.ID, &p.MessageID, &p.SessionID, &p.TimeCreated, &p.TimeUpdated, &data)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("part %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	p.Raw = json.RawMessage(data)
	p.Type = p.TypeOf()
	return &p, nil
}

// toWindowPart converts a part into its JSON form with full content.
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
		wp.Output = truncateRunes(t.State.Output, maxToolOutput)
	case extract.PartTypePatch:
		t, err := p.ParsePatch()
		if err != nil {
			return wp, err
		}
		wp.Files = t.Files
	case extract.PartTypeStepStart, extract.PartTypeStepFinish, extract.PartTypeCompaction:
		// Markers only; no text is returned.
	default:
		// Unknown types are returned without text.
	}
	return wp, nil
}

// truncateRunes caps s at n runes, appending a marker when it was cut.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "...[truncated]"
}
