package extract

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Message is a row of the message table.
type Message struct {
	ID          string
	SessionID   string
	Role        string // user | assistant
	Agent       string
	ModelID     string
	TimeCreated int64 // epoch ms
	TimeUpdated int64
}

// MessageData is the parsed JSON of the message.data column.
type MessageData struct {
	Role  string `json:"role"`
	Agent string `json:"agent"`
	Model struct {
		ModelID string `json:"modelID"`
	} `json:"model"`
}

const messageCols = `id, session_id, time_created, time_updated, data`

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var data string
	if err := row.Scan(&m.ID, &m.SessionID, &m.TimeCreated, &m.TimeUpdated, &data); err != nil {
		return m, fmt.Errorf("extract: scan message: %w", err)
	}
	var md MessageData
	if err := json.Unmarshal([]byte(data), &md); err == nil {
		m.Role = md.Role
		m.Agent = md.Agent
		m.ModelID = md.Model.ModelID
	}
	return m, nil
}

// ListMessages returns a session's messages in chronological order
// (by time_created; id breaks ties at equal times).
func ListMessages(ctx context.Context, db *sql.DB, sessionID string) ([]Message, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+messageCols+` FROM message
		 WHERE session_id = ?
		 ORDER BY time_created, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("extract: list messages: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
