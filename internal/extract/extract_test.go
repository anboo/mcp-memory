package extract

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// newTestDB создаёт временную SQLite-базу с минимальной схемой opencode.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	stmts := []string{
		`CREATE TABLE session (
			id text PRIMARY KEY, project_id text NOT NULL, slug text NOT NULL,
			directory text NOT NULL, title text NOT NULL, version text NOT NULL,
			agent text, model text, time_created integer NOT NULL,
			time_updated integer NOT NULL, time_archived integer)`,
		`CREATE TABLE message (
			id text PRIMARY KEY, session_id text NOT NULL,
			time_created integer NOT NULL, time_updated integer NOT NULL,
			data text NOT NULL)`,
		`CREATE TABLE part (
			id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL,
			time_created integer NOT NULL, time_updated integer NOT NULL,
			data text NOT NULL)`,
		`CREATE TABLE project (
			id text PRIMARY KEY, worktree text NOT NULL, vcs text,
			name text, icon_url text, icon_color text,
			time_created integer NOT NULL, time_updated integer NOT NULL,
			time_initialized integer, sandboxes text NOT NULL)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func seedTestData(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO project (id, worktree, time_created, time_updated, sandboxes)
		VALUES ('p1', '/var/www/test', 1, 1, '[]')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO session
		(id, project_id, slug, directory, title, version, agent, model, time_created, time_updated)
		VALUES ('ses_1', 'p1', 'slug-1', '/var/www/test', 'Test session', '1.0',
			'build', '{"id":"m1","providerID":"p1"}', 100, 200)`)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []struct {
		id, role string
		t        int64
	}{
		{"msg_1", "user", 100},
		{"msg_2", "assistant", 200},
	}
	for _, m := range msgs {
		data, _ := json.Marshal(map[string]any{"role": m.role, "time": map[string]any{"created": m.t}})
		if _, err := db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data)
			VALUES (?, 'ses_1', ?, ?, ?)`, m.id, m.t, m.t, string(data)); err != nil {
			t.Fatal(err)
		}
	}
	parts := []struct {
		id, msg, typ, text string
	}{
		{"prt_1", "msg_1", "text", "привет, посмотри tree.sql"},
		{"prt_2", "msg_2", "reasoning", "The user wants me to look at git changes"},
		{"prt_3", "msg_2", "tool", ""}, // tool наполняется ниже
		{"prt_4", "msg_2", "compaction", ""},
	}
	for i, p := range parts {
		var data string
		switch p.typ {
		case "tool":
			d, _ := json.Marshal(map[string]any{
				"type": "tool", "tool": "bash", "callID": "c1",
				"state": map[string]any{
					"status": "completed",
					"input":  map[string]any{"command": "git diff"},
					"output": "diff --git a/tree.sql",
				},
			})
			data = string(d)
		case "compaction":
			d, _ := json.Marshal(map[string]any{
				"type": "compaction", "auto": true, "tail_start_id": "msg_1",
			})
			data = string(d)
		default:
			d, _ := json.Marshal(map[string]any{"type": p.typ, "text": p.text})
			data = string(d)
		}
		if _, err := db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
			VALUES (?, ?, 'ses_1', ?, ?, ?)`,
			p.id, p.msg, int64(100+i), int64(100+i), data); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListSessions(t *testing.T) {
	db := newTestDB(t)
	seedTestData(t, db)
	sessions, err := ListSessions(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	s := sessions[0]
	if s.ID != "ses_1" || s.ProjectID != "p1" || s.Title != "Test session" {
		t.Fatalf("unexpected session: %+v", s)
	}
	if s.TimeCreated != 100 || s.TimeUpdated != 200 {
		t.Fatalf("times: %d %d", s.TimeCreated, s.TimeUpdated)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	db := newTestDB(t)
	if _, err := GetSession(context.Background(), db, "nope"); err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestModelInfo(t *testing.T) {
	s := Session{Model: `{"id":"m1","providerID":"p1","variant":"default"}`}
	m := s.ModelInfo()
	if m.ID != "m1" || m.ProviderID != "p1" || m.Variant != "default" {
		t.Fatalf("unexpected model info: %+v", m)
	}
	var empty Session
	if got := empty.ModelInfo(); got.ID != "" {
		t.Fatalf("empty model should parse to zero value")
	}
}

func TestListMessages(t *testing.T) {
	db := newTestDB(t)
	seedTestData(t, db)
	msgs, err := ListMessages(context.Background(), db, "ses_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("roles: %s %s", msgs[0].Role, msgs[1].Role)
	}
}

func TestPartsWithPosition(t *testing.T) {
	db := newTestDB(t)
	seedTestData(t, db)
	parts, err := PartsWithPosition(context.Background(), db, "ses_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 4 {
		t.Fatalf("parts = %d, want 4", len(parts))
	}
	// порядок: сообщение msg_1 (time 100) раньше msg_2 (time 200)
	if parts[0].ID != "prt_1" || parts[0].Position != 0 {
		t.Fatalf("first part: %+v", parts[0])
	}
	if parts[len(parts)-1].Position != 3 {
		t.Fatalf("last position = %d, want 3", parts[len(parts)-1].Position)
	}
	for i, p := range parts {
		if p.Position != i {
			t.Fatalf("position %d != index %d", p.Position, i)
		}
	}
}

func TestParseTool(t *testing.T) {
	db := newTestDB(t)
	seedTestData(t, db)
	parts, err := PartsInOrder(context.Background(), db, "ses_1")
	if err != nil {
		t.Fatal(err)
	}
	var toolPart *Part
	for i := range parts {
		if parts[i].Type == PartTypeTool {
			toolPart = &parts[i]
			break
		}
	}
	if toolPart == nil {
		t.Fatal("tool part not found")
	}
	tp, err := toolPart.ParseTool()
	if err != nil {
		t.Fatal(err)
	}
	if tp.Tool != "bash" || tp.State.Input.Command != "git diff" {
		t.Fatalf("unexpected tool: %+v", tp)
	}
	if !strings.Contains(tp.State.Output, "tree.sql") {
		t.Fatalf("output mismatch: %q", tp.State.Output)
	}
}

func TestMarkCompacted(t *testing.T) {
	db := newTestDB(t)
	seedTestData(t, db)
	s := &Session{ID: "ses_1"}
	parts, err := PartsInOrder(context.Background(), db, "ses_1")
	if err != nil {
		t.Fatal(err)
	}
	MarkCompacted(s, parts)
	if !s.Compacted {
		t.Fatal("session should be compacted")
	}
	if s.TailStartID != "msg_1" {
		t.Fatalf("tail_start_id = %q, want msg_1", s.TailStartID)
	}
}

func TestProjectMap(t *testing.T) {
	db := newTestDB(t)
	seedTestData(t, db)
	m, err := ProjectMap(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if m["p1"] != "/var/www/test" {
		t.Fatalf("project map: %v", m)
	}
}
