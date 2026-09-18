// Package chunk turns session parts into searchable chunks according to the
// indexing policy:
//
//	text      -> chunk (embedding + FTS)
//	tool      -> chunk (embedding + FTS), output capped at 4 KiB
//	patch     -> chunk (FTS only, embedding nil)
//	reasoning -> chunk (FTS only, embedding nil)
//	step-start/finish, compaction -> not chunked
package chunk

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/anboo/mcp-memory/internal/extract"
)

// MaxToolOutputLen caps tool output inside content (used for embeddings and
// FTS). The full output is served by memory_read directly from SQLite.
const MaxToolOutputLen = 4096

// MaxEmbedContentLen caps the text sent to the embedder. Content is stored in
// full for FTS, but the vector is computed from a truncated prefix: bge-m3
// holds about 8192 tokens and long assistant answers (up to 176 KiB) exceed
// the context and slow the GPU down badly.
const MaxEmbedContentLen = 6000

// SnippetLen is the snippet length returned in search results.
const SnippetLen = 300

// Chunk is one unit of the search index.
type Chunk struct {
	ID          string
	SessionID   string
	ProjectID   string
	ProjectPath string
	MessageID   string
	MsgRole     string
	PartType    string
	Tool        string
	Command     string
	Content     string
	Snippet     string
	Files       []string
	Position    int
	TimeCreated int64
	TimeUpdated int64
	Truncated   bool
	// Embedding is filled separately by internal/embed; nil for reasoning and
	// patch chunks.
	Embedding []float32
}

// Chunkify turns session parts into chunks.
// projectID/projectPath bind the session to a project.
func Chunkify(sessionID, projectID, projectPath string, parts []extract.PartWithPos, messages map[string]extract.Message) []Chunk {
	out := make([]Chunk, 0, len(parts))
	for i := range parts {
		p := &parts[i]
		c, ok := chunkPart(p, sessionID, projectID, projectPath)
		if !ok {
			continue
		}
		if m, found := messages[p.MessageID]; found {
			c.MsgRole = m.Role
		}
		out = append(out, c)
	}
	return out
}

func chunkPart(p *extract.PartWithPos, sessionID, projectID, projectPath string) (Chunk, bool) {
	switch p.Type {
	case extract.PartTypeText:
		t, err := p.ParseText()
		if err != nil {
			return Chunk{}, false
		}
		content := strings.TrimSpace(t.Text)
		if content == "" {
			return Chunk{}, false
		}
		return Chunk{
			ID: p.ID, SessionID: sessionID, ProjectID: projectID, ProjectPath: projectPath,
			MessageID: p.MessageID, PartType: p.Type,
			Content: content, Snippet: snippet(content, SnippetLen),
			Position: p.Position, TimeCreated: p.TimeCreated, TimeUpdated: p.TimeUpdated,
		}, true

	case extract.PartTypeTool:
		t, err := p.ParseTool()
		if err != nil {
			return Chunk{}, false
		}
		cmd := sanitizeUTF8(t.State.Input.Command)
		output := sanitizeUTF8(t.State.Output)
		truncated := len([]rune(output)) > MaxToolOutputLen
		if truncated {
			output = truncateRunes(output, MaxToolOutputLen)
		}
		var b strings.Builder
		b.WriteString(t.Tool)
		if cmd != "" {
			b.WriteString(": ")
			b.WriteString(cmd)
		}
		if output != "" {
			b.WriteString("\n")
			b.WriteString(output)
		}
		content := sanitizeUTF8(b.String())
		if content == "" {
			return Chunk{}, false
		}
		return Chunk{
			ID: p.ID, SessionID: sessionID, ProjectID: projectID, ProjectPath: projectPath,
			MessageID: p.MessageID, PartType: p.Type,
			Tool: t.Tool, Command: cmd,
			Content: content, Snippet: snippet(content, SnippetLen),
			Position: p.Position, TimeCreated: p.TimeCreated, TimeUpdated: p.TimeUpdated,
			Truncated: truncated,
		}, true

	case extract.PartTypeReasoning:
		t, err := p.ParseReasoning()
		if err != nil {
			return Chunk{}, false
		}
		content := strings.TrimSpace(t.Text)
		if content == "" {
			return Chunk{}, false
		}
		// FTS only, no embedding.
		return Chunk{
			ID: p.ID, SessionID: sessionID, ProjectID: projectID, ProjectPath: projectPath,
			MessageID: p.MessageID, PartType: p.Type,
			Content: content, Snippet: snippet(content, SnippetLen),
			Position: p.Position, TimeCreated: p.TimeCreated, TimeUpdated: p.TimeUpdated,
		}, true

	case extract.PartTypePatch:
		t, err := p.ParsePatch()
		if err != nil {
			return Chunk{}, false
		}
		if len(t.Files) == 0 {
			return Chunk{}, false
		}
		content := strings.Join(t.Files, "\n")
		return Chunk{
			ID: p.ID, SessionID: sessionID, ProjectID: projectID, ProjectPath: projectPath,
			MessageID: p.MessageID, PartType: p.Type,
			Files: t.Files, Content: content,
			Snippet:  "patch: " + snippet(strings.Join(t.Files, ", "), SnippetLen),
			Position: p.Position, TimeCreated: p.TimeCreated, TimeUpdated: p.TimeUpdated,
		}, true
	}
	return Chunk{}, false
}

// snippet truncates a string to n runes (cyrillic-safe).
func snippet(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

// truncateRunes truncates a string to n runes without breaking UTF-8.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// sanitizeUTF8 cleans text before it is stored:
//   - broken UTF-8 sequences become U+FFFD (binary tool output);
//   - NUL and control bytes (0x00-0x08, 0x0B, 0x0C, 0x0E-0x1F) are removed.
//
// Tab, newline and carriage return are preserved.
func sanitizeUTF8(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	if strings.ContainsFunc(s, isControl) {
		s = strings.Map(func(r rune) rune {
			if isControl(r) {
				return -1
			}
			return r
		}, s)
	}
	return s
}

// isControl reports control characters that should not be stored.
// Tab (0x09), newline (0x0A) and carriage return (0x0D) are kept.
func isControl(r rune) bool {
	return r == 0 || (r < 0x09) || (r > 0x0A && r < 0x0D) || (r > 0x0D && r < 0x20)
}

// NeedsEmbedding reports which chunk types get a vector (text and tool).
func NeedsEmbedding(c *Chunk) bool {
	return c.PartType == extract.PartTypeText || c.PartType == extract.PartTypeTool
}

// EmbedContent returns the text for the embedder: a prefix capped at
// MaxEmbedContentLen. FTS uses the full Content.
func EmbedContent(c *Chunk) string {
	runes := []rune(c.Content)
	if len(runes) <= MaxEmbedContentLen {
		return c.Content
	}
	return string(runes[:MaxEmbedContentLen])
}

// Validate checks a chunk for minimal integrity (tests and debugging).
func (c *Chunk) Validate() error {
	if c.ID == "" || c.SessionID == "" || c.PartType == "" {
		return fmt.Errorf("chunk: missing required fields: %+v", c)
	}
	if c.Content == "" {
		return fmt.Errorf("chunk %s: empty content", c.ID)
	}
	if c.Position < 0 {
		return fmt.Errorf("chunk %s: negative position", c.ID)
	}
	return nil
}
