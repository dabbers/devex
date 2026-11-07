package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
)

const workspaceColumns = "id, user_id, repo_id, project_id, name, branch, state, state_reason, " +
	"instance_id, preview_url, ephemeral, created_at, updated_at, started_at, stopped_at, last_used_at"

// CreateWorkspace inserts a workspace, defaulting it to stopped: nothing is
// running until something starts it, and recording it as running before the
// machine exists is how a UI ends up showing a preview that was never there.
func (s *Store) CreateWorkspace(ctx context.Context, w *domain.Workspace) error {
	if w.ID == "" {
		w.ID = id.New(id.Workspace)
	}
	if w.State == "" {
		w.State = domain.WorkspaceStopped
	}
	if !w.State.Valid() {
		return fmt.Errorf("store: %q is not a workspace state", w.State)
	}
	now := time.Now().UTC()
	if w.CreatedAt.IsZero() {
		w.CreatedAt = now
	}
	w.UpdatedAt = now

	_, err := s.db.ExecContext(ctx,
		"INSERT INTO workspaces ("+workspaceColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		w.ID, w.UserID, w.RepoID, w.ProjectID, w.Name, w.Branch, string(w.State), w.StateReason,
		w.InstanceID, w.PreviewURL, boolToInt(w.Ephemeral),
		formatTime(w.CreatedAt), formatTime(w.UpdatedAt),
		nullTime(w.StartedAt), nullTime(w.StoppedAt), nullTime(w.LastUsedAt),
	)
	return wrapErr("create workspace", err)
}

// GetWorkspace looks a workspace up by id.
func (s *Store) GetWorkspace(ctx context.Context, workspaceID string) (*domain.Workspace, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+workspaceColumns+" FROM workspaces WHERE id = ?", workspaceID)
	return scanWorkspace(row)
}

// WorkspaceFilter narrows a workspace listing.
type WorkspaceFilter struct {
	UserID string
	RepoID string
	// States, when non-empty, keeps only workspaces in one of them.
	States []domain.WorkspaceState
	// IncludeEphemeral keeps the scratch workspaces a plan created. They are
	// excluded by default: a person's list of places to work should not fill
	// up with machines a batch run made for itself.
	IncludeEphemeral bool
}

// ListWorkspaces returns workspaces matching the filter, most recently used
// first, so the list reads as "where you were working".
func (s *Store) ListWorkspaces(ctx context.Context, filter WorkspaceFilter) ([]*domain.Workspace, error) {
	query := "SELECT " + workspaceColumns + " FROM workspaces WHERE 1 = 1"
	var args []any
	if filter.UserID != "" {
		query += " AND user_id = ?"
		args = append(args, filter.UserID)
	}
	if filter.RepoID != "" {
		query += " AND repo_id = ?"
		args = append(args, filter.RepoID)
	}
	if !filter.IncludeEphemeral {
		query += " AND ephemeral = 0"
	}
	if len(filter.States) > 0 {
		query += " AND state IN ("
		for i, state := range filter.States {
			if i > 0 {
				query += ", "
			}
			query += "?"
			args = append(args, string(state))
		}
		query += ")"
	}
	// COALESCE so a workspace that has never been started still sorts, by
	// creation, rather than falling to the bottom under a NULL.
	query += " ORDER BY COALESCE(last_used_at, created_at) DESC, id DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrapErr("list workspaces", err)
	}
	defer rows.Close()

	var workspaces []*domain.Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, w)
	}
	return workspaces, wrapErr("list workspaces", rows.Err())
}

// UpdateWorkspace persists a mutated workspace.
func (s *Store) UpdateWorkspace(ctx context.Context, w *domain.Workspace) error {
	if !w.State.Valid() {
		return fmt.Errorf("store: %q is not a workspace state", w.State)
	}
	w.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE workspaces SET name = ?, branch = ?, state = ?, state_reason = ?,
		        instance_id = ?, preview_url = ?, ephemeral = ?, updated_at = ?,
		        started_at = ?, stopped_at = ?, last_used_at = ?
		 WHERE id = ?`,
		w.Name, w.Branch, string(w.State), w.StateReason, w.InstanceID, w.PreviewURL,
		boolToInt(w.Ephemeral), formatTime(w.UpdatedAt),
		nullTime(w.StartedAt), nullTime(w.StoppedAt), nullTime(w.LastUsedAt), w.ID,
	)
	if err != nil {
		return wrapErr("update workspace", err)
	}
	return affectedOne("update workspace", res)
}

// TransitionWorkspace moves a workspace to a new state if the move is legal
// and the workspace is still in the state the caller last saw.
//
// Compare-and-set, like ClaimFork: two requests to start the same workspace
// would otherwise both see it stopped and both boot a machine, and nothing
// is reclaimed automatically, so the loser would be stranded.
func (s *Store) TransitionWorkspace(ctx context.Context, workspaceID string, from, to domain.WorkspaceState, reason string) (*domain.Workspace, error) {
	if !from.CanTransition(to) {
		return nil, fmt.Errorf("store: a workspace cannot go from %s to %s", from, to)
	}

	now := time.Now().UTC()
	query := `UPDATE workspaces SET state = ?, state_reason = ?, updated_at = ?`
	args := []any{string(to), reason, formatTime(now)}
	switch to {
	case domain.WorkspaceStarting:
		query += ", started_at = ?, last_used_at = ?"
		args = append(args, formatTime(now), formatTime(now))
	case domain.WorkspaceStopped:
		query += ", stopped_at = ?"
		args = append(args, formatTime(now))
	}
	query += " WHERE id = ? AND state = ?"
	args = append(args, workspaceID, string(from))

	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, wrapErr("transition workspace", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return nil, wrapErr("transition workspace", err)
	}
	if changed == 0 {
		// Either it is gone or somebody else moved it first. Reporting which
		// matters: the second is a race the caller should simply lose.
		current, err := s.GetWorkspace(ctx, workspaceID)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("store: workspace %s is %s, not %s", workspaceID, current.State, from)
	}
	return s.GetWorkspace(ctx, workspaceID)
}

// TouchWorkspace records that a workspace was used, which is what orders the
// listing.
func (s *Store) TouchWorkspace(ctx context.Context, workspaceID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE workspaces SET last_used_at = ? WHERE id = ?",
		formatTime(time.Now().UTC()), workspaceID)
	return wrapErr("touch workspace", err)
}

func scanWorkspace(row rowScanner) (*domain.Workspace, error) {
	var (
		w                            domain.Workspace
		state                        string
		ephemeral                    int
		created, updated             string
		started, stopped, lastUsedAt sql.NullString
	)
	if err := row.Scan(&w.ID, &w.UserID, &w.RepoID, &w.ProjectID, &w.Name, &w.Branch,
		&state, &w.StateReason, &w.InstanceID, &w.PreviewURL, &ephemeral,
		&created, &updated, &started, &stopped, &lastUsedAt); err != nil {
		return nil, wrapErr("get workspace", err)
	}
	w.State = domain.WorkspaceState(state)
	w.Ephemeral = ephemeral != 0

	var err error
	if w.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if w.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	if w.StartedAt, err = scanNullTime(started); err != nil {
		return nil, err
	}
	if w.StoppedAt, err = scanNullTime(stopped); err != nil {
		return nil, err
	}
	if w.LastUsedAt, err = scanNullTime(lastUsedAt); err != nil {
		return nil, err
	}
	return &w, nil
}
