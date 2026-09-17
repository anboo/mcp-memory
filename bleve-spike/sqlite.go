package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Session - строка таблицы session.
type Session struct {
	ID          string
	ProjectID   string
	Slug        string
	Directory   string
	Title       string
	Agent       string
	Model       string
	TimeCreated int64
	TimeUpdated int64
}

// Message - строка таблицы message.
type Message struct {
	ID          string
	Role        string
	TimeCreated int64
}

// Part - строка таблицы part.
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
// Position - номер части в порядке (message.time_created, part.rowid).
type PartWithPos struct {
	Part
	Position int
}

// openDB открывает opencode.db только для чтения.
func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return db, nil
}

// tableColumns возвращает список колонок таблицы (для команды schema).
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func ListTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ProjectMap - project_id -> worktree.
func ProjectMap(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, worktree FROM project`)
	if err != nil {
		return nil, fmt.Errorf("project map: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, worktree string
		if err := rows.Scan(&id, &worktree); err != nil {
			return nil, err
		}
		out[id] = worktree
	}
	return out, rows.Err()
}

// GetSession возвращает одну сессию по id.
func GetSession(ctx context.Context, db *sql.DB, id string) (*Session, error) {
	row := db.QueryRowContext(ctx,
		`SELECT id, project_id, slug, directory, title, agent, model,
		        time_created, time_updated
		 FROM session WHERE id = ?`, id)
	var s Session
	var agent, model sql.NullString
	if err := row.Scan(&s.ID, &s.ProjectID, &s.Slug, &s.Directory, &s.Title,
		&agent, &model, &s.TimeCreated, &s.TimeUpdated); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("session %s не найдена", id)
		}
		return nil, err
	}
	s.Agent = agent.String
	s.Model = model.String
	return &s, nil
}

// ListSessionIDs возвращает все id сессий, свежие первыми.
func ListSessionIDs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM session ORDER BY time_updated DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MessageRoles - message_id -> role для сессии.
func MessageRoles(ctx context.Context, db *sql.DB, sessionID string) (map[string]Message, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, time_created, data FROM message WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Message{}
	for rows.Next() {
		var id, data string
		var t int64
		if err := rows.Scan(&id, &t, &data); err != nil {
			return nil, err
		}
		var h struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal([]byte(data), &h)
		out[id] = Message{ID: id, Role: h.Role, TimeCreated: t}
	}
	return out, rows.Err()
}

// PartsWithPosition читает все части сессии в координатном порядке.
// Порядок тот же, что в opencode-rag: message.time_created, затем part.rowid.
func PartsWithPosition(ctx context.Context, db *sql.DB, sessionID string) ([]PartWithPos, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT p.id, p.message_id, p.session_id, p.time_created, p.time_updated, p.data
		 FROM part p
		 JOIN message m ON m.id = p.message_id
		 WHERE p.session_id = ?
		 ORDER BY m.time_created, p.rowid`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("parts: %w", err)
	}
	defer rows.Close()
	var out []PartWithPos
	i := 0
	for rows.Next() {
		var p Part
		var data string
		if err := rows.Scan(&p.ID, &p.MessageID, &p.SessionID,
			&p.TimeCreated, &p.TimeUpdated, &data); err != nil {
			return nil, fmt.Errorf("scan part: %w", err)
		}
		p.Raw = json.RawMessage(data)
		p.Type = partType(p.Raw)
		out = append(out, PartWithPos{Part: p, Position: i})
		i++
	}
	return out, rows.Err()
}

func partType(raw json.RawMessage) string {
	var h struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &h)
	return h.Type
}

// renderPart превращает часть в плоский текст для индекса/вывода.
// Возвращает content и список файлов (для patch), ok=false если часть
// не несёт текста (step-start/finish, compaction).
func renderPart(p *Part, maxToolOutput int) (content, tool, command string, files []string, ok bool) {
	switch p.Type {
	case "text":
		var t struct{ Text string }
		_ = json.Unmarshal(p.Raw, &t)
		t.Text = strings.TrimSpace(t.Text)
		return t.Text, "", "", nil, t.Text != ""
	case "reasoning":
		var t struct{ Text string }
		_ = json.Unmarshal(p.Raw, &t)
		t.Text = strings.TrimSpace(t.Text)
		return t.Text, "", "", nil, t.Text != ""
	case "patch":
		var t struct {
			Files []string `json:"files"`
		}
		_ = json.Unmarshal(p.Raw, &t)
		if len(t.Files) == 0 {
			return "", "", "", nil, false
		}
		return strings.Join(t.Files, "\n"), "", "", t.Files, true
	case "tool":
		var t struct {
			Tool  string `json:"tool"`
			State struct {
				Input struct {
					Command     string `json:"command"`
					Description string `json:"description"`
				} `json:"input"`
				Output string `json:"output"`
			} `json:"state"`
		}
		_ = json.Unmarshal(p.Raw, &t)
		out := t.State.Output
		if maxToolOutput > 0 && len([]rune(out)) > maxToolOutput {
			out = string([]rune(out)[:maxToolOutput])
		}
		var b strings.Builder
		b.WriteString(t.Tool)
		if t.State.Input.Command != "" {
			b.WriteString(": ")
			b.WriteString(t.State.Input.Command)
		}
		if out != "" {
			b.WriteString("\n")
			b.WriteString(out)
		}
		s := strings.TrimSpace(b.String())
		return s, t.Tool, t.State.Input.Command, nil, s != ""
	}
	return "", "", "", nil, false
}
