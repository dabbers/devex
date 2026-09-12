package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/id"
)

const forkColumns = `id, task_id, user_id, repo_id, project_id, name, description, branch,
	state, state_reason, serialize_group, instance_id, preview_url,
	usage_json, escalation_json, created_at, updated_at, started_at, ended_at`

// CreateFork inserts a fork in the queued state.
func (s *Store) CreateFork(ctx context.Context, f *domain.Fork) error {
	if f.ID == "" {
		f.ID = id.New(id.Fork)
	}
	now := time.Now().UTC()
	if f.CreatedAt.IsZero() {
		f.CreatedAt = now
	}
	f.UpdatedAt = now
	if f.State == "" {
		f.State = domain.ForkQueued
	}

	usage, err := marshalJSON(f.Usage)
	if err != nil {
		return err
	}
	escalation, err := marshalJSON(f.Escalation)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO forks ("+forkColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		f.ID, f.TaskID, f.UserID, f.RepoID, f.ProjectID, f.Name, f.Description, f.Branch,
		string(f.State), f.StateReason, f.SerializeGroup, f.InstanceID, f.PreviewURL,
		usage, escalation, formatTime(f.CreatedAt), formatTime(f.UpdatedAt),
		nullTime(f.StartedAt), nullTime(f.EndedAt),
	)
	return wrapErr("create fork", err)
}

// GetFork looks a fork up by id.
func (s *Store) GetFork(ctx context.Context, forkID string) (*domain.Fork, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+forkColumns+" FROM forks WHERE id = ?", forkID)
	return scanFork(row)
}

// ForkFilter narrows a fork listing.
type ForkFilter struct {
	TaskID string
	UserID string
	// RepoID scopes the listing to one repo. The orchestrator tracks active
	// agents per repo rather than globally, so this is the common case.
	RepoID string
	States []domain.ForkState
	// Active, when true, restricts the listing to forks holding VM capacity.
	Active bool
	Limit  int
}

// ListForks returns forks matching the filter, oldest first so that queue
// ordering is preserved.
func (s *Store) ListForks(ctx context.Context, f ForkFilter) ([]*domain.Fork, error) {
	var (
		where []string
		args  []any
	)
	if f.TaskID != "" {
		where = append(where, "task_id = ?")
		args = append(args, f.TaskID)
	}
	if f.UserID != "" {
		where = append(where, "user_id = ?")
		args = append(args, f.UserID)
	}
	if f.RepoID != "" {
		where = append(where, "repo_id = ?")
		args = append(args, f.RepoID)
	}

	states := f.States
	if f.Active {
		states = append(states, activeStates()...)
	}
	if len(states) > 0 {
		where = append(where, "state IN ("+placeholders(len(states))+")")
		for _, st := range states {
			args = append(args, string(st))
		}
	}

	query := "SELECT " + forkColumns + " FROM forks"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY created_at ASC, id ASC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrapErr("list forks", err)
	}
	defer rows.Close()

	var forks []*domain.Fork
	for rows.Next() {
		fork, err := scanFork(rows)
		if err != nil {
			return nil, err
		}
		forks = append(forks, fork)
	}
	return forks, wrapErr("list forks", rows.Err())
}

// UpdateFork persists a mutated fork.
func (s *Store) UpdateFork(ctx context.Context, f *domain.Fork) error {
	f.UpdatedAt = time.Now().UTC()
	usage, err := marshalJSON(f.Usage)
	if err != nil {
		return err
	}
	escalation, err := marshalJSON(f.Escalation)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE forks SET name = ?, description = ?, branch = ?, state = ?, state_reason = ?,
		        serialize_group = ?, instance_id = ?, preview_url = ?, usage_json = ?,
		        escalation_json = ?, updated_at = ?, started_at = ?, ended_at = ?
		 WHERE id = ?`,
		f.Name, f.Description, f.Branch, string(f.State), f.StateReason, f.SerializeGroup,
		f.InstanceID, f.PreviewURL, usage, escalation, formatTime(f.UpdatedAt),
		nullTime(f.StartedAt), nullTime(f.EndedAt), f.ID,
	)
	if err != nil {
		return wrapErr("update fork", err)
	}
	return affectedOne("update fork", res)
}

// TransitionFork applies a lifecycle transition and persists it in one step,
// stamping the start and end times the lifecycle implies.
func (s *Store) TransitionFork(ctx context.Context, f *domain.Fork, next domain.ForkState, reason string) error {
	if err := domain.TransitionFork(f, next); err != nil {
		return err
	}
	f.StateReason = reason
	now := time.Now().UTC()
	if next == domain.ForkProvisioning && f.StartedAt == nil {
		f.StartedAt = &now
	}
	if next.Terminal() && f.EndedAt == nil {
		f.EndedAt = &now
	}
	return s.UpdateFork(ctx, f)
}

// CountActiveForks reports how many forks currently hold VM capacity, which is
// what the scheduler admits against.
func (s *Store) CountActiveForks(ctx context.Context) (int, error) {
	states := activeStates()
	args := make([]any, 0, len(states))
	for _, st := range states {
		args = append(args, string(st))
	}
	row := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM forks WHERE state IN ("+placeholders(len(states))+")", args...)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, wrapErr("count active forks", err)
	}
	return n, nil
}

// activeStates lists the fork states that consume scheduler capacity.
func activeStates() []domain.ForkState {
	return []domain.ForkState{
		domain.ForkProvisioning,
		domain.ForkCoding,
		domain.ForkVerifying,
		domain.ForkFixing,
		domain.ForkAwaitingMerge,
		domain.ForkMerging,
	}
}

func scanFork(row rowScanner) (*domain.Fork, error) {
	var (
		f                domain.Fork
		state            string
		usage            sql.NullString
		escalation       sql.NullString
		created, updated string
		started, ended   sql.NullString
	)
	if err := row.Scan(&f.ID, &f.TaskID, &f.UserID, &f.RepoID, &f.ProjectID, &f.Name,
		&f.Description, &f.Branch, &state, &f.StateReason, &f.SerializeGroup,
		&f.InstanceID, &f.PreviewURL, &usage, &escalation,
		&created, &updated, &started, &ended); err != nil {
		return nil, wrapErr("get fork", err)
	}
	f.State = domain.ForkState(state)
	if err := unmarshalJSON(usage, &f.Usage); err != nil {
		return nil, err
	}
	if err := unmarshalJSON(escalation, &f.Escalation); err != nil {
		return nil, err
	}
	var err error
	if f.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if f.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	if f.StartedAt, err = scanNullTime(started); err != nil {
		return nil, err
	}
	if f.EndedAt, err = scanNullTime(ended); err != nil {
		return nil, err
	}
	return &f, nil
}
