package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/simple"
	"github.com/blevesearch/bleve/v2/analysis/lang/ru"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/analysis/tokenizer/regexp"
	"github.com/blevesearch/bleve/v2/mapping"
)

// identAnalyzer - кастомный анализатор для точных идентификаторов.
// regexp-токенайзер режет по всему, кроме [A-Za-z0-9_./:@-], поэтому
// "index.html", "tree.sql", "tool_name", "docs/openapi/swagger.yaml",
// "CVE-2023-1234" остаются ОДНИМ токеном (только в нижнем регистре).
// camelCase-фильтр НЕ используем: он дополнительно режет токен по
// не-алфанумерике (tool_name -> tool, _, name) и ломает точный term/prefix.
const identAnalyzer = "ident"

const identPattern = `[A-Za-z0-9_./:@-]+`

// Doc - документ индекса: ровно один на часть сессии.
//
// Идея: в индексе лежит только то, что нужно для поиска и для возврата
// координат. Полные оригиналы (текст, вывод команд) в индекс НЕ кладём -
// их отдаёт readParts() из SQLite по (session_id, position), как memory_read
// в opencode-rag. Это делает индекс маленьким и не дублирует 4.7 ГБ истории.
type Doc struct {
	Content      string `json:"content"`       // поисковое тело, ru-анализатор, хранится для фрагментов
	ContentExact string `json:"content_exact"` // то же тело, simple-анализатор (точные имена, без стемминга)
	ContentIdent string `json:"content_ident"` // то же тело, ident-анализатор (идентификаторы целиком)
	SessionID    string `json:"session_id"`    // координата: сессия
	ProjectPath  string `json:"project_path"`  // фильтр
	PartType     string `json:"part_type"`     // text|tool|reasoning|patch
	Role         string `json:"role"`          // user|assistant
	Tool         string `json:"tool"`          // имя инструмента (для tool-частей)
	Command      string `json:"command"`       // команда (для tool-частей)
	Snippet      string `json:"snippet"`       // короткий сниппет для выдачи, не индексируется
	Position     int    `json:"position"`      // координата: позиция внутри сессии (0-based)
	TimeCreated  int64  `json:"time_created"`  // epoch ms
}

// buildMapping описывает структуру индекса.
//
// Ключевые решения:
//   - content индексируется ru-анализатором (стемминг + стоп-слова); Store
//     включается отдельно (storeContent): хранить полный текст нужно только
//     ради подсветки фрагментов, а координаты работают и без него;
//   - content_exact индексируется simple-анализатором (lowercase без стемминга)
//     для точных имён (CLUSTERDOWN, tree.sql, RISK-123), в хранилище не идёт;
//   - session_id / position / part_type / role / project_path - keyword/numeric,
//     индексируются и хранятся: именно они возвращают координаты;
//   - time_created - numeric, для фильтра "не старше N".
func buildMapping(storeContent bool) (mapping.IndexMapping, error) {
	im := bleve.NewIndexMapping()
	im.DefaultAnalyzer = ru.AnalyzerName

	// кастомный анализатор идентификаторов: regexp-токенайзер + lower + camelCase
	if err := im.AddCustomTokenizer("ident_tok", map[string]any{
		"type":   regexp.Name,
		"regexp": identPattern,
	}); err != nil {
		return nil, fmt.Errorf("custom tokenizer: %w", err)
	}
	if err := im.AddCustomAnalyzer(identAnalyzer, map[string]any{
		"type":          custom.Name,
		"tokenizer":     "ident_tok",
		"token_filters": []string{lowercase.Name},
	}); err != nil {
		return nil, fmt.Errorf("custom analyzer: %w", err)
	}

	dm := bleve.NewDocumentMapping()

	content := bleve.NewTextFieldMapping()
	content.Analyzer = ru.AnalyzerName
	content.Store = storeContent
	dm.AddFieldMappingsAt("content", content)

	contentExact := bleve.NewTextFieldMapping()
	contentExact.Analyzer = simple.Name
	contentExact.Store = false
	contentExact.IncludeInAll = false
	dm.AddFieldMappingsAt("content_exact", contentExact)

	contentIdent := bleve.NewTextFieldMapping()
	contentIdent.Analyzer = identAnalyzer
	contentIdent.Store = false
	contentIdent.IncludeInAll = false
	dm.AddFieldMappingsAt("content_ident", contentIdent)

	for _, f := range []string{"session_id", "project_path", "part_type", "role", "tool"} {
		fm := bleve.NewKeywordFieldMapping()
		fm.Store = true
		dm.AddFieldMappingsAt(f, fm)
	}

	command := bleve.NewTextFieldMapping()
	command.Analyzer = simple.Name
	command.Store = true
	dm.AddFieldMappingsAt("command", command)

	snippet := bleve.NewTextFieldMapping()
	snippet.Index = false
	snippet.Store = true
	snippet.IncludeInAll = false
	dm.AddFieldMappingsAt("snippet", snippet)

	position := bleve.NewNumericFieldMapping()
	position.Store = true
	dm.AddFieldMappingsAt("position", position)

	timeCreated := bleve.NewNumericFieldMapping()
	timeCreated.Store = true
	dm.AddFieldMappingsAt("time_created", timeCreated)

	im.DefaultMapping = dm
	return im, nil
}

// IndexStats - что получилось после индексации.
type IndexStats struct {
	Docs     int
	Duration time.Duration
	Bytes    int64
}

// CreateIndex заново создаёт индекс в dir.
// storeContent=true хранит полный content (нужно для подсветки), false -
// только координаты и сниппет.
func CreateIndex(dir string, storeContent bool) (bleve.Index, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("clear index dir: %w", err)
	}
	m, err := buildMapping(storeContent)
	if err != nil {
		return nil, err
	}
	idx, err := bleve.New(dir, m)
	if err != nil {
		return nil, fmt.Errorf("create index: %w", err)
	}
	return idx, nil
}

// OpenIndex открывает существующий индекс (только read).
func OpenIndex(dir string) (bleve.Index, error) {
	idx, err := bleve.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open index %s: %w", dir, err)
	}
	return idx, nil
}

// AddParts добавляет части одной сессии в индекс и возвращает число документов.
// parts уже в координатном порядке, roles - message_id -> Message.
func AddParts(idx bleve.Index, projectPath string, parts []PartWithPos, roles map[string]Message) (int, error) {
	batch := idx.NewBatch()
	n := 0
	for i := range parts {
		p := &parts[i]
		content, tool, command, _, ok := renderPart(&p.Part, 4096)
		if !ok {
			continue // step-start/finish, compaction и пустые части не индексируются
		}
		doc := Doc{
			Content:      content,
			ContentExact: content,
			ContentIdent: content,
			SessionID:    p.SessionID,
			ProjectPath:  projectPath,
			PartType:     p.Type,
			Tool:         tool,
			Command:      command,
			Snippet:      snippet(content, 300),
			Position:     p.Position,
			TimeCreated:  p.TimeCreated,
		}
		if m, found := roles[p.MessageID]; found {
			doc.Role = m.Role
		}
		if err := batch.Index(p.ID, doc); err != nil {
			return n, fmt.Errorf("index %s: %w", p.ID, err)
		}
		n++
		if n%500 == 0 {
			if err := idx.Batch(batch); err != nil {
				return n, fmt.Errorf("flush batch: %w", err)
			}
			batch = idx.NewBatch()
		}
	}
	if batch.Size() > 0 {
		if err := idx.Batch(batch); err != nil {
			return n, fmt.Errorf("flush batch: %w", err)
		}
	}
	return n, nil
}

func snippet(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
