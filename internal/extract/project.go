package extract

import (
	"context"
	"database/sql"
	"fmt"
)

// Project is a row of the project table.
type Project struct {
	ID       string
	Worktree string
	VCS      sql.NullString
	Name     sql.NullString
}

// ListProjects returns all projects.
func ListProjects(ctx context.Context, db *sql.DB) ([]Project, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, worktree, vcs, name FROM project`)
	if err != nil {
		return nil, fmt.Errorf("extract: list projects: %w", err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Worktree, &p.VCS, &p.Name); err != nil {
			return nil, fmt.Errorf("extract: scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectMap builds an id -> worktree map for fast session binding.
func ProjectMap(ctx context.Context, db *sql.DB) (map[string]string, error) {
	projects, err := ListProjects(ctx, db)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(projects))
	for _, p := range projects {
		m[p.ID] = p.Worktree
	}
	return m, nil
}
