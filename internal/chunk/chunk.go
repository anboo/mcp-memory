// Package chunk превращает части сессий в поисковые чанки по политике
// arch-дока B6.1:
//
//	text      -> чанк (embedding + FTS)
//	tool      -> чанк (embedding + FTS), output обрезан до 4 КБ
//	patch     -> чанк (только FTS, embedding = nil)
//	reasoning -> чанк (только FTS, embedding = nil)
//	step-start/finish, compaction -> не чанкуются
package chunk

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"opencode-rag/internal/extract"
)

// MaxToolOutputLen - лимит вывода tool в content (для эмбеддинга и FTS).
// Полный вывод отдаёт memory_read напрямую из SQLite (step-3).
const MaxToolOutputLen = 4096

// MaxEmbedContentLen - лимит длины текста, уходящего в эмбеддинг.
// Content хранится полным для FTS, но вектор считается по обрезанному
// префиксу: bge-m3 держит ~8192 токенов, длинные ассистентские ответы
// (до 176 КБ) превышают контекст и сильно замедляют GPU.
const MaxEmbedContentLen = 6000

// Chunk - единица поискового индекса (arch-док B6.2).
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
	// Embedding заполняется отдельно (internal/embed); nil для reasoning/patch.
	Embedding []float32
}

// SnippetLen - длина сниппета для выдачи (B6.3).
const SnippetLen = 300

// Chunkify превращает части сессии в чанки.
// projectID/projectPath - привязка сессии к проекту (arch-док A3.7).
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
		// только FTS, без embedding (B6.1)
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

// snippet обрезает строку до n символов (по runes - кириллица).
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

// truncateRunes обрезает строку до n рун (не разрывая UTF-8).
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// sanitizeUTF8 чистит текст для PostgreSQL:
//   - битые UTF-8 последовательности -> U+FFFD (binary output инструментов)
//   - NUL и управляющие байты (0x00-0x08, 0x0B, 0x0C, 0x0E-0x1F) -> удаляются
//
// Без этого PG отклонит вставку: "invalid byte sequence for encoding UTF8"
// (битые байты) или "invalid byte sequence ... 0x00" (NUL).
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

// isControl - управляющие символы, которые PG не принимает в text.
// Оставляем \t (0x09), \n (0x0A), \r (0x0D).
func isControl(r rune) bool {
	return r == 0 || (r < 0x09) || (r > 0x0A && r < 0x0D) || (r > 0x0D && r < 0x20)
}

// NeedsEmbedding - какие чанки получают вектор (B6.1: text и tool).
func NeedsEmbedding(c *Chunk) bool {
	return c.PartType == extract.PartTypeText || c.PartType == extract.PartTypeTool
}

// EmbedContent возвращает текст для эмбеддинга: обрезанный до
// MaxEmbedContentLen префикс. FTS использует полный Content.
func EmbedContent(c *Chunk) string {
	runes := []rune(c.Content)
	if len(runes) <= MaxEmbedContentLen {
		return c.Content
	}
	return string(runes[:MaxEmbedContentLen])
}

// Validate проверяет чанк на минимальную целостность (для тестов и отладки).
func (c *Chunk) Validate() error {
	if c.ID == "" || c.SessionID == "" || c.PartType == "" {
		return fmt.Errorf("chunk: пустые обязательные поля: %+v", c)
	}
	if c.Content == "" {
		return fmt.Errorf("chunk %s: пустой content", c.ID)
	}
	if c.Position < 0 {
		return fmt.Errorf("chunk %s: отрицательная позиция", c.ID)
	}
	return nil
}
