package store

import (
	"context"
	"errors"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
)

const projectColumns = "id, repo_id, name, path, toolchain, preview_command, preview_port, confirmed, created_at, updated_at"

// CreateProject inserts a project.
func (s *Store) CreateProject(ctx context.Context, p *domain.Project) error {
	if p.ID == "" {
		p.ID = id.New(id.Project)
	}
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	if p.Path == "" {
		p.Path = "."
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO projects ("+projectColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		p.ID, p.RepoID, p.Name, p.Path, p.Toolchain, p.PreviewCommand, p.PreviewPort,
		boolToInt(p.Confirmed), formatTime(p.CreatedAt), formatTime(p.UpdatedAt),
	)
	return wrapErr("create project", err)
}

// GetProject looks a project up by id.
func (s *Store) GetProject(ctx context.Context, projectID string) (*domain.Project, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+projectColumns+" FROM projects WHERE id = ?", projectID)
	return scanProject(row)
}

// GetProjectByPath looks a project up by its path within a repo.
func (s *Store) GetProjectByPath(ctx context.Context, repoID, path string) (*domain.Project, error) {
	if path == "" {
		path = "."
	}
	row := s.db.QueryRowContext(ctx,
		"SELECT "+projectColumns+" FROM projects WHERE repo_id = ? AND path = ?", repoID, path)
	return scanProject(row)
}

// ListProjects returns a repo's projects ordered by path.
func (s *Store) ListProjects(ctx context.Context, repoID string) ([]*domain.Project, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+projectColumns+" FROM projects WHERE repo_id = ? ORDER BY path", repoID)
	if err != nil {
		return nil, wrapErr("list projects", err)
	}
	defer rows.Close()

	var projects []*domain.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, wrapErr("list projects", rows.Err())
}

// UpsertProject creates a project or updates the existing one at the same
// repo-relative path. Monorepo discovery reruns as understanding of a repo
// improves, so it needs to be idempotent per path.
func (s *Store) UpsertProject(ctx context.Context, p *domain.Project) error {
	if p.Path == "" {
		p.Path = "."
	}
	existing, err := s.GetProjectByPath(ctx, p.RepoID, p.Path)
	switch {
	case err == nil:
		p.ID = existing.ID
		p.CreatedAt = existing.CreatedAt
		return s.UpdateProject(ctx, p)
	case errors.Is(err, ErrNotFound):
		return s.CreateProject(ctx, p)
	default:
		return err
	}
}

// UpdateProject persists a mutated project.
func (s *Store) UpdateProject(ctx context.Context, p *domain.Project) error {
	p.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET name = ?, path = ?, toolchain = ?, preview_command = ?,
		        preview_port = ?, confirmed = ?, updated_at = ?
		 WHERE id = ?`,
		p.Name, p.Path, p.Toolchain, p.PreviewCommand, p.PreviewPort,
		boolToInt(p.Confirmed), formatTime(p.UpdatedAt), p.ID,
	)
	if err != nil {
		return wrapErr("update project", err)
	}
	return affectedOne("update project", res)
}

func scanProject(row rowScanner) (*domain.Project, error) {
	var (
		p                domain.Project
		confirmed        int
		created, updated string
	)
	if err := row.Scan(&p.ID, &p.RepoID, &p.Name, &p.Path, &p.Toolchain,
		&p.PreviewCommand, &p.PreviewPort, &confirmed, &created, &updated); err != nil {
		return nil, wrapErr("get project", err)
	}
	p.Confirmed = confirmed != 0
	var err error
	if p.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if p.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &p, nil
}
