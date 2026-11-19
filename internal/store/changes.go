package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
)

const changeColumns = `id, installation_id, user_id, title, branch, workspace_id,
	database, state, state_reason, preview_url, checks, revision,
	created_at, updated_at, promoted_at, base_revision, conflicts`

// CreateChange records a proposed change.
func (s *Store) CreateChange(ctx context.Context, c *domain.Change) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.ID == "" {
		c.ID = id.New(id.Change)
	}
	if c.State == "" {
		c.State = domain.ChangeOpen
	}
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now

	checks, err := json.Marshal(nonNilChecks(c.Checks))
	if err != nil {
		return err
	}
	conflicts, err := json.Marshal(nonNilStrings(c.Conflicts))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO changes (`+changeColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.InstallationID, c.UserID, c.Title, c.Branch, nullableString(c.WorkspaceID),
		c.Database, string(c.State), c.StateReason, c.PreviewURL, string(checks), c.Revision,
		formatTime(c.CreatedAt), formatTime(c.UpdatedAt), nullTime(c.PromotedAt),
		c.BaseRevision, string(conflicts))
	if err != nil {
		return wrapErr("create change", err)
	}
	return nil
}

// GetChange loads one change.
func (s *Store) GetChange(ctx context.Context, changeID string) (*domain.Change, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+changeColumns+" FROM changes WHERE id = ?", changeID)
	return scanChange(row)
}

// ListChanges returns an installation's changes, newest first.
func (s *Store) ListChanges(ctx context.Context, installationID string, limit int) ([]*domain.Change, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+changeColumns+` FROM changes WHERE installation_id = ?
		 ORDER BY created_at DESC, id DESC LIMIT ?`, installationID, limit)
	if err != nil {
		return nil, wrapErr("list changes", err)
	}
	defer rows.Close()

	changes := make([]*domain.Change, 0, 8)
	for rows.Next() {
		one, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		changes = append(changes, one)
	}
	return changes, rows.Err()
}

// UpdateChange writes a change back.
func (s *Store) UpdateChange(ctx context.Context, c *domain.Change) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c.UpdatedAt = time.Now().UTC()

	checks, err := json.Marshal(nonNilChecks(c.Checks))
	if err != nil {
		return err
	}
	conflicts, err := json.Marshal(nonNilStrings(c.Conflicts))
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE changes SET title = ?, branch = ?, workspace_id = ?, database = ?,
			state = ?, state_reason = ?, preview_url = ?, checks = ?, revision = ?,
			updated_at = ?, promoted_at = ?, base_revision = ?, conflicts = ?
		WHERE id = ?`,
		c.Title, c.Branch, nullableString(c.WorkspaceID), c.Database,
		string(c.State), c.StateReason, c.PreviewURL, string(checks), c.Revision,
		formatTime(c.UpdatedAt), nullTime(c.PromotedAt), c.BaseRevision, string(conflicts), c.ID)
	if err != nil {
		return wrapErr("update change", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: change %s", ErrNotFound, c.ID)
	}
	return nil
}

// nonNilChecks keeps an empty list out of the database as [] rather than null,
// so a reader never has to special-case the first change.
func nonNilChecks(checks []domain.CheckResult) []domain.CheckResult {
	if checks == nil {
		return []domain.CheckResult{}
	}
	return checks
}

// nonNilStrings does the same for a list of names.
func nonNilStrings(names []string) []string {
	if names == nil {
		return []string{}
	}
	return names
}

func scanChange(row scannable) (*domain.Change, error) {
	var (
		one         domain.Change
		state       string
		workspaceID sql.NullString
		checks      string
		conflicts   string
		promotedAt  sql.NullString
		createdAt   string
		updatedAt   string
	)
	err := row.Scan(&one.ID, &one.InstallationID, &one.UserID, &one.Title, &one.Branch,
		&workspaceID, &one.Database, &state, &one.StateReason, &one.PreviewURL,
		&checks, &one.Revision, &createdAt, &updatedAt, &promotedAt,
		&one.BaseRevision, &conflicts)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: change", ErrNotFound)
	case err != nil:
		return nil, wrapErr("scan change", err)
	}

	one.State = domain.ChangeState(state)
	one.WorkspaceID = workspaceID.String
	if err := json.Unmarshal([]byte(checks), &one.Checks); err != nil {
		return nil, fmt.Errorf("store: unreadable checks on change %s: %w", one.ID, err)
	}
	if err := json.Unmarshal([]byte(conflicts), &one.Conflicts); err != nil {
		return nil, fmt.Errorf("store: unreadable conflicts on change %s: %w", one.ID, err)
	}
	if len(one.Conflicts) == 0 {
		one.Conflicts = nil
	}
	if one.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if one.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, err
	}
	if one.PromotedAt, err = scanNullTime(promotedAt); err != nil {
		return nil, err
	}
	return &one, nil
}

// ChangeByWorkspace returns the change a workspace is serving, if it is one.
func (s *Store) ChangeByWorkspace(ctx context.Context, workspaceID string) (*domain.Change, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+changeColumns+" FROM changes WHERE workspace_id = ? ORDER BY created_at DESC LIMIT 1",
		workspaceID)
	return scanChange(row)
}
