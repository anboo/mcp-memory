package extract

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// PartType - тип части (поле "type" в data JSON).
const (
	PartTypeText       = "text"
	PartTypeTool       = "tool"
	PartTypeReasoning  = "reasoning"
	PartTypePatch      = "patch"
	PartTypeStepStart  = "step-start"
	PartTypeStepFinish = "step-finish"
	PartTypeCompaction = "compaction"
	PartTypeFile       = "file"
)

// Part - строка таблицы part (см. arch-док A3.4).
// Raw хранит полный JSON data для разбора конкретного типа.
type Part struct {
	ID          string
	MessageID   string
	SessionID   string
	Type        string
	TimeCreated int64
	TimeUpdated int64
	Raw         json.RawMessage
}

// PartWithPos - часть с координатой внутри сессии.
// Порядок координат стабилен: message.time_created, затем part.rowid
// (у частей одного сообщения time_created может совпадать).
type PartWithPos struct {
	Part
	Position int
}

// Разобранные JSON-структуры частей (arch-док A3.4).

type TextPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ReasoningPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ToolPart struct {
	Type   string `json:"type"`
	Tool   string `json:"tool"`
	CallID string `json:"callID"`
	State  struct {
		Status string `json:"status"`
		Input  struct {
			Command     string `json:"command"`
			Description string `json:"description"`
		} `json:"input"`
		Output string `json:"output"`
	} `json:"state"`
}

type PatchPart struct {
	Type  string   `json:"type"`
	Hash  string   `json:"hash"`
	Files []string `json:"files"`
}

type CompactionPart struct {
	Type        string `json:"type"`
	Auto        bool   `json:"auto"`
	Overflow    bool   `json:"overflow"`
	TailStartID string `json:"tail_start_id"`
}

// ParseText разбирает Raw в TextPart.
func (p *Part) ParseText() (TextPart, error) {
	var t TextPart
	if err := json.Unmarshal(p.Raw, &t); err != nil {
		return t, fmt.Errorf("extract: parse text part %s: %w", p.ID, err)
	}
	return t, nil
}

// ParseReasoning разбирает Raw в ReasoningPart.
func (p *Part) ParseReasoning() (ReasoningPart, error) {
	var t ReasoningPart
	if err := json.Unmarshal(p.Raw, &t); err != nil {
		return t, fmt.Errorf("extract: parse reasoning part %s: %w", p.ID, err)
	}
	return t, nil
}

// ParseTool разбирает Raw в ToolPart.
func (p *Part) ParseTool() (ToolPart, error) {
	var t ToolPart
	if err := json.Unmarshal(p.Raw, &t); err != nil {
		return t, fmt.Errorf("extract: parse tool part %s: %w", p.ID, err)
	}
	return t, nil
}

// ParsePatch разбирает Raw в PatchPart.
func (p *Part) ParsePatch() (PatchPart, error) {
	var t PatchPart
	if err := json.Unmarshal(p.Raw, &t); err != nil {
		return t, fmt.Errorf("extract: parse patch part %s: %w", p.ID, err)
	}
	return t, nil
}

// ParseCompaction разбирает Raw в CompactionPart.
func (p *Part) ParseCompaction() (CompactionPart, error) {
	var t CompactionPart
	if err := json.Unmarshal(p.Raw, &t); err != nil {
		return t, fmt.Errorf("extract: parse compaction part %s: %w", p.ID, err)
	}
	return t, nil
}

// TypeOf читает только поле type из Raw без полного разбора.
func (p *Part) TypeOf() string {
	if p.Type != "" {
		return p.Type
	}
	var h struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(p.Raw, &h)
	return h.Type
}

// PartsInOrder возвращает части сессии в координатном порядке
// (message.time_created, затем part.rowid).
//
// rowid - порядок вставки внутри сообщения: при равных time_created
// это единственный стабильный порядок.
func PartsInOrder(ctx context.Context, db *sql.DB, sessionID string) ([]Part, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT p.id, p.message_id, p.session_id, p.time_created, p.time_updated, p.data
		 FROM part p
		 JOIN message m ON m.id = p.message_id
		 WHERE p.session_id = ?
		 ORDER BY m.time_created, p.rowid`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("extract: list parts: %w", err)
	}
	defer rows.Close()

	var out []Part
	for rows.Next() {
		var p Part
		var data string
		if err := rows.Scan(&p.ID, &p.MessageID, &p.SessionID,
			&p.TimeCreated, &p.TimeUpdated, &data); err != nil {
			return nil, fmt.Errorf("extract: scan part: %w", err)
		}
		p.Raw = json.RawMessage(data)
		p.Type = p.TypeOf()
		out = append(out, p)
	}
	return out, rows.Err()
}

// PartsWithPosition возвращает части сессии с сквозными координатами.
// Position - номер части в хронологическом порядке сессии (0-based).
// Используется индексатором (step-2) и memory_read (step-3).
func PartsWithPosition(ctx context.Context, db *sql.DB, sessionID string) ([]PartWithPos, error) {
	parts, err := PartsInOrder(ctx, db, sessionID)
	if err != nil {
		return nil, err
	}
	out := make([]PartWithPos, len(parts))
	for i, p := range parts {
		out[i] = PartWithPos{Part: p, Position: i}
	}
	return out, nil
}

// MarkCompacted помечает сессию скомпактированной и заполняет TailStartID,
// если среди частей есть compaction (arch-док A7.3).
func MarkCompacted(s *Session, parts []Part) {
	for i := range parts {
		if parts[i].Type != PartTypeCompaction {
			continue
		}
		c, err := parts[i].ParseCompaction()
		if err != nil {
			continue
		}
		s.Compacted = true
		if c.TailStartID != "" {
			s.TailStartID = c.TailStartID
		}
	}
}
