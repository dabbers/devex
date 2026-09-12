package store

import (
	"context"
	"fmt"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
)

const repoColumns = "id, user_id, name, remote_url, default_branch, discovered, created_at, updated_at"

// CreateRepo inserts a repo.
func (s *Store) CreateRepo(ctx context.Context, r *domain.Repo) error {
	if r.ID == "" {
		r.ID = id.New(id.Repo)
	}
	now := time.Now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	if r.DefaultBranch == "" {
		r.DefaultBranch = "main"
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO repos ("+repoColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		r.ID, r.UserID, r.Name, r.RemoteURL, r.DefaultBranch,
		boolToInt(r.Discovered), formatTime(r.CreatedAt), formatTime(r.UpdatedAt),
	)
	return wrapErr("create repo", err)
}

// GetRepo looks a repo up by id.
func (s *Store) GetRepo(ctx context.Context, repoID string) (*domain.Repo, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+repoColumns+" FROM repos WHERE id = ?", repoID)
	return scanRepo(row)
}

// GetRepoByName looks a repo up by its name within a user's scope.
func (s *Store) GetRepoByName(ctx context.Context, userID, name string) (*domain.Repo, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+repoColumns+" FROM repos WHERE user_id = ? AND name = ?", userID, name)
	return scanRepo(row)
}

// ListRepos returns a user's repos, newest first.
func (s *Store) ListRepos(ctx context.Context, userID string) ([]*domain.Repo, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+repoColumns+" FROM repos WHERE user_id = ? ORDER BY created_at DESC, id DESC", userID)
	if err != nil {
		return nil, wrapErr("list repos", err)
	}
	defer rows.Close()

	var repos []*domain.Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	return repos, wrapErr("list repos", rows.Err())
}

// UpdateRepo persists a mutated repo, bumping its update time.
func (s *Store) UpdateRepo(ctx context.Context, r *domain.Repo) error {
	r.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE repos SET name = ?, remote_url = ?, default_branch = ?, discovered = ?, updated_at = ?
		 WHERE id = ?`,
		r.Name, r.RemoteURL, r.DefaultBranch, boolToInt(r.Discovered), formatTime(r.UpdatedAt), r.ID,
	)
	if err != nil {
		return wrapErr("update repo", err)
	}
	return affectedOne("update repo", res)
}

func scanRepo(row rowScanner) (*domain.Repo, error) {
	var (
		r                domain.Repo
		discovered       int
		created, updated string
	)
	if err := row.Scan(&r.ID, &r.UserID, &r.Name, &r.RemoteURL, &r.DefaultBranch,
		&discovered, &created, &updated); err != nil {
		return nil, wrapErr("get repo", err)
	}
	r.Discovered = discovered != 0
	var err error
	if r.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if r.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &r, nil
}

// affectedOne converts an update that matched no rows into ErrNotFound, so
// callers do not silently succeed on a stale id.
func affectedOne(op string, res interface{ RowsAffected() (int64, error) }) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: %s: %w", op, err)
	}
	if n == 0 {
		return fmt.Errorf("store: %s: %w", op, ErrNotFound)
	}
	return nil
}
